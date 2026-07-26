---
name: autoplan
description: Drives one GitHub issue through this repo's full development loop — ralplan, sized execution, gates, PR, and a real review loop on the open PR — end to end
argument-hint: "[--yes] [--interactive] <issue-number>"
level: 4
---

<Purpose>
Autoplan takes a buzz-lakebox GitHub issue number and drives it through the
repo's development pipeline without the operator having to manually chain
skills:

```
ralplan ──► GitHub issue (already exists) ──► sized execution (ralph/autopilot/team)
  ──► pre-PR gate + code-review loop ──► PLAN/CONTRACT drift check ──► PR (Refs #NNN)
  ──► post-PR review loop (real PR comments, real amending commits)
```

`/autoplan 5` is shorthand for: read issue #5, ralplan it, execute it at the
right size, run this repo's mandatory gates, open the PR against `main`, then
keep addressing review feedback on that PR until it's clean.
</Purpose>

<Use_When>
- User says `autoplan <N>`, `autoplan issue <N>`, or `/autoplan <N>`
- User wants a GitHub issue taken from plan to a review-ready PR in one pass
- The issue already exists (autoplan does not file issues — that's ralplan's
  job when run standalone; see `Do_Not_Use_When`)
</Use_When>

<Do_Not_Use_When>
- No issue number given, or the issue doesn't exist yet — run `ralplan`
  directly first to produce the issue, then `autoplan <N>`
- User wants only the plan, not execution — run `ralplan` directly and stop at
  the consensus plan
- The issue's remaining work is **live acceptance** against real Databricks
  sandboxes (`docs/RUNBOOK.md` §9). Autoplan cannot deploy a sandbox, mint a
  relay-member key, or observe an `acp.log`. Report what is code-gated vs
  infrastructure-gated and stop.
- User wants a single trivial fix with no ceremony — delegate directly to an
  `executor` agent instead
</Do_Not_Use_When>

<Why_This_Exists>
This repo's real workflow (see the merged PRs on `main`: consensus-approved
ralplan, a whole-file commit split, a Docker toolchain-parity run, `Refs #NNN`
PRs, then review-fix commits) is consistent but manual. Autoplan encodes the
orchestration mechanics — which execution skill to invoke for a given issue's
size, which gates must be green before a PR exists, and how the review loop
posts and resolves real comments — so the loop runs the same way every time.

There is no AGENTS.md/CLAUDE.md in this repo; the mandatory gates live in the
`Makefile` and `.github/workflows/ci.yml`, and the milestone contract lives in
`docs/PLAN.md`. This skill references those directly.
</Why_This_Exists>

<Repo_Facts>
Load-bearing facts this skill depends on (verify if the repo has moved on):
- **Base branch:** single `main`. Every feature PR targets it.
  `mergeCommitAllowed` and `squashMergeAllowed` are both true — see
  `Merge_Discipline` below, this matters.
- **Branch naming:** `feat/<slug>` or `fix/<slug>` (see `feat/buzz-phase-2`,
  `feat/provider-m0-m1`). This skill creates `feat/<N>-<slug>` (or
  `fix/<N>-<slug>` for `bug`-labeled issues).
- **Stack:** Go. `go.mod` and CI both pin **1.22**; local dev is often newer, so
  toolchain parity is a real gate, not a formality (see below). Cobra CLI,
  stdlib-only testing, `goreleaser` for release artifacts.
- **What this binary is:** a Buzz *provider* — a one-shot stdin/stdout JSON
  process (`{"op":"deploy"|"info"}`) that provisions Databricks Lakebox
  sandboxes over the `databricks` CLI and SSH, plus operator lifecycle
  subcommands the desktop cannot yet invoke.
- **Packages (the "affected area" set for sizing):** `internal/deployflow` (the
  deploy + lifecycle flow), `internal/nest` (in-sandbox templates: env file,
  `launch.sh`, PAT stub), `internal/lakebox` (databricks CLI wrapper),
  `internal/redact`, `internal/state`, `internal/payload`, `internal/install`,
  `internal/sshx`, `internal/provider` (wire protocol), `internal/doctor`,
  `internal/identity`, `cmd/buzz-backend-databricks-lakebox`.
- **Mandatory gates** (all must pass with fresh output before a PR):
  - `make check` — `fmt-check` (`gofmt -l .` empty) + `go vet ./...` +
    `golangci-lint run ./...` + `go test ./... -race`
  - **Toolchain parity is part of the gate, not optional.** `golangci-lint` is
    frequently absent locally, and local Go usually differs from CI's 1.22.
    Reproduce CI exactly via Docker before pushing:
    ```
    docker run --rm -v "$PWD":/app -w /app golangci/golangci-lint:v2.11.4 golangci-lint run ./...
    docker run --rm -v "$PWD":/app -w /app -e GOFLAGS=-buildvcs=false golang:1.22 \
      sh -c 'go build ./... && go vet ./... && test -z "$(gofmt -l .)" && go test ./... -race -count=1'
    ```
