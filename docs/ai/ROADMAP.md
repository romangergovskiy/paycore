# Roadmap A–N (agent index)

GitHub-only agents: use **this file** plus [STATE.md](STATE.md) to pick the next **allowed** task. Product detail is in existing ADRs and schema, not a second copy of the spec.

Do not invent stages. Order is A→N. Do not start C while remaining B (especially Open) is incomplete. Do not close Q-13 / Q-30 here.

Live facts (SHA, open PRs): [STATE.md](STATE.md). Refresh those from GitHub; do not trust a stale “next = A” note from 2026-11-14 audits.

## Status on `main` (see STATE for SHA)

| Stage | Status | Evidence / remaining |
|---|---|---|
| Identity/Auth HTTP, bootstrap ADMIN, identity Admin API | **Done** on `main` | Live `/api/v1/auth/*`, `/api/v1/admin/users*`. `identity-auth.md` header still lags. |
| **A** HTTP idempotency + outbox/inbox | **Done** | [PR #45](https://github.com/romangergovskiy/paycore/pull/45) MERGED. `migrations/00007_*`, `internal/platform/{idempotency,outbox,inbox}`. `cmd/worker` is still **J**. |
| **B-GET** customer read API | **Done** | [PR #46](https://github.com/romangergovskiy/paycore/pull/46) MERGED. `GET /api/v1/accounts`… No POST Open. |
| **B-remainder** | **Not done** | `audit.audit_logs`; Block/Unblock/Close HTTP; durable Open UNIQUE; Admin accounts HTTP. **Open blocked by [Q-13](../architecture/open-questions.md#q-13)** (and [Q-30](../architecture/open-questions.md#q-30) for currency policy). |
| **C** Simulator `SIM_TOPUP` | **Not started** | First monetary HTTP. Needs remaining B (an account to credit) + L4 `DAILY_SIM_TOPUP`. Rapid starts here. |
| **D** Fees + `DAILY_OUTGOING` | Not started | After C. |
| **E** Transfers | Not started | After A–D. Q-16 still open (sync, no cancel). |
| **F** Merchants + API keys | Not started | After B. |
| **G** Checkout / capture / void / expire | Not started | After A, C, D, F. |
| **H** Refunds | Not started | Q-13b still open. |
| **I** Payouts | Not started | |
| **J** `cmd/worker`, webhooks, notify | Not started | Relay contract exists in A; worker is J. |
| **K** Audit writers + reports + D-31 | Not started | Table itself is **B**, not K. |
| **L** Remaining HTTP / OpenAPI | Not started | |
| **M** Frontend | Conditional | ТЗ §6 vs D-20 vs D-38 — user decides. Not an agent decision. |
| **N** E2E / compose / observ. | Not started | |

## Next allowed product task

From STATE: **B-remainder**, only after `user_allowed_next=true`.

1. **Unblocked by Q-13:** min `audit.audit_logs` (append-only, [database-schema.md §11](../database/database-schema.md)) and Block/Unblock/Close HTTP with L1→L3→(Close L5)→L8 including audit + outbox in one Tx ([concurrency.md §5.7](../database/concurrency.md)).
2. **Blocked:** `POST /api/v1/accounts` Open until the user answers Q-13 (number format) and Q-30 (RUB-only confirmation). Do not invent a number format.
3. **Forbidden now:** Stage C and any monetary HTTP.

Docs/STATE work is not a product stage.

## Stage contracts (enough to implement without Context)

Pointers, not a rewrite of [concurrency.md](../database/concurrency.md) or [decisions.md](../architecture/decisions.md).

### A — done

HTTP key + effect + outbox in one Tx ([D-07](../architecture/decisions.md), [D-23](../architecture/decisions.md)). Relay: `seq` order **inside** one `aggregate_id`; no order across aggregates. No Kafka. No `cmd/worker`. Tests used a fictitious non-ledger UNIQUE. **DEAD vs later seq** was implemented conservatively (do not skip DEAD); not a new ADR.

### B — remainder

Customer Accounts HTTP ([D-42](../architecture/decisions.md) ownership). GET is on `main`. Still needed:

- Open: HTTP key (A) **and** durable business UNIQUE (owner + open key). Schema today has no Open-key column; `UNIQUE(account_number)` is not that barrier if the number is regenerated ([Q-13](../architecture/open-questions.md#q-13)).
- Status writes: [concurrency.md §5.7](../database/concurrency.md) + `audit.audit_logs` in the same Tx. Hash chain is [Q-20](../architecture/open-questions.md#q-20) (do not compute).
- Do not invent Open/Statement use cases; they exist in application.

### C — not started (first money)

Only `SIM_TOPUP` credits the customer ([D-15](../architecture/decisions.md)). Order L1→L3→L4 (`DAILY_SIM_TOPUP` counter only)→L5→`post_journal`→L8. `MAX_SINGLE_OPERATION` is an amount check **without** `limit_counters` / L4 ([D-24](../architecture/decisions.md)). Add `pgregory.net/rapid` and `assertLedgerInvariants`. [Q-12](../architecture/open-questions.md#q-12) for seed numbers (tests may use 100 000 ₽/day until answered). **No C without daily L4.**

### D then E–N (short)

| Stage | Need | Spec |
|---|---|---|
| D | Integer fee engine HALF_UP bp; L4 `DAILY_OUTGOING`; reuse C’s limit tables | [D-17](../architecture/decisions.md), [D-27](../architecture/decisions.md), [D-24](../architecture/decisions.md) |
| E | Sync own + P2P transfer; no cancel unless Q-16 overturned | [D-05](../architecture/decisions.md) |
| F | Merchant settlement account, API key hash+prefix | [D-22](../architecture/decisions.md) |
| G | Checkout, HOLD_PLACE, one full capture, void/expire; fee at capture | [D-21](../architecture/decisions.md), [D-36](../architecture/decisions.md) |
| H | Refunds | [D-09](../architecture/decisions.md), [D-10](../architecture/decisions.md), Q-13b |
| I | Sync payouts, SAVEPOINT on 1100 | [D-25](../architecture/decisions.md) |
| J | `cmd/worker` relay + webhooks + notify stub | outbox contract from A |
| K | More audit writers, reports, maker-checker | [D-31](../architecture/decisions.md); table from B |
| L | Admin money/audit HTTP, OpenAPI | [D-39](../architecture/decisions.md) |
| M | Frontend only if user confirms MVP includes it | vs [D-38](../architecture/decisions.md) |
| N | compose, e2e, `go vet` + staticcheck already in CI | [D-38](../architecture/decisions.md) |

Out of path: fraud `REVIEW`, real cards/providers, Kafka/Redis/microservices, rewriting Auth/Accounts/ledger ([D-20](../architecture/decisions.md)).

## Open questions that block work

| Q | Blocks | Agent action |
|---|---|---|
| Q-13a account_number format | Open HTTP | Stop; do not invent |
| Q-30 confirm RUB-only vs spec | Open currency policy | Stop; D-30 exists, confirmation does not |
| Q-12 limit amounts | C/D seed acceptance | Tests may use 100 000 ₽/day; do not freeze in docs as ADR |
| Q-13b refund to CLOSED | H | Default in money-flows: `ACCOUNT_NOT_ACTIVE` until user says otherwise |
| Q-16 cancel transfer | E acceptance vs spec | Sync COMPLETED/FAILED until user says otherwise |
| Q-20 audit hash chain | Hash computation, not `audit_logs` DDL | Columns may exist; do not compute |

Full text: [../architecture/open-questions.md](../architecture/open-questions.md).

## Invariants agents must not revisit

Ledger SoT; double-entry; append-only POSTED; BIGINT minor units; `SECURITY DEFINER` only for financial writes; no app DML on ledger; L1…L8; key+effect in one Tx; transactional outbox; PostgreSQL modular monolith; no Kafka/Redis/microservices for MVP; do not rewrite Auth, Accounts application, or the ledger gateway.
