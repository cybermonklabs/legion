# What this repository reads out of a gotestsum --jsonfile: the `go test -json` events of every
# attempt of a run, one after the other, with no attempt number on any of them (gotestsum writes
# each event as `go test` printed it). An attempt of a package begins at that package's `start`
# event: the first run prints one for each package, and each rerun of a failed test prints one
# more for the package it reruns.
#
# Read by .github/scripts/go-test.sh and .github/scripts/ci-ok-retries.sh (no_test_failures),
# packages/envoy/scripts/database-skips.sh, packages/envoy/scripts/backend-count.sh and
# packages/envoy/scripts/forced-order.sh (last_attempt_results), and .github/scripts/go-test.test.sh
# (with_attempts), each as `jq -n -L .github/scripts 'include "go-test"; <def>(inputs)' FILE`: each
# def reads the events as a stream, one at a time, so a jsonfile of a whole module's verbose output
# is never held whole; what each keeps is one entry per package attempt or per test.

# with_attempts(events): each event, given `_attempt`, the number of its package's `start` events
# up to and including it: 1 for the first run, 2 for the first rerun of that package. An event
# before any `start` (a build's output, which names its package by ImportPath) is attempt 1.
def with_attempts(events):
  foreach events as $e ({};
    if $e.Action == "start" and $e.Test == null and $e.Package != null then .[$e.Package] += 1 else . end;
    $e + {_attempt: ((.[$e.Package // ""] // 0) | if . < 1 then 1 else . end)});

# no_test_failures(events): each attempt of a package that failed with no test failing, no test
# left running at its end, and no build failure, as {package, attempt}, in the order the attempts
# began. Such a failure is a TestMain check that runs after the tests pass, or a wrapper's own
# exit, and a rerun of it runs no test (gotestsum's `-test.run=^$`), so a check that needs the
# tests to have run passes there: the attempt that failed is what decides.
def no_test_failures(events):
  reduce (with_attempts(events | select(.Action != "output")) | select(.Package != null)) as $e ({order: [], at: {}};
    "\($e.Package)\t\($e._attempt)" as $key
    | (if .at[$key] == null then
         .order += [$key]
         | .at[$key] = {package: $e.Package, attempt: $e._attempt, failed: false, build: false,
                        test_failed: false, running: {}}
       else . end)
    | if $e.Test != null then
        if $e.Action == "run" then .at[$key].running[$e.Test] = true
        elif $e.Action == "fail" then .at[$key].test_failed = true | .at[$key].running |= del(.[$e.Test])
        elif $e.Action == "pass" or $e.Action == "skip" then .at[$key].running |= del(.[$e.Test])
        else . end
      elif $e.Action == "fail" then
        .at[$key].failed = true | .at[$key].build = ($e.FailedBuild != null)
      else . end)
  | .at as $at
  | .order[] | $at[.]
  | select(.failed and (.build | not) and (.test_failed | not) and (.running | length) == 0)
  | {package, attempt};

# last_attempt_results(events): for each test of each package, and for each package itself (`Test`
# null), the event that ended it in the last attempt that ran it: a rerun's result replaces the
# first run's, and within one attempt a failure outweighs a pass, so a test a binary runs twice
# (-count, or a TestMain that calls m.Run twice) and fails once is failed.
def last_attempt_results(events):
  reduce (with_attempts(events | select(.Action == "start" or .Action == "pass" or .Action == "fail" or .Action == "skip"))
          | select(.Action != "start")) as $e ({};
    "\($e.Package // "")\t\($e.Test // "")" as $key
    | if .[$key] == null or .[$key]._attempt < $e._attempt then .[$key] = $e
      elif .[$key]._attempt == $e._attempt and ($e.Action == "fail" or .[$key].Action != "fail") then .[$key] = $e
      else . end)
  | .[];
