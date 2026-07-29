package deployflow

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"regexp"

	"github.com/IceRhymers/buzz-lakebox/internal/nest"
	"github.com/IceRhymers/buzz-lakebox/internal/payload"
)

// runtimeOf resolves an agent's runtime for the provisioning flow.
// DeployRequest.Validate() has already rejected an unknown agent_command by
// the time provisioning runs, so the fallback is unreachable in practice —
// it exists so a future caller that skips validation degrades to the
// buzz-agent runtime rather than rendering a zero-value Runtime (which
// would emit an empty BUZZ_ACP_AGENT_COMMAND and produce an agent that
// cannot spawn). Note this REWRITES an unrecognized command rather than
// passing it through, which is why validation, not this, is the real gate.
func runtimeOf(agent payload.Agent) payload.Runtime {
	if rt, ok := payload.RuntimeFor(agent.AgentCommand); ok {
		return rt
	}
	return payload.RuntimeBuzzAgent
}

// newLaunchID returns the per-deploy launch identifier stamped into
// acp.log. NewLaunchID is overridable so tests get deterministic golden
// output; the default is 8 random bytes, which only has to be unique
// against the PREVIOUS launch on the same sandbox, not globally.
//
// A failure to read the system CSPRNG degrades to a fixed sentinel rather
// than failing the deploy: losing stale-log discrimination is strictly
// better than refusing to deploy, and the wait-for-death in
// prelaunchKillScript is the primary defense regardless.
func (d *Deployer) newLaunchID() string {
	if d.NewLaunchID != nil {
		return d.NewLaunchID()
	}
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "unseeded"
	}
	return hex.EncodeToString(b[:])
}

// prelaunchKillWaitSeconds bounds how long the prelaunch kill waits for a
// previous buzz-acp to actually exit after SIGTERM.
//
// Sized against upstream's own shutdown budget rather than guessed:
// buzz-acp's SIGTERM handler drains the pool by shutting down each agent in
// turn, waiting on every child process, plus the relay. 15s covers a
// realistic multi-agent pool with headroom while staying far inside the
// deploy budget. Exceeding it is a real failure (CodeStaleAgent), not a
// reason to launch anyway.
const prelaunchKillWaitSeconds = 15

// prelaunchStillAliveMarker is echoed by the kill script when a buzz-acp is
// STILL alive after the bounded wait and the SIGKILL escalation.
const prelaunchStillAliveMarker = "BUZZ_PRELAUNCH_STILL_ALIVE"

// prelaunchKillScript SIGTERMs any running buzz-acp, waits for it to
// actually die, escalates to SIGKILL, and only then reports whether one is
// still alive.
//
// Every pattern is the bracket idiom ('[b]uzz-acp'), never the literal
// 'buzz-acp': the literal text appears in this very command's own argv as
// seen by the remote shell, so pkill -f would signal its own invoking shell
// and abort the happy path.
//
// Liveness reuses nest.AliveCheckSnippet rather than a bare pgrep because a
// bare pgrep also matches ZOMBIES: buzz-acp is launched detached, so when it
// dies it is reparented to a PID 1 that is not guaranteed to reap it. An
// unreaped <defunct> process would make this wait time out forever on a
// runtime that has actually been dead the whole time.
//
// The script always exits 0 — "still alive" is reported via the marker, not
// via exit status, so a genuine SSH/transport failure stays distinguishable
// from the process simply refusing to die.
func prelaunchKillScript() string {
	return fmt.Sprintf(`set -eu
%s

pkill -f '[b]uzz-acp' 2>/dev/null || true

i=0
while [ "$i" -lt %d ]; do
  if ! buzz_acp_alive; then
    exit 0
  fi
  i=$((i + 1))
  sleep 1
done

# Graceful drain overran its budget: escalate, then give the kernel a
# moment to reap before deciding.
pkill -9 -f '[b]uzz-acp' 2>/dev/null || true
sleep 1
if buzz_acp_alive; then
  echo "%s"
fi
exit 0
`, nest.AliveCheckSnippet, prelaunchKillWaitSeconds, prelaunchStillAliveMarker)
}

