/** The stubbed layer's static file server (e2e/stub/static-server.ts): `e2e/global-setup.ts`
 *  starts it for a run that holds a stubbed row, and `e2e/global-teardown.ts` stops it.
 *
 *  It binds port 0 and prints the port the kernel gave it, which `startReporting`
 *  (e2e/harness-start.ts) reads as it reads every listener of the harness. So the server holds its
 *  port from the moment the port is chosen, and no other program can take the port first.
 *  Playwright's `webServer`, which started it before, cannot work that way: it tests a URL before
 *  it starts its command, so the port had to be fixed, and a second checkout's run on the same
 *  machine either answered that test or took the port between the test and the bind.
 *
 *  A run is given the server exactly when it holds a stubbed row and does not record
 *  (e2e/stub/selection.ts): a recording run meets the real harness, and the containerised
 *  `visual` job runs this config where there is no bun to start the server with. The origin goes
 *  to the workers in `stubOriginVariable` (e2e/stub/origin.ts). */
import { fileURLToPath } from "node:url";

import type { ReportingListener } from "../harness-listeners";
import { startReporting } from "../harness-start";
import { Supervisor } from "../local-instance-support";
import { stubListeningLine, stubOriginVariable } from "./origin";
import type { StubSelection } from "./selection";

/** How long the server may take to print its line: the 60 s Playwright's `webServer` gave it on
 *  main (`timeout: 60_000`), which this start keeps. A start of `bun` under a load of about 1,200
 *  on a shared machine took more than 30 s. */
const staticServerBootMs = 60_000;

// No health path: `Bun.serve` returns once the server listens, and the server prints its line
// after that, so a server that has printed its line answers.
const staticServer: ReportingListener = {
  argv: ["bun", fileURLToPath(new URL("./static-server.ts", import.meta.url))],
  bootMs: staticServerBootMs,
  id: "stub-static-server",
  name: "the stubbed layer's static file server",
  report: new RegExp(`^${stubListeningLine}`),
};

/** The server's supervisor while it runs. */
let running: Supervisor | undefined;

/** Starts the server for a run that `stub` says needs it, and returns once it serves, with its
 *  origin in `stubOriginVariable`. A start that fails stops the server before it throws. */
export async function startStubServer(stub: StubSelection): Promise<void> {
  if (!stub.holdsStubRow || stub.recording) return;
  running = new Supervisor();
  try {
    const port = await startReporting(staticServer, running);
    process.env[stubOriginVariable] = `http://127.0.0.1:${port}`;
  } catch (error) {
    await stopStubServer();
    throw error;
  }
}

/** Stops the server, if this process started one, and removes its origin, so a second setup in
 *  this process (Playwright's UI mode) starts a server of its own. */
export async function stopStubServer(): Promise<void> {
  const supervisor = running;
  running = undefined;
  delete process.env[stubOriginVariable];
  await supervisor?.stop();
}
