# envoy

Docker-first cross-machine notification routing for AI agent sessions.

## Goals

- Route GitHub, Slack, WhatsApp, and agent-to-agent notifications to the correct agent session

- Keep all envoy-owned code and infrastructure in this repo

- Run every envoy component inside Docker containers

- Avoid host installs except Docker on remote machines that need it

- Inject secrets only at runtime via the `secrets` wrapper from `~/.dotfiles/shims/secrets`

## Repo boundaries

This repo owns:

- NATS cluster config and container packaging

- Envoy shared code

- Envoy listener / router / webhook receivers

- Compose files, deploy scripts, and Dockerfiles

This repo does **not** silently absorb changes to other maintained software.

If envoy needs changes in `@sjawhar/whatsapp-mcp` or another maintained repo:

- make that change in the other repo

- keep it as a separate clear jj change/commit

- do **not** bundle it into an existing `sami` octopus merge

- document the dependency here under `docs/external-repos.md`

## Secrets

Secrets are never stored in this repo.
Runtime injection uses:

```bash
secrets ENVOY_SLACK_SIGNING_SECRET ENVOY_GITHUB_WEBHOOK_SECRET -- <command>
```

Current secret names:

- `ENVOY_SLACK_SIGNING_SECRET`

- `ENVOY_GITHUB_WEBHOOK_SECRET`

The secrets broker (`cmd/cipherd`) supports these types for `agent-secrets secret create --type` and `secret retag --type`:

- `github-app`: stores an App id and private key; releases installation tokens for its listed GitHub owners.

- `google-login`: stores an `authorized_user` credential; releases a Google access token, not the refresh token.

