import { describe, expect, test } from "bun:test";

import type { LegionIssueResponse } from "../../api/legion-issue";
import { PHASE_WORDS } from "../issue-page/page-words";
import accepting from "./fixtures/legion-issue/accepting.json";
import admitted from "./fixtures/legion-issue/admitted.json";
import another from "./fixtures/legion-issue/another.json";
import awaitingMerge from "./fixtures/legion-issue/awaiting_merge.json";
import awaitingPerson from "./fixtures/legion-issue/awaiting_person.json";
import configuredFalse from "./fixtures/legion-issue/configured-false.json";
import done from "./fixtures/legion-issue/done.json";
import held from "./fixtures/legion-issue/held.json";
import implementing from "./fixtures/legion-issue/implementing.json";
import integrating from "./fixtures/legion-issue/integrating.json";
import merging from "./fixtures/legion-issue/merging.json";
import planning from "./fixtures/legion-issue/planning.json";
import retro from "./fixtures/legion-issue/retro.json";
import reviewing from "./fixtures/legion-issue/reviewing.json";
import stateNull from "./fixtures/legion-issue/state-null.json";
import testing from "./fixtures/legion-issue/testing.json";
import unrecorded from "./fixtures/legion-issue/unrecorded.json";
import { legionRuns, type PhaseTrack, phaseTrack, TRACK_STEPS } from "./phase-track";

/** One row of the plan's phase table: the body the server at this branch's head answered for
 *  the case (`fixtures/README.md`), the `configured` and `state.phase` it holds, and the track
 *  Dashboard draws for it - none, or the step it marks and the words beside it. */
interface PhaseRow {
  readonly row: string;
  readonly body: LegionIssueResponse;
  readonly configured: boolean;
  readonly phase: string | null;
  readonly track: PhaseTrack | undefined;
}

const STEPS_MARKED: readonly Pick<PhaseRow, "body" | "phase" | "track">[] = [
  { body: admitted, phase: "admitted", track: { marked: "admitted" } },
  { body: implementing, phase: "implementing", track: { marked: "implementing" } },
  { body: testing, phase: "testing", track: { marked: "testing" } },
  { body: reviewing, phase: "reviewing", track: { marked: "reviewing" } },
  { body: accepting, phase: "accepting", track: { marked: "accepting" } },
  { body: retro, phase: "retro", track: { marked: "retro" } },
  { body: merging, phase: "merging", track: { marked: "merging" } },
];

const EACH_STEP: readonly PhaseRow[] = STEPS_MARKED.map((row) => ({
  ...row,
  configured: true,
  row: `each step: ${row.phase}`,
}));

const PHASE_TABLE: readonly PhaseRow[] = [
  {
    body: planning,
    configured: true,
    phase: "planning",
    row: "planning",
    track: { marked: "planning" },
  },
  {
    body: awaitingPerson,
    configured: true,
    phase: "awaiting_person",
    row: "awaiting_person",
    track: { marked: "admitted", words: PHASE_WORDS.awaiting_person },
  },
  {
    body: held,
    configured: true,
    phase: "held",
    row: "held",
    track: { words: PHASE_WORDS.held },
  },
  {
    body: integrating,
    configured: true,
    phase: "integrating",
    row: "integrating",
    track: { marked: "merging", words: PHASE_WORDS.integrating },
  },
  {
    body: awaitingMerge,
    configured: true,
    phase: "awaiting_merge",
    row: "awaiting_merge",
    track: { marked: "merging" },
  },
  {
    body: another,
    configured: true,
    phase: "parked",
    row: "another",
    track: { words: PHASE_WORDS.another },
  },
  ...EACH_STEP,
  { body: done, configured: true, phase: "done", row: "done", track: undefined },
  { body: unrecorded, configured: true, phase: "unrecorded", row: "unrecorded", track: undefined },
  { body: stateNull, configured: true, phase: null, row: "state null", track: undefined },
  {
    body: configuredFalse,
    configured: false,
    phase: null,
    row: "configured false",
    track: undefined,
  },
];

describe("the phase table", () => {
  for (const { body, configured, phase, row, track } of PHASE_TABLE) {
    test(row, () => {
      // The captured body is the case the row names.
      expect(body.configured).toBe(configured);
      expect(body.state?.phase ?? null).toBe(phase);
      // "An issue Legion runs" (R3) is one that draws a track, and no other.
      expect(legionRuns(body)).toBe(track !== undefined);
      expect(phaseTrack(body)).toEqual(track);
    });
  }
});

test("the track's eight steps stand in R3's order, under the page's words for them", () => {
  expect(TRACK_STEPS.map((step) => PHASE_WORDS.steps[step])).toEqual([
    "Admitted",
    "Planning",
    "Implementing",
    "Testing",
    "Reviewing",
    "Accepting",
    "Retro",
    "Merging",
  ]);
  expect([
    PHASE_WORDS.awaiting_person,
    PHASE_WORDS.integrating,
    PHASE_WORDS.held,
    PHASE_WORDS.another,
  ]).toEqual(["Waiting on a person", "Integrating", "Held", "In another phase"]);
});

test("no Legion state read yet is no issue Legion runs, and draws no track", () => {
  expect(legionRuns(undefined)).toBe(false);
  expect(phaseTrack(undefined)).toBeUndefined();
});
