# The outside agent's acceptance

This check proves what a person from outside the deployment's sign-in domain gets when their
agents work with ours: their agent, on their own machine and on the public internet, reaches
Dispatch, the Envoy listener and the secrets broker only through the three edge origins, is held
to the one project an access list names them on, and gets no secret. It has two legs and one gate.
The gate must pass before the deployment names a real person from outside on any access list.

| File | What it is |
| --- | --- |
| `outside-agent-tailnet.sh` | The one command. It runs on a developer box on the private network, runs rows T1-T4 there, starts the public leg and prints the gate. |
| `outside-agent-public.sh` | The public leg, rows P1-P9. A workflow on a runner on the public internet runs it, after that workflow's browser signs the test person in. |
| `outside-agent-lib.sh` | What both legs share, sourced from beside them: input and tool checks, the text and JSON reads, the real-tool links, and `reap`, which stops and waits for every background child a leg started. |
| `outside-agent-peer.sh` | The tailnet leg's peer of row P4 and its session outside the project of row P6. |
| `outside-agent-sweep.sh` | The tailnet leg's half of the read sweep and its two controls. |
| `outside-agent-probe.ts` | The calls that carry a broker credential (DPoP), which a shell cannot sign, the scripted peer, and the public session's keyless model. Both legs run it with `bun`. |
| `outside-path-reach.sh` | The TCP reach check that row P9 runs. |
| `../__tests__/outside-agent-acceptance.test.ts` | The test that each leg prints UNKNOWN, never PASS, for a row whose target is absent, with its control, and that each leg's own judgments hold against stand-ins. |

## What each leg prints

Each leg prints a TSV on stdout: the header `row want got verdict`, then one line per row. A row
reads `PASS`, `FAIL` or `UNKNOWN`. A row that could not run reads `UNKNOWN`, and its `got` names
what the leg could not read: an input that is not set, a program that is not on PATH, a server
that did not answer, or a row it needs that did not pass. A row never reads `PASS` because it did
not run, and a verdict that is none of the three, copied from another file, counts as `UNKNOWN`.
Each leg exits 0 when every row passed, 1 when a row failed, and 2 otherwise.

The public leg ends with a `verdict` row. The tailnet leg prints its own rows, then rows P1-P9 as
the public leg's TSV has them, then a `public` row (the public leg's own verdict, revocations and
end), the sweep and a `gate` row.

No row prints a token, a proof or a secret value. Rows T2 and T3 read the canary secret's length
alone. A row quotes no output of an operator's command (the approve, revoke and browser commands,
the plugin install), which may print a cookie, a code or an invite token: only its exit status and
a fixed phrase.

## How to run it

On a developer box, from a checkout of this repository with `bun install` done and
`packages/pi-envoy` built:

```bash
OUTSIDE_PINS=pins.txt \
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
OUTSIDE_WORKFLOW_REPO=example-owner/example-deploy \
OUTSIDE_WORKFLOW_INPUTS='edge_host=dispatch.edge.example' \
OUTSIDE_TAILNET_TSV=tailnet.tsv \
  bash packages/dispatch/e2e/acceptance/outside-agent-tailnet.sh EXAMPLE
```

`pins.txt` holds one `name file key` line for each of `broker`, `dispatch` and `edge-listener`:
the deployment's configuration file, at its main branch, and the key whose value is that
deployment's image. The leg reads the commit each image was built from with `crane config`
(`org.opencontainers.image.revision`), or with `OUTSIDE_IMAGE_REVISION_COMMAND`. It reads the pins
once, at the start: no deployment serves the commit it runs, so a second read could only read the
same files again.

The workflow's inputs reach `gh workflow run --json` on its stdin, never its command line, which
any account on the box can read. A workflow input is kept with the run, and anyone who can read
the run can read it, so no input may be a secret: the invite link reaches the workflow as its own
secret, not through `OUTSIDE_WORKFLOW_INPUTS`.

