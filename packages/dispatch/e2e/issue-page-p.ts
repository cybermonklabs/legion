import { expect, type Locator, type Page, test } from "@playwright/test";

import type { IssueDecisions } from "../web/src/api/types";
import { ACTION_WORDS } from "../web/src/features/issue-page/approval-chip-box";
import { CONTROL_LABELS } from "../web/src/features/issue-page/page-words";
import {
  baseUrl,
  createAsk,
  createIssue,
  createIssueArtifact,
  createProject,
  getIssue,
  patchIssue,
  userHeaders,
} from "./api";
import { filterRow } from "./issue-page";
import { assertWhole } from "./unclipped";

declare global {
  interface Window {
    /** `holdClipboard`'s clipboard: how many writes were made, and the resolve of each write
     *  still held. */
    __dispatchHeldClipboard?: { held: (() => void)[]; writes: number };
  }
}

/**
 * What the header's rows read: the header, its properties line, its readings against the
 * header's height limits and the typeface, and the one issue they read it on. The rows open the
 * page with `phase` and `gotoIssue` (`e2e/issue-page.ts`) and measure the header to the view
 * row's `filterRow`.
 *
 * This is the header rows' own file: no other part's rows read it.
 *
 * - **The header** `header[data-issue-header]` inside `[data-issue-page]`, drawing no box of its
 *   own (`display: contents`), and in it the band that stays from 761 px up,
 *   `[data-identity-block]`, with `data-condensed` on the header once the page has scrolled more
 *   than 40 px (`IssueHeader.tsx`).
 * - **The properties line** `[data-testid="issue-state-actions"]`: the status
 *   (`select[aria-label="Status"]`), the priority and the assignee (each a `select` named
 *   `Priority of <key>` and `Assignee of <key>`, drawn over an `aria-hidden` badge), the
 *   children's progress `[data-testid="issue-progress-children"]` and
 *   `[data-testid="issue-progress-subtree"]`, the count `[data-testid="issue-whose-turn"]`, the
 *   approval control, the route's marker `[data-testid="issue-route-unreachable"]`, the labels
 *   `[data-testid="issue-header-labels"] > [data-header-label]` and their count
 *   `[data-testid="issue-labels-left"]` (`IssuePropertiesRow.tsx`).
 */

/** The desktop the rows read at 1440 px. */
export const DESKTOP = { height: 900, width: 1440 } as const;

/** The iPhone 13's viewport, the suite's phone (`e2e/playwright.config.ts`'s `iphone` project),
 *  which is the screen the header's limit at 390 px is for. */
export const IPHONE_13 = { height: 664, width: 390 } as const;

/** The viewport a row reads `width` in: the iPhone 13's at 390 px, else a phone's height below
 *  1024 px and the desktop's from there. */
export function viewportAt(width: number): { readonly height: number; readonly width: number } {
  if (width === IPHONE_13.width) return IPHONE_13;
  return { height: width < 1024 ? 844 : DESKTOP.height, width };
}

/** A real issue's title, 115 characters: the one the header's widths were read on. */
export const MEASURED_TITLE =
  "Dispatch's issue page is redesigned from design research: an ordered body and a header that fits, with nothing lost";

/** Twenty labels, the most Dispatch lets an issue hold (`maxIssueLabels`,
 *  `packages/envoy/internal/dispatch/api/issue_queries.go`): more than the header's line holds
 *  at 1440 px and at 390 px, so it ends with the count of those left over. */
export const TWENTY_LABELS = [
  "dispatch",
  "layout",
  "redesign",
  "header",
  "details",
  "keyboard",
  "phone",
  "typeface",
  "accessibility",
  "approval",
  "outline",
  "presets",
  "contents",
  "composer",
  "decisions",
  "verification",
  "plan",
  "children",
  "routing",
  "web",
] as const;

/** The project the seed writes in. */
export const HEADER_PROJECT = "HDR";

/** The agent session that asks the seed's questions: a question it asks on an issue alice holds
 *  waits on alice (`waiting_on_person`). */
export const asAgent = {
  actor: { id: "issue-header-agent", kind: "session" as const },
  as: "agent" as const,
};

/** What `seedHeader` wrote. */
export interface HeaderSeed {
  readonly childKeys: readonly string[];
  readonly key: string;
  readonly parentKey: string;
  readonly specArtifactId: string;
}

/**
 * One issue holding every part of the header's list, written through `e2e/api.ts`: a parent in
 * progress whose route reaches nobody, so the parent draws the header's one route marker; the
 * issue under it in progress, at P1, assigned to alice, with a spec and a Verification document,
 * three children (one done, one in progress, one to do) and two questions an agent asked, which
 * wait on alice. No approval request is open: a row that needs one asks for it.
 *
 * The parent is in work, so the chain keeps Dispatch's parent-status rule (R24): a pick of the
 * issue's status into work is one the rule asks nothing of, and the page sends its PATCH after the
 * dry run, which Dispatch answers as the rule is on whatever its setting. A close is not: a row
 * that closes the issue closes its open children first (`closeOpenChildren`).
 */
