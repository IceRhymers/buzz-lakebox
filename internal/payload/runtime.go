package payload

import "fmt"

// Runtime is the agent runtime a deploy request selects via
// agent.agent_command. v0 supported exactly one (buzz-agent); v0.1 adds the
// Claude Code ACP adapter (issue #2) and Codex (issue #3).
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

	// RuntimeCodex is Codex driven through the
	// @agentclientprotocol/codex-acp ACP adapter, with inference pointed at
	// the workspace AI Gateway's codex surface
	// (docs/M3_CODEX_PROBE_RESULTS.md).
	//
	// Unlike the other two runtimes, codex takes its endpoint from a TOML
	// file rather than from environment variables, so the provider renders
	// a config.toml under a provider-owned CODEX_HOME at launch — see
	// nest.CodexEnvSnippet.
	RuntimeCodex Runtime = "codex"
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

	// Bare "codex" IS accepted, unlike bare "claude", and the asymmetry is
	// deliberate rather than an oversight — do not "fix" it by removing
	// this row. Upstream's zero-arg identity list (config.rs:689-691)
	// contains "codex" and "codex-acp" but NOT "claude", so accepting
	// "codex" keeps this provider's accepted set equal to what buzz-acp
	// will spawn with zero args, which is the rule the claude rows follow
	// too. The hazard bare "claude" was excluded for does not apply here
	// because every alias canonicalizes to "codex-acp" (see spawnCommands)
	// before anything reaches the sandbox, so the image's ucode wrapper is
	// unreachable regardless of what the payload said.
	"codex":     RuntimeCodex,
	"codex-acp": RuntimeCodex,
	"codex-cli": RuntimeCodex,
}

// spawnCommands is the canonical command buzz-acp is told to spawn
// (BUZZ_ACP_AGENT_COMMAND) for each Runtime. Canonicalizing here means
// internal/install symlinks exactly one name per runtime and no alias ever
// reaches the sandbox.
var spawnCommands = map[Runtime]string{
	RuntimeBuzzAgent: "buzz-agent",
	RuntimeClaude:    "claude-agent-acp",

	// Never bare "codex": the sandbox image ships /usr/local/bin/codex, a
	// Databricks `ucode` wrapper that takes no arguments and launches an
	// interactive TUI rather than speaking ACP on stdio (probe S1). The
	// adapter's own node_modules/.bin also contains a real `codex`, so the
	// name is doubly ambiguous. Canonicalizing to "codex-acp" means no
	// alias can reach either one.
	RuntimeCodex: "codex-acp",
}

// EnvShape describes which blocks of the rendered agent env file a runtime
// needs. It exists because the alternative — testing `rt != RuntimeClaude`
// at each site, which is what this package encoded while exactly two
// runtimes existed — silently means "buzz-agent OR ANYTHING ADDED LATER".
// A third runtime added under that spelling inherits buzz-agent's model
// variable, its stdio MCP server, and its inference config, none of which an
// ACP adapter can use, and loses the pinned permission mode. That ships
// green — the .deb installs, the ACP handshake passes, the inference probe
// passes — and fails at session time, which is the exact silent-agent class
// nest.go:264-272 and :247-254 record being live-bitten by twice.
//
// Naming is deliberately positive ("does this runtime want buzz-agent's
// wiring?") rather than negative, so the zero value withholds wiring rather
// than granting it: a runtime someone forgets to classify gets nothing it
// cannot use. The omission is still a bug, and TestEnvShapes_CoverEveryRuntime
// fails on it — but it fails safe in the meantime.
type EnvShape struct {
	// BuzzAgentWiring emits buzz-agent's own tool and inference
	// configuration: BUZZ_ACP_MODEL, BUZZ_ACP_MCP_COMMAND,
	// MCP_HOOK_SERVERS, BUZZ_AGENT_PROVIDER, DATABRICKS_MODEL.
	BuzzAgentWiring bool

	// PinACPPermissionMode emits BUZZ_ACP_PERMISSION_MODE for runtimes
	// driven through an ACP adapter. It pins parity with buzz-acp's own
	// default and is NOT a security control — buzz-acp auto-approves every
	// session/request_permission regardless of it.
	PinACPPermissionMode bool
}

// envShapes must have an entry for every Runtime in spawnCommands;
// TestEnvShapes_CoverEveryRuntime enforces it.
var envShapes = map[Runtime]EnvShape{
	RuntimeBuzzAgent: {BuzzAgentWiring: true},
	RuntimeClaude:    {PinACPPermissionMode: true},
	RuntimeCodex:     {PinACPPermissionMode: true},
}

// EnvShape returns the env-rendering capabilities of r.
func (r Runtime) EnvShape() EnvShape { return envShapes[r] }

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
	return "buzz-agent, claude-agent-acp (aliases: claude-code-acp, claude-code, claudecode), codex-acp (aliases: codex, codex-cli)"
}

// unsupportedRuntimeError is the rejection for an agent_command this
// provider does not implement. Every runtime buzz-acp can spawn with zero
// args is now covered except goose, so the error simply names what is
// accepted rather than pointing at a tracking issue.
func unsupportedRuntimeError(agentCommand string) error {
	return fmt.Errorf(
		"agent_command %q is not supported by this provider; supported: %s",
		agentCommand, supportedAgentCommandList(),
	)
}