- **Source of truth:** **`docs/PLAN.md`** — §4 is the deploy-flow contract, §6
  the milestone acceptance criteria (M0–M3), §5 the security posture. Alongside
  it: `docs/CONTRACT.md` (the **frozen** provider wire shape — changing it is a
  breaking change, not a refactor), `docs/RUNBOOK.md` (operator guide; §7 is the
  error-code table, §9 the live-acceptance procedure), `docs/ACCEPTANCE.md` (the
  *record* of what was actually observed live — §9 is the procedure, this is the
  record), and `docs/UPSTREAM_BUZZ_GAPS.md` (deliberately out of scope: upstream
  `block/buzz` asks, not work for this repo).
- **Commit style:** conventional commits with a scope — `fix(nest): ...`,
  `feat(lifecycle): ...`, `docs: ...`, `style(lakebox): ...`. The body leads
  with root cause and cites the mechanism (file:line, or the live observation
  that produced it). Trailer: `Co-authored-by: Isaac`.
</Repo_Facts>

<Merge_Discipline>
Two conventions here are easy to violate and hard to undo:

1. **`Refs #N`, not `Closes #N`, for any live-gated issue.** Several issues in
   this repo can only be closed by observations against real infrastructure
   (`docs/RUNBOOK.md` §9). A PR that ships zero live evidence must not
   auto-close them. Check both the PR body *and* every commit message — commit
   bodies landing on the default branch auto-close too:
   ```
   git log main..HEAD --format=%B | grep -inE '(clos(e|es|ed)|fix(e[sd])?|resolv(e|es|ed)) #'
   ```
   Use `Closes #N` only when the issue's acceptance is fully satisfiable by CI.
2. **Merge with `--merge`, never `--squash`.** Squash is enabled on this repo
   and collapses the commit split. Commit boundaries here are load-bearing: they
   are how a defect's root-cause explanation stays attached to the hunk that
   fixed it (`git log -S '<hunk>'` landing on exactly one commit is an acceptance
   criterion in past plans). Verify shape after merging, against `main`, not the
   branch.
</Merge_Discipline>

<Flags>
- `--yes`: Skip the confirmation checkpoint before pushing the branch and
  opening the PR (step 7). Without it, autoplan always pauses there — pushing
  and opening a PR are visible, hard-to-fully-undo actions.
- `--interactive`: Passed through to ralplan (step 2) for draft-plan review and
  explicit approval before execution, instead of ralplan's default automated
  Planner→Architect→Critic run.
</Flags>

<Steps>
1. **Load the issue**:
   ```
   gh issue view <N> --json number,title,body,labels,state,url
   ```
   - If it doesn't exist or `state != OPEN`, stop and report — do not proceed on
     a closed or missing issue.
   - Check for an existing open PR referencing it
     (`gh pr list --search "<N> in:body" --state open`); if one exists, stop and
     report rather than duplicating work.
   - **Classify the remaining work.** If the issue's open items are live
     acceptance rows (§9 Deploy/Lifecycle/PAT-opt-out), autoplan cannot finish
     it — say which items are code-gated and which need a sandbox or a
     relay-member key, then stop. This repo has issues in exactly that state.
   - Parse the body for the standard sections. If absent, proceed but note the
     gap — ralplan will infer scope from whatever prose is there.

2. **Ralplan the issue**: invoke `Skill("ralplan")` with the issue title, body,
   and labels as the task description (add `--interactive` if passed). Wait for
   Critic `APPROVE` before continuing.
   - If the consensus loop materially changes scope, update the issue body
     (`gh issue edit <N> --body ...`) so the issue stays the source of truth.
   - If the plan changes anything in `docs/PLAN.md` §4/§6, say so explicitly —
     the plan document is the contract the milestones are judged against.

3. **Resolve the base branch** — single trunk, but never guess silently:
   a. Scan the issue body for an explicit target-branch override; verify it
      exists (`git ls-remote --heads origin <branch>`) and use it.
   b. Otherwise `main` (confirm via `gh repo view --json defaultBranchRef`).

