# Notes for the envoy-go job's steps

The derivations and the history of `.github/workflows/ci-job-envoy-go.yaml`, which points into
this file. Each `# why:` line of that workflow names the section below that holds the derivation
or the record its job or step rests on. A value's own arithmetic stays beside the value.

## The unit step's skip count and the tests it skips

Each leg's packages run in one `go test -json` through database-skips.sh --count-table, which
also answers c8's ask that the database broker suites run with BROKER_TEST_DATABASE_URL set and
that none skips: go test reports a skip as a pass, so the script counts the skips of the broker's
packages among the leg's, ./internal/cipher/..., its command, ./cmd/cipherd/, and the writer
hold's client, ./internal/holdrecord/, and fails on any there but the airlock's model
comparisons, which the script's EXPECTED_SKIPS names (each needs a built Lean model of the
airlock, and the airlock-lean job runs them). The auth service's natscallout runs in a leg here
too, beside its race run in envoy-go-race, since its hold rows open the broker's Postgres and skip
without it: this run counts their skips (`.github/go-legs.yaml#twice`). A report that shows no run
of one of those packages prints database_skips=UNKNOWN and fails too, a leg that holds none of
them prints database_skips=NONE, and a failed test of any package of the leg fails it. The skips
of the rest of the module are not counted; hold_envoy_legs holds every package of the broker's
table to a leg here, so each is counted in one.

The two tests of the web app's display-tree fixture are skipped: without `--check`
TestWebDisplayFixture writes the committed fixture rather than checking it, so a run here would
ask nothing of it, and TestTheWebFixtureFindsItsFilesFromAnyDirectory stats that same file, which
would make it a read of this job; envoy-go-build runs both, the first with the check, and both
texts are built from the same list. So are the backend count's tests of cmd/cipher and
internal/cipher/api, which envoy-go-skips' count step runs in the same build and env
(BACKEND_COUNT_TESTS), and the rows whose bound is on time, which envoy-go-alone runs on a runner
nothing else loads (UNIT_ALONE_TESTS).
