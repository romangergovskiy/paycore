# Automation loop

Event-driven. Cloud Agents consume plan usage and **stop**. Resume from [STATE.md](STATE.md) + **live GitHub**, not chat. Local STATE is not enough to start work.

Cursor dashboard Automations / Bugbot are **not** created from this repository. Do not claim they fire unless GitHub/Cursor evidence shows them. See remaining user clicks at the end.

## Loop

```text
reconcile STATE vs GitHub (GitHub wins)
  → pick next *allowed* task (ROADMAP + P-07 + Q-xx blockers)
  → lock STATE (one implementer)
  → branch from origin/main (feat/ | docs/ | fix/; never cursor/)
  → implement + make check
  → open PR  →  labeler classifies (financial | docs-class | tests-class)
  → independent reviewer (different run / Bugbot / human) — not the implementer
  → CI job `check` (gofmt, vet, staticcheck, go test -race)
  → fix findings on the same PR (max 3) + re-review + re-check
  → merge per P-01: financial = human only; GitHub auto-merge stays OFF
  → STATE update from GitHub (MERGED + main SHA + check on that SHA)
  → clear lock; STOP. Do not cron the next product stage.
```

**Allowed** (see P-07): not already on `main`; dependencies met; Q-xx does not block **this** slice; no second implementer. Open HTTP blocked by Q-13/Q-30. Remaining Stage B product work and **Stage C** need `user_allowed_next=true`. Docs/STATE/automation-infra is not a product stage.

## Roles

| Actor | Does | Does not |
|---|---|---|
| Implementer | Branch, code, `make check`, PR, fix ≤3 | Review itself; approve itself; merge financial; start B-remainder/C; flip `user_allowed_next`; close Q-xx |
| Independent reviewer | Comment on the diff | Push; self-approve the implementer run; be the same agent/run |
| GitHub Actions `check` | Mechanical CI | AI review |
| Labeler `pr-class-labels` | `financial` / `docs-class` / `tests-class` | Skip tests; merge |
| State updater (dashboard Automation **when** the user creates it) | Re-query `gh`, STATE PR | Start Stage C; flip `user_allowed_next` |
| User | Financial merge, Q-xx, spend, dashboard Automations/Bugbot, GitHub protection clicks | — |

Do not use Cursor “PR Routing & Approval” auto-approve. Do not treat implementer self-review as independent. If no independent reviewer comments, the PR stays unreviewed.

## Recovery after stop

1. Read [../../AGENTS.md](../../AGENTS.md) and [STATE.md](STATE.md).
2. Reconcile with GitHub. If they disagree, GitHub wins.
3. Open PR + CI failed + `retry_count < 3` → same branch. After 3, `blocked`, stop. `merge_ready` → do not push. Stale lock 24h and no PR → clear lock. Slice on `main` → exit. Spend exhausted → `blocked`. Never continue Stage C from an old chat.

## Caps

| Cap | Default |
|---|---|
| Implementer fix pushes | 3 per PR, then stop |
| Concurrent implementers | 1 |
| Auto-start remaining B / Stage C | Never without `user_allowed_next` |
| GitHub auto-merge | **OFF** (`allow_auto_merge=false`; cannot path-restrict) |
| Cron “next stage forever” | Forbidden |
| Spend | Do not raise limits |

## Classifier (fail-safe)

Script [`.github/scripts/classify-pr-paths.sh`](../../.github/scripts/classify-pr-paths.sh), workflow [`.github/workflows/pr-class-labels.yml`](../../.github/workflows/pr-class-labels.yml) (runs on PR opened / synchronize / reopened / ready_for_review **after this file is on `main`**).

- **Ambiguous → financial.** Empty path list → `financial`.
- `docs-class` and `tests-class` are **allowlists**. `tests-class` is only those `*_test.go` files: `internal/platform/apperror/*_test.go`, `internal/platform/config/*_test.go`, `internal/platform/postgres/dsn_test.go`. Production files in those dirs (`apperror.go`, `config.go`) are **`financial`**. Any other `*_test.go` is **`financial`**.
- Ledger, CI, migrations, security/auth, money paths, financial-invariant docs, `.cursor/`, `AGENTS.md`, `docs/ai/`, `.github/` → **not** `docs-class` / `tests-class`.
- Labels are advisory. They do not merge and do not skip `check`.

Tests: `go test ./internal/ci/prclass` (also part of `make check`).

## Merge vs GitHub settings (live)

