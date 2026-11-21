# A CI job's result, and its read

These two calls are part of the `dispatch` command, which the package's
[README](../README.md) ("The `dispatch` command") describes.

`dispatch result --project <KEY> --check <id> --verdict pass|fail|could_not_run --commit <sha>`
is a CI job's record of its result of a check of the spec files, and the one call that is no
session's (`ownIdentityResult` in `dispatch-cli.ts`). It reads no `DISPATCH_HOST`, no envoy.json
and no session state, and it asks no AskGate and no listener. A refusal is written to a session's
ledger only where the environment names a session id, as every refusal of the command is. The job
runs it under `agent-secrets result-grant -- …`, which exchanges the run's token at the secrets
broker for the airlock's placeholder of a result grant and gives it to the command in
`DISPATCH_RESULTS_TOKEN`, beside the airlock proxy's settings. Dispatch is `DISPATCH_URL`, and the
credential is that placeholder alone (`ownIdentityDispatch` in `dispatch-config.ts`,
`airlockCredential` in `airlock-credential.ts`): the write carries it as its one `Authorization`
header, `DPoP <placeholder>`, and no proof of its own, and the airlock proxy puts the run's
results token and a proof of its own key in its place. The command reads no bearer and no broker
variable, so it trades nothing, `AGENT_SECRETS_MACHINE_IDENTITY` included, and holds no Dispatch
credential. With no placeholder it sends nothing and exits 2, naming `agent-secrets
result-grant`. The body is `{commit, check, verdict, observed}` and nothing else, and the write
sends that one `POST` and nothing else to Dispatch's host. A result keeps 2000
characters of `observed` (`SPEC_CHECK_OBSERVED_MAX`), so the command sends at most that: a
longer text goes as its first 500 characters (`SPEC_CHECK_OBSERVED_START`), a line that says the
middle was cut (`SPEC_CHECK_OBSERVED_CUT_NOTE`) and its last characters
(`SPEC_CHECK_OBSERVED_END`), since a test's output names the test at its start and says why it
failed at its end, and a test log of any size still writes its result. A NUL, which Dispatch
cannot store, goes as U+FFFD, and so does each byte of a file that is not UTF-8. It prints the
stored result as JSON and exits 0. A write that landed with its answer lost prints
`{"landed_without_answer":"<code>"}` and exits 0: the result is stored, and that includes a 201
that names the request's key back, as Dispatch's does, whose JSON is not a stored result
(`ANSWER_LOST`), and a `409 KEY_COMMITTED` that names it back. Dispatch's keyed layer names the
request's key back on every answer of a write route, so only such an answer is the job's stored
result. Each other 2xx, one that names another request's key back or names none, prints
`write_outcome_unknown`, whatever it holds: the command compares nothing of it with what the call
sent. So does a `409 KEY_COMMITTED` that does not name the key back, and a 2xx whose body is empty
or not JSON-typed, whatever key it names back, since every answer Dispatch writes is JSON. Such an
answer may be a proxy's page, a server that is not Dispatch, a proxy or a cache that answers one
write with another's, or a proxy that drops the header, so the result may be stored, and a second
run is safe. A 201 whose body breaks off, or is not the JSON its type says, is asked again under
the key within the write's bound, and one whose body the bound ends prints `write_outcome_unknown`.
[`write-answers.md`](write-answers.md) gives these rules for every write. No request of the command
follows a redirect: a redirect is a refusal of whatever answered. The line of a redirect from
Dispatch's place names the origin that the redirect points at, its scheme and host, never its path
or query, and the answer's body as one redacted line, and the command exits 1. A write that
reached Dispatch with no answer that says whether it stored the result (`writeOutcomeUnknown`)
prints `{"write_outcome_unknown":"<code>"}`, the code of its last failure such as `ECONNRESET` or
`HTTP_200`, and exits 1: the result may be stored, and a second run is safe, because Dispatch keeps
each result. The command refuses an empty `--project`, `--check` or `--commit` before any request,
with exit 2, and names the flag. An empty variable of the step gives such a value.

