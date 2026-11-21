import { Database } from "bun:sqlite";
import { describe, expect, test } from "bun:test";
import { existsSync, mkdirSync, readdirSync, readFileSync, writeFileSync } from "node:fs";
import path from "node:path";
import { linkOmpNatives } from "@legion/pi-shared/test/omp-natives";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";
import { SessionEntry, type SessionMessage, stripSkillHeader } from "../skill-scenarios/transcript";
import { type Arm, type ArmName, buildArm } from "./arms";
import {
  armPromptBlocks,
  type Capture,
  type CaptureOptions,
  captureBody,
  type HintWriter,
  NEUTRAL_CLAUDE_CONFIG_DIR,
  NEUTRAL_CWD,
  NEUTRAL_OS,
  ompHarnessRules,
  promptDigest,
  refusedTools,
  renderReminder,
  rewriteBody,
  type SavedHead,
  type SessionFacts,
  savedCapture,
  sessionFacts,
  sessionHeads,
  skillListing,
  userTurnKey,
} from "./capture";
import { type Case, type Profile, pathHash } from "./cases";
import { localSource, skillFrontmatter } from "./context";
import { BudgetSpent } from "./gateway";
import { END_TURN_REPLY, messageSse, sseResponse, startRecorder } from "./recorder";
import { BLOCK_REASON, MESSAGES_VARIABLE } from "./replay-hook";

// The capture on the pinned Oh My Pi (capture.ts), against the recorder, with made-up skills and
// repositories: `alpha` and `beta` of a made-up `acme/skills`, read from local directories in
// place of `gh`, and the messages of F1 (testdata/F1, an Oh My Pi 18.18.6 session that reads
// skill://alpha and skill://alpha/references/x.md, captured with `HOME=<scratch> omp -p
// --session-dir <scratch>/sessions --model anthropic/claude-sonnet-5-5 "<prompt>"`,
// testdata/CAPTURE.md). Hint requests go to a stand-in that writes a new hint on each
// call, so a row can tell a hint the cache gave from one written again. LEGION_TEST_OMP names the
// binary, as in this package's other real-binary tests: a run without it skips, except on GitHub
// Actions, where the pi-legion job installs the pin (GITHUB_ACTIONS, not CI: agent harnesses on
// the devbox export CI=true).
const omp = process.env.LEGION_TEST_OMP;
const onActions = process.env.GITHUB_ACTIONS === "true";
const skip = omp === undefined && !onActions;
if (skip) {
  console.log(
    "capture.test.ts: LEGION_TEST_OMP is unset, so its rows are skipped; set it to the pinned Oh My Pi binary to run them"
  );
}

/** The binary the rows run; throws on GitHub Actions without it. */
function binary(): string {
  if (omp === undefined) throw new Error("LEGION_TEST_OMP is unset on GitHub Actions");
  return omp;
}

/** A row's own timeout: up to seven Oh My Pi runs of about ten seconds each in a slow pod. */
const ROW_TIMEOUT_MS = 600_000;

const F1 = path.join(
  import.meta.dir,
  "testdata",
  "F1",
  "2026-11-20T01-59-41-780Z_01a1f6ae-51d4-715f-a2a6-59f275bdff80.jsonl"
);
const messages: SessionMessage[] = readFileSync(F1, "utf8")
  .split("\n")
  .filter((line) => line.trim() !== "")
  .map((line) => SessionEntry.parse(JSON.parse(line)))
  .flatMap((entry) => (entry.type === "message" && entry.message ? [entry.message] : []));
/** The session_init line of F1's subagent ProductiveChicken: the system prompt its session
 *  recorded (its skill listing names alpha and beta) and its tool names. */
const SUBAGENT_INIT = readFileSync(
  path.join(F1.replace(/\.jsonl$/, ""), "ProductiveChicken.jsonl"),
  "utf8"
)
  .split("\n")
  .filter((line) => line.trim() !== "")
  .map((line) => JSON.parse(line) as { type: string; systemPrompt?: string[]; tools?: string[] })
  .find((entry) => entry.type === "session_init");
const SUBAGENT_PROMPT = SUBAGENT_INIT?.systemPrompt ?? [];
const SUBAGENT_TOOLS = SUBAGENT_INIT?.tools ?? [];
/** The file F1's read result at `index` read, as it was on disk. */
function f1File(index: number): string {
  return `${stripSkillHeader(messages[index]?.content?.[0]?.text ?? "")}\n`;
}
const OLD_SKILL = f1File(2);
const X = f1File(4);
/** The new text: alpha's description and its last line, each with words added. */
const NEW_SKILL = OLD_SKILL.replace(
  /^description: (.*)$/m,
  "description: $1 It names the new text."
)
  .trimEnd()
  .concat(" Then stop.\n");
const BETA_SKILL =
  "---\nname: beta\ndescription: Use when a task asks for the beta greeting of the made-up project.\n---\n\n# Beta\n\nGreet with one line.\n";
/** A skill Oh My Pi installs and never lists, as two of cybermonklabs/legion's skills are. */
const DELTA_SKILL =
  "---\nname: delta\ndescription: Use when asked for the delta audit.\ndisable-model-invocation: true\n---\n\n# Delta\n";
const COMMIT = "0123456789abcdef0123456789abcdef01234567";
const MODELS = `providers:
  anthropic:
    baseUrl: http://127.0.0.1:9
    api: anthropic-messages
    compat: {replayUnsignedThinking: false, supportsLongCacheRetention: true}
    models:
      - {id: claude-sonnet-5-5, name: Claude Sonnet 5.5, reasoning: true, input: [text, image], contextWindow: 1000000, maxTokens: 128000, cost: {input: 2, output: 10, cacheRead: 0.1, cacheWrite: 2.5}}
`;

const dir = temporaryDirectory("skill-cuts-capture", { real: true });

/** Writes `files` (path to text) under `root`. */
function writeTree(root: string, files: Record<string, string>): void {
  for (const [file, text] of Object.entries(files)) {
    mkdirSync(path.dirname(path.join(root, file)), { recursive: true });
    writeFileSync(path.join(root, file), text);
  }
}

/** F1's `task` step as a case of alpha, with id `id`. */
function f1Case(id: string): Case {
  const lines = OLD_SKILL.trimEnd().split("\n");
  return {
    id,
    cut: "acme-skills-1",
    skill: "alpha",
    baseFile: "SKILL.md",
    line: { text: lines.at(-1) ?? "", lineNumber: lines.length, lineCount: lines.length },
    criterion: { pass: "The reply does the step.", fail: "The reply does not do the step." },
    model: "anthropic/claude-sonnet-5-5",
    thinking: "off",
    context: "devbox",
    profile: {
      skills: [
        { repo: "acme/skills", commit: COMMIT, path: "skills/alpha", name: "alpha" },
        { repo: "acme/skills", commit: COMMIT, path: "skills/beta", name: "beta" },
        { repo: "acme/skills", commit: COMMIT, path: "skills/delta", name: "delta" },
      ],
      plugins: [],
      // A devbox profile's context file: its dev box's user-level one (build.ts).
      contextFiles: [{ repo: "acme/project", commit: COMMIT, path: ".claude/CLAUDE.md" }],
    },
    messages: messages.slice(0, 7),
    skillReads: [2, 4],
    realStep: messages[7] ?? {},
    violatingStep: { role: "assistant", content: [{ type: "text", text: "Step 4 is done." }] },
    source: { store: "store_a", pathHash: pathHash("/sessions/alpha.jsonl") },
    sessionStart: "2026-11-20T01:59:41.780Z",
    secretsRemoved: 0,
    sessionDigest: "46874d907e557998",
  };
}

/** The made-up session the captures stand for: its working directory, the reminder Oh My Pi
 *  journaled for its first user turn (F1's first message), and the dev box's Claude Code config
 *  directory, as a store and a configuration would give them. */
const SESSION_CWD = "/srv/acme/widgets";
const SESSION_REMINDER = renderReminder("2026-11-19", SESSION_CWD);
const FIRST_TURN = userTurnKey({
  timestamp: Number(messages[0]?.timestamp),
  attribution: String(messages[0]?.attribution),
});
const DEVBOX_CLAUDE = "/srv/acme/dotfiles/.claude";
const SESSION_HEAD: SavedHead = {
  file: path.join(dir, "sessions", "made-up.json"),
  head: {
    store: "store_a",
    pathHash: pathHash("/sessions/alpha.jsonl"),
    held: true,
    cwd: SESSION_CWD,
    start: "2026-11-19T09:00:00.000Z",
    firstUser: { timestamp: Number(messages[0]?.timestamp), attribution: "user" },
    reminders: [{ turn: FIRST_TURN, reminder: SESSION_REMINDER, placement: "prefix" }],
    bytes: 4096,
    badLines: 1,
    readAt: "2026-11-21T00:00:00.000Z",
  },
};
const SESSION: SessionFacts = sessionFacts(SESSION_HEAD, {
  turn: FIRST_TURN,
  date: { value: "2026-11-20", from: "the UTC date of the case's step in the session" },
  devbox: true,
  devboxClaudeConfigDir: DEVBOX_CLAUDE,
});

