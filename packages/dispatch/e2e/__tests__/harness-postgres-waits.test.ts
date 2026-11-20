/** The rows of the program of each row's own limit (`harness-postgres-waits.ts`) and of the
 *  leftover rows' fixture `docker` under it: a row whose CI step ends before its limit ends inside
 *  the step and names its waits, each wait's share of what the step leaves is exact, a limit that
 *  counts a part below the bound of the part a fixture waits for fails by name, and a fixture call
 *  that runs out, or finds already passed, its row's deadline stops and removes what it made. */

import { afterAll, expect, test } from "bun:test";
import { existsSync, rmSync } from "node:fs";
import { join } from "node:path";
import { machine, removalBounds } from "../harness-postgres-container-removal";
import * as marks from "../harness-postgres-marks";
import {
  clockRoomS,
  fixtureDockerInChild,
  standInDocker,
  standInId,
} from "./harness-postgres-docker-fixtures";
import { holding, plantsRoot, scratch, until } from "./harness-postgres-removal-fixtures";
import {
  boundOf,
  claimEventMs,
  neverEnds,
  type Part,
  partOf,
  type Row,
  rowMs,
  rowOf,
  rowRoomMs,
  type Waits,
  waitsMs,
} from "./harness-postgres-waits";
import { noHookLimit, removeScratch, stepEndsVariable } from "./step-end";

afterAll(
  () => removeScratch([plantsRoot()], rowMs({ children: 1, reads: 0, bounds: [] })),
  // The file's own directory of plants (`plantsRoot`), which each row empties, goes in a process of
  // its own, which may take what the CI step leaves, or one child's limit, and the hook has no
  // limit of Bun's: step-end.ts says why.
  noHookLimit
);

const onLinux = process.platform === "linux";

/** What the next row waits for (`Waits`): a child, a holder read, an `until`, and in its `finally`
 *  the bound of a removal, so its own limit is minutes. */
const stepCut: Waits = {
  children: 1,
  reads: 1,
  bounds: [claimEventMs],
  ends: [removalBounds.boundMs],
};

// A row whose CI step ends before the row's own limit takes the step's end for its own: its waits
// and its `finally` share what the step leaves in the proportion of their bounds, and a wait that
// reaches the row's deadline ends there, naming what it waited for and the step's end. Off CI, no
// step sets an end, and the row keeps its own limit. The control: the same waits with no step's
// end keep their own deadline, and a wait of theirs keeps its own bound. The step's end is a
// moment from now on the real clock, since a row's deadline is on it (`machine`).
test(
  "a row whose CI step ends before its own limit ends inside the step, and its waits name what they waited for and the step's end",
  async () => {
    const saved = process.env[stepEndsVariable];
    let free: Row;
    let cut: Row;
    // The step's end is a whole second, as a step exports it, between 0.5 and 1.5 s from now.
    const stepEnd = Math.ceil((Date.now() + 500) / 1000) * 1000;
    try {
      delete process.env[stepEndsVariable];
      free = rowOf(stepCut);
      process.env[stepEndsVariable] = String(stepEnd / 1000);
      cut = rowOf(stepCut);
    } finally {
      if (saved === undefined) delete process.env[stepEndsVariable];
      else process.env[stepEndsVariable] = saved;
    }
    const own = rowMs(stepCut);
    expect([free.end - free.deadline, free.ms, free.cut]).toEqual([
      own - waitsMs(stepCut),
      waitsMs(stepCut),
      undefined,
    ]);
    expect(boundOf(free, claimEventMs)).toEqual({
      ms: claimEventMs,
      named: `within ${claimEventMs / 1000} s`,
    });
    const start = cut.deadline - cut.ms;
    expect(cut.end).toBe(stepEnd);
    expect(cut.ms).toBe(Math.floor((waitsMs(stepCut) * (stepEnd - start)) / own));
    expect(cut.cut).toContain(
      `the end of the CI step that runs it, at ${new Date(stepEnd).toISOString()} (${stepEndsVariable})`
    );
    await expect(until(cut, () => false, "a planted event", claimEventMs)).rejects.toThrow(
      new RegExp(
        `^a planted event did not come by the row's deadline, [\\d.]+ s after this wait began, which the end of the CI step that runs it, at .+ set, before its own ${claimEventMs / 1000} s$`
      )
    );
    expect(Date.now()).toBeLessThan(stepEnd + 1_000);
  },
  // What the row counts, summed by the fixtures' program (`rowMs`): one child, one holder read, the
  // planted event's bound, and the removal bound of its `finally`.
  rowMs(stepCut)
);

