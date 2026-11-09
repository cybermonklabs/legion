/** Where the stubbed layer's pages are served from, read by e2e/playwright.config.ts (the `stub`
 *  project's `baseURL` and the `webServer` that serves it) and by e2e/stub/static-server.ts.
 *
 *  The port is fixed rather than chosen by the OS, because Playwright's `webServer` must be given
 *  a URL to poll before the server exists. It is below Linux's ephemeral range (32768-60999), so
 *  nothing the harness starts can be given it by accident: every harness listener binds port 0 and
 *  takes what the kernel hands it (e2e/harness-start.ts).
 *
 *  One server serves every worker. It holds no state - it answers files out of `web/dist` and
 *  nothing else - so four lanes reading it at once is contention on the kernel's accept queue and
 *  nothing more.
 *
 *  A committed recording carries no origin of its own (e2e/stub/recording.ts rewrites each URL to
 *  `recordedOrigin` as it is written), so changing this port breaks no recording: the fixture
 *  retargets every entry at the run's own base URL before it replays. */

/** The variable that moves the stub server off its default port, for a machine already holding it. */
export const stubPortVariable = "DISPATCH_E2E_STUB_PORT";

export const defaultStubPort = 4319;

export function stubPort(env: NodeJS.ProcessEnv = process.env): number {
  const named = (env[stubPortVariable] ?? "").trim();
  if (named === "") return defaultStubPort;
  const port = Number(named);
  if (!Number.isInteger(port) || port < 1 || port > 65_535) {
    throw new Error(
      `${stubPortVariable} must be a port number, not ${JSON.stringify(named)}. Unset it to serve ` +
        `the stubbed layer on ${defaultStubPort}.`
    );
  }
  return port;
}

export function stubOrigin(env: NodeJS.ProcessEnv = process.env): string {
  return `http://127.0.0.1:${stubPort(env)}`;
}

/** The origin every committed recording names. It is a placeholder, not an address: no server
 *  answers it, and the fixture replaces it with the run's own origin before replay. A recording
 *  that carried a real port would pin the stub server's port into every committed file. */
export const recordedOrigin = "http://dispatch.stub";
