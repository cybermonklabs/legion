import { beforeAll, describe, expect, test } from "bun:test";
import { createHash } from "node:crypto";
import { existsSync, mkdirSync, readFileSync, writeFileSync } from "node:fs";
import path from "node:path";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";
import { countOnce, readCount, sessionKey } from "./attribute";
import { GhRetries } from "./common";
import { type Gh, type GhResult, parseTsv } from "./copies";
import { countSets, main, type SkillCount, setRules } from "./set-count";
import { readPins, SetReads } from "./sets";

// set-count.ts on count.sql's rows (testdata/F7-count-first.txt and F7-count-second.txt, whose
// capture commands psql-rows.test.ts's header gives; testdata/F14-count-planted.txt, made-up rows
// in their shape, CAPTURE.md "F14") and on F10's GitHub answers for the pins (CAPTURE.md "F10").
// The configuration is F7's (testdata/F7-skill-cuts.json: the store store_a and the window
// 2026-11-20 to 2026-11-22) with the two sets F10 pins at AT: alpha-set (skills alpha-skill-one,
// alpha-skill-three, alpha-skill-two; the package alpha-set under omp/plugins/node_modules) and
// beta-set (skills beta-skill-one, beta-skill-three, beta-skill-two; the vendor directory
// beta-set; its repository a fork of acme-upstream/beta-set-plugin).
//
// F14 holds, for store_a, the query of 2026-11-22 (after the window's end, so its summary row is
// of another window), reads on 2026-11-21, one session each but where noted:
// (a) alpha-skill-one by the Skill call alpha-set:alpha-skill-one, and in that session also at a
//     path elsewhere; (b) beta-skill-one by beta-set:beta-skill-one; (c) alpha-skill-two under
//     node_modules/alpha-set/; (d) beta-skill-one under vendor/beta-set/, with a reference read
//     there too; (e) beta-skill-two at /opt/acme/plugin/dist/skills/; (f) no-such-skill at
//     skill://no-such-skill; (g) alpha-skill-three in Claude Code's plugin cache under alpha-set/;
//     (h) alpha-skill-two at skill://alpha-skill-two; (k) beta-skill-two at the upstream's raw
//     GitHub URL. beta-skill-three has no read.

const TESTDATA = path.join(import.meta.dir, "testdata");
const fixture = (name: string) => readFileSync(path.join(TESTDATA, name), "utf8");
const F10: Readonly<Record<string, unknown>> = JSON.parse(fixture("F10-github.json"));
const DOTFILES = "acme/dotfiles";
const AT = "82e8f6dcac42f7e9e6f80033755bcea084d18b0c";
const SETS = [
  { id: "alpha-set", repo: "acme/alpha-set" },
  { id: "beta-set", repo: "acme/beta-set-plugin" },
];
const H = {
  a: "f14a00000000000a",
  b: "f14a00000000000b",
  c: "f14a00000000000c",
  d: "f14a00000000000d",
  e: "f14a00000000000e",
  f: "f14a00000000000f",
  g: "f14a000000000010",
  h: "f14a000000000011",
  k: "f14a000000000012",
};
const DAY_MS = 86_400_000;
const NOV_20 = Date.UTC(2026, 10, 20);

/** A stand-in gh that answers each `gh api [-H <header>] <route>` from F10, each route asked kept
 *  in `calls`; a route F10 does not hold is answered as GitHub answers a missing one. */
function f10Gh(calls: string[] = []): Gh {
  return async (args): Promise<GhResult> => {
    const route = args.filter((arg, at) => at > 0 && arg !== "-H" && args[at - 1] !== "-H").at(-1);
    calls.push(route ?? "");
    const answer = route === undefined ? undefined : F10[route];
    if (answer === undefined) return { code: 1, stdout: "", stderr: "gh: Not Found (HTTP 404)\n" };
    const stdout = typeof answer === "string" ? answer : JSON.stringify(answer);
    return { code: 0, stdout, stderr: "" };
  };
}

/** A psql table of count.sql's shape holding `rows` (count.sql's JSON rows), then a summary row of
 *  `store` over [from, to). */
