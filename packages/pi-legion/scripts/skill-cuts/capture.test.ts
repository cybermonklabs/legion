import { Database } from "bun:sqlite";
import { describe, expect, test } from "bun:test";
import { existsSync, mkdirSync, readdirSync, readFileSync, writeFileSync } from "node:fs";
import path from "node:path";
import { readDispatchFirstContext } from "@legion/envoy-client/dispatch-first";
import { linkOmpNatives } from "@legion/pi-shared/test/omp-natives";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";
import {
  SessionEntry,
  type SessionMessage,
  skillHeaderPath,
  stripSkillHeader,
} from "../skill-scenarios/transcript";
import { type Arm, type ArmName, buildArm, type DispatchAnswer } from "./arms";
import {
  armPromptBlocks,
  type Capture,
  type CaptureOptions,
  captureBody,
  capturePaths,
  extensionErrors,
  type HintWriter,
  LegionToolUndefined,
  listingOf,
  NEUTRAL_CLAUDE_CONFIG_DIR,
  NEUTRAL_CWD,
  NEUTRAL_OS,
  ompHarnessRules,
  ompVersion,
  pluginTools,
  promptDigest,
  refusedTools,
  renderReminder,
  rewriteBody,
  type SavedHead,
  type SessionFacts,
  savedCapture,
  sessionCheckout,
  sessionFacts,
  sessionHeads,
  skillListing,
  userTurnKey,
} from "./capture";
import { type Case, type Profile, pathHash } from "./cases";
import { Refused } from "./common";
import {
  type ContextRecord,
  cacheKey,
  localSource,
  type RepoSource,
  skillFrontmatter,
} from "./context";
import { BudgetSpent, type MessagesBody } from "./gateway";
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
      // A devbox profile's user-level context file: its dev box's user-level one (build.ts).
      contextFiles: [
        { repo: "acme/project", commit: COMMIT, path: ".claude/CLAUDE.md", user: true },
      ],
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

/** The skill a hint request's body names. */
const hintSkillOf = (body: MessagesBody) =>
  /Skill: ([^\\]+)\\nDescription: /.exec(JSON.stringify(body.messages))?.[1] ?? "";

