#!/usr/bin/env bash
# Fail-safe P-01 class. Ambiguous → financial.
# Reads changed paths from stdin (one per line). Prints: financial | docs-class | tests-class
# Advisory only: does not merge and does not skip GitHub Actions job `check`.
set -euo pipefail

financial=0
docs_only=1
tests_only=1
n=0

# Never auto docs-class / tests-class. Money, auth, CI/agent behavior, migrations,
# postgres journal/accounts/idempotency/outbox, app wiring, agent memory.
fin_re='^(AGENTS\.md|\.cursor/|docs/ai/|\.github/|migrations/|internal/ledger/|internal/platform/postgres/ledger|internal/platform/postgres/post_journal|internal/platform/postgres/account|internal/simulator/|internal/transfers/|internal/payments/|internal/refunds/|internal/payouts/|internal/limits/|internal/auth/|internal/identity/|internal/platform/authcrypto/|internal/platform/httpserver/|internal/platform/postgres/auth|internal/platform/idempotency/|internal/platform/outbox/|internal/platform/inbox/|internal/platform/postgres/idempotency|internal/platform/postgres/outbox|internal/platform/postgres/inbox|internal/accounts/|internal/app/|internal/ci/|cmd/migrate/|pkg/money/|docs/architecture/ledger|docs/architecture/money-flows|docs/architecture/chart-of-accounts|docs/architecture/financial-invariants|docs/architecture/ledger-schema|docs/architecture/identity-auth|docs/database/concurrency\.md|docs/database/data-integrity)'

# Ordinary docs only. Not AGENTS.md, not docs/ai/, not financial-invariant docs.
docs_re='^(README\.md|docs/architecture/README\.md)$'

# Ordinary tests allowlist: test files only, not production .go in those dirs.
# Any other path is financial (fail-safe).
tests_re='^(internal/platform/apperror/.*_test\.go$|internal/platform/config/.*_test\.go$|internal/platform/postgres/dsn_test\.go$)'

while IFS= read -r f; do
  [ -z "$f" ] && continue
  n=$((n + 1))
  if echo "$f" | grep -Eq "$fin_re"; then
    financial=1
  fi
  if ! echo "$f" | grep -Eq "$docs_re"; then
    docs_only=0
  fi
  if ! echo "$f" | grep -Eq "$tests_re"; then
    tests_only=0
  fi
done

if [ "$n" -eq 0 ]; then
  echo "financial"
  exit 0
fi

if [ "$financial" -eq 1 ] || { [ "$docs_only" -eq 0 ] && [ "$tests_only" -eq 0 ]; }; then
  echo "financial"
elif [ "$docs_only" -eq 1 ]; then
  echo "docs-class"
else
  echo "tests-class"
fi