The airlock proxy can refuse the write itself, and then nothing reached Dispatch. A tunnel it
refuses at the CONNECT reaches the command as Bun's `ERR_PROXY_TUNNEL`, whose message carries the
status line alone: a 407 for a grant that ended, or a proxy password that names none, ends the
write at once and prints `AIRLOCK_TUNNEL_REFUSED: the airlock proxy refused the connection (407
Proxy Authentication Required); nothing was sent to Dispatch`, and a 503 is a connection that
never opened, asked again within the write's bound as a refused connection is, and then printed
the same way. An answer the proxy gives itself inside an open connection carries
`Airlock-Forwarded: false` and its `Airlock-Code`: it prints `<code>: <the proxy's words>;
nothing was stored`, with each copy of the placeholder in the words redacted, and only its
`503 GRANT_UNREADABLE` is asked again within the bound. Each exits 1, and none is a
`write_outcome_unknown`, but where an earlier attempt of the same write may have reached Dispatch,
which leaves its outcome unknown. A call that Dispatch's check of the run's token
(`429 BROKER_TOKEN_RATE_LIMITED`, `503 BROKER_UNAVAILABLE`, `tokenNotChecked`) turns away before
anything is stored is asked again after the answer's `Retry-After`, or a second. Every CI write
reaches Dispatch from a broker pod's address, so the jobs that write at one time share that
check's bucket of the address. The whole call has one bound, 60 s from its start: one timeout,
made where the call starts, is the signal every await of it runs under (`ResultBound`). The write
client's deadline is that signal, its asks again under the key and the command's rounds start no
later than 7 s before its end, a reserve greater than the 5 s Dispatch's check of a token waits on
the broker, and a call turned away until then prints which turned it away with the last answer,
its code named, and exits 1, with no `write_outcome_unknown`: only an attempt that may have
reached a handler prints that line. An attempt the deadline ends before it is sent reached
nothing.

`dispatch results --project` prints its lines and `--json` by the same rule of a runner's
log as the command's own output. The
stored result names the run as its recorder and no person, so the job's log holds no person's
address. It prints Dispatch's refusal as `<CODE>: <message>` and exits 1, such as
`SPEC_FILES_REFUSED` with the first problem of spec files the graph logic refuses. A refusal of
its arguments, or of its environment, sends nothing and exits 2. `--observed`
text that opens with `-`, such as a test's `--- FAIL` line, goes as `--observed=<text>` or through
`--observed-file`. A GitHub runner reads workflow commands from the job's log, so each text the
command did not write (an argument, a variable, a file's name, an answer of the airlock proxy or of
Dispatch) prints by the rule of Go's `runnerlog.Printable` (`runner-log.ts`): each control
character, format character, line or paragraph separator as its escape, `\n` or `\u0085`, and
the `[` of every `##[` as `\x5b`, or as `\u005b` inside JSON, so no such text starts a line of its
own or names a workflow command. A refusal that starts with `::` has that first `:` written as
`\x3a`.
`dispatch results --project <KEY> --commit <sha> [--check <id>]` is a session's read of those
results, one line each, newest first, or the stored results as JSON with `--json`.

## The Go producer and the broker's grant

A CI job writes a check from the spec files at its commit with:

```sh
agent-secrets result-grant -- dispatch result --project ACME \
  --check BUNDLE-c1 --verdict pass --commit "$GITHUB_SHA" \
  --observed 'the command or evidence the job actually ran'
```

The Go producer reads the job's signed GitHub identity and trades it at the broker's token endpoint for an airlock placeholder (`requested_token_type=urn:envoy:params:oauth:token-type:airlock-placeholder`). The broker answers a result entry's run a placeholder and a result grant, which it keeps in its database, and never an access token: the same run asking for an access token is refused, and no other identity is granted a placeholder. The child receives `DISPATCH_RESULTS_TOKEN`, the placeholder, and the normal proxy and certificate settings. It receives neither a Dispatch access token nor a DPoP private key.

The airlock proxy carries a write to the configured Dispatch host only when the write's one `Authorization` header is `DPoP <placeholder>` and the rule's allow list names its method and path; it refuses a write with no placeholder, with the placeholder under another scheme or beside a second `Authorization` header (`403 NO_PLACEHOLDER`), and a write that asks to upgrade (`403 PROTOCOL_SWITCH_REFUSED`), before it dials anything. For a write it carries, the broker pod that serves it mints the run's results token for the grant, bound to that pod's own DPoP key, which the pod makes at start, holds in memory and logs by thumbprint alone (`broker: the airlock's DPoP key`). The proxy puts the token in place of the placeholder, replaces any `DPoP` header the command sent with a proof of the pod's key for Dispatch's public origin (`https://` and the bound host) and the write's path, whatever address it sends the write to, and drops the `Forwarded`, `X-Forwarded-For`, `X-Forwarded-Host`, `X-Forwarded-Proto`, `X-Real-IP` and `Via` headers the command sent. An answer the proxy gives itself says `Airlock-Forwarded: false` and names its `Airlock-Code`; an answer it carried from Dispatch says `Airlock-Forwarded: true` and carries no `Airlock-Code`. Its line for the write names the grant and the job's `ci_check_run_id`. A refused, expired or lost grant does not fall back to ordinary secret release or another identity. A grant lives in the broker's database, so any broker pod serves it and a pod's end ends none, and lasts fifteen minutes, or the broker's access-token lifetime when that is shorter; the token ends with it, and a write after its end is refused `407 GRANT_NOT_LIVE` at the proxy.

