package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/liuzhixin405/cove-agent/internal/api"
	"github.com/liuzhixin405/cove-agent/internal/filelock"
	"github.com/liuzhixin405/cove-agent/internal/fsatomic"
)

const queueVersion = 1

// QueueSnapshot preserves requests without authorizing their execution.
type QueueSnapshot struct {
	Version   int           `json:"version"`
	ID        string        `json:"id"`
	SessionID string        `json:"session_id"`
	Cwd       string        `json:"cwd"`
	OwnerPID  int           `json:"owner_pid"`
	Current   *api.Message  `json:"current,omitempty"`
	Pending   []api.Message `json:"pending,omitempty"`
	UpdatedAt time.Time     `json:"updated_at"`
}

// QueueStore stores independently owned task queues in dir.
type QueueStore struct{ dir string }

// OwnerRunning conservatively protects queues still owned by another process.
func (snapshot QueueSnapshot) OwnerRunning() bool { return queueOwnerRunning(snapshot.OwnerPID) }

// NewQueueStore creates a store without reading or starting saved requests.
func NewQueueStore(dir string) *QueueStore { return &QueueStore{dir: dir} }

func (store *QueueStore) path(id string) (string, error) {
	key, err := writableKey(id)
	if err != nil {
		return "", err
	}
	if key != id {
		return "", fmt.Errorf("task queue ID must be a plain name: %q", id)
	}
	return filepath.Join(store.dir, key+".json"), nil
}

// Save atomically writes a versioned snapshot, including attachment payloads.
func (store *QueueStore) Save(snapshot QueueSnapshot) error {
	path, err := store.path(snapshot.ID)
	if err != nil {
		return err
	}
	snapshot.Version = queueVersion
	snapshot.Cwd = NormalizeProjectDir(snapshot.Cwd)
	snapshot.UpdatedAt = time.Now()
	data, err := json.Marshal(snapshot)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(store.dir, 0700); err != nil {
		return err
	}
	return fsatomic.WriteFile(path, data, 0600)
}

// Load reads a snapshot and rejects unknown protocol versions.
func (store *QueueStore) Load(id string) (*QueueSnapshot, error) {
	path, err := store.path(id)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var snapshot QueueSnapshot
	if err := json.Unmarshal(data, &snapshot); err != nil {
		return nil, err
	}
	if snapshot.Version != queueVersion || snapshot.ID != id {
		return nil, fmt.Errorf("invalid task queue snapshot %q (version %d)", id, snapshot.Version)
	}
	return &snapshot, nil
}

// List returns this project's snapshots, newest first, without executing them.
func (store *QueueStore) List(cwd string) ([]QueueSnapshot, error) {
	files, err := os.ReadDir(store.dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	cwd = NormalizeProjectDir(cwd)
	var snapshots []QueueSnapshot
	var errs []error
	for _, file := range files {
		if file.IsDir() || filepath.Ext(file.Name()) != ".json" || fsatomic.IsTempName(file.Name()) {
			continue
		}
		id := keyOfFile(file.Name())
		snapshot, err := store.Load(id)
		if err != nil {
			errs = append(errs, fmt.Errorf("task queue %s: %w", id, err))
			continue
		}
		if NormalizeProjectDir(snapshot.Cwd) == cwd && (snapshot.Current != nil || len(snapshot.Pending) > 0) {
			snapshots = append(snapshots, *snapshot)
		}
	}
	sort.Slice(snapshots, func(first, second int) bool { return snapshots[first].UpdatedAt.After(snapshots[second].UpdatedAt) })
	return snapshots, errors.Join(errs...)
}

// Delete removes only the named queue snapshot.
func (store *QueueStore) Delete(id string) error {
	path, err := store.path(id)
	if err != nil {
		return err
	}
	err = os.Remove(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// Claim transfers an inactive snapshot only within its original project and session.
func (store *QueueStore) Claim(id, cwd, sessionID string) (*QueueSnapshot, error) {
	path, err := store.path(id)
	if err != nil {
		return nil, err
	}
	release, err := filelock.Acquire(path+".claim.lock", filelock.DefaultWait, filelock.DefaultStale)
	if err != nil {
		return nil, err
	}
	defer release()
	snapshot, err := store.Load(id)
	if err != nil {
		return nil, err
	}
	if NormalizeProjectDir(snapshot.Cwd) != NormalizeProjectDir(cwd) || snapshot.SessionID != sessionID {
		return nil, fmt.Errorf("task queue belongs to another project or session %s", snapshot.SessionID)
	}
	if snapshot.OwnerRunning() {
		return nil, fmt.Errorf("task queue is owned by running process %d", snapshot.OwnerPID)
	}
	snapshot.OwnerPID = os.Getpid()
	if err := store.Save(*snapshot); err != nil {
		return nil, err
	}
	return snapshot, nil
}
