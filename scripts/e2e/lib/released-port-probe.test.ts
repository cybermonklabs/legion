/** No tracked file hands a server the port of a probe socket that it has already closed.
 *
 *  R1 of PR #4236: "A test stack starts each server of this repository on a socket that the stack
 *  holds. It holds the socket from the choice of the port until the server serves on it." R2:
 *  "For a program that cannot take a socket, the program binds a port that the kernel chooses.
 *  The stack reads the port from the program: from its output, or from the kernel's list of the
 *  sockets that the process holds."
 *
 *  A command substitution that asks Python for a port, prints it and closes the socket hands the
 *  caller a port nothing holds: another program can take it before the server the caller starts on
 *  it binds it, which is the fault this pull request removes. At 919bdcbef8,
 *  packages/envoy/scripts/airlock-check.sh does that three times: k4's .svc stand-in (line 797)
 *  and k7's broker, its API and its proxy port (lines 1359-1360). The broker takes port 0 for each
 *  of its addresses and logs what it bound (packages/envoy/scripts/dev-airlock.sh, "a port the
 *  system picks (BROKER_*_LISTEN_ADDR of port 0)"); `python3 -m http.server 0` prints the port it
 *  serves.
 *
 *  The row reads every tracked file that names `getsockname` (`git grep -l`) and fails on each
 *  line that prints the port of a socket it closes in the same probe. A read that sees nothing
 *  passes nothing: the row fails when `held-port.py`, which names `getsockname`, is not among the
 *  files it read. */

import { expect, test } from "bun:test";
import { execFileSync } from "node:child_process";
import { readFileSync } from "node:fs";
import { join } from "node:path";
import { command, rowMs, rowOf } from "@legion/test-fixtures/row-waits";

const root = execFileSync("git", ["rev-parse", "--show-toplevel"], {
  cwd: import.meta.dir,
  encoding: "utf8",
}).trim();

// One probe: a socket bound to port 0, its port printed, and the socket closed, in one Python
// program on one line, whatever the variable is called.
const releasedProbe =
  /bind\(\(\s*["'][^"']*["']\s*,\s*0\s*\)\)[^\n]*getsockname\(\)\[1\][^\n]*\.close\(\)/;

/** What the row waits for, which its limit is the sum of (`rowMs` of
 *  `@legion/test-fixtures/row-waits`): one `git grep`, a process of its own. */
const theGrep = command("git grep for getsockname");
const oneGrep = [theGrep];

test(
  "no tracked file hands a server a port from a probe socket it has already closed",
  () => {
    rowOf(oneGrep).charge(theGrep);
    const probing = execFileSync("git", ["grep", "-lz", "getsockname"], {
      cwd: root,
      encoding: "utf8",
    })
      .split("\0")
      .filter((file) => file !== "" && !file.endsWith("released-port-probe.test.ts"));
    expect(probing).toContain("scripts/e2e/lib/held-port.py");
    const hits: string[] = [];
    for (const file of probing) {
      readFileSync(join(root, file), "utf8")
        .split("\n")
        .forEach((line, index) => {
          if (releasedProbe.test(line)) hits.push(`${file}:${index + 1}`);
        });
    }
    expect(hits).toEqual([]);
  },
  rowMs(oneGrep)
);
