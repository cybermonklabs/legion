import { test as bunTest, expect } from "bun:test";
import { screen, within } from "@testing-library/react";

import {
  bothCardsRow,
  cardOf,
  codeCardRows,
  REWORK_CARD,
} from "../../__tests__/code-cards-harness";

// The post-merge rework card of the Evals tab (dispatch://INFRA-3206/artifact/plan-md, T10): its
// rows of the cards' state table, one test a row, its name (spec R14), and the kinds of removal
// it draws beside each group's rework share.

bunTest.each(codeCardRows(REWORK_CARD))("%s", async (_row, run) => {
  await run();
});

bunTest(
  "the rework card is never named Rework share",
  bothCardsRow(() => {
    const card = cardOf(REWORK_CARD);
    expect(card).toHaveAttribute("aria-label", "Post-merge rework at 30 days");
    expect(
      within(card).getByRole("button", { name: "Post-merge rework at 30 days" })
    ).toBeInTheDocument();
    expect(card.textContent ?? "").not.toMatch(/rework share/i);
    // The title-match number keeps its own name, on its own chart in the same section.
    const section = screen.getByRole("region", { name: "Deploys and lead time" });
    expect(within(section).getByRole("article", { name: "Rework share" })).toHaveAttribute(
      "data-eval",
      "rework-share"
    );
  })
);

bunTest(
  "each group's rework share stands with iteration, no issue and other beside it",
  bothCardsRow(() => {
    expect(
      [...cardOf(REWORK_CARD).querySelectorAll("[data-group]")].map((group) => [
        group.getAttribute("data-group"),
        group.querySelector("[data-share]")?.textContent,
        [...group.querySelectorAll("[data-kind]")].map((kind) => [
          kind.getAttribute("data-kind"),
          kind.textContent,
        ]),
        group.querySelector("[data-counts]")?.textContent,
      ])
    ).toEqual([
      [
        "agents",
        "70.0%",
        [
          ["iteration", "iteration 2.0%"],
          ["noissue", "no issue 0.5%"],
          ["other", "other 3.0%"],
        ],
        "105 of 150 lines removed \u00b7 2 changes",
      ],
      [
        "people",
        "50.0%",
        [
          ["iteration", "iteration 2.0%"],
          ["noissue", "no issue 0.5%"],
          ["other", "other 3.0%"],
        ],
        "20 of 40 lines removed \u00b7 1 change",
      ],
    ]);
  })
);
