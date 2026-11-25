import { describe, expect, test } from "bun:test";
import { dispatchToolSchema, dispatchToolSpecs, zodSchemaApi } from "@legion/contracts";
import { z } from "zod";
import {
  commandFlags,
  commandHelp,
  commandLine,
  commandName,
  flagName,
  parseCommand,
  toolForCommand,
} from "../dispatch-command";

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

describe("the dispatch command surface", () => {
  test("every spec's example survives commandLine then parseCommand unchanged", () => {
    for (const spec of dispatchToolSpecs) {
      const argv = shellWords(commandLine(spec.name, spec.example as Record<string, unknown>));
      expect(argv[0]).toBe("dispatch");
      const parsed = parseCommand(argv.slice(1), noFiles);
      expect(parsed).toEqual({ kind: "call", tool: spec.name, args: spec.example, dryRun: false });
    }
  });

  test("names map both ways for all 41 tools, and no two flags of one command collide", () => {
    expect(dispatchToolSpecs).toHaveLength(41);
    for (const spec of dispatchToolSpecs) {
      expect(toolForCommand(commandName(spec.name))).toBe(spec.name);
      const flags = commandFlags(spec.name);
      expect(new Set(flags).size).toBe(flags.length);
    }
    expect(commandName("dispatch_issue_update")).toBe("issue-update");
    expect(flagName("reply_to_ask")).toBe("reply-to-ask");
  });

  // A flag that takes JSON lists the keys of its object, or of each item of its list, from the
  // schema the call is checked against, so a caller learns the field names from --help rather
  // than from the refusal of a wrong one.
  test("every JSON flag's help lists each key its object or list item takes", () => {
    interface Node {
      readonly type?: string;
      readonly items?: Node;
      readonly properties?: Readonly<Record<string, Node>>;
    }
    let checked = 0;
    for (const spec of dispatchToolSpecs) {
      const schema = z.toJSONSchema(dispatchToolSchema(spec, zodSchemaApi(z))) as Node;
      const help = commandHelp(spec.name);
      for (const [field, node] of Object.entries(schema.properties ?? {})) {
        const keys = node.properties ?? node.items?.properties;
        if (keys === undefined) continue;
        const flag = `  --${flagName(field)}-json <json>\n`;
        const at = help.indexOf(flag);
        expect(at, `${spec.name} ${field}`).toBeGreaterThan(-1);
        const end = help.indexOf("\n  --", at + flag.length);
        const section = help.slice(at, end === -1 ? undefined : end);
        for (const key of Object.keys(keys)) {
          expect(section, `${spec.name} ${field}.${key}`).toContain(`\n        ${key} (`);
          checked++;
        }
      }
    }
    expect(checked).toBeGreaterThan(20);
  });

  test("doc-edit --help lists each key an operation takes, with its type and its values", () => {
    const help = commandHelp("dispatch_doc_edit");
    expect(help).toContain(
      [
        "  --ops-json <json>",
        "      Flat tagged edits; an operation takes only the keys below, a misspelled one is refused rather than ignored, and the server validates the fields its op requires. Required.",
        "      Each item's keys:",
        "        op (required; replace|delete|insert|retype|move|delete_row|delete_column): Edit operation.",
        "",
      ].join("\n")
    );
    expect(help).toContain(
      "\n        index (integer): Zero-based row or column index for delete_row or delete_column.\n"
    );
    expect(help).toContain("\n        type (text): Typed block name for retype.\n");
    expect(help).toContain("\n        attributes (any JSON): Typed block attributes for retype;");
    expect(help).toContain(
      [
        "      Its keys:",
        "        document (text): Token for the exact canonical document returned by dispatch doc-read.",
        "        blocks (list of objects): Every content block this batch changes, each with the token dispatch doc-read --blocks lists.",
        "          Each item's keys:",
        "            id (required; text): Stable block id dispatch doc-read --blocks lists.",
        "            token (required; text): That block's full-state token, including inline marks.",
        "",
      ].join("\n")
    );
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

  // An empty issue key, project key or check id names nothing, and sent it reaches a route that is
  // no route (`PATCH /api/v1/issues/`), which a rolling release also answers. It is refused before
  // any request, naming the flag that gave it: an empty variable in a job's step writes one.
  test("an empty --issue, --project or --check is refused, naming the flag that gave it", () => {
    const emptyFile = {
      readText: (path: string): string => {
        if (path === "empty.txt") return "";
        throw new Error(`unexpected read ${path}`);
      },
    };
    for (const [argv, problem] of [
      [
        ["issue-update", "--issue", "", "--status", "in_progress"],
        "--issue is empty, so the call names no issue: a variable that gives it may be empty or unset",
      ],
      [
        ["issue-update", "--issue=", "--status", "in_progress"],
        "--issue is empty, so the call names no issue: a variable that gives it may be empty or unset",
      ],
      [
        ["read", "--issue-file", "empty.txt"],
        "--issue-file is empty, so the call names no issue: a variable that gives it may be empty or unset",
      ],
      [
        ["issues", "--project", ""],
        "--project is empty, so the call names no project: a variable that gives it may be empty or unset",
      ],
      [
        [
          "result",
          "--project",
          "ACME",
          "--check",
          "",
          "--verdict",
          "pass",
          "--commit",
          "a".repeat(40),
        ],
        "--check is empty, so the call names no check: a variable that gives it may be empty or unset",
      ],
    ] as const) {
      expect(parseCommand(argv, emptyFile), argv.join(" ")).toMatchObject({
        kind: "refused",
        problems: [problem],
      });
    }
  });

  // The same holds for every other value that names what a call is about or narrows what a read
  // answers: an empty commit records a result at no commit, an empty `--at` pins the read to no
  // commit and an issue list's empty `--parent` lists every issue of the project, each with no
  // line, and an empty result id or comment id goes into the path. `issue-update --parent ""`
  // clears the parent, as its flag says, so that command takes it.
  test("an empty --commit, --at, --retract, --comment or issue list --parent is refused, naming the flag", () => {
    const unset = (flag: string, noun: string) =>
      `${flag} is empty, so the call names no ${noun}: a variable that gives it may be empty or unset`;
    for (const [argv, problem] of [
      [
        ["result", "--project", "ACME", "--check", "probe", "--verdict", "pass", "--commit", ""],
        unset("--commit", "commit"),
      ],
      [["results", "--project", "ACME", "--commit", ""], unset("--commit", "commit")],
      [["results", "--issue", "ACME-1", "--at", ""], unset("--at", "commit")],
      [["issues", "--project", "ACME", "--parent", ""], unset("--parent", "parent")],
      [["result", "--retract", "", "--reason", "Wrong commit."], unset("--retract", "result")],
      [["resolve-comment", "--comment", ""], unset("--comment", "comment")],
    ] as const) {
      expect(parseCommand(argv, noFiles), argv.join(" ")).toMatchObject({
        kind: "refused",
        problems: [problem],
      });
    }
    expect(parseCommand(["issue-update", "--issue", "ACME-1", "--parent", ""], noFiles)).toEqual({
      kind: "call",
      tool: "dispatch_issue_update",
      args: { issue: "ACME-1", parent: "" },
      dryRun: false,
    });
  });

  // A value of only white space names nothing either: Dispatch trims a query value, so
  // `dispatch issues --project " "` listed every project's issues with no line. And `.` or `..`
  // in a path is a step of the path, which the URL parser drops: `dispatch read --issue ..` asked
  // `GET /api/v1/`, another route. So the one refusal reads the value trimmed, and refuses `.` and
  // `..` for each flag whose value goes into a path in some command, `--session` among them.
  test("a value of only white space, or . or .., is refused, naming the flag", () => {
    const blank = (flag: string, noun: string) =>
      `${flag} is only white space, so the call names no ${noun}`;
    const dots = (flag: string, value: string, noun: string) =>
      `${flag} is "${value}", only dots and slashes, so the call names no ${noun}`;
    for (const [argv, problem] of [
      [["issues", "--project", " "], blank("--project", "project")],
      [["issues", "--project", "ACME", "--parent", "\t"], blank("--parent", "parent")],
      [["open-asks", "--project", " "], blank("--project", "project")],
      [["results", "--issue", "ACME-1", "--at", " "], blank("--at", "commit")],
      [["issue-update", "--issue", "  ", "--status", "in_progress"], blank("--issue", "issue")],
      [["read", "--issue", ".."], dots("--issue", "..", "issue")],
      [["read", "--issue", " . "], dots("--issue", ".", "issue")],
      [
        ["result", "--retract", "..", "--reason", "Wrong commit."],
        dots("--retract", "..", "result"),
      ],
      [["resolve-comment", "--comment", "."], dots("--comment", ".", "comment")],
      [
        ["send-to-agent", "--session", "..", "--body", "Hello."],
        dots("--session", "..", "session"),
      ],
      [
        ["send-to-agent", "--session", "", "--body", "Hello."],
        "--session is empty, so the call names no session: a variable that gives it may be empty or unset",
      ],
      [
        ["architecture-sync", "--project", "..", "--repo", "acme/infra"],
        dots("--project", "..", "project"),
      ],
    ] as const) {
      expect(parseCommand(argv, noFiles), argv.join(" ")).toMatchObject({
        kind: "refused",
        problems: [problem],
      });
    }
    // A flag whose value goes into a URL path in no command sends a dot as it is: a commit of "."
    // is sent to Dispatch, which answers that no such commit exists.
    expect(parseCommand(["results", "--issue", "ACME-1", "--at", "."], noFiles)).toMatchObject({
      kind: "call",
    });
  });

  // Each item of a list names a thing as a single value does: Dispatch drops a blank project of
  // `dispatch priorities` and reads every project's counts with no line, so each item is refused
  // as the single value is.
  test("an item of a list value that names nothing is refused, naming the flag", () => {
    for (const argv of [
      ["priorities", "--project", " "],
      ["priorities", "--project", "ACME", "--project", "\t"],
    ]) {
      expect(parseCommand(argv, noFiles), argv.join(" ")).toMatchObject({
        kind: "refused",
        problems: ["--project is only white space, so the call names no project"],
      });
    }
    expect(parseCommand(["priorities", "--project", "ACME"], noFiles)).toMatchObject({
      kind: "call",
      args: { project: ["ACME"] },
    });
  });

  // A value of only dots and slashes names no thing of the flag's, wherever the command sends it:
  // `./`, `../` and `.//` are what a script writes for "./$X" with an empty X. In a URL path it
  // names no step, and no key or reference is made of dots and slashes where the command sends it
  // as a member of its body (`dispatch issue --project`), as a value of its query (`--project` of
  // `search`, `issues`, `open-asks` and `priorities`) or matches it against an issue's documents
  // (`--artifact` beside `--issue`). So the one reason is true in each use, and it names the flag
  // and the value and no place the value would go.
  test("a value of only dots and slashes is refused with one reason, wherever the command sends it", () => {
    const dots = (flag: string, value: string, noun: string) =>
      `${flag} is "${value}", only dots and slashes, so the call names no ${noun}`;
    for (const [argv, problem] of [
      [["read", "--issue", "./"], dots("--issue", "./", "issue")],
      [["read", "--issue", "../"], dots("--issue", "../", "issue")],
      [["read", "--issue", ".//"], dots("--issue", ".//", "issue")],
      [["resolve-comment", "--comment", "/"], dots("--comment", "/", "comment")],
      [
        ["send-to-agent", "--session", "...", "--body", "Hello."],
        dots("--session", "...", "session"),
      ],
      [["doc-read", "--project", "ACME", "--artifact", "./"], dots("--artifact", "./", "document")],
      [["issue", "--project", "./", "--title", "A title"], dots("--project", "./", "project")],
      [["search", "--project", "./", "--query", "ship it"], dots("--project", "./", "project")],
      [["issues", "--project", "./"], dots("--project", "./", "project")],
      [["open-asks", "--project", "./"], dots("--project", "./", "project")],
      [["priorities", "--project", "ACME", "--project", "./"], dots("--project", "./", "project")],
      [
        ["plan-approve", "--issue", "DSP-1", "--artifact", "./"],
        dots("--artifact", "./", "document"),
      ],
      [["doc-read", "--issue", "DSP-1", "--artifact", "./"], dots("--artifact", "./", "document")],
      [["read", "--issue", "DSP-1", "--artifact", "./"], dots("--artifact", "./", "document")],
    ] as const) {
      expect(parseCommand(argv, noFiles), argv.join(" ")).toMatchObject({
        kind: "refused",
        problems: [problem],
      });
    }
  });

  // A document of a project is found by its reference in `GET /api/v1/projects/{key}/artifacts/
  // {ref}`, so an empty --artifact reached the client's own refusal of the path, which names the
  // route and not the flag. Each command that takes a document refuses it here, as it does an
  // empty --issue.
  test("an empty --artifact is refused in each command that takes a project's document, naming the flag", () => {
    const unset =
      "--artifact is empty, so the call names no document: a variable that gives it may be empty or unset";
    for (const argv of [
      ["ask", "--project", "ACME", "--artifact", "", "--question", "Ship it?"],
      ["comment", "--project", "ACME", "--artifact", "", "--body", "Looks good."],
      [
        "suggest",
        "--project",
        "ACME",
        "--artifact",
        "",
        "--quote",
        "old wording",
        "--replace-with",
        "new wording",
      ],
      ["doc-edit", "--project", "ACME", "--artifact", "", "--ops-json", "[]"],
      ["doc-read", "--project", "ACME", "--artifact", ""],
      ["request-approval", "--project", "ACME", "--artifact", "", "--summary", "A live sync."],
      ["read", "--project", "ACME", "--artifact", ""],
    ]) {
      expect(parseCommand(argv, noFiles), argv.join(" ")).toMatchObject({
        kind: "refused",
        problems: [unset],
      });
    }
  });

  // Dispatch trims a value by Go's white space (`strings.TrimSpace`), which takes U+0085 where
  // JavaScript's `trim` keeps it: `dispatch issues --project $'\u0085'` listed every project's
  // issues with no line. So the refusal reads Go's set (`goBlankPattern`).
  test("a value of Go's white space alone, U+0085 among it, is refused, naming the flag", () => {
    for (const [argv, flag, noun] of [
      [["issues", "--project", "\u0085"], "--project", "project"],
      [["issues", "--project", "ACME", "--parent", "\u0085"], "--parent", "parent"],
      [["read", "--issue", " \u0085\u3000"], "--issue", "issue"],
    ] as const) {
      expect(parseCommand(argv, noFiles), argv.join(" ")).toMatchObject({
        kind: "refused",
        problems: [`${flag} is only white space, so the call names no ${noun}`],
      });
    }
  });

  // `dispatch issue --parent ""` made the issue with no parent and no line, as an empty variable
  // writes it, so the create refuses it as the issue list does. `issue-update --parent ""` clears
  // the parent, as its help says, so that command still takes it (the row above).
  test("an empty --parent of dispatch issue is refused, naming the flag", () => {
    expect(
      parseCommand(["issue", "--project", "ACME", "--title", "A title", "--parent", ""], noFiles)
    ).toMatchObject({
      kind: "refused",
      problems: [
        "--parent is empty, so the call names no parent: a variable that gives it may be empty or unset",
      ],
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
