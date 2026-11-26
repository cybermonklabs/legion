# The outside agent's acceptance

This check proves what a person from outside the deployment's sign-in domain gets when their
agents work with ours: their agent, on their own machine and on the public internet, reaches
Dispatch, the Envoy listener and the secrets broker only through the three edge origins, is held
to the one project an access list names them on, and gets no secret. It has two legs and one gate.
The gate must pass before the deployment names a real person from outside on any access list.

| File | What it is |
| --- | --- |
| `outside-agent-tailnet.sh` | The one command. It runs on a developer box on the private network, runs rows T1-T4 there, starts the public leg and prints the gate. |
| `outside-agent-public.sh` | The public leg, rows P1-P10 and the Codex leg's C0-C7. A workflow on a runner on the public internet runs it, after that workflow's browser signs the test person in. |
| `outside-agent-codex.sh` | The Codex leg, rows C0-C7: the outside person's Codex CLI with the codex-envoy plugin's npm release, under row P2's device login, and that release's provenance. The public leg runs it after P10. |
| `outside-agent-codex-model.ts` | The Codex leg's keyless stand-in model, which speaks the Responses API and plays one scripted turn per run. It binds a port of the kernel's choosing and answers `/_ready` with the leg's nonce. |
| `outside-agent-codex-provenance.ts` | The Codex leg's reads of row C0: the registry's record and provenance attestation of the plugin's release, and the byte comparison of two builds' bundles. |
| `outside-agent-lib.sh` | What the legs share, sourced from beside them: input and tool checks, the text and JSON reads, the real-tool links, the scratch issue's title, `await_child`, which waits for a child's end and returns within a tenth of a second of it, and `reap`, which stops and waits for every background child a leg started. A leg's wait for something on its own machine (a child's end, a file, a socket, a screen, a line of its model's log) looks at it every tenth of a second, so it ends as that holds; its bound only says how long it may take. A read of a remote service (the edge's session list, a workflow run) keeps its own pace. |
| `outside-agent-peer.sh` | The tailnet leg's peer of row P4 and its session outside the project of row P6. |
| `outside-agent-sweep.sh` | The tailnet leg's half of the read sweep and its two controls. |
| `outside-agent-pins.sh` | The tailnet leg's reads of the deployed pins and of the build Dispatch serves, for rows pins and pins-end. |
| `outside-agent-probe.ts` | The calls that carry a broker credential (DPoP), which a shell cannot sign, the scripted peer, and the public session's keyless model. Both legs run it with `bun`. |
| `outside-path-reach.sh` | The TCP reach check that row P9 runs. |
| `../__tests__/outside-agent-acceptance.test.ts` | The test that each leg prints UNKNOWN, never PASS, for a row whose target is absent, with its control; that `reap` stops each child shape the legs use and no other process; that `await_child` ends as its child ends and at its bound; and that the tailnet leg's own judgments hold against stand-ins. |
| `../__tests__/outside-agent-public.test.ts` | The test that the public leg's own judgments (P6, P7, P10 and T5's agent part) hold against stand-ins. |
| `../__tests__/outside-agent-codex.test.ts` | The test that the Codex leg's rows hold against stand-ins, each beside the change that makes it fail, and that its stand-in model answers the requests Codex sends as the rows need. |
| `../__tests__/outside-agent-fixture.ts` | What the test files share: where the legs are, the rows each prints, and the helpers that copy a leg and read its TSV. |
| `../__tests__/outside-agent-tailnet-stand-ins.ts` | The stand-ins the tailnet leg's tests run it against: the checkout, the deployment repository and workflow behind a stand-in `gh`, the broker's tools, Dispatch, the sweep and the public leg. |
| `../__tests__/outside-agent-stand-in-dispatch.ts` | The stand-in Dispatch those tests run in a process of its own, so no test worker's busy event loop holds its answers. It binds a port of the kernel's choosing and writes it to its state directory, and answers `/_ready` with a nonce the test passed it, so no other process can take its port first and no other run's stand-in passes for it. The closed ports the tests name are `outside-path-reach-fixtures.ts`'s `closedPort`: each held bound and never listening by the test process, so no other process can take one. |

## What each leg prints

Each leg prints a TSV on stdout: the header `row want got verdict`, then one line per row. A row
reads `PASS`, `FAIL` or `UNKNOWN`. A row that could not run reads `UNKNOWN`, and its `got` names
what the leg could not read: an input that is not set, a program that is not on PATH, a server
that did not answer, or a row it needs that did not pass. A row never reads `PASS` because it did
not run, and a verdict that is none of the three, copied from another file, counts as `UNKNOWN`.
Each leg exits 0 when every row passed, 1 when a row failed, and 2 otherwise.

The public leg ends with a `verdict` row. The tailnet leg prints its own rows, then rows P1-P10 and
C0-C7 as the public leg's TSV has them, then a `public` row (the public leg's own verdict,
revocations and end), the sweep, `pins-end` (the pins and Dispatch's serving build read again) and
a `gate` row. The gate passes only when every row reads PASS; its `got` names each row that did not,
with its verdict, so a C row that reads UNKNOWN holds the gate as a FAIL does.

No row prints a token, a proof or a secret value. Rows T2 and T3 read the canary secret's length
alone. A row quotes no output of an operator's command (the approve, revoke and browser commands,
the plugin install), which may print a cookie, a code or an invite token: only its exit status and
a fixed phrase.

## How to run it

On a developer box, from a checkout of this repository with `bun install` done and
`packages/pi-envoy` built:

```bash
OUTSIDE_PINS=pins.txt \
OUTSIDE_PINS_REPO=example-owner/example-deploy \
OUTSIDE_ENROLL_REFUSAL_COMMIT=<the merge commit of the enrollment refusal> \
OUTSIDE_DISPATCH_COMMITS='<the merge commit of no broker token approves> <the merge commit of no restricted member names an issue repository>' \
OUTSIDE_TAILNET_BROKER_URL=https://secrets.internal.example \
OUTSIDE_TAILNET_DISPATCH_URL=https://dispatch.internal.example \
OUTSIDE_LISTENER_URL=https://envoy.edge.example \
OUTSIDE_INTERNAL_LISTENER_URL=https://envoy.internal.example \
OUTSIDE_FOREIGN_PROJECT=<a project of the box's person other than the scratch project> \
OUTSIDE_FULL_COOKIE_FILE=full-member-cookie.txt \
OUTSIDE_PERSON=outside-e2e@example.com \
OUTSIDE_CANARY=<the canary secret's name> \
OUTSIDE_REPOSITORY=example-owner/example-repo \
OUTSIDE_SWEEP_PROJECT=<a project the test person is not on> \
OUTSIDE_SWEEP_FULL_TOKEN_FILE=full-member-token.txt \
OUTSIDE_SWEEP_CONVERSATION_FILE=<the conversation file of a session that worked in that project alone> \
OUTSIDE_SWEEP_WRITE_ISSUE=<a scratch issue of that project> \
OUTSIDE_WORKFLOW_REPO=example-owner/example-deploy \
OUTSIDE_WORKFLOW_INPUTS='invite_url=https://dispatch.edge.example/enroll#<token>' \
OUTSIDE_TAILNET_TSV=tailnet.tsv \
  bash packages/dispatch/e2e/acceptance/outside-agent-tailnet.sh EXAMPLE
```

