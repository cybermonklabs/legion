import { expect, test } from "bun:test";

import type { LegionBoardCard } from "../../api/types";
import { legionPhaseLabel, legionRolesText, legionSlotText } from "./issues-table-model";

function card(overrides: Partial<LegionBoardCard>): LegionBoardCard {
  return {
    blocked_by: [],
    checks_verdict: null,
    children: 0,
    key: "ACME-2",
    phase: "implementing",
    phase_since: null,
    priority: null,
    roles: [],
    slot: null,
    title: "A child",
    tree: "ACME-1",
    tree_slot: null,
    slot_since: null,
    waiting_place: null,
    waiting_on: null,
    ...overrides,
  };
}

test("a card's slot is its own, its tree root's for a child, or none", () => {
  expect(legionSlotText(card({ key: "ACME-1", slot: 3, tree: "ACME-1" }))).toBe("slot 3");
  expect(legionSlotText(card({ tree_slot: 3 }))).toBe("slot 3 · via ACME-1");
  expect(legionSlotText(card({}))).toBe("no slot");
  // An issue in a working phase that the waiting line names waits for a slot to come back.
  expect(legionSlotText(card({ waiting_place: 1 }))).toBe("waiting for a slot, #1 in line");
});

test("no role in a turn reads as between turns", () => {
  expect(legionRolesText(card({}))).toBe("between turns");
  expect(legionRolesText(card({ roles: ["implementer", "tester"] }))).toBe(
    "implementer, tester in a turn"
  );
});

test("an issue in retro reads 'Retro' though the board shows it in its Merging column", () => {
  expect(legionPhaseLabel("Merging", "retro")).toBe("Retro");
  expect(legionPhaseLabel("Merging", "merging")).toBe("Merging");
});