function psqlTable(store: string, from: number, to: number, rows: readonly unknown[] = []): string {
  const summary = { row: "summary", store, mtime_from: from, mtime_to: to, files: 0, parts: 0 };
  const more = { bytes: 0, lines_parsed: 0, bad_lines: 0, shared_call_ids: 0 };
  const lines = [...rows, { ...summary, ...more }].map((row) => ` ${JSON.stringify(row)}`);
  const count = `(${lines.length} ${lines.length === 1 ? "row" : "rows"})`;
  return [" total", "-".repeat(40), ...lines, count, ""].join("\n");
}

const scratch = temporaryDirectory("skill-cuts-set-count");
const at = (...parts: string[]) => path.join(scratch, ...parts);

/** Writes each of `files` (a path under `dir` to its text) and gives `dir`. */
function tree(dir: string, files: Readonly<Record<string, string>>): string {
  for (const [name, text] of Object.entries(files)) {
    mkdirSync(path.dirname(path.join(dir, name)), { recursive: true });
    writeFileSync(path.join(dir, name), text);
  }
  return dir;
}

/** A configuration file `name`: F7's, with the two sets, and `change` over it. */
function configFile(name: string, change: Record<string, unknown> = {}): string {
  const config = {
    ...JSON.parse(fixture("F7-skill-cuts.json")),
    sets: SETS,
    dotfilesRepo: DOTFILES,
  };
  writeFileSync(at(name), JSON.stringify({ ...config, ...change }));
  return at(name);
}

/** A store's roots.txt, the table `count-sql --query roots` prints (README.md, "attribute"),
 *  holding no file: readCount, set-count's reader of the rows as attribute reads them, refuses a
 *  store's output without one, and set-count reads no profile from it. */
const ROOTS = " root\n--------\n(0 rows)\n";

/** The rows of store_a that cover the window, with F14's query after it. */
const PLANTED_ROWS = {
  "store_a/2026-11-20.txt": fixture("F7-count-first.txt"),
  "store_a/2026-11-21.txt": fixture("F7-count-second.txt"),
  "store_a/2026-11-22.txt": fixture("F14-count-planted.txt"),
  "store_a/roots.txt": ROOTS,
};
const SPANS = `${JSON.stringify({ store: "store_a", earliest_mtime_ms: NOV_20 - 28 * DAY_MS, files: 4 })}\n`;

/** set-count's exit status and what it wrote to standard output and standard error. */
async function setCount(
  args: readonly string[],
  gh: Gh = f10Gh()
): Promise<{ code: number; out: string; err: string }> {
  const out: string[] = [];
  const err: string[] = [];
  const code = await main(args, {
    out: (text) => out.push(text),
    err: (line) => err.push(line),
    gh,
    sleep: async () => {},
  });
  return { code, out: out.join(""), err: err.join("\n") };
}

/** set-count's arguments for `config`, `rows` and `out`, with the spans file and the pins at AT. */
const argsOf = (config: string, rows: string, out: string, ...more: string[]) => [
  ...["--config", config, "--rows", rows, "--spans", at("spans.jsonl"), "--out", out],
  ...["--dotfiles-commit", AT, ...more],
];

/** The JSON value of the file `file`. */
const readJson = (file: string): unknown => JSON.parse(readFileSync(file, "utf8"));
const sha256 = (text: string) => createHash("sha256").update(text).digest("hex");

let plantedConfig = "";
let plantedRows = "";
beforeAll(() => {
  writeFileSync(at("spans.jsonl"), SPANS);
  plantedConfig = configFile("planted.json");
  plantedRows = tree(at("planted-rows"), PLANTED_ROWS);
});

