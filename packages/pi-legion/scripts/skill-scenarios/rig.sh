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
#   rig.sh batch     <scenario> <runs> <label>...  starts the scratch services, runs <runs> per
#                                                  label (SKILL_SCENARIOS_PARALLEL at once), each
#                                                  label's numbered from one above its highest
#                                                  run, live or archived, and stops the services
#   rig.sh score     [--repo <git dir> | --arm <scenario>[:<case>]:<label>=<n>]... [<scenario>]
#                                                  one row per run, then counts per label: the runs
#                                                  of <work>/runs and <work>/archive/runs, each
#                                                  read from its own record alone; a run whose
#                                                  label's daemon sources the --repo git
#                                                  directories cannot rebuild stops the score,
#                                                  named, exit 2, and so do no run at all (or none
#                                                  of <scenario>), an entry of a runs directory
#                                                  that is neither a run nor its <id>.json record,
#                                                  and an arm (score's key, `tester-proof:head`,
#                                                  `askgate:facts:on`) that no --arm names, or
#                                                  that holds other than the <n> runs, scored or
#                                                  rig errors, its --arm names
#   rig.sh health    [--tmp-snapshot <file>] [--repo <git dir>]... <run dir>...
#                                                  (each --repo a git directory the daemon sources
#                                                  of a run's label are rebuilt from, by the commit
#                                                  its built-from records)
#                                                  whether each run's world could take what its
#                                                  agents did and the run stayed in its own world,
#                                                  scoring nothing (health.ts): the run's boot
#                                                  record (<run>/booted) boots or skips each role
#                                                  its scenario boots, in order, skipping one only
#                                                  for the reason completion-answers.ts's SKIPS
#                                                  declares, and each role it booted finished on
#                                                  its own output and transcript; every service of
#                                                  its label's own, its Dispatch server among
#                                                  them, came from its label's checkout
#                                                  (<run>/services-from against its built-from,
#                                                  services_from), and the runs given of one
#                                                  batch record one system git configuration
#                                                  there (its `system-gitconfig` line); no
#                                                  agent's claim boot was refused but a first
#                                                  attempt's before its first turn, which the rig
#                                                  relaunched (boot_agent);
#                                                  every completion names its role and none was
#                                                  refused in a way its label's daemon would not
#                                                  refuse it, and its world took the completions
#                                                  it must take (completion-answers.ts's ANSWERS);
#                                                  the daemon stand-in answered no route 404 that
#                                                  the run's label's daemon registers, and no turn
#                                                  outcome it answered failed the claim; no agent
#                                                  met a push-guard refusal or a gh refusal its
#                                                  scenario does not declare, nor an error answer
#                                                  of the run's own Dispatch server
#                                                  (<run>/logs/dispatch.log) its scenario does not
#                                                  declare for its role; no tool call named
#                                                  another run's directory; and none read a path
#                                                  another of the runs given wrote, or /tmp scratch
#                                                  from before the run (<file>: the /tmp entries
#                                                  before the batch, for runs with no record of
#                                                  their own); no tool call named a path the world
#                                                  its namespace.txt records hides and does not
#                                                  show, another process's /proc environ or
#                                                  cmdline, or a credential under /var/run/legion or
#                                                  /var/run/secrets; and no call reached a host off
#                                                  its world, each command read by its own grammar
#                                                  once its wrappers are read off. It prints how
#                                                  many relaunches each label's runs took. Give a
#                                                  batch's runs in one call; exits 0 when every
#                                                  run is healthy, 1 when one is unhealthy, and 2
#                                                  on a usage error or when a run gets no verdict:
#                                                  one a rule cannot judge for an input it did not
#                                                  record (a namespace.txt, a built-from commit
#                                                  the --repo directories hold) and no other rule
#                                                  fails, named on stderr
#   rig.sh world     <label> <run dir>             the run's world, one `<kind> <path>` a line
#                                                  (world_paths), worked out of the pod as it
#                                                  stands: what its namespace hides and what it
#                                                  shows again; no reader judges a run by it
#   rig.sh judge     <run dir>...                  the judge's prompts on each spec-writing or
#                                                  spec-rewrite run, and on what the agents of
#                                                  each reviewer-text-findings or threads-closed
#                                                  run wrote (judge.ts run), each in a fresh
#                                                  Oh My Pi session with no Dispatch; a worker
#                                                  run's own end runs it too
#   rig.sh identity  <label>                       the label's three lines of the rig's identity
#                                                  (cmd_identity): its built profile's tree
#                                                  digest, its pack digest and the rig's revision
#   rig.sh judge-omp <ste|bare> <dir> <output>     one fresh judge session: `omp -p` of
#                                                  <dir>/prompt.txt in <dir>, under the judge's
#                                                  profile (ste: asd-ste100 its one skill; bare: no
#                                                  skill), in a Docker container that sees only
#                                                  <dir> (read-only but for <dir>/<output> and
#                                                  <dir>/sessions), or, where the batch's services
#                                                  are given, in the pod, in a namespace that
#                                                  shows <dir>, read-write whole (no mount onto
#                                                  a file), and a HOME of its own (cmd_judge_omp),
#                                                  output to <dir>/out.txt, the model and thinking
#                                                  level to <dir>/judge-model.txt
#   rig.sh class-search [<checkout>]               every class of defect INFRA-764's review
#                                                  rounds found, each as a named check of the
#                                                  checkout (this one, or <checkout>'s own
#                                                  class-search.ts), each hit with its verdict by
#                                                  the check's rule, the declared residuals apart
#                                                  (class-search.ts); exits 0 when no check has an
#                                                  instance outside them and none is stale
#
# A batch runs each run as `rig.sh run <scenario> <label> <n> <index> <start>`, which needs that
# batch's services: <index> is the run's place in the batch and <start> the batch's start in epoch
# seconds. One run on its own is `rig.sh batch <scenario> 1 <label>`.
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
#   reviewer-text-findings
#                   A reviewer phase worker, booted as tester-proof boots its tester, is told the
#                   tester has finished; its world is worker_fixture's with the reviewer's four
#                   additions: the change under review carries two text findings and no
#                   behavioural one (a sentence of docs/cli.md the change makes false, and a
#                   comment of .github/workflows/greet.yml naming a trigger the file does not
#                   carry, a file the implement App may push and the review App may not), the gh
#                   stand-in serves the pull request's one unresolved bot thread and answers
#                   `resolveReviewThread` as GitHub answers a role with write access, the run's
#                   issue holds
#                   the tester's passing result at the head as well as the implementer's (seed.ts
#                   tester-proof, implement-handoff and tester-result), so no gate of the
#                   reviewer's own refuses an approval, and the role booted is reviewer. The run's
#                   remote takes the reviewer's push of a commit that changes no workflow file,
#                   which moves the pull request's head, and refuses one that changes the
#                   workflow with GitHub's line (fixture_first_commit).
#                   SKILL_SCENARIOS_REVIEWER_CASE names the world: `text-only`, the change above,
#                   whose code is correct, which k1 measures; `defect`, that change with the CLI's
#                   blank-name guard dropped — a finding whose closing changes code that runs —
#                   which is k1's negative control; and `last-push`, text-only's world whose base
#                   rules require someone other than the last push's pusher to approve it
#                   (`require_last_push_approval: true`, base_ruleset_routes), which k3 measures.
#                   The case is in the run's record (cmd_run), whose name,
#                   reviewer-text-findings-<case>-<label>-<n>, keeps each case's batch's runs
#                   numbered apart, and never in its world. `base`, main's checkout, is the arm k1
#                   and k3 read as the cost: a base run's `REQUEST_CHANGES` on these text findings.
#                   Scored by reviewerTextFindings, counted per case and label.
#   threads-closed  The implementer's last chance at a reviewer's text commit, and the reviewer's
#                   answer to its push (spec v36's R2). One run boots its agents against one
#                   world, one after the other, each in a live session of its own and all on the
#                   run's one directory, gh stand-in and remote, so the second reads what the
#                   first left (run_threads_closed, worker_boot). First the implementer, which
#                   the reviewer's message naming its commit and its threads reaches in its own
#                   live session; its world is worker_fixture's with the round's additions: the
#                   reviewer's own text commit is the pull request's head and no approval stands
#                   (the issue in `reviewing`), or, in code-defect, the reviewer pushed nothing and
#                   its approval stands on the implementer's commit (the issue in `accepting`);
#                   the threads it left open are in the world's GraphQL graph and
#                   <run>/threads.json, the body carries the reviewer's `Threads:` count and one
#                   open entry per thread, `resolveReviewThread` answers as GitHub answers a role
#                   with write access, and the issue holds the implementer's and the tester's
#                   passing results at the implementer's commit, the head below the reviewer's
#                   commit, none at the reviewer's commit, and the reviewer's handoff (seed.ts
#                   tester-proof, implement-handoff, tester-result and review-handoff). Then,
#                   where the case has one, the reviewer, woken by the message the implementer
#                   published to its role topic and by nothing else (reviewer_wake,
#                   <run>/reviewer-wake.json). Each agent's calls carry the role that made them
#                   (its pane's LEGION_ROLE, a recorded call's `by`), so the run's one calls.jsonl
#                   and one pushes.log hold both halves and the score reads each.
#                   SKILL_SCENARIOS_THREADS_CASE names the world, and is required: `accept`, the
#                   reviewer's commit right and no thread left, which the implementer accepts
#                   in a comment and a message to the reviewer and the architect, pushing
#                   nothing, and which boots no reviewer; `correct`, the reviewer's commit making
#                   the document's sentence false another way and leaving a thread on the
#                   workflow file, which the implementer corrects in one push of text and the
#                   reviewer then resolves and approves; `crossing`, accept's world with a check
#                   that compares `bun greet.ts Ada` with the example line of docs/cli.md the
#                   reviewer's commit rewords, so the tester's earlier pass does not hold there
#                   (k3), which boots no reviewer; and `code-defect`, one thread whose sentence is
#                   false only because the code is wrong, which no text fix closes and which goes
#                   to the architect and the reviewer, neither role moving the issue — the
#                   negative control. The case is in the run's record, whose name,
#                   threads-closed-<case>-<label>-<n>, keeps each case's batch's runs numbered
#                   apart, and never in its world. `base`, main's checkout, is the arm each case is
#                   scored against. Scored by threadsClosed, counted per case and label.
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
#                   marker checks warn. The case and a set mode are in the run's record, whose
#                   name, askgate-<case>-<label>-<n>, keeps each case's batch's runs numbered
#                   apart (cmd_run, run_askgate). Scored by
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
# A batch's services are a Postgres, a NATS, the Dispatch server every run starts its own Dispatch
# on, built from this checkout, and one Envoy listener for each label, built from that label's own
# checkout (services_up), since a label's agents load that checkout's plugins and production runs
# a machine's listener from the release its plugins come from. Each binary is built once per
# checkout and kept under <work>/builds (go_binary). Each run carries the provenance of every
# service and program it is built or started from, <run>/services-from beside its built-from
# (services_from), and `rig.sh health` holds every service of its label's own to that label's
# checkout. The Postgres and the NATS are two containers the batch starts and removes, or, when
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
# port and docker assigns a container's host port; each listener serves on a port the kernel chose
# and scripts/e2e/lib/rig.sh holds for it until it serves (start_held), each run's Dispatch on its
# own run-server.sh holds the same way and reports, and the kernel assigns each run's daemon
# stand-in its port.
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
# `judge` and `judge-omp` also need Docker, where the batch's services are not given: each judge
# then runs in a container. Where they are given (SKILL_SCENARIOS_POSTGRES_URL and
# SKILL_SCENARIOS_NATS_URL), each judge runs in the pod, in a namespace of its own (cmd_judge_omp).
# The rig runs a mise of its own, never the caller's: a worker pod's image carries none (its
# `tools` stage alone installs mise), and the caller's is any version at all. `profile`,
# `live-read`, `batch` and `judge-omp` fetch it with curl (rig_mise) at the worker image's own
# pin, the ARG MISE_VERSION and ARG MISE_SHA256 of packages/daemon/docker/worker.Dockerfile, read
# there on every call, from the release URL that image's tools stage fetches. A fetch that fails,
# or an archive whose SHA-256 is any other, is refused, naming the step, and installs nothing; a
# mise an earlier call installed from that archive is reused. It is <work>/tools/bin/mise, first
# on the PATH the rig, the profile installer and every agent run on.
# Every run's agent starts in a detached tmux session (launch), and the rig brings no tmux of its
# own: `batch` and `run` first run `tmux -V` and start and kill one detached session on a socket
# of their own under the work directory (tmux_ready), and refuse, before they make any database,
# service or run directory, naming tmux, the step that failed and what tmux printed on stderr. A
# batch's runs take its PATH, and so the tmux it checked, and are not checked again. A worker
# pod's image carries tmux, trixie's package (packages/daemon/docker/worker.Dockerfile's apt
# list), which answered `tmux 3.5a` on 2026-11-25.
# Optional:
#   SKILL_SCENARIOS_WORK      work directory, outside any checkout, and with no `.omp` in it or in
#                             any directory above it (default ${TMPDIR:-/tmp}/work); its path
#                             names no scenario, case or label, nor the rig (launch refuses one)
#   SKILL_SCENARIOS_PARALLEL  concurrent runs in a batch (default 5)
#   SKILL_SCENARIOS_TIMEOUT   seconds one run, or one turn of a spec run, may take (default 1500)
#   SKILL_SCENARIOS_COMPOSE_ONLY  set to anything, each run composes its world up to its first
#                                 agent, runs launch's guard over it and ends there, exit 0, with
#                                 no agent started (launch): a batch's worlds read before any model
#                                 call is made, each run archived as any run is
#   SKILL_SCENARIOS_ASKGATE_CASE  askgate: facts, link, upload, nochange, nochange-rebuttal,
#                                 nochange-block, untried or tried (required for that scenario)
#   SKILL_SCENARIOS_ASKGATE_MODE  askgate: warn or block, the agent's OMP_ASKGATE (unset: the
#                                 verdicts block and the marker checks warn)
#   SKILL_SCENARIOS_REVIEWER_CASE reviewer-text-findings: text-only, defect or last-push (required
#                                 for that scenario)
#   SKILL_SCENARIOS_THREADS_CASE  threads-closed: accept, correct, crossing or code-defect (required
#                                 for that scenario)
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
# For the rig's own tests (`bun test`), never a batch:
#   SKILL_SCENARIOS_OLDER_CHECKOUT a whole checkout of main ff2a10ef (2026-11-07), an older daemon
#                               (completion-answers.ts's `refused` engine and `issue` route), say
#                               `git worktree add <dir> ff2a10ef`. daemon-standin.test.ts's "an
#                               older daemon's late completion is refused in that daemon's own
#                               sentence" reads its packages/daemon/internal/workflow/engine.go
#                               and internal/api/handoff.go and boots the daemon stand-in on it,
#                               which needs go and runs `go run -overlay` of daemon-pane.go in its
#                               packages/daemon (its packages/envoy beside it, as go.mod
#                               replaces it), so that module's dependencies must be in the Go
#                               module cache or fetchable. Unset, the row skips and says so
#
# Each run:
#   - Its agent is `omp -p` on the Oh My Pi the daemon pins (.omp-pin), in the label's profile
#     (make_omp_home, install-plugin-profile.sh, install-model-gateway.sh), under a HOME of its own
#     copied from the label's, with `env -i` and only the variables its pane file names (base_env).
#     An agent that writes to its HOME (`mise use -g`, say) changes nothing for another run.
#   - Its profile's settings set extensionHandlers.toolCallTimeoutMs to 120000 or more, as the model
#     route installers write them, so AskGate's review fits inside Oh My Pi's handler limit
#     (load_label refuses a profile that does not).
#   - Its directory is <work>/runs/<id>, an id of twelve hex digits, and its record, which says what
#     the run is — its scenario, case, label and number, its name <scenario>-<label>-<n>
#     (<scenario>-<case>-<label>-<n> for a cased scenario: askgate, reviewer-text-findings and
#     threads-closed), its batch and where it ran — is <work>/runs/<id>.json beside it (cmd_run,
#     run-names.ts's RunRecord), which no agent of the run can read. Every path, variable and
#     process its agents can read is built on the id, and launch starts no agent whose world names
#     a word of the scenarios', cases' or labels' (run-names.ts guard). It runs in the tmux
#     session <tag>-<id> on the work directory's own tmux server (<work>/tmux.sock). The pane
#     shows nothing: omp's output goes to <run>/out.txt, and the transcript, which grows as the
#     agent works, to the .jsonl under <run>/sessions. A run that boots more than one agent
#     (threads-closed) runs each in a tmux session and an Oh My Pi session of its own, under the
#     one run directory: the first agent's transcript is the run's own, each later agent's is in
#     <run>/sessions/<role>/, and each agent's last line of output is kept as
#     <run>/out-<role>.txt, out.txt being the last agent's (worker_boot).
#   - `gh` and `bun` stand-ins are first on its PATH (standins): no run reaches GitHub, and a bun
#     it reaches through PATH is the bun this script runs on, recorded. A bun named by its path or
#     run through `mise exec` bypasses the stand-in and goes unrecorded. Every container and tmux
#     session a work directory starts carries the directory's digest in its name. The gh stand-in
#     is a bundle of this script's own gh-standin.ts in the run's own directory, whichever label
#     the run is, so no run reads this script's directory (score.ts's RIG_DIR reads a run that
#     ran the stand-in from there, as runs before the names left the world did).
#   - Its pushes meet the push guard a Legion pod's do: the caller's `git` and `jj` (on a pod,
#     /opt/legion/bin's leak-check wrappers) with the caller's PUBLIC_LEAK_CHECK in its pane. A run
#     whose `git` is such a wrapper and whose caller names no readable PUBLIC_LEAK_CHECK is
#     refused before anything of it is built (push_guard).
#   - A worker run gets a Legion project of its own, digested from its scenario, its case, its
#     label, its number and its batch (run_project), since the Envoy role topics it claims carry
#     that project and every worker run is booted on one issue key.
#   - A phase worker whose claim boot is refused before its first turn (the daemon stand-in refuses
#     its `claims/register`, or its plugin prints `claim boot failed at …` and exits, as round
#     23's reviewer did on the Envoy listener's consumer refusal) is launched once more in the same
#     run, as production's daemon relaunches it, and the first attempt is kept under
#     <run>/boot-1/<role>/ (boot_agent). Once, where the daemon's launch budget allows more: a
#     second refusal stands, and health names both attempts. One refused after its first turn is
#     never relaunched.
#   - A worker run's boot record, <run>/booted, holds a line for each role its scenario boots:
#     `boot <role>` before that role's daemon stand-in starts, again before a relaunch, each
#     launch boot_agent asks about followed by `since <role> <time>`, and `skip <role> <reason>`
#     where the rig boots none, the reason completion-answers.ts's SKIPS declares (worker_boot,
#     boot_agent, run_threads_closed). health.ts reads it.
#   - Its Envoy listener, its Dispatch server and the daemon facts its stand-in answers by are its
#     label's own: services_up builds the listener, the server and daemon-pane.go out of the
#     label's checkout and reads every fact a stand-in boots on before any run starts
#     (label_facts), so a stand-in's boot reads its facts out of files.
#   - Its agent runs in a user, mount and PID namespace of its own, started with `env -i` (launch,
#     world_paths, private_world): the scratch root TMPDIR names, the issue workspaces, the work
#     directory, every label's checkout, /var/run/legion and /var/run/secrets are each an empty
#     tmpfs, and only the run's world is shown again: its own directory, its label's install, the
#     batch's tokens and the tools its pane names: no label's checkout and not this script's own
#     directory, whose path names the rig.
#     /tmp is the run's own $R/tmp and /proc holds its own processes alone. It can still read the
#     rest of the filesystem, the operator's home included. A host that refuses the namespace
#     starts no run (require_namespace). score.ts does not score a run
#     that reads a skill file outside its own label or a file of another label's checkout, or whose
#     PR body carries another run's line (unscored, testerProof). It flags, and still scores, a run
#     whose tool calls only name such a skill file or checkout (skillReaches), or name a path
#     outside its work directory, its label's checkout and the system locations (outsideReaches): a
#     flag shows the path was named, not that it was read. A path a call reaches without naming it,
#     as a glob from `/` does, is not caught. health.ts calls a run unhealthy, whatever it scored,
#     when a tool call names another run's directory, a path its world hides and does not show,
#     another process's /proc environ or cmdline or a credential path, or reaches a host off its
#     world, or reads a path another run of the batch wrote or /tmp scratch from before the run:
#     the second line behind the namespace.
#   - An exit stops what its command started (on_exit, stop_run, stop_batch); a serial run's exit
#     then records what appeared at the top of the machine's /tmp during the run (scratch_clean),
#     and moves the run's directory to <work>/archive/runs/<run> (archive_run), out of the runs/ a
#     later run lists.
#   - It never removes or replaces another run: a run whose name a live or archived run holds is
#     refused before it starts (cmd_run), and an archived run of its name is refused, never
#     replaced, when it ends (archive_run). The rig deletes only what the same run or command
#     made, and refuses a file an earlier one left where it would write (lock_services,
#     judge_profiles, cmd_judge_omp, cmd_live_read). The one exception is the gateway key
#     command's cached token, which a judge drops to mint one that outlives it (judge_keys).
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
# The work directory's default is a word of no measurement: every path a run's agent can read is
# built on it, and its old default, `skill-scenarios`, was in every one of them. launch starts no
# agent whose world names a word of the vocabulary, a work directory's included (run-names.ts guard).
work=${SKILL_SCENARIOS_WORK:-${TMPDIR:-/tmp}/work}
work=$(realpath -m -- "$work")

# The worker world (worker_fixture below), which tester-proof, reviewer-text-findings and
# threads-closed share: its repository and default branch, and the run's own issue, which each
# scenario's runner sets once seed.ts has filed it.
#
# These two are written here and nowhere else in the rig, and this script is what builds the
# world out of them: fixture_open opens the remote and the checkout on them, fixture_write
# writes them into the world the run keeps ($R/world.json) and into the gh stand-in's routes
# ($R/fixtures.json), and the pane's own GH_REPO and each runner's architect prompt name them.
# Every part outside this script reads them back out of $R/world.json: the repository the gh
# stand-in answers a review route of and the branch it diffs against (gh-standin.ts), the record
# the daemon stand-in answers a `read_record` with (daemon-standin.ts) and the addressing of the
# pane's own system prompt (worker-pane.ts). This is the one place that story is told, and every
# other comment on it points here.
#
# That record is production's record of the run's issue (standin-record.ts), and what it says of
# the issue's history it reads from where this script's seeds left it, never from a list of its
# own: the seeded handoff records by the ids $R/fixture.json names (worker_run_world and its
# runners), each read back from the run's own Dispatch with the session that wrote it, as the
# row of the role whose round it stands for; the spec's approval, which is the record's design
# gate (seed.ts tester-proof approves the spec); the pull request as the run's gh answers it;
# and the agents this script boots, by what each sends its stand-in. In INFRA-764's round 25 the
# record held eight keys and `workers: {}`, and every counted threads-closed run read it before
# its first write and saw no row of the implementer's round this script says stands.
#
# A second name is a second world, and an agent follows whichever name it is given. Of the forty
# runs recorded on 2026-11-10, six submitted their review to a repository their world has not
# got: four to `sjawhar/legion-smoke`, which the daemon stand-in's record named while the world
# was this one, their other queries answering 404, and two to `skills-rig/project`, which the
# pane's own addressing named — one of those two agents wrote that the architect's note and the
# addressing line gave two repositories. The scorer read those reviews as no review at all
# (gh-call.ts's `reviewsRoute`).
#
# The repository's name is one a team's could be, which the public leak check passes and which
# names no host and no deployment: it was `example/widgets`, and two agents of the batch at
# c273084d took the `example` for a simulation.
worker_key=''
worker_repo=fernbrook/widgets
worker_base=main

# The developer the repository's own history is written by, from before any role of Legion worked
# on it: the default branch's commit every worker world opens on (fixture_first_commit, through
# fixture_git) and the brainstorm run's to-do CLI. Every commit a role makes, seeded or the run's
# own, carries that role's App instead (role_identity). `git log` is something the world writes
# and the agent reads, so this is a name and an address a repository could hold, in no word of
# the rig's own: the world's commits were `Fixture`'s, the brainstorm repository's `rig`'s and
# jj's `Rig Worker`'s (review 5887659447, finding 2), and rig.test.ts's "no file, commit, commit
# identity or gh answer of a run's world holds a word of the rig's" holds every world to that.
world_author_name='Robin Hale'
world_author_email=robin.hale@example.invalid
# The two GitHub Apps a worker run's roles act as, each a slug and the id of its bot user, as
# production's daemon gives every role its App's bot identity (role_identity): the implement App
# for the roles that write an issue branch, the review App for every other. Names a repository's
# own Apps could have, in no word of the rig's.
world_implement_app=widgets-implementer
world_implement_app_id=318406215
world_review_app=widgets-reviewer
world_review_app_id=318406277
# The login of the user whose token a run's gh calls are made with, which every answer of the
# world that names a user carries (`gh api user`, the create of a review): the review App's bot
# login, in no word of the rig's own — the world answered `rig[bot]` — and the same rig.test.ts
# row reads every answer the world's routes serve for that.
world_login='widgets-reviewer[bot]'
# The remote helper the issue workspace's origin names (`remote.origin.vcs`), which git runs as
# `git-remote-<vcs>` from the pane's PATH for every fetch and push of origin (world_remote): the
# name of a helper a GitHub remote could carry, one for a GitHub App's installation, in no word of
# the measurement. run-names.ts's WORLD_REMOTE_VCS is the same name, which health's names rule
# reads an agent's text by, since no production clone has the key.
world_remote_vcs=ghapp

# role_identity ROLE prints, a tab between them, the name and the email every commit ROLE makes
# carries: its App's bot identity, `<slug>[bot]` and `<id>+<slug>[bot]@users.noreply.github.com`,
# the implement App for the implementer and the merger and the review App for every other role,
# as production decides it (packages/daemon/internal/appauth's `AppRoleFor` in identity.go and
# `botIdentity` in appauth.go). Every pane and every seeded commit of one author told the agents
# that read `git log` that one account made every role's work (INFRA-764 round 24: "the harness
# looks like it's using robin.hale as the default committer").
role_identity() {
  local slug=$world_review_app id=$world_review_app_id
  case $1 in implementer | merger) slug=$world_implement_app id=$world_implement_app_id ;; esac
  printf '%s[bot]\t%s+%s[bot]@users.noreply.github.com\n' "$slug" "$id" "$slug"
}

# role_env ROLE prints the six variables production's runtime gives a pane of ROLE so every
# commit it makes, with jj or with git, carries ROLE's identity (runtime.go's `GitIdentity.Env`):
# each wins over any configuration, and over the operator's own identity, which a Legion pod
# that runs this rig carries in the same six.
role_env() {
  local name email
  IFS=$'\t' read -r name email < <(role_identity "$1")
  printf '%s\n' "JJ_USER=$name" "JJ_EMAIL=$email" "GIT_AUTHOR_NAME=$name" \
    "GIT_AUTHOR_EMAIL=$email" "GIT_COMMITTER_NAME=$name" "GIT_COMMITTER_EMAIL=$email"
}

# git_pairs prints the five git settings production's runtime gives every role process
# (runtime/sandbox's gitConfigPairs in manifest.go, as GIT_CONFIG_COUNT and its numbered pairs, in
# its spelling), which `git config -l --show-scope` lists as the `command` scope: GitHub's
# credential through `legion git-credential`, no fsmonitor and no hook. None changes what a fetch
# or a push of the world does (measured on 2026-11-18): the remote helper asks no credential and
# drops the pairs for the remote side (world_remote), the leak check's own ls-remote reaches the
# run's remote and asks none either (push_guard), and a push's pre-push hook still runs, since
# Legion's git wrapper puts its own `-c core.hooksPath=` after them, which git reads last.
git_pairs() {
  printf '%s\n' GIT_CONFIG_COUNT=5 \
    GIT_CONFIG_KEY_0=credential.https://github.com.helper GIT_CONFIG_VALUE_0= \
    GIT_CONFIG_KEY_1=credential.https://github.com.helper 'GIT_CONFIG_VALUE_1=!legion git-credential' \
    GIT_CONFIG_KEY_2=credential.https://github.com.useHttpPath GIT_CONFIG_VALUE_2=true \
    GIT_CONFIG_KEY_3=core.fsmonitor GIT_CONFIG_VALUE_3= \
    GIT_CONFIG_KEY_4=core.hooksPath GIT_CONFIG_VALUE_4=/dev/null
}

# adopt_workspace ROLE gives the issue workspace's working copy ($R/ws) to ROLE before its agent
# starts, as production's runtime does at every role's start (packages/daemon/internal/shim's
# `runAdoption`): an undescribed working copy takes ROLE's identity as its author (`jj metaedit
# --update-author`), and one a previous role described, its own work, is kept and ROLE starts on a
# new one (`jj new`). jj sets a change's author when it makes the change, so without it a pane's
# first commit carried the author of the change the fixture made, whatever the pane committed
# as. A run with no workspace has nothing to adopt. Its jj reads the repository's configuration
# where the agents' jj does (fixture_workspace), and so writes none into the operator's.
adopt_workspace() {
  local identity described
  [ -d "$R/ws/.jj" ] || return 0
  mapfile -t identity < <(role_env "$1")
  identity+=("XDG_CONFIG_HOME=$R/state/home/.config")
  described=$(cd "$R/ws" && env "${identity[@]}" JJ_CONFIG="$R/jj.toml" \
    jj log -r '@ ~ description(exact:"")' --no-graph -T commit_id) || return 1
  if [ -n "$described" ]; then
    (cd "$R/ws" && env "${identity[@]}" JJ_CONFIG="$R/jj.toml" jj new) >/dev/null 2>&1
  else
    (cd "$R/ws" && env "${identity[@]}" JJ_CONFIG="$R/jj.toml" \
      jj metaedit --update-author -r '@ & description(exact:"")') >/dev/null 2>&1
  fi
}

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
# packages/envoy/scripts/lib/pg-url.sh's pg_url_run and pg_url_without_password: pg_sql gives psql
# a given Postgres URL's password in PGPASSWORD alone, and safe_url prints that URL without it.
# shellcheck source-path=SCRIPTDIR/../../../.. source=packages/envoy/scripts/lib/pg-url.sh
. "$root/packages/envoy/scripts/lib/pg-url.sh"

# The work directory's tag, which names its containers, its tmux sessions and each listener's
# machine id. A listener's machine id is what an agent's `envoy_whoami` answers, so the tag is a
# word of no measurement, as a machine's name is: it was `skill-scenarios-<digest>`.
tag=legion-$(printf '%s' "$work" | sha256sum | cut -c1-8)
tmux_socket=$work/tmux.sock
tmux() { command tmux -S "$tmux_socket" "$@"; }

# Where rig_mise installs the rig's own mise, under the work directory and in no word of the
# measurement's or the rig's: every agent's world shows it (world_paths), and launch's guard
# refuses a world that names one.
mise_dir=$work/tools/bin

# What a command started, for its exit trap: the run directory and its agent's tmux session, the
# pids lib/rig.sh's start_process sets (a run's own Dispatch server among them), each label's Envoy
# listener (listener_pids, one per label of the batch), and the services' ports and directory.
# `booted` is the role of the agent a run has already run, empty until the first one, and
# `sessions_dir` the Oh My Pi session directory of the agent running now: a run that boots more
# than one agent gives each its own (worker_boot). `services_env` is the services file of the
# run's label (cmd_run), which a worker run's pane and its recorder read. `batch_start` is the
# batch's start second, which each of its runs' records names (stop_batch).
R='' session='' daemon_pid='' dispatch_pid='' github_pid='' record_pid='' runs_pid='' batch_scenario='' batch_start=''
watcher_pid=''
S='' envoy_port='' dispatch_port='' pg_url='' nats_url='' booted='' sessions_dir='' services_env=''
listener_pids=()

label_dir() {
  [[ $1 =~ ^[a-z0-9][a-z0-9-]*$ ]] || fail "label '$1' is not lower-case letters, digits and hyphens"
  printf '%s\n' "$work/profiles/$1"
}

# The label's checkout, pin and profile name, recorded by `profile`, as the globals P (the label's
# own directory, <work>/profiles/<label>), I (where its profile is installed, its HOME and its
# plugins: <work>/installs/<id>, the id in $P/install, so no path a run's agent reads names the
# label; $P itself for a label built before the names left the world), co, pin and profile that
# every command and helper below reads.
load_label() {
  P=$(label_dir "$1")
  [ -f "$P/ready" ] || fail "no profile for label $1: run \`rig.sh profile $1 <checkout>\` first"
  I=$P
  [ ! -f "$P/install" ] || I=$work/installs/$(<"$P/install")
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
  local settings=$I/home/.omp/profiles/$profile/agent/config.yml remove=$P said read=0
  [ "$I" = "$P" ] || remove="$P and $I"
  said=$(bun -e 'const v = Bun.YAML.parse(await Bun.file(process.argv[1]).text())?.extensionHandlers?.toolCallTimeoutMs; process.exit(typeof v === "number" && v >= 120000 ? 0 : 3)' "$settings" 2>&1) || read=$?
  case $read in
    0) ;;
    3) fail "label $1's profile settings $settings do not set extensionHandlers.toolCallTimeoutMs to 120000 or more, so AskGate's review would often time out: remove $remove and run \`rig.sh profile $1 <checkout>\` from a checkout whose model route installer sets it" ;;
    *) fail "label $1: bun could not read the profile settings $settings: $said" ;;
  esac
}

# label_model prints the model the agents of the label load_label loaded last answer with: the
# `default` of the `modelRoles` its model route wrote into its profile's settings
# (install_model_route), `<provider>/<model>`, the form the daemon names a step's model in
# (packages/daemon/internal/stream's Answered); nothing for a profile whose settings are not there
# or name none. The world's history names it at the end of each run of a step (seed.ts's `steps`),
# as the label's daemon names the model each of those agents last answered with.
label_model() {
  local settings=${I:-}/home/.omp/profiles/${profile:-}/agent/config.yml
  [ -n "${I:-}" ] && [ -f "$settings" ] || return 0
  bun -e 'const v = Bun.YAML.parse(await Bun.file(process.argv[1]).text())?.modelRoles?.default; if (typeof v === "string") console.log(v);' "$settings"
}