// The row above draws its step's end from a window of 1 s, so a run of it reads one share of the
// row's limit. This row reads every whole ms of that window, on a clock it holds still
// (`machine.now`, the clock `rowOf` reads), and holds the share `rowOf` gives the row's waits and
// the share it gives its stops to the exact share, in integers. A share taken as a ratio first and
// then scaled rounds twice: it floored one ms short of an exact 438 in a run of the row above on
// 2026-11-19.
test(
  "a row's waits and its stops get the exact whole ms of what its CI step leaves, at every whole ms the row above may draw",
  () => {
    const own = rowMs(stepCut);
    const stops = own - waitsMs(stepCut) - (stepCut.roomMs ?? rowRoomMs);
    const exact = (ms: number, limit: number) => Number((BigInt(ms) * BigInt(limit)) / BigInt(own));
    // A whole second, as a step exports it, ahead of the first read of the step's end.
    const stepEnd = Math.ceil(Date.now() / 1000) * 1000 + 10_000;
    const saved = process.env[stepEndsVariable];
    const now = machine.now;
    const wrong: string[] = [];
    try {
      process.env[stepEndsVariable] = String(stepEnd / 1000);
      for (let limit = 500; limit <= 1_500; limit += 1) {
        machine.now = () => stepEnd - limit;
        const row = rowOf(stepCut);
        const got = [row.ms, row.stops - row.deadline];
        const want = [exact(waitsMs(stepCut), limit), exact(stops, limit)];
        if (got[0] !== want[0] || got[1] !== want[1]) {
          wrong.push(
            `${limit} ms left: waits ${got[0]} ms and stops ${got[1]} ms, where the exact shares are ${want[0]} and ${want[1]}`
          );
        }
      }
    } finally {
      machine.now = now;
      if (saved === undefined) delete process.env[stepEndsVariable];
      else process.env[stepEndsVariable] = saved;
    }
    expect(wrong).toEqual([]);
  },
  // What the row counts, as the row above: it calls `rowOf` of the same waits and waits for none.
  rowMs(stepCut)
);

/** What the charge of a row whose limit counts `counted` says of its waits for `waited`, one after
 *  another (`Row.part`): "counted" where the limit counts each of them, and else the refusal of
 *  the first it does not. The row of the next row's own, which waits for nothing. */
function chargeOf(counted: Waits, ...waited: readonly Part[]): string {
  const row = rowOf(counted);
  try {
    for (const part of waited) row.part(part);
    return "counted";
  } catch (error) {
    return error instanceof Error ? error.message : String(error);
  }
}

// A fixture that waits for a part (`Part`), such as a start of the pool, charges its row before
// the wait, and a row whose limit counts no such part with a bound as long fails there, naming it,
// whatever the load. Parts waited for at once count once, at the longest, and each counted part
// serves one wait.
test("a row whose limit counts a part below the bound of the part a fixture waits for fails there, naming it", () => {
  const short = partOf("a start of the pool", { children: 1, reads: 0, bounds: [] });
  const long = partOf("a start of the pool", { children: 1, reads: 1, bounds: [] });
  const atOnce: Waits = { children: 0, reads: 0, bounds: [], parts: [{ atOnce: [long, short] }] };
  expect(long.ms).toBeGreaterThan(short.ms);
  expect(chargeOf({ children: 0, reads: 0, bounds: [], parts: [short] }, long)).toBe(
    `the row's limit counts ${short.ms / 1000} s for a start of the pool it has not waited for yet, and it waits for a start of the pool for up to ${long.ms / 1000} s: its limit is below what it waits for`
  );
  expect(chargeOf({ children: 0, reads: 0, bounds: [long.ms] }, short)).toBe(
    `the row's limit counts nothing for a start of the pool it has not waited for yet, and it waits for a start of the pool for up to ${short.ms / 1000} s: its limit is below what it waits for`
  );
  expect(chargeOf(atOnce, short, long)).toBe("counted");
  expect(chargeOf(atOnce, short, long, short)).toContain("its limit is below what it waits for");
  expect(rowMs(atOnce)).toBe(long.ms + rowRoomMs);
});