4. **Branch**: create `feat/<N>-<slug>` (or `fix/<N>-<slug>` if labeled `bug`)
   off the resolved base. Slug from the issue title, kebab-case, short.
   - **If the working tree already carries uncommitted work**, back it up before
     any history surgery — this repo has been one `git checkout` away from
     losing an uncommitted changeset. The safe recipe (commit first, branch
     second, and never `checkout` across a dirty tree):
     ```
     git add -A && git commit -m "wip: backup"
     git branch <branch>-backup && git push -u origin <branch>-backup
     git reset --mixed main
     ```

5. **Size the execution** off the approved plan's affected-package set and
   acceptance-criteria count — a concrete check, not a judgment call:

   | Signal | Route |
   |---|---|
   | 1 package, ≤5 acceptance criteria, not `risk`-labeled | `ralph` — single-thread persistent loop is enough |
   | 1 package, >5 acceptance criteria, or touches secrets / `launch.sh` / the destructive `undeploy` path | `autopilot` — needs the full expand/plan/QA/validation lifecycle even for one workstream |
   | ≥2 packages (parallelizable workstreams, e.g. `internal/nest` + `internal/deployflow`) | `team` — parallel coordinated agents |

   Invoke the chosen skill (`Skill("ralph")`, `Skill("autopilot")`,
   `Skill("team")`) with the ralplan-approved plan as input — do not let the
   execution skill re-plan when a consensus plan already exists.
   - Whichever route runs must write tests alongside the change and keep step 6
     green.
   - **Hermeticity is not automatic here.** `internal/lakebox`'s
     `DefaultBinPath()` falls back to absolute install dirs (`/usr/local/bin`,
     …) after a PATH miss, so a test that merely empties `PATH` can execute the
     *real* `databricks` CLI — including a live `sandbox delete`. Any test that
     reaches the CLI layer must put a failing `databricks` shim on `PATH`.

6. **Pre-PR checks** (all required before a PR exists):
   - **Gates**, fresh output, not "should pass": `make check`, then the two
     Docker parity runs from `Repo_Facts`. Both, every time — `golangci-lint`
     being absent locally is the normal case, and new files routinely reach a PR
     never having been linted.
   - **Shell-behavior proofs actually ran.** `internal/nest`'s launch-guard
     tests execute the rendered `launch.sh` and can silently `t.Skip` on
     precondition failure, which reads identical to a pass in package-level
     output. Confirm `--- PASS`, not `--- SKIP`:
     ```
     go test ./internal/nest -run TestLaunchScript -v -count=1
     ```
   - **Docs drift:** `docs/PLAN.md` §6 milestone status, `docs/RUNBOOK.md`, and
     `README.md` must not claim more than was verified. A milestone whose
     acceptance is live-only is **"code complete, live acceptance pending"**,
     never "complete", until `docs/ACCEPTANCE.md` records a `LIVE` row. If the
     change alters operator-facing behavior, update `README.md` and
     `docs/RUNBOOK.md` in the same branch, never after. New error codes need a
     `docs/RUNBOOK.md` §7 row — `TestRunbook_DocumentsEveryCode` enforces this
     and will fail the build otherwise.
   - **Code-review loop (max 2 cycles):** a fresh `code-reviewer` agent (add
     `security-reviewer` in parallel if the change touches `internal/redact`,
     `internal/payload`, the env-file/PAT-stub templates, or the `undeploy`
     path) reviews the diff against the plan's acceptance criteria. Must run in
     a **separate context** from whichever agent implemented the change — never
     a self-review. Fix findings, re-review, stop after 2 cycles and report what
     remains.

7. **Checkpoint** (unless `--yes`): summarize branch, base, commits, and gate
   results; confirm before pushing and opening the PR.

8. **Open the PR**:
   ```
   gh pr create --base main --head <feat/N-slug> \
     --title "<scope>: <concise change>" \
     --body "Refs #<N>   # or Closes #<N> only if CI alone can satisfy acceptance

   <what a reviewer must actually judge, first>

   ## Verified locally
   <real make check / Docker parity / -race output>

   ## Not in this PR
   <deferred work, and anything live-gated>"
   ```
   Lead the body with the two or three findings that need human judgement, not
   with a file inventory. State explicitly if the PR does not close the issue it
   references, and why.

9. **Post-PR review loop** — must produce real GitHub artifacts, not just
   in-context findings:
   a. Spawn a fresh reviewer (`code-reviewer`, `+security-reviewer` if it
      touches the secret-handling paths) against `gh pr diff <PR>`.
   b. Post findings as an actual PR review: `gh pr review <PR> --comment
      --body "..."`, or `gh api repos/{owner}/{repo}/pulls/{PR}/comments` for
      line-anchored inline comments.
   c. Fix them as **new commits** in the repo's normal conventional style —
      never a force-push or amend of pushed history.
   d. Push, then reply to / resolve the addressed threads.
   e. Repeat until zero new findings, capped at 2 cycles. If still unclean,
      stop and hand off to a human with what's outstanding.

