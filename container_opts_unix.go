//go:build !windows

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

package containerd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"github.com/containerd/containerd/containers"
	"github.com/containerd/containerd/mount"
	"github.com/containerd/containerd/pkg/idtools"
	"github.com/containerd/errdefs"
	"github.com/opencontainers/image-spec/identity"
)

// WithRemappedSnapshot creates a new snapshot and remaps the uid/gid for the
// filesystem to be used by a container with user namespaces
func WithRemappedSnapshot(id string, i Image, uid, gid uint32) NewContainerOpts {
	idmap := idtools.IdentityMapping{
		UIDMaps: []idtools.IDMap{
			{
				ContainerID: 0,
				HostID:      int(uid),
				Size:        1,
			},
		},
		GIDMaps: []idtools.IDMap{
			{
				ContainerID: 0,
				HostID:      int(gid),
				Size:        1,
			},
		},
	}
	return withRemappedSnapshotBase(id, i, idmap, false)
}
func WithMultiRemappedSnapshot(id string, i Image, idmap idtools.IdentityMapping) NewContainerOpts {
	return withRemappedSnapshotBase(id, i, idmap, false)
}

// WithRemappedSnapshotView is similar to WithRemappedSnapshot but rootfs is mounted as read-only.
func WithRemappedSnapshotView(id string, i Image, uid, gid uint32) NewContainerOpts {
	idmap := idtools.IdentityMapping{
		UIDMaps: []idtools.IDMap{
			{
				ContainerID: 0,
				HostID:      int(uid),
				Size:        1,
			},
		},
		GIDMaps: []idtools.IDMap{
			{
				ContainerID: 0,
				HostID:      int(gid),
				Size:        1,
			},
		},
	}
	return withRemappedSnapshotBase(id, i, idmap, true)
}
func WithMultiRemappedSnapshotView(id string, i Image, idmap idtools.IdentityMapping) NewContainerOpts {
	return withRemappedSnapshotBase(id, i, idmap, true)
}

func withRemappedSnapshotBase(id string, i Image, idmap idtools.IdentityMapping, readonly bool) NewContainerOpts {
	return func(ctx context.Context, client *Client, c *containers.Container) error {
		diffIDs, err := i.(*image).i.RootFS(ctx, client.ContentStore(), client.platform)
		if err != nil {
			return err
		}

		parent := identity.ChainID(diffIDs).String()
		rootMap := idmap.RootPair()
		usernsID := fmt.Sprintf("%s-%d-%d", parent, rootMap.UID, rootMap.GID)

		c.Snapshotter, err = client.resolveSnapshotterName(ctx, c.Snapshotter)
		if err != nil {
			return err
		}
		snapshotter, err := client.getSnapshotter(ctx, c.Snapshotter)
		if err != nil {
			return err
		}
		if _, err := snapshotter.Stat(ctx, usernsID); err == nil {
			if _, err := snapshotter.Prepare(ctx, id, usernsID); err == nil {
				c.SnapshotKey = id
				c.Image = i.Name()
				return nil
			} else if !errdefs.IsNotFound(err) {
				return err
			}
		}
		mounts, err := snapshotter.Prepare(ctx, usernsID+"-remap", parent)
		if err != nil {
			return err
		}
		if err := remapRootFS(ctx, mounts, idmap); err != nil {
			snapshotter.Remove(ctx, usernsID)
			return err
		}
		if err := snapshotter.Commit(ctx, usernsID, usernsID+"-remap"); err != nil {
			return err
		}
		if readonly {
			_, err = snapshotter.View(ctx, id, usernsID)
		} else {
			_, err = snapshotter.Prepare(ctx, id, usernsID)
		}
		if err != nil {
			return err
		}
		c.SnapshotKey = id
		c.Image = i.Name()
		return nil
	}
}

func remapRootFS(ctx context.Context, mounts []mount.Mount, idmap idtools.IdentityMapping) error {
	return mount.WithTempMount(ctx, mounts, func(root string) error {
		return filepath.Walk(root, chown(root, idmap))
	})
}

func chown(root string, idmap idtools.IdentityMapping) filepath.WalkFunc {
	return func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		stat := info.Sys().(*syscall.Stat_t)
		h, cerr := idmap.ToHost(idtools.Identity{UID: int(stat.Uid), GID: int(stat.Gid)})
		if cerr != nil {
			return cerr
		}
		// be sure the lchown the path as to not de-reference the symlink to a host file
		return os.Lchown(path, h.UID, h.GID)
	}
}