/** The skill each hint request names, in the order the stand-in got them. */
const hintRequests: string[] = [];
/** The `anthropic-version` header each hint request came with. */
const hintVersions: (string | undefined)[] = [];
/** A stand-in hint writer: a new hint on each call, naming the skill and the call. */
const hints: HintWriter = async (body, headers) => {
  const skill = /Skill: ([^\\]+)\\nDescription: /.exec(JSON.stringify(body.messages))?.[1] ?? "";
  hintRequests.push(skill);
  hintVersions.push(headers["anthropic-version"]);
  return {
    text: `Use for the ${skill} work, stand-in hint ${hintRequests.length}.`,
    model: body.model,
  };
};

/** Every string inside `value`. */
function texts(value: unknown): string[] {
  if (typeof value === "string") return [value];
  if (Array.isArray(value)) return value.flatMap(texts);
  if (typeof value === "object" && value !== null) return Object.values(value).flatMap(texts);
  return [];
}

/** Every `tool_use` block inside `value`. */
function toolUses(value: unknown): unknown[] {
  if (Array.isArray(value)) return value.flatMap(toolUses);
  if (typeof value !== "object" || value === null) return [];
  if (Reflect.get(value, "type") === "tool_use") return [value];
  return Object.values(value).flatMap(toolUses);
}

/** The hints a capture's scratch profile holds, by Oh My Pi's key, read before the next capture
 *  of the case writes that profile again. */
function profileHints(capture: Capture): Map<string, string> {
  const file = path.join(
    capture.record.scratch,
    "home/.omp/profiles/capture/agent/skill-descriptions.db"
  );
  const db = new Database(file);
  try {
    const rows = db.query("SELECT key, description FROM skill_descriptions").all() as {
      key: string;
      description: string;
    }[];
    return new Map(rows.map((row) => [row.key, row.description]));
  } finally {
    db.close();
  }
}

/** A capture and the hints its scratch profile held once it was made. */
interface Captured {
  readonly capture: Capture;
  readonly profile: Map<string, string>;
}

/** The hint `captured`'s profile held for `skill`. */
function profileHint({ capture, profile }: Captured, skill: string): string | undefined {
  return profile.get(capture.record.hints.find((hint) => hint.skill === skill)?.ompKey ?? "");
}

interface Captures {
  readonly arms: Record<"old" | "new" | "none", Arm>;
  readonly old: Captured;
  readonly fresh: Captured;
  readonly none: Captured;
  readonly again: Captured;
  readonly hintsAfterFirst: number;
  readonly hintsBeforeAgain: number;
}
let captured: Promise<Captures> | undefined;

/** The captures the rows read, made once: the old, new and none arms of case c1, then the old arm
 *  of case c2, which has the same texts. */
function captures(): Promise<Captures> {
  captured ??= (async () => {
    const repos = path.join(dir, "repos");
    writeTree(path.join(repos, "acme/skills", COMMIT), {
      "skills/alpha/SKILL.md": OLD_SKILL,
      "skills/alpha/references/x.md": X,
      "skills/beta/SKILL.md": BETA_SKILL,
      "skills/delta/SKILL.md": DELTA_SKILL,
    });
    writeTree(path.join(repos, "acme/project", COMMIT), {
      ".claude/CLAUDE.md": "# Made-up user rules\n\nKeep each answer short.\n",
    });
    writeTree(path.join(dir, "old"), { "SKILL.md": OLD_SKILL, "references/x.md": X });
    writeTree(path.join(dir, "new"), { "SKILL.md": NEW_SKILL, "references/x.md": X });
    writeFileSync(path.join(dir, "models.yml"), MODELS);
    const options: CaptureOptions = {
      omp: binary(),
      outDir: path.join(dir, "out"),
      scratchDir: path.join(dir, "scratch"),
      cacheDir: path.join(dir, "cache"),
      source: localSource(repos),
      models: path.join(dir, "models.yml"),
      hints,
      session: SESSION,
    };
    const dirs = { old: path.join(dir, "old"), new: path.join(dir, "new") };
    const armOf = (c: Case, name: ArmName) => buildArm(c, name, dirs, path.join(dir, "arms"));
    const capture = async (c: Case, arm: Arm): Promise<Captured> => {
      const made = await captureBody(c, arm, options);
      return { capture: made, profile: profileHints(made) };
    };
    const c1 = f1Case("c1");
    const arms = { old: armOf(c1, "old"), new: armOf(c1, "new"), none: armOf(c1, "none") };
    const old = await capture(c1, arms.old);
    const hintsAfterFirst = hintRequests.length;
    const fresh = await capture(c1, arms.new);
    const none = await capture(c1, arms.none);
    const hintsBeforeAgain = hintRequests.length;
    const again = await capture(f1Case("c2"), armOf(f1Case("c2"), "old"));
    return { arms, old, fresh, none, again, hintsAfterFirst, hintsBeforeAgain };
  })();
  return captured;
}

/** `blocks` less every line of the skill listing that names `skill`. */
function withoutListingLine(blocks: readonly string[], skill: string): string[] {
  return blocks.map((block) =>
    block
      .split("\n")
      .filter((line) => !line.startsWith(`- ${skill}: `))
      .join("\n")
  );
}

test("an arm's recorded prompt is the recording with only the skill's listing line changed: under the arm's hint, or left out", () => {
  expect(SUBAGENT_PROMPT.length).toBeGreaterThan(1);
  const recordedLine = skillListing({
    system: SUBAGENT_PROMPT.map((text) => ({ type: "text", text })),
  }).get("alpha");
  expect(recordedLine).toBeDefined();
  const replaced = armPromptBlocks(SUBAGENT_PROMPT, "alpha", "Use for the arm's alpha.");
  expect(replaced.line).toBe("replaced");
  expect(skillListing({ system: replaced.blocks.map((text) => ({ type: "text", text })) })).toEqual(
    new Map([
      ["alpha", "Use for the arm's alpha."],
      [
        "beta",
        skillListing({ system: SUBAGENT_PROMPT.map((text) => ({ type: "text", text })) }).get(
          "beta"
        ) ?? "",
      ],
    ])
  );
  const removed = armPromptBlocks(SUBAGENT_PROMPT, "alpha", undefined);
  expect(removed.line).toBe("removed");
  for (const each of [replaced, removed]) {
    expect(withoutListingLine(each.blocks, "alpha")).toEqual(
      withoutListingLine(SUBAGENT_PROMPT, "alpha")
    );
  }
  expect(removed.blocks.join("").length).toBe(
    SUBAGENT_PROMPT.join("").length - `- alpha: ${recordedLine}\n`.length
  );
  expect(armPromptBlocks(SUBAGENT_PROMPT, "gamma", "Use for gamma.")).toEqual({
    blocks: SUBAGENT_PROMPT,
    line: "not listed",
  });
});

/** The texts of a body's `system` blocks, in order. */
function systemBlocks(body: { readonly [field: string]: unknown }): string[] {
  return Array.isArray(body.system)
    ? body.system.map((block: unknown) =>
        typeof block === "object" && block !== null ? String(Reflect.get(block, "text")) : ""
      )
    : [];
}

/** The Envoy and Legion plugins as the worker image ships them: each one's bundle, built from this
 *  checkout into the made-up repository acme/legion at COMMIT, its manifest naming the bundle. */
const PLUGINS = [
  {
    name: "pi-envoy",
    entry: "envoy",
    externals: ["pi-ai", "pi-coding-agent", "pi-tui", "pi-utils"],
  },
  {
    name: "pi-legion",
    entry: "legion",
    externals: ["pi-coding-agent", "pi-tui", "pi-utils"],
  },
];
/** The profile's entry of the dispatch-first skill, which the Envoy plugin reads from its skills
 *  at load. */
const DISPATCH_FIRST = {
  repo: "acme/skills",
  commit: COMMIT,
  path: "skills/dispatch-first",
  name: "dispatch-first",
};
let pluginsBuilt = false;

/** The profile's entries of the plugins `names` of PLUGINS. The first call builds every one of
 *  them and writes the dispatch-first skill into acme/skills. */
