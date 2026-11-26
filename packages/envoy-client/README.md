# Envoy client

Shared HTTP transport, tool contract, inbound delivery renderer, and consumer delivery for Envoy
hosts. pi-envoy and claude-envoy consume these modules rather than maintaining host-specific
envelope parsers.

This package has no release of its own, so nothing renders its changelog entries: each stays
readable as the fragment file that holds it, under `.changes/unreleased/`. `CHANGELOG.md` is the
history before that and stops where the fragments begin.

## Running the suite

`bun run test` runs the suite with `--timeout 30000`, because Bun's own per-test default is
5,000 ms and nothing here wrote it, sized it or explained it. On a busy machine that default is a
limit on a row's work rather than on a hang: measured in a 2-CPU pod under 16 busy loops
(8 x nproc), `dispatch-cwd.test.ts`'s row that kills a child ignoring SIGTERM reached 5,000.89 ms
and failed, where it costs 0.14 s idle and 0.31 s with its own file alone. A row that hangs still
fails the suite; it takes 30 s to say so rather than 5 s.

Bun reads that number from the command line alone. `bunfig.toml` has no `[test] timeout` key, a
preload that calls `jest.setTimeout` governs the first test file of a run only, and
`BUN_TEST_TIMEOUT` is not read, each of the three measured on Bun 1.4.4. So a bare `bun test` here
still runs under Bun's 5,000 ms, and every row whose own work reaches half of that carries its own
limit as the third argument to `test`, with what can hang and the two costs that sized it written
beside it. Each row's comment is the record of its own number.

A row whose limit is a bound on work rather than a hang guard says so beside its number, and a row
whose limit must stay below something the row must not wait out says that too.

## Inbound delivery

`renderInbound(raw, sessionID, subject?)` accepts an additive Envoy envelope and returns a TOON
block with recognized routing and delivery fields. The renderer includes `to` only for a direct
message to the reader, formats sender identity and reply metadata, renders structured payloads
once as `message`, and marks unknown sources or fields with `unrecognised`.

A payload the TOON encoder cannot lay out is shown as the text its sender sent, under `message`
(or `dispatch`): one nested deeper than the encoder's recursion reaches, and one whose layout
passes the card's bound, as an array chain 1,400 levels deep does from a 3 KB frame. The bound is
`INBOUND_CARD_MAX_BYTES` (1 MiB, the hub's largest frame), or twice the text the card lays out
(its frame and a body read whole) when that is more, so a comment Dispatch's API gives whole
still lays out whole. The result's `payloadShownAsText` says which. Each host logs that once,
with the message id, and takes the frame: a throw out of the renderer would fail every delivery
of it. A payload that is text already is never held to the bound; its frame bounds it.

Malformed input is safe: a non-JSON frame becomes a TOON block naming its topic and an
`unrecognised` parse failure. The renderer never emits raw frame bytes. Dispatch envelopes
(`source: "dispatch"`) render through the same TOON path as every other source, nesting the
parsed event under `dispatch: { owner, type, actor, payload }` with `payload` typed per event
type. The card's `payload` leaves out each field that states nothing (`null`, `""`, `false`,
`[]`, or an object left empty; a list keeps every item), each field that repeats the card
(`issue_key`, an `author` or `actor` each of whose fields is the event's `actor`'s, a `target`
naming this session on a card whose `to:` names it, a `created_at` less than a second from the
time Envoy issued the card, whose `at` is that time cut to the second, and a comment's
`thread_root_id` and `answers` equal to its `reply_to`), and Dispatch's own outbox and client
bookkeeping (`deliveries`, `thread_logins`, `references_changed*`, `mentions_decide_routes`,
`suppress*`, an issue's board `rank`). A field whose value is the fact even when empty stays as
written: each field an update's `changed` names, a present `suggestion`, and a present `forced`.
A session actor's `origin` keeps only its `session_title`, and an event on no issue and no
document, as a direct message is, prints no `owner`. A payload whose shape disagrees with
its event type's schema keeps its raw parsed fields under the same rules, and a frame that is no
Dispatch event is shown as its raw parsed value under `dispatch`; neither is dropped or reduced
to prose. A dispatch envelope with no payload at
all falls back to the same `summary` field every other source gets and marks `unrecognised:
payload`. On a `notifications.dispatch.*` topic, which carries every event of its owner, the
renderer skips the events Dispatch marks `quiet` — a delivery receipt, an anchor refresh,
settings or per-viewer bookkeeping, a targeted message its session already had, an ask edit that
only moved a version, or a version nobody named — before a host can steer or record them in its
inbox, as well as events authored by the reader's own session. It does not skip on `notify`,
which answers whether to wake the owner's route and is false for every event an agent wrote: a
session that subscribed to an issue's topic subscribed for exactly that activity. A frame with no
`quiet` was published before Dispatch stated it and is judged by `notify`, so a frame from the 72
hours the stream still holds shows no receipts.

## Consumer delivery

`createConsumerDelivery` (`@legion/envoy-client/consumer-delivery`) is a session's side of the
listener's relay of its durable consumer (the "Session consumers" item of "Runtime shape" in
`packages/envoy/docs/architecture.md`). A
host makes one per session, subscribes its `subject` (`notifications.envoy.deliver.<session id>`)
before it registers, passes `registration()` as the registration's `delivery` field, and gives the
registration's answer and the topics it carried to `answered`, which returns `consumer`, the one
mode there is, and
records the stored topics the listener relays (`relays`, `relaying`). It hands `receive` each relay message. The delivery decodes the frame's
`Envoy-*` headers (`decodeRelayFrame`), answers `received` on the frame's reply subject before any
host work, and hands the host the frames one at a time in stream order
(`ConsumerDeliveryHost.deliver`): the next waits until `deliver` returns or the host calls
`release`, `done` or `nak`. The host calls `done` once it wrote the frame, or skipped or refused
it, and `nak` when the session cannot take it; `progress` goes out every 20 s until then. A frame
whose `deliver` throws is answered `nak`, and from its third delivery the delivery ledger's
`liveFailed` shows its failure notice instead; the frame is answered `done` when that notice
reached somebody and `nak` when it did not, so a give-up nobody was told of waits on the stream
for whoever takes the session next. A frame the ledger holds, one on
a subject no followed topic matches, and one stored before its topic was followed (`setFollowed`)
are answered `done` and never handed over. A frame delivered
before, or stored before the current epoch began, is `late`, and a host runs no side turn for it.
`beginEpoch` starts a new epoch for each new NATS connection. `resumePoint` is the stream id and
the sequence below which the session holds nothing unwritten; `snapshot` and `restore` carry it
and the follow times across a restart, and the `persist` option is offered a snapshot whenever it
changed, so a host writes only changes. A session with no resume point of its own has its first
consumer created at the first message the stream holds on its own agent subject, and at the
stream's end only when it holds none, so a message stored for it before it registered reaches it.
A registration answer that reports a gap
or names another stream, and a frame that carries `Envoy-Gap`, call the host's `gap` once. `close`
answers every unsettled frame, and every later one, `nak`.
`streamStores` (`@legion/envoy-client/stream-topics`) says whether the stream stores a topic; the
listener refuses to register a `notifications.` topic it does not, so every followed topic is one
the relay carries.

## The delivery ledger

`createDeliveryLedger` (`@legion/envoy-client/delivery-ledger`) is what keeps a frame from being
shown twice: the session's consumer redelivers everything it has not acknowledged, and the ledger
holds what the host already wrote. A
host makes one per session process and hands it its live path's `createDeliveryDedupe`, the
listener client and callbacks (`DeliveryLedgerHost`): the session id, the followed subjects, how
to show a failure notice and a one-time notice, and how to
persist a change. Role forwards reach a session as stored copies on its own subject, so they go
through the ledger like any other frame.

The ledger holds the identity of each frame the session finished with (its `event_id`, and its
`dedupe_key` when that names its event, `ledgerIdentity`), each expiring 72 hours after it
was recorded, at most 20,000, the oldest dropped first. A host calls
`recordDelivered` once it wrote the frame to the session's record, before it settles the
claim, and drops a frame `delivered` finds. A `createDeliveryDedupe` claim is settled (`settle()`)
once the host is done with the frame, shown or not, or released as before; until then `pending()`
reads it true. A host that hands a frame, or a failure notice,
on without having
written it yet (Oh My Pi holds a card sent mid-turn until the turn takes it) claims the frame and
answers `queued`: nothing is recorded, so a process that
ends first is sent it again after the restart, until the host calls `recordDelivered` and settles the
claim once written. `recordDeliveredFor(sessionID, frame)` records the same way for the session a
frame was delivered in while the host shuts down: it writes while the host's optional
`writeSessionID()` still names that session, once `sessionID()` already answers none, and the
ledger holds the frame from the call on (Oh My Pi records every BTW's event id so, before its side
turn and before any reply is posted). It answers whether the write that keeps the record reached
the host, so a caller told `false` - the host moved while the write waited behind the writes
before it, or the write failed - reports the loss rather than settling the frame in silence.

The state holds the topics the session follows, so a host can follow them again after the
listener reaped its interests;
`restore(snapshot)` loads what a host persisted, dropping expired ledger entries and keeping the
state at any age. A write carries the state only when it changed. A frame the host fails to show is answered `nak` and sent again, and
on its third delivery the host shows a notice naming it instead (`showFailureNotice`); a host whose
delivery path failed a frame outside the relay calls `liveFailed(sessionID, envelope, error)`,
which shows that notice at once, to the session the frame was addressed to and to no other: a
frame that failed because the host moved on names a session that is closed here now, and nothing
is shown or recorded for it then. It answers whether the frame was reported, so the relay settles
a give-up nobody was told of with `nak` rather than acking a message that then exists nowhere; a
notice shown to the session it names is reported even where the host moved before it resolved,
and only its record waits for a host that is still on that session. `failureNoticeFields` words
that card, and
`envoyNoticeText` renders a notice's fields under `envoy:` in TOON, so a sender's summary that
holds a newline stays inside its own field. Work started for one session stops, writing nothing, once
`sessionID()` names another.

The host policy both plugins share sits in `@legion/envoy-client/stream-topics`, so each host keeps
only how it shows a card, how it warns, and how it persists. `replayFollowedSubjects(
ownAgentSubject, topics, heldForRole?)` is what a host's `followedSubjects()` answers, which is
what a restart follows again: its own agent subject and every topic it follows but a role lane, a
subject it holds for a role, and a topic reaching a private stream subject.
`reachesPrivateStreamSubject(ownAgentSubject, topic, roles?)` is the listener's own rule,
`privateStreamSubjects` and `ReachesPrivateStreamSubject` in
`packages/envoy/internal/bus/stream.go`, which a host's registration applies too: another session's
agent subject, a failed-delivery lane on an agent subject or a team topic, a controller notice lane
of a project whose controller role `roles` does not name, or a wildcard reaching one. A session
holding that role registers its own project's lane, since its consumer is the only way the notice
reaches it (`controllerNoticeRole` names the role a lane is for). `rememberedTopics(topics)` is what
a restore follows again from a persisted state: every subject but agent subjects and role lanes.
`streamDeliversNothing(topic)` is the topic a registration must not carry: a `notifications.` topic
no stream subject captures and no role lane, which the listener leaves out of the row it stores,
naming it in a warning. `refuseUncapturedTopics(sessionID, ownAgentSubject, topics)` throws that
warning's own wording for every topic a caller named by hand that a host's subscribe tool refuses,
which is such a topic unless it reaches a private stream subject, where the listener's own warning
says so instead. The sentence is `@legion/contracts`'s `uncapturedTopicRefusal`:
`packages/contracts/scripts/gen-go.ts` emits it as the `contracts.UncapturedTopicRefusal` the
listener's warning returns, so a reword reaches both languages from one place.

## NATS connection

`natsConnectionOptions({ servers, name, credential })` (`@legion/envoy-client/nats-connection`) is
the nats.js `connect` options every session plugin dials with: the credential `natsAuthOptions`
builds from the resolved `NatsCredential` (`src/nats-auth.ts`, which the package does not export, so
a plugin dials only with these options), a dial timeout of 10 s (`NATS_DIAL_TIMEOUT_MS`),
reconnecting forever (2 s apart for a seed or no credential), and a ping every 10 s
(`NATS_PING_INTERVAL_MS`) with the connection dropped on the tick that finds two pings unanswered
(`NATS_MAX_PINGS_OUT`). So a
path that freezes without closing is left within 30 s and redialed at once, and a session is back
within 42 s when that redial also hangs for its 10 s timeout and the 2 s wait (a broker-token
session lost less than a minute after its last dial: within 50 s); nats.js's
default 120 s interval left a frozen path for up to 6 minutes. A session that presents its broker
token never gives up over a refused credential, and redials on `brokerRedialDelays` instead of
2 s apart: at once when it loses a connection that lived a minute or more past its last dial,
then after waits drawn anew between half and all of 4 s, 8 s, 16 s, then 20 s for good
(`brokerRedialWaitBounds`), so a hub that refuses every session at once is asked again about 73
times a second by 1,100 of them rather than 550, and each one's next redial comes within 20 s of
the hub admitting again. A connection lost less than a minute after its last dial (for one that
was admitted at once, the dial that opened it) keeps its place in the backoff: its first redial
waits out the rest of the wait drawn at that dial. A token the connection has yet to present (a
renewal, a session switch) goes out within about a second (`BROKER_REDIAL_POLL_MS`), in the middle
of a wait too, once for each token, between the planned redials, whose schedule it does not move.
A token of another session than the last one dialed (a switch) goes out so whatever the wait; a
renewal goes out so at most once between two planned redials while the hub refuses, and once more
after each admission (`BrokerNatsCredential.admitted`). A seed or no credential keeps the flat 2 s.
A plugin opens its connection with
`connectNats(connect, options, plan, hooks)`: one bound around the whole `connect()` (the name
lookup, every address and every server, which nats.js times one at a time), and a failed dial
dialed again 250 ms later, doubling to 4 s, inside a budget (`NATS_DIAL_PLAN`, `natsDialBudget`),
unless no redial can cure the failure. Under a broker token (`hooks.credential`) a hub's refusal
waits on the broker-token backoff instead, 2-4 s, 4-8 s, 8-16 s, then 10-20 s, and a token it
refuses twice is dropped and another trade started at once, once for the session until a dial is
admitted, on the count the credential keeps and its keeper shares
(`BrokerNatsCredential.refused`); the host logs each drop as it is made, whether or not another
dial follows (`hooks.onTokenDropped`, `NATS_DIAL_TOKEN_DROPPED`). A server that closes a dial
during its
handshake, as
nats-server does to a CONNECT that comes more than 2 s after its INFO, fails it at once
(`patches/nats@2.29.3.patch`). A failed connect is logged as
`natsDialFailure(named, error, presenting)`: `could not connect to NATS at <servers> presenting
<credential>: <reason>`, built once for every caller (pi-envoy, the channel server, the e2e bridge,
`nats-stream` and `dispatch-user-turns`). The servers are named by the setting that holds them
(`natsServersForLog(setting, count)`: `the server ENVOY_NATS_URL names`, `the 2 servers
envoy.natsUrls in <file> names`), never by any part of their text: a server's text can be a
credential, and every rule that tried to tell a host from a secret by its spelling missed one. The
reason (`natsFailureReason`, `./errors`) is an allowlist, since nats.js passes the runtime's own
errors on and those quote what they were handed, and keeps a server's `-ERR` text whole, which a
peer that is no nats-server can fill with the CONNECT it was sent: nats-server's fixed words for its
refusals (`Authorization Violation`, `User Authentication Expired`, `Account Authentication
Expired`, `Authentication Timeout`) and otherwise that the server's text is withheld, nats.js's code
for its other errors (`CONNECTION_REFUSED`, `TIMEOUT`), the syscall and code of a runtime error
(`getaddrinfo EAI_AGAIN`, `ERR_SOCKET_BAD_PORT`, `ERR_INVALID_URL`), the message of a
`LoggableError` (this package's own reasons and a caller's; it keeps no cause), and for anything
else its type and `(message withheld)`. `natsDialedAddress(server)` is the host and port nats.js
itself dials (its own `hostPort`), which a caller that checks where a server leads reads, never a
log.

