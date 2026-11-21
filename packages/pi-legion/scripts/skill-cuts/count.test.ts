import { afterAll, beforeAll, describe, expect, test } from "bun:test";
import { createHash } from "node:crypto";
import { mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import path from "node:path";
import { SQL } from "bun";
import { z } from "zod";
import { pullTranscript } from "./build";
import { loadConfig } from "./config";
import {
  countSql,
  fillTemplate,
  main,
  modelsSql,
  mtimeRange,
  PULL_PAGE,
  pullSql,
  sessionHeadSql,
  sessionsSql,
  sqlString,
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
// the server it is skipped with a printed line (the pi-legion CI job has none). It makes six
// databases of its own on that server and drops them at the end:
// F1's session, its subagent and F4 in the first; F1b in the second; F1's session in the third,
// cut into three parts inside its `alpha` read call line; and, for models.sql, in the fourth: F1c
// (F1's subagent with its model_change line taken out, so its model is its session_init's) cut
// after its session_init line, F1d (F1b's first two lines, with no model line) and F1b cut inside
// its first line, so that its model_change line is in its second part; in the fifth a file made
// here, F4 with a long line before its first assistant message, whose head models.sql cuts at 256
// Ki; and in the sixth, for pull.sql and tasks.sql, the made-up sessions the pull tests describe.
// Each query's rows must equal those of its F7 capture (psql-rows.test.ts gives the capture
// commands of count.sql's and pull.sql's), so a change to the SQL that changes a result fails here
// until F7 is captured again.
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
    expect(err).toEqual(['countsql: --query "pull" is neither count nor models']);
  });

  test("session-head.sql is filled with the path hashes, each a SHA-256 in hex, and refuses any other", () => {
    const hash = createHash("sha256").update("made-up/a.jsonl").digest("hex");
    const sql = sessionHeadSql([hash]);
    expect(sql).toStartWith("SET LOCAL statement_timeout = '15min';");
    expect(sql).toContain(`= ANY (ARRAY['${hash}']::text[])`);
    expect(sql).not.toContain("{{");
    expect(() => sessionHeadSql(["0123456789abcdef"])).toThrow(
      '"0123456789abcdef" is not a path hash of 64 hex characters'
    );
    expect(() => sessionHeadSql(["x'; DROP TABLE y; --"])).toThrow("is not a path hash");
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

    test("a file row of over 1 MiB comes as slices of at most 1 MiB, each cut at a character's first byte, and its parts follow at its byte length", () => {
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
          unpaired: 0,
          unpaired_hashes: [],
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
        summary: expect.objectContaining({ files: 3, with_model: 3, bytes: 185790, bad_lines: 0 }),
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

    /** What session-head.sql must give for `file`: its header's cwd and start, its first user
     *  message's timestamp and attribution, and its journaled reminders, read from the fixture. */
    const headOf = (file: string) => {
      const text = readFileSync(file, "utf8");
      const lines = text
        .split("\n")
        .filter((line) => line.trim() !== "")
        .map((line) => JSON.parse(line));
      const header = lines.find((line) => line.type === "session");
      const user = lines.find((line) => line.type === "message" && line.message?.role === "user");
      return {
        hash: createHash("sha256").update(storedPath(file)).digest("hex"),
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
        bytes: Buffer.byteLength(Array.from(text).slice(0, 262_144).join("")),
        bad_lines: 0,
      };
    };

    test("session-head.sql: each file of a hash, its header's cwd and start, its first user turn and its journaled reminders; no row for a hash the store does not hold", () => {
      const session = headOf(F1_SESSION);
      const subagent = headOf(F1_SUBAGENT);
      // F1's session and its subagent each journaled the reminder of their first user turn.
      expect(session.reminders).toHaveLength(1);
      expect(session.reminders[0]?.turn).toBe(
        `user:${session.first_user.timestamp}:${session.first_user.attribution}`
      );
      expect(subagent.first_user.attribution).toBe("agent");
      const missing = createHash("sha256").update("made-up/missing.jsonl").digest("hex");
      const rows = run(names.first, sessionHeadSql([session.hash, subagent.hash, missing]));
      expect(rows).toEqual([session, subagent].toSorted((a, b) => (a.hash < b.hash ? -1 : 1)));
      // F1's session cut into three parts inside one line: the head is joined whole.
      expect(threeParts.length).toBeGreaterThan(1);
      expect(run(names.third, sessionHeadSql([session.hash]))).toEqual([session]);
      expect(run(names.first, sessionHeadSql([missing]))).toEqual([]);
    });
  }
);
