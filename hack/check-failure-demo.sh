#!/usr/bin/env bash
# Runs the deliberately failing suite in mode $1 (steps or env-setup) and
# checks that it failed the way it should and cleaned up after itself.
set -uo pipefail

mode="$1"
log="$(mktemp)"
XP_E2E_FAILURE_DEMO="$mode" go test ./providers/kubernetes/failures/ -v -count=1 -timeout 20m 2>&1 | tee "$log"
code="${PIPESTATUS[0]}"

fail() { echo "::error::failure demo ($mode): $*"; exit 1; }

[ "$code" -ne 3 ] || fail "TestMain reported a leaked cluster (exit 3)"
[ "$code" -ne 0 ] || fail "suite passed, but it is meant to fail"
grep -q 'no leak: cluster' "$log" || fail "no 'no leak' confirmation in the output"

if [ "$mode" = steps ]; then
  # Only top-level results count: steps inside a failing test (e.g. its
  # "create" step) are expected to pass.
  ! grep -qE -- '^--- PASS: TestExpectFail_' "$log" || fail "a TestExpectFail_ test passed"
  grep -qE -- '^--- FAIL: TestExpectFail_' "$log" || fail "no TestExpectFail_ test ran"
  grep -qE -- '^--- PASS: TestCleanupVerified' "$log" || fail "TestCleanupVerified did not pass"
fi
echo "failure demo ($mode): failed as intended and cleaned up"