Each leg's header lists every input it reads. The tailnet leg's optional inputs are
`OUTSIDE_PEER`, `OUTSIDE_PEER_SESSION`, `OUTSIDE_PEER_PLUGIN_DIR`, `OUTSIDE_PEER_AGENT_DIR`,
`OUTSIDE_PEER_AGENT_FILES`, `OUTSIDE_PEER_ENV`, `OUTSIDE_IMAGE_REVISION_COMMAND`,
`OUTSIDE_TAILNET_TSV`, `OUTSIDE_RESULT_ISSUE` (an issue that gets the result as one comment),
`OUTSIDE_LEGION_CHECKOUT` and `OUTSIDE_ALLOW_HTTP`.

`OUTSIDE_FULL_COOKIE_FILE` holds a full member's browser session at Dispatch's private origin,
`<name>=<value>` as the Cookie header carries it. The leg gives it to curl in a header file of mode
0600 alone. No broker token approves, and Dispatch's GitHub proxy reads as the browser's person and
never as a token, so the two positive controls below read with it.

The tailnet leg does these steps, in this order:

1. It reads the pins and prints them. It does not start unless the broker's build holds the
   enrollment refusal and Dispatch's build holds each commit of `OUTSIDE_DISPATCH_COMMITS`: no
   broker token gets the approve scope, which P7's refusal and its control rest on, and no
   restricted member names an issue's repository, which decides the repositories P5 and the
   sweep's GitHub proxy rows read (`git merge-base --is-ancestor`).
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
5. It runs the read sweep's full-member half: `restricted-sweep.sh` (beside this file) in
   its fills mode with the box's full member (`OUTSIDE_SWEEP_PROJECT`,
   `OUTSIDE_SWEEP_FULL_TOKEN_FILE`, `OUTSIDE_SWEEP_CONVERSATION_FILE`, the test person as
   `SWEEP_MEMBER`, and the full member's cookie as `SWEEP_FULL_COOKIE`, with which it reads the
   GitHub proxy; without `OUTSIDE_FULL_COOKIE_FILE` the sweep does not run and its row reads
   UNKNOWN), and starts its writer, which comments on the scratch issue
   `OUTSIDE_SWEEP_WRITE_ISSUE` every 15 s until the public leg ends, when the leg stops it. No full
   member's credential goes to the runner: the fills file holds none, and is at most 40 KiB so that
   it fits one input. Then the sweep's proxy control. The fills must hold a `can_fail` line, the
   full member's answer that can fail, for each GitHub proxy read the runner makes:
   `GET /api/github/rest/repos/{owner}/{repo}/` with `pulls/{number}`, `issues/{number}`,
   `commits/{sha}/check-runs` and `actions/runs?head_sha={sha}`, and
   `GET /api/v1/github/app-installation/{owner}/{repo}`. And the full member's browser reads,
   through the GitHub proxy, the workflow runs of one commit of the repository the fills name
   (the one only the sweep's project's issues resolve to, which the runner's proxy rows read), and
   must get 200 with a list of workflow runs.
6. It starts the public leg, within the code's 15 minutes: `gh workflow run --json` of
   `OUTSIDE_WORKFLOW` (default `outside-edge-e2e.yaml`) in `OUTSIDE_WORKFLOW_REPO`, its inputs
   `checks=all`, `project`, `peer_session`, `tailnet_user_code`, `sweep_fills` (the fills file,
   base64, when fills mode made one) and `OUTSIDE_WORKFLOW_INPUTS` on stdin. With
   `OUTSIDE_PUBLIC_COMMAND` set, it runs that command here instead, for a local stack.
7. It waits for the scratch login to be approved, and runs rows T1-T3, T3 against the broker
   `OUTSIDE_TAILNET_BROKER_URL` names. Then P7's control: the full member's browser mints a
   personal agent token, which must answer 201 and is revoked at once, and approves the machine
   login of a second scratch helper whose operator is that member, which must answer 200 and is
   revoked at once.
8. During the public run's T5 window it runs T5's removal. Once the peer's transcript holds
   `outside-e2e t5-open <nonce>`, it reads the test person on the access list (the control), asks
   the owner on this terminal to remove them and waits for Enter; with
   `OUTSIDE_OWNER_REMOVE_COMMAND` set, it runs that command instead, as a harness acting as the
   owner's browser does on a local stack. It reads its clock before it asks and after the owner
   answers: the removal was made between the two. Re-adding the person is the owner's own act.
9. It waits for the public run to end, downloads its artifacts `outside-agent-public`,
   `outside-edge-revocations` and `restricted-sweep`, stops the sweep's writer, and runs row T4.
10. It judges T5 from the public leg's T5 rows and the removal's two clocks, prints rows P1-P9,
    the public leg's own last word, the sweep and the gate.
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
| `OUTSIDE_REVOKE_COMMAND` | The workflow's revoke step, a command line the leg runs after P9 and before the T5 window, while the person's cookie still reads their own list. It gets `OUTSIDE_EDGE_REVOKE_KEEP`, P2's user code, and revokes every other device login the run approved, the scratch helper's included, writing the `revoke-<credential id>` rows of `outside-edge-revocations`. Exit 0, 1 or 2 is the leg's `revocations` row. P2's login stays live into the window, where the removal stops it; its key lives only in the leg's scratch HOME, which the leg removes. |

No model credential goes on the runner: the leg's two sends in P4 are scripted, and the model turn
is the peer's, on the developer box. The public Oh My Pi session still runs on a model, because an
Envoy card is handed to it as a turn and a session with no model refuses the card and holds
nothing: the leg runs it on `outside-agent-probe.ts model`, a keyless OpenAI-compatible endpoint
on 127.0.0.1 that answers every turn with one fixed line (`--model outside-e2e/scripted`) and
reads a request body of at most 4 MiB.

Programs on PATH: bash, curl, tmux, bun and `omp`. The step needs about 40 minutes: P5 waits up to
15 minutes for a GitHub event, beside P4 and P8, P8 waits for a token to expire (300 s), and the T5
window lasts up to 20 minutes.

After P9 the leg opens the T5 window. It tells the peer `outside-e2e t5-open <nonce> at <ms>`,
starts `OUTSIDE_T5_COMMAND`, trades P2's device login afresh every 10 s, and keeps a relay socket
open with the newest token, opening another when one closes, as the agent would. Once a trade is
refused, it watches the socket open then until it closes or 325 s pass, and goes on trading every
10 s meanwhile. It prints rows `T5-agent` (with `refused_at=`, `socket_opened_at=`,
`socket_closed_at=` in Unix seconds, the close `code=` and the count of `refusals=`), `T5-cookie`
and `T5-passkey`; the tailnet leg judges T5 from them.

Before the window, `OUTSIDE_REVOKE_COMMAND` revokes every device login the run approved but P2's,
the scratch helper's included, while the person's cookie still reads their own list. The workflow
uploads its rows, one per revoke, `revoke-<credential id>`, as artifact `outside-edge-revocations`.

## The public leg's rows

Every process the leg starts runs with `env -i`, a HOME of its own, and these settings alone:
`ENVOY_URL`, `ENVOY_INBOUND=listener`, `DISPATCH_URL`, `DISPATCH_TOKEN_BROKER=1`,
`AGENT_SECRETS_URL`, `AGENT_SECRETS_MACHINE_IDENTITY=device-login` and `AGENT_SECRETS_PROJECTS`,
beside `OMP_SKIP_SETUP=1`, Oh My Pi's own switch that keeps its first-run setup wizard shut in a
fresh HOME. The leg links the real `bun`, `omp` and `tmux` for them, because a version manager's
shim finds its program through the caller's HOME.

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
| T5-agent | After the first refused trade, no later trade is answered, and the socket open at that refusal closes with the edge's own code, 4401 or 4403. A trade answered later reads FAIL. With no socket open at the refusal (the removal came before one opened), or a close with another code, the socket part reads UNKNOWN. |

## The tailnet leg's rows

| Row | It passes when |
| --- | --- |
| pins | The broker's image was built from a commit that holds `OUTSIDE_ENROLL_REFUSAL_COMMIT`, and Dispatch's from one that holds each commit of `OUTSIDE_DISPATCH_COMMITS`. A pin that lacks one reads FAIL, and no leg starts. |
| issue | An issue of the project names `OUTSIDE_REPOSITORY`. |
| T1 | The scratch helper's machine login completes. Its machine file holds a credential, and `agent-secrets machine list` names the test person as its approver. `agent-secrets whoami` cannot show this: it reads an enrolled session, and T2 shows that the test person's login enrolls none. A login that ends `expired`, a code nobody approved, reads UNKNOWN: the public leg never approved it, which is a missing step, not a crossing. |
| T2 | `agent-secrets register` through the scratch helper is refused `403 RESTRICTED_MEMBER_ENROLLS_NOTHING`, by that code and no other, and the canary asked for through it gets no value, its session refused by that same code. A canary that gets no value for another reason, such as a requesters list on the secret that names someone else, reads FAIL: it says nothing of the member kind. |
| T3 | A fresh session enrolled through the box's own helper is granted the canary at once by the broker `OUTSIDE_TAILNET_BROKER_URL` names, and its value is 64 characters long. This is T2's positive control: the same broker grants the canary, so a refusal in T2 is the member kind's. |
| T4 | The public leg's revoke step, before the T5 window, revokes the scratch login (its `revoke-<credential id>` row passes), and `agent-secrets machine list` through the scratch helper is then refused `LAUNCHER_INVALID`. A revoke row whose verdict is none of PASS, FAIL and UNKNOWN reads UNKNOWN. |
| T5 | The owner's removal, made during the public run's T5 window between the two clocks the leg read around it; before it, the test person is on the project's access list (the control, read with the full member's browser session at `OUTSIDE_TAILNET_DISPATCH_URL`), and after the run the list no longer names them (else UNKNOWN). From the public leg's rows, each part stopped at or after the first clock: the cookie got 401 within 30 s of it, and the passkey sign-in, a fresh trade of the person's device login, and the relay socket open then each stopped within 325 s of it. A part that stopped more than its bound after the second clock reads FAIL; one between the two bounds reads UNKNOWN, as does one that stopped before the first clock. The socket part counts only for a socket opened before the first clock. A restricted member's token lasts at most 300 s and the edge checks an open socket every 20 s plus a 5 s cache. No terminal, no peer to open the window to, or a part not read, reads UNKNOWN. |
| public | The public leg's own `verdict` row and `revocations` row pass, and it ended well: exit 0 for a local command, or the run's conclusion `success`. A run concludes `failure` for an exit 1 and an exit 2 alike, so its verdict row says which. A public leg that failed, or left no verdict, never lets the gate pass. |
| sweep | `restricted-sweep.sh` on the runner exits 0 (read from the run's `restricted-sweep` artifact), and two controls of this box hold: at least one of the writer's comments was delivered to the full member's own feed strictly inside the sweep's `live window`, so its finding that nothing reached the test person's feed means something; and the proxy control held (a `can_fail` line in the fills for each of the five proxy reads, and the full member's browser answered 200 by the proxy), so the runner's finding that the GitHub proxy answers the test person as for a repository the App is not installed on means something. Without either, or without `OUTSIDE_FULL_COOKIE_FILE`, it reads UNKNOWN; a FAIL of the sweep stands. |
| gate | Every row above passes. |