// Catches: a second reader of the rows that drops a file, a query or a session attribute.ts
// reads, or one that counts a row's unpaired calls as reads.
test("reads count.sql's rows as attribute reads them", async () => {
  const rows = tree(at("f7-rows"), {
    "store_a/2026-11-20.txt": fixture("F7-count-first.txt"),
    "store_a/2026-11-21.txt": fixture("F7-count-second.txt"),
    "store_a/roots.txt": ROOTS,
  });
  const out = at("f7-out");
  const ran = await setCount(argsOf(plantedConfig, rows, out));
  expect(ran.err).toBe("");
  expect(ran.code).toBe(0);
  // What attribute.ts reads of the same directory: each query, and each session with a read once
  // a read two files hold counts in one of them (countOnce). The forked file 2e8fca5c3950db03, of
  // 2026-11-21, holds the lines of the read of alpha and of the reference read of 83af4b8e0c75a050,
  // of 2026-11-20 (a fork copies its parent's lines), and a read of its own; its hash sorts first,
  // so both copied reads count in the fork, and 83af4b8e0c75a050 is left with no read: 3 sessions
  // with a read, not the 4 files whose rows hold one.
  const read = readCount(rows, ["store_a"]);
  const sessions = new Set(
    countOnce(read.outputs).outputs.flatMap((output) =>
      output.rows
        .filter((row) => row.kind === "read")
        .flatMap((row) => row.hashes.map((hash) => sessionKey(row.store, hash)))
    )
  );
  expect(sessions.size).toBe(3);
  const summary = readJson(path.join(out, "summary.json")) as {
    rows: { files: { file: string; sha256: string }[]; digest: string };
    stores: { store: string; queries: number; sessionsWithRead: number }[];
  };
  const first = sha256(fixture("F7-count-first.txt"));
  const second = sha256(fixture("F7-count-second.txt"));
  expect(summary.rows.files).toEqual([
    { file: "store_a/2026-11-20.txt", sha256: first },
    { file: "store_a/2026-11-21.txt", sha256: second },
  ]);
  expect(summary.rows.digest).toBe(
    sha256(`${first}  store_a/2026-11-20.txt\n${second}  store_a/2026-11-21.txt\n`)
  );
  expect(summary.stores).toMatchObject([
    { store: "store_a", queries: read.outputs.length, sessionsWithRead: sessions.size },
  ]);
});

// Catches: the rows' digest taken after the GitHub reads, which can take minutes, so that it names
// bytes other than those counted.
test("the rows' digest is of the bytes counted, though a file of them changes while GitHub is read", async () => {
  const rows = tree(at("changed-rows"), PLANTED_ROWS);
  const out = at("changed-out");
  const f10 = f10Gh();
  let changed = false;
  const gh: Gh = async (args) => {
    if (!changed) {
      changed = true;
      writeFileSync(path.join(rows, "store_a", "2026-11-20.txt"), "changed after the count\n");
    }
    return f10(args);
  };
  const ran = await setCount(argsOf(plantedConfig, rows, out), gh);
  expect(ran.err).toBe("");
  expect(ran.code).toBe(0);
  const summary = readJson(path.join(out, "summary.json")) as {
    rows: { files: { file: string; sha256: string }[] };
  };
  expect(summary.rows.files[0]).toEqual({
    file: "store_a/2026-11-20.txt",
    sha256: sha256(fixture("F7-count-first.txt")),
  });
});

// Catches: an --out inside --rows taken for one outside it, as a folder whose name starts with
// `..` was.
test("refuses an --out inside --rows, a folder named ..<name> among them, before any read", async () => {
  const calls: string[] = [];
  const rows = tree(at("inside-rows"), PLANTED_ROWS);
  for (const out of [rows, path.join(rows, "out"), path.join(rows, "..cache")]) {
    const ran = await setCount(argsOf(plantedConfig, rows, out), f10Gh(calls));
    expect({ out, code: ran.code }).toEqual({ out, code: 2 });
    expect(ran.err).toContain(`--out ${out} is inside --rows ${rows}, which is only read`);
  }
  expect(calls).toEqual([]);
  expect(existsSync(path.join(rows, "..cache"))).toBe(false);
});

