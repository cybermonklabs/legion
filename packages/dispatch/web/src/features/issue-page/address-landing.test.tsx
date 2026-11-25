import { afterEach, beforeEach, expect, spyOn, test } from "bun:test";
import { act, cleanup, type RenderResult, waitFor } from "@testing-library/react";

import { nextFrame } from "../../__tests__/frames";
import { ApiError, api } from "../../api/client";
import type { ArtifactBlock, IssueCoverage, IssueDetails } from "../../api/types";
import {
  artifact,
  issueDetails,
  KEY,
  SPEC_SLUG,
  specRef,
} from "./decisions-filter/decisions-fixtures";
import { TWO_THREADS } from "./outline-fixtures";
import { type Preset, STATE_LINES } from "./page-words";
import { FAILED_READ_RETRY } from "./slots/failed-read-line";
import { drawnSlot, drawStage, GONE_COMMENT, REFUSED, stubReads } from "./slots/stage-fixtures";
import { blockNodeId } from "./tree-drafts";
import { nodeRowSelectorOf } from "./tree-focus";
import { check, VERIFICATION_SLUG } from "./verification-filter/verification-fixtures";

/**
 * An address that names a node moves the focus once, onto where it lands (dispatch://INFRA-1196
 * decision 413; part N's plan, ST-4): no view's tab stop and no opening of the outline takes the
 * focus while the address's landing is undecided. Each case reads every `focusin` from the
 * stage's mount until the landing, and two frames after it, and fails on any move before the
 * last, naming the element that took the focus.
 */

let stubs: { mockRestore(): void }[] = [];
/** Every element the focus moved to since the case's stage was drawn, in order. */
let moves: Element[] = [];
const heard = (event: FocusEvent) => {
  if (event.target instanceof Element) moves.push(event.target);
};
beforeEach(() => {
  stubs = stubReads();
  (document.activeElement as HTMLElement | null)?.blur();
  moves = [];
  document.addEventListener("focusin", heard, true);
});
afterEach(() => {
  document.removeEventListener("focusin", heard, true);
  cleanup();
  for (const stub of stubs) stub.mockRestore();
});

/** Takes the stage of one address down, and forgets the focus it moved, before a case draws the
 *  next. */
function drawnNext(): void {
  cleanup();
  moves = [];
}

/** The plan and the Verification document of the issue the cases draw (`verifiedIssue`), beside
 *  its spec and its decisions document. */
const PLAN_DOCUMENT = artifact("artifact-plan", "plan-md");
const VERIFICATION_DOCUMENT = artifact("artifact-verification", VERIFICATION_SLUG);

/** The issue with its plan and its Verification document too, each in its role. */
function verifiedIssue(): IssueDetails {
  const issue = issueDetails();
  return {
    ...issue,
    artifacts: [...(issue.artifacts ?? []), PLAN_DOCUMENT, VERIFICATION_DOCUMENT],
    plan_artifact_id: PLAN_DOCUMENT.id,
    verification_artifact_id: VERIFICATION_DOCUMENT.id,
  };
}

/** The block ids of the spec's two statements, and of the plan task and the check that each
 *  serve the first. The Spec view's tab stop starts on its first statement, so an address naming
 *  the second tells a landing on it from the take of the view's tab stop. */
const FIRST = "r1";
const SECOND = "r2";
const PLAN_TASK = "t1";
const CHECK = "c1";

/** One typed block of a document as the blocks read answers it, with the statement it serves
 *  (`serves`, a `block[]` pointer) where it is given one. */
function typedBlock(id: string, type: string, serves?: string): ArtifactBlock {
  return {
    attributes: serves === undefined ? [] : [{ kind: "block[]", name: "serves", value: serves }],
    from: 0,
    id,
    references: { asks: 0, comments: 0 },
    to: 1,
    token: id,
    type,
  };
}

/** Each document's blocks, by its id: the spec's statements, and the plan's task and the
 *  Verification document's check, whose `serves` places each under the first statement. */
