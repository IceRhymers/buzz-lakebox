package nest

import (
	"bytes"
	"os/exec"
	"runtime"
	"strings"
	"testing"

	"github.com/IceRhymers/buzz-lakebox/internal/payload"
)

// runSh executes script under /bin/sh -c and returns stdout, failing the
// test on any error. Used to verify shellQuote's escaping is actually
// safe by round-tripping through a real shell rather than just
// string-matching the escape sequence.
func runSh(t *testing.T, script string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("requires a POSIX shell")
	}
	cmd := exec.Command("/bin/sh", "-c", script)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("sh -c failed: %v (stderr: %s)", err, stderr.String())
	}
	return stdout.String()
}

func TestRenderEnv_GoldenFields(t *testing.T) {
	model := "databricks-claude-opus-4-8"
	provider := "databricks_v2"
	agent := payload.Agent{
		Name:                "Reviewer",
		RelayURL:            "wss://relay.example.com",
		PrivateKeyNsec:      "nsec1abc",
		AuthTag:             "tag-abc",
		AgentCommand:        "buzz-agent",
		AgentArgs:           []string{"--flag", "value"},
		SystemPrompt:        "You are a reviewer.",
		Model:               &model,
		Provider:            &provider,
		TurnTimeoutSeconds:  120,
		IdleTimeoutSeconds:  900,
		MaxTurnDurationSecs: 7200,
		Parallelism:         2,
		RespondTo:           "owner-only",
		RespondToAllowlist:  []string{"npub1a", "npub1b"},
		EnvVars: map[string]string{
			"DATABRICKS_HOST":  "https://example.databricks.com",
			"DATABRICKS_TOKEN": "dapi-marker-secret",
		},
	}

	env := RenderEnv(agent)

	wantLines := []string{
		`export BUZZ_PRIVATE_KEY='nsec1abc'`,
		`export BUZZ_AUTH_TAG='tag-abc'`,
		`export BUZZ_RELAY_URL='wss://relay.example.com'`,
		`export BUZZ_ACP_AGENT_COMMAND='buzz-agent'`,
		`export BUZZ_ACP_AGENT_ARGS='--flag value'`,
		`export BUZZ_ACP_AGENTS='2'`,
		`export BUZZ_ACP_SYSTEM_PROMPT='You are a reviewer.'`,
		`export BUZZ_ACP_MODEL='databricks-claude-opus-4-8'`,
		`export BUZZ_ACP_RESPOND_TO='owner-only'`,
		`export BUZZ_ACP_RESPOND_TO_ALLOWLIST='npub1a,npub1b'`,
		`export BUZZ_ACP_TURN_TIMEOUT_SECONDS='120'`,
		`export BUZZ_ACP_IDLE_TIMEOUT_SECONDS='900'`,
		`export BUZZ_ACP_MAX_TURN_DURATION_SECONDS='7200'`,
		`export NOSTR_PRIVATE_KEY='nsec1abc'`,
		`export BUZZ_AGENT_PROVIDER='databricks_v2'`,
		`export DATABRICKS_MODEL='databricks-claude-opus-4-8'`,
		`export DATABRICKS_HOST='https://example.databricks.com'`,
		`export DATABRICKS_TOKEN='dapi-marker-secret'`,
	}
	for _, want := range wantLines {
		if !strings.Contains(env, want) {
			t.Fatalf("env missing expected line %q; full env:\n%s", want, env)
		}
	}

	// env_vars must be emitted after (and thus win over) the fixed
	// BUZZ_AGENT_PROVIDER/DATABRICKS_MODEL block.
	if strings.Index(env, "DATABRICKS_MODEL") > strings.Index(env, "DATABRICKS_HOST") {
		t.Fatal("env_vars must be rendered after the fixed inference defaults")
	}
}

func TestRenderEnv_ProviderDefaultsWhenEmpty(t *testing.T) {
	agent := payload.Agent{AgentCommand: "buzz-agent"}
	env := RenderEnv(agent)
	if !strings.Contains(env, `export BUZZ_AGENT_PROVIDER='databricks_v2'`) {
		t.Fatalf("expected default provider databricks_v2, got:\n%s", env)
	}
}

func TestRenderEnv_ExplicitProviderWins(t *testing.T) {
	provider := "databricks"
	agent := payload.Agent{AgentCommand: "buzz-agent", Provider: &provider}
	env := RenderEnv(agent)
	if !strings.Contains(env, `export BUZZ_AGENT_PROVIDER='databricks'`) {
		t.Fatalf("expected explicit provider to be used, got:\n%s", env)
	}
}