// Catches: a count made on rows that miss a day of the window, or that take a query's file name
// for the day its summary row covers, or a refusal made after GitHub is read.
test("refuses rows where a store lacks a day of the window", async () => {
  const calls: string[] = [];
  const missing = tree(at("gap-rows"), {
    "store_a/2026-11-20.txt": fixture("F7-count-first.txt"),
    "store_a/2026-11-22.txt": fixture("F14-count-planted.txt"),
    "store_a/roots.txt": ROOTS,
  });
  const out = at("gap-out");
  const ran = await setCount(argsOf(plantedConfig, missing, out), f10Gh(calls));
  expect(ran.code).toBe(2);
  expect(ran.err).toContain(
    "store_a 2026-11-21: no summary row (no output of that day or of its hours)"
  );
  expect(calls).toEqual([]);
  expect(existsSync(path.join(out, "exemplar-count.md"))).toBe(false);

  const nov21 = NOV_20 + DAY_MS;
  const hours = tree(at("hour-rows"), {
    "store_a/2026-11-20.txt": fixture("F7-count-first.txt"),
    "store_a/2026-11-21T05.txt": psqlTable("store_a", nov21 + 5 * 3_600_000, nov21 + 6 * 3_600_000),
    "store_a/roots.txt": ROOTS,
  });
  const split = await setCount(argsOf(plantedConfig, hours, at("hour-out")));
  expect(split.code).toBe(2);
  expect(split.err).toContain("store_a 2026-11-21: summary rows of 1 of its 24 hours");

  const misnamed = tree(at("misnamed-rows"), {
    "store_a/2026-11-20.txt": fixture("F7-count-first.txt"),
    "store_a/2026-11-21.txt": fixture("F7-count-first.txt"),
    "store_a/roots.txt": ROOTS,
  });
  const wrong = await setCount(argsOf(plantedConfig, misnamed, at("misnamed-out")));
  expect(wrong.code).toBe(2);
  expect(wrong.err).toContain(
    "store_a 2026-11-21: its summary row covers [2026-11-20T00:00:00Z, 2026-11-21T00:00:00Z), not that query's"
  );
});

// Catches: a store's earliest record taken from anything but the spans file, or a store the spans
// file has no line for, or a null one, written as a time.
test("gives each store's earliest record from the spans file", async () => {
  const config = configFile("two-stores.json", {
    stores: [
      { name: "store_a", context: "devbox" },
      { name: "store_b", context: "legion:ACME" },
      { name: "store_c", context: "legion:ACME" },
    ],
  });
  const empty = (store: string) => ({
    [`${store}/2026-11-20.txt`]: psqlTable(store, NOV_20, NOV_20 + DAY_MS),
    [`${store}/2026-11-21.txt`]: psqlTable(store, NOV_20 + DAY_MS, NOV_20 + 2 * DAY_MS),
    [`${store}/roots.txt`]: ROOTS,
  });
  const rows = tree(at("spans-rows"), {
    ...PLANTED_ROWS,
    ...empty("store_b"),
    ...empty("store_c"),
  });
  const spans = tree(at("spans-file"), {
    "spans.jsonl": `${SPANS}${JSON.stringify({ store: "store_b", earliest_mtime_ms: null, files: 0 })}\n`,
  });
  const out = at("spans-out");
  const args = argsOf(config, rows, out).map((arg) =>
    arg === at("spans.jsonl") ? path.join(spans, "spans.jsonl") : arg
  );
  const ran = await setCount(args);
  expect(ran.err).toBe("");
  expect(ran.code).toBe(0);
  const summary = readJson(path.join(out, "summary.json")) as { stores: unknown[] };
  expect(summary.stores).toMatchObject([
    {
      store: "store_a",
      context: "devbox",
      earliestRecord: "2026-10-23T00:00:00Z",
      filesInStore: 4,
    },
    { store: "store_b", context: "legion:ACME", earliestRecord: "no record", filesInStore: 0 },
    {
      store: "store_c",
      context: "legion:ACME",
      earliestRecord: "not in the spans file",
      filesInStore: null,
    },
  ]);
  const markdown = readFileSync(path.join(out, "exemplar-count.md"), "utf8");
  expect(markdown).toContain("| store_a | devbox | 2026-10-23T00:00:00Z | 4 | 3 |");
  expect(markdown).toContain("| store_b | legion:ACME | no record | 0 | 2 |");
  expect(markdown).toContain(
    "| store_c | legion:ACME | not in the spans file | not measured | 2 |"
  );
});

