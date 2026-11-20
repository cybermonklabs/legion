/** When the CI step that runs this process ends it, and the removal of a test file's scratch that
 *  only that end bounds.
 *
 *  A hook of `bun test` that removes a directory in the test process cannot be stopped by its own
 *  limit: Bun judges a synchronous hook only after it returns, so `rmSync` runs to its end and the
 *  limit then fails a removal that ended. Beside a shared Linux machine's load on 2026-11-19, the
 *  `afterAll` of `scripts/e2e/lib/sweep-order.test.ts` took 3.5 to 5.3 s to remove its 570 entries,
 *  and Bun's own limit of 5 s failed 12 of 20 whole runs of that file there. A longer limit is the
 *  next load's failure. So each `afterAll` or `afterEach` of an e2e test file that removes a
 *  directory runs the removal as a process of its own (`removeScratch`) and gives Bun no limit for
 *  the hook (`noHookLimit`). The removal's one bound is what the CI step that runs the file leaves
 *  (`stepEnds`), or, where no step says when it ends, the limit of the file's own rows: a slow
 *  removal that ends passes, and one that never ends fails there, naming its directories.
 *
 *  It imports nothing but Bun's own modules, so the e2e libraries of `scripts/e2e/lib` use it as
 *  this directory's files do. */

/** The variable through which a CI step that runs these tests gives the time at which its own
 *  limit (`timeout-minutes`) ends it, in whole seconds since the epoch: the first line of the
 *  step's script exports it, as `date -u +%s` and that same limit times 60, and a row of
 *  `harness-postgres-source-rules.test.ts` reads every workflow of the repository for a step that
 *  runs a row of `harness-postgres-waits.ts`'s files and exports none
 *  (`stepsThatCutNoRow`).
 *  Its name is no one package's: any test of the repository may read when its step ends. */
export const stepEndsVariable = "LEGION_TEST_STEP_ENDS_AT";

/** When this module was first read in this process: as the first test file that reads it starts. */
const firstRead = Date.now();

/** The longest a GitHub Actions job runs, five days, on a self-hosted runner (GitHub's usage
 *  limits; a hosted runner's job ends at six hours): no step's end is farther than that from its
 *  start. An end in milliseconds, read as seconds, is thousands of years away. */
const longestJobMs = 5 * 24 * 3_600_000;

/** When the CI step that runs this process ends it (`stepEndsVariable`), in milliseconds since the
 *  epoch, or undefined where no step said so, as in a run on a machine of one's own. A value that
 *  is not a whole number of seconds, the empty one included, one that was already past when the
 *  first test file of this process that reads it started, and one farther than any job runs
 *  (`longestJobMs`), are refused by name: a garbled end is never read as none, as an end every
 *  wait has run out, or as one no wait reaches. */
export function stepEnds(): number | undefined {
  const value = process.env[stepEndsVariable];
  if (value === undefined) return undefined;
  const at = /^[0-9]+$/.test(value) ? Number(value) * 1000 : Number.NaN;
  if (!Number.isSafeInteger(at)) {
    throw new Error(
      `${stepEndsVariable} is ${JSON.stringify(value)}, which is no whole number of seconds since the epoch`
    );
  }
  const started = `the first test file of this process that reads it started, at ${new Date(firstRead).toISOString()}`;
  if (at <= firstRead) {
    throw new Error(
      `${stepEndsVariable} is ${value}, ${new Date(at).toISOString()}, which was already past when ${started}`
    );
  }
  // Before any date of it is written: a value this far can lie past the last date a Date holds.
  if (at - firstRead > longestJobMs) {
    throw new Error(
      `${stepEndsVariable} is ${value}, which is more than the ${longestJobMs / 3_600_000} hours any job runs after ${started}`
    );
  }
  return at;
}

// A value it refuses fails the first file that reads this module as that file starts, and not
// first at a wait or a hook of its end.
stepEnds();

/** A hook's limit that Bun reads as none. On 2026-11-19, under `bun test` 1.4.4, an `afterAll` and
 *  an `afterEach` whose body took 7 s, sleeping synchronously or awaiting, each passed with this
 *  limit; with no limit, the synchronous ones failed at 7 s, as they returned, and the awaiting ones
 *  at Bun's 5 s. A limit of 0 passed those too, but not a hook that awaits a process of its own:
 *  Bun killed that process at its 5 s ("killed 1 dangling process"), so a removal under 0 would end
 *  there, killed, and not at its own bound. A hook that runs `removeScratch` takes this, so the
 *  removal's own bound is the one bound of the hook. */
export const noHookLimit = Number.POSITIVE_INFINITY;

/** Bun's own limit of a row that states none, under `bun test` with no `--timeout`: the limit of the
 *  rows of a file whose rows state none, which such a file hands `removeScratch`. */
export const bunRowMs = 5_000;

/** What a removal leaves of the CI step that runs it, before the step's own limit ends the step:
 *  room for Bun to end the hook whose removal ran out and to write the line that names it, and for
 *  the step's clock, which starts before the step's first line exports its end. */
export const stepRoomMs = 5_000;

/** Removes each directory of `paths`, with `rm -rf` in a process of its own, for an `afterAll` or
 *  `afterEach` that gives Bun no limit (`noHookLimit`). The process may take what the CI step that
 *  runs this file leaves, less `stepRoomMs` (`stepEnds`), or, where no step says when it ends,
 *  `rowMs`, the limit of the file's own rows. A removal that does not end by then is killed, and
 *  this rejects naming its directories and the bound it ran out; one that exits other than 0
 *  rejects naming them and what `rm` said. `rm` is found on `PATH`, as a test's stand-in can be. */
export async function removeScratch(paths: readonly string[], rowMs: number): Promise<void> {
  if (paths.length === 0) return;
  const end = stepEnds();
  const bound =
    end === undefined
      ? { ms: rowMs, by: `the limit of this file's rows` }
      : {
          ms: Math.max(0, end - Date.now() - stepRoomMs),
          by: `${stepRoomMs / 1000} s before the end of the CI step that runs it, at ${new Date(end).toISOString()} (${stepEndsVariable})`,
        };
  const removal = Bun.spawn(["rm", "-rf", "--", ...paths], {
    stderr: "pipe",
    stdin: "ignore",
    stdout: "ignore",
  });
  // Read while it runs, so a removal that writes more than a pipe holds is not held up by it.
  const said = new Response(removal.stderr).text();
  const ranOut = Promise.withResolvers<"ran out">();
  const timer = setTimeout(() => ranOut.resolve("ran out"), bound.ms);
  const ended = await Promise.race([removal.exited, ranOut.promise]);
  clearTimeout(timer);
  const named = paths.join(", ");
  if (ended === "ran out") {
    // Not waited for: a process held in the kernel, as on a hung mount, ends only when the kernel
    // lets it, and the hook's line must come before the step's own limit.
    removal.kill("SIGKILL");
    throw new Error(
      `the removal of ${named} did not end within ${bound.ms / 1000} s, ${bound.by}, and was killed`
    );
  }
  if (ended !== 0) {
    throw new Error(`the removal of ${named} exited ${ended}: ${(await said).trim()}`);
  }
}
