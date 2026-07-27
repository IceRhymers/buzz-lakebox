// Package nest renders the in-sandbox "$HOME/.buzz" layout templates:
// the secret-bearing env file, launch.sh, and the comment-only PAT stub
// (docs/PLAN.md §4.4 steps 4, 6, 7, 9). Nothing here talks to a sandbox
// directly — internal/deployflow ships the rendered text over
// internal/sshx (the env file and PAT stub via RunWithStdin, since they
// carry or gate secrets; launch.sh's content is not itself secret and is
// also shipped via stdin purely to keep argv short).
package nest

import (
	"sort"
	"strconv"
	"strings"

	"github.com/IceRhymers/buzz-lakebox/internal/payload"
	"github.com/IceRhymers/buzz-lakebox/internal/shellquote"
)

// EnvFilePath / LaunchScriptPath / PATStubPath are the well-known
// in-sandbox destinations for the rendered templates.
const (
	EnvFilePath      = "$HOME/.buzz-backend/env"
	LaunchScriptPath = "$HOME/.buzz-backend/launch.sh"
	PATStubPath      = "$HOME/.databrickscfg"
)

// DefaultBuzzAgentProvider is used for BUZZ_AGENT_PROVIDER / DATABRICKS
// inference routing when the payload's provider field is empty
// (docs/PLAN.md §4.4 step 7).
const DefaultBuzzAgentProvider = "databricks_v2"