The tailnet leg holds a public row to what it saw beside it. P4 reads UNKNOWN when the peer did not
start. A PASS of P4 also needs the peer's own side: the peer was shown the ping and the reply within
10 s of their stamps, read from its transcript (or the scripted peer's log); not seen there, it
reads UNKNOWN. A PASS of P5 reads UNKNOWN when the issue row did not pass. A PASS of P6 reads
UNKNOWN when the peer did not start, the private listener did not list `<peer>-foreign`, or the
edge admits that session's token, which puts it in the project. A PASS of P7 reads UNKNOWN when
P7's control was not answered 201 and 200: no broker token approves, so the 403 the agent's token
gets says something about that token only where a browser session is answered on the same routes.
A FAIL of P5, P6 or P7 stands, because a missing refusal or a listed foreign session is evidence by
itself, with one exception: where the edge admits `<peer>-foreign`'s token, P6's parts about that
session (its listing and the send to it) are no evidence, since it is then in the project, and a
P6 FAIL of those parts alone reads UNKNOWN. A P6 FAIL whose relay `sub` was granted, or refused with
another code, stands whatever the foreign session did.

## The controls, run first and recorded

Run each control on `packages/envoy/scripts/dev-stack.sh`, which runs the real broker, Dispatch
and listener on one machine, with `OUTSIDE_ALLOW_HTTP=1` and `OUTSIDE_PUBLIC_COMMAND`:

- With the listener's edge mode off, rows P5-P8 do not pass: the listener admits every interest
  and lists every session.
- With edge mode on and a repository answer of `[]`, P5's admitted interest is refused.
- With Dispatch built to admit a broker token on the approve route, P7 reads FAIL: the approve the
  agent's token then reaches answers 404 for the record it names, never 403. P7's positive control
  is the tailnet leg's own and runs in every run: the full member's browser is answered 201 by the
  mint and 200 by the approve.
- With the broker built without the enrollment refusal, T2 fails and T3 still passes.

`bun test e2e/__tests__/outside-agent-acceptance.test.ts`, from `packages/dispatch`, runs each leg
with no input, and with every input set and nothing behind them, and wants UNKNOWN for every row
and exit 2. Its control runs a copy of each leg in which every `UNKNOWN` reads `PASS`, and the
same check must fail on that copy. It runs every background shape the legs start a child in, and
`reap` stops each, beside two shapes the legs no longer use, which `reap` cannot stop. It runs the
public leg against stand-ins of the plugin, the device login and the probe: P6 and P7 pass on the
edge's own refusals and fail on an accepted send or another refusal code, and T5's agent part
passes on refused trades and a 4401 close, fails on a trade answered after the refusal, and reads
UNKNOWN for a removal before any socket opened and for a 1006 close. It runs the tailnet leg
against stand-ins of the broker's helper, Dispatch, the sweep and the public leg: with the full
member's browser answered, T2, P7, the public row and the sweep pass, and the leg stops the
sweep's writer itself; with it refused, P7 and the sweep read UNKNOWN, and a canary refused for a
reason other than the member kind makes T2 FAIL; a login nobody approved makes T1 UNKNOWN; a
revoke row with no verdict makes T4 UNKNOWN and is counted by the gate; a public leg whose verdict
row fails makes the public row FAIL, and one with no verdict row UNKNOWN; P6's granted sub stands
as a FAIL; and a Dispatch pin that lacks a commit of `OUTSIDE_DISPATCH_COMMITS` makes the pins row
FAIL before any leg starts.

## What the two legs do not prove

- The broker's policy denial behind an enrollment is reached only by its unit test: a refused
  enrollment leaves no session to ask with.
- P8's 300 s bound is read on the token that the trade answered, not on a revoke that a person
  makes.
- P4's 10 s bounds compare the peer's clock with the runner's, so they hold only as well as both
  machines keep time. The public side times a card from its envelope `at`, to the second.
- T5 compares the runner's clock, which stamps the public leg's T5 rows, with this box's, which
  reads the removal's two clocks, so its bounds hold only as well as both machines keep time.
- The pins are the deployment repository's configuration at its main, read once: no deployment
  serves the commit it runs, so the check cannot see a deployment that runs another build than its
  configuration names.
