/** The image provider's start in a run directory whose path holds a character that a
 *  `docker create` argument or a Postgres connection string reads as syntax.
 *
 *  The provider bind-mounts a socket directory under the run directory into its container
 *  (e2e/harness-postgres.ts). A `--volume <path>:/var/run/postgresql` splits at each `:`, so a
 *  `TMPDIR` that holds one failed every start with `docker: Error response from daemon: invalid
 *  mode: /var/run/postgresql`. `--mount` reads its value as one CSV record, so its `source=`
 *  field is quoted, which is what lets a `"` stand in the path. A `,` cannot stand in any socket
 *  directory a stack names: psql (libpq) and the Dispatch server (pgx) both read the `host` of a
 *  `DATABASE_URL` as a list split at each `,`. So a run directory that holds one puts the socket
 *  in /tmp, as a run directory too long for a socket path does.
 *
 *  Each row makes its run directory under /tmp, not under `TMPDIR`: the socket must stay in the
 *  directory that holds the character, and a long `TMPDIR` would move it to /tmp and leave the
 *  row proving nothing. So the row reads where the socket went before it says the stack served.
 *  A developer's run without DISPATCH_E2E_PG_IMAGE skips it and says so on one line; a CI run
 *  never skips it, and fails naming the variable where the job sets none
 *  (`e2e/__tests__/postgres-provider.ts`). The `visual` job of
 *  .github/workflows/dispatch-browser.yaml runs it with that job's image, and the `dispatch-ui`
 *  job, which runs this directory with no provider, leaves it out (`postgres-provider.test.ts`). */

import { expect, test } from "bun:test";
import { mkdirSync, mkdtempSync, rmSync } from "node:fs";
import { join } from "node:path";

import { pgImageVariable, stackPostgres } from "../harness-postgres";
import { sqlAt } from "../psql";
import { ownStart, ownStop, startOwn, stopOwn } from "./harness-postgres-provider-children";
import { rowMs, rowOf, type Waits } from "./harness-postgres-waits";
import { requirePostgres, skipsWithoutPostgres } from "./postgres-provider";

const skipped = skipsWithoutPostgres("e2e/__tests__/harness-postgres-image-mount.test.ts", [
  pgImageVariable,
]);

const places = [
  { component: "c:olon", socketInRunDirectory: true },
  { component: 'q"uote', socketInRunDirectory: true },
  { component: "c,omma", socketInRunDirectory: false },
] as const;

/** What each row waits for (`Waits`): one start of the image provider's Postgres in the row's own
 *  process, at the provider's own bounds (`ownStart`), and, in its `finally`, its stop
 *  (`ownStop`). Its psql answers a started server within the room above them. */
const oneStart: Waits = {
  children: 0,
  reads: 0,
  bounds: [],
  parts: [ownStart(pgImageVariable)],
  ends: [ownStop(pgImageVariable)],
};

for (const [index, place] of places.entries()) {
  test.skipIf(skipped)(
    `the image provider's Postgres serves from a run directory that holds ${place.component}`,
    async () => {
      const row = rowOf(oneStart);
      requirePostgres([pgImageVariable]);
      const root = mkdtempSync(join("/tmp", "pg-mount-"));
      const dir = join(root, place.component);
      mkdirSync(dir);
      try {
        const provider = stackPostgres({
          dir,
          run: `mount-${process.pid}-${index}`,
          env: {
            ...process.env,
            DISPATCH_E2E_PG_CONNECTIONS: process.env.DISPATCH_E2E_PG_CONNECTIONS ?? "100",
          },
        });
        if (provider === undefined) throw new Error("DISPATCH_E2E_PG_IMAGE named no provider");
        const database = await startOwn(
          row,
          provider,
          pgImageVariable,
          0,
          `the start in ${place.component}`
        );
        try {
          expect(database.address.startsWith(`${dir}/`)).toBe(place.socketInRunDirectory);
          expect(database.address).not.toContain(",");
          expect(await sqlAt(database.url, ["select 1"])).toBe("1");
        } finally {
          await stopOwn(row, database, pgImageVariable);
        }
      } finally {
        rmSync(root, { recursive: true, force: true });
      }
    },
    // What the row waits for, summed by the fixtures' program (`rowMs`): the provider's start and
    // stop, each at the provider's own bounds. A first row on a runner pulls the image too, which
    // the `visual` job's runs have not timed apart. The three rows read 2.7, 3.4 and 3.4 s with
    // the image present, on a shared Linux machine under a load of about 200, on 2026-11-17.
    rowMs(oneStart)
  );
}