export async function seedHeader(): Promise<HeaderSeed> {
  await createProject({ key: HEADER_PROJECT, name: "Header" });
  const parent = await createIssue({
    force: true,
    project: HEADER_PROJECT,
    spec: "The parent's plan.",
    title: "The parent of the header's issue",
  });
  await patchIssue(parent.key, { route: "role:nobody-holds-this", status: "in_progress" });
  const issue = await createIssue({
    force: true,
    parent: parent.key,
    project: HEADER_PROJECT,
    spec: ":::requirement{#r1}\nThe header fits.\n:::\n",
    title: "The header's issue",
  });
  await patchIssue(issue.key, { assignee: "alice", priority: 1, status: "in_progress" });
  await createIssueArtifact(issue.key, {
    content: `:::check{#c1 serves="dispatch://${issue.key}/spec#b-r1" surface="browser" run="bun run e2e -- e2e/issue-page-p.e2e.ts"}\nThe header fits.\n:::\n`,
    name: "verification.md",
  });
  const childKeys: string[] = [];
  for (const [title, status] of [
    ["The child that is done", "done"],
    ["The child in progress", "in_progress"],
    ["The child to do", "todo"],
  ] as const) {
    const child = await createIssue({
      force: true,
      parent: issue.key,
      project: HEADER_PROJECT,
      title,
    });
    await patchIssue(child.key, { status });
    childKeys.push(child.key);
  }
  await createAsk(issue.key, { question: "Which width does it read first?" }, asAgent);
  await createAsk(issue.key, { question: "And which typeface?" }, asAgent);
  return {
    childKeys,
    key: issue.key,
    parentKey: parent.key,
    specArtifactId: issue.primary_artifact_id ?? "",
  };
}

/** Closes each open child of the seeded issue through the API, so that a close of the issue keeps
 *  Dispatch's parent-status rule (a parent is done only when each child is done) and the page
 *  writes it with no question. A child already done is left as it is: Dispatch refuses a write of
 *  a closed issue's status (`ISSUE_CLOSED`). A row about what a close does to the page, not about
 *  the rule's question, calls it before it closes the issue. */
export async function closeOpenChildren(seeded: HeaderSeed): Promise<void> {
  for (const child of seeded.childKeys) {
    if ((await getIssue(child)).status !== "done") await patchIssue(child, { status: "done" });
  }
}

/** The new page's header. */
export function issueHeader(page: Page): Locator {
  return page.locator("[data-issue-page] header[data-issue-header]");
}

/** The part of the header that stays at the top of the screen from 761 px up: the key's row
 *  and the title. */
export function identityBand(page: Page): Locator {
  return issueHeader(page).locator("[data-identity-block]");
}

/** The header's one properties line. */
export function propertiesLine(page: Page): Locator {
  return issueHeader(page).getByTestId("issue-state-actions");
}

/** Waits for the header's properties line: every row's first step, so a page that draws only
 *  the key and the title fails on this and not on a later reading. */
export async function expectHeaderDrawn(page: Page): Promise<void> {
  await expect(propertiesLine(page), "the new page draws no properties line").toBeVisible();
}

/** The labels the header's line draws, in order: the chips that fit whole. */
export function headerLabels(page: Page): Locator {
  return issueHeader(page).locator('[data-testid="issue-header-labels"] > [data-header-label]');
}

/** The count that ends the line with the labels left over, which opens Details at Labels. */
export function labelsLeft(page: Page): Locator {
  return issueHeader(page).getByTestId("issue-labels-left");
}

/** How many times `scope` draws `words`, read in its drawn text (`innerText`) with each run of
 *  white space one space, and counted only where the words stand whole, so a key such as
 *  `HDR-2` is not found inside `HDR-20`. A name a control carries for assistive technology is
 *  not drawn text, so the copy control's name, which holds the key, is not counted. */
export function timesDrawn(scope: Locator, words: string): Promise<number> {
  return scope.evaluate((element, words) => {
    const text = (element as HTMLElement).innerText.replace(/\s+/g, " ");
    const escaped = words.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
    const whole = new RegExp(`(?<![\\p{L}\\p{N}-])${escaped}(?![\\p{L}\\p{N}])`, "gu");
    return [...text.matchAll(whole)].length;
  }, words);
}

/** The name a reader hears of the first control row one of the header draws - its
 *  `aria-label`, else its words - or null where it draws none. Row one, the key's
 *  (`IssueIdentityRow.tsx`), is the band's first row on an issue that is not closed. */
export function firstControlOfRowOne(page: Page): Promise<string | null> {
  return identityBand(page).evaluate((band) => {
    const row = band.firstElementChild;
    const control = [...(row?.querySelectorAll("a, button, select, input, textarea") ?? [])].find(
      (one) => one.checkVisibility()
    );
    if (control === undefined) return null;
    return control.getAttribute("aria-label") ?? (control.textContent ?? "").trim();
  });
}

/** A part of the properties line `lineOrder` reads: the elements `selector` matches, and of
 *  those, where `words` is given, the ones whose words are exactly `words`. */
export interface LinePart {
  readonly name: string;
  readonly selector: string;
  readonly words?: string;
}

/** The parts of the properties line `parts` names, by `name`, in the line's document order: a
 *  run of one part's elements is named once, and a part the line does not draw is not named. */
export function lineOrder(page: Page, parts: readonly LinePart[]): Promise<string[]> {
  return propertiesLine(page).evaluate((line, parts) => {
    const drawn = parts.flatMap(({ name, selector, words }) =>
      [...line.querySelectorAll(selector)]
        .filter((element) => words === undefined || (element.textContent ?? "").trim() === words)
        .map((element) => ({ element, name }))
    );
    drawn.sort((a, b) =>
      a.element.compareDocumentPosition(b.element) & Node.DOCUMENT_POSITION_FOLLOWING ? -1 : 1
    );
    return drawn.map(({ name }) => name).filter((name, at, names) => name !== names[at - 1]);
  }, parts);
}

