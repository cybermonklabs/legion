/** Which runs of this config get the stubbed layer's static file server, and that the server
 *  starts whatever program holds the port a run names for it.
 *
 *  `globalSetup` starts `bun e2e/stub/static-server.ts` for a run that holds a stubbed row
 *  (e2e/stub/server.ts). Two jobs of `.github/workflows/dispatch-browser.yaml` run this config
 *  without a stubbed row, and one of them cannot start that command at all: `visual` runs
 *  Playwright inside the pinned container through `e2e/visual-run.sh`, and that image carries the
 *  three browser engines and node, not bun. A run that started the server there would die before
 *  its first row, and the pull-request lane skips `visual`, so no run on a pull request would say
 *  so. So the first row holds the config to its rule: a run that selects no stub project starts no
 *  server.
 *
 *  The server binds port 0 and the run reads its port from the server's own line. Before, it was
 *  Playwright's `webServer` on a fixed port, 4319 or the one `DISPATCH_E2E_STUB_PORT` named:
 *  Playwright tested the URL, then started the server, which bound the port later. So a second
 *  program on that port failed the run, either by answering the test (`http://127.0.0.1:4319/
 *  index.html is already used`) or by holding the port without listening, so the test found it
 *  free and the bind then failed (`Failed to start server. Is port 4319 in use?`). The second and
 *  third rows hold a port the kernel chose in those two ways, for the whole run, and name it in
 *  `DISPATCH_E2E_STUB_PORT`: a server that took that variable's port again would fail them, and no
 *  row takes a port that another program of the machine may want.
 *
 *  Each row runs the real Playwright runner over this config with a filter that matches no row and
 *  `--pass-with-no-tests`: the hooks still run, so the server starts and stops, and no browser
 *  starts. That run is a bounded child of the `short` bound: it starts no browser and no stack,
 *  and it reads about three seconds. The server serves a directory of this test's own
 *  (`DISPATCH_E2E_STUB_DIST`), so the rows need no build of `web/dist`. Each run writes its
 *  output in a directory of its own (`--output`), because Playwright clears a run's output
 *  directory at its start: the config's own, e2e/test-results, is in the tree that the listing of
 *  every other file of this directory walks, and `bun test --parallel` runs those listings beside
 *  this file, so a listing that met the directory as a run removed it failed with `ENOENT`. */

import { afterAll, expect, test } from "bun:test";
import { existsSync, mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath } from "node:url";

import { stubListeningLine } from "../stub/origin";

import { rowLimit, runBounded } from "./bounded-subprocess";
import { noHookLimit, removeScratch } from "./step-end";

const dispatchPackage = fileURLToPath(new URL("../..", import.meta.url));

/** The variable that named the stubbed layer's fixed port before its server bound port 0. */
const oldStubPortVariable = "DISPATCH_E2E_STUB_PORT";

const dist = mkdtempSync(join(tmpdir(), "stub-web-server-dist-"));
writeFileSync(join(dist, "index.html"), "<!doctype html><title>stub-web-server.test.ts</title>\n");
// The directory goes in a process of its own, which may take what the CI step leaves, or the limit
// of this file's rows, and the hook has no limit of Bun's: step-end.ts says why.
afterAll(() => removeScratch([dist], rowLimit("short")), noHookLimit);

/** The line the static file server prints once it serves. */
const listening = new RegExp(stubListeningLine, "g");

interface Run {
  readonly code: number;
  readonly output: string;
  /** The port on each listening line of the static file server, in order. */
  readonly served: readonly { readonly port: number; readonly dist: string }[];
}

/** A run of the config with `projects` that runs no row. `env` is all the environment it gets on
 *  top of `HOME` and `PATH`, so a variable of the machine's cannot change what it does. The run
 *  writes its output in a directory of its own, which it must hold `.last-run.json` in after. */
