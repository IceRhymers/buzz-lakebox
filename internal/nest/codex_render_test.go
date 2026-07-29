package nest

import (
	"strings"
	"testing"

	"github.com/IceRhymers/buzz-lakebox/internal/payload"
)

func codexTestAgent() payload.Agent {
	a := claudeTestAgent()
	a.AgentCommand = "codex-acp"
	return a
}

// TestRenderEnv_CodexGolden is dominated by its negative assertions, and
// that is the point. Every variable listed in dontWant would be inherited
// automatically if RenderEnv still spelled its runtime branches as
// `rt != payload.RuntimeClaude` — which reads as "buzz-agent" but means
// "buzz-agent OR anything added later". Each would fail quietly and at
// session time rather than at deploy time:
//
//   - BUZZ_ACP_MODEL: buzz-acp would attempt a per-session switch to a
//     gateway id absent from codex's catalog, emitting an unsupported_model
//     observer frame on every session (nest.go:224-234). The model belongs
//     in the generated config.toml instead.
//   - BUZZ_ACP_MCP_COMMAND / MCP_HOOK_SERVERS: buzz-dev-mcp is a STDIO MCP
//     server, and codex advertises mcpCapabilities {acp:false, http:true,
//     sse:false} — no stdio MCP at all (docs/M3_CODEX_PROBE_RESULTS.md S4).
//   - BUZZ_AGENT_PROVIDER / DATABRICKS_MODEL: buzz-agent's own inference
//     config, meaningless to an ACP adapter that reads a config.toml.
//
// The deploy would still go green — .deb installs, ACP handshake passes,
// inference probe passes — which is exactly the silent-agent class this repo
// records being live-bitten by twice.
func TestRenderEnv_CodexGolden(t *testing.T) {
	env := RenderEnv(codexTestAgent(), payload.RuntimeCodex, false)

	for _, want := range []string{
		`export BUZZ_ACP_AGENT_COMMAND='codex-acp'`,
		`export BUZZ_ACP_PERMISSION_MODE='bypass-permissions'`,
		`export BUZZ_ACP_RELAY_OBSERVER='true'`,
	} {
		if !strings.Contains(env, want) {
			t.Errorf("codex env missing %q", want)
		}
	}

	for _, dontWant := range []string{
		"BUZZ_ACP_MODEL=",
		"BUZZ_ACP_MCP_COMMAND=",
		"MCP_HOOK_SERVERS=",
		"BUZZ_AGENT_PROVIDER=",
		"DATABRICKS_MODEL=",
	} {
		if strings.Contains(env, dontWant) {
			t.Errorf("codex env must not contain %q — it is buzz-agent wiring an ACP adapter cannot use", dontWant)
		}
	}
}

// TestRenderEnv_CodexNoInertSandboxKeys pins the omission so it cannot be
// "fixed" by a reader who never opens codex.go. sandbox_mode and
// approval_policy are inert under codex-acp@1.1.7: the adapter applies an
// AgentMode preset per session that supersedes the file
// (docs/M3_CODEX_PROBE_RESULTS.md S7 measured read-only permitting writes;
// S10 read the mechanism out of the adapter source). Emitting them would put
// text in a generated config that reads like a security control, is reviewed
// as one, and constrains nothing.
func TestRenderEnv_CodexNoInertSandboxKeys(t *testing.T) {
	env := RenderEnv(codexTestAgent(), payload.RuntimeCodex, true)
	for _, dontWant := range []string{"sandbox_mode", "approval_policy"} {
		if strings.Contains(env, dontWant) {
			t.Errorf("generated codex config must not set %q: it is inert under the pinned adapter and reads as a control it is not", dontWant)
		}
	}
}

// TestRenderEnv_CodexNeverSpawnsBareCodex is the render-level half of the
// ucode-wrapper defense: whatever alias the payload used, no rendered
// artifact may tell buzz-acp to spawn bare `codex`.
func TestRenderEnv_CodexNeverSpawnsBareCodex(t *testing.T) {
	for _, alias := range []string{"codex", "codex-acp", "codex-cli"} {
		agent := codexTestAgent()
		agent.AgentCommand = alias
		env := RenderEnv(agent, payload.RuntimeCodex, false)
		if strings.Contains(env, `BUZZ_ACP_AGENT_COMMAND='codex'`) {
			t.Errorf("alias %q rendered a bare `codex` spawn command; the image's ucode wrapper does not speak ACP", alias)
		}
		if !strings.Contains(env, `BUZZ_ACP_AGENT_COMMAND='codex-acp'`) {
			t.Errorf("alias %q did not canonicalize to codex-acp", alias)
		}
	}
}

// TestRenderEnv_CodexSnippetOrderedLast pins the ordering the whole design
// rests on: CodexEnvSnippet must come after the env_vars block AND after
// SandboxAuthSnippet, because those are the two places DATABRICKS_HOST can
// come from. If it moved earlier it would derive nothing in sandbox mode and
// silently write no config.
func TestRenderEnv_CodexSnippetOrderedLast(t *testing.T) {
	env := RenderEnv(codexTestAgent(), payload.RuntimeCodex, true)
	if !strings.HasSuffix(env, CodexEnvSnippet) {
		t.Fatal("CodexEnvSnippet must be the last content of the rendered env")
	}
	iAuth := strings.Index(env, SandboxAuthSnippet)
	iCodex := strings.Index(env, CodexEnvSnippet)
	if iAuth < 0 || iCodex < iAuth {
		t.Fatalf("CodexEnvSnippet (at %d) must follow SandboxAuthSnippet (at %d)", iCodex, iAuth)
	}
}
