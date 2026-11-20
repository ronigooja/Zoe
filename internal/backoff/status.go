package backoff

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"time"
)

const StatusPath = "/var/lib/zoe/server/model-retry-status.json"

// PublishedState omits scope identifiers; only aggregate scope counts are public.
type PublishedState struct {
	Consumer      string    `json:"consumer"`
	Scopes        int       `json:"scopes"`
	Failures      int       `json:"failures"`
	NextAttemptAt time.Time `json:"next_attempt_at"`
	AuthFailed    bool      `json:"auth_failed"`
	LastError     Kind      `json:"last_error"`
	UpdatedAt     time.Time `json:"updated_at"`
}

type Snapshot struct {
	PublishedAt time.Time        `json:"published_at"`
	States      []PublishedState `json:"states"`
}

func (s Store) Publish(ctx context.Context, path string, now time.Time) error {
	states, err := s.List(ctx)
	if err != nil {
		return err
	}
	group := make(map[string]PublishedState)
	for _, state := range states {
		item := group[state.Consumer]
		item.Consumer = state.Consumer
		item.Scopes++
		if state.Failures > item.Failures {
			item.Failures = state.Failures
		}
		if !state.NextAttemptAt.IsZero() && (item.NextAttemptAt.IsZero() || state.NextAttemptAt.Before(item.NextAttemptAt)) {
			item.NextAttemptAt = state.NextAttemptAt
		}
		if state.UpdatedAt.After(item.UpdatedAt) {
			item.UpdatedAt = state.UpdatedAt
			item.LastError = state.LastError
		}
		item.AuthFailed = item.AuthFailed || state.AuthFailed
		group[state.Consumer] = item
	}
	snapshot := Snapshot{PublishedAt: now.UTC(), States: []PublishedState{}}
	for _, consumer := range []string{"register", "steward", "ui_controller", "compact", "task_summary", "memory_write", "memory_ingest"} {
		if item, ok := group[consumer]; ok {
			snapshot.States = append(snapshot.States, item)
		}
	}
	data, err := json.Marshal(snapshot)
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".model-retry-status-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if err = file.Chmod(0644); err == nil {
		_, err = file.Write(data)
	}
	if err == nil {
		err = file.Sync()
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(file.Name(), path)
}

func ReadSnapshot(path string, now time.Time) (Snapshot, error) {
	var snapshot Snapshot
	file, err := os.Open(path)
	if err != nil {
		return snapshot, err
	}
	defer file.Close()
	err = json.NewDecoder(io.LimitReader(file, 1<<20)).Decode(&snapshot)
	if err != nil {
		return Snapshot{}, err
	}
	if snapshot.PublishedAt.IsZero() || now.Sub(snapshot.PublishedAt) > 24*time.Hour {
		return Snapshot{}, nil
	}
	return snapshot, nil
}
