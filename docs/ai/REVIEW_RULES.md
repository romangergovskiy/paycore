# Independent review rules

The implementer must not be the only reviewer. A second agent, Bugbot, Security Review, or a human must read the PR. GitHub Actions is required but **is not** an independent AI review.

## What CI is

Workflow [`.github/workflows/ci.yml`](../../.github/workflows/ci.yml): gofmt, `go vet ./...`, `staticcheck@2025.1.1`, `go test -race ./...` on `pull_request` and `push` to `main`.

That catches format, some mistakes, and test failures. It does **not** judge architecture, L1…L8 lock order, financial invariants, or whether the slice matches [ROADMAP.md](ROADMAP.md).

Do not write “reviewed by CI”. Do not claim `make check` is green without running it. After push, cite the GitHub check URL for **this** head SHA.

## Who reviews

| Role | Allowed | Not allowed |
|---|---|---|
| Implementer Cloud Agent | Code, tests, respond to findings on the same PR (retry cap 3) | Approving its own PR; merging financial class |
| Independent AI (Bugbot, Security Review, or a **different** agent/run) | Diff review comments | Same conversation/run as implementer |
| GitHub Actions | Mechanical checks | Called “AI review” |
| Human | Always for financial class merge; architecture; Q-xx | — |

Free fallback if the user does not pay for Bugbot / a second model: CI + human reading the diff. **Do not call that a full independent AI review.**

Bugbot Autofix to the existing branch: keep **off** for financial PRs. A second-model reviewer is optional extra cost (legacy Pro Other Models pool / prepaid credit).

## Checklist (every PR)

1. **Diff vs task** — only the allowed slice. No drive-by rewrites of Auth, Accounts application, or ledger gateway.
2. **Architecture** — matches existing D-xx. No silent ADR. Open Q-xx stay open unless the user closed them.
3. **Security** — authz (JWT.roles is not authorization; live session + role), secrets not logged, no production DSNs, SQL stays parameterized.
4. **Concurrency** — lock order L1…L8 in [../database/concurrency.md](../database/concurrency.md). Do not invent a new order.
5. **SQL / Tx** — HTTP idempotency key + effect + outbox in **one** Tx ([D-07](../architecture/decisions.md), [D-23](../architecture/decisions.md)). App role has no ledger DML ([D-03](../architecture/decisions.md), [D-40](../architecture/decisions.md)).
6. **Errors** — [D-39](../architecture/decisions.md) JSON; no internal/EOF/secret text in client bodies.
7. **Tests** — exist for the behaviour; not 2xx-only; `paycore_app` for PG tests. Do not drop `-race`.
8. **Financial invariants** (money PRs, Stage C+): after each monetary integration/concurrency test, `assertLedgerInvariants` (∑D=∑C, projection vs journal, no negative, no bad holds) per [../architecture/financial-invariants-testing.md](../architecture/financial-invariants-testing.md). Until that helper exists, **refuse monetary HTTP**.

## Extra strict (financial class — P-01)

Ledger, balances, fees, transfers, payments, financial migrations, grants:

- Human merge confirmation. No auto-merge.
- `paycore_app` still cannot INSERT/UPDATE/DELETE journal, entries, balances, buckets, holds.
- No float money. BIGINT minor units only.
- Retry policy unchanged: [concurrency.md §6](../database/concurrency.md).
- Compensations are a new journal, not edits of POSTED rows.

## Findings loop

Reviewer comments on the PR. Implementer fixes on the **same** branch, `retry_count++`. After 3 fix pushes, stop (`blocked`). Do not open a parallel PR for the same slice.