A session's NATS credential (`@legion/envoy-client/nats-credential`) is one of three: the nkey user
a seed names, the session's broker token, or none. The broker token is the session's own, the one
its listener calls present too where they present a broker token rather than a bearer
(`brokerTokenFor`). It goes on the CONNECT as its token, with a proof of its key over the nonce the
server sent that one connection as its signature
(`DpopKey.connectionProof`), which is what the auth service of a hub that runs one admits as that
session alone. `brokerNatsCredential(env, session)` holds it: `ready()` takes a live token before a
dial in one of three modes, each one wait on the token client's retry loop. By default it fails
fast: a failure the loop trades again on fails it at once, naming that failure, and a pod session
not registered yet is waited out for 20 s (`TRADE_DEADLINE_MS`, nats.js's own dial timeout).
`ready({ waitForReady: true })`, an Oh My Pi session's start, waits for ready for at most those
20 s, through every failure the loop trades again on, and then fails naming the last one: in a
pass of 30 s, the floor of the pass's deadline (`ENVOY_PASS_DEADLINE_MS`; by default it is the
heartbeat, 90 s), 20 s leaves 10 s for the rest of the pass, and a start bounds it by its own
pass deadline less those 10 s where that is shorter, or by half a pass shorter than 20 s; and it
is longer than the loop's longest first wait, 15 s, so a first trade that fails at once is traded
again inside it. `ready({ life })`, a Claude Code channel server's start, waits for ready until a
token lands, a refusal no trade cures ends the loop, or `life` ends. `authenticator(nonce)` signs
one snapshot of it per CONNECT and presents nothing, starting a trade, where it holds no live
token, and `renew()` takes the token again, the next session's after a switch, failing fast
unless asked to wait for ready, as a switch's follow (`followNatsCredential`) does. A server that
sends no nonce runs no auth service, so nothing there checks a broker token:
the CONNECT fails, naming that and the setting to undo for that hub (`unset NATS_AUTH`, or take
`natsAuth` out of the file that set it; `noAuthService`), rather than going out with no credential.
`keepNatsCredential(connection, credential, warn)` renews ahead of each token's expiry and
reconnects the connection with each new token, before the server drops the user the old one was
admitted as; a renewal that fails is retried at 2 s doubling to a minute and warned about once,
then every 30 minutes, never naming the token. It reads the connection's status too: a server
that refuses the live token in hand is warned of at once, and at that token's second refusal the
keeper drops it (`BrokerNatsCredential.refused`, here and in the token client) and trades for
another. Refusals are counted per token (`tokenGeneration`), and a drop is made once per session
until the connection is back (`tokenSession`), so a session switched to has its revoked token
dropped in its turn, and an auth service that is away costs each session one trade for the
outage. `followNatsCredential(connection, credential,
boundMs?)` is a session switch's: it takes the token of the session the name reads now and, where
that is not the token the connection presented, reconnects and returns only once the connection
is back admitted with it, so a caller subscribes the new session's subjects after it, never
before (the server refuses a subscribe the old session's user was not granted, and nats.js
closes that subscription). It takes at most `boundMs`, `FOLLOW_DEADLINE_MS` (28 s) by default:
the trade all of it but the connection's share, 8 s or half the bound.
The module imports nats.js's types alone, since every bundle that reads `envoySettings` imports it.

