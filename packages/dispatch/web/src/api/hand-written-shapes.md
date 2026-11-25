# The shapes under `web/src/api` and `e2e` that are still written by hand

The Dispatch HTTP API's request and response shapes are generated: `envoy-dispatch openapi`
reflects the Go route table and the Go types each row takes and answers into
`packages/contracts/schemas/dispatch-openapi.json`, and every `bun install` renders that into
`packages/contracts/src/dispatch-api.generated.ts` (`bun run gen:api` in packages/contracts does
it again), which `@legion/contracts` re-exports (dispatch://INFRA-1406).

The refusal bodies are generated with the rest. Every route's default answer is the common
`{error, code}` (`ErrorBody`, Go's `errorBody`, which `writeError` writes). A route names each
richer refusal its handler writes (`routeShape.Refusals`), and every route that resolves an id
written short answers the prefix layer's `409 ID_AMBIGUOUS` (`IDAmbiguousError`, whose
`candidates` are `IDCandidate`s, Go's `idAmbiguousError` and `refs.Candidate`; `idRoute.refusals`
attaches it). `DispatchRefusal` is the union of every refusal body of every route, and both
clients read a refusal through it: `DispatchServiceErrorShape` in `@legion/contracts`, which the
dashboard's `ApiError` and the CLI's `DispatchServiceError` take. A test that reads a refusal body
itself, rather than through a client, takes it as its own route's (`Refusal` in `paths.ts`).

So a request or response shape written by hand here is a finding. This file is the list of what
is left and, for each entry, the one reason it is not a shape of that kind, so a reader checks
the grep's answer rather than deriving it again:

```
cd packages/dispatch && grep -rnE '^\s*(export )?(interface|type) [A-Za-z]' web/src/api e2e
```

A hit that is not an entry below, or not one of the classes below, is a shape that should be
generated.

## `web/src/api`

- **`client.ts`'s transport** — `FetchImplementation`, `UploadImplementation`,
  `UploadProgress`, `RequestBounds`, `WriteMethod`, `WriteRequest<T>`, `WriteBounds`,
  `ArtifactOwner`, `WriteVerb`. Not a response shape: these are the page's rules about how a
  request is sent and how its answer is kept, not what either carries - the silence deadline,
  the keepalive bound, the replay shape a kept write is stored as, and which of three routes an
  upload's owner picks.
- **`client.ts`'s projections of generated types** — `CreateArtifactReviewInput` is
  `Body<"/api/v1/artifacts/{id}/reviews", "post">` and `DeclineAskInput` is
  `Body<"/api/v1/asks/{id}/decline", "post">`, `ErrorCandidate` is the element of
  `DispatchServiceErrorShape["candidates"]` (every candidate shape a refusal carries), and
  `ReferenceSourceKind` is read off that route's own query schema. Not a response shape: each is
  read out of the generated document and declares no field.
- **`types.ts`'s `IssueCollaboratorLegionAgent`** — the generated `IssueCollaboratorAgent` with
  its `legion` entry narrowed to non-null. The two readers that keep the registry's sessions
  apart from the rest - the Collaborators tab's Legion sections and the composer's "Not running"
  options - narrow to it with a type guard of their own, so the narrowed shape is named once.
  Not a response shape: it declares no field the generated row does not, and is a refinement of
  it, as `MyReplyThreadRow` is of `MyReplyRow`.
- **`paths.ts`** — `PathsOf`, `Op`, `Json200`, `Json201`, `Answer`, `PathParams`, `QueryParams`,
  `JsonBodyOf`, `RequestBody`, `FormBody`, `Query`, `Body`, `Result`, `RefusalBody`, `Refusal`.
  Not a response shape: these are how a route method reads its own shapes **out of** the
  generated document; each is a projection of `paths`, and none declares a field.
- **`live-socket.ts`** — `LiveSocketConnection`, `LiveSocketState`, `DeliveryLiveVerdict`,
  `EventsHandlers`, `AgentHandlers`, `LiveSocket`, `PendingLoad`, the three `*Subscription`s,
  `Subscription`, `LiveSocketPill`. Not a response shape: they are the socket's own state
  machine, not a wire shape. The frames it carries are hand-written, in `@legion/contracts`, for
  the reason given below.
- **`event-feed.ts`'s `EventPages`, `query-refresh.ts`'s `RefreshTarget`, `RefreshCheck`,
  `DeferredCheck`, `SchedulerState` and `QueryOwner`, `queries.ts`'s `DeliveryLiveBodies`, and
  `reachability.ts`'s `Reachable`**. Not a response shape: they are the page's cache, its refresh
  scheduler and its reachability map, which hold answers rather than describe one.
- **`query-client.ts`'s `ReadHold`, `EndReadHold`, `HeldReads` and `ReadHolds`, and
  `write-events.ts`'s `WriteEvent`** — the holds a write keeps on the cache's reads while it is
  out (`holdBackReads`): the queries it holds back, the live event that ends the hold, the call
  that ends it, and the set of holds a client keeps. Not a response shape: they are the cache's
  rule about when a held read may be asked again, and `WriteEvent` is a test of a `DispatchEvent`
  (below), not a body.
- **`corrected-post.ts`'s `CorrectedPost`, `PostMark`, `KeptCorrection`, `FamilyWrite`,
  `RefusalWrite`, `RenamedVersion` and `PostEvent`, and `queries.ts`'s `ReferenceTargetsAnswer`**
  (main's #3512) — how the cache writes a comment's or a message's correction into every answer
  that holds a copy of it. Not a response shape: `CorrectedPost`, `PostMark` and `KeptCorrection`
  are the cache's record of a correction and what it was kept for, `FamilyWrite` and
  `RefusalWrite` are functions, `RenamedVersion` and `PostEvent` are read out of the generated
  event payloads, and `ReferenceTargetsAnswer` is the names read's cached answer, the generated
  `ReferenceTarget`s of every batch the query sends joined into one list.
- **An imported or re-exported name is a hit of the grep and not a declaration of its own.**
  `IdempotencyLandedCode`, `InboxThreadKind`, `DispatchServiceErrorShape` and `Sleep` in
  `client.ts` are `@legion/contracts`', and the `paths.ts` names it imports are that file's; `Query`,
  `QueryKey`, `QueryFunction`, `QueryFunctionContext`, `QueryObserverOptions`,
  `DefaultedQueryObserverOptions`, `DefaultError`, `RefetchOptions`, `RefetchQueryFilters` and
  `Updater` in `query-client.ts` and `queries.ts` are TanStack Query's; and `FakeLiveSocket` and
  `FakeLiveSockets` in `live-socket.test.ts` are `web/src/__tests__/live-socket-harness.ts`'s,
  the stand-in socket that test drives its subject with and the set of them a test installs. Not a
  response shape: each is read here under the name of the package or module that declares it.
- **A test file's own type is not a wire shape either**: `query-refresh.test.ts`'s `ServedQuery`,
  `argument-refusal.test.ts`'s `Refusal` and `corrected-post-families.test.ts`'s `Binding`,
  `CopyMarks`, `KeySite`, `Source`, `WriteRow` and `Resolved`. Not a response shape: each is the
  stand-in or the table a test drives its subject with.

## `e2e`

- **`api.ts`'s harness half** — `ApiOptions`, `CachedSession`, `CallArgs`, `RowKey`, and
  `ApprovalRequested`, which is `Result<"/api/v1/artifacts/{id}/approval-requests", "post">`.
  Not a response shape: the first four are the lane's base URL, the dev sign-in's cookie cache,
  the argument record the typed call seam takes and the branded project key `createProject` takes
  (`rowKey`, `fixedKey`), and the last is read from the document.
- **Every other declaration under `e2e/`** is a fixture or a harness type of the test that holds
  it (`FakeSession`, `Seeded`, `ProbeLine`, the Playwright re-exports). Not a response shape:
  none is a Dispatch request or response.

## The wire shapes that stay hand-written in `@legion/contracts`

These are not under the paths the grep reads, but they are the shapes a reader asks about next.
Each is in `packages/contracts/src/dispatch-api.ts` or the modules it re-exports:

- **The live-socket frames** (`LiveSubscribe`, `LiveServerFrame`, `LiveEventFrame`, the
  `DeliveryLive*` and `EventsLive*` families). Not a response of the route table: they travel
  `GET /ws/live`, which is no row of the table the document is emitted from, and
  `packages/contracts/fixtures/live/` with the Go diff (`delivery_live_diff_test.go`) holds them
  against the server instead.
- **The Going SSE frames** of `GET /api/v1/going/requests/{id}/stream`
  (`packages/contracts/src/going.ts`). Not a JSON response: that route answers a
  `text/event-stream` of `id:`/`event:`/`data:` lines, for whose items OpenAPI 3.1 has no schema,
  so its row is `Opaque` in `routeShapes` and `going.ts` types each frame's `data:`.
- **`DispatchEvent` and the event payloads.** Not a response shape: Go's `model.Event.Payload`
  is bytes, so the generated `Event` an event read answers says `payload: unknown`, and
  `DispatchEvent` is the reading of those bytes per event type, which travel the message bus
  rather than an HTTP body and which every event read is typed as.
- **The closed string enums** (`AskUrgency`, `IssuePriority`, `CheckWhen`, `IssueRouteStatus`,
  and their kind) and the `const` arrays they are read from. Not a response shape: each array is
  a value and not only a type - `dispatch-tools.ts` builds the MCP tools' `z.enum` from it and
  the dashboard renders its dropdown from it, and a schema in the generated document gives a
  type and no value. The document still widens these fields to `string`, because their Go
  fields carry no `enum:` tag; a surface that needs the closed list narrows a value to it at the
  boundary rather than widening itself. A Go field that does carry an `enum:` tag is the other
  case and belongs to no list here: the emitter renders its set as a closed union, so the field
  arrives narrowed and nothing widens or re-narrows it by hand (`IDCandidate.kind` is one).
  `grep -rn 'enum:"' packages/envoy/internal/dispatch` is the set, which is why this file names
  none of them.
- **The helpers no route answers** — `Advised<T>` and `DocumentAdvised<T>` (the advice envelope
  is a merge the document writes as an `allOf`, and the two differ in which advice a write
  carries), `ArtifactVersionBlob`/`ArtifactVersionContent` (the byte arm of a version download is
  a transport value, not a wire shape), `IssueRead`, `CreateArtifactInput` (it carries a `Blob`),
  `AuthenticatedUser` (`GET /auth/whoami` is outside `/api/v1`). Not a response shape of the
  table: none is the body of one of its rows - the two generics read every write's generated
  `allOf` at once, and the rest are a download's bytes, a client's composite of two reads, a
  request, and the answer of a route the table does not hold.
- **The Inbox row's two narrowings** — `MyReplyThreadRow`, `MyReplyAgentRow` and the
  `inboxThreadRow` that tells them apart. `GET /api/v1/me/replies` answers both kinds of row
  under one Go struct, so the generated `MyReplyRow` carries each kind's fields as optional and
  `kind` as the open string the Go field holds; these two say what a row of each kind always
  carries, as `IssueAskThreadRow` does for an ask read with its thread. Not a response shape:
  neither declares a field the generated row does not; each is a refinement of it.
- **`InboxDismissSeenReply`, `InboxDismissSeenAgent` and `InboxDismissSeen`** — what a dismissal
  carries per row in `InboxDismissRequest.seen_replies`. The route reads each entry in the shape
  its own row's kind takes and refuses it in its own words, so Go holds them unparsed
  (`inboxDismissRequest.SeenReplies` is a `map[string]json.RawMessage`) and the document can only
  say the map's values are bytes; these three are what each one must be. Not a response shape:
  they are a request's entries.
- **`DispatchServiceErrorShape`** (`dispatch-api-requests.ts`) — the generated `DispatchRefusal`
  read as one record: every field any refusal carries, each optional and typed as the refusals
  that carry it say. Not a response shape: it is a mapped type over the generated union and
  declares no field, so a refusal body a route grows or changes reaches it by regeneration.

## What is not generated, and is not an exception either

The error **code** strings. The route table records a route's codes only in its prose, so making
them machine-readable is a change to the table's own content, which dispatch://INFRA-1406 puts
out of scope; `ID_AMBIGUOUS` is one, which `scripts/gen-go.ts` renders into Go as
`contracts.IDAmbiguous`. The error **bodies** are generated, every refusal richer than
`{error, code}` included.

Headers are not in the document either — `Idempotency-Key`, `Dispatch-Login`,
`Idempotency-Replayed`, `Retry-After`, `If-None-Match`. They are the client's rules about how a
request is sent, and they live in the transport above.
