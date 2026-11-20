# A CI job's result, and its read

These two calls are part of the `dispatch` command, which the package's
[README](../README.md) ("The `dispatch` command") describes.

`dispatch result --project <KEY> --check <id> --verdict pass|fail|could_not_run --commit <sha>`
is a CI job's record of its result of a check of the spec files, and the one call that is no
session's (`ownIdentityResult` in `dispatch-cli.ts`). It reads no `DISPATCH_HOST`, no envoy.json
and no session state, and it asks no AskGate and no listener. A refusal is written to a session's
ledger only where the environment names a session id, as every refusal of the command is. Dispatch
is `DISPATCH_URL`, and the credential is this machine's own identity traded at the secrets broker
(`ownIdentityDispatch` in `dispatch-config.ts`: `AGENT_SECRETS_URL` with
`AGENT_SECRETS_MACHINE_IDENTITY`, `github-actions` in a GitHub Actions job, with
`AGENT_SECRETS_MACHINE_AUDIENCE` and the runner's `ACTIONS_ID_TOKEN_REQUEST_URL` and
`ACTIONS_ID_TOKEN_REQUEST_TOKEN`). It reads no bearer and trades no person's sign-in, and the
trade names no session, `AGENT_SECRETS_SESSION` included, since the broker's result entry fixes
it. The body is `{commit, check, verdict, observed}` and nothing else. A result keeps 2000
characters of `observed` (`SPEC_CHECK_OBSERVED_MAX`), so the command sends at most that: a
longer text goes as its first 500 characters (`SPEC_CHECK_OBSERVED_START`), a line that says the
middle was cut (`SPEC_CHECK_OBSERVED_CUT_NOTE`) and its last characters
(`SPEC_CHECK_OBSERVED_END`), since a test's output names the test at its start and says why it
failed at its end, and a test log of any size still writes its result. A NUL, which Dispatch
cannot store, goes as U+FFFD, and so does each byte of a file that is not UTF-8. It prints the
stored result as JSON and exits 0. A write that landed with its answer lost prints
`{"landed_without_answer":"<code>"}` and exits 0: the result is stored, and that includes a 201
that names the request's key back, as Dispatch's does, whose body is JSON that gives no stored
result (`ANSWER_LOST`), a 204 that names it back (`ANSWER_LOST`), and a `409 KEY_COMMITTED` that
names it back. A 201 whose body breaks off, or is not the JSON its type says, is asked again under
its key, since Dispatch replays what it stored; one whose body stalls until the bound, or that is
empty or no JSON, may have stored the result and prints `write_outcome_unknown`. Dispatch's keyed
layer names the request's key back on every answer of a write route, so only such an answer is the
job's stored result. Each other 2xx, one that names another request's key back or names none,
prints `write_outcome_unknown`, whatever it holds: the command compares nothing of it with what the
call sent. So does a `409 KEY_COMMITTED` that does not name the key back. Such an answer may be a
proxy's page, a server that is not Dispatch, a proxy or a cache that answers one write with
another's, or a proxy that drops the header, so the result may be stored, and a second run is safe.
[`write-answers.md`](write-answers.md) gives these rules for every write. No request of the command
follows a redirect, the request for the runner's OIDC token included: a redirect is a refusal of
whatever answered. The line of a redirect from Dispatch's place names the origin that its
`Location` points at, its scheme and host, never that target's path or query, and then the body as
one line with every token the command presented redacted, as any other body that is not Dispatch's
is named. The runner's endpoint's line names the status alone, and nothing of
the body. The command exits 1. A write that reached Dispatch with no answer that says whether it
stored the result (`writeOutcomeUnknown`) prints `{"write_outcome_unknown":"<code>"}`, the code of
its last failure such as `ECONNRESET`, `HTTP_200` or `TimeoutError`, and exits 1: the result may be
stored, and a second run is safe, because Dispatch keeps each result. A connection refused reached
nothing: it is asked again on the write's pacing, and then prints the transport's line with no
`write_outcome_unknown`. The command refuses an empty `--project`, `--check` or `--commit` before
any request, with exit 2, and names the flag. An empty variable of the step gives such a value. A
call that the broker turns away (its rate limit's 429, a 5xx, no answer within the trade's own
30 s, or a trade that could not be made) or that Dispatch's check of the job's token turns away
(`429 BROKER_TOKEN_RATE_LIMITED`, `503 BROKER_UNAVAILABLE`, `tokenNotChecked`) before anything is
stored is asked again. The broker client's retry loop trades again on its own schedule
(`@legion/broker-client`'s `token-client.ts`), or after the broker's `Retry-After` for its 429,
and the call asks again no sooner (`TradeRetryingError.retry`); Dispatch's answers are asked again
after their `Retry-After`, or a second. The whole call has one bound, 60 s from its start: one
timeout, made where the call starts, is the signal every await of it runs under, its waits on the
runner's token request and the broker's trade included (`ResultBound`); the trade itself runs under
its own 30 s bound. The write client is given that timeout as its `signal` and the same 60 s as its
own deadline (`deadlineMs`), from which it gives each credential presentation its time less the
request's reserve; its asks again under the key and the command's rounds start no later than 7 s
before the bound ends, a reserve greater than the 5 s
Dispatch's check of a token waits on the broker, and a call turned away until then prints which
turned it away with the last answer, its code named, and exits 1, with no `write_outcome_unknown`:
only an attempt that may have reached a handler prints that line. An attempt the deadline ends
before it is sent reached nothing. A bound that ends the wait on a trade not yet answered prints
which part, the job's own token or the broker's trade, and that nothing was written.
`dispatch results --project` prints its lines and `--json` by the same rule of a runner's
log as the command's own output. The
stored result names the run as its recorder and no person, so the job's log holds no person's
address. It prints Dispatch's refusal as `<CODE>: <message>` and exits 1, such as
`SPEC_FILES_REFUSED` with the first problem of spec files the graph logic refuses. A refusal of
its arguments, or an identity with a variable missing, sends nothing and exits 2. `--observed`
text that opens with `-`, such as a test's `--- FAIL` line, goes as `--observed=<text>` or through
`--observed-file`. A GitHub runner reads workflow commands from the job's log, so each text the
command did not write (an argument, a variable, a file's name, an answer of the broker or of
Dispatch) prints by the rule of Go's `runnerlog.Printable` (`runner-log.ts`): each control
character, format character, line or paragraph separator as its escape, `\n` or `\u0085`, and
the `[` of every `##[` as `\x5b`, or as `\u005b` inside JSON, so no such text starts a line of its
own or names a workflow command. A refusal that starts with `::` has that first `:` written as
`\x3a`.
`dispatch results --project <KEY> --commit <sha> [--check <id>]` is a session's read of those
results, one line each, newest first, or the stored results as JSON with `--json`.