/** The skill each hint request names, in the order the stand-in got them. */
const hintRequests: string[] = [];
/** The `anthropic-version` header each hint request came with. */
const hintVersions: (string | undefined)[] = [];
/** A stand-in hint writer: a new hint on each call, naming the skill and the call. */
const hints: HintWriter = async (body, headers) => {
  const skill = hintSkillOf(body);
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

/** `value` with every `cache_control` key taken out, at any depth. */
function withoutCacheControl(value: unknown): unknown {
  if (Array.isArray(value)) return value.map(withoutCacheControl);
  if (typeof value !== "object" || value === null) return value;
  return Object.fromEntries(
    Object.entries(value)
      .filter(([key]) => key !== "cache_control")
      .map(([key, field]) => [key, withoutCacheControl(field)])
  );
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

// Catches: two readers of one listing that disagreed on a name listed twice: build.ts's kept its
// first line, capture.ts's `skillListing` its last.
test("a skill listing reads a name listed twice under its first line, in the capture and in build.ts alike (listingOf)", () => {
  const blocks = [
    "<skills>\n- alpha: Use for the first alpha.\n- beta: Use for beta.\n</skills>",
    "<skills>\n- alpha: Use for the second alpha.\n</skills>",
  ];
  const first = new Map([
    ["alpha", "Use for the first alpha."],
    ["beta", "Use for beta."],
  ]);
  expect(listingOf(blocks)).toEqual(first);
  expect(skillListing({ system: blocks.map((text) => ({ type: "text", text })) })).toEqual(first);
});

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
  { name: "pi-envoy", entry: "envoy" },
  { name: "pi-legion", entry: "legion" },
];

/** The packages the build script of the plugin package in `packageDir` leaves out of its bundle:
 *  each `--external <name>` of its package.json's `build`. The test's bundle leaves out the same
 *  ones, read here and never copied: a copy went stale when the package's build left out one more
 *  (CI, which tests the merge with main, could not resolve a module only Oh My Pi holds). */
function buildExternals(packageDir: string): string[] {
  const build: unknown = JSON.parse(readFileSync(path.join(packageDir, "package.json"), "utf8"))
    .scripts?.build;
  if (typeof build !== "string") throw new Error(`${packageDir}/package.json has no build script`);
  const externals = [...build.matchAll(/--external[= ](\S+)/g)].map((match) => match[1] ?? "");
  if (externals.length === 0) {
    throw new Error(`${packageDir}/package.json's build script names no --external package`);
  }
  return externals;
}

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
    const buildStamped = path.join(dir, "build-stamped.ts");
    writeFileSync(
      buildStamped,
      `const [entry = "", outdir = "", externals = "[]"] = process.argv.slice(2);
const built = await Bun.build({
  entrypoints: [entry],
  outdir,
  target: "bun",
  format: "esm",
  external: JSON.parse(externals),
  plugins: [
    {
      name: "pack",
      setup(build) {
        build.onLoad({ filter: /package\\.json$/ }, async ({ path }) => {
          const manifest = JSON.parse(await Bun.file(path).text());
          manifest.legion = { ...manifest.legion, daemonApiVersion: 1 };
          return { contents: JSON.stringify(manifest), loader: "json" };
        });
      },
    },
  ],
});
if (!built.success) {
  console.error(built.logs.join("\\n"));
  process.exit(1);
}
`
    );
    for (const plugin of PLUGINS) {
      const dist = path.join(repo, "packages", plugin.name, "dist");
      // `bun build` of the extension, with the plugin's package.json inlined as a pack leaves it:
      // holding `legion.daemonApiVersion`. Since 2026-11-25 pi-legion on main reads that field in
      // the bundle as it loads and refuses to load without it, unless the bundle sits in a
      // checkout; a bundle built from the source manifest here has no `legion` tool (CI, which
      // tests the merge with main, saw that). The source file is not touched.
      const built = Bun.spawnSync(
        [
          "bun",
          "run",
          buildStamped,
          `extensions/${plugin.entry}.ts`,
          dist,
          JSON.stringify(buildExternals(path.join(packages, plugin.name))),
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
          legion: { daemonApiVersion: 1 },
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

/** The profile's entry of a stand-in of the Legion plugin, named `pi-legion`, in the made-up
 *  repository acme/<repo> at COMMIT: a plugin whose one extension is `extension` (the text of
 *  its module, `dist/legion.js`), or, with none, a plugin of skills only (its package.json names
 *  no `omp`). */
function standInLegion(repo: string, extension?: string): Profile["plugins"][number] {
  writeTree(path.join(dir, "repos/acme", repo, COMMIT, "packages/pi-legion"), {
    "package.json": JSON.stringify({
      name: "@acme/pi-legion",
      files: ["dist"],
      ...(extension === undefined ? {} : { omp: { extensions: ["dist/legion.js"] } }),
    }),
    ...(extension === undefined ? {} : { "dist/legion.js": extension }),
  });
  return { repo: `acme/${repo}`, commit: COMMIT, path: "packages/pi-legion", name: "pi-legion" };
}

/** The capture of the old arm of F1's case `id` in a Legion implementer pane whose profile
 *  installs `plugin`, with the options of `captures()`. */
function legionCapture(id: string, plugin: Profile["plugins"][number]): Promise<Capture> {
  const base = f1Case(id);
  const c: Case = {
    ...base,
    context: "legion:acme:implementer",
    profile: {
      ...base.profile,
      plugins: [plugin],
      appendSystemPrompt: "You are a made-up role of a made-up project.",
    },
  };
  const dirs = { old: path.join(dir, "old"), new: path.join(dir, "new") };
  return captureBody(c, buildArm(c, "old", dirs, dir), {
    omp: binary(),
    outDir: path.join(dir, "out"),
    scratchDir: path.join(dir, "scratch"),
    cacheDir: path.join(dir, "cache"),
    source: localSource(path.join(dir, "repos")),
    models: path.join(dir, "models.yml"),
    hints,
    session: SESSION,
  });
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
      // The context file reached the system prompt; the session's digest is recorded, and the
      // capture's own is the digest of the body it saved, after the harness-string rule.
      const body = old.capture.body;
      expect(texts(body.system).some((text) => text.includes("Keep each answer short."))).toBe(
        true
      );
      expect(old.capture.record.sessionDigest).toBe("46874d907e557998");
      expect(old.capture.record.captureDigest).toBe(promptDigest(body));
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
      // Oh My Pi's Anthropic provider puts prompt-cache breakpoints on a request's last two
      // messages whenever it gives that request cache control, and whether it does for the turn
      // after a tool result varies by run, so the two turns are compared without `cache_control`.
      // A capture has one turn only, which carries no message breakpoint (the first turn here),
      // and the hook's contract is the messages' content and order.
      expect(withoutCacheControl(first.messages)).toEqual(first.messages);
      expect(withoutCacheControl(second.messages)).toEqual(withoutCacheControl(first.messages));
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
      // The plugins' own words on a tool they left out come first, so a failure prints them.
      expect(record.pluginTools.leftOut).toEqual([]);
      expect(record.pluginTools.tools).toContain("legion");
      expect(record.pluginTools.envoy).toContain("envoy_send");
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

  // Catches: a Legion pane's capture saved with a body that lacks the legion tool, every session
  // of the pane having had it, when pi-legion's session start throws and Oh My Pi goes on.
  test(
    "a Legion pane's capture whose pi-legion throws in its session start on every start is refused after its second attempt, naming the plugin and each attempt's own error text, and saves nothing",
    async () => {
      await captures();
      const plugin = standInLegion(
        "legion-throws",
        `export default function legion(pi) {
  pi.on("session_start", async () => {
    throw new Error("the stand-in pi-legion's session start failed");
  });
}
`
      );
      // In one value, so a capture that is not refused shows what it saved of the plugin.
      const failed = await legionCapture("legion-throws-1", plugin).then(
        (made) => made.record.pluginTools,
        (caught: unknown) => caught
      );
      // The class cli.ts drops a behaviour case on (LegionToolUndefined), not any error.
      expect(failed).toBeInstanceOf(LegionToolUndefined);
      const message = String((failed as Error).message);
      expect(message).toStartWith(
        "case legion-throws-1 arm old: the captured body is refused: a Legion implementer pane whose context installs pi-legion as an extension must send pi-legion's legion tool, and the body of none of its 2 attempts defines it: attempt 1: its body defines "
      );
      const [first = "", second = "", ...more] = message.split(/attempt \d: /).slice(1);
      expect(more).toEqual([]);
      for (const attempt of [first, second]) {
        expect(attempt).toContain("Extension error");
        expect(attempt).toContain("the stand-in pi-legion's session start failed");
      }
      const files = capturePaths(path.join(dir, "out"), "legion-throws-1", "old");
      expect([existsSync(files.body), existsSync(files.record)]).toEqual([false, false]);
    },
    ROW_TIMEOUT_MS
  );

  // Catches: a refusal that says "none" for a plugin that never loaded, so the cause of a missing
  // legion tool (CI's red row of 2026-11-25: the plugin refusing a manifest with no contract) is
  // not in the message.
  test(
    "a Legion pane's capture whose pi-legion cannot load its module is refused, and each attempt names Oh My Pi's own words of the load failure",
    async () => {
      await captures();
      const plugin = standInLegion(
        "legion-no-load",
        `throw new Error("the stand-in pi-legion refuses to load: its manifest holds no contract");
export default function legion() {}
`
      );
      const failed = await legionCapture("legion-no-load-1", plugin).then(
        (made) => made.record.pluginTools,
        (caught: unknown) => caught
      );
      expect(failed).toBeInstanceOf(LegionToolUndefined);
      const [first = "", second = "", ...more] = String((failed as Error).message)
        .split(/attempt \d: /)
        .slice(1);
      expect(more).toEqual([]);
      for (const attempt of [first, second]) {
        expect(attempt).toContain("Failed to load extension");
        expect(attempt).toContain(
          "the stand-in pi-legion refuses to load: its manifest holds no contract"
        );
      }
    },
    ROW_TIMEOUT_MS
  );

  // Catches: a capture that takes the first body a Legion pane sends, so one start of pi-legion
  // that threw saves a body with no legion tool; or a second attempt that leaves no record of the
  // first.
  test(
    "a Legion pane's capture whose pi-legion throws in its first session start only is captured again, and its record keeps both attempts: the first with no legion tool and the plugin's error text, the second with the tool",
    async () => {
      await captures();
      // The stand-in's state, outside the capture's scratch: whether it has started once. It
      // registers its tool as pi-legion's own is registered (src/tools.ts), inactive until its
      // session start activates it, so Oh My Pi sends it as a tool of the body and not as an
      // xd:// device, as it sends a tool registered without `defaultInactive`.
      const started = path.join(dir, "legion-once", "started");
      mkdirSync(path.dirname(started), { recursive: true });
      const plugin = standInLegion(
        "legion-once",
        `import { existsSync, writeFileSync } from "node:fs";

export default function legion(pi) {
  pi.on("session_start", async () => {
    if (!existsSync(${JSON.stringify(started)})) {
      writeFileSync(${JSON.stringify(started)}, "started\\n");
      throw new Error("the stand-in pi-legion's first session start failed");
    }
    pi.registerTool({
      name: "legion",
      label: "legion",
      description: "A made-up stand-in of the Legion plugin's tool.",
      defaultInactive: true,
      parameters: pi.zod.object({}),
      execute: async () => ({ content: [{ type: "text", text: "ok" }] }),
    });
    await pi.setActiveTools([...pi.getActiveTools(), "legion"]);
  });
}
`
      );
      const { record } = await legionCapture("legion-once-1", plugin);
      expect(record.pluginTools.leftOut).toEqual([]);
      expect(record.pluginTools.tools).toContain("legion");
      const [first, second, ...more] = record.legionAttempts ?? [];
      expect(more).toEqual([]);
      expect([first?.legion, second?.legion]).toEqual([false, true]);
      expect(first?.stderr.join("\n")).toContain(
        "the stand-in pi-legion's first session start failed"
      );
      expect(second?.stderr).toEqual([]);
      // The second body defines the first's tools and the plugin's one more; the record's other
      // fields are the second attempt's, two capture runs and no hint run.
      expect(second?.tools).toBe((first?.tools ?? 0) + 1);
      expect(record.pluginTools.tools).toHaveLength(second?.tools ?? 0);
      expect(record.runs).toBe(2);
    },
    ROW_TIMEOUT_MS
  );

  // Catches: a refusal that takes every Legion pane's body with no legion tool for pi-legion's
  // failure, though its context does not install pi-legion as an extension, so its body never
  // had the tool.
  test(
    "a Legion pane's capture whose context installs pi-legion as skills only is neither refused nor made twice, and records no attempts",
    async () => {
      await captures();
      const { record } = await legionCapture("legion-skills-1", standInLegion("legion-skills"));
      expect(record.context.plugins.map((plugin) => [plugin.name, plugin.as])).toEqual([
        ["pi-legion", "skills only"],
      ]);
      expect({
        attempts: record.legionAttempts,
        runs: record.runs,
        legion: record.pluginTools.legion,
      }).toEqual({ attempts: null, runs: 1, legion: false });
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

  // Catches: a none capture of a recording that lists the skill and another source's copy of its
  // name as <namespace>/<name>, where Oh My Pi lists that copy bare once the tested copy is gone:
  // the capture owed no hint of the bare name, refused Oh My Pi's request for it, and failed; or
  // sent the <namespace>/<name> line, which Oh My Pi no longer lists.
  test(
    "a subagent case whose recording lists the skill and another source's copy of its name as <namespace>/<name> sends one line of the name in none, under that copy's hint, and no <namespace>/<name> line",
    async () => {
      await captures();
      writeTree(path.join(dir, "repos/acme/tools", COMMIT), {
        "skills/alpha/SKILL.md":
          "---\nname: alpha\ndescription: Use for the tools' own alpha list of the made-up project.\n---\n\n# Alpha of tools\n\nOne line.\n",
      });
      // The recording lists tools/alpha after beta, as Oh My Pi sorts a listing by name.
      const toolsLine = "- tools/alpha: Use for the tools' own alpha list, as recorded.";
      const recorded = SUBAGENT_PROMPT.map((block) =>
        block.replace(/^(- beta: .*)$/m, `$1\n${toolsLine}`)
      );
      expect([...listingOf(recorded).keys()]).toEqual(["alpha", "beta", "tools/alpha"]);
      const base = f1Case("subagent-relisted");
      const c: Case = {
        ...base,
        context: "subagent:task",
        profile: {
          ...base.profile,
          skills: [
            ...base.profile.skills,
            {
              repo: "acme/tools",
              commit: COMMIT,
              path: "skills/alpha",
              name: "alpha",
              namespace: "tools",
            },
          ],
          contextFiles: [],
          systemPrompt: recorded,
          tools: SUBAGENT_TOOLS,
        },
      };
      const dirs = { old: path.join(dir, "old"), new: path.join(dir, "new") };
      const none = buildArm(c, "none", dirs, path.join(dir, "arms-relisted"));
      const { body, record } = await captureBody(c, none, {
        omp: binary(),
        outDir: path.join(dir, "out"),
        scratchDir: path.join(dir, "scratch"),
        cacheDir: path.join(dir, "cache"),
        source: localSource(path.join(dir, "repos")),
        models: path.join(dir, "models.yml"),
        hints,
        session: SESSION,
      });
      // acme/tools' alpha is listed under the bare name, and its hint was written for that name.
      expect(none.relisted).toEqual(["tools/alpha"]);
      expect(record.context.skills.map((skill) => skill.name)).toEqual(["beta", "delta", "alpha"]);
      expect(record.hintCalls.map((call) => call.skill)).toEqual(["alpha"]);
      const hint = record.hints.find((each) => each.skill === "alpha")?.hint;
      expect(hint).toStartWith("Use for the alpha work, stand-in hint ");
      // One line of alpha, under that hint, where the recording's alpha line was; no tools/alpha.
      const sent = systemBlocks(body);
      const alphaLines = sent
        .flatMap((block) => block.split("\n"))
        .filter((line) => /^- (tools\/)?alpha: /.test(line));
      expect(alphaLines).toEqual([`- alpha: ${hint}`]);
      expect(sent).toEqual(armPromptBlocks(SUBAGENT_PROMPT, "alpha", hint).blocks);
      expect(record.recordedPrompt?.line).toBe("replaced");
      expect(record.extensionErrors).toEqual([]);
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
      expect(record.captureDigest).toBe(promptDigest(body));

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
      // Well within the 60 s the hint run's turn waits with no hint request or answer.
      expect(performance.now() - started).toBeLessThan(120_000);
      expect(existsSync(path.join(dir, "out-budget"))).toBe(false);
    },
    ROW_TIMEOUT_MS
  );

  // The rows on the hint run's bounds. Each needs real time: the bounds are Oh My Pi's own, on
  // the real binary (it gives a hint request up after 30 s and asks 4 at a time), which no fake
  // clock reaches.

  // Catches: a bound on the whole hint run (the 180 s the turn waited at 525af9d8, "Oh My Pi
  // asked for no hint of skill <name> within 180000 ms"), which a profile of more hints than fit
  // in it, every one of them answered in time, failed on every run with an empty cache.
  test(
    "a hint run longer than 180 s finishes: 32 hints, each answered after 25 s with Oh My Pi asking 4 at a time, are each stored and listed, the turn waiting while hint requests and answers keep coming",
    async () => {
      await captures();
      const repos = path.join(dir, "repos");
      const MANY_COMMIT = "fedcba9876543210fedcba9876543210fedcba98";
      const names = Array.from(
        { length: 30 },
        (_, at) => `chore-${String(at + 1).padStart(2, "0")}`
      );
      writeTree(
        path.join(repos, "acme/skills", MANY_COMMIT),
        Object.fromEntries(
          names.map((name) => [
            `skills/${name}/SKILL.md`,
            `---\nname: ${name}\ndescription: Use when a task asks for the made-up ${name} of the fixture project.\n---\n\n# ${name}\n`,
          ])
        )
      );
      const base = f1Case("many-1");
      const c: Case = {
        ...base,
        profile: {
          ...base.profile,
          skills: [
            ...base.profile.skills,
            ...names.map((name) => ({
              repo: "acme/skills",
              commit: MANY_COMMIT,
              path: `skills/${name}`,
              name,
            })),
          ],
        },
      };
      const slowHints: HintWriter = async (body) => {
        await Bun.sleep(25_000);
        return {
          text: `Use for the ${hintSkillOf(body)} work, slow stand-in hint.`,
          model: body.model,
        };
      };
      const started = performance.now();
      const made = await captureBody(
        c,
        buildArm(c, "old", { old: path.join(dir, "old"), new: path.join(dir, "new") }, dir),
        {
          omp: binary(),
          outDir: path.join(dir, "out-many"),
          scratchDir: path.join(dir, "scratch"),
          // A cache of its own, so every hint is owed and the hint run asks for each.
          cacheDir: path.join(dir, "cache-many"),
          source: localSource(repos),
          models: path.join(dir, "models.yml"),
          hints: slowHints,
          session: SESSION,
        }
      );
      // 8 rounds of 4 hints at 25 s each: more than the 180 s the whole hint run had.
      expect(performance.now() - started).toBeGreaterThan(180_000);
      expect(made.record.timings.hintRunMs).toBeGreaterThan(180_000);
      const listed = Object.fromEntries(skillListing(made.body));
      expect(Object.keys(listed).sort()).toEqual(["alpha", "beta", ...names]);
      for (const [name, line] of Object.entries(listed)) {
        expect(line).toBe(`Use for the ${name} work, slow stand-in hint.`);
      }
      expect(made.record.hints.every((hint) => hint.written)).toBe(true);
    },
    ROW_TIMEOUT_MS
  );

  // Catches: a hint answered after Oh My Pi gave its request up (30 s, the most a gateway call
  // that is tried again can take) reported as Oh My Pi failing to store a hint it keeps, as
  // 525af9d8 reported it ("a hint Oh My Pi keeps, and its database holds no row of it"), because
  // the capture counted a hint as written once the recorder answered, not once Oh My Pi stored its
  // row; and the hints Oh My Pi did store dropped with it.
  test(
    "a hint answered after Oh My Pi's 30 s fails the capture naming that limit and the answer's time, once the turn has waited for the row; the hint Oh My Pi did store is kept in the cache",
    async () => {
      await captures();
      const lateHints: HintWriter = async (body) => {
        const skill = hintSkillOf(body);
        if (skill === "beta") await Bun.sleep(31_000);
        return { text: `Use for the ${skill} work, late stand-in hint.`, model: body.model };
      };
      const c = f1Case("late-1");
      const cacheDir = path.join(dir, "cache-late");
      const failed = await captureBody(
        c,
        buildArm(c, "old", { old: path.join(dir, "old"), new: path.join(dir, "new") }, dir),
        {
          omp: binary(),
          outDir: path.join(dir, "out-late"),
          scratchDir: path.join(dir, "scratch"),
          cacheDir,
          source: localSource(path.join(dir, "repos")),
          models: path.join(dir, "models.yml"),
          hints: lateHints,
          // A shorter wait once hint requests and answers stop, yet longer than Oh My Pi's 30 s,
          // so that the turn is still held when beta's answer comes.
          hintIdleMs: 35_000,
          session: SESSION,
        }
      ).catch((caught: unknown) => caught);
      expect(failed).toBeInstanceOf(Error);
      const message = (failed as Error).message;
      expect(message).toContain(
        'case late-1 arm old: Oh My Pi stored no hint of skill beta: the model wrote "Use for the beta work, late stand-in hint."'
      );
      expect(message).toMatch(
        / 3\d{4} ms after Oh My Pi asked, past the 30000 ms Oh My Pi waits for a hint before it gives the request up/
      );
      // alpha's hint, which Oh My Pi stored, is kept, so a run made again asks for beta's alone.
      const kept = readdirSync(path.join(cacheDir, "hints")).map((file) =>
        JSON.parse(readFileSync(path.join(cacheDir, "hints", file), "utf8"))
      );
      expect(kept.map((hint) => [hint.name, hint.hint])).toEqual([
        ["alpha", "Use for the alpha work, late stand-in hint."],
      ]);
    },
    ROW_TIMEOUT_MS
  );

  // Catches: a profile that installs one text twice, once as `<namespace>/<name>`, whose capture
  // owed a hint of each copy, though Oh My Pi drops the copy whose text it admitted already and so
  // asks one hint: the hint run waited for the second until its bound and failed on every run
  // ("Oh My Pi asked for no hint of skill pi-envoy/plain-words within 180000 ms", 2026-11-24).
  test(
    "a profile that installs beta twice, once as pi-envoy/beta with the same text, lists beta once and owes one hint of it, as Oh My Pi asks",
    async () => {
      await captures();
      const asked: string[] = [];
      const twiceHints: HintWriter = async (body) => {
        const skill = hintSkillOf(body);
        asked.push(skill);
        return { text: `Use for the ${skill} work, stand-in hint.`, model: body.model };
      };
      const base = f1Case("twice-1");
      const c: Case = {
        ...base,
        profile: {
          ...base.profile,
          skills: [
            ...base.profile.skills,
            {
              repo: "acme/skills",
              commit: COMMIT,
              path: "skills/beta",
              name: "beta",
              namespace: "pi-envoy",
            },
          ],
        },
      };
      const made = await captureBody(
        c,
        buildArm(c, "old", { old: path.join(dir, "old"), new: path.join(dir, "new") }, dir),
        {
          omp: binary(),
          outDir: path.join(dir, "out-twice"),
          scratchDir: path.join(dir, "scratch"),
          // A cache of its own, so every hint is owed and the hint run asks for each.
          cacheDir: path.join(dir, "cache-twice"),
          source: localSource(path.join(dir, "repos")),
          models: path.join(dir, "models.yml"),
          hints: twiceHints,
          session: SESSION,
        }
      );
      expect(asked.sort()).toEqual(["alpha", "beta"]);
      expect(Object.fromEntries(skillListing(made.body))).toEqual({
        alpha: "Use for the alpha work, stand-in hint.",
        beta: "Use for the beta work, stand-in hint.",
      });
      expect(made.record.hints.map((hint) => hint.skill).sort()).toEqual(["alpha", "beta"]);
    },
    ROW_TIMEOUT_MS
  );

  // Catches: copies of a name compared otherwise than Oh My Pi's `resolveCollision` compares them
  // (the body and the frontmatter discovery/helpers.ts's `scanSkillsFromDir` keeps of each, at the
  // pinned tag). A copy that opens with an HTML comment before its frontmatter, one with blank
  // lines after its body, and one whose frontmatter names a nested key in camelCase where the
  // other names it in kebab-case are each the same text to Oh My Pi, which drops it and asks no
  // hint of it; the capture owed a hint of each, and the hint run failed once its turn had waited
  // HINT_IDLE_MS for them.
  test(
    "copies of beta that differ from it only in a leading HTML comment, in blank lines after its body, or in the case of a nested key are dropped as Oh My Pi drops them, and owe no hint",
    async () => {
      await captures();
      const COPIES_COMMIT = "2468ace02468ace02468ace02468ace02468ace0";
      const tagged = BETA_SKILL.replace(
        "\n---\n\n# Beta",
        "\nmetadata:\n  made-up-kind: greeting\n---\n\n# Beta"
      );
      writeTree(path.join(dir, "repos", "acme/skills", COPIES_COMMIT), {
        "skills/beta/SKILL.md": tagged,
        "skills/commented/SKILL.md": `<!-- A made-up note before the frontmatter. -->\n${tagged}`,
        "skills/spaced/SKILL.md": `${tagged}\n\n`,
        "skills/camel/SKILL.md": tagged.replace("made-up-kind", "madeUpKind"),
      });
      const beta = (at: string, namespace?: string) => ({
        repo: "acme/skills",
        commit: COPIES_COMMIT,
        path: `skills/${at}`,
        name: "beta",
        ...(namespace === undefined ? {} : { namespace }),
      });
      const asked: string[] = [];
      const copiesHints: HintWriter = async (body) => {
        const skill = hintSkillOf(body);
        asked.push(skill);
        return { text: `Use for the ${skill} work, stand-in hint.`, model: body.model };
      };
      const base = f1Case("copies-1");
      const c: Case = {
        ...base,
        profile: {
          ...base.profile,
          skills: [
            ...base.profile.skills.filter((skill) => skill.name !== "beta"),
            beta("beta"),
            beta("commented", "acme-a"),
            beta("spaced", "acme-b"),
            beta("camel", "acme-c"),
          ],
        },
      };
      const made = await captureBody(
        c,
        buildArm(c, "old", { old: path.join(dir, "old"), new: path.join(dir, "new") }, dir),
        {
          omp: binary(),
          outDir: path.join(dir, "out-copies"),
          scratchDir: path.join(dir, "scratch"),
          // A cache of its own, so every hint is owed and the hint run asks for each.
          cacheDir: path.join(dir, "cache-copies"),
          source: localSource(path.join(dir, "repos")),
          models: path.join(dir, "models.yml"),
          hints: copiesHints,
          session: SESSION,
        }
      );
      expect(asked.sort()).toEqual(["alpha", "beta"]);
      expect(Object.fromEntries(skillListing(made.body))).toEqual({
        alpha: "Use for the alpha work, stand-in hint.",
        beta: "Use for the beta work, stand-in hint.",
      });
      expect(made.record.hints.map((hint) => hint.skill).sort()).toEqual(["alpha", "beta"]);
    },
    ROW_TIMEOUT_MS
  );

  // The rows on a hint reply Oh My Pi does not keep. gamma is a skill of their own, whose
  // description has a first sentence of at least 40 characters within its first 99.
  const GAMMA_COMMIT = "89abcdef0123456789abcdef0123456789abcdef";
  const GAMMA_SKILL =
    "---\nname: gamma\ndescription: Use when a task asks for the gamma report of the made-up project. Not for the alpha checklist or the beta greeting.\n---\n\n# Gamma\n\nReport in one line.\n";
  /** The cut-down description Oh My Pi lists in place of a hint (skill-descriptions.ts at the
   *  pinned tag, `previewSkillDescription`): alpha's 108 characters cut at the last space within
   *  the first 99, beta's 66 whole, and gamma's first sentence. */
  const PREVIEWS = {
    alpha:
      "Use when a task asks for the alpha checklist of the made-up fixture project; it lists the three…",
    beta: "Use when a task asks for the beta greeting of the made-up project.",
    gamma: "Use when a task asks for the gamma report of the made-up project.",
  };
  /** A reply for each skill that Oh My Pi does not keep: one line of 184 characters, two lines,
   *  and an empty one. */
  const UNKEPT_REPLIES: Record<string, string> = {
    alpha:
      "Use for the alpha checklist of the made-up fixture project, when a task asks for its three steps, the reference file it reads first, one line per result, and the stop after step three.",
    beta: "Use for the beta greeting.\nNot for the alpha checklist.",
    gamma: "",
  };
  /** The skill each hint request of these rows named, and its body. */
  const unkeptAsked: string[] = [];
  const unkeptBodies: MessagesBody[] = [];
  /** A stand-in hint writer that answers each skill with its reply above. */
  const unkeptHints: HintWriter = async (body) => {
    const skill = hintSkillOf(body);
    unkeptAsked.push(skill);
    unkeptBodies.push(body);
    return { text: UNKEPT_REPLIES[skill] ?? "", model: body.model };
  };
  let unkeptMade:
    | Promise<{ readonly first: Captured; readonly second: Captured; readonly asked: number }>
    | undefined;
  /** The captures these rows read, made once: the old arm of a case whose profile adds gamma, with
   *  a cache of its own, so every hint is owed; then the old arm of a second case of the same
   *  texts and cache, under another hint model. */
  function unkeptCaptures() {
    unkeptMade ??= (async () => {
      await captures();
      const repos = path.join(dir, "repos");
      writeTree(path.join(repos, "acme/skills", GAMMA_COMMIT), {
        "skills/gamma/SKILL.md": GAMMA_SKILL,
      });
      const models = path.join(dir, "models-unkept.yml");
      writeFileSync(
        models,
        `${MODELS}      - {id: claude-haiku-5-5, name: Claude Haiku 5.5, reasoning: true, input: [text, image], contextWindow: 200000, maxTokens: 64000, cost: {input: 1, output: 5, cacheRead: 0.1, cacheWrite: 1.25}}\n`
      );
      const capture = async (id: string, hintModel?: string): Promise<Captured> => {
        const base = f1Case(id);
        const gamma = {
          repo: "acme/skills",
          commit: GAMMA_COMMIT,
          path: "skills/gamma",
          name: "gamma",
        };
        const c: Case = {
          ...base,
          profile: { ...base.profile, skills: [...base.profile.skills, gamma] },
        };
        const dirs = { old: path.join(dir, "old"), new: path.join(dir, "new") };
        const made = await captureBody(c, buildArm(c, "old", dirs, dir), {
          omp: binary(),
          outDir: path.join(dir, "out-unkept"),
          scratchDir: path.join(dir, "scratch"),
          cacheDir: path.join(dir, "cache-unkept"),
          source: localSource(repos),
          models,
          hints: unkeptHints,
          ...(hintModel === undefined ? {} : { hintModel }),
          session: SESSION,
        });
        return { capture: made, profile: profileHints(made) };
      };
      const first = await capture("unkept-1");
      const asked = unkeptAsked.length;
      const second = await capture("unkept-2", "anthropic/claude-haiku-5-5");
      return { first, second, asked };
    })();
    return unkeptMade;
  }

  // Catches: a hint reply Oh My Pi does not keep (over 160 characters, more than one line, or
  // empty) failing the capture; such a skill listed other than under the cut-down description Oh
  // My Pi lists in place of a hint; or that outcome left out of the cache or the record.
  test(
    "a hint reply Oh My Pi does not keep, over 160 characters, two lines or empty, fails no capture: the skill is listed under Oh My Pi's cut-down description, asked once, and the cache and the record say no hint was kept, the reply and why",
    async () => {
      const { first, asked } = await unkeptCaptures();
      const { record, body } = first.capture;
      expect(Object.fromEntries(skillListing(body))).toEqual(PREVIEWS);
      // Oh My Pi asked once for each skill's hint, and the capture run asked for none.
      expect(unkeptAsked.slice(0, asked).sort()).toEqual(["alpha", "beta", "gamma"]);
      expect(record.runs).toBe(2);
      expect(record.otherRequests).toEqual([]);
      // Each request asks for at most 1024 tokens and names no temperature: claude-sonnet-5-5
      // takes none, so the API's default holds.
      expect(
        unkeptBodies
          .slice(0, asked)
          .map((each) => ({ temperature: each.temperature, maxTokens: each.max_tokens }))
      ).toEqual(Array(3).fill({ temperature: undefined, maxTokens: 1024 }));
      expect(record.hints).toMatchObject([
        {
          skill: "alpha",
          hint: PREVIEWS.alpha,
          written: true,
          kept: false,
          reply: UNKEPT_REPLIES.alpha,
          reason: "the reply is 184 characters long, and Oh My Pi keeps a hint of at most 160",
        },
        {
          skill: "beta",
          hint: PREVIEWS.beta,
          written: true,
          kept: false,
          reply: UNKEPT_REPLIES.beta,
          reason: "the reply is more than one line",
        },
        {
          skill: "gamma",
          hint: PREVIEWS.gamma,
          written: true,
          kept: false,
          reply: "",
          reason: "the reply is empty",
        },
      ]);
      for (const entry of record.hints) {
        // Oh My Pi's record of the reply: the skill's key, its rule's revision and the hint model,
        // holding the cut-down description, as the capture's scratch database has it.
        expect(entry.ompKey).toMatch(/^[0-9a-f]{64}:v\d+:anthropic\/claude-sonnet-5-5$/);
        expect(first.profile.get(entry.ompKey)).toBe(entry.hint);
        const cached = JSON.parse(
          readFileSync(path.join(dir, "cache-unkept", "hints", `${entry.key}.json`), "utf8")
        );
        expect(cached).toMatchObject({
          name: entry.skill,
          hint: entry.hint,
          ompKey: entry.ompKey,
          kept: false,
          reply: entry.reply as string,
          reason: entry.reason as string,
        });
      }
      // Oh My Pi stored no hint.
      expect([...first.profile.keys()].filter((key) => /^[0-9a-f]{64}$/.test(key))).toEqual([]);
    },
    ROW_TIMEOUT_MS
  );

  // Catches: a later capture of a skill text whose reply Oh My Pi did not keep asking for a hint
  // again, or listing that skill other than the first capture did, under another hint model too.
  test(
    "a later capture of the same skill texts reads the cached outcome of a reply Oh My Pi did not keep and asks for no hint, under another hint model too",
    async () => {
      const { first, second, asked } = await unkeptCaptures();
      const { record, body } = second.capture;
      expect(unkeptAsked).toHaveLength(asked);
      expect(record.hintModel).toBe("anthropic/claude-haiku-5-5");
      expect(record.runs).toBe(1);
      expect(record.hintCalls).toEqual([]);
      expect(record.otherRequests).toEqual([]);
      expect(skillListing(body)).toEqual(skillListing(first.capture.body));
      expect(record.hints.map(({ skill, written, kept }) => ({ skill, written, kept }))).toEqual(
        ["alpha", "beta", "gamma"].map((skill) => ({ skill, written: false, kept: false }))
      );
      // The capture's scratch database holds Oh My Pi's record of each reply under this capture's
      // own hint model, so Oh My Pi asked it for no hint.
      for (const entry of record.hints) {
        const own = entry.ompKey.replace(/claude-sonnet-5-5$/, "claude-haiku-5-5");
        expect(second.profile.get(own)).toBe(entry.hint);
      }
    },
    ROW_TIMEOUT_MS
  );

  // The rows on a recorded listing line and on the draw (capture.ts's header, "A recorded listing
  // line" and "The draw"). Each captures cases of its own ids under out-recorded.
  /** The lines a made-up session recorded for alpha and beta. */
  const ALPHA_LINE = "Use for the alpha checklist, as the session listed it.";
  const BETA_LINE = "Use for the beta greeting, as the session listed it.";
  /** The capture options of the rows below: each capture under `outDir` of `dir`, against the
   *  shared cache, with `extra` over them. */
  const optionsWith = (outDir: string, extra: Partial<CaptureOptions> = {}): CaptureOptions => ({
    omp: binary(),
    outDir: path.join(dir, outDir),
    scratchDir: path.join(dir, "scratch"),
    cacheDir: path.join(dir, "cache"),
    source: localSource(path.join(dir, "repos")),
    models: path.join(dir, "models.yml"),
    hints,
    session: SESSION,
    ...extra,
  });
  /** The capture of case `id`'s arm `arm` with `extra` options, against the cache `cacheDir`
   *  (the shared one, which holds each text's hint of draw 1 once `captures` has run). */
  const recordedCapture = async (
    id: string,
    arm: ArmName,
    extra: Partial<CaptureOptions>,
    cacheDir = path.join(dir, "cache")
  ): Promise<Captured> => {
    await captures();
    const c = f1Case(id);
    const dirs = { old: path.join(dir, "old"), new: path.join(dir, "new") };
    const made = await captureBody(
      c,
      buildArm(c, arm, dirs, dir),
      optionsWith("out-recorded", { cacheDir, ...extra })
    );
    return { capture: made, profile: profileHints(made) };
  };
  /** Oh My Pi's key of `skill`'s old text: the one it stored c1's drawn hint under. */
  const ompKeyOf = async (skill: string) =>
    (await captures()).old.capture.record.hints.find((hint) => hint.skill === skill)?.ompKey;

  // Catches: a skill listed under a drawn hint though its session recorded its line, or a line
  // written under another key than Oh My Pi's, so that Oh My Pi lists the cached hint or asks a
  // model; and a recorded cut-down description taken for no line.
  test(
    "a skill with a recorded line is listed under that line in place of its cached hint, written under Oh My Pi's key of the skill text; a recorded cut-down description is listed as that cut-down description; no model is asked; and the record names each line's source",
    async () => {
      await captures();
      const asked = hintRequests.length;
      const oldText = skillFrontmatter(path.join(dir, "old")).description;
      const { capture, profile } = await recordedCapture("recorded-1", "old", {
        recordedLines: [
          { name: "beta", text: BETA_LINE, from: "session" },
          // The arm's own skill: its line names the text it was recorded for, the arm's.
          { name: "alpha", text: PREVIEWS.alpha, from: "subagent 0f1e2d3c", description: oldText },
        ],
      });
      expect(Object.fromEntries(skillListing(capture.body))).toEqual({
        alpha: PREVIEWS.alpha,
        beta: BETA_LINE,
      });
      // The cache holds each text's hint of draw 1, and so Oh My Pi's key of it: no hint run.
      expect(hintRequests).toHaveLength(asked);
      expect(capture.record.runs).toBe(1);
      expect(capture.record.hintCalls).toEqual([]);
      expect(capture.record.otherRequests).toEqual([]);
      const [alphaKey, betaKey] = [await ompKeyOf("alpha"), await ompKeyOf("beta")];
      expect(alphaKey).toMatch(/^[0-9a-f]{64}$/);
      expect(capture.record.hints).toEqual([
        {
          skill: "alpha",
          hint: PREVIEWS.alpha,
          key: expect.any(String),
          ompKey: alphaKey ?? "",
          written: false,
          source: "recorded (subagent 0f1e2d3c)",
        },
        {
          skill: "beta",
          hint: BETA_LINE,
          key: expect.any(String),
          ompKey: betaKey ?? "",
          written: false,
          source: "recorded (session)",
        },
      ]);
      expect(profile.get(alphaKey ?? "")).toBe(PREVIEWS.alpha);
      expect(profile.get(betaKey ?? "")).toBe(BETA_LINE);
    },
    ROW_TIMEOUT_MS
  );

  // Catches: a recorded line of a skill text no hint was drawn for left to Oh My Pi's preview, or
  // drawn from the model, for want of Oh My Pi's key.
  test(
    "a recorded line whose key the runner has not read is Oh My Pi's answer to its hint request in the hint run, no model asked; the key Oh My Pi stores it under is kept in the key cache, and the next capture reads it there and makes no hint run",
    async () => {
      await captures();
      const asked = hintRequests.length;
      const cacheDir = path.join(dir, "cache-recorded");
      const oldText = skillFrontmatter(path.join(dir, "old")).description;
      const recordedLines = [
        { name: "alpha", text: ALPHA_LINE, from: "session", description: oldText },
        { name: "beta", text: BETA_LINE, from: "session" },
      ];
      const first = await recordedCapture("recorded-2", "old", { recordedLines }, cacheDir);
      const second = await recordedCapture("recorded-3", "old", { recordedLines }, cacheDir);
      expect(hintRequests).toHaveLength(asked);
      expect([first.capture.record.runs, second.capture.record.runs]).toEqual([2, 1]);
      for (const { capture, profile } of [first, second]) {
        expect(Object.fromEntries(skillListing(capture.body))).toEqual({
          alpha: ALPHA_LINE,
          beta: BETA_LINE,
        });
        expect(capture.record.hintCalls).toEqual([]);
        expect(capture.record.otherRequests).toEqual([]);
        for (const [skill, line] of [
          ["alpha", ALPHA_LINE],
          ["beta", BETA_LINE],
        ] as const) {
          const entry = capture.record.hints.find((hint) => hint.skill === skill);
          // Oh My Pi's key of the skill text: the one it stored c1's drawn hint under.
          expect(entry?.ompKey).toBe((await ompKeyOf(skill)) ?? "");
          expect(profile.get(entry?.ompKey ?? "")).toBe(line);
          const kept = path.join(cacheDir, "hint-keys", `${entry?.key}.json`);
          expect(JSON.parse(readFileSync(kept, "utf8"))).toMatchObject({
            name: skill,
            ompKey: entry?.ompKey,
          });
        }
      }
      // No hint was drawn, so none is cached.
      expect(existsSync(path.join(cacheDir, "hints"))).toBe(false);
    },
    ROW_TIMEOUT_MS
  );

  // Catches: a recorded line listed for an arm whose text the session never had, or for the
  // arm's own skill with no text named, which would list a line made from another text.
  test(
    "an arm whose text differs from the one the session's line was recorded for, and an arm's own skill whose line names no text, get the hint of their draw, while the profile's other skill keeps its recorded line",
    async () => {
      const { old, fresh } = await captures();
      const oldText = skillFrontmatter(path.join(dir, "old")).description;
      const beta = { name: "beta", text: BETA_LINE, from: "session" };
      const changed = await recordedCapture("recorded-4", "new", {
        recordedLines: [
          { name: "alpha", text: ALPHA_LINE, from: "session", description: oldText },
          beta,
        ],
      });
      const unnamed = await recordedCapture("recorded-5", "old", {
        recordedLines: [{ name: "alpha", text: ALPHA_LINE, from: "session" }, beta],
      });
      for (const [each, drawn] of [
        [changed, fresh],
        [unnamed, old],
      ] as const) {
        const listing = skillListing(each.capture.body);
        expect(listing.get("alpha")).toBe(profileHint(drawn, "alpha") as string);
        expect(listing.get("beta")).toBe(BETA_LINE);
        expect(each.capture.record.hints.map(({ skill, source }) => ({ skill, source }))).toEqual([
          { skill: "alpha", source: "drawn (draw 1)" },
          { skill: "beta", source: "recorded (session)" },
        ]);
      }
    },
    ROW_TIMEOUT_MS
  );

  // Catches: a second draw that reads draw 1's cached hint, so that two draws are one; one that
  // writes over draw 1's file; or one that draws a hint where the session recorded a line.
  test(
    "draw 2 asks the hint model again for a skill text draw 1 has cached, caches that hint in a file of its own, and keeps a recorded line unchanged",
    async () => {
      const { old } = await captures();
      const asked = hintRequests.length;
      const { capture, profile } = await recordedCapture("recorded-6", "old", {
        hintDraw: 2,
        recordedLines: [{ name: "beta", text: BETA_LINE, from: "session" }],
      });
      expect(hintRequests.slice(asked)).toEqual(["alpha"]);
      const drawOne = old.capture.record.hints.find((hint) => hint.skill === "alpha");
      const drawTwo = capture.record.hints.find((hint) => hint.skill === "alpha");
      expect(drawTwo).toMatchObject({
        written: true,
        source: "drawn (draw 2)",
        ompKey: drawOne?.ompKey ?? "",
      });
      expect(drawTwo?.hint).not.toBe(drawOne?.hint);
      expect(drawTwo?.key).not.toBe(drawOne?.key);
      const listing = skillListing(capture.body);
      expect(listing.get("alpha")).toBe(drawTwo?.hint as string);
      expect(listing.get("beta")).toBe(BETA_LINE);
      expect(profile.get(drawOne?.ompKey ?? "")).toBe(drawTwo?.hint as string);
      const cached = (key: string | undefined) =>
        JSON.parse(readFileSync(path.join(dir, "cache", "hints", `${key}.json`), "utf8"));
      expect(cached(drawOne?.key)).toMatchObject({ hint: drawOne?.hint });
      expect(cached(drawOne?.key).draw).toBeUndefined();
      expect(cached(drawTwo?.key)).toMatchObject({
        hint: drawTwo?.hint,
        ompKey: drawOne?.ompKey,
        draw: 2,
      });
    },
    ROW_TIMEOUT_MS
  );

  // Catches: a context file of a parent of the working directory that Oh My Pi never reads (the
  // capture run at the checkout's root), one shown at the capture's scratch path, or a file of the
  // working directory not named AGENTS.md or CLAUDE.md sent in the appended system prompt.
  test(
    "a case whose working directory is below its checkout's root is captured there: Oh My Pi reads the working directory's context file and its parents', the farther first, each shown at the session's path",
    async () => {
      await captures();
      const rootRules = "# Made-up root rules\n\nRun the tests before each commit.\n";
      const vmRules = "# Made-up VM rules\n\nName the VM in each reply.\n";
      writeTree(path.join(dir, "repos", "acme/project", COMMIT), {
        "AGENTS.md": rootRules,
        "agents/vm/.github/copilot-instructions.md": vmRules,
      });
      const base = f1Case("c-levels");
      const c: Case = {
        ...base,
        profile: {
          ...base.profile,
          contextFiles: [
            ...base.profile.contextFiles,
            { repo: "acme/project", commit: COMMIT, path: "AGENTS.md" },
            {
              repo: "acme/project",
              commit: COMMIT,
              path: "agents/vm/.github/copilot-instructions.md",
            },
          ],
          workingDirectory: "agents/vm",
        },
      };
      const cwd = `${SESSION_CWD}/agents/vm`;
      const made = await captureBody(
        c,
        buildArm(
          c,
          "old",
          { old: path.join(dir, "old"), new: path.join(dir, "new") },
          path.join(dir, "arms-levels")
        ),
        optionsWith("out-levels", {
          session: sessionFacts(
            { ...SESSION_HEAD, head: { ...SESSION_HEAD.head, cwd, reminders: [] } },
            {
              turn: FIRST_TURN,
              date: { value: "2026-11-20", from: "the UTC date of the case's step in the session" },
              devbox: true,
              devboxClaudeConfigDir: DEVBOX_CLAUDE,
            }
          ),
        })
      );
      const { record } = made;
      const checkout = path.join(record.scratch, "work");
      // Oh My Pi ran in the working directory inside the checkout, and read both files there.
      const raw = readFileSync(record.harness.rawBody, "utf8");
      expect(raw).toContain(`current working directory: '${checkout}/agents/vm'`);
      const rawSystem = systemBlocks(JSON.parse(raw)).join("\n");
      expect(rawSystem).toContain(`<file path="${checkout}/AGENTS.md">`);
      expect(rawSystem).toContain(
        `<file path="${checkout}/agents/vm/.github/copilot-instructions.md">`
      );
      // The body the replays send shows each at the session's path, the farther first.
      const system = systemBlocks(made.body).join("\n");
      const rootBlock = `<file path="${SESSION_CWD}/AGENTS.md">`;
      const vmBlock = `<file path="${cwd}/.github/copilot-instructions.md">`;
      expect(system).toContain(rootBlock);
      expect(system).toContain("Run the tests before each commit.");
      expect(system).toContain(vmBlock);
      expect(system).toContain("Name the VM in each reply.");
      expect(system.indexOf(rootBlock)).toBeLessThan(system.indexOf(vmBlock));
      expect(JSON.stringify(made.body)).not.toContain(record.scratch);
      expect(record.context.contextFiles.map((file) => file.as)).toEqual([
        "user level",
        "working directory",
        "working directory",
      ]);
      expect(record.harness.replaced).toContainEqual({
        what: "a context file's path in the checkout",
        count: 1,
      });
    },
    ROW_TIMEOUT_MS
  );

  // Catches: a none arm of dispatch-first that removes the file the Envoy plugin reads as it
  // loads ("Failed to load extension"), old and new arms whose bodies hold no block of the
  // skill though the plugin puts it into every request of a session with Dispatch, and a record
  // that does not say whether the session had Dispatch, which that block rests on.
  test(
    "a dispatch-first case's none arm loads the Envoy plugin and sends no block of the skill; its old and new arms each send the plugin's block of their own text; each record says whether the session had Dispatch, and why",
    async () => {
      await captures();
      const plugins = builtPlugins(["pi-envoy"]);
      const skill = readFileSync(
        path.join(dir, "repos/acme/skills", COMMIT, "skills/dispatch-first/SKILL.md"),
        "utf8"
      );
      const cut = skill.replace(
        "## Search before you act",
        "## Search the made-up tracker before you act"
      );
      expect(cut).not.toBe(skill);
      const dirs = { old: path.join(dir, "df-old"), new: path.join(dir, "df-new") };
      writeTree(dirs.old, { "SKILL.md": skill });
      writeTree(dirs.new, { "SKILL.md": cut });
      const base = f1Case("dispatch-first-1");
      const lines = skill.trimEnd().split("\n");
      const c: Case = {
        ...base,
        skill: "dispatch-first",
        line: {
          text: "## Search before you act",
          lineNumber: lines.indexOf("## Search before you act") + 1,
          lineCount: lines.length,
        },
        profile: { ...base.profile, skills: [...base.profile.skills, DISPATCH_FIRST], plugins },
        // F1's prompt, then a read of skill://dispatch-first and its result.
        messages: [
          structuredClone(messages[0] ?? {}),
          {
            role: "assistant",
            content: [
              {
                type: "toolCall",
                id: "toolu_df",
                name: "read",
                arguments: { path: "skill://dispatch-first" },
              },
            ],
            stopReason: "toolUse",
            timestamp: Number(messages[0]?.timestamp) + 1000,
          },
          {
            role: "toolResult",
            toolCallId: "toolu_df",
            toolName: "read",
            content: [
              {
                type: "text",
                text: `[Skill file: /skills/dispatch-first/SKILL.md]\n${skill.trimEnd()}`,
              },
            ],
            isError: false,
            timestamp: Number(messages[0]?.timestamp) + 2000,
          },
        ],
        skillReads: [2],
      };
      const sent = new Map<string, string[]>();
      // Whether the session had Dispatch, which the block rests on: a dev box's session whose
      // messages show neither the block nor a Dispatch call (arms.ts's `dispatchOf`).
      const assumed: DispatchAnswer = {
        from: "assumed",
        why: "its context is devbox, and none of its messages holds the dispatch-first block or a Dispatch call, so the runner takes it that the session had Dispatch",
      };
      // none first: the plugin must load without the skill's own copy.
      for (const name of ["none", "old", "new"] as const) {
        const { body, record } = await captureBody(
          c,
          buildArm(c, name, dirs, path.join(dir, "arms-dispatch")),
          optionsWith("out-dispatch")
        );
        // The plugin loads in every arm, none among them.
        expect({ arm: name, errors: record.extensionErrors }).toEqual({ arm: name, errors: [] });
        expect(record.context.plugins.map((plugin) => [plugin.name, plugin.as])).toEqual([
          ["pi-envoy", "extension"],
        ]);
        expect(record.pluginTools.envoy).toContain("envoy_send");
        expect({ arm: name, dispatch: record.dispatch }).toEqual({ arm: name, dispatch: assumed });
        const blocks = texts(body.messages).filter((text) =>
          text.includes("<dispatch-first-skill>")
        );
        sent.set(name, blocks);
        if (name === "none") {
          expect(blocks).toEqual([]);
          // The hidden stand-in the plugin loads with reaches no part of the body.
          expect(JSON.stringify(body)).not.toContain("dispatch-first");
          expect(skillListing(body).has("dispatch-first")).toBe(false);
          expect(record.context.skills).toContainEqual({
            name: "dispatch-first",
            from: "stand-in",
          });
          expect(record.injections).toEqual([
            { by: "plugin", plugin: "pi-envoy", name: "dispatch-first", at: null },
          ]);
          continue;
        }
        // The block as the plugin writes it of the arm's file, in the first user message, where
        // the plugin puts it.
        const block = readDispatchFirstContext(path.join(dirs[name], "SKILL.md"));
        expect(blocks).toHaveLength(1);
        expect(blocks[0]).toEndWith(block);
        const first = (body.messages as { role: string; content: unknown }[])[0];
        expect(first?.role).toBe("user");
        expect(texts(first?.content)).toContain(blocks[0] ?? "");
        expect(skillListing(body).get("dispatch-first")).toStartWith(
          "Use for the dispatch-first work, stand-in hint "
        );
        expect(record.injections).toEqual([
          { by: "plugin", plugin: "pi-envoy", name: "dispatch-first", at: 0 },
        ]);
      }
      expect(sent.get("old")?.[0]).toContain("## Search before you act");
      expect(sent.get("new")?.[0]).toContain("## Search the made-up tracker before you act");
      expect(sent.get("new")?.[0]).not.toContain("## Search before you act");
    },
    ROW_TIMEOUT_MS
  );

  // Catches: a subagent case of a skill Oh My Pi autoloads into it whose arms send the recorded
  // text of that block, each, and none sends it too.
  test(
    "a subagent case's autoloaded block of the skill reaches each arm's body with that arm's text, and none's body holds none",
    async () => {
      await captures();
      const installed = path.dirname(skillHeaderPath(messages[2]?.content?.[0]?.text ?? "") ?? "");
      const autoloadPath = `${installed}/SKILL.md`;
      const bodyOf = (text: string) => text.slice(text.indexOf("\n---", 3) + 4).trim();
      const block: SessionMessage = {
        role: "custom",
        customType: "skill-prompt",
        content: [{ type: "text", text: `${bodyOf(OLD_SKILL)}\n\n---\n\nSkill: ${autoloadPath}` }],
        display: false,
        details: { name: "alpha", path: autoloadPath },
        attribution: "agent",
        timestamp: Number(messages[0]?.timestamp) - 1000,
      };
      const base = f1Case("autoload-1");
      const c: Case = {
        ...base,
        context: "subagent:alpha-runner",
        profile: {
          ...base.profile,
          contextFiles: [],
          systemPrompt: SUBAGENT_PROMPT,
          tools: SUBAGENT_TOOLS,
        },
        messages: [block, ...messages.slice(0, 7)],
        skillReads: [3, 5],
      };
      const dirs = { old: path.join(dir, "old"), new: path.join(dir, "new") };
      // new first: the old arm's text is the recorded one.
      for (const name of ["new", "none", "old"] as const) {
        const { body, record } = await captureBody(
          c,
          buildArm(c, name, dirs, path.join(dir, "arms-autoload")),
          optionsWith("out-autoload")
        );
        const autoloadedTexts = texts(body.messages).filter((text) =>
          text.includes(`Skill: ${autoloadPath}`)
        );
        if (name === "none") {
          expect(autoloadedTexts).toEqual([]);
          expect(record.injections).toEqual([{ by: "autoload", path: autoloadPath, at: null }]);
          expect(record.dispatch?.from).toBe("assumed");
          continue;
        }
        const own = name === "old" ? OLD_SKILL : NEW_SKILL;
        expect(autoloadedTexts).toHaveLength(1);
        expect(autoloadedTexts[0]).toEndWith(`${bodyOf(own)}\n\n---\n\nSkill: ${autoloadPath}`);
        expect(record.injections).toEqual([{ by: "autoload", path: autoloadPath, at: 0 }]);
        expect(autoloadedTexts[0]?.includes("Then stop.")).toBe(name === "new");
      }
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

// Catches: a rule that misses a harness string, touches a recorded prompt, reads a `$` of a
// session's value as a replacement pattern, or lets a body through that still names the scratch.
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
  // A `$` in a value the rules write is written as it is: `$&` and `$1` there are no patterns.
  const dollar = "/srv/acme/$&$1";
  const dollarFacts = sessionFacts(
    { ...SESSION_HEAD, head: { ...SESSION_HEAD.head, cwd: dollar, reminders: [] } },
    { turn: FIRST_TURN, date: STEP_DATE, devbox: true, devboxClaudeConfigDir: DEVBOX_CLAUDE }
  );
  const dollared = rewriteBody(body, ompHarnessRules(work, config, dollarFacts, true), [
    "/scratch/run-1",
  ]);
  expect(dollared.faults).toEqual([]);
  const dollaredText = JSON.stringify(dollared.body);
  expect(dollaredText).toContain(`<file path=\\"${dollar}/AGENTS.md\\">`);
  expect(dollaredText).toContain(`I am in ${dollar}/src.`);
  expect(dollaredText).toContain(`current working directory: '${dollar}'`);
});

// Catches: a parent's context file, or another mention of the capture's checkout, left naming the
// capture's scratch when the working directory is below the checkout's root.
test("the harness rules of a capture whose working directory is below its checkout's root name the session's checkout for a parent's context file and every other mention of the checkout", () => {
  const checkout = "/scratch/run-1/legion-retro/captures/abc/work";
  const work = `${checkout}/agents/vm`;
  const cwd = `${SESSION_CWD}/agents/vm`;
  const facts = sessionFacts(
    { ...SESSION_HEAD, head: { ...SESSION_HEAD.head, cwd, reminders: [] } },
    { turn: FIRST_TURN, date: STEP_DATE, devbox: false, devboxClaudeConfigDir: undefined }
  );
  const body = {
    model: "claude-sonnet-5-5",
    system: [
      {
        type: "text",
        text: `<file path="${checkout}/AGENTS.md">\nRoot rules.\n</file>\n<file path="${work}/AGENTS.md">\nVM rules.\n</file>`,
      },
    ],
    messages: [
      {
        role: "assistant",
        content: [{ type: "text", text: `I read ${checkout}/README.md and ${work}/notes.md.` }],
      },
    ],
  };
  const built = rewriteBody(
    body,
    ompHarnessRules(work, undefined, facts, true, { dir: checkout, workingDirectory: "agents/vm" }),
    ["/scratch/run-1"]
  );
  expect(built.faults).toEqual([]);
  const system = String(Object(built.body.system as unknown[])[0]?.text);
  expect(system).toContain(`<file path="${SESSION_CWD}/AGENTS.md">\nRoot rules.`);
  expect(system).toContain(`<file path="${cwd}/AGENTS.md">\nVM rules.`);
  expect(JSON.stringify(built.body)).toContain(
    `I read ${SESSION_CWD}/README.md and ${cwd}/notes.md.`
  );
  expect(built.replaced).toContainEqual({
    what: "a context file's path in the checkout",
    count: 1,
  });
  expect(built.replaced).toContainEqual({ what: "another mention of the checkout", count: 1 });
  // The session's checkout is its working directory less that path; a working directory that
  // does not end with it (a neutral one) has its checkout as many levels above it as the path
  // has parts.
  expect(sessionCheckout(`${cwd}/`, "agents/vm")).toBe(SESSION_CWD);
  expect(sessionCheckout("/srv/acme/other", "agents/vm")).toBe("/srv");
});

// Catches: a session checkout at the file system's root joined to a path as `//`.
test("the harness rules of a capture whose session's checkout is the file system's root name it with one slash", () => {
  const checkout = "/scratch/run-1/legion-retro/captures/abc/work";
  const work = `${checkout}/agents`;
  // No session is named: the working directory is NEUTRAL_CWD, one level below `/`.
  const facts = sessionFacts(undefined, {
    turn: FIRST_TURN,
    date: STEP_DATE,
    devbox: false,
    devboxClaudeConfigDir: undefined,
  });
  const body = {
    model: "claude-sonnet-5-5",
    system: [{ type: "text", text: `<file path="${checkout}/AGENTS.md">\nRoot rules.\n</file>` }],
    messages: [
      {
        role: "assistant",
        content: [{ type: "text", text: `I read ${checkout}/README.md in ${checkout}.` }],
      },
    ],
  };
  const built = rewriteBody(
    body,
    ompHarnessRules(work, undefined, facts, true, { dir: checkout, workingDirectory: "agents" }),
    ["/scratch/run-1"]
  );
  expect(built.faults).toEqual([]);
  const text = JSON.stringify(built.body);
  expect(text).toContain('<file path=\\"/AGENTS.md\\">');
  expect(text).toContain("I read /README.md in /.");
});

// Catches: a head read twice from the store, a hash the store does not hold taken as held, a
// failed read taken for a neutral session, or a second store's session at the same path refused
// for the first store's head file.
test("sessionHeads reads the heads it lacks in one query through the store command, saves each under its store and path hash, and reads a saved one back, one saved under its path hash alone too", async () => {
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
  expect(existsSync(path.join(sessions, `store_a-${held}.json`))).toBe(true);
  // Read again: both from their files, and no query.
  const again = await sessionHeads(command, "store_a", [held, missing], sessions);
  expect(readFileSync(calls, "utf8")).toBe("store_a\n");
  expect(again.get(held)?.head).toEqual(heads.get(held)?.head);
  // Another store's session at the same path gets its own head, read from that store.
  const other = await sessionHeads(command, "store_b", [held], sessions);
  expect(readFileSync(calls, "utf8")).toBe("store_a\nstore_b\n");
  expect(other.get(held)?.head).toMatchObject({ store: "store_b", pathHash: held, held: true });
  expect(existsSync(path.join(sessions, `store_b-${held}.json`))).toBe(true);
  // A head an earlier run saved under its path hash alone is read back with no query by its own
  // store, and is no head of another store's session at that path.
  const earlier = pathHash("/sessions/earlier.jsonl");
  const saved = { ...heads.get(held)?.head, pathHash: earlier };
  writeFileSync(path.join(sessions, `${earlier}.json`), JSON.stringify(saved));
  const kept = await sessionHeads(command, "store_a", [earlier], sessions);
  expect(kept.get(earlier)?.head).toEqual(saved as SavedHead["head"]);
  expect(readFileSync(calls, "utf8")).toBe("store_a\nstore_b\n");
  const another = await sessionHeads(command, "store_b", [earlier], sessions);
  expect(another.get(earlier)?.head).toMatchObject({ store: "store_b", held: false });
  expect(readFileSync(calls, "utf8")).toBe("store_a\nstore_b\nstore_b\n");
  // A command that fails is a failed read, never a neutral session.
  const failing = path.join(root, "failing.sh");
  writeFileSync(failing, "#!/bin/sh\necho 'permission denied for table' >&2\nexit 3\n", {
    mode: 0o755,
  });
  await expect(
    sessionHeads(failing, "store_a", [pathHash("/sessions/other.jsonl")], sessions)
  ).rejects.toThrow("exited 3: permission denied for table");
});

// Catches: a store command that hangs holding the capture's read of the session heads for ever,
// a read with no time limit of its own.
test("a store command that prints nothing for the read's quiet limit is ended, and the read fails naming the command and the limit", async () => {
  const root = path.join(dir, "heads-quiet");
  mkdirSync(root, { recursive: true });
  // It reads its query, then waits in a child of its own, which holds its output open.
  const hung = path.join(root, "hung.sh");
  writeFileSync(hung, "#!/bin/sh\ncat >/dev/null\nsleep 5\n", { mode: 0o755 });
  const started = performance.now();
  await expect(
    sessionHeads(hung, "store_a", [pathHash("/sessions/other.jsonl")], root, 200)
  ).rejects.toThrow(
    `the store command ${hung}'s read of the session heads printed nothing for 200 ms on store_a, so it was ended (STORE_QUIET_MS)`
  );
  expect(performance.now() - started).toBeLessThan(3_000);
});

// Catches: a store command that prints its answer and closes its output and error output, then
// does not exit, waited for with no limit (`child.exited` after the limit was cleared).
test("a store command that closes its output and does not exit is ended at the read's quiet limit too", async () => {
  const root = path.join(dir, "heads-lingers");
  mkdirSync(root, { recursive: true });
  const lingers = path.join(root, "lingers.sh");
  writeFileSync(
    lingers,
    "#!/bin/sh\ncat >/dev/null\nprintf ' value \\n-------\\n(0 rows)\\n\\n'\nexec >&- 2>&-\nsleep 5\n",
    { mode: 0o755 }
  );
  const started = performance.now();
  await expect(
    sessionHeads(lingers, "store_a", [pathHash("/sessions/other.jsonl")], root, 300)
  ).rejects.toThrow(
    `the store command ${lingers}'s read of the session heads printed nothing for 300 ms on store_a, so it was ended (STORE_QUIET_MS)`
  );
  expect(performance.now() - started).toBeLessThan(3_000);
});

// Catches: the heads read waiting for the store command's error output to close though the
// command exited 0, so one whose child (a background process of the wrapper) still holds that
// output open failed at the quiet limit, where build.ts's reads of the store succeed.
test("a store command that exits 0 while a process it started holds its error output open is done: the heads are read, as every store read reads them", async () => {
  const root = path.join(dir, "heads-exit-0");
  mkdirSync(root, { recursive: true });
  const leaves = path.join(root, "leaves.sh");
  writeFileSync(
    leaves,
    "#!/bin/sh\ncat >/dev/null\nsleep 5 >/dev/null &\nprintf ' value \\n-------\\n(0 rows)\\n\\n'\n",
    { mode: 0o755 }
  );
  const hash = pathHash("/sessions/other.jsonl");
  const started = performance.now();
  const heads = await sessionHeads(leaves, "store_a", [hash], root, 1_000);
  expect(heads.get(hash)?.head.held).toBe(false);
  expect(performance.now() - started).toBeLessThan(3_000);
});

// Catches: `omp --version` run with no limit, so a binary that never answers held the capture,
// and the run, for ever.
test("an `omp --version` that never exits is ended at its limit, and the capture fails naming the command and the limit", async () => {
  const root = path.join(dir, "omp-version-hangs");
  mkdirSync(root, { recursive: true });
  const omp = path.join(root, "omp");
  writeFileSync(omp, "#!/bin/sh\nsleep 5\n", { mode: 0o755 });
  const started = performance.now();
  await expect(ompVersion(omp, 200)).rejects.toThrow(
    `\`${omp} --version\` ran past its limit of 200 ms, so it was ended`
  );
  expect(performance.now() - started).toBeLessThan(3_000);
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

/** Saves a capture of `c`'s arm old under `outDir`, as captureBody reads one back: a body, and a
 *  record whose context installed alpha from the arm and each of `copies` from its repository. */
function saveOldCapture(
  outDir: string,
  c: Case,
  copies: readonly ContextRecord["skills"][number][]
): { body: string; record: string } {
  const files = capturePaths(outDir, c.id, "old");
  mkdirSync(outDir, { recursive: true });
  writeFileSync(files.body, JSON.stringify({ model: "claude-sonnet-5-5", messages: [] }));
  writeFileSync(
    files.record,
    JSON.stringify({
      case: c.id,
      arm: "old",
      harness: { head: null, replaced: [] },
      context: { skills: [{ name: "alpha", from: "arm" }, ...copies] },
    })
  );
  return files;
}

/** The record of the copy of acme/skills' `skills/<name>` at `commit` a context installs, under
 *  its cache key, or under `key`. */
function skillsCopy(
  name: string,
  commit = COMMIT,
  key = cacheKey("acme/skills", commit, `skills/${name}`)
): ContextRecord["skills"][number] {
  return { name, from: { repo: "acme/skills", commit, path: `skills/${name}`, key } };
}

// Catches: a replay of a saved capture that builds its context again, reading its repositories,
// though the cache holds every copy the capture installed.
test("a saved capture whose cache holds every installed copy its record names is read back with no read of any repository", async () => {
  const root = path.join(dir, "copies-present");
  const c = f1Case("c-present");
  writeTree(path.join(root, "old"), { "SKILL.md": OLD_SKILL, "references/x.md": X });
  const arm = buildArm(
    c,
    "old",
    { old: path.join(root, "old"), new: path.join(root, "old") },
    root
  );
  const cacheDir = path.join(root, "cache");
  for (const [name, text] of [
    ["beta", BETA_SKILL],
    ["delta", DELTA_SKILL],
  ] as const) {
    writeTree(path.join(cacheDir, "skills", cacheKey("acme/skills", COMMIT, `skills/${name}`)), {
      "SKILL.md": text,
    });
  }
  const outDir = path.join(root, "out");
  saveOldCapture(outDir, c, [skillsCopy("beta"), skillsCopy("delta")]);
  const reads: string[] = [];
  const read = async (what: string): Promise<never> => {
    reads.push(what);
    throw new Error(`no read is expected: ${what}`);
  };
  const source: RepoSource = {
    name: "no repository",
    file: (repo, _commit, file) => read(`file ${file} of ${repo}`),
    tree: (repo, _commit, at) => read(`tree ${at} of ${repo}`),
    link: (repo, _commit, file) => read(`link ${file} of ${repo}`),
  };
  const saved = await captureBody(c, arm, {
    outDir,
    scratchDir: path.join(root, "scratch"),
    cacheDir,
    source,
    hints,
    session: SESSION,
  });
  expect({ made: saved.made, reads }).toEqual({ made: false, reads: [] });
});

// Catches: a saved capture replayed with other copies than the ones it was made with, where the
// case's profile now names another commit of a copy than its record does, or the record keeps a
// copy under another cache key than the profile gives it; and a refusal that first makes the
// profile's copy in the cache, reading its repository for a capture it then refuses.
test("a saved capture whose record names other installed copies than its case's profile now gives is refused before any copy is made, naming the case, the arm and the copy", async () => {
  const root = path.join(dir, "copies-differ");
  const repos = path.join(root, "repos");
  writeTree(path.join(repos, "acme/skills", COMMIT), {
    "skills/beta/SKILL.md": BETA_SKILL,
    "skills/delta/SKILL.md": DELTA_SKILL,
  });
  writeTree(path.join(repos, "acme/project", COMMIT), {
    ".claude/CLAUDE.md": "# Made-up user rules\n\nKeep each answer short.\n",
  });
  writeTree(path.join(root, "old"), { "SKILL.md": OLD_SKILL, "references/x.md": X });
  const options: CaptureOptions = {
    outDir: path.join(root, "out"),
    scratchDir: path.join(root, "scratch"),
    cacheDir: path.join(root, "cache"),
    source: localSource(repos),
    hints,
    session: SESSION,
  };
  const other = "fedcba9876543210fedcba9876543210fedcba98";
  const betaKey = cacheKey("acme/skills", COMMIT, "skills/beta");
  for (const [id, was, wasWords] of [
    // The case file was built again after the capture: its profile names another commit of beta.
    [
      "c-moved",
      skillsCopy("beta", other),
      `beta from skills/beta of acme/skills at ${other} (cache key ${cacheKey("acme/skills", other, "skills/beta")})`,
    ],
    // The record keeps beta under a key the build does not give it.
    [
      "c-rekeyed",
      skillsCopy("beta", COMMIT, "0".repeat(32)),
      `beta from skills/beta of acme/skills at ${COMMIT} (cache key ${"0".repeat(32)})`,
    ],
  ] as const) {
    const c = f1Case(id);
    const dirs = { old: path.join(root, "old"), new: path.join(root, "old") };
    const arm = buildArm(c, "old", dirs, path.join(root, "arms"));
    const files = saveOldCapture(options.outDir, c, [was, skillsCopy("delta")]);
    const refused = await captureBody(c, arm, options).catch((caught: unknown) => caught);
    expect(refused).toBeInstanceOf(Refused);
    expect((refused as Error).message).toBe(
      `case ${id} arm old: the saved capture ${files.record} installed ${wasWords} where the case's profile now gives beta from skills/beta of acme/skills at ${COMMIT} (cache key ${betaKey}), so a replay of it would answer a read of that skill from another copy than the one it was made with: make it again under a new --out`
    );
  }
  // The refusal comes before any copy is made again: no scratch directory and no cache entry,
  // though the source holds every copy the profile names.
  expect(existsSync(options.scratchDir)).toBe(false);
  expect(existsSync(options.cacheDir)).toBe(false);
});

// Catches: a saved capture made under an older profile of its case replayed in silence because
// the cache still holds every copy its record names (the comparison skipped when no copy is
// missing), and a comparison that reads a repository or makes a copy to decide. Each row's cache
// holds exactly the copies its record names, and the source throws on any read.
test("a saved capture whose record names other installed copies than its case's profile now gives is refused even when the cache holds every copy its record names, and one whose copies match is read back with no read of any repository", async () => {
  const root = path.join(dir, "copies-cached");
  const old = path.join(root, "old");
  writeTree(old, { "SKILL.md": OLD_SKILL, "references/x.md": X });
  const texts: Readonly<Record<string, string>> = { beta: BETA_SKILL, delta: DELTA_SKILL };
  const other = "fedcba9876543210fedcba9876543210fedcba98";
  const gamma = { repo: "acme/skills", commit: COMMIT, path: "skills/gamma", name: "gamma" };
  const betaWords = `beta from skills/beta of acme/skills at ${COMMIT} (cache key ${cacheKey("acme/skills", COMMIT, "skills/beta")})`;
  const rows: readonly {
    readonly id: string;
    /** The repository skills the case's profile gained since the capture. */
    readonly gained: Profile["skills"];
    /** The repository copies the saved record names, each of them in the row's cache. */
    readonly was: readonly ContextRecord["skills"][number][];
    /** The copies the refusal names at the first place they differ: the record's, the profile's. */
    readonly refusal?: { readonly was: string; readonly now: string };
  }[] = [
    // (a) Catches the early return that reused the old profile's copy: the case file was built
    // again after the capture, its profile naming another commit of beta, and the cache still
    // holds the commit the record names.
    {
      id: "c-cached-moved",
      gained: [],
      was: [skillsCopy("beta", other), skillsCopy("delta")],
      refusal: {
        was: `beta from skills/beta of acme/skills at ${other} (cache key ${cacheKey("acme/skills", other, "skills/beta")})`,
        now: betaWords,
      },
    },
    // (b) Catches the early return that reused a listing short of a skill, and a comparison that
    // stops at the shorter list: the case file was built again with one more repository skill,
    // gamma, which the record has no copy of, and the cache holds every copy the record names.
    {
      id: "c-cached-gained",
      gained: [gamma],
      was: [skillsCopy("beta"), skillsCopy("delta")],
      refusal: {
        was: "no copy",
        now: `gamma from skills/gamma of acme/skills at ${COMMIT} (cache key ${cacheKey("acme/skills", COMMIT, "skills/gamma")})`,
      },
    },
    // (c) Catches a comparison that refuses copies that match, or reads a repository or makes a
    // copy where the cache holds them all: the record names the copies the profile gives.
    { id: "c-cached-same", gained: [], was: [skillsCopy("beta"), skillsCopy("delta")] },
  ];
  const reads: string[] = [];
  const read = async (what: string): Promise<never> => {
    reads.push(what);
    throw new Error(`no read is expected: ${what}`);
  };
  const source: RepoSource = {
    name: "no repository",
    file: (repo, _commit, file) => read(`file ${file} of ${repo}`),
    tree: (repo, _commit, at) => read(`tree ${at} of ${repo}`),
    link: (repo, _commit, file) => read(`link ${file} of ${repo}`),
  };
  const outcomes: unknown[] = [];
  const expected: unknown[] = [];
  for (const row of rows) {
    const base = f1Case(row.id);
    const c: Case = {
      ...base,
      profile: { ...base.profile, skills: [...base.profile.skills, ...row.gained] },
    };
    const arm = buildArm(c, "old", { old, new: old }, path.join(root, "arms"));
    const cacheDir = path.join(root, row.id, "cache");
    const keys: string[] = [];
    for (const copy of row.was) {
      if (typeof copy.from === "string") continue;
      writeTree(path.join(cacheDir, "skills", copy.from.key), {
        "SKILL.md": texts[copy.name] ?? "",
      });
      keys.push(copy.from.key);
    }
    const outDir = path.join(root, row.id, "out");
    const files = saveOldCapture(outDir, c, row.was);
    const scratchDir = path.join(root, row.id, "scratch");
    const outcome = await captureBody(c, arm, {
      outDir,
      scratchDir,
      cacheDir,
      source,
      hints,
      session: SESSION,
    }).then(
      (saved) => `read back, made ${saved.made}`,
      (caught: unknown) =>
        caught instanceof Refused ? `refused: ${caught.message}` : `failed: ${String(caught)}`
    );
    outcomes.push({
      id: row.id,
      outcome,
      reads: reads.splice(0),
      cache: readdirSync(path.join(cacheDir, "skills")).sort(),
      scratch: existsSync(scratchDir),
    });
    expected.push({
      id: row.id,
      outcome:
        row.refusal === undefined
          ? "read back, made false"
          : `refused: case ${row.id} arm old: the saved capture ${files.record} installed ${row.refusal.was} where the case's profile now gives ${row.refusal.now}, so a replay of it would answer a read of that skill from another copy than the one it was made with: make it again under a new --out`,
      reads: [],
      cache: keys.sort(),
      scratch: false,
    });
  }
  expect(outcomes).toEqual(expected);
});

// Catches: every left-out plugin said to have failed its build, whatever left it out.
test("each plugin tool left out is named with the reason its plugin was left out", () => {
  // A context record of two plugins left out: only the fields pluginTools reads.
  const context = {
    plugins: [
      {
        name: "pi-a",
        repo: "acme/a",
        commit: "c",
        path: "p",
        as: "left out",
        reason: "its package.json does not parse: Unexpected token",
      },
      {
        name: "pi-b",
        repo: "acme/b",
        commit: "c",
        path: "p",
        as: "left out",
        reason: "it could not be installed: a plugin named pi-b is installed already",
      },
      { name: "pi-c", repo: "acme/c", commit: "c", path: "p", as: "extension" },
    ],
  } as unknown as ContextRecord;
  const tools = pluginTools({ model: "claude-sonnet-5-5", messages: [] }, context, undefined, "");
  expect(tools.leftOut).toEqual([
    "pi-a: its package.json does not parse: Unexpected token",
    "pi-b: it could not be installed: a plugin named pi-b is installed already",
  ]);
});

// Catches: the directory Oh My Pi writes a refused request into (`logs/http-400-requests/`) read
// as a log file, which threw EISDIR and failed the capture after its turn was recorded.
test("extensionErrors reads the log files directly under the profile's logs, passes over a directory there, and keeps each line on an extension that failed to load", () => {
  const profileDir = path.join(dir, "logs-with-directory");
  const failed = '{"level":"error","message":"Failed to load extension","path":"/plugins/pi-a"}';
  writeTree(path.join(profileDir, "logs"), {
    "omp.2026-11-20.log": `{"level":"info","message":"started"}\n${failed}\n`,
    "http-400-requests/req-1.json": '{"status":400,"body":"refused by the stand-in"}\n',
  });
  expect(extensionErrors(profileDir, "pi-b: extension failed to load\nanother line\n")).toEqual([
    "pi-b: extension failed to load",
    failed,
  ]);
});
