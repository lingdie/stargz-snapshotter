package devbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/containerd/errdefs"
	bolt "go.etcd.io/bbolt"
)

var (
	snapshotsBucket = []byte("snapshots")
	contentBucket   = []byte("content")
)

type SnapshotRecord struct {
	Key       string `json:"key"`
	ID        string `json:"id"`
	ContentID string `json:"content_id"`
	LVName    string `json:"lv_name"`
	MountPath string `json:"mount_path"`
}

type ContentRecord struct {
	ContentID   string `json:"content_id"`
	LVName      string `json:"lv_name"`
	SnapshotKey string `json:"snapshot_key"`
	Status      string `json:"status"`
}

const (
	statusActive  = "active"
	statusRemoved = "removed"
)

type metadataStore struct {
	db *bolt.DB
}

func newMetadataStore(root string) (*metadataStore, error) {
	if err := os.MkdirAll(root, 0700); err != nil {
		return nil, err
	}
	db, err := bolt.Open(filepath.Join(root, "devbox.db"), 0600, nil)
	if err != nil {
		return nil, err
	}
	store := &metadataStore{db: db}
	if err := store.update(func(tx *bolt.Tx) error {
		if _, err := tx.CreateBucketIfNotExists(snapshotsBucket); err != nil {
			return err
		}
		if _, err := tx.CreateBucketIfNotExists(contentBucket); err != nil {
			return err
		}
		return nil
	}); err != nil {
		db.Close()
		return nil, err
	}
	return store, nil
}

func (s *metadataStore) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func (s *metadataStore) view(fn func(tx *bolt.Tx) error) error {
	return s.db.View(fn)
}

func (s *metadataStore) update(fn func(tx *bolt.Tx) error) error {
	return s.db.Update(fn)
}

func getSnapshotRecord(tx *bolt.Tx, key string) (SnapshotRecord, error) {
	b := tx.Bucket(snapshotsBucket)
	if b == nil {
		return SnapshotRecord{}, fmt.Errorf("devbox snapshots bucket missing: %w", errdefs.ErrNotFound)
	}
	raw := b.Get([]byte(key))
	if raw == nil {
		return SnapshotRecord{}, fmt.Errorf("snapshot %q: %w", key, errdefs.ErrNotFound)
	}
	var record SnapshotRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		return SnapshotRecord{}, err
	}
	return record, nil
}

func putSnapshotRecord(tx *bolt.Tx, record SnapshotRecord) error {
	b := tx.Bucket(snapshotsBucket)
	if b == nil {
		return fmt.Errorf("devbox snapshots bucket missing: %w", errdefs.ErrNotFound)
	}
	raw, err := json.Marshal(record)
	if err != nil {
		return err
	}
	return b.Put([]byte(record.Key), raw)
}

func deleteSnapshotRecord(tx *bolt.Tx, key string) error {
	b := tx.Bucket(snapshotsBucket)
	if b == nil {
		return fmt.Errorf("devbox snapshots bucket missing: %w", errdefs.ErrNotFound)
	}
	return b.Delete([]byte(key))
}

func getContentRecord(tx *bolt.Tx, contentID string) (ContentRecord, error) {
	b := tx.Bucket(contentBucket)
	if b == nil {
		return ContentRecord{}, fmt.Errorf("devbox content bucket missing: %w", errdefs.ErrNotFound)
	}
	raw := b.Get([]byte(contentID))
	if raw == nil {
		return ContentRecord{}, fmt.Errorf("content %q: %w", contentID, errdefs.ErrNotFound)
	}
	var record ContentRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		return ContentRecord{}, err
	}
	return record, nil
}

func putContentRecord(tx *bolt.Tx, record ContentRecord) error {
	b := tx.Bucket(contentBucket)
	if b == nil {
		return fmt.Errorf("devbox content bucket missing: %w", errdefs.ErrNotFound)
	}
	raw, err := json.Marshal(record)
	if err != nil {
		return err
	}
	return b.Put([]byte(record.ContentID), raw)
}

func deleteContentRecord(tx *bolt.Tx, contentID string) error {
	b := tx.Bucket(contentBucket)
	if b == nil {
		return fmt.Errorf("devbox content bucket missing: %w", errdefs.ErrNotFound)
	}
	return b.Delete([]byte(contentID))
}

