#!/usr/bin/env bun
// Dispatch's intake of a push, for a worker run's own Dispatch: on every push to the pull
// request's branch, the head Dispatch compares a `before_merge` check's results with moves to the
// pushed commit, as production's `pull_request` event moves it within seconds of the push
// (packages/envoy/internal/dispatch/delivery/intake.go). The world's Dispatch hears no event: it
// runs no NATS intake, and a read of its results learns no head by itself, since the read-through
// asks GitHub only for the commits of results that stand at no head (results_comparison.go,
// learn → readThroughHead), and every result stands at the head stored before the push. Measured
// on 2026-11-16: with nothing in between, `dispatch results` after a push still compared with the
// approved head and said a pass stands, and 3 of 20 recorded runs of the threads-closed case that
// `correct` replaced (4 calls, all base) had their implementer read exactly that.
//
// The push itself cannot write the head: the remote's post-receive hook runs inside the agent's
// namespace, which shows neither this directory nor the run's database URL (rig.sh's world_paths),
// and a hook that held the URL would hand it to the agent. So this process, which rig.sh starts
// outside the namespace for the length of the run (start_heads), follows the run's push log, which
// that hook writes (rig.sh's fixture_open), and stores each push's head through the same write the
// rig's seed makes (seed-handoffs.ts's pullRequestHead), one push at a time, in the order pushed.
//
//   head-watcher.ts <run dir>
//
// Its record is <run dir>/logs/heads.log, which the agent's world hides: a first line
// {"at","opened":"heads.log"}, then one JSON line per push to legion/<issue>, in push order,
// {"at","ref","pushed","ok":true} once the head is stored, or {"at","ref","pushed","ok":false,
// "error"} when the write failed. A push to any other ref is no pull request's head and writes no
// line. health.ts's "head" rule holds every push the log names to an ok line of its own commit.
// DISPATCH_E2E_PORT and SEED_DATABASE_URL name the run's Dispatch and database, as for seed.ts.
import { appendFileSync, existsSync, readFileSync, watch, writeFileSync } from "node:fs";
import path from "node:path";
import { pullRequestHead, TesterFixture } from "./seed-handoffs";

const run = Bun.argv[2];
if (run === undefined || Bun.argv.length !== 3) {
  console.error("usage: head-watcher.ts <run dir>");
  process.exit(2);
}
const fixture = path.join(run, "fixture.json");
const branch = `refs/heads/legion/${TesterFixture.parse(JSON.parse(readFileSync(fixture, "utf8"))).issue}`;
const pushes = path.join(run, "pushes.log");
const record = path.join(run, "logs", "heads.log");

/** Appends one line of the record. */
function note(line: Record<string, unknown>): void {
  appendFileSync(record, `${JSON.stringify({ at: new Date().toISOString(), ...line })}\n`);
}

/** The pushes the log holds, in order, as the hook writes them: `<at> <ref> <old> <new>`. */
function logged(): { ref: string; pushed: string }[] {
  if (!existsSync(pushes)) return [];
  return readFileSync(pushes, "utf8")
    .split("\n")
    .filter((line) => line.trim() !== "")
    .map((line) => {
      const [, ref = "", , pushed = ""] = line.split(" ");
      return { ref, pushed };
    });
}

// The pushes before this process started are the fixture's, which it emptied the log of
// (fixture_head_commit), so the record answers every line the log holds from here on.
let handled = logged().length;
let running: Promise<void> | undefined;
let again = false;

/** Stores the head of every push the log holds that no earlier pass handled, one after another. */
async function drain(): Promise<void> {
  do {
    again = false;
    const all = logged();
    for (const push of all.slice(handled)) {
      handled++;
      if (push.ref !== branch) continue;
      try {
        await pullRequestHead(
          fixture,
          path.join(run, "fixtures.json"),
          path.join(run, "remote.git")
        );
        note({ ref: push.ref, pushed: push.pushed, ok: true });
      } catch (error) {
        note({ ref: push.ref, pushed: push.pushed, ok: false, error: String(error) });
      }
    }
  } while (again);
}

/** Starts a pass, or asks the running one to look again once it is done. */
function wake(): Promise<void> {
  if (running !== undefined) {
    again = true;
    return running;
  }
  running = drain().finally(() => {
    running = undefined;
  });
  return running;
}

writeFileSync(record, `${JSON.stringify({ at: new Date().toISOString(), opened: "heads.log" })}\n`);
// The directory and not the file: the hook appends to the log, and the fixture made it before.
watch(run, (_event, name) => {
  if (name === "pushes.log") void wake();
});
// A stop finishes the pass in hand and reads the log once more, so no push made before it is lost.
process.on("SIGTERM", () => {
  void wake().then(() => process.exit(0));
});
void wake();
