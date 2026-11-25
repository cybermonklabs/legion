#!/usr/bin/env bun
/**
 * `rig.sh health <run dir>...`: whether the world of each run could take what its agents did, and
 * whether the run stayed inside its own world. It does not score the run, it judges no agent, and
 * it writes nothing. A run is healthy when every rule of RULES holds:
 *   1. finished: the run's boot record, <run>/booted (completion-answers.ts's BOOTED), holds one
 *      decision for each role its scenario boots (ANSWERS' `boots`), in that order, the first a
 *      `boot` of the first role, each other a `boot <role>` or a `skip <role> <reason>` with the
 *      reason SKIPS declares for the run's scenario and case; a relaunch's repeated `boot` line of
 *      the same role is that one decision, and the `since <role> <time>` line rig.sh's boot_agent
 *      writes after each `boot` line is the time that attempt's launch began, no decision. Each
 *      role it booted finished on its own record: its
 *      own output ends with an `exit=` line (<run>/out-<role>.txt, where rig.sh's worker_boot
 *      moves it when it boots another agent after it, or <run>/out.txt for the last role booted),
 *      and its own session directory holds a transcript with an entry (<run>/sessions/*.jsonl for
 *      the first role, <run>/sessions/<role>/*.jsonl for each later one, a subagent's beside them
 *      none of it). A run with no record is unhealthy (`unfinished`);
 *   2. completions: every completion line names a role the scenario boots, and no `handoff/complete`
 *      was refused 4xx with an answer its world does not allow that role, by the answers a daemon
 *      of either shape gives it there or else by those of how the run's label's daemon takes a
 *      completion (completion-answers.ts's `ANSWERS`), read out of the daemon sources of the
 *      commit the run's built-from records, rebuilt out of the repositories the caller names
 *      (`--repo`, daemon-pane.ts's `recordedCheckout` and `labelDaemon`) only for an answer that
 *      turns on it. A run with such an answer whose rebuilt daemon cannot be read is unhealthy
 *      (`withDaemon`); one whose daemon cannot be rebuilt is not judged by this rule. The
 *      scorer's refused-completion rule reads this through the same function
 *      (`refusedCompletion`, which score.ts's `unscored` calls);
 *   3. taken: a scenario whose world must take a role's completion (`ANSWERS`' `takes`) took it at
 *      least once, with an answer its world allows, read as rule 2 reads it.
 *      reviewer-text-findings requires one of the reviewer's; threads-closed requires none;
 *   4. routes: no request the daemon stand-in answered 404 is of a route the run's label's daemon
 *      registers (`unservedRoute`): the routes read out of the API source of the commit the run's
 *      built-from records, rebuilt as rule 2 rebuilds it (daemon-pane.ts's `labelRoutes`), which
 *      the stand-in itself answers by (daemon-standin.ts's `ROUTES`). A 404 of a route that daemon
 *      does not register is that daemon's own answer, as an older daemon's (main ff2a10ef's) to
 *      this checkout's `claims/turn-outcome` is. A run with a 404 whose daemon's routes cannot be
 *      read is unhealthy, since which it was is unknown; one whose daemon cannot be rebuilt is not
 *      judged by this rule;
 *   5. failed claim: no turn outcome the daemon stand-in answered failed the claim (its line's
 *      `failed`, `failedClaim`): the daemon fails a claim whose provider refused turns up to its
 *      launch failure limit, stops its process and takes its secret, so what the run did after
 *      is a world no daemon serves;
 *   6. push guard: no tool result of any agent of the run, and no out*.txt, holds a line the public
 *      leak check or Legion's git and jj wrappers print when they refuse a push or cannot run
 *      (GUARD_REFUSAL). No world of this rig plants a secret, so a refusal is the guard refusing
 *      what a production pod would let through, or a guard that could not start;
 *   7. gh: no `gh` call the gh stand-in refused (calls.jsonl, exit not 0) with a refusal the
 *      scenario does not declare for the calling role (GH_DECLARED, `ghRefusal`), and no call
 *      naming an owner outside the project the world answered, with any status, otherwise than
 *      production's gh answers it (`outsideAnswer`, the architect's rulings (a) of 2026-11-22 and
 *      of 2026-11-23T05:52:25Z): a write refused exit 2, a `gh api` read of the label's wrapper's
 *      read grammar answered the exit the capture of production's answer holds for it, and failing
 *      closed where the capture holds none, any other `gh api` call refused exit 2, and any other
 *      gh command naming such an owner failing closed. The owners a call names are read by the
 *      selector table of the wrapper of the run's label, which the world refused it by
 *      (<run>/gh-selectors.json, gh-selectors.ts's `runSelectors`). A worker run with a call whose
 *      selectors cannot be read is unhealthy, since which refusal it was is unknown. A write naming
 *      no such owner that the stand-in refused as one nothing of the world models is rule 20's.
 *      Rules 7 and 20 read only a call an agent of the run made itself (`typedBy`: its first word
 *      stands after a `gh` the agent ran), so a `gh send-telemetry` gh itself starts after a
 *      command is no call of the run's;
 *   8. another run: no tool call of the run names a path under another run's directory
 *      (`otherRun`): a sibling under the same runs directory, an archived run, or a run of another
 *      batch, wherever it sits;
 *   9. another run's write: no tool call of the run reads a path another run of those given wrote
 *      last before the read (`otherRunWrite`). A run's own writes outside its directory, HOME and
 *      TMPDIR are recorded (`writesOf`) and are no verdict on it. Rules 8 and 9 and that record
 *      read a path as the mounts the run's namespace recorded make it (`mounted`): under /tmp it
 *      is the run's own <run>/tmp, and under /dev/shm or a path its world covers a tmpfs of the
 *      agent's launch alone, which no other run reads, and rule 8 judges such a name as written;
 *      under a path its world shows in either, it is that path. A run from before the
 *      rig gave each run a namespace (one with no <run>/namespace.txt) wrote and read the
 *      machine's paths, and is read so;
 *  10. outside its world: no tool call names a path its world hides — the scratch root, the issue
 *      workspaces, the work directory, every label's checkout — outside its own directory, its
 *      /tmp (a namespace's <run>/tmp) and every path its world shows again (`outsideWorld`). An
 *      ancestor of its own directory or of a path its world shows is in its world: the namespace
 *      mounts those under the tmpfs that hides the rest, so the ancestor holds them alone. Its own
 *      directory is the one its record names (`recorded`), never the directory health was given,
 *      a copy or an archive no world of the run showed (the tester's finding of
 *      2026-11-22T04:07:41Z). The world is rig.sh's one declaration of it (world_paths), as the run
 *      recorded it for its namespace (<run>/namespace.txt, `namespaceOf`). A run from before the
 *      namespace recorded none, and this rule does not judge it;
 *  11. another process: no tool call reads a /proc/<n>/environ or cmdline but the caller's own:
 *      /proc/self's, /proc/thread-self's, or its shell's (`$$`, `${$}`, `$BASHPID`, `${BASHPID}`)
 *      (`otherProcess`). It reads only a run from before the rig gave each run a namespace (one
 *      with no <run>/namespace.txt), which shared the pod's processes: in a run's own PID namespace
 *      every process its /proc shows is the run's own;
 *  12. credentials: no tool call names a path under a credentials path of the world the run
 *      recorded, /var/run/legion or /var/run/secrets by either name (`credentialReach`); a run
 *      from before the namespace recorded none, and this rule does not judge it;
 *  13. off-world network: no call reaches a host other than the loopback, the hosts its pane's URLs
 *      name and the model gateway its profile's models.yml names (`offWorld`): no `web_search`
 *      call that carries the world off it or came back with the rig's own code (`searchRefusal`,
 *      the architect's ruling of 2026-11-22T01:44:54Z, read as the tester's world-health.py reads
 *      it): one whose query names a word of the world's own (its repository's full name and each
 *      half, its issue's key and the key's project and its issue branch, read from world.json,
 *      `worldWords`), a word of the measurement by rule 17's own definition (`proseNames`), a
 *      string of 16 or more characters a route of the run's fixtures.json answers with on stdout,
 *      but the repositories and commits the world's workflow names and the messages and codes of
 *      GitHub's own that the rig's GitHub sources hold (`fixtureAnswers`, `githubWordsOf`: the
 *      architect's ruling of 2026-11-22T04:06:48Z), or a
 *      commit of the run's own remote by a word of 7 or more of its leading hexadecimal digits
 *      (`remoteCommits`), or whose result (paired with it by its `toolCallId`) holds a page of
 *      cybermonklabs/legion or sjawhar/legion, which hold this rig's code (PROJECT_PAGE); any
 *      other search is part of the world, whether or not it names the workflow; no host a `bash`
 *      command reaches (`bashHosts`), a here-document an interpreter runs
 *      included; no host an `eval` cell reaches (`evalHosts`): each `tool.<name>(…)` call of it by
 *      that tool's own reading, and every network URL in the rest of it, in any language; no
 *      http(s) URL a `read`, `fetch` or `grep` is given as its path; no network URL in a file the
 *      run wrote and then ran, where the agent that ran it reached it (`fileRuns`, `Keeper`); and
 *      no network URL in any string argument of a tool
 *      TEXT_TOOLS does not name, a tool nobody named included. A URL in text a call writes (a
 *      review body, a reply, a commit message, a file never run) is a link, not a reach, and so
 *      is a URL in a bash command that no command hands to something that may fetch it: one only
 *      `echo` or `printf` prints or `grep`, `head`, `tail` or `jq` reads, its output going to
 *      nothing that fetches (QUIET, `unfetched`), and a `for` list's word or an assignment's value
 *      the reader writes into each place its variable is read (`literalFlow`), a variable of the
 *      run's pane file read with its value from the text's start. One the reader cannot follow
 *      (into a shell, xargs or a file, through `eval`, to a command it does not know) reaches its
 *      host, and a fetcher whose URLs are not in the text is off the world: a curl or a wget that
 *      takes its URLs from a file (`-K`, `-i`, URL_FILES), and a URL that holds what the reader
 *      cannot expand (a variable, a command substitution) and names no host, given to curl or wget
 *      as an operand or curl's `--url`, to git as the remote of clone, fetch, pull, push,
 *      ls-remote or `remote add`, by xargs from items no literal URL is piped into, or in a
 *      command substitution (`unexpandedUrls`: `x=$(gh api …); curl -s "$x"`, ``curl -s `cmd` ``,
 *      `echo "$x" | xargs curl -s`, `x=$(curl -s "$URL")`), or to a fetch of a program an
 *      interpreter runs or an eval cell holds (`programFetches`: `urlopen(sys.argv[1])`). A bash
 *      command is read by each program's own grammar, in the positions that take a host
 *      (`commandHosts`): a remote in scp form, `[user@]host:path`, only where git, jj, scp, rsync
 *      or sftp take an address; the host operands of ssh, scp, sftp, rsync, nc, ncat and telnet
 *      and the ssh options that name one (`-J`, `-o ProxyJump`, `-o HostName`); a host `gh`'s
 *      `--hostname`, `-R HOST/OWNER/REPO` (or its URL) or GH_HOST names, which the run's gh
 *      serves only for github.com and the loopback; and a gh named by a path, or run as
 *      `legion gh`, that is not the run's own stand-in (<run>/bin/gh), whatever it names. A
 *      wrapper (sudo, env, timeout, nice, nohup, xargs, command, exec, time, `sh -c`) is read off,
 *      and the command it runs read as if it stood alone. One reach off the world is allowed, as
 *      production allows it (`githubRead`): a GET or a HEAD with no body (a `curl` or `wget`
 *      sending none, a `read` or `fetch` of the URL), or a `git ls-remote`, which reads refs
 *      alone (`refsRead`), of a public repository the world's own workflow names in a `uses:`
 *      line, read from the run's remote at world.json's seeded head (`workflowUses`, whose names
 *      a web search's query may carry though fixtures.json answers with them), on
 *      api.github.com, github.com, raw.githubusercontent.com or codeload.github.com
 *      (GITHUB_READ_HOSTS); any other method, a
 *      body, a fetch, a clone, a push and any other repository or host stay off the world;
 *  14. claim boot: no agent's claim boot was refused but a first attempt's at boot, before its
 *      first turn, which rig.sh relaunched once and keeps under <run>/boot-1/<role>/ (`claimBoot`):
 *      a world answer production's daemon meets with a relaunch. A run whose relaunch registered
 *      is judged as any other, on the second attempt's record; one refused on both attempts, and
 *      one refused after a turn, are not. It is read third, after `finished` and rule 15;
 *  15. service provenance: the run carries <run>/services-from (rig.sh's services_from, which
 *      cmd_run copies into the run before any agent starts), and every service of it whose scope
 *      is `label` — its Envoy listener, Dispatch server, plugins, model route, daemon facts and
 *      worker pane, each of LABEL_SERVICES among them — came from the checkout its built-from names
 *      (`serviceProvenance`): a service from another checkout paired the label's plugins with a
 *      service no deploy of theirs ran beside them, as one listener built from the head checkout
 *      paired main ff2a10ef's pi-envoy with a listener from after #2603 and lost the base runs'
 *      claim boot to its `409 not_requested`. It is read second, after `finished`: a world built
 *      from the wrong checkout explains every rule after it;
 *  16. Dispatch: no request of an agent of the run that the run's own Dispatch server answered
 *      with an error, 4xx or 5xx, by the server's own line of it (<run>/logs/dispatch.log), unless
 *      the scenario declares that answer for the agent's role (DISPATCH_DECLARED) or it is the 403
 *      every agent of every run may meet (`personDeclared`, `dispatchUndeclared`): Dispatch's to a
 *      route the run's label's Dispatch gives the `person` scope, which the world's shared agent
 *      token does not hold, and its agents' client calls, read out of the routes table and the
 *      client of the commit the run's built-from records, rebuilt as rule 2 rebuilds it
 *      (person-routes.ts's `labelPersonRoutes`). A run with a 403 whose label's routes cannot be
 *      read so is unhealthy. A request is the agent's whose daemon stand-in opened last before the
 *      request began (`agentBoots`); a run with no such log is unhealthy. It is read right after
 *      rule 7;
 *  17. names: a word of the measurement an agent of the run says, in its own text or thinking,
 *      makes the run unhealthy only where an input held it before any agent of the run wrote it
 *      (`namesSaid`, the leak test the tester's reader uses): the run's prompt or system
 *      prompt (<run>/prompt.txt, <run>/system-args and the prompt file it names, read through
 *      run-names.ts's `promptOf` and `systemPromptOf`, whose refusal of a
 *      <run>/reviewer-wake.json it cannot parse or a system prompt it cannot read fails the run
 *      under this rule), or an entry of a transcript that is not an
 *      agent's own — a user message, a tool result (so every file an agent read, every page it
 *      fetched and every web search's result), a message it
 *      was sent, an entry Oh My Pi put in its context. A message the rig relayed into the prompt
 *      from one agent of the run to the next (<run>/reviewer-wake.json) is no input: it is its
 *      sender's own writing, at the time Envoy accepted it, where the run holds the sender's
 *      transcript, and the rig's text where it does not. The word is run-names.ts's vocabulary,
 *      derived from its SCENARIOS, completion-answers.ts's LABELS and the run's own label, and its
 *      REMOVED_NAMES, as its `proseNames` reads a sentence (rule 13 reads a web search's query by
 *      it too): a distinctive word (`text-only`, `threads-closed`), a removed name
 *      (`skill-scenarios`, `LWEVAL`, `rig-reviewer`), a run's name in rig.sh's old shape, or a
 *      case word beside a label word (`defect head`), never a plain word alone (`head SHA`,
 *      `base branch`, `not a defect`). "Before" is the agent's first
 *      writing of the word anywhere, its tool calls' arguments included: earlier in the same
 *      transcript, or earlier by the entries' times across two (a message one agent sent another
 *      holds what the sender wrote first). A word the run's agents wrote before any input held it
 *      is their own, and is reported with no verdict (`ownNames`): the preflight at cc09c638 found
 *      "text-only" in three head runs that no skill file, user message or tool result held first.
 *      On 2026-11-16, 16 of the 36 runs of the batch at c273084d reasoned from the run's name,
 *      which every path the run gave its agents carried;
 *  18. github: the run's own GitHub stand-in (github-standin.ts, which rig.sh's start_dispatch
 *      gives the run's Dispatch as its GitHub) answered as GitHub does (`githubRefusal`): its
 *      log, <run>/logs/github.log, opens with the stand-in's one `listening` line and holds no
 *      second one, no `refused` line, no request outside github-routes.ts's GITHUB_ROUTES, no
 *      success but the status GitHub answers its route with, no answer of 400 or more the
 *      scenario does not declare (GITHUB_DECLARED), and no line that is not the stand-in's own.
 *      A route it refused changed what Dispatch told an agent whether or not an agent asked for
 *      it. A worker world (run-names.ts's SCENARIOS, `world: "worker"`), whose seeds make its
 *      Dispatch ask GitHub, must also have kept that record and a request in it: a log missing,
 *      empty, not opening with the `listening` line, or holding that line alone is a run whose
 *      record was not kept, never one with no refusals. It is read right after rule 16;
 *  19. head: the head the run's Dispatch compares results with is the pull request's, as the
 *      push's own event moves it in production (`headUnmoved`): the run's head watcher
 *      (head-watcher.ts, which rig.sh's start_heads starts) recorded every push of
 *      <run>/pushes.log to the pull request's branch (a `refs/heads/legion/` line) as stored,
 *      `"ok":true`, in <run>/logs/heads.log, and the branch of the run's own remote stands at
 *      world.json's seeded head or at a head the record stored. A worker world's record, and any
 *      pushing run's, missing, empty or not opening with the watcher's `opened` line; a push with
 *      no line or a line saying the write failed; and a branch that moved with no line storing
 *      it are each unhealthy: an agent that read `dispatch results` after it was told a pass
 *      stands at a head the pull request no longer has (3 of 20 recorded threads-closed runs of
 *      the case correct replaced). It is
 *      read right after rule 18;
 *  20. unmodelled write: no `gh` write of calls.jsonl (a command of gh's own that writes, as gh's
 *      source at the tag the worker image pins reads, gh-writes.json; a GraphQL call whose
 *      document runs a mutation; any other `gh api` call sent with a verb other than GET or
 *      HEAD; each read from production's side by gh-writes.ts, never by the world's router)
 *      went unanswered by every route and answer the world models (`unmodelledWrite`): one the gh
 *      stand-in recorded as answered by nothing (`handler` `unrouted`, the 404 the world refuses
 *      an unmodelled write with) or, in a record from before it kept `handler`, one the catch-all
 *      write route answered a success that changed nothing, which no answer of that world took.
 *      The world could not take what the agent did, where GitHub would have. It is read right
 *      before rule 7, which leaves such a write to it;
 *  21. credential printed: no `bash` call of the run runs `gh auth status` with `-t` or
 *      `--show-token`, alone or among joined short flags (`-at`), or `gh auth token`, each of which
 *      prints the GitHub token of the pane's gh (`credentialPrinted`), read off the command's raw
 *      text: a `-t` after `;`, `&`, `|` or on a later line is another command's. Production
 *      answers both with the token, which no stand-in reads, so the world refuses both (rule 7
 *      would name that refusal); the run is the agent printing its credential, a finding the
 *      tester reports (the architect's ruling of 2026-11-18). It is read right before rule 20;
 *  22. capability report: no claim of the run sent a `claims/ready` after its boot's
 *      (`capabilityReport`), read off <run>/daemon.log: each stand-in's part of it, after its
 *      opening line, serves one claim, whose first `claims/ready` answered 204 is its boot's. A
 *      later one is a session's capability report, whose rows the daemon normalises and the
 *      stand-in keeps as sent (daemon-standin.ts's `ReadyReport`, a residual of the class search,
 *      the architect's ruling of 2026-11-19T05:18Z), so from that call on the world's capability
 *      rows are not production's. None of round 25's 69 agents sent one. It is read right after
 *      rule 5;
 *  23. system git configuration: every run given of the run's batch (its record's `batch`,
 *      run-names.ts's RunRecord) records the same system git configuration in its
 *      services-from as the run (`systemGitconfig`): rig.sh's services_from line
 *      `system-gitconfig`, the file the panes' git reads and its digest, or `none`. A batch's
 *      runs share one pod, whose file every run's world shows as the pod holds it (rig.sh's
 *      world_paths), so two runs of one batch that record two ran their agents' git on two
 *      system configurations, and the batch's arms in two worlds. A run with no record, or whose
 *      services-from holds no such line (one recorded before 2026-11-19), is compared with no
 *      other. It is read right after rule 14;
 *  24. unmerged head: every push of pushes.log to the issue branch leaves a head whose history
 *      holds the base's tip on the run's remote (`unmergedHead`), so the head merges into the base
 *      and the world's test merge is GitHub's. A head without it (a force-push of another
 *      history, which the remote takes as GitHub takes one on an unprotected branch) may not
 *      merge, and for one that does not GitHub keeps the last test merge it made, where the world
 *      answers none (gh-standin.ts's `heldRefs`, a residual of the class search). It is read right
 *      after rule 19;
 * Rules 6, 8 to 13, 17 and 21 read every transcript of the run (`<run>/sessions/**\/*.jsonl`: each
 * agent's and each subagent's) through score.ts's readers. The run's own directory is the one its
 * record names (run-names.ts's `ran`; for a run recorded before the names left the world, the one
 * its pane file names, `SKILL_SCENARIO_RUN`), which the transcripts' paths are written under, and
 * the directory health was given, which differ for a run read from an archive or a copy. A run's
 * scenario, case and label are its record's (run-names.ts's `runIdentity`), and a run recorded
 * before the names left the world has them in its directory's name. Rules 9 and 23 read the runs
 * they are given together, so a batch's runs are judged in one call: given one run, neither sees
 * another run's writes or system git configuration and neither flags anything, and rule 10 sees
 * no other label's checkout but those the run's namespace.txt covers and the built-from of each
 * run given names, while the other rules read each run alone. No rule reads the pod as it stands
 * now: the work directory, the judge's TMPDIR, HOME, PATH and working directory decide no
 * verdict.
 *
 * Prints one line per run judged, `<run>  healthy` or `<run>  unhealthy: <reason>`, naming the
 * first rule the run fails with the evidence it failed on; then, for each rule, `health: <rule>:
 * <n> of <m> runs`, the runs judged that rule alone flags; then, for each label of the runs given,
 * how often rig.sh relaunched an agent at its boot (`relaunches`: `health: boot relaunches,
 * <label>: <n> in <r> of <m> runs`, each relaunch an agent's first attempt kept under
 * <run>/boot-1/<role>/); then how many runs wrote outside their own directory, each by name with
 * the paths it wrote, and how many had a path appear in the shared scratch while they ran that
 * none of their calls wrote, each by name with those paths (`wroteOutside`), which is no verdict;
 * then `health: <n> of <m> runs unhealthy`. A run is judged unless a rule cannot judge it for an
 * input it did not record or the caller's repositories cannot rebuild (run-records.ts's `Refused`)
 * and every other rule passes it: such a run gets no verdict, and stderr names it with the input
 * (`health: no verdict on <run>: …`). Exits 0 when every run is healthy, 1 when one is unhealthy
 * and none is refused, and 2 when one is refused, the runs judged still printed; and 2, printing
 * no run, for no argument or for a path that is not a run directory of a scenario `ANSWERS` names
 * whose label is read (`labelOf`).
 *
 * `health.ts --boot-refusal <run dir> <role> <since> <sessions dir>` is the question rig.sh's
 * boot_agent asked after an agent's launch in every batch before the names left the world (it
 * asks `bun boot-refusal.ts` with the same arguments now), kept so a replay of such a batch reads
 * its boots through the entry they ran by: it prints the claim boot refusal that agent met before
 * its first turn, read from <run>/out.txt, the stand-in's lines of that role at or after <since>
 * and the transcripts under <sessions dir>, and exits 0 for a relaunch; exits 1 for none, a
 * refusal after a turn included; and 2 for a usage error. boot-refusal.ts answers it
 * (`askBootRefusal`).
 */
import { existsSync, lstatSync, readdirSync, readFileSync, statSync } from "node:fs";
import path from "node:path";
import { dispatchToolSpecs } from "@legion/contracts/dispatch-tools";
import { z } from "zod";
import { askBootRefusal, bootRefusal, entriesUnder, madeTurn } from "./boot-refusal";
import {
  ANSWERS,
  answerOf,
  BOOTED,
  DAEMON_LOG,
  DaemonLine,
  daemonLines,
  exited,
  LABELS,
  type LabelDaemon,
  labelOf,
  refusedCompletion,
  SKIPS,
  scenarioOf,
  takenCompletions,
} from "./completion-answers";
import {
  judgedOnDaemon,
  labelCheckout,
  labelRoutes,
  recordedCheckout,
  serves,
} from "./daemon-pane";
import {
  endpointPath,
  isPost,
  methodOf,
  readWorld,
  reviewsRoute,
  runNamed,
  submitsReview,
} from "./gh-call";
import { capturedRead, runsOnPaneToken } from "./gh-pane-token";
import { isObject } from "./gh-routes";
import { type GhSelectors, ghTargetOf, namesOwnerOutside, runSelectors } from "./gh-selectors";
import { graphqlOf, type Recorded, type ThreadRoute, threadCall, threadState } from "./gh-threads";
import { FIELD_REFUSED, PULL_NOT_FOUND } from "./gh-view";
import { callWrites } from "./gh-writes";
import { githubRouteOf } from "./github-routes";
import { labelPersonRoutes } from "./person-routes";
import { PRODUCTION_SHAPES } from "./production-shapes";
import { REST_REQUIRED } from "./rest-required";
import {
  entryKind,
  holds,
  promptOf,
  proseNames,
  RUN_ID,
  rigText,
  runIdentity,
  SCENARIOS,
  systemPromptOf,
  type Vocabulary,
  vocabulary,
  workerWorld,
} from "./run-names";
import { CALLS, Opened, Refused } from "./run-records";
import {
  ABSOLUTE_PATH,
  expandedWord,
  literalWord,
  outputs,
  SHELL_COMMAND,
  SHELL_WORD,
  type ShellToken,
  shellTokens,
  shellWord,
  shellWords,
  transcriptFiles,
  withoutBodies,
} from "./score";
import { type SessionEntry, sessionId, transcriptEntries } from "./transcript";

const USAGE = "usage: rig.sh health [--tmp-snapshot <file>] [--repo <git dir>]... <run dir>...";
const BOOT_USAGE = "usage: health.ts --boot-refusal <run dir> <role> <since> <sessions dir>";

/** A tool call's arguments, as a transcript records them: an object of named values. */
const ToolArgs = z.record(z.string(), z.unknown());
type ToolArgs = z.infer<typeof ToolArgs>;

/** A path a tool call of the run reached, when, and where the call is: a read, or a write
 * outside the run's own directory, HOME and TMPDIR. The path is the machine's: the one the call
 * names, as the run's namespace mounts it (`reachedIn`). */
interface Access {
  readonly path: string;
  /** Epoch milliseconds: the transcript entry's time for a tool call, the file's modification
   * time for an entry rig.sh's scratch_clean recorded. */
  readonly at: number;
  /** The transcript the call is in, or the record that holds it. */
  readonly file: string;
  readonly tool: string;
}

/** An entry of a transcript an agent wrote or was given, or a file of the run it was given
 * (rule 17): where it is (a transcript's path under the run, `<run>/prompt.txt`, or
 * `<run>/reviewer-wake.json` for a message the rig relayed), its kind, its text, its place in that
 * transcript or record (-1 for a file of the run, which every agent was given before it wrote
 * anything) and its time (NaN where the entry has none). */
interface TracedPart {
  readonly file: string;
  readonly kind: string;
  readonly text: string;
  readonly index: number;
  readonly at: number;
}

/** What an input entry of a transcript is, as rule 17's sentence names it. */
function inputKind(entry: SessionEntry): string {
  const role = entry.message?.role;
  if (role === "toolResult") return `the result of ${entry.message?.toolName ?? "a tool call"}`;
  if (role !== undefined) return `a ${role} message`;
  if (entry.type === "custom_message") return `the ${entry.customType ?? "custom"} message`;
  if (entry.customType !== undefined) return `the ${entry.customType} entry`;
  return `its ${entry.type ?? "untyped"} entry`;
}

/** All an input entry of a transcript can put in the agent's context: a message's parts, a
 * delivered message's content, a custom entry's data. */
function inputText(entry: SessionEntry): string {
  const parts = (entry.message?.content ?? []).map((part) => part.text ?? "");
  const content =
    entry.content === undefined
      ? ""
      : typeof entry.content === "string"
        ? entry.content
        : JSON.stringify(entry.content);
  const data = entry.data === undefined ? "" : JSON.stringify(entry.data);
  return [...parts, content, data].filter((one) => one !== "").join("\n");
}

/** One run as the rules read it. */
interface Run {
  readonly dir: string;
  readonly scenario: string;
  readonly label: string;
  /** The run's own directory: the one its record (or, for a run recorded before the names left
   * the world, its pane file) names, and the one health was given. */
  readonly own: readonly string[];
  /** The run's own directory as its record (or, for a run recorded before the names left the
   * world, its pane file) names it, the one its agents ran in; none where neither names one.
   * Never the directory health was given, a copy or an archive no world of the run showed. */
  readonly recorded: readonly string[];
  /** Whether the run's agents ran in a namespace of the run's own (NAMESPACE), whose mounts the
   * run recorded (`namespaceOf`): a path under /tmp is the run's own or one its world shows, a
   * path under /dev/shm is one its world shows or a tmpfs of the agent's launch alone, as a path
   * its world covers is (`mounted`), and every process /proc shows is the run's own
   * (`otherProcess`). */
  readonly namespaced: boolean;
  /** The git directories the caller named (`--repo`), which the sources of each run's label are
   * rebuilt from by the commit its built-from records (daemon-pane.ts's `recordedCheckout`),
   * never read off the pod's checkout. */
  readonly repos: readonly string[];
  /** Every tool call of every transcript of the run, with the transcript it is in, when, and the
   * text of the result that transcript holds for it (its `toolCallId`), where it holds one. */
  readonly calls: readonly {
    file: string;
    tool: string;
    args: ToolArgs;
    at: number;
    result?: string;
  }[];
  /** The transcript of each session of the run, by the session id its header names
   * (`sessionId`): the agent whose transcript it is sent each message Envoy recorded that session
   * as the sender of (rule 17). */
  readonly sessions: ReadonlyMap<string, string>;
  /** Every tool result's text of every transcript of the run, with the transcript it is in. */
  readonly results: readonly { file: string; text: string }[];
  /** Every text and thinking part an agent of the run wrote, with the transcript it is in, its
   * kind (`text` or `thinking`), the entry's place in that transcript and its time. */
  readonly said: readonly TracedPart[];
  /** Every assistant entry of the run, its text, thinking and tool calls' arguments together:
   * all an agent wrote, and when (rule 17). */
  readonly wrote: readonly TracedPart[];
  /** Every other entry of every transcript, each an input of the agent whose transcript it is in:
   * a user message, a tool result, a message delivered, an entry Oh My Pi kept (rule 17). */
  readonly heard: readonly TracedPart[];
  /** What the run wrote outside its own directory, HOME and TMPDIR, and what appeared in the
   * shared scratch while it ran (`writesOf`). */
  readonly writes: readonly Access[];
  /** Every path a tool call of the run reads (`readsOf`). */
  readonly reads: readonly Access[];
  /** The entries at the top of the shared scratch when the run started, from its
   * scratch-before.txt (rig.sh's scratch_before, `scratchRecords`); undefined for a run with no
   * record. */
  readonly before: readonly string[] | undefined;
}

/** The run directory `dir` names, with its scenario and label, or why it is none: a run is a
 * directory the rig made, so a symlink, read through, would be judged by what the run does not
 * hold, and is refused as score.ts's `runsOf` refuses one, naming its kind. */
function runOf(dir: string): { scenario: string; label: string } | string {
  const stats = lstatSync(dir, { throwIfNoEntry: false });
  if (stats === undefined) return `${dir} is not a directory`;
  const kind = entryKind(stats);
  if (kind !== "directory") return `${dir} is a ${kind}, not a run directory`;
  try {
    const scenario = scenarioOf(dir);
    if (scenario === undefined)
      return `${dir} is no run of a scenario health reads (completion-answers.ts's ANSWERS)`;
    return { scenario, label: labelOf(dir) };
  } catch (error) {
    return error instanceof Error ? error.message : String(error);
  }
}

/** The run's pane file (rig.sh's base_env and worker_boot write it), as its variables; empty for
 * a run that has none. */
function paneOf(dir: string): Map<string, string> {
  const file = path.join(dir, "pane.env");
  const vars = new Map<string, string>();
  if (!existsSync(file)) return vars;
  for (const line of readFileSync(file, "utf8").split("\n")) {
    const equals = line.indexOf("=");
    if (equals > 0) vars.set(line.slice(0, equals), line.slice(equals + 1));
  }
  return vars;
}

/** The world rig.sh's launch writes for the namespace it starts each agent of a run in
 * (<run>/namespace.txt, world_paths' lines), which private_world mounts. A run that holds it ran
 * there and is read by it (`namespaceOf`); a run from before the rig made a namespace holds none,
 * and its agents wrote and read the machine's own paths. */
const NAMESPACE = "namespace.txt";

export function loadRun(
  dir: string,
  scenario: string,
  label: string,
  repos: readonly string[] = []
): Run {
  const pane = paneOf(dir);
  // Where the run ran: its record's `ran`, or, for a run recorded before the names left the
  // world, the run its pane file names.
  const identity = runIdentity(dir);
  const ranAt = typeof identity === "string" ? undefined : identity.ran;
  const recorded = [ranAt ?? pane.get("SKILL_SCENARIO_RUN")]
    .filter((one): one is string => one !== undefined && one !== "")
    .map((one) => path.posix.normalize(one));
  const own = [path.posix.normalize(path.resolve(dir)), ...recorded];
  const writable = [
    ...own,
    ...[pane.get("HOME"), pane.get("TMPDIR")].filter(
      (one): one is string => one?.startsWith("/") === true
    ),
  ];
  const ran = { dir, label, namespaced: existsSync(path.join(dir, NAMESPACE)) };
  const calls: { file: string; tool: string; args: ToolArgs; at: number; result?: string }[] = [];
  const sessions = new Map<string, string>();
  const results: { file: string; text: string }[] = [];
  const said: TracedPart[] = [];
  const wrote: TracedPart[] = [];
  const heard: TracedPart[] = [];
  for (const full of transcriptFiles(dir)) {
    const file = path.relative(dir, full);
    const entries = transcriptEntries(full);
    const id = sessionId(entries);
    if (id !== undefined && !sessions.has(id)) sessions.set(id, file);
    // Each call of the transcript by its id, and the text of the result answering each id: a
    // call's result is the last one the transcript holds for it (rule 13 reads a web search's).
    const asked: [id: string, call: (typeof calls)[number]][] = [];
    const answers = new Map<string, string>();
    for (const [index, entry] of entries.entries()) {
      const message = entry.message;
      const at = Date.parse(entry.timestamp ?? "");
      if (message?.role === "assistant") {
        const own: string[] = [];
        for (const part of message.content ?? []) {
          if (part.type === "toolCall") {
            const call = {
              file,
              tool: part.name ?? "tool",
              args: ToolArgs.safeParse(part.arguments).data ?? {},
              at,
            };
            calls.push(call);
            if (part.id) asked.push([part.id, call]);
            own.push(JSON.stringify(part.arguments ?? {}));
          }
          // An agent's own words: its text, and its thinking (`{type: "thinking", thinking}`).
          const words = part.type === "thinking" ? part.thinking : part.text;
          if ((part.type === "text" || part.type === "thinking") && typeof words === "string") {
            said.push({ file, kind: part.type, text: words, index, at });
            own.push(words);
          }
        }
        wrote.push({ file, kind: "assistant", text: own.join("\n"), index, at });
      } else heard.push({ file, kind: inputKind(entry), text: inputText(entry), index, at });
      if (message?.role === "toolResult") {
        const text = (message.content ?? []).map((part) => part.text ?? "").join("");
        results.push({ file, text });
        if (message.toolCallId) answers.set(message.toolCallId, text);
      }
    }
    for (const [callId, call] of asked) {
      const text = answers.get(callId);
      if (text !== undefined) call.result = text;
    }
  }
  return {
    dir,
    scenario,
    label,
    own,
    recorded,
    repos,
    namespaced: ran.namespaced,
    calls,
    sessions,
    results,
    said,
    wrote,
    heard,
    writes: writesOf(ran, calls, writable),
    // A read of the launch's own tmpfs reads nothing another run wrote (`reachedIn`).
    reads: calls.flatMap((call) =>
      readsOf(call.tool, call.args).flatMap((read) => {
        const reached = reachedIn(ran, read);
        return reached === undefined ? [] : [{ ...call, path: reached }];
      })
    ),
    before: existsSync(path.join(scratchRecords(dir), "scratch-before.txt"))
      ? readFileSync(path.join(scratchRecords(dir), "scratch-before.txt"), "utf8")
          .split("\n")
          .filter((line) => line !== "")
      : undefined,
  };
}

/** Whether `target` is `dir` or a path under it. */
function inside(target: string, dir: string): boolean {
  return target === dir || target.startsWith(`${dir}/`);
}

/** A line the push guard prints when it refuses a push or cannot run, at the start of a line of
 * output, under any `<name>: ` prefix a caller puts before a child's line (jj prints the git it
 * runs as `git: …`): the shared check's `LEAK CHECK: refusing …`, `LEAK CHECK ERROR: …`,
 * `LEAK CHECK SELF-TEST FAILED: …` and `LEAK CHECK: cannot read the visibility …`, and the
 * `LEAK CHECK SETUP ERROR: …` of Legion's guard and its jj wrapper (packages/daemon/internal/
 * leakguard/errors.go, /opt/legion/bin/jj). A clean push prints `LEAK CHECK:` lines too — `clean`,
 * `read <n> commit(s)`, `self-test` — and they are none of these. A line of the check's own source
 * a tool printed (`echo "LEAK CHECK ERROR: …"`, a quoted Python string) does not start so. */
const GUARD_REFUSAL =
  /^[ \t]*(?:[\w.-]+: )*LEAK CHECK(?: SETUP ERROR\b| ERROR\b| SELF-TEST FAILED\b|: refusing\b|: cannot read the visibility\b).*$/m;

/** Rule 4: the first push-guard refusal an agent of the run was shown, in a tool result of any
 * transcript or in its own output. */
export function guardRefusal(run: Pick<Run, "dir" | "results">): string | undefined {
  const texts = [...run.results, ...outputs(run.dir).map(([file, text]) => ({ file, text }))];
  for (const { file, text } of texts) {
    const found = GUARD_REFUSAL.exec(text);
    if (found !== null)
      return `push guard: ${file} holds the guard's refusal "${found[0].trim().slice(0, 240)}"`;
  }
  return undefined;
}

