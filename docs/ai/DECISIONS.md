# Automation / policy decisions (P-xx)

Not a second ADR book. Product decisions remain [../architecture/decisions.md](../architecture/decisions.md) (D-xx). P-xx apply to how agents work on this repo. P-07 must match [../../AGENTS.md](../../AGENTS.md), [AUTOMATION.md](AUTOMATION.md), and [STATE.md](STATE.md).

## P-01 — Merge classes (GitHub auto-merge OFF)

Three classes. **Ambiguous = financial.**

| Class | What | Merge |
|---|---|---|
| **Docs + safe config** | Ordinary docs only (`README.md`, `docs/architecture/README.md`). Not `AGENTS.md`, not `docs/ai/`, not `.cursor/`, not workflows. | Conditions for a **future** auto-merge: required `check` green + independent review (or user waiver for a trivial docs fix). **Not enabled:** GitHub `allow_auto_merge` stays **false** because the platform cannot restrict auto-merge by path. |
| **Tests / non-critical tech** | Ordinary tests that do **not** match the financial class (example: `internal/platform/apperror/*_test.go`) | Future auto-merge only if: `check` green; independent review with no blocking findings; `retry_count` ≤ 3; not a new product stage blocked by Q-xx. **Not enabled.** |
| **Financial / ledger** | Ledger, ledger **tests**, CI, migrations, security/auth, payments/transfers/refunds/limits, financial-invariant docs, `.cursor/`, `AGENTS.md`, `docs/ai/`, `.github/` (agent/CI behavior), balances, fees, holds, payouts, simulator top-up, grants, `SECURITY DEFINER` | **Never auto-merge.** Human confirmation required. Extra review in [REVIEW_RULES.md](REVIEW_RULES.md). |

Path hints (non-exhaustive; the labeler is fail-safe): `internal/ledger/` (including `*_test.go`), `internal/platform/postgres/ledger*`, `migrations/**`, `internal/auth/`, `internal/accounts/`, `pkg/money/`, future `internal/simulator/` `internal/transfers/` `internal/payments/` `internal/refunds/` `internal/payouts/`, [../architecture/financial-invariants-testing.md](../architecture/financial-invariants-testing.md), [../database/concurrency.md](../database/concurrency.md), `.github/workflows/ci.yml`, `.cursor/environment.json`, `AGENTS.md`, `docs/ai/**`.

Workflow [`.github/workflows/pr-class-labels.yml`](../../.github/workflows/pr-class-labels.yml) applies labels `financial` / `docs-class` / `tests-class` using [`.github/scripts/classify-pr-paths.sh`](../../.github/scripts/classify-pr-paths.sh). Tests covering financial invariants are **not** `tests-class`. Labels are **advisory**. They do not merge. They do not skip `check`.

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

Forbidden: scheduled “pick whatever is next”, cron implementer, auto-approve (PR Routing & Approval), auto next roadmap stage.

## P-05 — STATE is GitHub truth

[STATE.md](STATE.md) may only quote `git` / `gh`. **Before every task**, reconcile STATE vs GitHub CLI/API. GitHub is source of truth; local STATE is not enough to start work. Done = `MERGED` + `origin/main` contains it + `check` success on **that** SHA. After merge, update STATE and **stop**.

## P-06 — No Cursor traces in git

Branches `feat/` `fix/` `docs/` only. No `cursor/` prefix, no Cursor trailers. English commits. Authored as the user.

## P-07 — Product stages need explicit allow

`user_allowed_next` defaults to `false`. Agents must **not** silently flip it. After a product-stage merge, keep it `false` unless the user set it.

| May proceed without flipping `user_allowed_next` | Must stop until the user sets the flag / answers Q-xx |
|---|---|
| Docs/STATE/automation-infra already specified (this classifier, STATE refresh, review rules) | Remaining Stage B **product** work (`audit.audit_logs`, Block/Unblock/Close HTTP, Open HTTP) |
| Mechanical tests/refactors with no new product behaviour, when they are not a new stage | Stage **C** and any monetary HTTP |
| | Inventing Q-xx answers; closing Q-12 / Q-13 / Q-30; new product behaviour not in D-xx |

Open HTTP stays blocked by Q-13 and Q-30. Docs/STATE/automation-infra is not a product stage.

This paragraph is the only P-07. Do not keep a looser copy in AGENTS.md or AUTOMATION.md.

## P-08 — Independent review ≠ CI

`gofmt` + `go vet` + `staticcheck` + `go test -race` (check name **`check`**) is mechanical. Not an AI review. Not a self-review. Reviewer ≠ implementer run.

## P-09 — Spend and stop

Do not raise spend limits. When the pool/cap hits, stop and leave STATE recoverable.

## P-10 — Do not invent ADRs or Q-xx

Stop and ask. Do not resolve Q-13, Q-30, Q-12, Q-16, or Q-20 in passing. Do not close Q-12 / Q-13 / Q-30.

## P-11 — Branch protection (live 2026-11-23)

`main` is protected by repository ruleset `protect-main` (id `27877702`): PRs required (`required_approving_review_count` **0**), `require_code_owner_review` **false**, no force-push (`non_fast_forward`), no deleting `main` (`deletion`), required status check context **`check`**, strict vs the latest SHA, `bypass_actors` empty. Classic “branch protection” API may still 404; the **ruleset** is what GitHub enforces. `allow_auto_merge=false`.

**Consequence:** a write-capable agent can merge a green financial PR today. See P-12.

## P-12 — Financial merge block (prepared, not PATCHed)

GitHub cannot require approving reviews only for label `financial`. This user-owned repository cannot use team `required_reviewers` file patterns.

Prepared (in git; **user must click** to enforce):

1. Mechanism for **all** PRs: [`.github/CODEOWNERS`](../../.github/CODEOWNERS) `* @romangergovskiy`.
2. Settings click: enable Require review from Code Owners and/or `required_approving_review_count=1` on `protect-main`. Keep auto-merge off, keep `check` required, keep bypass empty.
3. A second GitHub account must approve (author cannot approve own PR; agent uses the same identity). Do not add the owner as a bypass actor.
4. Do not weaken `main`.
5. Agents do not PATCH GitHub protection without explicit user permission in the prompt.

Until those clicks, human-only financial merge is **policy**, not GitHub enforcement.
