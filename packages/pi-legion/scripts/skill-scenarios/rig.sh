#!/usr/bin/env bash
# Skill scenarios: fresh Oh My Pi agents, each running one checkout's packed plugins (and so that
# checkout's skills), each given one task a skill rule governs, scored afterwards from what the
# agent actually wrote. Two checkouts under the same scenario compare two versions of a skill.
#
#   rig.sh profile   <label> <checkout> [--envoy-json <file>]
#                                                  build the isolated Oh My Pi home for a checkout;
#                                                  <file>, a JSON object, is merged into the envoy.json
#                                                  the label's sessions read
#   rig.sh live-read <label> <skill>...            one session reads every file of each skill
#   rig.sh batch     <scenario> <runs> <label>...  starts the scratch services, runs 1..<runs> per
#                                                  label (SKILL_SCENARIOS_PARALLEL at once), and
#                                                  stops the services
#   rig.sh score     [<scenario>]                  one row per run, then counts per label
#   rig.sh hidden-proof [<scenario>]               the proof that each run of a -hidden label had
#                                                  Dispatch's computed state hidden from its agent
#                                                  (hidden-proof.ts)
#   rig.sh judge     <run dir>...                  the judge's prompts on each spec-writing or
#                                                  spec-rewrite run (judge.ts run), each in a fresh
#                                                  Oh My Pi session with no Dispatch
#   rig.sh judge-omp <ste|bare> <dir> <output>     one fresh judge session: `omp -p` of
#                                                  <dir>/prompt.txt in <dir>, under the judge's
#                                                  profile (ste: asd-ste100 its one skill; bare: no
#                                                  skill), in a Docker container that sees only
#                                                  <dir> (read-only but for <dir>/<output> and
#                                                  <dir>/sessions), output to <dir>/out.txt, the
#                                                  model and thinking level to <dir>/judge-model.txt
#
# A batch runs each run as `rig.sh run <scenario> <label> <n> <index> <start> [<case>]`, which
# needs that batch's services: <index> is the run's place in the batch, <start> the batch's start
# in epoch seconds, and <case> the case of a heads scenario's world that names one
# (heads-scenarios.txt, the catalog of those worlds, `<scenario> [<case>]` a line, in the order a
# batch runs them). One run on its own is `rig.sh batch <scenario> 1 <label>`.
#
# A run's agent must not learn its scenario, its case or its label: a reviewer whose path read
# `reviewer-own-head-sibling-fails-ablate-head-1` was told the rule the ablate label had removed.
# So every directory an agent sees has a random name, and the names live in the map, <work>/maps,
# which no agent sees (launch):
#   <work>/runs/<id>                 a run's directory, <id> 16 random hex digits
#   <work>/profiles/<id>             a label's profile, <id> 16 random hex digits
#   <work>/maps/runs/<run>           a link to the run's directory, <run> its name,
#                                    <scenario>-<label>-<n> (<scenario>-<case>-<label>-<n> for
#                                    askgate and a world that names a case); score.ts reads runs
#                                    here
#   <work>/maps/profiles/<label>     a link to the label's profile
#   <work>/maps/built-from/<run>     the run's label's built-from, as the run started
#   <work>/maps/quiet/<id>...        a run of a -hidden label: its switch, the rig's reads of the
#                                    results just before and just after the switch went on, and
#                                    its results-before.txt while its agent works (hide_state_on)
# A run's Dispatch database (run_<id>), its Legion project (p<12 hex digits of id>), its tmux
# session (<tag>-<id>) and its Oh My Pi profile (`skills`, the same for every label) name nothing
# either. visible-world.ts lists each place in a run's world that still names one.
#
# A label whose name ends in `-hidden` hides Dispatch's computed state from its agent. From the
# rig's own reads of a run's results until its agent ends, the run's Dispatch leaves each check's
# `pass_stands` and `ci` and each result's `currency` out of its results answer, so `dispatch
# results` prints no state line for the check or for a head (hide_state_on). Each result keeps every
# other field. Only a heads scenario runs such a label. Its checkout is a copy of the checkout of the
# label without that end (label-checkouts.sh --hidden), never the same one, since score.ts leaves
# unscored a run that reads inside another label's checkout.
#
# The tools of this directory (each one's header has its own usage):
#   seed.ts               seeds a batch's scratch Dispatch and each run's issue, and reads back what
#                         the run left there (rig.sh runs it)
#   score.ts              scores each run from its transcript and what seed.ts read back
#   hidden-proof.ts       proves the hiding of each run of a -hidden label from the rig's own reads,
#                         and scans every transcript of the run for a line of the state
#   judge.ts              the person stand-in that answers a spec run's asks (`answer <run dir>
#                         <turn>`), and the judge's prompts on a finished run (`run <run dir>...`)
#   judge-prompts.ts      the ten prompts judge.ts gives the judge, the stand-in's among them
#   label-comments.ts --since <t> --until <t> --out <dir> (--gateway <url> --key-command <file> |
#                     --key-file <file>)
#                         labels people's production Dispatch comments in the window with the
#                         INFRA-140 questions; writes <dir>/labels.jsonl, prints the counts
#   fetch-spec-sources.ts --out <dir> <issue key | dispatch://<PROJECT>/artifact/<slug>>...
#                         reads each source from production Dispatch into <dir>/<name>/
#                         (title.txt, messages.md, asks.json, and old-spec.md for an issue);
#                         --agent-texts --since <t> --until <t> --out <dir> writes
#                         <dir>/agent-texts.json
# label-comments.ts and fetch-spec-sources.ts read production Dispatch with the operator's own
# credentials, which this script unsets, so the operator runs them before a batch. Each refuses an
# --out inside a git or jj checkout (exit 2). Nothing they write is committed.
#
# Scenarios (each one's scoring rule is on the score.ts function that scores it):
#   ask-on-message  A plain session (no Legion role) is told to ask Sami whether to go ahead with a
#                   plan an owner session posted in a Dispatch message; each run seeds its own
#                   issue, attached table and message (seed.ts). Scored by askOnMessage.
#   tester-proof    A tester phase worker, booted by the Legion extension against a daemon stand-in,
#                   is told the implementer has finished; its world is worker_fixture's, with GitHub
#                   a recording stand-in, the remote a local bare repository, and the implementer's
#                   handoff a record on the run's own Dispatch issue (seed.ts tester-proof and
#                   implement-handoff). After the run, seed.ts capture-handoffs reads back every
#                   handoff record of the issue. Scored by testerProof.
#   measure-before-ask
#                   A plain session is told to ask Sami which of three ways to finish an owner
#                   session posted, with the export the message names in its working directory;
#                   the export shows one way reaches only 18 of the 430 issues it names
#                   (run_measure_before_ask). Scored by measureBeforeAsk.
#   brainstorm      A plain session (no Legion role) gets a bare `/brainstorming` prompt on a
#                   scratch to-do CLI, with no word of Dispatch in the prompt and no Dispatch
#                   configuration anywhere in its environment; the run's own Dispatch holds one
#                   project, TODO, with two unrelated issues (seed.ts brainstorm), so the agent must
#                   find it by search, never its own environment or the Dispatch HTTP API. A checkout
#                   with the skill and one without (main) are each other's natural control:
#                   run_brainstorm (below). Scored by brainstormSurface.
#   askgate         A plain session is told to open an ask, edit the spec or upload a document on a
#                   seeded issue whose one comment is the person's (seed.ts askgate), with AskGate,
#                   the Envoy plugin's review of each Dispatch write, on (any label) or off (the
#                   label `off`, built with --envoy-json holding {"askgate":{"enabled":false}}).
#                   SKILL_SCENARIOS_ASKGATE_CASE names the prompt: `facts` (an ask carrying facts
#                   that do not help the choice), `link` (an ask linking the person's comment, on a
#                   choice the comment leaves open), `upload` (an upload of the comment as a
#                   document to link), and five cases of AskGate's rules check: `nochange` (an ask
#                   whose own recommendation keeps the nightly export at 02:00 as it is),
#                   `nochange-rebuttal` (that ask, sent with --advisor-rebuttal), `nochange-block`
#                   (a dispatch doc-edit that inserts that ask as an ask block in the issue's
#                   spec), `untried` (an ask that asks the person to run a gh read the agent did
#                   not run) and `tried` (that ask, after the agent ran the read and got the gh
#                   stand-in's error). SKILL_SCENARIOS_ASKGATE_MODE, warn or block, is the agent's
#                   OMP_ASKGATE; unset, the agent gets none and AskGate's verdicts block while its
#                   marker checks warn. The case is in the run's name, askgate-<case>-<label>-<n>,
#                   so each case's batch keeps its own runs, and each run writes a set mode to
#                   <run>/mode.txt, its agent's own OMP_ASKGATE (run_askgate). Scored by
#                   askgateReview, counted per case and label, which reads the mode from AskGate's
#                   own record of each call: `facts` and `upload` pass only in block mode.
#   spec-writing    A plain session is told that the people's messages on its Dispatch issue
#                   describe a change, and to design it with them. The prompt names no skill. The
#                   run's issue holds the input of one source, which fetch-spec-sources.ts read
#                   from production Dispatch into $SKILL_SCENARIOS_WORK/sources/<name>/: each
#                   message and comment as a message by the person alice, and each answered ask as
#                   an ask another agent opened and alice answered, all in time order (seed.ts
#                   spec-writing). The run is a conversation with a person stand-in, run_spec (below).
#                   Judged by judge.ts, scored by specRow.
#   spec-rewrite    The same, on an issue whose spec is the source's old spec, with a prompt that
#                   asks for the change the newest message names. Scored by specRow.
#   reviewer-own-head
#                   A reviewer, booted by the Legion extension against a daemon stand-in with the
#                   task the daemon gives a reviewer once testing has passed (reviewer_prompt),
#                   reviews #702 of the run's own issue, whose #703 is open too (heads_fixture). Each
#                   run has a NATS and a GitHub stand-in of its own beside its Dispatch (own_github):
#                   seed.ts publishes each pull request's opened event, from which the run's
#                   Dispatch learns its head, and records the case's results at the heads
#                   (heads_world). The case is in the run's name,
#                   reviewer-own-head-<case>-<label>-<n>, and a batch runs
#                   each of the six: sibling-fails (both roles' passes at #702, the tester's fail
#                   at #703), ci-only (k1 carries ci_check, and CI's run passed at #702),
#                   no-implementer and no-tester (one role's pass alone at #702), no-verdict-pass
#                   (both roles' passes at #702) and no-verdict-fail (the implementer's pass, the
#                   tester's record and then its fail at #702), whose task, as every task of today,
#                   names no verdict of the tester. #702's line reads yes in each but
#                   no-verdict-fail, where it reads not known, and the state at the head decides,
#                   whoever recorded the pass, so the reviewer approves #702 in the other five.
#                   Scored by headsReview, counted per case and label.
#   reviewer-pass-and-fail
#                   The same reviewer and pull requests, with a pass of the implementer, a pass of
#                   the tester, the tester's record and then a fail of the tester at #702's head.
#                   Scored by headsReview.
#   architect-pass-and-fail
#                   The tree's root architect, in the daemon's catch-up of a tree in accepting,
#                   judges #702 alone, at whose head the same results stand, the tester's fail
#                   saying its runner lost its checkout before grep ran, after the reviewer's
#                   approval and review record; its daemon stand-in serves sign_off and
#                   request_backward_move. A batch also runs its cases no-verdict-pass and
#                   no-verdict-fail, the results of the reviewer-own-head case of that name at
#                   #702's head under the same approval and review record, each named
#                   architect-pass-and-fail-<case>-<label>-<n>; a run of the scenario's own world
#                   names no case. Scored by headsArchitect, counted per case and label.
#   ci-pass-and-fail
#                   One world, #702 alone, in which k1 carries ci_check="go-checks": the job
#                   recorded a pass and then a fail at #702's head while its third attempt there is
#                   still in progress, so #702's line reads not known for the pass and the fail
#                   beside "CI run go-checks has no result (not_completed)" (ci_job_results). Its
#                   case is the role it drives, in the run's name, ci-pass-and-fail-<role>-<label>-<n>:
#                   reviewer (on the tester's pass), tester (on the implementer's completion) and
#                   architect (in accepting, after the reviewer's approval). Scored by headsReview,
#                   headsTester and headsArchitect, counted per role and label.
#
# A scenario measures its rule only if a label whose skills lack the rule scores lower than one
# whose skills state it. So every comparison carries an ablate label: a checkout of the head with
# the scenario's scored rule deleted from its skills, never committed. measure-before-ask is the
# instrument: gate 2 lives in the dispatch skill alone, and its ablate arm scores below the head.
# ask-on-message and tester-proof are controls: their rules also live in dispatch-first and
# `dispatch ask --help`, and in the tester role prompt, so ablate scores as the head does
# and they cannot detect a lost skill rule. A new scenario is an instrument only once an ablate
# arm has scored lower on it. A label's cuts are its patch's (label-checkouts.sh), and its runs
# show that the patch cuts the rule: the label scores below the head.
#
# A batch's services are a Postgres, a NATS, and the Envoy listener built from this checkout. The
# Postgres and the NATS are two containers the batch starts and removes, or, when
# SKILL_SCENARIOS_POSTGRES_URL and SKILL_SCENARIOS_NATS_URL are both set, the two servers those
# URLs name, which the batch uses and leaves running: that is how a batch runs where there is no
# docker. The two Optional entries below say what each given server must be: the Postgres on this
# machine, and a NATS of the batch's own. Each run gets a Dispatch of its own (own_dispatch): a
# database named after the run's directory (run_<id>) in the batch's Postgres, and the e2e
# harness's real Go Dispatch server (packages/dispatch/e2e/run-server.sh) on that database, seeded
# and read back through its API (seed.ts) and stopped when the run ends. A `dispatch search`
# searches every project of its server, and a Dispatch bearer cannot be limited to one project, so
# a shared server would show each run the other runs' issues and asks. Each run records its
# Dispatch in <run>/dispatch.json, and score.ts does not score two runs that name one database. A
# given server's URL carries its port; a container publishes none (services_up): NATS binds a
# loopback port its own bind chooses, and Postgres answers on its unix socket. The listener serves
# on a port the kernel chose and scripts/e2e/lib/rig.sh holds for it until it serves (start_held),
# each run's Dispatch on its own run-server.sh holds the same way and reports, and the kernel
# assigns each run's daemon stand-in its port.
#
# Operator input (no default in this repository), for `profile`, `judge` and `judge-omp`:
#   LEGION_E2E_MODEL_ROUTE        gateway (the default), key-file or provider: how the agents reach
#                                 Anthropic; on the gateway route a judge's key command runs on the
#                                 host, once per judge, and its container gets only the token
#                                 (cmd_judge_omp)
#   LEGION_E2E_MODEL_GATEWAY_URL  gateway: the model gateway (scripts/e2e/lib/install-model-gateway.sh)
#   LEGION_E2E_ANTHROPIC_KEY_FILE key-file: a 0600 file holding the plaintext key the endpoint
#                                 takes (scripts/e2e/lib/install-key-file-model-route.sh); the key
#                                 command that reads it lives in the label's profile directory,
#                                 and removing that directory removes it
#   LEGION_E2E_MODEL_BASE_URL     key-file: an Anthropic-compatible gateway in place of Anthropic
#                                 itself, whose key the file above holds, so a batch reaches a
#                                 gateway on the documented route rather than a hand-written
#                                 provider block
#   LEGION_E2E_MODEL_PROVIDER_FILE, LEGION_E2E_MODEL_PROVIDER
#                                 provider: an operator's own models.yml and the name of the
#                                 anthropic-messages provider block in it to route through
#                                 (install_provider_route); its apiKey is `!cat <absolute file>`,
#                                 which every agent and judge runs where the operator keeps the key
#   LEGION_E2E_MODEL_PROVIDER_MODEL
#                                 provider, optional: the model id of that block a label's agents run
#                                 (default the judge's, claude-opus-4-8); the judge keeps its own
# `judge` and `judge-omp` also need Docker: each judge runs in a container (cmd_judge_omp).
# Optional:
#   SKILL_SCENARIOS_WORK      work directory, outside any checkout, and with no `.omp` in it or in
#                             any directory above it (default ${TMPDIR:-/tmp}/skill-scenarios)
#   SKILL_SCENARIOS_PARALLEL  concurrent runs in a batch (default 5)
#   SKILL_SCENARIOS_TIMEOUT   seconds one run, or one turn of a spec run, may take (default 1500)
#   SKILL_SCENARIOS_DOCKER_TIMEOUT  seconds one Docker call of judge-omp may wait on the daemon
#                             (default 300): the image's inspect, each pull, and the judge's
#                             container around its omp, whose run is bounded at this plus
#                             SKILL_SCENARIOS_TIMEOUT; a call past it ends the judge at once with a
#                             refusal that names the call, with no further call and no further pull
#                             (cmd_judge_omp)
#   SKILL_SCENARIOS_MEMORY_MAX  the memory an agent's own systemd user scope may hold, with no
#                             swap (default 8G): an agent past it is killed alone (launch)
#   SKILL_SCENARIOS_ASKGATE_CASE  askgate: a case askgate-cases.txt lists (required for that
#                                 scenario)
#   SKILL_SCENARIOS_ASKGATE_MODE  askgate: warn or block, the agent's OMP_ASKGATE (unset: the
#                                 verdicts block and the marker checks warn)
#   SKILL_SCENARIOS_FIXTURE_ONLY  reviewer-own-head, reviewer-pass-and-fail, architect-pass-and-fail
#                                 and ci-pass-and-fail: 1 stops each run once its Dispatch knows
#                                 #702's head (fixture_heads), with <run>/fixture-only and no agent
#   SKILL_SCENARIOS_OWNER     a label `owner=<value>` on the batch's Postgres and NATS containers,
#                             for a machine whose rules name each container's owner; each
#                             container's id is in <work>/services/containers either way
#   SKILL_SCENARIOS_POSTGRES_URL  a Postgres the batch uses in place of the container it would
#                                 start, as a postgres:// or postgresql:// URL
#                                 (postgres://<user>:<password>@<host>:<port>/<database>), never
#                                 a key=value connection string: services_up refuses one, since
#                                 the batch can take a password out of a URL alone. It must hold
#                                 no sslpassword parameter, a client key's passphrase, its key
#                                 percent-decoded as safe_url decodes it (`%73slpassword=` is
#                                 `sslpassword=`): services_up refuses one, since pg_sql takes
#                                 only the password out of the URL it gives psql, and every user
#                                 of this machine can read psql's arguments. It must be the
#                                 Postgres on this machine: services_up reads the URL with
#                                 require_loopback_dsn (scripts/e2e/lib/loopback-dsn.sh) before
#                                 anything connects to it or the batch writes the URL anywhere,
#                                 and refuses every host libpq reads from it but 127.0.0.1,
#                                 localhost and ::1. Then it refuses a URL with a value that the
#                                 connection string each run's Dispatch gets cannot carry
#                                 (run_database_url), a password with a character that does not
#                                 print among them. Its login must be allowed to CREATE
#                                 DATABASE, since each run makes a database of
#                                 its own on it (own_dispatch), and each new database must offer
#                                 the `vector` extension, as the container (pgvector) does: each
#                                 run's Dispatch runs `create extension if not exists vector`, so
#                                 the extension is in the server's template1 or the login may
#                                 create it. Those databases stay there, because the batch drops
#                                 no database and stops no server it was given, so drop them
#                                 between batches: a later run of the same name finds its database
#                                 there and fails, naming it. Its max_connections must admit, for
#                                 each concurrent run, the connections one Dispatch server opens
#                                 (`store/cmd/connections -process` prints them) and 20 more:
#                                 services_up reads max_connections and refuses a
#                                 SKILL_SCENARIOS_PARALLEL above what the server admits, so a
#                                 server at Postgres's default of 100 is refused
#   SKILL_SCENARIOS_NATS_URL      a NATS the batch uses in place of the container it would start,
#                                 as a URL (nats://<host>:<port>). It must run JetStream, as the
#                                 container does (`-js`), on a nats-server at or above the floor
#                                 packages/envoy/natsversion holds: v2.11.12, v2.12.4, or v2.14.0
#                                 and later (the container runs the image
#                                 scripts/e2e/lib/nats-image.sh reads). It must be a scratch server
#                                 of the batch's own: the listener and every tester-proof run's
#                                 agent (as ENVOY_NATS_URL) get this URL, so never give a pod's own
#                                 ENVOY_NATS_URL or a shared hub here
# Set both and the batch starts no container, so it needs neither docker nor a docker daemon. It
# needs psql and python3 instead: it reads the Postgres URL with python3 and libpq, and reaches the
# server with psql. Set one without the other and the batch ignores it and starts both
# containers, as it does when neither is set.
# For spec-writing and spec-rewrite:
#   SKILL_SCENARIOS_SOURCE      `batch`: a directory name under $SKILL_SCENARIOS_WORK/sources
#   SKILL_SCENARIOS_SUPERPOWERS `profile`: the superpowers plugin's directory; its writing-plans
#                               and brainstorming skills go into the label's profile, and a label
#                               built without it runs neither scenario
#
# Each run:
#   - Its agent is `omp -p` on the Oh My Pi the daemon pins (.omp-pin), in the label's profile
#     (make_omp_home, install-plugin-profile.sh, install-model-gateway.sh), under a HOME of its own
#     copied from the label's, with `env -i` and only the variables its pane file names (base_env).
#     An agent that writes to its HOME (`mise use -g`, say) changes nothing for another run.
#   - Its profile's settings set extensionHandlers.toolCallTimeoutMs to 120000 or more, as the model
#     route installers write them, so AskGate's review fits inside Oh My Pi's handler limit
#     (load_label refuses a profile that does not).
#   - It runs in the tmux session skill-scenarios-<digest>-<id> on the work directory's own tmux
#     server (<work>/tmux.sock). The pane shows nothing: omp's output goes to
#     <work>/runs/<id>/out.txt, and the transcript, which grows as the agent works, to the .jsonl
#     under <work>/runs/<id>/sessions.
#   - `gh` and `bun` stand-ins are first on its PATH (standins): no run reaches GitHub, and a bun
#     it reaches through PATH is the bun this script runs on, recorded. A bun named by its path or
#     run through `mise exec` bypasses the stand-in and goes unrecorded. Every container and tmux
#     session a work directory starts carries the directory's digest in its name.
#   - Its agent runs under bubblewrap (launch), in a PID namespace of its own, so it sees no other
#     process, and in a view that holds only what it runs (agent_view): the system's /usr and /etc,
#     its run's directory with the run's own tmp at /tmp, its label's plugins and model key, the
#     batch's services, and the installs of the tools it runs. So it sees neither the map nor
#     another run, label or work directory, no label's checkout, and nothing of the operator's home
#     or the disks but those installs: a reviewer that searched the whole machine (`find / -name …`)
#     read the skills of another label's profile in an older work directory, and of the operator's
#     own plugins. visible-world.ts walks every name of the view. score.ts does not score a run
#     that reads a skill file outside its own label or a file of another label's checkout, or whose
#     PR body carries another run's line (unscored, testerProof). It flags, and still scores, a run
#     whose tool calls only name such a skill file or checkout (skillReaches), or name a path
#     outside its work directory, its label's checkout and the system locations (outsideReaches): a
#     flag shows the path was named, not that it was read. A path a call reaches without naming
#     it, as a glob from `/` does, is not caught.
#   - An exit stops what its command started (on_exit, stop_run, stop_batch).
#
# A spec run (run_spec) is a conversation of at most 8 turns. Turn 1 is the prompt. After each turn,
# `seed.ts state` writes the issue's open asks (none a seed opened) and the spec's open approval
# request to <run>/state-<turn>.json. Then:
#   1. An open approval request: `seed.ts approve` approves the spec as alice, and the session runs
#      one last turn, "The person approved the spec of <issue>."
#   2. Open asks: `judge.ts answer` has the person stand-in choose each answer
#      (<run>/answers-<turn>.jsonl, appended to answers.jsonl), `seed.ts answer` posts them as
#      alice, and the session continues, "The person answered your asks on <issue>."
#   3. Neither: the run ends.
# A later turn is `omp -p --continue` on the run's own session directory. On the pinned Oh My Pi,
# --continue opens the newest transcript in that directory that holds a turn
# (SessionManager.continueRecent), which in a run's directory is the run's one transcript, so each
# turn appends to it. The rig fails a run whose directory holds more than one. Each turn's output
# is <run>/out-<turn>.txt, and out.txt is the last turn's. turns.txt has one line per turn:
# `<turn> <first|answered|approved> <exit line>`. `seed.ts capture-spec` then reads back what the
# run wrote (seed.ts's header names each file), and `rig.sh judge` judges the run afterwards.
set -euo pipefail
# Nothing the rig starts inherits a service endpoint or credential from the caller: a Legion pane
# exports ENVOY_URL, DISPATCH_URL and their token files, a developer's shell can hold NATS
# credentials (packages/dispatch/e2e/run-server.sh strips the same three families), and e2e/api.ts,
# which seed.ts calls, addresses PLAYWRIGHT_BASE_URL with E2E_AGENT_TOKEN whenever it is set.
# PGHOSTADDR, PGSERVICE and PGSERVICEFILE go too, for the loopback guard services_up reads a given
# Postgres URL with (require_loopback_dsn, scripts/e2e/lib/loopback-dsn.sh): the guard reads the
# URL alone, psql reads PGHOSTADDR, and PGSERVICE with PGSERVICEFILE name a service entry that can
# set hostaddr, so psql would connect to an address the guard never read. The sweep runs in every
# process of this script, so `rig.sh run`, which runs own_dispatch, has them cleared too.
for name in $(compgen -e); do
  case $name in DISPATCH_* | ENVOY_* | NATS_* | PLAYWRIGHT_BASE_URL | E2E_AGENT_TOKEN | PGHOSTADDR | PGSERVICE | PGSERVICEFILE) unset "$name" ;; esac
