# Buzz agent-session hosting: current architecture and remote-sandbox integration points

> Lane A of the Databricks-sandbox deep-dive (2026-07-24). Source: code-search index of `block/buzz@main` (commit 3bd3a014).

## 1. Component map

Repo `block/buzz` (Rust monorepo + Tauri desktop). Sources: `README.md`, `ARCHITECTURE.md`.

| Component | Path | Role |
|---|---|---|
| **buzz-relay** | `crates/buzz-relay` | Single source of truth. Nostr relay (Axum WS + REST), NIP-42 auth, Postgres/Redis/MinIO. Does **not** spawn or orchestrate agents. |
| **buzz-acp** | `crates/buzz-acp` | The agent-session host. Standalone binary: `Relay ──WS──> buzz-acp ──stdio (ACP/JSON-RPC)──> agent (goose/codex/claude)`. Key modules: `relay.rs` (WS+REST), `queue.rs` (per-channel queue), `main.rs` (event loop), `pool.rs` (1–32 agent subprocess pool), `acp.rs` (ACP client), `config.rs`, `filter.rs`, `setup_mode.rs`, `observer.rs`. Persists no state (ARCHITECTURE.md §buzz-acp, ~line 644). |
| **Buzz Desktop** | `desktop/src-tauri` | Tauri app. Owns the agent registry, keys, personas/teams, runtime catalog, workspace ("nest") provisioning, and *spawns buzz-acp locally* — see `desktop/src-tauri/src/managed_agents/*` (module list in `managed_agents/mod.rs` lines 1–35). |
| **buzz-cli** | `crates/buzz-cli` | Agent-facing CLI (JSON in/out) the agent uses to act in Buzz. |
| **buzz-agent** | `crates/buzz-agent` | Buzz's own native ACP agent (a runtime option alongside goose/codex/claude). |
| Others | `buzz-sdk`, `buzz-dev-mcp` (shell/file tools), `buzz-persona`, `buzz-workflow`, `buzz-admin`, `git-credential-nostr` / `git-sign-nostr` | Support crates. |

## 2. How an agent session starts today (mention → running process)

Two stages: **desktop starts the harness process**, then **the harness runs sessions**.

**Stage A — Desktop spawns buzz-acp (local process model):**
1. User creates/starts a managed agent → `start_managed_agent` in `desktop/src-tauri/src/commands/agents.rs` (~line 1040). For `BackendKind::Local` it calls `start_local_agent_with_preflight` → `start_managed_agent_process` → `spawn_agent_child` (`desktop/src-tauri/src/managed_agents/runtime.rs` line 1627).
2. `spawn_agent_child` builds a `std::process::Command` for the **`buzz-acp` binary** (`record.acp_command`), sets `current_dir` to `default_agent_workdir()` = `~/.buzz` (`managed_agents/mod.rs` line 88), assembles ~30 env vars (see §4), puts the child in its own process group (Unix) / Job Object (Windows), pipes stdout/stderr to a per-agent log file, and spawns. PID receipts are written for adoption after desktop restarts (`restore.rs`, `runtime_types.rs`).
3. `restore.rs` re-spawns agents with `start_on_app_launch` at boot; `sweep.rs`/`stop.rs` kill orphaned harness trees.

**Stage B — buzz-acp runs sessions (per-channel, in-memory):**
1. buzz-acp connects to the relay via WS with NIP-42 auth using `BUZZ_PRIVATE_KEY`, discovers channels via REST (`GET /api/channels?member=true`), subscribes (`relay.rs`).
2. It spawns N agent subprocesses (`BUZZ_ACP_AGENT_COMMAND`, default `goose acp`; codex/claude via npm ACP adapters), sends ACP `initialize` (`crates/buzz-acp/README.md` "How It Works").
3. A **kind:9 message @mentioning the agent's pubkey (`p` tag)** passes the inbound author gate (`--respond-to owner-only|allowlist|anyone|nobody`) and queues per channel (`queue.rs`). At most one prompt in flight per channel.
4. The event loop claims an idle agent from the pool (`AgentPool::try_claim`, `pool.rs` ~line 552; prefers an agent that already holds a session for that channel), creates/reuses an **ACP session** via `session/new` with an absolute `cwd` and MCP server config (`acp.rs` lines 558–599 `session_new_full(cwd, mcp_servers, system_prompt)`), and sends the batched prompt via `session/prompt`.
5. The agent replies by shelling out to `buzz-cli` (`send_message`, etc.). Runtime/provider/model config comes from the env the desktop injected (see §4); model switching happens via ACP `session/set_model` / `set_config_option` (`pool.rs`).