const BLOCKS: Readonly<Record<string, readonly ArtifactBlock[]>> = {
  "artifact-spec": [typedBlock(FIRST, "requirement"), typedBlock(SECOND, "requirement")],
  [PLAN_DOCUMENT.id]: [typedBlock(PLAN_TASK, "plan_task", specRef(FIRST))],
  [VERIFICATION_DOCUMENT.id]: [typedBlock(CHECK, "check", specRef(FIRST))],
};

/** The coverage read with the spec's two statements. */
const COVERAGE: IssueCoverage = {
  gaps: [],
  has_plan: true,
  issue: KEY,
  requirements: [FIRST, SECOND].map((block, at) => ({
    block_id: block,
    checks: block === FIRST ? 1 : 0,
    gaps: [],
    plan_tasks: block === FIRST ? 1 : 0,
    ref: specRef(block),
    text: `The page draws statement ${at + 1}.`,
  })),
  spec_artifact_id: "artifact-spec",
};

/** Answers the reads the tree and the views are drawn from for `verifiedIssue`, and holds out
 *  the messages read, one of the reads the tree's rows come from: until it answers no view can
 *  tell the stage whether it draws the node an address names, while the views draw their rows
 *  from the reads already in. Gives the call that answers it. Each spy is the one `stubReads`
 *  set, which `spyOn` hands back, so the `afterEach` that restores those restores it. */
function answerReadsButMessages(): () => Promise<void> {
  spyOn(api, "getIssueCoverage").mockResolvedValue(COVERAGE);
  spyOn(api, "getArtifactBlocks").mockImplementation(async (id: string) => [...(BLOCKS[id] ?? [])]);
  spyOn(api, "getIssueResults").mockResolvedValue({
    checks: [check({ block_id: CHECK })],
    issue: KEY,
    verification_artifact_id: VERIFICATION_DOCUMENT.id,
  });
  spyOn(api, "listComments").mockResolvedValue([...TWO_THREADS]);
  const messagesOut = Promise.withResolvers<[]>();
  spyOn(api, "listIssueMessages").mockReturnValue(messagesOut.promise);
  return () => act(async () => messagesOut.resolve([]));
}

/** Each view's body, by its own mark. */
const VIEW_BODY: Readonly<Record<"plan" | "spec" | "verification", string>> = {
  plan: "[data-plan-body]",
  spec: "[data-spec-body]",
  verification: "[data-verification-body]",
};

/** The row of the node `id` that a view draws, and the one the outline draws. */
const viewRow = (preset: keyof typeof VIEW_BODY, id: string) =>
  `${VIEW_BODY[preset]} ${nodeRowSelectorOf(id)}`;
const outlineRow = (id: string) => `[data-issue-body] ${nodeRowSelectorOf(id)}`;

/** What an element that took the focus is, for a case's message: a row by whose it is, the
 *  outline's or a view's, and its node; a failed read's Retry by its line; else its tag and
 *  words. */
function named(element: Element | null | undefined): string {
  if (element === null || element === undefined) return "nothing";
  const row = element.closest<HTMLElement>("[data-node-row]");
  const whose = element.closest("[data-issue-body]") === null ? "the view's" : "the outline's";
  if (row !== null) return `${whose} row of ${row.dataset.nodeId}`;
  const line = element.closest("[data-failed-read]");
  if (line !== null) return `the Retry of ${whose} line "${line.textContent}"`;
  return `${element.tagName.toLowerCase()} "${element.textContent?.trim() ?? ""}"`;
}

/** Waits for the view of `preset`, or the outline where no view is on, to draw a row, its reads
 *  but the held one in. */
async function viewDrawsRows(
  view: RenderResult,
  preset: Exclude<Preset, "decisions">,
  what: string
): Promise<void> {
  const body = preset === "conversation" ? "[data-issue-body]" : VIEW_BODY[preset];
  await waitFor(() =>
    expect(
      view.container.querySelector(`${body} [data-node-row]`),
      `${what} drew no row of the view with the messages out; the stage drew ${drawnSlot(view)}`
    ).not.toBeNull()
  );
}

/** Waits for `landed` to find the element the landing puts the focus on and for that element to
 *  hold it, then two frames more, for a take the stage gives a frame; and reads that the focus
 *  moved once from the stage's mount on, onto that element. */
