import { afterEach, expect, test } from "bun:test";
import { chmod, readFile, writeFile } from "node:fs/promises";
import * as path from "node:path";
import { STALL_EXIT_CAP, STALL_EXIT_CODE } from "@legion/pi-shared/stall-watchdog";
import { call } from "@legion/pi-shared/test/omp-harness";
import {
  ARCHITECT_TOPIC,
  BOUND_MS,
  childOf,
  cleanup,
  conversation,
  DEADLINE_MS,
  exitsFile,
  ISSUE,
  kernelOf,
  lastMessage,
  ofKind,
  QUIET_MS,
  ROLE,
  runs,
  SHELL_DEADLINE_MS,
  STEER_TEXT,
  STEP_MS,
  type StallPane,
  scratchDir,
  skip,
  startPane,
  stepOf,
  WARM_CELL,
  warmedWedge,
} from "../test/stall-watchdog-pane";

// The stall watch's ladder on the real Oh My Pi (INFRA-1274's `k6`): a worker whose tool call
// stops, carried through the three steps of `@legion/pi-shared/stall-watchdog` by this package's
// four collaborators, against the binary that ships. Only the real binary can say any of this: the
// clock the watch judges a call by starts at the host's own `tool_execution_start`, the processes
// the first step ends are the ones this host really started, the second step is the host's
// `ctx.abort()` and the turn it does or does not end, and the last step is this process exiting
// with the status the daemon reads as a death.
//
// LEGION_TEST_OMP names the binary, as in legion-phase-stall-omp.test.ts: a run without one skips,
// except on GitHub Actions, where a skip would hide the only run of the check on the host that
// ships it (GITHUB_ACTIONS, not CI: agent harnesses on the devbox export CI=true).
//
// **What was measured here on 2026-11-07, against `/opt/omp/bin/omp` (`omp/18.16.0`) in
// `--mode rpc`**, and what each case below is built on:
//
//  1. **An `eval` cell with an explicit long deadline holds the turn open and writes no end
//     frame**: no `tool_execution_end`, no `turn_end`, no `agent_end`, until something ends it.
//     Its deadline is one the host may pause (`STALL_PAUSED_DEADLINE_TOOLS`), so the watch judges
//     it by the role's bound alone, which is the number these cases set low.
//  2. **`tool_execution_start` reaches the plugin about a second after the cell has begun.** The
//     kernel, and a child the cell starts at once, are already running when the watch starts the
//     call's clock, so the "started after the call" rule (`src/stall-recovery.ts`, rule 2) cannot
//     see them. Each case that wants that rule has its cell wait before it forks; the case that
//     wants the runner rule (the last one here) runs a cell with no child at all.
//
//     **On a session whose `eval` kernel does not exist yet, that second is a race, and the
//     kernel is what it is played for.** The first cell of a session starts the kernel too, so
//     the kernel's own start and the call's recorded start are that second of host plumbing
//     apart, in no fixed order: measured here the kernel is the older by 0.95 s and the step ends
//     the child alone, but the margin is that second less the interpreter's own startup, so a
//     host slower to get `python3` onto `/proc` puts the kernel at or after the call's start,
//     where rule 2 takes it along with the child. Measured by delaying the interpreter — a
//     `python3` on the pane's PATH that sleeps two seconds before running the real one — the
//     first step's outcome became "ended 3 processes of the call" (the shim, the interpreter and
//     the child), the host answered the cell with "Kernel died", and the ladder took no further
//     step, because a call that ends closes its own record. That is what CI's `pi-legion` lane
//     reports as "ended 2 processes of the call" on a machine of its own speed. Every case whose
//     kernel must outlive the call it wedges therefore warms the kernel with a cell of its own
//     first (`warmedWedge`): a kernel started by a turn that has already ended is older than the
//     call being judged by the whole of that turn — 1.4 s here — and the slower the host, the
//     older it is.
//  3. **`ctx.abort()` ends every wedged call this file could build, and resolves only once that
//     call has ended.** Five shapes were tried against the pin: an `eval` cell looping in
//     `time.sleep`; one ignoring SIGINT; one ignoring SIGINT, SIGTERM and SIGHUP, which the host
//     answered by shutting the kernel down; one blocked in a host `wait()` on a subagent whose
//     model request never answered; and a `pty: true` shell command trapping every signal. Each
//     came back within about five seconds of the abort, with the turn ended — and a turn that
//     ends closes the call's record, so the ladder's last step, which runs *only* where the abort
//     did not end the turn, has no tool call on this binary that can reach it. The two cases that
//     prove that last step therefore take `abort` off the session context the host hands its
//     extensions, with a one-handler extension written into the case's own scratch root: that is
//     exactly the host `src/stall-abort.ts` documents ("A host with no `abort()` … throws from
//     here"). Nothing of the plugin is replaced, and everything else in those cases is the real
//     thing — the real worker exits, the real counter file is written and read back by the
//     relaunch, and the real notice reaches the listener.