So today, **the agent process runs on whatever machine runs buzz-acp — for desktop-managed agents that is the owner's local machine**, cwd `~/.buzz`.

## 3. The runtime/backend abstractions (the seams)

There are **two orthogonal abstractions**; only one is about *where* execution happens.

**(a) Runtime = which harness binary (NOT a location abstraction).** `ManagedAgentRecord.runtime: Option<String>` (`managed_agents/types.rs`) is a string ID (`goose`/`claude`/`codex`/`buzz-agent`) resolved against a catalog in `managed_agents/discovery.rs` (`known_acp_runtime`, `runtime_metadata`) with per-runtime `config_bridge/{goose,claude,codex}.rs` and readiness probes (`readiness.rs`). Looked for and did NOT find a `RuntimeKind` enum or any trait abstracting execution location here.

**(b) Backend = where the harness runs. This is the seam.** `BackendKind` in `desktop/src-tauri/src/managed_agents/types.rs` lines 4–13:

```rust
pub enum BackendKind {
    #[default] Local,
    Provider { id: String, config: serde_json::Value },
}
```

- **Provider binaries**: executables named `buzz-backend-<id>` discovered on PATH (`managed_agents/backend.rs`: `discover_provider_candidates` line 412, `resolve_provider_binary` ~line 476). Protocol: **one-shot JSON over stdin → JSON over stdout** (`invoke_provider`, line 19; 600 s timeout for deploy; secret redaction built in).
- **Ops implemented today**: `"op": "deploy"` (`backend.rs` line 367, `provider_deploy` returns a provider-assigned `agent_id` persisted as `record.backend_agent_id`) and `"op": "info"` (`commands/agent_providers.rs` line 38). Deploy is idempotent update-in-place; **there is no `undeploy`/`start`/`stop`/`status`/`logs` op — explicitly "deferred to v2"** (comment in `commands/agents.rs` ~line 455).
- Wiring: create-with-spawn (`commands/agents.rs` ~line 983, Phase 5), `start_managed_agent` (deploys instead of spawning, ~line 1105/1125), `stop_managed_agent` **rejects** provider agents — "remote agents are stopped via !shutdown message, not this command" (~line 1230), and `delete_managed_agent` requires `force_remote_delete: true` for deployed remote agents.
- Payload: `build_deploy_payload` / `deploy_payload_json` in `desktop/src-tauri/src/commands/agents_deploy.rs` (lines 60, 113). It ships everything a remote harness needs: `name`, resolved `relay_url`, **`private_key_nsec`**, `auth_tag`, `agent_command`, `agent_args`, `system_prompt`, `model`, `provider`, timeouts, `parallelism`, `respond_to(+allowlist)`, merged `env_vars` (global < persona < agent).
- `validate_provider_config` (`backend.rs`) forbids secret-looking keys in `provider_config` (secrets go in `env_vars` instead), max 20 scalar fields.

**A "Databricks sandbox" backend plugs in here**: ship a `buzz-backend-databricks` executable that accepts `{op: "deploy", agent: {...}, provider_config: {...}}`, provisions an ephemeral sandbox, runs `buzz-acp` inside it with the payload projected into the env vars of §4, and returns `{ok: true, agent_id}`. No desktop code changes are needed to appear in the UI — providers are PATH-discovered (`list_agent_providers` in `commands/agent_providers.rs`). What you *would* need to add: v2 lifecycle ops (status/stop/logs/undeploy), since today the only stop channel is the relay-side `!shutdown` owner mention and the only health signal is buzz-acp's own relay observer frames.

Evidence a real remote provider already exists out-of-tree: README.md line 124 — the internal Block build "comes pre-wired to the Block relay and agent provider" (`squareup/buzz-releases`, not in this repo).

## 4. Credentials, env delivery, workspace

