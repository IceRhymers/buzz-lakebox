package nest

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/IceRhymers/buzz-lakebox/internal/payload"
	"github.com/IceRhymers/buzz-lakebox/internal/shellquote"
)

// These tests source ClaudeEnvSnippet through a real POSIX shell, the same
// way sandbox_auth_exec_test.go does for SandboxAuthSnippet and for the
// same reason: a shell guard can be syntactically present and still be
// wrong on a real input. They matter more than usual here because the
// snippet's whole job is a security property — never emit a credential
// without an endpoint to send it to — and that property is invisible to
// string matching.

type claudeResult struct {
	baseURL, authToken string
	exitCode           int
	env                string
}

// sourceClaudeSnippet sources ClaudeEnvSnippet under prelude with the given
// variables pre-exported, and reports what it derived.
//
// preset values are exported BEFORE the snippet, which is exactly the
// ordering RenderEnv produces: the fixed block and agent env_vars are
// written first, and the snippet is appended last precisely so it observes
// whatever they set.
func sourceClaudeSnippet(t *testing.T, preset map[string]string, prelude string, checkLeaks bool) claudeResult {
	t.Helper()
	home := t.TempDir()

	snippetPath := filepath.Join(home, "claude.sh")
	// Written as the ENTIRE file, so every case also proves the snippet is
	// safe as the final content of a sourced file (its trailing bare `:`).
	if err := os.WriteFile(snippetPath, []byte(ClaudeEnvSnippet), 0o600); err != nil {
		t.Fatalf("write snippet: %v", err)
	}

	var script strings.Builder
	script.WriteString(prelude)
	script.WriteString("\n")
	// Sorted for determinism is unnecessary here (independent names), but
	// exporting through the same `export K=V` form RenderEnv emits keeps
	// this faithful to production.
	for k, v := range preset {
		script.WriteString("export " + k + "=" + shellquote.Single(v) + "\n")
	}
	script.WriteString(". " + shellquote.Single(snippetPath) + "\n")
	script.WriteString("echo BUZZ_TEST_SOURCED_OK\n")
	script.WriteString(`echo "BUZZ_TEST_BASE_URL=${ANTHROPIC_BASE_URL:-}"` + "\n")
	script.WriteString(`echo "BUZZ_TEST_AUTH_TOKEN=${ANTHROPIC_AUTH_TOKEN:-}"` + "\n")
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
		exitErr, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("sh failed to run: %v (stderr: %s)", err, stderr.String())
		}
		exitCode = exitErr.ExitCode()
	}

	out := stdout.String()
	if !strings.Contains(out, "BUZZ_TEST_SOURCED_OK") {
		t.Fatalf("sourcing shell did not survive; exit=%d stdout=%q stderr=%q", exitCode, out, stderr.String())
	}

	res := claudeResult{exitCode: exitCode}
	for _, line := range strings.Split(out, "\n") {
		switch {
		case strings.HasPrefix(line, "BUZZ_TEST_BASE_URL="):
			res.baseURL = strings.TrimPrefix(line, "BUZZ_TEST_BASE_URL=")
		case strings.HasPrefix(line, "BUZZ_TEST_AUTH_TOKEN="):
			res.authToken = strings.TrimPrefix(line, "BUZZ_TEST_AUTH_TOKEN=")
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

const (
	testHost  = "https://example.databricks.com"
	testToken = "dapi1234567890abcdef"
	wantURL   = "https://example.databricks.com/ai-gateway/anthropic"
)

// TestClaudeEnv_MissingHostDerivesNeitherVar is the most important test in
// this file. It pins the fail-closed coupling: with a token available but
// NO host, the snippet must derive NEITHER variable.
//
// The failure it prevents is a credential leaving the workspace. Claude
// Code falls back to https://api.anthropic.com when ANTHROPIC_BASE_URL is
// unset, a Lakebox sandbox has open egress to that host (verified live),
// and ANTHROPIC_AUTH_TOKEN is sent as an Authorization: Bearer header
// regardless of destination — so deriving the token here would ship a live
// Databricks PAT to a third party.
//
// Asserting only that ANTHROPIC_BASE_URL is empty would PASS in the leaking
// state; the token assertion is the point.
func TestClaudeEnv_MissingHostDerivesNeitherVar(t *testing.T) {
	requireShAndAwk(t)

	res := sourceClaudeSnippet(t, map[string]string{"DATABRICKS_TOKEN": testToken}, "set -eu", false)

	if res.baseURL != "" {
		t.Fatalf("no DATABRICKS_HOST: ANTHROPIC_BASE_URL should stay unset, got %q", res.baseURL)
	}
	if res.authToken != "" {
		t.Fatalf("no DATABRICKS_HOST: ANTHROPIC_AUTH_TOKEN must NOT be derived (it would be sent to api.anthropic.com), got %q", res.authToken)
	}
	if res.exitCode != 0 {
		t.Fatalf("sourcing must not fail the shell, exit=%d", res.exitCode)
	}
}

// TestClaudeEnv_EnvMode covers inference_auth="env": the credentials arrive
// from agent env_vars, which RenderEnv emits before the snippet.
func TestClaudeEnv_EnvMode(t *testing.T) {
	requireShAndAwk(t)

	res := sourceClaudeSnippet(t, map[string]string{
		"DATABRICKS_HOST":  testHost,
		"DATABRICKS_TOKEN": testToken,
	}, "set -eu", false)

	if res.baseURL != wantURL {
		t.Fatalf("ANTHROPIC_BASE_URL = %q, want %q", res.baseURL, wantURL)
	}
	if res.authToken != testToken {
		t.Fatalf("ANTHROPIC_AUTH_TOKEN = %q, want the DATABRICKS_TOKEN value", res.authToken)
	}
}

// TestClaudeEnv_SandboxModeParity is the both-modes claim made observable.
// It drives the SAME snippet with credentials produced the way
// inference_auth="sandbox" produces them — by sourcing SandboxAuthSnippet
// against a baked ~/.databrickscfg first — and requires byte-identical
// results to env mode. One derivation text, two credential sources.
func TestClaudeEnv_SandboxModeParity(t *testing.T) {
	requireShAndAwk(t)

	home := t.TempDir()
	cfg := "[DEFAULT]\nhost = " + testHost + "\ntoken = " + testToken + "\n"
	if err := os.WriteFile(filepath.Join(home, ".databrickscfg"), []byte(cfg), 0o600); err != nil {
		t.Fatalf("write cfg: %v", err)
	}
	// Exactly RenderEnv's sandbox-mode ordering: SandboxAuthSnippet, then
	// ClaudeEnvSnippet, both as the tail of one sourced file.
	combined := filepath.Join(home, "combined.sh")
	if err := os.WriteFile(combined, []byte(SandboxAuthSnippet+"\n"+ClaudeEnvSnippet), 0o600); err != nil {
		t.Fatalf("write combined: %v", err)
	}

	script := "set -eu\n. " + shellquote.Single(combined) + "\n" +
		`echo "BUZZ_TEST_BASE_URL=${ANTHROPIC_BASE_URL:-}"` + "\n" +
		`echo "BUZZ_TEST_AUTH_TOKEN=${ANTHROPIC_AUTH_TOKEN:-}"` + "\n"

	cmd := exec.Command("sh", "-c", script)
	cmd.Env = []string{"HOME=" + home, "PATH=" + os.Getenv("PATH")}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("sandbox-mode chain failed: %v (stderr: %s)", err, stderr.String())
	}

	out := stdout.String()
	if !strings.Contains(out, "BUZZ_TEST_BASE_URL="+wantURL) {
		t.Fatalf("sandbox mode should derive the same base URL as env mode; got: %q", out)
	}
	if !strings.Contains(out, "BUZZ_TEST_AUTH_TOKEN="+testToken) {
		t.Fatalf("sandbox mode should derive the token from the baked cfg; got: %q", out)
	}
}

// TestClaudeEnv_OwnerSuppliedValuesWin proves the snippet is a fallback,
// never an override — the same contract SandboxAuthSnippet holds.
func TestClaudeEnv_OwnerSuppliedValuesWin(t *testing.T) {
	requireShAndAwk(t)

	res := sourceClaudeSnippet(t, map[string]string{
		"DATABRICKS_HOST":      testHost,
		"DATABRICKS_TOKEN":     testToken,
		"ANTHROPIC_BASE_URL":   "https://byo.example.com",
		"ANTHROPIC_AUTH_TOKEN": "owner-supplied-token",
	}, "set -eu", false)

	if res.baseURL != "https://byo.example.com" {
		t.Fatalf("owner-supplied ANTHROPIC_BASE_URL must win, got %q", res.baseURL)
	}
	if res.authToken != "owner-supplied-token" {
		t.Fatalf("owner-supplied ANTHROPIC_AUTH_TOKEN must win, got %q", res.authToken)
	}
}

// TestClaudeEnv_HostNormalization covers the shapes a host can actually
// arrive in. SandboxAuthSnippet takes the cfg value verbatim (no scheme
// handling), and an owner may equally type a bare hostname or leave a
// trailing slash — all must yield exactly one well-formed URL.
func TestClaudeEnv_HostNormalization(t *testing.T) {
	requireShAndAwk(t)

	for _, tc := range []struct{ host, want string }{
		{"https://h.example.com", "https://h.example.com/ai-gateway/anthropic"},
		{"http://h.example.com", "http://h.example.com/ai-gateway/anthropic"},
		{"h.example.com", "https://h.example.com/ai-gateway/anthropic"},
		{"https://h.example.com/", "https://h.example.com/ai-gateway/anthropic"},
	} {
		res := sourceClaudeSnippet(t, map[string]string{
			"DATABRICKS_HOST":  tc.host,
			"DATABRICKS_TOKEN": testToken,
		}, "set -eu", false)
		if res.baseURL != tc.want {
			t.Errorf("host %q: ANTHROPIC_BASE_URL = %q, want %q", tc.host, res.baseURL, tc.want)
		}
	}
}

// TestClaudeEnv_APIKeyIsInert pins a behavior that was hypothesized to be
// dangerous and proved harmless live: an ANTHROPIC_API_KEY (empty or set)
// alongside the derived bearer token does not prevent derivation, and the
// adapter completes turns normally in both cases. Recorded as a test so the
// discarded "reject both keys" guard is not reintroduced on a hunch.
func TestClaudeEnv_APIKeyIsInert(t *testing.T) {
	requireShAndAwk(t)

	for name, apiKey := range map[string]string{"empty": "", "set": "sk-ant-example"} {
		res := sourceClaudeSnippet(t, map[string]string{
			"DATABRICKS_HOST":   testHost,
			"DATABRICKS_TOKEN":  testToken,
			"ANTHROPIC_API_KEY": apiKey,
		}, "set -eu", false)
		if res.baseURL != wantURL {
			t.Errorf("API_KEY %s: base URL = %q, want %q", name, res.baseURL, wantURL)
		}
		if res.authToken != testToken {
			t.Errorf("API_KEY %s: auth token = %q, want it derived", name, res.authToken)
		}
	}
}

// TestClaudeEnv_NoScratchVarLeakUnderSetA pins the invariant that matters
// in install.BuildVerifyCommand's context, which sources the env file under
// `set -a`: every variable assigned after that is auto-exported, so an
// un-unset scratch variable would leak into the agent's own environment.
func TestClaudeEnv_NoScratchVarLeakUnderSetA(t *testing.T) {
	requireShAndAwk(t)

	res := sourceClaudeSnippet(t, map[string]string{
		"DATABRICKS_HOST":  testHost,
		"DATABRICKS_TOKEN": testToken,
	}, "set -eu; set -a", true)

	if res.env == "" {
		t.Fatal("no environment captured")
	}
	for _, line := range strings.Split(res.env, "\n") {
		if strings.HasPrefix(line, "buzz_") {
			t.Errorf("scratch variable leaked into the environment under set -a: %q", line)
		}
	}
}

// TestClaudeEnv_RenderedEnvSourcesCleanly drives the FULL rendered env file
// (not just the snippet) through a shell in both auth modes, which is what
// launch.sh and the verify handshake actually do.
func TestClaudeEnv_RenderedEnvSourcesCleanly(t *testing.T) {
	requireShAndAwk(t)

	agent := payload.Agent{
		PrivateKeyNsec: "nsec1example",
		AuthTag:        "tag",
		RelayURL:       "wss://relay.example",
		AgentCommand:   "claude-code",
		Parallelism:    1,
		EnvVars:        map[string]string{"DATABRICKS_HOST": testHost, "DATABRICKS_TOKEN": testToken},
	}

	for _, sandboxMode := range []bool{false, true} {
		home := t.TempDir()
		envPath := filepath.Join(home, "env")
		if err := os.WriteFile(envPath, []byte(RenderEnv(agent, payload.RuntimeClaude, sandboxMode)), 0o600); err != nil {
			t.Fatalf("write env: %v", err)
		}
		script := "set -eu\n. " + shellquote.Single(envPath) + "\n" +
			`echo "BUZZ_TEST_BASE_URL=${ANTHROPIC_BASE_URL:-}"` + "\n" +
			`echo "BUZZ_TEST_CMD=${BUZZ_ACP_AGENT_COMMAND:-}"` + "\n"
		cmd := exec.Command("sh", "-c", script)
		cmd.Env = []string{"HOME=" + home, "PATH=" + os.Getenv("PATH")}
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		if err := cmd.Run(); err != nil {
			t.Fatalf("sandboxMode=%v: rendered env failed to source: %v (stderr: %s)", sandboxMode, err, stderr.String())
		}
		out := stdout.String()
		if !strings.Contains(out, "BUZZ_TEST_BASE_URL="+wantURL) {
			t.Errorf("sandboxMode=%v: base URL not derived from the rendered env; got %q", sandboxMode, out)
		}
		// The canonical spawn command, not the "claude-code" alias.
		if !strings.Contains(out, "BUZZ_TEST_CMD=claude-agent-acp") {
			t.Errorf("sandboxMode=%v: agent command should be canonicalized; got %q", sandboxMode, out)
		}
	}
}

// TestClaudeEnv_OwnerBaseURLDoesNotReceiveDerivedToken is a credential-
// routing guard, and the distinction it pins is subtle enough to be worth
// spelling out: the token is derived only when THIS SNIPPET also chose the
// endpoint, not merely when some endpoint happens to be set.
//
// The failure it prevents is specific to inference_auth="sandbox", where
// DATABRICKS_TOKEN is the sandbox's baked creator-identity PAT — an
// owner-level workspace credential the payload never had to carry. If the
// token were gated on "ANTHROPIC_BASE_URL is non-empty", an agent whose
// env_vars named any other endpoint would have that PAT forwarded there by
// this provider, on every turn.
func TestClaudeEnv_OwnerBaseURLDoesNotReceiveDerivedToken(t *testing.T) {
	requireShAndAwk(t)

	res := sourceClaudeSnippet(t, map[string]string{
		"ANTHROPIC_BASE_URL": "https://byo.example.com/v1",
		"DATABRICKS_HOST":    testHost,
		"DATABRICKS_TOKEN":   testToken,
	}, "set -eu", false)

	if res.baseURL != "https://byo.example.com/v1" {
		t.Fatalf("owner-supplied base URL must be preserved, got %q", res.baseURL)
	}
	if res.authToken != "" {
		t.Fatalf("the workspace token must NOT be attached to an endpoint this snippet did not choose, got %q", res.authToken)
	}
}

// TestClaudeEnv_BringYourOwnEndpointAndToken is the supported form of the
// case above: name the endpoint AND the credential, and both survive.
func TestClaudeEnv_BringYourOwnEndpointAndToken(t *testing.T) {
	requireShAndAwk(t)

	res := sourceClaudeSnippet(t, map[string]string{
		"ANTHROPIC_BASE_URL":   "https://byo.example.com/v1",
		"ANTHROPIC_AUTH_TOKEN": "byo-token",
		"DATABRICKS_TOKEN":     testToken,
	}, "set -eu", false)

	if res.baseURL != "https://byo.example.com/v1" || res.authToken != "byo-token" {
		t.Fatalf("bring-your-own endpoint+token must both be preserved, got url=%q token=%q", res.baseURL, res.authToken)
	}
}