/** How long the next row's fixture call may run before the row's deadline: the row's own, short
 *  so the row takes seconds. A fixture call waits for the time its row has left, which the program
 *  of a row's limit counts (`makeMs`), and no fixed time of its own. */
const ranOutMs = 5_000;

/** What the next row waits for (`Waits`): one child, the fixture call's wait to its deadline, and
 *  the room above that deadline in which it removes what the call made (`rowRoomMs`). */
const ranOut: Waits = { children: 1, reads: 0, bounds: [ranOutMs, rowRoomMs] };

// A daemon that many sessions share took 133 s over one `docker create` of the leftover rows on
// 2026-11-19, while it answered every other call; the fixture then gave the create up at a fixed
// 120 s, and the container Docker made stayed, Created. A fixture call of Docker now waits for
// the time its row has left (`docker` of harness-postgres-leftover-fixtures.ts), and one that
// runs out that time names the call on one line, removes what it made, by the name and the label
// it gave, and fails the row with that line. The stand-in `docker` makes the container and never
// answers the create; it lists the container by that name and label while it has it, and its
// `docker rm` removes it.
test.skipIf(!onLinux)(
  "v-no-leftover-postgres: a fixture's docker create that Docker never answers ends at the row's deadline, names the call on one line, removes the container it made by its name and label, and fails the row with that line",
  async () => {
    const row = rowOf(ranOut);
    const { tmp } = scratch();
    const name = `dispatch-e2e-pg-ran-out-${process.pid}`;
    const label = "dev.legion.dispatch-e2e.ran-out";
    try {
      const docker = standInDocker(tmp, [
        'made="$(dirname "$0")/made"',
        'case "$1" in',
        `  create) : >"$made"; ${neverEnds(row)} ;;`,
        `  ps) if [ -e "$made" ] && [[ " $* " == *" name=^/?${name}$ "* ]] && [[ " $* " == *" label=${label} "* ]]; then echo ${standInId}; fi ;;`,
        `  rm) rm -f "$made"; echo "$4" ;;`,
        "esac",
      ]);
      const args = ["create", "--name", name, "--label", `${label}=1`, "postgres"];

      const { ended, lines } = await fixtureDockerInChild(row, docker.bin, args, {
        children: 0,
        reads: 0,
        bounds: [ranOutMs],
      });

      const line = `e2e/__tests__/harness-postgres-leftover-fixtures.ts: docker ${args.join(" ")} did not end within the ${ranOutMs / 1000} s the row waits in all, so the row stopped it, and removed container ${standInId.slice(0, 12)} (${name}) it made`;
      expect(ended).toEqual({ error: line });
      expect(holding(lines, ": docker create ")).toEqual([line]);
      expect(existsSync(join(docker.bin, "made"))).toBe(false);
      // The calls but the removal's own asks of whether Docker still lists the container, which a
      // `docker rm` that load holds past one pause makes (`removeContainer`).
      expect(
        docker
          .calls()
          .filter((call) => !call.startsWith("ps --all --quiet --no-trunc --filter id="))
      ).toEqual([
        args.join(" "),
        `ps --all --quiet --no-trunc --filter name=^/?${name}$ --filter label=${label}`,
        `rm --force --volumes ${standInId}`,
      ]);
      // On the stand-in's clock: the row stopped the create at its deadline, and no sooner.
      const ticks = docker.clock();
      const created = ticks.find((tick) => tick.call.startsWith("create "))?.at ?? Number.NaN;
      const listed = ticks.find((tick) => tick.call.startsWith("ps "))?.at ?? Number.NaN;
      expect(listed - created).toBeGreaterThan(ranOutMs / 1000 - 1);
      expect(listed - created).toBeLessThan(ranOutMs / 1000 + clockRoomS);
      await until(
        row,
        () => docker.running().length === 0,
        "the end of the docker create the row stopped",
        claimEventMs
      );
    } finally {
      rmSync(tmp, { recursive: true, force: true });
    }
  },
  // What the row waits for, summed by the fixtures' program (`rowMs`): one child, the fixture
  // call's wait to its deadline and the removal of what the call made.
  rowMs(ranOut)
);

