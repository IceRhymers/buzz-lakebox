package nest

import (
	"strings"
	"testing"

	"github.com/IceRhymers/buzz-lakebox/internal/payload"
)

func claudeTestAgent() payload.Agent {
	model := "databricks-claude-opus-4-8"
	provider := "databricks_v2"
	return payload.Agent{
		Name:           "Reviewer",
		RelayURL:       "wss://relay.example.com",
		PrivateKeyNsec: "nsec1abc",
		AuthTag:        "tag-abc",
		AgentCommand:   "claude-code",
		SystemPrompt:   "You are a reviewer.",
		Model:          &model,
		Provider:       &provider,
		Parallelism:    2,
		RespondTo:      "owner-only",
		EnvVars: map[string]string{
			"DATABRICKS_HOST":  "https://example.databricks.com",
			"DATABRICKS_TOKEN": "dapi-marker-secret",
		},
	}
}

// TestRenderEnv_ClaudeGolden pins what the Claude runtime's env file
// contains — and, just as importantly, what it must NOT contain. The
// negative assertions are the substance here: every omitted variable is a
// deliberate decision with a live-probe or upstream-parity reason behind
// it, and each would fail in a different, quiet way if it crept back in.
func TestRenderEnv_ClaudeGolden(t *testing.T) {
	env := RenderEnv(claudeTestAgent(), payload.RuntimeClaude, false)

	wantLines := []string{
		`export BUZZ_PRIVATE_KEY='nsec1abc'`,
		`export BUZZ_RELAY_URL='wss://relay.example.com'`,
		// The canonical spawn command, NOT the "claude-code" alias the
		// payload carried.
		`export BUZZ_ACP_AGENT_COMMAND='claude-agent-acp'`,
		`export BUZZ_ACP_AGENTS='2'`,
		`export BUZZ_ACP_RELAY_OBSERVER='true'`,
		`export BUZZ_ACP_PERMISSION_MODE='bypass-permissions'`,
		// Credentials still travel: they are the snippet's inputs.
		`export DATABRICKS_HOST='https://example.databricks.com'`,
		`export DATABRICKS_TOKEN='dapi-marker-secret'`,
	}
	for _, line := range wantLines {
		if !strings.Contains(env, line) {
			t.Errorf("claude env missing line %q\n--- got ---\n%s", line, env)
		}
	}

	dontWantPrefixes := map[string]string{
		// buzz-agent's tool wiring. Claude Code ships its own tools, and
		// the desktop's claude runtime sets mcp_command None.
		"export BUZZ_ACP_MCP_COMMAND=": "claude has built-in tools and needs no buzz-dev-mcp bridge",
		"export MCP_HOOK_SERVERS=":     "read by buzz-agent itself, meaningless to the adapter",
		// buzz-agent's inference wiring.
		"export BUZZ_AGENT_PROVIDER=": "buzz-agent-specific provider selection",
		"export DATABRICKS_MODEL=":    "buzz-agent-specific model selection",
		// Model selection: a gateway model id is never in the adapter's
		// catalog, so buzz-acp would emit an unsupported_model observer
		// frame every session.
		"export BUZZ_ACP_MODEL=": "the adapter does not support ACP model switching",
		// Live-verified: passing a real gateway id here makes the SDK
		// rewrite it into a canonical Anthropic id the gateway does not
		// serve, hard-failing every turn with model_not_found.
		"export ANTHROPIC_MODEL=": "a gateway model id is rewritten by the SDK and then rejected",
	}
	for prefix, why := range dontWantPrefixes {
		if strings.Contains(env, prefix) {
			t.Errorf("claude env must not contain %q (%s)\n--- got ---\n%s", prefix, why, env)
		}
	}

	if !strings.Contains(env, ClaudeEnvSnippet) {
		t.Error("claude env must append ClaudeEnvSnippet")
	}
}

// buzzAgentBaselineEnv is the EXACT output the renderer produced for
// claudeTestAgent-with-AgentCommand-buzz-agent before the Claude runtime
// existed, captured verbatim. Comparing against it byte-for-byte is the
// point: substring assertions cannot detect a reordering, an inserted line,
// or a changed value elsewhere in the file, and those are precisely the
// regressions a runtime-conditional renderer can introduce.
//
// If a deliberate change to the buzz-agent rendering lands, update this
// constant in the same commit — the diff to it IS the review artifact.
const buzzAgentBaselineEnv = `export BUZZ_PRIVATE_KEY='nsec1abc'
export BUZZ_AUTH_TAG='tag-abc'
export BUZZ_RELAY_URL='wss://relay.example.com'
export BUZZ_ACP_AGENT_COMMAND='buzz-agent'
export BUZZ_ACP_AGENT_ARGS=''
export BUZZ_ACP_AGENTS='2'
export BUZZ_ACP_SYSTEM_PROMPT='You are a reviewer.'
export BUZZ_ACP_MODEL='databricks-claude-opus-4-8'
export BUZZ_ACP_RESPOND_TO='owner-only'
export NOSTR_PRIVATE_KEY='nsec1abc'
export BUZZ_ACP_MCP_COMMAND='buzz-dev-mcp'
export MCP_HOOK_SERVERS='*'
export BUZZ_ACP_RELAY_OBSERVER='true'
export BUZZ_ACP_DEDUP='queue'
export BUZZ_ACP_MULTIPLE_EVENT_HANDLING='steer'
export BUZZ_AGENT_PROVIDER='databricks_v2'
export DATABRICKS_MODEL='databricks-claude-opus-4-8'
export DATABRICKS_HOST='https://example.databricks.com'
export DATABRICKS_TOKEN='dapi-marker-secret'
`

