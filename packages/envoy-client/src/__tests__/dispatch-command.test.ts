import { describe, expect, test } from "bun:test";
import { dispatchToolSchema, dispatchToolSpecs, zodSchemaApi } from "@legion/contracts";
import { z } from "zod";
import { fakeDispatchFetch } from "../../test/fake-dispatch";
import { bearerCredential } from "../bearer";
import {
  COMMANDS_OUTSIDE_THE_SPECS,
  commandFlags,
  commandHelp,
  commandLine,
  commandName,
  commandsHelp,
  fieldFlag,
  flagName,
  parseCommand,
  toolForCommand,
} from "../dispatch-command";
import { executeDispatchTool } from "../dispatch-execute";
import { NotUtf8Error } from "../dispatch-file-text";
import { projectBody } from "./dispatch-command-surface-responses";

const noFiles = {
  readText: (path: string): string => {
    throw new Error(`unexpected read ${path}`);
  },
};

function shellWords(line: string): string[] {
  // Splits the POSIX single-quoted output of commandLine back into argv.
  const words: string[] = [];
  let current = "";
  let quoted = false;
  let inWord = false;
  for (let i = 0; i < line.length; i++) {
    const c = line[i];
    if (quoted) {
      if (c === "'") quoted = false;
      else current += c;
    } else if (c === "'") {
      quoted = true;
      inWord = true;
    } else if (c === "\\" && line[i + 1] === "'") {
      current += "'";
      i++;
    } else if (c === " ") {
      if (inWord) words.push(current);
      current = "";
      inWord = false;
    } else {
      current += c;
      inWord = true;
    }
  }
  if (inWord) words.push(current);
  return words;
}

/** The parts of a call's JSON schema the help cases read. */
interface SchemaNode {
  readonly description?: string;
  readonly items?: SchemaNode;
  readonly properties?: Readonly<Record<string, SchemaNode>>;
}

/**
 * The part of `help` under `flag`: from the flag's own line, `\n  --issue <text>` or
 * `\n  --multiple` for a flag with no value, up to the next flag's line or the end. Empty when no
 * line of `help` is the flag's.
 */
function flagSection(help: string, flag: string): string {
  const line = `\n  ${flag}`;
  const at = Math.max(help.indexOf(`${line} `), help.indexOf(`${line}\n`));
  if (at === -1) return "";
  const end = help.indexOf("\n  --", at + 1);
  return help.slice(at, end === -1 ? undefined : end);
}