/** The longest label Dispatch takes, 40 UTF-16 units (`maxIssueLabel16`,
 *  `packages/envoy/internal/dispatch/api/issue_queries.go`): wider than what the header's line
 *  at 1440 px leaves beside the labels it draws of the twenty, count or no count, so drawn after
 *  them it is the one label left out. */
export const LONGEST_LABEL = "the-longest-label-the-header-never-holds";

/**
 * A label whose chip just fits the room the properties line leaves after its last label chip,
 * read on a line that draws no count: at least `under` px narrower than that room, and less than
 * one letter more than that, so drawn after those chips it fits while no count is drawn, and not
 * once a count and its gap take their room out of the line. A copy of the last chip, laid out
 * unseen, takes the label's words one letter more at a time, and the label is the longest that
 * still fits; a room that holds all of its words, or not one letter, throws.
 */
export async function labelFillingTheRoom(page: Page, under: number): Promise<string> {
  // The one caller waits for the labels drawn and no count first, so the line has settled; an
  // error only throws.
  const filled = await propertiesLine(page).evaluate((line, under) => {
    const box = line.querySelector('[data-testid="issue-header-labels"]');
    const last = [...(box?.querySelectorAll<HTMLElement>("[data-header-label]") ?? [])].at(-1);
    if (box === null || last === undefined) return { error: "the line draws no label", label: "" };
    if (line.querySelector('[data-testid="issue-labels-left"]') !== null) {
      return { error: "the line draws a count", label: "" };
    }
    const room =
      box.getBoundingClientRect().right -
      last.getBoundingClientRect().right -
      Number.parseFloat(getComputedStyle(box).columnGap) -
      under;
    const probe = last.cloneNode() as HTMLElement;
    probe.removeAttribute("data-header-label");
    probe.style.position = "absolute";
    probe.style.visibility = "hidden";
    line.append(probe);
    const words = "the-label-that-fits-only-with-no-count";
    let label = "";
    for (const letter of words) {
      probe.textContent = label + letter;
      if (probe.getBoundingClientRect().width > room) break;
      label += letter;
    }
    probe.remove();
    if (label === "") return { error: `${room.toFixed(1)} px holds no label`, label };
    if (label === words) return { error: `${room.toFixed(1)} px holds all of "${words}"`, label };
    return { error: null, label };
  }, under);
  if (filled.error !== null) throw new Error(`no label fills the line's room: ${filled.error}`);
  return filled.label;
}

/** The count of what waits on the reader, or whose turn it is when nothing waits. */
export function waitingCount(page: Page): Locator {
  return issueHeader(page).getByTestId("issue-whose-turn");
}

/** The issue's decisions as the server reads them for alice, each with `waiting_review`
 *  (`GET /api/v1/issues/{key}/decisions`). `e2e/api.ts` wraps no read of the route, so this is a
 *  plain fetch with her sign-in, as `e2e/bare-references.e2e.ts` reads an issue. */
export async function issueDecisions(key: string): Promise<IssueDecisions> {
  const path = `/api/v1/issues/${encodeURIComponent(key)}/decisions`;
  const response = await fetch(new URL(path, baseUrl), { headers: await userHeaders("alice") });
  if (!response.ok) {
    throw new Error(`GET ${path} failed: ${response.status} ${await response.text()}`);
  }
  return (await response.json()) as IssueDecisions;
}

/** Details, the drawer over the right of the page. */
export function details(page: Page): Locator {
  return page.locator("aside[data-details]");
}

/** How far below the bottom of the header's actions control its open menu starts, in px:
 *  negative where the menu covers the control, where a second press on the control lands on the
 *  menu's first action instead of closing it. The menu is the element the control names in
 *  `aria-controls`. */
export async function actionsMenuDrop(page: Page): Promise<number> {
  // The one caller waits for the control's `aria-expanded` to read true first, so the menu is
  // open when this reads it; a miss only throws.
  const drop = await issueHeader(page)
    .getByRole("button", { exact: true, name: CONTROL_LABELS.issue_actions })
    .evaluate((control) => {
      const menu = document.getElementById(control.getAttribute("aria-controls") ?? "");
      if (menu === null || !menu.checkVisibility()) return null;
      return menu.getBoundingClientRect().top - control.getBoundingClientRect().bottom;
    });
  if (drop === null) throw new Error("the actions control names no open menu");
  return drop;
}

/** A `"` inside an attribute value would end the selector's string. */
function cssValue(value: string): string {
  expect(value, `${value} cannot be written into a selector`).not.toContain('"');
  return value;
}

/** The value a badge-and-select control shows a reader: the badge drawn under the `select`
 *  named `label` (`web/src/features/issue/badge-select.ts`). The badge is hidden from assistive
 *  technology, because the select carries the name. */
export function badgeValue(page: Page, label: string): Locator {
  return issueHeader(page).locator(
    `:has(> select[aria-label="${cssValue(label)}"]) > [aria-hidden="true"]`
  );
}

/** The header's approval chip of the document `artifactId`: the issue's own chip, or the chip of
 *  a gated root's entry. */
export function approvalChip(page: Page, artifactId: string): Locator {
  return issueHeader(page).locator(`[data-approval-artifact="${cssValue(artifactId)}"]`);
}

