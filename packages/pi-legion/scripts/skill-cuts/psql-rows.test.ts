import { expect, test } from "bun:test";
import { createHash } from "node:crypto";
import { readFileSync } from "node:fs";
import path from "node:path";
import { z } from "zod";
import { PULL_START, PullCheck, PullRow, psqlRows } from "./psql-rows";

// psql-rows.ts on F7: psql's own output under the store command's flags, captured from two test
// databases loaded as count.test.ts's fixture proof loads its first two. Capture, from
// packages/pi-legion/scripts/skill-cuts, in a shell where LEGION_TEST_PG_DSN is the URL of the
// pod's test server (its path a database name, no query string):
//
//   N=$(date +%s); SERVER=${LEGION_TEST_PG_DSN%/*}
//   createdb --maintenance-db="$LEGION_TEST_PG_DSN" "skillcuts_first_$N"
//   createdb --maintenance-db="$LEGION_TEST_PG_DSN" "skillcuts_second_$N"
//   FIRST=$SERVER/skillcuts_first_$N; SECOND=$SERVER/skillcuts_second_$N
//   F1=testdata/F1/2026-11-20T01-59-41-780Z_01a1f6ae-51d4-715f-a2a6-59f275bdff80
//   bun fixturedb.ts --dsn "$FIRST" $F1.jsonl $F1/ProductiveChicken.jsonl \
//     testdata/F4/projects/-legion-scratch-tmp-acme-1-t1-runs-f3-work/6441e7f4-71f3-49f1-a76f-0e0ffc82ea11.jsonl
//   bun fixturedb.ts --dsn "$SECOND" testdata/F1b.jsonl
//   store() { psql -X -q -w -v ON_ERROR_STOP=1 --single-transaction \
//     -c 'SET TRANSACTION READ ONLY' -f - "$1"; }
//   bun countsql.ts --config testdata/F7-skill-cuts.json --store store_a --day 2026-11-20 \
//     | store "$FIRST" > testdata/F7-count-first.txt
//   bun countsql.ts --config testdata/F7-skill-cuts.json --store store_a --day 2026-11-21 \
//     | store "$SECOND" > testdata/F7-count-second.txt
//   bun -e 'import { pullSql } from "./countsql"; import { PULL_START } from "./psql-rows";
//     process.stdout.write(pullSql(
//     "F1/2026-11-20T01-59-41-780Z_01a1f6ae-51d4-715f-a2a6-59f275bdff80.jsonl", PULL_START))' \
//     | store "$FIRST" > testdata/F7-pull.txt
//   bun -e 'import { pullSql } from "./countsql"; import { PULL_START } from "./psql-rows";
//     process.stdout.write(pullSql("no/such/path.jsonl", PULL_START))' \
//     | store "$FIRST" > testdata/F7-empty.txt
//   bun -e 'import { tasksSql, mtimeRange } from "./countsql"; process.stdout.write(tasksSql(
//     mtimeRange("2026-11-20"), "2026-11-20T00:00:00Z", "2026-11-22T00:00:00Z", ["Fixture"]))' \
//     | store "$FIRST" > testdata/F7-tasks.txt
//   bun -e 'import { sessionsSql } from "./countsql"; process.stdout.write(sessionsSql([
//     "83af4b8e0c75a050", "0da7427e81182998", "1e93689d7698bd6a", "0000000000000000"]))' \
//     | store "$FIRST" > testdata/F7-sessions.txt
//   echo "SELECT json_build_object('o', -1, 'c', E'first line | one\\nsecond line | two')::text AS part;" \
//     | store "$FIRST" > testdata/F7-newline-pipe.txt
//
// count.test.ts's capture of the models query runs next, in the same shell; then drop both
// databases (`dropdb --maintenance-db="$LEGION_TEST_PG_DSN" "skillcuts_first_$N"`, and _second_).
//
// fixturedb.ts splits each file into two parts in the middle of a line, so F7-pull.txt holds F1's
// session as the file row (offset -1, empty) and two parts, each piece one slice (`k` 0).

const TESTDATA = path.join(import.meta.dir, "testdata");
const F1_SESSION = "F1/2026-11-20T01-59-41-780Z_01a1f6ae-51d4-715f-a2a6-59f275bdff80.jsonl";
const F1_SUBAGENT =
  "F1/2026-11-20T01-59-41-780Z_01a1f6ae-51d4-715f-a2a6-59f275bdff80/ProductiveChicken.jsonl";
const F4_SESSION =
  "F4/projects/-legion-scratch-tmp-acme-1-t1-runs-f3-work/6441e7f4-71f3-49f1-a76f-0e0ffc82ea11.jsonl";
const F1_PROFILE = "/legion/scratch/tmp/acme-1/t1-runs/f1/home/.omp/profiles/fixture/agent/skills";

function f7(name: string): string {
  return readFileSync(path.join(TESTDATA, name), "utf8");
}

/** A file's hash as count.sql names it: md5 of its path, cut to 16 hex characters. */
function fileHash(filePath: string): string {
  return createHash("md5").update(filePath).digest("hex").slice(0, 16);
}