# steps_seed PROJECT PHASE writes the record of each run of a step the run's world has run, as
# the label's daemon wrote them onto the run's Dispatch, with PHASE's run open, under the claims of
# the Legion project PROJECT (seed.ts's `steps`), before any agent of the run boots: so `dispatch
# steps` lists the runs the handoffs and the record show. A label whose daemon writes none gets
# none.
steps_seed() {
  seed_ts steps "$R/fixture.json" "$1" "$2" "$(label_model)"
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

# rig_mise installs the rig's own mise at $mise_dir/mise, unless it is there already, and puts
# $mise_dir first on PATH. Every `mise` the rig runs is that one: install-plugin-profile.sh's
# (`mise where`, `mise install` and `mise x`), pinned_omp's, the superpowers probe's, the judge's
# `mise where`, and the `mise x <pin>` each agent starts under in its namespace (launch), which
# finds it on the pane's PATH (base_env), in a world that shows it read-only (world_paths). The
# worker image installs mise in its `tools` stage alone, to resolve the pinned Oh My Pi, jj and gh,
# and its final image carries none, so a worker pod has no mise but this one; on another host the
# caller's mise is any version at all. `profile`, `live-read`, `batch` and `judge-omp` call this
# before their first `mise`. `rig.sh run`, which a batch alone starts, inherits the batch's PATH,
# and cmd_world reads the mise its run's pane names.
#
# The pin is the worker image's own: the ARG MISE_VERSION and ARG MISE_SHA256 of this checkout's
# packages/daemon/docker/worker.Dockerfile, read there on every call and written nowhere here, so
# the rig's mise and the image's cannot drift apart, and the archive is the one that image's tools
# stage fetches. A mise whose record (<work>/tools/installed, beside $mise_dir) says it came from
# the archive of that URL and that SHA-256, and whose bytes are the ones recorded, is reused, and
# nothing is fetched. Any other is fetched again. A fetch that fails, an archive whose SHA-256 is
# any other, an archive with no mise/bin/mise, and a binary that does not run are each refused,
# naming the step, what it expected and what it got. Each leaves nothing installed: the archive is
# fetched and unpacked in a directory of its own under the work directory, removed before this
# returns or refuses, and only a checked binary is moved into place.
rig_mise() {
  local dockerfile=$root/packages/daemon/docker/worker.Dockerfile record=${mise_dir%/*}/installed
  local version sha url pin tmp out status=0 got
  version=$(sed -n 's/^ARG MISE_VERSION=//p' "$dockerfile" 2>/dev/null) || version=''
  sha=$(sed -n 's/^ARG MISE_SHA256=//p' "$dockerfile" 2>/dev/null) || sha=''
  [[ $version =~ ^[A-Za-z0-9._-]+$ && $sha =~ ^[0-9a-f]{64}$ ]] ||
    fail "mise pin: expected one ARG MISE_VERSION=<release> and one ARG MISE_SHA256=<sha256> in $dockerfile, got '$version' and '$sha'; nothing was installed"
  url=https://github.com/jdx/mise/releases/download/$version/mise-$version-linux-x64.tar.gz
  pin="archive $url $sha"
  if ! { [ -x "$mise_dir/mise" ] && [ -f "$record" ] &&
    [ "$(<"$record")" = "$pin"$'\n'"mise $(sha256sum <"$mise_dir/mise" | cut -d' ' -f1)" ]; }; then
    tmp=$(mktemp -d "$work/.mise.XXXXXXXX")
    out=$(curl -fsSL -o "$tmp/mise.tar.gz" "$url" 2>&1) || status=$?
    [ "$status" = 0 ] || {
      rm -rf -- "$tmp"
      fail "mise fetch: expected the archive of mise $version (ARG MISE_VERSION of $dockerfile) from $url, got curl exit $status: ${out:-no message}; nothing was installed"
    }
    got=$(sha256sum <"$tmp/mise.tar.gz" | cut -d' ' -f1)
    [ "$got" = "$sha" ] || {
      rm -rf -- "$tmp"
      fail "mise check: expected SHA-256 $sha (ARG MISE_SHA256 of $dockerfile) of $url, got $got; nothing was installed"
    }
    out=$(tar -xzf "$tmp/mise.tar.gz" -C "$tmp" --no-same-owner mise/bin/mise 2>&1) || {
      rm -rf -- "$tmp"
      fail "mise unpack: expected mise/bin/mise in the archive of $url, got: $out; nothing was installed"
    }
    chmod 0755 "$tmp/mise/bin/mise"
    out=$("$tmp/mise/bin/mise" --version 2>&1) || status=$?
    [ "$status" = 0 ] || {
      rm -rf -- "$tmp"
      fail "mise run: expected the mise of $url to print its version, got exit $status: $out; nothing was installed"
    }
    printf '%s\nmise %s\n' "$pin" "$(sha256sum <"$tmp/mise/bin/mise" | cut -d' ' -f1)" >"$tmp/installed"
    mkdir -p "$mise_dir"
    mv -f -- "$tmp/mise/bin/mise" "$mise_dir/mise"
    mv -f -- "$tmp/installed" "$record"
    rm -rf -- "$tmp"
    note "mise: fetched $url, SHA-256 $got as ARG MISE_SHA256 of $dockerfile expects; installed $mise_dir/mise, which prints ${out%%$'\n'*}"
  fi
  [ "${PATH%%:*}" = "$mise_dir" ] || PATH=$mise_dir:$PATH
}

# The profile, its HOME and its packed plugins, are installed under <work>/installs/<id>, an id of
# no meaning that $P/install records (load_label's I), and the label's own directory, $P, keeps
# what the rig reads of the label. The profile's name is production's own, `legion`, the same for
# every label. Every skill file an agent reads and its HOME are under the install, so no path it
# reads names the label: the profile was `skills-<label>` under <work>/profiles/<label>.
cmd_profile() {
  local usage="usage: rig.sh profile <label> <checkout> [--envoy-json <file>]"
  [ $# = 2 ] || [ $# = 4 ] || fail "$usage"
  local label=$1 P I co profile=legion envoy_json='' id
  if [ $# = 4 ]; then
    [ "$3" = --envoy-json ] || fail "$usage"
    envoy_json=$(realpath -e -- "$4") || fail "--envoy-json $4 does not exist"
    jq -e 'type == "object"' "$envoy_json" >/dev/null 2>&1 || fail "--envoy-json $4 is not a JSON object"
  fi
  P=$(label_dir "$label")
  [ ! -e "$P" ] || fail "$P exists; remove it, and the install its install file names, to rebuild label $label"
  co=$(cd "$2" && pwd -P)
  [ -f "$co/scripts/e2e/lib/install-plugin-profile.sh" ] || fail "$co is not a Legion checkout"
  # The plugin installer and the superpowers probe run the rig's own mise, fetched before anything
  # of the label is made, so a refused fetch leaves no label to remove.
  rig_mise
  id=$(openssl rand -hex 6)
  I=$work/installs/$id
  mkdir -p "$P" "$work/installs"
  mkdir -- "$I" || fail "the install $I could not be made (mkdir says why above)"
  on_exit "$(printf 'drop_unready_keys %q %q' "$P/ready" "$I/model-key")"
  printf '%s\n' "$co" >"$P/checkout"
  printf '%s\n' "$profile" >"$P/profile"
  printf '%s\n' "$id" >"$P/install"
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
    make_omp_home "$I/home"
  )
  # Both plugins: a tester-proof worker's pane loads the Legion extension, which exits a Legion
  # session that finds no Envoy plugin, and every plain session gets its Dispatch skills and tools
  # from the Envoy plugin.
  bash "$co/scripts/e2e/lib/install-plugin-profile.sh" --package pi-envoy --profile "$profile" --home "$I/home" --dest "$I/pi-envoy" >"$P/install.log" 2>&1 ||
    fail "install-plugin-profile.sh --package pi-envoy failed; see $P/install.log"
  bash "$co/scripts/e2e/lib/install-plugin-profile.sh" --package pi-legion --profile "$profile" --home "$I/home" --dest "$I/pi-legion" >>"$P/install.log" 2>&1 ||
    fail "install-plugin-profile.sh --package pi-legion failed; see $P/install.log"
  # The envoy.json a session of the label reads: a named profile's $PI_CODING_AGENT_DIR/envoy.json
  # (envoyConfigPath, packages/envoy-client/src/envoy-config.ts), which Oh My Pi sets to the
  # profile's agent directory. The given object is merged over whatever is there.
  if [ -n "$envoy_json" ]; then
    local agent=$I/home/.omp/profiles/$profile/agent
    mkdir -p "$agent"
    [ -f "$agent/envoy.json" ] || echo '{}' >"$agent/envoy.json"
    jq -s '.[0] * .[1]' "$agent/envoy.json" "$envoy_json" >"$agent/envoy.json.new"
    mv "$agent/envoy.json.new" "$agent/envoy.json"
  fi
  install_model_route "$co" "$profile" "$I" "${LEGION_E2E_MODEL_PROVIDER_MODEL:-}" >>"$P/install.log" 2>&1 ||
    fail "the model route (${LEGION_E2E_MODEL_ROUTE:-gateway}) could not be installed; see $P/install.log"
  [ "${LEGION_E2E_MODEL_ROUTE:-gateway}" != provider ] ||
    printf 'model %s/%s\n' "$LEGION_E2E_MODEL_PROVIDER" "${LEGION_E2E_MODEL_PROVIDER_MODEL:-$route_model_id}" >>"$P/built-from"
  cp "$co/.omp-pin" "$P/pin"
  [ -z "${SKILL_SCENARIOS_SUPERPOWERS:-}" ] || install_superpowers "$P" "$I" "$profile" "$(<"$P/pin")"
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

# install_superpowers P I PROFILE PIN copies SKILL_SCENARIOS_SUPERPOWERS's writing-plans and
# brainstorming skills into the profile's user skill directory under the label's install I, the one
# the pinned Oh My Pi reads for a named profile ($HOME/.omp/profiles/<profile>/agent/skills/<name>/
# SKILL.md). Then one `omp -p` under the profile reads skill://writing-plans, and the step fails
# when the reply holds `Unknown skill`, Oh My Pi's answer for a skill it did not load. It also
# fails when no tool result of the probe's transcript holds the skill's own heading, so a probe
# that read nothing does not pass. It records the plugin's version and each skill's digest in the
# label's built-from. It marks the label P with a `superpowers` file, which a spec run needs. The
# probe is the rig's own and no run's, kept under P, outside every run's world.
install_superpowers() {
  local P=$1 I=$2 profile=$3 pin=$4 src=$SKILL_SCENARIOS_SUPERPOWERS skill dest probe version omp
  omp=$(pinned_omp "$pin") || exit 1
  dest=$I/home/.omp/profiles/$profile/agent/skills
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
      env -i HOME="$I/home" USER="$USER" TERM=xterm-256color TMPDIR="$probe/tmp" PATH="$PATH" \
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

# push_guard prints the PUBLIC_LEAK_CHECK line every agent's pane carries, and refuses a world in
# which every push would be refused. A Legion pod's `git` (/opt/legion/bin/git, the worker image's
# packages/daemon/docker/git) runs every push with `-c core.hooksPath=/opt/legion/githooks`, whose
# pre-push hook runs `legion leak-check pre-push`, and its `jj` pushes through that git; the guard
# finds the shared check through PUBLIC_LEAK_CHECK, then public-leak-check.sh on PATH, then the
# pointer file under the configuration home (packages/daemon/internal/leakguard's Resolve). A pod
# carries PUBLIC_LEAK_CHECK (runtime/sandbox's mainEnvironment), and a run's configuration home is
# its own ($R/home, or $R/state/home for a worker pane), which holds no pointer file. So:
#   - PUBLIC_LEAK_CHECK set in the caller's environment: passed to the pane as it is, or, in a
#     world whose issue workspace has a remote of its own (world_remote), as the world's run of it
#     (below);
#   - the caller's `git` a leak-check wrapper (a script that sets `core.hooksPath=` for a push) and
#     PUBLIC_LEAK_CHECK naming no readable file: the run is refused, naming the variable, since
#     its guard would refuse every push the run made (tester v21, k1 and k2: every push of the
#     batch refused with `LEAK CHECK SETUP ERROR`, and agents reading other runs' directories for
#     a pointer file to get through);
#   - any other `git`, and no PUBLIC_LEAK_CHECK: nothing is added.
#
# The world's run of the check is $R/lib/public-leak-check.sh, which runs the caller's check (named
# in $R/lib/leak-check, which world_paths shows) with GIT_EXEC_PATH at $R/lib/git-core, for that
# process and its children alone. The check runs `git ls-remote -- <the push's URL>`, which is
# origin's https URL: a URL argument, so no remote's vcs helper takes it, and git reached the live
# github.com, refused (the world's repository is no repository there), asked `git credential fill`
# and so the pane's `legion git-credential`, and asked github.com again with the world's token
# (measured on 2026-11-18: GIT_TRACE_CURL showed 2 connections to github.com, one with
# `Authorization: Basic`, and `legion git-credential get`, `erase`, `get`). A GIT_CONFIG pair
# cannot route it: the check deletes every GIT_CONFIG* variable before its own git
# (public_leak_check.py's CALLER_GIT_CONFIG_ENV), measured with url.<remote>.insteadOf appended to
# the pane's pairs. GIT_EXEC_PATH it keeps, and its git-remote-https (world_remote) sends the
# world's own URL to the run's remote: measured, 0 connections, no credential asked, the clean
# push passed, a credential in an added line refused by the shared check and a planted private
# host by the guard's own rules. So the pane's PUBLIC_LEAK_CHECK names the run's path, not the
# pod's, which an agent that reads the variable or the file sees: a residual the README names.
push_guard() {
  local git_path check=${PUBLIC_LEAK_CHECK:-}
  if [ -n "$check" ] && [ -f "$check" ] && [ -r "$check" ]; then
    if [ -n "${R:-}" ] && [ -d "$R/lib/git-core" ]; then
      printf '%s\n' "$check" >"$R/lib/leak-check"
      check=$R/lib/public-leak-check.sh
      printf '#!/usr/bin/env bash\nGIT_EXEC_PATH=%q exec bash "$(<%q)" "$@"\n' \
        "$R/lib/git-core" "$R/lib/leak-check" >"$check"
    fi
    printf 'PUBLIC_LEAK_CHECK=%s\n' "$check"
    return 0
  fi
  if git_path=$(command -v git) && [ -f "$git_path" ] && [ "$(head -c 2 "$git_path")" = '#!' ] &&
    grep -qF 'core.hooksPath=' "$git_path"; then
    fail "the git on PATH ($git_path) is a leak-check wrapper, which refuses every push whose leak check it cannot find, and PUBLIC_LEAK_CHECK names no readable check (${PUBLIC_LEAK_CHECK:-unset}): set PUBLIC_LEAK_CHECK to the shared public-leak-check.sh, as a Legion pod's environment carries it"
  fi
  [ -z "${PUBLIC_LEAK_CHECK:-}" ] || printf 'PUBLIC_LEAK_CHECK=%s\n' "$PUBLIC_LEAK_CHECK"
}

# Writes the variables every agent gets to $R/pane.env: a HOME copied from the label's install, the
# label's profile, the run's stand-ins first on PATH and the rig's own mise next (rig_mise), its own
# TMPDIR, the push guard's check (push_guard), and nothing else from the caller's environment. No
# variable names the rig: the run's directory reaches the gh stand-in through its bundle
# (standins), and every process of the run carries the run in its HOME (run_env_pids). A run that
# boots a second agent calls this again, and that agent gets a HOME copied afresh from the label's,
# as the first one did; the HOME the agent before it wrote in is kept beside its logs and its
# output, under $R/home-<its role> (worker_boot).
base_env() {
  mkdir -p "$R/tmp"
  # Nothing is removed here: the HOME of the agent before this one is moved to home-<its role>
  # by the caller, so this copy lands on nothing. A directory still here is that move not made,
  # which `cp -a` would answer by nesting the label's home inside the last agent's.
  [ ! -e "$R/home" ] || fail "base_env: $R/home is still there; worker_boot moves it to home-<role>"
  cp -a "$I/home" "$R/home"
  {
    echo "HOME=$R/home"
    # A pod's shell need not export USER (the issue pods of 2026-11-17 did not), and `set -u`
    # stops the run on an unset one: the user a run's agent is is the caller's own, by id.
    echo "USER=${USER:-$(id -un)}"
    echo "TERM=xterm-256color"
    echo "TMPDIR=$R/tmp"
    echo "MISE_DATA_DIR=${MISE_DATA_DIR:-${XDG_DATA_HOME:-$HOME/.local/share}/mise}"
    echo "OMP_PROFILE=$profile"
    echo "PI_NOTIFICATIONS=off"
    echo "PI_NO_TITLE=1"
    echo "PATH=$R/bin:$PATH"
    push_guard
  } >"$R/pane.env"
}

# The run's stand-ins in $R/bin: `gh`, which records the call and answers from $R/fixtures.json,
# and `bun`, which runs the bun this script runs on (a caller's `bun` may be a shim that resolves
# nothing under the agent's HOME) and records the call to $R/calls.jsonl as gh-standin.ts records
# its own, with the directory it ran in (`cwd`) and that directory's `git rev-parse HEAD` (`head`,
# empty outside a repository), read before bun runs. The score reads the tester's run of the CLI
# from that record, and an agent's own run of the world's check at a commit from its `head`
# (score.ts's `ownRun`). No `legion` stands in: an
# agent reaches the daemon stand-in through its `legion` tool alone, never `legion` from bash.
#
# `gh` runs $R/lib/gh.js, a bundle of gh-standin.ts built here (`bun build --target=bun`), and
# never this script's own directory, which the run's world does not show: its path names the
# rig. The bundle opens with the stand-in's one input, the run's directory, as GH_DATA_DIR,
# shaped as gh's own GH_CONFIG_DIR is; the role that makes a call is the pane's own LEGION_ROLE,
# which the stand-in reads itself. So no text of the bundle, and no variable of the pane, names
# the rig.
#
# It opens no record: a scenario that reads none needs no file, and the two whose score reads
# this one are worker runs, whose boot opens it by running this `gh` itself, the one the pane's
# PATH resolves to, with `--open` (worker_boot).
standins() {
  local bun_real
  bun_real=$(bun -e 'console.log(process.execPath)')
  mkdir -p "$R/bin" "$R/lib"
  bun build "$here/gh-standin.ts" --target=bun --minify-whitespace --outfile "$R/lib/gh.js" \
    --banner "process.env.GH_DATA_DIR = $(jq -n --arg run "$R" '$run');" \
    >/dev/null || fail "standins: the gh stand-in did not build"
  printf '#!/bin/sh\nexec %q %q gh "$@"\n' "$bun_real" "$R/lib/gh.js" >"$R/bin/gh"
  cat >"$R/bin/bun" <<EOF
#!/usr/bin/env bash
at=\$(date -u +%FT%T.%3NZ)
cwd=\$PWD
head=\$(git rev-parse HEAD 2>/dev/null) || head=
status=0
$(printf %q "$bun_real") "\$@" || status=\$?
jq -cn --arg at "\$at" --arg by "\${LEGION_ROLE:-}" --arg cwd "\$cwd" --arg head "\$head" --argjson exit "\$status" \\
  '{at:\$at, as:"bun", argv:\$ARGS.positional, exit:\$exit, cwd:\$cwd, head:\$head} + (if \$by == "" then {} else {by:\$by} end)' \\
  --args -- "\$@" >>$(printf %q "$R/calls.jsonl")
exit "\$status"
EOF
  chmod +x "$R/bin/gh" "$R/bin/bun"
  [ -f "$R/fixtures.json" ] || printf '{"routes":[]}\n' >"$R/fixtures.json"
}

# The PATH the namespace's own commands (sh, mount, mkdir, cp and the inner unshare) run on, and
# the whole environment its first process gets: with a PID namespace of its own the run's PID 1 is
# that process until it execs the agent, and one that inherited the caller's environment would
# hold the caller's Dispatch and Envoy variables and credentials there.
namespace_path=/usr/sbin:/usr/bin:/sbin:/bin

# pane_var NAME FILE prints the value of the pane file FILE's last NAME line, as `env -i` gives
# the agent the last of two, and nothing for a file that has none or does not exist.
pane_var() {
  [ ! -f "$2" ] || sed -n "s/^$1=//p" "$2" | tail -n 1
}

# system_gitconfig PATH prints the system configuration file of the git PATH resolves, as that git
# names it to an editor (`git config --system --edit`, here `echo`) whether or not it is there,
# under `env -i` as a pane's git runs, and nothing where that git names none. It is the file the
# agents' git reads as its `system` scope, and the run's world shows it as the host holds it, with
# no mount (world_paths' `ro` line for it), and the run's provenance records its digest
# (services_from's `system-gitconfig`): the rig runs in the deployment's own pod, so the host's
# file is the one that deployment's panes read, for any deployment and any image. A rule of the
# rig's own about it held for one image only: the world showed the file empty (an `empty` kind, an
# empty file bound over it) because the worker image had no /etc/gitconfig, and INFRA's image of
# 2026-11-19 (worker-infra@sha256:c2b4e9b7…) carries one, `core.packedGitWindowSize = 32m` and
# `core.packedGitLimit = 256m`, which that pod's own git lists as its `system` scope. So the world
# hid two lines a production pane reads, and a gVisor pod refuses a mount onto a file (`mount
# point is not a directory`), so that pod refused every run. On a host that is no deployment's pod
# the file is that host's, not production's (a GitHub runner's holds safe.directory=* and four
# filter.lfs keys, CI's git 2.56.1 at 427b02d4): a limit of a run off the pod, which the README's
# world section names. No variable a pod's pane has not got stands in for the file either
# (GIT_CONFIG_NOSYSTEM, GIT_CONFIG_SYSTEM).
system_gitconfig() {
  env -i PATH="$1" GIT_EDITOR=echo git config --system --edit 2>/dev/null || true
}

# world_paths R WORK I PANE [PIN_DIR [CO]] prints the world of the run R, one `<kind> <path>` a
# line: what the run's namespace hides (private_world mounts it) and what health.ts judges a run's
# tool calls by. It is the one declaration of that world: launch writes it to $R/namespace.txt for
# the namespace, and health.ts reads that file for each run it judges, and nothing else, so the
# namespace and the detector cannot hold two lists; a run with no namespace.txt recorded no world,
# and health does not work one out again (`rig.sh world` prints one for a person to read, from
# the pod as it stands). WORK is the run's work directory, I
# its label's install (load_label), PANE its pane file and PIN_DIR the pinned Oh My Pi's install.
# The kinds:
#   cover        hidden behind an empty tmpfs: a path a run names under one, outside its own
#                directory and every rw and ro path, is one the run reached out of its world;
#   credentials  hidden the same way: any path a run names under one is a reach for a credential;
#   rw, ro       shown again at its own path, writable or read-only, where it exists; a path that
#                is a link, or lies under one, is shown at what it resolves to as well. A
#                directory is bound back (private_world). The one file, the system git
#                configuration the pane's git reads where the host has one (system_gitconfig), is
#                shown through no mount of its own, as the host holds it: it lies under no cover
#                (/etc on a pod), it is the system's and not the agent's user's, so that user
#                cannot write it, and a gVisor pod takes no mount onto a file.
# A run recorded before 2026-11-19 can hold one more kind, `empty`: the system git configuration
# shown empty and read-only, an empty file bound over it. health.ts reads such a run as it was
# recorded. Anything a cover hides that no rw or ro line shows again is out of the run's world,
# and /tmp is the run's own $R/tmp (private_world's last bind). Every directory shown is a mount
# the agent's /proc/self/mountinfo names, at its own path and from its own path, so no path shown
# names the rig or the run's scenario, case or label (launch's guard refuses one that does): no
# label's checkout is shown, and not this script's own directory either, whose path names the rig.
# CO, the label's checkout, is given by cmd_world alone, for a run recorded before the names left
# the world, whose world showed that checkout, this script's directory and the zod its gh
# stand-in imported there.
world_paths() {
  local R=$1 work=$2 I=$3 pane=$4 pin_dir=${5:-} co=${6:-} workspaces=/legion/workspaces dir file entry kind real
  # Hidden. The scratch root the caller's TMPDIR names: on a Legion pod every role of the issue
  # writes there, and it holds the work directory, the base label's checkout and every extraction
  # of a tree (rig-at-<commit>, head-checkout).
  printf 'cover %s\n' "$(realpath -m -- "${TMPDIR:-/tmp}")"
  # The issue workspaces, <state>/workspaces/<owner>/<repo>/<issue> (packages/daemon/internal/
  # workspace's provision.go), whose root is LEGION_WORKSPACE's three levels up on a pod: the head
  # label's checkout is often one of them, and a run needs nothing there.
  [ -z "${LEGION_WORKSPACE:-}" ] || workspaces=${LEGION_WORKSPACE%/*/*/*}
  printf 'cover %s\n' "$(realpath -m -- "$workspaces")"
  # The work directory, wherever it is: the other runs, live and archived, and the other label's
  # profile.
  printf 'cover %s\n' "$work"
  # Every label's checkout: the other label's skills are no part of this run's world.
  for file in "$work"/profiles/*/checkout; do [ ! -f "$file" ] || printf 'cover %s\n' "$(<"$file")"; done
  # The pod's credentials, by both their names: Legion's role secrets, tokens and private state,
  # and the cluster's service account. /var/run/model-gateway-key and /var/run/operator stay: they
  # are the model route's own credentials, which every production agent's Oh My Pi reads.
  for dir in /var/run/legion /var/run/secrets; do
    printf 'credentials %s\n' "$dir"
    [ "$(realpath -m -- "$dir")" = "$dir" ] || printf 'credentials %s\n' "$(realpath -m -- "$dir")"
  done
  # Shown again. The run's own directory: its record, HOME, TMPDIR, cwd and world.
  printf 'rw %s\n' "$R"
  # Hidden inside it: the services' logs (<run>/logs), the one directory of the rig's records,
  # under an empty read-only tmpfs once the run's directory is bound back (private_world), so an
  # agent reads none of it and writes none of it, and the host keeps it whole: the run's record of
  # its launches (<run>/logs/launches, launch's), which tells the guard a run's first launch from
  # a later one, is there for that. The rig's records that are files (built-from, services-from,
  # namespace.txt and the rest) cannot be hidden so: a mount onto a file is refused on a gVisor
  # pod ("mount point is not a directory"), so an agent can read them, and launch's guard reads
  # them as it reads every file of the run (run-names.ts's `worldFiles`), services-from less the
  # rig's own directory, which it names in every run.
  printf 'hide %s\n' "$R/logs"
  {
    # The label's install: its packed plugins and model route, which the HOME copied from it
    # names.
    printf 'ro %s\n' "$I"
    # The model route's call record and minted key, which its key command writes on every model
    # call (install_model_route's DIR/key and DIR/cache).
    printf 'rw %s\n' "$I/key" "$I/cache"
    # The batch's tokens: the Dispatch and Envoy tokens the pane names, and nothing else of the
    # batch's services, whose files name every label.
    printf 'ro %s\n' "$work/services/tokens"
    # A run recorded before the names left the world: its label's checkout, this script's own
    # directory, and zod, the one package its scripts import, where bun's resolution finds it
    # from that directory, the first node_modules/zod above it.
    if [ -n "$co" ]; then
      printf 'ro %s\n' "$co" "$here" "$work/services"
      dir=$here
      while [ "$dir" != / ] && [ ! -e "$dir/node_modules/zod" ]; do dir=$(dirname -- "$dir"); done
      [ ! -e "$dir/node_modules/zod" ] || printf 'ro %s\n' "${dir%/}/node_modules/zod"
    fi
    # Every tool directory on the pane's PATH but the run's own: the rig's own mise (rig_mise),
    # tmux, git and jj with the push guard's wrappers, bun, the operator's tools.
    while IFS= read -r -d : entry; do
      case $entry in /*) ;; *) continue ;; esac
      case $entry/ in "$R"/*) continue ;; esac
      entry=${entry%/}
      printf 'ro %s\n' "${entry:-/}"
    done < <(printf '%s:' "$(pane_var PATH "$pane")")
    # mise's installs, which `mise x <pin>` runs the agent from; read-only, so no run installs a
    # tool into another's world.
    entry=$(pane_var MISE_DATA_DIR "$pane")
    [ -z "$entry" ] || printf 'ro %s\n' "$entry"
    # The pinned Oh My Pi, wherever `mise where` put it.
    [ -z "$pin_dir" ] || printf 'ro %s\n' "$pin_dir"
    # The push guard's shared check (push_guard's PUBLIC_LEAK_CHECK), which a Legion pod keeps
    # outside the scratch root. In a world with a remote of its own the pane names the world's run
    # of it, in the run's own directory, and the check it runs is the one $R/lib/leak-check names.
    entry=$(pane_var PUBLIC_LEAK_CHECK "$pane")
    if [ "$entry" = "$R/lib/public-leak-check.sh" ] && [ -f "$R/lib/leak-check" ]; then entry=$(<"$R/lib/leak-check"); fi
    [ -z "$entry" ] || printf 'ro %s\n' "$(dirname -- "$entry")"
    # The system git configuration the pane's git reads (system_gitconfig), where the host has
    # one, shown as the host holds it and mounted by nothing (the `rw, ro` kinds above say why).
    entry=$(system_gitconfig "$(pane_var PATH "$pane")")
    [ -z "$entry" ] || [ ! -e "$entry" ] || printf 'ro %s\n' "$entry"
  } | while read -r kind entry; do
    # A shown path that is a link, or lies under one, is shown at what it resolves to as well, read
    # here before anything is covered: a link the cover leaves visible resolves there once its
    # target is bound back (zod's node_modules/zod is a link into the root's .bun store, which may
    # lie under a cover), and a link the cover hides is shown again at its own path.
    printf '%s %s\n' "$kind" "$entry"
    real=$(realpath -m -- "$entry")
    [ "$real" = "$entry" ] || printf '%s %s\n' "$kind" "$real"
  done | LC_ALL=C sort -k2 | uniq
}

# The script launch.sh runs as root of a user, mount and PID namespace of its own (`unshare -Urm
# -p --fork --mount-proc`, with /proc mounted afresh, so the run sees its own processes alone),
# under `env -i` with namespace_path alone, with this user's uid and gid, the run directory and its
# cwd as its arguments, then the agent's command. It reads $R/namespace.txt (world_paths) and:
#   1. mounts a tmpfs on /dev/shm, the run's own, and a second on /dev/shm/holder, its holder, and
#      binds each rw and ro directory into the holder, $R first (a file shown, the system git
#      configuration, is left where it is: world_paths). A path under the machine's /dev/shm, which
#      the first tmpfs now hides, is read by its path relative to the machine's /dev/shm, the
#      script's working directory from before the mount, as $R/namespace.txt is where $R lies
#      there: rig.test.ts's work directory is under /dev/shm (memoryRoot), as an operator's can be;
#   2. covers each cover and credentials path with an empty tmpfs;
#   3. binds each held path back onto its own path, read-only unless it is rw, $R first. A path a
#      link the covers left visible still resolves elsewhere is left as it is: world_paths declared
#      what it resolves to, which is bound back at that path, and the link reaches it there. The
#      mount point is made for the target, never for the link, whose `mkdir -p` answers `File
#      exists` once its target is covered. A shown path under /dev/shm that is a link on the
#      machine is made again in the run's /dev/shm, which hides the machine's, as a link to what
#      the machine's names: its target is shown too, and bound back at its own path;
#   4. binds $R/tmp onto /tmp, and the held paths under /tmp again, since that bind hides them,
#      then covers each hide directory with an empty read-only tmpfs, which $R's bind had shown
#      again;
#   5. drops the holder (`umount -l`, which takes its binds with it: a gVisor pod refuses to
#      unmount one bind of a path the namespace did not mount, `not mounted`) and removes its mount
#      point, so /dev/shm is the run's own tmpfs, empty but for each held path under /dev/shm,
#      bound back there in step 3, marks $R/namespace-ready, and starts the command in a second
#      user namespace that maps it back to this user's uid and gid.
# The order is the record: covering first and binding $R onto itself afterwards binds the new
# tmpfs's empty $R, and the agent writes every file of the run there and nowhere the host can see,
# exit 0. The agent keeps this user's uid because one running as uid 0 meets git's ownership
# checks and tools behaving as root, and `--map-current-user` cannot take a mount (`must be
# superuser to use mount`). Any command that fails stops the script before the agent starts, with
# that command's own message on out.txt and no namespace-ready. No network namespace: the
# stand-ins, NATS and Postgres are reached on the loopback as before.
private_world='uid=$1 gid=$2 R=$3 cwd=$4; shift 4
h=/dev/shm/holder
cd /dev/shm && mount -t tmpfs tmpfs /dev/shm && mkdir "$h" && mount -t tmpfs tmpfs "$h" || exit 1
machine() { case $1 in /dev/shm/*) printf "%s\n" "${1#/dev/shm/}" ;; *) printf "%s\n" "$1" ;; esac; }
cp "$(machine "$R/namespace.txt")" "$h/plan" && : >"$h/held" || exit 1
i=0
while read -r kind p; do
  case $kind in rw | ro) [ -d "$(machine "$p")" ] || continue ;; *) continue ;; esac
  i=$((i + 1))
  mkdir "$h/$i" && mount --bind "$(machine "$p")" "$h/$i" && printf "%s %s %s\n" "$i" "$kind" "$p" >>"$h/held" || exit 1
done <"$h/plan"
while read -r kind p; do
  case $kind in cover | credentials) [ ! -d "$p" ] || mount -t tmpfs tmpfs "$p" || exit 1 ;; esac
done <"$h/plan"
back() { [ "$(realpath -m -- "$3")" = "$3" ] || return 0; mkdir -p "$3" && mount --bind "$h/$1" "$3" && { [ "$2" = rw ] || mount -o remount,bind,ro "$3"; }; }
while read -r i kind p; do back "$i" "$kind" "$p" || exit 1; done <"$h/held"
while read -r kind p; do
  case $kind:$p in rw:/dev/shm/* | ro:/dev/shm/*) ;; *) continue ;; esac
  [ -L "$(machine "$p")" ] && [ ! -e "$p" ] && [ ! -L "$p" ] || continue
  mkdir -p "${p%/*}" && ln -s "$(readlink "$(machine "$p")")" "$p" || exit 1
done <"$h/plan"
mount --bind "$R/tmp" /tmp || exit 1
while read -r i kind p; do
  case $p/ in /tmp/*) back "$i" "$kind" "$p" || exit 1 ;; esac
done <"$h/held"
while read -r kind p; do
  case $kind in hide) [ ! -d "$p" ] || mount -t tmpfs -o ro tmpfs "$p" || exit 1 ;; esac
done <"$h/plan"
umount -l "$h" && rmdir "$h" && : >"$R/namespace-ready" && cd "$cwd" || exit 1
exec unshare -U --map-user="$uid" --map-group="$gid" "$@"
'

# namespace_probe makes, under the same unshare, `env -i` and flags as launch.sh, a namespace that
# mounts a tmpfs, binds a directory of it and remounts that read-only, and maps this user back:
# each step private_world takes. It prints nothing and succeeds where the host allows that, and
# prints the host's refusal and fails where it does not (a GitHub runner answers
# `unshare: write failed /proc/self/uid_map: Operation not permitted`). private_world mounts
# nothing onto a file, which a gVisor pod refuses (`mount point is not a directory`), so the probe
# mounts none either. The unshare is the one on this script's PATH, as launch.sh's is.
namespace_probe() {
  local unshare out
  unshare=$(command -v unshare) || {
    echo "no unshare on PATH"
    return 1
  }
  # shellcheck disable=SC2016 # expanded by the namespace's sh, not here
  out=$(env -i PATH="$namespace_path" "$unshare" -Urm -p --fork --mount-proc sh -c 'mount -t tmpfs tmpfs /dev/shm && mkdir /dev/shm/held && mount --bind /dev/shm/held /dev/shm/held && mount -o remount,bind,ro /dev/shm/held && exec unshare -U --map-user="$1" --map-group="$2" true' - "$(id -u)" "$(id -g)" 2>&1) && return 0
  printf '%s\n' "${out:-$unshare exited $? and printed nothing}"
  return 1
}

# require_namespace refuses a run, a batch or a live read before anything of it starts, on a host
# that refuses the namespace every agent runs in, naming the host's refusal. There is no fallback:
# a run outside its namespace sees every other run, the operator's scratch and the pod's
# credentials.
require_namespace() {
  local why
  why=$(namespace_probe) || fail "this host refuses the namespace every agent of a run starts in (unshare -Urm -p --fork --mount-proc), so nothing starts: $why"
}

# tmux_ready refuses a run or a batch before it makes any database, service or run directory,
# where the tmux on PATH cannot start the detached session every agent starts in (launch): a
# host can carry no tmux, and a tmux binary that cannot load libevent_core-2.1.so.7 starts no
# session either, which a batch met only mid-run, once its databases and services were up. It
# runs `tmux -V`, then starts one detached session as launch starts an agent's and kills
# it, on a socket of its own in a directory it makes under the work directory and removes, never
# the work directory's tmux server (tmux_socket), so it leaves nothing a run reads. A step that
# fails is refused, naming tmux, the step and what tmux printed on stderr, and a tmux on PATH that
# does not start is refused as a missing one is. It sets RIG_TMUX_CHECKED to the tmux it checked,
# by path: a batch exports it to its runs, each of which takes the batch's PATH and so that tmux,
# and a run its batch checked is not checked again (cmd_batch, cmd_run).
tmux_ready() {
  local path tmp step=-V err status=0
  path=$(type -P tmux) ||
    fail "tmux -V: no tmux on PATH; every run's agent starts in a detached tmux session (launch), so nothing starts"
  err=$("$path" -V 2>&1 >/dev/null) || status=$?
  if [ "$status" = 0 ]; then
    tmp=$(mktemp -d "$work/.tmux.XXXXXX")
    step=new-session
    err=$("$path" -S "$tmp/s" new-session -d -s ready -x 200 -y 50 'sleep 60' 2>&1 >/dev/null) || status=$?
    if [ "$status" = 0 ]; then
      step=kill-session
      err=$("$path" -S "$tmp/s" kill-session -t =ready 2>&1 >/dev/null) || status=$?
    fi
    [ "$status" = 0 ] || "$path" -S "$tmp/s" kill-server >/dev/null 2>&1 || true
    rm -rf -- "$tmp"
  fi
  [ "$status" = 0 ] ||
    fail "tmux $step: $path exited $status: ${err:-no message}; every run's agent starts in a detached tmux session (launch), so nothing starts"
  RIG_TMUX_CHECKED=$path
}

# launch NAME [OMP_ARG]... runs `omp -p "$(cat $R/prompt.txt)"` and then each OMP_ARG in $R/cwd
# under $R/pane.env, followed by $R/system-args (one --append-system-prompt argument, as shell text)
# when it exists, as the tmux session <tag>-<name>. It waits for the session to exit or time out. A
# timeout is the run's result (`exit=timeout`), not a rig failure, so this wait is not an
# until_true. A spec run's later turn passes --continue. Each launch starts out.txt again, so its
# wait reads only its own exit line. The session's transcript goes to $sessions_dir, which is
# $R/sessions for the run's own agent and a directory of its own for each later one (worker_boot).
#
# The agent and every process it starts run in a world of the run's own (world_paths,
# private_world): the scratch root, the issue workspaces, the work directory, every label's
# checkout and the pod's credentials are each an empty tmpfs, with the run's own $R, its label's
# install, the batch's tokens and the tools the pane names shown again; $R/tmp is /tmp, and /proc
# holds the run's own processes alone. So `ls` of the runs directory shows this run alone, no
# other run's directory, live or archived, no other role's scratch, no other label's checkout and
# no credential under /var/run/legion can be read, and nothing a run writes to /tmp reaches the
# machine's /tmp or another run: it stays under $R as the run's record. The 2026-11-14 agents read
# other runs' pointer files under the runs directory and wrote /tmp/LWEVAL-1-review.json, a name
# every run's agent picks, into the /tmp every run shared. An agent that does not start in this
# namespace fails the run, naming the host's refusal: there is no fallback to the shared world. A
# path the run's world shows that holds a path it hides is refused before anything starts, since
# showing it would show all it holds. scratch_clean and health.ts's rules stay behind this as a
# second line.
#
# Nothing the rig composes for the agent names the run's scenario, case or label, or a name the
# rig gave its world: before the agent starts, `run-names.ts guard` reads every variable of its
# pane, every mount of its namespace, its working and session directories, its system prompt and
# everything of the run's directory the rig wrote, by its path and its text, the files it builds
# (the gh bundle) included, for a word of the vocabulary (run-names.ts's `worldNames`, through its
# one walker of a run's files, `worldFiles`), and the run fails, naming the word and where it is,
# where one is there. It reads nothing whose words are an agent's own (run-names.ts's
# AGENTS_OWN, each with why): the agents' transcripts, output and logs, an earlier agent's HOME
# and TMPDIR, the repositories' version-control records, and a message one agent of the run sent
# the next that the rig relays into the next one's prompt (run_threads_closed, which keeps each
# whole in $R/reviewer-wake.json): the message's words are its sender's, so the guard reads the
# prompt less each message, at the place the rig wrote it, and the rest, each line's own prefix
# included, is the rig's (run-names.ts's `promptOf`); a reviewer-wake.json it cannot parse fails
# the run. On 2026-11-16, 16 of the 36 runs of the batch at c273084d reasoned from the run's
# name, which every path of the run carried.
#
# It reads what the rig wrote for the run, never what the label's checkout or the host provides
# (the architect's ruling of 2026-11-21T17:22:22Z). Of the HOME base_env copies from the label's
# install it reads the files cmd_profile writes there alone (run-names.ts's RIG_HOME, each with
# its write: the profile's settings and model route, envoy.json, the superpowers skills and
# make_omp_home's jj overlay), and the label's packed plugins there are the label's
# (run-names.ts's LABELS_OWN). It enters no link (lstat): it reads the path a link names, and a
# link that names a path inside the run is read no further, since the walker reads that path as
# itself; one into the label's install ($I, passed as the guard's fourth argument) or into the
# host's native module cache or git exec path (run-names.ts's `hostProvided`: make_omp_home's
# natives, the `git --exec-path` world_remote links $R/lib/git-core into) is not read; and one
# anywhere else fails the run, naming the link. It holds one file's text at a time. INFRA-764's
# round 31 at c42409bc: the walker went through the HOME's links into the label's plugins, refused
# every run on a scenario's name in pi-envoy's dist/dispatch.js, and, read whole with the host's
# natives (124 MiB), the guard reached 1,170 MiB and the pod's memory guard killed it twice.
#
# What an agent of the run can write is read at the run's first launch alone (run-names.ts's
# AGENT_WRITTEN, each with the write that reaches it): its working directory, the issue workspace
# and its clone, its HOME and its pane's XDG directories, the Legion plugin's files in the state
# directory, and the gh answers the gh stand-in rewrites on an agent's body edit (fixtures.json).
# Before the first launch they hold only what the rig wrote, and the guard reads them; at every
# later launch (a spec run's later turn, threads-closed's reviewer, boot_agent's relaunch) they
# hold what an agent wrote too, whose words are that agent's own (the architect's ruling of
# 2026-11-21T14:24:55Z), and the guard reads every other file of the run. First is told from later
# by the run's record of its launches, $R/logs/launches, a line `<time> <tmux session>` that this
# function appends once the guard has passed and before the agent starts: <run>/logs is under the
# empty read-only tmpfs every agent's namespace covers it with (world_paths' `hide`), which a
# namespace of the agent's own cannot take off either, so no agent can write the record, and a run
# with no line in it is at its first launch (run-names.ts's `firstLaunch`). The rig writes nothing
# into those paths after the first launch: rig.test.ts's "after a run's first launch the rig writes
# nothing into what an agent of the run can write" composes every later launch the scenarios
# reach and holds each path to the bytes its first launch read.
#
# Under SKILL_SCENARIOS_COMPOSE_ONLY the run ends once the guard has read its world, exit 0, its
# agent never started and no launch on its record: everything the rig writes, copies and builds
# into the run before its first agent is there, and nothing of an agent's. rig.test.ts's "no file
# rig.sh writes, copies or builds into a run before its agent starts names a word of the
# measurement" composes every scenario's run so, as cmd_run composes it, each label on a real
# install (make_omp_home, install-plugin-profile.sh, install_model_route), its links included, and
# reads every file of it through the guard's walker and every run with the guard itself, holding
# the guard's peak memory under a bound: at 9083afb5 the gh bundle carried the route table's
# evidence, a scenario's name in a file that no row had built, and the guard refused every
# reviewer of INFRA-764's round 30; at c42409bc the row's labels had a HOME of one file, so no row
# met the plugins' links that refused every run of round 31.
launch() {
  local system='' extra='' omp unshare kind path other hidden
  omp=$(pinned_omp "$pin") || exit 1
  unshare=$(command -v unshare) || fail "no unshare on PATH: every agent runs in a namespace of its own"
  session=$tag-$1
  shift
  [ $# = 0 ] || extra=" $(printf '%q ' "$@")" extra=${extra% }
  [ ! -f "$R/system-args" ] || system=" $(<"$R/system-args")"
  mkdir -p "$R/tmp" "$R/logs" "$work/runs" "$work/archive"
  world_paths "$R" "$work" "$I" "$R/pane.env" "${omp%/bin/omp}" >"$R/namespace.txt"
  while read -r kind path; do
    case $kind in rw | ro) ;; *) continue ;; esac
    while read -r other hidden; do
      case $other in cover | credentials) ;; *) continue ;; esac
      case $hidden in "${path%/}"/?*) fail "launch: the run's world shows $path ($kind), which holds $hidden, a path it hides (world_paths)" ;; esac
    done <"$R/namespace.txt"
  done <"$R/namespace.txt"
  bun "$here/run-names.ts" guard "$R" "${sessions_dir:-$R/sessions}" "$work" "$I" ||
    fail "launch: the world of ${R##*/} names the run or the rig (above), so its agent does not start"
  if [ -n "${SKILL_SCENARIOS_COMPOSE_ONLY:-}" ]; then
    note "${R##*/}: composed, and the guard read its world; SKILL_SCENARIOS_COMPOSE_ONLY starts no agent"
    exit 0
  fi
  printf '%s %s\n' "$(date -u +%FT%T.%3NZ)" "$session" >>"$R/logs/launches"
  rm -f "$R/namespace-ready"
  cat >"$R/launch.sh" <<EOF
#!/usr/bin/env bash
cd $(printf %q "$R/cwd")
mapfile -t vars <$(printf %q "$R/pane.env")
exec env -i PATH=$namespace_path $(printf %q "$unshare") -Urm -p --fork --mount-proc sh -c $(printf %q "$private_world") - $(id -u) $(id -g) $(printf %q "$R") $(printf %q "$R/cwd") env -i "\${vars[@]}" mise x $(printf %q "$pin") -- $(printf %q "$omp") -p "\$(cat $(printf %q "$R/prompt.txt"))"$extra$system --session-dir $(printf %q "${sessions_dir:-$R/sessions}") >$(printf %q "$R/out.txt") 2>&1
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
  [ -e "$R/namespace-ready" ] ||
    fail "the agent of ${R##*/} did not start: its namespace could not be made: $(grep -v '^exit=' "$R/out.txt" | head -n 3 | tr '\n' ' ')"
}

# run_env_pids DIR prints `PID START` (stat field 22) for each process whose environment's HOME is
# DIR or a path under it: an agent gets its HOME, under its run's directory, from its pane file, and
# every command it runs inherits it. It finds what stop_tree cannot reach from the pids a command
# recorded: a process that left their tree, such as a `setsid -f` child. A process that merely works
# under DIR, such as a developer's shell watching a run, has a HOME of its own, and another user's
# environment cannot be read. It is procs_naming (scripts/e2e/lib/process-tree.sh), which hands every
# /proc/<pid>/environ to grep through xargs in calls that fit ARG_MAX, reads each match whole, and
# prints why and fails with 2 when the machine's environments could not be read.
#
# An agent and what it starts run in a PID namespace of their own (launch), where each has a pid of
# that namespace's. This shell reads its own /proc, of the PID namespace the rig runs in, which
# lists every process of the agent's namespace too, under the pid and start time this shell's
# namespace gives it: the pids printed here, and every pid kill_held then reads, freezes and kills,
# are this shell's own. No pid is ever read from inside the namespace (its namespace-ready is an
# empty file), so none of the namespace's own pids, which name other processes or none here, can
# reach a signal.
# shellcheck disable=SC2329 # stop_env_processes, which the exit traps run, runs it
run_env_pids() { procs_naming "$1" HOME; }

# stop_env_processes DIR ends each process run_env_pids DIR finds and everything under it, a child
# that dropped its HOME included, through kill_held (scripts/e2e/lib/process-tree.sh), the run's
# sweep's own loop: each is frozen and read again, ended only while it is still the process the walk
# read (its start time) and its HOME is still DIR or under it, and then ended with its tree, its own
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
  kill_held "$1" HOME <<<"$pairs" ||
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

# A run's or a live read's exit: its daemon stand-in, its own Dispatch server, its agent's session,
# and every process whose HOME is under the run's directory; then the run's directory goes to the
# archive with its record (archive_run). The run's database stays in the batch's Postgres: services_down removes
# the container it started, and leaves a given server, with every database its runs made, to the
# operator. The two services are this shell's own jobs, so each is waited for once it is killed:
# bash otherwise prints a `Killed` notice for it on stderr.
# shellcheck disable=SC2329 # on_exit's trap runs it
stop_run() {
  local pid
  # The recorder first, and with TERM rather than stop_tree's STOP-then-KILL: it writes the
  # closing line of `<run>/published.jsonl` on TERM, and that line is what tells a reader the
  # record is whole (record-publishes.ts, score.ts's `unscored`). stop_pid gives it ten seconds
  # and kills it after, so a recorder that will not stop leaves an unfinished record rather than
  # holding the run open.
  if [ -n "$record_pid" ]; then
    stop_pid "$record_pid" && wait "$record_pid" 2>/dev/null || true
    record_pid=''
  fi
  # The head watcher with TERM, as the recorder: it finishes the push in hand and reads the push
  # log once more before it exits (head-watcher.ts), so its record answers every push the run made.
  if [ -n "$watcher_pid" ]; then
    stop_pid "$watcher_pid" && wait "$watcher_pid" 2>/dev/null || true
    watcher_pid=''
  fi
  for pid in "$daemon_pid" "$dispatch_pid" "$github_pid"; do
    [ -z "$pid" ] || { stop_tree "$pid" && wait "$pid" 2>/dev/null || true; }
  done
  [ -z "$session" ] || stop_panes "$session"
  [ -z "$R" ] || stop_env_processes "$R"
  [ -z "$session" ] || tmux kill-session -t "=$session" 2>/dev/null || true
  scratch_clean
  archive_run
}

# The directories every run on this machine shares and an agent writes scratch files into by a
# name of its own choosing: the machine's /tmp, where the 2026-11-14 agents, each on issue
# LWEVAL-1, wrote /tmp/LWEVAL-1-review.json, a name the next run's agent picks too. A run's own
# TMPDIR is inside it ($R/tmp, base_env). The caller's TMPDIR is left out on purpose: on a Legion
# pod it is the scratch every role of the issue shares, and deleting what appeared there would
# delete their work. One agent of that batch also wrote /legion/tmp by climbing out of its run
# (`ws/../../../../../../tmp`); a path like that, one run in a batch reaches by accident, is
# health.ts's to catch, not a directory to empty.
shared_scratch=(/tmp)

# scratch_before records the entries this user owns at the top of each shared_scratch directory
# when the run starts, to $R/logs/scratch-before.txt, so scratch_clean can tell what the run made.
# The record names what anything else on the machine left in /tmp, other test suites' scratch
# among it, so it goes in the run's logs, which the agent's world hides (world_paths' `hide`) and
# launch's guard does not read as the agent's world: at the top of the run it was both, and on a
# host whose /tmp held a suite's `skill-scenarios-*` directory the guard refused every launch.
scratch_before() {
  local dir
  for dir in "${shared_scratch[@]}"; do
    find "$dir" -mindepth 1 -maxdepth 1 -user "$(id -u)" -printf '%p\n' 2>/dev/null || true
  done >"$R/logs/scratch-before.txt"
}

# scratch_clean, at a run's end, records each entry this user owns at the top of a shared_scratch
# directory that was not there when the run started (scratch_before), with its modification time,
# to $R/logs/scratch-created.txt (`<epoch seconds> <path>`, one a line), beside scratch_before's
# record and hidden with it, since it names the machine's /tmp too; health.ts reads each line as a
# write of the run. It deletes nothing: an entry that appeared during the run may be anything else
# this user ran on the machine meanwhile, not the run's, and the run's agent never writes the
# machine's /tmp, whose place its namespace gives $R/tmp (launch, private_world). This holds for
# serial runs alone (SKILL_SCENARIOS_PARALLEL=1): with runs side by side, what appeared during one
# run is every concurrent run's too, so the record would name another run's files as this one's.
# At any other parallelism this records nothing and says so.
# shellcheck disable=SC2329 # stop_run, an exit trap, runs it
scratch_clean() {
  [ -n "${R:-}" ] && [ -f "$R/logs/scratch-before.txt" ] || return 0
  if [ "${SKILL_SCENARIOS_PARALLEL:-5}" != 1 ]; then
    note "${R##*/}: recorded nothing of ${shared_scratch[*]}: only serial runs (SKILL_SCENARIOS_PARALLEL=1) record what a run made there"
    return 0
  fi
  local dir line
  : >"$R/logs/scratch-created.txt"
  for dir in "${shared_scratch[@]}"; do
    find "$dir" -mindepth 1 -maxdepth 1 -user "$(id -u)" -printf '%T@ %p\n' 2>/dev/null || true
  done | while IFS= read -r line; do
    ! grep -qxF -- "${line#* }" "$R/logs/scratch-before.txt" || continue
    printf '%s\n' "$line" >>"$R/logs/scratch-created.txt"
  done
}

# archive_run moves the directory of a run that has ended, and whose processes are stopped, out
# of $work/runs, where every run of a batch is made, to $work/archive/runs/<run>, and its record
# (<run>.json, cmd_run) beside it. An archived run of that id is an earlier run's evidence:
# archive_run refuses it, naming the run and the path, and exits 1, removing and replacing
# nothing, so the archived run stays as it was and this run stays in $work/runs. cmd_run refuses
# such a run before it starts, so this is the last line behind it. An agent sees neither
# directory but its own run (launch's private_world); outside that namespace the same Unix user
# reaches both, so a run's directory keeps out of the runs/ every later run is made in, and
# health.ts's rule against naming another run's directory, the archive's included, stays the
# check behind them. `rig.sh score` reads the archive, and score.ts and judge.ts read an archived
# run's work directory as this one (score.ts's `workOfRuns`). A run directory outside $work/runs
# (a live read's) stays where it is.
# shellcheck disable=SC2329 # stop_run, an exit trap, runs it
archive_run() {
  case ${R:-} in "$work/runs/"?*) ;; *) return 0 ;; esac
  [ -d "$R" ] || return 0
  local to=$work/archive/runs/${R##*/}
  mkdir -p "$work/archive/runs"
  [ ! -e "$to" ] && [ ! -L "$to" ] && [ ! -e "$to.json" ] && [ ! -L "$to.json" ] ||
    fail "archive_run: run ${R##*/} is already archived at $to; neither is removed or replaced, and this run stays at $R"
  # -n and the check after it: a directory that appeared at $to since the check above is skipped,
  # never replaced, and the run that could not move is refused as above. The record follows the
  # directory, so a run whose directory could not move keeps its record beside it.
  mv -nT -- "$R" "$to" || true
  [ ! -e "$R" ] ||
    fail "archive_run: run ${R##*/} could not move to $to; nothing there is removed or replaced, and this run stays at $R"
  [ ! -e "$R.json" ] || mv -nT -- "$R.json" "$to.json" || true
  [ ! -e "$R.json" ] ||
    fail "archive_run: the record of run ${R##*/} could not move to $to.json; it stays at $R.json"
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
  require_namespace
  # launch's pinned_omp and the agent's `mise x` run the rig's own mise (rig_mise).
  rig_mise
  shift
  local paths=() skill list
  # The label's install id names the read, so no path its agent reads names the label.
  R=$work/live-read/${I##*/}
  # An earlier live read of the label is that read's record: refused, naming it, never replaced,
  # before the exit handler (stop_run) is set on a directory this read did not make.
  mkdir -p "$work/live-read"
  mkdir -- "$R" ||
    fail "live-read: $R could not be made (mkdir says why above); an earlier live read of label ${P##*/} is never replaced: move it aside to read again"
  on_exit stop_run
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
  launch "live-read-${I##*/}"
  bun "$here/score.ts" live-read "$R" "$co/skills" "$R/home/.omp/profiles/$profile/logs"
}

# The services file of LABEL's runs, which services_up writes for each label of its batch: that
# label's own Envoy listener (ENVOY_URL), the batch's NATS and the listener token, the lines a
# worker run's pane carries (worker_boot) and its recorder reads (start_recorder). cmd_run points
# services_env at its label's.
label_services() { printf '%s\n' "$work/services/env-$1"; }
# The service provenance of LABEL's runs (services_from), which services_up writes beside the
# services file and cmd_run copies into each run as <run>/services-from.
label_provenance() { printf '%s\n' "$work/services/services-from-$1"; }
# The Postgres SKILL_SCENARIOS_POSTGRES_URL names, written by services_up and read by own_dispatch,
# which runs in a process of its own (`rig.sh run`): a run takes the Postgres its batch brought up,
# never one its own environment names. The file is 0600, since the URL can carry a password, and
# its absence is how a run knows its batch started a container instead.
services_postgres=$work/services/postgres-url
# The batch's Envoy and Dispatch tokens, which services_up writes and every run's pane names: a
# directory of their own, the one part of the batch's services a run's world shows (world_paths),
# since the services files beside it name every label.
tokens=$work/services/tokens
# The two images a batch starts when it is given no servers, by digest (services_up), which each
# run's provenance names (services_from): ghcr.io/cybermonklabs/legion/ci's copy of
# pgvector/pgvector (.github/ci-images.yaml), and the hub's nats-server (read_nats_image).
postgres_image=ghcr.io/cybermonklabs/legion/ci@sha256:2358fcba361ed2233a5ed81b5fe4ca779ccb304120ce531a3bf51c0ed7e2bc11
# read_nats_image sets nats_image to the hub's nats-server, which scripts/e2e/lib/nats-image.sh
# reads from testnats.HubImage: the listener and the daemon refuse a nats-server below the floor
# packages/envoy/natsversion holds, every 2.10 among them. Only what starts that image or names it
# reads it, services_up's container and services_from's line for it: sourced at the top of this
# script, it refused every command, judge-omp's among them, on a root with no
# packages/envoy/internal/testnats/nats.go before that command reached its own checks.
read_nats_image() {
  # shellcheck source-path=SCRIPTDIR/../../../.. source=scripts/e2e/lib/nats-image.sh
  . "$root/scripts/e2e/lib/nats-image.sh"
}

# build_dir CHECKOUT prints where the Go binaries built from CHECKOUT are kept:
# $work/builds/<the first 16 hex digits of the digest of CHECKOUT's path>, beside the daemon facts
# daemon-pane.ts keeps under $work/daemon-facts, and kept, as they are, from one batch to the next.
build_dir() { printf '%s/builds/%s\n' "$work" "$(printf '%s' "$1" | sha256sum | cut -c1-16)"; }

# go_binary CHECKOUT MODULE PACKAGE NAME [FLAG...] builds the command PACKAGE (./cmd/listener,
# ./cmd/dispatch, ./cmd/daemon-pane) of CHECKOUT's own packages/MODULE module (envoy, daemon), in
# that module and under its own go.work, with each FLAG, as NAME in CHECKOUT's build directory
# (build_dir), and prints the binary's path. One binary per checkout, kept across batches: `go
# build -o` relinks only an output whose sources changed, so each binary is the build of its
# checkout's tree as it stands, and a second batch of an unchanged tree builds nothing. One batch
# at a time holds the work directory (lock_services), so no two builds of one binary race.
go_binary() {
  local dir
  dir=$(build_dir "$1")
  mkdir -p "$dir"
  (cd "$1/packages/$2" && go build -o "$dir/$4" "${@:5}" "$3") >&2 || return 1
  printf '%s\n' "$dir/$4"
}

# daemon_pane_binary CHECKOUT builds this directory's daemon-pane.go in CHECKOUT's daemon module
# (go_binary), as daemon-pane.ts's runDaemonPane runs it with `go run`: the program in place of
# <module>/cmd/daemon-pane/main.go, through an overlay kept beside the binary. It prints the
# binary's path. Each process that reads a label's daemon runs its label's build
# (SKILL_SCENARIOS_DAEMON_PANE: label_facts, worker_boot), so a read takes milliseconds where a
# `go run` compiles the daemon module first: 6.2-7.4 s a read with a warm Go build cache and 282 s
# with a cold one, measured on 2026-11-15.
daemon_pane_binary() {
  local overlay
  overlay=$(build_dir "$1")/daemon-pane-overlay.json
  mkdir -p "${overlay%/*}"
  jq -n --arg main "$1/packages/daemon/cmd/daemon-pane/main.go" --arg program "$here/daemon-pane.go" \
    '{Replace: {($main): $program}}' >"$overlay"
  go_binary "$1" daemon ./cmd/daemon-pane daemon-pane -overlay "$overlay"
}

# The daemon stand-ins the scenarios boot (worker_boot), each `<role>` when started in the role's
# own phase and `<role>:<phase>` when started in another: tester-proof's tester,
# reviewer-text-findings' reviewer, and threads-closed's implementer and reviewer, in `reviewing`
# (accept, correct and crossing) or `accepting` (code-defect). label_facts reads every fact each
# one's boot reads.
worker_boots=(tester reviewer implementer:reviewing implementer:accepting reviewer:reviewing reviewer:accepting)

# label_facts LABEL BINARY reads, through BINARY (daemon_pane_binary of LABEL's checkout, which
# load_label loaded last), every fact of that checkout's daemon a stand-in of worker_boots reads
# at its boot (daemon-pane.ts's `facts`, its standinFacts), and the selector table of that
# checkout's gh wrapper the gh stand-in refuses an owner by (its labelGhSelectors, which
# worker_boot writes into each run), into the facts cache under $work, before any run of the batch
# starts, so each boot reads its facts out of files. A boot that read them itself ran
# daemon-pane.go once a fact: in the preflight of 2026-11-15 at 7bf20811, in a fresh work
# directory, a first boot took ~43 s (head) and ~36 s (base) with a warm Go build cache, past
# worker_boot's 30 s bound for the stand-in to listen, and 6 of the 8 runs ended there. A fact that
# cannot be read fails the batch here, naming it, where no run has started; so does a selector
# form the label's wrapper holds that the world's gh does not model.
label_facts() {
  SKILL_SCENARIOS_WORK=$work SKILL_SCENARIOS_DAEMON_PANE=$2 bun "$here/daemon-pane.ts" facts "$co" "${worker_boots[@]}" ||
    fail "a fact of the daemon of label $1's checkout $co could not be read (above), so no daemon stand-in of the label could boot"
}

# label_contract LABEL holds the state the daemon stand-in serves to LABEL's own contract: the
# `LegionStateResponse` of the checkout load_label loaded last (packages/contracts/src/
# legion-api.ts), the schema the plugin that label's agents load reads every state answer with.
# standin-record.ts's `contract` hands that schema every kind of body a stand-in of the rig can
# serve (its sampleFacts: each phase a scenario boots in or the round moves to, each claim state,
# each merge state and decision of the pull request, each state of the design gate) and names the
# first it refuses, with the schema's own error. A label's agents load the plugin of their own
# checkout, so a body one label's contract describes and the other's does not breaks one arm
# alone, in its agents' own turns: every state read of all 30 base runs of 2026-11-09 was
# refused `a body the daemon API contract does not describe` (standin-record.ts's `settings`
# line). A label whose contract does not describe the state fails the batch here, naming the
# label and the error, before that label's listener or any run starts; so does a checkout whose
# contract cannot be read.
label_contract() {
  bun "$here/standin-record.ts" contract "$1" "$co" ||
    fail "label $1's own contract ($co/packages/contracts/src/legion-api.ts) does not describe the state the daemon stand-in serves (above), so every read_record of that label's agents would be refused"
}

# start_listener LABEL BINARY serves LABEL's Envoy listener, BINARY (go_binary of the label's own
# checkout), on a port the kernel chose, held from that choice until the listener serves on it
# (lib/rig.sh's start_held), and sets envoy_port. Each label's listener is a service of its own,
# listener_<label> (its log $S/logs/listener_<label>.log and its port file
# $work/listener_<label>.port, the label's hyphens as underscores, which no label holds), its pid
# in listener_pids, under a machine id of its own: a listener's durable NATS consumer is named for
# its machine (bus.ListenerDurable), and two listeners under one id on the batch's one NATS would
# take each other's deliveries, which no two machines of production do. The machine id is the tag
# and the label's install id (load_label's I), since an agent's `envoy_whoami` answers it: it
# named the label.
start_listener() {
  local name=listener_${1//-/_} pid
  ENVOY_API_TOKEN="$(<"$tokens/envoy-token")" ENVOY_LISTEN_HOST=127.0.0.1 \
    ENVOY_MACHINE_ID="$tag-${I##*/}" NATS_URLS="$nats_url" \
    start_held "$name" envoy_port ENVOY_LISTEN_FD --env 'PORT={port}' -- "$2"
  pid=${name}_pid
  listener_pids+=("${!pid}")
  await_start "$name" "${!pid}" 60 "the $1 label's Envoy listener to answer /v1/sessions" listener_answers
}

# listener_answers reads /v1/sessions of the Envoy listener start_listener started last, on
# envoy_port, with the rig's bearer, which reaches curl on standard input from printf, a shell
# builtin, so it is in no command line.
# shellcheck disable=SC2329 # await_start in start_listener runs it
listener_answers() {
  printf 'Authorization: Bearer %s' "$(<"$tokens/envoy-token")" |
    curl -fsS -o /dev/null -H @- "http://127.0.0.1:$envoy_port/v1/sessions"
}

# file_digest FILE prints `sha256:<hex>` of FILE's bytes, or `missing` when there is no such file.
file_digest() {
  [ -f "$1" ] || {
    echo missing
    return 0
  }
  printf 'sha256:%s\n' "$(sha256sum <"$1" | cut -d' ' -f1)"
}

# services_from LISTENER DISPATCH PANE prints the service provenance of the runs of the label
# load_label loaded last (co, pin, P, profile): one `<service> <scope> <from> <digest>` a line,
# for every service and program a run of that label is built or started from. services_up writes
# it for each label before any run starts, and cmd_run copies it into the run as
# <run>/services-from, beside its built-from, before any agent of the run starts: health.ts's
# "service provenance" rule reads it against that built-from. <scope> is
#   label   a service the run's own label's checkout gives it, <from> being that checkout, which
#           health holds to built-from's `checkout` line: its Envoy listener (LISTENER) and its
#           Dispatch server (DISPATCH), each built from it, its packed plugins and its model
#           route (cmd_profile), the daemon facts the daemon stand-in answers by (daemon-pane.ts,
#           read out of built-from's checkout) and its worker pane, which that checkout's daemon
#           module composes (worker-pane.ts), both read through PANE, daemon-pane.go built in that
#           module (daemon_pane_binary);
#   pin     its Oh My Pi, the version that checkout pins (.omp-pin), <from> being the pin;
#   shared  a service production shares across versions, or this rig's own instrument, <from>
#           being where it comes from: each program of this directory a run runs, by name in
#           byte order, then NATS, Postgres, the push guard's check and the system git
#           configuration. The programs are read off this script's own text and their imports
#           (run-programs.ts, which says which): every one this script's functions name as
#           $here/<name>.ts or $here/<name>.go, each module of the directory one of them imports,
#           and every Go program of the directory, which daemon-pane.ts hands to `go run`. No
#           list names them, so a program this script starts to run is recorded with no edit
#           here; the list this kept by hand left out daemon-pane.ts (label_facts, worker_boot),
#           score.ts (reviewer_wake), completion-answers.ts (skip_reason) and judge.ts
#           (judge_ts), and every module of the directory those import but github-routes.ts. The
#           system git configuration (`system-gitconfig`) is the file the git of this PATH names
#           (system_gitconfig), the PATH every pane of the batch's runs extends, which each run's
#           world shows as the host holds it (world_paths): <from> is `none` where that git names
#           none, and <digest> is `none` where the host has no such file. A batch's runs share
#           one pod, so each label's provenance records the same one, and health.ts flags a batch
#           whose runs do not (its rule 23).
# <digest> is file_digest of the binary or program file a run starts (`missing` where that file is
# not there), or `-` where it starts none. The README's skill-scenarios section
# (packages/pi-envoy/scripts/README.md) holds the table of every service, with the reason each
# shared one is shared.
services_from() {
  local listener=$1 dispatch=$2 pane=$3 omp programs program gitconfig
  omp=$(pinned_omp "$pin") || exit 1
  programs=$(bun "$here/run-programs.ts" "$here/rig.sh") ||
    fail "services_from: the programs a run of $here/rig.sh runs could not be read (above), so no run's provenance can name them"
  printf 'envoy-listener label %s %s\n' "$co" "$(file_digest "$listener")"
  printf 'dispatch label %s %s\n' "$co" "$(file_digest "$dispatch")"
  printf 'plugins label %s -\n' "$co"
  printf 'model-route label %s %s\n' "$co" "$(file_digest "$I/home/.omp/profiles/$profile/agent/models.yml")"
  printf 'daemon-facts label %s %s\n' "$co" "$(file_digest "$pane")"
  printf 'worker-pane label %s %s\n' "$co" "$(file_digest "$pane")"
  printf 'omp pin %s %s\n' "$pin" "$(file_digest "$omp")"
  while IFS= read -r program; do
    printf '%s shared %s %s\n' "$program" "$here" "$(file_digest "$here/$program")"
  done <<<"$programs"
  if given_services; then
    printf 'nats shared %s -\n' "$(safe_url "$nats_url")"
    printf 'postgres shared %s -\n' "$(safe_url "$pg_url")"
  else
    read_nats_image
    printf 'nats shared %s sha256:%s\n' "${nats_image%@*}" "${nats_image#*@sha256:}"
    printf 'postgres shared %s sha256:%s\n' "${postgres_image%@*}" "${postgres_image#*@sha256:}"
  fi
  if [ -n "${PUBLIC_LEAK_CHECK:-}" ]; then
    printf 'leak-check shared %s %s\n' "$PUBLIC_LEAK_CHECK" "$(file_digest "$PUBLIC_LEAK_CHECK")"
  else
    printf 'leak-check shared none -\n'
  fi
  gitconfig=$(system_gitconfig "$PATH")
  if [ -z "$gitconfig" ]; then
    printf 'system-gitconfig shared none none\n'
  elif [ -f "$gitconfig" ]; then
    printf 'system-gitconfig shared %s %s\n' "$gitconfig" "$(file_digest "$gitconfig")"
  else
    printf 'system-gitconfig shared %s none\n' "$gitconfig"
  fi
}

# start_dispatch DATABASE_URL starts the e2e harness's Dispatch server on that database. The
# harness's fake Envoy, which run-server.sh names and this rig does not run, is port 1, where
# nothing listens; its broker switch is empty, so the server has no secrets broker. Its GitHub is
# the run's own GitHub stand-in (github-standin.ts), started here before the server: production's
# Dispatch reads GitHub through its App to say where a result's commit stands, and with GitHub at
# port 1 every reviewer was told a pass stands "not known", Dispatch "cannot read the repository",
# where production answers that commit is none of the compared commits. The stand-in answers the
# reads the world's flows were measured to make (github-routes.ts's GITHUB_ROUTES) from the run's
# own remote, its pull request opened by the author of the issue branch's first commit, the
# implementer's App, as production's is (rest-objects.ts's `pullRequestJson`, the gh stand-in's
# reading too), and refuses any other, naming it in $evidence/logs/github.log, the log the world
# does not show and health.ts's `github` rule reads. The
# server is the one the batch built from the run's label's own checkout (services_up, go_binary),
# which run-server.sh runs in place of a build of its own (DISPATCH_E2E_SERVER_BIN), so every run
# runs the binary its provenance names (services_from). run-server.sh itself is this checkout's,
# the harness that starts a build: it hands a build the same environment whichever tree it came
# from, since a release ignores a setting it does not know (its own comment says so).
# With no DISPATCH_E2E_PORT, run-server.sh serves Dispatch on a port the kernel chose, held until
# the server serves on it, and the server reports it (`msg="dispatch: listening"
# addr=127.0.0.1:<port>`), which sets dispatch_port. Each run has an evidence directory of its own,
# and the log is emptied first, so the line read is this run's own start.
# The server's agent bearer is the batch's own ($tokens/dispatch-token, services_up), the one each
# agent's DISPATCH_TOKEN_FILE names and can read, and not the harness's `e2e-token`, a word of the
# harness the world would show: run-server.sh gives its server the token its
# DISPATCH_E2E_AGENT_TOKEN_FILE holds, and e2e/api.ts, which seed.ts calls, reads the same file,
# so it is exported for every seed.ts call of the run after this one.
start_dispatch() {
  local url=$1 log=$evidence/logs/dispatch.log github=$evidence/logs/github.log server github_port
  server=$(build_dir "$co")/dispatch-e2e
  [ -x "$server" ] || fail "no Dispatch server of $co at $server: the run's batch builds it (services_up)"
  : >"$log"
  : >"$github"
  start_process github bun "$here/github-standin.ts" "$R" "$worker_repo" "$worker_base"
  await_start github "$github_pid" 30 "the GitHub stand-in to report its port" \
    report_port "$github" '"listening":"127\.0\.0\.1:'
  github_port=$(report_port "$github" '"listening":"127\.0\.0\.1:')
  export DISPATCH_E2E_AGENT_TOKEN_FILE=$tokens/dispatch-token
  DATABASE_URL=$url FAKE_ENVOY_PORT=1 FAKE_GITHUB_PORT=$github_port DISPATCH_E2E_AGENT_SECRETS_URL='' \
    DISPATCH_E2E_SERVER_BIN=$server start_process dispatch bash "$root/packages/dispatch/e2e/run-server.sh"
  await_start dispatch "$dispatch_pid" 300 "Dispatch to report its port" \
    report_port "$log" 'msg="dispatch: listening" addr=127\.0\.0\.1:'
  dispatch_port=$(report_port "$log" 'msg="dispatch: listening" addr=127\.0\.0\.1:')
  await_start dispatch "$dispatch_pid" 60 "Dispatch to answer /api/v1/whoami" dispatch_whoami
}

# dispatch_whoami asks the run's Dispatch (dispatch_port) who the bearer start_dispatch gave it
# is, the header on stdin (`-H @-`) so the bearer is on no command line.
dispatch_whoami() {
  printf 'Authorization: Bearer %s\n' "$(<"$DISPATCH_E2E_AGENT_TOKEN_FILE")" |
    curl -fsS -o /dev/null -H @- "http://127.0.0.1:$dispatch_port/api/v1/whoami"
}

# services_files prints each file a batch's services make, one a line: the given Postgres's URL
# every run reads, the listener token, the Dispatch token, and each label's services file
# (label_services), its provenance (label_provenance) and the port file lib/rig.sh's start_held
# writes for its listener (start_listener; start_held removes it before it starts the listener),
# those three of every label whose file is there. lock_services refuses a batch while one of them
# is there and services_down removes them, so each is a file this batch made, never an earlier
# batch's. The binaries are no such file: they are kept from one batch to the next (go_binary).
services_files() {
  local file
  printf '%s\n' "$services_postgres" "$tokens/envoy-token" "$tokens/dispatch-token"
  for file in "$S"/env-* "$S"/services-from-* "$work"/listener_*.port; do
    [ ! -e "$file" ] && [ ! -L "$file" ] || printf '%s\n' "$file"
  done
}

# One batch at a time per work directory: $work/services/up is its lock, taken before the batch
# starts or stops anything. A batch killed outright leaves it, with its containers, its processes
# and its services' files (services_files). A file of those that is there when the lock is taken
# is an earlier batch's, which this batch would overwrite or remove: the batch is refused, naming
# each, and drops only the lock it took.
lock_services() {
  S=$work/services
  mkdir -p "$S/logs"
  (set -C && : >"$S/up") 2>/dev/null ||
    fail "a batch already holds $S/up; a batch killed outright leaves it, its $tag-* containers and its processes behind"
  local file left=()
  while IFS= read -r file; do
    [ ! -e "$file" ] && [ ! -L "$file" ] || left+=("$file")
  done < <(services_files)
  [ ${#left[@]} = 0 ] || {
    rm -f -- "$S/up"
    fail "an earlier batch left ${left[*]}; a batch replaces no earlier batch's file, so remove them once that batch's containers and processes are stopped"
  }
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

# Each container services_up started, named here once its `docker run` succeeded; services_down
# removes these alone. A `docker run` docker refused (a name an earlier batch's container still
# holds) names nothing, so that container stays.
containers=()

# connections_needed sets services_up's per_run, the connections one run's Dispatch server opens,
# which `store/cmd/connections -process` prints, and its needs, what parallel runs and the batch's
# own queries open at once: per_run for each run and 20 more. It fails when no count is printed.
connections_needed() {
  per_run=$(cd "$root/packages/envoy" && go run ./internal/dispatch/store/cmd/connections -process) &&
    [[ "$per_run" =~ ^[1-9][0-9]*$ ]] ||
    fail "store/cmd/connections -process printed no count of one Dispatch server's connections: $per_run"
  needs=$((per_run * parallel + 20))
}

# services_up LABEL... brings up the batch's services: its Postgres and its NATS, the servers the
# operator gave (given_services) or two containers it starts, and, for each LABEL, out of that
# label's own checkout: an Envoy listener (go_binary, start_listener), the
# Dispatch server each run of the label starts its own Dispatch on (own_dispatch), daemon-pane.go
# built in its daemon module (daemon_pane_binary) and every fact of that daemon a stand-in reads at
# its boot (label_facts), the check that the label's own contract describes the state a stand-in
# serves (label_contract), the label's services file (label_services) naming that listener, and the
# label's service provenance (services_from, label_provenance). A label's agents load the plugins
# of its own checkout, so one listener built from this checkout paired a base label's plugins,
# main ff2a10ef's from before #2603, with a listener from after it, a pairing no deploy ever had,
# and the base runs lost their claim boot to its `409 not_requested`. The Dispatch server is no
# more the same across versions: ff2a10ef's `dispatch result` prints a result's line ending with
# the check's text where the head's ends with the role's reading
# (packages/envoy/internal/dispatch/api/results.go, checkResultLine), and ff2a10ef's server refuses
# a body field it does not declare, `400 INVALID_JSON` (api/server.go's DisallowUnknownFields),
# where the head's ignores it.
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
# key percent-decoded. Its URL is read with require_loopback_dsn (scripts/e2e/lib/loopback-dsn.sh)
# before anything connects to that server or this batch writes the URL anywhere. The guard reads
# the host with libpq, and pg_sql connects with psql, which is libpq, so the host psql reaches is
# the host the guard read. run_database_url then rebuilds, for a placeholder database, the
# connection string each run's Dispatch gets (own_dispatch), and refuses a value that string
# cannot carry, so such a URL is refused here, before anything starts, and not in every run.
services_up() {
  evidence=$S
  local parallel=${SKILL_SCENARIOS_PARALLEL:-5} per_run needs admits fits pg_port nats_port pg_host run_url tool rest param key
  local label dispatch listener pane
  local -a params=() listening=()
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
  else
    connections_needed
    # A given Postgres's URL that a batch killed outright left would send this batch's runs there.
    rm -f -- "$services_postgres"
    # ghcr.io/cybermonklabs/legion/ci, the copy of pgvector/pgvector (.github/ci-images.yaml)
    docker run -d --name "$tag-pg" --cpus 2 --memory 2g --mount type=tmpfs,destination=/var/lib/postgresql \
      -e POSTGRES_PASSWORD=ci -p 127.0.0.1::5432 ghcr.io/cybermonklabs/legion/ci@sha256:2358fcba361ed2233a5ed81b5fe4ca779ccb304120ce531a3bf51c0ed7e2bc11 \
      -c max_connections=$needs >/dev/null
    containers+=("$tag-pg")
    pg_port=$(docker port "$tag-pg" 5432/tcp | sed -n '1s/.*://p')
    [ -n "$pg_port" ] || fail "docker assigned Postgres no host port"
    pg_url="postgres://postgres:ci@127.0.0.1:$pg_port/postgres?sslmode=disable"
    # The hub's nats-server (lib/nats-image.sh reads testnats.HubImage): the listener and the daemon
    # refuse a nats-server below the floor packages/envoy/natsversion holds, every 2.10 among them.
    # shellcheck source-path=SCRIPTDIR/../../../.. source=scripts/e2e/lib/nats-image.sh
    . "$root/scripts/e2e/lib/nats-image.sh"
    docker run -d --name "$tag-nats" --cpus 1 --memory 512m -p 127.0.0.1::4222 "$nats_image" -js >/dev/null
    containers+=("$tag-nats")
    nats_port=$(docker port "$tag-nats" 4222/tcp | sed -n '1s/.*://p')
    [ -n "$nats_port" ] || fail "docker assigned NATS no host port"
    nats_url=nats://127.0.0.1:$nats_port
    until_true 60 "Postgres to accept TCP connections" docker exec "$tag-pg" pg_isready -h 127.0.0.1 -p 5432 -U postgres
    until_true 60 "NATS to be ready" sh -c "docker logs '$tag-nats' 2>&1 | grep -q 'Server is ready'"
  fi
  # Each bearer is 24 random bytes in hex, a batch's own, the 48 letters and digits of the agent
  # token a deployment of Dispatch is given (packages/envoy/infra/gcp's dispatch_cloud.go).
  (umask 077 && mkdir -p "$tokens" && openssl rand -hex 24 >"$tokens/envoy-token" && openssl rand -hex 24 >"$tokens/dispatch-token")
  for label in "$@"; do
    load_label "$label"
    listener=$(go_binary "$co" envoy ./cmd/listener envoy-listener) || fail "the Envoy listener of label $label's checkout $co did not build"
    dispatch=$(go_binary "$co" envoy ./cmd/dispatch dispatch-e2e) || fail "the Dispatch server of label $label's checkout $co did not build"
    pane=$(daemon_pane_binary "$co") || fail "daemon-pane.go did not build in the daemon module of label $label's checkout $co"
    label_facts "$label" "$pane"
    label_contract "$label"
    start_listener "$label" "$listener"
    {
      echo "ENVOY_URL=http://127.0.0.1:$envoy_port"
      echo "ENVOY_NATS_URL=$nats_url"
      echo "ENVOY_TOKEN_FILE=$tokens/envoy-token"
    } >"$(label_services "$label")"
    services_from "$listener" "$dispatch" "$pane" >"$(label_provenance "$label")"
    listening+=("$label :$envoy_port")
  done
  note "services up: listeners ${listening[*]}, NATS $(safe_url "$nats_url"), Postgres $(safe_url "$pg_url")"
}

# services_down stops and removes what this batch started and made, and nothing else: each
# label's listener (listener_pids), the containers it started (containers), and its services'
# files (services_files), which lock_services found absent before this batch made them, the
# tokens directory they leave empty, then its lock. A given Postgres and a given NATS keep
# running, and every database a run made on that Postgres (own_dispatch) stays there for the
# operator to drop.
# shellcheck disable=SC2329 # stop_batch, an exit trap, runs it
services_down() {
  local pid files
  for pid in "${listener_pids[@]}"; do stop_tree "$pid"; done
  [ ${#containers[@]} = 0 ] || docker rm -fv "${containers[@]}" >/dev/null 2>&1 || true
  mapfile -t files < <(services_files)
  rm -f -- "${files[@]}" "$S/up"
  rmdir -- "$tokens" 2>/dev/null || true
}

# conninfo_url CONNINFO prints the postgres:// URL of the keyword/value connection string CONNINFO
# run_database_url printed (loopback-dsn.py's `--dbname` form, `key='value' ...`, each value as
# libpq decoded it, a backslash before each `\` and `'`): the user and the password, the host (in
# brackets where it is ::1), the port and the database, each value percent-encoded, and every
# other keyword as a query parameter. It is the one URL the run's two readers that open its
# database with Bun.SQL take (seed-handoffs.ts's `pullRequestHead`, which seed.ts pull-request
# and head-watcher.ts run): Bun.SQL takes no keyword/value string (`Invalid URL`), and it reads
# the host of a given URL's own text where libpq reads another one: of
# `postgres://evil.example?@127.0.0.1:5433/db`, Bun 1.4.4 read the host evil.example and libpq
# 127.0.0.1 (measured 2026-11-25). Rebuilt from what libpq read, the URL names the host the
# loopback guard read, and a user `evil.example?` is `evil.example%3F`. CONNINFO reaches jq in its
# environment, never on a command line, since it can hold the password.
conninfo_url() {
  CONNINFO=$1 jq -nr '
    [env.CONNINFO | capture("(?<key>[a-z_]+)=\u0027(?<value>(?:[^\u0027\\\\]|\\\\.)*)\u0027"; "g")
      | .value |= gsub("\\\\(?<c>.)"; "\(.c)")]
    | from_entries
    | "postgres://"
      + (if .user or .password
         then (.user // "" | @uri) + (if .password then ":" + (.password | @uri) else "" end) + "@"
         else "" end)
      + (if (.host | test(":")) then "[\(.host)]" else .host end)
      + (if .port then ":\(.port)" else "" end)
      + "/" + (.dbname | @uri)
      + ([to_entries[] | select(.key | IN("user", "password", "host", "port", "dbname") | not)
          | "\(.key | @uri)=\(.value | @uri)"]
         | if length > 0 then "?" + join("&") else "" end)'
}

# own_dispatch SEED gives the run a Dispatch of its own and seeds it with `seed.ts SEED`, the world
# the scenario needs (project, or brainstorm): a database named after the run in the batch's
# Postgres, and a Dispatch server on it, which stop_run stops. It records the Postgres, the
# database and the server in $R/dispatch.json, which score.ts reads, and sets dispatch_port and
# dispatch_database_url, the database's postgres:// URL, which seed_ts and start_heads hand
# seed.ts and the head watcher. The batch's Postgres is the server $services_postgres names, and
# the batch's own container when that file is absent.
# On a given server, the run's Dispatch gets the connection string
# run_database_url (scripts/e2e/lib/loopback-dsn.sh) rebuilds from the given URL with libpq, with
# the run's database as its dbname: pgx, which Dispatch opens it with, reads that keyword/value
# form. The two readers of Bun.SQL get that string as a URL (conninfo_url), never the given URL.
own_dispatch() {
  local seed=$1 database=run_${R##*/} pg_port postgres url
  database=${database//-/_}
  # Postgres cuts a longer name to 63 bytes, and two runs whose names agree that far would share it.
  [ "${#database}" -le 63 ] || fail "the run's database name $database is longer than Postgres's 63 bytes"
  if [ -f "$services_postgres" ]; then
    postgres=$(<"$services_postgres")
    run_database_url url SKILL_SCENARIOS_POSTGRES_URL "$postgres" "$database" "this run creates a database there" || fail "$url"
    dispatch_database_url=$(conninfo_url "$url") ||
      fail "the connection string of $database on $(safe_url "$postgres") makes no URL (conninfo_url)"
    # A given server must let its login make a database. Nothing here checks that: the refusal a
    # server without the right gives is Postgres's own, in the run's fixture.log.
    pg_sql "$postgres" "CREATE DATABASE $database" >>"$R/fixture.log" 2>&1 ||
      fail "CREATE DATABASE $database on $(safe_url "$postgres") failed; see $R/fixture.log"
    postgres=$(safe_url "$postgres")
  else
    pg_port=$(docker port "$tag-pg" 5432/tcp | sed -n '1s/.*://p')
    postgres=$(docker inspect -f '{{.Id}}' "$tag-pg")
    [ -n "$pg_port" ] && [ -n "$postgres" ] || fail "no Postgres container $tag-pg for the run's Dispatch"
    docker exec "$tag-pg" psql -U postgres -v ON_ERROR_STOP=1 -qc "CREATE DATABASE $database" >>"$R/fixture.log" 2>&1 ||
      fail "CREATE DATABASE $database failed; see $R/fixture.log"
    url="postgres://postgres:ci@127.0.0.1:$pg_port/$database?sslmode=disable"
    dispatch_database_url=$url
  fi
  start_dispatch "$url"
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
    echo "DISPATCH_TOKEN_FILE=$tokens/dispatch-token"
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
# SKILL_SCENARIOS_ASKGATE_MODE is the pane's OMP_ASKGATE and the run record's `mode` (cmd_run);
# unset, the pane gets no OMP_ASKGATE, so AskGate's verdicts block and its marker checks warn.
run_askgate() {
  local name=$1 askCase=$2 mode=${SKILL_SCENARIOS_ASKGATE_MODE:-} issue message ref ask billing one
  case $mode in '' | warn | block) ;; *) fail "SKILL_SCENARIOS_ASKGATE_MODE is '$mode', not warn or block" ;; esac
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
  # The repository's history is its own developer's, given as fixture_git gives it: in the
  # variables git reads ahead of any `-c`, so the operator's own GIT_AUTHOR_* (a Legion pod's App)
  # never reaches the agent's `git log`.
  GIT_AUTHOR_NAME=$world_author_name GIT_AUTHOR_EMAIL=$world_author_email \
    GIT_COMMITTER_NAME=$world_author_name GIT_COMMITTER_EMAIL=$world_author_email \
    git -C "$R/cwd" -c commit.gpgsign=false commit -qm "A tiny to-do CLI"
  # shellcheck disable=SC2016 # the backticks are the prompt's own, a command name for the agent
  printf '/brainstorming a small feature: let a task in this to-do CLI carry an optional due date, and have `list` show overdue tasks first.\n' \
    >"$R/prompt.txt"
  dispatch_session "$name" capture-brainstorm
}

# seed_ts MODE ARG... runs `seed.ts MODE ARG...` against the run's own Dispatch (own_dispatch), its
# output appended to $R/fixture.log. A failure is the rig's, never the run's. The run's database
# URL goes in seed.ts's environment, never its command line, which every user of the box reads:
# the one mode that writes the database itself (`pull-request`) reads it there.
seed_ts() {
  (cd "$root/packages/dispatch" && DISPATCH_E2E_PORT=$dispatch_port SEED_DATABASE_URL=$dispatch_database_url \
    bun "$here/seed.ts" "$@") >>"$R/fixture.log" 2>&1 || fail "seed.ts $1 failed; see $R/fixture.log"
}

# judge_ts MODE ARG... runs `judge.ts MODE ARG...` with SKILL_SCENARIOS_WORK at this rig's work
# directory, its output appended to $R/judge.log.
judge_ts() {
  SKILL_SCENARIOS_WORK=$work bun "$here/judge.ts" "$@" >>"$R/judge.log" 2>&1 ||
    fail "judge.ts $1 failed; see $R/judge.log"
}

# next_run NAME LABEL prints one above the highest <n> of a run named <NAME>-<LABEL>-<n>, live or
# archived (archive_run), or 1 when there is none: a run's name is its record's `name` (cmd_run),
# or, for a run recorded before the names left the world, its directory's own. NAME is the
# scenario, with its case after it for a cased scenario, as cmd_run names a run: cmd_batch numbers
# every run it starts from this, so no batch takes the name of a run of an earlier one.
next_run() {
  local name n highest=0 record
  {
    for name in "$work/runs/$1-$2-"* "$work/archive/runs/$1-$2-"*; do
      [ ! -d "$name" ] || printf '%s\n' "${name##*/}"
    done
    for record in "$work"/runs/*.json "$work"/archive/runs/*.json; do
      [ ! -f "$record" ] || jq -r '.name' "$record"
    done
  } | {
    while IFS= read -r name; do
      case $name in "$1-$2-"*) ;; *) continue ;; esac
      n=${name#"$1-$2-"}
      [[ $n =~ ^[0-9]+$ ]] || continue
      ((10#$n <= highest)) || highest=$((10#$n))
    done
    printf '%s\n' "$((highest + 1))"
  }
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
    echo "DISPATCH_TOKEN_FILE=$tokens/dispatch-token"
    echo "ENVOY_URL=http://127.0.0.1:1"
  } >>"$R/pane.env"
  spec_turns "$name" "$issue" "$prompt"
  seed_ts capture-spec "$R/fixture.json" "$R"
}

# The judge's two profiles, under $work/judge-profiles/<kind>, each under a HOME of its own with the
# model route install_model_route installs and no plugin. `ste` holds one user skill, this
# checkout's skills/asd-ste100. `bare` holds none. The first judge-omp call builds both, and every
# other call waits on the same lock, so parallel calls are safe. A build an earlier call left
# unready (no ready file: it failed, or was killed outright) is refused, naming the directory,
# before this call sets its exit handler: its install.log says why it failed, and the operator
# removes the directory to rebuild, as a label's profile is rebuilt (cmd_profile).
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
    [ -z "$(ls -A -- "$dir")" ] ||
      fail "judge-omp: $dir holds a judge-profile build an earlier call left unready (no $dir/ready); see its install.log, then remove $dir to rebuild"
    on_exit "$(printf 'drop_unready_keys %q %q %q' "$dir/ready" "$dir/ste/model-key" "$dir/bare/model-key")"
    [ -f "$root/skills/asd-ste100/SKILL.md" ] || fail "$root has no skills/asd-ste100, the judge's one skill"
    commit=$(checkout_commit "$root") || fail "cannot read the commit of $root with jj or git"
    for kind in ste bare; do
      P=$dir/$kind
      mkdir "$P"
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
# AT is the path the judge sees KEYS at: /opt/key, where the container mounts it (the default), or
# KEYS itself for a judge that runs in the pod (judge_in_pod), whose world shows KEYS at its own
# path; the key command and the models.yml name the key command and the token there.
judge_keys() {
  local P=$1 agent=$2 keys=$3 at=${4:-/opt/key} host_command token exp deadline payload key_file read_from
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
    read_from=$at/token
  fi
  # $(cat) drops trailing newlines and keeps a key whose file has none, as the host's key command
  # does with $(<file).
  (umask 077 && printf '#!/bin/sh\nkey=$(cat %s) && [ -n "$key" ] && printf "%%s\\n" "$key"\n' "$read_from" >"$keys/anthropic-key")
  chmod 0700 "$keys/anthropic-key"
  sed "s#!exec $host_command\"#!exec $at/anthropic-key\"#g" "$agent/models.yml" >"$keys/models.yml"
  grep -qF "!exec $at/anthropic-key\"" "$keys/models.yml" && ! grep -qF "$host_command" "$keys/models.yml" ||
    fail "judge-omp: $agent/models.yml does not name the key command $host_command"
  [ -z "${key_file:-}" ] || printf '%s\n' "$key_file"
}

# judge_world DIR W KEYS OMP_DIR [KEY_DIR] prints the world of one judge that runs in the pod
# (cmd_judge_omp), one `<kind> <path>` a line in world_paths' kinds, for private_world to mount.
# Hidden: every path world_paths hides from a run's agent (the scratch root, the issue
# workspaces, the work directory, every label's checkout and the pod's credentials), and the
# operator's HOME, which no judge reads (the container shows none of it). Shown again: the judge's
# own directory DIR and its world W (its HOME and /tmp) read-write, and read-only its key
# directory KEYS (judge_keys), the directory of the omp it runs, and KEY_DIR, the directory of the
# file its key command reads on the key-file and provider routes. A shown path that is a link, or
# lies under one, is shown at what it resolves to as well, as world_paths shows one.
judge_world() {
  local dir=$1 W=$2 keys=$3 omp_dir=$4 key_dir=${5:-} workspaces=/legion/workspaces file entry kind real
  {
    printf 'cover %s\n' "$(realpath -m -- "${TMPDIR:-/tmp}")"
    [ -z "${LEGION_WORKSPACE:-}" ] || workspaces=${LEGION_WORKSPACE%/*/*/*}
    printf 'cover %s\n' "$(realpath -m -- "$workspaces")"
    printf 'cover %s\n' "$work"
    for file in "$work"/profiles/*/checkout; do [ ! -f "$file" ] || printf 'cover %s\n' "$(<"$file")"; done
    case ${HOME:-/} in /) ;; *) printf 'cover %s\n' "$(realpath -m -- "$HOME")" ;; esac
    for entry in /var/run/legion /var/run/secrets; do
      printf 'credentials %s\n' "$entry"
      [ "$(realpath -m -- "$entry")" = "$entry" ] || printf 'credentials %s\n' "$(realpath -m -- "$entry")"
    done
    {
      printf 'rw %s\n' "$W" "$dir"
      printf 'ro %s\n' "$keys" "$omp_dir"
      [ -z "$key_dir" ] || printf 'ro %s\n' "$key_dir"
    } | while read -r kind entry; do
      printf '%s %s\n' "$kind" "$entry"
      real=$(realpath -m -- "$entry")
      [ "$real" = "$entry" ] || printf '%s %s\n' "$kind" "$real"
    done
  } | LC_ALL=C sort -k2 | uniq
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
# Where the batch has its services given (given_services: SKILL_SCENARIOS_POSTGRES_URL and
# SKILL_SCENARIOS_NATS_URL), as in a Legion pod, which has no Docker, the judge runs in the pod
# instead, in the form a run's agent runs in (launch): under `env -i`, with a HOME of its own,
# DIR.world/home, which holds the profile's config.yml, the key directory's models.yml and the
# profile's skills as the container's start copies them, and DIR.world/tmp as its /tmp, in a
# namespace of its own (private_world) whose world judge_world declares. Its key directory is seen
# at its own path (judge_keys' AT). The namespace takes no mount onto a file (a gVisor pod refuses
# one), so DIR is shown read-write whole, where the container shows DIR/OUTPUT and DIR/sessions
# alone writable: judge.ts reads the one answer file and nothing else of DIR. A host that refuses
# the namespace is refused before anything is built (require_namespace), as a run is, with no
# fallback to the shared world. DIR.world is refused as a leftover and removed at the judge's end
# as the key directory is. Its CPU and memory are bound by the pod's alone, as a run's agent's are.
cmd_judge_omp() {
  [ $# = 3 ] || fail "usage: rig.sh judge-omp <ste|bare> <dir> <output file name>"
  local kind=$1 dir output=$3 skills status=0 omp P agent keys key_file h=/home/judge
  case $kind in ste) skills=(--skills asd-ste100) ;; bare) skills=(--no-skills) ;; *) fail "judge-omp: '$kind' is ste or bare" ;; esac
  dir=$(cd "$2" && pwd -P) || fail "judge-omp: $2 is not a directory"
  [ -f "$dir/prompt.txt" ] || fail "judge-omp: $dir has no prompt.txt"
  case $output in
  "" | . | .. | */* | prompt.txt | sessions | out.txt | judge-model.txt | .tmp) fail "judge-omp: '$output' is not the name of an answer file in $dir" ;;
  esac
  # What an earlier judge of DIR left is that judge's: its output and model record, and the key
  # directory and the world beside DIR that a judge killed outright leaves. Each is refused, naming
  # it, and nothing of it is removed or replaced. The key directory and the world are removed by
  # the judge that made them alone (on_exit, below).
  local leftover world
  keys=$dir.key
  world=$dir.world
  for leftover in "$dir/out.txt" "$dir/judge-model.txt" "$keys" "$world"; do
    [ ! -e "$leftover" ] && [ ! -L "$leftover" ] ||
      fail "judge-omp: $leftover is there from an earlier judge of $dir; nothing of it is removed or replaced: give this judge a directory of its own, and remove a key directory a killed judge left"
  done
  case "$(omp_ancestor "$dir" || true)" in "") ;; *) fail "judge-omp: $dir is under $(omp_ancestor "$dir"), whose plugins Oh My Pi would load" ;; esac
  case ${LEGION_E2E_MODEL_ROUTE:-gateway} in gateway | key-file | provider) ;; *) fail "judge-omp: LEGION_E2E_MODEL_ROUTE is '$LEGION_E2E_MODEL_ROUTE': it is gateway, key-file or provider" ;; esac
  if given_services; then
    require_namespace
  else
    command -v docker >/dev/null || fail "judge-omp: docker is not on PATH; each judge runs in a container"
  fi
  judge_profiles
  # The pinned omp, by the rig's own mise (rig_mise), found before this judge writes anything, so a
  # refused fetch leaves nothing that the next judge of DIR would refuse.
  if [ -n "${SKILL_SCENARIOS_JUDGE_OMP:-}" ]; then
    omp=$SKILL_SCENARIOS_JUDGE_OMP
  else
    rig_mise
    omp=$(pinned_omp "$(<"$work/judge-profiles/pin")") || exit 1
  fi
  [ -x "$omp" ] || fail "judge-omp: $omp is not an executable omp"
  P=$work/judge-profiles/$kind
  agent=$P/home/.omp/profiles/judge-$kind/agent
  local route=() host ip extra=() words=() word
  on_exit "$(printf 'rm -rf -- %q %q' "$keys" "$world")"
  if given_services; then
    key_file=$(judge_keys "$P" "$agent" "$keys" "$keys") || exit 1
  else
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
  fi
  printf 'model %s\nthinking %s\n' "$judge_model" "$judge_thinking" >"$dir/judge-model.txt"
  mkdir -p "$dir/sessions"
  : >>"$dir/$output"
  for word in ${SKILL_SCENARIOS_JUDGE_ENV:-}; do
    words+=("$word")
    extra+=(-e "$word")
  done
  if given_services; then
    # The judge's HOME and /tmp, then its world. A shown path that is or holds a path the world
    # hides would show all it holds, so it is refused before the judge starts, as launch refuses
    # one. The key file's directory is shown on the two routes whose key command reads a file: the
    # file's own on the key-file route, the directory judge_keys names on the provider route.
    local a=$world/home/.omp/profiles/judge-$kind/agent shown='' line_kind shown_path other hidden
    mkdir -p "$world/tmp" "$a"
    cp "$agent/config.yml" "$keys/models.yml" "$a"/
    [ ! -d "$agent/skills" ] || cp -r "$agent/skills" "$a"/
    [ -z "$key_file" ] || { shown=$key_file && [ -d "$shown" ] || shown=$(dirname -- "$shown"); }
    judge_world "$dir" "$world" "$keys" "$(dirname -- "$omp")" "$shown" >"$world/namespace.txt"
    while read -r line_kind shown_path; do
      case $line_kind in rw | ro) ;; *) continue ;; esac
      while read -r other hidden; do
        case $other in cover | credentials) ;; *) continue ;; esac
        case $hidden in "${shown_path%/}" | "${shown_path%/}"/?*) fail "judge-omp: the judge's world shows $shown_path ($line_kind), which is or holds $hidden, a path it hides (judge_world)" ;; esac
      done <"$world/namespace.txt"
    done <"$world/namespace.txt"
    env -i PATH="$namespace_path" "$(command -v unshare)" -Urm -p --fork --mount-proc sh -c "$private_world" - "$(id -u)" "$(id -g)" "$world" "$dir" \
      env -i PATH="$namespace_path" HOME="$world/home" USER=judge TERM=xterm-256color TMPDIR=/tmp OMP_PROFILE="judge-$kind" \
      PI_NOTIFICATIONS=off PI_NO_TITLE=1 ENVOY_URL=http://127.0.0.1:1 "${words[@]}" \
      timeout "${SKILL_SCENARIOS_TIMEOUT:-1500}" "$omp" -p "$(<"$dir/prompt.txt")" --no-extensions "${skills[@]}" \
      --model "$judge_model" --thinking "$judge_thinking" --session-dir "$dir/sessions" </dev/null >"$dir/out.txt" 2>&1 || status=$?
    [ -e "$world/namespace-ready" ] ||
      fail "judge-omp: the judge of $dir did not start: its namespace could not be made: $(head -n 3 "$dir/out.txt" | tr '\n' ' ')"
    echo "exit=$status" >>"$dir/out.txt"
    rm -rf -- "$keys" "$world"
    return "$status"
  fi
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

# cmd_judge RUN... runs judge.ts's prompts on each spec run, and on what the agents of each
# reviewer-text-findings or threads-closed run wrote (judge.ts's `judgeWorker`), which the run's
# own end judges already and this judges again once its answers are moved aside.
cmd_judge() {
  [ $# -ge 1 ] || fail "usage: rig.sh judge <run dir>..."
  SKILL_SCENARIOS_WORK=$work exec bun "$here/judge.ts" run "$@"
}

# tree_digest ROOT... prints one SHA-256 of the trees at each ROOT, in order: each entry's path
# below its root, its kind, its mode and a link's target, then each regular file's bytes, each
# list in byte order (LC_ALL=C), so two trees one entry, byte, mode or link apart digest apart. A
# link is never followed. The model route's live directories under an install (key, cache and
# model-key: its call record, its minted key and its key command, install_model_route) are left
# out, since every model call of a batch rewrites them.
tree_digest() {
  local tree
  for tree in "$@"; do
    [ -d "$tree" ] || fail "tree_digest: $tree is not a directory"
    printf 'root\n'
    (cd "$tree" && find . \( -path ./key -o -path ./cache -o -path ./model-key \) -prune -o -printf '%y %m %p %l\n' | LC_ALL=C sort)
    (cd "$tree" && find . \( -path ./key -o -path ./cache -o -path ./model-key \) -prune -o -type f -print0 | LC_ALL=C sort -z | xargs -0 -r sha256sum)
  done | sha256sum | cut -d' ' -f1
}

# cmd_identity LABEL prints the three lines of the rig's identity that Verification v47's rule 4
# has each result name for a label it ran, each `<name> <value>`:
#   profile-tree  the tree digest (tree_digest) of the label's built profile: its directory and
#                 its install (load_label's P and I), which hold the plugins, the pinned Oh My Pi,
#                 the profile and the checkout it was built from
#   pack          the pack digest, the `skills` line of the label's built-from (cmd_profile)
#   rig           the rig's own revision: this checkout's commit, as checkout_commit reads it
# The fourth line of rule 4, the daemon gate (rule 5), the role that records the check reads
# itself.
cmd_identity() {
  [ $# = 1 ] || fail "usage: rig.sh identity <label>"
  local P I pack rev tree
  P=$(label_dir "$1")
  [ -f "$P/ready" ] || fail "no profile for label $1: run \`rig.sh profile $1 <checkout>\` first"
  I=$P
  [ ! -f "$P/install" ] || I=$work/installs/$(<"$P/install")
  pack=$(sed -n 's/^skills //p' "$P/built-from")
  [ -n "$pack" ] || fail "label $1's built-from ($P/built-from) has no skills line"
  rev=$(checkout_commit "$root") || fail "cannot read the commit of $root with jj or git"
  tree=$(tree_digest "$P" "$I")
  printf 'profile-tree %s\npack %s\nrig %s\n' "$tree" "$pack" "$rev"
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

# The phase worker's frozen world under $R, on the run's own issue <key> (seed.ts tester-proof,
# which the scenario's runner files first): a bare remote whose post-receive hook logs every push,
# the issue workspace cloned from it, the implementer's commit on legion/<key>, the PR the gh
# stand-in serves, and the implementer's handoff body at $R/implement.json, which the runner
# records on the issue. The fixture's content is the same in every run; the PR number is 1000 plus
# <index>, the run's index in its batch, and the commits are dated <index> seconds after <start>,
# the batch's start, so no two runs of a batch share a PR number or a head (two runs can meet in a
# scratch file in the shared /tmp), and each batch's heads carry its own start time.
# The CLI must meet every requirement the seeded spec marks, and the issue's Verification document
# holds the check that the implementer's seeded result and the tester's own run are against.
# testerProof fails a run that
# pushes a change to legion/<key>, and it cannot tell a tester that pinned a real defect with a red
# test, which roles/core/tester.md asks for, from one that went off-task. A defect left in the
# fixture turns correct testing into a fail. What a push to legion/<key> means is each world's
# own, said below: tester-proof's ends with no commit of the tester's own and fails one; the
# review round's text-only world asks for the reviewer's one text commit, and its defect and
# last-push worlds fail any push; and the threads-closed worlds ask for the implementer's one push
# of text in correct and for none in accept, crossing and code-defect.
#
# <case>, empty for tester-proof, is the cased scenario's case: reviewer-text-findings' text-only,
# defect and last-push, and threads-closed's accept, correct, crossing and code-defect. Each names
# one of the three worlds below, the case it is built for, and whether the CLI's blank-name guard,
# which R2 of the seeded spec asks for, is dropped (case_guard).
worker_fixture() {
  local index=$1 start=$2 case_name=${3:-} guard
  guard=$(case_guard "$case_name")
  case $case_name in
  '') tester_proof_world "$index" "$start" ;;
  text-only | defect | last-push) review_world "$index" "$start" "$guard" "$case_name" ;;
  accept | correct | crossing | code-defect) approved_world "$index" "$start" "$guard" "$case_name" ;;
  *) fail "worker_fixture: '$case_name' is no case of reviewer-text-findings or threads-closed" ;;
  esac
}

# case_guard CASE prints fixture_cli's GUARD for CASE's world: `dropped` for the two negative
# controls, defect and code-defect, whose CLI greets a blank name, and nothing for every other
# world, text-only's, last-push's and crossing's among them. It is one answer for two readers,
# because the world's code and the issue's check go together: worker_fixture builds the code with
# it, and case_check gives the issue the check that missed the blank name where the code dropped
# the guard (seed.ts tester-proof's `no-name`), since a check that covered the blank name would
# hold a recorded pass the head fails.
case_guard() {
  case $1 in defect | code-defect) echo dropped ;; esac
}

# case_check CASE prints the kind of check seed.ts tester-proof gives CASE's issue (world-issue.ts's
# CheckCovers), and nothing for the default, `r2`: `no-name` where case_guard drops the guard, for
# the reason it gives, and `doc-example` for crossing, whose check also compares `bun greet.ts Ada`
# with the example line of docs/cli.md (fixture_round_files) the reviewer's commit rewords, so the
# tester's earlier pass does not hold at that commit (approved_world, Verification v47's k3).
case_check() {
  case $1 in
  crossing) echo doc-example ;;
  *) [ -z "$(case_guard "$1")" ] || echo no-name ;;
  esac
}

# The world the tester reads: the CLI, its tests, and the implementer's handoff and check line,
# with nothing a later phase left. It keeps the blank-name guard, so the CLI meets R2, and a push
# to legion/<key> is this world's own failure, since the tester ends with no commit of its own.
# No review stands on its pull request, so GitHub reports it `REVIEW_REQUIRED` and `BLOCKED`, as
# production reports a head its base's approval is still owed on (gh-threads.ts's
# `reviewDecisionOf`, `mergeStateOf`).
tester_proof_world() {
  local index=$1 start=$2 src when worker_pr worker_pr_url C1 body
  fixture_open "$index" "$start"
  fixture_first_commit
  fixture_cli ''
  fixture_head_commit
  fixture_implement
  fixture_workspace
  fixture_body "**Threads:** 0 resolved, 0 unresolved." "$(fixture_checks implementer)"
  fixture_write '[]' '[]'
}

# The two text findings a round's world carries, each a named constant because a world states
# which findings it has by passing them and no world takes either by default. The sentence the
# change makes false, and a comment naming a push trigger `on:` does not carry: closing either
# changes no code that runs. The negative control is the world that passes neither
# (approved_world's dropped guard), and it says so in the same place, so no edit to a default
# can quietly give it a text finding it is the control for not having.
text_finding_doc_line='It prints the name exactly as it was given, spaces and all.'
text_finding_trigger=' and on every push to main'

# The example line docs/cli.md gives for the one command the issue's check runs, which every round
# world's document carries below the change's own line (fixture_round_files), true of the CLI in
# every world and no finding of any. The check of crossing's issue compares the command's output
# with it (case_check's `doc-example`), and the reviewer's commit of accept and crossing rewords it
# and keeps its greeting (approved_world), so the reworded line stays true and only crossing's check
# reads the line it changed.
# shellcheck disable=SC2016 # the backticks are the document's own markdown
cli_example_line='`bun greet.ts Ada` prints `Hello, Ada!`'

# round_world INDEX START GUARD DOC_LINE TRIGGER builds the base both worlds of a round after
# the implementer are built on, every step they share: the tester-proof world's commits, the two
# files such a round reads — docs/cli.md, whose last sentence is DOC_LINE, and
# .github/workflows/greet.yml, whose comment names TRIGGER beside the pull request trigger `on:`
# carries (a file the implement App may push and the review App may not) — the implementer's
# handoff at its commit, whose diff touches four files, the tester's handoff body at $R/test.json,
# which the runner records beside the implementer's, and the issue workspace. DOC_LINE and
# TRIGGER are the world's own and are required: every caller names the text findings it means
# its world to carry, an empty TRIGGER being an honest comment and so a finding the world has
# not got. GUARD is fixture_cli's: `dropped` drops the CLI's blank-name guard, so a blank name
# prints a greeting where R2 of the seeded spec asks for the usage line and exit 2, and a
# behavioural finding stands.
#
# It leaves the caller's own $src, $when, $C1, $worker_pr and $worker_pr_url set, as the steps it
# calls do. Each world adds its own routes around the ones fixture_write writes for every world,
# in the order the stand-in matches them (gh-standin.ts). No world routes `gh pr diff`: the
# stand-in answers that from the run's own remote at the call, since a diff frozen here would read
# the same whatever the run pushed. No world routes the submit of a review (`…/reviews/<id>/events`)
# either: the stand-in answers that from what the run has already done, since what GitHub answers
# there turns on the review's own state and no frozen answer could tell a pending review from a
# submitted one.
round_world() {
  [ $# = 5 ] || fail "round_world: expected INDEX START GUARD DOC_LINE TRIGGER, got $# argument(s)"
  local index=$1 start=$2 guard=$3 doc_line=$4 workflow_trigger=$5
  fixture_open "$index" "$start"
  fixture_docs_before
  fixture_first_commit
  fixture_cli "$guard"
  fixture_round_files "$doc_line" "$workflow_trigger"
  fixture_head_commit
  fixture_implement
  fixture_test_handoff
  fixture_workspace
}

# review_world INDEX START GUARD [CASE] builds the world a review round reads, which
# reviewer-text-findings' three cases build: the round's base world (round_world), whose two
# findings this round is the first to read, and the pull request's
# one unresolved bot thread, whose finding is a wording nit the reviewer answers and closes
# nothing for. The bot left it as GitHub leaves every inline comment, in a review of its own
# (COMMENTED, with no body), so the reviews list holds that review and the thread's comment is a
# review comment of it, read the same through GraphQL and REST. `dropped` is that change with the
# blank-name guard gone, the negative control's behavioural finding. The run's remote takes a push
# of the reviewer's that changes no workflow file, onto legion/<key>, and the pull request's head
# moves with it, its checks concluded as every head's of this world are (fixture_write's
# concluded_checks); it refuses one that changes the workflow (fixture_first_commit). Before the
# review no
# approval stands, so GitHub reports the head `REVIEW_REQUIRED` and `BLOCKED` with every check
# green, as production reports a head awaiting its review (gh-threads.ts's `reviewDecisionOf`,
# `mergeStateOf`). The base requires conversation resolution too (resolution_protection_routes)
# and the bot's thread is open, so the head stays `BLOCKED` after the reviewer's approval, until
# the run resolves the last thread: the approval then waits in `reviewing`, as main's engine
# leaves it (review.go's `reviewRound`, which reports the round `stuckMergeState`), so a reviewer
# that reads the record after its approval reads `reviewing`, and the wake of the implementer it
# owns at the approval is the one step that starts the fix (review 5887659447, finding 3). The
# thread's comment names its bot as GitHub's GraphQL names one, a `Bot` by its App's slug with no
# `[bot]`, and its review names it as the REST API does, with its `[bot]` (gh-threads.ts's
# `actorOf`, measured on 2026-11-18). CASE `last-push` is text-only's world (GUARD empty) on a
# base whose rules require the last push approved by someone other than its pusher
# (`require_last_push_approval: true`, resolution_protection_routes): there the reviewer pushes no
# fix, and the world's head is the implementer's push, which the tester's pass stands at. Every
# other CASE, and none, is a base that asks no approval of the last push, production's.
review_world() {
  local index=$1 start=$2 guard=$3 last_push=false src when worker_pr worker_pr_url C1 body threads reviews
  [ "${4:-}" != last-push ] || last_push=true
  round_world "$index" "$start" "$guard" "$text_finding_doc_line" "$text_finding_trigger"
  fixture_body "**Threads:** 0 resolved, 1 unresolved." "$(fixture_checks implementer tester)"
  threads=$(jq -n '[{id:"PRRT_kwDOrig0001", isResolved:false, path:"greet.test.ts", line:4, review:4001,
    comments:[{author:{__typename:"Bot", login:"style-bot"},
      body:"greet.test.ts:4: `greets by name` names the same thing the next test does; name each test for the case it covers."}]}]')
  reviews=$(jq -n --arg head "$C1" '[{id:4001, state:"COMMENTED", commit_id:$head,
    author:{login:"style-bot[bot]"}, user:{login:"style-bot[bot]"}, body:""}]')
  fixture_write "$(resolution_protection_routes "$last_push")" "$reviews" '{}' "$threads"
}

# The world the implementer's last chance reads, which threads-closed's four cases build (spec
# v36's R2, Verification v47's k2 and k3): the round's base world (round_world), then, in accept,
# correct and crossing, the reviewer's own text commit above the implementer's, which the review
# App made and pushed and which is the pull request's head, with no approval: the reviewer
# records no pass at its own commit and waits for the implementer's answer, so GitHub reports the
# pull request `REVIEW_REQUIRED` and its head `BLOCKED` (gh-threads.ts's `reviewDecisionOf`,
# `mergeStateOf`) and the issue stays in `reviewing`; in code-defect no commit of the reviewer's
# and its approval of the implementer's head, which stands, so GitHub reports the pull request
# `APPROVED`. Its body carries the reviewer's `Threads:` count and one open entry per thread it
# left, the reviewer's handoff body is at $R/review.json for the runner to record, and the
# threads themselves are in the world's GraphQL graph (fixture_write's THREADS), each opened by an
# inline comment of the reviewer's review 5001 — a `COMMENT` review at its commit in correct (spec
# v36's R1, step 4), the approval in code-defect — and in $R/threads.json, which the score reads.
# The implementer's and the tester's passes stand at the implementer's commit, $C1, in the body's
# check lines and in the run's seeds (worker_run_world, run_threads_closed), and no result stands
# at the reviewer's commit: whether an earlier pass holds there is the implementer's to decide, as
# it did not make that commit. The issue workspace was cloned before the reviewer's push, so it
# stands at the implementer's commit until the implementer fetches, as a pod's does. The threads
# are GitHub's state and so the run's: `resolveReviewThread` answers as GitHub answers a role with
# write access, the thread resolved, a reply by GraphQL or by the REST route is its thread's last
# comment from then on, and every later read of the threads finds both (gh-standin.ts's
# `threadState`). It encodes no verdict of its own — a stand-in that refused the mutation would
# prove the claim the scenario is there to measure — and the day a live reviewer finds GitHub
# refusing it, this answer and the rule change together on that evidence. The base branch's
# protection is read at both reads the merge gate prescribes — the rules of the branch and the
# branch itself — and fixture_write serves both, since a 404 there is a refused read, which the
# rule has a reviewer report and never guess around (the batch of 2026-11-10 holds 91 refused
# reads of this kind); the base's ruleset names the checks the head reports as the ones it requires.
# `gh pr diff` is the diff of the run's own remote at the call, so each agent reads the commits
# pushed so far and no world freezes a diff of its own (gh-standin.ts). The score reads the calls
# each role made and never the threads' state, so that state changes what a role reads and
# nothing of what a run scores.
#
# CASE names the world:
#   accept       the reviewer's commit says what the CLI does — that it trims the spaces around the
#                name — and rewords the example line of docs/cli.md ($cli_example_line), keeping
#                its greeting, and the workflow's comment is honest (the empty trigger), so no
#                thread is left. The issue's check runs `bun greet.ts Ada` and reads no line of
#                docs/cli.md, so the tester's earlier pass holds at the reviewer's commit. Its base
#                requires conversation resolution, which no open thread holds; with no approval
#                the head is `BLOCKED` and the issue in `reviewing` (run_threads_closed).
#   crossing     accept's world, the reviewer's commit the same, but for the issue's check, which
#                also compares the output of `bun greet.ts Ada` with the example line of
#                docs/cli.md (case_check's `doc-example`): that commit rewords the line, so the
#                tester's earlier pass does not hold at it, while the reworded line stays true, so
#                the implementer's own run there passes and no correction is due (k3).
#   correct      the reviewer's commit makes the sentence false another way — that the CLI strips
#                every space from the name, where it trims only the spaces around it — and the
#                workflow's comment still names a push trigger the file does not carry, which the
#                review App may not push, so its `COMMENT` review left one thread on the workflow.
#                Its base requires conversation resolution (resolution_protection_routes), so the
#                head is `BLOCKED` until an approval stands and that thread is resolved
#                (gh-threads.ts's `mergeStateOf`): the reviewer's approval of the head the
#                implementer's push made, once it has resolved the thread, is what ends the round
#                (daemon-standin.ts's `servedPhase`; run_threads_closed boots both halves in
#                `reviewing`).
#   code-defect  the change with the blank-name guard gone (GUARD `dropped`), an honest workflow
#                comment, no commit of the reviewer's, and one thread on the docs/cli.md sentence
#                the change adds, which says that a blank name exits 2: that sentence is false only
#                because the code is wrong, so no text fix closes the thread and it is a message to
#                the architect and the reviewer. It is the negative control: its base requires no
#                conversation resolution and its approved head is `CLEAN`, so the round ended on
#                the approval and the loop runs in `accepting` (run_threads_closed says why).
approved_world() {
  local index=$1 start=$2 guard=$3 case_name=$4 src when worker_pr worker_pr_url C1 body extra
  local threads entries threads_line reviews pr_head fix reworded=()
  # The two files the world is written with, and the threads the reviewer left, each with what
  # the reviewer wrote and what its PR body entry says still stands. Every world but the dropped
  # guard's documents the name as printed exactly as given, which the trim makes false; the
  # dropped guard's documents what R2 asks for, which only that guard makes false. Both are passed
  # to round_world, which takes no default, so each world's own text is written here rather than
  # left to what some other world does not override.
  local doc_line=$text_finding_doc_line trigger=$text_finding_trigger
  case $case_name in
  accept | crossing)
    trigger=''
    fix='It trims the spaces around the name.'
    # shellcheck disable=SC2016 # the backticks are the document's own markdown
    reworded=("$cli_example_line" 'Run `bun greet.ts Ada`, and it prints `Hello, Ada!`')
    threads='[]'
    ;;
  correct)
    fix='It strips every space from the name.'
    threads=$(jq -n '[
      {id:"PRRT_kwDOfix002", path:".github/workflows/greet.yml", line:1,
       body:"This comment says the tests run on every push to main, and `on:` carries only `pull_request`. The trigger is the one we want, so the comment is what to fix; the review App may not push a workflow file, so it is yours.",
       open:"the comment still names a push trigger the file does not carry"}
    ]')
    ;;
  code-defect)
    doc_line='A blank name, like no name at all, prints the usage line on stderr and exits 2.'
    trigger=''
    fix=''
    threads=$(jq -n '[
      {id:"PRRT_kwDOfix101", path:"docs/cli.md", line:5,
       body:"`bun greet.ts \"  \"` prints a greeting and exits 0, so this sentence is false. R2 of the issue asks for the usage line and exit 2 on a blank name.",
       open:"a blank name still prints a greeting, so the sentence does not hold"}
    ]')
    ;;
  *) fail "approved_world: '$case_name' is no case of threads-closed" ;;
  esac
  round_world "$index" "$start" "$guard" "$doc_line" "$trigger"
  pr_head=$C1
  [ -z "$fix" ] || pr_head=$(fixture_reviewer_commit "$doc_line" "$fix" "${reworded[@]}")
  # The section as the reviewer left it (skill://legion-worker/references/pr-body.md): the count
  # of what is open, then one entry per open thread, each stamped with the head the reviewer
  # reviewed. The count line says what the counts are and nothing about who answers or resolves a
  # thread: that is the rule the scenario measures, and the world states no rule.
  entries=$(jq -r --arg head "${pr_head:0:12}" \
    '.[] | "- Thread \(.id): still open — \(.open); unresolved at \($head)."' <<<"$threads")
  threads_line="**Threads:** 0 resolved, $(jq length <<<"$threads") unresolved."
  [ -z "$entries" ] || threads_line="$threads_line
$entries"
  printf '%s\n' "$threads" >"$R/threads.json"
  # The reviewer's reviews on GitHub, in the shape of the reviews list (production-shapes.json's
  # `gh api GET repos/fernbrook/widgets/pulls/{n}/reviews`, read 2026-11-17): in code-defect its
  # approval of the implementer's head; in correct the `COMMENT` review whose inline comment opened
  # the workflow's thread, at the reviewer's commit, as GitHub lists a `COMMENT` review whose body
  # is empty (review_world's bot review, `COMMENTED`); in accept and crossing none, since the
  # reviewer left no thread and has not approved.
  case $case_name in
  code-defect)
    reviews=$(jq -n --arg head "$pr_head" '[{id:5001, state:"APPROVED", commit_id:$head,
      author:{login:"legion-review[bot]"}, user:{login:"legion-review[bot]"},
      body:"Approved at this head. The open threads are text findings and are the work the merge waits for."}]')
    ;;
  correct)
    reviews=$(jq -n --arg head "$pr_head" '[{id:5001, state:"COMMENTED", commit_id:$head,
      author:{login:"legion-review[bot]"}, user:{login:"legion-review[bot]"}, body:""}]')
    ;;
  *) reviews='[]' ;;
  esac
  # The reviewer's handoff body, which the runner records on the issue at the version the agents'
  # tasks name: `head`, the commit it reviewed, and no note. A handoff holds its head and a short
  # note and nothing else (#5287): the decision and each finding are the reviews on GitHub, which
  # `reviews` holds, so a handoff of a round with no approval says no more than one with it.
  jq -n --arg head "$pr_head" '{head:$head}' >"$R/review.json"
  fixture_body "$threads_line" "$(fixture_checks implementer tester)"
  # A resolution succeeds, as GitHub answers a role with write access, and a reply goes through
  # the GraphQL mutation or the REST reply route, each answered from the threads as they stand
  # (gh-standin.ts's `threadState`); the threads the reviewer left open are the ones
  # $R/threads.json holds, which fixture_write's graph serves, each opened by an inline comment of
  # review 5001 (`review: 5001`). The create
  # of a review is the POST of the reviews path, chosen by the call's own verb (`methods`, read as
  # the score reads it, so a create written `-f commit_id=<head>` with no method is the create
  # `--method POST` is), and it stands ahead of fixture_write's own; the reviews already there are
  # REVIEWS, which fixture_write lists and reads by id. The
  # create's answer carries what is the world's — the id it gives every create, the URL, and the
  # user whose token the run calls with — while the state, the head it was created at and its body
  # are the call's own and the stand-in answers them (gh-standin.ts), since a world cannot know
  # whether the create it is answering named an event. Nothing here records what a run did: the
  # score reads the calls the stand-in recorded, never these answers.
  #
  # The submit of a review (`…/reviews/<id>/events`) is routed by no world: GitHub answers it from
  # the review's own state, and the gh stand-in answers it for every world of this rig
  # (gh-standin.ts's reviewSubmission) — the review itself for the one review a run left pending,
  # and 422 for every other, which is every review of this world, each submitted by the call that
  # created it. Measured on 2026-11-11 in threads-closed's head arm: a reviewer approved correctly
  # and then made that call again, the create route matched it, and the stand-in answered a review
  # object with no `commit_id` — an approval at no head, which GitHub never writes and which the
  # score then read as the submission that stands, failing a run whose reviewer had done its work.
  # A world that answers what GitHub refuses is the same defect as a record naming another
  # repository, and a refusal written here would have been one world's while the defect control
  # that passed on it was built by review_world.
  #
  # Accept's, correct's and crossing's bases require conversation resolution, and the negative
  # control's none (the header above says why each).
  local protection='[]'
  [ -n "$guard" ] || protection=$(resolution_protection_routes)
  extra=$(jq -n --arg url "$worker_pr_url" --argjson number "$worker_pr" --arg repo "$worker_repo" \
    --arg login "$world_login" --argjson protection "$protection" '
    ($repo | gsub("(?<c>[.+*?()|\\[\\]{}^$\\\\])"; "\\\(.c)")) as $repo_re
    | $protection + [
    {match:("^gh api .*pulls/" + ($number|tostring) + "/reviews([ ?]|$)"), methods:["POST"],
     endpoint:("repos/" + $repo_re + "/pulls/" + ($number|tostring) + "/reviews"),
     stdout:{id:5002, html_url:($url+"#pullrequestreview-5002"), user:{login:$login}}}
  ]')
  fixture_write "$extra" "$reviews" '{}' "$(jq '[.[] | {id, isResolved:false, path, line,
    review:5001, comments:[{author:{__typename:"Bot", login:"legion-review"}, body}]}]' <<<"$threads")"
}

# The steps the three worlds are built from. Each reads and sets the world function's own
# variables: $src and $when, the checkout and the date every commit of this world carries; $C1,
# the implementer's commit, which is the pull request's head; and $body, the pull request body.

# fixture_open INDEX START opens the run's remote and its checkout: the pull request's number and
# URL (1000 plus INDEX), the date of this world's commits (INDEX seconds after START), a bare
# remote whose post-receive hook logs every push, and $src on the world's default branch
# ($worker_base) with the README and the MIT license every world carries. The license is the one
# GitHub reads off the file and answers the repository with (rest-objects.ts's `repositoryJson`),
# as production's repository answers one (production-shapes.json's `repos/<repo>`, its
# `license` an object): a repository without one answered `license: null` where production's is
# an object.
fixture_open() {
  local index=$1 start=$2
  worker_pr=$((1000 + index))
  worker_pr_url=https://github.com/$worker_repo/pull/$worker_pr
  when="@$((start + index)) +0000"
  git init -q --bare -b "$worker_base" "$R/remote.git"
  cat >"$R/remote.git/hooks/post-receive" <<EOF
#!/bin/sh
while read old new ref; do echo "\$(date -u +%FT%T.%3NZ) \$ref \$old \$new" >>$(printf %q "$R/pushes.log"); done
EOF
  chmod +x "$R/remote.git/hooks/post-receive"
  src=$R/src
  git init -q -b "$worker_base" "$src"
  printf '# widgets\n\nSmall CLI helpers.\n' >"$src/README.md"
  printf '%s\n' 'MIT License' '' "Copyright (c) 2026 ${worker_repo%%/*}" '' \
    'Permission is hereby granted, free of charge, to any person obtaining a copy of this software and associated documentation files (the "Software"), to deal in the Software without restriction, including without limitation the rights to use, copy, modify, merge, publish, distribute, sublicense, and/or sell copies of the Software, and to permit persons to whom the Software is furnished to do so, subject to the following conditions:' '' \
    'The above copyright notice and this permission notice shall be included in all copies or substantial portions of the Software.' '' \
    'THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY, FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM, OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE SOFTWARE.' \
    >"$src/LICENSE"
}

# The world's git in $src: this world's date on every commit, and the identity a commit of $src
# carries, the repository's own developer ($world_author_name) or, where the caller names a role
# in $fixture_by, that role's App (role_identity). The identity goes in the variables git reads
# ahead of any `-c`, so the operator's own GIT_AUTHOR_* and GIT_COMMITTER_* never reach the
# world's history: on a Legion pod they are the pod's App, and the seeded commits of round 24's
# preflight carried it.
fixture_git() {
  local name=$world_author_name email=$world_author_email
  [ -z "${fixture_by:-}" ] || IFS=$'\t' read -r name email < <(role_identity "$fixture_by")
  GIT_AUTHOR_DATE=$when GIT_COMMITTER_DATE=$when GIT_AUTHOR_NAME=$name GIT_AUTHOR_EMAIL=$email \
    GIT_COMMITTER_NAME=$name GIT_COMMITTER_EMAIL=$email git -C "$src" -c commit.gpgsign=false "$@"
}

# docs/cli.md as it stands before the change this world's commit makes, which leaves it with one
# sentence the change makes false (fixture_round_files, which adds the change's own line below
# this one). Its sentence is how every command of the CLI is run and what it exits with, which
# greet.ts meets in every world but the two whose CLI greets a blank name, where it is false only
# because the code is wrong. It named `bun hello.ts`, a command no world holds, and two reviewers
# of round 24 found that third false sentence by reading, which no case intends.
fixture_docs_before() {
  mkdir -p "$src/docs"
  # shellcheck disable=SC2016 # the backticks are the document's own markdown
  printf '# The widgets CLI\n\n%s\n' 'Each command is a Bun script at the root of the repository, run as `bun <command>.ts`. A command exits 0 when it printed and 2 when it was given arguments it cannot use.' >"$src/docs/cli.md"
}

# fixture_first_commit commits what $src holds, pushes the world's default branch and opens
# legion/<key> on it: the base the implementer's own commit sits above, and the other side of the
# diff the gh stand-in serves. The world's GitHub holds the accounts its answers name
# ($R/accounts.json, read by rest-objects.ts's `accountsOf`): its developer's, as production's
# holds the account a base head's author is (production-shapes.json's `branches-main`, where
# `commit.author` and `commit.committer` are user objects), its login the local part of the
# developer's address with each dot a dash (GitHub's logins hold none), its id, and the address
# its commits carry; the organization the repository is in; and the bot of each role App
# (role_identity), its id and its noreply address, which every commit a role makes carries, so
# GitHub links those commits to it as it links production's, and the App it is, its id the
# world's own. Then the SSH key the developer's account signs with ($R/allowed-signers, in git's
# allowed-signers form), whose private half signs this commit, as production's base head is
# signed and GitHub verifies it (`commit.verification`). The commit's author and committer lines
# are the developer's as before (fixture_git); the signature is a header of its own.
# gh-standin.ts's `commitJson` reads both files.
#
# Once the base is pushed its ruleset protects it at the remote as GitHub protects production's
# `main` (base_ruleset_routes): every push that updates or deletes the base is refused. The remote
# hands every command to its proc-receive hook (receive.procReceiveRefs `refs/`), which refuses
# one on the base the way GitHub does — `remote:` lines naming each rule the push breaks, then
# ` ! [remote rejected] <src> -> <base> (push declined due to repository rule violations)`, exit
# 1 — where a pre-receive hook can only make git print `(pre-receive hook declined)`, which no
# GitHub push prints. The rules a push breaks, in the ruleset's order: a deletion breaks
# `deletion` alone (`Cannot delete this branch`); an
# update breaks `non_fast_forward` where the base's tip is not an ancestor of the new one
# (`Cannot force-push to this branch`), `required_linear_history` where a commit it adds is a
# merge (`This branch must not contain merge commits.`, then the violations), and `pull_request`,
# `required_status_checks` and `merge_queue` whatever it carries, since the base changes only
# through a pull request the queue merges. The first line, the review link, `Changes must be made
# through a pull request.`, `Required status check "<name>" is expected.` (for one check) and
# `Changes must be made through the merge queue` are GitHub's as measured on 2026-11-12 (INFRA-2713's
# sandbox, a direct `git push origin HEAD:main` to a base whose ruleset required a pull request, a
# check and its merge queue); the lines of the other rules, the count line for several checks
# and the rejection line are GitHub's form as public reports of a rejected push print it,
# unmeasured here, since no read-only call makes GitHub refuse a push.
#
# The same hook refuses the push of an App without the `workflows` permission whose commits
# create, update or delete a file under `.github/workflows/`, as GitHub refuses it: `git push`
# prints ` ! [remote rejected] <src> -> <dst> (refusing to allow a GitHub App to create or update
# workflow `<path>` without `workflows` permission)`, with no `remote:` line, and exits 1, and
# the ref does not move. The reason is GitHub's words as the review App's refused pushes printed
# them (docs/solutions/github/moving-a-package-between-jobs-moves-its-reasons-too-and-only-the-
# implementer-pushes-a-workflow-file-pr2516.md, and the LEGION-779 page under docs/solutions/
# testing/ it extends); the line around them is git's for a ref a hook refuses. The App is the
# one the push reached the remote as (GITHUB_APP, which the world's remote helper sets,
# world_remote_app), and the implement App alone holds `workflows` (packages/daemon/internal/
# appauth's writePermissions; scripts/github-apps/implement.json and review.json). A push that
# carries no App, the world's build's own, is taken. What touches a workflow is GitHub's rule as
# its documentation gives it (docs.github.com, read 2026-11-19): the `workflow` scope "Grants the
# ability to add and update GitHub Actions workflow files. Workflow files can be committed without
# this scope if the same file (with both the same path and contents) exists on another branch in
# the same repository" (Scopes for OAuth apps), and an App's `workflows` permission is asked of a
# workflow file's delete as of its write (Permissions required for GitHub Apps, "Workflows": `PUT`
# and `DELETE /repos/{owner}/{repo}/contents/{path}`). So a path under `.github/workflows/`
# counts where a commit of the push that is on no ref of the remote yet creates, changes or
# deletes it and its content at that commit is on no branch the remote holds; a merge that takes
# one side's version of the path counts for nothing (`git diff-tree -c` names only a path a merge
# leaves unlike every parent). Unmeasured: a deletion pushed, since the delete above is the
# contents API's; which path the line names when a push touches several (here, the first in byte
# order); and the order of the two refusals, where a push breaks both: the workflow line alone,
# since GitHub refuses a token what its permission does not grant before it reads a ruleset.
#
# Every command the hook takes it applies itself, as git leaves a proc-receive hook its commands'
# updates (`git update-ref` with the old value the client sent), and the post-receive hook logs
# it in pushes.log as before; a refused one is applied and logged nowhere.
fixture_first_commit() {
  local login=${world_author_email%%@*}
  rm -f "$R/signing-key" "$R/signing-key.pub"
  ssh-keygen -q -t ed25519 -N '' -C "$world_author_email" -f "$R/signing-key"
  printf '%s %s\n' "$world_author_email" "$(cut -d' ' -f1,2 "$R/signing-key.pub")" >"$R/allowed-signers"
  jq -n --arg login "${login//./-}" --arg email "$world_author_email" --arg org "${worker_repo%%/*}" \
    --arg implementer "$(role_identity implementer)" --argjson implementer_id "$world_implement_app_id" \
    --arg reviewer "$(role_identity reviewer)" --argjson reviewer_id "$world_review_app_id" '
    def bot($identity; $id; $app): ($identity | split("\t")) as [$name, $address]
      | {login:$name, id:$id, email:$address, app:{id:$app, slug:($name | rtrimstr("[bot]"))}};
    [{login:$login, id:318400017, email:$email}, {login:$org, id:318399944, type:"Organization"},
     bot($implementer; $implementer_id; 2101307), bot($reviewer; $reviewer_id; 2101311)]' >"$R/accounts.json"
  fixture_git add -A &&
    fixture_git -c gpg.format=ssh -c user.signingkey="$R/signing-key" commit -S -qm "init"
  fixture_git push -q "$R/remote.git" "$worker_base"
  local checks workflows
  IFS=$'\t' read -r workflows _ < <(role_identity implementer)
  checks=$(base_ruleset_routes | jq -r '.[] | select(.match | test("/rules/branches/")) | .stdout[]
    | select(.type == "required_status_checks") | .parameters.required_status_checks
    | if length == 1 then "Required status check \"\(.[0].context)\" is expected."
      else "\(length) of \(length) required status checks are expected." end')
  {
    printf '#!/bin/sh\nrepo=%q base=%q checks=%q workflows=%q\n' "$worker_repo" "$worker_base" "$checks" "${workflows%'[bot]'}"
    cat <<'EOF'
pkt() { len=$(head -c 4) && [ -n "$len" ] && [ "$len" != 0000 ] && line=$(head -c $((0x$len - 4)) | tr '\0' ' '); }
out() { printf '%04x%s\n' $((${#1} + 5)) "$1"; }
while pkt; do :; done
out version=1
printf 0000
set --
while pkt; do set -- "$@" "$line"; done
zero=0000000000000000000000000000000000000000
held() (
  blob=$(git rev-parse -q --verify "$1:$2") || exit 1
  for tip in $(git for-each-ref --format='%(objectname)' refs/heads/); do
    [ "$(git rev-parse -q --verify "$tip:$2")" != "$blob" ] || exit 0
  done
  exit 1
)
workflow() (
  for commit in $(git rev-list "$1" --not --all); do
    git diff-tree -r -c --root --no-commit-id --name-only "$commit" -- .github/workflows/ |
      while read -r path; do held "$commit" "$path" || printf '%s\n' "$path"; done
  done | LC_ALL=C sort | head -n 1
)
for command; do
  old=${command%% *} new=${command#* } ref=${command##* }
  new=${new%% *}
  path=
  [ "$new" = "$zero" ] || [ -z "${GITHUB_APP:-}" ] || [ "$GITHUB_APP" = "$workflows" ] || path=$(workflow "$new")
  if [ -n "$path" ]; then
    out "ng $ref refusing to allow a GitHub App to create or update workflow \`$path\` without \`workflows\` permission"
    continue
  fi
  if [ "$ref" != "refs/heads/$base" ]; then
    if [ "$new" = "$zero" ]; then
      git update-ref -d "$ref" "$old"
    else
      git update-ref "$ref" "$new" "$old"
    fi && out "ok $ref" || out "ng $ref failed to update ref"
    continue
  fi
  {
    printf 'error: GH013: Repository rule violations found for %s.\n' "$ref"
    printf 'Review all repository rules at http://github.com/%s/rules?ref=refs%%2Fheads%%2F%s\n\n' "$repo" "$base"
    if [ "$new" = "$zero" ]; then
      printf -- '- Cannot delete this branch\n\n'
    else
      if [ "$old" != "$zero" ] && ! git merge-base --is-ancestor "$old" "$new"; then
        printf -- '- Cannot force-push to this branch\n\n'
      fi
      if [ "$old" = "$zero" ]; then
        merges=$(git rev-list --merges "$new" --not --all)
      else
        merges=$(git rev-list --merges "$new" "^$old")
      fi
      if [ -n "$merges" ]; then
        count=$(printf '%s\n' "$merges" | wc -l)
        [ "$count" = 1 ] && found='1 violation' || found="$count violations"
        printf -- '- This branch must not contain merge commits.\n  Found %s:\n\n' "$found"
        printf '  %s\n' $merges
        printf '\n'
      fi
      printf -- '- Changes must be made through a pull request.\n\n- %s\n\n' "$checks"
      printf -- '- Changes must be made through the merge queue\n\n'
    fi
  } >&2
  out "ng $ref push declined due to repository rule violations"
done
printf 0000
EOF
  } >"$R/remote.git/hooks/proc-receive"
  chmod +x "$R/remote.git/hooks/proc-receive"
  git -C "$R/remote.git" config receive.procReceiveRefs refs/
  fixture_git checkout -qb "legion/$worker_key"
}

# fixture_cli [dropped] writes the CLI under test and its tests. The command trims its argument
# once, as the spec's Goal places the trim, and greet() greets what it is handed. Its tests run
# greet() and the command itself, one test for each requirement: R1's greeting and exit 0, R3's
# trim, and R2's usage line and exit 2 for no name and for a blank one, so no requirement is left
# without a test (round 24: 16 of 46 reviews named R2's missing test).
#
# `dropped` drops the blank-name guard R2 asks for: the command then tests only for a missing
# argument, so a name of spaces trims to nothing and the CLI greets nobody, and its tests leave out
# the blank name, as the issue's check does (seed.ts tester-proof's `no-name`). The missed test
# and the missed check are part of that world's one intended defect, not findings of their own:
# an implementer who forgot the blank name wrote neither, which is how the defect reached review
# with every recorded pass and every check green (the architect's ruling of 2026-11-16). Do not
# give that world the blank-name test: it would fail at the head whose checks report success.
fixture_cli() {
  local guard=$1 blank_name='name === undefined || name === ""'
  [ -z "$guard" ] || blank_name='name === undefined'
  cat >"$src/greet.ts" <<EOF
export function greet(name: string): string {
  return \`Hello, \${name}!\`;
}

if (import.meta.main) {
  const name = Bun.argv[2]?.trim();
  if ($blank_name) {
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
  expect(cli("  Ada  ")).toEqual({ exitCode: 0, stdout: "Hello, Ada!\n", stderr: "" });
});

test("prints the greeting and exits 0", () => {
  expect(cli("Ada")).toEqual({ exitCode: 0, stdout: "Hello, Ada!\n", stderr: "" });
});

test("prints the usage line and exits 2 when no name is given", () => {
  expect(cli()).toEqual({ exitCode: 2, stdout: "", stderr: "usage: greet.ts <name>\n" });
});
EOF
  [ -n "$guard" ] || cat >>"$src/greet.test.ts" <<'EOF'

test("prints the usage line and exits 2 when the name is blank", () => {
  expect(cli("   ")).toEqual({ exitCode: 2, stdout: "", stderr: "usage: greet.ts <name>\n" });
});
EOF
  cat >>"$src/greet.test.ts" <<'EOF'

/** `bun greet.ts <args>`, run in this directory: its exit code and what it printed. */
function cli(...args: string[]) {
  const run = Bun.spawnSync([process.execPath, "greet.ts", ...args], { cwd: import.meta.dir });
  return { exitCode: run.exitCode, stdout: run.stdout.toString(), stderr: run.stderr.toString() };
}
EOF
}

# fixture_round_files DOC_LINE TRIGGER writes the two files a round after the implementer reads:
# docs/cli.md's line for the command, after the sentence fixture_docs_before wrote, whose last
# sentence is DOC_LINE, then the example line for the command the issue's check runs
# ($cli_example_line), and the workflow, whose comment names TRIGGER beside the pull request
# trigger `on:` carries.
fixture_round_files() {
  local doc_line=$1 workflow_trigger=$2
  # shellcheck disable=SC2016 # the backticks are the document's own markdown
  printf '\n`bun greet.ts <name>` prints `Hello, <name>!`. %s\n\n%s\n' "$doc_line" "$cli_example_line" >>"$src/docs/cli.md"
  mkdir -p "$src/.github/workflows"
  # The workflow installs bun before it runs the tests: bun is on no GitHub runner, so without
  # that step `bun` is absent and the job can never pass, which is a finding whose closing changes
  # code that runs. The scenario's premise is a change carrying text findings and no behavioural
  # one, so the world must not carry that defect; the comment on the file's first line, which
  # names a trigger the file does not have, is the text finding the run is there to find. That
  # is said here and never in the file: the file is the world's, and the same sentences written
  # into it told every agent that read it what it was there to find (review 5887659447, finding
  # 2). Only text closes that finding, because the issue's spec says when CI runs: on pull
  # requests alone, never on a push to `main` (world-issue.ts's WORLD_SPEC, Context), so adding
  # the push trigger the comment names breaks the spec and correcting the comment is the close.
  #
  # Nothing else of the workflow is a finding, in any world: each action is pinned by digest, as
  # this repository pins them, the token is read-only (`permissions`), and the job runs the whole
  # suite rather than one file. Round 24's reviewers named the tag pins and the missing
  # `permissions` in 42 of 46 reviews, and the one named file in 3, none of which a case intends.
  cat >"$src/.github/workflows/greet.yml" <<EOF
# Runs the greet tests on every pull request$workflow_trigger.
name: greet
on:
  pull_request:
permissions:
  contents: read
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7.0.1
      - uses: oven-sh/setup-bun@0c5077e51419868618aeaa5fe8019c62421857d6 # v2.2.0
      - run: bun test
EOF
}

# fixture_head_commit commits the implementer's change as $C1, pushes legion/<key> and empties
# the push log, so the only pushes it holds are the run's own: the fixture's own two pushes are
# the world being built and no act of the run. It creates nothing else. The run's other two
# records — the gh and bun stand-ins' calls (calls.jsonl) and the daemon stand-in's answers
# (daemon.log) — are each opened by the stand-in that keeps it, with one line of its own that
# says it was there and records no act: the gh stand-in's when a worker run boots (worker_boot),
# the daemon stand-in's when it starts listening. So the score can tell a record holding that
# line alone, which is the run that called nothing or moved nothing, from a record that is not
# there at all, which is one nothing ever wrote (score.ts's `unscored`: a missing record is a
# rig error naming it, a record with no act in it a reading of the run the row names).
# A file this function created would say neither: the audit of 2026-11-10 deleted all three
# from a recorded failing run and it scored a pass, and a code-defect head whose gh stand-in
# never ran passed on an empty calls.jsonl, which was the rig's own file and no evidence that
# the stand-in had ever run. The push log stays emptied because the post-receive hook has
# already written the fixture's own pushes into it, so the empty file is the reading "no push",
# which is what a review round leaves.
fixture_head_commit() {
  fixture_git add -A &&
    fixture_by=implementer fixture_git commit -qm "feat: greet(), a greeting helper for the widgets CLI ($worker_key)"
  C1=$(fixture_git rev-parse HEAD)
  fixture_git push -q "$R/remote.git" "legion/$worker_key"
  : >"$R/pushes.log"
}

# fixture_reviewer_commit DOC_LINE FIX [LINE TEXT]... rewrites the sentence DOC_LINE of docs/cli.md
# as FIX, and each further LINE as its TEXT, and nothing else, commits it as the review App
# (role_identity) and pushes it onto legion/<key>, which the run's remote takes since it changes no
# workflow file, and prints the commit: the reviewer's own text commit above the implementer's
# (approved_world). The push log is emptied of the world's own push, as fixture_head_commit
# empties it of the implementer's.
fixture_reviewer_commit() {
  local doc
  doc=$(<"$src/docs/cli.md")
  while [ $# -ge 2 ]; do
    doc=${doc/"$1"/"$2"}
    shift 2
  done
  printf '%s\n' "$doc" >"$src/docs/cli.md"
  fixture_by=reviewer fixture_git commit -qam "docs: say what the CLI does with the spaces around the name"
  fixture_git push -q "$R/remote.git" "legion/$worker_key"
  : >"$R/pushes.log"
  fixture_git rev-parse HEAD
}

# fixture_implement writes the implement handoff as the implementer records it with `dispatch
# handoff-write`: `head`, the commit the implementer pushed, and nothing else, since the record
# holds the issue and the phase and Dispatch records when it was written (#5287). What the
# implementer changed is the diff (seed.ts plan reads its files from the remote) and what it ran
# is no part of it: seed.ts implement-handoff records that as a result against the issue's check
# k1. The PR's head is the implementer's code commit, since no handoff commit sits above it.
fixture_implement() {
  jq -n --arg head "$C1" '{head:$head}' >"$R/implement.json"
}

# fixture_test_handoff writes the tester's handoff body at $R/test.json, as the tester wrote it
# before this round, in the shape main's tester role gives it (score.ts's TestHandoff): `head`,
# the commit it tested, its verdict on the implementer's results and its failures, none. The
# runner records it on the issue beside the implementer's.
fixture_test_handoff() {
  jq -n --arg head "$C1" '
    {head:$head,
     implementerProof:{verdict:"verified", how:("ran `bun greet.ts Ada` at " + ($head | .[0:12]))},
     failures:[]}' >"$R/test.json"
}

# world_remote writes the transport the issue workspace's origin reaches the run's remote by, into
# the run's own directory, which the agent's namespace shows. Production's origin is
# https://github.com/<owner>/<repo>, and the world's is too, which `git remote -v` and `jj git
# remote list` print; url.<local>.insteadOf would route it, and both print the local path instead
# (measured on 2026-11-17, jj 0.45.1, git 2.47.3). So origin names a remote helper
# (remote.origin.vcs, fixture_workspace), $R/bin/git-remote-<vcs> on the pane's PATH (base_env),
# which git runs for every fetch and push of origin by its name, git's own and jj's alike: `git
# remote-ext` hands the call to git-upload-pack or git-receive-pack of $R/remote.git. It drops the
# variables of the caller's repository first (`git rev-parse --local-env-vars`: GIT_DIR,
# GIT_CONFIG_COUNT and its pairs, GIT_CONFIG_PARAMETERS and the rest), as git's own local transport
# drops them for the remote side: measured on 2026-11-18, the pane's core.hooksPath=/dev/null
# reached git-receive-pack through remote-ext, and the remote's post-receive, which writes
# pushes.log, never ran. protocol.ext.allow is set nowhere: git asks it of an ext:: URL, and none is
# one here (measured: a fetch and a push exit 0 without it). A call that names the URL itself
# (`git ls-remote https://github.com/<owner>/<repo>`) is no call of origin by its name: git takes
# it to github.com, and health calls it off the world.
#
# The helper is written for one role's App at a time (world_remote_app): the implement App's here,
# the App the workspace is provisioned with, and each booted role's by worker_boot.
#
# And $R/lib/git-core, the exec path the shared leak check's own git runs with (push_guard): a
# link to every program of git's own exec path but git-remote-https, which sends the world's own
# URL, with `.git` or without, to the same helper, and every other URL to git's own.
world_remote() {
  local real url=https://github.com/$worker_repo entry
  real=$(git --exec-path)
  mkdir -p "$R/bin" "$R/lib/git-core"
  world_remote_app implementer
  for entry in "$real"/*; do ln -s "$entry" "$R/lib/git-core/${entry##*/}"; done
  rm "$R/lib/git-core/git-remote-https"
  # shellcheck disable=SC2016 # the helper's own expansions
  printf '#!/bin/sh\ncase $2 in %q | %q) unset GIT_EXEC_PATH; exec %q "$@" ;; esac\nexec %q "$@"\n' \
    "$url" "$url.git" "$R/bin/git-remote-$world_remote_vcs" "$real/git-remote-https" \
    >"$R/lib/git-core/git-remote-https"
  chmod +x "$R/lib/git-core/git-remote-https"
}

# world_remote_app ROLE writes the issue workspace's remote helper ($R/bin/git-remote-<vcs>,
# world_remote) for ROLE: every fetch and push of origin reaches the run's remote as ROLE's App
# (role_identity's bot, its slug in GITHUB_APP), which the remote's proc-receive hook reads
# (fixture_first_commit). Production's GitHub reads which App pushed off the token the push
# presents, the token of the App the daemon gave the role (appauth's `AppRoleFor`), which `legion
# git-credential` answers from the gh bundle the daemon renders for the role's pane at its
# spawn. Measured on 2026-11-19 (git 2.47.3, jj 0.45.1) with a pre-receive hook that wrote what
# reached it, at a `git push origin` and a `jj git push` from a reviewer's pane: this helper ran
# for both, the parent of `git remote-ext` and so of git-receive-pack; no credential was asked,
# since the helper asks none; and the pane's environment arrived whole but for the repository
# variables the helper drops, LEGION_ROLE, GIT_AUTHOR_* and JJ_USER among it. A variable of that
# environment is the agent's to set for one command (`LEGION_ROLE=implementer git push`), which
# would give a reviewer the implement App's push with no credential at all, where production's
# reviewer would need the implement App's token; and the credential reaches the remote only if
# something reads the token, which no stand-in does (gh-standin.ts's PANE_GH_TOKEN_SHOWN). So the
# App is the helper's own, written for the role as the daemon renders the role's bundle, and set
# over whatever the caller's environment holds: to push as another App an agent must rewrite the
# world's transport. A push that names $R/remote.git itself, as the world's build pushes, reaches
# the remote with no App, and the remote takes it as a push of the repository's own developer.
world_remote_app() {
  local app remote=$R/remote.git
  IFS=$'\t' read -r app _ < <(role_identity "$1")
  # remote-ext splits its command at each space and reads `%` as a placeholder's start: `% ` and
  # `%%` are the literal two.
  remote=${remote//%/%%}
  # shellcheck disable=SC2016 # the helper's own expansions
  printf '#!/bin/sh\nunset $(git rev-parse --local-env-vars)\nexport GITHUB_APP=%q\nexec git remote-ext "$1" %q\n' \
    "${app%'[bot]'}" "git-%s ${remote// /% }" >"$R/bin/git-remote-$world_remote_vcs"
  chmod +x "$R/bin/git-remote-$world_remote_vcs"
}

# fixture_workspace makes $R/ws, the issue workspace every role of the run shares, as production's
# provisioning makes an issue pod's (packages/daemon/internal/workspace: provision.go's Provision,
# clone.go's feedClone, bookmark.go's createWorkspace), as the implement App, the App it
# provisions with (packages/daemon/internal/daemon's outbox `createBranch`); each role then adopts
# its working copy at its boot (adopt_workspace):
#   - the shared clone, $R/repos/github.com/<owner>/<repo> where a pod's is /legion/repos/github.com/
#     <owner>/<repo>: cloned from the run's remote with no working tree, as a pod's is from its feed
#     (`git clone --no-checkout`), its origin GitHub's URL with the world's helper (world_remote),
#     made jj's with no working copy of its own (`jj git init --colocate`, `jj sparse set --clear`,
#     then `jj abandon @`), the default branch tracked, and unreachable commits kept (`jj config
#     set --repo git.abandon-unreachable-commits false`, ensureFetchConfiguration);
#   - the workspace, added to it at the issue branch with `jj workspace add --sparse-patterns
#     empty`, which jj colocates as a git worktree, so $R/ws/.git is the file a pod's is (`gitdir:
#     <clone>/.git/worktrees/ws`), its creation recorded in .jj/legion-created as RecordCreated
#     writes it (an RFC 3339 instant in UTC, its fraction's trailing zeros dropped, as Go's
#     RFC3339Nano prints one), locked with provisioning's reason (lockGitWorktree), the clone's
#     .git/info/exclude given `.codegraph/` (excludeCodegraphDirectory) and then checked out (`jj
#     sparse reset`). Measured on 2026-11-18: `jj log`, `jj commit`, `jj git fetch` and `jj git
#     push`, and `git status`, `log`, `diff`, `show`, `fetch`, `push` and `ls-remote origin`,
#     each in the workspace as in a pod's;
#   - both of the worktree's links absolute and the clone's format a pod's, whatever the host's git:
#     jj adds the worktree with `git -c worktree.useRelativePaths=true worktree add`, which a pod's
#     git 2.47.3 does not know and git 2.48 and later honour: they write `gitdir: ../repos/…` and
#     `../../../../../../../ws/.git`, raise core.repositoryformatversion to 1 and set
#     extensions.relativeWorktrees (CI's git 2.56.1 at 427b02d4, and git 2.55.0 measured on
#     2026-11-18), where a pod's clone holds absolute paths, format 0 and no extension. So `git
#     worktree repair` with worktree.useRelativePaths=false writes both links absolute, the
#     extension is unset and the format set back to 0; a 2.47.3 leaves all of it as it was. Any
#     other form refuses the build (workspace_form);
#   - the repository's jj configuration where the agents' own jj reads it: jj 0.45 keeps it under
#     the configuration home of whoever runs jj, `$XDG_CONFIG_HOME/jj/repos/<.jj/repo/config-id>/`,
#     and a jj that finds none there prints `Per-repo config not found. Generating an empty one.`
#     on its first command, as every agent of the 35 runs recorded on 2026-11-17 was shown, since
#     jj wrote it into the operator's. A pod's workspace-init writes it into the configuration home
#     each role's is a copy of (runtime/sandbox's manifest.go, xdgConfigHome), so a role's jj
#     reads `trunk()` and `git.abandon-unreachable-commits = false` there and prints no such line.
#     So every jj here runs with the configuration home a worker pane gets, $R/state/home/.config
#     (the tmux runtime's xdgDirectories under the pane's LEGION_STATE_DIR, $R/state), which
#     adopt_workspace's jj runs with too, and none writes the operator's;
#   - the clone's git configuration, production's (configureRepositoryCredential,
#     configureCloneReads): its `local` scope, as the tester read a pod's with `git config -l
#     --show-scope`, holds production's keys in production's order and remote.origin.vcs. A `jj git
#     clone` made it before, which added core.ignorecase and core.precomposeunicode, keys no
#     pod's clone holds, and set no credential and no core.warnAmbiguousRefs. Its git reads none
#     of the operator's configuration, whose global file and GIT_CONFIG_COUNT pairs are no clone's.
#     The credential lines are text nothing of the world calls: the helper asks no credential, and
#     the leak check's own ls-remote reaches the run's remote (push_guard).
# It is isolated from the operator's own jj configuration (fsmonitor.backend = "watchman" on a
# devbox that runs it) by a bare JJ_CONFIG naming only fsmonitor.backend = "none", so jj falls back
# to its built-in defaults and no snapshot registers this run's workspace with the operator's
# long-running watchman; and from the operator's own identity by the App's (role_env).
fixture_workspace() {
  local jj_setup_config=$R/jj-setup.toml identity clone=$R/repos/github.com/$worker_repo jj git form
  mapfile -t identity < <(role_env implementer)
  printf '[fsmonitor]\nbackend = "none"\n' >"$jj_setup_config"
  jj=(env "${identity[@]}" JJ_CONFIG="$jj_setup_config" XDG_CONFIG_HOME="$R/state/home/.config" jj)
  git=(env -u GIT_CONFIG_COUNT -u GIT_CONFIG_PARAMETERS GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1 git)
  world_remote
  mkdir -p "${clone%/*}"
  {
    "${git[@]}" clone --quiet --no-checkout "$R/remote.git" "$clone" &&
      "${git[@]}" --git-dir="$clone/.git" remote set-url origin "https://github.com/$worker_repo" &&
      "${git[@]}" --git-dir="$clone/.git" config remote.origin.vcs "$world_remote_vcs" &&
      (cd "${clone%/*}" && "${jj[@]}" git init --colocate "$clone") &&
      (
        cd "$clone" &&
          "${jj[@]}" sparse set --clear --color=never -R "$clone" &&
          "${jj[@]}" abandon @ --color=never -R "$clone" &&
          "${jj[@]}" bookmark track "$worker_base" --remote origin --ignore-working-copy -R "$clone" &&
          "${jj[@]}" config set --repo git.abandon-unreachable-commits false --ignore-working-copy -R "$clone" &&
          "${jj[@]}" bookmark track "legion/$worker_key" --remote origin --ignore-working-copy -R "$clone" &&
          "${jj[@]}" workspace add "$R/ws" --name ws --sparse-patterns empty -R "$clone" \
            --revision "$("${jj[@]}" log -r "legion/$worker_key@origin" --no-graph -T commit_id --ignore-working-copy -R "$clone")"
      ) &&
      "${git[@]}" --git-dir="$clone/.git" -c worktree.useRelativePaths=false worktree repair "$R/ws" &&
      { ! "${git[@]}" --git-dir="$clone/.git" config --local --get extensions.relativeWorktrees ||
        { "${git[@]}" --git-dir="$clone/.git" config --local --unset extensions.relativeWorktrees &&
          "${git[@]}" --git-dir="$clone/.git" config --local core.repositoryformatversion 0; }; } &&
      printf '%sZ\n' "$(date -u +%FT%T.%N | sed -E 's/\.?0+$//')" >"$R/ws/.jj/legion-created" &&
      "${git[@]}" --git-dir="$clone/.git" worktree lock \
        --reason "legion workspace: its directory may be invisible to other processes sharing this clone" "$R/ws" &&
      "${git[@]}" --git-dir="$clone/.git" config --replace-all credential.helper "" &&
      "${git[@]}" --git-dir="$clone/.git" config --add credential.helper '!legion git-credential' &&
      "${git[@]}" --git-dir="$clone/.git" config --replace-all credential.https://github.com.helper "" &&
      "${git[@]}" --git-dir="$clone/.git" config --add credential.https://github.com.helper '!legion git-credential' &&
      "${git[@]}" --git-dir="$clone/.git" config credential.interactive false &&
      "${git[@]}" --git-dir="$clone/.git" config credential.useHttpPath true &&
      "${git[@]}" --git-dir="$clone/.git" config core.warnAmbiguousRefs false &&
      printf '.codegraph/\n' >>"$clone/.git/info/exclude" &&
      (cd "$R/ws" && "${jj[@]}" sparse reset)
  } >/dev/null 2>&1 || fail "fixture_workspace: the issue workspace $R/ws could not be made from $R/remote.git"
  form=$(workspace_form "$clone" "$R/ws")
  [ -z "$form" ] || fail "fixture_workspace: the issue workspace $R/ws is not in a pod's form: $form"
}

# workspace_form CLONE WS prints each way the issue workspace WS, a worktree of the shared clone
# CLONE, is not in the form a pod's is, one line each, and nothing when it is in it: `.git`
# reading `gitdir: ` and the real path of the clone's entry for it, that entry's gitdir the real
# path of WS's .git, and the clone's configuration holding core.repositoryformatversion 0 and no
# extension, as a pod's git 2.47.3 writes them (fixture_workspace).
workspace_form() {
  local clone=$1 ws=$2 entry line
  entry=$(realpath -m -- "$clone/.git/worktrees/${ws##*/}")
  line=$(cat -- "$ws/.git" 2>&1) || :
  [ "$line" = "gitdir: $entry" ] || printf "%s/.git reads '%s', not 'gitdir: %s'\n" "$ws" "$line" "$entry"
  line=$(cat -- "$entry/gitdir" 2>&1) || :
  [ "$line" = "$(realpath -m -- "$ws")/.git" ] || printf "%s/gitdir reads '%s', not '%s/.git'\n" "$entry" "$line" "$(realpath -m -- "$ws")"
  line=$(git config --file "$clone/.git/config" core.repositoryformatversion) || :
  [ "$line" = 0 ] || printf '%s/.git/config holds core.repositoryformatversion=%s, not 0\n' "$clone" "$line"
  { git config --file "$clone/.git/config" --get-regexp '^extensions\.' || :; } | sed "s|^|$clone/.git/config holds an extension, |"
}

# fixture_checks ROLE... prints the pull request body's `**Checks:**` section: one line per role
# that recorded a pass on the issue's one before_merge check at the implementer's commit, $C1,
# which is the check the implementer's seeded result and the tester's own run are against. Each
# line says what the role ran, which is what the check names: both halves of R2 in every world but
# the two whose CLI dropped the blank-name guard, whose check, and so whose runs, missed the blank
# name (fixture_cli), and, in crossing, whose check also compares the output with docs/cli.md's
# example line (case_check's `doc-example`), that comparison too. It reads the world function's
# own $guard, unset in the tester's world, and its $case_name.
fixture_checks() {
  local role separator='' r2='with no name exited 2 and printed the usage line'
  # shellcheck disable=SC2016 # the backticks are the pull request body's own markdown
  [ -n "${guard:-}" ] || r2='with no name and `bun greet.ts '"'"'   '"'"'` each exited 2 and printed the usage line'
  # shellcheck disable=SC2016 # the backticks are the pull request body's own markdown
  [ "${case_name:-}" != crossing ] || r2="$r2"', and `Hello, Ada!` is the greeting the example line of `docs/cli.md` gives for `bun greet.ts Ada`'
  for role in "$@"; do
    printf -- '%s- Check k1 (before_merge): pass at %s, recorded by %s. Ran `bun greet.ts '"'"'  Ada  '"'"'` and read `Hello, Ada!`, so the name is trimmed, and `bun greet.ts` %s.' \
      "$separator" "${C1:0:12}" "$role" "$r2"
    separator=$'\n'
  done
}

# fixture_body THREADS CHECKS leaves the pull request body the gh stand-in serves in $body:
# THREADS is its `Threads:` section and CHECKS its check lines (fixture_checks).
fixture_body() {
  body="Dispatch: $worker_key

Adds \`greet(name)\` and \`bun greet.ts <name>\`.

## Verification

$1

**Checks:**
$2

**Fast-follow:** none.

**Chain:** not stacked."
}

# fixture_write EXTRA REVIEWS writes the two files the run reads the world through: the gh
# stand-in's answers ($R/fixtures.json), EXTRA's routes ahead of the ones every world shares, and
# the world the scorer reads ($R/world.json), whose head is the PR's head, the implementer's code
# commit or the reviewer's text commit above it (below), and whose base is the branch the pull
# request is open against, the other side of the diff the gh stand-in serves from the run's own
# remote (gh-standin.ts).
#
# `gh pr list` is among the shared routes because the worker skill has a role with no pull
# request number find one that way (`gh pr list --repo <repo> --head legion/<KEY> --state open
# --json number`), and a 404 there reads to the agent as a branch with no pull request open on
# it. The world has exactly one open pull request and its head is `legion/<key>`. The two routes
# hold the world's own facts of it — its number, node id, URL, title, body, state, branches and
# the checks of a head no remote holds — and the gh stand-in answers each `gh pr view` and `gh pr
# list` from them and from what the run did, as gh reads a pull request (gh-standin.ts's
# `prRead`): its head, checks, review decision, merge state and mergeability, the last three
# computed from the base's rules as GitHub computes them (gh-threads.ts's `reviewDecisionOf`,
# `mergeStateOf`, `mergeableOf`), its reviews (the ones the world held, REVIEWS, and the run's),
# its conversation, the files and commits of its diff, each field `--json` names and gh's text
# without it, and gh's refusal of a pull request, a repository or a field the world has not got.
# So no review, comment, file, decision or merge state is written here: a copy frozen here
# answered `gh pr view --json reviews` in GitHub's REST shape, which gh never prints (INFRA-764
# round 25's agents read `.commit.oid` null), and a decision and a state frozen here read `CLEAN`
# before any review, where production's base, which requires an approval, reads
# `REVIEW_REQUIRED` and `BLOCKED` (INFRA-764's ruling of 2026-11-18).
#
# The base branch's reads are shared routes for the same reason: the merge gate prescribes two of
# them by name (skill://legion-worker/references/merge-gate.md, "the base branch's rulesets'
# required checks and its branch protection's"), and that rule has a refused read reported to the
# architect and never guessed, so a 404 stops a reviewer where GitHub would have answered it — the
# batch of 2026-11-10 holds 91 refused reads of this kind. Every world's base is protected as
# production's `main` is, by a repository ruleset alone holding production's seven rules
# (base_ruleset_routes, which a world's own routes stand ahead of where its ruleset requires
# review threads resolved: resolution_protection_routes): the ruleset requires an approval and the
# checks the head reports (concluded_checks, `test` and `lint`), so the gate's rulesets read
# prints them, and the base has no branch protection of the classic kind, so
# its branch read answers `protected: true` with that protection off and naming no check, and the
# classic protection's own endpoint, `…/branches/<base>/protection`, is GitHub's 403 to a role
# App, whose token may not read a repository's administration: `gh: Resource not accessible by
# integration (HTTP 403)`, GitHub's error body on stdout, exit 1. A world whose classic
# protection required the checks while its rulesets required none gave a reviewer that reads the
# rulesets alone no required check before its review, where production's ruleset names them
# (INFRA-764's ruling of 2026-11-17); a base that named no required check at all left reviewers
# writing that the world held no required-check configuration (INFRA-764 round 24,
# reviewer-text-findings-text-only-base-3 and defect-base-3). Each of these reads answers in the
# shape production's does (production-shapes.json, which gh-standin.test.ts holds them to), with
# the world's own values: the branch carries its `commit`, the base's head on the run's remote
# (gh-standin.ts's `branch` answer). The rules route is ahead of the branch route, whose regex
# matches the rules path too, and so is the protection route, which that same regex matches: a
# call on the protection endpoint answered from the branch route got the branch object, which is
# no answer to the call it made. The stand-in answers from the first route that matches the whole
# call.
#
# Every other read a role text of either label names, and every read the census saw an agent
# make, is a shared route too, each answered as GitHub answers it from the world's state
# (gh-routes.ts's WORLD_ROUTES, which rig.test.ts pins both ways against the texts): the
# repository itself, with its default branch; a commit's combined status, its check runs, the
# workflow runs and a run's jobs, four GitHub routes with four shapes, where one regex over
# `check-runs|status` once answered the first three with the check-runs list, so a read of the
# statuses or the runs failed on a shape GitHub never gives; the pull request's conversation and
# its review comments; a ref, a list of the refs and a list of the branches; a commit, the
# commits a ref reaches, a commit's git data, a tree, the tags, and the pull request's commits and
# files (any number, so a number the world holds no pull request of is GitHub's 404 of the list),
# each the run's remote's as it stands (the routes ahead of `contents`, each on the world's
# repository alone, so a read of another owner's repository the pane's token runs stays the
# world's 404; the commit route's regex leaves a commit's own
# endpoints, `…/check-runs`, `…/pulls` and the rest, to their routes or to none); a file or a
# directory at a ref; and GraphQL, whose queries the
# stand-in answers from the graph written here (gh-standin.ts's `graphAnswer`): the pull request,
# its review threads (THREADS), its timeline, the viewer, and the two mutations a role makes on a
# thread. A route answered from what the run did — the reviews it created, the comments it
# posted, the ref a commit status names, the commit whose check runs are read, the commit a file
# is read at, the commits, refs, trees and tags of its remote — carries `answer`, which names the
# stand-in's reading of the run's own record and
# remote; its `stdout` is what the world held before the run. The review threads are the run's
# too: the graph holds the world's own, and the stand-in answers every read of a thread or of a
# review comment, GraphQL or REST, with those threads and the effect of every call the run made
# on them (gh-standin.ts's `threadState`), and the two mutations' payloads from that same state,
# so the graph's `mutation` names the two GitHub serves a role and holds no answer of its own. The
# world holds no workflow run and no commit status: its checks are check runs of no Actions
# workflow, so the runs list and the statuses are empty, a run's jobs and `gh run view`/`gh run
# watch` are GitHub's and gh's refusals for a run that is not there (the stand-in's `run` answer
# prints gh's lines, the run's URL or gh's usage, as measured on 2026-11-18), which health.ts
# declares for every role (GH_DECLARED's NO_RUN) since a role that meets one meets production,
# and the combined status reads `pending`, as GitHub's does for a head with none. `gh auth status`
# is gh's text for the pane's own gh configuration (the stand-in's `auth-status` answer).
# world.json names the bot of each role App (`logins`, role_identity's names), which every
# answer naming the account a call's token acts as — the GraphQL viewer, the author of a review,
# a comment, a reply or a resolution the call makes — reads for the role that made the call
# (gh-threads.ts's `loginOf`), as GitHub answers each App's token as its own bot.
#
# A write is answered only where the world models it, each by a route of its own that names its
# verb (`methods`, read with the score's own reader: gh sends a call carrying a field or an
# `--input` as a POST, so a create written `-f commit_id=<head>`, `--method=POST`, `-XPOST` or in
# lower case is the create `--method POST` is) and the one path it writes, the world's repository
# and pull request read whole (`endpoint`, gh-call.ts's `routeFor`): the create of a review, its
# submit, its update (`PUT …/reviews/<id>`), the pull request's body (a PATCH, or `gh pr edit`
# with the body alone), an inline review comment, the REST reply under one, a comment on the
# conversation (the REST POST, or `gh pr comment`) and the two GraphQL mutations the graph holds.
# Each is answered as GitHub answers it from the world's state (gh-standin.ts): the create with
# the review (id 6001 for the run's first, where a world names no create route of its own, and
# the next id for each create after it; its URL; the bot of the App the call's role acts as),
# whose state, head and body the stand-in adds from the call (`reviewCreate`, `reviewRead`); the
# PATCH with the pull request carrying the body it left. Every
# other write — another path, another verb on a read's path (each read route takes GET alone), a
# mutation the graph holds no answer for, `gh pr review`, `ready`, `merge`, `close`, `create` and
# an edit of anything but the body — matches no route and is refused as GitHub refuses an
# endpoint it does not serve, `gh: Not Found (HTTP 404)`, which `rig.sh health` names (its
# unmodelled-write rule). One route answered every POST, PATCH and PUT no route above it took
# with `{id:6001, html_url}` and exit 0, and changed nothing: 23 recorded PATCH calls read `.body`
# back from it as null (review 5887659447, finding 6), and in INFRA-764 round 25 a reviewer's
# update of its review's body (`PUT …/reviews/5002`, runs/1e2a0aeced4b, calls.jsonl:23) was
# answered so, the body stayed as it was, and no rule saw it, since a route had matched.
#
# fixture_write EXTRA REVIEWS [MERGE_STATES] writes MERGE_STATES, a JSON object from a head's SHA
# to the merge state GitHub reports for the pull request while it is at that head for a reason no
# rule of its base decides (GitHub's names: `CLEAN`, `BEHIND`, `DIRTY`, …), beside the routes as
# `mergeStates`, and every head it names none for is `CLEAN` (gh-call.ts's `mergeStateOn`). The
# base's rules hold that state `BLOCKED` while one of them is unmet — no approval standing, a
# required check or the code-scanning tool's check not passed on the head, a thread open where
# the base requires them resolved — from the reviews, checks and threads as the run leaves them
# (gh-threads.ts's `mergeStateOf`), so no world writes the `BLOCKED` its rules cause. The pull
# request's own answers carry the state of the head the run's remote is at (gh-standin.ts), and
# the daemon stand-in moves the record on that same state as main's engine moves on
# `record.MergeState` (daemon-standin.ts's `servedPhase`).
# fixture_write EXTRA REVIEWS MERGE_STATES [THREADS] writes THREADS, the pull request's
# review threads, each given as `id`, `isResolved`, `path`, `line`, `review` (the id of the
# review whose inline comment opened it, one of REVIEWS) and `comments`, each with its `author`
# and `body`, into the graph in the shape GitHub's GraphQL answers them: each comment carries its
# node id, its `databaseId` — the id of the same comment in GitHub's REST API, which the REST
# reply route names — its path, its line, the commit it is on, the review it belongs to and, for
# every comment after the first, the one it replies to. The comments are numbered from 7001 in
# the order given, and the time they were made is this world's ($when). A world with none passes
# none. A thread whose comment carried no `databaseId` and no REST comment left reviewers writing
# that the thread was a synthetic one no REST reply could reach (INFRA-764 round 24,
# reviewer-text-findings-defect-base-1, -2 and -3). The pull request's head is the world's
# $pr_head where it sets one, the reviewer's text commit above the implementer's (approved_world),
# and the implementer's commit $C1 otherwise.
fixture_write() {
  jq -n --arg head "${pr_head:-$C1}" --arg body "$body" --arg url "$worker_pr_url" --arg ref "legion/$worker_key" \
    --arg login "$world_login" --arg when "${when:-}" \
    --arg key "$worker_key" --arg repo "$worker_repo" --arg base "$worker_base" \
    --argjson number "$worker_pr" --argjson extra "$1" \
    --argjson reviews "$2" --argjson runs "$(concluded_checks)" \
    --argjson ruleset "$(base_ruleset_routes)" \
    --argjson states "${3:-"{}"}" --argjson threads "${4:-"[]"}" '
    ($number|tostring) as $n
    | ($repo | split("/")) as [$owner, $name]
    | ($repo | gsub("(?<c>[.+*?()|\\[\\]{}^$\\\\])"; "\\\(.c)")) as $repo_re
    | (if $when == "" then null else ($when | ltrimstr("@") | split(" ")[0] | tonumber | todate) end) as $at
    | ("https://api.github.com/repos/" + $repo + "/branches/" + $base + "/protection") as $protection_url
    | (reduce $threads[] as $thread ({next:7001, out:[]};
        .next as $first
        | .out += [($thread | del(.review)) + {__typename:"PullRequestReviewThread",
            isOutdated:false, isCollapsed:$thread.isResolved, originalLine:$thread.line,
            startLine:null, originalStartLine:null, diffSide:"RIGHT", startDiffSide:null,
            subjectType:"LINE", resolvedBy:null, viewerCanResolve:($thread.isResolved | not),
            viewerCanUnresolve:$thread.isResolved, viewerCanReply:true,
            comments:[$thread.comments | to_entries[] | (.key + $first) as $id | .value + {
              __typename:"PullRequestReviewComment", id:("PRRC_kwDOAAAAAc4AAC" + ($id|tostring)),
              databaseId:$id, fullDatabaseId:($id|tostring), authorAssociation:"NONE",
              path:$thread.path, line:$thread.line, originalLine:$thread.line,
              url:($url + "#discussion_r" + ($id|tostring)),
              createdAt:$at, updatedAt:$at, publishedAt:$at, state:"SUBMITTED", outdated:false,
              isMinimized:false,
              replyTo:(if .key == 0 then null
                else {id:("PRRC_kwDOAAAAAc4AAC" + ($first|tostring)), databaseId:$first} end),
              commit:{oid:$head}, originalCommit:{oid:$head},
              pullRequestReview:(if $thread.review == null then null
                else {id:("PRR_kwDOAAAAAc4AAE" + ($thread.review|tostring)), databaseId:$thread.review} end)}]}]
        | .next += ($thread.comments | length))
      | .out) as $seeded
    | {number:$number, id:("PR_kwDOAAAAAc4AAA" + $n), url:$url,
     title:("feat: greet() (" + $key + ")"), state:"OPEN", isDraft:false, isCrossRepository:false,
     headRefName:$ref, headRefOid:$head, baseRefName:$base, body:$body, mergedAt:null,
     autoMergeRequest:null,
     statusCheckRollup:[$runs[] | {name, status:(.status|ascii_upcase), conclusion:(.conclusion|ascii_upcase)}]} as $pr
    | {id:1, node_id:"R_kgDOAAAAAQ", name:$name, full_name:$repo, owner:{login:$owner, type:"Organization"},
       private:false, visibility:"public", fork:false, archived:false, disabled:false, description:null,
       language:"TypeScript", default_branch:$base,
       allow_squash_merge:true, allow_merge_commit:true, allow_rebase_merge:true,
       allow_auto_merge:false, delete_branch_on_merge:false,
       squash_merge_commit_title:"COMMIT_OR_PR_TITLE", squash_merge_commit_message:"COMMIT_MESSAGES",
       merge_commit_title:"MERGE_MESSAGE", merge_commit_message:"PR_TITLE",
       permissions:{admin:false, maintain:false, push:true, triage:true, pull:true}} as $repository
    | ("repos/" + $repo_re + "/pulls/" + $n) as $pulls
    | {mergeStates:$states, routes:($extra + [
      {match:"^gh pr view", stdout:$pr},
      {match:"^gh pr list", stdout:[$pr]},
      {match:"^gh pr edit( |$)", stdout:$url},
      {match:"^gh pr comment( |$)", answer:"pr-comment"},
      {match:"^gh api .*graphql", endpoint:"graphql", graph:{
        query:{viewer:{login:$login},
          repository:{__args:{owner:$owner, name:$name}, name:$name, nameWithOwner:$repo,
            defaultBranchRef:{name:$base},
            pullRequest:{__args:{number:$number}, reviewThreads:$seeded, timelineItems:[],
              isInMergeQueue:false, mergeQueueEntry:null}}},
        mutation:{resolveReviewThread:{}, addPullRequestReviewThreadReply:{}}}},
      {match:("^gh api .*pulls/" + $n + "/reviews([ ?]|$)"), methods:["POST"], endpoint:($pulls + "/reviews"),
       stdout:{id:6001, html_url:($url+"#pullrequestreview-6001"), user:{login:$login}}},
      {match:("^gh api .*pulls/" + $n + "/reviews/[0-9]+/events([ ?]|$)"), methods:["POST"],
       endpoint:($pulls + "/reviews/[0-9]+/events"), answer:"review-submit"},
      {match:("^gh api .*pulls/" + $n + "/reviews/[0-9]+([ ?]|$)"), methods:["PUT"],
       endpoint:($pulls + "/reviews/[0-9]+"), answer:"review-update", stdout:$reviews},
      {match:("^gh api .*pulls/" + $n + "([ ?]|$)"), methods:["PATCH"], endpoint:$pulls, answer:"pull-request",
       stdout:$pr},
      {match:("^gh api .*pulls/" + $n + "/comments([ ?]|$)"), methods:["POST"], endpoint:($pulls + "/comments"),
       answer:"review-comment"},
      {match:("^gh api .*pulls/" + $n + "/comments/[0-9]+/replies([ ?]|$)"), methods:["POST"],
       endpoint:($pulls + "/comments/[0-9]+/replies"), answer:"comment-reply"},
      {match:("^gh api .*issues/" + $n + "/comments([ ?]|$)"), methods:["POST"],
       endpoint:("repos/" + $repo_re + "/issues/" + $n + "/comments"), answer:"conversation-comment"},
      {match:("^gh api .*pulls/" + $n + "/reviews/[0-9]+/comments([ ?]|$)"), methods:["GET"], answer:"review-comments"},
      {match:("^gh api .*pulls/" + $n + "/reviews/[0-9]+([ ?]|$)"), methods:["GET"], answer:"review", stdout:$reviews},
      {match:("^gh api .*pulls/" + $n + "/reviews([ ?]|$)"), methods:["GET"], answer:"reviews", stdout:$reviews},
      {match:("^gh api .*pulls/" + $n + "/comments([ ?]|$)"), methods:["GET"], answer:"review-comments"},
      {match:("^gh api .*issues/" + $n + "/comments([ ?]|$)"), methods:["GET"], answer:"issue-comments", stdout:[]},
      $ruleset[],
      {match:("^gh api .*/branches/" + $base + "/protection"), methods:["GET"],
       stdout:{message:"Resource not accessible by integration",
         documentation_url:"https://docs.github.com/rest/branches/branch-protection#get-branch-protection",
         status:"403"},
       stderr:"gh: Resource not accessible by integration (HTTP 403)\n", exit:1},
      {match:("^gh api .*/branches/" + $base), methods:["GET"], answer:"branch",
       stdout:{name:$base,
         _links:{self:("https://api.github.com/repos/" + $repo + "/branches/" + $base),
           html:("https://github.com/" + $repo + "/tree/" + $base)},
         protected:true,
         protection:{enabled:false, required_status_checks:{enforcement_level:"off", contexts:[], checks:[]}},
         protection_url:$protection_url}},
      {match:"^gh api .*commits/[^/ ]+/check-runs([ ?]|$)", methods:["GET"], answer:"check-runs",
       stdout:{total_count:($runs|length), check_runs:$runs}},
      {match:"^gh api .*commits/[^/ ]+/status([ ?]|$)", methods:["GET"], answer:"combined-status", stdout:{statuses:[]}},
      {match:"^gh api .*actions/runs/[0-9]+/jobs([ ?]|$)", methods:["GET"],
       stdout:{message:"Not Found",
         documentation_url:"https://docs.github.com/rest/actions/workflow-jobs#list-jobs-for-a-workflow-run",
         status:"404"},
       stderr:"gh: Not Found (HTTP 404)\n", exit:1},
      {match:"^gh api .*actions/runs([ ?]|$)", methods:["GET"], stdout:{total_count:0, workflow_runs:[]}},
      {match:("^gh api .*repos/" + $repo_re + "/git/ref/"), methods:["GET"], answer:"git-ref"},
      {match:("^gh api .*repos/" + $repo_re + "/git/refs([/?][^ ]*)?( |$)"), methods:["GET"], answer:"git-ref"},
      {match:("^gh api .*repos/" + $repo_re + "/git/commits/[^/ ?]+([ ?]|$)"), methods:["GET"], answer:"git-commit"},
      {match:("^gh api .*repos/" + $repo_re + "/git/trees/[^ ?]+([ ?]|$)"), methods:["GET"], answer:"git-tree"},
      {match:("^gh api .*repos/" + $repo_re + "/commits([?][^ ]*)?( |$)"), methods:["GET"], answer:"commits"},
      {match:("^gh api .*repos/" + $repo_re + "/commits/(?![^ ?]*/(check-runs|check-suites|status|statuses|comments|pulls|branches-where-head)([ ?]|$))[^ ?]+([ ?]|$)"),
       methods:["GET"], answer:"commit"},
      {match:("^gh api .*repos/" + $repo_re + "/branches([?][^ ]*)?( |$)"), methods:["GET"], answer:"branches"},
      {match:("^gh api .*repos/" + $repo_re + "/tags([?][^ ]*)?( |$)"), methods:["GET"], answer:"tags"},
      {match:("^gh api .*repos/" + $repo_re + "/pulls/[0-9]+/commits([?][^ ]*)?( |$)"), methods:["GET"], answer:"pull-commits"},
      {match:("^gh api .*repos/" + $repo_re + "/pulls/[0-9]+/files([?][^ ]*)?( |$)"), methods:["GET"], answer:"pull-files"},
      {match:"^gh api .*repos/[^/ ]+/[^/ ]+/contents([/?][^ ]*)?( |$)", methods:["GET"], answer:"contents"},
      {match:("^gh api .*pulls/" + $n + "([ ?]|$)"), methods:["GET"], answer:"pull-request", stdout:$pr},
      {match:("^gh api (.* )?/?repos/" + $repo_re + "([?][^ ]*)?( |$)"), methods:["GET"], answer:"repository",
       stdout:$repository},
      {match:"^gh api (.* )?/?user([/?][^ ]*)?( |$)", methods:["GET"],
       stdout:{message:"Resource not accessible by integration",
         documentation_url:"https://docs.github.com/rest/users/users#get-the-authenticated-user",
         status:"403"},
       stderr:"gh: Resource not accessible by integration (HTTP 403)\n", exit:1},
      {match:"^gh auth status( |$)", methods:["GET"], answer:"auth-status"},
      {match:"^gh run (view|watch)( |$)", answer:"run", exit:1}
    ])}' >"$R/fixtures.json"
  jq -n --arg key "$worker_key" --arg repo "$worker_repo" --arg base "$worker_base" \
    --argjson pr "$worker_pr" --arg head "${pr_head:-$C1}" \
    --arg implementer "$(role_identity implementer)" --arg reviewer "$(role_identity reviewer)" \
    '{key:$key, repo:$repo, pr:$pr, branch:("legion/" + $key), base:$base, head:$head,
      logins:{implement:($implementer | split("\t")[0]), review:($reviewer | split("\t")[0])}}' >"$R/world.json"
}

# concluded_checks prints the check runs GitHub reports on a head whose CI has finished, in the
# shape of the check-runs route, each naming the App that ran it by id (gh-call.ts's `CheckRun`):
# `test` and `lint`, GitHub Actions' (15368), each completed with success, with the link of the
# workflow's job (`details_url`); and `CodeQL`, GitHub Advanced Security's (57789), the check that
# reports the code-scanning tool's analysis of the head, completed with success with the title
# and summary that App writes where the head brings no new alert, and no link of its own, since
# GitHub's page of the run is its link (gh-standin.ts's `reportedRuns`). Measured on 2026-11-18 on
# the heads of open cybermonklabs/legion pull requests: each carried a `CodeQL` check run of that
# App, beside the `Analyze (<language>)` jobs of the workflow that uploads its results, and an
# approved head was `CLEAN` only once it had concluded (gh-threads.ts's `mergeStateOf`). The
# times say how long each ran, and `gh pr checks` prints them. It is the answer every head of
# every world takes: no world holds a head's CI running, since what holds an approved head
# `BLOCKED` here is the open threads (resolution_protection_routes), not a check. The gh stand-in
# reports each on the commit it is read for, its ids that commit's and its times counted from when
# that commit reached the remote (gh-standin.ts's `reportedRuns`). Every world's base requires
# the two of GitHub Actions as status checks and the third as its code-scanning tool's
# (base_ruleset_routes).
concluded_checks() {
  jq -n --arg repo "${worker_repo:-}" --arg pr "${worker_pr:-}" '[
    {name:"test", status:"completed", conclusion:"success", details_url:"https://example.invalid/2",
     started_at:"2026-01-01T00:00:00Z", completed_at:"2026-01-01T00:00:20Z", app:{id:15368}},
    {name:"lint", status:"completed", conclusion:"success", details_url:"https://example.invalid/1",
     started_at:"2026-01-01T00:00:00Z", completed_at:"2026-01-01T00:00:10Z", app:{id:15368}},
    {name:"CodeQL", status:"completed", conclusion:"success",
     started_at:"2026-01-01T00:00:00Z", completed_at:"2026-01-01T00:02:30Z", app:{id:57789},
     output:{title:"No new alerts in code changed by this pull request",
       summary:("[View all branch alerts](/" + $repo + "/security/code-scanning?query=pr%3A" + $pr + "+tool%3ACodeQL+is%3Aopen).")}}]'
}

# base_ruleset_routes [RESOLUTION [LAST_PUSH]] prints the routes of the one repository ruleset that
# protects every world's base ($worker_base of $worker_repo), as a ruleset alone protects
# production's `main`: active on the default branch, holding production's seven rules in
# production's order, each with production's parameters in production's kinds and at production's
# values (production-shapes.json's ruleset reads of 2026-11-19T05:35:00Z: a rule's parameters are
# the deployment's configuration, not a value that names production), but for the checks and the
# tool, which are the ones the world's head reports, RESOLUTION and LAST_PUSH. RESOLUTION, `true`
# or `false` (the default, production's), is whether the base requires review threads resolved,
# which a world whose premise is a head its open threads hold `BLOCKED` asks for
# (resolution_protection_routes). LAST_PUSH, `true` or `false` (the default, production's), is the
# `pull_request` rule's `require_last_push_approval`, which last-push's world asks for
# (review_world): the answer keeps the shape of those reads, and only that parameter's value
# changes. The rules, and what the world does with each
# (packages/pi-envoy/scripts/README.md lists every rule and every parameter nothing acts on):
#   - `deletion`, `non_fast_forward`, `required_linear_history`: the run's remote refuses a
#     deletion of the base, a push to it that is no fast-forward and one carrying a merge commit,
#     as GitHub refuses them, and every other update of it (fixture_first_commit);
#   - `pull_request`: one approval required, which decides the review decision and holds the merge
#     state `BLOCKED` until it stands; no approval dismissed on a push, so an approval stands
#     across the implementer's push; where LAST_PUSH says so, no approval by the account that made
#     the head's last push counts (GitHub's REST description: "the most recent reviewable push
#     must be approved by someone other than the person who pushed it"); threads resolved where
#     RESOLUTION says so (gh-threads.ts's `reviewDecisionOf`, `mergeStateOf`). Its code owner
#     review, true as production's has been since 2026-11-18T18:19:22Z, asks the approval of a
#     code owner of each changed file a CODEOWNERS file of the base names one for, and the world's
#     repository holds none, so it asks no approval of its own (`reviewDecisionOf`). Its extra
#     approval for unattributed changes is asked only of a pull request Copilot opened with no
#     human collaborator (GitHub's REST description), which no world's is: the implement App's bot
#     opens it, so nothing acts on it;
#   - `required_status_checks`: the checks GitHub Actions reports on the head (concluded_checks,
#     `test` and `lint`), by its integration id, which hold the merge state `BLOCKED` until they
#     pass;
#   - `merge_queue`: the world holds no queue, and the merge state of an approved, green, resolved
#     head is `CLEAN`, as production's is with its queue on (measured on 2026-11-18,
#     gh-threads.ts's `mergeStateOf`);
#   - `code_scanning`: the tool whose check the head reports (concluded_checks' `CodeQL`), which
#     holds the merge state `BLOCKED` until that check succeeds; the world raises no alert, so
#     nothing acts on its thresholds.
# The world's `rulesets` list holds this ruleset alone: production's lists two, its `main`'s and
# one on another branch, and the world's repository protects its base and no other branch, so
# the list is the one a repository with production's `main` ruleset and nothing else answers, in
# production's item shape. Its routes are the three reads of a ruleset GitHub answers a role App,
# each in production's shape: the rules in force on the branch, each with the ruleset it comes
# from (`rules/branches/<base>`, the merge gate's rulesets read); the repository's rulesets; and
# the ruleset by its id, GitHub's 404 for any other id (gh-standin.ts's `ruleset` answer). Its id,
# node id and times are the world's own.
base_ruleset_routes() {
  jq -n --arg repo "$worker_repo" --arg base "$worker_base" --argjson runs "$(concluded_checks)" \
    --argjson resolution "${1:-false}" --argjson last_push "${2:-false}" '
    {id:1, name:$base, target:"branch", source_type:"Repository", source:$repo, enforcement:"active",
     conditions:{ref_name:{exclude:[], include:["~DEFAULT_BRANCH"]}},
     rules:[{type:"deletion"}, {type:"non_fast_forward"}, {type:"required_linear_history"},
       {type:"pull_request", parameters:{required_approving_review_count:1,
         dismiss_stale_reviews_on_push:false, required_reviewers:[], require_code_owner_review:true,
         dismissal_restriction:{enabled:false, allowed_actors:[]}, require_last_push_approval:$last_push,
         required_review_thread_resolution:$resolution,
         require_extra_approval_for_unattributed_changes:true, allowed_merge_methods:["squash"]}},
       {type:"required_status_checks", parameters:{strict_required_status_checks_policy:false,
         do_not_enforce_on_create:false,
         required_status_checks:[$runs[] | select(.app.id == 15368) | {context:.name, integration_id:.app.id}]}},
       {type:"merge_queue", parameters:{merge_method:"SQUASH", max_entries_to_build:5,
         min_entries_to_merge:1, max_entries_to_merge:4, min_entries_to_merge_wait_minutes:0,
         grouping_strategy:"ALLGREEN", check_response_timeout_minutes:180}},
       {type:"code_scanning", parameters:{code_scanning_tools:[$runs[] | select(.app.id == 57789)
         | {tool:.name, security_alerts_threshold:"high_or_higher", alerts_threshold:"none"}]}}],
     node_id:"RRS_lACqUmVwb3NpdG9yec4AAAABzgAAAAE", created_at:"2026-01-01T00:00:00.000Z",
     updated_at:"2026-01-01T00:00:00.000Z", current_user_can_bypass:"never",
     _links:{self:{href:("https://api.github.com/repos/" + $repo + "/rulesets/1")},
       html:{href:("https://github.com/" + $repo + "/rules/1")}}} as $ruleset
    | [
      {match:("^gh api .*/rules/branches/" + $base), methods:["GET"],
       stdout:[$ruleset.rules[] + {ruleset_source_type:"Repository", ruleset_source:$repo, ruleset_id:$ruleset.id}]},
      {match:"^gh api .*/rulesets/[^/ ?]+([ ?]|$)", methods:["GET"], answer:"ruleset", stdout:$ruleset},
      {match:"^gh api .*/rulesets([ ?]|$)", methods:["GET"],
       stdout:[$ruleset | {id, name, target, source_type, source, enforcement, node_id, _links, created_at, updated_at}]}
    ]'
}

# resolution_protection_routes [LAST_PUSH] prints the routes of a base whose ruleset requires
# review threads resolved beside every other rule every world's base holds (base_ruleset_routes,
# LAST_PUSH its `require_last_push_approval`, `false` by default), which a world puts ahead of
# fixture_write's own, so that with its approval standing and every check green the one rule that
# holds its head `BLOCKED` is an open review thread (gh-threads.ts's `mergeStateOf`).
resolution_protection_routes() {
  base_ruleset_routes true "${1:-false}"
}

# run_project SCENARIO CASE LABEL N [BATCH] prints the Legion project token of one worker run.
# Every run of a batch gets a token of its own, and no two batches share one: the Envoy role
# topics a worker session claims are built from the project and the issue key
# (notifications.role.legion-<project>-<issue>-<role>, contracts' roleToken), every worker run is
# booted on the same issue key, and a role message no holder was there to take is kept in the
# batch's NATS for the role's next holder, for an hour (packages/envoy/cmd/listener/role_keep.go).
# So two runs under one project read each other's messages, and a given NATS
# (SKILL_SCENARIOS_NATS_URL), which the batch leaves running, carries a kept message from one
# batch to the next: BATCH is what tells one batch from another, the work directory's tag and the
# batch's start second.
#
# A project token is lower-case letters and digits alone (isLegionProjectToken), and the agent
# reads it in its LEGION_PROJECT, its role topics and its system prompt's addressing, so the token
# is the one production makes of a deployment's project, the repository's name lower-cased with
# every other character dropped (contracts' legionProjectToken: `cybermonklabs/legion` is
# `cybermonklabslegion`), and the first 12 hex digits of the parts' digest, which tell the runs
# apart and name none of the parts: it was `skills` and the digest, and `skills` named the
# experiment. The run's scenario, case, label and number are its record's (cmd_run).
run_project() {
  local digest
  digest=$(printf '%s\0' "$1" "$2" "$3" "$4" "${5:-}" | sha256sum)
  printf '%s%.12s\n' "$(printf '%s' "$worker_repo" | tr 'A-Z' 'a-z' | tr -cd 'a-z0-9')" "$digest"
}

# worker_boot NAME PROJECT ROLE [PHASE [COMPLETED]] boots the label's checkout's phase worker for
# ROLE on the run's issue and waits for it: the part of a worker run that is the same in every
# phase. The run's
# prompt and its world are the scenario's own (run_tester_proof, run_reviewer_text_findings,
# run_threads_closed). PROJECT is the run's own Legion project, computed once by cmd_run and
# passed to the recorder and to every boot of the run (run_project): two copies of it drift
# silently, since the recorder would subscribe to one project's topics while the agents published
# on another's and every send would read as never made. PHASE is the phase the issue stands in
# while this agent runs, empty for the phase the role's own worker is started in (the daemon
# stand-in's default); threads-closed passes `reviewing` or `accepting` by its case
# (run_threads_closed), neither of which is the implementer's phase and the second of which is
# not the reviewer's, so a completion there is a late one, which the stand-in answers as the run's
# label's daemon answers it (daemon-standin.ts). COMPLETED is the roles, comma-separated, whose
# phase the run's world says was completed with a commit before the run began, which the daemon
# refuses a late completion from (HANDOFF_NOT_CURRENT_PHASE); empty for none. A role whose
# seeded record stands on its row is such a role without being named here (the stand-in reads
# the seeds); the argument names one for a run whose seeds record none of it.
#
# A run that boots a second agent calls this again with that agent's role, after writing its
# prompt: the two run one after the other on the one run directory, so the second reads the gh
# stand-in, the remote and the Dispatch issue the first left. Each agent gets its own daemon
# stand-in, whose own output ($R/logs/daemon.log, where start_process writes it) is moved aside
# under the role of the agent it served, its own HOME, its own Oh My Pi session directory and its
# own last line of output, and the role of each is in the calls it records (its pane's LEGION_ROLE),
# while the run keeps one calls.jsonl, one pushes.log and one record of what reached the daemon,
# $R/daemon.log, which every stand-in of the run appends to. The agent is launched by boot_agent,
# which relaunches it once when its claim boot is refused before its first turn.
#
# Each boot is on the run's boot record, <run>/booted (completion-answers.ts's BOOTED), before its
# stand-in starts: `boot <role>`, a line per attempt (boot_agent writes the relaunch's), so a boot
# whose stand-in never listened is on the record too, followed by the `since <role> <time>`
# boot_agent asks the boot's reader with. A role the rig decides not to boot is there as `skip
# <role> <reason>` (run_threads_closed). Health holds the record to the scenario's roles
# (ANSWERS' boots, SKIPS) and each booted role to its own output and transcript.
worker_boot() {
  local name=$1 project=$2 role=$3 phase=${4:-} completed=${5:-} state port pane_gh pane_vars pane_build
  state=$R/state
  mkdir -p "$state/gh" "$state/secrets"
  chmod 0700 "$state/secrets"
  (umask 077 && openssl rand -hex 16 >"$state/secrets/boot")
  local identity_name identity_email
  IFS=$'\t' read -r identity_name identity_email < <(role_identity "$role")
  printf '[user]\nname = "%s"\nemail = "%s"\n[ui]\npaginate = "never"\n' \
    "$identity_name" "$identity_email" >"$R/jj.toml"
  if [ -n "$booted" ]; then
    # The agent before this one has exited, and its stand-in goes with it: stopped here so no run
    # leaves one listening, and its log kept, so the port read below is this stand-in's own. The
    # stand-in is one process with no children, and it is stopped with `stop_pid`, which signals
    # TERM and then KILL, never `stop_tree`, which freezes the process first: this shell's own
    # process group is often orphaned (a run is started by xargs under a batch), and the kernel
    # sends SIGHUP to every process of an orphaned group one of whose members is stopped, which
    # the run's HUP trap takes as its own exit. Measured here on 2026-11-06: the run ended, with
    # no message, between the two agents.
    stop_pid "$daemon_pid" && wait "$daemon_pid" 2>/dev/null || true
    daemon_pid=''
    mv "$R/logs/daemon.log" "$R/logs/daemon-$booted.log"
    mv "$R/out.txt" "$R/out-$booted.txt"
    # base_env copies a HOME afresh from the label's for the agent starting now, so the one the
    # agent before it wrote in is moved aside rather than removed with it: a run's evidence is
    # every half's, and the half that ran first left its own files there.
    mv "$R/home" "$R/home-$booted"
    sessions_dir=$R/sessions/$role
  else
    sessions_dir=$R/sessions
  fi
  mkdir -p "$sessions_dir"
  # Port 0: the kernel assigns the stand-in's port, which it prints, so concurrent runs never race.
  # It is given the run directory, not a repository: the record it answers names the world's own
  # repository and default branch, which it reads out of $R/world.json (the worker world comment).
  # It is given the work directory, under which daemon-pane.ts keeps what it read of the label's
  # daemon (its facts cache), which services_up filled for every boot of worker_boots
  # (label_facts), and the label's build of daemon-pane.go, which reads a fact the cache does not
  # hold in milliseconds (daemon_pane_binary). It is given the run's own Dispatch and the bearer
  # the run's panes read it with, since a completion naming a handoff record is read back from
  # there, as the daemon reads it (daemon-standin.ts's `handoffRecord`), and refused as the daemon
  # refuses a record that is not there or is another issue's, phase's or version's.
  pane_build=$(build_dir "$co")/daemon-pane
  printf 'boot %s\n' "$role" >>"$R/booted"
  start_process daemon env SKILL_SCENARIOS_WORK="$work" SKILL_SCENARIOS_DAEMON_PANE="$pane_build" \
    DISPATCH_URL="http://127.0.0.1:$dispatch_port" DISPATCH_TOKEN_FILE="$tokens/dispatch-token" \
    bun "$here/daemon-standin.ts" 0 \
    "$R/daemon.log" "$state/secrets/boot" "$project" "$worker_key" "$R" "$role" "$phase" "$completed"
  await_start daemon "$daemon_pid" 30 "the daemon stand-in to listen" grep -q '^listening on ' "$R/logs/daemon.log"
  port=$(sed -n 's|^listening on http://127\.0\.0\.1:||p' "$R/logs/daemon.log")
  base_env
  standins
  # The gh stand-in refuses a call naming an owner outside the world's project as the Legion gh
  # wrapper of the label's checkout refuses it, by that wrapper's own selector table
  # (gh-selectors.ts), which services_up read into the facts cache (label_facts). The agent's
  # namespace shows neither the checkout nor the work directory, so the table is written into
  # the run, beside the routes the stand-in answers by, before the agent starts.
  SKILL_SCENARIOS_WORK=$work bun "$here/daemon-pane.ts" gh-selectors "$co" "$R/gh-selectors.json" ||
    fail "worker_boot: the gh selector table of label checkout $co could not be read (above), so the run's gh could refuse no owner as the label's gh wrapper does"
  # The pane the label's checkout's daemon would give that role, over the base every agent gets:
  # worker.env and system-args. standins runs first, since worker-pane.ts names its $R/bin/gh, the
  # recording stand-in, as the pane's real gh. It reads the repository and the default branch its
  # addressing names out of $R/world.json too, as the daemon stand-in does, and composes it with
  # the label's build of daemon-pane.go, as the stand-in reads its facts.
  SKILL_SCENARIOS_DAEMON_PANE=$pane_build bun "$here/worker-pane.ts" "$R" "$R/pane.env" "$port" "$profile" "$project" "$worker_key" "$role" "$co"
  # The run's Envoy is its label's own listener in the batch, built from the label's checkout
  # (services_up), named once. The daemon's pane carries an ENVOY_URL of its own — the default it
  # gives a pane whose environment configures no Envoy (daemon-pane.ts's DEFAULT_ENVOY_URL) — and
  # $services_env carries the label's listener, so the file held the address twice and `launch`'s
  # `env -i` decided which one the agent got by the order of the two lines. The pane's line is
  # dropped here: one fact, one line, and a reader of pane.env sees the address the run's
  # sessions actually reach. `sed` rather than `grep -v`, which exits 1 on a worker.env whose
  # every line it dropped and would take `set -e` with it. Then production's git settings for
  # every role process (git_pairs), where the pane held GIT_CONFIG_COUNT=0 before.
  {
    sed '/^ENVOY_URL=/d' "$R/worker.env"
    cat "$services_env"
    echo "DISPATCH_URL=http://127.0.0.1:$dispatch_port"
    echo "DISPATCH_TOKEN_FILE=$tokens/dispatch-token"
    git_pairs
    role_env "$role"
    echo "JJ_CONFIG=$R/jj.toml"
    echo "GH_REPO=$worker_repo"
  } >"$R/pane.env"
  # The record of the run's calls is opened here, before the agent starts, by the `gh` the pane's
  # own PATH resolves to, run in the pane's own environment: the file above is the one `launch`
  # gives the agent, so `command -v` on its PATH finds the gh the agent's own calls will reach —
  # whatever stands between it and the stand-in, since the daemon's pane leads PATH with its
  # launcher directory — and `env -i` hands that gh the variables `launch` hands the agent: the
  # run it records under is its bundle's own (standins). `--open` writes the one line that says
  # the stand-in was there and records no call, as record-publishes.ts writes its `recording`
  # line. So a run whose agents call neither gh nor bun leaves calls.jsonl there holding that
  # line alone, which the score reads as the run that called nothing, where a record nothing
  # wrote is absent and stays the rig error (score.ts's `unscored`; fixture_head_commit tells
  # the audit behind it). An empty file written by this script would say neither, and neither
  # would the stand-in run by its own path: a $R/bin/gh that cannot start, a pane PATH that
  # reaches no gh, or a pane that names no run would leave the opening line there and have the
  # run read as one that called nothing, when no call of its agents could have been recorded at
  # all. Opened this way the line means what the score reads it as — this run's own gh reached
  # the stand-in — and a broken wrapper, PATH or pane is the rig error it is, with the record
  # absent. The second agent of a run opens nothing: the record is open.
  mapfile -t pane_vars <"$R/pane.env"
  pane_gh=$(PATH=$(sed -n 's/^PATH=//p' "$R/pane.env" | tail -n 1); command -v gh) ||
    fail "worker_boot: $R/pane.env names no PATH with a gh on it, so no call of this run could reach the gh stand-in"
  env -i "${pane_vars[@]}" "$pane_gh" --open ||
    fail "worker_boot: the pane's gh ($pane_gh) could not open $R/calls.jsonl"
  adopt_workspace "$role" || fail "worker_boot: $role could not take the issue workspace's working copy ($R/ws)"
  # The issue workspace's origin reaches the run's remote as this role's App from here on, as a
  # pod's git presents the token of the App the daemon gave the role (world_remote_app).
  [ ! -e "$R/bin/git-remote-$world_remote_vcs" ] || world_remote_app "$role"
  boot_agent "$name" "$role"
  booted=$role
}

# boot_agent NAME ROLE launches the agent of ROLE (launch) and, when its claim boot was refused
# before its first turn, launches it once more in the same run, as production's daemon relaunches
# an agent whose process exits at its boot. Round 23's world-health check at 2926519c lost
# reviewer-text-findings-text-only-base-1 so: its plugin exited at boot with `claim boot failed
# at claimRole after claims/register …: … asked for no session consumer …`, the refusal of the
# Envoy listener (session_delivery.go), then one listener built from the head clone for both
# labels and now each label's own (services_up), and a rig run is one `omp -p` with nobody to
# launch it again. How often it fires, by label, is the count `rig.sh health` prints. What counts
# is read by boot-refusal.ts, the one reader of it, run here as its own program, whose "claim
# boot" rule in health holds the run to the same record: the daemon stand-in's refusal of the
# agent's `claims/register` (its $R/daemon.log lines of ROLE since the launch began), or the
# plugin's own `claim boot failed at …` line in $R/out.txt, and only when the agent's transcripts
# hold no turn, no model call and no tool call. A refusal after the first turn is never
# relaunched: the daemon's relaunch would not take that turn back.
#
# The `since` the reader is asked with, the time the launch began, is on the run's boot record
# right after the attempt's `boot <role>` line, as `since <role> <time>` (completion-answers.ts's
# BOOTED), so a replay of the boot asks the reader with the call's own argument rather than one
# rebuilt from the stand-in's log. The relaunch is asked nothing, and its `boot` line has none.
#
# The first attempt is kept whole beside the second, under $R/boot-1/ROLE: its out.txt, its
# session directory (sessions/) and the stand-in's lines of it (daemon.log), while the second
# attempt's output, transcript and HOME stand where any agent's do, so the score and health read
# the run on the second attempt's record. The stand-in, its boot token and the agent's HOME are
# the first attempt's, as a pod's are across the daemon's relaunch. This is narrower than the
# daemon's launch budget, which relaunches an agent until its launch failure limit: the rig
# relaunches once, and a second refusal stands, the run unhealthy, naming both attempts. The
# relaunch is a second attempt of the same boot, so it adds a second `boot <role>` line to the
# run's boot record before it starts (worker_boot), which health reads as that one boot.
boot_agent() {
  local name=$1 role=$2 since first refusal status=0
  since=$(date -u +%FT%T.%3NZ)
  printf 'since %s %s\n' "$role" "$since" >>"$R/booted"
  launch "$name"
  refusal=$(bun "$here/boot-refusal.ts" "$R" "$role" "$since" "$sessions_dir") || status=$?
  case $status in
  0) ;;
  1) return 0 ;;
  *) fail "boot_agent: boot-refusal.ts could not read the $role's boot (exit $status)" ;;
  esac
  first=$R/boot-1/$role
  mkdir -p "$first"
  mv "$R/out.txt" "$first/out.txt"
  mv "$sessions_dir" "$first/sessions"
  mkdir -p "$sessions_dir"
  jq -c --arg role "$role" --arg since "$since" 'select(.role == $role and .at >= $since)' \
    "$R/daemon.log" >"$first/daemon.log"
  note "${R##*/}: the $role's claim boot was refused before its first turn ($refusal); launching it once more, the first attempt kept in $first"
  printf 'boot %s\n' "$role" >>"$R/booted"
  launch "$name"
}

# worker_run_world INDEX START [CASE] is the start every worker run shares: the run's own
# Dispatch and its own issue on it, whose newest implement record a worker reads and whose newest
# records it writes; the world CASE names (worker_fixture); and the issue as a Legion issue stands
# once its implementer has finished, in the order production writes it. The issue resolves to the
# world's repository (seed.ts project) and is handed to Legion, its check the one CASE's world
# asks for (case_check); then the planner's approved plan.md and plan handoff, at the default
# branch the implementer started from; the pull request the world's remote holds, as Dispatch
# stores its head; and the implementer's handoff and result at the commit it pushed, the head its
# own handoff names, after which the issue is `testing`. That commit is the pull request's head in
# every world but those whose reviewer pushed a text commit above it (approved_world), where no
# result stands at the head and `dispatch results` says no pass stands there. It leaves the issue
# key in $worker_key, the pull request's number and head in the caller's $pr and $head, which
# every runner's prompt names, and the implementer's commit in the caller's $implemented, which
# the tester's result is recorded at.
worker_run_world() {
  local index=$1 start=$2 case_name=${3:-} covers=() kind
  kind=$(case_check "$case_name")
  [ -z "$kind" ] || covers=("$kind")
  own_dispatch project
  seed_ts tester-proof "$R/fixture.json" "${covers[@]}"
  worker_key=$(jq -r .issue "$R/fixture.json")
  worker_fixture "$index" "$start" "$case_name" >>"$R/fixture.log" 2>&1 ||
    fail "the fixture failed; see $R/fixture.log"
  read -r pr head < <(jq -r '"\(.pr) \(.head)"' "$R/world.json")
  implemented=$(jq -r .head "$R/implement.json")
  seed_ts plan "$R/fixture.json" "$R/implement.json" "$(git -C "$R/remote.git" rev-parse "$worker_base")" "$R/remote.git"
  seed_ts pull-request "$R/fixture.json" "$R/fixtures.json" "$R/remote.git"
  seed_ts implement-handoff "$R/fixture.json" "$R/implement.json" "$implemented"
  start_heads
}

# start_heads starts the run's head watcher (head-watcher.ts), outside the agent's namespace, for
# the rest of the run: on every push to the pull request's branch it stores the pushed head on the
# run's Dispatch, as production's `pull_request` event does at the push. The push's own path
# cannot: the remote's post-receive hook runs inside the agent's namespace, which shows neither
# this directory nor the run's database URL (world_paths), and a hook holding the URL would hand
# it to the agent. Its record, <run>/logs/heads.log, is hidden from the agent like every log of the
# run's, and health's "head" rule reads it (health.ts's headUnmoved). stop_run stops it. The
# database's URL (own_dispatch's dispatch_database_url, which can hold the given server's
# password) goes in the watcher's environment, never on `env`'s command line, which every user of
# the box reads, as seed_ts's goes.
start_heads() {
  SEED_DATABASE_URL=$dispatch_database_url start_process watcher \
    env --chdir="$root/packages/dispatch" DISPATCH_E2E_PORT="$dispatch_port" bun "$here/head-watcher.ts" "$R"
  await_start watcher "$watcher_pid" 30 "the head watcher to open its record" \
    grep -q '"opened":"heads.log"' "$R/logs/heads.log"
}

# heads_stored succeeds once the head watcher's record (head-watcher.ts) answers every push the
# run's push log holds to the pull request's branch, each by its own commit, stored or failed: the
# wait a reader makes before an agent that reads the head the run's Dispatch compares with.
heads_stored() {
  local commit
  for commit in $(awk '$2 ~ /^refs\/heads\/legion\// { print $4 }' "$R/pushes.log"); do
    grep -q "\"pushed\":\"$commit\"" "$R/logs/heads.log" || return 1
  done
}

run_tester_proof() {
  local name=$1 project=$2 index=$3 start=$4 pr head implemented version
  worker_run_world "$index" "$start"
  version=$(jq -r .implement.version "$R/fixture.json")
  # The architect's assignment, with the Handoff line the daemon's task names the predecessor's
  # record in (packages/daemon/internal/workflow/effects.go).
  printf 'Architect → tester, %s. The implementer has finished: PR #%s (%s, repository %s) is at %s. Handoff: implement version %s (dispatch handoff-read --issue %s --phase implement --version %s). Test it and finish your phase.\n' \
    "$worker_key" "$pr" "https://github.com/$worker_repo/pull/$pr" "$worker_repo" "$head" "$version" "$worker_key" "$version" >"$R/prompt.txt"
  steps_seed "$project" testing
  worker_boot "$name" "$project" tester
  # What the run left on its issue: every handoff record, each with its body, for testerProof.
  seed_ts capture-handoffs "$R/fixture.json" "$R"
}

# The reviewer's round on the world worker_fixture's <review> case builds: two text findings and no
# behavioural one (text-only), those two and a blank-name guard that is gone (defect), or
# text-only's two on a base whose rules require another approver of the last push (last-push).
# The issue carries the implementer's passing result on its one before_merge check and the
# tester's own at the same head, in every case, so no gate of the reviewer's refuses an approval
# and its verdict rests on what it found. The case is in the run's name and in <run>/case.txt,
# which reviewerTextFindings reads back and holds to the name. The reviewer is booted in
# `reviewing`, its own phase, and its review moves the issue (daemon-standin.ts's `servedPhase`),
# so a completion after that is a late one, answered as the run's label's daemon answers it. What
# each label's daemon may answer it here, and in which cases the world must take at least one of
# the reviewer's completions (defect and last-push; a text-only reviewer that pushed its fix waits
# for the implementer and completes nothing), is completion-answers.ts's
# ANSWERS["reviewer-text-findings"], which `rig.sh health` and the score read.
run_reviewer_text_findings() {
  local name=$1 project=$2 index=$3 start=$4 review=$5 pr head implemented version round
  start_recorder "$project"
  worker_run_world "$index" "$start" "$review"
  seed_ts tester-result "$R/fixture.json" "$R/test.json" "$head"
  version=$(jq -r .test.version "$R/fixture.json")
  # The daemon's two sentences of the review round end every task of the reviewer, as the label's
  # daemon ends them (packages/daemon/internal/workflow/review_round.go, main's since #4328): this
  # is the issue's first round, so no review of the reviewer's has read a commit, and its entry is
  # 1, the one its start raises (daemon-standin.ts's ENTRIES, which the record answers as
  # phaseEntry). A daemon of main ff2a10ef's shape ends no task with them, and the reader prints
  # nothing for it (daemon-pane.ts's reviewRoundSentences).
  round=$(bun "$here/daemon-pane.ts" review-round "$co" '' 1) ||
    fail "the sentences the daemon of label checkout $co ends a reviewer's task with could not be read (above)"
  # The architect's assignment, with the Handoff line the daemon's task names the predecessor's
  # record in (packages/daemon/internal/workflow/effects.go): for a review round, the tester's.
  printf 'Architect → reviewer, %s. The tester has finished: PR #%s (%s, repository %s) is at %s. Handoff: test version %s (dispatch handoff-read --issue %s --phase test --version %s). Review it and finish your phase.%s\n' \
    "$worker_key" "$pr" "https://github.com/$worker_repo/pull/$pr" "$worker_repo" "$head" "$version" "$worker_key" "$version" "$round" >"$R/prompt.txt"
  steps_seed "$project" reviewing
  worker_boot "$name" "$project" reviewer
  # What the run left on its issue: every handoff record, each with its body, and every result of
  # its check, beside the review submission the gh stand-in recorded, which is what the score
  # reads; then the judge's readings of what its agents wrote (judge.ts's `judgeWorker`).
  seed_ts capture-handoffs "$R/fixture.json" "$R"
  seed_ts capture-results "$R/fixture.json" "$R"
  judge_ts run "$R"
}

# start_recorder starts this run's record of what Envoy accepted on its own role topics
# (record-publishes.ts): `<run>/published.jsonl` is what the score and reviewer_wake both read, so
# neither works out what was sent from what an agent wrote. The two scenarios that score a message
# call it before their first agent, and every other use of the rig starts none, since a scenario
# that scores no message would take a dependency on the bus for nothing — `rig-isolation.test.ts`
# gives its runs a services file with no NATS in it at all, and a recorder there ended every run
# of that suite before its first agent (CI of 2026-11-09, run 42626483576).
#
# The bus comes from the batch's own services file: `rig.sh run` is a process of its own, and the
# URL the batch resolved lives there and in no variable of this one. It is exported rather than
# passed as an argument, since a URL may carry userinfo and an argument list is world-readable.
#
# start_recorder PROJECT takes the run's project rather than computing one of its own: the
# recorder subscribes to the topics of that project, and the agents publish on the topics
# worker_boot gives them, so two computations of it that drift would leave every send read as
# never made — a silent fail of the run rather than an error of the rig (cmd_run).
start_recorder() {
  local project=$1
  : >"$R/published.jsonl"
  export SKILL_SCENARIOS_NATS_URL
  SKILL_SCENARIOS_NATS_URL=$(sed -n 's/^ENVOY_NATS_URL=//p' "$services_env")
  start_process record bun "$here/record-publishes.ts" "$R/published.jsonl" "legion-$project-"
  await_start record "$record_pid" 30 "the publish recorder to subscribe" \
    grep -q '^recording ' "$R/logs/record.log"
}

# reviewer_wake prints every message the implementer published to the reviewer's role topic, in
# the order Envoy accepted them, as one JSON array; an implementer that published none prints an
# empty one. It asks score.ts, which is the one reader of a publish this rig has: the harness and
# the score read the same record, so a message one of them reads and the other does not cannot
# score a wake that reached nobody. What two readers of that one fact cost is in
# record-publishes.ts's header. Every message is given, not the last: Envoy delivers each one,
# and a reviewer handed only the last would be missing a wake its own run sent.
reviewer_wake() {
  bun "$here/score.ts" published "$R" reviewer
}

# skip_reason SCENARIO CASE ROLE prints the reason completion-answers.ts's SKIPS declares for
# booting no ROLE in SCENARIO's CASE, the one place it is written: the run's boot record carries
# it verbatim on its `skip <role> <reason>` line, which health and the score hold to the same
# SKIPS. It fails where SKIPS declares none, since a skip with no declared reason is a boot the
# rig dropped.
skip_reason() {
  bun -e 'const { SKIPS } = await import(Bun.argv[1]); const reason = SKIPS[Bun.argv[2]]?.[Bun.argv[3]]?.[Bun.argv[4]]; if (typeof reason !== "string") process.exit(1); console.log(reason);' \
    -- "$here/completion-answers.ts" "$@" ||
    fail "completion-answers.ts's SKIPS declares no reason to boot no $3 in $1 $2"
}

# The implementer's last chance at the reviewer's text commit, on the world worker_fixture's <case>
# builds (approved_world): the reviewer pushed a commit of its own that is right and left no
# thread (accept, and crossing, whose check its earlier pass does not hold at that commit), pushed
# one that makes the document's sentence false another way and left a thread on the workflow file
# (correct), or approved the head and left the one thread whose sentence the code makes false
# (code-defect). The issue carries the implementer's and the tester's passing results on its one
# before_merge check at the implementer's commit, none at the reviewer's, and the reviewer's
# handoff. The implementer runs first, in its own live session, which the reviewer's message
# reaches, as spec v36's R1 has the reviewer send one: in accept, correct and crossing the commit
# it pushed, by SHA, what it changes, and each thread it left open; in code-defect the approval and
# its thread. Then, in correct and code-defect, the reviewer, woken by the implementer's own
# message and by nothing else; accept and crossing boot none (completion-answers.ts's SKIPS).
# Neither is told what to do: that is the rule the scenario measures. The case is in the run's
# name and in <run>/case.txt, which threadsClosed reads back and holds to the name.
#
# The phase the halves are booted in is the one main's engine leaves the issue in (review.go's
# `reviewRound`, daemon-standin.ts's `servedPhase`):
#   accept, correct, crossing
#                `reviewing`. The reviewer has not approved, so no review decision ends the round
#                and the issue stays in `reviewing` while the implementer answers (Verification
#                v47's k2): the daemon starts no phase for that answer, and in correct the
#                reviewer's approval of the head the implementer's push made, `CLEAN` once it has
#                resolved the thread, ends the round and moves the issue to `accepting`.
#   code-defect  `accepting`. Its approved head is `CLEAN`, so the round ended on the
#                approval, and the issue stands where the architect reads it. It stays there
#                because a `REQUEST_CHANGES` submitted in `reviewing` ends the round and moves
#                the issue back (review.go's `roundRejected`), which would make the reviewer's
#                review a route for the finding, and the control reads the messages to the
#                architect and the reviewer as the one route (score.ts's threadsClosed). So each
#                half's `read_record` answers `accepting`, the architect's phase
#                (packages/daemon/internal/workflow/engine.go, RoleFor).
# Both roles' rounds already stand on their rows: the implementer's implementing round with its
# commit and its record (worker_boot's COMPLETED, and the seeded implement record), and the
# reviewer's review round with the review record seeded below, which the stand-in reads onto
# the row. So the implementer's `handoff_complete` is refused 409 HANDOFF_NOT_CURRENT_PHASE in
# either phase by either label's daemon, the one deliberate refusal, and so is the reviewer's
# once the round has moved the issue, or from its boot in `accepting` (the engine's
# `leftItsPhase` refuses a role whose row holds a completion). The reviewer's completion in
# `reviewing` is of its own phase and is taken. A completion naming a seeded record is one
# naming a record another session wrote, which the daemon's route refuses 403
# HANDOFF_SESSION_MISMATCH: the agent writes its own. What each label's daemon may answer each
# half here is completion-answers.ts's ANSWERS["threads-closed"], which `rig.sh health` and the
# score read.
run_threads_closed() {
  local name=$1 project=$2 index=$3 start=$4 threads_case=$5 pr head implemented open wake skip \
    changes phase=reviewing completed=implementer
  [ "$threads_case" != code-defect ] || phase=accepting
  start_recorder "$project"
  worker_run_world "$index" "$start" "$threads_case"
  seed_ts tester-result "$R/fixture.json" "$R/test.json" "$implemented"
  seed_ts review-handoff "$R/fixture.json" "$R/review.json"
  # The reviewer's message as it reaches the implementer's live session: the head it was given,
  # its own commit where it pushed one, what that commit changes, and the threads it left open,
  # each named by its node id, its file and what the reviewer wrote.
  open=$(jq -r '.[] | "- Thread \(.id), \(.path): \(.body)"' "$R/threads.json")
  if [ "$threads_case" = code-defect ]; then
    printf 'Reviewer → implementer, %s. I approved PR #%s (%s, repository %s) at %s. Its only open findings are text, and each is the review thread I wrote on it:\n%s\nOver to you.\n' \
      "$worker_key" "$pr" "https://github.com/$worker_repo/pull/$pr" "$worker_repo" "$head" "$open" >"$R/prompt.txt"
  else
    [ -n "$open" ] || open='none'
    changes='rewrites the sentence of docs/cli.md the change made false'
    # shellcheck disable=SC2016 # the backticks are the message's own markdown
    [ "$threads_case" = correct ] || changes="$changes"', and rewords the example line docs/cli.md gives for `bun greet.ts Ada`'
    printf 'Reviewer → implementer, %s. On PR #%s (%s, repository %s) I pushed my text commit %s, which %s. Threads I left open:\n%s\nOver to you.\n' \
      "$worker_key" "$pr" "https://github.com/$worker_repo/pull/$pr" "$worker_repo" "$head" "$changes" "$open" >"$R/prompt.txt"
  fi
  steps_seed "$project" "$phase"
  worker_boot "$name-implementer" "$project" implementer "$phase" "$completed"
  # Every push the implementer made has moved the head the run's Dispatch compares results with
  # (start_heads), as its own event moves it in production, and the reviewer is not booted before
  # the watcher has answered each of them: the reviewer then decides each earlier pass at the head
  # the implementer's push made, as production's Dispatch shows it.
  await_start watcher "$watcher_pid" 30 "the head watcher to store the implementer's pushes" heads_stored
  # The reviewer's own wake is the implementer's message and nothing else: the daemon starts no
  # phase for the implementer's push and no architect speaks, so the reviewer learns of the push
  # from that message alone. An implementer that published none leaves the reviewer with no wake
  # at all; inventing one here would hand the reviewer the head the implementer never named. What
  # the reviewer was given is kept whole in <run>/reviewer-wake.json, the cases apart in its
  # `wake`.
  wake=$(reviewer_wake)
  # One delivery per message, in Envoy's own order, which is what the reviewer's session would
  # have been handed; an implementer that published none leaves the prompt empty. Each line's
  # prefix is the rig's text and the message after it the implementer's own words: launch's guard
  # and rule 17 of health.ts read the prompt less each message of <run>/reviewer-wake.json, at the
  # end of its line here (run-names.ts's `promptOf`), and rule 17 reads the message as the
  # implementer's writing at the time Envoy accepted it.
  jq -r --arg key "$worker_key" '.[] | "Implementer → reviewer, \($key). \(.message)"' \
    <<<"$wake" >"$R/prompt.txt"
  jq -n --argjson wake "$wake" '{wake:$wake}' >"$R/reviewer-wake.json"
  # The run is scored on the halves it ran, which its boot record names: a reviewer it does not
  # boot has the line `skip reviewer <reason>` there, the reason completion-answers.ts's SKIPS
  # declares for it (skip_reason), which the score and health read against the same SKIPS. In
  # accept and crossing the implementer's answer is the run's one half, so no reviewer is booted
  # whatever it sent. In code-defect the implementer tells the architect and the reviewer of the
  # thread (Verification v47's k2 negative control), so a wake of none is a run that already
  # fails that message: booting a reviewer then puts it in front of an empty prompt, and its acts
  # are scored by no term the run could still pass. In correct the implementer must have sent a
  # wake, so the reviewer is booted on whatever it sent, an empty prompt included, and the empty
  # wake is the fail on `toldReviewer` it already was.
  if [ "$threads_case" = accept ] || [ "$threads_case" = crossing ] ||
    { [ "$threads_case" = code-defect ] && [ "$(jq length <<<"$wake")" -eq 0 ]; }; then
    skip=$(skip_reason threads-closed "$threads_case" reviewer)
    printf 'skip reviewer %s\n' "$skip" >>"$R/booted"
    note "$name: no reviewer half — $skip"
  else
    worker_boot "$name-reviewer" "$project" reviewer "$phase" "$completed"
  fi
  # What the run left on its issue: every handoff record, each with its body, and every result of
  # its check, beside the calls the gh stand-in recorded and the pushes the remote logged, which
  # are what the score reads; then the judge's readings of what its agents wrote (judge.ts's
  # `judgeWorker`).
  seed_ts capture-handoffs "$R/fixture.json" "$R"
  seed_ts capture-results "$R/fixture.json" "$R"
  judge_ts run "$R"
}

# scenario_case SCENARIO prints the case SCENARIO's own variable names, nothing for a scenario
# that takes none, and fails, naming the variable and every case it takes, when a cased scenario's
# variable names none: run-names.ts's `caseOf`, which reads the rig's one scenario table
# (SCENARIOS). Every cased scenario's variable is required, since each check that runs one names
# its case. A batch and each of its runs read the case through this, so a batch refuses a case no
# run would take, and the run's record carries the one word the scorer and health read back.
scenario_case() {
  local out status=0
  out=$(bun "$here/run-names.ts" case "$1" 2>&1) || status=$?
  [ "$status" = 0 ] || fail "$out"
  [ -z "$out" ] || printf '%s\n' "$out"
}

# run_named NAME prints the directory of the run named NAME (`<scenario>[-<case>]-<label>-<n>`),
# live or archived, and fails when there is none: the run whose record (cmd_run) names it, or a run
# recorded before the names left the world, whose directory is the name itself.
run_named() {
  local dir record
  for dir in "$work/runs/$1" "$work/archive/runs/$1"; do
    [ ! -e "$dir" ] && [ ! -L "$dir" ] || {
      printf '%s\n' "$dir"
      return 0
    }
  done
  for record in "$work"/runs/*.json "$work"/archive/runs/*.json; do
    [ -f "$record" ] && [ "$(jq -r .name "$record")" = "$1" ] && {
      printf '%s\n' "${record%.json}"
      return 0
    }
  done
  return 1
}

# A run's directory is $work/runs/<id>, an id of no meaning (twelve hex digits), and every path,
# variable and process its agents can read is built on that directory, so none of them names the
# run's scenario, case or label: the run's name, `<scenario>[-<case>]-<label>-<n>`, named its
# directory until 2026-11-16, and 16 of the 36 runs of the batch at c273084d reasoned from it.
# What the run is — its scenario, case, label, number, name, batch, where it ran, and askgate's
# mode where one is set — is its record, $work/runs/<id>.json beside the directory
# (run-names.ts's RunRecord), written before anything of the run starts, which the run's
# namespace covers with the rest of the work directory and which archive_run moves with it. Every
# reader of a run's scenario, case or label reads the record (run-names.ts's `runIdentity`).
cmd_run() {
  [ $# = 5 ] || fail "usage: rig.sh run <ask-on-message|measure-before-ask|tester-proof|reviewer-text-findings|threads-closed|brainstorm|askgate|spec-writing|spec-rewrite> <label> <n> <index> <start>"
  local scenario=$1 n=$3 index=$4 start=$5 id name run_case project taken mode=''
  load_label "$2"
  # A world whose push guard cannot find its check refuses every push, so it is refused before
  # anything of it is built (push_guard).
  push_guard >/dev/null
  # A host that refuses the namespace its agent runs in starts nothing of it (require_namespace).
  require_namespace
  # Nor does a tmux that starts no session for its agent (tmux_ready). A run its batch started
  # takes the batch's PATH, and with it the tmux the batch checked, which RIG_TMUX_CHECKED names:
  # that run is not checked again, and any other run is.
  if [ -z "${RIG_TMUX_CHECKED:-}" ] || [ "$RIG_TMUX_CHECKED" != "$(type -P tmux)" ]; then tmux_ready; fi
  [[ $n =~ ^[0-9]+$ ]] || fail "run number '$n' is not a number"
  [[ $index =~ ^[0-9]+$ && $start =~ ^[0-9]+$ ]] || fail "run index '$index' or batch start '$start' is not a number"
  # The run's services are its label's in its batch (services_up): its listener, built from the
  # label's own checkout, and that label's provenance, which the batch wrote with it.
  services_env=$(label_services "${P##*/}")
  [ -f "$services_env" ] && [ -f "$(label_provenance "${P##*/}")" ] ||
    fail "a run needs its batch's services for label $2 ($services_env and $(label_provenance "${P##*/}")): run \`rig.sh batch $scenario 1 $2\`"
  # A cased scenario's run name carries its case, so each case's batches are numbered apart
  # (next_run) and no case's run takes another's name.
  run_case=$(scenario_case "$scenario") || exit 1
  if [ "$scenario" = askgate ]; then
    mode=${SKILL_SCENARIOS_ASKGATE_MODE:-}
    case $mode in '' | warn | block) ;; *) fail "SKILL_SCENARIOS_ASKGATE_MODE is '$mode', not warn or block" ;; esac
  fi
  name=$scenario${run_case:+-$run_case}-${P##*/}-$((10#$n))
  # A run never replaces another: a run of this name, live or archived, is an earlier run's
  # evidence, so the run is refused, naming it, before anything of it starts, and its exit
  # handler (stop_run, which stops processes of $R and archives $R) is set only once `mkdir`
  # has made $R this run's own.
  if taken=$(run_named "$name"); then
    fail "run $name is already at $taken; a run never replaces another (a batch numbers its runs above each label's highest)"
  fi
  id=$(openssl rand -hex 6)
  R=$work/runs/$id
  mkdir -p "$work/runs"
  mkdir -- "$R" ||
    fail "run $name could not be made at $R (mkdir says why above); a run never replaces another"
  jq -n --arg run "$id" --arg scenario "$scenario" --arg case "$run_case" --arg label "${P##*/}" \
    --argjson n "$((10#$n))" --arg name "$name" --arg batch "$tag-$start" --arg ran "$R" --arg mode "$mode" \
    '{run:$run, scenario:$scenario, case:$case, label:$label, n:$n, name:$name, batch:$batch, ran:$ran}
     + (if $mode == "" then {} else {mode:$mode} end)' >"$R.json"
  evidence=$R
  on_exit stop_run
  mkdir -p "$R/bin" "$R/sessions" "$R/cwd" "$R/logs"
  # What the shared scratch held before this run, so its exit records what the run made there.
  scratch_before
  cp "$P/built-from" "$R/built-from"
  # What each service of the run comes from, beside what its label was built from, before any of
  # it starts (services_from): health.ts holds every service of the label's own to built-from's
  # checkout.
  cp "$(label_provenance "${P##*/}")" "$R/services-from"
  # The run's own Legion project, computed here and nowhere else, so no run claims another's role
  # token or reads a message addressed to one: the run's scenario, case, label and number, and
  # its batch (run_project). Its recorder subscribes to this project's topics and its agents
  # publish on them, and a second computation of it that drifted from this one would leave every
  # send read as never made, with nothing anywhere saying so (start_recorder, worker_boot).
  project=$(run_project "$scenario" "$run_case" "${P##*/}" "$n" "$tag-$start")
  # Each runner names the run's tmux sessions by its id (launch), as no agent reads them.
  case $scenario in
  ask-on-message) run_ask_on_message "$id" ;;
  measure-before-ask) run_measure_before_ask "$id" ;;
  tester-proof) run_tester_proof "$id" "$project" "$index" "$start" ;;
  reviewer-text-findings) run_reviewer_text_findings "$id" "$project" "$index" "$start" "$run_case" ;;
  threads-closed) run_threads_closed "$id" "$project" "$index" "$start" "$run_case" ;;
  brainstorm) run_brainstorm "$id" ;;
  askgate) run_askgate "$id" "$run_case" ;;
  spec-writing | spec-rewrite) run_spec "$scenario" "$id" ;;
  *) fail "unknown scenario $scenario" ;;
  esac
  echo "$name ($id): $(tail -n 1 "$R/out.txt")"
}