// claudeProbeModel is the model id the reachability probe sends. It is a
// PLACEHOLDER, not the model the agent will use: the Claude runtime emits
// no model variable at all (see nest.RenderEnv), so the adapter picks its
// own. The probe only needs a syntactically valid request to get an HTTP
// answer back, which is why an endpoint that does not serve this id is
// explicitly NOT a failure — see the case statement in
// claudeInferenceProbeScript.
const claudeProbeModel = "databricks-claude-sonnet-4-5"

// httpStatusPattern is the exact shape claudeInferenceProbeScript emits for
// BUZZ_CLAUDE_PROBE_STATUS (curl's %{http_code}). Anything else is not ours
// and is reported as "unknown" rather than echoed.
var httpStatusPattern = regexp.MustCompile(`^[0-9]{3}$`)

// claudeProbeCauseMarkerPrefix is the claude inference probe's own cause
// marker, distinct from the zero-token auth probe's so the two can never be
// confused when both run in one deploy (inference_auth="sandbox" + claude).
const claudeProbeCauseMarkerPrefix = "BUZZ_CLAUDE_PROBE_CAUSE="

// claudeInferenceProbeScript verifies the deployed agent can actually reach
// the AI Gateway, using the SAME rendered env file the agent will use — it
// materializes this call's stdin to a mktemp file, sources it, and removes
// it, so what it validates is exactly what launch.sh will export, including
// ClaudeEnvSnippet's derivation.
//
// It writes to a mktemp path and never to nest.EnvFilePath, so a probe
// failure leaves no partially-provisioned state behind.
//
// Causes, each echoed before a non-zero exit so the Go side can
// disambiguate:
//
//   - "unset": ClaudeEnvSnippet produced no ANTHROPIC_BASE_URL or no
//     ANTHROPIC_AUTH_TOKEN. This is the fail-closed path working as
//     designed (a missing DATABRICKS_HOST yields NEITHER variable), caught
//     here rather than shipping an agent that would silently fall back to
//     the public Anthropic API.
//   - "auth": the endpoint answered 401/403 — the credential was refused.
//   - "unreachable": curl could not complete the request at all.
//
// max_tokens:1 keeps the cost of this to a single trivial completion; the
// same call was measured at 1.44s live.
func claudeInferenceProbeScript() string {
	return fmt.Sprintf(`set -eu
BUZZ_PROBE_TMP=$(mktemp)
BUZZ_PROBE_ERR=$(mktemp)
BUZZ_PROBE_HDR=$(mktemp)
# The trap is not belt-and-braces: BUZZ_PROBE_TMP holds the agent's REAL env
# — nsec, auth tag, and the Databricks token. Sourcing it under set -e can
# abort (a readonly-builtin collision, a transport drop, ENOSPC mid-cat),
# and without the trap the plain rm below would be skipped, stranding a
# secret-bearing file that no teardown or undeploy path ever reclaims. Both
# temps are created and trapped up front so neither can outlive the script.
trap 'rm -f "$BUZZ_PROBE_TMP" "$BUZZ_PROBE_ERR" "$BUZZ_PROBE_HDR"' EXIT
cat > "$BUZZ_PROBE_TMP"
# shellcheck disable=SC1090
. "$BUZZ_PROBE_TMP"
rm -f "$BUZZ_PROBE_TMP"

if [ -z "${ANTHROPIC_BASE_URL:-}" ] || [ -z "${ANTHROPIC_AUTH_TOKEN:-}" ]; then
  echo "%sunset"
  exit 1
fi

# The bearer goes in a 0600 config file, never in argv: this probe runs
# BEFORE the prelaunch kill, so a previous deploy's agent may still be alive
# in this sandbox, and /proc/<pid>/cmdline is readable by the same uid.
printf 'header = "Authorization: Bearer %%s"\n' "${ANTHROPIC_AUTH_TOKEN}" > "$BUZZ_PROBE_HDR"

BUZZ_PROBE_CODE=$(curl -sS -o /dev/null -w '%%{http_code}' --max-time 30 \
  -X POST "${ANTHROPIC_BASE_URL%%/}/v1/messages" \
  -K "$BUZZ_PROBE_HDR" \
  -H 'content-type: application/json' \
  -d '{"model":"%s","max_tokens":1,"messages":[{"role":"user","content":"hi"}]}' 2>"$BUZZ_PROBE_ERR") || {
  echo "%sunreachable"
  echo "BUZZ_CLAUDE_PROBE_DETAIL=$(tr -d '\n' < "$BUZZ_PROBE_ERR" | cut -c1-200)"
  exit 1
}

# Only an outright credential rejection fails the deploy. The probe's claim
# is narrow on purpose: "this endpoint is reachable and accepted this
# credential". Any HTTP answer other than 401/403 already proves both —
# including 400/404, which for a bring-your-own endpoint most likely means
# the probe's placeholder model id is not served there rather than that
# anything is misconfigured. Failing on those would break the very
# bring-your-own-endpoint configuration payload validation explicitly
# permits, and would delete a freshly-created sandbox to do it. 429/5xx are
# transient by definition and are likewise not evidence of misconfiguration.
case "$BUZZ_PROBE_CODE" in
  401|403)
    echo "%sauth"
    echo "BUZZ_CLAUDE_PROBE_STATUS=$BUZZ_PROBE_CODE"
    exit 1
    ;;
  *) exit 0 ;;
esac
`, claudeProbeCauseMarkerPrefix, claudeProbeModel, claudeProbeCauseMarkerPrefix, claudeProbeCauseMarkerPrefix)
}

