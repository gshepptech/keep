#!/usr/bin/env bash
# docs-scene.sh -- build the scene the README and guide are photographed
# against, and serve it.
#
# The six images under docs/images have now been retaken by hand three
# times, and each time the hard part was not the screenshot but rebuilding
# the situation: two repositories, a ranked queue with a real spread of
# scores, closed work behind it so the activity page has something to show,
# and two agents genuinely holding files so the collision rows are real
# rather than mocked up. This script is that situation, so the next reshoot
# costs a command instead of an afternoon.
#
# It is hermetic. HOME is exported into its own temporary tree before the
# first `keep` call, which is what keeps the registry it writes out of the
# developer's real ~/.rounce/repos.json -- the same rule every demo-phase
# script in this directory follows, and for the same reason.
#
# Usage:
#   scripts/docs-scene.sh            # build, serve, print the URLs, wait
#   scripts/docs-scene.sh --keep     # leave the scene on disk when it exits
#
# Everything is torn down on exit, including the board, on success and on
# failure alike.

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
KEEP_BIN="$ROOT/keep"
LEAVE_SCENE=0
[[ "${1:-}" == "--keep" ]] && LEAVE_SCENE=1

if [[ ! -x "$KEEP_BIN" ]]; then
  echo "building keep..." >&2
  (cd "$ROOT" && go build -o keep ./cmd/keep)
fi

SCENE="$(mktemp -d)"
BOARD_PID=""

cleanup() {
  # Guarded on being non-empty rather than defaulted with ${BOARD_PID:-0}:
  # an unset PID defaulting to "0" signals process 0, which hits the whole
  # calling process group including the invoking shell. That exact bug was
  # found and fixed once already in demo-phase-3.sh.
  if [[ -n "$BOARD_PID" ]]; then
    kill "$BOARD_PID" 2>/dev/null || true
    wait "$BOARD_PID" 2>/dev/null || true
  fi
  if [[ "$LEAVE_SCENE" -eq 1 ]]; then
    echo "scene left at $SCENE" >&2
  else
    rm -rf "$SCENE"
  fi
}
trap cleanup EXIT

export HOME="$SCENE/home"
mkdir -p "$HOME"

# Two repositories, because one is not enough to photograph the project
# switcher or the cross-project queue, and three is more than the switcher
# needs to demonstrate itself.
API="$SCENE/checkout-api"
WEB="$SCENE/storefront"
mkdir -p "$API" "$WEB"

for d in "$API" "$WEB"; do
  git -C "$d" init -q
  # A local identity, because HOME points inside this scene and so the
  # developer's global git config is deliberately not visible here.
  git -C "$d" config user.email scene@example.invalid
  git -C "$d" config user.name "docs scene"
  git -C "$d" commit -q --allow-empty -m "initial"
  (cd "$d" && "$KEEP_BIN" init >/dev/null)
done

# add files a lease can be taken on. Interlock holds paths, and a path that
# does not exist is not a convincing photograph.
mkdir -p "$API/internal/payments" "$API/internal/auth" "$WEB/src/checkout"
touch "$API/internal/payments/refund.go" "$API/internal/auth/token.go" \
      "$WEB/src/checkout/basket.ts"

add() { # add <dir> <title> <score> <type> <why> <acceptance> <paths>
  (cd "$1" && "$KEEP_BIN" add "$2" --score "$3" --type "$4" \
     --why "$5" --acceptance "$6" --paths "$7")
}

# The queue. Scores are spread deliberately: a flat queue photographs as a
# list, and the whole claim this board makes is that it ranks.
TOKEN=$(add "$API" "Refresh tokens are accepted after the user signs out" 9/9/2 bug \
  "A signed-out session can be revived from a stolen refresh token for the rest of its 30-day window. This is the one item here that is a security hole rather than a defect." \
  "Signing out invalidates every refresh token issued to that session, proven by a test that signs out and then replays the token." \
  "internal/auth/token.go")

add "$API" "Refunds double-charge when the gateway retries" 9/7/5 bug \
  "The gateway retries on a 5xx and the handler is not idempotent, so a customer can be refunded twice for one order. Two support tickets this month." \
  "Replaying the same refund request is a no-op the second time. A test issues the same idempotency key twice and asserts one ledger row." \
  "internal/payments/refund.go" >/dev/null

BASKET=$(add "$WEB" "Basket totals disagree with the order confirmation" 8/8/3 bug \
  "The basket rounds per line and the confirmation rounds once at the end, so a five-line order can differ by a penny. Cheap to fix and visible to every customer." \
  "One rounding rule, applied in one place. A property test over random baskets asserts the two totals are equal." \
  "src/checkout/basket.ts")

