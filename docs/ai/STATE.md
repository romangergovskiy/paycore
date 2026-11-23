# STATE — confirmed GitHub / git facts only

Do not fill this file from chat. **Before every task**, reconcile this file against GitHub CLI/API (`git fetch origin main`, `git rev-parse origin/main`, `gh pr list --state open`, checks on that SHA). **GitHub is the source of truth.** Local STATE is not enough to start work. After a successful merge, a slice is done only when `origin/main` SHA and CI on that SHA are confirmed here.

## Snapshot

| Field | Value |
|---|---|
| `last_confirmed_main_sha` | `85e5fe07d93482980e66d4d65a0471efb42904e3` |
| `last_confirmed_main_at` | `2026-11-23T05:51:26Z` (merge of [PR #47](https://github.com/romangergovskiy/paycore/pull/47)) |
| `current_stage` | `B-remainder` |
| `last_completed_stage` | Stage A ([PR #45](https://github.com/romangergovskiy/paycore/pull/45) MERGED `2026-11-20T06:19:29Z`) + Stage B-GET ([PR #46](https://github.com/romangergovskiy/paycore/pull/46) MERGED `2026-11-20T07:35:16Z`). Docs memory files ([PR #47](https://github.com/romangergovskiy/paycore/pull/47) MERGED `2026-11-23T05:51:26Z`, merge commit `85e5fe07d93482980e66d4d65a0471efb42904e3`) — not a product stage. |
| `open_prs` | [#48](https://github.com/romangergovskiy/paycore/pull/48) `docs/ai-state-after-47` OPEN (STATE-only; stale `open_prs: none` vs live GitHub). [#49](https://github.com/romangergovskiy/paycore/pull/49) `docs/ai-automation-loop` OPEN (`docs-class`). This branch `docs/automation-hardening` is intended to supersede both (user closes #48/#49; agents must not close them). |
| `unfinished_work` | Stage B remainder: `POST /accounts` (Open), Block/Unblock/Close HTTP, Admin accounts HTTP, `audit.audit_logs`. Then Stage C `SIM_TOPUP` (**not started**; not allowed). |
| `blockers` | [Q-13](../architecture/open-questions.md#q-13) (account number format) and [Q-30](../architecture/open-questions.md#q-30) (RUB confirmation) block **Open HTTP**. Do not close them here. [Q-12](../architecture/open-questions.md#q-12) blocks Stage C seed amounts. |
| `next_task` | Remaining Stage B after user approval: min `audit.audit_logs` + Block/Unblock/Close HTTP. **Open waits on Q-13.** **Do not start Stage C.** Do not start remaining B from this automation-infra PR. |
| `user_allowed_next` | `false` |
| `status` | `in_progress` (automation-hardening docs/classifier; not a product stage) |
| `lock` | `branch=docs/automation-hardening`, `retry_count=0`, `started_at=2026-11-23`. Resume this branch; do not open a second hardening PR. |
| `last_check_date` | `2026-11-23` |
| `ci_on_main` | success — https://github.com/romangergovskiy/paycore/actions/runs/45120378724 (`headSha` = `85e5fe07d93482980e66d4d65a0471efb42904e3`) |
| `github_protection` | Ruleset `protect-main` id `27877702` active; required context `check`; `required_approving_review_count=0`; `require_code_owner_review=false`; `bypass_actors=[]`; `allow_auto_merge=false`; `delete_branch_on_merge=true`. Classic branch-protection API 404. |
| `automation_status` | Cursor Automations **not observed** (`source=automations` agents: 0, including archived). Bugbot **not observed** (no Bugbot checks/comments on `main`, #48, or #49). Do not claim they fire. |

## What is on `main` (code facts)

- Identity/Auth HTTP, `migrate bootstrap-admin`, identity Admin API.
- Stage A: `migrations/00007_platform_idempotency_outbox.sql`, `internal/platform/{idempotency,outbox,inbox}` + postgres adapters.
- Stage B-GET: `GET /api/v1/accounts`, `/{id}`, `/{id}/balance`, `/{id}/statement` (Bearer + CUSTOMER). No `POST /api/v1/accounts`.
- Agent memory docs from PR #47: `AGENTS.md`, `docs/ai/*`, `.cursor/environment.json`. Classifier workflow **not** on `main` until this PR merges.
- Ledger gateway and Accounts **application** Open/status exist; no monetary HTTP. Stage C not started.

## How to refresh

```sh
git fetch origin main
git rev-parse origin/main
gh pr list --state open --json number,title,url,headRefName,labels
gh run list --branch main --limit 1 --json conclusion,status,headSha,url
gh api repos/romangergovskiy/paycore --jq '{allow_auto_merge,delete_branch_on_merge}'
```

If GitHub disagrees with any row above, **this file is wrong**. Update it from those commands before starting product work. Do not copy Context-store notes. Do not start Stage C. Do not flip `user_allowed_next`.
