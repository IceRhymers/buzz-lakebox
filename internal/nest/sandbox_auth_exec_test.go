package nest

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/IceRhymers/buzz-lakebox/internal/shellquote"
)

// These tests actually source SandboxAuthSnippet through a real POSIX
// shell against fixture ~/.databrickscfg files, the same way
// nest_test.go's evalEnvVar round-trips RenderEnv's quoting through
// /bin/sh rather than string-matching it: a shell/awk parser can be
// syntactically present and still misbehave on a real input.
//
// Unlike launch_exec_test.go's guard proofs (newLaunchHarness), these
// tests need only `sh` and `awk` — no flock/setsid/pgrep — so they are
// NOT skipped on non-Linux platforms. launch_exec_test.go's Linux-only
// skip exists specifically for flock/setsid (util-linux tools), which is
// not a constraint here (Critic note 4).
func requireShAndAwk(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("requires a POSIX shell")
	}
	for _, tool := range []string{"sh", "awk"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not available", tool)
		}
	}
}

// sourceSnippet writes cfgContent (if non-nil) to $HOME/.databrickscfg
// inside a fresh temp $HOME, then sources SandboxAuthSnippet through a
// real shell under the given prelude (e.g. "set -eu" or "set -eu; set
// -a"), optionally with DATABRICKS_TOKEN preset. It returns the derived
// DATABRICKS_HOST, DATABRICKS_TOKEN, the shell's exit status, and — when
// checkLeaks is true — the full post-source environment (for asserting
// no buzz_-prefixed scratch variable survives a set -a source).
type sourceResult struct {
	host, token string
	exitCode    int
	env         string
}

func sourceSnippet(t *testing.T, cfgContent *string, presetToken string, prelude string, checkLeaks bool, chmodCfg *os.FileMode) sourceResult {
	t.Helper()
	home := t.TempDir()
	if cfgContent != nil {
		cfgPath := filepath.Join(home, ".databrickscfg")
		if err := os.WriteFile(cfgPath, []byte(*cfgContent), 0o600); err != nil {
			t.Fatalf("write fixture cfg: %v", err)
		}
		if chmodCfg != nil {
			if err := os.Chmod(cfgPath, *chmodCfg); err != nil {
				t.Fatalf("chmod fixture cfg: %v", err)
			}
		}
	}

	snippetPath := filepath.Join(home, "snippet.sh")
	// Written as the ENTIRE file content (no trailing text after it) so
	// this doubles as the "snippet is the last content sourced" case when
	// the caller sources exactly this file.
	if err := os.WriteFile(snippetPath, []byte(SandboxAuthSnippet), 0o600); err != nil {
		t.Fatalf("write snippet: %v", err)
	}

	var script strings.Builder
	script.WriteString(prelude)
	script.WriteString("\n")
	if presetToken != "" {
		script.WriteString("export DATABRICKS_TOKEN=" + shellquote.Single(presetToken) + "\n")
	}
	script.WriteString(". " + shellquote.Single(snippetPath) + "\n")
	// Marker echoed AFTER sourcing proves the sourcing shell survived
	// (didn't die under set -eu) — the exit code below is belt-and-suspenders.
	script.WriteString("echo BUZZ_TEST_SOURCED_OK\n")
	script.WriteString(`echo "BUZZ_TEST_HOST=${DATABRICKS_HOST:-}"` + "\n")
	script.WriteString(`echo "BUZZ_TEST_TOKEN=${DATABRICKS_TOKEN:-}"` + "\n")
	if checkLeaks {
		script.WriteString("echo BUZZ_TEST_ENV_BEGIN\n")
		script.WriteString("env\n")
		script.WriteString("echo BUZZ_TEST_ENV_END\n")
	}

	cmd := exec.Command("sh", "-c", script.String())
	cmd.Env = []string{"HOME=" + home, "PATH=" + os.Getenv("PATH")}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()

	exitCode := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			t.Fatalf("sh failed to run: %v (stderr: %s)", err, stderr.String())
		}
	}

	out := stdout.String()
	if !strings.Contains(out, "BUZZ_TEST_SOURCED_OK") {
		t.Fatalf("sourcing shell did not survive (never printed the post-source marker); exit=%d, stdout=%q, stderr=%q", exitCode, out, stderr.String())
	}

	res := sourceResult{exitCode: exitCode}
	for _, line := range strings.Split(out, "\n") {
		switch {
		case strings.HasPrefix(line, "BUZZ_TEST_HOST="):
			res.host = strings.TrimPrefix(line, "BUZZ_TEST_HOST=")
		case strings.HasPrefix(line, "BUZZ_TEST_TOKEN="):
			res.token = strings.TrimPrefix(line, "BUZZ_TEST_TOKEN=")
		}
	}
	if checkLeaks {
		if begin := strings.Index(out, "BUZZ_TEST_ENV_BEGIN\n"); begin >= 0 {
			if end := strings.Index(out, "BUZZ_TEST_ENV_END"); end > begin {
				res.env = out[begin+len("BUZZ_TEST_ENV_BEGIN\n") : end]
			}
		}
	}
	return res
}

