import { expect, test } from "bun:test";
import { GO_PLANT, run } from "./db-openers";
import { PLANTED, plantedName } from "./db-openers/planted";
import type { JudgedOpener } from "./db-openers.judged";

/** What the search printed and answered over `typescript` and no Go module, with `judged`. */
function searched(
  typescript: Record<string, string>,
  judged: readonly JudgedOpener[] = [],
  requirePlants = false
): { exit: number; lines: string[] } {
  const lines: string[] = [];
  const exit = run(
    { go: [], typescript: new Map(Object.entries(typescript)) },
    judged,
    (line) => lines.push(line),
    { requirePlants }
  );
  return { exit, lines };
}

const OPENER = `import { SQL } from "bun";
export function open(url: string) {
  return new SQL(url);
}
`;

// A search that read nothing listed nothing: that is not a pass, and it says so.
test("a search that lists no opener is UNKNOWN and exits 1", () => {
  expect(searched({})).toEqual({
    exit: 1,
    lines: [
      "TypeScript: 0 files read, 0 uses",
      "UNKNOWN the search listed no opener in 0 Go modules and 0 TypeScript files, so nothing was checked",
    ],
  });
});

// The real run reads both halves' planted shapes; one that did not is blind, whatever it found.
test("a run that did not read its planted shapes is UNKNOWN and exits 1", () => {
  const { exit, lines } = searched({ "a.ts": OPENER }, [], true);
  expect(exit).toBe(1);
  expect(lines).toContain(
    `UNKNOWN the TypeScript half did not read its planted shapes (${plantedName})`
  );
  expect(lines).toContain(
    `UNKNOWN no Go module read is ${GO_PLANT.module}, which holds the Go half's planted shapes, so they were not read`
  );
});

const JUDGEMENT = {
  file: "a.ts",
  function: "open",
  callee: "bun SQL",
  argument: "url",
  why: "a test's own judgement",
};

test("the TypeScript half lists each planted shape as marked", () => {
  const { lines } = searched({ [plantedName]: PLANTED, "a.ts": OPENER }, [JUDGEMENT]);
  expect(lines.filter((line) => line.startsWith("UNKNOWN"))).toEqual([]);
});

test("an opener handed a value no reading judges fails, naming it", () => {
  expect(searched({ "a.ts": OPENER })).toEqual({
    exit: 1,
    lines: [
      "UNJUDGED a.ts:3 open: bun SQL(url): the value url",
      "TypeScript: 1 files read, 1 uses",
      "FAIL 1 opener calls: 0 judged by what they pass, 0 in wrappers judged at each call, 0 by the list; 1 unjudged, 0 stale entries",
    ],
  });
});

test("a judgement that names no call the search lists fails, naming it", () => {
  const judged = [JUDGEMENT, { ...JUDGEMENT, function: "gone", why: "a call since removed" }];
  expect(searched({ "a.ts": OPENER }, judged)).toEqual({
    exit: 1,
    lines: [
      "STALE a.ts gone: bun SQL(url): the list judges a call the search no longer lists",
      "TypeScript: 1 files read, 1 uses",
      "FAIL 1 opener calls: 0 judged by what they pass, 0 in wrappers judged at each call, 1 by the list; 0 unjudged, 1 stale entries",
    ],
  });
});

// A judgement is of one opener: the same call made with another client is a new call to judge,
// and the old judgement, which names a call no longer there, is stale. The control is the call
// unchanged, which its judgement still judges.
test("a judgement names the call's opener: the same call with another client is unjudged", () => {
  expect(searched({ "a.ts": OPENER }, [JUDGEMENT]).exit).toBe(0);
  const pg = OPENER.replace('import { SQL } from "bun"', 'import { Client as SQL } from "pg"');
  expect(searched({ "a.ts": pg }, [JUDGEMENT])).toEqual({
    exit: 1,
    lines: [
      "UNJUDGED a.ts:3 open: pg Client(url): the value url",
      "STALE a.ts open: bun SQL(url): the list judges a call the search no longer lists",
      "TypeScript: 1 files read, 1 uses",
      "FAIL 1 opener calls: 0 judged by what they pass, 0 in wrappers judged at each call, 0 by the list; 1 unjudged, 1 stale entries",
    ],
  });
});

// A judgement names the whole argument, so a change past the 120 characters a line prints is a
// new call to judge. The control is the long call unchanged, and the same call written over two
// lines, since only the white space is folded.
test("a judgement names the call's whole argument: a change past its printed part is unjudged", () => {
  const head = `{ connectionString: url, application_name: "${"x".repeat(150)}", password: `;
  const call = (rest: string) =>
    `import { SQL } from "bun";\nexport function open(url: string) {\n  return new SQL(${head}${rest} });\n}\n`;
  const judged = [{ ...JUDGEMENT, argument: `${head}"" }` }];
  expect(searched({ "a.ts": call('""') }, judged).exit).toBe(0);
  expect(
    searched({ "a.ts": call('""').replace("password: ", "password:\n    ") }, judged).exit
  ).toBe(0);
  const changed = searched({ "a.ts": call("process.env.OTHER_PASSWORD") }, judged);
  expect(changed.exit).toBe(1);
  expect(
    changed.lines.filter((line) => /^(UNJUDGED|STALE) /.test(line)).map((l) => l.split(" ")[0])
  ).toEqual(["UNJUDGED", "STALE"]);
  expect(changed.lines.every((line) => line.length < 400)).toBe(true);
});
