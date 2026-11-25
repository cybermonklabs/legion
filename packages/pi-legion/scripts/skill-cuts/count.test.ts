import { afterAll, beforeAll, describe, expect, test } from "bun:test";
import { createHash } from "node:crypto";
import { mkdtempSync, readdirSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import path from "node:path";
import { SQL } from "bun";
import { z } from "zod";
import { attribute, parseQueryOutput, type QueryOutput, type R4Tables } from "./attribute";
import { pullTranscript } from "./build";
import { pathHash } from "./cases";
import { loadConfig } from "./config";
import {
  countSql,
  digestTimesSql,
  fillTemplate,
  hashesSql,
  main,
  modelsSql,
  mtimeRange,
  PULL_PAGE,
  pullSql,
  rootsSql,
  sessionHeadSql,
  sessionPathsSql,
  sessionsSql,
  sqlString,
  subagentsSql,
  tasksSql,
} from "./countsql";
import {
  cutOffsets,
  main as fixturedbMain,
  lastTimestampMs,
  loadFixtures,
  storedPath,
  storeRefusal,
} from "./fixturedb";
import { PULL_START, PullRow, psqlRows } from "./psql-rows";

// The fixture proof of count.sql (the read count) and of models.sql, and the pieces they stand on:
// countsql.ts's filling and fixturedb.ts's loading. The proof needs the pod's test server
// (LEGION_TEST_PG_DSN) and psql, and runs each query with the store command's own flags; without
// the server it is skipped with a printed line (the pi-legion CI job has none). It makes eight
// databases of its own on that server and drops them at the end:
// F1's session, its subagent and F4 in the first; F1b in the second; F1's session in the third,
// cut into three parts inside its `alpha` read call line; and, for models.sql, in the fourth: F1c
// (F1's subagent with its model_change line taken out, so its model is its session_init's) cut
// after its session_init line, F1d (F1b's first two lines, with no model line) and F1b cut inside
// its first line, so that its model_change line is in its second part; in the fifth a file made
// here, F4 with a long line before its first assistant message, whose head models.sql cuts at 256
// Ki; in the sixth, for pull.sql and tasks.sql, the made-up sessions the pull tests describe; in
// the seventh a made-up session and three forks of it, which the fork tests describe; and in the
// eighth, for roots.sql, made-up files at absolute paths of a named profile, of the default
// profile and of neither. Three digest-times.sql rows each make one more database, and drop it in
// their own `finally`: F1's session cut inside its digest line, a made-up root of 256 MiB, and a
// made-up root with digest lines whose timestamps are no times.
// The rows of count.sql, models.sql and the pull, tasks and sessions queries on the fixture
// databases must equal those of their F7 captures (psql-rows.test.ts and this header give the
// capture commands), so a change to one of those queries that changes such a result fails here
// until F7 is captured again. The other rows (the made-up files', session-head.sql's,
// session-paths.sql's, subagents.sql's and digest-times.sql's) are checked against values the
// test computes from its inputs.
//
// F1c, F1d and models.sql's captures, from packages/pi-legion/scripts/skill-cuts, in the shell
// that ran psql-rows.test.ts's capture commands (its N, SERVER, FIRST, F1 and store()):
//
//   createdb --maintenance-db="$LEGION_TEST_PG_DSN" "skillcuts_models_$N"
//   MODELS=$SERVER/skillcuts_models_$N
//   grep -v '"type":"model_change"' $F1/ProductiveChicken.jsonl > testdata/F1c.jsonl
//   head -n 2 testdata/F1b.jsonl > testdata/F1d.jsonl
//   bun fixturedb.ts --dsn "$MODELS" --line '"type":"thinking_level_change"' testdata/F1c.jsonl
//   bun fixturedb.ts --dsn "$MODELS" testdata/F1d.jsonl
//   bun fixturedb.ts --dsn "$MODELS" --line '"type":"title"' testdata/F1b.jsonl
//   bun countsql.ts --config testdata/F7-skill-cuts.json --store store_a --day 2026-11-20 \
//     --query models | store "$FIRST" > testdata/F7-models-first.txt
//   bun countsql.ts --config testdata/F7-skill-cuts.json --store store_a --day 2026-11-20 \
//     --query models | store "$MODELS" > testdata/F7-models-second.txt
//   bun countsql.ts --config testdata/F7-skill-cuts.json --store store_a --day 2026-11-21 \
//     --query models | store "$MODELS" > testdata/F7-models-later.txt
//   dropdb --maintenance-db="$LEGION_TEST_PG_DSN" "skillcuts_models_$N"

const TESTDATA = path.join(import.meta.dir, "testdata");
const F1 = path.join(TESTDATA, "F1/2026-11-20T01-59-41-780Z_01a1f6ae-51d4-715f-a2a6-59f275bdff80");
const F1_SESSION = `${F1}.jsonl`;
const F1_SUBAGENT = `${F1}/ProductiveChicken.jsonl`;
const F4_SESSION = path.join(
  TESTDATA,
  "F4/projects/-legion-scratch-tmp-acme-1-t1-runs-f3-work/6441e7f4-71f3-49f1-a76f-0e0ffc82ea11.jsonl"
);
const F1B = path.join(TESTDATA, "F1b.jsonl");
const F1C = path.join(TESTDATA, "F1c.jsonl");
const F1D = path.join(TESTDATA, "F1d.jsonl");
const CONFIG = loadConfig(path.join(TESTDATA, "F7-skill-cuts.json"));
/** The text fixturedb cuts inside for the three-part file: F1's `alpha` read call line. */
const ALPHA_CALL = '"path":"skill://alpha"';

/** A file's hash as count.sql names it. */
function fileHash(stored: string): string {
  return createHash("md5").update(stored).digest("hex").slice(0, 16);
}

/** A read's key as count.sql names it: md5 of its call's id, a space and its call line's
 *  timestamp in UTC to the microsecond (`at`), cut to 16 hex characters. */
function callKey(id: string, at: string): string {
  return createHash("md5").update(`${id} ${at}`).digest("hex").slice(0, 16);
}

/** The fields of a count row the proof reads. */
const CountRow = z.object({
  row: z.literal("count"),
  skill: z.string(),
  kind: z.string(),
  copy: z.string(),
  files: z.number(),
  hashes: z.array(z.string()),
});

describe("countsql.ts fills the SQL", () => {
  test("count.sql for a day: every placeholder filled, the timeout first, the store a value", () => {
    const sql = countSql(CONFIG, "store_a", "2026-11-20");
    expect(sql.split("\n")[0]).toBe("SET LOCAL statement_timeout = '15min';");
    expect(sql).not.toContain("{{");
    expect(sql).toContain(
      `f.mtime_ms >= ${Date.parse("2026-11-20T00:00:00Z")} AND f.mtime_ms < ${Date.parse("2026-11-21T00:00:00Z")}`
    );
    expect(sql).toContain(
      "c.ts >= '2026-11-20T00:00:00.000Z'::timestamptz AND c.ts < '2026-11-22T00:00:00.000Z'::timestamptz"
    );
    expect(sql).toContain("'store', 'store_a'");
  });

  test("the hour form fills one hour of the day", () => {
    expect(mtimeRange("2026-11-20", 23)).toEqual({
      from: Date.parse("2026-11-20T23:00:00Z"),
      to: Date.parse("2026-11-21T00:00:00Z"),
    });
    expect(countSql(CONFIG, "store_a", "2026-11-20", 5)).toContain(
      `f.mtime_ms >= ${Date.parse("2026-11-20T05:00:00Z")} AND f.mtime_ms < ${Date.parse("2026-11-20T06:00:00Z")}`
    );
  });

  test("models.sql for a day or an hour: the same files as count.sql's, the timeout first, the store a value", () => {
    const sql = modelsSql(CONFIG, "store_a", "2026-11-20");
    expect(sql.split("\n")[0]).toBe("SET LOCAL statement_timeout = '15min';");
    expect(sql).not.toContain("{{");
    expect(sql).toContain(
      `f.mtime_ms >= ${Date.parse("2026-11-20T00:00:00Z")} AND f.mtime_ms < ${Date.parse("2026-11-21T00:00:00Z")}`
    );
    expect(sql).toContain("'store', 'store_a'");
    expect(modelsSql(CONFIG, "store_a", "2026-11-20", 5)).toContain(
      `f.mtime_ms >= ${Date.parse("2026-11-20T05:00:00Z")} AND f.mtime_ms < ${Date.parse("2026-11-20T06:00:00Z")}`
    );
    expect(() => modelsSql(CONFIG, "store_b", "2026-11-20")).toThrow('names no store "store_b"');
  });

  test.each([
    [
      "a store the configuration does not name",
      () => countSql(CONFIG, "store_b", "2026-11-20"),
      'names no store "store_b"',
    ],
    ["a day that is not a calendar day", () => mtimeRange("2026-11-31"), "is not a calendar day"],
    ["an hour out of range", () => mtimeRange("2026-11-20", 24), "is not an hour from 0 to 23"],
    [
      "a placeholder with no value",
      () => fillTemplate("SELECT {{a}}, {{b}}", { a: "1" }),
      "no value for the placeholder b",
    ],
    [
      "a value with no placeholder",
      () => fillTemplate("SELECT {{a}}", { a: "1", b: "2" }),
      "no placeholder for b",
    ],
    [
      "a pull place before the file row",
      () => pullSql("a.jsonl", { o: -3, k: 0 }),
      "is not an offset of -2 or more and a slice of 0 or more",
    ],
    [
      "a pull place of a slice below 0",
      () => pullSql("a.jsonl", { o: -1, k: -1 }),
      "is not an offset of -2 or more and a slice of 0 or more",
    ],
    [
      "a hash that is not 16 hex characters",
      () => sessionsSql(["83af4b8e0c75a05"]),
      "is not a file hash",
    ],
    [
      "a task query with no term",
      () => tasksSql(mtimeRange("2026-11-20"), "2026-11-20T00:00:00Z", "2026-11-21T00:00:00Z", []),
      "at least one term",
    ],
  ])("%s is refused", (_name, call, fault) => {
    expect(call).toThrow(fault);
  });

  test("a string value is one literal, whatever it holds", () => {
    expect(sqlString("it's a path\\with'quotes")).toBe("'it''s a path\\with''quotes'");
    expect(pullSql("a'b.jsonl", PULL_START)).toContain("WHERE f.path = 'a''b.jsonl'");
  });

  test("the command prints the query and refuses a store the configuration does not name", () => {
    const out: string[] = [];
    const err: string[] = [];
    const io = { out: (text: string) => out.push(text), err: (text: string) => err.push(text) };
    const config = path.join(TESTDATA, "F7-skill-cuts.json");
    expect(
      main(["--config", config, "--store", "store_a", "--day", "2026-11-20", "--hour", "3"], io)
    ).toBe(0);
    expect(out.join("")).toBe(countSql(CONFIG, "store_a", "2026-11-20", 3));
    expect(main(["--config", config, "--store", "store_c", "--day", "2026-11-20"], io)).toBe(2);
    expect(main(["--config", config, "--day", "2026-11-20"], io)).toBe(2);
    expect(
      main(["--config", config, "--store", "store_a", "--day", "2026-11-20", "--days", "2"], io)
    ).toBe(2);
    expect(err).toHaveLength(3);
    expect(err[0]).toContain('names no store "store_c"');
  });

  test("--query models prints models.sql, the count stays the default, and another query is refused", () => {
    const out: string[] = [];
    const err: string[] = [];
    const io = { out: (text: string) => out.push(text), err: (text: string) => err.push(text) };
    const config = path.join(TESTDATA, "F7-skill-cuts.json");
    const day = ["--config", config, "--store", "store_a", "--day", "2026-11-20"];
    expect(main([...day, "--query", "models", "--hour", "4"], io)).toBe(0);
    expect(main([...day, "--query", "count"], io)).toBe(0);
    expect(out).toEqual([
      modelsSql(CONFIG, "store_a", "2026-11-20", 4),
      countSql(CONFIG, "store_a", "2026-11-20"),
    ]);
    expect(main([...day, "--query", "pull"], io)).toBe(2);
    expect(err).toEqual([
      'countsql: --query "pull" is none of count, models, roots, hashes, transcript and transcript-coverage',
    ]);
  });

  test("session-head.sql is filled with the path hashes, each a SHA-256 in hex, and refuses any other", () => {
    const hash = pathHash("made-up/a.jsonl");
    const sql = sessionHeadSql([hash]);
    expect(sql).toStartWith("SET LOCAL statement_timeout = '15min';");
    expect(sql).toContain(`= ANY (ARRAY['${hash}']::text[])`);
    expect(sql).not.toContain("{{");
    expect(() => sessionHeadSql(["0123456789abcdef"])).toThrow(
      '"0123456789abcdef" is not a path hash of 64 hex characters'
    );
    expect(() => sessionHeadSql(["x'; DROP TABLE y; --"])).toThrow("is not a path hash");
  });

  test("session-paths.sql is filled with the path hashes, each a SHA-256 in lowercase hex, and refuses any other", () => {
    const hash = pathHash("made-up/a.jsonl");
    const sql = sessionPathsSql([hash]);
    expect(sql).toContain(`WHERE h.hash = ANY (ARRAY['${hash}']::text[])`);
    expect(sql).not.toContain("{{");
    for (const bad of [hash.toUpperCase(), "0123456789abcdef", "x'; DROP TABLE y; --"]) {
      expect(() => sessionPathsSql([hash, bad])).toThrow(
        `${JSON.stringify(bad)} is not a path hash of 64 hex characters`
      );
    }
  });

  test("subagents.sql is filled with each session's path hash and the times its subagent's start falls between, as one JSON array, and refuses a hash or a time of any other form", () => {
    const hash = pathHash("made-up/a.jsonl");
    const sql = subagentsSql([
      { hash, from: "2026-11-20T01:59:48.959Z", to: null },
      { hash, from: null, to: null },
    ]);
    expect(sql).toStartWith("SET LOCAL statement_timeout = '15min';");
    expect(sql).toContain(
      `jsonb_array_elements('[{"hash":"${hash}","from":"2026-11-20T01:59:48.959Z","to":null},{"hash":"${hash}","from":null,"to":null}]'::jsonb)`
    );
    expect(sql).not.toContain("{{");
    for (const bad of [hash.toUpperCase(), "0123456789abcdef", "x'; DROP TABLE y; --"]) {
      expect(() => subagentsSql([{ hash: bad, from: null, to: null }])).toThrow(
        `${JSON.stringify(bad)} is not a path hash of 64 hex characters`
      );
    }
    // A time Date.parse reads but that is not an ISO time (an HTTP date, a date alone) is refused
    // too: the query compares the text as given with `::timestamptz`.
    for (const bad of ["'; DROP TABLE y; --", "Fri, 20 Nov 2026 01:59:48 GMT", "2026-11-20"]) {
      expect(() => subagentsSql([{ hash, from: null, to: bad }])).toThrow(
        `${JSON.stringify(bad)} is not a time`
      );
    }
  });

  test("digest-times.sql is filled with the sessions' path hashes, each a SHA-256 in lowercase hex, and refuses any other", () => {
    const hash = pathHash("made-up/a/b.jsonl");
    const sql = digestTimesSql([hash]);
    expect(sql).toStartWith("SET LOCAL statement_timeout = '15min';");
    expect(sql).toContain(`SELECT DISTINCT unnest(ARRAY['${hash}']::text[]) AS hash`);
    expect(sql).not.toContain("{{");
    for (const bad of [hash.toUpperCase(), "0123456789abcdef", "x'; DROP TABLE y; --"]) {
      expect(() => digestTimesSql([hash, bad])).toThrow(
        `${JSON.stringify(bad)} is not a path hash of 64 hex characters`
      );
    }
  });

  // Catches: a store query with no statement_timeout (pull.sql, sessions.sql and session-paths.sql
  // had none), which only the client's quiet limit ended, and ending the client does not cancel
  // the query on the server: common.ts's STORE_QUIET_MS and the README say every SQL of the runner
  // ends itself after 15 minutes.
  test("every SQL file of the runner starts with the 15-minute statement_timeout", () => {
    const files = readdirSync(import.meta.dir).filter((file) => file.endsWith(".sql"));
    expect(files.length).toBeGreaterThanOrEqual(9);
    const first = (file: string) =>
      readFileSync(path.join(import.meta.dir, file), "utf8").split("\n")[0];
    expect(
      files.filter((file) => first(file) !== "SET LOCAL statement_timeout = '15min';")
    ).toEqual([]);
  });
});

describe("fixturedb.ts loads a fixture as the store keeps a file", () => {
  test("a file is cut inside the line that holds its middle byte, or the line --line names", () => {
    const bytes = new Uint8Array(readFileSync(F1_SESSION));
    for (const options of [{ parts: 2 }, { parts: 3, line: ALPHA_CALL }]) {
      const cuts = cutOffsets(bytes, options);
      expect(cuts).toHaveLength(options.parts - 1);
      const start = bytes.lastIndexOf(0x0a, (cuts[0] ?? 0) - 1) + 1;
      const end = bytes.indexOf(0x0a, cuts[0]);
      for (const cut of cuts) expect(cut > start && cut < end).toBe(true);
      if (options.line !== undefined) {
        expect(Buffer.from(bytes.subarray(start, end)).toString("utf8")).toContain(options.line);
      }
    }
  });

  test("a cut never splits a UTF-8 character, and a line too short to cut is refused", () => {
    // The line "a€€" is bytes 3 to 9; its middle, byte 6, is the first euro sign's last byte, so
    // the cut moves back over two continuation bytes to that sign's first byte, 4.
    const bytes = new TextEncoder().encode("{}\na€€\n{}\n");
    expect(cutOffsets(bytes, { parts: 2 })).toEqual([4]);
    expect(() =>
      cutOffsets(new TextEncoder().encode("{}\nx\n{}\n"), { parts: 3, line: "x" })
    ).toThrow("too short");
  });

  test("mtime_ms is the timestamp of the file's last line that has one", () => {
    expect(lastTimestampMs(readFileSync(F1_SESSION, "utf8"))).toBe(
      Date.parse("2026-11-20T02:00:04.020Z")
    );
    expect(lastTimestampMs(readFileSync(F4_SESSION, "utf8"))).toBe(
      Date.parse("2026-11-20T02:15:25.948Z")
    );
    expect(lastTimestampMs(readFileSync(F1B, "utf8"))).toBe(Date.parse("2026-11-21T01:59:48.974Z"));
  });

  test("a fixture is stored by its path below testdata/", () => {
    expect(storedPath(F1_SUBAGENT)).toBe(
      "F1/2026-11-20T01-59-41-780Z_01a1f6ae-51d4-715f-a2a6-59f275bdff80/ProductiveChicken.jsonl"
    );
  });

  test("the session store's DSN is refused", () => {
    const dir = mkdtempSync(path.join(process.env.TMPDIR ?? tmpdir(), "skill-cuts-fixturedb-"));
    try {
      const file = path.join(dir, "OMP_SESSION_SQL_DSN");
      writeFileSync(file, "postgres://sessions@db.example:5432/sessions\n");
      const env = { OMP_SESSION_SQL_DSN_FILE: file };
      expect(storeRefusal("postgres://sessions@db.example:5432/sessions", env)).toContain(
        "session store"
      );
      expect(storeRefusal("postgres://legion_test@127.0.0.1:5433/fixtures", env)).toBeUndefined();
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  });

  test("fixturedb.ts refuses, exit 2 and before any write, --parts under 2, a --line no line holds, and a session-store file it cannot read", async () => {
    const dir = mkdtempSync(path.join(process.env.TMPDIR ?? tmpdir(), "skill-cuts-fixturedb-"));
    try {
      // No database is reached: nothing listens on the DSN's port.
      const dsn = "postgres://legion_test@127.0.0.1:1/fixtures";
      const run = async (args: string[], env: Record<string, string> = {}) => {
        const err: string[] = [];
        const code = await fixturedbMain(args, env, {
          out: () => {},
          err: (text) => err.push(text),
        });
        return { code, err: err.join("\n") };
      };
      for (const parts of ["0", "1"]) {
        expect(await run(["--dsn", dsn, "--parts", parts, F1_SESSION])).toEqual({
          code: 2,
          err: `fixturedb: --parts ${parts} is not 2 or more`,
        });
      }
      const line = "a text no line of the fixture holds";
      expect(await run(["--dsn", dsn, "--line", line, F1_SESSION])).toEqual({
        code: 2,
        err: `fixturedb: no line holds ${JSON.stringify(line)}`,
      });
      // The session store's file names a directory, which cannot be read as a file.
      const unread = await run(["--dsn", dsn, F1_SESSION], { OMP_SESSION_SQL_DSN_FILE: dir });
      expect(unread.code).toBe(2);
      expect(unread.err).toContain("$OMP_SESSION_SQL_DSN_FILE cannot be read");
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  });
});

const serverDsn = process.env.LEGION_TEST_PG_DSN;
if (serverDsn === undefined) {
  console.log(
    "count.test.ts: LEGION_TEST_PG_DSN is unset: count.sql's fixture proof on Postgres is skipped"
  );
}

describe.skipIf(serverDsn === undefined)(
  "count.sql and models.sql on the fixtures, in test databases",
  () => {
    const stamp = `${process.pid}_${Date.now()}`;
    const names = {
      first: `skill_cuts_count_${stamp}_first`,
      second: `skill_cuts_count_${stamp}_second`,
      third: `skill_cuts_count_${stamp}_third`,
      models: `skill_cuts_count_${stamp}_models`,
      big: `skill_cuts_count_${stamp}_big`,
      pull: `skill_cuts_count_${stamp}_pull`,
      fork: `skill_cuts_count_${stamp}_fork`,
      roots: `skill_cuts_count_${stamp}_roots`,
    };
    /** The DSN of one of this run's databases on the server LEGION_TEST_PG_DSN names. */
    const dsnOf = (name: string) => {
      const url = new URL(serverDsn ?? "");
      url.pathname = `/${name}`;
      return url.toString();
    };
    let threeParts: readonly number[] = [];
    /** F4 with a 400,000-character line before its first assistant message, made in beforeAll. */
    const scratch = mkdtempSync(path.join(process.env.TMPDIR ?? tmpdir(), "skill-cuts-models-"));
    const BIG = path.join(scratch, "big.jsonl");
    let bigParts: readonly number[] = [];

    // The sixth database's made-up sessions. IMPORTED is stored as the store keeps a whole file
    // it imported and then appended to (ompsessions.go's Import and Append): its file row holds
    // more than 2 MiB, and its two parts start at the row's byte length. The file row's pad line
    // has a 3-byte character across each 1 MiB cut, so each cut moves back to that character's
    // first byte; its user message comes after its first MiB; its note line holds a DEL (U+007F,
    // which psql prints as \x7F), the text \x7F, a backslash before a DEL, and U+0085; and it
    // ends with a read of skill://alpha and the first half of its result's line, which the first
    // part ends. FIRST_MESSAGE is a file row only: its user message, then a line that runs past
    // the first MiB. MANY is loaded by fixturedb.ts in 70 parts, more than one call's page of rows.
    const MIB = 1_048_576;
    const IMPORTED = "made-up/imported.jsonl";
    const FIRST_MESSAGE = "made-up/first-message.jsonl";
    const MANY = path.join(scratch, "many.jsonl");
    /** A store command that runs the query in the database its URL argument names. */
    const STORE = path.join(scratch, "store.sh");
    const jsonLine = (value: unknown) => `${JSON.stringify(value)}\n`;
    const importedHeader = jsonLine({
      type: "session",
      version: 3,
      id: "made-up-imported",
      timestamp: "2026-11-20T03:00:00.000Z",
      cwd: "/work",
    });
    /** The bytes before the pad's text: the header line and `{"type":"pad","pad":"`. */
    const padAt = Buffer.byteLength(importedHeader) + '{"type":"pad","pad":"'.length;
    const readResult = jsonLine({
      type: "message",
      id: "r0000001",
      parentId: "c0000001",
      timestamp: "2026-11-20T03:00:01.600Z",
      message: {
        role: "toolResult",
        toolCallId: "toolu_made_up",
        toolName: "read",
        isError: false,
        content: [{ type: "text", text: "[Skill file: /skills/alpha/SKILL.md]\n# alpha\n" }],
      },
    });
    const resultCut = Math.floor(readResult.length / 2);
    const importedRow = [
      importedHeader,
      jsonLine({
        type: "pad",
        pad: `${"x".repeat(MIB - 1 - padAt)}€${"x".repeat(MIB - 3)}€${"x".repeat(1000)}`,
      }),
      jsonLine({
        type: "message",
        id: "u0000001",
        parentId: null,
        timestamp: "2026-11-20T03:00:01.000Z",
        message: { role: "user", content: "Fixture task: list the made-up steps." },
      }),
      jsonLine({ type: "note", text: "a\u007fb \\x7F c\\\u007f d\u0085" }),
      jsonLine({
        type: "message",
        id: "c0000001",
        parentId: "u0000001",
        timestamp: "2026-11-20T03:00:01.500Z",
        message: {
          role: "assistant",
          content: [
            {
              type: "toolCall",
              id: "toolu_made_up",
              name: "read",
              arguments: { path: "skill://alpha" },
            },
          ],
        },
      }),
      readResult.slice(0, resultCut),
    ].join("");
    const importedParts = [
      readResult.slice(resultCut),
      jsonLine({
        type: "note",
        timestamp: "2026-11-20T03:00:03.000Z",
        text: "Step 1 \u007f done.",
      }),
    ];
    const importedText = importedRow + importedParts.join("");
    const firstMessageRow =
      jsonLine({
        type: "message",
        id: "u0000002",
        parentId: null,
        timestamp: "2026-11-20T04:00:00.000Z",
        message: { role: "user", content: "Fixture task: write the made-up report." },
      }) + jsonLine({ type: "pad", pad: "y".repeat(MIB + 200_000) });

    // The seventh database's made-up sessions: PARENT reads alpha once, and three forks of it each
    // hold a copy of that read (its call and result lines byte for byte: the same toolCallId and
    // timestamp). Each is stored as one file row of the mtime given: PARENT and SAME_DAY of the
    // mtime day 2026-11-20, COPY of 2026-11-21, and OWN of 2026-11-22, which also reads alpha
    // itself after the copy. PARENT's path hash sorts before the forks' path hashes, and
    // SAME_DAY's path sorts before PARENT's path, so the rule (the file whose path hash sorts
    // first) and the path order pick different files.
    const FORK = {
      parent: "made-up/fork/parent.jsonl",
      sameDay: "made-up/fork/child-same-day.jsonl",
      copy: "made-up/fork/child-copy.jsonl",
      own: "made-up/fork/child-own.jsonl",
    };
    /** The call line and the result line of a read of skill://alpha whose call is `id`, at `at`. */
    const alphaRead = (id: string, at: string) =>
      jsonLine({
        type: "message",
        id: `c-${id}`,
        parentId: null,
        timestamp: at,
        message: {
          role: "assistant",
          content: [{ type: "toolCall", id, name: "read", arguments: { path: "skill://alpha" } }],
        },
      }) +
      jsonLine({
        type: "message",
        id: `r-${id}`,
        parentId: `c-${id}`,
        timestamp: new Date(Date.parse(at) + 1000).toISOString(),
        message: {
          role: "toolResult",
          toolCallId: id,
          toolName: "read",
          isError: false,
          content: [{ type: "text", text: "[Skill file: /skills/alpha/SKILL.md]\n# alpha\n" }],
        },
      });
    const PARENT_CALL = "toolu_made_up_parent";
    const parentRead = alphaRead(PARENT_CALL, "2026-11-20T02:00:01.000Z");
    /** A session's header line; a fork's names its parent's file. */
    const sessionLine = (id: string, at: string, parent?: string) =>
      jsonLine({
        type: "session",
        version: 3,
        id,
        timestamp: at,
        cwd: "/work",
        ...(parent === undefined ? {} : { parentSession: parent }),
      });
    const forkFiles = [
      {
        path: FORK.parent,
        mtime: "2026-11-20T02:00:05.000Z",
        text: sessionLine("made-up-parent", "2026-11-20T02:00:00.000Z") + parentRead,
      },
      {
        path: FORK.sameDay,
        mtime: "2026-11-20T05:00:00.000Z",
        text: sessionLine("made-up-same-day", "2026-11-20T05:00:00.000Z", FORK.parent) + parentRead,
      },
      {
        path: FORK.copy,
        mtime: "2026-11-21T03:00:00.000Z",
        text: sessionLine("made-up-copy", "2026-11-21T03:00:00.000Z", FORK.parent) + parentRead,
      },
      {
        path: FORK.own,
        mtime: "2026-11-22T00:30:00.000Z",
        text:
          sessionLine("made-up-own", "2026-11-21T22:00:00.000Z", FORK.parent) +
          parentRead +
          alphaRead("toolu_made_up_own", "2026-11-21T23:00:00.000Z"),
      },
    ];
    /** The eighth database's files, each at the path the store keeps it under, with the profile
     *  roots.sql names for it: a named profile's session and its subagent's file below it, the
     *  default profile's session, and a path below neither root. */
    const ROOTS = [
      ["/home/made-up/.omp/profiles/fixture/agent/sessions/--work--/named.jsonl", "fixture"],
      ["/home/made-up/.omp/profiles/fixture/agent/sessions/--work--/named/sub.jsonl", "fixture"],
      ["/home/made-up/.omp/agent/sessions/--work--/default.jsonl", "default"],
      ["/home/made-up/sessions/--work--/elsewhere.jsonl", "unknown"],
    ] as const;
    /** No copies: the fork tests count sessions, and place no read. */
    const NO_COPIES: R4Tables = {
      copies: [],
      prefixes: [],
      formerStages: [],
      installers: [],
      loaders: [],
      repositories: [],
    };
    /** psql's output of `sql` run in the database `name` with the store command's psql flags. */
    function output(name: string, sql: string): string {
      const psql = Bun.spawnSync(
        [
          "psql",
          "-X",
          "-q",
          "-w",
          "-v",
          "ON_ERROR_STOP=1",
          "--single-transaction",
          "-c",
          "SET TRANSACTION READ ONLY",
          "-f",
          "-",
          dsnOf(name),
        ],
        { stdin: new TextEncoder().encode(sql), stdout: "pipe", stderr: "pipe" }
      );
      if (psql.exitCode !== 0)
        throw new Error(`psql exited ${psql.exitCode}: ${psql.stderr.toString()}`);
      return psql.stdout.toString();
    }

    /** The rows of `sql` run in the database `name` with the store command's psql flags. */
    const run = (name: string, sql: string) => psqlRows(output(name, sql));

    /** The rows of an F7 capture. */
    const captured = (name: string) => psqlRows(readFileSync(path.join(TESTDATA, name), "utf8"));

    beforeAll(async () => {
      const server = new SQL(serverDsn ?? "");
      try {
        for (const name of Object.values(names)) await server.unsafe(`CREATE DATABASE ${name}`);
      } finally {
        await server.close();
      }
      await loadFixtures(dsnOf(names.first), [F1_SESSION, F1_SUBAGENT, F4_SESSION], { parts: 2 });
      await loadFixtures(dsnOf(names.second), [F1B], { parts: 2 });
      const [loaded] = await loadFixtures(dsnOf(names.third), [F1_SESSION], {
        parts: 3,
        line: ALPHA_CALL,
      });
      threeParts = loaded?.offsets ?? [];
      await loadFixtures(dsnOf(names.models), [F1C], {
        parts: 2,
        line: '"type":"thinking_level_change"',
      });
      await loadFixtures(dsnOf(names.models), [F1D], { parts: 2 });
      await loadFixtures(dsnOf(names.models), [F1B], { parts: 2, line: '"type":"title"' });
      const f4 = readFileSync(F4_SESSION, "utf8").split("\n");
      const firstAssistant = f4.findIndex((line) => line.includes('"type":"assistant"'));
      const pad = JSON.stringify({ type: "pad", pad: "x".repeat(400_000) });
      writeFileSync(
        BIG,
        [...f4.slice(0, firstAssistant), pad, ...f4.slice(firstAssistant)].join("\n")
      );
      const [big] = await loadFixtures(dsnOf(names.big), [BIG], { parts: 3, line: '"type":"pad"' });
      bigParts = big?.offsets ?? [];
      writeFileSync(
        MANY,
        jsonLine({ type: "session", id: "made-up-many", timestamp: "2026-11-20T05:00:00.000Z" }) +
          jsonLine({ type: "pad", pad: "z".repeat(7_000) })
      );
      await loadFixtures(dsnOf(names.pull), [MANY], { parts: 70 });
      const pull = new SQL(dsnOf(names.pull));
      try {
        let offset = Buffer.byteLength(importedRow);
        await pull`
          INSERT INTO public.omp_session_files (path, content, mtime_ms, byte_len)
          VALUES (${IMPORTED}, ${importedRow}, ${Date.parse("2026-11-20T03:00:03.000Z")},
            ${Buffer.byteLength(importedText)})
        `;
        for (const part of importedParts) {
          await pull`
            INSERT INTO public.omp_session_files_parts (path, start_offset, content)
            VALUES (${IMPORTED}, ${offset}, ${part})
          `;
          offset += Buffer.byteLength(part);
        }
        await pull`
          INSERT INTO public.omp_session_files (path, content, mtime_ms, byte_len)
          VALUES (${FIRST_MESSAGE}, ${firstMessageRow}, ${Date.parse("2026-11-20T04:00:00.000Z")},
            ${Buffer.byteLength(firstMessageRow)})
        `;
      } finally {
        await pull.close();
      }
      // No file to load: this makes the fork database's tables.
      await loadFixtures(dsnOf(names.fork), [], { parts: 2 });
      const fork = new SQL(dsnOf(names.fork));
      try {
        for (const file of forkFiles) {
          await fork`
            INSERT INTO public.omp_session_files (path, content, mtime_ms, byte_len)
            VALUES (${file.path}, ${file.text}, ${Date.parse(file.mtime)},
              ${Buffer.byteLength(file.text)})
          `;
        }
      } finally {
        await fork.close();
      }
      await loadFixtures(dsnOf(names.roots), [], { parts: 2 });
      const roots = new SQL(dsnOf(names.roots));
      try {
        for (const [stored] of ROOTS) {
          const text = sessionLine("made-up-root", "2026-11-20T02:00:00.000Z");
          await roots`
            INSERT INTO public.omp_session_files (path, content, mtime_ms, byte_len)
            VALUES (${stored}, ${text}, ${Date.parse("2026-11-20T02:00:00.000Z")},
              ${Buffer.byteLength(text)})
          `;
        }
      } finally {
        await roots.close();
      }
      writeFileSync(
        STORE,
        `#!/bin/sh\nexec psql -X -q -w -v ON_ERROR_STOP=1 --single-transaction -c 'SET TRANSACTION READ ONLY' -f - "$1"\n`,
        { mode: 0o755 }
      );
    }, 60_000);

    afterAll(async () => {
      const server = new SQL(serverDsn ?? "");
      try {
        for (const name of Object.values(names))
          await server.unsafe(`DROP DATABASE IF EXISTS ${name} WITH (FORCE)`);
      } finally {
        await server.close();
        rmSync(scratch, { recursive: true, force: true });
      }
    });

    test("roots.sql reports only the count hash and profile of each stored path", () => {
      expect(run(names.roots, rootsSql(CONFIG, "store_a"))).toEqual(
        ROOTS.map(([stored, profile]) => ({ hash: fileHash(stored), profile })).sort((a, b) =>
          a.hash < b.hash ? -1 : 1
        )
      );
    });

    test("hashes.sql includes files at the start bound and files modified after the window", () => {
      const all = ROOTS.map(([stored]) => ({ hash: fileHash(stored) }));
      const afterEnd = hashesSql(
        { ...CONFIG, windowStart: "2026-11-19T00:00:00Z", windowEnd: "2026-11-20T00:00:00Z" },
        "store_a"
      );
      expect(run(names.roots, afterEnd)).toEqual(expect.arrayContaining(all));
      const onStart = hashesSql({ ...CONFIG, windowStart: "2026-11-20T02:00:00.000Z" }, "store_a");
      expect(run(names.roots, onStart)).toHaveLength(all.length);
      const beforeStart = hashesSql(
        { ...CONFIG, windowStart: "2026-11-20T02:00:00.001Z" },
        "store_a"
      );
      expect(run(names.roots, beforeStart)).toEqual([]);
    });

    test("the first database: alpha one read and one reference session, the missing skill none, beta one read in the subagent's file, F4's Skill call one read", () => {
      const rows = run(names.first, countSql(CONFIG, "store_a", "2026-11-20"));
      expect(rows).toEqual(captured("F7-count-first.txt"));
      const counts = rows.flatMap((row) => {
        const parsed = CountRow.safeParse(row);
        return parsed.success ? [parsed.data] : [];
      });
      const count = (skill: string, kind: string) =>
        counts.filter((row) => row.skill === skill && row.kind === kind);
      const alphaReads = count("alpha", "read");
      expect(
        alphaReads.map((row) => [
          row.copy.endsWith("/profiles/fixture/agent/skills/alpha/SKILL.md"),
          row.files,
        ])
      ).toEqual([
        [true, 1],
        [false, 1],
      ]);
      expect(count("alpha", "reference").map((row) => row.files)).toEqual([1]);
      expect(count("gamma", "read")).toEqual([]);
      expect(
        count("beta", "read").find((row) => row.copy.includes("/profiles/fixture/"))?.hashes
      ).toEqual([fileHash(storedPath(F1_SUBAGENT))]);
      expect(alphaReads[1]?.copy).toEndWith("/plugins/acme-tools/skills/alpha/SKILL.md");
      expect(alphaReads[1]?.hashes).toEqual([fileHash(storedPath(F4_SESSION))]);
    });

    test("the second database: F1b's alpha, read on two days, is one read session", () => {
      const rows = run(names.second, countSql(CONFIG, "store_a", "2026-11-21"));
      expect(rows).toEqual(captured("F7-count-second.txt"));
      expect(rows[0]).toMatchObject({
        row: "count",
        skill: "alpha",
        kind: "read",
        day: "2026-11-20",
        files: 1,
      });
    });

    test("a matched line that spans three parts is still counted", () => {
      const bytes = new Uint8Array(readFileSync(F1_SESSION));
      expect(threeParts).toHaveLength(3);
      const middle = Buffer.from(bytes.subarray(threeParts[1], threeParts[2]));
      expect(middle.includes(0x0a)).toBe(false);
      const rows = run(names.third, countSql(CONFIG, "store_a", "2026-11-20"));
      expect(rows.slice(0, 2)).toEqual(captured("F7-count-first.txt").slice(0, 2));
      expect(rows.at(-1)).toMatchObject({ row: "summary", files: 1, parts: 3, bad_lines: 0 });
    });

    test("the pull, task, sessions and empty queries give their F7 rows, and pullTranscript pulls the file whole", async () => {
      const pull = run(names.first, pullSql(storedPath(F1_SESSION), PULL_START));
      expect(pull).toEqual(captured("F7-pull.txt"));
      const spool = path.join(scratch, "f1-pulled.jsonl");
      const f1 = readFileSync(F1_SESSION);
      expect(
        await pullTranscript(STORE, dsnOf(names.first), storedPath(F1_SESSION), spool)
      ).toEqual({ bytes: f1.length, calls: 1 });
      expect(readFileSync(spool).equals(f1)).toBe(true);
      expect(run(names.first, pullSql("no/such/path.jsonl", PULL_START))).toEqual([]);
      expect(
        run(
          names.first,
          tasksSql(mtimeRange("2026-11-20"), "2026-11-20T00:00:00Z", "2026-11-22T00:00:00Z", [
            "Fixture",
          ])
        )
      ).toEqual(captured("F7-tasks.txt"));
      const hashes = [F1_SESSION, F1_SUBAGENT, F4_SESSION].map((file) =>
        fileHash(storedPath(file))
      );
      expect(run(names.first, sessionsSql([...hashes, "0000000000000000"]))).toEqual(
        captured("F7-sessions.txt")
      );
    });

    /** Each pull row's place and the bytes of its text: `[o, k, bytes]`. */
    const placed = (rows: unknown[]) =>
      rows.map((row) => {
        const { o, k, c } = PullRow.parse(row);
        return [o, k, Buffer.byteLength(c)];
      });

    test("a file row of over 1 MiB comes as slices cut at each MiB, each cut moved back to the first byte of its character, and its parts follow at its byte length", () => {
      const row = Buffer.from(importedRow);
      // The cuts at 1 MiB and 2 MiB each fall on the second byte of a euro sign.
      expect([row[MIB], row[2 * MIB]].map((byte) => ((byte ?? 0) & 0xc0) === 0x80)).toEqual([
        true,
        true,
      ]);
      const first = row.length;
      const second = first + Buffer.byteLength(importedParts[0] ?? "");
      const all = [
        [-1, 0, MIB - 1],
        [-1, 1, MIB],
        [-1, 2, row.length - (2 * MIB - 1)],
        [first, 0, Buffer.byteLength(importedParts[0] ?? "")],
        [second, 0, Buffer.byteLength(importedParts[1] ?? "")],
      ];
      expect(placed(run(names.pull, pullSql(IMPORTED, PULL_START)))).toEqual(all);
      // A call starts after the place it is given: inside the file row, after it, after a part.
      expect(placed(run(names.pull, pullSql(IMPORTED, { o: -1, k: 0 })))).toEqual(all.slice(1));
      expect(placed(run(names.pull, pullSql(IMPORTED, { o: -1, k: 2 })))).toEqual(all.slice(3));
      expect(placed(run(names.pull, pullSql(IMPORTED, { o: first, k: 0 })))).toEqual(all.slice(4));
    });

    test("pullTranscript pulls an imported session appended to byte for byte, its DEL among them, though psql prints a DEL as \\x7F", async () => {
      const printed = output(names.pull, pullSql(IMPORTED, { o: -1, k: 1 }));
      expect(printed).toContain(String.raw`a\x7Fb`);
      expect(printed).toContain(String.raw`Step 1 \x7F done.`);
      const spool = path.join(scratch, "imported-pulled.jsonl");
      expect(await pullTranscript(STORE, dsnOf(names.pull), IMPORTED, spool)).toEqual({
        bytes: Buffer.byteLength(importedText),
        calls: 1,
      });
      expect(readFileSync(spool).equals(Buffer.from(importedText))).toBe(true);
    });

    test("a pull of more rows than a call's page takes calls until one returns fewer, and keeps the file's bytes", async () => {
      const stored = storedPath(MANY);
      expect(run(names.pull, pullSql(stored, PULL_START))).toHaveLength(PULL_PAGE);
      const spool = path.join(scratch, "many-pulled.jsonl");
      const many = readFileSync(MANY);
      expect(await pullTranscript(STORE, dsnOf(names.pull), stored, spool)).toEqual({
        bytes: many.length,
        calls: 2,
      });
      expect(readFileSync(spool).equals(many)).toBe(true);
    });

    test("the task query reads each file's first MiB only: a first user message after it gives no row", () => {
      expect(
        run(
          names.pull,
          tasksSql(mtimeRange("2026-11-20"), "2026-11-20T00:00:00Z", "2026-11-22T00:00:00Z", [
            "Fixture",
          ])
        )
      ).toEqual([{ path: FIRST_MESSAGE, text: "Fixture task: write the made-up report." }]);
    });

    test("count.sql on an imported session appended to: a read in its file row, whose result's line runs on into its first part", () => {
      expect(run(names.pull, countSql(CONFIG, "store_a", "2026-11-20"))).toEqual([
        {
          row: "count",
          store: "store_a",
          day: "2026-11-20",
          skill: "alpha",
          copy: "/skills/alpha/SKILL.md",
          kind: "read",
          files: 1,
          hashes: [fileHash(IMPORTED)],
          calls: {
            [fileHash(IMPORTED)]: [callKey("toolu_made_up", "2026-11-20T03:00:01.500000Z")],
          },
          unpaired: 0,
          unpaired_hashes: [],
          unpaired_calls: {},
        },
        {
          row: "summary",
          store: "store_a",
          mtime_from: Date.parse("2026-11-20T00:00:00Z"),
          mtime_to: Date.parse("2026-11-21T00:00:00Z"),
          files: 3,
          parts: 2 + 70,
          bytes:
            Buffer.byteLength(importedText) +
            Buffer.byteLength(firstMessageRow) +
            readFileSync(MANY).length,
          lines_parsed: 2,
          bad_lines: 0,
          shared_call_ids: 0,
        },
      ]);
    });

    /** count.sql's output on the fork database for the mtime day `day`, read by attribute.ts: one
     *  query a day, kept, since no test changes the database. */
    const forkQueries = new Map<string, QueryOutput>();
    const forkQuery = (day: string) => {
      let read = forkQueries.get(day);
      if (read === undefined) {
        read = parseQueryOutput(
          "store_a",
          day,
          output(names.fork, countSql(CONFIG, "store_a", day))
        );
        forkQueries.set(day, read);
      }
      return read;
    };
    /** attribute.ts's count of the fork database's queries of the mtime days `days`. */
    const forkCount = (...days: string[]) =>
      attribute(CONFIG, days.map(forkQuery), NO_COPIES, new Map());

    test("a fork written on another mtime day than its parent, whose only read is a copy of the parent's, counts once across the two days' queries", () => {
      // The fork's day query holds the fork and not its parent, so it counts the fork.
      const copyDay = forkQuery("2026-11-21");
      expect(copyDay.rows.map((row) => row.hashes)).toEqual([[fileHash(FORK.copy)]]);
      const count = attribute(CONFIG, [forkQuery("2026-11-20"), copyDay], NO_COPIES, new Map());
      expect(count.readSessions).toBe(1);
      expect(count.stores[0]?.readSessions).toBe(1);
    });

    test("a fork with a read of its own as well counts as its own session, and the client counts the call id its parent and it hold across two queries", () => {
      expect(forkCount("2026-11-20", "2026-11-22").stores[0]).toMatchObject({
        readSessions: 2,
        sharedCallIds: 1,
        sharedAcrossQueries: 1,
        filePairsAcrossQueries: 1,
      });
    });

    test("the shared call id is reported: by the query that holds two files of it, which counts it in the file whose path hash sorts first, and by the client across the queries, with the file pairs", () => {
      // SAME_DAY's path sorts before PARENT's, and PARENT's path hash before SAME_DAY's.
      expect(FORK.sameDay < FORK.parent).toBe(true);
      expect(fileHash(FORK.parent) < fileHash(FORK.sameDay)).toBe(true);
      const parentDay = forkQuery("2026-11-20");
      const key = callKey(PARENT_CALL, "2026-11-20T02:00:01.000000Z");
      expect(parentDay.rows).toEqual([
        {
          row: "count",
          store: "store_a",
          day: "2026-11-20",
          skill: "alpha",
          copy: "/skills/alpha/SKILL.md",
          kind: "read",
          files: 1,
          hashes: [fileHash(FORK.parent)],
          calls: { [fileHash(FORK.parent)]: [key] },
          unpaired: 0,
          unpaired_hashes: [],
          unpaired_calls: {},
        },
      ]);
      expect(parentDay.summary.shared_call_ids).toBe(1);
      // PARENT on its day, COPY and OWN each on theirs: three files of three queries hold the key,
      // three pairs of them.
      const count = forkCount("2026-11-20", "2026-11-21", "2026-11-22");
      expect(count.readSessions).toBe(2);
      expect(count.stores[0]).toMatchObject({
        readSessions: 2,
        sharedCallIds: 1,
        sharedAcrossQueries: 1,
        filePairsAcrossQueries: 3,
      });
    });

    /** The model rows of a models.sql result, each `[hash, model, source]`, and its summary. */
    const models = (rows: unknown[]) => ({
      files: rows.slice(0, -1).map((row) => {
        const { hash, model, source } = row as { hash: string; model: unknown; source: unknown };
        return [hash, model, source];
      }),
      summary: rows.at(-1),
    });

    test("models.sql on the first database: Oh My Pi's model_change, in a subagent's file too, and F4's first assistant message from its second part", () => {
      const rows = run(names.first, modelsSql(CONFIG, "store_a", "2026-11-20"));
      expect(rows).toEqual(captured("F7-models-first.txt"));
      // fixturedb cut F4 at its middle byte, before its first assistant message.
      const f4 = readFileSync(F4_SESSION);
      expect(f4.indexOf('"type":"assistant"')).toBeGreaterThan(
        cutOffsets(f4, { parts: 2 })[0] ?? 0
      );
      const sonnet = "anthropic/claude-sonnet-5-5";
      expect(models(rows)).toEqual({
        files: [
          [fileHash(storedPath(F1_SUBAGENT)), sonnet, "model_change"],
          [fileHash(storedPath(F4_SESSION)), "claude-sonnet-5-5", "assistant"],
          [fileHash(storedPath(F1_SESSION)), sonnet, "model_change"],
        ],
        // Each file is whole in its head: its every part starts below 262144.
        summary: expect.objectContaining({ files: 3, with_model: 3, bytes: 185787, bad_lines: 0 }),
      });
    });

    test("models.sql: a subagent's session_init, none in a file with no model line, and F1b's model_change from its second part", () => {
      const rows = run(names.models, modelsSql(CONFIG, "store_a", "2026-11-20"));
      expect(rows).toEqual(captured("F7-models-second.txt"));
      expect(models(rows).files).toEqual([
        [fileHash(storedPath(F1D)), null, null],
        [fileHash(storedPath(F1C)), "anthropic/claude-sonnet-5-5", "session_init"],
      ]);
      // F1b's first part is the first 127 bytes of its first line; its model_change line (its
      // third) is in its second part.
      const later = run(names.models, modelsSql(CONFIG, "store_a", "2026-11-21"));
      expect(later).toEqual(captured("F7-models-later.txt"));
      expect(readFileSync(F1B, "utf8").split("\n")[2]).toContain('"type":"model_change"');
      expect(models(later)).toEqual({
        files: [[fileHash(storedPath(F1B)), "anthropic/claude-sonnet-5-5", "model_change"]],
        summary: expect.objectContaining({ files: 1, with_model: 1, bytes: 19716 }),
      });
    });

    test("models.sql cuts the head at 256 Ki and reads no part that starts at 262144 or after", () => {
      // The long line's second cut starts a part past 262144; F4's first assistant message is
      // after it, and its model attachment before.
      expect(bigParts).toHaveLength(3);
      expect(bigParts[1] ?? 0).toBeLessThan(262_144);
      expect(bigParts[2] ?? 0).toBeGreaterThanOrEqual(262_144);
      const rows = run(names.big, modelsSql(CONFIG, "store_a", "2026-11-20"));
      // The head is the file's first 262144 characters (F4's lines hold a few that are not ASCII).
      const head = Array.from(readFileSync(BIG, "utf8")).slice(0, 262_144).join("");
      expect(head).not.toContain('"type":"assistant"');
      expect(models(rows)).toEqual({
        files: [[fileHash(storedPath(BIG)), "claude-sonnet-5-5", "model attachment"]],
        summary: expect.objectContaining({
          files: 1,
          with_model: 1,
          bytes: Buffer.byteLength(head),
          bad_lines: 0,
        }),
      });
    });

    /** The path hash of the fixture file `file` (cases.ts's `pathHash`), as the databases store
     *  its path. */
    const hashOf = (file: string) => pathHash(storedPath(file));

    /** What session-head.sql must give for `file`: its header's cwd and start, its first user
     *  message's timestamp and attribution, its journaled reminders and the runtime version of its
     *  first model_change line, read from the fixture. */
    const headOf = (file: string) => {
      const text = readFileSync(file, "utf8");
      const lines = text
        .split("\n")
        .filter((line) => line.trim() !== "")
        .map((line) => JSON.parse(line));
      const header = lines.find((line) => line.type === "session");
      const user = lines.find((line) => line.type === "message" && line.message?.role === "user");
      return {
        hash: hashOf(file),
        cwd: header.cwd,
        start: header.timestamp,
        first_user: {
          timestamp: user.message.timestamp,
          attribution: user.message.attribution ?? null,
        },
        reminders: lines
          .filter((line) => line.type === "custom" && line.customType === "date-cwd-reminder")
          .map(({ data }) => ({
            turn: data.turn,
            reminder: data.reminder,
            placement: data.placement,
          })),
        runtime: lines.find((line) => line.type === "model_change")?.runtime?.version ?? null,
        bytes: Buffer.byteLength(Array.from(text).slice(0, 262_144).join("")),
        bad_lines: 0,
      };
    };

    test("session-head.sql: each file of a hash, its header's cwd and start, its first user turn, its journaled reminders and its first model_change line's runtime version; no row for a hash the store does not hold", () => {
      const session = headOf(F1_SESSION);
      const subagent = headOf(F1_SUBAGENT);
      // F1's session and its subagent each journaled the reminder of their first user turn.
      expect(session.reminders).toHaveLength(1);
      expect(session.reminders[0]?.turn).toBe(
        `user:${session.first_user.timestamp}:${session.first_user.attribution}`
      );
      expect(subagent.first_user.attribution).toBe("agent");
      // F1 ran Oh My Pi 18.18.6, which its model_change line records.
      expect(session.runtime).toBe("18.18.6");
      const missing = pathHash("made-up/missing.jsonl");
      const rows = run(names.first, sessionHeadSql([session.hash, subagent.hash, missing]));
      expect(rows).toEqual([session, subagent].toSorted((a, b) => (a.hash < b.hash ? -1 : 1)));
      // F1's session cut into three parts inside one line: the head is joined whole.
      expect(threeParts.length).toBeGreaterThan(1);
      expect(run(names.third, sessionHeadSql([session.hash]))).toEqual([session]);
      expect(run(names.first, sessionHeadSql([missing]))).toEqual([]);
    });

    test("session-paths.sql: the path of each file of a path hash, in path order; no row for a hash the store does not hold", () => {
      const files = [F4_SESSION, F1_SUBAGENT, F1_SESSION].map((file) => ({
        path: storedPath(file),
        hash: hashOf(file),
      }));
      const missing = pathHash("made-up/missing.jsonl");
      const rows = run(names.first, sessionPathsSql([...files.map((each) => each.hash), missing]));
      expect(rows).toEqual(files.toSorted((a, b) => (a.path < b.path ? -1 : 1)));
      expect(run(names.first, sessionPathsSql([missing]))).toEqual([]);
    });

    test("subagents.sql: of a session's subagents one level below its directory, the earliest with a recorded skill listing whose start falls between the times, its session_init line whole, or none; the counts; no row for a session with no subagent, or a hash the store does not hold", () => {
      const lines = readFileSync(F1_SUBAGENT, "utf8").split("\n");
      const header = JSON.parse(lines.find((line) => line.includes('"type":"session"')) ?? "{}");
      const init = lines.find((line) => line.startsWith('{"type":"session_init"'));
      expect(init).toContain("<skills>");
      const start = header.timestamp as string;
      const before = new Date(Date.parse(start) - 1).toISOString();
      const missing = pathHash("made-up/missing.jsonl");
      const session = hashOf(F1_SESSION);
      const found = { hash: hashOf(F1_SUBAGENT), start, init, subagents: 1, recorded: 1, under: 1 };
      const none = { hash: null, start: null, init: null, subagents: 1, recorded: 1, under: 0 };
      expect(
        run(
          names.first,
          subagentsSql([
            { hash: session, from: null, to: null },
            { hash: session, from: start, to: null },
            { hash: session, from: null, to: start },
            { hash: session, from: before, to: start },
            { hash: hashOf(F1_SUBAGENT), from: null, to: null },
            { hash: missing, from: null, to: null },
          ])
        )
      ).toEqual([
        // No bound; from its start on (a start at `from` counts); before its start (one at `to`
        // does not count); and a window that ends at its start.
        { parent: session, from: null, to: null, ...found },
        { parent: session, from: null, to: start, ...none },
        { parent: session, from: before, to: start, ...none },
        { parent: session, from: start, to: null, ...found },
      ]);
      // The third database holds F1's session alone: no file below its directory, so no row.
      expect(run(names.third, subagentsSql([{ hash: session, from: null, to: null }]))).toEqual([]);
    });

    test("digest-times.sql: for a subagent, its root (the file its topmost <…>Z_<id> directory stands for), and for a top-level session itself, with the time and digest of each of the root's system-prompt-digest lines, a line the file row and its first part split read whole; no row for a hash the store does not hold", async () => {
      // F1's session's own digest lines, read as Oh My Pi wrote them.
      const digests = readFileSync(F1_SESSION, "utf8")
        .split("\n")
        .filter((line) => line.includes('"customType":"system-prompt-digest"'))
        .map((line) => {
          const entry = JSON.parse(line);
          return { at: entry.timestamp as string, digest: entry.data as string };
        });
      expect(digests.length).toBeGreaterThan(0);
      const missing = pathHash("made-up/missing.jsonl");
      const session = hashOf(F1_SESSION);
      const subagent = hashOf(F1_SUBAGENT);
      expect(run(names.first, digestTimesSql([subagent, session, missing]))).toEqual(
        [
          { hash: subagent, root: session, rootHeld: true, digests },
          { hash: session, root: session, rootHeld: true, digests },
        ].toSorted((a, b) => (a.hash < b.hash ? -1 : 1))
      );
      // F1's session stored as a file the store imported and then appended to, the file row
      // ending inside its digest line and its one part holding the rest.
      const text = readFileSync(F1_SESSION, "utf8");
      const cut = text.indexOf('"customType":"system-prompt-digest"') + 20;
      const name = `skill_cuts_count_${stamp}_digest_cut`;
      const server = new SQL(serverDsn ?? "");
      try {
        await server.unsafe(`CREATE DATABASE ${name}`);
        await loadFixtures(dsnOf(name), [], { parts: 2 });
        const db = new SQL(dsnOf(name));
        try {
          await db`
            INSERT INTO public.omp_session_files (path, content, mtime_ms, byte_len)
            VALUES (${storedPath(F1_SESSION)}, ${text.slice(0, cut)}, ${Date.parse("2026-11-20T02:00:04.020Z")},
              ${Buffer.byteLength(text)})
          `;
          await db`
            INSERT INTO public.omp_session_files_parts (path, start_offset, content)
            VALUES (${storedPath(F1_SESSION)}, ${Buffer.byteLength(text.slice(0, cut))}, ${text.slice(cut)})
          `;
        } finally {
          await db.close();
        }
        expect(run(name, digestTimesSql([session]))).toEqual([
          { hash: session, root: session, rootHeld: true, digests },
        ]);
      } finally {
        await server.unsafe(`DROP DATABASE IF EXISTS ${name} WITH (FORCE)`);
        await server.close();
      }
    });

    // Catches: the digest pattern run over a whole piece. Postgres's regexp_matches takes 4 bytes
    // a byte of its input in one allocation and refuses one past 1 GB, so a file row of 256 MiB
    // (count.sql's header: a file row that holds a whole older file) failed the whole query. The
    // row is made by the server (`repeat`), so no 256 MiB passes through this process.
    test("digest-times.sql: a root whose file row is one piece of 256 MiB of short lines gives its digest line, read line by line", async () => {
      const ROOT = "made-up/2026-11-01T10-00-00-000Z_0a0b0c0d-0000-4000-8000-000000000001.jsonl";
      const at = "2026-11-01T10:00:01.000Z";
      const line = `{"type":"custom","customType":"system-prompt-digest","data":"d1","id":"a1","parentId":null,"timestamp":"${at}"}\n`;
      const root = pathHash(ROOT);
      const name = `skill_cuts_count_${stamp}_digest_big`;
      const server = new SQL(serverDsn ?? "");
      try {
        await server.unsafe(`CREATE DATABASE ${name}`);
        await loadFixtures(dsnOf(name), [], { parts: 2 });
        const db = new SQL(dsnOf(name));
        try {
          // 262144 lines of 1 KiB, then the digest line.
          await db.unsafe(
            `INSERT INTO public.omp_session_files (path, content, mtime_ms, byte_len)
             SELECT $1, repeat(repeat('x', 1023) || E'\\n', 262144) || $2, 0, 0`,
            [ROOT, line]
          );
        } finally {
          await db.close();
        }
        expect(run(name, digestTimesSql([root]))).toEqual([
          { hash: root, root, rootHeld: true, digests: [{ at, digest: "d1" }] },
        ]);
      } finally {
        await server.unsafe(`DROP DATABASE IF EXISTS ${name} WITH (FORCE)`);
        await server.close();
      }
    }, 120_000);

    // Catches: the digests ordered by `at::timestamptz` with no guard, so one digest line whose
    // timestamp is no time failed the whole query (count.sql, tasks.sql and subagents.sql guard
    // their times with pg_input_is_valid).
    test("digest-times.sql: a digest line whose timestamp is no time is kept, after the lines whose timestamps are times, by its data; the query does not fail", async () => {
      const ROOT = "made-up/2026-11-01T10-00-00-000Z_0a0b0c0d-0000-4000-8000-000000000002.jsonl";
      const digest = (data: string, at: string) =>
        `{"type":"custom","customType":"system-prompt-digest","data":"${data}","id":"i-${data}","parentId":null,"timestamp":"${at}"}\n`;
      const lines = [
        digest("b-bad", "not a time"),
        digest("d2", "2026-11-01T10:05:00.000Z"),
        digest("a-bad", "later"),
        digest("d1", "2026-11-01T10:00:00.000Z"),
      ];
      const root = pathHash(ROOT);
      const name = `skill_cuts_count_${stamp}_digest_bad_time`;
      const server = new SQL(serverDsn ?? "");
      try {
        await server.unsafe(`CREATE DATABASE ${name}`);
        await loadFixtures(dsnOf(name), [], { parts: 2 });
        const db = new SQL(dsnOf(name));
        try {
          await db`
            INSERT INTO public.omp_session_files (path, content, mtime_ms, byte_len)
            VALUES (${ROOT}, ${lines.join("")}, ${0}, ${0})
          `;
        } finally {
          await db.close();
        }
        expect(run(name, digestTimesSql([root]))).toEqual([
          {
            hash: root,
            root,
            rootHeld: true,
            digests: [
              { at: "2026-11-01T10:00:00.000Z", digest: "d1" },
              { at: "2026-11-01T10:05:00.000Z", digest: "d2" },
              { at: "later", digest: "a-bad" },
              { at: "not a time", digest: "b-bad" },
            ],
          },
        ]);
      } finally {
        await server.unsafe(`DROP DATABASE IF EXISTS ${name} WITH (FORCE)`);
        await server.close();
      }
    }, 60_000);
  }
);