async function landsOnce(
  view: RenderResult,
  landed: () => Element | null,
  what: string
): Promise<void> {
  const element = await waitFor(() => {
    const found = landed();
    expect(
      found,
      `${what} drew nothing to land on; the stage drew ${drawnSlot(view)}, with the focus on ${named(document.activeElement)}`
    ).not.toBeNull();
    expect(found, `${what} left the focus on ${named(document.activeElement)}`).toHaveFocus();
    return found as Element;
  });
  await act(() => nextFrame());
  await act(() => nextFrame());
  expect(
    moves.slice(0, -1).map(named),
    `${what} moved the focus before it landed on ${named(element)}`
  ).toEqual([]);
  expect(named(moves.at(-1)), `${what} moved the focus last`).toBe(named(element));
}

// A statement the Spec view draws, the second, and a check the Verification view draws: until
// every read has settled the view cannot tell the stage it draws the node, and its tab stop
// takes no focus meanwhile, so the focus goes once, to the node's row. Taken as the rows were
// drawn, it went to the first statement, then to the second.
test("an address lands the focus once on a node the view draws", async () => {
  for (const [preset, block, row] of [
    ["spec", SECOND, blockNodeId(SPEC_SLUG, SECOND)],
    ["verification", CHECK, blockNodeId(VERIFICATION_SLUG, CHECK)],
  ] as const) {
    const messages = answerReadsButMessages();
    const view = drawStage(`#b-${block}`, { issue: verifiedIssue(), preset });
    const what = `the address of ${block} under ${preset}`;
    await viewDrawsRows(view, preset, what);
    await messages();
    await landsOnce(view, () => view.container.querySelector(viewRow(preset, row)), what);
    expect(drawnSlot(view), `${what} drew the outline in place of the view`).toBe("the view");
    drawnNext();
  }
});

// A plan task under the statement is no row of the Verification view, and a comment no row of
// the Plan view: each view tells the stage so once every read has settled, and the outline drawn
// in its place lands on the node's row. The view's tab stop took the focus first, before it.
test("an address lands the focus once on a node beside the view", async () => {
  for (const [preset, after, row] of [
    ["verification", `#b-${PLAN_TASK}`, blockNodeId(PLAN_DOCUMENT.slug, PLAN_TASK)],
    ["plan", "?comment=head-b", "head-b"],
  ] as const) {
    const messages = answerReadsButMessages();
    const view = drawStage(after, { issue: verifiedIssue(), preset });
    const what = `the address ${after} under ${preset}`;
    await viewDrawsRows(view, preset, what);
    await messages();
    await landsOnce(view, () => view.container.querySelector(outlineRow(row)), what);
    drawnNext();
  }
});

// A comment the issue holds none of, beside each view and on the outline's own address: the view
// draws no row for it, the outline drawn in its place holds none either, and its line says so once
// the comment's own read answers, a couple of frames after every other read has settled, as a
// request does. Neither the view's tab stop nor the outline's opening on its first row takes the
// focus before that line's heading does.
test("an address lands the focus once on a gone node", async () => {
  for (const preset of ["spec", "verification", "plan", "conversation"] as const) {
    const messages = answerReadsButMessages();
    const commentOut = Promise.withResolvers<never>();
    const read = spyOn(api, "getComment").mockReturnValue(commentOut.promise);
    try {
      const view = drawStage(`?comment=${GONE_COMMENT}`, { issue: verifiedIssue(), preset });
      const what = `the address of a gone comment under ${preset}`;
      await viewDrawsRows(view, preset, what);
      await messages();
      await act(() => nextFrame());
      await act(() => nextFrame());
      await act(async () =>
        commentOut.reject(new ApiError(404, { code: "NOT_FOUND", error: "no such comment" }))
      );
      await landsOnce(view, () => view.queryByRole("heading", { name: "Comment not found" }), what);
    } finally {
      read.mockRestore();
    }
    drawnNext();
  }
});

// The view's own address names no node, so there is no landing to wait for: the tab stop takes
// the focus as the view draws its rows, with a read still out, and nothing moves it after.
test("an address that names no node puts the focus on the view's tab stop at once", async () => {
  const messages = answerReadsButMessages();
  const view = drawStage("", { issue: verifiedIssue(), preset: "spec" });
  const first = viewRow("spec", blockNodeId(SPEC_SLUG, FIRST));
  await waitFor(() =>
    expect(
      view.container.querySelector(first),
      `the Spec view's address left the focus on ${named(document.activeElement)} with the messages out`
    ).toHaveFocus()
  );
  await messages();
  await landsOnce(view, () => view.container.querySelector(first), "the Spec view's own address");
});

