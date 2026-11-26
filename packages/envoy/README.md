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
  It names the stored secret, and a profile of that secret releases the token under its own name too; a profile of another secret never does.
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
  Each entry names a stored secret, never a profile:
  a profile of a bound secret mints under its source's binding, whatever its own name.
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
The edge takes over no frame: `takeOverFrame` is off at an edge listener, whose grant publishes no kept role lane, so nothing moves a role message the role lane forwarded to the session's own agent subject before that registration to the role's kept lane.
The relay's check of each frame's topic (`edgeFrameVerdict`, `cmd/listener/edge_frames.go`) withholds such a frame (`TestAnEdgeWithholdsARoleMessageForwardedBeforeItsRegistration`), and with it two cases that need a writer other than the edge: a role message forwarded under a claim another writer granted the session after its edge registration, and a topic another writer stored in the session's interest record, taken in at a later refollow.
`ENVOY_EDGE_PROJECTS` alone turns edge mode on; no edge listener opens to an outside person from a build without that check.
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

- **Health**: every listener serves `GET /readyz`, which answers whether /v1 can serve; the edge's load balancer health check reads it, and the deployment's health-check policy for the edge names the path.
  It answers `503 {"status":"starting"}` until the /v1 gate opens, `503 {"status":"unavailable","error":"nats unavailable"}` while the bus does not answer after that, the same check that turns `/healthz` `503` (`busAnswers`), and `200 {"status":"serving"}` otherwise, again as soon as the bus answers. It takes no credential and reads no store (`readyzHandler`).
  So the load balancer's check drops a replica whose bus is cut, whose `/v1` answers bus-unavailable `503`s, and takes it back when the bus returns. The pod's readiness does not do that: GKE's standalone NEGs keep a NOT-READY endpoint, since ingress-gce's `EndpointsDataFromEndpointSlices` drops only terminating endpoints and records Ready without reading it (`pkg/neg/types/types.go:394-417` at `ba5195c`), and `toZoneNetworkEndpointMap` adds every address with no Ready check (`pkg/neg/syncers/utils.go:254`).
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
  Four cases still answer the balancer's retry `401`, and the status stays `503` in each, the one every route documents for a fault the client can ask again about:
  - a spent-proof store that did not answer while its write of the proof landed;
  - a release whose delete did not reach the bucket in its second (`proofReleaseBudget`), which a bus that did not answer the route makes likely, logged as `listener: a 503 could not give its request's DPoP proof back`;
  - a `502` or `504` the balancer wrote itself after the proof was recorded: a connection that ended before an answer (a killed pod, a handler that panicked, a shutdown that ended with the request in flight), or a request past the backend timeout;
  - a retry whose create meets the release's delete marker while its read of the key from the stream's leader gets no answer: the create cannot tell the marker from a live record, so it reports the key held (`nats.ErrKeyExists`, `bus`'s `streamMessageReads.Create`) and the retry reads as a replay, failing closed.

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
  The relay writes the mark on the entry before it acknowledges such an event, and gives the event back for redelivery when the write fails, does not answer within 2 seconds (`repositoryGapWriteTimeout`, which bounds the clear after a frame too), or the bucket holds no entry of the session; every other write of the entry, a registration's and the outage renewal's among them, keeps the mark the stored entry holds (`session.SessionRegistry.PutWithin`, `RenewWithin`).
  Each of those writes is conditional on the revision it read, reads the entry again from the bucket stream's leader after a write it lost, and ends with an error once it has lost 32 writes to other writers (`entryWriteLosses`).
  Each withheld event's mark is a new id, and the clear after a frame removes the mark that frame carried alone (`ClearRepositoryGap`), so while two relays of one session run at once, as while it moves, a mark either relay wrote since, for an event withheld later, stays on the entry for a later frame, also where the relay that clears adopted the mark from the entry (`noteRepositoryGap`).
  A relay run reads the mark its listener's copy of the entry carries before each frame it owes no mark for (`owedRepositoryGap`), so a mark another relay of the session writes while the run is live rides the first frame of the run after that copy shows it: the copy follows the bucket, so that can be a frame later than the write, and the other relay's own next frame carries it too. A mark a frame of the run carried is not taken again, so a copy that trails the clear sends the session no second notice.
  A gap mark with no reason is the stream's own retention gap, and a frame that marks both carries no reason.
  A refresh removes a session's interest in a repository that left its projects.
  Each run of failures is logged once when it starts and once when it ends, however long it lasts: a project's failed reads, which any good read of the project ends, a registration's included (the WARN's `withheld_from` is when its sessions' GitHub events start to be withheld, the moment its last answer is 60 seconds old, or the failed read when no answer stands; the INFO that ends the run counts `failed_reads`), the give-backs of a session whose entry takes no gap mark, which a mark that lands or a fresh answer that relays or drops a GitHub event of the session ends (`given_back`), a relay socket's naks of GitHub frames withheld for want of a fresh answer (`given_back`), and a refresh's failed lists of the interest records (`failed_passes`).
  A nak of a given-back event that fails is logged each time.
  A session whose interests in a repository that left cannot be removed is logged once; the removal that succeeds is logged as every removal is, and a run that ends otherwise, no interest of the session being outside its projects' repositories any more (the answer named the repository again, or the session dropped the topic itself), its interests being another listener's, or its record gone, is logged with `why`.

- **Publishes**: `POST /v1/messages/publish` takes only a role subject in the form that names the project, `notifications.role.<KEY>.<role>`, of one of the token's projects and for a role that works for that project;
  anything else gets `403 topic_not_published` (`edgePublishRefusal`).

- **Senders**: `POST /v1/messages/send` and `POST /v1/messages/publish` take only a message from the session itself: `source` `agent` or left out, and `source_session` the token's session.
  Any other gets `403 sender_not_session` (`edgeSenderRefusal`), so no edge session sends a message under another source, such as a Dispatch event or a person's message, or under no session.

- **Roles**: `POST /v1/roles/set` and `GET /v1/roles/{role}` take only a role that works for one of the token's projects, else `403 role_not_in_project` (`edgeRoleRefusal`).
  A role held by a session whose registering token listed none of the token's projects, live or lapsed, reads as one no claim holds (`edgeScope.holderConfirmed`): `GET /v1/roles/{role}` answers it `404 unclaimed`, and a publish to it and a claim's `409` name no holder, client instance, state, last registration, hand-back or claim revision (`edgeScope.namesHolderConfirmed`, `roleHeldElsewhere`). The holder is another project's session, whose id and activity the edge shows no one; a publish still reaches it.
  A lapsed holder whose session record is gone has no projects to show, so it reads as unclaimed even to a reader of its own project: the rule fails closed, and a soft claim still takes the role from a holder that stopped answering.
  A read of a holder's session that does not answer is the `503` with `Retry-After: 1` every listener gives it, in the listener's own words, which name no holder, and the read's error goes to the log (`edgeScope.unresolved`): a role read's and a role send's resolution of the holder, and a soft claim's read of the holder's liveness or of the predecessor it names.
  So is a leader read that confirms a session's stored record and does not answer (`edgeScope.confirmUnanswered`), wherever the edge reads one: the role read's holder, either `409`'s holder, a row of the session list, a Legion agent's session or its replacement, and the session a send, the full view or the registration view names. That `503`'s body names no session, since the read's own error names the session it read, which the read did not confirm and the request may not have named; the read's error goes to the log with that session.
  A role send whose forward the bus refuses, or whose window ends after its last forward met an earlier forward still in process (the `503` with `maybe_stored`), is answered for a holder the edge does not show in the listener's own words under the same status and code, since the forward's error can name the holder's agent subject (`roleSend.shownForwardError`). So is a repeat whose stored forward cannot be read back (the `503` with `Retry-After: 1`), whose read's error names the subject it read, also where that is the forward to the role's earlier holder that a sender's repeat after it took the role answers (`roleSend.deliveredHeld`).
  Each registration first releases the claims of another project's roles that the session holds, which another listener granted it or a token that listed more did, and the answer's `warnings` name them (`store.Registry.ReleaseRefusedRoleClaims`).
  It finds them in the session's stored interest row and in the listener's view of the role claims, which it may serve up to 2 minutes after it stopped following the role bucket, so a claim the view does not show yet is released at a later registration; while the view cannot be served, the release reads the role bucket itself.
  Each release frees the claim it read: a claim granted to the session again after that read stands, and the next registration's release frees it.
  A release that fails refuses the registration with the retryable `503`, and nothing of it is stored; the `503` names the claim record the bus answered and the release could not read or free, or says the bus did not answer. It renews only the session's claims of roles of its token's projects.
  A claimant made live again is handed the kept messages of its token's projects' roles alone.

- **Sends**: `POST /v1/messages/send` reaches only a session whose registering token listed one of the sender's projects, confirmed by its newest record on the session bucket's stream, read from the stream's leader with its headers (`edgeScope.showsConfirmed`): a record the server wrote, such as a RePublish of another stream at the session's key, reads as absent.
  Any other target, and one the session registry reads as gone, gets the `404` of a session no listener has heard of, and no wake is reported (`edgeScope.hidesConfirmed`); a confirmation read that did not answer is the bus-unavailable `503`.

- **Reads**: `GET /v1/sessions`, `GET /v1/registry/{session_id}` and the three `/v1/legion/agents` reads answer only the sessions and Legion agents of the token's projects, each session with only those projects' roles and topics (`edgeScope`);
  any other session gets the `404` of an unknown one.
  Each session shown is confirmed by its stored record as a send's target is.
  A Legion agent is shown only when its session is a session of its issue's project, a record bound to its key, so a gone agent of the token's own project is not shown either; its replacement is shown only when that is such a session too (`edgeScope.showsAgentConfirmed`).
  A session's own `GET /v1/interests/{session_id}` shows the topics of its token's projects alone.
  `GET /v1/interests/` and another session's `GET /v1/interests/{session_id}` get `403 session_not_in_project`, and a write of the Legion agent registry gets `403 legion_registry_not_written`.

- **Frames**: the relay and the relay socket each hand a session a frame only of a project its token lists, as the session's own message, or as a GitHub event the rule above passes (`edgeFrameVerdict`, `cmd/listener/edge_frames.go`).
  The relay holds a session to the token its replica admitted the session's registration or role claim on, which it keeps in memory, and the socket to the token it was opened with; neither reads a stored record, so no session entry, interest row, role claim or consumer filter widens what reaches the session.
  A message on the session's own agent subject that carries a role - a role-lane forward, whose topic names the lane, or a role-marked send, whose role mark names the role - passes only for the project its role's claim works for, judged by the role and not by the subject it is stored on, so a forged role claim cannot deliver another project's role message and a kept or taken copy of the session's own project's role still reaches it; the role is read as the session relay reads it (`readRoleFrame`), by the exact `topic` key, as the plugins read it, and the exact `role` key, so a field `encoding/json` refuses does not hide it.
  A role mark counts only where a role message's topic is (`roleFrameVerdict`, `roleMarkOnAgentSubject`): on a role lane, or on an agent subject in the old form or in the new form under one of the token's projects; the edge checks the topic's form alone, since it cannot tell which session held a role before. A role-marked frame whose topic is another project's event, a GitHub event, another project's or the wildcard project's agent subject, or no topic is judged by its topic as a frame with no role mark, since the plugin shows it by that topic; on the session's own agent subject a role mark of a project the token does not list is withheld on any topic, a lane forward of the token's project's lane among them.
  Any other message there, one whose role that read cannot tell among them, is judged by the topic its envelope names, a send or a dead letter to the session passing.
  The rule reads each field of a frame it judges by - the topic, the role mark, the `source`, a GitHub event's payload and the payload's `repo`, and a Dispatch payload's `event`, `delivery`, `project` and `issue_key` - by its exact key, past one leading byte order mark and at any depth, as the session's plugin reads the frame (`exactFrameField`, `exactFramePayload`, through `topLevelJSONFields`, the one reader of a frame's top-level fields, which the relay's `readRoleFrame` reads through too), and withholds a frame that names such a field under more than one key or under a key that differs from the field's name only in case (`"Topic"`, `"TOPIC"`), on which readers disagree, a role message among them. It also withholds a frame with a top-level `"__proto__"` key, however escaped: the plugin's envelope read copies that key onto the envelope as its prototype and reads the fields the frame lacks through it. A frame on an agent subject that `encoding/json` does not read (`json.Valid`) and that is no role message whose topic is a role lane or an agent subject is withheld, and a GitHub event whose envelope or payload it does not read names no repository, as before; on any other subject such a frame is judged by the topic the rule reads, or by its subject when it names none.
  A Dispatch frame (an envelope whose `source` is `dispatch`) is withheld on every branch unless its payload is an event the session's plugin shows, of a project the token lists (`dispatchFrameWithheld`, `dispatchEventProject`): Dispatch sends an issue's event to a session by the session's name - an owner route, a claim, a primary document's or an author's route, a follow, the previous claimant, a mention, a targeted message, a role forward - on the session's own agent subject or the role's lane, under a topic that names no project. The plugin's `renderInbound` reads every Dispatch payload with one function, `readDispatchPayload` (`packages/contracts`), and the edge judges a payload only when it has the shape that function reads an event by (`contracts.DispatchPayloadIsEvent`, over the JSON Schema `bun run gen:go` renders from the same zod schemas, `schemas/dispatch-frame.schema.json`); a targeted frame must hold `event` and `delivery` and nothing else, so no field beside the event the plugin shows decides the frame, and the plugin shows nothing of a Dispatch payload it reads no event of. The project is the event's `project`, or, where Dispatch leaves that empty (a mention's and a targeted message's event), the project its `issue_key` names; both named must agree. A person's message to the session in its own conversation, which names no project, passes on the session's own agent subject when it is that and nothing else (`dispatchDirectMessage`: a `message.created` event of no issue, document or project, whose message is aimed at this session, in this session's thread, with a delivery line of an attempt and a mode alone). Any other frame the edge cannot place in one project - no payload, a payload the plugin reads as no event, an event naming neither, two that disagree - is withheld under `reason="dispatch_project_unread"` and logged with its subject, never its content; a comment mention's event names its project on every send, the first and every re-send. A listener that is no edge one hands every Dispatch frame on as before.
  Every frame withheld this way is acknowledged undelivered and counted in `envoy_edge_github_withheld_total` under `reason="project_not_held"`, a frame with a repeated or case-variant key under `reason="ambiguous_key"`, a frame with a `"__proto__"` key under `reason="prototype_key"`, and a Dispatch frame the edge cannot place in a project under `reason="dispatch_project_unread"`.
  A relay replica that admitted no registration or role claim of the session since it started relays it nothing until the session registers or claims a role through it again, at its next heartbeat, and its messages wait in its consumer.
  A role claim through a replica renews the session's record there and so moves the session's relay to that replica, which admits the session on the claim's own token as a registration does, so its messages keep reaching it.
  A replica forgets a session's admission once the session's record has left its cache and the admission is older than one prune pass (30 s), so it keeps no list for a session that deregistered or lapsed.