## Job and broker configuration

The job needs `id-token: write` and the GitHub runner's `ACTIONS_ID_TOKEN_REQUEST_URL` and `ACTIONS_ID_TOKEN_REQUEST_TOKEN`. Configure:

- `AGENT_SECRETS_URL`: the broker's base URL.
- `AGENT_SECRETS_MACHINE_IDENTITY=github-actions`.
- `AGENT_SECRETS_MACHINE_AUDIENCE`: the audience the broker requires for this workflow.
- `DISPATCH_URL`: the Dispatch HTTPS origin.

A job whose runner writes its signed identity to a file can instead set `AGENT_SECRETS_MACHINE_TOKEN_FILE`. The Go identity source rereads it; no TypeScript metadata client trades this identity. Do not put a bearer, signing key or session login in the command's arguments. A personal `DISPATCH_TOKEN` is not a results credential and cannot replace the placeholder.

The broker needs its result-workflow entry, the existing airlock proxy and certificate configuration, and a rule for the actual Dispatch host that allows the results write:

```json
{
  "secrets": [
    {"name": "DISPATCH_RESULTS_TOKEN", "kind": "dpop", "hosts": ["dispatch.example.com"],
     "headers": ["Authorization"], "allow": [{"method": "POST", "pathPrefix": "/api/v1/projects/"}]}
  ]
}
```

The rule carries the placeholder only in `Authorization`, and the proxy sends only the requests its allow list names. The broker refuses to start with a result-workflow entry and no such rule: of kind `dpop`, binding a host, with a non-empty allow list and `Authorization` as its one header. The rule does not grant a project or choose a run: the broker derives those from the verified GitHub identity and its result-workflow entry. A workflow authorized only for ordinary credentials cannot obtain a placeholder through this endpoint. Use HTTPS outside the isolated loopback test environment.

A failure to prove the job, trade its token or obtain the required scope runs no child command. The producer prints one safe diagnostic. A failure after the child sends its result follows the normal keyed-write rules: matching successful answers settle the call, a committed write with a lost answer is a landing, and an uncertain write must be read back before a new command uses another key.

## Stored statement and scope

`stored.attestation` is a bare, unsigned in-toto Statement v1 (`https://in-toto.io/Statement/v1`) with predicate type `https://github.com/cybermonklabs/legion/attestations/spec-check-result/v1` (`spec_results.go` in `packages/envoy/internal/dispatch/api`). The statement is the immutable result row in a standard envelope, not a claim of cryptographic signing. A later DSSE envelope would have to sign exactly this stored statement.

Dispatch derives the subject SHA, project, workflow, job, check run and run attempt from what GitHub and the broker authenticated. A request cannot substitute another commit or job. The recorder is the authenticated workflow run and session, **not** the person named in the broker's configuration; that person did not run the job and is not attributed as its recorder. Each accepted invocation appends a result. Retries of the same keyed write do not append it twice.

A results-only credential reaches no other Dispatch route. A normal session can read the stored results, but cannot write one.

## Real local verification

With a local Postgres server and Docker available, run:

```sh
DISPATCH_TEST_DATABASE_URL=postgres://… bun packages/envoy-client/test/ci-results-stack.ts
```

The stack runs the real broker and Dispatch, built from this checkout, with a stand-in for GitHub and a TLS front door in an owned, capped container. No fixed host port is claimed and no host network setting is changed. It seeds project ACME and one commit with the probe spec, prints the variables of one job of a push run and complete `agent-secrets result-grant -- …` commands, the controls among them (another commit, another job, another route and a forged token), and runs until Ctrl-C.

`src/__tests__/ci-results.live.test.ts` runs the same stack with `LEGION_E2E_DISPATCH_STACK=1` and `DISPATCH_TEST_DATABASE_URL`. It asserts the stored statement and its run-only recorder, wrong-commit and wrong-job refusals, forged issuer and unlisted-ref refusals, a random access token refused as not active, session-token restrictions, results-only route restrictions, no person in the stored result or the command's output, refusals of spec files that the graph logic refuses, and an observed text of 1 MiB stored as its start and its end.

To exercise the forged-issuer case separately:

```sh
bun packages/envoy-client/test/ci-results-stack.ts --forge-only
```

It starts the stand-in's token endpoint alone and prints the variables a job reads it by. Each token it mints is signed by a key absent from the issuer's published key set, so a broker that verifies GitHub's tokens refuses it before the child writes anything.
