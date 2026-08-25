package nest

import (
	"strings"
	"testing"

	"github.com/IceRhymers/buzz-lakebox/internal/payload"
)

// TestRenderEnv_McpDirectEmitsCommandForAllRuntimes asserts that a non-empty
// mcpCommand causes BUZZ_ACP_MCP_COMMAND=<command> to be emitted for EVERY
// runtime — buzz-agent, codex, AND claude. This is the issue #14 win for
// claude, which by default emits nothing (EnvShape.StdioMCPCommand=false).
// The exact export line is asserted, not just containment of the value string.
func TestRenderEnv_McpDirectEmitsCommandForAllRuntimes(t *testing.T) {
	const mcpCmd = "shellbox"
	const wantLine = `export BUZZ_ACP_MCP_COMMAND='shellbox'`

	buzzAgent := claudeTestAgent()
	buzzAgent.AgentCommand = "buzz-agent"

	cases := []struct {
		name  string
		agent payload.Agent
		rt    payload.Runtime
	}{
		{"buzz-agent", buzzAgent, payload.RuntimeBuzzAgent},
		{"codex", codexTestAgent(), payload.RuntimeCodex},
		{"claude", claudeTestAgent(), payload.RuntimeClaude},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := RenderEnv(tc.agent, tc.rt, false, mcpCmd)
			if !strings.Contains(env, wantLine) {
				t.Errorf("runtime %s: env missing %q when mcpCommand=%q\n--- got ---\n%s",
					tc.name, wantLine, mcpCmd, env)
			}
		})
	}
}

// TestRenderEnv_McpDirectOverridesShapeDefault confirms that a non-empty
// mcpCommand replaces the runtime's EnvShape.StdioMCPCommand default
// (buzz-dev-mcp) rather than co-existing with it. The shape default must not
// appear in the output when mcpCommand overrides it.
func TestRenderEnv_McpDirectOverridesShapeDefault(t *testing.T) {
	agent := claudeTestAgent()
	agent.AgentCommand = "buzz-agent"
	env := RenderEnv(agent, payload.RuntimeBuzzAgent, false, "shellbox")

	if !strings.Contains(env, `export BUZZ_ACP_MCP_COMMAND='shellbox'`) {
		t.Errorf("buzz-agent: expected BUZZ_ACP_MCP_COMMAND='shellbox'\n--- got ---\n%s", env)
	}
	if strings.Contains(env, `BUZZ_ACP_MCP_COMMAND='buzz-dev-mcp'`) {
		t.Errorf("buzz-agent: shape default 'buzz-dev-mcp' must not appear when mcpCommand overrides it\n--- got ---\n%s", env)
	}
}

// TestRenderEnv_McpMuxEmitsBzmux asserts that RenderEnv emits
// BUZZ_ACP_MCP_COMMAND='bzmux' when called with the MCP-mux command
// (payload.MuxBinaryName = "bzmux").  Matches the assertion style of
// TestRenderEnv_McpDirectEmitsCommandForAllRuntimes.
func TestRenderEnv_McpMuxEmitsBzmux(t *testing.T) {
	agent := claudeTestAgent()
	agent.AgentCommand = "buzz-agent"
	const wantLine = `export BUZZ_ACP_MCP_COMMAND='bzmux'`
	env := RenderEnv(agent, payload.RuntimeBuzzAgent, false, payload.MuxBinaryName)
	if !strings.Contains(env, wantLine) {
		t.Errorf("env missing %q when mcpCommand=%q:\n%s", wantLine, payload.MuxBinaryName, env)
	}
}

// TestRenderEnv_OwnerEnvVarsWinOverMcpCommand asserts that an agent whose
// env_vars supplies BUZZ_ACP_MCP_COMMAND renders it AFTER the provider-emitted
// mcpCommand, so sourcing the file leaves the owner's value as the effective
// one. This preserves the "escape hatch" comment in nest.go (L364-366): an
// owner who wants different tooling can override BUZZ_ACP_MCP_COMMAND via
// env_vars and that override wins.
func TestRenderEnv_OwnerEnvVarsWinOverMcpCommand(t *testing.T) {
	agent := claudeTestAgent()
	agent.AgentCommand = "buzz-agent"
	// Owner overrides the MCP command via env_vars.
	agent.EnvVars["BUZZ_ACP_MCP_COMMAND"] = "owner-value"

	env := RenderEnv(agent, payload.RuntimeBuzzAgent, false, "provider-value")

	const providerLine = `export BUZZ_ACP_MCP_COMMAND='provider-value'`
	const ownerLine = `export BUZZ_ACP_MCP_COMMAND='owner-value'`

	// Both lines must be present (provider emitted by the mcpCommand path,
	// owner emitted by the env_vars block).
	if !strings.Contains(env, providerLine) {
		t.Errorf("provider-emitted line %q missing from env:\n%s", providerLine, env)
	}
	if !strings.Contains(env, ownerLine) {
		t.Errorf("owner env_vars line %q missing from env:\n%s", ownerLine, env)
	}

	// Owner's line must appear AFTER the provider's: the last `export` wins
	// when the file is sourced, so position encodes precedence.
	providerIdx := strings.Index(env, providerLine)
	ownerIdx := strings.Index(env, ownerLine)
	if ownerIdx <= providerIdx {
		t.Errorf(
			"owner env_vars must render AFTER the provider mcpCommand so the owner value wins on source "+
				"(provider at byte %d, owner at byte %d):\n%s",
			providerIdx, ownerIdx, env,
		)
	}
}