- Required PR + required **`check`** on `main`: ruleset `protect-main` id **27877702**. Direct push / force-push / deleting `main` blocked. This identity **cannot bypass** (`bypass_actors=[]`, `current_user_can_bypass=never`).
- `delete_branch_on_merge`: on.
- `allow_auto_merge`: **false**. Enabling it would let a human click auto-merge on a **financial** PR too. Labels cannot stop that.
- `required_approving_review_count`: **0**. `require_code_owner_review`: **false**. Classic branch-protection API: 404 (ruleset is the control).
- Actions workflow token: `can_approve_pull_request_reviews=false`.

**With current rights a write-capable agent can still merge a green financial PR** (`gh pr merge` after `check` is green). Labels do not block merge. That is the remaining hole. This repository did **not** PATCH the ruleset.

## GitHub protection — prepared, not applied

GitHub **cannot** require reviews only when the PR has label `financial`. This is a user-owned repo, so the team `required_reviewers` ruleset rule is also unavailable. Do not fake a label-scoped block.

Prepared mechanism that applies to **ALL** PRs (in git, not yet enforced):

1. [`.github/CODEOWNERS`](../../.github/CODEOWNERS) — `* @romangergovskiy` (path-based; every file).
2. **User must click** (Settings → Rules → `protect-main`):
   - Keep `allow_auto_merge` **OFF**.
   - Keep required status check **`check`**, strict vs latest SHA.
   - Keep `bypass_actors` empty (do not weaken `main`).
   - Turn on **Require review from Code Owners** (`require_code_owner_review=true`) **and/or** set `required_approving_review_count` to **1**.
   - Because the agent authenticates as the same GitHub user as the PR author, that user **cannot approve their own PR**. A required-review setting therefore needs a **second GitHub account** to approve, or merges deadlock (this ruleset has no admin bypass). Do **not** add `@romangergovskiy` as a bypass actor: the agent would inherit it.
3. **Agent permissions today:** OAuth scopes `repo` + `workflow`; `permissions.admin=true`. After (2), merge of any green PR (including financial) waits on a different human account. Until then, the agent can still merge.
4. Do not remove the required `check`. Do not enable auto-merge. Do not add bypass actors.
5. Agents must **not** PATCH rulesets or repo merge settings unless the user explicitly authorizes that change in the prompt.

## Independent review process

1. Implementer opens the PR and stops reviewing it.
2. A **different** agent/run, Bugbot (if the user enables it), Security Review, or a human comments on the diff.
3. Implementer may fix ≤3 times on the same branch. Then stop.
4. Financial class: a human merges. Docs/tests: still no GitHub auto-merge.
5. If nobody independent comments, the PR stays **unreviewed**. Self-review does not count.

`.cursor/config/bugbot.yaml` sets `autofix.mode: disabled`. That does **not** mean Bugbot is running. Bugbot reads the file from the **base** branch after merge; it does not review this PR by existing. No Bugbot checks or comments were observed on `main` / #48 / #49. Do not claim Bugbot works without GitHub/Cursor evidence.

## Intended dashboard Automations (not created here)

| Trigger | Who | Prompt gist |
|---|---|---|
| Pull request opened / ready | Reviewer (not implementer) | Read REVIEW_RULES.md; comment; do not push; do not approve if you implemented it. |
| CI completed (failure) on PR | Implementer | Same branch; fix; retry_count; stop at 3. Do not start a new task. |
| Pull request merged | State updater | Refresh STATE from `gh`. Do not start C. Do not flip `user_allowed_next`. |

No schedule trigger. Do not create these from an agent unless the user asks.

## Remaining user clicks (cannot do from this CLI)

1. Install the **Cursor GitHub app** on `romangergovskiy/paycore` if Cloud Agents should comment via ManagePullRequest.
2. [cursor.com/automations](https://cursor.com/automations) — create the three event Automations above; model Grok 4.6; **no cron**. Not created from this repo.
3. Bugbot: enable on this repo if desired; Autofix must stay **disabled**. Do not add Bugbot as a required check until it actually runs.
4. Confirm spend limit in the dashboard; **do not raise** it from an agent.
5. GitHub protection clicks in the section above (CODEOWNERS required review / second reviewer). Not PATCHed here.
6. Close superseded [#48](https://github.com/romangergovskiy/paycore/pull/48) and [#49](https://github.com/romangergovskiy/paycore/pull/49) **yourself** after this PR exists. Agents must not close them.
7. Flip `user_allowed_next` only when you approve a product slice. Confirm Q-13 / Q-30 before Open HTTP; Q-12 before Stage C seeds.

## What this repo already configured (live)

- Ruleset `protect-main`: https://github.com/romangergovskiy/paycore/rules/27877702
- Labels `financial`, `docs-class`, `tests-class`
- `allow_auto_merge=false`, `delete_branch_on_merge=true`
