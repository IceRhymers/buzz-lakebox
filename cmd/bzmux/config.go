package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/IceRhymers/buzz-lakebox/internal/muxcfg"
)

// resolveConfigPath returns the path to mcp-mux.json using Decision C1:
// executable-relative first, then $HOME/.buzz-backend/mcp-mux.json as fallback.
func resolveConfigPath() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("os.Executable: %w", err)
	}
	p := filepath.Join(filepath.Dir(exe), "mcp-mux.json")
	if _, err := os.Stat(p); err == nil {
		return p, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("home dir: %w", err)
	}
	return filepath.Join(home, ".buzz-backend", "mcp-mux.json"), nil
}

// loadConfig reads and parses mcp-mux.json at path.  It returns actionable
// errors for missing files, bad JSON, and empty server lists.
// §5B validations (name collisions, "__", 64-byte budget) are run at
// catalog-merge time (after child initialization) rather than here, because
// they require the actual tool lists from the children.
func loadConfig(path string) (*muxcfg.Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var cfg muxcfg.Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if len(cfg.Servers) == 0 {
		return nil, fmt.Errorf("%s: no servers configured", path)
	}
	// Basic structural validation: each server must have a name and command.
	seen := make(map[string]bool)
	for i, srv := range cfg.Servers {
		if srv.Name == "" {
			return nil, fmt.Errorf("%s: server[%d] missing name", path, i)
		}
		if srv.Command == "" {
			return nil, fmt.Errorf("%s: server %q missing command", path, srv.Name)
		}
		if seen[srv.Name] {
			return nil, fmt.Errorf("%s: duplicate server name %q", path, srv.Name)
		}
		seen[srv.Name] = true
	}
	return &cfg, nil
}

// buildChildEnv builds the environment slice for a child process from its
// per-child allowlist (§5C, Decision B1 — least-privilege).
//
// Base process hygiene: PATH and HOME are forwarded unconditionally so
// children can resolve executables and find their home directory. TMPDIR is
// forwarded if present in bzmux's own env.  These are non-secret process
// hygiene vars, distinct from the secret allowlist — secrets still only reach
// children that list them via Inherit or Set.  Inherit/Set entries take
// precedence over the base vars (Set overrides Inherit which overrides base).
//
// Never pass bzmux's full os.Environ() to any child.
// Secret values from Set are NEVER logged.
func buildChildEnv(senv muxcfg.ServerEnv) []string {
	env := make(map[string]string)

	// Base process hygiene vars (lowest precedence — Inherit/Set override).
	for _, key := range []string{"PATH", "HOME"} {
		if val, ok := os.LookupEnv(key); ok {
			env[key] = val
		}
	}
	if val, ok := os.LookupEnv("TMPDIR"); ok {
		env["TMPDIR"] = val
	}

	// Per-child Inherit/Set allowlist (higher precedence than base).
	for _, name := range senv.Inherit {
		if val, ok := os.LookupEnv(name); ok {
			env[name] = val
		}
	}
	for k, v := range senv.Set {
		env[k] = v
	}

	result := make([]string, 0, len(env))
	for k, v := range env {
		result = append(result, k+"="+v)
	}
	sort.Strings(result) // deterministic order
	return result
}