/** The entry of a gated root's step `step` among the header's approval steps: the approval of the
 *  document that step approves - its chip, or `Not requested` - and, while that document's
 *  request waits on the reader, its actions. */
export function approvalEntry(page: Page, step: string): Locator {
  return issueHeader(page)
    .getByTestId("issue-approval-steps")
    .locator(`[data-approval-step="${cssValue(step)}"]`);
}

/** The reads that redraw the header's approval control after a review of the issue's document,
 *  held from `hold` until `release`. */
export interface HeldReviewReads {
  /** Holds every read made from now until `release`. */
  hold(): void;
  /** How many reads of the issue itself have been held. */
  issueReadsHeld(): number;
  /** Lets every held read on, and holds no read made after. */
  release(): void;
}

/**
 * Routes the page's reads that follow a review of the document `artifactId` of the issue `key`,
 * so that a row can press a control after the review has landed and before the page has drawn
 * what it reads back: each `GET` of the issue itself, which the header draws the approval from
 * (`useIssueDetail`'s `primaryArtifact`), of the issue's documents and of its asks, and of the
 * document and every address under it, its reviews among them - the reads `ApprovalChip`'s
 * `onSuccess` refreshes. Each read waits in the route's handler, and goes on once released. A
 * write is never held, and neither is a read made before `hold` or after `release`.
 */
export async function holdReviewReads(
  page: Page,
  key: string,
  artifactId: string
): Promise<HeldReviewReads> {
  const issue = `/api/v1/issues/${key}`;
  const artifact = `/api/v1/artifacts/${artifactId}`;
  const released = Promise.withResolvers<void>();
  let holding = false;
  let issueReads = 0;
  await page.route(
    ({ pathname }) =>
      pathname === issue ||
      pathname === `${issue}/artifacts` ||
      pathname === `${issue}/asks` ||
      pathname === artifact ||
      pathname.startsWith(`${artifact}/`),
    async (route) => {
      if (route.request().method() !== "GET" || !holding) return route.fallback();
      if (new URL(route.request().url()).pathname === issue) issueReads += 1;
      await released.promise;
      return route.fallback();
    }
  );
  return {
    hold: () => {
      holding = true;
    },
    issueReadsHeld: () => issueReads,
    release: () => {
      holding = false;
      released.resolve();
    },
  };
}

/**
 * Every element the header draws that a reader reads or presses, by what it is: its test id, or
 * its tag with the name a reader hears; and every run of words drawn outside those, as
 * `text <its words>`. Words count as drawn outside them where an element holds a text of its own
 * and no part that holds no other part - a control, the title, a test id around words alone -
 * holds that element: the project, the key and a pill are text in a span, and a property added
 * that way is named too. A part that holds others, as the properties line does, names nothing
 * of the words in it. Text it lays out unseen is left out.
 */
export function headerParts(page: Page): Promise<string[]> {
  return issueHeader(page).evaluate((header) => {
    const all = [...header.querySelectorAll("a, button, select, h1, textarea, [data-testid]")];
    const leaves = all.filter(
      (part) => !all.some((other) => other !== part && part.contains(other))
    );
    const parts = all
      .filter((part) => part.checkVisibility())
      .map(
        (part) =>
          part.getAttribute("data-testid") ??
          `${part.tagName.toLowerCase()} ${part.getAttribute("aria-label") ?? (part.textContent ?? "").replace(/\s+/g, " ").trim()}`
      );
    const texts = [...header.querySelectorAll("*")].filter(
      (element) =>
        [...element.childNodes].some(
          (node) => node.nodeType === Node.TEXT_NODE && (node.textContent ?? "").trim() !== ""
        ) &&
        element.checkVisibility() &&
        !leaves.some((leaf) => leaf.contains(element))
    );
    // A run of words is named once, by its outermost element, so words drawn around a span of
    // their own - a number in a sentence - are one run and not two.
    const words = texts
      .filter((element) => !texts.some((other) => other !== element && other.contains(element)))
      .map((element) => `text ${(element.textContent ?? "").replace(/\s+/g, " ").trim()}`);
    return [...parts, ...words];
  });
}

/**
 * The header's controls that fail WCAG 2.2's target size (2.5.8) as written: each target at
 * least 24 by 24 CSS px, or, where one is smaller, a 24 px circle centred on it that overlaps no
 * other target.
 */
export function undersizedTargets(page: Page): Promise<string[]> {
  return issueHeader(page).evaluate((header) => {
    const targets = [...header.querySelectorAll<HTMLElement>("a, button, select")]
      .filter((target) => target.checkVisibility())
      .map((target) => ({
        box: target.getBoundingClientRect(),
        name: target.getAttribute("aria-label") ?? (target.textContent ?? "").trim(),
      }));
    const overlaps = (centre: { x: number; y: number }, box: DOMRect) => {
      const x = Math.max(box.left, Math.min(centre.x, box.right));
      const y = Math.max(box.top, Math.min(centre.y, box.bottom));
      return (x - centre.x) ** 2 + (y - centre.y) ** 2 < 12 ** 2;
    };
    return targets
      .filter(({ box }, index) => {
        if (box.width >= 24 - 0.5 && box.height >= 24 - 0.5) return false;
        const centre = { x: box.left + box.width / 2, y: box.top + box.height / 2 };
        return targets.some((other, at) => at !== index && overlaps(centre, other.box));
      })
      .map(({ box, name }) => `${name}: ${box.width.toFixed(1)} x ${box.height.toFixed(1)}`);
  });
}