function builtPlugins(names: readonly string[]): Profile["plugins"] {
  if (!pluginsBuilt) {
    const repo = path.join(dir, "repos/acme/legion", COMMIT);
    const packages = path.resolve(import.meta.dir, "../../..");
    for (const plugin of PLUGINS) {
      const dist = path.join(repo, "packages", plugin.name, "dist");
      const built = Bun.spawnSync(
        [
          "bun",
          "build",
          `extensions/${plugin.entry}.ts`,
          "--outdir",
          dist,
          "--target",
          "bun",
          "--format",
          "esm",
          ...plugin.externals.flatMap((name) => ["--external", `@oh-my-pi/${name}`]),
        ],
        { cwd: path.join(packages, plugin.name), stderr: "pipe" }
      );
      expect({
        code: built.exitCode,
        stderr: built.exitCode === 0 ? "" : built.stderr.toString(),
      }).toEqual({ code: 0, stderr: "" });
      writeTree(path.join(repo, "packages", plugin.name), {
        "package.json": JSON.stringify({
          name: `@acme/${plugin.name}`,
          files: ["dist"],
          omp: { extensions: [`dist/${plugin.entry}.js`], skills: ["dist/skills"] },
        }),
      });
    }
    writeTree(path.join(dir, "repos/acme/skills", COMMIT), {
      "skills/dispatch-first/SKILL.md": readFileSync(
        path.resolve(packages, "../skills/dispatch-first/SKILL.md"),
        "utf8"
      ),
    });
    pluginsBuilt = true;
  }
  return names.map((name) => ({
    repo: "acme/legion",
    commit: COMMIT,
    path: `packages/${name}`,
    name,
  }));
}