// Catches: a store with no rows left out of the count without a word, or the transcripts the rows
// cannot hold left unnamed.
test("names a configured store with no rows, and the Claude Code transcripts, as not read", async () => {
  const config = configFile("unread-store.json", {
    stores: [
      { name: "store_a", context: "devbox" },
      { name: "store_b", context: "legion:ACME" },
    ],
  });
  const out = at("unread-out");
  const why = "the transcript store holds no Claude Code session until they are moved into it";
  const ran = await setCount(
    argsOf(config, plantedRows, out, "--not-read", `Claude Code transcripts=${why}`)
  );
  expect(ran.err).toBe("");
  expect(ran.code).toBe(0);
  const summary = readJson(path.join(out, "summary.json")) as { notRead: unknown[] };
  expect(summary.notRead).toEqual([
    { name: "store_b", reason: "the rows hold no output of this store" },
    { name: "Claude Code transcripts", reason: why },
  ]);
  const markdown = readFileSync(path.join(out, "exemplar-count.md"), "utf8");
  expect(markdown).toContain("- store_b: the rows hold no output of this store");
  expect(markdown).toContain(`- Claude Code transcripts: ${why}`);
  const bad = await setCount(
    argsOf(config, plantedRows, at("unread-bad"), "--not-read", "no reason")
  );
  expect(bad.code).toBe(2);
  expect(bad.err).toContain('--not-read "no reason" is not <name>=<reason>');
});

// Catches: a count whose rows cannot be made again (no count-sql line for a query the rows hold,
// an hour's query given as its day, or no line for a store's roots.txt, which readCount refuses
// the store's rows without) or whose own command does not give the same pins again.
test("prints the count-sql command of each store and day and of each store's roots.txt, and its own", async () => {
  const config = configFile("commands.json", {
    stores: [
      { name: "store_a", context: "devbox" },
      { name: "store_b", context: "legion:ACME" },
    ],
  });
  const nov21 = NOV_20 + DAY_MS;
  const rows = tree(at("commands-rows"), {
    "store_a/2026-11-20.txt": fixture("F7-count-first.txt"),
    "store_a/2026-11-22.txt": fixture("F14-count-planted.txt"),
    "store_a/roots.txt": ROOTS,
    ...Object.fromEntries(
      Array.from({ length: 24 }, (_, hour) => [
        `store_a/2026-11-21T${String(hour).padStart(2, "0")}.txt`,
        psqlTable("store_a", nov21 + hour * 3_600_000, nov21 + (hour + 1) * 3_600_000),
      ])
    ),
    "store_b/2026-11-20.txt": psqlTable("store_b", NOV_20, nov21),
    "store_b/2026-11-21.txt": psqlTable("store_b", nov21, nov21 + DAY_MS),
    "store_b/roots.txt": ROOTS,
  });
  const out = at("commands-out");
  const label = "the count's raw rows, version 6";
  const ran = await setCount(argsOf(config, rows, out, "--rows-label", label));
  expect(ran.err).toBe("");
  expect(ran.code).toBe(0);
  const markdown = readFileSync(path.join(out, "exemplar-count.md"), "utf8");
  const prefix = `bun scripts/skill-cuts/cli.ts count-sql --config ${config} --store`;
  const countSql = (store: string, query: string, file: string) =>
    `${prefix} ${store} ${query} | /usr/bin/psql ${store} > ${store}/${file}.txt`;
  expect(markdown).toContain(countSql("store_a", "--day 2026-11-20", "2026-11-20"));
  expect(markdown).toContain(countSql("store_a", "--day 2026-11-21 --hour 0", "2026-11-21T00"));
  expect(markdown).toContain(countSql("store_a", "--day 2026-11-21 --hour 23", "2026-11-21T23"));
  const lines = markdown.split("\n").filter((line) => line.startsWith(prefix));
  // store_a's 26 queries, then its roots query; then store_b's two queries and its roots query.
  expect(lines).toHaveLength(30);
  expect(lines.slice(25)).toEqual([
    countSql("store_a", "--day 2026-11-22", "2026-11-22"),
    countSql("store_a", "--query roots", "roots"),
    countSql("store_b", "--day 2026-11-20", "2026-11-20"),
    countSql("store_b", "--day 2026-11-21", "2026-11-21"),
    countSql("store_b", "--query roots", "roots"),
  ]);
  const own = [
    "bun scripts/skill-cuts/cli.ts set-count",
    `--config ${config} --rows ${rows} --spans ${at("spans.jsonl")} --out ${out}`,
    `--dotfiles-commit ${AT} --rows-label 'the count'\\''s raw rows, version 6'`,
  ].join(" ");
  expect(markdown).toContain(own);
  expect(markdown).toContain(label);
  const summary = readJson(path.join(out, "summary.json")) as {
    commands: { countSql: string[]; setCount: string };
  };
  expect(summary.commands.setCount).toBe(own);
  expect(summary.commands.countSql).toEqual(lines);

  // Without --dotfiles-commit, the pins are the default branch's commit's (here AT), and its own
  // command names that commit, so it reads the same pins again.
  const f10 = f10Gh();
  const head: Gh = async (args) => {
    const route = args.at(-1);
    if (route === `repos/${DOTFILES}`) {
      return { code: 0, stdout: JSON.stringify({ default_branch: "main" }), stderr: "" };
    }
    if (route === `repos/${DOTFILES}/git/ref/heads/main`) {
      return {
        code: 0,
        stdout: JSON.stringify({ object: { sha: AT, type: "commit" } }),
        stderr: "",
      };
    }
    return f10(args);
  };
  const headOut = at("commands-head-out");
  const given = ["--config", config, "--rows", rows, "--spans", at("spans.jsonl")];
  const fromHead = await setCount([...given, "--out", headOut], head);
  expect(fromHead.err).toBe("");
  expect(fromHead.code).toBe(0);
  const headMarkdown = readFileSync(path.join(headOut, "exemplar-count.md"), "utf8");
  expect(headMarkdown).toContain(
    `bun scripts/skill-cuts/cli.ts set-count ${given.join(" ")} --out ${headOut} --dotfiles-commit ${AT}`
  );
  expect(headMarkdown).toContain(
    `The pins of ${DOTFILES} at ${AT} (the default branch main of ${DOTFILES} when the count ran).`
  );
});

