# Dispatch

Dispatch is the React single-page application for the native Dispatch workspace. A
workspace issue holds its status, primary specification, open asks, comments and
suggestions, artifacts, and durable event history. The Go server in
`packages/envoy/cmd/dispatch` serves the production build from `web/dist`.

## Application shape

`AuthGate` requests `GET /auth/whoami` and sends unauthenticated visitors to the
Google sign-in flow. All application requests are same-origin. React Router serves
the Inbox at `/`, an issue workspace at `/issues/:key/*`, and the
repository-to-project settings page at `/settings`; TanStack Query and the live socket's events
feed keep the issue, Inbox, documents, and sidebar current.

The desktop shell has a sidebar, issue content, and review margin. Issue content has Spec,
Conversation, Children, Artifacts, and Activity tabs; the Conversation lists one card per
conversation and the Activity tab holds the issue's whole event history. The Spec document
renders and edits through `@legion/proof-editor`, with Yjs presence and margin-linked marks. The
margin holds Comments and Pinned. Each issue-owned anchored comment thread is one card with flat
replies, inline reply and author-edit controls, mark-aligned cross-hover, and resolved history
behind `Resolved (N)`. Project-document thread replies are handled by the margin-by-owner
surface. Below the `xl` breakpoint, navigation is a drawer and the margin is a bottom sheet;
opening a thread uses a full-height view with a bottom-pinned composer. Controls use 44 px
minimum touch targets.

## Development

Run commands from this package:

```bash
bun run dev
bun run build:web
bun run typecheck
bun run lint
bun run test
```

`bun run build:web` produces `web/dist` for the Dispatch server. `bun run dev`
runs the Vite development server for interface work.

## End-to-end tests

`bun run e2e` builds `@legion/contracts` and the SPA and drives Playwright
against the real Go Dispatch
server and Postgres. Playwright starts the harness itself: `e2e/global-setup.ts`,
its `globalSetup`, starts one stack per worker (`e2e/harness-pool.ts`:
the fakes, then `e2e/run-server.sh`, then the
plain-HTTP proxy) unless `PLAYWRIGHT_BASE_URL` selects a deployed server, and
`e2e/global-teardown.ts` stops them. It keeps the stacks' logs unless the run
showed that it passed. A run of one stack writes none, since that stack's
servers write to the run's own stderr.
Playwright runs under Node, which reads `@legion/contracts` from that package's
built `dist/`, and a fresh checkout has no `dist/`: `bun install` makes none.
The hook does not build it and cannot. Playwright loads
`e2e/playwright.config.ts` before it runs any hook, and that config imports the
package, so a run that has not built it is already dead when the hook is
reached. Whoever starts Playwright builds it instead: `bun run e2e` and the
package's other e2e scripts run `build:contracts` first, every CI job that runs
the suite carries a `Build contracts for dispatch e2e` step
(`.github/workflows/dispatch-browser.yaml`), and a Playwright command you type
yourself needs `bun run build:contracts` once.
The hook refuses the run before it starts a
harness or a Postgres when Playwright has not downloaded a browser the run
needs, naming `bun run e2e:install` and how large that download is.
`e2e/run-server.sh` builds the server
with the caller's `go` into `packages/envoy/dispatch-e2e` and execs it, so a
SIGTERM to the pid it hands its caller stops the server. It pins every server
setting: cookie identity, the production mode, with `alice` and `bob` signed in through the
server's dev sign-in route (`DISPATCH_DEV_SIGNIN=1`, fenced to a loopback
origin, listener and database, with a signing key the server generates for its
process, or one key per run in two-process mode, below),
`DISPATCH_NATS_DISABLED=1`, the fake GitHub origin, a throwaway App
key, a loopback listen host and the suite's dashboard origin. Address the
harness as `127.0.0.1:<port>`, never `localhost`: under that flag the router
refuses any other `Host`. The server process has no caller Home or XDG
directory and receives no inherited `DISPATCH_*`, `ENVOY_*` or `NATS_*`
variable, so neither a shell setting nor `~/.config/legion/envoy.json` /
`~/.local/share/dispatch` can redirect it. The one way in from outside is the
broker switch below: a URL in `DISPATCH_E2E_AGENT_SECRETS_URL`, with the token
of a ServiceAccount that broker lists in
`DISPATCH_E2E_AGENT_SECRETS_ACCOUNT_TOKEN_FILE`, points the server's credential
requests at a broker the caller runs, as the secrets broker's docs rig
(`docs/site/media/broker/rig.sh`) does with the token its local issuer
(`packages/envoy/cmd/dev-oidc-issuer`) keeps.