// The comments read refused: the Spec view draws no line of its own for it, so it tells the
// stage it draws no node, and the outline drawn in its place opens on the comments' Retry, the
// one move. The coverage read refused: the Verification view draws its own line for it and no
// row, and tells the stage nothing; its Retry takes the focus as it is drawn, and nothing moves
// it after.
test("an address lands the focus once on a failed read's Retry", async () => {
  const messages = answerReadsButMessages();
  spyOn(api, "listComments").mockRejectedValue(REFUSED);
  const spec = drawStage("?comment=head-b", { issue: verifiedIssue(), preset: "spec" });
  await viewDrawsRows(spec, "spec", "a comment's address under Spec");
  await messages();
  await landsOnce(
    spec,
    () =>
      [...spec.container.querySelectorAll("[data-issue-body] [data-failed-read]")]
        .find((line) => line.textContent?.includes(STATE_LINES.comments_unreadable))
        ?.querySelector(FAILED_READ_RETRY) ?? null,
    "a comment's address under Spec with the comments read refused"
  );
  drawnNext();
  const messagesAgain = answerReadsButMessages();
  spyOn(api, "getIssueCoverage").mockRejectedValue(REFUSED);
  const verification = drawStage(`#b-${CHECK}`, {
    issue: verifiedIssue(),
    preset: "verification",
  });
  const retry = `${VIEW_BODY.verification} ${FAILED_READ_RETRY}`;
  await waitFor(() =>
    expect(
      verification.container.querySelector(retry),
      `the Verification view drew no Retry; the stage drew ${drawnSlot(verification)}`
    ).not.toBeNull()
  );
  await messagesAgain();
  await landsOnce(
    verification,
    () => verification.container.querySelector(retry),
    "a check's address under Verification with the coverage read refused"
  );
});

/** The reader's acts a case makes before an address lands, each by its name: a key, a press on
 *  blank space and a turn of the wheel (`READER_ACTS`), none of which moves the focus itself. */
const READER_ACTS_FIRST = [
  [
    "a key",
    () =>
      document.body.dispatchEvent(new KeyboardEvent("keydown", { bubbles: true, key: "Shift" })),
  ],
  [
    "a press",
    () => document.body.dispatchEvent(new PointerEvent("pointerdown", { bubbles: true })),
  ],
  ["a wheel turn", () => window.dispatchEvent(new Event("wheel"))],
] as const;

// The reader acts before the view draws a row, with the coverage read out: the landing then moves
// nothing (I's and Q's rule), and neither does the view's tab stop once the view tells the stage
// it draws the node. Taken as the rows were drawn, the tab stop took the focus after the act.
test("an address the reader acts on first moves the focus nowhere", async () => {
  for (const [preset, block, row] of [
    ["spec", SECOND, blockNodeId(SPEC_SLUG, SECOND)],
    ["verification", CHECK, blockNodeId(VERIFICATION_SLUG, CHECK)],
  ] as const) {
    for (const [name, acted] of READER_ACTS_FIRST) {
      const messages = answerReadsButMessages();
      const coverageOut = Promise.withResolvers<IssueCoverage>();
      spyOn(api, "getIssueCoverage").mockReturnValue(coverageOut.promise);
      const view = drawStage(`#b-${block}`, { issue: verifiedIssue(), preset });
      const what = `the address of ${block} under ${preset} after ${name}`;
      act(() => {
        acted();
      });
      await act(async () => coverageOut.resolve(COVERAGE));
      await messages();
      await waitFor(() =>
        expect(
          view.container.querySelector(viewRow(preset, row)),
          `${what} drew no row of it; the stage drew ${drawnSlot(view)}`
        ).not.toBeNull()
      );
      await act(() => nextFrame());
      await act(() => nextFrame());
      expect(moves.map(named), `${what} moved the focus`).toEqual([]);
      drawnNext();
    }
  }
});
