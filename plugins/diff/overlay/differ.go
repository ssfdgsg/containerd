//go:build linux

/*
   Copyright The containerd Authors.

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

package overlay

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/containerd/continuity/fs"
	"github.com/containerd/errdefs"
	"github.com/containerd/log"
	digest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/diff"
	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/containerd/v2/pkg/archive"
	"github.com/containerd/containerd/v2/pkg/archive/compression"
	"github.com/containerd/containerd/v2/pkg/labels"
	"github.com/containerd/containerd/v2/plugins/diff/walking"
)

type overlayDiff struct {
	store content.Store
}

var emptyDesc = ocispec.Descriptor{}

// NewOverlayDiff returns a diff.Comparer optimized for overlayfs.
// It reads the upperdir directly instead of mounting upper, and only
// walks files actually written by the container (O(upperdir files)
// instead of O(total image files)).
func NewOverlayDiff(store content.Store) diff.Comparer {
	return &overlayDiff{store: store}
}

// Compare creates a diff between the given mounts and uploads the result
// to the content store. When upper is an overlay mount with an upperdir,
// it uses the DiffDirChanges fast path; otherwise it falls back to walking diff.
func (s *overlayDiff) Compare(ctx context.Context, lower, upper []mount.Mount, opts ...diff.Opt) (d ocispec.Descriptor, err error) {
	upperRoot, err := extractUpperdir(upper)
	if err != nil {
		return walking.NewWalkingDiff(s.store).Compare(ctx, lower, upper, opts...)
	}

	var config diff.Config
	for _, opt := range opts {
		if err := opt(&config); err != nil {
			return emptyDesc, err
		}
	}

	compressionType := compression.Uncompressed
	if config.Compressor != nil {
		if config.MediaType == "" {
			return emptyDesc, fmt.Errorf("media type must be explicitly specified when using custom compressor")
		}
		compressionType = compression.Unknown
	} else {
		if config.MediaType == "" {
			config.MediaType = ocispec.MediaTypeImageLayerGzip
		}
		switch config.MediaType {
		case ocispec.MediaTypeImageLayer:
		case ocispec.MediaTypeImageLayerGzip:
			compressionType = compression.Gzip
		case ocispec.MediaTypeImageLayerZstd:
			compressionType = compression.Zstd
		default:
			return emptyDesc, fmt.Errorf("unsupported diff media type: %v: %w", config.MediaType, errdefs.ErrNotImplemented)
		}
	}

	var newReference bool
	if config.Reference == "" {
		newReference = true
		config.Reference = uniqueRef()
	}

	cw, err := s.store.Writer(ctx,
		content.WithRef(config.Reference),
		content.WithDescriptor(ocispec.Descriptor{
			MediaType: config.MediaType,
		}))
	if err != nil {
		return emptyDesc, fmt.Errorf("failed to open writer: %w", err)
	}

	var errOpen error
	defer func() {
		if errOpen != nil {
			cw.Close()
			if newReference {
				if abortErr := s.store.Abort(ctx, config.Reference); abortErr != nil {
					log.G(ctx).WithError(abortErr).WithField("ref", config.Reference).Warnf("failed to delete diff upload")
				}
			}
		}
	}()
	if !newReference {
		if errOpen = cw.Truncate(0); errOpen != nil {
			return emptyDesc, errOpen
		}
	}

	if compressionType != compression.Uncompressed {
		dgstr := digest.SHA256.Digester()
		var compressed io.WriteCloser
		if config.Compressor != nil {
			compressed, errOpen = config.Compressor(cw, config.MediaType)
			if errOpen != nil {
				return emptyDesc, fmt.Errorf("failed to get compressed stream: %w", errOpen)
			}
		} else {
			compressed, errOpen = compression.CompressStream(cw, compressionType)
			if errOpen != nil {
				return emptyDesc, fmt.Errorf("failed to get compressed stream: %w", errOpen)
			}
		}
		errOpen = writeDiff(ctx, io.MultiWriter(compressed, dgstr.Hash()), lower, upperRoot)
		compressed.Close()
		if errOpen != nil {
			return emptyDesc, fmt.Errorf("failed to write compressed diff: %w", errOpen)
		}
		if config.Labels == nil {
			config.Labels = map[string]string{}
		}
		config.Labels[labels.LabelUncompressed] = dgstr.Digest().String()
	} else {
		if errOpen = writeDiff(ctx, cw, lower, upperRoot); errOpen != nil {
			return emptyDesc, fmt.Errorf("failed to write diff: %w", errOpen)
		}
	}

	var commitopts []content.Opt
	if config.Labels != nil {
		commitopts = append(commitopts, content.WithLabels(config.Labels))
	}

	dgst := cw.Digest()
	if errOpen = cw.Commit(ctx, 0, dgst, commitopts...); errOpen != nil {
		if !errdefs.IsAlreadyExists(errOpen) {
			return emptyDesc, fmt.Errorf("failed to commit: %w", errOpen)
		}
		errOpen = nil
	}

	info, err := s.store.Info(ctx, dgst)
	if err != nil {
		return emptyDesc, fmt.Errorf("failed to get info from content store: %w", err)
	}
	if info.Labels == nil {
		info.Labels = make(map[string]string)
	}
	if _, ok := info.Labels[labels.LabelUncompressed]; !ok {
		info.Labels[labels.LabelUncompressed] = config.Labels[labels.LabelUncompressed]
		if _, err := s.store.Update(ctx, info, "labels."+labels.LabelUncompressed); err != nil {
			return emptyDesc, fmt.Errorf("error setting uncompressed label: %w", err)
		}
	}

	return ocispec.Descriptor{
		MediaType: config.MediaType,
		Size:      info.Size,
		Digest:    info.Digest,
	}, nil
}

func writeDiff(ctx context.Context, w io.Writer, lower []mount.Mount, upperRoot string) error {
	return mount.WithTempMount(ctx, lower, func(lowerRoot string) error {
		cw := archive.NewChangeWriter(w, upperRoot)
		if err := fs.DiffDirChanges(ctx, lowerRoot, upperRoot, fs.DiffSourceOverlayFS, cw.HandleChange); err != nil {
			return fmt.Errorf("failed to create diff: %w", err)
		}
		return cw.Close()
	})
}

func extractUpperdir(mounts []mount.Mount) (string, error) {
	for _, m := range mounts {
		if m.Type != "overlay" {
			continue
		}
		for _, opt := range m.Options {
			if after, found := strings.CutPrefix(opt, "upperdir="); found {
				if after == "" {
					return "", fmt.Errorf("upperdir option is empty")
				}
				return after, nil
			}
		}
	}
	return "", fmt.Errorf("no upperdir found in overlay mounts")
}

func uniqueRef() string {
	t := time.Now()
	var b [3]byte
	rand.Read(b[:])
	return fmt.Sprintf("%d-%s", t.UnixNano(), base64.URLEncoding.EncodeToString(b[:]))
}