/** GitHub's answer to a role App's read of a branch's classic protection, which the world's
 * protection route gives for every base (rig.sh's fixture_write): the App's token may not read a
 * repository's administration (production-shapes.json's `branches-main-protection`). */
const PROTECTION_403 = "gh: Resource not accessible by integration (HTTP 403)";
/** The gh stand-in's 422s, each GitHub's own answer to that call (gh-standin.ts): the create of a
 * review with an event GitHub does not take, a REQUEST_CHANGES or COMMENT with no body, or a
 * pending review while one is pending (`reviewCreate`); and the submit of a review that is not
 * pending, or with no event GitHub takes (`reviewSubmission`). */
const CREATE_422 = "review create 422";
const SUBMIT_422 = "review submit 422";
/** The gh stand-in's own fallback for a call no route of the world answers, `gh: Not Found (HTTP
 * 404)`: the world is missing an answer GitHub would give, so it is never declared. */
const UNROUTED = "gh: Not Found (HTTP 404) (no route of the world answers it)";
/** The world holds no workflow run (rig.sh's fixture_write: its checks are check runs of no
 * Actions workflow), so three of its routes give the refusal production gives for a run that is
 * not there: GitHub's 404 for the jobs of a run (`…/actions/runs/<id>/jobs`, read back from the
 * call by `JOBS`, since another route's 404 is another refusal), gh's own for `gh run view` or
 * `gh run watch <id>` of such a run, and gh's for `gh run view` or `gh run watch` naming no run
 * where no terminal can pick one. */
const JOBS_404 = "gh: Not Found (HTTP 404) (the jobs of a workflow run the world holds none of)";
const RUN_404 = "failed to get run: HTTP 404: Not Found";
const VIEW_ID_REQUIRED = "run or job ID required when not running interactively";
const WATCH_ID_REQUIRED = "run ID required when not running interactively";
const NO_RUN = [JOBS_404, RUN_404, VIEW_ID_REQUIRED, WATCH_ID_REQUIRED];
/** `repos/<owner>/<repo>/actions/runs/<id>/jobs`, with the leading slash and the query string
 * `gh api` takes. */
const JOBS = /^\/?repos\/[^/]+\/[^/]+\/actions\/runs\/\d+\/jobs(?:\?.*)?$/;
/** What production's gh answers a call naming an owner outside the world's project, as the
 * architect's rulings of 2026-11-22T16:36:59Z and 17:57Z read it (ruling (a), on the tester's
 * capture, dispatch://INFRA-764/artifact/infra-764-pinned-actions-capture-txt, and as the
 * tester's reader world-health.py v79 reads it), and of 2026-11-23T05:52:25Z: ruling (a) means "as
 * production answers", and the capture is the record of production's answer, its 404s included.
 * The call is read for such an owner by the label's wrapper's own selector table (`ghTargetOf`)
 * whatever its status, before any route, and each answer production does not give is named
 * (`outsideAnswer`):
 *   - a write (gh-writes.ts's `callWrites`) is refused by the Legion gh wrapper, exit 2 before gh
 *     runs (gh.go's runGh: a call naming an owner the bundle holds no directory for, but one that
 *     writes nothing, R4 of INFRA-3399), so another non-zero status is OWNER_NOT_REFUSED;
 *   - a `gh api` read whose every word is in the label's wrapper's read grammar (gh-pane-token.ts's
 *     `runsOnPaneToken`: gh_reads.go's ghAPIReadOptions, `-X` exactly GET, no method override) is
 *     run on the issue's token and answered by GitHub with the exit the capture holds for it
 *     (gh-pane-token.ts's `capturedRead`: v4's round 33 reads of the pinned actions, exit 0, its
 *     round 34 reads of a tag object GitHub has not got, its 404 and exit 1, and of the commits
 *     and the tags of each action, exit 0), so any other
 *     status is OUTSIDE_READ_UNANSWERED; one the capture holds no answer for is one whose
 *     production answer is not known, and fails closed whatever its status
 *     (OUTSIDE_READ_UNCAPTURED), naming the call so that it can be captured. The rule before was
 *     "exit 0 or unhealthy", which voided round 34's world-health run a167a5bc6341 for
 *     production's own 404 and passed at exit 0 a read no capture holds;
 *   - any other `gh api` call (`-i` among them, measured refused at 17:55Z) is refused, exit 2, so
 *     any other status, 0 among them, is OUTSIDE_FORM_NOT_REFUSED;
 *   - any other gh command naming such an owner, and every read of a label whose wrapper reads no
 *     read grammar (main's before #4035), is one no production answer is held here for, and fails
 *     closed (OUTSIDE_UNHELD), as v79 reads a gh command other than `gh api`: a leaf read the
 *     wrapper runs on the token (`gh repo view actions/checkout --json name`) is answered exit 0 by
 *     GitHub, which the world's 404 is not; this also reads unhealthy a form outside the grammar the
 *     wrapper refuses exit 2 (`gh repo view … --web`) and every read of an older label, each a run
 *     replaced and never a wrong count.
 * No declaration takes any of them. */
const OWNER_NOT_REFUSED =
  "the refusal of an owner outside the project with another status than the Legion gh wrapper's 2";
const OUTSIDE_READ_UNANSWERED =
  "a read of an owner outside the project in the Legion gh wrapper's read grammar answered otherwise than production's gh, which runs it on the issue's token and exits as the capture of its answer holds";
const OUTSIDE_READ_UNCAPTURED =
  "a read of an owner outside the project in the Legion gh wrapper's read grammar that no capture of production's answer holds, so it fails closed: capture it";
const OUTSIDE_FORM_NOT_REFUSED =
  "a `gh api` call of an owner outside the project in a form outside the Legion gh wrapper's read grammar answered otherwise than the wrapper's refusal, exit 2";
const OUTSIDE_UNHELD =
  "a read of an owner outside the project by a gh command other than a `gh api` call of the label's read grammar, which no answer of production's gh held here covers, so it fails closed";
/** GitHub's answer to a role App's read of the user its token is (`gh api user`), which the
 * world's user route gives every role (rig.sh's fixture_write): an installation token is no
 * user's, and GitHub refuses it 403 (measured on 2026-11-17 from a Legion pod: `gh api user`, exit
 * 1, `Resource not accessible by integration`). Its text is PROTECTION_403's, so it is read back
 * from the call by `USER`. */
const USER_403 =
  "gh: Resource not accessible by integration (HTTP 403) (the user a role App's token is)";
/** `user` and the paths under it, with the leading slash and the query string `gh api` takes. */
const USER = /^\/?user(?:[/?].*)?$/;
/** gh's refusals of a read of a pull request, which the world's gh gives as gh does and records
 * by name (gh-view.ts's PULL_NOT_FOUND and FIELD_REFUSED): a `gh pr view` or `gh pr diff` naming
 * a pull request or a branch GitHub has not got, or any of `gh pr view`, `list` and `diff` naming
 * a repository it has not got (`GraphQL: Could not resolve to …`, `no pull requests found for
 * branch …`), and a `--json` naming a field gh does not take (`Unknown JSON field: …`), each exit 1
 * as measured on 2026-11-17 against cybermonklabs/legion. */
const PULL_MISSING =
  "GraphQL: Could not resolve to a PullRequest or a Repository, or no pull requests found for branch (gh's refusal of one GitHub has not got)";
const FIELD_UNKNOWN = "Unknown JSON field (gh's refusal of a --json field it does not take)";
/** The refusals every role of every scenario may meet, since production refuses each so for every
 * role: USER_403, since any role may ask GitHub who its token is; and PULL_MISSING and
 * FIELD_UNKNOWN, since any role may read a pull request, and production gives every role that
 * names one, a branch, a repository or a field it has not got gh's own refusal (the architect's
 * ruling of 2026-11-17). */
const GH_DECLARED_EVERY_ROLE: readonly string[] = [USER_403, PULL_MISSING, FIELD_UNKNOWN];

/** The gh refusals each scenario provokes on purpose, or that a correct agent meets in it, by the
 * role that meets them; every other refusal is unhealthy (rule 7). Both labels' worlds are one
 * GitHub, so the table names no label; calls.jsonl records no phase, so it names none. Declared:
 *   - the reviewer's PROTECTION_403, in both scenarios: the merge gate has the reviewer read the
 *     base branch's protection (skills/legion-worker/references/merge-gate.md), a reviewer reaches
 *     for the protection endpoint too, and GitHub answers a role App's read of it 403;
 *   - the reviewer's CREATE_422 and SUBMIT_422, in both scenarios: the create and the submit of a
 *     review are the reviewer's own step in both worlds, and the stand-in refuses one exactly
 *     where GitHub refuses it, so a reviewer that met one met GitHub;
 *   - NO_RUN, for every role of both scenarios: the merge gate has the reviewer find a check's
 *     run and its jobs, and pr-body.md has a role watch CI with `gh run watch` and `gh run view`;
 *     in a world that holds no workflow run those reads get production's own refusals, so a role
 *     that met one met production.
 * Not declared: the implementer's PROTECTION_403 and 422s, since it reads no protection and makes
 * no review in threads-closed; any UNROUTED, which is the world missing an answer; and any other
 * refusal of a route the worlds add later, until a row here says why a correct agent meets it. */
const GH_DECLARED: Readonly<Record<string, Readonly<Record<string, readonly string[]>>>> = {
  "reviewer-text-findings": { reviewer: [PROTECTION_403, CREATE_422, SUBMIT_422, ...NO_RUN] },
  "threads-closed": {
    reviewer: [PROTECTION_403, CREATE_422, SUBMIT_422, ...NO_RUN],
    implementer: NO_RUN,
  },
};

/** A call of calls.jsonl as the gh stand-in records it (gh-standin.ts's `exit`); the record's
 * opening line parses as one with no `as`. `handler` is what answered it, which a record from
 * before the stand-in kept it does not hold. */
const RecordedCall = z.looseObject({
  as: z.string().optional(),
  by: z.string().optional(),
  argv: z.array(z.string()).optional(),
  route: z.string().nullable().optional(),
  review: z.unknown().optional(),
  handler: z.string().optional(),
  exit: z.number().optional(),
});
type RecordedCall = z.infer<typeof RecordedCall>;
/** A route of the world's fixtures.json, as rig.sh's fixture_write writes it and the gh stand-in
 * matches it; the stand-in rewrites the file on a body edit and keeps these four. */
const Route = z.looseObject({
  match: z.string(),
  exit: z.number().optional(),
  stderr: z.string().optional(),
  answer: z.string().optional(),
});
type Route = z.infer<typeof Route>;
const Fixtures = z.looseObject({ routes: z.array(Route) });
/** A comparison of two refs, and the read of one review, the gh stand-in answers itself (its
 * `comparison` and `reviewRead`). A comparison is a basehead of two dots or of three: the stand-in
 * answers `<base>..<head>` with GitHub's 404 of a comparison, as GitHub answers it, measured on
 * 2026-11-21: `gh api -i repos/cybermonklabs/legion/compare/446a02fb..bbf69791` answered `HTTP/2.0
 * 404 Not Found` with `Date: Sat, 21 Nov 2026 08:43:19 GMT`, where `446a02fb...bbf69791` answered
 * 200 (08:43:20 GMT), so a run that meets that 404 met GitHub's own. */
const OWN_READ = /\/compare\/[^?]+\.\.|\/pulls\/\d+\/reviews\/\d+(?:\?|$)/;

/** How `call`, naming an owner outside the project of the world's repository `repo` by
 * `selectors`, was answered otherwise than production's gh answers it (OWNER_NOT_REFUSED's
 * comment: a write refused exit 2, a `gh api` read of the label's read grammar answered the exit
 * the capture holds for it and failing closed where it holds none, any other `gh api` call refused
 * exit 2, and every other read failing closed); undefined where production gives that answer.
 * `writes`: whether the call writes (`callWrites`). */
function outsideAnswer(
  call: RecordedCall,
  writes: boolean,
  selectors: GhSelectors,
  repo: string
): string | undefined {
  const argv = call.argv ?? [];
  if (writes) return call.exit === 2 ? undefined : OWNER_NOT_REFUSED;
  if (selectors.paneToken === undefined || argv[0] !== "api") return OUTSIDE_UNHELD;
  // Every worker pane that names its gh owners directory names its own GH_CONFIG_DIR, and the
  // pane's GH_REPO the world's repository (daemon-pane.go's pane), which the record keeps neither
  // of.
  const pane = { GH_CONFIG_DIR: "<the pane's own>", GH_REPO: repo };
  if (runsOnPaneToken(argv, selectors, repo, pane)) {
    const captured = capturedRead(argv);
    if (captured === undefined) return OUTSIDE_READ_UNCAPTURED;
    return call.exit === captured.exit ? undefined : OUTSIDE_READ_UNANSWERED;
  }
  return call.exit === 2 ? undefined : OUTSIDE_FORM_NOT_REFUSED;
}

/** The refusal the gh stand-in gave `call`, which names no owner outside the world's project
 * (`outsideAnswer` reads one that does) and exited with a status other than 0, or undefined
 * when that status is no refusal of the world's. The record keeps the status and not the text, so
 * the refusal is read back from the call the way the stand-in decided it:
 *   - a read of the pull request gh refused (`gh pr view`, `list` or `diff` of one, a branch or a
 *     repository GitHub has not got, or a `--json` field gh does not take) is the refusal the
 *     record names it by (PULL_MISSING, FIELD_UNKNOWN); another such read that exited non-zero
 *     was answered, its status its `--jq`'s;
 *   - the submit of a review (`reviewSubmission`) that left no review on the record was refused
 *     422 (SUBMIT_422); one that left a review was taken, and its status is its own `--jq`'s;
 *   - the create of a review (`reviewCreate`) a route matched that left no review was refused 422
 *     (CREATE_422); one that left a review was taken, as above;
 *   - a call a route matched is the route's refusal where the route has a status of its own (its
 *     stderr's first line; JOBS_404 for the jobs of a run and USER_403 for the user a token is,
 *     each read off the call; and for the route whose `run` answer the stand-in refuses `gh run
 *     view` and `gh run watch` with, RUN_404 for a call naming a run and the refusal of the
 *     command for one naming none, read off the call by gh-call.ts's `runNamed`), and otherwise was
 *     answered, its status being its `--jq`'s or `--json`'s over that answer, gh's exit on the
 *     errors of a GraphQL answer, or the refusal production gives where the stand-in reads the
 *     route's answer from the run and finds nothing there (a review, a ref or a commit the world
 *     has not got, gh-standin.ts's `fromRun`), which is GitHub's answer and is not judged;
 *   - a call no route matched is the stand-in's fallback 404 (UNROUTED) unless the stand-in
 *     answers it itself: `gh pr checks`, which exits 1 or 8 on the checks it printed as gh does,
 *     and 1 with gh's own line on a head that reports none, `gh pr diff`, a comparison and the
 *     read of one review (OWN_READ). Their non-zero statuses are not judged: each is a `--jq`
 *     over an answer, gh's own report of a check, or, for a comparison or a read of refs or a
 *     review the run has not got, the 404 GitHub gives too.
 * What this cannot see: the text an agent was shown, which calls.jsonl does not keep; and a jq
 * failure over an answer the world gave without a field GitHub's carries, which is the world's
 * incompleteness but no refusal. */
function ghRefusal(
  call: RecordedCall,
  routes: readonly Route[],
  repo: string | undefined
): string | undefined {
  const argv = call.argv ?? [];
  // gh's own refusals of a read of the pull request, which the record names (gh-view.ts).
  if (call.handler === PULL_NOT_FOUND) return PULL_MISSING;
  if (call.handler === FIELD_REFUSED) return FIELD_UNKNOWN;
  if (repo !== undefined && argv[0] === "api" && isPost(argv)) {
    if (argv.some((arg) => submitsReview(arg, repo) !== undefined))
      return call.review === undefined ? SUBMIT_422 : undefined;
    if (call.route !== null && argv.some((arg) => reviewsRoute(arg, repo)))
      return call.review === undefined ? CREATE_422 : undefined;
  }
  if (call.route !== null && call.route !== undefined) {
    // A read a route matched that the world still does not model (a `gh pr list --search`, GraphQL
    // it does not read), refused 404 and recorded `unrouted`, is the world missing an answer.
    if (call.handler === "unrouted") return UNROUTED;
    const route = routes.find((one) => one.match === call.route);
    if (route?.exit === undefined || route.exit === 0) return undefined;
    if (route.answer === "run")
      return runNamed(argv) !== undefined
        ? RUN_404
        : argv[1] === "watch"
          ? WATCH_ID_REQUIRED
          : VIEW_ID_REQUIRED;
    const refusal = (route.stderr ?? `exit ${route.exit}`).split("\n")[0];
    const reads = (path: RegExp) => argv[0] === "api" && argv.some((arg) => path.test(arg));
    if (refusal === "gh: Not Found (HTTP 404)" && reads(JOBS)) return JOBS_404;
    if (refusal === PROTECTION_403 && reads(USER)) return USER_403;
    return refusal;
  }
  if (argv[0] === "pr" && (argv[1] === "checks" || argv[1] === "diff")) return undefined;
  if (argv[0] === "api" && methodOf(argv) === "GET" && argv.some((arg) => OWN_READ.test(arg)))
    return undefined;
  return UNROUTED;
}

/** Whether a recorded `gh` call is one an agent of the run made itself. */
export type Typed = (call: { readonly by?: string; readonly argv: readonly string[] }) => boolean;

/** The word right after each `gh` a text runs — one by a path among them (`/usr/bin/gh`), and
 * one a subprocess call's argument list holds (`["gh", "pr", …]`), across its quotes and commas. */
const GH_WORD = /(?<![\w.-])gh["']?[\s,]+["']?([^\s"',;|&<>()]+)/g;

/** Which `gh` calls of calls.jsonl an agent of the run made itself (the architect's ruling of
 * 2026-11-18): those whose first word stands right after a `gh` (GH_WORD) in a text an agent of
 * the call's role ran — a bash command, an eval cell, or a file the run wrote and a call of that
 * role runs, as rule 13 credits a script (`fileRuns`). A `gh api` any of them names covers every
 * `gh api` call of the role, since a shell builds an endpoint out of what the text does not hold
 * (`gh api --method PUT "$ep"`). Every other record is a call some other program started, which
 * is no act of the agent's: gh itself starts `gh send-telemetry` after a command, a POST to
 * GitHub's telemetry service the world refuses as a write it does not model, and rules 7 and 20
 * read such a record as no call of the run's. A record whose role booted no agent of the run
 * (no `by`, a role the scenario boots no launch for) is held to every agent's texts; one that
 * names no word is counted. */
export function typedBy(run: Pick<Run, "scenario" | "calls">): Typed {
  const byRole = new Map<string, Set<string>>();
  const every = new Set<string>();
  const written: Written[] = [];
  const ordered = run.calls
    .map((call, index) => ({ call, index }))
    .sort((a, b) => a.call.at - b.call.at || a.index - b.index);
  for (const { call } of ordered) {
    const texts = [
      call.tool === "bash" ? (stringArg(call.args, "command") ?? "") : "",
      call.tool === "eval" ? (stringArg(call.args, "code") ?? "") : "",
      ...fileRuns(call, written).map(({ file }) => file.text),
    ];
    const role = launchOf(run, call.file);
    const words = byRole.get(role) ?? new Set<string>();
    for (const text of texts)
      for (const hit of text.matchAll(GH_WORD)) {
        words.add(hit[1] ?? "");
        every.add(hit[1] ?? "");
      }
    byRole.set(role, words);
    written.push(...record(writtenText(call), written, call.tool, call.file));
  }
  return ({ by, argv }) => {
    const [word] = argv;
    if (word === undefined) return true;
    return ((by === undefined ? undefined : byRole.get(by)) ?? every).has(word);
  };
}

/** Rule 7: the first `gh` call of calls.jsonl the world answered otherwise than production's gh
 * answers a call naming an owner outside the world's project (`outsideAnswer`, whatever its
 * status, so a read answered exit 0 in a form production refuses is caught), or the stand-in
 * refused with a refusal neither GH_DECLARED_EVERY_ROLE nor GH_DECLARED declares for the scenario
 * and the calling role. In a worker world each call is read first for such an owner, by the
 * selectors the world refused an owner by (gh-selectors.ts's `runSelectors`, which worker_boot
 * writes from the run's label's checkout), read at the first call that needs them. A run holding
 * none was answered by a world that refused no owner by them, and its calls are judged by their
 * routes. Only a call `typed` says an agent made is read (`typedBy`; every call, given none). */
export function ghUndeclared(
  run: Pick<Run, "dir" | "scenario">,
  typed: Typed = () => true
): string | undefined {
  const file = path.join(run.dir, CALLS);
  if (!existsSync(file)) return undefined;
  const fixtures = path.join(run.dir, "fixtures.json");
  const routes = existsSync(fixtures)
    ? Fixtures.parse(JSON.parse(readFileSync(fixtures, "utf8"))).routes
    : [];
  const read = readWorld(run.dir);
  const repo = "world" in read ? read.world.repo : undefined;
  let selectors: GhSelectors | undefined | null = null;
  for (const [index, line] of readFileSync(file, "utf8").split("\n").entries()) {
    if (line.trim() === "") continue;
    const raw: unknown = JSON.parse(line);
    const call = RecordedCall.parse(raw);
    if (call.as !== "gh" || call.exit === undefined) continue;
    if (!typed({ ...call, argv: call.argv ?? [] })) continue;
    const writes = callWrites({ ...(raw as object), argv: call.argv ?? [] });
    const named = `${CALLS}:${index + 1}, \`gh ${(call.argv ?? []).join(" ").slice(0, 160)}\` of the ${call.by ?? "unnamed role"}, exited ${call.exit}`;
    if (repo !== undefined && selectors === null) {
      try {
        selectors = runSelectors(run.dir);
      } catch (error) {
        return `gh refusal unknown: ${named}, and the selectors its world refused an owner outside the project by cannot be read: ${error instanceof Error ? error.message : String(error)}`;
      }
    }
    const outside =
      repo !== undefined &&
      selectors !== null &&
      selectors !== undefined &&
      namesOwnerOutside(ghTargetOf(call.argv ?? [], selectors, {}), repo)
        ? selectors
        : undefined;
    // A call naming no owner outside the project that exited 0 is no refusal, and a write the
    // world answered with no route and no answer of its own is rule 20's, which names it as the
    // write it is (`unmodelledWrite`). A call naming such an owner, a write among them, is
    // `outsideAnswer`'s whatever its status.
    if (outside === undefined && (call.exit === 0 || (writes && call.handler === "unrouted")))
      continue;
    const refusal =
      outside === undefined
        ? ghRefusal(call, routes, repo)
        : outsideAnswer(call, writes, outside, repo ?? "");
    if (refusal === undefined) continue;
    const declared = [
      ...GH_DECLARED_EVERY_ROLE,
      ...(GH_DECLARED[run.scenario]?.[call.by ?? ""] ?? []),
    ];
    if (declared.includes(refusal)) continue;
    return `gh refusal: ${named}: ${refusal}, which ${run.scenario} does not declare for that role`;
  }
  return undefined;
}

/** The regex the record names the route by that every POST, PATCH and PUT no route above it took
 * fell to: rig.sh's fixture_write wrote it until the world modelled each write it answers, and it
 * answered each such write `{id:6001, html_url}` and exit 0, and changed nothing. */
const CATCH_ALL_WRITE = "^gh api ";
/** A reply POSTed under a parent that is no review comment's number (a review thread's node id,
 * `PRRT_…`), which GitHub refuses itself: `gh api -i --method POST
 * repos/cybermonklabs/legion/pulls/2827/comments/PRRT_kwDOrig0001/replies -f body=…` answered
 * `HTTP/2.0 404 Not Found`, `{"message":"Parent comment not found"}` (`Date: Sun, 22 Nov 2026
 * 16:30:31 GMT`, the tester's read; the architect's ruling of 2026-11-22T16:32Z). Its path,
 * `endpointPath`. */
const UNNUMBERED_REPLY =
  /^repos\/[^/]+\/[^/]+\/pulls\/\d+\/comments\/(?!\d+\/replies$)[^/]+\/replies$/;

/** Rule 20: the first write of calls.jsonl, read from production's side (gh-writes.ts's
 * `callWrites`), that no route or answer of the world models.
 * The gh stand-in records what answered each call (gh-standin.ts's `handler`), and a write it
 * answered with no route and no answer of its own (`unrouted`) was refused 404 where GitHub
 * answers it: the world could not take it; but a reply under a parent that is no number
 * (UNNUMBERED_REPLY) the world so refused got GitHub's own 404. A write it answered with a success
 * that kept nothing of it (`keptNothing`) is named too. A record from before the stand-in kept
 * `handler` is read by the route that answered: a write the world's catch-all write route answered
 * (CATCH_ALL_WRITE), which no answer of that world took either — it filed no comment in a review
 * thread when the run's calls are replayed over the world's threads (gh-threads.ts's
 * `threadCall`, as the stand-in answered the REST reply and the inline comment under that route),
 * and it was no submit of a review (`submitsReview`, which the stand-in answered from the run's
 * own record) — was answered a success that changed nothing. Measured in INFRA-764 round 25:
 * runs/1e2a0aeced4b, calls.jsonl:23, the reviewer's update of its approval's body
 * (`PUT …/reviews/5002`), answered `{id:6001}` and exit 0 with the body unchanged, while every
 * rule read the run healthy since a route had matched. A record that names no route at all, as
 * the score's own rows write one, is no reading of the world's and is not judged. Only a call
 * `typed` says an agent made is read (`typedBy`; every call, given none), and every call moves
 * the threads all the same, since the world answered each. */
export function unmodelledWrite(
  run: Pick<Run, "dir">,
  typed: Typed = () => true
): string | undefined {
  const file = path.join(run.dir, CALLS);
  if (!existsSync(file)) return undefined;
  const fixtures = path.join(run.dir, "fixtures.json");
  const routes: ThreadRoute[] = existsSync(fixtures)
    ? Fixtures.parse(JSON.parse(readFileSync(fixtures, "utf8"))).routes
    : [];
  const read = readWorld(run.dir);
  const world = "world" in read ? read.world : undefined;
  const at = { routes, world };
  const threads = threadState(at, []);
  for (const [index, line] of readFileSync(file, "utf8").split("\n").entries()) {
    if (line.trim() === "") continue;
    const call = JSON.parse(line) as Partial<Recorded> & { by?: string; body?: string };
    if (!Array.isArray(call.argv)) continue;
    const recorded: Recorded = { ...call, argv: call.argv };
    // The threads as the world held them before this call, moved by every call before it in the
    // order the run made them, as the stand-in answered each.
    const filed = threadCall(at, threads, recorded);
    if ((call.as ?? "gh") !== "gh" || !typed(recorded) || !callWrites(recorded)) continue;
    const named = `${CALLS}:${index + 1}, \`gh ${call.argv.join(" ").slice(0, 160)}\` of the ${call.by ?? "unnamed role"}, exited ${call.exit}`;
    if (call.handler === "unrouted") {
      if (call.exit !== 0 && isPost(call.argv) && UNNUMBERED_REPLY.test(endpointPath(call.argv)))
        continue;
      return `unmodelled write: ${named}: no route or answer of the world models it, so it was refused 404 where GitHub takes it`;
    }
    if (keptNothing(recorded, call.body, routes))
      return `unmodelled write: ${named}: \`${call.handler}\` answered it a success that kept nothing of it, which no answer of the world models`;
    const submits =
      world !== undefined && call.argv.some((arg) => submitsReview(arg, world.repo) !== undefined);
    if (
      call.handler === undefined &&
      call.route === CATCH_ALL_WRITE &&
      filed === undefined &&
      !submits
    )
      return `unmodelled write: ${named}: the route every write once fell to (\`${CATCH_ALL_WRITE}\`) answered it a success that changed nothing, and no answer of its world models it`;
  }
  return undefined;
}

/** Whether the gh stand-in answered `call`, a write, by what keeps nothing of it (its record's
 * `handler`, and `body`, the pull request body it kept): a route's frozen answer (`route`), which
 * models no write but a `gh pr edit` that kept the body it sent; the pull request's answer to a
 * PATCH that kept no body (`pull-request`); or the graph's answer to a mutation one of whose
 * fields the world's graph holds no answer for (`graph`). Each is a success that changed nothing,
 * as INFRA-764's class search of writes found four spellings answered (2026-11-18): the create
 * of `…/reviews?x=1` by the create route's frozen `{id:6001}`, a PATCH of `…/pulls/<n>?x=1` by
 * the pull request with its old body, `gh pr edit --body ""` by the pull request's URL, and a
 * mutation sent to `gh api /graphql` by an empty answer. The stand-in models each now, so a record
 * holding one is a record written before it. */
function keptNothing(
  call: Recorded,
  body: string | undefined,
  routes: readonly ThreadRoute[]
): boolean {
  if (call.handler === "route") return body === undefined;
  if (call.handler === "pull-request") return methodOf(call.argv) === "PATCH" && body === undefined;
  if (call.handler !== "graph") return false;
  const graph = routes.find((route) => isObject(route.graph))?.graph;
  const mutation = isObject(graph) && isObject(graph.mutation) ? graph.mutation : {};
  const operation = graphqlOf(call).operation;
  return (
    operation?.type === "mutation" &&
    operation.selections.some((one) => one.kind !== "field" || !(one.name in mutation))
  );
}

/** The run's own Dispatch server's output, under the run directory: rig.sh's own_dispatch gives
 * each run a server of its own (start_dispatch, on a database of its own), which start_process
 * starts with its output appended to <run>/logs/dispatch.log, emptied first, so every line of it
 * is the run's. */
const DISPATCH_LOG = "logs/dispatch.log";
/** The message of the line the server writes for every request it served, when the request ends
 * (packages/envoy/internal/dispatch/accesslog's `Message`), matched whole: another of the server's
 * messages starts with the same words. */
const REQUEST_MESSAGE = "dispatch: request";
/** A `key=value` attribute of a line of Go's slog text handler, which the server logs with
 * (packages/envoy/cmd/dispatch/main.go): a bare value, or one Go quoted (strconv.Quote), as slog
 * writes a value that holds a blank, a quote, an `=` or a character it does not print. */
