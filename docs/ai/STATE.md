# STATE — confirmed GitHub / git facts only

Do not fill this file from chat. Re-query `git fetch origin main` and `gh` before editing. After a successful merge, a slice is done only when `origin/main` SHA and CI on that SHA are confirmed here.

## Snapshot

| Field | Value |
|---|---|
| `last_confirmed_main_sha` | `abefad53747d1d4ea5340d0430b342676481a7d6` |
| `last_confirmed_main_at` | `2026-11-20T07:35:16Z` (merge of [PR #46](https://github.com/romangergovskiy/paycore/pull/46)) |
| `current_stage` | `B-remainder` |
| `last_completed_stage` | Stage A ([PR #45](https://github.com/romangergovskiy/paycore/pull/45) MERGED `2026-11-20T06:19:29Z`) + Stage B-GET ([PR #46](https://github.com/romangergovskiy/paycore/pull/46) MERGED `2026-11-20T07:35:16Z`) |
| `open_prs` | [#47](https://github.com/romangergovskiy/paycore/pull/47) `docs/ai-automation` — this docs PR (`gh pr list --state open` 2026-11-23 after open). Before this PR: none. |
| `unfinished_work` | Stage B remainder: `POST /accounts` (Open), Block/Unblock/Close HTTP, Admin accounts HTTP, `audit.audit_logs`. Then Stage C `SIM_TOPUP` (not started). |
| `blockers` | [Q-13](../architecture/open-questions.md#q-13) (account number format) and [Q-30](../architecture/open-questions.md#q-30) (RUB confirmation) block **Open HTTP**. Do not close them here. [Q-12](../architecture/open-questions.md#q-12) blocks Stage C seed amounts. |
| `next_task` | Remaining Stage B after user approval: min `audit.audit_logs` + Block/Unblock/Close HTTP. **Open waits on Q-13.** **Do not start Stage C.** |
| `user_allowed_next` | `false` |
| `status` | `review` (waiting for independent review; do not merge; do not start Stage B/C) |
| `lock` | `branch=docs/ai-automation`, `pr=47`, `retry_count=0`, `started_at=2026-11-23`. Resume this PR for review fixes; do not open a second PR for the same slice. |
| `last_check_date` | `2026-11-23` |
| `ci_on_main` | success — https://github.com/romangergovskiy/paycore/actions/runs/44696066836 (`headSha` = `abefad53747d1d4ea5340d0430b342676481a7d6`) |

## What is on `main` (code facts)

- Identity/Auth HTTP, `migrate bootstrap-admin`, identity Admin API.
- Stage A: `migrations/00007_platform_idempotency_outbox.sql`, `internal/platform/{idempotency,outbox,inbox}` + postgres adapters.
- Stage B-GET: `GET /api/v1/accounts`, `/{id}`, `/{id}/balance`, `/{id}/statement` (Bearer + CUSTOMER). No `POST /api/v1/accounts`.
- Ledger gateway and Accounts **application** Open/status exist; no monetary HTTP.

## How to refresh

```sh
git fetch origin main
git rev-parse origin/main
gh pr list --state open --json number,title,url,headRefName
gh run list --branch main --limit 1 --json conclusion,headSha,url
```

If `origin/main` ≠ `last_confirmed_main_sha`, update this file from those commands. Do not copy Context-store notes.
