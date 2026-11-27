import { answerAsk, createIssue, createProject, getArtifactText, getAsk, getIssue } from "./api";
import { expectHosted } from "./ask-blocks-rows";
import {
  type Clipboard,
  copy,
  documentEditor,
  indexedBlockAsk,
  openWithCaret,
  paste,
  selectEditorText,
} from "./editor";
import { type Browser, expect, type Page, test } from "./test";
import { asUser } from "./users";

/**
 * Copy and paste of typed blocks - decision blocks and callouts - in the live document editor: a
 * copied block carries its content and its attributes, a copy of an answered decision pasted
 * above it leaves the answer on the original, and text pasted as plain text beside, inside or
 * over a block lands where each row says. The rows that answer a decision block in the document
 * are `ask-blocks.e2e.ts`'s.
 */

/** Copies the whole document through the editor's own copy handler. */
async function copyWholeDocument(page: Page): Promise<Clipboard> {
  await documentEditor(page).click();
  await page.keyboard.press("ControlOrMeta+A");
  return copy(page);
}

/** Pastes clipboard contents at the start of the text `quote`. */
async function pasteBefore(page: Page, quote: string, clipboard: Clipboard): Promise<void> {
  await selectEditorText(page, quote);
  await page.keyboard.press("ArrowLeft");
  await paste(page, clipboard);
}

const answeredDecisionSpec =
  'Intro.\n\n:::ask{#decision urgency="med" multiple="false"}\nWhich one?\n\n- A\n- B\n:::\n\nAfter.\n';

/** Seeds a spec holding the decision `decision`, answers it with A, and opens it as alice. */
async function openAnsweredDecision(browser: Browser) {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({
    project: "CORE",
    spec: answeredDecisionSpec,
    title: "Copied ask",
  });
  const blockAsk = await indexedBlockAsk(issue.key, "decision");
  await answerAsk(blockAsk.id, { expected_edited_at: null, selected: ["A"], text: "Go A." });
  const alice = await asUser(browser, "alice");
  const page = await alice.newPage();
  await page.goto(`/issues/${issue.key}`);
  const decision = documentEditor(page).locator('[data-dispatch-ask-block="decision"]');
  await expect((await expectHosted(decision)).getByText("Go A.")).toBeVisible();
  return { alice, blockAsk, issue, page };
}

/** Opens a spec holding only "End.", with the caret at its start or its end. */
function openEnd(browser: Browser, title: string, caret: "start" | "end") {
  return openWithCaret(browser, title, "End.\n", "End.", caret);
}

// A typed block's rendered DOM includes a header above its content hole. When a browser copies
// that rendered block, the parse rule must read the content hole rather than treat the header as
// its first paragraphs.
test("a copied decision block carries its content, not its attribute header", async ({
  browser,
}) => {
  const { alice, issue, page } = await openAnsweredDecision(browser);
  try {
    const clipboard = await copyWholeDocument(page);
    expect(clipboard.html).toContain("Which one?");
    expect(clipboard.html).not.toContain("data-proof-block-summary");
    expect(clipboard.html).not.toContain("answered_by");

    const header =
      '<header data-proof-block-summary=""><span data-proof-block-name="">ask</span>' +
      '<dl data-proof-block-attributes=""><dt>urgency</dt><dd data-proof-block-attribute="urgency">med</dd>' +
      '<dt>answer</dt><dd data-proof-block-attribute="answer">Go A.</dd></dl></header>';
    const rendered = clipboard.html.replace(
      /(<section[^>]*data-proof-block-type="ask"[^>]*>)([\s\S]*?)(<\/section>)/,
      `$1${header}<div data-proof-block-content="">$2</div>$3`
    );
    expect(rendered).toContain("data-proof-block-summary");
    expect(rendered).toContain("data-proof-block-content");
    await pasteBefore(page, "Intro.", { html: rendered, text: clipboard.text });

    await expect
      .poll(async () => ((await getIssue(issue.key)).open_asks ?? []).map((ask) => ask.question))
      .toEqual(["Which one?"]);
  } finally {
    await alice.close();
  }
});