func derefOrEmpty(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func derefOrDefault(s *string, def string) string {
	if s == nil || *s == "" {
		return def
	}
	return *s
}

// PATStubMarker is the first line of PATStub, hoisted into its own
// exported const so a later deploy-time auth probe (internal/deployflow)
// can grep an in-sandbox ~/.databrickscfg for this exact text to detect
// "this sandbox was already deployed in env mode and its baked PAT is
// gone" without keeping a second copy of the literal that could drift
// from PATStub itself.
const PATStubMarker = `# Databricks CLI config managed by buzz-backend-databricks-lakebox.`

// PATStub is the comment-only ~/.databrickscfg content that neutralizes
// the baked creator-identity PAT (docs/PLAN.md §4.4 step 4 / §5): no
// profiles, no credentials. Built from PATStubMarker by compile-time
// concatenation so the two can never drift apart.
const PATStub = PATStubMarker + `
#
# The baked creator-identity PAT has been intentionally removed by this
# provider (docs/PLAN.md §4.4 step 4, §5): the sandbox's in-image
# ~/.databrickscfg granted owner-level workspace access, which the agent
# must not retain. Inference credentials (if any) are supplied explicitly
# via the DATABRICKS_HOST / DATABRICKS_TOKEN environment instead.
#
# This file intentionally contains no profile sections or credentials.
`

// AliveCheckSnippet defines the POSIX shell function every in-sandbox
// buzz-acp liveness test uses: launch.sh's double-launch guard, deploy's
// step-10 verification, and the operator `status` probe.
//
// It is a function rather than a bare `pgrep -f '[b]uzz-acp'` because a
// bare pgrep also matches ZOMBIES. buzz-acp is launched detached
// (`setsid nohup`), so when it dies its parent is already gone and it is
// reparented to the sandbox's PID 1 — `sandbox-daemon`, not an init that
// is guaranteed to reap (docs/M05_PROBE_RESULTS.md §5: systemd is not
// booted). An unreaped <defunct> buzz-acp would make `status` report a
// dead agent as running and make launch.sh refuse to relaunch it —
// exactly the silent-death mode the whole verify path exists to catch.
// Reproduced locally by the guard-proof tests in launch_exec_test.go.
//
// Bracket idiom ('[b]uzz-acp', not 'buzz-acp') for the reason documented
// at deployflow's pkill sites: the literal pattern would otherwise match
// this very command's own argv when run over `databricks sandbox ssh`.
const AliveCheckSnippet = `buzz_acp_alive() {
  for pid in $(pgrep -f '[b]uzz-acp' 2>/dev/null); do
    st=$(ps -o stat= -p "$pid" 2>/dev/null | tr -d ' ')
    case "$st" in
      Z*|"") continue ;;
      *) return 0 ;;
    esac
  done
  return 1
}`

// SandboxAuthSnippet derives DATABRICKS_HOST/DATABRICKS_TOKEN at launch
// time from the sandbox's baked creator-identity ~/.databrickscfg, for
// provider_config.inference_auth="sandbox" (docs/PLAN.md zero-token
// design, option A). RenderEnv appends it, verbatim and with no payload
// interpolation, after the sorted env_vars block. Both launch.sh (step 9)
// and install.BuildVerifyCommand's handshake `.`-source the rendered env
// content, the latter under `set -a` — so this text has to survive both
// sourcing contexts unconditionally. That drives every rule below:
//
//   - It is a no-op unless DATABRICKS_TOKEN is unset/empty AND
//     ~/.databrickscfg is readable, so env_vars-supplied credentials (or
//     a value some other mechanism already exported) always win —
//     derivation is a fallback, never an override.
//   - [DEFAULT]-section-scoped extraction only: a small awk state machine
//     tracks whether the current line is inside "[DEFAULT]" and resets on
//     any other "[section]" line, so profiles before/after DEFAULT (or a
//     DEFAULT block that isn't first) can't leak their host/token in.
//     "key = value" and "key=value" spacing are both tolerated (optional
//     tabs/spaces around "="); the value is taken verbatim (docs/M05
//     precedent: host used as-is) with no scheme normalization or
//     trimming beyond the delimiter's own surrounding whitespace.
//   - R3(a): every command substitution below ends `2>/dev/null || true`,
//     AND the awk program itself always `exit 0`s from its END block —
//     belt and suspenders — so a parse failure can never hand a non-zero
//     status to the sourcing `set -eu` shell.
//   - R3(b): control flow is if/fi only; there is no top-level `&&` chain
//     standing in for it (the `&&` inside the outer `if [ ... ] && [ ... ];
//     then` is the condition of that if, which `set -e` always exempts,
//     not a trailing list). The snippet's last line is a bare `:`, so
//     whatever the last `if` decided, the `.` builtin's reported status
//     (the status of the last command run) is always 0 — sourcing can
//     never die here even when this is the last content in the file.
//   - R3(c): every scratch variable is `buzz_`-prefixed and explicitly
//     `unset` before the final `:`. install.BuildVerifyCommand sources
//     this content under `set -a`, which auto-exports every variable
//     assigned afterward; without the unset, buzz_awk_extract/buzz_host/
//     buzz_token would leak into buzz-agent's own handshake environment.
const SandboxAuthSnippet = `# Zero-token inference auth (provider_config.inference_auth="sandbox"):
# derive DATABRICKS_HOST/DATABRICKS_TOKEN from the sandbox's baked
# creator-identity ~/.databrickscfg, only if not already set above by
# env_vars. This block is a fallback, never an override: env_vars are
# rendered first and this only fires when DATABRICKS_TOKEN is still unset.
if [ -z "${DATABRICKS_TOKEN:-}" ] && [ -r "$HOME/.databrickscfg" ]; then
  buzz_awk_extract='
    BEGIN { insec = 0; found = 0 }
    /^\[/ {
      insec = ($0 == "[DEFAULT]") ? 1 : 0
      next
    }
    insec && !found {
      line = $0
      sub(/^[ \t]+/, "", line)
      if (line ~ ("^" want "[ \t]*=")) {
        sub(("^" want "[ \t]*=[ \t]*"), "", line)
        print line
        found = 1
      }
    }
    END { exit 0 }
  '
  buzz_host=$(awk -v want=host "$buzz_awk_extract" "$HOME/.databrickscfg" 2>/dev/null || true)
  buzz_token=$(awk -v want=token "$buzz_awk_extract" "$HOME/.databrickscfg" 2>/dev/null || true)

  if [ -z "${DATABRICKS_HOST:-}" ] && [ -n "$buzz_host" ]; then
    export DATABRICKS_HOST="$buzz_host"
  fi
  if [ -n "$buzz_token" ]; then
    export DATABRICKS_TOKEN="$buzz_token"
  fi
fi
unset buzz_awk_extract buzz_host buzz_token
:
`

// RenderEnv renders the $HOME/.buzz-backend/env content for agent: one
// `export KEY='value'` line per field (docs/PLAN.md §4.4 step 7 field
// list), in a fixed order, with merged env_vars emitted LAST (sorted by
// key for determinism) so they win over the fixed inference defaults on
// `source` (agent env_vars carry DATABRICKS_HOST/DATABRICKS_TOKEN).
//
// sandboxInferenceAuth mirrors provider_config.inference_auth=="sandbox":
// when true, SandboxAuthSnippet is appended AFTER the env_vars block, so
// it can only ever fill in credentials env_vars didn't already supply.
// When false, output is byte-identical to the pre-zero-token behavior.
func RenderEnv(agent payload.Agent, sandboxInferenceAuth bool) string {
	var b strings.Builder
	// emit writes KEY unquoted/raw (only VALUE is shellquote'd) into a file
	// that RenderLaunchScript's `. "$HOME/.buzz-backend/env"` line
	// `.`-sources with a shell — an attacker-controlled key containing
	// shell metacharacters or a newline would execute in that shell, which
	// has just exported the agent's nsec, auth tag, and DATABRICKS_TOKEN.
	// This is safe ONLY because agent.EnvVars keys are validated upstream
	// by payload.Agent.Validate() (^[A-Za-z_][A-Za-z0-9_]*$) before
	// RenderEnv ever sees them; do not call RenderEnv on unvalidated input.
	emit := func(key, value string) {
		b.WriteString("export ")
		b.WriteString(key)
		b.WriteString("=")
		b.WriteString(shellquote.Single(value))
		b.WriteString("\n")
	}

	emit("BUZZ_PRIVATE_KEY", agent.PrivateKeyNsec)
	emit("BUZZ_AUTH_TAG", agent.AuthTag)
	emit("BUZZ_RELAY_URL", agent.RelayURL)
	emit("BUZZ_ACP_AGENT_COMMAND", agent.AgentCommand)
	// buzz-acp splits BUZZ_ACP_AGENT_ARGS on COMMAS, not spaces
	// (block/buzz crates/buzz-acp/README.md: "comma-separated";
	// crates/buzz-acp/src/config.rs value_delimiter = ','; the desktop
	// joins with ",") — BUG 7 fix.
	emit("BUZZ_ACP_AGENT_ARGS", strings.Join(agent.AgentArgs, ","))
	emit("BUZZ_ACP_AGENTS", strconv.Itoa(agent.Parallelism))
	emit("BUZZ_ACP_SYSTEM_PROMPT", agent.SystemPrompt)
	emit("BUZZ_ACP_MODEL", derefOrEmpty(agent.Model))
	emit("BUZZ_ACP_RESPOND_TO", agent.RespondTo)
	// Only emit BUZZ_ACP_RESPOND_TO_ALLOWLIST in allowlist mode: the
	// desktop sets it only when the list is non-empty and removes it
	// otherwise (block/buzz desktop/src-tauri/src/managed_agents/
	// runtime.rs:1574-1576) — BUG 7 fix. Comma-joined, same contract as
	// BUZZ_ACP_AGENT_ARGS above.
	if len(agent.RespondToAllowlist) > 0 {
		emit("BUZZ_ACP_RESPOND_TO_ALLOWLIST", strings.Join(agent.RespondToAllowlist, ","))
	}
	// Timeout env names must match buzz-acp's real contract:
	// BUZZ_ACP_IDLE_TIMEOUT / BUZZ_ACP_MAX_TURN_DURATION — the previous
	// *_SECONDS-suffixed names exist nowhere in buzz-acp and were
	// silently ignored (live-verified via the startup config line, which
	// showed the 900s/7200s defaults despite our exports). Mirror the
	// desktop (block/buzz runtime.rs:1847-1858): emit only when
	// explicitly set, and never emit the upstream-deprecated
	// BUZZ_ACP_TURN_TIMEOUT at all.
	if agent.IdleTimeoutSeconds > 0 {
		emit("BUZZ_ACP_IDLE_TIMEOUT", strconv.Itoa(agent.IdleTimeoutSeconds))
	}
	if agent.MaxTurnDurationSecs > 0 {
		emit("BUZZ_ACP_MAX_TURN_DURATION", strconv.Itoa(agent.MaxTurnDurationSecs))
	}
	// Same nsec, consumed by git-credential-nostr (docs/PLAN.md §4.4 step 7).
	emit("NOSTR_PRIVATE_KEY", agent.PrivateKeyNsec)

	// Tool wiring. Without BUZZ_ACP_MCP_COMMAND, session/new carries
	// mcpServers:[] and buzz-agent has NO tools — the model then emits
	// its bash tool call as plain text and ends the turn, so the agent
	// can never run `buzz messages send`: deployed agents look alive but
	// are permanently silent (live-bitten). buzz-dev-mcp ships in the
	// same .deb the installer unpacks and resolves via launch.sh's PATH
	// prepend. Desktop parity: block/buzz runtime.rs:1723-1739 +
	// discovery.rs buzz-agent entry (mcp_command "buzz-dev-mcp",
	// mcp_hooks true → MCP_HOOK_SERVERS "*").
	emit("BUZZ_ACP_MCP_COMMAND", "buzz-dev-mcp")
	emit("MCP_HOOK_SERVERS", "*")
	// Observer frames are the desktop's ONLY health signal for a
	// provider-deployed agent (block/buzz runtime.rs:1934).
	emit("BUZZ_ACP_RELAY_OBSERVER", "true")
	// Explicit parity with the desktop's mention-handling knobs
	// (block/buzz runtime.rs:1861-1862); these currently match
	// buzz-acp's defaults, pinned here so an upstream default change
	// can't silently diverge sandbox agents from desktop ones.
	emit("BUZZ_ACP_DEDUP", "queue")
	emit("BUZZ_ACP_MULTIPLE_EVENT_HANDLING", "steer")

	// Inference auth for buzz-agent (docs/M05_PROBE_RESULTS.md §2,
	// docs/CONTRACT.md §7): BUZZ_AGENT_PROVIDER defaults to
	// DefaultBuzzAgentProvider when the payload's provider is empty;
	// DATABRICKS_HOST/DATABRICKS_TOKEN arrive via env_vars below and
	// override nothing here since those keys aren't set above.
	emit("BUZZ_AGENT_PROVIDER", derefOrDefault(agent.Provider, DefaultBuzzAgentProvider))
	emit("DATABRICKS_MODEL", derefOrEmpty(agent.Model))

	keys := make([]string, 0, len(agent.EnvVars))
	for k := range agent.EnvVars {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		emit(k, agent.EnvVars[k])
	}

	if sandboxInferenceAuth {
		b.WriteString("\n")
		b.WriteString(SandboxAuthSnippet)
	}

	return b.String()
}

// RenderLaunchScript renders the static $HOME/.buzz-backend/launch.sh
// content (docs/PLAN.md §4.4 step 9): re-assert the PAT stub (covers the
// unverified "does start restore the baked file?" question by
// construction) UNLESS keepWorkspacePAT is true, source the env file,
// provision the nest working dirs, guard against a double-launch via
// flock + pgrep, and launch buzz-acp detached. No secret is ever
// embedded here — it only *sources* the env file that was separately
// written via RunWithStdin.
//
// keepWorkspacePAT mirrors provider_config.keep_workspace_pat (BUG 3
// fix): when true, this script must NOT re-assert the stub, since
// launch.sh runs on every deploy AND every future `start`/supervisor
// relaunch — unconditionally re-asserting would clobber the owner's kept
// PAT on the very first relaunch after deploy, defeating the opt-out.
//
// sandboxInferenceAuth mirrors provider_config.inference_auth=="sandbox"
// and ALSO skips the stub, for the same underlying reason: zero-token
// mode needs the sandbox's baked creator-identity ~/.databrickscfg intact
// so SandboxAuthSnippet (rendered into the env file by RenderEnv, sourced
// a few lines below) has something to derive DATABRICKS_HOST/
// DATABRICKS_TOKEN from at every launch. inference_auth:"sandbox"
// therefore supersedes keep_workspace_pat: either flag alone is enough to
// keep the cfg, and setting both is redundant but harmless.
func RenderLaunchScript(keepWorkspacePAT, sandboxInferenceAuth bool) string {
	var b strings.Builder
	b.WriteString("#!/bin/sh\n")
	b.WriteString("set -eu\n\n")

	if !keepWorkspacePAT && !sandboxInferenceAuth {
		b.WriteString("# Re-assert the PAT stub on every launch (deploy, `start`, and any future\n")
		b.WriteString("# supervisor all funnel through this script) — covers the unverified\n")
		b.WriteString("# \"does sandbox start restore the baked PAT file?\" case by construction.\n")
		b.WriteString("# Skipped entirely when provider_config.keep_workspace_pat=true, so the\n")
		b.WriteString("# owner's retained PAT survives every relaunch, not just the initial\n")
		b.WriteString("# deploy (BUG 3 fix: this used to run unconditionally here). Also\n")
		b.WriteString("# skipped when provider_config.inference_auth=\"sandbox\": that mode\n")
		b.WriteString("# needs the baked cfg intact for SandboxAuthSnippet to derive\n")
		b.WriteString("# credentials from, and it supersedes keep_workspace_pat.\n")
		b.WriteString("umask 077\n")
		b.WriteString(`cat > "$HOME/.databrickscfg" <<'BUZZ_PAT_STUB_EOF'` + "\n")
		b.WriteString(PATStub)
		b.WriteString("BUZZ_PAT_STUB_EOF\n\n")
	}

	b.WriteString("# shellcheck disable=SC1090\n")
	b.WriteString(`. "$HOME/.buzz-backend/env"` + "\n\n")

	b.WriteString("# Installed Buzz binaries must be on PATH: buzz-acp spawns the agent\n")
	b.WriteString("# command (BUZZ_ACP_AGENT_COMMAND=\"buzz-agent\") by bare name, and the\n")
	b.WriteString("# agent itself shells out to `buzz` and git-credential-nostr — none of\n")
	b.WriteString("# which the sandbox's default PATH can see (live-bitten: every worker\n")
	b.WriteString("# died at spawn with \"No such file or directory\"; only buzz-acp itself\n")
	b.WriteString("# survived because this script launches it by absolute path).\n")
	b.WriteString(`export PATH="$HOME/.buzz-backend/bin:$PATH"` + "\n\n")

	b.WriteString(`mkdir -p "$HOME/.buzz" "$HOME/.buzz/REPOS" "$HOME/.buzz/OUTBOX" "$HOME/.buzz-backend"` + "\n")
	b.WriteString(`cd "$HOME/.buzz"` + "\n\n")

	b.WriteString("# flock + pidfile + pgrep guard against a double-launch (redeploy /\n")
	b.WriteString("# concurrent start racing an already-running buzz-acp).\n")
	b.WriteString(`LOCK="$HOME/.buzz-backend/launch.lock"` + "\n")
	b.WriteString(`exec 9>"$LOCK"` + "\n")
	b.WriteString("if ! flock -n 9; then\n")
	b.WriteString(`  echo "launch.sh already running (flock held); exiting" >&2` + "\n")
	b.WriteString("  exit 0\n")
	b.WriteString("fi\n\n")

	// A zombie buzz-acp must not count as "already running" — see
	// AliveCheckSnippet.
	b.WriteString(AliveCheckSnippet)
	b.WriteString("\n\n")
	b.WriteString("if buzz_acp_alive; then\n")
	b.WriteString(`  echo "buzz-acp already running; not relaunching" >&2` + "\n")
	b.WriteString("  exit 0\n")
	b.WriteString("fi\n\n")

	// 9>&- closes the lock fd in the launched child. Without it the agent
	// — and every worker it spawns — inherits the open descriptor and so
	// keeps the flock held for as long as ANY of them lives. A crashed
	// buzz-acp with one lingering worker would then make every future
	// `start` exit early with "already running (flock held)" while the
	// agent is in fact dead: unrecoverable without manual intervention.
	// The lock's job is only to serialize concurrent launch.sh runs;
	// buzz_acp_alive above is what guards against a live agent.
	b.WriteString(`setsid nohup "$HOME/.buzz-backend/bin/buzz-acp" >> "$HOME/.buzz-backend/acp.log" 2>&1 9>&- &` + "\n")
	b.WriteString(`echo $! > "$HOME/.buzz-backend/acp.pid"` + "\n")

	return b.String()
}