/**
 * What one case's cleanup has to do, and why it gets a bound of its own: each pane's steps kill a
 * real Oh My Pi and wait for it to be reaped, close the stand-in server and the socket that
 * refuses NATS, and remove a scratch root holding that pane's home, its sessions and its `eval`
 * kernel's temporary files. On a loaded pod that is seconds of real work, and Bun's default hook
 * bound is 5 s: one run of three at `c9f2ddf3c71f` came back 6 pass, 2 fail with 68 `expect()`
 * calls against a clean run's 83, two cases cut short at the hook rather than at an assertion.
 * The bound here is proportionate to what the hook does, in a suite whose own cases are allowed
 * three minutes each; a cleanup that really hangs still fails, two minutes later.
 */
const CLEANUP_MS = 120_000;

afterEach(async () => {
  for (const step of cleanup.splice(0).reverse()) await step();
}, CLEANUP_MS);

test.skipIf(skip)(
  "a cell that ends when its child dies: the call is judged from its execution start, the evidence names the child, the owner is told, and the first step ends the child with no abort sent",
  async () => {
    const state = await scratchDir("child-state");
    const pidFile = path.join(await scratchDir("child-pid"), "child.pid");
    const pane = await startPane({
      name: "stall-child",
      stateDir: state,
      reply: warmedWedge(pidFile, { goOn: false }, "The call came back."),
    });
    const child = await childOf(pane, pidFile);

    // The evidence, written before any step ran: the call's own clock, the rule that judged it —
    // the role's bound, since the host may pause an `eval` deadline — and the child in the tree
    // the reader took from `/proc`.
    const evidence = await pane.until("the evidence for the stuck call", async () =>
      ofKind(await pane.records(), "evidence").at(0)
    );
    expect(evidence.tool).toBe("eval");
    expect(evidence.session).toBe("phase-worker");
    expect(evidence.issue).toBe(ISSUE);
    expect(evidence.role).toBe(ROLE);
    expect(evidence.startedAt).not.toBeNull();
    expect(evidence.rule).toEqual({ kind: "bound", boundMs: BOUND_MS });
    expect(evidence.allowedMs).toBe(BOUND_MS);
    expect(evidence.openMs).toBeGreaterThan(BOUND_MS);
    expect(evidence.arguments).toContain('language="py"');
    if (!evidence.tree.read) throw new Error(`the tree was not read: ${evidence.tree.reason}`);
    const seen = evidence.tree.processes.find((process) => process.pid === child);
    expect(seen?.command).toContain("/bin/sleep 3000");

    // The owner's notice: one publish to the tree's architect, the sentence that names the call,
    // and the whole record as its payload.
    const notice = await pane.until("the notice the owner was sent", () => pane.publishes().at(0));
    expect(notice.topic).toBe(ARCHITECT_TOPIC);
    expect(String(notice.message)).toContain(`The ${ROLE} of ${ISSUE}`);
    expect(String(notice.message)).toContain("has a `eval` call that has not returned");
    expect(JSON.parse(String(notice.payload))).toMatchObject({
      kind: "evidence",
      toolCallId: evidence.toolCallId,
    });

    // The first step: the child, and only the child — the cell's own child gone, this session's
    // kernel untouched, and exactly one process ended. The count is pinned again because the
    // warming cell is what made it deterministic: the kernel was started by a turn that had
    // already ended, so it is older than the judged call by the whole of that turn and rule 2
    // can never take it along with the child, on this host or a slower one (measurement 2).
    // Unwarmed, the kernel's start and the call's are a second of host plumbing apart in no
    // fixed order, which is what made the count 2 on CI's lane and 3 with a delayed
    // interpreter; warmed, the only process started inside the call is the cell's own child. A
    // step that took the kernel too would end the call instead of letting it come back.
    const ended = await pane.until("the first step of the ladder", async () =>
      stepOf(await pane.records(), "end-processes")
    );
    expect(ended.toolCallId).toBe(evidence.toolCallId);
    expect(ended.outcome).toBe("ended 1 process of the call");
    expect(await runs(child)).toBe(false);
    expect(await runs(kernelOf(evidence).pid)).toBe(true);

    // The cell was waiting on that child, so the call came back on its own: the next turn carries
    // its result, and the ladder never took a second step.
    const after = await pane.until("the turn that answers the call's result", () =>
      pane.turns().find((turn) => conversation(turn).includes("the child is gone"))
    );
    expect(conversation(after)).toContain("the child is gone");
    // An absence read taken while the pane is mid-write would be the absence of a line still
    // being written, not of the record: `records()` withholds an unfinished last line while the
    // pane runs (test/transcript-entries.ts, dispatch://INFRA-2188), so this waits for the
    // transcript to end on a newline before it reads one.
    await pane.settled();
    const records = await pane.records();
    expect(ofKind(records, "step").map((record) => record.step)).toEqual(["end-processes"]);
    expect(ofKind(records, "stopped")).toEqual([]);
    expect(await pane.restarts()).toEqual([]);
    expect(pane.running()).toBe(true);
  },
  DEADLINE_MS + 60_000
);

