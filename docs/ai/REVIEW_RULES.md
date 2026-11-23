# Independent review rules

The implementer must not be the only reviewer and must **not** self-review or self-approve. A second agent/run, Bugbot (if actually running), Security Review, or a human must read the PR. GitHub Actions is required and **is not** an independent AI review.

If no independent reviewer comments, the PR stays **unreviewed**. Do not treat the implementer’s own reading of the diff as independent review.

## What CI is

Workflow [`.github/workflows/ci.yml`](../../.github/workflows/ci.yml): gofmt, `go vet ./...`, `staticcheck@2025.1.1`, `go test -race ./...`.

GitHub Checks **name / required context: `check`** (the job id). Do not require `ci / check` — that string is not what the API reports.

That job does **not** judge architecture, L1…L8, or financial invariants. Do not write “reviewed by CI”. After push, cite the check URL for **this** head SHA.

Labeler [`.github/workflows/pr-class-labels.yml`](../../.github/workflows/pr-class-labels.yml) does not skip `check`. Ambiguous diffs and ledger tests are `financial`, not `tests-class`.

## Who reviews

| Role | Allowed | Not allowed |
|---|---|---|
| Implementer | Code, tests, respond to findings (retry cap 3) | Approving its own PR; reviewing its own PR; merging financial class |
| Independent AI (Bugbot, Security Review, or a **different** agent/run) | Diff comments | Same conversation/run as implementer |
| GitHub Actions `check` | Mechanical | Called “AI review” |
| Human | Always for financial merge; Q-xx | — |

Free fallback: `check` + human reading the diff. **Do not call that a full independent AI review.**

Bugbot: `.cursor/config/bugbot.yaml` sets Autofix **disabled**. That file does not enable Bugbot and is read from the **base** branch after merge. Do not claim Bugbot reviewed a PR unless a Bugbot check or comment exists on that PR.

## Checklist (every PR)

1. Diff vs allowed slice. No drive-by Auth/Accounts/ledger rewrites.
2. Architecture matches D-xx. Open Q-xx stay open unless the user closed them. Agents do not close Q-12 / Q-13 / Q-30.
3. Security: JWT.roles is not authorization; no secret leaks; parameterized SQL.
4. Concurrency: L1…L8 in [../database/concurrency.md](../database/concurrency.md).
5. SQL/Tx: key + effect + outbox in one Tx. App has no ledger DML.
6. Errors: D-39; no internals in JSON.
7. Tests exist; not 2xx-only; do not drop `-race`.
8. Money PRs (Stage C+): `assertLedgerInvariants` after monetary tests. Until that helper exists, **refuse monetary HTTP**.

## Extra strict (financial class)

Human merge. No auto-merge. No float. Compensations = new journal. Retry policy unchanged. Auth, CI, migrations, `AGENTS.md`, `docs/ai/`, `.cursor/`, and ledger tests are financial under P-01.

## Findings loop

Reviewer (not the implementer run) comments. Implementer fixes on the **same** branch, `retry_count++`. After 3, `blocked`, **stop**. No parallel PR for the same slice. After the user merges, a later run updates STATE from GitHub and **stops** (no auto next stage).
