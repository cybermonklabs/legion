/** The image provider's Postgres serves though other programs hold ports, and its container
 *  publishes none (pull request #4236, acceptance finding D2).
 *
 *  Requirement R2 of that pull request: "For a program that cannot take a socket, the program
 *  binds a port that the kernel chooses. The stack reads the port from the program: from its
 *  output, or from the kernel's list of the sockets that the process holds."
 *
 *  Before that pull request, the image provider published `127.0.0.1::5432` and let Docker choose
 *  the host port. Docker does not ask the kernel: it hands out host ports in order after the last
 *  one it chose, tries ten of them, and then fails the `docker run` with `failed to bind host port
 *  127.0.0.1:<n>/tcp: address already in use` (dockerd 29.9.2 logs `Failed to allocate port`
 *  attempt=1..10). So another program that held the ports just past Docker's last choice failed
 *  the stack's Postgres. The provider now publishes no port: its server listens on no TCP port
 *  and serves on a unix socket in a bind mount (e2e/harness-postgres.ts).
 *
 *  The row holds 64 loopback ports the kernel chooses, as any other programs could, starts one
 *  stack's Postgres from the image, and requires that it serves and that its container publishes
 *  no port, so no port another program holds can fail it. It holds no port Docker would choose
 *  next: a hold of those failed every other `docker run -p` on the daemon while it lasted, and two
 *  runs of the row at once took them from each other.
 *  A developer's run without DISPATCH_E2E_PG_IMAGE skips it and says so on one line; a CI run
 *  never skips it, and fails naming the variable where the job sets none
 *  (`e2e/__tests__/postgres-provider.ts`). The `visual` job of
 *  .github/workflows/dispatch-browser.yaml runs it with that job's image, and the `dispatch-ui`
 *  job, which runs this directory with no provider, leaves it out (`postgres-provider.test.ts`). */

import { expect, test } from "bun:test";
import { execFileSync } from "node:child_process";
import { randomBytes } from "node:crypto";
import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

import { pgImageVariable, stackPostgres } from "../harness-postgres";
import { ownStart, ownStop, startOwn, stopOwn } from "./harness-postgres-provider-children";
import { rowMs, rowOf, type Waits } from "./harness-postgres-waits";
import { requirePostgres, skipsWithoutPostgres } from "./postgres-provider";

/** What the row waits for (`Waits`): one start of the image provider's Postgres in the row's own
 *  process, at the provider's own bounds (`ownStart`), and, in its `finally`, its stop
 *  (`ownStop`). Its psql and its `docker port` read a started server and its container within the
 *  room above them. */
const oneStart: Waits = {
  children: 0,
  reads: 0,
  bounds: [],
  parts: [ownStart(pgImageVariable)],
  ends: [ownStop(pgImageVariable)],
};

test.skipIf(
  skipsWithoutPostgres("e2e/__tests__/harness-postgres-image-port.test.ts", [pgImageVariable])
)(
  "the image provider's Postgres serves though other programs hold ports, and its container publishes none",
  async () => {
    const row = rowOf(oneStart);
    requirePostgres([pgImageVariable]);
    // Other programs on the machine: each listens on a port the kernel chose, for the whole row.
    const held = Array.from({ length: 64 }, () =>
      Bun.listen({ hostname: "127.0.0.1", port: 0, socket: { data() {} } })
    );
    const dir = mkdtempSync(join(tmpdir(), "pg-image-port-"));
    const run = `port-${randomBytes(8).toString("hex")}`;
    try {
      expect(new Set(held.map((listener) => listener.port)).size).toBe(64);
      const provider = stackPostgres({
        dir,
        run,
        env: {
          ...process.env,
          DISPATCH_E2E_PG_CONNECTIONS: process.env.DISPATCH_E2E_PG_CONNECTIONS ?? "100",
        },
      });
      if (provider === undefined) throw new Error("DISPATCH_E2E_PG_IMAGE named no provider");
      const database = await startOwn(row, provider, pgImageVariable, 0, "the row's stack's start");
      try {
        const answer = execFileSync("psql", [database.url, "-XAtc", "select 1"], {
          encoding: "utf8",
        });
        expect(answer.trim()).toBe("1");
        // The stack's container, by the name the provider gives it (`dispatch-e2e-pg-<run>-<index>`),
        // publishes no port: Docker chooses none for it, so no port another program holds can
        // fail its start. `docker port` prints one line for each published port; the 5432/tcp the
        // image exposes is not one where nothing publishes it.
        const published = execFileSync("docker", ["port", `dispatch-e2e-pg-${run}-0`], {
          encoding: "utf8",
        });
        expect(published.trim()).toBe("");
      } finally {
        await stopOwn(row, database, pgImageVariable);
      }
    } finally {
      for (const listener of held) listener.stop(true);
      rmSync(dir, { recursive: true, force: true });
    }
  },
  // What the row waits for, summed by the fixtures' program (`rowMs`): the provider's start and
  // stop, each at the provider's own bounds. The start on a runner pulls the image too, which the
  // `visual` job's runs have not timed apart. The row read 6.6 to 14.8 s with the image present,
  // two runs at once on a shared Linux machine under a load of about 430, on 2026-11-18.
  rowMs(oneStart)
);