# A batch's exit: its runs (each run's shell, daemon stand-in and fixture commands), the agents they
# started (each tmux session of a run whose record names this batch), every process whose HOME is
# under the runs directory, and the services.
# shellcheck disable=SC2329 # on_exit's trap runs it
stop_batch() {
  local s id sessions=() ids=()
  stop_tree "${runs_pid:-}"
  mapfile -t ids < <(jq -r --arg batch "$tag-$batch_start" 'select(.batch == $batch) | .run' \
    "$work"/runs/*.json "$work"/archive/runs/*.json 2>/dev/null || true)
  for s in $(tmux list-sessions -F '#{session_name}' 2>/dev/null || true); do
    for id in "${ids[@]}"; do
      case $s in "$tag-$id" | "$tag-$id"-*) sessions+=("$s") ;; esac
    done
  done
  for s in "${sessions[@]}"; do stop_panes "$s"; done
  stop_env_processes "$work/runs"
  services_down
}

cmd_batch() {
  [ $# -ge 3 ] || fail "usage: rig.sh batch <ask-on-message|measure-before-ask|tester-proof|reviewer-text-findings|threads-closed|brainstorm|askgate|spec-writing|spec-rewrite> <runs> <label>..."
  local runs=$2 label n index=0 run_case first
  batch_scenario=$1
  case $batch_scenario in ask-on-message | measure-before-ask | tester-proof | reviewer-text-findings | threads-closed | brainstorm | askgate | spec-writing | spec-rewrite) ;; *) fail "unknown scenario $batch_scenario" ;; esac
  # A cased scenario's case, refused here before any service starts, as each run refuses it. It
  # is part of each run's name, so it is part of the name the batch numbers from (next_run).
  run_case=$(scenario_case "$batch_scenario") || exit 1
  [ "$batch_scenario" != askgate ] || case ${SKILL_SCENARIOS_ASKGATE_MODE:-} in
    '' | warn | block) ;;
    *) fail "an askgate batch takes SKILL_SCENARIOS_ASKGATE_MODE unset, warn or block (got '$SKILL_SCENARIOS_ASKGATE_MODE')" ;;
  esac
  [[ $runs =~ ^[0-9]+$ ]] || fail "run count '$runs' is not a number"
  # A spec batch takes one run of one label. That limit was set when a spec batch alone numbered
  # its run above its label's highest; every batch does now (next_run, below), and the limit
  # stays as it was set. Each run has a Dispatch of its own (own_dispatch), so no run sees a
  # sibling's issue.
  case $batch_scenario in spec-writing | spec-rewrite)
    [ $((runs * ($# - 2))) = 1 ] ||
      fail "a $batch_scenario batch takes one run of one label (got $runs of $(($# - 2))); run each label as its own batch"
    ;;
  esac
  shift 2
  for label in "$@"; do load_label "$label"; done
  # Each run refuses a world whose push guard has no check (push_guard); the batch refuses it
  # here, before any service starts.
  push_guard >/dev/null
  # And a host that refuses the agents' namespace, likewise (require_namespace).
  require_namespace
  # And a tmux that starts no agent's session, likewise (tmux_ready). Each `rig.sh run` the batch
  # starts takes this PATH, and so the tmux checked here, and is not checked again (cmd_run).
  tmux_ready
  export RIG_TMUX_CHECKED
  # A spec run reads its source and the label's superpowers skills: refused here, before any
  # service starts.
  case $batch_scenario in spec-writing | spec-rewrite)
    source_dir "$batch_scenario" "${SKILL_SCENARIOS_SOURCE:-}" >/dev/null
    require_superpowers "$batch_scenario"
    ;;
  esac
  # services_from's pinned_omp and every run's agent run the rig's own mise (rig_mise); each
  # `rig.sh run` the batch starts inherits this PATH.
  rig_mise
  lock_services
  on_exit stop_batch
  # One listener per label, each built from that label's own checkout (services_up).
  services_up "$@"
  batch_start=$(date +%s)
  # Each label's runs are numbered from one above that label's highest run, live or archived
  # (next_run), read once this batch holds the work directory (lock_services): a batch never gives
  # a run the name of an earlier one, which cmd_run would refuse and archive_run could not keep.
  for label in "$@"; do
    first=$(next_run "$batch_scenario${run_case:+-$run_case}" "$label")
    for n in $(seq "$first" $((first + runs - 1))); do
      index=$((index + 1))
      printf '%s %s %s %s %s\n' "$batch_scenario" "$label" "$n" "$index" "$batch_start"
    done
  done | xargs -r -P "${SKILL_SCENARIOS_PARALLEL:-5}" -L 1 bash "$here/rig.sh" run &
  runs_pid=$!
  wait "$runs_pid"
}

# cmd_world LABEL RUN prints the world of the run directory RUN of label LABEL as world_paths
# declares it, from what the run recorded and what the pod holds now: its own directory as its
# record names it (run-names.ts's `ran`; a run recorded before the names left the world names it
# in its pane file's SKILL_SCENARIO_RUN; RUN itself for a run with neither), the work directory
# that holds it, the label's install there, its pane file, and the pin's install where the mise
# its pane's PATH names still has it, under the caller's TMPDIR and LEGION_WORKSPACE. A run with no
# record is read against the world such a run had, which showed its label's checkout (the
# checkout its built-from names) and this script's own directory (world_paths' CO). No reader
# judges a run by it: health.ts reads the world a run recorded, its namespace.txt, and refuses a
# rule that needs one where there is none (the architect's ruling of 2026-11-18).
cmd_world() {
  [ $# = 2 ] || fail "usage: rig.sh world <label> <run dir>"
  [[ $1 =~ ^[a-z0-9][a-z0-9-]*$ ]] || fail "label '$1' is not lower-case letters, digits and hyphens"
  local label=$1 dir=$2 R='' run_work P I co='' pin_dir='' pane_path mise
  if [ -f "$dir.json" ]; then
    R=$(jq -r .ran "$dir.json")
  else
    R=$(pane_var SKILL_SCENARIO_RUN "$dir/pane.env")
    co=$(sed -n 's/^checkout //p' "$dir/built-from" 2>/dev/null | head -n 1) || co=''
  fi
  [ -n "$R" ] || R=$(realpath -m -- "$dir")
  run_work=${R%/*/*}
  P=$run_work/profiles/$label
  I=$P
  [ ! -f "$P/install" ] || I=$run_work/installs/$(<"$P/install")
  [ -f "$dir.json" ] || [ -n "$co" ] || [ ! -f "$P/checkout" ] || co=$(<"$P/checkout")
  pane_path=$(pane_var PATH "$dir/pane.env")
  if [ -f "$P/pin" ] && [ -n "$pane_path" ] && mise=$(PATH=$pane_path && command -v mise); then
    pin_dir=$("$mise" where "$(<"$P/pin")" 2>/dev/null) || pin_dir=''
  fi
  world_paths "$R" "$run_work" "$I" "$dir/pane.env" "$pin_dir" "$co"
}