describe("planted rows", () => {
  let counts: readonly SkillCount[] = [];
  beforeAll(async () => {
    const reads = new SetReads(
      f10Gh(),
      new GhRetries({ sleep: async () => {}, err: () => {} }, "set-count")
    );
    const pins = await readPins(reads, SETS, DOTFILES, AT);
    counts = countSets(setRules(pins.sets), readCount(plantedRows, ["store_a"]).outputs);
  });
  const EXCLUSIVE = ["own", "upstream_url", "other_copy", "no_copy"] as const;

  // Catches: a prefix, a node_modules or vendor path, or Claude Code's plugin cache not counted as
  // the set's own copy; a path elsewhere (a fork's dist/skills) counted as its own; a read with no
  // path given a copy; the upstream's URL counted as its own; a session counted in two columns.
  test.each([
    ["(a) a Skill call's alpha-set: prefix", H.a, "alpha-set", "alpha-skill-one", "own"],
    ["(b) a Skill call's beta-set: prefix", H.b, "beta-set", "beta-skill-one", "own"],
    ["(c) a path under node_modules/alpha-set/", H.c, "alpha-set", "alpha-skill-two", "own"],
    ["(d) a path under vendor/beta-set/", H.d, "beta-set", "beta-skill-one", "own"],
    ["(e) a path elsewhere", H.e, "beta-set", "beta-skill-two", "other_copy"],
    [
      "(g) Claude Code's plugin cache under alpha-set/",
      H.g,
      "alpha-set",
      "alpha-skill-three",
      "own",
    ],
    ["(h) skill://<name> with no path", H.h, "alpha-set", "alpha-skill-two", "no_copy"],
    ["(k) the upstream's raw GitHub URL", H.k, "beta-set", "beta-skill-two", "upstream_url"],
    ["a reference read under vendor/beta-set/", H.d, "beta-set", "beta-skill-one", "reference_own"],
  ] as const)("%s", (_name, hash, set, skill, column) => {
    const count = counts.find((each) => each.set === set && each.skill === skill);
    const key = sessionKey("store_a", hash);
    expect(count?.sessions[column].has(key)).toBe(true);
    if (column !== "reference_own") {
      for (const other of EXCLUSIVE.filter((each) => each !== column)) {
        expect(count?.sessions[other].has(key)).toBe(false);
      }
    }
  });

  // Catches: a skill of no set counted for a set.
  test("(f) a skill no set holds is counted for no set", () => {
    const key = sessionKey("store_a", H.f);
    for (const count of counts) {
      for (const sessions of Object.values(count.sessions)) expect(sessions.has(key)).toBe(false);
    }
  });

  // Catches: a session that reads its own copy and a path elsewhere counted twice, or under the
  // path elsewhere; beta-skill-one's two sessions counted once.
  test("a session counts once for a skill, by its own copy first", () => {
    const one = counts.find((count) => count.skill === "alpha-skill-one");
    expect([...(one?.sessions.own ?? [])]).toEqual([sessionKey("store_a", H.a)]);
    expect(one?.sessions.other_copy.size).toBe(0);
    const beta = counts.find((count) => count.skill === "beta-skill-one");
    expect(beta?.sessions.own.size).toBe(2);
  });
});

