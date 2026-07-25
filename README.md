# buzz-lakebox

A [Buzz](https://github.com/block/buzz) agent-backend provider that runs agent sessions in ephemeral **Databricks Sandboxes** (Lakebox) instead of on the owner's machine.

## How it works

Buzz Desktop discovers `buzz-backend-<id>` executables on `PATH` (`BackendKind::Provider`) and hands them a one-shot JSON payload over stdin. This repo builds **`buzz-backend-databricks-lakebox`**: on `deploy` it provisions a Databricks Sandbox, installs the Buzz harness into the sandbox's persistent `$HOME`, ships the agent's identity/env over SSH stdin, and launches `buzz-acp` as a detached background process. The agent then talks to the Buzz relay outbound-only over WSS — no inbound connectivity to the sandbox is required for sessions.

```
Buzz Desktop ──stdin JSON {op:"deploy",...}──> buzz-backend-databricks-lakebox
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
| stop/status/logs/undeploy | Not in Buzz's provider protocol yet ("v2"). The binary registers matching subcommands but they are **M2 stubs** — operate deployed agents with the raw `databricks sandbox` CLI for now (see [Operating a deployed agent](#operating-a-deployed-agent)) |

Auth: the operator's existing `~/.databrickscfg` profile, selected via `provider_config.profile`. The Databricks Sandbox preview is region-gated (verified in us-west-2).

## Install from source

### Prerequisites

- [Go](https://go.dev/dl/) 1.22 or newer
- `git` and `make`
- The [`databricks` CLI](https://docs.databricks.com/dev-tools/cli/) **1.8.0 or newer** (the version that ships the `sandbox` command group). The provider shells out to it for everything, and resolves it from `PATH` plus the usual install dirs (`/opt/homebrew/bin`, `/usr/local/bin`, `~/.local/bin`, `~/bin`) so a Dock-launched Buzz Desktop finds it too.
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

   This runs `go install` with the version stamped in, placing `buzz-backend-databricks-lakebox` into `$GOBIN` (or `$(go env GOPATH)/bin` if `GOBIN` is unset). To stamp a specific version string, pass `VERSION`:

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

   Note that a GUI-launched Buzz Desktop (Dock/Finder) does not see your shell's `PATH` — it inherits launchd's minimal `PATH` and augments its provider search with only its own app bundle directory and `~/.local/bin` (it never scans `/usr/local/bin`). To cover that case, symlink the binary into `~/.local/bin`:

   ```sh
   make symlink               # links into ~/.local/bin; no-op if the link already exists
   ```

   Pass `SYMLINK_DIR=<dir>` to link somewhere else. No sudo is needed for the default location.

4. **Verify the install**

   ```sh
   buzz-backend-databricks-lakebox version   # prints the stamped version
   buzz-backend-databricks-lakebox doctor    # checks the runtime environment
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
   buzz-backend-databricks-lakebox --profile fevm-west doctor
   ```

3. **Baked in at install time** — `make install PROFILE=fevm-west` stamps the fallback default (normally `DEFAULT`) into the binary via ldflags. This is the way to point Buzz Desktop at a specific profile when its payload doesn't set one, since no flags reach provider mode.

Whichever way you choose, verify it resolves before deploying:

```sh
buzz-backend-databricks-lakebox doctor            # uses the baked-in default
buzz-backend-databricks-lakebox --profile fevm-west doctor
```

### Register your sandbox SSH key

Deploys run every in-sandbox step over `databricks sandbox ssh`, which needs your machine's sandbox key registered with the **target** workspace:

```sh
databricks sandbox register -p <profile>
```

⚠️ **One key, one workspace.** The sandbox gateway is shared per region and binds a key to a single workspace identity. If the key is already registered elsewhere, registering (and deploying) fails with `this SSH key is already registered to another user`. Free it first on the workspace that owns it:

```sh
databricks sandbox ssh-key list -p <other-profile>
databricks sandbox ssh-key delete <key-hash> -p <other-profile>
databricks sandbox register -p <target-profile>
```

Moving the key breaks sandbox SSH on the previous workspace until you register back. Preflight verifies actual registration (not just the register command's exit code) and fails with this remedy in the message.

## Setting up an agent in Buzz Desktop

With the binary symlinked into `~/.local/bin`, **restart Buzz Desktop** (it snapshots its provider scan at launch), then create the agent:

1. **Create agent** → fill in name and instructions.
2. **AI configuration** → *Customize for this agent*:
   - **Agent harness**: Buzz Agent
   - **LLM provider**: **Databricks v2** from the list — *not* "Custom provider…" (buzz-agent only understands the built-in provider ids, and v2 routes Claude/GPT models through the workspace AI Gateway)
   - **Model**: pick from the discovered list or enter a custom gateway model id (e.g. `databricks-claude-opus-5`)
3. **Environment variables** (Advanced):
   - `DATABRICKS_HOST` — the workspace URL serving the model
   - `DATABRICKS_TOKEN` — **required for sandbox deploys.** buzz-agent's default Databricks auth is a browser OAuth (PKCE) flow, which cannot happen inside a headless sandbox; without a token the agent deploys fine and then fails its first LLM call. Least-privilege option: a service-principal token with CAN QUERY on the gateway endpoints.
4. **Run on** → select `databricks-lakebox`. (The section only appears when at least one `buzz-backend-*` binary is discoverable; if it's missing, re-check the symlink and restart the desktop.)
5. **Create agent** — the deploy takes a couple of minutes on the first run (it downloads and installs the Buzz `.deb` into the sandbox), and is an idempotent update-in-place on redeploys.

Talk to the agent by **@mentioning it in a channel it's a member of** (`respond_to` defaults to owner-only, so mention it as the owner). The desktop's status indicators for remote agents come from relay observer frames, which the rendered env enables (`BUZZ_ACP_RELAY_OBSERVER=true`).

## Operating a deployed agent

The provider's lifecycle subcommands are M2 stubs; until then, operate through the `databricks` CLI (the sandbox id is the `agent_id` returned by deploy, also visible in `databricks sandbox list`):

```sh
databricks sandbox list -p <profile>                     # find the sandbox; AUTOSTOP should read "never"
databricks sandbox ssh <id> -p <profile> -- 'tail -50 $HOME/.buzz-backend/acp.log'   # agent health/log
databricks sandbox stop <id> -p <profile>                # stop compute (agent dies; $HOME persists)
databricks sandbox start <id> -p <profile>               # restart compute…
databricks sandbox ssh <id> -p <profile> -- 'sh $HOME/.buzz-backend/launch.sh'       # …then relaunch the agent (nothing relaunches it automatically)
```

Healthy log lines to look for: `agent_pool_ready agents=N`, `connected to relay`, `subscribed to channel …`, `presence set to online`. You can also stop a remote agent from chat with a `!shutdown` owner mention. Deploys default the sandbox to `--no-autostop` (relay traffic doesn't count as sandbox activity, so any idle timeout would kill healthy agents); pass `provider_config.idle_timeout` to opt back in, accepting manual `start`-based recovery.

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

**M1 — working end-to-end** (live-verified 2026-07-25): deploy from Buzz Desktop → Databricks Sandbox provisioned → harness installed → agent online on the relay → owner mention answered via the workspace AI Gateway. Lifecycle ops (`status`/`logs`/`stop`/`start`/`undeploy`) remain M2 stubs; operate via the `databricks sandbox` CLI in the interim.