/** The controls the header holds to 24 px tall at every width: the status, the priority, the
 *  assignee, the count, Approve (`Approve v3` on a stale approval), Request changes, the labels'
 *  count and the control that copies the key, 24 by 24 px as row one's other controls are, each
 *  as `<name>: <height>` where it is drawn shorter. The two actions are the buttons whose words
 *  are an action's (`ACTION_WORDS`), the patterns the header itself finds them by. */
export function shortControls(page: Page, key: string): Promise<string[]> {
  const actionWords = Object.values(ACTION_WORDS).map((pattern) => pattern.source);
  return issueHeader(page).evaluate(
    (header, { actionWords, key }) => {
      const actions = actionWords.map((source) => new RegExp(source));
      const named: [string, Element | null][] = [
        ["the status", header.querySelector('select[aria-label="Status"]')],
        ["the priority", header.querySelector(`select[aria-label="Priority of ${key}"]`)],
        ["the assignee", header.querySelector(`select[aria-label="Assignee of ${key}"]`)],
        ["the count", header.querySelector('button[data-testid="issue-whose-turn"]')],
        ["the labels' count", header.querySelector('[data-testid="issue-labels-left"]')],
        ["Copy issue key", header.querySelector('button[aria-label^="Copy issue key "]')],
        ...[...header.querySelectorAll("button")]
          .map((button): [string, Element] => [(button.textContent ?? "").trim(), button])
          .filter(([words]) => actions.some((action) => action.test(words))),
      ];
      return named
        .filter(([, control]) => control !== null)
        .map(([name, control]) => [name, control?.getBoundingClientRect().height ?? 0] as const)
        .filter(([, height]) => height < 24 - 0.5)
        .map(([name, height]) => `${name}: ${height.toFixed(1)} px`);
    },
    { actionWords, key }
  );
}

/** The header as its height limit measures it: from the top of the header to the top of the
 *  view row. */
interface HeaderReading {
  readonly height: number;
  /** The words of each line drawn between the two, top to bottom. */
  readonly lines: readonly string[];
}

/**
 * The header's band and the lines drawn in it. A line is a run of drawn text: every text node of
 * the page inside the band and inside the header's own column, one box per line the text wraps
 * to, and a form control's value as a box of its own font size at the control's middle, since a
 * select draws its value without a text node. Boxes that overlap vertically are one line. Text a
 * box of at most a pixel clips away, or that is invisible or transparent, is not drawn. Null when
 * the page draws no header or no view row.
 */
function headerReading(page: Page): Promise<HeaderReading | null> {
  return page.evaluate(() => {
    const root = document.querySelector("[data-issue-page]");
    const header = root?.querySelector("header[data-issue-header]") ?? null;
    const row = root?.querySelector("[data-filter-row]") ?? null;
    if (root === null || root === undefined || header === null || row === null) return null;
    // The `<header>` draws no box of its own (`display: contents`), so its column is the box
    // around what it holds: the band, the properties line and what a write answers.
    const parts = [...header.children].map((part) => part.getBoundingClientRect());
    const column = {
      left: Math.min(...parts.map((part) => part.left)),
      right: Math.max(...parts.map((part) => part.right)),
      top: Math.min(...parts.filter((part) => part.height > 0).map((part) => part.top)),
    };
    const top = column.top;
    const bottom = row.getBoundingClientRect().top;
    const inBand = (box: DOMRect): boolean =>
      box.width > 0 &&
      box.height > 0 &&
      box.bottom > top &&
      box.top < bottom &&
      box.right > column.left &&
      box.left < column.right;
    const drawn = (element: Element): boolean => {
      for (let at: Element | null = element; at !== null; at = at.parentElement) {
        const style = getComputedStyle(at);
        if (style.visibility !== "visible" || style.opacity === "0") return false;
        const box = at.getBoundingClientRect();
        if (style.overflow !== "visible" && (box.width <= 1 || box.height <= 1)) return false;
      }
      return true;
    };
    const pieces: { bottom: number; left: number; text: string; top: number }[] = [];
    const walker = document.createTreeWalker(root, NodeFilter.SHOW_TEXT);
    for (let node = walker.nextNode(); node !== null; node = walker.nextNode()) {
      const content = node.textContent ?? "";
      const element = node.parentElement;
      if (content.trim() === "" || element === null || element.closest("select") !== null) continue;
      const range = document.createRange();
      range.selectNodeContents(node);
      const boxes = [...range.getClientRects()].filter(inBand);
      if (boxes.length === 0 || !drawn(element)) continue;
      // A text that wraps is one box per line; each line is given the characters drawn in it,
      // so a failure names the words of each line rather than the whole text on each.
      const words = boxes.length === 1 ? [content] : boxes.map(() => "");
      for (let at = 0; boxes.length > 1 && at < content.length; at += 1) {
        range.setStart(node, at);
        range.setEnd(node, at + 1);
        const glyph = range.getClientRects()[0];
        if (glyph === undefined) continue;
        const line = boxes.findIndex(
          (box) => glyph.top >= box.top - 1 && glyph.bottom <= box.bottom + 1
        );
        if (line >= 0) words[line] += content[at];
      }
      boxes.forEach((box, line) => {
        const text = (words[line] ?? "").replace(/\s+/g, " ").trim();
        pieces.push({ bottom: box.bottom, left: box.left, text, top: box.top });
      });
    }
    for (const control of root.querySelectorAll("select, input:not([type='hidden']), textarea")) {
      const box = control.getBoundingClientRect();
      if (!inBand(box) || !drawn(control)) continue;
      const size = Number.parseFloat(getComputedStyle(control).fontSize);
      const middle = (box.top + box.bottom) / 2;
      const text =
        control instanceof HTMLSelectElement
          ? (control.selectedOptions[0]?.textContent ?? "").trim()
          : (control as HTMLInputElement).value;
      pieces.push({ bottom: middle + size / 2, left: box.left, text, top: middle - size / 2 });
    }
    pieces.sort((a, b) => a.top - b.top);
    const lines: (typeof pieces)[] = [];
    let lineBottom = Number.NEGATIVE_INFINITY;
    for (const piece of pieces) {
      const line = lines.at(-1);
      if (line !== undefined && piece.top < lineBottom - 1) {
        line.push(piece);
        lineBottom = Math.max(lineBottom, piece.bottom);
      } else {
        lines.push([piece]);
        lineBottom = piece.bottom;
      }
    }
    return {
      height: bottom - top,
      lines: lines.map((line) =>
        line
          .sort((a, b) => a.left - b.left)
          .map((piece) => piece.text)
          .join(" ")
      ),
    };
  });
}