describe("the dispatch command surface", () => {
  test("every spec's example survives commandLine then parseCommand unchanged", () => {
    for (const spec of dispatchToolSpecs) {
      const argv = shellWords(commandLine(spec.name, spec.example as Record<string, unknown>));
      expect(argv[0]).toBe("dispatch");
      const parsed = parseCommand(argv.slice(1), noFiles);
      expect(parsed).toEqual({ kind: "call", tool: spec.name, args: spec.example, dryRun: false });
    }
  });

  test("names map both ways for all 43 tools, and no two flags of one command collide", () => {
    expect(dispatchToolSpecs).toHaveLength(43);
    for (const spec of dispatchToolSpecs) {
      expect(toolForCommand(commandName(spec.name))).toBe(spec.name);
      const flags = commandFlags(spec.name);
      expect(new Set(flags).size).toBe(flags.length);
    }
    expect(commandName("dispatch_issue_update")).toBe("issue-update");
    expect(flagName("reply_to_ask")).toBe("reply-to-ask");
  });

  // A flag that takes JSON lists the keys of its object, or of each item of its list, from the
  // schema the call is checked against, each key on its own line with the description its schema
  // gives it, so a caller learns the field names and what each holds from --help rather than from
  // the refusal of a wrong one.
  test("every JSON flag's help lists each key its object or list item takes, with the key's description", () => {
    let checked = 0;
    for (const spec of dispatchToolSpecs) {
      const help = commandHelp(spec.name);
      const schema = z.toJSONSchema(dispatchToolSchema(spec, zodSchemaApi(z))) as SchemaNode;
      for (const [field, node] of Object.entries(schema.properties ?? {})) {
        const keys = node.properties ?? node.items?.properties;
        if (keys === undefined) continue;
        const flag = `--${flagName(field)}-json`;
        const section = flagSection(help, flag);
        expect(section, `${spec.name} ${field}`).toStartWith(`\n  ${flag} `);
        for (const [key, { description }] of Object.entries(keys)) {
          const told = `${spec.name} ${field}.${key}`;
          const line = `\n        ${key} (`;
          expect(section, told).toContain(line);
          checked++;
          if (description === undefined) continue;
          // `<key> (<type>): <description>`, the description on the key's own line, each later
          // line of it indented as every line of a flag's help is.
          const at = section.indexOf(line);
          const said = section.indexOf(`): ${description.replaceAll("\n", "\n      ")}`, at);
          expect(said, `${told} has no description after it`).toBeGreaterThan(at);
          expect(
            section.slice(at + 1, said),
            `${told}'s description is not on its own line`
          ).not.toContain("\n");
        }
      }
    }
    expect(checked).toBeGreaterThan(20);
  });

  // A command's help prints its tool's description, and under the flag that sets a field the
  // description the field's schema holds, so the help says what the spec says.
  test("every command's help prints its spec's description and each field's under its flag", () => {
    let checked = 0;
    for (const spec of dispatchToolSpecs) {
      const help = commandHelp(spec.name);
      expect(help, spec.name).toContain(`\n\n${spec.description}\n\n`);
      const schema = z.toJSONSchema(dispatchToolSchema(spec, zodSchemaApi(z))) as SchemaNode;
      for (const [field, { description }] of Object.entries(schema.properties ?? {})) {
        if (description === undefined) continue;
        const flag = fieldFlag(spec.name, field);
        const section = flagSection(help, flag);
        expect(section, `${spec.name} ${field}`).toStartWith(`\n  ${flag}`);
        expect(section, `${spec.name} ${field}`).toContain(
          description.replaceAll("\n", "\n      ")
        );
        checked++;
      }
    }
    expect(checked).toBeGreaterThan(20);
  });

  test("doc-edit --help lists each key an operation takes, with its type and its values", () => {
    const help = commandHelp("dispatch_doc_edit");
    expect(help).toContain("\n  --ops-json <json>\n");
    expect(help).toContain(
      [
        "Required.",
        "      Each item's keys:",
        "        op (required; replace|delete|insert|retype|move|delete_row|delete_column)",
      ].join("\n")
    );
    expect(help).toContain("\n        index (integer)");
    expect(help).toContain("\n        type (text)");
    expect(help).toContain("\n        attributes (any JSON)");
    expect(help).toContain("\n      Its keys:\n        document (text)");
    expect(help).toContain("\n        blocks (list of objects)");
    expect(help).toContain("\n          Each item's keys:\n            id (required; text)");
    expect(help).toContain("\n            token (required; text)");
  });

  test("dispatch project takes its key and name as one flag each", () => {
    expect(commandName("dispatch_project")).toBe("project");
    expect(commandFlags("dispatch_project")).toEqual(expect.arrayContaining(["--key", "--name"]));
    expect(parseCommand(["project", "--key", "SMOKE684", "--name", "Smoke"], noFiles)).toEqual({
      kind: "call",
      tool: "dispatch_project",
      args: { key: "SMOKE684", name: "Smoke" },
      dryRun: false,
    });
  });

  test("dispatch project-update takes the project and on or off as one flag each", () => {
    expect(commandName("dispatch_project_update")).toBe("project-update");
    expect(commandFlags("dispatch_project_update")).toEqual(
      expect.arrayContaining(["--project", "--proven-sessions"])
    );
    for (const setting of ["on", "off"]) {
      expect(
        parseCommand(["project-update", "--project", "CORE", "--proven-sessions", setting], noFiles)
      ).toEqual({
        kind: "call",
        tool: "dispatch_project_update",
        args: { project: "CORE", proven_sessions: setting },
        dryRun: false,
      });
    }
  });

  // --labels-add, repeated, is the call's labels_add, which adds labels and keeps the issue's
  // others; --label stays labels, the whole set. A command line whose --labels-add were dropped
  // would send an update naming no field.
  test("dispatch issue-update --labels-add, repeated, is the call's labels_add", () => {
    expect(commandFlags("dispatch_issue_update")).toEqual(
      expect.arrayContaining(["--label", "--labels-add"])
    );
    expect(
      parseCommand(
        [
          "issue-update",
          "--issue",
          "ACME-10",
          "--labels-add",
          "accessibility",
          "--labels-add",
          "api",
        ],
        noFiles
      )
    ).toEqual({
      kind: "call",
      tool: "dispatch_issue_update",
      args: { issue: "ACME-10", labels_add: ["accessibility", "api"] },
      dryRun: false,
    });
  });

  test("clearing, booleans, repeats, options and stdin", () => {
    const stdin = { readText: (path: string) => (path === "-" ? "line one\nline two\n" : "") };
    expect(
      parseCommand(["issue-update", "--issue", "LEGION-2", "--clear-priority"], noFiles)
    ).toMatchObject({ kind: "call", args: { issue: "LEGION-2", priority: null } });
    expect(parseCommand(["claim", "--issue", "LEGION-2", "--release"], noFiles)).toMatchObject({
      args: { issue: "LEGION-2", release: true },
    });
    expect(
      parseCommand(
        ["issue", "--project", "LEGION", "--title", "T", "--label", "a", "--label", "b"],
        noFiles
      )
    ).toMatchObject({ args: { labels: ["a", "b"] } });
    expect(
      parseCommand(
        [
          "ask",
          "--issue",
          "LEGION-2",
          "--question",
          "Q?",
          "--option",
          "Ship: costs a day",
          "--option",
          "Wait",
        ],
        noFiles
      )
    ).toMatchObject({
      args: { options: [{ label: "Ship", description: "costs a day" }, { label: "Wait" }] },
    });
    expect(
      parseCommand(["message", "--issue", "LEGION-2", "--body-file", "-"], stdin)
    ).toMatchObject({ args: { body: "line one\nline two\n" } });
    expect(
      parseCommand(
        ["issues", "--project", "LEGION", "--priority", "0", "--priority", "none"],
        noFiles
      )
    ).toMatchObject({ args: { priority: [0, null] } });
  });

  test("a single-value flag given twice is named twice whether or not its first value parsed", () => {
    expect(
      parseCommand(
        ["issue-update", "--issue", "LEGION-2", "--priority", "bad", "--priority", "3"],
        noFiles
      )
    ).toEqual({
      kind: "refused",
      tool: "dispatch_issue_update",
      problems: ['--priority must be a number, not "bad"', "--priority is given twice"],
    });
    expect(
      parseCommand(
        ["issue-update", "--issue", "LEGION-2", "--priority", "1", "--priority", "2"],
        noFiles
      )
    ).toEqual({
      kind: "refused",
      tool: "dispatch_issue_update",
      problems: ["--priority is given twice"],
    });
  });

  test("an option label holding the separator survives commandLine then parseCommand", () => {
    const args = {
      issue: "LEGION-2",
      question: "Q?",
      options: [
        { label: "b: with colon", description: "x" },
        { label: "Plain", description: "costs: a day" },
        { label: "c: no description" },
      ],
    };
    const argv = shellWords(commandLine("dispatch_ask", args));
    expect(parseCommand(argv.slice(1), noFiles)).toEqual({
      kind: "call",
      tool: "dispatch_ask",
      args,
      dryRun: false,
    });
    // Every label free of the separator keeps the readable flag.
    expect(
      commandLine("dispatch_ask", {
        issue: "LEGION-2",
        question: "Q?",
        options: [{ label: "Ship", description: "a: b" }],
      })
    ).toContain("--option 'Ship: a: b'");
  });

  test("an unknown flag and a missing value are refused together, naming the flags", () => {
    const parsed = parseCommand(["message", "--isue", "LEGION-2", "--body"], noFiles);
    expect(parsed.kind).toBe("refused");
    if (parsed.kind !== "refused") return;
    expect(parsed.problems.join("\n")).toContain("--isue");
    expect(parsed.problems.join("\n")).toContain("--body needs a value");
  });

  test("an unknown command is refused with the list of commands", () => {
    expect(parseCommand(["isue"], noFiles)).toMatchObject({ kind: "refused" });
  });

  test("dispatch --help lists every command as a row, and an unknown command's refusal names each", () => {
    const outside = COMMANDS_OUTSIDE_THE_SPECS.map(({ name }) => name);
    expect(outside).toEqual(["proxy", "login"]);
    const names = [...dispatchToolSpecs.map((spec) => commandName(spec.name)), ...outside];
    const rows = commandsHelp()
      .split("\n")
      .flatMap((line) => /^ {2}([a-z][a-z0-9-]*) {2,}\S/.exec(line)?.[1] ?? []);
    expect(rows).toEqual(names);
    // Each row of a command outside the specs is the first sentence of its own help.
    for (const { name, sentence, help } of COMMANDS_OUTSIDE_THE_SPECS) {
      expect(commandsHelp()).toContain(`  ${sentence}\n`);
      expect(help().split("\n\n")[1], name).toStartWith(sentence);
    }
    const refused = parseCommand(["isue"], noFiles);
    if (refused.kind !== "refused") throw new Error(`isue was not refused: ${refused.kind}`);
    expect(refused.problems.join("\n")).toContain(`the commands are: ${names.join(", ")}`);
  });

  test("a bare issue key first is the --issue of a command that takes --issue", () => {
    expect(parseCommand(["read", "ACME-12"], noFiles)).toEqual(
      parseCommand(["read", "--issue", "ACME-12"], noFiles)
    );
    expect(parseCommand(["read", "ACME-12"], noFiles)).toEqual({
      kind: "call",
      tool: "dispatch_read",
      args: { issue: "ACME-12" },
      dryRun: false,
    });
    expect(parseCommand(["message", "ACME-12", "--body", "hi"], noFiles)).toMatchObject({
      kind: "call",
      args: { issue: "ACME-12", body: "hi" },
    });
  });

  test("every command that takes --issue takes a bare key as its first argument", () => {
    const commands = dispatchToolSpecs
      .map((spec) => spec.name)
      .filter((tool) => commandFlags(tool).includes("--issue"))
      .map(commandName);
    expect(commands.sort()).toEqual(
      [
        "artifact",
        "ask",
        "claim",
        "comment",
        "doc-edit",
        "doc-read",
        "handoff-read",
        "handoff-write",
        "issue-update",
        "message",
        "plan-approve",
        "read",
        "request-approval",
        "review",
        "result",
        "results",
        "steps",
        "suggest",
      ].sort()
    );
    for (const command of commands) {
      const bare = parseCommand([command, "ACME-12", "--dry-run"], noFiles);
      expect(bare).toEqual(parseCommand([command, "--issue", "ACME-12", "--dry-run"], noFiles));
      expect(JSON.stringify(bare)).not.toContain("unexpected argument");
    }
  });

  test("a key-shaped word after the first argument is refused, not taken as --issue", () => {
    expect(parseCommand(["message", "--body", "hi", "SHA-256"], noFiles)).toEqual({
      kind: "refused",
      tool: "dispatch_message",
      problems: [
        "unexpected argument SHA-256: a bare issue key is the --issue only as the first argument; otherwise write --issue SHA-256",
      ],
    });
  });

  test("a lowercase issue key is refused", () => {
    expect(parseCommand(["read", "acme-12"], noFiles)).toEqual({
      kind: "refused",
      tool: "dispatch_read",
      problems: ["unexpected argument acme-12: every value follows its flag"],
    });
  });

  test("a bare issue key is refused by a command without --issue", () => {
    expect(parseCommand(["search", "--query", "q", "ACME-12"], noFiles)).toMatchObject({
      kind: "refused",
      tool: "dispatch_search",
    });
  });

  test("a bare issue key is refused when --issue is also given", () => {
    expect(parseCommand(["read", "--issue", "ACME-1", "ACME-12"], noFiles)).toEqual({
      kind: "refused",
      tool: "dispatch_read",
      problems: ["unexpected argument ACME-12: --issue is already ACME-1"],
    });
    const issueFile = {
      readText: (path: string): string => {
        if (path === "issue.txt") return "ACME-1";
        throw new Error(`unexpected read ${path}`);
      },
    };
    expect(parseCommand(["read", "--issue-file", "issue.txt", "ACME-12"], issueFile)).toEqual({
      kind: "refused",
      tool: "dispatch_read",
      problems: ["unexpected argument ACME-12: --issue-file already set --issue to ACME-1"],
    });
    expect(parseCommand(["read", "ACME-12", "--issue", "ACME-1"], noFiles)).toEqual({
      kind: "refused",
      tool: "dispatch_read",
      problems: ["the issue key ACME-12 and --issue both set issue"],
    });
  });

  test("a second bare issue key is refused", () => {
    expect(parseCommand(["read", "ACME-12", "ACME-13"], noFiles)).toEqual({
      kind: "refused",
      tool: "dispatch_read",
      problems: [
        "unexpected argument ACME-13: a bare issue key is the --issue only as the first argument; otherwise write --issue ACME-13",
      ],
    });
  });

  test("a bare value that is not an issue key is refused", () => {
    expect(parseCommand(["read", "hello"], noFiles)).toMatchObject({
      problems: ["unexpected argument hello: every value follows its flag"],
    });
  });

  test("handoff-write reads its body from a file or stdin unchanged, and handoff-read takes a version", () => {
    const body = '{"schemaVersion":1,\n  "phase": "implement"}\n';
    const files = { readText: (path: string) => (path === "-" || path === "h.json" ? body : "") };
    for (const source of ["h.json", "-"]) {
      expect(
        parseCommand(
          ["handoff-write", "DSP-1", "--phase", "implement", "--body-file", source],
          files
        )
      ).toEqual({
        kind: "call",
        tool: "dispatch_handoff_write",
        args: { issue: "DSP-1", phase: "implement", body },
        dryRun: false,
      });
    }
    expect(
      parseCommand(["handoff-read", "DSP-1", "--phase", "plan", "--version", "2"], noFiles)
    ).toEqual({
      kind: "call",
      tool: "dispatch_handoff_read",
      args: { issue: "DSP-1", phase: "plan", version: 2 },
      dryRun: false,
    });
  });
});

