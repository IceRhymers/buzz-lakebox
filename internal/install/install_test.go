package install

import (
	"strings"
	"testing"
)

func TestBuildInstallScript_DefaultVersion(t *testing.T) {
	script, err := BuildInstallScript("")
	if err != nil {
		t.Fatalf("BuildInstallScript(\"\") error: %v", err)
	}
	if !strings.Contains(script, DefaultVersion) {
		t.Fatalf("script should reference the default pinned version %q", DefaultVersion)
	}
	if !strings.Contains(script, pinnedSHA256[DefaultVersion]) {
		t.Fatal("script should embed the pinned sha256")
	}
}

func TestBuildInstallScript_UnknownVersionFailsLoud(t *testing.T) {
	_, err := BuildInstallScript("v99.99.99")
	if err == nil {
		t.Fatal("expected an error for an unpinned version")
	}
	if !strings.Contains(err.Error(), "v99.99.99") {
		t.Fatalf("error should name the requested version, got: %v", err)
	}
}

func TestBuildInstallScript_NoSetX(t *testing.T) {
	script, err := BuildInstallScript(DefaultVersion)
	if err != nil {
		t.Fatalf("BuildInstallScript error: %v", err)
	}
	for _, line := range strings.Split(script, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "set -x" || strings.HasPrefix(trimmed, "set -ex") || strings.Contains(trimmed, "set -xe") {
			t.Fatalf("script must never enable shell tracing, found line: %q", line)
		}
	}
	if !strings.Contains(script, "set -eu") {
		t.Fatal("script should set -eu")
	}
	if !strings.Contains(script, "umask 077") {
		t.Fatal("script should set umask 077")
	}
}

func TestBuildInstallScript_SkipsWhenMarkerMatches(t *testing.T) {
	script, err := BuildInstallScript(DefaultVersion)
	if err != nil {
		t.Fatalf("BuildInstallScript error: %v", err)
	}
	if !strings.Contains(script, "already installed; skipping download") {
		t.Fatal("script should contain marker-match skip logic")
	}
	if !strings.Contains(script, `[ "$(cat "$MARKER")" = "$BUZZ_VERSION" ]`) {
		t.Fatal("script should compare the marker file content against the pinned version")
	}
}

func TestBuildInstallScript_SymlinksAllBinaries(t *testing.T) {
	script, err := BuildInstallScript(DefaultVersion)
	if err != nil {
		t.Fatalf("BuildInstallScript error: %v", err)
	}
	for _, bin := range BinNames {
		if !strings.Contains(script, `"$BIN_DIR/`+bin+`"`) {
			t.Fatalf("script should symlink %q into BIN_DIR", bin)
		}
	}
}

func TestBuildInstallScript_ChecksumVerification(t *testing.T) {
	script, err := BuildInstallScript(DefaultVersion)
	if err != nil {
		t.Fatalf("BuildInstallScript error: %v", err)
	}
	if !strings.Contains(script, "sha256sum -c -") {
		t.Fatal("script should verify sha256 via sha256sum -c")
	}
	if !strings.Contains(script, "curl -fL") {
		t.Fatal("script should fetch via curl -fL")
	}
	if !strings.Contains(script, "dpkg-deb -x") {
		t.Fatal("script should extract via dpkg-deb -x")
	}
}

func TestBuildVerifyCommand_NoSecretsSourcesEnvFile(t *testing.T) {
	cmd := BuildVerifyCommand("$HOME/.buzz-backend/.env.verify", 10)
	if !strings.Contains(cmd, InitializeFrame) {
		t.Fatal("verify command should pipe the ACP initialize frame")
	}
	if !strings.Contains(cmd, "timeout 10") {
		t.Fatal("verify command should enforce a 10s timeout")
	}
	if !strings.Contains(cmd, `. '$HOME/.buzz-backend/.env.verify'`) {
		t.Fatalf("verify command should source the env file, got: %q", cmd)
	}
	if !strings.Contains(cmd, "trap 'rm -f") {
		t.Fatal("verify command should clean up the temp env file on exit")
	}
	if !strings.Contains(cmd, "buzz-agent") {
		t.Fatal("verify command should invoke buzz-agent")
	}
}

func TestBuildVerifyCommand_ShellQuotesEnvFilePath(t *testing.T) {
	// Even a path with a single quote (pathological, but exercises the
	// escaping) must round-trip safely.
	cmd := BuildVerifyCommand("/tmp/weird'path", 5)
	if !strings.Contains(cmd, `/tmp/weird'\''path`) {
		t.Fatalf("expected escaped single quote in path, got: %q", cmd)
	}
}
