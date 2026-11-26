import { afterAll, beforeEach, describe, expect, test } from "bun:test";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories";
import { runDispatchCli } from "../dispatch-cli";
import { resetAdviceMemory } from "../dispatch-execute";

// A priority of 0 needs the p0 scope: Dispatch refuses the create and the change that would make
// an issue's own priority P0 from any credential without it, 403 OAUTH_SCOPE_REQUIRED with the
// challenge naming p0, and writes nothing of the request. The command adds no rule of its own: it
// sends the write, prints Dispatch's code and words, and its help says what the write needs. The
// stub answers both writes as the server answers a credential without p0 (the refusal of
// `packages/envoy/internal/dispatch/api/p0_scope.go`), in the shape of `dispatch-review.test.ts`:
// one stub server driven through `runDispatchCli`, with a temporary state directory per row.

const makeDirectory = temporaryDirectories("dispatch-issue-p0");

/** The Dispatch bearer each command is handed. */
const TOKEN = fixtureSecret("dsp");

/** The words Dispatch refuses a priority of 0 with, from a credential that does not hold p0. */
const P0_REFUSAL =
  "this request's credential does not hold the p0 scope, which a priority of 0 needs: a person sets P0 at a browser, or gives p0 to a personal token in Settings, Agent tokens";

/** The challenge that refusal carries, naming the scope the request lacks. */
const P0_CHALLENGE = 'Bearer error="insufficient_scope", scope="p0"';

interface Recorded {
  readonly method: string;
  readonly path: string;
  readonly body: unknown;
}

const requests: Recorded[] = [];

/** DSP-1 as `GET /api/v1/issues/DSP-1` reads it: open, at priority 2. */
const ISSUE = {
  key: "DSP-1",
  title: "Native workspace",
  status: "in_progress",
  labels: [],
  priority: 2,
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

const server = Bun.serve({
  port: 0,
  async fetch(request) {
    const { pathname } = new URL(request.url);
    const text = await request.text();
    const body: unknown = text === "" ? undefined : JSON.parse(text);
    requests.push({ method: request.method, path: pathname, body });
    if (pathname === "/api/v1/issues/DSP-1" && request.method === "GET") {
      return Response.json(ISSUE);
    }
    const write =
      (pathname === "/api/v1/issues/DSP-1" && request.method === "PATCH") ||
      (pathname === "/api/v1/issues" && request.method === "POST");
    if (write) {
      return Response.json(
        { code: "OAUTH_SCOPE_REQUIRED", error: P0_REFUSAL },
        { status: 403, headers: { "WWW-Authenticate": P0_CHALLENGE } }
      );
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

async function run(argv: readonly string[]): Promise<{
  code: number;
  stdout: string;
  stderr: string;
}> {
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
}

/** The `--priority` row of a command's help: the flag's own line and the lines of its
 *  description under it, up to the next flag. */
function priorityRow(help: string): string {
  const lines = help.split("\n");
  const start = lines.findIndex((line) => /^ {2}--priority\b/.test(line));
  if (start === -1) throw new Error(`the help has no --priority row:\n${help}`);
  const next = lines.findIndex((line, index) => index > start && /^ {2}--/.test(line));
  return lines.slice(start, next === -1 ? undefined : next).join("\n");
}

describe("a priority of 0 needs the p0 scope", () => {
  test("a P0 write refused for want of p0 prints the code and the scope, and exits 1", async () => {
    for (const [argv, sent] of [
      [
        ["issue-update", "--issue", "DSP-1", "--priority", "0"],
        { method: "PATCH", path: "/api/v1/issues/DSP-1" },
      ],
      [
        ["issue", "--project", "DSP", "--title", "Rotate the signing key", "--priority", "0"],
        { method: "POST", path: "/api/v1/issues" },
      ],
    ] as const) {
      requests.length = 0;
      const what = argv.join(" ");

      const result = await run(argv);

      expect(result.code, what).toBe(1);
      expect(result.stdout, what).toBe(`OAUTH_SCOPE_REQUIRED: ${P0_REFUSAL}\n`);
      // Dispatch's refusal is final: the write went once, with the priority it was given, and
      // not again.
      expect(
        requests.filter((recorded) => recorded.method !== "GET"),
        what
      ).toEqual([{ ...sent, body: expect.objectContaining({ priority: 0 }) }]);
    }
  });

  test("the help of issue and issue-update says a priority of 0 needs p0", async () => {
    for (const command of ["issue", "issue-update"]) {
      const { code, stdout, stderr } = await run([command, "--help"]);

      expect(stderr, command).toBe("");
      expect(code, command).toBe(0);
      expect(priorityRow(stdout), command).toMatch(/\bp0\b/);
    }
    expect(requests).toEqual([]);
  });
});