Every refusal of a project rule above has the body `listenerwire.EdgeRefusal`, whose comment in `listenerwire/listenerwire.go` names each reason and says how a client reads a refused registration's `refused` list.

Every listener that checks broker tokens records the registering token's projects on the session's entry in `envoy_sessions` (`session.SessionEntry.Projects`).
The edge's send and read rules read that record, and no answer of the API carries it.

A session record and an interest row each name the key they are stored under (`session_id`), which every writer sets.
The server itself stores a record when a request names a key's subject as its reply subject, so a record that names another key, or names its key with no `machine_id`, no positive `updated_at` or (a session record) no `projects`, is no registration and reads as absent on every listener (`session.DecodeEntry`, the store's `boundInterest`).
The next write of such a record replaces it at the stream sequence it is really stored at.
A session record that names no key was written by an earlier build: every listener, the edge included, reads it as before for its sweeps, reapers and role routes, and the edge reads it as no project's (`edgeScope.shows`), so it lists such a session to no one and answers a send to it as to a missing session until the session's own listener writes the key.
A role claim that renews such a record writes the projects of the claim's own token, as a registration does, and never the record's: written back under its key, the record's projects would read as the session's own registration of them.

On every listener, a role claim's key is `store.RoleClaimKeyOf` of the name it was claimed under, and that function's comment says which key each name gets.
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

