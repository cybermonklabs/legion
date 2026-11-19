import type { Locator, Page } from "@playwright/test";

import { CONTROL_LABELS } from "../web/src/features/issue-page/page-words";
import { createIssue, createProject, getArtifact } from "./api";
import { PROJECT } from "./issue-layout-seed-support";

/**
 * How the Spec view's rows read the new issue page's Spec view and its Contents slot
 * (`e2e/issue-page-j.e2e.ts`): the readers that `e2e/issue-page.ts` (the page's root and its
 * frame), `e2e/issue-page-g.ts` (one node's row, and the tree as rows) and `e2e/issue-page-i.ts`
 * (the outline) do not hold, and the one seed of the view's own.
 *
 * What the Spec view and the Contents slot draw, beyond what those files name:
 *
 * - **The issue's three sentences** `[data-testid="issue-three-things"]`, each an `li` whose
 *   label is `[data-page-chrome]` and whose sentence is `[data-author-text]`: in the Contents
 *   slot (the Contents pane and the Outline sheet), and as the Spec view's first group.
 * - **The Contents list**, one `[data-contents-entry="<node id>"]` per entry, its line
 *   `[data-contents-line]`, and above it the toggle named `CONTROL_LABELS.focus_branch`.
 * - **The Spec view** `[data-spec-body]`: its legend `[data-spec-legend]` of four
 *   `[data-spec-legend-mark]`, its column heads `[data-spec-head]`, its tree `[data-spec-tree]`,
 *   one `[data-spec-group]` per parent and one `[data-spec-run]` per run of statements, its
 *   decision cells `[data-spec-cell]` (`data-said`, `data-spec-statements`), its statements
 *   `[data-spec-statement]` (`data-spec-mark`), and under each statement the line
 *   `[data-spec-because]` naming its decision.
 */

// ---------------------------------------------------------------------------------------------
// The seed.
// ---------------------------------------------------------------------------------------------

/** One issue of `seedChildWithSpec`: its key, its spec's slug, and the block ids of the
 *  statements its spec holds, in document order. */
export interface IssueWithSpec {
  readonly key: string;
  readonly requirements: readonly string[];
  readonly specSlug: string;
}

export interface ChildWithSpecSeed {
  readonly child: IssueWithSpec;
  readonly parent: IssueWithSpec;
}

/** A statement of a seeded spec: one `requirement` block. */
export function statementBlock(id: string, words: string): string {
  return `:::requirement{#${id}}\n${words}\n:::`;
}

/** The slug of the spec `createIssue` made for an issue, by its artifact's id. */
async function specSlugOf(artifactId: string): Promise<string> {
  if (artifactId === "") throw new Error("the seeded issue holds no spec");
  return (await getArtifact(artifactId)).slug;
}

/**
 * A parent and its child, each with a spec of its own, in the project the issue-layout seeds
 * make: the parent's two statements, then the child's summary and its two statements, each in
 * words of its own. The child's Spec view draws its own two statements and none of its parent's
 * (R6; `useIssueTree.ts` reads the issue's own coverage). `e2e/issue-layout-seed.ts`'s seeds
 * give a parent no spec, so none of them can show that.
 */
export async function seedChildWithSpec(): Promise<ChildWithSpecSeed> {
  await createProject({ key: PROJECT, name: "Issue layout" });
  const parentRequirements = ["r-parent-1", "r-parent-2"] as const;
  const parent = await createIssue({
    force: true,
    project: PROJECT,
    spec: `${[
      statementBlock(parentRequirements[0], "The release ships on one day for every customer."),
      statementBlock(parentRequirements[1], "The release can be rolled back within ten minutes."),
    ].join("\n\n")}\n`,
    title: "The spring release ships as one",
  });
  const childRequirements = ["r-child-1", "r-child-2"] as const;
  const child = await createIssue({
    force: true,
    parent: parent.key,
    project: PROJECT,
    spec: `${[
      ':::callout{#summary title="What this issue is"}\n' +
        "The release notes ship with the release. They name every change a customer sees. " +
        "Each change links the pull request that made it.\n:::",
      statementBlock(childRequirements[0], "The notes list each change under the part it changed."),
      statementBlock(childRequirements[1], "The notes are published an hour before the release."),
    ].join("\n\n")}\n`,
    title: "The release notes ship with the spring release",
  });
  return {
    child: {
      key: child.key,
      requirements: childRequirements,
      specSlug: await specSlugOf(child.primary_artifact_id),
    },
    parent: {
      key: parent.key,
      requirements: parentRequirements,
      specSlug: await specSlugOf(parent.primary_artifact_id),
    },
  };
}

// ---------------------------------------------------------------------------------------------
// The readers.
// ---------------------------------------------------------------------------------------------

/** The issue's three sentences under "In short", wherever they are drawn. */
export function threeThings(page: Page): Locator {
  return page.getByTestId("issue-three-things");
}

/** The author named inside `scope`, where the page names one. */
export function rowAuthor(scope: Locator): Locator {
  return scope.locator("[data-node-author]");
}

/** The rows of the Contents list, wherever it is drawn: the pane, or the Outline sheet. */
export function contentsEntries(scope: Locator): Locator {
  return scope.locator("[data-contents-entry]");
}

