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
#                                                  stops the services; a scored scenario
#                                                  (owner-walk) runs with batch-scored instead
#   rig.sh score     [<scenario>]                  one row per run, then counts per label; a scored
#                                                  scenario's runs (owner-walk) are scored one by
#                                                  one (`score.ts runs --run`), then their rows, a
#                                                  `reach` line for each entry of each run's reach,
#                                                  and the cells (`score.ts cells`)
#   rig.sh label-tree <name> <commit> <manifest>   a label's tree: a fresh detached checkout of
#                                                  <commit> of this checkout's repository under
#                                                  <work>/label-trees/<name>/tree, each line of the
#                                                  manifest applied (cmd_label_tree)
#   rig.sh unpatch-holds <name> <patch>            the label tree <name> differs from its commit in
#                                                  the patch's lines alone (unpatch_holds)
#   rig.sh plans-capture <checkout> <out dir>      the recorded answer of a real plans read at the
#                                                  checkout, and its source (cmd_plans_capture)
#   rig.sh batch-scored <scenario> <count> <label>...
#                                                  starts the scratch services and runs each
#                                                  label's cell of the case the scenario's case
#                                                  variable names until <count> runs of it scored,
#                                                  at most three times <count> launches, a run
#                                                  already in <work>/runs counted among them; a
#                                                  cell short of its count prints UNKNOWN. With
#                                                  RT_STORE, each run is stored as it ends, its
#                                                  pack and then its row, which names the pack's
#                                                  sha256 (store_scored_run)
#   rig.sh restore-stored <scenario>               each run of the scenario whose row an earlier
#                                                  pod of the Job stored (RT_STORE): the pack its
#                                                  row names, checked and unpacked into
#                                                  <work>/runs (cmd_restore_stored)
#   rig.sh check-stored <dir> <job log>...         a download of a Job's store, checked against
#                                                  the sha256 its logs and its rows name: each run
#                                                  unpacked into <dir>/checked, or a rig error row
#                                                  naming each fault; then the cells
#   rig.sh rows      [<scenario>]                  each scored run's row.json, then each entry of
#                                                  its reach, without scoring it again
#   rig.sh cell      <scenario> <label> <case> <count>
#                                                  the line of one case's cell (score.ts cell): a
#                                                  case form's runs at <count>, the runs that
#                                                  passed each clause, and its verdict; a control
#                                                  form's case the control's own cell over every
#                                                  owner of the form, with cells' verdict
#   rig.sh evidence  [<scenario>]                  the cells, each label's built-from and each
#                                                  run's row and model, as Markdown
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
# A batch runs each run as `rig.sh run <scenario> <label> <n> <index> <start>`, which needs that
# batch's services: <index> is the run's place in the batch and <start> the batch's start in epoch
# seconds. One run on its own is `rig.sh batch <scenario> 1 <label>`, or `rig.sh batch-scored
# owner-walk 1 <label>` for owner-walk.
#
# The tools of this directory (each one's header has its own usage):
#   seed.ts               seeds a batch's scratch Dispatch and each run's issue, and reads back what
#                         the run left there (rig.sh runs it)
#   score.ts              scores each run from its transcript and what seed.ts read back
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
#                   the Envoy plugin's review of each gated Dispatch write, on (any label) or off (the
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
#                   so each case's batch keeps its own runs, and each run writes the case to
#                   <run>/case.txt and a set mode to <run>/mode.txt (run_askgate). Scored by
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
#   owner-walk      A scored scenario (scored-runs.ts SCORED_SCENARIOS): one fresh owner, a root's
#                   architect or a child's, plans the issue G of worlds/owner-walk/issue.md and then
#                   answers its form's questions, on a label's own texts (run_owner_walk, below).
#                   SKILL_SCENARIOS_OWNER_WALK_CASE names the case, <kind>-<form>-<family>
#                   (owner-walk.ts cases), which the run's name carries:
#                   owner-walk-<case>-<label>-<n>. Scored by ownerWalk, one run at a time
#                   (`score.ts runs --run`), and counted in cells (`score.ts cells`).
#
# A scenario measures its rule only if a label whose skills lack the rule scores lower than one
# whose skills state it. So every comparison carries an ablate label: a checkout of the head with
# the scenario's scored rule deleted from its skills, never committed. measure-before-ask is the
# instrument: gate 2 lives in the dispatch skill alone, and its ablate arm scores below the head.
# ask-on-message and tester-proof are controls: their rules also live in dispatch-first and
# `dispatch ask --help`, and in the tester role prompt, so ablate scores as the head does
# and they cannot detect a lost skill rule. A new scenario is an instrument only once an ablate
# arm has scored lower on it.
#
# A batch's services are a Postgres, a NATS, and the Envoy listener built from this checkout. The
# Postgres and the NATS are two containers the batch starts and removes, or, when
# SKILL_SCENARIOS_POSTGRES_URL and SKILL_SCENARIOS_NATS_URL are both set, the two servers those
# URLs name, which the batch uses and leaves running: that is how a batch runs where there is no
# docker. The two Optional entries below say what each given server must be: the Postgres on this
# machine, and a NATS of the batch's own. Each run gets a Dispatch of its own (own_dispatch): a
# database named after the run in the batch's Postgres, and the e2e harness's real Go Dispatch
# server (packages/dispatch/e2e/run-server.sh) on that database, seeded and read back through its
# API (seed.ts) and stopped when the run ends. A `dispatch search` searches every project of its
# server, and a Dispatch bearer cannot be limited to one project, so a shared server would show
# each run the other runs' issues and asks. Each run records its Dispatch in <run>/dispatch.json,
# and score.ts does not score two runs that name one database. A given server's URL carries its
# port and docker assigns a container's host port; the listener serves on a port the kernel chose
# and scripts/e2e/lib/rig.sh holds for it until it serves (start_held), each run's Dispatch on its
# own run-server.sh holds the same way and reports, and the kernel assigns each run's daemon
# stand-in its port.
#
# A build pod has no docker: the runner's pod gives a Postgres and a NATS of its own, which
# scored-job.sh gives the batch as SKILL_SCENARIOS_POSTGRES_URL and SKILL_SCENARIOS_NATS_URL. The
# runs' databases stay on that server; the pod's end removes them.
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
#                                 provider block in it to route through (install_provider_route):
#                                 an anthropic-messages or an openai-responses block, whose apiKey
#                                 is `!cat <absolute file>`, which every agent and judge runs where
#                                 the operator keeps the key, or `!<absolute executable>`, a
#                                 command that prints a fresh key on each call (a build pod's
#                                 /work/bin/model-token); a judge takes the first form alone
#   LEGION_E2E_MODEL_PROVIDER_MODEL
#                                 provider, optional: the model id of that block a label's agents run
#                                 (default the judge's, claude-opus-4-8); the judge keeps its own
#   SKILL_SCENARIOS_MODEL         `profile`: standin, in place of every route above: the profile's
#                                 one provider block is the model stand-in (install_standin_route),
#                                 which model-standin.ts serves for each run of a scored scenario,
#                                 and its built-from's model line names it
# `judge` and `judge-omp` also need Docker: each judge runs in a container (cmd_judge_omp).
# Optional:
#   SKILL_SCENARIOS_WORK      work directory, outside any checkout, and with no `.omp` in it or in
#                             any directory above it (default ${TMPDIR:-/tmp}/skill-scenarios)
#   SKILL_SCENARIOS_PARALLEL  concurrent runs in a batch (default 5)
#   SKILL_SCENARIOS_TIMEOUT   seconds one run, or one turn of a spec run, may take (default 1500)
#   SKILL_SCENARIOS_ASKGATE_CASE  askgate: facts, link, upload, nochange, nochange-rebuttal,
#                                 nochange-block, untried or tried (required for that scenario)
#   SKILL_SCENARIOS_ASKGATE_MODE  askgate: warn or block, the agent's OMP_ASKGATE (unset: the
#                                 verdicts block and the marker checks warn)
#   SKILL_SCENARIOS_OMP           the Oh My Pi binary every command runs, <dir>/bin/omp, in place of
#                                 the one mise installed for the label's .omp-pin, and with no
#                                 mise: the label's installers (omp_mise), the superpowers probe,
#                                 launch and launch_rpc, each through rig_omp; its --version must
#                                 be the pin's release (rig_omp), as LEGION_TEST_OMP's is
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
#                                 require_loopback_env (scripts/e2e/lib/loopback-dsn.sh) before
#                                 anything connects to it or the batch writes the URL anywhere,
#                                 and refuses every host libpq reads from it but 127.0.0.1,
#                                 localhost and ::1. Then it refuses a URL with a value that the
#                                 connection string each run's Dispatch gets cannot carry
#                                 (run_database_url_env), a password with a character that does not
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
# For owner-walk:
#   SKILL_SCENARIOS_OWNER_WALK_CASE  the run's case, <kind>-<form>-<family> (required)
#   SKILL_SCENARIOS_PLANS_ANSWER  a directory holding plans-answer.json and plans-answer.source,
#                                 which `rig.sh plans-capture` writes (required)
#   SKILL_SCENARIOS_GUIDANCE_MODELS  forms A, C and D: "<claude model> <gpt model>", the ids the
#                                 guidance names (owner-walk.ts world), the ids the gateway lists
#   SKILL_SCENARIOS_LAUNCH_NOTICES  `world` (the default) or `none`: what an owner's launch request
#                                 holds of the world's notices (launch_request). `none` gives the
#                                 launch no notice, so the label's launch publishes none. The rig is
#                                 test infrastructure, and production never sets this: rig.test.ts's
#                                 notice row sets it for its control, which publishes a notice of
#                                 its own to another role
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
#   - It runs in the tmux session skill-scenarios-<digest>-<run> on the work directory's own tmux
#     server (<work>/tmux.sock), <run> being <scenario>-<label>-<n> (askgate-<case>-<label>-<n> for
#     askgate). The pane shows nothing: omp's output goes to <work>/runs/<run>/out.txt, and the
#     transcript, which grows as the agent works, to the .jsonl under <work>/runs/<run>/sessions.
#   - `gh` and `bun` stand-ins are first on its PATH (standins): no run reaches GitHub, and a bun
#     it reaches through PATH is the bun this script runs on, recorded. A bun named by its path or
#     run through `mise exec` bypasses the stand-in and goes unrecorded. Every container and tmux
#     session a work directory starts carries the directory's digest in its name.
#   - Its agent can still read the whole filesystem, the operator's home included, and write the
#     machine's /tmp, which nothing here cleans: a namespace sandbox needs user namespaces, which
#     Ubuntu's AppArmor policy denies unprivileged users. score.ts does not score a run that reads
#     a skill file outside its own label or a file of another label's checkout, or whose PR body
#     carries another run's line (unscored, testerProof). It flags, and still scores, a run whose
#     tool calls only name such a skill file or checkout (skillReaches), or name a path outside its
#     work directory, its label's checkout and the system locations (outsideReaches): a flag shows
#     the path was named, not that it was read. A path a call reaches without naming it, as a
#     glob from `/` does, is not caught.
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
#
# An owner's run (run_owner_walk, scored_session) starts from the label's own texts, through the
# label's one entry, packages/pi-legion/scripts/skill-scenarios/label-texts.sh, built once per label
# outside every checkout (load_label); a label without the entry is refused by name. The run gets its
# own Dispatch behind dispatch-proxy.ts, its world and its launch from label-texts.sh, the daemon
# and pod stand-ins, and then the label's Oh My Pi in RPC mode (launch_rpc, rpc.sh). The launch has
# no productionCheck, so the architect's prompt reads "Production check: off", right for a rig. A
# turn ends when it has ended, no turn runs, and no background work or reminder is pending; at its
# bound (SKILL_SCENARIOS_TIMEOUT) out.txt ends exit=bound and held.txt says what held the run.
# `seed.ts capture-scored` then reads back what the run left on Dispatch. Runs are serial: owners
# of one kind share one role token on the listener.
set -euo pipefail
# Nothing the rig starts inherits a service endpoint or credential from the caller: a Legion pane
# exports ENVOY_URL, DISPATCH_URL and their token files, a developer's shell can hold NATS
# credentials (packages/dispatch/e2e/run-server.sh strips the same three families), and e2e/api.ts,
# which seed.ts calls, addresses PLAYWRIGHT_BASE_URL with E2E_AGENT_TOKEN whenever it is set.
# PGHOSTADDR, PGSERVICE and PGSERVICEFILE go too, for the loopback guard services_up reads a given
# Postgres URL with (require_loopback_env, scripts/e2e/lib/loopback-dsn.sh): the guard reads the
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

# The tester-proof world (worker_fixture below): its repository and that repository's default
# branch, and the run's own issue, which run_tester_proof sets once seed.ts has filed it.
# run_tester_proof gives the daemon stand-in the repository and the branch as arguments, for its
# state. daemon-pane.go names them again, as rigRepository and rigDefaultBranch, which the pane's
# addressing names, and each must equal the value here.
worker_key=''
worker_repo=example/widgets
worker_default_branch=trunk

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
# The model stand-in's provider block and model, which install_standin_route writes into a
# profile; model-standin.ts answers any model a request names, and the transcript of a run on it
# names this one (rig.test.ts, "the model stand-in mode routes the provider to model-standin.ts").
# The base its block names until a run routes it to the run's own stand-in (route_model_standin) is
# a name that resolves nowhere, so a run that was not routed reaches no model at all.
model_standin=standin/standin-model
model_standin_unrouted=http://model-standin.invalid
# The owner-walk world's repository, which the label's engine and launch record (TestRigWorld,
# TestRigLaunch), its Dispatch project (OWNER_WALK_PROJECT of scored-runs-contracts.ts), and the
# stage of owner-walk.ts's OWNER_WALK_STAGES every run's world is of (run_owner_walk, cmd_cell).
owner_walk_repo=example/csv-importer
owner_walk_project=LWEVAL
owner_walk_stage=guidance

mkdir -p "$work"

# scripts/e2e/lib/rig.sh's bounded waits, held ports, and process start and stop. Each command
# points `evidence` at the directory its processes log under ($evidence/logs/<name>.log).
evidence=$work
# shellcheck disable=SC2034 # lib/rig.sh's until_true reads it
timeout_hook=
# shellcheck source-path=SCRIPTDIR/../../../.. source=scripts/e2e/lib/rig.sh disable=SC1091 # followed under shellcheck -x
. "$root/scripts/e2e/lib/rig.sh"
# packages/envoy/scripts/lib/pg-url.sh's pg_url_run and pg_url_without_password: pg_sql gives psql
# a given Postgres URL's password in PGPASSWORD alone, and safe_url prints that URL without it.
# shellcheck source-path=SCRIPTDIR/../../../.. source=packages/envoy/scripts/lib/pg-url.sh disable=SC1091 # followed under shellcheck -x
. "$root/packages/envoy/scripts/lib/pg-url.sh"
# rpc.sh: the run's RPC session (launch_rpc and its rpc_* functions and clock), which rpcRun in
# rig.test.ts sources too.
# shellcheck source=packages/pi-legion/scripts/skill-scenarios/rpc.sh disable=SC1091 # followed under shellcheck -x
. "$here/rpc.sh"

tag=skill-scenarios-$(printf '%s' "$work" | sha256sum | cut -c1-8)
tmux_socket=$work/tmux.sock
tmux() { command tmux -S "$tmux_socket" "$@"; }

# What a command started, for its exit trap: the run directory and its agent's tmux session, the
# pids lib/rig.sh's start_process sets (a run's own Dispatch server among them), and the services'
# ports, URLs and directory.
R='' session='' daemon_pid='' listener_pid='' dispatch_pid='' runs_pid='' batch_scenario=''
proxy_pid='' model_pid='' omp_pid='' rpc_fd=''
S='' envoy_port='' dispatch_port='' pg_url='' nats_url=''

label_dir() {
  [[ $1 =~ ^[a-z0-9][a-z0-9-]*$ ]] || fail "label '$1' is not lower-case letters, digits and hyphens"
  printf '%s\n' "$work/profiles/$1"
}

