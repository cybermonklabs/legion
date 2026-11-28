# Notes for the envoy-go job's steps

The derivations of `.github/workflows/ci-job-envoy-go.yaml`, which points into this file. Each
`# why:` line of that workflow names the section below that holds the derivation its step rests
on. A value's own arithmetic stays beside the value.

## The tests the race build cannot run at all or at full size

The step "Test what the race build cannot run at all or at full size" of envoy-go-race-impossible.

The tests the race detector makes impossible that measure no time, which the race job of each of
these packages therefore cannot make, so envoy-go keeps a plain run of exactly them: internal/bus
counts HoldsSeed's allocations, which the race runtime's own allocations would be counted into; the
API's priorities read's four tests run over a corpus of production's counts (13,298 issues, 678,943
events), whose seed is heavy work, and the bound's test prints read times, which the detector
stretches; and the documents package measures the heap a room keeps, which the detector's shadow
memory and instrumentation move. Those are out of the race build altogether (//go:build !race), and
the two render-reuse storms run fewer seeds and fewer edits under it (raceEnabled), so this is their
full-size run. The API's line runs the four on the Postgres the step above starts
(DISPATCH_TEST_DATABASE_URL): the bound's test reads each statement's database time from
pg_stat_statements, which a server loads only at its start, and the service starts on the one
command line every Postgres service is held to, which preloads nothing. The four assert no time: the
bound's test holds the answers of the reads it times and their shape under each plan (the
statements, the round trips and the event probes) and prints their times, which c-both-forms' step,
outside CI, holds to the bound, and the other three hold answers, walks and buffer ratios on clones
of the corpus the first of them builds once per test binary (storetest.OpenFrom), so they run in its
go test, on the same server. On a four-CPU build pod (c4d-highmem-8, INFRA-5791), the bound's test
(TestThePrioritiesReadAnswersWithinItsBoundOnEveryPlan) took 47.1 s for the corpus's build and its
840 reads, TestThePrioritiesReadKeepsItsPlansWithAProjectNamed 1.4 s,
TestThePrioritiesReadWalksIssuesOnceOnTheCorpus 1.3 s and
TestThePrioritiesReadAnswersAsTheMergeBaseAnswers 4.5 s, 54.4 s for the package's run. The documents
package's line runs under Go's cleanup detector (GODEBUG=checkfinalizers=1), as envoy-go-race-docs'
run of the rest of it does, since it is the one package that registers a cleanup (ci_job_reads.py's
CLEANUP_DETECTOR), and with -v, so its TestMain's count of services left alive prints. The rows of
that run that measure time (the xid window tests, the xid rig, the API's issue-list bound and the
auth service's restart storm) run alone in envoy-go-alone. The API's mention sender claim model is
out of the race build too, and runs in a job of its own, envoy-go-claim-model; the API's ten source
guards, and one_expression_guard_test.go beside them, carry that tag as well and are deliberately
not here: they read the module's source, the text-check job runs their twenty-four tests by name,
and ci_job_reads.py's CLAIM_MODEL_TESTS and TEXT_CHECK_TESTS leave exactly those tests out of this
step rather than have each of them run twice. The -run lists are derived in
.github/scripts/ci_job_reads.py from the race lane's own packages and those files' //go:build !race
and raceEnabled, so a test added to either and missing here fails the ci-filters job naming this
step. 10 minutes each is ample.

## The container-backed NATS matrix

The step "Test the container-backed NATS matrix (natslive)" of envoy-go-natslive.

internal/testnats runs its server in the test process, so its untagged tests, which a unit leg of
envoy-go-legs runs, start no container. The real-server tests, a rendered authorization on the image
the hub runs among them, are the one thing an in-process server cannot reproduce: they sit behind
the natslive tag, and this step is where they run (INFRA-250). internal/bus has tests of its own
there. One is the consumer cap: the module links nats-server v2.15.1 in process, which caps a stream
that sets no cap at a default of its own and takes a change of MaxConsumers on a stream that already
exists, so the raise of an uncapped ENVOY_NOTIFICATIONS to the consumer cap this binary compiles and
the keeper's restore of a cap an operator lowered or unset are proved in process. What is left for a
real image is the pair of 2.10 cases - the stream with no cap the binary must leave alone, and the
refusal of the raise of a lower cap, naming the field, the cap and the first server taking the
change - and the hub's own reconcile on the nats:2.15.1-alpine image the deployment runs
(INFRA-1384). The others are a session consumer's own messages behind other sessions' traffic on the
hub's image, the listener's refusal of nats:2.10 (natsversion), and that refusal's control: on 2.10,
stepped past, the consumer loses its old-form message. internal/cipher/natscallout has one too: that
the image the hub runs lets a user with an empty publish allow list publish anywhere, the fail-open
its guard refuses, so the guard is shown able to fail on the server production runs. natsgrant and
cmd/listener have the edge listener's: the server refuses the edge's user a Dispatch event, and the
edge listener starts, relays a send and is reached by a role message through a shared listener under
its rendered grant, with no violation, and ends its start, naming its inbox prefix, under a grant
without it; a role message of the edge reaches no holder of another project's role; and the relay
socket's rows, most of the step's time, run two edge listeners and the real broker with 30 s tokens,
and carry frames and receipts, refuse other sessions' subjects, and close the socket at a token's
expiry, a revoke, a Reset, a person's removal, a changed answer and an unreachable broker. Each
package runs only the tests its tag adds: `-tags natslive ./pkg` would re-run the package's untagged
tests, which already run elsewhere - internal/testnats and natsgrant in a unit leg, internal/bus and
cmd/listener in envoy-go-race, and internal/cipher/natscallout in both - so the -run comes from
.github/scripts/go-tagged-tests.sh, which reads the names off the source at run time, and no test
name is spelled here. Each assignment is a line of its own: under bash -e it carries the script's
exit status, so an empty or refused list fails the step instead of running -run "", which matches
every test.
