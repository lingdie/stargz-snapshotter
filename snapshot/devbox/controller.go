package devbox

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/containerd/containerd/v2/core/snapshots"
	"github.com/containerd/errdefs"
	"github.com/moby/sys/mountinfo"
)

const DefaultVolumeGroup = "devbox-lvm-vg"

type Config struct {
	VolumeGroup  string
	ThinPoolName string
}

type PrepareRequest struct {
	SnapshotDir    string
	Key            string
	Kind           snapshots.Kind
	Labels         map[string]string
	ParentUpperdir string
}

type PreparedSnapshot struct {
	TempDir  string
	Finalize func(ctx context.Context, finalPath string) error
	Register func(ctx context.Context, key, id, finalPath string) error
	Abort    func(ctx context.Context) error
}

type Runner interface {
	CombinedOutput(ctx context.Context, name string, args ...string) ([]byte, error)
}

type MountPoint struct {
	Source     string
	Mountpoint string
}

type Option func(*Controller)

type Controller struct {
	root         string
	volumeGroup  string
	thinPoolName string
	store        *metadataStore
	runner       Runner
	mountFn      func(source, target, fstype string, flags uintptr, data string) error
	unmountFn    func(target string, flags int) error
	mountInfoFn  func() ([]MountPoint, error)
}

type execRunner struct{}

