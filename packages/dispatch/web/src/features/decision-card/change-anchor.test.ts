import { beforeAll, expect, test } from "bun:test";

import { displayTree } from "../../__tests__/display-trees";
import type { Decision } from "../../api/types";
import { warmMarkdownRenderer } from "../refs/markdown-engine";
import { changeAnchor } from "./change-anchor";

/**
 * The place a Change of a decision is anchored on (`changeAnchor`), as the Change of every
 * surface writes it: today's Decisions tab and comments margin through the decision card, and
 * the new issue page's `DecisionActions.tsx`. These cases came from `DecisionActions.test.tsx`
 * with the functions they test (R22); the send that reads the decisions document is each
 * caller's, and its own tests read it.
 */

const KEY = "CORE-1";

const decision: Decision = {
  asks: [],
  block_id: "d1",
  changed_at: null,
  changed_version: 1,
  heading: "One tree, five presets",
  kept: null,
  kind: "agent",
  links: [],
  ref: `dispatch://${KEY}/artifact/decisions-md#b-d1`,
  replaced: false,
  text: "One tree, five presets\n\nThe presets are queries over the one tree.",
  text_sha256: "sum",
  unsure: false,
  waiting_review: true,
};

/** Two decisions whose first blocks both hold "Draw it once.", the second's whole: a Change of
 *  the second quotes words that stand more than once in the decisions document. */
const ABOVE: Decision = {
  ...decision,
  block_id: "d0",
  heading: "Keep the model pure",
  ref: `dispatch://${KEY}/artifact/decisions-md#b-d0`,
  text: "## Keep the model pure\n\nKeep the model pure. Draw it once.",
};
const OWN: Decision = {
  ...decision,
  heading: "Draw it once",
  text: "## Draw it once\n\nDraw it once.",
};

/** The decisions document that holds `ABOVE` and `OWN`, below its title and `intro`. */
function twoDecisionsText(intro: string): string {
  return `# Decisions of CORE-1\n\n${intro}\n\n${ABOVE.text}\n\n${OWN.text}`;
}

beforeAll(async () => {
  await warmMarkdownRenderer();
});

// Decision 92: two decisions can open with the same sentence, and Dispatch refuses a quote that
// matches more than one place unless the write names which. A Change names its own decision's
// place: here the quote also stands, earlier, in the decision above it, so it is the second.
test("a Change names which place of the quote is its decision's own", () => {
  const document = displayTree(twoDecisionsText("The decisions this issue made."));
  expect(changeAnchor(OWN, [ABOVE, OWN], document)).toEqual({
    occurrence: 1,
    quote: "Draw it once.",
  });
});

// Decision 92 as amended: the server counts a quote's places over the whole decisions document,
// so a Change counts over its reading (`markdown_tree`), never over the decisions read alone.
// Here the document's intro, which no decision holds, quotes the decision too, so its own place
// is the third: a count over the decisions read says the second.
test("a Change counts its quote's places over the decisions document's reading", () => {
  const document = displayTree(
    twoDecisionsText("Draw it once. That is the rule of every decision below.")
  );
  expect(
    changeAnchor(OWN, [ABOVE, OWN], document),
    "the Change's place was not counted over the decisions document's reading"
  ).toEqual({ occurrence: 2, quote: "Draw it once." });
});