const canonicalCfg = "[DEFAULT]\nhost = https://example.databricks.com\ntoken = dapi1234567890abcdef\n"

func TestSandboxAuthSnippet_Canonical(t *testing.T) {
	requireShAndAwk(t)
	cfg := canonicalCfg
	res := sourceSnippet(t, &cfg, "", "set -eu", false, nil)
	if res.exitCode != 0 {
		t.Fatalf("expected exit 0, got %d", res.exitCode)
	}
	if res.host != "https://example.databricks.com" {
		t.Fatalf("host = %q, want https://example.databricks.com", res.host)
	}
	if res.token != "dapi1234567890abcdef" {
		t.Fatalf("token = %q, want dapi1234567890abcdef", res.token)
	}
}

func TestSandboxAuthSnippet_ExtraProfilesBeforeAndAfterDefault(t *testing.T) {
	requireShAndAwk(t)
	cfg := "[other]\nhost = https://wrong.example.com\ntoken = dapiWRONGWRONGWRONG\n" +
		"[DEFAULT]\nhost = https://example.databricks.com\ntoken = dapi1234567890abcdef\n" +
		"[another]\nhost = https://also-wrong.example.com\ntoken = dapiALSOWRONG\n"
	res := sourceSnippet(t, &cfg, "", "set -eu", false, nil)
	if res.exitCode != 0 {
		t.Fatalf("expected exit 0, got %d", res.exitCode)
	}
	if res.host != "https://example.databricks.com" || res.token != "dapi1234567890abcdef" {
		t.Fatalf("got host=%q token=%q, want the [DEFAULT] section's values only (profiles before/after must not leak in)", res.host, res.token)
	}
}

func TestSandboxAuthSnippet_DefaultSectionNotFirst(t *testing.T) {
	requireShAndAwk(t)
	cfg := "[zzz]\nfoo=bar\n[DEFAULT]\nhost=https://not-first.databricks.com\ntoken=dapiNOTFIRST\n"
	res := sourceSnippet(t, &cfg, "", "set -eu", false, nil)
	if res.exitCode != 0 {
		t.Fatalf("expected exit 0, got %d", res.exitCode)
	}
	if res.host != "https://not-first.databricks.com" || res.token != "dapiNOTFIRST" {
		t.Fatalf("got host=%q token=%q, want derivation to still work when [DEFAULT] isn't the first section", res.host, res.token)
	}
}

func TestSandboxAuthSnippet_HostWithoutScheme(t *testing.T) {
	requireShAndAwk(t)
	cfg := "[DEFAULT]\nhost = noscheme.databricks.com\ntoken = dapiabc\n"
	res := sourceSnippet(t, &cfg, "", "set -eu", false, nil)
	if res.exitCode != 0 {
		t.Fatalf("expected exit 0, got %d", res.exitCode)
	}
	if res.host != "noscheme.databricks.com" {
		t.Fatalf("host = %q, want the scheme-less value taken verbatim", res.host)
	}
}

func TestSandboxAuthSnippet_CommentsAndBlankLines(t *testing.T) {
	requireShAndAwk(t)
	cfg := "[DEFAULT]\n# a comment\n; another comment\n\nhost = https://commented.databricks.com\ntoken = dapicommented\n"
	res := sourceSnippet(t, &cfg, "", "set -eu", false, nil)
	if res.exitCode != 0 {
		t.Fatalf("expected exit 0, got %d", res.exitCode)
	}
	if res.host != "https://commented.databricks.com" || res.token != "dapicommented" {
		t.Fatalf("got host=%q token=%q, want comments/blank lines to be tolerated", res.host, res.token)
	}
}

