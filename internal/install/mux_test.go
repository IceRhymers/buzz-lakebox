package install

import (
	"os"
	"strings"
	"testing"

	"github.com/IceRhymers/buzz-lakebox/internal/muxcfg"
)

// goldenMuxConfig returns the canonical two-server config used for the golden
// test and as the baseline for validation tests. Values are placeholders only —
// no real secrets appear here.
func goldenMuxConfig() muxcfg.Config {
	return muxcfg.Config{
		Servers: []muxcfg.Server{
			{
				Name:    "buzz-dev-mcp",
				Command: "buzz-dev-mcp",
				Env: muxcfg.ServerEnv{
					Inherit: []string{
						"BUZZ_PRIVATE_KEY",
						"BUZZ_AUTH_TAG",
						"BUZZ_RELAY_URL",
						"NOSTR_PRIVATE_KEY",
					},
				},
			},
			{
				Name:    "shellbox-mcp",
				Command: "shellbox-mcp",
				Env: muxcfg.ServerEnv{
					Set: map[string]string{
						"SOME_TOKEN": "PLACEHOLDER",
					},
				},
			},
		},
	}
}

// TestBuildMuxConfigJSON_Golden asserts that BuildMuxConfigJSON produces
// byte-identical output to the committed golden file for the canonical
// two-server config. The golden uses placeholder secrets only.
func TestBuildMuxConfigJSON_Golden(t *testing.T) {
	got, err := BuildMuxConfigJSON(goldenMuxConfig())
	if err != nil {
		t.Fatalf("BuildMuxConfigJSON error: %v", err)
	}

	goldenPath := "testdata/mcp-mux.golden.json"
	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("reading golden file %s: %v", goldenPath, err)
	}

	if string(got) != string(want) {
		t.Fatalf("BuildMuxConfigJSON output does not match golden %s.\nGot:\n%s\nWant:\n%s",
			goldenPath, got, want)
	}
}

// TestBuildMuxConfigJSON_Validation checks that BuildMuxConfigJSON rejects
// clearly-invalid configs with actionable errors.
func TestBuildMuxConfigJSON_Validation(t *testing.T) {
	t.Run("empty servers", func(t *testing.T) {
		_, err := BuildMuxConfigJSON(muxcfg.Config{})
		if err == nil {
			t.Fatal("expected error for empty servers slice")
		}
	})

	t.Run("nil servers", func(t *testing.T) {
		_, err := BuildMuxConfigJSON(muxcfg.Config{Servers: nil})
		if err == nil {
			t.Fatal("expected error for nil servers")
		}
	})

	t.Run("duplicate server names", func(t *testing.T) {
		cfg := muxcfg.Config{
			Servers: []muxcfg.Server{
				{Name: "dup", Command: "cmd-a"},
				{Name: "dup", Command: "cmd-b"},
			},
		}
		_, err := BuildMuxConfigJSON(cfg)
		if err == nil {
			t.Fatal("expected error for duplicate server names")
		}
		if !strings.Contains(err.Error(), "dup") {
			t.Fatalf("error should name the duplicate server, got: %v", err)
		}
	})

	t.Run("empty command", func(t *testing.T) {
		cfg := muxcfg.Config{
			Servers: []muxcfg.Server{
				{Name: "my-server", Command: ""},
			},
		}
		_, err := BuildMuxConfigJSON(cfg)
		if err == nil {
			t.Fatal("expected error for empty command")
		}
		if !strings.Contains(err.Error(), "my-server") {
			t.Fatalf("error should name the offending server, got: %v", err)
		}
	})

	t.Run("empty server name", func(t *testing.T) {
		cfg := muxcfg.Config{
			Servers: []muxcfg.Server{
				{Name: "", Command: "something"},
			},
		}
		_, err := BuildMuxConfigJSON(cfg)
		if err == nil {
			t.Fatal("expected error for empty server name")
		}
	})
}

