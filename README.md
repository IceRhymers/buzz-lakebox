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

## Install from source

### Prerequisites

- [Go](https://go.dev/dl/) 1.22 or newer
- `git` and `make`
- A configured `~/.databrickscfg` profile with access to the Databricks Sandbox preview (needed at runtime, not build time)

### Steps

1. **Clone the repository**

   ```sh
   git clone https://github.com/IceRhymers/buzz-lakebox.git
   cd buzz-lakebox
   ```

2. **Build and install the binary**

   ```sh
   make install
   ```

   This runs `go install` with the version stamped in, placing `buzz-backend-databricks` into `$GOBIN` (or `$(go env GOPATH)/bin` if `GOBIN` is unset). To stamp a specific version string, pass `VERSION`:

   ```sh
   make install VERSION=v0.1.0
   ```

   To bake in a default Databricks CLI profile other than `DEFAULT`, pass `PROFILE` (see [Choosing a Databricks profile](#choosing-a-databricks-profile)):

   ```sh
   make install PROFILE=fevm-west
   ```

3. **Ensure the install directory is on your `PATH`**

   Buzz Desktop discovers providers by scanning `PATH` for `buzz-backend-<id>` executables, so this step is required — not just convenient:

   ```sh
   export PATH="$(go env GOPATH)/bin:$PATH"
   ```

   Add that line to your shell profile (`~/.zshrc`, `~/.bashrc`, …) to make it permanent.

4. **Verify the install**

   ```sh
   buzz-backend-databricks version   # prints the stamped version
   buzz-backend-databricks doctor    # checks the runtime environment
   ```

To build into the repo root instead of installing (e.g. for local iteration), use `make build`, and run `make check` to execute the same vet + lint + test gauntlet as CI. See `make help` for all targets.

### Choosing a Databricks profile

The provider authenticates with a profile from `~/.databrickscfg` (create one with `databricks auth login -p <name> --host https://<workspace-url>`). There are three ways to select it, from most to least specific:

1. **Per-deploy, in the payload** — `provider_config.profile` in the JSON Buzz Desktop sends on stdin (or in the file given to `deploy --payload-file`). This always wins when set:

   ```json
   {"agent": {...}, "provider_config": {"profile": "fevm-west"}}
   ```

2. **Per-invocation, on the CLI** — the `--profile` flag, honored by all subcommands (`doctor`, `deploy`, ...). Used when the payload leaves the profile empty. Note that Buzz Desktop invokes the provider without arguments, so this only applies to manual CLI use:

   ```sh
   buzz-backend-databricks --profile fevm-west doctor
   ```

3. **Baked in at install time** — `make install PROFILE=fevm-west` stamps the fallback default (normally `DEFAULT`) into the binary via ldflags. This is the way to point Buzz Desktop at a specific profile when its payload doesn't set one, since no flags reach provider mode.

Whichever way you choose, verify it resolves before deploying:

```sh
buzz-backend-databricks doctor            # uses the baked-in default
buzz-backend-databricks --profile fevm-west doctor
```

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
