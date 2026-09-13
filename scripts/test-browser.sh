#!/bin/sh
set -eu
: "${TEST_DATABASE_URL:?An isolated TEST_DATABASE_URL is required}"
mkdir -p test-results
fixture_dir=$(mktemp -d)
fixture_pid=
cleanup() {
 if [ -n "$fixture_pid" ]; then
  kill "$fixture_pid" 2>/dev/null || true
  wait "$fixture_pid" 2>/dev/null || true
 fi
 if [ -f "$fixture_dir/server.log" ]; then cp "$fixture_dir/server.log" test-results/browser-fixture.log; fi
 rm -rf "$fixture_dir"
}
trap cleanup EXIT
trap 'exit 130' INT TERM
export WEB_UI_E2E=1
export WEB_UI_TEST_ORIGIN=http://127.0.0.1:18080
export WEB_UI_TEST_ADDR=127.0.0.1:18080
go test -c -o "$fixture_dir/server.test" ./tests/integration
"$fixture_dir/server.test" -test.timeout=5m -test.run '^TestBrowserWorkspaceFixture$' -test.v >"$fixture_dir/server.log" 2>&1 &
fixture_pid=$!
ready=false
for attempt in $(seq 1 60); do
 if grep -q 'browser fixture ready' "$fixture_dir/server.log" && curl --fail --silent "$WEB_UI_TEST_ORIGIN/health/ready" >/dev/null; then ready=true; break; fi
 if ! kill -0 "$fixture_pid" 2>/dev/null; then cat "$fixture_dir/server.log"; exit 1; fi
 sleep 1
done
if [ "$ready" != true ]; then cat "$fixture_dir/server.log"; exit 1; fi
npm run test:browser
