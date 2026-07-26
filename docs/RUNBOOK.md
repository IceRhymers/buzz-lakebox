# Operator runbook — `buzz-backend-databricks-lakebox`

For the person who owns a deployed agent when something looks wrong.
Everything here runs on **your** machine against **your** Databricks
profile; the provider binary is the only tool you need beyond the stock
`databricks` CLI.

`<id>` below is a sandbox id (e.g. `cherished-piglet-2474`). Every
subcommand resolves the sandbox for you when the profile has exactly one
— pass the id explicitly when it has several.

---

## 0. The one thing to internalize

**Buzz Desktop's "DEPLOYED" badge is a deployment receipt, not a health
signal.** It means "a deploy succeeded at some point", and it keeps
saying that after the sandbox has stopped, the agent has crashed, or the
storage has been wiped. The desktop also cannot invoke any lifecycle op —
Buzz's provider protocol has only `deploy` and `info` today (v2 ops are
deferred upstream; see `docs/UPSTREAM_BUZZ_GAPS.md` §1, §2, §5).

So: when an agent stops answering mentions, the desktop will not tell you
and will not fix it. **`status` is the source of truth, `start` is the
fix.**

```
buzz-backend-databricks-lakebox status        # sandbox state + agent liveness + log tail
buzz-backend-databricks-lakebox start         # bring a dead agent back
```

---

## 1. Triage: "my agent stopped answering"

```
buzz-backend-databricks-lakebox status
```

The JSON tells you which case you are in:

| `sandbox_status` | `acp_running` | What happened | Fix |
|---|---|---|---|
| `Running` | `true` | The agent is alive. The problem is elsewhere: relay membership, mention routing, or a mention sent while it was down (see §2). | `logs` |
| `Running` | `false` | The agent process died; the sandbox is fine. | `start` |
| `Stopped` | `false` | The sandbox stopped: manual `stop`, an opted-in idle timeout, or the platform's lifetime cap. Nothing inside relaunches the agent — there are no boot hooks (`docs/M05_PROBE_RESULTS.md` §5). | `start` |
| *(error `sandbox.status`)* | — | The sandbox no longer exists (deleted, or Beta storage loss). | §4 |

`status` exits non-zero when the agent is not running, so it composes in
scripts.

**Why did it stop?** Deploys set `--no-autostop` by default precisely
because relay WSS traffic does **not** count as sandbox activity: an idle
timeout kills a perfectly healthy, connected agent ~11–12 minutes after
your last SSH (`docs/M05_PROBE_RESULTS.md` §4). If you opted back in with
`provider_config.idle_timeout`, this is the expected cost, and `start` is
the accepted recovery.

---

## 2. Mentions sent while the agent was down are lost

buzz-acp subscribes to the relay from a **startup watermark** (`since =
start − 5s`), so mentions posted during an outage are never replayed —
the agent simply appears to ignore them. This is an upstream gap
(`docs/UPSTREAM_BUZZ_GAPS.md` §3), not something the provider can fix.

After any recovery, **re-send anything you sent while it was down.**

---

## 3. `!shutdown` and the stop channel

The desktop's only post-deploy control over a remote agent is the
relay-side `!shutdown` owner mention. Know what each one leaves behind:

| Action | buzz-acp | Sandbox | Billing | Recover with |
|---|---|---|---|---|
| `!shutdown` mention | exits | **keeps running** | yes | `start` |
| `stop` | dies with the sandbox | Stopped | no | `start` |
| `undeploy` | gone | **deleted** | no | redeploy from the desktop |

An `!shutdown` alone therefore stops the agent but not the bill. Follow it
with `stop` if you want the compute to go away too.

---

## 4. Sandbox gone: Beta storage loss / lifetime cap / manual delete

Lakebox is Beta: storage is explicitly not durable, and sandboxes can be
deleted out of band. Symptoms: `status` fails with `sandbox.status`, or
`databricks sandbox list` no longer shows the id.

1. Confirm: `databricks sandbox list -p <profile>`.
2. **Redeploy from Buzz Desktop.** The provider's state file still maps
   this agent to the dead id; the deploy's status probe rejects it and
   falls through to creating a fresh sandbox, then rewrites the mapping.
   Nothing manual is required.
3. The agent's `$HOME` is gone with it: any repos it had cloned and
   anything in `OUTBOX/` are not recoverable.

---

## 5. Orphaned / duplicate sandboxes

Reuse across redeploys is keyed by the provider's own state file
(`~/.local/state/buzz-lakebox/agents.json`), because the desktop never
echoes `backend_agent_id` back and Lakebox does not persist caller-set
sandbox names (`docs/UPSTREAM_BUZZ_GAPS.md` §4).

