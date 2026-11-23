#!/usr/bin/env bash
# Fail-safe P-01 class. Ambiguous → financial.
# Reads changed paths from stdin (one per line). Prints: financial | docs-class | tests-class
# Advisory only: does not merge and does not skip GitHub Actions job `check`.
set -euo pipefail

financial=0
docs_only=1
tests_only=1
n=0

# Never auto docs-class / tests-class. Includes ledger tests, CI, migrations,
# security/auth, money HTTP, financial-invariant docs, agent memory, Cursor
# config, and workflows/scripts that change agent or CI behavior.
fin_re='^(AGENTS\.md|\.cursor/|docs/ai/|\.github/|migrations/|internal/ledger/|internal/platform/postgres/ledger|internal/simulator/|internal/transfers/|internal/payments/|internal/refunds/|internal/payouts/|internal/limits/|internal/auth/|internal/identity/|internal/platform/authcrypto/|internal/platform/httpserver/auth|internal/platform/postgres/auth|internal/accounts/|internal/ci/|pkg/money/|docs/architecture/ledger|docs/architecture/money-flows|docs/architecture/chart-of-accounts|docs/architecture/financial-invariants|docs/architecture/ledger-schema|docs/architecture/identity-auth|docs/database/concurrency\.md|docs/database/data-integrity)'

# Ordinary docs only. Not AGENTS.md, not docs/ai/, not financial-invariant docs.
docs_re='^(README\.md|docs/architecture/README\.md)$'

# Ordinary tests. Ledger / auth / money tests match fin_re first and stay financial.
tests_re='(_test\.go$|/testdata/)'

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