describe.skipIf(skip)("capture on the pinned Oh My Pi", () => {
  test(
    "the recorded body lists each installed skill under the hint the profile's cache holds, never its description",
    async () => {
      const { old, fresh } = await captures();
      for (const each of [old, fresh]) {
        const listing = skillListing(each.capture.body);
        expect([...listing.keys()].sort()).toEqual(["alpha", "beta"]);
        for (const skill of ["alpha", "beta"]) {
          const hint = profileHint(each, skill);
          expect(hint).toStartWith(`Use for the ${skill} work, stand-in hint `);
          expect(listing.get(skill)).toBe(hint as string);
        }
        // The record lists each skill under the hint the profile's cache holds for it.
        expect(each.capture.record.listing.map((entry) => [entry.name, entry.hint]).sort()).toEqual(
          ["alpha", "beta"].map((skill) => [skill, profileHint(each, skill) as string])
        );
      }
      const oldText = skillFrontmatter(path.join(dir, "old")).description;
      const newText = skillFrontmatter(path.join(dir, "new")).description;
      expect(newText).not.toBe(oldText);
      const oldBody = old.capture.body;
      const freshBody = fresh.capture.body;
      expect(skillListing(oldBody).get("alpha")).not.toContain(oldText.slice(0, 30));
      // The new text gets a hint of its own, written for it; beta's comes from the cache.
      expect(skillListing(freshBody).get("alpha")).not.toBe(skillListing(oldBody).get("alpha"));
      expect(skillListing(freshBody).get("beta")).toBe(skillListing(oldBody).get("beta"));
      expect(old.capture.record.hintCalls.map((call) => call.skill).sort()).toEqual([
        "alpha",
        "beta",
      ]);
      expect(old.capture.record.runs).toBe(2);
      // delta is installed and hidden, so it is neither listed nor given a hint.
      expect(old.capture.record.context.skills.map((skill) => skill.name)).toEqual([
        "alpha",
        "beta",
        "delta",
      ]);
      expect(old.capture.record.hints.map((hint) => hint.skill)).toEqual(["alpha", "beta"]);
      expect(fresh.capture.record.hintCalls.map((call) => call.skill)).toEqual(["alpha"]);
    },
    ROW_TIMEOUT_MS
  );

  test(
    "the none arm's body lists none of the skill and lists exactly the profile's other skills",
    async () => {
      const { none, arms } = await captures();
      const body = none.capture.body;
      expect([...skillListing(body).keys()]).toEqual(["beta"]);
      expect(arms.none.profileSkills.map((skill) => skill.name)).toEqual(["beta", "delta"]);
      // No call reads the skill: the user's prompt still names it, as the session's did.
      const calls = toolUses(body.messages);
      expect(calls.length).toBeGreaterThan(0);
      expect(calls.filter((call) => JSON.stringify(call).includes("skill://alpha"))).toEqual([]);
      expect(none.capture.record.hintCalls).toEqual([]);
      expect(none.capture.record.runs).toBe(1);
    },
    ROW_TIMEOUT_MS
  );

  test(
    "the body's messages are the arm's, and the placeholder prompt is in none of it",
    async () => {
      const { old, fresh, arms } = await captures();
      for (const [each, arm] of [
        [old, arms.old],
        [fresh, arms.new],
      ] as const) {
        const sent = texts(each.capture.body.messages);
        for (const index of [0, 2, 4]) {
          const text = arm.messages[index]?.content?.[0]?.text ?? "";
          expect(text).not.toBe("");
          expect(sent.some((one) => one.includes(text))).toBe(true);
        }
        expect(texts(each.capture.body).some((text) => text.includes("placeholder prompt"))).toBe(
          false
        );
      }
      const named = (each: Captured) =>
        texts(each.capture.body.messages).some((text) => text.includes("It names the new text."));
      expect(named(fresh)).toBe(true);
      expect(named(old)).toBe(false);
      // The context file reached the system prompt, and the session's digest is recorded.
      const body = old.capture.body;
      expect(texts(body.system).some((text) => text.includes("Keep each answer short."))).toBe(
        true
      );
      expect(old.capture.record.sessionDigest).toBe("46874d907e557998");
      // The turn's and the hints' `anthropic-*` headers go with them, for a replay to send.
      expect(old.capture.record.requestHeaders["anthropic-version"]).toBe("2023-06-01");
      expect(
        Object.keys(old.capture.record.requestHeaders).every((name) =>
          name.startsWith("anthropic-")
        )
      ).toBe(true);
      expect(hintVersions.length).toBeGreaterThan(0);
      expect(hintVersions.every((version) => version === "2023-06-01")).toBe(true);
    },
    ROW_TIMEOUT_MS
  );

  test(
    "a devbox case's context file is Oh My Pi's user-level one: CLAUDE.md in the scratch Claude Code config directory its run names, not a project file, shown at the dev box's",
    async () => {
      const { old } = await captures();
      const { record } = old.capture;
      const configDir = path.join(record.scratch, "claude-config");
      expect(record.context.contextFiles).toMatchObject([
        { repo: "acme/project", path: ".claude/CLAUDE.md", as: "user level" },
      ]);
      // As Oh My Pi sent it, its block names the file where Oh My Pi read it at user level
      // (CLAUDE_CONFIG_DIR/CLAUDE.md), and the working directory holds no .claude/CLAUDE.md.
      const raw = systemBlocks(JSON.parse(readFileSync(record.harness.rawBody, "utf8"))).join("\n");
      expect(raw).toContain(
        `<file path="${path.join(configDir, "CLAUDE.md")}">\n# Made-up user rules\n\nKeep each answer short.`
      );
      expect(raw).not.toContain(path.join(record.scratch, "work", ".claude"));
      expect(existsSync(path.join(record.scratch, "work", ".claude"))).toBe(false);
      // The body the replays send names it at the dev box's config directory, the configuration's
      // devboxClaudeConfigDir, and names the scratch one nowhere.
      const system = systemBlocks(old.capture.body).join("\n");
      expect(system).toContain(
        `<file path="${DEVBOX_CLAUDE}/CLAUDE.md">\n# Made-up user rules\n\nKeep each answer short.`
      );
      expect(system).not.toContain(configDir);
      expect(record.harness.claudeConfigDir).toEqual({
        value: DEVBOX_CLAUDE,
        from: "the configuration's devboxClaudeConfigDir",
      });
      // It is the prompt's one context file.
      expect(system.match(/<file path="/g)).toHaveLength(1);
    },
    ROW_TIMEOUT_MS
  );

  // Catches: a captured body that still names the capture's scratch directory (which names the
  // run, the cut and the skill under test), or a reminder of the capture's own working directory
  // and date in place of the session's.
  test(
    "the body holds no path of the capture's scratch, and its reminder is the session's own: its recorded working directory, byte for byte",
    async () => {
      const { old, fresh, none } = await captures();
      for (const { capture } of [old, fresh, none]) {
        const { body, record } = capture;
        const sent = JSON.stringify(body);
        const own = [
          record.scratch,
          path.join(dir, "scratch"),
          path.join(dir, "old"),
          path.join(dir, "new"),
        ];
        for (const scratchPath of own) expect(sent).not.toContain(scratchPath);
        // The reminder at the start of the first user turn is the one the session journaled.
        const first = (body.messages as { content: { text?: string }[] }[])[0]?.content[0]?.text;
        expect(first).toBe(SESSION_REMINDER);
        const shown = [...sent.matchAll(/current working directory: '([^']*)'/g)].map(
          (match) => match[1]
        );
        expect(shown).toEqual([SESSION_CWD]);
        for (const name of ["alpha", "acme-skills-1", "old", "new", "none", "scratch"]) {
          expect(shown[0]).not.toContain(name);
        }
        // The body as Oh My Pi sent it held the capture's working directory, which names the
        // capture's scratch: the rule replaced it.
        const raw = readFileSync(record.harness.rawBody, "utf8");
        expect(raw).toContain(`current working directory: '${path.join(record.scratch, "work")}'`);
        expect(record.harness.replaced).toContainEqual({
          what: "the date and working-directory reminder",
          count: 1,
        });
        expect(record.harness.cwd).toEqual({
          value: SESSION_CWD,
          from: "the session header's cwd",
        });
        expect(record.harness.reminder.from).toBe(
          `the session's journaled reminder of ${FIRST_TURN}`
        );
        expect(record.harness.head).toEqual({
          store: "store_a",
          pathHash: pathHash("/sessions/alpha.jsonl"),
          file: SESSION_HEAD.file,
          held: true,
        });
        // A prompt Oh My Pi built shows the neutral OS line, the same in every arm.
        expect(sent).toContain(`<workstation>\\n- OS: ${NEUTRAL_OS}\\n`);
        expect(record.harness.os?.value).toBe(NEUTRAL_OS);
      }
    },
    ROW_TIMEOUT_MS
  );

  test(
    "another capture of one arm text writes no hint and lists the same one, and a saved capture is read back",
    async () => {
      const { old, again, hintsAfterFirst, hintsBeforeAgain } = await captures();
      expect(hintsAfterFirst).toBe(2);
      expect(hintRequests.length).toBe(hintsBeforeAgain);
      expect(again.capture.record.runs).toBe(1);
      expect(skillListing(again.capture.body)).toEqual(skillListing(old.capture.body));
      expect(again.capture.record.hints.every((hint) => !hint.written)).toBe(true);
      expect(profileHint(again, "alpha")).toBe(profileHint(old, "alpha") as string);
      const c1 = f1Case("c1");
      const read = await captureBody(
        c1,
        buildArm(c1, "old", { old: path.join(dir, "old"), new: path.join(dir, "new") }, dir),
        {
          omp: binary(),
          outDir: path.join(dir, "out"),
          scratchDir: path.join(dir, "scratch"),
          cacheDir: path.join(dir, "cache"),
          source: localSource(path.join(dir, "repos")),
          models: path.join(dir, "models.yml"),
          hints,
          session: SESSION,
        }
      );
      expect(read.record).toEqual(old.capture.record);
      expect(hintRequests.length).toBe(hintsBeforeAgain);
    },
    ROW_TIMEOUT_MS
  );

  test(
    "the hook gives the model the arm's messages and blocks every tool call, and the capture's digest is the one Oh My Pi records",
    async () => {
      const root = path.join(dir, "hook");
      const home = path.join(root, "home");
      const agentDir = path.join(home, ".omp/profiles/hook/agent");
      const sessions = path.join(root, "sessions");
      const marker = path.join(root, "marker-the-block-must-stop");
      for (const each of [agentDir, sessions, path.join(root, "work")]) {
        mkdirSync(each, { recursive: true });
      }
      await linkOmpNatives(binary(), home);
      const bodies: string[] = [];
      const recorder = startRecorder({
        route: (request) => {
          if (request.method !== "POST" || request.path !== "/v1/messages") return undefined;
          bodies.push(request.body);
          if (bodies.length > 1) return sseResponse(END_TURN_REPLY);
          return sseResponse(
            messageSse({
              id: "msg_hook_1",
              model: "claude-sonnet-5-5",
              content: [
                {
                  type: "tool_use",
                  id: "toolu_hook_1",
                  name: "bash",
                  input: { command: `touch ${marker}` },
                },
              ],
              stopReason: "tool_use",
            })
          );
        },
      });
      try {
        // The scratch profile's provider, as a capture writes it: the recorder, a placeholder key.
        writeFileSync(
          path.join(agentDir, "models.yml"),
          MODELS.replace("http://127.0.0.1:9", recorder.url).replace(
            "    api: anthropic-messages\n",
            "    api: anthropic-messages\n    auth: apiKey\n    apiKey: placeholder-the-recorder-holds-no-key\n"
          )
        );
        writeFileSync(
          path.join(agentDir, "config.yml"),
          "modelRoles:\n  default: anthropic/claude-sonnet-5-5\n  smol: anthropic/claude-sonnet-5-5\n"
        );
        const messagesFile = path.join(root, "messages.json");
        writeFileSync(messagesFile, JSON.stringify(messages.slice(0, 3)));
        const child = Bun.spawn(
          [
            binary(),
            "--profile",
            "hook",
            "-p",
            "--mode",
            "json",
            "--session-dir",
            sessions,
            "--model",
            "anthropic/claude-sonnet-5-5",
            "--thinking",
            "off",
            "--hook",
            path.join(import.meta.dir, "replay-hook.ts"),
            "placeholder prompt",
          ],
          {
            cwd: path.join(root, "work"),
            env: {
              HOME: home,
              PATH: "/usr/local/bin:/usr/bin:/bin",
              TMPDIR: root,
              PI_NO_TITLE: "1",
              [MESSAGES_VARIABLE]: messagesFile,
            },
            stdout: "pipe",
            stderr: "pipe",
          }
        );
        const [stdout, stderr, code] = await Promise.all([
          new Response(child.stdout).text(),
          new Response(child.stderr).text(),
          child.exited,
        ]);
        expect({ code, stderr: code === 0 ? "" : stderr }).toEqual({ code: 0, stderr: "" });
        // The blocked call's result, as Oh My Pi's own event stream records it.
        expect(stdout).toContain(BLOCK_REASON);
      } finally {
        await recorder.close();
      }
      expect(existsSync(marker)).toBe(false);
      expect(bodies.length).toBe(2);
      const [first, second] = bodies.map((body) => JSON.parse(body));
      // The hook gives each request exactly the arm's messages, the run's own dropped.
      expect(second.messages).toEqual(first.messages);
      const sent = texts(first.messages);
      for (const index of [0, 2]) {
        const text = messages[index]?.content?.[0]?.text ?? "";
        expect(sent.some((each) => each.includes(text))).toBe(true);
      }
      expect(texts(second.messages).some((text) => text.includes(BLOCK_REASON))).toBe(false);
      const transcript = readdirSync(sessions, { recursive: true })
        .map(String)
        .filter((file) => file.endsWith(".jsonl"));
      expect(transcript.length).toBe(1);
      const digests = readFileSync(path.join(sessions, transcript[0] as string), "utf8")
        .split("\n")
        .filter((line) => line.trim() !== "")
        .map((line) => JSON.parse(line))
        .filter((entry) => entry.type === "custom" && entry.customType === "system-prompt-digest")
        .map((entry) => entry.data);
      expect(digests.length).toBeGreaterThan(0);
      expect(digests[0]).toBe(promptDigest(first));
    },
    ROW_TIMEOUT_MS
  );

  test(
    "a Legion-context capture defines the Legion and Envoy plugins' tools, and every endpoint it reached was a listener of its own on 127.0.0.1",
    async () => {
      await captures();
      const base = f1Case("legion-1");
      const c: Case = {
        ...base,
        context: "legion:acme:implementer",
        profile: {
          ...base.profile,
          skills: [...base.profile.skills, DISPATCH_FIRST],
          plugins: builtPlugins(["pi-envoy", "pi-legion"]),
          appendSystemPrompt: "You are a made-up role of a made-up project.",
        },
      };
      const capture = await captureBody(
        c,
        buildArm(c, "old", { old: path.join(dir, "old"), new: path.join(dir, "new") }, dir),
        {
          omp: binary(),
          outDir: path.join(dir, "out"),
          scratchDir: path.join(dir, "scratch"),
          cacheDir: path.join(dir, "cache"),
          source: localSource(path.join(dir, "repos")),
          models: path.join(dir, "models.yml"),
          hints,
          session: SESSION,
        }
      );
      const { record, body } = capture;
      expect(record.context.plugins.map((plugin) => plugin.as)).toEqual(["extension", "extension"]);
      expect(record.pluginTools.tools).toContain("legion");
      expect(record.pluginTools.envoy).toContain("envoy_send");
      expect(record.pluginTools.leftOut).toEqual([]);
      // Every endpoint the pane was given is a listener of the capture's own on 127.0.0.1, and
      // the stand-in daemon took the registration the tool waits for.
      expect(record.legion?.role).toBe("implementer");
      for (const endpoint of Object.values(record.legion?.endpoints ?? {})) {
        expect(endpoint).toMatch(/^(http|nats):\/\/127\.0\.0\.1:\d+(\/|$)/);
      }
      expect(record.connections.http).toContain("POST /legion/v1/claims/register");
      expect(record.connections.http).toContain("POST /v1/messages");
      // Nothing the run added reaches the model: no endpoint's failure, no placeholder.
      expect(record.hookDropped[0]).toBeGreaterThan(0);
      expect(texts(body.messages).some((text) => text.includes("127.0.0.1"))).toBe(false);
      expect(texts(body).some((text) => text.includes("placeholder prompt"))).toBe(false);
      expect(record.exit.code === 0 || record.exit.reason !== "").toBe(true);
    },
    ROW_TIMEOUT_MS
  );

  // Catches: a devbox capture that loads the Envoy plugin and gives it no listener to reach, so
  // its body defines none of the Envoy tools the dev box's sessions had.
  test(
    "a devbox capture whose profile installs the Envoy plugin defines the Envoy tools, reaches only its own stand-in listener, and is otherwise a devbox capture: its user-level CLAUDE.md, no Legion plugin, no scratch path in its body",
    async () => {
      await captures();
      const base = f1Case("devbox-envoy-1");
      const c: Case = {
        ...base,
        profile: {
          ...base.profile,
          skills: [...base.profile.skills, DISPATCH_FIRST],
          plugins: builtPlugins(["pi-envoy"]),
        },
      };
      expect(c.context).toBe("devbox");
      const { record, body } = await captureBody(
        c,
        buildArm(c, "old", { old: path.join(dir, "old"), new: path.join(dir, "new") }, dir),
        {
          omp: binary(),
          outDir: path.join(dir, "out"),
          scratchDir: path.join(dir, "scratch"),
          cacheDir: path.join(dir, "cache"),
          source: localSource(path.join(dir, "repos")),
          models: path.join(dir, "models.yml"),
          hints,
          session: SESSION,
        }
      );
      expect(record.context.plugins.map((plugin) => [plugin.name, plugin.as])).toEqual([
        ["pi-envoy", "extension"],
      ]);
      expect(record.pluginTools.envoy).toContain("envoy_send");
      expect(record.pluginTools.leftOut).toEqual([]);
      expect(record.extensionErrors).toEqual([]);
      // Every Envoy endpoint the run was given is a listener of the capture's own on 127.0.0.1,
      // the run reached them (an Envoy route of the recorder, or a dial of the NATS sink), and no
      // stand-in daemon. In one value, so a failure shows every request the run made.
      expect(Object.keys(record.envoy?.endpoints ?? {})).toEqual(["ENVOY_URL", "ENVOY_NATS_URL"]);
      for (const endpoint of Object.values(record.envoy?.endpoints ?? {})) {
        expect(endpoint).toMatch(/^(http|nats):\/\/127\.0\.0\.1:\d+(\/|$)/);
      }
      const { http, nats } = record.connections;
      expect({
        reachedEnvoy: nats > 0 || http.some((request) => /^\w+ \/envoy\/v1\//.test(request)),
        daemon: http.filter((request) => /^\w+ \/legion\//.test(request)),
        turn: http.includes("POST /v1/messages"),
        http,
      }).toEqual({ reachedEnvoy: true, daemon: [], turn: true, http });
      expect(record.otherRequests).toEqual([]);
      // No Legion plugin, pane or claim: the profile installs none.
      expect(record.pluginTools.tools).not.toContain("legion");
      expect(record.legion).toBeNull();
      // Its one context file is still the user-level CLAUDE.md, shown at the dev box's.
      const system = systemBlocks(body).join("\n");
      expect(system).toContain(
        `<file path="${DEVBOX_CLAUDE}/CLAUDE.md">\n# Made-up user rules\n\nKeep each answer short.`
      );
      expect(system.match(/<file path="/g)).toHaveLength(1);
      // The harness-strings rule holds with the listener's answers in the run: no path of the
      // capture's scratch, no endpoint of its listeners, no id of the run's own session (which
      // the answers echo) but in `metadata`, which a replay draws again for every run
      // (gateway.ts's `freshIdentity`), and the session's own reminder.
      const sent = JSON.stringify(body);
      for (const scratchPath of [
        record.scratch,
        path.join(dir, "scratch"),
        path.join(dir, "old"),
        path.join(dir, "new"),
      ]) {
        expect(sent).not.toContain(scratchPath);
      }
      // Oh My Pi's own bash tool names 127.0.0.1 in every capture; no listener's port is named.
      for (const endpoint of Object.values(record.envoy?.endpoints ?? {})) {
        expect(sent).not.toContain(new URL(endpoint).host);
      }
      const sessions = path.join(record.scratch, "sessions");
      const transcripts = readdirSync(sessions, { recursive: true })
        .map(String)
        .filter((file) => file.endsWith(".jsonl"));
      expect(transcripts.length).toBe(1);
      const runSession = readFileSync(path.join(sessions, transcripts[0] as string), "utf8")
        .split("\n")
        .filter((line) => line.trim() !== "")
        .map((line) => JSON.parse(line) as { type: string; id?: string })
        .find((entry) => entry.type === "session")?.id;
      expect(runSession).toMatch(/^[0-9a-f-]{36}$/);
      const { metadata: _, ...unreplaced } = body;
      expect(JSON.stringify(unreplaced)).not.toContain(runSession as string);
      const first = (body.messages as { content: { text?: string }[] }[])[0]?.content[0]?.text;
      expect(first).toBe(SESSION_REMINDER);
      expect(record.harness.cwd.value).toBe(SESSION_CWD);
      expect(texts(body).some((text) => text.includes("placeholder prompt"))).toBe(false);
    },
    ROW_TIMEOUT_MS
  );

  test(
    "a subagent case's capture sends its recorded system prompt byte for byte but for the skill's listing line, per arm, and only its recorded tools",
    async () => {
      await captures();
      const base = f1Case("subagent-1");
      const c: Case = {
        ...base,
        context: "subagent:task",
        profile: {
          ...base.profile,
          contextFiles: [],
          systemPrompt: SUBAGENT_PROMPT,
          tools: SUBAGENT_TOOLS,
        },
      };
      const dirs = { old: path.join(dir, "old"), new: path.join(dir, "new") };
      for (const name of ["old", "new", "none"] as const) {
        const { body, record } = await captureBody(
          c,
          buildArm(c, name, dirs, path.join(dir, "arms-subagent")),
          {
            omp: binary(),
            outDir: path.join(dir, "out"),
            scratchDir: path.join(dir, "scratch"),
            cacheDir: path.join(dir, "cache"),
            source: localSource(path.join(dir, "repos")),
            models: path.join(dir, "models.yml"),
            hints,
            session: SESSION,
          }
        );
        const sent = systemBlocks(body);
        expect(sent).toHaveLength(SUBAGENT_PROMPT.length);
        // Every byte the recording holds but the skill's line of its listing.
        expect(withoutListingLine(sent, "alpha")).toEqual(
          withoutListingLine(SUBAGENT_PROMPT, "alpha")
        );
        const hint = record.hints.find((each) => each.skill === "alpha")?.hint;
        const listing = skillListing(body);
        if (name === "none") {
          expect(listing.has("alpha")).toBe(false);
          expect(record.recordedPrompt?.line).toBe("removed");
        } else {
          expect(hint).toStartWith("Use for the alpha work, stand-in hint ");
          expect(listing.get("alpha")).toBe(hint as string);
          expect(record.recordedPrompt?.line).toBe("replaced");
        }
        expect(sent).toEqual(
          armPromptBlocks(SUBAGENT_PROMPT, "alpha", name === "none" ? undefined : hint).blocks
        );
        expect(record.recordedPrompt?.blocks).toBe(SUBAGENT_PROMPT.length);
        expect(record.tools).toEqual({
          recorded: SUBAGENT_TOOLS,
          sent: SUBAGENT_TOOLS,
          missing: [],
          extra: [],
          leftOut: [],
        });
      }
    },
    ROW_TIMEOUT_MS
  );

  test(
    "a subagent case whose recorded tools include one nothing the capture loads defines is captured, that tool left out of --tools and recorded",
    async () => {
      await captures();
      const base = f1Case("subagent-2");
      const recorded = [...SUBAGENT_TOOLS, "acme_absent_tool"];
      const c: Case = {
        ...base,
        context: "subagent:task",
        profile: {
          ...base.profile,
          contextFiles: [],
          systemPrompt: SUBAGENT_PROMPT,
          tools: recorded,
        },
      };
      const { body, record } = await captureBody(
        c,
        buildArm(c, "old", { old: path.join(dir, "old"), new: path.join(dir, "new") }, dir),
        {
          omp: binary(),
          outDir: path.join(dir, "out"),
          scratchDir: path.join(dir, "scratch"),
          cacheDir: path.join(dir, "cache"),
          source: localSource(path.join(dir, "repos")),
          models: path.join(dir, "models.yml"),
          hints,
          session: SESSION,
        }
      );
      expect(record.tools?.recorded).toEqual(recorded);
      expect(record.tools?.sent).toEqual(SUBAGENT_TOOLS);
      expect(record.tools?.missing).toEqual(["acme_absent_tool"]);
      expect(record.tools?.leftOut.map((tool) => tool.name)).toEqual(["acme_absent_tool"]);
      expect(record.tools?.leftOut[0]?.reason).toContain("defines it");
      expect(record.pluginTools.leftOut.some((line) => line.startsWith("acme_absent_tool: "))).toBe(
        true
      );
      expect(systemBlocks(body)).toHaveLength(SUBAGENT_PROMPT.length);
    },
    ROW_TIMEOUT_MS
  );

  test(
    "a subagent case that recorded retain, a memory built-in Oh My Pi refuses in a session without memory, is captured with retain left out and recorded",
    async () => {
      await captures();
      const base = f1Case("subagent-3");
      const recorded = [...SUBAGENT_TOOLS, "retain"];
      const c: Case = {
        ...base,
        context: "subagent:task",
        profile: {
          ...base.profile,
          contextFiles: [],
          systemPrompt: SUBAGENT_PROMPT,
          tools: recorded,
        },
      };
      const { body, record } = await captureBody(
        c,
        buildArm(c, "old", { old: path.join(dir, "old"), new: path.join(dir, "new") }, dir),
        {
          omp: binary(),
          outDir: path.join(dir, "out"),
          scratchDir: path.join(dir, "scratch"),
          cacheDir: path.join(dir, "cache"),
          source: localSource(path.join(dir, "repos")),
          models: path.join(dir, "models.yml"),
          hints,
          session: SESSION,
        }
      );
      expect(record.exit.code).toBe(0);
      expect(record.tools?.sent).toEqual(SUBAGENT_TOOLS);
      expect(record.tools?.missing).toEqual(["retain"]);
      expect(record.tools?.leftOut.map((tool) => tool.name)).toEqual(["retain"]);
      expect(record.tools?.leftOut[0]?.reason).toContain("in a session without memory");
      expect(record.pluginTools.leftOut.some((line) => line.startsWith("retain: "))).toBe(true);
      expect(systemBlocks(body)).toHaveLength(SUBAGENT_PROMPT.length);
    },
    ROW_TIMEOUT_MS
  );

  test(
    "a case on openai/gpt-6.1-sol is captured as the Responses body Oh My Pi sends to /v1/responses (F8's shape), its hints from the Anthropic hint model, and a recorded prompt as its instructions",
    async () => {
      await captures();
      const models = path.join(dir, "models-openai.yml");
      // The pod's models.yml shape: Oh My Pi's built-in openai provider at the gateway's /v1,
      // with no api and no models list, beside the Messages route's provider.
      writeFileSync(
        models,
        `${MODELS}  openai:\n    baseUrl: http://127.0.0.1:9/v1\n    compat: {statefulResponses: false}\n    modelOverrides:\n      gpt-6.1-sol: {contextWindow: 922000}\n`
      );
      const options: CaptureOptions = {
        omp: binary(),
        outDir: path.join(dir, "out"),
        scratchDir: path.join(dir, "scratch"),
        cacheDir: path.join(dir, "cache"),
        source: localSource(path.join(dir, "repos")),
        models,
        hints,
        session: SESSION,
        hintModel: "anthropic/claude-sonnet-5-5",
      };
      const dirs = { old: path.join(dir, "old"), new: path.join(dir, "new") };
      const c: Case = { ...f1Case("openai-1"), model: "openai/gpt-6.1-sol" };
      const arm = buildArm(c, "old", dirs, path.join(dir, "arms-openai"));
      const { body, record } = await captureBody(c, arm, options);
      expect(body.model).toBe("gpt-6.1-sol");
      expect(body.messages).toBeUndefined();
      expect(Array.isArray(body.input)).toBe(true);
      expect(typeof body.instructions).toBe("string");
      expect(JSON.stringify(body)).not.toContain("cache_control");
      expect(record.model).toBe("openai/gpt-6.1-sol");
      expect(record.hintModel).toBe("anthropic/claude-sonnet-5-5");
      expect(record.requestHeaders).toEqual({});
      expect(record.connections.http).toContain("POST /v1/responses");
      expect(record.connections.http).not.toContain("POST /v1/messages");
      // Oh My Pi read the recorder's Responses end of turn as a whole reply.
      expect(record.exit).toEqual({ code: 0, signal: null, reason: "" });
      // The arm's messages are the input: its user text, and its read results as tool outputs.
      const sent = texts(body.input);
      for (const index of [0, 2, 4]) {
        const text = stripSkillHeader(arm.messages[index]?.content?.[0]?.text ?? "");
        expect(text).not.toBe("");
        expect(sent.some((one) => one.includes(text))).toBe(true);
      }
      expect(texts(body).some((text) => text.includes("placeholder prompt"))).toBe(false);
      // The listing is in its instructions, each skill under the hint its profile holds.
      const listing = skillListing(body);
      expect([...listing.keys()].sort()).toEqual(["alpha", "beta"]);
      for (const { skill, hint } of record.hints) expect(listing.get(skill)).toBe(hint);

      // A recorded prompt goes as its blocks joined, a blank line between, into the instructions.
      const sub: Case = {
        ...f1Case("openai-subagent-1"),
        model: "openai/gpt-6.1-sol",
        context: "subagent:task",
        profile: {
          ...c.profile,
          contextFiles: [],
          systemPrompt: SUBAGENT_PROMPT,
          tools: SUBAGENT_TOOLS,
        },
      };
      const subArm = buildArm(sub, "old", dirs, path.join(dir, "arms-openai-subagent"));
      const recorded = await captureBody(sub, subArm, options);
      const hint = recorded.record.hints.find((each) => each.skill === "alpha")?.hint;
      expect(recorded.body.instructions).toBe(
        armPromptBlocks(SUBAGENT_PROMPT, "alpha", hint)
          .blocks.filter((block) => block.trim() !== "")
          .join("\n\n")
      );
      expect(recorded.record.recordedPrompt?.line).toBe("replaced");

      // The hint model is the case's model by default, and hints are written on the Messages
      // route only: an OpenAI case with no hint model is refused.
      const { hintModel: _, ...noHintModel } = options;
      const refused = await captureBody(
        { ...f1Case("openai-2"), model: "openai/gpt-6.1-sol" },
        buildArm(f1Case("openai-2"), "old", dirs, path.join(dir, "arms-openai-2")),
        noHintModel
      ).catch((caught: unknown) => caught);
      expect(String(refused)).toContain(
        "the hint model openai/gpt-6.1-sol is not served on the Anthropic Messages route"
      );
    },
    ROW_TIMEOUT_MS
  );

  test(
    "a Legion pane whose plugin registers the session and claims its role before the first model request gets through its boot: the stand-in answers the registration and the claim in shapes the client parses",
    async () => {
      await captures();
      // A made-up plugin whose boot does what the Legion plugins did before their role claim
      // moved to the first turn: at session start it registers the session with the Envoy
      // listener and claims the role, reads each answer as the Envoy client of this checkout
      // reads it (`parseSubscribeAnswer` takes no registration without a consumer), defines its
      // tool only then, and ends the process when either is refused, as that boot did.
      const transport = path.resolve(import.meta.dir, "../../../envoy-client/src/transport.ts");
      const repo = path.join(dir, "repos/acme/boot", COMMIT);
      writeTree(path.join(repo, "packages/boot-probe"), {
        "package.json": JSON.stringify({
          name: "@acme/boot-probe",
          files: ["dist"],
          omp: { extensions: ["dist/boot.js"] },
        }),
        "src/boot.ts": `import { parseSubscribeAnswer } from ${JSON.stringify(transport)};

const post = async (route, body) => {
  const answer = await fetch(process.env.ENVOY_URL + route, {
    method: "POST",
    headers: { "content-type": "application/json" },
    body: JSON.stringify(body),
  });
  if (!answer.ok) {
    console.error("[boot-probe] claim boot failed: POST " + route + " answered " + answer.status);
    process.exit(1);
  }
  return answer.json();
};

export default function bootProbe(pi) {
  pi.on("session_start", async (_event, context) => {
    const session = context.sessionManager.getSessionId();
    parseSubscribeAnswer(
      await post("/v1/interests/subscribe", {
        session_id: session,
        dir: process.cwd(),
        topics: [],
        delivery: { consumer: true, epoch: "boot-probe" },
      })
    );
    await post("/v1/roles/set", { session_id: session, role: "capture-role" });
    pi.registerTool({
      name: "boot_probe",
      label: "boot_probe",
      description: "A made-up tool its plugin defines once its boot has registered and claimed.",
      parameters: pi.zod.object({}),
      execute: async () => ({ content: [{ type: "text", text: "ok" }] }),
    });
    await pi.setActiveTools([...pi.getActiveTools(), "boot_probe"]);
  });
}
`,
      });
      // Bundled as the Legion plugins are, so the client's own imports travel with it.
      const built = Bun.spawnSync(
        ["bun", "build", "src/boot.ts", "--outdir", "dist", "--target", "bun", "--format", "esm"],
        { cwd: path.join(repo, "packages/boot-probe"), stderr: "pipe" }
      );
      expect({
        code: built.exitCode,
        stderr: built.exitCode === 0 ? "" : built.stderr.toString(),
      }).toEqual({ code: 0, stderr: "" });
      const base = f1Case("boot-1");
      const c: Case = {
        ...base,
        context: "legion:acme:implementer",
        profile: {
          ...base.profile,
          plugins: [
            { repo: "acme/boot", commit: COMMIT, path: "packages/boot-probe", name: "boot-probe" },
          ],
          appendSystemPrompt: "You are a made-up role of a made-up project.",
        },
      };
      const { record } = await captureBody(
        c,
        buildArm(c, "old", { old: path.join(dir, "old"), new: path.join(dir, "new") }, dir),
        {
          omp: binary(),
          outDir: path.join(dir, "out"),
          scratchDir: path.join(dir, "scratch"),
          cacheDir: path.join(dir, "cache"),
          source: localSource(path.join(dir, "repos")),
          models: path.join(dir, "models.yml"),
          hints,
          session: SESSION,
        }
      );
      expect(record.exit).toEqual({ code: 0, signal: null, reason: "" });
      // In one value, so a failure shows why the plugin defined no tool.
      expect({
        defined: [...record.pluginTools.tools, ...record.pluginTools.devices].includes(
          "boot_probe"
        ),
        extensionErrors: record.extensionErrors,
        http: record.connections.http,
      }).toEqual({
        defined: true,
        extensionErrors: [],
        http: expect.arrayContaining([
          "POST /envoy/v1/interests/subscribe",
          "POST /envoy/v1/roles/set",
          "POST /v1/messages",
        ]),
      });
      expect(record.runs).toBe(1);
    },
    ROW_TIMEOUT_MS
  );

  test(
    "a hint write that fails ends the capture at once with its own error: a BudgetSpent stays one, and neither Oh My Pi nor the hint wait is blamed",
    async () => {
      await captures();
      const refusedHint: HintWriter = async () => {
        throw new BudgetSpent(0, 0);
      };
      const c = f1Case("budget-1");
      const started = performance.now();
      const failed = await captureBody(
        c,
        buildArm(c, "old", { old: path.join(dir, "old"), new: path.join(dir, "new") }, dir),
        {
          omp: binary(),
          outDir: path.join(dir, "out-budget"),
          scratchDir: path.join(dir, "scratch"),
          // A cache of its own, so every hint is owed and the hint run asks for it.
          cacheDir: path.join(dir, "cache-budget"),
          source: localSource(path.join(dir, "repos")),
          models: path.join(dir, "models.yml"),
          hints: refusedHint,
          session: SESSION,
        }
      ).catch((caught: unknown) => caught);
      expect(failed).toBeInstanceOf(BudgetSpent);
      // Well within the 180 s the hint run's turn waits for a hint it is owed.
      expect(performance.now() - started).toBeLessThan(120_000);
      expect(existsSync(path.join(dir, "out-budget"))).toBe(false);
    },
    ROW_TIMEOUT_MS
  );
});

// Oh My Pi 18.18.6's own refusals of a run's --tools (validateToolNames,
// packages/coding-agent/src/cli/args.ts at the pin), each wording with one tool and with several,
// and both lines in one refusal.
test("each tool Oh My Pi refuses in --tools is read from its refusal, one or several, with why", () => {
  const memory = "in a session without memory";
  expect(
    refusedTools("Error: Built-in tool unavailable in this session: retain.\nRun `omp --help`.\n")
  ).toEqual([{ name: "retain", reason: expect.stringContaining(memory) }]);
  expect(
    refusedTools(
      "Error: Built-in tools unavailable in this session: memory_edit, retain, recall, reflect."
    ).map((tool) => tool.name)
  ).toEqual(["memory_edit", "retain", "recall", "reflect"]);
  const both = refusedTools(
    "Error: Unknown tools in --tools: codegraph, acme_tool.\nBuilt-in tools: read, bash.\nBuilt-in tool unavailable in this session: eval.\n"
  );
  expect(both.map((tool) => tool.name)).toEqual(["codegraph", "acme_tool", "eval"]);
  expect(both[0]?.reason).toContain("defines it");
  expect(both[2]?.reason).toBe("Oh My Pi defines it as a built-in, but refuses it in this session");
  expect(refusedTools("Error: Unknown tool in --tools: codegraph.")).toEqual([
    { name: "codegraph", reason: expect.stringContaining("defines it") },
  ]);
  expect(refusedTools("some other failure")).toEqual([]);
});

/** A head of the made-up session at `pathHash`, with `fields` in place of the default ones. */
function madeUpHead(fields: Partial<SavedHead["head"]>): SavedHead {
  return { file: SESSION_HEAD.file, head: { ...SESSION_HEAD.head, ...fields } };
}

const STEP_DATE = { value: "2026-11-21", from: "the UTC date of the case's step in the session" };

// Catches: a capture that shows a neutral or a rendered value where the store holds the
// session's own, or the capture's own value where it holds none.
test("sessionFacts: the journaled reminder byte for byte, else one rendered from the header's working directory and the step's date, else neutral values", () => {
  const options = {
    turn: FIRST_TURN,
    date: STEP_DATE,
    devbox: false,
    devboxClaudeConfigDir: DEVBOX_CLAUDE,
  };
  const journaled = sessionFacts(SESSION_HEAD, options);
  expect(journaled.cwd).toEqual({ value: SESSION_CWD, from: "the session header's cwd" });
  expect(journaled.reminder.value).toBe(SESSION_REMINDER);
  expect(journaled.date).toEqual({
    value: "2026-11-19",
    from: "the session's journaled reminder of the turn",
  });
  // Not a dev box: Claude Code's config directory is neutral whatever the configuration names.
  expect(journaled.claudeConfigDir).toEqual({
    value: NEUTRAL_CLAUDE_CONFIG_DIR,
    from: "neutral: the context is no dev box's",
  });
  // A reminder of another turn is not this one's: the reminder is rendered for the step's date.
  const otherTurn = sessionFacts(SESSION_HEAD, { ...options, turn: "user:1:user" });
  expect(otherTurn.reminder).toEqual({
    value: renderReminder("2026-11-21", SESSION_CWD),
    from: "rendered from the working directory (the session header's cwd) and the date (the UTC date of the case's step in the session)",
  });
  // No header cwd: the journaled reminder's working directory.
  const noHeader = sessionFacts(madeUpHead({ cwd: null }), options);
  expect(noHeader.cwd).toEqual({
    value: SESSION_CWD,
    from: "the session's journaled reminder of the turn",
  });
  // Neither: neutral, saying why; and a session the store does not hold.
  const neither = sessionFacts(madeUpHead({ cwd: null, reminders: [] }), options);
  expect(neither.cwd.value).toBe(NEUTRAL_CWD);
  expect(neither.cwd.from).toContain("neither a header cwd nor a reminder");
  expect(neither.reminder.value).toBe(renderReminder("2026-11-21", NEUTRAL_CWD));
  const gone = sessionFacts(
    madeUpHead({ held: false, cwd: null, reminders: [], firstUser: null }),
    options
  );
  expect(gone.cwd).toEqual({
    value: NEUTRAL_CWD,
    from: "neutral: the store holds no session of this hash",
  });
  expect(gone.head?.held).toBe(false);
  const unnamed = sessionFacts(undefined, options);
  expect(unnamed.head).toBeNull();
  expect(unnamed.cwd.from).toBe("neutral: no session is named");
  // A dev box: the configuration's config directory, else a neutral one saying why.
  expect(sessionFacts(SESSION_HEAD, { ...options, devbox: true }).claudeConfigDir.value).toBe(
    DEVBOX_CLAUDE
  );
  expect(
    sessionFacts(SESSION_HEAD, { ...options, devbox: true, devboxClaudeConfigDir: undefined })
      .claudeConfigDir
  ).toEqual({
    value: NEUTRAL_CLAUDE_CONFIG_DIR,
    from: "neutral: the configuration names no devboxClaudeConfigDir",
  });
});

// Catches: a rule that misses a harness string, touches a recorded prompt, reads a `$` of the
// session's text as a pattern, or lets a body through that still names the scratch.
test("the harness rules replace the capture's reminder, context-file paths, OS line and working directory, and leave a recorded prompt as recorded", () => {
  const work = "/scratch/run-1/legion-retro/captures/abc/work";
  const config = "/scratch/run-1/legion-retro/captures/abc/claude-config";
  const facts = sessionFacts(SESSION_HEAD, {
    turn: FIRST_TURN,
    date: STEP_DATE,
    devbox: true,
    devboxClaudeConfigDir: DEVBOX_CLAUDE,
  });
  const captured = renderReminder("2026-11-21", work);
  const body = {
    model: "claude-sonnet-5-5",
    system: [
      {
        type: "text",
        text: `<project-context>\n<workstation>\n- OS: linux 4.19.0-gvisor\n- Arch: x64\n</workstation>\n<file path="${work}/AGENTS.md">\nKeep $1 and $& as they are.\n</file>\n<file path="${config}/CLAUDE.md">\nRules.\n</file>`,
      },
    ],
    messages: [
      {
        role: "user",
        content: [
          { type: "text", text: captured },
          {
            type: "text",
            text: "Cost: $5. Look in /legion/scratch/tmp/own-session for the notes.",
          },
        ],
      },
      { role: "assistant", content: [{ type: "text", text: `I am in ${work}/src.` }] },
    ],
  };
  const built = rewriteBody(body, ompHarnessRules(work, config, facts, true), ["/scratch/run-1"]);
  expect(built.faults).toEqual([]);
  const text = JSON.stringify(built.body);
  expect(text).not.toContain("/scratch/run-1");
  expect((built.body.messages as unknown[] | undefined)?.[0]).toEqual({
    role: "user",
    content: [
      { type: "text", text: SESSION_REMINDER },
      // The session's own text, a `$` and its own scratch path among it, is left as it is.
      { type: "text", text: "Cost: $5. Look in /legion/scratch/tmp/own-session for the notes." },
    ],
  });
  const system = String(Object(built.body.system as unknown[])[0]?.text);
  expect(system).toContain(`<file path="${SESSION_CWD}/AGENTS.md">\nKeep $1 and $& as they are.`);
  expect(system).toContain(`<file path="${DEVBOX_CLAUDE}/CLAUDE.md">`);
  expect(system).toContain(`<workstation>\n- OS: ${NEUTRAL_OS}\n- Arch: x64\n`);
  expect(text).toContain(`I am in ${SESSION_CWD}/src.`);
  expect(built.replaced).toEqual([
    { what: "the date and working-directory reminder", count: 1 },
    { what: "a context file's path in the working directory", count: 1 },
    { what: "the user-level context file's path", count: 1 },
    { what: "the workstation's OS line", count: 1 },
    { what: "another mention of the working directory", count: 1 },
  ]);
  // A recorded prompt is sent as recorded: its OS line stays, and a scratch path in it is a fault.
  const recorded = rewriteBody(
    body,
    ompHarnessRules(work, config, facts, false),
    ["/scratch/run-1"],
    (where) => where.length === 1 && where[0] === "system"
  );
  expect(recorded.body.system).toEqual(body.system);
  expect(recorded.faults).toEqual(["system.0.text: /scratch/run-1"]);
  expect(recorded.replaced.map((rule) => rule.what)).not.toContain("the workstation's OS line");
  // The body given is never changed.
  expect(body.messages[0]?.content[0]?.text).toBe(captured);
});

// Catches: a head read twice from the store, a hash the store does not hold taken as held, or a
// failed read taken for a neutral session.
test("sessionHeads reads the heads it lacks in one query through the store command, saves each, and reads a saved one back", async () => {
  const root = path.join(dir, "heads");
  mkdirSync(root, { recursive: true });
  const calls = path.join(root, "calls.txt");
  const held = pathHash("/sessions/held.jsonl");
  const missing = pathHash("/sessions/missing.jsonl");
  const row = JSON.stringify({
    hash: held,
    cwd: SESSION_CWD,
    start: "2026-11-19T09:00:00.000Z",
    first_user: { timestamp: 1795139983655, attribution: "user" },
    reminders: [
      { turn: "user:1795139983655:user", reminder: SESSION_REMINDER, placement: "prefix" },
    ],
    bytes: 4096,
    bad_lines: 1,
  });
  // A store command that prints the row as psql prints a one-column table, and keeps its query.
  const command = path.join(root, "store.sh");
  writeFileSync(
    command,
    `#!/bin/sh\ncat > "${root}/query-$$.sql"\necho "$1" >> "${calls}"\nprintf ' head \\n------\\n %s\\n(1 row)\\n\\n' '${row.replaceAll("'", "'\\''")}'\n`,
    { mode: 0o755 }
  );
  const sessions = path.join(root, "sessions");
  const heads = await sessionHeads(command, "store_a", [held, missing, held], sessions);
  expect(readFileSync(calls, "utf8")).toBe("store_a\n");
  const queries = readdirSync(root).filter((file) => file.startsWith("query-"));
  expect(queries).toHaveLength(1);
  const query = readFileSync(path.join(root, queries[0] ?? ""), "utf8");
  expect(query).toContain(`'${held}'`);
  expect(query).toContain(`'${missing}'`);
  expect(heads.get(held)?.head).toMatchObject({
    store: "store_a",
    pathHash: held,
    held: true,
    cwd: SESSION_CWD,
    firstUser: { timestamp: 1795139983655, attribution: "user" },
    reminders: [
      { turn: "user:1795139983655:user", reminder: SESSION_REMINDER, placement: "prefix" },
    ],
    bytes: 4096,
    badLines: 1,
  });
  expect(heads.get(missing)?.head).toMatchObject({ held: false, cwd: null, reminders: [] });
  expect(existsSync(path.join(sessions, `${held}.json`))).toBe(true);
  // Read again: both from their files, and no query.
  const again = await sessionHeads(command, "store_a", [held, missing], sessions);
  expect(readFileSync(calls, "utf8")).toBe("store_a\n");
  expect(again.get(held)?.head).toEqual(heads.get(held)?.head);
  // A command that fails is a failed read, never a neutral session.
  const failing = path.join(root, "failing.sh");
  writeFileSync(failing, "#!/bin/sh\necho 'permission denied for table' >&2\nexit 3\n", {
    mode: 0o755,
  });
  await expect(
    sessionHeads(failing, "store_a", [pathHash("/sessions/other.jsonl")], sessions)
  ).rejects.toThrow("exited 3: permission denied for table");
});

// Catches: a capture made before the harness-string rule read back and replayed.
test("a saved capture whose record holds no harness record is refused", () => {
  const root = path.join(dir, "saved-before");
  mkdirSync(root, { recursive: true });
  const files = {
    body: path.join(root, "c.old.body.json"),
    record: path.join(root, "c.old.capture.json"),
  };
  writeFileSync(files.body, JSON.stringify({ model: "claude-sonnet-5-5", messages: [] }));
  writeFileSync(files.record, JSON.stringify({ case: "c", arm: "old" }));
  expect(() => savedCapture(files)).toThrow("was made before the harness-string rule");
  writeFileSync(files.record, JSON.stringify({ case: "c", arm: "old", harness: { replaced: [] } }));
  expect(savedCapture(files)?.record.case).toBe("c");
});
