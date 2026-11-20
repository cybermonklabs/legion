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
| `outside-agent-probe.ts` | The calls that carry a broker credential (DPoP), which a shell cannot sign, and the peer session. Both legs run it with `bun`. |
| `outside-path-reach.sh` | The TCP reach check that row P9 runs. |
| `../__tests__/outside-agent-acceptance.test.ts` | The test that each leg prints UNKNOWN, never PASS, for a row whose target is absent, with its control. |

## What each leg prints

Each leg prints a TSV on stdout: the header `row want got verdict`, then one line per row. A row
reads `PASS`, `FAIL` or `UNKNOWN`. A row that could not run reads `UNKNOWN`, and its `got` names
what the leg could not read: an input that is not set, a program that is not on PATH, a server
that did not answer, or a row it needs that did not pass. A row never reads `PASS` because it did
not run. Each leg exits 0 when every row passed, 1 when a row failed, and 2 otherwise.

The public leg ends with a `verdict` row. The tailnet leg prints its own rows, then rows P1-P9 as
the public leg's TSV has them, then a `gate` row.

No row prints a token, a proof or a secret value. Rows T2 and T3 read the canary secret's length
alone.

## How to run it

On a developer box, from a checkout of this repository with `bun install` done:

```bash
OUTSIDE_PINS=pins.txt \
OUTSIDE_ENROLL_REFUSAL_COMMIT=<the merge commit of the enrollment refusal> \
OUTSIDE_TAILNET_BROKER_URL=https://secrets.internal.example \
OUTSIDE_TAILNET_DISPATCH_URL=https://dispatch.internal.example \
OUTSIDE_LISTENER_URL=https://envoy.edge.example \
OUTSIDE_INTERNAL_LISTENER_URL=https://envoy.internal.example \
OUTSIDE_PERSON=outside-e2e@example.com \
OUTSIDE_CANARY=<the canary secret's name> \
OUTSIDE_REPOSITORY=example-owner/example-repo \
OUTSIDE_WORKFLOW_REPO=example-owner/example-deploy \
OUTSIDE_WORKFLOW_INPUTS='invite_url=https://dispatch.edge.example/invite/...' \
OUTSIDE_TAILNET_TSV=tailnet.tsv \
  bash packages/dispatch/e2e/acceptance/outside-agent-tailnet.sh EXAMPLE
```

`pins.txt` holds one `name file key` line for each of `broker`, `dispatch` and `edge-listener`:
the deployment's configuration file, at its main branch, and the key whose value is that
deployment's image. The leg reads the commit each image was built from with `crane config`
(`org.opencontainers.image.revision`), or with `OUTSIDE_IMAGE_REVISION_COMMAND`.

Each leg's header lists every input it reads. The tailnet leg's optional inputs are
`OUTSIDE_PEER_SESSION`, `OUTSIDE_IMAGE_REVISION_COMMAND`, `OUTSIDE_TAILNET_TSV`,
`OUTSIDE_RESULT_ISSUE` (an issue that gets the result as one comment) and `OUTSIDE_ALLOW_HTTP`.

The tailnet leg does these steps, in this order:

1. It reads the pins and prints them. It does not start unless the broker's build holds the
   enrollment refusal (`git merge-base --is-ancestor`).
2. It finds or makes the scratch project's issue that names `OUTSIDE_REPOSITORY`, with the box's
   own `dispatch` command. Row P5's admitted interest reads that repository.
3. It starts a scratch `agent-secrets-helper serve` in a `mktemp -d` directory, with its own HOME,
   XDG directories and `AGENT_SECRETS_*` paths, the test person in its operator file, and the
   broker's private origin as `AGENT_SECRETS_URL`. It runs `agent-secrets machine login` there and
   reads the code.
4. It starts the peer at the edge listener, with the box's own identity and a token that names the
   project. By default (`OUTSIDE_PEER=omp`) the peer is an Oh My Pi session in tmux, run with the
   box's own model profile and only the envoy plugin `OUTSIDE_PEER_PLUGIN_DIR` names (default this
   checkout's `packages/pi-envoy`, built), its sessions kept in the scratch directory; its model
   answers the public side. On a local stack with no model, `OUTSIDE_PEER=probe` runs the scripted
   peer (`outside-agent-probe.ts peer`) instead; only with `OUTSIDE_ALLOW_HTTP=1`. It also keeps
   a session outside the project, `<peer>-foreign`, registered at the private listener, for row P6.