func TestSandboxAuthSnippet_MissingFile(t *testing.T) {
	requireShAndAwk(t)
	res := sourceSnippet(t, nil, "", "set -eu", false, nil)
	if res.exitCode != 0 {
		t.Fatalf("missing cfg file must not fail the sourcing shell under set -eu, got exit %d", res.exitCode)
	}
	if res.host != "" || res.token != "" {
		t.Fatalf("got host=%q token=%q, want both unset when the cfg file is missing", res.host, res.token)
	}
}

func TestSandboxAuthSnippet_TokenLineMissing(t *testing.T) {
	requireShAndAwk(t)
	cfg := "[DEFAULT]\nhost = https://notoken.databricks.com\n"
	res := sourceSnippet(t, &cfg, "", "set -eu", false, nil)
	if res.exitCode != 0 {
		t.Fatalf("expected exit 0, got %d", res.exitCode)
	}
	if res.host != "https://notoken.databricks.com" {
		t.Fatalf("host = %q, want it still derived", res.host)
	}
	if res.token != "" {
		t.Fatalf("token = %q, want empty when the cfg has no token line", res.token)
	}
}

func TestSandboxAuthSnippet_PresetTokenWins_DerivationNoOp(t *testing.T) {
	requireShAndAwk(t)
	cfg := canonicalCfg
	res := sourceSnippet(t, &cfg, "preset-token-value", "set -eu", false, nil)
	if res.exitCode != 0 {
		t.Fatalf("expected exit 0, got %d", res.exitCode)
	}
	if res.token != "preset-token-value" {
		t.Fatalf("token = %q, want the preset DATABRICKS_TOKEN to win (derivation must be a no-op)", res.token)
	}
	if res.host != "" {
		t.Fatalf("host = %q, want empty: the outer gate is DATABRICKS_TOKEN being unset, so a preset token must skip the WHOLE block, not just the token export", res.host)
	}
}

func TestSandboxAuthSnippet_UnreadableCfg_Chmod000(t *testing.T) {
	requireShAndAwk(t)
	if os.Geteuid() == 0 {
		t.Skip("running as root; chmod 000 does not deny root read access")
	}
	cfg := canonicalCfg
	mode := os.FileMode(0o000)
	res := sourceSnippet(t, &cfg, "", "set -eu", false, &mode)
	if res.exitCode != 0 {
		t.Fatalf("an unreadable cfg must not fail the sourcing shell under set -eu, got exit %d", res.exitCode)
	}
	if res.host != "" || res.token != "" {
		t.Fatalf("got host=%q token=%q, want both unset when the cfg file is unreadable", res.host, res.token)
	}
}

// TestSandboxAuthSnippet_AsLastContentOfSourcedFile is R3(b)'s direct
// proof: `.`-sourcing a file whose LAST line is this snippet must still
// return exit status 0, even under set -eu, regardless of which branch
// the snippet's own if/fi took.
func TestSandboxAuthSnippet_AsLastContentOfSourcedFile(t *testing.T) {
	requireShAndAwk(t)
	cfg := canonicalCfg
	res := sourceSnippet(t, &cfg, "", "set -eu", false, nil)
	if res.exitCode != 0 {
		t.Fatalf("sourcing a file ending in SandboxAuthSnippet must exit 0, got %d", res.exitCode)
	}
}

// TestSandboxAuthSnippet_NoScratchVarLeakUnderSetA is R3(c)'s direct
// proof: install.BuildVerifyCommand sources the env content under `set
// -a`, which auto-exports every variable assigned afterward — the
// snippet's scratch variables must not survive that.
func TestSandboxAuthSnippet_NoScratchVarLeakUnderSetA(t *testing.T) {
	requireShAndAwk(t)
	cfg := canonicalCfg
	res := sourceSnippet(t, &cfg, "", "set -eu; set -a", true, nil)
	if res.exitCode != 0 {
		t.Fatalf("expected exit 0, got %d", res.exitCode)
	}
	if res.host != "https://example.databricks.com" || res.token != "dapi1234567890abcdef" {
		t.Fatalf("got host=%q token=%q under set -a, want derivation to still succeed", res.host, res.token)
	}
	for _, line := range strings.Split(res.env, "\n") {
		if strings.HasPrefix(line, "buzz_") {
			t.Fatalf("scratch variable leaked into the environment under set -a: %q (full env dump:\n%s)", line, res.env)
		}
	}
}
