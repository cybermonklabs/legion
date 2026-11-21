# Envoy diagnostic scripts

## e2e-local.sh

Runs the local listener against a throwaway NATS server it builds and runs
itself (`internal/testnats/cmd/natsd`), so it needs no container runtime. It
posts signed public GitHub fixtures and a three-paragraph direct message,
checks the one-warning response for a never-seen GitHub repository, reads the
registered session back by id from `/v1/registry/<session_id>` and an id that
never registered as `404 {"error":"no live session <id>; this listener does
not know when it last registered","state":"gone"}` — dispatch://INFRA-392 puts
the state the refusal was decided on beside the kept `no live session <id>`
prefix, with a `last_seen` only when this listener still remembers a
registration, and, when it remembers none, those words in its place rather
than a missing field, so a sender can tell a session this listener watched
reach the gone horizon from one it has never heard of — rejects an unheld
role publish before claiming it for a stand-in session that takes every
delivery from its own NATS consumer, refuses a registration naming a port,
captures the raw notification envelopes, and proves that the shared TypeScript
renderer prints the full direct message once, with no separate summary line
repeating its first line.

It also proves the three answers a sender gets when a send cannot be delivered:
a send to a session the store does not hold is refused `404 {"error":"no live
session <id>","state":"gone"}`; each send leaves exactly one `listener send`
record in `listener.log`, INFO when accepted and WARN when refused, carrying
`target_session`, `event_id`, `source`, `source_session`, `payload_summary`,
`outcome` and `status`; and a send whose bus is gone is refused `503` with
`{"error":"envoy: the message bus did not answer: …"}` and `Retry-After: 1`
inside the sending client's five-second deadline, both over curl and through
`createEnvoyClient`, which raises an `EnvoySendUnconfirmedError` naming its resend key, with
the listener's 503 `EnvoyApiError` as its last answer, rather than timing out. The
bus-down step runs last, because it ends the run's NATS.

```bash
packages/envoy/scripts/e2e-local.sh
```

The run writes its reusable evidence to `packages/envoy/out/e2e/`:

- `envelopes.jsonl` — raw NATS envelope frames
- `session-frames.jsonl` — the envelopes the session took from its own consumer
- `rendered-ts.txt` — `renderInbound` output for every envelope
- `listener.log` — the listener's own log, including one record per send and publish
- `absent-session-response.json` — the 404 body naming the session nothing registered
- `bus-down-send-response.json`, `bus-down-send-headers.txt`,
  `bus-down-send-result.txt` — the bus-down 503 body, its headers, and its
  status and `time_total`
- `bus-down-client-error.json` — the `EnvoySendUnconfirmedError` the real client raised for it

The OS chooses the NATS and listener ports, and each URL is built from the port
its own process reports. `E2E_NATS_PORT` or `E2E_PORT` sets a port explicitly:
it is used as given, and one already in use fails the run with `address already
in use`. The session opens no port: it registers with none, which a listener
now refuses above 0, and takes its messages from its own NATS consumer. The
NATS server is a process of this checkout's own build, stopped on exit with the
listener and the two fake clients. CI runs it as the `envoy-e2e-local` job of
`.github/workflows/ci.yaml` on every change its `envoy_e2e_local` filter matches.

## e2e-refill.sh

Proves on the real listener and a NATS server of its own (`internal/testnats/cmd/natsd`, its file
store under the output directory) that the listener keeps its copy of a bucket while it reads the
bucket again, and that its two list routes refuse, with no active-sessions sample, while it cannot
stand behind that copy (INFRA-261). `internal/testnats/cmd/refillrig` does the work a shell cannot
do quickly: it seeds the session and interest buckets, through the listener's
`POST /v1/interests/subscribe` and straight into the buckets, deletes and recreates a bucket,
polls `GET /v1/sessions`, `GET /v1/interests/` and `/metrics` as fast as they answer, and judges
each scenario from those polls beside the listener's own log lines.

```bash
packages/envoy/scripts/e2e-refill.sh [--scenario reread|interrupt|refuse|recreate-live|all]
```

The listener's watcher reads its bucket's stream every 2 s, so it finds a recreated bucket itself
and starts the read of the new bucket at once. The `reread` and `interrupt` scenarios observe that
read of a whole bucket, so the rig stops the listener (`SIGSTOP`) while it deletes, creates and
fills the bucket, and continues it once the bucket is full, just after a tick of the listener's
30 s self-health monitor, so no tick falls while the listener is stopped.

- `reread`: registers 3,000 `old` and 3,000 `both` sessions, then recreates the session bucket
  holding `both` with changed titles and 3,000 `new`, the interest bucket the same way with changed
  topics, and the session bucket again with the very records it holds (only its creation time
  changes). Each new bucket carries 100,000 delete markers among its records, so the read outlasts
  many list answers. Every `old` and `both` row must be listed until the read's completed
  `cache warm-up` line, a delivered row never goes back, and the list is exactly the bucket after
  it.
- `interrupt`: on a 41,000-record session bucket, stops NATS the moment the read of the new bucket
  begins (its `bucket was recreated; its cache is re-read` line), either with `SIGSTOP` until the
  scan times out (or the watcher ends) and then `SIGCONT`, or with `SIGKILL` and a restart
  (`--interrupt-by stop|restart`, both by default). Nothing may be removed until a later read,
  which the listener starts itself, completes and sweeps; the verdict names which incomplete
  outcome the log shows.