// `--run -` would send the one character "-" to a caller who meant stdin, which only `--run-file -`
// reads. So every text flag refuses a bare `-`, but `dispatch artifact --path`, whose value names
// a local file. Every other value passes as given.
describe("a bare - given to a text flag", () => {
  const commit = "1111111111111111111111111111111111111111";
  /** A result call with every required field, then `rest`. */
  const result = (...rest: string[]): string[] => [
    "result",
    "--issue",
    "DSP-1",
    "--check",
    "k1",
    "--role",
    "implementer",
    "--verdict",
    "pass",
    "--commit",
    commit,
    ...rest,
  ];
  const resultArgs = {
    issue: "DSP-1",
    check: "k1",
    role: "implementer",
    verdict: "pass",
    commit,
  };
  /** The problem a bare `-` given to `flag` is refused with. */
  const bareDash = (flag: string): string =>
    `${flag} - sends the one character "-". ${flag}-file <path> reads ${flag} from a file, and ${flag}-file - reads it from stdin. One flag of a call may read stdin.`;

  test("is refused once per flag, in command-line order, listing no flags", () => {
    expect(
      parseCommand(result("--run", "-", "--observed", "-", "--negative-control", "-"), noFiles)
    ).toEqual({
      kind: "refused",
      tool: "dispatch_result",
      problems: [bareDash("--run"), bareDash("--observed"), bareDash("--negative-control")],
      aboutAFlag: false,
    });
  });

  test("--run=- reads as --run -", () => {
    expect(parseCommand(result("--run=-"), noFiles)).toEqual({
      kind: "refused",
      tool: "dispatch_result",
      problems: [bareDash("--run")],
      aboutAFlag: false,
    });
    expect(parseCommand(result("--run=-"), noFiles)).toEqual(
      parseCommand(result("--run", "-"), noFiles)
    );
  });

  test("an enum flag is a text flag", () => {
    expect(
      parseCommand(
        [
          "result",
          "--issue",
          "DSP-1",
          "--check",
          "k1",
          "--role",
          "implementer",
          "--verdict",
          "-",
          "--commit",
          commit,
        ],
        noFiles
      )
    ).toEqual({
      kind: "refused",
      tool: "dispatch_result",
      problems: [bareDash("--verdict")],
      aboutAFlag: false,
    });
  });

  test("dispatch artifact --path - names a local file and passes", () => {
    expect(
      parseCommand(["artifact", "--issue", "DSP-1", "--name", "a.txt", "--path", "-"], noFiles)
    ).toEqual({
      kind: "call",
      tool: "dispatch_artifact",
      args: { issue: "DSP-1", name: "a.txt", path: "-" },
      dryRun: false,
    });
  });

  test("a list flag sends it, and a number or JSON flag refuses it as before, listing the flags", () => {
    expect(
      parseCommand(["issue", "--project", "DSP", "--title", "T", "--label", "-"], noFiles)
    ).toEqual({
      kind: "call",
      tool: "dispatch_issue",
      args: { project: "DSP", title: "T", labels: ["-"] },
      dryRun: false,
    });
    expect(
      parseCommand(["issue-update", "--issue", "DSP-1", "--priority", "-"], noFiles)
    ).toStrictEqual({
      kind: "refused",
      tool: "dispatch_issue_update",
      problems: ['--priority must be a number, not "-"'],
    });
    expect(
      parseCommand(["doc-edit", "--issue", "DSP-1", "--ops-json", "-"], noFiles)
    ).toStrictEqual({
      kind: "refused",
      tool: "dispatch_doc_edit",
      problems: [expect.stringMatching(/^--ops-json is not JSON: ./)],
    });
  });

  test("a - that --<field>-file read from a file or stdin passes", () => {
    const dashes = {
      readText: (path: string): string => {
        if (path === "f" || path === "-") return "-";
        throw new Error(`unexpected read ${path}`);
      },
    };
    expect(parseCommand(result("--run-file", "f", "--observed-file", "-"), dashes)).toEqual({
      kind: "call",
      tool: "dispatch_result",
      args: { ...resultArgs, run: "-", observed: "-" },
      dryRun: false,
    });
  });

  test("a longer value that starts with -, or holds - or | among other characters, passes as given", () => {
    expect(
      parseCommand(
        result(
          "--run",
          "-run 'A|B'",
          "--observed=--- PASS: X",
          "--negative-control",
          "go test -run 'A|B' | tee out"
        ),
        noFiles
      )
    ).toEqual({
      kind: "call",
      tool: "dispatch_result",
      args: {
        ...resultArgs,
        run: "-run 'A|B'",
        observed: "--- PASS: X",
        negative_control: "go test -run 'A|B' | tee out",
      },
      dryRun: false,
    });
  });

  test("beside any other problem, the refusal keeps both, in command-line order, and lists the flags", () => {
    const file = {
      readText: (path: string): string => {
        if (path === "f") return "x";
        throw new Error(`unexpected read ${path}`);
      },
    };
    expect(parseCommand(result("--run", "-", "--run", "-"), noFiles)).toStrictEqual({
      kind: "refused",
      tool: "dispatch_result",
      problems: [bareDash("--run"), "--run is given twice"],
    });
    expect(parseCommand(result("--run", "-", "--run-file", "f"), file)).toStrictEqual({
      kind: "refused",
      tool: "dispatch_result",
      problems: [bareDash("--run"), "--run and --run-file both set run"],
    });
    expect(parseCommand(["read", "--issue", "-", "ACME-12"], noFiles)).toStrictEqual({
      kind: "refused",
      tool: "dispatch_read",
      problems: [bareDash("--issue"), "unexpected argument ACME-12: --issue is already -"],
    });
    expect(parseCommand(result("--run", "-", "--runn", "x"), noFiles)).toStrictEqual({
      kind: "refused",
      tool: "dispatch_result",
      problems: [bareDash("--run"), "unknown flag --runn"],
    });
  });
});