// claudeProbeCauseMessage renders a human diagnosis for one of the probe's
// three disambiguated causes. The raw probe output is scrubbed and bounded
// by the caller via remoteText; nothing secret is interpolated here.
func claudeProbeCauseMessage(cause, out string) string {
	switch cause {
	case "unset":
		return "the rendered env produced no ANTHROPIC_BASE_URL/ANTHROPIC_AUTH_TOKEN — in inference_auth \"env\" set env_vars DATABRICKS_HOST and DATABRICKS_TOKEN; " +
			"in \"sandbox\" mode the baked ~/.databrickscfg did not yield both. The agent was NOT launched, deliberately: without a base URL it would fall back to the public Anthropic API"
	case "auth":
		// Validated to the shape WE emit rather than passed through.
		// probeCause returns the rest of the marker line verbatim from
		// remote stdout, and this value is interpolated into an error
		// that does not otherwise go through remoteText — so anything
		// that can write the sandbox user's shell startup files (which
		// the Claude runtime's own shell tool can) could otherwise push
		// unbounded, unscrubbed text into the operator's terminal.
		status := probeCause(out, "BUZZ_CLAUDE_PROBE_STATUS=")
		if !httpStatusPattern.MatchString(status) {
			status = "unknown"
		}
		return fmt.Sprintf("the inference endpoint refused this credential (HTTP %s) — in inference_auth \"env\" check that env_vars DATABRICKS_TOKEN is valid "+
			"and authorized for the AI Gateway (CAN QUERY on the endpoints); in \"sandbox\" mode the baked credential was rejected, so fall back to env mode for this agent", status)
	case "unreachable":
		// curl's own stderr, bounded server-side (200 chars, newlines
		// stripped) AND put through remoteText here — it is remote text
		// reaching an operator-facing error, so it gets the same
		// truncate+redact treatment every other remote string does.
		detail := probeCause(out, "BUZZ_CLAUDE_PROBE_DETAIL=")
		msg := "could not reach the inference endpoint at all from inside the sandbox — check the host value and the sandbox's network egress"
		if detail != "" {
			msg += ": " + remoteText(detail)
		}
		return msg
	default:
		return "the inference probe failed before reporting a cause"
	}
}