/**
 * The header's height limit at `width`: at 1440 px 3 lines and 130 px, and 4 lines and 130 px
 * where the issue's project gates two or three steps, since the header then draws one approval
 * entry for each (R3, decision 213); at 390 px, on the 20-label seed and the measured title,
 * 8 lines and 250 px while no approval request waits on the reader and 9 lines and 280 px while
 * one does. No other width has a limit.
 */
function headerLimit(
  width: number,
  requestWaiting: boolean,
  gatedSteps: number
): { readonly height: number; readonly lines: number } | undefined {
  if (width === DESKTOP.width) return { height: 130, lines: gatedSteps >= 2 ? 4 : 3 };
  if (width !== IPHONE_13.width) return undefined;
  return requestWaiting ? { height: 280, lines: 9 } : { height: 250, lines: 8 };
}

/**
 * The header read against its limit at `width`, softly so one run reports every width and seed,
 * with the page at its top; at 390 px the title is drawn whole. `drawnIn` names the typeface the
 * page is drawn in, for the message, and `gatedSteps` how many steps the issue's project gates
 * (one, or none, unless a caller says otherwise). The header is read alone: no row of the
 * outline is waited for.
 */
export async function expectHeaderWithinItsLimit(
  page: Page,
  width: number,
  requestWaiting: boolean,
  drawnIn: string,
  gatedSteps = 1
): Promise<void> {
  const limit = headerLimit(width, requestWaiting, gatedSteps);
  if (limit === undefined) return;
  const gated = gatedSteps >= 2 ? ` on a project that gates ${gatedSteps} steps` : "";
  const where = `at ${width} px in ${drawnIn} with ${requestWaiting ? "a request" : "no request"} waiting${gated}`;
  await expect(filterRow(page), `the page draws no view row ${where}`).toBeVisible();
  await page.evaluate(() => window.scrollTo(0, 0));
  // The view row was waited visible just above, and every caller waits for the header drawn
  // first; null only throws.
  const reading = await headerReading(page);
  if (reading === null) throw new Error(`the page draws no header or no view row ${where}`);
  const drawnAs = `${reading.lines.length} lines, ${reading.height.toFixed(1)} px`;
  test.info().annotations.push({ description: `${where}: ${drawnAs}`, type: "header" });
  expect
    .soft(
      reading.lines.length,
      `the header takes ${drawnAs} ${where}: ${JSON.stringify(reading.lines)}`
    )
    .toBeLessThanOrEqual(limit.lines);
  expect
    .soft(reading.height, `the header takes ${drawnAs} ${where}`)
    .toBeLessThanOrEqual(limit.height);
  if (width === IPHONE_13.width) {
    await assertWhole(issueHeader(page).getByRole("heading", { level: 1 }));
  }
}

/** What stays at the top of the screen and what scrolls away, where the page stands now. */
export interface StickyReading {
  /** The band - the key's row and the title - by its box on the screen. */
  readonly block: { readonly bottom: number; readonly height: number; readonly top: number };
  /** Whether the header says it is stuck (`data-condensed`), which draws its shadow. */
  readonly condensed: boolean;
  /** The band's `box-shadow`, as computed. */
  readonly shadow: string;
  /** The properties line, by its box on the screen. */
  readonly properties: { readonly bottom: number; readonly top: number };
  /** The height the page publishes for whatever stays (`--issue-identity-height`), in px. */
  readonly published: number;
  /** How far the page has scrolled. */
  readonly scrolled: number;
  /** The title's box and its type, as computed. */
  readonly title: {
    readonly bottom: number;
    readonly clamp: string;
    readonly line: string;
    readonly size: string;
    readonly spacing: string;
    readonly top: number;
    readonly weight: string;
    readonly wordsWidth: number;
  };
}

