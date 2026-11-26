import { describe, expect, test } from "bun:test";
import { LEGION_CLAIM_STATES } from "@legion/contracts/legion-api";

import type { LegionIssueResponse } from "../../api/types";
import { ROLE_STATE_WORDS } from "../issue-page/page-words";
import awaitingPersonBody from "./fixtures/legion-issue/awaiting_person.json";
import planningBody from "./fixtures/legion-issue/planning.json";
import { agentsAtWork, roleStateWord } from "./role-words";

// The bodies the server at this branch's head answered (`fixtures/README.md`): at planning, one
// role per claim state the daemon names and `role-unlisted` in a state it does not; awaiting a
// person, the architect working and the planner idle.
const PLANNING: LegionIssueResponse = planningBody;
const AWAITING_PERSON: LegionIssueResponse = awaitingPersonBody;

/** A claim state no list holds, which the planning body's `role-unlisted` is in. */
const UNKNOWN = "parked";

/** The plan's role table: each claim state of `LEGION_CLAIM_STATES` and one unknown, with the
 *  role words Dashboard draws for it, or none for a claim that is not listed. */
const ROLE_TABLE: readonly { readonly state: string; readonly words: string | undefined }[] = [
  { state: "working", words: ROLE_STATE_WORDS.in_turn },
  { state: "ready", words: ROLE_STATE_WORDS.idle },
  { state: "idle", words: ROLE_STATE_WORDS.idle },
  { state: "registered", words: ROLE_STATE_WORDS.idle },
  { state: "suspended", words: ROLE_STATE_WORDS.paused },
  { state: "queued", words: ROLE_STATE_WORDS.starting },
  { state: "launching", words: ROLE_STATE_WORDS.starting },
  { state: "launch_uncertain", words: ROLE_STATE_WORDS.starting },
  { state: "shim_connected", words: ROLE_STATE_WORDS.starting },
  { state: "failed", words: ROLE_STATE_WORDS.failed },
  { state: "retired", words: undefined },
  { state: UNKNOWN, words: ROLE_STATE_WORDS.another },
];

const ROLES = PLANNING.state?.roles ?? [];

describe("the role table", () => {
  for (const { state, words } of ROLE_TABLE) {
    test(`${state}: ${words ?? "not listed"}`, () => {
      // The captured body holds a role in this state.
      const role = ROLES.find((claim) => claim.state === state)?.role;
      if (role === undefined) throw new Error(`the captured body holds no role in ${state}`);
      expect(roleStateWord(state)).toBe(words);
      const drawn = agentsAtWork(ROLES).filter((line) => line.role === role);
      expect(drawn).toEqual(words === undefined ? [] : [{ role, words }]);
    });
  }

  test("each role that holds a claim stands in the order the state gives, a retired one left out", () => {
    for (const body of [PLANNING, AWAITING_PERSON]) {
      const roles = body.state?.roles ?? [];
      expect(agentsAtWork(roles).map((line) => line.role)).toEqual(
        roles.filter((claim) => claim.state !== "retired").map((claim) => claim.role)
      );
    }
    expect(agentsAtWork(AWAITING_PERSON.state?.roles).map((line) => line.role)).toEqual([
      "architect",
      "planner",
    ]);
  });
});

test("the table holds every claim state the daemon names, and one it does not", () => {
  expect(ROLE_TABLE.map((row) => row.state).sort()).toEqual(
    [...LEGION_CLAIM_STATES, UNKNOWN].sort()
  );
});

test("the role words are INFRA-1336's words for the state, each once", () => {
  const words: readonly string[] = Object.values(ROLE_STATE_WORDS);
  expect([...words].sort()).toEqual(
    ["in a turn", "idle", "paused", "starting", "failed", "in another state"].sort()
  );
});

test("a state with no roles read draws no agent", () => {
  expect(agentsAtWork(null)).toEqual([]);
  expect(agentsAtWork(undefined)).toEqual([]);
});