# The label's checkout, pin and profile name, recorded by `profile`, as the globals P (the label's
# directory), co, pin and profile that every command and helper below reads. RIG_TEXTS_BIN, the
# directory label-texts.sh builds the label's test binaries in, is the label's own label-texts/,
# which every process of the label inherits: label_texts's calls and the daemon stand-in's
# register_plan.
load_label() {
  P=$(label_dir "$1")
  [ -f "$P/ready" ] || fail "no profile for label $1: run \`rig.sh profile $1 <checkout>\` first"
  co=$(<"$P/checkout")
  pin=$(<"$P/pin")
  profile=$(<"$P/profile")
  export RIG_TEXTS_BIN=$P/label-texts
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
# SKILL_SCENARIOS_MODEL=standin takes the place of every route: the model stand-in's block
# (install_standin_route), whatever LEGION_E2E_MODEL_ROUTE says. Only a scored run starts a
# stand-in and routes its profile to it (scored_session): a run of another scenario on such a label
# reaches no model, and score.ts makes it a rig error (modelFailure).
# The gateway and key-file routes write their call record to DIR/key. The key-file route's key
# command names the key file's path, so it goes to DIR/model-key, apart from the record: removing
# the profile (DIR), the one way to rebuild a label, removes it, and a failed install removes it at
# once. The provider route writes no key command and no record: its block's own `!cat` reads the key.
install_model_route() {
  local co=$1 profile=$2 dir=$3
  case ${SKILL_SCENARIOS_MODEL:-} in
  standin)
    install_standin_route "$profile" "$dir/home"
    return
    ;;
  '') ;;
  *)
    echo "SKILL_SCENARIOS_MODEL is '$SKILL_SCENARIOS_MODEL': it is standin, or unset for the route LEGION_E2E_MODEL_ROUTE names" >&2
    return 1
    ;;
  esac
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
# its own name, with every field but its model lists as the operator wrote it (its apiKey among
# them, so the key stays where the operator keeps it and no copy of it is written), and with one
# model, MODEL: the block's own entry for that id when it has one (its thinking levels, say), else
# just the id, whose metadata Oh My Pi fills from its bundled catalog. Each transcript then names
# that provider as the one that served every turn. The block speaks anthropic-messages (a Claude
# model at the gateway's root) or openai-responses (a GPT model under its /v1), and reads its key
# with `!cat <absolute file>`, which a judge's container mounts read-only, or with
# `!<absolute executable>`, a command that prints a fresh key on each call. models.yml is written
# as JSON, which YAML reads as it stands.
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
    const { accessSync, constants, statSync } = await import("node:fs");
    const [file, name, model, out] = process.argv.slice(1);
    const fail = (why) => { console.error(`provider route: ${why}`); process.exit(1); };
    let doc;
    try { doc = Bun.YAML.parse(await Bun.file(file).text()); } catch (e) { fail(`cannot read ${file}: ${e.message}`); }
    const block = doc?.providers?.[name];
    if (!block || typeof block !== "object") fail(`${file} has no provider block ${name}`);
    if (block.api !== "anthropic-messages" && block.api !== "openai-responses")
      fail(`provider ${name} speaks ${block.api}, not anthropic-messages or openai-responses`);
    if (typeof block.baseUrl !== "string" || block.baseUrl === "") fail(`provider ${name} has no baseUrl`);
    const key = typeof block.apiKey === "string" ? block.apiKey : "";
    const command = /^!(\/\S+)$/.exec(key)?.[1];
    if (!/^!cat \/\S+$/.test(key) && command === undefined)
      fail(`provider ${name} does not read its key as "!cat <absolute file>" or "!<absolute executable>"`);
    if (command !== undefined) {
      let runs = false;
      try { runs = statSync(command).isFile() && (accessSync(command, constants.X_OK), true); } catch {}
      if (!runs) fail(`provider ${name} reads its key with ${command}, which is not an executable file`);
    }
    const { models: listed, modelOverrides: _o, discovery: _d, ...connection } = block;
    const entry = (Array.isArray(listed) ? listed : []).find((m) => m?.id === model) ?? { id: model };
    const models = { providers: { [name]: { ...connection, models: [entry] } } };
    await Bun.write(out, `${JSON.stringify(models, null, 2)}\n`);
  ' "$LEGION_E2E_MODEL_PROVIDER_FILE" "$LEGION_E2E_MODEL_PROVIDER" "$model_id" "$agent/models.yml" || return 1
  route_config "$agent/config.yml" "$LEGION_E2E_MODEL_PROVIDER/$model_id" install_provider_route
}

# install_standin_route PROFILE HOME writes the profile's models.yml and config.yml for the model
# stand-in (SKILL_SCENARIOS_MODEL=standin): one provider block, standin, speaking
# anthropic-messages with a literal key that the stand-in never reads, as the harness's stand-in
# profile writes one (writeStandinProfile, @legion/pi-shared/test/omp-harness), and one model,
# standin-model. Its baseUrl is model_standin_unrouted until a run routes the run's own copy of the
# profile to the stand-in it starts (route_model_standin).
install_standin_route() {
  local profile=$1 home=$2 agent
  agent=$home/.omp/profiles/$profile/agent
  mkdir -p "$agent"
  jq -n --arg base "$model_standin_unrouted/anthropic" --arg provider "${model_standin%%/*}" --arg model "${model_standin#*/}" '
    {providers: {($provider): {baseUrl: $base, auth: "apiKey", api: "anthropic-messages", apiKey: "standin-not-a-secret",
      models: [{id: $model, name: ("Stand-in " + $model), reasoning: false, input: ["text"],
        contextWindow: 200000, maxTokens: 8000, cost: {input: 0, output: 0, cacheRead: 0, cacheWrite: 0}}]}}}' \
    >"$agent/models.yml" || return 1
  route_config "$agent/config.yml" "$model_standin" install_standin_route
}

# route_config FILE MODEL WRITER writes the profile settings of a route with one model, MODEL: the
# handler limit, the one enabled model, every built-in provider disabled but the one MODEL's
# provider block is named after, and every model role on MODEL. WRITER names the function that
# wrote it.
route_config() {
  local file=$1 model=$2 role builtin
  {
    echo "# Written by rig.sh $3: $model is the one model, and no other provider"
    echo "# a session or a subagent could fall back to without a word is enabled."
    # The handler limit the gateway and key-file installers write, and which load_label requires
    # of every profile: AskGate's review is one tool call, and the default limit cuts it short.
    printf 'extensionHandlers:\n  toolCallTimeoutMs: 120000\n'
    printf 'enabledModels:\n  - %s\n' "$model"
    # Every built-in provider but the block's own name: a block named after one (a box's
    # `anthropic` block that points at its gateway) replaces that provider, and disabling the name
    # would disable the block too, leaving the agents no model.
    printf 'disabledProviders:\n'
    for builtin in anthropic amazon-bedrock bedrock-mantle google ollama llama.cpp lm-studio openai; do
      [ "$builtin" = "${model%%/*}" ] || printf '  - %s\n' "$builtin"
    done
    printf 'modelRoles:\n'
    for role in default smol slow vision plan commit tiny task advisor review oracle deep; do
      printf '  %s: %s\n' "$role" "$model"
    done
  } >"$file"
}

# route_model_line prints the `model <provider>/<model>` line the profile's route runs every turn
# on, for built-from, and nothing for a route that names no one model: the stand-in's, or the
# provider route's block and model. A scored run is held to it (scored-runs.ts modelProblem).
route_model_line() {
  if [ "${SKILL_SCENARIOS_MODEL:-}" = standin ]; then
    printf 'model %s\n' "$model_standin"
  elif [ "${LEGION_E2E_MODEL_ROUTE:-gateway}" = provider ]; then
    printf 'model %s/%s\n' "$LEGION_E2E_MODEL_PROVIDER" "${LEGION_E2E_MODEL_PROVIDER_MODEL:-$route_model_id}"
  fi
}

# route_model_standin HOME PROFILE BASE routes the stand-in block of the profile under HOME, a
# run's own copy of its label's, to the model stand-in at BASE (model-standin.ts's base URL).
route_model_standin() {
  local file=$1/.omp/profiles/$2/agent/models.yml provider=${model_standin%%/*}
  jq -e --arg provider "$provider" --arg unrouted "$model_standin_unrouted/anthropic" \
    '.providers[$provider].baseUrl == $unrouted' "$file" >/dev/null 2>&1 ||
    fail "$file holds no unrouted model stand-in block: its label was not built with SKILL_SCENARIOS_MODEL=standin"
  jq --arg provider "$provider" --arg base "$3/anthropic" '.providers[$provider].baseUrl = $base' "$file" >"$file.new"
  mv "$file.new" "$file"
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
  local label=$1 P co profile=skills-$1 envoy_json=''
  if [ $# = 4 ]; then
    [ "$3" = --envoy-json ] || fail "$usage"
    envoy_json=$(realpath -e -- "$4") || fail "--envoy-json $4 does not exist"
    jq -e 'type == "object"' "$envoy_json" >/dev/null 2>&1 || fail "--envoy-json $4 is not a JSON object"
  fi
  P=$(label_dir "$label")
  [ ! -e "$P" ] || fail "$P exists; remove it to rebuild label $label"
  co=$(cd "$2" && pwd -P)
  [ -f "$co/scripts/e2e/lib/install-plugin-profile.sh" ] || fail "$co is not a Legion checkout"
  mkdir -p "$P"
  on_exit "$(printf 'drop_unready_keys %q %q' "$P/ready" "$P/model-key")"
  printf '%s\n' "$co" >"$P/checkout"
  printf '%s\n' "$profile" >"$P/profile"
  # What the label runs: the checkout's working-copy commit and a digest of its skills tree, the
  # files the packed plugins stage into their dist/skills, listed in byte order (LC_ALL=C) so the
  # digest is the same in every locale.
  local commit installer_path=$PATH
  commit=$(checkout_commit "$co") || fail "cannot read the commit of $co with jj or git"
  {
    printf 'checkout %s\n' "$co"
    printf 'commit %s\n' "$commit"
    printf 'skills %s\n' "$(cd "$co" && find skills -type f -print0 | LC_ALL=C sort -z | xargs -0 sha256sum | sha256sum | cut -d' ' -f1)"
    [ -z "$envoy_json" ] || printf 'envoy-json %s\n' "$(jq -S . "$envoy_json" | sha256sum | cut -d' ' -f1)"
    # A label tree's manifest and the files it names (cmd_label_tree's record beside the tree).
    [ ! -f "$(dirname -- "$co")/label-tree" ] || grep -E '^manifest(-file)? ' "$(dirname -- "$co")/label-tree"
  } >"$P/built-from"
  (
    # shellcheck source=/dev/null
    source "$co/scripts/e2e/lib/omp-home.sh"
    make_omp_home "$P/home"
  )
  # The label's installers run the Oh My Pi mise installed for its pin, or the one
  # SKILL_SCENARIOS_OMP names, served to them as that install (omp_mise).
  [ -z "${SKILL_SCENARIOS_OMP:-}" ] || installer_path=$(omp_mise "$(<"$co/.omp-pin")"):$PATH
  # Both plugins: a tester-proof worker's pane loads the Legion extension, which exits a Legion
  # session that finds no Envoy plugin, and every plain session gets its Dispatch skills and tools
  # from the Envoy plugin.
  PATH=$installer_path bash "$co/scripts/e2e/lib/install-plugin-profile.sh" --package pi-envoy --profile "$profile" --home "$P/home" --dest "$P/pi-envoy" >"$P/install.log" 2>&1 ||
    fail "install-plugin-profile.sh --package pi-envoy failed; see $P/install.log"
  PATH=$installer_path bash "$co/scripts/e2e/lib/install-plugin-profile.sh" --package pi-legion --profile "$profile" --home "$P/home" --dest "$P/pi-legion" >>"$P/install.log" 2>&1 ||
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
    fail "the model route (${SKILL_SCENARIOS_MODEL:-${LEGION_E2E_MODEL_ROUTE:-gateway}}) could not be installed; see $P/install.log"
  route_model_line >>"$P/built-from"
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

# rig_omp PIN prints the Oh My Pi a run starts: the binary SKILL_SCENARIOS_OMP names when it is
# set, held to PIN, else the one mise installed for PIN (pinned_omp). .omp-pin names
# <backend>:<owner>/oh-my-pi@<release>-<build>, and `omp --version` prints omp/<release> alone, so
# the binary is held to the release. It must be <dir>/bin/omp, an install's own layout, which
# omp_mise serves to the label's installers as the pin's install.
rig_omp() {
  local pin=$1 omp=${SKILL_SCENARIOS_OMP:-} release printed
  if [ -z "$omp" ]; then
    pinned_omp "$pin"
    return
  fi
  [ -f "$omp" ] && [ -x "$omp" ] || fail "SKILL_SCENARIOS_OMP $omp is not an executable file"
  [ "$(basename -- "$omp")" = omp ] && [ "$(basename -- "$(dirname -- "$omp")")" = bin ] ||
    fail "SKILL_SCENARIOS_OMP $omp is not <dir>/bin/omp, the layout of an install of the pin"
  release=${pin##*@}
  release=${release%%-*}
  printed=$("$omp" --version 2>/dev/null </dev/null) || printed=
  printed=${printed%%$'\n'*}
  [ "$printed" = "omp/$release" ] ||
    fail "SKILL_SCENARIOS_OMP $omp prints --version '$printed', and the label's .omp-pin names $pin (omp/$release)"
  printf '%s\n' "$omp"
}

# omp_mise PIN prints a directory whose `mise` serves the label's own installers
# (install-plugin-profile.sh) the Oh My Pi SKILL_SCENARIOS_OMP names, held to PIN (rig_omp), where
# no mise is, as in a build pod: it answers `mise where PIN` with that install's directory, runs
# `mise x PIN -- <command>` as the command itself, and refuses any other call, naming it.
omp_mise() {
  local pin=$1 omp dir=$work/omp-mise
  omp=$(rig_omp "$pin") || exit 1
  mkdir -p "$dir"
  {
    echo '#!/usr/bin/env bash'
    echo '# Written by rig.sh omp_mise: mise for the one install SKILL_SCENARIOS_OMP names.'
    printf 'pin=%q install=%q\n' "$pin" "$(dirname -- "$(dirname -- "$omp")")"
    # shellcheck disable=SC2016 # the stand-in's own text, expanded when it runs
    printf '%s\n' \
      'case "${1:-} ${2:-}" in' \
      '"where $pin") printf "%s\n" "$install" ;;' \
      '"x $pin") [ "${3:-}" = -- ] || { echo "mise (rig.sh): x $pin takes -- and a command" >&2; exit 2; }; shift 3; exec "$@" ;;' \
      '*) echo "mise (rig.sh): $* is not served: only where and x of $pin, the Oh My Pi SKILL_SCENARIOS_OMP names" >&2; exit 2 ;;' \
      'esac'
  } >"$dir/mise"
  chmod +x "$dir/mise"
  printf '%s\n' "$dir"
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
  local P=$1 profile=$2 pin=$3 src=$SKILL_SCENARIOS_SUPERPOWERS skill dest probe version omp run
  omp=$(rig_omp "$pin") || exit 1
  # The pin's own install runs under `mise x`, which gives it the pin's tools; the binary
  # SKILL_SCENARIOS_OMP names runs alone, as in a pod with no mise.
  run=("$omp")
  [ -n "${SKILL_SCENARIOS_OMP:-}" ] || run=(mise x "$pin" -- "$omp")
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
        "${run[@]}" -p 'Call the read tool once on the path skill://writing-plans, and nothing else. Then reply with the first line of what it returned.' \
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

# owner_only FILE writes standard input to FILE, readable and writable by its owner alone. It removes
# FILE first, since a write onto an existing file keeps that file's mode, whatever the umask. The
# owner run's pane, request and answer can carry the batch's NATS URL, including a credential.
owner_only() {
  (umask 077 && rm -f -- "$1" && cat >"$1")
}

# Writes the variables every agent gets to $R/pane.env: a HOME copied from the label's, the
# label's profile, the run's stand-ins first on PATH, its own TMPDIR, and nothing else from the
# caller's environment. The file is its owner's alone, and later appends keep that mode.
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
  } | owner_only "$R/pane.env"
}

# The run's stand-ins in $R/bin: `gh`, which records the call and answers from $R/fixtures.json,
# and `bun`, which runs the bun this script runs on (a caller's `bun` may be a shim that resolves
# nothing under the agent's HOME) and records the call to $R/calls.jsonl as gh-standin.ts records
# its own. The score reads the tester's run of the CLI from that record. No `legion` stands in: an
# agent reaches the daemon stand-in through its `legion` tool alone, never `legion` from bash.
standins() {
  local bun_real
  bun_real=$(bun -e 'console.log(process.execPath)')
  mkdir -p "$R/bin"
  printf '#!/bin/sh\nexec %q %q gh "$@"\n' "$bun_real" "$here/gh-standin.ts" >"$R/bin/gh"
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

# launch NAME [OMP_ARG]... runs `omp -p "$(cat $R/prompt.txt)"` and then each OMP_ARG in $R/cwd
# under $R/pane.env, followed by $R/system-args (one --append-system-prompt argument, as shell text)
# when it exists, as the tmux session <tag>-<name>. It waits for the session to exit or time out. A
# timeout is the run's result (`exit=timeout`), not a rig failure, so this wait is not an
# until_true. A spec run's later turn passes --continue. Each launch starts out.txt again, so its
# wait reads only its own exit line. The pane's variables reach mise, which starts omp, through the
# launch file's environment, never through process arguments: the pane can carry the batch's NATS
# URL, including a credential. The file starts itself again with an empty environment, exports each
# pane line, then execs the launcher with the pane's lines as its whole environment.
launch() {
  local system='' extra='' omp run
  omp=$(rig_omp "$pin") || exit 1
  # As install_superpowers runs it: the pin's own install under `mise x`, the binary
  # SKILL_SCENARIOS_OMP names alone.
  run=$(printf %q "$omp")
  [ -n "${SKILL_SCENARIOS_OMP:-}" ] || run="mise x $(printf %q "$pin") -- $run"
  session=$tag-$1
  shift
  [ $# = 0 ] || extra=" $(printf '%q ' "$@")" extra=${extra% }
  [ ! -f "$R/system-args" ] || system=" $(<"$R/system-args")"
  cat >"$R/launch.sh" <<EOF
#!/usr/bin/env bash
[ "\${1:-}" = clean ] || exec env -i "\$BASH" "\$0" clean
cd $(printf %q "$R/cwd")
export -n PWD OLDPWD
mapfile -t vars <$(printf %q "$R/pane.env")
for var in "\${vars[@]}"; do export "\$var"; done
exec env -u SHLVL $run -p "\$(cat $(printf %q "$R/prompt.txt"))"$extra$system --session-dir $(printf %q "$R/sessions") >$(printf %q "$R/out.txt") 2>&1
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

# A run's or a live read's exit: its RPC session (launch_rpc's Oh My Pi), its daemon stand-in, its
# Dispatch proxy and model stand-in, its own Dispatch server, its agent's session, and every
# process that carries its SKILL_SCENARIO_RUN. The run's database stays in the batch's Postgres:
# services_down removes the container it started, and leaves a given server, with every database
# its runs made, to the operator. The services are this shell's own jobs, so each is waited for
# once it is killed: bash otherwise prints a `Killed` notice for it on stderr.
# shellcheck disable=SC2329 # on_exit's trap runs it
stop_run() {
  local pid
  [ -z "$rpc_fd" ] || { exec {rpc_fd}>&- && rpc_fd=''; } 2>/dev/null || true
  for pid in "$omp_pid" "$daemon_pid" "$proxy_pid" "$model_pid" "$dispatch_pid"; do
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
  launch "live-read-${P##*/}"
  bun "$here/score.ts" live-read "$R" "$co/skills" "$R/home/.omp/profiles/$profile/logs"
}