/** The fields of a count row this file checks; the rest (store, unpaired_hashes) are dropped. */
const CountRow = z.object({
  row: z.literal("count"),
  skill: z.string(),
  kind: z.string(),
  copy: z.string(),
  day: z.string(),
  files: z.number(),
  hashes: z.array(z.string()),
  unpaired: z.number(),
});

/** The count rows of a count result, without the summary and without their `row` field. */
function counts(rows: unknown[]) {
  return rows.flatMap((row) => {
    const parsed = CountRow.safeParse(row);
    if (!parsed.success) return [];
    const { row: _row, ...count } = parsed.data;
    return [count];
  });
}

test("the first database's count: alpha read and referenced once, the missing skill not at all, beta read in the subagent's file, F4's Skill call counted", () => {
  const rows = psqlRows(f7("F7-count-first.txt"));
  expect(counts(rows)).toEqual([
    {
      skill: "alpha",
      kind: "read",
      copy: `${F1_PROFILE}/alpha/SKILL.md`,
      day: "2026-11-20",
      files: 1,
      hashes: [fileHash(F1_SESSION)],
      unpaired: 0,
    },
    {
      skill: "alpha",
      kind: "reference",
      copy: `${F1_PROFILE}/alpha/references/x.md`,
      day: "2026-11-20",
      files: 1,
      hashes: [fileHash(F1_SESSION)],
      unpaired: 0,
    },
    {
      skill: "alpha",
      kind: "read",
      copy: "/legion/scratch/tmp/acme-1/t1-runs/f3/marketplace/plugins/acme-tools/skills/alpha/SKILL.md",
      day: "2026-11-20",
      files: 1,
      hashes: [fileHash(F4_SESSION)],
      unpaired: 0,
    },
    {
      skill: "beta",
      kind: "read",
      copy: `${F1_PROFILE}/beta/SKILL.md`,
      day: "2026-11-20",
      files: 1,
      hashes: [fileHash(F1_SUBAGENT)],
      unpaired: 0,
    },
    {
      skill: "beta",
      kind: "read",
      copy: "/legion/scratch/tmp/acme-1/t1-runs/f3/config/plugins/cache/acme-market/acme-tools/0.1.0/skills/beta/SKILL.md",
      day: "2026-11-20",
      files: 1,
      hashes: [fileHash(F4_SESSION)],
      unpaired: 0,
    },
  ]);
  expect(rows.at(-1)).toEqual({
    row: "summary",
    store: "store_a",
    mtime_from: Date.parse("2026-11-20T00:00:00Z"),
    mtime_to: Date.parse("2026-11-21T00:00:00Z"),
    files: 3,
    parts: 6,
    bytes: 185790,
    lines_parsed: 15,
    bad_lines: 0,
    shared_call_ids: 0,
  });
});

test("the second database's count: F1b's alpha, read on two days, is one session on its first day", () => {
  const rows = psqlRows(f7("F7-count-second.txt"));
  const alpha = counts(rows).filter((row) => row.skill === "alpha" && row.kind === "read");
  expect(alpha).toEqual([
    {
      skill: "alpha",
      kind: "read",
      copy: `${F1_PROFILE}/alpha/SKILL.md`,
      day: "2026-11-20",
      files: 1,
      hashes: [fileHash("F1b.jsonl")],
      unpaired: 0,
    },
  ]);
  expect(rows.at(-1)).toMatchObject({
    row: "summary",
    files: 1,
    mtime_from: Date.parse("2026-11-21T00:00:00Z"),
  });
});

/** The text `rows` give, each taken in turn by one PullCheck, as pullTranscript takes them. */
function pulled(rows: readonly unknown[]): string {
  const check = new PullCheck();
  return Buffer.concat(rows.map((row) => check.take(row))).toString("utf8");
}

test("a transcript pull's rows join into the file's text, the file row first", () => {
  const rows = psqlRows(f7("F7-pull.txt"));
  expect(rows.map((row) => [PullRow.parse(row).o, PullRow.parse(row).k])).toEqual([
    [-1, 0],
    [0, 0],
    [8461, 0],
  ]);
  expect(pulled(rows)).toBe(readFileSync(path.join(TESTDATA, F1_SESSION), "utf8"));
});

test("an imported session appended to: the file row's slices first, then a part at the file row's byte length", () => {
  // The store's import keeps a whole file as the file row's content, its byte_len the content's
  // bytes, and an append writes a part at the row's byte_len (ompsessions.go's Import and
  // Append), so the file's first part starts after the file row's bytes, not at 0.
  const head = ['{"type":"session","cwd":"/work/é"}\n', '{"type":"title","title":"x"}\n'];
  const after = Buffer.byteLength(head.join(""));
  const parts = ['{"type":"message","n":1}\n', '{"type":"message","n":2}\n'];
  const rows = [
    { o: -1, k: 0, c: head[0] },
    { o: -1, k: 1, c: head[1] },
    { o: after, k: 0, c: parts[0] },
    { o: after + Buffer.byteLength(parts[0] ?? ""), k: 0, c: parts[1] },
  ];
  const check = new PullCheck();
  expect(check.last).toEqual(PULL_START);
  expect(Buffer.concat(rows.map((row) => check.take(row))).toString("utf8")).toBe(
    [...head, ...parts].join("")
  );
  expect(check.bytes).toBe(Buffer.byteLength([...head, ...parts].join("")));
  expect(check.last).toEqual({ o: rows[3]?.o, k: 0 });
  // A part at 0 after a file row that holds text overlaps it.
  expect(() =>
    pulled([
      { o: -1, k: 0, c: "ab" },
      { o: 0, k: 0, c: "c" },
    ])
  ).toThrow("the part at offset 0 does not start where the text before it ends (2)");
});