- `azure-login`: stores `{"tenant_id":"<tenant>","client_id":"<client>"}`;
  releases JSON with `tenant_id`, `client_id`, `federated_token` and `expires_at` (RFC3339 UTC, from the token's JWT `exp`).
  Set `BROKER_AZURE_LOGIN_SECRET` to the one agent-secret name allowed to release that token; unset, every Azure release is refused.
  The configured secret must be human tier in the policy snapshot checked for each release, and its grant decision must record a person's approval, never an automatic grant.
  Retagging another secret to `azure-login`, or retagging the configured secret to agent tier, releases no Azure token.
  Each release reads the broker pod's projected ServiceAccount token from `BROKER_AZURE_TOKEN_FILE`, default `/var/run/secrets/azure/tokens/azure-identity-token`.
  Unreadable, malformed or expired tokens refuse the release; token bytes never enter an error or log.
  Mount the token read-only in the broker container alone, with audience `api://AzureADTokenExchange` and a one-hour lifetime.
  Exactly one Entra identity may trust that ServiceAccount subject, since a released token can sign in to every identity that trusts it.

- `google-service-account`: stores `{"service_account":"<id>@<project>.iam.gserviceaccount.com"}`;
  releases a bare Google access token for that account, minted by impersonating it with the broker's own credential, good for one hour.
  Set `BROKER_SERVICE_ACCOUNT_SECRETS` to whitespace-separated `<SECRET NAME>=<account email>` entries, such as `GCP_ADMIN=gcp-admin@example-project.iam.gserviceaccount.com`;
  it, and not the stored value, chooses the account the broker mints for, and a secret it does not bind releases nothing (`class=binding`).
  `--scopes "cloud-platform bigquery"` names the OAuth scopes the token carries, each the part of a Google scope after `https://www.googleapis.com/auth/`;
  without it the token carries `cloud-platform` alone, and the flag is refused on any other type.
  `--admin true` marks a secret whose grant is an administrator's:
  it is human tier, names its `--requesters`, and takes a `--max-grant` of an hour or less, as a `google-service-account` secret does whether or not it is marked.
  The secret's `--requesters` name its one person owner, and at human tier the registered services that may ask beside them.

Without a type, the broker releases the stored value unchanged.

One decoder reads each type's stored value, `secrets.ParseValue`, which the minters, the broker's policy load and `agent-secrets secret` all call.
`secret create --type`, `secret set` on a typed secret, and `secret retag` that changes the type refuse a value that type’s minter would refuse and write nothing.
A retag whose read of the value the store refuses as access denied writes the tags without the check and says so on standard error.
The broker reads each typed secret’s value once for each new version or type, and refuses a secret whose value the decoder refuses as `value-malformed`.

## Layout

- `internal/contracts/generated.go` — generated Go contract output from `packages/contracts/scripts/gen-go.ts`

- `internal/config` — env parsing

- `internal/webhook` — the GitHub, Slack and Ghost Wispr webhook handlers, and the `ENVOY_WEBHOOKS` read at start-up

- `internal/verify` — GitHub, Slack and Ghost Wispr signature verification

- `cmd/listener` — per-machine listener service, which serves the webhook routes

- `docker/Dockerfile` — the one released image, holding the listener, Dispatch, the broker and `agent-secrets`

- `deploy` — compose files and deploy scripts

- `docs` — architecture, constraints, external repo touchpoints

## Development

Run the Go commands from `packages/envoy`:

```bash
go build ./cmd/dispatch ./cmd/listener
go test -timeout 20m ./...
go test -count=1 -run '^TestName$' ./internal/bus/
gofmt -l .
go vet ./...
```

`internal/dispatch/api` alone takes close to `go test`'s ten-minute default deadline, so a
run that includes it passes `-timeout 20m` (`docs/testing.md` at the repository root, "Go's
package deadline").

`./...` here does not build `infra/`, a Go module of its own that imports packages of this
one (`internal/bus` and `internal/contracts` among them): after a change to one of them,
vet `infra/` too (`infra/README.md`, "Changing the module").

The Postgres-backed tests read `DISPATCH_TEST_DATABASE_URL` (Dispatch) and
`BROKER_TEST_DATABASE_URL` (the secrets broker). Without them most of those tests skip, and
`go test` still prints `ok`. `scripts/dev-postgres.sh` starts a pgvector Postgres and
prints its URL, and `scripts/database-skips.sh` (`scripts/README.md`) fails a run on any
skip.

Build tags hold the slow and live suites, which `go test ./...` does not build: among them
`natslive` (a NATS image, so it needs Docker), `smoke` (the listener image),
`multiprocess` (Dispatch and the broker), and Dispatch's `twobuild` and `memory`
(`cmd/dispatch/README.md`, "Tests"). The `ci-job-*.yaml` files in `.github/workflows/` at
the repository root show how CI runs each.

`internal/dispatch/pmdoc` tests compare with the browser editor through bun scripts in
`pmdoc/gen`: they need `bun` and a `bun install` at the repository root. Without `bun` they
skip, except where `CI` is set, where they fail.

## The edge listener

`ENVOY_EDGE_PROJECTS` makes a listener the edge listener, the one an outside person's agents reach Envoy through.
It names the projects the edge serves: project keys separated by commas or white space, read as a broker token's own list is (`parseEdgeProjects`, `internal/config/config.go`).
A list that no token could carry refuses the start, naming the variable, and so does a set value that names no project, such as a lone comma or white space.
Unset or empty, the listener serves as every other listener does, and nothing in this section applies to it except what a bullet says holds for every listener: `/readyz` (**Health**) and the closed `/v1` gate's `503` ahead of any credential (**A `503` and the balancer's retry**).

The edge holds each session to the projects of the broker token that admitted it, also in a deployment where each project is public.
It follows, stores and shows a session no topic of no project, except the GitHub topics of its projects' repositories, and at each registration it releases the session's claims of another project's roles that its view of the role claims shows (see **Roles** below).
The edge takes over no frame: `takeOverFrame` is off at an edge listener, whose grant publishes no kept role lane, so a role message the role lane forwarded to the session's own agent subject before that registration reaches the session after it (`TestAnEdgeRelaysARoleMessageForwardedBeforeItsRegistration`, which turns to require the frame withheld once the check below is in).
The relay's check of each frame's topic holds such a frame, and with it two cases that need a writer other than the edge: a role message forwarded under a claim another writer granted the session after its edge registration, and a topic another writer stored in the session's interest record, taken in at a later refollow. #4730 builds that check on a base this branch does not hold.
No code holds the edge closed until that check is in: `ENVOY_EDGE_PROJECTS` alone turns edge mode on. The gate is the deployment plan's: no edge listener opens to an outside person before its build holds the relay's check of each frame's topic, from #4730 or the change that brings it to this base.
The header of `cmd/listener/project_scope.go` lists the rules and the function that holds each; `cmd/listener/edge_mode_test.go`, `cmd/listener/edge_roles_test.go` and `cmd/listener/edge_repositories_test.go` test each:

- **Start**: the edge refuses to start beside `ENVOY_API_TOKEN`, any `ENVOY_OIDC_*` setting, `ENVOY_DISPATCH_PRINCIPAL`, `ENVOY_WEBHOOKS` or a webhook secret, and without the broker-token settings (`ENVOY_AGENT_SECRETS_URL`, `ENVOY_AGENT_SECRETS_ACCOUNT_TOKEN_FILE` or `ENVOY_AGENT_SECRETS_GOOGLE_AUDIENCE`, and `ENVOY_PUBLIC_URL`).
  Each refusal names the variable (`validateEdgeMode`).
  Dispatch sends its role-marked messages through a shared listener, and no session of the edge sends as Dispatch, so the edge names no principal whose send may carry a role mark.

- **Admission**: `/v1` admits a broker token alone, of project scope `named` or `person`, that lists at least one project and only projects the edge serves (`EdgeAdmits`).
  Any other session's broker token gets `403` with the reason `token_not_admitted`, whose text names what the token carries (its scope, or a project it lists that the edge does not serve) and no project the edge serves; the projects served go to the log line `listener: edge listener refused a broker token` alone.
  A read-only token, and a request with no broker token, get `401` before any handler runs: the listener's verifier refuses a read-only token as no session's.
  `/metrics` and `/healthz` answer on the same port with no credential, so the edge's public route forwards `/v1/` alone.

- **Behind Google's load balancer**: the edge is reached through Google's global external Application Load Balancer, so every request comes from one of its proxies, in `35.191.0.0/16` or `130.211.0.0/22`, or `2600:2d00:1:1::/64` over IPv6 (Google's "GFE proxy ranges").
  That holds for container-native backends (zonal NEGs), which the edge's route uses: behind instance-group backends kube-proxy rewrites the source to a node's address, and every request would be keyed on the node.
  From those ranges the edge reads its caller's address from the second-to-last entry of the one `X-Forwarded-For` line the balancer writes, and keys the broker token's introspection bucket on it (a burst of 20, then five a second, per address and process: `ciphertoken.DefaultAddressLimit`); an IPv6 caller is keyed by its `/64`.
  A request from those ranges with no `X-Forwarded-For`, more than one line, a line of one entry, or an entry there that is no IP address gets `400` before the broker is asked anything, logged as `listener: edge request without the address Google's load balancer forwards`.
  A request from any other address is keyed on its connection, as on every other listener, and its `X-Forwarded-For` chooses nothing (`edgeCallerAddress`, `cmd/listener/edge_forwarded.go`).
  The `400` body is the listener's plain error (`listenerwire.APIError`), since no project rule refused the request.
  That trust rests on the deployment, not on the code: only Google's Application Load Balancer may front the edge port. Google's proxy Network Load Balancers send from the same ranges and append no `X-Forwarded-For`, and so would a sidecar in front of the edge port; behind either, a client would write the entry the edge keys on.
  Keying an IPv6 caller by its `/64` hands one host that holds a `/48` 65,536 buckets (read from the code, not measured); a `/56` or `/48` key, or one ceiling shared by every bucket, would bound that.

- **Health**: every listener serves `GET /readyz`, which reflects only whether /v1 serves: 503 until the /v1 gate opens, 200 after; the edge's load balancer health check reads it, and the deployment's health-check policy for the edge names the path.
  It takes no credential, answers `503 {"status":"starting"}` and `200 {"status":"serving"}`, and reads nothing else (`readyzHandler`).
  The gate never closes again, so once /v1 opens `/readyz` answers 200 also while the bus is down and /v1 answers bus-unavailable `503`s: the pod's readiness probe stays on `/healthz`, which answers `503` then, so such a replica still leaves the balancer's backends (an inference: that the edge's backends follow the pod's readiness rests on its Deployment, which is outside this repository).
  `/healthz` is unchanged for its other callers: it answers `200 {"status":"starting"}` from the listener's first answer, before `/v1` serves, so a balancer that read it would send requests to a pod that answers them `503`.

- **A `503` and the balancer's retry**: Google's balancer sends a request that carries no body once more after a `502`, `503` or `504`, with the same headers, its DPoP proof among them, and a proof is good for one request.
  So the listener answers every `503` it can before it checks or records the proof: the closed `/v1` gate answers before any credential is read (`startingGate.ahead`, `v1API`), on every listener, and a broker or a spent-proof store that did not answer ends the check before the proof is recorded.
  On the edge alone, a `503` a route answers after the proof was recorded, to a request with no body, gives the proof back: the listener deletes the record that request's own presentation wrote, and only that record, before it writes the `503` (`proofReleasingWriter`).
  The balancer's retry is then served, and a proof presented again after a request it served is still refused as a replay.
  A `503` says the listener cannot tell whether it acted, and the body-less routes are idempotent, so the retry has the outcome the client's own retry with a fresh proof would have.
  A proof signs the method, the path and the token, and not the query, the body or any other header.
  So a given-back proof can be presented once more on the same method and path with another query, such as a session's `DELETE` without the conditions its query carried; that takes the token and the proof in plaintext, inside the proof's window.
  TLS covers the client's hop to the balancer alone: the hop from Google's proxy to the pod is HTTP inside Google's network, where the token and the proof travel in plaintext, so whoever reads that hop could replay a given-back proof once; that risk is accepted.
  A request with a body keeps its proof spent whatever its status; the balancer does not retry it, and the client sends it again with a fresh proof.
  Three cases still answer the balancer's retry `401`, and the status stays `503` in each, the one every route documents for a fault the client can ask again about:
  - a spent-proof store that did not answer while its write of the proof landed;
  - a release whose delete did not reach the bucket in its second (`proofReleaseBudget`), which a bus that did not answer the route makes likely, logged as `listener: a 503 could not give its request's DPoP proof back`;
  - a `502` or `504` the balancer wrote itself after the proof was recorded: a connection that ended before an answer (a killed pod, a handler that panicked, a shutdown that ended with the request in flight), or a request past the backend timeout.

  The retry's create reads the delete marker from the stream's leader, since the edge's bucket handles read with stream message gets (`bus.WithStreamMessageReads`), so `ENVOY_NATS_REPLICAS` changes none of this.

  The relay socket's client reads an upgrade refused `401` as a refused token and opens the socket again after a second with a fresh token and a fresh proof.
  A plain `/v1` call gets the generic `401` body every refused credential gets, which names nothing, and the client sends no request again on a `401`; the listener logs it as `listener: broker token rejected` with the reason `proof_invalid`, the class a replay is logged under.
  Every other listener gives no proof back, since no such balancer stands in front of it.

- **Topics**: a registration follows only the session's own agent subject, the topics of the token's projects, and the GitHub topics of one repository each that the repository answer of those projects names.
  The `403` of a refused registration lists each refused topic, with the reason `repository_not_in_project` when each is a GitHub topic and `topic_not_in_project` otherwise, and nothing of the registration is stored (`edgeTopicHold`).
  The topics the session's interest record already holds, which another listener may have stored under a token of other projects, are held to the same rule at each registration, a registration planned again after a conflict included.
  Each one the rule refuses is removed from the record and from the session's consumer, and the answer's `warnings` name it.

- **GitHub repositories**: the edge asks the secrets broker which repositories each project's issues resolve to, `GET <ENVOY_AGENT_SECRETS_URL>/v1/project-repositories?project=<KEY>`, with its own broker account.
  It asks at each registration that follows a GitHub topic, and every 30 seconds for each project it serves, each project's read on its own (`cmd/listener/edge_repositories.go`), and it compares owner and name ignoring case.
  A registration for which it gets no whole answer gets `503` with the reason `repositories_unknown`, whose text is the listener's own: what the broker answered goes to the log alone.
  The relay delivers a GitHub event only while an answer no older than 60 seconds names the repository the event's own payload names.
  A subject writes a dot in a name as `_`, so `acme/a.b` and `acme/a_b` publish on one subject, and an answer that names one admits only that one's events.
  It acknowledges any other GitHub event undelivered and counts it in `envoy_edge_github_withheld_total`:
  under `reason="not_in_project"`, or under `reason="answer_unknown"` while a project of the session has no fresh answer.
  In that second case the session's next frame carries `Envoy-Gap: true` with `Envoy-Gap-Reason: repository_withheld`, and every frame that is not GitHub's is delivered as before.
  The mark is kept for the session until a frame carrying it is published: across the session's relay runs on this listener, and on the session's entry in `envoy_sessions` (`session.SessionEntry.RepositoryGap`), which the next listener to relay the session reads after a move or a restart.
  The relay writes the mark on the entry before it acknowledges such an event, and gives the event back for redelivery when the write fails or the bucket holds no entry of the session; every other write of the entry, a registration's and the outage renewal's among them, keeps the mark the stored entry holds (`session.SessionRegistry.PutWithin`, `RenewWithin`).
  A mark is an id of the relay process that wrote it, and the clear after that relay's frame removes that mark alone (`ClearRepositoryGap`), so while two relays of one session run at once, as while it moves, a mark the other relay wrote since stays for that relay's frame.
  A gap mark with no reason is the stream's own retention gap, and a frame that marks both carries no reason.
  A refresh removes a session's interest in a repository that left its projects.

- **Publishes**: `POST /v1/messages/publish` takes only a role subject in the form that names the project, `notifications.role.<KEY>.<role>`, of one of the token's projects and for a role that works for that project;
  anything else gets `403 topic_not_published` (`edgePublishRefusal`).

- **Senders**: `POST /v1/messages/send` and `POST /v1/messages/publish` take only a message from the session itself: `source` `agent` or left out, and `source_session` the token's session.
  Any other gets `403 sender_not_session` (`edgeSenderRefusal`), so no edge session sends a message under another source, such as a Dispatch event or a person's message, or under no session.

- **Roles**: `POST /v1/roles/set` and `GET /v1/roles/{role}` take only a role that works for one of the token's projects, else `403 role_not_in_project` (`edgeRoleRefusal`).
  A role held by a session whose registering token listed none of the token's projects, live or lapsed, reads as one no claim holds (`edgeScope.holder`): `GET /v1/roles/{role}` answers it `404 unclaimed`, and a publish to it and a claim's `409` name no holder, client instance, state, last registration or hand-back (`edgeScope.namesHolder`). The holder is another project's session, whose id and activity the edge shows no one; a publish still reaches it.
  Each registration first releases the claims of another project's roles that the session holds, which another listener granted it or a token that listed more did, and the answer's `warnings` name them (`store.Registry.ReleaseRefusedRoleClaims`).
  It finds them in the session's stored interest row and in the listener's view of the role claims, which it may serve up to 2 minutes after it stopped following the role bucket, so a claim the view does not show yet is released at a later registration; while the view cannot be served, the release reads the role bucket itself.
  A release that fails refuses the registration with the retryable `503`, and nothing of it is stored; the `503` names the claim record the bus answered and the release could not read or free, or says the bus did not answer. It renews only the session's claims of roles of its token's projects.
  A claimant made live again is handed the kept messages of its token's projects' roles alone.

- **Sends**: `POST /v1/messages/send` reaches only a session whose registering token listed one of the sender's projects.
  Any other target gets the `404` of a session no listener has heard of, and no wake is reported (`edgeScope.hides`).

- **Reads**: `GET /v1/sessions`, `GET /v1/registry/{session_id}` and the three `/v1/legion/agents` reads answer only the sessions and Legion agents of the token's projects, each session with only those projects' roles and topics (`edgeScope`);
  any other session gets the `404` of an unknown one.
  A session's own `GET /v1/interests/{session_id}` shows the topics of its token's projects alone.
  `GET /v1/interests/` and another session's `GET /v1/interests/{session_id}` get `403 session_not_in_project`, and a write of the Legion agent registry gets `403 legion_registry_not_written`.

Every refusal of a project rule above has the body `listenerwire.EdgeRefusal`, whose comment in `listenerwire/listenerwire.go` names each reason and says how a client reads a refused registration's `refused` list.

Every listener that checks broker tokens records the registering token's projects on the session's entry in `envoy_sessions` (`session.SessionEntry.Projects`).
The edge's send and read rules read that record, and no answer of the API carries it.

On every listener, a role's name is read as the claim it names by `contracts.RoleClaimOf`, whose comment says which project each name works for and which key it gets (`store.RoleClaimKeyOf` is that key).
A free-form role under a project (`PRIV.lead-reviewer`) is that project's own claim, keyed `<project>.<role>`, beside the claim of the same name under no project;
every other name keeps its bare role name as its key.
`POST /v1/roles/set` refuses with `400`, naming both projects, a Legion role under any project but the one its own name gives, and the edge's role and publish rules read a role's project by that function too.
A role message on a lane in the form that names a project, for a Legion role of another project (`notifications.role.PRIV.legion-infra-reviewer`), reaches no holder: the role lane drops it with the delivery exception `no_holder` on the lane's exception twin, as for any role message that reaches no holder, and `POST /v1/messages/publish` refuses it with `400` naming the role's own project (`roleLaneProjectError`).

## The relay socket

`GET /v1/sessions/{session}/relay` is a WebSocket that only the edge listener serves (`cmd/listener/relay_socket.go`).
A session that holds no NATS connection of its own, such as an outside person's agent, receives its relay frames on it and answers them over HTTPS.
Every other listener answers the path `404`, an upgrade included, so no shared listener's grant has to cover it.

- **The upgrade**: the subprotocol `envoy-relay.v1`, `Authorization: DPoP <token>` and a `DPoP` proof for `ENVOY_PUBLIC_URL` plus the path, which `apiAuth` checks first, as it checks every `/v1` request, the edge's admission included.
  Then a `{session}` that is not one NATS subject token (it holds a `.`, `*`, `>`, white space or a control character) gets `400` before the upgrade, since its relay subject would be a wildcard or reach under another session's.
  A token of another session gets `403`; an upgrade without `envoy-relay.v1` gets `400`; an `Origin` of another host gets `403` (a client outside a browser sends none); a session's fifth socket on one replica, or a person's seventeenth on one replica, over all of their sessions (`relaySocketMaxPerCaller`; a token that names no person counts against its service), gets `429`; an upgrade past the replica's 256 sockets, or while the listener stops, gets `503`, on which the client opens it on another replica.

- **Frames**: text frames of JSON.
  `{"op":"sub","sid":<int>,"subject":<s>}` subscribes the session's own relay subject (`bus.DeliverSubject`) on the sockets' own NATS connection; any other subject gets `{"op":"err","sid":<int>,"code":"subject_not_granted"}`, and a second `sub` gets `subscription_limit`, since a socket has one subject to subscribe.
  `{"op":"unsub","sid":<int>}` ends a subscription.
  `{"op":"pub","subject":<s>,"data":<base64>}` publishes a receipt, whose subject must be the `reply` of a frame the socket forwarded in the last 10 minutes and whose data one of the relay's reply words (`received`, `progress`, `done`, `nak`), else `publish_not_granted`.
  The server's `{"op":"msg","sid":…,"subject":…,"reply":…,"headers":{…},"data":<base64>}` is one relay frame; `headers` is always present, each name to every one of its values in order.
  A frame the socket cannot read gets `{"op":"err","code":"bad_frame"}` and a close `1008`; a frame over 1 MiB closes the socket `1009`, and a binary frame `1003 text_frames_only`.

- **Order**: the socket handles each frame whole, a `sub`'s subscribe and the flush that makes the server hold it included, before it reads the next one.
  The client's `flush()` is a WebSocket ping, so its pong comes only after every frame sent before it is handled.

- **What it carries**: a frame's receipt subject is the owning machine's, under `notifications.envoy.relayreply.<machine id>.` (`natsgrant.EdgeRelayReplyPrefix`), since the edge's grant publishes no reply inbox and the socket may be on another machine (`startSessionRelays`): the edge's grant publishes every machine's receipts and subscribes only its own, so the socket's replica publishes the client's receipt on the owner's subject, which the owner alone reads.
  The owning replica holds at most 16 of a session's frames in flight, awaiting `done` or `nak` (its session consumers' MaxAckPending, `relayEdgeInFlight`), so a client that reads promptly takes any backlog with its socket open.
  The connection probe is never forwarded or answered, and a message on the relay subject whose reply is no receipt of the session's own frame (the receipt family, a machine id, a relay's nuid, the session and a token: `relayReceiptOf`) is not forwarded either, so a client is handed no other session's receipt to publish.

