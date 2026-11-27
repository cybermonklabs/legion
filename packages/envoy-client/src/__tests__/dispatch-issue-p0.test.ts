import { afterAll, beforeEach, describe, expect, test } from "bun:test";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories";
import { runDispatchCli } from "../dispatch-cli";
import { resetAdviceMemory } from "../dispatch-execute";

// A priority of 0 needs the p0 scope: Dispatch refuses the create and the change that would make
// an issue's own priority P0 from any credential without it, 403 OAUTH_SCOPE_REQUIRED with the
// challenge naming p0, and writes nothing of the request. An issue with no priority of its own
// takes the priority of the nearest issue above it that has one, so a move under another parent,
// a clear of the issue's own priority and a new child with no priority can make work P0 too, and
// Dispatch refuses each the same way, unless the new child's writer is its P0 parent's architect
// (J2 of dispatch://INFRA-5793). The command adds no rule of its own: it sends the write, prints
// Dispatch's code and words, and its help says what the write needs. The stub answers each write
// as the server answers a credential without p0 (the refusals of
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

/** DSP-9, the P0 issue the move and the new child go under. */
const P0_PARENT = "DSP-9";

/** The words Dispatch refuses a move with that makes work P0, from a credential without p0. */
const MOVE_REFUSAL = `this request's credential does not hold the p0 scope, which this move needs: under ${P0_PARENT}, which reads P0, DSP-1 and each issue under it that takes its priority from it would read P0; a person makes the move at a browser, or gives p0 to a personal token in Settings, Agent tokens`;

/** The words Dispatch refuses a clear with that makes work P0, from a credential without p0. */
const CLEAR_REFUSAL =
  "this request's credential does not hold the p0 scope, which this clear needs: with no priority of its own, DSP-1 would take P0 from the issue above it; a person clears it at a browser, or gives p0 to a personal token in Settings, Agent tokens";

/** The words Dispatch refuses a new issue with no priority under a P0 issue with, from a
 *  credential without p0 whose session is not that issue's architect. */
const NEW_CHILD_REFUSAL = `this request's credential does not hold the p0 scope, which a new issue with no priority under ${P0_PARENT} needs, since ${P0_PARENT} reads P0: give it a priority from 1 to 3, file it as the architect of ${P0_PARENT}, or ask a person with p0`;

/** The refusal the stub answers a write with, by what the write carries: a priority of 0, else a
 *  create (a new child), else a clear, else a move. */
function refusalWords(method: string, body: Record<string, unknown>): string {
  if (body.priority === 0) return P0_REFUSAL;
  if (method === "POST") return NEW_CHILD_REFUSAL;
  return body.priority === null ? CLEAR_REFUSAL : MOVE_REFUSAL;
}

// The words a sentence of the help is read for: the ones the route index test reads in the routes'
// own text (`TestTheRouteIndexSaysWhenAMoveAClearOrANewChildNeedsP0`), so a row reads the words and
// not a copy of one sentence.
const MOVES = /\bmov(?:e|es|ing)\b/i;
const CLEARS = /\bclear(?:s|ing|ed)?\b/i;
const NEEDS_P0 = /\bneeds? (?:the )?p0\b/i;
/** A new child of a P0 issue: its parent, that the parent reads P0, that it needs p0, and the
 *  parent's architect, whose session files it without p0. */
