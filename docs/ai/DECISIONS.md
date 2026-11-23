# Automation / policy decisions (P-xx)

Not a second ADR book. Product decisions remain [../architecture/decisions.md](../architecture/decisions.md) (D-xx). Do not copy D-xx here. P-xx apply to how agents work on this repo.

## P-01 — Merge classes (design; not enabled)

Documented policy for a **later** enablement. This repository does **not** auto-merge today. Agents must not merge unless the user has turned this policy on **and** the PR matches a class below.

| Class | What | Merge |
|---|---|---|
| **Docs + safe config** | `AGENTS.md`, `docs/ai/**`, `.cursor/environment.json`, README pointers to those files | **May** auto-merge later after GitHub Actions `check` is green on the head SHA **and** independent review has no blocking findings (or the user waived review for a trivial docs fix). |
| **Tests / non-critical tech** | Tests or infra that do **not** touch financial paths listed below | Auto-merge later **only if** all of: CI green; independent review recorded with no blocking findings; `lock.retry_count` ≤ 3; diff does not match the financial class; not a new product stage unless `user_allowed_next` is true. |
| **Financial / ledger** | Ledger, balances, fees, transfers, payments, holds capture/void, simulator top-up, `limit_*` as money controls, financial Goose migrations, DB role **grants**, `SECURITY DEFINER` functions, `internal/ledger/**`, `internal/platform/postgres/ledger*`, `migrations/00003*`, `migrations/00004*` | **User confirmation required.** Never auto-merge. Extra review in [REVIEW_RULES.md](REVIEW_RULES.md). |

Path hints for the financial class (non-exhaustive): `internal/ledger/`, `migrations/00003_ledger.sql`, `migrations/00004_ledger_functions.sql`, postgres ledger adapters, future `internal/simulator/`, `internal/transfers/`, `internal/payments/`, grant statements on ledger/accounts money tables.

If unsure, treat as financial and do not auto-merge.

## P-02 — One implementer

`STATE.lock` holds at most one implementer (`agent_id`, `branch`, `pr`, `started_at`, `retry_count`). A second agent must not mutate the same task. Reviewer automations comment only; they do not push.

## P-03 — Retry cap

Implementer may push review/CI fixes on the **same** PR at most **3** times (`STATE.lock.retry_count`). Then `status=blocked`, clear the urge to loop, stop.

## P-04 — No infinite cron

Do not schedule an automation that picks “whatever is next” on a timer. Cloud Agents consume plan usage and stop. Event triggers (PR opened → review; CI failed → fix under P-03; PR merged → STATE refresh) are the intended later Automations. A “keep implementing stages A–N” schedule is forbidden.

## P-05 — STATE is GitHub truth

[STATE.md](STATE.md) may only quote `git` / `gh`. A PR is not done until `state=MERGED` **and** `origin/main` contains it **and** CI on that main SHA is green.

## P-06 — No Cursor traces in git

Branches `feat/`, `fix/`, `docs/` only. No `cursor/` prefix, no Cursor trailers in commits or PR titles. English commit messages. Authored as the user.

## P-07 — Product stages need explicit allow

`user_allowed_next` defaults to `false`. After a product-stage merge, set it back to `false`. Docs/STATE maintenance is not a product stage. Do not start Stage C or Open HTTP while Q-13 / Q-30 remain open.

## P-08 — Independent review ≠ CI

GitHub Actions (`gofmt`, `go vet`, `staticcheck`, `go test -race`) is mechanical. It is **not** a full independent AI review. See [REVIEW_RULES.md](REVIEW_RULES.md).

## P-09 — Spend and stop

Cloud Agents and Automations bill from the Cursor plan (legacy Pro: monthly pools + optional on-demand spend limit). When the pool or spend cap hits, stop and leave STATE recoverable. Do not retry past P-03 to “burn through” a limit.

## P-10 — Do not invent ADRs

If a product choice is not in D-xx or an answered Q-xx, stop and ask the user. Do not resolve Q-13, Q-30, Q-12, Q-16, or Q-20 in passing.