services_env=$work/services/env
# The Postgres SKILL_SCENARIOS_POSTGRES_URL names, written by services_up and read by own_dispatch,
# which runs in a process of its own (`rig.sh run`): a run takes the Postgres its batch brought up,
# never one its own environment names. The file is 0600, since the URL can carry a password, and
# its absence is how a run knows its batch started a container instead.
services_postgres=$work/services/postgres-url

# start_listener serves the Envoy listener on a port the kernel chose, held from that choice until
# the listener serves on it (lib/rig.sh's start_held), on the NATS nats_url names, and sets
# envoy_port.
start_listener() {
  ENVOY_API_TOKEN="$(<"$S/envoy-token")" ENVOY_LISTEN_HOST=127.0.0.1 \
    ENVOY_MACHINE_ID="$tag" NATS_URLS="$nats_url" \
    start_held listener envoy_port ENVOY_LISTEN_FD --env 'PORT={port}' -- "$S/envoy-listener"
  await_start listener "$listener_pid" 60 "the Envoy listener to answer /v1/sessions" listener_answers
}

# listener_up builds the Envoy listener from this checkout, makes the batch's two tokens, and
# starts the listener (start_listener) on the NATS nats_url names.
listener_up() {
  (cd "$root/packages/envoy" && go build -o "$S/envoy-listener" ./cmd/listener)
  (umask 077 && openssl rand -hex 24 >"$S/envoy-token" && printf 'e2e-token\n' >"$S/dispatch-token")
  start_listener
}

# listener_answers reads the listener's /v1/sessions with the rig's bearer, which reaches curl on
# standard input from printf, a shell builtin, so it is in no command line.
# shellcheck disable=SC2329 # await_start in start_listener runs it
listener_answers() {
  printf 'Authorization: Bearer %s' "$(<"$S/envoy-token")" |
    curl -fsS -o /dev/null -H @- "http://127.0.0.1:$envoy_port/v1/sessions"
}