/** `StickyReading` of the page, or null where it draws no header. */
export function stickyReading(page: Page): Promise<StickyReading | null> {
  return page.evaluate(() => {
    const header = document.querySelector("[data-issue-page] header[data-issue-header]");
    const block = header?.querySelector("[data-identity-block]") ?? null;
    const title = header?.querySelector("h1") ?? null;
    const properties = header?.querySelector('[data-testid="issue-state-actions"]') ?? null;
    if (header === null || header === undefined || block === null || title === null) return null;
    if (properties === null) return null;
    const box = (element: Element) => {
      const rect = element.getBoundingClientRect();
      return { bottom: rect.bottom, height: rect.height, top: rect.top };
    };
    const type = getComputedStyle(title);
    // The box the title's words wrap in: the heading less its padding and its rule.
    const wordsWidth =
      title.getBoundingClientRect().width -
      Number.parseFloat(type.paddingLeft) -
      Number.parseFloat(type.paddingRight) -
      Number.parseFloat(type.borderLeftWidth) -
      Number.parseFloat(type.borderRightWidth);
    return {
      block: box(block),
      condensed: header.getAttribute("data-condensed") === "true",
      properties: box(properties),
      published: Number.parseFloat(
        getComputedStyle(document.documentElement).getPropertyValue("--issue-identity-height")
      ),
      scrolled: window.scrollY,
      shadow: getComputedStyle(block).boxShadow,
      title: {
        ...box(title),
        clamp: type.webkitLineClamp,
        line: type.lineHeight,
        size: type.fontSize,
        spacing: type.letterSpacing,
        weight: type.fontWeight,
        wordsWidth,
      },
    };
  });
}

/** The family `web/src/styles.css` declares for the typeface the new page ships and draws in. */
export const TYPEFACE = "Dispatch Inter";

/** The address the page's stylesheet loads the typeface from, read from its own `@font-face`
 *  rule. Throws when no rule declares the family. */
