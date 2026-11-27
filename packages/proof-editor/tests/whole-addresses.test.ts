import { expect, test } from "bun:test";
import type { Node as ProseMirrorNode } from "@milkdown/kit/prose/model";
import { createHeadlessProof } from "../src/lib-headless.js";
import { withMarksEditor } from "./marks-editor";
import { pastePlainText } from "./paste";

// A person's address is `dispatch://person/<login>`, and a login is an email address, so its local
// part may hold a dot or a hyphen. remark-gfm's transform starts an email after either, which
// read `dispatch://person/john.doe@example.com` as the text `dispatch://person/john.` and a link to
// `mailto:doe@example.com`. src/whole-addresses.ts puts such a link back as text, so the address
// is one text with no link, as Go's reader (pmdoc's keepAddressesWhole) reads it.
const PARAGRAPH =
  "Ask dispatch://person/john.doe@example.com and dispatch://person/person-2@example.com now.";

/** Each text node of `doc`, with the names of the marks it carries. */
function texts(doc: ProseMirrorNode): { marks: string[]; text: string }[] {
  const found: { marks: string[]; text: string }[] = [];
  doc.descendants((node) => {
    if (node.isText) {
      found.push({ marks: node.marks.map((mark) => mark.type.name), text: node.text ?? "" });
    }
    return true;
  });
  return found;
}

test("the headless engine reads a person's address whose login holds a dot or a hyphen as one text with no link", async () => {
  const { parseMarkdown } = await createHeadlessProof();
  expect(texts(parseMarkdown(`${PARAGRAPH}\n`))).toEqual([{ marks: [], text: PARAGRAPH }]);
});

test("a paste into the live editor keeps a person's address whose login holds a dot or a hyphen whole, with no link", async () => {
  await withMarksEditor("", async ({ view }) => {
    pastePlainText(view, `${PARAGRAPH}\n`);
    expect(texts(view.state.doc)).toEqual([{ marks: [], text: PARAGRAPH }]);
  });
});

// An address written after `](` ends at its first `)` that no `(` in it opened, the link's own, as
// `referenceSpans` reads it, so an email typed straight after that `)` starts outside the address
// and stays the link remark-gfm made, as Go's reader reads it. An escaped bracket opens no link,
// and the address still ends there.
test("the headless engine links an email typed straight after a link address's parenthesis", async () => {
  const { parseMarkdown } = await createHeadlessProof();
  expect(texts(parseMarkdown("Mail \\[w](dispatch://ACME-1)me@example.com now.\n"))).toEqual([
    { marks: [], text: "Mail [w](dispatch://ACME-1)" },
    { marks: ["link"], text: "me@example.com" },
    { marks: [], text: " now." },
  ]);
});

// An empty document's file holds no markdown (a VFile made of "" carries no value), and its tree
// holds nothing for the step to read, so the step lets it through: the engine reads the empty
// document, which holds no text, rather than throwing.
test("the headless engine reads an empty document", async () => {
  const { parseMarkdown } = await createHeadlessProof();
  expect(texts(parseMarkdown(""))).toEqual([]);
});