# cmd_class_search [CHECKOUT] runs the class search (class-search.ts) over this script's own
# checkout, or over CHECKOUT with the class-search.ts that checkout holds, so the search of a
# commit is that commit's own command over that commit's tree. Where bun exits nonzero it says
# so on stderr and exits with bun's status: a run at 7812df81 exited 2 after 549 s with both
# streams empty, and nothing said that bun was what had failed.
cmd_class_search() {
  [ $# -le 1 ] || fail "usage: rig.sh class-search [<checkout>]"
  local search=$here/class-search.ts status=0
  [ $# = 0 ] || search=$1/packages/pi-legion/scripts/skill-scenarios/class-search.ts
  [ -f "$search" ] || fail "class-search: $search is not there, so ${1:-this checkout} has no class search"
  bun "$search" || status=$?
  [ "$status" = 0 ] || printf 'class-search: bun exited %s\n' "$status" >&2
  return "$status"
}

# Sourced (`. rig.sh`), the script defines its functions and values and runs no command:
# rig.test.ts composes each scenario's run so, by services_up and cmd_run themselves, with the
# processes and servers a run reaches stubbed (launch's SKILL_SCENARIOS_COMPOSE_ONLY).
[ "${BASH_SOURCE[0]}" = "$0" ] || return 0

# Bash reads a script as it runs it, so the dispatch and its exit are one command line: a batch
# outlives any edit to this file, and resuming at its old offset would run whatever lies there now.
case "${1:-}" in
profile) shift && cmd_profile "$@" ;;
live-read) shift && cmd_live_read "$@" ;;
batch) shift && cmd_batch "$@" ;;
run) shift && cmd_run "$@" ;;
score) shift && SKILL_SCENARIOS_WORK=$work bun "$here/score.ts" runs "$work/runs" "$@" ;;
health) shift && SKILL_SCENARIOS_WORK=$work bun "$here/health.ts" "$@" ;;
world) shift && cmd_world "$@" ;;
class-search) shift && cmd_class_search "$@" ;;
judge) shift && cmd_judge "$@" ;;
identity) shift && cmd_identity "$@" ;;
judge-omp) shift && cmd_judge_omp "$@" ;;
*) sed -n '2,/^set -euo/p' "${BASH_SOURCE[0]}" | sed '$d' | sed 's/^# \{0,1\}//' && exit 2 ;;
esac; exit