**If that file is lost**, the next deploy creates a *new* sandbox and the
old one keeps running with `--no-autostop` — billing forever, and
possibly double-responding on the relay.

Audit and clean up:

```
databricks sandbox list -p <profile>
cat ~/.local/state/buzz-lakebox/agents.json      # what the provider still tracks
buzz-backend-databricks-lakebox undeploy <id>    # for the stale one
```

An `identity.ambiguous` deploy error is the other face of this: two
sandboxes match one agent's name prefix. The provider refuses to guess —
running two harnesses on one key means both answer every mention. Delete
the stale one, then redeploy.

---

## 6. Retiring an agent

```
buzz-backend-databricks-lakebox undeploy <id>
```

Shreds the in-sandbox secret files (the env file holding the agent's
nsec, auth tag, and inference token), deletes the sandbox, then drops the
reuse mapping. It asks you to type the sandbox id back; `--yes` skips the
prompt and is **required** when stdin is not a terminal.

Notes:

- A stopped sandbox cannot be SSH'd into, so the shred is skipped and the
  secrets are destroyed with the sandbox's storage instead. The command
  says so on stderr.
- A failed shred never aborts the delete: a surviving sandbox is strictly
  worse (still billing, still holding the nsec).
- Also delete or rotate the agent in Buzz Desktop. Rotation is the remedy
  for any suspicion the key leaked — the nsec lived in Databricks-managed
  Beta infrastructure.

---

## 7. Reading a failure: error codes

Every `{"ok": false}` and every CLI error is shaped:

```
[<code>] <what went wrong> — remedy: <what to do> (sandbox <id>, databricks cli <version>)
```

Match on the code, not the prose.

| Code | Remedy |
|---|---|
| `validation` | fix the deploy payload field named above and redeploy |
| `preflight.cli_version_unknown` | install the Databricks CLI and make sure it is on PATH (`databricks version` must work); run `buzz-backend-databricks-lakebox doctor` |
| `preflight.cli_too_old` | upgrade the Databricks CLI, then rerun `buzz-backend-databricks-lakebox doctor` |
| `preflight.profile` | check ~/.databrickscfg for the profile and re-authenticate (`databricks auth login -p <profile>`) |
| `preflight.sandbox_register` | confirm the workspace is in a Lakebox-enabled region (us-west-2 verified) and that your profile may use sandboxes |
| `identity.derive` | the agent's private_key_nsec is not a valid bech32 nsec — re-mint the agent's key in Buzz Desktop and redeploy |
| `identity.ambiguous` | delete the stale sandbox(es) listed above with `databricks sandbox delete <id>`, then redeploy |
| `state.read` | inspect (or delete) ~/.local/state/buzz-lakebox/agents.json — a corrupt mapping file blocks reuse; deleting it costs only orphan detection, which `databricks sandbox list` can replace |
| `state.write` | make ~/.local/state/buzz-lakebox writable — without a persisted mapping every redeploy orphans a still-billing sandbox |
| `sandbox.list` | verify sandbox access with `databricks sandbox list -p <profile>` |
| `sandbox.create` | check the workspace's sandbox quota and region gating with `databricks sandbox list -p <profile>` |
| `sandbox.start` | retry; if the sandbox is mid-transition (Stopping), wait for it to settle and redeploy |
| `sandbox.wait_running` | check the sandbox with `databricks sandbox status <id>` — it may be stuck mid-transition; retry once it settles |
| `sandbox.status` | confirm the sandbox still exists with `databricks sandbox list -p <profile>`; if it was deleted, redeploy to create a new one |
| `sandbox.stop` | retry, or stop it directly with `databricks sandbox stop <id>` |
| `sandbox.delete` | delete it manually with `databricks sandbox delete <id> --auto-approve` — an undeleted sandbox keeps billing |
| `provision.pat_reset` | retry; if it persists the sandbox may be unreachable over SSH — check `databricks sandbox ssh <id> -- true` |
| `install.script` | pass a known `provider_config.buzz_version` (the error lists the pinned versions this build ships sha256s for) |
| `install.write` | check sandbox SSH reachability with `databricks sandbox ssh <id> -- true` |
| `install.exec` | read the install output above: a sha256 mismatch means the pinned release changed; a fetch failure means the sandbox lost egress to GitHub |
| `install.runtime_verify` | the installed buzz-agent could not complete an ACP initialize handshake — check `logs` and the inference env (BUZZ_AGENT_PROVIDER / DATABRICKS_HOST / DATABRICKS_TOKEN) |
| `provision.env_write` | check sandbox SSH reachability and that $HOME is writable in the sandbox |
| `launch.prelaunch_kill` | check sandbox SSH reachability with `databricks sandbox ssh <id> -- true` |
| `launch.write` | check sandbox SSH reachability and that $HOME is writable in the sandbox |
| `launch.exec` | run `logs <sandbox-id>` for the agent's own output, then `start <sandbox-id>` to retry the launch |
| `verify.unreachable` | the sandbox stopped responding right after launch — run `status <sandbox-id>`, then `start <sandbox-id>` |
| `verify.unparseable` | run `status <sandbox-id>` and `logs <sandbox-id>` to see the agent's real state before redeploying |
| `verify.process_dead` | run `logs <sandbox-id>` for the crash output; the acp.log tail is included above |
| `verify.relay_denied` | mint or register a relay-member key for this agent in Buzz Desktop and redeploy — this key is not a member of the target relay |
| `verify.pool_not_ready` | run `logs <sandbox-id>`; the agent started but never reported a ready pool within the verification window |
| `autostop.config` | run `databricks sandbox config <sandbox-id> --no-autostop` manually, or redeploy — the agent itself is healthy |
| `lifecycle.not_deployed` | deploy this sandbox first (from Buzz Desktop, or `deploy --payload-file`) |
| `lifecycle.logs_read` | confirm the sandbox is Running with `status <sandbox-id>` — a stopped sandbox has no readable log |
| `lifecycle.status_probe` | confirm sandbox SSH reachability with `databricks sandbox ssh <id> -- true` |

