# Wire contract freeze — provider protocol v1

> Frozen against `block/buzz` @ `3bd3a014c6ed3f8c8c6c6ea359fee9a7e98dd670` (main), read 2026-07-24 via code-search. This satisfies the PLAN §6 M0 precondition. If buzz moves, re-verify against these files before changing provider behavior — tolerate unknown request fields, never remove response fields.

## 1. Invocation model

Source: `desktop/src-tauri/src/managed_agents/backend.rs` — `invoke_provider()` (line 19).

- The desktop spawns the provider binary with **no argv**, writes **one JSON object followed by `\n`** to stdin, and **closes stdin immediately** (provider sees EOF).
- Working directory: the desktop sets cwd to `default_agent_workdir()` when available (backend.rs:30-32) — do not assume cwd is the repo or `$HOME`.
- stdout is read incrementally and capped at **1 MB** (`STDOUT_CAP`, backend.rs:9); stderr capped at **64 KB** (`STDERR_CAP`, backend.rs:6). Response parsing: first stdout line that parses as JSON wins, else the whole trimmed buffer is tried (backend.rs:224-229). A trailing newline is not required but emitting exactly one JSON object on one line is the safe shape.
- **Timeouts**: `info` 10 s (`commands/agent_providers.rs:42`), `deploy` 600 s (backend.rs:372). On timeout the child is killed and the desktop reports `provider timed out after {N}s`.

## 2. Exit-code semantics (critical)

Source: backend.rs:205-217.

- **Non-zero exit fails the invocation regardless of stdout content** — the desktop discards any JSON already emitted and surfaces `provider failed (exit code N). stderr: <first 4096 chars, redacted>`.
- Therefore the provider must **exit 0 in every handled case, including errors**, and communicate failure via `{"ok":false,"error":"..."}` on stdout. Reserve non-zero exit for unhandleable crashes.

## 3. Request shapes

### `info` (agent_providers.rs:37-44)

```json
{"op":"info","request_id":"<uuid-v4>"}
```

`request_id` is for provider-side logging only — never validated in the response (stdin→stdout is 1:1 per process).

### `deploy` (backend.rs:365-371 `provider_deploy`)

```json
{
  "op": "deploy",
  "request_id": "<uuid-v4>",
  "agent": { ... },
  "provider_config": { ... }
}
```

The agent payload is **nested under `"agent"`** — not flat at the top level.

### `agent` fields (exhaustive)

Source: `desktop/src-tauri/src/commands/agents_deploy.rs` — `deploy_payload_json()` (line 112; doc comment: "every field the provider harness receives is deliberately listed here"). This also confirms PLAN §4.4 step 6: **there is no persona/team file dependency** — persona material arrives only as `system_prompt` and merged `env_vars`.

| Field | Type | Notes |
|---|---|---|
| `name` | string | display name; cosmetic only — never identity |
| `relay_url` | string | already resolved to a concrete URL by the desktop |
| `private_key_nsec` | string | SECRET; identity key (npub derivation, PLAN §4.1). Desktop fails closed on keyring outage before serializing an empty one, but validate non-empty anyway |
| `auth_tag` | string | SECRET |
| `agent_command` | string | v0: must be `buzz-agent`, else reject (PLAN §4.2) |
| `agent_args` | array of strings | |
| `system_prompt` | string | |
| `model` | string \| null | resolved persona→record→global |
| `provider` | string \| null | structured provider id (e.g. `databricks_v2`) |
| `turn_timeout_seconds` | number | |
| `idle_timeout_seconds` | number | |
| `max_turn_duration_seconds` | number | |
| `parallelism` | number | |
| `respond_to` | string | falls back to `owner-only` if unread |
| `respond_to_allowlist` | array of strings | |
| `env_vars` | object (string→string) | merged global < persona < agent |

### `provider_config`

Validated by the desktop before it reaches us (`validate_provider_config`, backend.rs:390-421): flat object, ≤20 fields, ≤64 KB, scalar values only, no secret-like key names (`secret|password|token|key|credential` as word segments). Our keys (`profile`, `idle_timeout`, `keep_workspace_pat`, `buzz_version`, `inference_auth`) all pass — `inference_auth`'s own segments (`inference`, `auth`) don't match the filter, which is exactly why the knob is named that and never `*token*`.

- `inference_auth` — string enum `""` \| `"env"` \| `"sandbox"`, default `""` (treated as `"env"`). `"env"`: current behavior — the provider resets the sandbox's baked creator-identity PAT to a stub and the owner supplies `DATABRICKS_HOST`/`DATABRICKS_TOKEN` via `agent.env_vars`. `"sandbox"`: zero-token — the provider skips the PAT reset/re-assert, keeps the baked `~/.databrickscfg`, and derives `DATABRICKS_HOST`/`DATABRICKS_TOKEN` from it at every launch (only-if-unset, so explicit `env_vars` still win). `"sandbox"` supersedes `keep_workspace_pat` when both are set on the same deploy — they're redundant, not conflicting: both keep the baked cfg in place.

