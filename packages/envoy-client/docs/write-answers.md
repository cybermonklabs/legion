# What an answer of Dispatch becomes

This file uses semantic line breaks: one sentence per line, and a long sentence may break after a clause.
A blank line separates list items.
Add a new item next to the item it belongs with, never at the end of a list unless it belongs there, so two pull requests that add items or change different sentences do not conflict.
`bun scripts/semantic-line-breaks.ts packages/envoy-client/docs/write-answers.md` checks the layout, and `--write` fixes it.

`DispatchClient` (`dispatch-http.ts`) reads each answer of Dispatch as one of the outcomes below, and the `dispatch` command prints each outcome as the last section says.
The package's [README](../README.md) ("Dispatch") says how a write carries its `Idempotency-Key` and when the client asks again under it.
[`ci-results.md`](ci-results.md) says what `dispatch result --project`, a CI job's write, prints for each outcome.

## A refusal

A non-2xx answer is a `DispatchServiceError`.
Dispatch's own refusal is JSON with a string `error`.
The error's message is that text, under its `code`, or under `HTTP_<status>` when Dispatch set no code, and `fromDispatch` is true.

Any other body did not come from Dispatch: a gateway's HTML page, an empty body, or JSON of another shape.
It is a `DispatchGatewayError`, whose `fromDispatch` is false.
Its message names the method and the URL with its query, and leaves out a user and a password that the URL may carry.
The bearer is a header, so no message names it.
The message then names the status and the reason phrase, a one-line plain-text excerpt of the body, and what asking again can do.
The excerpt drops scripts, styles and tags, and it reads at most the body's first 65,536 characters (`EXCERPT_SCAN_LIMIT` in `excerpt.ts`), in linear time.
In the excerpt and in the reason phrase, the client redacts the value after `Authorization:` or `Bearer`.
It also redacts each run of 8 or more characters of its own bearer and of each token it presented lately (`SentTokens`), and a shorter token only whole.
So a copy of a token that is cut short, split by markup or overlapped by another copy leaves no piece behind.
The message names a body with no text in the part that the excerpt reads by its size in bytes.
Only a body with nothing in it is "an empty body".

What asking again can do depends on the status (`gatewayAdvice`).
A status that cannot clear gets the same answer until someone fixes the Dispatch URL, or whatever answers in its place.
A 5xx, a 408 or a 429 can clear.
A `GET` that a gateway answered so may succeed when the client asks it again.
So may a write that a gateway answered 408 or 429.
A gateway sends its own timeout or rate limit before it forwards anything (`mayHaveReachedDispatch` is false).
A write that a gateway answered 5xx may or may not have reached Dispatch.
The advice is then to check whether it took effect before you send it again.
A caller that knows more about its own request can give its own advice from the error's `answer`, `advice`, `transient` and `mayHaveReachedDispatch`.
The close path of `dispatch issue-update` does so.
The executor reads a status as Dispatch's meaning only when `fromDispatch` is true.
Such a 404 means no linked issue, no such document, or a server without the route.

## A redirect

No request of the client follows a redirect: not a read, not a write, and not the `/healthz` probe.
Dispatch's API routes answer no request with a 3xx, so a 3xx is a refusal of whatever answered in Dispatch's place.
It is a `DispatchGatewayError`, whatever its body is.
Its message names the request as for any gateway's answer, then the status, the reason phrase and the origin that the redirect points at: `answered 302 (a redirect to <origin>, which this client does not follow)`.
That origin is the scheme and the host of the `Location`, with a port that is not the scheme's own (`redirectOrigin` in `excerpt.ts`).
That origin holds nothing of the target's path, query, fragment or user.
It names the body as it names any gateway's body: one plain-text line, every token the client presented redacted.
The `/healthz` probe reads no 3xx as Dispatch's word, and reads no body of one.

The listener's client refuses a redirect the same way.
An `EnvoyApiError` of a 3xx names the method, the URL without its user and password, the status and the origin (`transport.ts`).
It then names the body as one plain-text line, every token the client presented redacted, unless the body is the listener's own error JSON, whose error it names.
The pool identity of `@legion/broker-client` names a 3xx of the security token service by its status, as `HTTP <status>`, with the Google error code that a JSON body names (`googleErrorCode`).
The GitHub Actions identity, of `@legion/broker-client` and of the Go broker client, follows no redirect of the runner's OIDC token endpoint either, since that request carries the runner's bearer, and names a 3xx by its status alone.

