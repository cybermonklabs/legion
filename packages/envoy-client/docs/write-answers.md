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
The excerpt drops scripts, styles and tags, and it reads at most the first 64 KiB of the body, in linear time.
In the excerpt and in the reason phrase, the client redacts the value after `Authorization:` or `Bearer`.
It also redacts each run of 8 or more characters of its own bearer, and a shorter bearer only whole.
So a copy of the bearer that is cut short, split by markup or overlapped by another copy leaves no piece behind.
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

No request of the client follows a redirect: not a read, and not a write.
Dispatch's API routes answer no request with a 3xx, so a 3xx is a refusal of whatever answered in Dispatch's place.
It is a `DispatchGatewayError`, whatever its body is.
Its message names the request as for any gateway's answer, then the status and the reason phrase, and the origin that the redirect points at.
That origin is the scheme and the host of the `Location`, with a port that is not the scheme's own (`redirectOrigin` in `excerpt.ts`).
The message holds nothing else of the `Location`: not its path, query, fragment or user.
The body is named as any gateway's body is: one redacted plain-text line, as the section above says (`bodyForText` in `excerpt.ts`).

The listener's client refuses a redirect the same way.
An `EnvoyApiError` of a 3xx names the method, the URL without its user and password, the status and the origin (`transport.ts`).
It names the body as one redacted line too.
The pool identity of `@legion/cipher-client` names a 3xx of the security token service as it names any other non-2xx: `HTTP <status>`, with the Google error code that its body names, if any.
The GitHub Actions identity of the Go broker client follows no redirect of the runner's OIDC token endpoint either, since that request carries the runner's bearer, and names a 3xx by its status alone.

## An answer of the secrets broker's airlock proxy

A CI job's result write goes out through the secrets broker's airlock proxy, Cipher's proxy, with the proxy placeholder of its run's result grant as its credential (`airlockCredential`) and the proxy named by the job's `HTTPS_PROXY`.
A person's review from a box goes through the same proxy, which the client names itself (`reviewArtifact`, `via`).
The proxy can refuse the write itself, and then the write reached no service and stored nothing.
Every write reads the proxy's own answers by one rule (`proxyOwnAnswer`, `proxyEvent`), whichever names the proxy: before any byte of the write left the proxy, an outage (`outage`: a 408, a 429 or a 5xx) is asked again, and every other status ends the write at once.

A tunnel that the proxy refuses at the `CONNECT` reaches the client as Bun's `ERR_PROXY_TUNNEL`, whose message carries the proxy's status line, with the answer's headers and no body.
The write loop reads it as an `AirlockTunnelRefusedError`, with code `AIRLOCK_TUNNEL_REFUSED`, the `status`, the `reason` phrase, and the proxy's `Cipher-Proxy-Code` as `proxyCode`.
Its message says that the proxy refused the connection and that nothing was sent to Dispatch.
An outage, such as a 503, is a connection that never opened, so the write asks again on its schedule, and ends with that error when the schedule runs out.
Every other status ends the write at once, a 407 for a grant that ended among them.

An answer that the proxy gives itself inside an open connection, before any byte of the request left it, carries `Cipher-Proxy-Forwarded: false` and a `Cipher-Proxy-Code`.
It is an `AirlockRefusedError`, with the status, that code, or `HTTP_<status>` where it names none, and the proxy's words as `body`, quoted through the client's bounded excerpt, as every answer that the proxy gives itself is: one line, cut at the excerpt's length, read from the body's first 64 KiB, each piece of a token the client presented redacted.
A word of the proxy's that the placeholder spells, such as "placeholder", is redacted with it.
An outage of it, such as a grant that the proxy could not read at that moment (`503 GRANT_UNREADABLE`) or a dial to Dispatch that failed (`502 UPSTREAM_UNREACHABLE`), is asked again under the key first: nothing reached Dispatch, and the write is keyed.
Every other status of it ends the write at once.

Neither error is a `DispatchServiceError`, and `writeOutcomeUnknown` is false for both.
Where an earlier attempt of the same write may have reached Dispatch, the write ends as a `DispatchWriteUnsettledError`, as for any answer that proves nothing of that attempt.

An answer that the proxy gives itself after the request may have reached Dispatch carries `Cipher-Proxy-Forwarded: unknown` and a `Cipher-Proxy-Code`: the proxy's connection to Dispatch broke after the write was sent (`502 UPSTREAM_UNREACHABLE`), or the write's grant ended while Dispatch held the call (`407 GRANT_NOT_LIVE`).
The write loop reads it as an answer after the write went out (`proxy-unsettled`, `AirlockOutcomeUnknownError`, with the status, the code and the redacted words): the write may have landed, so it asks again under its key, and ends as a `DispatchWriteUnsettledError` when no answer settles it.

