package payload

import "fmt"

// Runtime is the agent runtime a deploy request selects via
// agent.agent_command. v0 supported exactly one (buzz-agent); v0.1 adds the
// Claude Code ACP adapter (issue #2). Codex is still outstanding (issue #3).
//
// The provider branches on Runtime — never on the raw agent_command string —
// so the alias set below is the only place aliases exist. Everything
// downstream (env rendering, install, verification) sees a canonical value.
type Runtime string

const (
	// RuntimeBuzzAgent is Buzz's own agent, shipped in the pinned .deb.
	RuntimeBuzzAgent Runtime = "buzz-agent"

	// RuntimeClaude is Claude Code driven through the
	// @agentclientprotocol/claude-agent-acp ACP adapter, with inference
	// pointed at the workspace AI Gateway's Anthropic-shaped surface
	// (docs/M2_CLAUDE_PROBE_RESULTS.md).
	RuntimeClaude Runtime = "claude"
)

// SupportedAgentCommand is the v0 agent_command value, retained as a named
// constant because docs and older callers refer to it.
//
// Deprecated: branch on Runtime via RuntimeFor instead — the provider now
// supports more than one agent_command.
const SupportedAgentCommand = string(RuntimeBuzzAgent)

// supportedAgentCommands maps every accepted agent_command to its Runtime.
//
// The Claude alias set is the exact intersection of the two upstream lists
// that matter, so a payload this provider accepts is always one buzz-acp
// will also spawn with zero args:
//
//   - block/buzz crates/buzz-acp/src/config.rs:691 default_agent_args() —
//     the zero-arg agent identities, matched after
//     normalize_agent_command_identity()
//   - block/buzz desktop/src-tauri/src/managed_agents/discovery.rs:104-105 —
//     the "claude" KnownAcpRuntime's commands + aliases
//
// Bare "claude" is deliberately absent from both upstream lists: it names the
// underlying CLI binary, not the ACP adapter, and accepting it here would
// spawn a program that does not speak ACP on stdio.
var supportedAgentCommands = map[string]Runtime{
	"buzz-agent": RuntimeBuzzAgent,

	"claude-agent-acp": RuntimeClaude,
	"claude-code-acp":  RuntimeClaude,
	"claude-code":      RuntimeClaude,
	"claudecode":       RuntimeClaude,
}

// spawnCommands is the canonical command buzz-acp is told to spawn
// (BUZZ_ACP_AGENT_COMMAND) for each Runtime. Canonicalizing here means
// internal/install symlinks exactly one name per runtime and no alias ever
// reaches the sandbox.
var spawnCommands = map[Runtime]string{
	RuntimeBuzzAgent: "buzz-agent",
	RuntimeClaude:    "claude-agent-acp",
}

// RuntimeFor resolves an agent_command to its Runtime. Matching is
// exact-case, mirroring the deliberate no-normalization stance of
// validInferenceAuthValues: a typo must fail loudly rather than quietly
// resolve to some other runtime.
func RuntimeFor(agentCommand string) (Runtime, bool) {
	rt, ok := supportedAgentCommands[agentCommand]
	return rt, ok
}

// SpawnCommand is the canonical BUZZ_ACP_AGENT_COMMAND value for r.
func (r Runtime) SpawnCommand() string {
	return spawnCommands[r]
}

// supportedAgentCommandList renders the accepted agent_command values for
// error text, in a fixed order (map iteration order is randomized, and this
// string is asserted on by tests).
func supportedAgentCommandList() string {
	return "buzz-agent, claude-agent-acp (aliases: claude-code-acp, claude-code, claudecode)"
}

// unsupportedRuntimeError is the rejection for an agent_command this
// provider does not implement. It points at issue #3 (codex), the only
// runtime still outstanding — issue #1 (buzz-agent) and #2 (claude) are
// both closed by the runtimes above.
func unsupportedRuntimeError(agentCommand string) error {
	return fmt.Errorf(
		"agent_command %q is not supported by this provider; supported: %s — see https://github.com/IceRhymers/buzz-lakebox/issues/3 for codex support",
		agentCommand, supportedAgentCommandList(),
	)
}