test.skipIf(skip)(
  "a cell that goes on after its child dies: the abort ends the turn, the session lives, and the worker takes a turn again on the message it sent itself",
  async () => {
    const state = await scratchDir("abort-state");
    const pidFile = path.join(await scratchDir("abort-pid"), "child.pid");
    const pane = await startPane({
      name: "stall-abort",
      stateDir: state,
      reply: warmedWedge(pidFile, { goOn: true }, "Noted; I will do something else."),
    });
    const child = await childOf(pane, pidFile);

    // The same first step as the case above, with the same pinned count and for the same reason:
    // the evidence, the owner's one notice, the cell's own child ended and nothing else, and the
    // kernel the warming cell started a turn earlier still running. The warm-up is what makes
    // the count deterministic — a kernel older than the judged call by a whole turn is one rule
    // 2 can never take (measurement 2) — and here a step that took the kernel would end the
    // call, leaving no turn for the second step to abort.
    const evidence = await pane.until("the evidence for the stuck call", async () =>
      ofKind(await pane.records(), "evidence").at(0)
    );
    const ended = await pane.until("the first step of the ladder", async () =>
      stepOf(await pane.records(), "end-processes")
    );
    expect(ended.toolCallId).toBe(evidence.toolCallId);
    expect(ended.outcome).toBe("ended 1 process of the call");
    expect(await runs(child)).toBe(false);
    expect(await runs(kernelOf(evidence).pid)).toBe(true);
    expect(pane.publishes().map((publish) => publish.topic)).toEqual([ARCHITECT_TOPIC]);

    // The cell went on without its child, so the second step ran: the host's own abort ended the
    // turn, and the worker put one message in front of its own session.
    const aborted = await pane.until("the second step of the ladder", async () =>
      stepOf(await pane.records(), "abort-turn")
    );
    expect(aborted.outcome).toBe(
      "the turn was ended with the host's abort(); the session was sent the message naming the ended call"
    );
    expect(aborted.openMs).toBeGreaterThanOrEqual(ended.openMs + STEP_MS);

    // The session is the same session, and it took a turn on the message it sent itself.
    const again = await pane.until("the turn the worker's own message started", () =>
      pane.turns().find((turn) => lastMessage(turn).includes("Your turn was ended because"))
    );
    expect(lastMessage(again)).toContain("`eval` call");
    expect(lastMessage(again)).toContain("Decide whether that call is still worth making");
    const restarts = await pane.restarts();
    expect(restarts).toHaveLength(1);
    expect(restarts[0]).toContain("Your turn was ended because its `eval` call");
    expect(pane.running()).toBe(true);

    // The ladder stopped at the abort: the turn it ended closed the call's record, so the step
    // that ends this process never ran, nothing was told that the ladder had stopped, and no
    // exit was counted.
    await pane.settled();
    const records = await pane.records();
    expect(ofKind(records, "step").map((record) => record.step)).toEqual([
      "end-processes",
      "abort-turn",
    ]);
    expect(ofKind(records, "stopped")).toEqual([]);
    await expect(readFile(exitsFile(state), "utf8")).rejects.toThrow();
  },
  DEADLINE_MS + 60_000
);

