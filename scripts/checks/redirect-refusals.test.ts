import { expect, test } from "bun:test";
import { LISTED_SITES, run, SITES } from "./redirect-refusals";

/** What the check of `sites`, counted as `count`, printed and answered, with no `--rows`. */
function checked(sites: typeof SITES, count: number): { exit: number; lines: string[] } {
  const lines: string[] = [];
  const exit = run([], sites, count, (line) => lines.push(line));
  return { exit, lines };
}

// A list that names no site checks nothing: such a run is not a pass, and it says what it could not
// read. No row runs.
test("a list of sites that names none is UNKNOWN, naming the list, and exits 1", () => {
  for (const count of [LISTED_SITES, 0]) {
    expect(checked({}, count)).toEqual({
      exit: 1,
      lines: [
        "UNKNOWN 0 listed sites: the list of sites of scripts/checks/redirect-refusals.ts (SITES) names none, so no row ran and nothing was checked",
      ],
    });
  }
});

// A site an edit drops from the list is checked by no row, so the list's length is held to the
// count beside it. No row runs.
test("a list of sites that lost one of its count fails before any row runs", () => {
  const [, ...six] = Object.entries(SITES);
  expect(checked(Object.fromEntries(six), LISTED_SITES)).toEqual({
    exit: 1,
    lines: [
      `FAIL the list of sites (SITES) names 6 sites, and the count beside it (LISTED_SITES) says ${LISTED_SITES}: a site the list lost or gained is checked only once the count moves with it`,
    ],
  });
});

// Each site's row is the redirect cases of the test file beside it, so two listed sites that name
// one file are checked by one row: the second site's own row never runs, and that site would pass
// while it followed a redirect again. Such a list fails, naming both sites, before any row runs.
test("a list in which two sites name one row file fails, naming both, before any row runs", () => {
  const label = "packages/pi-legion/scripts/skill-scenarios/label-comments.ts label";
  const production =
    "packages/pi-legion/scripts/skill-scenarios/production-dispatch.ts ProductionDispatch";
  const labelRow = SITES[label];
  if (labelRow === undefined) throw new Error(`SITES lists no ${label}`);
  expect(checked({ ...SITES, [production]: labelRow }, LISTED_SITES)).toEqual({
    exit: 1,
    lines: [
      `FAIL 2 listed sites name one row, packages/pi-legion/scripts/skill-scenarios/label-comments.test.ts: ${label}; ${production}. Each site's row is the test file beside it, so that one row would stand for all 2, and 1 of their own rows would never run`,
    ],
  });
});

test("the check's own list of sites is as long as the count beside it", () => {
  expect(Object.keys(SITES)).toHaveLength(LISTED_SITES);
});