done

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)
root=$(cd "$here/../../../.." && pwd -P)
work=${SKILL_SCENARIOS_WORK:-${TMPDIR:-/tmp}/skill-scenarios}
work=$(realpath -m -- "$work")

# The world of a phase worker (worker_fixture and heads_fixture below): its repository and default
# branch, rig-world.json's, which the pane's addressing (daemon-pane.go) and the GitHub stand-in's
# seed (heads-world.ts) read too, so the text an agent gets names the world it reads; the run's own
# issue, which each run function sets once seed.ts has filed it; and the date of the fixture's
# commits, which fixture_repo sets.
worker_key=''
worker_repo=$(jq -er .repository "$here/rig-world.json") || {
  echo "skill-scenarios: $here/rig-world.json names no repository" >&2
  exit 1
}
worker_base=$(jq -er .defaultBranch "$here/rig-world.json") || {
  echo "skill-scenarios: $here/rig-world.json names no defaultBranch" >&2
  exit 1
}
fixture_when=''

note() { printf 'skill-scenarios: %s\n' "$*"; }
fail() {
  echo "skill-scenarios: $*" >&2
  exit 1
}

# omp_ancestor DIR prints DIR/.omp, or the .omp of the nearest directory above DIR that holds one,
# and fails when none holds one. Oh My Pi reads a `.omp` in any directory from a session's cwd up
# to / as that project's configuration, past the session's HOME. A run's cwd is under the work
# directory, so a `.omp` above it (the operator's own ~/.omp, say) gives every run the operator's
# plugins in place of its label's packed ones.
omp_ancestor() {
  local dir=$1
  while :; do
    if [ -e "$dir/.omp" ] || [ -L "$dir/.omp" ]; then
      printf '%s\n' "${dir%/}/.omp"
      return 0
    fi
    [ "$dir" != / ] || return 1
    dir=$(dirname -- "$dir")
  done
}

case "$work/" in "$root"/*) fail "SKILL_SCENARIOS_WORK $work is inside the checkout $root, where jj would snapshot it" ;; esac
if found=$(omp_ancestor "$work"); then
  fail "SKILL_SCENARIOS_WORK $work is under $found: Oh My Pi reads that .omp as the configuration of every run's cwd, so a run would load its plugins and not its label's; give a work directory with no .omp in it or above it"
fi

# The judge's model and thinking level, the same for every judge session (cmd_judge_omp), recorded
# in each session's judge-model.txt. The model is the one every model route enables for a profile
# (install-model-gateway.sh, install-key-file-model-route.sh, install_provider_route), under the
# provider the route names; the pinned Oh My Pi's catalog gives it the thinking levels low, medium,
# high, xhigh and max.
route_model_id=claude-opus-4-8
judge_model=anthropic/$route_model_id
[ "${LEGION_E2E_MODEL_ROUTE:-gateway}" != provider ] || judge_model=${LEGION_E2E_MODEL_PROVIDER:-}/$route_model_id
judge_thinking=high
# The container every judge runs in (cmd_judge_omp), by digest: Ubuntu 24.04, whose glibc runs the
# pinned omp binary. Every image a CI job fetches is a copy in ghcr.io/cybermonklabs/legion/ci, named by
# digest, which ci.yaml's ci-images job publishes from .github/ci-images.yaml.
judge_image=ghcr.io/cybermonklabs/legion/ci@sha256:534baea6a22c03a63003dbc8dbe78fe34bc0d7e595d9a9dc9834884ff530eb55
# The superpowers skills a spec run's profile holds as its own user skills (cmd_profile).
superpowers_skills=(writing-plans brainstorming)

mkdir -p "$work"

# scripts/e2e/lib/rig.sh's bounded waits, held ports, and process start and stop. Each command
# points `evidence` at the directory its processes log under ($evidence/logs/<name>.log).
evidence=$work
timeout_hook=
# shellcheck source-path=SCRIPTDIR/../../../.. source=scripts/e2e/lib/rig.sh
. "$root/scripts/e2e/lib/rig.sh"
# Every wait for a service the rig starts (await_start, until_true) reads that service's own
# readiness signal, its log line, its open socket or an answer of its own, and await_start fails the
# moment the service exits, so a wait's bound is only for a service that lives and never says it is
# ready. That bound is the run's own deadline, SKILL_SCENARIOS_TIMEOUT (1500 s unset), at each call:
# nothing but the box's own speed delays a readiness signal, a stalled box held the daemon
# stand-in's past a fixed 30 s, and a service held past the run's deadline fails the run anyway.
# packages/envoy/scripts/lib/pg-url.sh's pg_url_run and pg_url_without_password: pg_sql gives psql
# a given Postgres URL's password in PGPASSWORD alone, and safe_url prints that URL without it.
# shellcheck source-path=SCRIPTDIR/../../../.. source=packages/envoy/scripts/lib/pg-url.sh
. "$root/packages/envoy/scripts/lib/pg-url.sh"

tag=skill-scenarios-$(printf '%s' "$work" | sha256sum | cut -c1-8)
tmux_socket=$work/tmux.sock
maps=$work/maps
tmux() { command tmux -S "$tmux_socket" "$@"; }

# What a command started, for its exit trap: the run directory and its agent's tmux session, the
# pids lib/rig.sh's start_process sets (a run's own Dispatch server among them, and a heads run's
# own natsd and GitHub stand-in), and the services' ports, URLs and directory.
R='' session='' daemon_pid='' listener_pid='' dispatch_pid='' runs_pid='' batch_scenario=''
# A run of a -hidden label: its switch (hide_state_on). Empty for every other run.
hide_state_file=''
natsd_pid='' fake_github_pid=''
S='' envoy_port='' dispatch_port='' pg_socket='' pg_url='' nats_url='' github_port='' run_nats_url=''

# label_dir LABEL prints the profile directory the map's link <work>/maps/profiles/LABEL points at,
# or nothing when the link or the directory is missing.
label_dir() {
  [[ $1 =~ ^[a-z0-9][a-z0-9-]*$ ]] || fail "label '$1' is not lower-case letters, digits and hyphens"
  realpath -e -- "$maps/profiles/$1" 2>/dev/null || true
}

# The label's checkout, pin and profile name, recorded by `profile`, as the globals P (the label's
# directory), co, pin and profile that every command and helper below reads.
load_label() {
  P=$(label_dir "$1")
  [ -n "$P" ] && [ -f "$P/ready" ] || fail "no profile for label $1: run \`rig.sh profile $1 <checkout>\` first"
  co=$(<"$P/checkout")
  pin=$(<"$P/pin")
  profile=$(<"$P/profile")
  # AskGate holds its review's deadline 5 s under Oh My Pi's limit for one tool_call handler, and at
  # that limit's 30 s default a review of a long session often times out and its write posts
  # unreviewed (packages/pi-envoy/README.md, "The time limit"): an askgate run would then score the
  # timeout, not the rule. The model route installers set the limit in the profile's settings
  # (scripts/e2e/lib/install-model-gateway.sh, install-key-file-model-route.sh); a profile built
  # before they did is refused here. The read is bun's, which exits 0 where the key is set and 3
  # where it is not: any other exit is a bun that could not run or could not read the settings,
  # refused with bun's own words and never as a missing key.
  local settings=$P/home/.omp/profiles/$profile/agent/config.yml said read=0
  said=$(bun -e 'const v = Bun.YAML.parse(await Bun.file(process.argv[1]).text())?.extensionHandlers?.toolCallTimeoutMs; process.exit(typeof v === "number" && v >= 120000 ? 0 : 3)' "$settings" 2>&1) || read=$?
  case $read in
    0) ;;
    3) fail "label $1's profile settings $settings do not set extensionHandlers.toolCallTimeoutMs to 120000 or more, so AskGate's review would often time out: remove $P and run \`rig.sh profile $1 <checkout>\` from a checkout whose model route installer sets it" ;;
    *) fail "label $1: bun could not read the profile settings $settings: $said" ;;
  esac
}

# install_model_route CHECKOUT PROFILE DIR installs the model route of the profile whose HOME is
# DIR/home, as LEGION_E2E_MODEL_ROUTE picks it, with the checkout's own installer:
#   gateway (the default)  the Hawk model gateway LEGION_E2E_MODEL_GATEWAY_URL names
#                          (scripts/e2e/lib/install-model-gateway.sh)
#   key-file               the 0600 file LEGION_E2E_ANTHROPIC_KEY_FILE names, read fresh on every
#                          call (scripts/e2e/lib/install-key-file-model-route.sh)
#   provider               the provider block LEGION_E2E_MODEL_PROVIDER names in the operator's
#                          LEGION_E2E_MODEL_PROVIDER_FILE (install_provider_route, this script's own,
#                          so a checkout older than the route takes it too)
# The gateway and key-file routes write their call record to DIR/key. The key-file route's key
# command names the key file's path, so it goes to DIR/model-key, apart from the record: removing
# the profile (DIR), the one way to rebuild a label, removes it, and a failed install removes it at
# once. The provider route writes no key command and no record: its block's own `!cat` reads the key.
install_model_route() {
  local co=$1 profile=$2 dir=$3
  case ${LEGION_E2E_MODEL_ROUTE:-gateway} in
  gateway)
    bash "$co/scripts/e2e/lib/install-model-gateway.sh" --profile "$profile" --home "$dir/home" --dest "$dir/key" --cache-dir "$dir/cache"
    ;;
  key-file)
    if [ -z "${LEGION_E2E_ANTHROPIC_KEY_FILE:-}" ]; then
      echo "LEGION_E2E_ANTHROPIC_KEY_FILE is unset: name a 0600 file holding a plaintext Anthropic key" >&2
      return 1
    fi
    local base=()
    # An Anthropic-compatible gateway, whose key the file holds, in place of Anthropic itself:
    # the installer's own --base-url, which this route did not pass, so a batch on a gateway had
    # to be written as a provider block by hand (scripts/e2e/claude-backend-tmux.sh takes the
    # same variable for the same reason).
    [ -z "${LEGION_E2E_MODEL_BASE_URL:-}" ] || base=(--base-url "$LEGION_E2E_MODEL_BASE_URL")
    bash "$co/scripts/e2e/lib/install-key-file-model-route.sh" --profile "$profile" --home "$dir/home" --dest "$dir/key" \
      --key-dir "$dir/model-key" --key-file "$LEGION_E2E_ANTHROPIC_KEY_FILE" "${base[@]}" || {
      rm -rf "$dir/model-key"
      return 1
    }
    ;;
  provider)
    install_provider_route "$profile" "$dir/home" "${4:-$route_model_id}"
    ;;
  *)
    echo "LEGION_E2E_MODEL_ROUTE is '$LEGION_E2E_MODEL_ROUTE': it is gateway, key-file or provider" >&2
    return 1
    ;;
  esac
}

# install_provider_route PROFILE HOME MODEL writes the profile's models.yml and config.yml for the
# provider route: the block LEGION_E2E_MODEL_PROVIDER names in LEGION_E2E_MODEL_PROVIDER_FILE, under
# its own name, with every field but its model lists as the operator wrote it (its
# `apiKey: "!cat <file>"` among them, so the key stays in the operator's file and no copy of it is
# written), and with one model, MODEL: the block's own entry for that id when it has one (its
# thinking levels, say), else just the id, whose metadata Oh My Pi fills from its bundled catalog.
# Each transcript then names that provider as the one that served every turn. The block must speak
# anthropic-messages and read its key with `!cat` from an absolute path, which a judge's container
# mounts read-only. models.yml is written as JSON, which YAML reads as it stands.
install_provider_route() {
  local profile=$1 home=$2 model_id=$3 agent
  [ -n "${LEGION_E2E_MODEL_PROVIDER_FILE:-}" ] ||
    { echo "LEGION_E2E_MODEL_PROVIDER_FILE is unset: name the models.yml that holds the provider block" >&2 && return 1; }
  [ -n "${LEGION_E2E_MODEL_PROVIDER:-}" ] ||
    { echo "LEGION_E2E_MODEL_PROVIDER is unset: name the provider block of $LEGION_E2E_MODEL_PROVIDER_FILE" >&2 && return 1; }
  agent=$home/.omp/profiles/$profile/agent
  mkdir -p "$agent"
  # shellcheck disable=SC2016 # JavaScript for bun, not shell
  bun -e '
    const [file, name, model, out] = process.argv.slice(1);
    const fail = (why) => { console.error(`provider route: ${why}`); process.exit(1); };
    let doc;
    try { doc = Bun.YAML.parse(await Bun.file(file).text()); } catch (e) { fail(`cannot read ${file}: ${e.message}`); }
    const block = doc?.providers?.[name];
    if (!block || typeof block !== "object") fail(`${file} has no provider block ${name}`);
    if (block.api !== "anthropic-messages") fail(`provider ${name} speaks ${block.api}, not anthropic-messages`);
    if (typeof block.baseUrl !== "string" || block.baseUrl === "") fail(`provider ${name} has no baseUrl`);
    if (typeof block.apiKey !== "string" || !/^!cat \/\S+$/.test(block.apiKey))
      fail(`provider ${name} does not read its key as "!cat <absolute file>"`);
    const { models: listed, modelOverrides: _o, discovery: _d, ...connection } = block;
    const entry = (Array.isArray(listed) ? listed : []).find((m) => m?.id === model) ?? { id: model };
    const models = { providers: { [name]: { ...connection, models: [entry] } } };
    await Bun.write(out, `${JSON.stringify(models, null, 2)}\n`);
  ' "$LEGION_E2E_MODEL_PROVIDER_FILE" "$LEGION_E2E_MODEL_PROVIDER" "$model_id" "$agent/models.yml" || return 1
  local model=$LEGION_E2E_MODEL_PROVIDER/$model_id role builtin
  {
    echo "# Written by rig.sh install_provider_route: $model is the one model, and no other provider"
    echo "# a session or a subagent could fall back to without a word is enabled."
    # The handler limit the gateway and key-file installers write, and which load_label requires
    # of every profile: AskGate's review is one tool call, and the default limit cuts it short.
    printf 'extensionHandlers:\n  toolCallTimeoutMs: 120000\n'
    printf 'enabledModels:\n  - %s\n' "$model"
    # Every built-in provider but the block's own name: a block named after one (a box's
    # `anthropic` block that points at its gateway) replaces that provider, and disabling the name
    # would disable the block too, leaving the agents no model.
    printf 'disabledProviders:\n'
    for builtin in anthropic amazon-bedrock bedrock-mantle google ollama llama.cpp lm-studio; do
      [ "$builtin" = "$LEGION_E2E_MODEL_PROVIDER" ] || printf '  - %s\n' "$builtin"
    done
    printf 'modelRoles:\n'
    for role in default smol slow vision plan commit tiny task advisor review oracle deep; do
      printf '  %s: %s\n' "$role" "$model"
    done
  } >"$agent/config.yml"
}

# drop_unready_keys READY KEY_DIR... removes each KEY_DIR, a key-file route's key command
# (install_model_route's DIR/model-key), unless READY exists. A profile setup sets it as its exit
# handler (on_exit) before it installs a route: a setup that stops before its ready file keeps no
# profile that uses the key command, and install-key-file-model-route.sh asks its caller to remove
# the key command on every such exit. A ready profile keeps its key command, and uses it.
# shellcheck disable=SC2329 # on_exit's trap runs it
drop_unready_keys() {
  local ready=$1
  shift
  [ -f "$ready" ] || rm -rf -- "$@"
}

# checkout_commit CHECKOUT prints the commit the checkout's working copy stands on and that
# commit's parents, as `<commit> parents=<parent>,...`. A jj checkout (one that holds .jj) gives
# jj's working-copy commit. Any other checkout gives git's HEAD, with ` dirty` after it when
# `git status --porcelain` lists a change. A git worktree holds no .jj.
checkout_commit() {
  local line head parents status
  if [ -d "$1/.jj" ]; then
    line=$(jj -R "$1" log -r @ --no-graph -T 'commit_id ++ " parents=" ++ parents.map(|c| c.commit_id()).join(",")') || return 1
    printf '%s\n' "$line"
    return 0
  fi
  line=$(git -C "$1" show -s --format='%H %P' HEAD) || return 1
  read -r head parents <<<"$line"
  status=$(git -C "$1" status --porcelain) || return 1
  printf '%s parents=%s%s\n' "$head" "${parents// /,}" "${status:+ dirty}"
}

cmd_profile() {
  local usage="usage: rig.sh profile <label> <checkout> [--envoy-json <file>]"
  [ $# = 2 ] || [ $# = 4 ] || fail "$usage"
  # Every label's Oh My Pi profile has the one name `skills`: the name is in each agent's
  # environment (OMP_PROFILE) and its HOME's paths, so a name per label would tell it its label.
  local label=$1 P co profile=skills envoy_json=''
  if [ $# = 4 ]; then
    [ "$3" = --envoy-json ] || fail "$usage"
    envoy_json=$(realpath -e -- "$4") || fail "--envoy-json $4 does not exist"
    jq -e 'type == "object"' "$envoy_json" >/dev/null 2>&1 || fail "--envoy-json $4 is not a JSON object"
  fi
  P=$(label_dir "$label")
  [ -z "$P" ] || fail "$P, label $label's profile, exists; remove it to rebuild label $label"
  co=$(cd "$2" && pwd -P)
  [ -f "$co/scripts/e2e/lib/install-plugin-profile.sh" ] || fail "$co is not a Legion checkout"
  # A directory of a random name, which every agent of the label sees (its plugins live there), and
  # the map's link that names it.
  P=$work/profiles/$(openssl rand -hex 8)
  mkdir -p "$P" "$maps/profiles"
  ln -sfn -- "../../profiles/${P##*/}" "$maps/profiles/$label"
  on_exit "$(printf 'drop_unready_keys %q %q' "$P/ready" "$P/model-key")"
  printf '%s\n' "$co" >"$P/checkout"
  printf '%s\n' "$profile" >"$P/profile"
  # What the label runs: the checkout's working-copy commit and a digest of its skills tree, the
  # files the packed plugins stage into their dist/skills, listed in byte order (LC_ALL=C) so the
  # digest is the same in every locale.
  local commit
  commit=$(checkout_commit "$co") || fail "cannot read the commit of $co with jj or git"
  {
    printf 'checkout %s\n' "$co"
    printf 'commit %s\n' "$commit"
    printf 'skills %s\n' "$(cd "$co" && find skills -type f -print0 | LC_ALL=C sort -z | xargs -0 sha256sum | sha256sum | cut -d' ' -f1)"
    [ -z "$envoy_json" ] || printf 'envoy-json %s\n' "$(jq -S . "$envoy_json" | sha256sum | cut -d' ' -f1)"
  } >"$P/built-from"
  (
    # shellcheck source=/dev/null
    source "$co/scripts/e2e/lib/omp-home.sh"
    make_omp_home "$P/home"
  )
  # Both plugins: a tester-proof worker's pane loads the Legion extension, which exits a Legion
  # session that finds no Envoy plugin, and every plain session gets its Dispatch skills and tools
  # from the Envoy plugin.
  bash "$co/scripts/e2e/lib/install-plugin-profile.sh" --package pi-envoy --profile "$profile" --home "$P/home" --dest "$P/pi-envoy" >"$P/install.log" 2>&1 ||
    fail "install-plugin-profile.sh --package pi-envoy failed; see $P/install.log"
  bash "$co/scripts/e2e/lib/install-plugin-profile.sh" --package pi-legion --profile "$profile" --home "$P/home" --dest "$P/pi-legion" >>"$P/install.log" 2>&1 ||
    fail "install-plugin-profile.sh --package pi-legion failed; see $P/install.log"
  # The envoy.json a session of the label reads: a named profile's $PI_CODING_AGENT_DIR/envoy.json
  # (envoyConfigPath, packages/envoy-client/src/envoy-config.ts), which Oh My Pi sets to the
  # profile's agent directory. The given object is merged over whatever is there.
  if [ -n "$envoy_json" ]; then
    local agent=$P/home/.omp/profiles/$profile/agent
    mkdir -p "$agent"
    [ -f "$agent/envoy.json" ] || echo '{}' >"$agent/envoy.json"
    jq -s '.[0] * .[1]' "$agent/envoy.json" "$envoy_json" >"$agent/envoy.json.new"
    mv "$agent/envoy.json.new" "$agent/envoy.json"
  fi
  install_model_route "$co" "$profile" "$P" "${LEGION_E2E_MODEL_PROVIDER_MODEL:-}" >>"$P/install.log" 2>&1 ||
    fail "the model route (${LEGION_E2E_MODEL_ROUTE:-gateway}) could not be installed; see $P/install.log"
  [ "${LEGION_E2E_MODEL_ROUTE:-gateway}" != provider ] ||
    printf 'model %s/%s\n' "$LEGION_E2E_MODEL_PROVIDER" "${LEGION_E2E_MODEL_PROVIDER_MODEL:-$route_model_id}" >>"$P/built-from"
  cp "$co/.omp-pin" "$P/pin"
  [ -z "${SKILL_SCENARIOS_SUPERPOWERS:-}" ] || install_superpowers "$P" "$profile" "$(<"$P/pin")"
  touch "$P/ready"
  trap - EXIT HUP INT TERM
  cat "$P/built-from"
}