10. **Report**: PR URL, base branch, execution route and why, gate results
    (including the Docker parity runs and whether the launch proofs PASSed or
    SKIPped), review-loop outcome, and — if the issue is live-gated — exactly
    what remains before it can close.
</Steps>

<Tool_Usage>
- `Skill("ralplan")`, `Skill("ralph")`, `Skill("autopilot")`, `Skill("team")` —
  invoke via the `Skill` tool, not `Task`.
- `Task(subagent_type="code-reviewer", ...)` /
  `Task(subagent_type="security-reviewer", ...)` for both the pre-PR (step 6)
  and post-PR (step 9) passes — always a fresh subagent, never the implementing
  context reviewing itself.
- `gh` CLI for all GitHub state. Never fabricate issue or PR content — read it
  fresh each step.
- `AskUserQuestion` only for genuine ambiguity — not for decisions this skill
  resolves deterministically (base is `main`; sizing is a table lookup).
</Tool_Usage>

<Examples>
<Good>
`autoplan 7`
→ loads issue #7 (state-file concurrency; affected package `internal/state`
only, 4 acceptance criteria, not risk-labeled) → ralplan reaches `APPROVE` →
base `main` → sizes to `ralph` (1 package, ≤5 criteria) → branch
`feat/7-state-flock` → `make check` green, both Docker parity runs green →
checkpoint → PR opened `Refs #7`, base `main` → review loop posts a finding
about lock-file cleanup, fixed in a new commit, thread resolved.
</Good>
<Bad>
`autoplan 5` where #5's checklist is entirely `docs/RUNBOOK.md` §9 live rows.
Why bad: no amount of code work closes it — it needs real sandboxes and a
relay-member key. Report the split and stop instead of opening an empty PR.
</Bad>
<Bad>
Opening the PR on `make check` alone because `golangci-lint` isn't installed.
Why bad: CI pins golangci-lint v2.11.4 and Go 1.22 while local dev is usually
newer; "make check passed" without the parity runs routinely means new files
were never linted at all.
</Bad>
<Bad>
Flipping `docs/PLAN.md` §6 to "✅ COMPLETE" in the PR for a milestone whose
acceptance criteria are all live.
Why bad: it asserts an acceptance nothing points at. It stays "code complete,
live acceptance pending" until `docs/ACCEPTANCE.md` has the `LIVE` row.
</Bad>
</Examples>

<Escalation_And_Stop_Conditions>
- Issue is missing, closed, or already has an open PR referencing it.
- The issue's remaining work is live acceptance — report the code-gated vs
  infrastructure-gated split and stop.
- Any mandatory gate fails and can't be fixed in the current execution route —
  stop and report; never open a PR with red gates.
- A change would alter `docs/CONTRACT.md`'s frozen wire shape — stop and
  surface it as a breaking change for an explicit decision.
- Post-PR review loop still unresolved after 2 cycles — hand off to a human.
- User says "stop", "cancel", or "abort".
</Escalation_And_Stop_Conditions>

<Final_Checklist>
- [ ] Issue loaded fresh, open, not already covered by an open PR, and not
      purely live-gated
- [ ] Ralplan consensus reached (Critic `APPROVE`)
- [ ] Base branch resolved (`main`, or a stated issue-body override)
- [ ] Pre-existing uncommitted work backed up before any history surgery
- [ ] Branch named `feat/<N>-<slug>` / `fix/<N>-<slug>`
- [ ] Execution route chosen per the size table, with the driving signal stated
- [ ] `make check` green (fresh output)
- [ ] Docker parity: golangci-lint v2.11.4 and Go 1.22 both green
- [ ] `internal/nest` launch proofs report PASS, not SKIP
- [ ] `docs/PLAN.md` §6 / `README.md` / `docs/RUNBOOK.md` claim no more than was
      verified; new error codes have a §7 row
- [ ] Pre-PR review loop clean (or reported unclean after 2 cycles)
- [ ] User checkpoint passed before push (unless `--yes`)
- [ ] PR uses `Refs #N` unless CI alone satisfies the issue's acceptance; no
      auto-close keyword in any commit body either
- [ ] Post-PR review loop: real PR comments posted, real fix commits pushed,
      threads resolved
- [ ] Merged with `--merge`, never `--squash`; commit split verified on `main`
- [ ] Final report includes PR URL and every routing decision's rationale
</Final_Checklist>