const NEW_CHILD = [/\bparent\b/i, /\bP0\b|\b[Pp]riority (?:of )?0\b/, NEEDS_P0, /\barchitect\b/i];

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
    if (pathname === `/api/v1/issues/${P0_PARENT}` && request.method === "GET") {
      return Response.json({ ...ISSUE, key: P0_PARENT, title: "Rotate every key", priority: 0 });
    }
    const write =
      (pathname === "/api/v1/issues/DSP-1" && request.method === "PATCH") ||
      (pathname === "/api/v1/issues" && request.method === "POST");
    if (write) {
      return Response.json(
        {
          code: "OAUTH_SCOPE_REQUIRED",
          error: refusalWords(request.method, (body ?? {}) as Record<string, unknown>),
        },
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

/** The lines of a command's help that one flag prints: the flag's own line and the lines of its
 *  description under it, up to the next flag. */
function flagLines(help: string, flag: string): string[] {
  const lines = help.split("\n");
  const start = lines.findIndex((line) => line === `  ${flag}` || line.startsWith(`  ${flag} `));
  if (start === -1) throw new Error(`the help has no ${flag} row:\n${help}`);
  const next = lines.findIndex((line, index) => index > start && /^ {2}--/.test(line));
  return lines.slice(start, next === -1 ? undefined : next);
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
      expect(flagLines(stdout, "--priority").join("\n"), command).toMatch(/\bp0\b/);
    }
    expect(requests).toEqual([]);
  });
});

describe("a move, a clear or a new child that makes work P0 needs the p0 scope", () => {
  test("a move, a clear and a new child refused for want of p0 print the code and the scope, and exit 1", async () => {
    for (const [argv, sent, words] of [
      [
        ["issue-update", "--issue", "DSP-1", "--parent", P0_PARENT],
        { method: "PATCH", path: "/api/v1/issues/DSP-1", body: { parent: P0_PARENT } },
        MOVE_REFUSAL,
      ],
      [
        ["issue-update", "--issue", "DSP-1", "--clear-priority"],
        { method: "PATCH", path: "/api/v1/issues/DSP-1", body: { priority: null } },
        CLEAR_REFUSAL,
      ],
      [
        ["issue", "--project", "DSP", "--title", "Rotate the signing key", "--parent", P0_PARENT],
        { method: "POST", path: "/api/v1/issues", body: { parent: P0_PARENT } },
        NEW_CHILD_REFUSAL,
      ],
    ] as const) {
      requests.length = 0;
      const what = argv.join(" ");

      const result = await run(argv);

      expect(result.code, what).toBe(1);
      // One line, Dispatch's code and its words, which name the scope.
      expect(result.stdout, what).toBe(`OAUTH_SCOPE_REQUIRED: ${words}\n`);
      // The refusal is final: the write went once, with what it was given, and not again.
      expect(
        requests.filter((recorded) => recorded.method !== "GET"),
        what
      ).toEqual([{ ...sent, body: expect.objectContaining(sent.body) }]);
    }
  });

  // Each flag's description is read without the flag's own line, so `--parent` does not name the
  // parent for it. The clear is `--clear-priority`, whose row sits under `--priority`'s and whose
  // words come from the same field, so the two rows are read as one.
  test.each([
    ["issue-update", "--parent", "a move under it needs p0", [MOVES, NEEDS_P0]],
    ["issue-update", "--priority and --clear-priority", "a clear needs p0", [CLEARS, NEEDS_P0]],
    ["issue", "--parent", "a new child of a P0 issue needs p0, but from its architect", NEW_CHILD],
    [
      "issue",
      "--priority",
      "a new child of a P0 issue needs p0, but from its architect",
      NEW_CHILD,
    ],
  ] as const)("the help of %s, under %s, says that %s", async (command, flags, says, words) => {
    const { code, stdout, stderr } = await run([command, "--help"]);
    expect(stderr, command).toBe("");
    expect(code, command).toBe(0);
    const described = flags
      .split(" and ")
      .flatMap((flag) => flagLines(stdout, flag).slice(1))
      .map((line) => line.trim())
      .join(" ");
    const sentences = described.split(/[.!?](?:\s+|$)/);

    expect(
      sentences.filter((sentence) => words.every((word) => word.test(sentence))),
      `dispatch ${command} --help, under ${flags}: a sentence that says ${says} ` +
        `(one matching each of ${words.map(String).join(", ")}), among ${JSON.stringify(sentences)}`
    ).not.toEqual([]);
    expect(requests).toEqual([]);
  });
});
