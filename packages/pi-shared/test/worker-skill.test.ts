// The rules written over the repository's own skills/, rather than over a plugin's staged
// partition: they hold whichever plugin ships the worker skill, so they are declared once, here,
// rather than in skills-guard.ts, which five test files import and which would run them once per
// importer.
import { expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import {
  brokenSharedDatabaseRules,
  CI_CHECKS,
  CONTROLLER_SKILL,
  censusFaults,
  FINDING_ASKS,
  MERGE_GATE,
  RED_AT_THE_HEAD,
  RESULTS,
  redAtTheHeadFaults,
  retiredWorkerLifecycleClaims,
  testerCiCheckFaults,
  WORKER_SKILL,
  WORKER_TOOLS,
} from "./skills-guard";

/** The worker skill, read once: two rules below are written over it. */
const workerSkill = readFileSync(WORKER_SKILL, "utf8");

/** The four files `redAtTheHeadFaults` reads, read once: the rows below it splice into the page. */
const redAtTheHeadPages = {
  workerSkill,
  redAtTheHead: readFileSync(RED_AT_THE_HEAD, "utf8"),
  mergeGate: readFileSync(MERGE_GATE, "utf8"),
  results: readFileSync(RESULTS, "utf8"),
};

test("the worker skill says a released pod is not a stop, and never that a role stays live", () => {
  expect(retiredWorkerLifecycleClaims(workerSkill)).toEqual([]);
});

test("the worker tool reference forbids reads through the session URL, and claims no login that creates databases", () => {
  expect(brokenSharedDatabaseRules(readFileSync(WORKER_TOOLS, "utf8"))).toEqual([]);
});

test("the re-run rule's page says who presses it, and the skill, the merge gate and the results reference point at it", () => {
  expect(redAtTheHeadFaults(redAtTheHeadPages)).toEqual([]);
});

// The page's retired "fails the round" (the architect's gap review of 2026-11-20, item 3), which
// the row above expects nowhere in the page, and which a pattern that matches nothing passes too.
// So each wording of the class is added to the page as it stands, and is refused: the four the
// page carried at 7bf70a93, then the class's other tenses and determiners.
const roundFailures = [
  "It fails the round, or it requests changes, names the check, and says the head's one re-run is spent.",
  "- Red again: the round fails, or the reviewer requests changes, and the check is named.",
  "A test that passes locally is still the change's, and the round fails on it with no re-run.",
  "On its second red it is the round's failure, as *Red again* above says.",
  "A tester would fail the round on it.",
  "The round failed on that red.",
  "That is a failed round.",
];

test.each(roundFailures)("a page that says %p is refused", (wording) => {
  const redAtTheHead = `${redAtTheHeadPages.redAtTheHead}\n${wording}\n`;
  expect(redAtTheHeadFaults({ ...redAtTheHeadPages, redAtTheHead })).toEqual([
    expect.stringMatching(/^skills\/legion-worker\/references\/red-at-the-head\.md says "/),
  ]);
});

test("the controller's census acts on a test re-run green only where no open issue tracks it, and the skill routes it there", () => {
  expect(
    censusFaults({
      findingAsks: readFileSync(FINDING_ASKS, "utf8"),
      controllerSkill: readFileSync(CONTROLLER_SKILL, "utf8"),
    })
  ).toEqual([]);
});

test("the `ci_check` reference has the tester neither read nor record a check CI proves", () => {
  expect(testerCiCheckFaults(readFileSync(CI_CHECKS, "utf8"))).toEqual([]);
});
