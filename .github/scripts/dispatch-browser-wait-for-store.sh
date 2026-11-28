#!/usr/bin/env bash
# The readiness wait for the S3-compatible server the shard job starts for its stacks' uploads:
# it waits for the filer to answer, makes the bucket, writes one object, reads it back and
# deletes it, and prints the server's own log for whichever of those did not happen.
#
# Who runs it: the step "Wait for the S3-compatible server" of
# .github/workflows/dispatch-browser.yaml's shard job;
# .github/ci-notes/dispatch-browser.md#why-the-stores-readiness-wait-writes-reads-and-deletes-an-object
# says why a bucket alone is not enough to wait for. It takes no argument. $RUNNER_TEMP is where
# the step before it
# redirected the server's log, which is what a job in a container has in place of `docker logs`.
# The runner sets $RUNNER_TEMP for every step.
#
# Run from anywhere, with $RUNNER_TEMP set: .github/scripts/dispatch-browser-wait-for-store.sh
# CI runs it in dispatch-browser.yaml's shard job, at the step "Wait for the S3-compatible
# server".
set -euo pipefail

# Every address here is written out at its call rather than held in a variable. This is a
# readiness poll of a process the step before this one started and no download, which is what
# .github/scripts/check-download-retry.sh's first scoping says; that check tells the two apart by
# the loopback address in the word it reads, so a `"$store/readiness"` hides the host and the poll
# reads as an unretried fetch and as a loop around one.
for attempt in $(seq 1 60); do
  curl -sf -o /dev/null http://127.0.0.1:18333/ && break
  [ "$attempt" -lt 60 ] || { cat "$RUNNER_TEMP/weed.log"; exit 1; }
  sleep 1
done
curl -sf -X PUT -o /dev/null http://127.0.0.1:18333/dispatch-e2e-files \
  || { cat "$RUNNER_TEMP/weed.log"; exit 1; }
# The write is retried: the bucket exists as soon as the filer answers, while the
# volume server the object lands in can still be coming up behind it.
for attempt in $(seq 1 60); do
  printf 'ready' \
    | curl -sf -X PUT --data-binary @- -o /dev/null http://127.0.0.1:18333/dispatch-e2e-files/readiness \
    && break
  [ "$attempt" -lt 60 ] || { echo "::error::the file store took no object"; cat "$RUNNER_TEMP/weed.log"; exit 1; }
  sleep 1
done
[ "$(curl -sf http://127.0.0.1:18333/dispatch-e2e-files/readiness)" = ready ] \
  || { echo "::error::the file store did not serve back what it stored"; cat "$RUNNER_TEMP/weed.log"; exit 1; }
curl -sf -X DELETE -o /dev/null http://127.0.0.1:18333/dispatch-e2e-files/readiness