## A path value that names nothing

The client refuses a call whose URL path would hold a value that names nothing, before any request, for every caller (`DispatchEmptyPathError`, thrown by `#url`).
Such a value is empty, only white space, or `.` or `..` (`namesNothing`).
Sent, `POST /api/v1/comments//resolve` names no comment, and Dispatch's router answers it with a redirect to another path.
The URL drops `.` and `..` as steps of the path, so `GET /api/v1/issues/..` asks `GET /api/v1/`, another route.
The message names the route with `{empty}`, or the value in double quotes, in place of that value, and says that nothing was sent.
It adds that a variable that gives the value may be empty or unset.
A model's tool call reaches this refusal, since a tool's schema takes such a value.
The `dispatch` command refuses each such value of a flag before it calls the client, naming the flag, so no argument of the command reaches it.

## A write whose outcome is unknown

Every attempt, answer, failure and wait of a write is one event of the write table (`write-outcome.ts`), which says what follows it: the answer is the call's, the write is asked again under its key, or the write ends.
Before anything of the write may have landed, a write that ends throws the event's own error: the refusal, the transport's failure, the credential's, the deadline's.
Once an attempt may have landed, every end but the keyed layer's verdict is a `DispatchWriteUnsettledError`.
An attempt may have landed once it went out and got no definitive answer: a 5xx, `409 KEY_IN_FLIGHT`, a silence cut, a reset, a 2xx whose body broke off or is not the JSON it says, or a 2xx that is no answer of Dispatch's.
That error is deliberately not a `DispatchServiceError`, because a caller reads a `DispatchServiceError` as a refusal that Dispatch decided.
Its message gives the last answer and the number of attempts.
It then says whether an earlier attempt may have reached Dispatch, or whether Dispatch was still running one.
It says that the write may or may not have taken effect.
Where the call saw the keyed layer judge the write, the message says that Dispatch finishes an attempt that it received.
It then says to read the write back before you send it again, because a new command sends it under a new key.
Otherwise it says to check whether the write took effect before you send it again.

A write that ends with no answer at all once an attempt may have landed is a `DispatchWriteUnsettledError` too, and its message names the request.
The client cut off its last attempt for silence ("made no progress for 20 s, so it was cut off").
Or the call's deadline ended it, while an attempt was out or after one that may have reached Dispatch.
The message then says that the write "had no definitive answer when its deadline ended it".
Its `last` is the cut or the timeout.
The client does not send an attempt that the deadline ends before it goes out.
Neither that attempt nor a credential that could not present counts as an attempt that may have reached Dispatch.
When the deadline or the caller's stop ends a wait before anything may have landed, the write ends with why nothing was sent or run: the credential's failure, else the answer of Dispatch's check of the token.

What the attempts showed is a fact of the whole call (`WriteCall`), not of one series of attempts.
A write carries one key across its no-route retries and its re-send after a dead token.
The keyed layer wraps each route's handler, so it stores nothing under the key for those answers.
Once an attempt may have reached a handler, the call ends as a `DispatchWriteUnsettledError`, unless its last answer is the keyed layer's verdict.
That verdict is a definitive answer that names the key back and that the layer keeps as the key's outcome.
It is the stored result, a replay, a landed answer, or the handler's refusal.
An echoed 401 is no verdict, because it can come after the handler committed.
`422 KEY_REUSED` is no verdict either, because it says only that this request did not run.
A no-route answer, Dispatch's check of the token, a dead token or a credential that could not present proves nothing of the earlier attempt.
The client throws the caller's own cancellation as it is only before anything of the write may have landed; after that, the cancellation is the `last` of a `DispatchWriteUnsettledError` that says the caller stopped the write with no definitive answer.

`writeOutcomeUnknown(error)` says whether a write's error leaves its outcome unknown.
It does for a `DispatchWriteUnsettledError`, for a gateway's answer with `mayHaveReachedDispatch`, and for Dispatch's own 5xx.
It also does for a transport failure after an attempt that may have reached Dispatch, which the client throws as a `DispatchWriteUnsettledError` with that failure as its `last`.
A connection that never opened reached nothing: the host refused it, its name did not resolve, or no route reached the host.
A request that Dispatch's check of its broker token turned away before any handler ran reached nothing too.
Those answers are `429 BROKER_TOKEN_RATE_LIMITED` and `503 BROKER_UNAVAILABLE` (`tokenNotChecked`).
A refusal carries its `Retry-After` as `retryAfterMs`.