**Env assembly** (all in `spawn_agent_child`, `runtime.rs` ~lines 1710–2010; mirrored for remote by `deploy_payload_json`):
- Identity/secrets: `BUZZ_PRIVATE_KEY` (nsec, line 1720), `BUZZ_AUTH_TAG` (NIP-OA attestation), `BUZZ_RELAY_URL`, plus `NOSTR_PRIVATE_KEY` + `GIT_CONFIG_*` pointing at `git-credential-nostr` so agents can clone/push relay-hosted repos via NIP-98 (~line 1946).
- Harness knobs: `BUZZ_ACP_AGENT_COMMAND/ARGS/MCP_COMMAND`, `BUZZ_ACP_AGENTS` (parallelism), `BUZZ_ACP_IDLE_TIMEOUT`, `BUZZ_ACP_MAX_TURN_DURATION`, `BUZZ_ACP_SYSTEM_PROMPT`, `BUZZ_ACP_MODEL`, `BUZZ_ACP_RESPOND_TO(_ALLOWLIST)`, `BUZZ_ACP_SETUP_PAYLOAD` (setup-listener mode when not ready), `BUZZ_ACP_TEAM_INSTRUCTIONS`, `BUZZ_ACP_RELAY_OBSERVER`.
- Provider/model: `BUZZ_AGENT_PROVIDER`/`BUZZ_AGENT_MODEL` and runtime-specific vars (`GOOSE_PROVIDER`, …) via `runtime_metadata_env_vars`; internal builds bake `DATABRICKS_HOST` etc. at compile time (`agent_env.rs` — `BUZZ_DESKTOP_BUILD_AGENT_ENV`, base64 `KEY=VALUE` lines; the tests literally use `DATABRICKS_HOST`/`DATABRICKS_MODEL` examples).
- Layering: baked build floor < Buzz-set vars < global < persona < per-agent (`env_vars.rs`; `RESERVED_ENV_KEYS` at line ~60 blocks user override of identity/relay/exec-surface keys).
- Key storage: nsec lives in the **OS keyring**, blanked in `managed-agents.json` (`types.rs` doc on `private_key_nsec`; `storage.rs`). Deploy fails closed if the key is unavailable (`spawn_key_refusal`).

**Workspace**: cwd is the "nest" `~/.buzz` (`default_agent_workdir`, `managed_agents/mod.rs:88`; provisioning in `nest.rs`). `~/.buzz/REPOS` is either a real dir or a symlink to a user-configured `repos_dir` (`managed_agents/repos.rs`; persisted in `~/.buzz/.repos-dir`). buzz-acp injects a `[Workspace]` section into the system prompt naming the cwd, `OUTBOX/`, and `{cwd}/REPOS/` (`pool.rs` `workspace_section`, ~lines 1160–1175). A sandbox backend must recreate this layout (nest + REPOS + persona/team dirs + runtime config bridges) inside the sandbox.

## 5. Session lifecycle / remote-execution prior art

- Sessions are **per-channel ACP sessions cached in memory on pool agents** (`OwnedAgent.state.sessions`, `pool.rs`); affinity claim reuses them; `invalidate_channel_sessions` / owner `!rotate` force a fresh session; `!cancel` cancels a turn; `max_turns_per_session` rotates proactively; idle/max-turn timeouts cancel turns. **No session persistence** — on restart the harness replays unprocessed @mentions via a `since` filter (buzz-acp README "Recovery"). Crash recovery respawns agent subprocesses.
- Desktop keeps harnesses alive across its own restarts via PID receipts + adoption (`restore.rs`, `runtime.rs` sweep), auto-restart on config drift (`spawn_hash.rs`), and observability via owner-encrypted relay observer frames (`crates/buzz-acp/src/observer.rs`; surfaced in `desktop/src/features/channels/ui/AgentSessionThreadPanel.tsx`).
- **Searched for sandbox/container/docker/remote/ephemeral/environment: nothing for agent execution.** Docker appears only for relay dev infra (docker-compose Postgres/Redis/MinIO, relay image CI). The only "remote" notions are: `BackendKind::Provider` (§3), the `!shutdown` convention for stopping remote agents, and `mesh-llm`/"Buzz shared compute" (`relay_mesh.rs`, `docs/buzz-shared-compute-dev.md`) — which is remote **inference**, explicitly *not* remote compute for the harness ("do not select a remote compute backend as the run", line 119).

## 6. Buzz Desktop's role vs. sandboxed sessions

Desktop owns: agent records + keys (`managed-agents.json` + keyring), personas/teams, the ACP runtime catalog + binary discovery + readiness/login probes, env layering, nest/REPOS provisioning, spawn/restore/stop lifecycle, and observer UI. For `Provider` agents it already steps back to a thin orchestrator: it mints identity, builds the deploy payload, calls the provider binary, records `backend_agent_id`, and stops managing the process (status, restart-on-drift, logs, readiness are all local-only today). Sandboxed sessions would **not bypass** the desktop — they'd ride the existing `BackendKind::Provider` path — but making them first-class needs the provider protocol v2 (status/stop/logs/undeploy) or a relay-side orchestrator, because currently the desktop's only post-deploy control over a remote agent is sending `!shutdown` over the relay, and delete can orphan remote infra (hence the `force_remote_delete` guard).

**Not found (looked for explicitly):** any `RuntimeKind`/execution-location trait; any in-repo `buzz-backend-*` implementation; provider ops beyond `deploy`/`info`; relay-side agent spawning; container/sandbox tooling for agents; ACP session persistence/resume across harness restarts.
