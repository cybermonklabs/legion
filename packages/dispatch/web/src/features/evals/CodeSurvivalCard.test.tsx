import { test as bunTest, expect } from "bun:test";
import { screen } from "@testing-library/react";

import {
  bothCardsRow,
  cardOf,
  codeCardRows,
  REWORK_CARD,
  SURVIVAL_CARD,
} from "../../__tests__/code-cards-harness";

// The code survival card of the Evals tab (dispatch://INFRA-3206/artifact/plan-md, T10): its rows
// of the cards' state table, one test a row, and the inventory of what both card faces name
// (spec R14, control 10 of dispatch://INFRA-3117/artifact/method-md).

bunTest.each(codeCardRows(SURVIVAL_CARD))("%s", async (_row, run) => {
  await run();
});

bunTest(
  "each card face names the reading and the diff",
  bothCardsRow(() => {
    for (const card of [SURVIVAL_CARD, REWORK_CARD]) {
      const face = cardOf(card);
      // On the face, beside the value: the card is drawn closed, its method and panel unopened.
      expect(face.querySelector("[data-reading]")?.textContent).toBe(
        "replay reading, not blame \u00b7 GitHub diff"
      );
      expect(face.querySelector("[data-group] [data-share]")?.textContent).toBe("70.0%");
    }
    expect(screen.queryByRole("dialog")).toBeNull();
  })
);

bunTest(
  "each group's line names the lines alive of the lines added, and its changes",
  bothCardsRow(() => {
    expect(
      [...cardOf(SURVIVAL_CARD).querySelectorAll("[data-group]")].map((group) => [
        group.getAttribute("data-group"),
        group.querySelector("[data-counts]")?.textContent,
      ])
    ).toEqual([
      ["agents", "105 of 150 lines alive \u00b7 2 changes"],
      ["people", "20 of 40 lines alive \u00b7 1 change"],
    ]);
  })
);