async function runOf(projects: readonly string[], env: Record<string, string> = {}): Promise<Run> {
  const outputDir = mkdtempSync(join(tmpdir(), "stub-web-server-output-"));
  try {
    const { code, named, stderr, stdout } = await runBounded({
      argv: [
        "bunx",
        "playwright",
        "test",
        "--config",
        "e2e/playwright.config.ts",
        ...projects.map((project) => `--project=${project}`),
        "--grep",
        "a title no row of this suite holds, 5d4c0a",
        "--pass-with-no-tests",
        "--output",
        outputDir,
      ],
      bound: "short",
      cwd: dispatchPackage,
      env: {
        DISPATCH_E2E_STUB_DIST: dist,
        HOME: process.env.HOME ?? "",
        PATH: process.env.PATH ?? "",
        ...env,
      },
      from: import.meta.url,
    });
    const output = `${named}\n${stdout}${stderr}`;
    expect(
      existsSync(join(outputDir, ".last-run.json")),
      `the run wrote no .last-run.json in its own output directory ${outputDir}, so it wrote ` +
        `its output where a listing of another file may be walking:\n${output}`
    ).toBe(true);
    const served = [...output.matchAll(listening)].map((match) => ({
      dist: match[2].trim(),
      port: Number(match[1]),
    }));
    return { code, output, served };
  } finally {
    rmSync(outputDir, { force: true, recursive: true });
  }
}

/** Asserts that a run of the stubbed layer passed with one static file server, which served this
 *  test's directory on a port other than `held`. */
function expectServedBeside(run: Run, held: number): void {
  expect(run.code, `the run of the stubbed layer exited ${run.code}:\n${run.output}`).toBe(0);
  expect(
    run.served,
    `the run printed no single listening line of the server:\n${run.output}`
  ).toHaveLength(1);
  expect(run.served[0].dist).toBe(dist);
  expect(run.served[0].port).not.toBe(held);
}

test(
  "a run that selects no stub project starts no static file server",
  async () => {
    // A run against servers already up, as `visual` is (e2e/visual-run.sh), so the hook starts no
    // stack either and the row needs no Postgres.
    const run = await runOf(["visual", "visual-phone"], { DISPATCH_E2E_REUSE_SERVERS: "1" });
    expect(run.code, `the run exited ${run.code}:\n${run.output}`).toBe(0);
    expect(
      run.served,
      "The containerised `visual` job runs this config with no bun on its PATH, so a run that " +
        "holds no stubbed row must start no static file server."
    ).toEqual([]);
  },
  rowLimit("short")
);

test(
  "the stubbed layer is served while another program serves on the port the run names for it",
  async () => {
    // Another program serving on a port the kernel chose, held by this row for the whole run.
    const rival = Bun.serve({
      fetch: () => new Response("another checkout's page\n"),
      hostname: "127.0.0.1",
      port: 0,
    });
    try {
      const held = rival.port ?? 0;
      expect(held).toBeGreaterThan(0);
      expectServedBeside(
        await runOf(["stub", "stub-phone"], { [oldStubPortVariable]: String(held) }),
        held
      );
    } finally {
      rival.stop(true);
    }
  },
  rowLimit("short")
);

test(
  "the stubbed layer is served while another program holds, without listening, the port the run names for it",
  async () => {
    // A socket bound and never listening, on a port the kernel chose, held for the whole run: a
    // test of the port finds nothing there, and a bind of it fails. That is a port taken between a
    // test and a bind. Python binds without listening; Bun and node bind only to listen. The row
    // drives that program and stops it, so it starts outside the bounded children.
    const rival = Bun.spawn(
      [
        "python3",
        "-c",
        "import socket, sys\n" +
          "s = socket.socket()\n" +
          's.bind(("127.0.0.1", 0))\n' +
          "print(s.getsockname()[1], flush=True)\n" +
          "sys.stdin.read()\n",
      ],
      { stdin: "pipe", stdout: "pipe" }
    );
    try {
      const reader = rival.stdout.getReader();
      const first = new TextDecoder().decode((await reader.read()).value);
      reader.releaseLock();
      const held = Number(first.trim());
      expect(Number.isInteger(held) && held > 0, `the holder printed ${first}`).toBe(true);
      expectServedBeside(
        await runOf(["stub", "stub-phone"], { [oldStubPortVariable]: String(held) }),
        held
      );
    } finally {
      rival.kill();
      await rival.exited;
    }
  },
  rowLimit("short")
);
