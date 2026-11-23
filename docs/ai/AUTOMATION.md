# Automation loop (design)

This describes how agents **should** work. It does **not** enable Cursor Automations, GitHub auto-merge, or Bugbot. The user still configures those (see the end of this file).

Cloud Agents consume Cursor Pro / Premium usage and **stop**. That is normal. Resume from [STATE.md](STATE.md) + GitHub, not from the old chat.

## Intended loop

```text
check origin/main + STATE
  → pick next *allowed* task (ROADMAP + STATE.user_allowed_next + blockers)
  → lock STATE (one implementer)
  → branch from origin/main (feat/ | docs/ | fix/; never cursor/)
  → implement + make check
  → open PR
  → independent reviewer (not the implementer)
  → fix findings on the same PR (max 3) + re-review + CI
  → merge per P-01 policy (not enabled today → stop at merge_ready)
  → update STATE from GitHub (MERGED + main SHA + CI on that SHA)
  → clear lock; user_allowed_next=false after a product stage
  → record next_task; do not cron into the following product stage
```

**Allowed** means all of:

1. Not already on `main` (do not re-run a finished slice).
2. Dependencies on `main` (e.g. C needs remaining B).
3. Blockers do not apply to **this** slice (Open is blocked by Q-13; status HTTP is not).
4. Product stages need `user_allowed_next=true`. Docs/STATE refresh after merge does not.
5. No second implementer: `lock` is null or this agent owns it.

## Roles

| Actor | Does | Does not |
|---|---|---|
| Implementer (Cloud Agent or local Agent) | Branch, code, `make check`, PR, fix up to 3 times | Merge financial class; review itself; start C while B remainder / Q-13 open |
| Independent reviewer | Comment on the PR diff | Push to the implementer branch; merge |
| GitHub Actions | Mechanical CI | AI review |
| State updater (later Automation on `pull_request merged`) | Re-query `gh`, patch STATE | Start the next product stage |
| User | `user_allowed_next`, financial merge, Q-xx, spend limit, enable Automations | — |

Do not use Cursor “PR Routing & Approval” to auto-approve PayCore PRs.

## Recovery after stop / context loss

A new agent, no chat:

1. Read [../../AGENTS.md](../../AGENTS.md) and [STATE.md](STATE.md).
2. `git fetch origin main && git rev-parse origin/main`.
3. `gh pr list --state open` and, if `lock.pr` is set, `gh pr view <n> --json state,headRefOid,url`.
4. Choose:

| Situation | Action |
|---|---|
| Open PR for the slice, CI failed, `retry_count < 3` | Resume **same** branch |
| Open PR, `merge_ready` or CI green awaiting policy | Do not push; wait for user / policy |
| `lock` older than 24h, no PR, no commits | Clear lock, `status=idle` |
| Chat gone, PR exists | GitHub is memory; **do not** open a second PR |
| Slice already on `main` | Exit; optionally refresh STATE |
| Spend/usage exhausted | `blocked`; do not tight-loop |

Never continue Stage C because an old transcript mentioned C.

## Caps (policy, not a Cursor product guarantee)

| Cap | Default |
|---|---|
| Implementer fix pushes | 3 per PR |
| Concurrent implementers | 1 (`STATE.lock`) |
| Auto-start next **product** stage | No (`user_allowed_next` resets false) |
| Auto-merge | **Off** until the user enables P-01 |
| Stale lock | 24h without PR/commits |
| Scheduled “next stage forever” | Forbidden |
| Cursor spend | User-set on-demand / additional-usage limit **before** Automations |

Cursor may rate-limit Automations. MCP rate limits pause; do not add a retry hammer.

## Merge policy (document only)

See [DECISIONS.md](DECISIONS.md) P-01. **Do not turn on GitHub auto-merge or an auto-merge Automation in the PR that adds these files.**

When the user later enables it:

1. Classify the PR (docs / tests / financial) from the diff.
2. Require `ci / check` success on the head SHA (cite the run URL).
3. Require independent review for tests and financial; docs may use a lighter bar as in P-01.
4. Financial class: stop and ask the user. Never auto-merge.
5. After merge, state updater verifies MERGED + `origin/main` + CI on main. Then STATE. That updater must not chain-implement the next product stage.

Until branch protection exists (today: **off**), auto-merge is unsafe even for docs. Enabling P-01 should include required status check `check`.

## Prototype vs later

| Phase | In design | Enable now? |
|---|---|---|
| Memory files in git | This change | Yes (the files) |
| Manual Cloud Agent: read STATE, do not edit | User runs | User |
| Automations: PR opened → reviewer | Later | No |
| Automations: CI failed → implementer fix (cap 3) | Later | No |
| Automations: PR merged → STATE PR | Later | No |
| Cron implementer | Never | No |

## Cursor Pro / Cloud Agent limits (facts + what to do)

- Cloud Agents and Automations use plan usage at the model rate. Automations use max context (no toggle). They are not free infinite workers.
- Legacy Cursor **Pro** includes Cloud Agents, Automations, Bugbot (usage-metered). Confirm the live plan in the dashboard.
- Grok 4.6 draws the Cursor-models pool; overage is billed. A second (third-party) review model can cost extra.
- When the pool or spend cap hits, the agent stops. Leave STATE + the PR so another run can resume.
- Self-hosted “My Machines” does not read `.cursor/environment.json`. Cursor VMs should start Docker via that file.

## What the user still configures

1. Connect the Cursor GitHub app to `romangergovskiy/paycore`.
2. Set a Cloud Agent / on-demand **spend limit**.
3. Decide Bugbot on/off (`autofix` off for financial).
4. Cloud VM vs MacBook self-hosted worker.
5. Branch protection + required `check` before any auto-merge.
6. Flip `user_allowed_next` when a product slice is approved.
7. Confirm Q-13 / Q-30 before Open HTTP; Q-12 before Stage C seeds.
8. Actually enable Automations later — these markdown files do not create them.