func (s *metadataStore) Snapshot(ctx context.Context, key string) (SnapshotRecord, error) {
	_ = ctx
	var record SnapshotRecord
	err := s.view(func(tx *bolt.Tx) error {
		var err error
		record, err = getSnapshotRecord(tx, key)
		return err
	})
	return record, err
}

func (s *metadataStore) Content(ctx context.Context, contentID string) (ContentRecord, error) {
	_ = ctx
	var record ContentRecord
	err := s.view(func(tx *bolt.Tx) error {
		var err error
		record, err = getContentRecord(tx, contentID)
		return err
	})
	return record, err
}

func (s *metadataStore) Save(ctx context.Context, snapshot SnapshotRecord, content ContentRecord) error {
	_ = ctx
	return s.update(func(tx *bolt.Tx) error {
		if err := putSnapshotRecord(tx, snapshot); err != nil {
			return err
		}
		return putContentRecord(tx, content)
	})
}

func (s *metadataStore) MarkContentRemoved(ctx context.Context, contentID string) error {
	_ = ctx
	return s.update(func(tx *bolt.Tx) error {
		record, err := getContentRecord(tx, contentID)
		if err != nil {
			return err
		}
		record.Status = statusRemoved
		return putContentRecord(tx, record)
	})
}

func (s *metadataStore) ClearContentSnapshot(ctx context.Context, contentID, snapshotKey string) error {
	_ = ctx
	return s.update(func(tx *bolt.Tx) error {
		record, err := getContentRecord(tx, contentID)
		if err != nil {
			return err
		}
		if record.SnapshotKey == snapshotKey {
			record.SnapshotKey = ""
			return putContentRecord(tx, record)
		}
		return nil
	})
}

func (s *metadataStore) RemoveSnapshot(ctx context.Context, key string) error {
	_ = ctx
	return s.update(func(tx *bolt.Tx) error {
		record, err := getSnapshotRecord(tx, key)
		if err != nil {
			if errdefs.IsNotFound(err) {
				return nil
			}
			return err
		}
		if err := deleteSnapshotRecord(tx, key); err != nil {
			return err
		}
		if record.ContentID == "" {
			return nil
		}
		content, err := getContentRecord(tx, record.ContentID)
		if err != nil {
			if errdefs.IsNotFound(err) {
				return nil
			}
			return err
		}
		if content.Status == statusRemoved {
			return deleteContentRecord(tx, content.ContentID)
		}
		if content.SnapshotKey == key {
			content.SnapshotKey = ""
			return putContentRecord(tx, content)
		}
		return nil
	})
}

func (s *metadataStore) RenameSnapshot(ctx context.Context, oldKey, newKey string) error {
	_ = ctx
	return s.update(func(tx *bolt.Tx) error {
		record, err := getSnapshotRecord(tx, oldKey)
		if err != nil {
			if errdefs.IsNotFound(err) {
				return nil
			}
			return err
		}
		if err := deleteSnapshotRecord(tx, oldKey); err != nil {
			return err
		}
		record.Key = newKey
		if err := putSnapshotRecord(tx, record); err != nil {
			return err
		}
		if record.ContentID == "" {
			return nil
		}
		content, err := getContentRecord(tx, record.ContentID)
		if err != nil {
			if errdefs.IsNotFound(err) {
				return nil
			}
			return err
		}
		if content.SnapshotKey == oldKey {
			content.SnapshotKey = newKey
			return putContentRecord(tx, content)
		}
		return nil
	})
}

func (s *metadataStore) ReferencedLVs(ctx context.Context) (map[string]struct{}, error) {
	_ = ctx
	lvs := make(map[string]struct{})
	err := s.view(func(tx *bolt.Tx) error {
		b := tx.Bucket(contentBucket)
		if b == nil {
			return fmt.Errorf("devbox content bucket missing: %w", errdefs.ErrNotFound)
		}
		return b.ForEach(func(_, value []byte) error {
			if value == nil {
				return nil
			}
			var record ContentRecord
			if err := json.Unmarshal(value, &record); err != nil {
				return err
			}
			if record.LVName != "" {
				lvs[record.LVName] = struct{}{}
			}
			return nil
		})
	})
	return lvs, err
}

func isNotFound(err error) bool {
	return err != nil && (errdefs.IsNotFound(err) || errors.Is(err, os.ErrNotExist))
}