nats.js 2.29.3, the last 2.x release, leaves a redial's socket open when it gives up on that dial
at its dial timeout (a TCP connect that hangs, or a server that never sends `INFO`): it closes
only a transport that finished connecting, so the socket keeps the process alive and a channel
server never exits on SIGTERM. Its stale-connection close (two unanswered pings) flushes the
transport before it destroys the socket, and behind data a dead path never acknowledged that flush
never completes, so the transport never closes and a client that wrote since the path froze never
redials. `patches/nats@2.29.3.patch` (root `package.json`'s `patchedDependencies`, applied by bun
at install and so carried by both plugins' bundles) makes `NodeTransport` keep the socket from the
moment it dials and destroy it when a transport that never connected is closed, and makes the
stale-connection close skip the flush (`transport.close()` in place of `transport.disconnect()`
in `ProtocolHandler.disconnect()`, which the stale-connection close after two unanswered pings and
`handleAuthError` with no `connectError` handler both call, while a client-initiated `reconnect()`
still flushes); `src/__tests__/nats-stall.test.ts` fails without either. Bun ignores a
`patchedDependencies` entry for a version that is not installed without a word, so moving nats
means carrying the patch to the new version.

`@legion/envoy-client/test/stall-proxy`, `@legion/envoy-client/test/envoy-stack` and
`@legion/envoy-client/test/stall-scenario` (Bun only, not built, in `test/`) are what the stall
tests run: a TCP proxy whose `freeze()` stops forwarding on, and stops reading, the connections
open at that moment without closing either side, and which records when each connection was
accepted, first subscribed and closed; a real natsd and listener that each stack builds with
`go build` from `packages/envoy` into its own scratch directory (its `monitorUrl` is natsd's
monitoring port); and `runFreezeScenario`, the freeze both plugins' stall tests put their session
through. The stack's listener answers `consumer`, so the scenario checks consumer delivery across
the freeze: the connection nats.js dials in place of the frozen one subscribes the relay subject
again (`/connz`), the session's `envoy_sessions` entry names a new epoch, and the message sent
during the freeze and one sent after it are each shown once, neither as a replay. A test that
uses the stack runs only with `LEGION_E2E_ENVOY_STACK=1`.

`@legion/envoy-client/test/dispatch-stack` (Bun only, not built, in `test/`) is the same shape for
a real Dispatch server: it builds `./cmd/dispatch` with `go build` from `packages/envoy` into its
own scratch directory, makes a database of its own on the server `DISPATCH_TEST_DATABASE_URL`
names and drops it at the end, starts the server, which migrates that database as it boots, and
answers its URL and the agent bearer token it minted for the run. Among the tests that run on it
are `src/__tests__/design-gate-advice.live.test.ts`, which proves the design-gate clause end to
end, and `src/__tests__/moved-approval-advice.live.test.ts`, which proves the moved-approval line
of a document write's advice the same way; each is the real CLI entry `bin/dispatch.ts` writing to
that server, the server's record and the client's sentence together. They share
`@legion/envoy-client/test/dispatch-cli-live` (Bun only, not built, in `test/`), which holds the
real CLI's `docEdit`, the person's and the machine's writes, and the issue and document a row
writes about; each file keeps the constants and the sentences that are its own.
`src/__tests__/artifact-dry-run.live.test.ts` holds `dispatch artifact --dry-run` to the call on
the same stack: each of its rows runs `bin/dispatch.ts` as its own process, with `--dry-run` and
without, over a text of
`packages/envoy/internal/dispatch/api/testdata/text-refusals.json` in one combination of the way
the text is given, `--force` and a new or a held name. A test that uses this stack runs only with
`LEGION_E2E_DISPATCH_STACK=1`, and it needs `DISPATCH_TEST_DATABASE_URL` and `go` on `PATH`; with
the variable set it fails rather than skips. So, to run them here:

```sh
cd packages/envoy-client
LEGION_E2E_DISPATCH_STACK=1 DISPATCH_TEST_DATABASE_URL="$LEGION_TEST_PG_DSN" \
  bun test src/__tests__/design-gate-advice.live.test.ts \
  src/__tests__/moved-approval-advice.live.test.ts
LEGION_E2E_DISPATCH_STACK=1 DISPATCH_TEST_DATABASE_URL="$LEGION_TEST_PG_DSN" \
  bun run test src/__tests__/artifact-dry-run.live.test.ts
```

The dry-run file runs through the package's `test` script, whose `--timeout` its rows take. CI
runs each of them in `ci.yaml`'s `envoy-session-stall` job, which is the one lane of this package
with both a PostgreSQL and a Go; the dry-run file runs in a leg of that job of its own,
`artifact-dry-run` in `.github/workflows/ci-job-envoy-session-stall.yaml`, so the job's time stays
under the pull request's 10-minute line (`.github/ci-notes/ci.md`, "The session stall suites, the
session state rig and the live Dispatch stack").

## The listener's relay socket

A session whose settings say `ENVOY_INBOUND=listener` takes its inbound messages through the
listener's relay socket rather than NATS, so an agent on a machine outside the deployment's network
reaches Envoy over HTTPS alone. `connectListenerRelay(options, signal)`
(`@legion/envoy-client/listener-relay`) opens it and answers a `ListenerRelay` once the first socket
is open: a connection of the shape a session plugin holds over NATS (`InboundConnection`:
`subscribe`, `publish`, `flush`, `isClosed`, `drain`, `close`, `status` and `closed`), so the
plugin's consumer delivery runs on either. Each open is `GET <listener>/v1/sessions/<session>/relay`,
a WebSocket of subprotocol `envoy-relay.v1`, with a fresh proof of the session's broker token
(`Authorization: DPoP`, `DPoP`, `htm` GET, `htu` the socket's https URL). With the setting, a NATS
server named or not, a listener at a plain http address of another machine turns Envoy off as a
whole, its tools included, since the token would cross the network unencrypted (a conflict with a
NATS server is named first); this machine's own
listener may be plain http, and a session not set to `listener` (a pod inside its cluster, say)
keeps a plain http listener. The frames are JSON: the
session sends `sub` of its own relay subject, `unsub`, and `pub` of a relay receipt on a frame's
reply, which is all the socket takes; the listener sends `msg`, whose `headers` map each name, case
kept, to the list of its values in order (`RelaySocketHeaders.get` and `values`), and `err`. A
frame past 2 MiB, larger than any the listener sends, is dropped unread. A
`sub` the listener refuses (`subject_not_granted`, or `subscription_limit` past the one
subscription a socket holds) ends that subscription with an error naming its subject and code, as
a nats.js subscription a server refused does, and is logged once; a frame it cannot
read it answers `bad_frame` and closes the socket 1008, which the client logs naming the ops of the
frames it sent since the listener last answered a ping, since the answer names none.
The listener reads a socket's frames one at a time and answers no `sub` it grants, so `flush` sends
a WebSocket ping and resolves at its pong, after every `sub` before it is held; a flush whose
socket closes or is replaced first rejects. The socket carries
neither the connection probe nor the agent stream, so a host opens no live view over it, and the
start notice says so. The listener closes an open socket by why: 4401 (the token lapsed) and 1012
(it is stopping) open again at once however often they come, the first with a fresh trade; 4403
(the token was revoked or no longer names what the socket was opened with) after 1 s doubling to a
minute, with a fresh trade; 1013 (the broker or NATS cannot be asked) after 5 s doubling to a
minute; 1001 and 1006 at once, then after 1 s doubling while they repeat; any other close, 1008
included, after 1 s doubling (`relayReopenDelay`). An upgrade the listener refuses names its status
in Bun's `error` event: 401 and 403 wait as 4403 does and trade a fresh token, 503 after a 1013
goes on with the 1013's waits, and any other status, an upgrade not answered within 20 s, a
listener that cannot be reached and a failed trade are tried again after 1 s doubling to a minute.
Each cause doubles its own waits, and a socket open a minute starts them all afresh. Every other
open presents the token in hand, with a fresh proof. A trade that failed is told to the session
once per outage (`inboundUnavailable`). The socket never ends an outage: the report is the
host's, shared with its registrations, so only a registration the listener accepted ends it, and
one follows every open. A repeated
failure is logged once until its words change. Each reopen takes every live subscription again and
reports `reconnect`, so the host registers a new epoch and the listener redelivers what it never
acknowledged. `follow()` opens the socket again for the session the name reads now, which a host
runs when it moves to another session.

## HTTP transport

`createEnvoyClient` sends and receives the listener's `/v1` JSON contracts. Send and publish
accept optional `inReplyTo`, `supersedes`, `urgency`, `expectsReply`, and `expiresAt` metadata.
Successful direct sends return both the envelope and listener-selected recipient, and
`duplicate` when the listener already held a send under that target and idempotency key.
`send` and `publish` return a `KeyTakenResult` instead, `{ keyTaken: { eventId, topic, issuedAt },
delivered: false }`, when another sender's message already holds the idempotency key: Envoy stored
nothing for this caller, and the listener names only that message's `event_id`, topic and time,
with none of its text. A delivered result never carries `keyTaken`, so a caller tells the two
apart by it; `keyTakenText` and `keyTakenDetails` (`tool-contract`) give the line and the fields a
host shows for it. The listener gives that answer only to a request that names it, in the header
`Envoy-Reads: key-taken`, which the client sends on a send or a publish that names its source
session; a call that names none gets the envelope the listener built for it, with `duplicate`,
since the listener cannot tell that caller's own earlier attempt from another sessionless
sender's. A publish returns the envelope and an optional role holder. A role publish has three
answers, told apart by status. 200 is a live holder that took the message, named by `holder`.
202 is a message the listener kept for one hour because no live session holds the role, with `kept`,
`keptReason` (`unclaimed`, or `holder_lapsed` with the `holder`, its `holderState` and its
`lastSeen` when the claim's holder is not live) and `keptUntil`, the end of that hour in epoch
milliseconds. The next session to hold the role live before `keptUntil` is handed the message. If
none does, the listener drops it and sends the publishing session a dead letter, when the publish
named that session as its source and the listener did not drop it more than an hour late. A
listener of an earlier release sends no `keptUntil`. 404 is nothing kept, thrown as an
`EnvoyApiError`, and the sender's own fallback must run. A sender that
keeps a queue of its own passes `keep: false` to get the 404. Network failures, timeouts (5 s an
attempt, the answer's body included) and 5xx responses retry once after 250 ms, under the same
idempotency key; a 4xx whose body does not arrive is still a refusal. A send any attempt of which
got no answer, an unreadable 2xx or a 5xx throws `EnvoySendUnconfirmedError`, even when a later
attempt was refused: the message may or may not have arrived. The error names its
`idempotencyKey` and says in `resendSafe` whether the listener's dedupe key for it
(`agent.<target>.<key>`) is one a repeat is recognised by (`MINTED_DEDUPE_KEY_PATTERN`); when it
is, a resend under that key inside the 72-hour duplicate window is dropped as a repeat if the
first copy arrived (`DELIVERY_DUPLICATE_WINDOW_MS` names the rare cases a recipient that restarted
still sees it twice). One 5xx is not such an attempt: the listener's `503` whose `error` opens
with `MESSAGE_NOT_SENT_PREFIX` (`@legion/contracts`), which says the message bus refused the
message before anything went out, because the deployed stream does not capture its topic for
now, as during a hub move. It is sent again like any 5xx, under the same key, and a send or a
publish that gets it on every attempt throws that `EnvoyApiError`, whose `notSent` is true and
whose message is the listener's: nothing was stored, so the message may be sent again later.
After an earlier attempt that got no answer or another 5xx, a send is unconfirmed as above, and a
publish's error says that attempt may have stored the message, with `notSent` false. A send
refused with a 4xx on every attempt stays an `EnvoyApiError`. An `EnvoyApiError` keeps the
listener's `error` as its message, with ` (reason: <reason>)` after it where the body names one,
and keeps the body's `code`, `reason` and `expected`, and the `retry_after_seconds` of a refusal
for the sender's rate as `retryAfterSeconds`. A 429 is not sent again: the client throws it at
once, and the caller decides whether to wait `retryAfterSeconds` and send the message again. The
listener's bearer is the `token` the client is given (`envoySettings`); without one it sends no
`Authorization` header.

Every call whose body names the session it acts for — `subscribe`, `unsubscribe`,
`unregisterSession`, `setRole`, and a `send` or `publish` with a `sourceSessionID` —
presents that session's own credential, since the listener holds such a write to the session its
broker token names and answers 403 to any other. That is what lets a process that has switched
sessions hand back a role the session it left still holds: the old session's credential is a
client of its own and answers out of the token it already has. A read of another session's
record (`getInterest`, `getRole`, `listSessions`) names no session and is answered the
credential the client was built with. A bearer is the same header whichever session a call acts
for.

`subscribe` sends the host's `plugin` (`{name, version}`, required) as `plugin` and
`plugin_version` on every registration, and exposes listener warnings, including a topic that has
no event in the configured stream. It sends the client instance driving the session
(`clientInstanceID`: one Oh My Pi Envoy extension instance, one Claude Code channel-server
process, minted once when that instance starts) as `client_instance_id`, omitted when the host
has none so an older listener sees an unchanged body; the listener stores it on the session's
registry entry and renews with it every role claim this session holds that names that same
process, so each registration is the holder's answer for the roles it holds. It renews every such
claim the listener has not marked handed back, whatever topics the registration carries: what
says a role was given up is the mark a hand-back leaves on a claim it could not free, and a
marked claim is renewed by nobody. A registration that
states no instance renews the claims of its own session that name no process, and adopts none, so
a claim standing at a deploy is kept by a client that cannot say which process it is.
`getRole` returns a role holder with its last-seen timestamp and, where the
claim names a process, the `client_instance_id` of that process; a claim that names none leaves
the field out, on the 200 as on a lapsed holder's 404. It also returns `handed_back` where the
claim carries the listener's hand-back mark, on both answers and left out where it carries none.
`listSessions` accepts optional directory
and title filters; its rows carry `plugin` and `plugin_version` when the session registered them.

`roleHeldBy(info, sessionID, clientInstanceID)` and `mayActOnRoleRecord(writer, clientInstanceID,
holdsRole)` are exported beside `RoleInfo` as the two rules every client keeps over those ids:
whether a role lookup's answer is this caller's own hold of the role — this session *and* this
process, a claim naming no process read as this one's, and a claim the listener marked handed
back no hold of anyone's — and whether a persisted role record, a
transcript's claim entry or a channel server's role file, is this process's to act on, which a
record another process wrote is only while this one holds no role. Both read an empty id as no
process named, exactly as an absent one. The mark is why a holder an older client's hand-back
reached claims its role again at its next heartbeat and has it back, the grant writing a fresh
record that carries no mark, rather than reading the lookup as confirmation until the role claim
reaper frees the role a live window and up to one reap interval later. That holds for a claim
that names the holder's process. A soft claim over a marked claim that names no process is
weighed by that claim's renewal and refused until the renewal is older than the live window or
the reaper frees the claim; the way back is an outright claim (`setRole` without `soft`), which
lands at once and writes a fresh record with no mark.

`setRole` and `unsubscribe` take a `clientInstanceID` of their own, sent as `client_instance_id`
and omitted from the wire when the caller states none: the listener records the claim's as the
process holding the role, and frees on a release only the role claims naming the release's own,
so a second process that resumed this session id can neither take the role nor hand it back, and
a release naming no process frees no claim at all, and no release but the administrator's force
frees a claim that names no process. `unsubscribe` answers in `removed` the topics that left the
record rather than the ones it named: a role lane topic whose claim the release did not free
stays in the record, because one interest record serves every process running under one session
id and that topic is the interest half of a claim that still stands. Such a release, where it
names that role's own lane topic, is a hand-back, and the listener marks the claim it could not
free, whether that claim names a process or none. A release the holder's own renewal beat under
each of its three conditional deletes fails rather than answering success, so `unsubscribe`
rejects and the caller sends the hand-back again rather than treating the role as given up. A
`setRole` soft claim (`soft`) naming a predecessor (`previousSessionID`) takes the role from it
only when that predecessor has no live registry entry, when its live entry carries this client
instance's own instance id, or when either side carries no instance id. A soft claim of a role
already held under the claimant's own session id is decided on the standing claim rather than on
the session record: granted when that claim names this very process, granted when nobody renewed
it inside the listener's live window, and otherwise `{claimed: false, holder}` naming that same
session — which `getRole`'s `client_instance_id` tells apart from a different session holding
the role. Any other live holder keeps the role and is answered as `{claimed: false, holder}`.
`unregisterSession({sessionID, clientInstanceID?})` deletes the session's registry entry: with an
instance id the listener deletes it only when the entry carries the same id or none, so a
departing client instance never deletes the registration a successor instance made under the
same session id, and without one it deletes the entry as before. The answer says whether the
entry stayed, and the call resolves `void` because no caller acts on it: a deregistering client
has nothing to retry either way.

`subscribe`'s answer carries the registration's `delivery` field; a registration the listener can
give no consumer is refused by name, and its reason is on `EnvoyApiError`, under the one canonical
list of them (`ErrorWireSchema.reason` in `src/transport.ts`). The same list holds
`sender_rate_limited`, the reason of the 429 a send or a publish gets when its agent session has
sent faster than the listener lets one session send: the message was not stored. A listener in
edge mode refuses a whole registration that names a topic of no project of the session's token,
403 `topic_not_in_project` or `repository_not_in_project`, naming each refused topic as the
registration carried it (`EnvoyApiError.refused`): `registerWithRelay`
(`@legion/envoy-client/relay-registration`) has the host drop exactly those and tell the session
which, once, with the reason (`drop`, worded by `refusedTopicsNoticeText`), logs one line naming
them, and registers the rest at once, whichever path registered. The resend names only the topics
the session still follows (`followed` is read again before it), so one unsubscribed while the
registration waited is not put back, and the relay forgets only the refused topics
(`ConsumerDelivery.unfollow`), so one another registration added meanwhile stays followed. Its 503
`repositories_unknown` and 403 `token_not_admitted` drop nothing.

## envoy.json

Every client and command reads one envoy.json holding a `dispatch` section and an `envoy` section:

```json
{
  "dispatch": {
    "enabled": true,
    "serverUrl": "https://dispatch.example",
    "token": "!secrets get DISPATCH_TOKEN"
  },
  "envoy": {
    "enabled": true,
    "url": "http://127.0.0.1:9020",
    "natsUrls": ["nats://nats.internal.example:4222"],
    "token": "listener-token",
    "natsSeed": "!secrets get NATS_NKEY_SEED"
  }
}
```

`envoyConfigPath(harness, env)` (`envoy-config.ts`) picks the file. A plugin reads its own
harness's directory and no other harness's: Oh My Pi's `$PI_CODING_AGENT_DIR/envoy.json` when
`OMP_PROFILE` names a profile other than `default`, Claude Code's `$CLAUDE_CONFIG_DIR/envoy.json`.
Without that directory it reads the file `ENVOY_CONFIG_FILE` names, else
`${XDG_CONFIG_HOME:-$HOME/.config}/legion/envoy.json`. A script outside any plugin (harness
`command`: `scripts/dispatch/ask-census.ts`, the e2e operator scripts, `claude-envoy`'s
`envoy-send`) reads `ENVOY_CONFIG_FILE` first, then the Oh My Pi and Claude Code directories in
that order (Claude Code's only under `CLAUDE_CODE_CHILD_SESSION=1`, which Claude Code alone sets,
in every Bash it runs), then the same neutral file. A relative `ENVOY_CONFIG_FILE`, harness
directory or `XDG_CONFIG_HOME` is refused, naming it and its value, when the rule would use it.
Each plugin hands the path it chose to the commands its session runs in `ENVOY_CONFIG_FILE` (Oh My
Pi sets it in `process.env` at load, Claude Code's `SessionStart` hook appends it to
`$CLAUDE_ENV_FILE`), beside `DISPATCH_HOST`; the innermost session's value wins.

The `dispatch` command reads the file its session's plugin chose: with `DISPATCH_HOST` set, the
file `ENVOY_CONFIG_FILE` names, else its host's own rule (`envoyConfigPath(host)`). A Bash that
lacks what the hook wrote, as a Claude Code subagent's can (Claude Code issue 46696), has
`DISPATCH_HOST` unset and `CLAUDE_CODE_CHILD_SESSION=1`, which Claude Code sets in every Bash it
runs; `dispatch` then takes host `claude` and Claude Code's rule, so `CLAUDE_CONFIG_DIR`, the
session's own directory, comes ahead of any `ENVOY_CONFIG_FILE` an outer session exported.

`readEnvoyConfig(path, env, { cwd })` reads that file and a checkout's `<cwd>/.envoy/envoy.json`
as written, running no command, and resolves to the result: every file it reads or looks for is
read through `fs/promises`, so a read waiting on a loaded disk holds the read alone and never the
process's event loop, as do `askgateSetting` and the files the `*_FILE` variables name
(`readSecretFile`). Each section is strict: an unknown key is refused naming it, and
so is a top-level `natsUrls`, which belongs in `envoy.natsUrls`. JSON that does not parse is
refused at its line and column, never with the parser's message, which quotes the text it stopped
at. A missing chosen file contributes nothing and sets `missing`: each section is then on only when
its variables name it, and a section still off says `no envoy.json at <path>: Dispatch is off`
(`Envoy is off`, or `Dispatch and Envoy are off` when neither is on; Dispatch with
`DISPATCH_TOKEN_FILE` set says the lost `DISPATCH_URL` instead, below), with
`(a link to <target>, which does not exist)` after the path when it is a dangling link. A chosen
file that cannot be read or is not valid is `failed`, naming the file and the key, and both
sections are off with that error.

Nothing reads the OpenCode-era `$HOME/.config/opencode/envoy.json`. While it is there and the
chosen file is missing, the notice ends with what to do about it, decided by the chosen file's
path: for `$HOME/.config/legion/envoy.json`, whichever rule chose it,
`; <old file> is not read: mkdir -p ~/.config/legion && mv ~/.config/opencode/envoy.json ~/.config/legion/`;
for a profile's own file, `; <old file> is not read for this profile; copy or link it to <path>, or
write that file`; for the neutral file under a relocated `XDG_CONFIG_HOME`, as a Legion pane's is,
`; <old file> is not read under XDG_CONFIG_HOME=<dir>`, without asking anyone to move it.

No notice tells anyone to `mv` across a mount. When the old file's directory and the nearest
existing ancestor of the new file's directory are on different filesystems (`stat`'s device
differs, as in an agent box whose `~/.config/opencode` is a bind mount of its host's), the move
reads `<old file> is not read, and is on another filesystem than <new file> (a mount): create <new
file>, and move <old file> on the machine that owns it`, with no command, for the user file and the
checkout's `.opencode/envoy.json` alike; `readEnvoyConfig`'s `deviceOf` option answers a
directory's device, `stat`'s by default. The Go Dispatch server's refusal and warning follow the
same rule.

A checkout's file is the repository's, and its failure is its own: one that cannot be read or is
not valid refuses Dispatch with its error only while `DISPATCH_URL` is unset (a session's start
notice then says `<error>; not read while DISPATCH_URL is set`), Envoy never reads it, and a `.envoy`
that is a plain file is no checkout file. It may configure Dispatch alone: a top-level `natsUrls`
there is refused saying a checkout's envoy.json holds only a `dispatch` section, and an `envoy`
section, whatever it holds, is not read, which the start notice says
(`<checkout>: envoy is not read from a checkout's envoy.json; put it in <chosen file>`). So a cloned
repository neither chooses where a session's deliveries come from or which servers the user's NATS
seed signs in to, nor turns off the Envoy the session's environment configured. The Go Dispatch
server, which reads the same files, refuses to start on a checkout's `envoy` section. A checkout's
OpenCode-era `.opencode/envoy.json` is never read: while the checkout has no `.envoy/envoy.json`,
`warnings` holds `<old file> is not read: mkdir -p <cwd>/.envoy && mv <old file> <cwd>/.envoy/envoy.json`,
which the start notice and the `dispatch` command say.

Each section is resolved on its own: `dispatchSettings(env, config)` reads the chosen file's
`dispatch` section and, while `DISPATCH_URL` is unset, the checkout's, and `envoySettings(env,
config)` reads the chosen file's `envoy` section alone. Both follow one rule for `enabled`:
`enabled: false` turns the section off whatever else is set; otherwise the section's fields apply,
whether or not it sets `enabled`, and it is on when its address resolves, from the file or a
variable:

|                                        | Dispatch                                    | Envoy                                               |
| -------------------------------------- | ------------------------------------------- | --------------------------------------------------- |
| off whatever else is set               | `dispatch.enabled: false`                   | `envoy.enabled: false`                              |
| its address, from the file             | `dispatch.serverUrl`                        | `envoy.url`, `envoy.natsUrls`                       |
| its address, from a variable           | `DISPATCH_URL`                              | `ENVOY_URL`, `ENVOY_NATS_URL`                       |
| `enabled: true` with no address names  | `http://localhost:8766`                     | the listener `http://127.0.0.1:9020`, no NATS server |
| off, saying so, when nothing names one | `<file> names no Dispatch server, and DISPATCH_URL is not set: Dispatch is off` | `<file> names no Envoy listener or NATS server, and neither ENVOY_URL nor ENVOY_NATS_URL is set: Envoy is off` |

So a file `{"envoy": {"url": …, "token": …}}` with no `enabled` is on with its bearer, and so is
`{"dispatch": {"serverUrl": …, "token": …}}`; a section holding only a token is off until a variable
names its server. `DISPATCH_URL` replaces a checkout's file whole: while it is set, the checkout's
server, token and `dispatch.enabled: false` are neither used nor refused. Otherwise a checkout
contributes only its `dispatch.enabled: false`, or a `dispatch.serverUrl` with the `dispatch.token`
written beside it, which is an address: its `dispatch.enabled: true` alone turns nothing on, since it
would name the default server and draw a variable's token to it. A checkout's `dispatch.serverUrl`
without its own token is refused, and so is its `dispatch.token` without its own
`dispatch.serverUrl`, unless `DISPATCH_TOKEN` or `DISPATCH_TOKEN_FILE` replaces it. A checkout's
server with its own token points the session at that server as that token's holder, whichever
server it names, the user's own included; what a checkout cannot do is take the user's token, or a
variable's, anywhere.

A `token` or `natsSeed` value starting with `!` is a command (`fileValue`): it runs under
`/bin/sh -c` in the directory of the envoy.json that names it, never the session's working
directory, so `./bin/token` or a directory-configured shim (mise, direnv) resolves against that
file and not whatever repository the session started in. It runs in its own process group for at
most 10 s, and its trimmed stdout is the value. The read waits for it without holding the process:
every resolver that can run one (`fileValue`, `fileBearer`, `dispatchSettings`, `envoySettings`,
`sessionSettings`, `sessionDispatchSettings`, `sessionEnvoySettings`) returns a Promise, so the
session's timers, deliveries and NATS pings are served while it runs, and `sessionSettings` runs
the two sections' commands at once. The command's exit status decides: a command that exited 0
has printed its value even when a process it left running holds stdout open; a timeout kills the
whole group. The group also ends with the process however the process ends: its own exit, a
signal it handles through `process.reallyExit` (as Oh My Pi's SIGINT, SIGTERM, SIGHUP and
fatal-error handlers do), or a SIGKILL. A watch in the group (`spawnWatched` in
`src/process-group.ts`) reads a pipe the process holds and kills the group once the kernel has
closed it. The command's stdout goes to a file whose name is removed before the command starts,
so no exit leaves the value on disk under a name. It runs only when its section's resolver is
about to use the value (the section on, no variable overriding the field). Its value
is kept for the process, shared across module instances (one map on `globalThis` under
`Symbol.for("legion.envoy-config.commands.v1")`, since Oh My Pi loads each extension instance, a
`task` subagent's included, as a fresh module, and pi-envoy and pi-legion, released separately,
each bundle this module; a change to an entry's shape takes a new key), and keyed by the command,
its file and its working directory. A read that starts while the same command runs, in any of
those instances at or after this change, waits for that run rather than starting another (a second
map, of the runs under way, under `Symbol.for("legion.envoy-config.command-runs.v1")`); a bundle
from before it keeps no such map, so a read in it starts a run of its own. A command that ran out
of time is kept as failed for the life of the process: a hanging command never runs again in that
process, since each run would hold every read waiting on it for the whole 10 s. A failure that
ended on its own (a non-zero exit, empty output, more than one line) is kept for 60 s
and then the command runs again, so a cause that clears (a keyring unlocked after the session
started) needs no restart. The `dispatch` command is one process per call, and each Claude Code
hook one per event, so a `!` token runs once for each. A non-zero
exit, a timeout or empty output is an error naming the field and the file, never the output. A
value with whitespace or a control character inside it, written, printed by a command, or given in
`DISPATCH_TOKEN`, `ENVOY_TOKEN` or a `_FILE` variable's file, is refused without being echoed, since
it cannot go in a header as one bearer. A checkout's file is the repository's, so a command in it
is refused.

`envoySettings` is the Envoy a session uses: `{ enabled: true, url, token, inbound, heartbeatMs }`,
or a `SectionOff`, `{ enabled: false, notice, error }` with exactly
one of the two set. A `notice` says why nothing is wrong: `<file> sets envoy.enabled to false:
Envoy is off`, `<file> names no Envoy listener or NATS server, and neither ENVOY_URL nor
ENVOY_NATS_URL is set: Envoy is off`, or the missing-file text. An `error` names what could not be
read or used. `ENVOY_URL`, `ENVOY_NATS_URL` (comma-separated), `ENVOY_TOKEN_FILE` then
`ENVOY_TOKEN`, `NATS_NKEY_SEED_FILE` then `NATS_NKEY_SEED`, `NATS_AUTH` and `ENVOY_INBOUND` each
override their own field of the section; with no `envoy` section, `ENVOY_URL` or `ENVOY_NATS_URL`
alone names an Envoy, which is how a Legion pane gets the one its daemon names. An empty `ENVOY_*`
or `NATS_AUTH` variable counts as unset; an empty `NATS_NKEY_SEED_FILE` or `NATS_NKEY_SEED` is
refused, naming it. `inbound` is how the session takes inbound messages: `{ natsUrls,
natsCredential }`, never with an empty `natsUrls`; `{ relaySetting, relayCredential }`, the
listener's relay socket (below); or `{ off }`, `off` saying why: `Envoy has no NATS server
(envoy.natsUrls, ENVOY_NATS_URL)` when nothing names one, a credential's refusal (below), or
`this session opens no NATS connection` for a caller that opens none (`{ nats: false }`: the
`dispatch` command, `claude-envoy`'s `envoy-send`, pi-legion's extension). `ENVOY_INBOUND`, else
`envoy.inbound`, chooses between the first two: `nats`, or nothing set, is NATS, resolved as
before; `listener` is the relay socket at `<ENVOY_URL>/v1/sessions/<session>/relay`, presenting the
session's broker credential (`relayCredential`, `brokerCredential`), which is the only credential
the socket admits. `listener` is refused, turning off inbound messages alone, beside a NATS server
(`ENVOY_INBOUND is listener, and ENVOY_NATS_URL names NATS servers: set one of the two`, or the
file's `envoy.natsUrls`), with a bearer for the listener, on a machine with no broker route, and for a
caller that names no session (`noSessionNamed`); a listener at a plain http address of another
machine turns Envoy off as a whole (`localhost`, 127.0.0.0/8 and `::1` may stay plain http); any
other value is refused as `ENVOY_INBOUND
is neither nats nor listener`, never echoing it. The NATS credential is resolved only when there
are NATS servers to use it on, so a caller that opens no NATS connection resolves none.
A seed is the credential wherever one is set. Otherwise
`NATS_AUTH`, else `envoy.natsAuth`, names it: `broker` is the session's broker token, and `none`, or
nothing set, is no credential, whatever broker route the machine has, since that route says nothing
about whether the hub runs an auth service. `broker` on a machine with no broker route
(`NATS_AUTH is broker, but <gap>`), `broker` beside a seed (`NATS_AUTH is broker, and
NATS_NKEY_SEED names an nkey seed: set one of the two`), the file's `broker` with an
`ENVOY_NATS_URL` naming other servers than the file's own (`ENVOY_NATS_URL names other servers
than <file>'s envoy.natsUrls; set NATS_AUTH=broker for them`, as for the file's seed) and any other
value (`NATS_AUTH is neither broker nor none`, never echoing the value, which may be a credential
pasted into the wrong place) turn off inbound messages alone. A set `_FILE` pointer that
cannot be read is refused, never a fallback. The file's `envoy.token` goes with `ENVOY_URL` only
when it names the origin of the listener written in the file (`http://127.0.0.1:9020` when it names
none), and is otherwise refused with `ENVOY_URL names <origin>, but <file>'s envoy.token is for
<origin>; set ENVOY_TOKEN or ENVOY_TOKEN_FILE for it`; an empty `envoy.token` is refused, as an
empty `dispatch.token` is. A refusal of the listener's bearer turns Envoy off with that `error`.
The file's `envoy.natsSeed` goes only with the servers written in the file, since a server that is
not the real one can relay the real server's nonce and replay the signature: `ENVOY_NATS_URL` gets
it only when it names those servers, compared as a set of normalised URLs (scheme and host
lower-cased, a trailing `/` dropped, the default port 4222 written when none is, so
`nats://a:4222` and `nats://a:4222/` pair), and is otherwise refused with `ENVOY_NATS_URL names
other servers than <file>'s envoy.natsUrls; set NATS_NKEY_SEED_FILE for them`. The seed is for NATS
alone, so its refusal (that pairing, a `natsSeed` command that fails, or a seed variable that cannot
be read) leaves Envoy on with its listener and bearer, and turns off inbound messages: `inbound.off`
names the refusal. A credential from the variables
goes with whatever address resolves. `natsAuthOptions` takes the resolved credential and names a
seed's source when it is not a user seed; `natsSeedFromEnvironment` resolves one from the two
variables alone.

Every caller resolves through `session-settings.ts`. `sessionConfigFile(harness, env)` is the file
`envoyConfigPath` chooses, with the rule's refusal of a relative path as `error` instead of a
throw, so a plugin that cannot work out its file still loads, with both sections off;
`handedConfigFile(env)` is the file `ENVOY_CONFIG_FILE` names, the one a plugin hands its
session's commands, or null when that is unset or empty. `sessionSettings(file, env, { cwd, nats
})` reads it once and returns both sections (`dispatchSettings`, `envoySettings`) and the one
notice the session shows at its start: the file's error (`<error>: Dispatch and Envoy are off`),
the missing file's notice, the missing-file text for a section still off when a variable turned
the other on, the lost-`DISPATCH_URL` notice for Dispatch when `DISPATCH_TOKEN_FILE` is set and no
server is named (below), or a section's own error (`Dispatch is off: <error>`,
`Envoy is off: <error>`), then what a checkout's files hold that the session does not read (its
`envoy` section, its `askgate` section as `askgate in <checkout file> is not read; set it in
<session file>`, the file's own error while `DISPATCH_URL` replaces it, or its
`.opencode/envoy.json`), then, for a session that opens a NATS connection (`nats` not `false`),
that an Envoy that is on takes no inbound messages: `Envoy's inbound messages are off:
<inbound.off>; the Envoy tools still work`. Otherwise a file that turns a section off, or holds no
section for it, says nothing.
`askgateSetting(file)` resolves to the session file's top-level `askgate` section, read now, on
its own, so a section that does not parse never fails the Dispatch or Envoy read:
`{"askgate":{"enabled":false}}` turns AskGate off (`{ enabled: false, error: null }`); no file, no
section or `enabled: true` leaves it on. A section that does not parse, or a file that cannot be
read or is not JSON, leaves it on with an `error` naming the file and `askgate.enabled` and ending
`AskGate stays on`. A checkout's `.envoy/envoy.json` cannot set it.
`sessionDispatchSettings(file, env, { cwd })` reads
Dispatch alone, now: `{ dispatch, warnings, missing }`, Dispatch's resolution beside the files the
read found and did not read and the missing-file record, for a command, which says the `warnings`
itself, and for a plugin whose Dispatch calls follow a file that changes. `activeDispatchSettings`
turns its `dispatch` into the configuration a caller uses, null when nothing configures Dispatch,
or a `dispatch config: <reason>` throw. `sessionDispatchUrl(file, env, { cwd })` resolves to the
URL alone (`dispatchUrl`), without the token's value, so no `!` command runs, or undefined when
Dispatch is off before its token is read: what a caller on a deadline names to
`readSessionAnswers` as `dispatchUrl` while a kept resolution is off with an error, so only the list
read from that URL stands in. `sessionEnvoySettings(file, env, { cwd, nats })` reads
Envoy alone, for a command such as `envoy-send`.

## Dispatch

`dispatch-http.ts` sends the native Dispatch JSON API with the configured bearer
token. `dispatch-execute.ts` validates a call against its shared spec, derives
the caller's session origin and Legion issue reference, executes the operation,
and returns its text and typed details. No result carries a subscription topic:
`dispatch ask` and an ask reply through `dispatch comment` return
`details.follows = { ask }` (the session follows that ask server-side). A write's text names the
`envoy_subscribe notifications.dispatch.issue.<KEY>.>` line an agent passes to subscribe to the
whole issue itself only when none of the session's subscriptions, as the Envoy listener records
them, covers the issue; with Envoy off or no answer from the listener it names none.
`dispatch-subscribe.ts` turns `details.follows` into the one-time host notice.
`dispatch-first.ts` reads the `dispatch-first` skill from a plugin's staged `skills/` and wraps it in
`DISPATCH_FIRST_MARKER`, the text pi-envoy and claude-envoy inject into a session with Dispatch.

Every `POST` and `PATCH` of `DispatchClient` carries one key. Each JSON and multipart call mints a
`crypto.randomUUID()` `Idempotency-Key` per `POST` and `PATCH` call, once (`#writeCall`), and sends
it on every attempt: the write's own attempts, the re-send after `401 BROKER_TOKEN_NOT_ACTIVE`,
and each retry after an old build's answer of no route. So Dispatch runs the write once however
many attempts reach it and answers a repeat from that run without running it again
(`packages/envoy/cmd/dispatch/README.md`, "Keyed writes", says with what); a `GET`, `PUT` or
`DELETE` carries none and is sent once, the `DELETE` of `deleteComment` and `deleteMessage`
included. No Dispatch request follows a redirect (`redirect: "manual"`): Dispatch's API
answers none, and followed, a 301, 302 or 303 turns a write's `POST` into a `GET` whose 2xx would
read as the write's answer, while a 307 or 308 sends it again to another origin without its
credential. The body is encoded once
for the whole call, a multipart form under a boundary of its own, since a retry whose bytes differ
from the first attempt's is a different request to Dispatch and its key is refused. A transport
failure, any 5xx, a 408, a 429 and Dispatch's `409 KEY_IN_FLIGHT` (`retriesUnderKey`, the rule
the page shares from `@legion/contracts`) are asked again under that key, `DISPATCH_WRITE_RETRY`'s
pace: at most five attempts, 1, 2, 4 and 8 s apart, a longer `Retry-After` waited instead, and no
attempt started more than 30 s after the first. An attempt that goes 20 s without progress before
its answer begins (`silenceMs`) is cut off and asked again the same way, so a write Dispatch is
slow to finish is answered `409 KEY_IN_FLIGHT` and then replayed rather than waited on for the
whole deadline. Progress is a read of the body: a body of a megabyte or more goes out as a stream
read 64 KiB at a time, with its `Content-Length`, and each read restarts the bound; a smaller body
leaves in one write. The bound counts those reads, not the bytes that leave. Bun's fetch reads the
next 64 KiB only once the connection's buffers have room for it, and they hold megabytes, so on a
slow line the reads come seconds apart and the last read comes while megabytes are still to go.
An attempt is cut off when its next read takes longer than `silenceMs`, or when the rest of its
bytes and Dispatch's answer after its last read take longer together, and either can happen while
the upload is still going out. Measured in build pods
against a stand-in Dispatch that reads at a fixed rate, with the 20 s bound: at 128 KiB/s an
upload was cut off 20 s after its last read, 0.88 MiB of its 12 MiB short of the stand-in, and at
64 KiB/s 20 s after a read part way through, 9.88 MiB short. Once an attempt was cut off for
silence or answered `409 KEY_IN_FLIGHT`, Dispatch may still be running the write, and its keyed
layer finishes that run whatever the client does, so the write keeps asking every 3 s at most
(`inFlightPauseMs`; a longer `Retry-After` is still waited),
with no attempt started more than 55 s after the first (`inFlightWithinMs`), until the run's stored
answer comes back: a write that takes Dispatch 50 s, or two silent attempts in a row, still ends in
its answer. An upload (a body of a megabyte or more) keeps the five-attempt schedule, since each
repeat sends its whole body again. No attempt of any write starts more than 55 s after the client
was made, since its 60 s deadline counts from there. The numbers come from the 3,612 `dispatch`
write commands that succeeded between 2026-10-31 21:00Z and 2026-11-02 07:30Z, each timed whole
(p50 1.8 s, p95 19.6 s, p99 45.5 s). All of it sits inside the call's own 60 s deadline, so a
Dispatch that is down holds a `dispatch` command, and the session waiting on it, no longer than
that.
How a write ends is one transition table (`write-outcome.ts`, `writeStep`), and the write
loop (`#write`) decides nothing else. A write is in one of two states: nothing sent, or may have
landed. An attempt may have landed when it got a 5xx or `409 KEY_IN_FLIGHT`, was cut off for
silence (an upload's included), failed in transport after it went out (a reset after Dispatch
read it), its answer's body broke off or was ended by the deadline or the caller's stop, or it was
ended by either while it was in flight; a refused connection or a name that did not resolve
reached nothing. Every answer's body is read inside its attempt, so a 2xx settles only once its
body is read whole. From "may have landed", only a 2xx Dispatch wrote, read whole (not an empty,
untyped or non-JSON one; a bodiless 204 or 205 is one; not one without exactly this request's key
once an earlier answer of the call carried it), `409 KEY_COMMITTED` (a landing) or a final
refusal settles the write. A final refusal is one of these:

