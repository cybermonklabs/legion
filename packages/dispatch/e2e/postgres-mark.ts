/** The command through which a shell test stack marks its Postgres container and removes, at its
 *  start, the Postgres of each stack that is gone: the one mark of `e2e/harness-postgres-marks.ts`
 *  and the one removal of `e2e/harness-postgres-removal.ts`, which each start of the browser pool
 *  runs too (`e2e/harness-pool.ts`). `scripts/e2e/lib/leftovers.sh` calls it.
 *
 *    bun packages/dispatch/e2e/postgres-mark.ts label OWNER RUN CONTAINER
 *      prints the `--label` value that marks the container CONTAINER of the run whose work
 *      directory is RUN, whose owner is the process OWNER of this PID namespace: the shell's own
 *      `$$`, the driver whose death ends the run. The container serves on a port Docker publishes,
 *      so the mark names no socket directory. The mark holds RUN by its one spelling
 *      (`containerMark`), so a RUN spelt with a doubled or a trailing `/`, as a `TMPDIR` that ends
 *      in one gives it, is marked as the directory it names. Exits 2, printing why, where OWNER is
 *      no process this one can read, RUN is no directory named by an absolute path, or CONTAINER
 *      is no name Docker takes.
 *    bun packages/dispatch/e2e/postgres-mark.ts sweep
 *      removes each Postgres whose mark names a process that is gone, leaves every other one, and
 *      names what it leaves (`removeLeftoverPostgres`), on stderr. Exits 0. */
import { statSync } from "node:fs";
import { isAbsolute } from "node:path";

import {
  containerMark,
  containerName,
  labelOf,
  type Mark,
  processOwner,
} from "./harness-postgres-marks";
import { removeLeftoverPostgres } from "./harness-postgres-removal";

/** What this command's own refusals are prefixed with, as every starter in `e2e/` names itself. */
const named = "e2e/postgres-mark.ts";

function refuse(why: string): never {
  console.error(`${named}: ${why}`);
  process.exit(2);
}

const [command, ...args] = process.argv.slice(2);
if (command === "sweep" && args.length === 0) {
  await removeLeftoverPostgres();
} else if (command === "label" && args.length === 3) {
  const [ownerArg = "", run = "", container = ""] = args;
  const pid = /^\d+$/.test(ownerArg) ? Number(ownerArg) : Number.NaN;
  const owner = Number.isSafeInteger(pid) && pid > 0 ? processOwner(pid) : null;
  if (owner === null) refuse(`${ownerArg} names no process of this PID namespace it can read`);
  if (!containerName.test(container))
    refuse(`${container} is no name Docker takes for a container`);
  // The one spelling of RUN, read once: `containerMark` throws where RUN names nothing.
  let mark: Mark | undefined;
  try {
    mark = isAbsolute(run) ? containerMark(owner, run, container) : undefined;
  } catch {
    mark = undefined;
  }
  if (mark === undefined || !statSync(mark.identity, { throwIfNoEntry: false })?.isDirectory()) {
    refuse(`${run} is no directory named by an absolute path`);
  }
  console.log(labelOf(mark));
} else {
  refuse("usage: postgres-mark.ts label OWNER RUN CONTAINER | postgres-mark.ts sweep");
}
