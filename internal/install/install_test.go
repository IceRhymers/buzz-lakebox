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

func TestBuildVerifyCommand_CombinedScript(t *testing.T) {
	cmd, err := BuildVerifyCommand(`$HOME/.buzz-backend/.env.verify`, 10)
	if err != nil {
		t.Fatalf("BuildVerifyCommand error for the canonical trusted path: %v", err)
	}
	if !strings.Contains(cmd, InitializeFrame) {
		t.Fatal("verify command should pipe the ACP initialize frame")
	}
	if !strings.Contains(cmd, "timeout 10") {
		t.Fatal("verify command should enforce a 10s timeout")
	}
	// envFile is a trusted "$HOME"-relative literal: it must be assigned
	// via a double-quoted literal (so "$HOME" expands), not
	// shellquote.Single (which would suppress that expansion and break
	// sourcing — the exact BUG 1 regression this pins).
	if !strings.Contains(cmd, `ENVF="$HOME/.buzz-backend/.env.verify"`) {
		t.Fatalf("verify command should assign the trusted env file path with $HOME expansion preserved, got: %q", cmd)
	}
	if !strings.Contains(cmd, `. "$ENVF"`) {
		t.Fatalf("verify command should source the env file via the expanded $ENVF variable, got: %q", cmd)
	}
	if !strings.Contains(cmd, `cat > "$ENVF"`) {
		t.Fatal("verify command should write its own stdin into the env file (single combined round trip)")
	}
	if !strings.Contains(cmd, `trap 'rm -f "$ENVF"' EXIT`) {
		t.Fatal("verify command should clean up the real (expanded-path) env file on exit via trap")
	}
	if !strings.Contains(cmd, "buzz-agent") {
		t.Fatal("verify command should invoke buzz-agent")
	}
}

// TestBuildVerifyCommand_RejectsHostilePaths pins the round-2 defensive
// guard: envFile is interpolated unescaped inside a double-quoted shell
// assignment (required so "$HOME" expands), so any character that could
// carry shell syntax through that context — a closing double quote, a
// backtick, $( ) command substitution, backslash, whitespace — must be
// rejected outright. BuildVerifyCommand accepts trusted static literals
// only.
func TestBuildVerifyCommand_RejectsHostilePaths(t *testing.T) {
	hostile := []string{
		`$HOME/x"; rm -rf /; echo "`,         // embedded double quote breaks out of the assignment
		"$HOME/`touch /tmp/pwned`",           // backtick command substitution
		`$HOME/$(touch /tmp/pwned)`,          // $() command substitution
		`$HOME/.buzz-backend/.env verify`,    // whitespace
		`$HOME\.buzz-backend\.env.verify`,    // backslash
		`$HOME/.buzz-backend/.env.verify'x`,  // single quote
		"$HOME/.buzz-backend/.env\nrm -rf /", // newline
		"",                                   // empty
	}
	for _, path := range hostile {
		if cmd, err := BuildVerifyCommand(path, 10); err == nil {
			t.Fatalf("BuildVerifyCommand(%q) should have been rejected, got script: %q", path, cmd)
		}
	}
}
