import { afterAll, beforeEach } from "bun:test";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories";
import { runDispatchCli } from "../dispatch-cli";
import { resetAdviceMemory } from "../dispatch-execute";

// The real `dispatch` CLI run against one fake Dispatch that records what it was sent, for the
// rows that check how `dispatch issue-update` writes a nullable field (`--clear-assignee` in
// `dispatch-asked-of.test.ts`, `--clear-parent` in `dispatch-clear-parent.test.ts`).

interface Recorded {
  readonly method: string;
  readonly path: string;
  readonly body: unknown;
}

/** DSP-1 as `GET /api/v1/issues/DSP-1` reads it, before a caller's overrides: open, with nobody
 *  holding it and no parent. */
const ISSUE = {
  key: "DSP-1",
  title: "Native workspace",
  status: "in_progress",
  labels: [],
  priority: null,
  assignee: null,
  route: null,
  parent: null,
  blocked_by: [],
  components: [],
  repo: null,
  resolved_repo: null,
  closed_at: null,
  external_links: [],
  artifacts: [],
};

/** The Dispatch bearer each command is handed. */
const TOKEN = fixtureSecret("dsp");

/** What one CLI run printed and the code it exited with. */
export interface CliRun {
  readonly code: number;
  readonly stdout: string;
  readonly stderr: string;
}

/** The CLI and the fake Dispatch it talks to, as {@link useIssueUpdateCli} returns them. */
export interface IssueUpdateCli {
  /** Runs `dispatch` with `argv` against the fake Dispatch. */
  run(argv: readonly string[]): Promise<CliRun>;
  /** What the fake Dispatch saw since the test began or {@link forget} ran, as `METHOD path`. */
  calls(): string[];
  /** The body of the one PATCH sent since the test began or {@link forget} ran. */
  patchBody(): Record<string, unknown>;
  /** Forgets every request recorded so far, so a second run in one test reads alone. */
  forget(): void;
}

/**
 * Starts a fake Dispatch serving DSP-1 (the fixture above with `overrides` applied) and returns
 * the CLI that runs against it. Call it at a test file's module scope: it registers that file's
 * `beforeEach`, which forgets the requests and gives each test a fresh state directory, and the
 * `afterAll` that stops the server, and Bun evaluates this module once per process, so a hook in
 * its body would take the scope of whichever file imported it first.
 */
export function useIssueUpdateCli(
  prefix: string,
  overrides: Partial<Record<keyof typeof ISSUE, unknown>> = {}
): IssueUpdateCli {
  const makeDirectory = temporaryDirectories(prefix);
  const issue = { ...ISSUE, ...overrides };
  const requests: Recorded[] = [];

  const server = Bun.serve({
    port: 0,
    async fetch(request) {
      const { pathname } = new URL(request.url);
      const text = await request.text();
      const body: unknown = text === "" ? undefined : JSON.parse(text);
      requests.push({ method: request.method, path: pathname, body });
      if (pathname === "/api/v1/issues/DSP-1" && request.method === "GET") {
        return Response.json(issue);
      }
      if (pathname === "/api/v1/issues/DSP-1" && request.method === "PATCH") {
        // The issue as the server stores it: each field the body names takes the body's value,
        // null included, and every other field keeps its old one.
        const sent = Object.entries((body ?? {}) as Record<string, unknown>).filter(
          ([field]) => field in issue
        );
        return Response.json({ ...issue, ...Object.fromEntries(sent) });
      }
      return Response.json({ error: `unexpected ${request.method} ${pathname}` }, { status: 500 });
    },
  });

  afterAll(() => server.stop(true));

  let stateDir = "";
  beforeEach(() => {
    requests.length = 0;
    stateDir = makeDirectory();
    resetAdviceMemory();
  });

  return {
    async run(argv) {
      let stdout = "";
      let stderr = "";
      const code = await runDispatchCli(
        argv,
        {
          HOME: stateDir,
          DISPATCH_URL: `http://127.0.0.1:${server.port}`,
          DISPATCH_TOKEN: TOKEN,
          DISPATCH_STATE_DIR: stateDir,
          DISPATCH_HOST: "omp",
          DISPATCH_SESSION_ID: "ses-1",
        },
        {
          stdout: (text) => {
            stdout += text;
          },
          stderr: (text) => {
            stderr += text;
          },
          readText: () => "",
          cwd: stateDir,
        }
      );
      return { code, stdout, stderr };
    },
    calls() {
      return requests.map((recorded) => `${recorded.method} ${recorded.path}`);
    },
    patchBody() {
      const patch = requests.find((recorded) => recorded.method === "PATCH");
      if (patch === undefined) throw new Error("no PATCH was sent");
      return patch.body as Record<string, unknown>;
    },
    forget() {
      requests.length = 0;
    },
  };
}
