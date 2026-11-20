/** The image provider's server loads the disk stall's library, so a row that makes the disk stop
 *  answering reaches it (pull request #4236, `e2e/disk-stall.ts`).
 *
 *  The provider runs its container as this process's user, which the image's passwd file does not
 *  hold, and for such a user the image's entrypoint preloads nss_wrapper INSTEAD of what it was
 *  given (`export LD_PRELOAD="$wrapper"`, docker-library/postgres's docker-entrypoint.sh) and
 *  unsets LD_PRELOAD once the cluster is made. The server then loaded no stall library and every
 *  flush went through: run 44647294777's `read-marker-disk-stall.multiprocess.e2e.ts` failed,
 *  naming the library its processes had not loaded. The provider now mounts a passwd file that
 *  holds this user, and this row reads the server's own memory map for the library, which is the
 *  one reading that fails whenever the preload is lost again. */

import { expect, test } from "bun:test";
import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

import { diskStallVariable, stallEnvironment } from "../disk-stall";
import { pgImageVariable, stackPostgres } from "../harness-postgres";
import { sqlAt } from "../psql";
import { ownStart, ownStop, startOwn, stopOwn } from "./harness-postgres-provider-children";
import { rowMs, rowOf, type Waits } from "./harness-postgres-waits";
import { requirePostgres, skipsWithoutPostgres } from "./postgres-provider";

const skipped = skipsWithoutPostgres("harness-postgres-image-stall.test.ts", [pgImageVariable]);

/** What the row waits for: one start of the image provider's Postgres at the provider's own
 *  bounds, and its stop in the `finally`. Its psql answers a started server within the room. */
const oneStart: Waits = {
  children: 0,
  reads: 0,
  bounds: [],
  parts: [ownStart(pgImageVariable)],
  ends: [ownStop(pgImageVariable)],
};

test.skipIf(skipped)(
  "the image provider's server loads the disk stall's library, which the image's entrypoint would otherwise replace",
  async () => {
    const row = rowOf(oneStart);
    requirePostgres([pgImageVariable]);
    const dir = mkdtempSync(join("/tmp", "pg-stall-row-"));
    const stall = mkdtempSync(join(tmpdir(), "stall-"));
    try {
      const provider = stackPostgres({
        dir,
        run: `stall-${process.pid}`,
        env: {
          ...process.env,
          DISPATCH_E2E_PG_CONNECTIONS: process.env.DISPATCH_E2E_PG_CONNECTIONS ?? "100",
          [diskStallVariable]: stall,
        },
      });
      if (provider === undefined) throw new Error(`${pgImageVariable} named no provider`);
      const database = await startOwn(row, provider, pgImageVariable, 0, "the start with a stall");
      try {
        const preloaded = stallEnvironment(stall).LD_PRELOAD;
        const loaded = await sqlAt(database.url, [
          `SELECT position(' ${preloaded.replaceAll("'", "''")}' || chr(10) in pg_read_file('/proc/self/maps')) > 0`,
        ]);
        expect({ loaded, preloaded }).toEqual({ loaded: "t", preloaded });
      } finally {
        await stopOwn(row, database, pgImageVariable);
      }
    } finally {
      rmSync(dir, { recursive: true, force: true });
      rmSync(stall, { recursive: true, force: true });
    }
  },
  // The provider's start and stop at its own bounds, as the image-mount rows state theirs.
  rowMs(oneStart)
);