5. It runs the read sweep's full-member half: `restricted-sweep.sh` (beside this file) in
   its fills mode with the box's full member (`OUTSIDE_SWEEP_PROJECT`,
   `OUTSIDE_SWEEP_FULL_TOKEN_FILE`, `OUTSIDE_SWEEP_CONVERSATION_FILE`, the test person as
   `SWEEP_MEMBER`), and starts its writer, which comments on the scratch issue
   `OUTSIDE_SWEEP_WRITE_ISSUE` every 15 s until the public leg ends. No full member's credential
   goes to the runner: the fills file holds none, and is at most 40 KiB so that it fits one input.
6. It starts the public leg, within the code's 15 minutes: `gh workflow run` of
   `OUTSIDE_WORKFLOW` (default `outside-edge-e2e.yaml`) in `OUTSIDE_WORKFLOW_REPO`, with
   `checks=all`, `project`, `peer_session`, `tailnet_user_code`,
   `sweep_fills` (the fills file, base64, when fills mode made one) and
   `OUTSIDE_WORKFLOW_INPUTS`. With `OUTSIDE_PUBLIC_COMMAND` set, it runs that command here
   instead, for a local stack.
7. It waits for the scratch login to be approved, and runs rows T1-T3.
8. It waits for the public run to end, downloads its artifacts `outside-agent-public` and
   `outside-edge-revocations`, and runs row T4.
9. During the public run's T5 window it runs T5's removal. Once the peer is shown
   `outside-e2e t5-open <nonce>`, it reads the test person on the access list (the control), asks the
   owner on this terminal to remove them and waits for Enter; with `OUTSIDE_OWNER_REMOVE_COMMAND`
   set, it runs that command instead, as a harness acting as the owner's browser does on a local
   stack. It keeps the removal's time. Re-adding the person is the owner's own act.
10. It judges T5 from the public leg's T5 rows and the removal's time, prints rows P1-P9, the sweep
    and the gate, and reads the pins again. A pin that moved during the run holds the gate.
11. It stops the scratch helper and the peer, and removes its scratch directory.

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
is the peer's, on the developer box.

Programs on PATH: bash, curl, tmux, bun and `omp`. The step needs about 40 minutes: P5 waits up to
15 minutes for a GitHub event, beside P4 and P8, P8 waits for a token to expire (300 s), and the T5
window lasts up to 20 minutes.

After P9 the leg opens the T5 window. It tells the peer `outside-e2e t5-open <nonce> at <ms>`,
starts `OUTSIDE_T5_COMMAND`, polls a fresh trade of P2's device login every 10 s, and keeps a relay
socket open with the newest token, as the agent would. The window ends once a trade is refused and
the socket open then has closed, or at the deadline. It prints rows `T5-agent` (with
`refused_at=` and `socket_closed_at=` in Unix seconds), `T5-cookie` and `T5-passkey`; the
tailnet leg judges T5 from them.

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
| P4 | An Oh My Pi session in tmux registers at the edge listener, and the edge serves a relay socket. The probe, as that session's own session id, sends the peer `outside-e2e ping <nonce> at <ms>`. The session shows the peer's `pong` and then its `hello` within 10 s of the peer's sends. The probe sends `outside-e2e reply <nonce> at <ms>`, and the session shows the peer's `done` within 10 s. Each message carries its sender's clock. |
| P5 | Interests in another project's Dispatch topic (403), in `notifications.slack.>`, `notifications.whatsapp.>` and `notifications.team.>` (403 `topic_not_in_project`), in another repository and in `notifications.github.>` (403 `repository_not_in_project`), and a publish of the project's own Dispatch event (403 `topic_not_published`) are refused. An interest in `OUTSIDE_REPOSITORY`'s events is admitted, and one of its events arrives within 15 minutes. |
| P6 | `GET /v1/sessions` lists the peer, which is the control, and not `<peer>-foreign`. A relay-socket `sub` to the peer's subject is refused `subject_not_granted`. |
| P7 | `POST /api/v1/me/agent-tokens` and a credential-request approve with the agent's token answer 403. |
| P8 | A relay socket closes with 4401 within 25 s after its token's expiry, and the token lasts at most 300 s. A message sent to the Oh My Pi session after its own token has expired is shown within 10 s, so the session opened its socket again. |
| P9 | `outside-path-reach.sh` from the Dispatch edge origin passes. |