`pins.txt` holds exactly one `name file key` line for each of `broker`, `dispatch` and
`edge-listener` (a second line of one name reads UNKNOWN, since only one image could be held): the
deployment's configuration file, a path in `OUTSIDE_PINS_REPO`, and the key whose value is that
deployment's image. The leg reads each file at that repository's main with `gh api`, at the start
and again at the end, and reads the commit each image was built from with `crane config`
(`org.opencontainers.image.revision`), or with `OUTSIDE_IMAGE_REVISION_COMMAND`. Without
`OUTSIDE_PINS_REPO` each file is a local file, and row `pins-end` reads UNKNOWN: a local file cannot
show that the main moved.

What each service says of the build it serves: Dispatch's `/healthz` names `commit`, the commit its
binary was built from, and `fleet_commit`, the commit every live server process serves. The
broker's and the edge listener's `/healthz` name a status alone. So the leg holds Dispatch's
serving build to its pin at the start and at the end, and takes the broker's and the edge
listener's builds to be their pins'.

The workflow's inputs reach `gh workflow run --json` on its stdin, never its command line, which
any account on the box can read. The leg sends `checks=all`, `project`, `peer_session`,
`tailnet_user_code`, and the sweep's `sweep_project` and `sweep_fills`; `OUTSIDE_WORKFLOW_INPUTS`
adds the rest, one `name=value` a line. The workflow declares one more, `invite_url`: the owner's
single-use enrollment link for the test person, which the workflow masks before any step prints
and the run spends. GitHub refuses a run with an input the workflow does not declare (422).

Each leg's header lists every input it reads. The tailnet leg's optional inputs are
`OUTSIDE_PEER`, `OUTSIDE_PEER_SESSION`, `OUTSIDE_PEER_PLUGIN_DIR`, `OUTSIDE_PEER_AGENT_DIR`,
`OUTSIDE_PEER_AGENT_FILES`, `OUTSIDE_PEER_ENV`, `OUTSIDE_OWNER_REMOVE_COMMAND`,
`OUTSIDE_IMAGE_REVISION_COMMAND`, `OUTSIDE_TAILNET_TSV`, `OUTSIDE_RESULT_ISSUE` (an issue that gets
the result as one comment), `OUTSIDE_LEGION_CHECKOUT` and `OUTSIDE_ALLOW_HTTP`.

`OUTSIDE_FULL_COOKIE_FILE` holds a full member's browser session at Dispatch's private origin,
`<name>=<value>` as the Cookie header carries it. The leg gives it to curl in a header file of mode
0600 alone, with the origin's own `Origin` on every call, which Dispatch asks of a cookie's write.
No broker token a machine holds approves, so the positive controls of T3 and P7 read with it. The
sweep's GitHub proxy reads use it where it is given, and else the full member's token
(`OUTSIDE_SWEEP_FULL_TOKEN_FILE`): a build of Dispatch whose proxy reads a browser's person alone
refuses that token (401 `NO_IDENTITY`, 403 `OAUTH_SCOPE_REQUIRED`), and the sweep row then reads
UNKNOWN naming the refusal.

The tailnet leg does these steps, in this order:

1. It reads the pins and prints them, and reads the build Dispatch serves. It does not start
   unless the broker's build holds the enrollment refusal, Dispatch serves its pin's build on every
   process (`commit` and `fleet_commit` both the pin's), and that build holds each commit of
   `OUTSIDE_DISPATCH_COMMITS`: no broker token gets the approve scope, which P7's refusal and its
   control rest on, and no restricted member names an issue's repository, which decides the
   repositories P5 and the sweep's GitHub proxy rows read (`git merge-base --is-ancestor`).
2. It finds or makes the scratch project's issue that names `OUTSIDE_REPOSITORY`, with the box's
   own `dispatch` command. Row P5's admitted interest reads that repository.
3. It starts a scratch `agent-secrets-helper serve` in a `mktemp -d` directory, with its own HOME,
   XDG directories and `AGENT_SECRETS_*` paths, the test person in its operator file, and the
   broker's private origin as `AGENT_SECRETS_URL`. It runs `agent-secrets machine login` there and
   reads the code.
4. It starts the peer at the edge listener, with the box's own identity and a token that names the
   project. By default (`OUTSIDE_PEER=omp`) the peer is an Oh My Pi session in tmux whose model
   answers the public side. Its input is Envoy cards from any session the edge admits into the
   project, so it runs with nothing of the box but its model and its broker identity: `env -i`, a
   scratch HOME holding only copies of the model's configuration and credential (the files
   `OUTSIDE_PEER_AGENT_FILES` names, default `config.yml models.yml agent.db`, out of
   `OUTSIDE_PEER_AGENT_DIR`, default `~/.omp/agent`), the box's `AGENT_SECRETS_*` settings with its
   sign-in directory pinned, the variables `OUTSIDE_PEER_ENV` names, no `OUTSIDE_*` variable, no
   built-in tool (`--no-tools`), and only the envoy extension (`dist/envoy.js`) of the plugin
   `OUTSIDE_PEER_PLUGIN_DIR` names (default this checkout's `packages/pi-envoy`, built). Sending an
   Envoy message is all it can do. On a local stack with no model, `OUTSIDE_PEER=probe` runs the
   scripted peer (`outside-agent-probe.ts peer`) instead; only with `OUTSIDE_ALLOW_HTTP=1`. It also
   keeps a session outside the project, `<peer>-foreign`, registered at the private listener, for
   row P6. Its token names `OUTSIDE_FOREIGN_PROJECT` alone: a token naming no project carries every
   project of the box's person, the scratch project among them, and the edge would rightly list it.
   The probe presents that token to the edge too, which must refuse it `token_not_admitted`.
5. It runs the read sweep's full-member half: `restricted-sweep.sh` (beside this file, added by
   #4608; until #4608 merges the file is absent, the sweep row reads UNKNOWN, and so the gate cannot
   pass) in its fills mode with the box's full member (`OUTSIDE_SWEEP_PROJECT`,
   `OUTSIDE_SWEEP_FULL_TOKEN_FILE`, a personal token of the person scope given to the sweep as
   `SWEEP_FULL_TOKEN`, with which it makes its reads, its writer's comment and direct message and
   the agent view, `OUTSIDE_SWEEP_CONVERSATION_FILE`, the test person as `SWEEP_MEMBER`, and the
   full member's cookie as `SWEEP_FULL_COOKIE`, with which it reads the GitHub proxy where it is
   given, and with the token where it is not; a build of Dispatch whose proxy reads a browser's
   person alone refuses the token, and its fills then hold no `can_fail` line, its proxy control
   names that refusal and its row reads UNKNOWN), and starts its writer. Every 15 s until the public leg
   ends, when the leg stops it, the
   writer comments on the scratch issue `OUTSIDE_SWEEP_WRITE_ISSUE` and sends a direct message to
   the fills' session of that project, and prints `write <began> <ended> <comment id> <message id>
   delivered` (`began` read before the comment's POST, `ended` after the message's POST answered),
   `... not delivered` when the full member's own feed was not sent both, or `write <began> <ended>
   - not written`. A write is inside the runner's live window when it began after the window opened
   and ended before it closed. As #4608's own join reads them, every write the writer printed must
   be delivered, inside the window or not: one not delivered or not written anywhere reads UNKNOWN;
   then one delivered write inside the window is the control. A line whose stamps are not of the
   form `date -u +%Y-%m-%dT%H:%M:%SZ` prints, that began after it ended, or that is delivered with
   ids not of Dispatch's lowercase 8-4-4-4-12 form, reads UNKNOWN too. No full member's credential
   goes to the runner: the
   fills file holds none, and is at most 40 KiB so that it fits one input. Its contract is #4608's at
   `ca10a682ba`, fills version 4 (`restricted-sweep-fills<TAB>4`), which name the SHA-256 of the
   origin they were made at (`origin_sha256`), of the `routes.txt` they were made from (`routes`)
   and of the member's own projects' keys and creation times (`deployment`). Fills mode writes no
   file that lacks a fill,
   nor one where the member's project list gives a project no creation time, and the leg sends no
   fills of another version (each, the sweep row UNKNOWN on the box). A runner refuses, before its
   first request, fills of another `routes.txt` than its own checkout's, of another deployment, or
   older than 55 minutes, and a `SWEEP_LIVE_SECONDS` below 45; so the workflow's checkout must hold
   this checkout's `routes.txt`, the workflow sets no shorter window, and the leg starts the public
   leg right after the fills.
   Each refusal is the runner's UNKNOWN, and the sweep row's. The runner's own control of the
   member's feed takes one of two forms, and the workflow gives exactly one: with
   `SWEEP_MEMBER_SESSION`, the member's direct message to their own session in the scratch project,
   which the workflow sets to the session its driver registered at the edge as the test person (the
   agents leg); with `SWEEP_MEMBER_ISSUE`, the member's own comment and message on the scratch
   issue, which the workflow sets from its `sweep_member_issue` input (the browser leg, where the
   person holds no session). The member's feed must be sent each write, and the message must read
   200 by `GET /api/v1/messages/{id}`. It needs no input from this leg. Without either, or with
   both, the runner reads UNKNOWN, and so does the sweep row. A runner's
   UNKNOWN, such as a GET route the deployment's own index lists and `routes.txt` lacks, keeps the
   sweep row UNKNOWN. Then the sweep's proxy control. The fills must hold a `can_fail` line, the
   full member's answer that can fail, for each GitHub proxy read the runner makes:
   `GET /api/github/rest/repos/{owner}/{repo}/` with `pulls/{number}`, `issues/{number}`,
   `commits/{sha}/check-runs` and `actions/runs?head_sha={sha}`, and
   `GET /api/v1/github/app-installation/{owner}/{repo}`. And the full member's browser reads,
   through the GitHub proxy, the workflow runs of one commit of the repository the fills name
   (the one only the sweep's project's issues resolve to, which the runner's proxy rows read), and
   must get 200 with a list of workflow runs.
6. It starts the public leg, within the code's 15 minutes, and only in a private or internal
   repository (`gh repo view` reads `PRIVATE` or `INTERNAL`), since a run's inputs are as readable
   as its repository and an internal one is readable by the owning enterprise's members alone; a
   public repository, or a visibility not read (a `gh repo view` that exits non-zero reads nothing,
   whatever it printed), reads UNKNOWN and starts no run: `gh workflow run
   --json` of `OUTSIDE_WORKFLOW` (default `outside-edge-e2e.yaml`) in `OUTSIDE_WORKFLOW_REPO`, its inputs
   `checks=all`, `project`, `peer_session`, `tailnet_user_code`, `sweep_project`, `sweep_fills`
   (the fills file, base64, when fills mode made one) and `OUTSIDE_WORKFLOW_INPUTS` on stdin. With
   `OUTSIDE_PUBLIC_COMMAND` set, it runs that command here instead, for a local stack. When the
   public leg did not start, row T1 reads UNKNOWN at once, with no wait for the code: only the
   public leg's browser approves it.
7. It waits for the scratch login to be approved, and runs rows T1 and T2. Then the full member's
   helper, the positive control of T3 and P7: the full member's browser mints a personal agent
   token, which must answer 201 and is revoked at once, and approves the machine login of a second
   scratch helper whose operator is that member and whose broker is `OUTSIDE_TAILNET_BROKER_URL`,
   the broker T2's helper enrolls at, which must answer 200. T3 enrolls through that helper; then
   its login is revoked and the helper stopped. Should the leg end before that, an INT, a TERM or
   any exit, its cleanup revokes that login, so the full member's credential never outlives the leg.
8. During the public run's T5 window it runs T5's removal. Once the peer's transcript holds
   `outside-e2e t5-open <nonce>`, it reads the test person on the access list (the control), asks
   the owner on this terminal to remove them and waits for Enter; with
   `OUTSIDE_OWNER_REMOVE_COMMAND` set, it runs that command instead, as a harness acting as the
   owner's browser does on a local stack. It reads its clock before it asks and after the owner
   answers: the removal was made between the two. Re-adding the person is the owner's own act.
9. It waits for the public run to end, at most two hours (the workflow's budget is 115 minutes;
   a run not ended by then reads as no end, and the public row UNKNOWN), downloads its artifacts
   `outside-agent-public`, `outside-edge-revocations` and `restricted-sweep`, stops the sweep's
   writer, and runs row T4. The wait for the T5 window to open is held to an hour alike.
10. It judges T5 from the public leg's T5 rows and the removal's two clocks, prints rows P1-P10
    and C0-C7, the public leg's own last word and the sweep, reads the pins and Dispatch's serving
    build again (row `pins-end`), and prints the gate.
11. It stops every child it started and waits for each, stops the peer's tmux server and waits
    for the peer to exit, and removes its scratch directory.

## The workflow's contract with the public leg

The workflow runs `outside-agent-public.sh` after its own sign-in, with:

| Variable | What it holds |
| --- | --- |
| `OUTSIDE_DISPATCH_URL`, `OUTSIDE_BROKER_URL`, `OUTSIDE_LISTENER_URL` | The three edge origins. |
| `OUTSIDE_PROJECT`, `OUTSIDE_PERSON`, `OUTSIDE_PEER_SESSION` | The scratch project, the test person, our peer session. |
| `OUTSIDE_APPROVE_COMMAND` | A command line that approves one device-login code as the signed-in test person. Run with the code as its last word, it prints `APPROVED <code>` and exits 0, or exits 1 (refused) or 2 (could not tell). |
| `OUTSIDE_PI_ENVOY_VERSION` | The `@sjawhar/pi-envoy` release that row P1 installs. |
| `OUTSIDE_REPOSITORY` | The repository the scratch project's issue names. |
| `OUTSIDE_PATH_TARGETS` | The target list of row P9. |
| `OUTSIDE_AGENT_PUBLIC_TSV` | Where the leg writes its TSV. The workflow uploads it as artifact `outside-agent-public`. |
| `OUTSIDE_T5_COMMAND` | The workflow's browser for the T5 window: a command line, run in the background with `OUTSIDE_T5_DEADLINE` (Unix seconds) and `OUTSIDE_T5_TSV` (a file). It polls the test person's cookie at most every 5 s until it gets 401, then tries the passkey sign-in, and writes rows `T5-cookie` and `T5-passkey` (`row want got verdict`), each `got` holding `at=<Unix seconds>` of the 401 and of the refused sign-in, before the deadline. |
| `OUTSIDE_T5_SECONDS` | How long the T5 window stays open, at most (default 1200). |
| `OUTSIDE_REVOKE_COMMAND` | The workflow's revoke step, a command line the leg runs after P10 and before the T5 window, while the person's cookie still reads their own list. It gets `OUTSIDE_EDGE_REVOKE_KEEP`, P2's user code, and revokes every other device login the run approved, the scratch helper's included, writing the `revoke-<credential id>` rows of `outside-edge-revocations`. Exit 0, 1 or 2 is the leg's `revocations` row. P2's login stays live into the window, where the removal stops it; its key lives only in the leg's scratch HOME, which the leg removes. |
| `OUTSIDE_INSIDE_DISPATCH_URL` | Dispatch's private origin, `https://<host>`, which C2's control points the Codex plugin's `DISPATCH_URL` at. It has no default, and no host is written in this repository: the workflow passes it from its own secrets or inputs, as it passes the edge origins. Without it C2 reads UNKNOWN, and so the gate does not pass. |
| `OUTSIDE_CODEX_ENVOY_VERSION` | The exact `@sjawhar/codex-envoy` release the Codex leg installs from npm, as the outside person installs it (`5.0.1`, never a range or a dist-tag). No default; without it every C row reads UNKNOWN. |
| `OUTSIDE_CODEX_ENVOY_COMMIT`, `OUTSIDE_DEVICE_LOGIN_COMMIT` | Row C0: the commit the release's provenance must name, and the device login's merge commit, which that commit must hold. Whole 40-character commit ids; without either, C0 reads UNKNOWN. The workflow's checkout must hold the history between them (a checkout with its whole history; a shallow one reads UNKNOWN). |

