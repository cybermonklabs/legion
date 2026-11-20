/** The image provider's start returns only once the container's final server serves, not the
 *  server the image's own initialisation runs first (pull request #4236).
 *
 *  The image's entrypoint (docker-library/postgres's docker-entrypoint.sh) makes a fresh cluster
 *  with a temporary server, which listens on the same unix socket as the final one. That server
 *  makes the stack's database, runs the scripts of /docker-entrypoint-initdb.d, and stops, and
 *  only then does the final server start. A start that took the first `select 1` that answered
 *  could take it from the temporary server, and hand its stack a socket that went away a moment
 *  later: `harness-postgres-image-mount.test.ts` failed one run in four so, at `create extension`,
 *  with "No such file or directory" on that socket.
 *
 *  The row widens that moment to seconds: it starts the provider from an image of its own, made
 *  from the one the run names with one init script that sleeps 5 s, and then reads, for the
 *  next 8 s, the start time of the server that answers. Every read must answer, and every
 *  answer must name one server. The image is made with `docker commit` rather than a build, so
 *  it needs no registry and no builder. Skipped without DISPATCH_E2E_PG_IMAGE outside CI, and
 *  failed without it in CI (`./postgres-provider`). */

import { expect, test } from "bun:test";
import { randomBytes } from "node:crypto";
import { mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

import { pgConnectionsVariable, pgImageVariable, stackPostgres } from "../harness-postgres";
import { removalBounds } from "../harness-postgres-container-removal";
import {
  docker,
  psql,
  removeAll,
  removeImage,
  removeWithin,
} from "./harness-postgres-leftover-fixtures";
import { ownStart, ownStop, startOwn, stopOwn } from "./harness-postgres-provider-children";
import { makeMs, rowMs, rowOf, type Waits } from "./harness-postgres-waits";
import { requirePostgres, skipsWithoutPostgres } from "./postgres-provider";

const skipped = skipsWithoutPostgres("e2e/__tests__/harness-postgres-image-ready.test.ts", [
  pgImageVariable,
]);

/** How long the init script of the row's image sleeps after the temporary server made the
 *  stack's database, in seconds. */
const initSleep = 5;
/** How long the row reads the server after the start returned: past the init script's sleep,
 *  and past the moment the temporary server stops and the final one starts. */
const watchMs = 8_000;

/** What the row waits for (`Waits`): a `docker create` and a `docker commit` of the row's own
 *  (`makes`), with the `docker cp` between them, a call of the daemon that writes a container, at
 *  the fixtures' time for such a call (`makeMs`); one start of the provider in the row's own
 *  process, at the provider's own bounds (`ownStart`); and 8 s of reads. Its `finally` then stops
 *  the stack (`ownStop`), removes the builder of its image, and removes the image, each within
 *  the start's own bound for a removal (`removalBounds.boundMs`): a fixture call that runs out
 *  the row's deadline leaves them their own time. */
const slowInit: Waits = {
  children: 0,
  reads: 0,
  makes: 2,
  bounds: [makeMs, watchMs],
  parts: [ownStart(pgImageVariable)],
  ends: [ownStop(pgImageVariable), removalBounds.boundMs, removalBounds.boundMs],
};

test.skipIf(skipped)(
  "image provider: a stack's Postgres answers from the moment its start returns, though the image's own init runs a script after it made the database",
  async () => {
    const row = rowOf(slowInit);
    const { value: image } = requirePostgres([pgImageVariable]);
    // Random bytes, never the pid, which a run in another PID namespace can share: the image's
    // tag and the builder's name are the row's alone, and the builder is removed by its id, or,
    // where the row's deadline stopped its create, by that name (`docker`).
    const own = randomBytes(8).toString("hex");
    const slow = `dispatch-e2e-pg-slow-init-${own}`;
    const dir = mkdtempSync(join(tmpdir(), "pg-image-ready-"));
    let builder = "";
    let made = false;
    try {
      const script = join(dir, "slow.sh");
      writeFileSync(script, `sleep ${initSleep}\n`);
      builder = (await docker(row, "create", "--name", `${slow}-builder`, image)).trim();
      await docker(row, "cp", script, `${builder}:/docker-entrypoint-initdb.d/slow.sh`);
      await docker(row, "commit", builder, slow);
      made = true;

      const provider = stackPostgres({
        dir,
        run: `slow-init-${own}`,
        env: {
          ...process.env,
          [pgImageVariable]: slow,
          [pgConnectionsVariable]: process.env[pgConnectionsVariable] ?? "100",
        },
      });
      if (provider === undefined) throw new Error(`${pgImageVariable} named no provider`);
      const database = await startOwn(
        row,
        provider,
        pgImageVariable,
        0,
        "the image provider's start of the row's stack"
      );
      try {
        // Each read names the server that answered by its start time, so a start that returned
        // with the temporary server shows as two servers even where no read fell in the moment
        // between the two, and a read that fell there shows as a refusal.
        const refused: string[] = [];
        const servers = new Set<string>();
        const until = Date.now() + watchMs;
        while (Date.now() < until) {
          const read = await psql(row, database.url, "select pg_postmaster_start_time()");
          if (read.status === 0) servers.add(read.stdout.trim());
          else refused.push(`${new Date().toISOString()} ${read.stderr.trim()}`);
        }
        expect({ refused, servers: servers.size }).toEqual({ refused: [], servers: 1 });
      } finally {
        await stopOwn(row, database, pgImageVariable);
      }
    } finally {
      // The builder goes at the row's end, after its reads, within a removal's own bound, which
      // the row's limit counts (`Waits.ends`), and every removal runs even where another fails
      // (`removeAll`). A daemon that many sessions shared took 17 and 21 minutes over this
      // removal on 2026-11-19, while it stalled; between the commit and the provider's start, with
      // no bound of its own, it used up the row's deadline. The image declares a volume, so the
      // builder's create made an anonymous one, which goes with it (`removeContainer`).
      await removeAll(
        () => builder !== "" && removeWithin(builder, `the builder ${slow}-builder`),
        () => made && removeImage(slow),
        () => rmSync(dir, { recursive: true, force: true })
      );
    }
  },
  // What the row waits for, summed by the fixtures' program (`rowMs`): its create, its copy and its
  // commit, the provider's start, the reads, the stop and the removals of the builder and the image.
  rowMs(slowInit)
);