test.skipIf(skip)(
  "a call the abort does not end: the worker ends its own process, the count survives in the state directory, and the launch that reaches the cap ends nothing and tells its owner",
  async () => {
    const state = await scratchDir("exit-state");
    /** One launch of the same claim on that state directory, whose host has no `abort()`. */
    const launch = async (name: string): Promise<StallPane> => {
      const pidFile = path.join(await scratchDir(`${name}-pid`), "child.pid");
      const pane = await startPane({
        name,
        stateDir: state,
        denyAbort: true,
        // Warmed, as every case whose cell must outlive the first step is: a kernel this call
        // started itself would go with the child, and a call that ends takes the ladder's
        // remaining steps with it (measurement 2).
        reply: warmedWedge(pidFile, { goOn: true }, "Noted."),
      });
      await childOf(pane, pidFile);
      return pane;
    };

    // The first launch: the abort fails for the host's own reason, so the ladder takes its last
    // step and this worker ends itself with the status the daemon reads as a death.
    const first = await launch("stall-exit-1");
    const firstExit = await first.until("the last step of the ladder", async () =>
      stepOf(await first.records(), "exit-process")
    );
    expect(stepOf(await first.records(), "abort-turn")?.outcome).toContain(
      "the host's abort() failed: this host has no ctx.abort()"
    );
    expect(firstExit.outcome).toBe(`ending this process, stall exit 1 of ${STALL_EXIT_CAP}`);
    expect(await first.exited).toBe(STALL_EXIT_CODE);
    expect(JSON.parse(await readFile(exitsFile(state), "utf8"))).toMatchObject({
      exits: 1,
      who: `${ISSUE.toLowerCase()}-${ROLE}`,
    });

    // The daemon relaunches the claim in the same pod, so the next worker reads that count back
    // and spends the last exit the cap allows.
    const second = await launch("stall-exit-2");
    const secondExit = await second.until("the last step of the second launch", async () =>
      stepOf(await second.records(), "exit-process")
    );
    expect(secondExit.outcome).toBe(
      `ending this process, stall exit ${STALL_EXIT_CAP} of ${STALL_EXIT_CAP}`
    );
    expect(await second.exited).toBe(STALL_EXIT_CODE);
    expect(JSON.parse(await readFile(exitsFile(state), "utf8"))).toMatchObject({
      exits: STALL_EXIT_CAP,
    });

    // The launch after that is at the cap: it ends nothing, writes why, and hands its owner a
    // session that is stuck rather than a claim the daemon would fail.
    const third = await launch("stall-exit-3");
    const stopped = await third.until("the ladder stopping short", async () =>
      ofKind(await third.records(), "stopped").at(0)
    );
    expect(stopped.reason).toContain(
      `this worker has already ended itself ${STALL_EXIT_CAP} times for a stuck call`
    );
    expect(stopped.reason).toContain(`cap of ${STALL_EXIT_CAP}`);
    const told = await third.until("the notice that the ladder stopped short", () =>
      third.publishes().find((publish) => String(publish.message).includes("cannot recover"))
    );
    expect(told.topic).toBe(ARCHITECT_TOPIC);
    expect(String(told.message)).toContain("it cannot recover from on its own");
    expect(String(told.message)).toContain(`cap of ${STALL_EXIT_CAP}`);
    // The process is what the cap protects. A real clock is the only thing that shows it is still
    // there: an exit the ladder did not take emits no signal to await.
    await Bun.sleep(2 * STEP_MS);
    expect(third.running()).toBe(true);
    expect(JSON.parse(await readFile(exitsFile(state), "utf8"))).toMatchObject({
      exits: STALL_EXIT_CAP,
    });
  },
  3 * DEADLINE_MS
);