`DATABASE_URL` is a one-stack run's input unless it names a Postgres of its own
(stacks, below), and must name an isolated
loopback database: `e2e/seed.ts` empties it before every row and never selects a shared
default, and the dev sign-in flag refuses a non-loopback host. psql runs without
`PGHOSTADDR` (`e2e/psql.ts`), which would otherwise send it somewhere the server
never checked. The harness ports `DISPATCH_E2E_PORT`, `FAKE_ENVOY_PORT`,
`FAKE_GITHUB_PORT`, `PLAIN_HTTP_PORT` and `FAKE_BROKER_PORT` are its other
inputs, resolved for the whole suite by `e2e/harness-ports.ts`. Unset, each
server serves on a port the OS chooses and holds until it serves: the Dispatch
server on a socket `scripts/e2e/lib/held-port.py` hands it, each Bun listener by
binding port 0. Each reports its port on its listening line, and
`e2e/global-setup.ts` writes them into the `DISPATCH_E2E_STACKS` manifest every
worker inherits, which is the only variable that hook writes, so
no run shares a port with another and
none empties the database behind a server it did not start. Set, a port is
used as given, and an occupied one fails with `address already in use`.
`DISPATCH_E2E_REUSE_SERVERS=1` says the ports name a harness you started
yourself, and the hook then starts nothing. `AGENTS.md`'s end-to-end section
states that
rule in full — the accepted values and what a bad or duplicated port does. The
harness starts `e2e/fake-envoy.ts` and that listener is the only Envoy the
server ever talks to; tests seed its
live sessions with `setLiveSessions` from `e2e/agents.ts`. It starts
`e2e/fake-broker.ts` as the secrets broker, so the
credential-request feature is on, and tests seed its pending requests with
`setPendingCredentialRequests` from `e2e/fake-broker-helpers.ts`; an empty
`DISPATCH_E2E_AGENT_SECRETS_URL` runs the suite with no broker instead, as a
deployment that configures none (`AGENTS.md` has the switch). It also starts
`e2e/plain-http-proxy.ts` for the plain-HTTP project.

`DISPATCH_E2E_EMBEDDER=fake` starts one listener more, `e2e/fake-embedder.ts`, and points the
server's meaning search at it: the server calls it as it calls Bedrock, and it counts the calls
it was asked for, so a row reads back how many embeddings a stretch of typing cost. The rows of
`e2e/search-tiers.e2e.ts` need it and are skipped without it, since a suite whose assertions
count search hits counts the keyword answers Dispatch gives a deployment with no embedder:

```bash
cd packages/dispatch
DISPATCH_E2E_EMBEDDER=fake bun run e2e -- --project=chromium --project=iphone search-tiers
```

Run the local harness with its isolated database available:

```bash
cd packages/dispatch
DATABASE_URL='postgres://postgres:dispatch@127.0.0.1:55432/dispatch_<issue>?sslmode=disable' \
  bun run e2e
```

One spec by hand is the Playwright command itself, as in
`bunx playwright test --config e2e/playwright.config.ts --project=chromium e2e/broadcast.e2e.ts`
(`bun run e2e` runs the same after the contracts and SPA builds). Playwright stops the harness
when the run ends, a Ctrl-C included, and exits with its own status.

A SIGTERM stops the stacks this suite starts and no more. A run that holds a stubbed row also
starts Playwright's own `webServer` (`e2e/stub/static-server.ts`), which Playwright stops on a
SIGINT and leaves running on a SIGTERM: measured on playwright 1.64.1, a signalled mixed run
exited 143 with every stack of this suite gone and that static server still serving its fixed
port, so the
next run holding a stubbed row was refused with
`http://127.0.0.1:4319/index.html is already used`. It is Playwright's own leak, not this
suite's - a run of the stubbed layer alone, which installs no signal handler here, leaks the
same server - so `kill` that process, or use a Ctrl-C, which stops everything.

Playwright's UI mode starts the stacks itself. The UI server runs `globalSetup`
when a client connects rather than when it begins listening, so a session
opened on the listening URL starts its own stacks, runs rows against them, and
stops them when the session ends: measured on playwright 1.64.1,
a second Run all in
the one session left no second set of stacks, and closing it left nothing
behind. Every project is in play in such a session, whatever `--project` the
command line held, so it needs all three engines downloaded
(`bun run e2e:install`); with only Chromium, it stops at its `global-setup.ts`
node while the same command line without `--ui` runs green.

To point a session at a stack that is already up instead, start one, apply the
variables it prints, and start the UI in that environment:

```bash
cd packages/dispatch
bun e2e/harness-up.ts > exports.sh &
# wait for its last line, `export DISPATCH_E2E_REUSE_SERVERS=1`, then
. ./exports.sh
bunx playwright test --config e2e/playwright.config.ts --ui
```

`kill` that harness when the session ends. `e2e/visual-run.sh` is the same
recipe written as a script, for the container it runs Playwright in. A session
of the stubbed layer alone needs no stack at all, since it addresses none:
`--project=stub --ui` opens as it is.

Playwright's own `--workers <n>` runs the suite in parallel, and the config's
default is 1. `e2e/global-setup.ts` starts one stack
per worker before the first row. A stack is a whole harness of its own — a
Dispatch server, the three fakes, the plain-HTTP proxy, a file-store bucket and
ports the OS chose — and a Postgres of its own, because `e2e/seed.ts` empties
its stack's database before every row. A worker finds its stack by
`TEST_PARALLEL_INDEX`, which Playwright gives it and a restarted worker keeps;
`e2e/harness-ports.ts` resolves it against the `DISPATCH_E2E_STACKS` manifest
the hook wrote, for the ports and for psql's database alike.

A stack's Postgres comes from one of two variables, and a run of more than one
stack that sets neither is refused. On a machine with Docker and no Postgres
binaries, `DISPATCH_E2E_PG_IMAGE` is a `docker run` of the image digest you
name; CI's visual job uses it too, since it runs on the runner with only its
Playwright command in a container. On a machine with a Postgres installation
and no Docker, such as an issue pod or CI's shard and alone jobs, which run in
a container carrying PostgreSQL 18 with `vector`, `DISPATCH_E2E_PG_BIN` is a
directory holding `initdb` and `pg_ctl`.
Each stack's data lives in its own `worker-<i>` directory under the run's own
temporary directory and goes with the run.