- **While it is open**: every 20 seconds, and once at its token's expiry, the socket asks the broker about its token again, from a copy of the upgrade request, spending nothing of the source address's introspection budget (`Verifier.LiveUntil`); every 20 seconds it also pings its client.
  It closes `4401 credential_expired` at the token's expiry, also while the broker still answers it live with that `exp` for up to a second after it, `4403 credential_refused` when the broker no longer holds the token live (a revoke of the token or of the device login it was traded from) or its answer names another caller, and `1013 broker_unavailable` when the broker cannot be asked.
  It closes `1013 nats_unavailable` when its subscription is lost, and `1013 slow_consumer` when its client reads its frames slower than they come: past 32 frames or 16 MiB that the socket holds for it, past 64 MiB that one person's sockets on the replica hold together (`relaySocketCallerPendingBytes`, a quarter of the replica's budget), or past 256 MiB that all of the replica's sockets hold together, whoever opened them (`relaySocketReplicaPendingBytes`, a quarter of the listener container's default 1Gi).
  Each socket holds its frames in a queue of its own, which one writer drains in order, so the replica's count of what its sockets' queues hold is exact, a frame blocked on a client that reads nothing included; a frame still in the NATS client's own buffer ahead of a queue is not in the count.
  The listener's stop closes every socket `1012 listener_stopping`.
  A close takes at most one second (`relaySocketCloseGrace`), also for a client that reads nothing: no close or check waits for a data frame blocked on such a client, and a write that fails drops the connection at once.
  A frame unanswered at a close, or held by a socket that closed `slow_consumer`, is sent again by the owning relay, so a reopen loses nothing.
  So is a frame the NATS client drops past its own pending limits ahead of the queue (the socket's 32 frames and 16 MiB): the socket stays open, and the frame comes again after the owning relay's ack wait.

- **Tests**: `cmd/listener/relay_socket_test.go` and `cmd/listener/relay_socket_caller_test.go` run the socket in one process with a stand-in broker, on the rig of `relay_socket_fixture_test.go`.
  `cmd/listener/relay_socket_live_test.go` (build tag `natslive`) runs two edge listeners under `natsgrant.Edge` on the hub's NATS image, with the real broker of `internal/cipher/ciphertest` and 30-second tokens; its binary checks every second (`-ldflags "-X main.relaySocketCheckBuild=1s"`).

- **Rollout**: the socket forwards every relay frame of its session, and holds no frame to the projects of the token that opened it.
  A session's relay subject can carry a frame of a project the token does not list: a role message of another project's role, another project's subject in the session's consumer filter, a GitHub event judged against the session's stored projects, a message any user that publishes `notifications.>` puts on the subject, or a frame of a project the person has since lost, from an earlier registration.
  So no edge pod serves relay sockets until the hold of each frame to its token's projects is in its build: `edgeFrameVerdict`, which the change that holds the edge relay's frames to a session's projects adds, called from the socket's `forward` before a frame is held.

## Local end-to-end

Run the reusable local acceptance driver from the repository root:

```bash
packages/envoy/scripts/e2e-local.sh
```

It builds `cmd/listener` and a throwaway NATS server of its own (`internal/testnats/cmd/natsd`), so it needs no container runtime, starts both, and submits signed public GitHub fixtures plus a three-paragraph direct agent message to a stand-in session that takes every delivery from its own NATS consumer.
The driver asserts the one-warning response for a never-seen GitHub repository, refuses a registration naming a port, keeps an unheld role's publish and hands it to the stand-in session that claims that role, keeps a publish to a role whose holder is gone and hands it to that session when it claims the role again, refuses a `keep: false` publish to an unheld role, validates settled pull-request checks, lifecycle and comment topics, PR-less workflow and Actions-job behavior, obsolete-topic absence, and the direct-message summary/body contract `renderInbound` prints.

The NATS server is a process of this checkout's own build, stopped on exit with the listener and the two fake clients.

Evidence remains in `packages/envoy/out/e2e/` after the run:

- `envelopes.jsonl` captures the raw NATS frames.

- `session-frames.jsonl` captures the envelopes the session's own consumer delivered to it.

- `rendered-ts.txt` is every frame rendered by `@legion/envoy-client`'s `renderInbound`, the one inbound renderer.

The OS chooses the NATS and listener ports, and the run builds each URL from the port its own process reports.
`E2E_NATS_PORT` or `E2E_PORT` sets a port explicitly: it is used as given, and one already in use fails the run with `address already in use`.
The session opens no port of its own.
CI runs the driver as the `envoy-e2e-local` job of `.github/workflows/ci.yaml`.

## Contract source of truth

The authoritative event contract lives in `packages/contracts/`.
Run `bun run gen:go` in that package before validating or releasing `packages/envoy`.

Each `legion-envoy-v` release publishes the changelog entries its own tag first shipped:
this package's fragment files (`.changes/unreleased/`) are rendered into that GitHub release's notes.
`CHANGELOG.md` is the history before that and stops where the fragments begin.