test.skipIf(skip)(
  "a counter file the worker cannot read ends no process: the ladder stops at its last step and tells its owner instead",
  async () => {
    const state = await scratchDir("unreadable-state");
    // A counter file this worker may not read: a count it cannot trust is one it will not end its
    // own process on.
    const file = exitsFile(state);
    await writeFile(
      file,
      `${JSON.stringify({ exits: 0, at: "2026-11-07T00:00:00Z", who: "x" })}\n`
    );
    await chmod(file, 0o000);
    const pidFile = path.join(await scratchDir("unreadable-pid"), "child.pid");
    const pane = await startPane({
      name: "stall-unreadable",
      stateDir: state,
      denyAbort: true,
      reply: warmedWedge(pidFile, { goOn: true }, "Noted."),
    });
    await childOf(pane, pidFile);

    const stopped = await pane.until("the ladder stopping short", async () =>
      ofKind(await pane.records(), "stopped").at(0)
    );
    expect(stopped.reason).toBe(
      "the stall exit count could not be read, so the process was not ended"
    );
    const told = await pane.until("the notice that the ladder stopped short", () =>
      pane.publishes().find((publish) => String(publish.message).includes("cannot recover"))
    );
    expect(String(told.message)).toContain("could not be read");
    // As above: the proposition is a process that is still there.
    await Bun.sleep(2 * STEP_MS);
    expect(pane.running()).toBe(true);
  },
  DEADLINE_MS + 60_000
);

test.skipIf(skip)(
  "a cell that finishes inside its bound is left alone: no record, no notice, no process ended, and its result comes back as usual",
  async () => {
    const state = await scratchDir("control-state");
    const pidFile = path.join(await scratchDir("control-pid"), "child.pid");
    const pane = await startPane({
      name: "stall-control",
      stateDir: state,
      reply: (index) =>
        index === 1
          ? [
              call("eval", {
                language: "py",
                code: [
                  "import pathlib, subprocess",
                  'p = subprocess.Popen(["/bin/sleep", "300"], start_new_session=True)',
                  `pathlib.Path(${JSON.stringify(pidFile)}).write_text(str(p.pid))`,
                  "print('the control started', p.pid)",
                ].join("\n"),
                timeout: 600,
              }),
            ]
          : [{ type: "text", text: "The control is done." }],
    });
    const child = await childOf(pane, pidFile);

    // The cell returned at once, so its result is the next turn's and nothing about it is overdue.
    const after = await pane.until("the turn that answers the control's result", () =>
      pane.turns().find((turn) => conversation(turn).includes("the control started"))
    );
    expect(conversation(after)).toContain("the control started");

    // Past the bound and two sweeps. A real clock, deliberately: the proposition is that the watch
    // did nothing at all, and a watch that does nothing emits no signal to await.
    await Bun.sleep(QUIET_MS);
    await pane.settled();
    expect(await pane.records()).toEqual([]);
    expect(await pane.restarts()).toEqual([]);
    expect(pane.publishes()).toEqual([]);
    // The child the cell left running is still running: the first step ends the processes of a
    // call that is overdue, and no call here was.
    expect(await runs(child)).toBe(true);
    expect(pane.running()).toBe(true);
    await expect(readFile(exitsFile(state), "utf8")).rejects.toThrow();
  },
  DEADLINE_MS + 60_000
);