export async function typefaceAddress(page: Page): Promise<string> {
  // Callers wait for the header drawn first, and the page's script runs only once its stylesheet
  // has loaded, so the rule is there; a miss only throws.
  const address = await page.evaluate((family) => {
    const find = (rules: CSSRuleList, base: string): string | null => {
      for (const rule of rules) {
        if (rule instanceof CSSFontFaceRule) {
          const named = rule.style.getPropertyValue("font-family").replace(/["']/g, "").trim();
          const source = /url\(["']?([^"')]+)["']?\)/.exec(rule.style.getPropertyValue("src"));
          if (named === family && source?.[1] !== undefined) return new URL(source[1], base).href;
        } else if (rule instanceof CSSGroupingRule) {
          const found = find(rule.cssRules, base);
          if (found !== null) return found;
        }
      }
      return null;
    };
    for (const sheet of document.styleSheets) {
      const found = find(sheet.cssRules, sheet.href ?? document.baseURI);
      if (found !== null) return found;
    }
    return null;
  }, TYPEFACE);
  if (address === null) {
    throw new Error(`the page's stylesheet declares no @font-face for ${TYPEFACE}`);
  }
  return address;
}

/** How each face of the typeface's family stands in the document's font set: `loaded` once the
 *  file arrived, `error` once its request failed. */
export function typefaceStatuses(page: Page): Promise<string[]> {
  return page.evaluate(
    (family) =>
      [...document.fonts]
        .filter((face) => face.family.replace(/["']/g, "") === family)
        .map((face) => face.status),
    TYPEFACE
  );
}

/** The stack the header's band is drawn in, as its computed `font-family`, each family
 *  unquoted. */
export function headerFamilies(page: Page): Promise<string[]> {
  return identityBand(page).evaluate((band) =>
    getComputedStyle(band)
      .fontFamily.split(",")
      .map((family) => family.replace(/["']/g, "").trim())
  );
}

/** The header's labels against their count, read off the drawn page: how many chips are drawn
 *  wholly inside the labels' box, each chip the box's edge cuts (`<label>: <its box> in <the
 *  labels' box>`), and the number the count names, 0 where no count is drawn. A chip wholly
 *  outside the box is not drawn. The count is a count of the rest where the chips drawn whole and
 *  its number make every label, and no chip is cut. */
export interface LabelsAgainstTheCount {
  readonly cut: readonly string[];
  readonly left: number;
  readonly whole: number;
}

/** Reads `LabelsAgainstTheCount` off the header's properties line `line`, inside the page.
 *  Playwright sends the page a page function's own source and nothing it closes over, so this
 *  closes over nothing, and a page function builds it there from `LABELS_READING`. */
function readLabelsAgainstTheCount(line: Element): LabelsAgainstTheCount {
  const box = line.querySelector('[data-testid="issue-header-labels"]');
  if (box === null) throw new Error("the header draws no labels' box");
  const outer = box.getBoundingClientRect();
  const edges = (rect: DOMRect) =>
    `${rect.left.toFixed(1)}-${rect.right.toFixed(1)} x ${rect.top.toFixed(1)}-${rect.bottom.toFixed(1)}`;
  const cut: string[] = [];
  let whole = 0;
  for (const chip of box.querySelectorAll("[data-header-label]")) {
    const rect = chip.getBoundingClientRect();
    const inside =
      rect.left >= outer.left - 0.5 &&
      rect.right <= outer.right + 0.5 &&
      rect.top >= outer.top - 0.5 &&
      rect.bottom <= outer.bottom + 0.5;
    const outside =
      rect.right <= outer.left + 0.5 ||
      rect.left >= outer.right - 0.5 ||
      rect.bottom <= outer.top + 0.5 ||
      rect.top >= outer.bottom - 0.5;
    if (inside) whole += 1;
    else if (!outside)
      cut.push(`${(chip.textContent ?? "").trim()}: ${edges(rect)} in ${edges(outer)}`);
  }
  const count = line.querySelector('[data-testid="issue-labels-left"]');
  return { cut, left: Number(/\d+/.exec(count?.textContent ?? "")?.[0] ?? "0"), whole };
}

/** The source of `readLabelsAgainstTheCount`, which a page function turns back into the reading
 *  with `new Function`, as `labelsAgainstTheCount` does. A row whose one page function reads the
 *  line at many widths reads with it too, so the two read a chip the same way. Dispatch sends no
 *  Content-Security-Policy, so the page may build a function from a string. */
export const LABELS_READING = String(readLabelsAgainstTheCount);

/**
 * Reads `LabelsAgainstTheCount` on the header's properties line. With `narrowBy`, it reads the
 * frame after the line narrows by that many px: in one task, so before any frame is drawn and
 * before the labels are read again on the next one, the line takes an inline `max-width` that
 * much under its width and the chips are then read. The line keeps the inline width; the row
 * takes it off.
 */
export function labelsAgainstTheCount(
  page: Page,
  narrowBy?: number
): Promise<LabelsAgainstTheCount> {
  return propertiesLine(page).evaluate(
    (line, { narrowBy, reading }) => {
      if (narrowBy !== undefined) {
        line.style.maxWidth = `${line.getBoundingClientRect().width - narrowBy}px`;
      }
      const read = new Function(`return ${reading}`)() as (line: Element) => LabelsAgainstTheCount;
      return read(line);
    },
    { narrowBy, reading: LABELS_READING }
  );
}

/** Holds the header's labels' box at the width the chips it draws now fill, rounded up to a
 *  whole pixel, whatever the rest of the line does after: an inline `flex` of that basis that
 *  neither grows nor shrinks. */
export function holdLabelsAtTheirChips(page: Page): Promise<void> {
  return propertiesLine(page).evaluate((line) => {
    const box = line.querySelector<HTMLElement>('[data-testid="issue-header-labels"]');
    const last = [...(box?.querySelectorAll("[data-header-label]") ?? [])].at(-1);
    if (box === null || last === undefined) throw new Error("the header draws no label");
    const width = Math.ceil(last.getBoundingClientRect().right - box.getBoundingClientRect().left);
    box.style.flex = `0 0 ${width}px`;
  });
}

/** What the count's press left in the history entry: its `waitingPress` number, or null. */
export function waitingPressInHistory(page: Page): Promise<number | null> {
  return page.evaluate(() => {
    const found = /"waitingPress":(-?\d+(?:\.\d+)?(?:e[+-]?\d+)?)/.exec(
      JSON.stringify(history.state) ?? ""
    );
    return found?.[1] === undefined ? null : Number(found[1]);
  });
}

/** A title of `lines` lines at the header's width: words added until the heading, drawn whole,
 *  takes that many lines of its own line height. */
export async function titleOfLines(page: Page, lines: number): Promise<string> {
  const words = ["The", "header", "keeps", "the", "press", "that", "ends", "a", "title", "edit"];
  let title = "Edit";
  for (let index = 0; index < 400; index += 1) {
    title = `${title} ${words[index % words.length]}`;
    // The caller waits for the header drawn first, and each call lays out and measures its own
    // hidden copy within the call.
    const drawn = await page.evaluate((text) => {
      const heading = document.querySelector<HTMLElement>(
        "[data-issue-page] [data-issue-header] h1"
      );
      if (heading === null) return 0;
      const probe = heading.cloneNode() as HTMLElement;
      probe.textContent = text;
      probe.style.display = "block";
      probe.style.webkitLineClamp = "unset";
      probe.style.width = `${heading.getBoundingClientRect().width}px`;
      probe.style.position = "absolute";
      probe.style.visibility = "hidden";
      heading.parentElement?.append(probe);
      const lineHeight = Number.parseFloat(getComputedStyle(probe).lineHeight);
      const count = Math.round((probe.getBoundingClientRect().height - 2) / lineHeight);
      probe.remove();
      return count;
    }, title);
    if (drawn === lines) return title;
  }
  throw new Error(`no title takes ${lines} lines`);
}

/** The page's clipboard, as `holdClipboard` replaces it. */
export interface HeldClipboard {
  /** Lets every held write land: each `writeText` promise still out resolves. */
  land(): Promise<void>;
  /** How many writes the page has made to the clipboard. */
  writes(): Promise<number>;
}

/**
 * Replaces the page's clipboard, before the page loads, with one that counts each
 * `navigator.clipboard.writeText` and holds the promise it returns until the row calls `land`,
 * so a row can press a copy control while its write is out. Called before the row's first
 * navigation, as `recordClipboard` (`clipboard.ts`) is. It lives in this file, not beside
 * `recordClipboard`, because this part changes no browser file of the suite but its own.
 */
export async function holdClipboard(page: Page): Promise<HeldClipboard> {
  await page.addInitScript(() => {
    const state = { held: [] as (() => void)[], writes: 0 };
    window.__dispatchHeldClipboard = state;
    Object.defineProperty(navigator, "clipboard", {
      configurable: true,
      value: {
        writeText: () => {
          state.writes += 1;
          const { promise, resolve } = Promise.withResolvers<void>();
          state.held.push(resolve);
          return promise;
        },
      },
    });
  });
  return {
    land: () =>
      page.evaluate(() => {
        for (const resolve of window.__dispatchHeldClipboard?.held.splice(0) ?? []) resolve();
      }),
    writes: () => page.evaluate(() => window.__dispatchHeldClipboard?.writes ?? 0),
  };
}