add "$API" "Rate-limit the password reset endpoint" 7/8/2 task \
  "It is the only unauthenticated write endpoint with no limit on it." \
  "Ten requests per hour per address, with a test that asserts the eleventh is refused." \
  "internal/auth/token.go" >/dev/null

add "$WEB" "Ship the guest checkout flow" 8/5/8 task \
  "The largest single drop-off in the funnel is the account-creation step. Big, and the estimate is soft until the payment side is scoped." \
  "A customer completes a purchase without an account, and the order appears in the same place as every other order." \
  "src/checkout/basket.ts" >/dev/null

add "$API" "Replace the hand-rolled retry loop with a backoff library" 4/6/3 task \
  "It retries immediately three times, which is not a backoff, and it is copied into four call sites." \
  "One retry helper, exponential with jitter, and the four call sites use it." \
  "internal/payments/refund.go" >/dev/null

add "$WEB" "Dark mode for the checkout pages" 3/6/5 task \
  "Asked for twice. The rest of the site already has it, so checkout is the odd one out." \
  "Checkout follows the same theme token as every other page, at both settings." \
  "src/checkout/basket.ts" >/dev/null

# Closed and dropped work, so the activity page has a history rather than
# one row, and so the queue is visibly a live thing with a past.
DONE=$(add "$API" "Stop logging card numbers in the gateway error path" 10/9/1 bug \
  "The 4xx branch logged the whole request body." \
  "No card data reaches the log, proven by a test that asserts the redaction." \
  "internal/payments/refund.go")
(cd "$API" && "$KEEP_BIN" start "$DONE" >/dev/null && \
  "$KEEP_BIN" close "$DONE" --reason "Redacted at the logger rather than the call site, so a new call site cannot reintroduce it." >/dev/null)

GONE=$(add "$WEB" "Rewrite the basket in a different framework" 2/2/9 task \
  "Proposed during the outage, and it does not survive daylight." \
  "n/a" "src/checkout/basket.ts")
(cd "$WEB" && "$KEEP_BIN" drop "$GONE" --reason "The rounding bug it was meant to fix is one line, and it is already filed separately." >/dev/null)

# Two agents holding files, through Interlock's real hook rather than a
# hand-written lease file. Without this the collision rows on the board are
# a feature nobody can photograph.
if command -v interlock >/dev/null 2>&1; then
  hold() { # hold <dir> <session> <path>
    printf '{"session_id":"%s","tool_name":"Write","tool_input":{"file_path":"%s","content":"x"}}' "$2" "$3" \
      | (cd "$1" && interlock hook PreToolUse claude >/dev/null 2>&1) || true
  }
  (cd "$API" && interlock init >/dev/null 2>&1) || true
  (cd "$WEB" && interlock init >/dev/null 2>&1) || true
  hold "$API" "sess-refund-agent" "$API/internal/payments/refund.go"
  hold "$WEB" "sess-basket-agent" "$WEB/src/checkout/basket.ts"
else
  echo "note: interlock is not on PATH -- the held-file rows will be empty" >&2
fi

# Serve. --web prints the URL it chose; --port pins it so the shot script
# and the person reading this output are looking at the same place.
PORT="${PORT:-7788}"

# Refuse a port that is already answering. Without this check the wait loop
# below succeeds against whatever is already there -- which is exactly what
# happened the first time this script ran, against a board left over from
# three hours earlier, and the screenshots taken from it were of the old
# build. A scene script that photographs the wrong process is worse than no
# scene script.
if curl -fsS -m 1 "http://127.0.0.1:$PORT/" >/dev/null 2>&1; then
  echo "port $PORT is already answering -- something else is serving there." >&2
  echo "stop it, or run with PORT=<other> scripts/docs-scene.sh" >&2
  exit 1
fi

(cd "$API" && "$KEEP_BIN" board --web --port "$PORT" >"$SCENE/board.log" 2>&1) &
BOARD_PID=$!

READY=0
for _ in $(seq 1 50); do
  curl -fsS "http://127.0.0.1:$PORT/" >/dev/null 2>&1 && { READY=1; break; }
  sleep 0.1
done
if [[ "$READY" -eq 0 ]]; then
  echo "the board never came up on $PORT. Its output:" >&2
  cat "$SCENE/board.log" >&2
  exit 1
fi

cat <<EOF

scene ready on http://127.0.0.1:$PORT

  /                              the queue          -> docs/images/keep-queue.png
  /search?q=refund               a search           -> docs/images/keep-search.png
  /activity                      what has happened  -> docs/images/keep-activity.png
  /item/$TOKEN                one item           -> docs/images/keep-item.png
  /projects                     the project list
  /all                          every project's queue

  the held-file rows are on / , below the queue     -> docs/images/keep-held.png
  the first-run screen needs a repository with no store; see docs/guide.md

Ctrl-C to tear it all down.
EOF

wait "$BOARD_PID"