No model credential goes on the runner: the leg's two sends in P4 are scripted, and the model turn
is the peer's, on the developer box. The public Oh My Pi session still runs on a model, because an
Envoy card is handed to it as a turn and a session with no model refuses the card and holds
nothing: the leg runs it on `outside-agent-probe.ts model`, a keyless OpenAI-compatible endpoint
on 127.0.0.1 that answers every turn with one fixed line (`--model outside-e2e/scripted`) and
reads a request body of at most 4 MiB.

Programs on PATH: bash, curl, tmux, bun, `omp`, git and tar, and npm (with node), which the Codex
leg installs `@openai/codex@0.171.0` and verifies the plugin's release with. The step needs about 50
minutes: P5 waits up to 15 minutes for a GitHub event, beside P4 and P8, P8 waits for a token to
expire (300 s), the Codex leg takes a few minutes (two npm installs and three interactive Codex
runs, each bounded), and the T5 window lasts up to 20 minutes.

After P10 and the revocations the leg opens the T5 window. It tells the peer
`outside-e2e t5-open <nonce> at <ms>`, starts `OUTSIDE_T5_COMMAND`, trades P2's device login afresh
every 10 s, and keeps a relay socket open with the newest token, opening another when one closes,
as the agent would. Once a trade is refused, it watches the socket open then until it closes or
325 s pass, and goes on trading every 10 s meanwhile and once more as it closes. It prints rows
`T5-agent` (with `last_answered_at=`, the time of the last trade answered, `refused_at=`,
`socket_opened_at=`, `socket_closed_at=` in Unix seconds, the close `code=`, the count of
`refusals=` and of `unread_after=`, the later trades with no readable answer), `T5-cookie` and
`T5-passkey`; the tailnet leg judges T5 from them.

Before the window, `OUTSIDE_REVOKE_COMMAND` revokes every device login the run approved but P2's,
the scratch helper's included, while the person's cookie still reads their own list. The workflow
uploads its rows, one per revoke, `revoke-<credential id>`, as artifact `outside-edge-revocations`.

## The public leg's rows

Every process the leg starts runs with `env -i`, a HOME of its own, and these settings alone:
`ENVOY_URL`, `ENVOY_INBOUND=listener`, `DISPATCH_URL`, `DISPATCH_TOKEN_BROKER=1`,
`AGENT_SECRETS_URL`, `AGENT_SECRETS_MACHINE_IDENTITY=device-login` and `AGENT_SECRETS_PROJECTS`,
beside `OMP_SKIP_SETUP=1`, Oh My Pi's own switch that keeps its first-run setup wizard shut in a
fresh HOME. The leg links the real `bun`, `omp` and `tmux` for them, because a version manager's
shim finds its program through the caller's HOME. A program it finds and cannot link reads P1
UNKNOWN naming the program and the reason, and every row that starts a child under that PATH waits
on P1. The tailnet leg reads every row from its pins on UNKNOWN the same way, and the Codex leg
every one of its rows.

| Row | It passes when |
| --- | --- |
| P1 | `omp plugin install @sjawhar/pi-envoy@<version>` installs that version. |
| P2 | `dispatch login --device --person <person>` prints a code, the approve command approves it, the login exits 0, and `~/.config/legion/device-login.json` has mode 0600. |
| P3 | Dispatch's `GET /api/v1/whoami` with the agent's token answers `kind` agent, `owner` the test person, `project` the scratch project and `scopes` `[]`. |
| P4 | An Oh My Pi session in tmux registers at the edge listener, and the edge serves a relay socket. The probe, as that session's own session id, sends the peer `outside-e2e ping <nonce> at <ms>`. The session holds the peer's `pong` and then its `hello` within 10 s of the peer's sends. The probe sends `outside-e2e reply <nonce> at <ms>`, and the session holds the peer's `done` within 10 s. What the session holds is read from its transcript, since the TUI folds an Envoy card to its header, and a card's lag is timed from its envelope `at`, the time its sender's client sent it. A card held with no envelope `at` reads UNKNOWN. |
| P5 | Interests in another project's Dispatch topic (403), in `notifications.slack.>`, `notifications.whatsapp.>` and `notifications.team.>` (403 `topic_not_in_project`), in another repository and in `notifications.github.>` (403 `repository_not_in_project`), and a publish of the project's own Dispatch event (403 `topic_not_published`) are refused. An interest in `OUTSIDE_REPOSITORY`'s events is admitted, and one of its events arrives within 15 minutes; the probe opens its socket again on a fresh token each time the edge closes one at its token's expiry. A 5xx is the edge not deciding, and reads UNKNOWN. |
| P6 | `GET /v1/sessions` lists the peer, which is the control, and not `<peer>-foreign`; a send to `<peer>-foreign` is answered 404, as a send to a session no listener knows; and a relay-socket `sub` to the peer's subject is refused `subject_not_granted`. Its `got` ends with each part's reading: `peer_listed`, `foreign_listed`, `foreign_send` and `sub`. |
| P7 | `POST /api/v1/me/agent-tokens` and a credential-request approve with the agent's token each answer 403 `OAUTH_SCOPE_REQUIRED`, the refusal of a credential that lacks the route's scope. A 403 with another code says nothing of that rule, and reads FAIL. |
| P8 | A relay socket closes with 4401 within 25 s after its token's expiry, and the token lasts at most 300 s. A close before the expiry, such as a dropped connection, reads UNKNOWN: it shows nothing of a close at the expiry. A message sent to the Oh My Pi session 330 s after it was first listed is in its transcript within 10 s, so the session opened its socket again: every token its first socket rode was traded before it was listed. |
| P9 | `outside-path-reach.sh` from the Dispatch edge origin passes. |
| P10 | No live view of the outside session reaches Dispatch. A session's live view is its `agentstream` frames, which Dispatch reads for its own people's sessions. As the Oh My Pi session's own session id at the edge, a publish of `agentstream.<session>.frames` is refused 403 `topic_not_published`, an interest in `agentstream.>` 403 `topic_not_in_project`, and a relay-socket `sub` of `agentstream.<session>.>` `subject_not_granted`. Another answer reads FAIL; a 5xx or no readable answer reads UNKNOWN. It runs after P8, because its interest and its socket, made as that session, replace the session's own. |
| T5-agent | After the first refused trade, every later trade is refused too, and the socket open at that refusal closes with the edge's own code, 4401 or 4403. A trade answered later reads FAIL; a later trade with no readable answer reads UNKNOWN, since whether it was refused is not read. With no socket open at the refusal (the removal came before one opened), or a close with another code, the socket part reads UNKNOWN. |

## The Codex leg's rows

After P10 the public leg runs `outside-agent-codex.sh` with its own HOME, where P2's device login
is, so the Codex agent trades the same login and no second one is approved. The Codex leg:

