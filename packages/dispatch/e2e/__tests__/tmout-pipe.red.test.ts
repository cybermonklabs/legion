/** Red test for #4158 at 374db19cf0 (AcceptB7, round 4): an inherited `TMOUT` is bash's default
 *  timeout for `read`, so with `TMOUT=1` and a list on a pipe whose second line comes 2 s after its
 *  first, the one `read -d ''` of the list stops after a second. bash 5.2 keeps the partial input
 *  and the run probes the first line alone: it printed PASS while the second line's target was
 *  reachable. bash 3.2 drops the partial input and reads "lists no target". The run must read the
 *  whole list whatever TMOUT holds, so it reaches the second target and fails naming it. */

import { describe, expect } from "bun:test";
import {
  bash,
  closedPort,
  fakeDispatch,
  holdingTarget,
  lineFor,
  row,
  script,
  verdict,
  withoutTools,
} from "./outside-path-reach-fixtures";

describe("outside-path-reach.sh under an inherited TMOUT", () => {
  for (const tmout of ["1", "0.5"]) {
    row(withoutTools)(
      `reads the whole list on a pipe whose second line comes 2 s late, with TMOUT=${tmout}`,
      async () => {
        const origin = fakeDispatch();
        const hub = holdingTarget();
        try {
          const closed = await closedPort();
          const child = Bun.spawn(
            [
              bash,
              "-c",
              '{ printf "closed 127.0.0.1 %s\\n" "$1"; sleep 2; printf "hub 127.0.0.1 %s\\n" "$2"; } | "$3" "$4"',
              "red",
              String(closed),
              String(hub.port),
              bash,
              script,
            ],
            {
              env: {
                PATH: process.env.PATH ?? "",
                HOME: "/nonexistent",
                TMOUT: tmout,
                OUTSIDE_PATH_ALLOW_HTTP: "1",
                OUTSIDE_ORIGIN: `http://127.0.0.1:${origin.port}`,
                OUTSIDE_PATH_TARGETS: "/dev/stdin",
              },
              stdin: "ignore",
              stdout: "pipe",
              stderr: "pipe",
            }
          );
          const [code, stdout, stderr] = await Promise.all([
            child.exited,
            new Response(child.stdout).text(),
            new Response(child.stderr).text(),
          ]);
          const run = { code, stdout, stderr, seconds: 0 };
          expect(run.stdout).toContain("outside-path-reach: targets /dev/stdin (2)\n");
          expect(lineFor(run, "hub")).toContain(" REACHED; ");
          expect(verdict(run)).toBe("FAIL: reached hub");
          expect(run.code).toBe(1);
        } finally {
          origin.stop(true);
          hub.stop(true);
        }
      }
    );
  }
});
