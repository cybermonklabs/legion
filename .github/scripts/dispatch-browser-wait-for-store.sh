#!/bin/sh
# The readiness wait for the S3-compatible server the shard job starts for its stacks' uploads.
# Once the S3 port answers, it grows the store's one collection before anything is written
# (packages/envoy/internal/tests3/one-collection.sh prepare), makes the bucket, writes one object,
# reads it back and deletes it, then checks that no write grew the store (one-collection.sh
# check). For whichever of those did not happen it prints an ::error:: line and the server's own
# log, and exits 1.
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
# once its deadline passes. packages/envoy's TestEveryWaitAndCheckOfTheScriptHasACase
# (internal/tests3/storesteps_test.go) parses this file as POSIX shell and holds it to those rules.
# It fails on a command off its list (wget, tail, weed shell, .), a curl but s3's (-q: no .curlrc;
# -g: no URL lists; --proto =http: no file://, which --max-time does not end), an s3 call of 0
# seconds or with more than -o, -X, --data-binary of literal text and one URL, a wait but an until
# right after its deadline, or shell it cannot read.
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
# (.github/ci-notes/dispatch-browser.md#why-the-stores-readiness-wait-writes-reads-and-deletes-an-object).
deadline=$(( $(date +%s) + 120 ))
until s3 10 "the S3 port's first request" -o /dev/null http://127.0.0.1:18333/; do
  [ "$(date +%s)" -lt "$deadline" ] || { echo "::error::the file store's S3 port did not answer within two minutes"; cat "$RUNNER_TEMP/weed.log"; exit 1; }
  sleep 1
done
packages/envoy/internal/tests3/one-collection.sh prepare dispatch-e2e-objects "$RUNNER_TEMP/weed.log"
store=http://127.0.0.1:18333/dispatch-e2e-files
s3 10 "the bucket" -X PUT -o /dev/null -- "$store" || { echo "::error::the file store made no bucket"; cat "$RUNNER_TEMP/weed.log"; exit 1; }
# The write is retried: the bucket exists as soon as the filer answers, while the
# volume server the object lands in can still be coming up behind it.
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