// TestBuildMuxInstallScript asserts the rendered chmod script contains the
// correct safety settings (umask 077, set -eu), chmods both paths with the
// right permissions, and references the exported path constants.
func TestBuildMuxInstallScript(t *testing.T) {
	script, err := BuildMuxInstallScript(MuxBinPath, MuxConfigPath)
	if err != nil {
		t.Fatalf("BuildMuxInstallScript error: %v", err)
	}

	// Safety header: matches install.go / extrabin.go convention.
	if !strings.HasPrefix(script, "#!/bin/sh\nset -eu\numask 077\n") {
		t.Fatalf("script header must be #!/bin/sh / set -eu / umask 077, got:\n%s", script)
	}

	// Must never enable shell tracing.
	for _, line := range strings.Split(script, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "set -x" || strings.HasPrefix(trimmed, "set -ex") || strings.Contains(trimmed, "set -xe") {
			t.Fatalf("script must never enable shell tracing, found: %q", line)
		}
	}

	// chmod 755 on the binary.
	if !strings.Contains(script, `chmod 755 "`+MuxBinPath+`"`) {
		t.Fatalf("script must chmod 755 the binary at MuxBinPath, got:\n%s", script)
	}

	// chmod 600 on the config (secrets file, least-privilege).
	if !strings.Contains(script, `chmod 600 "`+MuxConfigPath+`"`) {
		t.Fatalf("script must chmod 600 the config at MuxConfigPath, got:\n%s", script)
	}

	// mkdir -p guards against a fresh sandbox.
	if !strings.Contains(script, `mkdir -p "`+MuxBinDir+`"`) {
		t.Fatalf("script must mkdir -p MuxBinDir, got:\n%s", script)
	}
}

// TestBuildMuxInstallScript_RejectsHostilePaths ensures BuildMuxInstallScript
// refuses paths that contain characters outside the trusted allowlist, for the
// same reason BuildVerifyCommand validates its envFile parameter.
func TestBuildMuxInstallScript_RejectsHostilePaths(t *testing.T) {
	hostile := []string{
		`$HOME/x"; rm -rf /; echo "`,
		"$HOME/`touch /tmp/pwned`",
		`$HOME/$(evil)`,
		`$HOME/bin/bzmux path`,
		"",
	}
	for _, path := range hostile {
		if _, err := BuildMuxInstallScript(path, MuxConfigPath); err == nil {
			t.Errorf("BuildMuxInstallScript(%q, ...) should have been rejected", path)
		}
		if _, err := BuildMuxInstallScript(MuxBinPath, path); err == nil {
			t.Errorf("BuildMuxInstallScript(..., %q) should have been rejected", path)
		}
	}
}

// TestMuxPathConstants checks structural invariants of the exported constants
// so deployflow and nest can rely on them without re-deriving path layouts.
func TestMuxPathConstants(t *testing.T) {
	// MuxBinDir must be the same as BinDir (so bzmux is on PATH via the .deb
	// install step's mkdir'd BinDir which launch.sh prepends).
	if MuxBinDir != BinDir {
		t.Errorf("MuxBinDir %q must equal BinDir %q", MuxBinDir, BinDir)
	}

	// MuxBinPath must be bzmux inside MuxBinDir.
	if MuxBinPath != MuxBinDir+"/bzmux" {
		t.Errorf("MuxBinPath %q must be MuxBinDir+/bzmux", MuxBinPath)
	}

	// MuxConfigPath must be mcp-mux.json beside the binary (Decision C1:
	// executable-relative config lookup in bzmux).
	if MuxConfigPath != MuxBinDir+"/mcp-mux.json" {
		t.Errorf("MuxConfigPath %q must be MuxBinDir+/mcp-mux.json", MuxConfigPath)
	}

	// Both paths must be under .buzz-backend so they are within the
	// well-known sandbox home-dir subtree.
	for _, path := range []string{MuxBinDir, MuxBinPath, MuxConfigPath} {
		if !strings.Contains(path, ".buzz-backend") {
			t.Errorf("path %q must be under .buzz-backend", path)
		}
	}
}