/** How long the next row's child waits, a wait its row names, before its fixture call: past the
 *  row's deadline (`ranOutMs`), by more than the room one start of a stand-in takes under load. */
const usedUpMs = ranOutMs + 3_000;

/** What the next row waits for (`Waits`): one child, and in it the named wait past the deadline
 *  and the fixture call after it, which starts nothing. */
const usedUp: Waits = { children: 1, reads: 0, bounds: [usedUpMs] };

// The image-ready row ran out its deadline on 2026-11-19 in its removal of the builder of its
// image, which Docker took 17 and 21 minutes over while its daemon stalled. The row's next fixture
// call, a `psql`, then started after the deadline, was stopped at once, and its line said that the
// `psql` "did not end within" the row's time, naming a call that had had no time at all. A fixture
// call that its row's deadline finds already passed now starts nothing, and its line says so and
// names the wait during which the deadline passed (`during` of harness-postgres-waits.ts).
test.skipIf(!onLinux)(
  "v-no-leftover-postgres: a fixture's docker call that its row's deadline finds already passed starts nothing, says the deadline had passed before it, and names the wait that used it up",
  async () => {
    const row = rowOf(usedUp);
    const { tmp } = scratch();
    try {
      const docker = standInDocker(tmp, ["echo answered"]);
      const args = ["ps", "--all", "--quiet"];
      const what = "the row's own wait past its deadline";

      const { ended, lines } = await fixtureDockerInChild(
        row,
        docker.bin,
        args,
        { children: 0, reads: 0, bounds: [ranOutMs] },
        { before: { what, ms: usedUpMs } }
      );

      const line = new RegExp(
        `^e2e/__tests__/harness-postgres-leftover-fixtures\\.ts: docker ${args.join(" ")} did not start: the ${ranOutMs / 1000} s the row waits in all had run out \\d+\\.\\d s before it, during ${what}, from \\d+\\.\\d s to \\d+\\.\\d s of the row$`
      );
      expect(ended).toEqual({ error: expect.stringMatching(line) });
      expect(holding(lines, ": docker ps ")).toEqual([expect.stringMatching(line)]);
      // The call started nothing: the stand-in `docker` got no call.
      expect(existsSync(join(docker.bin, "calls"))).toBe(false);
    } finally {
      rmSync(tmp, { recursive: true, force: true });
    }
  },
  // What the row waits for, summed by the fixtures' program (`rowMs`): one child, and in it the
  // named wait past the deadline.
  rowMs(usedUp)
);

/** The room the next row gives above its waits and those of its `finally` (`Waits.roomMs`): its
 *  own, 6 s, in place of `rowRoomMs`, through the same parameter every row's room is read from, so
 *  the row sees its `finally`'s stop and the removal after it in seconds and not in the room's
 *  minute. Half of it is ample for the removal of one container by a stand-in `docker`, a bash
 *  script, which answers in well under a second. */
const ranOutRoomMs = 6_000;

/** How long the next row's `finally` takes over its stop, as a leftover row's stop of a stack it
 *  started does: more than the half of the row's room that a removal of what the row made has
 *  above the row's deadline, so the removal after it has time left only where the deadline did
 *  not count the stop (`Waits.ends`). */