// A real clipboard copy holds the typed blocks' client-owned DOM attributes. A rendered page also
// adds a header above each content hole. Pasting that rendered form must retain a callout's
// attributes and each block's exact content, while the existing answered decision keeps its id
// and server-owned answer.
test("a pasted rendered callout and decision keep attributes, content, and the original answer", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({
    project: "CORE",
    spec:
      'Intro.\n\n:::callout{#callout kind="warning" title="Risk"}\nCareful.\n:::\n\n' +
      ':::ask{#decision urgency="med" multiple="false"}\nWhich one?\n\n- A\n- B\n:::\n\nAfter.\n',
    title: "Copied typed blocks",
  });
  const originalAsk = await indexedBlockAsk(issue.key, "decision");
  await answerAsk(originalAsk.id, {
    expected_edited_at: null,
    selected: ["A"],
    text: "Go A.",
  });
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}`);
    const clipboard = await copyWholeDocument(page);
    expect(clipboard.html).toContain('data-proof-block-attr-kind="warning"');
    expect(clipboard.html).toContain('data-proof-block-attr-title="Risk"');

    const rendered = await page.evaluate((html) => {
      const template = document.createElement("template");
      template.innerHTML = html;
      for (const section of template.content.querySelectorAll<HTMLElement>(
        "section[data-proof-block-type]"
      )) {
        const header = document.createElement("header");
        header.dataset.proofBlockSummary = "";
        header.textContent = "This rendered header must not become document content.";
        const content = document.createElement("div");
        content.dataset.proofBlockContent = "";
        while (section.firstChild !== null) {
          content.append(section.firstChild);
        }
        section.append(header, content);
      }
      return template.innerHTML;
    }, clipboard.html);
    expect(rendered).toContain("data-proof-block-content");
    await pasteBefore(page, "Intro.", { html: rendered, text: clipboard.text });

    await expect
      .poll(async () => ((await getIssue(issue.key)).open_asks ?? []).map((ask) => ask.question))
      .toEqual(["Which one?"]);
    const stored = (await getArtifactText(issue.primary_artifact_id)).markdown;
    const callouts = [...stored.matchAll(/:::callout\{#([^ }]+)([^}]*)\}\n([\s\S]*?)\n:::/g)];
    const decisions = [...stored.matchAll(/:::ask\{#([^ }]+)([^}]*)\}\n([\s\S]*?)\n:::/g)];
    expect(callouts).toHaveLength(2);
    expect(callouts.map((match) => match[1])).toEqual([
      expect.not.stringMatching(/^callout$/),
      "callout",
    ]);
    expect(callouts.map((match) => match[2])).toEqual([
      expect.stringContaining('kind="warning"'),
      expect.stringContaining('kind="warning"'),
    ]);
    expect(callouts.map((match) => match[2])).toEqual([
      expect.stringContaining('title="Risk"'),
      expect.stringContaining('title="Risk"'),
    ]);
    expect(callouts.map((match) => match[3])).toEqual(["Careful.", "Careful."]);
    expect(decisions).toHaveLength(2);
    expect(decisions.map((match) => match[1])).toEqual([
      expect.not.stringMatching(/^decision$/),
      "decision",
    ]);
    expect(decisions.map((match) => match[3])).toEqual([
      "Which one?\n\n- A\n- B",
      "Which one?\n\n- A\n- B",
    ]);
    expect(stored).not.toContain("This rendered header must not become document content.");
    await expect
      .poll(async () => getAsk(originalAsk.id))
      .toMatchObject({
        ask: {
          answer: { selected: ["A"], text: "Go A." },
          block_id: "decision",
          state: "answered",
        },
      });
    await page.close();
  } finally {
    await alice.close();
  }
});

// A copy of an answered decision pasted above it is a new block. The editor keeps a block's id on
// the block that held it before the paste and mints one for the copy, so the answered ask stays
// on the original and the copy is indexed as a fresh open ask with the same question. Keeping the
// id on whichever of the two comes first would hand the copy the original's ask, its answer with
// it, and bring the original back as a fresh ask.
test("a copy of an answered decision pasted above it leaves the answer on the original", async ({
  browser,
}) => {
  const { alice, blockAsk, issue, page } = await openAnsweredDecision(browser);
  try {
    await pasteBefore(page, "Intro.", await copyWholeDocument(page));

    await expect
      .poll(async () => ((await getIssue(issue.key)).open_asks ?? []).map((ask) => ask.question))
      .toEqual(["Which one?"]);
    const copy = ((await getIssue(issue.key)).open_asks ?? [])[0];
    expect(copy?.block_id).not.toBe("decision");
    // Both asks read "Which one?", so only the stored order tells the copy from the original.
    const stored = (await getArtifactText(issue.primary_artifact_id)).markdown;
    expect([...stored.matchAll(/:::ask\{#([^ }]+)/g)].map((match) => match[1])).toEqual([
      copy?.block_id,
      "decision",
    ]);
    const held = (await getAsk(blockAsk.id)).ask;
    expect({ block: held.block_id, question: held.question, state: held.state }).toEqual({
      block: "decision",
      question: "Which one?",
      state: "answered",
    });
  } finally {
    await alice.close();
  }
});

// The editor's own HTML cleanup still runs on a paste: a paste from Google Docs arrives wrapped in
// <b id="docs-internal-guid-…">, which the editor unwraps, so the text pastes as written rather
// than bold.
test("a paste from Google Docs keeps the editor's own cleanup of its wrapper", async ({
  browser,
}) => {
  const { alice, issue, page } = await openEnd(browser, "Pasted wrapper", "end");
  try {
    await paste(page, {
      html: '<b id="docs-internal-guid-4a1b2c3d-7fff"><p>Wrapped words</p></b>',
      text: "Wrapped words",
    });

    await expect
      .poll(async () => (await getArtifactText(issue.primary_artifact_id)).markdown)
      .toBe("End.Wrapped words\n");
  } finally {
    await alice.close();
  }
});

// Typed blocks pasted as plain text are read as markdown, and the editor renders what it parsed
// to HTML before parsing that back: the rendering carries a typed block's section and content
// only, so a pasted decision's question and a callout's text arrive as written, never with the
// block's attribute list in front of them.
test("an ask and a callout pasted together as plain text arrive as written", async ({
  browser,
}) => {
  const { alice, issue, page } = await openEnd(browser, "Plain text paste", "end");
  try {
    await paste(page, {
      html: "",
      text:
        ':::ask{#d2 urgency="med" multiple="false"}\nWhich one?\n\n- A\n- B\n:::\n\n' +
        ':::callout{#c2 kind="warning" title="Risk"}\nCareful.\n:::\n',
    });

    await expect
      .poll(async () => ((await getIssue(issue.key)).open_asks ?? []).map((ask) => ask.question))
      .toEqual(["Which one?"]);
    const stored = (await getArtifactText(issue.primary_artifact_id)).markdown;
    expect(stored).toMatch(/:::callout\{#c2[^}]*\}\nCareful\.\n:::/);
  } finally {
    await alice.close();
  }
});

// A lone typed block pasted as plain text beside a paragraph's text stays a block of its own, on
// either side of that text: the pasted slice stops at the block, so its content never joins the
// paragraph the caret is in. Were it to join, an ask pasted at the end of "End." would store
// "End.Which one?" and a bare list, one pasted at its start "- BEnd." as the ask's last option, and
// a callout "End.Careful." or "Careful.End." inside the callout.
const loneAsk = ':::ask{#d2 urgency="med" multiple="false"}\nWhich one?\n\n- A\n- B\n:::\n';
const loneAskStored = /:::ask\{#d2 [^}]*\}\nWhich one\?\n\n- A\n- B\n:::\n/.source;
const loneCallout = ':::callout{#c2 kind="warning" title="Risk"}\nCareful.\n:::\n';
const loneCalloutStored = /:::callout\{#c2 [^}]*\}\nCareful\.\n:::\n/.source;

for (const [caret, where] of [
  ["end", "after"],
  ["start", "before"],
] as const) {
  const around = (block: string) =>
    new RegExp(caret === "end" ? `^End\\.\\n\\n${block}$` : `^${block}\\nEnd\\.\\n$`);

  test(`a lone ask pasted as plain text ${where} text stays an ask`, async ({ browser }) => {
    const { alice, issue, page } = await openEnd(browser, "Lone ask paste", caret);
    try {
      await paste(page, { html: "", text: loneAsk });

      await expect
        .poll(async () => ((await getIssue(issue.key)).open_asks ?? []).map((ask) => ask.question))
        .toEqual(["Which one?"]);
      await expect
        .poll(async () => (await getArtifactText(issue.primary_artifact_id)).markdown)
        .toMatch(around(loneAskStored));
    } finally {
      await alice.close();
    }
  });

  test(`a lone callout pasted as plain text ${where} text stays a callout`, async ({ browser }) => {
    const { alice, issue, page } = await openEnd(browser, "Lone callout paste", caret);
    try {
      await paste(page, { html: "", text: loneCallout });

      await expect
        .poll(async () => (await getArtifactText(issue.primary_artifact_id)).markdown)
        .toMatch(around(loneCalloutStored));
    } finally {
      await alice.close();
    }
  });
}

/** A document's markdown with each directive's attributes dropped, so a row compares the blocks and
 * the text around them. */
function withoutAttributes(markdown: string): string {
  return markdown.replace(/\{#([^ }]+)[^}]*\}/g, "{#$1}");
}

// The pasted block keeps its own block wherever the caret's container, or one around it inside the
// same typed block or table cell, can hold it.
for (const [target, spec, quote, pasted, stored] of [
  [
    "a blockquote",
    "Intro.\n\n> Quoted.\n",
    "Quoted.",
    loneAsk,
    "Intro.\n\n> Quoted.\n>\n> :::ask{#d2}\n> Which one?\n>\n> - A\n> - B\n> :::\n",
  ],
  [
    "a blockquote",
    "Intro.\n\n> Quoted.\n",
    "Quoted.",
    loneCallout,
    "Intro.\n\n> Quoted.\n>\n> :::callout{#c2}\n> Careful.\n> :::\n",
  ],
  [
    "a callout",
    ':::callout{#k1 kind="note" title=""}\nInside.\n:::\n',
    "Inside.",
    loneAsk,
    ":::callout{#k1}\nInside.\n:::\n\n:::ask{#d2}\nWhich one?\n\n- A\n- B\n:::\n",
  ],
  [
    "a callout",
    ':::callout{#k1 kind="note" title=""}\nInside.\n:::\n',
    "Inside.",
    loneCallout,
    ":::callout{#k1}\nInside.\n:::\n\n:::callout{#c2}\nCareful.\n:::\n",
  ],
  [
    "a list item",
    "- Listed.\n- Second.\n",
    "Listed.",
    loneAsk,
    "- Listed.\n  :::ask{#d2}\n  Which one?\n  - A\n  - B\n  :::\n- Second.\n",
  ],
  [
    "a list item",
    "- Listed.\n- Second.\n",
    "Listed.",
    loneCallout,
    "- Listed.\n  :::callout{#c2}\n  Careful.\n  :::\n- Second.\n",
  ],
] as const) {
  const block = pasted === loneAsk ? "ask" : "callout";
  test(`a lone ${block} pasted as plain text into ${target} keeps its block`, async ({
    browser,
  }) => {
    const { alice, issue, page } = await openWithCaret(browser, "Paste", spec, quote, "end");
    try {
      await paste(page, { html: "", text: pasted });

      await expect
        .poll(async () =>
          withoutAttributes((await getArtifactText(issue.primary_artifact_id)).markdown)
        )
        .toBe(stored);
      await expect
        .poll(async () => ((await getIssue(issue.key)).open_asks ?? []).map((ask) => ask.question))
        .toEqual(block === "ask" ? ["Which one?"] : []);
    } finally {
      await alice.close();
    }
  });
}

// Where nothing inside the caret's typed block can hold the pasted block, the pasted text joins the
// text at the caret. A lone callout, a lone ask, a list or a table pasted into an ask's question, as
// plain text or as HTML, joins the question as text, since an ask holds only its question and one
// options list. Splitting the ask instead would move its own options to a new ask under an empty
// question. Text joining a bold question takes the bold only when it is one line, as a one-line
// paste does anywhere else, so a lone callout or a list pasted there does not come out bold.
const question = "Which here?";
// It ends on a letter: after "?" the closing ** would sit between punctuation and a letter, where
// pmdoc writes it but doesn't read it back as closing the bold.
const boldQuestion = "**Which here**";
const calloutText = { html: "", text: loneCallout };
const listText = { html: "", text: "- x\n- y\n" };
const tableHtml = {
  html: "<table><tr><th>one</th><th>two</th></tr><tr><td>1</td><td>2</td></tr></table>",
  text: "one\ttwo\n1\t2",
};
for (const [shape, before, clipboard, stored] of [
  ["a lone callout as plain text", question, calloutText, "Which here?Careful."],
  ["a lone ask as plain text", question, { html: "", text: loneAsk }, "Which here?Which one? A B"],
  ["a list as plain text", question, listText, "Which here?x y"],
  [
    "a list as HTML",
    question,
    { html: "<ul><li>x</li><li>y</li></ul>", text: "x\ny" },
    "Which here?x y",
  ],
  ["a table as HTML", question, tableHtml, "Which here?one two 1 2"],
  ["one line as plain text", boldQuestion, { html: "", text: "x" }, "**Which herex**"],
  ["a lone callout as plain text", boldQuestion, calloutText, "**Which here**Careful."],
  ["a list as plain text", boldQuestion, listText, "**Which here**x y"],
] as const) {
  test(`${shape} pasted after the question ${JSON.stringify(before)} stores ${JSON.stringify(stored)}`, async ({
    browser,
  }) => {
    const { alice, issue, page } = await openWithCaret(
      browser,
      "Paste into a question",
      `:::ask{#q1 urgency="med" multiple="false"}\n${before}\n\n- X\n- Y\n:::\n`,
      before.replaceAll("*", ""),
      "end"
    );
    try {
      await paste(page, clipboard);

      await expect
        .poll(async () =>
          withoutAttributes((await getArtifactText(issue.primary_artifact_id)).markdown)
        )
        .toBe(`:::ask{#q1}\n${stored}\n\n- X\n- Y\n:::\n`);
      await expect
        .poll(async () => ((await getIssue(issue.key)).open_asks ?? []).map((ask) => ask.block_id))
        .toEqual(["q1"]);
    } finally {
      await alice.close();
    }
  });
}

// Plain text that holds no typed block pastes as it always has: its first paragraph or list item
// joins the text before the caret and its last one the text after it.
for (const [caret, text, stored] of [
  ["end", "More words.", "End.More words.\n"],
  ["end", "- x\n- y\n", "End.x\n\n- y\n"],
  ["start", "Alpha\n\nBravo\n", "Alpha\n\nBravoEnd.\n"],
] as const) {
  test(`${JSON.stringify(text)} pasted as plain text at the ${caret} of text stores ${JSON.stringify(stored)}`, async ({
    browser,
  }) => {
    const { alice, issue, page } = await openEnd(browser, "Plain paste", caret);
    try {
      await paste(page, { html: "", text });

      await expect
        .poll(async () => (await getArtifactText(issue.primary_artifact_id)).markdown)
        .toBe(stored);
    } finally {
      await alice.close();
    }
  });
}
