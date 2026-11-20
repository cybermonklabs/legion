/** Where the stubbed layer's pages are served from, read by e2e/playwright.config.ts (the `stub`
 *  projects' `baseURL`) and written by e2e/stub/server.ts, which starts the static file server.
 *
 *  The port is the kernel's: the server binds port 0 and prints what it was given, and
 *  `globalSetup` puts the origin in `stubOriginVariable` once the server serves. Every worker
 *  inherits that variable, as it inherits the stacks' manifest, and evaluates the config with it.
 *
 *  One server serves every worker. It holds no state - it answers files out of `web/dist` and
 *  nothing else - so four workers reading it at once is contention on the kernel's accept queue
 *  and nothing more.
 *
 *  A committed recording carries no origin of its own (e2e/stub/recording.ts rewrites each URL to
 *  `recordedOrigin` as it is written), so a port that changes from run to run breaks no
 *  recording: the fixture retargets every entry at the run's own base URL before it replays. */

/** The origin of the static file server this run started, `http://127.0.0.1:<port>`, which
 *  e2e/stub/server.ts sets once the server serves and removes when it stops it. */
export const stubOriginVariable = "DISPATCH_E2E_STUB_ORIGIN";

/** How the static file server's listening line begins: e2e/stub/static-server.ts prints it, then
 *  its URL and the directory it serves. It holds no character that a regular expression treats
 *  as special. */
export const stubListening = "dispatch stub static server: listening";

/** The whole listening line as a regular expression's source, which e2e/stub/server.ts and
 *  e2e/__tests__/stub-web-server.test.ts read the line with: group 1 is the port, group 2 the
 *  directory served. */
export const stubListeningLine = `${stubListening} http://127\\.0\\.0\\.1:(\\d+)/ over (.+)`;

/** The stubbed layer's origin. Before `globalSetup` has started the server - the config's first
 *  evaluation, in Playwright's runner, where no row runs - it answers port 0, as `e2e/api.ts`'s
 *  `baseUrl` does there. */
export function stubOrigin(env: NodeJS.ProcessEnv = process.env): string {
  return env[stubOriginVariable] ?? "http://127.0.0.1:0";
}

/** The origin every committed recording names. It is a placeholder, not an address: no server
 *  answers it, and the fixture replaces it with the run's own origin before replay. A recording
 *  that carried a real port would pin the stub server's port into every committed file. */
export const recordedOrigin = "http://dispatch.stub";
