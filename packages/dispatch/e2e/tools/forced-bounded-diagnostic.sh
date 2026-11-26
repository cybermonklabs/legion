#!/usr/bin/env bash
# legion: tool - forces INFRA-3904's two timeout diagnostics at a base and a head when a person reruns #5655's control
# Forces the two timeout diagnostics that INFRA-3904 records. At the merge base the runner copies
# the head's two tests, so each reaches the old bounded-subprocess implementation; at the head it
# runs the checked-in tests. The generic child and the public T5 relay hold then name their own
# forced timeout rather than relying on machine load.
set -u

if [ "$PWD" = "${BASE_DIR:-}" ]; then
  : "${FORCED_HEAD_DIR:?FORCED_HEAD_DIR is required while forcing the base}"
  cp "$FORCED_HEAD_DIR/packages/dispatch/e2e/__tests__/bounded-subprocess.test.ts" \
    packages/dispatch/e2e/__tests__/bounded-subprocess.test.ts
  cp "$FORCED_HEAD_DIR/packages/dispatch/e2e/__tests__/outside-agent-public.test.ts" \
    packages/dispatch/e2e/__tests__/outside-agent-public.test.ts
fi

cd packages/dispatch || exit 2

run_forced() {
  local file=$1 title=$2 log rc named pass fail bound
  log=$(mktemp) || return 2
  bun run test:harness "$file" -t "$title" >"$log" 2>&1
  rc=$?
  cat "$log"
  named=$(grep -F -c "$title" "$log" || true)
  pass=$(grep -cE '^ *1 pass$' "$log" || true)
  fail=$(grep -cE '^ *[1-9][0-9]* fail$' "$log" || true)
  bound=$(grep -c '^bounded child:.*bound passed' "$log" || true)
  rm -f "$log"
  case "$rc:$named:$pass:$fail:$bound" in
    0:[1-9]*:1:0:[1-9]*)
      printf 'forced bounded diagnostic: %s passed with its timeout plant fired\n' "$title"
      return 0
      ;;
    1:[1-9]*:0:[1-9]*:[1-9]*)
      printf 'forced bounded diagnostic: %s failed with its timeout plant fired\n' "$title"
      return 1
      ;;
    *)
      printf 'forced bounded diagnostic: %s did not run as one named result with its plant fired (exit=%s named=%s pass=%s fail=%s bound=%s)\n' "$title" "$rc" "$named" "$pass" "$fail" "$bound" >&2
      return 2
      ;;
  esac
}

first=0
run_forced e2e/__tests__/bounded-subprocess.test.ts \
  'a bounded child at its bound reports its last output and the processes it still runs' || first=$?
second=0
run_forced e2e/__tests__/outside-agent-public.test.ts \
  'a T5 relay hold that never closes leaves a named timeout diagnostic' || second=$?
case "$first:$second" in
  0:0) exit 0 ;;
  1:1) exit 1 ;;
  *)
    printf 'forced bounded diagnostic: expected both rows to agree, got generic=%s public=%s\n' "$first" "$second" >&2
    exit 2
    ;;
esac
