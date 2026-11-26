import { afterEach, beforeEach, expect, spyOn, test } from "bun:test";
import { QueryClient } from "@tanstack/react-query";
import { act, cleanup, type RenderResult, render, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { api } from "../../api/client";
import type { Ask, AuthenticatedUser, IssueChild, IssueDetails, Project } from "../../api/types";
import { DESKTOP_WIDTH, PHONE_WIDTH, stubViewport } from "../margin/margin-fixture";
import { userPreferenceStorageKey } from "../shell/userPreference";
import { issueDetails, KEY } from "./decisions-filter/decisions-fixtures";
import { IssueOutlinePage } from "./IssueOutlinePage";
import { presetPath } from "./issue-address";
import { TWO_THREADS } from "./outline-fixtures";
import { AppProviders, commentNodes, stubReads, treeOf } from "./slots/stage-fixtures";
import type { TreeAnswer } from "./tree-read/tree-read";

/**
 * The places the header and the lines above the rows hold before their reads answer (R3, "The
 * places held" of the plan, rows 2 to 4, and the moves the CI legs read at `da175e7a` that are the
 * header's or a line's own size). Each line drawn before its read answers holds the place of what
 * the read brings, so nothing after it moves when the answer comes:
 *
 * - row 2: the count of what waits on the reader (`[data-testid="issue-whose-turn"]`), which the
 *   load rows saw drawn into the properties line once the tree read answered, sliding the
 *   approval control after it 160 px right at both widths;
 * - row 3: the approval steps of a root of a gated project, drawn once the projects read answers;
 * - row 4: the children's section, drawn open and then folded once the reader's login said which
 *   fold they saved;
 * - the line under each of the two rows of "In short" above the rows below 1024 px, which counts
 *   what each row's view holds once the tree's reads answer, and which the load rows at 390 px saw
 *   push the children's rows down 30 px.
 *
 * A unit test's DOM lays nothing out, so a held place is read by what decides its box: the element
 * that holds it, where it stands in its line, and its classes; and the answer by the element it
 * is drawn in. The boxes the browser lays out are read by row 1 of `c-load-1440` and `c-load-390`.
 */

/** The reader every case signs in as. */
const READER = "alice";

/** The open question that waits on the reader: the header's count of what waits counts it. */
const WAITING_ASK: Ask = {
  anchor: null,
  answer: null,
  artifact_id: null,
  asked_of: null,
  asked_of_at: null,
  asked_of_by: null,
  author: { id: "session-1", kind: "session" },
  block_id: null,
  created_at: "2026-11-05T00:00:00Z",
  edited_at: null,
  id: "ask-open",
  issue_key: KEY,
  kind: "question",
  multiple: false,
  opened_event_id: 1,
  options: [],
  question: "Should this ship?",
  state: "open",
  urgency: "med",
  waiting_on: "human",
  waiting_on_person: READER,
};

/** The issue's two children, which the children's section lists. */
const CHILDREN: IssueChild[] = ["CORE-2", "CORE-3"].map((key) => ({
  active_at: "2026-11-05T12:00:00Z",
  external_links: null,
  key,
  status: "todo",
  subtree_done: 0,
  subtree_total: 1,
  title: `The child ${key}`,
}));

/** The issue's project, which gates the Done-when criteria: its root draws that step. */
const GATED: Project = {
  created_at: "2026-09-09T00:00:00Z",
  gated_steps: ["criteria"],
  key: "CORE",
  name: "Core",
  open_asks: 0,
  proven_sessions: false,
};

/** The fixtures' issue, a root of its project, with the question that waits on the reader and
 *  its two children. */
function heldIssue(): IssueDetails {
  return {
    ...issueDetails(),
    children: CHILDREN,
    open_asks: [{ ...WAITING_ASK, last_reply: null }],
  };
}

/** A read the case holds until it settles it: what `Promise.withResolvers` gives. */
interface Held<T> {
  readonly promise: Promise<T>;
  readonly reject: (reason?: unknown) => void;
  readonly resolve: (value: T) => void;
}
/** The reads a case holds, each answered by the case: the tree read, the projects read and the
 *  reader's login. */
let tree: Held<TreeAnswer>;
let projects: Held<Project[]>;
let whoami: Held<AuthenticatedUser>;

let stubs: { mockRestore(): void }[] = [];
let restoreViewport: () => void = () => {};
beforeEach(() => {
  tree = Promise.withResolvers();
  projects = Promise.withResolvers();
  whoami = Promise.withResolvers();
  stubs = [
    ...stubReads(),
    spyOn(api, "getIssue").mockResolvedValue(heldIssue()),
    spyOn(api, "listComments").mockResolvedValue([...TWO_THREADS]),
    spyOn(api, "getIssueTree").mockImplementation((_key, query = {}) =>
      query.view === undefined ? tree.promise : Promise.resolve(treeOf([]))
    ),
    spyOn(api, "listProjects").mockImplementation(() => projects.promise),
    spyOn(api, "whoAmI").mockImplementation(() => whoami.promise),
    spyOn(api, "getMyState").mockResolvedValue({}),
    spyOn(api, "listProjectArchitectureSources").mockResolvedValue([]),
  ];
});
afterEach(() => {
  cleanup();
  restoreViewport();
  for (const stub of stubs.reverse()) stub.mockRestore();
  window.localStorage.removeItem(userPreferenceStorageKey(READER, "issue.children-section"));
});

/** The page's root at Conversation's address of the held issue, at `width`, under what the app
 *  provides above it. */
function drawPage(width = DESKTOP_WIDTH): RenderResult {
  restoreViewport = stubViewport(width);
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

/** The header's properties line, once it is drawn. */
async function propertiesLine(view: RenderResult): Promise<HTMLElement> {
  return view.findByTestId("issue-state-actions");
}

/** The place among the line's own items of the item that is or holds `element`: a list the line
 *  draws as items of its own (`layout="contents"`) stands inside a box that draws none, which is
 *  the line's item. */
function placeIn(line: HTMLElement, element: Element | null): number {
  return element === null
    ? -1
    : [...line.children].findIndex((item) => item === element || item.contains(element));
}

/** The classes that decide a box's size: every class but colour and focus ring ones, each in
 *  every theme, which draw no box of their own. */
function boxClasses(element: Element | null): string[] {
  return (element?.className ?? "")
    .split(/\s+/u)
    .filter(
      (name) =>
        name !== "" &&
        !/^(?:dark:)?(?:text-page|bg-page|border-page|focus-visible:|invisible|border-transparent)/u.test(
          name
        )
    )
    .sort();
}

// Row 2, and the approval control's slide the CI legs read. The count of what waits on the reader
// is drawn once the reads it counts answer, the tree read among them; until then the line holds a
// place of the count's own box where it will stand, and the count is drawn in that place, so the
// approval control after it - "Not requested" - is the same element at the same place in the line
// before and after.
test("the line holds the count's box before the tree read answers, and the count is drawn in it", async () => {
  await act(async () => whoami.resolve({ event_head: "0", kind: "user", login: READER }));
  await act(async () => projects.resolve([]));
  const view = drawPage();
  const line = await propertiesLine(view);
  await waitFor(() =>
    expect(line.textContent, "the line drew no approval control").toContain("Not requested")
  );
  const slot = line.querySelector("[data-whose-turn-slot]");
  expect(
    line.querySelector('[data-testid="issue-whose-turn"]'),
    "the count was drawn before the tree read answered"
  ).toBeNull();
  expect(slot, "the line held no place for the count while the tree read was out").not.toBeNull();
  expect(slot?.getAttribute("aria-hidden"), "the held place is heard").toBe("true");
  const approval = [...line.children].find((item) => item.textContent === "Not requested") ?? null;
  const at = placeIn(line, slot);
  const approvalAt = placeIn(line, approval);

  await act(async () => tree.resolve(treeOf(commentNodes(TWO_THREADS))));
  const chip = await waitFor(() => {
    const drawn = line.querySelector('[data-testid="issue-whose-turn"]');
    expect(drawn, "the count was not drawn once the tree read answered").not.toBeNull();
    return drawn;
  });
  expect(line.querySelector("[data-whose-turn-slot]"), "the held place stayed").toBeNull();
  expect(placeIn(line, chip), "the count was not drawn where the line held its place").toBe(at);
  expect(boxClasses(chip), "the count's box is not the box its place held").toEqual(
    boxClasses(slot)
  );
  expect(
    [...line.children].includes(approval as Element) && placeIn(line, approval),
    "the approval control after the count moved in the line, or was drawn again"
  ).toBe(approvalAt);
});

// Row 3. A root of a gated project draws its approval steps once the projects read answers; until
// then the line holds a place of a step's height where they will stand, and the steps are drawn
// in that place.
test("a root's line holds the approval steps' place before the projects read answers", async () => {
  await act(async () => whoami.resolve({ event_head: "0", kind: "user", login: READER }));
  await act(async () => tree.resolve(treeOf(commentNodes(TWO_THREADS))));
  const view = drawPage();
  const line = await propertiesLine(view);
  const slot = await waitFor(() => {
    const held = line.querySelector("[data-approval-steps-slot]");
    expect(
      held,
      "the root's line held no place for its steps while the projects read was out"
    ).not.toBeNull();
    return held;
  });
  expect(slot?.getAttribute("aria-hidden"), "the held place is heard").toBe("true");
  expect(slot?.className, "the held place holds no step's height").toContain("min-h-6");
  const at = placeIn(line, slot);

  await act(async () => projects.resolve([GATED]));
  const steps = await waitFor(() => {
    const drawn = line.querySelector('[data-testid="issue-approval-steps"]');
    expect(drawn, "the steps were not drawn once the projects read answered").not.toBeNull();
    return drawn;
  });
  expect(line.querySelector("[data-approval-steps-slot]"), "the held place stayed").toBeNull();
  expect(placeIn(line, steps), "the steps were not drawn where the line held their place").toBe(at);
});

// Row 4. The children's section is drawn once the reader's saved fold of it is known, which their
// login says: a section the reader folded is drawn folded, and never drawn open first.
test("the children's section is drawn once, folded as the reader saved it, after their login answers", async () => {
  window.localStorage.setItem(
    userPreferenceStorageKey(READER, "issue.children-section"),
    "collapsed"
  );
  await act(async () => tree.resolve(treeOf(commentNodes(TWO_THREADS))));
  await act(async () => projects.resolve([]));
  const view = drawPage();
  await propertiesLine(view);
  const openDrawn: string[] = [];
  const watch = new MutationObserver(() => {
    const toggle = view.container.querySelector("[data-children-toggle]");
    if (toggle?.getAttribute("aria-expanded") === "true") openDrawn.push("open");
  });
  watch.observe(view.container, { attributes: true, childList: true, subtree: true });
  try {
    await act(async () => {});
    expect(
      view.container.querySelector("[data-children-section]"),
      "the section was drawn before the reader's login answered"
    ).toBeNull();
    await act(async () => whoami.resolve({ event_head: "0", kind: "user", login: READER }));
    const toggle = await waitFor(() => {
      const drawn = view.container.querySelector("[data-children-toggle]");
      expect(drawn, "the section was not drawn once the login answered").not.toBeNull();
      return drawn;
    });
    expect(toggle, "the section was not drawn folded as saved").toHaveAttribute(
      "aria-expanded",
      "false"
    );
    expect(openDrawn, "the section was drawn open before it was folded as saved").toEqual([]);
  } finally {
    watch.disconnect();
  }
});

// The children's rows the CI legs saw pushed 30 px down at 390 px in Conversation, Verification and
// Plan, as the tree's reads answered: below 1024 px the issue's three sentences and their two rows
// stand above the children's section (`contents-frame.tsx`'s `ThreeThingsAbove`), and each row
// drew the line that counts what its view holds only once those reads answered. Each row holds
// that line's place from its first drawing, and draws the counts in it.
test("below 1024 px each row of In short holds its counts' line before the reads answer", async () => {
  await act(async () => whoami.resolve({ event_head: "0", kind: "user", login: READER }));
  await act(async () => projects.resolve([]));
  const view = drawPage(PHONE_WIDTH);
  const rows = await waitFor(() => {
    const drawn = [...view.container.querySelectorAll<HTMLElement>("[data-brief-row]")];
    expect(drawn.length, "the page drew no row of In short above the rows").toBe(2);
    return drawn;
  });
  const held = rows.map((row) => row.querySelector("[data-brief-counts]"));
  for (const [at, line] of held.entries()) {
    const row = rows[at]?.dataset.briefRow;
    expect(line, `the ${row} row held no counts' line while the reads were out`).not.toBeNull();
    expect(line?.className, `the ${row} row's held line holds no line's height`).toContain(
      "min-h-[18px]"
    );
    expect(line?.textContent, `the ${row} row's held line holds text`).toBe("");
  }

  await act(async () => tree.resolve(treeOf(commentNodes(TWO_THREADS))));
  await waitFor(() =>
    expect(
      rows.map((row) => row.querySelector("[data-brief-counts]")?.textContent ?? ""),
      "a row drew no counts once the reads answered"
    ).not.toContain("")
  );
  expect(
    rows.map((row) => row.querySelector("[data-brief-counts]")),
    "a row drew its counts in another line than the one it held"
  ).toEqual(held);
});
