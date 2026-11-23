import { describe, expect, test } from "bun:test";

import { type CitedBlock, citationLabel } from "./citation-label";

/** A document of a heading, a paragraph, a requirement with its label and a check without one. */
const markdown = [
  "## Requirements",
  "",
  "Each lease is held once.",
  "",
  ":::requirement{#r3}",
  "**R3. Flip audit.** Before the lease handler ships, the team audits.",
  ":::",
  "",
  ":::check{#c1}",
  "The audit runs on a week of refused leases.",
  ":::",
  "",
].join("\n");

/** The blocks of `markdown`, each where its text runs. */
function blocksOf(): CitedBlock[] {
  const at = (text: string) => markdown.indexOf(text);
  const span = (id: string, type: string, first: string, last: string): CitedBlock => ({
    from: at(first),
    id,
    to: at(last) + last.length,
    type,
  });
  return [
    span("h", "heading", "## Requirements", "## Requirements"),
    span("p", "paragraph", "Each lease", "held once."),
    span("r3", "requirement", ":::requirement", "audits.\n:::"),
    span("c1", "check", ":::check", "leases.\n:::"),
  ];
}

describe("citationLabel", () => {
  test("names a typed block by its own label, up to the label's full stop", () => {
    expect(citationLabel(markdown, blocksOf(), "r3")).toBe("R3");
  });

  test("names a block with no label of its own by the heading it stands under", () => {
    expect(citationLabel(markdown, blocksOf(), "p")).toBe("Requirements");
    expect(citationLabel(markdown, blocksOf(), "c1")).toBe("Requirements");
  });

  test("says block where the block has neither, or the document holds no such block", () => {
    const withoutHeading = blocksOf().filter((block) => block.type !== "heading");
    expect(citationLabel(markdown, withoutHeading, "p")).toBe("block");
    expect(citationLabel(markdown, blocksOf(), "gone")).toBe("block");
  });
});
