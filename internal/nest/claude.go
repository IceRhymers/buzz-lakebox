package nest

// ClaudeEnvSnippet points the Claude Code ACP adapter at the workspace AI
// Gateway's Anthropic-shaped surface. RenderEnv appends it verbatim, with no
// payload interpolation, as the LAST content of the env file — after the
// sorted env_vars block AND after SandboxAuthSnippet.
//
// Why it must be a shell derivation appended last, rather than plain
// `export` lines emitted with the other fixed vars: the two inputs it needs
// (DATABRICKS_HOST, DATABRICKS_TOKEN) do not exist when the fixed block is
// written. In inference_auth="env" they arrive from the agent's env_vars,
// which RenderEnv emits after the fixed block; in inference_auth="sandbox"
// they are materialized by SandboxAuthSnippet, which is appended after
// THAT. Appending last is what lets one identical text serve both modes —
// the mode difference is invisible here.
//
// FAIL-CLOSED COUPLING (the reason ANTHROPIC_AUTH_TOKEN is gated on
// buzz_derived_url rather than on ANTHROPIC_BASE_URL being merely non-empty):
//
// Claude Code falls back to https://api.anthropic.com when
// ANTHROPIC_BASE_URL is unset, and a Lakebox sandbox has OPEN egress to that
// host — probed live, HTTP 401 in 147ms with a genuine Anthropic error body
// and request_id (docs/M2_CLAUDE_PROBE_RESULTS.md). ANTHROPIC_AUTH_TOKEN is
// sent as `Authorization: Bearer <token>` regardless of which host it
// reaches. So if the token were derived independently, an env-mode deploy
// that supplied DATABRICKS_TOKEN but omitted DATABRICKS_HOST would ship a
// live workspace PAT to a third party — silently, because this snippet
// cannot fail loudly (see below) and because no provider-side gate touches
// the LLM: install verification is an ACP handshake and launch verification
// reads a log line. Deriving the token ONLY where a base URL is known makes
// the missing-host case inert instead of leaking.
//
// The payload layer rejects that configuration outright
// (payload.DeployRequest.validateClaudeInferenceSource), so this is the
// second of two independent defenses. It is the one that also covers
// sandbox mode, where the host is derived at runtime and no payload-time
// check can see it — e.g. a baked ~/.databrickscfg carrying a token line
// but no host line.
//
// The gate is "did WE derive this endpoint", not "is some endpoint set",
// and the difference is load-bearing. Gating on a merely-non-empty
// ANTHROPIC_BASE_URL would attach the credential to an endpoint the
// snippet never chose: in inference_auth="sandbox" the token comes from
// the sandbox's baked creator-identity cfg, so an agent whose env_vars
// name any other base URL would have that owner-level PAT forwarded to it
// — by this provider, on every turn, without the payload ever having to
// carry a credential. Bring-your-own-endpoint therefore means
// bring-your-own-token: supply ANTHROPIC_AUTH_TOKEN alongside
// ANTHROPIC_BASE_URL and both are left untouched.
//
// Every SandboxAuthSnippet invariant applies here verbatim, and for the same
// reasons — this text is sourced by BOTH launch.sh (under `set -eu`) and
// install.BuildVerifyCommand's handshake (under `set -eu` AND `set -a`):
//
//   - Never overrides an owner-supplied value: every assignment is inside a
//     `[ -z "${VAR:-}" ]` guard, and env_vars are rendered before this.
//     Emptiness is the right test rather than set-ness — an exported-but-
//     empty ANTHROPIC_BASE_URL is a misconfiguration we should repair, not
//     a value to preserve.
//   - Never returns non-zero: control flow is if/fi and case/esac only (no
//     top-level `&&` chain standing in for an if), and the last line is a
//     bare `:` so the `.` builtin always reports 0 even when this is the
//     final content of the file.
//   - No scratch-variable leak: every temporary is buzz_-prefixed and
//     explicitly unset before the final `:`. Without that, `set -a` in the
//     verify path would export them into the agent's own environment.
//
// The host is normalized because it is taken verbatim from wherever it came
// from: SandboxAuthSnippet reads ~/.databrickscfg without scheme handling,
// and an owner may equally write "dbc-x.cloud.databricks.com" or a value
// with a trailing slash. Under `set -u` a bare $DATABRICKS_HOST would also
// be a hard exit, hence ${DATABRICKS_HOST:-} throughout.
//
// Deliberately NOT set here: ANTHROPIC_MODEL. A valid gateway model id is
// rewritten by the SDK into a canonical Anthropic id the gateway does not
// serve, hard-failing every turn with model_not_found — verified live, and
// inconsistently across model families (sonnet survived the rewrite, haiku
// did not). The adapter's own default works, so no model variable is
// emitted for this runtime at all. See RenderEnv.
const ClaudeEnvSnippet = `# Claude Code inference via the workspace AI Gateway. Appended last so it
# sees DATABRICKS_HOST/DATABRICKS_TOKEN from either source: agent env_vars
# (inference_auth="env") or SandboxAuthSnippet (inference_auth="sandbox").
# The token is derived ONLY when a base URL is known, so a missing host can
# never send a workspace credential to the public Anthropic API.
buzz_derived_url=
if [ -z "${ANTHROPIC_BASE_URL:-}" ] && [ -n "${DATABRICKS_HOST:-}" ]; then
  buzz_h="${DATABRICKS_HOST:-}"
  case "$buzz_h" in
    http://*|https://*) ;;
    *) buzz_h="https://$buzz_h" ;;
  esac
  export ANTHROPIC_BASE_URL="${buzz_h%/}/ai-gateway/anthropic"
  buzz_derived_url=1
fi
if [ -n "$buzz_derived_url" ] && [ -z "${ANTHROPIC_AUTH_TOKEN:-}" ] && [ -n "${DATABRICKS_TOKEN:-}" ]; then
  export ANTHROPIC_AUTH_TOKEN="$DATABRICKS_TOKEN"
fi
unset buzz_h buzz_derived_url
:
`
