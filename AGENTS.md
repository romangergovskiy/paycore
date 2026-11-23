# PayCore — instructions for coding agents

Read this file first, then [docs/ai/STATE.md](docs/ai/STATE.md). GitHub + these files are memory. Do not rely on chat history. The Cursor Context store is **not** visible to a Cloud Agent that only clones this repository.

Operating model: **event-driven**. PR opened → independent review. CI `check` failed → implementer fix ≤3. PR merged → STATE update from GitHub. **No infinite cron.** Model for this setup: Grok 4.6.

## Read order

1. [docs/ai/STATE.md](docs/ai/STATE.md) — confirmed `main` SHA, stage, open PRs, lock, next task.
2. Confirm GitHub: `git fetch origin main` and `gh pr list --state open`. If they disagree with STATE, refresh STATE from GitHub **before** implementing. Do not implement from a stale SHA.
3. If `status` is `in_progress` or `review` and `lock` is fresh (see [docs/ai/AUTOMATION.md](docs/ai/AUTOMATION.md)), **do not start a second implementer**. Resume that lock’s branch/PR or stop.
4. [docs/ai/ROADMAP.md](docs/ai/ROADMAP.md) — stages A–N and what is allowed next.
5. [docs/ai/AUTOMATION.md](docs/ai/AUTOMATION.md) — loop, recovery, caps, merge classes.
6. [docs/ai/REVIEW_RULES.md](docs/ai/REVIEW_RULES.md) — independent review. CI is not an AI review. Do not self-approve.
7. Product ADRs: [docs/ai/ARCHITECTURE.md](docs/ai/ARCHITECTURE.md) → `docs/architecture/` and `docs/database/`. Do not invent ADRs or Q-xx answers.

## Hard guardrails

- No real payment systems, production secrets, or live customer data.
- **Financial class never auto-merges** ([docs/ai/DECISIONS.md](docs/ai/DECISIONS.md) P-01). Ledger, balances, fees, limits, holds, payments, transfers, refunds, financial migrations, grants, `SECURITY DEFINER`, anything money-adjacent. **Ambiguous = financial.** Agents must not merge those PRs.
- GitHub **auto-merge stays OFF** (cannot restrict by path). Do not enable it. Do not merge from an agent unless the user explicitly asked to merge **that** PR and it is not financial.
- Do not start Stage C, monetary HTTP, or Open HTTP while [Q-13](docs/architecture/open-questions.md#q-13) / [Q-30](docs/architecture/open-questions.md#q-30) are open. Do not invent Q-xx answers. Do not close Q-12 / Q-13 / Q-30.
- Do not silently flip `user_allowed_next`. P-07: mechanical/docs/tech work already allowed by spec/ROADMAP may proceed; **product stages** still stop on open Q-xx and unapproved product behaviour.
- Do not silently change architecture. New behaviour needs an existing D-xx or a user-approved Q-xx.
- Do not rewrite Auth, Accounts application, or the ledger gateway “while here”.
- Do not disable tests, skip `go test -race`, or shrink CI. Quote the GitHub check URL for **this** head SHA. The required check context is **`check`** (job name in `.github/workflows/ci.yml`), not `ci / check`.
- One implementer per task. Reviewer must be a **different** agent/run (or Bugbot/human). No self-approve.
- Cloud Agents stop; that is expected. Resume from STATE + GitHub.

A slice is **not done** until: GitHub PR `state=MERGED`, `origin/main` SHA verified, CI on **that** SHA green (`check`), and STATE updated. A green PR is `merge_ready` only.

## Git

- Branch from current `origin/main`. Names: `feat/…`, `fix/…`, `docs/…`. **No** `cursor/` prefix, no Cursor trailers.
- Commit messages in English. Author as the repo’s normal user.
- Do not dirty `~/paycore` if that is the human checkout. New worktree from `origin/main`. Do not reuse `stage-a-idempotency-outbox` or `stage-b-accounts-http`.

## Checks

```sh
make check   # gofmt, go vet, staticcheck, go test -race
```

Integration tests need Docker (testcontainers, PostgreSQL 16). If `docker info` fails on a Cloud VM, start Docker (`sudo service docker start`) and retry. Do **not** skip PG tests to go green. CI installs `staticcheck@2025.1.1` and does not skip.

## Pointers (do not duplicate)

| Need | File |
|---|---|
| Current facts | [docs/ai/STATE.md](docs/ai/STATE.md) |
| Next stage / A–N | [docs/ai/ROADMAP.md](docs/ai/ROADMAP.md) |
| Automation loop | [docs/ai/AUTOMATION.md](docs/ai/AUTOMATION.md) |
| Review + merge classes | [docs/ai/REVIEW_RULES.md](docs/ai/REVIEW_RULES.md) |
| Automation policy (P-xx) | [docs/ai/DECISIONS.md](docs/ai/DECISIONS.md) |
| Product ADRs (D-xx) | [docs/architecture/decisions.md](docs/architecture/decisions.md) |
| Open questions | [docs/architecture/open-questions.md](docs/architecture/open-questions.md) |
| Spec | [docs/PROJECT_SPEC.md](docs/PROJECT_SPEC.md) |
| Locks / Tx | [docs/database/concurrency.md](docs/database/concurrency.md) |
| Schema | [docs/database/database-schema.md](docs/database/database-schema.md) |
| Invariant tests | [docs/architecture/financial-invariants-testing.md](docs/architecture/financial-invariants-testing.md) |

## Cursor Cloud specific instructions

- Confirm `origin/main` before branching. Ignore a stale laptop checkout.
- On Cursor-hosted VMs: `.cursor/environment.json` `start` runs `sudo service docker start`. Self-hosted workers **do not** read that file — start Docker on the machine.
- Generate local secrets in the VM (`openssl rand`). Never copy production `PAYCORE_*`.
- If usage/spend limits stop the run, leave STATE recoverable and stop. Do not loop. Do not raise spend limits.
- If the UI offers a `cursor/…` branch, rename to `feat/` / `docs/` / `fix/` before push, or stop.
