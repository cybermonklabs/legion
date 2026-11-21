import { describe, expect, test } from "bun:test";

import { blockQuestionMarkdown } from "./block-question";

const markdown = [
  "# Cold start",
  "",
  ':::ask{#cold-start urgency="med"}',
  "Should a cold start replay the whole ledger?",
  "",
  "**Recommended: from the snapshot.** The mirror is back sooner.",
  "",
  "- Full replay: Every lease.",
  "- From the snapshot: What came after it.",
  ":::",
  "",
].join("\n");

const from = markdown.indexOf(":::ask");
const blocks = [{ from, id: "cold-start", to: markdown.lastIndexOf(":::") + 3, type: "ask" }];

describe("blockQuestionMarkdown", () => {
  test("is the block's question as the document holds it, its marks kept and its options left out", () => {
    expect(blockQuestionMarkdown(markdown, blocks, "cold-start")).toBe(
      "Should a cold start replay the whole ledger?\n\n**Recommended: from the snapshot.** The mirror is back sooner."
    );
  });

  test("is undefined where the document holds no such decision block", () => {
    expect(blockQuestionMarkdown(markdown, blocks, "other")).toBeUndefined();
  });
});