- `recreate-live`: recreates the session bucket under a running listener, 26 to 28 s before a
  self-health tick, so the tick cannot be what reads it, in two shapes: an old stream shorter than
  the new one, and one longer (the old bucket's records written three times). The old bucket has a
  60 s `MaxAge` (the listener adopts a bucket's), 1,000 `same` and 1,000 `chg` sessions, and `hb`,
  whose old record expires 3.5 s after the recreate; the new one holds `same` unchanged, `chg` with
  changed titles, 1,000 `new` and `hb` renewed. The recreate lands between two of the watcher's
  stream probes, and NATS is stopped for 4 s from the watcher's `bucket was recreated under its
  watcher` line, so `hb`'s old record expires after the copy stopped following and before the read
  delivers its renewal. Each shape passes when the watcher logs that finding and completes the read
  of the new bucket before the tick, with no self-health failure or rebuild; every `GET
  /v1/sessions` answer lists every record of the old copy, `hb` included, until the read completes;
  none answered before a line says the copy stopped following lists records only the new bucket has
  while lacking others it held (a consumer nats.go moved onto the new stream resumes past the
  sequences the old one covered); and once the read and the fill are done the list is exactly the
  new bucket. A listener that finds a recreate only by the self-health tick's `Check` fails both
  shapes: the longer one shows nothing of the new bucket before the tick, the shorter one part of
  it. `refillrig recreate -fill-log` records when each record of the fill was acknowledged, so a
  read that begins before the fill is done is judged against what the bucket held.
- `refuse`: a start on 40,000-record buckets, where every list answer is the refilling `503` with
  `Retry-After: 5` or the whole bucket and no gauge has a sample while its list refuses; NATS down
  past the 2-minute stale bound, where both lists answer the refilling `503` and `/metrics` has
  neither gauge's sample, then a complete `200` within 30 s of NATS's return with both gauges equal
  to the lists; and empty buckets, which answer `200 []` and `0` samples. The outage waits out the
  real bound, which the listener has no setting for, so the scenario takes about four minutes.
  `--during-refuse <command>` runs while both lists refuse and `--after-refuse <command>` once they
  are whole again, each with `bash -c`, `ENVOY_URL` and `REFILL_RIG_ROLE`, `REFILL_RIG_HOLDER` (a
  registered session whose topics make it that role's holder), `REFILL_RIG_SESSIONS` and
  `REFILL_RIG_DIR` exported, so another client (an MCP host, Dispatch) is checked in the same
  window; their output is in `refuse/during-refuse.log` and `refuse/after-refuse.log`.

Each scenario prints `verdict <scenario> PASS <what it saw>`, `FAIL <reason>`, or
`INVALID <reason>` when the run missed the window it was there to observe (no poll inside the read,
a scan that finished before the interruption, a read that began before the bucket was filled, a
self-health tick inside the read of `recreate-live`), which is never a pass. It exits 0 when every
verdict passes, 1 on a `FAIL`, and 4 on an `INVALID` with no `FAIL`. `--listener-bin` and
`--natsd-bin` run other builds, such as the merge base's for a negative control. The evidence (each
scenario's listener and NATS logs and every poll, one JSON line each) goes to `--out` (default
`$TMPDIR/envoy-e2e-refill`). `E2E_REFILL_NATS_PORT` (14232) and `E2E_REFILL_PORT` (19030) move the
ports. A full run takes about ten minutes, so it is a manual proof, not a CI job.

## dev-airlock.sh

Brings up the whole local surface the airlock's checks run on, with no container
runtime and no cluster, and prints the exports a check needs. It runs
`dev-broker.sh` for Postgres, `dev-oidc-issuer`, the six built binaries and that
script's own broker, and reads what it printed, rather than starting any of that a
second time. On top it starts a `cmd/dev-proxy-stands` of its own (the stand-in
services S and T, the GitHub, Google and Microsoft endpoints and the AWS service,
and the certificate authority they are signed by), which is a second such process:
`dev-broker.sh` starts one too, for the GitHub its own `github-app` secrets mint
at, and this script points nothing at that one. It then writes a rules file and a
fake Secrets Manager naming each other, starts **two** broker processes on a
database of their own with the whole `BROKER_AIRLOCK_*` block, puts one TCP front
over both brokers' API ports and one over both proxy ports (`dev-airlock-stands
front`, round-robin per connection), and enrols one pod session the way the Legion
daemon does — a service login at `POST /v1/launcher-credentials/token`, then
`POST /v1/enrollments` of kind `pod` — so `agent-secrets self` answers `kind: pod`.
It keeps that session's lease alive for as long as it runs, with the renewer the
Legion worker shim gives a pod: `agent-secrets renew --pod-token-file` on the pod
token `dev-oidc-issuer` rewrites in place. The broker ends a session whose lease
(`BROKER_LEASE_SECONDS`, 900 seconds by default) passes unrenewed, and every grant
of it with it, so without the renewer a check that holds a grant for longer would
have its calls refused 407 midway.

```bash
DEV_BROKER_POSTGRES_URL="$LEGION_TEST_PG_DSN" packages/envoy/scripts/dev-airlock.sh
```

Everything it makes lives under `${TMPDIR:-/tmp}`, as `dev-broker.sh`'s work
directory now does. Each bound host (`s.airlock.test` and the rest) listens on port
443 of a loopback address of its own, because the airlock refuses a request whose
`Host` header is not its CONNECT target and a client writes the port into that
header for every port but 443. The airlock's secrets are owned by the service the
enrolled pod runs as, so its requests for them are granted at once; the
`azure-login` secret is human tier, and the exports name the approver and the
sign-in URL a check approves it with.

Options: `--keep-values` and `--no-kind`, the two negative controls
(`BROKER_AIRLOCK_TEST_KEEP_VALUES` and `BROKER_AIRLOCK_TEST_NO_KIND`),
`--idle-seconds N` (`BROKER_AIRLOCK_IDLE_SECONDS`; check k9's control sets 60),
`--one-broker`, and `--help`. A usage error exits 2.

`dev-airlock.test.sh` proves the driver's own decisions behind fake binaries — the
work directories under `TMPDIR`, two brokers with the whole airlock block on one
database, one front over each pair of ports, the options reaching the brokers, the
enrolment written beside the session key, the session's renewer, the usage error,
and the cleanup stopping everything it started — with no real Postgres, no network
and no Go build; CI runs it in the `envoy-go-checks` job
(`.github/workflows/ci-job-envoy-go-checks.yaml`).

`dev-airlock-lease.test.sh` proves the renewer works on the real stack: it starts
the stack with one broker and `BROKER_LEASE_SECONDS=91`, the shortest lease the
broker starts with at its default sweep, holds one 30-minute grant of
`AIRLOCK_DEMO_TOKEN`, and calls S through the proxy front at once and again three
leases later. Both calls must reach S with the real value, and the session must still
be the one enrolled, with a later lease. It prints a `PASS:` or `FAIL:` line for each
and ends `dev-airlock-lease: passed` or `dev-airlock-lease: failed (<n> of <m>)`. It
needs what `dev-airlock.sh` needs, takes the stack's start plus about five minutes,
and is not part of CI. Without the renewer, the second call is refused 407 "grant is
expired, revoked, or its session ended".

## airlock-check.sh

Runs one `before_merge` check of the airlock's verification document
(dispatch://INFRA-1352/artifact/verification-md) end to end and says whether it
passed, so the implementer who records the result and the tester who re-runs it read
the same evidence. It starts the stack the check needs (`dev-airlock.sh`, with the
options that check's negative control asks for), runs the check's steps, runs the
negative control, stops the stack, and prints a result block to paste into `dispatch
result` — what the check expected, what this run observed and what the control did —
then one `PASS:` or `FAIL:` line for each property the check's outcome sentence
names and one for its control, and last `check <id>: passed` or `check <id>: failed
(<n> of <m>)`.

```bash
DEV_BROKER_POSTGRES_URL="$LEGION_TEST_PG_DSN" packages/envoy/scripts/airlock-check.sh k3
```

`<id>` is `k1`, `k2`, `k3`, `k4`, `k5`, `k6`, `k7`, `k8a`, `k9` or `k15`.
`--keep-stack` leaves the stack and the run's work directory up to look at a failure
with, `--help` prints the usage, and a usage error exits 2. `k9` is a long run — ten
minutes of connections and two stacks, about twenty-five minutes in all; every other
check takes a few minutes.

No line it prints is a credential: a value is compared by its SHA-256 (the first 16
hex digits) and its length, and every line that compares one says so, which is a rule
of the verification document itself.

Four checks need more than the stack. `k1` scans the command's memory, and
`/proc/sys/kernel/yama/ptrace_scope` is 1 in a Legion pod, so only an ancestor of the
command may read its `/proc/<pid>/mem`: the scan is a small Python program
(`scripts/airlock/memscan.py`) that starts `agent-secrets` itself and scans the
descendant it started. `k6` and `k7` drive real clients and the real consumers:
`airlock-clients.sh` below installs `az`, `gcloud`, `pulumi` and `boto3`, and the
consumers (`scripts/az-broker`, `scripts/secret-run`, `shims/gh`,
`scripts/gcloud-broker` and the tier commands of `gws/accounts.json`) are fetched from
`sjawhar/dotfiles` at main with `gh api`. `k15` needs no stack: it builds the Lean
model with `lake`, prints its theorem statements and their axioms, runs the comparison
fuzz test for ten minutes, and runs the six mutants through `airlock-mutants.sh`.

Before starting `k7`, set these operator inputs. An absent or empty input is a usage
error (exit 2), reported before setup starts, and so is a broker name that is not
uppercase letters, digits and single underscores starting with a letter, the one form
the broker reads:

- `AIRLOCK_GCLOUD_SECRET`: the broker name of the operator's gcloud login, which the
  consumer's `scripts/gcloud-broker` reads, for example `GCLOUD_OPERATOR`.
- `AIRLOCK_GWS_READ_SECRET`: the broker name of the operator's Google Workspace
  read login, for example `GWS_OPERATOR_READ_OAUTH`.
- `AIRLOCK_GWS_ADMIN_SECRET`: the broker name of the operator's Google Workspace
  administrator login, for example `GWS_OPERATOR_ADMIN_OAUTH`.
- `AIRLOCK_GWS_TIER_KEY`: the account key in `gws/accounts.json` whose
  `tiers.read.command` the check reads, for example `operator`.

The check uses fake login values in its local store, each stored under the name the
broker reads its input's name by (lower-cased, each underscore a hyphen). These inputs
identify the consumer's existing broker names and account; they do not rename the
operator's logins. `bash packages/envoy/scripts/airlock-check.test.sh` calls the k7
fixture and Google consumer boundary directly under fakes. It checks required inputs,
fake broker rules, store names and consumer results without starting a stack.

The programs the checks drive are files, not text a run writes out. The three Go
clients are subcommands of `cmd/dev-proxy-stands`, the binary `dev-broker.sh`
builds and `dev-airlock.sh` runs: `probe-private` is `k4`'s private-address probe,
`probe-bound` is `k7`'s call to a bound host, and `load` is `k9`'s load driver and
the plain tunnel its baseline goes through. CI compiles and vets all three with the
rest of that command. The others are under `scripts/airlock/`: `k7-consumers.sh`
(the fixture and Google consumer boundary), `memscan.py`, `k6`'s boto3 steps
(`aws-boto3.py`), `k7`'s Slack poster (`slack.ts`) and `k7`'s git credential helper.

The grant's proxy password and placeholder stay out of a command's arguments, which
every process of the pod reads in `/proc/<pid>/cmdline`. `load` reads the proxy URL from
`--proxy-file` and the bearer from `--header-file`, and refuses `--proxy` and `--header`;
curl reads the proxy from a config (`-K`) and the header from standard input; and `k7`'s
git step reads the proxy from `GIT_CONFIG_*`. Each file is a `<(printf …)` path.

Where a check needs a secret the stack does not hold, the run extends the stack's own
fake Secrets Manager and makes every broker reread it. A control that needs other
rules or another proxy setting — `k4`'s, which turns the refusal of a loopback address
off for one host — is a second broker process on the stack's own database, authority
and stand-ins with that one setting changed, and the step's `HTTPS_PROXY` pointed at
it. Every broker the run starts of its own — that control's, `k7`'s and the ones `k8a`
expects to refuse to start — takes its environment from the running stack broker
(`/proc/<pid>/environ`) with the few variables it must change put in their place, so
none of them can be missing a setting `dev-airlock.sh` gave the stack's. `k7` goes
furthest with that device: its names are the consumers' own and its hosts are the real
services, so its broker has the stack's whole environment with its rules file, its two
ports and its public URL changed, and both `AGENT_SECRETS_URL` and the grant's proxy
point at it. `k5`'s control is a second stack under `--keep-values`, `k6`'s a second
stack under `--no-kind`, and `k9`'s a second stack under `--idle-seconds 60`.

`k6` approves its own human-tier grant the way a person does: a pod session has no
operator, so the run signs in at the stack's issuer, which signs the approver in with
no page to fill, reads the request's page and posts the decision back with an `Origin`
header. `k9` runs each broker with `GOMAXPROCS=1` by exporting it into the environment
it starts the stack with, since `dev-airlock.sh` has no option for it, and reads each
broker's CPU time from `utime` and `stime` in `/proc/<pid>/stat` and its peak memory
from `VmHWM` in `/proc/<pid>/status`, which is the quantity `getrusage` reports as
`ru_maxrss` for a process one can wait on — these two are `dev-airlock.sh`'s children,
not the check's.

It needs bash, curl, jq, go, python3, openssl, node and psql; `k2`, `k6` and `k7` also
need `gh` (`k2`'s own step and the negative controls of `k2` and `k7` run the
`agent-secrets` of the pinned release before the airlock, `OLD_CLI_DEFAULT_TAG` in the
script, which `AIRLOCK_OLD_CLI_TAG` overrides, and `k6`'s and `k7`'s consumers come
from `sjawhar/dotfiles`), `k6` and `k7` need `bun`,
`wget`, `git`, `uv` and the clients `airlock-clients.sh` installs, `k9` needs `git`,
and `k15` needs `lake` from the Lean toolchain the environment names. Each bound host
listens on one loopback address and port 443, so only one stack runs on a machine at a
time.

## github-narrowing-check.sh

Runs the four checks of `agent-secrets`' GitHub App narrowing that need a broker, a
GitHub and a session — `k1`, `k2` and `k4` of dispatch://INFRA-2687's verification
document, and `k1` of dispatch://INFRA-3099's — end to end on a local stack of its
own, and prints one `PASS` or `FAIL` line for each property they name, then `all <n>
assertions passed` when every one passed, so a reader tells a complete run from a
cut-off one.

```bash
DEV_BROKER_POSTGRES_URL="$LEGION_TEST_PG_DSN" packages/envoy/scripts/github-narrowing-check.sh
```

It starts `dev-broker.sh`, which holds the three `github-app` secrets and the
stand-in GitHub they are minted against, and enrols one pod session of the service
that owns two of them, as the Legion daemon enrols a worker pod, so each request for
one of those is granted without asking. `k1` spends a narrowed token at the stand-in
and reads back what the narrowed mint asked GitHub for; `k2` sets the stand-in to
refuse the mint (HTTP 422) and then to answer wider than the narrowing asked, at the
answer route the stack's banner names (`POST {"mode":
"ordinary"|"refuse"|"refuse-repository"|"refuse-permissions"|"widen"|"fail-once",
"repository": "<name>"}`), since one stack drives every answer this check needs;
`k4` drives the secret whose
release holds two installations, which no one token can be handed over for; and `k1`
of INFRA-3099 drives the third secret, the one whose request reaches a person, with
the stand-in refusing one repository, so that narrowing is refused before the
approval request is opened: the command does not run, the refusal names the secret,
that repository and the sentence it carries, its stderr names no page to approve
anything on, the broker's own `credential_requests` gains no record, and the
stand-in's records hold the search for the refused part and a revoke for every token
it minted. Each check runs its own negative control — INFRA-3099's is the same
request for a repository the stand-in reaches, approved on the broker's page as the
person does, by `approve_record` and `approve_watch` copied from `airlock-check.sh`
because each check script here stands alone — and every line names the exit status it
read, because a property that failed and a command that never ran look alike in a
transcript that holds neither.

No line it prints is a credential: the child reads its token out of its own
environment and prints the repository it asked for and the status it got, and a
refusal is printed as `agent-secrets` wrote it, which names no part of any token. The
child's `curl` calls carry no `--cacert` and no `--resolve`: the GitHub stand is
served over plain http on `127.0.0.1`, because the broker mints a type's value with
an HTTP client that trusts the host's own roots and nothing else.

`--keep-stack` leaves the stack and the run's work directory up to look at a failure
with, `--help` prints the usage, and a usage error exits 2.
`DEV_BROKER_POSTGRES_URL` is required: the stack is driven against the server it
names, and the record property is read from the broker's own database there, whose
name the script takes from `dev-broker.sh`'s own log and puts in place of that URL's
path, so no password is ever parsed out of a banner. It needs bash, curl, jq, go,
psql and setsid, and takes a few minutes once Go's build cache holds the stack's
binaries. On a cold cache `dev-broker.sh`'s six builds take longer than all the rest,
so the script waits for them under a bound of their own, up to 30 minutes, and then
for the stack's start under a short one. The stack runs in a session of its own and
is stopped as one process group, so a stop during the build — the script's exit, a
wait that ran out, Ctrl-C — ends that build with the stack.

## callout-check.sh

Proves, on the whole local stack behind the NATS auth service, what the hub lets a
connection reach: `check c-callout-live` of the NATS auth service's verification.
`dev-stack.sh --callout` is that stack: natsd behind the auth callout, the service in two
brokers on one database, and the listener and Dispatch connecting as services with the
NATS-audience tokens the issuer writes them. All of what `--callout` adds is in
`lib/callout-stack.sh`, which `dev-stack.sh` sources on every run; without the option each
addition is empty. The check prints one line per property:
a session subscribes its own delivery channel; it is refused another session's, in
nats-server's own words (`Permissions Violation for Subscription to "..."`); a connection
with no credential is refused, and a token neither broker issued is refused (a stand-in token
endpoint plays the other broker), each in nats-server's own `Authorization Violation`, the only
answer `natsprobe` exits 4 for; the hub lists the listener's connection authorized as
`listener/<its machine id>`; and with one broker killed, three new sessions are each
admitted within 2 s, the first counted from the kill. A session opened before the hub has
read the killed broker's close can be routed to the dead replica and refused at the hub's
authorization timeout, so the check opens the first session once the hub lists one
`envoy-broker-callout` connection fewer (both brokers' connections carry that name), and says
how long the drop took. The last line is `all 6 assertions passed` (exit 0) or `<k> of 6
assertions failed` (exit 1); a probe that reports nothing reads `UNKNOWN` and fails.

```bash
LEGION_TEST_PG_DSN=postgres://... bash scripts/callout-check.sh   # starts and stops its own stack
# or, in a shell that exported a `dev-stack.sh --callout` banner:
bash scripts/callout-check.sh
```

With nothing exported it starts `dev-stack.sh --callout` itself, passing on its own
arguments (`--database-url`, `--binaries`), and stops it on exit, which drops its
databases; against an exported stack it kills that stack's second broker, which the stack
outlives. Each probe is `natsprobe` (`cmd/natsprobe`), which tries any one subject by
hand too: as a session it trades the stack's pod token for a broker token, as a service
it presents `--service-token-file`, and `--no-credential` presents nothing.
`callout-check.test.sh` holds the verdicts to fakes, including the negative control:
`CALLOUT_CHECK_GRANT_ALL=1`, a service granting every session's channel, fails line 2, and
a hub that never drops the killed broker's connection fails line 6.

## backend-count.sh

Counts, for each test it names, the runs that passed on each store backend: the test's
own `aws` and `gcp` subtests (`onEachStore`, `cmd/cipher/secret_kind_test.go`),
so a store or use change cannot pass on one backend alone.

```bash
packages/envoy/scripts/backend-count.sh PACKAGE... -- TEST...
```

It runs only the named tests in only the named packages, `go test -json -count=1 -timeout 5m
-run '^(TEST|…)$' PACKAGE...`, each package a `./` path under `packages/envoy`, and prints
`TEST aws=N gcp=M` for each, then `go_test_exit=E`. It exits 1 when no package or no test is
named, when a package is not a `./` path, when a name is not a Go test name, when a named test
has no passing run on either backend (a test in none of the named packages included), or when go
test failed. It counts only the tests it names, so the `aws` and `gcp` subtests of other tests
cannot stand in for a new test's.
`GO_TEST_SKIP` is go test's `-skip` pattern: the negative control
`GO_TEST_SKIP='^(TestA|TestB)$/^aws$'` makes both lines read `aws=0` and the run exit 1.
`GO_TEST_TIMEOUT` replaces the 5-minute `-timeout`, which `ci.yaml`'s `envoy-go` job counts
in its time budget; the job runs it as "Count the store-form rows on each backend", over
`./cmd/cipher/` and `./internal/cipher/api/`, as those tests' one run: the job's unit
step and its database-skips step skip them by name (`BACKEND_COUNT_TESTS` in
`.github/scripts/ci_job_reads.py`).

## database-skips.sh

The one skip counter of the module's database-backed packages: it runs `go test -json` and
counts the tests that skipped, and fails on any. A database suite skips, which go test counts
as a pass and prints nothing for without `-v`, when its variable is unset, and a `t.Skip`
left in one skips it with the variable set.

```bash
BROKER_TEST_DATABASE_URL=postgres://… packages/envoy/scripts/database-skips.sh
DISPATCH_TEST_DATABASE_URL=postgres://… BROKER_TEST_DATABASE_URL=postgres://… \
  packages/envoy/scripts/database-skips.sh --var DISPATCH_TEST_DATABASE_URL \
  --var BROKER_TEST_DATABASE_URL --race --timeout 70m ./internal/dispatch/api/
packages/envoy/scripts/database-skips.sh --report REPORT.json [PACKAGE...]
```

A `PACKAGE` is a pattern under `packages/envoy`, `./dir/` or `./dir/...`. With none it runs
its default table, `./internal/cipher/...` and `./cmd/cipherd/`, less the packages its
`DEFAULT_EXCEPT` takes out of them: the race lane's packages under the broker's tree, of which the
auth service's `natscallout`, which opens no database, is the one today (`DATABASE_SKIPS_PATTERN`
in `.github/scripts/ci_job_reads.py`, which refuses the script when `DEFAULT_EXCEPT` says
anything else). That is one `go test -json -count=1 -timeout 15m` from `packages/envoy`, then it
reads that report. `--var NAME` names a variable the packages need, and repeats; with none it is
`BROKER_TEST_DATABASE_URL`. A named variable that is unset or empty prints
`database_skips=UNKNOWN` naming it, and nothing runs.
`--race` adds `-race`, and `--timeout D` replaces the `-timeout`, as `GO_TEST_TIMEOUT` does
when `--timeout` is not given; it bounds each package's test binary and not the run, since
the packages run several at a time, so one that starts late ends later. `--skip PATTERN` adds
go test's `-skip`; go test reports nothing for a test it leaves out, so that test is neither
run nor counted as a skip. `--report` reads a `go test -json` report instead and runs nothing,
and refuses the four options that shape a run.

It names the packages as `go list` does under the run's own flags, so a pattern spelled
`./cmd/./cipherd/` counts the package it runs. It prints `SKIP PACKAGE TEST` for each skip of a
listed package, then `database_skips=N` and, for a run of its own, `go_test_exit=E`. The tests
`EXPECTED_SKIPS` names in the script skip by their own design in every CI run, which sets the
database variables and none of what those tests need (a corpus, a seeded database, a built
Lean model), and are not counted; each line gives its reason, and a line without one
is refused. A report that is missing or empty, one in which a listed package with test files
holds no test that passed or failed (its tests all skipped, or its `TestMain` returned before
it ran them), and an `EXPECTED_SKIPS` line whose package is listed but whose test the report
does not hold, print `database_skips=UNKNOWN`, name what could not be read, and exit 1. A
failed test fails the count, read from a report as in a run. It exits 0 only when tests ran,
none skipped but the expected ones, none failed, and go test passed. A failure prints, on
stderr, each failed test, each test a failed package never finished, that package's output
from its first `panic: ` on, that of its tests that did not pass, and any line test2json files
under a test after that test's own `--- PASS` or `--- SKIP` (a timed-out binary's goroutine
dump and a data race report among them), the build output and, for a run of its own, go test's
stderr. A run of its own also prints each package's `pass|fail PACKAGE <seconds>s` on stderr.

`ci.yaml`'s `envoy-go` job runs it as "Count the database broker suites' skips", with
`--timeout 20m`, the unit step's own `-timeout`, `--skip` of the one test of
`./internal/cipher/api/` that the job's "Count the store-form rows on each backend" runs
(`backend-count.sh`, above), and a step limit of its own
(`timeout-minutes: 30`) that its time budget counts, and `envoy-go-race-api` runs the
Dispatch API through it as the second line above.
`database-skips.test.sh` holds it to five controls, which `envoy-go` runs as "Test the database
skip counter": a skip planted through a Go overlay in `./internal/cipher/api/`, `./cmd/cipherd/`
and `./internal/dispatch/api/` is counted; either variable unset or empty is UNKNOWN; a report
that shows no run (no test ran, only expected skips ran, or one package ran none) is UNKNOWN,
a failed test fails, a race report filed under a passed subtest is printed, and a skip in a
package spelled other than canonically is counted;
`--timeout 1s` ends a package's binary; and a copy with
one `EXPECTED_SKIPS` line deleted, in a temporary checkout, counts that test's skip. It needs
a scratch Postgres named by both `BROKER_TEST_DATABASE_URL` and `DISPATCH_TEST_DATABASE_URL`.

## aws-moto-check.sh

Runs the built `agent-secrets` against moto's AWS, with no AWS account:
`cmd/cipher/secret_moto_test.go`'s `TestTheBuiltCLIAgainstMoto` (build tag
`moto`), which creates IAM users `writer` and `nobody` in moto, writes them as profiles,
turns moto's IAM checks on, and runs the secret forms under each source of the AWS SDK's
own order: access keys in the environment, which outrank `AWS_PROFILE`; `AWS_PROFILE`;
`AWS_DEFAULT_PROFILE`; and the rest of the default chain (a container credentials
endpoint the test serves), reading each form's `using` line. A profile variable that names
a profile the AWS config does not hold stops the command before that endpoint signs in.

```bash
packages/envoy/scripts/aws-moto-check.sh [BINARY]
```

It starts `moto[server]==5.2.5` through `uvx` on a free loopback port under the account
the tests' key ARN names, runs the test with `AGENT_SECRETS_BINARY` set to BINARY (empty:
the test builds the CLI from this tree), stops moto, and exits with go test's status; the
test fails, and never skips, without `AGENT_SECRETS_MOTO_URL`. A release binary as BINARY
is the negative control. moto ignores a policy's conditions and refuses with an XML
`AccessDenied` rather than `AccessDeniedException`, so this proves no IAM policy, and
the refusal's text is proved in process only. It needs uv, curl, python3 and go.

## airlock-lean-axioms.sh

Holds the airlock model's theorems to the axioms Lean's own kernel admits, and is the
one copy of that check: `ci.yaml`'s `airlock-lean` job runs it, and so does `k15` of
`airlock-check.sh`, so what the model must rest on is written once, in this script.
How many theorems there are is written nowhere: this script and `k15` both count the
`#print axioms` lines of `Axioms.lean`, so a theorem added there is one both hold the
model to.

```bash
packages/envoy/scripts/airlock-lean-axioms.sh packages/envoy/internal/cipher/proxyrules/lean
```

It runs `lake env lean Axioms.lean` in that directory and writes what that printed to
its stdout, so a reader of either log sees the axioms. What is wrong goes to stderr
and exits 1: an axiom outside `propext`, `Classical.choice` and `Quot.sound`, a
`sorryAx`, a count of printed theorems other than the number of `#print axioms` lines
in `Axioms.lean`, or a `lake env lean` that failed. `--axioms-file PATH` keeps a copy
of the axiom text for a caller that reads it back, which is what `k15` does for its
result block; `--help` prints the usage, and a usage error exits 2.

## airlock-clients.sh

Installs the clients checks `k6` and `k7` drive — the Azure CLI, the Google Cloud CLI,
Pulumi with the Tailscale provider, and boto3 — each at a pinned version whose SHA-256
is checked before anything of it runs, under `${TMPDIR:-/tmp}`, and prints the exports
a check needs on stdout. A Legion issue pod has no Pulumi and no boto3, and a pod on a
worker image from before dispatch://INFRA-3257 has no `az` and no `gcloud` either. An
image since then carries both, and the script still installs its own, first on the
`PATH` its exports set, so the checks drive the same versions on every digest:

```bash
eval "$(packages/envoy/scripts/airlock-clients.sh)"
```

Everything it says about itself goes to stderr and every line of stdout is an export
(`PATH`, `CLOUDSDK_PYTHON`, `PULUMI_HOME`, `UV_CACHE_DIR`, `AIRLOCK_BOTO3_PYTHON`,
`AIRLOCK_PULUMI_TAILSCALE_VERSION`, `AIRLOCK_CLIENTS_DIR`), so the line above is the
whole of how a caller uses it; `airlock-check.sh` runs it that way for `k6` and `k7`.
A second run finds what the first installed and fetches nothing: each client sits
beside a stamp file holding the version and the digest the script pins, and a stamp
that matches is the install. `--dir DIR` installs elsewhere, `--force` fetches again,
`--help` prints the usage, and a usage error exits 2.

Every fetch is a download with a checksum, this repository's one toolchain pattern
(`.github/actions/install-elan/action.yml`,
`packages/daemon/docker/worker.Dockerfile`): the bytes are written to a temporary
name, their SHA-256 is compared with the pinned digest, and only then are they
unpacked or installed. A digest that does not match leaves nothing behind and ends the
run. The two Python clients are pinned that way rather than by `uv run --with
<package>==<version>`, which pins the version but checks no digest: their wheels are
fetched and checked, then installed with `uv` into an environment of their own, so the
client itself is the pinned and checked artifact and `uv` resolves its dependencies
from that wheel's own requirements.

## airlock-mutants.sh

Proves that the airlock's differential fuzz test and the Lean proofs catch a defect
in the decision code. Each file of `internal/cipher/proxyrules/testdata/mutants` is one
defect as a unified diff; for each, the script copies `packages/envoy` to a fresh
directory under `$TMPDIR`, applies that one patch there, and requires the copy to
fail — a `go-*.patch` under the comparison fuzz test over the committed seed corpus,
a `lean-*.patch` under `lake build` or, when it still builds, that same comparison
against the mutated model. A mutant nothing catches fails the run.

```bash
AIRLOCK_MODEL=<the built airlock-model> packages/envoy/scripts/airlock-mutants.sh [<name>...]
```

With no name it runs every mutant; a name is a patch's file name with or without
`.patch`. It prints one `PASS:` or `FAIL:` line per mutant and exits non-zero when
any was not caught, 2 when the model mutants need `lake` and it is not installed.
No patch ever touches this repository.

## listener-deploy-probe.sh

Watches an Envoy listener from a client's seat while its deployment rolls, and
judges whether any task refused `/v1` while it was in service. A rolling deploy
runs the old and the new task side by side, and a caller that resolves the
listener's name (Dispatch, the Legion daemon, every plugin) can reach either.
Every tick the probe resolves the name with `getent ahostsv4` (or takes the
tasks' addresses from `--targets`) and asks each address, and the name itself,
for `/healthz` and `GET /v1/sessions`. It needs bash, curl, getent and sed only
(no jq), so it also runs inside a listener task over `aws ecs execute-command`.

```bash
ENVOY_TOKEN_FILE=<path to the listener bearer> \
  packages/envoy/scripts/listener-deploy-probe.sh \
  --url http://envoy-listener.internal.example:9020 --duration 300
```

Start it, then start the deploy (for example an ECS `--force-new-deployment`),
and let it run past the old task's exit. Options:

- `--targets <ip,ip>` — probe these addresses instead of resolving the name,
  e.g. both tasks' addresses from `aws ecs describe-tasks`.
- `--token-file <path>` — the listener's `/v1` bearer (default
  `$ENVOY_TOKEN_FILE`, then `$ENVOY_TOKEN`). The bearer reaches curl in a header
  file, never on its command line.
- `--interval <s>` (default 1) and `--duration <s>` (default 300; `0` runs
  until Ctrl-C, which still prints the summary).
- `--send-to <session>` — also `POST /v1/messages/send` to that session at every
  target each tick, under one idempotency key per tick.
- `--dispatch-url`, `--dispatch-token-file`, `--dispatch-issue <KEY>`,
  `--dispatch-session <session>` (all four together), `--dispatch-mode btw|steer`
  (default `btw`), `--dispatch-every <n>` (default 10) — post a Dispatch issue
  message targeting the session every `n` ticks and record its delivery
  attempt's `state` (and `error`), or `http_<code>` (and Dispatch's `error`)
  when Dispatch refuses the post, which is how a Dispatch delivery during the
  overlap is observed.

Each tick prints one tab-separated line per target:
`ts target healthz_code healthz_status v1_code v1_error send_code dispatch_state`
(`000` when no whole answer came, with `v1_error` naming why for `/v1`: `no connection`,
`timeout`, `empty reply`, `connection reset`; `-` for a column that does not apply; the Dispatch
attempt rides the name's line). On exit it prints, per target, when it was
seen, how many ticks `/healthz` answered, and the `/v1` non-200 answers at the
ticks `/healthz` answered 200, counted by `code:error` with the first and last
time, then the sends and Dispatch attempts by outcome, then the verdict. A
signal ends the run the same way, whether it reaches the probe alone
(`kill <pid>`) or its whole process group, as Ctrl-C at a terminal and
`timeout -s TERM` send it. The group's signal also kills the request or the
sleep in flight. The target whose request it cut short, and each target after
it in that tick, print no line for that tick and are not counted; the summary
and the verdict cover every target line printed before it, and a Dispatch
message that tick had already posted.

### Verdict and exit codes

A target **fails** when `/v1/sessions` answers anything but 200 at any tick
where its `/healthz` answered 200 (whatever its `status`, `starting` included),
or when it never answers `/v1/sessions` 200 at all. A `/v1` request that times
out (the probe allows 3 s, less than Dispatch's 5 s client timeout), or whose
connection closes or resets without an answer (what Go's `net/http` does after
a handler panics), is a non-200 answer: `000:timeout`, `000:empty reply`,
`000:connection reset`. One that finds nothing listening right after `/healthz`
answered (curl exit 7) is counted in the summary, not judged a refusal: a
listener that stops closes its listening socket first, so the task stopped
between the tick's two requests. A target that never answers `/healthz` is
**unreached**: named in the summary, not failed (a stale A record during the
handover is one). With `--dispatch-*`, the run also **fails** when any Dispatch
message did not record `state` `sent`: a failed delivery attempt, or a post
Dispatch refused (`http_401:invalid bearer token` for a wrong Dispatch
bearer), and the verdict names each such outcome with its count.

| Code | Meaning |
|------|---------|
| 0 | At least one target answered `/healthz`, every target that did passed, and every Dispatch message recorded `state` `sent`. |
| 1 | A target failed, a Dispatch message did not record `state` `sent`, or no target ever answered `/healthz`. |
| 2 | Usage error, or every `/v1` answer of the run was 401 or 403: the bearer is wrong, not the listener. |
| 4 | A required tool is missing (`curl`, `sed`, or `getent` without `--targets`). |

A listener from before LEGION-456 keeps `/v1` closed until its durable binds,
so during a rolling deploy the replacement answers `/healthz` 200 `starting`
and `/v1` 503 `service starting` for most of a minute and the run exits 1. A
listener that opens `/v1` once its caches are warm passes.

The refilling answer is another cause. A listener answers `GET /v1/sessions`
`503` with `Retry-After: 5` and code `refilling` (`503:envoy: the agent list is
refilling; try again in a moment` in the summary) while it cannot stand behind
its copy of the session or interest bucket: before its first complete read of
the bucket (its closed `/v1` start gate gives this route the same answer, and a
cache warm-up that timed out opens `/v1` on a copy still being read), or once a
re-read has not completed for longer than its bound (2 minutes, or half the
bucket's `MaxAge` when that is shorter). Its `/healthz` answers 200 all the
while, so the target fails: a client that reached it then was refused the list
(INFRA-261).

`listener-deploy-probe.test.sh` proves the verdict over two fake tasks, one
Python process per case serving `127.0.0.1` and `127.0.0.2` on one port the
kernel chose, behind a fake `getent`, and that a `TERM` sent to the probe's
process group during its sleep or a request, or an `INT` during its sleep,
still ends in the summary and the verdict; CI runs it in the `envoy-go-checks`
job, whose steps are in `.github/workflows/ci-job-envoy-go-checks.yaml`.

## daemon-release-gate.sh

Says whether every production reader of a message name runs a build of cybermonklabs/legion at or
after a commit: the reading a pull request quotes before it moves a producer onto the new form of a
name (dispatch://INFRA-3862). A Legion daemon whose image predates the both-forms reader takes an
issue event named in the new form for another project's and acknowledges it with no fact, so a
producer that moves first stops the fleet's workflow in silence.

```bash
packages/envoy/scripts/daemon-release-gate.sh --deployment <owner/repo> <commit>
packages/envoy/scripts/daemon-release-gate.sh --deployment <owner/repo> --listeners <commit>
packages/envoy/scripts/daemon-release-gate.sh --deployment <owner/repo> --clients [--dotfiles <dir>] <commit>
```

`--deployment` names the deployment repository whose Pulumi stacks pin the readers' images; it is
required, and the gate compiles in no deployment's name. Each stack's config keys are under its
Pulumi project's name, which the gate reads from that stack's own `Pulumi.yaml` (`name:`): a stack
whose `Pulumi.yaml` is missing, declares no name, or declares one Pulumi refuses (anything but
letters, digits, `_`, `.` and `-`) prints `UNKNOWN` by name, and no name is guessed.

The deployment's Legion projects are the files of its `legion/projects/` (`<nn>-<KEY>.yaml`, one
mapping of a project's settings each, in file-name order), as the deployment's legion program reads
them. Only a deployment with no such directory (GitHub answers 404) still declares them in the
projects list of `legion/Pulumi.prod.yaml`, as that program did before the files. Any other failed
listing, a listing of 1,000 entries or more (GitHub lists no more, so it may have stopped short),
an entry that is no file (a link or a directory), a file with no download URL (GitHub lists a
submodule so), a file that is not one mapping, a file that program refuses (a name that is not
`<nn>-<KEY>.yaml`, or a `key` that is not that KEY), and a deployment that declares no project
print `UNKNOWN` naming what the gate read. A file GitHub serves with any encoding but base64 (it
serves a file of 1 to 100 MB with encoding `none` and no content) is a file the gate cannot read,
never an empty one. A project whose `key` is no project key (`[A-Z][A-Z0-9]*`, which the list is
held to as the files are), whose `replicas` is no count, whose `daemon` is no boolean, or whose
image is no plain reference prints `UNKNOWN` by name: it is never skipped, and its image is never
read cut short.

Without a mode it reads the image each Legion daemon runs: each project's `daemonImage` when
set, else its `image`; a project at `replicas: 0` or `daemon: false` prints `SKIPPED` by name. An entry at
`runtime: tmux` runs its daemon on the project's host VM from `host.legionWorker`'s legion, not from
its `image` (deployment `legion/host.go`), and the gate reads no image for it: it prints `UNKNOWN`
by name, as does an entry on any runtime but `kubernetes`, the default. Each pin's
`org.opencontainers.image.source` and `.revision`
labels are read with `docker buildx imagetools inspect`, an OCI image index one platform at a time
(its `.Image.Config.Labels` is `null`), its `unknown/unknown` attestation entries left out, every
platform agreeing. A pin built from `https://github.com/cybermonklabs/legion`, or from a name the
deployment records as the same repository — the `repositoryRenames` pairs of its Legion projects,
which the daemon reads as `repository_renames`, so a build made as `sjawhar/legion` before the
transfer is a build of this repository — passes when GitHub's compare of `<commit>...<revision>`
answers `ahead` or `identical`, its line naming the rename; a pin built from any other repository
is refused by name and never compared. A revision label that is not a commit id, 7 to 40 lowercase
hex digits, prints `UNKNOWN` and is never compared: GitHub's compare resolves any ref, so a label
naming a branch (`main`) would compare as wherever that branch points now.

With `--listeners` it reads the listener pins the same way: envoy-cluster's `envoyImage`,
dispatch's `listenerImage`, which that stack sets on its listener in place of `envoyImage`, the
Dispatch server's (a dispatch stack that sets none reads `UNKNOWN`, since its deployment requires
that key, deployment `dispatch/main.go`, and the listener runs no other image), and envoy's
`listenerImage` when the deployment declares the VM hub, `gcp:NatsHub`, whose listener is that
pin's only reader. The hub's removal deletes the pin with it, so the gate decides the hub from the
deployment's program, never from the pin: it fetches the deployment repository as one archive at
the head of its default branch and reads every Go file in it that is not a `_test.go`, outside
comments and strings, a link at its target. GitHub makes the archive as `git archive` does, which
leaves out each path a `.gitattributes` marks `export-ignore` and rewrites each it marks
`export-subst`, so the gate also reads the commit's tree (`git/trees/<sha>?recursive=1`) and holds
the archive to it: each Go and module file the tree holds must be in the archive, and each file it
reads must be the tree's blob, by git's own hash of it. A use of `NewNatsHub` anywhere but as the
name of the func that defines it, which Go declares only outside every brace, declares the hub, and
so does the hub's type token, `envoy-infra:gcp:NatsHub`, written as one string, anywhere but inside
the generated SDK's own `NewNatsHub` and as a `case` of its switch of the types it rebuilds: the
gate prints `VM HUB: <file>:<line> at <sha> uses NewNatsHub` (or `registers
envoy-infra:gcp:NatsHub`) and reads the pin, which, if absent, stays `UNKNOWN`. Used nowhere, every
file read, it prints `NO VM HUB: envoy/ at <sha> declares no gcp:NatsHub` and reads no VM pin. A
head, tree, archive or file it cannot read, a tree GitHub lists only in part, a file the archive
leaves out or changed, a Go or module file that is a Git LFS pointer, a hard link, a link that
leaves the repository, a submodule (a tree entry of type commit, or a `.gitmodules`), whose code
GitHub's archive does not hold, a local `replace` or `use` of `envoy/go.mod` or a `go.work` that
leaves it, and an `envoy/` with no Go file or no `go.mod` print `UNKNOWN`, so a missing pin never
reads as a hub that is gone. A stack pin that is no plain image reference prints `UNKNOWN` and is
never read.

With `--clients` it reads the clients that deliver to a session, each of which must carry the
client follow filter of `<commit>`, since an older one passes a frame in a form it does not match
in silence:

- In each `image` a running project names (each read once however many projects name it), and in
  the agent-session image the deployment's `sessions/` stack pins (`sessions/Pulumi.prod.yaml`
  `<project>:image`, `<project>` the name `sessions/Pulumi.yaml` declares; that image is built FROM
  the worker image and links its `/opt/legion/pi-envoy`), the two clients the worker build puts at
  `/opt/legion`: `/opt/legion/pi-envoy/package.json` and `/opt/legion/claude-envoy/package.json`
  must both be there, each the package it should be, and the image is judged by the commit its
  build ran at, which `/opt/legion/bin/legion version` in the same image prints (`legion <version>
  commit <sha>`). A manifest's version names no build: claude-envoy's keeps its source's static
  version, and pi-envoy's names a release only when a release build sets it. A `sessions/` stack
  or pin the gate cannot read prints `UNKNOWN` by name. The gate reads pins, not running pods: a
  session launched with an image of its own (a per-launch `--image`) runs clients outside them, and
  a pod keeps the image it started with until it is rolled or repinned, so right after a pin moves,
  `GATE OPEN` speaks for the pins while running sessions may still load the older pi-envoy.
- For each running project's daemon, its `daemonImage`, else its `image`. No session loads the
  clients of the daemon's own container, but the daemon installs `@sjawhar/pi-envoy@<version>` into
  a kept session pod on a stale-plugin relaunch (`packages/daemon/internal/daemon/kubernetes.go`,
  `ResolvedPlugins`): the project's `runtime.kubernetes.plugins` when its `legion.yaml` sets one,
  else the pair linked into the image's legion, which `legion version` prints on its second line
  (`plugins @sjawhar/pi-legion <v> @sjawhar/pi-envoy <v>`). That version is judged by the commit of
  its release tag, never by the image's build commit. The gate reads a project's `legion.yaml` from
  the deployment's rendering of it, `legion/testdata/<key in lower case>-legion.yaml`, which the
  deployment's `TestCommittedStackConfig` holds byte for byte to what its legion program renders
  (`renderLegionYAML`); the project files carry no `legion.yaml` key of their own. That program
  renders no `plugins` key today and refuses a project field it does not know, so the linked pair
  decides, and reading the rendering keeps the gate right if the program starts to set it. A
  rendering it cannot read or that sets no `runtime.kubernetes` (every kubernetes project's
  rendering holds it), a legion that links no pair where the rendering sets none, and a second
  line that names no pi-envoy version print `UNKNOWN`. A project with no `daemonImage` runs its
  daemon from its `image`, which is then judged both ways.
- Each command runs in the image with `docker run --rm --quiet --network none --read-only
  --cap-drop ALL --security-opt no-new-privileges --entrypoint`, and every text read from an image
  is printed with its control characters written as `\x<hh>`, so no image can print a line of the
  gate's own. An image not pinned by digest prints `UNKNOWN` and is never run: under a tag, docker
  runs whatever image it holds by that name. `docker run` pulls an image docker does not hold, and
  the gate leaves it there; each worker image is several gigabytes, so remove the ones a run pulled
  (`docker image rm <ref>@sha256:<digest>`) when you are done. A project on a tmux host prints
  `UNKNOWN`: its sessions run the host's clients.
- On this developer box, under `--dotfiles` (default `~/.dotfiles`): the `@sjawhar/pi-envoy` pin of
  `omp/plugins/package.json` and, where it differs, the copy installed beside it; each
  `claude-envoy@legion-plugins` install the dotfiles' Claude Code plugin record
  (`.claude/plugins/installed_plugins.json`) holds, by the manifest at its `installPath`, which must
  lie inside the dotfiles' `.claude`; and the claude-envoy version the marketplace `legion-plugins`
  serves to `.claude/settings.json`. The marketplace is read as Claude Code reads it: its GitHub
  `repo` at its `ref` (sent percent-encoded), the file at its `path`, by default
  `.claude-plugin/marketplace.json`, and the plugin from its npm `registry` when the entry names
  one, which must be an `https` URL (`--registry=<url>`); a `path` with an empty, `.` or `..`
  segment, and any other key of the marketplace source or the npm source, print `UNKNOWN`, since
  either may move what is installed. The marketplace's `version` is the spec npm installs from, and
  only a registry spec names a release: npm's own parser (the npm-package-arg npm ships) must call
  it a range, an exact version or a dist-tag. Any other spec (a directory, a git repository, a
  tarball URL, an alias) prints `UNKNOWN` before npm fetches it, since npm builds it from its
  source and runs its `prepare` script even under `--ignore-scripts`. The version is the one npm
  itself takes on this box: the gate runs `npm pack @sjawhar/claude-envoy@<range> --dry-run
  --ignore-scripts --json` in a directory with no `package.json` or `.npmrc` of a project, so npm
  applies every rule of its own pick under the box's npm configuration (the dist-tag its `tag`
  setting names, `before` and, on npm 11, `min-release-age`, deprecation, and the engines of the
  node and npm it runs on), and the gate judges the one version npm answers. The dry run fetches
  the package into npm's cache and writes nothing else. An answer that is not one package named
  `@sjawhar/claude-envoy` prints `UNKNOWN`.
  Each version must be an exact release version and is judged by the commit of its release tag,
  `pi-envoy-v<version>` or `claude-envoy-v<version>`. A settings file that enables no
  `claude-envoy@legion-plugins` prints `SKIPPED`. A pi-envoy in `~/.omp/plugins` or a profile's
  plugin tree that is not the dotfiles' copy, a claude-envoy recorded in `~/.claude` or another
  `CLAUDE_CONFIG_DIR`, a link that does not resolve, a directory the gate may not search, a
  marketplace entry it cannot read, and a marketplace that names claude-envoy twice print
  `UNKNOWN`. A session started before a pin moved still runs the plugin it loaded: the gate reads
  pins and installs, not processes.

It prints a line per pin or surface (`PASS`, `GATE SHUT`, `UNKNOWN`, `SKIPPED`, `VM HUB` or
`NO VM HUB`) and the verdict on the last: `GATE OPEN` (exit 0) only when every pin it read passed
and it read at least one, `GATE SHUT` (exit 1), or `UNKNOWN` (exit 3) for a pin, file, package,
version or comparison it could not read. Every text it reads from outside it (an image, the
deployment's files and archive, a marketplace, npm, the dotfiles, an error a command printed) is
printed with its control characters written as `\x<hh>`, so none can start or rewrite a line of
the gate's own. A usage error, a missing `--deployment`, both modes, or a `--dotfiles` without
`--clients` among them, exits 2 and reads nothing. It needs `gh` with read access to both
repositories, docker with buildx able to read each pin's registry, `jq`, python3 with PyYAML and,
for `--clients`, `npm`. Make each registry login into a `DOCKER_CONFIG` of your own, so the shared
one is untouched: for ghcr.io, a token holding `read:packages`, such as `gh auth token`, given to
`docker login ghcr.io -u x --password-stdin`; for the Artifact Registry images
(`<region>-docker.pkg.dev`), a Google login that may read them, in the same `DOCKER_CONFIG`, for
example `gcloud auth
print-access-token | docker login <region>-docker.pkg.dev -u oauth2accesstoken --password-stdin`,
or `gcloud auth configure-docker <region>-docker.pkg.dev` run with that `DOCKER_CONFIG` set, which
makes docker ask `docker-credential-gcloud` for each pull. With only the ghcr.io login, every
Artifact Registry image prints `UNKNOWN`. The VM hub, the images' clients and the developer box
are read by `lib/daemon-release-gate-read-vm-hub.sh`, `lib/daemon-release-gate-read-image-clients.sh`
and `lib/daemon-release-gate-read-developer-box.sh`, which the gate sources.

`daemon-release-gate.test.sh` proves every verdict behind a fake `gh`, a fake `docker` that
answers a single image and an index the way the real one does and runs an image's files, and a
fake `npm`. Its fakes, fixture builders and assertion helpers are in
`lib/daemon-release-gate-harness.sh`, and its scenarios of the VM hub, the images' clients, the
developer box, its versions and a project's fields in `lib/daemon-release-gate-cases-*.sh`, which
it sources; CI runs it in the `envoy-go-checks` job of `.github/workflows/ci.yaml`. Two controls run
with it: a copy of the gate whose VM leg reads an absent `listenerImage` as no hub, and one that
reads pi-envoy alone in an image, must each open on the fixture that exists to catch it.

## jetstream-account-probe.sh

Says whether a client of the clustered hub can still have a JetStream answer stored on a subject a
stream captures, the reading the JetStream account move closes once its switch places every client
in `ENVOY`, the account that reaches JetStream only through imports. It runs
`jetstream-account-move probe` from a one-off Job in namespace `envoy`, in the listener's own image,
as the listener Deployment's ServiceAccount and NATS credential (its seed file, or with
`--token-file` its projected token), and prints what the probe printed:

```bash
bash scripts/jetstream-account-probe.sh --context <kube-context> [--token-file <path>] \
  [--inbox-prefix <prefix>] [--remover-seed-secret <secret>] [--image <image>]
```

The probe's ordinary request, `$JS.API.STREAM.INFO.ENVOY_NOTIFICATIONS`, must come back with the
stream's configuration, and its control, a plain publish to `$KV.envoy_session_consumers.probe`,
must be refused; else it is `UNKNOWN`, since an identity refused everything stores nothing. Then the
same request, its reply aimed at `notifications.dispatch.issue.ZZPROBE.1`, must leave nothing
stored there. The last line is `GATE OPEN` (exit 0), `GATE SHUT` (exit 1) when anything was stored,
each sequence removed by the JetStream account's operator when `--remover-seed-secret` names the
Secret in `envoy` holding its seed under `seed` (`REMOVED`), or named as left (`LEFT`), or `UNKNOWN`
(exit 3), the Job made, read and deleted or not. The probe reads the stored count up to ten times,
200 ms apart, after a later request was answered: the listener's grant cannot subscribe the target,
so nothing else says the answer was sent.

`jetstream-account-probe.test.sh` runs the real probe behind a fake `kubectl` against in-process
hubs of each layout (`cmd/jetstream-account-move/testhub`, core's render as its goldens pin it): the
private render reads `GATE OPEN`; the merge base's layout `GATE SHUT`, with `REMOVED` or `LEFT`; an
identity denied the API and a layout missing the `$JS.API.>` import each `UNKNOWN`, never
`GATE OPEN`. CI runs it in the `envoy-go-checks` job.

## Measuring search on a copy of the corpus

Three scripts measure a change to Dispatch search against the real corpus rather than a seeded
one. The copy is production data: it stays on the machine that restored it.

`corpus-copy.sh` restores the newest nightly dump into a Postgres container of its own and prints
its `DATABASE_URL`. The bucket the dumps land in is the deployment's, so it is a required input:

```bash
DISPATCH_BACKUP_BUCKET=<bucket> packages/envoy/scripts/corpus-copy.sh
```

It downloads the dump under `DISPATCH_CORPUS_DIR` (default `$TMPDIR` or `/tmp`) and deletes it once
it is restored, whether or not the restore succeeded. `DISPATCH_CORPUS_CONTAINER` (default
`dispatch-corpus-pg`), `DISPATCH_CORPUS_PORT` (default `55433`, on `127.0.0.1`) and
`DISPATCH_CORPUS_DATABASE` (default `dispatch_corpus`) name the copy. Run again while the container
holds the database, it prints the URL and restores nothing; `docker rm -f -v <container>` deletes the
copy and its data volume.

`search-smoke.sh` prints what a searcher sees for each query in a file
(`search-smoke-queries.txt` is one): it starts `envoy-dispatch` against the copy, built from this
checkout or named with `--binary`, and prints each answer as one line of totals, a `took_ms` line,
and one `pos kind owner id snippet` line per hit. Two builds' runs diff, which is the before and
after of a ranking change. The server runs with a throwaway `HOME`, a made-up agent token and a
trusted identity header, so it reads no operator configuration and holds no credential, and with
NATS off. It migrates the database it is given, as every start does, so point it only at a copy.

```bash
DATABASE_URL=<copy> packages/envoy/scripts/search-smoke.sh \
  --queries packages/envoy/scripts/search-smoke-queries.txt [--binary <envoy-dispatch>] [--limit 20] [--offset 0]
```

`search-latency-compare.sh` compares a base checkout (usually `main`) with this one on the same
copy: it runs `TestSearchLatencyOnCorpus` from each, alternately, for five rounds by default, prints
every run's p50 and p95, and exits 1 when this checkout's median p95 is more than 10% above the
base's. A copy answers in whatever time the machine and its load allow, so the bound is relative,
never a fixed number of milliseconds.

```bash
DISPATCH_BENCH_DATABASE_URL=<copy> packages/envoy/scripts/search-latency-compare.sh <base-checkout> [rounds]
```

## build-dispatch-build.sh

Builds one Dispatch build from a commit of this repository into a directory, for the two-build
test (`cmd/dispatch/twobuild_*_test.go`, `go test -tags twobuild`), which runs it beside this tree
on one Postgres and one NATS, and for the browser suite's cross-build run
(`packages/dispatch/e2e/cross-build.multiprocess.e2e.ts`), which drives one build's page against
the other build's server. It extracts `git archive <commit>` into `<dir>/src`, so it makes no
worktree or branch, and builds each part as that commit's own image build does:
`bun install --frozen-lockfile`, the contracts' `gen:go` and `build`, `bun run build:web`, the
commit's own `packages/envoy-client/scripts/build-dispatch-cli.sh`, and `go build ./cmd/dispatch`
with `GOTOOLCHAIN=auto` and `-ldflags "-X main.buildCommit=<sha>"`.

```bash
packages/envoy/scripts/build-dispatch-build.sh --previous-release --out "$TMPDIR/dispatch-previous"
packages/envoy/scripts/build-dispatch-build.sh --commit <sha|tag> --out <dir>
```

`--previous-release` builds the newest `legion-envoy-v*` tag that is an ancestor of
`git merge-base HEAD origin/main`, so it needs `origin/main` and the tags fetched. `--commit HEAD`
builds the current branch the same way. The script refuses a commit that is not an ancestor of
`HEAD`, and a commit older than the release `legion-envoy-v30.11.0`, the oldest build either test
can run as the earlier side: that release carries migration 0117, from which two builds share one
database, and migration 20261111210103, after which every project it creates must name a default
assignee (`cmd/dispatch/README.md`, "Database migrations"), serves on a socket handed to it, which
the browser harness holds for it, and knows the one dev sign-in cookie key that run gives both of
its servers. `<dir>` must be absent or empty, or already
hold a complete build of the same commit, which the script leaves as it is. It then holds
`envoy-dispatch`, `web/` (the web app), `dispatch.js` (the `dispatch` command, run as
`bun dispatch.js`), `pmdoc/testdata/corpus/`, `pmdoc/schema/blocks.json` and `text/testdata/` from
the commit, and `COMMIT`, the full sha, written last. Point `DISPATCH_PREVIOUS_BUILD` at `<dir>` to
run the two-build test.

`--resolve` in place of `--out <dir>` builds nothing: it makes the same refusals and prints the full
sha the script would build, which CI's `dispatch-two-builds` job (`.github/workflows/ci.yaml`) keys
its cache of the build by before it builds.

```bash
packages/envoy/scripts/build-dispatch-build.sh --previous-release --resolve
```

## Measuring the related-item check on a project's duplicates

`related-recall.sh` copies one project's pairs of issues closed one into the other, and every other
issue of the project they compete with, from a Dispatch server into a JSONL file, for
`TestRelatedRecallOnDuplicates` (`internal/dispatch/api`). A pair is a done issue whose close
reason names one other issue that covers it: `Duplicate of`, `Duplicate:`, `Folded into`,
`Covered by` or `Superseded by`, in any case, followed by a Dispatch issue reference
(a Dispatch link to `<KEY>`, a bare `<KEY>` of one of the server's projects, or an `/issues/<KEY>` link).
The close reason is the issue's last top-level message posted at or before its close, the message
`dispatch issue-update --status done --reason` posts just before the close; the `issue.closed`
event carries no reason. A reason naming only a pull request, several issues or the issue itself
is not a pair. Two closes naming the same two issues make one pair, by the first close.

Each pair's query is its later-filed issue as filed: its first title and first spec version. That
is the closed issue when the issue its reason names came first (forward), and the named issue
when that came later (reversed). The target, the issue the check should list, is the earlier one,
its title and spec as of the query's filing. Pairs naming another project's issue, or one that
does not exist, are counted and left out. It reads with the caller's own token, sends only
`GET`s, one at a time, and prints counts only: done issues read, close reasons with a wording,
those that are not a pair by what they name (`no_reference` is a wording followed by no issue or
pull request), pairs found and kept per wording with forward and reversed, pairs left out by
reason, chains (a pair's named issue itself closed into another), and issues copied.

```bash
DISPATCH_URL=<server> DISPATCH_TOKEN_FILE=<token file> \
  packages/envoy/scripts/related-recall.sh <project> "$TMPDIR/recall.jsonl"
```

The file is production data: it is written with mode 600 and never committed. The test loads it
into a fresh test database, every issue open, under its own key, with its latest title and spec.
For each pair it sets the target's title and spec to their text at the query's filing, takes the
query out of the search, refreshes the term statistics, runs the check's old 40-word OR query and
its keyword search over the query's text, then puts both issues back. It prints each pair's
wording, direction, the target's rank in each search and whether each lists it among its three
related items, then the counts per wording and over every pair. An idf-weighted coordination rank
of the same terms is printed beside them as an experiment. `DISPATCH_RECALL_REQUIRE_NO_LOSS=1`
fails it if the keyword search lists fewer targets than the old query.
`TestRelatedCheckLatencyOnRecallCorpus` loads the same corpus and prints the keyword search's p50,
p95 and max over 20 runs for the text of the issue `DISPATCH_RECALL_LATENCY_ISSUE` names and for a
10,000-word spec, and the refresh's duration.

```bash
cd packages/envoy && DISPATCH_TEST_DATABASE_URL=<test database> DISPATCH_RECALL_CORPUS="$TMPDIR/recall.jsonl" \
  go test ./internal/dispatch/api -run 'TestRelatedRecallOnDuplicates|TestRelatedCheckLatencyOnRecallCorpus' -v -count=1 -timeout 60m
```

## Measuring Dispatch's statements and its write routes on the seeded database

These are INFRA-325's own measurements: every changed statement's plan and time (A8), and the
median wall time of the six write routes the new triggers slow (A9). Both run against the seeded
measurement database `cmd/dispatch-seed` fills, never against a test database and never against
production. `DISPATCH_BENCH_DATABASE_URL` is the one gate: CI leaves it unset, so CI runs no
measurement.

### The statement timings

`internal/dispatch/benchtime` is the runner, and each package that owns statement texts carries
its own `statement_timing_test.go`, all named `TestStatementTimings`: `internal/dispatch/api`,
`internal/dispatch/refs` and `internal/dispatch/docs`.
`Pair` takes a named pair of statement texts with their arguments, runs `EXPLAIN (ANALYZE, BUFFERS)` of each alternately on one connection, reports planning plus execution as the median of runs 2 to 12, prints both plans and fails when the after text's median is not below the before text's.
`Report` is that same two-sided reading with no bound, for a statement whose gain is real but smaller than the run-to-run spread of the parts it shares a statement with: the gate then sits on the piece whose text differs, as a pair of its own.
The runner records the server settings it found and the subjects it bound (the largest project, its most heavily linked issue, the most anchored document, five sessions, an embedding and three words), each read from the database at run time, so a re-seed moves them with it.

```bash
cd packages/envoy && DISPATCH_BENCH_DATABASE_URL=<seeded database> \
  go test -run TestStatementTimings -v -count=1 -timeout 90m \
  ./internal/dispatch/api ./internal/dispatch/refs ./internal/dispatch/docs
```

A statement whose gain needs an index or a table a migration of this release adds has no such pair, because one schema cannot hold both sides of it.
`Before` and `After` measure one side of such a statement alone, with its plan and no bound, each line naming the side it read: the after side on a database at this release's schema, the before side on one migrated from the release before it.
Running this same test against each of the two databases is what puts the two sides together.
Each test file says which statement is under which protocol and why.

`TestRelatedTermsRefreshTiming`, in `internal/dispatch/api`, measures the related-terms refresh,
which has no statement pair: it times one whole pass of the release before this one, one whole
pass of this build with no change, and eleven rounds each preceded by a hundred item writes
outside the timed part, and reports the median of the eleven. It writes 1,100 items to the
database it measures and leaves them there, so run it after the statement timings.

```bash
cd packages/envoy && DISPATCH_BENCH_DATABASE_URL=<seeded database> \
  go test -run TestRelatedTermsRefreshTiming -v -count=1 -timeout 90m ./internal/dispatch/api
```

### The write routes

`route-timings.sh` times the six write routes against one running server: the median wall time of
50 requests to each of `POST /api/v1/issues/{key}/comments`, `.../asks`, `.../messages`,
`.../artifacts` under a name that already exists, that same upload over a 100-660 KB document,
and `PATCH /api/v1/issues/{key}` with a title alone. It prints one tab-separated line per route -
the label, the route, the median in milliseconds and every reading - so a reader sees the spread.

The caller gives it the base URL and the issue key, so one command measures the base server and
the head server, and the plan's three base-and-head pairs are a loop:

```bash
for round in 1 2 3; do
  packages/envoy/scripts/route-timings.sh --url "$BASE_URL" --issue SEED1-1 --label "base-$round" >>runs.tsv
  packages/envoy/scripts/route-timings.sh --url "$HEAD_URL" --issue SEED1-1 --label "head-$round" >>runs.tsv
done
packages/envoy/scripts/route-timings.sh --summarise runs.tsv
```

`--summarise` prints each route's runs, the highest base run - what a rise is judged against,
since a rise inside the spread of the base runs is no rise - and each head run against it.
`DISPATCH_USER` names the caller (default `bench`), and the server must take it as a human
identity. The script writes 50 comments, 50 asks, 50 messages, 100 document versions and 50 title
edits for every run, which is what A9 measures: run it after the statement timings, and re-seed
before anything reads a row count again. `--requests`, `--body-bytes`, `--artifact` and
`--large-artifact` are the knobs; `--help` lists them.

## coverage-scale.sh

Measures the search coverage statement as the route runs it (the head: `coverageQuery` under the settings `planCoverage` sets) against the statement it replaced (`mainCoverageQuery`, the base) at production's size and twice it,
in six states of `embeddings`, four of its visibility map and statistics and two after an embedder change,
and states the corpus size at which the head would reach its 10 s bound in the slowest of them.
It runs `TestCoverageStatementScale` (`internal/dispatch/embedqueue`), which builds a synthetic corpus in a database of its own at each scale:
`testdata/coverage_corpus.sql`, production's counts and proportions of 2026-11-17 times the scale
(11,151 issues, 216,414 document versions and 984,012 window rows at scale 1),
with texts cut from this repository's `docs/` and `skills/` and each issue's spec of a size sampled from real specs.
Autovacuum is off on `embeddings`, so nothing but the test sets its visibility map or its statistics.
Once the corpus is loaded it drops the two window indexes, `embeddings_window_embedded` and `embeddings_window_unembedded`, and builds each again with its migration's own statement, read from its file under `internal/dispatch/store/migrations` and run outside a transaction as the migration runner runs it,
timing each build and reading each index's size:
the downtime a deploy of those migrations takes, since Dispatch migrates before it serves.
On each corpus it then measures six states in turn:
`fresh`, every table vacuumed and analyzed;
`third`, one row of every third heap page of `embeddings` updated and the table analyzed, so about a third of its pages are not all-visible;
`all`, one row of every page updated and the table analyzed, so no page is all-visible;
`stale`, the table vacuumed, one row of every even page updated and the table analyzed, then one row of every odd page updated with no analyze after it,
so no page is all-visible while the statistics still record about half of them all-visible;
`embedder`, the configured embedder changed to another identity, whose identity step (`dropForeignVectors`, over every covered kind) drops every vector the corpus holds, with no analyze after it, so the statistics are the `stale` state's;
and `embedder-analyzed`, the same table analyzed.
The test fails the `stale` state unless its statistics record at least 0.3 of the pages all-visible (`laggingVisibleShare`), the share from which an earlier version of this statement read the windows by index-only scans, which fetched every window row from the heap one at a time in such a state (dispatch://INFRA-4241/artifact/tester-r2-job-2f9a6fc1-log),
and the `embedder` state unless the identity step dropped a vector; it logs how many it dropped.
In each state it checks that the base and the head answer the same rows in one snapshot, for the state's configured identity (the new one from `embedder` on),
and times both on one connection, one warm-up and then five alternating runs each under `EXPLAIN (ANALYZE, BUFFERS)`:
the base on the connection,
the head in a read-only transaction that `planCoverage` plans, as the route runs it, with no `statement_timeout`.
In every state it also runs a negative control:
the head's text once with none of `planCoverage`'s settings, the planner free.
It drops the database before the next scale.

It runs as a Job in the cluster's `builds` namespace, never in an issue pod.
It starts a Postgres of its own on 127.0.0.1:5433 with `legion-testpg ensure`, where an issue pod's own test server already listens,
and a corpus of production's size is more disk than an issue pod may use;
the script refuses to start where a server already answers on that port.
It needs the worker image, which carries Go, the PostgreSQL 18 server with pgvector and `legion-testpg`,
and a scratch disk of about 10 GB for scales 1 and 2:
one scale's database at a time, the write-ahead log, the Go caches and the clone.
The Job runs it from `packages/envoy` of a clone:

```bash
cd packages/envoy && COVERAGE_SCALE_SCRATCH=/scratch scripts/coverage-scale.sh
```

`COVERAGE_SCALE_SCRATCH` (default `/scratch`) names the scratch disk.
The server's data directory (`LEGION_TESTPG_DIR`), `GOCACHE`, `GOTMPDIR`, `GOMODCACHE` and the test's log, `coverage-scale.log`, go under it.
`DISPATCH_COVERAGE_SCALES` (default `1 2`) lists the scales, space-separated, and reaches the test as it is.
The script sets `LEGION_TEST_PG_DSN` and `DISPATCH_COVERAGE_SCALE_URL` itself,
both to `postgres://legion_test@127.0.0.1:5433/legion_test?sslmode=disable`.

It prints the server's version and the settings the times depend on
(`shared_buffers`, `work_mem`, `maintenance_work_mem`, `jit`, `max_parallel_workers_per_gather`, `effective_cache_size`),
then the test's output: for each scale the corpus's counts, each window index's build time and size, the head's report kind by kind and the items;
and for each state how many vectors its embedder change dropped, how many pages of `embeddings` the statistics record as all-visible and that share, the database's size,
each statement's median and every run, one plan of each,
and the negative control's time and whether it read `embeddings` by a Seq Scan.
Read the result from the line that starts `coverage-scale:`, which the script prints again as its last line.
It names each window index's build time and size at the two smallest scales;
then, for each state, the vectors dropped where the state changes the embedder, the share of pages the statistics record as all-visible, the head's and the base's medians, the negative control's times and whether it read `embeddings` by a Seq Scan, at both scales;
and the item count at which the head would reach 10 s by a linear fit and by a power law `t = a × s^k` through the two points,
with the smaller of the two as a multiple of the 1 × corpus's items.
It ends with the slowest state, the one with the smallest multiple, and its multiple;
where a state has no fit, it says the slowest state is not known and names the slowest of the others.
`pass` means that multiple is at least 4.
`FAIL` means it is below 4, or that the head's time did not grow between the two scales in some state, so no fit is defined.
The script exits with the test's own status, and with 1 when the test printed no `coverage-scale:` line.
CI never runs it: the test skips while `DISPATCH_COVERAGE_SCALE_URL` is unset.

## going-smoke.sh

Proves, on a booted server, that the real `envoy-dispatch` binary answers a question over HTTP and
streams it. It builds `./cmd/dispatch`, starts the fake chat-completions model it serves itself,
boots the binary against `DATABASE_URL` with `DISPATCH_ANSWERER=endpoint` pointed at that model,
reads `GET /api/v1/going/capability` and refuses anything but `{"model_configured":true}`, seeds a
project and an issue whose own text is worth citing through the API as the identity header's
person, asks a question of that issue over `POST /api/v1/going/requests` and prints the run's id
and `ends_at`, then prints every frame of `GET /api/v1/going/requests/{id}/stream` as
`<elapsed ms> <event> <data>` as it arrives, so two builds' runs diff, with the first `text`
frame's arrival on a line of its own. It ends `going-smoke: OK` only when the stream carried at
least one `text` frame, exactly one `item` frame naming the seeded issue and exactly one terminal
frame, and the first `text` frame arrived before the stream ended; any refusal names what was
missing and prints the server's and the model's logs. It needs `curl`, `jq`, `python3`, `openssl`
and `psql`, and names the one it cannot find.

No cloud model is called: the model is a fake the script starts. It serves TLS, because
`DISPATCH_CHAT_ENDPOINT` is an absolute https URL with a host and no query or fragment
(`cmd/dispatch/answerer.go`, `chatEndpoint`), so the run makes a self-signed certificate for
`127.0.0.1` with the IP in a subject alternative name, serves it from a small `python3` server and
hands the binary `SSL_CERT_FILE`, which Go's `crypto/x509` reads — no setting of Dispatch's own is
bent to admit a plain-http endpoint. The fake answers a streamed completion that links the seeded
issue by the href the server gave it, in several frames with a pause between them, and refuses any
call that does not present the bearer `DISPATCH_CHAT_TOKEN_FILE` holds, so a run that reports OK is
one whose token file reached the call. The server runs with a throwaway `HOME`, a made-up agent
token and a trusted identity header, so it reads no operator configuration and holds no credential,
and with NATS off. `DATABASE_URL` names the Postgres server rather than one database: the run makes
a database of its own on it, which the binary migrates as every start does, and drops it at the end.

```bash
legion-testpg ensure
cd packages/envoy && DATABASE_URL="$LEGION_TEST_PG_DSN" bash scripts/going-smoke.sh
```

Options:

- `--binary <envoy-dispatch>` — run this binary instead of building `./cmd/dispatch` from this
  checkout.
- `--model-status <code>` — answer every call to the fake model with that status instead of a
  completion. `--model-status 401` is one negative control: the stream carries
  `error ANSWER_FAILED` and nothing else, and the run exits 1 naming the frames it did not get.
- `--no-model` — boot the server with `DISPATCH_ANSWERER` unset and no `DISPATCH_CHAT_*` setting.
  That is the other negative control: the capability route answers `{"model_configured":false}`
  and the run exits 1 naming it.

No CI job runs this one, and none should: it builds the binary, boots a server, serves TLS and
reads an answer as it streams, which takes a machine with a Postgres server and the better part of
a minute, and what it proves is that a reader gets an answer on a real deployment's own path — a
thing a person reads rather than a status a job reports. Run it by hand, or as the role proving a
change to `going/`, to the answerer settings in `cmd/dispatch`, or to the three `going` routes, and
paste its output: the frame lines are the evidence.
