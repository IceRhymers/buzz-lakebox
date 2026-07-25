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

// shellQuote single-quotes s for safe interpolation into a POSIX shell
// script, escaping any embedded single quote via the standard
// close-quote/escaped-quote/reopen-quote trick. Single-quoted shell
// strings preserve embedded newlines literally, so multi-line values
// (e.g. system_prompt) need no further escaping (PLAN.md §7 golden test:
// "quoting incl. embedded quotes/newlines in system_prompt").
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

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

// PATStub is the comment-only ~/.databrickscfg content that neutralizes
// the baked creator-identity PAT (docs/PLAN.md §4.4 step 4 / §5): no
// profiles, no credentials.
const PATStub = `# Databricks CLI config managed by buzz-backend-databricks.
#
# The baked creator-identity PAT has been intentionally removed by this
# provider (docs/PLAN.md §4.4 step 4, §5): the sandbox's in-image
# ~/.databrickscfg granted owner-level workspace access, which the agent
# must not retain. Inference credentials (if any) are supplied explicitly
# via the DATABRICKS_HOST / DATABRICKS_TOKEN environment instead.
#
# This file intentionally contains no profile sections or credentials.
`

// RenderEnv renders the $HOME/.buzz-backend/env content for agent: one
// `export KEY='value'` line per field (docs/PLAN.md §4.4 step 7 field
// list), in a fixed order, with merged env_vars emitted LAST (sorted by
// key for determinism) so they win over the fixed inference defaults on
// `source` (agent env_vars carry DATABRICKS_HOST/DATABRICKS_TOKEN).
func RenderEnv(agent payload.Agent) string {
	var b strings.Builder
	emit := func(key, value string) {
		b.WriteString("export ")
		b.WriteString(key)
		b.WriteString("=")
		b.WriteString(shellQuote(value))
		b.WriteString("\n")
	}

	emit("BUZZ_PRIVATE_KEY", agent.PrivateKeyNsec)
	emit("BUZZ_AUTH_TAG", agent.AuthTag)
	emit("BUZZ_RELAY_URL", agent.RelayURL)
	emit("BUZZ_ACP_AGENT_COMMAND", agent.AgentCommand)
	emit("BUZZ_ACP_AGENT_ARGS", strings.Join(agent.AgentArgs, " "))
	emit("BUZZ_ACP_AGENTS", strconv.Itoa(agent.Parallelism))
	emit("BUZZ_ACP_SYSTEM_PROMPT", agent.SystemPrompt)
	emit("BUZZ_ACP_MODEL", derefOrEmpty(agent.Model))
	emit("BUZZ_ACP_RESPOND_TO", agent.RespondTo)
	emit("BUZZ_ACP_RESPOND_TO_ALLOWLIST", strings.Join(agent.RespondToAllowlist, ","))
	emit("BUZZ_ACP_TURN_TIMEOUT_SECONDS", strconv.Itoa(agent.TurnTimeoutSeconds))
	emit("BUZZ_ACP_IDLE_TIMEOUT_SECONDS", strconv.Itoa(agent.IdleTimeoutSeconds))
	emit("BUZZ_ACP_MAX_TURN_DURATION_SECONDS", strconv.Itoa(agent.MaxTurnDurationSecs))
	// Same nsec, consumed by git-credential-nostr (docs/PLAN.md §4.4 step 7).
	emit("NOSTR_PRIVATE_KEY", agent.PrivateKeyNsec)

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

	return b.String()
}

// RenderLaunchScript renders the static $HOME/.buzz-backend/launch.sh
// content (docs/PLAN.md §4.4 step 9): re-assert the PAT stub (covers the
// unverified "does start restore the baked file?" question by
// construction), source the env file, provision the nest working dirs,
// guard against a double-launch via flock + pgrep, and launch buzz-acp
// detached. No secret is ever embedded here — it only *sources* the env
// file that was separately written via RunWithStdin.
func RenderLaunchScript() string {
	var b strings.Builder
	b.WriteString("#!/bin/sh\n")
	b.WriteString("set -eu\n\n")

	b.WriteString("# Re-assert the PAT stub on every launch (deploy, `start`, and any future\n")
	b.WriteString("# supervisor all funnel through this script) — covers the unverified\n")
	b.WriteString("# \"does sandbox start restore the baked PAT file?\" case by construction.\n")
	b.WriteString("umask 077\n")
	b.WriteString(`cat > "$HOME/.databrickscfg" <<'BUZZ_PAT_STUB_EOF'` + "\n")
	b.WriteString(PATStub)
	b.WriteString("BUZZ_PAT_STUB_EOF\n\n")

	b.WriteString("# shellcheck disable=SC1090\n")
	b.WriteString(`. "$HOME/.buzz-backend/env"` + "\n\n")

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

	b.WriteString("if pgrep -f buzz-acp >/dev/null 2>&1; then\n")
	b.WriteString(`  echo "buzz-acp already running; not relaunching" >&2` + "\n")
	b.WriteString("  exit 0\n")
	b.WriteString("fi\n\n")

	b.WriteString(`setsid nohup "$HOME/.buzz-backend/bin/buzz-acp" >> "$HOME/.buzz-backend/acp.log" 2>&1 &` + "\n")
	b.WriteString(`echo $! > "$HOME/.buzz-backend/acp.pid"` + "\n")

	return b.String()
}