# start_dispatch DATABASE_URL [CHECKOUT] starts the e2e harness's Dispatch server on that
# database, from CHECKOUT's run-server.sh (a scored run's label's), this checkout's by default. The
# harness's fake Envoy and GitHub, which run-server.sh names and this rig does not run, are port 1,
# where nothing listens; its broker switch is empty, so the server has no secrets broker.
# With no DISPATCH_E2E_PORT, run-server.sh serves Dispatch on a port the kernel chose, held until
# the server serves on it, and the server reports it (`msg="dispatch: listening"
# addr=127.0.0.1:<port>`), which sets dispatch_port. Each run has an evidence directory of its own,
# and the log is emptied first, so the line read is this run's own start.
start_dispatch() {
  local url=$1 log=$evidence/logs/dispatch.log
  : >"$log"
  DATABASE_URL=$url FAKE_ENVOY_PORT=1 FAKE_GITHUB_PORT=1 DISPATCH_E2E_AGENT_SECRETS_URL='' \
    start_process dispatch bash "${2:-$root}/packages/dispatch/e2e/run-server.sh"
  await_start dispatch "$dispatch_pid" 300 "Dispatch to report its port" \
    report_port "$log" 'msg="dispatch: listening" addr=127\.0\.0\.1:'
  dispatch_port=$(report_port "$log" 'msg="dispatch: listening" addr=127\.0\.0\.1:')
  await_start dispatch "$dispatch_pid" 60 "Dispatch to answer /api/v1/whoami" \
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
# URL with: the user info runs to the last `@` before the path, the query or the fragment, so a raw
# `@` in the password goes too. A user and password keeps its user; user info with no `:` is a
# token to nats.go, so it goes whole.
safe_url() {
  local url scheme rest user='' kept='' param key prefix authority info
  local -a params=()
  case $1 in
  postgres://* | postgresql://*) ;;
  *://*)
    prefix=${1%%://*}://
    rest=${1:${#prefix}}
    authority=${rest%%[/?#]*}
    if [[ $authority == *@* ]]; then
      info=${authority%@*}
      [[ $info == *:* ]] && info=${info%%:*}@ || info=''
      printf '%s%s%s%s\n' "$prefix" "$info" "${authority##*@}" "${rest:${#authority}}"
    else
      printf '%s\n' "$1"
    fi
    return
    ;;
  *)
    printf '%s\n' "$1"
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
# between them. psql is libpq, so the host it connects to is the host require_loopback_env read
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
# shellcheck disable=SC2329 # until_true in services_up runs it
nats_ready() {
  local hostport=${1#*://} line=''
  hostport=${hostport##*@}
  hostport=${hostport%%/*}
  # shellcheck disable=SC2016 # $1, $2 and $line are the inner bash's own, expanded there
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

# services_up [PARALLEL] brings up the batch's services: its Postgres and its NATS, the servers the
# operator gave (given_services) or two containers it starts, and the Envoy listener built from
# this checkout, which the services file ($services_env) names with the NATS and the listener
# token. PARALLEL is how many runs the batch runs at once, SKILL_SCENARIOS_PARALLEL (default 5)
# when it is not given; an owner-walk batch runs one at a time and gives 1.
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
# A given Postgres must be a postgres:// or postgresql:// URL, the one form safe_url and pg_sql take
# a password out of. It must hold no sslpassword parameter, a client key's passphrase: pg_sql takes
# only the password out of the URL it gives psql, and every user of this machine can read psql's
# arguments. Its query is read as safe_url reads it: from the first `?` past the user info, each
# key percent-decoded. Its URL is read with require_loopback_env (scripts/e2e/lib/loopback-dsn.sh)
# before anything connects to that server or this batch writes the URL anywhere. The guard reads
# the host with libpq, and pg_sql connects with psql, which is libpq, so the host psql reaches is
# the host the guard read. run_database_url_env then rebuilds, for a placeholder database, the
# connection string each run's Dispatch gets (own_dispatch), and refuses a value that string
# cannot carry, so such a URL is refused here, before anything starts, and not in every run. Both
# take the variable's name rather than its value, so their lines put no DSN into a `bash -x` trace.
services_up() {
  evidence=$S
  local parallel=${1:-${SKILL_SCENARIOS_PARALLEL:-5}} per_run needs admits fits pg_port nats_port pg_host run_url tool rest param key
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
    require_loopback_env pg_host SKILL_SCENARIOS_POSTGRES_URL "each run creates a database there" || fail "$pg_host"
    run_database_url_env run_url SKILL_SCENARIOS_POSTGRES_URL run_placeholder "each run's Dispatch opens its database there" || fail "$run_url"
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
  else
    connections_needed
    # A given Postgres's URL that a batch killed outright left would send this batch's runs there.
    rm -f -- "$services_postgres"
    # ghcr.io/cybermonklabs/legion/ci, the copy of pgvector/pgvector (.github/ci-images.yaml)
    docker run -d --name "$tag-pg" --cpus 2 --memory 2g --mount type=tmpfs,destination=/var/lib/postgresql \
      -e POSTGRES_PASSWORD=ci -p 127.0.0.1::5432 ghcr.io/cybermonklabs/legion/ci@sha256:2358fcba361ed2233a5ed81b5fe4ca779ccb304120ce531a3bf51c0ed7e2bc11 \
      -c max_connections="$needs" >/dev/null
    pg_port=$(docker port "$tag-pg" 5432/tcp | sed -n '1s/.*://p')
    [ -n "$pg_port" ] || fail "docker assigned Postgres no host port"
    pg_url="postgres://postgres:ci@127.0.0.1:$pg_port/postgres?sslmode=disable"
    # The hub's nats-server (lib/nats-image.sh reads testnats.HubImage): the listener and the daemon
    # refuse a nats-server below the floor packages/envoy/natsversion holds, every 2.10 among them.
    # shellcheck source-path=SCRIPTDIR/../../../.. source=scripts/e2e/lib/nats-image.sh disable=SC1091 # followed under shellcheck -x
    . "$root/scripts/e2e/lib/nats-image.sh"
    # shellcheck disable=SC2154 # nats-image.sh assigns it
    docker run -d --name "$tag-nats" --cpus 1 --memory 512m -p 127.0.0.1::4222 "$nats_image" -js >/dev/null
    nats_port=$(docker port "$tag-nats" 4222/tcp | sed -n '1s/.*://p')
    [ -n "$nats_port" ] || fail "docker assigned NATS no host port"
    nats_url=nats://127.0.0.1:$nats_port
    until_true 60 "Postgres to accept TCP connections" docker exec "$tag-pg" pg_isready -h 127.0.0.1 -p 5432 -U postgres
    until_true 60 "NATS to be ready" sh -c "docker logs '$tag-nats' 2>&1 | grep -q 'Server is ready'"
  fi
  listener_up
  {
    echo "ENVOY_URL=http://127.0.0.1:$envoy_port"
    echo "ENVOY_NATS_URL=$nats_url"
    echo "ENVOY_TOKEN_FILE=$S/envoy-token"
  } | owner_only "$services_env"
  note "services up: listener :$envoy_port, NATS $(safe_url "$nats_url"), Postgres $(safe_url "$pg_url")"
}

# services_down stops and removes what this batch started and made: the listener, the two
# containers services_up starts when it is given no servers, the services file, the given
# Postgres's URL, and the lock. A given Postgres and a given NATS keep running, and every database
# a run made on that Postgres (own_dispatch) stays there for the operator to drop.
# shellcheck disable=SC2329 # stop_batch, an exit trap, runs it
services_down() {
  stop_tree "${listener_pid:-}"
  given_services || docker rm -fv "$tag-nats" "$tag-pg" >/dev/null 2>&1 || true
  rm -f "$services_env" "$services_postgres" "$S/up"
}

# own_dispatch SEED [CHECKOUT] gives the run a Dispatch of its own and seeds it with `seed.ts SEED`,
# the world the scenario needs (project, or brainstorm): a database named after the run in the
# batch's Postgres, and a Dispatch server on it from CHECKOUT's run-server.sh (start_dispatch),
# which stop_run stops. It records the Postgres, the database and the server in $R/dispatch.json,
# which score.ts reads, and sets dispatch_port, and pg_url, a login to the batch's Postgres, which
# label_texts gives the label's entry. The batch's Postgres is the server $services_postgres names,
# and the batch's own container when that file is absent. On a given server, services_up checks the
# URL with run_database_url_env, which takes the variable's name; here own_dispatch calls
# run_database_url (scripts/e2e/lib/loopback-dsn.sh) on the URL read from that file, with the run's
# database as its dbname. Dispatch's pgx driver reads the resulting keyword/value connection string.
own_dispatch() {
  local seed=$1 database=run_${R##*/} pg_port postgres url
  database=${database//-/_}
  # Postgres folds an unquoted name to lower case, and a URL's name is read as written.
  database=${database,,}
  # Postgres cuts a longer name to 63 bytes, and two runs whose names agree that far would share it.
  [ "${#database}" -le 63 ] || fail "the run's database name $database is longer than Postgres's 63 bytes"
  if [ -f "$services_postgres" ]; then
    postgres=$(<"$services_postgres")
    run_database_url url SKILL_SCENARIOS_POSTGRES_URL "$postgres" "$database" "this run creates a database there" || fail "$url"
    # A given server must let its login make a database. Nothing here checks that: the refusal a
    # server without the right gives is Postgres's own, in the run's fixture.log.
    pg_sql "$postgres" "CREATE DATABASE $database" >>"$R/fixture.log" 2>&1 ||
      fail "CREATE DATABASE $database on $(safe_url "$postgres") failed; see $R/fixture.log"
    pg_url=$postgres
    postgres=$(safe_url "$postgres")
  else
    pg_port=$(docker port "$tag-pg" 5432/tcp | sed -n '1s/.*://p')
    postgres=$(docker inspect -f '{{.Id}}' "$tag-pg")
    [ -n "$pg_port" ] && [ -n "$postgres" ] || fail "no Postgres container $tag-pg for the run's Dispatch"
    docker exec "$tag-pg" psql -U postgres -v ON_ERROR_STOP=1 -qc "CREATE DATABASE $database" >>"$R/fixture.log" 2>&1 ||
      fail "CREATE DATABASE $database failed; see $R/fixture.log"
    pg_url="postgres://postgres:ci@127.0.0.1:$pg_port/postgres?sslmode=disable"
    url="postgres://postgres:ci@127.0.0.1:$pg_port/$database?sslmode=disable"
  fi
  start_dispatch "$url" "${2:-}"
  jq -n --arg postgres "$postgres" --arg database "$database" --argjson port "$dispatch_port" \
    '{postgres:$postgres, database:$database, port:$port, url:("http://127.0.0.1:" + ($port|tostring))}' >"$R/dispatch.json"
  (cd "$root/packages/dispatch" && DISPATCH_E2E_PORT=$dispatch_port bun "$here/seed.ts" "$seed") >>"$R/fixture.log" 2>&1 ||
    fail "seed.ts $seed failed; see $R/fixture.log"
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

# dispatch_session NAME [CAPTURE [VAR=VALUE...]] runs $R/prompt.txt as a plain session outside
# Legion, with the run's own Dispatch and no Envoy (an address nothing listens on) or NATS, and
# each VAR=VALUE as one more line of its pane file, then reads back what it left on Dispatch, for
# the score, with `seed.ts CAPTURE` (default `capture`).
dispatch_session() {
  local name=$1 capture=${2:-capture}
  shift $(($# < 2 ? $# : 2))
  base_env
  standins
  {
    echo "DISPATCH_URL=http://127.0.0.1:$dispatch_port"
    echo "DISPATCH_TOKEN_FILE=$work/services/dispatch-token"
    echo "ENVOY_URL=http://127.0.0.1:1"
  } >>"$R/pane.env"
  [ $# = 0 ] || printf '%s\n' "$@" >>"$R/pane.env"
  launch "$name"
  (cd "$root/packages/dispatch" && DISPATCH_E2E_PORT=$dispatch_port bun "$here/seed.ts" "$capture" "$R/fixture.json" "$R") \
    >>"$R/fixture.log" 2>&1 || fail "the capture failed; see $R/fixture.log"
}

run_ask_on_message() {
  local name=$1 issue message
  own_dispatch project
  seed_dispatch ask-on-message
  printf 'On Dispatch issue %s, the Dispatch owner session posted its plan for the rest of the work in this message: dispatch://%s/message/%s. Open an ask on %s asking Sami whether to go ahead with that plan. Do nothing else.\n' \
    "$issue" "$issue" "$message" "$issue" >"$R/prompt.txt"
  dispatch_session "$name"
}

# The export the owner's message names, in the agent's working directory: 430 stranded issues, of
# which only every 23rd (18) has the owner option A needs. Its example.invalid addresses give an
# agent a second reason to drop A; measureBeforeAsk says how the first runs split on it.
run_measure_before_ask() {
  local name=$1 issue message
  own_dispatch project
  seed_dispatch measure-before-ask
  awk 'BEGIN { print "key,title,component,owner"
    for (n = 1; n <= 430; n++) printf "ISS-%d,Stranded issue %d,billing-api,%s\n", 1000 + n, n, (n % 23 == 0 ? "owner" n "@example.invalid" : "") }' \
    >"$R/cwd/stranded.csv"
  printf 'On Dispatch issue %s, the Dispatch owner session posted three ways to finish its work in this message: dispatch://%s/message/%s. The export it names, stranded.csv, is in your working directory. Open an ask on %s asking Sami which way to go. Change nothing else.\n' \
    "$issue" "$issue" "$message" "$issue" >"$R/prompt.txt"
  dispatch_session "$name"
}

# The person's words are one comment on the run's own issue, and AskGate reviews each gated
# Dispatch write the agent makes. The case's prompt names that issue, and every case but `facts` and
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
  local name=$1 askCase=${SKILL_SCENARIOS_ASKGATE_CASE:-} mode=${SKILL_SCENARIOS_ASKGATE_MODE:-} issue message ref ask billing one
  case $mode in '' | warn | block) ;; *) fail "SKILL_SCENARIOS_ASKGATE_MODE is '$mode', not warn or block" ;; esac
  printf '%s\n' "$askCase" >"$R/case.txt"
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
  nochange-rebuttal) printf 'Open an ask on Dispatch issue %s with exactly this question and these options, and nothing else, and send it with the flag --advisor-rebuttal whose value is this line: "The person asked for the move, so keeping 02:00 needs their authority." The flag is not dispatch'"'"'s own: AskGate, the review of each gated Dispatch write in this session, reads it and keeps it as a shell comment at the end of the command'"'"'s first line before dispatch runs, so `dispatch ask --help` does not list it and `dispatch ask --dry-run` refuses it. Send the ask once, with the flag. %s Question: "%s" Options: "Keep it at 02:00", "Move it to 04:00".\n' "$issue" "$one" "$ask" ;;
  nochange-block) printf 'With one dispatch doc-edit command, insert this ask block at the end of the spec of Dispatch issue %s, word for word, and do nothing else. Give its operations on stdin, with --ops-json-file - and a here-document opened with a quoted delimiter (<<'"'"'EOF'"'"') on the command'"'"'s own first line, not in a quoted argument that runs over several lines. %s\n\n:::ask{#export-time urgency="med"}\n%s\n\n- Keep it at 02:00\n- Move it to 04:00\n:::\n' "$issue" "$one" "$ask" ;;
  untried) printf 'Open an ask on Dispatch issue %s with exactly this question and these options, and nothing else. Do not run the command it names yourself. %s Question: "Can you run %s and paste its output here? Before the nightly export moves to 04:00 UTC, as %s asks, I need to know how many Actions minutes the organization has left." Options: "Done", "Cannot run it".\n' "$issue" "$one" "$billing" "$ref" ;;
  tried) printf 'First run %s yourself. It will fail. Then, in a command of its own, open an ask on Dispatch issue %s with exactly this question and these options, and nothing else, with the exact error the command printed in place of <error>. %s Question: "I ran %s and it failed with <error>. Can you run it with your access and paste its output here? Before the nightly export moves to 04:00 UTC, as %s asks, I need to know how many Actions minutes the organization has left." Options: "Done", "Cannot run it".\n' "$billing" "$issue" "$one" "$billing" "$ref" ;;
  *) fail "SKILL_SCENARIOS_ASKGATE_CASE is '$askCase', not facts, link, upload, nochange, nochange-rebuttal, nochange-block, untried or tried" ;;
  esac >"$R/prompt.txt"
  dispatch_session "$name" capture-askgate ${mode:+"OMP_ASKGATE=$mode"}
}

# A tiny to-do CLI in a scratch git repository with no Dispatch configuration and no word of due
# dates. The run's own Dispatch holds one project (seed.ts brainstorm: TODO, with two unrelated
# issues whose titles name the "todo CLI", so a search for it finds the project), and
# `seed.ts capture-brainstorm` reads back the issue this run's own transcript created, for
# brainstormSurface.
run_brainstorm() {
  local name=$1
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
  dispatch_session "$name" capture-brainstorm
}

# seed_ts MODE ARG... runs `seed.ts MODE ARG...` against the run's own Dispatch (own_dispatch), its
# output appended to $R/fixture.log. A failure is the rig's, never the run's.
seed_ts() {
  (cd "$root/packages/dispatch" && DISPATCH_E2E_PORT=$dispatch_port bun "$here/seed.ts" "$@") \
    >>"$R/fixture.log" 2>&1 || fail "seed.ts $1 failed; see $R/fixture.log"
}

# judge_ts MODE ARG... runs `judge.ts MODE ARG...` with SKILL_SCENARIOS_WORK at this rig's work
# directory, its output appended to $R/judge.log.
judge_ts() {
  SKILL_SCENARIOS_WORK=$work bun "$here/judge.ts" "$@" >>"$R/judge.log" 2>&1 ||
    fail "judge.ts $1 failed; see $R/judge.log"
}

# next_run SCENARIO LABEL prints one above the highest <n> of a run directory
# $work/runs/<SCENARIO>-<LABEL>-<n>, or 1 when there is none.
next_run() {
  local dir n highest=0
  for dir in "$work/runs/$1-$2-"*; do
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

# require_superpowers SCENARIO fails unless the label (P) was built with SKILL_SCENARIOS_SUPERPOWERS.
require_superpowers() {
  [ -f "$P/superpowers" ] ||
    fail "$1 needs a label built with SKILL_SCENARIOS_SUPERPOWERS (the superpowers plugin's directory): remove $P and run \`rig.sh profile ${P##*/} <checkout>\` again with it set"
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
    if [ "$turn" = 1 ]; then launch "$name"; else launch "$name" --continue; fi
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

# run_spec SCENARIO NAME: a spec-writing or spec-rewrite run on the source SKILL_SCENARIOS_SOURCE
# names. The source goes to $R/source/ and its name to $R/source.txt. seed.ts seeds the run's issue
# from it, spec_turns runs the conversation, and `seed.ts capture-spec` reads back what the run
# wrote. The prompts name no skill.
run_spec() {
  local scenario=$1 name=$2 source issue prompt
  source=$(source_dir "$scenario" "${SKILL_SCENARIOS_SOURCE:-}") || exit 1
  require_superpowers "$scenario"
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
    key_file=$(jq -r '[.providers[].apiKey] | first // empty' "$agent/models.yml") || key_file=
    key_file=${key_file#'!cat '}
    case $key_file in /*) ;; *) fail "judge-omp: $agent/models.yml does not read its key as \"!cat <absolute file>\"" ;; esac
    case $key_file in *[!A-Za-z0-9/._-]*) fail "judge-omp: the key file's path '$key_file' holds a character a mount cannot name plainly" ;; esac
    cp -- "$agent/models.yml" "$keys/models.yml"
    dirname -- "$key_file"
    return 0
  fi
  if [ "${LEGION_E2E_MODEL_ROUTE:-gateway}" = key-file ]; then
    host_command=$P/model-key/anthropic-key
    [ -f "$host_command" ] || fail "judge-omp: the key-file profile $P has no key command $host_command"
    # The key command assigns the key file's path with printf %q: read it as bash reads that line.
    key_file=$(bash -c "$(grep -m1 '^key_file=' "$host_command")"$'\nprintf %s "$key_file"')
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
  # shellcheck disable=SC2016 # the key command's own text, which expands when it runs
  (umask 077 && printf '#!/bin/sh\nkey=$(cat %s) && [ -n "$key" ] && printf "%%s\\n" "$key"\n' "$read_from" >"$keys/anthropic-key")
  chmod 0700 "$keys/anthropic-key"
  sed "s#!exec $host_command\"#!exec /opt/key/anthropic-key\"#g" "$agent/models.yml" >"$keys/models.yml"
  if ! grep -q '!exec /opt/key/anthropic-key"' "$keys/models.yml" || grep -qF "$host_command" "$keys/models.yml"; then
    fail "judge-omp: $agent/models.yml does not name the key command $host_command"
  fi
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
cmd_judge_omp() {
  [ $# = 3 ] || fail "usage: rig.sh judge-omp <ste|bare> <dir> <output file name>"
  local kind=$1 dir output=$3 skills status=0 omp P agent keys key_file h=/home/judge
  case $kind in ste) skills=(--skills asd-ste100) ;; bare) skills=(--no-skills) ;; *) fail "judge-omp: '$kind' is ste or bare" ;; esac
  dir=$(cd "$2" && pwd -P) || fail "judge-omp: $2 is not a directory"
  [ -f "$dir/prompt.txt" ] || fail "judge-omp: $dir has no prompt.txt"
  case $output in
  "" | . | .. | */* | prompt.txt | sessions | out.txt | judge-model.txt | .tmp) fail "judge-omp: '$output' is not the name of an answer file in $dir" ;;
  esac
  case "$(omp_ancestor "$dir" || true)" in "") ;; *) fail "judge-omp: $dir is under $(omp_ancestor "$dir"), whose plugins Oh My Pi would load" ;; esac
  case ${LEGION_E2E_MODEL_ROUTE:-gateway} in gateway | key-file | provider) ;; *) fail "judge-omp: LEGION_E2E_MODEL_ROUTE is '$LEGION_E2E_MODEL_ROUTE': it is gateway, key-file or provider" ;; esac
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
  # token it refuses to mint, a manifest GET it drops) and serves the next, so the pull is asked
  # three times a couple of seconds apart before the rig refuses. Only the last attempt's own
  # words are kept, and only for the refusal: an attempt the next one makes good says nothing.
  if ! docker image inspect "$judge_image" >/dev/null 2>&1; then
    local pulled='' pull_error='' attempt
    for attempt in 1 2 3; do
      pull_error=$(docker pull --quiet "$judge_image" 2>&1 >/dev/null) && {
        pulled=1
        break
      }
      [ "$attempt" -eq 3 ] || sleep 2
    done
    [ -n "$pulled" ] || fail "judge-omp: docker could not pull $judge_image in 3 attempts: $pull_error"
  fi
  docker run --rm --init --cpus 2 --memory 4g --user "$(id -u):$(id -g)" --read-only \
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
      exec timeout "$1" "$2" "${@:3}"' - "${SKILL_SCENARIOS_TIMEOUT:-1500}" "/opt/omp/$(basename "$omp")" \
    -p "$(<"$dir/prompt.txt")" --no-extensions "${skills[@]}" \
    --model "$judge_model" --thinking "$judge_thinking" --session-dir "$dir/sessions" </dev/null >"$dir/out.txt" 2>&1 || status=$?
  echo "exit=$status" >>"$dir/out.txt"
  rm -rf -- "$keys"
  return "$status"
}

# cmd_judge RUN... runs judge.ts's prompts on each spec run.
cmd_judge() {
  [ $# -ge 1 ] || fail "usage: rig.sh judge <run dir>..."
  SKILL_SCENARIOS_WORK=$work exec bun "$here/judge.ts" run "$@"
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
# run_tester_proof files first): a bare remote whose post-receive hook logs every push, the issue
# workspace cloned from it, the implementer's commit on legion/<key>, the PR the gh stand-in serves
# (worker_routes), and the implementer's handoff body at $R/implement.json, which run_tester_proof
# records on the issue. The fixture's content is the same in every run; the PR number is 1000 plus
# <index>, the run's index in its batch, and the commits are dated <index> seconds after <start>,
# the batch's start, so no two runs of a batch share a PR number or a head (two runs can meet in a
# scratch file in the shared /tmp), and each batch's heads carry its own start time.
# The CLI must meet every requirement the seeded spec marks, and the issue's Verification document
# holds the check that the implementer's seeded result and the tester's own run are against.
# testerProof fails a run that
# pushes a change to legion/<key>, and it cannot tell a tester that pinned a real defect with a red
# test, which roles/core/tester.md asks for, from one that went off-task. A defect left in the
# fixture turns correct testing into a fail.
worker_fixture() {
  local index=$1 start=$2 src C1 body when worker_pr worker_pr_url jj_setup_config pr
  worker_pr=$((1000 + index))
  worker_pr_url=https://github.com/$worker_repo/pull/$worker_pr
  when="@$((start + index)) +0000"
  git init -q --bare -b "$worker_default_branch" "$R/remote.git"
  cat >"$R/remote.git/hooks/post-receive" <<EOF
#!/bin/sh
while read old new ref; do echo "\$(date -u +%FT%T.%3NZ) \$ref \$old \$new" >>$(printf %q "$R/pushes.log"); done
EOF
  chmod +x "$R/remote.git/hooks/post-receive"
  src=$R/src
  git init -q -b "$worker_default_branch" "$src"
  g() {
    GIT_AUTHOR_DATE=$when GIT_COMMITTER_DATE=$when \
      git -C "$src" -c user.name=Fixture -c user.email=fixture@example.invalid -c commit.gpgsign=false "$@"
  }
  printf '# widgets\n\nSmall CLI helpers.\n' >"$src/README.md"
  g add -A && g commit -qm "init"
  g push -q "$R/remote.git" "$worker_default_branch"
  g checkout -qb "legion/$worker_key"
  cat >"$src/greet.ts" <<'EOF'
export function greet(name: string): string {
  return `Hello, ${name.trim()}!`;
}

if (import.meta.main) {
  const name = Bun.argv[2];
  if (name === undefined || name.trim() === "") {
    console.error("usage: greet.ts <name>");
    process.exit(2);
  }
  console.log(greet(name));
}
EOF
  cat >"$src/greet.test.ts" <<'EOF'
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
  # Isolated from the operator's own jj configuration (fsmonitor.backend = "watchman" on a devbox
  # that runs it): a bare JJ_CONFIG naming only fsmonitor.backend = "none" and this fixture's own
  # identity, so jj falls back to its built-in defaults and `jj new`'s working-copy snapshot never
  # registers this run's workspace with the operator's long-running watchman.
  jj_setup_config=$R/jj-setup.toml
  printf '[user]\nname = "Rig Worker"\nemail = "rig@example.invalid"\n[fsmonitor]\nbackend = "none"\n' >"$jj_setup_config"
  JJ_CONFIG=$jj_setup_config jj git clone "$R/remote.git" "$R/ws" >/dev/null 2>&1
  (cd "$R/ws" && JJ_CONFIG=$jj_setup_config jj bookmark track "legion/$worker_key" --remote origin && JJ_CONFIG=$jj_setup_config jj new "legion/$worker_key") >/dev/null 2>&1
  body="Dispatch: $worker_key

Adds \`greet(name)\` and \`bun greet.ts <name>\`.

## Verification

**Threads:** 0 resolved, 0 unresolved.

**Checks:**
- Check k1 (before_merge): pass at ${C1:0:12}, recorded by implementer. Ran \`bun greet.ts '  Ada  '\` and read \`Hello, Ada!\`, so the name is trimmed, and \`bun greet.ts\` with no name exited 2 and printed the usage line.

**Fast-follow:** none.

**Chain:** not stacked."
  pr=$(jq -n --arg head "$C1" --arg body "$body" --arg url "$worker_pr_url" --arg ref "legion/$worker_key" \
    --arg key "$worker_key" --arg base "$worker_default_branch" --argjson number "$worker_pr" '
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

# handoff_line WORD VERSION ISSUE prints the sentence the daemon's task names a predecessor's
# handoff record in (task, packages/daemon/internal/workflow/effects.go), read from the daemon's
# copy of it with its placeholders filled. That copy is the one a workflow test holds to the task
# the daemon writes (TestTheHandoffLineExtractIsTheSentenceTheTaskWrites), so the rig keeps no
# sentence of its own that could drift from the daemon's.
handoff_line() {
  local extract=$root/packages/daemon/internal/workflow/handoff_line_extract.txt line
  line=$(awk 'body { print; exit } /^$/ { body = 1 }' "$extract") || fail "could not read $extract"
  [[ -n $line ]] || fail "$extract holds no sentence after the blank line under its header"
  line=${line//"{word}"/"$1"}
  line=${line//"{version}"/"$2"}
  printf '%s\n' "${line//"{issue}"/"$3"}"
}

run_tester_proof() {
  local name=$1 n=$2 index=$3 start=$4 state project port pr head version handoff
  # The run's own Dispatch, and its own issue on it, whose newest implement record the tester
  # reads and whose newest test record it writes.
  own_dispatch project
  seed_ts tester-proof "$R/fixture.json"
  worker_key=$(jq -r .issue "$R/fixture.json")
  worker_fixture "$index" "$start" >>"$R/fixture.log" 2>&1 || fail "the fixture failed; see $R/fixture.log"
  seed_ts implement-handoff "$R/fixture.json" "$R/implement.json" "$(jq -r .head "$R/world.json")"
  version=$(jq -r .implement.version "$R/fixture.json")
  read -r pr head < <(jq -r '"\(.pr) \(.head)"' "$R/world.json")
  # The architect's assignment, with the Handoff line the daemon's task names the predecessor's
  # record in (handoff_line).
  handoff=$(handoff_line implement "$version" "$worker_key")
  printf 'Architect → tester, %s. The implementer has finished: PR #%s (%s, repository %s) is at %s. %s Test it and finish your phase.\n' \
    "$worker_key" "$pr" "https://github.com/$worker_repo/pull/$pr" "$worker_repo" "$head" "$handoff" >"$R/prompt.txt"
  state=$R/state
  # Each run its own Legion project, so concurrent runs claim distinct role tokens on one listener.
  # A project token is lower-case letters and digits only (isLegionProjectToken), so the label's
  # hyphens go and `r` separates it from the run number.
  project=skills${P##*/}r$n
  project=${project//-/}
  mkdir -p "$state/gh" "$state/secrets"
  chmod 0700 "$state/secrets"
  (umask 077 && openssl rand -hex 16 >"$state/secrets/boot")
  printf '[user]\nname = "Rig Worker"\nemail = "rig@example.invalid"\n[ui]\npaginate = "never"\n' >"$R/jj.toml"
  # Port 0: the kernel assigns the stand-in's port, which it prints, so concurrent runs never race.
  start_process daemon bun "$here/daemon-standin.ts" 0 "$R/daemon.log" "$state/secrets/boot" "$project" "$worker_key" tester \
    "$worker_repo" "$worker_default_branch"
  await_start daemon "$daemon_pid" 30 "the daemon stand-in to listen" grep -q '^listening on ' "$R/logs/daemon.log"
  port=$(sed -n 's|^listening on http://127\.0\.0\.1:||p' "$R/logs/daemon.log")
  base_env
  standins
  # The pane the label's checkout's daemon would give the tester, over the base every agent gets:
  # worker.env and system-args. standins runs first, since worker-pane.ts names its $R/bin/gh, the
  # recording stand-in, as the pane's real gh.
  bun "$here/worker-pane.ts" "$R" "$R/pane.env" "$port" "$profile" "$project" "$worker_key" tester "$co"
  tester_pane
  launch "$name"
  # What the run left on its issue: every handoff record, each with its body, for testerProof.
  seed_ts capture-handoffs "$R/fixture.json" "$R"
}

# tester_pane writes the tester's pane, $R/pane.env: worker.env (worker-pane.ts's), the batch's
# services, the run's own Dispatch, and git, jj and gh as the tester's. The services carry the
# batch's NATS URL, which can hold a credential, so the pane is its owner's alone (owner_only).
tester_pane() {
  {
    cat "$R/worker.env" "$services_env"
    echo "DISPATCH_URL=http://127.0.0.1:$dispatch_port"
    echo "DISPATCH_TOKEN_FILE=$work/services/dispatch-token"
    echo "GIT_CONFIG_COUNT=0"
    echo "JJ_USER=Rig Worker"
    echo "JJ_EMAIL=rig@example.invalid"
    echo "JJ_CONFIG=$R/jj.toml"
    echo "GH_REPO=$worker_repo"
  } | owner_only "$R/pane.env"
}

# ---- The scored scenarios: a label's own texts, and an owner in RPC mode ----

# require_label_texts CHECKOUT LABEL fails, naming the label and the file, unless CHECKOUT holds
# the label's one entry (label-texts.sh): a label older than the entry has no code that gives its
# texts, and the rig writes none of its own.
require_label_texts() {
  [ -f "$1/packages/pi-legion/scripts/skill-scenarios/label-texts.sh" ] ||
    fail "label $2: its checkout $1 has no packages/pi-legion/scripts/skill-scenarios/label-texts.sh, the entry that gives a label's own texts, so it runs no scored scenario"
}

# label_texts VERB [REQUEST ANSWER] runs the label's entry (co, the label's checkout) with the
# batch's Postgres as LEGION_TEST_PG_DSN, where its world walks, and the label's RIG_TEXTS_BIN
# (load_label), where its binaries are, its output to $R/texts/label-texts.log, or to the label's
# own log for `build`. A failure names the first failure line of the label's test
# (`<file>_test.go:<line>: …`), or else the entry's own last line.
label_texts() {
  local log=${R:+$R/texts/label-texts.log} why
  [ -n "$log" ] || log=$P/label-texts.log
  mkdir -p "$(dirname -- "$log")"
  LEGION_TEST_PG_DSN=$pg_url bash "$co/packages/pi-legion/scripts/skill-scenarios/label-texts.sh" "$@" >>"$log" 2>&1 && return 0
  why=$(grep -m1 -E '^[[:space:]]+[a-z_]+_test\.go:[0-9]+: ' "$log" | sed 's/^[[:space:]]*//') ||
    why=$(tail -n 1 "$log")
  fail "label-texts.sh $1 of label ${P##*/} failed: $why; see $log"
}

# owner_walk_case prints the case SKILL_SCENARIOS_OWNER_WALK_CASE names, and fails, naming every
# case owner-walk.ts gives, when it is unset or names none of them.
owner_walk_case() {
  local cases value=${SKILL_SCENARIOS_OWNER_WALK_CASE:-}
  cases=$(bun "$here/owner-walk.ts" cases | cut -d' ' -f1) || fail "owner-walk.ts cases failed"
  [ -n "$value" ] ||
    fail "SKILL_SCENARIOS_OWNER_WALK_CASE is unset: an owner-walk run takes one case, <kind>-<form>-<family>, of $(paste -sd' ' <<<"$cases")"
  grep -qxF -- "$value" <<<"$cases" ||
    fail "SKILL_SCENARIOS_OWNER_WALK_CASE is '$value', none of owner-walk's cases: $(paste -sd' ' <<<"$cases")"
  printf '%s\n' "$value"
}

# world_request FIXTURE prints the world request (C2's WorldRequest) of owner-walk's world as
# seed.ts owner-walk filed it (FIXTURE, OwnerWalkFixture), at the owner's moment: for a root owner,
# R admitted with its spec approved (its gate open at the approved version, the spec's real
# document) and G its backlog child; for a child owner, R and C implementing and G C's backlog
# child, with no gate. Each issue keeps its title, parent and status as Dispatch holds them.
world_request() {
  jq --arg workspace "$R/cwd" --arg repository "$owner_walk_repo" '
    .kind as $kind | (.issues | map({key, value: .}) | from_entries) as $by
    | (.issues[] | select(.part == "root")) as $root
    | (if $kind == "root" then [$root.key, .planned] else [$root.key, .owner, .planned] end) as $keys
    | {project, repository: $repository, defaultBranch: "main", workspace: $workspace,
       issues: [$keys[] | $by[.] | {key, title, parent, tree: $root.key, status,
         phase: (if .part == "planned" then null elif $kind == "root" then "admitted" else "implementing" end)}],
       gate: (if $kind == "root"
         then {issue: $root.key, step: "spec", version: $root.spec.approvedVersion, approved: true, artifactId: $root.spec.artifact}
         else null end),
       subject: .owner, plan: null, earlier: [], pullRequest: null, moment: "owner", role: "architect"}' "$1"
}

# launch_request WORLD_REQUEST WORLD_ANSWER DAEMON_URL [GUIDANCE] prints the launch request (C2's
# LaunchRequest) of the owner's claim: the world's project, the run's state directory and
# workspace, the daemon stand-in, the batch's listener, NATS and Envoy token, the run's boot token,
# the gh stand-in, the pane's PATH (base_env's), the architect's claim on the owner's issue in R's
# tree, an empty file of deployment instructions (the rig is no deployment and writes none), every
# notice of the world's answer (none with SKILL_SCENARIOS_LAUNCH_NOTICES=none, the header), and
# planGuidanceFile for a world with guidance (forms A, C and D).
# The batch's NATS URL can carry a credential, so jq reads it from LAUNCH_NATS_URL, its environment,
# never from argv, which every user of this machine reads.
launch_request() {
  local notices=${SKILL_SCENARIOS_LAUNCH_NOTICES:-world}
  case $notices in
  world | none) ;;
  *) fail "SKILL_SCENARIOS_LAUNCH_NOTICES is '$notices', not world or none" ;;
  esac
  : >"$R/instructions.md"
  LAUNCH_NATS_URL=$(service_value ENVOY_NATS_URL) jq -n --slurpfile world "$1" --slurpfile answer "$2" --arg daemon "$3" --arg guidance "${4:-}" \
    --arg state "$R/state" --arg workspace "$R/cwd" --arg envoy "$(service_value ENVOY_URL)" \
    --arg envoyToken "$(service_value ENVOY_TOKEN_FILE)" \
    --arg boot "$R/secrets/boot" --arg gh "$R/bin/gh" --arg path "$R/bin:$PATH" --arg instructions "$R/instructions.md" --arg notices "$notices" '
    $world[0] as $w
    | {project: $w.project, stateDir: $state, workspace: $workspace, daemonUrl: $daemon, envoyUrl: $envoy,
       envoyTokenFile: $envoyToken, natsUrls: [$ENV.LAUNCH_NATS_URL], bootTokenFile: $boot, ghToken: "rig-gh-token",
       ghPath: $gh, path: $path, claim: {tree: $w.issues[0].tree, issue: $w.subject, role: $w.role},
       repository: $w.repository, defaultBranch: $w.defaultBranch, instructionsFile: $instructions,
       notices: (if $notices == "none" then [] else $answer[0].notices end)}
    + (if $guidance == "" then {} else {planGuidanceFile: $guidance} end)'
}

# service_value NAME prints the value of NAME in the batch's services file.
service_value() {
  sed -n "s|^$1=||p" "$services_env" | head -n 1
}

# launch_env ANSWER prints the pane's variables of the launch answer ANSWER (C2's LaunchAnswer),
# NAME=value a line, for the run's pane file. A launch whose environment holds a LEGION_STALL_
# variable is refused, naming it: a run waits on the limits of the plugin's defaults
# (pi-shared/src/stall-limits.ts), which a deployment may set and a run never does. So is a name
# that is not a shell variable's (`^[A-Za-z_][A-Za-z0-9_]*$`), naming it: the launch file exports
# each pane line, and bash refuses to export such a name and goes on without it, so the agent would
# run without that variable. So is a value that holds a line break, which a pane file cannot carry.
launch_env() {
  local stall names
  stall=$(jq -r '.env | keys[] | select(startswith("LEGION_STALL_"))' "$1") || fail "the launch answer $1 does not parse"
  [ -z "$stall" ] ||
    fail "the launch's environment holds $(paste -sd' ' <<<"$stall"): a run waits on the plugin's defaults, which a deployment may set and a run never does, so the launch is refused"
  names=$(jq -r '.env | keys[] | select(test("^[A-Za-z_][A-Za-z0-9_]*$") | not) | @json' "$1")
  [ -z "$names" ] ||
    fail "the launch's environment holds $(paste -sd' ' <<<"$names"), which is no shell variable's name: the launch file could not export it, and the agent would run without it, so the launch is refused"
  jq -e '[.env[] | test("\n")] | any | not' "$1" >/dev/null ||
    fail "a variable of the launch answer $1 holds a line break, which a pane file cannot carry"
  jq -r '.env | to_entries[] | "\(.key)=\(.value)"' "$1"
}

# owner_launch DAEMON_URL [GUIDANCE] composes the owner's launch through the label's composer and
# runtime. The request, answer and pane can name the batch's NATS URL, including a credential, so
# each is owner-only: the request through owner_only; the answer under umask 077, because the label
# removes and recreates it; the pane through base_env, whose appends keep its mode.
owner_launch() {
  launch_request "$R/texts/world-request.json" "$R/texts/world-answer.json" "$1" "${2:-}" | owner_only "$R/texts/launch-request.json"
  (umask 077 && label_texts launch "$R/texts/launch-request.json" "$R/texts/launch-answer.json")
  launch_env "$R/texts/launch-answer.json" >>"$R/pane.env"
  {
    echo "DISPATCH_URL=$(sed -n 's|^listening on ||p' "$R/logs/proxy.log")"
    echo "DISPATCH_TOKEN_FILE=$work/services/dispatch-token"
  } >>"$R/pane.env"
}

# pod_standins puts pod-standin.ts first on the run's PATH for each pod tool (POD_TOOLS of
# scored-runs-contracts.ts, which bun prints after its own path): each call is recorded in
# $R/pod-calls.jsonl and refused.
pod_standins() {
  local tool printed=()
  # shellcheck disable=SC2016 # JavaScript for bun, not shell
  mapfile -t printed < <(bun -e '
    const { POD_TOOLS } = await import(process.argv[1]);
    console.log(process.execPath);
    for (const tool of POD_TOOLS) console.log(tool);
  ' "$here/scored-runs-contracts.ts")
  [ ${#printed[@]} -ge 2 ] || fail "bun printed no POD_TOOLS of $here/scored-runs-contracts.ts"
  for tool in "${printed[@]:1}"; do
    printf '#!/bin/sh\nexec %q %q %s "$@"\n' "${printed[0]}" "$here/pod-standin.ts" "$tool" >"$R/bin/$tool"
    chmod +x "$R/bin/$tool"
  done
}

# scored_session runs the owner of owner-walk's world, seeded by seed.ts owner-walk into
# $R/fixture.json, on the label's own texts (the header's "An owner's run").
scored_session() {
  local fixture=$R/fixture.json texts=$R/texts owner tree_root phase port guidance='' file i
  mkdir -p "$texts" "$R/state" "$R/secrets"
  chmod 0700 "$R/secrets"
  (umask 077 && openssl rand -hex 16 >"$R/secrets/boot")
  owner=$(jq -r .owner "$fixture")
  # The world's root issue; never `root`, which names this checkout for every helper below.
  tree_root=$(jq -r '.issues[] | select(.part == "root") | .key' "$fixture")
  # The world, through the label's engine.
  world_request "$fixture" >"$texts/world-request.json"
  label_texts world "$texts/world-request.json" "$texts/world-answer.json"
  phase=$(jq -r --arg key "$owner" '.issues[] | select(.key == $key) | .phase' "$texts/world-request.json")
  jq '[.issues[] | {key, tree, phase, status}]' "$texts/world-request.json" >"$texts/issues.json"
  # The daemon stand-in: the owner's claim, the world's issues on the world's repository and default
  # branch, the label's contract and its register_plan, which reads the run's Dispatch itself, not
  # through the proxy. Its project is the project's token, lower case, as the daemon's
  # claim.ProjectToken makes it from the Dispatch project.
  start_process daemon bun "$here/daemon-standin.ts" 0 "$R/daemon.log" "$R/secrets/boot" "${owner_walk_project,,}" "$owner" architect \
    "$(jq -r .repository "$texts/world-request.json")" "$(jq -r .defaultBranch "$texts/world-request.json")" "$phase" \
    --tree "$tree_root" --issues "$texts/issues.json" --label "$co" --dispatch "http://127.0.0.1:$dispatch_port" "$work/services/dispatch-token"
  await_start daemon "$daemon_pid" 30 "the daemon stand-in to listen" grep -q '^listening on ' "$R/logs/daemon.log"
  port=$(sed -n 's|^listening on http://127\.0\.0\.1:||p' "$R/logs/daemon.log")
  # The agent's Dispatch: the proxy, which answers the plans read with the recorded answer.
  start_process proxy bun "$here/dispatch-proxy.ts" 0 "http://127.0.0.1:$dispatch_port" "$R/dispatch-proxy.jsonl" \
    --plans "$R/plans-answer.json" "$owner_walk_project"
  await_start proxy "$proxy_pid" 30 "the Dispatch proxy to listen" grep -q '^listening on ' "$R/logs/proxy.log"
  # The pane, and the launch through the label's composer and runtime.
  base_env
  standins
  pod_standins
  if [ "$(jq -r '.guidance != null' "$R/world.json")" = true ]; then
    guidance=$R/guidance.md
    bun "$here/owner-walk.ts" guidance "$R/world.json" "$guidance" || fail "owner-walk.ts guidance failed"
  fi
  owner_launch "http://127.0.0.1:$port" "$guidance"
  cp "$(jq -r .systemPromptFile "$texts/launch-answer.json")" "$R/system-prompt.md"
  # The model stand-in, for a label whose model line names it.
  if grep -qxF "model $model_standin" "$R/built-from"; then
    start_process model bun "$here/model-standin.ts" "$R/model-standin.jsonl"
    await_start model "$model_pid" 30 "the model stand-in to listen" grep -q '^listening on ' "$R/logs/model.log"
    route_model_standin "$R/home" "$profile" "$(sed -n 's|^listening on ||p' "$R/logs/model.log")"
  fi
  # The turns: each start task the world queued for the owner, then the form's questions.
  local starts=() count
  count=$(jq --arg key "$owner" '[.starts[] | select(.role == "architect" and .issue == $key)] | length' "$texts/world-answer.json")
  for ((i = 0; i < count; i++)); do
    file=$R/start-$((i + 1)).txt
    jq -j --arg key "$owner" --argjson i "$i" '[.starts[] | select(.role == "architect" and .issue == $key)][$i].task' "$texts/world-answer.json" >"$file"
    starts+=("$file")
  done
  bun "$here/owner-walk.ts" questions "$R/world.json" >"$R/questions.txt" || fail "owner-walk.ts questions failed"
  launch_rpc "${SKILL_SCENARIOS_TIMEOUT:-1500}" "${starts[@]}" -- "$R/questions.txt"
  seed_ts capture-scored "$fixture" "$R"
}

# run_owner_walk NAME CASE: an owner-walk run of the case CASE (<kind>-<form>-<family>). The recorded
# plans answer and its source (SKILL_SCENARIOS_PLANS_ANSWER) go into the run, owner-walk.ts writes
# its world.json (and its guidance's two models, from SKILL_SCENARIOS_GUIDANCE_MODELS, for forms A,
# C and D), the gh stand-in serves E1's pull request, and seed.ts owner-walk files the world on the
# run's own Dispatch, from the label's run-server.sh. Then scored_session.
run_owner_walk() {
  local case=$2 kind form models=() plans=${SKILL_SCENARIOS_PLANS_ANSWER:-}
  require_label_texts "$co" "${P##*/}"
  [ -n "$plans" ] && [ -f "$plans/plans-answer.json" ] && [ -f "$plans/plans-answer.source" ] ||
    fail "SKILL_SCENARIOS_PLANS_ANSWER '$plans' holds no plans-answer.json and plans-answer.source: run \`rig.sh plans-capture <checkout> <dir>\` first"
  cp "$plans/plans-answer.json" "$plans/plans-answer.source" "$R/"
  IFS=- read -r kind form _ <<<"$case"
  case $form in
  A | C | D)
    read -r -a models <<<"${SKILL_SCENARIOS_GUIDANCE_MODELS:-}"
    [ ${#models[@]} = 2 ] ||
      fail "form $form's guidance names a Claude model and a GPT model: set SKILL_SCENARIOS_GUIDANCE_MODELS to \"<claude model> <gpt model>\" (got '${SKILL_SCENARIOS_GUIDANCE_MODELS:-}')"
    ;;
  esac
  bun "$here/owner-walk.ts" world "$case" "$owner_walk_stage" "$R/plans-answer.json" "$R/world.json" "${models[@]}" ||
    fail "owner-walk.ts world $case failed"
  cp "$here/worlds/owner-walk/fixtures.json" "$R/fixtures.json"
  own_dispatch project "$co"
  seed_ts owner-walk "$R/fixture.json" "$kind" "$R/plans-answer.json"
  scored_session
}

# Where a label manifest's patch and text files are named from (controls/patches/README.md).
rig_files=$here

# cmd_label_tree NAME COMMIT MANIFEST makes the label tree NAME (C4 of scored-runs-contracts.ts):
# a fresh detached worktree of COMMIT, a commit or ref of this checkout's repository, at
# <work>/label-trees/NAME/tree, with each line of MANIFEST applied in order (label_tree_apply), as
# the contracts' parseManifest reads it, its patch and text files named from this directory
# (rig_files). A line that does not apply fails the whole tree, naming the line, and leaves no
# tree. Beside the tree it writes `label-tree`, the record (`commit <commit>`, `manifest <sha256 of
# the manifest>`, and `manifest-file <file> <sha256>` for each file a line names), whose manifest
# lines `rig.sh profile` copies into built-from; `manifest`, a copy; and diff.numstat, the numstat
# of the tree against COMMIT. A COMMIT this checkout lacks prints `UNKNOWN: <commit> not in this
# checkout` and exits 3: no tree, and never a pass.
cmd_label_tree() {
  [ $# = 3 ] || fail "usage: rig.sh label-tree <name> <commit> <manifest>"
  local name=$1 ref=$2 manifest commit lines line dir tree files file
  label_dir "$name" >/dev/null
  manifest=$(realpath -e -- "$3") || fail "the manifest $3 does not exist"
  commit=$(git -C "$root" rev-parse --verify -q "$ref^{commit}") || {
    echo "UNKNOWN: $ref not in this checkout" >&2
    exit 3
  }
  dir=$work/label-trees/$name
  [ ! -e "$dir" ] || fail "$dir exists; remove it to make the label tree $name again: \`git -C $root worktree remove --force $dir/tree\`, then \`rm -rf $dir\`; a tree deleted by hand stays registered until \`git -C $root worktree prune\`"
  # shellcheck disable=SC2016 # JavaScript for bun, not shell
  lines=$(bun -e '
    const { parseManifest } = await import(process.argv[1]);
    for (const line of parseManifest(await Bun.file(process.argv[2]).text())) console.log(JSON.stringify(line));
  ' "$here/scored-runs-contracts.ts" "$manifest" 2>&1) || fail "the manifest $manifest does not parse: $lines"
  mkdir -p "$dir"
  tree=$dir/tree
  git -C "$root" worktree add -q --detach "$tree" "$commit" >"$dir/worktree.log" 2>&1 || {
    local why
    why=$(tail -n 3 "$dir/worktree.log" | paste -sd' ')
    rm -rf "$dir"
    fail "git worktree add of $commit failed for the label tree $name: ${why:-git printed nothing}; a tree deleted by hand stays registered until \`git -C $root worktree prune\`"
  }
  while IFS= read -r line; do
    [ -n "$line" ] || continue
    label_tree_apply "$tree" "$line" 2>>"$dir/apply.log" || {
      local why
      why=$(tail -n 1 "$dir/apply.log")
      git -C "$root" worktree remove --force "$tree" >/dev/null 2>&1 || true
      rm -rf "$dir"
      fail "the label tree $name: line $(jq -r .line <<<"$line") of $manifest ($(jq -r .text <<<"$line")) does not apply: $why"
    }
  done <<<"$lines"
  git -C "$tree" add -A
  git -C "$tree" diff --cached --numstat "$commit" >"$dir/diff.numstat"
  cp "$manifest" "$dir/manifest"
  files=$(jq -r '.patchFile, .textFile, .oldFile, .newFile | values' <<<"$lines")
  {
    printf 'commit %s\n' "$commit"
    printf 'manifest %s\n' "$(sha256sum <"$manifest" | cut -d' ' -f1)"
    while IFS= read -r file; do
      [ -z "$file" ] || printf 'manifest-file %s %s\n' "$file" "$(sha256sum <"$rig_files/$file" | cut -d' ' -f1)"
    done <<<"$files"
  } >"$dir/label-tree"
  cat "$dir/label-tree"
}

# label_tree_apply TREE LINE applies one manifest line (a ManifestLine as JSON) to TREE.
label_tree_apply() {
  local tree=$1 kind path commit patch
  kind=$(jq -r .kind <<<"$2")
  path=$(jq -r '.path // empty' <<<"$2")
  commit=$(jq -r '.commit // empty' <<<"$2")
  case $kind in
  file)
    mkdir -p "$(dirname -- "$tree/$path")"
    git -C "$root" show "$commit:$path" >"$tree/$path"
    ;;
  reverse)
    patch=$(dirname -- "$tree")/reverse.patch
    git -C "$root" diff --binary "$commit^" "$commit" -- "$path" >"$patch"
    [ -s "$patch" ] || {
      echo "$commit changes no $path" >&2
      return 1
    }
    git -C "$tree" apply -R "$patch"
    ;;
  patch) git -C "$tree" apply "$rig_files/$(jq -r .patchFile <<<"$2")" ;;
  unpatch) git -C "$tree" apply -R "$rig_files/$(jq -r .patchFile <<<"$2")" ;;
  drop) label_tree_text "$tree/$path" "$rig_files/$(jq -r .textFile <<<"$2")" "" ;;
  replace) label_tree_text "$tree/$path" "$rig_files/$(jq -r .oldFile <<<"$2")" "$rig_files/$(jq -r .newFile <<<"$2")" ;;
  *)
    echo "no verb $kind" >&2
    return 1
    ;;
  esac
}

