// SPDX-License-Identifier: Apache-2.0

package storage

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// CleanSentinelName is the directory level half of the clean shutdown marker.
const CleanSentinelName = ".clean"

// ShutdownStateClean and ShutdownStateDirty are the two recorded outcomes of a
// shutdown. The design is double written on purpose: the sentinel file and the
// database flag are independent, so one damaged store cannot convince the next
// start that a crash was orderly.
const (
	ShutdownStateClean = "clean"
	ShutdownStateDirty = "dirty"
)

// CleanMarker reads and writes the shutdown state of a storage scope.
type CleanMarker struct {
	root  string
	audit *AuditSink
}

// NewCleanMarker binds a marker to a scope root and the ledger in that scope.
func NewCleanMarker(root string, audit *AuditSink) (*CleanMarker, error) {
	if strings.TrimSpace(root) == "" {
		return nil, fmt.Errorf("storage: clean marker root must not be empty")
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("storage: create marker root: %w", err)
	}
	return &CleanMarker{root: root, audit: audit}, nil
}

type sentinel struct {
	State    string    `json:"state"`
	PID      int       `json:"pid"`
	Recorded time.Time `json:"recorded"`
}

// State reports the recorded shutdown state. A missing sentinel and a missing
// database flag are both reported as dirty, because an absent marker is exactly
// what an abrupt kill leaves behind.
func (m *CleanMarker) State(ctx context.Context) (string, error) {
	if m.audit == nil {
		return m.sentinelState()
	}
	databaseState, err := m.audit.ShutdownState(ctx)
	if err != nil {
		return "", err
	}
	fileState, err := m.sentinelState()
	if err != nil {
		return "", err
	}
	if databaseState == ShutdownStateClean && fileState == ShutdownStateClean {
		return ShutdownStateClean, nil
	}
	return ShutdownStateDirty, nil
}

func (m *CleanMarker) sentinelState() (string, error) {
	raw, err := os.ReadFile(filepath.Join(m.root, CleanSentinelName))
	if err != nil {
		if os.IsNotExist(err) {
			return ShutdownStateDirty, nil
		}
		return "", fmt.Errorf("storage: read clean sentinel: %w", err)
	}
	var value sentinel
	if err := json.Unmarshal(raw, &value); err != nil {
		return ShutdownStateDirty, nil
	}
	if value.State != ShutdownStateClean {
		return ShutdownStateDirty, nil
	}
	return ShutdownStateClean, nil
}

// IsClean reports whether the previous shutdown was orderly.
func (m *CleanMarker) IsClean(ctx context.Context) (bool, error) {
	state, err := m.State(ctx)
	if err != nil {
		return false, err
	}
	return state == ShutdownStateClean, nil
}

// MarkClean records an orderly shutdown in both places.
func (m *CleanMarker) MarkClean(ctx context.Context) error {
	return m.mark(ctx, ShutdownStateClean)
}

// MarkDirty records that the shutdown did not finish, so the next start knows
// to run recovery.
func (m *CleanMarker) MarkDirty(ctx context.Context) error {
	return m.mark(ctx, ShutdownStateDirty)
}

func (m *CleanMarker) mark(ctx context.Context, state string) error {
	value := sentinel{State: state, PID: os.Getpid(), Recorded: time.Now().UTC()}
	encoded, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("storage: encode clean marker: %w", err)
	}
	if err := os.WriteFile(filepath.Join(m.root, CleanSentinelName), encoded, 0o600); err != nil {
		return fmt.Errorf("storage: write clean sentinel: %w", err)
	}
	if m.audit != nil {
		if err := m.audit.SetShutdownState(ctx, state); err != nil {
			return err
		}
	}
	return nil
}

// Remove deletes the sentinel, which is how a session scope is torn down.
func (m *CleanMarker) Remove() error {
	err := os.Remove(filepath.Join(m.root, CleanSentinelName))
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("storage: remove clean sentinel: %w", err)
	}
	return nil
}
