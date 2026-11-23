# Automation loop

Event-driven. Cloud Agents consume plan usage and **stop**. Resume from [STATE.md](STATE.md) + GitHub, not chat.

Cursor dashboard Automations / Bugbot are **not** created from this repository. Do not claim they fire unless the dashboard shows them. See remaining user clicks at the end.

## Loop

```text
check origin/main + STATE
  → pick next *allowed* task (ROADMAP + P-07 + Q-xx blockers)
  → lock STATE (one implementer)
  → branch from origin/main (feat/ | docs/ | fix/; never cursor/)
  → implement + make check
  → open PR  →  labeler classifies (financial | docs-class | tests-class)
  → independent reviewer (different run / Bugbot / human) — not the implementer
  → CI job `check` (gofmt, vet, staticcheck, go test -race)
  → fix findings on the same PR (max 3) + re-review + re-check
  → merge per P-01: financial = user only; GitHub auto-merge stays OFF
  → STATE update from GitHub (MERGED + main SHA + check on that SHA)
  → clear lock; do not cron the next product stage
```

**Allowed** (see P-07): not already on `main`; dependencies met; Q-xx does not block **this** slice; no second implementer. Open HTTP blocked by Q-13/Q-30. **Stage C not allowed.**

## Roles

| Actor | Does | Does not |
|---|---|---|
| Implementer | Branch, code, `make check`, PR, fix ≤3 | Review itself; approve itself; merge financial; start C |
| Independent reviewer | Comment on the diff | Push; self-approve the implementer run |
| GitHub Actions `check` | Mechanical CI | AI review |
| Labeler `pr-class-labels` | `financial` / `docs-class` / `tests-class` | Skip tests; merge |
| State updater (dashboard Automation **when** the user creates it) | Re-query `gh`, STATE PR | Start Stage C |
| User | Financial merge, Q-xx, spend, dashboard Automations/Bugbot | — |

Do not use Cursor “PR Routing & Approval” auto-approve.

## Recovery after stop

1. Read [../../AGENTS.md](../../AGENTS.md) and [STATE.md](STATE.md).
2. `git fetch origin main && git rev-parse origin/main`.
3. `gh pr list --state open`; if `lock.pr` set, `gh pr view <n>`.
4. Open PR + CI failed + `retry_count < 3` → same branch. `merge_ready` → do not push. Stale lock 24h and no PR → clear lock. Slice on `main` → exit. Spend exhausted → `blocked`. Never continue Stage C from an old chat.

## Caps

| Cap | Default |
|---|---|
| Implementer fix pushes | 3 per PR |
| Concurrent implementers | 1 |
| Auto-start Stage C | Never |
| GitHub auto-merge | **OFF** (`allow_auto_merge=false`; cannot path-restrict) |
| Cron “next stage forever” | Forbidden |
| Spend | Do not raise limits |

## Merge vs GitHub settings

- Required PR + required **`check`** on `main`: ruleset `protect-main`. Direct push / force-push / deleting `main` blocked.
- `delete_branch_on_merge`: on (safe cleanup of merged heads).
- `allow_auto_merge`: **false**. Enabling it would let a human click auto-merge on a **financial** PR too. Labels cannot stop that.

## Intended dashboard Automations (not created here)

| Trigger | Who | Prompt gist |
|---|---|---|
| Pull request opened / ready | Reviewer (not implementer) | Read REVIEW_RULES.md; comment; do not push; do not approve if you implemented it. |
| CI completed (failure) on PR | Implementer | Same branch; fix; retry_count; stop at 3. Do not start a new task. |
| Pull request merged | State updater | Refresh STATE from `gh`. Do not start C. Do not flip `user_allowed_next`. |

No schedule trigger.

## Remaining user clicks (cannot do from this CLI)

1. Install the **Cursor GitHub app** on `romangergovskiy/paycore` (ManagePullRequest and Cloud Agents on GitHub comments need it). GitHub MCP auth was **declined**.
2. [cursor.com/automations](https://cursor.com/automations) — create the three event Automations above; model Grok 4.6; **no cron**.
3. Bugbot: enable on this repo if desired; Autofix must stay **disabled** (repo file `.cursor/config/bugbot.yaml` already sets `autofix.mode: disabled`).
4. Confirm spend limit in the dashboard; **do not raise** it from an agent.
5. Optional: Bugbot check is **not** required on `main` (would block when Bugbot is off).

## What this repo already configured

- Ruleset `protect-main`: https://github.com/romangergovskiy/paycore/rules/27877702
- Labels `financial`, `docs-class`, `tests-class`
- Workflow `pr-class-labels.yml` (does not replace `ci.yml`)
