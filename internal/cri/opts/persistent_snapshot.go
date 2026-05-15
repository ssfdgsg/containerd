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

package opts

import (
	"context"
	"fmt"
	"strings"

	"github.com/containerd/errdefs"
	"github.com/containerd/log"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/containers"
	"github.com/containerd/containerd/v2/core/snapshots"
)

const persistImageDigestLabel = "persist.containerd.dev/image-digest"
const persistRuntimeKeyPrefix = "persist/"
const persistImportKeyPrefix = "persist-import/"

func persistentImportedSnapshotKey(runtimeKey string) (string, bool) {
	if !strings.HasPrefix(runtimeKey, persistRuntimeKeyPrefix) {
		return "", false
	}
	return persistImportKeyPrefix + strings.TrimPrefix(runtimeKey, persistRuntimeKeyPrefix), true
}

// WithPersistentSnapshot reuses a named writable snapshot if it already exists,
// otherwise it creates it from the supplied image.
func WithPersistentSnapshot(id string, i containerd.Image, expectedImageRef string, labels map[string]string, appendSnapshotLabels bool, opts ...snapshots.Opt) containerd.NewContainerOpts {
	return func(ctx context.Context, client *containerd.Client, c *containers.Container) error {
		if c.Snapshotter == "" {
			return fmt.Errorf("no snapshotter set for persistent snapshot %q", id)
		}

		snapshotter := client.SnapshotService(c.Snapshotter)
		log.G(ctx).Infof("persistent snapshot requested key=%s snapshotter=%s", id, c.Snapshotter)

		checkImageRef := func(key string, info snapshots.Info) error {
			if expectedImageRef == "" {
				return nil
			}
			got := ""
			if info.Labels != nil {
				got = info.Labels[persistImageDigestLabel]
			}
			if got != expectedImageRef {
				return fmt.Errorf("persistent snapshot %q was created for image %q, not %q: %w", key, got, expectedImageRef, errdefs.ErrFailedPrecondition)
			}
			return nil
		}

		info, err := snapshotter.Stat(ctx, id)
		if err == nil {
			log.G(ctx).Infof("persistent snapshot stat key=%s kind=%s parent=%s", id, info.Kind, info.Parent)
			if err := checkImageRef(id, info); err != nil {
				return err
			}
			switch info.Kind {
			case snapshots.KindActive, snapshots.KindView:
				if err := containerd.WithSnapshot(id)(ctx, client, c); err != nil {
					return err
				}
				c.Image = i.Name()
				return nil
			case snapshots.KindCommitted:
				return fmt.Errorf("persistent snapshot %q exists as committed snapshot and cannot be used directly as runtime writable rootfs; import into %q instead: %w", id, persistImportKeyPrefix+strings.TrimPrefix(id, persistRuntimeKeyPrefix), errdefs.ErrFailedPrecondition)
			default:
				return fmt.Errorf("persistent snapshot %q has unsupported kind %s for runtime reuse: %w", id, info.Kind, errdefs.ErrFailedPrecondition)
			}
		}
		if !errdefs.IsNotFound(err) {
			return err
		}

		snapshotOpts := make([]snapshots.Opt, 0, len(opts)+1)
		if len(labels) > 0 {
			snapshotOpts = append(snapshotOpts, snapshots.WithLabels(labels))
		}
		snapshotOpts = append(snapshotOpts, opts...)

		if importedKey, ok := persistentImportedSnapshotKey(id); ok {
			importedInfo, importedErr := snapshotter.Stat(ctx, importedKey)
			if importedErr == nil {
				log.G(ctx).Infof("persistent imported base found key=%s kind=%s parent=%s", importedKey, importedInfo.Kind, importedInfo.Parent)
				if importedInfo.Kind != snapshots.KindCommitted {
					return fmt.Errorf("persistent imported base %q has kind %s, want committed: %w", importedKey, importedInfo.Kind, errdefs.ErrFailedPrecondition)
				}
				if err := checkImageRef(importedKey, importedInfo); err != nil {
					return err
				}
				log.G(ctx).Infof("persistent snapshot prepare active key=%s parent=%s", id, importedKey)
				if _, err := snapshotter.Prepare(ctx, id, importedKey, snapshotOpts...); err != nil {
					return err
				}
				c.SnapshotKey = id
				c.Image = i.Name()
				return nil
			}
			if importedErr != nil && !errdefs.IsNotFound(importedErr) {
				return importedErr
			}
		}

		log.G(ctx).Infof("persistent snapshot fallback new image snapshot key=%s", id)
		return WithNewSnapshot(id, i, appendSnapshotLabels, snapshotOpts...)(ctx, client, c)
	}
}