# pinned_omp PIN prints the path of the omp binary mise installed for PIN. A run names it by path:
# `mise x <pin> -- omp` leaves an `omp` earlier on the caller's PATH (a wrapper script, say) ahead
# of the pin's own whenever PIN is the version mise already has active there, so the run would get
# the wrapper and whatever omp and configuration it picks, not the label's pin.
pinned_omp() {
  local omp
  omp=$(mise where "$1")/bin/omp || fail "mise has no install of $1"
  [ -x "$omp" ] || fail "$omp, the pin $1's binary, is not executable"
  printf '%s\n' "$omp"
}

# install_superpowers P PROFILE PIN copies SKILL_SCENARIOS_SUPERPOWERS's writing-plans and
# brainstorming skills into the profile's user skill directory, the one the pinned Oh My Pi reads
# for a named profile ($HOME/.omp/profiles/<profile>/agent/skills/<name>/SKILL.md). Then one
# `omp -p` under the profile reads skill://writing-plans, and the step fails when the reply holds
# `Unknown skill`, Oh My Pi's answer for a skill it did not load. It also fails when no tool result
# of the probe's transcript holds the skill's own heading, so a probe that read nothing does not
# pass. It records the plugin's version and each skill's digest in built-from. It marks the label
# with a `superpowers` file, which a spec run needs.
install_superpowers() {
  local P=$1 profile=$2 pin=$3 src=$SKILL_SCENARIOS_SUPERPOWERS skill dest probe version omp
  omp=$(pinned_omp "$pin") || exit 1
  dest=$P/home/.omp/profiles/$profile/agent/skills
  mkdir -p "$dest"
  for skill in "${superpowers_skills[@]}"; do
    [ -f "$src/skills/$skill/SKILL.md" ] || fail "SKILL_SCENARIOS_SUPERPOWERS $src has no skills/$skill/SKILL.md"
    cp -RL "$src/skills/$skill" "$dest/$skill"
  done
  version=$(jq -r '.version // "unknown"' "$src/package.json" 2>/dev/null) || version=unknown
  {
    printf 'superpowers %s %s\n' "$src" "$version"
    for skill in "${superpowers_skills[@]}"; do
      printf 'superpowers-skill %s %s\n' "$skill" "$(cd "$dest/$skill" && find . -type f -print0 | LC_ALL=C sort -z | xargs -0 sha256sum | sha256sum | cut -d' ' -f1)"
    done
  } >>"$P/built-from"
  # The read runs as a run does: the label's HOME and profile, no caller variable, an empty cwd, no
  # Dispatch, and an Envoy address nothing listens on.
  probe=$P/superpowers-probe
  mkdir -p "$probe/cwd" "$probe/tmp"
  (
    cd "$probe/cwd" &&
      env -i HOME="$P/home" USER="$USER" TERM=xterm-256color TMPDIR="$probe/tmp" PATH="$PATH" \
        MISE_DATA_DIR="${MISE_DATA_DIR:-${XDG_DATA_HOME:-$HOME/.local/share}/mise}" OMP_PROFILE="$profile" \
        PI_NOTIFICATIONS=off PI_NO_TITLE=1 ENVOY_URL=http://127.0.0.1:1 timeout "${SKILL_SCENARIOS_TIMEOUT:-1500}" \
        mise x "$pin" -- "$omp" -p 'Call the read tool once on the path skill://writing-plans, and nothing else. Then reply with the first line of what it returned.' \
        --session-dir "$probe/sessions" </dev/null
  ) >"$probe/out.txt" 2>&1 || fail "the read of skill://writing-plans under profile $profile failed; see $probe/out.txt"
  ! grep -q 'Unknown skill' "$probe/out.txt" "$probe"/sessions/*.jsonl 2>/dev/null ||
    fail "profile $profile does not load skill://writing-plans (Unknown skill); see $probe/out.txt"
  jq -e -s --arg heading "$(grep -m1 '^# ' "$dest/writing-plans/SKILL.md")" \
    'any(.[]; .message.role? == "toolResult" and any(.message.content[]?; (.text // "") | contains($heading)))' \
    "$probe"/sessions/*.jsonl >/dev/null 2>&1 ||
    fail "the probe under profile $profile read no skill://writing-plans; see $probe/out.txt"
  : >"$P/superpowers"
}

# Writes the variables every agent gets to $R/pane.env: a HOME copied from the label's, the
# label's profile, the run's stand-ins first on PATH, its own TMPDIR, and nothing else from the
# caller's environment.
base_env() {
  mkdir -p "$R/tmp"
  cp -a "$P/home" "$R/home"
  {
    echo "HOME=$R/home"
    echo "USER=$USER"
    echo "TERM=xterm-256color"
    echo "TMPDIR=$R/tmp"
    echo "MISE_DATA_DIR=${MISE_DATA_DIR:-${XDG_DATA_HOME:-$HOME/.local/share}/mise}"
    echo "OMP_PROFILE=$profile"
    echo "PI_NOTIFICATIONS=off"
    echo "PI_NO_TITLE=1"
    echo "SKILL_SCENARIO_RUN=$R"
    echo "PATH=$R/bin:$PATH"
  } >"$R/pane.env"
}

# The run's stand-ins in $R/bin: `gh`, which records the call and answers from $R/fixtures.json,
# and `bun`, which runs the bun this script runs on (a caller's `bun` may be a shim that resolves
# nothing under the agent's HOME) and records the call to $R/calls.jsonl as gh-standin.ts records
# its own. The score reads the tester's run of the CLI from that record. No `legion` stands in: an
# agent reaches the daemon stand-in through its `legion` tool alone, never `legion` from bash.
# gh-standin.ts is bundled into $R/bin, so the `gh` an agent reads names no path of this checkout,
# which is the head label's own checkout when the rig runs from it.
standins() {
  local bun_real
  bun_real=$(bun -e 'console.log(process.execPath)')
  mkdir -p "$R/bin"
  bun build --target bun --minify --outfile "$R/bin/gh-standin.js" "$here/gh-standin.ts" >/dev/null ||
    fail "gh-standin.ts did not bundle"
  printf '#!/bin/sh\nexec %q %q gh "$@"\n' "$bun_real" "$R/bin/gh-standin.js" >"$R/bin/gh"
  cat >"$R/bin/bun" <<EOF
#!/usr/bin/env bash
at=\$(date -u +%FT%T.%3NZ)
status=0
$(printf %q "$bun_real") "\$@" || status=\$?
jq -cn --arg at "\$at" --argjson exit "\$status" '{at:\$at, as:"bun", argv:\$ARGS.positional, exit:\$exit}' \\
  --args -- "\$@" >>$(printf %q "$R/calls.jsonl")
exit "\$status"
EOF
  chmod +x "$R/bin/gh" "$R/bin/bun"
  [ -f "$R/fixtures.json" ] || printf '{"routes":[]}\n' >"$R/fixtures.json"
}

# view_bind MODE SOURCE [DEST] adds SOURCE to the agent's view at DEST (SOURCE's own path when none
# is given), MODE --ro-bind or --bind, and records SOURCE in `bound`: the arrays of agent_view's
# caller, launch.
view_bind() {
  view+=("$1" "$2" "${3:-$2}")
  bound+=("$2")
}

# view_tool NAME PATH DATA gives the agent's shell the tool NAME: its first match on PATH (the
# pane's) that is the run's own, the system's (under /usr, where /bin, /sbin and /lib lead) or in a
# mise install, <DATA>/installs/<tool>/<version> (DATA the pane's MISE_DATA_DIR). A mise install
# binds whole, at the path PATH names, since a tool's links and libraries sit beside its binary,
# and the run's bin links NAME to it: `mise x`, which runs the agent, takes every mise install but
# the pin's off the agent's PATH, and the run's bin stays first on it. Any other match is not in
# the view, and the search goes on past it: a wrapper in the operator's home, or a mise shim, which
# resolves the tool from the operator's own mise config. No match gives nothing.
view_tool() {
  local name=$1 data=$3 dir dirs found real tool version install
  IFS=: read -ra dirs <<<"$2"
  for dir in "${dirs[@]}"; do
    found=$dir/$name
    [ -f "$found" ] && [ -x "$found" ] || continue
    case $found in "$R"/*) return 0 ;; esac
    real=$(realpath -e -- "$found") || continue
    case $real in /usr/* | /bin/* | /sbin/* | /lib/* | /lib32/* | /lib64/* | /libx32/*) return 0 ;; esac
    [ -n "$data" ] || continue
    case $dir/ in "$data"/installs/*/*/*) ;; *) continue ;; esac
    tool=${dir#"$data"/installs/}
    version=${tool#*/}
    install=$data/installs/${tool%%/*}/${version%%/*}
    case $real in "$(realpath -e -- "$install")"/*) ;; *) continue ;; esac
    view_bind --ro-bind "$(realpath -e -- "$install")" "$install"
    mkdir -p "$R/bin"
    ln -sfn -- "$found" "$R/bin/$name"
    return 0
  done
}

# provider_key_file MODELS prints the file that the provider route's models.yml (MODELS, JSON, as
# install_provider_route writes it) reads its key from with `!cat <file>`, and nothing for a
# models.yml of another route.
provider_key_file() {
  local key
  key=$(jq -r '[.providers[].apiKey] | first // empty' "$1" 2>/dev/null) || key=
  case $key in '!cat '*) printf '%s\n' "${key#'!cat '}" ;; esac
}

# key_command_file COMMAND prints the key file that a key-file route's key command
# (install-key-file-model-route.sh) reads on each call: its `key_file=` line assigns the path with
# printf %q, so the line is read as bash reads it.
key_command_file() {
  bash -c "$(grep -m1 '^key_file=' "$1")"$'\nprintf %s "$key_file"'
}

# fresh_gateway_key makes the gateway route's kept key good past the run's deadline. The route's
# key command, <P>/key/hawk-token (install-model-gateway.sh), serves the key it keeps in <P>/cache
# and mints one with hawk-token when none is good, and hawk-token needs the operator's HOME and
# session bus, which no agent's view holds. So the key is minted here, on the host, and the agent's
# key command serves it from its read-only view of the cache. A kept key that expires before the
# run's deadline is dropped and minted once more, as judge_keys does for a judge.
fresh_gateway_key() {
  local cache=$P/cache/hawk-token.key kept_until='' deadline=$((EPOCHSECONDS + ${SKILL_SCENARIOS_TIMEOUT:-1500}))
  "$P/key/hawk-token" >/dev/null || fail "the gateway's key command gave no key; see $P/key/hawk-token.log"
  [ ! -s "$cache" ] || read -r kept_until _ <"$cache"
  [ "${kept_until:-0}" -lt "$deadline" ] || return 0
  rm -f -- "$cache"
  "$P/key/hawk-token" >/dev/null || fail "the gateway's key command gave no key; see $P/key/hawk-token.log"
  kept_until=''
  [ ! -s "$cache" ] || read -r kept_until _ <"$cache"
  [ "${kept_until:-0}" -ge "$deadline" ] ||
    fail "the gateway's key command keeps its key until ${kept_until:-no time}, before the run's deadline $deadline, and an agent's key command cannot mint one in its view"
}

# agent_view OMP MISE fills launch's `view` with the bubblewrap arguments of the agent's view: an
# empty root, and in it only what the agent runs.
#   - The system, read-only: /usr, the links at / that lead into it (or the directories, on a
#     system that keeps them apart), /etc, and the directory of each file outside both that a link
#     of /etc names (resolv.conf's, under /run). A /dev of its own, its PID namespace's /proc, and
#     an empty /dev/shm and /var/tmp, since the machine's are every run's.
#   - The run's directory, writable, with its own tmp at /tmp: in the third round 27 agents wrote
#     scratch files to the machine's /tmp under fixed names (`/tmp/lweval1_review.json` by five),
#     which another run could read.
#   - The label's packed plugins and model key and the batch's services, read-only, and none of
#     the profile's own records (checkout, built-from), which name the label's checkout.
#   - The key the model route reads, read-only: the directory of the provider route's `!cat <file>`
#     in the run's models.yml (the directory, so a key the operator renews by writing a new file in
#     its place still reaches the agent), the key file of the key-file route's key command, and the
#     gateway route's kept key (fresh_gateway_key).
#   - The tools it runs: OMP's install, which MISE (run by its path) runs as the pin; MISE and the
#     bun the stand-ins run; the native module cache the run's HOME links to (make_omp_home),
#     writable, since omp extracts its own version's addon there when another session has pruned
#     it; and the install of each of agent_tools on the pane's PATH (view_tool). The agents' shells
#     are seen to run jj, jq, git and python3 (the eval tool's kernel among them) beyond the
#     system's own commands.
# It fails when a label's checkout holds a path the view binds, or lies under one.
agent_view() {
  local omp=$1 mise=$2 dir link target key_file record checkout pane_path data tool agent_tools=(git jj jq python3)
  view_bind --ro-bind /usr
  for dir in /bin /sbin /lib /lib32 /lib64 /libx32; do
    if [ -L "$dir" ]; then
      view+=(--symlink "$(readlink -- "$dir")" "$dir")
    elif [ -d "$dir" ]; then
      view_bind --ro-bind "$dir"
    fi
  done
  view_bind --ro-bind /etc
  for link in /etc/*; do
    [ -L "$link" ] || continue
    target=$(realpath -e -- "$link" 2>/dev/null) || continue
    case $target in /etc/* | /usr/* | /proc/* | /dev/*) ;; *) view_bind --ro-bind "$(dirname -- "$target")" ;; esac
  done
  view+=(--dev /dev --tmpfs /dev/shm --proc /proc --tmpfs /var/tmp)
  view_bind --bind "$R"
  view_bind --bind "$R/tmp" /tmp
  for dir in "$P/pi-envoy" "$P/pi-legion" "$P/model-key" "$P/key" "$work/services"; do
    [ ! -e "$dir" ] || view_bind --ro-bind "$dir"
  done
  key_file=$(provider_key_file "$R/home/.omp/profiles/$profile/agent/models.yml")
  case $key_file in
  '') ;;
  /*) view_bind --ro-bind "$(dirname -- "$key_file")" ;;
  *) fail "the run's models.yml reads its key with \`!cat $key_file\`, not from an absolute path" ;;
  esac
  if [ -f "$P/model-key/anthropic-key" ]; then
    key_file=$(key_command_file "$P/model-key/anthropic-key")
    [ -f "$key_file" ] || fail "the key command $P/model-key/anthropic-key names '$key_file', which is not a file"
    view_bind --ro-bind "$key_file"
  fi
  if [ -x "$P/key/hawk-token" ]; then
    fresh_gateway_key
    view_bind --ro-bind "$P/cache"
  fi
  dir=$(dirname -- "$(dirname -- "$omp")")
  view_bind --ro-bind "$(realpath -e -- "$dir")" "$dir"
  view_bind --ro-bind "$mise"
  target=$(bun -e 'console.log(process.execPath)') || fail "cannot run the bun this script runs on, which the run's stand-ins run"
  target=$(realpath -e -- "$target") || fail "the bun this script runs on, $target, is no file"
  view_bind --ro-bind "$target"
  link=$R/home/.omp/natives
  if [ -L "$link" ] && target=$(realpath -e -- "$link"); then view_bind --bind "$target" "$(readlink -- "$link")"; fi
  pane_path=$(sed -n 's/^PATH=//p' "$R/pane.env" | tail -n 1)
  data=$(sed -n 's/^MISE_DATA_DIR=//p' "$R/pane.env" | tail -n 1)
  for tool in "${agent_tools[@]}"; do view_tool "$tool" "$pane_path" "$data"; done
  for record in "$maps"/profiles/*/checkout; do
    [ -f "$record" ] || continue
    checkout=$(<"$record")
    for dir in "${bound[@]}"; do
      case $dir/ in "$checkout"/*) fail "label checkout $checkout holds $dir, which the agent's view binds" ;; esac
      case $checkout/ in "$dir"/*) fail "label checkout $checkout is under $dir, which the agent's view binds" ;; esac
    done
  done
}

# launch [OMP_ARG]... runs `omp -p "$(cat $R/prompt.txt)"` and then each OMP_ARG in $R/cwd under
# $R/pane.env, followed by $R/system-args (one --append-system-prompt argument, as shell text) when
# it exists, as the tmux session <tag>-<id>, <id> the name of the run's directory. It waits for the
# session to exit or time out. A timeout is the run's result (`exit=timeout`), not a rig failure,
# so this wait is not an until_true. A spec run's later turn passes --continue. Each launch starts
# out.txt again, so its wait reads only its own exit line.
launch() {
  local system='' extra='' omp mise scope bwrap memory_max=${SKILL_SCENARIOS_MEMORY_MAX:-8G} view=() bound=()
  omp=$(pinned_omp "$pin") || exit 1
  mise=$(command -v mise) || fail "mise is not on PATH: a run's agent runs its pin through \`mise x\`"
  mise=$(realpath -e -- "$mise") || fail "mise on PATH, $mise, is no file"
  # The agent runs in a systemd user scope of its own, capped at SKILL_SCENARIOS_MEMORY_MAX (8G)
  # with no swap: an agent that grows past it is killed alone (exit 137 in out.txt), and never
  # the caller's session, whose scope a run's processes would otherwise share.
  scope=$(command -v systemd-run) || fail "systemd-run is not on PATH: a run's agent runs in a capped user scope"
  # And under bubblewrap, in a PID namespace of its own, where it sees no process but its own (not
  # the batch's command line, which names its labels), in a view that holds only what it runs
  # (agent_view). bwrap reads the view from <work>/views/<id> (--args), which the view does not
  # hold and visible-world.ts walks, and neither launch.sh nor bwrap's own command line, which the
  # agent's /proc shows as its process 1, lists it. bwrap reaches the namespaces through the user
  # namespace AppArmor lets it make.
  bwrap=$(command -v bwrap) || fail "bwrap is not on PATH: a run's agent runs in a PID namespace and a view of its own"
  agent_view "$omp" "$mise"
  view+=(--unshare-pid --die-with-parent --chdir "$R/cwd")
  mkdir -p "$work/views"
  printf '%s\0' "${view[@]}" >"$work/views/${R##*/}"
  session=$tag-${R##*/}
  [ $# = 0 ] || extra=" $(printf '%q ' "$@")" extra=${extra% }
  [ ! -f "$R/system-args" ] || system=" $(<"$R/system-args")"
  cat >"$R/launch.sh" <<EOF
#!/usr/bin/env bash
cd $(printf %q "$R/cwd")
mapfile -t vars <$(printf %q "$R/pane.env")
exec $(printf %q "$scope") --user --scope --quiet -p MemoryMax=$(printf %q "$memory_max") -p MemorySwapMax=0 -- $(printf %q "$bwrap") --args 3 -- env -i "\${vars[@]}" $(printf %q "$mise") x $(printf %q "$pin") -- $(printf %q "$omp") -p "\$(cat $(printf %q "$R/prompt.txt"))"$extra$system --session-dir $(printf %q "$R/sessions") 3<$(printf %q "$work/views/${R##*/}") >$(printf %q "$R/out.txt") 2>&1
EOF
  chmod +x "$R/launch.sh"
  : >"$R/out.txt"
  tmux kill-session -t "=$session" 2>/dev/null || true
  tmux new-session -d -s "$session" -x 200 -y 50 "bash $(printf %q "$R/launch.sh"); echo exit=\$? >>$(printf %q "$R/out.txt")"
  local deadline=$((SECONDS + ${SKILL_SCENARIOS_TIMEOUT:-1500}))
  # The tmux command appends `exit=<status>` as out.txt's last line once omp has exited.
  while [ $SECONDS -lt $deadline ]; do
    tail -n 1 "$R/out.txt" 2>/dev/null | grep -q '^exit=' && break
    sleep 5
  done
  tail -n 1 "$R/out.txt" 2>/dev/null | grep -q '^exit=' || echo "exit=timeout" >>"$R/out.txt"
  tmux kill-session -t "=$session" 2>/dev/null || true
}

# run_env_pids DIR prints `PID START` (stat field 22) for each process whose environment names a run
# at or under DIR in SKILL_SCENARIO_RUN, which an agent gets from its pane file and every command it
# runs inherits. It finds what stop_tree cannot reach from the pids a command recorded: a process
# that left their tree, such as a `setsid -f` child. A process that merely works under DIR, such as a
# developer's shell watching a run, has no such variable, and another user's environment cannot be
# read. It is procs_naming (scripts/e2e/lib/process-tree.sh), which hands every /proc/<pid>/environ
# to grep through xargs in calls that fit ARG_MAX, reads each match whole, and prints why and fails
# with 2 when the machine's environments could not be read.
# shellcheck disable=SC2329 # stop_env_processes, which the exit traps run, runs it
run_env_pids() { procs_naming "$1" SKILL_SCENARIO_RUN; }

# stop_env_processes DIR ends each process run_env_pids DIR finds and everything under it, a child
# that dropped the variable included, through kill_held (scripts/e2e/lib/process-tree.sh), the
# run's sweep's own loop: each is frozen and read again, ended only while it is still the process
# the walk read (its start time) and still names the run, and then ended with its tree, its own
# parent not read again. When run_env_pids could not read the machine's environments, or a tree's
# processes could not all be read (kill_held), it says so, and this returns, so the exit handler
# that runs it goes on with the rest of its cleanup.
# shellcheck disable=SC2329 # the exit traps run it
stop_env_processes() {
  local pairs
  pairs=$(run_env_pids "$1") || {
    echo "stop_env_processes: stopped no process of the run under $1, since none could be named" >&2
    return 0
  }
  kill_held "$1" SKILL_SCENARIO_RUN <<<"$pairs" ||
    echo "stop_env_processes: the processes under some process of the run under $1 could not be read" >&2
}

# stop_panes SESSION stops, with stop_tree, each pane process of SESSION on the work directory's
# tmux server and everything under it, while the server is still its parent.
# shellcheck disable=SC2329 # the exit traps run it
stop_panes() {
  local server pid
  server=$(tmux display-message -p '#{pid}' 2>/dev/null) || return 0
  for pid in $(tmux list-panes -s -t "=$1" -F '#{pane_pid}' 2>/dev/null || true); do stop_tree "$pid" "$server"; done
}

# A run's or a live read's exit: the switch of a -hidden run turned off (hide_state_off), its daemon
# stand-in, its own Dispatch server, a heads run's own natsd and GitHub stand-in, its agent's
# session, and every process that carries its SKILL_SCENARIO_RUN. The run's database stays in the
# batch's Postgres: services_down removes the container it started, and leaves a given server, with
# every database its runs made, to the operator. The services are this shell's own jobs, so each is
# waited for once it is killed: bash otherwise prints a `Killed` notice for it on stderr.
# shellcheck disable=SC2329 # on_exit's trap runs it
stop_run() {
  local pid
  hide_state_off
  for pid in "$daemon_pid" "$dispatch_pid" "$natsd_pid" "$fake_github_pid"; do
    [ -z "$pid" ] || { stop_tree "$pid" && wait "$pid" 2>/dev/null || true; }
  done
  [ -z "$session" ] || stop_panes "$session"
  [ -z "$R" ] || stop_env_processes "$R"
  [ -z "$session" ] || tmux kill-session -t "=$session" 2>/dev/null || true
}

# on_exit HANDLER runs HANDLER at the command's exit, INT, TERM and HUP included.
on_exit() {
  # shellcheck disable=SC2064 # the handler's name, expanded now on purpose
  trap "$1" EXIT
  trap 'exit 129' HUP
  trap 'exit 130' INT
  trap 'exit 143' TERM
}

# skill_reads CHECKOUT SKILL prints the read path of each markdown file of the checkout's skill
# SKILL, in the order a live read reads them: skill://SKILL (its SKILL.md) first, then
# skill://SKILL/<path> for every other .md file under the skill's directory, in byte order.
skill_reads() {
  local dir=$1/skills/$2 file
  [ -f "$dir/SKILL.md" ] || fail "$1 has no skill $2"
  printf 'skill://%s\n' "$2"
  while IFS= read -r -d '' file; do
    printf 'skill://%s/%s\n' "$2" "${file#./}"
  done < <(cd "$dir" && find . -type f -name '*.md' ! -path ./SKILL.md -print0 | LC_ALL=C sort -z)
}

cmd_live_read() {
  [ $# -ge 2 ] || fail "usage: rig.sh live-read <label> <skill>..."
  load_label "$1"
  shift
  local paths=() skill list
  R=$work/live-read/${P##*/}
  on_exit stop_run
  rm -rf "$R"
  mkdir -p "$R/cwd" "$R/sessions"
  for skill in "$@"; do
    list=$(skill_reads "$co" "$skill") || exit 1
    mapfile -t -O "${#paths[@]}" paths <<<"$list"
  done
  printf 'Call the read tool once for each of these paths, in this order, one call at a time, and nothing else: %s. Then reply with the single word DONE.\n' "${paths[*]}" >"$R/prompt.txt"
  base_env
  standins
  # No Dispatch, and an Envoy address nothing listens on: the session reads files and nothing else.
  echo "ENVOY_URL=http://127.0.0.1:1" >>"$R/pane.env"
  launch
  bun "$here/score.ts" live-read "$R" "$co/skills" "$R/home/.omp/profiles/$profile/logs"
}

services_env=$work/services/env
# The Postgres SKILL_SCENARIOS_POSTGRES_URL names, written by services_up and read by own_dispatch,
# which runs in a process of its own (`rig.sh run`): a run takes the Postgres its batch brought up,
# never one its own environment names. The file is 0600, since the URL can carry a password, and
# its absence is how a run knows its batch started a container instead.
services_postgres=$work/services/postgres-url

# start_listener serves the Envoy listener on a port the kernel chose, held from that choice until
# the listener serves on it (lib/rig.sh's start_held), and sets envoy_port.
start_listener() {
  ENVOY_API_TOKEN="$(<"$S/envoy-token")" ENVOY_LISTEN_HOST=127.0.0.1 \
    ENVOY_MACHINE_ID="$tag" NATS_URLS="$nats_url" \
    start_held listener envoy_port ENVOY_LISTEN_FD --env 'PORT={port}' -- "$S/envoy-listener"
  await_start listener "$listener_pid" "${SKILL_SCENARIOS_TIMEOUT:-1500}" "the Envoy listener to answer /v1/sessions" listener_answers
}

# listener_answers reads the listener's /v1/sessions with the rig's bearer, which reaches curl on
# standard input from printf, a shell builtin, so it is in no command line.
# shellcheck disable=SC2329 # await_start in start_listener runs it
listener_answers() {
  printf 'Authorization: Bearer %s' "$(<"$S/envoy-token")" |
    curl -fsS -o /dev/null -H @- "http://127.0.0.1:$envoy_port/v1/sessions"
}

# start_dispatch DATABASE_URL starts the e2e harness's Dispatch server on that database. The
# harness's fake Envoy, which run-server.sh names and this rig does not run, is port 1, where
# nothing listens, and so is its fake GitHub, with no NATS, unless own_github gave the run a GitHub
# stand-in and a NATS of its own: then the server reads that GitHub and its delivery intake reads
# that NATS (DISPATCH_E2E_NATS_URLS). Its broker switch is empty, so the server has no secrets
# broker. With no DISPATCH_E2E_PORT, run-server.sh serves Dispatch on a port the kernel chose, held
# until the server serves on it, and the server reports it (`msg="dispatch: listening"
# addr=127.0.0.1:<port>`), which sets dispatch_port. Each run has an evidence directory of its own,
# and the log is emptied first, so the line read is this run's own start. In a run of a -hidden
# label, DISPATCH_TEST_HIDE_STATE_FILE names the run's switch, which run-server.sh passes to the
# server: its results answer leaves out the computed state while that file exists (hide_state_on).
start_dispatch() {
  local url=$1 log=$evidence/logs/dispatch.log
  : >"$log"
  DATABASE_URL=$url FAKE_ENVOY_PORT=1 FAKE_GITHUB_PORT=${github_port:-1} DISPATCH_E2E_NATS_URLS=${run_nats_url:-} \
    DISPATCH_TEST_HIDE_STATE_FILE=$hide_state_file \
    DISPATCH_E2E_AGENT_SECRETS_URL='' start_process dispatch bash "$root/packages/dispatch/e2e/run-server.sh"
  await_start dispatch "$dispatch_pid" "${SKILL_SCENARIOS_TIMEOUT:-1500}" "Dispatch to report its port" \
    report_port "$log" 'msg="dispatch: listening" addr=127\.0\.0\.1:'
  dispatch_port=$(report_port "$log" 'msg="dispatch: listening" addr=127\.0\.0\.1:')
  await_start dispatch "$dispatch_pid" "${SKILL_SCENARIOS_TIMEOUT:-1500}" "Dispatch to answer /api/v1/whoami" \
    curl -fsS -o /dev/null -H 'Authorization: Bearer e2e-token' "http://127.0.0.1:$dispatch_port/api/v1/whoami"
}

# One batch at a time per work directory: $work/services/up is its lock, taken before the batch
# starts or stops anything. A batch killed outright leaves it, with its containers and processes.
lock_services() {
  S=$work/services
  mkdir -p "$S/logs"
  (set -C && : >"$S/up") 2>/dev/null ||
    fail "a batch already holds $S/up; a batch killed outright leaves it, its $tag-* containers and its processes behind"
}

# given_services is whether the operator gave the batch both a Postgres and a NATS, so it starts
# no container and needs no docker. One URL without the other is ignored: a batch that started the
# other service as a container would need docker anyway.
given_services() {
  [ -n "${SKILL_SCENARIOS_POSTGRES_URL:-}" ] && [ -n "${SKILL_SCENARIOS_NATS_URL:-}" ]
}

# safe_url URL prints URL without its password, for a note, a failure message and a run's
# dispatch.json: a server the operator gave can carry one in its URL. A postgres:// or
# postgresql:// URL loses its password as pg_url_without_password (pg-url.sh, sourced above) takes
# it out, which reads the URL as libpq reads it. Then it loses each `sslpassword` parameter, which
# pg-url.sh keeps: the query starts at the first `?` past the user info, each key is
# percent-decoded as libpq decodes it (`%73slpassword=` is `sslpassword=`), the other parameters
# stay in their order, and a query that keeps none prints no `?`. For a URL pg-url.sh refuses,
# pg-url.sh names the fault on standard error, and safe_url prints a fixed placeholder, never the
# URL. Any other URL, the NATS one, is read as Go's net/url reads it, which nats.go parses a server
# URL with: the password runs to the last `@` before the path, the query or the fragment, so a raw
# `@` in it goes too.
safe_url() {
  local url scheme rest user='' kept='' param key
  local -a params=()
  case $1 in
  postgres://* | postgresql://*) ;;
  *)
    printf '%s\n' "$1" | sed -E 's,^([a-z]+://[^:/@]+):[^/?#]*@,\1@,'
    return
    ;;
  esac
  if ! url=$(pg_url_without_password "$1"); then
    printf '%s\n' '<a Postgres URL pg-url.sh refuses>'
    return
  fi
  scheme=${url%%://*} rest=${url#*://}
  if [[ ${rest%%/*} == *@* ]]; then
    user=${rest%%@*}@
    rest=${rest#*@}
  fi
  case $rest in *\?*)
    IFS='&' read -ra params <<<"${rest#*\?}"
    rest=${rest%%\?*}
    ;;
  esac
  for param in "${params[@]}"; do
    key=${param%%=*}
    printf -v key '%b' "${key//%/\\x}"
    [ "$key" = sslpassword ] || kept+="&$param"
  done
  printf '%s://%s%s%s\n' "$scheme" "$user" "$rest" "${kept:+?${kept#&}}"
}

# pg_sql URL SQL runs SQL on URL with psql and prints each row's values, one row a line, tabs
# between them. psql is libpq, so the host it connects to is the host require_loopback_dsn read
# from the same URL (services_up). pg_url_run (pg-url.sh, sourced above) gives psql the URL
# without its password, and the password in PGPASSWORD for that one command, so no process list
# shows it. psql never asks for a password (-w): a server that asks for one the URL does not hold
# fails at once, and does not wait on a terminal prompt that no time limit covers. A failure is
# psql's own: it prints what Postgres answered and exits non-zero. A URL pg-url.sh refuses runs
# no psql: pg-url.sh names the fault on standard error and returns 2.
# until_true polls this for readiness, so the connect carries a bound of its own
# (PGCONNECT_TIMEOUT), as nats_ready's probe does: a host that takes the connection and never
# answers holds no caller past five seconds.
pg_sql() {
  PGCONNECT_TIMEOUT=5 pg_url_run "$1" psql -X -q -w -v ON_ERROR_STOP=1 -A -t -F $'\t' -c "$2"
}

# nats_ready URL succeeds when a server answers on URL's host and port with the INFO line every
# NATS server sends on connect, so it tells a NATS from any other process holding the port. bash's
# own /dev/tcp opens the connection, in a child with a timeout of its own: the probe needs no
# client, and a host that swallows the connection cannot hold its caller past its bound.
nats_ready() {
  local hostport=${1#*://} line=''
  hostport=${hostport##*@}
  hostport=${hostport%%/*}
  line=$(timeout 5 bash -c 'exec 3<>"/dev/tcp/$1/$2" && IFS= read -r -t 5 -u 3 line && printf "%s" "$line"' \
    -- "${hostport%:*}" "${hostport##*:}" 2>/dev/null) || return 1
  case $line in 'INFO '*) ;; *) return 1 ;; esac
}

# connections_needed sets services_up's per_run, the connections one run's Dispatch server opens,
# which `store/cmd/connections -process` prints, and its needs, what parallel runs and the batch's
# own queries open at once: per_run for each run and 20 more. It fails when no count is printed.
connections_needed() {
  per_run=$(cd "$root/packages/envoy" && go run ./internal/dispatch/store/cmd/connections -process) &&
    [[ "$per_run" =~ ^[1-9][0-9]*$ ]] ||
    fail "store/cmd/connections -process printed no count of one Dispatch server's connections: $per_run"
  needs=$((per_run * parallel + 20))
}

# services_up brings up the batch's services: its Postgres and its NATS, the servers the operator
# gave (given_services) or two containers it starts, and the Envoy listener built from this
# checkout, which the services file ($services_env) names with the NATS and the listener token.
#
# Each run's Dispatch server opens at most store.ConnectionsPerProcess connections to the batch's
# Postgres: its counted pools and its permanent listeners (packages/envoy/internal/dispatch/store/
# pool.go), which `store/cmd/connections -process` prints, so this script holds no number of its
# own. Postgres admits 100 by default, too few for 5 servers at once, so a container this batch
# starts gets max_connections of that count for each concurrent run and 20 more for the batch's own
# queries. A given server's max_connections is the operator's, not the batch's, so services_up
# reads it and refuses a parallelism the server cannot serve. The count is read after a given
# URL's checks, so a refused URL builds nothing.
#
# Each container's id goes to $S/containers, and SKILL_SCENARIOS_OWNER labels it. Neither container
# publishes a host port. Docker would choose one among the ports it has not handed out itself,
# which another process of a shared host can hold, and `docker run` then fails with "address
# already in use" (a batch of reviewer-pass-and-fail ended so, before any run). NATS runs on the
# host's network and binds a loopback port its own bind chooses (`-p -1`), and the rig reads that
# port from its log, so nothing can take the port between the choice and the bind. Its container's
# address on the Docker network would be free of the race too, but the listener refuses a NATS
# server that is not this machine's (bus: ENVOY_ALLOW_REMOTE_NATS), and the rig turns that guard off
# nowhere. Postgres is reached through its unix socket, in a directory of the batch's own
# (pg_socket) that the container mounts: each run's Dispatch serves dev sign-in, which refuses a
# database host that is neither loopback nor a socket (loopbackDatabase,
# packages/envoy/cmd/dispatch/main.go). The container runs as this user, so the socket and its lock
# are this user's to remove.
#
# A given Postgres must be a postgres:// or postgresql:// URL, the one form safe_url and pg_sql take
# a password out of. It must hold no sslpassword parameter, a client key's passphrase: pg_sql takes
# only the password out of the URL it gives psql, and every user of this machine can read psql's
# arguments. Its query is read as safe_url reads it: from the first `?` past the user info, each
# key percent-decoded. Its URL is read with require_loopback_dsn (scripts/e2e/lib/loopback-dsn.sh)
# before anything connects to that server or this batch writes the URL anywhere. The guard reads
# the host with libpq, and pg_sql connects with psql, which is libpq, so the host psql reaches is
# the host the guard read. run_database_url then rebuilds, for a placeholder database, the
# connection string each run's Dispatch gets (own_dispatch), and refuses a value that string
# cannot carry, so such a URL is refused here, before anything starts, and not in every run.
services_up() {
  local owner=()
  evidence=$S
  local parallel=${SKILL_SCENARIOS_PARALLEL:-5} per_run needs admits fits nats_port pg_host run_url tool rest param key postgres
  local -a params=()
  if given_services; then
    case $SKILL_SCENARIOS_POSTGRES_URL in
    postgres://* | postgresql://*) ;;
    *) fail "SKILL_SCENARIOS_POSTGRES_URL is not a postgres:// or postgresql:// URL: the batch names the server in its notes and in each run's dispatch.json, and it can take a password out of a URL alone, never out of a key=value connection string" ;;
    esac
    rest=${SKILL_SCENARIOS_POSTGRES_URL#*://}
    [[ ${rest%%/*} != *@* ]] || rest=${rest#*@}
    case $rest in *\?*) IFS='&' read -ra params <<<"${rest#*\?}" ;; esac
    for param in "${params[@]}"; do
      key=${param%%=*}
      printf -v key '%b' "${key//%/\\x}"
      [ "$key" != sslpassword ] ||
        fail "SKILL_SCENARIOS_POSTGRES_URL holds an sslpassword parameter, a client key's passphrase, whose value this line does not print: pg_sql takes only the password out of the URL it gives psql, so psql's arguments, which every user of this machine can read, would hold the passphrase"
    done
    pg_url_without_password "$SKILL_SCENARIOS_POSTGRES_URL" >/dev/null ||
      fail "SKILL_SCENARIOS_POSTGRES_URL is a URL pg-url.sh refuses, for the reason on the line above: pg_sql and safe_url take its password out with pg-url.sh, so the batch could neither reach that server nor name it"
    for tool in psql python3; do
      command -v "$tool" >/dev/null ||
        fail "$tool is not on PATH: a batch given SKILL_SCENARIOS_POSTGRES_URL reads that URL with python3 and libpq, and reaches the server with psql"
    done
    require_loopback_dsn pg_host SKILL_SCENARIOS_POSTGRES_URL "$SKILL_SCENARIOS_POSTGRES_URL" "each run creates a database there" || fail "$pg_host"
    run_database_url run_url SKILL_SCENARIOS_POSTGRES_URL "$SKILL_SCENARIOS_POSTGRES_URL" run_placeholder "each run's Dispatch opens its database there" || fail "$run_url"
    connections_needed
    pg_url=$SKILL_SCENARIOS_POSTGRES_URL
    nats_url=$SKILL_SCENARIOS_NATS_URL
    (umask 077 && printf '%s\n' "$pg_url" >"$services_postgres")
    until_true 60 "Postgres at $(safe_url "$pg_url") to answer a query" pg_sql "$pg_url" 'select 1'
    until_true 60 "NATS at $(safe_url "$nats_url") to send its INFO line" nats_ready "$nats_url"
    admits=$(pg_sql "$pg_url" 'SHOW max_connections')
    fits=$(((admits - 20) / per_run))
    [ "$fits" -ge 1 ] ||
      fail "$(safe_url "$pg_url") admits $admits connections, and one run needs $((per_run + 20)): give a Postgres whose max_connections is $((per_run + 20)) or more"
    [ "$parallel" -le "$fits" ] ||
      fail "SKILL_SCENARIOS_PARALLEL is $parallel, so this batch needs $needs connections to $(safe_url "$pg_url"), which admits $admits: set SKILL_SCENARIOS_PARALLEL to $fits or lower"
    postgres=$(safe_url "$pg_url")
  else
    connections_needed
    # A given Postgres's URL that a batch killed outright left would send this batch's runs there.
    rm -f -- "$services_postgres"
    [ -z "${SKILL_SCENARIOS_OWNER:-}" ] || owner=(--label "owner=$SKILL_SCENARIOS_OWNER")
    # A unix socket's path holds at most 107 bytes, and a TMPDIR can be longer than that (a test's
    # or a session's own), so the directory is a new one of /tmp's.
    pg_socket=$(mktemp -d /tmp/rig-pg.XXXXXX)
    # ghcr.io/cybermonklabs/legion/ci, the copy of pgvector/pgvector (.github/ci-images.yaml)
    docker run -d --name "$tag-pg" "${owner[@]}" --user "$(id -u):$(id -g)" --cpus 2 --memory 2g \
      --mount type=tmpfs,destination=/var/lib/postgresql --mount "type=bind,source=$pg_socket,destination=/var/run/postgresql" \
      -e POSTGRES_PASSWORD=ci ghcr.io/cybermonklabs/legion/ci@sha256:2358fcba361ed2233a5ed81b5fe4ca779ccb304120ce531a3bf51c0ed7e2bc11 \
      -c max_connections=$needs >>"$S/containers"
    # The hub's nats-server (lib/nats-image.sh reads testnats.HubImage): the listener and the daemon
    # refuse a nats-server below the floor packages/envoy/natsversion holds, every 2.10 among them.
    # shellcheck source-path=SCRIPTDIR/../../../.. source=scripts/e2e/lib/nats-image.sh
    . "$root/scripts/e2e/lib/nats-image.sh"
    docker run -d --name "$tag-nats" "${owner[@]}" --network host --cpus 1 --memory 512m "$nats_image" -js -a 127.0.0.1 -p -1 >>"$S/containers"
    until_true "${SKILL_SCENARIOS_TIMEOUT:-1500}" "Postgres to accept TCP connections" docker exec "$tag-pg" pg_isready -h 127.0.0.1 -p 5432 -U postgres
    until_true "${SKILL_SCENARIOS_TIMEOUT:-1500}" "NATS to be ready" sh -c "docker logs '$tag-nats' 2>&1 | grep -q 'Server is ready'"
    nats_port=$(docker logs "$tag-nats" 2>&1 | sed -n 's/.*Listening for client connections on 127\.0\.0\.1:\([0-9][0-9]*\)$/\1/p' | head -1)
    [ -n "$nats_port" ] || fail "NATS logged no client port on 127.0.0.1; see docker logs $tag-nats"
    nats_url=nats://127.0.0.1:$nats_port
    postgres=$pg_socket
  fi
  (cd "$root/packages/envoy" && go build -o "$S/envoy-listener" ./cmd/listener)
  (umask 077 && openssl rand -hex 24 >"$S/envoy-token" && printf 'e2e-token\n' >"$S/dispatch-token")
  start_listener
  {
    echo "ENVOY_URL=http://127.0.0.1:$envoy_port"
    echo "ENVOY_NATS_URL=$nats_url"
    echo "ENVOY_TOKEN_FILE=$S/envoy-token"
  } >"$services_env"
  note "services up: listener :$envoy_port, NATS $(safe_url "$nats_url"), Postgres $postgres"
}

# services_down stops and removes what this batch started and made: the listener, the two
# containers services_up starts when it is given no servers and the directory of their Postgres's
# socket, the services file, the given Postgres's URL, and the lock. A given Postgres and a given
# NATS keep running, and every database a run made on that Postgres (own_dispatch) stays there for
# the operator to drop.
# shellcheck disable=SC2329 # stop_batch, an exit trap, runs it
services_down() {
  stop_tree "${listener_pid:-}"
  given_services || docker rm -fv "$tag-nats" "$tag-pg" >/dev/null 2>&1 || true
  [ -z "$pg_socket" ] || rm -rf -- "$pg_socket"
  rm -f "$services_env" "$services_postgres" "$S/up"
}

# own_dispatch SEED gives the run a Dispatch of its own and seeds it with `seed.ts SEED`, the world
# the scenario needs (project, or brainstorm): a database named after the run's directory in the
# batch's Postgres, and a Dispatch server on it, which stop_run stops. It records the Postgres, the
# database and the server in $R/dispatch.json, which score.ts reads, and sets dispatch_port. The
# batch's Postgres is the server $services_postgres names, and the batch's own container, reached
# through its socket, when that file is absent. On a given server, the run's Dispatch gets the
# connection string run_database_url (scripts/e2e/lib/loopback-dsn.sh) rebuilds from the given URL
# with libpq, with the run's database as its dbname: pgx, which Dispatch opens it with, reads that
# keyword/value form.
own_dispatch() {
  local seed=$1 database=run_${R##*/} postgres socket url
  database=${database//-/_}
  # Postgres cuts a longer name to 63 bytes, and two runs whose names agree that far would share it.
  [ "${#database}" -le 63 ] || fail "the run's database name $database is longer than Postgres's 63 bytes"
  if [ -f "$services_postgres" ]; then
    postgres=$(<"$services_postgres")
    run_database_url url SKILL_SCENARIOS_POSTGRES_URL "$postgres" "$database" "this run creates a database there" || fail "$url"
    # A given server must let its login make a database. Nothing here checks that: the refusal a
    # server without the right gives is Postgres's own, in the run's fixture.log.
    pg_sql "$postgres" "CREATE DATABASE $database" >>"$R/fixture.log" 2>&1 ||
      fail "CREATE DATABASE $database on $(safe_url "$postgres") failed; see $R/fixture.log"
    postgres=$(safe_url "$postgres")
  else
    # The container's id, and the directory of its socket that services_up mounted; an inspect that
    # answers nothing leaves both empty for the refusal below.
    read -r postgres socket < <(docker inspect -f \
      '{{.Id}} {{range .Mounts}}{{if eq .Destination "/var/run/postgresql"}}{{.Source}}{{end}}{{end}}' "$tag-pg") || true
    [ -n "$postgres" ] && [ -n "$socket" ] || fail "no Postgres container $tag-pg with its socket for the run's Dispatch"
    docker exec "$tag-pg" psql -U postgres -v ON_ERROR_STOP=1 -qc "CREATE DATABASE $database" >>"$R/fixture.log" 2>&1 ||
      fail "CREATE DATABASE $database failed; see $R/fixture.log"
    url="postgres:///$database?host=$socket&user=postgres&sslmode=disable"
  fi
  start_dispatch "$url"
  jq -n --arg postgres "$postgres" --arg database "$database" --argjson port "$dispatch_port" \
    '{postgres:$postgres, database:$database, port:$port, url:("http://127.0.0.1:" + ($port|tostring))}' >"$R/dispatch.json"
  (cd "$root/packages/dispatch" && DISPATCH_E2E_PORT=$dispatch_port bun "$here/seed.ts" "$seed") >>"$R/fixture.log" 2>&1 ||
    fail "seed.ts $seed failed; see $R/fixture.log"
}

# own_github gives a run of a heads scenario a NATS and a GitHub of its own, which stop_run stops,
# so its events and its fixture stay apart from the other runs of its batch (`PUT
# /__fixture/repos` replaces every repository the stand-in holds): the batch's build of
# packages/envoy's testnats natsd ($work/services/bin/natsd, cmd_batch), its store under
# $R/nats-store, and packages/dispatch/e2e/fake-github.ts. Each binds a port the kernel picks and
# logs it, and the port is read from that line, as scripts/e2e/verification-results.sh reads
# natsd's and e2e/harness-start.ts reads the stand-in's. The stand-in's FAKE_GITHUB_PORT is empty
# for that: harness-ports.ts takes an empty one as port 0 and refuses a written 0. Sets
# run_nats_url and github_port, which start_dispatch and seed_ts read.
own_github() {
  local log=$evidence/logs
  start_process natsd "$work/services/bin/natsd" -p -1 -store_dir "$R/nats-store"
  await_start natsd "$natsd_pid" "${SKILL_SCENARIOS_TIMEOUT:-1500}" "natsd to be ready" grep -q 'Server is ready' "$log/natsd.log"
  run_nats_url=nats://127.0.0.1:$(report_port "$log/natsd.log" 'Listening for client connections on [^ ]*:') ||
    fail "natsd logged no client port; see $log/natsd.log"
  FAKE_GITHUB_PORT='' start_process fake_github bun "$root/packages/dispatch/e2e/fake-github.ts"
  await_start fake_github "$fake_github_pid" "${SKILL_SCENARIOS_TIMEOUT:-1500}" "the GitHub stand-in to report its port" \
    report_port "$log/fake_github.log" 'fake github listener on 127\.0\.0\.1:'
  github_port=$(report_port "$log/fake_github.log" 'fake github listener on 127\.0\.0\.1:')
}

# seed_dispatch SCENARIO seeds the run's own issue, and the owner's message when the scenario has
# one, with `seed.ts SCENARIO`, and sets the caller's issue and message (empty when the fixture
# names none), which its prompt names.
seed_dispatch() {
  (cd "$root/packages/dispatch" && DISPATCH_E2E_PORT=$dispatch_port bun "$here/seed.ts" "$1" "$R/fixture.json") \
    >>"$R/fixture.log" 2>&1 || fail "the fixture failed; see $R/fixture.log"
  issue=$(jq -r .issue "$R/fixture.json")
  message=$(jq -r '.message // empty' "$R/fixture.json")
}

# dispatch_session [CAPTURE [VAR=VALUE...]] runs $R/prompt.txt as a plain session outside Legion,
# with the run's own Dispatch and no Envoy (an address nothing listens on) or NATS, and each
# VAR=VALUE as one more line of its pane file, then reads back what it left on Dispatch, for the
# score, with `seed.ts CAPTURE` (default `capture`).
dispatch_session() {
  local capture=${1:-capture}
  shift $(($# < 1 ? $# : 1))
  base_env
  standins
  {
    echo "DISPATCH_URL=http://127.0.0.1:$dispatch_port"
    echo "DISPATCH_TOKEN_FILE=$work/services/dispatch-token"
    echo "ENVOY_URL=http://127.0.0.1:1"
  } >>"$R/pane.env"
  [ $# = 0 ] || printf '%s\n' "$@" >>"$R/pane.env"
  launch
  (cd "$root/packages/dispatch" && DISPATCH_E2E_PORT=$dispatch_port bun "$here/seed.ts" "$capture" "$R/fixture.json" "$R") \
    >>"$R/fixture.log" 2>&1 || fail "the capture failed; see $R/fixture.log"
}

run_ask_on_message() {
  local issue message
  own_dispatch project
  seed_dispatch ask-on-message
  printf 'On Dispatch issue %s, the Dispatch owner session posted its plan for the rest of the work in this message: dispatch://%s/message/%s. Open an ask on %s asking Sami whether to go ahead with that plan. Do nothing else.\n' \
    "$issue" "$issue" "$message" "$issue" >"$R/prompt.txt"
  dispatch_session
}

# The export the owner's message names, in the agent's working directory: 430 stranded issues, of
# which only every 23rd (18) has the owner option A needs. Its example.invalid addresses give an
# agent a second reason to drop A; measureBeforeAsk says how the first runs split on it.
run_measure_before_ask() {
  local issue message
  own_dispatch project
  seed_dispatch measure-before-ask
  awk 'BEGIN { print "key,title,component,owner"
    for (n = 1; n <= 430; n++) printf "ISS-%d,Stranded issue %d,billing-api,%s\n", 1000 + n, n, (n % 23 == 0 ? "owner" n "@example.invalid" : "") }' \
    >"$R/cwd/stranded.csv"
  printf 'On Dispatch issue %s, the Dispatch owner session posted three ways to finish its work in this message: dispatch://%s/message/%s. The export it names, stranded.csv, is in your working directory. Open an ask on %s asking Sami which way to go. Change nothing else.\n' \
    "$issue" "$issue" "$message" "$issue" >"$R/prompt.txt"
  dispatch_session
}

# The person's words are one comment on the run's own issue, and AskGate reviews each Dispatch
# write the agent makes. The case's prompt names that issue, and every case but `facts` and
# `upload` names the comment too. `link` asks a choice the comment leaves open, so only AskGate's
# rule 10 (the person's words with no link to them) bears on it: an ask whether to do what the
# person asked is authority 6 (applying a decision they already gave). Its reindex fact cites the
# file it comes from, export-host.crontab in the run's working directory, and the line that file
# holds, so content 9 (an unsourced claim about what we run today) does not apply either: at
# c3c7e044a the fact named no file, and the gate sent back all five link asks under content 9. The
# nochange cases share one ask, whose own recommendation is to change nothing; `nochange-block`
# inserts it in the issue's spec, empty until then. `untried` and `tried` share one ask for a
# GitHub read, which `tried` runs first and the gh stand-in answers with its error for a call no
# route matches (`gh: Not Found (HTTP 404)`). Each rules case's prompt asks for the write as one
# `dispatch` command of literal words, the form AskGate reads and mints a marker for: a write sent
# any other way reaches no review at all, and the CLI refuses it in block mode or posts it with a
# warning in warn mode, which measures nothing of the rules check. A set
# SKILL_SCENARIOS_ASKGATE_MODE is the pane's OMP_ASKGATE and <run>/mode.txt; unset, the pane gets
# no OMP_ASKGATE, so AskGate's verdicts block and its marker checks warn.
run_askgate() {
  local askCase=${SKILL_SCENARIOS_ASKGATE_CASE:-} mode=${SKILL_SCENARIOS_ASKGATE_MODE:-} issue message ref ask billing one
  case $mode in '' | warn | block) ;; *) fail "SKILL_SCENARIOS_ASKGATE_MODE is '$mode', not warn or block" ;; esac
  [ -z "$mode" ] || printf '%s\n' "$mode" >"$R/mode.txt"
  own_dispatch project
  seed_dispatch askgate
  ref=$(jq -r .comment_ref "$R/fixture.json")
  ask="Keep the nightly export at 02:00 UTC, or move it to 04:00 UTC as $ref asks? I recommend keeping it at 02:00 as it is."
  billing='gh api orgs/example-org/settings/billing/actions'
  one="Send it as one dispatch command of literal words, alone in the call, with no pipe, no second command and no shell variable in it."
  # shellcheck disable=SC2016 # each prompt is literal text; its Markdown backticks expand nothing
  case $askCase in
  facts) printf 'Open an ask on Dispatch issue %s with exactly this question and these options, and nothing else. Question: "Which branch should the release build from? The repository is nine years old and has 214 contributors." Options: "main", "release".\n' "$issue" ;;
  link)
    printf '# m h dom mon dow command\n0 2 * * * /opt/export/nightly-export\n0 4 * * 0 /opt/search/reindex --all\n' >"$R/cwd/export-host.crontab"
    printf 'Open an ask on Dispatch issue %s with exactly this question and these options, and nothing else. Question: "export-host.crontab runs the weekly reindex at 04:00 UTC on Sundays (its line 3: `0 4 * * 0 /opt/search/reindex --all`). Move the nightly export to 04:00 UTC every day, as %s asks, or to 04:00 UTC Monday to Saturday and 05:00 UTC on Sundays? I recommend 05:00 on Sundays." Options: "04:00 every day", "05:00 on Sundays".\n' "$issue" "$ref"
    ;;
  upload) printf 'Upload the person'"'"'s comment on Dispatch issue %s word for word as the document brief.md, with dispatch artifact --force, so that you can link it. Then open an ask that links brief.md and asks whether to move the nightly export.\n' "$issue" ;;
  nochange) printf 'Open an ask on Dispatch issue %s with exactly this question and these options, and nothing else. %s Question: "%s" Options: "Keep it at 02:00", "Move it to 04:00".\n' "$issue" "$one" "$ask" ;;
  nochange-rebuttal) printf 'Open an ask on Dispatch issue %s with exactly this question and these options, and nothing else, and send it with the flag --advisor-rebuttal whose value is this line: "The person asked for the move, so keeping 02:00 needs their authority." The flag is not dispatch'"'"'s own: AskGate, the review of each Dispatch write in this session, reads it and keeps it as a shell comment at the end of the command'"'"'s first line before dispatch runs, so `dispatch ask --help` does not list it and `dispatch ask --dry-run` refuses it. Send the ask once, with the flag. %s Question: "%s" Options: "Keep it at 02:00", "Move it to 04:00".\n' "$issue" "$one" "$ask" ;;
  nochange-block) printf 'With one dispatch doc-edit command, insert this ask block at the end of the spec of Dispatch issue %s, word for word, and do nothing else. Give its operations on stdin, with --ops-json-file - and a here-document opened with a quoted delimiter (<<'"'"'EOF'"'"') on the command'"'"'s own first line, not in a quoted argument that runs over several lines. %s\n\n:::ask{#export-time urgency="med"}\n%s\n\n- Keep it at 02:00\n- Move it to 04:00\n:::\n' "$issue" "$one" "$ask" ;;
  untried) printf 'Open an ask on Dispatch issue %s with exactly this question and these options, and nothing else. Do not run the command it names yourself. %s Question: "Can you run %s and paste its output here? Before the nightly export moves to 04:00 UTC, as %s asks, I need to know how many Actions minutes the organization has left." Options: "Done", "Cannot run it".\n' "$issue" "$one" "$billing" "$ref" ;;
  tried) printf 'First run %s yourself. It will fail. Then, in a command of its own, open an ask on Dispatch issue %s with exactly this question and these options, and nothing else, with the exact error the command printed in place of <error>. %s Question: "I ran %s and it failed with <error>. Can you run it with your access and paste its output here? Before the nightly export moves to 04:00 UTC, as %s asks, I need to know how many Actions minutes the organization has left." Options: "Done", "Cannot run it".\n' "$billing" "$issue" "$one" "$billing" "$ref" ;;
  *) fail "run_askgate has no prompt for the askgate case '$askCase', which askgate-cases.txt lists" ;;
  esac >"$R/prompt.txt"
  dispatch_session capture-askgate ${mode:+"OMP_ASKGATE=$mode"}
}

