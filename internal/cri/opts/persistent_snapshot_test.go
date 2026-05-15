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

import "testing"

func TestPersistentImportedSnapshotKey(t *testing.T) {
	tests := []struct {
		runtimeKey string
		want       string
		ok         bool
	}{
		{
			runtimeKey: "persist/demo-rootfs-1-box",
			want:       "persist-import/demo-rootfs-1-box",
			ok:         true,
		},
		{
			runtimeKey: "random/demo-rootfs-1-box",
			want:       "",
			ok:         false,
		},
	}

	for _, tt := range tests {
		got, ok := persistentImportedSnapshotKey(tt.runtimeKey)
		if got != tt.want || ok != tt.ok {
			t.Fatalf("persistentImportedSnapshotKey(%q) = (%q, %v), want (%q, %v)", tt.runtimeKey, got, ok, tt.want, tt.ok)
		}
	}
}