# label_tree_text FILE OLD NEW takes the exact bytes of the file OLD out of FILE, and puts NEW's
# there when NEW is not empty. OLD's bytes must occur exactly once.
label_tree_text() {
  # shellcheck disable=SC2016 # JavaScript for bun, not shell
  bun -e '
    const { readFileSync, writeFileSync } = await import("node:fs");
    const [file, oldFile, newFile] = process.argv.slice(1);
    const body = readFileSync(file);
    const old = readFileSync(oldFile);
    const replacement = newFile === "" ? Buffer.alloc(0) : readFileSync(newFile);
    if (old.length === 0) { console.error(`${oldFile} is empty`); process.exit(1); }
    let count = 0, first = -1;
    for (let at = body.indexOf(old); at !== -1; at = body.indexOf(old, at + 1)) {
      if (first < 0) first = at;
      count += 1;
    }
    if (count !== 1) { console.error(`the text of ${oldFile} occurs ${count} times in ${file}, not once`); process.exit(1); }
    writeFileSync(file, Buffer.concat([body.subarray(0, first), replacement, body.subarray(first + old.length)]));
  ' "$1" "$2" "$3"
}

# cmd_unpatch_holds NAME PATCH succeeds when the label tree NAME differs from its commit in the
# lines PATCH adds alone (a manifest of one `unpatch` line, as form D's): its diff.numstat is the
# patch's own numstat reversed, and the lines it takes out are the patch's added lines, in order.
# A difference fails, naming both.
cmd_unpatch_holds() {
  [ $# = 2 ] || fail "usage: rig.sh unpatch-holds <name> <patch>"
  local dir=$work/label-trees/$1 patch want got removed added
  patch=$(realpath -e -- "$2") || fail "the patch $2 does not exist"
  [ -f "$dir/label-tree" ] || fail "no label tree $1 under $work/label-trees"
  want=$(git -C "$dir/tree" apply --numstat "$patch" | awk -F'\t' '{ print $2 "\t" $1 "\t" $3 }')
  got=$(<"$dir/diff.numstat")
  [ "$got" = "$want" ] ||
    fail "the label tree $1 differs from its commit in more than the lines of $patch: its numstat reads '${got//$'\t'/ }', and the patch's own, reversed, reads '${want//$'\t'/ }'"
  removed=$(git -C "$dir/tree" diff --cached "$(sed -n 's/^commit //p' "$dir/label-tree")" | awk '/^-/ && !/^---/ { print substr($0, 2) }')
  added=$(awk '/^\+/ && !/^\+\+\+/ { print substr($0, 2) }' "$patch")
  [ "$removed" = "$added" ] ||
    fail "the label tree $1 takes out other lines than $patch adds: it takes out '$removed', and the patch adds '$added'"
  printf 'the label tree %s differs from its commit in the lines of %s alone: %s\n' "$1" "${patch##*/}" "${got//$'\t'/ }"
}

# plans_source ANSWER COMMIT prints the source of the recorded plans answer ANSWER
# (PlansAnswerSource): COMMIT, the capture's label commit, the answer's sha256, and the key map:
# the answer's project to the run's (LWEVAL), and its issues, in the order of their numbers, to
# LWEVAL-1 to LWEVAL-N, the keys a run's Dispatch gives them as seed.ts owner-walk files them, first
# and in that order.
plans_source() {
  jq -c --arg commit "$2" --arg sha256 "$(sha256sum <"$1" | cut -d' ' -f1)" --arg project "$owner_walk_project" '
    ([.issues[].issue] | unique | sort_by(sub("^.*-"; "") | tonumber)) as $keys
    | {commit: $commit, sha256: $sha256,
       keyMap: ({(.project): $project} + ([range(0; $keys | length) | {($keys[.]): "\($project)-\(. + 1)"}] | add // {}))}' "$1"
}

# cmd_plans_capture CHECKOUT OUT runs the capture of a real plans read (scripts/e2e/plan-ratings.sh
# with PLAN_RATINGS_PLANS_ANSWER set, which needs LEGION_TEST_PG_DSN) at
# CHECKOUT, its log in OUT/plans-capture.log, reads the answer's body from the markers in that
# log (decode_plans_answer) and holds it to the file the driver wrote, then writes
# OUT/plans-answer.json and OUT/plans-answer.source (plans_source).
cmd_plans_capture() {
  [ $# = 2 ] || fail "usage: rig.sh plans-capture <checkout> <out dir>"
  local co out status=0 commit
  co=$(cd "$1" && pwd -P) || fail "no checkout $1"
  out=$(realpath -m -- "$2")
  mkdir -p "$out"
  commit=$(git -C "$co" rev-parse HEAD) || fail "cannot read the commit of $co"
  note "the plans capture at $co ($commit)"
  (cd "$co" && PLAN_RATINGS_PLANS_ANSWER="$out/plans-answer.driver.json" bash scripts/e2e/plan-ratings.sh) >"$out/plans-capture.log" 2>&1 || status=$?
  [ "$status" = 0 ] || fail "scripts/e2e/plan-ratings.sh at $co exited $status; see $out/plans-capture.log"
  decode_plans_answer "$out/plans-capture.log" "$out/plans-answer.json"
  cmp -s "$out/plans-answer.json" "$out/plans-answer.driver.json" ||
    fail "the answer decoded from $out/plans-capture.log is not the file the driver wrote, $out/plans-answer.driver.json"
  plans_source "$out/plans-answer.json" "$commit" >"$out/plans-answer.source"
  note "plans-answer.json, $(wc -c <"$out/plans-answer.json") bytes, sha256 $(jq -r .sha256 "$out/plans-answer.source"), key map $(jq -c .keyMap "$out/plans-answer.source")"
}

# decode_plans_answer LOG OUT reads the one block of the plans answer TestPlanRatingsLive prints in
# LOG, each marker a whole line (`plans answer begin`, the body in base64 lines of 76, `plans answer
# end`, `plans answer sha256 <hex>`), and writes the body to OUT once its sha256 is the printed
# one. It refuses a log without the test's PASS line, a marker missing or twice, the markers out of
# order, a body line that is not base64 of that width, and a body of another sha256.
decode_plans_answer() {
  local log=$1 out=$2 marker n begin end sum_at want got partial=$2.partial
  grep -qaE '^--- PASS: TestPlanRatingsLive ' "$log" || fail "$log holds no '--- PASS: TestPlanRatingsLive' line"
  for marker in 'plans answer begin' 'plans answer end'; do
    n=$(grep -caxF -- "$marker" "$log" || true)
    [ "$n" = 1 ] || fail "$log holds $n lines '$marker', not one"
  done
  n=$(grep -caE '^plans answer sha256 [0-9a-f]{64}$' "$log" || true)
  [ "$n" = 1 ] || fail "$log holds $n lines 'plans answer sha256 <64 hex>', not one"
  begin=$(grep -naxF 'plans answer begin' "$log" | cut -d: -f1)
  end=$(grep -naxF 'plans answer end' "$log" | cut -d: -f1)
  sum_at=$(grep -naE '^plans answer sha256 [0-9a-f]{64}$' "$log" | cut -d: -f1)
  ((begin + 1 < end && sum_at == end + 1)) || fail "the markers of $log are out of order (lines $begin, $end, $sum_at)"
  sed -n "$((begin + 1)),$((end - 1))p" "$log" | awk -v last=$((end - begin - 1)) '
    !/^[A-Za-z0-9+\/]+={0,2}$/ || length($0) > 76 || (NR < last && length($0) != 76) { bad = 1 }
    END { exit bad }' || fail "a body line of $log is not base64 of 76 characters"
  sed -n "$((begin + 1)),$((end - 1))p" "$log" | base64 -d >"$partial" || fail "the body of $log does not decode as base64"
  want=$(sed -n "${sum_at}p" "$log" | cut -d' ' -f4)
  got=$(sha256sum <"$partial" | cut -d' ' -f1)
  [ "$got" = "$want" ] || fail "the body decoded from $log has the sha256 $got, and the log printed $want"
  mv "$partial" "$out"
}

# scored_count LABEL CASE prints how many runs of CASE under LABEL hold a scored row.
scored_count() {
  local dir n=0
  for dir in "$work/runs/owner-walk-$2-$1-"*; do
    [[ ${dir##*/} =~ ^owner-walk-$2-$1-[0-9]+$ ]] && [ -f "$dir/row.json" ] || continue
    [ "$(jq -r .outcome "$dir/row.json")" != scored ] || n=$((n + 1))
  done
  printf '%s\n' "$n"
}

# score_scored_run NAME scores the scored scenario's run NAME (`score.ts runs --run`), which
# writes its row.json.
score_scored_run() {
  bun "$here/score.ts" runs "$work/runs" --run "$1" >/dev/null || fail "score.ts runs --run $1 failed"
}

# scored_rig_error NAME LABEL CASE REASON writes the row of a run the rig failed (RigErrorRow),
# with the rig's own reason, beside rig-error.txt, which keeps `rig.sh score` from scoring it again.
scored_rig_error() {
  local dir=$work/runs/$1 reason=${4#skill-scenarios: }
  [ -n "$reason" ] || reason="rig.sh run failed and named no reason"
  mkdir -p "$dir"
  printf '%s\n' "$reason" >"$dir/rig-error.txt"
  jq -cn --arg run "$1" --arg label "$2" --arg case "$3" --arg error "the rig failed the run: $reason" \
    '{outcome: "rig-error", run: $run, scenario: "owner-walk", label: $label, case: $case, error: $error}' >"$dir/row.json"
}

# scored_attempt SCENARIO LABEL CASE N launches one run, `rig.sh run`, and writes its row: the
# scenario's score (score_scored_run), or the rig's error with the reason the run's stderr named.
# Then it stores the run, scored or not (store_scored_run).
scored_attempt() {
  local name=$1-$3-$2-$4 err status=0
  mkdir -p "$work/scored-logs"
  err=$work/scored-logs/$name.err
  scored_index=$((scored_index + 1))
  SKILL_SCENARIOS_OWNER_WALK_CASE=$3 bash "$here/rig.sh" run "$1" "$2" "$4" "$scored_index" "$start" 2>"$err" || status=$?
  cat "$err" >&2
  if [ "$status" = 0 ]; then
    score_scored_run "$name"
  else
    scored_rig_error "$name" "$2" "$3" "$(grep '^skill-scenarios: ' "$err" | tail -n 1)"
  fi
  store_scored_run "$name"
}
scored_index=0

# batch_scored_cell SCENARIO LABEL CASE COUNT launches runs of CASE under LABEL (scored_attempt)
# until COUNT of them hold a scored row, and stops the cell at three times COUNT launches, where
# it prints UNKNOWN: a cell short of its count is never a pass. The runs of the cell already in
# $work/runs, each with its row, count as launches too: a restarted Job counts the runs rig.sh
# restore-stored put back. A run its store does not hold, one lost to an eviction mid-run or whose
# pack or put failed, is not counted, so a Job that restarts can launch more than three times its
# count in all. The count runs without RT_STORE too: in a work directory that already holds runs
# of the cell, those runs are among its launches.
batch_scored_cell() {
  local scenario=$1 label=$2 case=$3 count=$4 launches=0 dir
  for dir in "$work/runs/$scenario-$case-$label-"*; do
    [[ ${dir##*/} =~ ^$scenario-$case-$label-[0-9]+$ ]] && [ -f "$dir/row.json" ] || continue
    launches=$((launches + 1))
  done
  while [ "$(scored_count "$label" "$case")" -lt "$count" ]; do
    if [ "$launches" -ge $((3 * count)) ]; then
      echo "UNKNOWN: $scenario $label $case: $(scored_count "$label" "$case") scored runs of $count after $launches launches, three times its count"
      return 0
    fi
    launches=$((launches + 1))
    scored_attempt "$scenario" "$label" "$case" "$(next_run "$scenario-$case" "$label")"
  done
}

# The store of a scored run. RT_STORE is the runner's helper (legion-remote-test.sh --store-key):
# `put <file> <name>`, `list <prefix>` and `get <name> <file>`, each under the Job's own key, each
# put and get printing `stored|got <name> sha256 <hex>` in the Job's log. The bucket's name stays
# in the runner. One key is one Job's. A run is two objects: its pack, runs/<run>.tar.gz, and then
# its row, runs/<run>.row.json, which is the run's row.json with one more field, `pack`, the pack's
# sha256. The row goes last, so a stored row says its pack is stored too, and names which pack. A
# pod stopped between the two leaves a pack no restart fetches, and its run's name is run, and its
# pack stored, again.
#
# A pack holds the run's regular files and directories alone, a hard link as its file. It leaves
# out every other entry (a symlink, a FIFO, a socket), and names them. It leaves out, by name, the
# places the run's credentials can be, without reading what is in them: every directory named
# secrets (the daemon stand-in's boot token, the owner's Envoy token and gh configuration), every
# directory named home (the label's HOME, copied, and the owner's state/home), pane.env and the
# launch's texts/launch-request.json and texts/launch-answer.json, which can carry the batch's NATS
# URL with its credential. The scorer reads none of them.

# store_scored_run NAME stores the run NAME, when RT_STORE is set. A failed pack or put is noted and
# stops neither the batch nor the run's count in this pod. After a failed pack or a failed put of
# it, its row is not stored, so no restart counts the run; at download (cmd_check_stored) a run
# whose object the log names and the download lacks is a rig error.
store_scored_run() {
  [ -n "${RT_STORE:-}" ] || return 0
  local name=$1 pack=$work/stored/$1.tar.gz row=$work/stored/$1.row.json list=$work/stored/$1.list
  local hex left
  mkdir -p "$work/stored"
  rm -f -- "$pack" "$row" "$list"
  (cd "$work/runs" && find "$name" \( -type d \( -name home -o -name secrets \) -o -path "$name/pane.env" \
    -o -path "$name/texts/launch-request.json" -o -path "$name/texts/launch-answer.json" \) -prune \
    -o \( -type d -o -type f \) -print0) >"$list"
  left=$(cd "$work/runs" && find "$name" \( -type d \( -name home -o -name secrets \) -o -path "$name/pane.env" \
    -o -path "$name/texts/launch-request.json" -o -path "$name/texts/launch-answer.json" \) -prune \
    -o ! -type d ! -type f -printf '%P\n' | LC_ALL=C sort)
  [ -z "$left" ] ||
    note "the pack of $name leaves out $(grep -c . <<<"$left") entries that are no regular file or directory: $(paste -sd' ' <<<"$left")"
  if ! tar -czf "$pack" -C "$work/runs" --no-recursion --hard-dereference --null -T "$list"; then
    note "the pack of $name failed, so the run is not stored"
    rm -f -- "$pack" "$list"
    return 0
  fi
  rm -f -- "$list"
  hex=$(sha256sum <"$pack")
  hex=${hex%% *}
  if ! "$RT_STORE" put "$pack" "runs/$name.tar.gz" </dev/null; then
    note "the store of runs/$name.tar.gz failed, so its row is not stored"
    rm -f -- "$pack"
    return 0
  fi
  rm -f -- "$pack"
  jq -c --arg pack "$hex" '. + {pack: $pack}' "$work/runs/$name/row.json" >"$row"
  "$RT_STORE" put "$row" "runs/$name.row.json" </dev/null || note "the store of runs/$name.row.json failed"
  rm -f -- "$row"
}

# pack_problem PACK RUN prints why the pack of the run RUN may not be unpacked, or nothing. Each of
# its members must be a regular file or a directory, by its type in tar's own listing, and RUN or
# under RUN/. Its name says nothing of a member's type: a symlink, or a hard link, lets a later
# member, or a later write, land in another run or outside the directory it is unpacked in.
pack_problem() {
  local listing names i what
  local -a types members
  if ! listing=$(tar -tvzf "$1" 2>/dev/null) || ! names=$(tar -tzf "$1" 2>/dev/null); then
    echo "is no pack: tar cannot list it"
    return
  fi
  mapfile -t types < <(cut -c1 <<<"$listing")
  mapfile -t members <<<"$names"
  if [ ${#types[@]} != ${#members[@]} ]; then
    echo "is listed as ${#types[@]} members with their types and ${#members[@]} by name"
    return
  fi
  for i in "${!members[@]}"; do
    case ${types[$i]} in
    - | d) what='' ;;
    l) what="a symlink" ;;
    h) what="a hard link" ;;
    *) what="a member of type '${types[$i]}'" ;;
    esac
    if [ -z "$what" ] && [[ ${members[$i]} != "$2" && ${members[$i]} != "$2/"* ]]; then
      what="which is not under $2/"
    fi
    if [ -n "$what" ]; then
      echo "holds '${members[$i]}', $what: a pack holds its run's regular files and directories alone"
      return
    fi
  done
}

# unpack_fresh PACK RUN PARENT unpacks PACK, which pack_problem passed, into a new directory under
# PARENT, and prints that directory, which holds RUN alone.
unpack_fresh() {
  local fresh
  fresh=$(mktemp -d "$3/.unpack.XXXXXX") || fail "cannot make a directory under $3 to unpack $1 in"
  tar -xzf "$1" -C "$fresh" --no-same-owner || fail "the unpack of $1 failed"
  [ -d "$fresh/$2" ] && [ ! -L "$fresh/$2" ] || fail "$1 holds no directory $2"
  printf '%s\n' "$fresh"
}

# same_row STORED ROW is true when the stored row STORED, its `pack` taken out, is the run's ROW.
same_row() {
  [ "$(jq -S -c 'del(.pack)' "$1" 2>/dev/null)" = "$(jq -S -c . "$2" 2>/dev/null)" ]
}

# cmd_restore_stored SCENARIO (the header): each run of SCENARIO whose row an earlier pod of the
# same Job stored, put back into $work/runs. Its row is fetched first, then the pack it names; the
# pack must be of the row's sha256, pass pack_problem, unpack into a fresh directory, and hold the
# row's own row.json. Each get prints its object's sha256, so the Job's log names every object a
# cell counts. A run already in $work/runs is left. A failed list or get, or a pack refused, ends
# the restore and the Job: a pod that cannot read its store would run again, and store over, the
# names it already holds.
cmd_restore_stored() {
  [ $# = 1 ] || fail "usage: rig.sh restore-stored <scenario>"
  [ -n "${RT_STORE:-}" ] || fail "restore-stored reads the Job's store, and RT_STORE is unset: legion-remote-test.sh --store-key sets it"
  local names name run listed=0 restored=0 here_dir problem want hex fresh
  names=$("$RT_STORE" list runs/ </dev/null) || fail "the store's list of runs/ failed"
  mkdir -p "$work/runs" "$work/stored"
  while IFS= read -r name; do
    [ -n "$name" ] || continue
    listed=$((listed + 1))
    [[ $name =~ ^runs/($1-[A-Za-z0-9_-]+)\.row\.json$ ]] || continue
    run=${BASH_REMATCH[1]}
    [ ! -e "$work/runs/$run" ] || continue
    here_dir=$work/stored/$run
    rm -rf -- "$here_dir"
    mkdir -p "$here_dir"
    "$RT_STORE" get "runs/$run.row.json" "$here_dir/row.json" </dev/null || fail "the get of runs/$run.row.json failed"
    want=$(jq -r '.pack // empty' "$here_dir/row.json" 2>/dev/null)
    [[ $want =~ ^[0-9a-f]{64}$ ]] || fail "runs/$run.row.json names no pack sha256"
    "$RT_STORE" get "runs/$run.tar.gz" "$here_dir/pack.tar.gz" </dev/null || fail "the get of runs/$run.tar.gz failed"
    hex=$(sha256sum <"$here_dir/pack.tar.gz")
    hex=${hex%% *}
    [ "$hex" = "$want" ] || fail "runs/$run.tar.gz is not the pack its row names: the row names $want, the store holds $hex"
    problem=$(pack_problem "$here_dir/pack.tar.gz" "$run")
    [ -z "$problem" ] || fail "runs/$run.tar.gz $problem"
    fresh=$(unpack_fresh "$here_dir/pack.tar.gz" "$run" "$here_dir")
    same_row "$here_dir/row.json" "$fresh/$run/row.json" || fail "runs/$run.tar.gz's row.json is not the row stored beside it"
    mv -- "$fresh/$run" "$work/runs/$run"
    rm -rf -- "$here_dir"
    restored=$((restored + 1))
  done <<<"$names"
  note "the store lists $listed names under runs/; $restored runs of $1 restored from it"
}

# cmd_check_stored DIR LOG... (the header). DIR is a download of a Job's store, DIR/runs/<run>.tar.gz
# and DIR/runs/<run>.row.json, and the LOGs every log of the Job, in the order its pods ran
# (legion-remote-test.sh keeps every attempt's in <out>.attempts, in that order). Their `stored`
# and `got` lines name each object's sha256. A `stored` line sets it, and these are faults:
#   - a `got` line of another sha256 than the `stored` line before it: the object changed between
#     the pods;
#   - a `got` line of a name no LOG stores: no pod is known to have stored that object, so a run
#     put into the store by anything but a pod, and every run of a pod whose LOG is not given, is
#     refused, never counted;
#   - a `stored` line, of another sha256, for an object of a run whose row a LOG stored before: a
#     stored run is never run, or stored, again.
# A pack stored twice before its run's row is no fault: a pod stopped between the two puts, and
# the later pod ran and stored the run again. Each owner-walk run whose row the download holds, or
# whose object a LOG names, is checked: its row must be in the download, stored by a LOG, and of
# that sha256; its pack must be in the download, of the sha256 its row names (the row, checked
# against the LOGs, vouches for it), pass pack_problem, and hold the row's own row.json. A run that passes is unpacked
# into DIR/checked, through a fresh directory. One that fails gets a rig error row there, naming
# each fault, and its pack is not unpacked; with no world.json, it is in no cell. Then a line for
# each run, and the cells (score.ts cells). It exits 0 only when every run is as stored and no
# file of the download was left out; any fault, or UNKNOWN, exits 1.
#
# What it cannot see: an object an agent in the same pod changed before the rig stored it; and a
# run that only a LOG not given names, whose objects are gone from the store, since nothing else
# names it. check-stored cannot know that a log is missing: give it every attempt's.
cmd_check_stored() {
  [ $# -ge 2 ] || fail "usage: rig.sh check-stored <dir> <job log>..."
  local dir=$1 log line file name run object hex faults joined kind form family kase label rest want problem fresh
  local left=0 faulted=0 status
  local -A logged=() named=() conflict=() rowstored=()
  shift
  faults=()
  for log in "$@"; do
    [ -f "$log" ] || fail "the Job's log $log is not a file"
    while IFS= read -r line; do
      [[ $line =~ ^(stored|got)\ (runs/(owner-walk-[A-Za-z0-9_-]+)\.(tar\.gz|row\.json))\ sha256\ ([0-9a-f]{64})$ ]] || continue
      object=${BASH_REMATCH[2]} run=${BASH_REMATCH[3]} hex=${BASH_REMATCH[5]}
      named[$run]=1
      if [ "${BASH_REMATCH[1]}" = stored ]; then
        if [ -n "${rowstored[$run]:-}" ] && [ "$hex" != "${logged[$object]:-}" ]; then
          conflict[$object]="$object was stored again after its run's row was stored: ${logged[$object]:+first with sha256 ${logged[$object]}, then }with sha256 $hex"
          continue
        fi
        logged[$object]=$hex
        [ "${BASH_REMATCH[4]}" != row.json ] || rowstored[$run]=1
      elif [ -z "${logged[$object]:-}" ]; then
        conflict[$object]="$object was got with sha256 $hex, and no log given stores it: give every pod's log"
      elif [ "$hex" != "${logged[$object]}" ]; then
        conflict[$object]="$object was stored with sha256 ${logged[$object]} and later got with sha256 $hex"
      fi
    done <"$log"
  done
  # A download that holds nothing was not checked, whatever the logs name: each run would read as
  # its objects missing, and the count of that is no measurement of the store.
  if ! compgen -G "$dir/runs/*" >/dev/null; then
    echo "UNKNOWN: $dir/runs holds no object: nothing was checked"
    return 1
  fi
  for file in "$dir"/runs/*.row.json; do
    [ -f "$file" ] || continue
    name=${file##*/}
    if [[ $name =~ ^(owner-walk-[A-Za-z0-9_-]+)\.row\.json$ ]]; then
      named[${BASH_REMATCH[1]}]=1
    else
      printf 'UNKNOWN: %q in %s/runs is no run'"'"'s object: it is left out\n' "$name" "$dir"
      left=$((left + 1))
    fi
  done
  if [ ${#named[@]} = 0 ]; then
    echo "UNKNOWN: neither the Job's logs nor $dir/runs name an owner-walk run: nothing was checked"
    return 1
  fi
  rm -rf -- "$dir/checked"
  mkdir -p "$dir/checked"
  while IFS= read -r run; do
    faults=()
    object=runs/$run.row.json file=$dir/$object want=''
    if [ ! -f "$file" ]; then
      faults+=("$object is not in the download")
    elif [ -n "${conflict[$object]:-}" ]; then
      faults+=("${conflict[$object]}")
    elif [ -z "${logged[$object]:-}" ]; then
      faults+=("$object has no stored line in the Job's logs")
    else
      hex=$(sha256sum <"$file")
      hex=${hex%% *}
      if [ "$hex" != "${logged[$object]}" ]; then
        faults+=("$object differs from the Job's log: logged ${logged[$object]}, downloaded $hex")
      else
        want=$(jq -r '.pack // empty' "$file" 2>/dev/null)
        [[ $want =~ ^[0-9a-f]{64}$ ]] || {
          faults+=("$object names no pack sha256")
          want=''
        }
      fi
    fi
    object=runs/$run.tar.gz file=$dir/$object
    if [ ! -f "$file" ]; then
      faults+=("$object is not in the download")
    else
      hex=$(sha256sum <"$file")
      hex=${hex%% *}
      [ -z "${conflict[$object]:-}" ] || faults+=("${conflict[$object]}")
      [ -z "$want" ] || [ "$hex" = "$want" ] ||
        faults+=("$object is not the pack its row names: the row names $want, the download holds $hex")
    fi
    if [ ${#faults[@]} = 0 ]; then
      problem=$(pack_problem "$file" "$run")
      [ -z "$problem" ] || faults+=("$object $problem")
    fi
    if [ ${#faults[@]} = 0 ]; then
      fresh=$(unpack_fresh "$file" "$run" "$dir/checked")
      if same_row "$dir/runs/$run.row.json" "$fresh/$run/row.json"; then
        mv -- "$fresh/$run" "$dir/checked/$run"
        rm -rf -- "$fresh"
        printf 'checked %s: as stored\n' "$run"
        continue
      fi
      rm -rf -- "$fresh"
      faults+=("$object's row.json is not the row stored beside it")
    fi
    # The run's name gives its case and label: owner-walk-<kind>-<form>-<family>-<label>-<n>.
    rest=${run#owner-walk-}
    IFS=- read -r kind form family label <<<"$rest"
    kase=$kind-$form-$family label=${label%-*}
    printf -v joined '%s; ' "${faults[@]}"
    joined=${joined%; }
    mkdir -p "$dir/checked/$run"
    jq -cn --arg run "$run" --arg label "$label" --arg case "$kase" --arg error "the store's objects of the run are at fault: $joined" \
      '{outcome: "rig-error", run: $run, scenario: "owner-walk", label: $label, case: $case, error: $error}' >"$dir/checked/$run/row.json"
    printf 'checked %s: rig error: %s\n' "$run" "$joined"
    faulted=$((faulted + 1))
  done < <(printf '%s\n' "${!named[@]}" | LC_ALL=C sort)
  bun "$here/score.ts" cells "$dir/checked" owner-walk
  status=$?
  [ "$status" = 0 ] || return "$status"
  if [ "$faulted" != 0 ] || [ "$left" != 0 ]; then
    echo "check-stored: $faulted of ${#named[@]} runs are rig errors of the store, and $left files of the download were left out"
    return 1
  fi
}

# cmd_batch_scored SCENARIO COUNT LABEL... (the header): the batch's services, each label's texts
# built once (`label-texts.sh build`), and one cell of COUNT scored runs for each label, of the
# case SKILL_SCENARIOS_OWNER_WALK_CASE names, one run at a time.
cmd_batch_scored() {
  [ $# -ge 3 ] || fail "usage: rig.sh batch-scored owner-walk <count> <label>..."
  local scenario=$1 count=$2 label case
  [ "$scenario" = owner-walk ] || fail "batch-scored runs owner-walk, the one scored scenario, not '$scenario'"
  [[ $count =~ ^[1-9][0-9]*$ ]] || fail "the count '$count' is not a whole number above 0"
  case=$(owner_walk_case) || exit 1
  shift 2
  for label in "$@"; do
    load_label "$label"
    require_label_texts "$co" "$label"
  done
  lock_services
  batch_scenario=$scenario
  on_exit stop_batch
  services_up 1
  for label in "$@"; do
    load_label "$label"
    label_texts build
  done
  start=$(date +%s)
  for label in "$@"; do batch_scored_cell "$scenario" "$label" "$case" "$count"; done
}

# cmd_rows [SCENARIO] prints each scored run's row (`row <row.json>`), then a line for each entry of
# each run's reach, `reach <run>: <tool> names <host> (session <s>, message <i>)`. The reach is a
# measurement and no guard: only a plain-text mention of one of the three hosts in a tool's
# arguments reaches it, and a host reached through a variable, an encoding or a file is not listed.
cmd_rows() {
  local dir rows=()
  for dir in "$work/runs/${1:-owner-walk}"-*; do
    [ ! -f "$dir/row.json" ] || rows+=("$dir/row.json")
  done
  if [ ${#rows[@]} = 0 ]; then
    note "no run of ${1:-owner-walk} under $work/runs holds a row.json"
    return 0
  fi
  for dir in "${rows[@]}"; do printf 'row %s\n' "$(jq -c . "$dir")"; done
  jq -r '.run as $run | .reach[]? | "reach \($run): \(.tool) names \(.host) (session \(.session), message \(.message))"' "${rows[@]}"
}

# cmd_cell SCENARIO LABEL CASE COUNT prints the line of the cell of CASE under LABEL (score.ts
# cell, scored-runs.ts caseCell), in the stage every run's world is of: for a case form, its runs,
# its scored runs (the first COUNT by number) and rig errors, for each clause how many scored runs
# passed it, and the verdict of a case's cell (pass when each of its COUNT runs passes); for a
# control form (D, E), the control's own cell over every owner of the form under LABEL, at the
# control's count, with the verdict cells gives it (pass when fewer than its bound pass its clause).
cmd_cell() {
  [ $# = 4 ] || fail "usage: rig.sh cell <scenario> <label> <case> <count>"
  bun "$here/score.ts" cell "$work/runs" "$1" "$owner_walk_stage" "$2" "$3" "$4"
}

# cmd_evidence [SCENARIO] prints the evidence of a scenario's runs as Markdown: the rig's commit,
# each label's built-from, the cells (`score.ts cells`), and each run's model and row.
# shellcheck disable=SC2016 # the backticks are Markdown's fences, not substitutions
cmd_evidence() {
  local scenario=${1:-owner-walk} dir
  printf '# Scored runs of %s\n\nThe rig: %s\n\n## Labels\n' "$scenario" "$(checkout_commit "$root")"
  for dir in "$work/profiles"/*/; do
    [ -f "$dir/built-from" ] || continue
    printf '\n### %s\n\n```text\n%s\n```\n' "$(basename -- "$dir")" "$(<"$dir/built-from")"
  done
  printf '\n## Cells\n\n```json\n%s\n```\n\n## Runs\n' "$(bun "$here/score.ts" cells "$work/runs" "$scenario" 2>&1)"
  for dir in "$work/runs/$scenario"-*/; do
    [ -f "$dir/row.json" ] || continue
    printf '\n### %s\n\nModel: %s\n\n```json\n%s\n```\n' "$(basename -- "$dir")" \
      "$(paste -sd' ' "$dir/model.txt" 2>/dev/null || echo none)" "$(jq . "$dir/row.json")"
  done
}

# cmd_score [SCENARIO] scores the runs of the scenario (`score.ts runs`), or of every scenario.
# A scored scenario's run is scored one by one (score_scored_run), but a run the rig failed, whose
# row is its rig error (rig-error.txt), and then their rows and reach lines (cmd_rows) and cells.
cmd_score() {
  local scenario=${1:-} dir names=()
  if [ "$scenario" != owner-walk ]; then
    bun "$here/score.ts" runs "$work/runs" "$@" || return
    [ -z "$scenario" ] || return 0
  fi
  for dir in "$work/runs"/owner-walk-*/; do
    [ -d "$dir" ] || continue
    names+=("$(basename -- "$dir")")
  done
  if [ ${#names[@]} = 0 ]; then
    [ -z "$scenario" ] || note "no owner-walk run under $work/runs"
    return 0
  fi
  for dir in "${names[@]}"; do
    [ -f "$work/runs/$dir/rig-error.txt" ] || score_scored_run "$dir"
  done
  cmd_rows owner-walk
  bun "$here/score.ts" cells "$work/runs" owner-walk
}

cmd_run() {
  [ $# = 5 ] || fail "usage: rig.sh run <ask-on-message|measure-before-ask|tester-proof|brainstorm|askgate|spec-writing|spec-rewrite|owner-walk> <label> <n> <index> <start>"
  local scenario=$1 n=$3 index=$4 start=$5 name owner_case=''
  load_label "$2"
  [[ $n =~ ^[0-9]+$ ]] || fail "run number '$n' is not a number"
  [[ $index =~ ^[0-9]+$ && $start =~ ^[0-9]+$ ]] || fail "run index '$index' or batch start '$start' is not a number"
  [ -f "$services_env" ] || fail "a run needs its batch's services: run \`rig.sh batch $scenario 1 $2\` (\`rig.sh batch-scored $scenario 1 $2\` for a scored scenario)"
  # An askgate run's name carries its case, so the batches of the cases, each numbered from 1,
  # never remove each other's runs.
  if [ "$scenario" = askgate ]; then
    case ${SKILL_SCENARIOS_ASKGATE_CASE:-} in
    facts | link | upload | nochange | nochange-rebuttal | nochange-block | untried | tried)
      name=$scenario-$SKILL_SCENARIOS_ASKGATE_CASE-${P##*/}-$n
      ;;
    *) fail "SKILL_SCENARIOS_ASKGATE_CASE is '${SKILL_SCENARIOS_ASKGATE_CASE:-}', not facts, link, upload, nochange, nochange-rebuttal, nochange-block, untried or tried" ;;
    esac
  elif [ "$scenario" = owner-walk ]; then
    # A scored run's name carries its case (score.ts runs --run reads it).
    owner_case=$(owner_walk_case) || exit 1
    name=$scenario-$owner_case-${P##*/}-$n
  else
    name=$scenario-${P##*/}-$n
  fi
  R=$work/runs/$name
  evidence=$R
  on_exit stop_run
  rm -rf "$R"
  mkdir -p "$R/bin" "$R/sessions" "$R/cwd" "$R/logs"
  cp "$P/built-from" "$R/built-from"
  case $scenario in
  ask-on-message) run_ask_on_message "$name" ;;
  measure-before-ask) run_measure_before_ask "$name" ;;
  tester-proof) run_tester_proof "$name" "$n" "$index" "$start" ;;
  brainstorm) run_brainstorm "$name" ;;
  askgate) run_askgate "$name" ;;
  spec-writing | spec-rewrite) run_spec "$scenario" "$name" ;;
  owner-walk) run_owner_walk "$name" "$owner_case" ;;
  *) fail "unknown scenario $scenario" ;;
  esac
  echo "$name: $(tail -n 1 "$R/out.txt")"
}

# A batch's exit: its runs (each run's shell, daemon stand-in and fixture commands), the agents they
# started, every process that carries a run's SKILL_SCENARIO_RUN, and the services.
# shellcheck disable=SC2329 # on_exit's trap runs it
stop_batch() {
  local s sessions=()
  stop_tree "${runs_pid:-}"
  for s in $(tmux list-sessions -F '#{session_name}' 2>/dev/null || true); do
    case $s in "$tag-$batch_scenario"-*) sessions+=("$s") ;; esac
  done
  for s in "${sessions[@]}"; do stop_panes "$s"; done
  stop_env_processes "$work/runs"
  services_down
}

cmd_batch() {
  [ $# -ge 3 ] || fail "usage: rig.sh batch <ask-on-message|measure-before-ask|tester-proof|brainstorm|askgate|spec-writing|spec-rewrite> <runs> <label>..."
  local runs=$2 label n index=0 start parallel=${SKILL_SCENARIOS_PARALLEL:-5}
  batch_scenario=$1
  case $batch_scenario in
  ask-on-message | measure-before-ask | tester-proof | brainstorm | askgate | spec-writing | spec-rewrite) ;;
  *) fail "rig.sh batch runs ask-on-message, measure-before-ask, tester-proof, brainstorm, askgate, spec-writing or spec-rewrite, not $batch_scenario; a scored scenario runs with \`rig.sh batch-scored owner-walk <count> <label>...\`" ;;
  esac
  [ "$batch_scenario" != askgate ] || case ${SKILL_SCENARIOS_ASKGATE_CASE:-} in
    facts | link | upload | nochange | nochange-rebuttal | nochange-block | untried | tried) ;;
    *) fail "an askgate batch needs SKILL_SCENARIOS_ASKGATE_CASE set to facts, link, upload, nochange, nochange-rebuttal, nochange-block, untried or tried (got '${SKILL_SCENARIOS_ASKGATE_CASE:-}')" ;;
  esac
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
  for label in "$@"; do load_label "$label"; done
  # A spec run reads its source and the label's superpowers skills: refused here, before any
  # service starts. Its number is one above the label's highest, so no batch removes an earlier run.
  local first=1
  case $batch_scenario in spec-writing | spec-rewrite)
    source_dir "$batch_scenario" "${SKILL_SCENARIOS_SOURCE:-}" >/dev/null
    require_superpowers "$batch_scenario"
    first=$(next_run "$batch_scenario" "$1")
    ;;
  esac
  lock_services
  on_exit stop_batch
  services_up "$parallel"
  start=$(date +%s)
  for label in "$@"; do
    for n in $(seq "$first" $((first + runs - 1))); do
      index=$((index + 1))
      printf '%s %s %s %s %s\n' "$batch_scenario" "$label" "$n" "$index" "$start"
    done
  done | xargs -r -P "$parallel" -L 1 bash "$here/rig.sh" run &
  runs_pid=$!
  wait "$runs_pid"
}

# Bash reads a script as it runs it, so the dispatch and its exit are one command line: a batch
# outlives any edit to this file, and resuming at its old offset would run whatever lies there now.
case "${1:-}" in
profile) shift && cmd_profile "$@" ;;
live-read) shift && cmd_live_read "$@" ;;
batch) shift && cmd_batch "$@" ;;
batch-scored) shift && cmd_batch_scored "$@" ;;
restore-stored) shift && cmd_restore_stored "$@" ;;
check-stored) shift && cmd_check_stored "$@" ;;
run) shift && cmd_run "$@" ;;
score) shift && cmd_score "$@" ;;
rows) shift && cmd_rows "$@" ;;
cell) shift && cmd_cell "$@" ;;
evidence) shift && cmd_evidence "$@" ;;
label-tree) shift && cmd_label_tree "$@" ;;
unpatch-holds) shift && cmd_unpatch_holds "$@" ;;
plans-capture) shift && cmd_plans_capture "$@" ;;
judge) shift && cmd_judge "$@" ;;
judge-omp) shift && cmd_judge_omp "$@" ;;
*) sed -n '2,/^set -euo/p' "${BASH_SOURCE[0]}" | sed '$d' | sed 's/^# \{0,1\}//' && exit 2 ;;
esac; exit