const SLOG_ATTRIBUTE = /(?:^| )([^\s="]+)=("(?:[^"\\]|\\.)*"|[^\s"]*)/g;

/** The attributes of a slog text line, a quoted value read between its quotes. No attribute the
 * rule reads holds a character Go escapes: the message is the access log's own, a method is an
 * HTTP token, a path is escaped (a quote or a backslash in it is a `%22` or a `%5C`), and the
 * rest are numbers and a time. */
function slogAttributes(line: string): Map<string, string> {
  const attributes = new Map<string, string>();
  for (const [, key = "", value = ""] of line.matchAll(SLOG_ATTRIBUTE))
    attributes.set(key, value.startsWith('"') ? value.slice(1, -1) : value);
  return attributes;
}

/** A request line of the server's log, as the rule reads it: when the request ended and how long
 * it took, its method, its path (escaped, without its query) and the status of the answer. The
 * line also names the route pattern it was served under and its caller, which the rule does not
 * read: a read made with the run's shared agent token names no session. */
const DispatchRequest = z.looseObject({
  time: z.string().refine((time) => Number.isFinite(Date.parse(time)), "no time"),
  elapsed_ms: z.string().regex(/^\d+$/).transform(Number),
  method: z.string(),
  path: z.string(),
  status: z
    .string()
    .regex(/^\d{3}$/)
    .transform(Number),
});
type DispatchRequest = z.infer<typeof DispatchRequest>;

/** HANDOFF_NOT_FOUND, Dispatch's answer to a read of the newest handoff of `phase` when the phase
 * holds none: the one 404 of that route (packages/envoy/internal/dispatch/api/handoffs.go,
 * `writeHandoff`). `dispatch handoff-read --issue <issue> --phase <word>` makes that read when it
 * names no `--version`, and the Legion plugin makes it at a completion, for the handoff the
 * completion reports (packages/pi-shared/src/handoff-record.ts, `findHandoffReport`). */
const noHandoff = (phase: string) => `GET /api/v1/issues/{key}/handoffs/${phase}/latest 404`;

/** The 403s `personDeclared` declares for a run's agents, each `<method> <pattern> 403`, or why
 * they cannot be read. */
type PersonDeclared = readonly string[] | { readonly unknown: string };

/** The error answers of Dispatch every agent of every run may meet, whatever its scenario and its
 * role, beside those DISPATCH_DECLARED declares for one: OAUTH_SCOPE_REQUIRED, Dispatch's 403 to
 * a route that needs the `person` scope, from a session of the shared agent token, which holds no
 * scope. Each agent of a run runs under the world's shared agent token, as a Legion pod's agents
 * run under production's, and production gives that session the same 403: the tester's own pod
 * was answered so for `dispatch send-to-agent` (2026-11-20T19:53:57Z), which posts to
 * /api/v1/agents/{session_id}/messages, and for `dispatch inbox` (2026-11-21T03:30:08Z), which
 * reads /api/v1/inbox, /api/v1/me/replies and /api/v1/credential-requests; `dispatch review`
 * posts to /api/v1/artifacts/{id}/versions/{number}/reviews, which needs it too. INFRA-764's round
 * 27 at efea8554 met the first: the reviewer of threads-closed-correct-base-1 messaged the
 * implementer that way. The routes are the class, never a list (the architect's ruling of
 * 2026-11-21): every route the label's own Dispatch gives the person scope (routes_table.go's
 * `routeScopes`) that its agents' client calls (envoy-client's dispatch-http.ts), read by
 * person-routes.ts's `labelPersonRoutes` out of the sources the run's built-from's commit holds,
 * rebuilt out of the caller's repositories (`recordedCheckout`, which refuses the run where it
 * cannot), so a person-scoped route main adds is declared at the label that has it. Each is
 * `<method> <pattern> 403`, as DISPATCH_DECLARED's are, and no other answer of those routes is
 * declared. Where those sources are of no shape that reading knows, why, the rebuilt directory
 * named by its commit. */
function personDeclared(run: Run): PersonDeclared {
  const { dir, commit } = recordedCheckout(run.dir, run.repos);
  try {
    return labelPersonRoutes(dir).map((route) => `${route} 403`);
  } catch (error) {
    return {
      unknown: (error instanceof Error ? error.message : String(error)).replaceAll(
        dir,
        `<commit ${commit}>`
      ),
    };
  }
}

/** The error answers of Dispatch each scenario provokes on purpose, or that a correct agent meets
 * in it, by the role that meets them, each `<method> <path> <status>` with `{<name>}` for a path
 * segment of any value; every other error answer of the run's Dispatch to an agent but a 403
 * `personDeclared` declares is unhealthy (rule 16). The server's line keeps the status of an answer
 * and not the code its body carried, so a declaration names a path whose status says which answer
 * it was. Declared: HANDOFF_NOT_FOUND for the newest `review` handoff, for the reviewer of
 * reviewer-text-findings: its world records no review before the reviewer writes its own, and the
 * reviewer reads that phase first, as the skill has it, and at a completion made before its own
 * write, which the plugin reads it for.
 * Not declared: HANDOFF_NOT_FOUND for the `plan`, `implement` and `test` handoffs, which every
 * worker world records before its first agent (seed.ts's plan, implement-handoff and
 * tester-result), or for threads-closed's `review`, which its world records too (review-handoff):
 * a world that lost one is missing an answer production gives. The `plan` one was declared for
 * every role while no world recorded a plan, and an issue past planning holds one in production.
 * Nor is any other error answer of any route, a `dispatch handoff-read --version` of a version
 * that is not there among them, which the command answers itself after a list Dispatch answers
 * 200, and so never reaches this log. */
const DISPATCH_DECLARED: Readonly<Record<string, Readonly<Record<string, readonly string[]>>>> = {
  "reviewer-text-findings": { reviewer: [noHandoff("review")] },
};

/** Whether `declared`, an entry of DISPATCH_DECLARED or of `personDeclared`, is the answer the
 * server gave `request`. */
function declares(declared: string, request: DispatchRequest): boolean {
  const [method, pattern = "", status] = declared.split(" ");
  const segments = pattern.split("/");
  const requested = request.path.split("/");
  return (
    request.method === method &&
    String(request.status) === status &&
    segments.length === requested.length &&
    segments.every((segment, at) => /^\{\w+\}$/.test(segment) || segment === requested[at])
  );
}

/** One decision of a run's boot record (<run>/booted, completion-answers.ts's BOOTED), which
 * rig.sh writes at the site that decides it: a role booted (`boot <role>`), or a role left
 * unbooted with the rig's reason (`skip <role> <reason>`). */
type BootDecision =
  | { readonly kind: "boot"; readonly role: string }
  | { readonly kind: "skip"; readonly role: string; readonly reason: string };

/** A line of the boot record: `boot <role>`; `since <role> <time>`, the time (UTC, to the
 * millisecond) rig.sh's boot_agent began that attempt's launch, the `since` it asks
 * boot-refusal.ts with, which it writes right after the attempt's `boot` line; or `skip <role>
 * <reason>`, the reason the rest of the line. */
const BOOT_LINE =
  /^(boot) (\S+)$|^(skip) (\S+) (\S.*)$|^(since) (\S+) (\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d\.\d{3}Z)$/;

/** The run's boot decisions in the order rig.sh wrote them, a relaunch's `boot` line of the role
 * booted on the line before it read as that same boot (boot_agent writes one per attempt), and a
 * `since` line no decision; undefined for a run with no record. Throws for a line of no such
 * shape, and for a `since` line that does not follow a `boot` line of its role. */
function bootDecisions(dir: string): BootDecision[] | undefined {
  const file = path.join(dir, BOOTED);
  if (!existsSync(file)) return undefined;
  const decisions: BootDecision[] = [];
  let previous = "";
  for (const [index, line] of readFileSync(file, "utf8").split("\n").entries()) {
    if (line === "") continue;
    const [, boot, booted, skip, skipped, reason, since, timed] = BOOT_LINE.exec(line) ?? [];
    const last = decisions.at(-1);
    if (boot !== undefined && booted !== undefined) {
      if (last?.kind !== "boot" || last.role !== booted)
        decisions.push({ kind: "boot", role: booted });
    } else if (skip !== undefined && skipped !== undefined && reason !== undefined) {
      decisions.push({ kind: "skip", role: skipped, reason });
    } else if (since !== undefined && timed !== undefined) {
      if (previous !== `boot ${timed}`)
        throw new Error(
          `${BOOTED}:${index + 1} is a \`since\` line that does not follow its role's \`boot\` line: ${line}`
        );
    } else {
      throw new Error(
        `${BOOTED}:${index + 1} is none of \`boot <role>\`, \`since <role> <time>\` and \`skip <role> <reason>\`: ${line}`
      );
    }
    previous = line;
  }
  return decisions;
}

/** Whether the session directory `dir` holds a transcript of its own with an entry: a `.jsonl`
 * directly in it. A subagent's transcript, one directory down (`<session>/<name>.jsonl`), and a
 * later agent's (`sessions/<role>/`) are not the agent's own. */
function holdsTranscript(dir: string): boolean {
  if (!existsSync(dir)) return false;
  return readdirSync(dir, { withFileTypes: true }).some(
    (entry) =>
      entry.isFile() &&
      entry.name.endsWith(".jsonl") &&
      transcriptEntries(path.join(dir, entry.name)).length > 0
  );
}

/** Rule 1: why the run did not finish, or undefined when it did. Which roles it booted is the
 * run's own record of it (<run>/booted), held to the scenario's one definition of them
 * (ANSWERS' `boots`, SKIPS), and never the definition read as the record: a run of code-defect
 * whose implementer sent the reviewer nothing boots no reviewer, and the first head run of
 * threads-closed at 7bf20811, whose reviewer's stand-in never started, left out.txt the
 * implementer's, its `exit=` line too, when worker_boot still copied it. So each role is read on
 * its own output and its own transcript, and a record of an earlier role never stands in for a
 * later one's.
 * The case is the one the run's record names (run-names.ts's `runIdentity`; a run recorded before
 * the names left the world names it in its directory's name, `<scenario>-<case>-<label>-<n>`),
 * which is the case rig.sh booted it for. */
export function unfinished(run: Pick<Run, "dir" | "scenario" | "label">): string | undefined {
  const decisions = bootDecisions(run.dir);
  if (decisions === undefined)
    return `no boot record: the run has no <run>/${BOOTED}, so which roles of ${run.scenario} it booted, and whether each finished, is unknown`;
  const roles = ANSWERS[run.scenario]?.boots ?? [];
  const definition = `${run.scenario} boots ${roles.map((role) => `the ${role}`).join(", then ")} (completion-answers.ts's ANSWERS)`;
  const first = decisions[0];
  if (first?.kind !== "boot" || first.role !== roles[0])
    return `boot record: <run>/${BOOTED} starts with ${first === undefined ? "no line" : `\`${first.kind} ${first.role}\``}, not \`boot ${roles[0]}\`; ${definition}`;
  for (const [at, decision] of decisions.entries()) {
    if (decision.role === roles[at]) continue;
    if (!roles.includes(decision.role))
      return `boot record: <run>/${BOOTED} decides the ${decision.role}, a role ${run.scenario} boots none of; ${definition}`;
    if (decisions.slice(0, at).some((earlier) => earlier.role === decision.role))
      return `boot record: <run>/${BOOTED} decides the ${decision.role} twice; ${definition}`;
    return `boot record: <run>/${BOOTED} decides the ${decision.role} where the ${roles[at]} comes; ${definition}`;
  }
  const missing = roles[decisions.length];
  if (missing !== undefined)
    return `boot record: <run>/${BOOTED} neither boots nor skips the ${missing}; ${definition}`;
  const identity = runIdentity(run.dir);
  const runCase = typeof identity === "string" ? "" : identity.case;
  for (const decision of decisions) {
    if (decision.kind !== "skip") continue;
    const declared = SKIPS[run.scenario]?.[runCase]?.[decision.role];
    if (decision.reason !== declared)
      return `undeclared skip: <run>/${BOOTED} skips the ${decision.role} because "${decision.reason}", ${declared === undefined ? `and ${run.scenario}${runCase === "" ? "" : ` ${runCase}`} declares no skip of it` : `where ${run.scenario} ${runCase} declares "${declared}"`} (completion-answers.ts's SKIPS)`;
  }
  const booted = decisions.filter((decision) => decision.kind === "boot");
  for (const [at, { role }] of booted.entries()) {
    const output = at === booted.length - 1 ? "out.txt" : `out-${role}.txt`;
    if (!exited(path.join(run.dir, output)))
      return `unfinished: the ${role}'s output, <run>/${output}, ${existsSync(path.join(run.dir, output)) ? "has no exit= line" : "is missing"}`;
    const sessions = at === 0 ? "sessions" : path.join("sessions", role);
    if (!holdsTranscript(path.join(run.dir, sessions)))
      return `unfinished: the ${role} left no transcript: <run>/${sessions} holds no .jsonl of its own with an entry`;
  }
  return undefined;
}

/** When each agent of the run was booted, in order, with its role: the opening line each daemon
 * stand-in writes to <run>/daemon.log when it starts listening (daemon-standin.ts). rig.sh's
 * worker_boot starts a stand-in of its own before it launches each agent and stops it once that
 * agent has exited, keeping it for a relaunch (boot_agent), so the k-th opening line is the boot
 * of the k-th role the run's boot record boots (`bootDecisions`); a boot past those, or any boot
 * of a run with no record, has no role. */
function agentBoots(run: Run): { readonly at: number; readonly role: string | undefined }[] {
  const file = path.join(run.dir, DAEMON_LOG);
  if (!existsSync(file)) return [];
  const roles = (bootDecisions(run.dir) ?? [])
    .filter((decision) => decision.kind === "boot")
    .map((decision) => decision.role);
  return readFileSync(file, "utf8")
    .split("\n")
    .filter((line) => line.trim() !== "")
    .flatMap((line) => {
      const opened = Opened.safeParse(JSON.parse(line));
      return opened.success ? [Date.parse(opened.data.at)] : [];
    })
    .map((at, index) => ({ at, role: roles[index] }));
}

/** Rule 16: the first request of the run's Dispatch log (DISPATCH_LOG) that the server answered
 * with an error, 4xx or 5xx, which neither DISPATCH_DECLARED for the scenario and the role of the
 * agent that made it nor, for a 403, the run's own label (`personDeclared`) declares: the agent
 * booted last before the request began, its line's time less its elapsed time (`agentBoots`).
 * rig.sh runs the agents one after another and seeds the world before the first and reads it back
 * after the last with seed.ts, which takes no answer that is not 2xx, so every error answer
 * between two boots is that agent's, its plugins' included. One before the first boot, or after a
 * boot past the roles the run's boot record boots, is no role's, and no declaration takes it. The
 * label's person-scoped routes are read at the first 403 of an agent's that DISPATCH_DECLARED
 * does not declare, and a run where they cannot be read is unhealthy, since whether that 403 was
 * production's own answer is unknown. A run with no log is unhealthy: every run health reads has
 * a Dispatch of its own, started with that log, so whether Dispatch refused an agent of the run is
 * unknown. */
function dispatchUndeclared(run: Run): string | undefined {
  const file = path.join(run.dir, DISPATCH_LOG);
  if (!existsSync(file))
    return `no Dispatch record: the run has no <run>/${DISPATCH_LOG}, so whether its Dispatch refused a call of its agents is unknown`;
  const boots = agentBoots(run);
  let person: PersonDeclared | undefined;
  for (const [index, line] of readFileSync(file, "utf8").split("\n").entries()) {
    const attributes = slogAttributes(line);
    if (attributes.get("msg") !== REQUEST_MESSAGE) continue;
    const parsed = DispatchRequest.safeParse(Object.fromEntries(attributes));
    if (!parsed.success)
      throw new Error(
        `${DISPATCH_LOG}:${index + 1} is no request line of Dispatch's: ${parsed.error.message}`
      );
    const request = parsed.data;
    if (request.status < 400) continue;
    const began = Date.parse(request.time) - request.elapsed_ms;
    const boot = boots.findLast((one) => one.at <= began);
    const role = boot?.role;
    const declared = role === undefined ? [] : (DISPATCH_DECLARED[run.scenario]?.[role] ?? []);
    if (declared.some((answer) => declares(answer, request))) continue;
    const call = `${DISPATCH_LOG}:${index + 1}, ${request.method} ${request.path}`;
    if (role !== undefined && request.status === 403) {
      person ??= personDeclared(run);
      if ("unknown" in person)
        return `dispatch refusal unknown: ${call} of the ${role}, answered 403, and which routes the ${run.label} label's Dispatch gives the person scope that its agents' client calls cannot be read: ${person.unknown}`;
      if (person.some((answer) => declares(answer, request))) continue;
    }
    if (boot?.role === undefined)
      return `dispatch refusal: ${call}, made ${boot === undefined ? "before the run's first agent was booted" : `after boot ${boots.indexOf(boot) + 1} of the run, past the roles its boot record boots`}, answered ${request.status}, which no role's declaration takes`;
    return `dispatch refusal: ${call} of the ${boot.role}, answered ${request.status}, which ${run.scenario} does not declare for that role`;
  }
  return undefined;
}

/** The run's own GitHub stand-in's log (github-standin.ts), start_process's log of the service
 * rig.sh's start_dispatch starts as `github`, emptied before it starts. */
export const GITHUB_LOG = "logs/github.log";
/** One answer of GitHub's own the stand-in gave a route of its table, which a scenario's world
 * provokes or a correct agent of it meets: the route's template and the status. */
export interface GithubDeclared {
  readonly route: string;
  readonly status: number;
  readonly reason: string;
}
/** The answers of 400 or more the GitHub stand-in may give in each scenario's world, by scenario:
 * every other one makes the run unhealthy (rule 18), and a `refused` line, a route the stand-in's
 * table does not hold, is never declared, since GitHub answers every route. None is declared: no
 * measured flow of any world met one. A row names the scenario it holds for, as GH_DECLARED's do,
 * so a 404 one world provokes is no declared 404 in another. */
export const GITHUB_DECLARED: Readonly<Record<string, readonly GithubDeclared[]>> = {};
/** A line of the stand-in's log: its `listening` line, or a request it answered. */
const GithubLine = z.union([
  z.object({ at: z.string(), listening: z.string() }),
  z.object({
    at: z.string(),
    method: z.string(),
    path: z.string(),
    accept: z.string(),
    status: z.number(),
    route: z.string().optional(),
    refused: z.string().optional(),
  }),
]);
/** Why a run with no record of its GitHub is unhealthy, by how the record is absent: a worker
 * world's missing, any run's empty or not opened, and a worker world's that holds no request. */
export const GITHUB_NOT_KEPT = {
  missing: `github: ${GITHUB_LOG} is missing, so the run kept no record of what its Dispatch asked GitHub`,
  empty: `github: ${GITHUB_LOG} is empty, so the run kept no record of what its Dispatch asked GitHub`,
  unopened: `github: ${GITHUB_LOG} does not open with the stand-in's listening line, so it is no record of the run's GitHub`,
  unasked: `github: ${GITHUB_LOG} holds the stand-in's listening line and no request, so the run's Dispatch never asked GitHub what its world's seeds make it ask`,
};

/** Rule 18: why the run's GitHub stand-in's record makes it unhealthy, or undefined. A worker
 * world (run-names.ts's `workerWorld`, the rig's scenario table), whose seeds make its Dispatch
 * ask GitHub (measured: the installation read, the token's mint and a commit read at each result
 * a seed records), must have kept the record and must hold a request in it (GITHUB_NOT_KEPT). Any
 * other run that has a record is read the same way but for that last, and one with none started
 * no stand-in. Then each line after the first: one the stand-in does not write, a second
 * `listening` line (the stand-in started twice), a refusal, a request that is no route of
 * GITHUB_ROUTES (`githubRouteOf`, read from the table whatever route the line names), a success
 * other than the one GitHub answers that route with (its `status`), and an answer of 400 or more
 * `declared` (GITHUB_DECLARED) does not declare for the run's scenario. */
export function githubRefusal(
  run: Pick<Run, "dir" | "scenario">,
  declared: Readonly<Record<string, readonly GithubDeclared[]>> = GITHUB_DECLARED
): string | undefined {
  const file = path.join(run.dir, GITHUB_LOG);
  const worker = workerWorld(run.scenario);
  if (!existsSync(file)) return worker ? GITHUB_NOT_KEPT.missing : undefined;
  const lines = readFileSync(file, "utf8").split("\n");
  if (lines[lines.length - 1] === "") lines.pop();
  if (lines.length === 0) return GITHUB_NOT_KEPT.empty;
  const parsed = lines.map((line) => {
    try {
      return GithubLine.safeParse(JSON.parse(line));
    } catch {
      return undefined;
    }
  });
  const first = parsed[0];
  if (first?.success !== true || !("listening" in first.data)) return GITHUB_NOT_KEPT.unopened;
  let requests = 0;
  for (const [index, read] of parsed.entries()) {
    if (index === 0) continue;
    const where = `${GITHUB_LOG}:${index + 1}`;
    if (read?.success !== true)
      return `github: ${where} is no line the stand-in writes: ${(lines[index] ?? "").slice(0, 200)}`;
    const line = read.data;
    if (!("method" in line))
      return `github: ${where} is a second listening line, so the stand-in started twice in one run`;
    requests += 1;
    const asked = `${line.method} ${line.path} (Accept: ${line.accept})`;
    if (line.refused !== undefined)
      return `github refusal: ${where}, ${asked} answered ${line.status}: ${line.refused}, which no declaration of ${run.scenario} can take, since GitHub answers every route`;
    const route = githubRouteOf(line.method, line.path, line.accept)?.route;
    if (route === undefined)
      return `github: ${where}, ${asked} is no route of GITHUB_ROUTES (github-routes.ts), whatever route the line names`;
    if (line.status < 400) {
      if (line.status === route.status) continue;
      return `github: ${where}, ${asked} answered ${line.status} on ${route.route}, where GitHub answers ${route.status}`;
    }
    const held = declared[run.scenario] ?? [];
    if (held.some((answer) => answer.route === route.route && answer.status === line.status))
      continue;
    return `github refusal: ${where}, ${asked} answered ${line.status} on ${route.route}, which ${run.scenario} does not declare (health.ts GITHUB_DECLARED)`;
  }
  return worker && requests === 0 ? GITHUB_NOT_KEPT.unasked : undefined;
}

/** The run's head watcher's record (head-watcher.ts), under the run directory. */
const HEADS_LOG = "logs/heads.log";
/** The record's first line, which the watcher writes when it starts. */
const HeadsOpened = z.object({ at: z.string(), opened: z.literal("heads.log") });
/** One line of that record after its first: a push to the pull request's branch, and whether its
 * head was stored. */
const HeadLine = z.looseObject({
  pushed: z.string(),
  ok: z.boolean(),
  error: z.string().optional(),
});
/** Why a run with no record of its heads is unhealthy, by how the record is absent: a worker
 * world's, or a run's that pushed, missing; and any run's empty or not opened. */
export const HEADS_NOT_KEPT = {
  missing: `head: ${HEADS_LOG} is missing, so the run kept no record of the heads its Dispatch was given`,
  empty: `head: ${HEADS_LOG} is empty, so the run kept no record of the heads its Dispatch was given`,
  unopened: `head: ${HEADS_LOG} does not open with the head watcher's opened line, so it is no record of the heads its Dispatch was given`,
};

/** Rule 19: why the head the run's Dispatch compares results with was not the pull request's, or
 * undefined. A worker world (`workerWorld`), whose head watcher rig.sh starts for the length of
 * the run, must have kept the record whether or not anything was pushed, as must any run that
 * pushed (HEADS_NOT_KEPT); a line after the first that the watcher does not write is unhealthy.
 * Then every push of pushes.log to `refs/heads/legion/…` must be on a line of the record that says
 * it was stored, and the issue branch of the run's own remote (<run>/remote.git, world.json's
 * `branch`) must stand at world.json's seeded head or at a head the record stored: a branch that
 * moved with no line storing it is a push the record missed, whatever pushes.log says. */
export function headUnmoved(run: Pick<Run, "dir" | "scenario">): string | undefined {
  const pushes = path.join(run.dir, "pushes.log");
  const pushed = existsSync(pushes)
    ? readFileSync(pushes, "utf8")
        .split("\n")
        .map((line, index) => ({ index, fields: line.split(" ") }))
        .filter(({ fields }) => /^refs\/heads\/legion\//.test(fields[1] ?? ""))
    : [];
  const file = path.join(run.dir, HEADS_LOG);
  if (!existsSync(file))
    return workerWorld(run.scenario) || pushed.length > 0 ? HEADS_NOT_KEPT.missing : undefined;
  const raw = readFileSync(file, "utf8").split("\n");
  if (raw[raw.length - 1] === "") raw.pop();
  if (raw.length === 0) return HEADS_NOT_KEPT.empty;
  const parse = (text: string): unknown => {
    try {
      return JSON.parse(text);
    } catch {
      return undefined;
    }
  };
  if (!HeadsOpened.safeParse(parse(raw[0] ?? "")).success) return HEADS_NOT_KEPT.unopened;
  const lines: z.infer<typeof HeadLine>[] = [];
  for (const [index, text] of raw.entries()) {
    if (index === 0) continue;
    const read = HeadLine.safeParse(parse(text));
    if (!read.success)
      return `head: ${HEADS_LOG}:${index + 1} is no line the head watcher writes: ${text.slice(0, 200)}`;
    lines.push(read.data);
  }
  const stored = (commit: string) => lines.some((line) => line.pushed === commit && line.ok);
  for (const { index, fields } of pushed) {
    const commit = fields[3] ?? "";
    if (stored(commit)) continue;
    const failed = lines.find((line) => line.pushed === commit && !line.ok);
    return failed === undefined
      ? `head: pushes.log:${index + 1} pushed ${commit} to ${fields[1]}, and ${HEADS_LOG} holds no line storing it, so the run's Dispatch went on comparing results with the head before it`
      : `head: pushes.log:${index + 1} pushed ${commit} to ${fields[1]}, and the head watcher could not store it: ${(failed.error ?? "").slice(0, 200)}`;
  }
  const read = existsSync(path.join(run.dir, "world.json")) ? readWorld(run.dir) : undefined;
  if (read === undefined || !("world" in read)) return undefined;
  const { branch, head } = read.world;
  const tip = Bun.spawnSync([
    "git",
    "-C",
    path.join(run.dir, "remote.git"),
    "rev-parse",
    "--verify",
    "--quiet",
    `refs/heads/${branch}^{commit}`,
  ]);
  const at = tip.exitCode === 0 ? tip.stdout.toString().trim() : "";
  if (at === "" || at === head || stored(at)) return undefined;
  return `head: remote.git's refs/heads/${branch} stands at ${at}, not world.json's seeded head ${head}, and ${HEADS_LOG} holds no line storing it, so a push the record missed left the run's Dispatch comparing results with the head before it`;
}

/** Rule 24: the first push of pushes.log to the issue branch (world.json's `branch`) whose head
 * does not hold the base's tip in its history on the run's own remote, or undefined. The base
 * never moves during a run (its ruleset refuses every push to it, rig.sh's fixture_first_commit),
 * so a head that holds its tip merges into it, and the world's test merge of it is the one GitHub
 * makes; a head that does not (another history, force-pushed: the remote takes it, measured on
 * 2026-11-21 against a copy of run 94caf2e09844's remote, `git push --force <remote>
 * orphan:refs/heads/legion/WID-1` answered `+ a8dfa29...3b7b9ed orphan -> legion/WID-1 (forced
 * update)`, exit 0, and `git merge-tree --write-tree` of it and the base `fatal: refusing to merge
 * unrelated histories`) may merge with none, and GitHub keeps the last test merge it made of such
 * a head where the world answers none. Read with `git merge-base --is-ancestor`, which writes
 * nothing into the run's archive; a deletion of the branch is no head. */
export function unmergedHead(run: Pick<Run, "dir">): string | undefined {
  const pushes = path.join(run.dir, "pushes.log");
  const read = existsSync(path.join(run.dir, "world.json")) ? readWorld(run.dir) : undefined;
  if (!existsSync(pushes) || read === undefined || !("world" in read)) return undefined;
  const { base, branch } = read.world;
  for (const [index, line] of readFileSync(pushes, "utf8").split("\n").entries()) {
    const [, ref = "", , pushed = ""] = line.split(" ");
    if (ref !== `refs/heads/${branch}` || /^0+$/.test(pushed)) continue;
    const holds = Bun.spawnSync([
      ...["git", "-C", path.join(run.dir, "remote.git"), "merge-base", "--is-ancestor"],
      `refs/heads/${base}`,
      pushed,
    ]);
    if (holds.exitCode !== 0)
      return `unmerged head: pushes.log:${index + 1} pushed ${pushed} to ${ref}, whose history does not hold ${base}'s tip, so it may not merge, and GitHub keeps the last test merge of such a head where the world answers none`;
  }
  return undefined;
}

/** A string argument of a tool call, or undefined. */
function stringArg(args: ToolArgs, key: string): string | undefined {
  const value = args[key];
  return typeof value === "string" ? value : undefined;
}

/** The path each `[PATH#TAG]` header of an `edit` call's input edits. */
const EDIT_HEADER = /^\[([^\]\n]+?)#[0-9A-Fa-f]{4}\]/gm;

/** Every absolute path a tool call names as a path: a `read`, `grep`, `glob`, `write` or `edit`
 * call's `path`, each file an `edit` call's input edits, and every absolute path in the text a
 * `bash` call runs (here-document bodies left out: they are data) or an `eval` cell holds. A path
 * the text goes on to build with an expansion (`/tmp/out.$$`, `/tmp/${name}`) is passed over: its
 * name is not in the text, and its leading part names no file. */
function namedPaths(tool: string, args: ToolArgs): string[] {
  const named: string[] = [];
  const pathArg = stringArg(args, "path");
  if (["read", "grep", "glob", "write", "edit"].includes(tool) && pathArg !== undefined)
    named.push(pathArg);
  if (tool === "edit")
    for (const hit of (stringArg(args, "input") ?? "").matchAll(EDIT_HEADER))
      named.push(hit[1] ?? "");
  const text =
    tool === "bash"
      ? withoutBodies(stringArg(args, "command") ?? "").code
      : tool === "eval"
        ? (stringArg(args, "code") ?? "")
        : "";
  for (const hit of text.matchAll(ABSOLUTE_PATH))
    if (!/^[$`{*?]/.test(text.slice((hit.index ?? 0) + hit[0].length))) named.push(hit[0]);
  return named.filter((one) => one.startsWith("/")).map((one) => path.posix.normalize(one));
}

/** A run directory's name as rig.sh's cmd_run gave it before the names left the world,
 * `<scenario>[-<case>]-<label>-<n>`, for each scenario of run-names.ts's SCENARIOS. */
const RUN_NAME = new RegExp(`^(?:${Object.keys(SCENARIOS).join("|")})-[a-z0-9][a-z0-9-]*-\\d+$`);

/** The run directory `target` lies in or names, when that is a run other than this one: the
 * first component of the path that names a run, wherever it sits — beside this run in the runs
 * directory, in the archive rig.sh moves a finished run to, or under another batch's work
 * directory: a run's id (RUN_ID) under a `runs` directory, as cmd_run makes a run, or a name of
 * a run recorded before the names left the world (RUN_NAME). */
function otherRun(target: string, run: Run): string | undefined {
  if (run.own.some((dir) => inside(target, dir))) return undefined;
  const parts = target.split("/");
  const at = parts.findIndex(
    (part, index) => RUN_NAME.test(part) || (RUN_ID.test(part) && parts[index - 1] === "runs")
  );
  return at < 0 ? undefined : parts.slice(0, at + 1).join("/");
}

/** Rule 8: the first tool call of the run that names a path under another run's directory, the
 * name read as the run's namespace mounts it (`reachedIn`): a directory named as a run under a
 * namespace's /tmp is the run's own scratch, <run>/tmp, where no other run is. Where the
 * namespace leaves the name no path of the machine's (a covered path, among them the work
 * directory every run is in), the name is judged as the agent wrote it: it is the reach the
 * agent tried. What it cannot see is a reach that names no such path: a `find` or a glob from a
 * directory above the runs, a path built in a variable, or a command that reads the runs
 * directory itself and acts on what it listed. */
function otherRunReach(run: Run): string | undefined {
  for (const call of run.calls)
    for (const target of namedPaths(call.tool, call.args)) {
      const other = otherRun(reachedIn(run, target) ?? target, run);
      if (other !== undefined)
        return `another run: a ${call.tool} call in ${call.file} names ${target}, under ${other}`;
    }
  return undefined;
}

/** A run's namespace as rig.sh declares it (world_paths, the one declaration its namespace
 * mounts): what the namespace hides behind an empty tmpfs (`cover`), the credentials it hides the
 * same way (`credentials`), what it shows again (`rw`, `ro`), the run's own directory first
 * (`own`: world_paths prints it before every other path it shows, and private_world binds it
 * first), and what of those it hides again behind an empty read-only tmpfs (`hide`). A `ro` line
 * can name a file, the system git configuration, which the world shows as the host holds it and
 * mounts nothing onto: a name of it is that path, shown. A run recorded before 2026-11-19 can
 * hold an `empty` line for that file instead (an empty file bound over it), which no rule reads,
 * then as now: its path lies under no cover and is shown by no other line, so a name of it is
 * that path, as the run was judged when it was recorded. It is not gh-call.ts's `World`, the
 * run's world.json. */
interface RunNamespace {
  readonly own: string;
  readonly covers: readonly string[];
  readonly credentials: readonly string[];
  readonly shown: readonly string[];
  readonly hidden: readonly string[];
}

const namespaces = new Map<string, RunNamespace>();

/** The run's namespace, read once per run, from what it recorded alone: <run>/namespace.txt
 * (NAMESPACE), the lines world_paths printed for it when launch started its agents there, which
 * private_world mounted. Working its world out again at judging time reads another one: `rig.sh
 * world` takes its scratch-root cover from the judge's TMPDIR, its workspaces from the judge's
 * LEGION_WORKSPACE and its label's install from the work directory as it stands, so INFRA-764
 * round 25's reviewer-text-findings-defect-head-1 (b37dc3dc7f25), judged from a copy whose work
 * directory was gone, read "outside its world" for a skill file of its label's install, which its
 * namespace.txt shows (`ro …/installs/4c0768d2ae4b`). A run from before the rig made a namespace
 * recorded no world, and nothing it recorded rebuilds one, so this throws a Refused naming the run
 * and namespace.txt: the rules that read the world (10 and 12) do not judge it, and the run is
 * judged by the others or refused (the architect's ruling of 2026-11-18). */
function namespaceOf(run: Pick<Run, "dir" | "namespaced">): RunNamespace {
  const known = namespaces.get(run.dir);
  if (known !== undefined) return known;
  if (!run.namespaced)
    throw new Refused(
      `${path.basename(run.dir)}: it records no ${NAMESPACE}, the world its agents ran in, which rules 10 and 12 read; nothing else it recorded gives that world, and no reading of the pod stands in for it`
    );
  const declared = readFileSync(path.join(run.dir, NAMESPACE), "utf8");
  const world = {
    own: "",
    covers: [] as string[],
    credentials: [] as string[],
    shown: [] as string[],
    hidden: [] as string[],
  };
  for (const line of declared.split("\n")) {
    const at = line.indexOf(" ");
    const where = path.posix.normalize(line.slice(at + 1));
    const kind = line.slice(0, at);
    if (kind === "cover") world.covers.push(where);
    else if (kind === "credentials") world.credentials.push(where);
    else if (kind === "hide") world.hidden.push(where);
    else if (kind === "rw" || kind === "ro") {
      if (world.shown.length === 0) world.own = where;
      world.shown.push(where);
    }
  }
  namespaces.set(run.dir, world);
  return world;
}

/** The machine's path that `named`, a path a tool call names, reaches in a run's namespace, by
 * the mounts rig.sh's private_world makes from the run's world, in its order, where the last
 * mount that holds the name decides what it is:
 *   1. /dev/shm: a tmpfs, the holder of the paths the world shows;
 *   2. each cover and credentials path: an empty tmpfs;
 *   3. each path the world shows (`rw`, `ro`): bound back at its own path, into that tmpfs too
 *      where it lies under /dev/shm;
 *   4. /tmp: the run's own <run>/tmp, bound over the machine's;
 *   5. each shown path at or under /tmp: bound back at its own path again;
 *   6. each `hide` path: an empty read-only tmpfs.
 * So a name under a shown path, and one outside every mount, is that path; a name under /tmp is
 * <run>/tmp's; and a name under a tmpfs (1, 2, 6) reaches no path of the machine's, which is
 * undefined: each launch mounts tmpfs of its own, which the agent and the processes it starts
 * alone hold and which go when it exits, so no other run, and no later agent of the run, reads
 * what it wrote there. Read by /tmp alone, two runs that each wrote and read /dev/shm/x read as
 * one run reading the other's write (INFRA-764's class search of namespace reads, 2026-11-18). A
 * shown path lies under /dev/shm where the work directory does, as rig.test.ts's does. */
export function mounted(
  world: Pick<RunNamespace, "own" | "covers" | "credentials" | "shown" | "hidden">,
  named: string
): string | undefined {
  const under = (dirs: readonly string[]) => dirs.some((dir) => inside(named, dir));
  if (under(world.hidden)) return undefined;
  if (inside(named, "/tmp"))
    return under(world.shown.filter((shown) => inside(shown, "/tmp")))
      ? named
      : path.posix.join(world.own, named);
  if (under(world.shown)) return named;
  return inside(named, "/dev/shm") || under([...world.covers, ...world.credentials])
    ? undefined
    : named;
}

/** The machine's path a tool call of the run reaches by the name `named`: the name as the run's
 * namespace mounts it (`mounted`), undefined where that is a tmpfs of the agent's launch alone,
 * and the name itself for a run that ran in none. */
function reachedIn(
  run: Pick<Run, "dir" | "label" | "namespaced">,
  named: string
): string | undefined {
  return run.namespaced ? mounted(namespaceOf(run), named) : named;
}

/** Rule 10: the first path a tool call of the run names under what its world hides
 * — the scratch root, the issue workspaces, the work directory and every label's checkout (those
 * of the runs given too, by their built-from) — outside the run's own directory, its /tmp and
 * every path its world shows again. The world is the one its namespace mounted (`namespaceOf`),
 * so the allowlist is that declaration, and a path under the other label's checkout is outside
 * it. Its /tmp is the namespace's <run>/tmp, so a run names nothing hidden there. An ancestor of
 * the run's own directory or of a path its world shows is in its world: the namespace covers the
 * hidden path with a tmpfs and mounts those under it, so the ancestor holds them alone (the
 * architect's ruling of 2026-11-22T01:44:54Z: round 32's 189e7ebeb7da ran `find <work>/runs` and
 * found its own run's files alone). The run's own directory is the one it ran in, as its record
 * names it (`recorded`) and its namespace showed it, never the directory health was given: a copy
 * or an archive is a path no world of the run showed, so a name under it or above it is read as
 * any other (the tester's finding of 2026-11-22T04:07:41Z, the class of round 25's "a reader
 * reads the run's archive and its recorded provenance alone"). A run from before the namespace
 * recorded no world, and this refuses it (`namespaceOf`). What it cannot see is what rule 8
 * cannot: a reach that names no path. */
function outsideWorld(run: Run, runs: readonly Run[]): string | undefined {
  const world = namespaceOf(run);
  const hidden = [
    ...world.covers,
    ...runs.flatMap((one) => {
      try {
        return [path.posix.normalize(labelCheckout(one.dir))];
      } catch {
        return [];
      }
    }),
  ];
  const shown = [...run.recorded, ...world.shown];
  const allowed = [...shown, "/tmp"];
  for (const call of run.calls)
    for (const target of namedPaths(call.tool, call.args)) {
      if (allowed.some((dir) => inside(target, dir))) continue;
      if (shown.some((dir) => inside(dir, target))) continue;
      const zone = hidden.find((dir) => inside(target, dir));
      if (zone !== undefined)
        return `outside its world: a ${call.tool} call in ${call.file} names ${target}, under ${zone}, which the run's world hides and does not show again (rig.sh's world_paths)`;
    }
  return undefined;
}

/** The text a call runs or reads by: a `bash` command whole, an `eval` cell, and a `read`, `grep`
 * or `glob` path. */
function callText(tool: string, args: ToolArgs): string {
  if (tool === "bash") return stringArg(args, "command") ?? "";
  if (tool === "eval") return stringArg(args, "code") ?? "";
  return ["read", "grep", "glob"].includes(tool) ? (stringArg(args, "path") ?? "") : "";
}

/** A command substitution in place of `<n>`: `$(…)`, up to two levels of parentheses inside it
 * (an arithmetic `$((…))` among them), or `` `…` ``. The shell puts there what the command printed,
 * which names whatever process that command found (`$(pgrep bun)`). */
const SUBSTITUTION = /\$\((?:[^()]|\((?:[^()]|\([^()]*\))*\))*\)|`[^`]*`/.source;

/** Any other character of the word in place of `<n>`: none that ends a path's segment or a shell
 * word. */
const PROCESS_CHARACTER = /[^/\s'"`;|&<>()]/.source;

/** A read of a process's environment or command line: `/proc/<n>/environ` or
 * `/proc/<n>/cmdline`, the word in place of `<n>` captured. */
const PROCESS_READ = new RegExp(
  String.raw`/proc/((?:${SUBSTITUTION}|${PROCESS_CHARACTER})+)/(?:environ|cmdline)(?![\w.-])`,
  "g"
);

/** The words in place of `<n>` that name the caller's own process, read as the kernel and bash
 * read them:
 *   - `self` and `thread-self`: the kernel's links to the process, and the thread, that opens the
 *     path (proc(5));
 *   - `$$` and `${$}`, the special parameter `$`: it "expands to the process ID of the shell. In a
 *     subshell, it expands to the process ID of the invoking shell, not the subshell", and
 *     "assignment to them is not allowed" (Bash Reference Manual, Special Parameters): the shell
 *     that runs the command, wherever in the command the read stands;
 *   - `$BASHPID` and `${BASHPID}`: it "expands to the process ID of the current Bash process",
 *     which differs from `$$` in a subshell that is a fork of that same shell, and "assignments to
 *     BASHPID have no effect" (Bash Reference Manual, Bash Variables). Only an `unset BASHPID`
 *     before a new value takes that from it, which this does not read.
 * The worker prompt both labels load reads Dispatch's address out of `/proc/$$/environ`
 * (packages/daemon/internal/prompts/go/worker-common.md), a redirect from /proc/self/environ
 * reading empty under the pod's gVisor (src/stall-evidence.ts). In a run that shares the pod's
 * processes every other word names another process, or may: `$PPID`, which is the shell's parent;
 * any other variable; a number; a glob; a command substitution (SUBSTITUTION). */
const OWN_PROCESS = /^(?:self|thread-self|\$\$|\$\{\$\}|\$BASHPID|\$\{BASHPID\})$/;

/** Rule 11: the first tool call of the run that reads another process's environment or command
 * line (PROCESS_READ, but OWN_PROCESS), here-document bodies included: the 2026-11-14 probe, which
 * ran before the rig gave each run a namespace, read the caller's Dispatch variables out of the
 * pod's /proc/1/environ. A run that holds <run>/namespace.txt (`namespaced`) reads none: rig.sh's
 * launch starts its agents with `unshare -Urm -p --fork --mount-proc`, in a PID namespace of the
 * run's own with a /proc of its own, so every /proc/<n> they can read is a process of the run,
 * whatever stands in place of `<n>`: PID 1, `$PPID`, a number, a glob, a substitution. Measured in
 * INFRA-764 round 25: a namespaced reviewer that looked its own session id up in /proc/1/environ
 * was called unhealthy and replaced. A run without it shared the pod's processes, and there a glob
 * or a variable in place of `<n>` is another process's. */
function otherProcess(run: Run): string | undefined {
  if (run.namespaced) return undefined;
  for (const call of run.calls)
    for (const found of callText(call.tool, call.args).matchAll(PROCESS_READ))
      if (!OWN_PROCESS.test(found[1]))
        return `another process: a ${call.tool} call in ${call.file} reads ${found[0]}, a process the run does not own`;
  return undefined;
}

/** Rule 12: the first path a tool call of the run names under a credentials path of
 * its world (/var/run/legion, /var/run/secrets, by either name), whatever the run then did with
 * it. */
function credentialReach(run: Run): string | undefined {
  const { credentials } = namespaceOf(run);
  for (const call of run.calls)
    for (const target of namedPaths(call.tool, call.args)) {
      const under = credentials.find((dir) => inside(target, dir));
      if (under !== undefined)
        return `credentials: a ${call.tool} call in ${call.file} names ${target}, under ${under}`;
    }
  return undefined;
}

/** A `gh` command that prints the GitHub token its gh holds: `gh auth status` given `-t` or
 * `--show-token`, alone or among joined short flags (`-at`, `-ta`), or `gh auth token`. A flag
 * after `;`, `&`, `|` or a line end belongs to another command. Read off a bash command's raw
 * text, as the tester's world-health.py reads it (its rule 13). */
export const PRINTS_TOKEN =
  /\bgh\s+auth\s+status\b[^\n;|&]*?\s(?:--show-token\b|-[A-Za-z]*t[A-Za-z]*(?=[\s'"]|$))|\bgh\s+auth\s+token\b/;

/** Rule 21: the first bash call of the run that prints its GitHub token (PRINTS_TOKEN). Production
 * answers both calls with the token itself, which no stand-in reads (daemon-pane.ts's
 * `RIG_GH_TOKEN`), so the world refuses them (gh-standin.ts's `authStatus`, and no route of `gh
 * auth token`), and rule 7 would name that refusal (neither writes, as gh's source reads them,
 * gh-writes.json): this names what the agent did,
 * print its credential into its transcript (the architect's ruling of 2026-11-18T11:34Z and
 * 11:38Z). */
export function credentialPrinted(run: Pick<Run, "calls">): string | undefined {
  for (const call of run.calls) {
    if (call.tool !== "bash") continue;
    const found = PRINTS_TOKEN.exec(stringArg(call.args, "command") ?? "");
    if (found !== null)
      return `credential printed: a bash call in ${call.file} runs \`${found[0]}\`, which prints the run's GitHub token`;
  }
  return undefined;
}

/** A URL that reaches a host, whole, with its host: the schemes of the network, not Oh My Pi's
 * own references (skill://, dispatch://, local://), which reach none. */
const NETWORK_URL =
  /\b(?:https?|wss?|ssh|git|git\+ssh|ftp|sftp|rsync):\/\/(?:[^\s/@'"`]*@)?(\[[0-9A-Fa-f:.]+\]|[^\s/:?#'"`()<>;|&]+)[^\s'"`()<>;|&]*/gi;
/** Any URL, with its host: what the pane names (nats://, postgres://) counts as the world's. */
const ANY_URL =
  /\b[a-z][a-z0-9+.-]*:\/\/(?:[^\s/@'"`]*@)?(\[[0-9A-Fa-f:.]+\]|[^\s/:?#'"`()<>;|&]+)/gi;
/** An address in scp form, `[<user>@]<host>:<path>`, with its host: no slash before its first
 * colon, which is how git (`url_is_local_not_ssh`), scp(1) and rsync(1) tell a remote from a
 * local path, so `docs/cli.md:12` and `./a:b` are paths. */
const SCP_ADDRESS = /^(?:[^\s@/:]+@)?(\[[0-9A-Fa-f:.]+\]|[^\s@/:[\]]+):/;

/** The host an address in scp form names (SCP_ADDRESS), or undefined for a URL (`://`, which
 * `urlHosts` reads) and for a path; rsync's `<host>::<module>` is a daemon on that host. Read
 * only where a command takes an address, never as a free word: in a word anywhere else,
 * `HEAD:docs/cli.md`, `main:path` or `main@origin:docs/cli.md` is a revision and a path. */
function scpHost(address: string): string | undefined {
  return address.includes("://") ? undefined : SCP_ADDRESS.exec(address)?.[1];
}

/** A host a curl or wget argument names without a scheme: a name with a dot, an address or
 * localhost, followed by a port or a path. */
const BARE_HOST =
  /^(localhost|\d{1,3}(?:\.\d{1,3}){3}|(?:[a-z0-9-]+\.)+[a-z]{2,})(?::\d+)?(?:[/:]|$)/i;

/** Option names, given apart by blanks, as a set. */
function optionSet(names: string): ReadonlySet<string> {
  return new Set(names.trim().split(/\s+/));
}
const NO_OPTIONS: ReadonlySet<string> = new Set();

/** A command's arguments: its operands in order, and each option it is given with its value. */
interface Arguments {
  readonly operands: string[];
  readonly options: [name: string, value?: string][];
}

/** Reads the option `words[at]` into `options`, `valued` naming the options whose value is the
 * next word, and answers where the next word to read stands. `--name=value` is one option with
 * its value. A word of short options (`-sSo`) is each of its letters, and the first of them that
 * takes a value takes the rest of the word, or the next word when it ends the word, as getopt
 * (curl, wget, the OpenSSH tools), git's parse-options and jj's clap read them. */
function readOption(
  words: readonly string[],
  at: number,
  valued: ReadonlySet<string>,
  options: [name: string, value?: string][]
): number {
  const word = words[at] ?? "";
  const long = /^(--[^=]+)=([\s\S]*)$/.exec(word);
  if (long !== null) {
    options.push([long[1] ?? "", long[2]]);
    return at + 1;
  }
  if (word.startsWith("--") || word === "-") {
    options.push(valued.has(word) ? [word, words[at + 1]] : [word]);
    return valued.has(word) ? at + 2 : at + 1;
  }
  for (let letter = 1; letter < word.length; letter++) {
    const name = `-${word[letter]}`;
    if (!valued.has(name)) options.push([name]);
    else if (letter + 1 < word.length) {
      options.push([name, word.slice(letter + 1)]);
      return at + 1;
    } else {
      options.push([name, words[at + 1]]);
      return at + 2;
    }
  }
  return at + 1;
}

/** A command's words read as its Arguments by the options it takes (`readOption`), wherever they
 * stand among its operands, as a getopt that permutes them reads them (GNU's, git's, jj's). */
function argumentsOf(words: readonly string[], valued: ReadonlySet<string>): Arguments {
  const operands: string[] = [];
  const options: [name: string, value?: string][] = [];
  for (let at = 0; at < words.length; ) {
    const word = words[at] ?? "";
    if (word.startsWith("-")) at = readOption(words, at, valued, options);
    else {
      operands.push(word);
      at++;
    }
  }
  return { operands, options };
}

/** Whether `args` gives any of the options `names`. */
function given(args: Arguments, ...names: string[]): boolean {
  return args.options.some(([name]) => names.includes(name));
}

/** The value `args` gives each of the options `names`, in order. */
function valuesOf(args: Arguments, ...names: string[]): (string | undefined)[] {
  return args.options.filter(([name]) => names.includes(name)).map(([, value]) => value);
}

/** Where the first operand of `words` stands past the options before it, `valued` naming those
 * whose value is the next word; the words' length when they have none. */
function firstOperand(words: readonly string[], valued: ReadonlySet<string>): number {
  for (let at = 0; at < words.length; at++) {
    const word = words[at] ?? "";
    if (valued.has(word)) at++;
    else if (!word.startsWith("-")) return at;
  }
  return words.length;
}

/** A command's words read as its Arguments the way a getopt that stops at the first operand reads
 * them (POSIX order: a `+` option string, or OpenSSH's own getopt): options (`readOption`) up to
 * the first word that is none, or up to `--`, which ends them, and every word from there an
 * operand; `-` alone is an operand. sudo, env, timeout, nice, nohup, xargs, time, ssh, scp and
 * sftp read theirs so, and a wrapper's operands are the command it runs. */
function posixArguments(words: readonly string[], valued: ReadonlySet<string>): Arguments {
  const options: [name: string, value?: string][] = [];
  for (let at = 0; at < words.length; ) {
    const word = words[at] ?? "";
    if (word === "--") return { operands: words.slice(at + 1), options };
    if (!word.startsWith("-") || word === "-") return { operands: words.slice(at), options };
    at = readOption(words, at, valued, options);
  }
  return { operands: [], options };
}

/** curl's options whose value is the next word (`curl --help all`, curl 8.14.1). */
const CURL_VALUED = optionSet(`
  --abstract-unix-socket --alt-svc --aws-sigv4 --cacert --capath -E --cert --cert-type --ciphers
  -K --config --connect-timeout --connect-to -C --continue-at -b --cookie -c --cookie-jar
  --create-file-mode --crlfile --curves -d --data --data-ascii --data-binary --data-raw
  --data-urlencode --delegation --dns-interface --dns-ipv4-addr --dns-ipv6-addr --dns-servers
  --doh-url -D --dump-header --ech --egd-file --engine --etag-compare --etag-save
  --expect100-timeout -F --form --form-string --ftp-account --ftp-alternative-to-user
  --ftp-method -P --ftp-port --ftp-ssl-ccc-mode --happy-eyeballs-timeout-ms --haproxy-clientip
  -H --header -h --help --hostpubmd5 --hostpubsha256 --hsts --interface --ip-tos --ipfs-gateway
  --json --keepalive-cnt --keepalive-time --key --key-type --krb --libcurl --limit-rate
  --local-port --login-options --mail-auth --mail-from --mail-rcpt --max-filesize --max-redirs
  -m --max-time --netrc-file --noproxy --oauth2-bearer -o --output --output-dir --parallel-max
  --pass --pinnedpubkey --preproxy --proto --proto-default --proto-redir -x --proxy
  --proxy-cacert --proxy-capath --proxy-cert --proxy-cert-type --proxy-ciphers --proxy-crlfile
  --proxy-header --proxy-key --proxy-key-type --proxy-pass --proxy-pinnedpubkey
  --proxy-service-name --proxy-tls13-ciphers --proxy-tlsauthtype --proxy-tlspassword
  --proxy-tlsuser -U --proxy-user --proxy1.0 --pubkey -Q --quote --random-file -r --range --rate
  -e --referer -X --request --request-target --resolve --retry --retry-delay --retry-max-time
  --sasl-authzid --service-name --sigalgs --socks4 --socks4a --socks5 --socks5-gssapi-service
  --socks5-hostname -Y --speed-limit -y --speed-time --ssl-sessions --stderr -t --telnet-option
  --tftp-blksize -z --time-cond --tls-max --tls13-ciphers --tlsauthtype --tlspassword --tlsuser
  --trace --trace-ascii --trace-config --unix-socket -T --upload-file --upload-flags --url
  --url-query -u --user -A --user-agent --variable --vlan-priority -w --write-out
`);
/** The options of CURL_VALUED whose value is a host or a URL curl connects to: the URL it fetches
 * (`--url`), a proxy's (`[protocol://]host[:port]`), its DNS-over-HTTPS server's and its IPFS
 * gateway's. */
const CURL_HOSTS = [
  "--url",
  "-x",
  "--proxy",
  "--preproxy",
  "--proxy1.0",
  "--socks4",
  "--socks4a",
  "--socks5",
  "--socks5-hostname",
  "--doh-url",
  "--ipfs-gateway",
];
/** wget's options whose value is the next word (`wget --help`, GNU Wget 1.25.0). */
const WGET_VALUED = optionSet(`
  -e --execute -o --output-file -a --append-output --report-speed -i --input-file -B --base
  --config --rejected-log -t --tries --retry-on-http-error -O --output-document --start-pos
  --progress -T --timeout --dns-timeout --connect-timeout --read-timeout -w --wait --waitretry
  -Q --quota --bind-address --limit-rate --restrict-file-names --prefer-family --user --password
  --use-askpass --local-encoding --remote-encoding -P --directory-prefix --cut-dirs --http-user
  --http-password --default-page --header --compression --proxy-user --proxy-password --referer
  -U --user-agent --load-cookies --save-cookies --post-data --post-file --method --body-data
  --body-file --secure-protocol --certificate --certificate-type --private-key
  --private-key-type --ca-certificate --ca-directory --crl-file --pinnedpubkey --ciphers
  --ftp-user --ftp-password --warc-file --warc-header --warc-max-size --warc-dedup
  --warc-tempdir -l --level --backups -A --accept -R --reject --accept-regex --reject-regex
  --regex-type -D --domains --exclude-domains --follow-tags --ignore-tags -I
  --include-directories -X --exclude-directories --hsts-file
`);
/** The options of WGET_VALUED whose value is a URL wget fetches from: `--base`, which it resolves
 * the links of its `--input-file` against. */
const WGET_HOSTS = ["-B", "--base"];
/** The options of CURL_VALUED and WGET_VALUED whose value is a file the command reads the URLs it
 * fetches from, `-` its input: curl's config file (`-K`), whose `url` lines it fetches, and wget's
 * input file (`-i`). The reader does not read the file, so the hosts such a command reaches are not
 * in the text (`commandHosts`). */
const URL_FILES: Readonly<Record<string, readonly string[]>> = {
  curl: ["-K", "--config"],
  wget: ["-i", "--input-file"],
};

/** A git or jj subcommand that takes a repository's address: its options whose value is the next
 * word, and the addresses its arguments give. */
type AddressTaker = readonly [
  valued: ReadonlySet<string>,
  addresses: (args: Arguments) => readonly (string | undefined)[],
];

/** git's own options, before its subcommand, whose value is the next word: git(1)'s `-C <path>`
 * and `-c <name>=<value>`, and those git 2.47's git.c (`handle_options`) also takes apart from
 * their value (`--git-dir`, `--work-tree`, `--namespace`, `--config-env`, `--attr-source`,
 * `--shallow-file`). */
const GIT_GLOBAL = optionSet(
  "-C -c --git-dir --work-tree --namespace --config-env --attr-source --shallow-file"
);

/** The address a git configuration variable gives (git-config(1), Variables): the value of
 * `remote.<name>.url` and `remote.<name>.pushurl`, and the `<base>` of `url.<base>.insteadOf` and
 * `url.<base>.pushInsteadOf`, which git rewrites a URL that starts with the value to. */
function configAddress(key: string, value: string | undefined): string | undefined {
  if (/^remote\..+\.(?:push)?url$/i.test(key)) return value;
  return /^url\.(.+)\.(?:push)?insteadof$/i.exec(key)?.[1];
}

/** git config's modes whose operand after a variable's name is a pattern or a URL it matches,
 * not a value it sets (git-config(1), DEPRECATED MODES). */
const CONFIG_MATCHES = [
  "--get",
  "--get-all",
  "--get-regexp",
  "--get-urlmatch",
  "--unset",
  "--unset-all",
];

/** The git subcommands that take a repository's address, by their path: each one's options whose
 * value is the next word, as git 2.47.3's `git <subcommand> -h` lists them, and the arguments its
 * documentation's synopsis names as an address. */
const GIT_TAKERS = new Map<string, AddressTaker>([
  // git-clone(1): `git clone [<options>] [--] <repository> [<directory>]`.
  [
    "clone",
    [
      optionSet(`
        -j --jobs --template --reference --reference-if-able -o --origin -b --branch -u
        --upload-pack --depth --shallow-since --shallow-exclude --separate-git-dir --ref-format -c
        --config --server-option --filter --bundle-uri
      `),
      ({ operands }) => [operands[0]],
    ],
  ],
  // git-fetch(1): `git fetch [<options>] [<repository> [<refspec>...]]`, and
  // `git fetch --multiple [<options>] [(<repository> | <group>)...]`.
  [
    "fetch",
    [
      optionSet(`
        --upload-pack -j --jobs --depth --shallow-since --shallow-exclude --deepen --refmap -o
        --server-option --negotiation-tip --filter
      `),
      (args) => (given(args, "--multiple", "-m") ? args.operands : [args.operands[0]]),
    ],
  ],
  // git-pull(1): `git pull [<options>] [<repository> [<refspec>...]]`.
  [
    "pull",
    [
      optionSet(`
        --cleanup -s --strategy -X --strategy-option --upload-pack --depth --shallow-since
        --shallow-exclude --deepen --refmap -o --server-option --negotiation-tip
      `),
      ({ operands }) => [operands[0]],
    ],
  ],
  // git-push(1): `git push [<options>] [<repository> [<refspec>...]]`, and `--repo=<repository>`,
  // "equivalent to the <repository> argument. If both are specified, the command-line argument
  // takes precedence": `git push --repo=<url> HEAD:main` pushes to the repository `HEAD:main`.
  [
    "push",
    [
      optionSet("--repo --recurse-submodules --receive-pack --exec -o --push-option"),
      (args) => [args.operands[0] ?? valuesOf(args, "--repo").at(-1)],
    ],
  ],
  // git-ls-remote(1): `git ls-remote [<options>] [<repository> [<patterns>...]]`.
  [
    "ls-remote",
    [optionSet("--upload-pack --sort -o --server-option"), ({ operands }) => [operands[0]]],
  ],
  // git-remote(1): `git remote add [<options>] <name> <URL>`.
  ["remote add", [optionSet("-t --track -m --master"), ({ operands }) => [operands[1]]]],
  // git-remote(1): `git remote set-url [--push] <name> <newurl> [<oldurl>]` and
  // `git remote set-url --add [--push] <name> <newurl>`; with `--delete` its <URL> is a regex of
  // the URLs it removes.
  ["remote set-url", [NO_OPTIONS, (args) => (given(args, "--delete") ? [] : [args.operands[1]])]],
  // git-submodule(1): `git submodule add [<options>] [--] <repository> [<path>]`, its options as
  // git-submodule.sh's cmd_add reads them.
  [
    "submodule add",
    [
      optionSet("-b --branch --reference --ref-format --name --depth"),
      ({ operands }) => [operands[0]],
    ],
  ],
  // git-submodule(1): `git submodule set-url [--] <path> <newurl>`.
  ["submodule set-url", [NO_OPTIONS, ({ operands }) => [operands[1]]]],
  // git-archive(1): `--remote=<repo>`, "retrieve a tar archive from a remote repository".
  [
    "archive",
    [
      optionSet(
        "--format --prefix --add-file --add-virtual-file -o --output --mtime --remote --exec"
      ),
      (args) => valuesOf(args, "--remote"),
    ],
  ],
  // git-config(1): `git config set <name> <value>`, and `git config [--add | --replace-all] <name>
  // <value>` of its deprecated modes. Its other subcommands (`get`, `unset`, …) name no variable
  // first, so configAddress reads none of them.
  [
    "config",
    [
      optionSet("-f --file --blob -t --type --value --url --default --comment"),
      (args) => {
        const [mode, ...rest] = args.operands;
        const set = mode === "set" ? rest : given(args, ...CONFIG_MATCHES) ? [] : args.operands;
        return [configAddress(set[0] ?? "", set[1])];
      },
    ],
  ],
]);

/** The subcommands of GIT_TAKERS whose address is a remote git fetches from or pushes to, or
 * names one by: clone, fetch, pull, push, ls-remote and `remote add`, as the tester's reader
 * reads a fetcher's URL (`unexpandedUrls`). */
const GIT_REMOTES = new Map(
  [...GIT_TAKERS].filter(([subcommand]) =>
    ["clone", "fetch", "pull", "push", "ls-remote", "remote add"].includes(subcommand)
  )
);

/** jj's global options whose value is the next word, which stand before or after its subcommand
 * (jj's CLI reference, https://docs.jj-vcs.dev/latest/cli-reference/, `jj`'s Options). */
const JJ_GLOBAL = optionSet(
  "-R --repository --at-operation --at-op --color --config --config-file"
);

/** The jj subcommands that take a repository's address, by their path, as jj's CLI reference
 * gives them: each one's options whose value is the next word, and its arguments that are an
 * address. */
const JJ_TAKERS = new Map<string, AddressTaker>([
  // `jj git clone [OPTIONS] <SOURCE> [DESTINATION]`, <SOURCE> the "URL or path of the Git repo".
  [
    "git clone",
    [
      optionSet("--remote --depth -b --branch -t --tag --object-hash"),
      ({ operands }) => [operands[0]],
    ],
  ],
  // `jj git remote add [OPTIONS] <REMOTE> <URL>`, and `--push-url <PUSH_URL>`.
  [
    "git remote add",
    [optionSet("--push-url"), (args) => [args.operands[1], ...valuesOf(args, "--push-url")]],
  ],
  // `jj git remote set-url [OPTIONS] <REMOTE> [URL]`, and `--push <PUSH>` and `--fetch <FETCH>`.
  [
    "git remote set-url",
    [
      optionSet("--push --fetch"),
      (args) => [args.operands[1], ...valuesOf(args, "--push", "--fetch")],
    ],
  ],
]);

/** The addresses `words` give as the subcommand of `takers` that their first operands name
 * (`remote add`, after `path`), the options before each of those operands read by `valued`, which
 * that subcommand takes too. None when they name no subcommand of `takers`. */
function takerAddresses(
  words: readonly string[],
  valued: ReadonlySet<string>,
  takers: ReadonlyMap<string, AddressTaker>,
  path = ""
): readonly (string | undefined)[] {
  const at = firstOperand(words, valued);
  if (at === words.length) return [];
  const key = `${path}${words[at]}`;
  const rest = words.slice(at + 1);
  const taker = takers.get(key);
  if (taker !== undefined) return taker[1](argumentsOf(rest, new Set([...valued, ...taker[0]])));
  return [...takers.keys()].some((one) => one.startsWith(`${key} `))
    ? takerAddresses(rest, valued, takers, `${key} `)
    : [];
}

/** The repository addresses a `git` or `jj` command gives where it takes one: the address each
 * `git -c <name>=<value>` gives (configAddress), and the arguments of the subcommand of GIT_TAKERS
 * or JJ_TAKERS it runs that are an address. A word anywhere else is none: round 23's
 * `jj -R . file show main@origin:docs/cli.md` names a revision and a path, and reaches no host. */
function repositoryAddresses(name: string, words: readonly string[]): string[] {
  let found: readonly (string | undefined)[] = [];
  if (name === "jj") found = takerAddresses(words, JJ_GLOBAL, JJ_TAKERS);
  if (name === "git") {
    const sub = firstOperand(words, GIT_GLOBAL);
    const settings = valuesOf(argumentsOf(words.slice(0, sub), GIT_GLOBAL), "-c");
    found = [
      ...settings.map((setting = "") => {
        const [key = "", ...value] = setting.split("=");
        return configAddress(key, value.join("="));
      }),
      ...takerAddresses(words.slice(sub), NO_OPTIONS, GIT_TAKERS),
    ];
  }
  return found.filter((one): one is string => one !== undefined);
}

/** A host as the world compares it: lower case, an IPv6 address out of its brackets. */
function hostName(host: string): string {
  return host.toLowerCase().replace(/^\[(.*)\]$/, "$1");
}

/** ssh's options whose value is the next word (ssh(1), OpenSSH 9). */
const SSH_VALUED = optionSet("-B -b -c -D -E -e -F -I -i -J -L -l -m -O -o -P -p -Q -R -S -W -w");
/** scp's options whose value is the next word (scp(1), OpenSSH 9). */
const SCP_VALUED = optionSet("-c -D -F -i -J -l -o -P -S -X");
/** sftp's options whose value is the next word (sftp(1), OpenSSH 9). */
const SFTP_VALUED = optionSet("-B -b -c -D -F -i -J -l -o -P -R -S -s -X");
/** rsync's options whose value is the next word (rsync(1), rsync 3.2). */
const RSYNC_VALUED = optionSet(`
  -e --rsh -B --block-size -f --filter -T --temp-dir -M --remote-option -@ --modify-window
  --rsync-path --backup-dir --suffix --chmod --max-delete --max-size --min-size --max-alloc
  --partial-dir --usermap --groupmap --chown --timeout --contimeout --compare-dest --copy-dest
  --link-dest --compress-choice --zc --compress-level --zl --skip-compress --exclude
  --exclude-from --include --include-from --files-from --out-format --log-file --log-file-format
  --password-file --bwlimit --stop-after --stop-at --write-batch --only-write-batch --read-batch
  --protocol --iconv --checksum-seed --checksum-choice --cc --address --port --sockopts --outbuf
  --info --debug --stderr --early-input --copy-as
`);
/** nc's options whose value is the next word: OpenBSD's netcat (nc(1)), which Debian and Ubuntu
 * install as `nc`. */
const NC_VALUED = optionSet("-e -H -I -i -K -M -m -O -o -P -p -q -R -s -T -V -W -w -X -x -Z");
/** ncat's options whose value is the next word (ncat(1), Nmap 7). */
const NCAT_VALUED = optionSet(`
  -c --sh-exec -e --exec --lua-exec -g -G -m --max-conns -d --delay -o --output -x --hex-dump -i
  --idle-timeout -p --source-port -s --source -w --wait --proxy --proxy-type --proxy-auth
  --proxy-dns --ssl-cert --ssl-key --ssl-trustfile --ssl-ciphers --ssl-servername --ssl-alpn
  --allow --allowfile --deny --denyfile
`);
/** telnet's options whose value is the next word (telnet(1), netkit's and inetutils'). */
const TELNET_VALUED = optionSet("-b -e -k -l -n -S -X");

/** The host of each `[<user>@]<host>[:<port>]` of a comma-separated list: an ssh jump (`-J`,
 * `ProxyJump`), a proxy of nc's or ncat's. An `ssh://` one is a URL, which `urlHosts` reads, and
 * `none` names none. */
function endpointHosts(list: string): string[] {
  return list.split(",").flatMap((one) => {
    if (one === "" || one === "none" || one.includes("://")) return [];
    const host = one.slice(one.lastIndexOf("@") + 1);
    return [
      host.startsWith("[") ? host.slice(0, host.indexOf("]") + 1) : host.replace(/:\d*$/, ""),
    ];
  });
}

/** The hosts ssh's options `args` connect to (ssh(1), ssh_config(5)): each jump host of `-J` and
 * of `-o ProxyJump`, and the host `-o HostName` names, which ssh connects to in place of the one
 * its destination names (a `%` token in it names that destination again). An `-o` value is
 * `Keyword=value` or `Keyword value`, its keyword in any case. */
function sshOptionHosts(args: Arguments): { jumps: Reach[]; target?: Reach } {
  const jumps: Reach[] = [];
  let target: Reach | undefined;
  for (const value of valuesOf(args, "-J"))
    for (const host of endpointHosts(value ?? ""))
      jumps.push({ host: hostName(host), named: value ?? "" });
  for (const value of valuesOf(args, "-o").map((one) => one ?? "")) {
    const option = /^\s*([A-Za-z]+)\s*(?:=\s*|\s+)(\S+)/.exec(value);
    const keyword = option?.[1]?.toLowerCase();
    const setting = option?.[2] ?? "";
    if (keyword === "proxyjump")
      for (const host of endpointHosts(setting)) jumps.push({ host: hostName(host), named: value });
    if (keyword === "hostname" && !setting.includes("%"))
      target = { host: hostName(setting), named: value };
  }
  return { jumps, target };
}

/** The host an ssh destination names, `[<user>@]<host>` (the last `@` ends the user); none for an
 * `ssh://` one, a URL `urlHosts` reads. */
function destinationHost(destination: string): string | undefined {
  return destination.includes("://")
    ? undefined
    : destination.slice(destination.lastIndexOf("@") + 1) || undefined;
}

/** The hosts each of ssh, scp, sftp, rsync, nc, ncat and telnet reaches, by its own grammar: the
 * operands it takes a host in and the options that name one. A word anywhere else is none. */
const REMOTE_READERS: Readonly<Record<string, (words: readonly string[]) => Reach[]>> = {
  // ssh(1): `ssh [options] destination [options] [command]`; OpenSSH reads its options again
  // after the destination. `-G` prints the configuration and `-V` the version, and neither
  // connects.
  ssh: (words) => {
    const before = posixArguments(words, SSH_VALUED);
    const [destination = "", ...rest] = before.operands;
    const args = {
      operands: [],
      options: [...before.options, ...posixArguments(rest, SSH_VALUED).options],
    };
    if (given(args, "-G", "-V")) return [];
    const { jumps, target } = sshOptionHosts(args);
    const host = destinationHost(destination);
    const reached =
      target ?? (host === undefined ? undefined : { host: hostName(host), named: destination });
    return [...jumps, ...(reached === undefined ? [] : [reached])];
  },
  // scp(1): `scp [options] source ... target`, each a local path or `[user@]host:[path]`
  // (SCP_ADDRESS), every remote one reached at `-o HostName` where that names one.
  scp: (words) => {
    const args = posixArguments(words, SCP_VALUED);
    const { jumps, target } = sshOptionHosts(args);
    const remotes = args.operands.flatMap((operand) => {
      const host = scpHost(operand);
      return host === undefined ? [] : [{ host: hostName(host), named: operand }];
    });
    return [...jumps, ...(target !== undefined && remotes.length > 0 ? [target] : remotes)];
  },
  // sftp(1): `sftp [options] destination`, `[user@]host[:path]`.
  sftp: (words) => {
    const args = posixArguments(words, SFTP_VALUED);
    const { jumps, target } = sshOptionHosts(args);
    const [destination] = args.operands;
    const host =
      destination === undefined
        ? undefined
        : (scpHost(destination) ?? destinationHost(destination));
    const reached =
      host === undefined
        ? undefined
        : (target ?? { host: hostName(host), named: destination ?? "" });
    return [...jumps, ...(reached === undefined ? [] : [reached])];
  },
  // rsync(1): each operand a local path, `[user@]host:path` through its remote shell or
  // `[user@]host::module` of a daemon (SCP_ADDRESS), an `rsync://` URL being read as one. The
  // remote shell `-e` names is a program, never a host; an ssh one's own options are read
  // (`-e 'ssh -J bastion'`).
  rsync: (words) => {
    const args = argumentsOf(words, RSYNC_VALUED);
    const shells = valuesOf(args, "-e", "--rsh")
      .map((value) => shellWords(value ?? ""))
      .filter(([program = ""]) => path.posix.basename(program) === "ssh")
      .map((shell) => sshOptionHosts(posixArguments(shell.slice(1), SSH_VALUED)));
    const target = shells.find((shell) => shell.target !== undefined)?.target;
    const remotes = args.operands.flatMap((operand) => {
      const host = scpHost(operand);
      return host === undefined ? [] : [{ host: hostName(host), named: operand }];
    });
    return [
      ...shells.flatMap((shell) => shell.jumps),
      ...(target !== undefined && remotes.length > 0 ? [target] : remotes),
    ];
  },
  // nc(1), OpenBSD's: `nc [options] [destination] [port]`, and `-x proxy[:port]`. A listener
  // (`-l`) reaches nothing, nor a Unix socket (`-U`).
  nc: (words) => ncHosts(argumentsOf(words, NC_VALUED), ["-x"], ["-l", "-U"]),
  netcat: (words) => ncHosts(argumentsOf(words, NC_VALUED), ["-x"], ["-l", "-U"]),
  // ncat(1): `ncat [options] [hostname] [port]`, and `--proxy host[:port]`.
  ncat: (words) =>
    ncHosts(
      argumentsOf(words, NCAT_VALUED),
      ["--proxy"],
      ["-l", "--listen", "-U", "--unixsock", "--vsock"]
    ),
  // telnet(1): `telnet [options] [host [port]]`.
  telnet: (words) => {
    const [host] = argumentsOf(words, TELNET_VALUED).operands;
    return host === undefined ? [] : [{ host: hostName(host), named: host }];
  },
};

/** The hosts an nc or ncat command reaches: each proxy `proxies` names, and its destination, its
 * first operand, unless one of `local` makes it listen or speak to a socket of no host. */
function ncHosts(args: Arguments, proxies: string[], local: string[]): Reach[] {
  const through = valuesOf(args, ...proxies).flatMap((value = "") =>
    endpointHosts(value).map((host) => ({ host: hostName(host), named: value }))
  );
  const [destination] = args.operands;
  return given(args, ...local) || destination === undefined
    ? through
    : [...through, { host: hostName(destination), named: destination }];
}

/** The hosts the run's gh serves: github.com, which its stand-in plays (gh-standin.ts), and the
 * loopback. */
const GH_SERVED = ["github.com", "127.0.0.1", "localhost", "::1"];

/** The repositories a gh command's words give `-R`/`--repo`, in either spelling. */
function ghRepos(words: readonly string[]): string[] {
  return words.flatMap((word, at) => {
    const before = words[at - 1];
    const repo =
      before === "-R" || before === "--repo" ? word : /^(?:-R|--repo=)(.+)$/.exec(word)?.[1];
    return repo === undefined ? [] : [repo];
  });
}

/** Every host a `gh` command names, which the run's gh serves when it is one of GH_SERVED:
 * `--hostname`'s, the HOST a `-R`/`--repo` names (gh's `[HOST/]OWNER/REPO`, and the URL
 * `https://HOST/OWNER/REPO` it takes, which `bashHosts` leaves to this rule rather than its URL
 * reading) and GH_HOST's, `env` giving the variables the command runs with. And a gh named by a
 * path or run through `legion gh` (`gh`, that path) is the real gh unless the path is the run's
 * own stand-in (`offWorldReach`): a bare `gh` resolves on the pane's PATH to <run>/bin/gh, and any
 * other gh reaches GitHub itself, whatever host it names. A path that holds an expansion
 * (`$R/bin/gh`) is not in the text, so it is read as the stand-in. */
function ghHosts(words: readonly string[], env: ReadonlyMap<string, string>, gh?: string): Reach[] {
  const named: Reach[] = [];
  words.forEach((word, at) => {
    const host = words[at - 1] === "--hostname" ? word : /^--hostname=(.+)$/.exec(word)?.[1];
    if (host !== undefined) named.push({ host: hostName(host), named: word, served: GH_SERVED });
  });
  for (const repo of ghRepos(words)) {
    const url = /^https?:\/\/(?:[^/@\s]*@)?([^/:\s]+)(?::\d+)?\/[^/\s]+\/[^/\s]+\/?$/i.exec(repo);
    const parts = repo.includes("://") ? [] : repo.split("/");
    const host = url?.[1] ?? (parts.length === 3 ? parts[0] : undefined);
    if (host !== undefined) named.push({ host: hostName(host), named: repo, served: GH_SERVED });
  }
  const host = env.get("GH_HOST");
  if (host !== undefined && host !== "")
    named.push({ host: hostName(host), named: `GH_HOST=${host}`, served: GH_SERVED });
  return gh === undefined
    ? named
    : [{ host: named[0]?.host ?? "github.com", named: gh, gh }, ...named];
}

/** sudo's options whose value is the next word (sudo(8), sudo 1.9). */
const SUDO_VALUED = optionSet(`
  -a -C -c -D -g -p -R -r -T -t -U -u --auth-type --close-from --login-class --chdir --group
  --prompt --chroot --role --command-timeout --type --other-user --user
`);
/** env's options whose value is the next word (env(1), GNU coreutils 9, and BSD's `-P`). */
const ENV_VALUED = optionSet("-u --unset -C --chdir -S --split-string -P");
/** A variable's assignment, `NAME=value`, as a shell, env(1) and sudo(8) read one. */
const ASSIGNMENT = /^([A-Za-z_]\w*)=([\s\S]*)$/;

/** What a wrapper runs: the command, and the variables it sets for it. */
interface Wrapped {
  readonly env: readonly [name: string, value: string][];
  readonly command: readonly string[];
}

/** The command `words` give after the assignments before it (env(1), sudo(8)), with those
 * assignments; undefined for words that give none. */
function assigned(words: readonly string[]): Wrapped | undefined {
  const env: [name: string, value: string][] = [];
  let at = 0;
  for (let found = ASSIGNMENT.exec(words[0] ?? ""); found !== null; ) {
    env.push([found[1] ?? "", found[2] ?? ""]);
    found = ASSIGNMENT.exec(words[++at] ?? "");
  }
  return at < words.length ? { env, command: words.slice(at) } : undefined;
}

/** The command `operands` give, or undefined for none. */
function ran(operands: readonly string[]): Wrapped | undefined {
  return operands.length === 0 ? undefined : { env: [], command: operands };
}

/** xargs's options whose value is the next word (xargs(1), GNU findutils 4.9); `-e`, `-i` and
 * `-l` take a value only against them. */
const XARGS_VALUED = optionSet(
  "-a -d -E -I -L -n -P -s --arg-file --delimiter --max-args --max-procs --max-chars --process-slot-var"
);

/** What xargs gives the command it runs from the items it reads (xargs(1)): each item as one more
 * argument, or, with `-I R` (`-i` and `--replace`, R `{}` unless given), the item in place of R in
 * each argument (`replace`); and whether it reads them from a file (`-a`, `--arg-file`) rather
 * than its input (`file`). */
interface XargsInput {
  readonly replace?: string;
  readonly file: boolean;
}

/** What the xargs whose words are `words` gives the command it runs (XargsInput). */
function xargsInput(words: readonly string[]): XargsInput {
  const args = posixArguments(words, XARGS_VALUED);
  const replace =
    valuesOf(args, "-I", "--replace").at(-1) ?? (given(args, "-i", "--replace") ? "{}" : undefined);
  return { ...(replace ? { replace } : {}), file: given(args, "-a", "--arg-file") };
}

/** The commands that run another, each read by its own grammar (`posixArguments`, every one of
 * them stopping at its first operand) into the command it runs, or undefined where it runs none.
 * A wrapper is no reach of its own: what it runs is read as if it stood alone (`invocationOf`). */
const WRAPPERS: Readonly<Record<string, (words: readonly string[]) => Wrapped | undefined>> = {
  // sudo(8): `sudo [options] [VAR=value...] [--] command`. `-e` edits files, `-l` lists what
  // may run, `-v` and `-K` refresh or drop the timestamp, `-V` and `-h` print: none runs it.
  sudo: (words) => {
    const args = posixArguments(words, SUDO_VALUED);
    const runs = !given(args, "-e", "--edit", "-l", "--list", "-v", "--validate", "-K");
    return runs && !given(args, "--remove-timestamp", "-V", "--version", "-h", "--help")
      ? assigned(args.operands)
      : undefined;
  },
  // env(1): `env [options] [-] [NAME=value...] [command]`, `-` being `-i`; `-S` splits its
  // string into the words it stands for. With no command it prints the environment.
  env: (words) => {
    const args = posixArguments(words, ENV_VALUED);
    const split = valuesOf(args, "-S", "--split-string").flatMap((value) =>
      shellWords(value ?? "")
    );
    const operands = [...split, ...args.operands];
    return assigned(operands[0] === "-" ? operands.slice(1) : operands);
  },
  // timeout(1): `timeout [options] DURATION command`.
  timeout: (words) =>
    ran(posixArguments(words, optionSet("-s --signal -k --kill-after")).operands.slice(1)),
  // nice(1): `nice [-n N] command`, and the older `nice -N command`.
  nice: (words) => ran(posixArguments(words, optionSet("-n --adjustment")).operands),
  // nohup(1): `nohup command`.
  nohup: (words) => ran(posixArguments(words, NO_OPTIONS).operands),
  // xargs(1): `xargs [options] [command [initial-arguments]]`, `echo` with no command; what it
  // gives the command from what it reads is read where a fetcher's URL is (`xargsInput`).
  xargs: (words) => ran(posixArguments(words, XARGS_VALUED).operands),
  // The shell's `command [-p] command`; `command -v` and `-V` print what a name is, running none.
  command: (words) => {
    const args = posixArguments(words, NO_OPTIONS);
    return given(args, "-v", "-V") ? undefined : ran(args.operands);
  },
  // The shell's `exec [-cl] [-a name] [command]`; with no command it only redirects.
  exec: (words) => ran(posixArguments(words, optionSet("-a")).operands),
  // The shell's `time [-p] command`, and time(1)'s `-f FORMAT` and `-o FILE`.
  time: (words) => ran(posixArguments(words, optionSet("-f --format -o --output")).operands),
};

/** The program a shell's words give it inline, the first operand after a `-c` among its options
 * (`sh -c '…'`, `bash -lc '…'`, `bash -o pipefail -c '…'`), its options read as bash(1) reads
 * them (`-o`, `+o`, `-O`, `+O`, `--rcfile` and `--init-file` take the next word); undefined for
 * a shell given no `-c`. */
function inlineProgram(words: readonly string[]): string | undefined {
  let inline = false;
  for (let at = 0; at < words.length; at++) {
    const word = words[at] ?? "";
    if (word === "--" || word === "-") return inline ? words[at + 1] : undefined;
    if (!/^[-+]/.test(word)) return inline ? word : undefined;
    if (word === "--rcfile" || word === "--init-file") at++;
    else if (!word.startsWith("--")) {
      if (word.slice(1).includes("c")) inline = true;
      if (/[oO]/.test(word.slice(1))) at++;
    }
  }
  return undefined;
}

/** A flag whose value is text the command writes (a comment's body, a commit's message, a
 * title), with that value, one shell word (SHELL_WORD): a URL in it is a link the agent wrote, not
 * a host it reached. */
const TEXT_FLAG = new RegExp(
  String.raw`(^|\s)(--body|-b|--title|-t|-m|--message|--subject)(=|[ \t]+)${SHELL_WORD}`,
  "g"
);
/** A `gh api` field flag with its `key=value` word (SHELL_WORD); its value is written text when
 * the key is `body`, `title` or `message`. */
const FIELD_FLAG = new RegExp(
  String.raw`(^|\s)(-f|-F|--field|--raw-field)(=|[ \t]+)(${SHELL_WORD})`,
  "g"
);

/** A host a call reaches, with the text that names it and, for a URL a program holds, where that
 * program is (` in a here-document python3 runs`). `served` names the hosts that serve it in place
 * of the run's world (a gh host value, GH_SERVED); `gh`, a gh named by a path or run through
 * `legion gh`, which reaches off the world unless it is the run's own stand-in, whatever host it
 * names (`ghHosts`); `get`, a reach that reads and sends nothing (a GET or a HEAD with no body,
 * `httpRead`; a `read` or `fetch` of the URL; git's read of a repository's refs, `refsRead`), the
 * one kind of reach rule 13 allows off the world (`githubRead`); `unread`, why the hosts a command
 * reaches are not in the text (a curl or a wget that takes its URLs from a file, URL_FILES; a URL
 * a fetcher is given that holds what the reader cannot expand and names no host, `unreadUrl`),
 * which makes the reach off the world whatever it names. */
interface Reach {
  readonly host: string;
  readonly named: string;
  readonly via?: string;
  readonly served?: readonly string[];
  readonly gh?: string;
  readonly get?: boolean;
  readonly unread?: string;
}

/** curl's options that send a body: its data and form options and an upload; `-X`/`--request`
 * is read by its value (`httpRead`). `-I`/`--head` asks for a HEAD, which sends none. */
const CURL_SENDS = [
  "-d",
  "--data",
  "--data-ascii",
  "--data-binary",
  "--data-raw",
  "--data-urlencode",
  "--json",
  "-F",
  "--form",
  "--form-string",
  "-T",
  "--upload-file",
];
/** wget's options that send a body: its post and body options; `--method` is read by its value
 * (`httpRead`). `--spider` asks for a HEAD, which sends none. */
const WGET_SENDS = ["--post-data", "--post-file", "--body-data", "--body-file"];
/** The methods that read and send nothing: a GET, and a HEAD, which reads no more than a GET. */
const READ_METHODS = ["GET", "HEAD"];

/** Whether a command is a curl or a wget that sends a GET or a HEAD with no body: none of the
 * options that send one (CURL_SENDS, WGET_SENDS) given, and every method it names (curl's
 * `-X`/`--request`, wget's `--method`) one of READ_METHODS. */
function httpRead({ name, words }: Pick<Invocation, "name" | "words">): boolean {
  if (name !== "curl" && name !== "wget") return false;
  const args = argumentsOf(words, name === "curl" ? CURL_VALUED : WGET_VALUED);
  const [sends, method] =
    name === "curl" ? [CURL_SENDS, ["-X", "--request"]] : [WGET_SENDS, ["--method"]];
  return (
    !given(args, ...sends) &&
    valuesOf(args, ...method).every((value) => READ_METHODS.includes(value?.toUpperCase() ?? ""))
  );
}

/** Whether a command is `git ls-remote`, which reads a repository's refs and nothing else.
 * Measured on 2026-11-16 with git 2.47.3 against https://github.com/actions/checkout (GIT_TRACE_CURL):
 * `git ls-remote <url> refs/tags/v7.0.1` sends `GET …/info/refs?service=git-upload-pack` and, under
 * the protocol v2 git speaks by default, one `POST …/git-upload-pack` carrying the `ls-refs`
 * command (under `-c protocol.version=0`, the GET alone); a `git fetch --depth 1` of the same ref
 * sends that GET and two POSTs to git-upload-pack, the second asking for the objects. So
 * ls-remote is a read of refs as a GET of them is, and a fetch or a clone is not. */
function refsRead({ name, words }: Pick<Invocation, "name" | "words">): boolean {
  return name === "git" && words[firstOperand(words, GIT_GLOBAL)] === "ls-remote";
}

/** The interpreters whose program health reads: the shells and the languages an agent writes a
 * script in. `jq` is none: a jq program reaches no host, so a body it reads is data. */
const INTERPRETER = /^(?:python[\d.]*|node|bun|deno|sh|bash|zsh|dash|ksh|perl|ruby|php)$/;
/** The shells, whose program health reads as a `bash` command's (`bashHosts`). */
const SHELL = /^(?:sh|bash|zsh|dash|ksh)$/;
/** A script's first line naming a shell as its interpreter. */
const SHELL_SHEBANG = /^#![^\n]*\b(?:ba|z|da|k)?sh\b/;

/** A command of shell text, as SHELL_COMMAND reads it: its first word as the shell reads it
 * (`shellWord`), that word's base name, the rest of its text, its lead, the text before its first
 * word that SHELL_COMMAND reads as its place (the separator before it, its prefix assignments and
 * a `command`, `exec` or `env` it starts with, env's own assignments among them), its whole text,
 * and where the command lies in the text. */
interface ShellCommand {
  readonly word: string;
  readonly name: string;
  readonly rest: string;
  readonly lead: string;
  readonly text: string;
  readonly at: number;
  readonly end: number;
}

/** Each command of shell text by SHELL_COMMAND. */
function commandsOf(code: string): ShellCommand[] {
  return [...code.matchAll(SHELL_COMMAND)].map((hit) => {
    const word = shellWord(hit[1] ?? "");
    return {
      word,
      name: path.posix.basename(word),
      rest: hit[2] ?? "",
      lead: hit[0].slice(0, hit[0].length - (hit[1] ?? "").length - (hit[2] ?? "").length),
      text: hit[0],
      at: hit.index ?? 0,
      end: (hit.index ?? 0) + hit[0].length,
    };
  });
}

/** The variables the assignments among shell text's words set (`shellWords`): a command's lead
 * (`commandsOf`), which SHELL_COMMAND reads before its word, holds its prefix assignments and
 * env's; one before a `;`, `&` or `|` there stands alone, and every pane exports GH_HOST, the one
 * variable health reads, so it is the command's too. */
function assignedIn(text: string): Map<string, string> {
  const set = new Map<string, string>();
  for (const found of shellWords(text).map((word) => ASSIGNMENT.exec(word)))
    if (found !== null) set.set(found[1] ?? "", found[2] ?? "");
  return set;
}
/** The wrapper a command's lead ends with, which SHELL_COMMAND reads as part of its place: env's
 * `-i` with it, as SHELL_COMMAND reads it, and the options after that env's own (WRAPPERS). */
const LEAD_WRAPPER =
  /(?:^|[\s;&|(`])(command|exec|env)(?:[ \t]+(?:-i|-|--ignore-environment))*[ \t]+$/;

/** A command as it runs once its wrappers are read off (WRAPPERS): the word that names it, that
 * word's base name, its words, the variables its prefix assignments and the wrappers before it
 * set, and, where an xargs among those wrappers runs it, what xargs gives it from what it reads
 * (`xargsInput`). */
interface Invocation {
  readonly word: string;
  readonly name: string;
  readonly words: readonly string[];
  readonly env: ReadonlyMap<string, string>;
  readonly xargs?: XargsInput;
}

/** The command `one` runs (Invocation), or undefined for a wrapper that runs none (`command -v
 * gh`) and for a command that is assignments alone (`assignmentsOf`). */
function invocationOf(one: ShellCommand): Invocation | undefined {
  const env = assignedIn(one.lead);
  const wrapper = LEAD_WRAPPER.exec(one.lead)?.[1];
  let word = wrapper ?? one.word;
  let words = [...(wrapper === undefined ? [] : [one.word]), ...shellWords(one.rest)];
  let xargs: Invocation["xargs"];
  if (wrapper === undefined && ASSIGNMENT.test(word)) return undefined;
  for (;;) {
    const name = path.posix.basename(word);
    const read = WRAPPERS[name];
    if (read === undefined) return { word, name, words, env, ...(xargs ? { xargs } : {}) };
    const wrapped = read(words);
    if (wrapped === undefined) return undefined;
    if (name === "xargs") xargs = xargsInput(words);
    for (const [key, value] of wrapped.env) env.set(key, value);
    [word = "", ...words] = wrapped.command;
  }
}

/** The variables a command that is assignments alone sets (`GH_HOST=ghe.example.com;`), or
 * undefined for any other command. SHELL_COMMAND reads its last assignment as its word. */
function assignmentsOf(one: ShellCommand): Map<string, string> | undefined {
  if (LEAD_WRAPPER.test(one.lead) || !ASSIGNMENT.test(one.word)) return undefined;
  return assignedIn(one.text);
}

/** The program an interpreter command runs: its first operand, a script's path (`python3 f.py`,
 * `bun run f.ts`) or a program given inline (`python3 -c '…'`, which names no file the run
 * wrote); `-` for a program it reads on stdin (`python3 -`, `node`, `bash -s arg`); or undefined
 * for a command that is no interpreter. */
function programOf(name: string, words: readonly string[]): string | undefined {
  if (!INTERPRETER.test(name)) return undefined;
  if (SHELL.test(name) && words.includes("-s")) return "-";
  const operands = words.filter((word) => word === "-" || !word.startsWith("-"));
  if ((name === "bun" || name === "deno") && operands[0] === "run") operands.shift();
  return operands[0] ?? "-";
}

/** The interpreter that runs the here-document whose `<<` lies at `at` in `code` as its program:
 * an interpreter that reads its program on stdin (`python3 - <<EOF`), or one a bare `cat` pipes
 * the body into (`cat <<EOF | node`). Undefined for every other command, whose body is text it
 * writes or data it reads: `cat > f`, `tee`, `gh … --body-file -`, `git commit -F -`, `jq`. */
function bodyRunner(code: string, at: number): string | undefined {
  const commands = commandsOf(code);
  const index = commands.findIndex((one) => one.at <= at && at < one.end);
  const owner = commands[index];
  if (owner === undefined) return undefined;
  const words = shellWords(owner.rest);
  if (programOf(owner.name, words) === "-") return owner.name;
  const next = commands[index + 1];
  return owner.name === "cat" &&
    words.length === 0 &&
    next !== undefined &&
    code[next.at] === "|" &&
    code.slice(owner.end, next.at).trim() === "" &&
    programOf(next.name, shellWords(next.rest)) === "-"
    ? next.name
    : undefined;
}

/** The host of every network URL in `text`. */
function urlHosts(text: string): Reach[] {
  return [...text.matchAll(NETWORK_URL)].map((hit) => ({
    host: hostName(hit[1] ?? ""),
    named: hit[0],
  }));
}

/** Every host a program reaches: a shell script's as a `bash` command's (`bashHosts`), and the
 * host of every network URL any other language's program holds, whatever it does with it, and
 * each fetch of it whose URL the reader cannot tell (`programFetches`). */
function programHosts(text: string, shell: boolean): Reach[] {
  return shell ? bashHosts(text) : [...urlHosts(text), ...programFetches(text)];
}

/** Every host one command reaches, read as it runs once its wrappers are read off
 * (`invocationOf`), by its own grammar, with `env` the variables it runs with: a remote in scp
 * form where `git` or `jj` takes a repository's address (`repositoryAddresses`, `scpHost`); a host
 * a curl or wget operand or an option of theirs that takes a host (CURL_HOSTS, WGET_HOSTS) names
 * without a scheme, and the hosts of each file a curl or a wget takes its URLs from (URL_FILES),
 * which are not in the text; the hosts of ssh, scp, sftp, rsync, nc, ncat and telnet
 * (REMOTE_READERS); and those a `gh` names, and the gh itself when it is named by a path or run as
 * `legion gh` (`ghHosts`). A host that holds an expansion (`ssh "$HOST"`, xargs's `{}`) is passed
 * over, since it is not in the text. */
function commandHosts(
  { word, name, words }: Invocation,
  env: ReadonlyMap<string, string>
): Reach[] {
  const hosts: Reach[] = [];
  for (const address of repositoryAddresses(name, words)) {
    const host = scpHost(address);
    if (host !== undefined) hosts.push({ host: hostName(host), named: address });
  }
  if (name === "curl" || name === "wget") {
    const [valued, takesHost] =
      name === "curl" ? [CURL_VALUED, CURL_HOSTS] : [WGET_VALUED, WGET_HOSTS];
    const args = argumentsOf(words, valued);
    const get = httpRead({ name, words });
    for (const one of [...args.operands, ...valuesOf(args, ...takesHost)]) {
      const bare = BARE_HOST.exec(one ?? "");
      if (one !== undefined && bare !== null && !one.includes("://"))
        hosts.push({ host: hostName(bare[1] ?? ""), named: one, ...(get ? { get } : {}) });
    }
    for (const [option, file = ""] of args.options.filter(([one]) =>
      URL_FILES[name]?.includes(one)
    ))
      hosts.push({
        host: `the hosts ${file === "-" ? "its input" : file} names`,
        named: `${option} ${file}`,
        unread: `${name} reads the URLs it fetches from there, which the reader does not read, so none is known to be a host of the run's world`,
      });
  }
  hosts.push(...(REMOTE_READERS[name]?.(words) ?? []));
  if (name === "gh")
    hosts.push(...ghHosts(words, env, word.includes("/") && !/[$`]/.test(word) ? word : undefined));
  if (name === "legion" && words[0] === "gh")
    hosts.push(...ghHosts(words.slice(1), env, "legion gh"));
  return hosts.filter(
    (one) => one.gh !== undefined || one.unread !== undefined || !/[$`{}*?]/.test(one.host)
  );
}

/** The reach of a URL a fetcher is given whose host the reader cannot tell: `url`, which `fetcher`
 * fetches as its `what` (its URL, git's remote), holds `expansion`, a variable the reader holds no
 * value of, a command substitution or a program's expression, and names no host of its own, so
 * the host it reaches is not in the text. Rule 13 refuses it whatever it names (`unread`), and
 * says what the reader cannot expand. */
function unreadUrl(fetcher: string, what: string, url: string, expansion: string): Reach {
  return {
    host: `the host ${url} names`,
    named: url,
    unread: `${fetcher}'s ${what} is ${url}, ${url === expansion ? "which" : `whose ${expansion}`} the reader cannot expand, so its host is not known to be one of the run's world`,
  };
}

/** What a fetcher's URL is where xargs gives it from items the reader does not read
 * (`unexpandedUrls`). */
const XARGS_INPUT = "<xargs's input>";

/** The URLs a command fetches whose host the reader cannot tell (`unreadUrl`), read off its words
 * as it runs once its wrappers are read off (`invocationOf`), each value the reader holds written
 * in (`literalFlow`): curl's operands and its `--url`, wget's operands, and the remote git's
 * clone, fetch, pull, push, ls-remote and `remote add` take (GIT_REMOTES), where one holds an
 * expansion (EXPANSION, a command substitution among them) and no host, neither in a URL
 * (`urlHosts`, whose host rule 13 judges as it reads it) nor where the command reads one without
 * a scheme (BARE_HOST, `scpHost`): `curl -s "$x"`, `wget "${base}/x"`, `git clone "$remote"`,
 * ``curl -s `cmd` ``. Where xargs runs the command and the reader does not read the items it
 * reads (`input`: a file, or an input no literal URL is piped into, `pipedText`), what xargs gives
 * it is such an expansion too (XARGS_INPUT): an item in place of its `-I` string, or, with none,
 * one more URL of curl's or wget's, and git's remote where its words give none (`echo "$x" |
 * xargs curl -s`, `cat remotes | xargs git clone`). A word in any other place is none (a header,
 * `-H "…$TOKEN"`; an output file, `-o "$out"`; a revision, `git log "$sha"`), and so is a command
 * that is given no URL (`git status`). */
function unexpandedUrls(
  { name, words, xargs }: Pick<Invocation, "name" | "words" | "xargs">,
  input: boolean
): Reach[] {
  let urls: (string | undefined)[];
  let bare: (url: string) => string | undefined;
  if (name === "curl" || name === "wget") {
    const args = argumentsOf(words, name === "curl" ? CURL_VALUED : WGET_VALUED);
    urls = [...args.operands, ...(name === "curl" ? valuesOf(args, "--url") : [])];
    bare = (url) => BARE_HOST.exec(url)?.[1];
  } else if (name === "git") {
    urls = [
      ...takerAddresses(words.slice(firstOperand(words, GIT_GLOBAL)), NO_OPTIONS, GIT_REMOTES),
    ];
    bare = scpHost;
  } else return [];
  const fed = input ? xargs : undefined;
  const replace = fed?.replace;
  if (replace !== undefined) urls = urls.map((url) => url?.replaceAll(replace, XARGS_INPUT));
  else if (fed !== undefined && (name !== "git" || urls.some((url) => url === undefined)))
    urls.push(XARGS_INPUT);
  return urls.flatMap((url = "") => {
    const expansion =
      EXPANSION.exec(url)?.[0] ?? (url.includes(XARGS_INPUT) ? XARGS_INPUT : undefined);
    const host = bare(url);
    if (expansion === undefined || urlHosts(url).length > 0) return [];
    if (host !== undefined && !/[$`{}*?]/.test(host)) return [];
    return [unreadUrl(name, name === "git" ? "remote" : "URL", url, expansion)];
  });
}

/** Where the quoted string or command substitution that opens at `at` of `code` ends, one past
 * its close, as the shell reads it: `'…'`; `"…"` and `` `…` ``, a backslash escaping the
 * character after it and a `$(…)` in the first read whole; or `$(…)`, its brackets, quoted
 * strings and substitutions nested. -1 for one that does not close. */
function shellSpanEnd(code: string, at: number): number {
  const open = code[at] ?? "";
  if (open === "'") {
    const close = code.indexOf("'", at + 1);
    return close < 0 ? -1 : close + 1;
  }
  const quoted = open === '"' || open === "`";
  let depth = 0;
  for (let i = at + (quoted ? 1 : 2); i < code.length; i++) {
    const char = code[i] ?? "";
    if (char === "\\") i++;
    else if (quoted && char === open) return i + 1;
    else if (
      code.startsWith("$(", i) ||
      (!quoted && (char === "'" || char === '"' || char === "`"))
    ) {
      const end = shellSpanEnd(code, i);
      if (end < 0) return -1;
      i = end - 1;
    } else if (quoted) continue;
    else if (char === "(") depth++;
    else if (char === ")") {
      if (depth === 0) return i + 1;
      depth--;
    }
  }
  return -1;
}

/** The command `one`, read off `spoken` (`commandsOf`), as it runs, read from `code` at the same
 * places (`spoken` is `code` with each quoted string that holds a separator emptied at its own
 * length, `bashHosts`): its words as written, a URL in a command substitution (`"$(gh api …)"`)
 * and a program an interpreter is given inline (`python3 -c '…'`) among them, its text read on
 * past a command substitution it holds outside quotes (`$(…)`, `` `…` ``, `shellSpanEnd`), which
 * SHELL_COMMAND ends it at, each one word of it. It ends at a `;`, `&`, `|`, line break or `)`
 * outside quotes and substitutions, or at the backquote that closes the one it opened in. */
function writtenInvocation(one: ShellCommand, code: string): Invocation | undefined {
  const restAt = one.end - one.rest.length;
  const substitutions: (readonly [from: number, to: number])[] = [];
  let end = restAt;
  for (; end < code.length; end++) {
    const char = code[end] ?? "";
    if (char === "\\") end++;
    else if (/[;&|\n)]/.test(char) || (char === "`" && one.lead.startsWith("`"))) break;
    else if (char === "'" || char === '"' || char === "`" || code.startsWith("$(", end)) {
      const close = shellSpanEnd(code, end);
      if (close < 0) break;
      if (char === "`" || char === "$") substitutions.push([end, close]);
      end = close - 1;
    }
  }
  // Each substitution a piece of its word SHELL_WORD reads (`\uE000<index>\uE001`), put back after.
  let rest = "";
  let from = restAt;
  for (const [index, [at, to]] of substitutions.entries()) {
    rest += `${code.slice(from, at)}\uE000${index}\uE001`;
    from = to;
  }
  rest += code.slice(from, end);
  const lead = code.slice(one.at, one.at + one.lead.length);
  const word = shellWord(code.slice(one.at + one.lead.length, restAt));
  const text = code.slice(one.at, end);
  const run = invocationOf({ ...one, word, name: path.posix.basename(word), rest, lead, text });
  const written = (piece: string) =>
    piece.replace(/\uE000(\d+)\uE001/g, (_all, index: string) => {
      const [at, to] = substitutions[Number(index)] ?? [0, 0];
      return code.slice(at, to);
    });
  return run && { ...run, word: written(run.word), words: run.words.map(written) };
}

/** The text a pipe hands the command `one` of `spoken` (`commandsOf`) on its input: the commands
 * of its pipeline before it, each compound command and bracket among them whole, back to the `;`,
 * `&`, `&&`, `||`, line break or opening bracket outside them that starts the pipeline; "" for a
 * command no pipe feeds (`|&` among them). Read off `spoken` by its tokens (`scanned`), and given
 * from `code`, which holds the same places. */
function pipedText(spoken: string, code: string, one: ShellCommand): string {
  if (!one.lead.startsWith("|") || spoken[one.at - 1] === "|") return "";
  const { tokens, raw, starts } = scanned(spoken);
  let depth = 0;
  let from = one.at;
  for (let i = tokens.findLastIndex((token) => token.end <= one.at); i >= 0; i--) {
    const gap = spoken.slice(tokens[i]?.end ?? 0, from);
    for (let k = gap.length - 1; k >= 0; k--) {
      const char = gap[k] ?? "";
      if (char === ")") depth++;
      else if (char === "(") depth--;
      const ends =
        /[;&\n]/.test(char) || (char === "|" && (gap[k - 1] === "|" || gap[k + 1] === "|"));
      if (depth < 0 || (depth === 0 && ends)) return code.slice(from, one.at);
    }
    const word = raw[i] ?? "";
    if (starts[i] && CLOSES.has(word)) depth++;
    else if ((starts[i] && OPENS.has(word)) || word === "{") depth--;
    if (depth < 0) return code.slice(from, one.at);
    from = tokens[i]?.at ?? 0;
  }
  return code.slice(from, one.at);
}

/** `code` with each quoted string that holds a `;`, `&`, `|`, a bracket, a backquote or a line
 * break emptied at its own length, the text `commandsOf` reads a `bash` command's commands off
 * (`bashHosts`). */
function spokenOf(code: string): string {
  return code.replace(SHELL_QUOTED, (quoted) =>
    /[;&|()`\n]/.test(quoted) ? `${quoted[0]}${"_".repeat(quoted.length - 2)}${quoted[0]}` : quoted
  );
}

/** The reaches of the command `one` of `code` (`spoken`, `spokenOf`, the text `commandsOf` read
 * it off) whose host the reader cannot tell, read off its words as written
 * (`writtenInvocation`): each URL a fetcher is given that holds what the reader cannot expand and
 * names no host (`unexpandedUrls`), what xargs gives it from items the reader does not read
 * among them, the reader reading those where a literal URL is piped into it (`pipedText`: `echo
 * https://… | xargs curl -s`, which the URL reading judges); and each fetch of a program an
 * interpreter is given inline whose URL it cannot tell (`programFetches`). */
function writtenReaches(one: ShellCommand, code: string, spoken: string): Reach[] {
  const full = writtenInvocation(one, code);
  if (full === undefined) return [];
  const input =
    full.xargs !== undefined &&
    (full.xargs.file || urlHosts(pipedText(spoken, code, one)).length === 0);
  const program = SHELL.test(full.name) ? undefined : programOf(full.name, full.words);
  return [
    ...unexpandedUrls(full, input),
    ...(program === undefined ? [] : programFetches(program)).map((reach) => ({
      ...reach,
      via: ` in the program ${full.name} runs`,
    })),
  ];
}

/** The text inside each command substitution of `code`, `$(…)` or `` `…` `` (`shellSpanEnd`),
 * outside single quotes and in double quotes too, but not the ones inside another, which are its
 * own text's, each with where that text starts in `code`. */
function substitutionsIn(code: string): { at: number; inner: string }[] {
  const inner: { at: number; inner: string }[] = [];
  let double = false;
  for (let i = 0; i < code.length; i++) {
    const char = code[i] ?? "";
    if (char === "\\") i++;
    else if (char === '"') double = !double;
    else if (char === "'" && !double) {
      const close = code.indexOf("'", i + 1);
      if (close < 0) break;
      i = close;
    } else if (char === "`" || code.startsWith("$(", i)) {
      const end = shellSpanEnd(code, i);
      if (end < 0) break;
      const at = i + (char === "`" ? 1 : 2);
      inner.push({ at, inner: code.slice(at, end - 1) });
      i = end - 1;
    }
  }
  return inner;
}

/** The `writtenReaches` of every command in a command substitution of `code`, those nested in it
 * included (`substitutionsIn`): SHELL_COMMAND reads a substitution among a command's words, an
 * assignment's value among them (`x=$(curl -s "$URL")`), as text of that command, so its
 * commands are read here, each as a command of its own. */
function substitutionReaches(code: string): Reach[] {
  return substitutionsIn(code).flatMap(({ inner }) => {
    const spoken = spokenOf(inner);
    return [
      ...commandsOf(spoken).flatMap((one) => writtenReaches(one, inner, spoken)),
      ...substitutionReaches(inner),
    ];
  });
}

/** Where each command in a command substitution of `code`, those nested in it included
 * (`substitutionsIn`), lies in `code` that reads and sends nothing (a GET or a HEAD with no body,
 * `httpRead`, and git's read of refs, `refsRead`), as `bashHosts` reads a command of the text's
 * own: `SHA=$(curl -s https://… | grep …)` is such a read of its URL. */
function substitutionReads(code: string): (readonly [from: number, to: number])[] {
  return substitutionsIn(code).flatMap(({ at, inner }) => [
    ...commandsOf(spokenOf(inner)).flatMap((one) => {
      const run = assignmentsOf(one) === undefined ? invocationOf(one) : undefined;
      return run !== undefined && (httpRead(run) || refsRead(run))
        ? [[at + one.at, at + one.end] as const]
        : [];
    }),
    ...substitutionReads(inner).map(([from, to]) => [at + from, at + to] as const),
  ]);
}

/** A gap between two tokens of shell text (`shellTokens`) that holds blanks alone, so the two are
 * words of one command. */
const BLANK_GAP = /^[ \t]*$/;
/** A gap of blanks and line breaks with no more than one `;` in it. */
const SEQUENCE_GAP = /^[ \t\n]*;?[ \t\n]*$/;
/** Whether a gap ends one command and starts the next whatever the first did: a `;` or a line
 * break (never `;;`, `&`, `&&`, `|`, `||`, a bracket or a backquote), or the start or the end of
 * the text (`edge`). */
function sequenced(gap: string, edge: boolean): boolean {
  return SEQUENCE_GAP.test(gap) && (edge || /[;\n]/.test(gap));
}
/** The reserved words a command follows (bash(1), SHELL GRAMMAR). */
const TAKES_COMMAND = new Set([
  "then",
  "do",
  "else",
  "elif",
  "if",
  "while",
  "until",
  "!",
  "{",
  "time",
]);
/** The reserved words that open a compound command where a command starts, and those that close
 * one; a `{` opens one wherever it stands as a word, so a function's body (`function f {`) does. */
const OPENS = new Set(["if", "case", "for", "select", "while", "until"]);
const CLOSES = new Set(["fi", "esac", "done", "}"]);
/** The loops, each closed by a `done`. */
const LOOPS = new Set(["for", "select", "while", "until"]);

/** Shell text as its tokens (`shellTokens`): each as written (`raw`), the gap before each and the
 * one after the last (`gaps`), and whether each starts a command (`starts`): the first, one after
 * a gap that holds more than blanks (an operator, a bracket, a line break), and one after a
 * reserved word a command follows (TAKES_COMMAND). */
interface Scanned {
  readonly tokens: readonly ShellToken[];
  readonly raw: readonly string[];
  readonly gaps: readonly string[];
  readonly starts: readonly boolean[];
}
function scanned(text: string): Scanned {
  const tokens = shellTokens(text);
  const raw = tokens.map((token) => text.slice(token.at, token.end));
  const gaps = [...tokens.map((token) => token.at), text.length].map((end, i) =>
    text.slice(tokens[i - 1]?.end ?? 0, end)
  );
  const starts: boolean[] = [];
  for (const [i, gap] of gaps.slice(0, tokens.length).entries())
    starts.push(
      i === 0 ||
        !BLANK_GAP.test(gap) ||
        (starts[i - 1] === true && TAKES_COMMAND.has(raw[i - 1] ?? ""))
    );
  return { tokens, raw, gaps, starts };
}

/** The last token of the command token `at` of `scan` starts: the last before a gap that holds
 * more than blanks. */
function commandEnd(scan: Scanned, at: number): number {
  let last = at;
  while (last + 1 < scan.raw.length && BLANK_GAP.test(scan.gaps[last + 1] ?? "")) last++;
  return last;
}

/** The words of a `for` loop's list, tokens `from` up to `to` (its `do`) of `scan`: each a literal
 * (`literalWord`: no expansion, command substitution, glob, brace or blank), blanks alone between
 * them, and a `;` or a line break after the last; undefined for any other list. */
function listWords(scan: Scanned, from: number, to: number): string[] | undefined {
  const words: string[] = [];
  for (let i = from; i < to; i++) {
    const word =
      scan.tokens[i]?.redirect === undefined ? literalWord(scan.raw[i] ?? "") : undefined;
    if (word === undefined || !BLANK_GAP.test(scan.gaps[i] ?? "")) return undefined;
    words.push(word);
  }
  return sequenced(scan.gaps[to] ?? "", false) ? words : undefined;
}

/** A `for` loop over literal words: its variable, its words, and its `do` and its `done` tokens. */
interface LiteralLoop {
  readonly name: string;
  readonly words: readonly string[];
  readonly opens: number;
  readonly closes: number;
}
/** The loop `for NAME in WORDS; do BODY done` that token `at` of `scan` starts, its list read by
 * `listWords` and its `done` the one that closes it; undefined for any other command. */
function literalLoop(scan: Scanned, at: number): LiteralLoop | undefined {
  const { raw, gaps, starts } = scan;
  const name = raw[at + 1] ?? "";
  if (!/^[A-Za-z_]\w*$/.test(name) || raw[at + 2] !== "in") return undefined;
  if (!BLANK_GAP.test(gaps[at + 1] ?? "") || !BLANK_GAP.test(gaps[at + 2] ?? "")) return undefined;
  const opens = raw.findIndex((word, i) => i > at + 2 && starts[i] === true && word === "do");
  if (opens < 0) return undefined;
  let closes = -1;
  for (let i = opens + 1, nested = 0; i < raw.length && closes < 0; i++) {
    if (starts[i] !== true) continue;
    if (LOOPS.has(raw[i] ?? "")) nested++;
    else if (raw[i] === "done") {
      if (nested === 0) closes = i;
      nested--;
    }
  }
  if (closes < 0) return undefined;
  const words = listWords(scan, at + 3, opens);
  if (words === undefined) return undefined;
  return { name, words, opens, closes };
}

/** The last token of the command of assignments alone (each word `NAME=value`, ASSIGNMENT) that
 * token `at` of `scan` starts, where the shell runs it in the text's own shell whatever ran before
 * it: after a `;`, a line break or the text's start and before the next or the text's end
 * (`sequenced`), and inside no bracket, backquote or compound command (`open`). -1 for any other
 * command: a prefix assignment (`repo=x curl …$repo`, whose `$repo` the shell expands before it
 * assigns), one after `&&`, `||` or `|`, one in `( … )` or an `if`. */
function assignmentsAlone(scan: Scanned, at: number, open: boolean): number {
  const last = commandEnd(scan, at);
  if (open) return -1;
  if (!sequenced(scan.gaps[at] ?? "", at === 0)) return -1;
  if (!sequenced(scan.gaps[last + 1] ?? "", last + 1 === scan.raw.length)) return -1;
  for (let i = at; i <= last; i++)
    if (scan.tokens[i]?.redirect !== undefined || !ASSIGNMENT.test(scan.raw[i] ?? "")) return -1;
  return last;
}

/** What `flowed` finds in a call's text, at places in the call's text: where a command sets a
 * variable as it reads it (`sites`: the variable, and where its name stands), and the spans that
 * name no variable to the shell (`literal`): literal text it reads as a value (the words of a
 * loop's list, the value of an assignment that is a literal), and each word `literalFlow` reads as
 * data a process of its own is given (`childWords`). */
interface Places {
  readonly sites: { readonly name: string; readonly at: number }[];
  readonly literal: (readonly [from: number, to: number])[];
}

/** Literal text `flowed` writes where a variable it reads is set, as a place in the text it
 * returns: a word of a loop's list that it writes the loop's body out with, or the value of an
 * assignment it binds the variable to; `name` is the variable. */
interface FlowSource {
  readonly name: string;
  readonly from: number;
  readonly to: number;
}

/** Shell text (`text`, lying at `offset` in the call's), with the variables `bound` holds written
 * where the shell expands them (`expandedWord`), in order: each loop `literalLoop` reads written
 * out once per word of its list, its body read with its variable bound to that word where
 * `eligible` holds the variable (and read once, the variable unbound, where it does not); and
 * each command of assignments alone (`assignmentsAlone`) binding each variable `eligible` holds to
 * the literal its value reads to (`literalWord`), or unbinding it where its value reads to none.
 * After a loop its variable and each one its body sets are unbound: the reader does not say which
 * word the loop ended on. `places` collects where each of those sets a variable, and the literal
 * text each reads. Returns the text, the variables it set, and where in that text each word of a
 * list it wrote a loop out with and each value it bound a variable to lies (`sources`). */
function flowed(
  text: string,
  offset: number,
  bound: Map<string, string>,
  eligible: ReadonlySet<string> | undefined,
  places: Places
): { text: string; set: ReadonlySet<string>; sources: FlowSource[] } {
  const scan = scanned(text);
  const { tokens, raw, gaps, starts } = scan;
  const set = new Set<string>();
  const sources: FlowSource[] = [];
  let out = "";
  let from = 0;
  // The brackets and compound commands open at the token, the backquotes before it, and the last
  // token of the command of assignments alone it is in.
  let depth = 0;
  let ticks = 0;
  let binds = -1;
  for (let i = 0; i < tokens.length; i++) {
    const token = tokens[i];
    if (token === undefined) break;
    const word = raw[i] ?? "";
    const gap = gaps[i] ?? "";
    depth += gap.split("(").length - gap.split(")").length;
    ticks += gap.split("`").length - 1;
    const loop = starts[i] && word === "for" ? literalLoop(scan, i) : undefined;
    const opens = tokens[loop?.opens ?? -1];
    const closes = tokens[loop?.closes ?? -1];
    if (loop !== undefined && opens !== undefined && closes !== undefined) {
      places.sites.push({ name: loop.name, at: offset + (tokens[i + 1]?.at ?? 0) });
      for (const word of tokens.slice(i + 3, loop.opens))
        places.literal.push([offset + word.at, offset + word.end]);
      const body = text.slice(opens.end, closes.at);
      // One copy of the body per word; one, the variable unbound, for a variable the reader does
      // not read (`eligible`), and for the first reading, which only finds where variables are set.
      const values: readonly (string | undefined)[] = eligible?.has(loop.name)
        ? loop.words
        : [undefined];
      const copies = values.map((value) => {
        const within = new Map(bound);
        if (value === undefined) within.delete(loop.name);
        else within.set(loop.name, value);
        return flowed(body, offset + opens.end, within, eligible, places);
      });
      out += text.slice(from, opens.end);
      if (eligible?.has(loop.name))
        for (const word of tokens.slice(i + 3, loop.opens))
          sources.push({
            name: loop.name,
            from: out.length - (opens.end - word.at),
            to: out.length - (opens.end - word.end),
          });
      for (const [index, copy] of copies.entries()) {
        if (index > 0) out += "\n";
        for (const one of copy.sources)
          sources.push({ ...one, from: out.length + one.from, to: out.length + one.to });
        out += copy.text;
      }
      for (const name of [loop.name, ...copies.flatMap((copy) => [...copy.set])]) {
        bound.delete(name);
        set.add(name);
      }
      from = closes.at;
      i = loop.closes;
      continue;
    }
    if (starts[i]) binds = assignmentsAlone(scan, i, depth !== 0 || ticks % 2 !== 0);
    const put = expandedWord(word, bound);
    out += text.slice(from, token.at) + put;
    from = token.end;
    const found = i <= binds ? ASSIGNMENT.exec(word) : null;
    if (found !== null) {
      const name = found[1] ?? "";
      const written = found[2] ?? "";
      const value = eligible?.has(name) ? literalWord(expandedWord(written, bound)) : undefined;
      places.sites.push({ name, at: offset + token.at });
      if (literalWord(written) !== undefined)
        places.literal.push([offset + token.end - written.length, offset + token.end]);
      if (value === undefined) bound.delete(name);
      else {
        bound.set(name, value);
        sources.push({ name, from: out.length - put.length + name.length + 1, to: out.length });
      }
      set.add(name);
    }
    if (word === "{" || (starts[i] && OPENS.has(word))) depth++;
    else if (starts[i] && CLOSES.has(word)) depth--;
  }
  return { text: out + text.slice(from), set, sources };
}

/** The commands that run text this reader does not read as the call's own commands: the string
 * `eval` runs, the file `source` and `.` read, the action a `trap` sets. */
const RUNS_TEXT = new Set(["eval", "source", ".", "trap"]);
/** The builtins that set or unset a variable by a name they are given (bash(1), SHELL BUILTIN
 * COMMANDS), `printf` by its `-v`: an expansion among their words may name any variable. */
const SETS_NAMED = new Set([
  "declare",
  "typeset",
  "local",
  "readonly",
  "export",
  "read",
  "readarray",
  "mapfile",
  "getopts",
  "unset",
  "let",
  "printf",
  "wait",
]);
/** The builtins that give a variable an attribute; with `-i` (an integer, whose assignments are
 * arithmetic) or `-n` (a name for another variable, whose assignments set that one), a literal
 * value set to a variable sets another. */
const GIVES_ATTRIBUTES = new Set(["declare", "typeset", "local", "readonly", "export"]);
/** The variables the shell sets itself (bash(1), Shell Variables), as `read` sets REPLY and `cd`
 * sets PWD, with no word of the call's naming them. */
const SHELL_SETS =
  /^(?:_|BASH\w*|COMP_\w+|READLINE_\w+|COPROC|DIRSTACK|EPOCHREALTIME|EPOCHSECONDS|EUID|FUNCNAME|GROUPS|HISTCMD|HOSTNAME|HOSTTYPE|LINENO|MACHTYPE|MAPFILE|OLDPWD|OPTARG|OPTIND|OSTYPE|PIPESTATUS|PPID|PWD|RANDOM|REPLY|SECONDS|SHELLOPTS|SHLVL|SRANDOM|UID)$/;

/** bash's builtins and reserved words (bash 5.2's `compgen -b` and `compgen -k`): the commands the
 * shell runs itself, any of which may read a word it is given as a variable's name, as code or as
 * arithmetic, and so set that variable (`read 'name'`, `time -p eval 'name=…'`, `[[ 1 -eq
 * 'name=1' ]]`, `test -v 'a[name=1]'`, each measured with bash 5.2.37). */
const SHELL_BUILTINS = new Set(
  `. : [ alias bg bind break builtin caller cd command compgen complete compopt continue declare
  dirs disown echo enable eval exec exit export false fc fg getopts hash help history jobs kill let
  local logout mapfile popd printf pushd pwd read readarray readonly return set shift shopt source
  suspend test times trap true type typeset ulimit umask unalias unset wait if then else elif fi
  case esac for select while until do done in function time { } ! [[ ]] coproc`.split(/\s+/)
);
/** A word of shell text that is one single-quoted string, in which the shell expands nothing. */
const SINGLE_QUOTED = /^'[^']*'$/;

/** Where `scan` gives a program the shell runs as a process of its own a word that is one
 * single-quoted string (SINGLE_QUOTED): a word of a command after its name, past its prefix
 * assignments and redirections, the name a literal (`literalWord`) that is no option and none of
 * SHELL_BUILTINS. The shell reads nothing in such a word and the process cannot set the shell's
 * variables, so the word names no variable to the shell: `grep -E '"ref"'`, `python3 -c '…'`,
 * `bash -c '…'`, whose own shell's variables are not the call's. None in a text that may run a
 * builtin by such a program's name: one that defines a function (`f() …`, `function f`) or an
 * alias (`alias`), either of which may run the builtin a word names (`f() { builtin "$@"; }; f
 * eval 'name=…'`), or that runs a command named by an expansion (`$a grep=eval`), which may be
 * `alias`. */
function childWords(scan: Scanned): (readonly [from: number, to: number])[] {
  const { tokens, raw, gaps, starts } = scan;
  const words: (readonly [from: number, to: number])[] = [];
  // Whether the command the token is in has had its name read, and whether that name is a program
  // the shell runs as a process of its own.
  let named = false;
  let child = false;
  for (const [i, token] of tokens.entries()) {
    const word = raw[i] ?? "";
    const read = literalWord(word);
    if (read === "alias" || read === "function" || /^[ \t]*\([ \t]*\)/.test(gaps[i + 1] ?? ""))
      return [];
    if (starts[i]) named = false;
    if (named) {
      if (child && token.redirect === undefined && SINGLE_QUOTED.test(word))
        words.push([token.at, token.end]);
      continue;
    }
    if (token.redirect !== undefined || ASSIGNMENT.test(word)) continue;
    if (read === undefined && /[$`]/.test(word)) return [];
    named = true;
    child = read !== undefined && !read.startsWith("-") && !SHELL_BUILTINS.has(read);
  }
  return words;
}

/** Each segment of the path of each network URL of `code` (NETWORK_URL) that ends at a `/` with
 * no `=` after it, at a `?` or a `#`, or where the URL ends: text the shell does not read as a
 * variable's name, as `/git/ref/tags/$tag` names `ref` to no shell. */
function urlSegments(code: string): (readonly [from: number, to: number])[] {
  const segments: (readonly [from: number, to: number])[] = [];
  for (const hit of code.matchAll(NETWORK_URL)) {
    const url = hit[0];
    const start = url.indexOf("/", url.indexOf("://") + 3);
    if (start < 0) continue;
    for (const one of url.slice(start).matchAll(/\/([\w.~%-]+)(?=\/(?!=)|[?#]|$)/g)) {
      const from = (hit.index ?? 0) + start + (one.index ?? 0) + 1;
      segments.push([from, from + (one[1] ?? "").length]);
    }
  }
  return segments;
}

/** Whether every place `code` names the variable `name` is a read of it (`$name`, or `${name…}`
 * but its assigning `${name=…}` and `${name:=…}`), one of the sites `flowed` reads it set, or
 * inside a span that names no variable to the shell (`places`' `literal`: literal text `flowed`
 * reads as a value, a single-quoted word a process of its own is given, a segment of a URL's path,
 * `urlSegments`). The places are read in the text as written, quotes and all, so that none is
 * missed for being quoted (`export "name=…"`, `eval 'name=…'`, `[[ -v 'a[name=1]' ]]`): a word of
 * quoted text that names it is one more, but in one of those spans. */
function namedOnlyAt(code: string, name: string, places: Places): boolean {
  const sites = new Set(places.sites.map((site) => site.at));
  for (const hit of code.matchAll(new RegExp(`(?<!\\w)${name}(?!\\w)`, "g"))) {
    const at = hit.index ?? 0;
    const read =
      code[at - 1] === "$" ||
      (code.slice(at - 2, at) === "${" && !/^:?=/.test(code.slice(at + name.length)));
    const literal = places.literal.some(([from, to]) => from <= at && at < to);
    if (!read && !literal && !sites.has(at)) return false;
  }
  return true;
}

/** A `bash` command's text with each variable it sets to a literal written where the shell
 * expands it (`flowed`, in two readings: the first finds where the text sets each variable, the
 * second writes those it reads): a `for` loop's, over a list of literal words, written out once
 * per word; and a command of assignments alone's, its value a literal or the value of a variable
 * the reader holds, as is or trimmed by `%`, `%%`, `#` or `##` (`expandedWord`). Each variable
 * `env` gives the text (the pane's, `bashHosts`) whose value is a literal is held from the text's
 * start, so a read of it is written as that value until the text sets it. No command runs:
 * nothing set any other way is read, a command's output, a file's text, arithmetic, a loop over
 * anything but literal words, an assignment inside a bracket, a compound command or after `&&`,
 * and its expansion stays as written. A variable is read only where every place the text names it
 * is a read of it, a place `flowed` reads it set, literal text it reads as a value, a
 * single-quoted word a process of its own is given (`namedOnlyAt`, `childWords`: the pattern of
 * `grep -E '"ref"'`) or a segment of a URL's path (`urlSegments`: `…/git/ref/tags/$tag` of a loop
 * over `ref`), where no word of a command of SETS_NAMED ends with its name (`read -arepo`
 * sets `repo`), and never one the shell sets itself (SHELL_SETS). The text is returned whole
 * where it holds what may set any variable unread: a comment (`#`, whose end the reader does not
 * read), a command of RUNS_TEXT, one of SETS_NAMED given an expansion among its words, one of
 * GIVES_ATTRIBUTES given `-i` or `-n` (each named bare or after `command` or `builtin`), or IFS,
 * which would split a value the reader writes as one word. What an earlier call left in a
 * persistent shell is not read, as no reader of a call's text reads it. Returns the text, and
 * where in it each literal lies that it gave a variable and wrote into every place the text reads
 * that variable (`flowed`'s `sources`, each of a variable of which no read stays in the text): that
 * value reaches only what those places hand it to. A value of a variable read where the reader
 * writes no value (after its loop, in single quotes, after an assignment of a value it does not
 * hold) goes where the reader does not follow, and is none of them. And the variables of `env` it
 * held that the text sets nowhere (`kept`), whose value a program the text runs gets too. */
function literalFlow(
  code: string,
  env: ReadonlyMap<string, string> = new Map()
): {
  code: string;
  given: (readonly [from: number, to: number])[];
  kept: ReadonlyMap<string, string>;
} {
  const whole = { code, given: [], kept: new Map<string, string>() };
  const scan = scanned(code);
  const { tokens, raw, gaps, starts } = scan;
  if (/(?<!\w)IFS(?!\w)/.test(code)) return whole;
  // The words of each command of SETS_NAMED, any of which may name a variable with an option's
  // letters before it.
  const naming: string[] = [];
  for (const [i, word] of raw.entries()) {
    if (tokens[i]?.redirect === undefined && word.startsWith("#")) return whole;
    if (!starts[i]) continue;
    const wrapped = (word === "command" || word === "builtin") && BLANK_GAP.test(gaps[i + 1] ?? "");
    const named = wrapped ? (raw[i + 1] ?? "") : word;
    if (RUNS_TEXT.has(named)) return whole;
    const last = commandEnd(scan, i);
    const words = raw.slice(i + 1, last + 1);
    if (GIVES_ATTRIBUTES.has(named) && words.some((one) => /^-\w*[in]/.test(one))) return whole;
    if (!SETS_NAMED.has(named)) continue;
    if (named === "printf" && !words.some((one) => one.startsWith("-v"))) continue;
    if (words.some((one) => /[$`]/.test(one)) || /[(`]/.test(gaps[last + 1] ?? "")) return whole;
    naming.push(...words);
  }
  const places: Places = { sites: [], literal: [...childWords(scan), ...urlSegments(code)] };
  flowed(code, 0, new Map(), undefined, places);
  const set = new Set(places.sites.map((site) => site.name));
  // The variables `env` gives the text that it names, each one whose value is a literal.
  const held = [...env].filter(
    ([name, value]) =>
      /^[A-Za-z_]\w*$/.test(name) && code.includes(name) && literalWord(value) === value
  );
  const eligible = new Set(
    [...set, ...held.map(([name]) => name)].filter(
      (name) =>
        !SHELL_SETS.test(name) &&
        namedOnlyAt(code, name, places) &&
        !naming.some((word) => new RegExp(`${name}(?!\\w)`).test(word))
    )
  );
  if (eligible.size === 0) return whole;
  const bound = held.filter(([name]) => eligible.has(name));
  const read = flowed(code, 0, new Map(bound), eligible, { sites: [], literal: [] });
  return {
    code: read.text,
    given: read.sources
      .filter((one) => !new RegExp(`\\$\\{?[#!]?${one.name}(?!\\w)`).test(read.text))
      .map((one) => [one.from, one.to] as const),
    kept: new Map(bound.filter(([name]) => !set.has(name))),
  };
}

/** The commands that fetch nothing and run nothing they are given, each with why: a URL among
 * their words or on their input is text they print or search, and reaches nothing where their own
 * output does not go on to something that may fetch it (`unfetched`). */
const QUIET: Readonly<Record<string, string>> = {
  echo: "it prints its words",
  printf:
    "it prints its words by a format, unless `-v` sets a variable to them, which may go on anywhere",
  grep: "it searches its input or its files for a pattern, as the measured calls' `| grep -E '\"sha\"'`",
  head: "it prints the first lines of its input or its files, as the measured calls' `| head -5`",
  tail: "it prints the last lines of its input or its files",
  jq: "its program reaches no host (INTERPRETER), so what it is given is data",
};
/** The words that open a compound command a word closes, each with that word. */
const CLOSER: Readonly<Record<string, string>> = {
  if: "fi",
  while: "done",
  until: "done",
  for: "done",
  select: "done",
  "{": "}",
};
/** The commands a command of QUIET may not stand beside: a function or an alias may be named as
 * one and run anything (`echo() { curl "$@"; }`), and `exec` sends every later output where its
 * redirections say. */
const RENAMES = new Set(["function", "alias", "exec"]);

/** Whether token `at` of `scan` makes one descriptor a copy of another, `N>&M` (`2>&1`, `>&2`),
 * which shellTokens reads as the operator with no word, a gap of `&` and the word `M`. */
function duplicates(scan: Scanned, at: number): boolean {
  const token = scan.tokens[at];
  return (
    token?.redirect !== undefined &&
    token.word === "" &&
    /^\d*[<>]$/.test(token.redirect) &&
    scan.gaps[at + 1] === "&" &&
    /^(?:\d+-?|-)$/.test(scan.raw[at + 1] ?? "")
  );
}

/** The last token of the command token `at` of `scan` starts (`commandEnd`), past each descriptor
 * it copies (`duplicates`): `curl … 2>&1 | grep` is one command piped into grep. */
function commandLast(scan: Scanned, at: number): number {
  let last = commandEnd(scan, at);
  while (duplicates(scan, last)) last = commandEnd(scan, last + 1);
  return last;
}

/** Whether a redirection among tokens `from` to `to` of `scan` sends output where the reader does
 * not follow it: every one but one that reads (`<`, `<<`), one to /dev/null, and `2>&1`, which
 * sends the errors where the output goes. */
function redirectsAway(scan: Scanned, from: number, to: number): boolean {
  for (let at = from; at <= to; at++) {
    const token = scan.tokens[at];
    const redirect = token?.redirect;
    if (redirect === undefined || /^\d*<<?$/.test(redirect) || token?.word === "/dev/null")
      continue;
    if (redirect === "2>" && duplicates(scan, at) && scan.raw[at + 1] === "1") continue;
    return true;
  }
  return false;
}

/** A command or a compound command of shell text, as `unfetched` follows its output: the compound
 * it stands in (`parent`, -1 for none); where its output goes (`outlet`): on where its compound's
 * goes, or out of the call, `"on"`; into the piece a pipe hands it to, that piece's index; or where
 * the reader does not follow it, `"lost"` (a file, a command substitution, the background); and,
 * for a command of QUIET, the text it spans (`quiet`). */
interface Piece {
  readonly parent: number;
  outlet?: "on" | "lost" | number;
  readonly quiet?: readonly [from: number, to: number];
}

/** The spans of `code`, a `bash` command's text, that hand nothing to anything that fetches: each
 * command of QUIET whose output goes, through each pipe and each compound command it stands in,
 * out of the call or into a command of QUIET whose output goes so. A command of QUIET is none where
 * an assignment comes before its name (`PATH=… grep`), where `printf` is given `-v`, where its text
 * holds a command substitution, or where it redirects its output (`redirectsAway`). The commands
 * are read off `spoken`, `code` with each quoted string that holds a separator emptied at its own
 * length (`bashHosts`), by the tokens of `scanned` and the operators in the gaps between them: `;`,
 * `&&`, `||` and a line break hand a command's output on where its compound's goes, `|` and `|&`
 * into the command or compound after it, and `&`, a substitution (`$(`, `<(`, `>(`, a backquote)
 * and an output redirection lose it. None in a text whose commands may run something by a quiet
 * command's name or whose shape the reader does not follow: one with a function, an alias or
 * `exec` (RENAMES), a command named by an expansion, a command of RUNS_TEXT, `case`, or a bracket
 * or a compound command that does not close. */
function unfetched(code: string, spoken: string): (readonly [from: number, to: number])[] {
  const scan = scanned(spoken);
  const { tokens, raw, gaps, starts } = scan;
  const pieces: Piece[] = [];
  // The brackets and compound commands open where the reader stands, each with what closes it.
  const open: { piece: number; closer: string }[] = [];
  // The piece whose outlet the next operator decides, and the one a pipe waits to hand on.
  let current = -1;
  let piping = -1;
  const settle = (outlet: "on" | "lost") => {
    const piece = pieces[current];
    if (piece !== undefined) piece.outlet ??= outlet;
    current = -1;
  };
  const add = (piece: Piece): number => {
    const at = pieces.push(piece) - 1;
    const from = pieces[piping];
    if (from !== undefined) from.outlet ??= at;
    piping = -1;
    return at;
  };
  const parent = () => open.at(-1)?.piece ?? -1;
  const close = (closer: string): boolean => {
    const top = open.at(-1);
    if (top?.closer !== closer || piping >= 0) return false;
    settle("on");
    open.pop();
    current = top.piece;
    return true;
  };
  // Reads the operators in the gap before token `at`; false for a gap the reader does not follow.
  const operators = (at: number): boolean => {
    const start = tokens[at - 1]?.end ?? 0;
    for (const hit of (gaps[at] ?? "").matchAll(/;;|&&|\|\||\|&|[;\n|&()`]/g)) {
      const op = hit[0];
      if (op === "|" || op === "|&") {
        if (current < 0 || piping >= 0) return false;
        piping = current;
        current = -1;
      } else if (op === "(" || (op === "`" && open.at(-1)?.closer !== "`")) {
        // A substitution's output is a word of the command it stands in, which may fetch it.
        const capture = op === "`" || /[$<>]/.test(spoken[start + (hit.index ?? 0) - 1] ?? "");
        if (current >= 0) settle("lost");
        const piece = add({ parent: parent(), ...(capture ? { outlet: "lost" as const } : {}) });
        open.push({ piece, closer: op === "(" ? ")" : "`" });
      } else if (op === ")" || op === "`") {
        if (!close(op)) return false;
      } else if (piping >= 0) {
        // A pipe goes on across a line break, and into nothing after any other operator.
        if (op !== "\n") return false;
      } else if (op === "&") settle("lost");
      else if (op === ";;") return false;
      else settle("on");
    }
    return true;
  };
  for (let i = 0; i < tokens.length; ) {
    if (!starts[i] || !operators(i)) return [];
    const word = raw[i] ?? "";
    if (word === "case") return [];
    if (Object.hasOwn(CLOSER, word)) {
      open.push({ piece: add({ parent: parent() }), closer: CLOSER[word] ?? "" });
      i = word === "for" || word === "select" ? commandEnd(scan, i) + 1 : i + 1;
      continue;
    }
    if (TAKES_COMMAND.has(word)) {
      i++;
      continue;
    }
    const last = commandLast(scan, i);
    if (CLOSES.has(word)) {
      if (!close(word)) return [];
      if (redirectsAway(scan, i + 1, last)) settle("lost");
      i = last + 1;
      continue;
    }
    let name = i;
    while (
      name <= last &&
      (tokens[name]?.redirect !== undefined || ASSIGNMENT.test(raw[name] ?? ""))
    )
      name++;
    const named = literalWord(raw[name] ?? "");
    if (name <= last && named === undefined && /[$`]/.test(raw[name] ?? "")) return [];
    const runs =
      named === "command" || named === "builtin" ? literalWord(raw[name + 1] ?? "") : named;
    if (runs !== undefined && (RUNS_TEXT.has(runs) || RENAMES.has(runs))) return [];
    if (/^[ \t]*\([ \t]*\)/.test(gaps[name + 1] ?? "")) return [];
    const from = tokens[i]?.at ?? 0;
    const to = tokens[last]?.end ?? 0;
    const quiet =
      named !== undefined &&
      Object.hasOwn(QUIET, named) &&
      !raw.slice(i, name).some((one) => ASSIGNMENT.test(one)) &&
      !(named === "printf" && raw.slice(name + 1, last + 1).some((one) => one.startsWith("-v"))) &&
      !/\$\(|`|[<>]\(/.test(code.slice(from, to));
    current = add({ parent: parent(), ...(quiet ? { quiet: [from, to] as const } : {}) });
    if (redirectsAway(scan, i, last)) settle("lost");
    i = last + 1;
  }
  if (!operators(tokens.length) || open.length > 0 || piping >= 0) return [];
  settle("on");
  const harmless = (at: number): boolean => {
    const piece = pieces[at];
    if (piece?.outlet === "on") return piece.parent < 0 || harmless(piece.parent);
    return (
      typeof piece?.outlet === "number" &&
      pieces[piece.outlet]?.quiet !== undefined &&
      harmless(piece.outlet)
    );
  };
  return pieces.flatMap((piece, at) =>
    piece.quiet !== undefined && harmless(at) ? [piece.quiet] : []
  );
}

/** Every host a `bash` command reaches, with the text that names it: the host of each network URL
 * in it that a command hands to something that may fetch it, which is every URL but those only a
 * command of QUIET prints or searches, its output going on to nothing that fetches (`unfetched`:
 * `echo "== $url =="`, `printf '%s\n' URL | grep x`, but not `echo URL | sh` or `echo URL | xargs
 * curl`), the literal values `literalFlow` wrote into each place their variable is read, which
 * reach what those places hand them to (a word of a `for` list, an assignment's value), those in
 * text it writes (the values of TEXT_FLAG and of a `body`, `title` or `message` FIELD_FLAG, and
 * here-document bodies but those an interpreter runs, `bodyRunner`) and the repository URL a gh's
 * `-R` names, which the gh rule reads (`ghHosts`); and every host each of its commands reaches
 * (`commandHosts`), the program a shell is given inline (`sh -c '…'`, `inlineProgram`) read as a
 * command of its own. A URL a GET or a HEAD with no body is given, or git's read of refs, is read
 * as such a reach (`get`); every other one reaches its host as a call that may send anything, so
 * a URL that goes where the reader does not follow it (a pipe into a shell or xargs, `eval`, a
 * file a later command reads, a command it does not know) is off the world unless its host is the
 * world's. And a URL a fetcher is given that holds what the reader cannot expand and names no host
 * reaches a host the text does not name, which is off the world whatever it is (`unread`): curl's,
 * wget's or git's (`unexpandedUrls`: `x=$(gh api …); curl -s "$x"`, `wget "$URL"`, `git clone
 * "$remote"`, ``curl -s `cmd` ``), what xargs gives one from items no literal URL is piped into
 * (`echo "$x" | xargs curl -s`; `echo https://… | xargs curl -s` is judged by the URL), and the
 * fetch of a program an interpreter is given inline (`python3 -c 'urlopen(sys.argv[1])'`,
 * `programFetches`); each command is read there with its words as the text holds them, a command
 * substitution one word (`writtenReaches`), and so is each command inside a command substitution
 * (`substitutionReaches`: `x=$(curl -s "$URL")`). No command is otherwise read inside a quoted
 * string a command is given as text (`spoken`). A command's words leave its redirections
 * out (`shellWords`), so `curl … > report.json` names no host `report.json`. The variables a
 * command runs with are those its prefix and its wrappers set, over those `env` holds (the pane's,
 * `offWorldReach`) and those a command of assignments alone or an `export` set before it: every
 * pane exports GH_HOST (empty, daemon-pane.ts), so a `GH_HOST=…` alone reaches every gh after it.
 * Each of the text's variables that a `for` loop over literal words or an assignment of a literal
 * sets, and each `known` holds a literal of (the pane's, or in a shell's inline program those of
 * the pane the text around it sets nowhere), is read where it is expanded (`literalFlow`), so a
 * loop's body is read once per word of its list, a URL the pane names (`curl "$DISPATCH_URL/…"`)
 * is judged by its value, and every reach of each of them is judged. */
function bashHosts(
  command: string,
  env: ReadonlyMap<string, string> = new Map(),
  known: ReadonlyMap<string, string> = env
): Reach[] {
  const { code: written, bodies } = withoutBodies(command);
  // A here-document's body is read at the place of its `<<` in the text (`bodyRunner`), which a
  // variable's value written in would move: a call with one has none written in.
  const flow =
    bodies.length === 0
      ? literalFlow(written, known)
      : { code: written, given: [], kept: new Map<string, string>() };
  const { code } = flow;
  const inline = [...code.matchAll(SHELL_INLINE)];
  // A quoted string that holds a `;`, `&`, `|`, a bracket, a backquote or a line break is text a
  // command is given (a commit message, a review body) or a program given whole (`sh -c '…'`):
  // SHELL_COMMAND would read a command inside it at each of those, so a body's `…; ssh keys
  // unchanged` would name a host `keys`. Commands are read with each such string emptied at its
  // own length (`spokenOf`), so a shell's program is still read from `code`, where SHELL_INLINE
  // finds it.
  const spoken = spokenOf(code);
  // The text the URL reading reads: `code` with each repository URL a gh's `-R` names emptied at
  // its own length, since the gh rule decides that host (github.com is the world's there), and
  // each literal value `literalFlow` wrote where its variable is read and each command of QUIET
  // whose output goes on to nothing that fetches (`unfetched`), neither of which reaches a host.
  const urls = code.split("");
  for (const [from, to] of [...flow.given, ...unfetched(code, spoken)]) urls.fill("_", from, to);
  const commands: Reach[] = [];
  // The reaches of each command that reads and sends nothing (a GET or a HEAD with no body,
  // `httpRead`, and git's read of refs, `refsRead`): every URL of its own text, read there as
  // such a reach and left out of the reading of the rest.
  const gets: Reach[] = [];
  const shell = new Map(env);
  for (const one of commandsOf(spoken)) {
    const set = assignmentsOf(one);
    for (const [key, value] of set ?? []) shell.set(key, value);
    const run = set === undefined ? invocationOf(one) : undefined;
    if (run === undefined) continue;
    if (run.name === "export")
      for (const found of run.words.map((word) => ASSIGNMENT.exec(word)))
        if (found !== null) shell.set(found[1] ?? "", found[2] ?? "");
    const runs = new Map([...shell, ...run.env]);
    commands.push(...commandHosts(run, runs));
    commands.push(...writtenReaches(one, code, spoken));
    if (httpRead(run) || refsRead(run)) {
      for (const reach of urlHosts(code.slice(one.at, one.end))) gets.push({ ...reach, get: true });
      urls.fill("_", one.at, one.end);
    }
    const gh =
      run.name === "gh"
        ? run.words
        : run.name === "legion" && run.words[0] === "gh"
          ? run.words
          : [];
    for (const repo of ghRepos(gh).filter((one) => one.includes("://"))) {
      const at = code.indexOf(repo, one.at);
      if (at >= 0 && at < one.end) urls.fill("_", at, at + repo.length);
    }
    const program = SHELL.test(run.name) ? inlineProgram(run.words) : undefined;
    if (program === undefined) continue;
    // The program whole, from `code`, where SHELL_INLINE reads the quotes of one that starts in
    // this command: `spoken` empties one that holds a separator.
    const quoted = inline.find((hit) => (hit.index ?? 0) >= one.at && (hit.index ?? 0) < one.end);
    const text = quoted === undefined ? program : (quoted[1] ?? "").slice(1, -1);
    // The program is read as a command of its own, each of its reaches with it, so the URL
    // reading of this text leaves its quoted string out: a GET the program runs is one there.
    if (quoted !== undefined) {
      const at = (quoted.index ?? 0) + quoted[0].length - (quoted[1] ?? "").length;
      urls.fill("_", at, at + (quoted[1] ?? "").length);
    }
    for (const reach of bashHosts(text, runs, flow.kept))
      commands.push({ ...reach, via: reach.via ?? ` in the program ${run.name} -c runs` });
  }
  commands.push(...substitutionReaches(code));
  for (const [from, to] of substitutionReads(code)) {
    for (const reach of urlHosts(code.slice(from, to))) gets.push({ ...reach, get: true });
    urls.fill("_", from, to);
  }
  const reached = urls
    .join("")
    .replace(TEXT_FLAG, (_all, lead, flag, separator) => `${lead}${flag}${separator}''`)
    .replace(FIELD_FLAG, (all, lead, flag, separator, value) =>
      /^['"]?(?:body|title|message)=/.test(value) ? `${lead}${flag}${separator}''` : all
    );
  const hosts = urlHosts(reached);
  for (const body of bodies) {
    const runner = bodyRunner(code, body.at);
    if (runner !== undefined)
      for (const one of programHosts(body.text, SHELL.test(runner)))
        hosts.push({ ...one, via: one.via ?? ` in a here-document ${runner} runs` });
  }
  return [...hosts, ...gets, ...commands];
}

/** The files a `bash` command writes a here-document's body into, with the body: `cat > f`,
 * `cat >> f` (`append`), `tee f`, `tee -a f`, each file one shell word (`shellTokens`). */
function bashScripts(command: string): { path: string; text: string; append: boolean }[] {
  const { code, bodies } = withoutBodies(command);
  const commands = commandsOf(code);
  return bodies.flatMap((body) => {
    const owner = commands.find((one) => one.at <= body.at && body.at < one.end);
    if (owner?.name === "tee") {
      const words = shellWords(owner.rest);
      const append = words.includes("-a") || words.includes("--append");
      return words
        .filter((word) => !word.startsWith("-"))
        .map((file) => ({ path: file, text: body.text, append }));
    }
    if (owner?.name !== "cat") return [];
    return shellTokens(owner.rest).flatMap(({ word, redirect }) =>
      redirect !== undefined && word !== "" && /^1?>>?\|?$/.test(redirect)
        ? [{ path: word, text: body.text, append: redirect.includes(">>") }]
        : []
    );
  });
}

/** A shell's quoted string. */
const SHELL_QUOTED = /'[^']*'|"(?:\\.|[^"\\])*"/g;
/** The program a shell is given inline: `sh -c '<program>'`, `bash -lc "<program>"`. */
const SHELL_INLINE =
  /\b(?:ba|z|da|k)?sh[ \t]+(?:-[a-z]*c|-\w+[ \t]+-c)[ \t]+('[^']*'|"(?:\\.|[^"\\])*")/g;

/** The files a `bash` command runs as a program, with what runs each: an interpreter's script
 * (`python3 f.py`, `bash f.sh`, `bun run f.ts`), a file `source` or `.` reads, a command named by
 * its path (`./f`), and each of those in a program a shell is given inline (`sh -c '…'`). A
 * quoted string of more than one word or line is text the command is given (a commit message, a
 * `--run` note), whose commands run nothing. A command substitution starts a command of its own,
 * so `body=$(cat f)` runs `cat`, not `f`: SHELL_COMMAND reads `body=$(cat` as one assignment. */
function bashRuns(command: string): { path: string; runner: string }[] {
  const code = withoutBodies(command).code;
  const inline = [...code.matchAll(SHELL_INLINE)].flatMap((hit) =>
    bashRuns((hit[1] ?? "").slice(1, -1))
  );
  const spoken = code
    .replace(SHELL_QUOTED, (quoted) => (/[\s;&|`]/.test(quoted) ? "''" : quoted))
    .replace(/\$\(|`/g, "$& ");
  return commandsOf(spoken)
    .flatMap((one) => {
      const words = shellWords(one.rest);
      const program = programOf(one.name, words);
      if (program !== undefined && program !== "-") return [{ path: program, runner: one.name }];
      if ((one.name === "source" || one.name === ".") && words[0] !== undefined)
        return [{ path: words[0], runner: "source" }];
      return one.word.includes("/") ? [{ path: one.word, runner: one.word }] : [];
    })
    .concat(inline);
}

/** A profile's models.yml, as far as its providers' addresses. */
const ModelsFile = z.looseObject({
  providers: z.record(z.string(), z.looseObject({ baseUrl: z.string().optional() })).optional(),
});

/** The hosts of the run's world: the loopback, every host a URL of its pane file names (its
 * stand-ins', its Dispatch's, its Envoy's and its NATS's), and the host of every provider's
 * baseUrl in the models.yml of each HOME the run's agents ran under (`home`, `home-<role>`): the
 * model gateway its profile names, read from the profile's own configuration. */
function worldHosts(run: Run): Set<string> {
  const hosts = new Set(["127.0.0.1", "localhost", "::1"]);
  const pane = path.join(run.dir, "pane.env");
  if (existsSync(pane))
    for (const hit of readFileSync(pane, "utf8").matchAll(ANY_URL))
      hosts.add(hostName(hit[1] ?? ""));
  const homes = readdirSync(run.dir).filter((name) => /^home(?:-[\w-]+)?$/.test(name));
  for (const home of homes) {
    const profiles = path.join(run.dir, home, ".omp", "profiles");
    if (!existsSync(profiles)) continue;
    for (const profile of readdirSync(profiles)) {
      const models = path.join(profiles, profile, "agent", "models.yml");
      if (!existsSync(models)) continue;
      const parsed = ModelsFile.safeParse(Bun.YAML.parse(readFileSync(models, "utf8")));
      for (const provider of Object.values(parsed.data?.providers ?? {}))
        for (const hit of (provider.baseUrl ?? "").matchAll(ANY_URL))
          hosts.add(hostName(hit[1] ?? ""));
    }
  }
  return hosts;
}

/** A tool call as a transcript records it, with the transcript it is in and when. */
type Call = Run["calls"][number];

/** The tools whose arguments rule 13 reads as text a call writes or sends, never as a host it
 * reaches, each with why. A tool neither here nor read by a door of its own (`reachesOf`) has
 * every string argument read for a network URL, so a tool nobody named is read too. */
const TEXT_TOOLS: Readonly<Record<string, string>> = {
  write:
    "its content is a file's text; a file the run goes on to run is read where it runs (`fileRuns`)",
  edit: "its input is a file's text; a file the run goes on to run is read where it runs (`fileRuns`)",
  task: "its prompts are a subagent's instructions; the subagent's own calls are transcripts of the run, read as any",
  todo: "its items are the agent's own list of steps",
  envoy_publish: "its message is text a session of the run's Envoy reads",
  envoy_send: "its message is text a session of the run's Envoy reads",
  legion: "its summary and decision are text the run's daemon, a host of its world, records",
  report_issue: "its content is a report on a tool, which the harness records",
  ...Object.fromEntries(
    dispatchToolSpecs.map((spec) => [
      spec.name,
      "a Dispatch tool: its arguments are text and refs the run's Dispatch, a host of its world, records or reads",
    ])
  ),
};
/** The tools that read the `path` (or `url`) they are given: an http(s) URL there is fetched. A
 * `grep`'s pattern is text it searches for. */
const PATH_READERS = new Set(["read", "fetch", "web_fetch", "grep"]);

/** The call a tool call is: a `write` to an `xd://<tool>` device is a call of that tool with the
 * arguments its content holds (a content that is no JSON object is its one `content`). */
function deviceCall(call: Call): Call & { shown: string } {
  const target = call.tool === "write" ? (stringArg(call.args, "path") ?? "") : "";
  if (!target.startsWith("xd://")) return { ...call, shown: call.tool };
  const content = stringArg(call.args, "content") ?? "";
  let args: ToolArgs = { content };
  try {
    args = ToolArgs.safeParse(JSON.parse(content)).data ?? args;
  } catch {}
  return { ...call, tool: target.slice("xd://".length), args, shown: `write ${target}` };
}

/** Every string a value holds, in its arrays and objects too. */
function stringsOf(value: unknown): string[] {
  if (typeof value === "string") return [value];
  if (Array.isArray(value)) return value.flatMap(stringsOf);
  return typeof value === "object" && value !== null ? Object.values(value).flatMap(stringsOf) : [];
}

/** A file the run wrote, as far as its text, and the call that wrote it last. */
interface Written {
  readonly path: string;
  readonly text: string;
  readonly tool: string;
  readonly file: string;
}

/** Whether two paths the run named are one file: equal once normalized, or the one relative and
 * the other ending in it (`fetch.py`, `<ws>/fetch.py`), since a transcript does not say the cwd. */
function sameFile(one: string, other: string): boolean {
  const [a, b] = [path.posix.normalize(one), path.posix.normalize(other)];
  return (
    a === b ||
    (!b.startsWith("/") && a.endsWith(`/${b}`)) ||
    (!a.startsWith("/") && b.endsWith(`/${a}`))
  );
}

/** The files a call writes and the text it writes into each: a `write` call's content, an `edit`
 * call's input appended to each file it edits, a here-document a `bash` command writes to a file
 * (`bashScripts`), and each of those an `eval` cell's session tool calls write (`evalToolCalls`). */
function writtenText(call: Call): { path: string; text: string; append: boolean }[] {
  if (call.tool === "eval")
    return (evalToolCalls(stringArg(call.args, "code") ?? "") ?? []).flatMap((one) =>
      writtenText({ ...call, tool: one.tool, args: one.args })
    );
  if (call.tool === "write")
    return [
      {
        path: stringArg(call.args, "path") ?? "",
        text: stringArg(call.args, "content") ?? "",
        append: false,
      },
    ];
  if (call.tool === "edit") {
    const input = stringArg(call.args, "input") ?? "";
    const edited = [...input.matchAll(EDIT_HEADER)].map((hit) => hit[1] ?? "");
    const named = stringArg(call.args, "path");
    return [...edited, ...(named === undefined ? [] : [named])].map((file) => ({
      path: file,
      text: input,
      append: true,
    }));
  }
  return call.tool === "bash" ? bashScripts(stringArg(call.args, "command") ?? "") : [];
}

/** A Python or JavaScript string literal on one line, with its text. */
const STRING_LITERAL = /(['"`])((?:\\.|(?!\1)[^\\\n])*)\1/g;
/** The command a subprocess call of an eval cell runs, as its first argument: a string literal
 * or a list of them (`execSync("bun f.ts")`, `subprocess.run(["python3", "f.py"])`,
 * `os.system(…)`), or Bun's shell (`` $`bun f.ts` ``). */
const SUBPROCESS =
  /\b(?:execSync|execFileSync|execFile|exec|spawnSync|spawn|system|popen|Popen|run|call|check_call|check_output|getoutput|getstatusoutput)\s*\(\s*(\[[^\]\n]*\]|(['"`])(?:\\.|(?!\2)[^\\\n])*\2)|\$(`[^`]*`)/g;
/** A file an eval cell loads as code by its path: Python's `runpy.run_path`, `execfile` and
 * `exec(open(…))`, JavaScript's `require`, `import(…)` and `import … from`; and a module Python
 * imports by its name (`import fetch`, `from fetch import …`). */
const EVAL_LOAD =
  /\b(?:run_path|execfile|require|import)\s*\(\s*(['"`])([^'"`\n]+)\1|\bexec\s*\(\s*open\s*\(\s*(['"])([^'"\n]+)\3|\bfrom\s+(['"])([^'"\n]+)\5|^[ \t]*(?:from[ \t]+([\w.]+)[ \t]+import|import[ \t]+([\w.]+))/gm;

/** The files an `eval` cell runs: each it loads as code (EVAL_LOAD), and each the command of a
 * subprocess call of it runs (SUBPROCESS, read by `bashRuns`): a string's text is that command,
 * and each literal of a list is one word of it, each character a shell word ends at escaped, so
 * `["python3", "my fetch.py"]` runs `my fetch.py`. A file a string of the cell names otherwise
 * (`open("f")`, a message quoting `` `bun f.ts` ``) it does not run. */
function evalRuns(code: string): { path: string; runner: string }[] {
  const commands = [...code.matchAll(SUBPROCESS)].map((hit) => {
    const given = hit[1] ?? hit[3] ?? "";
    const literals = [...given.matchAll(STRING_LITERAL)].map((word) => word[2] ?? "");
    return given.startsWith("[")
      ? literals.map((word) => word.replace(/[^\w@%+=:,./-]/g, "\\$&") || "''").join(" ")
      : literals.join(" ");
  });
  return [
    ...commands.flatMap(bashRuns),
    ...[...code.matchAll(EVAL_LOAD)].map((hit) => ({
      path: hit[2] ?? hit[4] ?? hit[6] ?? `${(hit[7] ?? hit[8] ?? "").replace(/\./g, "/")}.py`,
      runner: "eval",
    })),
  ];
}

/** The files the run wrote (`written`) that a call runs, with whether each is read as a shell
 * script: a file a `bash` command runs (`bashRuns`), the files its own here-documents write
 * (`bashScripts`, `cat > f <<EOF … EOF; python3 f`) among them, or an `eval` cell runs
 * (`evalRuns`), by its path or, for a module a cell loads with no extension, by that path with
 * one. */
function fileRuns(call: Call, written: readonly Written[]): { file: Written; shell: boolean }[] {
  const command = call.tool === "bash" ? (stringArg(call.args, "command") ?? "") : "";
  const runs =
    call.tool === "bash"
      ? bashRuns(command)
      : call.tool === "eval"
        ? evalRuns(stringArg(call.args, "code") ?? "")
        : [];
  const files = [...written, ...record(bashScripts(command), written, call.tool, call.file)];
  return runs.flatMap((run) => {
    const file = files.findLast(
      (one) =>
        sameFile(one.path, run.path) ||
        (run.runner === "eval" && sameFile(one.path.replace(/\.[cm]?[jt]s$/, ""), run.path))
    );
    if (file === undefined) return [];
    const shell =
      SHELL.test(run.runner) ||
      run.runner === "source" ||
      (!INTERPRETER.test(run.runner) &&
        (/\.(?:sh|bash)$/.test(file.path) || SHELL_SHEBANG.test(file.text)));
    return [{ file, shell }];
  });
}

/** The end (one past it) of the Python or JavaScript string literal whose quote is at `at`: a
 * triple-quoted Python string, a JavaScript template with the code of each `${…}` skipped as code,
 * or a one-line string; -1 for one that does not end. */
function literalEnd(code: string, at: number): number {
  const quote = code[at] ?? "";
  if (quote === "`") {
    for (let i = at + 1; i < code.length; i++) {
      if (code[i] === "\\") i++;
      else if (code[i] === "`") return i + 1;
      else if (code.startsWith("${", i)) {
        i = closer(code, i + 2, "}");
        if (i < 0) return -1;
      }
    }
    return -1;
  }
  const fence = code.startsWith(quote.repeat(3), at) ? quote.repeat(3) : quote;
  for (let i = at + fence.length; i < code.length; i++) {
    if (code[i] === "\\") i++;
    else if (code.startsWith(fence, i)) return i + fence.length;
    else if (code[i] === "\n" && fence.length === 1) return -1;
  }
  return -1;
}

/** The brackets of Python and JavaScript, by their opener. */
const CLOSERS: Readonly<Record<string, string>> = { "(": ")", "[": "]", "{": "}" };

/** Where the `close` lies that ends the code from `at`, past nested brackets and string literals
 * (`literalEnd`); -1 for code it does not end in, or that closes another bracket first. */
function closer(code: string, at: number, close: string): number {
  const open: string[] = [];
  for (let i = at; i < code.length; i++) {
    const char = code[i] ?? "";
    if (char === "'" || char === '"' || char === "`") {
      const end = literalEnd(code, i);
      if (end < 0) return -1;
      i = end - 1;
    } else if (CLOSERS[char] !== undefined) open.push(CLOSERS[char]);
    else if (char === ")" || char === "]" || char === "}") {
      if (open.length === 0) return char === close ? i : -1;
      if (open.pop() !== char) return -1;
    }
  }
  return -1;
}

/** A call of a program that fetches the URL an argument gives, with its callee whole
 * (`urllib.request.urlopen`): Python's urllib (`urlopen` and `urlretrieve` under any module's
 * name, `Request`), requests, httpx and http.client (whose connection is given its host);
 * JavaScript's `fetch`, axios and node's http and https; Ruby's Net::HTTP and `URI.open`. A method
 * of anything else is none: `tool.fetch(…)`, a session tool call `evalHosts` reads, and
 * `origin.fetch()`. */
const FETCH_CALL =
  /(?<![\w$.])((?:[A-Za-z_][\w.]*\.)?(?:urlopen|urlretrieve)|(?:urllib\.request\.)?Request|(?:requests|httpx)\.(?:get|head|post|put|patch|delete|options|request|stream)|(?:http\.client\.)?HTTPS?Connection|fetch|axios(?:\.(?:get|head|post|put|patch|delete|options|request))?|https?\.(?:get|request)|Net::HTTP\.(?:get|get_response|start)|URI\.open)\s*\(/g;

/** The arguments of the call whose bracket opens right before `open` in `code`, each its text
 * trimmed, up to the `)` that closes it (`closer`), past nested brackets and string literals;
 * undefined for a call that does not close. */
function callArguments(code: string, open: number): string[] | undefined {
  const close = closer(code, open, ")");
  if (close < 0) return undefined;
  const args: string[] = [];
  let depth = 0;
  let from = open;
  for (let i = open; i < close; i++) {
    const char = code[i] ?? "";
    if (char === "'" || char === '"' || char === "`") {
      const end = literalEnd(code, i);
      if (end < 0) return undefined;
      i = end - 1;
    } else if (CLOSERS[char] !== undefined) depth++;
    else if (char === ")" || char === "]" || char === "}") depth--;
    else if (char === "," && depth === 0) {
      args.push(code.slice(from, i).trim());
      from = i + 1;
    }
  }
  return [...args, code.slice(from, close).trim()].filter((one) => one !== "");
}

/** Whether `text` is one Python or JavaScript string literal (`literalEnd`, a Python prefix
 * before it) of plain text: no `$` (a template's interpolation, or a shell's expansion written
 * into a program it is given inline) and no f-string's interpolation (a brace but a doubled one). */
function plainLiteral(text: string): boolean {
  const prefix = /^[rRbBuUfF]{0,2}/.exec(text)?.[0] ?? "";
  const quote = text[prefix.length];
  if (quote !== "'" && quote !== '"' && quote !== "`") return false;
  if (literalEnd(text, prefix.length) !== text.length || text.includes("$")) return false;
  return !/[fF]/.test(prefix) || !text.replace(/\{\{/g, "").includes("{");
}

/** The fetches of a program in a language an interpreter runs or an eval cell holds (FETCH_CALL)
 * whose URL the reader cannot tell (`unreadUrl`): the URL the call is given (its `url=`, else its
 * first argument, requests' and httpx's `request(method, url)` its second) is no string literal
 * of plain text (`plainLiteral`: an expression, `sys.argv[1]`, `base + "/x"`, an interpolation)
 * and names no host (`urlHosts`, whose host rule 13 judges where it reads the program). A call
 * that does not close is read so too, its URL the rest of its line. One given no URL is none. */
function programFetches(code: string): Reach[] {
  return [...code.matchAll(FETCH_CALL)].flatMap((hit) => {
    const callee = hit[1] ?? "";
    const open = (hit.index ?? 0) + hit[0].length;
    const args = callArguments(code, open);
    const keyed = args?.find((one) => /^url\s*=(?!=)/.test(one));
    const url =
      args === undefined
        ? (code.slice(open).split("\n")[0] ?? "").trim()
        : (keyed?.replace(/^url\s*=\s*/, "") ??
          args[/^(?:requests|httpx)\.request$/.test(callee) ? 1 : 0]);
    if (url === undefined || url === "" || plainLiteral(url) || urlHosts(url).length > 0) return [];
    const named = url.replace(/\s+/g, " ");
    return [unreadUrl(callee, "URL", named, named)];
  });
}

/** The name a string literal is given by the text before it: `key:`, `"key":` or `key=`, a
 * Python string's prefix (`f`, `r`, `b`) between. */
const KEYED = /(?:(\w+)|(["'])(\w+)\2)\s*[:=]\s*[rbfuRBFU]{0,2}$/;

/** A Python f-string's prefix (`f`, `rf`, `Fr`) right before its quote. */
const F_STRING = /[rRbBuU]?[fF][rRbBuU]?$/;

/** The arguments a session tool call's argument text gives, as far as its string literals: each
 * by its key (`{path: "…"}`, `path="…"`, `{"path": "…"}`), the rest by place (`_0`, `_1`, a
 * quoted key among them); and the code of each `${…}` in a JavaScript template and each `{…}` in
 * a Python f-string among them (`{{` is a brace of the text). Undefined for text whose literals
 * or expressions do not end. */
function literalArgs(text: string): { args: ToolArgs; code: string[] } | undefined {
  const args: ToolArgs = {};
  const code: string[] = [];
  let place = 0;
  for (let at = 0; at < text.length; at++) {
    const quote = text[at] ?? "";
    if (quote !== "'" && quote !== '"' && quote !== "`") continue;
    const end = literalEnd(text, at);
    if (end < 0) return undefined;
    const fence = quote !== "`" && text.startsWith(quote.repeat(3), at) ? 3 : 1;
    const value = text.slice(at + fence, end - fence);
    const opener = quote === "`" ? "${" : F_STRING.test(text.slice(0, at)) ? "{" : undefined;
    for (let i = opener === undefined ? -1 : value.indexOf(opener); i >= 0; ) {
      if (opener === "{" && value[i + 1] === "{") {
        i = value.indexOf(opener, i + 2);
        continue;
      }
      const close = closer(value, i + (opener?.length ?? 0), "}");
      if (close < 0) return undefined;
      code.push(value.slice(i + (opener?.length ?? 0), close));
      i = value.indexOf(opener ?? "", close + 1);
    }
    const key = KEYED.exec(text.slice(0, at));
    args[key?.[1] ?? key?.[3] ?? `_${place++}`] = value;
    at = end - 1;
  }
  return { args, code };
}

/** A session tool an eval cell calls by name, `tool.<name>(`, in Python or JavaScript. */
const TOOL_CALL = /\btool\.(\w+)\s*\(/g;

/** Each session tool call of an eval cell (TOOL_CALL), in the cell's order: its tool, the
 * arguments its string literals give (`literalArgs`), the code of the expressions among them, and
 * where its argument text lies. Undefined for a cell with a call whose brackets or literals do
 * not end. */
function evalToolCalls(code: string) {
  const calls: { tool: string; args: ToolArgs; code: string[]; open: number; close: number }[] = [];
  for (const hit of code.matchAll(TOOL_CALL)) {
    const open = (hit.index ?? 0) + hit[0].length;
    const close = closer(code, open, ")");
    const given = close < 0 ? undefined : literalArgs(code.slice(open, close));
    if (given === undefined) return undefined;
    calls.push({ tool: hit[1] ?? "", ...given, open, close });
  }
  return calls;
}

/** Every host an `eval` cell reaches: each session tool call of it (`evalToolCalls`) by the
 * reading its tool gets as a call of its own (`reachesOf`: none for a tool of TEXT_TOOLS,
 * `bashHosts` for `bash`, the URL a `read` is given, the class default for a tool nobody named),
 * given the files the run wrote, those the cell's calls before it wrote included, the pane's
 * variables (`pane`), and the code of each expression among its literals; a `fetch` the cell
 * gives no URL of plain text that names a host (`{url: u}`, a template's `${base}/x`), whose host
 * the reader cannot tell (`unreadUrl`); then the rest of the cell, those calls blanked, as a
 * program (`programHosts`: every network URL in it, and each fetch of it whose URL the reader
 * cannot tell). A cell with a call whose brackets or literals do not end is read whole as code. */
function evalHosts(
  call: Call,
  written: readonly Written[],
  pane: ReadonlyMap<string, string>
): Reach[] {
  const code = stringArg(call.args, "code") ?? "";
  const calls = evalToolCalls(code);
  if (calls === undefined) return programHosts(code, false);
  const rest = code.split("");
  const reached: Reach[] = [];
  const seen = [...written];
  for (const one of calls) {
    const inner = { ...call, tool: one.tool, args: one.args };
    const url = stringArg(one.args, "url") ?? stringArg(one.args, "path");
    const given = code.slice(one.open, one.close).replace(/\s+/g, " ").trim();
    const unread =
      (one.tool === "fetch" || one.tool === "web_fetch") &&
      (url === undefined ? given !== "" : /[$`{]/.test(url) && urlHosts(url).length === 0);
    for (const hit of [
      ...reachesOf(inner, seen, pane),
      ...(unread ? [unreadUrl(`tool.${one.tool}`, "URL", url ?? given, url ?? given)] : []),
      ...one.code.flatMap((expression) =>
        evalHosts({ ...call, args: { code: expression } }, seen, pane)
      ),
    ])
      reached.push({ ...hit, via: hit.via ?? ` in the cell's tool.${one.tool} call` });
    seen.push(...record(writtenText(inner), seen, call.tool, call.file));
    rest.fill(" ", one.open, one.close);
  }
  return [...reached, ...programHosts(rest.join(""), false)];
}

/** Every host a call reaches, by its tool: a `bash` command's (`bashHosts`, the pane's variables
 * `pane` its own); an `eval` cell's (`evalHosts`), in whatever language; an http(s) URL a `read`,
 * `fetch` or `grep` is given as its path; and for a file the call runs that the run wrote
 * (`fileRuns`), every host its program reaches. A tool of TEXT_TOOLS reaches none by its
 * arguments; any other tool reaches the host of every network URL in any string argument but `i`,
 * the intent every call names. */
function reachesOf(
  call: Call,
  written: readonly Written[],
  pane: ReadonlyMap<string, string>
): Reach[] {
  const ran = fileRuns(call, written).flatMap(({ file, shell }) =>
    programHosts(file.text, shell).map((one) => ({
      ...one,
      via: ` in ${file.path}, which a ${file.tool} call in ${file.file} wrote and it runs`,
    }))
  );
  if (call.tool === "bash")
    return [...bashHosts(stringArg(call.args, "command") ?? "", pane), ...ran];
  if (call.tool === "eval") return [...evalHosts(call, written, pane), ...ran];
  if (PATH_READERS.has(call.tool)) {
    // A read of a URL is a GET with no body, unless the call names another method than a GET or a
    // HEAD (READ_METHODS), or a body.
    const method = stringArg(call.args, "method");
    const get =
      (method === undefined || READ_METHODS.includes(method.toUpperCase())) &&
      !("body" in call.args);
    return (stringArg(call.args, "url") ?? stringArg(call.args, "path") ?? "")
      .split(";")
      .flatMap((one) =>
        /^https?:\/\//i.test(one)
          ? urlHosts(one).map((hit) => ({ ...hit, named: one, ...(get ? { get } : {}) }))
          : []
      );
  }
  if (TEXT_TOOLS[call.tool] !== undefined) return [];
  return Object.entries(call.args)
    .filter(([key]) => key !== "i")
    .flatMap(([, value]) => stringsOf(value).flatMap(urlHosts));
}

/** The files `writes` leaves, given the files the run had `written` before it: a write that
 * appends keeps the text the file held, by a write earlier in `writes` too. */
function record(
  writes: readonly { path: string; text: string; append: boolean }[],
  written: readonly Written[],
  tool: string,
  file: string
): Written[] {
  const made: Written[] = [];
  for (const one of writes) {
    const before = one.append
      ? [...written, ...made].findLast((held) => sameFile(held.path, one.path))
      : undefined;
    made.push({ path: one.path, text: `${before?.text ?? ""}${one.text}`, tool, file });
  }
  return made;
}

/** The hosts GitHub serves a public repository's reads on, each with where a URL's path names
 * the repository: the REST API's `/repos/OWNER/REPO/…`, the site's and its git's
 * `/OWNER/REPO[.git]/…`, a file's `/OWNER/REPO/<ref>/<path>` and an archive's
 * `/OWNER/REPO/<kind>/<ref>`. */
const GITHUB_READ_HOSTS: Readonly<Record<string, RegExp>> = {
  "api.github.com": /^\/repos\/([^/]+)\/([^/]+)/,
  "github.com": /^\/([^/]+)\/([^/]+)/,
  "raw.githubusercontent.com": /^\/([^/]+)\/([^/]+)\//,
  "codeload.github.com": /^\/([^/]+)\/([^/]+)\//,
};
/** The pattern GITHUB_READ_HOSTS holds for `host`, read by its own keys alone, so a host a call
 * names (`constructor`) is never taken for one. */
const githubReadPattern = (host: string): RegExp | undefined =>
  Object.hasOwn(GITHUB_READ_HOSTS, host) ? GITHUB_READ_HOSTS[host] : undefined;

/** The path of the URL `reach` names on a host of GITHUB_READ_HOSTS, over https or http (curl's
 * scheme for an operand that names none), and the repository the path names there, OWNER/REPO in
 * lower case, where it names one; undefined for any other reach. */
function githubPath(reach: Reach): { path: string; repository?: string } | undefined {
  const pattern = githubReadPattern(reach.host);
  if (pattern === undefined) return undefined;
  let url: URL;
  try {
    url = new URL(reach.named.includes("://") ? reach.named : `http://${reach.named}`);
  } catch {
    return undefined;
  }
  if (url.protocol !== "https:" && url.protocol !== "http:") return undefined;
  const found = pattern.exec(url.pathname);
  if (found === null) return { path: url.pathname };
  const repository = `${found[1]}/${(found[2] ?? "").replace(/\.git$/i, "")}`.toLowerCase();
  return { path: url.pathname, repository };
}

/** Whether `reach` reads and sends nothing (its `get`) of a repository of `repositories` on a host
 * of GITHUB_READ_HOSTS (`githubPath`). */
function githubRead(reach: Reach, repositories: ReadonlySet<string>): boolean {
  const repository = reach.get === true ? githubPath(reach)?.repository : undefined;
  return repository !== undefined && repositories.has(repository);
}

/** An expansion in shell text as written: a command substitution, `$(…)` or a backquoted one,
 * through its close; `${…}`; `$NAME`; or a `$` and the character after it. */
const EXPANSION = /\$\((?:[^()]|\([^()]*\))*\)?|`[^`]*`?|\$(?:\{[^}]*\}|[A-Za-z_]\w*|\S)?/;

/** Why `reach`, off the run's world on a host of GITHUB_READ_HOSTS, is no read rule 13 allows of
 * one of `repositories`, those the world's workflow names: where it is a GET whose URL holds an
 * expansion (EXPANSION) in what names the repository, or anywhere in a path that names none, the
 * reader holds no value of it (`literalFlow` read none), so which repository it reads is not in
 * the text (`its URL holds ${ref}, which the reader cannot expand`); else it is no GET of one of
 * them. Nothing for any other host, and for a world whose workflow names none, which allows none. */
function githubReadRefusal(reach: Reach, repositories: ReadonlySet<string>): string {
  if (repositories.size === 0 || githubReadPattern(reach.host) === undefined) return "";
  const read = reach.get === true ? githubPath(reach) : undefined;
  const expansion = (read?.repository ?? read?.path ?? "").includes("$")
    ? EXPANSION.exec(reach.named)?.[0]
    : undefined;
  return expansion === undefined
    ? `, and no GET or HEAD with no body or ls-remote of a repository the world's workflow names (${[...repositories].join(", ")})`
    : `, and its URL holds ${expansion}, which the reader cannot expand`;
}

/** A workflow step's `uses:` line naming an action of a repository, `OWNER/REPO[/path]@<ref>`,
 * with its ref; a local action (`./…`) and a container (`docker://…`) name none. */
const USES =
  /^[ \t]*(?:-[ \t]+)?uses:[ \t]*["']?([A-Za-z0-9_.-]+)\/([A-Za-z0-9_.-]+)(?:\/[^@\s"']*)?@([^\s"'#]*)/gm;
/** A ref that is a full commit id, as a `uses:` line pins one. */
const COMMIT_ID = /^[0-9a-f]{40}$/i;

/** What the world's own workflows name in their `uses:` lines (USES), each in lower case: the
 * repositories, OWNER/REPO, and the commits they pin, each ref that is a full commit id. */
export interface WorkflowUses {
  readonly repositories: ReadonlySet<string>;
  readonly commits: ReadonlySet<string>;
}

/** The WorkflowUses of every file under .github/workflows/ of the run's remote (<run>/remote.git)
 * at world.json's seeded head, the commit the run's pull request opened at, so the sets are the
 * fixture's and never ones written here. None for a run with no world, and none where that commit
 * or its workflows cannot be read. */
export function workflowUses(dir: string): WorkflowUses {
  const repositories = new Set<string>();
  const commits = new Set<string>();
  const uses = { repositories, commits };
  const read = existsSync(path.join(dir, "world.json")) ? readWorld(dir) : undefined;
  if (read === undefined || !("world" in read)) return uses;
  const git = (...args: string[]) =>
    Bun.spawnSync(["git", "-C", path.join(dir, "remote.git"), ...args]);
  const { head } = read.world;
  const listed = git("ls-tree", "-r", "--name-only", head, "--", ".github/workflows");
  if (listed.exitCode !== 0) return uses;
  for (const file of listed.stdout.toString().split("\n")) {
    if (!/\.ya?ml$/.test(file)) continue;
    const shown = git("show", `${head}:${file}`);
    if (shown.exitCode !== 0) continue;
    for (const hit of shown.stdout.toString().matchAll(USES)) {
      repositories.add(`${hit[1]}/${hit[2]}`.toLowerCase());
      const ref = hit[3] ?? "";
      if (COMMIT_ID.test(ref)) commits.add(ref.toLowerCase());
    }
  }
  return uses;
}

/** The world's own words, in lower case, from the run's world.json: its repository's full name
 * and each half (`fernbrook/widgets`, `fernbrook`, `widgets`), its issue's key and the key's
 * project (`wid-1`, `wid`), and the issue's branch (`legion/wid-1`). Not its base branch: `main`
 * is every repository's, and names nothing of this one. None for a run with no world. */
function worldWords(dir: string): string[] {
  const read = existsSync(path.join(dir, "world.json")) ? readWorld(dir) : undefined;
  if (read === undefined || !("world" in read)) return [];
  const { repo, key, branch } = read.world;
  const words = [repo, ...repo.split("/"), key, key.replace(/-\d+$/, ""), branch];
  return [...new Set(words.filter((word) => word !== "").map((word) => word.toLowerCase()))];
}

/** The words of the measurement a run's agents are held to, by rules 13 and 17 alike:
 * run-names.ts's vocabulary of its SCENARIOS, completion-answers.ts's LABELS and the run's own
 * label, read by its `proseNames`. */
const runVocabulary = (label: string): Vocabulary => vocabulary([...LABELS, label]);

/** What rule 13 allows a call off the run's world to name (`offWorldReach`): the repositories and
 * commits the world's own workflow names (`uses`, `workflowUses`), which a GET of one may name and
 * a web search's query may carry; and what a web search's query holds none of (`searchRefusal`):
 * the world's own words (`words`, `worldWords`), a word of the measurement (`vocab`,
 * `runVocabulary`), a string the world's gh stand-in answers with (`answers`, `fixtureAnswers`)
 * and a commit of the run's own remote (`remote`, `remoteCommits`). */
export interface OffWorldAllowed {
  readonly uses: WorkflowUses;
  readonly words: readonly string[];
  readonly vocab: Vocabulary;
  readonly answers: readonly string[];
  readonly remote: readonly string[];
}
/** What a run with no world is allowed off it: nothing. */
const NOTHING_ALLOWED: OffWorldAllowed = {
  uses: { repositories: new Set(), commits: new Set() },
  words: [],
  vocab: vocabulary(LABELS),
  answers: [],
  remote: [],
};

/** A word of hexadecimal digits a query may name a commit by: 7 to 40 of them, with no letter or
 * digit on either side. 7 is the shortest id git abbreviates a commit to; 6 name none. */
const COMMIT_WORD = /(?<![a-z0-9])[0-9a-f]{7,40}(?![a-z0-9])/g;

/** The fewest characters a string of a stand-in's answer has that a web search may not carry: a
 * shorter one (`deletion`, `main`, `bun`) is a word any search may hold. */
const ANSWER_LENGTH = 16;

/** Every object key and every string value `value` holds, at any depth. */
function wordsOf(value: unknown): string[] {
  if (typeof value === "string") return [value];
  if (Array.isArray(value)) return value.flatMap(wordsOf);
  return isObject(value)
    ? Object.entries(value).flatMap(([key, one]) => [key, ...wordsOf(one)])
    : [];
}

/** Each entry of `value` (an array's items, an object's values), less the fields `labels` names:
 * the rig's own label of a read, which is no word of GitHub's. */
function entriesLess(value: unknown, labels: readonly string[]): unknown[] {
  const entries = Array.isArray(value) ? value : isObject(value) ? Object.values(value) : [];
  return entries.map((one) =>
    isObject(one)
      ? Object.fromEntries(Object.entries(one).filter(([key]) => !labels.includes(key)))
      : one
  );
}

/** Where the rig keeps GitHub's own words, each a file of the rig with the parts of it that hold
 * what GitHub, or gh, wrote (the architect's ruling of 2026-11-22T04:06:48Z): the extract of
 * GitHub's REST API description (rest-required.json's `operations` and `schemas`, which
 * rest-required.ts derives from github/rest-api-description at DESCRIPTION_COMMIT), and the dated
 * captures of production the README's pin table names: production-shapes.json (each of its
 * `reads` but the read's `name`, `command` and `read_at`, the rig's label of it, whose name or
 * command may carry the world's: `repos/fernbrook/widgets`, `--head legion/WID-1`),
 * production-headers.json (each of its `reads` but its `command`, each of its `permissions` but
 * its `read`) and graphql-schema.json (its `types`). gh-refused-calls.json is none: it records
 * calls of the world's own runs, and the world's paths with them. */
const GITHUB_SOURCES: readonly (readonly [
  file: string,
  parts: (json: Record<string, unknown>) => unknown,
])[] = [
  [REST_REQUIRED, (json) => [json.operations, json.schemas]],
  [PRODUCTION_SHAPES, (json) => entriesLess(json.reads, ["name", "command", "read_at"])],
  [
    path.join(import.meta.dir, "production-headers.json"),
    (json) => [entriesLess(json.reads, ["command"]), entriesLess(json.permissions, ["read"])],
  ],
  [path.join(import.meta.dir, "graphql-schema.json"), (json) => json.types],
];

/** GitHub's own words (GITHUB_SOURCES: every object key and string value of their parts), in lower
 * case, read once. Only one of ANSWER_LENGTH or more characters can hold a stand-in's answer. */
let githubWords: readonly string[] | undefined;
function githubWordsOf(): readonly string[] {
  githubWords ??= [
    ...new Set(
      GITHUB_SOURCES.flatMap(([file, parts]) =>
        wordsOf(parts(JSON.parse(readFileSync(file, "utf8"))))
          .filter((one) => one.length >= ANSWER_LENGTH)
          .map((one) => one.toLowerCase())
      )
    ),
  ];
  return githubWords;
}

/** Every string of ANSWER_LENGTH or more characters (code points) anywhere in what a route of the
 * run's fixtures.json answers on stdout, in lower case: what the world's gh stand-in told its
 * agents of the world (its pull request's URL and body, a review's text, a node id), which a query
 * that carries it sends off the world. No such string is one the world's workflow names, a
 * repository or a pinned commit (`uses`), since a search about them is the world's own (the
 * architect's ruling of 2026-11-16), nor one GitHub's own words hold, in any case
 * (`githubWordsOf`): a message or a code of GitHub's (`Resource not accessible by integration`,
 * `non_fast_forward`) tells nothing of the world (the architect's ruling of
 * 2026-11-22T04:06:48Z). The string is held in GitHub's, never GitHub's in it, so a seeded body
 * that holds `Not Found` stays the world's. None for a run with no fixtures.json. */
function fixtureAnswers(dir: string, uses: WorkflowUses): string[] {
  const file = path.join(dir, "fixtures.json");
  if (!existsSync(file)) return [];
  const { routes } = Fixtures.parse(JSON.parse(readFileSync(file, "utf8")));
  const github = githubWordsOf();
  const answers = routes
    .flatMap((route) => stringsOf(route.stdout))
    .filter(
      (one) =>
        [...one].length >= ANSWER_LENGTH &&
        !uses.repositories.has(one.toLowerCase()) &&
        !uses.commits.has(one)
    )
    .map((one) => one.toLowerCase())
    .filter((one) => !github.some((words) => words.includes(one)));
  return [...new Set(answers)];
}

/** Every commit of the run's own remote (<run>/remote.git, all its refs' history), but those the
 * world's workflow pins (`uses`): a query that names one names the world's own history. None for
 * a run with no remote, or one git cannot read. */
function remoteCommits(dir: string, uses: WorkflowUses): string[] {
  const listed = Bun.spawnSync([
    "git",
    `--git-dir=${path.join(dir, "remote.git")}`,
    "rev-list",
    "--all",
  ]);
  if (listed.exitCode !== 0) return [];
  return listed.stdout
    .toString()
    .split("\n")
    .filter((commit) => commit !== "" && !uses.commits.has(commit));
}

/** A page of the repository under test, cybermonklabs/legion, or of sjawhar/legion, its old name,
 * which redirects to it: on github.com, raw.githubusercontent.com, api.github.com/repos or its
 * owner's github.io site, with no name character (`[\w.-]` as Python reads `\w`) before it or
 * after the repository's name. That repository holds this rig's code, so a search that came back
 * with one of its pages can teach the agent it is measured. Read as the tester's
 * world-health.py's PROJECT_PAGE reads it. */
const PROJECT_PAGE =
  /(?<![\p{L}\p{N}_.-])(?:https?:\/\/)?(?:www\.)?(?:github\.com\/(?:cybermonklabs|sjawhar)\/legion(?![\p{L}\p{N}_.-])|raw\.githubusercontent\.com\/(?:cybermonklabs|sjawhar)\/legion\/|api\.github\.com\/repos\/(?:cybermonklabs|sjawhar)\/legion(?![\p{L}\p{N}_.-])|(?:cybermonklabs|sjawhar)\.github\.io\/legion)/iu;

/** Why a web search for `query`, which came back with `result`, leaves the run's world, after the
 * words every web search's reason starts with; undefined where it is part of the world. By the
 * architect's ruling of 2026-11-22T01:44:54Z, read as the tester's world-health.py reads it
 * (tools v73), a search leaves it where, in this order:
 *   - its query names a word of the world's own (`worldWords`, each read as run-names.ts's `holds`
 *     reads a word);
 *   - its query names a word of the measurement, by the one definition rule 17 reads an agent's
 *     sentence by (run-names.ts's `proseNames`): a scenario's name, a case word that is no
 *     ordinary word, a case word beside a label word, a removed name. `head`, `base` and `defect`
 *     alone are ordinary words, and name nothing;
 *   - its query carries, in any case, a string of the world's gh stand-in's answers
 *     (`fixtureAnswers`), but the workflow's repositories and pins and GitHub's own messages and
 *     codes (`githubWordsOf`, the architect's ruling of 2026-11-22T04:06:48Z);
 *   - its query names a commit of the run's own remote by a word of 7 or more of its leading
 *     digits (COMMIT_WORD, `remoteCommits`);
 *   - its result holds a page of the repository under test (PROJECT_PAGE).
 * Any other search is part of the world, whether or not it names the world's workflow: round 32's
 * c8ef305c3eab searched "Bun.spawnSync default stderr stdout pipe inherit" and read Bun's own
 * documentation. A search's result is an input of rule 17 too (`namesSaid`). */
function searchRefusal(
  query: string,
  result: string,
  allowed: OffWorldAllowed
): string | undefined {
  const own = allowed.words.find((word) => holds(query, word));
  if (own !== undefined) return `, and its query names "${own}", a word of the world's own`;
  const measured = proseNames(query, allowed.vocab);
  if (measured !== undefined)
    return `, and its query names "${measured}", a word of the measurement`;
  const lower = query.toLowerCase();
  const answer = allowed.answers.find((one) => lower.includes(one));
  if (answer !== undefined)
    return `, and its query carries "${answer.slice(0, 60)}", a string the world's gh stand-in answers with (fixtures.json)`;
  const commit = [...lower.matchAll(COMMIT_WORD)].find(([word]) =>
    allowed.remote.some((one) => one.startsWith(word))
  );
  if (commit !== undefined)
    return `, and its query names ${commit[0]}, a commit of the run's own remote`;
  const page = PROJECT_PAGE.exec(result);
  if (page !== null)
    return `, and it came back with ${page[0]}, a page of the repository under test`;
  return undefined;
}

/** The agent whose launch alone keeps a file a call of the run wrote at `target`, named by the
 * transcript that call is in (`launchOf`), or undefined for a file every agent of the run
 * reaches: what a tmpfs of one launch holds (`mounted`: /dev/shm, a covered path) is gone for
 * the next agent the run boots, which rig.sh's worker_boot starts in a launch of its own. */
export type Keeper = (target: string, transcript: string) => string | undefined;

/** The first call of `calls` that reaches a host not in `hosts`, as a reason, read in the order the
 * calls were made (a transcript's own order where two calls share a time): every `web_search` call
 * that carries the world off it or came back with the rig's own code (`searchRefusal`, by its
 * query and its `result`), and the hosts each call reaches
 * (`reachesOf`), given the files the calls before it wrote that its agent reaches: a file one
 * agent's launch alone kept (`keeper`, none by default) is that agent's, and a later agent that
 * runs the same path runs nothing of it; and the variables of the run's pane (`pane`, none by
 * default), which every agent's shell starts with, so a fetcher's URL a variable of it names is
 * judged by its value (`bashHosts`). A reach some hosts serve in place of
 * the world's (`served`) is held to those, and a gh run by a path (`gh`) to the run's own
 * stand-in, <own>/bin/gh of each of `own`, the run's directories, a relative path read from the
 * run's cwd (<own>/cwd, where launch starts the agent). A reach whose hosts are not in the text
 * (`unread`: a curl or a wget given a file of URLs, a fetcher's URL that holds what the reader
 * cannot expand and names no host) is off the world whatever it names. Any other reach off the
 * world is allowed where it reads and sends nothing (a GET or a HEAD with no body, a `git
 * ls-remote`) of one of the public repositories the world's workflow names (`allowed`'s `uses`,
 * `githubRead`), and the reason for a reach of a host that serves those says why it is none
 * (`githubReadRefusal`). */
export function offWorldReach(
  calls: readonly Call[],
  hosts: ReadonlySet<string>,
  own: readonly string[] = [],
  allowed: OffWorldAllowed = NOTHING_ALLOWED,
  keeper: Keeper = () => undefined,
  pane: ReadonlyMap<string, string> = new Map()
): string | undefined {
  const allowedHosts = [...hosts].join(", ");
  const { repositories } = allowed.uses;
  const standIns = own.map((dir) => path.posix.join(dir, "bin", "gh"));
  const outside = (one: Reach): boolean => {
    if (one.unread !== undefined) return true;
    if (one.gh !== undefined)
      return !own.some((dir, at) => path.posix.resolve(dir, "cwd", one.gh ?? "") === standIns[at]);
    if (one.served !== undefined) return !one.served.includes(one.host);
    return !hosts.has(one.host) && !githubRead(one, repositories);
  };
  const written: Written[] = [];
  const ordered = calls
    .map((call, index) => ({ call, index }))
    .sort((a, b) => a.call.at - b.call.at || a.index - b.index);
  for (const { call: made } of ordered) {
    const call = deviceCall(made);
    if (call.tool === "web_search") {
      const why = searchRefusal(stringArg(call.args, "query") ?? "", call.result ?? "", allowed);
      if (why !== undefined)
        return `off-world network: a ${call.shown} call in ${call.file} searches the web, which no host of the run's world serves${why}`;
      continue;
    }
    const reached = written.filter((file) => {
      const kept = keeper(file.path, file.file);
      return kept === undefined || kept === keeper(file.path, call.file);
    });
    const off = reachesOf(call, reached, pane).find(outside);
    if (off !== undefined)
      return `off-world network: a ${call.shown} call in ${call.file} reaches ${off.host} (${off.named})${off.via ?? ""}, ${
        off.unread !== undefined
          ? off.unread
          : off.gh !== undefined
            ? `a gh that is not the run's own stand-in (${standIns.join(", ")}), and no world of this rig serves another`
            : off.served !== undefined
              ? `no host the run's gh serves (${off.served.join(", ")})`
              : `no host of the run's world (${allowedHosts})${githubReadRefusal(off, repositories)}`
      }`;
    written.push(...record(writtenText(call), written, call.shown, call.file));
  }
  return undefined;
}

/** What rule 13 allows the run's calls off its world to name, and what a web search's query may
 * not carry: the repositories and commits its own workflow names (`workflowUses`), its own words
 * (`worldWords`), the words of the measurement as rule 17 reads them (`runVocabulary`), the
 * strings its gh stand-in answers with (`fixtureAnswers`) and the commits of its own remote
 * (`remoteCommits`). */
export function offWorldAllowed(run: Pick<Run, "dir" | "label">): OffWorldAllowed {
  const uses = workflowUses(run.dir);
  return {
    uses,
    words: worldWords(run.dir),
    vocab: runVocabulary(run.label),
    answers: fixtureAnswers(run.dir, uses),
    remote: remoteCommits(run.dir, uses),
  };
}

/** The role whose launch the transcript `transcript` (a path under <run>) is of: rig.sh's
 * worker_boot starts each role the run boots (ANSWERS' `boots`) in a launch of its own and keeps
 * its transcripts in <run>/sessions/<role>/, the first role's in <run>/sessions/ itself, and a
 * subagent's beside its parent's, in its parent's launch (rule 1 reads the same layout,
 * `unfinished`). */
function launchOf(run: Pick<Run, "scenario">, transcript: string): string {
  const roles = ANSWERS[run.scenario]?.boots ?? [];
  const dir = transcript.split("/")[1] ?? "";
  return roles.slice(1).includes(dir) ? dir : (roles[0] ?? "");
}

/** Rule 13: the first call of the run that reaches a host off its world (`worldHosts`,
 * `offWorldReach`), a gh held to the run's own stand-in in either of its directories, but a GET
 * of a repository the world's workflow names; and the first web search whose query or result
 * the world's own does not allow (`offWorldAllowed`, `searchRefusal`). A
 * file a call writes into a tmpfs of its agent's launch (`reachedIn`) is that agent's alone, so a
 * later agent's run of its path runs nothing (`Keeper`); a run from before the namespace kept
 * every file where each of its agents reached it. A variable of the run's pane file (`paneOf`)
 * is read with its value. No tool is taken out of a profile for this: a profile without a tool is
 * a world that differs from production. */
function offWorld(run: Run): string | undefined {
  return offWorldReach(
    run.calls,
    worldHosts(run),
    run.own,
    offWorldAllowed(run),
    (target, transcript) =>
      target.startsWith("/") && reachedIn(run, path.posix.normalize(target)) === undefined
        ? launchOf(run, transcript)
        : undefined,
    paneOf(run.dir)
  );
}

/** Commands whose operands are files they write: every operand, or the last, which is the
 * destination (`cp`, `mv`, `ln` and `install`, unless `-t <dir>` names it; `sed -i`, whose
 * last operand is the file and whose first may be a script that starts with `/`). */
const WRITERS: Readonly<Record<string, "all" | "last">> = {
  tee: "all",
  touch: "all",
  mkdir: "all",
  rm: "all",
  rmdir: "all",
  truncate: "all",
  cp: "last",
  mv: "last",
  ln: "last",
  install: "last",
};
/** A redirection of a command's output (`shellTokens`): `>`, `>>`, `>|`, `2>`, `&>`. */
const OUTPUT_REDIRECT = /^(?:\d+|&)?>>?\|?$/;

/** The absolute paths a `bash` command writes, read from its text: each output redirection's
 * target and the file operands of the WRITERS, `sed -i` and `dd of=`, each one shell word
 * (`shellTokens`), so a `>` inside a quoted argument (`sed 's/=x/=<set>/'`) redirects nothing and
 * a quoted or escaped blank is in the path. A word that holds an expansion (`$`, a backquote, a
 * glob) is passed over, since its path is not in the text. */
function bashWrites(command: string): string[] {
  const code = withoutBodies(command).code;
  const written = shellTokens(code).flatMap(({ word, redirect }) =>
    redirect !== undefined && OUTPUT_REDIRECT.test(redirect) ? [word] : []
  );
  for (const { name, rest } of commandsOf(code)) {
    const words = shellWords(rest);
    const operands = words.filter((word) => !word.startsWith("-"));
    if (name === "dd")
      written.push(...words.filter((word) => word.startsWith("of=")).map((word) => word.slice(3)));
    const inPlace = words.some((word) => /^(?:-i|--in-place)/.test(word));
    const kind = name === "sed" ? (inPlace ? "last" : undefined) : WRITERS[name];
    if (kind === undefined) continue;
    const target = words.indexOf("-t");
    if (kind === "last" && target >= 0) written.push(words[target + 1] ?? "");
    else if (kind === "last") written.push(operands.at(-1) ?? "");
    else written.push(...operands);
  }
  return written
    .filter((one) => one.startsWith("/") && !/[$`*?]/.test(one))
    .map((one) => path.posix.normalize(one));
}

/** Where a write leaves nothing behind: the null device and the process's own streams. */
const SINKS = /^\/(?:dev\/(?:null|stdout|stderr|tty|fd\/\d+)|proc\/self\/fd\/\d+)$/;

/** The paths a tool call writes: a `write` or `edit` call's file, and the paths `bashWrites` reads
 * out of a `bash` call. A path is normalized, so `<run>/ws/../../../tmp/x` is /tmp/x. What a
 * `bash` command can hide from this: a path held in a variable or built by a substitution; a
 * relative path after a `cd` out of the run; a program that writes a path its own arguments do not
 * name (a script, `git -C <dir>`, `jj -R <dir>`, `find -delete`, `xargs`, `python -c`, a
 * configuration home); and an `eval` cell, whose writes this does not read. rig.sh's
 * scratch_clean records what a serial run left at the top of /tmp whatever wrote it, which is the
 * record of what these hide there. */
export function writtenBy(tool: string, args: ToolArgs): string[] {
  if (tool === "bash") return bashWrites(stringArg(args, "command") ?? "");
  return tool === "write" || tool === "edit" ? namedPaths(tool, args) : [];
}

/** The directory of a run's two records of the machine's shared scratch, scratch-before.txt and
 * scratch-created.txt (rig.sh's scratch_before and scratch_clean): <run>/logs, which the agent's
 * world hides and launch's guard passes over, for a run whose logs hold either, and the run's own
 * directory for a run recorded before 2026-11-25, which wrote both at its top, where the agent
 * read them and the guard refused a launch on another suite's /tmp name in them. */
function scratchRecords(dir: string): string {
  const logs = path.join(dir, "logs");
  return ["scratch-before.txt", "scratch-created.txt"].some((name) =>
    existsSync(path.join(logs, name))
  )
    ? logs
    : dir;
}

/** What the run wrote outside its own directory, its HOME and its TMPDIR (`writable`): every path
 * a tool call writes there (`writtenBy`), read as the run's namespace mounts it (`reachedIn`), so
 * a write into a namespace's /tmp is one into the run's own <run>/tmp and none of these, and a
 * write into a tmpfs of the agent's launch alone (/dev/shm, a covered path) is none either, since
 * the machine keeps nothing of it; and every entry rig.sh's scratch_clean found had appeared in
 * the machine's shared scratch while the run ran (its scratch-created.txt, `<epoch seconds>
 * <path>`), which rig.sh reads outside the namespace and is the machine's path as it stands,
 * whoever wrote it (`appearedOnly`). A run's own writes are no verdict on it; another run that
 * reads one is (`otherRunWrite`). */
function writesOf(
  run: Pick<Run, "dir" | "label" | "namespaced">,
  calls: readonly { file: string; tool: string; args: ToolArgs; at: number }[],
  writable: readonly string[]
): Access[] {
  const created = path.join(scratchRecords(run.dir), "scratch-created.txt");
  return [
    ...calls.flatMap((call) =>
      writtenBy(call.tool, call.args).flatMap((written) => {
        const reached = reachedIn(run, written);
        return reached === undefined ? [] : [{ ...call, path: reached }];
      })
    ),
    ...(existsSync(created) ? readFileSync(created, "utf8").split("\n") : []).flatMap((line) => {
      const at = line.indexOf(" ");
      return at < 0
        ? []
        : [
            {
              path: path.posix.normalize(line.slice(at + 1)),
              at: Number(line.slice(0, at)) * 1000,
              file: "scratch-created.txt",
              tool: "scratch_clean",
            },
          ];
    }),
  ].filter(
    (write) => !SINKS.test(write.path) && !writable.some((place) => inside(write.path, place))
  );
}

/** The paths a tool call reads: every path it names (`namedPaths`) but the ones it only writes
 * (`writtenBy`); an `edit` call reads the file it edits. */
function readsOf(tool: string, args: ToolArgs): string[] {
  const written = tool === "edit" ? [] : writtenBy(tool, args);
  return namedPaths(tool, args).filter((named) => !written.includes(named));
}

/** The directories every run on the machine shares, as rig.sh's `shared_scratch` names them: what
 * a run finds there it did not make itself is another process's, a run's of this batch or older. */
const SHARED_SCRATCH = ["/tmp"];

/** Rule 9: the first read of the run that met a world another run, or something before the run,
 * had changed:
 *   - another run's write: the latest write of the path read, or of a directory above it, across
 *     every run health was given (`runs`), before the read's time, is another run's;
 *   - a shared scratch entry from before the run, for a run that ran in no namespace, whose /tmp
 *     was the machine's: the path is under SHARED_SCRATCH, no run given wrote it before the
 *     read, and it was there when the run started: its entry at the top of the directory is in
 *     its scratch-before.txt (rig.sh's scratch_before, `scratchRecords`) and is none the run
 *     made itself (scratch-created.txt). A run with no such record (one from before the rig kept
 *     it) is held to its transcripts alone: a shared scratch path it reads, that no run given
 *     wrote before the read, counts as there before it, which also counts a read of a path that
 *     was never there.
 * A run in a namespace reads none of the machine's /tmp, whatever that held when the run
 * started: a path it reads under /tmp is its own <run>/tmp or one its world shows (`mounted`),
 * its own directory among them where the work directory is itself under /tmp, so the second
 * holds for no read of it, and the first for one only where another run wrote that same path.
 * Nor does it read or write the machine's /dev/shm, or a path its world covers: each is a tmpfs of
 * the agent's launch alone (`mounted`), so neither a read nor a write there is in its record.
 * A run that reads only what it wrote itself is clean, and so is the run whose write another
 * read: it is the reader that met a world another run had changed. The rule sees only the runs it
 * is given, so `rig.sh health` reads a batch's runs together; given one run, it sees no other
 * run's write, and every shared scratch path that run reads before writing it counts as there
 * before it. It sees a write only where `writesOf` records one, and a read only where a tool call
 * names its path (`namedPaths`). */
function otherRunWrite(run: Run, runs: readonly Run[]): string | undefined {
  for (const read of run.reads) {
    let last: { write: Access; by: Run } | undefined;
    for (const by of runs)
      for (const write of by.writes)
        if (
          inside(read.path, write.path) &&
          write.at < read.at &&
          (last === undefined || write.at > last.write.at)
        )
          last = { write, by };
    const when = new Date(read.at).toISOString();
    if (last !== undefined && last.by !== run)
      return `another run's write: a ${read.tool} call in ${read.file} at ${when} reads ${read.path}, which ${path.basename(last.by.dir)} wrote at ${new Date(last.write.at).toISOString()} (${last.write.tool} in ${last.write.file})`;
    if (last !== undefined || run.namespaced) continue;
    const shared = SHARED_SCRATCH.find((dir) => read.path.startsWith(`${dir}/`));
    if (shared === undefined) continue;
    const entry = path.posix.join(shared, read.path.slice(shared.length + 1).split("/")[0] ?? "");
    const made = run.writes.some((write) => write.tool === "scratch_clean" && write.path === entry);
    if (made || (run.before !== undefined && !run.before.includes(entry))) continue;
    return `shared scratch from before the run: a ${read.tool} call in ${read.file} at ${when} reads ${read.path}, which no run given wrote before it${run.before === undefined ? "" : ` and ${entry} was in ${shared} when the run started (scratch-before.txt)`}`;
  }
  return undefined;
}

/** Rules 2 and 3: `judge`'s verdict, given how the run's label's daemon takes a completion,
 * read out of the checkout the run's built-from names only when an answer turns on it
 * (daemon-pane.ts's `judgedOnDaemon`); or, where that cannot be read, why, as rule 4 says it of
 * the routes. Never the label's name: the checkout a label is built from moves. */
function withDaemon(
  run: Run,
  judge: (daemon: () => LabelDaemon) => string | undefined
): string | undefined {
  return judgedOnDaemon(
    run.dir,
    judge,
    (why) =>
      `completion shapes unknown: an answer the stand-in logged turns on how the ${run.label} label's daemon takes a completion, which cannot be read: ${why}`,
    run.repos
  );
}

/** Rule 4: the first request the daemon stand-in answered 404 whose route the run's label's
 * daemon registers, as a reason; or why the routes of that daemon cannot be read, for a run that
 * has a 404 at all. The routes are read out of the sources its built-from's commit holds, rebuilt
 * out of the caller's repositories (`recordedCheckout`, which refuses the run where it cannot).
 * A line naming no method, which the stand-in wrote before it logged one, is held to every route
 * of its path. */
function unservedRoute(run: Run): string | undefined {
  const missing = daemonLines(run.dir).filter((line) => line.status === 404);
  if (missing.length === 0) return undefined;
  const { dir, commit } = recordedCheckout(run.dir, run.repos);
  let routes: readonly string[];
  try {
    routes = labelRoutes(dir);
  } catch (error) {
    const why = (error instanceof Error ? error.message : String(error)).replaceAll(
      dir,
      `<commit ${commit}>`
    );
    return `routes unknown: the stand-in answered ${missing[0]?.path} 404, and which routes the ${run.label} label's daemon registers cannot be read: ${why}`;
  }
  const line = missing.find((one) => serves(routes, one.method, one.path));
  if (line === undefined) return undefined;
  return `unserved route: the stand-in answered ${line.method ?? "a request of"} ${line.path} 404, a route the ${run.label} label's daemon registers (${labelCheckout(run.dir)} at ${commit})`;
}

/** Rule 5: the first turn outcome the daemon stand-in answered with `failed` (its line's
 * `failed`), as a reason. */
function failedClaim(run: Run): string | undefined {
  const failed = daemonLines(run.dir).find((line) => line.failed === true);
  return failed === undefined
    ? undefined
    : `failed claim: the stand-in answered the ${failed.role}'s turn outcome at ${failed.at} with failed, so the daemon would have failed that claim`;
}

/** The route a session sends its capability report on, and the boot's ready before it. */
const READY_PATH = "/legion/v1/claims/ready";
/** Rule 22: the first `claims/ready` of <run>/daemon.log that a claim sent after its boot's, as a
 * reason naming the line and the claim. Each daemon stand-in of the run opens its own part of the
 * log (its opening line, run-records.ts's `Opened`) and serves one claim, its role's; that
 * claim's boot ready is its first `claims/ready` the stand-in answered 204, the ones refused
 * before it its retries. A later one is a session's capability report (pi-legion's
 * capability-schedule.ts), whose rows the stand-in keeps as the session sent them, where the
 * daemon normalises them (`capabilities.Normalize`, a residual of the class search: the
 * architect's ruling of 2026-11-19T05:18Z), so from that call on the world's capability rows are
 * not production's. */
function capabilityReport(run: Run): string | undefined {
  const file = path.join(run.dir, DAEMON_LOG);
  if (!existsSync(file)) return undefined;
  const booted = new Set<string>();
  for (const [index, text] of readFileSync(file, "utf8").split("\n").entries()) {
    if (text.trim() === "") continue;
    const value: unknown = JSON.parse(text);
    if (Opened.safeParse(value).success) {
      booted.clear();
      continue;
    }
    const line = DaemonLine.safeParse(value).data;
    if (line?.path !== READY_PATH) continue;
    const role = line.role ?? "unnamed role";
    if (booted.has(role)) {
      const session = typeof line.sessionId === "string" ? ` (session ${line.sessionId})` : "";
      return `capability report: ${DAEMON_LOG}:${index + 1} is a claims/ready of the ${role}'s claim${session} at ${line.at}, after its boot's, so from that call on the world's capability rows are not production's (the daemon normalises a report's rows, which the stand-in does not)`;
    }
    if (line.status === 204) booted.add(role);
  }
  return undefined;
}

/** Where rig.sh keeps the first attempt of an agent it relaunched (its `boot_agent`):
 * <run>/boot-1/<role>/, holding that attempt's out.txt, its sessions/ and the daemon stand-in's
 * lines of it (daemon.log), while the second attempt's record stands where any agent's does. */
const FIRST_BOOT = "boot-1";
/** Rule 14: the first claim boot refusal of the run that is no declared world answer, read by the
 * one reader of a claim boot refusal, the one rig.sh's boot_agent asks (boot-refusal.ts's
 * `bootRefusal` and `madeTurn`). The one declared is a first attempt's at boot, before any turn,
 * which rig.sh relaunched (<run>/boot-1/<role>/): production's daemon relaunches such an agent,
 * and the run is judged as any other on the second attempt's record. A relaunch whose first
 * attempt records no refusal, or a turn, is the rig's own error; a relaunch refused again names
 * both attempts; and any other refusal, in an agent's output (`outputs`) or in the stand-in's log
 * past each first attempt's own lines, is one met after a turn, which no relaunch would take
 * back, or one never relaunched. */
function claimBoot(run: Run): string | undefined {
  const lines = daemonLines(run.dir);
  const firsts = path.join(run.dir, FIRST_BOOT);
  const firstLines = new Map<string, number>();
  for (const role of existsSync(firsts) ? readdirSync(firsts).sort() : []) {
    const dir = path.join(firsts, role);
    const out = path.join(dir, "out.txt");
    const first = {
      run: run.dir,
      out: existsSync(out) ? readFileSync(out, "utf8") : "",
      daemon: daemonLines(dir),
      entries: entriesUnder(path.join(dir, "sessions")),
    };
    const refusal = bootRefusal(first);
    if (refusal === undefined)
      return `relaunched with no refusal: <run>/${FIRST_BOOT}/${role} records no claim boot refusal of the ${role}'s first attempt`;
    if (madeTurn(first))
      return `relaunched after a turn: the ${role}'s first attempt (<run>/${FIRST_BOOT}/${role}) made a turn before its claim boot was refused (${refusal}), which the daemon's relaunch would not take back`;
    firstLines.set(role, first.daemon.length);
    // The second attempt's output: out-<role>.txt once another agent was booted after it
    // (worker_boot), and out.txt while it is the run's last.
    const file = existsSync(path.join(run.dir, `out-${role}.txt`)) ? `out-${role}.txt` : "out.txt";
    const again = bootRefusal({
      run: run.dir,
      out: existsSync(path.join(run.dir, file))
        ? readFileSync(path.join(run.dir, file), "utf8")
        : "",
      daemon: lines.filter((line) => line.role === role).slice(first.daemon.length),
      entries: [],
    });
    if (again !== undefined)
      return `claim boot refused on both attempts: the ${role}'s first (<run>/${FIRST_BOOT}/${role}/out.txt): ${refusal}; its relaunch (<run>/${file}): ${again}`;
  }
  const never =
    "and the agent was not relaunched: the rig relaunches an agent once when its claim boot is refused before its first turn, and never after it";
  for (const [file, text] of outputs(run.dir)) {
    const refusal = bootRefusal({ run: run.dir, out: text, daemon: [], entries: [] });
    if (refusal !== undefined) return `claim boot refused: ${refusal} (<run>/${file}), ${never}`;
  }
  // The stand-in's lines of each role past those its first attempt holds.
  const seen = new Map<string, number>();
  const later = lines.filter((line) => {
    const index = seen.get(line.role ?? "") ?? 0;
    seen.set(line.role ?? "", index + 1);
    return index >= (firstLines.get(line.role ?? "") ?? 0);
  });
  const refusal = bootRefusal({ run: run.dir, out: "", daemon: later, entries: [] });
  return refusal === undefined
    ? undefined
    : `claim boot refused: ${refusal} (<run>/daemon.log), ${never}`;
}

/** Where rig.sh's cmd_run keeps what each service of the run comes from (its services_from), one
 * `<service> <scope> <from> <digest>` a line, beside the run's built-from, before any agent of the
 * run starts. */
const PROVENANCE = "services-from";
/** The services the run's own label's checkout gives every run of the label, which its
 * provenance must name with scope `label`: the Envoy listener and the Dispatch server built from
 * that checkout, the daemon facts the stand-in answers by, read out of it, and the worker pane its
 * daemon module composes. The Dispatch server is the label's because what its agents read of it
 * differs across versions: main ff2a10ef's prints a check result's line ending with the check's
 * text where the head's ends with the role's reading, and refuses a body field it does not
 * declare where the head's ignores it (rig.sh's services_up). rig.sh names more services of scope
 * `label` (the plugins and the model route, which cmd_profile installs from the checkout), and
 * each is held to the checkout as these are. */
const LABEL_SERVICES = ["envoy-listener", "dispatch", "daemon-facts", "worker-pane"] as const;

/** One line of a run's provenance, as the rule reads it: its digest is a record for the reader. */
interface Provenance {
  readonly service: string;
  /** `label`, `pin` or `shared` (rig.sh's services_from says what each means). */
  readonly scope: string;
  readonly from: string;
}

/** The lines of the provenance file `file`: each line's first two words, and what lies between
 * them and its last word, the digest, as its `from`, so a checkout whose path holds a space reads
 * whole. Throws for a line of fewer than four words, which rig.sh never writes. */
function provenanceOf(file: string): Provenance[] {
  return readFileSync(file, "utf8")
    .split("\n")
    .filter((line) => line !== "")
    .map((line) => {
      const words = line.split(" ");
      if (words.length < 4)
        throw new Error(
          `<run>/${PROVENANCE} holds a line that is no \`<service> <scope> <from> <digest>\`: ${line}`
        );
      return {
        service: words[0] ?? "",
        scope: words[1] ?? "",
        from: words.slice(2, -1).join(" "),
      };
    });
}

/** Rule 15: why the run's services are not all its own label's, as a reason: no provenance
 * file, a built-from that names no checkout, a service of scope `label` from any checkout but the
 * one built-from names (a base run whose listener the head checkout built, say), or a service of
 * LABEL_SERVICES the provenance does not name as its label's. */
export function serviceProvenance(run: Pick<Run, "dir" | "label">): string | undefined {
  const file = path.join(run.dir, PROVENANCE);
  if (!existsSync(file))
    return `no service provenance: the run has no <run>/${PROVENANCE}, so which checkout its Envoy listener and its other services came from is unknown`;
  let checkout: string;
  try {
    checkout = path.posix.normalize(labelCheckout(run.dir));
  } catch (error) {
    return `service provenance unknown: ${error instanceof Error ? error.message : String(error)}`;
  }
  const lines = provenanceOf(file);
  const foreign = lines.find(
    (line) => line.scope === "label" && path.posix.normalize(line.from) !== checkout
  );
  if (foreign !== undefined)
    return `foreign service: the run's ${foreign.service} came from ${foreign.from}, not from ${checkout}, the checkout of the ${run.label} label its built-from names`;
  const missing = LABEL_SERVICES.find(
    (service) => !lines.some((line) => line.service === service && line.scope === "label")
  );
  return missing === undefined
    ? undefined
    : `service provenance: <run>/${PROVENANCE} names no ${missing} of the ${run.label} label's own checkout (${checkout})`;
}

/** The service of a run's provenance that names the system git configuration its panes' git
 * reads (rig.sh's services_from): its `<from>` the file that git names, `none` for none, and its
 * digest, `none` where the host has no such file. */
const SYSTEM_GITCONFIG = "system-gitconfig";

/** The system git configuration the provenance of the run `dir` records, `<file> at <digest>`;
 * undefined for a run that records none: one recorded before 2026-11-19, or one with no
 * provenance, which rule 15 names. */
function systemGitconfigOf(dir: string): string | undefined {
  const file = path.join(dir, PROVENANCE);
  if (!existsSync(file)) return undefined;
  const words = readFileSync(file, "utf8")
    .split("\n")
    .find((line) => line.startsWith(`${SYSTEM_GITCONFIG} shared `))
    ?.split(" ");
  return words === undefined || words.length < 4
    ? undefined
    : `${words.slice(2, -1).join(" ")} at ${words.at(-1)}`;
}

/** The batch the record of the run `dir` names; undefined for a run with no record. */
function batchOf(dir: string): string | undefined {
  const identity = runIdentity(dir);
  return typeof identity === "string" ? undefined : identity.batch;
}

/** Rule 23: the first other run of `runs` in the run's batch whose provenance records another
 * system git configuration than the run's, as a reason. A batch's runs share one pod, whose file
 * every run's world shows as the pod holds it, so two of them recorded two only where the
 * batch's agents' git did not read one world. */
export function systemGitconfig(
  run: Pick<Run, "dir">,
  runs: readonly Pick<Run, "dir">[]
): string | undefined {
  const batch = batchOf(run.dir);
  const own = systemGitconfigOf(run.dir);
  if (batch === undefined || own === undefined) return undefined;
  for (const other of runs) {
    if (other.dir === run.dir || batchOf(other.dir) !== batch) continue;
    const theirs = systemGitconfigOf(other.dir);
    if (theirs !== undefined && theirs !== own)
      return `system git configuration: <run>/${PROVENANCE} records ${own}, and ${path.basename(other.dir)} of its batch ${batch} records ${theirs}, so the batch's agents' git did not read one pod's system configuration`;
  }
  return undefined;
}

/** How often rig.sh relaunched an agent of `runs` at its boot, one line per label in byte order:
 * `health: boot relaunches, <label>: <n> in <r> of <m> runs`, each relaunch an agent's first
 * attempt rig.sh kept under <run>/boot-1/<role>/ (its boot_agent), <r> the runs that hold one and
 * <m> the label's runs given. A label none of whose runs was relaunched reads 0, so a batch's
 * count names every label of it. */
function relaunches(runs: readonly Run[]): string[] {
  const counts = new Map<string, { relaunched: number; runs: number; of: number }>();
  for (const run of runs) {
    const first = path.join(run.dir, FIRST_BOOT);
    const roles = existsSync(first)
      ? readdirSync(first).filter((role) => statSync(path.join(first, role)).isDirectory()).length
      : 0;
    const count = counts.get(run.label) ?? { relaunched: 0, runs: 0, of: 0 };
    counts.set(run.label, {
      relaunched: count.relaunched + roles,
      runs: count.runs + (roles > 0 ? 1 : 0),
      of: count.of + 1,
    });
  }
  return [...counts]
    .sort(([a], [b]) => (a < b ? -1 : a > b ? 1 : 0))
    .map(
      ([label, count]) =>
        `health: boot relaunches, ${label}: ${count.relaunched} in ${count.runs} of ${count.of} runs`
    );
}

/** How many paths of one kind of a run's writes outside its own directory `wroteOutside` names
 * before it counts the rest. */
const WRITES_NAMED = 3;

/** Whether `write`, one of the run's (`writesOf`), is an entry only rig.sh's scratch_clean
 * recorded: one that appeared at the top of the machine's shared scratch while the run ran
 * (scratch-created.txt), and that no tool call of the run wrote, itself or a path under it. Who
 * wrote it is unknown: any other process of the machine's user may have, and in a namespace the
 * run's agents reach none of the machine's /tmp at all. */
function appearedOnly(run: Run, write: Access): boolean {
  return (
    write.tool === "scratch_clean" &&
    !run.writes.some((call) => call.tool !== "scratch_clean" && inside(call.path, write.path))
  );
}

/** The line of the runs of `runs` that left a path outside their own directory, HOME and TMPDIR
 * (`writesOf`), which is no verdict on any of them, in two kinds each counted and named on its
 * own: `health: wrote outside its own directory, HOME and TMPDIR (no verdict): <n> of <m> runs`,
 * the paths a tool call of the run wrote, and an entry scratch_clean recorded that one of those
 * calls wrote or wrote under; then `. Appeared in the shared scratch while the run ran, by an
 * unknown writer (no verdict): <k> of <m> runs`, the entries scratch_clean alone recorded
 * (`appearedOnly`). Where its count is not 0, each kind goes on with `: <run> (<path>, <path>,
 * <path> and <j> more); <run> (…)`: each such run by its directory's name, in the order given,
 * with its paths of that kind in the order it first wrote each, the first WRITES_NAMED of them and
 * a count of the rest. So the line itself says which runs they were, where each wrote and what
 * appeared while each ran, with no copy of the runs to read them out of. On 2026-11-16 one kind
 * alone read /tmp/go-build218583819, which only reviewer-text-findings-text-only-head-5's
 * scratch-created.txt names, as a write of that run, whose agents' /tmp was the run's own. */
function wroteOutside(runs: readonly Run[]): string {
  const listed = (appeared: boolean) => {
    const named = runs.flatMap((run) => {
      const paths = [
        ...new Set(
          run.writes
            .filter((write) => appearedOnly(run, write) === appeared)
            .map((write) => write.path)
        ),
      ];
      const rest = paths.length - WRITES_NAMED;
      return paths.length === 0
        ? []
        : [
            `${path.basename(run.dir)} (${paths.slice(0, WRITES_NAMED).join(", ")}${rest > 0 ? ` and ${rest} more` : ""})`,
          ];
    });
    return `${named.length} of ${runs.length} runs${named.length === 0 ? "" : `: ${named.join("; ")}`}`;
  };
  return `health: wrote outside its own directory, HOME and TMPDIR (no verdict): ${listed(false)}. Appeared in the shared scratch while the run ran, by an unknown writer (no verdict): ${listed(true)}`;
}

/** A sentence of an agent's text: what ends at a `.`, `!` or `?` before a space, or at a line's
 * end. */
const SENTENCE = /(?<=[.!?])\s+|\n+/;

/** Whether `input` came before `written`: a file of the run (index -1) before everything, an
 * entry earlier in the same transcript, and, across two transcripts, an entry earlier by time
 * (an entry with no time is before nothing in another transcript). */
function before(input: TracedPart, written: TracedPart): boolean {
  if (input.index < 0) return true;
  if (input.file === written.file) return input.index < written.index;
  return Number.isFinite(input.at) && Number.isFinite(written.at) && input.at < written.at;
}

/** Where each sentence of an agent of a run that names the measurement got its word (rule 17):
 * `leaked`, the first whose word an input held before any agent of the run wrote it, naming that
 * input; `own`, every other, whose word the run's agents wrote before any input held it. */
interface Names {
  readonly leaked: string | undefined;
  readonly own: readonly string[];
}
const namesRead = new WeakMap<object, Names>();

/** Rule 17's reading of `run`: each sentence of an agent's own text or thinking, in any
 * transcript of the run, that says a word of the measurement, a scenario, case or label word or a
 * removed name (run-names.ts's `proseNames`, over `runVocabulary`, which rule 13 reads a web
 * search's query by too), traced to where it came from. Its inputs are the rig's own text of the
 * run's prompt (run-names.ts's `promptOf` and `rigText`), its system prompt and every entry of
 * every transcript that is not an agent's own (`heard`); its writings, every assistant entry of
 * every transcript, tool calls' arguments included (`wrote`), and every message the rig relayed
 * into the prompt from one agent of the run to the next (<run>/reviewer-wake.json), which is its
 * sender's own writing at the time Envoy accepted it, not an input: a run's agents' own words are
 * no leak, the words one agent sends another included, and a word is an agent's own only where
 * nothing the rig wrote carried it to an agent of the run first (the architect's ruling of
 * 2026-11-20T17:30Z and 17:35Z). A relayed message whose sender has no transcript in the run
 * (`sessions`) is no agent's of the run, so it stays the rig's text of the prompt. The word leaked
 * where an input held it before every writing of it (`before`, `holds`): the agent was given it.
 * Otherwise the run's agents made it, and it is theirs: the preflight at cc09c638 read "Since
 * those are text-only issues rather than code that changes behavior" in a head run where no input
 * held the word, and INFRA-764's round 26 at 10c26bb8 read "Pushed one text-only commit" in the
 * message the implementer of threads-closed-correct-head-1 sent its reviewer. */
export function namesOf(
  run: Pick<Run, "dir" | "label" | "sessions" | "said" | "wrote" | "heard">
): Names {
  const read = namesRead.get(run);
  if (read !== undefined) return read;
  const vocab = runVocabulary(run.label);
  const given = (file: string, text: string): TracedPart => ({
    file,
    kind: "",
    text,
    index: -1,
    at: Number.NaN,
  });
  const inputs: TracedPart[] = [];
  const wrote = [...run.wrote];
  try {
    const prompt = promptOf(run.dir);
    if (prompt !== undefined) {
      const theirs = prompt.relayed.filter(({ relayed }) => run.sessions.has(relayed.sender));
      inputs.push(given("<run>/prompt.txt", rigText(prompt.text, theirs)));
      for (const one of theirs)
        wrote.push({
          file: "<run>/reviewer-wake.json",
          kind: "a relayed message",
          text: one.relayed.message,
          index: prompt.relayed.indexOf(one),
          at: Date.parse(one.relayed.at),
        });
    }
    const system = systemPromptOf(run.dir);
    if (system !== undefined) {
      inputs.push(given("<run>/system-args", system.argument));
      if (system.place !== "<run>/system-args") inputs.push(given(system.place, system.text));
    }
  } catch (error) {
    // A relayed message or a system prompt that cannot be read is an input no word can be traced
    // to, so no sentence of the run's agents can be called their own: the rule fails the run,
    // naming why.
    const names = {
      leaked: `names: ${error instanceof Error ? error.message : String(error)}, so no word it gave the run's agents can be traced`,
      own: [],
    };
    namesRead.set(run, names);
    return names;
  }
  inputs.push(...run.heard);
  const own: string[] = [];
  let leaked: string | undefined;
  for (const part of run.said) {
    for (const sentence of part.text.split(SENTENCE)) {
      const word = proseNames(sentence, vocab);
      if (word === undefined) continue;
      const quoted = `a ${part.kind} part in ${part.file} says "${word}": "${sentence.trim().slice(0, 240)}"`;
      const writings = wrote.filter((one) => holds(one.text, word));
      const source = inputs.find(
        (input) => holds(input.text, word) && writings.every((one) => before(input, one))
      );
      if (source === undefined) own.push(quoted);
      else {
        const where = source.index < 0 ? source.file : `${source.kind} in ${source.file}`;
        leaked = `names: ${quoted}, which ${where} held before any agent of the run wrote it`;
        break;
      }
    }
    if (leaked !== undefined) break;
  }
  const names = { leaked, own };
  namesRead.set(run, names);
  return names;
}

/** Rule 17: the first sentence of an agent of the run that says a word an input gave it
 * (`namesOf`). */
export function namesSaid(
  run: Pick<Run, "dir" | "label" | "sessions" | "said" | "wrote" | "heard">
): string | undefined {
  return namesOf(run).leaked;
}

/** The line of the runs of `runs` whose agents said a word of the measurement that they wrote
 * before any input held it (`namesOf`'s `own`), which is no verdict: each run's id and the first
 * such sentence, with a count of the rest. */
export function ownNames(runs: readonly Run[]): string {
  const named = runs.flatMap((run) => {
    const { own } = namesOf(run);
    const [first] = own;
    return first === undefined
      ? []
      : [
          `${path.basename(run.dir)} (${first}${own.length > 1 ? ` and ${own.length - 1} more` : ""})`,
        ];
  });
  return `health: names its agents wrote before any input held them (no verdict): ${named.length} of ${runs.length} runs${named.length === 0 ? "" : `: ${named.join("; ")}`}`;
}

/** Every rule, in the order a run's line names the first it fails. A rule is given the run and
 * every run health was given, which only the cross-run rule reads. */
const RULES: readonly {
  readonly name: string;
  readonly judge: (run: Run, runs: readonly Run[]) => string | undefined;
}[] = [
  { name: "finished", judge: unfinished },
  // Rule 15, read right after the first: a service from another checkout than the label's own
  // explains every rule after it (a listener that refused the label's plugins at their boot).
  { name: "service provenance", judge: serviceProvenance },
  // Rule 14, read next: an agent that died at its boot explains every rule after it (no
  // completion taken, a half that never started).
  { name: "claim boot", judge: claimBoot },
  // Rule 23, read right after it: the system git configuration a batch's runs record, which no
  // rule after it reads.
  { name: "system git configuration", judge: systemGitconfig },
  {
    name: "completions",
    judge: (run) =>
      withDaemon(run, (daemon) => {
        const refused = refusedCompletion(daemonLines(run.dir), run.scenario, daemon);
        if (refused === undefined) return undefined;
        if ("unkeyed" in refused)
          return `unkeyable completion: ${refused.unkeyed} (POST ${refused.line.path} answered ${answerOf(refused.line)}), so whose completion it was is unknown`;
        return `refused completion: POST ${refused.line.path} of the ${refused.role} answered ${answerOf(refused.line)}, which the ${run.label} label's daemon does not answer it with in this world`;
      }),
  },
  {
    name: "taken",
    judge: (run) =>
      withDaemon(run, (daemon) => {
        const taken = takenCompletions(daemonLines(run.dir), run.scenario, daemon);
        return taken !== undefined && taken.count === 0
          ? `no completion taken: no completion of the ${taken.role} was answered as the ${run.label} label's daemon answers one it takes`
          : undefined;
      }),
  },
  { name: "routes", judge: unservedRoute },
  { name: "failed claim", judge: failedClaim },
  // Rule 22, read right after its sibling: what the run's daemon took from a claim after its boot.
  { name: "capability report", judge: capabilityReport },
  { name: "push guard", judge: guardRefusal },
  // Rule 21, read right before rules 20 and 7, which would name the world's refusal of the same
  // call: a call that prints the run's GitHub token is the agent printing its credential.
  { name: "credential printed", judge: credentialPrinted },
  // Rule 20, read right before its sibling: a write the world does not model, which rule 7
  // leaves to it.
  { name: "unmodelled write", judge: (run) => unmodelledWrite(run, typedBy(run)) },
  { name: "gh", judge: (run) => ghUndeclared(run, typedBy(run)) },
  // Rule 16, read right after its sibling: an agent's Dispatch call refused, as rule 7 reads a
  // refused `gh` call.
  { name: "dispatch", judge: dispatchUndeclared },
  // Rule 18, read right after rule 16: the run's GitHub, which its Dispatch read for an agent.
  { name: "github", judge: (run) => githubRefusal(run) },
  // Rule 19, read right after rule 18: each push's head, which the run's Dispatch compares the
  // check's results with for every agent that reads them after it.
  { name: "head", judge: (run) => headUnmoved(run) },
  // Rule 24, read right after rule 19: each push's head, which the world's test merge is made of.
  { name: "unmerged head", judge: unmergedHead },
  { name: "another run", judge: otherRunReach },
  { name: "another run's write", judge: otherRunWrite },
  { name: "outside its world", judge: outsideWorld },
  { name: "another process", judge: otherProcess },
  { name: "credentials", judge: credentialReach },
  { name: "off-world network", judge: offWorld },
  // Rule 17, read last: what an agent said, once the world it said it in has been read.
  { name: "names", judge: namesSaid },
];

if (import.meta.main) {
  const argv = Bun.argv.slice(2);
  // rig.sh's boot_agent asks, once an agent's launch has ended, whether to relaunch it: the one
  // reader of a boot refusal (boot-refusal.ts), which the "claim boot" rule then holds the record
  // to. It prints the refusal and exits 0 for a relaunch, exits 1 for none, and 2 for a usage
  // error or a record it cannot read, which a status of 1 would have read as no refusal.
  if (argv[0] === "--boot-refusal")
    process.exit(askBootRefusal(argv.slice(1), BOOT_USAGE, "health"));
  // The shared scratch's entries before the batch's first run, one path a line: what a run with
  // no record of its own (scratch-before.txt) is held to, and added to the record of one that has.
  let snapshot: string[] | undefined;
  // The git directories the run's label sources are rebuilt from (`recordedCheckout`), each
  // `--repo <git dir>` the caller gives, in its order.
  const repos: string[] = [];
  // The options stand before the runs, in any order.
  while (argv[0] === "--tmp-snapshot" || argv[0] === "--repo") {
    const [option, value] = argv;
    argv.splice(0, 2);
    if (option === "--repo") {
      if (value === undefined || !existsSync(value)) {
        console.error(
          `health: --repo names no git directory${value === undefined ? "" : `: ${value}`}`
        );
        console.error(USAGE);
        process.exit(2);
      }
      repos.push(path.resolve(value));
      continue;
    }
    if (value === undefined || !existsSync(value)) {
      console.error(
        `health: --tmp-snapshot names no file${value === undefined ? "" : `: ${value}`}`
      );
      console.error(USAGE);
      process.exit(2);
    }
    snapshot = readFileSync(value, "utf8")
      .split("\n")
      .map((line) => line.trim())
      .filter((line) => line !== "");
  }
  if (argv.length === 0) {
    console.error(USAGE);
    process.exit(2);
  }
  const named = argv.map((dir) => ({ dir, run: runOf(dir) }));
  const bad = named.find((one) => typeof one.run === "string");
  if (bad !== undefined) {
    console.error(`health: ${String(bad.run)}`);
    console.error(USAGE);
    process.exit(2);
  }
  const runs = named.flatMap(({ dir, run }) => {
    if (typeof run === "string") return [];
    const loaded = loadRun(dir, run.scenario, run.label, repos);
    return [
      snapshot === undefined
        ? loaded
        : { ...loaded, before: [...(loaded.before ?? []), ...snapshot] },
    ];
  });
  let failed = 0;
  const flagged = new Map(RULES.map((rule) => [rule.name, 0]));
  // The runs no verdict is given on, each with why: a rule that reads an input the run did not
  // record, or one the caller's repositories cannot rebuild (Refused), which no reading of the pod
  // stands in for. A run is healthy only when every rule holds, so a rule that cannot judge it
  // leaves it unjudged, unless a rule that can judge it fails it: that run is unhealthy whatever
  // the other rule would read.
  const refused: string[] = [];
  for (const run of runs) {
    let refusal: string | undefined;
    const found = RULES.flatMap((rule) => {
      // A record of the run that does not parse (a daemon.log line that is no JSON, say) is a run
      // whose world cannot be read, which is no healthy one.
      let reason: string | undefined;
      try {
        reason = rule.judge(run, runs);
      } catch (error) {
        if (error instanceof Refused) {
          refusal ??= error.message;
          return [];
        }
        reason = `unreadable record: ${error instanceof Error ? error.message : String(error)}`;
      }
      return reason === undefined ? [] : [{ rule: rule.name, reason }];
    });
    if (found.length === 0 && refusal !== undefined) {
      refused.push(refusal);
      continue;
    }
    for (const { rule } of found) flagged.set(rule, (flagged.get(rule) ?? 0) + 1);
    if (found.length > 0) failed += 1;
    // Keyed by the run's directory, its id, as every reader of these lines keys them; a run with a
    // record also names it last, `(<scenario>[-<case>]-<label>-<n>)`, which no verdict reads, so a
    // person can tell the run without opening the record. Health runs outside every run's world.
    const identity = runIdentity(run.dir);
    const name = typeof identity !== "string" && identity.recorded ? `  (${identity.name})` : "";
    console.log(
      `${path.basename(run.dir)}  ${found[0] === undefined ? "healthy" : `unhealthy: ${found[0].reason}`}${name}`
    );
  }
  const judged = runs.length - refused.length;
  for (const [name, count] of flagged) console.log(`health: ${name}: ${count} of ${judged} runs`);
  // How often each label's agents met a boot refusal the rig relaunched them for, which no rule
  // flags (rule 14 declares a first attempt's): a batch reads its relaunches here, by label.
  for (const line of relaunches(runs)) console.log(line);
  // A run's own writes outside its directory, and what appeared in the shared scratch while it
  // ran, are no verdict on it: named here, run by run and kind by kind, and judged where another
  // run read them (`otherRunWrite`).
  console.log(wroteOutside(runs));
  // A word of the measurement the run's agents made themselves is no verdict on the run: named
  // here, run by run, beside rule 17's verdict on a word an input gave them.
  console.log(ownNames(runs));
  console.log(`health: ${failed} of ${judged} runs unhealthy`);
  // A run refused is judged neither healthy nor unhealthy: each is named with the input it lacks,
  // and the command exits 2, as for a usage error, never 0 or 1.
  if (refused.length > 0) {
    for (const why of refused) console.error(`health: no verdict on ${why}`);
    console.error(`health: ${refused.length} of ${runs.length} runs refused`);
    process.exit(2);
  }
  process.exit(failed === 0 ? 0 : 1);
}