## A write that landed with its answer gone

Dispatch's keyed layer names the request's `Idempotency-Key` back on every answer that it gives a keyed write.
So an answer that names this call's key back is Dispatch's word about this write.
A landing needs that key.
The client takes a write as made, with its answer gone, only from an answer that names the call's key back.
The rule is the same for a 2xx and for a `409 KEY_COMMITTED` (`#notTheRow`).
A key that a caller gives is the call's key: `docEdit` and the upload of a project's document take one.
An empty key names no request, so no answer names it back, and an answer that names no key is no landing of it.

A write that Dispatch made and whose answer is gone is a `DispatchWriteLandedError`, with `code`, a `DispatchLandedCode`, and `status`.
On a route that stores what it is sent, these answers are a landing when they name the call's key back:

- `409 KEY_COMMITTED`: the write landed before its answer was lost, and Dispatch will not run it again.

- A replayed 2xx (`Idempotency-Replayed: ?1`) that reads `ANSWER_NOT_KEPT`: the first answer was larger than Dispatch keeps (`answerNotKept`, which the page shares).

- A 2xx that names the key back and whose body gives no row: the client's own `ANSWER_LOST` (`DISPATCH_ANSWER_LOST`), since Dispatch answers a write's 2xx only after the write committed.
  JSON that is not the row is such a landing, and so is a bodiless 204 or 205 on a write whose row needs a body.
  A body that breaks off, or that is not the JSON its type says, is asked again under the key instead, since Dispatch replays what it stored; a body that stalls until the deadline ends the call as unknown.
  A 2xx whose body is empty or not JSON-typed is no answer Dispatch writes to a keyed write, so the write ends on it as unknown.

Every `POST` and `PATCH` of the client reads its 2xx into the row that its callers read (`dispatch-write-rows.ts`).
A row holds those members, in the forms that the route answers.
A 2xx that names the key back and gives the row is the call's answer.

A landing is no refusal and no `DispatchServiceError`, and the client never asks it again.
Its message is the one line that a session reads.
It says that the method and URL were made, and why the answer is gone.
It then says to read back what it made with `dispatch read` or the matching list.

Three routes store nothing: the round trip of a text, the related-item check and a result's dry run.
An answer of one of them that is not its answer made nothing, so it is never a landing.
That holds for a 2xx with no row, a 2xx that names no key back, a replay whose first answer Dispatch did not keep, and a `409 KEY_COMMITTED`.
Each is a `DispatchAnswerMissingError` (`ANSWER_LOST`), which says that nothing was made and that asking again is safe.

## A 2xx that names no key back

Dispatch's keyed layer runs on every `POST` and `PATCH` route.
So a 2xx that names no key back, or another request's key, is never this call's answer, whatever it holds.
The client compares nothing of it with what the call sent, because an equal value does not tell this call's write from another call's.
A proxy's page, a server that is not Dispatch, a proxy or cache that answers one write with another's, and a proxy that drops the header give such an answer.
A Dispatch older than the keyed layer names no key back either, so each of its 2xx is such an answer.
A `409 KEY_COMMITTED` that names no key back, or another request's key, is no such answer either.
The write may have reached Dispatch behind what answered it, so its outcome is unknown, a `DispatchWriteUnsettledError`.
Check whether it took effect before you send it again.

## What the `dispatch` command prints

A session's `dispatch` command prints what the model reads, and the README ("The `dispatch` command") gives its exit codes.

- The call's answer: the command prints the result, and exits 0.

- A landing: the command prints the landing's one line, exits 0, and records `landed_without_answer: <code>` in its ledger entry.

- A landing of a step before the command's own write: the command fails, exits 1, and prints the line with what it means.
  For a picture upload, the command wrote nothing else, and the call run again uploads the picture as its next version.
  For the closing reason of `issue-update --status done`, the command did not send the close.
  Read the issue's messages for the reason, then close with a reason that points at it.
  The close's own `PATCH` is the command's write, so its landing exits 0.

- Dispatch's refusal: the command prints `<CODE>: <message>`, and exits 1.

- A gateway's answer or a redirect: the command prints `HTTP_<status>: ` and the message, and exits 1.

- A missing answer or an unknown outcome: the command prints the error's message, and exits 1.

- A flag whose value names nothing: the command prints the refusal, and exits 2.
