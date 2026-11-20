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
Its message names the method and the request's URL as built, where the configured URL is a plain http(s) URL, and that URL's fixed phrase otherwise (`requestForText`, `@legion/contracts/log-text`).
The bearer is a header, so no message names it.
The message then names the status and the reason phrase, a one-line plain-text excerpt of the body, and what asking again can do.
The excerpt drops scripts, styles and tags, and it reads at most the first 64 KiB of the body, in linear time.
In the excerpt and in the reason phrase, the client redacts the value after `Authorization:`, `Bearer` or `DPoP`, and every value shaped as a JWS (`eyJ…`, a proof or a JWT) wherever it stands.
It also redacts each run of 8 or more characters of each token it presented lately (its credential's now, and those it sent before a renewal: `SentTokens` in `excerpt.ts`), and a shorter token only whole.
So a copy of a token that is cut short, split by markup or overlapped by another copy leaves no piece behind.
The message names a body with no text in the part that the excerpt reads by its size in bytes.
Only a body with nothing in it is "an empty body".

A read's 2xx is no answer of Dispatch's either where its body is empty, not typed JSON or not JSON, but for a bodiless 204 or 205.
Every route the client reads as JSON answers JSON, so such a read is a `DispatchGatewayError` too, and a retry may succeed: a proxy or a captive portal in the way can be got past.

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
It is a `DispatchGatewayError`, whatever its body is, and no request reads that body: it is cancelled (`redirectUnread` in `dispatch-http.ts`).
Its message names the request as for any gateway's answer, then the status and the reason phrase, and the origin that the redirect points at: `answered 302 Found (a redirect to <origin>, which this client does not follow)`.
That origin is the scheme and the host of the `Location`, with a port that is not the scheme's own (`redirectOrigin` in `excerpt.ts`).
The message holds nothing of the target's path, query, fragment or user, and nothing of the answer's body.
A redirector writes its target into the body too, as text or as the text of a link, and that target can carry a secret.
A probe answered with a redirect says nothing of the keyed layer.

The listener's client refuses a redirect the same way.
An `EnvoyApiError` of a 3xx names the method, the URL as a text names it, the status and the origin, `(a redirect to <origin>, which this client does not follow)` (`transport.ts`).
It names nothing of the body, which is cancelled unread.
The pool identity of `@legion/broker-client` names a 3xx of the security token service by its status alone, as `HTTP <status>`.
The GitHub Actions identity, of `@legion/broker-client` and of the Go broker client, follows no redirect of the runner's OIDC token endpoint either, since that request carries the runner's bearer, and names a 3xx by its status alone.

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

How a write ends is one transition table (`write-outcome.ts`); the README ("Dispatch") says how it reads each answer and when the client asks again under the key.
Before anything of the write may have landed, a write that ends throws that end's own error: the refusal, the transport's failure, the credential's, the deadline's or the stop's.
Once an attempt may have landed, the write ends as a `DispatchWriteUnsettledError`, unless an answer settles it or a landing is known.
That error is deliberately not a `DispatchServiceError`, because a caller reads a `DispatchServiceError` as a refusal that Dispatch decided.
Its message gives the last answer or failure and the number of attempts.
It then says whether an earlier attempt may have reached Dispatch, or whether Dispatch was still running one.
It says that the write may or may not have taken effect.
Where the call saw the keyed layer judge the write, the message says that Dispatch finishes an attempt that it received.
It then says to read the write back before you send it again, because a new command sends it under a new key.
Otherwise it says to check whether the write took effect before you send it again.
Its `key` is the key every attempt carried, and its `last` what ended the write.

A write that ends with no answer at all names the request.
The client cut off its last attempt for silence ("made no progress for 20 s, so it was cut off").
Or the call's deadline ended it, while an attempt was out or after one that may have reached Dispatch.
The message then says that the write "had no definitive answer when its deadline ended it".
The client does not send an attempt that the deadline ends before it goes out.
Neither that attempt nor a credential that could not present counts as an attempt that may have reached Dispatch.

The keyed layer's verdict on the key settles a write that may have landed: the stored result, a replay, a landed answer, or the handler's refusal named back, `422 KEY_REUSED` among them.
An echoed 401 is no verdict, because a handler gave it, perhaps after it committed, and the layer stores no 401.
A no-route answer, Dispatch's check of the token, a dead token or a credential that could not present proves nothing of the earlier attempt.

`writeOutcomeUnknown(error)` says whether a write's error leaves its outcome unknown.
It does for a `DispatchWriteUnsettledError`, for a gateway's answer with `mayHaveReachedDispatch`, and for Dispatch's own 5xx.
A connection that never opened reached nothing: the host refused it, its name did not resolve, or no route reached the host (`DispatchUnreachableError`, whose `code` is the failure's class, such as `ECONNREFUSED`).
A request that Dispatch's check of its broker token turned away before any handler ran reached nothing too.
Those answers are `429 BROKER_TOKEN_RATE_LIMITED` and `503 BROKER_UNAVAILABLE` (`tokenNotChecked`), and a write asks again after them as after a 429.
A refusal carries its `Retry-After` as `retryAfterMs`.

## A write that landed with its answer gone

Dispatch's keyed layer names the request's `Idempotency-Key` back on every answer that it gives a keyed write.
So an answer that names this call's key back is Dispatch's word about this write.
A key that a caller gives is the call's key: `docEdit` and the upload of a project's document take one.
An empty key names no request, so no answer names it back.

A write that Dispatch made and whose answer is gone is a `DispatchWriteLandedError`, with `code`, a `DispatchLandedCode`, and `status`.
On a route that stores what it is sent, these answers are a landing:

- `409 KEY_COMMITTED`: the write landed before its answer was lost, and Dispatch will not run it again.
  One that names the call's key back is a landing; so is one that names no key back where the process knows nothing of the keyed layer, as from a Dispatch without it.

- A replayed 2xx (`Idempotency-Replayed: ?1`) that reads `ANSWER_NOT_KEPT`: the first answer was larger than Dispatch keeps (`answerNotKept`, which the page shares).

- A 2xx that names the call's key back and whose body never gives the row: the client's own `ANSWER_LOST` (`DISPATCH_ANSWER_LOST`), since Dispatch answers a write's 2xx only after the write committed.
  JSON that is not the row, a body cut short, and a bodiless 204 or 205 are asked again under the key, and Dispatch replays what it stored; the asking spent, the write landed with its answer lost.
  A body that is no JSON whatever its type says, or empty, ends so at once.
  So does every later end of a write that heard such a 2xx: a deadline or a stop that cuts its body, or the attempts after it.

Every `POST` and `PATCH` of the client reads its 2xx into the row that its callers read (`dispatch-write-rows.ts`).
A row holds those members, in the forms that the route answers.
A 2xx that names the key back and gives the row is the call's answer.

A landing is no refusal and no `DispatchServiceError`, and the client never asks it again.
Its message is the one line that a session reads.
It says that the method and URL were made, and why the answer is gone.
It then says to read back what it made with `dispatch read` or the matching list.

Two routes store nothing: the round trip of a text and the related-item check.
An answer of either that is not its answer made nothing, so it is never a landing.
That holds for a 2xx with no row, a 2xx that is no answer of Dispatch's, a replay whose first answer Dispatch did not keep, and a `409 KEY_COMMITTED`.
Each is a `DispatchAnswerMissingError` (`ANSWER_LOST`), which says that nothing was made and that asking again is safe.

## A 2xx that names no key back, or another request's

A 2xx, or a `409 KEY_COMMITTED`, that names another request's key back came through a keyed layer, and not for this call: a proxy or a cache that answers one write with another's.
It is never this call's answer, whatever it holds and whatever the process knows of the keyed layer.

One that names no key back is no answer of this call's where the process knows this Dispatch runs the keyed layer: an answer of it named a key back, or its `/healthz` said `idempotency_keys: true`, within the last five minutes (`KEYED_LAYER_MEMORY_MS`).
Such a Dispatch names the key back on every answer it gives a keyed write, so the answer came from something in its place: a proxy's page, a server that is not Dispatch, or a proxy that drops the header.
Where the process knows nothing of the layer, it is what a Dispatch without the layer answers: a 2xx whose body is the row is the call's answer, and a `409 KEY_COMMITTED` is its landing.

The client compares nothing of such an answer with what the call sent, because an equal value does not tell this call's write from another call's.
An answer that is no answer of this call's leaves the write's outcome unknown, a `DispatchWriteUnsettledError` whose `last` is a `DispatchGatewayError` of its status (`HTTP_<status>`), since the write may have reached Dispatch behind what answered it.
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