# A tiny to-do CLI in a scratch git repository with no Dispatch configuration and no word of due
# dates. The run's own Dispatch holds one project (seed.ts brainstorm: TODO, with two unrelated
# issues whose titles name the "todo CLI", so a search for it finds the project), and
# `seed.ts capture-brainstorm` reads back the issue this run's own transcript created, for
# brainstormSurface.
run_brainstorm() {
  own_dispatch brainstorm
  cat >"$R/cwd/todo.py" <<'PYEOF'
"""A tiny command-line to-do list that keeps its tasks in tasks.json next to this file."""

import json
import sys
from pathlib import Path

STORE = Path(__file__).with_name("tasks.json")


def load() -> list[dict]:
    if not STORE.exists():
        return []
    return json.loads(STORE.read_text())


def save(tasks: list[dict]) -> None:
    STORE.write_text(json.dumps(tasks, indent=2) + "\n")


def main(argv: list[str]) -> int:
    if not argv:
        print("usage: todo.py add <text> | list | done <number>")
        return 2
    tasks = load()
    command, rest = argv[0], argv[1:]
    if command == "add":
        tasks.append({"text": " ".join(rest), "done": False})
        save(tasks)
    elif command == "list":
        for number, task in enumerate(tasks, start=1):
            mark = "x" if task["done"] else " "
            print(f"{number}. [{mark}] {task['text']}")
    elif command == "done":
        tasks[int(rest[0]) - 1]["done"] = True
        save(tasks)
    else:
        print(f"unknown command {command}")
        return 2
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
PYEOF
  cat >"$R/cwd/README.md" <<'MDEOF'
# todo

A tiny command-line to-do list for one person.

```sh
python todo.py add "buy milk"
python todo.py list
python todo.py done 1
```

Tasks live in `tasks.json` beside `todo.py`.
MDEOF
  git -C "$R/cwd" init -q
  git -C "$R/cwd" add -A
  git -C "$R/cwd" -c user.name=rig -c user.email=rig@example.invalid -c commit.gpgsign=false \
    commit -qm "A tiny to-do CLI"
  # shellcheck disable=SC2016 # the backticks are the prompt's own, a command name for the agent
  printf '/brainstorming a small feature: let a task in this to-do CLI carry an optional due date, and have `list` show overdue tasks first.\n' \
    >"$R/prompt.txt"
  dispatch_session capture-brainstorm
}

