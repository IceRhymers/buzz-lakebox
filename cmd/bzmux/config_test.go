package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/IceRhymers/buzz-lakebox/internal/muxcfg"
)

// ---------------------------------------------------------------------------
// buildChildEnv — least-privilege env allowlist
// ---------------------------------------------------------------------------

func TestBuildChildEnv_InheritForwardsListed(t *testing.T) {
	t.Setenv("TEST_SECRET_A", "val_a")
	t.Setenv("TEST_SECRET_B", "val_b")

	senv := muxcfg.ServerEnv{
		Inherit: []string{"TEST_SECRET_A"},
	}
	env := buildChildEnv(senv)

	if !containsEnv(env, "TEST_SECRET_A=val_a") {
		t.Errorf("TEST_SECRET_A should be in child env, got: %v", env)
	}
	if containsEnvKey(env, "TEST_SECRET_B") {
		t.Errorf("TEST_SECRET_B not in allowlist but reached child env: %v", env)
	}
}

func TestBuildChildEnv_SetAppearsInEnv(t *testing.T) {
	senv := muxcfg.ServerEnv{
		Set: map[string]string{"EXPLICIT_VAR": "explicit_val"},
	}
	env := buildChildEnv(senv)
	if !containsEnv(env, "EXPLICIT_VAR=explicit_val") {
		t.Errorf("EXPLICIT_VAR should appear in child env; got: %v", env)
	}
}

func TestBuildChildEnv_SetOverridesInherit(t *testing.T) {
	t.Setenv("OVERRIDE_VAR", "from_inherit")

	senv := muxcfg.ServerEnv{
		Inherit: []string{"OVERRIDE_VAR"},
		Set:     map[string]string{"OVERRIDE_VAR": "from_set"},
	}
	env := buildChildEnv(senv)

	if !containsEnv(env, "OVERRIDE_VAR=from_set") {
		t.Errorf("Set should override Inherit for same key; got %v", env)
	}
}

func TestBuildChildEnv_LeastPrivilege_NoBlankSlate(t *testing.T) {
	// An empty allowlist must not pass os.Environ() to the child.  Only the
	// non-secret process hygiene base vars (PATH, HOME, TMPDIR) are forwarded
	// unconditionally; arbitrary secrets must never leak through an empty
	// allowlist.
	t.Setenv("BZMUX_TEST_UNRELATED", "should_not_leak")

	senv := muxcfg.ServerEnv{} // no Inherit, no Set
	env := buildChildEnv(senv)

	// Security property: arbitrary secrets must not reach children.
	if containsEnvKey(env, "BZMUX_TEST_UNRELATED") {
		t.Errorf("empty allowlist leaked BZMUX_TEST_UNRELATED into child env")
	}
	// Only the documented process-hygiene base vars are permitted.
	for _, entry := range env {
		key := strings.SplitN(entry, "=", 2)[0]
		switch key {
		case "PATH", "HOME", "TMPDIR":
			// expected non-secret base vars
		default:
			t.Errorf("empty allowlist forwarded unexpected var %q", key)
		}
	}
}

func TestBuildChildEnv_MissingInheritVarIsSkipped(t *testing.T) {
	os.Unsetenv("DEFINITELY_NOT_SET_BZMUX_TEST") //nolint:errcheck
	senv := muxcfg.ServerEnv{
		Inherit: []string{"DEFINITELY_NOT_SET_BZMUX_TEST"},
	}
	env := buildChildEnv(senv)
	if containsEnvKey(env, "DEFINITELY_NOT_SET_BZMUX_TEST") {
		t.Error("unset inherit var should be silently skipped")
	}
}

// ---------------------------------------------------------------------------
// loadConfig
// ---------------------------------------------------------------------------

func writeConfig(t *testing.T, cfg interface{}) string {
	t.Helper()
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "mcp-mux.json")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func TestLoadConfig_ValidConfig(t *testing.T) {
	path := writeConfig(t, map[string]interface{}{
		"servers": []interface{}{
			map[string]interface{}{"name": "srv-a", "command": "/usr/bin/echo"},
			map[string]interface{}{"name": "srv-b", "command": "/usr/bin/true"},
		},
	})
	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if len(cfg.Servers) != 2 {
		t.Errorf("servers: want 2, got %d", len(cfg.Servers))
	}
}

func TestLoadConfig_EmptyServersIsError(t *testing.T) {
	path := writeConfig(t, map[string]interface{}{"servers": []interface{}{}})
	_, err := loadConfig(path)
	if err == nil {
		t.Fatal("expected error for empty servers, got nil")
	}
}

func TestLoadConfig_MissingNameIsError(t *testing.T) {
	path := writeConfig(t, map[string]interface{}{
		"servers": []interface{}{
			map[string]interface{}{"command": "/bin/sh"},
		},
	})
	_, err := loadConfig(path)
	if err == nil || !strings.Contains(err.Error(), "name") {
		t.Fatalf("expected missing-name error, got %v", err)
	}
}

func TestLoadConfig_MissingCommandIsError(t *testing.T) {
	path := writeConfig(t, map[string]interface{}{
		"servers": []interface{}{
			map[string]interface{}{"name": "srv"},
		},
	})
	_, err := loadConfig(path)
	if err == nil || !strings.Contains(err.Error(), "command") {
		t.Fatalf("expected missing-command error, got %v", err)
	}
}

func TestLoadConfig_DuplicateServerNameIsError(t *testing.T) {
	path := writeConfig(t, map[string]interface{}{
		"servers": []interface{}{
			map[string]interface{}{"name": "dup", "command": "/bin/sh"},
			map[string]interface{}{"name": "dup", "command": "/bin/sh"},
		},
	})
	_, err := loadConfig(path)
	if err == nil || !strings.Contains(err.Error(), "dup") {
		t.Fatalf("expected duplicate-name error, got %v", err)
	}
}

func TestLoadConfig_MissingFileIsError(t *testing.T) {
	_, err := loadConfig("/nonexistent/path/mcp-mux.json")
	if err == nil {
		t.Fatal("expected error for missing file, got nil")
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func containsEnv(env []string, entry string) bool {
	for _, e := range env {
		if e == entry {
			return true
		}
	}
	return false
}

func containsEnvKey(env []string, key string) bool {
	prefix := key + "="
	for _, e := range env {
		if strings.HasPrefix(e, prefix) {
			return true
		}
	}
	return false
}