test.skipIf(skip)(
  "a cell with no child, on a kernel older than its call: the first step ends this session's own `eval` runner, which is the rule that needs the runner's real command",
  async () => {
    const state = await scratchDir("runner-state");
    const pane = await startPane({
      name: "stall-runner",
      stateDir: state,
      // The first cell starts the kernel and returns, so the second cell's runner is older than
      // the call that wedges: the one process rule 3 of `src/stall-recovery.ts` may end, and the
      // only thing left to end when a cell forks nothing.
      reply: (index) => {
        if (index === 1) return [WARM_CELL];
        if (index === 2) {
          return [
            call("eval", {
              language: "py",
              code: ["import time", "while True:", "    time.sleep(0.5)"].join("\n"),
              timeout: 600,
            }),
          ];
        }
        return [{ type: "text", text: "The call came back." }];
      },
    });

    const evidence = await pane.until("the evidence for the stuck call", async () =>
      ofKind(await pane.records(), "evidence").at(0)
    );
    // The runner as this host really writes it: a directory named for the user, holding a file
    // named for the kernel. The pattern that identified a runner stopped at the first path
    // separator, so it matched nothing here and this step ended nothing — which is what this case
    // caught and `STALL_TOOL_RUNNERS` now spans.
    const runner = kernelOf(evidence);
    expect(runner.command).toMatch(/omp-python-runner-[^\s]*\.py$/);
    expect(runner.command).toContain("/runner-");

    const ended = await pane.until("the first step of the ladder", async () =>
      stepOf(await pane.records(), "end-processes")
    );
    expect(ended.outcome).toBe("ended 1 process of the call");
    expect(await runs(runner.pid)).toBe(false);

    // Ending the runner ends the call: the host hands back a dead kernel, so the ladder takes no
    // second step and this worker goes on with its work.
    const after = await pane.until("the turn that answers the call's result", () =>
      pane.turns().find((turn) => conversation(turn).includes("Kernel died"))
    );
    expect(conversation(after)).toContain("Kernel died");
    await pane.settled();
    expect(ofKind(await pane.records(), "step").map((record) => record.step)).toEqual([
      "end-processes",
    ]);
    expect(pane.running()).toBe(true);
  },
  DEADLINE_MS + 60_000
);

test.skipIf(skip)(
  "a steering delivery into the turn a wedged cell holds open takes nothing out of the watch's book, and the entry closes the run's calls at an end an extension could have told apart",
  async () => {
    const state = await scratchDir("steer-state");
    const pidFile = path.join(await scratchDir("steer-pid"), "child.pid");
    const pane = await startPane({
      name: "stall-steer",
      stateDir: state,
      // The delivery lands as the wedging cell begins, which is where a message from this
      // worker's architect lands while its turn is parked in a call (steerExtension).
      steer: { text: STEER_TEXT, when: "legion-watch-steer-pid" },
      reply: warmedWedge(pidFile, { goOn: true }, "Noted."),
    });
    const child = await childOf(pane, pidFile);

    // The steer does not free the agent from the call: the host neither backgrounds the cell nor
    // ends the turn for it, so the call is still the turn's and still the watch's to judge. The
    // evidence is the proposition — a watch that had lost the call would write nothing at all,
    // and every delivery a worker gets would be a hole in this change.
    const evidence = await pane.until("the evidence for the stuck call", async () =>
      ofKind(await pane.records(), "evidence").at(0)
    );
    expect(evidence.tool).toBe("eval");
    expect(evidence.rule).toEqual({ kind: "bound", boundMs: BOUND_MS });
    const ended = await pane.until("the first step of the ladder", async () =>
      stepOf(await pane.records(), "end-processes")
    );
    expect(ended.toolCallId).toBe(evidence.toolCallId);
    expect(await runs(child)).toBe(false);

    // Then the turn the abort ended settles, and the host has the steered message to go on with:
    // the run's own end says so — `isTerminal` false on the RPC frame — and the host starts
    // another turn for that message at once, which the worker answers.
    const steered = await pane.until("the turn the steering delivery started", () =>
      pane.turns().find((turn) => conversation(turn).includes(STEER_TEXT))
    );
    expect(conversation(steered)).toContain(STEER_TEXT);
    expect(
      pane
        .frames()
        .filter((frame) => frame.type === "agent_end")
        .map((frame) => frame.isTerminal)
    ).toContain(false);

    // **And the close at that end is a choice.** An extension is told which end it is: the host
    // answers `ctx.hasPendingMessages()` true at the non-terminal end and false at the terminal
    // one that follows, which is what this pane's own steering extension records. The entry
    // reads none of it and closes the run's calls at either end, because the host writes every
    // call's `tool_execution_end` before a run's `turn_end` and `agent_end` — so no call of the
    // run is open at that point — and because the turn after it is a turn of its own, which a
    // ladder judging the ended run's calls would abort (`legion.ts`'s `agent_end` handler, and
    // the case in `legion.test.ts` that drives `willContinue`).
    const ends = await pane.until("the two run ends this session reported", async () => {
      const seen = await pane.agentEnds();
      return seen.length >= 2 ? seen : undefined;
    });
    expect(ends.map((end) => end.pendingMessages)).toEqual([true, false]);
    expect(pane.running()).toBe(true);
  },
  DEADLINE_MS + 60_000
);

