package deployflow

import "fmt"

// codexProbeCauseMarkerPrefix is the codex inference probe's own cause
// marker, distinct from both the zero-token auth probe's and the claude
// probe's so that markers can never be confused when more than one probe
// runs in a single deploy.
const codexProbeCauseMarkerPrefix = "BUZZ_CODEX_PROBE_CAUSE="

// codexInferenceProbeScript verifies the deployed agent can actually reach
// the AI Gateway, using the SAME rendered env file the agent will use — it
// materializes this call's stdin to a mktemp file, sources it, and removes
// it, so what it validates is exactly what launch.sh will export, including
// CodexEnvSnippet's config generation.
//
// It reads base_url OUT OF THE GENERATED config.toml rather than re-deriving
// it from DATABRICKS_HOST. That is the whole point: re-deriving would test a
// URL the agent may not use, and would silently pass in exactly the case
// worth catching — a config that exists but points somewhere else. Parsing
// the artifact means the probe and the agent cannot disagree.
//
// Unlike the claude probe, this one leaves state behind, and deliberately:
// the generated config.toml is authored by the sourced snippet, not by this
// script, and it is what the agent will use moments later. Removing it would
// undo the provisioning this probe exists to verify. It holds NO secret —
// env_key is a variable NAME, and base_url/model are not credentials — so
// the "no partially-provisioned state" reasoning in claudeInferenceProbeScript
// does not carry over. The env file it sources DOES hold secrets and is
// removed via the same up-front trap.
//
// Causes, each echoed before a non-zero exit so the Go side can
// disambiguate:
//
//   - "unset": CodexEnvSnippet produced no config.toml, or one without a
//     base_url. This is the fail-closed path working as designed (a missing
//     DATABRICKS_HOST yields no file at all), caught here rather than
//     shipping an agent that would fall back to the image's ~/.codex
//     symlink and its baked workspace credential.
//   - "auth": the endpoint answered 401/403 — the credential was refused.
//   - "unreachable": curl could not complete the request at all.
//
// The request is a minimal /responses call. Model support is per-MODEL on
// this gateway, not per-surface — /chat/completions is rejected outright for
// codex-class models (docs/M3_CODEX_PROBE_RESULTS.md G2) — so /responses is
// the only shape worth probing.
func codexInferenceProbeScript() string {
	return fmt.Sprintf(`set -eu
BUZZ_PROBE_TMP=$(mktemp)
BUZZ_PROBE_ERR=$(mktemp)
BUZZ_PROBE_HDR=$(mktemp)
# The trap is not belt-and-braces: BUZZ_PROBE_TMP holds the agent's REAL env
# — nsec, auth tag, and the Databricks token. Sourcing it under set -e can
# abort, and without the trap the plain rm below would be skipped, stranding
# a secret-bearing file that no teardown path ever reclaims.
trap 'rm -f "$BUZZ_PROBE_TMP" "$BUZZ_PROBE_ERR" "$BUZZ_PROBE_HDR"' EXIT
cat > "$BUZZ_PROBE_TMP"
# shellcheck disable=SC1090
. "$BUZZ_PROBE_TMP"
rm -f "$BUZZ_PROBE_TMP"

# Read the endpoint back out of the artifact the agent will use, rather than
# re-deriving it: a config that exists but points elsewhere must fail here.
BUZZ_PROBE_CFG="${CODEX_HOME:-}/config.toml"
BUZZ_PROBE_URL=
if [ -n "${CODEX_HOME:-}" ] && [ -f "$BUZZ_PROBE_CFG" ]; then
  BUZZ_PROBE_URL=$(sed -n 's/^[[:space:]]*base_url[[:space:]]*=[[:space:]]*"\(.*\)"[[:space:]]*$/\1/p' "$BUZZ_PROBE_CFG" | head -n 1)
fi

if [ -z "$BUZZ_PROBE_URL" ] || [ -z "${DATABRICKS_TOKEN:-}" ]; then
  echo "%sunset"
  exit 1
fi

# The bearer goes in a 0600 config file, never in argv: these probes run
# BEFORE the prelaunch kill, so a previous deploy's agent may still be alive
# in this sandbox, and /proc/<pid>/cmdline is readable by the same uid.
#
# But a config file is PARSED, so the token is charset-gated first. curl's
# -K config syntax treats an embedded quote-newline as the end of one directive and
# the start of another, so a crafted token can append a url directive and make
# curl issue a SECOND request replaying this Authorization header and the
# request body to an attacker's endpoint (verified against curl 8.20). It
# also decodes backslash escapes inside double quotes, silently corrupting
# an otherwise valid token into a false "auth" diagnosis. Anything outside
# the base64url/PAT charset fails closed as "unset" instead.
#
# -q MUST stay the FIRST argument, and it is a security control rather
# than tidiness. Without it curl reads $CURL_HOME/.curlrc, then
# $XDG_CONFIG_HOME/curlrc, then $HOME/.curlrc, BEFORE applying argv --
# and a url directive in any of them makes curl issue a second
# request replaying this Authorization header and the request body to
# an attacker's endpoint (reproduced against curl 8.20: two requests,
# the first to the attacker, both carrying the bearer).
#
# That is the same primitive the token charset gate above closes,
# reached through a channel the gate structurally cannot see: it
# inspects a variable, this is a file. It matters here specifically
# because these probes run BEFORE the prelaunch kill, so a previous
# deploy's agent -- the hostile neighbour the -K change exists to
# defend against -- may still be alive to have written it. $HOME
# persists across redeploys into a reused sandbox, which is the
# normal path. curl only honours -q in first position.
case "${DATABRICKS_TOKEN:-}" in
  *[!A-Za-z0-9._~+/=-]*)
    echo "%smalformed"
    exit 1
    ;;
esac
printf 'header = "Authorization: Bearer %%s"\n' "${DATABRICKS_TOKEN}" > "$BUZZ_PROBE_HDR"

BUZZ_PROBE_CODE=$(curl -q -sS -o /dev/null -w '%%{http_code}' --max-time 30 \
  -X POST "${BUZZ_PROBE_URL%%/}/responses" \
  -K "$BUZZ_PROBE_HDR" \
  -H 'content-type: application/json' \
  -d '{"model":"%s","input":"hi"}' 2>"$BUZZ_PROBE_ERR") || {
  echo "%sunreachable"
  echo "BUZZ_CODEX_PROBE_DETAIL=$(tr -d '\n' < "$BUZZ_PROBE_ERR" | cut -c1-200)"
  exit 1
}

# Only an outright credential rejection fails the deploy, for the same
# reason as the claude probe: the claim is narrow on purpose — "this
# endpoint is reachable and accepted this credential". Any other HTTP answer
# already proves both, and failing on 400/404 would delete a freshly-created
# sandbox over a model id rather than a misconfiguration. 429/5xx are
# transient by definition.
case "$BUZZ_PROBE_CODE" in
  401|403)
    echo "%sauth"
    echo "BUZZ_CODEX_PROBE_STATUS=$BUZZ_PROBE_CODE"
    exit 1
    ;;
  *) exit 0 ;;
esac
`, codexProbeCauseMarkerPrefix, codexProbeCauseMarkerPrefix, codexProbeModel, codexProbeCauseMarkerPrefix, codexProbeCauseMarkerPrefix)
}