test("the task query's rows are each a path and a first user message", () => {
  const rows = z
    .array(z.strictObject({ path: z.string(), text: z.string() }))
    .parse(psqlRows(f7("F7-tasks.txt")));
  expect(rows.map((row) => row.path)).toEqual([F1_SESSION, F4_SESSION]);
  expect(rows[0]?.text).toStartWith("This is a test of skill reads in a made-up fixture project.");
  expect(rows[1]?.text).toStartWith(
    "This is a test of skill loading in a made-up fixture project."
  );
});

test("the sessions query names each hashed file the store holds", () => {
  expect(psqlRows(f7("F7-sessions.txt"))).toEqual(
    [F1_SESSION, F1_SUBAGENT, F4_SESSION].map((filePath) => ({
      path: filePath,
      hash: fileHash(filePath),
    }))
  );
});

test("an empty result is no row", () => {
  expect(psqlRows(f7("F7-empty.txt"))).toEqual([]);
});

test("a value that holds a newline and a | stays one row", () => {
  expect(psqlRows(f7("F7-newline-pipe.txt"))).toEqual([
    { o: -1, c: "first line | one\nsecond line | two" },
  ]);
});

test("a DEL that psql prints as \\x7F is read back, and a text's own \\x7F stays as it is", () => {
  // JSON leaves DEL (U+007F) and U+0080 to U+009F as they are; psql prints DEL as the four
  // characters \x7F and U+0085 as \u0085. The value holds a DEL, the text \x7F, a backslash
  // before a DEL, and U+0085.
  const output = [
    " part ",
    "------",
    String.raw` {"o" : 0, "k" : 0, "c" : "a\x7Fb \\x7F c\\\x7F d\u0085"}`,
    "(1 row)",
    "",
  ].join("\n");
  expect(psqlRows(output)).toEqual([{ o: 0, k: 0, c: "a\u007fb \\x7F c\\\u007f d\u0085" }]);
});

test("a row line that is not one JSON value is refused, naming its line", () => {
  const lines = f7("F7-count-second.txt").split("\n");
  lines[3] = `${lines[3]?.slice(0, 40)}`;
  expect(() => psqlRows(lines.join("\n"))).toThrow("psql output line 4: not one JSON value");
});

test.each([
  [
    "a footer that gives another count",
    (text: string) => text.replace("(3 rows)", "(4 rows)"),
    "line 6: the footer gives 4 rows, the table holds 3",
  ],
  ["no footer", (text: string) => text.replace("(3 rows)", ""), "no footer (N rows)"],
  ["no header rule", (text: string) => text.replace(/^-+$/m, ""), "no header rule"],
  ["empty output", () => "", "no header rule"],
  [
    "text after the footer",
    (text: string) => `${text}SET\n`,
    "line 8: text after the table's footer",
  ],
  [
    "a second column",
    (text: string) => text.replace(/^(-+)$/m, "$1+-----"),
    "line 2: the table has more than one column",
  ],
])("%s is refused", (_name, change, fault) => {
  expect(() => psqlRows(change(f7("F7-count-second.txt")))).toThrow(fault);
});

test("a pull with a part left out, a row twice or out of order, a slice left out, or a row of another shape is refused", () => {
  const rows = psqlRows(f7("F7-pull.txt"));
  expect(() => pulled([rows[0], rows[2]])).toThrow(
    "the part at offset 8461 does not start where the text before it ends (0)"
  );
  expect(() => pulled([...rows, rows[2]])).toThrow(
    "pull rows give the piece at offset 8461, slice 0, twice"
  );
  expect(() => pulled([...rows, rows[1]])).toThrow(
    "the piece at offset 0, slice 0, comes after the one at offset 8461, slice 0"
  );
  expect(() => pulled([...rows].reverse())).toThrow(
    "the part at offset 8461 does not start where the text before it ends (0)"
  );
  expect(() =>
    pulled([
      { o: -1, k: 0, c: "a" },
      { o: -1, k: 2, c: "b" },
    ])
  ).toThrow("the piece at offset -1, slice 2, does not follow slice 0");
  expect(() => pulled([{ o: -1, k: 1, c: "a" }])).toThrow(
    "the piece at offset -1, slice 1, is not the piece's first"
  );
  expect(() => pulled([{ o: 0, c: "x" }])).toThrow(
    'pull row 1 is not {"o": <offset>, "k": <slice>, "c": <text>}'
  );
});
