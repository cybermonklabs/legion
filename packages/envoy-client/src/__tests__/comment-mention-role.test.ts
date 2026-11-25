import { beforeEach, describe, expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import { join } from "node:path";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories";
import { runDispatchCli } from "../dispatch-cli";
import { resetAdviceMemory } from "../dispatch-execute";

// `dispatch comment --mention-role <name>` mentions a role by its name alone: each value goes to
// Dispatch as {"target": "role:<name>"} in the comment's mentions, in argv order with any
// --mention, and an empty name is a usage error (exit 2) before anything is sent. The command runs
// whole, through runDispatchCli, against a stub Dispatch that records every request it gets.

const makeDirectory = temporaryDirectories("comment-mention-role");

/** The Dispatch bearer each command is handed. */
const TOKEN = fixtureSecret("dsp");

/** The repository's root, which the Dispatch docs pages sit under. */
const REPO_ROOT = join(import.meta.dir, "..", "..", "..", "..");

interface Recorded {
  readonly method: string;
  readonly path: string;
  readonly body: unknown;
}

const requests: Recorded[] = [];
let stateDir = "";

beforeEach(() => {
  requests.length = 0;
  stateDir = makeDirectory();
  // Each run is its own process in production: nothing the library remembers carries over.
  resetAdviceMemory();
});

/** A Dispatch that records each request and posts a comment on DSP-1; it answers nothing else.
 *  Its answer names the write's Idempotency-Key back, as Dispatch's keyed layer does on every
 *  answer to a keyed write, so the client reads it as this write's answer (docs/write-answers.md). */
const stubDispatch = (async (input: string | URL | Request, init?: RequestInit) => {
  const request = new Request(input, init);
  const { pathname } = new URL(request.url);
  const text = await request.text();
  requests.push({
    method: request.method,
    path: pathname,
    body: text === "" ? undefined : JSON.parse(text),
  });
  if (request.method === "POST" && pathname === "/api/v1/issues/DSP-1/comments") {
    const answer = Response.json({
      id: "comment-1",
      issue_key: "DSP-1",
      advice: { issue_status: "in_progress", session_writes_since_human: 0, your_open_asks: [] },
    });
    const key = request.headers.get("Idempotency-Key");
    if (key !== null) answer.headers.set("Idempotency-Key", key);
    return answer;
  }
  return Response.json({ error: "not found", code: "NOT_FOUND" }, { status: 404 });
}) as typeof fetch;

async function run(argv: readonly string[]): Promise<{ code: number; stdout: string }> {
  let stdout = "";
  const code = await runDispatchCli(
    argv,
    {
      HOME: stateDir,
      DISPATCH_HOST: "omp",
      DISPATCH_SESSION_ID: "ses-1",
      DISPATCH_URL: "http://127.0.0.1:9",
      DISPATCH_TOKEN: TOKEN,
      DISPATCH_STATE_DIR: stateDir,
    },
    {
      stdout: (text) => {
        stdout += text;
      },
      stderr: () => {},
      readText: (path) => readFileSync(path, "utf-8"),
      cwd: stateDir,
      fetchImpl: stubDispatch,
    }
  );
  return { code, stdout };
}

describe("dispatch comment --mention-role", () => {
  // R1: each value is one role target, in the order given, beside the --mention values the
  // command already sends, in argv order with them.
  test.each([
    [
      "two roles",
      ["--mention-role", "legion-acme-controller", "--mention-role", "legion-acme-architect"],
      ["role:legion-acme-controller", "role:legion-acme-architect"],
    ],
    [
      "two roles around a --mention",
      [
        "--mention-role",
        "legion-acme-controller",
        "--mention",
        "session:ses_01-AB",
        "--mention-role",
        "legion-acme-architect",
      ],
      ["role:legion-acme-controller", "session:ses_01-AB", "role:legion-acme-architect"],
    ],
  ] as const)("%s go to Dispatch as mentions, in the order given", async (_label, flags, targets) => {
    const { code, stdout } = await run([
      "comment",
      "--issue",
      "DSP-1",
      "--body",
      "Over to you.",
      ...flags,
    ]);

    expect(code, stdout).toBe(0);
    expect(stdout).toStartWith("Posted comment comment-1");
    const posted = requests.filter((request) => request.method === "POST");
    expect(posted).toEqual([
      {
        method: "POST",
        path: "/api/v1/issues/DSP-1/comments",
        body: expect.objectContaining({
          body: "Over to you.",
          mentions: targets.map((target) => ({ target })),
        }),
      },
    ]);
  });

  // R1: an empty name would send `role:` alone, which names nobody. It is a usage error, beside a
  // good name too, and nothing is sent: not the comment, not any read.
  test.each([
    ["an empty value", ["--mention-role", ""]],
    ["an empty inline value", ["--mention-role="]],
    ["a blank value", ["--mention-role", "  "]],
    [
      "an empty value after a good one",
      ["--mention-role", "legion-acme-controller", "--mention-role", ""],
    ],
  ] as const)("%s exits 2 and sends nothing", async (_label, flags) => {
    const { code, stdout } = await run([
      "comment",
      "--issue",
      "DSP-1",
      "--body",
      "Over to you.",
      ...flags,
    ]);

    expect(code).toBe(2);
    expect(stdout.split("\n")[0]).toBe("dispatch comment was not called: 1 problem");
    expect(stdout).toContain("- --mention-role needs a role's name, not an empty value\n");
    expect(requests).toEqual([]);
  });

  // A name written with its prefix would go out as `role:role:<name>`, which Dispatch's schema
  // refuses without naming the flag. The command refuses it here instead, naming both spellings
  // that work, and sends nothing.
  test("a name that already starts with role: exits 2, naming both spellings, and sends nothing", async () => {
    const { code, stdout } = await run([
      "comment",
      "--issue",
      "DSP-1",
      "--body",
      "Over to you.",
      "--mention-role",
      "role:legion-acme-controller",
    ]);

    expect(code).toBe(2);
    expect(stdout.split("\n")[0]).toBe("dispatch comment was not called: 1 problem");
    expect(stdout).toContain(
      "- --mention-role takes a role's name alone, so --mention-role role:legion-acme-controller would send role:role:legion-acme-controller: write --mention-role legion-acme-controller, or --mention role:legion-acme-controller\n"
    );
    expect(requests).toEqual([]);
  });

  // Two append flags of one field build one list; any other second setter of a field is still
  // refused, in either order: a clear beside an append, and a JSON list beside an append.
  test.each([
    [
      ["comment", "--issue", "DSP-1", "--body", "x", "--clear-mentions", "--mention-role", "a"],
      "--clear-mentions and --mention-role both set mentions",
    ],
    [
      ["comment", "--issue", "DSP-1", "--body", "x", "--mention-role", "a", "--clear-mentions"],
      "--mention-role and --clear-mentions both set mentions",
    ],
    [
      ["comment", "--issue", "DSP-1", "--body", "x", "--clear-mentions", "--mention", "role:a"],
      "--clear-mentions and --mention both set mentions",
    ],
    [
      [
        "ask",
        "--issue",
        "DSP-1",
        "--question",
        "q",
        "--options-json",
        '[{"label":"x"}]',
        "--option",
        "a",
      ],
      "--options-json and --option both set options",
    ],
    [
      ["issue", "--project", "DSP", "--title", "t", "--clear-labels", "--label", "a"],
      "--clear-labels and --label both set labels",
    ],
  ] as const)("%j is refused: %s", async (argv, problem) => {
    const { code, stdout } = await run(argv);

    expect(code).toBe(2);
    expect(stdout).toContain(`- ${problem}\n`);
    expect(requests).toEqual([]);
  });

  // R2: the command's help names the flag and says what a role mention reaches.
  test("dispatch comment --help names the flag and says the comment reaches the role's holder", async () => {
    const { code, stdout } = await run(["comment", "--help"]);

    expect(code).toBe(0);
    const lines = stdout.split("\n");
    const at = lines.indexOf("  --mention-role <name>");
    expect(at, stdout).toBeGreaterThan(-1);
    expect(lines[at + 1]).toContain(
      "Dispatch delivers the comment to the session that holds the role"
    );
    expect(requests).toEqual([]);
  });

  // R2: the guide for agents' Comments row names the flag.
  test("the guide for agents' Comments row names --mention-role", () => {
    const page = readFileSync(
      join(REPO_ROOT, "docs", "site", "src", "content", "docs", "dispatch", "for-agents.md"),
      "utf8"
    );
    const rows = page.split("\n").filter((line) => line.startsWith("| Comments |"));
    expect(rows).toHaveLength(1);
    expect(rows[0]).toContain("`--mention-role <name>`");
  });
});