// codexProbeModel is the model id the reachability probe sends. It matches
// nest.CodexDefaultModel, the id actually written into the generated
// config.toml — unlike the claude probe, whose model is a pure placeholder
// because that runtime emits no model at all. Keeping them equal means a
// gateway that does not serve the configured model shows up here.
const codexProbeModel = "databricks-gpt-5-3-codex"

// codexProbeCauseMessage renders a human diagnosis for one of the probe's
// three disambiguated causes. The raw probe output is scrubbed and bounded
// by the caller via remoteText; nothing secret is interpolated here.
func codexProbeCauseMessage(cause, out string) string {
	switch cause {
	case "malformed":
		// Distinct from "unset" deliberately. Both fail closed, but the
		// remedies point at different variables, and sending an operator
		// to check DATABRICKS_HOST when the host is fine and the TOKEN
		// carries a stray character is worse than no diagnosis. The
		// realistic trigger is not an attacker: SandboxAuthSnippet's awk
		// strips leading whitespace after "=" but the cfg is regenerated
		// into /run on every start (probe S2), so a trailing space or CR
		// on the token line lands here.
		return "the credential contains characters outside the token charset (A-Za-z0-9._~+/=-) and was refused BEFORE use, " +
			"because a config-file parser treats some of them as directive separators — check for whitespace, quoting, or a stray carriage return " +
			"in env_vars DATABRICKS_TOKEN, or on the `token =` line of the sandbox's ~/.databrickscfg"
	case "unset":
		return "the rendered env produced no codex config.toml with a base_url — in inference_auth \"env\" set env_vars DATABRICKS_HOST and DATABRICKS_TOKEN; " +
			"in \"sandbox\" mode the baked ~/.databrickscfg did not yield a host. The agent was NOT launched, deliberately: with no config codex falls back to ~/.codex/config.toml, " +
			"which in this image is a symlink to a baked gateway config that reads the workspace PAT out of ~/.databrickscfg"
	case "auth":
		// Validated to the shape WE emit rather than passed through, for
		// the same reason as the claude probe: this value is interpolated
		// into an error that does not otherwise go through remoteText, and
		// anything able to write the sandbox user's shell startup files
		// could otherwise push unbounded text into the operator's terminal.
		status := probeCause(out, "BUZZ_CODEX_PROBE_STATUS=")
		if !httpStatusPattern.MatchString(status) {
			status = "unknown"
		}
		return fmt.Sprintf("the inference endpoint refused this credential (HTTP %s) — in inference_auth \"env\" check that env_vars DATABRICKS_TOKEN is valid "+
			"and authorized for the AI Gateway (CAN QUERY on the endpoints); in \"sandbox\" mode the baked credential was rejected, so fall back to env mode for this agent", status)
	case "unreachable":
		detail := probeCause(out, "BUZZ_CODEX_PROBE_DETAIL=")
		msg := "could not reach the inference endpoint at all from inside the sandbox — check the host value and the sandbox's network egress"
		if detail != "" {
			msg += ": " + remoteText(detail)
		}
		return msg
	default:
		return "the inference probe failed before reporting a cause"
	}
}