func TestRenderEnv_QuotingEmbeddedQuotesAndNewlines(t *testing.T) {
	agent := payload.Agent{
		AgentCommand: "buzz-agent",
		SystemPrompt: "Line one.\nSay 'hello' and \"goodbye\".\nLine three.",
	}
	env := RenderEnv(agent)

	// The rendered assignment must be shell-safe: verify by sourcing the
	// *entire* rendered env (since the quoted value itself contains
	// literal embedded newlines, it can't be sliced out line-by-line)
	// through a real shell, rather than string-matching the escape
	// sequence.
	got := evalEnvVar(t, env, "BUZZ_ACP_SYSTEM_PROMPT")
	want := "Line one.\nSay 'hello' and \"goodbye\".\nLine three."
	if got != want {
		t.Fatalf("system prompt did not round-trip through the shell:\ngot:  %q\nwant: %q", got, want)
	}
}

func TestRenderEnv_Deterministic(t *testing.T) {
	agent := payload.Agent{
		AgentCommand: "buzz-agent",
		EnvVars:      map[string]string{"Z_VAR": "z", "A_VAR": "a", "M_VAR": "m"},
	}
	first := RenderEnv(agent)
	for i := 0; i < 5; i++ {
		if got := RenderEnv(agent); got != first {
			t.Fatalf("RenderEnv is not deterministic across calls")
		}
	}
	// env_vars should be sorted (A_VAR before M_VAR before Z_VAR).
	if strings.Index(first, "A_VAR") > strings.Index(first, "M_VAR") || strings.Index(first, "M_VAR") > strings.Index(first, "Z_VAR") {
		t.Fatalf("expected env_vars sorted by key, got:\n%s", first)
	}
}

func TestRenderLaunchScript_GoldenInvariants(t *testing.T) {
	script := RenderLaunchScript()

	if !strings.Contains(script, "set -eu") {
		t.Fatal("launch.sh must set -eu")
	}
	for _, line := range strings.Split(script, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "set -x" || strings.HasPrefix(trimmed, "set -ex") || strings.Contains(trimmed, "set -xe") {
			t.Fatalf("launch.sh must never enable shell tracing (set -x), found: %q", line)
		}
	}
	if !strings.Contains(script, `cat > "$HOME/.databrickscfg"`) {
		t.Fatal("launch.sh must re-assert the PAT stub")
	}
	if !strings.Contains(script, "flock -n 9") {
		t.Fatal("launch.sh must guard against double-launch via flock")
	}
	if !strings.Contains(script, "pgrep -f buzz-acp") {
		t.Fatal("launch.sh must also guard via pgrep")
	}
	if !strings.Contains(script, "setsid nohup") {
		t.Fatal("launch.sh must launch buzz-acp via setsid nohup")
	}
	if !strings.Contains(script, `. "$HOME/.buzz-backend/env"`) {
		t.Fatal("launch.sh must source the env file")
	}
}

func TestRenderLaunchScript_NoSecrets(t *testing.T) {
	// launch.sh is a static template with no per-request substitution, so
	// no secret value can ever appear in it structurally — this test
	// pins that invariant.
	script := RenderLaunchScript()
	for _, marker := range []string{"nsec1", "BUZZ_PRIVATE_KEY=", "DATABRICKS_TOKEN="} {
		if strings.Contains(script, marker) {
			t.Fatalf("launch.sh unexpectedly contains %q", marker)
		}
	}
}

func TestPATStub_CommentOnly(t *testing.T) {
	for _, line := range strings.Split(PATStub, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if !strings.HasPrefix(trimmed, "#") {
			t.Fatalf("PAT stub must be comment-only, found non-comment line: %q", line)
		}
	}
	if strings.Contains(PATStub, "[") {
		t.Fatal("PAT stub must not contain a [profile] section header")
	}
}

// evalEnvVar sources the entire rendered env text through /bin/sh and
// echoes the resulting value of the named variable, to verify
// shellQuote's escaping is actually safe rather than merely
// string-matching it. Sourcing the whole env (rather than slicing out one
// line) is required because a quoted value may itself contain literal
// embedded newlines.
func evalEnvVar(t *testing.T, env, key string) string {
	t.Helper()
	script := env + "\nprintf '%s' \"$" + key + "\"\n"
	return runSh(t, script)
}