- an answer marked `Idempotency-Replayed: ?1`: Dispatch's stored answer, or a recovery replay;
- `422 KEY_REUSED` that echoes exactly this request's key. Every attempt of one call sends the
  call's one key and its bytes encoded once, and Dispatch's digest covers the method, the route
  pattern, its path values, the query, the content type less its boundary, and the body
  (`writeRequestFields`), so a key held under another digest was claimed by no attempt of this
  call. That needs the client never to send one key for two different writes: `#writeCall`
  (`src/dispatch-http.ts:2176`) mints one per call, `dispatch-keyed-writes.test.ts` ("send one
  Idempotency-Key per write call") shows five writes going under five keys, and
  `dispatch-write-outcomes.test.ts` fails any run whose attempts carried two keys. A caller that
  names its own key (`projectArtifact`, `docEdit`) names one intent with it: AskGate's decision
  record reads `KEY_REUSED` as another writer of that intent;
- a fresh answer that echoes exactly this request's key, has a 4xx status `storedStatus` keeps
  (not 401, 408 or 429), and has none of the keyed layer's own codes `KEY_IN_FLIGHT`,
  `KEY_COMMITTED` and `KEY_REUSED`: a handler answered under the key's claim, and Dispatch keeps
  that answer for the key.

A refusal made before the key claim echoes no key (a 401, a 403, a 413,
`400 REQUEST_BODY_UNREADABLE`), and neither does a build without the keyed layer, so each leaves
the write unsettled. The key decides only how a final answer reads; every retry goes regardless.
Each clause holds only once every replica runs `b1c42d1d7d` (below). Every resend starts within
55 s of the first attempt, far below the 24 h `IdempotencyKeyTTL`
(`packages/envoy/internal/contracts/generated.go`), after which Dispatch deletes a key and claims
a later request under it fresh. A 2xx that is no answer of Dispatch
settles nothing from either state: one whose body is empty or not typed JSON (a captive portal's
200 page; every Dispatch write answers JSON, or a bodiless 204 or 205), or one without exactly the
request's key once an earlier answer of the same call carried it back, since Dispatch's keyed
layer answers every keyed write with the key (a gateway's own JSON 201). Only the call's own
answers count; nothing is kept across calls. Every other end — a 3xx,
a page something in Dispatch's place wrote, a refusal Dispatch's auth made before its keyed layer
ran (a 401, `403 READ_ONLY_TOKEN`, a second `401 BROKER_TOKEN_NOT_ACTIVE`), a credential that
cannot present, no route to the end, the deadline, the caller's stop, or no attempt or time left,
a 5xx included — is `DispatchWriteUnsettledError`, with what ended it as its cause and the
write's key as its `key`.
Before anything may have landed, a write that ends is told that event's own error: the refusal (a
3xx included, which says nothing reached Dispatch), the transport's failure (a refused connection
on every attempt is told as Dispatch being unreachable), the credential's, the deadline's or the
stop's. A credential whose trade goes on (the broker credential's fail-fast `TradeRetryingError`)
is presented again on the write's pacing, and before anything may have landed a deadline or a stop
that ends the wait after it ends the write with that credential's error (after a landing, the write
is unsettled with the deadline's or the stop's own); a trade still unanswered, a pod's registration
still pending (each told only once the credential has waited out its share of the deadline) and a
credential refused for good end the write at once. A write presents as one identity. Dispatch keys
a write per login, the person or the service and never the session (`requestKeyLogin`), so a repeat
as another person or as the machine is a new key there and could run the write twice. The client's
pin is stricter than that key on purpose: an identity is the session and the broker variables the
client was made from as well as the sign-in it trades (`RequestCredential.identity`), since the
client cannot tell whether another session trades as the same person. So once an attempt may have
landed, a presentation as another identity (a sign-out, a sign-in, a session switch) is never sent,
and the write ends unsettled with its key; before that, a repeat reaches no handler and goes as the
new identity. One change no identity sees is the broker binding another person to the same sign-in
between two trades of one write; the broker does not do that today. When the write ends unsettled,
the error says Dispatch finishes an attempt it received, and to read the write back before sending
it again, since a new command sends it under a new key. A conversion that writes nothing (the
markdown round trip) never leaves "nothing sent", so it keeps its own answers.

Every Dispatch that carries `b1c42d1d7d` runs the keyed layer on every `POST` and `PATCH` route
(the deploy order below says when the client may rely on that). A retry
therefore always sends the same key and encoded body again, without probing `/healthz` or keeping
process or session memory of a server capability. A transport failure, a silence cut, a gateway or
Dispatch 5xx, a 408, a 429 and `409 KEY_IN_FLIGHT` each retry within the write's pacing and
deadline. `dispatch-keyed-resend.test.ts` uses a Bun stand-in that stores one comment for a retry
under its key while `/healthz` is unavailable. `dispatch-keyed-resend-live.test.ts` starts a real
Dispatch whose first caller lookup answers 503 before it stores anything, then proves the client's
same-key resend stores one comment. `dispatch-write-outcomes.test.ts` drives every (state, event)
pair and every sequence of events up to its limits.

Before sessions use this client, every replica that can serve a write route must run
`b1c42d1d7d` or later; only then does a final refusal prove that nothing earlier landed. An older
build runs the handler with no key when it cannot resolve the caller or the body is past its limit
(`keyedWrites`), so an earlier attempt can land unkeyed and lose its answer: this client's retry
under its key then runs the write again, and a fresh echoed refusal or a `KEY_REUSED` after it
would read as nothing written. A fleet commit that names `b1c42d1d7d` while a rollout is still
going does not prove that every replica runs it. Do not roll Dispatch back below that build while
sessions use this client; fix forward instead.

