import { describe, expect, test } from "bun:test";
import { dispatchToolSchema, dispatchToolSpecs, zodSchemaApi } from "@legion/contracts";
import { z } from "zod";
import {
  commandFlags,
  commandHelp,
  commandLine,
  commandName,
  dispatchLine,
  flagName,
  keyNamedIn,
  parseCommand,
  rerunWords,
  toolForCommand,
} from "../dispatch-command";
import { shellWords } from "./shell-words";

const noFiles = {
  readText: (path: string): string => {
    throw new Error(`unexpected read ${path}`);
  },
};

describe("the dispatch command surface", () => {
  test("every spec's example survives commandLine then parseCommand unchanged", () => {
    for (const spec of dispatchToolSpecs) {
      const argv = shellWords(commandLine(spec.name, spec.example as Record<string, unknown>));
      expect(argv[0]).toBe("dispatch");
      const parsed = parseCommand(argv.slice(1), noFiles);
      expect(parsed).toEqual({ kind: "call", tool: spec.name, args: spec.example, dryRun: false });
    }
  });

  test("names map both ways for all 35 tools, and no two flags of one command collide", () => {
    expect(dispatchToolSpecs).toHaveLength(35);
    for (const spec of dispatchToolSpecs) {
      expect(toolForCommand(commandName(spec.name))).toBe(spec.name);
      const flags = commandFlags(spec.name);
      expect(new Set(flags).size).toBe(flags.length);
    }
    expect(commandName("dispatch_issue_update")).toBe("issue-update");
    expect(flagName("reply_to_ask")).toBe("reply-to-ask");
  });

  // A write that may have landed names its key; the same command sent again with
  // `--idempotency-key <key>` sends its writes under it. The key is the call's, not an argument
  // of the tool, so the arguments are the ones the first call sent.
  test("--idempotency-key takes the key a write that may have landed named, beside the arguments", () => {
    const key = "0b9c1e7a-55f4-4d2e-9a6b-3c2f1d0e8a71";
    const message = ["message", "--issue", "LEGION-2", "--body", "hi"];
    const expected = {
      kind: "call" as const,
      tool: "dispatch_message",
      args: { issue: "LEGION-2", body: "hi" },
      dryRun: false,
      idempotencyKey: key,
    };
    expect(parseCommand([...message, "--idempotency-key", key], noFiles)).toEqual(expected);
    expect(parseCommand([...message, `--idempotency-key=${key}`], noFiles)).toEqual(expected);
    expect(parseCommand(message, noFiles)).not.toHaveProperty("idempotencyKey");
    for (const [extra, problem] of [
      [["--idempotency-key", key, "--idempotency-key", key], "--idempotency-key is given twice"],
      [["--idempotency-key"], "--idempotency-key needs a value"],
      [
        ["--idempotency-key", "retry-1"],
        '--idempotency-key must be the key a write that exited 75 named, a lower-case UUID; got "retry-1"',
      ],
    ] as const) {
      expect(parseCommand([...message, ...extra], noFiles)).toEqual({
        kind: "refused",
        tool: "dispatch_message",
        problems: [problem],
      });
    }
    expect(commandFlags("dispatch_message")).toContain("--idempotency-key");
  });

  // `--dry-run-out <file>` is a dry run whose JSON goes to the file the caller names.
  test("--dry-run-out is a dry run that names the file its JSON goes to", () => {
    const message = ["message", "--issue", "LEGION-2", "--body", "hi"];
    expect(parseCommand([...message, "--dry-run-out", "out.json"], noFiles)).toEqual({
      kind: "call",
      tool: "dispatch_message",
      args: { issue: "LEGION-2", body: "hi" },
      dryRun: true,
      dryRunOut: "out.json",
    });
    expect(parseCommand([...message, "--dry-run-out"], noFiles)).toEqual({
      kind: "refused",
      tool: "dispatch_message",
      problems: ["--dry-run-out needs a value"],
    });
    expect(parseCommand([...message, "--dry-run-out=a", "--dry-run-out=b"], noFiles)).toEqual({
      kind: "refused",
      tool: "dispatch_message",
      problems: ["--dry-run-out is given twice"],
    });
  });

  // Whatever else a call's words hold, the CLI reads the key they name, so a refused call that
  // names the key of a write an earlier call may have landed says that write may have landed:
  // an unknown command or flag included. A value that is no key the CLI names is none.
  test("keyNamedIn reads the key a call's words name, whether or not the call parses", () => {
    const key = "0b9c1e7a-55f4-4d2e-9a6b-3c2f1d0e8a71";
    expect(keyNamedIn(["nope", "--idempotency-key", key, "--bogus"])).toBe(key);
    expect(keyNamedIn(["message", `--idempotency-key=${key}`])).toBe(key);
    expect(keyNamedIn(["message", "--body", "--idempotency-key"])).toBeUndefined();
    expect(keyNamedIn(["message", "--idempotency-key", "retry-1"])).toBeUndefined();
    expect(keyNamedIn(["message", "--body", "hi"])).toBeUndefined();
  });

  test("dispatchLine quotes each word so the shell splits the line back into the same words", () => {
    const words = ["message", "--body", "it's\ntwo lines", "--issue", "LEGION-2", "--x=--y", ""];
    expect(shellWords(dispatchLine(words))).toEqual(["dispatch", ...words]);
    expect(dispatchLine(["read", "LEGION-2"])).toBe("dispatch read LEGION-2");
    // An apostrophe closes the single quotes and stands alone in double quotes: AskGate's literal
    // grammar reads that, and refuses the backslash of `'\''`. A `!` stays in single quotes, where
    // an interactive shell expands no history.
    expect(dispatchLine(["message", "--body", "it's !done"])).toBe(
      `dispatch message --body 'it'"'"'s !done'`
    );
    expect(dispatchLine(["message", "--body", "it's !done"])).not.toContain("\\");
  });

  // The command that sends a call again reads what the call read: each text a flag read from a
  // file or from stdin comes from a copy, and a long value moves to a copy read through the flag's
  // file twin. Everything else, the inline `--flag=value` form included, stays as written.
  test("rerunWords reads every text the call read from a copy, and a long value from one", () => {
    const kept: string[] = [];
    const keep = (text: string, place: number): string => {
      kept.push(text);
      return `/copies/${place}`;
    };
    const read = new Map([
      ["-", "from stdin\n"],
      ["/tmp/ops.json", '[{"op":"delete","find":"x"}]'],
    ]);
    const long = "x".repeat(21);
    expect(
      rerunWords(
        [
          "doc-edit",
          "LEGION-2",
          "--artifact=spec",
          "--summary-file",
          "-",
          "--ops-json-file=/tmp/ops.json",
          "--precondition-json",
          `{"blocks":[{"id":"${long}"}]}`,
          "--idempotency-key",
          "0b9c1e7a-55f4-4d2e-9a6b-3c2f1d0e8a71",
        ],
        read,
        20,
        keep
      )
    ).toEqual([
      "doc-edit",
      "LEGION-2",
      "--artifact=spec",
      "--summary-file",
      "/copies/1",
      "--ops-json-file",
      "/copies/2",
      "--precondition-json-file",
      "/copies/3",
      "--idempotency-key",
      "0b9c1e7a-55f4-4d2e-9a6b-3c2f1d0e8a71",
    ]);
    expect(kept).toEqual([
      "from stdin\n",
      '[{"op":"delete","find":"x"}]',
      `{"blocks":[{"id":"${long}"}]}`,
    ]);
    // A value at the bound, a flag with no file twin (a number), and words of no known command
    // stay as they are.
    expect(rerunWords(["issues", "--limit", "12345678901234567890123"], read, 20, keep)).toEqual([
      "issues",
      "--limit",
      "12345678901234567890123",
    ]);
    expect(rerunWords(["message", "--body", "x".repeat(20)], read, 20, keep)).toEqual([
      "message",
      "--body",
      "x".repeat(20),
    ]);
    expect(rerunWords(["nope", "--body-file", "-"], read, 20, keep)).toEqual([
      "nope",
      "--body-file",
      "-",
    ]);
    expect(kept).toHaveLength(3);
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