test.skipIf(skip)(
  "a shell call the host backgrounds leaves the watch's book with the host's own end, past the moment its own deadline would have had it judged, while the process it started runs on",
  async () => {
    const state = await scratchDir("background-state");
    const pidFile = path.join(await scratchDir("background-pid"), "child.pid");
    const pane = await startPane({
      name: "stall-background",
      stateDir: state,
      // Three things make this case able to fail, and each is here for that alone. **The
      // steer**, because a plain shell call is backgrounded on its own only after about a
      // minute, and the watch judges such a call by the deadline its arguments declare plus the
      // grace: a case long enough to hold the host's minute must declare a deadline longer than
      // itself, and then no sweep could judge the call whether the watch had closed it or not.
      // Steered, the host backgrounds it at once and the short deadline below falls inside the
      // case. **A raised bound**, so the only rule in play is that deadline and the cell below
      // is judged by nothing. **And a cell that never returns**, because no sweep judges
      // anything while the host reports the session idle (`src/stall-watchdog.ts`), which a
      // worker that has answered and stopped is: without it this case would be empty whatever
      // the watch held.
      steer: { text: STEER_TEXT, when: "legion-watch-background-pid" },
      boundMs: 600_000,
      reply: (index) =>
        index === 1
          ? [
              call("bash", {
                command: `echo $$ > ${JSON.stringify(pidFile)}; while true; do sleep 0.5; done`,
                timeout: SHELL_DEADLINE_MS / 1_000,
              }),
            ]
          : [
              call("eval", {
                language: "py",
                code: ["import time", "while True:", "    time.sleep(0.5)"].join("\n"),
                timeout: 600,
              }),
            ],
    });
    const shell = await childOf(pane, pidFile);

    // What the host does with a shell command that does not return: it backgrounds the command
    // and ends the call itself, with no error and the job still running (measurement 1 and
    // measurement 5 in `@legion/pi-shared/stall-watchdog`'s header).
    const end = await pane.until("the host's own end of the call", () =>
      pane.frames().find((frame) => frame.type === "tool_execution_end")
    );
    expect(end.isError).toBe(false);
    expect(end.result).toMatchObject({ details: { async: { state: "running" } } });
    // The command the call started is running at that end, which is the whole of what makes
    // the closed record worth a case; the host kills the job itself at the deadline below, so
    // this is read here rather than at the end of the case.
    expect(await runs(shell)).toBe(true);

    // So a backgrounded call is a closed call in this watch's book, and the process it started
    // lives on outside it. That is the right record and not a gap: the agent is no longer parked
    // in that call — the host freed its turn and handed it the job — and the ladder's steps would
    // end the processes of a call nobody is waiting on and abort a turn that is working. What the
    // agent does next is a call of its own with a clock of its own, and a session that then goes
    // quiet altogether is the daemon's backstop to end, not this watch's.
    //
    // The wait is what makes the emptiness mean something: the call declared its own deadline,
    // so a record still open would be overdue a grace later and written up by the next
    // 5-second sweep. This waits past that moment and several sweeps beyond it. Driven: with
    // every close of the entry cut — `executionEnded`, `resultDelivered` and the `turnEnded`
    // of both `turn_end` and `agent_end` — this case is red, the sweep writing the evidence
    // and the ladder's steps for the backgrounded `bash` call. Cutting the two closing by id
    // alone leaves it green, and that is the system rather than the case: the host ends the
    // turn immediately after it backgrounds the call, so the turn's own close takes the record
    // either way. No live case can hold one of these four alone.
    await Bun.sleep(SHELL_DEADLINE_MS + QUIET_MS);
    await pane.settled();
    expect(await pane.records()).toEqual([]);
    expect(pane.publishes()).toEqual([]);
    expect(pane.running()).toBe(true);
  },
  DEADLINE_MS + 60_000
);