A non-2xx answer is thrown as `DispatchServiceError`. Dispatch's own refusal, JSON with a string
`error`, is the error's message under its `code` (`HTTP_<status>` when the server set none), with
`fromDispatch` true. Any other body — a gateway's HTML page, an empty body, JSON of another shape —
did not come from Dispatch, so it is a `DispatchGatewayError` (`fromDispatch` false) whose message
names the method, the request's URL as built where the configured URL is a plain http(s) URL and
that URL's fixed phrase otherwise (`requestForText`, `@legion/contracts/log-text`; never the
bearer, which is a header), the status and reason phrase, a one-line plain-text excerpt of the
body, and what asking again can do. The excerpt
drops scripts, styles and tags, reads at most the body's first 64 KiB in linear time, and, like the
reason phrase, has the value after `Authorization:`, `Bearer` or `DPoP` redacted, every value shaped
as a JWS (`eyJ…`, a proof or a JWT) wherever it stands, escaped or in a list, and every run of 8
or more of the characters of each token the client presented lately (its credential's now, and
those it sent before a renewal: `SentTokens`, `excerpt.ts`; a shorter token only whole), so a copy
cut short, split by markup or overlapping another leaves no such piece. A body none of whose read
part is text is named
by its size in bytes; only a body with nothing in it is "an empty body". A status that cannot clear
answers the same until the Dispatch URL, or whatever
answers in its place, is fixed. A 5xx, 408 or 429 may clear: a GET may succeed on a retry, and so
may a write answered 408 or 429, the gateway's own timeout or rate limit, which it sends before it
forwards anything (`mayHaveReachedDispatch` false); a write answered 5xx may or may not have reached
Dispatch, so the advice is to check whether it took effect before retrying. The error's `answer`,
`advice`, `transient` and `mayHaveReachedDispatch` let a caller that knows more about its own
request, such as `dispatch issue-update`'s close path, give its own advice instead. The executor
reads a status as Dispatch's meaning (a 404 as no linked issue, no such document, or a server
without the route) only when `fromDispatch` is true. A 2xx is no answer of Dispatch's either where
its body is empty, not typed JSON (`application/json` or a `+json` type, letter case aside, as
Dispatch's own rule reads it) or not JSON, but for a bodiless 204 or 205: every route this client
reads as JSON answers JSON (`WriteJSON`), so such a read is a `DispatchGatewayError` too, naming
the body the same way, and a retry may succeed, since a proxy or a captive portal in the way can
be got past. Before, its text was handed back as the route's answer, and `dispatch open-asks` read a
portal's page as no asks at all. Such a 2xx reaches that advice only for a read, a `PUT` or a
`DELETE`: a keyed write's 2xx, a `POST`'s or a `PATCH`'s, is read by its own table
(`write-outcome.ts`), which says the write may have landed.
`readRoute` and `sendRoute`, which send the route `dispatch api`'s caller wrote, give every 2xx
but Dispatch's JSON back as the bytes that came, with its Content-Type (`RouteAnswer`): a file
version's bytes and a document version's Markdown download are such answers, and an answer that
carries `Content-Disposition: attachment` is stored content whatever its type, JSON included, so it
is never parsed. A 2xx whose body holds a credential the request presented, a token or its DPoP
proof, is refused instead (`DispatchCredentialEchoError`, code `CREDENTIAL_ECHOED`), since no
route of Dispatch's answers the caller's own credential.

`DispatchWriteUnsettledError` is deliberately not a `DispatchServiceError`, since a caller reads
one as a refusal Dispatch decided: its message is the last answer or failure, the number of
attempts, whether an earlier attempt may have reached Dispatch or Dispatch was still running one,
and that the write may or may not have taken effect, so check before running it again - a new
command is a new key. `key` is the key every attempt carried and `last` what ended the write. A
write that ends with no answer at all names the request: its last attempt cut off for silence
("made no progress for 20 s, so it was cut off"), or the call's deadline ending it while an
attempt was out or after one that may have reached Dispatch ("had no definitive answer when its
deadline ended it"). A write Dispatch
made whose answer is gone is a `DispatchWriteLandedError` (`code`, an `IdempotencyLandedCode`, and
`status`): `409 KEY_COMMITTED`, a write that landed before its answer was lost, which Dispatch will
not run again, or a replayed 2xx (`Idempotency-Replayed: ?1`) reading `ANSWER_NOT_KEPT`, a first
answer Dispatch gives no copy of: one larger than the 1 MiB a key keeps, or a success that holds a
comment or a message that was deleted, or a document version name a delete replaced
(`answerNotKept`, shared with the page; `IDEMPOTENCY_ANSWER_NOT_KEPT` in `@legion/contracts`). It
is no refusal and no `DispatchServiceError`, and it is never asked again. Its message is the one
line the session reads: the method and URL were made, why the answer is gone, and to read what it
made back with `dispatch read` or the matching list. The `dispatch` command exits 0 on one and
records `landed_without_answer: <code>` in its ledger entry. A step before the
command's own write that lands this way fails the command (exit 1) with that line and what it
means: a picture upload (nothing else was written: run the call again, which uploads it as its
next version), or the closing reason of `issue-update --status done` (the close was not sent: read
the issue's messages for the reason, then close with one that points at it). The close's own
`PATCH` landing this way is the command's write, and exits 0. A `POST` that writes nothing
(`markdownRoundTrip`, `fileRoundTrip`, `related`, `dryRunResult`) made nothing either, so that
replayed 2xx is no landing: the client asks the call once more under a new key, which runs it
afresh, and a second such answer is a `DispatchAnswerNotKeptError` (`code` `ANSWER_NOT_KEPT`,
`status`), no `DispatchServiceError`. Its message says the call wrote nothing, that Dispatch kept
no copy of its answer, and to run the command again, which asks under a new key: `dispatch related`
and a `dispatch result --dry-run` exit 1 on one. `dispatch artifact` refuses an upload of text
that is not blank, given inline, read from `--content-file` or from stdin, when the check of that
text got such an answer, saying to send it again; blank text given any of those ways and a
Markdown `--path` file go up unchecked when their check gets no answer, as
the account of the dry run further down says. A replayed refusal is the refusal Dispatch first
gave, thrown as any other.

Successful write responses may include `advice`. The executor preserves that object as
`details.advice` and appends short pointers after the subscription/follow suffix: a primary spec
with no decision blocks, three or more session writes without a human response (with stronger
wording from six), a write while the issue is still in triage, and up to two still-open asks owned
by the session. Ask replies do not trigger the write-cadence pointer and suppress an open-ask
pointer when replying to that ask; a status-setting issue update does not trigger the triage
pointer. Triage advice is shown once per issue for the lifetime of the executor process; absent
`advice` leaves the prior result text and details unchanged.

`dispatchSettings(env, config)` (`dispatch-config.ts`), which `sessionDispatchSettings` runs over
the envoy.json a caller reads, enables Dispatch only when both its URL and bearer token resolve,
under the `enabled` rule above. `DISPATCH_URL` overrides `dispatch.serverUrl`, and
`dispatch.enabled: true` with neither names `http://localhost:8766`, the Go server's listen
address; an empty `DISPATCH_*` variable counts as unset. The token resolves from
`DISPATCH_TOKEN_FILE` (a path; the trimmed file contents are the token — how the Legion daemon hands
the bearer to a pane without putting it on argv), then `DISPATCH_TOKEN`, then the chosen file's
`dispatch.token`; while `DISPATCH_URL` is unset, a checkout that names its own `dispatch.serverUrl`
gives that server and its own token instead. A set `DISPATCH_TOKEN_FILE` that is unreadable or blank
disables Dispatch and names the variable and the read error's code in `error`, never the path; it
never falls back. The chosen file's token goes
with `DISPATCH_URL` only when it names the origin of the server written beside it: any other server
needs `DISPATCH_TOKEN` or `DISPATCH_TOKEN_FILE`, and is refused with
`DISPATCH_URL names <origin>, but <file>'s dispatch.token is for <origin>; set DISPATCH_TOKEN or
DISPATCH_TOKEN_FILE for it`. Invalid configuration, malformed URLs, and empty tokens leave Dispatch
disabled and name the failing source in `error`; nothing configuring it, or a file turning it off,
leaves it disabled with a `notice` (`<file> sets dispatch.enabled to false: Dispatch is off`,
`<file> names no Dispatch server, and DISPATCH_URL is not set: Dispatch is off`, or the
missing-file text). One case is told apart: `DISPATCH_TOKEN_FILE` set while nothing names the
server. Legion sets that variable only beside `DISPATCH_URL`, a Legion pod has no envoy.json, and
an Oh My Pi session keeps one shell, where an `unset DISPATCH_URL` lasts for every later command.
So the notice names the lost variable rather than the file (`lostDispatchUrlNotice`):
`DISPATCH_TOKEN_FILE is set and DISPATCH_URL is not, so Dispatch is off: this shell lost the
DISPATCH_URL it started with. Export DISPATCH_URL again, set to the Dispatch server that token is
for`. When the chosen file is missing, what may have named the server follows it: a dangling link
(`; <file> is a link to <target>, which does not exist`), then the unread OpenCode-era user file,
as in the missing-file text. The session's start notice (`sessionSettings`) says the same for
Dispatch. `DISPATCH_TOKEN` alone keeps the file's notice, since it also goes with a server an
envoy.json names. `DISPATCH_PUBLIC_URL`, when set, is Dispatch's dashboard as people open it, for
a session that reaches Dispatch at an address they cannot (a Legion pod's in-cluster one): the
settings carry it as `publicUrl`, less a trailing slash; every link the executor prints resolves
against it rather than the API's URL, which every call still goes to; a dashboard URL given as a
`ref` resolves on either address; and a value that is not an http or https URL disables Dispatch,
naming it in `error`.

With no bearer configured at all, the session's credential is traded at the secrets broker
instead (`AGENT_SECRETS_URL` with `AGENT_SECRETS_MACHINE_IDENTITY`, `brokerCredential`). The
credential is then a DPoP-bound access token presented under the `DPoP` scheme rather than a
bearer. Every bearer source above is resolved first and unchanged, so a session that has one
behaves exactly as it did. The machine's identity is one of five (`machineIdentity` in
`@legion/cipher-client/environment`). `kubernetes` reads a pod's projected token from
`AGENT_SECRETS_MACHINE_TOKEN_FILE`. `metadata` asks the metadata server for a Compute Engine
machine's Google identity token, for the audience `AGENT_SECRETS_MACHINE_AUDIENCE`. `pool` mints
a Google identity token through a workload identity pool, as the Go client's `PoolIdentity`
does (`pool-identity.ts`), for a machine with no Google service account of its own, such as an
Azure box. `pool` needs all four of `AGENT_SECRETS_MACHINE_TOKEN_FILE`,
`AGENT_SECRETS_MACHINE_AUDIENCE`, `AGENT_SECRETS_POOL_AUDIENCE` and `AGENT_SECRETS_POOL_ACCOUNT`.
`github-actions` asks GitHub for a GitHub Actions run's OIDC token, as the Go client's
`GitHubActionsIdentity` does (`github-actions-identity.ts`), from `ACTIONS_ID_TOKEN_REQUEST_URL`,
`ACTIONS_ID_TOKEN_REQUEST_TOKEN` and `AGENT_SECRETS_MACHINE_AUDIENCE`, read in that order. GitHub
sets the first two in a job that holds `id-token: write`. The client adds `audience` to the
request URL's own query and sends the request token as its bearer, follows no redirect, and reads
the answer's `value`; it refuses a request URL that is not https, except on this machine's
loopback, and no refusal names the URL or the token. It asks for a new token at every trade,
since the broker takes each GitHub token once, and the broker answers it from the workflow's
entry. `AGENT_SECRETS_MACHINE_AUDIENCE` must equal the broker's `BROKER_EXCHANGE_GITHUB_AUDIENCE`.
An identity with one of its variables missing is an error that names the variable.

`device-login` is the identity of a machine that has none of its own, such as a person's laptop
(`device-login.ts` in `@legion/cipher-client`). The machine logs in once with
`dispatch login --device` (below), and the login is kept in the file
`AGENT_SECRETS_DEVICE_LOGIN_FILE` names, else `~/.config/legion/device-login.json`, mode 0600:
the login's P-256 key as base64 PKCS#8 DER, its launcher credential's `credential_id` and
`expires_at`, its `refresh_token`, the `broker` it was made at, and the last `access_token` with
its `access_expires_at` and `access_life_seconds`, the life in whole seconds that token had when it
landed. The file is refused when its group or anyone else may read it, when its directory lets
them write to it, or when another user owns either. A trade presents that access token as the
subject, under RFC 8693's own `urn:ietf:params:oauth:token-type:access_token`, with no actor, and
every trade's DPoP proof is signed with the login's key; a read trade adds a holder proof (below).
A login made at another broker than `AGENT_SECRETS_URL` names is refused, and so is a broker on
`http` off this machine's loopback: the login's tokens cross that connection. It names the projects
`AGENT_SECRETS_PROJECTS` lists, and its session is the caller's own, else `AGENT_SECRETS_SESSION`,
else `device-login-<thumbprint>`, the first 12 characters of the key's thumbprint. The login is the
person's own credential, so no `agent-secrets login` sign-in on the machine is read beside it, and a
process with `PERSON_SIGNIN_TRADE=off` (the controller `legion controller start` launches) is
refused it, since the broker mints its trade as that person. Only this client trades a device
login. The Go tools that read `AGENT_SECRETS_MACHINE_IDENTITY` (`agent-secrets`, the helper,
`legion controller start`) refuse `device-login`, naming it as the TypeScript client's alone, so a
machine that sets it for its sessions sets it for them, not for those tools.

A refresh rotates the login's refresh token and revokes every access token the login answered
before, and the broker revokes the whole login when a rotated refresh token is presented again
after its five-minute grace. So each trade runs under the login's lock, the file beside it with
`.lock` added, which its holder touches every 5 s; a lock untouched for 15 s is taken as one whose
holder died (`device-login-lock.ts` holds the protocol). A holder stopped for longer, as a laptop
that sleeps is, loses the lock the same way: once it goes on, it refreshes and keeps nothing, it
looks at the lock again just before it replaces the file, and a trade of it the broker refuses is
traded again. Under the lock a process reads the file again, refreshes when the access token kept
there has less than a minute left, or when the broker refuses it as a token it no longer honours
("Token is not valid or has expired", or the login ended: a revoke ends a login before its token
lapses), keeps the rotated login, and trades. A read trade refreshes first while the access token
has less than the client's renewal window and a second left, capped at half the life the last
refresh gave it (`access_life_seconds`) and never under the minute: a token fresh from a refresh
meets it wherever that life is over a minute, and the next refresh comes no sooner than half a life
later wherever that half is over the minute. A file with no `access_life_seconds`, as an older
build writes it, keeps the minute alone until the next refresh. A refusal of the trade's session or
projects is not refreshed: a refresh cures none of those. A refresh the broker did not answer is
sent once more at once: the broker redeems the spent refresh token again within its five-minute
grace, and past it the login ends. The broker's 400 or 401 `invalid_grant` to a refresh is final
(`DeviceLoginEndedError` in `token-errors.ts`, decided by `endsGrant` in `oauth-refusal.ts`): the
login was revoked or ran out, and only a new `dispatch login --device` cures it. An
`invalid_grant` at another status, a 403 or a 429, is a refusal like any other
(`BrokerRefusedError`), and a later refresh gets past a 429. A trade whose time ran out while
another process of the machine held the lock says so (`LockHeldError`), and not that the broker
did not answer.

A pod writes under a session name only once its deployment has registered that name for the pod,
and its trade can reach the broker first. The broker refuses that trade with a description
beginning `pod_session_not_registered:`, and that refusal is never final (`podSessionUnregistered`
in `@legion/cipher-client/token-errors`).

The client trades through one retry loop per session (`BrokerTokenClient.token`), and no caller
starts a second one or cuts its wait short, so the broker sees one trade per slot of the loop's
schedule however many callers ask. This is gRPC's connection backoff (grpc/grpc,
`doc/connection-backoff.md`): capped exponential backoff with jitter, each wait between half its
length and all of it, around a per-trade timeout of 30 s. A caller is one of gRPC's two kinds. A
fail-fast caller, the default, is told at once, with `TradeRetryingError`, where the loop's last
trade failed in a way it trades again on and no live token is in hand: every Dispatch and Envoy
call, and a NATS connection's keeper. A wait-for-ready caller waits through the loop's retries:
the startup paths, which have nothing to do until they hold a token: a Claude Code channel
server's start (`ready({ life })`), for its life, and an Oh My Pi session's start
(`ready({ waitForReady: true })`) and a switch's follow, each for 20 s. Every caller waits
out a pod session not registered yet within its own deadline. What a trade comes to decides the
next one:

| The trade | The loop |
|---|---|
| a token | answers every caller, and both schedules start over |
| a pod session not registered yet | trades again 2 s later, doubling to 30 s, so the trade after a registration lands is at most 30 s away; that trade has its own 30 s |
| no answer within 30 s, no connection, a 5xx, a 408 or a 429, a DPoP refusal, a 4xx naming no OAuth error, an answer that is not the grant's, an identity source that could not be read | trades again 15 s later, doubling to 5 minutes, the Go client's schedule |
| a refusal no trade cures | ends; a call before the next slot is told the same refusal with no trade |

The loop logs through `console.warn`, as the Go client does: a pod session not registered yet
once, naming the session, again every 30 minutes while it lasts and once when it ends; every other
failure at each wait, saying when it trades again. Each Dispatch and Envoy call hands the
credential its own deadline (`RequestCredential.headers`), so a call still waiting on a pending
registration at that deadline fails with `RegistrationPendingError`, which names the session.
Once a renewal has failed, a call made while the token in hand is live goes on with that token.

The identity the client trades depends on the sign-ins on the machine (`brokerTokenClient`). With
no sign-in, the client trades the machine. The machine's identity is the subject, and the session
name is the caller's own session, else `AGENT_SECRETS_SESSION`. In a pod, where
`AGENT_SECRETS_MACHINE_IDENTITY` is `kubernetes` and `AGENT_SECRETS_KEY_DIR` names the role's own
directory, the DPoP key is the role's own agent-secrets key, the `key.pem` that
`agent-secrets keygen` wrote there, which the broker holds the pod session's trade to; when that
file cannot be read, the call fails naming the path. Everywhere else the DPoP key is one this
process makes. Where a person ran `agent-secrets login` on the machine, the client trades that
person, as the Go client does:

- The subject is the person's Google identity token. The client renews it at every trade, from
  the refresh token the sign-in keeps, at the sign-in issuer (`person-identity.ts`).
- The actor is the machine's identity token. The broker takes only a Google identity token as
  the actor, so the machine's identity is `metadata` or `pool`. The broker refuses a `kubernetes`
  or `github-actions` actor in its own words.
- The DPoP key is the sign-in's box key. So the broker records one sign-in for the box, and a
  revoke of that sign-in stops the session's trades.
- The session name is the caller's own session, else `signin-<person>-<thumbprint>`, where
  `<thumbprint>` is the first 12 characters of the box key's thumbprint (Go's `SessionFor`). It
  is never `AGENT_SECRETS_SESSION`: the machine trades under that name, and the broker gives a
  name to one identity only.
- The projects the token names are the ones `AGENT_SECRETS_PROJECTS` lists, project keys separated
  by spaces (`tradeProjects`). The broker names them only when Dispatch says the person may see
  each one, and refuses the trade naming the first they may not see. A person who signs in is of
  the broker's sign-in domain, so the broker asks Dispatch no member kind for this trade. A value
  that is no list fails the call naming the variable. The machine's own trade names no project.

Dispatch gives the token that names a person the `person` scope, so the session writes as that
person. It never gives a broker token `approve`: a review, an answer to an approval request, a
machine login's decision and a personal token's mint need the person's browser, or a personal
token (`DISPATCH_TOKEN`) the person gave `approve`. A read trade's token
(`DISPATCH_TOKEN_BROKER_READ`, below) holds `person` too, and writes nothing.

The client reads the sign-ins as the Go client reads them (`signins.ts`). The directory is
`AGENT_SECRETS_SIGNIN_DIR`, else `$XDG_STATE_HOME/agent-secrets/signins`, else
`$HOME/.local/state/agent-secrets/signins`. With two or more people signed in,
`AGENT_SECRETS_PERSON` names the person the session acts for. Without it, the client refuses and
names the people. The renewal goes to the issuer `AGENT_SECRETS_SIGNIN_ISSUER` names
(`https://accounts.google.com` when it is unset). It authenticates as the client
`AGENT_SECRETS_SIGNIN_CLIENT_ID` names, with the secret in the file
`AGENT_SECRETS_SIGNIN_CLIENT_SECRET_FILE` names. A sign-in on a machine that sets neither client
variable is an error that names both.

While a sign-in record is on the machine, the client never trades the machine in its place. An
error while it chooses, reads or trades the sign-in fails the call. Each refusal that a new
sign-in cures names `agent-secrets login`. Some refusals are final (`isFinalRefusal`): the broker
refused the sign-in, or the issuer answered the renewal of the sign-in 400 or 401 `invalid_grant`.
An issuer's 403, 429 or 5xx is an outage that a later call asks again about, whatever its body
names. The process keeps a final refusal against that sign-in, the person and the box key's
thumbprint, for the session it was refused to.
A later call of that session in that process fails with the same words and sends nothing to the
issuer or the broker. This stops when the store no longer holds that sign-in: a new
`agent-secrets login` makes a new sign-in, with a new box key. A new process, such as the next
`dispatch` command, keeps nothing and asks the broker again.

The one exception is `PERSON_SIGNIN_TRADE=off` (`PERSON_TRADE_VAR` in `signins.ts`). A process
sets it where its own credentials are a service's and never a person's. Under it, the client
trades the machine as where nobody signed in, even where a sign-in record is on the machine, and
it reads no sign-in at all. Unset or blank, the person trade is on. Any other value is refused
with an error that names the variable. `legion controller start` sets it for the controller
under every configuration. So the controller's own Dispatch credential, and its Envoy
credential where no Envoy bearer is set, trade the machine even where the operator ran
`agent-secrets login`, and no credential the controller trades at the broker acts in its
operator's name. A bearer the controller is given is presented as it is at main: the token
`dispatch_token_file` names, and an Envoy bearer from the operator's environment or from the
`envoy.json` under the HOME the controller keeps, each of which resolves before the broker
route. Only this client reads the setting, so the controller's `agent-secrets` secret reads are
signed as they would be without it. Its name lies outside `AGENT_SECRETS_*` because
`agent-secrets NAME -- <cmd>` strips most variables of that prefix from its child, so a
`dispatch` command the controller runs under `agent-secrets NAME --` keeps it.

Dispatch answers `401 BROKER_TOKEN_NOT_ACTIVE` for a token the broker no longer holds as live,
such as one a revoke ended before it lapsed. `DispatchClient` then tells the credential which
presentation was dead (`stale(session, presented)`), presents the credential again and sends the
request one more time. The credential drops that dead token alone, from the client that presented
it: a session the host moved to in between keeps its live token, and a token the client traded
since is kept; the next presentation uses the token the client holds now, trading at the broker
where it holds none. A keyed write, a named command's (`#write`), goes again under the same
`Idempotency-Key`. A read does this once for each send, and a keyed write once for the call: a
second such answer is a refusal Dispatch's auth made before its keyed layer ran, so it is the
write's refusal while nothing may have landed and the write is `DispatchWriteUnsettledError` once
something may have (`write-outcome.ts`). If the broker refuses the new trade, the call fails in
the broker's words, unless an attempt of a keyed write may have landed: then that write asks again
on its pacing, and with no ask left it is `DispatchWriteUnsettledError`. `dispatch api`'s request,
its GET or its write, goes through one `#send`: one resend after `401 BROKER_TOKEN_NOT_ACTIVE`,
never on a pacing, and never `DispatchWriteUnsettledError`. Dispatch's answer of no route does not
repeat it, and the command prints a refusal, the broker's refusal of the trade among them, as it
came.

A sign-in reaches the sessions that start after it. The broker holds a session name for one
identity for 90 days after the name's last token. So a session that traded as the machine cannot
trade as the person under its own name: the broker refuses that trade and says that the name
belongs to another identity. After `agent-secrets login`, start a new session rather than resume
one that traded at the broker as the machine. A session that used a bearer did not trade at the
broker, so you can resume it.

Each person's trade costs one refresh at the issuer and one `launcher_credential.token_login`
audit row at the broker. The broker writes that row for every person's ordinary trade, and it lets
no such trade decline the launcher credential (`personDeclinesNothing`). A person's read trade
declines it and writes no such row. The client also reads the issuer's discovery document once.
A broker access token lasts `BROKER_ACCESS_TOKEN_SECONDS` (900 seconds by default), and the client
trades again five minutes before the token lapses (`DEFAULT_RENEW_BEFORE_MS`), and never before
half the life the token had when it landed (`renewalAt`, the Go client's rule too), so a token of
less than ten minutes is traded again at its half life. So with the broker's default, a long-lived
session that keeps calling trades about every ten minutes. Every `dispatch` command, Claude Code
hook and `envoy-send` is a process of its own, so each one trades once.

A revoke of the sign-in on Dispatch ends the tokens of that sign-in, and the next trade is
refused with words that name `agent-secrets login`. Dispatch gives the sign-in's tokens no
`approve` (above), so an agent under it makes no personal token and decides no machine login. A
Dispatch release before that rule did give them `approve`: a personal token or a machine-login
approval an agent made under one then is not ended by the revoke, so revoke each on its own
Dispatch page.

`DISPATCH_TOKEN_BROKER=1` makes the broker route the only one. No bearer is read at all: not the
variables, not the chosen file's `dispatch.token`, not a checkout's pair. A machine that can trade
none leaves Dispatch disabled with an `error` naming the broker variable that is missing, rather
than falling back to whatever bearer the box holds. The broker route includes a person's sign-in
on the machine, as above. A deployment sets the variable on a session whose Dispatch credential
must be traded at the broker. The controller that `legion controller start` launches under
`dispatch_token_broker` is such a session. It runs in the operator's own HOME, where that
person's envoy.json holds their `dispatch.token`. Any other value of the variable is refused,
naming it: `DISPATCH_TOKEN_BROKER is "<value>" and takes 1 …`. The start also sets
`PERSON_SIGNIN_TRADE=off` for the controller, so its broker route trades the machine and never
the operator's sign-in (above).

`DISPATCH_TOKEN_BROKER_READ=1` narrows the trade of Dispatch's credential to reading. The trade
asks the broker for the scope `read`, and the token keeps the identity the same trade would have
had and writes nothing. `DISPATCH_TOKEN_BROKER_AS_OF=<an RFC 3339 time>` beside it adds a cut
(`as_of`), the past time the token reads at. Either setting takes the broker route as
`DISPATCH_TOKEN_BROKER=1` does and reads no bearer, so a shell that also holds `DISPATCH_TOKEN`
never writes with it. `DISPATCH_TOKEN_BROKER_READ` takes `1` alone. A cut without
`DISPATCH_TOKEN_BROKER_READ=1`, a cut that is not an RFC 3339 time with at most six fractional
digits, and a cut at the zero time (`0001-01-01T00:00:00Z`, under any offset), which the broker
refuses since it would read as no cut, turn Dispatch off with an error that names the setting. No
such error quotes the value.

The two settings narrow the Dispatch trade alone (`brokerCredential`'s `read`). The Dispatch
credential gets a broker client of its own, keyed by the session, the broker variables and the two
settings. The Envoy listener and NATS keep the session's shared client, which asks for no scope,
since both refuse a read-only token and a session's inbound messages must keep working. A
person's read trade sends no `launcher_credential`, which the broker reads as declined. It makes a
P-256 key of its own for the process and sends it as the holder key (`DPoP-Holder`) beside the box
key's DPoP proof. That key signs every proof Dispatch sees, so the box key signs the trade alone.
A device login's read trade also sends a holder key of its own beside the login key's DPoP proof,
because the login's key signs the machine login's launcher proofs and a key that signs launcher
proofs is bound to no read token.
The machine's own trade already proves a key the process made, which holds no launcher credential,
so it sends no holder key. A person's read trade is refused once the sign-in's launcher credential
lapses, until one ordinary trade renews it (the token files, below).

Under a read-only token, a write prints Dispatch's refusal (`READ_ONLY_TOKEN: …`) and exits 1.
`dispatch whoami` says that the credential may only read, and names its cut where it has one.
Under a cut, every other read prints `AS_OF_TOKEN_NOT_SERVED: …` and exits 1, because Dispatch
serves no read at a past time yet.

### The `dispatch` command

Agents reach Dispatch through one command, `dispatch <command> [flags]`, which each host plugin
bundles from `bin/dispatch.ts` into its `dist/dispatch.js` and puts on its agents' shell `PATH`
through a `bin/dispatch` shim. `dispatch-command.ts` derives the commands and flags from the
specs: one command per spec, named by the spec's name without its `dispatch_` prefix and with
hyphens for underscores (`issue-update`), one flag per field
(`reply_to_ask` → `--reply-to-ask`), `--<field>-file <path>` for every string (`-` reads stdin),
`--no-<field>` for a boolean, `--clear-<field>` for a nullable field or a list, a repeated
singular flag for a list (`--label a --label b`, `--option "Label: what it costs"`, split at the
first `: `; a label holding `: ` goes in `--options-json`, which the generated command line uses
for such a list), and `--<field>-json` for any other object. Only one flag of a call may read
stdin. The command reads each file and stdin as bytes and decodes them as UTF-8, keeping a byte
order mark as U+FEFF (`readFileText`, `dispatch-file-text.ts`, which `bin/dispatch.ts` hands
`runDispatchCli`). Bytes that are not UTF-8, which it once stored as U+FFFD, refuse the command,
for every field and every command, with exit 1, before anything is sent. A field that holds no
document's text, every field but one, is refused for them alone, in Dispatch's own refusal of the
first U+0000 or byte that is not UTF-8 among them, whichever comes first, as Dispatch reads them
(`unstorableBytes`): `NUL_CHARACTER: <field> holds a NUL character (U+0000) at character N,
which Dispatch cannot store` or `INVALID_UTF8: <field> holds a byte that is not UTF-8 (0xFF) at
character N, which Dispatch cannot store`, where `<field>` is the argument the flag sets (`body`
for `--body-file`) and N is where the character stands, counted from 1 in UTF-16 units, as
Dispatch counts it. UTF-8 holding a U+0000 is not refused there: `dispatch comment --body-file`
sends it to Dispatch, which answers it. The one field the command checks as a Markdown document's
bytes is `dispatch artifact`'s `content`, which the command's entry in the command table names
(`DispatchCommand.documentFields`, `dispatch-commands/table.ts`). Read from `--content-file`
or stdin, its bytes are refused whether or not they are UTF-8, as a Markdown `--path` file of the
same bytes is, in that upload's order and words (`documentBytesRefusal`) under `content`: text
past 1 MiB with `CAP_EXCEEDED` first, then the first U+0000 or byte that is not UTF-8. Text the
strict read decoded is UTF-8, and so is text given inline, so only its size or a U+0000 can
refuse it, and only such text is checked again in its UTF-8 (`documentFieldRefusal`). A value
given inline in argv holds no U+0000 on any kernel, and Bun hands the command its argv already
decoded, each byte that is not UTF-8 written as U+FFFD. Linux holds one argument to
`MAX_ARG_STRLEN`, 32 pages, which is 128 KiB on 4 KiB pages and 2 MiB on 64 KiB pages
(`include/uapi/linux/binfmts.h`), so with 4 KiB pages argv carries no text past 1 MiB, and with
64 KiB pages it can. A program that calls `runDispatchCli` itself can hand it either on any
kernel. The check runs on inline text however it is given, so such text is refused as the same
text read from `--content-file` is, whatever the page size.
For a string or enum field, the command refuses `--<field> -` and `--<field>=-` with exit 2,
and its refusal names `--<field>-file`. The one exception is `dispatch artifact --path`, whose value
names a local file. `dispatch --help` lists the commands, `dispatch <command> --help` its flags and
an example, and `--dry-run` runs the call's own argument check (`checkDispatchCall`: the owner, the
strict schema, the cross-field checks and the picture files), refusing the arguments the call would
refuse with the call's message, and otherwise prints the arguments as given, not as the call fills
them (an issue from LEGION_ISSUE or the ref). It stores nothing, and reads no state of this session
but what a `dispatch result` record's dry run reads (below). Each refusal of a file's, stdin's or
inline text's bytes above, and of a Markdown `--path` file's (below), comes before that check,
once the flags are read and before anything that needs Dispatch (its configuration, the refusal
of a Dispatch that is off, any client), by the dry run as by the call, with the same refusal line
and exit 1, with Dispatch on or off, so `dispatch artifact` refuses such a text without asking
Dispatch.
A command whose call asks Dispatch something before it writes asks it in its dry run too, through
its step in the command table (`DispatchCommand.dryRun`, `dispatch-commands/table.ts`). The one
such step is `dispatch artifact`'s: for text given inline, read from `--content-file` or from
stdin, it makes the call's own stored-text check
(`storedTextCheck`), and refuses with the call's refusal and exit code a text the call refuses: one
Dispatch would store under another spelling (`POST /api/v1/markdown/round-trip`, asked for every
text but the empty string), or one the call refuses for the text alone. A text whose typed
blocks' attributes differ from Dispatch's only in their order is no such text: the call uploads it
in Dispatch's order (`storedUpload`, `dispatch-commands/stored-spelling.ts`), so the dry run passes
it. The check is the one place that decides what blank text (what Go's `strings.TrimSpace` empties, `goBlankPattern`) is
asked: a refusal alone, never its spelling, and a check of it that gets no answer leaves the upload
to make its own refusal. Text past the 1 MiB of UTF-8 one document holds is refused with
`CAP_EXCEEDED` and exit 1 without asking Dispatch anything (`documentTooLarge`): the call refuses
it before it reads whether the owner holds the name, so the two answer alike under a held name
too, and the dry run builds no client for it, so it refuses it with no Dispatch configured too. A
round trip answer Dispatch marks as its refusal of the text (the `Dispatch-Text-Refusal` header,
`TEXT_REFUSAL_HEADER`, which the client reads for its presence alone), at any status, is refused
with Dispatch's own refusal (`<CODE>: <message>`) and exit 1, as the call refuses it. The route
makes, in the order, status, code and words of an upload, every check that upload makes of the
text alone, and marks each refusal. Every upload makes some of them, a replacement included (a
JSON upload of blank content over a held document is refused as blank before its text is parsed):
a NUL character (`NUL_CHARACTER`), a byte that is not UTF-8 (`INVALID_UTF8`), the bytes, and the
parse's own refusals - its element budget (`CAP_EXCEEDED`), markdown it refuses or that reads
back otherwise (`INVALID_MARKDOWN`), and an ask that breaks its content rule in markdown whose
rendering does not read back, which that refusal names instead (`INVALID_ASK_BLOCK`). Only a new
document's seed makes the rest, which a replacement relaxes: a block id written twice and an
element this build cannot create yet (`INVALID_MARKDOWN`), an ask block that breaks its content
rule (`INVALID_ASK_BLOCK`), a rendering past the bytes or the elements (`CAP_EXCEEDED`), and a
check proved by CI that breaks the rules its attributes keep (`INVALID_CI_CHECK`). The route
makes those only for a request that asks for them (`new_document: true`). It answers any other
as the most permissive replacement takes the text,
one over a document already holding what the text holds: with the rendering that replacement
stores, or with a refusal every replacement makes, the parse's own, the ask refusal it names
instead and a render that fails. The check asks for them where the call makes a new document,
without `--force`; with `--force` the upload may replace a document, judged by those looser
rules, so the check asks for none of them and compares the rendering Dispatch answers as for any
text, and every marked refusal it gets is the call's. So its dry run passes, with no warning, a
text the call's upload can then refuse for one of a new document's checks: under a name the owner
does not hold, because `--force` uploads a new document; under a name the owner holds, because a
replacement holds those rules for the blocks it adds or changes, and refuses a rendering that
grows past the bytes one document holds over the version it replaces. A check Dispatch adds to
the upload reaches the dry run with no change here. An unmarked answer says nothing of the text,
whatever its status. A
`--path` file Dispatch stores as a Markdown document (a path ending in .md or .markdown, which the
upload carries as `text/markdown`) is held to the same checks, in the same place: once the flags
are read, before the argument check and before anything that needs Dispatch, the command reads it
once, as its upload reads it (`documentFileRead`; the command table names the field,
`DispatchCommand.documentPathField`), and refuses a file past 1 MiB by its size before it reads
it and then by the bytes it read, and then the first U+0000 or byte that is not UTF-8 in those
bytes, in the upload's words, which name the field `file` (`documentBytesRefusal`). All of this
comes before AskGate's gate of the call too. The command hands the bytes it read to the dry run's
step, which checks those bytes and does not read the file again, and to the call. Where AskGate
reviews the call, its check reads the file a second time for its digest, and the call checks and
sends the bytes that read took in place of the command's; otherwise the call checks and sends the
command's bytes and does not read the file again. A file the command cannot read (missing, not
readable, a directory) is no refusal of its text: the argument check comes first, and then the
dry run's step, where it runs, and the call read the file and answer with the read's own error.
A file that is neither a regular file nor a directory (a FIFO, a device, a socket), whose read can
wait forever, the command does not read, nor does the dry run's step, which asks Dispatch nothing
about it: the call uploads it unchecked, and Dispatch's upload judges it. The call and its dry run
then send the bytes they checked to their own round trip (`POST /api/v1/markdown/file-round-trip`,
`file` in base64, with `new_document` as for inline text), which Dispatch checks as it checks an
upload's file part, and take only its refusal (`storedFileCheck`), never a spelling, since the call
stores a Markdown file as the store spells it. The call throws that refusal before it uploads, so
the two print the same refusal and exit alike; on any other answer, a spelling or none (no answer
in time, a failure, or a Dispatch without the route, which answers 404 or 405), the call uploads
the file, and its upload makes its own refusal. Since the file goes up whatever the round trip
failed with, the call asks it with the dry run's own client options (`askOnceWithinDryRun`):
once, within the same 5 s, with no wait before asking a route again. A file Dispatch stores as
its bytes, or a FIFO, a device or a socket, is asked nothing, by the call as by the dry run. What
only the call checks, since it needs more than the
text, stays the call's: whether the owner holds the name, the token of a replacement, a
replacement's own rules, a block pointer's target, whether a check proved by CI holds a recorded
result, and the issue's state. The call checks the text, however it is given, before it reads
whether the owner holds the name, so a text the check refuses gets the same refusal from the call
and its dry run under a held name too. A text that is not blank, given inline, read from
`--content-file` or from stdin, whose check got no answer from a Dispatch that has the route, the
call refuses there too, before it reads the name (`storedTextCheckFailed`), while its dry run
passes it with the warning below. A text the check passes, the call without `--force` still
refuses under a held name, and the dry run, which cannot know the name, passes it. With no
Dispatch configured, the dry run and the call alike still refuse, with exit 1, once the flags are
read and before the argument check (above), a document's text past 1 MiB, and one holding a
U+0000 or a byte that is not UTF-8, whether it is given inline, read from `--content-file` or from
stdin, or in a Markdown `--path` file. Any other text that is not empty, given inline, read from
`--content-file` or from stdin, and any other Markdown file the command reads, the dry run passes
with a warning that the call is refused for the same reason; the empty string, which it asks
Dispatch nothing about, it passes with no warning; and the call is refused for Dispatch being off,
exit 2. The
step waits at most 5 s. For a text it asks about (each but an empty text given inline, read from
`--content-file` or from stdin) and does not refuse before it asks, when it gets no answer (no
Dispatch configured, any other failure, no answer in time, or a Dispatch without the route), the
dry run passes, and one `dispatch: warning:` line on stderr says why and what the call does then,
as the check's kind says it: text given inline, read from `--content-file` or from stdin, that a
Dispatch without
the route cannot check, and blank text whose check got no answer, the call uploads unchecked; a
file the call asks about once more, and uploads unchecked if that gets no answer either; any other
text the call refuses until Dispatch answers.
The JSON stays alone on stdout. With
`DISPATCH_DRY_RUN_LOCAL=1` an upload's dry run asks Dispatch nothing, for a caller that reads the
arguments alone: it still refuses, once the flags are read, a text given inline, read from
`--content-file` or from stdin, or in a Markdown `--path` file, for its bytes. The variable does
not change a
`dispatch result` record's dry run. That dry run asks Dispatch through
the call itself (`dryRunAsksDispatch`): it needs Dispatch on, as the record does, reads what the
record reads (this session's title among it), and after the arguments prints the pull-request body
line Dispatch answers on `POST …/results/dry-run`, recording nothing. A retraction's dry run sends
nothing. Every other dry run reads no Dispatch, so it runs with Dispatch off too. A call that a dry
run passed can still stop on what Dispatch answers or on this session's state.

`dispatch login --device [--person <address>]` logs the machine in at the secrets broker
(`dispatch-commands/login.ts`). It is the one command that is no Dispatch call. A person runs
it once per machine, in a terminal of their own: it waits for the approval until the code
lapses, which a command timeout of an agent's shell can cut short. The command is each plugin's
`bin/dispatch`, under the directory the host installed the plugin in (claude-envoy's or
pi-envoy's), or `bun packages/envoy-client/bin/dispatch.ts` from a checkout of this repository.
It reads no host, no session and no envoy.json. It reads `AGENT_SECRETS_URL`, and the person who
approves the login from `--person`, else `AGENT_SECRETS_PERSON`. It prints the broker's page and
the code to type there, waits while that person approves it in Dispatch, and keeps the login
(above). Without `--device`, a person, a broker a device login may use (https, or http on this
machine's loopback), or a login directory of this user's own that this user can enter and write to
and others cannot write to, it exits 2 before any request, so no person approves a login the
machine could not keep there. It exits 2 too, before any request, for a file setting that names
something other than a regular file, a link to one included: a directory, which no save can
replace, a named pipe, which a read waits on until a process writes to it, a socket, a device or a
loop of links. Asked for anything but its `--help`, it exits 2, before any request, while either of
the command's token files is set (below). It reads the login a new one replaces, and every login it
trades, only from a regular file of at most 64 KiB, in a directory it checks before it opens the
file; where the setting names a link, it checks the directory of each link on the way and of the
file at its end as well. Every directory on the way to those but the ones above the home directory,
which hold the home directory itself, must be one that this user or root owns, the directory each
link on the way sits in included, and that others may not write to unless it has the sticky
bit, as /tmp has. Its group may write to it only where that group is this user's private group:
/etc/passwd and /etc/group name this user in it and no other member, as a Legion pod's /legion,
of group `legion`, is; where either file cannot be read, or /etc/group does not name the group,
the directory is refused. Each link on the way must be this user's or root's too. The login's own
directory is held to more: neither its group nor anyone else may write to it. The home directory
is this user's in /etc/passwd; where /etc/passwd cannot be read, or names no user of this uid
(a container can run as such a uid), it is `os.userInfo().homedir`, which Bun answers from the
`HOME` the process started with. In a user namespace other than the first one (a gVisor sandbox,
a rootless container), the overflow uid counts as root: root's own files show it there, and so
does every owner the namespace does not map, so each such owner passes. The walk stops at the
home directory as OpenSSH's StrictModes check of the path to a key does (OpenSSH reads the home
directory from the password database), and the owner rule is OpenSSH's too. The private-group
exception is Debian's OpenSSH (its `user-group-modes` patch, which Debian and Ubuntu ship),
not upstream OpenSSH, which refuses every directory its group may write to. The rest is this
command's own choice: it takes a sticky directory, which OpenSSH refuses, and it checks each link
on the way and the directory each is in, where OpenSSH resolves the path first. It exits 1 when
the broker refuses the login, the person denies it, its code expires, the broker answers a code
that would lapse before its first poll (refused before the code is printed, naming the broker's
interval and its codes' lifetime, or the hour this machine polls a code at most, and what must
change at the broker), or the broker answers a poll with any other 4xx, with another OAuth error
or without one (a proxy's page, a bare challenge), naming its status. The status decides first
(`oauth-refusal.ts` in `@legion/cipher-client`): a poll the broker did not answer, a 408, its 429
and a 5xx are polled again, whatever OAuth error they name, and only in another
4xx does a deciding error count: `access_denied`, which the broker answers 403, is the denial,
`expired_token` and `invalid_grant` the expiry, and `authorization_pending` and `slow_down` poll
again. It polls at the broker's interval, 1 s at least, and 5 s where it names none; each
`slow_down` adds 5 s, at whatever status it comes, and so does each poll the broker did not answer
or answered 408, 429 or 5xx, once for a poll that is both, as RFC 8628 has it. Such an answer's
`Retry-After`, in seconds or as a date, is waited before the next poll where it asks longer,
though never past the code's lifetime, which no wait passes and which is kept on the monotonic
clock, so a step of the system clock does not move it; a code that lapses after such polls says
how many came in a row before it. It then writes no file. It
exits 1 too when a login that was approved cannot be kept: it names the login's credential, which
the broker holds live, and says to revoke it in Machine logins on the page the code was typed
into. Where it wrote a copy of the login whole,
the copy is kept beside the file, under the file's name with `.kept-<uuid>` added, and the message
names that suffix, whatever fails after it, letting go of the lock included: moved over the file,
the copy is the login and its credential needs no revoke; unused, the person deletes it, since it
holds the login's key and refresh token, and revokes the credential. A login kept whose lock the
command then cannot let go of is kept all the same: it exits 0, says why the lock was left, and
names no credential to revoke; another process takes that lock over once it is 15 s old. A code
or an access token is taken to live an hour at most, whatever the broker says. What the broker
answers for the person to read (the page, the code, the credential's id) is refused where it holds
a control or format character (a right-to-left override), and an OAuth code is shown only where
it is one the clients know. The host it names is this machine's name in the form the broker takes
(`Adas-MacBook-Pro.local` is `adas-macbook-pro.local`). A second login of the machine replaces the
file, and the earlier login's credential stays live at the broker until it lapses or is revoked, so
the command names that credential and says to revoke it in Machine logins on the page the code was
typed into. The earlier login is the one the file holds just before the save, read under the lock
the save holds, so of two logins made at once the one kept second names the other. On a machine
whose `AGENT_SECRETS_MACHINE_IDENTITY` is not `device-login`, it says to set that variable.

`dispatch api --path <route> [--method POST|PUT|PATCH|DELETE] [--body-file <file>|-]` sends one
request of a route under `/api/v1` that no other command sends (`apiCommand`,
`src/dispatch-commands/api.ts`), with the session's own credential as every command presents it
(a set bearer, the token files' broker token, else a token traded at the secrets broker, with a
new DPoP proof for the request), and prints the answer's body: Dispatch's JSON, indented as every
command prints it, for `jq`; any other 2xx body as it came. It is the contract's spec
`dispatch_api`. With no `--method` it sends a GET (`DispatchClient.readRoute`); `--method` sends
`POST`, `PUT`, `PATCH` or `DELETE` in its place (`DispatchClient.sendRoute`), and `GET` is no
value of it. A write that a named command
makes goes through that command (`dispatch --help` lists them): `dispatch api` is for a route no
command sends, and it is how a script or a job writes, since a broker token is bound to a key and
no script can send it as a bearer. `--body-file <file>` sends the file's text, `--body-file -`
standard input, and `--body` its value, as the request's JSON body
(`Content-Type: application/json`), and only beside `--method`: a GET sends no body, and a write
given none sends no body and no `Content-Type`. Standard input is read only with
`--body-file -`. `<route>` is `/api/v1`, which lists every route, or a route under it, with its
query after `?`. The spec refuses, before anything is sent, a path that does not begin `/api/v1`
(a URL among them), one that holds a space, a backslash, a `#` or a character outside printable
ASCII, one that holds `"`, `<`, `>`, `^`, `` ` ``, `{` or `}` before its query, which a URL parser
would send percent-encoded, and one with a `.` or `..` segment, plain or percent-encoded, since a
URL parser would resolve any of those out of `/api/v1`; each refusal names the path, so the
credential goes to no other server and no other route. It sends the request once, and once more
only after Dispatch answers that the token it presented is not live (Dispatch authenticated
nothing, so nothing ran), with no redirect followed and no `Idempotency-Key`. On a 2xx answer it
exits 0 and prints Dispatch's JSON, indented as every command prints it; any other 2xx body as it
came, byte for byte, with nothing added and nothing else on standard output: a bodiless 204 as
nothing, a body typed JSON that is empty or does not parse, and stored content, such as a file
version's bytes (`GET /api/v1/artifacts/{id}/versions/{number}`) or a document version's Markdown
(`?download=true`). An answer that carries `Content-Disposition: attachment` is stored content
whatever its type, so a file stored as JSON prints as it was stored and is never parsed. Under a
host, the session's ledger records a body printed as it came by its Content-Type and size, never
its bytes. A 2xx whose body holds a credential the command presented (its token, or the DPoP
proof of its request) is not printed: something in Dispatch's place echoed the request, so the
command exits 1 with `CREDENTIAL_ECHOED`, naming the status, saying that a write may have been
made and to check whether it took effect before sending it again, and naming nothing of the
body. Any other answer exits 1 with its code: a refusal is Dispatch's error with its code and,
where Dispatch gives one, its hint
(`NOT_FOUND: no route for GET /api/v1/x; GET /api/v1 lists every route`), and an answer that is
neither a 2xx nor Dispatch's JSON, such as a proxy's page or a redirect, is named by its status. A
usage error, such as a path outside `/api/v1` or a body with no method, exits 2. Under Claude
Code, a JSON answer longer than the output limit is written to a file the output names; a body
printed as it came is written whole.

Under a host it acts as that host's session, as every command does, and refuses a missing
session id with the usage error they give. Where no host is found (`commandHost`), as in a
GitHub Actions job, it runs all the same, like `dispatch login`: it reads Dispatch from the
environment alone and no envoy.json (`unhostedDispatch`): `DISPATCH_URL`, and `DISPATCH_TOKEN`,
`DISPATCH_TOKEN_FILE`, the token files (below) or the broker's `AGENT_SECRETS_URL` and
`AGENT_SECRETS_MACHINE_IDENTITY`, with `DISPATCH_TOKEN_BROKER` read as every command reads it.
Its trade names the session `DISPATCH_SESSION_ID` holds, else `AGENT_SECRETS_SESSION`'s, else no
session, which the fixed session of the job's broker entry answers; with none, the broker refuses
the trade and the command exits 1 naming the refusal. With no Dispatch or no credential set it
exits 2, naming those variables. It keeps no session's state there, since no session runs it:
only a refusal made before a credential is resolved, a usage error or AskGate's refusal of a
write, goes to the ledger of the session the environment names, if any, and a failure of the
credential, the broker's refusal of the trade among them, or of the request records nothing. For
a write, the AskGate record of every session the environment names
(`ENVOY_SESSION_ID`, `CLAUDE_CODE_SESSION_ID`, `DISPATCH_SESSION_ID`, `CODEX_THREAD_ID`) is read,
host or no host. Where any of them is on, or cannot be read, it sends only `GET`, and refuses
every other method before any request with exit 1, naming the `dispatch` command that makes that
write (`WRITE_ROUTES`), which AskGate reviews; the gate sits in the command itself, under a host
and with none, and the refusal's ledger line carries AskGate's mark, so pi-envoy counts it. For a
write, a variable that holds no valid session id (1 to 128 letters, digits, `.`, `_` or `-`) is a
usage error that names the variable; a `GET` reads no AskGate record. Where every record is off,
or the environment names no session (CI, a terminal, a script), every method is sent.

A GitHub Actions job runs it with no host plugin. The command is the `bin/dispatch` of
`@sjawhar/codex-envoy`, which is that package's npm `bin`
(`npx --package @sjawhar/codex-envoy dispatch api …`), or of `@sjawhar/pi-envoy`, which declares
no npm `bin` (`node_modules/@sjawhar/pi-envoy/bin/dispatch` once the package is installed). Each
runs the bundled `dist/dispatch.js` with `bun`, so the job puts bun on its `PATH` first. The job
holds `id-token: write`, the broker lists its workflow (`BROKER_EXCHANGE_JOB_WORKFLOWS` for a job
that writes for no person), and the job's environment names the identity `github-actions`
(above), with `AGENT_SECRETS_MACHINE_AUDIENCE` set to the broker's
`BROKER_EXCHANGE_GITHUB_AUDIENCE`:

```yaml
permissions:
  id-token: write
steps:
  # bun is on PATH from an earlier step: bin/dispatch runs dist/dispatch.js with it.
  - run: npx --package @sjawhar/codex-envoy dispatch api --path /api/v1/whoami
    env:
      DISPATCH_URL: https://dispatch.example
      AGENT_SECRETS_URL: https://cipher.example
      AGENT_SECRETS_MACHINE_IDENTITY: github-actions
      AGENT_SECRETS_MACHINE_AUDIENCE: <the broker's BROKER_EXCHANGE_GITHUB_AUDIENCE>
```

`runDispatchCli` (`dispatch-cli.ts`) runs one call per process. The host plugin sets
`DISPATCH_HOST` (`omp` or `claude`) and the session's id (`CLAUDE_CODE_SESSION_ID` under Claude
Code, where claude-envoy's QA override `ENVOY_SESSION_ID` wins as it does on the
session's claim; `DISPATCH_SESSION_ID` elsewhere); with either missing the command exits 2
naming it, and an empty variable counts as unset. `DISPATCH_SESSION_TITLE`, or the session
directory's `title` file, names the session on what it writes. It resolves Dispatch from the
envoy.json its session's plugin chose (above): with Dispatch off it prints `dispatch: <notice>` for
nothing configuring it or a file turning it off, and `dispatch: dispatch config: <error>` for what
could not be read or used, each exiting 2. `DISPATCH_HOST` unset with
`CLAUDE_CODE_CHILD_SESSION=1` is host `claude`. stdout carries what
the model reads: the result, one `- picture: <path>` line per image (written under the session
directory), the follow notice the first time a call follows an ask, and every refusal. It exits 0
on success, 1 when Dispatch or AskGate refused the call, and 2 on a usage error. A usage error
includes every refusal of the flags or arguments before any request, and a corrupted `state.json`.
Under Claude Code the whole output stays under 25,000 characters: the result text is cut to fit,
the full text written to a file the output names, and the picture lines and follow notice kept.
The one exception is a call whose whole output is one JSON object (`dispatch issues --json`,
`printsJson`): a cut object is no JSON object, so it is printed whole on every host, and the CLI
prints its own lines on stderr. A `409 PARENT_STATUS_CONFLICT`, whichever command Dispatch refused,
prints the refusal, then each issue that holds the rule as `KEY [status] title`, then what to do
next, and exits 1, as a near copy's refusal does.
That last part follows what the call changed, so it names no step that the rule refuses.
A create (`dispatch issue`) is refused only under a done parent and takes no flag that moves it,
so it prints how to reopen that parent first
(`dispatch issue-update --issue <PARENT> --status in_progress --include-parent`, because the rule
also refuses a plain status change of that parent where the issue above it is done or not
started) or file the issue under an open parent or none.
A reparent (`dispatch issue-update --parent <PARENT>`) refused for the parent it names, which no
flag of the command moves, is told to move that parent first
(`dispatch issue-update --issue <PARENT> --status in_progress --include-parent`), then to run the
reparent again, with `--issue` and `--status` only where the call had them.
A status change prints the flags of `dispatch issue-update --status` that move the issues.
Those flags go with `--status` alone, and with `--reason` for done, so a call that also changed
another field is told to send that change in a call of its own.
A reparent refused for a child, against the status it also wrote, is told to run the status change
first, in a call of its own, and the reparent after it, without `--status`.
Dispatch refuses a credential scoped to one project each step that writes an issue of another
project (`403 PROJECT_SCOPE`), and the CLI does not know its credential's scope. So where an issue
the refusal names, which a flag of the line writes, is in another project than the issue the call
names (its `--issue`, else `LEGION_ISSUE`), the line also says that such a credential is refused
that step, and that a person, or a credential that names no project, must make it.
When Dispatch took the call but the session's state (a picture, the ledger, the memory) could not
be written, the result still prints, with one line saying so.

`dispatch-session-state.ts` keeps one directory per session under `DISPATCH_STATE_DIR` (default
`<XDG_STATE_HOME or ~/.local/state>/dispatch/sessions/<id>`): `state.json` carries what a
long-lived host remembered across calls (the triage lines and pictures already shown, and the asks
whose follow notice was printed), and
`results.jsonl` gets one line per call with the tool's `details` or its refusal, a refusal the CLI
made before the call reached Dispatch included, which pi-envoy's run-end check reads; a line that
is not JSON is named (file and line number) and skipped. Directories idle for 14 days are removed,
at most once a day.

`bun bin/stand-in-gateway.ts --status 502 --body html` serves one non-Dispatch answer (`html`,
`empty`, `text` or `json`, from `src/stand-in-gateway.ts`) on `--port` (ephemeral by default) and
prints its URL, for proving what a command shows when a gateway, not Dispatch, answers.

### The `dispatch` command's token files

A copy of the command can use a broker token that was traded elsewhere, so the copy holds no
subject token and trades nothing. Two settings name two files (`file-token-credential.ts`):

- `DISPATCH_BROKER_TOKEN_FILE` names a file that holds one broker access token and nothing else.
  Whitespace around it is trimmed.
- `DISPATCH_BROKER_DPOP_KEY_FILE` names a file that holds the P-256 private key the token is bound
  to, as one PKCS#8 PEM block (a block labelled `PRIVATE KEY`, as
  `openssl genpkey -algorithm EC -pkeyopt ec_paramgen_curve:P-256` writes it). A SEC1
  (`EC PRIVATE KEY`) or PKCS#1 block is refused, though the runtime would read it.

The key is made for the copy that holds the file. It is never a sign-in's box key, it signs no
launcher proof and holds no launcher credential, and no token is bound to it but the read-only
tokens traded for that copy. The broker's read trade binds such a key and refuses a key that a
live launcher credential is bound to. Keeping every other token off the key is the rule of the
driver that trades for the copy.

With both set, the command's one Dispatch credential presents `Authorization: DPoP <token>` and a
proof signed with the key, naming the request's method, its URL and the token (`ath`). It sends
them to `DISPATCH_URL` alone, and refuses a request to any other origin. `DISPATCH_URL` must be
Dispatch's public origin, because Dispatch checks each proof's URL against it. The command reads
both files again before each request and keeps neither, so the driver renews the token by writing
a new file and renaming it over the old one, and keeps the key. The Envoy listener never gets the
token: it takes only its own settings (`ENVOY_TOKEN_FILE`, `ENVOY_TOKEN`, `envoy.token`), and the
command makes no trade at the broker for Dispatch or for any other service.

A person's read trade does not extend the launcher credential of the person's sign-in. Once that
credential lapses, the broker refuses the driver's read trades and names the cure: one ordinary
trade of the person, which `agent-secrets` makes. The read tokens traded under the lapsed
credential end with it.

Each proof carries the process's own clock in `iat`, and Dispatch takes a proof only within one
minute of its own time, so the command must run on the real clock.

The command refuses to start, exits 2 and names every setting at fault, when one of the two
settings is set alone, when `DISPATCH_URL` is not set, or when any of these is set beside them:
`DISPATCH_TOKEN_BROKER`, `DISPATCH_TOKEN_BROKER_READ`, `DISPATCH_TOKEN_BROKER_AS_OF`,
`DISPATCH_TOKEN`, `DISPATCH_TOKEN_FILE`, `AGENT_SECRETS_URL`, `AGENT_SECRETS_MACHINE_IDENTITY`, a
`dispatch.token` in the chosen `envoy.json`, or a `dispatch.token` in the checkout's
`.envoy/envoy.json`. The checkout's file is read for this check even with `DISPATCH_URL` set, and
one that cannot be read is refused too, since whether it holds a token cannot be told. A `!`
command in either `dispatch.token` is found by the field's presence and never run. `dispatch login`
exits 2 too, `--help` aside, before any request, while either setting is set, and names each of the
variables above that is set beside them: a device login asks the broker and keeps a login in a file,
and the token files trade nothing. A file that cannot be read, a token file that is empty or blank,
and a key that is not P-256 in one PKCS#8 PEM block each fail the request with an error that names
the setting and the file and quotes none of its bytes. Under a read-only token, a write prints
Dispatch's refusal (`READ_ONLY_TOKEN: …`) and exits 1.

Only the `dispatch` command reads the two settings: it passes `tokenFiles` to
`sessionDispatchSettings`. Every other caller of `dispatchSettings` that finds either set, each
plugin and its hooks included (claude-envoy, its Codex server, pi-envoy, pi-legion), turns Dispatch
off with an error that says so, rather than writing with another credential beside them.

## Command specs

`@legion/contracts` `src/dispatch-tools.ts` is the single source for the
Dispatch specs behind the `dispatch` command: each one's name, description, field schemas
and example. `dispatch-command.ts` derives the command and its flags from them, and no host
adds aliases or descriptions of its own.
`dispatch issue-update` reads the issue first and sends `PATCH /api/v1/issues/{key}` as the
session actor: `status` (a Legion lifecycle status), `title`, `labels` (`--label`, replacing the
set), `labels_add` (`--labels-add`, repeated, adding to the labels the issue holds inside
Dispatch's write and keeping the others; never beside `labels`, which is refused before any
request), `priority` (`0` for P0 down to `3` for P3, or `null` to clear it), `route`,
and `external_links`: a non-empty list merges into the issue's existing links by URL rather than
replacing them, and an explicit empty list (`--clear-external-links`) clears every link instead;
`before` and `after` (`--before <KEY>` / `--after <KEY>`) move the issue on its project's board —
above that issue, below it, or between the two — and go as the server's `rank` field on the same
PATCH only when a neighbour is named, since a `rank` with neither neighbour ranks the issue last
in its project. Closing an issue (`status: "done"`) requires `reason`, which it posts as an issue
message before the PATCH, so the close carries its note; an issue already closed when it is read
is refused `ISSUE_CLOSED` before anything is posted. A failed post sends no PATCH, and
a PATCH that fails after the post names the posted message. The result is one line —
`KEY: status a -> b; priority -> P1; rank -> before KEY; linked <url> (N links)`, with
`priority cleared` on a clear and `reason posted as message <id> (<ref>)` ahead of a close — and a
server refusal keeps its `code` (`INVALID_STATUS`, `ISSUE_CLOSED`, `EXTERNAL_LINK_TAKEN`) at the
head of the thrown message.
With `--include-children`, `--unlink-children` or `--include-parent`, `dispatch issue-update`
sends `POST /api/v1/issues/{key}/status-change` (`DispatchClient.changeIssueStatus`) in place of
the PATCH. The three flags go with `--status` alone, and with `--reason` beside `done`, which is
posted first as for every close. The schema refuses them beside any other field,
`--include-children` beside `--unlink-children`, and `--include-children` with `--status done`,
before anything is sent (exit 2). Only a person who is signed in at a browser can close the open
issues under an issue together with it, and the command presents a token: that person does it on
the issue's page in Dispatch (Close issue, then Move them with it). The answer names each issue
the call changed with its status after the call, so the command reads before it writes: the
issue's own read names its children, with `--include-children` it reads the project's issues too,
and with `--include-parent` alone it reads each issue above the issue, one parent at a time. It
prints one line for each changed issue, in the order the steps ran: `KEY: status a -> b`, or
`KEY: unlinked from P`. The issue the call names always gets a line, which carries the posted
reason's message where there is one: in the order of the steps where the call changed it, and last
where the call left its status as it was. A refusal names the issues the steps before it changed.
A build older than the route answers no route: the
write asks again as every write does, then the command refuses naming the route and sends no
PATCH, since a PATCH would write the status without the option. The CLI's `--dry-run` prints the
arguments and asks Dispatch nothing.
`dispatch issues --json` prints `{issues, total, offset, limit}`, each issue with its status,
parent and labels, as one JSON object and nothing else.
`dispatch request-approval` refuses, before it sends anything, while the version the request would
name (the document's latest, `approval.latest_version`) holds a decision block open. It reads the
live document's `ask` blocks (`GET /api/v1/artifacts/{id}/blocks`), then that version's markdown
and the owner's asks (the issue's for an issue document, since the artifact route refuses those),
and judges each block by its `state` on every line of the version that opens it, so a line quoting
the opener can add an open block but never hide one: an answer or a resolution closes the ask at
once but reaches a version only when the document settles or the next edit is written. A block the
version does not hold yet, or one with no ask yet, counts as open. A request over an open block
would be retracted by the version its answer writes. The refusal names each block and its ask and
tells the agent to ask the human to answer or waive it. A document already approved at its latest
version skips the reads and gets the server's answer. `dispatch doc-edit` refuses, before it sends
anything, a `delete` or `retype` by block id that would take an `ask` block out of the document
(the block itself, or one inside a deleted block) while its ask is open: the edit would write its
version at once and settlement would retract the ask without writing another, so the question
would leave the human's Inbox unanswered and the approval refusal would find no block to judge.

`dispatch review --issue <KEY> [--artifact <document>] --version <N> --verdict <verdict>
[--reason <text>]` records a person's review of one version of an issue's document: what the
document header's Approve and Request changes record (`reviewCommand`,
`src/dispatch-commands/reviews.ts`). The verdict is `approved` or `changes_requested`. The
document is the issue's spec unless `--artifact` names another, resolved as `dispatch doc-read`
resolves it. `changes_requested` needs `--reason`, and the command refuses it without one before
it sends anything. It posts `{state, reason?}` to
`POST /api/v1/artifacts/{id}/versions/{number}/reviews` (`DispatchClient.reviewArtifact`), the
review route that names the version in its path, and asks no other review route. A Dispatch route
ignores a body member it does not declare, so a version sent in the body of the older
`POST /api/v1/artifacts/{id}/reviews` would be dropped by a build that predates the guard, and
the review recorded at a version that build chooses. A build without the versioned route answers
no route instead: once the client's no-route retries are spent, the command fails `NOT_FOUND`,
says that an answer of no route records no review, and names `GET /healthz`, whose `commit`
member is the commit of the build that answers that probe where its image was stamped with one
(`null` otherwise). The no-route answer does not name the build, and while a release rolls out
the build that answers `/healthz` may not be the one that refused, so run the command again a
minute later before reading the refusal as an older Dispatch. In a rollout, a send that a newer
build took but whose answer was lost, and was then sent again to an older build, may still have
recorded the review, at the version named: `dispatch doc-read` shows the document's approval.
A review records an approval or a request for changes in the person's name, so it works only with
a token that names a person who holds `approve`: a personal token (`DISPATCH_TOKEN`) the person
gave `approve`. A broker token, a person's sign-in included, never holds it. Dispatch refuses any
other token with `403 OAUTH_SCOPE_REQUIRED` or `403 SIGN_IN_REQUIRED`. `--version` is the version
the caller read, and it must be the version the review would record: the document's latest
settled version, or the version an open approval request names for the document. Dispatch
refuses any other version with `409 REVIEW_VERSION_MOVED`,
which names the version to read, and records nothing. With an approval request open, the review
answers that request and records the same verdict on every document the request names, each at
the version the request names. The output names the state, the version and the actor that
Dispatch recorded, and says when the review also answered an open request. A refusal starts with
Dispatch's code.