## 4. Response shapes

### `info`

Passed through to the frontend **unvalidated** (agent_providers.rs returns `invoke_provider`'s value directly) — but not inert. As of `block/buzz` @ `8bb43d51` (pin this citation; re-verify if buzz moves), Buzz Desktop reads an additive `config_schema` field and renders it as create-agent inputs: `desktop/src/shared/api/types.ts:437` (`BackendProviderProbeResult`) declares the shape; `WhereToRunSection.tsx` → `ProviderConfigFields.tsx` walks `config_schema.properties` and renders one free-text input per property, seeded from `title`/`description`/`default`; `config_schema.required` gates the create-agent button; `coerceConfigValues` type-coerces the entered strings back (e.g. `"true"` → bool) before they're stored on the agent record. Those stored values are resent as `provider_config` on **every** redeploy. Older desktops that predate this field simply ignore it — it's additive and does not change wire compatibility.

Convention (must not set `ok:false`):

```json
{"ok":true,"name":"Databricks Lakebox","version":"<semver>","description":"...","protocol":"v1","ops":["info","deploy"],"config_schema":{"type":"object","properties":{"profile":{"type":"string","title":"Databricks CLI profile","description":"Databricks CLI profile selection; empty = the build's baked default."},"inference_auth":{"type":"string","title":"Inference auth","default":"env","description":"env (default): you supply DATABRICKS_HOST/DATABRICKS_TOKEN in the agent's environment variables. sandbox: zero-token — the agent reuses the sandbox's built-in per-user credential and can act AS YOU across the whole workspace (opt-in security tradeoff)."},"idle_timeout":{"type":"string","title":"Idle timeout","description":"Duration like 30m or 2h; empty = no autostop (default)."}},"required":[]}}
```

We advertise only `profile`, `inference_auth`, and `idle_timeout` in `config_schema.properties`. `keep_workspace_pat` and `buzz_version` stay expert-only — documented here in CONTRACT.md, not schema-advertised — since they're footguns better set deliberately in a hand-written payload than exposed as a free-text field in the desktop UI.

### `deploy` success (backend.rs:373-376)

```json
{"ok":true,"agent_id":"<sandbox-id>"}
```

The **only** field the desktop reads is `agent_id` (must be a JSON string; missing → error `deploy response missing agent_id`). Extra fields are ignored — safe to add diagnostics (e.g. `cli_version`, `sandbox_name`).

### Any-op failure (backend.rs:232-235)

```json
{"ok":false,"error":"<actionable message>"}
```

Exit 0. `error` must be a string; the desktop surfaces it (after its own redaction pass) as the deploy failure. Embed the sandbox id and recorded CLI version here per PLAN §4.3.

### Unknown op

Same failure shape, exit 0: `{"ok":false,"error":"unknown op \"<op>\"; supported: info, deploy"}` — forward-compatible with v2 ops.

## 5. Desktop-side redaction (defense in depth, not a substitute)

backend.rs:189-193, 253-297: the desktop redacts `agent.env_vars` values (≥4 chars, longest-first) plus `nsec1…`/`sprt_tok_…` prefixed tokens out of stderr and `error` strings. It does **not** know about `private_key_nsec`/`auth_tag` beyond the prefix rules, and never sees our in-sandbox logs — the provider's own `internal/redact` must scrub every payload secret from everything it emits (PLAN §5).

## 6. Discovery

backend.rs:427-543: binaries named `buzz-backend-<id>` with the executable bit, discovered on `PATH` + the desktop exe's own dir + `~/.local/bin`; id must match `[a-z0-9][a-z0-9_-]*`. Our id: `databricks-lakebox` → binary `buzz-backend-databricks-lakebox` (buzz ships its own built-in `databricks` provider, so the plain id would collide).

## 7. Engineering constants settled here

- Minimum `databricks` CLI version gate: **v1.8.0** — the version live-verified with the full `sandbox` command group (lane C/D probes). `doctor` and deploy preflight enforce ≥ this; every output records the actual version string (PLAN §3.1).
- buzz-agent inference env (verified live, `docs/M05_PROBE_RESULTS.md` §2): `BUZZ_AGENT_PROVIDER` (`databricks_v2` | `databricks`), `DATABRICKS_HOST`, `DATABRICKS_TOKEN`, `DATABRICKS_MODEL`.