```bash
cd packages/dispatch
DISPATCH_E2E_PG_BIN=/usr/lib/postgresql/18/bin \
  bun run e2e -- --workers 4
```

A run of more than one worker is one stack per worker, each with a Postgres, a
file-store bucket and ports of its own, so five variables that name one of
those are refused before anything starts:

- `DATABASE_URL`, because each stack runs its own Postgres, whose URL the
  harness hands its servers and its psql,
- `PLAYWRIGHT_BASE_URL`, because a deployed run has one server every stack
  would share,
- `DISPATCH_E2E_REUSE_SERVERS`, because the servers a person started are one
  set,
- `DISPATCH_E2E_PROCESSES=2`, because two-process mode is two servers on one
  stack's database,
- any harness port you set yourself, because a port is one listener's.

Run instead a Postgres provider with those five unset: `DISPATCH_E2E_PG_BIN`, a
directory holding `initdb` and `pg_ctl`, or `DISPATCH_E2E_PG_IMAGE`, each of
which is a Postgres per stack. The other way is one worker, which the config
defaults to and the one case that takes a `DATABASE_URL`.
`e2e/global-setup.ts` refuses the run before it starts anything, naming the
variable and the reason, so you meet a sentence rather than an `EADDRINUSE`.
The refusal is the harness's own rule, not one caller's habit: a run of more
than one worker meets it however you started it, because one stack owns each of
the five. CI's shard leg sets `DISPATCH_E2E_PG_BIN` and none of the five, so
the rule reaches no job of this repository and only a run by hand.

Every other mode runs on the default one worker, and so one stack: the deployed
run, two-process mode,
a run against servers you started (`DISPATCH_E2E_REUSE_SERVERS=1`) and
`bun run dispatch:local`. `AGENTS.md`'s end-to-end section has the whole model.