// Catches: a skill of a set with no read left out of the table, or a table that lists a skill
// the set does not hold at its pin.
test("lists every skill of each set at the pin, one with no read at 0", async () => {
  const out = at("inventory-out");
  const ran = await setCount(argsOf(plantedConfig, plantedRows, out));
  expect(ran.err).toBe("");
  expect(ran.code).toBe(0);
  const table = parseTsv(readFileSync(path.join(out, "exemplar-count.tsv"), "utf8"));
  const columns = [
    "own_sessions",
    "upstream_url_sessions",
    "other_copy_sessions",
    "no_copy_sessions",
    "reference_own_sessions",
  ];
  expect(table.map((row) => [row.set, row.skill, ...columns.map((column) => row[column])])).toEqual(
    [
      ["alpha-set", "alpha-skill-one", "1", "0", "0", "0", "0"],
      ["alpha-set", "alpha-skill-three", "1", "0", "0", "0", "0"],
      ["alpha-set", "alpha-skill-two", "1", "0", "0", "1", "0"],
      ["beta-set", "beta-skill-one", "2", "0", "0", "0", "1"],
      ["beta-set", "beta-skill-three", "0", "0", "0", "0", "0"],
      ["beta-set", "beta-skill-two", "0", "1", "1", "0", "0"],
    ]
  );
  // Each column also per store.
  expect(table[3]?.["own_sessions:store_a"]).toBe("2");
  expect(table[5]?.["upstream_url_sessions:store_a"]).toBe("1");
  const markdown = readFileSync(path.join(out, "exemplar-count.md"), "utf8");
  expect(markdown).toContain("| beta-set | beta-skill-three | 0 | 0 | 0 | 0 | 0 |");
  expect(readFileSync(path.join(out, "sessions", "beta-set", "beta-skill-one.jsonl"), "utf8")).toBe(
    `${JSON.stringify({ store: "store_a", hash: H.b })}\n${JSON.stringify({ store: "store_a", hash: H.d })}\n`
  );
  expect(
    readFileSync(path.join(out, "sessions", "beta-set", "beta-skill-three.jsonl"), "utf8")
  ).toBe("");
  const summary = readJson(path.join(out, "summary.json")) as {
    pins: { set: string; commit: string; skills: number }[];
    skills: { set: string; skill: string }[];
    atLeast20: unknown[];
  };
  expect(summary.pins.map((pin) => [pin.set, pin.commit, pin.skills])).toEqual([
    ["alpha-set", "2ce8aa15d05bd57213e6f746f953aafb7ded7a8b", 3],
    ["beta-set", "8a776c13a08eea0d6485c26c9bc96ea84d5a6cc6", 3],
  ]);
  expect(summary.skills).toHaveLength(6);
  expect(summary.atLeast20).toEqual([]);
});

