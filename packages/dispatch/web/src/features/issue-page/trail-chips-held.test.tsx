import { afterEach, beforeAll, beforeEach, expect, spyOn, test } from "bun:test";
import { QueryClient } from "@tanstack/react-query";
import { act, cleanup, type RenderResult, render, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { displayTree } from "../../__tests__/display-trees";
import { api } from "../../api/client";
import type { IssueDetails, ReferenceTarget, ReferenceTargets } from "../../api/types";
import { DESKTOP_WIDTH, stubViewport } from "../margin/margin-fixture";
import { warmMarkdownRenderer } from "../refs/markdown-engine";
import { artifact, issueDetails, KEY, SPEC_DOCUMENT } from "./decisions-filter/decisions-fixtures";
import { IssueOutlinePage } from "./IssueOutlinePage";
import { presetPath } from "./issue-address";
import { CONTROL_LABELS } from "./page-words";
import { AppProviders, inDocument, stubReads, treeOf } from "./slots/stage-fixtures";
import { coverageRead, statementRef } from "./spec-filter/spec-fixtures";
import { treeNode, treeTuple } from "./tree-read/tree-fixtures";
import { VERIFICATION_SLUG } from "./verification-filter/verification-fixtures";

/**
 * The chips of a row's trail held from the first drawing (R3, the architect's choice 1 for the
 * check row the CI legs read at `da175e7a`: `LAY-1>spec#b-r3>verification-md#b-c29` grew from
 * 150 to 175 px in Conversation at 1440 px with no read in the frame before). A check that serves
 * three statements is drawn under each, and under r3 its trail holds two "Also under" chips, one
 * for r2 and one for r4. Each chip named its statement only once the page's names read had
 * answered, and drew nothing before: the trail then held two chips' words more, ran past the
 * words' last line and took a line of its own. Each block chip of a trail is drawn from the first
 * drawing with the name the tree already holds for its block, cut as the chip cuts it, invisible,
 * heard by no screen reader and reached by no Tab, and once the names read has answered the same
 * chip draws its name from that read, visible, as the page draws it at the base.
 *
 * A unit test's DOM lays nothing out, so the chip's box is read by what decides it: the element
 * itself, the words it holds and the classes of its name. The box the browser lays out is read by
 * row 1 of `c-load-1440`.
 */

beforeAll(async () => {
  await warmMarkdownRenderer();
});

/** The Verification document, whose check the case draws. */
const VERIFICATION_DOCUMENT = artifact("artifact-verification", VERIFICATION_SLUG);

/** The fixtures' issue with its Verification document beside its spec. */
function verifiedIssue(): IssueDetails {
  const issue = issueDetails();
  return {
    ...issue,
    artifacts: [...(issue.artifacts ?? []), VERIFICATION_DOCUMENT],
    queries: [
      {
        artifact_id: VERIFICATION_DOCUMENT.id,
        query: "verification",
        subtree: `issue:${KEY}`,
        takes: ["check", "result"],
      },
    ],
    verification_artifact_id: VERIFICATION_DOCUMENT.id,
  };
}

/** The spec's four statements, each by its block id. */
const STATEMENTS = ["r1", "r2", "r3", "r4"] as const;

/** A statement's words, as the tree read and the names read both hold them. */
const wordsOf = (block: string) => `The page draws ${block}.`;

/** The check that serves r2, r3 and r4: under r2 where the read places it, and under r3 and r4
 *  by its `serves` tuples. */
const CHECK = "c29";

/** The tree read's answer: the four statements, each with the server's reading of its words, and
 *  the check, with its three `serves` tuples. */
function treeAnswer() {
  return treeOf(
    [
      ...STATEMENTS.map((block, position) =>
        treeNode(block, "requirement", KEY, {
          ...inDocument(SPEC_DOCUMENT, position),
          text: wordsOf(block),
          text_tree: displayTree(wordsOf(block)),
        })
      ),
      treeNode(CHECK, "check", "r2", {
        ...inDocument(VERIFICATION_DOCUMENT),
        text: "Draw the page and read it.",
        text_tree: displayTree("Draw the page and read it."),
      }),
    ],
    ["r2", "r3", "r4"].map((block) => treeTuple("serves", CHECK, block))
  );
}

/** The names read's answer for each statement `refs` asks for: the block, named by its words. */
function namesAnswer(refs: readonly string[]): ReferenceTargets {
  const targets = STATEMENTS.flatMap((block): ReferenceTarget[] =>
    refs.includes(statementRef(block))
      ? [
          {
            gone: false,
            kind: "block",
            name: wordsOf(block),
            ref: statementRef(block),
            text_tree: displayTree(wordsOf(block)),
          },
        ]
      : []
  );
  return { targets };
}

/** The names read, held until the case answers it with what it asked for. */
interface HeldNames {
  readonly asked: () => readonly string[];
  readonly answer: () => Promise<void>;
}

let stubs: { mockRestore(): void }[] = [];
let restoreViewport: () => void = () => {};
let names: HeldNames;
beforeEach(() => {
  const asked: string[][] = [];
  const answers: ((value: ReferenceTargets) => void)[] = [];
  stubs = [
    ...stubReads(),
    spyOn(api, "getIssue").mockResolvedValue(verifiedIssue()),
    spyOn(api, "getIssueCoverage").mockResolvedValue(coverageRead([...STATEMENTS])),
    spyOn(api, "getIssueTree").mockImplementation(async (_key, query = {}) =>
      query.view === undefined ? treeAnswer() : treeOf([])
    ),
    spyOn(api, "getReferenceTargets").mockImplementation((refs) => {
      asked.push([...refs]);
      const { promise, resolve } = Promise.withResolvers<ReferenceTargets>();
      answers.push(resolve);
      return promise;
    }),
    spyOn(api, "getMyState").mockResolvedValue({}),
    spyOn(api, "listProjectArchitectureSources").mockResolvedValue([]),
  ];
  names = {
    answer: () =>
      act(async () => {
        for (const [at, resolve] of answers.entries()) resolve(namesAnswer(asked[at] ?? []));
      }),
    asked: () => asked.flat(),
  };
  restoreViewport = stubViewport(DESKTOP_WIDTH);
});
afterEach(() => {
  cleanup();
  restoreViewport();
  for (const stub of stubs.reverse()) stub.mockRestore();
});

/** The page's root at Conversation's address, under what the app provides above it. */
function drawPage(): RenderResult {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  return render(
    <MemoryRouter initialEntries={[presetPath(KEY, "conversation")]}>
      <AppProviders client={client}>
        <IssueOutlinePage />
      </AppProviders>
    </MemoryRouter>
  );
}

/** The check's row under r3. */
function checkRowUnderR3(view: RenderResult): HTMLElement | null {
  return view.container.querySelector<HTMLElement>(
    `[data-issue-body] [data-node-row][data-node-id$="#b-${CHECK}"][data-row-key*="#b-r3>"]`
  );
}

/** The chips of `row`'s trail, in order. */
function trailChips(row: HTMLElement): HTMLElement[] {
  return [...row.querySelectorAll<HTMLElement>("[data-node-trail] [data-node-chip]")];
}

/** The words each chip names its statement by. */
function chipNames(chips: readonly HTMLElement[]): (string | null)[] {
  return chips.map((chip) => chip.querySelector("[data-chip-name]")?.textContent ?? null);
}

/** The classes of each chip's name that decide its width: its weight and its wrapping. */
function nameBoxes(chips: readonly HTMLElement[]): string[][] {
  return chips.map((chip) =>
    (chip.querySelector("[data-chip-name]")?.className ?? "")
      .split(/\s+/u)
      .filter((name) => name === "font-medium" || name === "whitespace-nowrap")
      .sort()
  );
}

test("a check row's Also under chips are drawn invisible with the tree's names before the names read, and shown in the same place once it answers", async () => {
  const view = drawPage();
  const row = await waitFor(() => {
    const drawn = checkRowUnderR3(view);
    expect(drawn, "the outline drew no row of the check under r3").not.toBeNull();
    return drawn as HTMLElement;
  });
  await waitFor(() =>
    expect(names.asked(), "the page never asked for the statements' names").toContain(
      statementRef("r2")
    )
  );

  const held = trailChips(row);
  expect(
    held.length,
    "the row's trail held no chip for the two other statements before the names read answered"
  ).toBe(2);
  expect(chipNames(held), "the held chips are not the tree's names of r2 and r4").toEqual([
    wordsOf("r2"),
    wordsOf("r4"),
  ]);
  for (const chip of held) {
    expect(chip.getAttribute("aria-hidden"), "a held chip is heard").toBe("true");
    expect(chip.className, "a held chip is drawn visible").toContain("invisible");
    expect(
      chip.querySelector("a, [tabindex]"),
      "a held chip takes a press or a tab stop"
    ).toBeNull();
    expect(chip.textContent, "a held chip does not hold its tie's label").toContain(
      CONTROL_LABELS.also_under
    );
  }
  const heldBoxes = nameBoxes(held);
  const heldText = held.map((chip) => chip.textContent);

  await names.answer();
  await waitFor(() =>
    expect(
      trailChips(row).map((chip) => chip.querySelector("a[data-chip-name]") !== null),
      "a chip drew no link to its statement once the names read answered"
    ).toEqual([true, true])
  );
  const shown = trailChips(row);
  expect(
    shown.every((chip, at) => chip === held[at]),
    "the names read drew the chips again as other elements"
  ).toBe(true);
  expect(checkRowUnderR3(view), "the names read drew the row again as another element").toBe(row);
  for (const chip of shown) {
    expect(chip.getAttribute("aria-hidden"), "a shown chip is hidden").toBeNull();
    expect(chip.className, "a shown chip is still invisible").not.toContain("invisible");
  }
  expect(chipNames(shown), "the shown chips do not name r2 and r4 by the names read").toEqual([
    wordsOf("r2"),
    wordsOf("r4"),
  ]);
  expect(
    shown.map((chip) => chip.textContent),
    "a chip's words changed as the names read answered, which changes its box"
  ).toEqual(heldText);
  expect(nameBoxes(shown), "a chip's name changed its weight or its wrapping").toEqual(heldBoxes);
});