# seed_ts MODE ARG... runs `seed.ts MODE ARG...` against the run's own Dispatch (own_dispatch), and
# a heads run's own GitHub stand-in and NATS (own_github), its output appended to $R/fixture.log. A
# failure is the rig's, never the run's.
seed_ts() {
  (cd "$root/packages/dispatch" && DISPATCH_E2E_PORT=$dispatch_port FAKE_GITHUB_PORT=${github_port:-} \
    DISPATCH_E2E_NATS_URLS=${run_nats_url:-} bun "$here/seed.ts" "$@") \
    >>"$R/fixture.log" 2>&1 || fail "seed.ts $1 failed; see $R/fixture.log"
}

# judge_ts MODE ARG... runs `judge.ts MODE ARG...` with SKILL_SCENARIOS_WORK at this rig's work
# directory, its output appended to $R/judge.log.
judge_ts() {
  SKILL_SCENARIOS_WORK=$work bun "$here/judge.ts" "$@" >>"$R/judge.log" 2>&1 ||
    fail "judge.ts $1 failed; see $R/judge.log"
}

# next_run SCENARIO LABEL prints one above the highest <n> of a run the map names
# $maps/runs/<SCENARIO>-<LABEL>-<n>, or 1 when there is none.
next_run() {
  local dir n highest=0
  for dir in "$maps/runs/$1-$2-"*; do
    [ -d "$dir" ] || continue
    n=${dir##*/"$1-$2-"}
    [[ $n =~ ^[0-9]+$ ]] || continue
    ((10#$n <= highest)) || highest=$((10#$n))
  done
  printf '%s\n' "$((highest + 1))"
}

# source_dir SCENARIO NAME prints the directory of the source NAME under $work/sources, and fails,
# naming what is missing, when NAME is unset or not one directory name, or the source lacks a file
# seed.ts reads: title.txt, messages.md and asks.json, and old-spec.md for spec-rewrite.
source_dir() {
  local name=$2 dir file files=(title.txt messages.md asks.json)
  [ -n "$name" ] || fail "SKILL_SCENARIOS_SOURCE is unset: name a directory under $work/sources (fetch-spec-sources.ts writes them)"
  [[ $name =~ ^[A-Za-z0-9][A-Za-z0-9._-]*$ ]] || fail "SKILL_SCENARIOS_SOURCE '$name' is not a directory name under $work/sources"
  dir=$work/sources/$name
  [ -d "$dir" ] || fail "SKILL_SCENARIOS_SOURCE $name: $dir does not exist (fetch-spec-sources.ts writes it)"
  [ "$1" != spec-rewrite ] || files+=(old-spec.md)
  for file in "${files[@]}"; do
    [ -f "$dir/$file" ] || fail "the source $dir has no $file, which $1 needs"
  done
  printf '%s\n' "$dir"
}

# require_superpowers SCENARIO LABEL fails unless LABEL's profile (P) was built with
# SKILL_SCENARIOS_SUPERPOWERS.
require_superpowers() {
  [ -f "$P/superpowers" ] ||
    fail "$1 needs a label built with SKILL_SCENARIOS_SUPERPOWERS (the superpowers plugin's directory): remove $P and run \`rig.sh profile $2 <checkout>\` again with it set"
}

# spec_turns NAME ISSUE PROMPT runs a spec run's conversation (the header's "A spec run"): the turn
# loop, at most 8 turns. It reads the open asks and the approval request from each turn's
# state-<turn>.json, which `seed.ts state` writes, and it fails the run when the session directory
# holds more than one transcript after a turn. A turn that does not exit 0 ends the run.
spec_turns() {
  local name=$1 issue=$2 turn=0 kind=first last transcripts
  local answered="The person answered your asks on $issue." approved="The person approved the spec of $issue."
  printf '%s\n' "$3" >"$R/prompt.txt"
  : >"$R/answers.jsonl"
  : >"$R/turns.txt"
  while :; do
    turn=$((turn + 1))
    if [ "$turn" = 1 ]; then launch; else launch --continue; fi
    cp "$R/out.txt" "$R/out-$turn.txt"
    last=$(tail -n 1 "$R/out.txt")
    printf '%s %s %s\n' "$turn" "$kind" "$last" >>"$R/turns.txt"
    transcripts=$(find "$R/sessions" -maxdepth 1 -name '*.jsonl' | wc -l)
    [ "$transcripts" -le 1 ] || fail "after turn $turn, $R/sessions holds $transcripts transcripts: --continue did not append to the run's one transcript"
    seed_ts state "$R/fixture.json" "$turn" "$R"
    [ "$kind" != approved ] && [ "$last" = exit=0 ] && [ "$turn" -lt 8 ] || break
    if [ "$(jq -r '.approval != null' "$R/state-$turn.json")" = true ]; then
      seed_ts approve "$R/fixture.json"
      kind=approved
      printf '%s\n' "$approved" >"$R/prompt.txt"
    elif [ "$(jq '.open_asks | length' "$R/state-$turn.json")" != 0 ]; then
      judge_ts answer "$R" "$turn"
      cat "$R/answers-$turn.jsonl" >>"$R/answers.jsonl"
      seed_ts answer "$R/fixture.json" "$R/answers-$turn.jsonl"
      kind=answered
      printf '%s\n' "$answered" >"$R/prompt.txt"
    else
      break
    fi
  done
  note "$name: $turn turns, the last $last"
}

# run_spec SCENARIO NAME LABEL: a spec-writing or spec-rewrite run on the source
# SKILL_SCENARIOS_SOURCE names. The source goes to $R/source/ and its name to $R/source.txt. seed.ts
# seeds the run's issue from it, spec_turns runs the conversation, and `seed.ts capture-spec` reads
# back what the run wrote. The prompts name no skill.
run_spec() {
  local scenario=$1 name=$2 label=$3 source issue prompt
  source=$(source_dir "$scenario" "${SKILL_SCENARIOS_SOURCE:-}") || exit 1
  require_superpowers "$scenario" "$label"
  cp -R "$source" "$R/source"
  printf '%s\n' "${source##*/}" >"$R/source.txt"
  own_dispatch project
  seed_ts "$scenario" "$R/fixture.json" "$R/source"
  issue=$(jq -r .issue "$R/fixture.json")
  case $scenario in
  spec-writing) prompt="On Dispatch issue $issue, the people's messages describe a change. Design it with them." ;;
  spec-rewrite) prompt="On Dispatch issue $issue, the newest message asks for a change. Make it." ;;
  esac
  base_env
  standins
  {
    echo "DISPATCH_URL=http://127.0.0.1:$dispatch_port"
    echo "DISPATCH_TOKEN_FILE=$work/services/dispatch-token"
    echo "ENVOY_URL=http://127.0.0.1:1"
  } >>"$R/pane.env"
  spec_turns "$name" "$issue" "$prompt"
  seed_ts capture-spec "$R/fixture.json" "$R"
}

# The judge's two profiles, under $work/judge-profiles/<kind>, each under a HOME of its own with the
# model route install_model_route installs and no plugin. `ste` holds one user skill, this
# checkout's skills/asd-ste100. `bare` holds none. The first judge-omp call builds both, and every
# other call waits on the same lock, so parallel calls are safe.
# A judge's HOME is only the .omp directory the route installers require, not make_omp_home's: no
# Oh My Pi and no jj runs under it on this host. cmd_judge_omp copies the profile's agent directory
# into the container's own HOME and mounts no native cache, so make_omp_home's natives link and jj
# overlay would reach no judge, while its read of the operator's jj config would make judge-omp
# fail on a machine with no jj on PATH.
judge_profiles() {
  local dir=$work/judge-profiles kind P commit
  mkdir -p "$dir"
  exec 9>"$dir.lock"
  flock 9
  if [ ! -f "$dir/ready" ]; then
    on_exit "$(printf 'drop_unready_keys %q %q %q' "$dir/ready" "$dir/ste/model-key" "$dir/bare/model-key")"
    [ -f "$root/skills/asd-ste100/SKILL.md" ] || fail "$root has no skills/asd-ste100, the judge's one skill"
    commit=$(checkout_commit "$root") || fail "cannot read the commit of $root with jj or git"
    for kind in ste bare; do
      P=$dir/$kind
      rm -rf "$P"
      mkdir -p "$P/home/.omp"
      install_model_route "$root" "judge-$kind" "$P" >"$P/install.log" 2>&1 ||
        fail "the judge's model route (${LEGION_E2E_MODEL_ROUTE:-gateway}) could not be installed; see $P/install.log"
    done
    mkdir -p "$dir/ste/home/.omp/profiles/judge-ste/agent/skills"
    cp -RL "$root/skills/asd-ste100" "$dir/ste/home/.omp/profiles/judge-ste/agent/skills/asd-ste100"
    cp "$root/.omp-pin" "$dir/pin"
    {
      printf 'model %s\nthinking %s\npin %s\n' "$judge_model" "$judge_thinking" "$(<"$dir/pin")"
      printf 'commit %s\n' "$commit"
      printf 'asd-ste100 %s\n' "$(cd "$root/skills/asd-ste100" && find . -type f -print0 | LC_ALL=C sort -z | xargs -0 sha256sum | sha256sum | cut -d' ' -f1)"
    } >"$dir/built-from"
    touch "$dir/ready"
    trap - EXIT HUP INT TERM
  fi
  exec 9>&-
}

# judge_keys P AGENT KEYS writes into KEYS (0700) what one judge's container needs to reach the
# model, for the route LEGION_E2E_MODEL_ROUTE names, and nothing any other judge shares writably:
#   anthropic-key  a key command that prints the key, run in the container
#   models.yml     the profile's models.yml, naming /opt/key/anthropic-key in place of the host's
#                  key command
# key-file: the key command reads the key file on each call (the file is mounted read-only at its
# own path), so a key the operator rotates mid-run reaches the judge. It records no call: the
# host's key command appends to a call record every judge of the profile would share writably.
# gateway: the host's key command runs hawk-token, which needs the operator's home and session
# bus, so it runs here, once. Its token goes to KEYS/token (0600), which the key command prints.
# That key command serves a kept token until shortly before it expires, so a token that would
# expire before the judge's deadline is dropped and minted once more. Prints the key file's path
# on the key-file route, for the caller to mount.
# provider: no key command. KEYS/models.yml is the profile's own, whose `!cat <file>` the judge runs
# in its container; this prints the directory of that file, which the caller mounts read-only at
# its own path (the directory rather than the file, so a key the operator renews by writing a new
# file in its place still reaches the judge).
judge_keys() {
  local P=$1 agent=$2 keys=$3 host_command token exp deadline payload key_file read_from
  (umask 077 && mkdir -p -- "$keys") || fail "judge-omp: cannot make $keys"
  if [ "${LEGION_E2E_MODEL_ROUTE:-gateway}" = provider ]; then
    key_file=$(provider_key_file "$agent/models.yml")
    case $key_file in /*) ;; *) fail "judge-omp: $agent/models.yml does not read its key as \"!cat <absolute file>\"" ;; esac
    case $key_file in *[!A-Za-z0-9/._-]*) fail "judge-omp: the key file's path '$key_file' holds a character a mount cannot name plainly" ;; esac
    cp -- "$agent/models.yml" "$keys/models.yml"
    dirname -- "$key_file"
    return 0
  fi
  if [ "${LEGION_E2E_MODEL_ROUTE:-gateway}" = key-file ]; then
    host_command=$P/model-key/anthropic-key
    [ -f "$host_command" ] || fail "judge-omp: the key-file profile $P has no key command $host_command"
    key_file=$(key_command_file "$host_command")
    [ -f "$key_file" ] || fail "judge-omp: the key command names '$key_file', which is not a file"
    case $key_file in *[!A-Za-z0-9/._-]*) fail "judge-omp: the key file's path '$key_file' holds a character a key command cannot name plainly" ;; esac
    read_from=$key_file
  else
    host_command=$P/key/hawk-token
    [ -x "$host_command" ] || fail "judge-omp: the gateway profile $P has no key command $host_command"
    token=$("$host_command" 2>/dev/null) && [ -n "$token" ] ||
      fail "judge-omp: the gateway's key command gave no key; see $P/key/hawk-token.log"
    deadline=$((EPOCHSECONDS + ${SKILL_SCENARIOS_TIMEOUT:-1500}))
    payload=${token#*.}
    payload=${payload%%.*}
    payload=$(printf '%s' "$payload" | tr '_-' '/+')
    while [ $((${#payload} % 4)) != 0 ]; do payload+='='; done
    exp=$(printf '%s' "$payload" | base64 -d 2>/dev/null | jq -r '.exp // empty | numbers | floor' 2>/dev/null) || exp=
    if [ -n "$exp" ] && [ "$exp" -lt "$deadline" ]; then
      rm -f -- "$P/cache/hawk-token.key"
      token=$("$host_command" 2>/dev/null) && [ -n "$token" ] ||
        fail "judge-omp: the gateway's key command gave no key; see $P/key/hawk-token.log"
    fi
    (umask 077 && printf '%s\n' "$token" >"$keys/token")
    read_from=/opt/key/token
  fi
  # $(cat) drops trailing newlines and keeps a key whose file has none, as the host's key command
  # does with $(<file).
  (umask 077 && printf '#!/bin/sh\nkey=$(cat %s) && [ -n "$key" ] && printf "%%s\\n" "$key"\n' "$read_from" >"$keys/anthropic-key")
  chmod 0700 "$keys/anthropic-key"
  sed "s#!exec $host_command\"#!exec /opt/key/anthropic-key\"#g" "$agent/models.yml" >"$keys/models.yml"
  grep -q '!exec /opt/key/anthropic-key"' "$keys/models.yml" && ! grep -qF "$host_command" "$keys/models.yml" ||
    fail "judge-omp: $agent/models.yml does not name the key command $host_command"
  [ -z "${key_file:-}" ] || printf '%s\n' "$key_file"
}

# cmd_judge_omp KIND DIR OUTPUT runs one fresh judge session in DIR on DIR/prompt.txt: no plugins,
# the KIND profile's skills alone (ste: asd-ste100; bare: none), the judge's pinned model and
# thinking level, its transcript under DIR/sessions, its output to DIR/out.txt with a last line
# exit=<n>. It exits with omp's status.
#
# The judge runs in a Docker container of judge_image, as the operator's uid, and sees only what it
# needs. A judge with the whole filesystem read the run's transcript and spec versions when its
# directory held no input, and returned rows it made up from them. Mounted:
#   DIR                     read-only, so the judge reads its inputs and nothing beside them
#   DIR/OUTPUT, DIR/sessions  writable: its one answer file and its transcript
#   the pinned omp's install read-only (SKILL_SCENARIOS_JUDGE_OMP names another binary, for tests)
#   the profile's agent directory read-only, copied into a fresh tmpfs HOME at start, where Oh My
#     Pi writes its databases and logs, and extracts its own native addon (the operator's native
#     cache is not mounted: other sessions prune it, and a judge that found its version gone failed
#     to start)
#   the judge's own key directory read-only at /opt/key (judge_keys): a key command and a
#     models.yml that names it, and on the gateway route the token. It sits beside DIR, is the
#     owner's alone (0700, the token 0600), and is removed when the judge ends. On the key-file
#     route the key file is mounted read-only too, and the key command reads it on each call. On the
#     provider route the directory of the file the block's `!cat` reads is mounted read-only.
#     Nothing writable is shared between judges: one judge cannot leave a file for another to read.
#   on the gateway and provider routes, the endpoint's host name resolved here, since a container
#     cannot resolve a tailnet name itself
# The network is Docker's default bridge, for the model's endpoint. The container gets 2 CPUs and
# 4 GiB, so the judges of a busy batch cannot take the machine. SKILL_SCENARIOS_JUDGE_ENV adds
# `NAME=value` words to the container's environment, for tests.
#
# Each Docker call waits on the daemon, which a loaded box stalls (on 20 November the seven rows of
# a test run that start a judge failed inside one stall of the daemon), so each has a bound of its
# own (judge_docker):
# SKILL_SCENARIOS_DOCKER_TIMEOUT for the image's inspect and for each pull, and that plus omp's own
# SKILL_SCENARIOS_TIMEOUT for the run. A call past its bound ends the judge at once with a refusal
# that names the call and its bound: no further call, and no further pull after a pull its bound
# ended, waits on a daemon that stalled, so a stalled daemon costs a judge one call's bound, not the
# sum of every call's. A stall shorter than the bound costs only time.
#
# judge_docker SECONDS DOCKER-ARG... runs `docker DOCKER-ARG...` and kills it at SECONDS: the
# client of a stalled daemon may ignore a TERM (`docker run` hands it to its container and waits on).
# The kill reaches the client alone (`--foreground`): a timeout that kills its own process group
# kills itself too, and bash then writes a `Killed` line into the judge's out.txt. It returns
# docker's status, and sets docker_late to 1 when the bound ended the call (killed, and SECONDS
# gone), else to nothing. The run's `--rm` is the daemon's own, so a container whose client was
# killed is removed once it ends, and the omp in it ends at its own bound.
judge_docker() {
  local seconds=$1 started=$EPOCHSECONDS code=0
  shift
  timeout --foreground -s KILL "$seconds" docker "$@" || code=$?
  docker_late=
  [ "$code" != 137 ] || [ $((EPOCHSECONDS - started)) -lt "$seconds" ] || docker_late=1
  return "$code"
}

cmd_judge_omp() {
  [ $# = 3 ] || fail "usage: rig.sh judge-omp <ste|bare> <dir> <output file name>"
  local kind=$1 dir output=$3 skills status=0 omp P agent keys key_file h=/home/judge docker_late=''
  local docker_timeout=${SKILL_SCENARIOS_DOCKER_TIMEOUT:-300} omp_timeout=${SKILL_SCENARIOS_TIMEOUT:-1500}
  case $kind in ste) skills=(--skills asd-ste100) ;; bare) skills=(--no-skills) ;; *) fail "judge-omp: '$kind' is ste or bare" ;; esac
  dir=$(cd "$2" && pwd -P) || fail "judge-omp: $2 is not a directory"
  [ -f "$dir/prompt.txt" ] || fail "judge-omp: $dir has no prompt.txt"
  case $output in
  "" | . | .. | */* | prompt.txt | sessions | out.txt | judge-model.txt | .tmp) fail "judge-omp: '$output' is not the name of an answer file in $dir" ;;
  esac
  case "$(omp_ancestor "$dir" || true)" in "") ;; *) fail "judge-omp: $dir is under $(omp_ancestor "$dir"), whose plugins Oh My Pi would load" ;; esac
  case ${LEGION_E2E_MODEL_ROUTE:-gateway} in gateway | key-file | provider) ;; *) fail "judge-omp: LEGION_E2E_MODEL_ROUTE is '$LEGION_E2E_MODEL_ROUTE': it is gateway, key-file or provider" ;; esac
  [[ $docker_timeout =~ ^[1-9][0-9]*$ ]] ||
    fail "judge-omp: SKILL_SCENARIOS_DOCKER_TIMEOUT is '$docker_timeout', not a whole number of seconds"
  [[ $omp_timeout =~ ^[1-9][0-9]*$ ]] || fail "judge-omp: SKILL_SCENARIOS_TIMEOUT is '$omp_timeout', not a whole number of seconds"
  command -v docker >/dev/null || fail "judge-omp: docker is not on PATH; each judge runs in a container"
  judge_profiles
  P=$work/judge-profiles/$kind
  agent=$P/home/.omp/profiles/judge-$kind/agent
  local route=() host ip
  keys=$dir.key
  rm -rf -- "$keys"
  on_exit "$(printf 'rm -rf -- %q' "$keys")"
  key_file=$(judge_keys "$P" "$agent" "$keys") || exit 1
  route=(-v "$keys:/opt/key:ro")
  [ -z "$key_file" ] || route+=(-v "$key_file:$key_file:ro")
  # The judge's container resolves the model endpoint's hostname itself, and a tailnet name the
  # host resolves is often not resolvable inside Docker, so the host's answer is passed in. This
  # once skipped the key-file route, which reached Anthropic's own public name and needed none;
  # since that route takes LEGION_E2E_MODEL_BASE_URL it can name a gateway like any other, and a
  # route left out here would let the scenario agents reach it while its judges could not.
  host=$(sed -n 's#^ *"\{0,1\}baseUrl"\{0,1\}: *"\{0,1\}https\{0,1\}://\([^/:"]*\).*#\1#p' "$agent/models.yml" | head -1)
  ip=$(getent hosts "$host" 2>/dev/null | awk '{print $1; exit}') || ip=
  [ -z "$ip" ] || [ "$ip" = "$host" ] || route+=(--add-host "$host:$ip")
  printf 'model %s\nthinking %s\n' "$judge_model" "$judge_thinking" >"$dir/judge-model.txt"
  mkdir -p "$dir/sessions"
  : >>"$dir/$output"
  omp=${SKILL_SCENARIOS_JUDGE_OMP:-$(mise where "$(<"$work/judge-profiles/pin")")/bin/omp}
  [ -x "$omp" ] || fail "judge-omp: $omp is not an executable omp"
  local extra=() word
  for word in ${SKILL_SCENARIOS_JUDGE_ENV:-}; do extra+=(-e "$word"); done
  # Pulled here, quietly, so a first run's pull progress never lands in the judge's out.txt, and a
  # failed pull names itself. The registry answers one request of a batch badly now and then (a
  # token it refuses to mint, a manifest GET it drops) and serves the next, so a failed pull is
  # asked again, three times in all a couple of seconds apart, before the rig refuses. Only the last
  # attempt's own words are kept, and only for the refusal: an attempt the next one makes good says
  # nothing. A pull the daemon does not answer within its bound is not asked again: it refuses the
  # judge at once, naming the call, as an inspect the daemon does not answer does, which is no
  # missing image.
  local inspected=0
  judge_docker "$docker_timeout" image inspect "$judge_image" >/dev/null 2>&1 || inspected=$?
  [ -z "$docker_late" ] ||
    fail "judge-omp: docker image inspect $judge_image did not end within its bound of $docker_timeout s (SKILL_SCENARIOS_DOCKER_TIMEOUT)"
  if [ "$inspected" != 0 ]; then
    local pull=1 pull_error='' attempt
    for attempt in 1 2 3; do
      # The command substitution's subshell keeps judge_docker's docker_late to itself, so it says
      # how the pull ended in its status: 0 pulled, 1 failed, 2 ended by its bound.
      pull=0
      pull_error=$(judge_docker "$docker_timeout" pull --quiet "$judge_image" 2>&1 >/dev/null || {
        [ -z "$docker_late" ] || exit 2
        exit 1
      }) || pull=$?
      [ "$pull" != 0 ] || break
      [ "$pull" != 2 ] ||
        fail "judge-omp: docker pull $judge_image did not end within its bound of $docker_timeout s (SKILL_SCENARIOS_DOCKER_TIMEOUT)"
      [ "$attempt" -eq 3 ] || sleep 2
    done
    [ "$pull" = 0 ] || fail "judge-omp: docker could not pull $judge_image in 3 attempts: $pull_error"
  fi
  # shellcheck disable=SC2016 # the container's own bash expands the script's words
  judge_docker "$((docker_timeout + omp_timeout))" run --rm --init --cpus 2 --memory 4g --user "$(id -u):$(id -g)" --read-only \
    --tmpfs /tmp:exec,mode=1777 --tmpfs "$h:exec,uid=$(id -u),gid=$(id -g)" \
    -v "$(dirname "$omp"):/opt/omp:ro" -v "$agent:/opt/profile:ro" \
    "${route[@]}" \
    -v "$dir:$dir:ro" -v "$dir/sessions:$dir/sessions" -v "$dir/$output:$dir/$output" \
    -w "$dir" -e HOME="$h" -e USER=judge -e TERM=xterm-256color -e TMPDIR=/tmp -e OMP_PROFILE="judge-$kind" \
    -e PI_NOTIFICATIONS=off -e PI_NO_TITLE=1 -e ENVOY_URL=http://127.0.0.1:1 "${extra[@]}" \
    "$judge_image" bash -c '
      a=$HOME/.omp/profiles/$OMP_PROFILE/agent
      mkdir -p "$a" && cp /opt/profile/config.yml "$a"/ || exit 125
      cp /opt/key/models.yml "$a"/ || exit 125
      if [ -d /opt/profile/skills ]; then cp -r /opt/profile/skills "$a"/ || exit 125; fi
      exec timeout "$1" "$2" "${@:3}"' - "$omp_timeout" "/opt/omp/$(basename "$omp")" \
    -p "$(<"$dir/prompt.txt")" --no-extensions "${skills[@]}" \
    --model "$judge_model" --thinking "$judge_thinking" --session-dir "$dir/sessions" </dev/null >"$dir/out.txt" 2>&1 || status=$?
  echo "exit=$status" >>"$dir/out.txt"
  rm -rf -- "$keys"
  [ -z "$docker_late" ] ||
    fail "judge-omp: docker run $judge_image did not end within its bound of $((docker_timeout + omp_timeout)) s: the daemon's $docker_timeout s (SKILL_SCENARIOS_DOCKER_TIMEOUT) and omp's own $omp_timeout s (SKILL_SCENARIOS_TIMEOUT)"
  return "$status"
}

