# Upstream buzz gaps at the provider seam

Raw material for future block/buzz issues/PRs, collected while operating a
real provider-backed agent end-to-end (deploy → chat → stop → recover;
live-verified 2026-07-25/26 against a Databricks Lakebox sandbox, Buzz
Desktop on macOS, `block/buzz` main @ `ab3af828`/`25e7864b`). Each gap
notes the observed failure, the code seam, a proposed fix, and rough size.
Ordered smallest-first — the order we'd contribute them.

Context that strengthens the pitch: the provider seam is deliberately
third-party-shaped (PATH discovery, "no desktop code changes needed to
appear in the UI"), and Block ships an internal remote provider of its
own — gaps 1–3 degrade their product experience too, not just ours.

## 1. "DEPLOYED" status is a deployment receipt, not health

**Observed:** with the sandbox fully stopped and the agent offline on the
relay, the Donkey profile panel showed Status **DEPLOYED** with no hint
anything was wrong.

**Seam:** `desktop/src-tauri/src/managed_agents/runtime.rs` ~1405 — for
provider backends, `status = if record.backend_agent_id.is_some() {
"deployed" }`. Presence of a past deploy result is displayed as current
state.

**Proposed fix:** derive provider-agent status from relay observer frame
recency. The desktop already sets `BUZZ_ACP_RELAY_OBSERVER=true` for
local agents (`runtime.rs:1934`) and this provider now sets it for
remote ones; buzz-acp emits per-turn observer context plus presence.
Show "deployed · online" vs "deployed · unreachable" (frame age >
heartbeat window). No protocol change.

**Size:** small. Pure desktop change.

## 2. No recovery affordance for a provider agent believed deployed

**Observed:** dead remote agent; the profile panel offers chat / edit /
stop only. Stop is explicitly rejected for provider agents
(`commands/agents.rs` ~1230: "remote agents are stopped via !shutdown
message, not this command"). The mention flow won't help either — it
only redeploys when `agent.status !== "deployed"`
(`desktop/src/features/messages/ui/useMentionSendFlow.ts`, the
`isProviderBackedAgent` branch in `ensureManagedAgentMentionsReady`).
Result: the only recovery is provider-side CLI (`buzz-backend-… start`)
— invisible to a desktop user.

**Proposed fix (either or both):**
- Mention flow: when the agent is provider-backed AND observed offline
  (gap 1's signal), re-invoke deploy instead of skipping. Deploy is
  contractually idempotent update-in-place, so this makes recovery
  automatic on the next mention.
- Profile panel: a Start/Redeploy action for provider agents wired to
  the existing `start_managed_agent` deploy path (`commands/agents.rs`
  ~1105 already deploys instead of spawning).

**Size:** small–medium. Desktop only; depends on gap 1 for the offline
signal.

## 3. Mentions sent during an outage are silently lost

**Observed:** buzz-acp's channel subscriptions use a startup watermark —
`startup watermark set to <now>` / `subscribed to channel … (with since
filter)` at `since = startup − 5s`. Mentions posted while the agent was
down are never replayed after recovery; the owner sees the agent simply
ignore them. (Design docs describe "replays unprocessed @mentions via a
`since` filter" — the window is real but anchored to the process's own
start, so it only covers blips *within* a session, not downtime.)

**Seam:** `crates/buzz-acp/src/…` relay subscription setup (the
"startup watermark" / membership-notif `since` logic).

**Proposed fix:** persist a last-processed-event timestamp (or event id
cursor) under `$HOME/.buzz-backend`/`$HOME/.buzz` and subscribe from
`min(persisted cursor, startup − 5s)` with the existing dedup guarding
double-processing. `$HOME` persists across sandbox stop/start, so this
composes with remote providers for free.

**Size:** medium. buzz-acp change; needs care around dedup and replay
bounds (a week-old backlog should probably be capped).

## 4. Deploy payload omits `backend_agent_id`, forcing provider-side state

**Observed:** the desktop persists the provider-returned `agent_id` as
`record.backend_agent_id`, but never echoes it back — `provider_deploy`
(`desktop/src-tauri/src/managed_agents/backend.rs` ~360) sends only
`{op, request_id, agent, provider_config}` and `deploy_payload_json`
(`desktop/src-tauri/src/commands/agents_deploy.rs`) lists every field: no
id. A provider therefore cannot know which backend resource an agent
maps to. Compounded here by Lakebox not persisting caller-set sandbox
names, this forced a provider-side state file
(`~/.local/state/buzz-lakebox/agents.json`) as the only orphan
prevention: without it, every redeploy created a fresh
never-autostopping sandbox.

**Proposed fix:** include `backend_agent_id` (nullable) in the deploy
request envelope. Providers that ignore it lose nothing; providers can
then be stateless and the README's "reuse via backend_agent_id" becomes
true. One field in `provider_deploy` + docs.

**Size:** small. Protocol-additive, backward compatible.

## 5. Provider protocol v2: lifecycle ops

**Observed/known:** the protocol has only `deploy` and `info`; the
desktop's own comments defer `status`/`start`/`stop`/`logs`/`undeploy`
"to v2" (`commands/agents.rs` ~455). Consequences hit above: no health,
no recovery, delete can orphan remote infra (hence the
`force_remote_delete` guard).

**Proposed fix:** add the five ops to the one-shot stdin/stdout JSON
protocol, with desktop plumbing + UI. This provider already implements
the semantics behind four of them as operator CLI subcommands
(`internal/deployflow/lifecycle.go`) and would serve as a working
reference implementation for the PR.

**Size:** large (protocol + desktop + UI). Best proposed as a design
issue first, with gaps 1–2 as its independently-landable slices.

## 6. Codex tool-path network access is described in macOS Seatbelt terms, on a Linux sandbox

**Status: unverified, low confidence, non-blocking.** Raised so it is not rediscovered as a mystery.

`crates/buzz-acp/src/config.rs` (`codex_network_env`, ~:697-749) injects `CODEX_CONFIG={"sandbox_workspace_write":{"network_access":true}}` for codex agents, so the buzz-cli MCP subprocess can reach the relay. Its doc comment describes this as opening "the Seatbelt network sandbox" — Seatbelt is macOS. A Lakebox sandbox is Linux, where codex's `workspaceWrite` policy is enforced by a different mechanism.

Why it matters here: the codex adapter's default `AgentMode` sets `networkAccess: false` (`@agentclientprotocol/codex-acp@1.1.7`, `src/AgentMode.ts` — see `docs/M3_CODEX_PROBE_RESULTS.md` S10), and this provider's own probe observed codex's shell tool unable to resolve DNS while an ordinary shell in the same sandbox had open egress. That probe drove the adapter **directly, with no relay**, so buzz-acp never injected `CODEX_CONFIG` and the observation says nothing about a real deploy — but it does mean the injection is the only thing standing between a deployed codex agent and a tool path with no network.

Two secondary notes on the same function: it returns `None` (injecting nothing, silently) when the relay URL cannot be parsed, and this provider validates `agent.relay_url` only as non-empty; and its equivalence claim to the TOML key `sandbox_workspace_write.network_access` is itself unverified from our side, since a neighbouring key in that same namespace (`sandbox_mode`) was measured to be ignored entirely.

**Ask:** confirm whether `network_access: true` opens the Linux sandbox path as it does the macOS one, or document that it does not. **Provider-side action:** none available — this is upstream's mechanism. Confirm at live acceptance.

## Non-gaps we already absorbed provider-side (no upstream ask)

For contrast, these session findings were fixable entirely in this repo
and are NOT upstream asks: launchd-minimal-PATH resolution of the
`databricks` CLI, `~/.local/bin` symlink placement, sandbox SSH key
registration verification, integer ACP `protocolVersion` in the verify
frame, in-sandbox PATH for spawned workers, `BUZZ_ACP_MCP_COMMAND`
tool wiring (the desktop-parity env contract is on us to mirror), and
the state-file reuse in gap 4's workaround.