- installs one exact Codex release, `npm install @openai/codex@0.171.0`, into its own scratch
  directory, checks that the integrity npm recorded for it and for its Linux x64 binary package is
  the one the leg pins (else every row reads UNKNOWN), and runs that native binary;
- installs the plugin as the outside person does: a local marketplace named `outside-e2e` whose
  plugin source is `{"source": "npm", "package": "@sjawhar/codex-envoy", "version":
  "<OUTSIDE_CODEX_ENVOY_VERSION>"}`, then `codex plugin marketplace add <dir>` and `codex plugin add
  codex-envoy@outside-e2e`, into a `CODEX_HOME` of its own. Codex names the root it installed at,
  `$CODEX_HOME/plugins/cache/outside-e2e/codex-envoy/<version>` on 0.171.0; the leg reads the
  release's version there and its integrity at the registry. `OUTSIDE_CODEX_ENVOY_SOURCE=checkout`
  builds the plugin from the checkout instead (`packages/claude-envoy/scripts/build.ts
  --codex-out`), for a development run, whose C0 reads UNKNOWN, so it never passes the gate;
- runs the command the outside person is told to run, `codex --no-daemon -m gpt-5.5 "<prompt>"`,
  three times, each in a tmux pane of its own as P4 runs Oh My Pi, with no hook-trust bypass and
  never `codex exec`, in a folder that is a git repository, with no approval policy, sandbox mode
  or folder trust of the leg's own (its `config.toml` names the stand-in model's provider alone),
  under `env -i`, that HOME, the `CODEX_HOME` and the outside person's settings alone: the main run
  (C1-C5), a run with `ENVOY_URL` at `http://127.0.0.1:1`, a port no process but root's can listen
  on, read to refuse a connection before and after the run (C6), and a run with `DISPATCH_URL` at
  `OUTSIDE_INSIDE_DISPATCH_URL` (C2's control). It reads each screen and answers those the outside
  person is told how to answer: "Trust this folder?" (the folder's first start) with "Trust and
  continue", "Hooks need review" with "Trust all and continue", "Meet GPT-6 Sol" with "Use
  existing model", and, at the model's first `envoy_send`, `Allow the envoy MCP server to run tool
  "envoy_send"?` with "Always allow". It moves the menu one entry at a time toward the entry that
  reads so, each move once the screen shows the last one, takes it, and waits for the screen to
  go. Codex keeps each answer in `config.toml`, so no later run asks it again. Any other screen
  that holds the start for `OUTSIDE_CODEX_START_SECONDS` (default 90) makes the run's rows
  UNKNOWN, naming the screen's text. `OUTSIDE_CODEX_FIRST_START=no-trust` or
  `new-model` is a control run that answers "Continue without trusting" or "Try new model" instead:
  with the hooks untrusted no session record, registration or delivery comes, so C1, C4 and C5 do
  not pass; with the new model the session lists no plugin tool, so C2 and C3 do not pass. Each
  row's evidence names the screens and the answers, and a control run. Each run is stopped once
  the model's log holds the lines its rows read (the main run's `sent` or `wait-ended`, the other
  two runs' `whoami`), or once its turn lists no plugin tool, within its bound; no other line ends
  it;
- runs the installed release's own entry from a terminal, `bun <root>/dist/dispatch.js whoami`,
  with the host `codex` and a terminal session of its own, under that HOME and those settings (C7).

Codex runs against `outside-agent-codex-model.ts`, a keyless stand-in model on 127.0.0.1 that
speaks the Responses API and plays a scripted turn, so no model credential goes on the runner. It
binds a port of the kernel's choosing and prints it, and the leg points Codex at it only once it
answers `/_ready` with the run's nonce: a line that names another run's stand-in is never used.
A server at the port the model's whole line names that answers another word ends the wait at once,
every C row UNKNOWN; with no answer, the leg waits at most 30 s.
Codex 0.171.0 with `gpt-6.2-sol` runs no tool at all; with `-m gpt-5.5` it lists the plugin's tools
behind `tool_search`, and the stand-in finds them there. `--no-daemon` runs the session in its own
process, outside Codex's shared app server, where the plugin registers no session. In the main run
the model calls `dispatch whoami`, then `dispatch comment` on the scratch issue (the tailnet leg's,
found by its title), then `sleep 2` until a request carries the probe's message (at most 60 times),
then `envoy_send` to the probe's receiver. It logs each answer, and the leg judges the log.

Each row's `got` ends with what ran: `codex --version`, the npm package and version, the model
name, and the plugin's version, its integrity at the registry and the root Codex installed it at.

| Row | It passes when |
| --- | --- |
| C0 | The release's provenance holds, and Codex installed the attested tarball. At npm's registry (`https://registry.npmjs.org`; the leg names no other to any npm call), the release has a SLSA provenance attestation, which `npm publish --provenance` writes, naming `https://github.com/cybermonklabs/legion`'s `.github/workflows/release.yaml`, the ref `refs/heads/main` and `OUTSIDE_CODEX_ENVOY_COMMIT`, and attesting the tarball whose integrity the registry names. The attestation's signing certificate names the same repository, workflow, ref and commit in its Fulcio extensions as its payload does: `npm audit signatures` verifies that certificate's chain and the signature, and the leg holds the payload to the certificate. The tarball the registry serves is the bytes that integrity names, and every file of the tree Codex installed is that tarball's, with none more or fewer, a link holding the same target and no entry another type; `npm audit signatures`, in a project of its own that holds the release alone, verifies that attestation; and the commit holds `OUTSIDE_DEVICE_LOGIN_COMMIT` (`git merge-base --is-ancestor`). A release with no provenance (one published by hand), another workflow, ref, commit or tarball, a certificate whose extensions differ from the payload or lack one of them, an installed tree that differs from the tarball (the row names each differing file), or a commit without the device login reads FAIL; a certificate that cannot be read reads UNKNOWN. |
| C1 | The session id the plugin's SessionStart hook recorded for the Codex process is on the edge's session list within 30 s (`OUTSIDE_CODEX_LIST_SECONDS`) while Codex runs. Not listed in that time while it runs reads FAIL, and the run is stopped. The hook runs as the session starts, before the turn's first request, so the leg waits for the record until the turn's next request (`whoami` or `end` in the model's log), at most that window: no record by then reads UNKNOWN, naming it. |
| C2 | `dispatch whoami` through the plugin's `dispatch` tool names the Codex session, the test person as owner and the scratch project, and no other session for the credential; and the same call with `DISPATCH_URL` at `OUTSIDE_INSIDE_DISPATCH_URL` is refused: the command itself exits 1 or more. The plugin refusing its own configuration (`dispatch config: ...`, as a release without the device login refuses `AGENT_SECRETS_MACHINE_IDENTITY=device-login`) reads FAIL. Without the input, or where the control's call answers (exit 0) or ends with no exit of the command (a timeout, a cancel or a kill, which the model's log reads as -1), C2 reads UNKNOWN: its PASS would not tell the edge from the inside origin. The command's whoami prints no scopes; P3 reads them on the same login's token. |
| C3 | `dispatch comment` through the tool on the scratch issue exits 0, and the probe reads that comment on the issue with its author as Dispatch records a broker-issued token's write: kind `session`, its id the Codex session, proven by the credential, the test person as owner and the scratch project as project. Each must equal what the leg expects; an absent field never matches. The outside person is a restricted member, whose machine-login trade is named with the one project, so the project is on the author; against a broker that mints a restricted member's token as acting for the person, with no project, C3 reads FAIL by design. A refusal (exit 1), or exit 0 with no such comment or another author, reads FAIL; with no session record from Codex there is no session to hold the author to, and C3 reads UNKNOWN. |
| C4 | The probe sends the session `outside-e2e codex-in <nonce>` through the edge, and a model request holds it within 20 s: the plugin's PostToolUse hook puts it there. The control: before that the probe sends `<peer>-foreign`, the session outside the project of row P6, another message, which the edge answers 404 and which no request ever holds. That message reaching the session reads FAIL, and so does the session's own message never reaching it; a control send answered otherwise than 404 reads UNKNOWN. The tailnet leg holds C4's PASS as it holds P6's: where that session outside the project was not set up (its `foreign_problem`), C4 reads UNKNOWN. |
| C5 | The model's `envoy_send` reaches the probe's receiver, a session of the project that opened its relay socket first, within 10 s, sent as the Codex session. The 10 s count from the model's send, or from the leg's "Always allow" at Codex's approval prompt where that came later, as the send waits for the outside person there. An error from the tool, nothing received, or another sender reads FAIL. |
| C6 | With `ENVOY_URL` at `http://127.0.0.1:1`, which refuses a connection before and after the run, `dispatch whoami` through the tool still names the test person and the project: the `dispatch` tool works while Envoy cannot be reached. It is read only once C2's whoami passed; a port that did not refuse reads UNKNOWN. |
| C7 | The installed release's own entry, `bun <root>/dist/dispatch.js whoami`, run from a terminal under P2's login with the outside person's settings, the host `codex` and a terminal session of its own, names the test person and the scratch project: the entry the outside person's install text logs in with. A release with no such entry, one whose entry refuses its configuration, or one that names another person or project reads FAIL. |

## The tailnet leg's rows

| Row | It passes when |
| --- | --- |
| pins | `OUTSIDE_ENROLL_REFUSAL_COMMIT` and each word of `OUTSIDE_DISPATCH_COMMITS` are whole 40-character commit ids, and the list names at least one (else UNKNOWN). The broker's image was built from a commit that holds `OUTSIDE_ENROLL_REFUSAL_COMMIT`; Dispatch's `/healthz` names its pin's commit as both `commit` and `fleet_commit`; and that build holds each commit of `OUTSIDE_DISPATCH_COMMITS`. A build that lacks one reads FAIL; a Dispatch that serves another build than its pin, or is half rolled out, reads UNKNOWN; either way no leg starts. |
| issue | An issue of the project names `OUTSIDE_REPOSITORY`. |
| T1 | The scratch helper's machine login completes. Its machine file holds a credential, and `agent-secrets machine list` names the test person as its approver. `agent-secrets whoami` cannot show this: it reads an enrolled session, and T2 shows that the test person's login enrolls none. A login that ends `expired`, a code nobody approved, reads UNKNOWN: the public leg never approved it, which is a missing step, not a crossing. |
| T2 | `agent-secrets register` through the scratch helper is refused `403 RESTRICTED_MEMBER_ENROLLS_NOTHING`, by that code and no other, and the canary asked for through it gets no value, its session refused by that same code. A canary that gets no value for another reason, such as a requesters list on the secret that names someone else, reads FAIL: it says nothing of the member kind. |
| T3 | A fresh session enrolled through the full member's helper, whose broker is `OUTSIDE_TAILNET_BROKER_URL`, the broker T2's helper enrolls at, is granted the canary at once, and its value is 64 characters long. This is T2's positive control: the same broker grants the canary to a full member's session, so a refusal in T2 is the member kind's. Without the full member's helper (its login not approved by the full member's browser), T3 reads UNKNOWN. |
| T4 | The public leg's revoke step, before the T5 window, revokes the scratch login: its `revoke-<credential id>` row, the only row read for it, passes. `agent-secrets machine list` through the scratch helper is then refused `LAUNCHER_INVALID`, a reading beside the row and no proof of the revoke alone, since an expired or skewed proof is refused so too. A revoke row whose verdict is none of PASS, FAIL and UNKNOWN reads UNKNOWN. |
| T5 | The owner's removal, made during the public run's T5 window between the two clocks the leg read around it; before it, the test person is on the project's access list (the control, read with the full member's browser session at `OUTSIDE_TAILNET_DISPATCH_URL`), and after the run the list no longer names them (else UNKNOWN). From the public leg's rows, each part stopped at or after the first clock: the cookie got 401 within 30 s of it, and the passkey sign-in, a fresh trade of the person's device login, and the relay socket open then each stopped within 325 s of it. A part that stopped more than its bound after the second clock reads FAIL, and so does a trade answered more than 325 s after the second clock (`last_answered_at`), refused trade or not, once the list confirms the removal; a part between the two bounds reads UNKNOWN, as does one that stopped before the first clock. The socket part counts only for a socket opened before the first clock. A restricted member's token lasts at most 300 s and the edge checks an open socket every 20 s plus a 5 s cache. No terminal, no peer to open the window to, or a part not read, reads UNKNOWN. |
| public | The public leg's own `verdict` row and `revocations` row pass, and it ended well: exit 0 for a local command, or the run's conclusion `success`. A run concludes `failure` for an exit 1 and an exit 2 alike, so its verdict row says which. A public leg that failed, or left no verdict, never lets the gate pass. |
| sweep | `restricted-sweep.sh` on the runner exits 0 (read from the run's `restricted-sweep` artifact), and two controls of this box hold: at least one of the writer's writes was delivered to the full member's own feed, and began after the sweep's `live window` opened and ended before it closed, so its finding that nothing reached the test person's feed means something; and the proxy control held (a `can_fail` line in the fills for each of the five proxy reads, and the full member answered 200 by the proxy, with their browser session where `OUTSIDE_FULL_COOKIE_FILE` is given and with their token where it is not), so the runner's finding that the GitHub proxy answers the test person as for a repository the App is not installed on means something. Without either it reads UNKNOWN, as it does, naming the refusal, where a build refuses the token; a FAIL of the sweep stands. |
| pins-end | The pins read again at the end, from `OUTSIDE_PINS_REPO`'s main, are those read at the start, and Dispatch still serves the build it served at the start, on every process. A pin that moved between the legs, or a Dispatch build that changed, reads UNKNOWN: the rows ran against builds the start did not check. Without `OUTSIDE_PINS_REPO` it reads UNKNOWN. |
| gate | Every row above passes. |

Each leg reads every row of a TSV another program wrote, the public leg's, the browser's T5 rows
and the revocations, through one `worst_row`: where the TSV holds a row more than once, the worst
copy, FAIL, then any word but PASS, then PASS, so a later PASS never hides an earlier FAIL. The
tailnet leg holds a public row to what it saw beside it. P4 reads UNKNOWN when the peer did not
start. A PASS of P4 also needs the peer's own side: the peer was shown the ping and the reply within
10 s of their stamps, read from its transcript (or the scripted peer's log); not seen there, it
reads UNKNOWN, and so does a lag below -2 s on either side, a message shown before it was sent,
which says the two clocks disagree.
A PASS of P5 reads UNKNOWN when the issue row did not pass. A PASS of P6 reads
UNKNOWN when the peer did not start, the private listener did not list `<peer>-foreign`, or the
edge admits that session's token, which puts it in the project. A PASS of P7 reads UNKNOWN when
P7's control was not answered 201 and 200: no broker token a machine holds approves, so the 403
the agent's token gets says something about that token only where a browser session is answered on
the same routes.
A FAIL of P5, P6 or P7 stands, because a missing refusal or a listed foreign session is evidence by
itself, with one exception: where the edge admits `<peer>-foreign`'s token, P6's parts about that
session (its listing and the send to it) are no evidence, since it is then in the project, and a
P6 FAIL of those parts alone reads UNKNOWN. A P6 FAIL whose relay `sub` was granted, or refused with
another code, stands whatever the foreign session did.

## The browser gate

`OUTSIDE_GATE=browser` runs the browser leg alone: what a person from outside gets in their
browser, proved while nothing of the agents' path serves (no edge listener, no broker route at the
edge, no broker project check). It is the same command, `outside-agent-tailnet.sh <project>`;
`OUTSIDE_GATE=all`, the default, is both legs as above, and any other value reads every row UNKNOWN
and exits 2.

The project's owner acts through four commands. Each is a command line, split on blanks, that holds
its own owner credential, which the leg never sees:

| Variable | Its last words | It must |
| --- | --- | --- |
| `OUTSIDE_OWNER_ADD_COMMAND` | the address, the project | put the address on the project's access list, and exit 0. |
| `OUTSIDE_OWNER_LIST_COMMAND` | the project | print the addresses on the project's access list, one a line, and exit 0. |
| `OUTSIDE_OWNER_INVITE_COMMAND` | the address, the project | print the address's invitation link alone and exit 0; or print Dispatch's refusal, `<status> <code>`, on its first line and exit 1; or exit 2 when it could not tell. |
| `OUTSIDE_OWNER_REMOVE_COMMAND` | the address, the project | take the address off the project's access list, and exit 0. |

A command left unset is made by the leg itself with the owner's own browser session,
`OUTSIDE_FULL_COOKIE_FILE` (the project's owner holds the person and approve scopes no agent token
holds today), at `OUTSIDE_TAILNET_DISPATCH_URL`: `PUT` and `DELETE
/api/v1/projects/<key>/access/people/<address>`, `GET /api/v1/projects/<key>/access` and `POST
.../people/<address>/invitations`, each read as the command's contract says. Dispatch refuses a
cookie's write whose `Origin` is not the origin called, 403 `ORIGIN_REFUSED`, which a row
then names; a refusal with no code is quoted by its words. With neither the command nor the
session, the rows that need the act read UNKNOWN, naming both.

The leg quotes no word of what they print but a refusal's `<status> <code>`, since a link is a
secret, and asks no terminal. In this gate it does these steps:

1. It reads the pins of the broker and Dispatch alone, as row `pins` does above; the pins file's
   `edge-listener` line is not read. No run starts unless the row passes.
2. It finds or makes the scratch project's issue (row `issue`).
3. It starts no scratch helper, no peer, no session outside the project and no full member's
   helper, so it runs no T1-T4 and needs none of `OUTSIDE_TAILNET_BROKER_URL`,
   `OUTSIDE_LISTENER_URL`, `OUTSIDE_INTERNAL_LISTENER_URL`, `OUTSIDE_FOREIGN_PROJECT` and
   `OUTSIDE_CANARY`. It runs the sweep's full-member half, its writer and its proxy control, as
   step 5 above says, the proxy reads with `OUTSIDE_FULL_COOKIE_FILE` where it is given and with the
   full member's token where it is not; a build that refuses the token holds the sweep row at
   UNKNOWN, naming the refusal.
4. The owner puts the test person on the access list, the list command reads them on it (its
   control), and the owner's invite makes their link. Then row `unlisted`: the owner's invite for
   an address on no list, `OUTSIDE_UNLISTED_PERSON` (default
   `outside-e2e-unlisted-<random>@example.com`).
5. Seconds before it starts the workflow, it reads the full member's list of credential grants
   (`GET /api/v1/credential-grants?approver=me`, with `OUTSIDE_SWEEP_FULL_TOKEN_FILE`) and picks the
   probe grant: the first grant with no approver and no operator, whose approver and operator name
   neither the full member (their token's whoami) nor the test person. No person may end such a
   grant at the broker, whatever Dispatch relays, so the probe ends nothing.
   `outside-agent-grants.sh`, run alone (`bash outside-agent-grants.sh`, with
   `OUTSIDE_SWEEP_FULL_TOKEN_FILE`, `OUTSIDE_TAILNET_DISPATCH_URL` and `OUTSIDE_PERSON`), is the
   same read as a preflight before a live gate is asked for: it prints `grant <id>` (exit 0), `no
   grant with no approver and no operator` (exit 1), or `UNKNOWN: <why>` (exit 2).
6. It starts the workflow as step 6 above says, with `checks=browser`, `project`,
   `routed_controls=dispatch-healthz`, the link as `invite_url`, the issue's key as
   `sweep_member_issue` (the issue the person's own comment and issue message go on, the control of
   the sweep's member feed on the runner), the probe grant as `grants_probe_id`, `sweep_project`,
   `sweep_fills`, and each line of `OUTSIDE_WORKFLOW_INPUTS` but an `invite_url` one, which it leaves
   out. With no link it starts no run. It refuses `OUTSIDE_PUBLIC_COMMAND`, since only the
   workflow's step status opens its window.
7. It reads the run every 10 s (`gh run view --json status,jobs`) until its step
   `The delisting window` reads `in_progress`; then it reads the person on the access list with the
   list command (the control) and runs the owner's remove command, its clock read before and after,
   as T5's removal above does. A step read `completed` or `skipped` before it was read running (the
   step ends at once when the driver kept no cookie), a run that ended first, or an hour with no such
   read is T5's UNKNOWN, and no removal is made.
8. It waits for the run's end, downloads its artifacts `outside-edge-refusals`,
   `outside-path-reach`, `outside-edge`, `outside-edge-grants`, `restricted-sweep` and
   `outside-edge-t5`, and reads the full member's list of credential grants again.

It prints the rows pins, issue, unlisted, T5, refusals, reach, sign-in, projects, limits, grants,
public, sweep, pins-end and the gate:

| Row | It passes when |
| --- | --- |
| unlisted | The owner's invite for the unlisted address prints `409 PERSON_NOT_LISTED` and exits 1, and the same command made the test person's link, its positive control. A link made reads FAIL, and the row prints no link; any other refusal (a 403, a 404, another code) or exit reads UNKNOWN. |
| T5 | As the tailnet leg's T5 above, from the rows `T5-cookie` (401 within 30 s of the first clock) and `T5-passkey` (refused within 325 s) of `outside-edge-t5.tsv` alone: on this path the person has no device login and no socket. The owner's list command names the person before the removal and not after the run. |
| refusals | The run's `outside-edge-refusals.code`, the exit its Verdict step reads, is 0; 1 reads FAIL, and 2, none or another word UNKNOWN. Its `got` is the last line the check printed. |
| reach | The same, of `outside-path-reach`. |
| sign-in, projects | The driver's rows of those names in `outside-edge.tsv`, read through `worst_row`. |
| limits | Every `limit-*` row of `outside-edge.tsv` passes, and there is at least one. One FAIL reads FAIL; the driver's `limits` row of no request counted, or no limit row, reads UNKNOWN. |
| grants | The driver's rows `grants-list` (the test person's own list holds only their own sessions' grants) and `grants-revoke` (their revoke of the probe grant is refused by Dispatch itself, 403 `RESTRICTED_MEMBER`) in `outside-edge-grants.tsv` both pass, a probe grant was given, and the full member's list holds a grant every signed-in person is listed (one the model judge approved for `anyone`, or one a pod that acts for no person got without asking) both before and after the run, so one was live while the runner read. Either driver row's FAIL reads FAIL with or without that control; no `outside-edge-grants.tsv`, no probe grant, or no such grant in both reads reads UNKNOWN. It FAILs until Dispatch holds a restricted member to their own sessions' grants. |
| public | The run's conclusion is `success`. A `failure` reads FAIL where a check's exit is 1, and UNKNOWN otherwise; its `got` names each check's exit. |

Rows pins, issue, sweep and pins-end read as above. A removal ends the person's account, so each
run's owner add puts them back on the list and its invite makes a new link.

## The controls, run first and recorded

Run each control on `packages/envoy/scripts/dev-stack.sh`, which runs the real broker, Dispatch
and listener on one machine, with `OUTSIDE_ALLOW_HTTP=1` and `OUTSIDE_PUBLIC_COMMAND`:

- With the listener's edge mode off, rows P5-P8 and P10 do not pass: the listener admits every
  interest and publish and lists every session.
- With edge mode on and a repository answer of `[]`, P5's admitted interest is refused.
- With Dispatch built to admit a broker token on the approve route, P7 reads FAIL: the approve the
  agent's token then reaches answers 404 for the record it names, never 403. P7's positive control
  is the tailnet leg's own and runs in every run: the full member's browser is answered 201 by the
  mint and 200 by the approve.
- With the broker built without the enrollment refusal, T2 fails and T3 still passes.

From `packages/dispatch`,
`bun test e2e/__tests__/outside-agent-acceptance.test.ts e2e/__tests__/outside-agent-public.test.ts`
runs each leg with no input, and with every input set and nothing behind them, and wants UNKNOWN
for every row and exit 2. Its control runs a copy of each leg in which every `UNKNOWN` reads
`PASS`, and the same check must fail on that copy. It runs every background shape the legs start a
child in, and `reap` stops each, beside two shapes the legs no longer use, which `reap` cannot
stop; and a process on the leg's list that is no child of the leg, as a reused pid would be, which
`stop_child` and `reap` must leave running. `await_child` ends at its bound for a child that never
ends (the control), and at the child's end, with its exit status, for one that ends far inside its
bound.

It runs the public leg against stand-ins of the plugin, the device login and the probe: P6, P7 and
P10 pass on the edge's own refusals and fail on an accepted send or another refusal code, and T5's
agent part passes on refused trades and a 4401 close, fails on a trade answered after the refusal,
and reads UNKNOWN for a later trade with no readable answer, a removal before any socket opened
and a 1006 close.

The stand-in Dispatch's port is its own: under a squat that takes over any port a listener of the
test process closes, a prepared run's origin still serves that run's stand-in, and the squat
takes nothing (its control: the squat does take a port a listener closed there).

It runs the tailnet leg against stand-ins of the broker's helper, Dispatch, the deployment
repository, the workflow, the sweep and the public leg: with the full member's browser answered,
T2, T3 (enrolled through the full member's helper), P7, the public row and the sweep pass, and the
leg stops the sweep's writer itself; with it refused, P7 and the sweep read UNKNOWN, and a canary
refused for a reason other than the member kind makes T2 FAIL; a login nobody approved makes T1
UNKNOWN; a revoke row with no verdict makes T4 UNKNOWN and is counted by the gate; a trade
answered past its bound after a removal the list confirms makes T5 FAIL, and one within it
UNKNOWN; a public leg whose verdict row fails makes the public row FAIL, and one with no verdict
row UNKNOWN; P6's granted sub stands as a FAIL; a Dispatch build that lacks a commit of
`OUTSIDE_DISPATCH_COMMITS` makes the pins row FAIL before any leg starts; a Dispatch that serves
another build than its pin or is half rolled out holds the gate, as do a pin that moves during the
run and local pin files (`pins-end`); and the workflow gets only the inputs its file declares, on
stdin, `sweep_project` among them, while an undeclared input is refused 422 and makes T1 UNKNOWN
at once.

It runs the browser gate against stand-ins of the owner commands, `dispatch` and the workflow's run,
whose step `The delisting window` the stand-in `gh` reports: every row passes and the gate exits 0,
with a pins file whose `edge-listener` line names a file the deployment repository lacks (the same
stand-ins under `OUTSIDE_GATE=all` read that pin, its control), with none of the agents' path's
inputs, no `agent-secrets` call and no full member's helper, and the workflow gets `checks=browser`,
`routed_controls=dispatch-healthz`, the owner's one link (not the link `OUTSIDE_WORKFLOW_INPUTS`
held), `sweep_member_issue` and the probe grant as `grants_probe_id`, on stdin, while no output holds
the link; the full member's list of credential grants is read before the run and after it. Each
plant holds its own row: an owner's invite refused `403 BROWSER_SESSION_REQUIRED` starts no run; an
unlisted address already listed gets a link, which fails row unlisted; a remove that takes nobody
off holds T5 at UNKNOWN; `OUTSIDE_PUBLIC_COMMAND` starts no run; a skipped window step makes no
removal; a cookie's 401 a minute after the removal fails T5; a check's exit 1 fails its row and the
public row; a check with no `.code` reads UNKNOWN; a limit row's FAIL, or no limit row, holds
limits; with no `OUTSIDE_FULL_COOKIE_FILE` the sweep's proxy reads use the full member's token and
every row passes, while a build that refuses the token holds the sweep row and the gate at UNKNOWN,
naming the refusal, its fills going to the workflow with no `can_fail` line (the tailnet leg's own
row holds the same in both legs' gate); either driver row's FAIL fails grants, with or without its
control, and an empty control, no grant with no approver and no operator (no `grants_probe_id` is
sent), the driver's own guard and no `outside-edge-grants.tsv` each read UNKNOWN; with no owner
command every owner act is made with the owner's browser session and every row passes, while the
same acts sent another `Origin` are refused `403 ORIGIN_REFUSED`, which rows unlisted and
public name, and with neither a command nor the session the owner rows read UNKNOWN naming both. Run
alone, `outside-agent-grants.sh` prints the probe grant, `no grant with no approver and no operator`
(exit 1), or UNKNOWN without the token (exit 2). With no input the browser gate reads each of its
rows UNKNOWN, and `OUTSIDE_GATE` of another word every row.

## What the two legs do not prove

- The broker's policy denial behind an enrollment is reached only by its unit test: a refused
  enrollment leaves no session to ask with.
- P8's 300 s bound is read on the token that the trade answered, not on a revoke that a person
  makes.
- P4's 10 s bounds compare the peer's clock with the runner's, so they hold only as well as both
  machines keep time. The public side times a card from its envelope `at`, to the second.
- T5 compares the runner's clock, which stamps the public leg's T5 rows, with this box's, which
  reads the removal's two clocks, so its bounds hold only as well as both machines keep time.
- The broker's and the edge listener's `/healthz` name no build, so their builds are taken to be
  their pins': the check cannot see either running another build than the deployment repository's
  configuration names. Dispatch's serving build is read, at the start and at the end, and held to
  its pin.
- The browser gate proves nothing of the agents' path: the device login, the relay socket, rows
  P1-P10 and T1-T4, and T5's trade and socket parts are the agents' gate's, `OUTSIDE_GATE=all`. Its
  row unlisted reads the owner's invite route; that an address taken off the list cannot sign in
  again is its T5's passkey part.
- Row grants reads which grants every signed-in person is listed from the fields the full member's
  list shows. A person's pod that the broker no longer maps to them shows the same fields (no
  approver, no operator) on that person's list without being on everyone's, so where the full
  member is such a pod's former person, that grant counts in the control though the test person
  would not be listed it. Its revoke probe ends nothing only while the probe grant names no approver
  and no operator; the box reads that seconds before the run, and the driver again from its own
  list just before the revoke.
