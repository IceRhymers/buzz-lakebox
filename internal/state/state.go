// Package state persists the provider's own agent→sandbox mapping.
//
// This file is the ONLY durable reuse key a redeploy has: the desktop
// does not echo backend_agent_id back in deploy payloads (block/buzz
// backend.rs provider_deploy sends only op/request_id/agent/
// provider_config), and Lakebox does not persist caller-set sandbox
// names (live-verified 2026-07-26: `sandbox list`/`status` echo the
// sandbox id as "name"), so the PLAN §4.1 name-prefix match can never
// find a sandbox created by an earlier run. Without this store, every
// redeploy would orphan the previous sandbox — still running, with
// --no-autostop, billing forever.
//
// Losing the file is not fatal: deploy falls back to creating a fresh
// sandbox (and the stale one must be cleaned up manually via
// `databricks sandbox list/delete`). Entries are keyed by
// profile + npub so the same agent identity deployed to two workspaces
// tracks two sandboxes.
package state

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Entry records where one agent identity was last deployed.
type Entry struct {
	SandboxID string    `json:"sandbox_id"`
	Profile   string    `json:"profile"`
	UpdatedAt time.Time `json:"updated_at"`
}

// fileFormat is the on-disk shape. Version guards future migrations.
type fileFormat struct {
	Version int              `json:"version"`
	Agents  map[string]Entry `json:"agents"`
}

const currentVersion = 1

// Store reads and writes the agents.json mapping at Path. The zero
// value is unusable; construct via NewDefault or with an explicit Path.
type Store struct {
	Path string

	mu sync.Mutex
}

// Key builds the map key for one (profile, npub) identity.
func Key(profile, npub string) string {
	return profile + ":" + npub
}

// DefaultPath returns $XDG_STATE_HOME/buzz-lakebox/agents.json, falling
// back to ~/.local/state/buzz-lakebox/agents.json.
func DefaultPath() (string, error) {
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("state: resolve home dir: %w", err)
		}
		base = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(base, "buzz-lakebox", "agents.json"), nil
}

// NewDefault returns a Store at DefaultPath, or nil when no home
// directory can be resolved (callers treat a nil store as "no
// persistence available" and skip lookup/record).
func NewDefault() *Store {
	path, err := DefaultPath()
	if err != nil {
		return nil
	}
	return &Store{Path: path}
}

// Lookup returns the entry for key. A missing file or missing key is
// (Entry{}, false, nil) — only a present-but-unreadable file is an error.
func (s *Store) Lookup(key string) (Entry, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := s.load()
	if err != nil {
		return Entry{}, false, err
	}
	e, ok := f.Agents[key]
	return e, ok, nil
}

// Record upserts key→entry with an atomic write (temp file + rename,
// 0600 file / 0700 dir) so a crash can never leave a torn mapping.
func (s *Store) Record(key string, e Entry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := s.load()
	if err != nil {
		return err
	}
	if e.UpdatedAt.IsZero() {
		e.UpdatedAt = time.Now().UTC()
	}
	f.Agents[key] = e
	return s.save(f)
}

func (s *Store) load() (fileFormat, error) {
	f := fileFormat{Version: currentVersion, Agents: map[string]Entry{}}
	data, err := os.ReadFile(s.Path)
	if err != nil {
		if os.IsNotExist(err) {
			return f, nil
		}
		return f, fmt.Errorf("state: read %s: %w", s.Path, err)
	}
	if err := json.Unmarshal(data, &f); err != nil {
		return f, fmt.Errorf("state: parse %s: %w", s.Path, err)
	}
	if f.Agents == nil {
		f.Agents = map[string]Entry{}
	}
	return f, nil
}

func (s *Store) save(f fileFormat) error {
	f.Version = currentVersion
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return fmt.Errorf("state: marshal: %w", err)
	}
	dir := filepath.Dir(s.Path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("state: mkdir %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".agents-*.json.tmp")
	if err != nil {
		return fmt.Errorf("state: temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }() // no-op after successful rename
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("state: chmod temp: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("state: write temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("state: close temp: %w", err)
	}
	if err := os.Rename(tmpName, s.Path); err != nil {
		return fmt.Errorf("state: rename into place: %w", err)
	}
	return nil
}
