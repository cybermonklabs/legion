#!/bin/sh
# The readiness wait for the S3-compatible server the shard job starts for its stacks' uploads.
#
# Who runs it: the step "Wait for the S3-compatible server" of
# .github/workflows/dispatch-browser.yaml's shard job;
# .github/ci-notes/dispatch-browser.md#why-the-stores-readiness-wait-writes-reads-and-deletes-an-object
# says why a bucket alone is not enough to wait for, why no write may grow a collection, and how
# long each bound below is and why. It takes no argument. $RUNNER_TEMP is where the step before it
# redirected the server's log, which is what a job in a container has in place of `docker logs`.
# The runner sets $RUNNER_TEMP for every step.
#
# It is POSIX sh, as the shard job's own steps are (`sh -e`), and it holds every request and every
# wait to a bound. Every request goes through s3 below, under curl's --max-time, and one the store
# took and did not answer within it fails the step at once, by name; a store that is still
# starting refuses the connection at once instead, which the waits try again a second later. Each
# wait counts seconds, not tries, since a try can take its whole bound, and ends the step by name
# once its deadline passes. packages/envoy's TestTheStoreStepsRunTheRuleWholeUnderTheirBounds
# (internal/tests3/storesteps_test.go) holds its first statement to `set -eu`, each
# one-collection.sh line to a statement of its own and the S3 port's wait to portDeadline.
#
# Run from the repository's root, with $RUNNER_TEMP set:
#   .github/scripts/dispatch-browser-wait-for-store.sh
# CI runs it in dispatch-browser.yaml's shard job, at the step "Wait for the S3-compatible
# server", whose working directory is the repository's root.
set -eu

# s3 <seconds> <what> <curl argument>...: one request to the store, bounded by seconds.
s3() {
  seconds=$1 what=$2
  shift 2
  curl -q -g --proto =http -sf --max-time "$seconds" "$@" && return 0
  status=$?
  if [ "$status" = 28 ]; then
    echo "::error::the file store took the connection for $what and did not answer within $seconds s"
    cat "$RUNNER_TEMP/weed.log"
    exit 1
  fi
  return "$status"
}
# The S3 port's wait ends when the store answers; its 2 minutes decide only a store that never
# does. They are the shard job's budget rule over the slowest store start read in CI, 66.2 s
# (.github/ci-notes/dispatch-browser.md#why-the-stores-readiness-wait-writes-reads-and-deletes-an-object),
# and packages/envoy/internal/tests3/seaweedfs.go's portDeadline, the Go tests' bound for the same
# start, which TestTheStoreStepsRunTheRuleWholeUnderTheirBounds holds these seconds to.
deadline=$(( $(date +%s) + 120 ))
until s3 10 "the S3 port's first request" -o /dev/null http://127.0.0.1:18333/; do
  [ "$(date +%s)" -lt "$deadline" ] || { echo "::error::the file store's S3 port did not answer within two minutes"; cat "$RUNNER_TEMP/weed.log"; exit 1; }
  sleep 1
done
packages/envoy/internal/tests3/one-collection.sh prepare dispatch-e2e-objects "$RUNNER_TEMP/weed.log"
store=http://127.0.0.1:18333/dispatch-e2e-files
s3 10 "the bucket" -X PUT -o /dev/null -- "$store" || { echo "::error::the file store made no bucket"; cat "$RUNNER_TEMP/weed.log"; exit 1; }
deadline=$(( $(date +%s) + 60 ))
until s3 15 "the readiness object" -X PUT --data-binary ready -o /dev/null -- "$store/readiness"; do
  [ "$(date +%s)" -lt "$deadline" ] || { echo "::error::the file store took no object"; cat "$RUNNER_TEMP/weed.log"; exit 1; }
  sleep 1
done
# Read into a file, not a command substitution, whose subshell would keep s3's exit from
# ending the step.
s3 10 "the readiness object's read" -o "$RUNNER_TEMP/readiness" -- "$store/readiness" && [ "$(cat "$RUNNER_TEMP/readiness")" = ready ] ||
  { echo "::error::the file store did not serve back what it stored"; cat "$RUNNER_TEMP/weed.log"; exit 1; }
s3 10 "the readiness object's delete" -X DELETE -o /dev/null -- "$store/readiness" ||
  { echo "::error::the file store did not delete the readiness object"; cat "$RUNNER_TEMP/weed.log"; exit 1; }
packages/envoy/internal/tests3/one-collection.sh check dispatch-e2e-objects "$RUNNER_TEMP/weed.log"