// Catches: the list of skills at 20 or more sessions cut at another number, or made from another
// column than the own copy's.
test("lists each skill with at least 20 own-copy sessions", async () => {
  const hashes = (n: number, from: number) =>
    Array.from({ length: n }, (_, i) => (from + i).toString(16).padStart(16, "0"));
  const row = (skill: string, copy: string, list: string[]) => ({
    row: "count",
    store: "store_a",
    day: "2026-11-21",
    skill,
    copy,
    kind: "read",
    files: list.length,
    hashes: list,
    // Each file's one read, with a call key of its own: its hash with `c` for its first digit.
    calls: Object.fromEntries(list.map((hash) => [hash, [`c${hash.slice(1)}`]])),
    unpaired: 0,
    unpaired_hashes: [],
    unpaired_calls: {},
  });
  const nov23 = NOV_20 + 3 * DAY_MS;
  // With F14's one session each: alpha-skill-one 21 own-copy sessions, alpha-skill-two 19, and
  // beta-skill-two 25 that read only another copy.
  const busy = psqlTable("store_a", nov23, nov23 + DAY_MS, [
    row("alpha-skill-one", "alpha-set:alpha-skill-one", hashes(20, 0x100)),
    row("alpha-skill-two", "alpha-set:alpha-skill-two", hashes(18, 0x200)),
    row(
      "beta-skill-two",
      "/opt/acme/plugin/dist/skills/beta-skill-two/SKILL.md",
      hashes(25, 0x300)
    ),
  ]);
  const rows = tree(at("busy-rows"), { ...PLANTED_ROWS, "store_a/2026-11-23.txt": busy });
  const out = at("busy-out");
  const ran = await setCount(argsOf(plantedConfig, rows, out));
  expect(ran.err).toBe("");
  expect(ran.code).toBe(0);
  const written = readJson(path.join(out, "summary.json")) as { atLeast20: unknown[] };
  expect(written.atLeast20).toEqual([{ set: "alpha-set", skill: "alpha-skill-one", own: 21 }]);
  const markdown = readFileSync(path.join(out, "exemplar-count.md"), "utf8");
  expect(markdown).toContain("- alpha-set alpha-skill-one: 21 sessions");
});

// Catches: a read that a fork and its parent both hold, the two written on different days and so
// in two queries, counted as two sessions, or counted in the file of the earlier query rather than
// in the file whose hash sorts first (count.sql's rule, as attribute.ts's countOnce applies it).
test("a read two files of two queries hold counts in one session, the file whose hash sorts first", async () => {
  // A parent file of 2026-11-23's query reads alpha-skill-two by its Skill call; its fork, written
  // on 2026-11-24, copies that read's line, so both files hold its call key. The fork's hash sorts
  // first.
  const [parent, fork, key] = ["f14e0000000000b0", "f14e0000000000a0", "f14f000000000001"];
  const query = (day: number, file: string) => {
    const row = {
      ...{ row: "count", store: "store_a", day: "2026-11-21", skill: "alpha-skill-two" },
      ...{ copy: "alpha-set:alpha-skill-two", kind: "read", files: 1, hashes: [file] },
      ...{ calls: { [file]: [key] }, unpaired: 0, unpaired_hashes: [], unpaired_calls: {} },
    };
    const from = NOV_20 + day * DAY_MS;
    return psqlTable("store_a", from, from + DAY_MS, [row]);
  };
  const rows = tree(at("fork-rows"), {
    ...PLANTED_ROWS,
    "store_a/2026-11-23.txt": query(3, parent),
    "store_a/2026-11-24.txt": query(4, fork),
  });
  const out = at("fork-out");
  const ran = await setCount(argsOf(plantedConfig, rows, out));
  expect(ran.err).toBe("");
  expect(ran.code).toBe(0);
  // With F14's (c), two own-copy sessions of alpha-skill-two, not three: the fork's, not the
  // parent's.
  expect(
    readFileSync(path.join(out, "sessions", "alpha-set", "alpha-skill-two.jsonl"), "utf8")
  ).toBe(
    `${JSON.stringify({ store: "store_a", hash: H.c })}\n${JSON.stringify({ store: "store_a", hash: fork })}\n`
  );
  const table = parseTsv(readFileSync(path.join(out, "exemplar-count.tsv"), "utf8"));
  expect(table.find((row) => row.skill === "alpha-skill-two")?.own_sessions).toBe("2");
});
