package snapshot

import (
	"context"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/containerd/containerd/v2/core/snapshots"
	snapshotdevbox "github.com/containerd/stargz-snapshotter/snapshot/devbox"
)

func TestDevboxSnapshotLifecycle(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	runner := newSnapshotFakeRunner()
	mounts := newSnapshotFakeMountTable()

	controller, err := snapshotdevbox.NewController(root, snapshotdevbox.Config{VolumeGroup: "testvg"},
		snapshotdevbox.WithRunner(runner),
		snapshotdevbox.WithMountFunc(mounts.mount),
		snapshotdevbox.WithUnmountFunc(mounts.unmount),
		snapshotdevbox.WithMountInfoFunc(mounts.info),
	)
	if err != nil {
		t.Fatal(err)
	}

	sn, err := NewSnapshotter(ctx, root, dummyFileSystem(), WithDevboxController(controller))
	if err != nil {
		t.Fatal(err)
	}
	defer sn.Close()

	key := "active-key"
	mountsOut, err := sn.Prepare(ctx, key, "", snapshots.WithLabels(map[string]string{
		snapshotdevbox.ContentIDLabel:    "content-1",
		snapshotdevbox.StorageLimitLabel: "1Mi",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if len(mountsOut) != 1 {
		t.Fatalf("unexpected mount count %d", len(mountsOut))
	}
	expectedSource := filepath.Join(root, "snapshots", "1", "fs")
	if mountsOut[0].Source != expectedSource {
		t.Fatalf("unexpected upperdir source %q", mountsOut[0].Source)
	}

	if err := sn.Commit(ctx, "base", key); err != nil {
		t.Fatal(err)
	}
	if _, err := sn.Update(ctx, snapshots.Info{
		Name: "base",
		Labels: map[string]string{
			snapshotdevbox.UnmountLVLabel: "true",
		},
	}, "labels."+snapshotdevbox.UnmountLVLabel); err != nil {
		t.Fatal(err)
	}
	if _, err := sn.Update(ctx, snapshots.Info{
		Name: "base",
		Labels: map[string]string{
			snapshotdevbox.RemoveContentID: "content-1",
		},
	}, "labels."+snapshotdevbox.RemoveContentID); err != nil {
		t.Fatal(err)
	}
	if err := sn.Remove(ctx, "base"); err != nil {
		t.Fatal(err)
	}
	if len(runner.removed) != 1 {
		t.Fatalf("expected one LV cleanup, got %d", len(runner.removed))
	}
}

type snapshotFakeRunner struct {
	mu      sync.Mutex
	sizes   map[string]int64
	removed []string
}

func newSnapshotFakeRunner() *snapshotFakeRunner {
	return &snapshotFakeRunner{sizes: make(map[string]int64)}
}

func (r *snapshotFakeRunner) CombinedOutput(ctx context.Context, name string, args ...string) ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	switch name {
	case "lvcreate":
		var lvName string
		var size int64
		for i := 0; i < len(args); i++ {
			switch args[i] {
			case "-n":
				i++
				lvName = args[i]
			case "-L", "-V":
				i++
				size = snapshotParseSize(args[i])
			}
		}
		r.sizes[lvName] = size
		return nil, nil
	case "mkfs.ext4", "resize2fs":
		return nil, nil
	case "lvresize":
		var size int64
		for i := 0; i < len(args); i++ {
			switch args[i] {
			case "-L", "-V":
				i++
				size = snapshotParseSize(args[i])
			}
		}
		r.sizes[filepath.Base(args[len(args)-1])] = size
		return nil, nil
	case "lvs":
		if snapshotContains(args, "lv_size") {
			lvName := filepath.Base(args[len(args)-1])
			return []byte(fmt.Sprintf("%d\n", r.sizes[lvName])), nil
		}
		if snapshotContains(args, "lv_name") {
			var out []string
			for lvName := range r.sizes {
				out = append(out, lvName)
			}
			return []byte(strings.Join(out, "\n")), nil
		}
	case "lvremove":
		lvName := filepath.Base(args[len(args)-1])
		delete(r.sizes, lvName)
		r.removed = append(r.removed, lvName)
		return nil, nil
	}
	return nil, nil
}

type snapshotFakeMountTable struct {
	mu     sync.Mutex
	mounts map[string]string
}

func newSnapshotFakeMountTable() *snapshotFakeMountTable {
	return &snapshotFakeMountTable{mounts: make(map[string]string)}
}

func (m *snapshotFakeMountTable) mount(source, target, fstype string, flags uintptr, data string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.mounts[target] = source
	return nil
}

func (m *snapshotFakeMountTable) unmount(target string, flags int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.mounts, target)
	return nil
}

func (m *snapshotFakeMountTable) info() ([]snapshotdevbox.MountPoint, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []snapshotdevbox.MountPoint
	for target, source := range m.mounts {
		out = append(out, snapshotdevbox.MountPoint{Source: source, Mountpoint: target})
	}
	return out, nil
}

func snapshotContains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func snapshotParseSize(raw string) int64 {
	raw = strings.TrimSuffix(strings.ToLower(raw), "b")
	value, _ := strconv.ParseInt(raw, 10, 64)
	return value
}
