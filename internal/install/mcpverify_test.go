package install

import (
	"strings"
	"testing"
)

// TestBuildMcpVerifyCommand_RejectsBadPaths pins the charset validation on all
// three interpolated inputs: a bad envFile, muxBinPath, or slotCommand must be
// rejected rather than smuggling shell syntax through.
func TestBuildMcpVerifyCommand_RejectsBadPaths(t *testing.T) {
	good := "$HOME/.buzz-backend/.env.verify"
	goodBin := MuxBinPath

	cases := []struct {
		name        string
		envFile     string
		muxBinPath  string
		mode        string
		slotCommand string
	}{
		{"bad envFile", "$HOME/x;rm -rf /", goodBin, McpVerifyModeMux, ""},
		{"envFile with backtick", "$HOME/`whoami`", goodBin, McpVerifyModeMux, ""},
		{"bad muxBinPath", good, "$HOME/bin/bzmux; echo pwned", McpVerifyModeMux, ""},
		{"empty muxBinPath", good, "", McpVerifyModeMux, ""},
		{"bad slotCommand", good, goodBin, McpVerifyModeDirect, "shellbox-mcp; rm -rf /"},
		{"slotCommand with quote", good, goodBin, McpVerifyModeDirect, `foo"bar`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := BuildMcpVerifyCommand(tc.envFile, 15, tc.muxBinPath, tc.mode, tc.slotCommand); err == nil {
				t.Fatalf("expected rejection for %s, got nil error", tc.name)
			}
		})
	}
}

// TestBuildMcpVerifyCommand_ModeValidation pins the direct/mux argument rules.
func TestBuildMcpVerifyCommand_ModeValidation(t *testing.T) {
	good := "$HOME/.buzz-backend/.env.verify"
	// direct mode requires a slot command.
	if _, err := BuildMcpVerifyCommand(good, 15, MuxBinPath, McpVerifyModeDirect, ""); err == nil {
		t.Fatal("direct mode with empty slot command must be rejected")
	}
	// mux mode must not carry a slot command.
	if _, err := BuildMcpVerifyCommand(good, 15, MuxBinPath, McpVerifyModeMux, "shellbox-mcp"); err == nil {
		t.Fatal("mux mode with a slot command must be rejected")
	}
	// unknown mode.
	if _, err := BuildMcpVerifyCommand(good, 15, MuxBinPath, "bogus", ""); err == nil {
		t.Fatal("unknown mode must be rejected")
	}
}

// TestBuildMcpVerifyCommand_DirectRendering asserts direct mode renders
// --command and --timeout with the absolute bzmux path.
func TestBuildMcpVerifyCommand_DirectRendering(t *testing.T) {
	got, err := BuildMcpVerifyCommand("$HOME/.buzz-backend/.env.verify", 15, MuxBinPath, McpVerifyModeDirect, "shellbox-mcp")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, want := range []string{
		`"` + MuxBinPath + `" --verify-mcp`,
		`--command "shellbox-mcp"`,
		"--timeout 15",
		"timeout 15",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("direct rendering missing %q:\n%s", want, got)
		}
	}
}

// TestBuildMcpVerifyCommand_MuxRendering asserts mux mode renders no --command
// but does pass --timeout.
func TestBuildMcpVerifyCommand_MuxRendering(t *testing.T) {
	got, err := BuildMcpVerifyCommand("$HOME/.buzz-backend/.env.verify", 20, MuxBinPath, McpVerifyModeMux, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(got, "--command") {
		t.Errorf("mux rendering must not contain --command:\n%s", got)
	}
	for _, want := range []string{"--verify-mcp", "--timeout 20", "timeout 20"} {
		if !strings.Contains(got, want) {
			t.Errorf("mux rendering missing %q:\n%s", want, got)
		}
	}
}

// TestBuildMcpVerifyCommand_Golden pins the exact rendered script shape: env
// over stdin only (no secret interpolation), trap-rm, chmod 600, set -a source,
// launch.sh PATH export, tail -c 4096, and the final exit $rc.
func TestBuildMcpVerifyCommand_Golden(t *testing.T) {
	got, err := BuildMcpVerifyCommand("$HOME/.buzz-backend/.env.verify", 15, MuxBinPath, McpVerifyModeDirect, "shellbox-mcp")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := `set -eu
umask 077
ENVF="$HOME/.buzz-backend/.env.verify"
OUTF="$ENVF.out"
trap 'rm -f "$ENVF" "$OUTF"' EXIT
cat > "$ENVF"
chmod 600 "$ENVF"
set -a
# shellcheck disable=SC1090
. "$ENVF"
set +a
export PATH="` + MuxBinDir + `:$PATH"
rc=0
timeout 15 "` + MuxBinPath + `" --verify-mcp --command "shellbox-mcp" --timeout 15 > "$OUTF" 2>&1 || rc=$?
tail -c 4096 "$OUTF"
exit $rc
`
	if got != want {
		t.Errorf("rendered script mismatch:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}