A review through the proxy reads the same answers by the same rule, and ends with a `DispatchProxyRefusalError` in place of the two airlock errors: a refusal with the proxy's status and code that names the proxy by its origin, says whether anything may have reached Dispatch, and quotes the proxy's words through the client's bounded excerpt.
It is thrown only while no attempt may have reached Dispatch, so `writeOutcomeUnknown` is false for it; once one may have, it is the `last` of a `DispatchWriteUnsettledError`.

## A path value that names nothing

The client refuses a call whose URL path would hold a value that names nothing, before any request, for every caller (`DispatchEmptyPathError`, thrown by `#url`).
Such a value is only Go's white space, or nothing at all, or only dots and slashes inside such white space, such as `.`, `..`, `./`, `../`, `.//` or `/` (`pathNamesNothingPattern` in `go-space.ts`).
Go's white space is the set Dispatch trims a value by (`strings.TrimSpace`); it holds U+0085, which JavaScript's `trim` keeps, and not U+FEFF, which `trim` takes.
Sent, `POST /api/v1/comments//resolve` names no comment, and Dispatch's router answers it with a redirect to another path.
The URL drops `.` and `..` as steps of the path, so `GET /api/v1/issues/..` asks `GET /api/v1/`, another route.
A script that writes `"./$X"` with an empty `X` sends `./`, which a server that reads the path decoded takes as no step.
The message names the route with `{empty}`, or the value in double quotes, in place of that value, and says that nothing was sent.
It adds that a variable that gives the value may be empty or unset.
A model's tool call reaches this refusal, since a tool's schema takes such a value.
The `dispatch` command refuses each such value first, naming the flag, for each flag whose value goes into a URL path (each `path` field of `IDENTIFIER_FIELDS` in `dispatch-command.ts`: `--issue`, `--project`, `--artifact`, `--retract`, `--comment` and `--session`).
So no argument of the command reaches this refusal: a program that ran every command in each of its forms (of an issue, of a project, and of a project's document) with 14 such values in each string flag that has no fixed set of values found no run that did.

## A write whose outcome is unknown

Once an attempt may have reached a handler, a call without a definitive keyed answer throws `DispatchWriteUnsettledError`, not the last attempt's raw error.
It carries the write's key as `key` and the last safe error as `last` and `cause`.
It is deliberately not a `DispatchServiceError`, which a caller reads as a refusal Dispatch decided.
The message gives the last answer, attempt count and possible effect, and says to read the write back before a new command sends a new key.

A write that ends with no answer at all is a `DispatchWriteUnsettledError` too, and its message names the request.
The client cut off its last attempt for silence ("made no progress for 20 s, so it was cut off").
Or the call's deadline ended it, while an attempt was out or after one that may have reached Dispatch.
The message then says that the write "had no definitive answer when its deadline ended it".
Its `last` is the cut or the timeout.
The client does not send an attempt that the deadline ends before it goes out.
Neither that attempt nor a credential that could not present counts as an attempt that may have reached Dispatch.
When the deadline ends a pause that followed answers that reached no handler, the write ends with the last of those answers.

What the attempts showed is a fact of the whole call (`WriteCall`), not of one series of attempts.
A write carries one key across its no-route retries and its re-send after a dead token.
The keyed layer wraps each route's handler, so it stores nothing under the key for those answers.
Once an attempt may have reached a handler, the call ends as a `DispatchWriteUnsettledError`, unless its last answer is the keyed layer's verdict.
That verdict is a definitive answer that names the key back and that the layer keeps as the key's outcome.
It is the stored result, a replay, a landed answer, the handler's refusal, or `422 KEY_REUSED`.
`KEY_REUSED` settles the call because every attempt of it sends the same bytes under its one key, and Dispatch's digest covers the method, the route, its path values, the query, the content type less its boundary and the body, so a key held for another request was claimed by no attempt of this call.
An echoed 401 is no verdict, because it can come after the handler committed.
A no-route answer, Dispatch's check of the token, a dead token or a credential that could not present proves nothing of the earlier attempt.
Before any attempt may have reached a handler, the caller's cancellation remains its own error.
After a possible landing it is the unsettled error's `last` and `cause`.

`writeOutcomeUnknown(error)` says whether a write's error leaves its outcome unknown.
It does for a `DispatchWriteUnsettledError`, for a gateway's answer with `mayHaveReachedDispatch`, and for Dispatch's own 5xx.
A transport failure after an attempt that may have reached Dispatch is wrapped in `DispatchWriteUnsettledError` as well.
A connection that never opened reached nothing: the host refused it, its name did not resolve, or no route reached the host.
So did a tunnel that a proxy refused to open, which Bun reports as `ERR_PROXY_TUNNEL`.
A request that Dispatch's check of its broker token turned away before any handler ran reached nothing too.
Those answers are `429 BROKER_TOKEN_RATE_LIMITED` and `503 BROKER_UNAVAILABLE` (`tokenNotChecked`).
A refusal carries its `Retry-After` as `retryAfterMs`.

## A write that landed with its answer gone

Dispatch's keyed layer names the request's `Idempotency-Key` back on every answer that it gives a keyed write.
So an answer that names this call's key back is Dispatch's word about this write.
A landing needs that key.
The client takes a write as made, with its answer gone, only from an answer that names the call's key back.
The rule is the same for a 2xx and for a `409 KEY_COMMITTED` (`#missingWriteAnswer`).
A key that a caller gives is the call's key: `docEdit` and the upload of a project's document take one.
An empty key names no request, so no answer names it back, and an answer that names no key is no landing of it.

A write that Dispatch made and whose answer is gone is a `DispatchWriteLandedError`, with `code`, a `DispatchLandedCode`, and `status`.
On a route that stores what it is sent, these answers are a landing when they name the call's key back:

- `409 KEY_COMMITTED`: the write landed before its answer was lost, and Dispatch will not run it again.

- A replayed 2xx (`Idempotency-Replayed: ?1`) that reads `ANSWER_NOT_KEPT`: the first answer was larger than Dispatch keeps (`answerNotKept`, which the page shares).

- A 2xx that the write table settles on and whose body gives no row: the client's own `ANSWER_LOST` (`DISPATCH_ANSWER_LOST`), since Dispatch answers a write's 2xx only after the write committed.
  The table settles on a 2xx that names the call's key back and whose body, read whole, is JSON, or a 204 or a 205, which carries none (`answerEvent`).
  JSON that is not the row is such a landing, and so is a 204 or a 205 where the row needs a body.

Every `POST` and `PATCH` of the client reads its 2xx into the row that its callers read (`dispatch-write-rows.ts`).
A row holds those members, in the forms that the route answers.
A 2xx that names the key back and gives the row is the call's answer.

A landing is no refusal and no `DispatchServiceError`, and the client never asks it again.
Its message is the one line that a session reads.
It says that the method and URL were made, and why the answer is gone.
It then says to read back what it made with `dispatch read` or the matching list.

Three routes store nothing: the round trip of a text, the related-item check and the dry run of a result.
An answer of any of them that is not its answer made nothing, so it is never a landing.
That holds for a 2xx with no row, a 2xx that names no key back, a 2xx whose body is empty or not JSON, a replay whose first answer Dispatch did not keep, and a `409 KEY_COMMITTED`.
Each is a `DispatchAnswerMissingError` (`ANSWER_LOST`), which says that nothing was made and that asking again is safe.
A 204 or a 205 that names the call's key back is the answer of such a route: an empty text, since no row was to be lost.

## A 2xx that names no key back

Dispatch's keyed layer runs on every `POST` and `PATCH` route.
So a 2xx that names no key back, or another request's key, is never this call's answer, whatever it holds.
The client compares nothing of it with what the call sent, because an equal value does not tell this call's write from another call's.
A proxy's page, a server that is not Dispatch, a proxy or cache that answers one write with another's, and a proxy that drops the header give such an answer.
A Dispatch older than the keyed layer names no key back either, so each of its 2xx is such an answer.
A `409 KEY_COMMITTED` that names no key back, or another request's key, is no such answer either.
The write may have reached Dispatch behind what answered it, so its outcome is unknown, a `DispatchWriteUnsettledError`.
Check whether it took effect before you send it again.

## A 2xx whose body is not JSON, or does not arrive whole

Every answer that Dispatch writes to a keyed write is JSON, or a 204 or a 205, which carries no body (`WriteJSON`).
So a 2xx whose body is empty, or whose `Content-Type` does not say JSON, is no answer of Dispatch's either, whatever key it names back (`foreign-2xx` in `write-outcome.ts`).
A captive portal's 200 page is such an answer.
The write ends there, and where an attempt may have reached Dispatch its outcome is unknown, a `DispatchWriteUnsettledError`.
Its message says that something in Dispatch's place answered.

The client reads every answer's body inside its attempt, so a 2xx settles the write only once its body is read whole.
A 2xx whose body breaks off before it is read whole (`body-broken`) is asked again under the key, and Dispatch replays what it stored.
So is a 2xx whose body, read whole, is not the JSON that its `Content-Type` says (`body-malformed`), an answer cut short on the way.
A 2xx whose body the call's deadline or its caller's stop ends is that stop, with an attempt in flight: the write ends, and its outcome is unknown.
When the asking runs out after such an answer, the write ends with it as `last`.

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

- An answer of the airlock proxy: the command prints the error's message, and exits 1.
  A CI job's `dispatch result --project` prints the code first, as [`ci-results.md`](ci-results.md) says.

- A missing answer or an unknown outcome: the command prints the error's message, and exits 1.

- A flag whose value names nothing: the command prints the refusal, and exits 2.
