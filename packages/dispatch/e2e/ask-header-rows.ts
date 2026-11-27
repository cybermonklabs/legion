// What the rows of `ask-header-sweep.e2e.ts` and `ask-header-at-width.e2e.ts` share
// (dispatch://INFRA-341, R1): the session every ask is asked by and the start every row takes
// from it, the widths `web/src/styles.css` names a header's layouts by, what a card's header and
// a closed issue's answered block draw (`Drawn`), and the answered decision block both files
// seed. A helper one of those files needs stays in that file.
import { readFileSync } from "node:fs";

import { type FakeSession, setLiveSessions } from "./agents";
import { answerAsk, createIssue, createProject } from "./api";
import type { Drawn, Lines } from "./ask-header-reader";
import { indexedBlockAsk } from "./editor";
import { resetDatabase } from "./seed";
import { sessionIds } from "./session-ids";

/** The one session every ask here is asked by, and whose turn an agent's turn is: its title is
 *  the longest name a header draws, the name that has to give up its width. */
export const planner: FakeSession = {
  capabilities: ["steer"],
  dir: "/workspaces/planner",
  last_seen: 1_700_000_000_000,
  machine_id: "e2e",
  roles: [],
  session_id: sessionIds.planner,
  title: "Release planner for the ledger mirror",
};

export const asker = {
  actor: { id: sessionIds.planner, kind: "session" as const },
  as: "agent" as const,
};

/** What every row of both files starts from, before it seeds anything: an emptied database, and
 *  `planner` the one session the fake Envoy holds live. */
export async function resetToPlanner(): Promise<void> {
  await resetDatabase();
  await setLiveSessions([planner]);
}

/** The sheet the three layout widths are named in, as container sizes of its `@theme`. */
const STYLESHEET = readFileSync(new URL("../web/src/styles.css", import.meta.url), "utf8");

/** The browser's own default font size, which every row reads at unless it says otherwise: the
 *  px of a `rem`, since nothing in `styles.css` sizes the root. */
export const DEFAULT_FONT = 16;

/** The width `styles.css`'s `@theme` names `--container-<name>`, in px of a root whose `rem` is
 *  `rem` px: the one place each layout width is written, so these rows read the width the
 *  classes do. */
export function containerWidth(name: string, rem = DEFAULT_FONT): number {
  const found = new RegExp(`--container-${name}:\\s*([\\d.]+)rem;`).exec(STYLESHEET);
  if (found?.[1] === undefined) {
    throw new Error(`web/src/styles.css's @theme names no --container-${name} in rem`);
  }
  return Number(found[1]) * rem;
}

/** From this width a read-only decision block's header is one line, below it rows
 *  (`--container-ask-block-line`, 36rem). */
const ASK_BLOCK_LINE = containerWidth("ask-block-line");
/** From this width of the ask body the end of its first line - the kind, who asked and when,
 *  the state label and the copy control - stands beside the question's words, on one line; below
 *  it, under them (`@container/ask-body` and `@max-[32rem]/ask-body` in
 *  `web/src/features/ask-body/AskHeader.tsx`, dispatch://INFRA-5702 R15). */
export const ASK_BODY_BESIDE = 32 * DEFAULT_FONT;

/** The end of the ask body's first line (`CardAskHeader`): one line beside the words from
 *  `ASK_BODY_BESIDE`; under them below it, where each of its parts - the kind, who asked and
 *  when, and the label with the copy control - moves to a line of its own before it gives up
 *  width. */
const CARD_LINES: Lines = [
  [ASK_BODY_BESIDE, 1, 1],
  [0, 1, 3],
];

/** A read-only decision block's header (`BlockAskHeader`): from `ASK_BLOCK_LINE` one line, with
 *  "Answered by" on it or on the next; below, the urgency, `Decision` and the label - in two
 *  lines where even those do not fit beside the copy control - then who asked, then who
 *  answered. */
const ANSWERED_BLOCK_LINES: Lines = [
  [ASK_BLOCK_LINE, 1, 2],
  [0, 3, 4],
];

/** The header's own parts on an open ask's first line, at each place a card is drawn: the
 *  header is that line (`[data-ask-card-row]`), its row the line's end, and its layout follows
 *  the ask body's width, which the sweeps size. The urgency notch, the anchor and whose turn it
 *  is stand outside it, on the card and under the words. */
export const CARD_HEADER: Drawn = {
  fields: {
    asked: "[data-ask-card-asked]",
    asker: "[data-ask-card-asked] span.truncate[title]",
    copy: "[data-copy-ref] button",
    label: "[data-ask-state-label]",
    "live link": '[data-ask-card-asked] a[aria-label^="Open live conversation"]',
    time: "[data-ask-card-asked] time",
  },
  lines: CARD_LINES,
  row: "[data-ask-card-end]",
  sized: "[data-ask-body]",
};

/** A closed issue's answered block, whose own header draws everything. */
export const ANSWERED_BLOCK: Drawn = {
  fields: {
    answered: "[data-dispatch-ask-answered]",
    asked: "[data-dispatch-ask-asked]",
    asker: "[data-dispatch-ask-asked] > [title]",
    copy: "[data-copy-ref] button",
    decision: "strong",
    label: "[data-ask-state-label]",
    time: "[data-dispatch-ask-asked] time",
    urgency: "[data-dispatch-ask-pill]",
  },
  lines: ANSWERED_BLOCK_LINES,
  row: "header",
};

/** An issue whose spec holds one decision block, asked by the long-titled session and answered,
 *  with the time the server says the answer was given. Each row closes the issue once it has
 *  seeded what it needs on it: a closed issue's answered block draws its own header, since no
 *  card under it carries the provenance. */
export async function seedAnsweredBlock() {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue(
    {
      project: "CORE",
      spec: 'Intro.\n\n:::ask{#decide urgency="blocking" multiple="false" state="open"}\nWhich way?\n\n- Left\n- Right\n:::\n',
      title: "Closed block header",
    },
    asker
  );
  const blockAsk = await indexedBlockAsk(issue.key, "decide");
  const answered = await answerAsk(blockAsk.id, {
    expected_edited_at: null,
    selected: ["Left"],
    text: "Left.",
  });
  if (answered.answer === null) throw new Error("the answered ask holds no answer");
  return { answeredAt: answered.answer.at, blockAsk, issue };
}
