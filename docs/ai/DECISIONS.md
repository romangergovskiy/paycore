# Automation / policy decisions (P-xx)

Not a second ADR book. Product decisions remain [../architecture/decisions.md](../architecture/decisions.md) (D-xx). P-xx apply to how agents work on this repo.

## P-01 — Merge classes (GitHub auto-merge OFF)

Three classes. **Ambiguous = financial.**

| Class | What | Merge |
|---|---|---|
| **Docs + safe config** | `AGENTS.md`, `docs/ai/**`, `.cursor/**` (except secrets), README pointers, labeler workflow | Conditions for a **future** auto-merge: required `check` green + independent review (or user waiver for a trivial docs fix). **Not enabled:** GitHub `allow_auto_merge` stays **false** because the platform cannot restrict auto-merge by path. |
| **Tests / non-critical tech** | Tests or infra that do **not** match the financial class | Future auto-merge only if: `check` green; independent review with no blocking findings; `retry_count` ≤ 3; not a new product stage blocked by Q-xx. **Not enabled** (same path-restriction gap). |
| **Financial / ledger** | Ledger, balances, fees, limits, holds, payments, transfers, refunds, payouts, simulator top-up, financial Goose migrations, DB role **grants**, `SECURITY DEFINER`, money-adjacent docs/code | **Never auto-merge.** User confirmation required. Extra review in [REVIEW_RULES.md](REVIEW_RULES.md). |

Path hints (non-exhaustive; labeler uses a similar list): `internal/ledger/`, `internal/platform/postgres/ledger*`, `migrations/00003*`, `migrations/00004*`, future `internal/simulator/`, `internal/transfers/`, `internal/payments/`, `internal/refunds/`, `internal/payouts/`, grant/`SECURITY DEFINER` SQL, [../architecture/ledger-model.md](../architecture/ledger-model.md), [../architecture/money-flows.md](../architecture/money-flows.md), [../architecture/financial-invariants-testing.md](../architecture/financial-invariants-testing.md), [../database/concurrency.md](../database/concurrency.md) when the diff is lock/money related.

Workflow [`.github/workflows/pr-class-labels.yml`](../../.github/workflows/pr-class-labels.yml) applies labels `financial` / `docs-class` / `tests-class`. Labels are **advisory**. They do not merge. They do not skip `check`.

Agents must not merge financial PRs. Do not turn on repository auto-merge.

## P-02 — One implementer

`STATE.lock` holds at most one implementer. A second agent must not mutate the same task. Reviewer comments only; does not push; does not approve its own implementer run.

## P-03 — Retry cap

Implementer may push review/CI fixes on the **same** PR at most **3** times. Then `status=blocked`, stop.

## P-04 — Event-driven only (no infinite cron)

Allowed later Automations (dashboard; not created by this repo):

1. **PR opened / ready** → independent reviewer (not the implementer).
2. **CI completed failure** on a PR → implementer fix under P-03.
3. **PR merged** → STATE refresh from `gh` (no next product stage).

Forbidden: scheduled “pick whatever is next”, cron implementer, auto-approve (PR Routing & Approval).

## P-05 — STATE is GitHub truth

[STATE.md](STATE.md) may only quote `git` / `gh`. Done = `MERGED` + `origin/main` contains it + `check` success on **that** SHA.

## P-06 — No Cursor traces in git

Branches `feat/` `fix/` `docs/` only. No `cursor/` prefix, no Cursor trailers. English commits. Authored as the user.

## P-07 — What may proceed without flipping `user_allowed_next` (revised 2026-11-23)

The user authorized agents to implement work **already allowed** by spec, [ROADMAP.md](ROADMAP.md), and open Q-xx **without stopping for every tiny tech task**.

Still **do not silently flip** `user_allowed_next` in STATE.

| May proceed without flipping `user_allowed_next` | Must stop |
|---|---|
| Docs/STATE/automation files; CI/labeler; mechanical refactors with no new product behaviour | Open HTTP (Q-13, Q-30) |
| Remaining B **status HTTP + `audit.audit_logs`** once the user has approved that product slice *or* it is clearly specified and unblocked | Stage **C** and any monetary HTTP until remaining B (including Open as required by ROADMAP) **and** required Qs (Q-12 seeds, Q-13 Open) |
| Tests that do not invent product rules | Inventing Q-xx answers; new product behaviour not in D-xx |

After a **product-stage** merge, keep `user_allowed_next=false` unless the user set it. Docs/STATE maintenance is not a product stage.

## P-08 — Independent review ≠ CI

`gofmt` + `go vet` + `staticcheck` + `go test -race` (check name **`check`**) is mechanical. Not an AI review. Not a self-approve.

## P-09 — Spend and stop

Do not raise spend limits. When the pool/cap hits, stop and leave STATE recoverable.

## P-10 — Do not invent ADRs or Q-xx

Stop and ask. Do not resolve Q-13, Q-30, Q-12, Q-16, or Q-20 in passing.

## P-11 — Branch protection (2026-11-23)

`main` is protected by repository ruleset `protect-main` (id `27877702`): PRs required (`required_approving_review_count` **0** — no fake reviewer), no force-push (`non_fast_forward`), no deleting `main` (`deletion`), required status check context **`check`**, strict vs the latest SHA. Classic “branch protection” API may still 404; the **ruleset** is what GitHub enforces.
