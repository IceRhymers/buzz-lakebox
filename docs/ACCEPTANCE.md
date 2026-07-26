# Live Acceptance Record — `buzz-backend-databricks-lakebox`

`docs/RUNBOOK.md` §9 is the **procedure**; this file is the **record**.
Every row is `LIVE` / `CI-ONLY` / `NOT RUN` with an evidence pointer a
third party can follow. No row may be blank.

**No secrets.** Only redacted output goes in this file; sandbox ids and
timestamps are fine — never paste raw payloads or env dumps. The check is
value-shaped rather than word-shaped, so that prose *about* credentials
(here and in the runbook) does not trip it while an actual leaked one
does:

```sh
grep -nEi 'nsec1[023456789acdefghjklmnpqrstuvwxyz]{20,}|dapi[0-9a-f]{16,}|(token|secret|password|api_?key)[=:][^ ]{8,}' \
  docs/ACCEPTANCE.md docs/RUNBOOK.md    # must find nothing
```

---

## §6 M1 — Deploy

Cross-checked against `docs/RUNBOOK.md` §9's Deploy block (items 1–4).

### 1(a) — non-member throwaway key

RUNBOOK §9 item 1 unpacks into five observable sub-clauses. None have
been run live.

| Check | Status | Evidence | Date | Sandbox id |
|---|---|---|---|---|
| Deploy fails with `verify.relay_denied` | NOT RUN | none found | — | — |
| Sandbox named `buzz-<npub12>-<slug>` exists | NOT RUN | none found | — | — |
| Binaries (incl. `buzz-agent`, ACP-initialize-verified) installed in `$HOME` | NOT RUN | none found | — | — |
| Env file is `0600` | NOT RUN | none found | — | — |
| In-sandbox `databricks current-user me` fails (PAT reset) | NOT RUN | none found | — | — |

### 1(b) — member test key

| Check | Status | Evidence | Date | Sandbox id |
|---|---|---|---|---|
| `{ok:true, agent_id}` in < 180 s, `status` shows `acp_running: true` | LIVE (partial) | README/PLAN "live-verified 2026-07-25"; commit 5abe10b smoke test on sandbox viable-pika-4294 (acp_running=true). 180s bound, agent_pool_ready, and N=10s liveness not recorded. | 2026-07-25 | `viable-pika-4294` |

### Item 2 — idempotent redeploy

| Check | Status | Evidence | Date | Sandbox id |
|---|---|---|---|---|
| Redeploy same agent → same `agent_id`, no second sandbox, one buzz-acp process afterward | NOT RUN post-fix | commit 5abe10b records this mechanism FAILING live pre-fix (every redeploy orphaned the previous sandbox); only smoke-tested after the state-file fix, never re-run against the §6 item-2 criteria. | — | — |

### Item 3 — induced-failure teardown

| Check | Status | Evidence | Date | Sandbox id |
|---|---|---|---|---|
| Kill network mid-deploy on a fresh create → `{ok:false}` naming the sandbox id, sandbox absent from `sandbox list` afterward | NOT RUN | none found | — | — |

---

## §6 M2 — Lifecycle

One row per RUNBOOK §9 Lifecycle line.

| Check | Status | Evidence | Date | Sandbox id |
|---|---|---|---|---|
| `stop <id>` → Stopped, agent dead | NOT RUN | none found | — | — |
| `start <id>` → Running + agent alive ≤ 60 s, no redeploy | NOT RUN | none found | — | — |
| In-sandbox `databricks current-user me` still fails after `start` (PAT stub survived restart) | NOT RUN | none found | — | — |
| `status <id>` emits stable JSON | NOT RUN | none found | — | — |
| `undeploy <id>` → gone from `sandbox list` | NOT RUN | none found | — | — |

---

## §6 M3 — PAT opt-out

| Check | Status | Evidence | Date | Sandbox id |
|---|---|---|---|---|
| Deploy with `keep_workspace_pat: true` → in-sandbox `databricks current-user me` SUCCEEDS as creator | NOT RUN | none found | — | — |

Note: CI proves the two decisions behind this check — the provider
neither writes the PAT stub at deploy nor re-asserts it from
`launch.sh` on any later relaunch. It cannot prove the live outcome,
which depends on the sandbox image's baked PAT actually being present
and working as the workspace creator.

---

## §6 M3 — fresh-machine walkthrough

Per RUNBOOK §8.

| Check | Status | Evidence | Date | Sandbox id |
|---|---|---|---|---|
| Fresh-machine walkthrough (CLI check → install → `doctor` → `info` → deploy → `status`) succeeds end-to-end | NOT RUN | none found | — | — |

---

## Pre-merge undeploy probe

| Check | Status | Evidence | Date | Sandbox id |
|---|---|---|---|---|
| `undeploy` exercises `SandboxStatus` → real SSH → secret-shred command → delete-while-Running → `ForgetSandbox` returning 0 mappings | NOT RUN | none found | — | — |

Scope note: when run, this probe covers only the path above. It does
**not** cover deleting a sandbox with a live buzz-acp inside it, and it
does **not** cover a non-zero mapping removal (i.e. `ForgetSandbox`
actually dropping a tracked entry) — both remain unexercised regardless
of this probe's outcome.

---

## Substitutions

Two places where RUNBOOK §9's procedure narrows PLAN §6 M1, so a future
reader knows a `LIVE` row there means the proxy passed, not the literal
clause:

- PLAN 1(a) says buzz-acp "exits 1"; §9 substitutes "no non-zombie
  `pgrep` match at N=10s" — a detached process's exit code is not
  observable to the operator.
- PLAN §4.4 step 5 symlinks five binaries; §9 checks two (`buzz-acp`,
  `buzz-agent`).

---

## Blocked

The member test key does not exist yet, so the M1 1(b)/item-2 rows and
every M2 Lifecycle row cannot be run until one is minted. Everything
else in this file is runnable now.
