/** `e2e/tools/shard-weights.ts` reads main's merged reports with gh. Whatever answers gh's calls in
 *  GitHub's place holds gh's token and writes gh's own words (its stderr) and its answers, so the
 *  tool prints neither, nor anything an answer holds but a run id and a commit of GitHub's shapes:
 *  it keeps each in a file of `shard-weights-words-<random>` under TMPDIR, which the line it prints
 *  names, with the three digits of gh's closing `(HTTP nnn)`.
 *
 *  Each row runs the tool itself, `--main 1` with a cache of its own, against a stand-in gh first
 *  on PATH that answers the listing and the download as the row says and records each call. A row
 *  puts a credential where the tool reads it and holds that no run of eight of its characters
 *  (redacttest.Piece's run) is in what the tool printed, stdout and stderr together; its twin, in
 *  the same row, holds that the credential did reach the tool: the file the tool names keeps it. The
 *  control is the stand-in's record of the calls the row expects. No row lets the tool reach its
 *  write of `e2e/shard-weights.json`: each one ends at the refusal it holds. */

import { describe, expect, test } from "bun:test";
import { existsSync, mkdirSync, readFileSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import { skipWithout } from "@legion/test-fixtures/host-tools";
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories";
import { rowLimit, runBounded } from "./bounded-subprocess";

const dispatchPackage = fileURLToPath(new URL("../..", import.meta.url));
const scratch = temporaryDirectories("shard-weights-gh-words");

/** The marker, as gh's own words and an answer's field carry it. */
const marker = "not-a-secret-token";
/** A credential of a token's shape, one word: a JSON parser's error quotes a bare word whole, so
 *  the rows of an answer that is no JSON carry this one, where the marker's hyphens would cut it. */
const tokenWord = "ghs_notasecrettoken0123456789";

const listCall =
  "api repos/{owner}/{repo}/actions/artifacts?name=dispatch-browser-report-1&per_page=100&page=1";
const zipCall = "api repos/{owner}/{repo}/actions/artifacts/7/zip";
const commit = "0123456789abcdef0123456789abcdef01234567";

/** The listing of one artifact of main's, its fields as `artifact` overrides them. */
function listing(workflowRun: Record<string, unknown> = {}): string {
  return JSON.stringify({
    artifacts: [
      {
        expired: false,
        id: 7,
        workflow_run: { head_branch: "main", head_sha: commit, id: 9, ...workflowRun },
      },
    ],
  });
}

/** Each run of eight characters of `secret` that `text` holds. */
function runsOf(secret: string, text: string): string[] {
  const runs: string[] = [];
  for (let start = 0; start + 8 <= secret.length; start++) {
    const run = secret.slice(start, start + 8);
    if (text.includes(run)) runs.push(run);
  }
  return runs;
}

/** What the stand-in gh does for one call: prints `out` as its answer and `err` on stderr, and
 *  exits `code`. */
interface Call {
  readonly out: string;
  readonly err?: string;
  readonly code?: number;
}

/** One run of the tool with the stand-in gh answering the listing as `list` and the download as
 *  `zip`, and the cache holding `cached` as run 9's report where it is given: what it printed,
 *  stdout and stderr together, its exit code, the stand-in's calls, and the text of each file the
 *  output names under the kept directory. `script` is the tool to run, this checkout's by default. */
async function toolRun(
  list: Call,
  zip: Call,
  cached?: string,
  script = join(dispatchPackage, "e2e/tools/shard-weights.ts")
): Promise<{ code: number; output: string; calls: string[]; kept: string[] }> {
  const bin = scratch("bin");
  const gh = scratch("gh");
  const cache = scratch("cache");
  const tmp = scratch("tmp");
  for (const [name, call] of [
    ["list", list],
    ["zip", zip],
  ] as const) {
    writeFileSync(join(gh, `${name}.out`), call.out);
    writeFileSync(join(gh, `${name}.err`), call.err ?? "");
    writeFileSync(join(gh, `${name}.code`), String(call.code ?? 0));
  }
  if (cached !== undefined) writeFileSync(join(cache, "9.json"), cached);
  writeFileSync(
    join(bin, "gh"),
    [
      "#!/usr/bin/env bash",
      `printf '%s\\n' "$*" >>'${gh}/calls'`,
      `case "$*" in`,
      `  "api repos/{owner}/{repo}/actions/artifacts?"*) call=list ;;`,
      `  "api repos/{owner}/{repo}/actions/artifacts/"*/zip) call=zip ;;`,
      `  *) echo "stand-in gh: no answer for this call" >&2; exit 3 ;;`,
      "esac",
      `cat '${gh}'/"$call.out"`,
      `cat '${gh}'/"$call.err" >&2`,
      `exit "$(cat '${gh}'/"$call.code")"`,
      "",
    ].join("\n"),
    { mode: 0o755 }
  );
  const { code, stdout, stderr } = await runBounded({
    argv: ["bun", script, "--main", "1", "--cache", cache],
    bound: "short",
    cwd: dispatchPackage,
    env: { HOME: scratch("home"), PATH: `${bin}:${process.env.PATH ?? ""}`, TMPDIR: tmp },
    from: import.meta.url,
  });
  const output = `${stdout}${stderr}`;
  const callsFile = join(gh, "calls");
  const calls = existsSync(callsFile)
    ? readFileSync(callsFile, "utf8")
        .split("\n")
        .filter((line) => line !== "")
    : [];
  const kept = [
    ...output.matchAll(new RegExp(`${tmp}/shard-weights-words-[^/\\s]+/[^\\s;)]+`, "g")),
  ]
    .map(([path]) => path)
    .filter((path) => existsSync(path))
    .map((path) => readFileSync(path, "utf8"));
  return { calls, code, kept, output };
}

const listed: Call = { out: listing() };

describe.skipIf(skipWithout("bash", "unzip"))("shard-weights.ts prints no word of gh's", () => {
  for (const row of [
    {
      name: "the listing gh failed, saying the marker",
      list: { out: "", err: `gh: ${marker} (HTTP 502)\n`, code: 1 },
      zip: { out: "" },
      secret: marker,
      prints:
        "gh's listing of the dispatch-browser-report-1 artifacts, page 1 exited 1 (HTTP 502); its own words are in ",
      calls: [listCall],
    },
    {
      name: "the listing gh answered with no JSON, a token-shaped word",
      list: { out: `${tokenWord}\n` },
      zip: { out: "" },
      secret: tokenWord,
      prints:
        "gh's listing of the dispatch-browser-report-1 artifacts, page 1 is no JSON; it is kept in ",
      calls: [listCall],
    },
    {
      name: "the listing gh answered with the marker as main's commit",
      list: { out: listing({ head_sha: marker }) },
      zip: { out: "" },
      secret: marker,
      prints:
        "main's full-lane artifact 1 of gh's listing has an id, a run id or a commit not of GitHub's shape; it is kept in ",
      calls: [listCall],
    },
    {
      name: "the download gh failed, saying the marker",
      list: listed,
      zip: { out: "", err: `gh: ${marker} (HTTP 404)\n`, code: 1 },
      secret: marker,
      prints: "gh's download of artifact 7 exited 1 (HTTP 404); its own words are in ",
      calls: [listCall, zipCall],
    },
    {
      name: "the cached report of a run gh listed is no JSON, a token-shaped word",
      list: listed,
      zip: { out: "" },
      cached: `${tokenWord}\n`,
      secret: tokenWord,
      prints: "the report of run 9 (",
      calls: [listCall],
    },
  ] as const) {
    test(
      `${row.name}: the tool prints no run of it, and names the file that keeps it`,
      async () => {
        const run = await toolRun(row.list, row.zip, "cached" in row ? row.cached : undefined);
        expect(runsOf(row.secret, run.output), `the tool printed:\n${run.output}`).toEqual([]);
        expect(run.output).toContain(row.prints);
        expect(run.code).toBe(1);
        // The control: the stand-in gh was asked, and only for the calls the row names.
        expect(run.calls).toEqual([...row.calls]);
        // The twin: a file the tool names keeps what gh said or answered.
        expect(
          run.kept.some((text) => text.includes(row.secret)),
          `the tool printed:\n${run.output}`
        ).toBe(true);
      },
      // The tool, a bounded child under the short bound.
      rowLimit("short")
    );
  }

  test(
    "a download unzip cannot read: the tool prints none of unzip's words and names the file that keeps them",
    async () => {
      const run = await toolRun(listed, { out: `${marker} is no zip\n` });
      expect(runsOf(marker, run.output), `the tool printed:\n${run.output}`).toEqual([]);
      expect(run.output).toMatch(
        /unzip -p \S+\/9\.zip report\.json exited \d+; its own words are in \S+/
      );
      expect(run.code).toBe(1);
      expect(run.calls).toEqual([listCall, zipCall]);
      // unzip's own words name the zip and quote none of it; they are kept all the same.
      expect(run.kept.length, `the tool printed:\n${run.output}`).toBe(1);
    },
    rowLimit("short")
  );
});

/** A report of one file per row, each `[project, file]` run once for a second. */
function reportOf(rows: readonly (readonly [string, string])[]): string {
  return JSON.stringify({
    suites: rows.map(([project, file]) => ({
      specs: [
        {
          file,
          tests: [{ projectName: project, results: [{ duration: 1000, status: "passed" }] }],
          title: "a row",
        },
      ],
      title: file,
    })),
  });
}

describe.skipIf(skipWithout("bash"))(
  "shard-weights.ts writes no row a report names but this checkout's",
  () => {
    test(
      "a row whose file is the marker's, and one whose project is: neither is written, and both are kept",
      async () => {
        // A copy of the tool beside a copy of the split it imports, an e2e/ of one spec and a config of
        // one project, so the run writes its table into the copy and never into this checkout's.
        const tree = scratch("tree");
        mkdirSync(join(tree, "tools"));
        writeFileSync(
          join(tree, "tools/shard-weights.ts"),
          readFileSync(join(dispatchPackage, "e2e/tools/shard-weights.ts"))
        );
        writeFileSync(
          join(tree, "shard-split.ts"),
          readFileSync(join(dispatchPackage, "e2e/shard-split.ts"))
        );
        writeFileSync(
          join(tree, "playwright.config.ts"),
          'const projects = [\n  {\n    name: "chromium",\n  },\n];\n'
        );
        writeFileSync(join(tree, "a.e2e.ts"), "");
        const run = await toolRun(
          listed,
          { out: "" },
          reportOf([
            ["chromium", "a.e2e.ts"],
            ["chromium", `${marker}.e2e.ts`],
            [marker, "a.e2e.ts"],
          ]),
          join(tree, "tools/shard-weights.ts")
        );
        const table = existsSync(join(tree, "shard-weights.json"))
          ? readFileSync(join(tree, "shard-weights.json"), "utf8")
          : "";
        // The control: the run read the listing and wrote its table, the admitted row in it.
        expect(run.calls).toEqual([listCall]);
        expect(run.code, `the tool printed:\n${run.output}`).toBe(0);
        expect(runsOf(marker, table), `the table:\n${table}`).toEqual([]);
        expect(JSON.parse(table).files).toEqual({ chromium: { "a.e2e.ts": 1 } });
        expect(runsOf(marker, run.output), `the tool printed:\n${run.output}`).toEqual([]);
        expect(run.output).toContain(
          "left out 2 rows whose project is none e2e/playwright.config.ts names"
        );
        // The twin: the file the tool names keeps both rows it left out.
        expect(run.kept.some((text) => runsOf(marker, text).length > 0)).toBe(true);
      },
      rowLimit("short")
    );
  }
);
