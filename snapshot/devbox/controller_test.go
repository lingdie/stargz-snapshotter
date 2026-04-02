package devbox

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/containerd/containerd/v2/core/snapshots"
	"github.com/containerd/errdefs"
)

func TestControllerLifecycle(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	snapshotDir := filepath.Join(root, "snapshots")
	if err := os.MkdirAll(snapshotDir, 0755); err != nil {
		t.Fatal(err)
	}

	mounts := newFakeMountTable()
	runner := newFakeRunner()
	controller, err := NewController(root, Config{VolumeGroup: "testvg"},
		WithRunner(runner),
		WithMountFunc(mounts.mount),
		WithUnmountFunc(mounts.unmount),
		WithMountInfoFunc(mounts.info),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer controller.Close()

	prepared, handled, err := controller.Prepare(ctx, PrepareRequest{
		SnapshotDir: snapshotDir,
		Key:         "active-key",
		Kind:        snapshots.KindActive,
		Labels: map[string]string{
			ContentIDLabel:    "content-1",
			StorageLimitLabel: "1Mi",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !handled {
		t.Fatal("expected devbox controller to handle prepare")
	}

	finalPath := filepath.Join(snapshotDir, "1")
	if err := prepared.Finalize(ctx, finalPath); err != nil {
		t.Fatal(err)
	}
	if err := prepared.Register(ctx, "active-key", "1", finalPath); err != nil {
		t.Fatal(err)
	}

	snapshot, err := controller.store.Snapshot(ctx, "active-key")
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.MountPath != finalPath {
		t.Fatalf("unexpected mount path %q", snapshot.MountPath)
	}
	content, err := controller.store.Content(ctx, "content-1")
	if err != nil {
		t.Fatal(err)
	}
	if content.SnapshotKey != "active-key" {
		t.Fatalf("unexpected snapshot key %q", content.SnapshotKey)
	}

	if handled, err := controller.HandleUpdate(ctx, "active-key", map[string]string{UnmountLVLabel: "true"}); err != nil || !handled {
		t.Fatalf("unexpected unmount result handled=%v err=%v", handled, err)
	}
	content, err = controller.store.Content(ctx, "content-1")
	if err != nil {
		t.Fatal(err)
	}
	if content.SnapshotKey != "" {
		t.Fatalf("expected snapshot key to be cleared, got %q", content.SnapshotKey)
	}

	if err := controller.RenameSnapshot(ctx, "active-key", "committed-key"); err != nil {
		t.Fatal(err)
	}
	if _, err := controller.store.Snapshot(ctx, "active-key"); !errdefs.IsNotFound(err) {
		t.Fatalf("old key should be gone, got %v", err)
	}
	if _, err := controller.store.Snapshot(ctx, "committed-key"); err != nil {
		t.Fatal(err)
	}

	if handled, err := controller.HandleUpdate(ctx, "committed-key", map[string]string{RemoveContentID: "content-1"}); err != nil || !handled {
		t.Fatalf("unexpected remove-content result handled=%v err=%v", handled, err)
	}
	content, err = controller.store.Content(ctx, "content-1")
	if err != nil {
		t.Fatal(err)
	}
	if content.Status != statusRemoved {
		t.Fatalf("expected removed status, got %q", content.Status)
	}

	if err := controller.RemoveSnapshot(ctx, "committed-key"); err != nil {
		t.Fatal(err)
	}
	if _, err := controller.store.Content(ctx, "content-1"); !errdefs.IsNotFound(err) {
		t.Fatalf("content should be deleted after removal, got %v", err)
	}

	if err := controller.Cleanup(ctx); err != nil {
		t.Fatal(err)
	}
	if len(runner.removed) != 1 {
		t.Fatalf("expected one LV removal, got %d", len(runner.removed))
	}
}

func TestVolumeNameIsStableAndSafe(t *testing.T) {
	name1 := volumeName("user/demo:1")
	name2 := volumeName("user/demo:1")
	if name1 != name2 {
		t.Fatalf("expected stable volume name, got %q and %q", name1, name2)
	}
	if strings.ContainsAny(name1, "/:") {
		t.Fatalf("unsafe volume name %q", name1)
	}
	if !strings.HasPrefix(name1, volumeNamePrefix) {
		t.Fatalf("expected %q to have prefix %q", name1, volumeNamePrefix)
	}
}

func TestParseLimit(t *testing.T) {
	tests := map[string]int64{
		"1Ki": 1024,
		"2Mi": 2 * 1024 * 1024,
		"3Gi": 3 * 1024 * 1024 * 1024,
		"4B":  4,
	}
	for raw, want := range tests {
		got, err := parseLimit(raw)
		if err != nil {
			t.Fatalf("parse %q: %v", raw, err)
		}
		if got != want {
			t.Fatalf("parse %q = %d, want %d", raw, got, want)
		}
	}
}

func TestCleanupSkipsThinPool(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	runner := newFakeRunner()
	runner.sizes["devbox-thinpool"] = 1024
	runner.sizes["devbox-content-1234"] = 1024
	runner.sizes["stargz-devbox-content-1234"] = 1024

	controller, err := NewController(root, Config{
		VolumeGroup:  "testvg",
		ThinPoolName: "devbox-thinpool",
	}, WithRunner(runner))
	if err != nil {
		t.Fatal(err)
	}
	defer controller.Close()

	if err := controller.Cleanup(ctx); err != nil {
		t.Fatal(err)
	}
	if len(runner.removed) != 1 {
		t.Fatalf("expected one removable LV, got %d", len(runner.removed))
	}
	if runner.removed[0] != "stargz-devbox-content-1234" {
		t.Fatalf("unexpected removed LV %q", runner.removed[0])
	}
	if _, ok := runner.sizes["devbox-thinpool"]; !ok {
		t.Fatal("thin pool should not be removed during cleanup")
	}
	if _, ok := runner.sizes["devbox-content-1234"]; !ok {
		t.Fatal("foreign devbox LV should not be removed during cleanup")
	}
}

func TestPrepareRecreatesMissingLVFromStaleMetadata(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	snapshotDir := filepath.Join(root, "snapshots")
	if err := os.MkdirAll(snapshotDir, 0755); err != nil {
		t.Fatal(err)
	}

	mounts := newFakeMountTable()
	runner := newFakeRunner()
	controller, err := NewController(root, Config{VolumeGroup: "testvg"},
		WithRunner(runner),
		WithMountFunc(mounts.mount),
		WithUnmountFunc(mounts.unmount),
		WithMountInfoFunc(mounts.info),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer controller.Close()

	contentID := "090a86d6-6585-4e7a-b4d4-2c274c060d01"
	staleLV := "devbox-090a86d6-6585-4e7a-b4d4--9e383ebb"
	if err := controller.store.Save(ctx,
		SnapshotRecord{
			Key:       "old-key",
			ID:        "12",
			ContentID: contentID,
			LVName:    staleLV,
			MountPath: filepath.Join(snapshotDir, "12"),
		},
		ContentRecord{
			ContentID:   contentID,
			LVName:      staleLV,
			SnapshotKey: "old-key",
			Status:      statusActive,
		},
	); err != nil {
		t.Fatal(err)
	}

	prepared, handled, err := controller.Prepare(ctx, PrepareRequest{
		SnapshotDir: snapshotDir,
		Key:         "active-key",
		Kind:        snapshots.KindActive,
		Labels: map[string]string{
			ContentIDLabel:    contentID,
			StorageLimitLabel: "1Gi",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !handled {
		t.Fatal("expected prepare to be handled")
	}

	finalPath := filepath.Join(snapshotDir, "13")
	if err := prepared.Finalize(ctx, finalPath); err != nil {
		t.Fatal(err)
	}
	if err := prepared.Register(ctx, "active-key", "13", finalPath); err != nil {
		t.Fatal(err)
	}

	if _, err := controller.store.Snapshot(ctx, "old-key"); !errdefs.IsNotFound(err) {
		t.Fatalf("expected stale snapshot to be removed, got %v", err)
	}

	content, err := controller.store.Content(ctx, contentID)
	if err != nil {
		t.Fatal(err)
	}
	if content.SnapshotKey != "active-key" {
		t.Fatalf("expected snapshot key to be rebound, got %q", content.SnapshotKey)
	}
	if content.LVName == staleLV {
		t.Fatalf("expected stale LV %q to be replaced", staleLV)
	}
	if !strings.HasPrefix(content.LVName, volumeNamePrefix) {
		t.Fatalf("expected replacement LV %q to use prefix %q", content.LVName, volumeNamePrefix)
	}
	if _, ok := runner.sizes[content.LVName]; !ok {
		t.Fatalf("expected replacement LV %q to be created", content.LVName)
	}
}

func TestCleanupContinuesWhenSingleLVRemovalFails(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	runner := newFakeRunner()
	runner.sizes["stargz-devbox-first"] = 1024
	runner.sizes["stargz-devbox-second"] = 1024
	runner.removeErrors["stargz-devbox-first"] = fmt.Errorf("lv busy")

	controller, err := NewController(root, Config{
		VolumeGroup:  "testvg",
		ThinPoolName: "devbox-thinpool",
	}, WithRunner(runner))
	if err != nil {
		t.Fatal(err)
	}
	defer controller.Close()

	if err := controller.Cleanup(ctx); err != nil {
		t.Fatalf("cleanup should continue on per-LV failure, got %v", err)
	}
	if len(runner.removed) != 1 || runner.removed[0] != "stargz-devbox-second" {
		t.Fatalf("expected only second LV to be removed successfully, got %v", runner.removed)
	}
	if _, ok := runner.sizes["stargz-devbox-first"]; !ok {
		t.Fatal("failed LV should remain for next cleanup")
	}
}

type fakeRunner struct {
	mu           sync.Mutex
	sizes        map[string]int64
	removed      []string
	removeErrors map[string]error
}

func newFakeRunner() *fakeRunner {
	return &fakeRunner{
		sizes:        make(map[string]int64),
		removeErrors: make(map[string]error),
	}
}

func (r *fakeRunner) CombinedOutput(ctx context.Context, name string, args ...string) ([]byte, error) {
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
				size = parseRunnerSize(args[i])
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
				size = parseRunnerSize(args[i])
			}
		}
		r.sizes[filepath.Base(args[len(args)-1])] = size
		return nil, nil
	case "lvs":
		if contains(args, "lv_size") {
			lvName := filepath.Base(args[len(args)-1])
			return []byte(fmt.Sprintf("%d\n", r.sizes[lvName])), nil
		}
		if contains(args, "lv_name") {
			lastArg := args[len(args)-1]
			if strings.HasPrefix(lastArg, "/dev/") {
				lvName := filepath.Base(lastArg)
				if _, ok := r.sizes[lvName]; !ok {
					return []byte(""), fmt.Errorf("Failed to find logical volume %q", strings.TrimPrefix(lastArg, "/dev/"))
				}
				return []byte(lvName + "\n"), nil
			}
			var out []string
			for lvName := range r.sizes {
				out = append(out, lvName)
			}
			return []byte(strings.Join(out, "\n")), nil
		}
	case "lvremove":
		lvName := filepath.Base(args[len(args)-1])
		if err := r.removeErrors[lvName]; err != nil {
			return nil, err
		}
		delete(r.sizes, lvName)
		r.removed = append(r.removed, lvName)
		return nil, nil
	}
	return nil, nil
}

type fakeMountTable struct {
	mu     sync.Mutex
	mounts map[string]string
}

func newFakeMountTable() *fakeMountTable {
	return &fakeMountTable{mounts: make(map[string]string)}
}

func (m *fakeMountTable) mount(source, target, fstype string, flags uintptr, data string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.mounts[target] = source
	return nil
}

func (m *fakeMountTable) unmount(target string, flags int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.mounts, target)
	return nil
}

func (m *fakeMountTable) info() ([]MountPoint, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []MountPoint
	for target, source := range m.mounts {
		out = append(out, MountPoint{Source: source, Mountpoint: target})
	}
	return out, nil
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func parseRunnerSize(raw string) int64 {
	raw = strings.TrimSuffix(strings.ToLower(raw), "b")
	value, _ := strconv.ParseInt(raw, 10, 64)
	return value
}
