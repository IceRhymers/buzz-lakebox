# buzz-lakebox

A [Buzz](https://github.com/block/buzz) agent-backend provider that runs agent sessions in ephemeral **Databricks Sandboxes** (Lakebox) instead of on the owner's machine.

## How it works

Buzz Desktop discovers `buzz-backend-<id>` executables on `PATH` (`BackendKind::Provider`) and hands them a one-shot JSON payload over stdin. This repo builds **`buzz-backend-databricks`**: on `deploy` it provisions a Databricks Sandbox, installs the Buzz harness into the sandbox's persistent `$HOME`, ships the agent's identity/env over SSH stdin, and launches `buzz-acp` as a detached background process. The agent then talks to the Buzz relay outbound-only over WSS — no inbound connectivity to the sandbox is required for sessions.

```
Buzz Desktop ──stdin JSON {op:"deploy",...}──> buzz-backend-databricks
                                                  │  /api/2.0/lakebox/* (create/config)
                                                  │  ssh (install binaries, ship env)
                                                  ▼
                                        Databricks Sandbox (microVM)
                                          └─ buzz-acp ──WSS──> Buzz relay ──> agent runtime
```

## Provider protocol

| Op | Behavior |
|---|---|
| `deploy` | Create (or reuse via `backend_agent_id`) a sandbox → set autostop policy → install pinned Buzz binaries into `$HOME` → write deploy-payload env (0600, via SSH stdin — never argv) → `setsid nohup buzz-acp` → return `{ok: true, agent_id: "<sandbox-id>"}` |
| `info` | Provider name/version/description |
| stop/status/logs/undeploy | Not in Buzz's provider protocol yet ("v2") — exposed as CLI subcommands of this binary in the interim |

Auth: the operator's existing `~/.databrickscfg` profile, selected via `provider_config.profile`. The Databricks Sandbox preview is region-gated (verified in us-west-2).

## Design inputs

Full research (buzz architecture, omnigent's Lakebox integration patterns, live probe evidence with commands and timings) lives in [`docs/`](docs/):

- [`BUZZ_AGENT_SESSION_ARCHITECTURE.md`](docs/BUZZ_AGENT_SESSION_ARCHITECTURE.md) — how buzz hosts agent sessions today and the `BackendKind::Provider` seam
- [`OMNIGENT_DATABRICKS_SANDBOX_PATTERNS.md`](docs/OMNIGENT_DATABRICKS_SANDBOX_PATTERNS.md) — prior art: omnigent's lakebox launcher contract, bootstrap, auth gotchas
- [`LAKEBOX_LIVE_PROBE_RESULTS.md`](docs/LAKEBOX_LIVE_PROBE_RESULTS.md) — live-verified API surface, lifecycle timings, egress, persistence semantics, end-to-end `buzz` CLI ↔ relay proof from inside a sandbox

## Key facts the design leans on (live-verified 2026-07-24)

- Sandbox create → Running in ~1s; restart after stop ~20s; idle autostop default 10m (tunable 1m–24h or `--no-autostop`).
- `$HOME` persists across stop/start; everything else (incl. `/tmp`) is wiped and processes die → binaries live in `$HOME`, relaunch-on-start is the provider's job.
- Outbound egress is open (Buzz relay WSS, GitHub, npm, PyPI, Anthropic). `setsid nohup` processes survive SSH disconnect.
- The public Buzz `.deb` release contains Linux x86_64 builds of `buzz-acp`, `buzz`, `buzz-agent`, `buzz-dev-mcp` — no cross-compile needed.
- The sandbox image bakes a **creator-identity PAT** in `~/.databrickscfg`; the provider must reset it unless the agent is meant to act as the owner on the workspace.

## Status

Design/scaffold phase. No working provider binary yet.
