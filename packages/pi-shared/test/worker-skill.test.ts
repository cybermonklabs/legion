// The rules written over the repository's own skills/, rather than over a plugin's staged
// partition: they hold whichever plugin ships the worker skill, so they are declared once, here,
// rather than in skills-guard.ts, which five test files import and which would run them once per
// importer.
import { expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import {
  brokenSharedDatabaseRules,
  MERGE_GATE,
  PR_BODY,
  RED_AT_THE_HEAD,
  redAtTheHeadFaults,
  retiredWorkerLifecycleClaims,
  WORKER_SKILL,
  WORKER_TOOLS,
} from "./skills-guard";

test("the worker skill says a released pod is not a stop, and never that a role stays live", () => {
  expect(retiredWorkerLifecycleClaims(readFileSync(WORKER_SKILL, "utf8"))).toEqual([]);
});

test("the worker tool reference forbids reads through the session URL, and claims no login that creates databases", () => {
  expect(brokenSharedDatabaseRules(readFileSync(WORKER_TOOLS, "utf8"))).toEqual([]);
});

test("the re-run rule's page says who presses it, and the skill, the merge gate and the body reference point at it", () => {
  expect(
    redAtTheHeadFaults({
      workerSkill: readFileSync(WORKER_SKILL, "utf8"),
      redAtTheHead: readFileSync(RED_AT_THE_HEAD, "utf8"),
      mergeGate: readFileSync(MERGE_GATE, "utf8"),
      prBody: readFileSync(PR_BODY, "utf8"),
    })
  ).toEqual([]);
});
