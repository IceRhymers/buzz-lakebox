package nest

// CodexDefaultModel is the gateway model id written into the generated
// config.toml. It is a fixed constant with NO payload input, deliberately.
//
// The config file is sourced-adjacent state written by a shell heredoc
// inside a file that launch.sh `.`-sources, so any payload-derived value
// here would be the arbitrary-command-execution vector
// payload.Agent.Validate() exists to prevent (payload.go env_vars key
// charset). agent.Model is a *string with no charset validation at all, so
// interpolating it would open exactly that hole. The gain would be nil: the
// gateway serves only "databricks-"-prefixed ids, and codex emits its
// "model metadata not found" warning for every one of them regardless
// (docs/M3_CODEX_PROBE_RESULTS.md S6/G5), so an override changes nothing an
// operator can perceive. If a model knob is ever genuinely needed, add
// provider_config.codex_model WITH a charset allowlist and a render test.
const CodexDefaultModel = "databricks-gpt-5-3-codex"

// CodexEnvSnippet points the Codex ACP adapter at the workspace AI Gateway
// by generating a config.toml under a provider-owned CODEX_HOME. RenderEnv
// appends it verbatim, with no payload interpolation, as the LAST content of
// the env file — after the sorted env_vars block AND after
// SandboxAuthSnippet, for the same ordering reason ClaudeEnvSnippet is.
//
// WHY A FILE AND NOT ENVIRONMENT VARIABLES. Codex takes its endpoint from
// TOML, not from env. There is no ANTHROPIC_BASE_URL equivalent — the
// provider block (base_url, wire_api, env_key) only exists in a config file.
// Everything below follows from that one fact.
//
// WHY CODEX_HOME AND NOT ~/.codex/config.toml. In a Lakebox sandbox
// ~/.codex/config.toml is a SYMLINK to /run/lakebox/codex-config.toml
// (docs/M3_CODEX_PROBE_RESULTS.md S2). Writing it would follow the symlink,
// clobber image-managed state, and be silently discarded on the next
// stop/start when /run is regenerated. docs/PLAN.md §4.4 step 6 said to
// write that path; the probe is why it does not.
//
// WHY CODEX_HOME IS EXPORTED OUTSIDE THE HOST GATE. This is a named
// invariant, not an implementation detail, because the natural-looking
// alternative is a total bypass. If the export lived inside the
// host-derivation branch — the shape symmetry with ClaudeEnvSnippet
// suggests, since everything there lives inside the `if` — then a DECLINED
// gate would leave CODEX_HOME unset, and codex would fall back to
// ~/.codex/config.toml: the image's baked gateway config, already pointed at
// the AI Gateway, with an auth.command that awks the workspace PAT straight
// out of the ~/.databrickscfg that inference_auth="sandbox" deliberately
// leaves intact. The failure path would produce a fully working agent
// running on an owner-level credential, outside every provider gate. The
// export is what prevents that, so it happens whenever this snippet owns
// CODEX_HOME at all.
//
// FAIL-CLOSED COUPLING, and why a stateless gate was not enough.
// ClaudeEnvSnippet gets its fail-closed property free: env vars are
// per-process, so "we did not set it" and "it is not set" are the same
// statement. A FILE breaks that equivalence. This provider reuses sandboxes
// across redeploys (deployflow keyed on npub) and $HOME persists, so:
// deploy #1 derives a host and writes config.toml; a redeploy hits the same
// sandbox with a regenerated ~/.databrickscfg carrying a token line but no
// host line (the exact case ClaudeEnvSnippet's gate exists for);
// SandboxAuthSnippet exports DATABRICKS_TOKEN anyway, because that export is
// NOT gated on the host; this snippet correctly declines to write — and
// deploy #1's file is still on disk, sending the CURRENT token to the
// PREVIOUS host. So the removal below is unconditional, and its success is
// CHECKED rather than assumed: `rm -f` is `|| :`-guarded like every other
// filesystem statement here, which would otherwise swallow a failure and
// leave the stale file in place behind a declined gate. If removal fails,
// CODEX_HOME is redirected to a directory that is clean BY CONSTRUCTION
// (mkdtemp — clean because it did not exist a moment ago, not because
// nothing is believed to write there) and nothing is written.
//
// The invariant is: THE AGENT NEVER RUNS AGAINST A config.toml THIS LAUNCH
// DID NOT WRITE. Re-asserting on every launch mirrors what launch.sh already
// does with the PAT stub, and for the same reason.
//
// WHAT IS DELIBERATELY NOT EMITTED: sandbox_mode and approval_policy. Both
// are INERT under @agentclientprotocol/codex-acp@1.1.7, and not merely
// unobserved-to-work: the adapter applies an AgentMode preset per session
// that supersedes the file (docs/M3_CODEX_PROBE_RESULTS.md S7 measured a
// config.toml sandbox_mode="read-only" permitting writes; S10 read the
// adapter source and found DEFAULT_AGENT_MODE = workspaceWrite /
// networkAccess:false / approvalPolicy:"on-request" applied at thread
// start). Emitting them would place text in a generated file that reads
// like a security control, is reviewed as one, and constrains nothing —
// strictly worse than their absence. RE-VERIFY ON ANY codex_adapter_version
// BUMP: we know the mechanism, but it lives in adapter source we do not
// control.
//
// Every SandboxAuthSnippet invariant applies here verbatim, and for the same
// reasons — this text is sourced by BOTH launch.sh (under `set -eu`) and
// install.BuildVerifyCommand's handshake (under `set -eu` AND `set -a`):
//
//   - Never overrides an owner-supplied value: the entire body is inside a
//     `[ -z "${CODEX_HOME:-}" ]` guard. An owner who brings their own
//     CODEX_HOME brings their own config, and this snippet touches nothing —
//     not the export, not the removal, not the write. Note that under
//     inference_auth="sandbox" the payload layer rejects that combination
//     outright (payload.validateCodexInferenceSource), because there the
//     credential in play is a workspace-owner PAT the payload never handled.
//   - Never returns non-zero: control flow is if/fi and case/esac only, EVERY
//     filesystem statement is `|| :`-guarded (a bare `mkdir -p`, `rm`, or
//     heredoc failing under `set -e` would abort the sourcing shell before
//     the trailing `:` was ever reached — the trailing `:` alone does NOT
//     deliver this invariant), and the last line is a bare `:`.
//   - No scratch-variable leak: every temporary is buzz_-prefixed and
//     explicitly unset before the final `:`. Without that, `set -a` in the
//     verify path would export them into the agent's own environment.
//
// The file is written 0600 under a scoped `umask` inside a subshell: the
// surrounding launch.sh sets `umask 077` only in its PAT-stub branch, so
// without scoping here the file would land 0644 in some paths, and a bare
// `umask 077` at this level would leak into every later file launch.sh
// creates (acp.log, acp.pid, launch.lock).
//
// The host is normalized for the same reasons ClaudeEnvSnippet normalizes
// it: SandboxAuthSnippet reads ~/.databrickscfg without scheme handling, and
// an owner may write a bare hostname or a trailing slash.
const CodexEnvSnippet = `# Codex inference via the workspace AI Gateway. Appended last so it sees
# DATABRICKS_HOST/DATABRICKS_TOKEN from either source: agent env_vars
# (inference_auth="env") or SandboxAuthSnippet (inference_auth="sandbox").
# Codex reads its endpoint from TOML, not env, so this generates a config
# file under a provider-owned CODEX_HOME. The file is written ONLY when a
# base URL was derived here, and any previous one is removed first, so a
# stale config from an earlier deploy can never outlive its host.
if [ -z "${CODEX_HOME:-}" ]; then
  # Exported even when no config is written: without it codex falls back to
  # ~/.codex/config.toml, which in this image is a symlink to the baked
  # gateway config that reads the workspace PAT out of ~/.databrickscfg.
  export CODEX_HOME="$HOME/.buzz-backend/codex"
  mkdir -p "$CODEX_HOME" 2>/dev/null || :
  buzz_codex_cfg="$CODEX_HOME/config.toml"
  rm -f "$buzz_codex_cfg" 2>/dev/null || :
  if [ -e "$buzz_codex_cfg" ]; then
    # Removal failed (non-writable dir, immutable attr, ENOTDIR). Do NOT
    # fall through to the gate: redirect to a directory that is clean by
    # construction and write nothing.
    buzz_codex_alt=$(mktemp -d "$HOME/.buzz-backend/codex.XXXXXX" 2>/dev/null) || buzz_codex_alt=
    if [ -n "$buzz_codex_alt" ]; then
      export CODEX_HOME="$buzz_codex_alt"
    fi
  elif [ -n "${DATABRICKS_HOST:-}" ]; then
    buzz_codex_h="${DATABRICKS_HOST:-}"
    case "$buzz_codex_h" in
      http://*|https://*) ;;
      *) buzz_codex_h="https://$buzz_codex_h" ;;
    esac
    buzz_codex_url="${buzz_codex_h%/}/ai-gateway/codex/v1"
    # Subshell so the umask does not leak into the rest of launch.sh.
    (
      umask 077
      cat > "$buzz_codex_cfg" <<BUZZ_CODEX_CONFIG_EOF
# Generated by buzz-backend-databricks-lakebox at launch.
# Rewritten on every start; edits are lost.
model = "databricks-gpt-5-3-codex"
model_provider = "databricks"

[model_providers.databricks]
name = "Databricks AI Gateway"
base_url = "$buzz_codex_url"
wire_api = "responses"
env_key = "DATABRICKS_TOKEN"
BUZZ_CODEX_CONFIG_EOF
    ) 2>/dev/null || :
    chmod 600 "$buzz_codex_cfg" 2>/dev/null || :
  fi
fi
unset buzz_codex_cfg buzz_codex_alt buzz_codex_h buzz_codex_url
:
`