// TestRenderEnv_BuzzAgentByteIdenticalToBaseline is the single most
// important regression guard in this change: adding a runtime must not
// perturb what buzz-agent deploys get, in either auth mode.
func TestRenderEnv_BuzzAgentByteIdenticalToBaseline(t *testing.T) {
	agent := claudeTestAgent()
	agent.AgentCommand = "buzz-agent"

	// env mode: byte-for-byte identical to the pre-Claude rendering.
	if got := RenderEnv(agent, payload.RuntimeBuzzAgent, false); got != buzzAgentBaselineEnv {
		t.Errorf("buzz-agent env-mode rendering changed.\n--- got ---\n%s\n--- want ---\n%s", got, buzzAgentBaselineEnv)
	}

	// sandbox mode: the same bytes plus exactly the zero-token snippet,
	// which is what the pre-Claude renderer produced too.
	wantSandbox := buzzAgentBaselineEnv + "\n" + SandboxAuthSnippet
	if got := RenderEnv(agent, payload.RuntimeBuzzAgent, true); got != wantSandbox {
		t.Errorf("buzz-agent sandbox-mode rendering changed.\n--- got ---\n%s\n--- want ---\n%s", got, wantSandbox)
	}
}

// TestRenderEnv_ClaudeSnippetOrderedLast pins the ordering the whole
// both-modes design rests on: ClaudeEnvSnippet must come after the env_vars
// block AND after SandboxAuthSnippet, because those are the two places its
// inputs can come from. If it ever moved earlier it would silently derive
// nothing in sandbox mode.
func TestRenderEnv_ClaudeSnippetOrderedLast(t *testing.T) {
	env := RenderEnv(claudeTestAgent(), payload.RuntimeClaude, true)

	envVarIdx := strings.LastIndex(env, `export DATABRICKS_TOKEN='dapi-marker-secret'`)
	sandboxIdx := strings.Index(env, SandboxAuthSnippet)
	claudeIdx := strings.Index(env, ClaudeEnvSnippet)

	if envVarIdx < 0 || sandboxIdx < 0 || claudeIdx < 0 {
		t.Fatalf("expected all three sections present (envVars=%d sandbox=%d claude=%d)", envVarIdx, sandboxIdx, claudeIdx)
	}
	if envVarIdx >= sandboxIdx || sandboxIdx >= claudeIdx {
		t.Fatalf("ordering must be env_vars < SandboxAuthSnippet < ClaudeEnvSnippet, got %d < %d < %d", envVarIdx, sandboxIdx, claudeIdx)
	}
}

// TestRenderEnv_ClaudeAliasesCanonicalized proves every accepted alias
// produces the same spawn command — the name internal/install symlinks and
// buzz-acp spawns.
func TestRenderEnv_ClaudeAliasesCanonicalized(t *testing.T) {
	for _, alias := range []string{"claude-agent-acp", "claude-code-acp", "claude-code", "claudecode"} {
		agent := claudeTestAgent()
		agent.AgentCommand = alias
		rt, ok := payload.RuntimeFor(alias)
		if !ok {
			t.Fatalf("alias %q should resolve to a runtime", alias)
		}
		env := RenderEnv(agent, rt, false)
		if !strings.Contains(env, `export BUZZ_ACP_AGENT_COMMAND='claude-agent-acp'`) {
			t.Errorf("alias %q did not canonicalize to claude-agent-acp", alias)
		}
	}
}

// TestRenderLaunchScript_LaunchEpochStamp pins that the launch marker is
// written only when asked, and only where it is meaningful: after the
// double-launch guards, immediately before the agent is spawned. A marker
// written before the guards would appear even on a run that decided not to
// launch, which is exactly the false-positive deploy verification exists to
// catch.
func TestRenderLaunchScript_LaunchEpochStamp(t *testing.T) {
	withID := RenderLaunchScript(false, false, "abc123")
	if !strings.Contains(withID, LaunchEpochPrefix+"abc123") {
		t.Fatal("launch script should stamp the launch id into acp.log")
	}
	stampIdx := strings.Index(withID, LaunchEpochPrefix+"abc123")
	guardIdx := strings.Index(withID, "buzz-acp already running")
	spawnIdx := strings.Index(withID, "setsid nohup")
	if guardIdx >= stampIdx || stampIdx >= spawnIdx {
		t.Fatalf("stamp must sit after the double-launch guard and before the spawn (guard=%d stamp=%d spawn=%d)", guardIdx, stampIdx, spawnIdx)
	}

	// Empty id preserves the previous rendering exactly.
	withoutID := RenderLaunchScript(false, false, "")
	if strings.Contains(withoutID, LaunchEpochPrefix) {
		t.Fatal("empty launch id should omit the stamp entirely")
	}
}