const endStopMs = ranOutRoomMs / 2 + 1_000;

/** The bound the next row's `finally` gives that stop (`Waits.ends`), with room above the time it
 *  takes for a slow start of the child's sleep. */
const endBoundMs = endStopMs + 10_000;

/** What the next row's child waits for (`Waits`): the fixture call's wait to its deadline, and, in
 *  its `finally`, the stop, before the removal of what the row made, with the row's own room. */
const ranOutThenStop: Waits = {
  children: 0,
  reads: 0,
  bounds: [ranOutMs],
  ends: [endBoundMs],
  roomMs: ranOutRoomMs,
};

/** What the next row waits for (`Waits`): the child, and everything the child's row waits for. */
const ranOutInChild: Waits = { ...ranOutThenStop, children: 1 };

// The image row of the leftovers ran out its deadline on 2026-11-19 with a create that Docker
// never answered, and its `finally` then stopped a stack it started, for about 30 s, before it
// removed the container of the stack it had killed: the deadline had counted that stop, so the
// removal's listing had no time left, and that stack's container stayed, with a mark no later
// start could judge. A row's deadline now ends its own waits alone, and its `finally` has the time
// its stops take after it (`Waits.ends`), and then half the room above them for its removals
// (`Row.cleanup`). The child's create never ends; its `finally` stops for `endStopMs`, and then
// lists the containers whose mark names the row's run and removes each.
test.skipIf(!onLinux)(
  "v-no-leftover-postgres: a row whose fixture call runs out the row's deadline still stops what it started in its finally, and then lists and removes the containers of its stacks",
  async () => {
    const row = rowOf(ranOutInChild);
    const { tmp, run } = scratch();
    const name = `dispatch-e2e-pg-ran-out-stop-${process.pid}`;
    const label = "dev.legion.dispatch-e2e.ran-out";
    const killedId = "1".repeat(64);
    const mark = JSON.stringify({ run });
    try {
      const docker = standInDocker(tmp, [
        'made="$(dirname "$0")/made"',
        'case "$1" in',
        `  create) : >"$made"; ${neverEnds(row)} ;;`,
        `  ps) if [[ " $* " == *" --format "* ]]; then printf '%s %s\\n' ${killedId} ${JSON.stringify(mark)}; elif [ -e "$made" ]; then echo ${standInId}; fi ;;`,
        `  rm) rm -f "$made"; echo "$4" ;;`,
        "esac",
      ]);
      const args = ["create", "--name", name, "--label", `${label}=1`, "postgres"];

      const { ended } = await fixtureDockerInChild(row, docker.bin, args, ranOutThenStop, {
        end: { stopMs: endStopMs, run },
      });

      expect(ended).toEqual({
        error: expect.stringContaining(`docker ${args.join(" ")} did not end within the `),
        removed: true,
      });
      const calls = docker.calls();
      expect(calls).toContain(
        `ps --all --no-trunc --filter label=${marks.markLabel} --format {{.ID}} {{.Label "${marks.markLabel}"}}`
      );
      expect(calls).toContain(`rm --force --volumes ${killedId}`);
      // On the stand-in's clock: the listing came after the deadline and the stop, past the half
      // of the room a removal had above the deadline where the deadline counted the stop.
      const ticks = docker.clock();
      const created = ticks.find((tick) => tick.call.startsWith("create "))?.at ?? Number.NaN;
      const listed =
        ticks.find((tick) => tick.call.startsWith("ps ") && tick.call.includes("--format"))?.at ??
        Number.NaN;
      expect(listed - created).toBeGreaterThan((ranOutMs + endStopMs) / 1000 - 1);
      await until(
        row,
        () => docker.running().length === 0,
        "the end of the docker create the row stopped",
        claimEventMs
      );
    } finally {
      rmSync(tmp, { recursive: true, force: true });
    }
  },
  // What the row waits for, summed by the fixtures' program (`rowMs`): one child, the fixture
  // call's wait to its deadline, the stop of its finally, and the removal of what the row made.
  rowMs(ranOutInChild)
);
