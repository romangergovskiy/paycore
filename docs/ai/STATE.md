# STATE — confirmed GitHub / git facts only

Do not fill this file from chat. Re-query `git fetch origin main` and `gh` before editing. After a successful merge, a slice is done only when `origin/main` SHA and CI on that SHA are confirmed here.

## Snapshot

| Field | Value |
|---|---|
| `last_confirmed_main_sha` | `85e5fe07d93482980e66d4d65a0471efb42904e3` |
| `last_confirmed_main_at` | `2026-11-23T05:51:26Z` (merge of [PR #47](https://github.com/romangergovskiy/paycore/pull/47)) |
| `current_stage` | `B-remainder` |
| `last_completed_stage` | Stage A ([PR #45](https://github.com/romangergovskiy/paycore/pull/45) MERGED `2026-11-20T06:19:29Z`) + Stage B-GET ([PR #46](https://github.com/romangergovskiy/paycore/pull/46) MERGED `2026-11-20T07:35:16Z`). Docs memory files ([PR #47](https://github.com/romangergovskiy/paycore/pull/47) MERGED `2026-11-23T05:51:26Z`, merge commit `85e5fe07d93482980e66d4d65a0471efb42904e3`) — not a product stage. |
| `open_prs` | none (`gh pr list --state open` at last check; PR #47 is MERGED, not open) |
| `unfinished_work` | Stage B remainder: `POST /accounts` (Open), Block/Unblock/Close HTTP, Admin accounts HTTP, `audit.audit_logs`. Then Stage C `SIM_TOPUP` (**not started**; not allowed). |
| `blockers` | [Q-13](../architecture/open-questions.md#q-13) (account number format) and [Q-30](../architecture/open-questions.md#q-30) (RUB confirmation) block **Open HTTP**. Do not close them here. [Q-12](../architecture/open-questions.md#q-12) blocks Stage C seed amounts. |
| `next_task` | Remaining Stage B after user approval: min `audit.audit_logs` + Block/Unblock/Close HTTP. **Open waits on Q-13.** **Do not start Stage C.** |
| `user_allowed_next` | `false` |
| `status` | `idle` |
| `lock` | `null` |
| `last_check_date` | `2026-11-23` |
| `ci_on_main` | **not claimed green.** Push-to-`main` run for `85e5fe07d93482980e66d4d65a0471efb42904e3` was `in_progress` at last check: https://github.com/romangergovskiy/paycore/actions/runs/45120378724. PR #47 `pull_request` check on head `519b363e3b7fce872b78936495e16e48e9bd88a9` concluded success: https://github.com/romangergovskiy/paycore/actions/runs/45119977884 — that is **not** CI on the merge SHA. |

## What is on `main` (code facts)

- Identity/Auth HTTP, `migrate bootstrap-admin`, identity Admin API.
- Stage A: `migrations/00007_platform_idempotency_outbox.sql`, `internal/platform/{idempotency,outbox,inbox}` + postgres adapters.
- Stage B-GET: `GET /api/v1/accounts`, `/{id}`, `/{id}/balance`, `/{id}/statement` (Bearer + CUSTOMER). No `POST /api/v1/accounts`.
- Agent memory docs from PR #47: `AGENTS.md`, `docs/ai/*`, `.cursor/environment.json`.
- Ledger gateway and Accounts **application** Open/status exist; no monetary HTTP. Stage C not started.

## How to refresh

```sh
git fetch origin main
git rev-parse origin/main
gh pr list --state open --json number,title,url,headRefName
gh run list --branch main --limit 1 --json conclusion,status,headSha,url
```

If `origin/main` ≠ `last_confirmed_main_sha`, update this file from those commands. Do not copy Context-store notes. Do not start Stage C.