- **Frames**: text frames of JSON. The listener sends a frame of more than 4 KiB as one text message in fragments of 4 KiB (`relaySocketWriteFragment`), which a WebSocket client joins again.
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
  A close takes at most one second (`relaySocketCloseGrace`), also for a client that reads nothing: a close or a ping waits for the 4 KiB fragment being written, not for the whole data frame, so it can reach a client that reads slowly in the middle of a frame; a close frame the socket cannot write within the second, as for a client that reads too little of that fragment, is not sent, and the connection is dropped; and a write that fails drops the connection at once.
  A frame unanswered at a close, or held by a socket that closed `slow_consumer`, is sent again by the owning relay, so a reopen loses nothing.
  So is a frame the NATS client drops past its own pending limits ahead of the queue (the socket's 32 frames and 16 MiB): the socket stays open, and the frame comes again after the owning relay's ack wait.

- **Tests**: `cmd/listener/relay_socket_test.go` and `cmd/listener/relay_socket_caller_test.go` run the socket in one process with a stand-in broker, on the rig of `relay_socket_fixture_test.go`.
  `cmd/listener/relay_socket_live_test.go` (build tag `natslive`) runs two edge listeners under `natsgrant.Edge` on the hub's NATS image, with the real broker of `internal/cipher/ciphertest` and 30-second tokens; its binary checks every second (`-ldflags "-X main.relaySocketCheckBuild=1s"`).

- **Rollout**: the socket holds each frame to the projects of the token that opened it before it holds the frame for its client (`edgeFrameVerdict`, called from `forward` before `enqueue`), as the relay holds each frame it sends (**Frames** above).
  A session's relay subject can carry a frame of a project the token does not list: a role message of another project's role, another project's subject in the session's consumer filter, a GitHub event judged against the session's stored projects, a message any user that publishes `notifications.>` puts on the subject, or a frame of a project the person has since lost, from an earlier registration. The socket answers each such frame on its receipt subject and forwards none of them.

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
