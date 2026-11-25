# Dispatch HTTP server

Go binary serving the Dispatch dashboard and API.
People sign in through a Cognito sign-in pool or with Google directly, where the `DISPATCH_SIGNIN_*` settings configure one, and are named by lowercase email;
the web app's GitHub reads go to GitHub as the GitHub App.
Dispatch keeps application state in Postgres.
Its subcommands (`census`, `settings`, `routes`, `openapi`, `rebuild-refs`, `backfill-files` and others) are the `subcommands` table in `cmd/dispatch/subcommands.go`, and an argument it does not know is refused with every one named.

This file uses semantic line breaks: one sentence per line, and a long sentence may break after a clause.
A blank line separates list items.
Add a new item next to the item it belongs with, never at the end of a list unless it belongs there, so two pull requests that add items or change different sentences do not conflict.
`bun scripts/semantic-line-breaks.ts packages/envoy/cmd/dispatch/README.md` checks the layout, and `--write` fixes it.

## Required configuration

Every Dispatch setting the server and its subcommands read is a row of one table, `cmd/dispatch/settings.go`:
`main` reads each row once, every reader takes its value from that read, and a test fails on any other environment read in `cmd/dispatch` or `internal/dispatch`.
`envoy-dispatch settings` prints the table (name, `_FILE` form, default, whether it is required, and a one-line description) without a database or a listener, and the docs site's Dispatch configuration reference is generated from it:
those are where every setting is listed.
The items below add only what a setting's one line cannot say.

Each item below names a variable, then its purpose.

- `DATABASE_URL`

  Postgres connection string.
  Dispatch applies embedded migrations before serving.
  The pool size is fixed in code (`store.sharedPoolSize`), so a connection string carrying `pool_max_conns` is refused at startup; remove the parameter.

- `DISPATCH_AGENT_TOKEN`

  One value, or several separated by whitespace, so the shared token can rotate with an overlap:
  write the new value followed by the old one, move every consumer to the new one, then write the new value alone.
  The first is the current value; the API accepts every value, comparing a bearer with each in constant time.
  The document websocket takes no bearer at all: it is a person's cookie connection only.
  Whitespace at either end is ignored; startup refuses an empty entry (two whitespace characters in a row, a CR LF among them) or a repeated one, naming its position, never its value.
  A request that authenticates with a value after the first logs one WARN line, `dispatch: request authenticated with a previous shared agent token`, with `entry` (the value's position), `address` (the rightmost `X-Forwarded-For` entry, which is the load balancer's view of the caller, or the connection's own address without one), `user_agent` and `path`, at most once per address and User-Agent every 10 minutes, and never a token.
  Each field keeps its first 256 bytes, and the server remembers up to 10,000 callers; one past that is logged on each request.
  Callers that share an address and User-Agent share a line, so the log says whether a value on its way out is still in use, not who uses it.

- `DISPATCH_SIGNING_KEY`

  One key, or several separated by whitespace, the first the current key:
  it signs every new session cookie, seals every refresh token Dispatch stores, and, under a key derived from it for that purpose alone, signs the sign-in's state cookie;
  every key of the set verifies a cookie and opens a seal (Identity, below).
  It is parsed as `DISPATCH_AGENT_TOKEN` is:
  whitespace at either end is ignored, and startup refuses an empty or repeated entry, naming its position and never its value.
  A value of nothing but whitespace counts as unset.
  Unset, each process mints a key of its own in its own data directory (`auth.LoadOrCreateSigningKey`), which no other process holds, so a deployment of more than one process gives every process the same keys:
  a session minted on one process is presented to whichever process answers next, and a key that process does not hold signs the person out and leaves their stored refresh token unopenable.
  A process whose key set a live server process does not share both ways, a minted key included, refuses to start, exiting 1 before it serves (Running more than one process, below).
  A rotation in three steps signs nobody out (Identity, below);
  dropping the key a cookie or a seal was made under signs its person out and leaves that seal unopenable.

- `NATS_NKEY_SEED_FILE`, `NATS_NKEY_SEED`

  The NATS nkey user Dispatch connects as: a file holding the seed (trimmed; wins), or the seed.
  A set but unusable value refuses startup naming the variable and path; neither set connects without a credential.
  A NATS that does not answer at boot, or refuses the credential, refuses nothing:
  the server starts and serves, each of its NATS clients connects in the background and retries forever, and a refused credential is logged at ERROR at most once a minute (Running more than one process, below).

- `NATS_SERVICE_TOKEN_FILE`, `NATS_SERVICE_GOOGLE_AUDIENCE`

  The platform identity token Dispatch presents, as user `service`, to a NATS hub whose connections the auth service in the secrets broker admits: a projected Kubernetes ServiceAccount token in a file, read afresh at each connect, or this VM's Google identity token for that audience.
  The auth service answers with Dispatch's grant for the account its `BROKER_NATS_SERVICES` entry lists, and that user expires with the token, so the connection moves to a later token ten minutes before the one it presented lapses.
  Either set beside any other NATS credential variable refuses startup naming both.

- `DISPATCH_TEST_HOOKS`

  Set to `1` to mount test-only routes: `POST /api/v1/live/_test/disconnect` closes every open live socket with 1012 (restart), and `POST /api/v1/live/_test/rotate` closes every one with 4001 (`max age`);
  `POST /api/v1/agents/{session_id}/stream/_test/publish` (needs the `person` scope) feeds the in-memory agent relay, with `as=drop` ending agent views as `dropped`;
  `POST /api/v1/artifacts/_test/quiesce` closes every live document and waits for settlements;
  `POST /api/v1/artifacts/{id}/_test/outside-schema` writes the crafted malformed document e2e uses;
  `POST /api/v1/artifacts/{id}/_test/newer-content` appends an element no build knows, so the document holds newer content;
  and `POST /api/v1/writes/_test/slow` `{method, path, delay_ms}` makes every keyed run of that method and exact path wait `delay_ms` once its key is claimed.
  The first four act on every process serving the database, whichever one the call reached (Running more than one process, below), and the flag costs each process one more Postgres connection outside the shared pool, the one those hooks listen on.
  Leave unset in every real deployment.

- `DISPATCH_DEV_SIGNIN`

  Set to `1` to mount `GET /auth/_dev/signin?login=<email>&next=<path>`, which signs any person in by the email it names, with no sign-in pool, so a browser or test harness can be signed in to a local instance.
  Boot refuses it unless identity is `cookie`, no `DISPATCH_SIGNIN_*` setting is set, the listen address is a loopback IP literal, the dashboard origin (`DISPATCH_SERVER_URL` or `dispatch.serverUrl`) names `127.0.0.1`, `[::1]` or `localhost`, every `DATABASE_URL` host is loopback or a unix socket, `DISPATCH_SIGNING_KEY` is unset, `ENVOY_ALLOW_REMOTE_NATS=1` is not set while NATS is on, `DISPATCH_AGENT_SECRETS_URL`, when set, names a loopback host, `ENVOY_URL` names a loopback host, and a loaded GitHub App private key comes from `DISPATCH_APP_PEM_B64` with `DISPATCH_GITHUB_API_BASE` naming a loopback host, never from the `pem` in `app.json`, where a developer keeps the real App's key:
  a signed-in session can have the App probe and import any repository it is installed on, and read GitHub through the App's proxy.
  That key must be a throwaway, as `packages/dispatch/e2e/run-server.sh` generates one, since every App call hands a signed App JWT to whatever listens at that base.
  While it is on every request must carry the dashboard origin as its `Host` (else `421 HOST_MISMATCH`).
  The session cookie is signed with a key generated for that process alone, so it is worthless on any other server, unless `DISPATCH_DEV_SIGNIN_SHARED_KEY` (below) gives the processes of one local harness one key;
  what a signed-in session writes to the database is not.
  It can mint a `dsp_` personal agent token, and its sign-out advances the person's session generation, and every server on the same database honours those rows.
  Give a dev-sign-in server a database no other server uses:
  the loopback check makes that likely, not certain, since a loopback address can be a tunnel to another machine's database or a database a second local server also runs on.
  Without `DISPATCH_DEV_SIGNIN_SHARED_KEY` it generates its key at every start, so a dev-sign-in server refuses to start beside any other live server on its database, and any other server refuses to start beside it (Running more than one process, below);
  a restart after a kill, which leaves the killed process's row behind, waits up to about 20 seconds for that row to lapse.
  The processes given one shared value start beside each other.
  Any value other than `1` or unset is refused.

- `DISPATCH_DEV_SIGNIN_SHARED_KEY`

  Only with `DISPATCH_DEV_SIGNIN=1`, which every fence above still applies to; boot refuses it otherwise.
  Every process given the same value signs session cookies with one key, so a person signed in on one of them stays signed in on the others:
  the browser suite's two-process mode (`packages/dispatch/e2e/run-server.sh`) gives both of its servers one random value per run.
  The key is not the value but the base64url of HMAC-SHA256 keyed by it over the fixed label `dispatch dev sign-in` (`devSignInSharedSigningKey`), so a production `DISPATCH_SIGNING_KEY` placed here signs no cookie, and seals no refresh token, that the production server accepts, and that server's cookies sign nobody in here.
  `DISPATCH_SIGNING_KEY` stays refused beside it.
  Unset, each dev-sign-in process generates a key of its own.

- `DISPATCH_FILE_STORE_BUCKET`

  Files are stored under `files/sha256/<hash>`, one object however often a file is uploaded; a document's markdown never leaves Postgres.
  The value chooses the store (`files.Open`), and the server and `backfill-files` both open it there, so they never read one value as two stores:

  - `gs://<bucket>` is a Google Cloud Storage bucket, reached through the JSON API with Application Default Credentials (on GKE, the pod's Workload Identity principal;
    elsewhere `GOOGLE_APPLICATION_CREDENTIALS` or a gcloud login).
    The principal needs `storage.objects.create` and `storage.objects.get` on the bucket and nothing more (`roles/storage.objectCreator` and `roles/storage.objectViewer`):
    an upload reads the key's metadata first and writes nothing when the bucket already holds it, then writes in one request with `ifGenerationMatch=0`, carrying the body's CRC32C and MD5, which GCS checks, so no object is ever overwritten and none is deleted;
    a write that loses that precondition to a concurrent writer of the same key is an object already held.
    A value after `gs://` that is not a Cloud Storage bucket name (a path after the bucket among them) refuses the boot.

  - Any other value is an S3 bucket's name.
    The credential chain on Fargate is the task role, which needs `s3:GetObject` and `s3:PutObject` on `files/*` and `s3:ListBucket` on the bucket:
    without `ListBucket`, S3 answers a missing object 403, which Dispatch reads as a store failure.
    The SDK must also find a region (`AWS_REGION`, or the shared config); without one the boot is refused.

  An image whose Dispatch does not know `gs://` reads the value as an S3 bucket's name and fails every call, so a deployment sets a `gs://` value only on an image that carries it.
  An upload writes its object before its row and before any lock or connection is taken, within 30 seconds, so a store that refuses answers 502 `FILE_STORE_UNAVAILABLE` and writes no version.
  While a live server process has no bucket (one without the setting, or one of a build that writes a `server_processes` row with no `file_store`), an upload's row keeps its bytes as well as the bucket, so that process serves it.
  A process of a build that writes no `server_processes` row is not counted, so set this no sooner than the release after the first one that carries that rule ("Two builds at once", below).
  The version route serves a row still holding bytes from the row and any other from the bucket:
  it opens the object within 30 seconds, sends its headers and `Content-Length` only then, and streams the body at the client's pace, checking it against the row's hash as it goes;
  a bucket that does not answer is 502, a row whose object the bucket does not hold is 500 `FILE_MISSING` (a restore from the bucket's versioning, not a retry), a cleared row on a server with the setting unset is 503, and a body that fails or hashes wrong mid-stream is cut off rather than ended as if whole.
  `/healthz` reports `files`: null when unset, else whether the bucket answered within two seconds (S3:
  a `HeadBucket`; Cloud Storage: a metadata read of a key no upload writes, whose absence is a healthy answer, so `storage.objects.get` alone serves it, while a bucket that does not exist or a refused grant fails it);
  it never decides `ok`, since a bucket outage must not take the one task, and with it documents and asks, down.
  `envoy-dispatch backfill-files` moves the rows uploaded before the bucket, and `--restore` is the rollback ("Moving uploaded files to the bucket", below).

- `DISPATCH_EMBEDDER`, `DISPATCH_AZURE_EMBED_*`, `DISPATCH_AZURE_TENANT_ID`, `DISPATCH_AZURE_CLIENT_ID`, `DISPATCH_AZURE_FEDERATED_TOKEN_FILE`

  Where meaning search embeds, and with what credential.
  The processes may take different values during a release that changes them:
  one embedder identity is active at a time, and only the processes configured with it embed (Search, below).
  A model change on Azure takes a new deployment name, since each vector records an identity that names the deployment and not the model behind it.
  A model service or Entra that is down refuses no boot: search answers by keyword and says so, and the queue retries.

- `DISPATCH_ANSWERER`, `DISPATCH_AZURE_CHAT_*`, `DISPATCH_CHAT_*`, and the same `DISPATCH_AZURE_TENANT_ID`, `DISPATCH_AZURE_CLIENT_ID` and `DISPATCH_AZURE_FEDERATED_TOKEN_FILE`

  Where the server generates text: `azure` is a chat deployment of an Azure AI Foundry resource, reached with the shared Azure sign-in;
  `endpoint` is any OpenAI-shaped chat completions service, reached with the bearer token `DISPATCH_CHAT_TOKEN_FILE` names, which is read on every call.
  Unset, no model is configured, and the server starts and says so.
  The sign-in is the embedder's too, read by whichever of the two is `azure`, so a deployment may embed on Bedrock and answer on Azure; it is refused only where neither is.
  `DISPATCH_AZURE_CHAT_SCOPE` is configuration because one Azure AI Foundry resource serves two inference families:
  the models inference endpoint (`/models`, an `api-version`, scope `https://cognitiveservices.azure.com/.default`) and the Azure OpenAI endpoint (`/openai/v1`, no `api-version`, scope `https://ai.azure.com/.default`), and which of the two a deployment serves is named by that deployment.

- `DISPATCH_DECISION_MODEL`, `DISPATCH_DECISION_MODEL_ENDPOINT`, `DISPATCH_DECISION_MODEL_TOKEN_FILE`

  The System One model the command bar's one decision call is put to before it answers (which action the words ask for, whether they point at the page's item, chart or prose), the endpoint's base URL, which `/v1/systemone` is appended to and which must be an absolute https URL with a host and no query or fragment, and the file holding the bearer token every call presents.
  Each defaults to empty; the endpoint and the token file are set together or not at all, and boot refuses one without the other, naming the one missing; the model is optional, since a deployment renders the endpoint and the file before it names a model.
  The model must be one the committed measurement holds (`packages/envoy/going/decision_models.json`, which the bake-off `cmd/going-bakeoff` writes, and which marks the model its rule chose): a name it does not hold is no decision model, so the server starts, logs one line saying why and decides nothing, as it does with no model or no endpoint.
  The boot line of a measured model says whether the record chose it.
  The token file is read on every call and never at boot, so a rotation needs no restart; a file that is absent or empty at a call fails that call as the decision model that cannot be reached, with no request made, and the capability route reports it unreachable.

- `DISPATCH_MAX_LIVE_ROOMS`, `DISPATCH_MAX_DOC_CONNECTIONS`

  Each process's own caps on the document rooms it holds in memory and the browser connections to them, so two processes of one deployment hold up to twice each;
  a document socket past either is refused `503` before it upgrades, never `401`.
  A socket takes its place under both caps before it loads anything, in the same step that checks each, so sockets that open at once never pass a cap:
  the room cap counts the rooms held and the rooms sockets being admitted reserved, each room once, and the connection cap the browsers connected and the sockets being admitted.
  The caps are counts kept as rooms load and unload and as sockets come and go, so admission walks no rooms.
  Boot logs both with the cold reads' cache budget, `dispatch: document rooms' limits` with `max_live_rooms`, `max_doc_connections` and `read_budget_bytes`;
  that budget is no setting but one sixteenth of the Go memory limit, at least 64 MiB and at most 1 GiB, and 256 MiB when no limit is set.
  `/debug/vars` (Profiling, below) shows how near each cap a running process is.

A project's default repository belongs to the project, and no setting holds it:
`PUT /api/v1/projects/{key}/default-repository` sets or clears it, and so does Settings → Projects (Routes, below).
An issue imported from GitHub (`POST /api/v1/issues` with `external`) names its project in `project`, as every other create does.
No setting seeds a default repository or chooses the project of an import.
A boot that still sets one of the two settings that did is refused, naming what replaced it (`removedSettings` in `cmd/dispatch/settings.go`).

Every project has an owner, and a boot over a project with none refuses to serve until `DISPATCH_PROJECT_OWNER_FALLBACK` names a full member to own it, or an operator gives it one (Giving a project to another owner, below).

Where browser sign-in is configured, `DISPATCH_SERVER_URL` IS the sign-in callback origin:
it must equal the URL humans type into their browser, and the sign-in pool's app client must list `<DISPATCH_SERVER_URL>/auth/callback` as a callback URL.
With no `DISPATCH_SIGNIN_*` setting nobody signs in through a browser, and the variable is the origin Dispatch builds its own links from and nothing more.

`DISPATCH_ALLOWED_LOGINS`, `DISPATCH_APP_CLIENT_SECRET` and `DISPATCH_ASSET_STORE_BUCKET` are removed:
a boot that still sets any of them is refused, naming what replaced it.

Agents normally authenticate with a personal `dsp_` token minted in Settings, sent as `Authorization: Bearer <token>`.
`DISPATCH_AGENT_TOKEN` remains the shared devbox fallback and does not attribute callers to an owner.

With `DISPATCH_OIDC_ISSUER` and `DISPATCH_OIDC_AUDIENCE` set, a Kubernetes pod's projected service-account token is a third bearer credential:
the server reads the issuer's discovery document in the background from boot, retrying after one second and doubling to thirty while it fails (each failure logged at ERROR at most once a minute), and an issuer that cannot be reached refuses nothing;
it verifies a JWT-shaped bearer against the issuer's published keys, the configured audience, and the token's expiry.
Until the document has been read, a JWT-shaped bearer is `503 OIDC_ISSUER_UNAVAILABLE`, never a 401, and every other credential is served.
Half the pair refuses to boot, naming the missing variable; neither leaves bearer handling exactly as it is without them.
A verified caller acts as the `session` it names in the request body, and every actor it writes carries `service`, the token's verified subject (`system:serviceaccount:<namespace>:<name>`)
— set from the token, never from the body.
A JWT the verifier rejects is `401 OIDC_TOKEN_INVALID` naming the reason class; it is never retried as a personal token.

With `DISPATCH_AGENT_SECRETS_URL` set, an access token the secrets broker issued is a fifth credential, and the only one that proves a session of its own.
It is presented under RFC 9449's DPoP scheme
— `Authorization: DPoP <token>` with a `DPoP:` proof header signed by the key the token is bound to
— because that RFC refuses a DPoP-bound token presented under any other scheme;
the server decides a request by its scheme before it reads any token, so such a presentation is never retried as a bearer, and a bearer is never sent to this check.
The server asks the broker what the token is (RFC 7662 introspection on `/v1/oauth/introspect`, presenting the same projected ServiceAccount token `DISPATCH_AGENT_SECRETS_ACCOUNT_TOKEN_FILE` names as the approval relays do, one affirmative answer reused for five seconds from the broker's answer) and checks the proof against the `jkt` that answer carries.
A revoked token therefore stops resolving within five seconds of the introspection that last called it live, not of the revocation:
an introspection already in flight when the token is revoked is remembered from the moment it completes, and it may take up to five seconds to complete.
The proof's `htu` must name one of the origins Dispatch is reached at, plus the request's path:
`DISPATCH_SERVER_URL`'s origin, or one `DISPATCH_PROOF_ORIGINS` adds, a comma-separated list of origins such as the in-cluster address pods and daemons call, since a client signs the URL it calls.
Unset, `DISPATCH_SERVER_URL`'s origin is the only one.
An entry that is not an absolute http(s) URL with no path, query, fragment or user refuses the start, naming it, and an entry equal to `DISPATCH_SERVER_URL`'s origin or to another entry is taken once.
A proof naming an origin neither setting names is refused as any wrong target is, and a proof is spent once whichever origin it names.
That list is the one setting of its own this credential takes.
A verified caller acts as the session the token names, for the person it names, or, where the broker names none, for the service it names (the fleet of the machine holding it, or the service a read entry names), and `actor` in the body does not decide that session:
absent is accepted, one naming the same session is accepted and its `origin` kept, and one naming any other writer is `403 ACTOR_SESSION_MISMATCH` naming the session the token carries.
A token scoped to one project carries that project on every actor it writes, set from the token and never from the body;
a token scoped to its person names no single project and carries none.
A read-only token names no project, unless it narrows a token scoped to one project, and then it names that project as the one it reads as and writes none.
Every actor such a write records carries `proven: true`, as a browser's person does, while a session a body names under any other credential carries no `proven`:
a reader that trusts only a session the server saw proved reads that mark, and a document version's author keeps it only when every write merged into that author carried it.
Each refusal is its own code and none repeats the token:
`401 BROKER_TOKEN_NOT_ACTIVE` (the broker does not hold the token live), `401 BROKER_TOKEN_PROOF_INVALID` (the proof did not meet RFC 9449 Section 4.3, a proof presented twice included
— a proof is single use by its `jti` and target URI), `401 BROKER_TOKEN_KEY_MISMATCH` (a sound proof signed by a key that is not the token's), `401 BROKER_TOKEN_NOT_A_SESSION` (the token is live but is no session's credential:
it names no session, it names neither a person nor a service, it was issued to a client other than the exchange a session's token comes from, which a machine login's own access token is, or its project scope and its OAuth scope are a shape no broker mints (the scope `read` or a cut beside the project scope `named` or `person`, or the project scope `none` or a narrowed one in any shape but a read-only token's)
— Dispatch records a write under the session its credential proved, so it acts on none of these rather than letting a body name a session for a credential that names none), `429 BROKER_TOKEN_RATE_LIMITED` (this source address's introspection bucket is empty;
nothing was asked of the broker) and `503 BROKER_UNAVAILABLE` (the broker answered nothing, so the token was never judged
— a 503 for the reason `OIDC_ISSUER_UNAVAILABLE` is one).
The live socket (`GET /ws/live`) re-resolves its caller every heartbeat from the request that opened it, and for this credential that re-resolution is the broker's introspection answer alone:
the upgrade's proof authenticated it and cannot be presented a second time.
Without `DISPATCH_AGENT_SECRETS_URL` there is no such credential, and a `DPoP` presentation is the `401 UNAUTHORIZED` `invalid bearer token` any Authorization header that is not a bearer has always been.

No broker-issued token holds the OAuth scope `approve`, whatever its project scope: one acting for its person holds `person` alone, and every other holds none (`brokerTokenActor`).
Each such token is made from a file on the machine that holds it, a machine login or a person's sign-in, so whoever copies that file holds it, and `approve` settles what only the person settles.
So every route whose `scopes` name `approve` (`GET /api/v1`; `x-oauth-scopes` in `dispatch-openapi.json`) refuses every broker-issued token at its scope gate (`gateRouteScopes`), before the handler runs and before anything is relayed to the broker:
`403 OAUTH_SCOPE_REQUIRED` naming `approve` for a token acting for its person, naming `person` for one that holds no scope, and `403 READ_ONLY_TOKEN` for a read-only token (below).
The answer to an approval request (`POST /api/v1/asks/{id}/answer`) needs only `person` at that gate, because the ask's kind is known only once its row is read, so a token that holds no scope and a read-only token are refused there as above;
a token acting for its person passes the gate and is refused inside the answer's transaction (`answerAsk`), once the ask's kind is read and before anything is written, with the same `403 OAUTH_SCOPE_REQUIRED` naming `approve`, and the request stays open.
The person makes those calls from their browser session, or with a personal token they gave `approve`.
A decision's Keep (`POST /api/v1/artifacts/{id}/blocks/{block_id}/keep`) needs `approve` because it records that the person agreed with the decision and takes it out of what waits on their review, so a personal token holding `person` alone is refused it too.

A read-only token is any token that holds `read`, and it may only read.
The broker mints it from a read entry (`BROKER_EXCHANGE_READ_ACCOUNTS`): that token holds the OAuth scope `read` alone and the project scope `none`, names a service and a fixed session, and names no person and no project.
The broker also narrows any trade that asks for `read`: that token keeps the session, the person or the service, and the projects the same trade would have named, under the narrowed project scope `read-named`, `read-person` or `read-none`, and it may carry a cut (`as_of`), the past time it reads at.
`brokerTokenVerifier` builds the verifier with `ciphertoken.Options.ServesReadOnly` set, so Dispatch accepts such a token and marks its credential read-only (`model.Credential.ReadOnly`), with its cut (`model.Credential.Cut`).
A `read-person` token that names its person holds the `person` scope alone, never `approve`, and every other read-only broker token holds no OAuth scope.
A personal token minted with `read` is read-only too (`/api/v1/me/agent-tokens`, below).
Dispatch serves a read-only token on the reads of its read class and on no other route.
`readClasses` (`internal/dispatch/api/read_classes.go`) gives every GET row of the route table one class: a read, which writes nothing but the record of the token's use and caches Dispatch rebuilds from what it holds, or a GET that writes.
`register` refuses to mount a table with a GET row that has no class.
Every other route of the table, and `GET /ws/live`, answers a read-only token `403 READ_ONLY_TOKEN` (`contracts.DispatchReadOnlyToken`, generated from `packages/contracts`) with no `WWW-Authenticate` challenge (`refuseReadOnlyToken`).
Every read but `GET /api/v1/whoami` answers a read-only token that carries a cut `403 AS_OF_TOKEN_NOT_SERVED` (`contracts.DispatchAsOfTokenNotServed`), naming the cut: this build answers no read at a past time, and it does not answer one with the present.
Both refusals come before the route's scopes, the sign-in check, the `Idempotency-Key` and the handler, so a refused request claims no key and writes nothing but the record of the token's use: a broker token's spent DPoP proof, which the token's verification records before the refusal, or a personal token's last use.
`GET /api/v1/issues/{key}/results` is a read: a read-only token's read of it asks GitHub exactly what a writer's read made at the same moment asks, answers what that read answers on the rows stored then, and stores no pull request head, no branch commit and no event.
It keeps no answer after its request: it reads the writer's claims and settles of a commit's pull requests, the unseen holds and the default-branch cache without writing any of them, and each of its reads of the stored heads runs the writer's own statement over the stored rows with that request's own answers laid on.
What it changes in the process is what a writer's read changes there the same way: the GitHub client's installation and token caches fill (`githubapp.Client.RepositoryToken`), a lapsed unseen hold is deleted when it is read (`delivery.Branches.unseenHeld`), and a rate limit GitHub answers is recorded in `github_rate_limits` for every process (`githubapp.Client.request`).
So every read at the present time asks GitHub what a writer's read at that moment asks.
For each result commit that no stored head and no writer's claim or settle covers, it makes one pulls read (`GET /repos/{owner}/{repo}/commits/{sha}/pulls`), and for each pull request that read names whose title, body, branch and links name no issue Dispatch holds, it reads the commit messages, one to five pages of 100 (`GET /repos/{owner}/{repo}/pulls/{number}/commits`, `delivery.fetchCommitMessagesWithToken`).
It makes one compare read for each step of an `after_merge` or `default_branch` walk that no fresh stored branch row and no unseen hold answers, the check-runs read of a check CI proves at the issue's newest pull request head (one page of 100, and the next while a page comes full) on every read, and the repository's default-branch read wherever the process's cache entry is older than five minutes.
A read by a token that carries a cut is refused `403 AS_OF_TOKEN_NOT_SERVED` before the handler and reads nothing from GitHub, and a read that names a commit with `?at=` asks GitHub only for the check runs of a check CI proves.
Those reads spend the GitHub App installation's rate limit, the one the writers' reads and both reconciles spend; GitHub gives an installation at least 5,000 REST requests an hour (its `X-RateLimit-Limit`, which Dispatch does not read).
A poll every N seconds of an issue whose read makes R of those reads costs R × 3600 / N of them an hour: an issue with one uncovered result commit whose pull request's title names it (`TestAReadOnlyResultsReadAnswersAsAWriterInEveryState`, rows 2 and 3) makes two, the pulls read and the default-branch read, so a poll every 10 seconds spends 720 an hour, and a read whose issue has 300 uncovered result commits makes at least 300 pulls reads, one for each, so 17 such reads spend 5,000.
When GitHub answers a request as rate-limited, the client records the limit until `Retry-After`, else `X-RateLimit-Reset` where no request is left, else a minute, and at most an hour (`githubapp.Client.request`), and until then it sends no request on that installation's token for any caller in any process (`githubapp.Client.send`; the App's own installation lookups and token mints are not held).
While it holds, a writer's read-through finds the repository unreadable, an answer the process then keeps for five minutes, or, where the process's answer for the repository is fresh, is refused after its claim, which then holds the commit off for five minutes (`delivery.CommitPullReadStaleAfter`); a check CI proves answers `unread` (`github_unreadable`); the CI reconcile's pass ends at the limit; and each step of the delivery reconcile is refused.
No cap bounds a read-only caller's reads.
The undo is to end the poll, or to end the read-only token, by its kind:

- A personal token: `DELETE /api/v1/me/agent-tokens/{id}`, which answers `401 TOKEN_REVOKED` from the next request.

- One the broker narrowed from a person's sign-in: the sign-in's revoke (`POST /api/v1/signins/{id}/revoke`).

- One narrowed from a dev box's machine login: that login's revoke (`POST /api/v1/machine-logins/{id}/revoke`), which ends its launcher credential and with it every read token traded from the login's access token (`oauthstore.NarrowedLive`).

- A read entry's token, or one narrowed from a pod's, a GitHub workflow's or a Google machine's identity token, which no person's revoke ends: taking its entry out of the broker's settings, which the broker reads when it starts, so a restart refuses its next trade, and waiting for the token to expire.
  For a read entry's token, take its entry out of `BROKER_EXCHANGE_READ_ACCOUNTS`.
  For one narrowed from a pod's, take the pod's entry out of `BROKER_EXCHANGE_ACCOUNTS`, or out of `BROKER_EXCHANGE_READ_ACCOUNTS` for a read entry's pod.
  For one narrowed from a GitHub workflow's, take the workflow's entry out of `BROKER_EXCHANGE_WORKFLOWS`.
  For one narrowed from a Google machine's, take the machine's account out of `BROKER_VM_SERVICES`, since the broker mints a token for every account that setting lists, whether or not `BROKER_VM_EXCHANGE_ACCOUNTS` names it.
  Take that account out of `BROKER_VM_EXCHANGE_ACCOUNTS`, `BROKER_VM_PROJECTS` and `BROKER_VM_INSTANCES` too, wherever they name it: the broker does not start when one of them names an account `BROKER_VM_SERVICES` does not list.
  Where it is the last account `BROKER_VM_SERVICES` lists, take out `BROKER_SIGNIN_DEVICE_AUDIENCE` too, and `BROKER_GOOGLE_OIDC_AUDIENCE` where nothing else takes a Google identity token, or the broker does not start (`config.Load`).
  A service that no other listed account names is then no owner whose secrets the broker serves (`serviceNames` in `cmd/cipherd`).
  Taking the account out also ends, at their next call, the launcher credential of every machine of that account and of every person's sign-in on one of those boxes (`enroll.tokenLoginListed`).
  The broker's access tokens live at most `BROKER_ACCESS_TOKEN_SECONDS` (900 by default), and a narrowed token no longer than the token it was traded from.

A rate limit GitHub already answered still ends at its own time, whichever undo is taken.

Dispatch stops serving a read-only broker token at most ten seconds after the broker ends it: Dispatch reuses a live answer for `ciphertoken.AffirmativeCacheTTL`, five seconds, from the introspection that gave it, and an introspection already in flight when the token ends may take `introspectTimeout`, five seconds, to finish.

The other mounts outside the table read no broker token: `/ws/doc/{room}` answers a read-only broker token `401`, and `/auth/*`, `/api/github/rest/*`, `/healthz`, `/livez` and `/metrics` take no broker token at all.
`/healthz`, `/livez` and `/metrics` take no credential (`dispatchHandler`), so they answer a request that carries a read-only token as they answer any other caller, and none of the five makes a write for it.
The document socket's heartbeat re-checks the browser credential it was admitted with, the sign-in cookie or, under `DISPATCH_IDENTITY=iap`, the IAP assertion its upgrade carried (Identity, above), and nothing the request carries beside it (`api.signInStillResolves`), so a read-only token sent beside a cookie cannot keep that socket open once the cookie ends.
A service whose verifier does not set the switch, the Envoy listener among them, answers a read-only broker token `401`, and the NATS auth service, which reads the broker's answer through `ciphertoken.SessionCaller` with the switch off, refuses a connection that presents one as `not_a_session`.

A credential that names one project may write that project and nothing else.
Every mutating row of the route table declares what its write belongs to (`internal/dispatch/api/routes_table.go`, `writeClasses`;
a `GET` row declares nothing), in four classes:
*a project's* — the issue-key rows, the project-key rows, the artifact-id rows, the ask-id rows, the comment-id rows, and the message rows that carry an issue;
*a project the body names* — the issue create alone, whose project its address does not name, so `createIssue` makes the same check itself once that project is settled;
*the calling session's* — an agent conversation's own artifact and messages, the two routes a session answers its own delivery on (or, for a delivery sent for a `role:` route, one sent for a role the listener's role lookup names it the live holder of, which a session that took the role from the one the delivery went to answers it by), and the caller-private `me` rows;
and *the deployment's* — the project create, a project's default repository, the delivery settings and session groups, the broadcast, and the credential and machine-login decisions.
`Register` wraps every mutating route in three checks (`internal/dispatch/api/project_scope.go`), which run after the caller is resolved and before the handler, so no route is left to check any of them for itself.

The project check resolves the project the addressed object belongs to
— an issue key, a project key, an artifact id, an ask id, a comment id or a message id, each by one read
— and lets through only a target that resolves to the caller's own project.
It fails closed, so a target that resolves to another project, to no project at all (an object belonging to none), or to nothing at all (no such row, or an id Postgres will not read as a uuid) is `403 PROJECT_SCOPE`, naming the project the credential covers and the project the write belongs to where one resolved, and nothing of the credential.
The two refusals still read differently — a target of another project names that project, one that resolved to nothing says the write resolves to no project of that credential's
— so failing closed does not close an existence oracle and does not claim to.
Nothing is read out that way: no GET route is project-scoped, so every answer this check can colour is a refusal of the caller's own write.
Each resolver sends the raw path value to Postgres, as the handler behind it does, so the check reads a path exactly as the handler then addresses its row by it
— Postgres's uuid parser takes a hyphen after any group of four digits, and a check that parsed ids in Go admitted those spellings unjudged.
A deployment-wide write is `403 PROJECT_SCOPE_DEPLOYMENT`, naming the credential's project and the route:
a credential that may write one project may not create another or change a setting that is not any project's.
`POST /api/v1/issues` takes its project from the body rather than from its address, so its handler makes the same check, with the same refusal, the moment that project is settled
— before the ref is resolved to an issue already imported, and before anything is written.

The session check holds the rows classed *the calling session's* whose path names a session
— the `agents/{session_id}` writes and `PUT /api/v1/me/agents/{session_id}/state` — to the session the caller's credential proved:
addressing another session's conversation is `403 SESSION_SCOPE`, naming the session the credential acts for, the route and the session addressed.
Only a broker-issued token proves a session;
a cookie, a personal token, a Kubernetes service-account token and the shared token prove none and are untouched, so a person still uploads into the conversation they name.

The sign-in check refuses every write by a credential that names a service and no person under the project scope `person`
— an access token the broker issued to a machine nobody has signed in on
— `403 SIGN_IN_REQUIRED`, naming that service and `agent-secrets login`, the command that signs a person in on the machine.
It stands in front of every mutating route whatever its class, and outermost of the three, so such a caller is told it may make no write at all rather than which project the write it tried belongs to.
Its reads are untouched: the machine reads everything its session may read, and what it may not do is write, because Dispatch records a write under the person who made it and this credential names none.
A broker token that does name a person, and every other credential, reaches the handler as before.
A broker token that names a service and no person under the project scope `named` reaches the handler too, where before this release it was refused `401 BROKER_TOKEN_NOT_A_SESSION`: the broker mints it only from a `BROKER_EXCHANGE_ACCOUNTS` entry that names no person and no project (`<service account>==`), its service is that account, and each of its writes is recorded as the token's session acting for the account and for no person.
Every verifier admits that shape only when its service is a Kubernetes account (`system:serviceaccount:<namespace>:<name>`).
Such a token holds no OAuth scope, so a route that needs `person` or `approve` refuses it `403 OAUTH_SCOPE_REQUIRED`.
`GET /api/v1/whoami` answers `sign_in_required` from the same decision this check makes: true for a machine nobody has signed in on whose token may write, and false for every other credential, this account token and every read-only token among them.
A read-only token may name a service and no person too, an account entry's narrowed to reading among them, but it never reaches this check: the read-only refusal above answers it `403 READ_ONLY_TOKEN` first, because signing a person in is no remedy for it.

The project check is a no-op for a credential that carries no project
— a browser cookie, a personal agent token, a Kubernetes service-account token, the shared token, an access token the broker scoped to a person rather than to one project, and a `named` token of an account entry that names no project.
The session check is a no-op only for a credential that proves no session
— a browser cookie, a personal agent token, a Kubernetes service-account token and the shared token; every broker-issued token proves its session, and the session check holds it to that session's own rows.
That is the rule and not a transitional measure: every caller that exists today writes exactly what it wrote before.
A read-only token may carry a project, as a token narrowed from one scoped to a project does, but none of the three checks reads it, because the read-only refusal answers it first on every mutating route.

GitHub App credentials come either from the `DISPATCH_APP_*` variables or from `~/.local/share/dispatch/app.json`; the variables take precedence.
Two of them carry rules beyond their row:

Each item below names a variable, then its purpose.

- `DISPATCH_APP_PEM_B64`

  GitHub App private key, as a PEM or the base64 of a PEM: a value holding `-----BEGIN` is the PEM itself, and any other is base64-decoded.
  A key that does not parse stops boot with an error naming the variable, never the key.
  With `DISPATCH_DEV_SIGNIN=1` this is the only source a key may come from (a `pem` in `app.json` is refused), it must be a throwaway, and `DISPATCH_GITHUB_API_BASE` must name a loopback host.

- `DISPATCH_GITHUB_API_BASE`

  GitHub API origin override for App calls (tests and e2e point it at a fake); empty means `https://api.github.com`.
  With `DISPATCH_DEV_SIGNIN=1` and an App private key loaded, it must name `127.0.0.1`, `[::1]` or `localhost`.
  That checks the host, not what listens there: every App call hands a signed App JWT to whatever owns the port, so the key must be a throwaway.

When no GitHub App credentials are configured, the server still starts, but the GitHub proxy answers `503 GITHUB_TOKEN_UNAVAILABLE`, and saving a project's architecture source answers `409 SOURCE_ACCESS` naming the missing key.

For a Google Cloud deployment, use `envoy-infra:gcp:Dispatch` from `packages/envoy/infra`.
It supplies the private Cloud SQL connection, the tailnet entry point, and the pod environment this binary needs;
see that component's README for its inputs, the IAM grant it writes and the one the consumer writes.

## Webhook redelivery

GitHub does not redeliver a webhook delivery that failed.
When NATS is configured and the App's private key (`DISPATCH_APP_PEM_B64`) is present, Dispatch redelivers them itself, following GitHub's documented approach.
At start and every two minutes after (Background loops, below:
each sweep at most two minutes, and more than 0.9 of that, after the last) it lists the App webhook's attempts (`GET /app/hook/deliveries`) and judges each delivery by its GUID, as GitHub's own redelivery script does:
a GUID with an `OK` attempt is delivered, whatever its failures, and it asks GitHub to redeliver each other GUID with an attempt whose status is not `OK` (`POST /app/hook/deliveries/{id}/attempts`).
Both endpoints accept only an App JWT, which is why Dispatch runs this.
The webhook the App delivers to is the Envoy listener's `/webhook/github`.
A redelivery carries the original `X-GitHub-Delivery`, and the listener publishes GitHub envelopes under a JetStream MsgId of it.
So redelivering a delivery that did reach the stream adds nothing to the stream.
`DISPATCH_TEST_REDELIVERY_INTERVAL` replaces the sweep's two minutes for an end-to-end test, and a deployment never sets it.

- Every failed delivery is asked for again, whatever status code its failed attempts carry:
  a 5xx, an attempt GitHub could not complete, and a 4xx alike (the listener's 401 for a signature it cannot check, 400 for a body it cannot use, 422 for a publish NATS refused), since what the listener cannot take now it may take later.
  The first request goes on the next sweep.
  While no attempt is `OK`, the next request waits a backoff of 2, 4, 8, 16 and 32 minutes after the one before, then 60 minutes each time: one, two, four, eight and sixteen two-minute intervals, then thirty.
  It is due nine tenths of that backoff after the sweep that made the request before it (`duty.LeasePeriod`), since a sweep comes more than nine tenths of two minutes after the one before: so the sweep that many intervals on makes it at the latest, and the first retry is the next sweep's.
  A redelivery request GitHub refuses (for example 422) delivers nothing and is asked again on the same schedule, as is an accepted request whose redelivery never appears in GitHub's log.

- The sweep stops asking 72 hours after the first failed attempt it saw for the delivery, since GitHub keeps a delivery 72 hours, or when GitHub answers the redelivery request 404 (it no longer has the delivery).
  It logs `level=ERROR msg="webhook delivery expired undelivered"` once per delivery, with `requests`, `first_delivered_at` and `reason` (`GitHub keeps a delivery 72 hours` or `GitHub no longer has the delivery`).

- A rate-limited answer (403 or 429, as GitHub's REST rate-limit documentation describes one) stops the sweep and counts nothing against the delivery.
  No sweep sends GitHub anything until the time GitHub gave (`Retry-After`, or `x-ratelimit-reset` when no requests remain), and at least one minute, doubling while limits follow one another, up to 64 minutes.
  Both live in `github_rate_limits` (migration `0160`), judged by the database's clock:
  GitHub's own wait under the App's scope, `app`, which every process's App calls obey (Running more than one process, below), and the doubling under the sweep's own, `redelivery`, so a limit met on redelivery requests never holds token mints and installation lookups past GitHub's wait.
  A wait longer than an hour is read as an hour, never as no wait, and a negative `Retry-After` as none (`internal/serverwait`):
  an hour is GitHub's own longest wait, since it counts its primary rate limits per hour, and a longer one would hold every App call of every process for as long, while one cut to an hour costs at most one more request that GitHub answers with a fresh wait.
  Each sweep reads both again right before every request it sends, so one limit stops every sweeper at its next request;
  a request already sent when another sweeper records a limit is not recalled.
  A sweep that finishes clear of a passed backoff deletes it, unless another sweep struck it meanwhile.
  It logs `level=WARN msg="webhook redelivery rate-limited by GitHub"`.
  Redelivery requests go a second apart, as GitHub asks of a large number of POSTs.

- A delivery GitHub has recorded an `OK` attempt for is never asked for again, even when a request for it was recorded as refused (an accepted request whose answer was lost).
  When the sweep had made at least one request for it, it logs `level=INFO msg="webhook delivery recovered"` once, with `requests`, the number of requests the delivery took.

- A process killed between claiming a delivery and recording GitHub's answer (a SIGKILL, an OOM kill, a lost node) leaves a claim that stands for one minute, three GitHub request timeouts.
  Until then a sweep that reads the delivery counts it `claimed_elsewhere` (`claimed-elsewhere` in the command's table, `--dry-run` included):
  the running sweep while the listing still carries its failed attempt, since the index holds an open delivery back until its claim can be void, and the operator command whether the delivery is listed or not.
  After the claim ends the delivery is asked for again (`--dry-run` reports `would-redeliver`), even once its failed attempt is older than the sweep's look-back.
  If GitHub had already taken the dead process's request, its log shows the redelivery by then:
  an `OK` attempt closes the delivery with no second request, and a failed one is redelivered like any other.
  A claim whose answer could not be written logs `level=ERROR msg="webhook redelivery
  outcome not recorded; the delivery is decided again once its claim expires"` with `claim_expires_at`.

- Every sweep logs an INFO `msg="webhook redelivery sweep"` line:
  `failed_attempts` (the failed attempts listed), `deliveries` (the deliveries the sweep decided:
  the GUIDs of the listed failed attempts, less any it left out (below), plus the open deliveries the listing no longer returns that the index says are due, one whose claim was never answered included), `index_entries` (the entries the index read returned, none in the operator command, which reads no index), and one count per outcome, which add up to `deliveries`.
  `closed` is a delivery an earlier sweep settled (GitHub recorded it `OK`, or it expired):
  its failed attempt stays listed until it is older than the sweep's look-back, so after a burst of failures is redelivered, the sweeps of the next hour and six minutes count the burst as `closed`.
  `waiting` is a delivery whose next request is not due yet:
  in the running sweep one the listing returned, one whose index entry is keyed to the sweep's own minute with its request later in that minute, since the index is keyed by the minute and brings back only the deliveries due by it, or one whose entry the sweep adopts because a build without the sweep's index rewrote its record, which the sweep indexes again before it waits, whatever minute that entry is keyed to;
  and in the operator command every open delivery it reads that is not due and not claimed, listed or not.
  `claimed_elsewhere` is a delivery another sweep is asking GitHub for.
  A delivery the sweep's cap of 300 requests left over is not counted; the next sweep takes it.

- A delivery whose GUID the sweep's bucket cannot key is left out of the sweep:
  a GUID outside the characters a NATS key may hold (letters, digits, `-`, `/`, `_`, `=` and `.`), one holding a space or an empty token, or one so long that its index entry's key passes the bucket's bound for a write (`bus.KeyValue.RefusesWrite`).
  No sweep could read or write such a delivery's record or index entry, so it would otherwise fail every sweep, or leave every sweep incomplete with its cursor held, for as long as GitHub lists it.
  Each sweep that lists it logs `level=WARN msg="webhook redelivery left out a delivery whose GUID the bucket cannot key"` with `guid` and `error` (the bucket's refusal), decides the rest, and stays complete.

- A delivery's GUID, event and action are GitHub's text, so every line and error of the sweep shows them, and an index entry's `key`, to a bound (`githubapp.ShownBody`):
  whole up to 512 characters, and past that the first 512 and `… (<n> bytes)`, where `<n>` is the size of the text.
  The GitHub App client (`githubapp`) shows a refusal's body in its errors as `githubapp.ShownBody` shows it, and `<n>` is the size of the body.
  A value of an answer that the client cannot decode, in a time parse error or a JSON type error, is cut alone, and `<n>` is the size of the value.
  That error still names the time layout, or the field and its Go type.
  For a valid time with text after it, the time parse error's reason quotes that text (`extra text`).
  The client cuts that reason alone too, and `<n>` is the size of the reason.
  An error for a request that GitHub did not answer, or whose answer could not be read or passed the size limit, names the request's URL as `githubapp.ShownBody` shows it, and `<n>` is the size of the URL.
  A transport error also cuts its cause, so it still names the cause after a long URL, for example `context deadline exceeded`.
  A line the cause quotes is cut with the cause's text, and `<n>` is the size of the cause's text.
  `githubapp.ShownErr` cuts whole any other error that quotes GitHub's text, such as an error of the bucket that quotes a key, and `<n>` is the size of the error's text.
  No error that `githubapp.ShownErr` answers is longer than 4,414 bytes: two texts as `githubapp.ShownBody` shows them, each at most 2,079 bytes, and 256 bytes for the error's own words.
  A time parse error, a JSON type error or a transport error whose text is still longer after the cuts above is cut whole, and `<n>` is the size of that text.
  A time value past ASCII can make such a text, since package time writes each of its bytes past ASCII as `\xNN`.
  The sweep's errors and lines therefore carry that text the same way.
  A next page link longer than 2048 bytes is refused rather than followed, and shown as `githubapp.ShownBody` shows it.
  The refusal of a shorter link that cannot be parsed, or that leaves GitHub's API origin, shows the link the same way, and so does an error that names a next page GitHub answered with a status other than 200.

- The running sweep lists the last hour and six minutes (`redeliver.Lookback`, the longest backoff plus three sweep intervals), long enough that a delivery coming due still sees GitHub's answer to its last request in the listing.
  To that it adds the open deliveries the listing no longer returns that its index says are due, one still owed a request and one whose claim has run out alike.
  It reads the index from two minutes before its last sweep's start up to now, and over the last three days when it has no cursor yet.
  The operator command reads every record in the bucket instead, so it decides every delivery whose record is still open, neither delivered nor expired, even when its attempts are older than its window.
  After a gap in sweeping (Dispatch down), the running sweep resumes its listing from its cursor, back to GitHub's three days.
  The first sweep ever made, with no cursor, looks back 72 hours.

The cursor, one record per delivery acted on, one index entry per open delivery, keyed `pending.<YYYYMMDD>.<HH>.<MM>.<GUID>` by the minute that delivery next needs a sweep, and the marker that says when a back-fill last gave every open record an entry live in the JetStream KV bucket `envoy_webhook_redelivery`.
Its 72-hour TTL matches GitHub's horizon, and an index entry a sweeper died before deleting expires with the records.
A sweep that meets open records an older build wrote without entries gives each one an entry and then writes the marker, which holds the moment that sweep started, at or before the scan it read the records with.
Once that moment is further behind than the sweep's look-back, the next sweep reads every record again, so the whole-bucket read costs one sweep an hour at most for as long as those back-fills land:
a back-fill that leaves a record unindexed writes no marker, so the sweep after it reads every record again;
a sweep that reads a cursor an older build wrote — one carrying no mark of this build's — back-fills at once rather than waiting out that hour.
The server opens the bucket on the first sweep that finds NATS connected, not at boot, so a NATS that is down at boot skips the sweeps until it answers, each logged as failed.
A compare-and-swap claim, written before the request and replaced by GitHub's answer after it, keeps two Dispatch processes from requesting the same attempt twice, and the operator command honours the same rate limits.

The operator command runs the same sweep over a chosen window.
It does not move the running sweep's cursor.
`--dry-run` lists every failed delivery and what the sweep would do with it, and requests and writes nothing of its own;
a rate limit GitHub answers its listing with is recorded for every process, as any process's is:

```bash
envoy-dispatch redeliver-webhooks --since 72h --dry-run   # list what would be redelivered
envoy-dispatch redeliver-webhooks --since 72h             # redeliver under the sweep's rules
```

Its table shows each delivery's GUID, event and action as `githubapp.ShownBody` shows them, as the sweep's lines do.

It reads the same App credentials and NATS configuration as the server, and opens the database `DATABASE_URL` names for the rate limits every process shares;
it migrates nothing, so it runs against a database the server of its release has already migrated.
It owns nothing on the bus: unlike the server it neither creates nor updates `ENVOY_NOTIFICATIONS`, and it refuses a NATS server that is not the machine it runs on, naming the URL.
It opens the redelivery bucket as the server does, at `ENVOY_NATS_REPLICAS` copies (creating it, or raising one it finds with fewer), so run it with the server's value;
a value other than a whole number from 1 to 5 stops it before it connects.
It runs no keeper: the server's keeper raises the bucket back to the server's value every 30 seconds, and a refused raise names the bucket, `raise KV_envoy_webhook_redelivery: ENVOY_NATS_REPLICAS=3: context deadline exceeded` under a grant without its stream update.
Run inside the Dispatch container it reaches that deployment's NATS with `ENVOY_ALLOW_REMOTE_NATS=1 envoy-dispatch redeliver-webhooks …`.
It connects as the nkey user `NATS_NKEY_SEED_FILE` (or `NATS_NKEY_SEED`) names and runs no `!` command, so it never reads `envoy.natsSeed`:
a run against a NATS that requires a user needs `NATS_NKEY_SEED_FILE`.

## CI reconcile

The Envoy listener records a check run and a check suite from the webhook GitHub sends it, and the Legion daemon reads the settlement those records produce.
A webhook that reached no record leaves the head's record behind GitHub, and nothing announces it:
a run whose end it missed holds the head for ever, so the daemon waits for an answer that never comes, and a run it missed whole is absent, so the head settles while GitHub still runs that run and the daemon reads the head as done.
A commit status reaches no record through a webhook at all: GitHub's `status` event carries a commit and no pull request, and the listener drops it.
So every 5 minutes (`cireconcile.Interval`, Background loops, below) one Dispatch process reads each open pull request's head from GitHub and records every check run, finished or not, and every completed check suite the listener's record of that head lacks, and, where the App's token holds Commit statuses, the head's commit statuses.
It runs under the lease `ci-reconcile` with that 5 minutes as its period, so one process serves each period however many run (Running more than one process, below), and its first pass is one interval after the start.

- It is on when NATS is configured and the App's private key (`DISPATCH_APP_PEM_B64`) is present.
  When it is off it logs one line at start, whichever of the two it lacks: `dispatch: CI reconcile off: it needs NATS and the GitHub App private key`.
  It writes through the listener's own CI bucket, `envoy_ci_state`, which every pass binds afresh and none creates:
  the listener stays the one owner of that bucket's TTL and its copies, and a pass that finds no bucket says so and reads GitHub nothing.

- A pass reads every repository it covers (`cireconcile.covered`): each project's default repository,
  and each one an issue names as its own `repo`.
  That is every repository an issue resolves to, since an issue resolves to its own repository, an ancestor's own, or its project's default.
  Each is read under the name it is called today
  — its own name, or, where a rename pair's old name is that name, the pair's new name (`refs.CanonicalRepoSQL`, over the pairs the boot read)
  — so a repository named under both an old and a new name is read once and a name it was renamed away from is never asked of GitHub, which answers 404 for that name's installation.
  The boot's rename pass writes the new name into both columns (Database migrations, below), so this read changes a name only where a failed pass left the old one.
  It resolves each against GitHub (`GET /repos/{owner}/{repo}`) for the name and id GitHub itself gives it, and deduplicates by that id as well.
  Every record it writes is keyed by GitHub's own owner and name, never the project's or the issue's, which are stored lower-cased:
  a record keyed from either would be a second record of the same head on a second settlement subject.
  A repository GitHub does not answer is named at WARN under the name the pass read it by, counted in `defaults_skipped` and passed over, so the names a rename joins into one repository are skipped once between them.
  A project's default repository the App is not installed on is skipped the same way at every pass (`ci reconcile skipped a repository it holds no token for`), because a default without the App is a fault to mend: install the App, or change the project's default repository.
  A repository only an issue names that the App is not installed on is no fault: an issue's own repository is any `owner/name`, and Dispatch refuses that issue no work.
  The pass counts it in `repositories_without_app`, never in `defaults_skipped`, and names it at INFO (`ci reconcile passed over a repository the App is not installed on`) only when it asks GitHub about it.
  It holds GitHub's answer that the App is not installed for an hour (`cireconcile.AbsentAppRecheck`), for a project's default repository too, so it asks about such a repository once an hour and not once a pass.
  The cost is that the repair can lag by up to an hour after an operator installs the App on a repository, which is accepted because the live webhook path carries the repository's new events meanwhile.
  For a project's default repository the hold has a second cost: after an operator installs the App, the WARN and `defaults_skipped` of 1 also stand for up to an hour, until a pass asks GitHub again.
  The issues half reads the repositories of closed issues too: an open issue that names no repository works in the repository of the nearest issue above it in its own project, whatever the status of that issue.

- A pass reads a repository's open heads in batches, over GraphQL: one query answers twenty open pull requests with every check suite and every check run of each one's head commit (`cireconcile.headsQuery`), and the pass asks for the next twenty with the cursor that answer carries until GitHub says there is no next page.
  The query asks for the runs of every check type (`filterBy: {checkType: ALL}`), as the REST listing is read with `filter=all`, so a head read either way is recorded from the same runs: GitHub's default answers the latest run of each name alone — on one measured head, 3 of its 32 runs.
  The record keeps the newest run of each name whichever is read, so the latest runs alone would now write the same record.
  It orders the pull requests by creation, which is their number order, since a repository hands out numbers as pull requests are opened: that order is what the cursor is a place in.
  Reading the same heads over REST took 1,720 requests and 26 minutes 25 seconds for 833 heads of 16 repositories; the same read in queries is about 53 of them, about 318 points of the App's GraphQL budget and about 178 seconds of GitHub's own processing time.

- What a query cannot answer whole is read over REST exactly as every head was read before: the commit's check runs (`filter=all`) and its check suites, page by page and conditional on the ETags the pass kept, and its commit statuses where the App's token holds Commit statuses (below).
  Four things fall back that way: a head with more than 30 check suites, a head one of whose suites has more than 60 check runs, a head whose Checks fields GitHub refuses or answers null, and every head of a page GitHub refuses, gives up on, or answers past the 1 MiB response limit.
  For a refused page the pass lists that repository's open pull requests over REST and reads every head from that page onwards: the cursor of the page after it comes only in the answer the pass did not get, so there is no next page to ask for.
  That listing is read in windows of twenty, the size of a query's page, and every page of it is named by a number: one past the last head of the page before it, which the pass has read, and nothing for a repository whose very first page GitHub refused, where the listing is read from its start.
  The page GitHub refused the query for is named that way too, by the page the query answered before it, so a window is always cut at the first open pull request at or above a number rather than at a position, and a pass taking a cursor up reads the listing again and begins where the page began however many pull requests have merged below it since.
  Each head read over REST is named at INFO with the reason, so a head that falls back at every pass says why.

- Each run and each completed suite is recorded through the mapping a webhook's own body goes through, whichever read answered it, so a pass over a run the listener already recorded changes no record and publishes no settlement.
  A query's answer is mapped field by field into that body's shape first: GraphQL's upper-case `status` and `conclusion` lower-cased, its `permalink` as the run's `html_url`, `databaseId` as the id, the suite's `app.databaseId` as the App, the suite's `updatedAt` as its `updated_at`, and the commit's `oid` as the head sha.
  A run GitHub has not finished (queued, in progress, waiting) is recorded too, as its own webhook would have been: a pass that recorded a head's finished runs alone, after the listener missed the webhooks of the rest, would leave the CI store a record of finished checks, and the store would announce `checks settled` for a head whose CI GitHub was still running.
  The record's ordering keeps such a write from taking the head back: a queued or running run never replaces the same run finished, nor a newer run of its name, so the pass re-arms a settled head only for a run GitHub holds unfinished as the newest of its name, and the head settles again, superseding, once that run ends.
  A head's runs are one write of its record, one compare-and-swap (`cistore.Writer.RecordRuns`), so they land together or not at all: a write that fails never leaves the record holding some of the head's runs without the rest, such as a passed run without the failed one beside it, which the store would announce as a green head.
  Each run keeps the record's rules inside that write, exactly as written alone; a failed write is named at WARN, `ci reconcile did not record the check runs of a head`, with `runs`, the repository, the pull request, the head's sha and the error, and the head is read in full again by the next pass.
  A write that would take the record past its bound is refused whole: the store writes the record as it was, marked Overflowed, so the head never settles, and the pass keeps the head's ETags, since every later pass would meet the same refusal.
  A head whose run write failed is written nothing more in that pass, no suite and no status: a completed suite written after it would re-arm a record that lacks the runs GitHub holds, and the store would settle the head again on what the record held before.
  A run GitHub leaves unfinished for ever holds its head for ever, as it does when its own webhook arrives.
  Only a completed suite is recorded: a suite that is not completed can hold the head as a queued run does (`suiteHolds`, `packages/envoy/internal/cistore/cistore.go`), so one left queued with no run of its own would hold it for ever; a running suite holds the head through its runs.
  A `tester` or `architect` check run counts only from the App `DISPATCH_REVIEWER_APP_ID` names, by the rule the listener's webhook handler calls;
  with that variable unset no verdict check is recorded, and the server says so once at start.
  A suite of the App `DISPATCH_VERDICT_APP_ID` names, the one that writes `legion-verdict`, is not recorded, as the listener leaves it out under `ENVOY_VERDICT_APP_ID`: it is no CI result, and written into a settled record it would re-arm it, so the head would be published a second time once the App's run completes its suite.
  Give the two variables one value; with `DISPATCH_VERDICT_APP_ID` unset every App's completed suite is recorded, and the server says so once at start.

- With `ENVOY_CI_VERIFY=on` on the listener, a head's settlement is published only after a GitHub read that began after the listener asked for one (`packages/envoy/internal/cistore/verify.go`, `cireconcile.Reconciler.Verify`).
  A record cannot tell "no more checks" from "their webhooks never reached me": on 2026-11-22 the listener answered GitHub 503 for about a minute while NATS moved, a pass then wrote the runs that had finished in that minute, and the store announced `checks settled` on heads whose CI GitHub was still running.
  When a head is ready to settle and the listener holds its claim, the listener publishes nothing yet: it writes a verify request, the key `verify.<the record's key>` of the CI bucket, naming the settlement episode (the claim's generation), and keeps the KV revision that write got.

- The verify loop (`cireconcile.Reconciler.Verify`) runs every 5 seconds (`cireconcile.VerifyInterval`) in each Dispatch process: it reads the CI bucket's stream identity and lists the requests, each at the revision the bucket holds it at, with no lease; a pass that lists none takes no lease and logs nothing, which is every pass while no listener runs with the setting on.
  A pass that lists some answers them under the lease `ci-verify`, its own reservation of the lease pool and apart from the pass's own, and reads each head from GitHub: every page of its check runs and check suites, and of its commit statuses where the token holds Commit statuses, with no ETag.
  It writes what it read by the pass's own rules, the runs in one compare-and-swap, and then writes the request's revision and the stream identity as the record's `verified_rev` and `verified_stream` (`cistore.Writer.MarkVerified`): the revision it read before the GitHub read, kept through every retry of that write and never read again after the GitHub read.
  `MarkVerified` refuses an answer read in a stream the bucket is no longer held in, since a bucket deleted and created again numbers its revisions from 1 again.
  A read that fails, a listing that does not end, and a write that does not land write no `verified_rev`; an answer read whole counts even when it is empty.
  The loop reads a head it failed on again 1 minute later, and after each further failure of the episode waits twice as long, up to 8 minutes (`verifyRetryAfter`); the listener writing the request again does not cut that wait short, and a new episode is read at once.
  A status listing GitHub refuses costs the answer the head's statuses alone, as in the pass; a rate limit counts as no failure, and only a limit on the App's budget ends the loop's pass; while GitHub still runs a check suite of the head with runs in it, the loop writes what it read and no answer, and reads the head again a minute later, or at once when the listener writes the request again.
  The loop gives an episode up only for a failure no later read mends, at once: the App not installed on the repository, GitHub's 404, 410 or 403 with no rate limit behind it, a page past the response limit, a listing past 100 pages, or the answer's own write past the record's bound.
  It logs one ERROR, `ci verify gave up an episode: no later read of the head mends its failure`, deletes the request at its revision, and deletes every later request of that episode unread.
  A write of what GitHub answered past the record's bound is no failure: the store writes the record marked Overflowed, which never settles, and the listener lets its claim go when it sees that.
  The loop deletes an answered request only where the key still holds it at that revision (`cistore.Writer.DoneVerifying`), so a request written meanwhile stays for its next read.

- The listener publishes once the record's `verified_rev` is at or past its request's revision and below the record's own revision, its `verified_stream` is the bucket's stream now, the record is still ready to settle on what the read merged in, and the record still carries the exact claim it asked under, with more than 30 seconds of the claim's life left; that check reads the record fresh, and it is the authorization point (`cistore.Store.ClaimStillHeld`).
  An authorization read that fails keeps the wait and looks again a second later.
  A run the read found unfinished re-arms the record, which takes the claim's snapshot away, so nothing is published and the head waits for that run; an answer for a claim that has gone authorizes nothing, and the record's next claim writes a request of its own, which no earlier answer reaches.
  A request unanswered for 2 minutes is written again, with a new revision, and the listener refreshes its claim, which lives 4 minutes with verify on rather than twice the debounce; each rewrite logs `checks verify request unanswered` at WARN.
  After 5 unanswered requests (10 minutes) the listener gives the episode up: one ERROR, `checks verify gave up; the head is not announced until Dispatch answers a later episode`, the claim released and the record marked (`verify_stopped`, `verify_stops`, `verify_retry_at`).
  No listener claims the head again for 5 minutes after the first give-up of its snapshot, twice as long after each further one, up to 30 minutes; then a new episode asks, so a head given up in an outage is published once Dispatch answers, with no new observation of it.
  A new observation, which changes the snapshot, starts an episode at once; a give-up made once the head's last observation is the bucket's TTL (7 days) old is for good, so its record expires.
  With Dispatch down nothing is published.
  A wait the listener drops deletes its own request.

- The pass reads the commit statuses of a repository's heads only where the App's token for that repository holds Commit statuses, at read or write (`githubapp.Client.RepositoryTokenWithStatuses`).
  It reads that from the `permissions` GitHub answers with the token itself (`POST /app/installations/{id}/access_tokens`), not from the installation: the client keeps a token until 5 minutes before it expires, so a token minted before a person approved Commit statuses does not hold it while the installation already does.
  For such a repository the query asks for the head commit's combined status too (`status { contexts { context state createdAt targetUrl description } }`, `headsQueryWithStatuses`), which adds no connection and no request, and every other repository is sent the query it was sent before the pass read statuses, byte for byte.
  A head read over REST lists its statuses once its check runs and check suites are recorded: `GET /repos/{owner}/{repo}/commits/{sha}/status`, at `per_page=100`, page by page and conditional on the ETags the pass kept, as the other two listings are.
  A status in `success`, `failure` or `error` is mapped into the REST shape (`context`, `state`, `created_at`, `target_url`, `description`) and recorded through `contracts.GithubStatusObservation`, and a `pending` or `expected` one is not recorded.
  The record keeps the newest status of each context (`recordStatus`, `packages/envoy/internal/cistore/cistore.go`), and a query's statuses are part of the head's fingerprint, so a pass in which only a status changed records it.
  A failed status read costs the head its statuses alone: any `errors` entry at or under the head commit's `status` field of a query's answer, whatever its type, or any failure of the REST status read but a rate limit and the pass's own end, leaves the head's check runs and suites recorded and sends no further status request for that head in that pass.
  Either failure is named at WARN, `ci reconcile read no commit status of a head`, with the repository, the pull request, the head's sha and the error, and a rate limit stops the pass, as at any read.
  Deploy the listener first, then each Legion daemon, then Dispatch.
  A rollback of the listener below the release that records commit statuses goes with a rollback of Dispatch: an earlier listener drops the statuses of each record it writes, and nothing writes them back while GitHub's answer for that head stays the same.

- Every REST page is conditional on the `ETag` of its own last answer, so an unchanged page costs none of the rate limit; a query carries no ETag and is always answered in full.
  A head's ETags are kept only when every page answered and every observation the rules name was recorded, and are dropped when a page failed, when a write a later pass might land did not, and when the record they stand for has gone (the bucket's seven-day TTL, or a delete), so nothing is hidden behind a `304`.
  A head whose answer held nothing to record keeps its ETags: it has no record to lose, and dropping them would read it in full at every pass for ever.
  A write the CI store refuses because the record would pass its bound keeps them too: the record is written marked overflowed and never settles on the checks it could not hold, exactly as the listener's own writes leave it, so every later pass would meet the same refusal.
  A head a pass's own deadline or a rate limit cut short keeps them as well: a cut is the pass ending, not a head that failed, and the next pass reads that head again from the start (the cursor, below).
  A head is forgotten only when a whole cycle no longer lists it.

- Requests are serial, as GitHub asks a poller for, and queries and REST reads are paced apart by the two budgets GitHub keeps them under.
  After each query the pass waits a third of what that query took, so its queries never take more than 45 seconds of a minute, inside GitHub's secondary limit of 60 seconds of GraphQL processing a minute for the whole App.
  Before each REST request it leaves the gap it set itself: the requests the last pass learned this one will take — the pages of each listing of each head a query could not answer, and one identity read for each repository the pass covers — divided into nine tenths of its own deadline.
  That gap is never below `cireconcile.FastestRequestPace` (100ms, ten requests a second, inside GitHub's secondary limit of 900 points a minute) and never above `cireconcile.RequestPace` (500ms), which is the pace a deployment configures and the slowest the pass reads.
  The first pass of a process knows of no work and reads at the floor, and the pass after it knows the real count.
  The passes after a pass that its own deadline ended before it reached a head — read one whole or gave one up — read at the floor until one reaches a head.
  In a process's first cycle such a pass learned no page of any head it listed, so the arithmetic would set the passes after it at the ceiling, and under a short deadline a pass at the ceiling can reach no head either, so no cycle would ever close; later, where the `Reconciler` holds pages from earlier passes, the floor is only the faster of the two answers, never a slower one.
  Every other way a pass ends with no head reached leaves that floor as it was: a rate limit, even one GitHub answers as the deadline falls; its caller's cancellation or deadline; an order with no head in it.

- A pass is bound at the interval its loop runs it on, so it always ends before the pass after it is due and always logs its one line.
  The pass walks one stable order — the repositories a pass covers (`cireconcile.covered`: the projects' default repositories and the ones an issue names as its own), and within each its open pull requests by number — from the first repository to the last page of the last one, and keeps a cursor in it.
  A pass starts at the cursor, reads to the end of the order and ends there rather than beginning the order again, and ends early when its deadline is up or at a rate limit.
  A pass cut short leaves the cursor at the first page or REST head it did not finish, which the pass after it takes up, so no head starves behind the heads the order holds before it; a pass that reaches the end of the order leaves the cursor at the start of it.
  One walk of the order, however many passes it takes, is a cycle, and only the pass that closes a cycle forgets what the cycle no longer lists, by what every pass of that cycle reached.
  The cursor is this process's own memory and is written nowhere: a restart deliberately begins a new cycle at the first page, which costs one cycle its fairness and nothing else.

- The shared GitHub rate limits in `github_rate_limits` are read before the pass, before each repository, before each query and before each head, and the first limit — one this process read, or one GitHub answered with — ends the pass where it stands.
  REST reads are held under the App's scope and the installation's; queries are held under `githubapp.GraphQLScope` (`graphql`) as well, which is the App's GraphQL budget and no installation's.
  A GraphQL limit does not look like a REST one: GitHub answers it with HTTP 200 or 403 carrying an `errors` entry of type `RATE_LIMITED`, `x-ratelimit-remaining: 0`, a `retry-after`, or its wording about a secondary rate limit — in an `errors` entry, and at 403 in the REST-shaped body whose top-level `message` carries it — and all of them are read as the limit (`githubapp.GraphQLRateLimit`), from GitHub's own envelope and headers and never from the data a query answered: a check run named for the limit is a repository's data, not GitHub speaking.
  Which of the two limits it is says the scope it is recorded under: the App's GraphQL points spent — the `RATE_LIMITED` entry, or no requests remaining — is recorded under the GraphQL scope, so every process's REST reads go on while the queries wait it out, and a secondary limit — a `retry-after`, or GitHub's own wording wherever the answer carries it — is recorded under the installation's scope, which GitHub shares between that installation's REST calls and its queries, with the doubling backoff a strike writes (`ratelimit.Store.Strike`), whose count starts again after an hour with no strike of that scope and is not held open by the ordinary REST limits recorded there meanwhile.
  A rate-limited answer is never a page to read another way: it stops the pass, where a refused page, and a page holding a pull request GitHub answered with no node or no head commit, send that page's heads to the REST fallback.
  The pass records no limit of its own beyond that: the App client records GitHub's own answer, as every process's App call does.

- Every pass logs one INFO `msg="ci reconcile pass"` line: `list_pages`, `heads`, `head_requests`, `heads_not_modified`, `records`, `defaults_skipped`, `heads_too_large`, `rate_limited`, with `rate_limit_scope` and `rate_limit_until` when it stopped at one, and then `heads_known`, `requests`, `pace`, `first_head`, `last_head`, `cycle_complete`, `deadline_reached`, `graphql_queries`, `graphql_points`, `rest_requests`, `rest_not_modified`, `heads_over_rest`, `heads_unchanged`, `duration` and `repositories_without_app`.
  `defaults_skipped` counts the repositories the pass skipped for a fault, each of them named at WARN, so a number above zero is something to mend;
  `repositories_without_app` counts the repositories only an issue names that the App is not installed on, which is no fault and stays above zero for as long as an issue names one.
  `records` counts the records the pass changed, one for each head whose record a write of the pass changed, never one for each observation it wrote:
  a head's runs, its suites and its statuses all write the one record of that head (`cistore.Key`), so a pass that records a head's run and its suite reports 1;
  a check write the CI store refuses for its bound counts for nothing, although it marks the record overflowed, and a status write it refuses for its bound counts for nothing and leaves the record as it was.
  `heads` is the heads this pass read whole, either way, and `heads_known` the open heads the cycle's queries listed; `first_head` and `last_head` name the first and the last head the pass read, each `owner/repo#number`, so two lines side by side say how far round the order each pass got;
  `graphql_queries` and `graphql_points` are the queries the pass asked and what GitHub charged them, `rest_requests` the REST reads beside them, `rest_not_modified` how many of those GitHub answered `304`, and `heads_over_rest` the heads a query could not answer whole — a number that climbing says the queries are answering less and less;
  `pace` is the gap it left between REST requests, `deadline_reached` says its own bound ended it, and `cycle_complete` says it reached the end of the order and left the cursor at the start.
  `DISPATCH_TEST_CI_RECONCILE_INTERVAL` replaces the 5 minutes for an end-to-end test, and a deployment never sets it; it is the pass's bound as well as the loop's period.

- Every pass also records itself in `ci_reconcile_passes`, which is that line in a table anyone can read a day later, and one thing the line does not carry: how the pass ended.
  A container log rotates — the production pod's reached back 45 minutes, nine passes — and the question a person asks is usually about a head read hours ago, so the counts live in Postgres as well.
  The pass writes its row when it starts, with its process's `server_processes` instance id and the moment it started, and fills the rest in when it ends: the end time, the eight counts its line carries (`graphql_queries`, `graphql_points`, `rest_requests`, `rest_not_modified`, `heads_over_rest`, `records`, `heads`, `heads_known`), how it ended (`complete`, `deadline`, `rate_limit` or `cancelled`, which are the whole set the column's check allows) and where it left the cursor.
  The end write runs on a bounded context of its own, never the pass's, exactly as a lease's own statements do (`duty.LeaseCall`): the pass's context is the thing that has just expired for the pass this record matters most for, and a write made on it would land nowhere.
  A pass whose process is not registered yet writes no row at all, which is the first pass or two of a process: the loops start before `processes.Register` returns, since registration waits out a live peer whose signing keys do not match.
  A pass its process's shutdown cancelled records `cancelled`, which its own line does not say (`deadline_reached` names the pass's own bound and nothing else) and the `dispatch duty: leased pass cut short by shutdown` line beside it says only while the log still holds it: the row is read a day later, and a reader of it must not take an interrupted pass for a finished one.
  The pass that ends deletes every row older than 48 hours, so one pass every five minutes leaves about 576 rows and the prune needs no loop of its own.
  `GET /api/v1/ci-reconcile/passes` answers the last 24 hours of them, newest first (Routes, below).
  A row with no end time reads as `running` only while its process is live by the registry's 15-second window, the `ci-reconcile` lease is held, and it is the newest row; otherwise it reads as `ended_without_completing`, which is a process killed between a pass's start and its end.
  No one of those three decides it alone: a killed process counts as live for up to a live window, a lease is held by any session the database has not reaped, and neither notices a row a later pass has overtaken.

## Moving uploaded files to the bucket

With `DISPATCH_FILE_STORE_BUCKET` set, new uploads go to the bucket;
the files uploaded before it stay in their rows until `backfill-files` moves them, oldest first, one row at a time:
it writes the object under the row's hash (a write the bucket verifies, S3 against that hash and Cloud Storage against its CRC32C and MD5, and the store refuses before any call when the bytes do not hash to it), reads it back and checks it, and only then clears the row's bytes, in one statement that clears only a row still holding them.
A row it cannot move (a store failure, bytes that do not hash to the row's hash, a row with no hash) is printed as `FAILED version <id>` and passed over, so one bad row holds up no other;
the run exits 1 when any row failed, and can be run again at any time, since each row is done or untouched and the server serves a row until its bytes are cleared.
Two writing passes at once (`backfill-files` and `--restore`, in any pairing) are refused by an advisory lock (`another backfill-files pass holds the
lock`).
`--verify-only` reads back every cleared row against its hash, moves nothing, takes no lock (it runs beside either pass, reading only what that pass has finished), and exits 1 naming each row whose object is missing or wrong.

While a live server process has no bucket (a process without `DISPATCH_FILE_STORE_BUCKET`, or one of a build that writes a `server_processes` row with no `file_store`), that process serves a file only from its row.
So an upload to a process with a bucket keeps its bytes in its row as well as in the bucket, and the move refuses before it touches a row, with exit 1 and a line that names each such process by its instance id, its build and when it started.
`--verify-only` and `--restore` take no bytes out of a row and run beside such a process.
A release that rolls the bucket out, or that runs beside a build before it that writes a `server_processes` row, therefore serves every upload from every process;
run `backfill-files` once every process has the bucket.
A build that writes no `server_processes` row (every release before migration `0135_server_processes`) is not seen by either check, so set the bucket no sooner than the release after the first one that carries this rule, and only after every process of the earlier release is gone ("Two builds at once", below).

`--restore` is the rollback, at any point before or after the clear:
it reads every cleared row's object back, verified against the row's hash, and writes the bytes into the row again, so a server with the setting unset, or an image from before the store existed, serves every file from its row.
The objects stay in the bucket, and a later `backfill-files` moves the rows out once more.
Turning the store off takes a restore before the release that unsets the setting:
a server without it serves a row still holding bytes and answers `503 FILE_STORE_UNAVAILABLE` for every row written while the bucket was set, an upload's as much as a cleared one's;
and an image from before the store serves a cleared row as an empty `200` under the file's own `ETag`, which a browser then caches.
While that release rolls, the processes that still have the bucket keep each new upload's bytes in its row too, since each new process registers that it has none.
Restore first, then roll the release out.

```bash
envoy-dispatch backfill-files                 # move every row still holding bytes; prints each one and a total; exit 1 if any failed
envoy-dispatch backfill-files --verify-only   # read back every moved row; exit 1 naming any that fails
envoy-dispatch backfill-files --restore       # the rollback: write every moved row's bytes back from the bucket
```

It needs `DATABASE_URL` and `DISPATCH_FILE_STORE_BUCKET`, which it opens as the server does (`gs://<bucket>` or an S3 bucket's name, with the same credentials and grant), refuses a bucket whose health probe fails (the one `/healthz` runs) before touching a row, and reads nothing else of the server's configuration.
Clearing a row's bytes leaves dead tuples behind:
the table shrinks on disk only after `VACUUM FULL` or `pg_repack`, while the nightly dump shrinks at once; measure the dump, or vacuum first.

To run a local server against a bucket of your own, point the SDK at an S3-compatible server with `AWS_ENDPOINT_URL_S3=http://127.0.0.1:<port>` (an IP literal:
with `localhost` the SDK addresses the bucket as a subdomain, which does not resolve), `AWS_REGION`, static `AWS_ACCESS_KEY_ID` and `AWS_SECRET_ACCESS_KEY`, and `AWS_EC2_METADATA_DISABLED=true`.
The Go tests start one (`internal/tests3`, SeaweedFS in a container, since MinIO's image left Docker Hub) and create the bucket with an anonymous `PUT /<bucket>`.

## Identity

`DISPATCH_IDENTITY` controls how browser requests identify a person, always by lowercase email:

- `cookie` (the default) accepts signed `dsession` cookies, which a sign-in issues (OpenID Connect authorization code), through one of two issuers.
  Beside `DISPATCH_SIGNIN_ISSUER`, `DISPATCH_SIGNIN_CLIENT_ID` and `DISPATCH_SIGNIN_CLIENT_SECRET`, exactly one of `DISPATCH_SIGNIN_GROUP` and `DISPATCH_SIGNIN_DOMAIN` is set;
  both, some without the others, a domain under any issuer but `https://accounts.google.com`, a group under that one, or a domain that is not a lowercase DNS name refuse the boot, each naming what is wrong (`checkSignInSettings`).

  With no `DISPATCH_SIGNIN_*` setting at all the server boots and runs no browser sign-in:
  it issues no `dsession` cookie, and it refuses every one it has issued, answering each request that needs a person `401 NO_IDENTITY` (`requestIdentityFor`).
  A cookie minted while sign-in was on still verifies for its thirty days, since the key signing it is the deployment's `DISPATCH_SIGNING_KEY` and outlives the configuration, and with no issuer nothing is left to confirm that the person it names may still use Dispatch.
  The agent-token API is served as it always is, and `GET /auth/start` answers `503 SIGNIN_UNCONFIGURED`.
  That is what a sandbox deployment runs (`envoy-infra:gcp:Dispatch` with its tailnet inputs unset, which has no origin an OAuth provider takes).
  A partial set is still half a sign-in and refuses the boot, naming every setting it misses (`checkSignInSettings`).
  Dev sign-in (`DISPATCH_DEV_SIGNIN`, below) sets none of these either and keeps its cookies: it mints them itself.

  The issuer's discovery document is read in the background from boot, on the service-account issuer's schedule (above), and an issuer that cannot be reached refuses nothing:
  until the document has been read, `/auth/start` and the callback answer `503 SIGNIN_UNAVAILABLE`, and the rest of Dispatch serves.
  A callback whose code exchange the issuer's token endpoint answers `5xx`, `408` or `429`, whatever OAuth error its body names, answers with a body cut off before it arrives whole, or does not answer, is `503 SIGNIN_UNAVAILABLE` too: that closed set is the issuer down.
  Every other answer is the endpoint answering, and the sign-in fails `502`: a `4xx` with or without an OAuth error, a `2xx` that names an error or holds no token, and a `3xx`.
  A sign-in in flight (its state, its nonce, the page it returns to and when it lapses, ten minutes after it started) travels in the browser's `dispatch_oauth_state` cookie, signed under a key derived from the current signing key for that purpose alone, and no process keeps it:
  a sign-in one process started completes on whichever process the issuer's redirect reaches, given one `DISPATCH_SERVER_URL` on every process:
  the exchange must send the callback URL the sign-in started with, and each process builds it from that setting, or without one from the request's `Host`.
  The callback clears the cookie first, and answers `400` "invalid or expired state" to a cookie that does not verify under a key of the set, has lapsed, or names a state other than the one the issuer returned.
  Each sign-in completes at most once: the issuer's code is single-use, and the exchange accepts only an ID token carrying the cookie's nonce.

- Through a shared Cognito sign-in pool (`DISPATCH_SIGNIN_GROUP`), the callback names the person by the email in their pool username (`<provider>_<email>`, for a provider the ID token's `identities` names), never by the `email` claim, which a person can write, and signs in only a member of `DISPATCH_SIGNIN_GROUP`;
  anyone else gets a 403 page naming them.

  With Google directly (`DISPATCH_SIGNIN_DOMAIN`, under Google's issuer and a Google web client whose redirect URI is `<DISPATCH_SERVER_URL>/auth/callback`), the authorization request asks for offline access with `prompt=consent`, so every exchange returns a refresh token, and names the domain as `hd`, a hint only.
  The callback signs in a person whose ID token's `hd` is the domain and whose `email_verified` is true (the JSON `true` or the string `"true"`;
  any other value is false), named by that `email`, lowercased; the token's `hd` decides, never the email's own domain.
  Anyone else gets a 403 page with the reason: not in the domain, email not verified, or no email.
  The code flow reads those claims from the verified token in domain mode alone;
  the shared verifier, which Cognito tokens, pods' tokens and the listener go through, reads none of them.
  A refusal logs `msg="dispatch: sign-in refused" email=<email> hd=<hd> reason=<reason>`, where a pool refusal logs `username=<username>`.

  Either way, Dispatch keeps the issuer's refresh token and confirms the person with it at least hourly, the hour measured on the database's clock (`people.confirmed_at`), so processes whose clocks differ agree on when it lapses.
  A refresh the issuer refuses (`400` or `401` with the OAuth error `invalid_grant`:
  the token expired, was revoked or is not the client's), or one whose ID token no longer passes the rule or names another email, ends every session they hold.
  A refresh that fails any other way (the issuer undiscovered, down or slow, a `5xx` or `429`, another OAuth error, or an answer without an ID token that verifies) ends nothing:
  the request that found the confirmation lapsed is refused `503 SIGNIN_UNAVAILABLE`, the sessions and the refresh token are kept, and the first request after the issuer answers again refreshes and is served.
  A refresh is one request to the issuer's token endpoint, with the client's secret sent the way the issuer's discovery document lists in `token_endpoint_auth_methods_supported`: by HTTP Basic where it names `client_secret_basic` (Google's does), else in the form where it names `client_secret_post`.
  Each request to the token endpoint has ten seconds, and the whole refresh, which also reads the person's row and, the first time, the discovery document, has fifteen; a refusal ends the sessions when its answer comes within both.
  An issuer whose document names neither is asked by HTTP Basic and, when that fails, may be asked again in the form, so its refusal can need both requests to answer within the fifteen seconds.
  The list says what the issuer supports, not what each client may use, so an issuer that holds a client to the one `token_endpoint_auth_method` it registered (RFC 7591 section 2) refuses a client registered for `client_secret_post` alone, under a document that names `client_secret_basic`, with `invalid_client` and no second request in the form: its sign-ins fail, and its refreshes end nothing and are answered `503 SIGNIN_UNAVAILABLE`.
  A Cognito sign-in pool's token endpoint takes `client_secret_basic` and `client_secret_post` from an app client with a secret, as AWS documents.
  Google's document names both, which says only what its endpoint supports: a 2026-11-21 probe of the Google Cloud SDK's installed-app client found both accepted, and no web client was probed.
  Logout ends Dispatch's session only, not the issuer's.

- A person is their email, so an address passed from one account to another is one person.
  In domain mode `people.sub` records the Google account (`sub`, never reused) that last signed in to the address:
  a sign-in under another `sub` ends every session of the address in the transaction that records the new one (a failed revoke records nothing, so the retry revokes again), logged `msg="dispatch: a sign-in under another Google account ended every session of the address"` at WARN with the email, and the hourly confirmation ends them when the refreshed token's `sub` is not the recorded one.
  A sign-in whose address another account signs in to between its record and its cookie gets `409 SIGNIN_SUPERSEDED` and no cookie:
  after reading the session generation the callback checks that the row still records its `sub`.
  Group mode neither reads nor writes `sub`, so there the old holder of a reassigned address keeps the cookies they have until their cookie expires, 30 days after it was issued, unless the pool user is removed from `DISPATCH_SIGNIN_GROUP` or disabled.

  The refresh token is stored sealed (`people.refresh_token`):
  AES-256-GCM under a key derived from the current key of `DISPATCH_SIGNING_KEY` with HKDF and a fixed purpose label, bound to the person's email, in a format whose `v1:` prefix names its version.
  A value is opened under each key of the set in turn, the GCM tag refusing a key it was not sealed under;
  it names no key, so the `v1:` format is byte for byte the one earlier releases write and read.
  A stored value that does not open (sealed under a key the set does not hold, moved from another person's row, or in plain text) counts as no refresh token:
  each read of it logs `msg="dispatch: a stored refresh token did not open; the person signs in again"` at WARN with the email and never the value, and writes nothing;
  the person's request answers `401 NO_IDENTITY`, and their next sign-in replaces the value.

- A refresh token stored in plain text (by the release before sealing, or by a task of it still serving during a roll or after a rollback) can be redeemed by anyone holding a copy of the row and the app client's secret until it expires, and forgetting it does not end it, so Dispatch retires it:
  at every boot, after migrations, it revokes each plain-text token at the pool's revocation endpoint (the `revocation_endpoint` the issuer's discovery document names) as the app client, then clears it and its confirmation while the row still holds that value.
  A sign-in that replaces a plain-text token retires it the same way first.
  A sealed value is never sent to the pool, and a redirect from the revocation endpoint is a failure, never followed.
  A token the boot cannot revoke, the issuer's discovery document not yet read included, stays in its row for the next boot and logs `msg="dispatch: the sign-in pool did not revoke a refresh token stored in plain text"` at ERROR with the email and the error (the pool's HTTP status, when it answered), never the token.
  A sign-in that cannot revoke the token it replaces goes on, since the person just signed in at the pool, and logs `msg="dispatch: a sign-in replaced a refresh token the pool did not revoke; sign this person out at the pool"` at ERROR with the email and the error:
  no later boot will see that token, so signing the person out at the pool is the only way to end it.
  The boot's retirement stops after 30 seconds, failing each token it has not revoked, and the start goes on;
  it logs `msg="dispatch: retired the refresh tokens stored in plain text" retired=<n> failed=<n>`.

  A database backup therefore holds no refresh token the pool would accept only once a boot of this release has retired every plain-text token, and then only for the tokens still in `people` when it ran:
  those stop working when the pool revokes them.
  A plain-text token `0068`'s release removed without revoking (a sign-in that replaced it, or a logout that cleared it) stays valid at the pool until it expires, in any backup that holds it (Database migrations, below).

  A rotation of `DISPATCH_SIGNING_KEY` signs nobody out when it is made in three steps, each rolled to every process before the next:
  (a) add the new key second, after the current one; (b) move it first; (c) remove the old key, no sooner than 30 days after (b), the session cookie's lifetime.
  A cookie made under the old key verifies until it expires, and a refresh token sealed under it is sealed again under the new key by the person's next hourly confirmation.
  Each step passes the boot's key check beside processes still on the step before (Running more than one process, below).
  Replacing the key in one step fails that check beside a live process that holds only the old key, and on a lone process signs everyone out.
  The release that first ships the key set is never also the release of a first rotation, and no build older than the key set is rolled back to while the setting holds two values:
  such a build reads `"K1 K0"` as one key, which signs everyone out.
  Removing a key revokes no refresh token at the pool:
  whoever holds a backup and the old key can still redeem the tokens sealed under it until they expire, so after a key leak, sign each person out at the pool.

- `iap` is for a Dispatch behind Google Cloud's Identity-Aware Proxy (IAP), which signs people in at the load balancer in front of it.
  On each request it lets through IAP adds an assertion it signs, `X-Goog-IAP-JWT-Assertion`, and Dispatch names the person from that assertion alone, never from IAP's unsigned `X-Goog-Authenticated-User-Email` or `X-Goog-Authenticated-User-Id`, which any request that reached Dispatch around IAP can carry.
  It verifies the assertion as Google documents ([Securing your app with signed headers](https://cloud.google.com/iap/docs/signed-headers-howto)) through `internal/oidc` (`oidc.IAPVerifier`):
  an ES256 signature by a key of IAP's set at `https://www.gstatic.com/iap/verify/public_key-jwk`, a fixed address, since IAP publishes no discovery document (`oidc.NewKeysAt`), read on the first request and cached as a service-account issuer's set is;
  the issuer `https://cloud.google.com/iap`;
  the audience `DISPATCH_IAP_AUDIENCE` (`/projects/<project number>/global/backendServices/<backend service id>` behind a load balancer);
  and `exp` and `iat` (and an `nbf`, which IAP does not write), each with the 30 seconds of clock skew Google documents; a time that is not a number of seconds from 1970 to 9999 is refused as malformed.
  Who may use Dispatch is the domain sign-in's rule (`identity.DomainRule`), so `DISPATCH_SIGNIN_DOMAIN` is required:
  a person is admitted when the assertion's `hd` is that domain and its `sub` names a Google account (`accounts.google.com:<id>`), and is named by the assertion's `email`, lowercased.
  An assertion carries no `email_verified`; its `email` is the address of the Google account IAP signed in, which Google tells an app to take as the person.
  `DISPATCH_SIGNIN_ISSUER`, `DISPATCH_SIGNIN_CLIENT_ID`, `DISPATCH_SIGNIN_CLIENT_SECRET` and `DISPATCH_SIGNIN_GROUP` are refused beside it, and so are dev sign-in and `DISPATCH_IAP_AUDIENCE` under any other identity.

- Under `iap`, IAP signs a fresh assertion for every request, so each request is decided on its own:
  Dispatch keeps no session cookie, refresh token or hourly confirmation for it, and a person IAP or the domain stops admitting is refused from their next request.
  A command-bar run is the one exception, since its calls come after the request that started or approved it: its grant names the person alone and lasts until the run's working budget ends, and an approval through IAP records it anew (Answering a question, below).
  Each person admitted is recorded in `people`.
  A request with no assertion, one that fails a check, or one whose person the rule refuses is `401 NO_IDENTITY`;
  a refused assertion logs `msg="dispatch: IAP assertion refused" reason=<class>` and nothing else, since the error of an assertion nobody signed quotes its own unverified `iss` or `aud`, which its sender chose; a refused person logs `msg="dispatch: IAP person refused" email=<email> hd=<hd> reason=<reason>`, read from an assertion IAP signed; both at WARN.
  An expired assertion logs `msg="dispatch: IAP assertion expired"` at DEBUG alone (below).
  Only an assertion IAP signed counts as expired: one past its `exp` whose ES256 signature IAP's key set verifies, for this audience, with an `exp` and an `iat` that hold, and whose person the rule admits (`oidc.IAPExpiredError`, `identity.ErrCredentialExpired`).
  An expired assertion that is wrong in any other way (a signature IAP's keys do not verify, another key id, RS256, an account the rule refuses, no `iat`) is refused for that, with its WARN line, as a fresh one is: the verifier checks the algorithm, signature, issuer and audience first, with go-oidc's own expiry check off, and reads `exp`, `iat` and `nbf` itself only from a payload that passed them.
  A key set that cannot be read reaches no verdict, and the request is `503 SIGNIN_UNAVAILABLE`.
  An unsafe request that carries an assertion is held to Dispatch's origin as one with a session cookie is (`api.EnforceCookieOrigin`), since a browser sends IAP's cookie with another site's request and IAP adds an assertion to it.
  Dispatch runs no sign-in of its own, so `/auth/start` answers `503 SIGNIN_UNCONFIGURED`, and `POST /auth/logout` signs nobody out of IAP: the next request carries IAP's next assertion.

- Under `iap`, a websocket is the one request whose assertion ages: the live socket and the document socket re-check the assertion their upgrade carried on every heartbeat (15 seconds, `api.whileCallerResolves`), and Google gives an assertion at most ten minutes and the skew.
  Once that assertion has expired, and IAP's keys still verify it and its person is still admitted, the heartbeat rotates the socket rather than cutting it (`identity.ErrCredentialExpired`):
  the live socket closes with `4001` (reason `credential expired`), on which the page reopens it at once and shows nothing, and the document socket with `4412` (`DOC_ADMISSION_REVOKED`), on which the provider reconnects.
  The reopened socket's upgrade carries a fresh assertion, judged again as any upgrade is, so no socket is served past its assertion, and an IAP user's sockets each reopen about every ten minutes.
  Any other refusal on a heartbeat closes the socket as before, an expired assertion that IAP's keys no longer verify (a key IAP withdrew) or whose person the rule no longer admits included: `1008` on the live socket, and no close frame (`1006`) on the document socket.

- `header:<Header-Name>` is for tests and local harnesses only, never for a production Dispatch deployment.
  It accepts a header value, lowercases it and records the person.
  The harness must set `DISPATCH_IDENTITY_HEADER_TRUSTED=1`;
  Dispatch refuses header identity when that flag is absent or when any `DISPATCH_SIGNIN_*` setting is present, so it cannot share a deployment with Google sign-in.

The people Dispatch has seen, either way, are the assignee picker's options and the only names an issue may be assigned to.

`DISPATCH_INSECURE_COOKIE=1` omits the `Secure` attribute for local plain-HTTP testing.
Do not use it on an HTTPS deployment.

`DISPATCH_DEV_SIGNIN=1` adds a second way to get the cookie on a local instance:
`GET /auth/_dev/signin?login=<email>` records the lowercased person and issues the session cookie an issuer sign-in does, with no issuer exchange and no group check.
The route serves only a loopback peer whose request carries no forwarding header, logs every mint at WARN, and boots only behind the fence its Required configuration item lists.
A request that reaches the process looking local (`ssh -L`, `socat`, a proxy that rewrites `Host` and adds nothing) is indistinguishable from one that is.
The per-process key bounds the cookie, not every credential a dev session can write:
a `dsp_` token it mints, and the session and token rows its sign-out changes, live in the database, and every server that reads that database acts on them.
A dev-sign-in server therefore needs a database of its own.
The one exception to the per-process key is a harness that runs several dev-sign-in processes on one database on purpose, as the browser suite's two-process mode does:
`DISPATCH_DEV_SIGNIN_SHARED_KEY` gives them one key, derived from the value under a dev-only label, so their cookies are each other's and never a production server's.

## Local Postgres

Start the local database once:

```sh
cd packages/envoy
./scripts/dev-postgres.sh
```

It prints the `DATABASE_URL` to use for development and tests.
Run again, it starts or reuses the same `dispatch-pg` container, which runs its pinned image:
pgvector on PostgreSQL 18, whose `vector` extension the migrations need.
It refuses a `dispatch-pg` on any other image or major and prints the commands that dump that container's databases and restore them into a new container on the pinned image.
The database retains the migrated schema; tests only truncate rows they create.

## A large test database

`go run ./cmd/dispatch-seed` fills an empty database with the data set a statement measurement is taken on:
ten times production's row counts at `--scale 1` — 10 projects, 20,000 issues over parent chains and `blocked_by` links, 40,000 documents with 200,000 versions, 80,000 asks, 370,000 comments, 60,000 messages, 1,250,000 events beside the `ask.opened` event every ask is written with, and a vector for half the issues, documents, asks, comments and messages
— and the same shape times the scale at any other.
It is deterministic: one fixed base time, one fixed random seed, and a fixed block id generator (`pmdoc.SetBlockIDGenerator`), so two runs at one scale write the same rows and a reading taken before a change and one taken after it read the same data.
The text is drawn from a Zipf list of 20,000 words, so the `tsvector` columns carry real lexemes at known frequencies.

```sh
cd packages/envoy
go run ./cmd/dispatch-seed --database-url <url> [--scale <f>] [--no-migrate]
```

`--database-url` is required.
`--scale` defaults to 1 and multiplies every row count, keeping the proportions and the text sizes;
`--scale 0.01` is about 200 issues and is what the CI test runs.
The command migrates the database itself with `store.Migrate`, as the other subcommands do, and never by booting a server, whose background loops would write to the data set it is building;
`--no-migrate` skips that step for a database somebody else migrated from another commit, which is what a before-and-after measurement across two commits needs.
Seed replies inherit their parent's owner and ask thread.
The writer stores the comment each reply answers when `comments.answers` is present, and can seed the schema before that column with `--no-migrate`.
It refuses a database that already holds a `projects` or an `issues` row, naming what it found:
it writes fixed keys, so a second run over the same rows would fail halfway through.

It calls the server's own indexers over what it writes
— `refs.PlanBlocks` for each document's latest version, `refs.IndexItem` for each comment's and message's blocks, `refs.PlanAskParts` for each ask's parts (its question, its options and the answer it holds), the two plans run by the seed's own transaction as a batch of their own (`store.RunWrite`), since it joins no write phase of the server's
— and then settles the state those writes leave owed, so a Dispatch server started on the seeded database rewrites nothing:
the block index, the item blocks, the task counts, the reference excerpts and the event publication are all already done, and the per-owner `last_seq` counters match the seeded events, so the first comment, ask, message, upload or title edit written to a seeded issue does not fail the unique `(issue_key, seq)`.
Every ask is written with its `ask.opened` event, in the ask's own transaction as the create route writes it, because every read of an ask resolves that event (`events.OpenedEventIDs`) and fails the whole request when the ask has none, which would answer 500 on the ask lists, the ask read, the Inbox and an issue's `open_asks`.
It ends with one full `search_term_stats` build for every project, so no measured related-terms pass is a first-use full build, and with `vacuum (analyze)`, not `analyze`:
a freshly loaded table has no all-visible pages, so the planner avoids an index-only scan on it however good the statistics are.

That build is the one the database's own schema carries, read once from the catalogue rather than by running a build and catching its error, which could not tell a missing column from a real failure:
a database holding `search_term_totals.snapshot` (migration `20261101182515_search_term_totals_snapshot`) is built by the server's own pass (`api.RefreshEverySearchTermStats`, which the command's `buildEverySearchTermStats` calls), so the row carries the snapshot that makes the first measured pass a step pass;
a database migrated from an older commit, which is what `--no-migrate` seeds for the "before" side of a two-commit measurement, is built by that release's four statements, which write `items` and `refreshed_at` alone.
Either way the step fails the command when it built fewer projects than the seed wrote.

The command prints every table's row count;
every figure the sizes are stated as a distribution rather than a count (the issues per project, the parent chain depths, the issues with three or more blockers, the documents with 28 versions, the markdown size bands, the anchored comments and asks, the open asks, the root messages, the events with a session actor, the rows with an embedding, and the edges of `graph_edges` by kind);
a rare, a medium and a common word with the number of documents each is in, measured rather than assumed;
and the subjects a measurement binds — the largest project, the heavily linked issue, the deepest parent chain, the document carrying anchored comments and a table-row anchor, the head of the longest mention chain, one of the large documents, the session ids for the agent activity read, the `model` value on every vector and the row whose embedding is the query vector to bind.

Measurements are taken with durability on and memory pinned, and every number is recorded beside those settings:
`fsync=on`, `synchronous_commit=on`, `full_page_writes=on`, and a `shared_buffers`, `maintenance_work_mem` and `effective_cache_size` the run names.
A cluster with the write settings off leaves no page all-visible, and a small `shared_buffers` decides a share-of-plan reading on its own.
A full run at `--scale 1` takes about an hour and twenty minutes and leaves a database of about 14 GB.

**What the seeded documents do not carry.**
The seed writes `artifact_versions`, the block index and the reference rows, and it does not build the ygo document state that a version written through the API carries.
Two things follow, and a measurement has to work around both:
an upload over a seeded document (`POST /api/v1/issues/{key}/artifacts` under its name) answers `404`, where an upload over a document the API itself made answers `201`;
and `GET /api/v1/issues/{key}/artifacts/{slug}/text` answers an empty markdown for one, however large its latest version is.
`dispatch census` and every statement this tool exists to measure read the rows and are unaffected.
A write-route measurement therefore creates its own document through the API on the seeded database and writes over that;
the triggers a version insert fires, which is what such a measurement is about, do not depend on who wrote the document's earlier versions.

## Running locally

Run `bun run dispatch:local` at the repository root.
It builds this server from the tree, runs it in cookie identity with the dev sign-in route, on a seeded database, and prints the URL that opens the dashboard signed in;
`packages/dispatch/README.md` ("Local instance") documents it.

The default listen address is `:8766`.
Set `DISPATCH_LISTEN_HOST` and `DISPATCH_PORT` to change it; `DISPATCH_LISTEN_HOST` may be an IPv6 literal with or without brackets (`::1` or `[::1]`).
A bad `DISPATCH_PORT` refuses the boot before anything connects, and an address the server cannot bind ends the process with exit status 1.

A test harness that holds the port instead hands the server a listening TCP socket it opened on port 0, so no other program can take the port the kernel chose before the server serves on it:
`DISPATCH_LISTEN_FD` is the descriptor number the server inherits it as.
The server then binds nothing.
It serves on that socket, and `DISPATCH_LISTEN_HOST` and `DISPATCH_PORT`, when set and not empty, must name the socket's address (`DISPATCH_PORT=0` never does).
A descriptor that is not a listening TCP socket, or a setting that names another address, refuses the boot, and nothing is bound in its place.
`DISPATCH_SERVER_URL` is not derived from the socket.
Either way the server logs the address it serves on once it serves, as `dispatch: listening addr=<host:port>`.

### Profiling

Set `DISPATCH_PPROF_PORT` to a port to serve the Go profiler (`net/http/pprof`) on `127.0.0.1:<port>`, on a mux of its own, apart from the server's routes.
It binds the loopback address only, so nothing outside the pod reaches it; unset or empty, nothing listens.
`0` serves it on a port the kernel chooses, which the server logs as `dispatch: pprof listening addr=127.0.0.1:<port>`: a test harness asks for that, since a port it picked and let go of is any program's to take before the server binds it.
Any other value that is not a port 1-65535, or a port already taken, refuses startup naming the variable.
The profiler binds before anything connects, so a slow boot can be profiled too.
The listener closes at once when the process is told to stop, cutting off a profile in flight, so it never delays the shutdown (and the shutdown's own drain cannot be profiled).
A `gcp:Dispatch` deployment turns it on with the component's `pprofPort` input (`packages/envoy/infra/README.md`), which sets the variable on the `envoy-dispatch` container;
setting it by hand on the Deployment rolls the pods and is reverted by the next update.
To take a 30-second CPU profile of a pod:

```sh
kubectl port-forward <pod> <port>:<port>
go tool pprof -seconds 30 http://127.0.0.1:<port>/debug/pprof/profile
```

`/debug/pprof/` lists the other profiles (`heap`, `goroutine`, `allocs`, `threadcreate`, `trace`).
`mutex` and `block` are listed too but record nothing: Dispatch sets no mutex or block sampling rate.

The same port serves the process's `expvar` variables at `/debug/vars` (Go's `cmdline` and `memstats` among them), and `dispatch_rooms`, the document rooms' figures, computed afresh on each read with no database query (`docs.Service.RoomFigures`):

```sh
curl -s http://127.0.0.1:<port>/debug/vars | jq .dispatch_rooms
```

- `resident_rooms`: the rooms held in memory, which with the rooms sockets being admitted reserved are what `DISPATCH_MAX_LIVE_ROOMS` caps.

- `room_records`: the room records the document service keeps.

- `connections`: the browsers connected to rooms and the sockets being admitted to one, the count `DISPATCH_MAX_DOC_CONNECTIONS` caps.

- `pending_updates`: the browser updates the rooms' persistence has yet to take.

- `pending_update_bytes`: the bytes of the updates the rooms' queues hold.

- `durable_appends`: the updates whose append has not landed.

- `append_batches`: the append transactions of browser updates committed since the process started, an update stored alone included.

- `appended_updates`: the browser updates those transactions stored.

- `append_round_trips`: the round trips those transactions sent from the document's lock to its release, failed ones included.

- `batch_sizes`: how many committed batches stored how many updates, by bucket (`1`, `2`, `3-4`, … `129-256`).

- `pools.room_appends`, `pools.rooms`: each pool's connections held (`acquired`) and idle (`idle`), the acquires that found no idle connection (`empty_acquires`), and the time acquires waited in all (`acquire_duration_ns`); zeros for a pool not opened yet.

- `read_cache`: the cold reads' cache's weight in bytes (`bytes`), its entries (`entries`) and its budget (`budget`).

- `preloads`: the cold sockets' loaded states waiting for their room's load.

- `resident_bytes`: the encoded bytes the resident rooms took, each load and every update since: loaded state only.
  It leaves out the updates a room parks after it loaded, while they wait for an edit it has not received;
  parked rows already stored are part of the history a load decodes, so they count.
  ygo bounds those per room by `MaxPendingItems`, which Dispatch sets to `maxUpdateItems` (1<<20, `internal/dispatch/docs/service.go` `New`):
  a count of parked items and, separately, of parked delete ranges, not bytes.

- `settlement_wakeups`: the wakes of the waits for a room's appends.

## The live socket behind a proxy

A page's live updates ride one WebSocket per tab, `GET /ws/live`, over HTTP/1.1.
A browser pools WebSockets apart from its six HTTP connections per host, so a socket that stays open for its whole age never holds one of the slots ordinary requests need (`docs/solutions/dispatch-web/a-never-ending-http-request-from-a-browser-page-pins-chromes-six-connections-once-its-cert-verifier-updates-INFRA-258.md`).
That holds only while the front proxy the browser reaches (the load balancer or ingress) does not advertise HTTP/2 extended CONNECT (RFC 8441, `SETTINGS_ENABLE_CONNECT_PROTOCOL`).
A browser that sees it runs the live socket as a stream of its HTTP/2 session instead, a session the browser marks going away stays open while that stream lasts, and the INFRA-258 hang can return.
`bun scripts/e2e/lib/h2-settings.ts https://<dispatch>` checks it:
it prints the HTTP/2 SETTINGS the proxy sends as one JSON line, and exits 1 when they advertise extended CONNECT, 0 when they do not or the proxy negotiates no HTTP/2, and 2 when it gets no answer (`scripts/e2e/README.md`).
Run it after any change to the proxy.

## Checking a deploy

After a deploy, check through the front proxy that the live socket, the keyed write path and the two keyed sends work.
Run the commands from a checkout of this repository with `bun install` run, each against `https://<dispatch>`, the deployment's dashboard origin, and the two sends from a browser signed in to it.
`<token file>` holds an agent token (the shared one, or a personal `dsp_` token from Settings → Agent tokens), and `<scratch issue>` is the key of an issue the checks may leave comments on.

1. `bun scripts/e2e/lib/h2-settings.ts https://<dispatch>/` exits 0: the proxy does not advertise extended CONNECT (the section above).

2. `curl -s https://<dispatch>/healthz` prints a body with `"idempotency_keys":true`: this build judges `Idempotency-Key` (Keyed writes, below).
   The field is a fact of the build, so a `503` for a database or NATS that is down carries it too;
   a body without it is a Dispatch that would make a retried write twice, and the `dispatch` command sends a failed write again under its key and asks nothing first, so that build must not serve.

3. `bun scripts/e2e/lib/dispatch-keyed-comment.ts --url https://<dispatch> --bearer-file <token file> --issue <scratch issue>` posts one comment twice under one `Idempotency-Key`, as a page whose first answer was lost asks again.
   It passes when the repeat is Dispatch's stored answer and the issue holds the comment once:
   it prints `{"post":2,…,"replayed":"?1","status":201}`, `{"comments_with_body":1}` and `{"ok":true}`, and exits 0 (`scripts/e2e/README.md` lists every line and failure).

4. The same command with `--cli` has this checkout's `dispatch comment` post it, through a proxy on `127.0.0.1` that hands the command a gateway's `502` page in place of Dispatch's answer to its first POST, so the command asks again under its own key.
   It passes when it prints two `proxy` lines for that POST under one `key`, the first `"answered":502` and the second `"replayed":"?1"`, then `{"cli":{"code":0,…,"stdout":"Posted comment <id>"}}`, `{"comments_with_body":1}` and `{"ok":true}`, and exits 0.

5. A page opened while a write commits shows it (INFRA-292).
   Save the `Cookie` request header a browser signed in to the deployment sends (the developer tools' Network panel, any request to `https://<dispatch>`) to `<cookie file>`, and run `bun scripts/e2e/lib/dispatch-live-events.ts --url https://<dispatch> --cookie-file <cookie file> --from-whoami --subscribe-after 30 --until <marker> --timeout 120`.
   It reads `GET /auth/whoami` as you, prints `{"whoami":{"event_head":"<id>","login":"<you>"}}` and `{"subscribing_in":30}`, and only then opens `/ws/live` from that `event_head`, as a page resumes its first subscription.
   Inside those 30 seconds, comment on `<scratch issue>` with `<marker>` in the body: the comment commits after whoami answered and before the socket opens.
   It passes when it prints the comment's `{"event": …}` line and exits 0; it exits 1 when the 120 seconds end first.
   Delete `<cookie file>` afterwards: it signs in as you.

6. `bun scripts/e2e/lib/dispatch-live-events.ts --url https://<dispatch> --bearer-file <token file> --until <marker> --timeout 4200` holds one `events` subscription on `GET /ws/live` for up to 70 minutes, longer than the 30 to 60 minutes a socket lives, under the bearer the file holds.
   It prints one JSON line per frame: `{"event": …}` for each event, `{"close": <code>, "reason": "…"}` when the socket closes, `{"resubscribed": "<id>"}` when it opens the next socket and resumes after the newest event it holds, and `{"duplicate": "<id>"}` for an event it already printed.
   Once a `{"close":4001,"reason":"max age"}` line and the `resubscribed` line after it have printed, comment on any issue with `<marker>` in the body:
   the probe exits 0 when that event arrives on the new socket, and 1 when the 70 minutes end first (2 is a usage error).

7. A send across a reload: on the Agents page, open a live agent's row, set the browser offline (the developer tools' Network panel), send a message, and reload the tab, confirming the browser's prompt to leave.
   Within five minutes set the browser online and open the Agents page again in the same tab.
   The message lands: the row shows it once and the agent receives it once.

8. A broadcast across an outage: tick two live agents, set the browser offline and press Send.
   After 30 seconds the send's row reads `Still sending to 2 agents — Dispatch has not confirmed it
   yet. It keeps trying, and the broadcast is made once.` with **Stop trying**.
   Set the browser online: the page opens the broadcast, which lists each agent once with one delivery.

## Running more than one process

Two `envoy-dispatch` processes serve one Postgres and one NATS, so a release rolls instead of taking Dispatch away from everyone while it deploys.
A rolling update is supported, under two preconditions; the server checks the first itself.

**Every process is given the same `DISPATCH_SIGNING_KEY`.**
A session cookie, or the sign-in's state cookie, minted by one process is presented to whichever process the next request reaches, and every stored refresh token is sealed under a key derived from the current key (Identity, above).
Unset, each process mints a key of its own in its own data directory (`cmd/dispatch/main.go`, `auth.LoadOrCreateSigningKey`), which no other process holds.
The server refuses that through the registry of live server processes (`internal/dispatch/processes`, migration `0135`):
once a process has bound its port, and before it serves, it registers the fingerprint of each of its keys (none reveals its key), and it refuses to start, logging `msg="dispatch: register this server process"` at ERROR with an error that names the other process and the rotation steps, and exiting 1, when a live process's key set does not share its own both ways:
the other's current key among its keys, and its current key among the other's.
The refusal stands only beside a process that stays live:
refused, a process logs at WARN which live process refuses it and waits up to one live window (15 seconds) and a heartbeat for that process to stop heartbeating, trying again every heartbeat, before it exits 1, serving nothing meanwhile (a SIGTERM or Ctrl-C during the wait is a clean stop:
the process logs `msg="dispatch: stopped while
waiting for admission to the registry of live server processes"` at INFO and exits 0).
So a restart after a kill, whose dead process's row still counts as live, starts once that row lapses, while a live process, heartbeating every 5 seconds, keeps refusing.
`DISPATCH_TEST_PROCESS_HEARTBEAT` replaces those 5 seconds for an end-to-end test, the live window staying three beats of it, and a deployment never sets it.
A process with a minted key therefore refuses beside any live process, and a process refuses beside it;
processes that share one data directory share its key and start.
A pod rolled with a changed secret beside pods that hold only the old key refuses the same way, so the roll stalls on the new pod while the old ones keep serving.
Each step of a rotation (Identity, above) passes the check beside processes still on the step before.
A process whose heartbeat lapsed (frozen, or cut from the database, for longer than the live window) or whose row was pruned drops out of the registry while it runs, so a process started meanwhile is checked without it;
the first one checks its keys again before it counts as live once more, and beside a live process whose keys it does not share it logs `msg="dispatch: this server process no longer shares its signing keys with a live server process"` at ERROR, stops serving as a SIGTERM stops it (the listener closes at once, the requests in flight are answered, its row is deleted), and exits 1.

**Every release keeps the build before it working** (Two builds at once, below):
a rolling update runs the old build and the new build against one database and one NATS at once, and the page and the `dispatch` command of either build reach either process.
A release is an exception to it only where this document says so, and each exception is named here or in the section that owns it.
Each crossing before this rule is named where it happened rather than counted here:
the migrations the Database migrations section below marks as deployed with no server of the earlier release running, which a deployment still on a release before them crosses with Dispatch stopped;
the release that introduced the process registry (`server.started`, below), deployed with no process of the release before it serving beside it;
and the release that moves live updates to `/ws/live` and keys every write (INFRA-258, immediately below), which deploys with no server of the earlier release running because of its API.
An exception still to come rather than past is named where it belongs too: an upgrade of ygo that changes its update encoding (Two builds at once, Stored documents, below), which no build can roll through because the build that cannot read the new encoding is already serving when it arrives.
Turning `DISPATCH_PARENT_STATUS_RULE` on is a crossing of its own, which a setting makes rather than a release (below).

**A build keeps the earlier build's requests answerable while both serve** (Two builds at once, below, states this surface by surface).
A route either build serves answers a body either build sends: a member one build does not declare is ignored rather than refused, so a request still does what it asks for, and a member whose loss would change what a request does ships as a route of its own instead.
A route is added freely and removed only in the release after the one that stops calling it, and a page or command that reaches a build without a route it wants is answered `404 NOT_FOUND` with `no route for <METHOD> <path>`, which it retries so that the next attempt can reach the other build.
So the two surfaces a client crosses a roll on, the body it sends and the route it sends it to, are the earlier build's as much as this one's for the length of the roll.

**The release that moves live updates to `/ws/live` and keys every write (INFRA-258) deploys with no server of the earlier release running**, because of its API rather than its schema.
An earlier server ignores the `Idempotency-Key` header and keeps no key, so while both serve one database a write can run twice:
one an earlier server ran whose answer was lost is run again by its retry on a new server, which holds no key for it, and a retry of one a new server ran runs again on an earlier one.
The page and the `dispatch` command both retry a write whose answer was lost, under its key, and ask nothing first.
Stop the earlier release, or scale it to zero, before this one serves:
a Kubernetes Deployment's `strategy: Recreate`, which `envoy-infra:gcp:Dispatch` uses at its default one replica;
a deployment running more than one replica, which rolls, is scaled to zero first, as for `0117` (Database migrations, below).
The compose auto-deployer stops the old container before it starts the new one.
Releases after this one roll again.
A rollback to an earlier release brings the same hazard back, the other way:
a tab still open, and every `dispatch` client, which sends a failed write again under its key and asks nothing first, retry a write whose answer was lost against the earlier server, which runs it again.
Fix forward instead; if you must roll back, stop this release first, knowing a write in flight at that moment may land twice.
A rollback to a build whose keyed layer passes a caller it cannot resolve on to the handler, rather than answering that caller itself, brings the double write back too, for every `dispatch` client that sends a failed write again with no `/healthz` read (`packages/envoy-client/README.md`), for as long as that build serves:
that build can run a keyed write with no key, so a write whose answer was lost, sent again under its key, can land twice.
Once a deployment's sessions run such a client, do not roll Dispatch back past `b1c42d1d7d` (the first build whose keyed layer answers that caller itself, cybermonklabs/legion#4350); fix forward instead.
Once `0141_drop_broadcast_idempotency_keys` has applied, a rollback past this release also loses broadcasts:
the earlier build's broadcast send reads and writes the table `0141` drops, so every broadcast send fails while the rest of that build keeps working.
A tab opened before the deploy needs a reload:
`GET /api/v1/events` is gone, so it shows `Live updates unavailable`, and its broadcast body still carries `idempotency_key`, which this release refuses by name (`400 INVALID_JSON`, "idempotency_key is the Idempotency-Key header, not a body member"), so that tab sends no broadcast until it reloads.

**Turning `DISPATCH_PARENT_STATUS_RULE` on (INFRA-4731) deploys with no process of the earlier setting serving**, because of the setting rather than the schema or the API.
With the setting on, a process refuses a write that leaves a parent's status out of line with its children's (`409 PARENT_STATUS_CONFLICT`, Routes, below), and its boot runs the one-time fix of the pairs that break that rule (below).
A process with the setting off still takes such a write after the fix ran, and the fix does not run again.
So no process of the earlier setting may serve once a process with the setting on serves, whether the deploy changes the setting or first brings an image that reads it.
Turn the setting on only when two conditions hold.
First, each Legion daemon that writes to this Dispatch runs the releases with the daemon's side of the rule, which send the options of `POST /api/v1/issues/{key}/status-change`.
Second, each project's instructions retry a failed root architect with a status write of `todo` alone, not `backlog` and then `todo`, and that project's daemon and controller have read them.
The rule refuses that `backlog` while an issue under the root is in work.
Stop every process of the earlier setting, or scale it to zero, before a process with the setting on serves.
At its default one replica, `envoy-infra:gcp:Dispatch` does that with a Kubernetes Deployment's `strategy: Recreate`, which stops the old pod before the new one starts (its input `parentStatusRule`, `packages/envoy/infra/README.md`).
A deployment that runs more than one replica rolls, so scale it to zero first, as for INFRA-258 above.

**The release that stores a message for a stopped session (INFRA-626) rolls, so for the length of the update an earlier server and a new server both take a model or thinking switch.**
A switch is the live view's one-line `/model <value>` or `/effort <value>` message, and a new server sends it with `connected_only` whatever its request says when it is a person's own Send or Aside, not one an agent wrote for them, on no issue and no broadcast's copy, the shape the session runs as its own turn (`isSessionSwitch`, `api/messages.go`), so a stopped session refuses it and its attempt records `the session is stopped`:
stored for a stopped session, the line would change that session's model at an unknown later time.
Every other message whose body is such a line - an issue message, a mention, a broadcast copy, a BTW, a session's message, one an agent wrote for its person - reaches the session as a card and changes no model, so a new server queues it for a stopped session like any other message.
A tab opened before the deploy sends the switch without `connected_only`, and a new server holds it to a running session by that rule all the same, so that tab needs no reload for it.
An earlier server that takes the same switch, until the update ends or for good after a rollback, sends it as that release always did:
its registry read does not find a session the listener knows only by its record, so it fails that attempt `no live session`, and it sends the switch to a session whose registry entry still stands, for which the listener stores it if that session's process has gone, as it did before this release.
A tab opened after the deploy sends `connected_only`, which an earlier server ignores, as `decodeJSON` ignores every member a route does not declare (it logs the member and decodes the rest).
So that earlier server sends the switch as that release always did (above), and the page shows no refusal.
After a rollback every press is sent that way until the deployment moves forward again.
An earlier server's delivery sweep resumes an attempt without the flag, since that build does not read the column, so it can send a switch attempt a new server left pending the way the earlier release sends one (above).
That takes a new server killed mid-send, whose drain therefore never settles the attempt, while an earlier server still serves, in a rolling update or a rollback.
The earlier server's sweep can then send that attempt once its claim lapses, one to two minutes after the kill (`model.DeliveryClaimLapse`, and the sweep's one pass a minute), unless a new server's sweep takes it first and sends it with the flag.

What the two processes share, and what each keeps to itself:

- One process publishes the events outbox at a time.
  Each process's outbox reads and sends events only while it holds the publisher hold `dispatch-outbox` (`duty.Hold`, migration `20261101143201_duty_holds`), renewed every second on the outbox's own pool, and stands by while the other holds it;
  `/healthz` says which (`outbox_publisher`).
  A holder that stops sends nothing more, records what NATS already answered, releases the rest of each batch in flight and then the hold, which wakes the standby at once;
  a graceful stop waits for that before it closes the database pool, up to `outboxStopBudget` after its shutdown starts.
  One that is killed is replaced once its hold lapses, six seconds after its last renewal;
  one whose NATS stays down for three seconds, or whose failed sends meet a connection that does not answer a probe, releases the hold too.
  One that finds its hold lost releases it as well, which stops its renewal and deletes the row only while the row still names it.
  One that is alive but whose renewal is not confirmed within four seconds of the last one that was stops publishing at that moment, and no standby may take the name while its row stands: until its release, right after the line that ends the turn, deletes the row, or until the row lapses two seconds later when that release cannot reach the database first;
  the line that ends the turn carries `stopped` and `stopped_by`, the moment it stopped bounded from below and from above rather than dated by the moment the line was written, and one value, the hold's deadline, whenever the turn ended there;
  the line that begins it (`took the publisher hold`) carries `started`, read before the process began publishing.
  A holder sends nothing once its hold's deadline has passed, a send already waiting for a connection included (`bus.Client.WindowUntil`), so one that freezes and resumes publishes nothing beside the new holder.
  Each owner's events (an issue's, a document's or a project's) keep their order per destination:
  an event goes to a session's subject, or to the owner's topic family, only after every earlier unpublished event of its owner bound there has reached it.
  The owner's topic family comes first beside that, because a session that follows the topic and is also sent the owner's events on its own subject takes both in stream order:
  no later event of the owner goes to any session's subject while an earlier one's topic send has not reached the topic family.
  So while an owner's topic family cannot be published, that owner's sends to session subjects wait too, until the retry that publishes the topic send, and a session that gets nothing from the owner may be waiting on the topic family rather than on its own subject.
  A failed send to a session's own subject holds back only that subject, and a mention's copies (the API's mention send and a fallback row) are outside this order.

- A batch does not block the next.
  The holder runs up to four side by side, each reading only owners no other batch holds in flight, so an event waiting out the five-second wait for the stream's acknowledgement of a role forward holds back its own owner's later events and nothing else;
  every other owner's events are read, sent and recorded beside it.
  An owner is settled, recorded and given back as soon as its own sends are answered rather than at its batch's end, so its next events follow at once.
  The batches run one statement at a time on the outbox's two connections, so the hold's renewal never waits behind them.

- Each event reaches each receiver once, delivered at least once.
  A holder that dies between NATS's acknowledgement and the write that records it leaves the batch to be sent again by the next holder:
  JetStream drops the repeat by its MsgId inside its 72-hour duplicate window, and a role's holder, to which the outbox forwards a role-routed event itself, drops a repeat forward by its dedupe key.
  Dispatch publishes nothing on `notifications.role.*`.

- A reader's events subscription on its live socket carries the other process's events.
  It is re-sourced from Postgres through a per-process relay on `listen dispatch_events` (migration `0107_events_notify`), so an event committed on either process reaches every open subscription with no reconnect.

- The two delivery durables keep their shape, and each delivers to one process at a time:
  the process that holds that durable's hold (`duty.Hold`, migration `20261101143201_duty_holds`, a row with a 15-second lease on the database's clock, renewed every five seconds).
  The other processes stand by and try for the hold every two seconds.
  A process that stops hands back every message it was delivered and did not handle and releases the hold, so another process takes the durable over at once and is redelivered those messages within seconds;
  a process that dies is replaced within the hold's 15 seconds and one try.
  A take-over recreates the durable at its ack floor on a fresh deliver subject, which fences the old holder off:
  a subscription its client sends again after a reconnect gets nothing.
  It runs whole under that durable's own Postgres consumer lease, which the reconciliation of the build before holds takes too, so no two processes change one durable at once.
  A durable bound by a build before holds, which binds by lookup and holds nothing, is left to that build until it lets go (`delivery/held_durable.go`, `heldDurable`).

- A delivery a process left behind is sent by another.
  A direct message's first attempt is written in the transaction that stores it, and a request's own send outlives a cut connection.
  An attempt a process left pending - killed between its claim and its settle, a direct message, a broadcast recipient's, a comment mention's or a Retry's alike - is sent by whichever process runs the delivery sweep next (`api/delivery_sweep.go`, `resumeOwedDeliveries`, a pass of the maintenance loop below, once a minute), once its claim is a minute old, under its own attempt number and idempotency key, so the session is handed it once.
  An attempt older than `DELIVERY_DUPLICATE_WINDOW_MS` (72 hours), past which a resend is no longer recognised as a repeat, is left for a person's Retry, and a mention attempt nothing can send - its mention gone, its document gone - is settled `failed` with that cause instead.

- A send waits out a listener take-over.
  While the active Envoy listener pod is replaced, its successor refuses connections, answers 502, 503 or 504, or does not hold the session yet;
  a send that meets one of those (or a receipt timeout, or a 503 coded `maybe_stored`, whose publish the stream never acknowledged) is tried again under the attempt it holds and the same idempotency key, renewing its claim before each try, for up to 12 seconds from its first try, and only then settles `failed` (`api/envoy_resolve.go`, `listenerTakeoverWindow`).
  A request that sends during a take-over answers up to 12 seconds later, and a mention of a session that is really gone reads `failed: no live session` about 12 seconds after it was made.
  A targeted message to such a session is queued instead at that moment:
  when neither the listener's registry nor its known-session store holds the session, Dispatch publishes the message on the session's own subject, `notifications.agent.<session>`, under the dedupe key the listener's send would have used, and records the attempt `queued` (`api/message_queue_publish.go`).
  The listener gives that copy up after its retry window with the agent wake, and the session reads it when it registers again under its id within the stream's 72 hours.
  A process with no NATS (`DISPATCH_NATS_DISABLED=1` without test hooks), a failed publish, a role target, a send whose request set `connected_only`, and a model or thinking switch, which is sent `connected_only` whatever its request says, still settle `failed`.
  A process killed during the wait leaves the attempt to the delivery sweep.

- Times that one process writes and another compares come from the database's clock, never a host's:
  an issue's `claimed_at`, an ask's answer time (the answer route's and a carried approval's, the `created_at` of the `ask.answered` event beside it) and the "snooze must be in the future" check, as the delivery claims' lease, the route roster's age and the GitHub rate limits are.

- Route status and the agents list come from one stored listener answer.
  Every issue read judges its `route_status` against the `route_roster` row (migration `0159`), the listener's last live-session list, and `GET /api/v1/agents` lists that row's sessions, so both processes read a route the same way, list the same agents, and a `route_status` page read on either is cut from the same answer.
  A process that finds the row older than five seconds by the database's clock asks the listener once, however many of its reads wait, and stores the answer, unless the other process stored one from a later call;
  a listener that did not answer is stored too, so every process reads `unknown` and answers the agents list `503 ENVOY_UNAVAILABLE` for the next five seconds together (`api/issue_route_status.go`).
  A refresh of the answer between two page reads can still move issues across a page boundary, as any change does under offset paging.

- GitHub's rate limits hold every process.
  GitHub budgets the App and each installation, not a process, and warns that requests sent while limited may get the integration banned, so a limit one process meets is recorded in `github_rate_limits` (migration `0160`) and every process reads it before it sends GitHub anything (`githubapp.Client.request`, `githubapp/ratelimit`).
  Scope `app` holds the calls the App JWT signs (installation lookups, token mints, the webhook delivery endpoints) and `installation:<id>` the calls under that installation's token, each for the wait GitHub gave, by the database's clock.
  While one is in force a call under it fails at once with no request:
  the GitHub proxy answers `503 GITHUB_RATE_LIMITED` with `Retry-After` (Routes, below), a delivery intake read fails and is acknowledged as any failure is and the five-minute reconcile catches up once the limit ends, and the webhook redelivery sweep stops (Webhook redelivery, above).
  A database that cannot be read or written leaves each process to GitHub's own answers, with a log line.
  Installation tokens stay in each process:
  GitHub budgets the installation, not the token, and storing tokens would put live credentials in Postgres and its backups.
  A repository's installation is remembered for ten minutes, so every process finds an App moved to another installation within that.

- No leased background loop runs a pass beside another process's pass of the same loop.
  Each pass takes a named session-level advisory lease for its length (`duty.Exclusive`), so a process whose attempt lands inside another's pass finds it held and logs a skip.
  The thirteen timer loops — the delivery reconcile, the architecture import, the webhook redelivery sweep, the CI reconcile pass (lease `ci-reconcile`, beside the redelivery sweep in what it recovers), the hourly document compaction sweep (lease `document-compaction`, first tick at start, with or without NATS), the block index reconciliation, the term-statistics refresh, the repair of closed and reopened issues' asks, the keyed writes' maintenance sweep (lease `suggestion-outcomes`), the document settlement resumption, the prune of the registry of live server processes, the Legion sampler's read of each project's daemon (lease `legion-state`, first tick at start, one pass a minute) and the display tree fill (`texttreefill`, lease `text-tree-fill`)
  — also record each pass in `duty_passes` (migration `0115`), in a row per lease and period, so a process whose tick lands between another's passes
  — the ordinary case, since each ticker draws its phase at boot and a rolling update starts the second process minutes after the first
  — finds the period already served and logs the same skip: each runs once per period across the processes.
  A row per lease and period is what keeps two builds whose interval for one loop differs from each running on every tick after the other's pass: the processes at one period make one pass per period between them, whatever the processes at another period do.

- The two leased woken loops, the reference excerpts and the edit notices, take their lease with no period:
  a process that finds it held tries again until a pass of its own runs, since the other process's pass may have read before the change that woke this one.
  The suggestion outcome sweep takes no lease at all:
  each of its pages holds the `suggestion_outcome_cursor` row `for update`, which serialises two processes by itself (Background loops, below).
  The settlement resumption is leased because a settlement reads the store under the document's lock whichever process runs it, so one process per minute arms every owed settlement.

- The embeddings `project_key` fill (lease `embeddings-project-key-fill`, `api.RunEmbeddingProjectKeyFill`) is leased and is no loop of a cadence: it is one pass of one to two hours that labels the rows stored before migration `20261109120000`, started once at boot, and it takes its lease with no period because the cursor row of `20261109120100` says when there is nothing left to do, so the process of a roll that finds the lease held logs the skip and fills nothing.
  That process then tries for the lease again every five minutes until that cursor row reads done, so a holder killed mid-pass costs the labelling one wait rather than lasting until something next boots, and each process's goroutine and its lease-pool connection go back for good once the rows are labelled.
  Twenty names are leased in all: the fourteen leased loops of the two items above, the CI verify loop (`ci-verify`, taken only by a verify pass that lists a request to answer) and the Legion sampler, the CI reconcile among them, one per delivery durable for the intake's take-over, the fill, and the boot's one-time parent status fix (lease `parent-status-fix`, `api.RunParentStatusFixAtBoot`), and the lease pool holds a connection for each of those callers, for each of the four callers of the two intakes' holds and for each of the two callers of the keyword fill's hold, twenty-six (`store.LeasePoolSize`'s comment in `internal/dispatch/store/pool.go` lists the names; every caller reserves its connection by name at boot and `store.LeaseCallers.Complete` refuses to serve unless the reservations are exactly the pool's size).
  The delivery live hub, the event-stream relay, the process's shared listener connection, with the document rooms' room sync and the woken loops' wakes on it, and the document rooms' timers stay per-process by design (below), the embedding queue and the task progress reconciliation claim their own work, the outbox publishes under its hold (above), and the keyword fill writes under a hold of its own (Background loops, below);
  the embedding queue's coverage pass runs in every process with no lease, each of its steps safe beside another process's, and only the processes whose embedder has the one active identity embed or drop a vector (Search, below);
  `cmd/dispatch/main.go` says at the call site which loop is which and why.

- The parent status fix runs once per database, at boot, before the server listens.
  With `DISPATCH_PARENT_STATUS_RULE=on`, the boot runs it after every caller of the lease pool has reserved its connection, and before it starts the events outbox, any background loop or the HTTP listener (`api.RunParentStatusFixAtBoot`, `cmd/dispatch/main.go`).
  So the process takes no write, and no loop writes, until the fix ends.
  One process takes the lease `parent-status-fix` and runs the fix: it unlinks each open issue from its `done` parent, then moves each not-started ancestor of an issue in work to `in_progress`, the highest first.
  Each change is one write through the `PATCH`'s write core with the rule on, as Dispatch's system actor (`parent-status-rule`), with a comment on the issue it changes.
  That process then writes the record of the fix, the one row of `parent_status_fix` (Database migrations, below).
  A process that finds the lease held waits for the record: it reads the record again every second, logs `msg="dispatch: the boot waits for the parent status fix, which another process runs under its lease"` at INFO every 15 seconds, and serves only once the record is there.
  A boot that finds the record runs nothing, also after the setting was off for a time, so turning the setting off and on again runs no second fix.
  A SIGTERM or Ctrl-C during the wait is a clean stop: the process logs `msg="dispatch: stopped while waiting for the parent status fix"` at INFO and exits 0.
  A change that fails ends the boot with exit 1 and no record, and the next boot plans the fix again from what stands.
  Each change and its comment commit together, so no issue is changed or commented twice.
  With the setting off, the boot reads and writes nothing for the fix, and the fix's connection of the lease pool stays reserved, so the process holds the same count of connections either way.
  Since no second fix runs, a parent and a child that came to break the rule while the setting was off still break it once the setting is on again.
  The rule judges each status change of such a child against its parent, a move within a group too (`parentStatusConflictOf`, `api/parent_status_rule.go`), so a daemon's move of the child from `in_progress` to `testing` under a `done` parent is refused with `409 PARENT_STATUS_CONFLICT`.
  Each such move is refused until a person takes the child off that parent, with `dispatch issue-update <child> --parent ""` or a move under another parent, or closes the child.
  `--unlink-children` on the parent does not do it: the option goes with a status, `done` on the closed parent is `409 ISSUE_CLOSED`, and any other status reopens the parent.

- The delivery live hub, the event-stream relay and the events outbox run in every process, each on a `LISTEN` connection of its own (`api/delivery_live_hub.go`, `api/event_stream_relay.go`, `outbox/lease.go`), and the document rooms' room sync shares the process's listener hub with the woken background loops (`docs/room_sync.go`, Background loops, above).
  Each process serves its own browser sockets - the Delivery and Pipeline pages' `GET /ws/delivery`, every SSE stream, every live document - so each needs the notifications and the recomputed views for them, and sharing them would need a cache across the processes.
  The cost is up to five Postgres connections per process outside the shared pool (`store.listenerConnections`):
  those three, the shared listener hub and the room sync's NOTIFY sender, the hub's and the relay's held only while something subscribes to them.

- A Delivery page configuration warning is logged once across the processes.
  Every read judges each deploy run, and a successful run holding none of `production_job_names`, or whose production job lists no step named `production_apply_step_name`, is named in the log;
  the process whose insert into `delivery_run_warnings` (migration `0161`) writes the run's row logs it, and every other process finds the row and stays quiet.
  A database that cannot record the row leaves the process to log it anyway.

- A test hook acts on every process (`DISPATCH_TEST_HOOKS=1` only, so never in a real deployment).
  A browser suite against two processes holds its page's stream, delivery socket, document and conversation viewer on whichever process the page reached, and calls a hook on whichever its request reaches.
  So each process with the hooks on listens on `dispatch_test_hooks` on a connection of its own, holding on it a session-level advisory lock keyed by that connection's backend pid, which dies with the connection;
  a hook call notifies every process, each acts on itself and acknowledges, and the call answers once every process that held a lock when it was sent has acknowledged or died, or `504 TEST_HOOK_TIMEOUT` after ten seconds naming the lock keys that never answered (`api/test_hooks_fanout.go`).
  `quiesce` therefore answers only once no process has a document open.
  A published frame larger than one notification's 8000 bytes travels in several, sent in one transaction.
  That connection is one more per process outside the shared pool, under the test hooks alone.

- The registry of live server processes is shared.
  Each process stamps its `server_processes` row every 5 seconds, on a one-connection pool of its own, and counts as live while that heartbeat is within 15 seconds of the database's `now()`;
  it deletes its row once its HTTP drain has ended, so a draining process counts as live until its last request is answered.
  A row a process left behind (killed, its node lost) counts as live until 15 seconds after its last heartbeat, so a process refused beside it at boot waits up to that window and a heartbeat for it to lapse before it exits 1 (the signing key precondition, above);
  the prune loop deletes such a row 10 minutes after its last heartbeat, under the lease `server-processes`.
  A process whose heartbeat lapsed (frozen, cut from the database) or whose row was pruned checks its keys again, under the registry's lock, before it counts as live once more:
  it writes its row again, logging that it rejoined at WARN, when every live process shares its key set, and otherwise stops, after draining, and exits 1 (the signing key precondition, above).
  Operator subcommands never register.

- A sign-in started on one process completes on the other:
  its state travels in the browser's signed `dispatch_oauth_state` cookie, not in a process (Identity, above), given one `DISPATCH_SERVER_URL` on every process.

- `server.started` names the build every live process serves, once each time that build changes.
  A process that finds every live process serving one stamped, non-empty commit that is its own and that the latest row of `server_build_announcements` does not name inserts a row, with `tuple_kinds` the relation kinds its build writes as tuples of the node and tuple store (`refs.TupleKinds`, the kinds `graph_edges`' tuple arm renders);
  a process whose heartbeat lapsed is no live process, so the fleet it reads can agree on another build's commit, and it inserts nothing for that commit, which it would otherwise name with its own build's kinds;
  any process whose bus is connected publishes each row not yet published, with the event id `dispatch-server-started-<commit>-<seq>` and `started_at` the database's time of the announcement.
  Each process looks every 5 seconds, and an advisory lock keeps two from inserting or publishing at once.
  So a roll announces nothing until its last old process is gone, a restart of every process on one commit announces nothing, and a rollback to an earlier commit announces it again.
  A process whose NATS is down announces into the table and leaves the publish to one whose bus answers.
  Builds before this registry do not register:
  the first release that carries it announces as soon as its own processes agree, and a process of an earlier release that starts during that release still publishes its own start, so that release is deployed with no process of the earlier one serving beside it (production ran one pod with `Recreate` for it).

- Neither NATS nor an OIDC issuer gates the boot.
  A process whose NATS does not answer, or refuses its credential, starts and serves everything that does not need the bus, and each of its NATS clients connects in the background, retrying forever;
  the outbox publishes what was written meanwhile once the bus answers, the webhook redelivery sweep opens its KV bucket on its first pass with NATS connected, and the agent conversation relay answers `503 STREAM_UNAVAILABLE` until then.
  A sign-in issuer or service-account issuer that cannot be reached is discovered in the background (Identity, above);
  until then only what needs it answers `503`.
  `/healthz` reports each as `nats`, `signin` and `service_tokens`, and none of them fails the probe.

- Each build's dashboard assets are in the database.
  Every process publishes its web dist's assets at boot, before it listens (`internal/dispatch/dashboard`, migration `0136`), and answers a local miss under `/assets/` from there (Routes, below), so a page of one build served by one process is served its chunks by a process of another build.
  A process that cannot publish (the database refuses the write, or the build holds a file over 8 MiB, which CI's `bun run check:asset-sizes` in `packages/dispatch` refuses first) exits before it is ready, so a rolling update keeps the processes it would have replaced.
  A restart on a build already stored writes no bytes.
  The prune after each publish keeps every build a live server process serves and the 20 builds started most recently;
  a failed prune is logged and the boot goes on.

**Two processes share a live document session through Postgres.**
Durable convergence does not depend on it:
every edit is appended under the document's advisory lock, Yjs updates commute, and the stored history is the union of them, so no edit is lost whichever process took it.
Each process holds its own room for a document.
Postgres is the only channel between the rooms (`docs/room_sync.go`), so the message bus can be down and the rooms still sync:

- **Edits.**
  Every durable change notifies `dispatch_doc_head` (`{"r": artifact id, "v": head,
  "p": process id}`) and `dispatch_doc_changed` (the bare artifact id, which the earlier build reads) from inside the transaction that wrote it.
  Every other process that holds a room of the document reads the stored rows its room does not hold from `doc_updates` and applies them as one batch.
  The room stays open, and its browsers see the edit on the socket they already have.

- **Closes.**
  Room sync closes and reloads a room only when its stored history is replaced:
  a rebuild, a prune, a delete, or a compaction that folds rows the room has not taken in yet.
  A compaction keeps the head row, so it closes only a room that is behind at that moment.
  A browser's append that such a fold lets through closes its room at once in the same way (below).
  The service also closes a room in three other cases:
  its issue closes or reopens, the room fails and its eviction closes it (`failRoom`), or the process shuts down with an editor connected to the room.
  No other close disconnects a browser.
  After a close, the browser reconnects and syncs a reloaded room.

- **Appends from a room that is behind.**
  A browser's edit is accepted: it is stored at the head, and the room's cursor takes it in when the catch-up fills the gap.
  It is accepted too when a compaction folded the rows the room holds, because the fold keeps every item the edit builds on.
  The append that stored the edit then closes the room at once (`closeRoomOnReplacedHistory`), and its browsers reconnect to a room reloaded at the head.
  A write computed from the room's content (a settlement's repair, a live write's fork, a version's capture) still needs a room at the head (`ErrRoomBehindHead`).
  A settlement catches its room up under the document's lock and then continues.
  A version whose room is behind is rendered from the stored document (`storedTree`).

- **Cursors and presence.**
  Each process relays its rooms' awareness on `dispatch_doc_awareness` through its NOTIFY connection.
  It sends the changes of one room once per 50 ms at most, and each send carries every reader who moved within that window, at the state the room holds when it ends, so readers moving at the same moment all cross rather than the last of them:
  the relay reads each room's awareness itself (`watchRoomAwareness`) rather than taking ygo's outbound lane, which keeps one awareness blob per room.
  A room that loads for a reader, and a reader that joins a loaded room, ask the other processes for their readers' states, so a reader who opens second sees the first reader's cursor at once.
  A room forgets a client it hears nothing from for 30 seconds (`AwarenessExpiry`), so a cursor relayed from a process that stopped without a word goes away.
  A document socket may hold at most four awareness clients at once (`maxAwarenessClientsPerSocket`).
  A tab sends its own client first, and then echoes the room's other readers back on the same socket, so the other three places hold echoes:
  `@hocuspocus/provider` 2.15.3 sends every client that an awareness change touched, and applying the room's other readers makes such a change.
  The clients a frame names past the four are taken out of it before ygo applies it (`awareness_limit.go`), and a dropped echo tells the room nothing it does not hold already.
  So a tab claiming thousands of client ids neither fills a room's memory, nor shows the room's other readers thousands of cursors, nor becomes thousands of notifications.
  A socket's clients go when it does, and a client that leaves gives its entry back at once rather than two expiries later (`reclaimAwarenessSlots`), so a reader who joins after such a frame is taken in and relayed at once.
  A client's removal is relayed at the clock the relay recorded for that client, so the other processes take it rather than drop it as older than the state they hold.
  A socket whose message names neither a document nor a message type - a three-byte message as much as one that has said nothing by 512 bytes (`maxDocumentMessageHeader`) - is closed rather than read on:
  nothing this service reads looks like that.
  A room also holds about 31 MB of its clients' states (`MaxAwarenessBytesPerRoom`) and, as a last resort, `maxAwarenessClientsPerRoom` client entries - 4,000, the connections one process admits (`maxRoomConnections`, 1,000) each holding the four clients a socket may, which is the same bound another process's relayed clients passed before they crossed - so it stands above every client real readers can put in a room and refuses none of them, here or on another process.

- **Issue close and reopen.**
  Migration `20261101181500`'s trigger notifies `dispatch_issue_open_changed` when an issue's `closed_at` changes, whichever process or build wrote it.
  Every process of this build then closes its rooms of the issue's documents, and their browsers reconnect read-only or read-write as the issue now allows.
  A browser's append also reads the issue's state in its own transaction.
  The append is refused when its room took the edit more than 2 seconds (`closeMargin`) after the close committed.
  An edit that the room took before then is stored, even when its append reaches the database after the close.
  The document's next settlement saves it, whether or not the issue is closed.
  A close through `PATCH` stamps `closed_at` again as its last statement before it commits, so the 2 seconds count from the commit.
  An edit typed while the close's transaction runs is stored, however long the close takes to withdraw the issue's open asks.
  The `issue.closed` event carries the earlier stamp, from the start of the close's transaction.
  The response and every later read carry the stored one.
  A close made through the earlier build stamps `closed_at` only at the start of its transaction, so for that close the 2 seconds count from that start.

- **Questions about a room.**
  A process asks the other members about a room on `dispatch_doc_query`, and each member answers on `dispatch_doc_answer`.
  For each question, the asker waits 500 ms at most.
  Then it continues without the members that did not answer, and logs a WARN that names them.
  An agent's write asks up to three questions, so it can wait up to 1.5 seconds in all:

  1. Before the write forks, every member that holds the room stores its updates, so the write sees what a person on another process typed.

  2. Before the commit, the lost-edit check has every member share its room's unstored updates and its connected people.
     `409 EDIT_LOST_TO_CONCURRENT_CHANGE` names the people connected on this process and on every member that answered.

  3. After the commit, every member that holds the room stores its updates again, and `lost_ops` is judged on the document at the stored head.

  A rebuild asks whether any member holds the room.
  It answers `409 DOCUMENT_LIVE` when this process holds one, or when a member that answered holds one.

- **Reads.**
  A read compares the room with the stored head.
  When the room is behind, the read takes the stored document and does not wait for the catch-up, so no read returns text older than the store.

- **Ask authors and the edit queue.**
  The author of an ask block is stored (`doc_ask_block_authors`, migration `20261101181501`), so a settlement on any process names who wrote it.
  The cap of 32 queued conditional edits of one document counts the queue of this process and of every other member (`doc_conditional_edit_queue`, migration `20261101181502`).
  Each process first queues a document's conditional edits in memory and admits them one at a time, and the admitted edits take turns.
  On each process, at most three of a document's conditional edits hold a database connection at once:
  the edit being admitted holds one for its short admission transaction, the edit whose turn it is holds its write transaction, and an edit that has just left holds one to write this process's count, which it does after it hands the turn on (`releaseConditionalEdit`).
  An edit that waits for its admission, or for its turn, holds none.

- **Which process served a socket.**
  Every answer that the document service's handler sends to a `/ws/doc/{room}` request carries `X-Dispatch-Process: <host name>`: the pod's name in production.
  That includes the `101` upgrade and the handler's `401`, `404` and `503` answers.
  A check that opens sockets through a load balancer reads it to record which process each socket reached.
  A host name the process cannot read, or one that holds bytes a header cannot carry, sends no header.
  The `400` answers sent before that handler carry no header.
  They refuse a room id or a query parameter that holds U+0000 or bytes that are not UTF-8.

The processes a question waits for are the members: the processes whose listener connection is named `dispatch-listener <process id>` in `pg_stat_activity`.
Each process makes its id when it starts.
A process that stops loses that connection, and with it its membership, so nothing has to be cleaned up.
A process of the earlier build has no such connection, so it is no member.
Room sync does not take its members from the registry of live server processes (above):
a member must be a process whose LISTEN connection is up now, since only such a process can hear a question and answer it.
The registry counts a process by its heartbeat, for 15 seconds after the last one and through its listener's redials and its shutdown drain, so it would count processes that cannot hear, and each question would wait 500 ms on them.

While a process's listener connection redials, live sync to that process pauses.
The wait between attempts doubles from 1 second to 30 seconds, plus up to half again, so one wait can last 45 seconds (`pglisten.RedialBackoff`).
In that time its rooms do not catch up and do not receive the other processes' awareness, and it is not a member, so no question waits for it.
Reads stay fresh, because a read compares with the stored head.
Appends stay correct, because the append gate reads the head and the issue's state under the document's lock.
When the LISTEN stands again, the process catches up every room it holds and reads the issue state of each one again.

During the roll that releases this behaviour, processes of the earlier build behave as before:

- They still close a room that falls behind, and its browsers reconnect.

- They relay no awareness and apply none, so they show no other process's cursors.

- They do not listen on `dispatch_issue_open_changed`, so they do not act on a close or reopen made through this build.
  Their open rooms of the issue's documents keep the scope that their browsers connected with, unless the close or reopen writes the document.
  A close writes the resolved state of the issue's open asks into the blocks that show them, and a reopen writes them open again (`WriteIssueAskStates`).
  That write moves the document's head, so an earlier-build room of the document closes as a room that falls behind, and its browsers reconnect with the scope the issue now allows.

- They are not members, so no question waits for them.

Every change this build writes still notifies `dispatch_doc_changed`, which the earlier build reads, and an issue that the earlier build closes still closes this build's rooms through the trigger.

**Each process holds its own connections, so a second process doubles them.**
Every pool's size is fixed in code (`internal/dispatch/store/pool.go`), and a serving process opens at most 64 + 26 + 8 + 4 + 2 + 2 + 1 + 2 + 2 + 1 + 5 = 117 connections to Postgres (`store.ConnectionsPerProcess`):

| Pool | Connections | What takes them |
| --- | --- | --- |
| Shared (`sharedPoolSize`) | 64 | Requests, settlements, and the document writers that hold a room's lock on a connection of their own (`lockRoom` through `withRoomLock`: snapshots, prunes, deletions). |
| Leases (`store.LeasePoolSize`) | 26 | The periodic maintenance loops' once-overall leases (`duty.Exclusive`), the delivery intakes' holds and the keyword fill's hold (`duty.Hold`) with the fill's batches, the Legion sampler's read of each project's daemon, the CI verify loop's lease, the embeddings `project_key` fill, the display tree fill and the boot's one-time parent status fix among them, one per caller that can be inside a lease or a hold statement at once (`store.LeaseCallers`). |
| Room appends (`roomAppendsPoolSize`) | 8 | The document rooms' batches of browser updates; `application_name=dispatch-room-appends`. |
| Rooms (`RoomsPoolSize`) | 4 | Document loads; `application_name=dispatch-rooms`. |
| Compaction (`compactionPoolSize`) | 2 | A document's compaction: the read transaction of the rows it folds, and its write back under the document's lock (`Pool.Compaction`); `application_name=dispatch-compaction`. |
| Display tree fill (`textTreePoolSize`) | 2 | The background fill's own reads and writes (`Pool.TextTrees`, `texttreefill`): a pass reads its next batch of texts only once the batch before it is stored, and stores a batch with at most two readings in flight, so two is every connection it can use. |
| Health (`healthPoolSize`) | 1 | The `/healthz` probe. |
| Outbox (`outboxPoolSize`) | 2 | The events outbox's scan and, beside it, the renewal of its publisher hold (`duty.Hold`). |
| Horizon (`horizonPoolSize`) | 2 | The event log's safe horizon (`events.Horizon`): its observations, its marks and the reads beside them, its stall diagnosis and its persist write. |
| Registry (`registryPoolSize`) | 1 | Held for the life of the process: the heartbeat that stamps its row in the registry of live server processes every 5 seconds (`processes.Register`). |
| Listeners (`listenerConnections`) | 5 | What the process may hold at once, each on a connection of its own: the events outbox's `LISTEN` on `dispatch_events` and `dispatch_duty_holds`, the shared listener hub the document rooms' room sync and the woken background loops wait on, and the document rooms' NOTIFY sender, each held for the life of the process, and the delivery hub's and the event stream relay's, each held only while something subscribes to them. |

Two processes at their ceiling hold 234, past a default Postgres's `max_connections` of 100, so size the database's limit for both, with whatever else uses that database counted in, before the second process starts.

**A connection is closed after its next response once it is 60 seconds old** (`maxConnAge`, `cmd/dispatch/conn_age.go`):
the server stamps each connection's accept time and answers the first request it reads at that age or later with `Connection: close`, and a request carrying an `Upgrade` header, a websocket handshake, keeps its connection.
kube-proxy and the front door spread load per connection, not per request, so while callers reuse their keep-alive connections a process that has just rolled in gets little traffic;
a closed connection makes the client open a new one, which kube-proxy spreads across every ready process.
Shutdown needs none of this: once `Server.Shutdown` begins, Go's server already writes every response with `Connection: close` and closes the connection after it.
One race remains: a client can write a request onto an idle pooled connection at the instant the server closes it.
Clients retry an idempotent request on a new connection, but a proxied `POST` can come back as a `502`.

### Two builds at once

A release rolls: while build N serves, build N+1 starts beside it on the same Postgres and NATS, and a page or a `dispatch` command of either build can reach a process of either build.
So for each surface the two builds share, this is what build N+1 may change while build N serves:

- **Request bodies.**
  A new member is additive and ignorable.
  A route decodes a body into the members it declares and ignores every other one (`api/server.go`, `decodeJSON`;
  the JSON upload, `jsonArtifactUpload`), and logs the ignored top-level names at debug with the route.
  So a member whose loss changes what a request does (a precondition, a guard, a member that makes the request do something else) ships as a new route, never as a new member:
  build N would ignore it and do the request without it.
  A member a route stops taking stays declared and is refused by name, so a page or command that still sends it is told rather than silently served without it:
  `primary` on the JSON upload, `idempotency_key` on a broadcast, whose key is the `Idempotency-Key` header.
  A test hook (`/_test/…`, mounted only under `DISPATCH_TEST_HOOKS`) refuses every member it does not declare (`decodeJSONTestHook`): no client of another build calls one.

- **Routes.**
  A route is added freely.
  Build N answers a route that only build N+1 serves with the JSON `404 NOT_FOUND` `no route for <METHOD> <path>`, and it answers a known path asked with a method it does not serve the same way (Routes, below).
  The page (`web/src/api/client.ts`) and the `dispatch` command (`packages/envoy-client/src/dispatch-http.ts`) send that request again, with the same body, after 1, 2 and 4 seconds, each wait jittered by a quarter:
  no handler ran, and the next attempt can reach the other build.
  After the last attempt they say that the action is not offered yet because a release is in progress.
  A route is removed only in the release after the one that stops calling it, so no page or command still serving calls a route that is gone.

- **Query values and opaque tokens** (a cursor, a `kind`, an enum value in a query): reader-first.
  The release that reads a new value or token shape ships before the release that sends or mints it.
  A reader that meets a value it does not know answers without it where it can:
  `GET /api/v1/references?kind=` answers no edges of a kind this build does not emit and lists that kind in `unknown_kinds`.

- **Socket frames.**
  A new member is additive: the delivery socket (`/ws/delivery`) ignores a member a frame does not declare (`decodeSocketClientFrame`).
  A new frame type or view kind degrades on build N, which answers it `INVALID_FRAME`:
  the page then closes the socket and reconnects with the backoff of a lost socket (`web/src/api/delivery-live.ts`), until it reaches a process that serves the frame.
  A subscribe frame's `resume` is the exception to the ignoring:
  it is the point the view restarts from, so a member the view's resume does not take is refused by name (`decodeLiveResume`) rather than dropped, which would restart the view somewhere else.

- **Response members.**
  A new member is optional to every reader: a page or command of build N+1 works when build N answers without it.
  A member is removed only once no page or command still serving reads it.

- **Stored documents.**
  Reader-first, by the schema manifest (`internal/dispatch/pmdoc/schema/blocks.json`).
  Every node, mark, typed block, attribute and enum value carries `since`, the schema version from which a build reads it, and `creatable_since`, the version from which a build creates it;
  both are 1 where the manifest names neither.
  A new element is readable one release before it is creatable, unless the page admission below gates it: `legion-envoy-v29.1.0` first declares `requirement`, `plan_task` and `check`, at manifest version 3, and this build gives them `since` 3 and `creatable_since` 3 (Generations and holds, below).
  A server-side write (an agent's edit, a settlement, an import) is bound by this build's manifest alone, so one can still add such an element during a roll; a build of this manifest at an earlier version then answers `409 DOC_NEWER_CONTENT` for that document (the release before the manifest: Generations and holds, below).
  Every write path refuses an element this build reads but may not create yet (`pmdoc`, `refuseUncreatable`), unless the document already holds it, and the editor offers only the elements it may create.
  An element stays readable while stored documents may hold it: no release removes a readable element from the manifest.
  A build that still meets an element it does not know answers `409 DOC_NEWER_CONTENT` for that document, and its document socket closes with `4425` (Document errors, below).
  No route repairs, replaces or rebuilds such a document, since each of those would delete the element;
  settlement writes no version of it and leaves an approval to carry over to a build that reads it; and the excerpt reconciliation leaves its mentions owed.
  A renderer change (the markdown a tree renders as) and a reference-grammar change (what a body's references parse as) are reader-first too:
  a build whose renderer writes an unchanged document differently writes no new version of it, and a rewrite of a body keeps the reference edges to node kinds this build does not know.

- **Stored documents: the document socket, and ygo's encoding.**
  A page's document socket is writable by the whole fleet's numbers (`internal/dispatch/docs/socket_admission.go`, `authorize`):
  each process registers V, the schema version it reads, and R, the highest `since` among the elements it may create, in `server_processes` (migration `20261101030537_server_processes_schema`).
  A page presents its own V and R as `schema_version` and `schema_creatable`, and is writable on an open issue when its R is at most every live process's V and at most the highest R among the live processes, and its V is at least every live process's R and the highest `since` among the elements the document holds; otherwise it is read-only.
  The bound on the highest R makes a page of a newer build read-only after a rollback: no live process creates what that page creates.
  A page that sends no `schema_creatable` predates the manifest, so its R is 1, and a process of a build that predates the fleet columns counts as reading version 0, which no page's R is at or under.
  Every 15 seconds (the registry's live window) each process checks its open document sockets again and closes each writable one the fleet no longer admits with `4412` (reason `DOC_ADMISSION_REVOKED`; `DOCUMENT_ADMISSION_REVOKED_CLOSE_CODE` and `DOCUMENT_ADMISSION_REVOKED_CLOSE_REASON` in `packages/contracts`);
  the close frame goes out once the message the server has in flight on that socket is out, which it waits at most a second for, so a peer too slow to take it in that second gets no close frame at all and reads the end as `1006`;
  the provider reconnects on either, the new socket is admitted again, and the reconnect's sync restores what was in flight, so the second is a bound on the close code a page reads and on nothing else.
  The same close ends a document socket whose upgrade's IAP assertion has expired (Identity, above), with the same reconnect.
  A change of ygo's own update encoding is the one stored-document change no build can answer and no test here proves: a build that meets history written with an encoding it does not know cannot decode it, and the build that cannot is already serving when the new encoding arrives, so the reader-first rule above has nothing to act on.
  An upgrade of that dependency that changes the encoding is therefore a release deployed with no server of the earlier release running, like the migrations that took that exception, rather than a roll.
  What this build does answer is an element it does not know (`DOC_NEWER_CONTENT`, above) and the encodings two deployed builds already speak, which the two-build documents test drives; neither is the upgrade.
  This is the one statement of that rule; the other documents point here for it.

- **NATS.**
  The settings of the stream `ENVOY_NOTIFICATIONS` and of the delivery durables carry a generation in their metadata (`dispatch.stream-generation`, `dispatch.consumer-generation`).
  A change to the stream's `MaxAge`, `Duplicates` or subjects, or to a durable's config, takes a new generation (`streamConfigGeneration` in `internal/bus/stream.go`, `consumerConfigGeneration` in `internal/dispatch/delivery/intake.go`, each pinned by a test row).
  A build that finds a higher generation deployed leaves the stream as it is and recreates a durable with the deployed config, so build N never reverts what build N+1 set.
  Dispatch keeps the stream's deployed replica count, and keeps its deployed consumer cap where that is higher than its own (`streamMaxConsumers`): the cap, like the replica count and the byte limit, is only ever raised, so it takes no generation and the two builds never lower each other's.
  Every ensure keeps a deployed subject it does not know, unless it overlaps a subject of its own, and each client publishes a topic on the transport the deployed stream's subjects call for (`bus.Client.UsesCoreTransport`), so build N publishes a topic under a subject only build N+1 adds to the stream too, with its message id; a topic build N stores itself is never sent over core NATS, and while the deployed stream does not capture it the publish sends nothing and fails with `bus.ErrSubjectNotDeployed`, so the queue of a message to a session no listener holds settles failed and the build announcer leaves its announcement unpublished for the next pass.
  No subject is retired while a running build publishes to it: retiring one is an operator step (`internal/bus/stream.go`, `streamReconciliation`).
  Each delivery durable has one intake holder at a time, the process that holds its `duty.Hold` (above).

- **Messages.**
  Envelope and payload members are additive:
  the members of the Dispatch events the outbox publishes (`internal/dispatch/outbox/publisher.go`, `envelope`), of `server.started` (`internal/dispatch/startup`) and of the GitHub envelopes the delivery intakes read (`internal/contracts/normalize.go`).
  Every consumer ignores a member it does not know.
  The listener decodes an envelope into the members it declares and hands the payload on unread (`cmd/listener/delivery.go`, `listenerDeliveryHandler`).
  The session plugins parse the envelope and the Dispatch event with schemas that drop or carry through every other member (`packages/envoy-client/src/delivery.ts`, `renderInbound`;
  `DispatchEventSchema` in `packages/contracts/src/dispatch-api.ts`).
  The Legion daemon decodes each envelope and payload into the members it declares (`packages/daemon/internal/intake/decode.go`, `decodeMessage`).
  The intakes decode an envelope into the members it declares, read a payload's string members and skip every other member (`internal/dispatch/delivery/intake.go`, `flatPayload`; `jobs.go`, `receive`).
  A consumer ignores an event type or `kind` it does not know, and acks it:
  the daemon decodes it to no fact and acks it (`decodeDispatchEvent`, `decodeDispatchProjectFact`, `decodeGitHubFact`;
  `consume.go`, `consumeMessage`), and the intakes ack it with no write and no GitHub call (`route`, `receive`).
  The session plugins refuse no type either: they show an event of a type they do not know with its type and payload as they came (`dispatchPayload`).
  So a member whose loss changes what a message means ships as a new type or kind, never as a new member:
  a consumer that does not know the member would act on the message without it.
  A member or a kind is removed only after no running build reads it.
  A new envelope `source`, `urgency` or `expects_reply` value is reader-first:
  the listener drops an envelope that holds a value it does not know and delivers it to no session (`internal/contracts/generated.go`, `Envelope.Validate`).

- **Settings.**
  A loop interval may change: a periodic loop records its passes per lease and period (`duty_passes`, the row `<lease>/<period_ns>`;
  `duty/exclusive.go`), so two builds with different intervals for one loop each run it once per their own period, and two builds at one interval share it as one build does.
  The embedder is coordinated by the code:
  one identity is active at a time (`embedder_active`, migration `20261101030538_embedder_active`), it stays active while a live process carries it, and only the processes configured with it embed, drop or reserve tokens (Search, below), so two builds with different embedders do not drop each other's vectors.
  So is the file store: while a live process has no bucket (`file_store` false or unset), an upload keeps its bytes in its row as well as in the bucket, and `backfill-files` refuses;
  turning the store off takes `backfill-files --restore` before the release that unsets it (Moving uploaded files to the bucket, above).
  Both decisions read only the live rows of `server_processes` (`internal/dispatch/processes`, `Live`).
  A build that writes a row but predates the fleet columns (migration `20261101030537_server_processes_schema`) registers no embedder identity and no `file_store`, so it counts as carrying no identity and as having no bucket.
  A build that writes no `server_processes` row at all (every release before migration `0135_server_processes`, such as `legion-envoy-v19.0.0`, which stops at migration `0134`) is invisible to both decisions:
  while it serves, every process that has a row can have a bucket, so an upload keeps no bytes in its row and `backfill-files` does not refuse, and a process of that build with no bucket then answers `503 FILE_STORE_UNAVAILABLE` for an upload whose bytes are only in the bucket.
  Every build before this rule drops the vectors of every identity but its own, whatever the active one is.
  So turn on the file store, or change the embedder, no sooner than the release after the first one that carries the rule, and only after every process of the earlier release is gone.

- **SQL.**
  Expand-then-contract across two releases (Database migrations, below), unchanged.

- **Owner locks.**
  Both builds' writers of one issue, document or project exclude each other, so a write of either build decides on what the other committed.
  Each process registers the owner lock protocol it serialises those writers by (`server_processes.owner_lock_protocol`, migration `20261115100924_server_processes_owner_lock_protocol`): a build that takes the owner's row first and holds it to its commit, as every build before that migration does, reads the column's default 0, and a build that takes the owner's advisory lock first (`store.LockOwner`) registers 1.
  A build at 1 names 1 in the `dashboard_builds` row its boot publishes too (migration `20261115200014_owner_lock_legacy_gate`), and an earlier build's row reads 0.
  A build that changes how it serialises an owner's writers registers a new protocol and keeps taking what the protocols still live take, the same way.

- **An earlier build in the fleet.**
  An earlier build is in the fleet while a row of it in `server_processes` has heartbeated within the last 10 minutes (`store.OwnerLockLegacyWindow`, the registry's prune age, `processes.PruneAge`), so until it is deregistered or pruned, or for an hour after it last arrived (`store.OwnerLockMarkWindow`).
  An earlier build that serves a dashboard arrives at its dashboard publish, the first thing its boot writes and before any write an owner lock guards: a publish that fails ends its boot, which serves nothing.
  An earlier build that serves no dashboard (its web dist holds no `index.html`, `dashboard.ErrNoBuild`) arrives at its registration instead, after its boot's repairs and background passes have begun writing; the image never runs that way, since it sets `DISPATCH_WEB_DIST` to the web app it ships (`packages/envoy/docker/Dockerfile`).
  The registration of an earlier build's process, and the return of its lapsed row, arrive too.
  Each arrival's insert, a row at 0, fires a trigger that takes the owner lock gate, a transaction advisory lock of its own (`store.OwnerLockGateNamespace`), exclusively, and stamps the arrival's time in `owner_lock_legacy_mark`: the trigger `dashboard_builds_owner_lock_legacy_arrival` runs the function `owner_lock_legacy_arrival`, and `server_processes_owner_lock_legacy_arrival` runs `owner_lock_legacy_registration`.
  The migration stamps that mark too, at the newest earlier publish already in `dashboard_builds` when that is within the hour, so an earlier build whose publish committed before the migration ran, and which has not registered yet, is in the fleet as well; a database no build has published to gets no mark.

- **A writer beside an earlier build.**
  While an earlier build is in the fleet, a build at 1 takes the owner's row too, right after the advisory lock and before the read the write decides on, and takes no gate.
  While none is, it tries the gate shared, without waiting, before its first owner lock, in the owner lock's own statement (`public.owner_lock`), reads the fleet again behind it, and takes the advisory lock alone.
  A try that fails, because an arrival holds the gate or waits for it (Postgres grants no conditional lock request that conflicts with one already waiting), runs that transaction as beside an earlier build: the advisory lock, then the owner's row.
  So an earlier build's arrival waits for every transaction of this build that holds the gate, every owner lock from the moment the arrival asks for the gate takes the owner's row, and every owner lock after the arrival commits reads the earlier build in the fleet.

- **What the owner lock gate costs.**
  A write never waits on the gate, and one that starts while an arrival holds or waits for it takes the owner's row, which gives its read phase a transaction id.
  An arrival waits only for the transactions that already held the gate when it asked for it, and holds the gate from its grant until it commits.
  A publish waits as long as the earlier build lets it, and every earlier build gives up its publish at 60 seconds and exits; a first publish of a build holds the gate across the upload of its assets, which comes after the insert in every earlier build, and a publish of a build already stored uploads nothing.
  A registration holds the registry's lock while it waits, and every process's heartbeat takes that lock within a 2-second bound of its own, so its wait for the gate is bounded at 5 seconds (the function's `lock_timeout`), after which its insert fails with `55P03`.
  Every release before this one then ends a fresh registration's process (read in every tag before this release that registers a process, the 220 from `legion-envoy-v20.0.0` to `legion-envoy-v30.39.0`; a release before migration `0135_server_processes` registers no process at all), which exits 1 before it serves anything, and logs the failed return of a lapsed row as a failed heartbeat and tries it again at its next one; that return is bounded at 2 seconds by its heartbeat's own bound in any case.
  A 5-second hold of the registry's lock costs each process at most one heartbeat, well inside the 15-second live window, but registrations queue on that lock one at a time, so earlier processes that register at once, each with no dashboard, hold it 5 seconds each, and an earlier build's own wait for that lock has no bound this build can set (below).
  After an earlier build's boot, a build at 1 takes the owner's row for an hour, as every write did before it moved to the advisory lock, even once the earlier build has stopped.

- **What the fleet does not see.**
  Three cases are not seen.
  An earlier build whose boot takes more than an hour from its dashboard publish to its registration is not in the fleet from that hour's end until it registers: every step between the two is bounded at 30 to 60 seconds and its admission at 20, but the seed of the repository projects is not.
  A process of an earlier build silent past the prune age (10 minutes) - frozen, or cut from the database - has its row pruned and is not in the fleet until a heartbeat's admission of it, whose insert is an arrival, commits.
  Each such admission waits for the gate at most 2 seconds, its heartbeat's own bound, so a write of this build that holds the gate longer defers it, and that process serves writes in its row-first order meanwhile; production rolls with `Recreate` (above), which stops every process of one release before the other starts, so it does not reach this case.
  That is the registry's own model: every other decision over the fleet (the signing keys, the embedder, the schema admission) counts a process silent past its live window as gone.
  A subcommand of an earlier build that an operator runs beside this one neither publishes nor registers, so it is never in the fleet.
  Of those subcommands, `rebuild-refs` takes issues' and documents' rows `for no key update` (from `legion-envoy-v30.21.1` on), and `backfill-block-ids` and `backfill-anchor-blocks` settle a document they open that owes a settlement, which takes its owner's row and appends its events: run those from a build that carries this change.
  The others write no owner's row: `census` reads in one read-only transaction, `stored-order` reads, `backfill-files` writes only file versions' bytes, `redeliver-webhooks` writes the shared GitHub rate limits and asks GitHub to deliver again to the serving processes, and `settings`, `routes` and `openapi` open no database.
  The production deployment runs two of them as Jobs from the image its server is moving to (example-org/deployment, `dispatch/main.go`), and neither writes an owner's row.
  `census` runs before the server's Deployment moves to that image, and runs again only when the Job's template changes, a new image among them, or after a run that failed (`declareCensusJob`): so a roll forward runs the later build's census beside the earlier server, and a rollback the earlier build's census beside the later server.
  `backfill-files` runs once the server has rolled out, and only when a file move or restore is set (`declareFileJob`).

- **What the fleet does not see of this build: a lapse.**
  The registry does not see this build's own processes for a moment when earlier registrations queue: an earlier build takes the registry's lock in its own code, before its insert, so the 5-second bound of its trigger does not bound that wait, and while the lock is held longer than about 10 seconds, two or more such registrations in a row, a heartbeat of this build can find its row past the 15-second live window.
  Every row of this build then reads as not live for up to that excess, until the heartbeat renews it as lapsed, rejoins with a WARN and runs admission again.
  Nothing is lost in that time, by what each reader of live rows decides: the schema admission starts from the reading process's own numbers and never narrows; the boot's dashboard prune keeps the 20 builds started last, which every boot of this build renews; the embedder identity and the file-store reads change anything only in a fleet whose processes already disagree, and then they re-queue derived vectors, or keep an upload's bytes in the bucket alone, where a process with no bucket answers 503 until `backfill-files --restore`; the new-version notice, the startup announcer and the CI pass list change only what they show.
  A lapse drains nothing unless a process whose signing keys differ registers inside it: its admission does not see this build's rows, and each lapsed process of this build is then refused at its next renew, drains and exits 1, and a cookie minted on one side fails on the other.
  Main carries the same path for any lapse, such as a stalled database; the lapse here needs earlier processes with no dashboard, which the image never runs.
  This was found by reading the code (`processes.go` `Live`, `admit`, `renew`), not measured.

The two-build test checks this rule (`cmd/dispatch/twobuild_*_test.go`, `go test -tags twobuild
./cmd/dispatch`, CI job `dispatch-two-builds`).
`packages/envoy/scripts/build-dispatch-build.sh` builds the previous release from its commit, and the test runs it beside this tree on one Postgres, cloned at this tree's schema, and one NATS.
It drives each build's `dispatch` command against the other build's server, reads and edits each build's stored documents and opens their document socket on the other build, passes messages both ways while it checks that the stream and consumer settings stay as they are, races a write of each build on one issue, the previous build's committing first, boots the previous build, with its web app, while a write of this tree is between its owner lock and its write, and boots it with no web app while a write of this tree holds the gate longer than the registration's bound, which refuses that registration and keeps every heartbeat of this tree inside the live window.
On the previous build it makes a document again where this build removed one, and removes a document this build made, once it has read whether that build writes document nodes (`twobuild_nodes_test.go`): beside a build that writes none, it holds the census to naming the removed document's node `orphaned`, the backfill to leaving it, and this build's block-index tick to curing every row the previous build left once that build stops; beside one that writes them, as this tree built as the previous build does (CI job `dispatch-two-builds-same`), it holds each node to that build's own statement and the census to naming none.
It also runs the page admission across the earlier build's departure.
Beside the previous release, a page of this tree is admitted by that release's numbers, and it stays writable throughout when they are this tree's own.
The first rollout of the fleet's numbers needs a release before the fleet columns, so `build-dispatch-build.sh` builds the first-rollout build as well, with `--commit edb69e0d7588ec037847182e74a04a343a0664c9` (the release tagged `legion-envoy-v30.20.2`), and the test runs it and holds a page of this tree read-only while that build's process is live and writable once that process has left.
That tag is lightweight and can move, so the commit is the pin: the test fails a first-rollout build of another commit (`firstRolloutRelease` in `twobuild_harness_test.go`), and the CI job fails when the tag names another commit (`FIRST_ROLLOUT_COMMIT`).
The same CI job then runs the browser suite's `cross-build.multiprocess` beside each of the two builds, each build's page against the other build's server, and expects the document writable or read-only as the live registry's numbers admit it (`packages/dispatch/e2e/cross-build.multiprocess.e2e.ts`, `pageWritable`).
In the run beside the first-rollout build, `DISPATCH_E2E_FIRST_ROLLOUT_COMMIT` holds the run to that build's commit and to a registry that reads version 0 while that build is live, so a build of another commit, or a build that registers its schema numbers, fails the run.
The command bar's row (`twobuild_going_runs_test.go`) runs a fixed release beside this tree instead, the one tagged `legion-envoy-v32.15.0`, pinned by its commit `6c46f6bb7dd6a1ac9b6235fb3bce990ec82eae86` as the first-rollout build is (`goingReleaseCommit`, and `GOING_RELEASE_COMMIT` in the CI job), since its maintenance sweep ends and deletes its own command bar's going requests, a sweep the next release drops.
Beside that release's sweep this tree's runs keep going: History lists the runs it saved, their frames stream as they were stored, a run at work ends with its model's answer, a waiting plan runs once it is approved, and a run saved at a run shape this tree does not read stays queued; a run whose saved steps name a tool this tree does not know fails, naming the tool.

Generations and holds protect from the release after the first one that carries them.
The build before them compares stream and consumer settings with no generation, binds its delivery durable by lookup with no hold, and refuses a request member it does not declare.
So each protection binds from the release after the one that introduces it, and that release would ideally change no request member, stream or consumer setting, or loop interval.
This one changes no stream setting from the release before it.
The stream's subjects in the release before this one already carry a team's own topic `notifications.team.>` and its exception lane `notifications.envoy.exceptions.notifications.team.>` (both since `legion-envoy-v30.8.0`) and the listener's own lanes `notifications.envoy.kept.>` and `notifications.envoy.resume.>`, at the same `MaxAge`, `Duplicates` and `Discard`, so the two builds of a roll compile one set of settings between them.
What this release adds is the stamp: generations 1 to 4 exist on this change alone, and generation 4 is the pinned row of the settings it ships (`internal/bus/stream_config_test.go`, `streamGenerations`), which is generation 3's subjects with the team topic and its exception lane added.
So the next build that changes a setting takes generation 5, and a build of this release leaves a stream of that generation alone (`keepStream`).
A build before the stamp keeps a deployed subject that overlaps none of its own (`reconcileSubjects`), so it leaves those subjects in place whatever the stamp says.
The document elements of version 3 are already out: the three typed blocks `requirement`, `plan_task` and `check` ship from `legion-envoy-v29.1.0`, the first release carrying the commit that added them.
Every release from that one on, `legion-envoy-v29.2.0` and this one included, reads and creates them at manifest version 3, and reads a document holding them as a document of its own schema.
A release before it, such as `legion-envoy-v29.0.0` at manifest version 2, declares none of the three and has no newer-content answer at all, so it reads a document holding one as outside the schema, and while it serves, a repair or a rebuild asked of it would rewrite that document without them.
Until every process of such a build is gone, leave such a document to a process of a build at version 3.
Version 4 adds one element, the `check` block's `ci_check` attribute (main's #3972), and it ships the same way: from `legion-envoy-v30.12.0`, the first release carrying it, every release reads and creates it at manifest version 4.
A release before it, such as `legion-envoy-v30.11.0` at version 3, does not declare it and has no newer-content answer either, so it reads a check carrying `ci_check` as outside the schema, and a repair or a rebuild asked of it would drop the attribute.
Until every process of such a build is gone, leave such a document to a process of a build at version 4.
This release's own document change is the page admission over the fleet (Stored documents, above), and it binds the same way.
A server of any build before this one knows no fleet: it compares the page's `schema_version` with its own manifest version exactly (`schemaReadOnly` in that build) and holds a page of any other version read-only, which is what `legion-envoy-v29.0.0` at version 2 does with every page of version 3, and what `legion-envoy-v29.2.0` at version 3 does with a page of any other version.
A process of such a build registers no schema numbers either, so while one is live this build reads the fleet's lowest version as 0 and holds every page it serves read-only, whichever build's page it is.
So during the first rollout onto this release every page is read-only on this build's processes, and a page is writable on a process of the older build only where its version is that build's own.
That lasts until every process of the older build is gone: a process stopped as a deploy stops it deletes its row at the end of its HTTP drain and leaves the fleet then, and one killed without deregistering leaves it up to 15 seconds after its last heartbeat, the registry's live window.
Each page is writable again on the socket it opens after that, which is the socket a reload opens: a socket already admitted read-only stays open and read-only, since the re-check closes writable sockets alone.
In that first overlap, a take-over (`delivery/held_durable.go`, `heldDurable.takeOver`) deletes a durable and creates it again at its ack floor, all under the durable's consumer lease, and tries a failed create again for up to 10 seconds while it holds its hold.
The build before holds reconciles the durable under that same lease every 30 seconds and does not read the start the take-over recorded.
So one case is left: a process of the new build killed between the delete and the create (or one that loses its hold, or fails every try, first) gives up the lease with the durable absent.
A process of the build before holds that takes the lease next creates the durable with its own config:
the events durable then delivers from the stream's first message, which replays up to 72 hours of GitHub events, and the jobs durable from the next new message, which skips every job the old durable had not acknowledged.
A take-over of the new build that takes the lease first creates the durable at the recorded start instead.
A body's reference edges bind from that release too:
a build before the guard rewrites a body by deleting every edge its source holds, so it drops the edges to the node kinds it does not know
— a comment's and a message's blocks among them — until a build that knows those kinds rewrites the body.
The file store and the embedder also bind only from the release after their rule:
a build before the rule writes no embedder identity and no `file_store`, or no `server_processes` row at all, and drops the vectors of every identity but its own.
So turn on the file store, or change the embedder, no sooner than the release after the first one that carries the rule, and only after every process of the earlier release is gone.

## Background loops

Dispatch's background loops come in three shapes, so that a database nobody writes costs them nothing except where GitHub gives them no change to wake on.
`cmd/dispatch/main.go` states the classes at the call site.

**Woken loops** run a pass at start and then only when a Postgres notification says their work may have changed, or when a due time their last pass asked for comes round.
With nothing written and nothing due they hold no timer and send no statement.
The four that run passes (`duty.RunWoken`) start two passes no closer together than the period of the ticker each replaced, so under load each runs no more often than that ticker did.

- The embedding queue (`embedqueue.Run`) scans when a write enqueues a row:
  the issue trigger's `embeddings_enqueue` (migration `0151`) and each indexer's `embeddings_enqueue_window` (migration `20261102233625`) notify `dispatch_embeddings` for a new row or a text whose hash changed, and a write that leaves the text as it was notifies nothing.
  The `embeddings` rows the coverage pass writes wake it in process.
  Scans start at least 5 s apart.
  After each scan it reads when the earliest pending row falls due (a failed row's retry, or another process's claim lapsing after its two minutes) and arms one timer for it, never sooner than 5 s away.
  A throttled batch waits 5 s plus its backoff before the next.
  It takes no lease: its claim keeps two processes' scans off each other's rows, and only a process whose embedder has the one active identity claims or embeds anything (Search, below).

- The reference excerpt reconciliation (`docs.Service.RunReferenceExcerptReconciliation`, lease `reference-excerpts`) is woken by a row trigger on `refs` (`0153`) that notifies `dispatch_excerpts_owed` for each document mention left owed an excerpt, whichever build or command wrote it (`rebuild-refs` included).
  Batches of 20 documents start at least 2 s apart, the next as soon as that allows while a full batch leaves documents it can still record.
  A document it cannot read or record is deferred in `reference_excerpt_backoff` (`0152`), keyed by the version that failed:
  one minute after the first failure, doubling to at most six hours, plus up to a fifth again of jitter;
  a success deletes the row, and a newer version starts again from one.
  A document holding an element this build cannot read (`DOC_NEWER_CONTENT`), which a newer build wrote while two builds serve one database, is deferred the same way, its mentions left owed for a build that reads it.
  A deferred document stays out of the batches until it falls due, so documents that keep failing neither hold the loop at 2 s nor keep the ones sorting after them waiting, and the earliest due time is the loop's only timer.
  Each process's first pass also marks owed the excerpts a pre-`0092` build left stale (`refs.MarkStaleExcerpts`), once.

- The edit notice sweep (`docs.Service.RunEditNotices`, lease `edit-notices`) is woken by a row trigger on `document_edit_notices` (`0157`) that notifies `dispatch_edit_notices` each time a writer gives a notice a due time:
  a person's settled version opening or moving its quiet window, from either build.
  A sweep sends the notices that are due, each as one `artifact.human_edited` event, and then reads the earliest due time left (`document_edit_notices_due`) and arms one timer for it, so a notice goes when its quiet window ends.
  Sweeps start at least 5 s apart, the period of the timer it replaced:
  a notice still due after a sweep (past a batch of 20, or one whose send failed) is swept again 5 s later, and a notice goes at most 5 s after its due time.
  A held window, which names nobody and has no due time, neither wakes it nor keeps a timer.

- The suggestion outcome sweep (`api.RunSuggestionOutcomeSweep`) is woken by `dispatch_events` (`0107`), which every event commit notifies whichever process made it.
  Passes start at least 60 s apart.
  A pass reads the events after its cursor (`suggestion_outcome_cursor`, `0154`) and at or below the event log's safe horizon, as the per-process relay behind a reader's events subscription reads:
  event ids are not commit order, so a cursor moved past an event that could still commit would never judge by it.
  The pass takes that horizon once (`events.Horizon.Safe`, the server's one `Horizon`;
  the first pass after a start takes `events.Horizon.Await`, which waits until the event writes of Dispatch's database open at the call end, and ends with its context), and one left with an event above it asks for a pass 60 s on rather than re-reading at once, so a horizon an open event write holds back costs one pass a minute;
  the late commit's own `dispatch_events` notification wakes the loop as any other does.
  A pass also reads the suggestions no pass has judged yet (`write_suggestions.swept`, `0155`), in pages of 1,000, each page in a transaction that holds the cursor row `for update`, and judges only the pending suggestions those can resolve:
  the newly recorded ones, against all their events, and those whose source issue, source document or suggested issue a page's event names.
  An event or suggestion less than 60 s old waits for a later pass, which the pass asks for as its due time, so an agent's writes within a minute are judged together.
  The first pass on a database, whose cursor is null, judges every pending suggestion once, as every pass of the release before it did.
  A failed pass is retried a minute later.
  Like the embedding queue, it takes no lease:
  the cursor row each page holds is what serialises two processes woken by one commit, the first pass included, since the row is there from `0154` with a null `event_id`.
  The second process's page waits on the row and then reads the cursor the first advanced, so it judges what the first did not rather than the same page twice, and no wake is given up to another process's pass.

- The delivery settings both GitHub intakes route by are one cache per process (`delivery.SettingsCache`), read at start and again when `delivery_settings` changes:
  `delivery_settings_changed` notifies `dispatch_delivery_settings` (`0156`) for a change to any column but the freshness ones every GitHub event and reconcile pass rewrite.
  A settings change reaches a running intake without a restart, and neither intake reads the row per message or on a timer;
  a read that fails is tried again 30 s later.
  The release before this one polls the row every 30 s and may run beside it during a roll.

- The block schema is one value per process, built from the store's type nodes (`schemanodes.Installer`), installed at boot beside the migration run and again when a supertag or attribute node changes:
  the triggers of migration `20261109051232` notify `dispatch_block_schema`.
  A type is data, so a type added by a migration reaches a running server without a restart;
  a read that fails is tried again 30 s later and leaves the schema in use where it is, and so does a value the install refuses; `schemanodes.Installer` makes both checks, the read's (`schemanodes.Read`) and the install's (`pmdoc.Install`).
  Migration `20261113230000` is the first type change after `20261109051232` seeded the types: it adds `check`'s `ci_check` attribute node and moves every block type to version 4, the version `pmdoc/schema/blocks.json` declares.
  Migration `20261123080000` adds `plan_task`'s `steps` attribute node and moves every block type to version 5, the version `pmdoc/schema/blocks.json` declares since #4611.
  It is the first type change written after the store's backfill (`backfill-graph-store`), which the releases from legion-envoy v34.11.0 on may have run over `graph_nodes` and `graph_tuples`, so it writes both tables by bounded writes alone (below): literal rows, and each block type's supertag node by its primary key, to the whole attributes the earlier type migrations left with the version moved (`TestATypeMigrationAfterTheBackfillIsAdmittedAndLeavesTheFilesSchema`).
  Each read runs in one read-only transaction at repeatable read, so a migration that commits while it reads is read whole by the next read, never half.
  A store schema above the version this binary was built for is a store the next release migrated: the process installs it at its own version, with the elements whose `since` is at or below that version and nothing else, and keeps serving, its boot and `/healthz` unaffected, so a release that ships a type migration takes no process of the previous build out of service.
  A read that moved the schema the process serves first writes the numbers it serves now into its registry row (`processes.Registration.RefreshSchema`), then closes every document socket it holds (`docs.Service.CloseDocumentSockets`); the browser connects again presenting the schema its own build compiled, and that admission decides against the schema installed now.
  The subcommands that parse or write documents (`backfill-block-ids`, `backfill-anchor-blocks`, `backfill-graph-store`, `rebuild-refs`) install the store's schema too, after their migration run, so they hold documents to the store's types and not to the file this binary embeds.
  `backfill-files` moves file bytes and parses no document, so it installs no schema.

- The record of GitHub reads, which `/healthz` answers as `github` (Routes, below), is one copy per process (`githubreads.Copy`), read at start and again each time any process writes `github_read_attempts`:
  the table's row trigger notifies `dispatch_github_read_attempts` (`20261118020802`) with the read's name, and each of the four loops that read GitHub under a lease (the delivery reconcile, the architecture import, the webhook redelivery sweep and the CI reconcile, below) writes its read's row at the end of each pass that asked GitHub anything or met a failure, unless this process's shutdown cut the pass short.
  The copy is kept only where a GitHub App is configured and holds no timer, and the probe reads it alone, so `github` sends the database no statement;
  a read that fails is logged at WARN and keeps the copy it had until the next wake, which is a write by any process or the hub's next successful LISTEN.

The two leased woken loops take their lease with no period.
A process that finds it held tries again
— every 2 s for the excerpts, every 5 s for the edit notices, plus up to a tenth again
— until a pass of its own runs, because the other process's pass may have read before the change that woke this one.

**Boot passes** run once at start:

- The embedding coverage pass (`embedqueue.Cover`, Search below) runs once, with no lease.
  Its identity steps and the step that records each issue's inputs (`recordIssueSources`) run again after it:
  at once when this process's queue first embeds successfully after the pass skipped the identity steps for a canary the embedder did not answer, and every `identityRecheckInterval` (10 minutes, plus up to a tenth again) for as long as the process runs, so a vector an old-configuration process commits during a roll is dropped, and the inputs of an issue edited since the run before are recorded and the coverage report stops building its text.
  Only the process whose embedder has the active identity drops or re-admits anything;
  a process whose identity is not active sends no canary and looks again on the timer, since the identity it is configured with becomes active once no live process carries the one before it.

- The task progress reconciliation (`docs.Service.RunTaskProgressReconciliation`) counts every issue whose stored task count is not its latest version's.
  Every version write records its count in its own transaction, so nothing makes new drift while the process runs but a row another writer held during the pass:
  those are retried up to six more passes, the first a second later and doubling to at most a minute, and drift left after them is logged at WARN and left to the next start.

**Timers that stay**, because nothing announces what they would miss
— a webhook that never arrived (a push to a component source's branch among them, which the architecture import repairs), a delivery that failed, an expired key, a delivery a process that died still owes:

- The delivery reconcile (`delivery.ReconcileInterval`) and the architecture import (`architecture.SyncInterval`) run every 5 minutes through `duty.RunJittered`, the first pass after a random delay within one interval.
  The webhook redelivery sweep (Webhook redelivery, above; `redeliver.Interval`) runs every 2 minutes, its first sweep at start.
  Each waits the interval less up to a tenth of it after every pass (`duty.JitteredWithin`), drawn afresh every time, so no wait is longer than the interval and two processes' passes drift apart rather than ticking in step.
  Each hands its lease a period of 0.9 of its interval (`duty.LeasePeriod`), which every wait is longer than, so a lone process's pass is never refused as a period its own last pass served.
  `DISPATCH_TEST_DELIVERY_RECONCILE_INTERVAL` replaces the delivery reconcile's 5 minutes for an end-to-end test, and a deployment never sets it.
  `DISPATCH_TEST_REDELIVERY_INTERVAL` replaces the sweep's 2 minutes for an end-to-end test, and a deployment never sets it.
  `DISPATCH_TEST_ARCHITECTURE_IMPORT_AT_START=1` runs the architecture import's first pass as the process starts, rather than after the random delay, for an end-to-end test that must see a pass inside its window, as `DISPATCH_TEST_RECONCILE_AT_START=1` does for the delivery reconcile, and a deployment never sets either.
  GitHub has no feed of what these three would miss.
  A reconcile pass re-reads the pull request heads first, whether or not the deployment has delivery settings and outside the deploy repository's permission check (`Reconcile.runOnce`), since every deployment compares a recorded result with those heads;
  every other step of the pass is gated on the settings and on that check, so a deployment with no settings runs the head pass and nothing else.

- The CI reconcile pass (`cireconcile.Interval`, lease `ci-reconcile`) runs every 5 minutes through `duty.RunLeased`, its first pass one interval after the start, and reads each open pull request's head of every project's default repository and every repository an issue names as its own, recording every check run, finished or not, and every completed check suite Envoy's listener record of that head lacks, and the head's commit statuses where the App's token holds Commit statuses (CI reconcile, above).
  A webhook GitHub never delivered leaves the head's record behind GitHub: the head never settles, or settles while GitHub still runs a check, and no notification announces either.
  It needs NATS, whose `envoy_ci_state` bucket it binds and never creates, and the App private key; without either it is off and says so at start.
  The pass compares a `tester` or `architect` check run's `app.id` against `DISPATCH_REVIEWER_APP_ID`, and with that variable unset records no verdict check at all.
  It leaves out the check suites of the App `DISPATCH_VERDICT_APP_ID` names, and with that variable unset records every App's completed suite.
  Its tick records its period in `duty_passes`, so one process serves each period however many run, and `DISPATCH_TEST_CI_RECONCILE_INTERVAL` replaces the 5 minutes for an end-to-end test.
  That interval is the pass's own bound too: a pass that cannot cover every head within it ends there, logs its line and leaves the cursor for the pass after it, so the lease is free when the next pass is due (CI reconcile, above).

- The keyed writes' maintenance sweep (`api.RunMaintenanceSweep`, lease `suggestion-outcomes`, `api.MaintenanceSweepInterval`) deletes the `Idempotency-Key` rows past their 24 hours, ends and clears the answer runs the going routes leave behind, deletes each command-bar conversation whose newest run is seven days old, stops each run that works under no live lease past its working budget (Answering a question, below), and then sends every delivery a process that died left owed (Keyed writes, below), once a minute across the processes:
  its pass records its period in `duty_passes` like the timers above, so the other process's tick inside a period already served skips.
  `DISPATCH_TEST_MAINTENANCE_SWEEP_INTERVAL` replaces the minute for an end-to-end test, and a deployment never sets it.
  No notification replaces this timer, which is why it is the one piece of maintenance that stayed one:
  a key passes its window by the clock alone, an answer run passes its own by the clock too, and a delivery is owed because the process holding it died, which commits nothing to announce.
  It keeps the lease name the suggestion outcome sweep carried this work under, so during a roll the earlier release's pass and this one still exclude each other on it.

Outside these classes, the document compaction sweep (lease `document-compaction`) runs every hour from a tick at start whether or not NATS is configured, the display tree fill (lease `text-tree-fill`, `texttreefill.Run`) runs every hour from a tick at start and holds its lease for a whole pass, which is as long as it takes to store a tree for every stored text that has none at this build's display-parser version, the outbox's publish scan is woken, never periodic (`outbox.Run`), the settlement resumption still runs every minute, the block index reconciliation (lease `block-index`) runs every 30 s (every 2 s while a batch is full) and the repair of closed and reopened issues' asks (lease `issue-ask-repair`, Database migrations below) every 10 minutes after its boot pass, each recording its passes in `duty_passes` with its period, and the per-feature listeners (the delivery live hub, the event stream relay and the events outbox) keep their own connections and keep-alive pings.
Each pass of the settlement resumption arms at most 50 owed documents, closed and open issues' alike: first the documents that no pass tried, then the documents that a pass tried, oldest try first.
The pass records each try in the database (`doc_settlements_pending.tried_at`), so the process that takes the lease next reads the same record.

**The keyword fill** (`textversions.Run`) is a held duty, not a loop woken per pass, and it has none of the three shapes above.
It writes `search_text_versions`, the keyword table of each past version of the searchable text, from the event log (Search, below).
One process at a time holds its `duty.Hold`, named `search-text-versions-fill`, and fills the table for as long as it serves.
A lease taken per pass would move the fill from one process to another at each pass, and the pause below reads only the answers reads of the process that writes.
The fill starts only after the server listens and the registry of live server processes admits this process (Running more than one process, above).
So the fill never runs inside a migration, and it never runs in a process that the registry refuses or has not admitted yet.
A process that the registry has not admitted serves no answers read, so the pause below would read nothing.
`DISPATCH_SEARCH_FILL=off`, read once at boot, defers the fill: the process starts no fill, takes no hold, writes no row of the table or its cursor, and logs `dispatch: the keyword fill is deferred; this process fills no keyword table` once (`setting=DISPATCH_SEARCH_FILL value=off`).
The same line says whether each table holds a row: `search_text_versions_rows` and `search_text_versions_fill_rows`, each `none` or `some`, read by `select exists(select 1 from public.<table>)`.
The read reads no column and stops at the first visible row, but it may scan dead rows before that one; the statement timeout below bounds that.
Each read runs in a read-only transaction of its own, under three bounds:

- `lock_timeout` of 5 s: Postgres ends a wait for a lock on the table with 55P03.

- `statement_timeout` of 6 s: Postgres ends a read that runs on for any reason with 57014, so no read is left running at the server.

- A client deadline of 10 s for each table, which also covers taking a connection from the pool, a dial or a wait for a free one included.
  At that deadline pgx closes the connection and asks the server to cancel the statement.

The two reads run one after the other before the server listens, so they delay the boot by at most 20 s in all; a lock on a table alone delays it by about 5 s for that table.
The read takes ACCESS SHARE on its table and holds it until its transaction rolls back or its connection ends, normally milliseconds after the read; no statement timeout bounds how long it holds it.
Row writes go on beside it, but a `TRUNCATE` or another statement that wants ACCESS EXCLUSIVE waits behind it for that long.
A read that fails, a timeout included, gives `unknown` and the error in `search_text_versions_rows_error` or `search_text_versions_fill_rows_error`, and the boot goes on.
So the boot log alone, with no read of the database, shows whether a process started with the fill off found both tables empty.
No route reads the table yet (Search, below), so search answers alike with the fill off, and a later boot with it `on` or unset starts the fill from the cursor where it stands.
`on` and unset start the fill; any other value refuses the boot, naming the variable.

- The hold has the values of the delivery intakes' holds.
  It lasts 15 s on the database's clock after each renewal, the holder renews it every 5 s, and the holder stops writing 2 s before it lapses.
  Each other process stands by.
  A standby tries for the hold at once, each time a hold is released (`dispatch_duty_holds`), and every 10 s.
  So it takes a released hold at once, and the hold of a holder that died within 25 s.
  When the process stops, the batch in flight commits or rolls back, and the holder releases its hold before the pool closes.

- The holder runs a pass when it takes the hold, and then each time a commit writes an event (`dispatch_events`).
  These two wakes are the fill's own, on the shared listener (below).
  A pass reads the event log's safe horizon once (`events.Horizon`) and writes the rows of the events above the cursor up to that horizon, in id order.
  While the horizon stands below an id that a read of the horizon saw, the holder reads it again after 1 s, then 2 s, 4 s and 8 s, until the horizon passes that id.
  An event write that rolls back ends with no notification, so nothing else wakes the fill for the events that wait behind it.
  A fill that is caught up holds no timer and sends no statement except the renewal of its hold.

- Before each batch the fill asks `sessionanswers.Latency`, which times the answers reads (`GET /api/v1/agents/{session_id}/answers`) that this process serves.
  The route times each read from the moment it enters the route's handler chain, as the access-log line times it.
  The fill pauses while a read that took longer than 800 ms ended within the last 10 s, and while a read has run longer than 800 ms and has not ended.
  It asks again each second, and it continues when neither condition has held for 10 s.
  A batch that runs while a read becomes slow commits, and the pause holds back the next batch.

- The cursor row is the one row of `search_text_versions_fill`: every event at or below its `event_id` has its rows.
  The fill creates the row when it finds none.
  A new holder, in this process or in another, continues from the cursor, so no event gets its rows twice and no event is missed.
  Each batch checks the log against the cursor (`events.EventLogWasReset`).
  When the log was reset, the batch deletes the rows of both tables and writes the cursor row again at event 0 with no reach, in one transaction, and the fill starts again from the first event.
  An event whose payload the rules cannot read stops the fill at that event.
  The fill then tries the pass again after 1 s, doubling to a minute, and logs the error each time.

- A batch reads at most 500 events and at most 2 MB of the text its vectors are made from.
  Each event of a type the rules read counts its payload, and a post whose event cut its body counts the body its present row holds too: the read of the events bounds those.
  An event that starts a version counts what that version's vector reads besides, measured while the rules decide:
  an issue's row counts the markdown of the primary document version it names (version 1 for `issue.created`, the version it carries forward for `issue.updated` and `issue.closed`, the named version for `artifact.version`), and a post whose event cut its body counts the whole body the rules give it, where that is longer than its row's.
  The rules end the batch before the first event that takes it past 2 MB.
  The title that an `artifact.version`'s row carries forward is not counted, at most 3,000 bytes a row.
  An event that counts more than 2 MB is a batch alone.
  A batch is one transaction.
  It locks the cursor row, checks that `duty_holds` still names this holder live, writes the rows, moves the cursor, stores the reach, and commits.
  A holder that lost its hold writes nothing.
  `lock_timeout` is 5 s, and `statement_timeout` is 10 s plus 1 s for each MB the batch counts.
  `idle_in_transaction_session_timeout` is 10 s, which ends the transaction of a holder frozen inside a batch and frees the cursor row.
  After each batch the holder rests for as long as the batch ran, so the fill takes at most half of the time of one backend.

- The fill logs when it takes, loses or releases its hold, when it pauses and continues, when it finds a reset log, and when it keeps a cut body.
  While it catches up, it logs a progress line at most every 30 s.
  When it is caught up, a pass logs at most once a minute the reach, the lag of the reach behind `now()`, and how many events the cursor is behind the newest event.
  It takes two connections of the lease pool: `search text versions fill`, for its hold statements, batches and reads, and `search text versions fill hold renewal`.

**One shared listener per process.**
Every woken loop waits on the process's one `pglisten.Hub`, the shared LISTEN connection the document rooms' room sync is registered on too (named `dispatch-listener <process id>` in `pg_stat_activity`, logged under the owner `dispatch`), which holds one connection from the store's listener pool and LISTENs on every loop's channel beside the room-sync channels.
A channel is registered only for a loop that will run:
`dispatch_embeddings` only with an embedder, `dispatch_delivery_settings` only with NATS and the GitHub App, and `dispatch_github_read_attempts` only with a GitHub App configured.
No loop waits for the listener: each runs its boot pass whether or not it has connected, and keeps its due-time timers while it has no connection.
Postgres does not replay a notification sent while nothing listened, so the hub signals every loop after each successful LISTEN, the first included, and a change made while it was redialing (backing off 1 to 30 s) is caught up then.

**The GitHub intake** (`delivery.Intake`) checks each message's subject before it decodes anything:
only `notifications.github.<owner>.<repo>.pr.<n>`, with nothing after the number, `notifications.github.<owner>.<repo>.workflow.<file>.<action>` and `notifications.github.<owner>.<repo>.push.branch.<branch>` can carry a kind it handles, and every other subject
— checks, comments, issues, a push of a tag, job events, `pr.<n>.review` — is acknowledged with no decode.
A wanted message runs under its key: a pull request under its repository and number, read from the subject, a push of a branch under its repository, read from the subject, and a workflow run under its payload's `repo` and `run_id`.
The intake hands a push of a branch to the architecture importer, which imports every enabled component source on that repository and branch at once rather than at its next five-minute pass (`delivery.PushImports`, `architecture.Importer.Pushed`).
The intake runs only while `delivery_settings` holds a row (`Intake.Run`), so a push is imported this way only where delivery is configured, and elsewhere the five-minute pass is a source's only automatic import.
A push is no delivery fact: a push of a branch calls the importer once, a push of a tag calls nothing, and neither records an `issue.deployed` or moves the Delivery page's event freshness, `last_event_at` (`TestTheIntakeHandsABranchPushToItsImporter`).
A hand-off that fails is logged at `WARN` (`process event`) and the envelope acknowledged, as `Intake.deliver` does for a handler's error other than `ErrHeadNotStored`, since the importer's five-minute pass imports the source all the same.
Messages of one key run in delivery order, each after the one before has finished, since each fetches its pull request's or run's current state from GitHub and writes it, and run at once an older fetch could be written over a newer one;
messages of different keys run at once, at most 4, the durable's `MaxAckPending`.
Its durable's `AckWait`, `MaxAckPending` and filter subject are unchanged.
Every envelope is acknowledged once its handler has run, whatever the handler made of it, with one exception:
a `pull_request` envelope whose pull request's head row could not be stored at all is handed back to NATS instead, after `delivery.headWriteRedeliverAfter` (10 s), for as many as `delivery.headWriteMaxDeliveries` (30) deliveries of it, and acknowledged with an error at the last of them.
No reconcile pass repairs a head row that was never written, since the passes read the rows that exist.
That hand-back is the process's own answer to the durable rather than the stop's, so it is made only while the process still holds the durable's hold and the hold records the deliver subject the envelope arrived on:
a process whose hold has lapsed publishes nothing, and the next holder's take-over, which recreates the durable at its ack floor, hands it that envelope once.
Once the stop has begun, it is withheld too when NATS does not answer within `bus.ConnLockWait` (100 ms) that the connection is connected (`intakeDelivery.handBackOwn`), since a stop waits for the handler that makes it.
Outside any stop, the hand-back is published; on a reconnecting link between attempts it is refused at once (the bus sets no reconnect buffer) with an INFO line, and the envelope comes back after the durable's ack wait (`intakeTuning.ackWait`, 5 min) rather than after 10 s.
While an attempt holds nats.go's lock, the publish waits until the attempt ends.
A message whose acknowledgement NATS refused is handed back the same way.
The count the cap is measured against is the consumer's own (`NumDelivered`), so a take-over starts it afresh and the cap bounds the in-flight slots of one consumer rather than the deliveries of all time.
`Intake.StopDelivering` hands the durable over, and the intake's run loop calls it when the process stops, when its hold is lost, and when a check finds the durable delivering elsewhere.
It ends the subscription the bus client holds for it
— after the client has replaced its connection, the one the client bound on the new connection
— and gives up that subscription's registration on the client, so no reconnect binds it again and a later subscribe starts afresh (`bus.Client.DrainSubscription`).
On a live link it ends it by draining it, so the server delivers it nothing more while nats.go hands the callback what the client had already buffered;
with the link down it unsubscribes it instead, and nats.go then drops what that subscription had buffered without calling the callback at all, so those messages are not handed back and there is no drain to wait for — the take-over that follows recreates the durable at its ack floor, so the next holder is handed them at once (`takeOverConfig`).
Either way it holds every message waiting behind its key, and every message the callback is handed meanwhile, until no handler is running.
The messages in their handlers get until the stop's deadline
— `intakeStopBudget`, 15 s after `Run`'s context ends, armed when it ends
— to finish and be acknowledged; the handlers still running then are given up, their GitHub calls cancelled, and their messages negatively acknowledged.
Only then does it negatively acknowledge the held messages, so NATS redelivers them at once to the next holder, which can then not start a pull request's or run's next message while this process still works on the one before it.
The stop's drain and unsubscribe, and the acknowledgements and hand-backs of the handlers it waits for, whether begun before the stop or after, each wait for NATS at most `bus.ConnLockWait` (100 ms), since nats.go holds its connection lock through a reconnect's dial and handshake: a call NATS has not taken by then goes on without its caller (`callNATS`).
A hand-back publish NATS has not taken in that time is published once NATS takes it (`intakeDelivery.publishHandBack`).
The run loop releases the hold only once every hand-back the stop published has reached the server, which a flush of up to 1 s confirms (`heldDurable.handBacksReached`).
With one still pending at the stop's deadline, or a flush that does not answer, `Run` returns and the hold stays renewing on a goroutine of its own (`heldDurable.finalizeHandBacks`).
It releases only once the hand-back has reached the server and a flush on the connection it was written to answers, or once the connection has closed.
Each hand-back marks the intake client's connection until such a flush confirms it (`bus.Client.MarkUnconfirmed`, `bus.Client.ConfirmMarked`), and a marked socket is reset rather than closed, at `Client.Close` and when nats.go gives the connection up as stale and reconnects:
so the hand-back's bytes still in the kernel's send queue are discarded rather than sent after the release, and a flush on the new socket after a reconnect confirms nothing the old one could still send.
A socket that carries no unconfirmed hand-back closes normally, as every other bus client's does.
The release waits for every hand-back the connection carries, not only the latest delivery's: a take-over the intake made again under the same hold can leave an earlier, stopped delivery whose hand-back no flush has confirmed, so a stop whose latest delivery handed nothing back still flushes while the connection carries a mark (`bus.Client.CarriesUnconfirmed`), and a flush confirms only once every marked hand-back has been taken and none was marked behind its PING.
A hold lost meanwhile — its renewals failing, as when the database is out of reach — cannot be kept, so the intake severs its NATS connection instead (`heldDurable.severHandBacks`, `bus.Client.Sever`):
it resets the connection's sockets without waiting for nats.go's lock, so neither a publish waiting behind a held write nor what the kernel had not sent reaches the server, and the bus client dials a new connection.
The hold reports itself lost `intakeHoldMargin` (2 s) before its row lapses, so the sever comes before any standby can recreate the durable.
Every delivery made under a hold has a watcher of that hold of its own (`heldDurable.watchLoss`), which does this on a goroutine nothing else of the intake blocks: at the loss it seals the delivery and severs unless every hand-back it made has landed and a flush answers within 1 s.
The watcher ends once the delivery's stop has sealed it with nothing handed back, or once a flush confirms its hand-backs, so the take-overs an intake makes again under one live hold (after a create or a token write that fails, or a check that finds the durable elsewhere) leave no goroutine behind; a stopped delivery whose hand-back is still unconfirmed keeps its watcher until the loss.
The sever reaches the connection without the bus client's own lock, which a caller holds while it waits for nats.go's, and a socket whose write failed is reset when nats.go closes it, so its unsent bytes are dropped too.
Dispatch shutdown closes that client, which resets each socket that still carries an unconfirmed hand-back, then waits for the finalizer to release the hold through the still-open pool.
That normally finishes inside the shutdown reserve, but a write can hold nats.go's lock until its one-minute write deadline; `Client.Close` waits for that lock, then the finalizer can spend up to 5 s releasing the hold.
A process killed before that ends the renewal with its connection, and the standby takes over at the lapse, up to the hold's TTL after its last renewal:
a hand-back that reached the server after a standby had recreated the durable would move that message's pending entry on the standby's consumer.
Every one of those answers, a given-up handler's, a held message's and one a refused acknowledgement makes alike, passes one rule (`intakeDelivery.handBack`).
Outside any stop, the hand-back is published; one made while a write holds nats.go's lock is left running and lands when the lock frees, and one on a reconnecting link is refused at once and comes back after the ack wait.
Once the stop has begun it is withheld unless NATS took the stop's drain on a live link, and after that, withheld when NATS does not answer within `bus.ConnLockWait` that the connection is connected:
nothing then says when the server has the subscription's interest gone, and a hand-back would wait for the reconnect.
It is withheld too once the hold's loss deadline has passed, as the head-write hand-back above is:
from that moment the durable the answer names may be one a take-over has recreated, where it would move the new holder's pending entry for that message and have it delivered a second time.
A holder whose stop runs on past the lapse — one frozen and resumed, one whose handler was given up a margin after the deadline — begins no new hand-back, and the take-over's recreate at the ack floor hands messages it left outstanding to the new holder once.
A hand-back that began before the lapse may still be waiting on NATS, and the hold is released only once each such hand-back has reached the server, as above.
Last it waits for a drain it began to end, cut short by that deadline or by a read of whether NATS is connected, made every 10 ms while it waits, that does not answer within `bus.ConnLockWait` or answers that it is not: nats.go ends a drain under its connection lock, which a reconnect attempt holds.
Cut short, it unsubscribes, with an ERROR line, and what nats.go had buffered for the subscription is not handed back and waits for the take-over; on a slow but healthy link, one read that takes over 100 ms does that.
Once the stop's drain has ended, or it has unsubscribed, no hand-back of that subscription begins any more (`intakeDelivery.seal`):
an unsubscribe does not wait for a subscription callback that is still running, and a callback handing back a message the client had buffered is either admitted and counted before the run loop decides to release the hold, which then waits for it, or withholds its hand-back (`intakeDelivery.admitHandBack`).
The run loop then releases the durable's hold once its hand-backs have reached the server, so a standby takes it over at once rather than at the hold's lapse.
A take-over under way when the context ends — a standby's poll
— spends the same deadline rather than starting one of its own:
its own bounds (`takeOverFinishWithin`, 10 s, and the hold's deadline) end it inside that budget, and no attempt starts once the context has ended.
The server's shutdown waits for `Run`'s stop, which begins at the signal and gives handlers 15 s from it, then closes the intake client and waits each pending hand-back finalizer before it deregisters the process and closes the database pool.
That wait can outlive the usual shutdown reserve only while `Client.Close` waits for nats.go's one-minute write deadline; a kubelet kill then ends the client and renewal, and the hold lapses.

## Database migrations

Boot applies `internal/dispatch/store/migrations/*.up.sql` in version order and records each version in `schema_migrations`, which it creates under the runner's advisory lock, so processes booting together against an empty database migrate one after the other.
Version 8 is an empty file: that version was recorded from Go by the one-time conversion of pre-Proof `Y.Text` rooms and offset anchors into Proof trees and mark anchors, which every deployed database has already run.

**A migration must leave the release before it able to read and write the schema.**
A rolling update runs the old build and the new build against one database at once (Running more than one process, above), and the old build applies no compatibility shim:
nothing at boot refuses a schema newer than the binary knows, since the runner applies the versions it has not recorded and judges only the versions it has a file of (`internal/dispatch/store/store.go`, `migrate`).
A later release records the identity of each version's first file (`pgmigrate.Migration.RecordedIdentity`, "A recorded version under another file" below), so what it records of a version the old build has a file of is an identity that file accepts.
So a schema change is expand-then-contract across two releases.
The expanding migration adds, with a default or a backfill, and the release that ships it writes both shapes;
the contracting migration — one that drops, renames or narrows — ships no earlier than the next release, once no running build reads the old shape:

- A `not null` column is added with a default
  — a constant default rewrites no row — and the release that ships it writes the column on every insert;
  the release after it may drop the default, once no build inserts a row without the column.

- A rename is two migrations in two releases:
  the new column added and filled (a backfill for the rows already stored), its release writing both columns, then the old column dropped a release later.
  One `alter table … rename column` breaks every statement the earlier release still sends.

- A narrowing — a type made smaller, a nullable column made `not null`, a check added
  — ships behind a release that writes only the values the narrower shape admits, and narrows in the release after it.

No migration is an exception to that rule: a change that cannot be written that way is split into releases that can, so no release stops every server process.
`0113`, `0117` and `20261111210103` below were each deployed with no server of the earlier release running, under an exception this rule no longer has (`20261111210103` ships in `legion-envoy-v30.11.0`), and a deployment still on a release before one of them crosses it once that way, before it runs more than one replica.
So a release from a build below `20261111210103` onto this one or any later one is stop-the-world: stop every server process of the old release, then start the new one.
The two-build guarantee does not cover that release, and it is an outage for as long as the migration and the new release's start take; it covers every release after it.
The pre-deploy census ("Pre-deploy census" below) is unchanged by that rule and still applies to every migration:
it is taken before any migration of the release applies, and a census that refuses fails the deploy before the service moves.
The rule in brief is `internal/dispatch/store/README.md`, beside the migrations directory rather than in it, since the loader below refuses a set holding a file named anything else.

A migration added from now on is named `<UTC timestamp>_<name>.up.sql`, the timestamp the moment its author created it, `YYYYMMDDHHMMSS` (`date -u +%Y%m%d%H%M%S`), with its census as `<same timestamp>_<name>.census.sql`.
The numbered versions are the sequence the set grew up with;
every one of those files keeps the number it landed with, and no number is added again:
the series closed at `0161_delivery_run_warnings.up.sql`, which `migrationid` records once for Dispatch, and a set holding a number above it is refused (`migrationid.Check`), so a branch that takes the next number fails on its own, before it merges.
Two pull requests open at once used to take one number each and collide only when the first merged
— nothing conflicts in git — while two timestamps taken a moment apart belong to their own authors.
The version is the file name's leading digits read as a number, as before, so every timestamp orders after every number and timestamps order among themselves by time.
A 14-digit version is past what an `integer` column holds, so `schema_migrations.version` is a `bigint`.
A database created before the change keeps its `integer` column until a boot whose set holds a timestamped migration, which widens it once, in the same locked transaction that creates the table (`prepareVersionTable`):
a boot with no timestamped migration leaves it alone, because the release before this change reads `max(version)` uncast and would fail that read once on every connection that prepared it while it still serves.
So this release is rolled out before any release that carries a timestamped migration:
a deployment that skips it costs the release it replaces one failed `/healthz` schema read on each connection that prepared the uncast read, which pgx then drops.
`schema_version` in `/healthz` is still `max(version)`, the highest version applied
— which is not proof that every version below it is applied, since a migration can merge below one a database already has.
The auto-deployer's rollback guard compares the whole set of recorded versions for that reason, rather than `max(version)` (`packages/envoy/deploy/README.md`, "Auto-deployer").

A migration that merges below one a database has already applied still runs there, exactly once, at the next boot:
the runner applies every version `schema_migrations` does not record, not only those above the highest it does, normally in its own transaction with its version row (`TestMigrateAppliesAMigrationThatArrivedBelowOneAlreadyApplied` and the proofs beside it in `internal/dispatch/store/migrate_out_of_order_test.go`).
Within one boot the runner applies the versions the database lacks in version order, so a fresh database applies the whole set in that order;
but a deployed database applies a migration when the release carrying it deploys.
If T2 deploys before T1 merges, that database applies T2 then T1 while a fresh one applies T1 then T2.
So a migration must not depend on its order relative to one written in a concurrent pull request, another branch open at the same time;
one that needs another's effect is written after that one has merged.

The runner applies every version it has not recorded, so before it touches the database it reads the whole directory the binary embeds (`pgmigrate.LoadBuilt`, every check `pgmigrate.Load` makes but the parse of non-transactional migrations below) and refuses to start, applying nothing and naming every file concerned, when two migrations share a version (it would apply the first and skip the rest as already applied;
keep the version on the file that merged to `main` first, since a database may already have applied it, and give each of the rest the UTC moment it was created), when a file is named other than `<version>_<name>.up.sql`, `<version>_<name>.down.sql` or `<version>_<name>.census.sql` (a `.down.sql` is a rollback script an operator runs by hand, and a `.census.sql` is the migration's census, "Pre-deploy census" below;
each needs its `.up.sql`), when a census is not one `select` or calls a function it may not (below), when a version is not decimal digits of a positive number the `bigint` column holds, and when a file cannot be read.
The directory is embedded with `all:`, which keeps a name beginning with `_` or `.`, so such a name is refused like any other.
An editor's swap file therefore stops a locally built `envoy-dispatch` from booting, and fails the store's tests, until it is removed, and a binary built while it was there keeps refusing to boot until it is rebuilt;
Vim writes its swap file beside the file it edits by default.
What no directive embeds, such as a symlink, a `.git` or a hidden name Go's module paths refuse, never reaches the runner, so only the store's tests catch it (`pgmigrate/pgmigratetest.CheckEmbedsEveryFile` names each cause).
Versions are applied by their number, not by file name, and the store's tests require every file on disk to be embedded (`TestEveryMigrationFileIsEmbedded`) and the identifiers to be the numbered migrations 1 to the number the series closed at (recorded once in `migrationid`), each taken once with none above it, and a real UTC instant above them (`TestMigrationSetTakesNumbersThenTimestamps`, `migrationid.Check`), both reading file names alone.

**A recorded version under another file.**
`schema_migrations` records three facts of each applied migration beside its version.
`name` and `checksum` hold an identity: a file's name, without its directory, and the sha256 of its whole text, in lower-case hex.
For a file with no `-- replaces:` line that identity is the file's own; for one with them it is the one its first line names, the version's first file (`pgmigrate.Migration.RecordedIdentity`), so every release whose file of the version is in the chain accepts it.
`applied_checksum` holds the sha256 of the text the runner applied, so a database that ran an edit of the file this build does not hold, such as another branch's, is told apart from one that ran this build's text.
A row the runner fills, which an earlier runner wrote, records no applied text, since nothing says which text ran.
The runner adds the nullable columns itself to a table an earlier runner made, in the locked transaction that creates the table (`prepareVersionTable`), reading the columns first so that the `alter table` runs once per database:
it takes `ACCESS EXCLUSIVE` on `schema_migrations`, and its wait is bounded by `pgmigrate.LockTimeout`, five seconds.
A wait that outlasts it, behind a `pg_dump` or a census, rolls the transaction back and tries it again, up to `pgmigrate.LockTimeoutAttempts` times with a pause between, as a migration is tried (`pgmigrate.RetryVersionTableLockTimeouts`); a holder that outlives every attempt fails the boot with the lock-timeout error, not a refusal.
Adding the columns is a catalog change that rewrites no row; where the same boot also widens the version column (above), once, for a table from before the timestamps, the one statement does both, and the widen rewrites the table's few hundred rows.
Every statement the release before it sends on the table names its columns, so that release serves beside it while it rolls, and the rows it writes record no name.
In the same transaction, before it applies anything, the runner reads every row and judges it against this build's file of the row's version (`pgmigrate.Reconcile`):

- A row whose name is null, which is every row a runner before this one wrote, and one written by hand, is filled with the recorded identity and never refused; a name that is not null, the empty string included, is judged like any other.
  Each fill logs `pgmigrate: recorded the identity of a migration applied before the runners recorded identities` at INFO, with the table, the version, the name and the checksum.

- A row whose identity is neither the file's own nor one its replaces lines name is refused.

- A row whose identity passes, but whose applied text is neither the file's own text nor one its replaces lines name, is refused: the database ran an edit of the file this build does not hold.

- Any other row passes and is left as it is: the release that recorded it still finds what it records there and boots, as a rolling update and the auto-deployer's version-only rollback need, on a database an earlier release created and a later one migrated forward (`TestTheReleaseBeforeBootsOverADatabaseTheNextReleaseBootedWithAReplacedFile`).
  A second edit of the same file keeps the first replaces line first and adds one naming the first edit's identity after it, so the chain covers every text a database may have applied, and the recorded identity stays the version's first.

- A version no file of the build has is left as it is, as before: an image rolled back meets the versions a later one recorded.

The refusal names the version, what the database records and what the build's file is, and both ways out:

```text
migrations refused, none applied: version 20261117022936 is recorded as 20261117022936_legion_current.up.sql sha256:<recorded>, and this build's file of version 20261117022936 is 20261117022936_legion_current.up.sql sha256:<file>: if this database ran another branch's or an earlier commit's migration, or an edited schema migration, reset it (drop it, create it empty and let the server migrate it); if the edit is intended (a replaces line accepts this database as it is, so it is safe only when the edit changes no schema (a comment, say); a database that ran an edited schema migration is reset instead), add this line to 20261117022936_legion_current.up.sql, after the replaces lines it has (its first replaces line names the identity version 20261117022936 first shipped with): -- replaces: 20261117022936_legion_current.up.sql sha256:<recorded>
```

A file whose replaces lines name other identities adds `, whose replaces lines name only <identity>` after its own.
A refusal of the applied text reads `version <v> is recorded as <identity>, which this build's file of version <v>, <file>, accepts, but the text applied under it has sha256 <text>, which neither that file nor its replaces lines name`, and offers the lines that would accept that text.
The runner gives the key back between that judgment and each migration, so another process can record any version in between: before it applies a migration, the runner judges every row again against its whole set, and applies nothing while any row is refused, in either apply mode.
Such a refusal reads `apply migration <version>: check migration: version …`, and the migrations the boot applied before it stay applied.
A lock timeout, or a failed read or write of the table, reads `read or record what schema_migrations records, none applied: …` instead, and asks for no reset.
What the operator does:

- A database that ran another branch's or an earlier commit's migration, or an edit of one, such as a development database or a preview stack's, holds a schema this build never wrote: reset it, by dropping the database, creating it empty, and starting the server, which migrates it from the first file.
  This includes a fresh preview or development database that a newer build created and an older build then boots: the older build refuses it, and you reset it.
  It includes too a development or preview database whose rows recorded no name when a branch's build first booted it under this runner, where that branch edits a migration in place and its file does not yet declare main's identity of it in a replaces line: the boot filled the row with the branch's own identity, not main's, and main refuses that identity, so you reset it.
  A branch whose edited file already declares main's identity, as the pre-merge check requires, fills the row with main's identity, and main boots on it.
  A production database meets this refusal only on a rollback by hand to a release it never ran, one older than the release that first applied a migration it holds; roll back only to a release the database ran, as the auto-deployer does, which rolls back to the image that was running, and only when the deploy applied no version.

- A file renamed, or edited on purpose in a way that changes no schema (a comment, say), whose schema the database already holds, takes the line the refusal names, alone on a line of its own and outside every literal; in a `-- pgmigrate:non-transactional` file it goes after the marker, which stays the first line, and the loader refuses a file whose marker stands on any other line, naming it, since such a file would run in a transaction.
  A replaces line accepts the database as it is, so an edit that changes the schema takes none: a database that ran the earlier text would lack the edit's tables or columns, and is reset as above.
  The first replaces line names the identity the version first shipped with, and a later line goes after the ones there, in their order; the next boot accepts the row as it is.
  The line names a `<version>_<name>.up.sql` of the file's own version and 64 lower-case hex digits, and the loader refuses the whole set, naming the file and line, for a line that begins `-- replaces:` and is written any other way, and for one that Postgres reads as part of a string literal, a dollar-quoted body or a continued string, where it would declare nothing.
  A line ending in a carriage return, as a file with CRLF line endings has, is read without it.
  Before the merge, the `envoy-go-build` job compares every migration the base already holds with the head's file of the same version, building `./internal/pgmigrate/cmd/edited-migrations` and running it with `--base <commit>` (built and run, it exits 2 when the check could not be taken; `go run` reports every failure as exit 1).
  The base is the first parent of the commit the run tests, on a pull request and in the merge queue; on the bypass check's run at main's tip (`main-bypass-check.yaml`, whose push can carry several commits) it is the commit main pointed at before that push, passed as ci.yaml's `migration-base` input, and a dispatch on main that names none fails the check.
  On a dispatch of another branch that names none, the base is the branch's merge base with main (`.github/scripts/edited-migrations-base.sh` picks the base for each run).
  It fails a head file whose first replaces line is not the base's first identity, a head file whose replaces lines do not begin with the base's, in the base's order, then one naming the base's file itself, name and checksum, since each of those is an identity or a text a database the base accepts may record, and a base file whose version the head no longer has a file of, each naming the lines to begin with, so the pull request that edits an applied migration, a comment included, meets the refusal before any database does.
  A revert of an edit is refused there too, since a database the edited release created applied the edit's text: the revert keeps the replaces lines.

**The limit.**
A release cannot accept a text it has never seen, so it refuses a database whose migration a later release applied from an edited text: a database that a later release created from empty when that release had edited a migration, and a production database that skipped the release which first shipped a migration, rolled back by hand to that release.
A release boots on a database a later release filled, since a fill records the version's first identity and no applied text.
That is the same row a development database leaves when it ran another branch's edit, and refusing it is what this check is for.
A production database is created once and booted by every later release, so a rollback onto it to a release it ran, and a rolling update over it, still boot: a later release applies no version the earlier one holds, and leaves each row's applied text as it was.
A fill records the file of the build that boots, not the text the database ran, so an edited text a database ran before the runners recorded identities is not detected: the first boot of this runner records its own file's identity for that version, and refuses nothing.
The two-build tests build their database the same way, in production's order: the previous release migrates it from empty, then this tree migrates it forward, and the previous release boots on that (`laneDatabase`, `cmd/dispatch/twobuild_harness_test.go`); the browser step starts the previous release first for the same reason (`packages/dispatch/e2e/harness-listeners.ts`).
So a pull request that edits a migration the previous release holds, with its replaces line, keeps that lane green, as it keeps production booting (`TestTheReleaseBeforeBootsOverTheLaneDatabaseOfAPullRequestThatEditsAShippedMigration`, whose control is a database the pull request's tree migrated from empty, which the previous release refuses).

Every row a runner before this one wrote has a null name, so a database's first boot of this runner fills each one with the identity this runner records of its file, and refuses none.
A migration edited in place after this runner's first release needs its replaces line, which the pre-merge check requires.
`DATABASE_URL=<url> envoy-dispatch versions`, run as a one-off task of the image about to roll out, reads in a read-only transaction what that image's next boot would do with each row, and exits 1 when a recorded version has no file, a row would be refused, or the table records nothing on the connection's `search_path`, which it reports as `NOTHING CHECKED` rather than as a clean check (`pgmigrate.CheckVersions`); a `search_path` that names no schema is an error, exit 2.
`envoy-broker versions` is the broker's, signing in as a broker pod does (`docs/site/src/content/docs/broker/operate.md`).
The pre-deploy census (`envoy-dispatch census`) reads only which versions are recorded, and judges no identity.

A file whose first line is exactly `-- pgmigrate:non-transactional` runs outside a transaction (`pgmigrate.ExecNonTransactional`).
The store's test `TestTheEmbeddedMigrationsPassEveryCheckLoadMakes` holds the embedded set to `pgmigrate.Load`, which accepts that marker only on one non-unique `CREATE INDEX CONCURRENTLY IF NOT EXISTS` statement, parsed as PostgreSQL parses it; UNIQUE indexes, multiple statements and other commands refuse the whole set there.
A start does not repeat that parse, which compiles libpg_query's WebAssembly, for a set that cannot change after the build: a marked migration the database has not applied is parsed when its turn comes, and refused before its statement runs (`pgmigrate.ExecNonTransactional`), after the pending migrations before it have been applied.
The loader, at a start as in that test, refuses, naming the line, a file that holds the marker as a comment of its own on any other line, as one does with a replaces line written above it: that file would run in a transaction, and its concurrent index would fail on a fresh database.
The runner owns and closes that connection and holds the session-level advisory lock on the same migration key through the build and its separate version insert, excluding transactional runners and schema-version readers too.
Every runner takes that key by polling `pg_try_advisory_lock` as statements of its own, with nothing open between two tries, and never by a blocking call: a backend blocked on a lock keeps the snapshot its statement took, and a concurrent index build waits for every older snapshot, so a blocking wait and the build that holds the key deadlock (SQLSTATE 40P01, measured on PostgreSQL 18.6).
That holds at each of the three places a boot waits for the key (`prepareVersionTable`, `applyMigrationOnce`, `applyNonTransactionalMigration`) and at the auto-deployer's schema read.
If recording the version fails after a successful build, the next boot safely repeats the idempotent statement.
An INVALID index of the migration's own index name, on the table the migration builds on, is the runner's own leftover, from a build its lock bound cancelled or a boot that was killed mid-build: the runner drops it with `drop index concurrently if exists`, logged at WARN, and builds the index again, so one failed build no longer stops every later boot.
An index of that name on another table, valid or INVALID, is refused with a schema-qualified `DROP INDEX CONCURRENTLY` recovery command, because `IF NOT EXISTS` skips on any relation of that name in the schema and the index would never be built on the target table.
A relation of that name that is no index at all is refused by name, with its relkind, to be renamed or removed: `DROP INDEX` on a table fails, so the runner does not offer it.
An index of another name, INVALID or not, is neither dropped nor refused.
The broker's all-in-one-transaction runner refuses a marked file before applying any migration.

Every migration's lock waits are bounded at five seconds (`pgmigrate.LockTimeout`), set after the runner's advisory lock: transaction-local in `pgmigrate.Exec`, or session-local and restored on return in `pgmigrate.ExecNonTransactional`.
A migration queued behind a long transaction gives up instead of holding every read and write of its table behind its request.
The runner then rolls the transaction back or closes the owned connection, holds nothing for two seconds (`pgmigrate.LockTimeoutRetryPause`) so queued requests run, and tries again, up to three attempts (`pgmigrate.LockTimeoutAttempts`, `pgmigrate.RetryLockTimeouts`).
A build that failed, or one still running, may leave an INVALID index of the migration's own name, which the next attempt drops and builds again; each of the drop's lock waits is bounded at the same five seconds and watched the same way, so a drop queued behind a lock is a timed-out attempt like any other.
An attempt is several lock waits, the drop's and the build's, so three attempts bound no total: five seconds bounds each wait, not the migration.
Each timed-out attempt is logged at WARN.
A migration that times out on every attempt fails the boot; each of its lock waits lasted at most five seconds, so it has held its table's reads and writes for no single wait longer than that.
The failure names the migration, the lock it wanted, the sessions it was queued behind with each one's diagnosis (as the census's refusals carry it, "Pre-deploy census" below), and a `pg_stat_activity` query for the sessions holding the lock;
end the holder or let it finish and start the server again.
That query can come back empty: a prepared transaction's locks carry no pid, so the join to `pg_stat_activity` drops them (LEGION-432).
A migration that needs another bound sets its own `SET LOCAL lock_timeout`, which each attempt sets again.
The secrets broker's runner applies every migration in one transaction, so it does not try again:
rolling one attempt back would roll back the migrations before it too.

The runner also applies each migration with `standard_conforming_strings` on, `backslash_quote` at `safe_encoding` and `client_encoding` at `UTF8` (`textSettings` in `internal/pgmigrate/lock.go`), set in the statement that sets `lock_timeout`: transaction-local in `pgmigrate.Exec`, or session-local and restored on return in `pgmigrate.ExecNonTransactional`.
So Postgres reads a migration's text as the census reads it ("Pre-deploy census" below), whatever a session setting, a connection option or a database default says.
A `set` without `local` in a migration stays on its connection after the migration's transaction ends, until the runner resets the connection's session (below).
So no later migration meets the setting.
With `standard_conforming_strings` off, a backslash before a quote in a plain literal escapes that quote, so a text that the census reads as one statement can hold a second statement for Postgres.
With `client_encoding` at `SJIS`, the last byte of a UTF-8 character and a backslash after it can be one character, so that backslash escapes nothing.
With `backslash_quote` off, Postgres refuses a `\'` in an `E'…'` literal, which the census reads as an escaped quote.
The runner needs a database whose encoding holds every character of the migrations it applies: `UTF8`, or `SQL_ASCII`.
With `client_encoding` at `UTF8`, Postgres converts the text of each migration to the database's encoding, and it stops at the first character that this encoding does not have.
On PostgreSQL 18.6, `select 1 -- em dash —` fails on a `LATIN1` database with `character with byte sequence 0xe2 0x80 0x94 in encoding "UTF8" has no equivalent in encoding "LATIN1"`, and it runs on a `SQL_ASCII` database.

At the start of each migration the runner also gives `session_authorization`, `role` and `search_path`, in that order, the values that `reset` gives them (`resetSettings` in `internal/pgmigrate/lock.go`): transaction-local in `pgmigrate.Exec`, or session-local and restored on return in `pgmigrate.ExecNonTransactional`.
Postgres resolves a name that a migration writes without its schema by these three settings.
`search_path` lists the schemas that a lookup searches, and its default starts with `"$user"`, the schema named for the session user, which `session_authorization` sets.
`role` sets the current user, whose privileges decide which of those schemas a lookup can use.
`session_authorization` comes first, because a change of it ends a `set role` and changes the user that `role` and `"$user"` depend on.
A migration can change each of the three for the session, with a `set` or through stored code that calls `set_config`, such as a column default, a check constraint, or a domain's check or default.
The change stays on the connection after the migration's transaction ends, until the runner resets the connection's session (below), so no later migration meets it.
The runner still gives the three these values at the start of each migration, whatever session the pooled connection brings.
Without either reset, on PostgreSQL 18.6, an insert ran a column default that put another schema first in the `search_path`.
The next migration on that connection added a column of type `mood`, which then named that schema's domain `mood` in place of the enum that the census read, and Postgres rewrote the table.
The census reads with the same three values ("Pre-deploy census" below), so a migration resolves its names as the census did, whatever an earlier migration did to the session.
The value that `reset` gives comes from a connection option, the role's default or the database's default, and never from a session `set`.
So the census and the runner must connect as the same login with the same connection options, which gives both the same values.
In a deployment, both take `DATABASE_URL` from the same Secret.
The runner gives the three these values for its own check and record of a migration's version too: before the check, and again after the migration's statements (`pgmigrate.Pin`), since stored code that a migration runs can change them for the rest of its transaction.
Without the second reset, stored code that put another schema with its own `schema_migrations` first in the `search_path` made the runner record the version in that table.
The runner does not give the three these values between two statements of one migration, so stored code that one statement runs can change what the names of a later statement of the same migration resolve to.
In a migration that the census exempts, only an added column's default runs code.
The census holds that default to a constant, a cast of a constant, an SQL value function or a stable or immutable `pg_catalog` function with no argument.
So such a change reaches a later statement of an exempt migration only through a cast whose function is user-written and not volatile.
Only an earlier release can have created such a cast, because a pending `create cast` stops the census's catalog reads ("Pre-deploy census" below).

After each migration's transaction ends, the runner gives its advisory key back and then resets the connection's session, before the connection goes back to the pool (`pgmigrate.ResetSession`).
The reset runs `discard all`, which gives every setting its `reset` value, ends a `set role`, and drops the temporary tables and prepared statements that the migration left.
It then runs pgx's `DeallocateAll`, because the pool runs in cache-statement mode, and pgx would otherwise name prepared statements that the server dropped (SQLSTATE `26000`).
The reset runs after the unlock, because `discard all` also releases the session's advisory locks.
A connection whose reset fails is closed, not given back.
The non-transactional path closes its connection, so it needs no reset.
The secrets broker's runner resets its connection the same way, after the one transaction in which it applies its whole set.
Without the reset, a migration's `set statement_timeout = 0` without `local` stayed on the pooled connection for the service's next queries.

The notes below describe some of the migrations, numbered and timestamped alike; not every migration has one.
A new migration describes itself in the header comment of its own `.up.sql`, as `0129_document_edit_notices.up.sql` does, and adds nothing here, so two pull requests that each add a migration do not edit this section.

Migrations `0056`–`0062` make search indexing linear.
Each table has its own migration, so its transaction holds an `ACCESS EXCLUSIVE` lock only for the `DROP EXPRESSION` and trigger setup;
`0062` re-indexes only rows holding sixteen or more underscore-joined segments under `ROW EXCLUSIVE`.
Their censuses answer `0` for 0056–0061, which neither refuse nor rewrite a row, and, for 0062, the candidate rows whose stored vector the new expression changes.

Migration `0063_doc_settlements_pending` creates the table in which every durable document update records the settlement it owes, which the settlement deletes when it commits.
A room's load and the server's minute-by-minute resumption arm the settlement a row names, whether or not the document's issue is closed, so one that a shutdown or a close cuts short still runs.
It creates a table and touches no row; its census answers `0`.

Migrations `0064`–`0066` let an approval request follow its document's versions.
`0064` backfills `requested_version` into every approval ask from its `version`, rebuilds `asks_approval_kind_check` to require it, and adds the nullable `asks.handed_back_reply_id` and an insert trigger that gives an older binary's approval row the same value; it locks `asks` alone.
`0065` sets `comments.created_at`'s default to `clock_timestamp()`, so a comment is stamped when its insert runs, under the owner lock every comment write takes, and an ask's newest reply is the one that committed last; it locks `comments` alone.
This build's two comment inserts (`queueCommentRow`, which the comment route and an issue write's own comment take, and `replyComment`) read `clock_timestamp()` under that lock before the insert, plan their writes on it and write it explicitly, so their comments take the same order.
`0066` adds `handed_back_reply_id`'s foreign key to `comments`, which takes `SHARE ROW EXCLUSIVE` on both tables, as adding any foreign key does:
a write waits behind it, a read does not.
In one migration, the column's key held `asks` `ACCESS EXCLUSIVE` while it waited for `comments`, and a comment write that went on to read `asks` deadlocked with it.
A comment write that goes on to write `asks` (accepting a suggestion, or an edit under a comment's anchor, whose version moves the approval request) can still close a cycle with `0066` while it waits for `comments`, and under the five-second bound either side could lose it:
Postgres checks a waiter once, `deadlock_timeout` (1 s by default) after it starts to wait, so a write that closes the cycle within that second fails `0066` and one that closes it later is itself the victim, answered `500`.
`0066` therefore sets its own `lock_timeout` of 500 ms, shorter than `deadlock_timeout`:
it gives up first (`55P03`, nothing applied), the write finishes, and the runner's next attempt, or the next boot, applies it;
the cost is that a comment write holding `comments` past half a second on every attempt fails that boot too, and so does an autovacuum of either table where `deadlock_timeout` is not shorter, which the census refuses (below).
Every census answers `0`: 0053's check guarantees every approval ask a `version` to backfill from, and the other two change no row.

Migration `0067_user_agent_reply_read` creates the table of replies a human has read by id, which the broadcast page writes for the replies it shows and the unread count leaves out, beside the per-session read mark, and its `(login, session_id)` index, which the read mark's prune of one session's rows reads.
It creates a table and touches no row; its census answers `0`.

Migration `0068_people` creates `people`, everyone who has signed in, holding the sign-in pool's refresh token for a person who signed in through it, and drops `users`, the GitHub OAuth token pair of each GitHub login that signed in, which the GitHub proxy acted with before it read GitHub as the App.
It locks `users` alone, `ACCESS EXCLUSIVE` for the drop; `people` is new.
Its census counts the `users` rows the drop deletes.

No migration retires the refresh tokens that `0068`'s release stored in plain text:
every boot does (Identity, above), since a task of that release can write one during the roll, or after a rollback, once every migration has run, and a token written after a boot waits for the next one.
So once the last task of the earlier release is gone, the deployer restarts Dispatch once and then checks three things:
that boot logged `failed=0`; no `a sign-in replaced a refresh token the pool did not revoke` line has appeared since the roll began (each one names a person to sign out at the pool);
and `select count(*) from people where refresh_token is not null and refresh_token not like 'v1:%'` answers `0`.
A boot line with `failed=<n>` above zero names each token it could not revoke in an ERROR line;
the next boot tries again, and signing that person out at the pool ends it at once.
A non-zero count with no such line is a token a task of the earlier release wrote after the last boot, which another restart retires.

The boot revokes only the plain-text tokens still in `people`.
Under `0068`'s release a sign-in that replaced a person's token, or a logout that cleared it, removed it without revoking it, and it stays valid at the pool until it expires in any backup that holds it.
So if any database backup was taken between `0068`'s deploy and the post-roll restart, either delete those backups or sign out at the pool everyone whose `people.signed_in_at` is after `0068`'s deploy.

Migration `0071_agent_artifacts` gives an artifact a third owner, an agent's conversation:
the nullable `artifacts.session_id`, a check that every artifact has exactly one owner (an issue, `issue_key` with its `project_key`;
a project, `project_key` alone; or a session, a non-empty `session_id` alone), and `project_key` made nullable for the session's rows.
`ref_key`, 0009's generated column, becomes a plain column that a `BEFORE INSERT OR UPDATE` trigger (`artifacts_ref_key`) fills, as `0058` did for `search`:
`DROP EXPRESSION` keeps every value and rewrites no row, the trigger computes 0009's expression for every other row and `agent/<session id>/<slug>` for a session's, and `artifacts_ref_key_key` keeps it unique.
It adds the partial index `artifacts_session_id`, which serves the two lookups an upload to an agent's conversation makes by `session_id` (its artifact by name, and whether a slug is taken).
It locks `artifacts` alone, `ACCESS EXCLUSIVE` for milliseconds while the check reads its metadata rows;
its census counts the rows the check would refuse, `project_key is null`, which 0009's `not null` keeps at `0`.

Migration `0082_answers_by_person` adds two partial indexes for `GET /api/v1/me/answers`:
one on `events` for user-authored `ask.answered` events, which the route filters to events whose actor is the answer's user, and one on `comments` for user replies on asks.
Building them takes `SHARE` on `events` and `comments`; its census answers `0` because it refuses and rewrites no row.

Migration `0087_search_vector_bound` creates `search_vector` and replaces the bodies of four search triggers' functions (documents, comments, asks and messages) so that each builds its vector with it ([Search](#search)).
Replacing a function locks no table, and for every text whose vector fits, which is every row already stored, the new functions build the vector the old ones did, so it re-indexes nothing; its census answers `0`.

Migrations `0088_issues_title_lexemes` and `0089_issues_title_lexemes_backfill` store each issue title's lexemes, which the duplicate-title check reads ([Search](#search)).
`0088` adds `issues.title_lexemes` with the constant default `{}`, a catalog change that rewrites no row, so it holds `issues` `ACCESS EXCLUSIVE` for milliseconds (3 ms on 2,905 issues with production's titles), creates `title_lexemes(title)`, the one definition of a title's lexemes, and has the issues trigger build `search` with `search_vector` and fill the column on every insert and every retitle.
`0089` fills it on every issue stored before, under `issues` `EXCLUSIVE` taken before it writes a row:
a read does not wait, while every write of an issue's row, and of a row that references an issue, waits until it commits (0.58 s on those issues at load 85).
The update alone locked each issue's row in the order rows lie on disk, which a reparent, locking its two issues in key order, could cross:
on 60,000 issues the two deadlocked (`40P01`) in each of two runs, and with the table lock the reparent waited for the migration and committed.
Neither refuses a row; both censuses answer `0`.

Migration `0091_artifacts_project_documents` adds the partial index `artifacts_project_documents`, on `artifacts (project_key)` where `issue_key` and `session_id` are null: a project's unlinked documents.
Settlement of a project document holding an ask block no ask of its own indexes reads its project's other documents through it, for the ask the block may be a copy of and for the copies of an ask it retracts, and so do the answer and resolve routes, for the copies of the ask they close (LEGION-651).
Building it takes `SHARE` on `artifacts`; its census answers `0` because it refuses and rewrites no row.

A decided ask that a release before LEGION-651 reworded gets back, in code, the wording its decision was given on:
the `previous` wording of the first `ask.edited` after its latest `ask.answered` or `ask.resolved`, with no `ask.opened` between.
No migration rewrites those rows, since the census refuses one that does (Pre-deploy census, below).
Every boot runs the restoration over every decided ask in one statement (`docs.Service.RestoreDecidedWordings`, bounded at 30 s), reading `events` through `events_ask_payload_id` and writing only the rows whose wording differs;
it logs `dispatch: restored a decided ask's wording` for each with the ask, its document and the question it asks again.
In the same transaction it marks owed, in one upsert, the settlement of each restored block ask's own document, whose block still asks the rewording beside the decision, and of every other document of each restored ask's owner whose latest version holds the ask's block with no ask of its own under that id, a copy that may have matched the rewording and shown the decision on it, and arms those settlements once it commits; then it logs `dispatch: restored
decided asks' wordings` with `restored=<n> copies=<n>`.
On the 2026-10-11 production dump (3,739 asks, 82,153 events) it restores 5 asks, each a block ask on a document of its own, and marks 0 copies owed, in 0.17-0.77 s:
the one other document holding one of those 5 blocks has an ask of its own under that id, so it is no copy, and the first boot of the release that carries it logs `restored=5 copies=0` and settles those 5 documents;
each whose block still asks the rewording gets the decided wording written back and logs `dispatch: wrote a decided ask's wording back into its
block`.
Each document's settlement runs the same statement over its own decided asks before it reconciles them, and marks the restored asks' copies owed the same way, so a rewording a task of the earlier release writes during the roll is undone by that document's next settlement on this release, which also writes the decided wording back into the block and settles its copies again.

A close by a release before the withdrawal left its issue's asks open, and a reopen by one left the asks a close withdrew resolved.
No migration writes the withdrawals, since the census refuses one that rewrites rows (Pre-deploy census, below);
code repairs them instead (`api.RepairIssueAsks`).
Every boot runs a pass, bounded at 30 s, and then every 10 minutes (`api.IssueAskRepairInterval`) one process runs another under the lease `issue-ask-repair`, bounded at 5 minutes, for the life of the process.
Each issue is repaired in its own transaction holding the issue's row:
a closed issue holding open asks gets them withdrawn exactly as a close withdraws them, attributed to the actor of its latest `issue.closed` (an issue with no such event is logged and skipped), so the asks left open on issues closed before the release are withdrawn naming each issue's earlier closer;
an open issue holding `withdrawn` asks gets them restored, attributed to the actor of the event that reopened it;
and a closed issue that holds open asks, or whose documents owe a settlement marked since the last pass (at boot, the last 10 minutes;
on the first interval pass, any), gets the close's final sweep in the same write of each document, which writes block `state` attributes only, never an ask row or a record.
The sweep's write leaves the document owed, and the document's settlement saves that write whether or not the issue is closed.
A document whose stored history does not load is logged and left as it is, as the close leaves it.
Each record is dated when the pass writes it.
A pass logs `dispatch: repaired the asks of closed and reopened issues` with `withdrawn=<n> restored=<n>
swept=<n>`; one that fails or runs out of time is logged, and the next pass repairs what it left:
each interval pass that listed its candidates moves the bound to one interval before its own start, and carries every issue it did not repair, failed or not reached, into the next pass.
The bound and the carried issues live in the memory of the process that ran the pass, not in the database:
when the lease moves to another process, that process's pass starts from its own bound and its own carried issues (on its first interval pass, no bound, so it sweeps every closed issue whose documents owe a settlement), and never sees the issues the other process carried.

While a release before this one serves beside it, a reopen made by a process of that release runs none of what this release's reopen does once a settlement of the closed issue's document has saved it:
no reopen mark (`docs.Service.MarkReopenOwedAskDocuments`), no move of the approval request and no carry of the approval to the version that settlement wrote (`docs.Service.FollowClosedIssueVersions`), and no deletion of the editors such settlements kept.
So a document that a closed issue's settlement saved waits for its next edit for the asks of its blocks that no settlement indexed, for the retraction of a restored ask whose block left it while the issue was closed, and for the move of its approval request or the carry of its approval.
Migration `20261115091510_doc_closed_edit_sources` creates `doc_closed_edit_sources`, the editor that a settlement of a closed issue's document credited, which this release keeps when that settlement deletes the document's pending row, and which the settlement after the reopen names on the ask events of the text that editor wrote (`docs.clearSettlementPending`, `docs.settlementPendingStatement`).
This release names that editor on no version that adds none of their text: a version that lists no one, whose latest edit source is only the kept editor, is written in the name of `SettlementActor` (`docs.settlementVersionCredit`).
The earlier release neither reads nor deletes the table:
a pending row that its reopen's write inserts names no editor, so the settlement after such a reopen names `SettlementActor` rather than the kept editor, whichever release runs it, and the kept editor stays until this release's next settlement of the document deletes it;
a pending row that a write of this release inserts before then, naming no editor of its own, names that kept editor, and a settlement of that row by the earlier release names that editor on its version too, since that release does not tell the kept editor apart.
The table is new and its foreign key cascades with the document, so no row is refused or rewritten, and its census answers `0`.

Migration `0092_refs_excerpts` adds three nullable columns to `refs` with no default (`excerpt_block_id`, `excerpt_text`, `excerpt_version`), a catalog change that rewrites no row, and the partial index `refs_excerpt_drift` over the rows whose `excerpt_version` is null:
the stored excerpt `GET /api/v1/references` answers a document mention with, which every version write fills and the reference-excerpt reconciliation fills for every row stored before it (`docs.Service.RunReferenceExcerptReconciliation`).
On the 2026-10-10 production dump the reconciliation's first pass filled 7,498 mentions of 2,150 documents in 248 s at `GOMAXPROCS=2`.
Its census answers `0`.

Migration `0094_issue_links` creates `issue_links`, the issues each issue waits on through `blocked_by`, and its target index, and re-states the `graph_edges` view with the `blocked_by` arm.
The table's two foreign keys take `SHARE ROW EXCLUSIVE` on `issues` for the migration's short transaction, so an issue write waits behind it while an issue read does not, and re-stating the view holds `graph_edges` `ACCESS EXCLUSIVE`, so a reference read waits too.
It touches no row; its census answers `0`.

Migrations `0095_user_threads` through `0099_user_threads_catch_up` build `user_threads`, the table behind the Inbox's Reply rows, which `20261105093500` through `20261105093503` give its dismissal mark and the two indexes the Inbox list and the Dismissed view read.
`0095` creates the table, its partial index `user_threads_unread` and its `(thread_id)` index, the functions `user_threads_login`, `user_threads_record`, `user_threads_comment` and `user_threads_message`, and the trigger on `comments`;
`0096` adds the trigger on `asks` (`insert or update of answer`); `0097` attaches `user_threads_message` to `messages`.
Each trigger is its own migration, so its transaction holds that one table's `SHARE ROW EXCLUSIVE` alone:
a write to it waits behind the migration and a read does not.
`0098` seeds a row for every person and thread they wrote in, under `ACCESS SHARE` on `comments`, `asks`, `messages`, `issues` and `artifacts`, writing only `user_threads`;
every row starts read, its read mark moved to the newest reply already stored, so the deploy turns no stored reply into an unread one.
The triggers are live by then, so a write that lands while it runs on a thread whose participant already has a row is caught by the trigger, and the seed merges with it by taking the later of each time.
A participant the seed is still inserting has no row the trigger's update can see, and the seed's snapshot cannot see that write, so `0099` reads the same writes under `ACCESS SHARE` on `comments`, `asks` and `messages` and moves each row to any write by someone else newer than its `replied_at`, which makes it unread past its read mark;
it moves nothing backwards, and a write the seed saw stays read.
On a copy of the 2026-10-12 production dump (11,497 comments, 3,882 asks, 14,289 messages) the seed wrote 3,819 rows for three logins, all of them read, in 0.93 s, and `0099` ran in 0.11 s under the server's own runner and moved no row, taking `ACCESS SHARE` on the three tables it reads and `ROW EXCLUSIVE` on `user_threads` alone.
All five censuses answer `0`: `0095`-`0097` create functions and triggers and refuse or rewrite no row, and `0098` and `0099` write only the table `0095` creates.

Migration `0113_repo_projects_default_and_issue_repo` adds `repo_projects.is_default` and `issues.repo`.
From its release on, an issue sits at most 32 links below its root, and the API refuses a write past that (`409 PARENT_INPUT`);
the release before it did not, so `0113` refuses to apply while any issue sits more than 32 links below its root or in a parent loop, naming up to 20 of them and the repair:
`PATCH /api/v1/issues/<ancestor>` with `{"parent": null}`, on the release still running.
Its census counts the same issues.
It takes `repo_projects` and `issues` in `ACCESS EXCLUSIVE` before it counts and holds both to commit, so a write of the earlier release that commits while it waits is in the count.

**`0113` was deployed with no server of the earlier release running, and no release older than it is started over it again.**
That instruction was `0113`'s own, not the rule for `0114` and after:
`0113` is applied in production, and a release whose migrations are written to the rule above rolls (Running more than one process, above).
Its own deploy stopped the earlier release, or scaled it to zero, before the new release booted and migrated
— for that deploy, a Kubernetes Deployment's `strategy: Recreate`.
A failed deploy recovers forward, not by rolling the image back.
Only the new release's API holds the limit; nothing in the database does.
Boot has no check that refuses a schema newer than the binary knows:
the runner applies the versions it has not recorded and reads no other (`internal/dispatch/store/store.go`, `migrate`).
So an earlier release left serving after `0113`, or started again over it by a rollback, writes as it always did, and can build a chain past the limit again.
Such a chain, or one written by direct SQL, fails every read of its issues with `500`, a project's list included.
The API repairs an acyclic one: `PATCH` an ancestor that still reads (its `GET` answers `200`) with `{"parent": null}`.
The `PATCH` reads the issue it changes first (`api/issue_patch.go`), so a parent loop none of whose members reads, which only direct SQL can write, needs a database repair:
`update issues set parent_key = null where key = '<a member>'`.
The auto-deployer (`packages/envoy/deploy/README.md`) keeps both rules.
Compose stops and removes the old `dispatch` container before it starts the new one.
When a deploy fails, the auto-deployer stops the failed image and confirms the stop before it reads the schema, and when the schema has moved past the one the previous image runs against, it refuses to start that image again and exits `1` for a forward recovery.

Migration `0117_architecture_sources_per_repo` lets a project import its components from more than one repository.
`architecture_sources` gains `id`, which names a source within its project, and its primary key moves from `(project_key)` to `(project_key, id)`, with a unique index on `(project_key, repo)`, so a repository is a source of a project at most once.
`components` gains `source_id` and a foreign key on `(project_key, source_id)` to its source's row, `on delete cascade`, so deleting a source deletes its components.
Both columns are added with the constant default `0`, which rewrites no row, and the default is then dropped:
every existing source reads `id` 0, every existing component `source_id` 0 (its project's one existing source), and a source added later gets an `id` from 1 on.
It takes `architecture_sources` and `components` in `ACCESS EXCLUSIVE` first and holds both to commit.
Its census counts the components whose project has no source, the rows the foreign key would refuse:

```sql
select count(*) from components c
where not exists (select 1 from architecture_sources s where s.project_key = c.project_key);
```

Only direct SQL writes such a component: the importer writes components under their source's row lock, and deleting a source deletes its components.
A census that refuses on it is repaired on the release still serving, before the deploy, by deleting them:
`delete from components c where not exists (select 1 from architecture_sources s where s.project_key = c.project_key)`; then take the census again.

**`0117` was deployed with no server of the earlier release running:
deploy it that way, and never start a release older than `0117` over it again.**
A deployment still on a release before it crosses it once with Dispatch stopped, before it runs more than one replica.
The last such migration is `20261111210103` (below), and every release after it rolls.
Stop the earlier release, or scale it to zero, before the new release boots and migrates:
a Kubernetes Deployment's `strategy: Recreate`, which `envoy-infra:gcp:Dispatch` uses at its default one replica (`packages/envoy/infra/README.md`);
a deployment running more than one replica, which rolls, is scaled to zero first.
The compose auto-deployer above stops the old container before it starts the new one.
A failed deploy recovers forward, not by rolling the image back.
The earlier release does not work over the new schema, and nothing at boot stops it from serving:

- Its save of a project's source upserts `on conflict (project_key)`, which no unique index covers once `0117` commits, so Postgres refuses the statement and the save answers `500`.

- Its import takes one source row of the project as the project's one source, whichever row Postgres returns first.
  When the model changed, it deletes every component of the project, every source's, and inserts that source's components with no `source_id`, which has no default.
  A model holding a component fails on the not-null column and rolls back, and its failure record writes the error onto every source row of the project.
  A model holding none (no file in that branch's `.dispatch/architecture/`) commits the delete, and every source's components are gone.

- Its delete removes every source of the project and every component of the project.

Migrations `0118_embeddings_pending_since` and `0119_drop_embeddings_backfill_progress` move meaning search's coverage into the server (Search, below).
`0118` adds `embeddings.pending_since` (`not null default now()`:
when the row last became pending, which the coverage route's oldest wait reads;
a row pending at the migration takes its own `created_at`) and `embeddings.dead_model` (nullable:
the embedder identity the row was confirmed dead under), and replaces `embeddings_enqueue` so that a text change also starts the row's wait again and clears `dead_model`.
A row already dead at the migration records `us.cohere.embed-v4:0`, the Bedrock identity every earlier release embedded with, so a deployment that keeps Bedrock does not re-admit it, while a switch to Azure re-admits it once;
a row the earlier release confirms dead during the roll records none and gets the new release's attempts once.
Both columns are additive and the earlier release names neither: it inserts only through `embeddings_enqueue` or with an insert the default fills.
`0119` drops `embeddings_backfill_progress`, the checkpoints of the removed `backfill-embeddings` subcommand.
Dropping a table in the same release as the code that stopped using it is what the rule below forbids for a table a running build reads;
no server of any release read or wrote this one, only that person-run subcommand did, so the drop breaks no build still serving through the roll.
Both censuses answer `0`: `0118`'s updates write only the columns it adds, and `0119` drops nothing a running build still reads.

Migration `0120_artifact_reviews_event_seq` adds `artifact_reviews.event_seq`, the sequence of the `artifact.*` event each review appended on its document's owner, which `docs.QueueReview` writes in the transaction that appends the event and the approval read returns as `seq`.
It is nullable, so a review recorded before it reads with no `seq`, and the release before it, which names no `event_seq` in its insert, rolls beside it.
Adding a nullable column with no default writes the catalog alone, under `artifact_reviews`' `ACCESS EXCLUSIVE` lock for milliseconds;
it refuses and rewrites no row, and its census answers `0`.
Its `event_seq > 0` check is a second statement, added `not valid` as the rule below requires:
an inline check would read every row of the table while it held that lock, and `not valid` still refuses every row written or updated after it.
Nothing validates it later — every row below the migration carries no sequence at all — so the constraint stays `not valid` for good.

`0120` also creates `artifact_reviews_by_artifact_event_seq` on `(artifact_id, event_seq desc nulls
last, created_at desc, id desc)`, `docs.ReviewOrder` per document, so the newest-review reads (`docs.LatestApprovedReview`, `latestReviewBy`) take one index entry rather than sort every review of the document.
It is a plain `create index`, not `concurrently`:
this unmarked migration runs inside one transaction (`internal/dispatch/store/store.go`, `pgmigrate.Exec`), where `concurrently` cannot run.
The build takes no lock the migration does not already hold
— its `alter table` statements hold `artifact_reviews`' `ACCESS EXCLUSIVE` — and holds it while it reads the table's rows, which the census bounds by its 1 GiB limit;
the census still answers `0`, since the build refuses and rewrites no row.
`artifact_reviews_by_artifact` (`0014`, `created_at desc, id desc`) stays, because the release before `0120` orders by it while it serves.

The release before `0120` keeps serving beside it while the app Deployment rolls (below), and again for as long as a rollback lasts, and it inserts every review naming no `event_seq`.
Left null, such a review — newer than every sequenced one
— would rank below them all in `docs.ReviewOrder` (`event_seq desc nulls last`):
a person's approval an old pod wrote would read back as the changes requested before it, and a changes request an old pod wrote would be overridden by the approval before it.
So `0120` also adds a trigger, which the rule below allows a migration to add:
`artifact_reviews_event_seq`, a constraint trigger on `artifact_reviews` deferred to the commit, gives every review inserted without a sequence, by either release, the sequence of the `artifact.*` event its own transaction appended for it (its type, document and version, among the events on the owner that its transaction wrote, which the owner's row lock keeps the newest there until the commit).
An event is the transaction's own when it carries the transaction's `now()` and its row's `xmin` is the transaction or one of its subtransactions (`artifact_reviews_own_row`, which reads it through `pg_xact_status`, so a savepoint's event counts and another transaction's does not, even one that started in the same microsecond and so shares `now()`).
It is deferred because every release inserts a review before it appends the event.
Every row that carries no sequence was therefore committed before `0120`, whose `ACCESS EXCLUSIVE` lock waits out every transaction that has inserted a review, and every sequenced row is newer.
The trigger is on `artifact_reviews`, the table `0120` already locks, so the migration holds no second lock:
a trigger on `events` would need that table's `SHARE ROW EXCLUSIVE` while holding `artifact_reviews`, against a transaction that appended an event and then waits to insert a review (a carried approval), and the census holds every table a migration names to its 1 GiB limit.

`0120` is additive — a nullable column, a `not valid` check, an index and a trigger
— so rolling Dispatch back past it is an image rollback alone, with no schema to restore and no backup to take for it:
the release before it runs over the new schema unchanged, and the trigger gives every review that release writes its sequence.
The auto-deployer is the exception, by its own rule above: once the schema has moved it refuses to start the previous tag, and recovery there is forward.

Migration `0121_block_graph` creates the tables behind a document's blocks as graph nodes ("Reference graph" below):
`document_blocks`, `block_refs`, `document_block_versions`, `document_block_texts` and `document_indexed_versions`, each with a foreign key to `artifacts`, and `block_grammar_owed`, with their indexes, among them the partial `document_block_versions_owed` and `block_grammar_owed_enqueued` through which the block-index reconciliation reads what is owed.
It creates the trigger function `block_grammar_owe`, which no trigger calls until `0123`, and re-states the `graph_edges` view with two more arms:
a block's mentions and its `part_of` edge to its document.
The five foreign keys take `SHARE ROW EXCLUSIVE` on `artifacts` for the migration's short transaction, so an artifact write waits behind it while a read does not, and re-stating the view holds `graph_edges` `ACCESS EXCLUSIVE`, so a reference read waits too.
Its one insert marks every document owed a block index in `document_block_versions`, a table it creates, and the server's reconciliation indexes them with no command run.
It refuses and rewrites no existing row; its census answers `0`.

Migration `0122_artifact_versions_block_index_trigger` creates `document_block_versions_owe` and an `after insert` trigger on `artifact_versions` that marks a document owed a block index whenever any build inserts a version carrying markdown.
A version a build without the block index writes during the roll is then indexed by the reconciliation, and a block-aware build's own index of the version, later in the same transaction, settles the mark.
The trigger takes `SHARE ROW EXCLUSIVE` on `artifact_versions` alone, so a version write waits behind it while a read does not.
It touches no row; its census answers `0`.

Migration `0123_comments_block_grammar_trigger` attaches `block_grammar_owe` to `comments`, `after insert or update of body`, for a body that holds `#b-`:
a comment any build writes that may cite a block is owed an index with the block grammar, which a build that predates the grammar does not give it.
The trigger takes `SHARE ROW EXCLUSIVE` on `comments` alone, so a comment write waits behind it while a read does not.
It touches no row; its census answers `0`.

Migration `0124_asks_block_grammar_trigger` does the same for `asks`, `after insert or update of
question`, for a question that holds `#b-`.
Its trigger takes `SHARE ROW EXCLUSIVE` on `asks` alone, so an ask write waits behind it while a read does not.
It touches no row; its census answers `0`.

Migration `0125_messages_block_grammar_trigger` does the same for `messages`, `after insert or
update of body`, for an issue message (`issue_key` set) whose body holds `#b-`; a direct message is indexed by nothing, so it owes nothing.
Its trigger takes `SHARE ROW EXCLUSIVE` on `messages` alone, so a message write waits behind it while a read does not.
It touches no row; its census answers `0`.

Migration `0126_block_grammar_seed` owes the same index to every comment, ask and issue message already stored whose text holds `#b-`, read with the three triggers' predicates.
The triggers are live by then, so a write that lands while it runs is owed by its trigger, and a row a trigger has already written keeps its `enqueued_at`.
It takes `ACCESS SHARE` on `comments`, `asks` and `messages`, which no write waits behind, and writes only `block_grammar_owed`.
Its census answers `0`: the one table it writes is `0121`'s, which holds no row at deploy, and the rows it inserts are new work owed, not a rewrite.

Migration `0127_asks_block_id_key` creates `block_id_key(id)`, the SHA-256 of a block id's UTF-8 bytes, written to be immutable so an index can use it, and rebuilds `asks_block_id_unique` (`0018`) and `asks_anchor_block_id_idx` (`0023`) under their names over it.
A B-tree entry holds at most 2704 bytes, and both indexes keyed the id itself, so an ask block or an ask anchored in a block whose incompressible id a browser's edit made longer failed its write, a settlement opening that ask included;
under the 32-byte key every such write is written.
`asks_block_id_unique` still keeps `(block_artifact_id, block_id)` unique under its name, and each index keeps the first column every read of it leads with, so the earlier release writes and reads through them as before.
Each drop holds `asks` `ACCESS EXCLUSIVE` until the migration commits, so every read and write of an ask waits while the two indexes build over its rows.
Each rebuild is a plain `create index`, not `concurrently`, which cannot run inside this unmarked migration's transaction (`internal/dispatch/store/store.go`, `pgmigrate.Exec`).
How long that wait lasts under production-scale data is unmeasured:
this entry claims no duration, since a run on a throwaway Postgres gives a number off other disks, another cache and no live workload, which a deployer must not plan a lock window by.
It refuses and rewrites no row: rows the old unique index held apart differ in their id, so they differ in its SHA-256.
Its census answers `0`.

Migration `0128_comments_anchor_block_id_key` rebuilds `comments_anchor_block_id_idx` (`0023`) over `0127`'s `block_id_key` the same way, so a comment anchored in a block whose id is any length is written.
Its own migration, so its transaction holds `comments` alone:
the drop holds it `ACCESS EXCLUSIVE`, and every read and write of a comment waits while the one index builds.
That rebuild is a plain `create index` too, not `concurrently`, for the same reason: the runner's one transaction cannot run it.
How long the wait lasts under production-scale data is unmeasured here as well, and this entry claims no duration.
It refuses and rewrites no row; its census answers `0`.

Migration `0129_document_edit_notices` creates `document_edit_notices`, one row per document holding a person's pending edit notice:
the versions its quiet window spans, every person who wrote one, the actor the notice's event carries and when the window ends.
A settlement of a person's browser edit opens or moves the row in its own transaction, and the leased edit-notice sweep deletes each due row and appends its `artifact.human_edited` event in the same transaction, so a notice still pending when a process stops is sent by either process's next sweep.
A settlement of a closed issue's document opens no row, and when it writes a version it deletes the document's row, so neither a reopen inside the quiet time nor the first notice after the reopen reports the edits that it saved.
The migration also adds `doc_settlements_pending.last_person`, the latest person sender of the document's pending edits, which names who edited last.
The table is new and the column a plain nullable add, so no row is refused or rewritten and its census answers `0`;
the foreign key takes `artifacts`' lock for the length of the migration, and the alter waits for `doc_settlements_pending` within the five-second lock bound.

Migration `0130_comments_written_version` adds `comments.written_version`, the version of its document current when a comment was written on it without an anchor (the comment's graph node's `parent_version`, [Reference graph](#reference-graph)), and a `before insert` trigger that sets it to the highest `artifact_versions.number` of the document on every comment with `artifact_id` set and `anchor` null whose insert names none, so the earlier release's inserts during the roll record it too.
The column is nullable with no default, a catalog change that rewrites and scans no row;
its own migration, so its transaction holds `comments` `ACCESS EXCLUSIVE` alone, for milliseconds.
It refuses and rewrites no row; its census answers `0`.

Migration `0131_asks_written_version` does the same for `asks`:
its trigger sets an approval ask's `written_version` to `approval.version`, the version the request names when it opens, which stays while the row's `approval.version` moves with later versions, and that of an ask with `artifact_id` set, `anchor` null and `block_artifact_id` null to the highest version of its document.
Its own migration, so its transaction holds `asks` `ACCESS EXCLUSIVE` alone, for milliseconds.
It refuses and rewrites no row; its census answers `0`.

Migration `0132_component_facts` creates `component_facts`, when each component was made and last changed and the digest of its own fields at its last recorded change, keyed `(project_key, id)` as `components` is, which every import keeps from then on ("Reference graph" below).
Its foreign key to `projects` cascades, and it has none to `components`, whose rows every import deletes and inserts again.
Its one insert seeds a row with all three null for each component already stored: when those were made or last changed was never recorded.
It reads `components` under `ACCESS SHARE`, and the foreign key takes `SHARE ROW EXCLUSIVE` on `projects` for the catalog change.
It refuses and rewrites no existing row; its census answers `0`.

Migration `0133_comments_written_version_backfill` fills `written_version` on the comments with `artifact_id` set and `anchor` null stored before `0130`'s trigger:
each takes the highest version of its document created at or before it, the only record those rows have, so a comment written while a version's transaction was still open can name that version.
A comment the trigger recorded keeps its value, one on a document with no version that old keeps null, and no update trigger fires.
It takes `comments` `EXCLUSIVE` before it writes a row, as `0089` takes `issues`, so no write that locks two of its rows in another order deadlocks with it:
every read goes on, and every comment write waits until it commits.
Its census answers `0`: it fills a column only this release reads and changes no value any reader had, and it cannot name the column, which does not exist when the census is taken.

Migration `0134_asks_written_version_backfill` does the same for `asks`, holding `asks` `EXCLUSIVE`:
an approval ask takes `approval.version` of its `ask.opened` event (the type list of `events_ask_payload_id`, `0030`, lets the lookup read that index), the version the request named when it opened, and keeps null without one;
another ask with `artifact_id` set, `anchor` null and `block_artifact_id` null takes the highest version of its document created at or before it, as `0133` reads a comment's.
Its census answers `0` for the same reasons.

Migration `0135_server_processes` creates two tables:
`server_processes`, the registry of live server processes, and `server_build_announcements`, each build the fleet has announced as `server.started` (Running more than one process, above).
It reads, locks and rewrites nothing that exists, and the release before it neither reads nor writes either table, so that release runs over the new schema unchanged and the census answers `0`.
Migration `20261115051200_server_build_announcements_tuple_kinds` later adds `tuple_kinds` to `server_build_announcements`, a nullable `text[]` with no default: the relation kinds the announced build writes as tuples, from which the release that deletes an old arm of `graph_edges` reads whether the release a rollback would reach answers that arm's kinds from the store too.
A row a build of an earlier release inserts names only `commit` and lists no kinds, which is true of it; the column rewrites no row, the release before it never reads it, and its census answers `0`.

Migration `0136_dashboard_assets` creates `dashboard_builds`, `dashboard_assets` and `dashboard_build_assets`, where every server process publishes its dashboard build's assets at boot and from which it answers a page of another build that asks it for that build's chunks (`internal/dispatch/dashboard`).
It creates tables and touches no row; its census answers `0`.
The release before it never reads or writes these tables, so it serves beside `0136` unaffected while both run
— its own pages are served from its own image, as before
— and rolling Dispatch back past `0136` is an image rollback alone, with no schema to restore:
the tables are left for the next release that carries them, whose boot prunes what no build still needs.

Migration `0137_request_keys` creates `request_keys`, the one record of every keyed write (Keyed writes, below), and its two indexes:
`request_keys_created_at`, for the sweep that deletes a key past its 24 hours, and the partial `request_keys_owed_sends` (`created_at`, over rows with no answer yet whose recovery point names a message), for the delivery sweep's read of a keyed send whose process died before its first attempt.
It is a new table that touches no row, which the release before it neither reads nor writes; its census answers `0`.
It follows `0129_document_edit_notices`, `0130_comments_written_version`, `0131_asks_written_version`, `0132_component_facts`, `0133_comments_written_version_backfill`, `0134_asks_written_version_backfill`, `0135_server_processes` and `0136_dashboard_assets`, which already use the preceding migration numbers.
From its release on, nothing reads or writes `broadcast_idempotency_keys` (`0055`), the broadcast's own key table before it;
it was kept through that release so a deploy of it could roll back, and `0141_drop_broadcast_idempotency_keys` drops it in a later release.

Migration `0138_pending_delivery_indexes` adds two partial indexes, `message_deliveries_pending` and `comment_deliveries_pending`, on the delivery attempts still pending, which the delivery sweep reads every minute (Keyed writes, below).
It reads, refuses and rewrites no row, and the release before it neither reads nor depends on them; its census answers `0`.
Each build holds its table's writes (a `SHARE` lock) for as long as it reads the table once.

Migration `0139_related_checks` adds three tables for the related-item check, which this release makes search by meaning first and by the project's rarest words.
`related_checks` holds one row per check of an issue, an ask or a decision block a document edit adds, written by the check itself once it has answered (a check that ran out of time, or whose request ended first, included;
the no-write related route records nothing):
its project and source, the actor, the outcome (`found`, `none` or `not_made`), how it searched (`meaning_and_keywords` or `keywords_only`, none when not made), the reason it was not made (`timeout` for the check's own allowance running out, apart from `cancelled` for the request that ran it ending first:
its client leaving for a write sent without an `Idempotency-Key`, the keyed run's own 10-minute bound for a keyed one) or searched by keywords alone, the lexemes it searched for, its keyword, meaning and total milliseconds and how many items it listed.
`search_term_stats` (how many of a project's searchable items hold each lexeme) and `search_term_totals` (how many searchable items the project has, and when they were counted) are the statistics the check picks its keywords from;
the term-statistics refresh, added with it, rebuilt a project's rows whole at boot and every 10 minutes, where `20261101182508`-`20261101182522` have it apply what changed, and the check only reads them.
Every row names its project with `on
delete cascade`, and a check's row also cascades from its issue, document or ask.
The tables are new, so the migration rewrites and refuses no row and its census answers `0`;
its foreign keys take the referenced tables' locks for milliseconds, as `0070`'s did.

Migration `0140_issue_embedding_spec` makes an issue's meaning-search text its title and its spec:
`issue_embedding_text(title, spec)` is the title, a blank line and the markdown of the latest version of the issue's primary document, or the title alone when that markdown is empty or only white space.
It replaces the function of `0073`'s issues trigger, which still fires on an insert or a title change, and adds a trigger on `artifact_versions` that queues the issue again when a new version of its primary document is written.
Replacing a function's body locks no table, so the migration takes `artifact_versions`' lock alone, for its trigger, as `0073`–`0077` each take one table's.
The coverage pass (Search, below) builds the same text, so an existing issue, whose row holds its title alone, is queued again by the first pass once an embedder runs and re-embedded at its title and spec;
nothing is re-embedded by the migration itself, and its census answers `0`.
While the release before it still serves, its coverage pass points those rows back at the title alone, and each pass of either release queues them again, so a roll that spans passes embeds some issues more than once;
a deployment with no embedder runs no pass and sees none of this.

Migration `0141_drop_broadcast_idempotency_keys` drops `broadcast_idempotency_keys`, the contracting half of `0137`:
a deploy that carries it can no longer roll back to a build from before `0137`, whose broadcast send writes that table.
It reads no row; dropping the table takes `ACCESS
EXCLUSIVE` on `broadcasts` too, because the table's foreign key puts triggers there, so a broadcast read or send waits for the instant the migration's transaction lasts.
Its census answers `0`.
It follows `0139_related_checks` and `0140_issue_embedding_spec`, which already use the preceding migration numbers.
Migrations `0145_events_person_words` and `0146_comment_deliveries_session` add two partial indexes for `GET /api/v1/agents/{session_id}/answers` (`sessionanswers.List`, Routes below), which every agent host reads on every model request.
`0145` created `events_person_words` on `events (created_at desc, id desc)` where the actor is a person and the type is `ask.answered`, `comment.created`, `message.created` or `message.answered`, and `20261111002445_drop_events_person_words` drops it:
the route reads a fifth type now and takes that window through `events_person_words_with_declines` (`20261111002444`, below) instead.
Through whichever index the release holds, the route reads the window's human events of those types, newest first, and keeps those whose `published_destinations` hold the session's own topic.
A GIN index on `published_destinations` would find the session's events directly, but the outbox appends each topic it published an event to that array, and every such append would then also write the new element's entry into the index:
a write load every outbox append would pay for a read only this route makes.
`0146` creates `comment_deliveries_session` on `comment_deliveries (session_id, created_at desc)` where `session_id` is not null:
the route reads the session's mentions created since an hour before its window as one range of it.
Each of these two was a plain `create index` inside the runner's transaction, so it held `SHARE` on its table while it built: writes to it wait, reads do not.
On a local Postgres 17 (two CPUs, `maintenance_work_mem` 64 MB) with `events` seeded to 1,000,000 rows (421 MB with its indexes, 126,541 of them in the index), `0145` built in 0.66-1.13 s over three runs and `0146` in 0.11-0.14 s on 50,003 deliveries.
Both censuses answer `0`: an index build refuses and rewrites no row, and the census report already counts and sizes each table it reads.

Migration `20261111002444_events_person_words_with_declines` creates `events_person_words_with_declines` on `events (created_at desc, id desc)` where the actor is a person and the type is `ask.answered`, `comment.created`, `message.created`, `message.answered` or `ask.resolved`, and `20261111002445_drop_events_person_words` drops `0145`'s index behind it.
The answers route lists a decline now — an ask a person chose not to answer — which it reads from the `ask.resolved` events whose `published_destinations` hold the session's own topic, so its read names five types and the four-type index cannot serve it at all.
The build runs `concurrently`, outside the runner's transaction (`-- pgmigrate:non-transactional`):
`events` is the largest table a deployment holds and grows with every write Dispatch makes, so the `SHARE` a plain build holds — which blocks every write to the table for as long as one read of it takes — grows with the table.
The drop lands in the same release because a read of four types implies the five-type predicate:
the release before this one reads `ask.answered`, `comment.created`, `message.created` and `message.answered`, a subset of the values the wider index is partial on, and Postgres proves that implication, so the wider index serves that build's read for as long as it keeps serving requests (`api.TestTheAnswersListHoldsADeclineForEachSessionItReached` explains both reads over a seeded `events` table and holds each to an index scan of it).
`drop index` takes `ACCESS EXCLUSIVE` on `events` for the instant it unlinks the index, inside the runner's transaction, so every read and write of an event waits behind it for that moment, and it reads no row of the table.
Both censuses answer `0`: an index build and an index drop each refuse and rewrite no row.
A failed concurrent build leaves an INVALID index of the new name on `events`, which the runner drops with `DROP INDEX CONCURRENTLY` and builds again on the next boot, so one failed build stops no later boot.

Migration `0147_message_deliveries_session_created` created `message_deliveries_session_created` on `message_deliveries (session_id, created_at desc) include (message_id)` where `state` is `sent`, for the same route's read of the session's sent message deliveries created since an hour before its window, and `20261114081436_drop_message_deliveries_session_created` drops it:
the route reads queued deliveries too now and takes that window through `message_deliveries_session_taken` (`20261114081435`, below) instead.
`message_deliveries_session` (`0051`) keys `session_id` alone, so through it that read fetched the session's whole delivery history and filtered it by time:
a long-lived session that receives every broadcast copy read all of its deliveries on every model request.
`0147`'s index held only the read's sent rows and carried the one column it returned, so the read was an index-only scan of the window's range, and a cached generic plan, which knows neither the session nor the window, chose it too:
costed as a plain `(session_id, created_at desc)` index it came within a percent of `0051`, which the generic plan kept.
`0051` stays for the conversation window, which reads every delivery a session ever received.
On the seeded database above, with one session given 60,003 sent deliveries (1,019 since an hour before its window), the read of them went from 58,984 rows fetched and discarded (798 buffers, 20-22 ms) to an index-only scan of the 1,019 (11 buffers, 0.3-0.4 ms), in the custom and the generic plan alike;
the index built in 0.34-0.52 s over three runs on 110,003 deliveries.
A plain `create index` inside the runner's transaction, it held `SHARE` on `message_deliveries` while it built: writes to it waited, reads did not.
Its census answers `0`.

Migration `20261114081434_delivery_queued_state` adds the fourth delivery attempt state, `queued`: the Envoy listener took the message and stored it for a session it knows and that is not running, which reads it when it runs again.
It drops `comment_deliveries_state_check` (`0039`) and `message_deliveries_state_check` (`0046`) without `cascade` and adds each back `not valid` with `queued` beside `pending`, `sent` and `failed`, so no stored row is read:
no row can fail a check wider than the one it was written under.
It adds `message_deliveries.connected_only`, a nullable `text` column with no default whose one other value is `connected_only` (its own check, also `not valid`), set on every attempt of a message whose request asked the listener for a session that is running now:
attempt 1 records the request's `connected_only`, and every later attempt of the message inherits attempt 1's value.
No row records the switch rule: every send of a person's own Send or Aside, not one an agent wrote for them, on no issue, no broadcast's copy, whose body is one `/model <value>` or `/effort <value>` line asks it whatever its row holds (`isSessionSwitch`, `deliverMessageResuming`), so a switch an earlier release stored with no flag is held to a running session on its Retry, Use Aside instead or resume too, and every other message, a BTW and an agent's message for its person among them, is queued.
The release before this one writes only `pending`, `sent` and `failed` and never names the column, so the two builds run against the widened schema at once.
Each statement is one of the census's catalog-only `alter table` forms, so the census exempts the migration from its table size limit at any size, and each takes `ACCESS EXCLUSIVE` on its table for its catalog write alone.
Its census answers `0`: it refuses and rewrites no row.
Rolling back past it is an image rollback alone while no row carries `queued`; once one does, the checks have to be widened again by hand before an earlier set is restored.

Migration `20261114081435_message_deliveries_session_taken` creates `message_deliveries_session_taken` on `message_deliveries (session_id, created_at desc) include (message_id, state)` where `state` is `sent` or `queued`, and `20261114081436_drop_message_deliveries_session_created` drops `0147`'s index behind it.
The answers route reads a session's queued message deliveries beside its sent ones, so its read names two states and `0147`'s `sent`-only index cannot serve it at all.
The index carries `state` beside `message_id` because Postgres drops an index predicate's clause from a scan only where the query's clause and the predicate imply each other, so over two states it keeps the read's own `state in ('sent', 'queued')` as a filter, which an uncarried column would turn into a heap read of every row the scan found.
The build runs `concurrently`, outside the runner's transaction (`-- pgmigrate:non-transactional`):
`message_deliveries` grows with every message and broadcast copy Dispatch sends, and a plain build holds `SHARE` on it, which blocks every write, for as long as one read of the table takes.
The drop lands in the same release because a read of `sent` alone implies the two-state predicate:
the release before this one reads `state = 'sent'`, and the wider index carries every column that read returns, so the planner reads it index-only through the wider index for as long as that build keeps serving requests.
`drop index` takes `ACCESS EXCLUSIVE` on `message_deliveries` for the instant it unlinks the index, inside the runner's transaction, and it reads no row of the table.
Both censuses answer `0`: an index build and an index drop each refuse and rewrite no row.
A failed concurrent build leaves an INVALID index of the new name on `message_deliveries`, which the runner drops with `DROP INDEX CONCURRENTLY` and builds again on the next boot, so one failed build stops no later boot.

Migration `0142_item_blocks` creates the tables behind the blocks of a comment or message as graph nodes ("Reference graph" below):
`comment_blocks`, `message_blocks`, `comment_block_texts`, `comment_block_state`, `message_block_state`, `comment_block_refs` and `message_block_refs`, each with a foreign key to `comments` or `messages` that cascades, with their indexes, among them the partial `comment_block_state_owed` and `message_block_state_owed` through which the block-index reconciliation reads what is owed.
It adds the nullable `replies_to_block` to `comments` and to `messages`, a catalog change that rewrites and scans no row, with a partial index on each.
It creates the trigger function `item_blocks_owe`, which no trigger calls until `0143`, and re-states the `graph_edges` view with six more arms:
a current block's `part_of` edge to its comment or message, the links a current block holds, and a reply's `replies_to` edge to the block it names, under the storage kinds `comment_block` and `message_block`, which a build without them skips as unknown kinds.
Adding the columns holds `comments` and `messages` `ACCESS EXCLUSIVE`, and re-stating the view holds `graph_edges` `ACCESS EXCLUSIVE`, for the migration's short transaction, so every read and write of a comment or message, and every reference read, waits behind it.
It writes no row: the state rows are seeded by `0143` and `0144`, after their triggers.
It refuses and rewrites no existing row; its census answers `0`.

Migration `0143_comments_item_blocks_trigger` attaches `item_blocks_owe` to `comments`, `after
insert or update of body`: a comment any build writes is owed a block index (`comment_block_state.owed_at`), which a block-aware build clears by indexing the comment in the same transaction, so a comment a build without the index writes during the roll, or after a rollback, stays owed until the reconciliation indexes it.
In the same transaction it then owes an index to every comment already stored, reading `comments` once.
The trigger takes `SHARE ROW EXCLUSIVE` on `comments` alone until the commit, so a comment write waits behind it, while a read does not, and no comment is written between the trigger and the seed.
Its census answers `0`: the one table it writes is `0142`'s, which holds no row at deploy, and the rows it inserts are new work owed, not a rewrite.

Migration `0144_messages_item_blocks_trigger` does the same for `messages`, `after insert`, a direct message included (no release of that build edited a message), and owes an index to every message already stored, reading `messages` once.
Its trigger takes `SHARE ROW EXCLUSIVE` on `messages` alone, so a message write waits behind it while a read does not.
Its census answers `0` for the same reasons.
Migration `20261110224508_post_corrections` replaces that trigger with `after insert or update of body`, the comment twin of `0143`'s, so an edited message is owed a block index as an edited comment is.

Migration `0148_asks_anchor_artifact_id` creates `asks_anchor_artifact_id`, an index over the asks whose `anchor` is set of `case when anchor->>'artifact_id' ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$' then (anchor->>'artifact_id')::uuid end`:
the uuid cast `comments_anchor_artifact` (`0032`) indexes, guarded by the form every server-written anchor carries, since an unguarded cast would fail the boot over an anchor an earlier release could store.
Settlement's sweep reads through it the marks a document's asks record, by the anchor's document alone, so an ask that also names an ask block on another document (`block_artifact_id`) keeps its mark on the document its anchor names.
Building it takes `SHARE` on `asks`, so ask writes wait for the build; it rewrites no row and refuses none, so its census answers `0`.
An anchor whose artifact id is not a lower-case document id is left out of the index, which costs the read nothing
— its text comparison never matched such a spelling either — and each boot logs the count of those asks with a warning when there are any.
The release before it never reads the index and serves beside it unchanged, except that its sweep still drops the mark of an ask holding both columns (no such row is written today), and rolling Dispatch back past `0148` is an image rollback alone:
the index is left for the next release that reads it.

Migration `0150_dpop_proofs` creates `dpop_proofs`, the record of every RFC 9449 DPoP proof Dispatch has accepted, and its expiry index.
A caller presenting an access token the secrets broker issued signs a fresh proof of that token's key for each request (`internal/ciphertoken`), and the row keyed on the proof's `jti` and normalized `htu` is what refuses the same proof presented twice;
the maintenance pass deletes every record past its proof's acceptance window beside the idempotency-key sweep.
It is a new table that touches no row, which the release before it neither reads nor writes; its census answers `0`.

Migration `20261028195714_doc_compaction_owed` creates `doc_compaction_owed`, the documents with rows a compaction could fold, which the hourly compaction sweep reads in place of a scan of `doc_updates`:
every append names its document there in the statement that inserts its row once that row is not the document's first, and a compaction that leaves no row above its fold point, a rebuild, a delete and the sweep (for a document with no row after its first) remove it, each under the document's lock.
It has no foreign key, so no append that names a document locks `artifacts`.
Its one insert names every document holding more than one row;
it reads `doc_updates` under `ACCESS SHARE` and writes only the table it creates, so `doc_updates`, which may be past the census's 1 GiB limit, is not a table it touches.
The release before it neither reads nor writes the table:
a document only that release appends to during the roll is compacted by the new release's queue on its next append or load.
It refuses and rewrites no existing row; its census answers `0`.

Migration `20261101143201_duty_holds` creates `duty_holds (name, holder, token, expires_at)`, one row per name a process holds by renewing it (`duty.Hold`):
the events outbox's publisher holds `dispatch-outbox` there, and each delivery intake holds one name per durable (Running more than one process, above).
`expires_at` is always the database's `now()` plus the hold's TTL, so a lapse is judged on the database's clock and never on two hosts' wall clocks.
`token` is free text the holder records: the outbox's host and process id, and an intake's deliver subject, or `<subject> start=<seq>` while a take-over is pending, from just before the take-over deletes the durable until it has created it again at stream sequence `<seq>` (`delivery/held_durable.go`, `intakeHoldToken`).
A release deletes the row and notifies `dispatch_duty_holds` with the name in one statement, which wakes a standby at once.
It creates a table and touches no row, and the release before it neither reads nor writes it; its census answers `0`.
Its identifier is the UTC moment it was created, as every migration added since the numbered series closed is (Database migrations, above).

Migration `20261118020802_github_read_attempts` creates `github_read_attempts`, the newest attempt of each GitHub read Dispatch makes under a lease, which `/healthz` answers as `github` (Routes below).
The key is `read`, and a check holds it to the four names `webhook_redelivery`, `architecture_import`, `delivery_reconcile` and `ci_reconcile`.
`at` is when the attempt was recorded, by the database's clock, `succeeded` whether it sent requests to GitHub and noted no failure, and `cause` the text of its first failure, at most 1,024 bytes;
two checks hold `cause` to that length and null exactly when `succeeded` is true.
The loop that holds a read's lease writes that read's row over the one before it when its pass ends (`store.GitHubReadAttempts.Record`), and a row trigger notifies `dispatch_github_read_attempts` with the read's name at each write, which refreshes every process's copy of the record (Background loops, above).
A new table: nothing existing is read, locked or rewritten, the previous release neither reads nor writes it, and its census answers `0`.

Migration `20261120222732_block_refs_to_source` creates `block_refs_to_source` on `block_refs (to_kind, to_id, ((artifact_id)::text))`, and `20261120222733_drop_block_refs_to` drops `0121`'s `block_refs_to` on `(to_kind, to_id)` behind it.
`heldByABlock`'s probe of `block_refs` (`refs/graph.go`) compares all three columns, and through `block_refs_to` it read every block link into the node once for each document that mentions it and filtered the source document: 999,000 rows removed for an issue that 1,000 documents mention in their text and in two blocks each.
The new index's first two columns are the old one's, so every read `block_refs_to` served it serves, and `block_refs` keeps one index of targets, with the key the target indexes of `comment_block_refs`, `message_block_refs` and `ask_block_refs` already have.
The build is a plain `create index` in the runner's transaction, not `concurrently`: production runs one app pod under `Recreate`, which migrates before it listens, so no writer runs while the build holds `SHARE` on `block_refs` for one scan of the table.
The census admits it only while `block_refs` is under `CensusTableLimit` and refuses it above, and then the build must be rewritten in the concurrent form (`-- pgmigrate:non-transactional`, `create index concurrently if not exists`, alone in its file) before the deployment can move.
The drop sets `lock_timeout` to `120s` for its own transaction, because it takes `ACCESS EXCLUSIVE` on `block_refs` for the instant it unlinks the index and waits for any read the stopped pod left running, which can outlast the runner's three attempts of 5 s.
That bound holds on each try, and `store.Migrate` tries a migration up to 3 times after a lock timeout (`pgmigrate.RetryLockTimeouts`), so the drop waits up to 120 s on each of those 3 tries.
Where two builds serve at once, the build holds off the earlier build's writes to `block_refs` for its scan, and the drop its reads and writes for as long as it waits, up to 120 s on each of `store.Migrate`'s 3 tries.
Both censuses answer `0`: an index build and an index drop each refuse and rewrite no row.
The release before them reads `block_refs` by `to_kind` and `to_id` alone, which the new index serves, so it runs unchanged beside them.

Migration `20261101030537_server_processes_schema` adds four columns to `server_processes`:
`schema_version` and `schema_creatable`, the document schema numbers each process reads and creates, which the document socket's admission reads (Two builds at once, above);
`embedder_identity`, the identity of the process's embedder; and `file_store`, whether it stores files in a bucket.
Their defaults are constants (`0`, `0`, `''` and null), so Postgres rewrites no row, and a row at the defaults is a process of the release before it, which names its columns in every statement on the table and so neither reads nor writes these.
Nothing else is read, locked or rewritten, and its census answers `0`.

Migration `20261101030538_embedder_active` creates `embedder_active (one, identity, since)`, at most one row (`one` is the primary key and can only be true):
the embedder identity whose processes embed and drop vectors (Search, below).
It is a new table: nothing existing is read, locked or rewritten, the release before it neither reads nor writes it, and its census answers `0`.

Migration `20261101143203_outbox_claim` gives the outbox's batch read three partial indexes and drops the two the per-row claim read.
`events_outbox_fresh` on `(id)` holds the unpublished events never tried, `events_outbox_retry` on `(next_attempt_at, id)` those waiting on a back-off, and `events_outbox_owner` on `(coalesce(issue_key, artifact_id::text, project_key), id)` each owner's unpublished events;
none holds a published event or a pending subscription command, so the read walks past neither.
It adds `events.publish_error`, nullable text with no default, a catalog change that rewrites no row:
why an event's last attempt failed, cleared once it is published.
It drops `events_unpublished` (`0003`) and `events_unpublished_ready` (`0020`), which only the per-row claim read;
the release before it reads neither the new column nor the new indexes, and its claim still runs without the dropped ones, at a cost.
That claim's predicate, `coalesce(payload ->> 'pending', 'false') <> 'true'`, proves none of the three new indexes' `(payload ->> 'pending') is distinct from 'true'`, so the earlier release reads the whole of `events` in a sequential scan on every scan it makes:
on 1,000,000 event rows, the last 100 unpublished, on Postgres 17, 125 ms and every one of the table's 19,231 pages, where `events_unpublished` took 3.4 ms.
It pays that while it serves beside this release in a rolling update, which is one more reason to deploy this release with `strategy: Recreate` (below), and for as long as a rollback to it lasts.
The migration runs in one transaction, so every lock it takes is held until it commits.
It builds the three indexes first: each reads every row of `events` under `SHARE`, which holds writes to `events` from then until the commit and lets reads go on (no `concurrently`, which a migration's transaction cannot run):
1.1 s on 1,000,000 event rows and 7.2 s on 5,000,000, on Postgres 17 with two cores.
It then adds the column and drops the two old indexes, each under `ACCESS EXCLUSIVE`, which holds reads of `events` too, so reads wait only for those last statements, which rewrite no row, and for the commit.
Its census answers `0`, since it refuses and rewrites no row, and the census reports `events`, the table it locks, with its row count and size.

From `20261101143203`'s release on, `next_attempt_at` is only a back-off, and an unpublished event holds back its owner's later events from each destination it has not reached.
The rows that start doing so at the deploy are the unpublished events that are not pending commands;
count them by attempt before the deploy with `select attempt_count, count(*) from events where published_at is null and
(payload ->> 'pending') is distinct from 'true' group by attempt_count order by attempt_count`.
The release before it takes no publisher hold and still claims and publishes batches of its own, and this release reads those one-minute claims as back-offs alone, so while a rolling update runs both the two publish side by side:
an issue's events can arrive out of order, as they could between two processes of the earlier release, and one event can be sent by both, its second copy dropped by JetStream's MsgId or by its receiver's dedupe.
A deployment that wants neither, nor the earlier release's whole-table claim scans, replaces the running server rather than rolling it for this one release (`strategy: Recreate`).

Migration `20261101180039_comment_fallback_copies` creates `comment_fallback_copies`, the copies of a new comment the outbox held back for a mention it waits on no longer - one the delivery sweep resends, one a sender still holds queued behind a lane held past a claim lease, and one a Retry overtook while its sender holds it - one row per comment and destination topic, which the outbox's fallback pass publishes once that send fails and settles with nothing published once it lands, or once the author has deleted the comment (`publishFallbackCopies` and `fallbackCopyWithdrawn` in `internal/dispatch/outbox/mentions.go`), and its partial index of the unsettled rows.
It names no other table, so a plain `create table` and `create index` in the runner's transaction lock only the new table, and the release before it neither reads nor writes it, so that release runs over the new schema unchanged and the census answers `0`.
That release does not publish a `comment_fallback_copies` row either:
it has no fallback pass, and its outbox skips an event already published, so a copy a row holds is never sent once it serves alone.
Before rolling back past `20261101180039`, run `select count(*) from comment_fallback_copies where settled_at is null` and roll back only when it answers `0`, or let this build drain the rows first.

Migration `20261101144855_block_pointer_links` gives `block_refs` a nullable `to_types text[]`, the types a typed block's `block[]` pointer may point at (null:
any), creates the function `block_ref_listed(to_types, to_id)`, true when `to_types` is null or no current `document_blocks` row of the target has a type outside it, and re-states `graph_edges` with `0142`'s arms, its `block_refs` arm listing a row only while `block_ref_listed` holds ("Reference graph" below states the typed block rules).
The column is added with no default, so no row is rewritten, and the previous release, which neither reads nor writes it, keeps writing rows that hold null and so are listed as before.
The column takes `block_refs` `ACCESS EXCLUSIVE` and the view `graph_edges` `ACCESS EXCLUSIVE` for the migration's short transaction, so a block index write and a reference read wait behind it.
It refuses and rewrites no row; its census answers `0`.
The shipped `plan_task` and `check` declare the `block[]` attributes `serves` and `needs`, so a document written since they shipped holds pointer rows, which a build that does not read `to_types` lists as it lists every other `block_refs` row.

Migration `20261101182419_ask_followers_session` creates `ask_followers_session_id` on `ask_followers (session_id)`, for the `followed_by` arm of `graph_edges`, which `refs.BacklinkCounts` now reads directly rather than leaving the planner the whole view.
The arm reads `ask_followers` by `session_id` and had no index for it:
the table's primary key leads with `ask_id`, which cannot serve that predicate, and its only other index, `ask_followers_ask_id_text` (`0032`), is on the ask as well, so every count of a session's backlinks scanned the table.
A plain `create index` inside the runner's transaction, it holds `SHARE` on `ask_followers` while it builds:
the build waits out a write of a follower already open and holds off a new one for that moment, while reads are not held, and the table is small.
Its census answers `0`: one index on an existing column, refusing no row and rewriting none.

Migration `20261101182426_asks_issue_key` creates `asks_issue_key` on `asks (issue_key)`, for the read of every ask of one issue that `GET /api/v1/issues/{key}/asks` makes with no state filter (`api/asks.go` `listIssueAsksQueryAll`) and for the reference closure's read of an issue's asks.
No index served either: `asks_open` (`0001`) is partial on `state = 'open'`, which a read asking for every state cannot use, and every other index of `asks` is keyed on something else, so both scanned the table.
The read orders by `created_at, id` within the issue, so `(issue_key, created_at, id)` would be strictly better
— the index would answer the order too and leave the plan no sort
— and this migration keeps the plain key the spec names, because the sort is over one issue's asks and this release does not change the statement.
A plain `create index` inside the runner's transaction, it holds `SHARE` on `asks` while it builds: ask writes wait, reads do not.
Its census answers `0`: one index on an existing column, refusing no row and rewriting none.

Migration `20261101182433_messages_issue_roots` creates `messages_issue_roots` on `messages (issue_key, created_at, id)`, for the read of an issue's root messages that `GET /api/v1/issues/{key}/messages` makes, `where issue_key = $1 and in_reply_to is null order by created_at, id` (`api/messages.go` `listIssueRootMessagesQuery`).
No index of `messages` led with `issue_key` — `messages_in_reply_to` (`0033`) leads with the parent and `messages_search` (`0061`) is the text index
— so every read of an issue's threads scanned the table, 15.8 s of database time in the 24 hours to 2026-10-30 22:00Z.
The key is the read's predicate and then its order, so the plan takes one range of the index and sorts nothing.
It is deliberately not partial on `in_reply_to is null`:
the reference closure reads an issue's messages whatever they reply to, and one whole index serves both reads.
A plain `create index` inside the runner's transaction, it holds `SHARE` on `messages` while it builds:
message writes wait, reads do not, and the census of 2026-10-30 gives `messages` 5,810 rows and 20 MiB.
Its census answers `0`.

Migration `20261101182440_drop_asks_open_artifact` drops `asks_open_artifact`, `asks (artifact_id) where state = 'open' and artifact_id is not null` (`0009`).
It was built for the one read that lists a document's open asks (`api/asks.go` `listArtifactAsksQueryOpen`), and that read uses `asks_artifact_id` (`0032`) instead, which a plan test holds it to;
the index audit of 2026-10-25 found `asks_open_artifact` unused from 2026-10-16 to 2026-10-25.
An index nothing reads is write cost on every ask that opens, closes or changes its document, and one more plan for the planner to cost on every read of `asks`.
The drop is a contracting change, and the expand-then-contract rule does not hold it to the next release:
an index is not a shape the running code reads, so the release before this one and the release after it both keep working over this schema, with one plan choice fewer.
`drop index` takes `ACCESS EXCLUSIVE` on `asks` for the instant it unlinks the index, inside the runner's transaction, so every read and write of an ask waits behind it for that moment.
Its census answers `0`: the drop reads no row, refuses none and rewrites none, and the census report still names `asks`, the table behind the index, so a deployment sees what it locks.
No operator follow-up: nothing is left to rebuild, and a rollback past it is an image rollback alone, since no release reads the index.

Migration `20261101182447_document_search` creates `document_search`, one row for each document holding its latest version's `search` vector (`artifact_id`, `number`, `created_at`, `search`), and `document_search_vector`, the GIN index over that vector.
Search's document arm, the related-item check's and the related-terms probe's each read every document's latest version through a lateral join and tested `v.search @@ q.tsq` on it before this release, because `artifact_versions.search` has no index and can have none:
`0010` left it out, since an index over every version would also find a word only an old version holds, and `artifact_versions` is 5.2 GiB, above the size the pre-deploy census lets a migration touch.
One row per document is an index over exactly the versions search reads.
The table declares no foreign key to `artifacts` or to `artifact_versions`:
a `references` makes the census size the table it points at, and when this migration was written the census did not admit a `create table`, so a key to `artifact_versions` would have refused every deploy carrying this migration.
Today the census still sizes the table a `references` points at, and it holds that table to the limit unless the migration is exempt ("Pre-deploy census" below), which needs the census to admit every statement of the migration.
Nothing is lost by leaving it out, because every reader joins `artifacts` on `a.kind = 'doc'`, so a row whose document no longer exists cannot surface;
there is no delete path and no trigger for one, since no production code path deletes an artifact or a version.
Both statements lock only the table this same migration creates, which no other session can reach before the commit, so the transaction holds nothing of `artifacts` or `artifact_versions` and takes milliseconds.
Its census answers `0`: the table is new and holds no row, and the migration reads no other table.
No operator follow-up — `20261101182454` keeps the table in step and the boot's fill (`20261101182501` below) covers the documents already stored, both in the same boot.

Migration `20261101182454_artifact_versions_document_search_trigger` creates `artifact_versions_document_search`, the function and the `after insert or update of markdown, search` trigger on `artifact_versions` that writes each version into `20261101182447`'s table under `on conflict (artifact_id) do update … where excluded.number >= document_search.number`, so a late write of an older version and an edit of an old version both leave the latest version's vector in place.
It fires after `0058`'s BEFORE trigger has filled `search`, and `when (new.markdown is not null)` keeps a version with no markdown
— an image or a file upload, whose vector is null — from blanking the document's row, exactly as `0122`'s trigger guards itself.
`update of search` is in the column list so that a writer setting the vector directly cannot leave the table behind.
A function and a trigger are the migration's only two statements, which is what exempts it from the census's table-size limit (`internal/pgmigrate/exempt.go`):
neither reads a row, so the time they hold their lock does not grow with the table.
Its census answers `0`: a trigger runs only on later writes and rewrites no row already stored.

The risk `20261101182454` carries is its lock.
`create trigger` takes `SHARE ROW EXCLUSIVE` on `artifact_versions`, the 5.2 GiB table every document save writes and every search page still reads for its snippet:
a read is not held, since `ACCESS SHARE` does not conflict with it, but every version write waits for the commit, and the migration waits for every version write already in flight.
Every migration runs with `lock_timeout` at `pgmigrate.LockTimeout`, five seconds, and the runner applies one whose lock wait outlasts it `pgmigrate.LockTimeoutAttempts` times, three, pausing two seconds between attempts and holding nothing meanwhile.
A holder that outlives all three fails the migration and the boot with it:
that pod never serves, the release before it keeps serving every request, and the deployment reports the new pod crash-looping.
A retry is safe — the failed attempt's transaction rolled back, so nothing of it is in the database and no version record was written, and the next boot applies the migration again from the same state.

Migration `20261101182501_document_search_backfill` creates `document_search_backfill`, the one row that records how far the fill of `20261101182447`'s table has got:
the keyset cursor (`artifact_id`, the highest document the pass has covered, the all-zero uuid before its first batch) and `done`.
It writes no `document_search` row and reads no large table, so it locks only the table it creates and commits in milliseconds.
Its census answers `0`: a new table, whose one row the migration inserts.

The fill itself is `store.FillDocumentSearch`, which the boot runs immediately after the migrations and before the server serves (`cmd/dispatch/main.go`), not a migration.
A migration is one transaction, and the single statement this used to be
— `insert … select distinct on (artifact_id) … from artifact_versions where markdown is not null order by artifact_id, number desc` — measured 35.5 s on a production-shaped database (`artifact_versions` 3337 MB, 200,000 versions, 40,000 documents), with every row it wrote uncommitted for those 35.5 s.
`20261101182454`'s trigger is live by then, so a version write to a document the statement had already written ran its own `insert … on conflict (artifact_id) do update` on that key and waited for the whole statement;
that writer holds its issue's owner lock (`store.LockOwner`) and its document's room for the whole wait, every other write on that issue queues behind it, and an API writer sets no `lock_timeout`.
Each batch is instead one short transaction that takes the next `store.DocumentSearchFillBatch` documents by keyset above the cursor, writes their latest versions under the same write guard as `20261101182454`'s trigger (`on conflict (artifact_id) do update … where excluded.number >= document_search.number`, so a row the trigger wrote while the pass runs is never replaced by an older one), moves the cursor and commits.
The batch is 1,000 documents, about 0.9 ms a document on the shape above, so a batch is about a second and the pass about forty of them, and a writer that meets a key the pass holds waits for one batch rather than for the whole fill.
The cursor is taken from the ids the batch read and never from the insert's `returning`, because a row the guard skips returns nothing and the cursor would stall on it;
the pass is finished when a batch takes no further id, and each batch is idempotent, so running it twice, or from two processes at once, writes the same table.
A boot after the pass has finished reads that one row and stops, so it costs no read of `artifact_versions` at all.
A failure stops the boot (`slog.Error`, then exit 1), unlike the `asks_anchor_artifact_id` count beside it:
this release's search reads `document_search` alone, so a process serving with the table unfilled would answer a search with documents missing.
The rolling deploy keeps that correct — the release before this one serves every request until the new process has filled the table and begins serving
— and the operator follow-up is that wait:
a pod of this release takes the fill's time (about 35 s on production's shape) to become ready on the first boot after the deploy, and the boot logs each batch and the total.

Migration `20261101182508_search_term_queue` creates `search_term_queue`, the items whose search text has changed since each project's word counts were last written.
The related-terms refresh counted every issue, document, comment, ask and message of every project every ten minutes, 1,882 s of database time in the 24 hours to 2026-10-30;
with this table it applies what changed and counts a project again only where no pass of this build has counted it.
Each row names the kind of item, the item
— a comment, an ask and a message by their own id, an issue by its key, a document by its artifact
— and the issue or the artifact whose project it takes, all copied from the row the trigger was handed, so a pass finds the project of an item that has since been deleted.
It carries the vector the item held before the change, which is what a pass subtracts, and the `pg_current_xact_id()` of the transaction that wrote it, which is what tells a change a pass's own snapshot sees from one committed after it.
The table declares no foreign key, to any table:
a `references` makes the census size the table it points at, and when this migration was written the census did not admit a `create table`, so it would have held that table to the size limit.
A key to an item would also refuse the very row a delete queues.
Today the census still sizes the table a `references` points at, and it holds that table to the limit unless the migration is exempt ("Pre-deploy census" below), which needs the census to admit every statement of the migration.
Both statements lock only the table this migration creates, which no other session can reach before the commit, so it takes milliseconds.
Its census answers `0`: the table is new and the migration reads no other.
No operator follow-up — the queue starts empty, and the first pass of this release counts every project in full, so nothing is owed for the writes made before it.

Migration `20261101182515_search_term_totals_snapshot` adds `snapshot pg_snapshot` and `snapshot_at timestamptz` to `search_term_totals`, both nullable and with no default.
Every pass of this release writes `items`, `refreshed_at`, `snapshot` and `snapshot_at` in the one statement that counts, `refreshed_at` and `snapshot_at` from that statement's `statement_timestamp()` and `snapshot` from the `pg_current_snapshot()` it takes there, and then drops exactly the queue rows that snapshot sees.
A later pass applies the queue on top of those counts only where the row is its own work, which is `snapshot is not null and refreshed_at is not distinct from snapshot_at`; otherwise it counts the project again.
The signal is that comparison and not a null snapshot, because the release before this one updates `items` and `refreshed_at` alone and leaves this release's snapshot in place, so during a roll a rule keyed on a null snapshot would apply the queue on top of counts that rebuild already held.
Adding a nullable column with no default writes `pg_attribute` and rewrites no row;
the `ACCESS EXCLUSIVE` it takes on `search_term_totals`, one row per project, is held for that catalog write alone.
Its census answers `0`: it reads no row, rewrites none and refuses none, and every row already there reads as the null snapshot of a project this release has not counted yet.
No operator follow-up.

Migration `20261101182522_search_term_queue_triggers` creates the five functions and the five triggers that fill `20261101182508`'s queue:
`asks` on `insert or delete or update of question, options, answer, search`, `comments` on `body, search`, `issues` on `key, title, search`, `messages` on `body, search` — the columns each item's search vector is built from (`0057`–`0061`) and the vector itself, since a write that sets `search` directly is a change to what the project holds and nothing else would see it
— and `document_search` on `insert or update of search`, which is where a document's change is taken from, because `20261101182447`'s table already holds exactly the vector the refresh counts and `20261101182454`'s trigger writes it inside the `artifact_versions` write.
The event broker advances `issues.last_seq` and `artifacts.last_seq` on every event and neither column is in a list here, so an event append queues nothing.
Each trigger is one insert that reads no row, which is what holds the writes to the 10% this change allows them.
Eleven statements, a `set` and then each a `create function` or a `create trigger`, which is what exempts the migration from the census's table-size limit (`internal/pgmigrate/exempt.go`):
the exemption is per migration and not per table, so one migration carries all five where five would have taken five lock windows and four more migrations.
`create trigger` takes `SHARE ROW EXCLUSIVE` on its table
— reads are not held, writes wait for the commit
— and the five are taken in one fixed order, `asks`, `comments`, `issues`, `messages`, then `document_search`, so two migrations written to this rule cannot each hold a lock the other waits for.
The four item tables are small (the census of 2026-10-30 gives `comments` 110 MiB and `messages` 20 MiB), and the functions, which lock no table, are created first, so the lock window is the five `create trigger` statements alone.
The migration still holds each table's lock while it waits for the next, and a live writer that writes two of these tables in the other order closes a cycle with it:
a comment write that goes on to write `asks`, as for `0066`, or a version write, whose insert takes `document_search` through `20261101182454`'s trigger and whose task-progress update then takes `issues`.
So it sets its own `lock_timeout` of 500 ms, shorter than `deadlock_timeout` (1 s by default), as `0066` does:
it gives up first (`55P03`, nothing applied), the writer finishes, and the runner applies it again, `pgmigrate.LockTimeoutAttempts` times, three, two seconds apart, then at the next boot.
The cost is `0066`'s too: a holder that outlasts half a second on every attempt fails that boot, an autovacuum of `asks`, `comments`, `issues` or `messages` among them where `deadlock_timeout` is not shorter, which the census refuses (below);
the release before keeps serving, and a retry is safe because the failed attempt's transaction rolled back.
Its census answers `0`: a trigger runs only on later writes and rewrites no row already stored.
The operator follow-up is the queue itself:
each row carries a copy of an item's old vector, a 660 KB document's included, so if the refresh loop stops, `select count(*) from search_term_queue` grows, and the recovery is a full build, which the refresh already runs for a project whose snapshot is null.

Migration `20261101183525_graph_edges_ask_parts_and_block_pointers` re-states `graph_edges` once with the arms of both migrations of this release that wrote it.
`20261101143211` added an ask's parts and `20261101144855` filtered the `block_refs` arm by `block_ref_listed`, each from `0142`'s arms, and timestamps sort them, so the later one replaced the view the earlier one wrote.
This states every arm of both, and is the arm list a later migration re-states from.
It creates nothing and reads no stored row; it refuses and rewrites no row, and its census answers `0`.
It takes the view `ACCESS EXCLUSIVE` for its short transaction, as the two before it do.

Migration `20261103205037_refs_grammar_owed` creates `refs_grammar_owed (kind, id, enqueued_at)`, the queue of text owed an index with the whole reference grammar
— the bare issue keys, the `#id`s naming a document's question blocks and the short ids a text writes — which a build that predates them indexes without.
`kind` is `comment`, `ask`, `message`, `artifact` (a document at its latest version) or `scope`, whose id is a document whose question blocks moved and which owes a re-index of every source whose text belongs to it (`refs.ScopeSources`), and `refs_grammar_owed_enqueued` is the index the drain reads oldest first.
It is a queue of its own rather than a row in `0121`'s `block_grammar_owed` because a server of the previous release drains that table:
during a rolling update it would consume the rows seeded there, index that text with the grammar it knows and delete them, and this release would never re-index them.
It also creates the three trigger functions no trigger calls until the four migrations after it (`refs_grammar_owe`, whose kind is its trigger's argument, `refs_grammar_owe_version` and `refs_grammar_owe_ask_scope`).
It creates a table, an index and three functions and touches no row, and the release before it neither reads nor writes any of them; its census answers `0`.

Migrations `20261103205038_comments_refs_grammar_trigger`, `20261103205039_asks_refs_grammar_trigger`, `20261103205040_messages_refs_grammar_trigger` and `20261103205041_artifact_versions_refs_grammar_trigger` attach those functions, each in its own migration so each transaction holds one table's `SHARE ROW EXCLUSIVE` alone (`0122`-`0125`'s precedent), so a write of that table waits behind it while a read does not.
They enqueue whatever the text says: no cheap SQL predicate finds a bare issue key, a `#id` or a short id, where `0123`-`0125` could test `like '%#b-%'`, and `0122`'s document trigger is the precedent for a trigger that enqueues every write.
What they test is whether the write changed what the index reads
— the text, and the columns that say which document a `#id` in it resolves against (`refs.ScopeOf`)
— since a write that leaves every one of them as it found it cites exactly what it cited.
A comment is owed on insert and on an update that changes `body`, `reply_to`, `artifact_id`, `ask_id`, `issue_key` or the `artifact_id` of its `anchor`, and an ask on insert and on an update that changes `question`, `block_artifact_id`, `artifact_id`, `issue_key` or the `artifact_id` of its `anchor`;
an issue message is owed on insert and on an update of `body`, and a direct message, which no mention is indexed from, is owed nothing.
Each of the two tables carries two triggers for this, one for the insert and one for the update, because a `WHEN` clause of an insert trigger may not name `OLD`.
The rest of an anchor is left out on purpose:
a settlement rewrites `anchor.quote` and `anchor.orphaned` as a quote moves (`docs/marks.go`) and writes `anchor.block_id` (`docs/anchor_blocks.go`) without re-indexing anything, so a trigger on the whole column would leave a row owed, and the drain re-reading an unchanged text, on every settlement that moves a quote.
An ask's `block_id` is left out for the same reason it is named by the scope triggers below:
it keys the question map of the document the block is on (`refs.QuestionMap`) rather than the ask's own scope, so a change to it owes that document's whole scope, which re-indexes the ask with every other source of it.
A markdown version owes its document and that document's scope, since the version declares the question blocks every `#id` in its scope resolves against, and an image's or file's version owes neither.
An ask that gains or changes `block_id` or `block_artifact_id` is the settlement of a question block, so it owes that document's scope, and the document it left as well where its block moved.
A write this release makes settles its own row in the write's transaction (`refs.replaceRefs`), so only a write of an earlier build is left owed.
Each touches no row; each census answers `0`.

Migration `20261103205042_refs_grammar_seed` owes that index to every text already stored:
each comment, each ask, each issue message and each document with a markdown version, and each such document's scope.
Those triggers are live by then, so a write that lands while it runs is owed by its trigger, and a row a trigger has already written keeps its `enqueued_at`.
It takes `ACCESS SHARE` on `comments`, `asks`, `messages`, `artifacts` and `artifact_versions`, which no write waits behind, and writes only `refs_grammar_owed`.
Its census answers `0`: the one table it writes is `20261103205037`'s, which holds no row at deploy, and the rows it inserts are new work owed, not a rewrite.
The server drains them itself, a fourth batch of `docs.Service.RunBlockIndexReconciliation`, so the links and the edges of text stored before this release appear with no edit and no command run.

Migration `20261102110000_artifact_document_roles` gives `artifacts` a nullable `role`, `verification` for the document an issue holds as `verification.md` and `plan` for its `plan.md`, null for every other document.
The upload route sets it from the slug when it creates the row and never changes it, as `is_primary` never changes, and the Verification and Plan tabs find their document by it rather than by reading names back.
Two checks hold it to those two values and to a markdown document of an issue, and the partial unique index `artifacts_issue_role` holds an issue to one document per role.
The backfill gives the role to the issue documents already stored under those slugs, so the Plan tab works on a plan written before this deploy;
`artifacts_ref_key_key` (`0009`) makes `<issue key>/<slug>` unique, so no issue holds two of either.
The column is nullable with no default, so the previous release keeps writing documents with no role, and the add and the backfill take `artifacts` `ACCESS EXCLUSIVE` in the migration's transaction.
It refuses and rewrites no row; its census answers `0`.

Migration `20261102110100_asks_approval_documents` lets one approval request name more than one document:
a spec and its issue's Verification document are approved in one Inbox card, and the human's answer records a review on each.
`asks.approval` grows a `documents` array, one entry per document with its `artifact_id`, `name`, `version` and `requested_version`, whose first entry is the document the request was opened on;
`approval.artifact_id`, `name`, `version` and `requested_version` go on holding that first document's four values, so the previous release reads such a request as the one-document request it has always read.
The backfill writes the array every stored approval already implies, and the trigger `asks_normalize_approval_documents` writes it for a row the previous release inserts or rewrites during a rolling deploy;
the rebuilt `asks_approval_kind_check` then requires the array, each entry held to the bounds the top-level four have always been held to.
`asks_open_approvals` (`0014`) still indexes the first document alone, and the new `asks_open_approval_documents` indexes every document of an open request, so a read by any of them seeks.
Every statement locks `asks` alone, the index build taking `SHARE`, so an ask write waits for it;
it refuses no stored row and rewrites only the approval blobs it fills in, and its census answers `0`.

Migration `20261102110200_artifact_review_kinds` gives `artifact_reviews` a not-null `kind` defaulted `person`, the other value being `plan_review`:
the approval a plan review records of a plan version (`POST /api/v1/artifacts/{id}/plan-reviews`), which no human is ever asked to give.
A plan review's state is `approved`, so `0120`'s trigger gives its row a sequence as it gives every other review one, and the approval goes stale at a later version exactly as a spec's does.
The constant default rewrites no row, so the statement holds `artifact_reviews` `ACCESS EXCLUSIVE` for the catalog change alone; its census answers `0`.

Migration `20261102122200_check_results` creates `check_results`, one row per result recorded against a `check` block of an issue's Verification document (`dispatch result`).
A row names the issue the result was recorded on, the check's own issue, document, block id, that block's `changed_version` and `text_sha256` when the result was recorded, the `role` that ran it, the `surface`, `run`, `observed`, `commit_sha`, `negative_control`, the `failed`, `skipped` and `excluded` counts, the `verdict`, the `screenshots`, the session and actor, and the `event_seq` of the `result.recorded` event on the check's issue.
The stored digest is what says the check's words moved after a result was recorded: a read compares it with the block's digest now.
Nothing updates a row — another run is another row — and a table check refuses a `pass` that counts a failed, skipped or excluded case.
`check_results_check` serves the Verification tab's read of one check's results newest first, and `check_results_issue` an issue's own reads.
A new table: nothing existing is read, locked or rewritten, the previous release neither reads nor writes it, and its census answers `0`.

Migration `20261117093000_check_results_commit_repo` adds the nullable column `check_results.commit_repo`: the repository the record route took a result's commit to be of, written only where that is a repository the check's issue links a pull request in rather than the one the issue resolves to, and null for every other row, the rows before it included.
Such a repository is one with an open or merged linked pull request that GitHub said holds the commit, and no other: where a linked read did not answer and no repository is shown to hold the commit, no result is stored at all (`502 GITHUB_UNREADABLE`).
The results read compares an `after_merge` or `default_branch` result with that repository's branches where the column names one, and reads a `before_merge` result's pull requests through it.
A nullable column with no default rewrites no row; the ALTER holds ACCESS EXCLUSIVE on `check_results` for the catalog write alone, the previous release neither reads nor writes the column, and its census answers `0`.

Migration `20261116163813_issue_role_holders` creates `issue_role_holders`, the session that holds each Legion role of an issue, which the Legion daemon publishes (`PUT /api/v1/issues/{key}/role-holders`, Routes below).
The key is `(issue_key, role)`, and a unique index on `(issue_key, session)` keeps a session to one role of an issue.
The `role` check names the six Legion roles, and a row goes with its issue.
A later release reads the table in the write transaction of a result, so that the stored role of a result is the role that its session holds.
A new table: the foreign key holds `issues` in `SHARE ROW EXCLUSIVE` for the migration's transaction, so an issue write waits behind it while a read does not; otherwise nothing existing is read or rewritten, the previous release neither reads nor writes it, and its census answers `0`.

Migration `20261120131444_parent_status_fix` creates `parent_status_fix`, the record that the one-time parent status fix ran on this database (Running more than one process, above).
It holds one row at most: `id` is always true, which its primary key and its check hold it to.
`ran_at` is when the fix wrote the row, by the database's clock.
`unlinked` counts the issues that the fix unlinked from a `done` parent, and `moved` counts the issues that it moved to `in_progress`.
With `DISPATCH_PARENT_STATUS_RULE` on, the process that runs the fix writes the row after the fix's last change commits, and every process serves only once the row is there (`api.RunParentStatusFixAtBoot`).
A boot that finds the row runs no fix, also after the setting was off for a time.
A new table: nothing existing is read, locked or rewritten, the previous release neither reads nor writes it, and its census answers `0`.

Migration `20261120204136_spec_writer_guard` adds `projects.proven_sessions`, `boolean not null default false`, the project's proven-sessions setting (`/api/v1/projects/{key}/proven-sessions`, Routes below), and creates `project_role_holder_marks`, one row for each project whose role holders Dispatch has taken a publication of (`PUT /api/v1/issues/{key}/role-holders`).
The check of who writes the spec and the Verification document of an issue that Legion runs reads both (`403 DOCUMENT_WRITER_NOT_ARCHITECT`, Document errors below): it holds only the issues of a marked project, and the setting decides whether an architect's session must be proven.
The role-holder route writes the project's row of `project_role_holder_marks` in the publication's transaction, and nothing deletes it; the migration marks each project that already has a row in `issue_role_holders`, as its next publication would.
The mark is a table of its own and not a column of `projects`: a write of a `projects` row from the role-holder route would take that row's lock under the issue's owner lock, while an issue create takes the project's owner lock and then waits for that row to number the issue, the order that deadlocked (`internal/dispatch/api/project_gated_steps.go`).
The setting is off by default, so every project keeps the behaviour it has until a `PUT` turns it on, and a project the previous release inserts, naming its own columns alone, reads as off.
A column with a constant default writes the catalog alone and rewrites no row; the `ALTER` holds `projects` `ACCESS EXCLUSIVE`, and the foreign key its `SHARE ROW EXCLUSIVE`, until the migration commits, the fill included.
The previous release neither reads nor writes either, and the census answers `0`.

Migration `20261121043859_request_key_steps` creates `request_key_steps`, one row for each committed step of a keyed status change (`POST /api/v1/issues/{key}/status-change`), in the order the steps committed.
A row holds the step's entry as the route answers it, its issue included, so a repeat of the call answers each issue as its step's write answered it, rather than as it stands then.
A row of a step of an option also holds, in `moved`, what that step changed, which the comment on the issue the call names lists; `moved` is null for the step of the issue the call names.
The head of the call's recovery point stays in `request_keys.recovery`, and a row goes with its key through the foreign key's cascade.
Each step adds one row, where the step would otherwise rewrite a growing value in the key's row.
A new table: the foreign key holds `request_keys` in `SHARE ROW EXCLUSIVE` for the migration's transaction, so a keyed request's claim waits behind it while a read does not; otherwise nothing existing is read or rewritten, the previous release neither reads nor writes it, and its census answers `0`.

Migration `20261117190118_events_owed_keys` adds `events.owed_keys`, nullable `text[]` with no default: what an event still owes once the outbox's last record of it was written (`recordBatch`, `owedKeys`).
It holds the key of each destination of the event's list that no batch has reached, the owner's topic family spelled `topic-family`, a session's own subject and a role's topic by their subjects, and `any-session` beside them when the event's recipient read failed;
an event the record publishes owes nothing, an empty array, and so does one the outbox cannot read: a row it read but cannot publish (its list holds a null, or its actor or payload does not decode), and a row that does not scan, which the batch still knows by its id and its owner's `last_seq`, the first two columns it reads;
an event no record has written it for holds null.
The record writes the column only while the publisher hold is its own, so a record made after its process lost the hold leaves it as it was, on an event it publishes too, where nothing reads it: both rules read unpublished events alone.
The same `ALTER` adds `events.owed_seq`, nullable `integer`: the owner's `last_seq` the batch that wrote `owed_keys` read beside the routing they come from, written with them and only with them.
A copy of a mention - the API's own send and the outbox's fallback row - waits behind every earlier event of its owner that still owes a send its session may take, or whose `owed_seq` is behind the owner's newest event - a write since the record may have made the session a destination of it (`model.MentionCopyHolderSQL`, read with the role routes in one statement, `model.MentionCopyRuleSQL`) - and null owes the topic family, so an event written before the migration holds those copies until the outbox records it again or publishes it.
The newest event is the newest that is no pending subscription command, which makes no session a destination and which no batch reads, so it leaves the record current.
The read walks `events_outbox_owner`, and reads the owner's newest seq once, backwards over its unique `(owner, seq)` index: the subquery ends with `offset 0`, so the planner does not run it again for each earlier event.
The columns are a catalog change that rewrites no row, and the `ALTER` holds `events` `ACCESS EXCLUSIVE` for that alone; its census answers `0`.
The previous release neither reads nor writes them, so the two roll side by side.
An event that release records keeps the values this release wrote last, or null, and every event written since - by either release - moves its owner's `last_seq` past them, so a copy this release sends waits until this release records the event again: while the previous release holds the publisher hold, copies on an owner with an unpublished earlier event wait longer, and pass none of its events.
This release also writes both again for every unpublished event a batch reads not due, when either changed.

Migration `20261120021308_search_text_versions` creates two new tables, which the keyword fill writes (Background loops, above) and a search at a past time reads (Search, below).
`search_text_versions` holds one row for each version of the text that keyword search reads of an issue, a comment, an issue message or an ask.
Its key is `(kind, id, event_seq)`, and a new version is a new row.
A row is updated in one case alone: the fill's re-read, below, writes again the rows a fill of a lower rules version decided otherwise than this build's rules.
`search_text_versions_fill` holds the fill's one cursor row, with the reach the fill stored, the holder that wrote the last batch, and the version of the event rules that wrote the rows up to the cursor.
`search_text_versions_item_start` on `(kind, id, started_at)` serves the relation's test for a later version of the same item.
`search_text_versions_search` is a GIN index on the vector with `fastupdate = off`, so a search never scans a pending list and an insert never empties one.
Neither table has a foreign key: a key locks the table it names, and a version is history that a later change to its item must not follow or refuse.
It is a new-table migration: it holds only `create table` and `create index` on the two new tables, the release before it neither reads nor writes them, and its census answers `0`.
No migration writes a row of either table.
At production's counts the table and its indexes take about 2 GB (1.9 GB measured on real text, 2,134 MB on the keyed copy) and grow by 30 to 40 MB a day, which is above the census's 1 GiB limit.
So once the table is filled, the census refuses a later migration that names it unless the census admits every statement of that migration.
A change to its shape is a new table that the fill writes again.
The cursor row's `rules` holds the lowest version of the event rules that wrote the rows up to the cursor: each batch keeps the lower of its own version and the one stored, so a newer build that takes the hold after an older one does not hide the older one's events.
Version 2 reads, besides version 1's, `message.edited`, `comment.deleted` and `message.deleted`, and takes a cut post body from the post's body history (`post_body_history`).
A version 1 fill that runs beside a version 2 build during a roll passes over the events of those three types and reads a cut body from a post's row after a later edit or delete changed it; and it lowers `rules` to 1.
So a batch of version 2 that finds `rules` below 2 reads again before it writes past the cursor (`textversions`' `rereadBatch`), in batches bounded as the fill's are, each after the pause and before the rest:
every event of the three types up to the cursor, decided against the row its post held just before it, each version written unless the table holds it;
and each row of a post with a later edit or delete whose body its event cut and that was read from another source than the post's body history, written again as version 2 decides it.
Its last batch raises `rules` to 2, and only it does: a re-read stopped before then, or a later version 1 batch that lowers `rules` again, has the next version 2 batch read again from the log's first event, which writes nothing twice.
Only a build with version 2 writes an event of the three types, so every one in the log was written by the roll that brought them or later, and the re-read misses none.
Its read takes the events of the types that change a post's text through `events_post_text_changes` (below), under a batch's statement time.

Migration `20261121160853_events_post_text_changes` creates `events_post_text_changes` on `events (id)` where the type is `comment.deleted`, `comment.edited`, `message.deleted` or `message.edited`: the events the keyword fill's re-read reads, in id order, which it took from a scan of the whole log before, 7.4 s of an 8.0 s re-read at 900,000 events.
The re-read's statement names the same list as text, not as a bound argument, so a generic plan proves the predicate too (`textversions`' `rereadLogTypes`, held to the migration and to the plan by `TestTheReReadsLogReadUsesItsIndex`).
It is built `concurrently` and non-transactional, as `20261111002444_events_person_words_with_declines` is, so it holds off no write to `events`; its census answers `0`.

Migration `20261102122300_pull_request_heads` creates `pull_request_heads`, the newest head Dispatch holds of every pull request it has seen an event for, so a recorded result can say whether it ran at the commit the work stands at.
This is not `delivery_pull_requests`, which keeps the merged pull requests of the delivery population and no head commit:
this table holds open ones too, by any author and in any repository, one row per pull request.
A row names the `repo` and `number`, the repository under the canonical name it is called today (`refs.Renames.Canonical`, over the rename pairs the server read at boot), so one repository stored under two names keeps one row and one attribution (`20261105075455_repo_projects_renamed_to`, below);
then the `head_sha` (on a merge, the final head of the merged work), the `head_ref` and `base_ref`, the `state` (`open`, `closed` or `merged`), the `issue_key` Dispatch's attribution picks and which of its five sources named it, the title and URL, the pull request's `updated_at` as the event reported it, and when the reconcile or a read-through last read it through the App.
That key is the first issue the five sources name that resolves to the repository the pull request is in, not the first key they name (`attributionCandidates`, `attributionHeld` in `delivery/attribution.go`):
a pull request titled after another repository's issue therefore keeps its own, and one no candidate of which resolves to its repository is attributed to nobody, with the source `none`, as is one whose head branch is in another repository (`headInOwnRepository`).
The hold is the server's own repository-resolution rule spliced into the statement that reads the candidates (`refs.IssueRepoTerms`, `attributionCandidatesQuery`), so the attribution and the issue page cannot disagree, and a pull request's best 25 candidates are the ones read (`attributionCandidateBound`).
One rule picks both attributions: this row's `issue_key` (`headAttribution`) and the issue of a `delivery_pull_requests` row, which the reconcile resolves again from the database alone (`AttributePullRequests`).
An older event arriving after a newer one leaves the row as it is, and the reconcile re-reads an open row older than 4.5 minutes (`delivery.PullRequestHeadStaleAfter`).
That head pass runs whether or not the deployment has delivery settings, and before the deploy repository's permission check (`Reconcile.runOnce`):
a recorded result is compared with those heads in every deployment, and behind the settings the pass never ran in one that had none, so a result recorded at a head a later push replaced went on standing at it for ever.
A failed head pass is logged, and named in `delivery_settings.last_error` beside the other steps' failures when the pass reached them.
The head pass also takes every merged row credited to nobody that the rule the running build judges by has not judged
— one no read of GitHub has reached, and one an older rule judged
— and offers such a row once per rule version (`delivery.AttributionRuleVersion`, `20261105070440_pull_request_heads_attribution_rule` below):
a `pull_request` envelope naming no head repository attributes nobody, a pull request merged under an older rule keeps whatever that rule concluded, and nothing else ever looks at a merged row again, so without that arm a result at such a merged head would stand at no basis for good.
The row is written outside the reconcile by the `pull_request` envelope, and by the read-through made for a commit that heads no pull request Dispatch knows (`delivery.Attributor.ReadThroughCommitPulls`):
a recorded result makes one, and so does the read of an issue's results, which spends at most one such read per repository and commit every `delivery.CommitPullReadStaleAfter` (`GET /api/v1/issues/{key}/results`, Routes below).
`pull_request_heads_issue_head` answers the heads one issue's checks are compared with, `pull_request_heads_open_stale` the reconcile's open batch, `pull_request_heads_merged_unread` and `pull_request_heads_merged_rule` its merged arm
— the first partial on `state`, `issue_key` and `checked_at`, the second on `state = 'merged'` and keyed by `attribution_rule` (`20261111060005_pull_request_heads_merged_rule`)
— and `pull_request_heads_repo_head` (`20261105112616`, below) the question the results read asks before it reads GitHub:
whether any open or merged stored pull request has this commit of this repository as its head.
A new table, as above; its census answers `0`.

Migration `20261102122400_default_branch_commits` creates `default_branch_commits`, what Dispatch's GitHub App answered about one commit of one repository: whether the repository's default branch carries it.
It cached the answer the results read asked for while that one question decided an `after_merge` and a `default_branch` check alike, a yes kept for good and a no re-read after five minutes.
`20261107234701_branch_commits` below took that answer over, keyed by the branch as well and seeded by a merge with its own merge commit on the base branch, and `20261110004829_drop_default_branch_commits` drops this table.
A build from before `20261107234701`'s release still serves on the schema that drop leaves, but its results read finds no cache: every `after_merge` and `default_branch` check reads GitHub on each results read, and logs `dispatch: read a commit's default-branch answer` and `dispatch: record a commit's default-branch answer` at WARN.
A new table, as above; its census answers `0`.

Migration `20261107234701_branch_commits` creates `branch_commits`, what Dispatch's GitHub App answered about one commit of one branch of one repository: whether that branch carries it.
A result for an `after_merge` check stands at a commit on the branch the check's issue's pull request merged into or on the repository's default branch, and one for a `default_branch` check only at a commit on the repository's default branch, so the question the results read asks names a branch and the answer is kept under one; the answer is read when the results are read, never written by a push, so this cache keeps those reads off GitHub.
Its primary key is `(repo, branch, sha)` — the repository, the branch as GitHub names it, and the 40-hex commit — with `on_branch`, GitHub's answer, and `checked_at`, when it was read.
A yes is kept for good, since a rewrite of a branch does not undo that the work reached it, and a no is re-read after five minutes, on every branch alike, since a commit merges later.
Recording a merged pull request writes one row of its own: the merge commit is on its base branch, which GitHub's merge proves and nothing can prove later (`delivery.UpsertPullRequestHead`): in a repository that deletes a branch when its pull request merges and merges by squash, no later compare read can show that commit on the base, and the default branch holds a squash of it instead.
Merges recorded before this release get no such row and nothing backfills them, the merge commit being carried only by the merge event, and neither do merges a process of the release before this one records while the release rolls, since that build writes no `branch_commits` row; on 2026-11-08 no issue that merged into another base holds an `after_merge` result, so that set is empty.
It is a new table and not a `branch` column on `default_branch_commits`, whose primary key was `(repo, sha)`, because a Dispatch release rolls: the release before this one ran `insert ... on conflict (repo, sha) do update` against that schema, and widening that key would have left that statement targeting no unique constraint, so every such write would have failed, while that build's single-row read could have answered another branch's yes as the default branch's.
So the release that added this table wrote nothing to `default_branch_commits` and left it as it was.
The table starts empty, with no backfill: a `default_branch_commits` row named no branch, and the default branch it was asked about may since have been renamed, so no backfill could be faithful and every answer is earned again with one GitHub read.
A new table, as above; its census answers `0`.

Migrations `20261105093500_user_threads_dismissed_at` through `20261105093507_user_agent_thread_dismissed_clear` are the Inbox's dismissal:
a thread stays in a person's Inbox, read or not, until they dismiss it.
`20261105093500` adds `user_threads.dismissed_at`, nullable with no default and no backfill, so the statement writes the catalogue row and nothing else:
no rewrite, no row read, and `ACCESS EXCLUSIVE` for that write alone.
`20261105093501` replaces `user_threads_record` with the same function plus one assignment, so the statement that moves the other participants' `replied_at` on a write by somebody else clears their `dismissed_at` when that write is newer than it;
`create or replace function` locks the function's own catalogue row and no table at all.
`20261105093502` and `20261105093503` create `user_threads_inbox` on `(login, replied_at desc, thread_id desc) where replied_at is not null and dismissed_at is null`, the Inbox list's read, and `user_threads_dismissed` on `(login, dismissed_at desc, thread_id desc) where dismissed_at is not null`, the Dismissed view's read and the cursor it resumes from.
Each column order is its read's own, the login it narrows to and then the order it answers in, so one index scan answers a page with no sort, and each predicate is its read's own filter, so neither state scans the table.
Building either takes `SHARE` on `user_threads`, so the writes its triggers make wait for the build while its readers do not, and a plain `create index` scans the table and sorts every row the predicate keeps, read rows included.
The window is one scan and sort of `user_threads` — a row per person per thread they have written in — with no row rewritten and none refused.
These unmarked migrations run in a transaction, so `create index concurrently` cannot run inside them, as for `0148`.
`20261105093505` creates `user_agent_thread_dismissed (login, message_id, dismissed_at)`, the same mark for an Agent thread, which has no `user_threads` row to carry one, with an index on `(login, dismissed_at desc, message_id desc)` for the Dismissed view's own read of it;
the table holds no row until the migration commits, so no existing table is locked and nothing is backfilled.
Its primary key is `(message_id, login)`, that order and not the other:
the next migration's trigger deletes by the root alone, since the reply that brings a thread back does not name whoever dismissed it, and under `(login, message_id)` every session reply would scan the table.
The Inbox's own reads want one person's whole set of marks, which the `login` index answers, and a dismiss and a restore name both columns, so two indexes serve all three reads.
`20261105093507` is the Agent half's own "a write by somebody else brings the thread back", which `user_threads_record` does for a Reply thread:
a trigger on `messages` that clears a root's mark when a session replies in it after that mark, in the reply's own transaction.
Somebody else on this half is the session, the only writer the Inbox reads in such a thread, so the person's own reply from the Dismissed view leaves the thread dismissed, exactly as their own reply leaves a Reply thread's mark where it stands; a mark at or after the new reply stays.
With it, "dismissed" is what it is on the other half
— a mark exists — so no read compares two times, and the Inbox's own reads leave a dismissed root out without reading its thread, which was 66 ms of the Inbox read's 69 at 10,000 dismissed Agent threads.
`create function` takes no table lock and `create trigger ... after insert on messages` takes `SHARE ROW EXCLUSIVE` on `messages` for the one catalogue write, blocking that table's writers and no reader; neither reads or rewrites a row.
Every census in the run answers `0`, and each up-file names the lock it takes and the row count its window was judged against.

Migration `20261105014808_comments_answers` gives `comments` a nullable `answers`, the comment a reply answers, and re-states `graph_edges` with one more arm, `answers` from the reply to that comment ("Reference graph" below).
A comment write climbs the reply chain to the head of the thread before it stores, which is what every thread read selects on, so `reply_to` holds the head and the comment the writer answered was lost;
`answers` holds the comment the request named, written whenever the request named one, and `reply_to` keeps exactly the value it held before, so every thread read answers as it did.
A reply whose `answers` and `reply_to` name the same comment is listed once, by `replies_to`, wherever `replies_to` is listed;
an explicit `answers` query still lists it.
A reply listed under a current comment block is not also listed as an answer to the whole comment, including when that comment is nested;
explicit `answers` queries still return it.
The column is nullable with no default, a catalog change that rewrites no row, and its foreign key to `comments(id)` matches `reply_to`'s (`0001`), so the add holds `comments` `ACCESS EXCLUSIVE` for milliseconds and the partial index `comments_answers` holds `SHARE` while it builds.
Migration `20261105083800_comments_answers_text` adds the partial expression index on `answers::text` used by graph reads and backlink counts, retaining the UUID index for foreign-key checks.
It holds `comments` `SHARE` while the index builds, rewrites no row, and its census answers `0`.
Re-stating the view holds `graph_edges` `ACCESS EXCLUSIVE` for the migration's short transaction, so a reference read waits behind it.
The arm list it re-states is `20261101183525`'s, with the new arm beside the comment reply arm, and it is the arm list a later migration re-states from.
The release before it names the column in no statement and writes every reply with `reply_to` alone, so a reply written during the roll, or after a rollback, carries no `answers` and reads as a reply to its thread's head, which is what every release before this one shows.
It refuses and rewrites no row; its census answers `0`.
Rolling Dispatch back past it is an image rollback alone;
`20261105014808_comments_answers.down.sql` is the by-hand script that also drops the column, which loses which comment each reply answered.

Migration `20261105070440_pull_request_heads_attribution_rule` gives `pull_request_heads` a not-null `attribution_rule` defaulted `0`, the rule that last judged that row's `issue_key` and `attribution_source`, as `delivery.AttributionRuleVersion` numbers it.
`0` is what every row written before the column has:
the rule that took the first candidate the five sources named outright, whatever repository that issue resolved to.
A merged row is the one row nothing revisits:
an open row comes round at every reconcile pass of one process, each starting at most five minutes after the one before it ended, but the merged arm took a merged row only while no read of GitHub had reached it, a merge envelope that names a head repository stamps `checked_at` as such a read, and a read-through runs only for a commit that heads no pull request Dispatch knows.
So a pull request merged under an older rule kept for good whatever that rule concluded, and one that rule credited to nobody left every `before_merge` result at its merged head standing at no basis for ever.
The column bounds the repair: the reconcile's merged arm takes a merged row credited to nobody that this build's rule has not judged, and the read stamps the row at that rule whether or not it could attribute it, so every such row stored before the deploy is judged once more and then left alone, and a later change to the attribution reaches the merged rows already stored by raising that constant by one.
The migration replaces `pull_request_heads_merged_unread` to follow the arm it serves:
`20261102122300` made it partial on `state = 'merged' and issue_key is null and checked_at is null`, and this one leaves it partial on the first two of those alone, since its arm takes such a row whatever its `checked_at` says.
`20261111060005_pull_request_heads_merged_rule` restores the third once the arm widens again, and gives the merged rows an older rule judged an index of their own.
The release before this one keeps working across that drop:
its query is partial on all three columns, so every row that query wants is in the new index, which serves it with the one test on `checked_at` left to the recheck, and an index decides a plan and never an answer, so that release answers the query correctly from a sequential scan too.
Adding a column with a constant default writes the catalog alone, so the statement holds `pull_request_heads` in `ACCESS EXCLUSIVE` for milliseconds and rewrites no row, `not null` included;
that release neither reads nor writes the column and names its columns in every insert, so it keeps writing heads while it still serves.
`drop index` holds `ACCESS EXCLUSIVE` for the instant it unlinks the index and `create index` `SHARE` while it builds, both inside the runner's one transaction, over a table that holds a row per pull request Dispatch has seen an event for.
Its census answers `0`.

Migration `20261105075455_repo_projects_renamed_to` gives `repo_projects` a nullable `renamed_to`, the full `owner/name` that mapping's repository is called today, null for one that was never renamed, which is every row the migration leaves.
One GitHub repository under two names was two repositories to Dispatch:
`sjawhar/legion` was renamed to `cybermonklabs/legion`, both names map to project INFRA, and an issue recorded under the old name was held to no pull request on the new one (`attributionHeld`) while the App, asked about the old name, answered 404 for its installation, so Dispatch could say of no `before_merge` result of such an issue where its commit stood.
The pair lives on this row because this row is where Dispatch already held the old name, the operator set it through the settings route of the release that added it, under the authority that set a mapping, and Dispatch asks GitHub nothing:
a name a repository was renamed away from cannot be read at all, so a rename is declared, never discovered.
The canonical name of a repository is the new name of its pair, else the name itself, compared case-insensitively (`refs.Renames.Canonical`, and `refs.CanonicalRepoSQL` inside a statement), over the pairs the server reads once at boot (below), and these sites read it:
both sides of the attribution's hold (`attributionHeld`) and its link sources, which compare a URL under both names of a pair (`attributionCandidates`); the repository the results route asks GitHub about (`checkIssueRepository`) and a result's linked repository (`commitRepositoryOf`, `linkedPullRequestTerms`); the key every writer of a `pull_request_heads` row uses (`Attributor.RefreshPullRequestHead`, `Attributor.ReadCommitPulls` under `ReadThroughCommitPulls`, and `Intake.recordPullRequestHead`), which re-keys a row stored under an old name and drops that row (`upsertHeadUnderCanonicalName`); the issue reference read, which finds a link under either name (`GET /api/v1/issues/resolve`, `resolveIssueRef`); and the CI reconcile, which reads every repository it covers under the name it has now, so no pass asks GitHub about a name it cannot read (`cireconcile`, `Reconciler.covered`).
A column check refuses a repository renamed to itself, and the route that wrote the pairs refused a pair whose target was itself renamed and a rename of a repository another mapping already pointed at, so the canonical name is one step away and no cycle exists.
The listener holds the other half of a rename:
`ENVOY_GITHUB_REPOSITORY_RENAMES` publishes every GitHub envelope of the current repository a second time under its former name, so the operator configures there each pair the rows hold (`packages/envoy/deploy/README.md`, "Listener configuration").
Adding a nullable column with no default writes the catalog alone, so it holds `repo_projects` in `ACCESS EXCLUSIVE` for milliseconds and rewrites no row;
the release before it neither reads nor writes the column and names its columns in every insert, so it keeps writing mappings while it still serves.
Its census answers `0`.
From the release of `20261119000230_projects_default_repo` (below) on, no route writes the column, and the server reads it once, at boot.

Migration `20261119000230_projects_default_repo` gives `projects` a nullable `default_repo`, the project's default repository:
the `owner/name` an issue of the project resolves to when neither the issue nor an issue above it names a repository (`refs.IssueRepoResolved`), and null for a project with none.
Its check holds a value to the form `parseIssueRepo` writes, never empty and in lower case.
In the same transaction it copies into the column the repository of each `repo_projects` row that carries `is_default`.
`repo_projects_default_per_project` (`0113`) holds those rows to one for each project, so every issue resolves to the repository it resolved to before.
The migration reads `repo_projects` and writes no row of it.
So a Legion daemon that still reads `GET /api/v1/settings/repo-projects` at boot, and a build rolled back to the release before this one, find the rows as the migration left them.
Neither of them sees a default that is set after the migration, which is on the project alone.
A default that the release before this one writes to `repo_projects` while the release rolls stays in `repo_projects` alone, and this release does not read it.
The release before this one names its own columns of `projects` in every read and write, so it serves on this schema, and a project it creates has no default repository.
The `ALTER` holds `projects` in `ACCESS EXCLUSIVE` to commit, the copy included, over one row for each project, and the copy reads `repo_projects` under `ACCESS SHARE`.
Its census counts the defaults the column's check would refuse, an empty name or one not in lower case, because the copy of one fails the migration and the boot with it.
The copy itself counts for nothing, by the rule `0133` states: it fills a column only this release reads and changes no value any reader had.

The same release ends every write of a rename pair, the `renamed_to` of a `repo_projects` row.
The server reads the pairs once, at boot, after it migrates (`store.ReadRepoRenames`), with both names of each pair in lower case, and it reads no pair once the table is gone.
It gives that one value, which nothing changes after the read (`refs.Renames`), to every reader of a repository name, so no other statement names `repo_projects` for a rename.
A read that fails is logged at ERROR and counted as a failure of the pass below, and the server then serves with no pairs.

Before it serves, the boot writes the new name of each pair in place of the old name in every stored repository field of Dispatch (`store.RewriteRenamedRepositories`, a boot task bounded at 30 s).
It writes fourteen lines of fields in this order, each line in one transaction:
`issues.repo`, `projects.default_repo`, `check_results.commit_repo`, `issue_external_links.url`, `pull_request_heads.repo` and `.url`, `branch_commits.repo`, `delivery_pull_requests.repo`, `.url` and `.attribution_cited_issues`, `delivery_runs.repo` and `.url` with `delivery_run_jobs.repo`, `delivery_run_warnings.repo`, `delivery_changes.repo`, `delivery_settings.deploy_repo` and `.excluded_repos`, `delivery_reconcile_progress.scope`, `architecture_sources.repo` and `ci_reconcile_passes.cursor_repository`.
A name matches when its lower case is the pair's old name.
A URL matches when its host is `github.com` or `www.github.com` and its first two path segments are the old `owner/name` in any case, and the pass writes the new `owner/name` there and keeps every other character.
A scope of the delivery reconcile's progress matches where it holds the old name as a token.
The transaction of each line takes the pass's one advisory lock (`repoRenameLockNamespace`, `0`) and a five-second `lock_timeout`, so two processes that boot at once take turns on each line, and the second finds nothing left to write.
Where the new name gives a row the key of another row, the row under the new name stays, and the pass deletes the row under the old name and logs it at WARN with its table and key.
The pass logs each field it writes at INFO with `field`, `from`, `to` and `rows`, and each line that fails at ERROR with its field, and it then goes on to the next line.
It writes no other column, `updated_at` included, and appends no event.
It never writes `repo_projects`, the event log, or the text of documents, comments, asks and messages, which keep their words.
After its writes it counts the rows of those fields that still name an old name.
It also counts them before its writes, and where that count finds none, it runs no line.
A failure of that first count is not a failure of the pass, which then runs every line.
Every boot logs one summary line, `dispatch: rewrote renamed repositories' old names`, with `pairs`, `rows`, `deleted`, `failures` and `left`:
at INFO when `failures` and `left` are `0`, and at ERROR otherwise, with `left=unknown` where nothing counted the rows.
A failure, a time-out included, does not stop the boot, and the next boot writes what is left.

From the same release on, every write of such a field stores the new name (`refs.Renames.Canonical` and `CanonicalURL`):
an issue's own repository and its external link URLs on a create and a patch, a project's default repository, the delivery settings' repositories, an architecture source's repository, and the issue URLs a pull request's body cites.
The attribution of pull requests, the results read, the writers of `pull_request_heads`, the issue reference read (`GET /api/v1/issues/resolve`) and the CI reconcile still read the pairs (`refs.Renames`, `refs.CanonicalRepoSQL`).
So a row that a failed pass left on an old name still attaches its pull requests.

Migration `20261105112616_pull_request_heads_repo_head` creates `pull_request_heads_repo_head` on `pull_request_heads (lower(repo), head_sha)`, the index behind the question the results read asks before it reads GitHub about a commit:
has any open or merged stored pull request this commit of this repository as its head, whatever issue it is attributed to (`delivery.HeadStoredAt`)?
A `before_merge` result whose commit heads no row attributed to the check's own issue is read through GitHub, which is how a result recorded before its pull request opened comes to stand at that head, and that read can repair nothing for a commit Dispatch already holds an open or merged pull request for:
the reconcile re-reads an open row at every pass of one process, each starting at most five minutes after the one before it ended, and offers a merged one once per rule version, so asking GitHub again from a read teaches nothing.
So the read asks this first, and a child's result on its parent's tab
— whose commit heads the child's own pull request, attributed to the child
— costs no GitHub call at all (`api/results_comparison.go`, `comparisonReader.readThroughHead`).
A closed-unmerged row counts for nothing there:
it is a basis for no result, no pass of Dispatch's reads it again, and a pull request closed at a commit does not stop another being opened from the same head, so counting one would leave every result at that commit standing at no basis for good.
The key is `lower(repo)` because that read compares the repository without case, as every other comparison of a repository does (`refs.CanonicalRepoSQL`):
a row written from a `pull_request` envelope carries GitHub's own spelling (`Acme/Widgets`) while the read is passed the lowercased spelling an issue's repository is stored in (`issues.repo`, or its project's `projects.default_repo`).
No index served that lookup: `pull_request_heads_issue_head` is keyed `(issue_key, head_sha)` and partial on `issue_key is not null`, which answers the heads of one issue and no lookup by repository and head commit, and the primary key is `(repo, number)`, so every results read of such a commit scanned the table.
`create index` takes `SHARE` on `pull_request_heads` while it builds, holding that table's writes for one read of a table that holds a row per pull request Dispatch has seen an event for;
the release before it neither reads nor writes the index, an index deciding a plan and never an answer, so it rolls beside this one.
Its census answers `0`.

Migrations `20261105145242_issues_search_spec` and `20261105145243_artifact_versions_issues_search_trigger` put an issue's primary document into the issue's own keyword vector ([Search](#search)), as `0140` put it into the issue's embedding.
`20261105145242` creates `issue_search_vector(key, title, spec)`, the one definition of that vector — the key and the title at weight `A`, exactly what `0088` wrote, joined with the spec's lexemes at weight `C` — has the issues trigger build `search` with it, and re-indexes the issues whose primary document holds text.
It keeps `0087`'s bound over the joined vector, halving the spec's part until it fits, so no write fails because an issue's spec is large.
The re-index writes `search` directly rather than `0062`'s no-op write of the title, which would queue every issue for re-embedding through `0073`'s trigger;
a direct write of `search` is still seen by `20261101182522`'s `issues_search_term_queue`, which must count those vectors again.
It takes `issues` in `ROW EXCLUSIVE`, and `search_term_queue` through that trigger, and reads each issue's latest primary version under `ACCESS SHARE` through `0140`'s `issue_embedding_spec`, so it names neither `artifacts` nor `artifact_versions` and no deploy is refused over the size of the 5.2 GiB table.
Its census answers `0`: `search` is a derived column no application code writes, every row it rewrites keeps at weight `A` every lexeme its key and title gave, and an issue whose document holds no text is not written at all.
`20261105145243` adds the function and the `after insert` trigger on `artifact_versions` that re-indexes an issue when its primary document gains a version, with `0140`'s join to the primary artifact and `20261101182454`'s `when (new.markdown is not null)` guard.
A function and a trigger are its only two statements, which is what exempts it from the census's table-size limit, and the re-index is the other migration's for that reason: an update is not such a statement, and one in this file would cost the exemption and refuse every deploy carrying it.
It is applied second so that a version an older build writes between the two commits meets no function the database does not have yet; such a version does not re-index its issue, which the next write of that document does.
Its census answers `0`: the trigger runs only on versions written after it.

Migration `20261121060636_search_document_text` creates `search_document_text(markdown)`, the one definition of a document's search text: its ask blocks removed, then `search_text` ([Search](#search)).
It replaces the bodies of `artifact_versions_search` and `20261105145242`'s `issue_search_vector`, which removed the ask blocks after `search_text`, so a block between two underscore runs joined them into one that the parser read in time quadratic in its length.
`create or replace` keeps each trigger bound to its function, so it takes no table lock, and the release before it still calls the functions it knows, whose answers keep their types.
It writes no row.
The two orders give a different text only where text the ask-block pattern matches has an underscore run's character on each side of it, which a real ask block, on a line of its own, never has: only `:::ask{…}:::` written inside a run, in a paragraph or a code block, is indexed differently.
Where the parser reads the joined run as words, as it reads `a_a_…`, both texts give the same vector; where it reads the run as one host name, email address, path or URL, the vector differs in that run's lexemes and in the positions of the words after it, and such a row keeps its vector until its document's next version.
While the release before this one still serves, its related-item check and duplicate check build that text in the old order, so for such a text they compare with a vector built in the other order.
Its census answers `0`.

Migration `0009_project_artifacts` deletes malformed derived artifact references, reports their count, and re-derives them from source text on the next write.
It aborts server boot before a migration record or schema change only when an existing artifact has no owning issue.
On success it backfills each artifact's project and generated `ref_key`.

Migration `20261111210103_projects_default_assignee_not_null` fills every project's `default_assignee` and then makes the column not null, so the last step of the chain that decides who a question reaches — the person the ask was handed to, then the issue's assignee, then the project's default — always lands on a person (INFRA-3090).
`0116_project_default_assignee` added the column nullable and nothing ever filled it, so an ask on an unassigned issue of a project nobody had set a default on reached nobody's Inbox, which is the defect this change ends.
The fill reads two sources and takes the first that names somebody who has signed in:
the login holding most of that project's issues (`issues.project_key`, `issues.assignee`), a tie broken by the most recent issue among the tied logins (`issues.created_at`, then `issues.key`);
then the person who created the project, read from its `project.created` event (`events.actor`, the jsonb of `model.Actor`: the `id` of an actor of kind `user`, else any actor's `owner`).
Both sources are held to `people`, because a login no `people` row holds reaches nobody, which is the defect itself, so a stale assignee is passed over rather than stored.
A project an agent session created with the shared bearer names nobody in its event (`api/projects_test.go`, `TestASessionBearerCreatesAProject`), so it is filled from its issues or not at all.
A project neither source names stops the migration with its key in the message, raised from a `DO` block, since a `select` cannot raise.
It locks `projects` alone, in `ACCESS EXCLUSIVE`, taken before the fill reads anything and held to commit, so no project created between the fill and the `ALTER` escapes with a null default;
`issues`, `events` and `people` are read under `ACCESS SHARE`, which no write waits behind, so the census's one-gigabyte table limit reaches none of them.
Its census counts the projects naming no default, which is every project the fill writes, so it refuses the deploy of a database nobody has filled.
That is the point of it: run the fill on the release still serving, which is the migration's own two statements, and then take the census again, so that whoever deploys types no login at all.

```sql
-- 1. The login holding most of the project's issues, the most recent issue breaking a tie.
with assigned as (
	select i.project_key, i.assignee, i.created_at, i.key
	from issues i
	join people p on p.email = i.assignee
), counted as (
	select project_key, assignee, count(*) as held
	from assigned
	group by project_key, assignee
), majority as (
	select distinct on (a.project_key) a.project_key, a.assignee
	from assigned a
	join counted c on c.project_key = a.project_key and c.assignee = a.assignee
	order by a.project_key, c.held desc, a.created_at desc, a.key desc
)
update projects p
set default_assignee = m.assignee
from majority m
where p.key = m.project_key and p.default_assignee is null;

-- 2. The person who created the project, where the first step named nobody.
update projects p
set default_assignee = creator.login
from (
	select distinct on (e.project_key)
		e.project_key,
		coalesce(
			case when e.actor->>'kind' = 'user' then lower(e.actor->>'id') end,
			lower(e.actor->>'owner')) as login
	from events e
	where e.type = 'project.created' and e.project_key is not null
	order by e.project_key, e.id
) creator
join people person on person.email = creator.login
where p.key = creator.project_key and p.default_assignee is null;
```

A project left with no default after both statements is one to name a person on by hand, `PUT /api/v1/projects/<key>/default-assignee` with `{"assignee": "<the person's email>"}`, which this release still serves.
Reaching the migration with one left stops it, and its refusal names the repair that works from there: nothing is serving by then, so it is `update projects set default_assignee = '<the person''s email>' where key = '<KEY>';` on the database itself.

**`20261111210103` is a stop-the-world migration: deploy it with no server of the earlier release running.**
The narrowing ships in the same release as the fill, where the rule above splits a narrowing across two releases, because the deployment runs one replica under a Kubernetes Deployment's `strategy: Recreate` (`packages/envoy/infra/README.md`):
no server of the earlier release serves while the migration runs, and the release that boots over it names a default on every project it creates.
An older release started over the new schema creates a project with no default, which the column now refuses (`23502`), so that create answers `500`;
recover a failed deploy forward, as `0113` and `0117` require.

A migration runs while the build before it is still serving.
The app Deployment of `envoy-infra:gcp:Dispatch` rolls above one replica (`packages/envoy/infra/README.md`), so from the start of a rollout until the last pod of the previous release is gone
— and again for as long as a rollback lasts — the previous image's queries run against the new schema.
A migration may therefore only add: a table, an index, a trigger, a column that is nullable or defaulted, a constraint added `not valid`.
In the same release as the code that depends on it, it may not drop or rename a column or a table, change a column's type, or tighten a constraint the running build's queries would violate.
Each of those is split across two releases: the first makes the schema accept both shapes, and the second removes the old one once no pod serves it.

The census below does not cover this.
It reports locks, sizes and counts — what a migration would wait behind and how much it would rewrite
— not whether the previous build's queries still answer against the new schema;
that one-release gap is the author's to reason about, and the two-build test (Two builds at once, above) boots the previous release on this tree's schema and drives it.
The five-second `pgmigrate.LockTimeout` also bites harder under a rolling update than under `Recreate`, which stops the previous pods before the new one boots:
while a deployment rolls, the old build's connections are still open and still holding locks on the tables the migration wants, so a migration that applies cleanly against an idle database can fail the new pod's boot.

### Giving a project to another owner

Every project has one owner, the full member who changes its access list and whether it is public or private (`projects.owner`).
An operator gives any project to another full member with the database credential alone, with no server running if need be:

```sh
envoy-dispatch project-owner <KEY> <email>
```

It reads `DATABASE_URL`, as every other subcommand does, and makes the change in one transaction that appends `project.access_changed` with change `owner`, written by the actor `{"kind": "system", "id": "envoy-dispatch project-owner"}` (`access.SetOwner`).
Who owned the project before, and whether that person is still a member, makes no difference.
It exits 0 once the change commits;
2 for a key no project holds, for an email that is not a full member's (a restricted member, or a login nobody has signed in with), and for arguments that are not exactly a key and an email, writing nothing;
and 1 when the database cannot be opened or written.
No write clears an owner: an update that would is refused with `check_violation` (`23514`, the trigger `projects_owner_kept`), and so is an owner that is empty or not lowercase (the column's check).
Giving a project to the person who owns it already writes nothing, and the command says so.

Migration `20261116015736_project_access` gives each project its default assignee as owner where that person has signed in.
An insert that names no owner, every project create included, takes its default assignee as owner (`projects_owner_from_default`).
A project whose default assignee had never signed in when the migration ran would keep no owner, so the migration's census counts such projects, and a count above `0` refuses the deploy.
Repair them before it, on the release still serving: give each a default assignee who has signed in with `PUT /api/v1/projects/{key}/default-assignee`, then deploy again.
They are listed there by the query `20261116015736_project_access.census.sql`'s header gives:

```sql
select key, default_assignee from projects p
where not exists (select 1 from people where email = p.default_assignee);
```

Should a project still have no owner at boot, the boot refuses to serve while any project has none, naming each one by its key, and both repairs, and no person's email, since it writes the refusal to its log (`store.EnsureProjectOwners`, run at boot, before the server serves):
set `DISPATCH_PROJECT_OWNER_FALLBACK` to the lowercase email of a full member who has signed in, and the next boot makes that person the owner of every such project, each grant recorded as the command's is (`project.access_changed`, written by the actor `{"kind": "system", "id": "DISPATCH_PROJECT_OWNER_FALLBACK"}`);
or give each one an owner with the command above.

### Pre-deploy census

`envoy-dispatch census` is run before a new image goes out, with the service's `DATABASE_URL` and under the service's own database credentials, and it is a gate:
a census that refuses stops the deploy before the service moves, and nothing accepts a refusal and rolls past it.
The `envoy-infra:gcp:Dispatch` component registers no census task, so a deployment runs it itself against its database before it updates the image:
as a Kubernetes Job from the new image, or by hand as the one-off pod in `packages/envoy/infra/README.md` ("The pods"), which runs `census` and `versions` alike.

```bash
DATABASE_URL=postgres://... envoy-dispatch census
```

The census is one `repeatable read, read only` transaction over the migrations this binary carries that the database has not applied:
every migration whose version `schema_migrations` does not record, the rule the runner applies them by (a database without that table records none, so everything is pending).
For each it prints the tables the migration locks above ACCESS SHARE (`pgmigrate.CensusTables`), with each table's total size, its row count and the sessions holding locks on it;
the transactions open longer than a minute anywhere in the database, and those whose age it cannot see (another role's without `pg_read_all_stats`, any with `track_activities` off, or a WAL sender's), which it tells from a session in no transaction by the virtual transaction id every transaction locks;
and the count the migration's own census answers.
Postgres reads `pg_stat_activity` once per transaction and serves that copy until the transaction ends, while `pg_locks` is read anew each time, so before each read of lock holders and of long transactions the census discards that copy (`pg_stat_clear_snapshot()`), and it measures a transaction's age at that read (`clock_timestamp()`), not at its own start.
The transaction gives `session_authorization`, `role` and `search_path` the values that `reset` gives them, as the runner does for each migration ("Database migrations" above), so a name resolves for the census as it resolves for the runner.
Those tables are:

- the ones its statements name (`pgmigrate.TouchedTables`:
  the targets of `alter table`, `create index … on`, `drop table`, `truncate`, `create trigger … on`, `drop trigger … on`, `update` with or without an alias, `delete from`, `insert into`, `lock table`, and a foreign key's `references`), read as Postgres's lexer reads the migration:
  not in its comments, its string literals or the body of a function it defines, and in a `DO` block's body, which it runs;

- the table behind each index it drops or alters;

- every table a foreign key reaches from a table whose rows it writes, or from a table it drops, read from `pg_constraint` when the census is taken, and from the `references` an earlier pending migration adds, which applies first at boot (in a `create table` or `alter table`, inside a `DO` block too, behind an `if not exists` over `pg_constraint` or not):
  a row inserted or updated (an upsert's `do update` included) is checked against the table its key references, a dropped table's own foreign keys go with it and take ACCESS EXCLUSIVE on each table they reference, and a row deleted, or one whose key an update changes, is checked against every table that references it or cascades into it, and on from there.
  The report prints such a table as `touches <table>
  through a foreign key with <table>`, and checks its holders and that it can be read, but neither counts its rows nor holds it to the size limit:
  the migration locks it to check or act on the rows that key connects, or, for a drop, only for the catalog change, reading no row.

A table a statement only reads (`insert … select from`, `create view … as`) is not listed: its ACCESS SHARE waits only behind an ACCESS EXCLUSIVE holder.
Each store's tests apply every migration in its declared execution mode and require that reading to be the tables a transactional migration locks above ACCESS SHARE (`pgmigratetest.CheckTouchedTablesAgainstLocks`); a concurrent build releases its table locks before the command-end event, so its actual index target is checked in the catalog there instead.
A migration written in a form the patterns do not know (`reindex`, `cluster`, `create policy … on`, `merge into`) fails the audit and needs a pattern.
They apply the real set to an empty database, where a foreign key locks nothing, since it locks only the rows it checks or cascades into;
the audit's own tests seed rows to hold the foreign-key reading to the locks it predicts.
A table only a `DO` block's body names is not held to the other half, a table read that the migration never locks:
the block can branch on rows the empty database does not hold (`if exists (select 1 from things) then update others …`).
The census checks such a table all the same.
A fresh database, one that records no version and holds no table, has no row a migration could refuse and no session to wait on:
the census prints `census: fresh database, nothing to check` and passes, so a new stack's first deploy takes the census like every later one.
It refuses:

- a migration whose census answers non-zero, naming the migration, the count and the census file;

- a table a migration names above 1 GiB (`pgmigrate.CensusTableLimit`, table, indexes and TOAST), whose rows it then does not count:
  a migration that locks a table that size needs another shape (`CONCURRENTLY`, batches).
  A migration is exempt when the census admits every one of its statements, as Postgres's own parser reads them (libpg_query, run in pure Go by `github.com/wasilibs/go-pgquery`).
  The census admits a statement of the kinds below (`catalogOnlyStatements` in `internal/pgmigrate/exempt.go`), under the rules of its item:
  each changes only the catalog or the transaction's own settings, or takes a lock, and reads no existing row of the table, so the time it holds its lock does not grow with the table.

  - `create function` (or procedure) in `plpgsql` or `sql`, or with an SQL-standard body (`begin atomic`, `return`), which is `sql`:
    Postgres writes `pg_proc`, and the language's validator checks the body without reading a row.
    The census does not admit a function in another language, since creating a C function loads its library, which can run any code.

  - `create trigger`, of any form:
    Postgres writes `pg_trigger`, and the trigger runs only on later writes, never on the rows already there.

  - `drop trigger`, `drop function`, `drop index` and `drop table`, with `restrict`, which is the default:
    Postgres deletes the catalog rows of the object, and the files of a dropped index or table, and it reads no row.
    The census does not admit a drop with `cascade`, since what it drops besides the object it names is not in the statement the census reads.
    It does not admit a drop of anything else (a view, a schema, a procedure) either.

  - `set` and `reset` of a run-time parameter, local or session, `set role` included:
    a setting takes no lock, reads no row and names no table, so a migration that bounds its own lock wait with `set local lock_timeout` keeps the exemption its other statements earn.
    Only a `set` or `reset` of `lock_timeout` or `statement_timeout` keeps the catalog reads on.
    Every other `set` or `reset`, and a `reset all`, stops them (below): a `set local` for its own migration alone, and any other for its own migration and every later one.

  - `alter table`, of a table, when the census admits each of its subcommands (`catalogOnlyAlterTable`):
    `drop constraint` and `drop column` without `cascade`, `add constraint … check … not valid`, `add constraint … foreign key … not valid`, `add column` under the rules of the next item, `alter column … set default` of any expression, `alter column … drop default`, `alter column … drop not null` and `alter column … drop expression`.
    None of them reads an existing row.
    Postgres deletes the catalog rows of a dropped constraint, and its index with them.
    It marks a dropped column as dropped in `pg_attribute`, and each row keeps the column's bytes until the next write of that row.
    It tests a `not valid` constraint on no existing row, of the table or of the table a foreign key references.
    `set default` and `drop default` change only what later inserts write, `drop not null` checks no row, and `drop expression` keeps the values a stored generated column holds.
    The census admits no other subcommand: `set not null` scans the table for nulls, `alter column … type` rewrites it, and `validate constraint` and a validated `add constraint` scan it.
    It does not admit `alter view`, `alter materialized view`, `alter index`, `alter sequence`, `alter foreign table` or `alter type … add attribute`, which Postgres parses as an `alter table` too.
    Each admitted subcommand still takes ACCESS EXCLUSIVE briefly under the runner's `lock_timeout`: the exemption removes a size refusal, not the lock bound.

  - `add column`, in such an `alter table`, of a column whose types and default the census admits (`catalogOnlyColumn`):
    Postgres then writes only the catalog, whatever the type, and stores a non-null default as the value every existing row reads.
    The column may carry `collate`, `storage` and `compression`, since each changes only what the catalog says of the column.
    The census judges every type the statement names, the column's type and the type its default casts to, by the rules below (`admittedType`).
    It admits a base type, an enum, a range, a composite type made with `create type … as`, and an array of one of these.

    - The type name must have one or two parts, and the type must resolve in the database the census reads.
      The census reads the catalog before any pending migration applies, so a type or a function that an `add column` uses must ship in an earlier release than the column.
      The census does not find one that an earlier pending migration creates, and then it does not admit the column.

    - A type name of one part must not be one of the six serial names (`smallserial`, `serial2`, `serial`, `serial4`, `bigserial`, `serial8`), in the column or in a cast, whatever type of that name exists.
      Postgres gives a column of such a name a sequence and a `nextval` default, which rewrites the table.
      Postgres resolves the name in a cast as it resolves any other type name, so the rule refuses more casts than it must, and no default a migration ships casts to one.
      The census also refuses an array of one of these names, in the column or in a cast.

    - The type must not be a domain, with constraints or without.
      Postgres rewrites the table for a domain with constraints, and the census reads only the type's kind (`typtype`) to refuse every domain.

    - The type must not be the row type of a table, a view or another relation.
      A pending `drop table` or `drop view` does not stop the catalog reads, and after it the same name can resolve to a type in a later schema of the `search_path`, which the census did not read.
      That type can be a domain with a constraint, or a type whose cast from the default calls a volatile function.

    - When the type is an array, its element type must not be a domain or the row type of a relation either.
      The drop can change what the element type resolves to in the same way, and the census refuses an array of a domain as it refuses the domain.

    A default, when the column has one, must be one of these four kinds:

    - a constant (a number, a string, a bit string, a boolean or `null`), or an `array[…]` of constants that holds no other array.

    - a cast of one of those to a type that the census admits by the rules above, such as `'x'::text` or `'x'::varchar(5)`.

    - a SQL value function, such as `current_timestamp`, `current_date` or `current_user`.

    - a call with no argument, written `f()` or `pg_catalog.f()`, of a `pg_catalog` function that is no aggregate, no window function and no set-returning function, and that the catalog marks stable or immutable, such as `now()` (`admittedCall`).
      The census judges `pg_catalog.f()` by the function in `pg_catalog` alone, since Postgres searches only that schema for a qualified name.
      It admits `f()` only when `pg_catalog` is the first schema of the census's `search_path` other than the temporary schema, and no function outside `pg_catalog` has the name, in any schema.
      Postgres's call resolution also takes a function whose arguments all have defaults, and of two candidates in different schemas the one in the earlier schema of the `search_path` wins.
      So `s1.now(a int default 0)` takes the call `now()` when `s1` comes before `pg_catalog`, and a pending `create function` can create such a function, which the census does not see.
      With `pg_catalog` first, the function of `pg_catalog` wins over the function of every other schema, a function that a pending migration creates included.
      Postgres never searches the temporary schema for a function.
      The rule about the name also covers a runner whose `search_path` is not the census's, for every function that exists when the census runs.

    The census admits no other default.
    A volatile function (`gen_random_uuid()`, `clock_timestamp()`) makes Postgres rewrite the table to give each row a value of its own, and a function of another schema can read any table while Postgres holds ACCESS EXCLUSIVE.
    The census does not admit an operator (`1 + 1`), a column reference, a subquery, or a cast of a cast or of a function either.
    Postgres can put a cast of its own between the default and the column (`int8(int4)` puts `0` into a `bigint`), and a volatile cast function makes it rewrite the table.
    So the census does not admit a column with a default when a cast to the column's type, to the default's cast type or to the element type of either calls a volatile function, which no cast of a stock PostgreSQL 18 does.
    The census admits `not null` only beside an admitted default that is not a null constant, cast or not, since Postgres scans the table for nulls when the default gives the column no value.
    A `pg_catalog` function that returns null at run time (`inet_client_addr()`) passes the census, and Postgres then fails on the first row and the migration rolls back: a failed deploy, not a long lock.
    The census admits `references`, with its `deferrable` and `initially` clauses, only on a column with no `default` clause at all, not even `default null`, since Postgres checks the key against every row when the column has one.
    The census admits no other column constraint and no explicit `null`: `check` scans the table, `unique` and `primary key` build an index over its rows, and an identity or a stored generated column rewrites it.

  - `rename column` (`alter table … rename column`), of a table:
    Postgres rewrites the column's `pg_attribute` row, and no row of the table holds a column's name.
    The census admits no other rename, a table, an index or a constraint rename among them.
    It reads every pending migration against the database as it stands before any of them applies, and it finds a table or an index by its name.
    So when a later pending migration names a renamed table or index, the census finds nothing under the new name and reads neither its size nor its lock holders: the later migration escapes the census.
    A constraint rename counts, because Postgres renames the index of a primary key, a unique constraint or an exclusion constraint with it.

  - `create table`, without `inherits`, `partition of` or `as`:
    the new table holds no row, so Postgres reads no existing row to create it with its columns, its constraints, its `like` clauses and its references to existing tables.
    Postgres checks no foreign key of the new table, and it locks the table a key references without reading it.
    `inherits` and `partition of` join the new table to an existing one, so the census does not admit them.
    `create table … as`, `select … into` and `create materialized view` fill a table with the rows their query reads, and the census does not admit them either.

  - `create index`, on a table that an earlier `create table` of the same migration creates without `if not exists`, named exactly as that `create table` names it, when that name resolves to no relation in the database the census reads:
    the build reads the rows of that table, and the new table has none.
    The name must resolve to no relation before the migration, so that it can resolve to nothing but the new table.
    The census does not admit an index on any other table, nor one on the table of a `create table if not exists`, since the build reads the rows of a table that already exists.
    `create index concurrently` has an exception of its own (below).

  - `create view` and `create or replace view`:
    Postgres writes the view's catalog rows and analyzes its query, which reads no row of the tables it names.
    The query runs only when a session reads the view.

  - `lock table`, in any mode:
    it takes a lock and reads no row.
    The lock lasts to the end of the migration, and the census must admit every other statement of the migration too, so the time the migration holds the lock does not grow with the table either.
    The runner's `lock_timeout` bounds its wait, as it bounds the wait of every statement.

  The census reads the catalog of the database it judges, in its read-only transaction, for an added column's type, for the cast and the function of its default, and for whether the table of a created index is new.
  No other kind needs a catalog read to be admitted.
  The census reads no catalog to admit a statement of the migrations below, so it admits none of their added columns, `text` included, and none of their created indexes:

  - a migration that sets or resets any setting other than `lock_timeout` and `statement_timeout`, `set local` included, or runs `reset all`.
    A setting can make the migration's names resolve to other objects than the census's names do, as `search_path`, `role` and `session_authorization` can, since the default `search_path` starts with `"$user"`.
    A setting can also change how Postgres reads a migration's text, as `standard_conforming_strings` can.
    The runner sets that setting and two others for each migration, and it gives `search_path`, `role` and `session_authorization` their `reset` values ("Database migrations" above), but the census cannot list every setting of either kind.
    So the census lists the two settings that it knows to do neither: `lock_timeout` and `statement_timeout`, which bound how long a statement waits or runs.
    The census compares the setting's name without regard to case, as Postgres does, so `set "Search_Path" = public` stops the reads and `set "Lock_Timeout" = '1s'` does not.

  - every migration after a pending migration that sets or resets such a setting for the session, without `local`, or runs `reset all`.
    Such a setting stays on the runner's connection after its migration's transaction ends, until the runner resets the connection's session before the connection goes back to its pool ("Database migrations" above).
    So no later migration meets the setting.
    The census still turns off the catalog reads of the later migrations, which only refuses more.
    At the start of each migration the runner sets `lock_timeout`, the three text settings and the three settings that resolve names, and no other setting.
    The census tells `lock_timeout` and `statement_timeout` from the other settings, because those two keep the reads on.
    It tells none of the other six settings that the runner sets from the rest, which only refuses more.
    A `set local` ends with its migration's transaction, so the later migrations still read the catalog.

  - a migration that holds a statement that is not on the list below, and every migration after it.
    The census reads the catalog as it stands before any pending migration applies.
    So the catalog that a later migration meets is the catalog the census reads only while each earlier statement leaves type and function names as they are.
    The list holds the statements that this repository's migrations write and that are known to leave these names as they are:

    - `insert`, `update`, `delete` and `select`, with a `with` or without, but not `select … into`, which creates a table and fills it as `create table … as` does.

    - `lock table`, `create trigger` and `create index`.

    - `create table` and `create view`, with or without `or replace`, when `pg_catalog` holds no type of the same name.
      The new row type can hide a type of the same name in a later schema of the `search_path`.
      But no cast targets the new row type, and it is not a domain, so Postgres adds a column of that type in the catalog alone.
      A `create table int4` stops the reads all the same: the census reads a later column of type `int4` as the `pg_catalog` type, and under a `search_path` that lists `pg_catalog` after the new table's schema, Postgres reads it as the new row type.

    - `alter table` of a table, and `alter index` other than a rename, when no subcommand has `cascade`.
      A `drop column … cascade` or a `drop constraint … cascade` can drop objects that the statement does not name.

    - `rename column` of a table, which renames no type.

    - `drop index`, `drop trigger`, `drop function`, `drop table` and `drop view`, with `restrict`, which is the default.
      Without `cascade`, Postgres does not drop a function that a cast calls, or a table or a view whose row type a cast or a function names.

    - `create function` and `create procedure` in `plpgsql` or `sql`, with or without `or replace`, when `pg_catalog` holds no function of the same name and no cast calls a function of that name.
      The census matches the name in any schema.
      The census admits a default that calls a `pg_catalog` function such as `now()`, and it reads that function in the catalog as the catalog is before the pending migrations.
      A function of the same name that a pending migration creates in another schema can take the call.
      A `create or replace function pg_catalog.now()` can make `now()` volatile, and on PostgreSQL 18.6 Postgres then rewrote the table for a later added column whose default calls `now()`.
      A volatile cast function makes Postgres rewrite the table for an added column with a default.

    - `set` and `reset` of `lock_timeout` and `statement_timeout`.
      A `set` or `reset` of any other setting stops the reads as the two items above say, so a `set local` stops them for its own migration alone.

    A statement on the list stops the reads all the same when it names a relation of `pg_catalog`, or a relation whose rules read one, or when it calls `set_config`.
    The census reads the whole statement for these: a `from`, a `join` or a `with`, the table that an `insert`, `update`, `delete` or `merge` writes, the table of a `lock`, a `create index` or a `create trigger`, a `references`, a `like` or an `inherits`, an expression such as a column's default, and an SQL-standard function body.
    The census counts a view over a `pg_catalog` relation, such as a view over `pg_proc` or a view of `information_schema`, and a table with a rule that reads one, as a relation of `pg_catalog`, through views over views to any depth.
    A write to `pg_proc` or `pg_cast`, by a superuser or through such a view, can make a cast's function volatile or add a cast, and an update of `pg_settings` changes a setting.
    The census reads the catalog as it was before the write, and it does not tell a read of such a relation from a write.
    A statement that names such a relation also takes the exemption from every later migration (below).
    A call of `set_config` changes a setting as a `set` does, `search_path` included.
    A view's query or a rule that calls `set_config`, of a relation that the statement names, stops the reads too, through views over views to any depth.
    A `select` of a view runs the view's query, and an `insert`, `update` or `delete` runs the rules of its table.
    The census counts every rule of such a relation, whatever the statement does, which only refuses more.

    Every other statement stops the reads, for its own migration and for every later one, and it also takes the exemption from every later migration (below).
    A `do` block, a `call`, an `alter function`, an `alter role … set` and an `alter database … set` stop them.
    So do `create table … as`, `select … into`, `create materialized view`, a drop with `cascade` and a function in another language.
    So do `create event trigger` and `alter event trigger`.
    So does every statement that creates, alters, renames or drops a type, a domain, a cast, a schema, an extension or a role.
    A rename of a table, a view, a materialized view or a foreign table stops the reads for every later migration, because the relation's row type keeps the casts that target it under its new name.
    After such a rename, a later migration's type name can resolve to that row type in an earlier schema of the `search_path`, through a cast that the census did not read.
    `create table`, `create view`, `drop table` and `drop view` do not stop the reads, because they are on the list.
    The census lists the statements that keep the reads on, and not the statements that stop them.
    A statement that a list of the second kind misses admits a column that Postgres rewrites the table for.
    A statement that this list misses only costs the later migrations their catalog reads and their exemption, which is a refusal.

  - every migration after a pending migration that does not parse in the census's Postgres 17 grammar, since the census cannot see what such a migration changes.
    A migration can parse on the PostgreSQL 18 server and not in the census (a `virtual` generated column).
    Such a migration is not exempt itself either, and no later migration is exempt (below).

  The census reads every statement that a pending migration's text shows, whoever may run it, a superuser included.
  It also reads the query of every view and every rule of a relation that a statement names, for a relation of `pg_catalog` and for a call of `set_config`.
  It does not read the body of a function, whether a statement, a view's query or a rule calls it, and it does not read a trigger's body.
  Nor does it read the code of a column default, a check or a domain, which changes no later migration's names, and a later statement's only through a cast function ("Database migrations" above).
  The limit is the rest of what stored code does, such as create a type or enable an event trigger.

  A marked single non-unique `CREATE INDEX CONCURRENTLY IF NOT EXISTS` has a separate size exception: its scans do not hold off ordinary writes, not because the build is catalog-only.
  The census still reports its table size, skips counting rows above the limit, and checks its holders and census as usual.
  Neither exception applies while an enabled event trigger on `ddl_command_start`, `ddl_command_end` or `sql_drop`, which every drop fires, carries no command tags or tags naming `CREATE FUNCTION`, `CREATE PROCEDURE`, `CREATE TRIGGER`, `DROP FUNCTION`, `DROP TRIGGER`, `DROP INDEX`, `ALTER TABLE`, `CREATE INDEX`, `CREATE TABLE`, `CREATE VIEW` or `DROP TABLE`, since it runs code of its own, and the size refusal then names it.
  A `rename column` fires its event triggers under the tag `ALTER TABLE`, and `lock table`, `set` and `reset` fire no event trigger.
  A trigger whose tags name none of those commands counts for nothing.
  The census reads those event triggers once for the whole database, so an enabled one that counts takes the exemption from every migration, a migration holding no drop included, which is the side that refuses rather than the side that applies.
  A pending migration takes both exceptions from every later pending migration, and the size refusal names that migration and says why, when it holds a statement that can create or enable an event trigger that the census does not see, or when it does not parse:

  - `create event trigger` and `alter event trigger`, whatever the trigger's tags.

  - a statement that names a relation of `pg_catalog`, or a relation whose rules read one, as the list of statements that keep the catalog reads on counts them (above), whether the statement is on that list or not.
    An `update pg_catalog.pg_event_trigger set evtenabled = 'O'` enables a disabled event trigger, as `alter event trigger … enable` does, and an `update` of a view over `pg_event_trigger` does the same.
    For each relation name that a statement gives, the census reads every relation of that name in every schema, whatever schema the name gives and whatever the `search_path` is.
    It makes this read in its own transaction, whether or not it reads the catalog for the migration.
    A pending `set` of `search_path`, a `drop table`, a `drop view`, a rename or a `set schema` can change which relation the name finds, and the census reads the catalog before any of them applies.
    So a statement that names a table counts when a view over a `pg_catalog` relation in another schema has the table's name, as the view `information_schema.columns` has the name `columns`.
    A name that no relation has does not count, because the statement that creates the relation is judged itself.
    A statement on the list that stops the reads only because it creates a function, a table or a view whose name `pg_catalog` holds, or because it calls `set_config` itself or through a view or a rule, takes no exception away.
    A function that such a statement creates or replaces runs only when a statement calls it.

  - every statement of a kind that is not on the list of statements that keep the catalog reads on, such as a `do` block, a `call`, a `create extension`, an `alter extension` or a `create table … as`.
    A `do` block and a `call` run code that the census does not read.
    The script of an extension can create an event trigger, as the scripts of pg_graphql and Citus do.
    A `set` or a `reset` of any setting takes no exception away: a setting creates and enables no event trigger, and the census counts every event trigger that is not disabled.

  - a migration that does not parse, since the census cannot see whether it creates an event trigger.

  The report still prints the table's size beside the limit and says why the migration is exempt, and its lock holders refuse it as they refuse any other.
  A trigger named only in a comment, a string literal or a function body exempts nothing, and one statement of any other kind beside it ends the exemption.

  - A table above the limit is also admitted, table by table, for bounded writes (`boundedWrites` in `internal/pgmigrate/bounded_writes.go`), when every statement of the migration that touches it, as the census reads touches, is one of these, as Postgres's own parser reads it:

    - an `insert` into the table of a `values` list of at most 1,000 rows (`pgmigrate.BoundedWriteRows`), each value a literal, with no `with`, `returning` or `overriding`, and with no `on conflict` or with `on conflict … do nothing`, under any conflict target or none.
      A conflict target only names the index Postgres checks, and Postgres evaluates none of it on a row, so a target of index expressions with a predicate (`on conflict (from_kind, from_id, kind, md5(to_id), source_event, asserted) where to_kind = 'value' and source_event <> 0 do nothing`) counts as no target does.

    - an `update` of the table, under its name or an alias, with no `with`, `from` or `returning`, that sets columns to literals and names at most 1,000 rows by its primary key, in a `where` clause of `pk = v`, `pk in (v, …)`, `(pk1, pk2) in ((v, v), …)` or `pk1 = v and pk2 = v`, which names each column of the key once and nothing else.
      It sets no column of the primary key and no column a foreign key references, since a change of such a key checks the table that references it.
      Each key value is a string constant or `null`, which takes the column's type, an integer constant for a `smallint`, `integer` or `bigint` column, or a cast to the column's own type: a comparison Postgres searches the key's index by, where `seq = 1.5` casts a `bigint` column to `numeric` and scans the table.

  - A literal is what Postgres stores as an added column's default without a rewrite, and none of them reads a table: a constant (a negative number and `null` among them), an `array[…]` of constants, a cast of one of those to a type the census admits for a column (`'{"a":1}'::jsonb`), a SQL value function (`current_timestamp`), or a call of no argument of a stable or immutable `pg_catalog` function under the rules for a default (`now()`).
    A cast and a call need the catalog reads, so a migration without them has only the others.
    An operator, a subquery, a call with arguments (`jsonb_build_object(…)`), a function outside `pg_catalog` and a column reference are not literals, and neither is `default`, since the census does not read the default it stands for.
    Such a write takes ROW EXCLUSIVE, which holds off no read and no write of another row, and reads and writes only the rows it names, so its time grows with its rows and not with the table:
    on this pod's PostgreSQL 18.6, over `graph_tuples` at 3,000,000 rows and 1.2 GiB, an insert of 1,000 literal rows took 63 to 194 ms and an update of 1,000 rows by `seq` 170 to 344 ms, where 10,000 took 1.0 s and 2.9 s (`pgmigrate.BoundedWriteRows` holds the measurement).

  - The table is held to more, since each of these makes a write of one row able to read the whole table:
    no rule (`pg_rewrite`), no row-level security (`relrowsecurity`), and, for an `update` without `only`, no child table.
    The census reads the table's keys, rules and triggers as they stand before any pending migration applies, so no earlier pending migration may touch the table with more than bounded writes, and neither the migration nor an earlier pending one may hold a statement whose effect the census cannot read, the ones that take the exceptions above from every later migration.
    Nor may the migration hold a statement after which its names can resolve to another table, a `set` of any setting but `lock_timeout` and `statement_timeout` among them.
    Three kinds of earlier pending migration do not count as touching the table with more than bounded writes, since none changes what the census judges those writes by:
    a single non-unique `create index concurrently` of the table;
    a migration exempt above as catalog-only that only adds columns to the table, each nullable with no default and no constraint (`alter table t add column c text`);
    and a migration exempt above as catalog-only that names the table only as the table its foreign keys reference, each by columns of the table's primary key (`alter table other add constraint other_t foreign key (t_id) references t (id) not valid`).
    An index adds no key, rule, row-level security, child table or trigger, a column with nothing on it is in no key and no foreign key references it, an update may not set a primary-key column anyway, and the triggers Postgres makes for a foreign key are internal ones, which the census leaves out of the table's triggers.
    A column with a default still counts, since an insert that leaves the column out runs its default for each row, and so does a key to a column outside the primary key, since an update may set such a column.
    The census reads the primary key in the catalog, so a migration that has no catalog reads still counts for every table its keys reference.
    An enabled trigger of the table, and the default of a column an insert leaves out, run for each row written, and the census reads neither, as it reads no function's body.
    A column default does not refuse, but an enabled trigger can read any table, so the admission is refused while the table has one that is not on the census's reviewed list (`reviewedTriggers`), whose entry says why its body is bounded; an admitted table's report prints the number of statements, the bound and the table's enabled triggers.
    The list holds `graph_nodes`' two triggers, `graph_nodes_block_schema_written` and `graph_nodes_block_schema_removed` (migration `20261109051232`): each runs `block_schema_changed`, which reads no table and sends one `dispatch_block_schema` notification, one per transaction however many rows it writes.
    An enabled event trigger does not take the admission away either, since an `insert` and an `update` fire none.
    A migration that writes rows of such a table any other way is refused with what is not bounded (`…, and lock table is not one`, `…, and an insert of 1,001 rows, above the 1,000 a bounded write may hold, is not one`), and one that writes none of its rows and is not exempt above is refused on the size alone;

- a session holding a lock on a touched table whose transaction has been open at least a minute (`pgmigrate.CensusLongTransaction`):
  the migration would give up on that lock during the rollout;

- a session holding a lock on a touched table whose transaction's age the census cannot see.
  `pg_stat_activity` shows another role's `xact_start` and `state` only to a superuser, a member of that role or a member of `pg_read_all_stats`, so without that grant every holder of another role (an operator's session, a dump) refuses, since the census cannot tell it is young, and the refusal names the grant;
  granting the census's role `pg_read_all_stats` lets it read the age of a holder of another role, and then it judges such a holder as it judges one of its own role, and the refusal never names the grant for a holder of the census's own role.
  A session with `track_activities` off shows its state as `disabled` and no `xact_start` to every role, so it refuses too, and the refusal says so.
  Postgres never shows a WAL sender's `xact_start`, to any role, and a `replication=database` connection runs SQL, so it can hold a lock in a transaction of any age:
  a holder that shows no `xact_start` and a state other than `idle` refuses, of the census's own role too, and the refusal names its state.
  Only an `idle` holder with no `xact_start` passes:
  `pg_stat_activity` read it between transactions, in the query that read its lock, so the transaction holding that lock began or ended within that query.
  A session caught at a statement's start or end reads `active` with no `xact_start` and refuses too, spuriously and only at that instant, so the census taken again passes it;

- an autovacuum worker holding a lock on a touched table, only when Postgres will not cancel it for the migration.
  Postgres cancels an autovacuum that holds a lock another session waits for once that session has waited `deadlock_timeout`, so an autovacuum passes at any age, with the grant or without (to a role without it, autovacuum is the holder that runs as no role), except an anti-wraparound vacuum, which Postgres does not cancel, or one on a server whose `deadlock_timeout` is not shorter than a lock timeout the migration waits under:
  the runner's five seconds, or the one its own `SET LOCAL lock_timeout` sets, which Postgres reads in a savepoint of the census's (a value it refuses there refuses the migration).
  `0066` sets 500 ms, so an autovacuum on `asks` or `comments` refuses it at the default one second, and `20261101182522_search_term_queue_triggers` sets the same, so an autovacuum on `asks`, `comments`, `issues` or `messages` refuses it.
  Postgres marks a vacuum anti-wraparound when it launches it, in its activity, which only a role with `pg_read_all_stats` can read, and which Postgres records only with `track_activities` on;
  where the census cannot read it, it refuses every vacuum of a table past its freeze age (`age(relfrozenxid)` or the multixact age at its `autovacuum_freeze_max_age`), one launched before the table passed it included;

- a touched table the census cannot read within five seconds (`pgmigrate.LockTimeout`), because another session holds a lock every read waits behind, or within the statement timeout (a minute, `pgmigrate.CensusStatementTimeout`), naming the holders;

- a census that fails or answers anything but one integer in one row;

- a database that records no version yet holds tables, on its first migration: the runner would apply every migration from `0001` over them.

Every refusal a lock holder causes (an old transaction, one whose age the census cannot see, an autovacuum Postgres will not cancel, a table it could not read) carries each such holder's diagnosis, read when the census refused, since the holder has usually gone by the time anyone looks:
its state, transaction start, `backend_xid`, wait event and query from `pg_stat_activity`, and every lock it holds or waits for from `pg_locks`.
Its query is printed with every literal, number and comment replaced (read with `standard_conforming_strings` on), so the values a query carries in those do not reach the report;
what Postgres hides from the census's role prints as `not visible`.
A diagnosis that cannot be read says so with its SQLSTATE, and the refusal stands as it is.
To read the same by hand for a session `<pid>`:

```sql
select a.state, a.xact_start, a.backend_xid::text, a.wait_event_type, a.wait_event, a.query,
	l.locktype, l.mode, l.granted, l.relation::regclass::text
from pg_stat_activity a left join pg_locks l on l.pid = a.pid
where a.pid = <pid>
order by l.granted desc, l.locktype, l.relation::regclass::text, l.mode;
```

So a migration that changes values readers already had, whose census counts the rows it rewrites, cannot pass the census wherever such a row exists, and a release that must repair existing rows does so in code instead:
an idempotent pass at boot, and on the path that reads the rows, so a task of the earlier release still running during the roll cannot undo it.
The restoration of decided asks' wordings (`docs.Service.RestoreDecidedWordings`, Database migrations above) is one.
The repair of closed and reopened issues' asks (`api.RepairIssueAsks`, Database migrations above) is the exception:
it repairs at boot and then every 10 minutes (`api.IssueAskRepairInterval`), never on a read, because those rows are read by every issue, ask, Inbox and count route, and a read that repaired them would put a close's document writes on all of them.
During a roll a close or reopen by a process of the earlier release stays unconverged for at most one interval and one pass, and the Inbox, `GET /api/v1/asks/open` and the counts keep leaving out the open asks of a closed issue to cover that window.

A migration declares its census in `<version>_<name>.census.sql` beside it:
one `select` (or `with … select`) answering one integer, how many existing rows the migration would refuse (a validating constraint's violators) or rewrite.
Every migration from `censusRequiredFrom` (`internal/dispatch/store/store_test.go`, 56) on declares one, which from now on is every migration added:
a timestamped version is above every number.
one that cannot refuse or rewrite a row says so with `select 0` and a comment (`0054_message_delivery_acceptance.census.sql`;
`0053_asks_approval_kind_check.census.sql`, the check's own predicate negated, is the other shape).
A census is written against the schema just before its migration, and the store's tests run every shipped census at exactly that schema and require it to answer (`TestEveryShippedCensusAnswersAtTheSchemaBeforeItsMigration`).
A deployment takes every census before any migration of its release applies, so a census names only what exists before its release.
One that names a table or column the database does not have refuses, even behind an earlier pending migration that may be what creates it:
the census cannot tell that from a typo without applying that migration, which takes the very locks it measures.
A release whose census reads what an earlier migration of the same release creates, renames or gives a new type therefore refuses every deploy, until a release carrying the earlier migration without the later one deploys first.
The store's tests find such a pair before it ships (`TestEveryShippedCensusNamesTheNewMigrationsItReads`, `pgmigratetest.CheckCensusNamesTheMigrationsItReads`):
a census that reads a table, column, function or type a migration from `censusRequiredFrom` on creates, renames or, for a column, gives a new type names that migration in a comment saying it ships in an earlier release.
A rename or a new type keeps the catalog row, so the check compares every object's name, and every column's type, before and after each migration.
Where releases split is not in the repository, so the comment is the author's word;
when both would ship together, split the release, or write the census without what the earlier one makes, which for a new table or column holds no row at deploy (`select 0` with a comment).
The runner never runs a census.
Exit 0 passes, 1 refuses (every reason is in the report), 2 the census could not be taken (no `DATABASE_URL`, no connection, a `search_path` naming no schema that exists, where `current_schema()` is null and the runner can create nothing, a migration set `pgmigrate.Load` refuses).

**A census is production-executed code, reviewed like the migration beside it.**
It runs as the service's own database role.
The read-only transaction stops every write, and the census's statement is sent through the extended protocol whatever the connection string asks for, so it cannot carry a second statement (a `commit` of its own, then a write).
Read-only does not stop `pg_terminate_backend` (which, as that role, ends the service's own sessions), `pg_cancel_backend`, advisory locks (`pg_try_advisory_*` included) or `pg_sleep`, nor a function that runs a query given as text (`query_to_xml` and its kin, `ts_stat`, `ts_rewrite`):
`pgmigrate.Load` refuses a census naming any of them, bare or quoted and in any case, anywhere outside its comments and string literals, and refuses a Unicode escape (`U&"…"`, `U&'…'`) outright, since one can spell any name and no census needs it.
`Load` finds the comments and literals as Postgres 18's lexer does (nested comments, `E'…'` escapes, continued and dollar-quoted literals), and the census runs with `standard_conforming_strings` on, `backslash_quote` at `safe_encoding` and `client_encoding` at `UTF8`, the settings the runner applies each migration with, so Postgres reads its literals the same way.
Review is the control past that, since any non-zero integer a census answers is printed.

**What the report prints:** counts, sizes (in KiB, MiB and GiB, or in bytes where a size above the limit would round to the limit's figure), versions, file names, the names of the enabled triggers of a table admitted for bounded writes, and session metadata (pid, role, application name, state, transaction age as Postgres measured it, to the microsecond, the figure the minute's bound is compared with;
never query text, "not visible" for a field Postgres hides, and "autovacuum worker" for one).
For a census that fails, its file name and the SQLSTATE;
Postgres's own message only when the error points into the census's own text (a position, which a parse or analysis error carries), since then it quotes that text and its identifiers.
An error raised while the census runs carries none, and its message is never printed:
a data exception's quotes the row value that failed to cast, and a `reg*` cast's (`regclass`, `regtype`, `regproc`) quotes the row's text under a class-42 SQLSTATE;
the advice about an earlier migration creating a missing name is given only for a name the census's own text spells.
The census writes nothing.
An argument `envoy-dispatch` does not know is refused with exit 2, never served, so an image that predates a subcommand cannot boot and migrate when a deployment asks it for one.

### The node and tuple store census

`envoy-dispatch census --store` is the other census: the node and tuple store (`store/migrations/20261109013400_graph_store.up.sql`) held against the tables that project it (`20261109022248_graph_projections.up.sql`), rather than the pending migrations held against the database.
It reads `DATABASE_URL` like every other subcommand, migrates nothing, writes nothing, and keeps the same exit convention: 0 when nothing refuses, 1 when the report refuses, 2 when the census could not be taken.
It is one `repeatable read, read only` transaction on a connection of its own, so everything it reports is one snapshot.
It records the read sequence that snapshot saw, the highest `graph_tuples.seq` it holds, or 0 for a store that holds no tuple, as R29 of the parent change has a query, a census and an approval each record the read sequence they read.
It reads that sequence inside its own transaction, so the sequence names the commit every count it prints answers, and not the end of the log when the census finishes.
The summary line after the kinds prints it last, `census: store rows_checked=<rows> differences=<differences> read_seq=<seq>`, in plain digits where the counts carry thousands separators, since it is a position to hold against `graph_tuples.seq` and not a count.

For each projected kind it counts the nodes the store holds at any state but `gone` against the projection rows, in both directions, and reports each difference by its cause and by the row it was found at:
a row naming no node, a row naming a node the store does not hold, a row naming one the store holds `gone` — which is the separate assertion that no gone node holds a projection row — a row whose shared fields disagree with its node's, and a node no row projects, named with the state it is at (`a live node no row projects`).
A state a later release adds, such as `proposed`, is a node a row projects as one at `live` is, so a build of this release reads no difference for it during a roll or after a rollback.
The shared fields are digested rather than printed, and for the three projected kinds they include the tuples that answer the row, read as the store reads a current relation (the newest tuple per kind and far end, kept while it asserts), and not the node row alone:
a row that names its node is answered from the tuples by `graph_edges`, `refs.Edges` and `refs.BacklinkCounts`, so a missing or retracted tuple takes a link out of every read while each node row still stands.
A comment's `created_at`, `author`, the artifact its `anchor` names, the artifact its `artifact_id` names and the author `graphstore.AuthorOf` reads off `author` are held against its node's `made_at` and `made_by` and its current `anchored_to`, `owned_by` and `authored_by` tuples.
A block's document and the block its `parent_block_id` names, or the document where it names none, are held against its current `in_document` and `part_of` tuples.
A block that states no parent at all digests differently from one the document holds directly, which is how the census tells a block the index has not reached yet from a block at a document's top level.
The kinds are:

- `comment` against `comments`, `block` against `document_blocks` and `block_ref` against `block_refs`, the three kinds a table projects, whose counts the report prints.
  A `block_refs` row is one link out of a block and its node is that block's, so several rows share one node: that kind's node side is every live block node that states a link now other than its `in_document` and `part_of`, whether a row names it or not, and every row of a block is held, as the set of links that block's rows name, against every such link its node states now, so a link with no tuple, a tuple with no row and a link out of a block no `block_refs` row names are all read.
  A block whose address is past the store's address bound (`graphstore.MaxAddressBytes`, which `refs.MaxBlockNodeBytes` is), and a `block_refs` row whose far end is past it, are no node and no tuple of the store by design, and both sides leave those rows out.

- `person`, `session`, `role` and a role's page, which no table projects.
  Their row side is the actors and sessions the projected tables and the delivery records name: the people and sessions `comments.author` names as `graphstore.AuthorOf` reads an actor, the sessions `comment_deliveries` and `message_deliveries` recorded reaching, and the roles `comment_deliveries.target`, `messages.target` and `issues.route` address now, with one page per role.
  A role's row side also holds every role an event of the log ever addressed: the `route` of every `issue.created`, `issue.updated` and `issue.closed` event, the `target` of every `message.created` and `message.answered` event, and the `target` of each mention a `comment.created` event names.
  A node is history and never goes, so an issue routed to one role and then re-routed to another still owes the first its node and its page, and the census reads the route history the events hold rather than only the current route.
  The publisher writes a role's node and its page from those same fields of each event it publishes, whether or not it routed anything to the role: a route an agent's write sets, which wakes nobody, a targeted message's role, which no route reaches, and a role a comment mentions on an issue routed elsewhere.
  A session's row side also holds every session a `comment.delivery` receipt of the event log says the listener took an attempt to.
  The census reads each of these row sides once a run, in one statement, and binds the names into every statement that reads that kind's rows; `backfill-graph-store` reads the same definition for roles and for sessions.
  A session's own page is no part of this census.

  An address past the store's bound is no part of the store on purpose: a role name longer than the bound is no node and no page, a comment anchored to or owned by a document whose `ref_key` is longer states no such tuple, and a session named past it is no node either.
  The census leaves each such address out of both comparisons and counts it as a class of its own, reporting the count as `addresses_past_bound` on the kind's line rather than as a difference, in both directions.

- The project of every node: `issue`, `artifact`, `ask`, `message`, `comment` and `block`, the kinds whose node names the project a read of it is limited by (`graphstore.Node`'s `Project`), or, for an object no project owns, the parties it is between (`Parties`).
  Each row of those kinds is held to what its node must carry, by the rule its writer follows (`graphcensus.scopedRows`): an issue's project; a document's project, or its conversation's session for a conversation's file; an ask's and a comment's issue's project, else its document's, else that document's conversation; a message's issue's project, or, on no issue, the parties of its thread's root (`graphstore.ThreadParties`: who wrote the root, and the session it was sent to); a block's document's.
  It prints one line per kind, `census: project kind=<kind> rows=<n> missing=<n> gone=<n> differs=<n> unscoped=<n> orphaned=<n>`: a row whose node the store does not hold, a row whose node is gone (a node at any other state, a later release's `proposed` included, is there) or is another row's, a row whose node names another project or other parties than the row's, a row whose node names neither, and a node no row stands behind.
  A second line per kind, `census: project past_bound kind=<kind> addresses=<n>`, counts its rows whose address is past the store's bound - a document whose `ref_key` is, a block whose address is, a comment on such a document - which no writer projects; they are in no count of the first line and no finding, as the store lines' `addresses_past_bound=` are.
  `gone` and `orphaned` are read for an issue, a document, an ask and a message, whose rows only their own creates write and only a document's removal deletes; the store census above reads both directions for a comment and a block.
  A document the removal can take - one of an issue that is not its spec, plays no role and is a document - gives up its address to the next document of that issue and name, so a node there may be an earlier row's: the census reads the newest state statement a create, removal or backfill made as this row's only when the row's own `artifact.created` caused it, or when it is at the sentinel and the row has no such event; a later release's mark is passed over.
  Every release appends that event for such a document, so no shared time makes an earlier row's statement read as a later one's.
  Every other document keeps its address while the store holds it, and a node not `gone` is enough for it; not all of them record an `artifact.created` of their own (`issue.created` records a spec, and a conversation's upload appends no event).
  A removal made to take another shape of document must add it to `graphcensus.removableDocument` in the same change, which `TestEveryDocumentARemovalCanTakeIsReadByItsCreateEvent` holds.

  - This build's removal states the node gone as it deletes the row, and a build before this one writes no node, so during a roll or after a rollback each class can appear on a healthy database: `gone` is a document an earlier build created at an address this build's removal stated gone, which `backfill-graph-store` gives a live node, and `orphaned` is a document an earlier build removed after this build wrote its node, which this build's block-index tick states gone during the roll (`docs/block_index_roll.go`, `writeObjectNodesAnotherBuildWrote`); an orphaned node a delete outside the builds made has no in-band cure.

  - A build before this one writes a comment's node and a block's node with no project, and this build's block-index tick gives a node only to a comment whose row names none and marks no document whose blocks that build stated at the document's version, so a comment such a build writes during a roll or after a rollback, and a block it states that no later write of this build states again, is `unscoped` until `backfill-graph-store` runs again once every server runs this build (Backfilling the node and tuple store, below).

  - Any count of the five refuses the census, and the first rows and nodes of them are named on `census: project incomplete <kind> <id>: <class>` lines (`graphcensus.ProjectComplete`, the same read), each ending with its cure: `; run envoy-dispatch backfill-graph-store` for `missing`, `gone` and `unscoped`, which the backfill writes, but for a row naming a party the store refuses (one empty or holding white space), which no writer and no pass of the backfill writes a node for and whose line says no command cures it; for `orphaned`, `; no in-band cure, unless an older build's removal made it during a two-build roll: this build's block-index tick then states that node gone`, since no backfill reads a node no row stands behind; and for `differs`, which a node in another project is at any state, that no command cures it: a node's project never moves, and a person must compare the row with its node.
    The refusal line names the same cures.
    An address past the store's bound is left out, as its writer leaves it out of the store.

It passes only when it checked more than zero rows of every kind it covers and found zero differences in both directions.
A total over the kinds is no such proof: two empty sides of one kind agree, so a kind that checked no row prints `census: store kind=<kind> checked no row` and the verdict refuses naming it.
A database the store does not exist in yet holds no kind to read: the census prints `census: store absent` and refuses, since it read nothing.

Two writes can state one fact twice by design and nothing reads wrong for it — a tuple at the `source_event` sentinel is outside both repeat keys, and the backfill writes every fact it cannot date from a row's own event at that sentinel — so the census counts nodes and projection rows, never tuples.

`envoy-dispatch census --store --sizing` adds the growth pass after that census, each line a driver can look for by its leading fields:
the write rate by kind over the last 28 days of `events`; the row count and total relation size of every table this branch's migrations touch; the tuples and nodes one write of each kind states, with how many such writes the window holds or, for a kind no table holds yet, is assumed to hold; the rows a day that puts in `graph_tuples` and in `graph_nodes`; the bytes a row each is measured at; and where each reaches in a year against `pgmigrate.CensusTableLimit`.
The kinds are a term for each kind R20 gives the five children of the parent change, each printed on a `census: sizing tuples-a-write ` line and a `census: sizing share ` line that names the child that moves it (`moved_by=`, one of the five names below), its writes inside the window and its share of the tuples a day.
Where a write states facts beyond its node, each such fact is a term of its own, counted over the writes that state it:

- `this-change`, this change itself: a comment's node and its three base attributes (visibility, state, and project or parties), and its `authored_by`, `anchored_to`, `owned_by` and aliases where its row names each; the node and three base attributes of an issue, an artifact, an ask and a message, and the `gone` state a document's removal states of its node (`artifact_removed`); a comment delivery's `delivered_to`; a block a version inserts afresh, restores, drops (its `gone` state) or moves to another parent (the old parent's retraction and the new one's assertion); a block reference's assertion and the retraction that may end it; and a person, a session, a role and a role's page, each counted over the ones the window records for the first time.

- `inbox-child`, the child that moves the inbox, the messages and the read state: a message's `authored_by` and `part_of`, and the tuples its target and its broadcast become; a reply's meaning tuple and its `part_of`; a comment's mentions; a person's part in a thread; the `delivered_to` of each write to every other participant of its thread and of an ask to the person it waits on, and the move of that person; the body blocks of a comment, a message and an ask, their links and their removal; a delivery attempt's node; and the message deliveries, read marks, clears, dismissals and snoozes.

- `blocks-child`, the child that moves issues, their documents, a block's text and rank, results and components: the text and rank values of each block whose text a version wrote; an issue's `child_of` and `needs`; an artifact's `part_of` its issue and a document version's node; a result's node with its values and tuples, its screenshots and its retraction; a component's node with its `part_of`, `depends_on` and `affects`; and a criterion's `authored_by`.

- `asks-child`, the child that moves decisions, readings, asks and their answers: an ask's `answer_state`, question, options, `authored_by`, `anchored_to` and `owned_by` that `ask.opened` states, and what its other `ask.*` events state, an edit, an answer, a resolution and a follower; a Keep; and an agent's decision and its reading.

- `lenses-child`, the child that moves interactions, lenses, lens kinds, constraints and risks: an interaction and its frames, a lens and its finding blocks, a lens kind, a constraint and a risk.

Each term a table can count is read from tables that exist before this branch's first migration, so the growth pass runs on a database the store does not exist in yet, which is where R4 takes its readings; there the census prints `census: store absent` and passes on the growth pass alone, which refuses nothing.
No such table holds a block's parent, so the moved parents are counted by parsing each version the block index indexed in the window beside the one it indexed before, placing the blocks as `refs.PlaceBlocks` places them, and matching the two versions' blocks by their text, since a version's markdown names no paragraph's id; blocks of one document with equal text are matched in document order, so that term is exact except there, and its share line carries `versions_read=` and `versions_unreadable=`.
Each version is parsed against the block schema the store's type nodes declare, read in the census's own transaction, and against the schema this binary embeds only where the store holds no type node, so a type the store holds and the file lacks does not read as unreadable.
A kind production writes no row of before its child ships (a criterion, a decision, a reading, and every kind of `lenses-child`) has no history to read, so its term carries an assumed rate instead of reading zero, its `driver=` states that rate and the reason for it, and its share line ends `rate=assumed`.
A decision, a reading and a criterion are blocks of a document, whose node and placement the block terms count already, so each of those terms counts only the tuples it states beyond a block's.
It last prints the block-index reconciliation's backlog on two `census: sizing backlog ` lines, because a loop that falls behind fails nothing and nobody would otherwise see it:
`owed_documents=` is how many documents are owed a block index (`document_block_versions.version` null, or this build's mark `store_owed_at` set, by the backfill or by the block-index tick's marking of the documents the events after its build's cursor name, of a marking rule a build of this binary's rule lists, as the tick's own count reads it), and `oldest_owed_at=` is when the oldest of those debts was incurred, with `oldest_owed_age_seconds=` beside it and `against=` naming the time that age is measured to.
No column dates a null version, so a debt is dated by the write that owed it — the `created_at` of the latest markdown version the document holds, or the document's own `created_at` where it holds no version — and a document holding the backfill's mark is dated the same way, and the line says so under `dated_by=`.
The age is measured against the newest write the database recorded (the `created_at` of its highest `events` id) and not against `now()`, so two runs print one age; a database that recorded no event prints `none` and an age of zero.
The growth pass refuses nothing on size.
Once the store is backfilled `graph_tuples` stays past the limit, so a pass that refused on size would refuse every run;
the gate is the migration census's bounded-write rule instead, under which a pending migration writes rows of a table above the limit only as an insert of at most 1,000 literal rows or an update of at most 1,000 rows by their primary key (`pgmigrate.BoundedWriteRows`, "Pre-deploy census" above).
Beside each table at or above the limit now, and each store table that projection puts past the limit inside a year, the pass names that rule on a line of its own, with the table's verdict now (`absent` for a table the database does not hold) and its year's (`unprojected` for a table other than the two store tables):
`census: sizing bounded-writes table=graph_tuples now=inside year=past: a pending migration that writes rows of graph_tuples may write them only with bounded writes once it is above the limit (pgmigrate's census: an insert of at most 1,000 literal rows, or an update of at most 1,000 rows by their primary key)`.
So `--store --sizing` exits 1 only where the store census itself refuses and 2 where it could not make a read, and the pass's last line is always `census: sizing ok`.
A backlog refuses nothing either: how large a backlog is too large depends on the write rate the lines above it carry, and the driver that watches it decides.
Every figure comes from a count or from a timestamp the database already holds, so two runs over one unchanged database print the same output.
A store table holding fewer than ten thousand rows is too small to measure a bytes-a-row from, and the projection uses 783 bytes a row instead, measured when the store was built (1,050,010 tuples at a `pg_total_relation_size` of 822,673,408 bytes on PostgreSQL 18.6, table, seven indexes and TOAST);
the output says so on the line that uses it.
`--sizing` without `--store` is a usage error: it is that census's growth pass and not a census of its own.

### Backfilling the node and tuple store

`envoy-dispatch backfill-graph-store` writes the nodes and tuples of the comments, document blocks and block references written before the store existed — every row of the three projected tables that names no node, since migration `20261109022248` added the columns and filled none of them.
It walks each kind in batches, writing a batch's nodes, its tuples and the projection columns that name them in one transaction, so a row and its node commit together or not at all, exactly as a live write does.
A comment batch takes each comment's owner lock before writing and rechecks that its row still exists and names no node, so a second run's cached batch cannot restore a comment whose row went after the batch read it.
It appends no event of its own, and a fact whose row's own event cannot be found carries the store's sentinel (`source_event` 0) as its cause:
a comment's own event is the `comment.created` that stored it, a block's is the event that recorded the version its row was written at, and a block reference's is its `source_seq`, which is that event's id already.
Each of those reads probes an index of its own, so the run reads no whole table once for each row: a block batch reads the `artifact.version` event of each document and version among its rows through `events_artifact_version_number` (migration `20261119020700`), and a comment batch reads each comment's `comment.created` event through `events_comment_created_id` (migration `20261119020800`) (`TestTheBackfillsEventReadsProbeTheirIndexes`).
The walks over the blocks and the block references read each batch from the last one's key on, in their order, through `document_blocks_naming_no_node` (migration `20261119020900`) and `block_refs_naming_no_node` (migration `20261119021000`), each partial on the rows naming no node (`TestTheBackfillsWalksReadOnlyTheirBatch`); `node_id` and `from_node` are hash indexed, which keeps no order, and the comment walk reads `comments_pkey`.
Before its passes the run analyzes `node_kind` of the three tables (`TestTheBackfillAnalyzesTheColumnsItsWalksReadBy`): the release adds those columns after any analysis, and with no statistics of them the planner guesses that a fraction of a percent of the rows name no node and reads every such row through a bitmap for each batch instead of one batch through the index.
A block-reference batch's read again under its locks checks each row's block node by that node's key, a subquery Postgres runs for each row (`TestTheHeldBlockRefReadProbesEachRowsBlockNode`): as a join, right after the block pass wrote the block nodes and before any analysis counted them, the plan read every block node once per batch.
The run refuses to start, naming each, while any of these four indexes is absent or invalid (`graphcensus.BackfillIndexes`, `TestTheBackfillRefusesToRunWithoutItsIndexes`, `TestTheBackfillRefusesToRunOverAnInvalidIndex`): it prints `backfill-graph-store: refused: the index(es) … are absent or invalid`, then the statements that build each one again by hand, and exits 1, before it writes anything.
The command migrates the database before it runs, so by then each index's version is recorded, and the runner applies a recorded version no more: an index the refusal names went missing or invalid after its migration ran (its down file run by hand, or a concurrent build by hand that failed), and no boot builds it again.
The remedy is the printed statements, run with `psql` one a line, each on its own outside a transaction as `psql`'s default autocommit runs them: a `set search_path` to the schema the tables are in, a `drop index concurrently if exists` of an index that stands invalid (its migration's `create … if not exists` would leave it as it is), and the migration's own `create index concurrently if not exists …`; then run the backfill again.
Without these indexes each event read was a read of `events` whole, and each batch read and sorted every row still naming no node, so the run's time grew with the square of the data: over a seeded database of 12,884 issues, 270,529 document blocks and 64,460 events, the block pass took 43.9 minutes, against 20.7 seconds over a tenth of that.
With them, on one machine and beside a serving build, the block pass took 85.7 s over 270,564 blocks (0.32 ms a block) against 7.2 s over a tenth (0.26 ms), the block-reference pass 0.26 ms a reference against 0.19 ms, and the comment pass 0.65 ms a comment against 0.43 ms, where the build before them took 6.24 ms a block over the full database.
Measured again in one pod against the build before these changes, over the same full database, the whole run took 228 s against 3,249 s: 0.58 ms a block against 5.55 ms, 0.69 ms a block reference against 0.74 ms, and 1.27 ms a comment against 61.0 ms.
Each tuple takes its row's own `created_at` as its time and its row's own `source_seq` as its `legacy_source_seq`, so a backfilled edge is dated and sequenced exactly as the `graph_edges` arm it replaces dates and sequences it.
Once every row of a kind the store can hold names its node, the run reports that kind `complete=true`, which is the moment the arm of `graph_edges` over that kind's table answers nothing.

It then gives every role the census's row side names, in a row now or in an event, and that has no node its node, its page and the page's `part_of` the role, as the publisher writes them for the events it publishes: the publisher writes none for an event it published before it wrote roles, so without this pass every role an older database names is a census difference.
The input is the census's own role row side, so the roles the pass gives a node are exactly the roles the census holds to one; each role's node is caused by the first event that named it, or by the sentinel for a role only a row names, and a role that already holds both nodes is left alone.
The report counts them on its `kind=role` line, and `remaining=` is how many roles the census names still lack a node or a page.

It then writes a live node for every issue, document, ask and message written before its kind's writer wrote one, or created by an earlier build at an address whose node is gone, or whose node names neither a project nor parties, and gives every comment and block node written before a node named its project that project, or its conversation's party.
Each pass walks only the rows its marker does not mark - a node not `gone` naming a project or parties for an issue, a document, an ask and a message, whose rows name no node in a column of their own, and for a document the removal can take such a node whose newest create, removal or backfill statement is the row's own (as the census above reads it), and the node's own `project_key` and `parties` for a comment and a block - and writes the marker in the transaction that states the facts, so a second run writes nothing.
A node that is its row's own and not `gone` is written again keeping the state and the visibility another write gave it, a later release's included, and states only the project or parties it lacks; a node held `gone`, or one another row left, is written as its new row makes it, and comes back `live`.
A batch of documents takes each document's owner lock first - its issue's, or the document's own where no issue owns it - which is the lock a removal of an issue's document takes, and writes only nodes whose document still has the id the batch read.
A document made again at the address, even within the same microsecond, has another id, so a removal or a re-created document that commits while the batch reads leaves no live node behind and no node describing the removed row.
A node the backfill makes is made by its row's own author at its row's own `created_at`; a node the store already holds keeps the `made_at` and `made_by` it has.
The row's author states the node live, and the backfill itself states gone a document's node that stood live for an earlier row; each statement is dated at the row's `created_at` or one microsecond past the newest statement the store holds about the node where that is later, so a read as of a time reads the order the store appended in.
A document's statements are caused by its own `artifact.created` event where the backfill finds one; an issue's, an ask's and a message's, and a document's with no such event, are at the sentinel.
A filled project is stated at the node's own `made_at`, by its `made_by`, under its cause (`graphstore.FillScopes`); and the fill of a block's project takes its document's revision fence first, as every writer of a block's node does.
The report prints a `kind=` line for each of `issue`, `artifact`, `ask`, `message`, `comment_project` and `block_project`; `envoy-dispatch census --store` then reads `missing=0 gone=0 differs=0 unscoped=0` for every project-owned kind, and `orphaned=0` unless an earlier build removed a document whose node this one wrote, which this build's block-index tick states gone during the roll.
A build before this one keeps writing comment and block nodes with no project while it serves, during a roll or after a rollback, and no tick of this build gives those nodes one (the node and tuple store census, above), so the backfill runs a second time once every server runs this build: that run gives the comments and blocks the earlier build wrote meanwhile their project and writes nothing over the rows the first run marked.

A block's parent is the one fact it does not write.
The parent is the nesting of the rendered version, which no column of `document_blocks` holds, and the version's stored markdown cannot give it either, since a paragraph's id is no part of that text; only the server renders the document the ids belong to.
So the run marks each document it touched, and the reconciliation's index pass writes every block's node, its `part_of` tuple and `parent_block_id`, which is that column's one writer, and clears the mark.
The mark is `document_block_versions.store_owed_at`, a column migration `20261116221539` adds and only this release reads or writes, and not the null `version` that `rebuild-refs` sets and every release reads; beside it `store_owed_rule`, which migration `20261120122600` adds, records the marking rule of the build that set it (below).
A process of the release before this one, during a roll or after a rollback, reads a null `version` as its own debt: it indexes the document, writes the rows and states nothing in the store, and the document would be owed nothing again.
It never reads or clears `store_owed_at`, so this release's reconciliation, which lists a document holding it as it lists one whose `version` is null, still reaches the document and states every parent (`TestABackfillWhoseMarkThePreviousReleaseDrainedStillReachesEveryParent`).
The batch that names a document's block nodes writes the mark in its own transaction, so a run stopped after any batch leaves every document of its committed batches marked (`TestABackfillKilledAfterOneBatchLeavesThatBatchsDocumentsOwed`).
So `census --store` reads a difference on every backfilled block until that reconciliation has drained the mark, at twenty documents a tick;
`TestTheBlockParentCensusReachesZeroAfterTheBackfill` is that whole route end to end.

A document that the release before this one edits during a roll, after this build's drain has cleared its mark, carries no mark, and neither does a document that release creates after the drain, the spec of an issue it creates included, nor a comment it writes after the backfill:
that release writes the version's rows and the comment's row with no node, states nothing in the store and never sets `store_owed_at`.
So every block-index tick reads the events after its build's cursor, whether or not another build serves, and marks owed every document an `artifact.version` or `artifact.created` event names, and every document of each issue an `issue.created` event names, whose `document_block_versions.store_version` is not its `version`.
`store_version` is the version whose blocks this build's index pass last stated in the store, and only that pass writes it, so a document this build wrote and indexed is not marked again.
A document the release before indexed after the store stated it, one it created, whose `store_version` is null, and one whose `version` is still owed an index (null) are marked, and so is a version that only drops, moves or rewords a block, which leaves no row naming no node (`TestAVersionThePreviousReleaseWritesThatOnlyDropsABlockReachesTheStore`).
A version event names its document in `payload->>'artifact_id'`, and the event that creates a document carries the whole artifact and names it in `payload->'artifact'->>'id'`.
An issue's create records its spec's version 1 with the `issue.created` event alone, which names the issue in its `issue_key` and no document, so the tick marks every `doc` artifact of that issue, read from `artifacts` by issue key.
The tick also reads `comment.created` and `comment.answered` events, each of which names the comment it stores in `payload->>'id'`, and gives each such comment whose row names no node the backfill's own write for that row, in the tick's transaction: its node, the facts its row projects and who wrote it, with that event as their cause (`TestACommentThePreviousReleaseWritesAfterTheBackfillReachesTheStore`).
That is the backfill's contract applied to the rows the log names: a row that names its node is left alone, so it is no reconcile.
The tick does all of this before it lists the owed documents, so the same tick drains a document it marks when its batch has room.
Because every tick reads its events, which build's ticks come first in each period of the shared block-index lease, and so whether this build ticks while the release before serves at all, decides nothing (`TestAVersionThePreviousReleaseWritesWhileItHoldsTheLeaseReachesTheStore`), and neither does a rollback or a roll forward that stops one build before it starts the other (`TestAVersionThePreviousReleaseWritesWhileThisBuildIsStoppedReachesTheStore`).
The cursor is one row per build commit of `block_index_event_cursors`, a table migration `20261118115645` creates and the previous release never reads, and its `event_id` is the highest event id the build's ticks have read or passed; its `marking_rule` (migration `20261120122600`) is the marking rule of that build.
A build that holds a row starts at its own row, however old, and reads every event since, one page a tick.
A build with no row of its own inserts one at its first tick that reads a proven safe horizon: at the highest `event_id` a row of a build of its own marking rule holds; where no such row exists, at the horizon the newest backfill of its own rule recorded when it began; and where neither exists, at that tick's horizon, so the cursor never starts at the beginning of the log (`TestAVersionThePreviousReleaseWritesBeforeTheNextBuildStartsReachesTheStore`, `TestADocumentThePreviousReleaseCreatesBeforeThisBuildsFirstTickReachesTheStore`, `TestASecondBuildsFirstTickReadsNoEventBelowTheCursorAnotherBuildLeft`).
A build reads no cursor and no horizon of another rule, since that build or that backfill marked what its own rule owes (`TestABuildOfANewerMarkingRuleReadsFromTheBackfillHorizon`, `TestAHigherRuleStartsAtItsOwnHorizonAboveALowerRulesBackfill`, `TestABuildWithOnlyALowerRulesHorizonStartsAtItsSafeHorizon`).
The backfill's horizons are the rows of `graph_backfill_horizon`, which the same migration creates and migration `20261120122600` keys by `marking_rule`: one row for each rule.
Before it reads any row, `backfill-graph-store` takes a mark of the event log, waits until no transaction that can still write an event took its transaction id before the mark (`events.Horizon.WriterFloor`), and records the newest event id the mark saw, at or below which every event was final by then, so every event the run cannot have seen is above it; it prints it as `backfill-graph-store: horizon=<id>`, and each run replaces the row of its own marking rule and leaves every other rule's (`TestABackfillOfALowerMarkingRuleLeavesAHigherRulesHorizon`).
The cursor moves with each tick, so each tick reads only the events since the last one, and nothing scans a whole table.
A tick reads at most 1,000 events, in id order, after the cursor and at or below the safe horizon (`events.Horizon.Safe`), because event ids are not commit order and an uncapped read could pass an event that is still committing.
A full page moves the cursor to the last event it read and brings the next tick 2 s later; otherwise the cursor moves to the horizon, and it never moves past it.
The mark is the backfill's own, set in one statement that takes the documents' `document_block_versions` rows in ascending `artifact_id` order and no owner row.
The tick's transaction takes its cursor row first, then those rows, and only then the comments' nodes, tuples and rows, since every writer under the revision fence takes a document's `document_block_versions` row before any node it writes; the marks, the comments and the cursor's move commit together.
The tick's `dispatch: block index` line carries `events=`, the events it read, `marked=`, the documents it marked, `comments=`, the comments it gave their nodes, and `another_build=`, whether a live process of the registry (`server_processes`) registered a commit other than this process's own (`main.buildCommit`, which `docs.Deps.BuildCommit` carries), which decides nothing of what the tick marks (`TestAVersionThePreviousReleaseWritesAfterTheDrainReachesTheStore`) and decides whether it owes the object pass (below).
The migration's census answers `0`: the two tables are new and hold no row at deploy.

The marking rule versions what a block-index pass and the tick owe: which documents a build marks and which comments its tick writes.
The code declares it once, `refs.BlockIndexMarkingRule`, which is 1, the first rule this code ships, and raises it whenever what a pass owes changes; `TestTheMarkingRuleNamesTheStatementsItMarksWith` pins a digest of the statements that decide it for each value of the rule, so a change to one of them that keeps the rule fails.
Every statement that marks a document writes its build's rule into `store_owed_rule` and keeps the higher of the stored rule and its own where a mark already stands; one that marks only a document holding no mark reads a mark of a lower rule as none and raises it (`TestEveryMarkingStatementKeepsOrRaisesTheRuleAsR13States`).
The column is `not null default 1`: the release before this one and the trigger `document_block_versions_owe` insert rows without naming it, so a mark no later rule wrote is rule 1's.
A pass of rule r lists a document whose mark is of rule r or lower, and one marked by a higher rule only where its `store_version` is null or below its `version`, which is work of its own there; it wins the fence at the version the row stands at only where a mark stands and the same holds; and it clears the mark only where the mark's rule is r or lower, decided in the fence's own statement against the row as it then stands (`TestALowerRulesDrainLeavesAHigherRulesMark`, `TestAHigherRulesPassListsAndClearsALowerRulesMark`, `TestAHigherRulesMarkWrittenDuringALowerRulesPassSurvivesItsStand`).
So a build of rule 1 that drains a document a build of rule 2 marked, during a roll or after a rollback, states the version and leaves the mark for the build of rule 2.
The tick's `owed_documents` and the sizing's backlog count only the documents the build lists (`TestABuildsBacklogCountsOnlyTheDocumentsItLists`).
Migration `20261120122600`'s census answers `0`: each column's constant default is kept in the catalog and rewrites no row, and every row that stands reads rule 1.

A row the release before this one deleted is nothing the run reads.
That release, indexing a version of a document the store has stated, deletes the `document_blocks` row of each block the version drops and the `block_refs` row of each link a block stops holding, and it states no node `gone` and retracts no tuple.
No row is left to read, and a node stated `gone` because its row is missing would be a repair.
What the run reads instead is the two versions the document's own row records: `version`, which that release writes, and `store_version`, the version at which this release's index pass last stated the document's blocks, which the same migration adds and only that pass writes.
A document whose `store_version` is below its `version` is one another release indexed after the store stated it, and the run marks it too.
The reconciliation's index pass over its latest version finds the store last stated the document at a version other than the latest one any release indexed, so it reads what the store still asserts of the document beside the rows and decides from the version's render: a block the store holds live that the version does not hold is stated `gone`, as a block a version drops is, and a link the store asserts that no block of the version holds is retracted (`TestTheBlockAndTheLinkThePreviousReleaseDroppedAreGoneAfterTheBackfill`).
A version of the document that this release writes before the run does the same, in its own index pass.
No release deletes a comment's row: no route of either deletes one, and a document is removed only while no comment points at it, so a comment leaves nothing to state `gone`.

It is no repair.
Its input is each row's own content and the two versions each document's row records: it never reads one side of a projection to write the other, so a row its marker marks is left alone and a kind whose rows disagree with the store is for the census to report.
Two reads of the store's own statements decide no fact and only order what it writes: for a document the removal can take, the cause of its node's newest state statement, its marker (above); and the time of a node's newest statement, which it dates past where the row's `created_at` is no later, so a read of the node as of a time keeps the order the store appended in.
It is safe to run again at any time, after a rollback as well: every pass walks only the rows its marker does not mark - a row that names no node, or for an issue, a document, an ask and a message a row no node at a state but `gone` stands for, or for a document the removal can take a row whose node's live statement is another row's - writes the marker in the transaction that writes its tuples, and reads no marked row, its guards included; a document is marked only while it holds no mark and the store has not stated its latest indexed version; and no release before this one drains the mark.
The pointer pass skips a link whose block's node the store does not hold or holds `gone`, read off `graph_nodes` and not off the block's `document_blocks` row, which is `graph_edges`' own test of the tuple arm: a link stamped against a gone node would answer from neither arm.
So a run interrupted anywhere carries on from where it stopped, and a run over tables whose every row names its node writes no node, no tuple and no node column (`TestASecondBackfillWritesNothingOverMarkedRows`).
It migrates the database first, as the other backfills do, and exits 1 when it skipped a row or left one naming no node.

### The view over the store

Migration `20261109215947_graph_edges_tuples` re-states `graph_edges` with one arm over the current tuples and a `node_kind is null` test on each of the four arms whose relations the store now writes.
The new arm takes, for each `(from_kind, from_id, kind, to_kind, to_id)`, the newest tuple, which is an edge only while that newest one asserts, with `to_kind <> 'value'` so no attribute's value is an edge.
It renders a block's `in_document` as the `part_of` to its document the view has always answered, a block's `mentions` and its pointer kinds, and a comment's `anchored_to` and `owned_by`, the list `refs.TupleKinds` holds and the announcer writes into `server_build_announcements.tuple_kinds`;
the tuple's `at` is the row's `created_at` and its `legacy_source_seq` the row's `source_seq`.
No other kind the store writes is an edge of this view — `authored_by`, `delivered_to`, `held_by`, the alias tuples, `attribute_of`, and a session's or a page's `part_of` — and a `part_of` whose source is a block is left out too, since the view has no block-to-block row and a top-level block would otherwise be rendered twice.

Each row decides which arm answers it: each of the four old arms answers only the rows of its own table whose `node_kind` is null, and the tuple arm answers an edge of those kinds only while the projection row that names it carries its node — the `document_blocks` row of the block for `in_document`, the `block_refs` row of the pair for a pointer or a mention, and the `comments` row of the comment for `anchored_to` and `owned_by`.
One transaction writes both sides of a row — the writer that states a row's tuples names its node on the row — so the view answers each row exactly once, at every point of the backfill and not only at its end, and a row a previous release wrote during a roll, a row written after a rollback and a block past `refs.MaxBlockNodeBytes` each keep their answer from the old arm.
The join is what keeps a rollback whole, since a build of the previous release retracts nothing in the store: a row it deletes leaves its tuple standing and answers from neither arm, and a row it deletes and writes again with no node answers once, from its old arm.
The arm also answers no row for a node whose current `state` is `gone`, the test the backfill's pointer guard takes too, and a pointer a version dropped is retracted in the same statement that deletes its `block_refs` row.
`refs.BacklinkCounts` moves the same four arms the same way, each reading its own table for the rows that name no node and the tuples, through the same join, for the rest, so a count and the list behind it cannot disagree.
A pointer's allowed targets come from the attribute node once the store answers it (`block_pointer_targets`), read as `pmdoc/schemanodes.Read` installs the schema: through the current `attribute_of` tuple and from a live attribute node, so a change to an attribute's `targets` reaches every link of that attribute with no pass over documents nobody edits.
The reads of the `block_refs` rows themselves, a document's links out (`refs.Edges`) and `refs.BlockLinks`, make the same test of a row that names its node (`refs.blockRefListed`), so no reader lists a link another hides; a row that names no node keeps the `to_types` its index copied.
`TestGraphEdgesLosesEveryArmOverAMovedKind` reads the arm list back out of the catalogue and reads the whole view, the previous release's view over the same rows and each arm on its own before the backfill, after it, beside rows a previous release wrote and after a rollback that deletes and rewrites rows, with every backlink count and the store census beside them.

## Stored-order check

`envoy-dispatch stored-order [--artifact <uuid>]... [--since <RFC 3339 time>]` reads, and never writes, whether each document's stored history replays:
folding its `doc_updates` rows in version order must give every row's `content_changed` (measured against the render the rows before it fold to, true when either side does not render) and every version's markdown at its `doc_update_version`, and no row may be stored ahead of one it depends on.
A row after which the fold parks structs is that only when no later row carries what it parked again:
the room stores a peer's edit that arrives ahead of the edit it builds on at once, so a restart keeps it, and the commit that integrates it once that edit arrives stores it again, so the check folds the history a second time without such rows and reports them as `pending` only when that fold differs.
A row still parked when the history ends is counted as `awaiting`, not a mismatch:
the store alone cannot tell an edit whose predecessor has not arrived from a dependency never stored, and a dependency the room held and the store lost also leaves every later version rendering text the fold lacks.
It reads `DATABASE_URL` as the other subcommands do and runs no migration; with no `--artifact` it checks every document.
Each document is read in one repeatable-read transaction, which stays open while the check renders the document at every checked update:
tens of thousands of updates hold one snapshot for minutes, and an open snapshot on the primary holds back vacuum.
On production, pass `--since` or point `DATABASE_URL` at a read replica.

The oldest retained row is the base: compaction folds older history into it and a rebuild seeds the history there, so its own class is not checked and a version recording an update below it is compared with nothing.
Compaction runs on its own, and no setting holds it off:
a resident room's rows fold once 16 of them are older than two minutes or one is older than fifteen minutes, and an hourly sweep folds every document due that rule.
So a version created inside the checked window can record an update a compaction has already folded into the base.
The check counts those versions `versions_compacted` and prints a reason naming the base;
a document's first version, recorded before any row of it was stored, names no update and is skipped as it always was.
A document the counted versions leave with no version compared is `unchecked`, never clean, whatever classes the fold measured, and a document with versions at or above the base is clean only on what it compared.
A history a prune rewrote after the base (a checkpoint standing, a gap, a version past the head or behind an earlier one) is refused.
`--since` folds rows and versions created before that time without checking them, so history an earlier build wrote is not read as this build's:
run it with the start of the window under test, and read `versions_compacted` for how much of that window a compaction folded away.

The check holds only for one renderer: run it from the same commit as the Dispatch whose history it reads.
A version an earlier build rendered (a change to `pmdoc`'s Markdown output) differs from this build's render of the same state.

Output, every line starting `stored-order: `:
one line per document (`<uuid> base=… updates_checked=… updates_skipped=… versions_checked=… versions_skipped=… versions_compacted=… mismatches=…`, with `compacted="<reason>"` for a document whose versions record updates below its base, `awaiting="<reason>"` for one holding rows still parked, and `refused="<reason>"` for a refused document), one line per mismatch (`class update=… stored=… expected=…`, `pending update=… parked_items=… parked_delete_ranges=…`, or `version number=… doc_update_version=… stored_md5=… stored_length=… fold_md5=… fold_length=…`), and last a summary (`documents=… clean=… mismatched=… refused=… failed=… unchecked=…`).
It prints no document text.
A document whose check compared no update and no version, one whose versions inside the window all sit below the base included, is `unchecked`, never clean.
Exit 0 when every document checked is clean, 1 on any mismatch, refusal, read failure, missing `DATABASE_URL`, unchecked document `--artifact` names, or a run that checked no document at all (an empty database included), 2 on a usage error.
So exit 0 always means the check compared something.

`packages/envoy/loadtest/browsertype` is the browser side of a load test this check judges:
it connects peers to documents' rooms as people, each named in the server's identity header (`-identity-header`, `X-Dispatch-User` by default, on a server run with `DISPATCH_IDENTITY=header:<name>` and `DISPATCH_IDENTITY_HEADER_TRUSTED=1`, since a room admits no agent token), and types inside existing paragraphs at a set rate (`browsertype -h` lists its flags).
A paragraph whose text holds a `-protect` value (repeatable) is never edited, so the text a load generator's own writes quote stays as it wrote it.

## Reference graph

Mentions (`dispatch://` references and same-origin dashboard URLs) in document versions, ask questions, comment bodies, and issue messages are indexed into `refs` on every write;
migration `0032_refs_provenance` gives each edge a `kind`, `created_at`, and `source_seq` (the `events.id` that introduced it), and creates the `graph_edges` view, which unions those mentions with the structural relations already stored on the node tables (`child_of`, `attached_to`, `anchored_to`, `owned_by`, `replies_to`, `followed_by`).
The name of a typed block's `block[]` attribute is an edge kind too:
each block the attribute points at is an edge of that kind from the typed block (migration `20261101144855`, Database migrations above).
`GET /api/v1/references?to=<ref>` lists everything that points at a node and `?from=<ref>` everything it points to, cross-project.
A row whose other end is a node kind the running build does not know (a newer build's, read during a rolling deploy) is left out of every list and every count that build serves (`nodeRegistry`, `refs/graph.go`, names the kinds it knows).
The migration refuses to run when an ask or comment anchor carries a non-uuid `artifact_id`.

Each block of a document is a node of the graph too (migration `0121`).
Its reference is the document's with `#b-` and the block's id, percent-encoded as `encodeBlockId` (`@legion/contracts`) writes it:
`dispatch://KEY/spec#b-<id>`, `dispatch://KEY/artifact/<slug>#b-<id>` or `dispatch://PROJECT/artifact/<slug>#b-<id>`.
Text cites a block only while its id holds at most 256 bytes decoded (`text.MaxBlockIDBytes`), and any node only while its target id, the id its edge is stored under (`text.TargetID`:
an issue's key, a document's `<KEY or PROJECT>/<slug>`, a document block's `<KEY or PROJECT>/<slug>#<id>`, a comment's or message's block's or an ask's part's `<item kind>/<item id>#<id>` (`ask/<ask uuid>#<part id>`), an ask's, a comment's or a message's id), holds at most 2048 bytes (`text.MaxTargetBytes`), since `refs` and `block_refs` hold it in B-tree keys and neither a slug nor an item id has a bound of its own:
text citing past either is written with no edge.
The bounds decide only what the graph indexes.
Every write is taken whatever the length of a block id it names (a seeded spec, an upload, an `insert`, a rebuild, an accepted suggestion, a browser's edit);
a block carrying an id past 256 bytes is no node, and the server logs a warning naming the document, the longest id's length and the bound, and writes the version.
A route reads the reference it is given whatever its length (`text.ParseRef`):
`?to=`, `?from=` and `GET /api/v1/block-links` read a node of any name, and a block whose id is past 256 bytes is `404 NOT_FOUND`, no node.
An ask block and an anchor in a block are written under an id of any length (migrations `0127` and `0128`, Database migrations above).
A mention written inside a block starts at the innermost block holding it, so `?to=` a cited node names that block, whose `document` is the citing document;
a document's own read also lists the links at its blocks, each edge naming its block as `block`, and `?kind=part_of` on a document lists each of its current blocks.
A block reference pinned to a version of its document, `#b-<id>@v<N>` with N at most 9223372036854775807, reads the block as that version held it:
the node's `pin` carries the block's text at `N` and says whether the block is `unchanged`, `changed` or `removed` since.
`GET /api/v1/block-links?from=<document>&to=<document>` lists the links from the blocks of one document to the blocks of another.
A typed block's `block[]` attribute holds canonical block references joined by one space, and its `targets` limit the types of the blocks it points at:
an agent's or API write introducing a pointer to an existing block of another type is refused, a browser's write never is, and every block-aware read lists a pointer's edge only while its target has an allowed type, so retyping the target hides or shows it.
Text stored before the release, and text an earlier build writes during the roll, is indexed by the server's block-index reconciliation (lease `block-index`), with no command run.

A comment and a message are blocks too (migrations `0142`-`0144`).
The server splits a body into its top-level blocks as the document parser reads them, each block the text as its author wrote it (a body the parser refuses, such as one holding block HTML, splits at blank lines into blocks of type `text`;
a body of only whitespace has none), and names them `b1`, `b2`, …, never minting a number twice in one item.
A block's reference is its item's with `#b-` and the id:
`dispatch://KEY/comment/<id>#b-<id>`, `dispatch://PROJECT/artifact/<slug>/comment/<id>#b-<id>` or `dispatch://KEY/message/<id>#b-<id>`, and a comment's or message's dashboard page with a `#b-` fragment names it too.
Its graph node is kind `block`, id `comment/<uuid>#<id>` or `message/<uuid>#<id>`, whose `parent` is the comment or message;
`graph_edges` stores it under the kinds `comment_block` and `message_block`, which a build without item blocks skips as unknown kinds.
A mention in a comment or issue message starts at the block holding it, so `?to=` a cited node names that block and counts each citing block once;
a comment's or message's own read lists the links at its blocks, each naming its block as `block`.
A direct message writes no mention.
An edit of a comment keeps each block's id:
with the ids the dashboard editor carried (`blocks` and `base_revision` on `PATCH /api/v1/comments/{id}`), a block keeps the id whose span starts in it, so a moved, reworded, split (the part holding the start), joined (the first) or restored block keeps it;
without them, or against a revision another save replaced, ids follow the text.
Each indexed edit of a comment is a revision, and a comment block reference pinned to revision N, `#b-<id>@v<N>`, reads the block as N held it, with `pin` saying `unchanged`, `changed` or `removed` since;
a message numbers no revisions, so nothing holds its blocks as an earlier reading left them and a message block takes no pin (`400 INVALID_REFERENCE` on a route, no reference in text).
A reply names the block it answers with `reply_to_block` (a comment) or `in_reply_to_block` (a message), a `replies_to` edge to that block, and every comment and message a route or event returns carries `reply_block`, `null` when it names none.
`GET /api/v1/comments/{id}/blocks` and `GET /api/v1/messages/{id}/blocks` list an item's current blocks.
A comment or message an earlier build writes during the roll, or after a rollback, is owed its blocks (`owed_at`, set by the triggers of `0143` and `0144`), which the same block-index reconciliation indexes, at most 200 a tick, with no command run.

An ask's parts are blocks too (migrations `20261101143211` and `20261101143212`):
its question, each of its options and its current answer, in that order (`refs.AskParts`), each a text of its own rather than a span of one body, since an ask has no body
— the question as the row holds it, an option as `<label> — <description>` or its label alone, and the answer as the one line an `ask.answered` summary renders it.
What identifies a part is its type and its text, and for the answer part its answerer, so the same answer from another person is a part of its own (`askAnswerPart`).
Parts are named `b1`, `b2`, … as an item's blocks are, and an edit keeps each id the same way, except that an answer is replaced rather than reworded:
`itemblocks.Align` takes the types a new block replaces instead of rewording as an argument of its own, which every caller fills, and the ask's index passes the answer part's (`refs.AskAnswerPart`), where a comment's and a message's pass none.
A later answer is a new part, so a reference to the answer it replaced reads `removed` where one to a reworded option reads `changed`.
A part's reference is the ask's with `#b-` and the id:
`dispatch://KEY/ask/<id>#b-<part>` or `dispatch://PROJECT/artifact/<slug>/ask/<id>#b-<part>`, and an ask's dashboard page with a `#b-` fragment names it too;
pinned, `#b-<part>@v<N>` names a revision of the ask's parts, as a comment's block pins a revision of the comment, and reads the part as that revision held it.
Its graph node is kind `block`, id `ask/<ask uuid>#<part id>`, whose `parent` is the ask;
`graph_edges` stores it under the kind `ask_block`, which a build without ask parts skips as an unknown kind.
An ask's detail read, its owner's ask list, the Inbox and the ask payloads the events route serves carry the part id of each option and of the answer, with the ask's `revision`, the number a pin names (`attachAskPartIDs`);
a write's own response carries neither, nor does an ask the index has not reached, whose revision reads 0.
An ask's own read lists the edges at its parts, each naming its part as `block`, and leaves out both its `replies_to` edges, its clarification thread, and its parts' `part_of` edges unless `?kind=` names them (`refs.AskBacklinkExclusions`).
A reply names a part of an ask only when its request names the ask, `ask_id` with `reply_to_block`:
a reply to a comment names a block of that comment, whether or not the comment's thread hangs under an ask, and so does the delivery callback `POST /api/v1/comments/{id}/reply`.
A reference to a part no current revision of the ask holds is `404 NOT_FOUND` `no part "<part>" in ask <id>`.
An ask any build writes is owed a part index (`20261101143212`'s trigger on `asks`), which the same block-index reconciliation drains.

```bash
DATABASE_URL=postgres://... envoy-dispatch rebuild-refs
```

reparses every source and reconciles the index with the text (surviving edges keep their provenance, edges of deleted sources are removed), marks every document owed a block index and every comment, message and ask owed a block index of its own blocks, an ask's parts included, which the server's block-index reconciliation then indexes, and prints `rebuild-refs: documents=N asks=N comments=N messages=N orphans=N edges=N blocks_owed=N items_owed=N`, `blocks_owed` being the documents it marked and `items_owed` the comments, messages and asks.
It reads `dispatch.server_url` from the envoy config the same way the server does and refuses an empty value:
dashboard-URL mentions are recognised only against it, so an empty URL would drop them all.

A picture in a message, comment, reply, ask question, answer or resolution reason is CommonMark's image syntax whose address is the picture's own Dispatch reference, versioned:
`![<file name>](dispatch://<owner>/artifact/<slug>@v<N>)`, where the owner is an issue key, a project key, or `agent/<session id>` for an agent's conversation.
The reference inside is indexed as any mention is.
An agent's conversation's file is `dispatch://agent/<session id>/artifact/<slug>[@vN]`, the session id as written, and its dashboard page `/agents/<session id>/artifacts/<slug>[?v=N]` names the same artifact;
a session id holding a `/`, `?`, `#` or whitespace names nothing.
`graph_edges` has no structural edge for such a file (no `attached_to`, since it belongs to no issue):
it appears in the graph through the mentions that cite it.

Every node the graph reads carries six facts, whatever its kind:
`made_by` and `made_at` (who made it and when), `changed_by` and `changed_at` (who last changed its own text or fields and when, its maker while nothing has changed it since), `parent` (`{kind, id, ref?}`, the node it belongs to, `id` that node's own id) and `parent_version` (the version of that parent it belongs to:
for a document's block its changed version, for a comment's block its changed revision;
for an ask or comment written on a document the version its anchor names, for a block ask the version that first held its block, for an approval request the version it named when it opened, else the version current when it was written).
A reply that names a block is part of that block.
A fact Dispatch never recorded is `[]` for an actor list and `null` otherwise:
a session has none, a component's makers are never recorded and its times only from this release on, a document block's maker, and a block ask's version, are known only when the block is in the document's first version or Dispatch indexed the version before the one that first held it, and a comment block's making time is not known for a block found at the first indexing of a comment already edited by then (each kind's loader in `nodeLoaders`, `refs/graph.go`, reads its sources).
Migrations `0130`-`0134` (Database migrations above) add what the facts read:
the version current when an unanchored comment or ask is written on a document, recorded at insert and filled for the rows already stored, and `component_facts`, which every import keeps.
`dispatch read` prints the facts of the node it reads on a `Node:` line, before `Referenced by:`.

## Search

Migration 0010 added stored generated `search` columns.
Migrations `0057`–`0061` made them plain columns that a `BEFORE INSERT OR UPDATE` trigger per table fills (`issues_search`, `artifact_versions_search`, `comments_search`, `asks_search`, `messages_search`), so no application code writes or refreshes a search vector.
Every indexed text, headline, and duplicate-title comparison passes through `search_text` (0056), which puts a space after every sixteenth underscore-joined segment.
A document's markdown, and so an issue's spec, passes through `search_document_text` (`20261121060636`), which removes the ask blocks and then applies `search_text`:
removed after it, a block between two runs joined them into one run that nothing broke, which the text-search parser read again from each letter, so 41,000 bytes of `a_` runs between ask blocks took 5 s per vector.
The document and issue triggers, the related-item check and the duplicate check each call it, so the text each compares with a stored vector is built as every vector written since that migration was.
A query is not normalised and is limited to 1,000 characters.
`websearch_to_tsquery` ANDs a person's words, so a question of theirs answers nothing as soon as one of its words is in no item at all.
That AND is what every search runs, whatever the question's length, and a question it answers is answered from it and nothing else.
Only a question it answers nothing at all for is searched a second time, for the `searchBroadenWords` (5) rarest words of the question ORed, each stemmed by the same `websearch_to_tsquery`, the words the question excludes ANDed onto them so that a `-word` still leaves its rows out (`searchRarestWords`).
Rarest is the document frequency the project's own term statistics hold (`search_term_stats`, the counts the related-item check weighs its terms by: `relatedCoordinationRank`), and a word those statistics do not know is kept only once the words they do know are spent.
The bound is what keeps that second search a search:
ORing all thirty of one ask's own words matched about 43,000 items of a corpus of 79,555, where the AND of them matched the one ask the question was written from, and with each leg read `contracts.SearchKindDepth` deep that ask left the page altogether.
`ts_rank_cd` ranks what either search matched, which puts a row holding all of the words first.
The operator forms a person writes — a `"phrase"`, an `OR`, a `-word` — are `websearch_to_tsquery`'s own reading throughout, and the answer names the ones it read (`query.operators`, `contracts.SearchOperator*`) and whether that second search is what answered (`query.broadened`).

Three filters narrow every leg of a search beside `project`, each in the arm itself rather than over the page, so a filtered search reads its own matches as deep as any other search reads (`searchRowFilters`):
`status` is the status of the issue a row belongs to, and a row whose kind has no issue — a project document, and a comment or ask on one — matches no status at all;
`author` is the actor that wrote the item the row names, matched on its id (a person's login or a session's id), an issue's and a document's being their creator;
and `updated_since` (RFC 3339) is when the item itself was last written, not when anything on its issue moved, since an issue's `updated_at` moves whenever anything on it does.
The answer carries the filters it applied (`filters`), as it carries how it read the words.

A search carrying `number_project`, `status`, `author`, `updated_since` or `keywords_only` is sent to `GET /api/v1/search/results`, which answers exactly what `GET /api/v1/search` answers and is served by the same handler.
It has a path of its own because a build released before those members answers it `404 no route`, where it answers `GET /api/v1/search` with the members dropped: unnarrowed rows under the filters a person chose, which is the wrong answer rather than a refusal, and the two builds of a rolling release serve at once.
`GET /api/v1/search` still reads all nine, so a page served by an earlier release that sends a filter there is still answered narrowed; what moved is the declared contract, and so every generated client, which offers the five on the results route alone.

Search covers issue titles, the latest settled document text, comments, asks (questions, option labels and descriptions, free-text answers and the options an answer chose), and messages.
An issue's own vector carries its key and its title at weight `A`, as `0088` left them, and the markdown of the latest version of its primary document at weight `C` (`20261105145242`), so an issue whose title holds none of its spec's words is still found by them, and below every issue whose key or title holds them:
`ts_rank_cd`, which ranks every keyword arm, scores `A` at 1.0 and `C` at 0.2.
The spec is read as the document arm reads that same document, through `search_document_text`, and a new version of it re-indexes its issue through `20261105145243`'s trigger on `artifact_versions`, as `0140`'s queues the issue's embedding.
An ask hit's excerpt is cut from that same indexed text (the question, then `ask_search_suffix`), never from the options' raw JSON, and the hit carries `ask`, where the ask stands when the search ran:
`open`, `answered` with its latest answer, or `resolved` with its resolution and, when anyone answered it, the last answer it had, which for an ask that document settlement retracted while it was answered is read from its `ask.answered` events, since its row keeps none.
Live document text takes up to the 2 s settle delay to appear in search results.

Every trigger builds its vector with `search_vector` (0087; the issues trigger's through `issue_search_vector`, `20261105145242`, which bounds the key, the title and the spec joined and cuts the spec's half alone).
Postgres holds at most 1,048,575 bytes of lexemes and positions in one vector.
Prose stays far below that, since its words repeat, but text of words no two alike passes it well inside a document's 1 MiB:
about 700 KB of `w000001 w000002 …`, or 475 KB of UUIDs.
`search_vector` indexes a text whole when its vector fits, and otherwise the longest of its first half, quarter, eighth, … that fits, cut between two words, so keyword search finds such a document, comment, ask or message by the whole words in that opening part and not by the words after it;
a text with no whitespace before the cut is found by none of its own.
An issue's key is always indexed whole.
Meaning search, where an embedder is configured (below), ranks by the embedding of a window of neighbouring blocks, not by the text's search vector, so it can still return such a text for a query whose words the vector does not hold, and it names the block of the window that ranks highest for the query (`block_id` on the hit).

**A search at a past time reads the keyword table.**
`search_text_versions` (migration `20261120021308`, Database migrations above) holds one row for each version of the text that keyword search reads.
For an issue it is the title, with the version of the primary document that the issue held.
For a comment or an issue message it is the body, and for an ask it is the question, a space and `ask_search_suffix` of its options and answer.
A version starts at the event that changed that text, and an event that changes no searchable text starts no version.
An edit of a comment or an issue message starts a version of its new body, and a delete of one starts a version of the empty body.
Where an event cut a post's body, the fill reads the whole body from the post's present row, else from its body history (`post_body_history`), the body the next edit or delete kept, else from a comment's first blocks.
The functions that make the item's own `search` column make the vector of each version, so the vector of an item unchanged since its version started equals that column.
The keyword fill writes the rows from the event log while Dispatch serves (Background loops, above); after a fill of an earlier rules version wrote during a roll, it reads again what that version passed over or decided otherwise (Database migrations, above).
`store.TextVersionsAt(ctx, cut)` answers `store.TextVersions`, the relation of the version of each item that held at the cut.
A statement joins it as `from <SQL()> v` and matches, ranks and cuts snippets in itself, as the arms of the present search do over the items' own tables.
`TextVersionsAt` refuses a cut that the fill has not reached with `*store.TextVersionsNotReachedError`, which names the reach (`internal/dispatch/store/README.md`).
The relation's `deleted_at` is the post's own delete mark on every version of a deleted comment or message, at every cut, so a reader gives no deleted post's words; no route searches at a past time yet.

Meaning search embeds with Cohere Embed v4 at the place `DISPATCH_EMBEDDER` names:
AWS Bedrock (`bedrock`, the default, on the AWS SDK's own region and credentials) or Azure AI Foundry (`azure`, the `DISPATCH_AZURE_*` settings, with the deployment's API key or a keyless sign-in that exchanges a federated token with Microsoft Entra).
Bedrock with no AWS region logs `dispatch: meaning search
unavailable` and leaves meaning search off;
then, and whenever the query's embedding fails, a search answers by keyword alone with `degraded: embedder_unavailable`.
A local harness embeds without a model service:
`DISPATCH_DEV_EMBEDDER=1`, which the server reads only beside `DISPATCH_DEV_SIGNIN=1`, hashes a text's words into a vector in the process (`embed/dev.go`), so two texts rank near each other by the words they share and nothing else.
Each vector records its embedder's identity in `embeddings.model`, `us.cohere.embed-v4:0` on Bedrock and `azure:<endpoint host>/<deployment>` on Azure, and search compares a query vector only with stored vectors of its own identity.
An issue's write enqueues its text through its table's trigger (0073, as 0140 replaced it) in the same transaction.
Every other kind is embedded in windows of its blocks, written through `embeddings_enqueue_window`, which also deletes the owner's windows the new set does not name.
No write writes them: a document's version write and a comment's, a message's or an ask's own write record the blocks and mark their owner owed its windows in the same transaction, and the block-index tick's window arm writes them within a tick, in a transaction of its own.
So a write holds its owner's lock for its own rows alone, and a comment's and a document's windows, and so their meaning-search hits, appear one reconciliation tick after the write rather than within it (INFRA-988).
A window holds at most 1,200 characters of block text, a heading starts a new one, and its text is the context that gives those blocks their meaning - the document's title and the heading above, the issue's title for a comment or a message, the question for an ask's part - and then the blocks themselves, so a passage at the end of a long document carries a vector of its own.
The block-index tick (Background loops, above) writes the windows of every owner whose blocks a write recorded since it last windowed it, gives windows to every owner that holds blocks and no window of them, and, once an owner's window carries a vector of its current text, deletes that owner's whole-text embeddings row.
An issue's text is its title and its spec, the latest version of its primary document (its title alone while that is blank), and a new version of the spec queues the issue again through `0140`'s trigger on `artifact_versions`, so meaning search, and the related-item check below, find an issue with a terse title by what its spec says.
The server also runs a coverage pass (`embedqueue.Cover`) once when it starts, so a deployment runs nothing by hand to embed what it already holds:
the pass adds a row for every searchable issue that has none (one written before the trigger existed, or with it off, as a restore or a replication writes), points a row whose issue's text changed with the trigger off at the current text, records, for every issue whose row then holds its current text, the inputs that text was built from (`recordIssueSources`), takes every vector another identity made out of the index and queues its item again, gives a row that died under another identity this identity's attempts once, and settles a dead row whose text has nothing to embed (empty or only white space) with no vector, as the queue settles a pending one.
The window kinds take those last three steps alone:
which windows exist is the block index's to say, so a row the pass invented would be embedded for ever at a text no reader can reach.
The two steps that act on another identity's rows wait until the configured embedder has answered:
an embedding this process's queue made has succeeded, or, when none has yet and such rows exist, the pass embeds a short fixed canary text and it succeeds.
Until then the pass leaves those vectors and dead rows as they are and logs one WARN naming the identity, the canary's error and how many of each it left, so a misnamed Azure deployment (which answers `404 DeploymentNotFound`) or a refused credential drops nothing, and a throttled canary waits too;
a pass with no such rows calls the embedder not at all.
After the boot pass those two steps run again, and so does the step that records issues' inputs (Background loops, above):
at once when this process's queue first embeds successfully after the pass skipped the two, and every `identityRecheckInterval` (10 minutes, plus up to a tenth again) for as long as the process runs, a canary first when the queue has not proved the embedder.
The rest of the pass runs again only at the next start:
a live write enqueues its own text through its trigger or through the block-index tick that windows its blocks, and what is written with the trigger off waits for that start.
Each step is a statement of at most 500 rows in its own short transaction, repeated until it changes nothing, and is safe beside a live write and beside another process's pass, so the pass takes no lease.
`GET /api/v1/search/coverage` reports what that leaves:
the identity, its place and model (all three null with no embedder), and in total and per kind the searchable items, how many carry a current vector of that identity, how many lack one, how many failed for good, and how long the oldest lacking item has waited.
The same report names, under `not_indexed`, every kind of node neither leg of search holds a row of — a component, a session, a document version, a result and a handoff — each with how many of it the database holds, read from the table that holds it.
A kind nothing indexes is named rather than left out, so a reader cannot read it as a kind that is covered with nothing left to embed; it carries no current, lacking or failed count, and it is in no total.
The report is one statement, so every count reads one snapshot, and it tests no window's text:
a window kind's rows are read through two partial indexes, `embeddings_window_embedded` (a row embedded at its current text, current when its model is the identity) and `embeddings_window_unembedded` (every other row, never current), whose predicates hold the test that leaves out a text with nothing to embed, so Postgres runs that test at each write of a row rather than at each read.
Both predicates test the kind by a regex that matches the window kinds alone (`kind ~ '^(document_block|comment_block|message_block|ask_part)$'`), not by `kind in (...)`:
a list of kinds is a condition that other indexes of `embeddings` read (`embeddings_pkey`, `embeddings_kind_project`, `embeddings_window_owner`), and no btree index reads that regex.
The statement reads both halves in one arm whose condition is the two predicates joined by OR, which no single window index answers, so Postgres reads the pair by one bitmap heap scan over a BitmapOr of both indexes:
it reads each heap page that holds a window once, whatever the visibility map holds, and runs the blank test on no window.
The arm groups its rows by the kind's position in the list of window kinds rather than by `kind`, so no index's order is of use to it either.
Each index is keyed on `kind` alone and carries no other column, since that heap scan reads every column the statement needs from the row anyway.
The report's transaction sets `enable_seqscan = off`, `enable_sort = off`, `max_parallel_workers_per_gather = 0`, `jit = off` and `work_mem = '128MB'` before the statement (`embedqueue`'s `planCoverage`), and reads no statistics to decide anything:
with sequential scans off, that bitmap is the only way left to read the window rows, so the plan does not rest on the planner's estimates, which are wrong for this table (both predicates compare `embedded_hash` with `content_hash`, which the planner cannot estimate);
its cost is every entry of both indexes and each heap page that holds a window once, whatever the visibility map holds, whatever the statistics record and whatever share of the windows is embedded, as after an embedder change drops every vector;
the larger `work_mem` keeps the bitmap exact, so no page is checked against the predicates' regex again;
and the other three settings take the planner's remaining choices by estimate, a sort, parallel workers and JIT, away from it.
Index scans stay on, so the issues' lookups keep theirs.
`packages/envoy/scripts/coverage-scale.sh` measures the statement at production's size and twice it in six states of `embeddings`: freshly vacuumed, a third of its pages written since and analyzed, every page written since and analyzed, every page written since while the statistics still record about half of them all-visible, and after an embedder change has dropped every vector, first with those statistics and then analyzed.
An issue's vector is judged against the inputs the pass last recorded for it in `embedding_issue_sources` (the id and row version of the latest version of its primary document, its row's `content_hash` and its title):
where they are still the issue's own, its text is the one its row's `content_hash` is the hash of, and only an issue whose inputs changed since they were recorded, or that has none recorded, has its text built and hashed, as the pass builds it (an issue whose title is empty, or starts with white space or with anything but printable ASCII, also builds its text, to test whether it has anything to embed).
So an issue edited since the record step last ran costs the report time, not a wrong count (`embedqueue`'s `issueSource` names the one theoretical exception).
The record step writes `embedding_issue_sources` alone, never a row of `embeddings`, and records an issue only where its row holds the hash of its current text, so an issue whose row is stale is hashed by the report until a boot pass repairs the row and, after the repair, records it.
The statement runs in a read-only transaction under a `statement_timeout` of 10 s:
past it the statement counts nothing and the route answers `503 SEARCH_COVERAGE_TIMEOUT`, to be asked again later.
The bound is the database's own so that it ends the statement while its caller still waits (the coverage probe gives up at 30 s), which then gets that answer rather than a cut connection and a `500`;
and it holds where the caller's own cancel cannot arrive: pgx answers a caller's context that ends mid-statement by sending Postgres a CancelRequest, which needs a connection of its own, and a saturated server may refuse one.
The boot pass reads the same report, under the same bound, for its closing log line;
a report that fails there while the process still runs is logged at WARN (`dispatch embedqueue: coverage pass report failed`, with what the pass changed) and the pass returns, since every step before it is done, rather than failing and running again whole every `identityRecheckInterval`.
The `backfill-embeddings` subcommand, which did the pass's first job by hand, is gone, with its checkpoint table (0119).

One embedder identity is active at a time, the one row of `embedder_active` (migration `20261101030538_embedder_active`, `internal/dispatch/embedqueue/active.go`).
Each process registers its embedder's identity in `server_processes` (`embedder_identity`), and the active identity stays active while any live process carries it, so a restart or a roll of the processes that carry it keeps it.
When no live process carries it, the identity of the earliest-started live process that carries one becomes active (a compare-and-set, logged at INFO with both identities);
with none, the identity of the process that looks, and with no row yet, the first process to look records its own.
Only a process whose identity is active drops other identities' vectors, re-admits rows dead under another identity, embeds queue rows and reserves tokens;
any other process leaves every row as it is, and its coverage pass logs `identityStepsSkipped=true` with the `activeIdentity`.
Its meaning search embeds the query with its own identity and finds no vector of it, so it answers with its text matches only, not degraded and not an error, and its coverage route counts for its own identity.
Two processes reserving with different token ceilings take no more than one process with the larger ceiling would.

Moving a deployment to another place or Azure deployment is a release like any other.
While both builds serve, the processes of the old identity keep it active and embed, and the processes of the new one answer meaning search by text alone.
Once the last process of the old identity has left the registry, the new identity becomes active:
its pass drops every vector the old identity made, and search finds by meaning only what the new identity has embedded, and the coverage route counts the rest as lacking, until the pass and the queue have re-embedded it at the place's background pace;
a new place that cannot embed leaves the old vectors in the database, unused by search, until it can.
A model change on Azure takes a new deployment name, since the identity names the deployment, not the model behind it.

Every match is ranked on one list across the kinds, by one score, rather than on a list of its own merged with the others by the position it holds there.
Each leg contributes its own score — the keyword leg's `ts_rank_cd` with length normalisation (flag 32), the meaning leg's cosine similarity — weighted onto the one scale the two can be added on (`searchKeywordWeight` 1, `searchMeaningWeight` 0.4), and the sum is divided by 61, the score reciprocal rank fusion gave the first row of a list, which is the scale `rank` has been published on since LEGION-386.
A row both legs find scores the sum of both, which is above what either leg alone gives it.
No kind is ranked ahead of another, in the legs or in the page: a kind leads only by scoring highest.
Hits that score alike are ordered by recency and id, so the order is total and consecutive `offset`s walk it once while the corpus holds still.
An issue the query names by key leads the page by `searchOwnKeyLead`, which is above every score the legs can reach together.
A query that is one issue key is read in each form a person writes it (`searchKeyForms`): `INFRA-1042`, `infra-1042`, `INFRA 1042` with a space where the hyphen is, and the number alone.
Only the first two match an issue's own vector, which holds its key as one hyphenated lexeme (`'infra' <-> '-1042'`), so the statement is given the keys themselves (`q.own_keys`) and its issue arm matches an issue named by key as well as one its tsquery finds.
The number alone names the issue of that number in the project the request reads it in, or in every project at once, and then every one of them heads the list, in project order, before any other row.
That project is `project` when the request narrows by one, since an issue the filter leaves out cannot be on the page whatever else a request says, and otherwise `number_project`, the project of the page the search was made from.
`number_project` narrows nothing: a search carrying it answers every project's rows, as a search carrying nothing does, and the answer's `filters` never echoes it.
It is the one thing the page a person searches from decides, so the bar can search everything from an issue page and still put that issue first for its own number.
The forms are read in Go rather than by a pattern inside the statement, because the statement then matches a list of keys against the issues primary key, which an index answers, where a pattern tested over every row of `issues` is a scan of that table on every search.
A number alone is the one form that costs a read of its own (`searchOwnIssueKeys`), since only `issues` knows which projects hold that number.
Each leg keeps its best `contracts.SearchKindDepth` (100) hits:
the answer's `total` counts every match of every kind, and `reachable` the hits the pages can return, so an `offset` at or past `reachable` answers no hits with both counts.

Search snippets are escaped text with only server-inserted `<mark>` elements around matches.
Native issue creation rejects a near copy of an open issue of the same project with `409 POSSIBLE_DUPLICATE` and up to five candidates;
`force` bypasses that refusal, and external references skip the check.
An issue title is at most 1,000 characters (`contracts.IssueTitleMax`, UTF-16 units after trimming), on creation and on a retitle, and a longer one is `400 CAP_EXCEEDED` before the near-copy check runs.
The check scores the project's issues whose status is not `done`, less the new issue's ancestors (its parent and every issue above it), on the share of the new text's rarest words that a candidate's title vector and its primary document's latest vector hold together:
the terms are the related-item check's own (`pickRelatedTerms`, `api/related_terms.go`), and the statement drives from them into `issues.search` and `document_search`, so each term is one probe of a GIN index and no candidate's spec vector is read more than once per term.
Every lexeme an issue key contributes is dropped from those terms, so a spec that cites the nearest issue it ruled out is not refused for citing it.
Because no key is scored, titles filed from one template, one for each issue they concern, would score as copies of each other;
so the scoring statement leaves a candidate out when both titles name issue keys (`[A-Z][A-Z0-9]{1,9}-[0-9]+`, which also reads a word such as `SHA-256`) and the new title names neither the candidate's own key nor any key the candidate's title names (`duplicateNamesAnotherIssue`, a condition inside the statement, before its limit of five candidates).
A key in a spec does not count.
A text whose picked terms number fewer than `duplicateMinTerms` (3), or that holds no more than three lexemes of its own, is not scored at all:
a share of one or two terms is 1.0 against any issue that holds them.
Such a text is judged by equality instead (`duplicateEqualLexemesQuery`):
the gate refuses an open issue of the project, outside the new issue's ancestors, whose title lexemes and whose primary document's lexemes are exactly the new title's and the new spec's as Dispatch would store it, and it names nothing else.
A candidate at or above `duplicateRefuseScore` refuses the create once the refusal's row in `near_copy_checks` is known to be written;
when that row cannot be written, the create goes in unchecked and its `201` names the candidate in `advice.suggestions.near_copy_check` (below);
the closest one at or above `duplicateNudgeScore` and below the refusal is named to the filer as `advice.suggestions.likely_duplicate`, which refuses nothing.
Both bars are set by `TestNearCopyRefusalReplay` over a frozen export of a project's history (`scripts/near-copy-replay.sh`), and a change to the term picker or to the scoring statement invalidates that calibration.
A candidate's snippet marks the words its title shares with the new one, and the `POSSIBLE_DUPLICATE` message quotes a candidate's whole title.
The check runs once for every create, forced or not, in a read transaction of its own bounded by `duplicateGateTimeout` (2 s) on the context and by `statement_timeout` inside it:
one bound, because with two, a read that ends between the two deadlines would refuse an unforced create while recording for a forced one that the check did not answer.
A create whose check does not answer is written either way, forced or not, and the `201` says so in `advice.suggestions.near_copy_check`:
the create went in without the near-copy check an unforced create otherwise meets.
A forced one also records `forced` with an empty `past` on its `issue.created` event, which is how the weekly audit tells it from an override.

Each answer the gate gives a create is one row of `near_copy_checks` (`api/near_copy_checks.go`), read through `GET /api/v1/near-copy-checks` and `GET /api/v1/near-copy-checks/{id}`:
`refused`, `forced`, `nudged`, `passed` or `not_made`, with the reason a check was not made (`bound`, `failed` or `unrecorded`), the candidates it read with their scores, and, for a refused create alone, the spec it refused.
Each row also holds how long the create waited on the gate's read, its transaction's rollback included (`read_ms`), and the step the read was in when it answered or ended (`read_stage`: `connect`, taking a pool connection and opening the read transaction; `terms`, picking the new text's rarest words; `keys`, reading the lexemes the project's issue keys give; or `score`, scoring the project's open issues, which every answered read ends in), so a `bound` or `failed` row says at which step its read was waiting;
both are null on a row written before Dispatch recorded them (`20261125005337_near_copy_checks_read_time`).
A refusal is answered only once its row is known to be written, so every `409 POSSIBLE_DUPLICATE` has its row;
otherwise the refusal refuses nothing, and the create goes in as `not_made`, reason `unrecorded`, its row written in its own transaction under the id the refusal used, so it replaces a refused row whose insert landed unseen.
Every other create's row is written after the commit, bounded to one second (`nearCopyRecordTimeout`), and tried once more in the background for up to 30 s (`retryRecord`) when a timeout or a lock stopped it, as a `related_checks` row is;
a row that still cannot be written is logged, and the create stands.

`POST /api/v1/issues`, `POST /api/v1/issues/{key}/asks` and `POST /api/v1/artifacts/{id}/asks` run the related-item check once the write has committed, and so does each decision block a document edit (`POST /api/v1/artifacts/{id}/edits`) adds, on the block's question (the check replaced LEGION-550's 300 ms keyword suggestions).
The check never refuses the write, but the response waits for it, at most 5 s (`relatedCheckAllowance`);
an edit's blocks are checked together under that one bound, the first five of them, and each block past five is answered as not made.
It searches the write's own project in two stages started together:

- Stage 1 picks the text's rarest words and runs search's keyword query over them.
  Each lexeme is scored by its count in the text times its idf, BM25's `ln(1 + (N − df + 0.5)/(df + 0.5))`, where df is how many of the project's searchable items hold it and N how many items the project has.
  At most 25 are kept, highest score first;
  a lexeme no other item holds, one held by more than a tenth of the project's items and more than 10, a number and a hash are dropped, and a word whose df would take the chosen words' summed df past 3,000 is skipped for the next by score, which bounds how many rows the OR of them matches and so its time.
  The query's keyword arms are ranked by idf-weighted coordination, the sum of the idf of the chosen words an item holds plus a thousandth of `ts_rank_cd` to order items holding the same words, where search ranks them by `ts_rank_cd` alone, so an item holding three of the text's rare words outranks one repeating one of them.
  The rest of the query, the per-kind lists and their fusion, is search's own.

- Stage 2, when an embedder is configured, embeds the text and runs search's meaning query, which fuses the same keyword arms with the nearest vectors.
  If it answers within 3 s of the check's start, its result is the check's (searched by meaning and keywords);
  otherwise stage 1's is (searched by keywords only, with the reason).
  If stage 1 has not answered when the 5 s end, the check was not made, for time (reason `timeout`);
  if the request that ran it ends first (its client leaving, for a write sent without an `Idempotency-Key`;
  for a keyed write, which no client's leaving ends, the keyed run's own 10-minute bound), it was not made, cancelled (reason `cancelled`), which says nothing of the search's time.

df and N come from `search_term_stats` and `search_term_totals` (`0139`), which the term-statistics refresh (`api.RunSearchTermStatsRefresh`, lease `search-term-stats`) keeps up to date for every project at boot and every 10 minutes; the check only reads them.
Each pass applies what changed: the triggers of `20261101182522` queue every item whose search text a write changed, with the vector it held before, and a pass subtracts that vector, adds what the item holds now, and moves the item count by the items that appeared less those that were there
— the counts a rebuild from every item would write, for a fraction of its work.
A pass counts a project from its items again where no pass of this build has counted it:
a project created since the last pass, a project whose row another writer touched (`20261101182515`), and every project on the first pass after this release is deployed.
A pass drains a project's whole queue, every row its own snapshot sees, before it moves `refreshed_at`;
a change committed after that snapshot waits for the next pass, so no change is applied twice and none is lost.
Each project a pass changes is logged on a line of its own, with `mode=step` or `mode=full` and how many items it applied.
A word written since the last pass is counted live, at most 50 per check, and a project with no statistics yet starts a refresh of its own in the background and has every word counted live meanwhile.

The result, under `advice.suggestions` (for a decision block, in `advice.decision_block_checks`, one `{block_id, question, suggestions}` per block in document order), always says what the check did.
`outcome` is `found`, `none` or `not_made`;
`search` is `meaning_and_keywords` or `keywords_only`, with `search_note` saying why keywords only, and is absent when the check was not made;
`missing` is the reason exactly when it was not made (the text holds no word to search for, the search behind it did not answer within 5s, the request that ran it ended before the check finished, the search behind it failed, …), so a client built before `outcome` still says so;
and `search_query` is two or three of the words it used, spelled as the title spells them, for a reader's own `dispatch search`.
`related` holds the three items most like what was filed, one per issue or document, each ask item carrying `ask`, where it stands (`open`;
`answered`, with its latest answer, who gave it, when and the options chosen;
or `resolved`, with the resolution and the last answer a person gave before it, if any).
`other_projects` rides beside `related`, never inside it, on a write that asks a person a question: an ask, a new decision block, and `POST /api/v1/projects/{key}/related`.
It holds at most two answered question asks of projects other than the one asked for, each carrying its `ask` state, its answer and the project it stands in (`owner.project`), found by meaning alone and offered with no claim that they answer the question.
An ask is offered only when it is a question, somebody answered it, the answer carries text, and it scores at or above the meaning floor the meaning search already uses.
An issue create and the `GET` form answer none, so a client that predates the field reads what it read before, and the dashboard's new-issue dialog is unchanged.
The leg runs beside the asking project's own meaning query, on a connection of its own, and reuses the one embedding the check already made.
The check waits for it at most 300 ms after that query answered, and never past the meaning search's own allowance, holding the asking project's answer while it waits, so the wait never costs the check its meaning result;
a leg that is slower than that, that fails, or that finds nothing leaves the asking project's result whole, with no cross-project item and no reason recorded.
An issue create forced past the near-copy refusal also names those issues in `title_matches`, and its `issue.created` event records them under `forced.past`;
an unforced create names its likely duplicate in `likely_duplicate` instead, and never fills `title_matches`.
The search leaves out everything the write's owner holds
— the new issue and its spec, or the issue or project document an ask or a block sits on and everything inside it
— so nothing is ever suggested that the writer already has.
Up to three open issues take the places before any item of another kind, and everything a `done` issue owns comes last, since an issue is what a filer can act on and keyword search otherwise puts an issue already closed as a duplicate above the open issue it was closed into.
`GET` and `POST /api/v1/projects/{key}/related` (`dispatch related`) both run the check on a title and a text and write nothing; the `POST` form answers `other_projects` and the `GET` form does not.

Every check of a write is recorded in `related_checks` (`0139`):
its outcome, how it searched, the reason, the words, and its keyword, meaning and total milliseconds.
`meaning_ms` is the whole meaning stage, so for a question's check it holds the wait for the other projects' answers too, up to 300 ms after the asking project's own query answered.
The row is written once the write that ran the check has committed, in a transaction of its own.
`GET /api/v1/related-checks` counts them by source, outcome, search mode and reason, with the keyword and total times' p50, p95, maximum and how many took more than 300 ms, within `[from, to)` and, when given, one project and one source kind.
It also answers `other_project_offers`, how many ask creates of the window offered at least one answered question of another project: a count of writes and not of items, read from `write_suggestions` under the same filters, and zero for a source kind no such row carries.
The two records are written at different moments and only the check's own row is retried, so a count at a window's edge can name one fewer write than the window's total:

```bash
printf 'Authorization: Bearer %s' "$(cat "$DISPATCH_TOKEN_FILE")" | curl -sf -H @- "$DISPATCH_URL/api/v1/related-checks?project=<KEY>&from=<time>&to=<time>"
```

Every suggestion an issue's or an ask's check offers is recorded in `write_suggestions`, whose `outcome` the server's outcome sweep (`api.RunSuggestionOutcomeSweep`) advances from `ignored` to `acted_on` (the suggested item was cited from the source, or the suggested issue was updated directly) or `overridden` (the source got further activity instead)
— a count of how often the suggestion was right, without refusing anything on its strength.
Each item of `other_projects` is recorded there too, under the role `other_project` and with its rank, which migration `20261111212235_write_suggestions_other_project` admits beside `related` and `decision`;
only an ask create writes such a row, because the table keys a row by the ask the check ran for and a decision block's check holds the document block's own id and no ask id.
No check writes the role `decision` any more, and the rows written under it before stay as they are.
The sweep is woken by event commits and judges a suggestion once the events that can resolve it are a minute old, reading only the events written since its last pass (Background loops, above);
a pending suggestion nothing writes to costs it nothing.
A ranking or latency change is measured on a restored copy of the production corpus:
`packages/envoy/scripts/corpus-copy.sh` restores one, `search-smoke.sh` prints what a searcher sees for a list of queries from any build, and `search-latency-compare.sh` runs `TestSearchLatencyOnCorpus` from a base checkout and this one alternately and gates on the difference (`packages/envoy/scripts/README.md`, "Measuring search on a copy of the corpus").

## Answering a question

Where the server generates text is one setting, `DISPATCH_ANSWERER`, and the model the command bar decides with is `DISPATCH_DECISION_MODEL` and its endpoint and token file (Required configuration, above).
`GET /api/v1/going/capability` reports what the bar can do here, each part apart, so a deployment that answers and cannot decide is told from one that does neither:
`{"answer": {"configured", "model", "blocks"}, "decision": {"configured", "model", "reachable"}, "session": {"configured"}, "label": {"configured"}}`.
`answer.model` is the model the answerer was configured with (`DISPATCH_AZURE_CHAT_DEPLOYMENT` or `DISPATCH_CHAT_MODEL`), `""` with none; `answer.blocks` is `true` once this process's model accepted a structured reply, `false` once its service refused a structured request with a 400 (after which no run asks for one until a restart), and `"unknown"` while neither has happened or with no answerer.
`decision.configured` is a measured decision model, a name the record does not hold reading `false` with `model` `""`; `decision.reachable` is whether its token file reads now, one file read and no call to the model.
`session` and `label` are the two new-issue settings (`GET /api/v1/settings/new-issue`).
The route names the models and no endpoint and no credential, so any caller Dispatch admits may read it.

`POST /api/v1/going/requests` takes a question, the project to ask it of and the item the bar's context chip names (`{path, project, issue, block}`), records the run and answers `201` with it;
the answer itself arrives on `GET /api/v1/going/requests/{id}/stream` as server-sent events, each frame carrying `id:` its sequence, `event:` its name (`decision`, `text`, `item`, `cut`, `changed`, `none`, `block`, `roster`, `end`, `error`) and `data:` that frame's own JSON (`packages/contracts/src/going.ts` declares the vocabulary once, for this server and for the page that reads it).
The two are separate requests because the server wraps every `POST` in its idempotency layer and that layer holds a whole answer rather than streaming it (Keyed writes, below), and because a deployment of more than one pod spreads two requests across pods:
the run's frames are rows of `going_events`, so any pod serves any reader, and a reader that drops resumes with `Last-Event-ID` after the last sequence it saw.
A repeat of the `POST` under one `Idempotency-Key` answers the first run's id, and its stream replays the same stored frames.
A run belongs to the credential that asked it, which the stream compares its reader with, so the `POST` refuses a credential that names nobody
— the deployment's shared token, which resolves as `{}` — with `403 GOING_ASKER_NAMES_NOBODY` and records nothing:
no reader of such a run could ever be served it.

What the model is given is what the caller may already read:
the hits of the server's own search of that project, and each hit's own stored text, at most 2,000 runes a hit and 30,000 in one run, after which a hit is still named to the model with no body and stays something the answer may cite.
The model may search twice more through one tool, the server supplying the project, the limit and the offset so it cannot widen the search, and a fourth search is refused back to the model.
It may also call the read tools, each made as the caller through the function its route answers with: the roster (`GET /api/v1/agents`), the progress of an issue or a project, a project's architecture counts, a project's events per ISO week (`GET /api/v1/projects/{key}/activity`) and the delivery figures, at most eight reads in one run.
The roster read gives each session the claims it holds on open issues (`claims`, each with its `issue` and `at`), and lists after the listener's sessions each session the listener lists no more that still holds a claim, with `state` `gone` and its claims, at most 50 sessions in all, the gone ones kept first; its totals count the gone sessions.
It reads only the claims on issues the run's person may read, so a claim on any other issue names neither that issue nor a gone session that holds nothing else.
The bar's agent reads the roster through `GET /api/v1/agents?limit=50` and is told that answer with the same claims and gone sessions; the route's own answer to every caller is unchanged.
A credential scoped to one project is offered no roster and no delivery read, and every other read is held to the run's project.
The citation rule is the server's and not the model's:
a text frame carries plain text and time links alone, every ASCII punctuation byte of the model's words escaped, which closes markdown's own constructs, and every word a renderer would link on its own
— a `www.` host, a scheme's `://`, a `mailto:` or `xmpp:`, an address
— dropped from the answer whole, since a renderer decodes an escape before it looks for such a link;
so an answer's links to items are its `item` frames and nothing else
— a link to something the model was given raises `item` once for that thing, every other link and every image is counted in `changed`, and either way no byte of the target reaches the reader, who is written the link's own visible text unless that text joins a word a renderer would link on its own, which is dropped whole as every such word is.
The one link a text frame carries is a time, `[5 days ago](dispatch-time:2026-11-18T11:35:00Z)`, whose target is `dispatch-time:` and an RFC 3339 time, which the page draws as a `<time>` with its full date on hover and which names no place to go:
it is passed on as the model wrote it and counted in nothing, its words escaped, judged and counted as every word is;
a target of that scheme that is no RFC 3339 time is reduced to its words as every other link is, and a time link whose first or last word is dropped, or that the cut falls inside, goes out as its words alone.
An answer is cut at 120 words with `cut`, one the model wrote no text for at all ends `none`, and either way `end` closes the stream.

A run has 30 seconds, and that window belongs to the record:
`going_requests.ends_at` is written by the `POST`, never held as a timer in the process running the answer.
A pod that dies mid-run therefore stores no terminal frame, and the next reader on any pod ends the stream itself with `error` `ANSWER_DID_NOT_ARRIVE`, as the maintenance sweep does for a run nobody reads (Background loops, above), which also deletes a run an hour old.
At that moment the running answer, a reader and the sweep may each try to end one run, so the table settles which of them does rather than whichever is first:
`going_events` carries a partial unique index over the run for a terminal frame name, every terminal write is an insert that does nothing on a conflict, and the writer that loses reads the stored frame and reports that one.
So a reader sees exactly one ending, never two and never none.

The delete of a comment or an issue message whose text a run gave its model ends that run's stream for every reader after it, as the stream route's description in `GET /api/v1` says (`goingWithdrawal`, `internal/dispatch/api/going.go`).

The short answer and its decision call stay in this server, each run bounded (`api/going_quick.go`).
The answer is one request of one person, a loop of at most 4 model calls with its searches and reads between them.
A run is its person's one question in flight, so a newer `POST` of the same credential ends the one before as its cancel does, its record marked cancelled;
a server answers 32 runs at once, and the next waits for a free slot within its 30 seconds, its decision call too, ending `ANSWER_DID_NOT_ARRIVE` where the window ends first;
the 30 seconds bound the whole loop, all its calls together, not each call; each call's stream is read up to 4 MiB, past which the run fails `ANSWER_FAILED`; and the answerer paces its calls to 25% of `DISPATCH_AZURE_CHAT_TOKENS_PER_MINUTE`, the agent's runs taking the rest: each call is charged a quarter of the characters of its prompt, its tool definitions and the tool output so far before it is made, and the tokens it spent past that, its reasoning among them, as it ends, which the next calls wait out (`going/pacer.go`, the agent's pacer too).

Words that are an issue's key, or the very key, title or name of a hit the keyword search finds for them, are the fast tier: the run stores that hit's `item` frame and `end`, and asks no model at all, neither the decision model nor the answer.
With a measured decision model configured, a run of any other words first makes one decision call, bounded at one second.
It asks three questions: which action of the catalogue (`GET /api/v1/going/catalogue`) the words ask for, among the actions the caller's credential may run, or `none`; whether the words are about the item the context chip names, items they name, or nothing; and chart or prose, which the run does not read.
The stream's first frame is then `decision`: the action chosen, the line on top of the bar it gives (`ask` for `none`, `file` for `dispatch_issue`, the line File as a new issue, and `do` for any other action), and the context.
The bar reads the line from that frame, and drops it where it comes 500 ms or more after the pause that asked it, so the line reads Ask.
A decision call that fails gives no `decision` frame, and the run answers as with no decision model.
The answer asks the model for a structured reply first: the answer and one block of a kind the words name.
A service that refuses that request with a `400`, or a reply that does not fit it, is asked again for prose, with no block; after a `400`, this process asks for prose alone until it restarts.
After the text frames come `block`, the one typed block the words asked for, which the server fills from its own read, and `roster`, `{age_ms, source}`, where the run read the roster.
The answer only reads: a change is made only by a run's approved plan (below).
`POST /api/v1/going/requests/{id}/cancel` stops a run its author typed past or closed: from then on the run stores no frame that does not end its stream, so the answer stops at its next delta on whichever pod is writing it.
The command bar sends it at the keystroke that types past the run, and its panel keeps what it drew of that run, marked as the answer to the earlier words, until the answer to the new words has text of its own, finds none or fails; no frame of the cancelled run reaches the panel after the keystroke, and emptying the box or closing the bar clears it.

A command-bar run changes nothing but through a plan its person approves (`api/going_plan.go`, `api/going_plan_run.go`).
The run's model proposes one with `propose_plan`, which the agent sends the server as a plan call: the server checks each change against its tool spec, refuses a change that needs a change after it, reads as the person the item each change writes, its value now and the holder of its issue's claim, and keeps the plan (`going_plans`, `going_plan_rows`), whose rows wait while the run waits.
A change names the issue an earlier create of its plan makes as `"$N"`, `N` that create's index, as an argument's value or an item of an argument's list, so a split of an issue is one plan: the new issues, and the moves of its sub-issues under them (`dispatch_issue_update` with `parent` `"$0"` and `needs` `[0]`).
The plan is refused where change `N` is no create of an issue (`dispatch_issue`) before the change, is not among its needs, or where `"$N"` names the item the change changes or sits inside an object; when the change runs, the server puts in the key the create's landing recorded, so its write, the read of its item before it and its undo name the issue itself, while the plan keeps the `"$N"` the person approved, which the page shows as the new issue's title and "(new)".
A comment, a suggestion or a document edit whose place a `dispatch://` reference gives (`ref`) is kept as the row of the change that names that place, the reference read as the server reads one (`text.ParseRef`): an issue's reference as the issue, a reference to an issue's document, its spec included, as the issue and the document, and one to a project's document as the project and the document, a suggestion given an issue alone going on its spec.
A change the bar can never send as written is refused there, whatever the words, since none of it needs a read: a comment, a question or a message carrying picture files (`images`), a comment, a suggestion or a document edit whose `ref` names no place it goes (no reference, a block, an ask, a comment, a message, a component, a file of a conversation or a version of a document) or another place than the change names besides it, an upload of a file (`path`) or with no text, an update with a closing `reason` that does not mark the issue `done`, and a sync of a repository not written as `owner/name`.
A picture list, a reference or a file path written empty names none, so it refuses nothing: the change is sent as it would be without it, since the bar sends none of the three on.
The refusal says what the bar cannot send and what the person at the bar can do there - ask again for the words alone, naming the issue or the document, with the text to upload in the question, and so on - naming no tool, no argument, no `dispatch` command and no flag.
`GET /api/v1/going/runs/{id}/plan` answers the run's latest plan to its person alone, each waiting row read again as them: the tool, the arguments, the rows it needs, the item, the fields and their value now, the claim, why it waits in every mode, and `changed` for a row whose item changed after the person saw it.
A row of an edit or a delete of a comment or an issue message (`dispatch_edit_comment`, `dispatch_delete_comment`, `dispatch_edit_message`, `dispatch_delete_message`) is a row of the post: its item is the post, its issue the post's, its one field `body`, and its value now `{author, body}`.
A comment or issue message reference with a short id is resolved within its named issue or project document before the command bar reads the plan row or sends the write.
The bar refuses a prefix that matches no post or more than one post.
Reopening a comment by a bare prefix resolves it among the comments the caller can read.
Once the post's author deletes it, the row's value now is the delete's mark, `{author, deleted_at, deleted_by}`, with no part of its body, and the row reads `deleted: true`, whatever its state, in every read of the plan (`maskDeletedPosts`).
The plan call's answer gives the run no value now of such a row: the run is given a post's words only by its own reads, each of which records the post as the run's (below).
`POST .../plan/approve` takes each row's tick and edited values; an untick unticks each row that needs it, and a ticked row whose item no longer reads as the plan showed waits again with the new value.
A ticked row whose post its author deleted after the plan read it is refused `409 POST_DELETED`, with nothing written: the person unticks it to approve the rest.
The approval moves the run's grant to the approving credential, a person's own sign-in or personal token holding the `approve` scope, and makes the run ready; the agent runs the approved rows in order, each as a call that names its row, and the server refuses every other write of the run (`WRITE_NOT_APPROVED`).
Before a row's first write its item is read again: a row whose item changed since its approval waits again (`ROW_WAITS`), and a row whose needed row failed or was skipped is skipped (`ROW_SKIPPED`); each row keeps the call key that ran it, so a second try of that key answers what landed and sends nothing twice.
`POST .../plan/revise` takes the person's words, which the run's next model call reads beside the plan's rows to propose another plan, and `POST .../plan/reject` ends the run done with nothing of the plan run.
`GET` and `PUT /api/v1/me/going/settings` read and write `mode`, the person's own setting, which the server holds (`user_going_settings`) and which is `ask` until the person sets it: in Ask every row waits; in Auto-review one model call of the run reads the person's words and the rows and lets through the changes of fields, links and claims and the creates with no text but a title that the words ask for, and in both modes an approval or a review in the person's name, an answer to an ask, a kept decision, a check result, a setting, a message or a broadcast, a change whose event the outbox would deliver to an agent session (`outbox.WouldDeliver`) and a change of an issue a session claims wait for the person.
An edit and a delete of a comment or an issue message change text, so each waits for the person in both modes.
`POST /api/v1/going/runs/{id}/undo` undoes one run as the credential that sends it, its run's person alone: each call of the run that landed a request (`going_landings`, phase `do`, with the row of the run's plan it ran), newest first, in-process, by the undo its tool states (`api/going_undo.go`).
`write_back` reads the item's value now and writes back the value the call found, field by field, only where the value now is the one the call left; a later value another writer left is kept and refused `409 GOING_UNDO_CHANGED`, naming what it holds and who changed that field after the call landed, and the rest is written back.
An issue's fields are written back so: `PATCH /api/v1/issues/{key}` answers the issue as the update left it, with `previous` beside it: each field the update names as the issue held it when the update took its lock, as the issue's read answers it (`advances_revision` beside `advances`), and for a rank the place the issue held, `{rank, after, before}`, beside `rank_place`, the place the update left it in.
An edit of a comment or an issue message is written back so: its `PATCH` route answers the body it replaced beside the post, `previous: {body}`, and the undo writes that body back through the same route while the post holds the body the edit left; a post its author deleted after the run is kept, named deleted and by whom, with no word of its body.
Who changed it is read from what came after the call's own landing, never from the item's newest event: for an issue's field, the newest event after the `last_seq` the call's route answered it left whose `changed` names that field; for an ask's part, the newest `ask.edited` begun after the landing that changed that part; for a post's body, the newest `comment.edited` or `message.edited` begun after the landing; for a document, the writer of the version it is at now, where that version was written after the landing; for an issue the run created, the newest change of its `status` or `closed_at` after the create.
Where no event after the landing names a writer, the undo says "another writer" and the field's `by` is absent, rather than naming who wrote a comment, a claim or another field since.
For a kept deletion, issue move or document version, Undo can return `by` with no `now` value.
`reverse` runs the call's reverse: a claim released, a release taken back, a follow unfollowed, a resolved thread reopened, an opened ask retracted, and a suggestion of the run rejected while it is still open.
`close_created` closes the issue the call created, with no message and no comment, only while its status is the one the create left it in; `not_undone` names the call "sent, not undone": a message, a broadcast, a review.
A delete of a comment or an issue message is `not_undone` too, named "deleted, not undone": nothing restores a deleted post.
What a call found and left is read off what its route answered of its own transaction, recorded beside its landing (`previous`, `rank_place`, `previous_token`, `previous_claim`, `claim_changed`, `opened`, and an issue's `last_seq`), never off a read since, so a run whose process ended after a write landed is undone all the same; a call whose record says nothing of what it left, or holds a value that does not decode, is refused `409 GOING_UNDO_AFTER_UNREAD`, and a document edit whose approval holds no read of the document, a read of another version than the one the edit replaced, or a read that does not hold the document's content is refused `409 GOING_UNDO_BEFORE_UNREAD`, with nothing undone.
A claim, a release and an approval request are undone by what their route answered they did: a claim of an issue the caller already held, a release of one nobody held and an approval request that renewed an ask already open are undone by nothing, and a release of another holder's claim is refused `409 GOING_UNDO_NOT_CALLERS_CLAIM` naming that holder, with nothing written, since the bar cannot claim for them.
Every undo that puts a value back has the route it writes through decide, under the lock that write takes, that the item still holds what the run left: a claim is released only while the holder it left holds it, a release is taken back only while nobody holds the issue, an issue's fields and rank, an ask's text, a post's body and a document's content are written back only while each holds what the run left, a created issue is closed only while its status is the one the create left, and a thread is reopened only while it holds the resolution the run left; a change another writer makes between the undo's read and its write is kept.
A post's write back is refused `409 COMMENT_CHANGED` or `409 MESSAGE_CHANGED` where the post holds another body, and `409 POST_DELETED` where its author deleted it meanwhile; the undo keeps either.
A document's write back names the document it read by its slug, which is unique within its owner and never changes, and the upload route also checks that the slug finds that same document by its id, so a document made since under that slug is refused `409 DOCUMENT_CHANGED` and left as it is.
Each of these is one of the bar's in-process conditions, which its in-process caller carries to the route on the request's context and never in a body, so no client can send one and no build can drop one: a request off the network naming `expect`, `expect_sha256`, `expect_new`, `expect_holder`, `expect_unclaimed` or `expect_resolved_at` is answered as one that names nothing.
A request of the undo is marked as a write back, and a route refuses a write back that carries no condition it decides `500 GOING_CONDITION_MISSING`, writing nothing; an edit with no mark, as a person's own, is made as before.
An undo sends one request at the most, which lands in `going_landings` under the key of the call it undoes, phase `undo`, in its route's own transaction, with the line the undo answers: a second press, after a crash or beside the first, answers each call whose undo landed from that landing and sends nothing twice, and a request that committed and then answered 5xx is answered from its landing too.
Whether an undo landed is read from that landing, never from the status its route answered: a request answered below 300 that committed nothing, as a follow of a session that follows the ask already answers 204, is answered ok with "the undo's route answered that it changed nothing - the item already stood as the undo asked - so there is nothing to undo", never as reversed.
That answer is recorded as the call's undo landing in a statement of its own, marked `unchanged`, since the route's transaction recorded none, so a second press answers it from there and sends nothing again.
So is an undo that keeps a change another writer made since the run, `409 GOING_UNDO_CHANGED` with nothing written, whether its read found the change and it sent nothing, or its route refused the request for it: a second press answers it as kept from that landing and sends nothing.
A refused field says what became of it without naming the field, which the line holds beside it; the line's own outcome message names each field it speaks for.
Undo is offered while History lists the run's conversation: until the conversation's newest run is seven days old, and a run past that answers `404 GOING_RUN_NOT_FOUND`.

A run is a row of `going_runs` (`api/going_runs.go`, `api/going_run_stream.go`): its words and context chip, its level and mode, the line on top it was started from (`line`: `ask`, `do` or `file`, null for a follow-up typed in the box, the side panel's box or a Continue), its state, what it spent, its conversation (the id of the conversation's first run) and the run it follows up; what it changed is its landings.
A run's read says whether Undo can still take back a change of it, `undoable`, decided as the undo itself decides it from each call's record, by one function (`goingUndoer.plan`): true while a call of it landed a change whose undo has not landed, whose tool's undo is not `not_undone`, whose last landed request is its own, not a close's reason sent before it, and whose record says what it found and left and that it changed something; false for a run that changed nothing, and for one whose only such calls are a claim its route answered `claim_changed` false, a create answered 200 for an issue Dispatch already held, an approval request that renewed an ask already open, or a call whose record cannot be read, of which Undo sends nothing.
It also says when its Undo landed, `undone_at`, the newest undo landing of the run, null for a run never undone.
It counts its changes by the same function, so a page says what Undo did and will do without inferring it from those two: `undoable_count`, the calls Undo can still take back now, a call whose undo's request failed among them; `undone_count`, the calls an Undo took back, one for each undo landing whose line is ok but those marked `unchanged`; and `standing_count`, the calls Undo tried and could not take back, so their change stands: a refusal from the record with nothing sent (`GOING_UNDO_AFTER_UNREAD`, `GOING_UNDO_BEFORE_UNREAD`, `GOING_UNDO_UNKNOWN`, `GOING_UNDO_UNSERVED`, `GOING_UNDO_NOT_CALLERS_CLAIM`, or a record it cannot use), and an undo landing whose line is not ok, a change kept as another writer changed it, whole or in some fields.
A call that is sent, not undone by design (a message, a broadcast, a review, every write Dispatch does not undo), a delete of a comment or an issue message (deleted, not undone), a call that changed nothing, and one whose undo's request changed nothing, is in no count: the undo's lines already name the first two.
All of these are read in the statement that reads the run, so History reads no more for each run it lists.
Of the answer of an issue's update, create or claim, which carries the whole issue, and of an edit of a post, which carries the whole post, that statement reads only the members the decision reads (the item's key, `previous` and the fields it names, an edit's `body` among them, `rank_place`, `advances_revision`, a create's `title` and `status`, a claim's `claim_changed`, `claim` and `previous_claim`), so a page of History does not carry a whole issue or post for each call; every other answer is small and read whole.
The short answer of a pause in typing writes no run: it is a going request with its events.
`POST /api/v1/going/runs` starts a run as the request's own credential, a person's sign-in or personal agent token, and records its grant; a broker-issued token, the shared token and a service-account token are refused `403 RUN_NEEDS_PERSON_CREDENTIAL`, and a person with 20 runs queued `429 GOING_RUN_QUEUE_FULL`.
A follow-up names the run it continues, which must be the caller's own, the newest of its conversation (`409 GOING_RUN_NOT_LATEST`) and ended (`409 GOING_RUN_NOT_ENDED`).
`kind` `new_issue` starts the run of the bar's line File as a new issue (`api/going_new_issue.go`): a Fast run (`400 GOING_INPUT` for another level) whose one change is the create of an issue from the words, with the label and the route `GET /api/v1/settings/new-issue` answers, which the request that started the run approves, so it waits for nobody; its end frame names the issue (`issue`) and offers `undo`, and a run that filed nothing ends failed with `NEW_ISSUE_NOT_FILED` and why.
`GET /api/v1/going/runs/{id}` reads a run and `POST .../stop` stops it, its person alone; every other caller is answered `404 GOING_RUN_NOT_FOUND`.
`GET /api/v1/going/runs/{id}/events` streams the run's frames as server-sent events from the reader's `Last-Event-ID`, woken by `NOTIFY dispatch_going_run_events`, and ends once the run has ended and every frame is written; a stream with nothing to write for 5 minutes writes `keepalive` and ends.
One process serves at most 4 streams of one run and 20 of one person's runs, and refuses the next `429 GOING_RUN_READERS_FULL`.
`GET /api/v1/me/going-runs`, History, answers the caller's own conversations, each with its runs newest first, while the conversation's newest run is younger than seven days, with `?issue=` and `?q=`; the maintenance sweep deletes a conversation whole once its newest run is seven days old, and stops a run that works under no live lease longer than its working budget allows (`TIME_BUDGET`).
`GET /api/v1/me/going-plans` answers the caller's runs whose plan waits for them, which the Inbox shows; each change of what it lists appends `going_plan.updated` `{login}`, which names no owner and wakes nobody.

A run is withdrawn once a comment or an issue message whose words it was given is deleted, or its row is gone (R3 of `dispatch://INFRA-1523`, as the short answer is withdrawn by `going_request_posts`).
Each answer a call of the run gets records, before the agent is given it, each post whose words it carries in `going_run_posts` (`api/going_run_posts.go`, migration `20261124130831_going_run_posts`): a post named anywhere in the answer or in the request's path, or a comment whose accepted suggestion named a version of a document the read names, whose words a string of the answer holds four in a row or holds alone; an id with none of a post's words records nothing.
A follow-up's claim records as its own the posts of each earlier run whose turns it is handed, and hands on no turn of a withdrawn run.
Every read reads the verdict as it reads the run, and the delete writes nothing of runs.
The stream of a withdrawn run writes none of its frames and one `end` frame, `{state: stopped, reason: "ANSWER_WITHDRAWN: …", offers: []}`, under the id of the run's last end, and nothing to a reader at or past it.
Its record and History read it `stopped` with that reason, its words kept; its plan gives no summary, no arguments and no value now (`withdrawn: true`); `GET /api/v1/me/going-plans` leaves it out; and an approval or a revision of its plan is refused `409 GOING_RUN_WITHDRAWN`.
A withdrawn run that has not ended is stopped with that reason by its stream, its read, its plan's routes, its next call or its next claim, whichever comes first, so it stores nothing more.
What it stored keeps its words, and its Undo still works.
A run withdrawn by its own deletes alone is withdrawn with a reason of its own: each post recorded for it that no longer stands was deleted by a `dispatch_delete_comment` or `dispatch_delete_message` call of that same run, a landing of `DELETE` to the post's own path (`goingRunWithdrawerOf`).
Its reason is `ANSWER_WITHDRAWN_BY_RUN: Dispatch withdrew this answer because this run deleted a comment or message it read.`, which its record and History read, and which a run that has not ended is stopped with, as above.
Its stream writes its `plan` frames, which carry a plan's number and state and no word, then one `end` frame with that reason, and none of its other frames.
Its plan reads as an ended run's, not withdrawn: its rows and what each write answered, each deleted post's value now its delete's mark with no body, and no summary, which the run's model wrote and which may quote the post.
An approval or a revision of it is refused as of any ended run, `409 GOING_PLAN_NOT_WAITING`, and `GET /api/v1/me/going-plans` leaves it out.
A run where any other writer deleted a post it read is withdrawn with `ANSWER_WITHDRAWN` as above, whatever its own deletes did.

A run acts with its grant (`api/going_grant.go`, `going_run_grants`): whom the request that started the run, or that approved its plan, names and by what, with no secret.
A sign-in is recorded by its sessions' generation and its cookie's expiry, a sign-in through IAP by its login alone, a personal agent token by its row, and the header identity of a test server or a local deployment by its login.
Each call of the run is made as the person the grant resolves to at that call, checked against the store as it is then, as the person's own request would be: a sign-out, a sign-in past its expiry, a revoked token or one no longer its person's, and a person the deployment's membership no longer admits each refuse the call, and the run stops with that reason (`SESSION_ENDED`, `SIGN_IN_EXPIRED`, `TOKEN_REVOKED`, `MEMBERSHIP_ENDED`).
IAP signs an assertion for each request and nobody keeps it, so an IAP grant lasts until the run's working budget ends, and an approval through IAP records it anew.
A call under a token holds the scopes its row holds at that call, and a sign-in every person scope.
Each change of a run records the person as its actor, with `act.agent` `command-bar` and `act.run` the run's id, so the item's history reads "<person>, through the command bar".

A run's agent process (`envoy-dispatch agent`) makes every read and write of a run as a call to this server over a Unix socket (`going/agentwire`, `api/going_agent.go`): `agent.sock` in `$XDG_RUNTIME_DIR/dispatch-agent`, or in a directory of the process's own under the system's temporary directory where that is unset, each of mode 0700.
Each call carries the 32-byte secret the server made when it started, compared in constant time; a call without it, or with another one, is refused `UNAUTHORIZED`.
A run is a row of `going_runs`: a claim takes the oldest queued run, or a working one whose lease ran out, whose `shape` this build reads (it writes the highest it reads into `server_processes.going_run_shape`), under a fresh lease token for 30 s, with at most 3 runs working and 20 queued for one person.
A renew extends the leases for 30 s and is answered after 10 s, or at once when the person stops a run of them; a claim waits up to 25 s; both are woken by `NOTIFY dispatch_going_runs`.
The agent ends its waiting renew as a claim lands and renews again, naming the new lease too; the server logs at debug the answer that finds that renew's connection closed, and as a warning any other answer it cannot write.
Every step, frames call, call and end carries the lease token, which the server checks in the transaction that writes it, so an owner whose lease ended writes nothing.
The server runs at most 16 calls at once, each on a context of its own bounded at 60 s, keeps at most 1 MiB of an answer and marks the rest cut, and answers a route that panics `CALL_PANICKED` and serves on.
A write runs only as an approved row of the run's plan (`WRITE_NOT_APPROVED` otherwise), and each request it sends in-process writes its `going_landings` row in the route's own transaction, keyed by the run, the step, the call and the request's number.
A second try of a call answers each request that landed from its row and sends the rest.
In this process a second try waits for a first try still running, at most 60 s, then answers from its rows or fails `CALL_UNKNOWN`.
In another process it does not wait: it runs, its landing's insert waits on the first try's row at its commit, and once the first try commits its change rolls back and it answers from the first try's row.
A message's or a comment's delivery recovery point goes into its landing's `recovery`, and the delivery sweep sends a landed message that has no delivery attempt.
A GitHub read goes through the server's GitHub App, only for a repository Dispatch maps to a project the person can read; a web read goes only to an address in the person's words or in a tool result of the run, connects only to a public address, checked on each connection after the DNS answer and on each redirect, and stops at 1 MiB or 10 s, marked cut.
At shutdown, after the HTTP drain, the server answers no claim a run, answers every waiting renew at once, closes the agent's standard input, so the agent exits within 1 s, waits for the calls in flight until 5 s before its stop grace ends, closes the socket, and kills an agent still running 2 s later.

The server starts the agent itself (`cmd/dispatch/agent_supervisor.go`):
its own binary as `envoy-dispatch agent`, with an environment of `DISPATCH_AGENT_SOCKET`, `DISPATCH_AGENT_SLOTS`, `GOMAXPROCS` (4 at the most), `GOMEMLIMIT`, the chat model's settings (`DISPATCH_ANSWERER` and the `DISPATCH_AZURE_CHAT_*` and Azure sign-in settings, or the `DISPATCH_CHAT_*` ones) and the server's own `SSL_CERT_FILE` where it sets one, and nothing else, the secret on its standard input, which stays open, and each of its threads under the `SCHED_IDLE` scheduling policy.
Under it the kernel gives the agent a CPU only where no thread of the server's wants one and takes it back as soon as one wakes, where at nice 10 a woken server thread waited out the agent's slice (check c29 measured the server's threads waiting 224 ms in one second);
the threads the agent's runtime makes later take the policy from the thread that makes them, and the server goes over the threads again until it finds every one under it, so none is made by a thread under another;
where the kernel refuses `SCHED_IDLE` the threads run at nice 10 instead, and the server logs that once.
The agent exits when its standard input closes, so it never outlives the server.
After an exit the server starts it again 1 s later, then twice as long after each exit within a minute of its start, 30 s at the most, logging `the agent process exited; it starts again` with the wait.
It reads the agent's `VmRSS` every 50 ms and kills it with SIGKILL above its ceiling, logging `agent killed for memory` with the memory at the kill, and kills an agent that has renewed nothing for 30 s (three renewals), which is how a SIGSTOP or a stuck runtime is found.
Each exit frees at once the leases of the runs the agent held;
a run it held alone counts one kill (`going_runs.kills`), and fails `AGENT_KILLED` at the third, and the runs it held beside others are claimed one at a time, each alone, until each saves a new step or ends.
Two kills for memory within an hour that no run alone caused halve the agent's run slots, to 1 at the least, for an hour:
a claim of an agent that holds its slots takes no run.
The limits come from the container's memory limit L, read through automemlimit's reader (`memlimit.FromCgroup`:
cgroup v2's `memory.max` or cgroup v1's `memory.limit_in_bytes`, `cmd/dispatch/memlimits.go`):
the agent's ceiling is the smaller of 4 GiB and L/6, its `GOMEMLIMIT` 75% of that, and the server's own memory limit 90% of what L leaves after the ceiling, set at boot unless `GOMEMLIMIT` already sets one;
with no limit the ceiling is 4 GiB and the server sets none.
The socket's directory is `dispatch-agent` under `XDG_RUNTIME_DIR`, a row of the settings table that the server hands its API server, so the API package reads no environment.
The agent works 8 runs at once.
`DISPATCH_TEST_AGENT_CEILING`, under the test hooks, sets the ceiling in bytes for a test of the kill;
`DISPATCH_TEST_AGENT_GOMEMLIMIT` the agent's `GOMEMLIMIT`, `DISPATCH_TEST_AGENT_SLOTS` its slots, `DISPATCH_TEST_AGENT_PROCS` its `GOMAXPROCS`, 1 to 4, in place of the CPUs the server gives it, `DISPATCH_TEST_AGENT_HOOKS=1` the agent's own test hooks (`going/agent/hooks.go`, among them `test_soft_limit`, a tool that works the agent at its soft limit until its run ends), a collection of the agent's heap at each `SIGUSR1`, which it logs as `agent: collection N found B bytes live and G goroutines` (`collectAgentOnSignal` in `cmd/dispatch/memory_hooks.go`), and its runtime figures as they stand at each `SIGUSR2`, with no collection, which it logs as `agent: runtime figures` and the values of `agentFigureNames` (`reportAgentFiguresOnSignal`), and `DISPATCH_TEST_AGENT_CALL_HOOK` rules, apart by `;`, on routes that panic in, or hold before their route answers or after it, every call the server runs for the agent.
For the test of the memory group (Tests, below), `DISPATCH_TEST_AGENT_GROW_AFTER` makes the agent allocate as fast as it can, on as many goroutines as its `GOMAXPROCS`, from that long after each start until it is killed, and `DISPATCH_TEST_SERVER_HEAP` makes the server hold that many bytes on its heap from its boot (`cmd/dispatch/memory_hooks.go`).
`DISPATCH_TEST_AGENT_NOW`, an RFC 3339 time, under the test hooks, is the time the agent's runs tell their model as the time now, in place of the clock's at each run's turn, and the time from which the agent writes each time it gives the model relative to now (`<field>_shown`, below);
the server hands it on to its agent and reads it for nothing else, so its leases, timeouts and stored times stay on the clock.
`cmd/going-eval` sets it to its seed's `now`, so the seed's times, written for that day, read as of that day.

`/metrics` shows both processes: `envoy_dispatch_process_goroutines`, `envoy_dispatch_process_heap_live_bytes`, `envoy_dispatch_process_memory_total_bytes`, `envoy_dispatch_process_memory_limit_bytes` and `envoy_dispatch_process_start_time_seconds`, each with `process="server"` and `process="agent"` (the agent's runtime figures as its latest renew carried them, none before its first renew);
`envoy_dispatch_cache_bytes` with `cache` `read`, `display_tree` and `list_answer`;
`envoy_dispatch_going_runs` with `state` `working`, `queued` and `waiting`;
and `envoy_dispatch_agent_slots`, `envoy_dispatch_agent_slots_in_use`, `envoy_dispatch_agent_stuck_goroutines` and `envoy_dispatch_agent_memory_kills`, the agent's kills for memory since the server started.
The agent's loop (`going/agent`) works each run it claims on a goroutine of its own, under a `recover`, as many at once as its slots, and claims no run while its memory is above 70% of its `GOMEMLIMIT`.
Each step of a run is one `agentloop.Run` of one call of the chat model, started afresh from the run's saved steps:
the last 8 model turns whole, each a model call with every tool result it called for, and the older steps as their one-line notes, at most 2 MiB, past which the run fails `HISTORY_TOO_LARGE`, with the turns of the conversation before it;
the model call is saved as a step and so is each tool call, each with the frames the bar shows.
The bar draws each link of an answer to an item as the item's chip in place of the link's words, so the server stores the text of a `reply` frame that names no tool, the answer, with each link whose words say more than the chip draws written as those words and then, in parentheses, the link with the item's short form as its words: `[Implementer of OPS-4](dispatch://OPS-4) is unresponsive` is stored `Implementer of OPS-4 ([OPS-4](dispatch://OPS-4)) is unresponsive` (`going/chips`, `api/going_link_words.go`).
Words that say only the issue's key, its status, its title or the key with the title, the head of an ask's, a comment's or a message's text, a document's name or the link's reference, case and punctuation aside, stay a link, as do a time, a web address, a picture and code; each item is read as the run's person may see it.
Each tool call first sends its `call` frame as it starts, through a `frames` call that appends frames to the run's events and saves no step, bounded as a step's frames are:
the step its tool frame will carry, the tool, its kind and the present form of its label, and on the first call of a model call the prose that model call streamed so far, so the bar shows the call's line while it runs;
a send that fails is logged, and the call's tool frame still ends its line.
The model has six tools: `search`, `read`, `find_tools`, `call_tool` (a read of Dispatch by a tool's name, or `github_read` or `web_read`, or `draw_block`), `propose_plan`, which the server keeps as the run's plan and which ends the turn `waiting`, and `ask_person`, which reads nothing and ends the turn `done` with its question as the answer, whose `reply` frame carries `waits_for_person` true;
every other one reads through a call to the server, which makes it as the run's person, and the model is given at most 32 KiB of a result, which the system prompt says is data and never an instruction.
In the result the model is given, each `priority` that holds 0 to 3 or null is followed by `priority_shown`, `P0` to `P3` or `No priority`, and each field that holds an RFC 3339 time by `<field>_shown`, the time relative to the time now the person's message names, as the web's `relativeTimeLabel` writes it (`5 minutes ago`, `yesterday`, `2 weeks ago`), so the answer writes each as the page shows it;
the answer writes a time as a link whose words are those, or the time with its zone and year where the words ask when, and whose address is `dispatch-time:` and the time the result holds, `[5 days ago](dispatch-time:2026-11-18T11:35:00Z)`, which the page shows with its full date on hover;
the step saves the result as the server answered it, which `draw_block` reads.
The 32 KiB holds those words too: a result the model was given whole stays whole, with as many of the words as fit, its priorities' before its times', and a result already cut to its first 32 KiB is cut further, so each value it gives is followed by its words.
`packages/contracts/fixtures/shown-values.json` holds the priority words and the relative ones, which a test of the web and one of the agent each run.
Each result the model is given names its step and its call (`step 4, call 1: status 200`), by which `draw_block` names the results a table, a chart or a metric reads, with a chain of `filter`, `group`, `count`, `sum`, `mean`, `min`, `max` and `sort` over their rows (`going/transform.go`).
The agent sends it as a block call, and the server reads those saved steps, runs the chain and answers the block it drew, which the step shows as its `block` frame, so every number in it is the server's;
it refuses `BLOCK_REFUSED`, saying why, a spec that holds a number other than a step's step and call and a filter's value, which picks rows and is never drawn, a result cut at 1 MiB or saved only as the 32 KiB the model was given, and a list read whose named pages do not reach its last.
A chart that names no `x` or `y` draws the column of its chain's group and the one column its count, sum, mean, min or max writes, a table that names no `columns` draws its chain's columns in their order, and a spec whose chain leaves one of them open is refused naming the columns its chain makes;
a group by a field no row holds, such as `priority_shown`, is refused naming the field the rows hold, and a chart of one point is told to draw a metric or a chart over more values.
In Auto-review the agent then makes one model call that names, with `review_plan`, the rows the person's words ask for, saved as a step of kind `review`;
the server lets through those its mode policy lets through.
A run the person's approval made ready runs its approved rows before its next model call, each as a call that names its row, saved as a tool step;
the person's answer is a step of kind `person` the server saves, and the model reads it and each row's result as the person's next message.
The levels bound a run: Fast 6 model calls, 90 s of working time and 1,000 output tokens a call at low effort;
Standard 24, 5 min, 2,500, medium; Deep 80, 20 min, 5,000, high (`going/agent/levels.go`); time a run waits for its person counts against none.
The output bound is a call's `max_tokens`, which bounds its completion tokens alone: the model's reasoning is not cut at it.
At a bound the run ends `stopped` with `CALL_BUDGET`, `TIME_BUDGET` or `OUTPUT_TOKEN_BUDGET` and offers Continue:
its end frame (`going_run_events`, name `end`) carries `offers: ["continue"]`.
A `TIME_BUDGET` reason of a run whose pacer held its model calls for more than half its working time says so, `…, and the model was busy for 52s of it; …`, which the bar shows.
A run's `budget_used` also says where its time went, and bounds nothing:
`model_ms`, what its model calls took, each from its request to its last token;
`pace_ms`, how much of that the agent's pacer held the calls before they were sent;
`tool_ms`, what its tool calls took; and `queued_ms`, how long it waited in the queue, which its first claim writes and a later claim keeps.
Each `reply` frame carries its model call's `model_ms`, `pace_ms` (none where the pacer held nothing;
`model_ms - pace_ms` is the provider's own time), `first_token_ms` (none for a reply of tool calls alone), `input_tokens`, `output_tokens` (every token the provider generated for the reply, its reasoning among them, whether the provider counts the reasoning among `completion_tokens` or apart from them), `reasoning_tokens` (of the output tokens, those that were reasoning), `reserved_tokens` (the pacer's estimate of the call, a quarter of the characters of its prompt and of its tool definitions, and its level's output tokens and reasoning tokens, Fast 353, Standard 865, Deep 1,967, summed over its HTTP attempts, counted where no quota is set too) and `charged_tokens` (what the pacer charged the call in the end:
what it drew for it, or the tokens the call spent where they are more), and each `tool` frame its call's `ms`.
A reply in prose that would end a turn with no plan is first checked by one more call of the chat model, which holds back a reply that says a change was made or leaves out a change the words ask for (`going/agent/noplan.go`);
a new-issue run's prose is held back with no check.
A turn of a run started from the line Ask, or from no line, whose words the decision model Jev reads as Ask makes no check, since its words ask for no change:
as the turn of the run's words starts, the agent makes a `line` call (`agentwire.KindLine`) beside its first model call, so it adds no wait, and the server puts the run's words and page to Jev as the bar's own decision does and answers the line Jev's choice gives (`api/going_agent_line.go`).
A claim names the run's `line` and whether the server names a decision model (`decider`); with none, the agent asks Jev nothing.
A turn of a run started from Do or File, a turn the person's answer to a plan started, and a turn whose line call failed, answered no line or had not answered when the reply came are checked; a line call that fails is logged once and never fails the run.
A `reply` frame of prose that the check judged, held back or not, also carries `check_ms`, what that check's model call took, which the run's working time counts and its model time does not, and `check_tokens`, the tokens the check spent, its prompt's and its reply's;
one whose check Jev's Ask skipped carries `check_skipped: "jev_read_ask"` and neither;
and each that the guard decided on where it asked Jev carries `jev_line`, what Jev read the words as when it decided: `ask`, `do`, `file`, `none` for no line, or `late` for a line call still out.
Every other `reply` frame carries none of them.
A model call's response is read up to 8 MiB and its reply up to 512 KiB, past which the run fails `MODEL_REPLY_TOO_LARGE`;
a 429 is tried again after the time it names, and each 429 is logged at WARN, `agentloop: the model service answered 429, its rate limit`, with the deployment, the attempt, the wait before the retry and whether the call is made again, by the agent and by the server's short answers alike;
the agent paces its calls to 75% of `DISPATCH_AZURE_CHAT_TOKENS_PER_MINUTE`, and charges that share the tokens a call spent past its estimate as the call ends, which the next calls wait out.
A grant that ended stops the run with its reason; a membership the sign-in issuer could not confirm is waited on and the call made again.
A Stop answers the agent's waiting renew at once, and the agent cancels the run's context, a model stream in flight included.
A watchdog gives up a step that runs past its model call's time and 10 s:
the run's lease is renewed no more and its slot is free, and its goroutine counts stuck; at 3 the agent exits and the server starts it again.
A server that names no chat model has an agent that ends each run it claims `failed` with `NO_MODEL`.

## Keyed writes

Every `POST` and `PATCH` route takes an optional `Idempotency-Key` request header naming the intent behind the write, the same on every retry of it and new for every new one (the IETF `Idempotency-Key` draft; Google AIP-155).
The dashboard mints one per press and the `dispatch` CLI one per command's write, and both retry under it by one rule (`retriesUnderKey` in `@legion/contracts`):
a failed fetch, an attempt silent too long, a 5xx, a 408, a 429 and a `409 KEY_IN_FLIGHT`.
No route requires the header: a write without one runs as it always did, unkeyed, so a script or the Legion daemon's outbox keeps working.
`PUT` and `DELETE` take no key, being idempotent already.
A key is 1 to 128 letters, digits, `.`, `_`, `:` and `-`, bare or double-quoted, and belongs to the caller who sent it:
a person's canonical login (from the dashboard or through a personal token), a service token's subject, or the shared agent token.
Dispatch keeps each key's answer for 24 hours (`request_keys`, migration `0137`; the maintenance sweep deletes older rows, Background loops above).

A keyed write finishes on the server whatever its client does.
Its handler runs with the client's cancellation taken off, bounded at ten minutes (past which its work rolls back and the key is released, as for a 5xx) and counted in the server's background wait, so a shutdown lets it finish within its 30 s stop grace;
one still running past that dies with the process, and the lease handover below takes its key over once its lease lapses.
While it runs it renews its key's 60 s lease every 20 s, until its ten minutes are up, so a client that gave up and asks again is answered `409 KEY_IN_FLIGHT` for as long as the first run lives, then that run's stored answer;
only a run whose process died is taken over, once its lease lapses.
A write Dispatch takes longer to finish than its client waits still lands once.
A repeat while the first still runs is never spooled, so a retried upload is never written to disk while its first attempt runs:
its body is read off the connection and discarded, hashed on the way, and once it has arrived the key is read again and judged by that digest.
The first still running is `KEY_IN_FLIGHT`;
a different body is `KEY_REUSED`, whether the first has answered or not;
a first that answered meanwhile is replayed;
one that committed and lost its answer is `KEY_COMMITTED`;
and one that gave the key up meanwhile is `KEY_IN_FLIGHT` with `Retry-After: 1`, whose retry runs the request.
An `Expect: 100-continue` repeat is `KEY_IN_FLIGHT` at once, with nothing read.
The key is claimed, and its answer recorded, on a context the client's leaving does not end, bounded at 10 s.

Every other answer Dispatch gives before it has read a request's body - `KEY_INVALID`, the closed-issue upload refusal, `LOGIN_CHANGED`, a refused path parameter, a refusal before decoding - is written only once the rest of the body has been read and discarded, never spooled, up to the route's limit (26 MiB on the upload routes, 1,576,960 bytes - six times the 256 KiB handoff body cap, plus 4 KiB - on the handoff write, 1 MiB on every other).
A browser that sends a whole upload before it reads the answer (WebKit, Firefox) otherwise met the connection's close as a reset and never saw the answer.
A body past the limit is answered `413 CAP_EXCEEDED` (the upload routes and the handoff write) or `413 REQUEST_TOO_LARGE` in that answer's place, and the connection closes;
a `401` is the one early answer that stands, since a caller nobody resolves is refused before any limit is judged.
Both reads wait at most a minute for a body: one that stops arriving is given its answer with `Connection: close`.
An `Expect: 100-continue` request answered early is never asked for its body: Dispatch sends no `100 Continue`, sends the answer and closes the connection.

Each item below names a status and code, then its meaning.

- The first answer, with `Idempotency-Replayed: ?1`

  A repeat under a key whose first request finished, with the same method, route, path values, query, media type and body:
  the stored status, every header the first answer set and its body, and nothing runs again.
  What a repeat answers on each route, and when a kept answer is not replayed, is `IDEMPOTENCY_KEY_HEADER`'s description in `packages/contracts/src/dispatch-api-limits.ts`.

- The first status with `ANSWER_NOT_KEPT`

  A repeat whose first answer Dispatch gives no copy of: one larger than the 1 MiB a key keeps, or one that holds a comment or a message that was deleted or a document version name a delete replaced.
  What a caller does with it - what a request that wrote made is read back, and a call that wrote nothing may be sent again under a new key - is `IDEMPOTENCY_ANSWER_NOT_KEPT`'s description in `packages/contracts/src/dispatch-api-limits.ts`.

- `409 KEY_IN_FLIGHT`, `Retry-After: 1`

  A repeat while the key's first request still runs (its lease renewed every 20 s while it does); nothing ran, and the caller asks again under the same key.
  A repeat once the lease has lapsed, 60 s after the last renewal, takes over a key whose holder died before it committed anything, and runs the request.

- `409 KEY_COMMITTED`, no `Retry-After`

  A repeat whose first request committed a write but whose answer was lost:
  its lease lapsed in flight, or it ended in a 5xx or a panic after committing (every transaction a keyed request commits that wrote a row marks the key's `committed_at` in that same transaction;
  a lock-only `SELECT … FOR UPDATE` marks nothing, and a COMMIT refused after the mark leaves none).
  The write landed and is not run again; there is no answer to replay, so the caller reads what it wrote.
  Also a repeat of a keyed `POST /api/v1/me/agent-tokens` that minted, once that first request has finished, answer lost or not:
  the mint's answer carries the plaintext token, so it is never kept under the key, never replayed, and no second token is minted (a mint refused with a 4xx commits nothing, so its key is released and a repeat runs it again).
  That is the one route whose answer is not stored.

- `422 KEY_REUSED`

  The key was already used for a different request; the message names its method and path, and this request did not run.
  A changed request needs a new key.

- `400 KEY_INVALID`

  The header is not 1 to 128 of those characters, or was sent twice.

- `409 LOGIN_CHANGED`

  The write names its presser in `Dispatch-Login` (every write the page makes does, `POST`, `PATCH`, `PUT` and `DELETE` alike) and its caller is someone else, or no person at all (any bearer):
  nothing ran and the key was not read, so it is untouched.
  A caller that resolves to nobody (a lapsed cookie) gets the `401` written further in instead, the keyed layer's for a keyed write and the handler's for any other, and a write without the header is not checked.
  The page drops every write that login pressed.

- `400 REQUEST_BODY_UNREADABLE`

  The client did not finish sending the body the layer reads before the handler; nothing ran and the key was not claimed, so the same key may be sent again.

A 5xx, a 401, a 408 and a 429 are not stored:
the key is released and the next attempt runs the request again, unless the request had already committed a write, which makes that next attempt `409 KEY_COMMITTED` at once.
Four kinds of write keep their own recovery point beside the key, so a repeat of `POST /api/v1/agents/{session_id}/messages`, `POST /api/v1/broadcasts`, `POST /api/v1/issues/{key}/messages`, or a comment (`POST /api/v1/issues/{key}/comments`, `POST /api/v1/artifacts/{id}/comments`) answers what it made as it stands now, under the first's `201`, however the first request ended, and puts on its way whatever delivery a dead process left owed;
a broadcast's repeat answers at once and sends what it owes behind the answer.
A comment's or an issue message's replay reads every member fresh and keeps from the first answer only its `advice` and `ask_waiting_on`, and only when that answer was a 2xx.
Neither send route takes a body `idempotency_key` any more.
A status change (`POST /api/v1/issues/{key}/status-change`) keeps a recovery point beside its key as well (Routes, below).
A repeat of one that finished answers its `200` from the point, with each entry's `issue`, and the answer's `issue`, as its step's write answered it.
A repeat that claims the key of one that stopped before it marked the point finished runs only the writes that the point does not hold, and its `changed` lists the point's entries first.
A command-bar run's Undo (`POST /api/v1/going/runs/{id}/undo`) keeps a recovery point too, the run it walks, bound before the walk sends anything: a walk commits as it goes and is idempotent by its landings, so a repeat under the key, however the first press ended, walks the run again as the key's person and answers its lines under `200`, finishing what a press whose process ended mid-walk left, where it would otherwise be told `409 KEY_COMMITTED`.

A delivery a dead process left owed is made whether anyone asks again or not.
Every write that delivers to sessions claims each attempt before its send and settles it after, on a context its client's cancellation does not reach.
The maintenance loop's pass, once a minute, first settles `failed`, with its receipt, every owed attempt nothing can send:
a mention attempt whose comment no longer holds the mention (`not
sent: the comment no longer mentions this target`) or whose document is no longer a project's (`not sent: the document is gone`), each checked again under its owner's lock so a comment that holds its mention again keeps its attempt owed.
A closed issue is not one of them: a message and a comment on a closed issue are delivered like any other, so an attempt stranded on an issue that closed under it is sent, not settled.
A document that is no longer a project's owns no receipt, so that settle fails and each pass logs one `level=ERROR msg="dispatch: settle an owed delivery nothing can send"` per such attempt, which stays pending and out of every send batch until an operator restores its document or deletes its rows.
It then sends each attempt that is still pending under a claim that has lapsed, is its message's or mention's latest, and was opened over a minute ago and inside the 72-hour duplicate window (`DELIVERY_DUPLICATE_WINDOW_MS`), the exact attempt it found under its own attempt number, so the listener drops a frame that did land;
and the first attempt of a keyed send whose process died between committing its message and opening that attempt.
An attempt that cannot be sent never takes a place in a send batch.
A send to one session answers once its own attempt has settled, which keeps a person's sends to one session in the order they pressed them;
a broadcast answers once its messages are committed, with its deliveries behind the answer.

The layer reads a keyed body before its handler runs, spooling at most the route's own limit:
26 MiB for the three upload routes (`POST /api/v1/issues/{key}/artifacts`, `/projects/{key}/artifacts`, `/agents/{session_id}/artifacts`), 1,576,960 bytes for the handoff write (`POST /api/v1/issues/{key}/handoffs/{phase}`), 1 MiB for every other.
A body past it is answered its route's `413` by the layer itself, and the handler does not run.
A body the client did not finish sending is `400 REQUEST_BODY_UNREADABLE`;
one Dispatch cannot spool to its temporary directory is Dispatch's own failure, a 5xx with the key echoed, which the client asks again.

Every answer the layer judged echoes the key back (`Idempotency-Key: <key>`) - not the `400 KEY_INVALID`, the refusal of a caller it could not resolve, or the `413` of a body past its route's limit, each of which it answers before it judges the key and none of which runs the handler - and `/healthz` answers `idempotency_keys: true` on every build that judges the header - a fact of the binary, so it is true whatever the database or NATS answer, on a `503` too.
A check tells a Dispatch that runs this layer from one that ignores the header, and would run a retried write a second time, by one or the other: the deploy check reads `/healthz` (`scripts/e2e/lib/dispatch-keyed-comment.ts`).
The agents' client reads no `/healthz`: every Dispatch that carries `b1c42d1d7d` runs this layer on every write route, so it sends a failed write again under its key and asks nothing first, whatever the echo says.
It reads the echo to settle a refusal: a fresh 4xx, or a `422 KEY_REUSED`, that echoes exactly its key is final (`answerEvent`, `packages/envoy-client/README.md`), so a change that drops or moves the echo turns such a refusal back into an unsettled write.
It also reads the echo to judge a 2xx: once an answer of the same write echoed the key, a 2xx without exactly that key is something in Dispatch's place, so a change that drops the echo from a 2xx alone turns that write's success into an unsettled write.

## Access log

Dispatch writes one line per served request, always, with no setting to turn it off.
It is written at INFO when the request ends, through the process's own logger:

```
time=2026-10-25T05:55:15.446Z level=INFO msg="dispatch: request" method=POST route=/api/v1/issues path=/api/v1/issues caller_kind=session caller=session-0123456789abcdef address=203.0.113.9 user_agent=dispatch-cli/1.0 status=201 elapsed_ms=10 bytes=412 credential=shared-token
```

Each item below names a field of the line, then what it holds.

- `method`

  The request's HTTP method.

- `route`

  The route's own pattern (`/api/v1/issues/{key}`), so one route's requests group together whatever ids their paths carry;
  `unmatched` for a path under `/api`, `/v1`, `/auth`, `/ws` or `/healthz` that no route answers.

- `path`

  The request path as it was sent, still escaped (`a%2Fb` stays `a%2Fb`), without its query string.

- `caller_kind`

  How the caller authenticated: `user` (a signed-in person), `session` (the session a bearer named in its `actor`, or the one a broker-issued token proved), `token` (a personal agent token with no session named), `service` (a verified service-account token), `broker-token` (an access token the secrets broker issued, on a line no session was named on), `shared-token` (`DISPATCH_AGENT_TOKEN` with no session named), or `anonymous` (a public route, a request whose credential was refused, or an `unmatched` path).

- `caller`

  Who that is: the person's lowercase email, the session id, the personal token's owner, the service-account subject, or the person a broker-issued token acts for.
  `shared-token` and `anonymous` name nobody.

- `address`

  The rightmost `X-Forwarded-For` entry, the address the load balancer in front of Dispatch saw, or the connection's own address when there is none.

- `peer`

  The connection's own address without its port: the hop that reached Dispatch.
  Unlike `address`, a caller cannot forge it, so a caller that reaches Dispatch directly is still named.

- `user_agent`

  The request's `User-Agent`.
  With `address`, it tells apart callers that share one credential.

- `status`

  The response status.
  A handler that panicked before writing is logged as `500`; the client gets a closed connection, not a status.

- `elapsed_ms`

  How long the request took, in milliseconds.
  A websocket's line is written when its connection ends, so it carries the connection's whole life.

- `bytes`

  The response body the handler wrote.
  A websocket upgrade is `101` with `0` bytes: its frames are written on the connection it took.

- `credential`

  Only on a `session` line: the credential behind the session.
  `token:<owner>`, `service:<subject>`, `shared-token` or `broker-token:<person>`.
  For every one but the last that session id is the caller's own text; under a broker-issued token the credential itself named it.

- `aborted`

  Only when the handler panicked (an artifact download whose store fails mid-stream aborts this way): `true`.
  The line is still written, and the panic still reaches the server.

The line never carries a token, a cookie, a query string, a request body or a response body.
The text in it a caller wrote, `method`, `path`, a session's `caller`, `address` and `user_agent`, is each cut to 256 bytes.

A websocket resolves its caller again on every heartbeat, and that resolution does not change the line: it names the caller the connection was admitted as.

Every route the server answers writes one:
every `/api/v1` route, the document websocket and the live socket (`/ws/live`), `/auth/*`, the GitHub proxy `/api/github/rest/`, and the 404 an `unmatched` path gets (a known path called with a method its route does not serve is one too).
The dashboard's own pages and assets write none, so a browser's page loads do not bury the callers.
`/healthz`, which a load balancer calls every few seconds, `/livez`, which a liveness or startup probe calls, and `/metrics`, which a scraper calls, write none either.
Three answers are given before any route runs and write none:
the `403` a cookie request from another origin gets, the `421` a request for another host gets on a server with local sign-in, and the `307` redirect Go's router gives an unclean path (`/api/v1//issues`) or a prefix route called without its trailing slash (`/api/github/rest`).
Match the line as `msg="dispatch: request"`: the previous-shared-token warning's message starts with the same words.
The line is one synchronous write to stderr per request, so a log pipe that blocks stalls requests.

## Routes

`GET /api/v1` (no credential) is the authoritative list:
every mounted `/api/v1` route with its `method`, `path`, `auth` (`public`, `any`, `human`, or `bearer`), and `description`, sorted by path then method.
`envoy-dispatch routes` prints the same body from the table alone, with no database or listener and never a test hook;
the docs site's HTTP API reference is generated from it.
`envoy-dispatch openapi` prints the OpenAPI 3.1 document of the same table with its test hooks in, each hook's operation marked `x-test-hook`, from the table, the Go types beside it (`internal/dispatch/api/routes_shapes.go`) and nothing else;
`--out <path>` writes it to a file and `--check` compares the committed `packages/contracts/schemas/dispatch-openapi.json` with a fresh render, printing a unified diff and exiting 1 when the two differ.
The clients' TypeScript types are rendered from that committed document, so a route added to the table reaches them with no type hand-written for it.
The list below is a summary.
An unknown path under `/api`, `/v1`, `/auth`, `/ws`, or `/healthz` is a JSON 404 `{"code":"NOT_FOUND","error":"no route for GET
/v1/issues","hint":"GET /api/v1 lists every route"}`, never the dashboard shell.
Every server process publishes its dashboard build's assets into the database at boot (Running more than one process, above), so a missing local file under `/assets/` is read whole from the database under the same key (`assets/<file>`) before Dispatch answers, through a cache of at most 64 MiB the process's requests share;
it never reads `index.html` or a non-asset path from there.
A path no build published remains `404 {"error":"not found"}` and any other read failure is a 502 with no cache header.
The dashboard's pages (`index.html` and the shell served for a browser route) carry `Cache-Control: no-cache` and an `ETag` of their bytes, no `Last-Modified`, and its hashed `/assets` files `public, max-age=31536000, immutable`, so a browser keeps no page from another build.


In this list, header identity means only the test/local harness mode described above.
Production Dispatch uses cookie identity through the sign-in pool.

Each item below names a path with its method and identity, then its purpose.

- `/api/v1` (GET; none)

  List every `/api/v1` route with its auth and purpose.

- `/api/v1/limits` (GET; cookie, trusted header, or bearer)

  The bounds this server holds a write to, each read from the constant that enforces it:
  `{block_units, item_blocks, ask_options, ask_units, event_body_units, request_bytes, document_bytes, handoff_bytes}`.
  `block_units` is the longest one block of a body may be and `item_blocks` the most blocks it may hold, `ask_options` the most options an ask may have and `ask_units` how long its question and options are together, `event_body_units` how much of a body one event carries whole, and the last three the bytes a request, a document and a handoff body may hold;
  the first five are UTF-16 code units or counts, the last three UTF-8 bytes.
  `dispatch limits` prints them, so no client carries a copy of a bound that can drift from what the server refuses.

- `/auth/start` (GET; none)

  Start Google sign-in at the sign-in issuer:
  the Cognito sign-in pool, or Google itself for `DISPATCH_SIGNIN_DOMAIN`, asking for offline access with `prompt=consent` and the domain as `hd`, with the sign-in's state in the signed `dispatch_oauth_state` cookie.
  `503 SIGNIN_UNCONFIGURED` on a server with no `DISPATCH_SIGNIN_*`;
  `503 SIGNIN_UNAVAILABLE`, and no cookie, while the issuer's discovery document has not been read.

- `/auth/callback` (GET; sign-in state)

  Exchange the issuer's code and sign in the person `DISPATCH_SIGNIN_GROUP` or `DISPATCH_SIGNIN_DOMAIN` admits:
  under a group, named by the email in their pool username, anyone outside the group refused with a 403 page naming them;
  under a domain, named by the verified `email` of a Google account whose `hd` is the domain, anyone else refused with a 403 page giving the reason.
  Record the person and the refresh token (and, under a domain, the account's `sub`, ending the address's sessions when another account held it), issue the cookie.
  Any process holding a key of the set completes a sign-in another process started.
  `400` "invalid or expired state" when the state cookie is missing, does not verify, has lapsed or names another state;
  `400` "missing code or state in callback", or "the callback's code is longer than 2048 bytes", before any request to the issuer, when the callback carries no code or state or a code longer than 2048 bytes;
  `503 SIGNIN_UNAVAILABLE` while this process has not read the issuer's discovery document, or when the issuer's token endpoint answers the exchange `5xx`, `408` or `429`, answers with a body cut off before it arrives whole, or does not answer;
  `502` for every other answer of the token endpoint (a `4xx` with or without an OAuth error, a `2xx` that names an error or holds no token, or a `3xx`) and every other failure of the sign-in;
  `409 SIGNIN_SUPERSEDED` and no cookie when, under a domain, another account signed in to the address between this sign-in's record and its cookie.

- `/auth/logout` (POST; cookie or trusted header)

  End the person's Dispatch sessions, forget their refresh token, and clear the session cookie.

- `/auth/whoami` (GET; cookie or trusted header)

  Return the resolved person, `{kind: "user", login, event_head}` with `login` their lowercase email and `event_head` an event id at or below which every event is final, read when the sign-in check answered.
  The page starts its events subscription from that id.
  The sign-in check comes first: a caller who is not signed in is `401 NO_IDENTITY` and the head is never read.
  A signed-in caller is `503 EVENT_HEAD_UNAVAILABLE` while no event id is proven final yet, which ends on its own and which the page asks again for, and `503 EVENT_HEAD_READ_FAILED` when the head cannot be read, which the page retries twice and then shows as an error.

- `/auth/_dev/signin` (GET; none; `DISPATCH_DEV_SIGNIN=1` only; loopback peer, no forwarding header)

  Sign in the person `login` names, lowercased, without the sign-in issuer: `302` with the session cookie to the page `next` names on the dashboard origin, as an absolute URL of that origin (default its `/`).
  `next` is a path, or an absolute URL of the dashboard origin.
  `400 DEV_SIGNIN_INPUT` without `login`, or for a `next` off the dashboard origin (another host, port or scheme, `//host`, userinfo, a relative path, a backslash), before anyone is recorded or any cookie minted; `403 DEV_SIGNIN_FORBIDDEN` for a non-loopback peer or a request carrying `Forwarded`, `X-Forwarded-For`, `X-Forwarded-Host`, `X-Forwarded-Proto`, `X-Real-IP` or `Via`.
  Each mint logs at WARN with the email and the peer.
  Not mounted otherwise.

- `/api/github/rest/...` (GET; cookie or trusted header)

  Read GitHub as the GitHub App installation that covers the repository:
  a pull request (`repos/{o}/{r}/pulls/{n}`), an issue (`repos/{o}/{r}/issues/{n}`), a commit's check runs (`repos/{o}/{r}/commits/{sha}/check-runs`) or a commit's workflow runs (`repos/{o}/{r}/actions/runs?head_sha={sha}`, the query being the full 40-hex `head_sha` and at most a numeric `per_page` and `page`, each once), the only reads the web app makes;
  the checks pill joins each check run to its workflow run for the workflow name and event, keeping the newest run of each check as GitHub's merge does and dropping the runs of an event GitHub does not evaluate for a pull request (only `push`, `pull_request`, `pull_request_review`, `pull_request_target`, `deployment` and `deployment_status` count).
  A workflow-runs answer of `200` is trimmed to `total_count` and, per run in GitHub's order, only `check_suite_id`, `event` and `name`, so a run's head commit (message, author), actor and repository never reach the browser;
  a body that does not decode is `502 GITHUB_UPSTREAM`, and any other status but a rate limit passes through unchanged.
  The pill needs the App's Actions: read, which the delivery reconcile already needs: without it `actions/runs` answers `403` and the pill disappears.
  Every person shares the App's credential, which reads far more of a repository than that, so any other method is `405 METHOD_NOT_ALLOWED`;
  any other path, a workflow-runs read with any other query, or a path under it (`actions/runs/{id}`, its jobs or logs) `404 GITHUB_PATH_REFUSED`;
  and a repository the App is not installed on, or no App key, `503 GITHUB_TOKEN_UNAVAILABLE`.
  A GitHub rate limit is `503 GITHUB_RATE_LIMITED` with `Retry-After` the whole seconds GitHub asked for (at least one), never `502 GITHUB_UPSTREAM` or GitHub's own `403`:
  GitHub's answer to the token mint or to the read, or a limit any process already met under the App or the installation, when GitHub is not asked at all (Running more than one process, above).

- `/healthz` (GET; none)

  The readiness probe: `200` while this process can serve requests, which is while Postgres answers within `store.healthProbeTimeout` (two seconds) on the health pool
  — a dedicated one-connection pool, never the shared one — and `ok` says the same.
  A database that stops answering is `503` with `db: false` inside that bound, never silence, and the reason is logged.
  It is not a liveness check (`/livez` is):
  it fails while the database is unreachable, so a liveness probe on it would restart every server process during a database failover.
  Every other field is reported and never decides `ok` or the status, and a deploy check that needs one asserts it itself (`nats: true`, `files: true`, `signin: true`):
  `nats`, whether the server's publisher is connected to NATS (`null` with `DISPATCH_NATS_DISABLED=1`), so a bus outage is `200` with `nats: false` while the process serves and its outbox waits for the bus;
  the client pings every second and declares a connection lost at two pings outstanding, so a silently cut bus reads `false` within two to three seconds;
  `files`, whether the file store's bucket answers its two-second probe (S3:
  `HeadBucket`; Cloud Storage: a metadata read of a key no upload writes, which a missing object answers healthy) where `DISPATCH_FILE_STORE_BUCKET` is set (`null` when it is not), so a bucket outage fails uploads and downloads and nothing else;
  `signin` and `service_tokens`, whether the sign-in issuer's and the service-account token issuer's discovery documents have been read (`false` before, `null` where that issuer is not configured).
  Two seconds fits the tightest prober here, the three-second compose healthcheck and deploy script, as well as the ALB's five.
  The body also names what is deployed: `commit`, the legion commit this process's image build stamped (the Dockerfile's `LEGION_COMMIT`;
  `null` in an unstamped build); `fleet_commit`, the commit of the latest announcement that every live server process serves one build, the one `server.started` carries (`null` when there is none, or before this process's announcer has first read the table, and up to one announcer pass, five seconds, behind it);
  and `schema_version`, the highest migration `schema_migrations` records, read by the same probe (`null` when `db` is false).
  `outbox_publisher` says whether this process publishes the events outbox now:
  `true` while it holds the outbox's publisher hold and the hold's deadline has not passed, `false` while it stands by for another process's, after it lost the hold, and with NATS disabled.
  One process of a deployment publishes at a time, so a healthy process may report `false`, and the field never decides `ok` or the status.
  `github` names each GitHub read Dispatch makes under a lease with its newest attempt (`githubreads`, `gitHubReads` in `main.go`), and it never decides `ok` or the status:
  a process serves everything but those reads while GitHub fails, so failing the probe would take every server process out of service for an outage each of them serves through.
  It is `null` where no GitHub App is configured (neither the `DISPATCH_APP_*` settings nor `app.json` name one, `loadAppCredentials`), and otherwise holds `webhook_redelivery`, `architecture_import`, `delivery_reconcile` and `ci_reconcile`, in that order, each `{state, at, cause}`:

  - `state` is `succeeded` when the newest recorded attempt sent requests to GitHub and noted no failure, `failed` when it noted one, `no_attempt` while the record holds no attempt of the read, `unknown` until this process's first read of the record succeeds, and `off` where this process does not run the read:
    all four without the App private key, and `webhook_redelivery` and `ci_reconcile` without NATS as well, since `webhookSweeper` and `ciReconciler` build nothing without a NATS client.
    `at` is when that attempt was recorded as its pass ended, by the database's clock, in RFC 3339 UTC, and is `null` unless the state is `succeeded` or `failed`.
    `cause` is the text of the attempt's first failure, made storable (`text.StorableReplacement`) and cut on a character boundary to at most 1,024 bytes (`githubreads.CauseLimit`), ending with ` … (cut from <n> bytes)` inside that limit when it was cut;
    it is `null` unless the state is `failed`, and the entry's Go type sets neither where the state carries none (`gitHubReadEntryOf`).

  - The probe sends the database nothing for `github` and waits on nothing:
    it answers from this process's copy of `github_read_attempts` (`githubreads.Copy`), read at start and again each time any process writes a row (Background loops, above), and a read that fails keeps the copy it had.
    So a missing table or a failed read of the record changes neither `db`, `ok` nor the status.
    During a roll the release before this one writes no row, so an entry can be an interval or more old, which `at` shows.

  - A cause can name a repository: the route takes no credential, a GitHub error carries the URL it asked, and Dispatch answers on its private network alone.

  - The delivery intakes read GitHub too, for each webhook event (`FetchPullRequest`, `FetchWorkflowRun` and `ListWorkflowRunJobs` in `internal/dispatch/delivery`), and have no entry of their own:
    they report through `delivery_reconcile`, since the delivery reconcile reads the same pull requests and runs with the same App every interval, so a GitHub failure of those reads shows there within one pass.

  - An attempt is one pass a loop runs under its lease, on the process that holds the lease (`duty.Exclusive`): one sweep, one import of every enabled source, one delivery reconcile pass or one CI reconcile pass.
    It opens in the function the lease runs and in nothing a route or a command also calls, so `envoy-dispatch redeliver-webhooks`, the Settings Refresh of a source and `dispatch architecture-sync` record nothing.
    It is recorded when the pass ends (`githubreads.Finish`), in place of the attempt before it:
    nothing when the pass sent GitHub nothing and noted no failure (the sweep before NATS connects, a CI reconcile that binds no CI record writer or covers no repository, an architecture import with no enabled source, a delivery reconcile with no settings and no pull request head to re-read), nothing when this process's shutdown cut the pass short, `failed` when a failure was noted, and `succeeded` otherwise.
    The write runs on a context of its own bounded at five seconds, so a CI reconcile pass that ends on its own bound is still recorded, and a failed write is logged at ERROR and changes nothing else of the pass.
    A CI reconcile pass can end `complete` in `ci_reconcile_passes` and record `failed` here: that row says how the pass ended, and this record says whether a GitHub read of it failed.

  - An attempt notes failures of two kinds, and both are listed here whole.
    A failure of the first kind is noted by the GitHub client on each request a pass sends (`githubapp.(*Client).send`, the one function that sends anything to GitHub, and `GraphQL`): the answers no caller takes as data.
    They are no answer (a transport error, the client's own `RequestTimeout` of 20 seconds ending while the caller's context is live, a body it could not read, a body past its limit), a 5xx or a 401, and a rate limit:
    an answer `RateLimit` reads as one (a 429, or a 403 that carries a limit), one `GraphQLRateLimit` reads (in a 200 too), or a limit `github_rate_limits` already holds, which stops the request before it is sent.
    The client notes no other 403, and no 404, 409 or 422: the same answer is data at one point and a failure at another, and only the loop's points below tell the two apart.
    It notes nothing for a request whose caller's context ended, by the pass's own bound or this process's shutdown.
    Such a cause reads as the client's own errors do: `<METHOD> <URL>: status <n>: <body>` for a 5xx or a 401, `<METHOD> <URL>: <error>` for a transport error or a body past the limit, `read <METHOD> <URL> response: <error>` for a body it could not read, `<METHOD> <URL>: rate-limited by GitHub (status <n>), next request in <wait>: <body>` for a limit GitHub answered, and, for a limit that held the request, the limit's scope and when it was recorded.
    Every text of a GitHub answer that a cause can carry ends at `status <n>` or at `<wait>`, with no `: ` after it, when the answer has an empty body (`githubapp.StatusText`): the client's notes above, the client's own errors for an answer it takes as a failure at a step a loop's read makes (the installation lookup, the token mint, `Ref`, `Dir`, `DirFiles`, `Deliveries` and `Redeliver`), `githubapp.StatusError` and `githubapp.RateLimitError`.
    Each of those texts that words an answer at a 2xx status and is no limit ends at `status <n>` too, whatever the body, with no marker in its place (`githubapp.StatusText`), and the text of a limit at a 2xx status ends at `<wait>`, or carries GitHub's wording of the limit after it (below), never its `Body` (`githubapp.RateLimitError`, whose text the client's note of a limit GitHub answered carries as it is):
    a 2xx body is the data the request asked for and not GitHub's refusal of it, as a 200 to the token mint, which expects 201, would carry the token.
    The client's errors of `CheckSource` and `Branch`, which only API routes call, and the CI reconcile's error for a query GitHub answered with a status other than 200, which the pass logs and replaces with a read of the repository's REST listing, noting that listing's own failure, follow both rules and reach no cause.
    The cause of a limit a query met at 200 carries GitHub's own wording of the limit in place of `<body>` (`graphQLLimitWording`): what of the envelope names the limit, the top-level `message` where it holds GitHub's secondary-limit wording and each `errors` entry of type `RATE_LIMITED` or whose message holds that wording, as `<type>: <message>`, joined by `; `, or, where nothing there names it (the answer that spends the last point carries its data and no error), the `X-RateLimit-Remaining`, `X-RateLimit-Reset` and `Retry-After` headers GitHub sent it with, each `<name>: <value>`.
    It never carries an `errors` entry about anything else, such as a repository the query could not resolve, since such an entry names another failure than the limit.
    It never carries the body's `data`: the body of a 200 is the query's answer, which can hold a private repository's pull requests or check runs, and this route takes no credential (`GraphQL`).
    The `githubapp.RateLimitError` the query's caller is given carries that wording in its text too, and never the data, since its text prints `Body` at no 2xx status whoever built the error, so a loop that notes that error in words of its own before the client's note is noted carries none of the data either.

  - Two loops' notes carry other text of a 200 answer, and each carries GitHub's own wording of an error in its `errors`, never the body's `data`.
    The CI reconcile's note of a head whose commit statuses it read none of (`statusesUnread`, below) reads `ci reconcile read no commit status of <owner>/<repo>#<n> at <sha>: <error>`, where `<error>` is the REST status read's error, or, for a query, each `errors` entry at or under the head commit's `status` field as `<TYPE> <message>`, joined by `; ` (`errorText`).
    The delivery reconcile's note of its merged pull request search (`recordRead` of `reconcile merged pull requests`) holds `page after "<cursor>": <message>` where a 200 search answer carries `errors`, `<message>` being the first entry's (`fetchSearchPage`).

  - A failure of the second kind is noted by each loop at each point where it gives up part of the GitHub read the deployment configured it to make, each a point where it already logs or records that failure;
    it counts whatever failed there, since a database error inside such a step ends that read too, and its cause is the loop's own error, which names the repository where there is one:
    the redelivery sweep where its listing of the App webhook's deliveries fails (`Sweep`), where GitHub refuses a redelivery request (`decide`, `RequestRefused`), and where a rate limit stops the sweep (`Report.RateLimitedUntil`);
    the CI reconcile where a repository it reads gets no token, no answer to its identity read, a name that is not `owner/name` or no listing of its open pull requests, where a project's default repository is one the App is not installed on (`appAbsent`, at every pass the fault stands for, the passes that hold GitHub's answer included), where a head cannot be read (`headFailed`, an answer too large included), where it reads none of a head's commit statuses (`statusesUnread`: any `errors` entry a query answers at or under the head commit's `status` field, whatever its type, and any failure of the REST status read but a rate limit and the pass's own end, a 403, 404 or 422, a 5xx, a transport error, an answer it cannot decode and a listing past its page bound included), and where a rate limit ends the pass, one in force at its start included (`Pass`), but not where the pass's own ending killed the read (`cutShort`);
    the architecture import where a source's sync fails at one of its four steps that read GitHub, `resolve` (the installation token), `Ref`, `Dir` or `DirFiles` (`Importer.syncOnce`), which covers the App not installed on the repository, a missing Contents permission and a branch GitHub does not have, but not where `DirFiles` refuses a note over `notes.MaxFileSize` (`notes.ErrFileTooLarge`);
    and the delivery reconcile where the deploy repository's permission check fails (`DeliveryPermissions`), where a step that reads GitHub returns an error (`recordRead` in `Reconcile.runOnce`: the merged, partial and head pull request steps, both workflows' runs, and the attribution inputs of stored pull requests), and where it gives up one row of such a step after the answers it takes as data: a head it cannot re-read (`reconcilePullRequestHeads`), a head attributed without its commit messages (`attributeHead`), a partial pull request it cannot complete (`completePartialPullRequest`, a 403 included) and a stored pull request whose attribution inputs it cannot read (`readAttributionInputs`).
    One failed request can be noted twice, once by the client and once by the loop, and the cause is the first noted.

  - These are no failure, and an attempt that meets only them stays `succeeded`:
    an answer a loop takes as data about one object and goes on, which is a delivery GitHub no longer has (a 404 to the redelivery request, marked `Expired`), a pull request or a workflow run that GitHub says is gone or whose repository no installation covers (404, 410 or `ErrNoInstallation`, marked unfetchable by `completePartialPullRequest`, the attribution read and the run jobs read), a pull request head GitHub no longer has or of a repository the App cannot read (`repositoryUnreadable`, stamped read), an architecture source's directory that GitHub answers `404` for at the commit, read as an empty model (`Dir`), a repository only an issue names that the App is not installed on (the CI reconcile's `appAbsent`, counted in `repositories_without_app`), and a `304`;
    a page the delivery reconcile asks for again after a 502, a 504 or a timeout (`retryGitHubPage`, at most three tries) and then reads, and a CI reconcile query whose repository the REST listing then reads, since each try and each query runs in a held scope of the attempt (`githubreads.Hold`), whose notes are dropped when the next try reads what it asked for and kept when it does not, so a page that fails every try counts by its last;
    a try of an architecture source's sync that another sync or a settings save superseded (`errSuperseded`), once a later try of the flight, which runs it again, reaches GitHub (`githubreads.Held.Reached`: it sends a request, or meets a failure that holds one unsent, a limit already recorded), since each try runs in a held scope whose notes are dropped then, and kept when the flight ends with no later try that reached GitHub (a retry that fails at its load of the row on the flight's spent deadline or on a database error), so the last try that reached GitHub counts;
    and an error outside the points above: of the database or NATS, which `db` and `nats` report (the sweep before NATS connects, the CI reconcile's check of a head's CI record), of the delivery reconcile's steps that read no GitHub (attributing stored pull requests, recording `issue.deployed` events), and of what an architecture source's files say (a note over `notes.MaxFileSize`, which `DirFiles` refuses by its listed size before any blob read or by a blob's decoded size, `notes.Parse`, a refused projection), which `architecture_sources.last_error` records.
    The delivery reconcile's runs backfills never fail its pass (`backfillRuns`), but the client still notes what they meet of the first kind.

- `/livez` (GET; none)

  Answer `204` with no body, reading nothing: it says only that the process answers.
  A liveness or startup probe asks it rather than `/healthz`, so a slow or unreachable Postgres, NATS or file store neither restarts the process nor adds load on every probe period.

- `/api/v1/events/log?after=&limit=` (GET; cookie, trusted header, or bearer)

  Page the whole event log by id, whatever owns each event (an issue, a document, a project or none):
  the events with id greater than `after` (default 0), oldest first, at most `limit` (1–1000, default 1000), each as the issue and document event lists serve it.
  An invalid value is `400 INVALID_QUERY`.

- `/metrics` (GET; none)

  The process's metrics in the Prometheus text format:
  `envoy_dispatch_outbox_publish_failures_total{reason}`, one count per destination of an outbox publish attempt that was not reached, `reason` being `denied` (the server refused it under the connection's own publish permissions), `refused` (NATS refuses the envelope or its subject however often it is retried), `unanswered` (NATS took the forward and the stream did not acknowledge the copy it stored, which is the whole of a forward's answer, for every holder alike) or `failed` (every other failure);
  and `envoy_dispatch_outbox_role_forwards_held_total{holder_state}`, one count per role forward a holder's resolution held pending with nothing sent, `holder_state` being `unresponsive` (the holder is past the live window), `undated` (nothing dates the holder) or `unresolved` (the role claim or the holder's session could not be read).
  A hold is a wait for that session to answer and no failure of the bus, so it is counted there and never under the publish failures an alert watches.
  It also carries four gauges of the copies of comment mentions that wait behind an earlier event of their owner, with no label, since an owner's key is neither bounded nor public and whoever reaches the port reads this endpoint: `envoy_dispatch_mention_sends_held` and `envoy_dispatch_mention_send_held_oldest_seconds`, the API's mention sends that wait and how long the oldest has (`api.registerHeldGauges`), and `envoy_dispatch_outbox_fallback_copies_held` and `envoy_dispatch_outbox_fallback_copy_held_oldest_seconds`, the same of the outbox's fallback copies (`heldCopies`).
  A held mention copy's wait has no bound, so beside the gauges a copy held `model.MentionCopyHeldWarnAfter` (one minute) logs one WARN naming its owner, the earlier event and that event's `publish_error`; the event's own failed publish logs `dispatch outbox: publish event` at ERROR at each attempt.
  `envoy_dispatch_document_replacements_unguarded_total`, with no label, counts the whole-document uploads that replaced a document naming no token for the version they replace, which Dispatch takes unchecked until every deployed `dispatch` client sends one (INFRA-4980); each also logs one INFO line naming the artifact and the document (`api.noteUnguardedReplacement`), and no caller: the access log's line for the same upload names the caller and its User-Agent, so the two are joined on the upload's path and time.
  `envoy_dispatch_file_replacements_unguarded_total`, with no label, counts the same for files and pictures: the uploads that replaced one naming no token for the version they replace, which Dispatch takes unchecked until every client, the dashboard's upload included, sends one; each also logs one INFO line naming the artifact and the file (`api.noteUnguardedFileReplacement`), and no caller.
  It is answered above the router, so it is never the dashboard's shell, and whoever reaches the HTTP port can read it.
  Nothing scrapes it yet.

- `/api/v1/artifacts/_test/quiesce` (POST; cookie, trusted header, or bearer; `DISPATCH_TEST_HOOKS=1`)

  Close every live document on every process and wait for the settlements in flight, answering once every process has; not mounted otherwise.

- `/api/v1/artifacts/_test/rooms` (GET; cookie, trusted header, or bearer; `DISPATCH_TEST_HOOKS=1`)

  List the documents this process holds a room of now: `{"rooms": [artifact id, ...]}`.
  Not mounted otherwise.

- `/api/v1/artifacts/{id}/_test/outside-schema` (POST; cookie, trusted header, or bearer; `DISPATCH_TEST_HOOKS=1`)

  Write the crafted malformed tree e2e uses; not mounted otherwise.

- `/api/v1/artifacts/{id}/_test/newer-content` (POST; cookie, trusted header, or bearer; `DISPATCH_TEST_HOOKS=1`)

  Append an element no build knows, so the document holds newer content (the cross-build browser test); not mounted otherwise.

- `/api/v1/artifacts/{id}/_test/room-counters` (GET; cookie, trusted header, or bearer; `DISPATCH_TEST_HOOKS=1`)

  What this process did with the document since the process started, and what its room holds now:
  `{"room_failures": n, "lock_waiting_compactions": n, "room_closes": n, "settlement_resumptions": [RFC 3339 time, ...], "awareness_clients": n}` - its room failures, lock-waiting compactions, closes of a room of the document by the service (a replaced history, an issue's close or reopen, a failed room's eviction, shutdown;
  not ygo's idle retirement), the time of each settlement-resumption pass that armed the document's owed settlement, and the awareness client entries its room of the document holds now (0 with no room here), which `maxAwarenessClientsPerRoom` bounds.
  Not mounted otherwise.

- `/api/v1/artifacts/{id}/_test/hold-persistence` (POST; cookie, trusted header, or bearer; `DISPATCH_TEST_HOOKS=1`)

  Take `{"duration": "<Go duration>"}` and hold every append of a browser update that this process's rooms of the document make, for that long from now;
  `204` once the hold is set, `400 DURATION_INPUT` for a duration that is not positive.
  Not mounted otherwise.

- `/api/v1/artifacts/{id}/_test/live-write-pauses/{point}` (PUT; cookie, trusted header, or bearer; `DISPATCH_TEST_HOOKS=1`)

  Park the document's next live write on this process at `{point}`: `after-first-append` (it holds the document's lock) or `after-commit` (before its publish).
  `204` once armed, `409 PAUSE_ARMED` when a pause is armed already, `400 PAUSE_POINT_INPUT` for another point.
  Not mounted otherwise.

- `/api/v1/artifacts/{id}/_test/live-write-pauses/{point}` (GET; cookie, trusted header, or bearer; `DISPATCH_TEST_HOOKS=1`)

  Wait up to `?wait` (a Go duration, 10 s when absent) for a write to park at the armed pause: `{"parked": true}`, or `{"parked": false}` when none did.
  `404 PAUSE_NOT_ARMED` when no pause is armed.
  Not mounted otherwise.

- `/api/v1/artifacts/{id}/_test/live-write-pauses/{point}` (DELETE; cookie, trusted header, or bearer; `DISPATCH_TEST_HOOKS=1`)

  Disarm the pause and let the write parked there continue: `{"parked": bool}`, whether a write had parked.
  `404 PAUSE_NOT_ARMED` when no pause is armed.
  Not mounted otherwise.

- `/api/v1/live/_test/disconnect` (POST; cookie, trusted header, or bearer; `DISPATCH_TEST_HOOKS=1`)

  Close every open live socket on every process with 1012, as a restart does; not mounted otherwise.

- `/api/v1/live/_test/rotate` (POST; cookie, trusted header, or bearer; `DISPATCH_TEST_HOOKS=1`)

  Close every open live socket on every process with 4001 (`max age`); not mounted otherwise.

- `/api/v1/agents/{session_id}/stream/_test/publish` (POST; cookie, trusted header, or bearer; needs the `person` scope; `DISPATCH_TEST_HOOKS=1`)

  Feed every process's in-memory agent relay;
  `as=drop` ends agent views as `dropped`, so the page subscribes again and rebuilds from the replay; not mounted otherwise.

- `/api/v1/writes/_test/slow` (POST; cookie, trusted header, or bearer; `DISPATCH_TEST_HOOKS=1`)

  `{method, path, delay_ms}` arms or replaces a delay for that method and exact request path, and `delay_ms: 0` removes it:
  every keyed run of it waits `delay_ms` on its handler's context once its key is claimed, and a context that ends during the wait answers `503 SLOW_WRITE_CUT_OFF` with the handler never run; a bad body is `400`.
  It arms the process the call reached, not every process.
  Not mounted otherwise.

- `/api/v1/settings/new-issue/_test` (POST; cookie, trusted header, or bearer; `DISPATCH_TEST_HOOKS=1`)

  `{label, session}` replaces what `GET /api/v1/settings/new-issue` answers on the process the call reached, in place of what that process's environment set; an empty value is none, and a bad body is `400`.
  A browser test reads the bar with a label and a session configured and then with the session cleared, which no deployment setting can change while the server runs.
  Not mounted otherwise.

- `/api/v1/messages/_test/queued` (GET; cookie, trusted header, or bearer; `DISPATCH_TEST_HOOKS=1`)

  List the targeted messages this process queued itself for a session that neither the listener's registry nor its known-session store held, oldest first, each as the envelope that session's own subject would hold:
  `{"queued": [{subject, dedupe_key, event_id, source, payload_summary, payload, urgency}]}`.
  A process with `DISPATCH_NATS_DISABLED=1` keeps them in its own memory, where this route reads them.
  A process with NATS publishes them to the stream, and answers `404 QUEUED_ON_BUS` here.
  Not mounted otherwise.

- `/ws/live` (GET; cookie, trusted header, or bearer)

  One WebSocket per browser tab, serving delivery, events, and agent subscriptions.
  A `legion_issue` view, `{kind: "legion_issue", issue}`, names one issue and nothing else, and its snapshot and deltas carry the body of `GET /api/v1/issues/{key}/legion`, as a `legion` view's carry the body of its project's board.
  A frame that names a project, a window, an `at` or a `query` beside the issue, or names no issue, is `INVALID_FRAME`.
  The view moves when the sampler reads the daemon of the issue's project, when `PUT` or `DELETE /api/v1/projects/{key}/legion/daemon` names or removes that daemon, and when a write of one of the issue's runs that stores something new commits (`dispatch_workflow_runs`).
  At each connection of the hub's listener to Postgres, the hub reads every `legion` and `legion_issue` view again, so a notification sent while the listener was down leaves no view stale.
  Dispatch never refuses a `legion_issue` view for a project that names no daemon: its body then says `configured` `false`.
  A well-formed view has one refusal, an `error` frame with the route's code `NOT_FOUND`, for a key that names no issue.
  An events subscription resumes after a decimal event id;
  its `events_start` says `replay_complete: true` when that id is at or below the log's head and the gap fits the replay's bounds (at most 256 events whose frames total at most 1 MiB), so every event after it is replayed before it goes live and a client may skip the refresh it would owe the gap, and `false` with no resume, past either bound, or above the head (a log that was reset).
  An events subscription that falls 64 events behind the relay catches up from the log within the same bounds and goes on;
  past them, or once the log is reset, it ends as `behind` and the page subscribes again.
  An agent view holds at most 8 MiB of queued frames and ends as `behind` past that.
  A page that stops reading meets the socket's own bounds first (#2449):
  the writer's 10 s write deadline (`liveSocketWriteWait`) ends the socket, which holds at most 16 of the hub's frames (`liveSocketQueue`) and 64 stream frames (`liveSocketStream`);
  the agent view's 8 MiB bounds only what it buffers while the socket writes slower than the session produces.
  A socket rotates with 4001 at a random age from 30 to 60 minutes, and under `DISPATCH_IDENTITY=iap` also once the IAP assertion its upgrade carried expires (reason `credential expired`, Identity, above);
  it closes with 1012 on restart, 1008 when its caller stops resolving, and 1013 when the hub queue fills.

- `/api/v1/inbox?project=&assignee=` (GET; cookie, trusted header, or bearer; needs the `person` scope)

  List open asks newest-first, including their issue key, title, and assignee.
  `assignee=me|unassigned|<email>` filters on the person each ask waits on — whoever it was handed to, the assignee of its issue until somebody hands it on, and the default assignee of its project while the issue holds nobody — so it keeps the asks waiting on the caller or on that person, and `unassigned` keeps the asks that chain names nobody for, which every project naming a default assignee leaves none of;
  an email nobody has signed in with is `400 ASSIGNEE_NOT_ALLOWED`.

- `/api/v1/me/answers?limit=&offset=` (GET; cookie, trusted header, or bearer; needs the `person` scope)

  List the caller's answers and replies on asks, newest first.
  A row is either an answer with `current`, or a reply, and carries the ask's question, kind, state, owner and deep link.
  Answers come from user-authored `ask.answered` events whose actor is the answer's user; a restored answered block adds no row.
  `limit` is 1–200 (default 50), `offset` is 0 or more, and an invalid value is `400 INVALID_QUERY`.

- `/api/v1/me/replies?kind=&state=&limit=&offset=&cursor=&issue=|artifact=` (GET; cookie, trusted header, or bearer; needs the `person` scope)

  List the caller's Inbox threads, in the state's own order, of the kinds `kind` names:
  `reply`, the default, the threads they wrote in (an ask, a comment thread, an issue message thread) where somebody else has written since they last read it, less an open ask on an open issue that waits on them, which the Inbox already lists;
  `agent`, their one-off messages to an agent session (a BTW, a broadcast's copy, or a Send or an Aside the session did not take as a turn of its live conversation) holding a reply from that session;
  `reply,agent`, both in one order.
  `state` is `unread`, the default and the Inbox count's own read, which lists the rows somebody else has written in since they read them, newest reply first;
  `inbox`, every row in their Inbox, read or not, less what they have dismissed, newest reply first;
  or `dismissed`, what they dismissed out of it, newest dismissal first, 200 rows at a time from `cursor`, which is the `next_cursor` the page before it answered.
  `limit` or `offset` under `dismissed`, and `cursor` under any other state, are each `400 INVALID_QUERY`.
  A row names the thread, its owner, its issue's priority, its project, its subject, its root's deep link, the newest reply's author, first line and time, the caller's read mark, whether it is unread, how many writes they have not read, and its dismissal mark when it has been dismissed.
  `issue` or `artifact` narrows to one owner (both is `400 INVALID_QUERY`, and either leaves out every agent row, which has neither).
  An absent `limit` is every row, which is what the Inbox reads; a `limit` the caller names is 1–200.
  The answer carries `as_of`, the database time of the one snapshot every row was read in, which is what Mark all read passes back as its read mark.

- `/api/v1/me/threads/{id}/read` (PUT; cookie, trusted header, or bearer; needs the `person` scope)

  Move the caller's read mark on one thread forward to `{read_through}`;
  it never moves back, a value more than a minute ahead of the server is `400 INVALID_STATE`, and a non-uuid id is `400 THREAD_ID_INPUT`.
  Answers `204`, writing nothing for a thread the caller never wrote in.

- `/api/v1/me/inbox/read` (POST; cookie, trusted header, or bearer; needs the `person` scope)

  Mark every unread Inbox thread read in one write:
  `{read_through}` (RFC3339, required; missing, unparsable or more than a minute ahead of the server is `400 INVALID_STATE`), with the Inbox's own narrowing
  — `kind` (`reply`, `agent`; both by default), `priority` (`P0`, `P1`, `P2`, `P3` and `none`), `urgency` (`low`, `med`, `high`, `blocking` and `none`), `project` (a project key and `none`), and `issue` or `artifact` — each absent list meaning every value.
  It marks exactly the threads `GET /api/v1/me/replies?state=unread` answers under that narrowing whose newest reply is at or before `read_through`, so a thread written in since stays unread and one already read past the mark keeps its later mark.
  Answers `{marked}`, how many threads it moved.

- `/api/v1/me/inbox/dismiss` (POST; cookie, trusted header, or bearer; needs the `person` scope)

  Take threads out of the caller's Inbox, or put them back.
  A thread stays in the Inbox, read or not, until it is dismissed, as an email stays until it is archived: reading it and Mark all read leave it there.
  The body names either `threads`, the Inbox's own row ids (`reply:<thread id>` for a Reply row, `agent:<root message id>` for an Agent row), or the narrowing Mark all read takes with `{dismiss_through}` (RFC3339, required there;
  missing, unparsable or more than a minute ahead of the server is `400 INVALID_STATE`), never both (`400 INVALID_QUERY`), and `restore`, which clears the marks instead of writing them.
  Under the narrowing it changes exactly the threads `GET /api/v1/me/replies` answers under it whose newest reply, or whose dismissal, is at or before that mark.
  A dismissal that names `threads` carries `seen_replies` beside them, one object per row it names and keyed by that same row id:
  what the page drew in that row, as `GET /api/v1/me/replies` answered it.
  A `reply:` row's object is `{version}`, the row's own `user_threads.version`;
  an `agent:` row has no `user_threads` row and so no version, and its object is `{newest_reply, unread_replies}`, the row's `reply.id` and its `unread_reply_ids` (which may be empty).
  The route writes a row's mark only while the row still holds what the page drew, so a dismissal buries no write nobody has read:
  a row's version moves on every write by somebody else, and why no comparison of this request's mark with the row's own times can stand in for that check is written on migration `20261105093501`'s increment.
  Seven more `400 INVALID_QUERY`: a row `threads` names that `seen_replies` leaves out, a key naming a row `threads` does not, a `reply:` entry that is not that row's one `version` (of the wrong type, or missing it, as an Agent entry in its place does; a member this build does not declare is ignored there, as it is in a body), an `agent:` entry that is not the `newest_reply` and `unread_replies` the page drew (the same three), an id that is not a uuid, `seen_replies` on a `restore` (which draws the rows back and checks nothing), and `seen_replies` with the narrowing, where the server draws the rows itself.
  A dismissal marks the thread read as well, so a dismissed thread counts as unread nowhere;
  a later write by somebody else brings it back unread, and the caller's own reply leaves it dismissed.
  An Ask row's id, a row id of neither shape and an id that is not a uuid are each `400 INVALID_QUERY`.
  Answers `{dismissed}`, how many threads it changed; a request that changes nothing answers 0 and appends no event.

- `/api/v1/agents?limit=` (GET; cookie, trusted header, or bearer)

  List live Envoy sessions (`session_id`, `title`, `dir`, `machine_id`, `roles`, `capabilities`, `last_seen`), newest first, from Dispatch's copy of the listener's session list, at most 5 s old;
  `503 ENVOY_REFILLING` with the listener's `Retry-After` (5 when it sends none) while the listener refuses its list as refilling (it cannot stand behind its copy of the sessions yet), and `503 ENVOY_UNAVAILABLE` when the listener cannot be reached, each for up to 5 s after that listener read without asking the listener again.
  Neither is an empty list: a client reads neither as "no agents".
  `limit`, a whole number of 1 or more (else `400 INVALID_QUERY`), lists that many of the newest seen sessions alone and reads the open asks and last activity of those alone; the command bar's roster read (`read_roster`) sends 50, so a run's read of a roster of any size loads no more rows than it answers.
  Without it every session is listed.

- `/api/v1/agents/{session_id}/answers?since=&max_chars=` (GET; cookie, trusted header, or bearer)

  What people wrote that Dispatch delivered to one session, newest first:
  each ask's current answer (`replaced` when it changed an earlier one), each decline of an ask (a question a person closed without answering it), each comment at its current body (`edited_at` when edited) and each message, once however it arrived:
  a person's `ask.answered`, `comment.created`, `message.created`, `message.answered` or `ask.resolved` event whose `published_destinations` hold the session's own topic, of which the list takes the declines alone, its sent or queued message deliveries, and its sent or queued comment deliveries (mentions).
  Never an agent's or the system's words, any other resolution of an ask or its hand-back, or a delivery to a `role:` route.
  Each item carries its `relation`, the `ask` and its question where it answers, declines or replies in one, whoever asked it, and its `owner`, `ref` and `href`.
  A session owns an ask when it wrote the ask or follows it, and only an owner is told that an ask is its own:
  an answer is `answered_your_ask` for an owner and `answered_an_ask` for any other session, a decline is `declined_your_ask` for an owner and `declined_an_ask` for any other session, and a reply in an ask's thread is `replied_on_your_ask` for an owner, which outranks `mentioned_you`.
  A session that does not own the ask reads a reply in its thread by the reason that delivered it: `mentioned_you` for a mention, else `replied_in_a_comment_thread`, `commented_on_a_document` or `commented_on_an_issue`, as for any other comment, when the issue's route, its claim or a credit on its primary document delivered it.
  A decline is the kind `declined`, and it carries its words in parts:
  `text` is the person's own note, empty where they wrote none, and `dispatch_words`, on `declined_your_ask` alone, the sentences Dispatch wrote beside it for the asking agent.
  `since` defaults to 72 hours ago (older than 7 days or in the future is `400 INVALID_QUERY`);
  `max_chars` (1–100,000) stops the list after the item whose words take the running sum past it, counting an item's `text` and the `dispatch_words` a decline carries beside it, `total` still counting every item;
  a bad `session_id` is `400 INVALID_SESSION_ID`.
  Not an authorization boundary: direct-message text already reaches every caller through `GET /api/v1/events`, and a person may read any session's list.

- `/api/v1/projects` (GET; cookie, trusted header, or bearer)

  List projects (`key`, `name`, `open_asks`, `gated_steps`, `proven_sessions`, `created_at`, and `default_repo` for a project with a default repository), ordered by key.
  `proven_sessions` is `true` while the project's proven-sessions setting is on (`/api/v1/projects/{key}/proven-sessions`, below).
  A project's default assignee is not listed: `GET /api/v1/settings/project-default-assignees`, which needs the `person` scope, is the one read of it.

- `/api/v1/projects` (POST; cookie, trusted header, or bearer)

  Create a project from `key`, `name` and an optional `default_assignee`, for a person or an agent session alike.
  Every project names a default assignee, the last step of the chain that decides who a question reaches, so the create takes the login `default_assignee` names, else the person creating the project, else the person its credential acts for.
  A call that can name none — a credential that acts for no person and names no `default_assignee` — is `400 INVALID_PROJECT` naming the field, and a login nobody has signed in with, whether the call names it or it is the person the credential acts for (a broker-issued token's owner comes from the broker, not from `people`), is `400 ASSIGNEE_NOT_ALLOWED`.
  The answer is the project plus the `default_assignee` it took; the `project.created` event carries the project alone.
  It also rejects a key that is not a project key, an empty name, a name holding a control character or a bidi embedding, override or isolate (U+202A-U+202E, U+2066-U+2069), and a name with no visible character (a letter, number, punctuation or symbol other than the blank fillers U+115F, U+1160, U+3164, U+FFA0 and U+2800, or a code point Go's Unicode tables leave unassigned, such as an emoji newer than them, other than a noncharacter (U+FDD0-U+FDEF and each code point ending in FFFE or FFFF) or a reserved default-ignorable (U+2065, U+FFF0-U+FFF8, U+E0000-U+E0FFF);
  combining marks, format characters, private-use characters and whitespace don't count) with `400 INVALID_PROJECT`, a name over 80 characters (`contracts.ProjectNameMax`, UTF-16 units after the trim) with `400 CAP_EXCEEDED`, and a duplicate key with `409 PROJECT_EXISTS`.

- `/api/v1/projects/{key}/proven-sessions` (PUT, DELETE; cookie, trusted header, or bearer; DELETE needs the `person` scope)

  Turn the project's proven-sessions setting (`projects.proven_sessions`) on with `PUT` or off with `DELETE`; a new project has it off.
  While it is on, an architect's write of an issue's spec or Verification document, and the Legion daemon's close, reopen or write that takes an issue out of Legion, pass only on a credential that proves the writer's session, a broker token of that session (`403 DOCUMENT_WRITER_NOT_ARCHITECT`, Document errors below).
  A role-holder publication then passes only on a credential that proves `legion-daemon:<project>`, and any other is `403 ROLE_HOLDER_NOT_PROVEN` (`/api/v1/issues/{key}/role-holders`, below).
  Turning it on makes a check stronger, so any credential that may write the project's issues may send the `PUT`, which declares no OAuth scope, and the project's daemon can send it with its own project-scoped token; a bearer names its session in `actor`.
  Turning it off makes a check weaker, so the `DELETE` needs the `person` scope and records the person: a credential the deployment configured, the shared token or a pod's, holds none and is `403 OAUTH_SCOPE_REQUIRED`, so no machine turns the setting off.
  A token scoped to another project is `403 PROJECT_SCOPE`, and an unknown project `404 PROJECT_NOT_FOUND`.
  A write that finds the setting as it asks changes nothing and appends no event; a change appends `project.updated`, with the project row and its writer.
  The answer is the project as `GET /api/v1/projects` lists it.
  `dispatch project-update --project <KEY> --proven-sessions on|off` sends the `PUT` or the `DELETE`.
  Only the project's roll-out issue turns it on, once the project's daemon and pods write with tokens of their own: turned on before that, it stops every architect write and every role-holder publication on the shared token until a person turns it off, which costs time and loses no data.

- `/api/v1/settings/repo-projects` (GET; cookie, trusted header, or bearer)

  Read only, and removed in the next release: the rows of `repo_projects` as the last release that wrote them left them, each with `default`, whether it was its project's default repository, and `renamed_to`, the `owner/name` the repository is called today, null for one that was never renamed.
  Nothing writes the rows, and `PUT` and `DELETE` under this path answer the JSON `404 NOT_FOUND` of a route the process does not serve.
  The route stays for the Legion daemons that read it at boot.
  A default set through `PUT /api/v1/projects/{key}/default-repository` is not in it.

- `/api/v1/projects/{key}/default-repository` (PUT; cookie, trusted header, or bearer)

  Set or clear a project's default repository: `{"repo": "owner/name"}` sets it, and `{"repo": null}` clears it.
  A bearer names its session in an optional `actor`.
  An issue of the project resolves to the default when neither the issue nor an issue above it names a repository.
  `repo` is read by the rule that reads an issue's own repository, and stored in lower case with no `.git`, under its new name where a rename pair renames it away.
  The answer is `{project, repo}`, where `repo` is the default the project holds now, or `null` for none.
  A write of the value the project holds already changes nothing and appends no event, and a change appends `project.updated` with the project row.
  A body without `repo`, or a `repo` that is not a repository GitHub could hold, is `400 INVALID_PROJECT` naming `repo`, and an unknown project is `404 PROJECT_NOT_FOUND`.
  The default belongs to the deployment, so a project-scoped credential is refused it with `403 PROJECT_SCOPE_DEPLOYMENT`, the project this path names included.
  It needs no `person` scope: the credential the deployment configured writes it, and so does a person.

- `/api/v1/projects/{key}/legion/daemon` (GET, PUT, DELETE; cookie, trusted header, or bearer)

  The Legion daemon this project's state is read from: GET reads it, with the actor that first named it and when it was last written, or `null` when the project names none; PUT names it, `{state_url}`; DELETE stops reading it, what the sampler already recorded staying.
  No route lists every project's daemon, since R26 of dispatch://INFRA-979's spec permits one cross-project read and this is not it: the Settings page's Legion section reads each project's.
  A `state_url` that is not an absolute http(s) URL with a host and no path is `400 LEGION_SETTING_INPUT`, a DELETE for a project that names no daemon `404 NOT_FOUND`, a key that is not a project key `400 INVALID_PROJECT`, and a project that does not exist `404 PROJECT_NOT_FOUND`.
  The daemon points Dispatch at a host, so it belongs to the deployment rather than to the project it is addressed under, and a project-scoped credential is refused the PUT and the DELETE as it is a project's default repository, the project this path names included.
  `internal/dispatch/api/settings_legion.go` gives the rest, including why neither write emits an event.

- `/api/v1/projects/{key}/legion/board` (GET; cookie, trusted header, or bearer)

  The Legion board of the project its path names, from what the sampler recorded of that project's daemon; the `/ws/live` `legion` view carries the same body and pushes each change, and no page draws it.
  Eleven columns (`queue`, `design`, `planning`, `implementing`, `testing`, `reviewing`, `accepting`, `retro`, `merging`, `awaiting_person`, `held`), each card naming its issue's key, title, priority, phase and when that phase began, the slot it holds, the roles in a turn now, the issues a blocker link holds it on, what it waits on from a person, its tree root and the children of its tree that have not started; and a header with the slots in use, the slot cap, the queue's length and the last read of the daemon with its failure.
  `404 LEGION_NOT_CONFIGURED` when the project names no daemon, `400 INVALID_PROJECT` for a key that is not a project key, `404 PROJECT_NOT_FOUND` for a project that does not exist.

- `/api/v1/issues/{key}/legion` (GET; cookie, trusted header, or bearer)

  One issue's Legion state, from what the sampler recorded of the daemon of the issue's project: `{issue, project, configured, state, read, runs_revision}`.
  The `/ws/live` `legion_issue` view carries the same body and sends each change.
  `project` is the project of the issue, and `configured` is `true` when that project names a daemon.
  `state` is `null` when the daemon does not record the issue.
  Otherwise it is `{phase, phase_since, phase_start_seen, roles, slot, waiting_line, blocked_by, waiting_on}`.
  `phase_since` is when Dispatch stored the phase, and `phase_start_seen` is `false` when that phase is the first value that Dispatch stored for the issue.
  `roles` is `[{role, state}]`, one entry for each role with a claim, and `[]` when no role has one.
  `slot` is the index of the slot that the issue holds, or `null`.
  `waiting_line` is `{place, length}`, where `place` counts from 1 in the daemon's waiting line, or `null` when that line does not name the issue.
  `blocked_by` lists the issues that block the issue, in the order the daemon stored them, and is `[]` when no issue blocks it.
  `waiting_on` is the daemon's object exactly as stored, or `null`.
  `read` is the newest read of the daemon, `{at, ok, error}`, or `null` when no read was recorded.
  `runs_revision` counts the tuples of the issue's runs: it is `0` for an issue with no run.
  Each write of a run that stores something new makes it larger, and a write sent again leaves it.
  A project that names no daemon is not refused: its issue answers `configured` `false`, `state` `null` and `read` `null`, whatever rows an earlier daemon left, and its `runs_revision`.
  The one refusal is `404 NOT_FOUND`, for a key that names no issue, as `GET /api/v1/issues/{key}` answers it.

- `/api/v1/github/app-installation/{owner}/{repo}` (GET; cookie, trusted header, or bearer)

  Whether Dispatch's GitHub App, which GitHub sends the repository's webhook events through, is installed on the repository:
  `200 {installed}`, `false` when GitHub answers 404.
  A path whose owner or name has a character GitHub does not allow, or whose name is `.`, `..`, empty or longer than 100 characters once a trailing `.git` is trimmed, is `400 INVALID_REPOSITORY`, and GitHub is asked nothing.
  `503 GITHUB_APP_UNCONFIGURED` when Dispatch holds no App key, `502 GITHUB_UNREADABLE` naming the repository and GitHub's error on any other failure.
  The Legion daemon asks it for every repository it serves, for `legion state`.

- `/api/v1/ci-reconcile/passes` (GET; cookie, trusted header, or bearer)

  The CI reconcile passes of the last 24 hours, newest first (CI reconcile, above), one object each:
  `{id, instance, started_at, ended_at, state, counts, cursor}`.
  `instance` is the `server_processes` id of the process that ran the pass, which may be long gone — the rows are kept for 48 hours and a process is not.
  `ended_at`, `counts` and `cursor` are `null` until the pass ends;
  `counts` is then the eight numbers the pass's one log line carries (`graphql_queries`, `graphql_points`, `rest_requests`, `rest_not_modified`, `heads_over_rest`, `records`, `heads`, `heads_known`) and `cursor` is `{repository, from, number, rest, counted}`, where the pass left the order for the pass after it and so the address that pass takes it up at: `from` names the page of the repository's open pull requests, one above the last head of the page before it; `number` is the first pull request of that page the pass did not finish, `0` for the whole page; `rest` says the page is read from the repository's REST listing rather than from a query; and `counted` that its heads are already among the heads the cycle knows the repository has.
  All of them are zero and false for a pass that reached the end of the order.
  The page's position in the repository is recorded nowhere: a pull request merged below moves the position and moves no number.
  `state` is `running`, `complete`, `deadline`, `rate_limit`, `cancelled` (its process shut down under the pass) or `ended_without_completing`, the last being a process killed between a pass's start and its end.
  A deployment that has run no pass answers `[]`.

- `/api/v1/settings/project-default-assignees` (GET; cookie, trusted header, or bearer; needs the `person` scope)

  List every project with the person it defaults to, by project key, as `[{project, assignee}]`; every project is listed, because every project names a default, and no project at all is an empty list.
  It needs the `person` scope, since the default names a person.

- `/api/v1/settings/new-issue` (GET; cookie, trusted header, or bearer)

  What the command bar's File as a new issue files a person's words as:
  `{label, session}`, the label that new issue carries and the session it is routed to, as `DISPATCH_NEW_ISSUE_LABEL` and `DISPATCH_NEW_ISSUE_SESSION` set them.
  Each is `""` where the deployment set none: the issue then carries no label, and takes no route.
  Enter on the bar's File as a new issue starts a new_issue run (`POST /api/v1/going/runs` with `kind` `new_issue`), whose create carries the label among its labels and the route `session:<id>`, and nothing is posted on the issue: no first message goes to the session, since a message to an agent is a change of a plan the person approves.

- `/api/v1/projects/{key}/default-assignee` (PUT; cookie, trusted header, or bearer; needs the `person` scope)

  Move a project's default assignee to another person, `{assignee}` being the email of a person who has signed in:
  who an issue created with no assignee, by nobody, under no parent or under a parent with nobody on it, goes to — the last step of the create chain — and who a question on an issue nobody holds reaches.
  Nothing clears a default, so a body whose `assignee` is absent, null or blank is `400 INVALID_PROJECT`.
  `400 ASSIGNEE_NOT_ALLOWED` for an email nobody has signed in with, `404 NOT_FOUND` for an unknown project.
  It needs the `person` scope too, and no agent moves a default: an agent names a project's first one on `POST /api/v1/projects`.

- `/api/v1/delivery/session-groups` (POST; cookie, trusted header, or bearer)

  Create a Delivery session group `{name, sessions}`: its sessions' pull requests take it as their Parent agent, `group:<id>`.
  `409 SESSION_IN_GROUP` for a session another group holds, `409 SESSION_GROUP_NAME_TAKEN` for a name another group has (ignoring case), `409 SESSION_GROUP_NAME_IS_SESSION_TITLE` for a name that is the latest title of a session outside the group, `400 SESSION_GROUP_INPUT` for a blank name or no sessions.
  The timeline lists every group (`session_groups`).

- `/api/v1/delivery/session-groups/{id}` (PATCH, DELETE; cookie, trusted header, or bearer)

  Rename a session group, add sessions to it or remove them `{name?, add?, remove?}`, with the create route's refusals and `400 SESSION_GROUP_INPUT` for an edit that would leave it empty;
  or delete it, its sessions showing as themselves again.
  `400 SESSION_GROUP_ID_INPUT` for an id that is not a UUID, `404 NOT_FOUND` for one no group has.

- `/api/v1/me/agent-tokens` (GET, POST; cookie, trusted header, or bearer; GET needs the `person` scope, POST `person` and `approve`)

  List personal-token metadata, the tokens that write and the read-only ones together, each row with the `scopes` its token holds, or mint a personal agent token `{name, scopes?}`.
  `scopes` defaults to the set the caller's own credential holds;
  a scope this deployment does not know, `approve` without `person`, or `read` beside `approve` is `400 TOKEN_SCOPES_INPUT`, and a scope the caller does not hold itself is `403 OAUTH_SCOPE_REQUIRED`.
  A set that holds `read`, alone or beside `person`, makes a read-only token: Dispatch serves it on the reads of its read class alone, and keeps it in a table of its own, `agent_read_tokens`, which a build before it never reads, so such a build answers the token `401`.
  Settings → Agent tokens offers it as "Read only".

- `/api/v1/me/agent-tokens/{id}` (PATCH, DELETE; cookie, trusted header, or bearer; needs the `person` and `approve` scopes)

  PATCH sets the token's scopes `{scopes}` — the whole set it is to hold, refused as the mint's are — and answers the token; DELETE revokes it, a token that writes or a read-only one.
  No token changes to or from `read`: a set that holds `read`, and any change to a read-only token, is `400 TOKEN_SCOPES_INPUT`, since a person makes a new token for that.
  Another person's token, or none, is `404 NOT_FOUND`.

- `/api/v1/users` (GET; cookie, trusted header, or bearer; needs the `person` scope)

  Everyone who has signed in, as `{users: [{login}]}` sorted by email — the assignee picker's options.

- `/api/v1/whoami` (GET; cookie, trusted header, bearer, or a broker-issued DPoP token)

  Who the server takes the caller for: `{kind: "user", login, scopes}` for a person (`login` their lowercase email), `{kind: "agent", owner, service, session, project, scopes, cut, sign_in_required}` for every other credential (`owner` is a personal token's owner or a broker-issued token's person, by lowercase email, null for the shared token and for a broker-issued token that names a service;
  `service` is a verified service-account token's Kubernetes subject, or, for a broker-issued token naming no person, the fleet its machine belongs to (a machine's token) or the Kubernetes account an exchange entry lists (an exchange entry's account token), null for every other credential;
  `session` is the session the credential itself proved, which only a broker-issued token does, null otherwise;
  `project` is the one project key that credential may write, null where it names none;
  `cut` is the past time a read-only credential reads at, an RFC 3339 time in UTC, null for a credential with no cut;
  `sign_in_required` is true where Dispatch refuses every write of the credential `403 SIGN_IN_REQUIRED` until a person signs in on its machine, which is a machine's broker-issued token, and false for every other credential, an exchange entry's account token and every read-only credential among them; it is the decision the sign-in check makes, and an older Dispatch omits it).
  `scopes` is the OAuth scopes the credential holds, `[]` for one that holds none, with `read` after them for a read-only credential, and every caller is answered it.
  A read-only token that carries a cut is answered here, and refused `403 AS_OF_TOKEN_NOT_SERVED` on every other read.

- `/api/v1/dashboard/builds` (GET; cookie, trusted header, or bearer)

  Which dashboard builds are served: `{serving, live}`, `serving` the entry chunk (`/assets/index-*.js`) of the build this process serves (`""` when it serves none), `live` the distinct entry chunks of the builds the live server processes serve (the registry of live server processes joined to `dashboard_builds` on the build id), sorted.
  The dashboard offers "A new version is available" only when `live` holds exactly one entry and it is not the tab's own, so no tab is told to reload while a rolling update or a rollback still runs two builds.
  An older process answers `404`, which the dashboard reads as no new version.

- `/api/v1/issues?project=&status=&parent=&priority=&advances=&own=&updated_since=&closed_since=&closed_before=&route_status=&moved=&limit=&offset=` (GET; cookie, trusted header, or bearer)

  List issue summaries: every matching issue as an array, or, with `limit` (1–250) or `offset` (0 or more;
  alone it pages 50), one page `{issues, total, limit, offset}` cut after every filter, `total` counting the issues they match.
  A repeated, blank, non-integer or out-of-range `limit` or `offset`, or any `cursor`, is `400 INVALID_QUERY` naming the parameter.
  The order is status, rank, creation time and key, so consecutive offsets cover the listing once while it does not change between reads;
  an issue that enters or leaves what the filters match, or whose status or rank changes, between two reads shifts rows across a page boundary, so one issue is served twice and another never.
  Only the unpaged array is an exact set in one read.
  Filters are optional; `updated_since` is RFC3339 and inclusive, matching issue changes and later issue events.
  `closed_since` and `closed_before` are RFC3339 bounds on the time an issue was closed, `closed_since` inclusive and `closed_before` exclusive, so `closed_since=2026-11-03T00:00:00Z&closed_before=2026-11-04T00:00:00Z` keeps an issue closed at midnight on the 3rd and not one closed at midnight on the 4th, and an open issue matches neither bound.
  A malformed `updated_since`, `closed_since` or `closed_before` is `400 INVALID_UPDATED_SINCE`, `400 INVALID_CLOSED_SINCE` or `400 INVALID_CLOSED_BEFORE`, and a `closed_before` at or before `closed_since` is `400 INVALID_CLOSED_WINDOW`.
  `priority` repeats (`priority=0&priority=1`), each value `0`–`3` or `none` for an issue with no priority; any other value is `400 INVALID_PRIORITY`.
  `status` repeats (`status=todo&status=in_progress`) and keeps the issues standing in any status named; no `status`, or only blank ones, is every status.
  A value is matched as written and never refused, so a status the lifecycle does not hold, or one in another case, alone answers the empty list, as a lone `status` always has, and beside another status adds nothing.
  `route_status` (`live`, `no_holder` or `unknown`;
  anything else is `400 INVALID_ROUTE_STATUS`) keeps the open issues whose route is in that state, whatever their priority;
  `live` or `no_holder` is `503 ENVOY_REFILLING` with the listener's `Retry-After` while the listener refuses its list as refilling, and `503 ENVOY_UNAVAILABLE` when it does not answer.

  - `advances` repeats, each value a priority id (the issues that advance that priority, by their own relation or one an issue above them states, in any project), `none` (the issues that advance no live priority) or `any` (the issues that advance one, every issue `none` leaves out);
    several are a union, so `advances=none&advances=any` is every issue, and an id no live priority carries matches no issue rather than refusing.
    A blank `advances` is `400 INVALID_ADVANCES`.
    `own=true` keeps, of the issues the `advances` values answer, those whose own row names a priority a value matches and no issue above them carries it: a child that only inherits a priority, or restates one an issue above it carries, is left out, and `none`, which matches no priority, keeps nothing; without an `advances` value it filters nothing.
    `advances=none&status=in_progress&status=testing&status=needs_review&status=retro` and `advances=any&own=true&status=backlog&status=icebox` are the two fault lists of `GET /api/v1/priorities` whole and uncut.
    `moved=true` adds `moved_at` and `moved_by` to each summary standing in `backlog` or `icebox`: when the issue was moved there and who moved it, read from its newest `issue.updated` event that set one of the two statuses, as the second fault of `GET /api/v1/priorities` reads them;
    a summary standing elsewhere, or whose events do not reach back to the move, carries neither field, and a listing without `moved=true` reads no event and carries neither on any summary.

  - Summaries contain `key`, `project` (the key of the project the issue belongs to), `title`, `status`, `priority`, `parent`, `assignee`, `route`, `route_status`, `route_holder`, `route_status_reason`, `updated_at`, `closed_at` (when the issue was closed, null while it is open), `last_seq`, and `open_asks`.
    Every issue read (this list, `?pinned=true`, and `GET /api/v1/issues/{key}`) resolves `route_status` against the process's one copy of the listener's `GET /v1/sessions`, stored in no table:
    that copy is at most 5 s old, one listener call refreshes it for every read that asks while it runs, and a listener that fails or does not answer within 2 s is asked again only after 5 s, every read in that window reading `unknown` at once.
    The status is `live` (a live session holds the role, or the session is live;
    `route_holder` names it), `no_holder` (nobody live holds the role, or the session is not live), `unknown` (the listener gave no list), or null with no route.
    `route_status_reason` says why a route is `unknown`:
    `refilling` (the listener refused its list as refilling, so the holder may well be running) or `unreachable` (the listener is not configured, did not answer in time, or failed otherwise); it is null with every other status.
    A refilling list never makes a route `no_holder`.

- `/api/v1/search?q=&project=&limit=&offset=` (GET; cookie, trusted header, or bearer)

  Full-text search over issue titles, latest document text, comments, asks, and messages;
  every match ranked on one list across the kinds by one score (above), with `<mark>` snippets and SPA `href`s.
  One page `{results, total, reachable, limit, offset, took_ms}`:
  `limit` 1–50 (default 20), `offset` 0 or more (default 0), `total` every match, `reachable` the hits the pages can return (each leg's best 100).
  A hit an issue owns (the issue itself, or its document, comment, ask or message) carries the issue's project in `owner.project`, as a project document's hit carries the document's.
  Every `ask` hit carries `ask`, where the ask stands when the search ran:
  `{state: "open"}`, `{state: "answered", answer}` with its latest answer, or `{state: "resolved", resolution, answer?}` with the last answer it had, absent when nobody answered;
  its excerpt is cut from the ask's indexed text ([Search](#search)).
  A hit the meaning list found carries `block_id`, the block of it whose own text ranks highest for the query:
  a block of the document, of the comment's or the message's body, or the part of an ask's question, options or answer.
  It is absent on an issue hit, which has no blocks, and on a hit only the keyword list found.
  An under-two-character query returns `400 INVALID_QUERY`;
  a stop-word-only query returns `200` with no results;
  `400 CAP_EXCEEDED` over 1,000 characters (`contracts.SearchQueryMax`, UTF-16 units after trimming), since the query rides in the URL;
  `400 INVALID_PROJECT` for a project that is not a project key (none searches every project); `400 INVALID_LIMIT`; `400 INVALID_OFFSET`.
  This route declares `q`, `project`, `limit` and `offset` alone.
  `number_project`, `status`, `author`, `updated_since` and `keywords_only` are declared on `GET /api/v1/search/results` below, and a client sends a search carrying one of them there.
  It still reads all nine, refusing each of those five under its own code (`400 INVALID_NUMBER_PROJECT`, `INVALID_STATUS`, `INVALID_AUTHOR`, `INVALID_UPDATED_SINCE`, `INVALID_KEYWORDS_ONLY`), so a page served by an earlier release that sends one here is still answered narrowed ([Search](#search)).
  Keyword search finds a text whose search vector would pass Postgres's limit on one vector by the words in its opening part only ([Search](#search)).

- `/api/v1/search/results?q=&project=&number_project=&status=&author=&updated_since=&keywords_only=&limit=&offset=` (GET; cookie, trusted header, or bearer)

  The same search as `GET /api/v1/search`: the same handler, the same answer and the same refusals.
  It declares all nine members, so every generated client offers the five here and nowhere else, and a client sends a search carrying `number_project`, `status`, `author`, `updated_since` or `keywords_only` here, because a build released before those five answers this path `404 NOT_FOUND no route for GET /api/v1/search/results`, where it answers the other path with the member dropped ([Search](#search)).
  `number_project` is the project of the page the search was made from, which a query that is a bare issue number is read in ([Search](#search));
  it narrows nothing, so a search from a project or issue page answers every project's rows.
  `keywords_only=true` runs the keyword leg alone and makes no embedding call, whatever embedder the deployment configured, and its answer is not stamped `degraded`, because nothing is unavailable: the caller asked for the words alone.
  The command bar sends it on every keystroke, so a person typing waits on no model call; a request that leaves it out searches as it always did, by meaning too wherever an embedder is configured.

- `/api/v1/search/coverage` (GET; cookie, trusted header, or bearer)

  Meaning search's coverage, counted in one statement ([Search](#search)):
  `{identity, place, model, total, kinds, not_indexed}`, the configured embedder's identity, its place (`bedrock`, `azure`, or `dev` for a local harness's) and its model, all three null with no embedder;
  then, in `total` and for each kind in `kinds` (`issue`, `document_block`, `comment_block`, `message_block`, `ask_part`), `items` (the searchable ones, a text with nothing to embed left out), `current` (embedded at the item's current text by that identity), `failed` (dead and not current), `lacking` (the rest) and `oldest_lacking_seconds`, how long the longest-waiting lacking item has waited (null when none lacks).
  A window kind's items are its own rows, since the indexers own which windows exist.
  With no embedder nothing is current.
  `not_indexed` is every kind of node neither leg of search holds a row of, each `{kind, items}`: `component`, `session`, `document_version`, `result` and `handoff`, with how many of that kind the database holds.
  Each count is read from the table that holds the kind — `components`, the listener's stored session list (`route_roster`, which is Dispatch's whole record of which sessions exist, and counts 0 before the listener has answered), `artifact_versions`, `check_results` and `handoffs` — in the statement that counts the covered kinds, so every number of the report reads one snapshot.
  Such a kind is named rather than left out, because a reader of a report carrying the covered kinds alone cannot tell a kind that is covered with nothing left to embed from one nothing indexes.
  It carries no `current`, `lacking` or `failed`, none of which is defined for a kind that has no vector to be behind on, and it is in no total.
  The statement reads a window kind through two partial indexes, and an issue's hash from its row wherever the inputs the coverage pass recorded for it are still its own, so it tests no window's text and builds only the text of an issue whose inputs changed ([Search](#search)).
  It reads the two indexes by one bitmap heap scan over both at once, which visits each heap page holding a window once, and by no other index or plan, whatever the visibility map holds, the statistics record or the share of windows embedded; it reads no statistics to choose a plan.
  It runs under a 10 s `statement_timeout`, and a statement past it counts nothing and answers `503 SEARCH_COVERAGE_TIMEOUT`, to be asked again later.

- `/api/v1/going/catalogue` (GET; cookie, trusted header, or bearer)

  The catalogue of operations an agent may propose on Dispatch's graph: twenty of the dispatch tools that write, of the specs agents already operate Dispatch with (`packages/contracts/src/dispatch-tools.ts`); the route's own description carries the counts, read from what the build holds.
  `dispatch_review` is not among them, on purpose: a review records an approval or a changes request in a person's name, and the decision call that chooses from this catalogue never names one; a command-bar run proposes a review only as a row of its plan, which waits for the person in every mode.
  Nor is `dispatch_project_update`, which turns a check of a project on or off (its proven-sessions setting): the command bar does not offer it at all, and its two routes are kept out of a run's tools on purpose (`api/going_tools.go`).
  Each action carries `action` (the tool's own name, such as `dispatch_issue`), its `description` (the tool spec's own, whole and unchanged, which is what a model chooses on), the `routes` it writes, each a `method` and a `pattern` of this table, the `relations` its verbs write (each a structural edge kind or a `block[]` attribute of a typed block), its `request` (the tool's own argument schema) and an `example` call that schema accepts.
  The catalogue is under the count where a model's choice falls apart, so it is read whole: it takes no query and serves no page.
  A tool naming a row this table does not hold, a row the table does not class as a write, a placeholder no argument of the tool fills, or a relation in neither edge family makes the server refuse to mount the table.
  A write no catalogue action performs is named by the catalogue's own test instead, and the review route and the two routes of the proven-sessions setting are kept out on purpose; a command-bar run's tools are another set, one for each route a person can use (`api/going_tools.go`).

- `/api/v1/projects/{key}/related?title=&text=&exclude_issue=&exclude_issues=&keywords_only=` (GET, POST; cookie, trusted header, or bearer)

  Run the related-item check an issue create runs ([Search](#search)) on a title and a text, checked as the title, a newline and the text (either alone is enough), and write nothing:
  no issue, ask, suggestion or `related_checks` row.
  `exclude_issue` leaves that issue of the project, and everything it holds, out of the results and out of the word statistics (`400 EXCLUDE_ISSUE_INPUT` when it is not an issue of the project);
  `exclude_issues`, a list of at most 50 issues of the project, leaves each of them out of the results alone, so a caller can leave out a whole subtree without changing how rare the text's words are (`400 EXCLUDE_ISSUE_INPUT` for a longer list or for a key the project does not hold);
  `keywords_only=true` skips meaning search.
  GET takes the fields in the query, `exclude_issues` repeated, title and text together at most 1,000 UTF-16 units (`400 CAP_EXCEEDED` past it);
  POST takes them as a JSON body `{title, text, exclude_issue, exclude_issues, keywords_only}`, a title of at most 1,000 and a text as long as the body's 1 MiB bound.
  It waits at most 5 seconds and answers `200` with the create's `advice.suggestions` shape (`outcome`, `search`, `search_note`, `search_query`, `related`, `missing`), a blank title and text included (`not_made`).
  The two forms do not answer alike: POST is the check an agent runs to ask a question, so it also answers `other_projects`, at most two answered question asks of other projects, and GET answers none of them, because its caller is the dashboard's new-issue dialog ([Search](#search)).
  `400 INVALID_PROJECT` for a key that is not a project key, `404 PROJECT_NOT_FOUND` for an unknown one.

- `/api/v1/related-checks?project=&source_kind=&from=&to=` (GET; cookie, trusted header, or bearer)

  Count the related-item checks recorded when an issue, an ask or a new decision block was written, within `[from, to)` (RFC3339, each optional) and of `project` and `source_kind` (`issue`, `ask` or `decision_block`;
  another is `400 INVALID_QUERY`) when given:
  `{project, source_kind, from, to}` echo the filters (null when absent), then `total`, `by_source_kind`, `by_outcome`, `by_search` (a check not made counted under `not_made`), `by_reason`, `completed` (found or none), `skipped_for_time` (reason `timeout`), `other_project_offers` (the ask creates of the window that offered at least one answered question of another project, read from `write_suggestions`), and `keyword_ms` and `total_ms`, each `{count, p50, p95, max, over_300ms}` over the checks holding that time (`model.RelatedChecksReport`).

- `/api/v1/near-copy-checks?project=&from=&to=&after=&limit=` (GET; cookie, trusted header, or bearer)

  List one project's near-copy gate answers (`near_copy_checks`, [Search](#search)) within `[from, to)` (RFC3339, each optional; `from` not before `to` is `400 INVALID_QUERY`), oldest first, at most `limit` (1 to 1,000, default 1,000) after the row `after` names:
  `{project, from, to, checks, next_after}` (`model.NearCopyChecksPage`), each row with no spec.
  `project` is required (`400 INVALID_PROJECT` when it is not a key), and an `after` that is no row of the project is `400 INVALID_QUERY`.
  A window should end some time before now: a row written in its create's transaction holds that transaction's start as `created_at` and is seen only once it commits, so a reader that pages up to now and resumes with `after` can miss it.

- `/api/v1/near-copy-checks/{id}` (GET; cookie, trusted header, or bearer)

  One near-copy gate answer (`model.NearCopyCheck`), with `spec` on a refused row alone;
  `404 NEAR_COPY_CHECK_NOT_FOUND` when there is none, `400 NEAR_COPY_CHECK_ID_INPUT` for an id that is not a uuid.

- `/api/v1/issues/{key}/references?versions=` (GET; cookie, trusted header, or bearer)

  Read the issue's references, one hop in each direction: `{links, linked_from, members, truncated}` (`model.IssueReferences`).
  The issue's own content is its documents and their blocks, its asks and their parts, and its comments and messages and their blocks; a link is a mention, or a typed pointer while the graph read lists it.
  `links` is every node the issue's own content links to, and `linked_from` every node that links to the issue or to its own content.
  Each holds one row for each node, `{node, name, kinds, via}`: the node as `GET /api/v1/references` names it, the words shown for it, the distinct kinds of its links in order, and the lowest of the issue's nodes at the near end of those links.
  A block of a document counts as that document while a current version holds the block, and a part of an ask or a block of a comment or a message counts as its item.
  The issue and its own content are in neither list, nor is a node that only a structural edge joins to the issue, such as its parent or a child.
  Each list is in the order of the node's kind, then its id, and is `[]` when it has no row.
  `members` is the documents of `links`, each with its versions and its row's via, and `truncated` is false: a member's `depth` is always 1, since the read reads one hop, and both fields stay for the clients that read them.
  Every statement of the read shares one 2 s budget, and a read that reaches it reads nothing and answers `503 REFERENCES_TIMEOUT`, to be asked again later.
  An `If-None-Match` value equal to the response ETag returns `304`; the ETag moves when a row of either list changes, as a new link into the issue's content or an edit of a linked node changes one.
  `versions=latest` leaves each member's `artifact.versions` holding only its latest version and carries its own ETag;
  absent, empty or `all` is the full answer; any other value, or `versions` given more than once, is `400 INVALID_QUERY`.

- `/api/v1/issues/{key}/collaborators` (GET; cookie, trusted header, or bearer)

  Everyone who worked the issue, running or not, as `{issue_key, people, agents, legion_registry}`.
  `people` (`kind: "user"`, one per lowercase login): the assignee, everyone who claimed it, posted a comment, message, ask or document version on it or its documents, or answered one of its asks.
  `agents` (`kind: "session"`, one per session id): the same writers that are agent sessions (the Legion daemon's `legion-daemon:<PROJECT>` writer left out), merged with every session the listener's Legion agent registry records for the issue, which adds `legion: {role, role_token, started_at, replaced_by, replaced_at}` and the registry's `title`.
  Each row carries `reasons` (`assignee`, `claimed`, `posted`, `answered`) and `last_at`; no running state.
  When the listener is not configured or does not answer, `legion_registry` is `"unavailable"` and the rest still answers; `404 NOT_FOUND` for an unknown issue.

- `/api/v1/references?to=|from=&kind=&since=` (GET; cookie, trusted header, or bearer)

  Edges of one node in the reference graph, newest first and cross-project:
  exactly one of `to` (backlinks) or `from` (links), each a `dispatch://` reference, read whatever its length (`text.ParseRef`);
  `kind` filters a csv of edge kinds, the structural ones and the name of each `block[]` attribute the block schema declares, and a kind this build does not emit (a newer build's, during a release) has no edges and is listed in the top-level `unknown_kinds`;
  `since=<events.id>` keeps mentions introduced after it (structural edges excluded).
  The answer is `{node, edges}`, the node read and its edges.
  Each edge carries the other `node`, an `excerpt` (the containing block for a document mention), `created_at`, and `source_seq`.
  The node read, each edge's `node` and each edge's `block` carry the six facts of every node, `made_by`, `made_at`, `changed_by`, `changed_at`, `parent` and `parent_version` ([Reference graph](#reference-graph)).
  What is listed, and from where:

  - A mention a block of a document holds is listed from that block (`node.kind` `block`, `node.document` its document);
    a typed block's `block[]` pointer is an edge of the attribute's kind from it, listed only while its target has a type the attribute allows;
    and a mention a block of a comment or issue message, or a part of an ask, holds is listed from that block (`node.kind` `block`, id `comment/<uuid>#<block id>`, `message/<uuid>#<block id>` or `ask/<uuid>#<part id>`, `node.parent` the comment, message or ask);
    a document's, comment's, message's or ask's own read also lists the edges at its blocks, an ask's parts among them, each naming the block as `block`, and leaves out their `part_of` edges unless `kind` names `part_of`, and a reply that names a block of the comment or message read, or a part of the ask read, is listed once, under that block.

  - A block reference (`#b-<id>`) names a `block` node carrying its whole current `text`;
    pinned to a version (`#b-<id>@v<N>`; for a comment's block, a revision of the comment, and for an ask's part, a revision of the ask's parts) it also carries `pin` (`version`, `ref`, the block's `text` at that version, `state` `unchanged`, `changed` or `removed`, and `removed_version`), and a removed block answers no edges.

  - `400 INVALID_REFERENCE` / `INVALID_SINCE`, `INVALID_REFERENCE` too for a message block pinned with `@v<N>` (a message numbers no revisions, so there is nothing to pin);
    `404` for a node that does not exist, a block no current version or revision holds (unpinned;
    an ask's part is called a part, `no part "<part>" in ask <id>`, where a comment's and a message's is called a block) or whose id is past 256 bytes (no node), or a pinned version the document lacks, has no record of the blocks of, or did not hold the block at, or a revision past the comment's or the ask's current one or that did not hold the block.

  - A `since` read stops at the event log's safe horizon, and first waits up to 1 s for the horizon to pass every event committed before the read, so it answers every mention committed before it, the caller's own included;
    when an open transaction holds the horizon back past that wait, it answers the mentions at or below the horizon and a later read the rest.
    A caller with no cursor starts from `since=0`, which lists every mention committed before the read that carries a `source_seq`, and after that reads again from the highest `source_seq` an answer showed it.
    A mention written with no event (a rebuild, a backfill, an older build) carries no `source_seq` until a later write of its source stamps it with that write's event, and until then only a read without `since` lists it.
    A signed-in page may start from `GET /auth/whoami`'s `event_head` instead, which takes a cookie or a trusted header and not a bearer, so a bearer caller starts from `0`.
    Never take a `since` from `max(events.id)` read straight from the database:
    event ids are not commit order, and an id taken that way can stand above an edge that commits later.
    An empty answer is not proof that nothing is new, so the caller reads again with the id it holds.

- `/api/v1/block-links?from=&to=` (GET; cookie, trusted header, or bearer)

  The links from blocks of the document `from` names to blocks of the document `to` names (an issue's spec or document, or a project's document;
  one document may be both), each reference read whatever its length:
  `{from, to, links}`, each link with its `kind`, both block nodes, and the `created_at` and `created_version` at which the source block first held it, newest first;
  `from`, `to` and every link's two blocks carry the six facts of every node ([Reference graph](#reference-graph)).
  A link is listed only while both blocks are current blocks of their documents, and a typed block's `block[]` pointer only while its target has a type the attribute allows.
  `400 INVALID_REFERENCE` when either is not a document reference; `404 NOT_FOUND` for a document that does not exist.

- `/api/v1/issues` (POST; cookie, trusted header, or bearer)

  Create an issue and its primary document.
  Omitting or leaving `spec` blank gives an empty primary document at version 1.
  `repo` (`owner/name`) is the issue's own repository, which may be any repository at all:
  `400 INVALID_ISSUE` when it is not `owner/name`.
  `route` (`role:<name>` or `session:<id>`) is the route the new issue takes, held to the rule `PATCH /api/v1/issues/{key}` holds a route to (`400 ROUTE_INVALID`); none, or `""`, leaves it with none.
  `parent` may be an issue of any project: `400 PARENT_INPUT` when no such issue exists, `409 PARENT_INPUT` when the new issue would sit more than 32 links below its root, counting the links in every project, and `403 PROJECT_SCOPE` when a credential that may write one project names a parent outside it.
  A parent of another project passes on no repository, components, `legion` label or assignee:
  `resolved_repo`, an inherited component attachment, the architecture tree's owner and `legion_managed_status` read only the ancestors in the issue's own project, so such an issue takes its own project's default repository, and a create that names no assignee and acts for no person lands on its own project's default assignee rather than the parent's.
  A title over 1,000 characters (`contracts.IssueTitleMax`, UTF-16 units after trimming) is `400 CAP_EXCEEDED` before the duplicate check.
  Refuses a near copy of an open issue of the project, other than the new issue's ancestors, with `409 POSSIBLE_DUPLICATE` and candidates unless `force` is true, once the refusal's `near_copy_checks` row is known to be written;
  a refusal whose row cannot be written refuses nothing, and the create goes in with `advice.suggestions.near_copy_check` naming the candidate.
  External references skip the check.
  A create that names `external`, an import of a GitHub issue, names its `project` too: one with no `project`, or an empty one, is `400 INVALID_ISSUE` (`project is required when external is set`), and it writes nothing.
  A forced create records the issues it went past on its `issue.created` event, under `forced.past`.
  A create the route made answers `201`; one naming an external reference Dispatch already holds an issue for answers that issue `200` and makes nothing, which is how a caller (the CLI's "already holds", the command bar's `unchanged` write) tells the two apart.
  A spec whose ask block breaks its content rule (`paragraph+ bullet_list?`:
  one or more paragraphs, then at most one bullet list, last) is `400 INVALID_ASK_BLOCK`, and a spec whose markdown makes more than 65,536 elements (weighed as Element weights below lists) is `413 CAP_EXCEEDED`, naming the line that passes the limit;
  so is one whose markdown as stored (what `.../text` answers) would be longer than 1 MiB or make more than 65,536 elements.
  A spec holding a check block that breaks a `ci_check` rule is `400 INVALID_CI_CHECK` (Document errors below).
  With `DISPATCH_PARENT_STATUS_RULE` on, a create under a `done` parent is `409 PARENT_STATUS_CONFLICT` `{code, error, issues}`, with `issues` naming the parent (`POST /api/v1/issues/{key}/status-change`, below).
  A create under a parent and a close of that parent at the same moment never both go through: the one that reaches the parent's row second is refused.
  When Legion runs the new issue, because it or an issue of its own project above it carries the label `legion`, a create under a parent from a writer that is no person and no architect of the parent or of an issue above it is `403 DOCUMENT_WRITER_NOT_ARCHITECT` (Document errors below).
  A create under a parent of another project is judged as a root of the new issue's own project, which the check does not judge.

- `/api/v1/issues/{key}` (PATCH; cookie, trusted header, or bearer)

  Update an issue's `title`, `status`, `priority`, `labels` (or `labels_add`, which adds to the labels the issue holds when the write commits;
  not with `labels`), `route`, `external_links`, `parent`, `rank`, `assignee`, `components` or `repo` (`null` clears it); the route's row in `internal/dispatch/api/routes_table.go` (`GET /api/v1`) and `patchIssue` in `internal/dispatch/api/issue_patch.go` give each field's rules.
  A `parent` may be an issue of any project, under the refusals a create meets, and one that would make the issue its own ancestor is `409 PARENT_INPUT`;
  a parent of another project passes on no repository, components or `legion` label, as on creation.
  A title is trimmed, and one over 1,000 characters is `400 CAP_EXCEEDED`, as on creation.
  `expected_sequence` makes the write conditional: `409 ISSUE_CHANGED` when the issue's last event is no longer that sequence.
  With `DISPATCH_PARENT_STATUS_RULE` on, a `status` or a `parent` that leaves a parent's status out of line with its children's is `409 PARENT_STATUS_CONFLICT` `{code, error, issues}`, as `POST /api/v1/issues/{key}/status-change` (below) describes it.
  The `PATCH` judges only the pairs it changes: a new status judges the issue with its parent and with each direct child, and a new parent judges the issue with that parent.
  A status the issue already has changes no pair, so it is not judged.
  The `PATCH` takes none of that route's options: it ignores them as members it does not declare.
  The answer is the issue, with `previous` and `rank_place` beside it (Undo, above, reads them).

  - On an issue Legion runs, a close, a reopen and a `labels` or `parent` write that takes the issue out of Legion are each `403 DOCUMENT_WRITER_NOT_ARCHITECT` from a writer that is no person, no architect of the issue or of an issue above it, and not the session of the project's Legion daemon (Document errors below).

  - A move under a parent, after which Legion runs the issue, is `403 DOCUMENT_WRITER_NOT_ARCHITECT` from a writer that is no person and no architect of the new parent or of an issue above it.
    A move under a parent of another project is judged as a root of the issue's own project, which this check does not judge.
    Wherever the new parent is, when Legion runs the issue, or an issue under it, before the move, the move is also `403 DOCUMENT_WRITER_NOT_ARCHITECT` from a writer that is no person and no architect of each such issue or of an issue above it where it stands.
    Such an issue is the issue itself when Legion runs it, and else each issue under it, at most 32 levels down, that carries the label `legion` and that it reaches through parents of its own project.
    So another tree's architect cannot move an issue with no label under its own tree to get the right to write a labelled issue under it.
    An architect that took the label off its own tree, or moved its own issue out to the root, cannot then move another tree's issue under it either.
    The session of the project's Legion daemon passes neither check of a move.
    Only these checks judge a move out of every tree Legion runs.
    The `parent` write that takes the issue out of Legion, above, is a write of `parent: null`.

  A close (`status: done`) withdraws every open ask of the issue, whoever's turn it is:
  decision blocks in its documents, plain asks and requests to approve one of its documents.
  Each is recorded `resolved` with resolution kind `withdrawn`, naming the closer as the request's actor, with the reason `Withdrawn by the close of <KEY>.`, and gets one `ask.resolved` before `issue.closed`;
  its block is written `state="resolved"` in the live document.
  The document's next settlement saves that write, and it writes a version when its rendering differs from the latest version.
  That settlement runs while the issue is closed too, but it opens no ask and wakes nobody:
  it changes no ask, records no edit notice, carries no approval, and its repair events carry the settlement's own actor.
  An ask block that has no ask row gets no ask: the settlement saves it `state="resolved"` and records its author.
  An ask answered or resolved before the close is left as it was, and the close records no answer and no approval.
  A reopening `status` (any but `done`) opens again exactly the asks the close withdrew, each with one `ask.opened` before `issue.updated`, and they return to the Inbox.
  The reopen also marks owed each document whose ask blocks wait for it, and the settlement after the reopen opens the ask of a block that has none, credited to its recorded author, retracts a restored ask whose block left its document while the issue was closed, restores a retracted ask whose block came back, and takes into a restored ask the wording that a person gave its block while the issue was closed.
  For each document whose latest version holds every stored edit, the reopen moves the open approval request that names it to that version and carries an approval to that version, as that version's own write does on an open issue, so a request that the reopen restores names the version that a settlement wrote while the issue was closed.
  A document with stored edits that its latest version does not hold, the reopen's own block writes among them, is left to the settlement that saves them, and that settlement's version moves the request and carries the approval.

- `/api/v1/issues/{key}/status-change` (POST; cookie, trusted header, or bearer)

  Write the status of an issue, and with an option what happens to the open issues under it or to the issues above it, so that each parent's status stays in line with its children's.
  Dispatch puts each status in one of three groups: not started (`triage`, `icebox`, `backlog`), in work (`todo`, `in_progress`, `testing`, `needs_review`, `retro`) and `done`.
  A parent is `done` only when each child is `done`, and while a child is in work, its parent is in work.
  The body is `{status, include_children?, unlink_children?, include_parent?, dry_run?, expected_sequence?, actor?}`, and the route reads `expected_sequence` and `actor` as the `PATCH` above reads them.
  Each option acts whenever the call passes it, also when the rule does not need it:

  - `include_children` writes the status on each open issue under the issue, at any depth, that is not in the status's group.

  - `unlink_children` removes each open child from the issue, so each one becomes a root and keeps its status.

  - `include_parent` moves each ancestor that is not in work to `in_progress`, a `done` ancestor too.

  The write itself records the caller, as a `PATCH` does, and Dispatch's system actor, `{kind: "system", id: "parent-status-rule"}`, writes these comments:

  - Each issue that an option changes gets a comment that names the caller, the issue the call names and the option.

  - When the options changed at least one issue, the issue the call names gets one comment after the last write, which names the call and lists each issue that the options changed, with the status it moved from and to, or its removal from the issue.
    The list is one block of the comment, so it stops before it passes the block bound (`block_units` of `GET /api/v1/limits`), and its last line then counts the issues it leaves out, each of which has its own comment.
    A call whose options changed no issue, a call with no option and a dry run leave the issue no such comment, and so does a call that a write refuses: each write that committed before the refusal keeps its own comment.
    A keyed call leaves it once, in the transaction that marks its recovery point finished, and a repeat that finishes the call lists the changes of the attempts before it too.

  Before the first write, the call reads its plan again under the owner locks of each issue it writes and of each one's current and new parent, and checks the whole plan.
  Then it writes one issue at a time, each through the `PATCH`'s own write in a transaction of its own, in an order where each write keeps the rule.
  That order is the ancestors that `include_parent` moves, highest first, then the children that `unlink_children` removes, then the issue itself.
  `include_children` moves the issues under it before the issue itself, deepest first, for a status that is `done` or not started, and after it, highest first, for a status in work.
  Each write decides what it writes from what it reads under its own lock, so a write that another writer made unneeded writes nothing and has no entry in `changed`.
  The answer is `200 {issue, changed}`, and `issue` is the issue as the `PATCH` answers it.
  The answer's `issue` is read at the issue's own write, so later events of the same call can follow it on the stored issue: a `child.status` for each direct child that `include_children` moves after it, for a status in work, and the `comment.created` of the comment on the issue the call names.
  So the stored `last_seq` can be past the answer's, and a client that sends the answer's `last_seq` as `expected_sequence` can get `409 ISSUE_CHANGED`: it reads the issue again before a write that the sequence fences.
  `changed` has one entry `{key, status, parent, previous_last_seq, issue}` for each issue whose status or parent the call changed, the issue itself included, in the order of the writes.
  `status` and `parent` are what the call left, `previous_last_seq` is the issue's `last_seq` as its own write read it under its lock, and `issue` is that issue as its write answered it.
  With `dry_run` the call writes no issue, event or comment, and answers the same shape: each entry holds the status and the parent that the call would leave and the issue's `last_seq` now, and no `issue`.
  A keyed call keeps its progress in its request key's recovery point, so a repeat finishes what its first attempt left (Keyed writes, above).
  A refused write stops the call, and no later write runs.
  Each refusal that the route answers carries `changed` beside its `code` and `error`: the entries of the writes that committed before it, `[]` when none did.
  Two refusals of the router come before the route and carry no `changed`: the `421 HOST_MISMATCH` of a server with the dev sign-in on, and the `403` of an unsafe request that carries a session cookie or an IAP assertion and no `Authorization`, from another origin (`enforceCookieOrigin`).
  With the setting off, the options, `dry_run` and the `403`s below work as they do with it on.
  The refusals are these:

  - `409 ISSUE_CHANGED`: `expected_sequence` is not the issue's last event.
    The route decides this before `ISSUE_CLOSED`, the `403`s and `PARENT_STATUS_CONFLICT`, also for a dry run.
    Each write checks it again, so an event on the issue that the call did not write stops the call.

  - `409 PARENT_STATUS_CONFLICT` `{code, error, issues, changed}`: the call breaks the rule.
    Each entry of `issues` is `{key, title, status, relation}`: an issue that holds the rule against the write, with its status as the write would leave it, and `relation` `parent` or `child`.
    `error` names each of them with its status, so a client that reads no `issues` still shows them.
    The route refuses this with `DISPATCH_PARENT_STATUS_RULE` on, and refuses a dry run of it whatever the setting.

  - `403 BROWSER_SIGN_IN_REQUIRED`: `include_children` with `done` from a request that presents a token, whatever the setting.
    Only a person who is signed in at a browser closes the open issues under an issue together with it.

  - `403 DOCUMENT_WRITER_NOT_ARCHITECT`: a write of the call closes, reopens or takes out of Legion an issue that Legion runs, from a writer that is no person, no architect of that issue or of an issue above it, and not the session of the project's Legion daemon (Document errors below).

  - `403 PROJECT_SCOPE`: the request's credential may write one project, and a write of the call is of an issue of another project: an ancestor that `include_parent` moves, or an issue under the issue that `include_children` moves or `unlink_children` removes, since a parent may be an issue of another project.
    The route checks every write of the plan before the first one, so it refuses the whole call before any write, a dry run too; it decides this after `ISSUE_CLOSED` and before `BROWSER_SIGN_IN_REQUIRED` and `PARENT_STATUS_CONFLICT`.
    A credential that names no project is refused nothing here.

  - `400 STATUS_CHANGE_INPUT`: no status, or `include_children` with `unlink_children`.

  - `400 INVALID_STATUS`: the status is not in the Legion lifecycle.

  - `409 PARENT_CONTENDED`: the issues of the call changed at each of two attempts to lock them, and nothing was applied.

  - `409 ISSUE_CLOSED`: `done` on an issue that is closed.

  - `404 NOT_FOUND`: no such issue.

- `/api/v1/issues/{key}/summary` (GET; cookie, trusted header, or bearer)

  What a reference to an issue shows, read without the full issue:
  `{key, project, title, status, documents}`, `documents` every artifact of the issue in the order the issue read lists them, each `{id, slug, name, kind, latest_version, primary}`, `primary` marking the spec, which a reference to a block of the spec names.
  One query for the issue and one for its artifacts, with no version history, approval, child or open-ask read.
  `404 NOT_FOUND` for no such issue.

- `/api/v1/issues/{key}/asks` (POST; cookie, trusted header, or bearer)

  Create an optionally anchored ask.
  An anchor is exactly `{artifact, quote, occurrence?}` for a server-written quote mark or `{artifact, mark_id}` for a mark already written by a browser.
  A quote mark carries the ask's id and who asked, which no rendering shows and every load of the document builds:
  the marks a document's comments, suggestions and asks hold on its text carry at most 1 MiB of ids and authors, and a quote anchor that would leave them past it and carrying more is `413 CAP_EXCEEDED`, naming the bound and both sizes.

- `/api/v1/issues/{key}/asks?state=&thread=` (GET; cookie, trusted header, or bearer)

  List an issue's asks, open and/or answered (`state`: `all` default, `open`, or `answered`).
  Each row carries `last_reply`, the newest comment in the ask's thread or null.
  `thread=1` adds `thread`, its `replies`, `edits`, `answers` and `followers`, batched over the list so no card fetches its own;
  without it no `thread` is returned, which is what the readers that render the ask alone want from this unpaged list.
  Any other `thread` value is `400 INVALID_ASK_THREAD`.

- `/api/v1/asks/{id}` (GET; cookie, trusted header, or bearer)

  Read an ask with its reply thread, edits, followers and `answers`, every user-authored answer oldest first.

- `/api/v1/asks/{id}` (PATCH; cookie, trusted header, or bearer)

  Edit one or more of `question`, `options`, `multiple`, or `urgency` on an open ask.
  A credential holding the `person` scope may edit any open ask, and somebody else's is recorded as the person with the agent beside them;
  every other must be the asking session, whose own reword is recorded as that session, as it always was.
  The response records `edited_at` and emits `ask.edited` with the current ask, prior mutable fields, and `edited_by`.
  Anchors are selected when the ask is created and cannot be changed by this route.
  On a block ask the `:::ask` block is written in the same transaction and the row takes the block's parsed values, so the edit versions the document once and no settlement reverts it.
  On a block ask in the spec or the Verification document of an issue Legion runs, a writer that is no person and no architect of the issue or of an issue above it is `403 DOCUMENT_WRITER_NOT_ARCHITECT` (Document errors below).
  Only the named fields are written: `urgency`/`multiple` alone go through the attribute path and leave the body's nodes, marks and inner block ids untouched, so an untouched question keeps its formatting, links and comment anchors;
  naming `question` or `options` replaces that part with the markdown pipeline's own parse, and anchors inside the replaced text move as for any document edit.
  A field named but unchanged is not rewritten, so an idempotent retry of the whole ask writes nothing, versions nothing, keeps every anchor and returns 200.
  Text the block cannot carry back unchanged
  — an option label containing `": "`, the separator between a label and its description, is one example
  — is `400 ASK_BLOCK_TEXT` naming the field, with nothing written.
  Text past what one write's markdown may make - weighed before it is rendered, an element for each backslash, `*`, `_`, `~`, backtick, `[`, `]` and `<` and two for each line feed, then as its markdown parses - is `413 CAP_EXCEEDED`.
  An edit of a block ask that would leave the document's markdown (what `.../text` answers) longer than 1 MiB and longer than it was, or making more than 65,536 elements and more than it did, is `413 CAP_EXCEEDED` and the ask keeps its text.

- `/api/v1/asks/{id}/asked-of` (PUT; cookie, trusted header, or bearer; needs the `person` scope)

  Hand an open ask to another person, `asked_of` being the email of somebody who has signed in; the ask waits on them from then on, wherever its issue goes, and their Inbox lists it.
  Naming the assignee of the ask's own issue stores no person, so the ask follows its issue again.
  Either way the row records who handed it on and when (`asked_of_by`, `asked_of_at`), and one `ask.asked_of` event names the person the ask waited on before and the one it waits on now.
  A login nobody has signed in with is `400 ASSIGNEE_NOT_ALLOWED`, an ask that is not open `409 ASK_NOT_OPEN`, and an ask whose issue is closed `409 ISSUE_CLOSED`.
  A credential without the `person` scope is `403 OAUTH_SCOPE_REQUIRED` before the handler runs: handing a question on is one person asking another, and no agent of a deployment may choose who answers.

- `/api/v1/asks/{id}/answer` (POST; cookie, trusted header, or bearer; needs the `person` scope, and `approve` besides for an ask of kind `approval`)

  Answer an open ask, or change the current answer with `expected_answer_at` set to that answer's `at`.
  Without it an answered ask is `409 ASK_CLOSED`.
  A stale value, or one sent for an open ask, is `409 ASK_ANSWER_CHANGED`; a different person is `403 NOT_ANSWERER`; an approval is `409 ASK_APPROVAL_REVIEW`.
  A change appends another `ask.answered` with `previous_answer`.
  An approval ask is moved to each document version before the human sees it, and an `ASK_EDITED` response means the question changed after the human reviewed it, so they reload and answer the moved request.
  On a block ask the answer is written into its block, and a changed answer without text removes the block's old `answer`.
  Its words and the options it chooses, whose labels are the asker's text, are weighed:
  where they would leave the document's markdown longer than 1 MiB and longer than it was, or making more than 65,536 elements and more than it did, the ask still takes the whole answer and the block is written without them, saying who answered and when.
  The answer's state and who gave it and when are not weighed, so the asks of a document already past those limits can still be answered.
  The answer is kept on the ask as well: a block that leaves the document and returns gets it back at settlement, which weighs each returning answer on its own in document order and keeps one on the ask alone, the block saying who answered and when, while writing it back would take the document past those limits and make it bigger.

- `/api/v1/asks/{id}/resolve` (POST; cookie, trusted header, or bearer)

  Retract or self-resolve an open ask with a recorded reason.
  On a block ask the block's `state` is written with it, and that is not weighed, so an ask on a document past what one upload may hold can still be resolved.
  On a block ask in the spec or the Verification document of an issue Legion runs, or one with a copy there, a writer that is no person and no architect of the issue or of an issue above it is `403 DOCUMENT_WRITER_NOT_ARCHITECT` (Document errors below).
  A reason beginning `removed from the document in version`, which marks a retraction settlement wrote and would have the retract undone when the block returns, is `400 INVALID_RESOLUTION`.
  The kind is `retracted` or `resolved`, and any other is `400 INVALID_RESOLUTION`: `withdrawn` is written only by an issue's close, and `declined` only by `POST /api/v1/asks/{id}/decline`, below.
  An ask on a closed issue is `409 ISSUE_CLOSED`; reopen the issue first.

- `/api/v1/asks/{id}/decline` (POST; cookie, trusted header, or bearer; needs the `person` scope)

  Decline to answer an open ask: the person closed the question without answering it.
  The body is `{note?, actor?}`, both optional:
  a person who writes no note declines all the same, and `actor` names the acting session a personal token's credential proves none of, as the answer route reads it.
  The note is trimmed, and one over 32,000 UTF-16 units (`contracts.DispatchEventBodyMax`, the bound it takes because the note rides the `ask.resolved` event) is `400 CAP_EXCEEDED`, with nothing written.
  The ask resolves with the kind `declined`:
  `reason` holds the person's own note alone, and `dispatch_words` the four sentences Dispatch writes for the asking agent (`model.DeclineDispatchWords`), which tell it to decide the next step without an answer, to ask again only if the facts change, and to write in its work what it assumed.
  No note is refused for how it opens, unlike a resolve's reason:
  a decline is no retraction, so nothing a person writes here can be read as settlement's own work.
  On a block ask the block's `state` is written as the resolve route writes it, and that is not weighed either.
  An ask of kind `approval` is `409 ASK_NOT_DECLINABLE`, decided inside the transaction under the ask's owner lock, since an ask's kind is known only once its row is read:
  an approval request is answered `Approve` or `Request changes`.
  An ask on a closed issue is `409 ISSUE_CLOSED`.
  The whole resolution rides `ask.resolved` to every session following the ask, and a write that opens another question on the issue is answered the issue's `declined_asks`.
  `GET /api/v1/agents/{session_id}/answers` lists the decline under the relation `declined_your_ask` for the ask's author and its followers, and under `declined_an_ask`, without Dispatch's sentences, for any other session it reached.

- `/api/v1/issues/{key}/comments?artifact=` (GET; cookie, trusted header, or bearer)

  List comments, optionally limited to an artifact ID.

- `/api/v1/issues/{key}/comments` (POST; cookie, trusted header, or bearer)

  Create a comment, root-level reply, or suggestion.
  An anchored comment uses the same quote-or-mark-ID shape as an ask; replies inherit their root's anchor and send none.
  A reply may name the block of the comment `reply_to` names that it answers, `reply_to_block` (`b2`), checked against that comment before `reply_to` climbs to its thread's head, and against that comment even when the climb reaches an ask;
  with `ask_id` instead of `reply_to` it names a part of that ask.
  `400 INVALID_COMMENT` with neither `reply_to` nor `ask_id`, and for a block or part the named item does not hold now.
  The delivery callback `POST /api/v1/comments/{id}/reply` takes `reply_to_block` for a block of comment `{id}`, never for a part of the ask its thread hangs under.
  A `reply_to` naming a comment its author deleted is `409 POST_DELETED`, the refusal every write that would add to a deleted post takes, read from the parent the create already locks and so before the named block is looked for;
  a reply written before the delete keeps its text and its place.
  A reply or error callback on `POST /api/v1/comments/{id}/reply` or `POST /api/v1/messages/{id}/reply` is accepted from the session the attempt names, and, for an attempt sent for a `role:` route, from the session the role's claim names when the reply arrives while the listener's role lookup (`GET /v1/roles/<role>`) calls it live: the session that took the role from the one Dispatch sent to, which the listener hands what that session had not read.
  Dispatch asks the listener before the reply's transaction opens, and only when the attempt names another session; a lookup the listener does not answer is `503 ENVOY_UNAVAILABLE`, and any other session, or the role's holder while it is not live, is `403 REPLY_FORBIDDEN`.
  The attempt's first stored reply stays its reply, and a later reply is answered as any second reply is (the stored reply; a message reply with `follow_up` is posted under the first).
  A comment's record in its document's margin - its body and replies, or a suggestion's and its replacement - holds at most 256 KiB of text, and a document's margin 1 MiB:
  a comment, reply or suggestion that would leave either past its bound and bigger is `413 CAP_EXCEEDED`, naming the bound and both sizes.
  A quote anchor that would leave the marks on the document's text carrying more than 1 MiB of ids and authors, and more than they carried, is `413 CAP_EXCEEDED` too, as for an ask's.

- `/api/v1/comments/{id}` (GET; cookie, trusted header, or bearer)

  Read a comment and its reply chain.
  Every comment a route or event returns carries `reply_block` (`{node, ref, block_id, text, removed}`), the block of the comment it answers, `null` when it names none:
  `text` the block's text cut to 480 characters, its last text once removed, and `removed` true while the comment's current revision no longer holds it.

- `/api/v1/comments/{id}/blocks` (GET; cookie, trusted header, or bearer)

  A comment's current blocks in body order:
  `{item, revision, indexed, blocks: [{id, type, text, ref, from, to, changed_revision, changed_at, embedding}]}`, `item` the comment's graph node, `revision` its indexed revision, `from` and `to` UTF-16 code units into its stored body, `embedding` where meaning search stands on the block:
  `embedded`, `pending`, or `none` while no window covers it.
  While the comment is owed a block index, or was indexed from another body than it stores, `indexed` is `false` and `blocks` empty; the read writes nothing.
  `400 COMMENT_ID_INPUT` for an id that is neither a whole uuid nor a unique prefix of 8 or more characters of one.

- `/api/v1/comments/{id}` (PATCH; cookie, trusted header, or bearer)

  Edit a comment body, its author alone (`403 NOT_AUTHOR`).
  Its rules are its description in the route table (`internal/dispatch/api/routes_table.go`), which `GET /api/v1` serves and the docs site's API reference prints.
  An edit that would leave the comment's margin record or its document's margin past its bound (above) and bigger is `413 CAP_EXCEEDED`.
  `blocks` (`[{id, from}]`, each block id the editor carried and where its span starts in `body`, in UTF-16 code units, in body order) with `base_revision` (the revision `.../blocks` answered) keeps each id on the new block its span starts in;
  with a `base_revision` that is no longer the comment's, or without `blocks`, the new blocks take ids by their text.
  `400 INVALID_COMMENT` naming the field for `blocks` without `base_revision`, and, at the current revision, for an id that is not a current block of the comment, an id twice, or a `from` outside `body` or before the one ahead of it.

- `/api/v1/comments/{id}` (DELETE; cookie, trusted header, or bearer)

  Delete a comment, its author alone (`403 NOT_AUTHOR`): the comment keeps its place, and no read of it gives its body again.
  Its rules are its description in the route table (`internal/dispatch/api/routes_table.go`), which `GET /api/v1` serves and the docs site's API reference prints.

- `/api/v1/comments/{id}/history?before=` (GET; cookie, trusted header, or bearer)

  The bodies an edit or a delete of this comment replaced, newest first: a delete writes an entry of its own, and a deleted comment's entries carry no body.
  Its rules are its description in the route table (`internal/dispatch/api/routes_table.go`), which `GET /api/v1` serves and the docs site's API reference prints.

- `/api/v1/comments/{id}/resolve` (POST; cookie, trusted header, or bearer)

  Resolve a comment.

- `/api/v1/comments/{id}/reopen` (POST; cookie, trusted header, or bearer; needs the `person` scope)

  Reopen a resolved thread-root comment.

- `/api/v1/comments/{id}/accept` (POST; cookie, trusted header, or bearer; needs the `person` scope)

  Apply and accept an anchored suggestion.
  An accept that would leave the document's markdown (what `.../text` answers) longer than 1 MiB and longer than it was, or making more than 65,536 elements and more than it did, is `413 CAP_EXCEEDED` and leaves the suggestion open.
  A change a concurrent browser deletion removes before the version is rendered is `409 EDIT_LOST_TO_CONCURRENT_CHANGE` and leaves the suggestion open;
  one removed after it answers `200` with `lost: true`.

- `/api/v1/comments/{id}/reject` (POST; cookie, trusted header, or bearer; needs the `person` scope)

  Reject a suggestion.

- `/api/v1/issues/{key}/messages` (POST; cookie, trusted header, or bearer)

  Post an issue message.
  A reply may name the block of the message `in_reply_to` names that it answers, `in_reply_to_block` (`b2`), as may `POST /api/v1/agents/{session_id}/messages` and, for a block of message `{id}`, `POST /api/v1/messages/{id}/reply` (not on a follow-up):
  `400 INVALID_MESSAGE` without `in_reply_to` and for a block that message does not hold now.
  A reply naming a block carries that block's text as `reply_body`.
  An `in_reply_to` naming a message its author deleted is `409 POST_DELETED`, as a `reply_to` naming a deleted comment is;
  a reply written before the delete keeps its text and its place.

- `/api/v1/issues/{key}/messages` (GET; cookie, trusted header, or bearer)

  List an issue's message threads: every root message, oldest first, unpaged, each with its delivery attempts and its replies.
  Every message a route or event returns carries `reply_block`, the block of the message it answers, as a comment does.

- `/api/v1/messages/{id}/blocks` (GET; cookie, trusted header, or bearer)

  A message's current blocks in body order:
  `{item, indexed, blocks: [{id, type, text, ref, from, to, embedding}]}`, `item` the message's graph node, `from` and `to` UTF-16 code units into its stored body, `ref` absent for a direct message's block, which has no address, `embedding` where meaning search stands on the block, as a comment's block carries it.
  Read under `GET /api/v1/messages/{id}`'s rule:
  a bearer reading a direct message names its session in `?session=` (`400 SESSION_REQUIRED`, `403 THREAD_FORBIDDEN`).
  While the message is owed a block index, `indexed` is `false` and `blocks` empty; the read writes nothing.

- `/api/v1/messages/{id}` (PATCH; cookie, trusted header, or bearer)

  Edit an issue message's body, its author alone (`403 NOT_AUTHOR`).
  Its rules are its description in the route table (`internal/dispatch/api/routes_table.go`), which `GET /api/v1` serves and the docs site's API reference prints.

- `/api/v1/messages/{id}` (DELETE; cookie, trusted header, or bearer)

  Delete an issue message, its author alone (`403 NOT_AUTHOR`): the message keeps its place, and no read of it gives its body again.
  Its rules are its description in the route table (`internal/dispatch/api/routes_table.go`), which `GET /api/v1` serves and the docs site's API reference prints.

- `/api/v1/messages/{id}/history?before=&session=` (GET; cookie, trusted header, or bearer)

  The bodies an edit or a delete of this message replaced, newest first: a delete writes an entry of its own, and a deleted message's entries carry no body.
  Its rules are its description in the route table (`internal/dispatch/api/routes_table.go`), which `GET /api/v1` serves and the docs site's API reference prints.

- `/api/v1/issues/{key}/results` (GET; cookie, trusted header, or bearer)

  GET lists the `check` blocks of the issue's Verification document in document order:
  `{issue, verification_artifact_id, checks}`, each check with its attributes (`when`, `surface`, `run`, `negative_control`, `ci_check`), the version its words last changed in, their digest, and every result recorded against it from any issue, newest first.

  - A check's results come newest first by `created_at`, the time the recording transaction began, with `id` descending breaking a tie, and a client that reads one result per check reads the first of them;
    `dispatch results` prints every one of them, each with the reading its role recorded.
    Two results recorded in transactions that overlapped can come back in either order.

  - `?at=<40-hex commit>` compares every check of the issue with that one commit and nothing else.
    The only read of GitHub it leaves is the check runs of a check CI proves, read at the issue's newest pull request head as on every read, so a derived result stands at the named commit only where that commit is that head.

  Each check carries `compared_with`, every commit and branch its results are compared with in the order they are asked about, `compared_nothing` where it has none, and `pass_stands`.
  Each basis is `{kind, repo?, commit?, branch?, pull_request?, learned_at?}`, `kind` `named`, `pull_request` or `branch`; `repo` is the `owner/repo` of a basis in a repository the issue links a pull request in and does not resolve to, and is absent from every basis of the issue's own repository, so pull request `#7` or branch `main` of that linked repository reads apart from the issue's own.
  Each result carries `commit_stands`, where its own commit stands against those, `check_words_moved`, and `retracted` where a retraction stands against it.

  - A `before_merge` check is compared with every head Dispatch holds for the check's own issue: the head of an `open` pull request, and the final head of a `merged` one, attributed to that issue.
    A pull request that the issue's external links name (`https://github.com/<owner>/<repo>/pull/<n>`) in a repository the issue does not resolve to counts as one attributed to it, since the attribution holds an issue to its own repository and can never give it that pull request (`delivery.Attributor.HeadsAttributedTo`, `linkedPullRequestTerms`); its basis names that repository in `repo`.
    The link is matched to the stored row by repository and number, so a push that moves that pull request's head moves the basis, and one that closed without a merge is no basis.
    The owner and the name are read by the patterns every name GitHub allows is read by, an owner of a letter or digit and then up to 38 letters, digits, `-` and `_` (a managed user's `mona-cat_octo` among them) and a name of letters, digits, `.`, `-` and `_`, and a name of `.` or `..` alone names no pull request, so a link cannot send a read of GitHub up the path.
    A link to a pull request of the issue's own repository adds nothing: the attribution alone decides which pull requests of that repository are the issue's.

  - An `after_merge` check is compared with the branch the check's issue's pull request merged into — the `base_ref` of the newest merged pull request attributed to that issue — and with that repository's default branch, which answers alone where Dispatch holds no merge of the issue or the merged row names no base branch.

  - A `default_branch` check is compared with that repository's default branch alone, whatever branch the pull request merged into.

  - A result of an `after_merge` or `default_branch` check whose commit the record route took to be of a repository the issue links a pull request in is compared in that repository:
    the record route keeps that repository with the result (`check_results.commit_repo`, POST below), and the read asks that repository's branches, for `after_merge` the base branch of the merged linked pull request (`delivery.Attributor.BaseBranchMergedIn`) and then its default branch, for `default_branch` its default branch alone; the branch the result stands at names that repository in `repo`.
    Asked of the issue's own repository, GitHub answers `404` for such a commit.
    A `before_merge` result of that kind is read through that repository, below, for the same reason.
    Every other result keeps no repository and is compared as above.

  - A check compared with nothing says why in `compared_nothing`:
    `no_pull_request_or_branch` where the issue resolves to no repository, and where a `before_merge` check's issue holds no pull request at all;
    `repository_unreadable` where the App cannot read that repository and Dispatch can name no branch of it to ask a stored answer about;
    `read_failed` where a read of Dispatch's own did not answer.

  - `commit_stands` is `{answer: "at_basis", basis}` where one of those commits is the result's own, or one of those branches carries it, `{answer: "other_commit"}` where none of them does, and `{answer: "unknown", reason}` where Dispatch cannot say.
    A commit of a repository the App cannot read is unknown wherever nothing places it, whatever the check's `when`, and so is one left by a read of Dispatch's own that failed:
    a commit there may head a pull request Dispatch was never able to learn.
    A branch check still places a commit a stored `branch_commits` yes carries, since that answer is keyed by a branch and stands for good.
    An earlier branch of an `after_merge` check is the one exception, and only where GitHub refused the read of it — a 403 or a 404, which a base branch GitHub no longer holds answers with: that says the branch does not carry the commit, and the default branch decides.
    Every other failure of that read, a rate limit, a 5xx, a timeout or a request whose caller went away, leaves the answer `unknown` wherever no branch carries the commit, and never `other_commit`.
    An issue that resolves to no repository is never unknown, since Dispatch holds every head such an issue will ever have, so a commit none of them names is another commit; the one exception is a `before_merge` result whose `check_results.commit_repo` names a linked repository, which that repository decides, as the next sentence says.
    A result whose commit is taken to be of a repository the issue links a pull request in (`check_results.commit_repo`) is decided by that repository: a `before_merge` one is `unknown` where Dispatch cannot read it and `other_commit` where no compared head is its commit, and a branch check's walks that repository's branches.
    Such an `other_commit` or `unknown` answer carries that repository as `repo` (`owner/name`), so a reader names it rather than the issue's own repository; an answer about the issue's own repository carries none, and an `at_basis` answer names a linked repository in its `basis`.

  - `check_words_moved` is whether the check's words changed after the result was recorded, which the digest the result kept decides against the block's digest now.
    It is a second fact and a second field: a result at a compared commit whose check has been reworded since is both, and one word for both would hide which.

  - `pass_stands` is `{answer, roles}` for the check as a whole: `yes`, with the roles of the passes that stand, while a result of it passed, stands at one of the compared commits, kept the check's words and carries no retraction;
    `unknown` where the repository is unreadable, for a check CI proves whose check runs GitHub could not be read (`ci.state` `unread`), or where such a pass is held up only by a commit Dispatch could not place;
    `no` otherwise, which a check compared with nothing for `no_pull_request_or_branch` answers, since there is nothing for a pass to stand at.

  - `currency` is a compatibility field for `dispatch` clients older than pi-envoy 17.1.0, which print it beside each result and read none of the fields above (`api/results_currency.go`).
    It is `current` where `commit_stands` is `at_basis`, `check_words_moved` is false and no retraction stands, and `older` where `commit_stands` is `other_commit` or it is `at_basis` and `check_words_moved` is true.
    It is left out everywhere else, where those clients print "unknown": a commit Dispatch cannot place, a retracted result at its basis under the check's words now, and every result of a read that names `?at=`.
    It is derived from those fields alone, with no read of its own, and it is removed once no client older than pi-envoy 17.1.0 is in use.

  Where the App can read that repository, a `before_merge` result whose commit heads no such pull request is read through GitHub itself before the answer is given, which is what makes a result recorded before its own pull request opened stand at that head once the pull request opens at its commit:

  - One `GET /repos/{owner}/{repo}/commits/{sha}/pulls`, and every head GitHub names is written (`delivery.Attributor.ReadThroughCommitPulls`).
    It asks the repository the result's `check_results.commit_repo` names where it names one, and the repository the issue resolves to otherwise: asked of the issue's own repository, a commit only a linked repository holds names no pull request on any read, and a linked pull request whose head reaches it after the record would never be learned.

  - Without that read it would stand at no basis for ever:
    the read-through the record itself makes runs once, while no pull request carries the commit, and a deployment holding no delivery settings binds no intake durable and so consumes no `pull_request` event at all (`delivery.Intake.Run`).

  - One repository and commit costs at most one such read every `delivery.CommitPullReadStaleAfter` (five minutes), however often the Verification tab refetches.

  - Eligibility for that read is the repository and commit's, never one result's:
    it is skipped only where GitHub's answer cannot change the result
    — Dispatch already holds an open or merged pull request whose head is that commit, whatever issue it is attributed to, since its own passes keep reading that row;
    or a read has already been told of a pull request whose head is another commit, which answers about this commit for now and holds the pair off for `delivery.CommitPullSettledFor` (one hour, `delivery.CommitPullReads`), after which it is read through again, since a pull request opened later can have its head at that commit.

  A check CI proves is one whose `ci_check` names a GitHub check run, with `when` `before_merge`, no `run`, a `negative_control`, and no result recorded against it (`validCICheck`, `docs.CICheckRuleIn`).
  No role records its result (POST below refuses one), and nothing is stored for it:
  each read takes the head of the issue's `open` pull request of the highest number, else the final head of its `merged` one of the highest number (`delivery.NewestPullRequestHead`), and reads that commit's check runs once, every page, under one 10-second deadline (`api/results_ci.go`, `deriveCIResults`).
  Of the runs named exactly as `ci_check` says, the one of the highest id decides.
  An issue holding no such check makes no read of check runs at all.

  - Such a check carries `ci`, `{run, state, case, detail, line}`.
    `state` is `pass`, `fail`, `no_result` or `unread`.
    `case` is the conclusion that decided it — `success` passes, and `failure`, `timed_out`, `action_required`, `startup_failure`, `stale` and a conclusion this list does not name, as GitHub wrote it, fail — or one of the cases of no result:
    `neutral`, `skipped`, `cancelled`, `not_completed`, `absent` (no run of that name at the head) and `no_head` (Dispatch holds no open or merged pull request head of the issue), each `no_result`, and `github_unreadable` (`unread`: no App key, a token GitHub would not mint, an HTTP error, a rate limit or the deadline).
    `detail` is one sentence naming the case, at most 2,000 runes; for `github_unreadable` it names GitHub's own answer, a refusal by its status and the `message` of its JSON body, never the body itself and never a token.
    `line` is the pull-request body line for the check: `- Check <id> (before_merge): <verdict> at <sha>, from CI run <name>. <the check's words>` for a pass or a fail, and `- Check <id> (before_merge): CI run <name> has no result (<case>)` for every other state; the check's words are kept, and the words cut, as a recorded line's reading is (POST below), so `text` keeps an issue key the line leaves out.
    The run's name is written word for word, its blanks as one, only when the line keeps every word of it; a name holding a word the line drops, such as an issue key or a word holding `_`, reads `the CI run the check names`, and `ci_check` and `run` keep it whole.

  - For a pass or a fail its `results` hold the one result derived from that run, and nothing else:
    `id` `ci-run:<check run id>`, `role` `ci`, `actor` `{kind: "github_app", id: <the slug of the App that made the run>}`, `session` `""`, `run` the run's name, `surface` and `negative_control` the check's, `commit_sha` the head read, `created_at` the run's `completed_at`, `observed` the conclusion, then the run's output title and summary cut together to 2,000 runes, then its URL, one a line, `check_sha256` the check's own digest, the counts 0, `screenshots` `[]`, and no `event_seq`.
    It is judged as a recorded result is (`comparisonReader`), so its `commit_stands` and the check's `pass_stands` read as a recorded result's would.

  A check carrying `ci_check` that breaks one of those rules, or that holds a recorded result, reads as an ordinary check with its recorded results.

  The issue a result was recorded on is read for nothing here, so a child's pull request is never a basis for a check above it.
  Nothing is written on a push: a push that moves a pull request's head leaves the result at the old head standing at no basis, by itself.
  An issue with no Verification document answers no checks.

- `/api/v1/issues/{key}/results` (POST; cookie, trusted header, or bearer)

  POST records one result: `{check, role, verdict, commit, surface?, run?, observed?, negative_control?, failed?, skipped?, excluded?, screenshots?, actor?}`.
  What it takes, and what it refuses:

  - `{key}` is the issue the result is recorded on;
    `check` names a check of its Verification document or of one of its ancestors', by block id or, when a block id is not unique in that set, by its canonical block reference (`409 CHECK_AMBIGUOUS`); any other id is `400 UNKNOWN_CHECK`.

  - A check CI proves (GET above) is `409 CHECK_PROVED_BY_CI` for every role, naming the block and the check run, since its result is read from that run;
    the test runs right after the check is resolved, before GitHub is asked about the commit, and again inside the recording transaction, where whether a result is recorded against the block is read again.

  - `role` is at most 32 characters of `[a-z-]`, `verdict` is `pass` or `fail`, `commit` is 40 lowercase hex, and absent counts are 0;
    a `pass` counting any failed, skipped or excluded case is `400 RESULT_CONTRADICTS`, and any other malformed field `400 RESULT_INPUT`.

  - Dispatch asks GitHub whether the repository the check's issue resolves to holds that commit, before it writes anything.
    When GitHub does not say it does — it says that repository does not hold it, or its read did not answer — Dispatch reads the external links of the check's issue: each link of the form `https://github.com/<owner>/<repo>/pull/<n>` names that repository, and the repository counts when GitHub says one of those pull requests is open or merged (`delivery.PullRequestOpenOrMerged`).
    A pull request that closed without a merge does not make its repository count, and neither does one GitHub answers `404` or `410` for (a mistyped link, or a pull request since deleted), nor a repository the App is not installed on, nor one it cannot read (GitHub's `403` that is no rate limit, as `githubapp.RateLimit` reads one): each is GitHub's answer, not a failed read, and no retry changes it until someone gives the App the repository.
    One record reads at most 10 linked pull requests, one after another, and each distinct repository once, until one of its pull requests counts (`linkedPullRequestReads`); a link past the bound is left unread.
    A link names a pull request only where the pattern the links are read by takes it (`delivery.Attributor.LinkedPullRequests`): an owner that is a login GitHub serves, and a name of letters, digits, `.`, `-` and `_` that is neither `.` nor `..`; any other link, such as one whose owner is `-acme` or `my.org`, spends no read, and no refusal names it.
    A link that pattern takes whose repository's name GitHub's naming rule then refuses (such as `acme/.git`, which is empty once `.git` is trimmed) does not count either: it spends one of the record's 10 reads, makes no GitHub request, and the refusal below names its repository as one whose links Dispatch set aside, which no grant of the App mends.
    Dispatch then asks GitHub whether each repository that counts holds the commit, and refuses a result at a commit that no repository it asked holds with `400 RESULT_COMMIT_UNKNOWN`, naming the commit, each `owner/repo` it asked, each linked pull request of a repository the App is not installed on or cannot read with its reason (`owner/repo#n (repository_unreadable)`), as a pull request it could not read, at most 10 of those and a count of the rest, or, where it read that pull request and could not read the commit, as the one in whose repository it could not read the commit, every one of those, since each spent one of the record's 10 reads, and that the App is to be granted that repository or a readable pull request linked, each repository whose links it set aside for the naming rule, and how many linked pull requests the bound left unread where it left any.
    So an issue whose work is in two repositories takes a result at the head of the pull request it links in the second repository, also while GitHub cannot answer for the first.

  - A read of a linked pull request's state or of a linked repository's commit that does not answer — a rate limit, a 5xx, a timeout — refuses the record where no repository is shown to hold the commit, with `502 GITHUB_UNREADABLE` naming that repository, the commit and GitHub's own message, and stores nothing:
    the repository may hold the commit, and stored under it the result would sit at a commit no read showed any repository to hold.
    The refusal tells the caller to record the result again, and a rate limit names GitHub's wait, in the sentence and in `Retry-After`.

  - A failed read of Dispatch's own rows of the issue's links refuses the record with `500 INTERNAL` and stores nothing: the links are read, before anything is written, wherever GitHub does not say that the issue's own repository holds the commit.

  - Otherwise only GitHub's `422 No commit found for SHA` refuses, and only once it has said so of the issue's own repository:
    a deployment with no App key and an issue resolving to no repository store the result as before, and so do an issue's own repository the App cannot read (GitHub answers `404` for that; a linked repository the App is not installed on or cannot read is one that does not count, as above), a rate limit, a timeout and a failed read of the issue's own repository, each where every linked read answered.
    Any of those four reads of the issue's own repository beside a linked read that did not answer is the `502 GITHUB_UNREADABLE` above.
    The stored result keeps a linked repository (`check_results.commit_repo`) where GitHub said that repository holds the commit.
    Where the read of the issue's own repository did not answer, every linked read answered and no linked repository holds the commit, the result keeps none, as before.
    After the write, the read-through of the pull requests GitHub names for the commit reads the repository the commit is taken to be of, that linked repository or the issue's own, so the pull request of a linked repository is learned from that repository.

  - A screenshot names an image document of the recording issue or of the check's issue.

  The result is appended as `result.recorded` on the check's own issue, naming the recording issue.
  A closed issue, the recording one or the check's, takes a result only for a check whose `when` is `after_merge` or `default_branch` — the production check after the merge
  — and refuses every other with `409 ISSUE_CLOSED`; every other write on a closed issue stays refused.
  The answer is `{result, line}`, `line` the pull-request body line the result implies:

  - The line is `- Check <id> (<when>): <verdict> at <commit>, recorded by <role>.` and the reading.
    It goes into a pull request, which can be public, so the reading keeps only plain words: a word every rune of which is a visible letter, a digit or one of `. , ; : ! ? ' " - ( ) /`.
    Every other rune drops its word whole: many of them, such as emphasis and strikethrough (`*`, `_`, `~`), code, HTML, links, escapes, character references, math, mentions and every Unicode dash, quotation mark or invisible character, can split, join, hide or forge a key or a host once the pull request renders it, and the rule allows these few runes rather than list which; `Vec<u8>`, `$PATH`, `ENVOY_TOKEN`, `a->b` and `it’s` leave the line too, while the record keeps the whole reading.
    Of the plain words it also leaves out every one that holds `://`, a host below `.internal.` or ending in `.internal`, a Tailscale MagicDNS host, this Dispatch's own host, or an issue key of a project this Dispatch holds, judged as written and under NFKC; a public host written as `www.` and a domain, which GitHub renders as a link, stays.
    The public text check refuses a host below `.internal.` everywhere, and one ending in `.internal` only in a pull request's own text, where it still lets `metadata.google.internal` and `host.docker.internal` pass: over the whole repository such names are well known, while the line is pasted into public text with no reader in between and drops them too.
    A key counts in any case, at the start of a word or after a character that is neither a letter nor a digit, and up to the end of its number: `ACME-12's`, `(ACME-12)` and `fix/acme-12-lines` each hold one.
    `UTF-8`, `SHA-256` and `TEST-1` hold none while no project is called UTF, SHA or TEST, and `kernel-6.8` holds none even where KERNEL is a project, since a number that runs on into a decimal part is a version.
    What is left, when it is longer than 80 runes, is cut at the last blank within its first 80 runes, or, where those runes hold no blank, at the limit itself, with `…` where it was cut; a cut text is judged again, since a cut at the limit can fall inside a word and leave a key the whole word did not hold; a reading of which nothing is left, before the cut or after that second judging, says `The reading holds only words this line leaves out.`, and a result with no reading says `The role recorded no reading.`
    Words are judged only until what is kept passes 80 runes, since the cut reads no further.
    No line is stored: each is written when it is answered, from the projects Dispatch holds then.

- `/api/v1/issues/{key}/results/dry-run` (POST; cookie, trusted header, or bearer)

  The dry run of a result's record, which `dispatch result --dry-run` sends: it takes the record's body and refuses it as the record does before it writes anything (`400 RESULT_INPUT`, `400 RESULT_CONTRADICTS`, `400 UNKNOWN_CHECK`, `409 CHECK_AMBIGUOUS`, `409 CHECK_PROVED_BY_CI`, `400 RESULT_COMMIT_UNKNOWN`, `502 GITHUB_UNREADABLE`).
  Otherwise it answers `200 {line}`, the line the record would answer, and records nothing: no row and no event.
  A closed issue, a screenshot and a check reworded while the record writes are refused by the record alone, since the record judges them under its locks.
  The dry run is a path of its own, not a flag on the record: a release that does not serve it answers `404`, where it would drop a flag it did not know and record the result.

- `/api/v1/issues/{key}/coverage` (GET; cookie, trusted header, or bearer)

  What each requirement of the issue's spec lacks: `{issue, spec_artifact_id, has_plan, requirements, gaps}`.

  A requirement is a `requirement` block of the spec, and `requirements` lists them in document order, each with its block reference, its words, and how many `check` and `plan_task` blocks name it in `serves`.

  A block counts when it is in any document of the issue or of any descendant issue, at any status;
  the descendants are one recursive `child_of` walk bounded by `refs.IssueDepthCap` (`refs.IssueDescendants`).

  `gaps` is one line per gap, `<block id> has no check` and `<block id> has no plan task`, the second only once the issue has a plan document, or the single line `the spec marks no requirements` for a spec that marks none.

  Nothing is ever refused for a gap.

  This is not `GET /api/v1/search/coverage`, which reports how much of what meaning search ranks carries a vector.

- `/api/v1/issues/{key}/handoffs/{phase}` (POST; bearer (an agent session naming itself in `actor`))

  Record the session's handoff for one phase of the issue, `{actor, body}`:
  `body` is one JSON object as a string of at most `contracts.DispatchHandoffBodyMax` (262,144) bytes, stored exactly as sent, and the record is the phase's next version from 1, numbered under the issue's owner lock.
  A cookie caller is `403 HANDOFF_FORBIDDEN`, an actor that is not a session `400 ACTOR_KIND`, a phase that is not a lowercase slug or a body that is not one JSON object `400 HANDOFF_INPUT`, an over-cap body `413 CAP_EXCEEDED` naming the cap, and a closed issue `409 ISSUE_CLOSED`.
  Answers `201 {id, issue_key, phase, version, author, created_at}` and appends `handoff.recorded` `{handoff_id, phase, version}` in the same transaction, which never carries the body and notifies no one.
  A keyed write: a retry under the same `Idempotency-Key` is answered the first call's `201` and records no second version.

- `/api/v1/issues/{key}/handoffs` (GET; cookie, trusted header, or bearer)

  List the issue's handoffs, newest first, without their bodies; a closed issue's read too.

- `/api/v1/issues/{key}/handoffs/{phase}/latest` (GET; cookie, trusted header, or bearer)

  The phase's newest handoff with its body, byte for byte as written; `404 HANDOFF_NOT_FOUND` when it has none.

- `/api/v1/issues/{key}/role-holders` (PUT; cookie, trusted header, or bearer; the actor must be the Legion daemon's session)

  Publish the session that holds each Legion role of the issue: `{holders: [{role, session}], actor}`, each pair naming both its role and its session.
  The role is one of `architect`, `planner`, `implementer`, `tester`, `reviewer` and `merger`.
  The session is a session id (`text.IsSessionID`) of at most 256 bytes: it holds no whitespace, no control character, no backtick and none of `/?#<>"'[]|`.
  The body is the whole set of the issue, and `[]` is an issue where no session holds a role.
  The write replaces the stored set, so a session that the body does not name holds no role of the issue.
  A pair that did not change keeps its `recorded_at`.
  The actor must be the session `legion-daemon:<project>`, the key up to its first `-`, which is the session that the daemon of the issue's project writes as.
  Any other actor is `403 ROLE_HOLDER_FORBIDDEN`.
  Dispatch takes the session that a bearer caller names as the caller names it.
  So any caller whose Dispatch credential proves no session of its own can name the daemon's session and write the set: the deployment's shared token, and a person's own agent token whatever its scopes.
  A project's proven-sessions setting closes that case for the project: while `proven_sessions` is on (`/api/v1/projects/{key}/proven-sessions`, above), the route takes a write only on a credential that proves the session `legion-daemon:<project>`, a broker token of that session, and answers every other `403 ROLE_HOLDER_NOT_PROVEN`, after its `400` and `404` refusals.
  While the setting is off, the case stays open.
  A body without `holders`, a role that is not one of the six, a session that is not a session id or is longer than 256 bytes, a role or a session named twice and an empty session are each `400 INVALID_ROLE_HOLDER`, and nothing is stored.
  An issue that Dispatch does not know is `404 ISSUE_NOT_FOUND`.
  The write takes a closed issue, appends no issue event and notifies nobody.
  Each publication the route takes marks the issue's project (`project_role_holder_marks`) in the transaction that writes the set, an empty set included, and a refused one marks nothing.
  The mark puts the issues of the project that Legion runs under the check of who writes their spec and Verification document (`403 DOCUMENT_WRITER_NOT_ARCHITECT`, Document errors below).
  The answer is `200 {issue, holders}`, the set that this write stored, each holder `{role, session, recorded_at}`, in workflow order.

  - **Two writes of one issue.**
    A write holds the issue's owner lock (`store.LockOwner`), which a second write of the same issue waits for, so the stored set is always one whole set.
    When two writes of one issue run at the same time, the one that finishes last wins.
    The daemon sends one write of an issue at a time: its outbox holds one `role_holders_publish` row for each issue, and a lane runs the rows of its issue one at a time (`packages/daemon/internal/daemon/outbox_lanes.go`).
    A claim change while the daemon's write of that issue is in flight leaves the older set stored until the outbox's first sweep after that row's lease ends: the lease ends 30 s after its last renewal, and the sweep runs every 30 s, so the older set can stay for about a minute before the row runs again with the live set.

  - **When the daemon publishes.**
    The daemon publishes the set when a claim readies, at each boot, and at each claim write that changes the session that holds a role: the agent registers, or the claim retires, fails, is suspended or launches again.
    The claim write queues the publication in its own transaction in the daemon's store, so the change and its publication commit together or not at all.
    An end whose claim write failed with an error that the daemon owes, such as a lost connection, is published when the daemon makes that write again at the claim's next decision, and not before: until then the store still holds the claim live.
    The session of an ended claim keeps its role until the next publication lands, and in that time a process whose stop failed can still write with the pane's Dispatch bearer.
    An end whose claim write failed and is not owed leaves the daemon's memory ended and its store live, and the set follows the store (dispatch://INFRA-4099).
    A later decision of that claim, such as a second stop, makes that write again only while the claim still holds a pending task that the decision retires, such as the task of a turn that is over, because the retirement writes the claim with it; otherwise the session stays listed until another write of that claim lands, and the daemon's next start takes the claim up from its row as live.
    Such ends include a suspension, a stop or a failure whose claim write the database refused, and a stop whose retirement of its round's ask failed.
    The boot publishes each issue with a live claim or a claim that changed in the last seven days, for the claims that an earlier release changed and did not publish.
    The daemon ends a publication that this route refuses with `400`, and logs one ERROR line with the issue and the code; the next ready, end or other change of a claim on the issue queues a fresh publication of the live set.
    A live claim whose session this route refuses stops each publication of its issue in that way, with one ERROR line each time, until that claim ends.

- `/api/v1/issues/{key}/role-holders` (GET; cookie, trusted header, or bearer)

  The set that the daemon last published: `{issue, holders}` in workflow order, and `[]` when no session holds a role.
  A closed issue reads too, and an issue that Dispatch does not know is `404 ISSUE_NOT_FOUND`.

- `/api/v1/handoffs/{id}` (GET; cookie, trusted header, or bearer)

  One handoff by id with its body; `404 HANDOFF_NOT_FOUND` when there is none, `400 HANDOFF_ID_INPUT` for an id that is not a uuid.

- `/api/v1/issues/{key}/artifacts` (GET, POST; cookie, trusted header, or bearer)

  List issue artifacts or create a version from a multipart `file` or JSON `{name, content, summary?, actor?}`.
  The JSON form requires `Content-Type: application/json`.
  A `text/markdown` file is a document.
  Any other file's kind is read from its bytes, never from the part's declared `Content-Type`:
  it is `image` only when its bytes are a PNG, JPEG, GIF or WebP, stored with `mime` the type its bytes are (a JPEG declared `image/png` is `image/jpeg`), and otherwise a `file` under its declared type, an SVG among them, so no client stores other bytes as an image.
  An upload's name finds the issue's artifact whose name or slug it is, so a name that is an artifact's slug (`plan-md` for `plan.md`) is that artifact's next version, never a second artifact the same reference would also name.
  The command bar's in-process caller makes an upload a create alone, or conditional on the markdown the document holds, by a condition it carries on the request's context and never in the body: a create alone where the issue holds an artifact the name finds when the upload takes the owner's lock is `409 DOCUMENT_EXISTS`, and an upload over a document that no longer holds the expected markdown `409 DOCUMENT_CHANGED`, each with nothing written.
  A body naming `expect_new` or `expect_sha256`, from any client, is answered as one that names neither.

  - A document an issue first holds under `verification.md`, `plan.md` or `decisions.md` carries that role (`artifacts.role`, `verification`, `plan` or `decisions`), which the row keeps for good:
    a later upload of the same name is that document's next version, and no upload moves a role.
    An issue holds at most one document of each role, and the issue read names all three (`verification_artifact_id`, `plan_artifact_id` and `decisions_artifact_id`), as the Verification and Plan tabs find them and as the design gate's `decisions` step finds its own document.

  - A document stored under one of those names before its role existed carries no role, and a reader finds it by its slug instead;
    the next upload of that name sets the role on the row it already has.

  - A role decides what a document is for: `POST /api/v1/artifacts/{id}/plan-reviews` takes only a `plan`, `POST /api/v1/artifacts/{id}/approval-requests` refuses one, and `--include-verification` and `GET .../results` read the `verification` document's `check` blocks.

  A version whose kind is not its artifact's is `400 ARTIFACT_KIND_MISMATCH`, naming the artifact's slug, name and kind.
  A name no artifact has yet is `400 INVALID_ARTIFACT`, naming the bound, when it, or the reference key `<owner>/<slug>` it gives the new artifact, is over 2692 bytes, the most a B-tree entry of `artifacts_ref_key_key` holds of incompressible text;
  an upload under an existing artifact's name, one stored longer included, is never refused for its length.
  An ask block whose body breaks its content rule (`paragraph+ bullet_list?`:
  one or more paragraphs, then at most one bullet list, last) is `400 INVALID_ASK_BLOCK`; a new version is held to it only for the asks it writes or changes.
  A check block that breaks a `ci_check` rule is `400 INVALID_CI_CHECK` (Document errors below).
  On an issue Legion runs, an upload of its spec or Verification document, a file under the name `verification.md` among them, from a writer that is no person and no architect of the issue or of an issue above it is `403 DOCUMENT_WRITER_NOT_ARCHITECT` (Document errors below).
  A markdown document over 1 MiB, or any file over 25 MiB, is `413 CAP_EXCEEDED`, and so is a document whose markdown makes more than 65,536 elements (weighed as Element weights below lists), naming the line that passes the limit;
  a document or version whose markdown as stored (what `.../text` answers) would be longer than 1 MiB and longer than the document's was, or make more than 65,536 elements and more than the document's did, or that would leave a live state ygo could not decode as one update;
  or whose formatting is more items than one document update can store (1,048,576), naming the count.
  A file the configured bucket refuses is `502 FILE_STORE_UNAVAILABLE`, with no version written.
  A fresh POST to a closed issue is `409 ISSUE_CLOSED` before its body is spooled, keyed or not, and claims no key;
  a repeat under a key Dispatch answers itself (a stored answer, a first still running, a write that committed and lost its answer) is answered as that key says whatever the issue's state now, so an upload that landed before the close is replayed its `201`;
  a malformed `Idempotency-Key` is `400 KEY_INVALID` first.

- `/api/v1/projects/{key}/artifacts` (GET, POST; cookie, trusted header, or bearer)

  List non-primary project artifacts (or only unlinked documents with `?unlinked=true`), or create an unlinked project artifact, its kind read, and a new artifact's name held to 2692 bytes, as an issue upload's are, the command bar's conditions holding as they do there.
  An ask block whose body breaks its content rule (`paragraph+ bullet_list?`:
  one or more paragraphs, then at most one bullet list, last) is `400 INVALID_ASK_BLOCK`; a new version is held to it only for the asks it writes or changes.
  A check block that breaks a `ci_check` rule is `400 INVALID_CI_CHECK` (Document errors below).
  A markdown document over 1 MiB, or any file over 25 MiB, is `413 CAP_EXCEEDED`, and so is a document whose markdown makes more than 65,536 elements (weighed as Element weights below lists), naming the line that passes the limit;
  a document or version whose markdown as stored (what `.../text` answers) would be longer than 1 MiB and longer than the document's was, or make more than 65,536 elements and more than the document's did, or that would leave a live state ygo could not decode as one update;
  or whose formatting is more items than one document update can store (1,048,576), naming the count.
  A file the configured bucket refuses is `502 FILE_STORE_UNAVAILABLE`, with no version written.

- `/api/v1/artifacts/{id}` (GET; cookie, trusted header, or bearer)

  Read an artifact, its versions, and incoming references.
  `{id}` must be a UUID.
  Every version on every artifact read and list (this route, the issue and project artifact lists, the issue read, the reference reads, and each version write's answer and event) carries `size`:
  the bytes it stores, a file's or an image's bytes or a document's markdown, read from the row's own length without rendering the document or loading its history.

- `/api/v1/artifacts/{id}/rebuild` (POST; cookie, trusted header, or bearer; needs the `person` scope)

  Rebuild a document only when its durable history cannot load (`409 DOCUMENT_UNLOADABLE` on its reads):
  deletes its document updates, checkpoints, and snapshots, then writes one fresh update from the latest saved version or optional `{markdown}`.
  A supplied markdown source that changes the document writes the next immutable version, emits `artifact.version`, and moves an open approval request to that version, where it waits on its agent, as every version does;
  an omitted source keeps the existing latest-version behavior.
  The rebuild, that version, its event and the move commit in one transaction, and every refusal comes before anything is written:
  a live room is `409 DOCUMENT_LIVE`; a healthy, non-resident document is `409 DOCUMENT_LOADS`;
  supplied markdown on a closed issue's document is `409 ISSUE_CLOSED`;
  supplied markdown held to a new document's limits - more than 65,536 elements, or stored as markdown longer than 1 MiB or making more than 65,536 elements - is `413 CAP_EXCEEDED`.
  Supplied markdown holding a check block that breaks a `ci_check` rule is `400 INVALID_CI_CHECK` (Document errors below).
  The latest saved version rebuilds whatever it weighs, except a document whose tree is more items than ygo decodes in one update (1,048,576), which no document can hold and which is `413 CAP_EXCEEDED` naming the count, from either source.
  A refused or failed rebuild leaves the document unchanged.
  It answers the report, whose `source_version` is the version the rebuilt document holds: its latest saved version, or the version supplied markdown wrote.

- `/api/v1/artifacts/{id}/text` (GET; cookie, trusted header, or bearer)

  Read a live document's markdown.
  `{id}` must be a UUID.
  A stored tree outside the Proof schema is `409 DOC_SCHEMA`, a stored tree holding an element this build cannot read `409 DOC_NEWER_CONTENT`, and a stored history that cannot load `409 DOCUMENT_UNLOADABLE` (Document errors).

- `/api/v1/artifacts/{id}/versions/{n}` (GET; cookie, trusted header, or bearer)

  Read a document version as JSON, or download any version.
  `{id}` must be a UUID.
  A file whose bytes are in the bucket is `502 FILE_STORE_UNAVAILABLE` when the bucket does not answer, `503` on a server with no bucket configured.
  A file or image version, whoever owns it, is served as `Content-Disposition: attachment` under the artifact's stored name, with `-v<number>` before the name's last `.` for a version older than the latest (`report-v2.pdf`; `Makefile-v1` and `.env-v1` for a name with no `.` after its first character), so a saved file is not named after the version number its address ends in.
  It carries `Cache-Control: private, max-age=31536000, immutable` beside its `ETag` (the bytes' sha256), since a version's bytes never change and the route is authenticated;
  a document version read without `?download=true`, and every error, carries no cache header.
  `?download=true` is the download address, which the web app's Download links use (exactly `true`; any other value answers as without it):
  every kind answers there `Content-Disposition: attachment` under that name and `Cache-Control: private, no-cache`, so a browser asks again before it uses a copy it holds and saves under the name of the moment, an older version's `-v<number>` included once a newer version arrives.
  A document version answers there its Markdown, the text the JSON's `markdown` field holds, rather than the JSON, as `text/markdown; charset=utf-8`, its name given `.md` first when it ends in neither `.md` nor `.markdown`, whatever their case (`Launch notes-v1.md`).
  `?blocks=true` (exactly `true`) carries the document version's typed blocks under `blocks`, each read from that version's own text under the id its opening line writes, with its `type`, its `attributes`, its `token` and its `from` and `to` byte range in the `markdown` of the same answer.
  That `markdown`, with `sha256` and `size`, is the version as this build renders it, which is the stored text unless an earlier renderer wrote it otherwise; a version holding an element this build cannot read is then `409 DOC_NEWER_CONTENT`.
  Untyped blocks are not listed, because the stored text names no id for them; a listed block's `references` read zero and it carries no `embedding`, which describe the live document.
  A build before this one ignores the parameter and answers no `blocks` key.

- `/api/v1/artifacts/{id}/versions` (POST; cookie, trusted header, or bearer)

  Create a named live-document version.
  `{id}` must be a UUID.
  On an issue Legion runs, a named version of its spec or Verification document from a writer that is no person and no architect of the issue or of an issue above it is `403 DOCUMENT_WRITER_NOT_ARCHITECT` (Document errors below).

- `/api/v1/artifacts/{id}/edits` (POST; cookie, trusted header, or bearer)

  Apply document edit operations.
  `{id}` must be a UUID.
  An edit is `400 INVALID_ASK_BLOCK` when an ask it writes or changes breaks its content rule (`paragraph+ bullet_list?`) or holds what settlement cannot read;
  an ask it carries through unchanged is not its to refuse.
  A check block it adds or changes that breaks a `ci_check` rule is `400 INVALID_CI_CHECK` (Document errors below).
  On an issue Legion runs, an edit of its spec or Verification document from a writer that is no person and no architect of the issue or of an issue above it is `403 DOCUMENT_WRITER_NOT_ARCHITECT` (Document errors below).
  A change a concurrent browser deletion removes before the version is rendered is `409 EDIT_LOST_TO_CONCURRENT_CHANGE` and writes nothing;
  one removed after it answers `200` with `lost_ops`.
  A batch whose markdown (its inserts and replaces together) makes more than 65,536 elements, or that would leave the document's markdown (what `.../text` answers) longer than 1 MiB and longer than it was, or making more than 65,536 elements and more than it did, or a live state ygo could not decode as one update, is `413 CAP_EXCEEDED` with the refusal itself as the error, and writes nothing.

- `/api/v1/artifacts/{id}/approval-requests` (POST; cookie, trusted header, or bearer)

  Ask a human to approve a document's latest settled version.
  With `include_verification` it asks for the issue's Verification document (`role` `verification`) in the same request, each document at its own current settled version, and the answer records the human's review on each;
  an issue with no Verification document is `409 NO_VERIFICATION_DOCUMENT`, and one whose Verification document has no settled version is `409 NO_VERSION`.
  The flag is for the issue's spec alone: on any other document it is `409 NOT_SPEC_DOCUMENT`.
  A closed issue's document is `409 ISSUE_CLOSED`, before any other 409; an issue's plan is `409 PLAN_APPROVED_BY_REVIEW`, as its plan review approves it, and a project `plan.md` is no plan.
  The question is `Approve <name> (version <N>)?`, or `Approve <name> (version <N>) and <name> (version <N>)?` for a request naming two documents, followed by the optional `summary` (blank is `400 SUMMARY_INPUT`;
  past the ask cap at the longest version a request can reach, ten digits, is `400 CAP_EXCEEDED` naming `summary`).
  A request on a document a human approved earlier names that approval in `ask.approval.approved` (`{version, at, by}`);
  a first request carries no `approved` key.
  One open approval row follows every document version in place, rewording its question and emitting `ask.edited`;
  while its `requested_version` is below the new version it waits on the agent.
  A row waiting on the human keeps its turn, quietly, at a settlement's version with the bytes of the named one;
  a version a route writes (a named version, an upload, an edit) moves it to the agent whatever its bytes.
  Only the first move since the request was opened or handed back notifies, even when a thread reply had already left it waiting on the agent;
  a later move, while `requested_version` is already below the version it named, is `quiet: true`, `notify: false`, and reaches no follower.
  A version a human's answer to a decision block writes, when that answer is the whole change since the version he approved (who answered what is the document's asks rows, and an ask block no row of the document backs, a copy, one settlement could not read, one whose server-owned attributes are not as settlement writes them from its row, or a block id the version repeats carries nothing), carries his approval to it in the same transaction - an `approved` review in his name whose `reason` names the carry, and `artifact.approved` - and answers the open request `Approve` in his name after the move (`ask.answered`, the review's `ask_id`), so nothing is requested again for it.
  A second call does one of two things, by what the row waits on:

  - Calling this route again while the row waits on the agent - moved, or a thread reply newer than its last hand-back holds the turn - hands it back to the human:
    a summary that changes its question rewords it first (`ask.edited`;
    an omitted summary keeps its prior one), then `requested_version` becomes the latest version and `ask.handed_back` is emitted, leaving `edited_at` as it was.

  - While the row waits on the human, the same summary or none is a repeat that answers `200` with no event, and a different one is `409 APPROVAL_WAITS_ON_HUMAN` and changes nothing, since it would rewrite the card the human is reading.

  It answers `201` when it wrote anything, with the document's `approval` as the call left it (`waiting_on` while awaiting), and `opened`, true only where this request opened the ask it answers, false where it renewed one already open or the documents stood approved.
  Every answer carries `coverage`, one line per requirement of the issue's spec with no check, or the single line `the spec marks no requirements` for a spec that marks none (`GET /api/v1/issues/{key}/coverage`), and no line at all for a covered spec;
  it is empty for a document that is not a spec.

  It never names a missing plan task, since the plan comes after the approval;
  a spec with gaps still opens the request, and a coverage read that failed answers `coverage_unread: true` with no lines.

- `/api/v1/artifacts/{id}/plan-reviews` (POST; cookie, trusted header, or bearer)

  Record a plan review's approval of a plan version: `{version}`, the version the review read.
  It writes an `approved` review of kind `plan_review` and emits `artifact.approved`, so every read of the document shows it approved and a later version makes it stale, as a spec's approval goes stale.
  It is `409 NOT_PLAN_DOCUMENT` on any document that is not the issue's plan, the issue's spec and a project document named `plan.md` included
  — an issue document whose `role` is `plan`, or an issue document a rolling deploy left with no role whose slug is `plan-md`, is the plan
  — `409 NO_VERSION` on a plan with no settled version, and `400 INVALID_REVIEW` for a version the plan does not have.
  No person is ever asked to approve a plan, which is why `/api/v1/artifacts/{id}/approval-requests` refuses one.
  A review Dispatch carries to a later version keeps the `kind` of the review it carries, so a plan version restored to the text its plan review approved is still `plan_review` and no carry ever records a plan's approval as a person's.

- `/api/v1/artifacts/{id}/asks?state=` (GET, POST; cookie, trusted header, or bearer)

  List or create asks on an unlinked document.

- `/api/v1/artifacts/{id}/comments` (GET, POST; cookie, trusted header, or bearer)

  List or create comments and suggestions on an unlinked document.

- `/api/v1/artifacts/{id}/events` (GET; cookie, trusted header, or bearer)

  Read an unlinked document's events.

- `/api/v1/artifacts/{id}/references` (GET; cookie, trusted header, or bearer)

  Read an artifact's outgoing and incoming reference edges.
  A link a block of the artifact holds is outgoing from the artifact, a block target listed with its document as `artifact` while a current version holds the block;
  every item and document citing the artifact or one of its current blocks is listed once, a document as itself.

- `/api/v1/issues/{key}/artifacts/{slug}` (GET; cookie, trusted header, or bearer)

  Read an issue artifact and its incoming references.
  `{slug}` is resolved within `{key}`.

- `/api/v1/issues/{key}/artifacts/{slug}/text` (GET; cookie, trusted header, or bearer)

  Read an issue artifact's live document markdown.

- `/api/v1/issues/{key}/artifacts/{slug}/versions/{n}` (GET; cookie, trusted header, or bearer)

  Read an issue artifact version or download it, as `/api/v1/artifacts/{id}/versions/{n}` does, `?download=true` and `?blocks=true` included.

- `/api/v1/issues/{key}/artifacts/{slug}/versions` (POST; cookie, trusted header, or bearer)

  Create a named issue-document version.
  On an issue Legion runs, a named version of its spec or Verification document from a writer that is no person and no architect of the issue or of an issue above it is `403 DOCUMENT_WRITER_NOT_ARCHITECT` (Document errors below).

- `/api/v1/issues/{key}/artifacts/{slug}/edits` (POST; cookie, trusted header, or bearer)

  Apply issue-document edit operations.
  On an issue Legion runs, an edit of its spec or Verification document from a writer that is no person and no architect of the issue or of an issue above it is `403 DOCUMENT_WRITER_NOT_ARCHITECT` (Document errors below).
  A change a concurrent browser deletion removes before the version is rendered is `409 EDIT_LOST_TO_CONCURRENT_CHANGE` and writes nothing;
  one removed after it answers `200` with `lost_ops`.

- `/api/v1/projects/{key}/artifacts/{slug}` (GET; cookie, trusted header, or bearer)

  Read an unlinked project artifact and its incoming references.
  `{slug}` is resolved within `{key}`.

- `/api/v1/projects/{key}/artifacts/{slug}/summary` (GET; cookie, trusted header, or bearer)

  What a reference to a project document shows, read without its versions:
  `{id, project, slug, name, kind, latest_version}`, `{slug}` (a slug or unambiguous filename) resolved as the project document read resolves it (`404 ARTIFACT_NOT_FOUND`, `400 ARTIFACT_REFERENCE_AMBIGUOUS`).

- `/api/v1/projects/{key}/artifacts/{slug}/text` (GET; cookie, trusted header, or bearer)

  Read an unlinked project document's live markdown.

- `/api/v1/projects/{key}/artifacts/{slug}/versions/{n}` (GET; cookie, trusted header, or bearer)

  Read an unlinked project document version or download it, as `/api/v1/artifacts/{id}/versions/{n}` does, `?download=true` and `?blocks=true` included.

- `/api/v1/projects/{key}/artifacts/{slug}/versions` (POST; cookie, trusted header, or bearer)

  Create a named project-document version.

- `/api/v1/projects/{key}/artifacts/{slug}/edits` (POST; cookie, trusted header, or bearer)

  Apply project-document edit operations.
  A change a concurrent browser deletion removes before the version is rendered is `409 EDIT_LOST_TO_CONCURRENT_CHANGE` and writes nothing;
  one removed after it answers `200` with `lost_ops`.

- `/api/v1/agents/{session_id}/artifacts` (POST; cookie, trusted header, or bearer)

  Upload a file or image to an agent's conversation:
  a multipart `file` (+ `name`, `summary?`, `actor?`) with an issue upload's caps, errors and file-store write, its kind read from its bytes as an issue upload's is (an `image` only for a PNG, JPEG, GIF or WebP, stored under the type its bytes are;
  any other file a `file` under its declared type), stored with `session_id` set, `issue_key` null, `project` `""` and `ref_key` `agent/<session_id>/<slug>`, addressed `dispatch://agent/<session_id>/artifact/<slug>`;
  it appends no event, since every event belongs to an issue, a document or a project.
  There is no list route: a conversation's pictures are reached through the references its messages carry, never enumerated.
  A caller whose credential proves a session of its own
  — an access token the secrets broker issued, the one that does
  — uploads only into that session's conversation;
  any other `session_id` is `403 SESSION_SCOPE`, naming the session the credential acts for and the session addressed.
  A person's cookie proves no session and uploads into the conversation it names.
  An agent's conversation holds files and images only, so a markdown document (JSON `content` or a `text/markdown` file) is `400 ARTIFACT_INPUT` ("an agent's conversation holds files and images;
  a document belongs to an issue or a project").
  A `session_id` that is empty or holds `/`, `?`, `#`, whitespace, a control character or a character that ends a reference in text (`<>"'` and a backtick, `[`, `]`, `|`) is `400 INVALID_SESSION_ID` on all three agent routes.
  Such a file takes no comment, ask, event read or subscriber through `/api/v1/artifacts/{id}/...`, which is `400 ARTIFACT_AGENT_OWNED`.

- `/api/v1/agents/{session_id}/artifacts/{slug}` (GET; cookie, trusted header, or bearer)

  Read an agent's conversation's file or image and its versions by slug.

- `/api/v1/agents/{session_id}/artifacts/{slug}/versions/{n}` (GET; cookie, trusted header, or bearer)

  Download one version of an agent's conversation's file or image, as `/api/v1/artifacts/{id}/versions/{n}` does:
  its bytes under the file's name, `-v<number>` included for an older version, with `Cache-Control: private, max-age=31536000, immutable`, or `Cache-Control: private, no-cache` with `?download=true`.
  A conversation holds no document, so no version here answers Markdown.

- `/...` (GET; none)

  Serve the dashboard static files.

An event's payload says what its write moved, so a reader can refresh only what it shows (the dashboard's rules are `packages/dispatch/web/src/api/event-refresh.ts`).
`issue.created`, `issue.updated` and `issue.closed` carry `changed`, the fields whose value the write moved (`title`, `status`, `closed_at` for a close or a reopen, `rank`, `priority`, `assignee`, `parent`, `labels`, `route`, `repo`, `components`, `blocked_by`, `external_links`;
on `issue.created` every field the new issue holds a value in), and `ancestors`, the issue's parent chain, parent first up to the root (`[]` for a root).
`thread_logins` is every login whose Inbox the write can have moved: which events carry it, and whom it names on each, is the `ThreadLogins` field of each event payload in `internal/dispatch/model/model.go`.
On an issue event it is the sorted union of two sets, naming a login in both of them once:
for a close or a reopen the logins with a row on the issue's open asks, which the Inbox leaves out of their Replies while the issue is open and the ask waits on them, and `[]` for any other status move;
for an assignee move the assignee the issue was taken from and the one it was given to, whose rows that same rule moves for every ask nobody has handed on, a side where the issue holds nobody being the project's default assignee, whom a question on such an issue reaches (`api/issue_patch.go`, over `movedLogins` in `api/thread_logins.go`).
An `ask.asked_of` names two people instead of a thread's readers: the person the ask waited on and the person it waits on now, since a hand-on moves the ask between their lists and changes nothing for a reader of its thread.
`user_state.updated` carries `changed`, which of `pinned`, `last_read_seq` and `dismissed` the write moved, or `["snoozed"]` for a snooze or an un-snooze of one Inbox row (`PUT` or `DELETE /api/v1/me/asks/{id}/snooze`, which append it only when the row moved).
A snooze's payload is exactly `{login, changed: ["snoozed"]}`:
every signed-in caller's event feed on the live socket replays it, so it names neither the ask nor the writer's issue state, and the writer's other tabs and devices refetch their whole Inbox.
`user_thread_state.updated` says that one viewer's read of the Inbox's threads moved, and carries `thread_id` only when one thread was marked (`PUT /api/v1/me/threads/{id}/read`):
a mark-all (`POST /api/v1/me/inbox/read`) and a dismissal (`POST /api/v1/me/inbox/dismiss`) each name none, since one event per project stands for every thread they changed there.
`priority.updated` is each priority write's (`POST /api/v1/priorities`, `PATCH` and `DELETE /api/v1/priorities/{id}`), and carries exactly `{id, change}`, `change` being `created`, `updated` or `deleted`:
a priority belongs to no project, so the event names no owner (`issue_key` null, `project` empty, `seq` 0), wakes nobody, and is published on no NATS subject; a reader refetches its priorities on every write, and on a delete also the work under that priority, the work under none and every issue read or listing whose rows advance it.
`going_plan.updated` carries exactly `{login}`: what that person's command-bar plans wait on changed - a run's plan began to wait for them, or a waiting plan was answered or stopped.
A run belongs to no project, so the event names no owner (`issue_key` null, `project` empty, `seq` 0), wakes nobody, and is published on no NATS subject; the person's pages read their Inbox's plans and History again.
`references_changed` names the targets whose inbound edges the write moved that a reader refreshes by name: an issue (`{kind:
"issue", id}`), an ask (`{kind: "ask", id, issue_key?, artifact_id?}`, with the issue or document whose ask list holds its row) and a document or file (`{kind: "artifact", id, ref}`:
its uuid, the node id `GET /api/v1/references` reports for it, and the `dispatch://` address its Referenced by panel is opened on, `ref` absent when its owner names no address), and a block of a document (`{kind: "block", id, ref, artifact_id}`:
its node id `<document ref_key>#<block id>`, its reference and its document's uuid, whose panel lists the edges at its blocks).
Each kind is capped on its own at 100 targets:
past the cap of a kind the event names none of that kind, lists it in `references_changed_truncated_kinds` (of `issue`, `ask`, `artifact` and `block`), still names the targets of every other kind, and carries `references_changed_truncated: true`, which a reader that does not read the kinds takes as every kind cut.
It also names a target whose edge the write kept but whose excerpt it moved:
an edit of an ask's question or a comment's body that changes the head of its text names every target the text still cites, a document version names each target whose stored excerpt it changed and each target a block it changed still cites, and a retitle names the issue's parent and the issues it is blocked by, whose panels show its title.
A document version also names each block it changed or deleted that something cites, since every link to the block shows its text, and a comment's or issue message's write names each target whose link it moved from one of the item's blocks to another.
An issue write that adds or removes a `blocked_by` entry names each blocker it added or removed, whose `referenced_by_count` counts the edge, and an `issue.created` names every blocker the new issue holds.
A changed target that is an issue's own content (a document of the issue or a block of it, an ask of it or a part of it, a comment, a message, or a block of one) also names that issue, as a target of kind `issue`, once and after the other targets, unless the write names that issue already; a comment, a message and their blocks still name no target of their own.
Those issues count toward the cap of `issue` (`refs.CountedChangeLimit`), so a page holding that issue's references read (`GET /api/v1/issues/{key}/references`, whose `linked_from` lists what links into the issue's content) refreshes it on that target.
Each field is absent from an event written before it existed.

## Dispatch topics

Document events publish retained envelopes on `notifications.dispatch.document.<PROJECT>.<slug>.<type>`.
Use `go run ./cmd/natstail -subject 'notifications.dispatch.document.>' -count 1` to print one matching envelope from the `envoy.natsUrls` configured in `envoy.json`.
`natstail` owns nothing on the bus - it neither creates nor updates `ENVOY_NOTIFICATIONS` - and refuses a NATS server that is not this machine's:
prefix the command with `ENVOY_ALLOW_REMOTE_NATS=1` where `envoy.json` names a shared server, as an agent devbox's does.
It connects as the nkey user `NATS_NKEY_SEED_FILE` (or `NATS_NKEY_SEED`) names, never `envoy.natsSeed`, so a run against a NATS that requires a user sets `NATS_NKEY_SEED_FILE` too.

## Document edit operations

`POST /api/v1/artifacts/{id}/edits` accepts an `ops` array.
Quote targets resolve against the document's plain text rather than Markdown source:
inline-code and link text match without their syntax, and a table-cell target is its cell text.
A quote holding a table with a row shorter than its delimiter row, the header included, is matched as the text it is.

Each item below names an operation and its required fields, then its behavior.

- `replace` (`find`, `with`)

  Replace one plain-text target.
  Every `replace` names `with`, and `with: ""` deletes the target on purpose: a `replace` takes `op`, `find`, `with` and `occurrence` alone, and one with no `with`, or carrying any other member whatever it holds (`markdown`, `markdown: ""`, `text`, `replacement`), is `400 REPLACE_FIELDS` naming the operation and the member, with nothing written, where it was read as an empty `with` and deleted the target, or applied beside a member it never read.
  The route reads the request body a second time for each operation's member names (`api.replaceFieldsRefusal`), since a decoded operation's `with` is empty both for no `with` and for `with: ""`, and a member the operation type does not declare decodes to nothing.
  `dispatch doc-edit` refuses both shapes before it sends anything, exit 2 naming `ops[<index>]` and the field.

- `delete` (`find`)

  Delete one plain-text target.

- `insert` (`markdown`, exactly one of `after` or `before`)

  Insert a sibling block before or after a quote or heading's enclosing document block.
  `"start"` and `"end"` select document edges.
  Pipe-table row fragments at a table-cell target are the exception: they extend that table before or after the containing row.
  Use `replace` for inline continuation.

Table-row fragments contain body rows only: omit the table header and delimiter row.
Short rows are padded to the table width while all operations in the edit request add at most 10,000 cells;
a larger request is rejected as `INVALID_OP` on `markdown`.
A row holding text in a cell past the table's width is rejected as `TABLE_WIDTH`; blank cells there are dropped.

## Element weights

One write's markdown, and the markdown a document stores, make at most 65,536 elements (`pmdoc.MaxDocumentElements`), weighed node by node over the tree goldmark parses, front matter apart.
Every route that takes caller markdown refuses past that with `413 CAP_EXCEEDED`, and its message lists these weights, written once in `pmdoc/elements.go` (`weighed`):

| Node | Weight |
| --- | --- |
| a block (paragraph, heading, quote, list, list item, code block, rule, table, table row, footnote definition, typed block) | 3 |
| the empty block an empty container is read as holding: the paragraph of a list item, quote, footnote definition or typed block holding nothing (or of a list item opening with another block), and the row of a table with no body row | 3 more |
| a table cell | 4 |
| a hard line break, beside its line of text | 3 |
| an image | 3 |
| a piece of inline HTML, a tag or a comment | 2 |
| an autolink | 2 |
| a footnote reference | 2 |
| an escape: a backslash before punctuation, or a character reference | 1 |
| a piece of inline syntax, a mark, a line of text | 1 |

Every node the Proof tree makes other than a text is an element of the live document carrying attributes, and costs the server about what two inline nodes do;
each node class that weighed less let the heaviest document of it the limit admitted hold past the 256 MiB one request may, until it was weighed as above.
`TestEveryNodeOfTheSchemaWeighsWhatItMakes` holds every node type of the schema to at least two elements for each element its cheapest markdown makes and one for each text, and fails for a node type the schema gains until it is given that markdown and a weight.
What each node type weighs:

| Proof node | Made of | Weight | Attributes |
| --- | --- | --- | --- |
| `doc` | the document | 0, one a document | none |
| `frontmatter` | front matter | 0, one a document, apart from the count | none |
| `paragraph` | a paragraph, or a tight list item's text | 3, or 3 as an empty container's | block id |
| `heading` | a heading | 3 | block id, level, id |
| `blockquote` | a quote | 3, and 3 when empty | block id |
| `bullet_list`, `ordered_list` | a list | 3 | block id, spread, order |
| `list_item` | a list item; a task item's marker is 1 more | 3, and 3 when empty or opening with another block | block id, label, list type, checked, spread |
| `code_block` | fenced or indented code | 3 | block id, language |
| `hr` | a thematic break | 3 | block id |
| `table` | a table | 3, and 3 with no body row | block id |
| `table_header_row`, `table_row` | a row | 3 | block id |
| `table_header`, `table_cell` | a cell and its paragraph | 4 | block id, alignment, spans |
| `footnote_definition` | a footnote definition | 3, and 3 when empty | block id, label |
| `callout`, `ask`, `requirement`, `plan_task`, `check` (typed blocks) | a `:::name{…}` block | 3, and 3 when empty | block id and the schema's attributes |
| `text` | a run of text; each mark a run carries is the inline syntax that opens it | 1 | none (marks) |
| `hardbreak` | a hard line break | 3, beside its line's 1 | isInline |
| `image` | an image; its alt text's nodes weigh 1 each | 3 | source, alt, title |
| `html` | a piece of inline HTML | 2 | the HTML |
| `footnote_reference` | a footnote reference | 2 | label |
## Input errors

Each item below names a status and code, then its meaning.

- `400 NUL_CHARACTER`

  Caller text holds a NUL character (U+0000), which PostgreSQL's text and jsonb cannot store:
  any string in a JSON body a route decodes, member names included, a multipart upload's field or markdown file, or any route's path or query parameter.
  It covers every parameter, field and member a request carries, including ones the route does not read.
  The message names where it stands (`title`, `options[1].label`, `file`, `path parameter session_id`, `query parameter label`), with a NUL in a name the caller wrote shown as `\u0000`, and the character's position, counted from 1 in UTF-16 units; nothing is written.
  A NUL a browser edit puts in a live document is written as U+FFFD, which is what CommonMark reads one as, in the document's version and every read, a block's id included, an anchor's quote and an ask block's question and options, and a quote, a `# Title` quote and a `heading:` anchor are matched against that text.

- `400 INVALID_UTF8`

  Caller text holds a byte that is not UTF-8, which PostgreSQL's text cannot store:
  a multipart upload's field, its markdown file or a binary file part's `Content-Type` (`file Content-Type`), or any route's path or query parameter (`%FF`).
  JSON cannot carry one, since decoding writes it as U+FFFD.
  The message names the field, the byte and its position, as `NUL_CHARACTER` does; nothing is written.

- `400 ARTIFACT_INPUT`

  An artifact upload's multipart body the parser cannot read, such as a part header holding a control character, named with the parser's reason.
  A body past the upload's size limit stays `413 CAP_EXCEEDED`, and a file part the server cannot spool to its temporary directory is `500 INTERNAL`, logged.
  A markdown document uploaded to an agent's conversation is refused with this code too:
  it holds files and images, and a document belongs to an issue or a project.

- `400 INVALID_SESSION_ID`

  An agent artifact route's `session_id` is empty or holds `/`, `?`, `#`, whitespace, a control character or a character that ends a reference in text, so no reference could name what it owns; nothing is written.

- `409 ID_AMBIGUOUS`

  The id in a route's path is written short
  — 8 or more characters of the hex digits and hyphens an id starts with, read without case — and two items or more carry it.
  The body carries `candidates`, each with its `kind`, its whole `id`, its `ref` (the whole `dispatch://` reference), the issue key or project document that owns it, and its `first_line`, which is left out for a direct conversation the caller is not in.
  Write more of the id.
  An id written short that names one item is resolved before the route's handler runs, so the handler, the project-scope check and the response all read the whole id;
  one that names none answers the 404 that route answers for a whole id that names none, and a value that is no short id at all
  — fewer than 8 characters, or not hex — gets the answer it has always had.
  The ids this covers are an ask's, a comment's, a message's, a document's and a document block's;
  an id in a request body (`reply_to`, `ask_id`, `in_reply_to`) and the `artifact=` filters are read whole, since the programs that write them hold the whole id.

## Document errors

Each item below names a status and code, then its meaning.

- `404 TARGET_NOT_FOUND`

  The quote requested by an anchor or document edit is absent, or the block id a block read (`GET /api/v1/artifacts/{id}/blocks/{block_id}`) names is not in the live document.

- `409 TARGET_AMBIGUOUS`

  A quote matches more than once without an `occurrence`; the response includes candidate ranges and context.

- `400 TARGET_SPANS_BLOCKS`

  A document edit quote crosses textblock boundaries.

- `400 REPLACE_FIELDS`

  A `replace` names no `with` (a JSON `null` is none), or carries a member besides `op`, `find`, `with` and `occurrence`, whatever that member holds (`markdown`, `markdown: ""`, `text`, `replacement`); nothing in the batch is written.
  Such a replace was read as an empty `with`, which deleted the text its `find` matched, or applied beside a member it never read; `with: ""` still deletes the match on purpose.
  The message names the operation and each such member: `operation 1: replace names no with, which would delete the text find matches, and carries markdown, which replace does not take; …`.

- `400 TABLE_WIDTH`

  A table-row fragment holds text in a cell past its target table's width; blank cells there are dropped.

- `409 ANCHOR_MISSING`

  A browser submitted a `mark_id` that the server did not observe in the live tree, in the second it waits for the document's next update and in one last read of the document when that wait ends.
  A mark no ask or comment row records is swept from the document a minute after a settlement first saw it.
  A mark whose `by` names a user with a browser on the room is kept instead, so a composer left open sends, and a mark a closed or offline tab left behind goes a minute after that user's last browser left the room.

- `409 ANCHOR_ORPHANED`

  An operation needs a mark whose anchored text has been deleted.

- `400 INVALID_ANCHOR`

  An anchor must provide exactly one of a nonempty `quote` or nonempty `mark_id`, with its document artifact.

- `400 INVALID_MARKDOWN`

  Uploaded document content cannot be represented by the Proof schema, such as a table row holding text in a cell past its delimiter row's width, which a pipe inside code or a link that is not backslash-escaped makes.
  A Markdown document nests at most 100 blocks (quotes, lists and their items, typed blocks, and footnote definitions), of them at most 32 typed blocks inside one another whatever other blocks stand between them, and one textblock's inline markdown at most 100 marks (emphasis, strong, strikethrough, links, images, and code);
  deeper content is refused naming the line.
  An accepted suggestion whose blocks nest within those bounds but land deep enough that the document would nest past one is `INVALID_OP` on `replace_with`, naming how many blocks, or typed blocks, the result nests.
  Malformed edit replacements report `INVALID_OP`.
  A typed block's `block[]` value that is not canonical block references joined by exactly one space is refused naming the attribute and the value, as any attribute value the schema refuses is.
  A write introducing a `block[]` pointer to an existing block whose type the attribute's `targets` do not allow is refused naming the typed block, the attribute, the reference, the target's type and the allowed types, on every route but `POST /edits`, which answers it `INVALID_OP` on `attributes`;
  a browser's edit is never refused for a pointer.
  An upload that moves an open anchor's quote where that anchor cannot stand - an ask's quote into a code block, or any open ask's, comment's or suggestion's quote into the front matter - and leaves no copy where it can is refused naming the anchor, its quote and where the text landed:
  move the text out, or resolve the anchor first.
  A copy there beside the original is taken, and the anchor stays on the original.

- `400 INVALID_CI_CHECK`

  A document write leaves a check block carrying `ci_check` that breaks one of the four rules of a check CI proves (`docs.CICheckRuleIn`), and the message names the block, its `ci_check` and the rule: `check <id> (ci_check "<name>"): <rule>`.
  The rules, in the order they are read: `a check proved by CI carries no run`; `ci_check takes when of before_merge`; `a check proved by CI names its negative control`;
  and, read in the write's transaction, `this check already holds a recorded result, so it does not become CI's; write a new check block under a new id and delete this one`.
  Every route that writes a document refuses it - an issue's create, an upload of a document or a version, `POST /edits` and its attribute operations (never wrapped into `INVALID_OP`), and a rebuild from supplied markdown - for each check block the write adds and each one whose `ci_check`, `run`, `when` or `negative_control` it changes, compared by block id with the document before the write;
  an edit elsewhere in a document never meets another writer's block.
  A blank `ci_check` names no run.
  A browser's edit is never refused: settlement removes such a block's `ci_check` and posts one `block.invalid` naming the block, the rule as `reason` and `block_type: "check"`.

- `403 DOCUMENT_WRITER_NOT_ARCHITECT`

  On an issue Legion runs, a version of the issue's spec (its primary document) or of its Verification document is written only by a person, by an architect of the issue, or by an architect of an issue above it (`internal/dispatch/api/spec_writer.go`).
  A person is a request whose credential holds the `person` scope, a person at a browser among them, or Dispatch's own system writer.
  An architect is the session of an `architect` row of the role holders on the issue's parent chain (`PUT /api/v1/issues/{key}/role-holders`).
  The chain climbs only to a parent of the issue's own project, so it ends at the first ancestor of another project, as at a root.
  Legion runs an issue when it or an issue of its own project above it carries the label `legion`, in any case.
  Each route that writes such a version refuses every other writer with this code, before it stores anything or appends an event, and the server logs one line for each refusal:
  an upload, an edit or a named version of the document, a file uploaded to the issue under the name `verification.md` among them;
  the reword, the retraction or the resolution of a block ask in it;
  a create under a parent, which writes the new issue's spec;
  and a close and a reopen, which write the asks in the spec, a move under a parent, which changes who can write it, and a `labels` or `parent` write that takes the issue out of Legion, after which any session can write it (`PATCH /api/v1/issues/{key}`, `POST /api/v1/issues/{key}/status-change`).
  A create and a move are judged on the issue as the write leaves it, with the architects of the new parent's chain.
  Under a parent of another project, the issue is judged as a root, which the check does not judge.
  Every move under a parent is also judged, wherever the new parent is, as it stands, on the chain of each issue of the moved subtree that Legion runs before the move: the issue's own chain when Legion runs it, and else the chain of each issue under it that carries the label `legion` and that it reaches through parents of its own project.
  So when the moved subtree holds such an issue, only a person, or an architect at or above each of those issues that is also at or above the new parent when the new parent's check judges the move, can make it, and the refusal names the first issue whose chain refuses the writer.
  An architect that took the label off its own tree, or moved its own issue out to the root, cannot then move another tree's issue under it to get the right to write it.
  The session of the project's Legion daemon, `legion-daemon:<project>`, passes a close, a reopen and a write that takes an issue out of Legion too, but it passes neither check of a move.
  While the project's `proven_sessions` is on (`/api/v1/projects/{key}/proven-sessions`, Routes above), an architect's session and the daemon's pass only on a credential that proves that session, a broker token of it.
  The message names the session, the write and the issue, who can make the write, the role the session holds on the issue and each architect session Dispatch records on the chain, and it ends:

  - The refusal of a move under a parent, and of a write that takes the issue out of Legion (an unlink by `unlink_children` among them), ends ``A suggestion or a comment cannot carry this write: ask one of the writers this message names, a person by `dispatch ask`.``

  - The refusal of a document write, a create, a close or a reopen ends `Propose the change with a suggestion or a comment.`

  A refusal for proof names the project instead, says that the request's credential does not prove the session, and ends with neither sentence.
  The check holds the issues of a project only once Dispatch has taken a role-holder publication for an issue of that project, which marks it (`project_role_holder_marks`), so nothing changes for a project until its daemon publishes its first role holders.
  A create under no parent is not checked, nor is an anchored comment, ask or suggestion: such a write adds only its writer's mark around words others wrote, and a comment or a suggestion is how a worker proposes a change to the architect.

- `409 DOC_SCHEMA`

  A stored tree is outside the Proof schema, such as one holding a node more than 1,000 levels below the document, a node attribute value nesting more than 100 arrays and objects, or a mark attribute value nesting more than 99;
  settlement writes no version of such a tree.
  Every route that starts from that tree answers it alike, with one message - reading its text or blocks (`/text`, `/blocks`), editing it (`POST /edits`) and naming a version (`POST /versions`) - and a schema refusal of a tree the request itself produced is a `500`.
  The message names the repair, a replacement from markdown, says that it discards every part of the document's current content that the markdown leaves out, and says that a person decides whether to replace the document.
  The replacement is an upload of markdown under the same document name;
  an upload preserves every open ask block, and is refused `400 INVALID_ASK_BLOCK` for an ask block that breaks the ask content rule, as a new document is.
  The document websocket admits no connection to such a room:
  it completes the upgrade and closes with code `4409` (reason `DOC_SCHEMA`, `DOCUMENT_SCHEMA_CLOSE_CODE` and `DOCUMENT_SCHEMA_CLOSE_REASON` in `packages/contracts`, generated into Go) before any sync, so a browser editor never receives a tree it would normalize and write back.

- `409 DOC_NEWER_CONTENT`

  A stored tree holds an element this Dispatch process's build does not know:
  a node, mark or typed block its schema manifest does not name, an attribute a typed block does not declare, or a value an enum attribute does not list.
  Two builds serve one database during a release, so a newer build can have written it;
  a tree that also holds a known element in a broken shape is this code, not `DOC_SCHEMA`.
  Every route that starts from that tree answers it alike, with one message that names the element, says to try again in one minute, and says that a replacement deletes the element.
  No route repairs it: an upload or replacement over it is refused with this code and leaves the document unchanged, and settlement writes no version of it.
  The document websocket admits no connection to such a room:
  it completes the upgrade and closes with code `4425` (reason `DOC_NEWER_CONTENT`, `DOCUMENT_NEWER_CONTENT_CLOSE_CODE` and `DOCUMENT_NEWER_CONTENT_CLOSE_REASON` in `packages/contracts`, generated into Go) before any sync.
  The Hocuspocus provider reconnects on that code with its backoff, so a later connection can reach a build that reads the document.

- `409 DOCUMENT_UNLOADABLE`

  A document's stored history does not decode, so nothing can read or open it.
  A rebuild from its latest saved version (`POST /api/v1/artifacts/{id}/rebuild`) discards every edit made after that version, and a person decides whether to do it;
  the dashboard offers that rebuild for this code alone.
  A room or store that could not serve the document stays `503 DOC_SERVICE_UNAVAILABLE`, which is retried, not rebuilt.

- `409 DOCUMENT_LIVE`

  A rebuild found the document resident in this server or in another Dispatch process on the database (it asks every member, and waits 500 ms at most for the answers):
  open in an editor, or left by its last editor less than 90 seconds ago (a room stays resident a minute after its last peer leaves, and the sweep that evicts it runs every 30 seconds).
  Nothing was deleted.
  The message says whether this process or another one holds the document, names the latest saved version N a rebuild from it would discard every later edit back to, and tells the caller to stop and ask a person whether to rebuild the document after two minutes;
  it offers no other repair.
  A rebuild also answers this code when the document's head moved while it ran, because of another rebuild or an edit;
  that message names the same saved version and says that another rebuild may already have repaired the document, so the caller reads it again and asks a person only if it still does not load.
  One `DOCUMENT_LIVE` names no version and no discard: `document <id> is already being rebuilt`, while a rebuild of it runs in this process, which says only that and offers no repair.
  One builder makes every other one (`liveRebuildRefusal`, `docs/rebuild.go`).

- `409 DOCUMENT_LOADS`

  A rebuild found that the document's durable history loads.
  Nothing was deleted.
  The message names the latest saved version, says that Dispatch rebuilds only a document whose stored history cannot load, and tells the caller to stop and ask a person what to do with the document;
  it offers no repair.

## Comment errors

Each item below names a status and code, then its meaning.

- `400 INVALID_COMMENT`

  A reply must target a thread-root comment.

- `403 NOT_AUTHOR`

  Only a comment's author may edit it.

## Tests

```sh
cd packages/envoy
export DISPATCH_TEST_DATABASE_URL='postgres://postgres:dispatch@127.0.0.1:55432/dispatch?sslmode=disable'
go test -timeout 20m ./internal/dispatch/api
go test -timeout 20m $(go list ./internal/dispatch/... ./cmd/dispatch/... | grep -v '/internal/dispatch/api$')
go vet ./...
```

`internal/dispatch/api` alone takes close to `go test`'s default ten-minute package deadline (about 600 s), so it runs first and on its own, with 20 minutes, as CI's `envoy-go` job runs it (`docs/testing.md`, "Go's package deadline").

`./scripts/dev-postgres.sh` prints the database URL to set (Local Postgres, above).
One test: `go test -run TestName ./internal/dispatch/api`, which needs no deadline of its own.

Three suites build only under a tag, and the commands above run none of them.
A claim about two processes, or two builds, on one database needs evidence from them.
Each needs `DISPATCH_TEST_DATABASE_URL`.

```sh
go test -tags memory -count=1 -timeout 60m ./cmd/dispatch        # resident memory of real servers
go test -tags multiprocess -count=1 -timeout 30m ./cmd/dispatch  # two servers, one database
bash scripts/build-dispatch-build.sh --previous-release --out "$TMPDIR/dispatch-previous"
bash scripts/build-dispatch-build.sh --commit edb69e0d7588ec037847182e74a04a343a0664c9 --out "$TMPDIR/dispatch-first-rollout"
bash scripts/build-dispatch-build.sh --commit 6c46f6bb7dd6a1ac9b6235fb3bce990ec82eae86 --out "$TMPDIR/dispatch-going-release"
DISPATCH_PREVIOUS_BUILD="$TMPDIR/dispatch-previous" DISPATCH_FIRST_ROLLOUT_BUILD="$TMPDIR/dispatch-first-rollout" \
  DISPATCH_GOING_RELEASE_BUILD="$TMPDIR/dispatch-going-release" \
  go test -tags twobuild -count=1 -timeout 30m ./cmd/dispatch   # this build beside earlier ones
```

"Two builds at once", above, says what the two-build test runs and why it needs the first-rollout build and the command bar's release.

`TestTheServerEndsTheAgentBeforeTheKernel` (`agent_memory_group_test.go`) skips, naming why, unless `DISPATCH_TEST_MEMORY_GROUP=1`, which its CI job sets on an amd64 runner with control groups v2 and Docker.
It runs the server, built for linux/amd64, in a Docker container whose memory limit is 8 GiB with swap off, on `--network host` against `DISPATCH_TEST_DATABASE_URL`; `DISPATCH_TEST_MEMORY_GROUP_IMAGE` names another image than its default, the copy of `debian:bookworm-slim` in `.github/ci-images.yaml`.
It measures the agent's fastest growth in 50 ms, holds the server's heap so that the container keeps twice that growth beside a server and an agent at its ceiling, and requires the server's kill of the agent before the kernel's: the container's `memory.events` counts no `oom_kill`, and the server keeps its process id.

`TestTheC28MethodFindsAPlantedLeak` (`memory_leaks_test.go`) runs the comparison of production's heap on a server that leaks on purpose, from two sets of `/debug/pprof/heap?gc=1` and `/debug/vars` read from its profiler (Profiling, above).
The comparison holds the heap outside the rooms (samples with a `reearth/ygo` frame) and the read cache (a `coldRead` frame) to 2 MB an hour, and the rooms' part divided by `resident_bytes` under twice its value at the first set.
With `DISPATCH_TEST_KEEP_CLOSED_ROOMS=1` the server keeps a copy of the document of each room a document socket leaves, and the rooms' ratio passes twice its first value;
with `DISPATCH_TEST_KEEP_REQUEST_MEGABYTES=1` it stores 1 MB for each request in a map nothing prunes, and the heap outside grows past its bound, with the hook's function among the five whose flat heap grew most.
Both hooks are refused without `DISPATCH_TEST_HOOKS=1` (`testHookBool` in `main.go`).
`TestTheLeaksOfC28AreFixed` holds a subtest for each leak that comparison named in production, and says it holds none while it named none.

`TestBackgroundRunsHoldNoMemory` (`agent_soak_test.go`, under the `multiprocess` tag with the other tests of the agent process, `agent_loop_test.go` and `agent_supervisor_test.go`, which CI's `dispatch-multiprocess` job runs) works 1,000 runs through a server's own agent after 100 more fill the caches - answers, searches, reads of a spec of 1 MiB, approved plans, follow-ups, runs their person stops, runs stopped at a bound of their level, runs that lose their lease and runs whose tool panics - and holds the agent's live heap, read from its collection at a `SIGUSR1`, to less than 1 KiB of growth for each run, and its goroutines to their count at the start; it then leaves 1,000 plans waiting and holds the agent to no goroutine more and both processes to less than 1 KiB for each, the server's live heap read from `/metrics` just after `/debug/pprof/heap?gc=1` made it collect.
`goleak` (`go.uber.org/goleak`) fails `going/agent`, `going/agentwire` and `internal/dispatch/api` at the end of their run when a test left a goroutine behind (`TestMain` in each).

Two tests of `agent_load_test.go` build under the `memory` tag.
`TestTheServerHeapUnderEightDeepRuns` seeds an Envoy listener of 200,000 sessions, a project of 5,000 issues, an issue of 1,000 comments and a spec of 1 MiB for each run, fills the read cache to its budget, and measures one Deep run at its limits:
the most the agent's total Go memory rises while it works is one run's peak, and the claim gate (70% of the agent's `GOMEMLIMIT`) less the agent's base, divided by that peak, 8 at the most, must be the agent's slots (`agentSlots` in `memlimits.go`).
Then 8 Deep runs at once fill every cache the runs fill, and 8 more read the issue listing, search, the roster, an issue's events and their spec, and edit it:
the server's live heap outside the document rooms (samples with a `reearth/ygo` or a room frame of `internal/dispatch/docs`) stays under 1 GiB above its level at rest before them and comes back to within 5% of it at rest after them, and the agent's total Go memory stays under its `GOMEMLIMIT`.
At rest is once the rooms have closed and the database owes the server no settlement, block index or re-index;
the test logs the three caches' bytes at each end and the five functions whose flat heap outside the rooms grew most between them.
`TestTheAgentLeavesTheServerItsCPU` holds the p99 of one fixed read under twice its p99 with the agent idle, every `/livez` under 1 s, and every thread of the agent under `SCHED_IDLE` (each thread's policy in its `/proc/<pid>/task/<tid>/stat`), while the agent works at its soft limit of 1 GiB through `test_soft_limit`, which allocates without pause beside a live heap it sizes after each collection so the memory the agent's runtime counts against its limit peaks at the limit, holding at most half the limit so each collection ends within a second (at about 80% held, a collection on the agent's one CPU outlasted a second).
Each window, idle and at the load alike, times the read at least 500 times and for at least a second (`cpuWindowLeast`), so it holds collections of the agent at its soft limit:
on a GKE node of 4 CPUs the hook refilled the heap in about 300 ms while 500 reads took 236 to 288 ms.
The p99 is over all the reads of the window, and the log says which of how many reads it is.
The idle window is one.
At the soft limit the test times up to 3 windows in turn (`cpuAtLimitWindows`) and judges the first in which the agent's threads ran at least 80% of its CPUs (`cpuAtLimitShare`), it used 90% to 110% of its limit at the window's start and end, and its collector completed a collection;
on a shared build node the agent at `SCHED_IDLE` once had 31 to 37% of its CPU in a second, and a mark the GC CPU limiter capped did not end in it, while the windows beside it were judged.
It logs every window, judged or not, with its p99 against the idle one, the agent's CPU share and runtime figures, and why it was or was not judged, and fails with all of them where none of the 3 is judged, since such a window says nothing of an agent at work at its soft limit.
The agent runs there with production's share of the runner's CPUs, a quarter of them and 1 at the least (`DISPATCH_TEST_AGENT_PROCS`), as production gives it 4 of a container's 16, since the 4 a server gives its agent are every CPU of a 4-CPU runner;
the test logs the CPUs it runs on, the kernel, the agent's `GOMAXPROCS` and its threads by policy, the p50, p90 and p99 of the read and of a round trip to the database, idle and at the load, what each window's 8 slowest reads met, and the CPU time the agent's threads had in the window at its soft limit, which shows whether a run beside a busy server would spend its time budget waiting for a CPU.
It skips on a kernel built with lazy preemption (`CONFIG_PREEMPT_LAZY=y`), such as the 7.0 kernel of GitHub's arm64 runners, where the read's p99 at the load was 2.0 to 2.6 times its idle p99 in five runs, the server's threads waiting 136 to 250 ms to run beside the agent at `SCHED_IDLE`, while on a GKE node of the 6.12 line production runs it was 1.00 and 1.06 times;
so check c29 runs it as a Job in the cluster's `builds` namespace on a `build-nosandbox` node of its own (no gVisor, 4 CPUs), `-tags memory -count=3 -run '^TestTheAgentLeavesTheServerItsCPU$' ./cmd/dispatch`, and its record names the node, the kernel and the agent's share of its CPU in each judged run.
The Job takes the node to itself with a `nodeSelector` and a matching `NoSchedule` toleration on a `group` label of its own, for which GKE Autopilot makes a tainted node no other workload shares.
On a shared node, other pods' CPU left every process of the test's pod waiting to run, the agent included, and the read's p99 at 2.19 and 2.18 times idle: on 2026-11-22 two other jobs used 5.8 to 6.3 cores of a 16-vCPU node, and the server's threads waited 84 and 259 ms to run in a second, the agent's 85 and 111 ms.
On a node of its own the p99 was 1.09 to 1.13 times idle, the server's threads waiting 4 to 31 ms.
`TestTheAgentRunsUnderSchedIdle` (`agent_environment_test.go`, untagged) holds the setter to every thread of an agent process:
`SCHED_IDLE`, or nice 10 where the kernel refuses it.