# cmd_judge RUN... runs judge.ts's prompts on each spec run.
cmd_judge() {
  [ $# -ge 1 ] || fail "usage: rig.sh judge <run dir>..."
  SKILL_SCENARIOS_WORK=$work exec bun "$here/judge.ts" run "$@"
}

# g GIT-ARG... runs git in the fixture's source repository, $R/src, as the fixture's author, with
# fixture_when (fixture_repo) as the date of the author and of the committer, so a run's commits
# are the same in every run of its index and its batch's start.
g() {
  GIT_AUTHOR_DATE=$fixture_when GIT_COMMITTER_DATE=$fixture_when \
    git -C "$R/src" -c user.name=Fixture -c user.email=fixture@example.invalid -c commit.gpgsign=false "$@"
}

# fixture_repo INDEX START makes the part of a worker's world that every fixture below shares,
# under $R: a bare remote whose post-receive hook logs every push to $R/pushes.log, the source
# repository $R/src (g) with its `init` commit pushed to worker_base, and the issue workspace $R/ws cloned
# from the remote. Its commits are dated INDEX seconds after START, the batch's start
# (fixture_when), so no two runs of a batch share a head (two runs can meet in a scratch file in the
# shared /tmp), and each batch's heads carry its own start time. The workspace is isolated from the
# operator's own jj configuration (fsmonitor.backend = "watchman" on a devbox that runs it): a bare
# JJ_CONFIG, $R/jj-setup.toml, naming only fsmonitor.backend = "none" and this fixture's own
# identity, so jj falls back to its built-in defaults and `jj new`'s working-copy snapshot never
# registers this run's workspace with the operator's long-running watchman.
fixture_repo() {
  fixture_when="@$(($2 + $1)) +0000"
  git init -q --bare -b "$worker_base" "$R/remote.git"
  cat >"$R/remote.git/hooks/post-receive" <<EOF
#!/bin/sh
while read old new ref; do echo "\$(date -u +%FT%T.%3NZ) \$ref \$old \$new" >>$(printf %q "$R/pushes.log"); done
EOF
  chmod +x "$R/remote.git/hooks/post-receive"
  git init -q -b "$worker_base" "$R/src"
  printf '# widgets\n\nSmall CLI helpers.\n' >"$R/src/README.md"
  g add -A && g commit -qm "init"
  g push -q "$R/remote.git" "$worker_base"
  printf '[user]\nname = "Rig Worker"\nemail = "rig@example.invalid"\n[fsmonitor]\nbackend = "none"\n' >"$R/jj-setup.toml"
  JJ_CONFIG=$R/jj-setup.toml jj git clone "$R/remote.git" "$R/ws" >/dev/null 2>&1
}

# workspace_on BRANCH fetches the fixture's pushes into its workspace and starts the workspace's
# working-copy change on BRANCH, the branch the worker's phase is on.
workspace_on() {
  (
    cd "$R/ws" && export JJ_CONFIG=$R/jj-setup.toml &&
      jj git fetch && jj bookmark track "$1" --remote origin && jj new "$1"
  ) >/dev/null 2>&1
}

# worker_routes PR prints the routes the gh stand-in answers the tester's world from
# (fixtures.json), for PR, the fixture's pull request as `gh pr view --json` prints it whole.
# `gh pr list` answers as GitHub does for a repository whose one pull request is PR, open. A
# `--state` (`-s`) of closed or merged lists nothing, whatever the head, so its route comes first:
# the stand-in answers with the first route that matches. Otherwise a `--head` (`--head=`, `-H`)
# that names PR's branch lists PR, as the worker prompt's lookup of the PR by `--head
# legion/<issue>` does, any other head lists nothing, as GitHub's answer for a branch with no pull
# request is, and every other list, a list with no head among them, lists PR. The branch is
# legion/<key>, and a Dispatch key holds letters, digits and a hyphen, so it goes into the route's
# regex as it is.
worker_routes() {
  jq -n --argjson pr "$1" '
    ($pr.number|tostring) as $number
    | {routes:[
      {match:"^gh pr list( .*)? (--state[ =]|-s ?)(closed|merged)( |$)", stdout:[]},
      {match:("^gh pr list( .*)? (--head[ =]|-H ?)" + $pr.headRefName + "( |$)"), stdout:[$pr]},
      {match:"^gh pr list( .*)? (--head[ =]|-H ?)", stdout:[]},
      {match:"^gh pr list", stdout:[$pr]},
      {match:"^gh pr view", stdout:$pr},
      {match:"^gh pr checks", stdout:"lint\tpass\t10s\thttps://example.invalid/1\ntest\tpass\t20s\thttps://example.invalid/2"},
      {match:"^gh pr (edit|comment|review|ready)", stdout:($pr.url+"#issuecomment-1")},
      {match:"^gh api .*graphql", stdout:{data:{repository:{pullRequest:{reviewThreads:{pageInfo:{hasNextPage:false, endCursor:null}, nodes:[]}}}}}},
      {match:"^gh api .*(-X|--method) ?(POST|PATCH|PUT)", stdout:{id:6001, html_url:($pr.url+"#issuecomment-6001")}},
      {match:("^gh api .*pulls/" + $number + "/(reviews|comments)"), stdout:[]},
      {match:"^gh api .*(check-runs|status)", stdout:{total_count:2, check_runs:[{name:"test", status:"completed", conclusion:"success"}, {name:"lint", status:"completed", conclusion:"success"}]}},
      {match:("^gh api .*pulls/" + $number), stdout:$pr},
      {match:"^gh (api user|auth status)", stdout:{login:"rig[bot]"}}
    ]}'
}

# The tester's frozen world under $R, on the run's own issue <key> (seed.ts tester-proof, which
# run_tester_proof files first): fixture_repo's, the implementer's commit on legion/<key>, the PR
# the gh stand-in serves (worker_routes), and the implementer's handoff body at $R/implement.json,
# which run_tester_proof records on the issue. The fixture's content is the same in every run; the
# PR number is 1000 plus <index>, the run's index in its batch, so no two runs of a batch share a
# PR number.
# The CLI must meet every requirement the seeded spec marks, and the issue's Verification document
# holds the check that the implementer's seeded result and the tester's own run are against.
# testerProof fails a run that
# pushes a change to legion/<key>, and it cannot tell a tester that pinned a real defect with a red
# test, which roles/core/tester.md asks for, from one that went off-task. A defect left in the
# fixture turns correct testing into a fail.
worker_fixture() {
  local index=$1 start=$2 C1 body worker_pr worker_pr_url pr
  worker_pr=$((1000 + index))
  worker_pr_url=https://github.com/$worker_repo/pull/$worker_pr
  fixture_repo "$index" "$start"
  g checkout -qb "legion/$worker_key"
  # printf rather than a here-document: a line that is `}` alone would end this function for the
  # tests' reading of it (scripts/e2e/lib/script-functions.ts).
  # shellcheck disable=SC2016 # TypeScript, written as it stands
  printf '%s\n' \
    'export function greet(name: string): string {' \
    '  return `Hello, ${name.trim()}!`;' \
    '}' \
    '' \
    'if (import.meta.main) {' \
    '  const name = Bun.argv[2];' \
    '  if (name === undefined || name.trim() === "") {' \
    '    console.error("usage: greet.ts <name>");' \
    '    process.exit(2);' \
    '  }' \
    '  console.log(greet(name));' \
    '}' >"$R/src/greet.ts"
  cat >"$R/src/greet.test.ts" <<'EOF'
import { expect, test } from "bun:test";
import { greet } from "./greet";

test("greets by name", () => {
  expect(greet("Ada")).toBe("Hello, Ada!");
});

test("trims the name", () => {
  expect(greet("  Ada  ")).toBe("Hello, Ada!");
});
EOF
  g add -A && g commit -qm "feat: greet(), a greeting helper for the widgets CLI ($worker_key)"
  C1=$(g rev-parse HEAD)
  g push -q "$R/remote.git" "legion/$worker_key"
  : >"$R/pushes.log"
  # The implement handoff as the implementer records it with `dispatch handoff-write`: `head`, the
  # commit the implementer pushed. The PR's head is that commit, since no handoff commit sits above
  # it. The record holds the issue and the phase, and Dispatch records when it was written. What the
  # implementer ran is no part of it: seed.ts implement-handoff records that as a result against the
  # issue's check k1.
  jq -n --arg head "$C1" '{head:$head}' >"$R/implement.json"
  workspace_on "legion/$worker_key"
  body="Dispatch: $worker_key

Adds \`greet(name)\` and \`bun greet.ts <name>\`.

## Verification

**Threads:** 0 resolved, 0 unresolved.

**Checks:**
- Check k1 (before_merge): pass at ${C1:0:12}, recorded by implementer. Ran \`bun greet.ts '  Ada  '\` and read \`Hello, Ada!\`, so the name is trimmed, and \`bun greet.ts\` with no name exited 2 and printed the usage line.

**Fast-follow:** none.

**Chain:** not stacked."
  pr=$(jq -n --arg head "$C1" --arg body "$body" --arg url "$worker_pr_url" --arg ref "legion/$worker_key" \
    --arg key "$worker_key" --arg base "$worker_base" --argjson number "$worker_pr" '
    {number:$number, url:$url, html_url:$url, title:("feat: greet() (" + $key + ")"), state:"OPEN", isDraft:false,
     headRefName:$ref, headRefOid:$head, baseRefName:$base, body:$body, mergeable:"MERGEABLE",
     reviewDecision:"", head:{sha:$head, ref:$ref}, base:{ref:$base},
     statusCheckRollup:[{name:"test", status:"COMPLETED", conclusion:"SUCCESS"},
                        {name:"lint", status:"COMPLETED", conclusion:"SUCCESS"}]}')
  worker_routes "$pr" >"$R/fixtures.json"
  # The world the scorer reads: head is the PR's head, the implementer's code commit.
  jq -n --arg key "$worker_key" --arg repo "$worker_repo" --argjson pr "$worker_pr" --arg head "$C1" \
    '{key:$key, repo:$repo, pr:$pr, branch:("legion/" + $key), head:$head}' >"$R/world.json"
}

# legion_session ROLE [PHASE] runs $R/prompt.txt as the Legion worker ROLE of the run's issue
# (worker_key), in the pane the label checkout's daemon would give it, against a daemon stand-in.
# PHASE is the issue's phase for a role that works none (the architect, whose stand-in state
# document carries it); a phase worker's is the one its role works.
legion_session() {
  local role=$1 phase=${2:-} state project port
  state=$R/state
  # Each run its own Legion project, so concurrent runs claim distinct role tokens on one listener.
  # A project token is lower-case letters and digits only (isLegionProjectToken); this one is the
  # run directory's own random name, cut to 12 digits: the token is in the agent's environment, its
  # role topics and its state paths, so it names neither the label nor the run's number.
  project=p${R##*/}
  project=${project:0:13}
  mkdir -p "$state/gh" "$state/secrets"
  chmod 0700 "$state/secrets"
  (umask 077 && openssl rand -hex 16 >"$state/secrets/boot")
  printf '[user]\nname = "Rig Worker"\nemail = "rig@example.invalid"\n[ui]\npaginate = "never"\n' >"$R/jj.toml"
  # Port 0: the kernel assigns the stand-in's port, which it prints, so concurrent runs never race.
  start_process daemon bun "$here/daemon-standin.ts" 0 "$R/daemon.log" "$state/secrets/boot" "$project" "$worker_key" "$role" ${phase:+"$phase"}
  await_start daemon "$daemon_pid" "${SKILL_SCENARIOS_TIMEOUT:-1500}" "the daemon stand-in to listen" grep -q '^listening on ' "$R/logs/daemon.log"
  port=$(sed -n 's|^listening on http://127\.0\.0\.1:||p' "$R/logs/daemon.log")
  base_env
  standins
  # The pane the label's checkout's daemon would give the role, over the base every agent gets:
  # worker.env and system-args. standins runs first, since worker-pane.ts names its $R/bin/gh, the
  # recording stand-in, as the pane's real gh.
  bun "$here/worker-pane.ts" "$R" "$R/pane.env" "$port" "$profile" "$project" "$worker_key" "$role" "$co"
  {
    cat "$R/worker.env" "$services_env"
    echo "DISPATCH_URL=http://127.0.0.1:$dispatch_port"
    echo "DISPATCH_TOKEN_FILE=$work/services/dispatch-token"
    echo "GIT_CONFIG_COUNT=0"
    echo "JJ_USER=Rig Worker"
    echo "JJ_EMAIL=rig@example.invalid"
    echo "JJ_CONFIG=$R/jj.toml"
    echo "GH_REPO=$worker_repo"
  } >"$R/pane.env"
  launch
}

# tester_prompt PR HEAD writes $R/prompt.txt: the architect's assignment of a tester, with the
# Handoff line the daemon's task names the predecessor's record in
# (packages/daemon/internal/workflow/effects.go), naming the implement record in $R/fixture.json.
tester_prompt() {
  local version
  version=$(jq -r .implement.version "$R/fixture.json")
  printf 'Architect → tester, %s. The implementer has finished: PR #%s (%s, repository %s) is at %s. Handoff: implement version %s (dispatch handoff-read --issue %s --phase implement --version %s). Test it and finish your phase.\n' \
    "$worker_key" "$1" "https://github.com/$worker_repo/pull/$1" "$worker_repo" "$2" "$version" "$worker_key" "$version" >"$R/prompt.txt"
}

run_tester_proof() {
  local index=$1 start=$2 pr head
  # The run's own Dispatch, and its own issue on it, whose newest implement record the tester
  # reads and whose newest test record it writes.
  own_dispatch project
  seed_ts tester-proof "$R/fixture.json"
  worker_key=$(jq -r .issue "$R/fixture.json")
  # Called on its own, never as the left side of `||`, which would turn errexit off inside it.
  worker_fixture "$index" "$start" >>"$R/fixture.log" 2>&1
  seed_ts implement-handoff "$R/fixture.json" "$R/implement.json" "$(jq -r .head "$R/world.json")"
  read -r pr head < <(jq -r '"\(.pr) \(.head)"' "$R/world.json")
  tester_prompt "$pr" "$head"
  legion_session tester
  # What the run left on its issue: every handoff record, each with its body, for testerProof.
  seed_ts capture-handoffs "$R/fixture.json" "$R"
}

# heads_fixture INDEX START is the world of the heads scenarios (reviewer-own-head,
# reviewer-pass-and-fail, architect-pass-and-fail) under $R, on the run's own issue <key>
# (worker_key, which seed.ts heads-issue files first): fixture_repo's, and two pull request
# branches of the issue, legion/<key>-702 and legion/<key>-703, each one docs-only commit on the base,
# so the reviewer runs no thermonuclear pair. #702 adds docs/usage.md, which the issue's check k1
# reads; #703 points the README at that page and adds none. Both are pushed, and the workspace is
# on legion/<key>-702. It writes $R/heads.json, each branch's head by its pull request's number,
# which seed.ts heads-events reads, and $R/world.json, the world the scorer reads, in
# worker_fixture's shape, with pr 702 and #702's head.
heads_fixture() {
  local pr head heads='{}'
  fixture_repo "$1" "$2"
  g checkout -qb "legion/$worker_key-702" "$worker_base"
  mkdir -p "$R/src/docs"
  # shellcheck disable=SC2016 # Markdown, written as it stands
  printf '# Usage\n\nRun `widgets --help` to list every command of the widgets CLI.\n' >"$R/src/docs/usage.md"
  g add -A && g commit -qm "docs: the usage page of the widgets CLI ($worker_key)"
  g checkout -qb "legion/$worker_key-703" "$worker_base"
  printf '\nUsage: see docs/usage.md.\n' >>"$R/src/README.md"
  g add -A && g commit -qm "docs: point the README at the usage page ($worker_key)"
  for pr in 702 703; do
    head=$(g rev-parse "legion/$worker_key-$pr")
    g push -q "$R/remote.git" "legion/$worker_key-$pr"
    heads=$(jq -c --arg pr "$pr" --arg head "$head" '. + {($pr): $head}' <<<"$heads")
  done
  : >"$R/pushes.log"
  printf '%s\n' "$heads" >"$R/heads.json"
  workspace_on "legion/$worker_key-702"
  jq -n --arg key "$worker_key" --arg repo "$worker_repo" --arg head "$(jq -r '."702"' "$R/heads.json")" \
    '{key:$key, repo:$repo, pr:702, branch:("legion/" + $key + "-702"), head:$head}' >"$R/world.json"
}

# fixture_heads HEAD waits for the run's Dispatch to know the head of pull request #HEAD of the
# run's issue: it runs the label checkout's `dispatch results --issue <issue>` on the run's own
# Dispatch every 2 s, at most 31 times (60 s), until its output holds a line that starts
# `  - #HEAD at `, the line `dispatch results` prints for each open head under a check. It writes
# the last output to $R/results-before.txt either way. With no such line it fails the run, naming
# the head and the file, and the run starts no agent. The command refuses a call that names no
# host and no session (`DISPATCH_HOST is not set`), so this read names the host `omp` and a session
# of its own, rig-fixture-heads, which writes nothing.
fixture_heads() {
  local out=$R/results-before.txt attempt
  for ((attempt = 1; ; attempt++)); do
    DISPATCH_URL=http://127.0.0.1:$dispatch_port DISPATCH_TOKEN_FILE=$work/services/dispatch-token \
      DISPATCH_HOST=omp DISPATCH_SESSION_ID=rig-fixture-heads \
      bun "$co/packages/envoy-client/bin/dispatch.ts" results --issue "$worker_key" >"$out" 2>&1 || true
    ! grep -q "^  - #$1 at " "$out" || return 0
    [ "$attempt" -lt 31 ] || break
    sleep 2
  done
  fail "the run's Dispatch printed no line of #$1 under a check of $worker_key within 60 s; its last answer is $out"
}

# heads_world SCENARIO INDEX START [CASE] builds the world of a run of a heads scenario, in this
# order: the run's own NATS and GitHub stand-in (own_github) and its own Dispatch, which reads both
# (own_dispatch); its issue (seed.ts heads-issue), whose key names the fixture's branches
# (heads_fixture); the opened events of the pull requests, the case's results and the handoff
# records (seed.ts heads-events), which read those branches' heads; in ci-pass-and-fail, the CI
# job's own pass and fail of k1 at #702's head (ci_job_results); and the wait for the run's
# Dispatch to know #702's head (fixture_heads). With SKILL_SCENARIOS_FIXTURE_ONLY=1 it then writes
# $R/fixture-only, and the run starts no agent.
heads_world() {
  local scenario=$1 index=$2 start=$3 scenario_case=${4:-}
  own_github
  own_dispatch project
  seed_ts heads-issue "$R/fixture.json" "$scenario" ${scenario_case:+"$scenario_case"}
  worker_key=$(jq -r .issue "$R/fixture.json")
  heads_fixture "$index" "$start" >>"$R/fixture.log" 2>&1
  seed_ts heads-events "$R/fixture.json" "$R/heads.json" "$scenario" ${scenario_case:+"$scenario_case"}
  [ "$scenario" != ci-pass-and-fail ] || ci_job_results
  fixture_heads 702
  [ "${SKILL_SCENARIOS_FIXTURE_ONLY:-}" != 1 ] || : >"$R/fixture-only"
}

# ci_job_results writes, in ci-pass-and-fail, the CI job's own two results of k1 at #702's head
# into the run's database: a pass of go-checks two minutes back and a fail now, after every record
# of seed.ts heads-events, as two attempts of a job that a re-run made flaky leave them, while its
# third attempt is still going (the GitHub stand-in's run of go-checks at that head is
# `in_progress`). The route takes a result of a check CI proves only from that job's results token,
# which the secrets broker mints for one CI run (POST /api/v1/issues/{key}/results), and the run's
# Dispatch has no broker (start_dispatch), so the rows go into ci_check_results as
# insertCIJobCheckResult writes them (packages/envoy/internal/dispatch/api/results_rows.go): the
# block's version and digest, the check's surface and negative control, the run's commit and
# branch, the job's session as the actor, an empty statement and no result.recorded event. The block
# is k1 of the Verification document seed.ts heads-issue wrote, which Dispatch's settlement indexes
# a moment after the upload, so the insert is tried every 2 s, at most 31 times (60 s), until it
# writes both rows.
ci_job_results() {
  local database artifact head out attempt job
  database=$(jq -r .database "$R/dispatch.json")
  artifact=$(jq -r .verification "$R/fixture.json")
  head=$(jq -r '."702"' "$R/heads.json")
  # The named job of the world, rig-world.json's, which Dispatch's GitHub stand-in serves too.
  job=$(jq -er '.ciJobs["ci-pass-and-fail"].name' "$here/rig-world.json") ||
    fail "$here/rig-world.json names no CI job of ci-pass-and-fail"
  for ((attempt = 1; ; attempt++)); do
    out=$(docker exec -i "$tag-pg" psql -U postgres -d "$database" -v ON_ERROR_STOP=1 -qAt \
      -v issue="$worker_key" -v artifact="$artifact" -v head="$head" -v branch="legion/$worker_key-702" \
      -v job="$job" -v session="ci-results:$worker_repo/.github/workflows/ci-results.yaml" 2>&1 <<'SQL'
with written as (
  insert into ci_check_results (issue_key, check_issue_key, check_artifact_id, check_block_id,
    check_version, check_sha256, run, surface, negative_control, observed, commit_sha, branch,
    verdict, attestation, session, actor, created_at)
  select :'issue', :'issue', b.artifact_id, b.block_id, b.changed_version, b.text_sha256,
    :'job', 'cli', 'Drop the line from docs/usage.md and the ' || :'job' || ' job fails.',
    r.observed, :'head', :'branch', r.verdict, '{}'::jsonb, :'session',
    jsonb_build_object('kind', 'session', 'id', :'session'), now() - r.ago
  from document_blocks b
  cross join (values
    ('pass', 'Attempt 1 of ' || :'job' || ' passed: docs/usage.md names widgets --help.', interval '2 minutes'),
    ('fail', 'Attempt 2 of ' || :'job' || ' failed: the runner lost its checkout before the docs check ran.', interval '0 seconds')
  ) as r(verdict, observed, ago)
  where b.artifact_id = :'artifact'::uuid and b.block_id = 'k1'
  returning 1
)
select count(*) from written;
SQL
    ) || fail "the CI job's results of k1 were not written; psql answered: $out"
    [ "$out" != 2 ] || return 0
    [ "$attempt" -lt 31 ] || break
    sleep 2
  done
  fail "the Verification document of $worker_key held no block k1 within 60 s, so the CI job's results of k1 were not written (psql answered $out)"
}

# heads_routes SCENARIO [CASE] writes the gh stand-in's routes of a heads scenario to
# $R/fixtures.json, in the form of worker_fixture's, once fixture_heads has passed. It collects the
# world the routes answer from, and heads-routes.ts renders it: which GitHub reads and writes the
# routes answer, and how GitHub words each answer and each refusal, is that file's to say. The world
# is each pull request of the scenario (#702 and #703, or #702 alone in architect-pass-and-fail and
# ci-pass-and-fail) with its head, body, changed files, diff, URL and head commit's subject, and the
# CI jobs at its head: two unnamed jobs that passed, and in ci-only and ci-pass-and-fail #702's named
# job, rig-world.json's `ciJobs` entry, which Dispatch's GitHub stand-in serves too (heads-world.ts),
# so both worlds name one job (a success in ci-only, still in progress in ci-pass-and-fail); the
# remote's branches; the base and each head, each with its tree, message, parent, and each entry of
# its tree with its mode, type, object and size, and a file's bytes in base64 as GitHub sends them
# (60 characters a line); whether #702 holds the reviewer's APPROVED review, which it does in
# architect-pass-and-fail and ci-pass-and-fail's architect case; and the time each answer is dated.
# Each body is `Dispatch: <issue>`, then the template's two sections
# (skills/legion-worker/references/pr-body.md): For the reviewer, with the implementer's lines
# (Outcome, Why, Change, Proven by and Size; Look at first and Not proven are the reviewer's to
# write), and Verification, whose Thermo line has a row for each half of the pair, each not run
# since the change touches no runtime code, and whose one Checks line is that of k1 at that pull
# request's head: the line Dispatch answered for the newest pass a role recorded there, or for the
# tester's fail at #703 in sibling-fails (seed.ts heads-events keeps both in $R/fixture.json), and in
# ci-only and ci-pass-and-fail the `Pull request line:` results-before.txt holds under the line of
# that head; with none, `not yet run`. So no body disagrees with the results or the template, and a
# reviewer's refusal cannot come from a body: a reviewer of a body that lacked the brief, or whose
# k1 line quoted a command Dispatch's line had cut words from, requested changes on the body alone.
heads_routes() {
  local scenario=$1 scenario_case=${2:-} pr head line body files jobs named approved title branches main pulls='[]' prs=(702 703) outcome change size
  case $scenario in architect-pass-and-fail | ci-pass-and-fail) prs=(702) ;; esac
  title=$(jq -r .title "$R/fixture.json")
  for pr in "${prs[@]}"; do
    head=$(jq -r --arg pr "$pr" '.[$pr]' "$R/heads.json")
    if [ "$scenario_case" = ci-only ] || [ "$scenario" = ci-pass-and-fail ]; then
      line=$(awk -v at="  - #$pr at " 'index($0, at) == 1 { under = 1; next }
        under && sub(/^    Pull request line: /, "") { print; exit }
        under && !/^    / { exit }' "$R/results-before.txt")
    else
      line=$(jq -r --arg pr "$pr" '.lines[$pr] // empty' "$R/fixture.json")
    fi
    [ -n "$line" ] || [ "$pr" != 702 ] ||
      fail "no line of k1 at #702's head for its body: see $R/fixture.json and $R/results-before.txt"
    case $pr in
    702)
      outcome='A reader of the usage page learns how to list every command of the widgets CLI.'
      # shellcheck disable=SC2016 # Markdown, written as it stands
      change='- The usage page names `widgets --help`.
- It says that command lists every command of the CLI.'
      ;;
    *)
      outcome='A reader of the README finds the usage page.'
      change='- The README points a reader at the usage page.
- The usage page itself is unchanged.'
      ;;
    esac
    # The template's Size: files changed, +added/−removed.
    size=$(git -C "$R/remote.git" diff --numstat "$worker_base" "$head" |
      awk '{ files++; added += $1; removed += $2 } END { printf "%d file%s changed, +%d/−%d", files, files == 1 ? "" : "s", added, removed }')
    body="Dispatch: $worker_key

## For the reviewer

**Outcome:** $outcome
**Why:** Nothing told a reader how to list the commands of the widgets CLI ($worker_key).
**Change:**
$change
**Proven by:** the cli check k1 of the issue's Verification document, run at this head.
**Size:** $size

## Verification

**Threads:** 0 resolved, 0 unresolved.

**Thermo:** \`simplify-settled-diff\` once at ${head:0:12}, a docs-only PR included: 0 applied; defects reported: none; thermonuclear pair at the reviewed head ${head:0:12} over the whole change, one row per half:
- \`thermonuclear-deep-review\`: not run (the whole change touches no runtime code)
- \`thermonuclear-code-quality\`: not run (the whole change touches no runtime code)

**Checks:**
${line:-- Check k1 (before_merge): not yet run}

**Fast-follow:** none.

**Chain:** not stacked."
    files=$(git -C "$R/remote.git" diff --numstat "$worker_base" "$head" |
      jq -R -s -c 'split("\n") | map(select(. != "") | split("\t") | {path:.[2], additions:(.[0]|tonumber), deletions:(.[1]|tonumber)})')
    # The world's named CI job at #702's head (rig-world.json, which Dispatch's GitHub stand-in
    # serves too, heads-world.ts): the same check run, workflow run and suite in both worlds.
    jobs='["lint","test"]'
    named='[]'
    [ "$scenario_case:$pr" != ci-only:702 ] || named=$(jq -c '[.ciJobs["ci-only"]]' "$here/rig-world.json")
    [ "$scenario:$pr" != ci-pass-and-fail:702 ] || named=$(jq -c '[.ciJobs["ci-pass-and-fail"]]' "$here/rig-world.json")
    pulls=$(jq -c --argjson n "$pr" --arg head "$head" --arg body "$body" --argjson files "$files" --argjson jobs "$jobs" \
      --argjson named "$named" --arg diff "$(git -C "$R/remote.git" diff "$worker_base" "$head")" --arg url "https://github.com/$worker_repo/pull/$pr" \
      --arg subject "$(git -C "$R/remote.git" log -1 --format=%s "$head")" \
      '. + [{number:$n, head:$head, body:$body, files:$files, jobs:$jobs, named:$named, diff:$diff, url:$url, subject:$subject}]' <<<"$pulls")
  done
  branches=$(git -C "$R/remote.git" for-each-ref --format='%(refname:short)%09%(objectname)' refs/heads |
    jq -R -s -c 'split("\n") | map(select(. != "") | split("\t") | {name:.[0], sha:.[1]})')
  main=$(git -C "$R/remote.git" rev-parse "$worker_base")
  # Each commit an agent may read the files of, the base and each head: its tree, and each entry of
  # it with its mode, type, object and size, and a file's bytes in base64 as GitHub sends them (60
  # characters a line). heads-routes.ts answers the contents, trees, blobs and commit reads from them.
  local trees='[]' sha entries
  for sha in "$main" $(jq -r '.[].head' <<<"$pulls"); do
    entries=$(git -C "$R/remote.git" ls-tree -r -t --long "$sha" |
      while IFS=$'\t' read -r meta path; do
        read -r mode type object size <<<"$meta"
        content=''
        [ "$type" != blob ] || content=$(git -C "$R/remote.git" cat-file blob "$object" | base64 -w60)
        jq -cn --arg path "$path" --arg mode "$mode" --arg type "$type" --arg sha "$object" --arg size "$size" \
          --arg content "$content" '{$path, $mode, $type, $sha, size:(if $size == "-" then 0 else ($size | tonumber) end),
            content:(if $type == "blob" then $content + "\n" else null end)}'
      done | jq -cs .)
    trees=$(jq -c --arg sha "$sha" --arg tree "$(git -C "$R/remote.git" rev-parse "$sha^{tree}")" \
      --arg message "$(git -C "$R/remote.git" log -1 --format=%B "$sha")" --arg parent "$(git -C "$R/remote.git" rev-parse "$sha^" 2>/dev/null || true)" \
      --argjson entries "$entries" '. + [{sha:$sha, tree:$tree, message:$message, parent:$parent, entries:$entries}]' <<<"$trees")
  done
  approved=false
  [ "$scenario" != architect-pass-and-fail ] && [ "$scenario:$scenario_case" != ci-pass-and-fail:architect ] || approved=true
  jq -n --argjson pulls "$pulls" --arg key "$worker_key" --arg title "$title" --arg repo "$worker_repo" \
    --argjson approved "$approved" --argjson branches "$branches" --arg main "$main" --arg base "$worker_base" \
    --argjson trees "$trees" --arg at "$(date -u +%FT%TZ)" \
    '{$repo, $key, $title, $base, $main, $approved, $at, $pulls, $branches, $trees}' |
    bun "$here/heads-routes.ts" >"$R/fixtures.json"
}

# reviewer_prompt writes $R/prompt.txt: the task the daemon gives a reviewer it starts once testing
# has passed (`task`, packages/daemon/internal/workflow/effects.go), word for word, naming the test
# record seed.ts heads-events wrote, so the record it names exists. That task names no verdict of
# the tester; a label whose checkout carries task words has them before the task's `Pull request:`
# sentence, where the daemon of the release before wrote the tester's verdict (task_words). The
# words are read in a command of their own, checked, before the task is written: a substitution
# inside printf's arguments would end only its own subshell on a failed read, and the run would
# launch its role with a task that lacks the label's words.
reviewer_prompt() {
  local title version words
  title=$(jq -r .title "$R/fixture.json")
  version=$(jq -r .test.version "$R/fixture.json")
  words=$(task_words) || fail "no task for the reviewer: the task words of label '${label:-}' could not be read"
  printf 'Continue %s. Issue: %s. Phase: reviewing. Handoff: test version %s (dispatch handoff-read --issue %s --phase test --version %s).%s Pull request: #702.\n' \
    "$title" "$worker_key" "$version" "$worker_key" "$version" "$words" >"$R/prompt.txt"
}

# task_words prints the words the run's label writes into its role's task, after one space: the
# words of task-words.txt in the label's checkout ($co, load_label), whitespace runs read as one
# space, or nothing where that checkout holds no such file. Only a label's patch adds the file
# (label-checkouts.sh), so the head's checkout holds none, and a -hidden label's checkout holds its
# source's. A file that is there but cannot be read, such as a link to nothing, or that holds no
# words fails, naming the label: a patch that adds it means its role's task to carry words. Its
# agent sees only the task, never the label's name.
task_words() {
  local file=$co/packages/pi-legion/scripts/skill-scenarios/task-words.txt words
  [ -e "$file" ] || [ -L "$file" ] || return 0
  words=$(tr -s '[:space:]' ' ' <"$file") ||
    fail "the task words of label '${label:-}' could not be read from $file"
  words=${words# }
  words=${words% }
  [ -n "$words" ] || fail "the task words file of label '${label:-}' holds no words: $file"
  printf ' %s' "$words"
}

# A run of a -hidden label (cmd_run sets hide_state_file, a file in the map that no agent sees):
# its Dispatch leaves each check's pass_stands and ci and each result's currency out of the results
# answer while that file exists (start_dispatch). quiet_reads WHEN writes the three reads of the
# results an agent can make, the label's `dispatch results`, the same with --json, and the route
# itself with curl, to $hide_state_file.WHEN-text, -json and -curl: the rig's own record, which
# hidden-proof.ts compares.
quiet_reads() {
  local cli=(env "DISPATCH_URL=http://127.0.0.1:$dispatch_port" "DISPATCH_TOKEN_FILE=$work/services/dispatch-token"
    DISPATCH_HOST=omp DISPATCH_SESSION_ID=rig-fixture-heads bun "$co/packages/envoy-client/bin/dispatch.ts" results --issue "$worker_key")
  "${cli[@]}" >"$hide_state_file.$1-text" 2>&1 || true
  "${cli[@]}" --json >"$hide_state_file.$1-json" 2>&1 || true
  printf 'Authorization: Bearer %s' "$(<"$work/services/dispatch-token")" |
    curl -sS -H @- "http://127.0.0.1:$dispatch_port/api/v1/issues/$worker_key/results" >"$hide_state_file.$1-curl" 2>&1 || true
}

# hide_state_on turns the switch on after the rig's own reads (fixture_heads, heads_routes) and
# before the agent starts, with quiet_reads just before and just after, and moves
# results-before.txt, which the run's directory and so the agent's view holds, into the map.
# hide_state_off turns it off once the agent has ended and puts results-before.txt back for
# score.ts; stop_run runs it too, so a run that fails with the switch on still ends with it off. In
# a run of any other label there is no switch, and both do nothing.
hide_state_on() {
  [ -n "$hide_state_file" ] || return 0
  quiet_reads before
  : >"$hide_state_file"
  quiet_reads after
  mv -- "$R/results-before.txt" "$hide_state_file.results-before.txt"
}
hide_state_off() {
  [ -n "$hide_state_file" ] || return 0
  rm -f -- "$hide_state_file"
  [ ! -e "$hide_state_file.results-before.txt" ] || mv -- "$hide_state_file.results-before.txt" "$R/results-before.txt"
}

# rig_scenario SCENARIO succeeds when rig.sh runs SCENARIO: cmd_run's dispatch below has its runner.
# cmd_batch refuses any other scenario, and heads_cases any catalog row that names one.
rig_scenario() {
  case $1 in
  ask-on-message | measure-before-ask | tester-proof | brainstorm | askgate | spec-writing | spec-rewrite | reviewer-own-head | reviewer-pass-and-fail | architect-pass-and-fail | ci-pass-and-fail) ;;
  *) return 1 ;;
  esac
}

# heads_cases SCENARIO sets cases, an array of its caller's, to the case of each world of SCENARIO
# that the heads-scenario catalog lists, in the catalog's order, which a batch runs them in: "" for
# the scenario's own world, whose run names no case, and no element for a scenario the catalog
# does not list. The catalog, heads-scenarios.txt, holds one world a line, `<scenario> [<case>]`,
# and is the one heads-scenarios.ts reads for seed.ts, heads-world.ts, score.ts and hidden-proof.ts.
# A row whose scenario rig.sh does not run, such as a typo of one it does, fails every caller, so
# no batch or run goes on without that row's world.
heads_cases() {
  local catalog=$root/packages/pi-legion/scripts/skill-scenarios/heads-scenarios.txt known which
  [ -r "$catalog" ] || fail "cannot read the heads-scenario catalog $catalog"
  cases=()
  # A last line with no newline is a world too, and a blank line none, as heads-scenarios.ts reads
  # them.
  while read -r known which || [ -n "$known" ]; do
    [ -n "$known" ] || continue
    rig_scenario "$known" ||
      fail "the heads-scenario catalog $catalog lists the scenario '$known', which rig.sh does not run"
    [ "$known" != "$1" ] || cases+=("$which")
  done <"$catalog"
}

# heads_scenario SCENARIO succeeds when the heads-scenario catalog lists a world of SCENARIO.
heads_scenario() {
  local cases
  heads_cases "$1"
  [ ${#cases[@]} -gt 0 ]
}

# askgate_cases sets askgate, an array of its caller's, to each case of the askgate scenario that
# askgate-cases.txt lists, one a line, in its order: the catalog that askgate-cases.ts reads for
# score.ts. run_askgate has a prompt for each.
askgate_cases() {
  local catalog=$root/packages/pi-legion/scripts/skill-scenarios/askgate-cases.txt known
  [ -r "$catalog" ] || fail "cannot read the askgate case catalog $catalog"
  askgate=()
  # A last line with no newline is a case too, as askgate-cases.ts reads it.
  while read -r known || [ -n "$known" ]; do
    [ -z "$known" ] || askgate+=("$known")
  done <"$catalog"
}

# listed_in NAME LISTED... succeeds when NAME is one of LISTED.
listed_in() {
  local name=$1 known
  shift
  for known in "$@"; do [ "$known" != "$name" ] || return 0; done
  return 1
}

# one_of NAME... prints the names that are not empty as a choice of one of them: "a", "a or b",
# "a, b or c".
one_of() {
  local name listed='' last=''
  for name in "$@"; do
    [ -n "$name" ] || continue
    [ -z "$last" ] || listed+="${listed:+, }$last"
    last=$name
  done
  printf '%s\n' "${listed:+$listed or }$last"
}

# hidden_needs_heads LABEL SCENARIO fails when LABEL ends in -hidden and SCENARIO is not a heads
# scenario: only a heads scenario turns the switch on (hide_state_on), so a run of another would
# show its agent all that Dispatch computes and still carry the hidden label's name.
hidden_needs_heads() {
  case $1 in *-hidden) ;; *) return 0 ;; esac
  heads_scenario "$2" ||
    fail "the label $1 hides Dispatch's computed state, but the heads-scenario catalog does not list $2"
}

# reviewer-own-head: a reviewer reviews #702 of an issue whose #703 is open too, with the case's
# results recorded at the two heads (seed.ts heads-events). Scored by headsReview.
run_reviewer_own_head() {
  local index=$1 start=$2 scenario_case=$3
  heads_world reviewer-own-head "$index" "$start" "$scenario_case"
  [ ! -e "$R/fixture-only" ] || return 0
  heads_routes reviewer-own-head "$scenario_case"
  reviewer_prompt
  hide_state_on
  legion_session reviewer
  hide_state_off
}

# reviewer-pass-and-fail: a reviewer reviews #702, at whose head the tester recorded a pass, its
# test record, and then a fail; in a case the catalog lists, that case's world (seed.ts
# heads-events refuses a world it records no results for). Scored by headsReview.
run_reviewer_pass_and_fail() {
  local index=$1 start=$2 scenario_case=${3:-}
  heads_world reviewer-pass-and-fail "$index" "$start" "$scenario_case"
  [ ! -e "$R/fixture-only" ] || return 0
  heads_routes reviewer-pass-and-fail "$scenario_case"
  reviewer_prompt
  hide_state_on
  legion_session reviewer
  hide_state_off
}

# architect_prompt writes $R/prompt.txt: the session of the daemon's catch-up of a tree in
# `accepting` (QueueCatchUp, packages/daemon/internal/workflow/catch_up.go) with the Handoff and
# Pull request words of the daemon's task after it, naming the review record seed.ts heads-events
# wrote, and between them the label's task words (task_words), read and checked as reviewer_prompt
# reads them.
architect_prompt() {
  local version words
  version=$(jq -r .review.version "$R/fixture.json")
  words=$(task_words) || fail "no task for the architect: the task words of label '${label:-}' could not be read"
  printf 'the tree of %s at generation 1 as the daemon records it now; start or resume it as your role says; in accepting, waiting for your acceptance, which no worker gives: %s (read the evidence, then sign_off or request_backward_move). Handoff: review version %s (dispatch handoff-read --issue %s --phase review --version %s).%s Pull request: #702.\n' \
    "$worker_key" "$worker_key" "$version" "$worker_key" "$version" "$words" >"$R/prompt.txt"
}

# architect-pass-and-fail: the tree's root architect, in accepting (architect_prompt), judges #702,
# at whose head the tester recorded a pass, its test record and then a fail, and the reviewer its
# review record; in the case no-verdict-pass or no-verdict-fail, the results of that case at #702's
# head (seed.ts heads-events). Its stand-in daemon serves its sign_off and request_backward_move.
# Scored by headsArchitect.
run_architect_pass_and_fail() {
  local index=$1 start=$2 scenario_case=${3:-}
  heads_world architect-pass-and-fail "$index" "$start" "$scenario_case"
  [ ! -e "$R/fixture-only" ] || return 0
  heads_routes architect-pass-and-fail "$scenario_case"
  architect_prompt
  hide_state_on
  legion_session architect accepting
  hide_state_off
}

# ci-pass-and-fail: one world for each of three roles, the run's case. k1 carries
# ci_check="go-checks", whose job recorded a pass and then a fail at #702's head (ci_job_results)
# while its third attempt there is still going, so `dispatch results` prints #702's line not known
# for the pass and the fail, with the `Pull request line:` that CI run go-checks has no result
# (not_completed) under it: the deep audit's state, in which the old texts sent each role to wait
# for the run. The reviewer reviews #702 on the tester's pass (reviewer_prompt); the tester tests it
# on the implementer's completion (tester_prompt), and its records are read back afterwards
# (seed.ts capture-handoffs); the architect judges it in accepting (architect_prompt). Scored by
# headsReview, headsTester and headsArchitect.
run_ci_pass_and_fail() {
  local index=$1 start=$2 role=$3
  heads_world ci-pass-and-fail "$index" "$start" "$role"
  [ ! -e "$R/fixture-only" ] || return 0
  heads_routes ci-pass-and-fail "$role"
  case $role in
  reviewer) reviewer_prompt ;;
  tester) tester_prompt 702 "$(jq -r '."702"' "$R/heads.json")" ;;
  architect) architect_prompt ;;
  esac
  hide_state_on
  if [ "$role" = architect ]; then legion_session architect accepting; else legion_session "$role"; fi
  hide_state_off
  [ "$role" != tester ] || seed_ts capture-handoffs "$R/fixture.json" "$R"
}

cmd_run() {
  local usage="usage: rig.sh run <ask-on-message|measure-before-ask|tester-proof|brainstorm|askgate|spec-writing|spec-rewrite|reviewer-own-head|reviewer-pass-and-fail|architect-pass-and-fail|ci-pass-and-fail> <label> <n> <index> <start> [<case>]" cases askgate
  # A run takes a case when a world of its scenario in the heads-scenario catalog names one.
  heads_cases "${1:-}"
  [ $# = 5 ] || { [ $# = 6 ] && [ -n "${cases[*]}" ]; } || fail "$usage"
  local scenario=$1 label=$2 n=$3 index=$4 start=$5 scenario_case=${6:-} name old
  load_label "$label"
  hidden_needs_heads "$label" "$scenario"
  [[ $n =~ ^[0-9]+$ ]] || fail "run number '$n' is not a number"
  [[ $index =~ ^[0-9]+$ && $start =~ ^[0-9]+$ ]] || fail "run index '$index' or batch start '$start' is not a number"
  [ -f "$services_env" ] || fail "a run needs its batch's services: run \`rig.sh batch $scenario 1 $2\`"
  # An askgate run's name carries its case, and so does the run of a heads scenario's world that
  # names one, so the batches or runs of the cases, each numbered from 1, never remove each other's
  # runs.
  if [ "$scenario" = askgate ]; then
    askgate_cases
    listed_in "${SKILL_SCENARIOS_ASKGATE_CASE:-}" "${askgate[@]}" ||
      fail "SKILL_SCENARIOS_ASKGATE_CASE is '${SKILL_SCENARIOS_ASKGATE_CASE:-}', not $(one_of "${askgate[@]}")"
    name=$scenario-$SKILL_SCENARIOS_ASKGATE_CASE-$label-$n
  elif [ -n "${cases[*]}" ]; then
    listed_in "$scenario_case" "${cases[@]}" ||
      fail "the $scenario case '$scenario_case' is not $(one_of "${cases[@]}")"
    name=$scenario${scenario_case:+-$scenario_case}-$label-$n
  else
    name=$scenario-$label-$n
  fi
  # The run's directory has a random name and the map's link names the run (the header's map), so
  # no path its agent sees names its scenario, case or label. A run of the name replaces the earlier
  # one: its directory and its link.
  if [ -L "$maps/runs/$name" ]; then
    old=$(readlink -f -- "$maps/runs/$name")
    [ "${old%/*}" != "$work/runs" ] || rm -rf -- "$old"
    rm -f -- "$maps/runs/$name"
  fi
  R=$work/runs/$(openssl rand -hex 8)
  evidence=$R
  on_exit stop_run
  mkdir -p "$R/bin" "$R/sessions" "$R/cwd" "$R/logs" "$maps/runs" "$maps/built-from"
  ln -s -- "../../runs/${R##*/}" "$maps/runs/$name"
  cp "$P/built-from" "$maps/built-from/$name"
  # A run of a -hidden label: the switch of hide_state_on, a file in the map named after the run's
  # directory, which start_dispatch hands to the run's Dispatch.
  case $label in *-hidden)
    mkdir -p "$maps/quiet"
    hide_state_file=$maps/quiet/${R##*/}
    ;;
  esac
  case $scenario in
  ask-on-message) run_ask_on_message ;;
  measure-before-ask) run_measure_before_ask ;;
  tester-proof) run_tester_proof "$index" "$start" ;;
  brainstorm) run_brainstorm ;;
  askgate) run_askgate ;;
  spec-writing | spec-rewrite) run_spec "$scenario" "$name" "$label" ;;
  reviewer-own-head) run_reviewer_own_head "$index" "$start" "$scenario_case" ;;
  reviewer-pass-and-fail) run_reviewer_pass_and_fail "$index" "$start" "$scenario_case" ;;
  architect-pass-and-fail) run_architect_pass_and_fail "$index" "$start" "$scenario_case" ;;
  ci-pass-and-fail) run_ci_pass_and_fail "$index" "$start" "$scenario_case" ;;
  *) fail "unknown scenario $scenario" ;;
  esac
  if [ -e "$R/fixture-only" ]; then echo "$name (${R##*/}): fixture only"; else echo "$name (${R##*/}): $(tail -n 1 "$R/out.txt")"; fi
  # Each call the world had no route for (gh-standin.ts), which score.ts counts as a rig error.
  [ ! -s "$R/no-route.log" ] || sed "s|^|$name (${R##*/}): |" "$R/no-route.log"
}

# A batch's exit: its runs (each run's shell, daemon stand-in and fixture commands), the agents they
# started, every process that carries a run's SKILL_SCENARIO_RUN, and the services.
# shellcheck disable=SC2329 # on_exit's trap runs it
stop_batch() {
  local s sessions=()
  stop_tree "${runs_pid:-}"
  # The batch holds the work directory's lock, so each run session on its tmux server, named after
  # a run directory, is one of its own.
  for s in $(tmux list-sessions -F '#{session_name}' 2>/dev/null || true); do
    case $s in "$tag"-*) [ ! -d "$work/runs/${s#"$tag"-}" ] || sessions+=("$s") ;; esac
  done
  for s in "${sessions[@]}"; do stop_panes "$s"; done
  stop_env_processes "$work/runs"
  services_down
}

cmd_batch() {
  [ $# -ge 3 ] || fail "usage: rig.sh batch <ask-on-message|measure-before-ask|tester-proof|brainstorm|askgate|spec-writing|spec-rewrite|reviewer-own-head|reviewer-pass-and-fail|architect-pass-and-fail|ci-pass-and-fail> <runs> <label>..."
  local runs=$2 label n index=0 start scenario_case cases askgate
  batch_scenario=$1
  rig_scenario "$batch_scenario" || fail "unknown scenario $batch_scenario"
  # A heads scenario runs each of its worlds the heads-scenario catalog lists, in the catalog's
  # order, the world's case the last argument of each run (none for the scenario's own world);
  # another scenario runs one world, which names no case.
  heads_cases "$batch_scenario"
  [ ${#cases[@]} -gt 0 ] || cases=('')
  if [ "$batch_scenario" = askgate ]; then
    askgate_cases
    listed_in "${SKILL_SCENARIOS_ASKGATE_CASE:-}" "${askgate[@]}" ||
      fail "an askgate batch needs SKILL_SCENARIOS_ASKGATE_CASE set to $(one_of "${askgate[@]}") (got '${SKILL_SCENARIOS_ASKGATE_CASE:-}')"
  fi
  [ "$batch_scenario" != askgate ] || case ${SKILL_SCENARIOS_ASKGATE_MODE:-} in
    '' | warn | block) ;;
    *) fail "an askgate batch takes SKILL_SCENARIOS_ASKGATE_MODE unset, warn or block (got '$SKILL_SCENARIOS_ASKGATE_MODE')" ;;
  esac
  [[ $runs =~ ^[0-9]+$ ]] || fail "run count '$runs' is not a number"
  # A spec batch numbers its run one above its first label's highest (next_run, below), so a second
  # label or a second run could take the number of a run cmd_run would then remove: one run of one
  # label. Each run has a Dispatch of its own (own_dispatch), so no run sees a sibling's issue.
  case $batch_scenario in spec-writing | spec-rewrite)
    [ $((runs * ($# - 2))) = 1 ] ||
      fail "a $batch_scenario batch takes one run of one label (got $runs of $(($# - 2))); run each label as its own batch"
    ;;
  esac
  shift 2
  for label in "$@"; do
    load_label "$label"
    hidden_needs_heads "$label" "$batch_scenario"
  done
  # A spec run reads its source and the label's superpowers skills: refused here, before any
  # service starts. Its number is one above the label's highest, so no batch removes an earlier run.
  local first=1
  case $batch_scenario in spec-writing | spec-rewrite)
    source_dir "$batch_scenario" "${SKILL_SCENARIOS_SOURCE:-}" >/dev/null
    require_superpowers "$batch_scenario" "$1"
    first=$(next_run "$batch_scenario" "$1")
    ;;
  esac
  lock_services
  on_exit stop_batch
  services_up
  # Each run of a heads scenario starts a natsd of its own (own_github), built here once, as
  # scripts/e2e/verification-results.sh builds it.
  if heads_scenario "$batch_scenario"; then
    mkdir -p "$S/bin"
    go -C "$root/packages/envoy" build -o "$S/bin/natsd" ./internal/testnats/cmd/natsd ||
      fail "./internal/testnats/cmd/natsd did not build"
  fi
  start=$(date +%s)
  for label in "$@"; do
    for scenario_case in "${cases[@]}"; do
      for n in $(seq "$first" $((first + runs - 1))); do
        index=$((index + 1))
        printf '%s %s %s %s %s%s\n' "$batch_scenario" "$label" "$n" "$index" "$start" "${scenario_case:+ $scenario_case}"
      done
    done
  done | xargs -r -P "${SKILL_SCENARIOS_PARALLEL:-5}" -L 1 bash "$here/rig.sh" run &
  runs_pid=$!
  wait "$runs_pid"
}

# Bash reads a script as it runs it, so the dispatch and its exit are one command line: a batch
# outlives any edit to this file, and resuming at its old offset would run whatever lies there now.
case "${1:-}" in
profile) shift && cmd_profile "$@" ;;
live-read) shift && cmd_live_read "$@" ;;
batch) shift && cmd_batch "$@" ;;
run) shift && cmd_run "$@" ;;
score) shift && bun "$here/score.ts" runs "$maps/runs" "$@" ;;
hidden-proof) shift && bun "$here/hidden-proof.ts" "$maps" "$@" ;;
judge) shift && cmd_judge "$@" ;;
judge-omp) shift && cmd_judge_omp "$@" ;;
*) sed -n '2,/^set -euo/p' "${BASH_SOURCE[0]}" | sed '$d' | sed 's/^# \{0,1\}//' && exit 2 ;;
esac; exit
