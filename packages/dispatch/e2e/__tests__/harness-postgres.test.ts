import { beforeAll, expect, test } from "bun:test";
import { chmodSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";

import { rowLimit, runBounded } from "./bounded-subprocess";

// What e2e/harness-postgres.ts's wait for a stack's Postgres (`ready`) does while the server
// refuses `select 1`, as one still starting up does. The wait has no bound of its own, so it ends
// on the server's answer; on the caller's abort, with an Interrupted that names the server it
// waited for; on the server's end, which the caller hands it (`gone`), with a refusal that says
// so; or on a psql that cannot be started at all, with a refusal that names psql and why. It
// writes a line as it begins and another as it ends (`announceWait`). Each row runs the wait in a
// child of its own, with a stand-in `psql` first on its PATH that answers each query as a server
// starting up does and exits 2, until it has refused `PSQL_ANSWER_AFTER` of them, where that is
// set, and then answers `1`; the psql row's PATH holds no psql at all. The stand-in sends its
// parent, the child, SIGUSR2 the first time it runs, so an abort lands while the wait is under way.
const module = (name: string) => fileURLToPath(new URL(`../${name}`, import.meta.url));
const stubs = temporaryDirectory("harness-postgres-");
/** A preload under which every read of `Date.now()` is ten minutes later than the one before, as
 *  on a machine so loaded that each try of the wait takes that long. */
const leapingClock = join(stubs, "leaping-clock.ts");

beforeAll(() => {
  writeFileSync(
    join(stubs, "psql"),
    `#!/bin/sh
[ -e "$PSQL_MARK" ] || { : > "$PSQL_MARK"; kill -USR2 "$PPID"; }
if [ -n "\${PSQL_ANSWER_AFTER:-}" ]; then
  echo refused >> "$PSQL_MARK"
  if [ "$(wc -l < "$PSQL_MARK")" -gt "$PSQL_ANSWER_AFTER" ]; then echo 1; exit 0; fi
fi
echo 'psql: error: connection to server at "127.0.0.1", port 9 failed: FATAL:  the database system is starting up' >&2
exit 2
`
  );
  chmodSync(join(stubs, "psql"), 0o755);
  writeFileSync(
    leapingClock,
    `const real = Date.now.bind(Date);
let ahead = 0;
Date.now = () => real() + (ahead += 600_000);
`
  );
});

/** Runs `ready` against the stand-in in a child, `gone` the body of the function it is handed,
 *  aborting on the stand-in's first call where `abort` is set, and answers what it threw or that
 *  the server answered. `answerAfter` is the stand-in's `PSQL_ANSWER_AFTER`, `preload` a preload
 *  of the child, and `path` its PATH in place of the stand-in's directory before the caller's. */
async function waited(
  gone: string,
  abort: boolean,
  mark: string,
  { answerAfter, path, preload }: { answerAfter?: number; path?: string; preload?: string } = {}
) {
  const script =
    `const { ready } = await import(${JSON.stringify(module("harness-postgres.ts"))});` +
    `const { Interrupted } = await import(${JSON.stringify(module("local-instance-support.ts"))});` +
    "const stopping = new AbortController();" +
    (abort
      ? 'process.once("SIGUSR2", () => stopping.abort());'
      : 'process.on("SIGUSR2", () => {});') +
    'try { await ready("postgres://postgres@127.0.0.1:9/none?sslmode=disable", ' +
    `"the stand-in's Postgres", () => ${gone}, stopping.signal); console.log('{"answered":true}'); }` +
    "catch (error) { console.log(JSON.stringify({ interrupted: error instanceof Interrupted, " +
    "message: error.message })); }";
  const run = await runBounded({
    argv: [process.execPath, ...(preload ? ["--preload", preload] : []), "--eval", script],
    bound: "short",
    env: {
      PATH: path ?? `${stubs}:${process.env.PATH ?? ""}`,
      PSQL_MARK: join(stubs, mark),
      ...(answerAfter === undefined ? {} : { PSQL_ANSWER_AFTER: String(answerAfter) }),
      TMPDIR: stubs,
    },
    from: import.meta.url,
  });
  expect(run.code, `${run.named} exited ${run.code}:\n${run.stderr}`).toBe(0);
  return {
    ...run,
    read: JSON.parse(run.stdout) as { answered?: true; interrupted?: boolean; message?: string },
  };
}

test(
  "a wait its caller aborts while the server refuses names the server it waited for",
  async () => {
    const { named, read, stderr, stdout } = await waited("false", true, "aborted");
    expect(read.interrupted, `${named} printed ${stdout}`).toBe(true);
    expect(read.message).toMatch(
      /^interrupted \d+\.\d s into the wait, before the stand-in's Postgres answered select 1$/
    );
    // The wait named what it waited for as it began, which is what a run killed outright leaves.
    expect(stderr).toContain(
      "e2e/harness-postgres.ts: waiting, with no bound of its own, until the stand-in's Postgres answers select 1\n"
    );
    // And wrote that it ended once the abort ended it.
    expect(stderr).toMatch(
      /e2e\/harness-postgres\.ts: no longer waiting \(after \d+\.\d s\) until the stand-in's Postgres answers select 1\n/
    );
  },
  rowLimit("short")
);

test(
  "a wait whose server has ended stops at the next refused try, naming it and the refusal",
  async () => {
    const { named, read, stdout } = await waited("true", false, "gone");
    expect(read.interrupted, `${named} printed ${stdout}`).toBe(false);
    expect(read.message).toStartWith(
      "The Dispatch e2e harness cannot start: the stand-in's Postgres stopped before it answered select 1: "
    );
    expect(read.message).toContain("the database system is starting up");
  },
  rowLimit("short")
);

// A psql that cannot be started would fail every later try the same way, with the server up and
// `gone` false, so the wait is refused at once, naming psql and why it could not start.
test(
  "a wait whose psql cannot be started is refused at once, naming why",
  async () => {
    const { named, read, stdout } = await waited("false", false, "no-psql", {
      path: join(stubs, "no-such-directory"),
    });
    expect(read.interrupted, `${named} printed ${stdout}`).toBe(false);
    expect(read.message).toStartWith(
      "The Dispatch e2e harness cannot start: psql, which asks the stand-in's Postgres for select 1, could not be started: "
    );
    // Bun says `Executable not found in $PATH: "psql"`, and Node `spawn psql ENOENT`.
    expect(read.message).toMatch(/Executable not found in \$PATH: "psql"|spawn psql ENOENT/);
  },
  rowLimit("short")
);

// The wait keeps no clock: a server refused three tries, each read ten minutes after the last,
// which a bound of 120 s failed at its second, and the wait still takes its answer.
test(
  "a wait under a clock ten minutes later at every read still takes the server's answer",
  async () => {
    const { named, read, stderr, stdout } = await waited("false", false, "late", {
      answerAfter: 3,
      preload: leapingClock,
    });
    expect(read, `${named} printed ${stdout}:\n${stderr}`).toEqual({ answered: true });
  },
  rowLimit("short")
);