func (execRunner) CombinedOutput(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

func WithRunner(runner Runner) Option {
	return func(c *Controller) {
		c.runner = runner
	}
}

func WithMountFunc(fn func(source, target, fstype string, flags uintptr, data string) error) Option {
	return func(c *Controller) {
		c.mountFn = fn
	}
}

func WithUnmountFunc(fn func(target string, flags int) error) Option {
	return func(c *Controller) {
		c.unmountFn = fn
	}
}

func WithMountInfoFunc(fn func() ([]MountPoint, error)) Option {
	return func(c *Controller) {
		c.mountInfoFn = fn
	}
}

func NewController(root string, cfg Config, opts ...Option) (*Controller, error) {
	store, err := newMetadataStore(root)
	if err != nil {
		return nil, err
	}
	controller := &Controller{
		root:         root,
		volumeGroup:  cfg.VolumeGroup,
		thinPoolName: cfg.ThinPoolName,
		store:        store,
		runner:       execRunner{},
		mountFn:      syscall.Mount,
		unmountFn:    syscall.Unmount,
		mountInfoFn: func() ([]MountPoint, error) {
			mounts, err := mountinfo.GetMounts(nil)
			if err != nil {
				return nil, err
			}
			out := make([]MountPoint, 0, len(mounts))
			for _, m := range mounts {
				out = append(out, MountPoint{
					Source:     m.Source,
					Mountpoint: m.Mountpoint,
				})
			}
			return out, nil
		},
	}
	if controller.volumeGroup == "" {
		controller.volumeGroup = DefaultVolumeGroup
	}
	for _, opt := range opts {
		opt(controller)
	}
	return controller, nil
}

func (c *Controller) Close() error {
	if c == nil {
		return nil
	}
	return c.store.Close()
}

func (c *Controller) Handles(labels map[string]string) bool {
	if c == nil {
		return false
	}
	_, hasContentID := labels[ContentIDLabel]
	_, hasLimit := labels[StorageLimitLabel]
	return hasContentID && hasLimit
}

func (c *Controller) Prepare(ctx context.Context, req PrepareRequest) (*PreparedSnapshot, bool, error) {
	if !c.Handles(req.Labels) || req.Kind != snapshots.KindActive {
		return nil, false, nil
	}

	contentID := req.Labels[ContentIDLabel]
	sizeBytes, err := parseLimit(req.Labels[StorageLimitLabel])
	if err != nil {
		return nil, true, err
	}

	state := &preparedState{
		controller: c,
		contentID:  contentID,
	}
	if state.tempDir, err = os.MkdirTemp(req.SnapshotDir, "new-"); err != nil {
		return nil, true, err
	}

	content, err := c.store.Content(ctx, contentID)
	if err == nil {
		if content.Status == statusRemoved {
			_ = os.RemoveAll(state.tempDir)
			return nil, true, fmt.Errorf("devbox content %q is already scheduled for removal", contentID)
		}
		if content.SnapshotKey != "" && content.SnapshotKey != req.Key {
			_ = os.RemoveAll(state.tempDir)
			return nil, true, fmt.Errorf("devbox content %q is already attached to snapshot %q", contentID, content.SnapshotKey)
		}
		if content.LVName == "" {
			_ = os.RemoveAll(state.tempDir)
			return nil, true, fmt.Errorf("devbox content %q has no logical volume recorded", contentID)
		}
		state.lvName = content.LVName
	} else if !errdefs.IsNotFound(err) {
		_ = os.RemoveAll(state.tempDir)
		return nil, true, err
	} else {
		state.lvName = volumeName(contentID)
		state.newVolume = true
	}

	if state.newVolume {
		if err := c.createVolume(ctx, state.lvName, sizeBytes); err != nil {
			_ = os.RemoveAll(state.tempDir)
			return nil, true, err
		}
		state.created = true
		if err := c.makeFilesystem(ctx, state.lvName); err != nil {
			_ = state.abort(ctx)
			_ = os.RemoveAll(state.tempDir)
			return nil, true, err
		}
	} else if err := c.ensureVolumeSize(ctx, state.lvName, sizeBytes); err != nil {
		_ = state.abort(ctx)
		_ = os.RemoveAll(state.tempDir)
		return nil, true, err
	}

	if err := c.mountVolume(ctx, state.lvName, state.tempDir); err != nil {
		_ = state.abort(ctx)
		_ = os.RemoveAll(state.tempDir)
		return nil, true, err
	}
	state.mountedTemp = true

	if err := ensureSnapshotLayout(state.tempDir, req.Kind); err != nil {
		_ = state.abort(ctx)
		_ = os.RemoveAll(state.tempDir)
		return nil, true, err
	}

	if _, ok := req.Labels[PrivateImageLabel]; ok && req.ParentUpperdir != "" && state.newVolume {
		if err := copyDir(req.ParentUpperdir, filepath.Join(state.tempDir, "fs")); err != nil {
			_ = state.abort(ctx)
			_ = os.RemoveAll(state.tempDir)
			return nil, true, err
		}
	}

	return &PreparedSnapshot{
		TempDir:  state.tempDir,
		Finalize: state.finalize,
		Register: state.register,
		Abort:    state.abort,
	}, true, nil
}

func (c *Controller) HandleUpdate(ctx context.Context, key string, labels map[string]string) (bool, error) {
	if c == nil {
		return false, nil
	}
	if labels[UnmountLVLabel] == "true" {
		record, err := c.store.Snapshot(ctx, key)
		if err != nil {
			return true, err
		}
		if record.ContentID != "" {
			if err := c.store.ClearContentSnapshot(ctx, record.ContentID, key); err != nil {
				return true, err
			}
		}
		return true, c.unmountIfMounted(record.MountPath)
	}
	if contentID := labels[RemoveContentID]; contentID != "" {
		return true, c.store.MarkContentRemoved(ctx, contentID)
	}
	return false, nil
}

func (c *Controller) RenameSnapshot(ctx context.Context, oldKey, newKey string) error {
	if c == nil {
		return nil
	}
	return c.store.RenameSnapshot(ctx, oldKey, newKey)
}

func (c *Controller) RemoveSnapshot(ctx context.Context, key string) error {
	if c == nil {
		return nil
	}
	return c.store.RemoveSnapshot(ctx, key)
}

func (c *Controller) CleanupSnapshotDirectory(ctx context.Context, dir string) error {
	if c == nil {
		return nil
	}
	source, ok, err := c.mountSourceForTarget(dir)
	if err != nil || !ok {
		return err
	}
	lvs, err := c.listVolumes(ctx)
	if err != nil {
		return err
	}
	for _, lvName := range lvs {
		if sameDevice(source, c.devicePath(lvName)) {
			return c.unmountIfMounted(dir)
		}
	}
	return nil
}

func (c *Controller) Cleanup(ctx context.Context) error {
	if c == nil {
		return nil
	}
	referenced, err := c.store.ReferencedLVs(ctx)
	if err != nil {
		return err
	}
	lvs, err := c.listVolumes(ctx)
	if err != nil {
		return err
	}
	for _, lvName := range lvs {
		if _, ok := referenced[lvName]; ok {
			continue
		}
		mounts, err := c.mountTargetsForDevice(ctx, c.devicePath(lvName))
		if err != nil {
			return err
		}
		for _, target := range mounts {
			if err := c.unmountIfMounted(target); err != nil {
				return err
			}
		}
		if err := c.removeVolume(ctx, lvName); err != nil {
			return err
		}
	}
	return nil
}

type preparedState struct {
	controller   *Controller
	contentID    string
	lvName       string
	tempDir      string
	finalPath    string
	newVolume    bool
	created      bool
	mountedTemp  bool
	mountedFinal bool
}

func (p *preparedState) finalize(ctx context.Context, finalPath string) error {
	if p.mountedTemp {
		if err := p.controller.unmountIfMounted(p.tempDir); err != nil {
			return err
		}
		p.mountedTemp = false
	}
	if err := os.Rename(p.tempDir, finalPath); err != nil {
		return err
	}
	p.tempDir = ""
	p.finalPath = finalPath
	if err := p.controller.mountVolume(ctx, p.lvName, finalPath); err != nil {
		return err
	}
	p.mountedFinal = true
	return ensureSnapshotLayout(finalPath, snapshots.KindActive)
}

func (p *preparedState) register(ctx context.Context, key, id, finalPath string) error {
	return p.controller.store.Save(ctx,
		SnapshotRecord{
			Key:       key,
			ID:        id,
			ContentID: p.contentID,
			LVName:    p.lvName,
			MountPath: finalPath,
		},
		ContentRecord{
			ContentID:   p.contentID,
			LVName:      p.lvName,
			SnapshotKey: key,
			Status:      statusActive,
		},
	)
}

func (p *preparedState) abort(ctx context.Context) error {
	if p.mountedFinal {
		if err := p.controller.unmountIfMounted(p.finalPath); err != nil {
			return err
		}
		p.mountedFinal = false
	}
	if p.mountedTemp {
		if err := p.controller.unmountIfMounted(p.tempDir); err != nil {
			return err
		}
		p.mountedTemp = false
	}
	if p.created {
		return p.controller.forceRemoveVolume(ctx, p.lvName)
	}
	return nil
}

func (c *Controller) run(ctx context.Context, name string, args ...string) ([]byte, error) {
	out, err := c.runner.CombinedOutput(ctx, name, args...)
	if err != nil {
		return out, fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return out, nil
}

func (c *Controller) createVolume(ctx context.Context, lvName string, sizeBytes int64) error {
	size := fmt.Sprintf("%db", sizeBytes)
	if c.thinPoolName != "" {
		_, err := c.run(ctx, "lvcreate", "--yes", "-V", size, "-T", fmt.Sprintf("%s/%s", c.volumeGroup, c.thinPoolName), "-n", lvName)
		return err
	}
	_, err := c.run(ctx, "lvcreate", "--yes", "-L", size, "-n", lvName, c.volumeGroup)
	return err
}

func (c *Controller) ensureVolumeSize(ctx context.Context, lvName string, requested int64) error {
	current, err := c.volumeSize(ctx, lvName)
	if err != nil {
		return err
	}
	if current >= requested {
		return nil
	}
	size := fmt.Sprintf("%db", requested)
	if c.thinPoolName != "" {
		if _, err := c.run(ctx, "lvresize", "--yes", "-V", size, c.devicePath(lvName)); err != nil {
			return err
		}
	} else if _, err := c.run(ctx, "lvresize", "--yes", "-L", size, c.devicePath(lvName)); err != nil {
		return err
	}
	_, err = c.run(ctx, "resize2fs", c.devicePath(lvName))
	return err
}

func (c *Controller) volumeSize(ctx context.Context, lvName string) (int64, error) {
	out, err := c.run(ctx, "lvs", "--noheadings", "--units", "b", "--nosuffix", "-o", "lv_size", c.devicePath(lvName))
	if err != nil {
		return 0, err
	}
	trimmed := strings.TrimSpace(string(out))
	if trimmed == "" {
		return 0, fmt.Errorf("empty lv size for %q", lvName)
	}
	return strconv.ParseInt(strings.Fields(trimmed)[0], 10, 64)
}

func (c *Controller) makeFilesystem(ctx context.Context, lvName string) error {
	_, err := c.run(ctx, "mkfs.ext4", "-F", c.devicePath(lvName))
	return err
}

func (c *Controller) mountVolume(ctx context.Context, lvName, target string) error {
	if err := os.MkdirAll(target, 0755); err != nil {
		return err
	}
	return c.mountFn(c.devicePath(lvName), target, "ext4", 0, "")
}

func (c *Controller) unmountIfMounted(path string) error {
	if path == "" {
		return nil
	}
	_, ok, err := c.mountSourceForTarget(path)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	return c.unmountFn(path, 0)
}

func (c *Controller) removeVolume(ctx context.Context, lvName string) error {
	_, err := c.run(ctx, "lvremove", "--yes", "--force", c.devicePath(lvName))
	return err
}

func (c *Controller) forceRemoveVolume(ctx context.Context, lvName string) error {
	_, err := c.run(ctx, "lvremove", "--yes", "--force", c.devicePath(lvName))
	return err
}

func (c *Controller) listVolumes(ctx context.Context) ([]string, error) {
	out, err := c.run(ctx, "lvs", "--noheadings", "-o", "lv_name", c.volumeGroup)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, line := range strings.Split(string(out), "\n") {
		name := strings.TrimSpace(line)
		if name == "" || name == c.thinPoolName {
			continue
		}
		if strings.HasPrefix(name, "devbox-") {
			names = append(names, name)
		}
	}
	return names, nil
}

func (c *Controller) devicePath(lvName string) string {
	return filepath.Join("/dev", c.volumeGroup, lvName)
}

func (c *Controller) mountSourceForTarget(target string) (string, bool, error) {
	mounts, err := c.mountInfoFn()
	if err != nil {
		return "", false, err
	}
	for _, m := range mounts {
		if m.Mountpoint == target {
			return m.Source, true, nil
		}
	}
	return "", false, nil
}

func (c *Controller) mountTargetsForDevice(ctx context.Context, device string) ([]string, error) {
	_ = ctx
	mounts, err := c.mountInfoFn()
	if err != nil {
		return nil, err
	}
	var targets []string
	for _, m := range mounts {
		if sameDevice(m.Source, device) {
			targets = append(targets, m.Mountpoint)
		}
	}
	return targets, nil
}

func parseLimit(raw string) (int64, error) {
	parse := func(value string, multiplier int64) (int64, error) {
		v, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return 0, err
		}
		if v <= 0 {
			return 0, fmt.Errorf("devbox storage limit must be greater than zero")
		}
		return v * multiplier, nil
	}
	switch {
	case strings.HasSuffix(raw, "Gi"):
		return parse(strings.TrimSuffix(raw, "Gi"), 1024*1024*1024)
	case strings.HasSuffix(raw, "Mi"):
		return parse(strings.TrimSuffix(raw, "Mi"), 1024*1024)
	case strings.HasSuffix(raw, "Ki"):
		return parse(strings.TrimSuffix(raw, "Ki"), 1024)
	case strings.HasSuffix(raw, "B"):
		return parse(strings.TrimSuffix(raw, "B"), 1)
	default:
		return 0, fmt.Errorf("invalid devbox storage limit %q", raw)
	}
}

func ensureSnapshotLayout(root string, kind snapshots.Kind) error {
	if err := os.MkdirAll(filepath.Join(root, "fs"), 0755); err != nil {
		return err
	}
	if kind == snapshots.KindActive {
		if err := os.MkdirAll(filepath.Join(root, "work"), 0711); err != nil {
			return err
		}
	}
	return nil
}

func copyDir(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		target := filepath.Join(dst, rel)
		info, err := d.Info()
		if err != nil {
			return err
		}
		mode := info.Mode()
		switch {
		case mode.IsDir():
			return os.MkdirAll(target, mode.Perm())
		case mode&os.ModeSymlink != 0:
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			return os.Symlink(link, target)
		case mode.IsRegular():
			return copyFile(path, target, mode.Perm())
		default:
			return nil
		}
	})
}

func copyFile(src, dst string, perm fs.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_RDWR|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	defer out.Close()

	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Chmod(perm)
}

func volumeName(contentID string) string {
	sum := sha256.Sum256([]byte(contentID))
	safe := make([]rune, 0, len(contentID))
	for _, r := range contentID {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' {
			safe = append(safe, r)
			continue
		}
		safe = append(safe, '-')
	}
	prefix := strings.Trim(strings.TrimSpace(string(safe)), "-")
	if prefix == "" {
		prefix = "content"
	}
	if len(prefix) > 24 {
		prefix = prefix[:24]
	}
	return fmt.Sprintf("devbox-%s-%s", prefix, hex.EncodeToString(sum[:4]))
}

func sameDevice(left, right string) bool {
	if left == right {
		return true
	}
	resolvedLeft, errLeft := filepath.EvalSymlinks(left)
	resolvedRight, errRight := filepath.EvalSymlinks(right)
	if errLeft == nil && errRight == nil {
		return resolvedLeft == resolvedRight
	}
	return false
}
