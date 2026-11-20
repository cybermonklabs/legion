// The two rows of extensions/envoy.test.ts that count what dials nats.js's default 127.0.0.1:4222
// must count in each run, whatever else runs on the machine: two runs of that file at once (two
// agents on a shared dev box, or a developer's watch run beside a full `bun run test`), and a run
// beside another program that holds 127.0.0.1:4222 (a developer's local NATS). In neither case may
// a row fail or skip.
//
// No row here binds a fixed port. The second row's other program is a listener of its own on a
// port the kernel chose, and the child run is started with test/nats-default-port-held.ts, which
// in that process alone makes a bind of port 4222 fail with EADDRINUSE and sends each dial of port
// 4222 to that listener. So the child sees 4222 held, and no other run on the machine sees it.
//
// How each row ended is read from the child's JUnit report, not its console: under an AI agent
// (CLAUDECODE, AGENT and the like in the environment) `bun test` prints no line for a passed test.
//
// A row waits for its child runs to end, which is the event it reads; its limit and the child's
// own row limit are bounds for a run that is stuck, not for one that is slow, and a run a machine
// stalls for minutes ends inside both. What a row made is removed after the file's rows end, a
// row that ran out of its limit included, whose own `finally` never runs.
import { afterAll, expect, test } from "bun:test";
import { existsSync, mkdtempSync, readFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { nativesEnvironment } from "@legion/test-fixtures/natives-environment";

const packageDirectory = join(import.meta.dir, "..");
const heldPreload = join(packageDirectory, "test", "nats-default-port-held.ts");

/** How long one dial row of a child run may take before `bun test` fails it: a stuck row's bound,
 *  not bun's 5 s default. The rows take milliseconds, but a stall of the machine stops a row and
 *  not its clock: on a shared Linux machine under a load of 1,147, on 2026-11-17, a child run
 *  stood still for more than 120 s. */
const childRowMs = 300_000;
/** How long a row of this file waits for its child runs to end: a child stuck past its own rows'
 *  bound, `childRowMs`, and the start of a `bun test` of extensions/envoy.test.ts before it. */
const rowMs = 600_000;

/** The directory every child run writes its JUnit report into, one directory per run. */
const reports = mkdtempSync(join(tmpdir(), "dial-rows-"));
/** Each child run that has not ended. */
const running = new Set<{ kill(signal?: NodeJS.Signals | number): void }>();
afterAll(() => {
  for (const child of running) child.kill("SIGKILL");
  rmSync(reports, { recursive: true, force: true });
});

const rows = "nothing dials nats.js's default 127.0.0.1:4222";
const rowNames = [
  `with its NATS seed refused (envoy.natsUrls other than ENVOY_NATS_URL's), ${rows}`,
  `with no NATS server, ${rows}`,
];

type RowEnd = "passed" | "failed" | "skipped";

interface DialRowsRun {
  readonly code: number;
  readonly output: string;
  /** How each test of the child's JUnit report ended, by its name. */
  readonly rows: ReadonlyMap<string, RowEnd>;
}

const xmlEntities: Record<string, string> = { amp: "&", apos: "'", gt: ">", lt: "<", quot: '"' };

/** Each `<testcase>` of a JUnit report, by its name, with how it ended. */
function rowEnds(junit: string): Map<string, RowEnd> {
  const ends = new Map<string, RowEnd>();
  for (const [, name = "", body = ""] of junit.matchAll(
    /<testcase name="([^"]*)"[^>]*?(?:\/>|>([\s\S]*?)<\/testcase>)/g
  )) {
    const end: RowEnd = /<(failure|error)\b/.test(body)
      ? "failed"
      : /<skipped\b/.test(body)
        ? "skipped"
        : "passed";
    ends.set(
      name.replace(/&(amp|apos|gt|lt|quot);/g, (_, entity: string) => xmlEntities[entity] ?? ""),
      end
    );
  }
  return ends;
}

/**
 * One run of the two dial rows, as a developer runs them, in a process of its own; `args` go to
 * `bun test` before the file, and `env` over this process's environment. The run loads the
 * extension, and so @oh-my-pi/pi-natives, so it gets the worker count of nativesEnvironment: two
 * runs at once that each start one worker for each CPU of a large machine cannot all start them
 * under a limit of tasks.
 */
async function runDialRows(
  args: readonly string[] = [],
  env: Record<string, string> = {}
): Promise<DialRowsRun> {
  const reportDirectory = mkdtempSync(join(reports, "run-"));
  const report = join(reportDirectory, "rows.xml");
  const child = Bun.spawn(
    [
      process.execPath,
      "test",
      ...args,
      `--timeout=${childRowMs}`,
      "--reporter=junit",
      `--reporter-outfile=${report}`,
      "extensions/envoy.test.ts",
      "-t",
      rows,
    ],
    {
      cwd: packageDirectory,
      env: nativesEnvironment({ ...process.env, ...env }),
      stdout: "pipe",
      stderr: "pipe",
    }
  );
  running.add(child);
  try {
    const [stdout, stderr, code] = await Promise.all([
      new Response(child.stdout).text(),
      new Response(child.stderr).text(),
      child.exited,
    ]);
    const junit = existsSync(report) ? readFileSync(report, "utf8") : "";
    return { code, output: stdout + stderr, rows: rowEnds(junit) };
  } finally {
    running.delete(child);
    rmSync(reportDirectory, { recursive: true, force: true });
  }
}

function expectBothRowsCounted(run: DialRowsRun): void {
  const ends = rowNames.map((name) => {
    const end = run.rows.get(name);
    return `${name}: ${end === "passed" ? end : `${end ?? "not run"}; the run printed ${run.output}`}`;
  });
  expect(ends).toEqual(rowNames.map((name) => `${name}: passed`));
  expect(run.code).toBe(0);
}

test(
  "two runs of the dial rows started at the same moment both count dials: neither fails nor skips",
  async () => {
    const [first, second] = await Promise.all([runDialRows(), runDialRows()]);
    expectBothRowsCounted(first);
    expectBothRowsCounted(second);
  },
  rowMs
);

test(
  "the dial rows count dials while another program holds 127.0.0.1:4222",
  async () => {
    const rival = Bun.listen({ hostname: "127.0.0.1", port: 0, socket: { data() {} } });
    try {
      expectBothRowsCounted(
        await runDialRows(["--preload", heldPreload], {
          NATS_DEFAULT_PORT_RIVAL: String(rival.port),
        })
      );
    } finally {
      rival.stop(true);
    }
  },
  rowMs
);