/** The Focus branch toggle above the Contents list in `scope`. */
export function focusBranchControl(scope: Locator): Locator {
  return scope.getByRole("button", { exact: true, name: CONTROL_LABELS.focus_branch });
}

/**
 * What is wrong with the line a Contents entry draws of `text`, read from the browser's own
 * layout in the face the page draws in now: the line must fit the box the entry leaves it, and
 * must hold the next word of `text` too wherever that word and the ellipsis fit there (R1's
 * first-line rule, cut at a word to the entry's width). Empty where the line is the right one.
 */
export function contentsLineFaults(entry: Locator, text: string): Promise<string[]> {
  return entry.evaluate((element, text) => {
    const line = element.querySelector<HTMLElement>("[data-contents-line]");
    if (line === null) return ["the entry draws no line"];
    const shown = line.textContent ?? "";
    const words = shown.endsWith("…") ? shown.slice(0, -1) : shown;
    if (!text.startsWith(words)) {
      return [`the entry draws "${shown}", which "${text}" does not begin with`];
    }
    const room = line.getBoundingClientRect().width;
    const drawn = document.createRange();
    drawn.selectNodeContents(line);
    const width = drawn.getBoundingClientRect().width;
    const faults: string[] = [];
    if (width > room + 1) {
      faults.push(`"${shown}" is ${width.toFixed(1)} px, past its ${room.toFixed(1)} px box`);
    }
    const next = text.slice(words.length).trim().split(" ")[0] ?? "";
    if (next === "") return faults;
    // The line with its next word, laid out beside it in the same face and size.
    const probe = line.cloneNode(false) as HTMLElement;
    probe.textContent = `${words} ${next}…`;
    probe.style.cssText =
      "position: absolute; visibility: hidden; flex: none; width: auto; white-space: nowrap";
    line.after(probe);
    const longer = probe.getBoundingClientRect().width;
    probe.remove();
    if (longer <= room - 2) {
      faults.push(
        `"${shown}" leaves out "${next}", though "${words} ${next}…" is ${longer.toFixed(1)} px, inside its ${room.toFixed(1)} px box`
      );
    }
    return faults;
  }, text);
}

/** One Decided by cell of the Spec view: whose words it draws, the statements its bracket
 *  spans, and its drawn text. */
export interface DecidedByCell {
  readonly ids: readonly string[];
  readonly said: string | null;
  readonly text: string;
}

/** Each Decided by cell of the Spec view, in the order they are drawn. */
export function decidedByCells(page: Page): Promise<DecidedByCell[]> {
  return page.evaluate(() =>
    [...document.querySelectorAll("[data-spec-cell]")].map((cell) => ({
      ids: (cell.getAttribute("data-spec-statements") ?? "").split(" ").filter(Boolean),
      said: cell.getAttribute("data-said"),
      text: (cell.textContent ?? "").replace(/\s+/g, " ").trim(),
    }))
  );
}

/** The one line under each statement of the Spec view that names the decision that shaped it,
 *  by statement id; `""` for a statement that draws none. */
export function becauseLines(page: Page): Promise<Record<string, string>> {
  return page.evaluate(() =>
    Object.fromEntries(
      [...document.querySelectorAll("[data-spec-statement]")].map((row) => [
        row.getAttribute("data-node-id") ?? "",
        (row.querySelector("[data-spec-because]")?.textContent ?? "").replace(/\s+/g, " ").trim(),
      ])
    )
  );
}

/** Where a box of the page is, in the viewport's pixels. */
export interface SpecBox {
  readonly bottom: number;
  readonly left: number;
  readonly right: number;
  readonly top: number;
  readonly width: number;
}

/** One run of the Spec view, `[data-spec-run]`: the box of its decision cell and of its first
 *  statement, each null where the run draws none. */
export interface SpecRun {
  readonly cell: SpecBox | null;
  readonly first: SpecBox | null;
}

/** Each run of the Spec view, in the order they are drawn. */
export function specRuns(page: Page): Promise<SpecRun[]> {
  return page.locator("[data-issue-page] [data-spec-run]").evaluateAll((runs) =>
    runs.map((run) => {
      const box = (element: Element | null) => {
        if (element === null) return null;
        const { bottom, left, right, top, width } = element.getBoundingClientRect();
        return { bottom, left, right, top, width };
      };
      return {
        cell: box(run.querySelector("[data-spec-cell]")),
        first: box(run.querySelector("[data-spec-statement]")),
      };
    })
  );
}

/** The text of each `[data-page-chrome]` element inside `scope`, which is the page's own words,
 *  with what a nested `[data-author-text]` holds left out: a person's or an agent's words inside
 *  the page's own, such as the count in "more · 180 words", are theirs (R7). */
export function ownWords(scope: Locator): Promise<string[]> {
  return scope.locator("[data-page-chrome]").evaluateAll((elements) =>
    elements.map((element) => {
      const own = element.cloneNode(true) as Element;
      for (const theirs of own.querySelectorAll("[data-author-text]")) theirs.replaceWith(" ");
      return own.textContent ?? "";
    })
  );
}
