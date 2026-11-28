import { beforeAll, describe, expect, test } from "bun:test";
import { writeFileSync } from "node:fs";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import { skipWithoutIgnoreMasks } from "@legion/test-fixtures/held-process";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";

import { ignoreMask } from "../signal-ignores";
import { rowLimit, runBounded } from "./bounded-subprocess";

// What e2e/signal-ignores.ts reads of the signals this process was started with ignored, and
// which stop listeners the harness arms from it (`armsStopListener`). Each process row runs a
// script of its own that imports the module, arms a SIGHUP listener of its own (which clears the
// mask's bit, so a read made after it would see nothing ignored), and prints what the module
// answers; the module reads the mask as it loads, before that listener. The script runs under a
// shell that ignores the row's signals first, as `trap ''`, `nohup` (SIGHUP) or a non-interactive
// shell's `&` job (SIGINT and SIGQUIT) do.
const withoutMasks = skipWithoutIgnoreMasks(
  "packages/dispatch/e2e/__tests__/signal-ignores.test.ts"
);
const module = fileURLToPath(new URL("../signal-ignores.ts", import.meta.url));
const stubs = temporaryDirectory("signal-ignores-");
const script = join(stubs, "read.ts");
/** A preload that has the module read a status file that does not exist, as on a machine with no
 *  /proc, so whether a signal was ignored is unknown to it. */
const unreadable = join(stubs, "unreadable.ts");

beforeAll(() => {
  writeFileSync(
    script,
    `import { armsStopListener, ignoredAtStart } from ${JSON.stringify(module)};
process.on("SIGHUP", () => {});
const answer: Record<string, unknown> = {};
for (const signal of ["SIGHUP", "SIGINT", "SIGQUIT", "SIGTERM"] as const) {
  answer[signal] = { ignored: ignoredAtStart(signal) ?? "unknown", armed: armsStopListener(signal) };
}
console.log(JSON.stringify(answer));
process.exit(0);
`
  );
  writeFileSync(
    unreadable,
    `import { plugin } from "bun";
import { readFileSync } from "node:fs";

plugin({
  name: "an unreadable /proc/self/status",
  setup(build) {
    build.onLoad({ filter: /\\/e2e\\/signal-ignores\\.ts$/ }, ({ path }) => {
      const source = readFileSync(path, "utf8");
      if (!source.includes('"/proc/self/status"')) throw new Error("no /proc/self/status read in " + path);
      return { contents: source.replace('"/proc/self/status"', '"/nonexistent/proc/self/status"'), loader: "ts" };
    });
  },
});
`
  );
});

/** What the script prints, started under a shell that ignores `ignores` first. */
async function readIn(ignores: string, preload?: string) {
  const run = await runBounded({
    argv: [
      "/bin/sh",
      "-c",
      `${ignores === "" ? "" : `trap '' ${ignores}; `}exec "$@"`,
      "sh",
      process.execPath,
      ...(preload ? ["--preload", preload] : []),
      script,
    ],
    bound: "short",
    env: { PATH: process.env.PATH ?? "", TMPDIR: stubs },
    from: import.meta.url,
  });
  expect(run.code, `${run.named} exited ${run.code}:\n${run.stderr}`).toBe(0);
  return JSON.parse(run.stdout) as Record<string, { ignored: boolean | "unknown"; armed: boolean }>;
}

const not = (armed: boolean) => ({ ignored: false, armed });

describe.skipIf(withoutMasks)("where the mask can be read", () => {
  test(
    "a process started ignoring SIGHUP and SIGQUIT reads both as ignored and arms no listener for them",
    async () => {
      expect(await readIn("HUP QUIT")).toEqual({
        SIGHUP: { ignored: true, armed: false },
        SIGINT: not(true),
        SIGQUIT: { ignored: true, armed: false },
        SIGTERM: not(true),
      });
    },
    rowLimit("short")
  );

  // A non-interactive shell's `&` job is started with SIGINT and SIGQUIT ignored, as
  // e2e/visual-run.sh and e2e/acceptance/with-harness.sh start e2e/harness-up.ts.
  test(
    "a process started ignoring SIGINT arms no SIGINT listener",
    async () => {
      expect((await readIn("INT")).SIGINT).toEqual({ ignored: true, armed: false });
    },
    rowLimit("short")
  );

  test(
    "a process started ignoring nothing reads nothing as ignored and arms every listener",
    async () => {
      expect(await readIn("")).toEqual({
        SIGHUP: not(true),
        SIGINT: not(true),
        SIGQUIT: not(true),
        SIGTERM: not(true),
      });
    },
    rowLimit("short")
  );
});

// Where the mask cannot be read the harness arms what it armed before it read one: SIGINT and
// SIGTERM, and no SIGHUP or SIGQUIT listener, which could undo a `nohup` it cannot see.
test(
  "where the mask cannot be read, every signal is unknown and only SIGINT and SIGTERM are armed",
  async () => {
    const unknown = (armed: boolean) => ({ ignored: "unknown" as const, armed });
    expect(await readIn("HUP QUIT", unreadable)).toEqual({
      SIGHUP: unknown(false),
      SIGINT: unknown(true),
      SIGQUIT: unknown(false),
      SIGTERM: unknown(true),
    });
  },
  rowLimit("short")
);

test("the mask is read from a SigIgn line, and is unknown without one", () => {
  expect(ignoreMask("Name:\tbun\nSigIgn:\t0000000000000003\nSigCgt:\t0\n")).toBe(3n);
  expect(ignoreMask("Name:\tbun\nSigCgt:\t0000000000000000\n")).toBeUndefined();
  expect(ignoreMask(undefined)).toBeUndefined();
});