The table is kept honest by `TestRunbook_DocumentsEveryCode` — a new code
without a row here fails CI.

### What never appears in these outputs

Secrets. The provider scrubs payload-derived values (nsec, auth tag,
every `env_vars` value) from every error, and additionally scrubs
credential-shaped `NAME=value` pairs and bare `nsec1…` tokens out of any
log tail it renders — including `logs` and `status`, which read the
agent's own output. Diagnostics that are *not* secret (relay URL, the
agent's `pubkey=…`) are left intact. If you need a genuinely raw log,
read it in place: `databricks sandbox ssh <id> -- cat '$HOME/.buzz-backend/acp.log'`.

---

## 8. Fresh-machine walkthrough

For a new operator machine, in order:

```
# 1. Databricks CLI present, recent, and authenticated to a Lakebox region
databricks version
databricks auth login -p <profile>

# 2. Install the provider (from a source checkout)
make install PROFILE=<profile>

# 3. Environment check — non-destructive, no deploy
buzz-backend-databricks-lakebox doctor

# 4. Confirm Buzz Desktop can see it
#    (providers are PATH-discovered; the profile picker should list
#     "Databricks Lakebox")
echo '{"op":"info"}' | buzz-backend-databricks-lakebox

# 5. Deploy an agent from Buzz Desktop, then verify from here
buzz-backend-databricks-lakebox status
```

`doctor` is the fast triage for steps 1–3: it checks CLI presence and
minimum version, that the profile resolves, and that the sandbox command
family is reachable, and prints a JSON summary.

---

## 9. Live acceptance checks

CI covers the flow against a fake `databricks` shim; these are the checks
that need real infrastructure. Use a **throwaway nostr key** for anything
that is not the member-key case, and delete the sandbox afterwards.

**Deploy (PLAN §6 M1)**

1. Non-member key → deploy fails with `verify.relay_denied`; before that,
   the sandbox exists, binaries are installed, the env file is `0600`,
   and in-sandbox `databricks current-user me` **fails** (PAT reset).
2. Member key → `{"ok": true, "agent_id": …}` in under 180 s;
   `status` shows `acp_running: true`.
3. Redeploy the same agent → same `agent_id`, no second sandbox in
   `databricks sandbox list`, exactly one buzz-acp process afterwards.
4. Induced mid-deploy failure on a fresh create → `{"ok": false}` naming
   the sandbox id, and the sandbox absent from `sandbox list` afterwards.

**Lifecycle (PLAN §6 M2)**

```
buzz-backend-databricks-lakebox stop <id>       # -> Stopped, agent dead
buzz-backend-databricks-lakebox start <id>      # -> Running + agent alive in <= 60s, no redeploy
databricks sandbox ssh <id> -- databricks current-user me   # must still FAIL (PAT stub survived the restart)
buzz-backend-databricks-lakebox status <id>     # stable JSON
buzz-backend-databricks-lakebox undeploy <id>   # -> gone from sandbox list
```

**PAT opt-out (PLAN §6 M3)**

Deploy with `provider_config.keep_workspace_pat: true`, then:

```
databricks sandbox ssh <id> -- databricks current-user me   # must SUCCEED, as the workspace creator
```

This is the one acceptance CI cannot express — it depends on the image's
baked credential. CI does prove the two decisions behind it: with the
opt-out, the provider neither writes the stub during deploy nor
re-asserts it from `launch.sh` on any later relaunch.