`e2e/run-server.sh` on its own is the server for a shell caller: with
`DATABASE_URL`, `FAKE_ENVOY_PORT`, `FAKE_GITHUB_PORT` and the broker switch's
input (`FAKE_BROKER_PORT`, or an empty `DISPATCH_E2E_AGENT_SECRETS_URL`) set and
`DISPATCH_E2E_PORT` unset, it writes
`msg="dispatch: listening" addr=127.0.0.1:<port>` to stderr once the server
serves; read the port from that line. `$!` of a backgrounded run is the server's
pid. A caller that needs the port before the server starts (the broker docs rig
names it in the broker's settings) starts `scripts/e2e/lib/held-port.py keep`
first and sets `DISPATCH_E2E_LISTEN_FROM` to the keeper's unix socket: the
server serves on the keeper's port.

Two-process mode runs two Dispatch servers against the one database, behind a round-robin proxy,
the way a rolling release serves a browser from two builds at once:

```bash
cd packages/dispatch
DATABASE_URL='postgres://postgres:dispatch@127.0.0.1:55432/dispatch_<issue>?sslmode=disable' \
  bun run e2e:multiprocess
```

It builds the dashboard twice (`web/dist`, and `web/dist-b` under another build id), then runs
Playwright with `DISPATCH_E2E_PROCESSES=2`.
Each half is a script you can call on its own: `bun run build:web-b` writes the second bundle,
and `bun run e2e:multiprocess:run` runs Playwright over bundles that are already built, which
is what CI does with the bundles its `binaries` job made.
The run is: Dispatch A on
`DISPATCH_E2E_PORT_A` serving `web/dist`, Dispatch B on `DISPATCH_E2E_PORT_B` serving
`DISPATCH_E2E_WEB_DIST_B` (here `web/dist-b`; default A's), and `e2e/round-robin-proxy.ts` on
`DISPATCH_E2E_PORT`, which alternates requests and websockets between them. As every harness
port, each is used as given when set and chosen by the OS when unset; the proxy starts before
the servers, since both serve its origin, so A's and B's ports are held first (a
`held-port.py keep` each) and each server serves on its keeper's port. Both servers sign
cookies with one key the run generates, so a person stays signed in whichever answers.
`DISPATCH_E2E_SERVER_BIN` runs a prebuilt `envoy-dispatch` instead of the one `run-server.sh`
builds from this checkout, and `DISPATCH_E2E_SERVER_BIN_B` gives Dispatch B one of its own, so a
run can put two builds of the server against the one database as `DISPATCH_E2E_WEB_DIST_B` puts
two builds of the dashboard there; `scripts/e2e/dispatch-two-builds.sh` is the proof that uses
them. It runs
the `*.multiprocess.e2e.ts` specs but the sign-in spec below, in Chromium, which the default run
leaves out; arguments after `--` reach Playwright
(`bun run e2e:multiprocess -- <name>.multiprocess.e2e.ts`). A spec
pins a request to one process with the `X-Dispatch-E2E-Process: a|b` header or a whole browser
context with the `dispatch-e2e-process` cookie, and reads which process served what from the
proxy's `/_proxy/requests` and `/_proxy/sockets`; `AGENTS.md`'s end-to-end section has the whole
contract. Any `DISPATCH_E2E_PROCESSES` other than `1`, `2` or unset is refused.

`DISPATCH_E2E_SIGNIN=oidc` signs people in as a deployment does instead of through dev sign-in:
the harness also starts `packages/envoy/cmd/dev-oidc-issuer` (`e2e/run-signin-issuer.sh`), and
both servers take it as their sign-in issuer, under one `DISPATCH_SIGNING_KEY` per run. Only
`e2e/signin.multiprocess.e2e.ts` runs then, which starts a sign-in at `/auth/start` on Dispatch A
and completes it at `/auth/callback` on B:

```bash
DATABASE_URL='postgres://postgres:dispatch@127.0.0.1:55432/dispatch_<issue>?sslmode=disable' \
  DISPATCH_E2E_SIGNIN=oidc bun run e2e:multiprocess
```

It needs `DISPATCH_E2E_PROCESSES=2`, which `e2e:multiprocess:run` sets and `e2e:multiprocess`
reaches through it; any value other than `oidc` or unset is refused.
The script builds the issuer from this checkout, or runs a prebuilt `dev-oidc-issuer` that
`DISPATCH_E2E_SIGNIN_ISSUER_BIN` names, as `DISPATCH_E2E_SERVER_BIN` does for the server.

## Local instance

`bun run dispatch:local`, from the repository root or from this package, brings up a seeded
Dispatch signed in as a person, to prove a change to a surface only a human acts on (the Inbox and
its snooze, the Agents page and its direct messages, a broadcast) with no production step. It is
the end-to-end harness, left running: it builds the SPA, starts the harness through
`e2e/harness-start.ts` as a Playwright run's stack pool does (the fake Envoy, the fake GitHub, the fake secrets
broker, `e2e/run-server.sh` and the plain-HTTP proxy) on ports the OS chooses, empties the
database, seeds `e2e/workspace.ts`, checks over
HTTP that a cookie from the dev sign-in route reads `/auth/whoami`, the Inbox, the broadcasts and
the agents, and prints:

- the URL that opens the dashboard signed in as `alice` (and one for `bob`),
- the seeded broadcast's page,
- the agent bearer (`e2e-token`) a local Oh My Pi session reaches this instance with,
- the database, the fake Envoy's session list and its reseed endpoint,
- the command that runs a spec against it, with the ports it serves on.

Open exactly the printed `127.0.0.1` origin: `localhost` is another `Host`, and the server refuses
it (`421 HOST_MISMATCH`). Ctrl-C stops every process it started, and so does a failure at any
step; the database's container stays.

Its inputs are the harness's own. `DATABASE_URL`, when set, is used as given: it must be a single
`postgres://` URL whose host (its `host` query parameter when it has one, as for libpq) is
`127.0.0.1`, `localhost`, `[::1]` or a Unix socket directory, and it is emptied and reseeded on
every start. Unset, the command runs `packages/envoy/scripts/dev-postgres.sh`, which creates,
starts or reuses the `dispatch-pg` container, and creates database `dispatch_local` in it once.
That script runs the container from its pinned image, pgvector on PostgreSQL 18, the version
Dispatch's tests and dev databases run; Dispatch's migrations need its `vector` extension. It
refuses a `dispatch-pg` on any other image or major and prints the commands that dump its
databases and restore them into a new container on the pinned image, quoted so they paste as they
read. The harness ports (`DISPATCH_E2E_PORT`, `FAKE_ENVOY_PORT`, `PLAIN_HTTP_PORT`,
`FAKE_GITHUB_PORT`, `FAKE_BROKER_PORT`) are unset unless you need a fixed one: unset, the OS
chooses each, so two instances never collide; set, the port is used as given, and an occupied one
fails with `address already in use`. psql runs without `PGHOSTADDR` (`e2e/psql.ts`), and a Ctrl-C
ends a psql in flight too.

A spec runs against the instance with the printed command, which sets
`DISPATCH_E2E_REUSE_SERVERS=1`, the five ports and the same `DATABASE_URL`; each spec empties the
database and the
fake Envoy, so restart the command to reseed it. `bun run build:web` while it runs refreshes the
SPA, which the server reads from `web/dist` on each request. `docker rm -f -v dispatch-pg` removes
the container, its anonymous data volume and every database in it.

## Acceptance drivers

`e2e/acceptance/block-graph.sh` drives the block graph end to end against a running server, as an
agent sees it, through the HTTP API and the real `dispatch` CLI
(`packages/envoy-client/bin/dispatch.ts`, run in an environment the script builds, so a calling
shell's Dispatch URL, token file and Envoy configuration never reach it). It covers a comment
citing a block and the block's backlinks, a deleted and restored block, a pinned block reference
that changes and is removed, `GET /api/v1/block-links` between the blocks of two documents, each
block's changed version, every block `part_of` its document, a link in a list item, the six facts
every node carries (`made_by`, `made_at`, `changed_by`, `changed_at`, `parent`, `parent_version`)
for an issue, a document, a file, a picture, a block, a removed pinned block, an ask, a comment, a
message, a session and a component, read by its own reference, as an edge's other end and on the
`Node:` line of `dispatch read`, and the older `GET /api/v1/artifacts/{id}/references` route. It
also covers the blocks of comments and messages: a mention in a comment starts at the comment's
block holding it, a node of kind `block`, id `comment/<uuid>#b<n>`, whose `parent` is the comment
(A10);
a comment or message reply that names a block (`--reply-to <comment>#b-b2`,
`--in-reply-to <message>#b-b2`) is a `replies_to` edge to that block, and `dispatch read` quotes it
(A7); the facts of a comment's and a message's block (A11); a block's id kept through a person's
move, reword, split, deletion and restore over `PATCH /api/v1/comments/{id}` with carried ids, and
a body holding block HTML read back byte for byte (A13); and a link pinned to a comment's block that
reads `changed`, then `removed` (A15). It also reads the bounds the server owns and no client
carries: a comment and a message of 48,000 characters taken and read back whole, a paragraph of
32,001 characters refused by name, and `dispatch limits` (A8); a passage only at the end of a block
longer than 32,000 characters found by meaning and a one-line ask option found by its text, each
hit naming the block (A9, skipped with one `SKIP:` line on a server with no embedder); and a
question of 2,001 characters taken by `dispatch ask` and by a document's decision block (A14).
Each passing check prints `PASS: <section> <what>`; the first
failure prints `FAIL: <section> <what>: <observed>` and exits 1. Its inputs:

- `DISPATCH_URL`, the server;
- `DISPATCH_TOKEN`, an agent bearer, or `DISPATCH_TOKEN_FILE`, a file holding one, which the script
  reads itself and never prints;
- `PROJECT`, an existing project key;
- `COMPONENT_SOURCE`, optional, where the component nodes come from (unset, the script reads no
  component node and prints one
  `SKIP: R12 component: COMPONENT_SOURCE is unset, so no component node is read; …` line):
  - `fixture` seeds the harness's fake GitHub on `FAKE_GITHUB_PORT` (9022 under
    `bun run dispatch:local`) with a two-component model in `acme/r12-arch` (`web`, part of
    `platform`), replacing every repository the fake served, adds that repository as an
    architecture source of `PROJECT`, syncs it, and removes the source on exit, also when a check
    fails;
  - `existing` reads the first project whose architecture holds a component with a parent, and
    writes nothing to it; a server with none is a `FAIL`;
  - `none` proves `GET /api/v1/settings/architecture-sources` is `[]` and prints
    `PASS: R12 component: this server has no architecture source, so it holds no component node`;
    a source present is a `FAIL` naming `COMPONENT_SOURCE=existing`.
- `DEV_SIGNIN_LOGIN`, optional, a login the server's dev sign-in route (`/auth/_dev/signin`) signs
  in: `bun run dispatch:local` has the route, a deployed server does not. Editing a comment needs
  the `person` scope, and is its author's alone, so the A13 and A15 comment sections sign that person in with a
  cookie jar, write the comments that person edits, and send each edit with the dashboard's origin.
  Unset, each of those two sections prints one
  `SKIP: … PATCH /api/v1/comments/{id} needs the person scope, which this bearer does not hold, and this server has no dev sign-in: DEV_SIGNIN_LOGIN is unset; …`
  line, and every other section runs.

It writes two issues in `PROJECT`: the first holds the spec, comments, messages and the documents
`cites.md`, `decisions.md`, `changes.md` and `list.md`; the second is its child, retitled, holding an
ask the script rewords, a comment on a quote of its spec and a reply, a message and a reply, and the
uploads `notes.txt` and `pixel.png`. It resolves the ask and closes both issues, also when a check
fails.

Its `A9` section needs a server with an embedder, so the harness is started with
`DISPATCH_E2E_EMBEDDER=1`, which gives it the development embedder beside its dev sign-in
(`e2e/run-server.sh`); without one that section prints its `SKIP:` line and every other section
runs. The harness leaves the embedder off by default, so that a suite whose assertions count
search hits counts the keyword answers Dispatch gives a deployment with no embedder.

Against `bun run dispatch:local`:

```bash
DISPATCH_URL=http://127.0.0.1:8777 DISPATCH_TOKEN=e2e-token PROJECT=CORE \
  COMPONENT_SOURCE=fixture FAKE_GITHUB_PORT=9022 DEV_SIGNIN_LOGIN=alice \
  bash packages/dispatch/e2e/acceptance/block-graph.sh
```

`e2e/acceptance/verification.sh` drives the verification rules the same way, through the HTTP API
and the same real CLI. It covers a `plan_task` and a `check` that serve a spec's `requirement`, and
the `serves` and `needs` edges `GET /api/v1/references` lists for them; every attribute a `check`
carries, read back on the head of `dispatch read --ref`; the refusals a pointer earns — a block no
document holds, a block of an issue in another tree, a `needs` naming a block outside its own plan
— and a `when` the schema does not know, each `400 INVALID_MARKDOWN` naming the pointer or the
attribute; a requirement's id through a `move`; `dispatch result` and `dispatch results`, with a
check id no Verification document in reach holds refused `400 UNKNOWN_CHECK` and a pass counting a
skipped case refused `400 RESULT_CONTRADICTS`; a closed issue's result for an `after_merge` check,
which it takes beside a comment and a message, and the result for a `before_merge` check and the
document write it still refuses `409 ISSUE_CLOSED`; the coverage flags `dispatch read --issue` prints after `Coverage: `,
cleared by a check of the issue and by a check of a child, back when that check is deleted, with
the missing plan task named only once the issue has a plan document; the gap an approval request's
answer names and the covered spec that earns none; `dispatch plan-approve`, refused `409
NOT_PLAN_DOCUMENT` on a spec, and `dispatch request-approval`, refused `409
PLAN_APPROVED_BY_REVIEW` on a plan; the one request that names a spec and its Verification
document together; and, in the reading section, every result of a check printed with all forty
characters of its commit, beside a result at a commit that repository does not hold refused
`400 RESULT_COMMIT_UNKNOWN`. Its inputs are `DISPATCH_URL`, the bearer, `PROJECT`, and two
optional ones:

- `FAKE_GITHUB_PORT`, the harness's fake GitHub, which the server reads as
  `DISPATCH_GITHUB_API_BASE`. The run gives it one repository holding one pull request whose
  title names the issue and one further commit beside that pull request's head, replacing every
  repository the fake served: where `PROJECT` maps no default repository the reading section
  seeds a repository of its own, maps it as `PROJECT`'s default and removes the mapping on exit;
  where `PROJECT` maps one, the setup seeds that repository and maps nothing. The results of the
  check are then compared with that pull request's head, a result at the further commit — whose
  first twelve characters are the head's — stands at none of them, and a result at a commit the
  repository holds neither of is refused. Unset, one `SKIP: reading …` line stands in for the
  section, which no other way of running has a pull request's head to read.
- `DEV_SIGNIN_LOGIN`, as `block-graph.sh` takes it. Recording a review is a person's write alone,
  so the two review sections sign that person in with a cookie jar and answer the approval requests
  the script opened; unset, each prints one
  `SKIP: review …: DEV_SIGNIN_LOGIN is unset; POST /api/v1/artifacts/{id}/reviews needs a person's …`
  line and every other section runs.

One rule governs every result the script records, beside the refusals above.
A result at a 40-character commit the repository of the check's issue does not hold is refused `400 RESULT_COMMIT_UNKNOWN`, naming the commit and that `owner/repo`.
Only GitHub's `422 No commit found for SHA` refuses, and the script reads `PROJECT`'s repository mappings once, in its setup, to know which of two runs it is.
Where `PROJECT` maps no default repository, every result recorded outside the reading section is stored because the check's issue resolves to no repository, which is never asked about; inside the section, each stored result names a commit it seeded its fake GitHub with, and the one commit it seeds nothing with is the refusal it drives.
Where `PROJECT` already maps one, every issue the script writes resolves to that repository, so the setup reads that repository's default-branch head from GitHub with `gh` and every result is recorded at that head: no check stands down, and the run prints no `SKIP` for a recorded result.
A repository `gh` cannot read fails the run, naming it.
Where `FAKE_GITHUB_PORT` is set the setup also gives that repository, that head, the look-alike and the pull request to the fake, since a server reading the fake asks the fake and not GitHub; a server reading the real GitHub needs no such seeding, and a fake-backed run proves this script's fixture and its count rather than a server talking to GitHub.
Every other check runs either way, the `result` section's two refusals among them: a malformed field and an unknown check are refused before any commit is read.

It writes four issues in `PROJECT` — a root with a spec, a Verification document and a plan, a
child of it, a sibling in another tree and one more whose spec and Verification document are
approved together — and closes every one of them, also when a check fails, which withdraws any
approval request it left open. Nothing it writes is read by a second run. Against
`bun run dispatch:local`:

```bash
DISPATCH_URL=http://127.0.0.1:8777 DISPATCH_TOKEN=e2e-token PROJECT=CORE \
  FAKE_GITHUB_PORT=9022 DEV_SIGNIN_LOGIN=alice \
  bash packages/dispatch/e2e/acceptance/verification.sh
```

`e2e/acceptance/block-coverage.sh` takes the first three inputs, with `PROJECT` optional (unset,
every project), and only reads. For every document it can list (each project's documents and each
issue's documents), it counts the document as not indexed when `GET /api/v1/artifacts/{id}/blocks`
lists blocks while `GET /api/v1/references?to=<document>&kind=part_of` lists none, and prints
`documents=<n> indexed=<n> not_indexed=<n>`, then each not-indexed document's reference. A document
with no blocks counts as indexed, since it owes no nodes.

`e2e/acceptance/create-document.sh` drives the command line's own create, the one that makes a
document from a title with no file and no text, through the same real CLI. It takes `DISPATCH_URL`,
the bearer and `DISPATCH_DEFAULT_ASSIGNEE`, a person who has signed in to that server: the bearer
acts for no person, so the project it creates must name the person its unowned work lands on
(`400 INVALID_PROJECT` otherwise). It makes the project and the issue it needs, and runs ten
assertions:
`dispatch artifact --issue <KEY> --name "Launch notes"`, with neither `--path` nor `--content`,
exits zero and says the document was made, naming its `dispatch://` reference; that reference
reads back one version whose markdown is empty; `dispatch read --issue <KEY>` lists the document
under `Documents:`; `dispatch doc-edit` writes a line into it and the line reads back; the same
name a second time with no content is refused, naming the document that holds the title; that
same create with `--force` is refused too, since a create with no content has no version
`--force` could ask for; `--name rows.jsonl` with no content is refused by the Markdown-name
rule, which sends nothing; and `--path` and `--content` each still make a document. Each passing
assertion prints `PASS: <n> <what>` on stderr, the first failure prints `FAIL:` and exits 1, and
a run that reaches the end writes `all <n> assertions passed` as the one line on stdout, so a run
cut short fails a reader that reads that line.

`e2e/acceptance/with-harness.sh` runs one command against a stack of its own, for a driver that
has no server to speak to: it starts `e2e/harness-up.ts`, waits for the stack's exports, applies
them, runs the command with `DISPATCH_E2E_PORT` and the rest in its environment, and stops the
stack when the command ends, exiting with the command's own status. From `packages/dispatch`:

```bash
DATABASE_URL=<a PostgreSQL this run may truncate> bash e2e/acceptance/with-harness.sh \
  bash -c 'curl -sS -o /dev/null "http://127.0.0.1:$DISPATCH_E2E_PORT/auth/_dev/signin?login=alice" &&
    DISPATCH_URL=http://127.0.0.1:$DISPATCH_E2E_PORT DISPATCH_TOKEN=e2e-token \
    DISPATCH_DEFAULT_ASSIGNEE=alice bash e2e/acceptance/create-document.sh'
```

The `curl` is the harness's dev sign-in (`/auth/_dev/signin`), which records `alice` as a person
who has signed in, so the project may name her.

`e2e-token` is the agent bearer the harness pins. Until cybermonklabs/legion#3435 the same thing
was written `bun e2e/run.ts -- <command>`; that program is gone, and the browser suite starts its
stacks through Playwright's own global setup now, which no shell driver can reach.

All four source `e2e/acceptance/lib.sh`, the preamble they share: it refuses an unset input or a
missing command with exit 2, and reads the bearer from `DISPATCH_TOKEN` or `DISPATCH_TOKEN_FILE`
without printing it, unsetting both so no process the script starts inherits it.

## Acceptance run against the deployed image

The `dispatch.acceptance.compose.yml` Compose file builds a Dispatch image from this tree and runs
it for Playwright with its own Postgres volume and database. It signs `alice` and `bob` in through
the dev sign-in route, as the local harness does. It also uses an acceptance-only agent token,
disabled NATS, and a private loopback fake Envoy with no token. `FAKE_ENVOY_PORT` and
`ENVOY_IMAGE_TAG` are its only required inputs: the acceptance service derives its `ENVOY_URL`
from `FAKE_ENVOY_PORT`, so it is the one harness port this recipe sets, and `e2e:deployed` starts
the fake listener on it. Before any row runs,
`e2e:deployed` puts a session in that fake and requires the server's `GET /api/v1/agents` to list
it, so a server reading another Envoy listener refuses the run instead of answering it from the
wrong sessions (`e2e/global-setup.ts`). With `PLAYWRIGHT_BASE_URL` set that hook starts only the
fake Envoy and the plain-HTTP proxy, the latter on a
port the OS chooses. The fake Envoy makes subscriber, Agents-page, and fixture-hook rows exercise
the deployed server instead of a real session. Fake GitHub rows still skip because the acceptance
service does not configure that listener. Its Postgres is PostgreSQL 18 on the volume
`dispatch-acceptance-pg18`; a volume `dispatch-acceptance-pg` left from before holds PostgreSQL 16
data no longer used, and `docker volume rm <project>_dispatch-acceptance-pg` removes it.

```bash
cd packages/envoy/deploy/compose
acceptance_envoy_port=19061
acceptance_dispatch_port=18767
acceptance_pg_port=55660
acceptance_image_tag=dispatch-acceptance-local
acceptance_compose_project=dispatch-acceptance
compose=(env FAKE_ENVOY_PORT="$acceptance_envoy_port" DISPATCH_ACCEPTANCE_PORT="$acceptance_dispatch_port" DISPATCH_ACCEPTANCE_PG_PORT="$acceptance_pg_port" ENVOY_IMAGE_TAG="$acceptance_image_tag" docker compose -p "$acceptance_compose_project" -f dispatch.acceptance.compose.yml)
"${compose[@]}" build dispatch-acceptance
"${compose[@]}" up -d dispatch-acceptance
cd ../../../dispatch
FAKE_ENVOY_PORT="$acceptance_envoy_port" \
PLAYWRIGHT_BASE_URL="http://127.0.0.1:${acceptance_dispatch_port}" \
PLAYWRIGHT_DATABASE_URL="postgres://postgres:dispatch@127.0.0.1:${acceptance_pg_port}/dispatch_acceptance?sslmode=disable" \
E2E_AGENT_TOKEN=acceptance-token \
bun run e2e:deployed
cd ../envoy/deploy/compose
"${compose[@]}" down -v
docker rmi "ghcr.io/cybermonklabs/legion/envoy:${acceptance_image_tag}"
```

`e2e/seed.ts` empties its database before each scenario. Always set
`PLAYWRIGHT_DATABASE_URL` to an isolated test database when using a deployed URL.

The stubbed layer reads three variables of its own, and a run that is not recording needs none
of them. `DISPATCH_E2E_STUB_RECORD=1` makes the stubbed rows meet the real harness and write
what they fetched (`bun run e2e:stub:record`). `DISPATCH_E2E_STUB_PORT` moves the static file
server off 4319, which is what a second checkout on one machine needs, since the server refuses
to reuse one already listening rather than serve another checkout's build. And
`DISPATCH_E2E_STUB_RECORDINGS` points a run at a set of recordings other than the committed
one, which the nightly's re-record job uses so that it can see a committed recording no row
writes any more. A fourth, `DISPATCH_E2E_STUB_ONLY=1`, is how the config carries "this run is
the stubbed layer alone" to the rest of the run; set by hand it tells `globalSetup` to start no
stack at all, for the whole run and real-stack projects included, which then find no server to
address.
The suite has fifteen projects. `chromium` runs every spec except the two plain-HTTP specs, the
three the `-alone` projects below take and the stubbed layer's own (`packages/dispatch/AGENTS.md`,
"The stubbed layer", which the last two projects of the list run).
`iphone` runs, from those same specs, only the tests tagged `@phone` (written at the end of the
test's or its `test.describe`'s title); a test takes the tag when the phone viewport can change
what it
asserts, and not when it only reaches the same control by another route, which is in
`packages/dispatch/AGENTS.md`, "Phone acceptance". An untagged test drops out of `iphone` with
no failure, and `e2e/__tests__/row-configurations.test.ts` refuses a row that runs twice in one
engine, viewport, origin and server-process count. The iPhone project uses Chromium with iPhone
13 viewport, touch, and user-agent emulation. A deployed run (`bun run e2e:deployed`) uses the
same projects but the two stubbed ones and the three `-alone` ones, so its `iphone` project also
runs only the `@phone` tests, and it holds no wall-clock row: that command names no project, and
the three wall-clock projects are out of the default selection. Listed at this head
(`PLAYWRIGHT_BASE_URL=https://<host> bunx playwright test --config e2e/playwright.config.ts --list`),
a deployed run holds 1,269 rows in 167 files, and the other 51 are a wall-clock run of its own,
under the same base URL:
`bun run build:contracts && PLAYWRIGHT_BASE_URL=… bunx playwright test --config e2e/playwright.config.ts --project=chromium-alone --project=firefox-alone --project=webkit-alone --workers=1`,
which is `bun run e2e:clock` less the SPA build that script runs first and a deployed run has
no use for. `bunx` and not a bare `playwright`, which is on no PATH here (`command -v
playwright` in this package exits 1), and `build:contracts` because a fresh checkout has no
`@legion/contracts` build for the configuration to load - the same build `bun run e2e:deployed`
runs before its own Playwright command. The stubbed layer is left out of a deployed run because
it replays a recording
against a build this checkout made, which says nothing about a deployed image, and because that
command builds no `web/dist` for its server to serve. `webkit` and `firefox` select by tag as
well, `@webkit` and `@firefox`: each tagged row exercises one engine behaviour the comment above
the project in `e2e/playwright.config.ts` names, and a comment beside the tag names which.
`webkit` covers where a caret lands beside a collaborator's cursor, and `firefox` covers text
typed over what follows a block's last line break, which that engine's own editing mishandles.
`firefox-download-navigation` runs the one row of
`e2e/shell.e2e.ts` that follows a Download link before a chunk fails. `webkit-iphone` runs the
phone-layout rows of `e2e/agent-view.e2e.ts`, `e2e/agent-view-settings.e2e.ts` and
`e2e/delivery-live.e2e.ts`, and the rows of `e2e/phone-conversation.e2e.ts` that check what the
Conversation's floating pills cover, selected by title, in WebKit with the iPhone 13 profile, since
iOS Safari is the engine those layouts exist for.
`chromium-plain-http` runs `e2e/plain-http-origin.e2e.ts` and `e2e/plain-http-proxy.e2e.ts`, with
the page opened at `http://dispatch-e2e.test:<PLAIN_HTTP_PORT>` (Chromium maps that name to the
local proxy), a plain-HTTP origin that is not loopback and so not a secure context. The proxy
forwards to the dev sign-in server at its loopback origin. The project proves a document takes a
new paragraph and a comment renders formatted there, where `crypto.randomUUID` does not exist, and
that the proxy refuses any other `Host` as the server's own fence does and keeps no request open
upstream once a page has closed. `chromium-plain-http-sign-out` runs the sign-out rows of
`e2e/keyed-writes.e2e.ts` on that same origin, where the browser has no `navigator.locks`, so a
tab holds the person's writes for another tab's sign-out on a renewed lease rather than on that
tab's lock.
The last three projects run the rows whose claim is a wall-clock time, with the machine to
themselves: `chromium-alone` runs `e2e/heavy-bodies.e2e.ts` and `e2e/nested-directive.e2e.ts`
(a card draws without holding the main thread for a second)
and `e2e/agent-workspace-timing.e2e.ts` (a sleeping live view of an ordinary conversation comes
back inside half a second on the fastest of up to five fresh wakes, a sleeping view at the
stream's worst case comes back inside 1.2 seconds on the fastest of up to eight fresh wakes, and
the Agents page paints six hundred sessions inside 100 milliseconds while holding the main
thread under 150 ms, the first load inside both of those bounds of up to twenty taken), and
`firefox-alone` and `webkit-alone` run `e2e/heavy-bodies.e2e.ts` in the other two engines.
Each sets `default: false`, so a run that names no project holds none of their 51 rows
(`bun run e2e:clock:run -- --list` prints `Total: 51 tests in 3 files`), and each
caps its own workers at one.
What holds a reading to a quiet machine is the run being one worker wide, and the three files
reach that in two ways: `e2e/main-thread-hold.ts` fails a measuring row of
`e2e/heavy-bodies.e2e.ts` or `e2e/nested-directive.e2e.ts` in a run of more than one worker,
since four workers on one machine make a second of wall clock unmeasurable, and the rows of
`e2e/agent-workspace-timing.e2e.ts`, which read a clock of their own, take that one worker from
the command that runs them.
`bun run e2e:clock` is the command that runs all three projects, which its `e2e:clock:run` half
names with `--workers=1` beside them; one file in one engine is
`bun run e2e -- --project=chromium-alone e2e/heavy-bodies.e2e.ts --workers=1`, which names that
worker count itself rather than taking the configuration's default, and which writes the project
as `--project=<name>`, since the option is variadic and would read the file after a space as a
second project name.
`bun run e2e:install` installs all three browsers.

## Phone check

Browser emulation covers responsive layout. A phone acceptance check runs against
a tailnet-reachable Dispatch server: configure `DISPATCH_LISTEN_HOST` with the
host's Tailscale IPv4 address (`tailscale ip -4`), use that tailnet address and
`DISPATCH_PORT`, open the Inbox from the phone, and answer an open ask. Set
`DISPATCH_INSECURE_COOKIE=1` only for HTTP; HTTPS keeps the normal secure cookie
setting.