## The tailnet leg's rows

| Row | It passes when |
| --- | --- |
| pins | The broker's image was built from a commit that holds `OUTSIDE_ENROLL_REFUSAL_COMMIT`. |
| issue | An issue of the project names `OUTSIDE_REPOSITORY`. |
| T1 | The scratch helper's machine login completes. Its machine file holds a credential, and `agent-secrets machine list` names the test person as its approver. `agent-secrets whoami` cannot show this: it reads an enrolled session, and T2 shows that the test person's login enrolls none. |
| T2 | `agent-secrets register` through the scratch helper is refused `403 RESTRICTED_MEMBER_ENROLLS_NOTHING`, by that code and no other, and the canary asked for through it gets no value. |
| T3 | A fresh session enrolled through the box's own helper is granted the canary at once, and its value is 64 characters long. This is T2's positive control: the same broker build and the same route grant the canary, so a refusal in T2 is the member kind's. |
| T4 | The public leg's revoke step, before the T5 window, revokes the scratch login (its `revoke-<credential id>` row passes), and `agent-secrets machine list` through the scratch helper is then refused `LAUNCHER_INVALID`. |
| T5 | The owner's removal, made during the public run's T5 window; before it, the test person is on the project's access list (the control, read with this box's own credential at `OUTSIDE_TAILNET_DISPATCH_URL`), and after the run the list no longer names them (else UNKNOWN). From the public leg's rows: the cookie got 401 within 30 s of the removal, and the passkey sign-in, a fresh trade of the person's device login, and the relay socket open then each stopped within 325 s of it, none before it. A restricted member's token lasts at most 300 s and the edge checks an open socket every 20 s plus a 5 s cache. No terminal, no peer to open the window to, or a part not read, reads UNKNOWN. |
| sweep | `restricted-sweep.sh` on the runner exits 0 (read from the run's `restricted-sweep` artifact), and at least one of the writer's comments was delivered to the full member's own feed strictly inside the sweep's `live window`, so its finding that nothing reached the test person's feed means something. With no such comment it reads UNKNOWN. |
| gate | Every row above and P1-P9 pass, and the pins read at the end are the pins read at the start. |

The tailnet leg holds a public row to what it saw beside it. P4 reads UNKNOWN when the peer did not
start. A PASS of P4 also needs the peer's own side: the peer was shown the ping and the reply within
10 s of their stamps, read from its pane (or the scripted peer's log); not seen there, it reads
UNKNOWN. A PASS of P5 reads UNKNOWN when the issue row did not pass. A PASS of P6 reads UNKNOWN when
the peer did not start or the private listener did not list `<peer>-foreign`. A FAIL of P5 or P6
stands, because a missing refusal or a listed foreign session is evidence by itself.

## The controls, run first and recorded

Run each control on `packages/envoy/scripts/dev-stack.sh`, which runs the real broker, Dispatch
and listener on one machine, with `OUTSIDE_ALLOW_HTTP=1` and `OUTSIDE_PUBLIC_COMMAND`:

- With the listener's edge mode off, rows P5-P8 do not pass: the listener admits every interest
  and lists every session.
- With edge mode on and a repository answer of `[]`, P5's admitted interest is refused.
- P7 with a full member's own token answers 201 and 200.
- With the broker built without the enrollment refusal, T2 fails and T3 still passes.

`bun test e2e/__tests__/outside-agent-acceptance.test.ts`, from `packages/dispatch`, runs each leg
with no input, and with every input set and nothing behind them, and wants UNKNOWN for every row
and exit 2. Its control runs a copy of each leg in which every `UNKNOWN` reads `PASS`, and the
same check must fail on that copy.

## What the two legs do not prove

- The broker's policy denial behind an enrollment is reached only by its unit test: a refused
  enrollment leaves no session to ask with.
- P8's 300 s bound is read on the token that the trade answered, not on a revoke that a person
  makes.
- P4's 10 s bounds compare the peer's clock with the runner's, so they hold only as well as both
  machines keep time. The model peer stamps its answers by running `date` just before each send, so
  its stamps include the model's own time between the two calls: a lag can read longer than the
  delivery took, never shorter.
- T5 compares the runner's clock, which stamps the public leg's T5 rows, with this box's, which
  stamps the removal, so its bounds hold only as well as both machines keep time.