describe("a file flag whose bytes are not UTF-8", () => {
  const bytes = Uint8Array.of(0x41, 0xff);
  const notUtf8 = {
    readText: (path: string): string => {
      if (path === "bad") throw new NotUtf8Error(bytes);
      throw new Error(`unexpected read ${path}`);
    },
  };

  test("is refused for the bytes, naming the field the flag sets, unless the argv is refused for a field given twice, in either order", () => {
    expect(
      parseCommand(["comment", "--issue", "DSP-1", "--body-file", "bad"], notUtf8)
    ).toStrictEqual({ kind: "unstorable", tool: "dispatch_comment", field: "body", bytes });
    expect(
      parseCommand(["comment", "--issue", "DSP-1", "--body-file", "bad", "--body", "x"], notUtf8)
    ).toStrictEqual({
      kind: "refused",
      tool: "dispatch_comment",
      problems: ["--body-file and --body both set body"],
    });
    expect(
      parseCommand(["comment", "--issue", "DSP-1", "--body", "x", "--body-file", "bad"], notUtf8)
    ).toStrictEqual({
      kind: "refused",
      tool: "dispatch_comment",
      problems: ["--body and --body-file both set body"],
    });
  });
});

// `dispatch project-update` turns a project's proven-sessions setting on with a PUT and off with a
// DELETE of `/api/v1/projects/{key}/proven-sessions`, and prints the setting as Dispatch answered
// it; `dispatch projects` marks each project that has it on. Each case drives `executeDispatchTool`
// over the shared fake Dispatch (`test/fake-dispatch.ts`) as a fetch.
describe("dispatch project-update and the proven-sessions line of dispatch projects", () => {
  const config = {
    enabled: true,
    url: "http://dispatch.test",
    credential: bearerCredential("secret"),
    error: null,
  };

  /** `tool` with `args` over a fake Dispatch that answers every request with `answer()`. */
  function run(tool: string, args: Record<string, unknown>, answer: () => Response) {
    const dispatch = fakeDispatchFetch({ handler: answer });
    const result = executeDispatchTool({
      tool,
      args,
      cwd: "/workspace",
      host: "omp",
      sessionId: "session-1",
      config,
      env: {},
      exec: async (file, argv) => {
        throw new Error(`unexpected command: ${file} ${argv.join(" ")}`);
      },
      fetchImpl: dispatch.fetchImpl,
    });
    return { result, requests: dispatch.calls };
  }

  test("on sends PUT /api/v1/projects/CORE/proven-sessions as the session, and prints the answered setting", async () => {
    const { result, requests } = run(
      "dispatch_project_update",
      { project: "CORE", proven_sessions: "on" },
      () => Response.json(projectBody(true))
    );

    const answered = await result;

    expect(requests.map((call) => `${call.method} ${call.url.pathname}`)).toEqual([
      "PUT /api/v1/projects/CORE/proven-sessions",
    ]);
    expect(JSON.parse(requests[0]?.body ?? "")).toMatchObject({
      actor: { kind: "session", id: "session-1" },
    });
    expect(answered.text).toBe("Project CORE (Core): proven sessions on.");
    expect(answered.details).toEqual({ project: "CORE", proven_sessions: true });
  });

  test("off sends DELETE /api/v1/projects/CORE/proven-sessions, and prints the answered setting", async () => {
    const { result, requests } = run(
      "dispatch_project_update",
      { project: "CORE", proven_sessions: "off" },
      () => Response.json(projectBody(false))
    );

    const answered = await result;

    expect(requests.map((call) => `${call.method} ${call.url.pathname}`)).toEqual([
      "DELETE /api/v1/projects/CORE/proven-sessions",
    ]);
    expect(answered.text).toBe("Project CORE (Core): proven sessions off.");
    expect(answered.details).toEqual({ project: "CORE", proven_sessions: false });
  });

  test("a refusal prints the server's code before its message", async () => {
    for (const [setting, code, message] of [
      ["off", "OAUTH_SCOPE_REQUIRED", "this route needs the person scope"],
      ["on", "PROJECT_SCOPE", "this token is scoped to project OPS"],
    ] as const) {
      const { result } = run(
        "dispatch_project_update",
        { project: "CORE", proven_sessions: setting },
        () => Response.json({ code, error: message }, { status: 403 })
      );

      await expect(result, setting).rejects.toThrow(`${code}: ${message}`);
    }
  });

  test("dispatch projects prints proven sessions on only for a project whose row has it", async () => {
    const { result } = run("dispatch_projects", {}, () =>
      Response.json([projectBody(true), { ...projectBody(false), key: "OPS", name: "Ops" }])
    );

    const answered = await result;

    expect(answered.text).toBe("CORE Core proven sessions on\nOPS Ops");
    expect(answered.details).toEqual({
      projects: [
        { key: "CORE", name: "Core" },
        { key: "OPS", name: "Ops" },
      ],
    });
  });
});
