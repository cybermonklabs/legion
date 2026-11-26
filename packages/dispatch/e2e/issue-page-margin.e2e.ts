/** comments on text are written through the server's routes and drawn beside their nodes from its
 *  tree, comments and messages reads */

import { type Result, routeUrl } from "../web/src/api/paths";
import type { IssueTreeRead } from "../web/src/api/types";
import { presetPath } from "../web/src/features/issue-page/issue-address";
import {
  LENS_WORDS,
  PRESET_NAMES,
  type Preset,
  sentenceCase,
} from "../web/src/features/issue-page/page-words";
import { MARGIN_ANSWERS, MARGIN_WORDS } from "../web/src/features/issue-page/page-words-margin";
import { treeIdOfNode } from "../web/src/features/issue-page/tree-read/tree-nodes";
import { buildIssuePath } from "../web/src/features/refs/routes";
import { deleteComment, editArtifact, getComment, listComments } from "./api";
import {
  closeDetails,
  contentsPane,
  gotoIssue,
  issuePage,
  marginResizeHandle,
  marginShell,
  openDetails,
  phase,
  viewportAt,
} from "./issue-page";
import {
  blockNode,
  DESKTOP,
  expectOpeningLanded,
  focusRow,
  nodeRow,
  PHONE,
  parentOfRow,
  READER,
  unfoldEveryBranch,
} from "./issue-page-g";
import {
  type MarginSeed,
  type NoThreadsSeed,
  readWithTrees,
  seedMarginIssue,
  seedNoThreads,
} from "./issue-page-margin";
import { GATES } from "./issue-page-margin-gates";
import { boxesPastTheirCard, verificationCardBoxes } from "./issue-page-v";
import { lensCatalogue, requestedTreeRead } from "./issue-page-w";
import { navigateInApp } from "./sends";
import { expect, type Locator, type Page, type Response, test } from "./test";
import {
  type ApiRequest,
  type ApiRequests,
  expectLoadSettled,
  type RouteCount,
  watchApiRequests,
} from "./traffic";
import { assertPageFits, assertWhole } from "./unclipped";

/**
 * The browser rows of comments on text (INFRA-5706): rows 1 to 8 of the issue's Verification
 * document, each titled as its check titles it, each reading the issue `seedMarginIssue`
 * (`e2e/issue-page-margin.ts`) writes, and, where the check reads it, the same issue with no
 * comment on text (`seedNoThreads`). Each row signs in as alice with the new page on (`phase`,
 * `e2e/issue-page.ts`) and opens it with `gotoIssue`.
 *
 * What the page draws, as the rows read it: the column at the right of the main column, an
 * `aside` with the role `region` named by the margin's words (`MARGIN_WORDS.region`), which sets
 * `data-text-margin` on the page's row while it is drawn; in it, one box for each thread,
 * `[data-text-thread="<root id>"]` with `data-text-thread-node` (the tree id of the node it stands
 * by) and `data-text-thread-parent` (that node's stored parent), placed at its quoted words'
 * first line; in the box, `ThreadCard`'s card `[data-margin-item="<root id>"]`, each reply a
 * `li[data-reply]` (`aria-current="true"` on the one an address names), each comment's acts
 * `[data-thread-acts]` with the Lens act `[data-row-lens]`, and each lens under a comment a
 * `[data-thread-lens]` row (`data-thread-lens-read` its read id). The quoted words carry the
 * highlights `text-comment` and `text-comment-focus` (`CSS.highlights`, `highlights.ts`), and
 * below 1024 px each node's threads stand in its sheet `[data-text-threads]` (`NodeThreads.tsx`).
 * A selection inside one row's words draws the control named `MARGIN_WORDS.comment` beside its
 * end (`SelectionComment.tsx`), which opens the composer `[data-text-composer]` in the column
 * (`SelectionComposer.tsx`); the root of the thread its send writes takes the focus.
 *
 * A reading that waits on a gate does not run: the row records it owed with an annotation of the
 * type `owed` that names the gate, read from `e2e/issue-page-margin-gates.ts` (the mirror of
 * `text-comments/gates.ts`), and passes on what it reads (decision 61). At this base G5
 * (`COMPOSER`) and G2 (`NODE_ANCHOR`) are on and every other gate is off, so each row reads what
 * those two bring - the control, the composer and its sends, the stacking, and the comments on a
 * node's words - and annotates the rest; row 7 (G7) and row 8 (part U) read nothing, and each
 * still runs, so a run finds it by its title.
 *
 * Before and after: at the merge-base, with this file, its seed (`e2e/issue-page-margin.ts`), the
 * gate table and `web/src/features/issue-page/page-words-margin.ts` (the words these rows read,
 * which draws nothing) copied in, row 1 fails on `the thread stands right of the main column`,
 * row 2 on `Comment stands beside the selection`, row 4 on `the lens stands under the comment in
 * its thread`, row 5 on `the named reply has the focus in its thread` and row 6 on `the region
 * draws each seeded thread`: each reads first what the base does draw (the rows of its nodes,
 * the comment on the statement as no row), then the column, which the base draws none of.
 */

// ---------------------------------------------------------------------------------------------
// The gates.
// ---------------------------------------------------------------------------------------------

type Gate = keyof typeof GATES;

/** What each line of the gate table waits for, as the plan's "The gates" names it. */
const GATE_WORDS: Readonly<Record<Gate, string>> = {
  ANCHOR_OCCURRENCE:
    "G3b, the tree read's document anchor answering `occurrence` (dispatch://INFRA-1253/spec@v237)",
  COMPOSER:
    "G5, pull request 3125 (INFRA-1702) on the base: the control, its composer, every send and the stacking",
  DASHBOARD: "G8, INFRA-5702's Dashboard items",
  NODE_ANCHOR:
    "G2 and G5, INFRA-1253's node anchor and the shell margin's comments read that asks for it",
  OTHER_ISSUE_COMMENTS:
    "G3c, INFRA-1253's comments on the text of another issue's node in each answer that draws it",
  PART_J: "G6, part J (INFRA-3939), the Spec view's rows",
  PART_M: "G6, part M (INFRA-3948), `address-target.ts`",
  PART_N: "G7, part N (INFRA-3940), the opened document",
  PART_U: "G6, part U (INFRA-4097), `DecisionsRow.tsx`",
  PART_X: "G6, part X (INFRA-4063), `NodeRow.tsx` and the Plan view's rows",
  PART_Y:
    "G6, part Y (INFRA-4066), the Verification view's rows, the block editor's caret and Write a criterion",
  REPLY_QUOTE: "G4, INFRA-5703's reply quote",
  THREADS: "G8, INFRA-5704's opened thread in Threads",
  TREE_ARTIFACT_ANCHOR: "G3a, INFRA-1253's anchor on the issue's `tree` artifact",
};

/** Records `reading` owed until `until`, which no line of the gate table may name yet. */
function owe(reading: string, until: string): void {
  test.info().annotations.push({ description: `${reading} until ${until}`, type: "owed" });
}

/** Whether every gate of `gates` is on this branch's base; where one is off, records `reading`
 *  owed until each gate that is off, and answers false. */
function onBase(reading: string, ...gates: readonly Gate[]): boolean {
  const off = gates.filter((gate) => !GATES[gate]);
  if (off.length > 0) owe(reading, off.map((gate) => GATE_WORDS[gate]).join("; and "));
  return off.length === 0;
}

/** The part that last reshapes each view's rows, whose gate brings the sheet's mount line there
 *  (G6, departure five). */
const SHEET_PART: Readonly<Record<View, Gate>> = {
  conversation: "PART_X",
  decisions: "PART_U",
  graph: "PART_X",
  plan: "PART_X",
  spec: "PART_J",
  verification: "PART_Y",
};

// ---------------------------------------------------------------------------------------------
// The views, the widths and what the page draws.
// ---------------------------------------------------------------------------------------------

/** The six views of R2, each of which draws the column beside the nodes it draws. */
const VIEWS = [
  "conversation",
  "spec",
  "verification",
  "plan",
  "decisions",
  "graph",
] as const satisfies readonly Preset[];
type View = (typeof VIEWS)[number];

/** The two outlines, which fold their branches: a row unfolds each before it reads them. */
const OUTLINES: Readonly<Record<View, boolean>> = {
  conversation: true,
  decisions: false,
  graph: true,
  plan: false,
  spec: false,
  verification: false,
};

/** `message` as a reading in `view` says it, so a failure names the view it read. */
function inView(view: View, message: string): string {
  return `${message}, in ${PRESET_NAMES[view]}`;
}

/** The widths (9) reads with the column drawn, widest first, and below 1024 px with none. */
const COLUMN_WIDTHS = [1920, 1440, 1280, 1279, 1100, 1024] as const;
const NO_COLUMN_WIDTHS = [1000, 768, 320] as const;

/** The highlight the column marks every thread's quoted words with (`highlights.ts`). */
const TEXT_COMMENT_HIGHLIGHT = "text-comment";

/** The style element that holds the highlights' rules (`highlights.ts`). */
const HIGHLIGHT_STYLE = "style[data-text-comment-highlights]";

/** The column of comments on text. */
function region(page: Page): Locator {
  return page.getByRole("region", { exact: true, name: MARGIN_WORDS.region });
}

/** The card of the thread whose root is `id`, in the column. */
function threadCard(page: Page, id: string): Locator {
  return region(page).locator(`[data-margin-item="${id}"]`);
}

/** The box the column places the thread whose root is `id` in. */
function threadBox(page: Page, id: string): Locator {
  return region(page).locator(`[data-text-thread="${id}"]`);
}

/** Every element the stage draws for the node whose tree id is `id`: its row in any view. */
function stageRow(page: Page, id: string): Locator {
  return page.getByTestId("issue-column").locator(`[data-node-id="${id}"]`);
}

/** The reply of the thread `root` the address names, as the card marks it. */
function namedReply(page: Page, root: string): Locator {
  return threadCard(page, root).locator('li[data-reply][aria-current="true"]');
}

// ---------------------------------------------------------------------------------------------
// The seeded threads and where they stand.
// ---------------------------------------------------------------------------------------------

/** One seeded thread on the words of a node, as a row reads its place. */
interface Spot {
  /** Its root's id, which its box and its card carry. */
  readonly id: string;
  /** The tree id of the node its words are in. */
  readonly node: string;
  /** Which match of `quote` in the node's words the page places it at, zero-based. */
  readonly occurrence: number;
  readonly quote: string;
  /** How many replies the seed wrote in it. */
  readonly replies: number;
  readonly what: string;
}

/** A seeded comment a row reads the place of: its id, the tree id of its node and its name. */
interface SpotRoot {
  readonly id: string;
  readonly node: string;
  readonly what: string;
}

/**
 * The spots of `roots`, each a seeded comment whose words are in a node of a document of the
 * issue, with the quote its anchor holds as the comments read answers it. A document anchor
 * stands at the first match of its quote until the tree read's anchor answers `occurrence` (G3b,
 * `text-threads.ts`), so the page places the comments the seed wrote on the second match at the
 * first until then.
 */
async function documentSpots(seeded: MarginSeed, roots: readonly SpotRoot[]): Promise<Spot[]> {
  const comments = await listComments(seeded.key);
  const slugs = new Map([
    [seeded.specArtifactId, seeded.specSlug],
    [seeded.verificationArtifactId, seeded.verificationSlug],
    [seeded.planArtifactId, seeded.planSlug],
  ]);
  return roots.map(({ id, node, what }) => {
    const anchor = comments.find((comment) => comment.id === id)?.anchor ?? null;
    const slug = anchor === null ? undefined : slugs.get(anchor.artifact_id);
    if (anchor === null || slug === undefined || anchor.block_id === null) {
      throw new Error(`the comments read of ${seeded.key} holds no document anchor of ${what}`);
    }
    // The generated contract declares no `occurrence` on a read anchor until G3b.
    const stored =
      "occurrence" in anchor && typeof anchor.occurrence === "number" ? anchor.occurrence : 0;
    return {
      id,
      node,
      occurrence: GATES.ANCHOR_OCCURRENCE ? stored : 0,
      quote: anchor.quote,
      replies: id === seeded.statementComment ? 1 : 0,
      what,
    };
  });
}

/** The seeded threads whose words are in a node of a document of the issue, each with the tree id
 *  of its node (the document's slug and the anchor's block): `documentSpots`. */
function threadSpots(seeded: MarginSeed): Promise<Spot[]> {
  // Each comment anchored to words of a typed block stores its inner paragraph's id in
  // `anchor.block_id`, not the block's own id; the view draws the typed block's node.
  return documentSpots(seeded, [
    ...seeded.threadsS1.map((id, at) => ({
      id,
      node: blockNode(seeded.specSlug, seeded.statementS1),
      what: `thread ${at + 1} on statement S1`,
    })),
    ...seeded.threadsS2.map((id, at) => ({
      id,
      node: blockNode(seeded.specSlug, seeded.statementS2),
      what: `thread ${at + 1} on statement S2`,
    })),
    {
      id: seeded.agentComment,
      node: blockNode(seeded.specSlug, seeded.agentStatement),
      what: "the agent's comment",
    },
    {
      id: seeded.resolvedThread,
      node: blockNode(seeded.specSlug, seeded.resolvedStatement),
      what: "the resolved thread",
    },
    {
      id: seeded.checkComment,
      node: blockNode(seeded.verificationSlug, seeded.check),
      what: "the comment on the check",
    },
    {
      id: seeded.planTaskComment,
      node: blockNode(seeded.planSlug, seeded.planTask),
      what: "the comment on the plan task",
    },
    {
      id: seeded.decisionComment,
      node: blockNode(seeded.specSlug, seeded.decision),
      what: "the comment on the decision",
    },
    {
      id: seeded.pagedChildThread,
      node: blockNode(seeded.specSlug, seeded.pagedChild55),
      what: "the comment on the 55th child",
    },
  ]);
}

/** The spot of the Change, part Q's comment on the words of the agent decision. */
async function changeSpot(seeded: MarginSeed): Promise<Spot> {
  const [change] = await documentSpots(seeded, [
    {
      id: seeded.change,
      node: blockNode(seeded.specSlug, seeded.agentDecision),
      what: "the Change",
    },
  ]);
  if (change === undefined) throw new Error("the seed wrote no Change");
  return change;
}

/** The seeded comments on the words of a node (G2): the lens on the resolved statement, its
 *  finding and its risk, the issue's constraint and the check's result, each by its name. */
function nodeComments(seeded: MarginSeed): { readonly id: string; readonly what: string }[] {
  return (
    [
      [seeded.lensComment, "the comment on the lens's verdict"],
      [seeded.findingComment, "the comment on the finding"],
      [seeded.riskComment, "the comment on the risk's heading"],
      [seeded.constraintComment, "the comment on the constraint's standard"],
      [seeded.resultComment, "the comment on the result's observed words"],
    ] as const
  ).map(([id, what]) => {
    if (id === null) throw new Error(`the seed wrote no ${what}`);
    return { id, what };
  });
}

/** A comment's node anchor as the server holds it (INFRA-1253 R31). */
type NodeAnchor = NonNullable<Result<"/api/v1/comments/{id}", "get">["comment"]["node_anchor"]>;

/** The node anchor the server holds for comment `id`: the comment read with `node_anchor=true`,
 *  which names the node its words are in by its id in the tree read. */
async function nodeAnchorOf(id: string): Promise<NodeAnchor> {
  const read = await readWithTrees<Result<"/api/v1/comments/{id}", "get">>(
    routeUrl("/api/v1/comments/{id}", { id }, { node_anchor: true })
  );
  const anchor = read.comment.node_anchor;
  if (anchor === undefined) throw new Error(`the server holds no node anchor of comment ${id}`);
  return anchor;
}

/** The tree id the page draws the node `readId` of Graph's read `answer` of issue `key` under
 *  (`tree-read/tree-nodes.ts`'s `treeIdOfNode`), which every view draws it under. */
function treeIdIn(answer: IssueTreeRead, key: string, readId: string, what: string): string {
  const node = answer.nodes?.find((one) => one.id === readId);
  if (node === undefined) throw new Error(`Graph's tree read of ${key} holds no node of ${what}`);
  return treeIdOfNode(node, key);
}

/** The spots of the seeded comments on the words of a node (`nodeComments`), each at the node and
 *  the match its node anchor names, which the page places it at from G2. */
async function nodeSpots(page: Page, seeded: MarginSeed): Promise<Spot[]> {
  const answer = await requestedTreeRead(page, seeded.key);
  return Promise.all(
    nodeComments(seeded).map(async ({ id, what }) => {
      const anchor = await nodeAnchorOf(id);
      return {
        id,
        node: treeIdIn(answer, seeded.key, anchor.node.id, what),
        occurrence: anchor.occurrence ?? 0,
        quote: anchor.quote,
        replies: 0,
        what,
      };
    })
  );
}

/** The threads whose nodes `view` draws for certain, at 1440 px, after an outline's branches are
 *  unfolded: the rest are read wherever the view draws their nodes. Graph draws every node of
 *  its read, the Change's decision and from G2 the nodes of the comments on a node's words among
 *  them, and Conversation the Change's decision. */
function mustDraw(seeded: MarginSeed, view: View): ReadonlySet<string> {
  const top = [...seeded.threadsS1, seeded.agentComment, seeded.resolvedThread];
  const both = [...top, ...seeded.threadsS2];
  const change = GATES.COMPOSER ? [seeded.change] : [];
  const nodes = GATES.NODE_ANCHOR ? nodeComments(seeded).map(({ id }) => id) : [];
  const ids: Readonly<Record<View, readonly string[]>> = {
    conversation: [...both, ...change],
    decisions: [seeded.decisionComment],
    graph: [...both, seeded.checkComment, seeded.planTaskComment, ...change, ...nodes],
    plan: [seeded.planTaskComment],
    spec: top,
    verification: [...seeded.threadsS1, seeded.checkComment],
  };
  return new Set(ids[view]);
}

/** A row each view draws on either seed, which a row waits for before it reads. On
 *  `seedNoThreads` Decisions draws no row of alice's decision, which no comment is on there; its
 *  review list holds the agent's decision. */
function anchorRow(page: Page, seeded: NoThreadsSeed, view: View): Locator {
  if (view === "plan") return stageRow(page, blockNode(seeded.planSlug, seeded.planTask)).first();
  if (view === "decisions") {
    return stageRow(page, blockNode(seeded.specSlug, seeded.decision))
      .or(stageRow(page, blockNode(seeded.specSlug, seeded.agentDecision)))
      .first();
  }
  return stageRow(page, blockNode(seeded.specSlug, seeded.statementS1)).first();
}

/** One box, as `getBoundingClientRect` measures it. */
interface Box {
  readonly bottom: number;
  readonly left: number;
  readonly right: number;
  readonly top: number;
}

/** What the page draws of one spot. */
interface SpotReading {
  /** Its box in the column, null where the column draws none, or hides it. */
  readonly box: Box | null;
  readonly id: string;
  /** Whether the stage draws a row of its node. */
  readonly rowDrawn: boolean;
  /** Whether a row of its node stands under the stored parent its box names. */
  readonly underParent: boolean;
  /** The first and the last line of its quoted words in its node's row, null where the row
   *  draws no such match. */
  readonly words: { readonly first: Box; readonly last: Box } | null;
}

interface SpotsReading {
  readonly mainRight: number;
  readonly spots: readonly SpotReading[];
}

/**
 * What the page draws of each of `spots`: its box in the column; the row of its node as the
 * column finds it (`TextMargin.tsx`'s `rowIn`: of two rows of one node, the one under the stored
 * parent the box names); and the lines of its quoted words in that row's words element
 * (`[data-node-words]`), found as the page's plain words read them, with what the row draws that
 * is no part of the node's text left out (`row-text-runs.ts`).
 */
function readSpots(page: Page, spots: readonly Spot[]): Promise<SpotsReading> {
  return page.evaluate(
    ({ asked, name }) => {
      const rect = (box: DOMRect): Box => ({
        bottom: box.bottom,
        left: box.left,
        right: box.right,
        top: box.top,
      });
      const stage = document.querySelector('[data-testid="issue-column"]');
      const column =
        [...document.querySelectorAll('[role="region"]')].find(
          (element) => element.getAttribute("aria-label") === name
        ) ?? null;
      const apart =
        '[contenteditable="false"], [data-page-chrome], [data-post-deleted], [data-node-trail], [hidden], [aria-hidden="true"], button, svg, style, script, textarea';
      const plain = (text: string) =>
        text
          .replace(/\u2019/gu, "'")
          .replace(/\s+/gu, " ")
          .trim();
      const parentOf = (row: Element) => row.getAttribute("data-row-key")?.split(">").at(-2);
      const wordsIn = (row: Element, quote: string, occurrence: number) => {
        const own = [...row.querySelectorAll("[data-node-words]")].filter(
          (words) => words.closest("[data-node-id]") === row
        );
        const root = own.length === 1 ? own[0] : row;
        const wanted = plain(quote);
        if (root === undefined || wanted === "") return null;
        const at: { readonly node: Text; readonly offset: number }[] = [];
        let text = "";
        const walker = document.createTreeWalker(root, NodeFilter.SHOW_TEXT);
        for (let node = walker.nextNode(); node !== null; node = walker.nextNode()) {
          const parent = node.parentElement;
          const hidden = parent?.closest(apart) ?? null;
          if (
            !(node instanceof Text) ||
            parent === null ||
            (hidden !== null && root.contains(hidden)) ||
            parent.closest("[data-node-id]") !== row
          ) {
            continue;
          }
          for (let index = 0; index < node.data.length; index += 1) {
            const character = node.data.charAt(index);
            const space = /\s/u.test(character);
            if (space && (text === "" || text.endsWith(" "))) continue;
            text += space ? " " : character === "\u2019" ? "'" : character;
            at.push({ node, offset: index });
          }
        }
        let from = -1;
        for (let match = 0; match <= occurrence; match += 1) {
          from = text.indexOf(wanted, from + 1);
          if (from === -1) return null;
        }
        const start = at[from];
        const end = at[from + wanted.length - 1];
        if (start === undefined || end === undefined) return null;
        const range = document.createRange();
        range.setStart(start.node, start.offset);
        range.setEnd(end.node, end.offset + 1);
        const lines = range.getClientRects();
        const first = lines.item(0);
        const last = lines.item(lines.length - 1);
        return first === null || last === null ? null : { first: rect(first), last: rect(last) };
      };
      return {
        mainRight: stage?.getBoundingClientRect().right ?? 0,
        spots: asked.map((spot) => {
          const box = column?.querySelector(`[data-text-thread="${CSS.escape(spot.id)}"]`) ?? null;
          const parent = box?.getAttribute("data-text-thread-parent") ?? null;
          const rows =
            stage === null
              ? []
              : [...stage.querySelectorAll(`[data-node-id="${CSS.escape(spot.node)}"]`)];
          const row =
            rows.length < 2 || parent === null
              ? rows[0]
              : (rows.find((one) => parentOf(one) === parent) ?? rows[0]);
          const shown =
            box instanceof HTMLElement &&
            !box.hidden &&
            getComputedStyle(box).visibility !== "hidden" &&
            box.getBoundingClientRect().height > 0
              ? box
              : null;
          return {
            box: shown === null ? null : rect(shown.getBoundingClientRect()),
            id: spot.id,
            rowDrawn: row !== undefined,
            underParent: parent !== null && rows.some((one) => parentOf(one) === parent),
            words: row === undefined ? null : wordsIn(row, spot.quote, spot.occurrence),
          };
        }),
      };
    },
    { asked: spots, name: MARGIN_WORDS.region }
  );
}

/** The name of spot `id`. */
function whatOf(spots: readonly Spot[], id: string): string {
  return spots.find((spot) => spot.id === id)?.what ?? id;
}

/** The spots the column must draw (`must`) or whose node the stage draws, that the column draws
 *  no box for. */
function undrawn(
  reading: SpotsReading,
  spots: readonly Spot[],
  must: ReadonlySet<string>
): string[] {
  return reading.spots.flatMap((spot) =>
    (must.has(spot.id) || spot.rowDrawn) && spot.box === null ? [whatOf(spots, spot.id)] : []
  );
}

/** Those spots whose box is not drawn at or right of the main column's right edge. */
function offMainColumn(
  reading: SpotsReading,
  spots: readonly Spot[],
  must: ReadonlySet<string>
): string[] {
  return reading.spots.flatMap((spot) =>
    !(must.has(spot.id) || spot.rowDrawn) ||
    (spot.box !== null && spot.box.left >= reading.mainRight - 0.5)
      ? []
      : [whatOf(spots, spot.id)]
  );
}

/** The drawn boxes of spots whose node's row the stage draws, top first. */
function drawnBoxes(
  reading: SpotsReading
): { readonly box: Box; readonly id: string; readonly words: SpotReading["words"] }[] {
  return reading.spots
    .flatMap((spot) =>
      spot.rowDrawn && spot.box !== null ? [{ box: spot.box, id: spot.id, words: spot.words }] : []
    )
    .sort((left, right) => left.box.top - right.box.top);
}

/**
 * The drawn threads whose box does not stand level with the first line of their quoted words in
 * their node's row (within 2 px), and, where the column stacks (`stacked`, G5), not 8 px or more
 * under the thread above it either; each with both tops.
 */
function offWords(reading: SpotsReading, spots: readonly Spot[], stacked: boolean): string[] {
  const failures: string[] = [];
  let above: Box | undefined;
  for (const { box, id, words } of drawnBoxes(reading)) {
    const level = words !== null && Math.abs(box.top - words.first.top) <= 2;
    const under = stacked && above !== undefined && box.top >= above.bottom + 8 - 0.5;
    if (!level && !under) {
      failures.push(
        `${whatOf(spots, id)}: its box at ${box.top}, its words at ${words?.first.top ?? "none"}`
      );
    }
    above = box;
  }
  return failures;
}

/** The drawn threads whose box covers the one above it. */
function covering(reading: SpotsReading, spots: readonly Spot[]): string[] {
  const failures: string[] = [];
  let above: { readonly box: Box; readonly id: string } | undefined;
  for (const drawn of drawnBoxes(reading)) {
    if (above !== undefined && drawn.box.top < above.box.bottom - 0.5) {
      failures.push(`${whatOf(spots, drawn.id)} covers ${whatOf(spots, above.id)}`);
    }
    above = drawn;
  }
  return failures;
}

// ---------------------------------------------------------------------------------------------
// Row 1's readings.
// ---------------------------------------------------------------------------------------------

/** What one card draws: its quoted words, whether it is expanded, and for its root and each
 *  reply whether it names its author beside its time. */
interface CardReading {
  readonly bylines: readonly boolean[];
  readonly expanded: string | null;
  readonly quote: string | null;
}

function readCard(card: Locator): Promise<CardReading> {
  return card.evaluate((element) => {
    const blocks = [element, ...element.querySelectorAll("li[data-reply]")];
    return {
      bylines: blocks.map((block) => {
        const time = block.querySelector("time[datetime]");
        const line = time?.parentElement?.textContent ?? "";
        return (
          time !== null &&
          line
            .replace(time.textContent ?? "", "")
            .replace(/·/gu, "")
            .trim() !== ""
        );
      }),
      expanded: element.getAttribute("aria-expanded"),
      quote: element.querySelector("blockquote")?.textContent?.trim() ?? null,
    };
  });
}

/** Whether the highlights' rules underline the quoted words, and the spots whose quoted words
 *  in their node's row the column's highlight holds no range of. */
function readMarks(
  page: Page,
  spots: readonly Spot[]
): Promise<{ readonly underline: boolean; readonly unmarked: readonly string[] }> {
  return page.evaluate(
    ({ asked, name, style }) => {
      const plain = (text: string) =>
        text
          .replace(/\u2019/gu, "'")
          .replace(/\s+/gu, " ")
          .trim();
      const ranges: Range[] = [];
      CSS.highlights.get(name)?.forEach((range) => {
        if (range instanceof Range) ranges.push(range);
      });
      const stage = document.querySelector('[data-testid="issue-column"]');
      const rules = document.querySelector(style)?.textContent ?? "";
      return {
        underline: new RegExp(`::highlight\\(${name}\\)\\s*\\{[^}]*underline`, "u").test(rules),
        unmarked: asked.flatMap((spot) => {
          const rows =
            stage === null
              ? []
              : [...stage.querySelectorAll(`[data-node-id="${CSS.escape(spot.node)}"]`)];
          return ranges.some(
            (range) =>
              plain(range.toString()) === plain(spot.quote) &&
              rows.some((row) => row.contains(range.commonAncestorContainer))
          )
            ? []
            : [spot.what];
        }),
      };
    },
    { asked: spots, name: TEXT_COMMENT_HIGHLIGHT, style: HIGHLIGHT_STYLE }
  );
}

/** Which of row 1's readings that wait on a gate are on this base: the threads of the replies
 *  that quote (G4), the stacking (G5), the Change's thread (G5) and the threads on a node's words
 *  (G2). */
interface RowOneGates {
  readonly change: boolean;
  readonly nodes: boolean;
  readonly quoted: boolean;
  readonly stacking: boolean;
}

/** (2): no comment of a thread is a row of `view`; from G5 nor is the Change, from G2 nor is a
 *  comment on a node's words, and once G4 is on the base, nor is a reply that quotes the post,
 *  the ask's part or the message, nor a reply in their threads. */
async function expectNoThreadRows(
  page: Page,
  seeded: MarginSeed,
  view: View,
  gates: RowOneGates
): Promise<void> {
  const quotedReplies = gates.quoted
    ? ([
        [seeded.postQuotedReply, "the reply that quotes the post is no row"],
        [seeded.postQuotedReplyFollowup, "the reply in the post's quoted thread is no row"],
        [seeded.askQuotedReply, "the reply that quotes the ask's part is no row"],
        [seeded.messageQuotedReply, "the reply that quotes the message is no row"],
      ] as const)
    : [];
  const nodeComment = gates.nodes
    ? nodeComments(seeded).map(({ id, what }) => [id, `${what} is no row`] as const)
    : [];
  const none: readonly (readonly [string | null, string])[] = [
    [seeded.statementComment, "the comment on the statement is no row"],
    [seeded.statementReply, "the reply in the statement's thread is no row"],
    [seeded.agentComment, "the agent's comment is no row"],
    [seeded.resolvedThread, "the resolved comment is no row"],
    [seeded.checkComment, "the comment on the check is no row"],
    [seeded.planTaskComment, "the comment on the plan task is no row"],
    [seeded.decisionComment, "the comment on the decision is no row"],
    ...(gates.change ? ([[seeded.change, "the Change is no row"]] as const) : []),
    ...nodeComment,
    ...quotedReplies,
  ];
  for (const [id, message] of none) {
    if (id === null) throw new Error(`the seed wrote no item for "${message}"`);
    await expect(
      issuePage(page).locator(`[role="treeitem"][data-node-id="${id}"]`),
      inView(view, message)
    ).toHaveCount(0);
  }
}

/** (2) in Conversation: the post on the issue is a row, with its reply and the reply that names a
 *  block of it with no quote under it, and it stands in no thread. */
async function expectPostRows(page: Page, seeded: MarginSeed): Promise<void> {
  await expect(
    nodeRow(page, seeded.post).first(),
    "the post on the issue is a row of Conversation"
  ).toBeVisible();
  await expect
    .poll(() => parentOfRow(page, seeded.postReply), {
      message: "the post's reply is a row under the post",
    })
    .toBe(seeded.post);
  await expect
    .poll(() => parentOfRow(page, seeded.postBlockReply), {
      message: "the reply that names a block of the post with no quote is a row under the post",
    })
    .toBe(seeded.post);
  await expect(threadBox(page, seeded.post), "the post stands in no thread").toHaveCount(0);
  await expect(
    threadBox(page, seeded.postReply),
    "the post's reply stands in no thread"
  ).toHaveCount(0);
}

/** (1) in `view`: each thread whose node the view draws stands in the column, at or right of the
 *  main column's right edge, level with its words; and, once the column stacks, no thread covers
 *  another. */
async function expectPlaced(
  page: Page,
  spots: readonly Spot[],
  must: ReadonlySet<string>,
  view: View,
  stacking: boolean
): Promise<void> {
  await expect
    .poll(async () => offMainColumn(await readSpots(page, spots), spots, must), {
      message: inView(view, "the thread stands right of the main column"),
    })
    .toEqual([]);
  await expect
    .poll(async () => offWords(await readSpots(page, spots), spots, stacking), {
      message: inView(
        view,
        "the thread stands level with its words, or 8 px or more under the thread above it"
      ),
    })
    .toEqual([]);
  if (stacking) {
    await expect
      .poll(async () => covering(await readSpots(page, spots), spots), {
        message: inView(view, "no thread covers another"),
      })
      .toEqual([]);
  }
}

/** (3) in `view`: each drawn thread's card draws its quoted words, its root and each reply whole
 *  with its author and time, expanded; the statement's thread its reply control and Resolve; the
 *  resolved thread Reopen; the agent's thread as any other; and the quoted words of each carry
 *  the underline mark. */
async function expectCards(
  page: Page,
  seeded: MarginSeed,
  spots: readonly Spot[],
  must: ReadonlySet<string>,
  view: View
): Promise<void> {
  // The column placed every drawn thread above, so one reading names the threads it draws.
  const reading = await readSpots(page, spots);
  const drawn = spots.filter((spot) =>
    reading.spots.some((one) => one.id === spot.id && one.rowDrawn && one.box !== null)
  );
  for (const spot of drawn) {
    const card = threadCard(page, spot.id);
    await expect(
      card.locator("li[data-reply]"),
      inView(view, `${spot.what} draws each reply`)
    ).toHaveCount(spot.replies);
    const read = await readCard(card);
    // Every quote the seed wrote is shorter than 80 characters, so the card draws it uncut;
    // `quote-line.test.ts` holds the cut at a word.
    expect(read.quote, inView(view, `${spot.what} draws its quoted words`)).toBe(spot.quote);
    expect(read.expanded, inView(view, `${spot.what} is not collapsed`)).toBe("true");
    expect(
      read.bylines,
      inView(view, `each comment of ${spot.what} draws its author and its time`)
    ).toEqual(Array.from({ length: spot.replies + 1 }, () => true));
    await assertWhole(card);
    for (let at = 0; at < spot.replies; at += 1) {
      await assertWhole(card.locator("li[data-reply]").nth(at));
    }
  }
  if (must.has(seeded.statementComment)) {
    const statement = threadCard(page, seeded.statementComment);
    await expect(
      statement.getByRole("textbox", { exact: true, name: "Reply" }),
      inView(view, "the statement's thread draws its reply control")
    ).toBeVisible();
    await expect(
      statement.getByRole("button", { exact: true, name: "Resolve" }),
      inView(view, "the statement's thread draws Resolve")
    ).toBeVisible();
  }
  if (must.has(seeded.resolvedThread)) {
    await expect(
      threadCard(page, seeded.resolvedThread).getByRole("button", { exact: true, name: "Reopen" }),
      inView(view, "the resolved thread is drawn, with Reopen")
    ).toBeVisible();
  }
  if (must.has(seeded.agentComment)) {
    await expect(
      threadCard(page, seeded.agentComment),
      inView(view, "the agent's thread is drawn in the column as alice's are")
    ).toBeVisible();
  }
  await expect
    .poll(async () => (await readMarks(page, drawn)).unmarked, {
      message: inView(view, "the quoted words of each thread carry the underline mark"),
    })
    .toEqual([]);
  expect(
    (await readMarks(page, drawn)).underline,
    inView(view, "the mark of the quoted words is an underline")
  ).toBe(true);
}

/** (4) in Graph: the check that serves two statements has its thread beside its row under its
 *  stored parent, and the column holds that thread once. */
async function expectCheckOnce(
  page: Page,
  seeded: MarginSeed,
  spots: readonly Spot[]
): Promise<void> {
  const check = spots.filter((spot) => spot.id === seeded.checkComment);
  await expect(
    threadBox(page, seeded.checkComment),
    "in Graph, the region holds the check's thread once"
  ).toHaveCount(1);
  await expect
    .poll(
      async () => {
        const [reading] = (await readSpots(page, check)).spots;
        return {
          beside:
            reading?.box != null &&
            reading.words !== null &&
            Math.abs(reading.box.top - reading.words.first.top) <= 2,
          underParent: reading?.underParent ?? false,
        };
      },
      { message: "in Graph, the check's thread stands beside its row under its stored parent" }
    )
    .toEqual({ beside: true, underParent: true });
}

/** Opens `view` of `key` at its own address and waits out its load. */
async function loadView(page: Page, traffic: ApiRequests, key: string, view: View): Promise<void> {
  traffic.reset();
  await gotoIssue(page, presetPath(key, view));
  await expectLoadSettled(traffic);
}

/** Opens `view` of `key` at its own address and waits out its load and, in an outline, unfolds
 *  every branch. */
async function openView(page: Page, traffic: ApiRequests, key: string, view: View): Promise<void> {
  await loadView(page, traffic, key, view);
  if (OUTLINES[view]) {
    await expectOpeningLanded(page);
    await unfoldEveryBranch(page);
  }
}

/** (1) to (4) at 1440 px in each view, (2) first: the base this part stands on already draws no
 *  row of a comment on text, so a run at the merge-base reads (2) and fails on (1). The Change's
 *  thread, where `change` holds it, is read in the two outlines, which draw the agent decision as
 *  a row: the Spec view draws it as a cell of part J's column of decisions, and Decisions draws
 *  the Change in part U's row (row 8). */
async function readEveryView(
  page: Page,
  traffic: ApiRequests,
  seeded: MarginSeed,
  spots: readonly Spot[],
  change: Spot | undefined,
  gates: RowOneGates
): Promise<void> {
  for (const view of VIEWS) {
    await openView(page, traffic, seeded.key, view);
    const must = mustDraw(seeded, view);
    const read = change === undefined || !OUTLINES[view] ? spots : [...spots, change];
    for (const spot of read) {
      if (!must.has(spot.id)) continue;
      await expect(
        stageRow(page, spot.node).first(),
        inView(view, `the view draws the row of the node of ${spot.what}`)
      ).toBeAttached();
    }
    await expectNoThreadRows(page, seeded, view, gates);
    if (view === "conversation") await expectPostRows(page, seeded);
    await expectPlaced(page, read, must, view, gates.stacking);
    await expectCards(page, seeded, read, must, view);
    if (view === "graph") await expectCheckOnce(page, seeded, read);
  }
}

/** (5) in Conversation at 1440 px, where S1's three long threads push S2's first thread under
 *  its words: with the focus on S2's row, S2's first thread stands level with its words (within
 *  2 px), and S1's last thread stands higher than it stood before the focus moved. */
async function readFocusedStack(
  page: Page,
  traffic: ApiRequests,
  seeded: MarginSeed,
  spots: readonly Spot[]
): Promise<void> {
  await page.setViewportSize(DESKTOP);
  await openView(page, traffic, seeded.key, "conversation");
  const last = spots.find((spot) => spot.id === seeded.threadsS1.at(-1));
  const first = spots.find((spot) => spot.id === seeded.threadsS2[0]);
  if (last === undefined || first === undefined) {
    throw new Error("the seed wrote no thread on S1 or on S2");
  }
  await expect
    .poll(async () => offWords(await readSpots(page, spots), spots, true), {
      message:
        "the threads stand at their words, or under the thread above, before the focus moves",
    })
    .toEqual([]);
  // The poll above waited until the column placed every thread, so one reading is S1's last.
  const [before] = (await readSpots(page, [last])).spots;
  const top = before?.box?.top;
  if (top === undefined) throw new Error("S1's last thread is not drawn before the focus moves");
  await focusRow(page, first.node);
  await expect
    .poll(
      async () => {
        const [s1, s2] = (await readSpots(page, [last, first])).spots;
        return {
          s1Higher: s1?.box != null && s1.box.top < top - 0.5,
          s2Level:
            s2?.box != null && s2.words !== null && Math.abs(s2.box.top - s2.words.first.top) <= 2,
        };
      },
      {
        message:
          "with the focus on S2's row, S2's first thread stands level with its words and S1's last thread higher than before",
      }
    )
    .toEqual({ s1Higher: true, s2Level: true });
}

/** The main column's left and right edges, in whole pixels. */
function mainColumnEdges(page: Page): Promise<{ readonly left: number; readonly right: number }> {
  return page.getByTestId("issue-column").evaluate((element) => {
    const box = element.getBoundingClientRect();
    return { left: Math.round(box.left), right: Math.round(box.right) };
  });
}

/** How many cards of the column are shown: not hidden, and not drawn `visibility: hidden`. */
function shownCards(page: Page): Promise<number> {
  return page.evaluate((name) => {
    const column = [...document.querySelectorAll('[role="region"]')].find(
      (element) => element.getAttribute("aria-label") === name
    );
    return [...(column?.querySelectorAll("[data-margin-item]") ?? [])].filter(
      (card) => card.closest("[hidden]") === null && getComputedStyle(card).visibility !== "hidden"
    ).length;
  }, MARGIN_WORDS.region);
}

/** The controls of the column that Tab can reach: each that stands inside no `inert` element. */
function reachableInColumn(page: Page): Promise<string[]> {
  return page.evaluate((name) => {
    const column = [...document.querySelectorAll('[role="region"]')].find(
      (element) => element.getAttribute("aria-label") === name
    );
    const stops =
      'a[href], button, textarea, input, select, [contenteditable="true"], [tabindex]:not([tabindex="-1"])';
    return [...(column?.querySelectorAll(stops) ?? [])]
      .filter((stop) => stop.closest("[inert]") === null)
      .map(
        (stop) => `${stop.tagName.toLowerCase()} "${(stop.textContent ?? "").trim().slice(0, 40)}"`
      );
  }, MARGIN_WORDS.region);
}

/** (6) while Details is open: the column draws nothing, Tab reaches none of its threads, and the
 *  main column's edges are those `column` measured before it opened. */
async function expectBlankWhileDetails(
  page: Page,
  column: { readonly left: number; readonly right: number },
  at: string
): Promise<void> {
  await expect
    .poll(() => shownCards(page), {
      message: `the region draws nothing while Details is open, ${at}`,
    })
    .toBe(0);
  expect(
    await reachableInColumn(page),
    `Tab reaches none of the region's threads while Details is open, ${at}`
  ).toEqual([]);
  expect(
    await mainColumnEdges(page),
    `the main column's left and right edges do not move while Details is open, ${at}`
  ).toEqual(column);
}

/** Drags Details' handle right past its narrowest, which leaves it 280 px wide. */
async function narrowDetails(page: Page): Promise<void> {
  // Details stands open above, so its handle is drawn and one measure of it is its place.
  const handle = await marginResizeHandle(page).boundingBox();
  if (handle === null) throw new Error("Details draws no handle to resize it by");
  const x = handle.x + handle.width / 2;
  const y = handle.y + handle.height / 2;
  await page.mouse.move(x, y);
  await page.mouse.down();
  await page.mouse.move(x + 300, y, { steps: 5 });
  await page.mouse.up();
  await expect
    .poll(async () => Math.round((await marginShell(page).boundingBox())?.width ?? 0), {
      message: "Details does not narrow to 280 px",
    })
    .toBe(280);
}

/** (6) in Conversation at 1440 px: Details at its default width and at 280 px, then closed; then
 *  the address of a reply in the statement's thread opened while Details is open. */
async function readDetails(
  page: Page,
  traffic: ApiRequests,
  seeded: MarginSeed,
  spots: readonly Spot[]
): Promise<void> {
  await page.setViewportSize(DESKTOP);
  await loadView(page, traffic, seeded.key, "conversation");
  const must = mustDraw(seeded, "conversation");
  const placed = spots.filter((spot) => must.has(spot.id));
  await expect
    .poll(async () => offWords(await readSpots(page, placed), placed, GATES.COMPOSER), {
      message: "the threads stand at their words before Details opens",
    })
    .toEqual([]);
  const column = await mainColumnEdges(page);
  await openDetails(page);
  await expectBlankWhileDetails(page, column, "at Details' default width");
  await narrowDetails(page);
  await expectBlankWhileDetails(page, column, "at 280 px");
  await closeDetails(page);
  await expect
    .poll(
      async () => {
        const reading = await readSpots(page, placed);
        return [...undrawn(reading, placed, must), ...offWords(reading, placed, GATES.COMPOSER)];
      },
      { message: "closing Details draws the threads again at their tops" }
    )
    .toEqual([]);
  await openDetails(page);
  await navigateInApp(
    page,
    buildIssuePath({ id: seeded.statementReply, key: seeded.key, kind: "comment" })
  );
  await expect(
    marginShell(page),
    "opening the address of a reply in the statement's thread closes Details"
  ).toBeHidden();
  await expect(
    namedReply(page, seeded.statementComment),
    "opening the address of a reply in the statement's thread with Details open focuses that reply"
  ).toBeFocused();
}

/** The three forms of Plan's chart: the phone step's cards, the narrow grid and the wide form. */
type PlanForm = "cards" | "narrow" | "wide";

const FORM_WORDS: Readonly<Record<PlanForm, string>> = {
  cards: "cards",
  narrow: "narrow grid",
  wide: "wide form",
};

/** Plan's chart as the page lays it out two frames after the last resize: its form, read off its
 *  step row (`PlanFilterBody.tsx`'s `CHART_COLUMNS` and `CHART_IN_COLUMN`), the width of the
 *  container its column steps read (the chart's parent, its content box), its right edge and the
 *  main column's; null while the view draws no chart. */
function planChart(page: Page): Promise<{
  readonly column: number;
  readonly form: string;
  readonly mainRight: number;
  readonly right: number;
} | null> {
  return page.evaluate(async () => {
    const { promise, resolve } = Promise.withResolvers<void>();
    requestAnimationFrame(() => requestAnimationFrame(() => resolve()));
    await promise;
    const chart = document.querySelector("[data-plan-chart]");
    const row = chart?.querySelector("[data-plan-bar-row]") ?? null;
    const container = chart?.parentElement ?? null;
    const main = document.querySelector('[data-testid="issue-column"]');
    if (chart === null || row === null || container === null || main === null) return null;
    // A step row is a flex box in the cards, and a grid whose first column names its form.
    const formOf = (style: CSSStyleDeclaration): string => {
      if (style.display === "flex") return "cards";
      const first = style.gridTemplateColumns.split(" ")[0];
      if (first === "330px") return "wide";
      if (first === "260px") return "narrow";
      return `${style.display} ${style.gridTemplateColumns}`;
    };
    const form = formOf(getComputedStyle(row));
    const box = getComputedStyle(container);
    const inset = ["borderLeftWidth", "borderRightWidth", "paddingLeft", "paddingRight"] as const;
    return {
      column:
        container.getBoundingClientRect().width -
        inset.reduce((sum, side) => sum + (Number.parseFloat(box[side]) || 0), 0),
      form,
      mainRight: main.getBoundingClientRect().right,
      right: chart.getBoundingClientRect().right,
    };
  });
}

/** The two forms of the Verification view: the phone step's cards and the table. */
type VerificationForm = "cards" | "table";

/** The Verification view as the page lays it out two frames after the last resize: its form, read
 *  off its head row and its first statement (`VerificationFilterBody.tsx`'s `TABLE`, `ROW` and
 *  `TABLE_IN_COLUMN`), and the width of the container its column step reads (the body's parent,
 *  the stage's column, its content box); null while the view draws no statement. */
function verificationForm(page: Page): Promise<{
  readonly column: number;
  readonly form: string;
} | null> {
  return page.evaluate(async () => {
    const { promise, resolve } = Promise.withResolvers<void>();
    requestAnimationFrame(() => requestAnimationFrame(() => resolve()));
    await promise;
    const body = document.querySelector("[data-verification-body]");
    const head = body?.querySelector("[data-verification-head]") ?? null;
    const card = body?.querySelector("[data-verification-card]") ?? null;
    const container = body?.parentElement ?? null;
    if (head === null || card === null || container === null) return null;
    // The table draws its head row as a grid and each statement as a band with square corners,
    // and the cards draw no head row and each statement as a card with 8 px corners.
    const display = getComputedStyle(head).display;
    const corner = getComputedStyle(card).borderTopLeftRadius;
    let form = `a ${display} head row over ${corner} corners`;
    if (display === "grid" && corner === "0px") form = "table";
    if (display === "none" && corner === "8px") form = "cards";
    const box = getComputedStyle(container);
    const inset = ["borderLeftWidth", "borderRightWidth", "paddingLeft", "paddingRight"] as const;
    return {
      column:
        container.getBoundingClientRect().width -
        inset.reduce((sum, side) => sum + (Number.parseFloat(box[side]) || 0), 0),
      form,
    };
  });
}

/** The narrowest window width from `from` up to `to`, where `read` answers `form` at `to` and not
 *  at `from`, at which it answers `form`, and the width of the column it measures there. */
async function narrowestColumn(
  page: Page,
  read: (page: Page) => Promise<{ readonly column: number; readonly form: string } | null>,
  form: PlanForm | VerificationForm,
  from: number,
  to: number
): Promise<{ readonly column: number; readonly width: number }> {
  let below = from;
  let at = to;
  while (at - below > 1) {
    const middle = Math.floor((below + at) / 2);
    await page.setViewportSize(viewportAt(middle));
    // The reading waits two frames after the resize, in which the view's steps apply, so one
    // answer says which form the view draws at this width.
    const drawn = await read(page);
    if (drawn?.form === form) at = middle;
    else below = middle;
  }
  await page.setViewportSize(viewportAt(at));
  // The search ended on this width, where the view drew this form two frames after the resize.
  const found = await read(page);
  return { column: found?.column ?? 0, width: at };
}

/** (8) with the threads drawn: Plan's chart at 1024, 1100, 1280, 1440 and 1920 px, and the
 *  narrowest columns of its wide form and its narrow grid, which the row records. */
async function readPlanSteps(page: Page, traffic: ApiRequests, seeded: MarginSeed): Promise<void> {
  await page.setViewportSize(DESKTOP);
  await loadView(page, traffic, seeded.key, "plan");
  await expect(
    threadBox(page, seeded.planTaskComment),
    "Plan draws the thread on its task in the region"
  ).toBeVisible();
  const steps: readonly (readonly [number, PlanForm])[] = [
    [1024, "cards"],
    [1100, "cards"],
    [1280, "narrow"],
    [1440, "narrow"],
    [1920, "wide"],
  ];
  for (const [width, form] of steps) {
    await page.setViewportSize(viewportAt(width));
    await expect
      .poll(async () => (await planChart(page))?.form ?? null, {
        message: `with a thread on the plan task, at ${width} px Plan's chart draws its ${FORM_WORDS[form]}`,
      })
      .toBe(form);
    if (form === "cards") {
      await expect
        .poll(
          async () => {
            const chart = await planChart(page);
            return chart !== null && chart.right <= chart.mainRight + 0.5;
          },
          {
            message: `at ${width} px Plan's chart's right edge is at or left of the main column's right edge`,
          }
        )
        .toBe(true);
    }
    await assertPageFits(page);
  }
  const narrow = await narrowestColumn(page, planChart, "narrow", 1024, 1279);
  const wide = await narrowestColumn(page, planChart, "wide", 1280, 1920);
  test.info().annotations.push(
    {
      description: `Plan's chart draws its wide form from a column of ${wide.column} px (a window of ${wide.width} px)`,
      type: "measured",
    },
    {
      description: `Plan's narrow grid holds down to a column of ${narrow.column} px (a window of ${narrow.width} px)`,
      type: "measured",
    }
  );
  expect(Math.round(wide.column), "Plan's chart draws its wide form from a column of 869 px").toBe(
    869
  );
  expect(
    Math.round(narrow.column),
    "Plan's narrow grid holds down to a column of about 482 px"
  ).toBe(482);
}

/** (8) with the threads drawn: the Verification view's cards at 1024 and 1100 px, each word inside
 *  its card, and the narrowest column at which it draws its table, which the row records. */
async function readVerificationSteps(
  page: Page,
  traffic: ApiRequests,
  seeded: MarginSeed
): Promise<void> {
  await page.setViewportSize(DESKTOP);
  await loadView(page, traffic, seeded.key, "verification");
  await expect(
    threadBox(page, seeded.statementComment),
    "Verification draws the thread on the statement in the region"
  ).toBeVisible();
  for (const width of [1024, 1100]) {
    await page.setViewportSize(viewportAt(width));
    await expect
      .poll(async () => Math.round((await region(page).boundingBox())?.width ?? 0), {
        message: inView("verification", `the region is 280 px wide at ${width} px`),
      })
      .toBe(280);
    await expect(
      verificationCardBoxes(page).first(),
      `the Verification view draws a statement at ${width} px`
    ).toBeVisible();
    // Part V's measure of the faults of the old tab: an element past a side of its card.
    await expect
      .poll(() => boxesPastTheirCard(page), {
        message: `no word of a Verification card reaches past its card at ${width} px`,
      })
      .toEqual([]);
    await expect
      .poll(async () => (await verificationForm(page))?.form ?? null, {
        message: `with a thread on a statement, at ${width} px the Verification view draws its cards`,
      })
      .toBe("cards");
    await assertPageFits(page);
  }
  // From 1280 px the column of comments on text is 320 px wide and the Contents pane 260 px, so
  // the main column is 588 px there, where the view draws its cards, and 748 px at 1440 px, where
  // it draws its table.
  const table = await narrowestColumn(page, verificationForm, "table", 1280, 1440);
  test.info().annotations.push({
    description: `The Verification view draws its table from a column of ${table.column} px (a window of ${table.width} px)`,
    type: "measured",
  });
  expect(
    Math.round(table.column),
    "the Verification view draws its table from a column of 656 px"
  ).toBe(656);
}

/**
 * The sheet readings of (9) below 1024 px in `view`, once its row site mounts the sheet (G6):
 * each thread whose node's row the stage draws stands in that row's own sheet
 * (`[data-text-threads]`), under the row's words and above its first child's row.
 */
async function expectSheets(
  page: Page,
  spots: readonly Spot[],
  view: View,
  width: number
): Promise<void> {
  const misplaced = () =>
    page.evaluate((asked) => {
      const stage = document.querySelector('[data-testid="issue-column"]');
      const rows = stage === null ? [] : [...stage.querySelectorAll("[data-node-row]")];
      return asked.flatMap((spot) => {
        const row = stage?.querySelector(`[data-node-id="${CSS.escape(spot.node)}"]`) ?? null;
        if (row === null) return [];
        const card = row.querySelector(
          `[data-text-threads] [data-margin-item="${CSS.escape(spot.id)}"]`
        );
        const sheet = card?.closest("[data-text-threads]") ?? null;
        if (sheet === null || sheet.closest("[data-node-id]") !== row) {
          return [`${spot.what} stands in no sheet of its node's row`];
        }
        const words = [...row.querySelectorAll("[data-node-words]")].find(
          (one) => one.closest("[data-node-id]") === row
        );
        const level = Number(row.getAttribute("aria-level") ?? "0");
        const nested = [...row.querySelectorAll("[data-node-row]")].find((one) => one !== row);
        const next = rows[rows.indexOf(row) + 1];
        const child =
          nested ??
          (next !== undefined && Number(next.getAttribute("aria-level") ?? "0") > level
            ? next
            : undefined);
        const box = sheet.getBoundingClientRect();
        const failures: string[] = [];
        if (words !== undefined && box.top < words.getBoundingClientRect().bottom - 0.5) {
          failures.push(`${spot.what} stands above its node's text`);
        }
        if (child !== undefined && box.bottom > child.getBoundingClientRect().top + 0.5) {
          failures.push(`${spot.what} stands under the first child's row`);
        }
        return failures;
      });
    }, spots);
  await expect
    .poll(misplaced, {
      message: inView(
        view,
        `at ${width} px the thread stands inside its node's row, under its text, and the thread stands above the first child's row`
      ),
    })
    .toEqual([]);
}

/** (9) on `chromium`: in each view, at each width from 1024 px the column's width, the page's
 *  fit and each thread whole; below it no column, the page's fit, and the sheets once G6 is on
 *  the base. */
async function readWidths(
  page: Page,
  traffic: ApiRequests,
  seeded: MarginSeed,
  spots: readonly Spot[]
): Promise<void> {
  for (const view of VIEWS) {
    await page.setViewportSize(DESKTOP);
    await loadView(page, traffic, seeded.key, view);
    for (const width of COLUMN_WIDTHS) {
      await page.setViewportSize(viewportAt(width));
      // The column is 320 px wide from 1280 px, and 280 px below (R2).
      const wanted = width >= 1280 ? 320 : 280;
      await expect
        .poll(async () => Math.round((await region(page).boundingBox())?.width ?? 0), {
          message: inView(view, `the region is ${wanted} px wide at ${width} px`),
        })
        .toBe(wanted);
      await assertPageFits(page);
      // The column stands drawn at this width above, so one reading names the threads it draws.
      const reading = await readSpots(page, spots);
      for (const spot of reading.spots) {
        if (spot.rowDrawn && spot.box !== null) await assertWhole(threadCard(page, spot.id));
      }
    }
    for (const width of NO_COLUMN_WIDTHS) {
      await page.setViewportSize(viewportAt(width));
      await expect(region(page), inView(view, `no region is drawn at ${width} px`)).toHaveCount(0);
      await assertPageFits(page);
      if (GATES[SHEET_PART[view]]) await expectSheets(page, spots, view, width);
    }
  }
}

/** (9) on `iphone` at 390 px: in each view no column, the page's fit, and the sheets once G6 is
 *  on the base. */
async function readPhone(
  page: Page,
  traffic: ApiRequests,
  seeded: MarginSeed,
  spots: readonly Spot[]
): Promise<void> {
  await page.setViewportSize(PHONE);
  for (const view of VIEWS) {
    await loadView(page, traffic, seeded.key, view);
    await expect(
      anchorRow(page, seeded, view),
      inView(view, "the view draws its rows at 390 px")
    ).toBeAttached();
    await expect(region(page), inView(view, "at 390 px no region is drawn")).toHaveCount(0);
    await assertPageFits(page);
    if (GATES[SHEET_PART[view]]) await expectSheets(page, spots, view, PHONE.width);
  }
}

/** The main column's width and what the window leaves it beside the rail and the Contents pane,
 *  in whole pixels. */
function columnRoom(page: Page): Promise<{ readonly column: number; readonly room: number }> {
  return page.evaluate(() => {
    const width = (selector: string) =>
      document.querySelector(selector)?.getBoundingClientRect().width ?? 0;
    return {
      column: Math.round(width('[data-testid="issue-column"]')),
      room: Math.round(
        document.documentElement.clientWidth -
          width('[data-testid="icon-rail"]') -
          width('[data-testid="issue-contents"]')
      ),
    };
  });
}

/** (7) and (8) on `seedNoThreads`: no column in any view, the main column as wide as the window
 *  less the rail and the pane, and Plan's chart in the form it draws at this part's base. */
async function readNoThreads(page: Page, seeded: NoThreadsSeed): Promise<void> {
  const traffic = watchApiRequests(page);
  for (const view of VIEWS) {
    await loadView(page, traffic, seeded.key, view);
    await expect(
      anchorRow(page, seeded, view),
      inView(view, "the view draws its rows on seedNoThreads")
    ).toBeAttached();
    await expect(region(page), inView(view, "on seedNoThreads no region is drawn")).toHaveCount(0);
    const { column, room } = await columnRoom(page);
    expect(
      column,
      inView(
        view,
        "on seedNoThreads issue-column is as wide as the window less the rail and the Contents pane"
      )
    ).toBe(room);
  }
  await loadView(page, traffic, seeded.key, "plan");
  // At this part's base the chart steps by the window alone: below 761 px its cards, below
  // 1241 px its narrow grid (`PlanFilterBody.tsx`'s `max-[761px]:` and `max-[1241px]:`).
  const base: readonly (readonly [number, PlanForm])[] = [
    [760, "cards"],
    [761, "narrow"],
    [1024, "narrow"],
    [1240, "narrow"],
    [1241, "wide"],
    [1440, "wide"],
  ];
  for (const [width, form] of base) {
    await page.setViewportSize(viewportAt(width));
    await expect
      .poll(async () => ({ form: (await planChart(page))?.form ?? null, width }), {
        message: `Plan draws its ${FORM_WORDS[form]} as at the base`,
      })
      .toEqual({ form, width });
    await assertPageFits(page);
  }
}

// ---------------------------------------------------------------------------------------------
// Row 1.
// ---------------------------------------------------------------------------------------------

// c-margin.
// SLOW(>=2s): the whole seeded issue read in six views at 1440 px with every outline branch
// opened, Conversation again with the focus moved to a row, Details opened twice and resized,
// Plan's chart resized through two searches of its steps and the Verification view through one,
// each view resized through nine widths, then the issue with no thread read again in six views
// and at six widths of Plan; on `iphone`, six views at 390 px.
test("a comment on text stands beside its node in every view @phone", async ({ browser }) => {
  test.setTimeout(900_000);
  const phone = test.info().project.name === "iphone";
  onBase(
    "(9) below 1024 px each thread inside its node's row, under its text and above its first child's row, and assertWhole for each thread there",
    "PART_X",
    "PART_U",
    "PART_J",
    "PART_Y"
  );
  const stacking = onBase(
    "(1) the stacking: no thread covers another, and a thread under another stands 8 px or more under it; and (5) with the focus on S2's row, S2's first thread level with its words and S1's last thread higher than before",
    "COMPOSER"
  );
  const quoted = onBase(
    "(2) none of the replies that quote the post, the ask's part and the message, nor a reply in their threads, is a row",
    "REPLY_QUOTE"
  );
  onBase(
    "(1) and (3) the threads of the replies that quote the post, the ask's part and the message: their place, their cards, and the message thread's reply control with no Resolve",
    "REPLY_QUOTE",
    "COMPOSER"
  );
  const deleted = onBase(
    "(3) after the API deletes the post, the reply that quoted it is a row under the post's deleted row and stands in no thread",
    "REPLY_QUOTE"
  );
  const nodes = onBase(
    "(1) and (3) the threads on the words of the lens, the finding, the risk, the constraint and the result",
    "NODE_ANCHOR"
  );
  const changeThread = onBase(
    "(1) and (2) the Change's thread beside the agent decision in Conversation and in Graph, and the Change as no row",
    "COMPOSER"
  );
  onBase(
    "(1) the Change's thread beside the agent decision's cell in the Spec view's column of decisions",
    "COMPOSER",
    "PART_J"
  );
  onBase(
    "(4) in Conversation and in Graph, the comment on the restating decision block's words stands in the region beside the ask that block restates, with the block's words as its quote, and no row draws it (decision 513)",
    "PART_U"
  );
  onBase(
    "(1) and (3) the place and the mark of the comments on the second of two equal words of the check, the plan task and the decision (until then the page places each at the first)",
    "ANCHOR_OCCURRENCE"
  );
  owe(
    "(3) the orphaned thread stands at its node's first line and draws ThreadCard's notice on an orphaned anchor",
    "INFRA-1253's `anchor.container_node_id` is on the base, which no line of the gate table holds yet: at this base the server hangs an orphaned document anchor under the issue and no read maps its paragraph to `r-orphan`, so the comment stays a row (dispatch://INFRA-5706/comment/27810bd6-9b41-40cc-9f9f-a93bd87506de, dispatch://INFRA-5706/comment/b02e28b0-c3ba-4af6-91ac-b9a6196fbb18)"
  );
  onBase(
    "(8) the Spec view: its decisions above the statements at 1280 px, beside them at 1440 px, its wide form at 1920 px, its form at the base on seedNoThreads, and the narrowest column of its two columns, 672 px",
    "PART_J"
  );
  await phase(browser, seedMarginIssue, async (page, seeded) => {
    const spots = [...(await threadSpots(seeded)), ...(nodes ? await nodeSpots(page, seeded) : [])];
    const change = changeThread ? await changeSpot(seeded) : undefined;
    const traffic = watchApiRequests(page);
    if (phone) {
      await readPhone(page, traffic, seeded, spots);
      return;
    }
    await readEveryView(page, traffic, seeded, spots, change, {
      change: changeThread,
      nodes,
      quoted,
      stacking,
    });
    if (stacking) await readFocusedStack(page, traffic, seeded, spots);
    await readDetails(page, traffic, seeded, spots);
    await readPlanSteps(page, traffic, seeded);
    await readVerificationSteps(page, traffic, seeded);
    await readWidths(page, traffic, seeded, spots);
    if (deleted) {
      const reply = seeded.postQuotedReply;
      if (reply === null) throw new Error("the seed wrote no reply that quotes the post");
      await deleteComment(seeded.post);
      await page.setViewportSize(DESKTOP);
      await openView(page, traffic, seeded.key, "conversation");
      await expect
        .poll(() => parentOfRow(page, reply), {
          message:
            "after the API deletes the post, the reply that quoted it is a row under the post's deleted row",
        })
        .toBe(seeded.post);
      await expect(
        threadBox(page, reply),
        "after the API deletes the post, the reply that quoted it stands in no thread"
      ).toHaveCount(0);
    }
  });
  if (phone) return;
  await phase(browser, seedNoThreads, readNoThreads);
});

// ---------------------------------------------------------------------------------------------
// Rows 2 and 3.
// ---------------------------------------------------------------------------------------------

/** The members of a reply's `reply_block` that a build storing no quote leaves out (G4). */
const QUOTE_MEMBERS: Readonly<Record<string, true>> = { occurrence: true, quote: true };

/** `value` with `QUOTE_MEMBERS` left out of the `reply_block` of every object in it whose `id`
 *  is one of `ids`. */
function withoutQuote(value: unknown, ids: ReadonlySet<string>): unknown {
  if (Array.isArray(value)) return value.map((one) => withoutQuote(one, ids));
  if (value === null || typeof value !== "object") return value;
  const entries = Object.entries(value);
  const own = entries.find(([name]) => name === "id")?.[1];
  return Object.fromEntries(
    entries.map(([name, member]) =>
      name === "reply_block" &&
      typeof own === "string" &&
      ids.has(own) &&
      member !== null &&
      typeof member === "object"
        ? [
            name,
            Object.fromEntries(
              Object.entries(member).filter(([field]) => QUOTE_MEMBERS[field] !== true)
            ),
          ]
        : [name, withoutQuote(member, ids)]
    )
  );
}

/**
 * Answers each reply the page writes with its real 201 less `reply_block.quote` and `occurrence`,
 * and strips the same two members from that reply in every later comments read, messages read
 * and tree read the page makes, as a build that stores no quote answers them (Playwright routes
 * over the real answers). Answers the ids of the replies it stripped.
 */
async function keepNoQuote(page: Page): Promise<ReadonlySet<string>> {
  const stripped = new Set<string>();
  const routes = /^\/api\/v1\/issues\/[^/]+\/(comments|messages|tree)$/u;
  await page.route(
    (url) => routes.test(url.pathname),
    async (route) => {
      const request = route.request();
      const response = await route.fetch();
      if (response.status() !== 200 && response.status() !== 201) {
        await route.fulfill({ response });
        return;
      }
      const body: unknown = await response.json();
      const written =
        request.method() === "POST" && typeof body === "object" && body !== null && "id" in body
          ? body.id
          : null;
      if (typeof written === "string") stripped.add(written);
      await route.fulfill({ json: withoutQuote(body, stripped), response });
    }
  );
  return stripped;
}

/** The first and the last line of a match of words, as `readSpots` measures them. */
type WordsBox = NonNullable<SpotReading["words"]>;

/** Scrolls the first row of `node` to the middle of the window at once, so a pointer reaches its
 *  words and the rows beside it. */
async function showRow(page: Page, node: string): Promise<void> {
  await stageRow(page, node)
    .first()
    .evaluate((row) => row.scrollIntoView({ behavior: "instant", block: "center" }));
}

/** The lines of match `occurrence` of `quote` in the words of the row of `node`, where the window
 *  shows them now. */
async function wordsAt(
  page: Page,
  node: string,
  quote: string,
  occurrence: number
): Promise<WordsBox> {
  // The view's load has settled before this, so one reading finds the words to select.
  const [reading] = (
    await readSpots(page, [{ id: "", node, occurrence, quote, replies: 0, what: quote }])
  ).spots;
  const words = reading?.words ?? null;
  if (words === null) {
    throw new Error(`the row of ${node} draws no match ${occurrence} of ${quote}`);
  }
  return words;
}

/** Presses at the left of the first character of `from` and drags to the right of the last
 *  character of `to`, as a pointer selects words. */
async function dragAcross(page: Page, from: WordsBox, to: WordsBox): Promise<void> {
  await page.mouse.move(from.first.left + 1, (from.first.top + from.first.bottom) / 2);
  await page.mouse.down();
  await page.mouse.move(to.last.right - 1, (to.last.top + to.last.bottom) / 2, { steps: 4 });
  await page.mouse.up();
}

/** Selects match `occurrence` of `quote` in the words of the row of `node` by pointer, its row
 *  brought into the window first: a press at its first character's left and a drag to its last
 *  character's right. */
async function selectWords(
  page: Page,
  node: string,
  quote: string,
  occurrence: number
): Promise<void> {
  await showRow(page, node);
  const words = await wordsAt(page, node, quote, occurrence);
  await dragAcross(page, words, words);
}

/** The lines of the first match of `quote` in the element `line` locates - a line a row draws
 *  under its words - scrolled to the middle of the window first. */
async function lineWords(line: Locator, quote: string): Promise<WordsBox> {
  // The row that holds the line is drawn above, so one reading of the line finds its words.
  const words = await line.first().evaluate((element, wanted) => {
    element.scrollIntoView({ behavior: "instant", block: "center" });
    const rect = (box: DOMRect) => ({
      bottom: box.bottom,
      left: box.left,
      right: box.right,
      top: box.top,
    });
    const walker = document.createTreeWalker(element, NodeFilter.SHOW_TEXT);
    for (let node = walker.nextNode(); node !== null; node = walker.nextNode()) {
      const at = node.textContent?.indexOf(wanted) ?? -1;
      if (at === -1) continue;
      const range = document.createRange();
      range.setStart(node, at);
      range.setEnd(node, at + wanted.length);
      const lines = range.getClientRects();
      const first = lines.item(0);
      const last = lines.item(lines.length - 1);
      return first === null || last === null ? null : { first: rect(first), last: rect(last) };
    }
    return null;
  }, quote);
  if (words === null) throw new Error(`the line draws no ${quote}`);
  return words;
}

/** The control "Comment" that stands beside a selection's end (`SelectionComment.tsx`). */
function commentControl(page: Page): Locator {
  return page.getByRole("button", { exact: true, name: MARGIN_WORDS.comment });
}

/** The box a composer of this part stands in, in the column or in a node's sheet. */
function composerBox(page: Page): Locator {
  return page.locator("[data-text-composer]");
}

/** The field of the composer of this part. */
function composerField(page: Page): Locator {
  return composerBox(page).getByRole("textbox").first();
}

/** Whether `response` answers a write of a comment on an issue. */
function commentWrite(response: Response): boolean {
  return (
    response.request().method() === "POST" &&
    /^\/api\/v1\/issues\/[^/]+\/comments$/u.test(new URL(response.url()).pathname)
  );
}

/** Waits out three animation frames: the control reads the selection a pointer left at most once
 *  in a frame (`SelectionComment.tsx`), and draws what it read in the next. */
function framesPass(page: Page): Promise<void> {
  return page.evaluate(async () => {
    for (let frame = 0; frame < 3; frame += 1) {
      const { promise, resolve } = Promise.withResolvers<void>();
      requestAnimationFrame(() => resolve());
      await promise;
    }
  });
}

/** What a send from the composer wrote: the members of the `anchor` its request carried, the id
 *  Dispatch's answer names, and the answer's status. */
interface Sent {
  readonly anchor: Readonly<Record<string, unknown>>;
  readonly id: string;
  readonly status: number;
}

/** Presses the Comment that stands beside the selection `at` names, writes `words` in the
 *  composer it opens and sends them, and answers what the send wrote. */
async function sendFromControl(page: Page, words: string, at: string): Promise<Sent> {
  const control = commentControl(page);
  await expect(control, `Comment stands beside the selection ${at}`).toBeVisible();
  await control.click();
  const field = composerField(page);
  await field.fill(words);
  const written = page.waitForResponse(commentWrite);
  await field.press("ControlOrMeta+Enter");
  const response = await written;
  const body: unknown = response.request().postDataJSON();
  const answer: unknown = await response.json();
  return {
    anchor:
      typeof body === "object" &&
      body !== null &&
      "anchor" in body &&
      typeof body.anchor === "object" &&
      body.anchor !== null
        ? Object.fromEntries(Object.entries(body.anchor))
        : {},
    id: typeof answer === "object" && answer !== null && "id" in answer ? String(answer.id) : "",
    status: response.status(),
  };
}

/** (3) after a send: the thread of `spot` stands in the region beside its node, right of the main
 *  column and level with its words, with the focus on its root (R5). */
async function expectLanded(page: Page, spot: Spot, view: View): Promise<void> {
  await expect(
    threadBox(page, spot.id),
    inView(view, `the thread on ${spot.what} stands beside its node`)
  ).toHaveAttribute("data-text-thread-node", spot.node);
  await expect(
    threadCard(page, spot.id),
    inView(view, `the focus is on the root of the thread on ${spot.what}`)
  ).toBeFocused();
  await expect
    .poll(
      async () => {
        const reading = await readSpots(page, [spot]);
        const [drawn] = reading.spots;
        return {
          level:
            drawn?.box != null &&
            drawn.words !== null &&
            Math.abs(drawn.box.top - drawn.words.first.top) <= 2,
          right: drawn?.box != null && drawn.box.left >= reading.mainRight - 0.5,
        };
      },
      {
        message: inView(
          view,
          `the thread on ${spot.what} stands right of the main column, level with its words`
        ),
      }
    )
    .toEqual({ level: true, right: true });
}

/** A block row 2 comments on, in the view that draws it: its node, its document and its block,
 *  the words whose second match it selects, and the seeded comment on the same text of the block,
 *  whose stored anchor names the paragraph the server stores the new one in. */
interface BlockWrite {
  readonly artifact: string;
  readonly block: string;
  readonly node: string;
  readonly quote: string;
  readonly seededOn: string;
  readonly view: View;
  readonly what: string;
}

/** (1)'s blocks: a statement under Spec, a check under Verification, a plan task under Plan and a
 *  decision under Decisions. Each text holds its quote twice: "A reader finds the first note and
 *  the last marker in order.", "Check the placement before and the placement after a reply.",
 *  "Build the column and keep the column beside the text." and "Keep the space between cards and
 *  the space between rows." (`e2e/issue-page-margin.ts`). */
function blockWrites(seeded: MarginSeed): BlockWrite[] {
  const [onS2] = seeded.threadsS2;
  if (onS2 === undefined) throw new Error("the seed wrote no thread on statement S2");
  return [
    {
      artifact: seeded.specArtifactId,
      block: seeded.statementS2,
      node: blockNode(seeded.specSlug, seeded.statementS2),
      quote: "the",
      seededOn: onS2,
      view: "spec",
      what: "statement",
    },
    {
      artifact: seeded.verificationArtifactId,
      block: seeded.check,
      node: blockNode(seeded.verificationSlug, seeded.check),
      quote: "placement",
      seededOn: seeded.checkComment,
      view: "verification",
      what: "check",
    },
    {
      artifact: seeded.planArtifactId,
      block: seeded.planTask,
      node: blockNode(seeded.planSlug, seeded.planTask),
      quote: "column",
      seededOn: seeded.planTaskComment,
      view: "plan",
      what: "plan task",
    },
    {
      artifact: seeded.specArtifactId,
      block: seeded.decision,
      node: blockNode(seeded.specSlug, seeded.decision),
      quote: "between",
      seededOn: seeded.decisionComment,
      view: "decisions",
      what: "decision",
    },
  ];
}

/** (1) to (3) on each block of `blockWrites`, in its own view: the selection's Comment, the send,
 *  the write's anchor and the anchor the server stored, and the thread beside the block with the
 *  focus on its root. A document anchor stands at the first match of its quote until G3b. */
async function writeOnBlocks(page: Page, traffic: ApiRequests, seeded: MarginSeed): Promise<void> {
  for (const target of blockWrites(seeded)) {
    const { view, what } = target;
    await openView(page, traffic, seeded.key, view);
    await selectWords(page, target.node, target.quote, 1);
    const sent = await sendFromControl(
      page,
      `A comment on words of the ${what}.`,
      inView(view, `in the ${what}`)
    );
    expect(sent.status, inView(view, `the comment on the ${what} is written`)).toBe(201);
    expect(
      sent.anchor.artifact,
      inView(view, `the ${what}'s anchor names its document's artifact`)
    ).toBe(target.artifact);
    expect(sent.anchor.block_id, inView(view, `the ${what}'s anchor carries block_id`)).toBe(
      target.block
    );
    expect(
      { occurrence: sent.anchor.occurrence, quote: sent.anchor.quote },
      inView(view, `the comment on the ${what} is written on the second match of its words`)
    ).toEqual({ occurrence: 1, quote: target.quote });
    const [stored, beside] = await Promise.all([getComment(sent.id), getComment(target.seededOn)]);
    const paragraph = beside.comment.anchor?.block_id;
    if (typeof paragraph !== "string") {
      throw new Error(`the seed's comment on the ${what} stores no block`);
    }
    expect(
      { block_id: stored.comment.anchor?.block_id, quote: stored.comment.anchor?.quote },
      inView(
        view,
        `the server stores the comment on the ${what} in its text, on the selected words`
      )
    ).toEqual({ block_id: paragraph, quote: target.quote });
    await expectLanded(
      page,
      {
        id: sent.id,
        node: target.node,
        occurrence: GATES.ANCHOR_OCCURRENCE ? 1 : 0,
        quote: target.quote,
        replies: 0,
        what: `the ${what}`,
      },
      view
    );
  }
}

/** The read id of the finding under the lens on the resolved statement, in Graph's read
 *  `answer`: the node under that lens that holds the finding's block. */
function rowFindingOf(seeded: MarginSeed, answer: IssueTreeRead): string {
  const finding = answer.nodes?.find(
    (node) => node.parent?.id === seeded.rowLens && node.block_id === seeded.rowFindingBlock
  );
  if (finding === undefined) {
    throw new Error(`Graph's tree read of ${seeded.key} holds no finding of the statement's lens`);
  }
  return finding.id;
}

/** A node row 2 comments on in Graph: its id in the tree read, the words whose second match it
 *  selects, and its name. */
interface NodeWrite {
  readonly quote: string;
  readonly read: string;
  readonly what: string;
}

/** (1)'s nodes in Graph, each holding its quote twice: the verdict of the lens on the resolved
 *  statement, its finding and its risk's heading (`seedReasoning`), the standard of the issue's
 *  constraint, and the observed words of the check's result. */
function nodeWrites(seeded: MarginSeed, answer: IssueTreeRead): NodeWrite[] {
  return [
    { quote: "note", read: seeded.rowLens, what: "lens's verdict" },
    { quote: "line", read: rowFindingOf(seeded, answer), what: "finding" },
    { quote: "reply", read: seeded.rowRisk, what: "risk's heading" },
    { quote: "standard", read: seeded.constraint, what: "constraint's standard" },
    { quote: "reading", read: seeded.result, what: "result's observed words" },
  ];
}

/** (1) to (3) on each node of `nodeWrites` in Graph, whose read is `answer`: the selection's
 *  Comment, the send, the write's `anchor.node` and the node anchor the server stored, the thread
 *  beside the node with the focus on its root; then, after a reload, the thread beside its node
 *  in every view that draws the node, Graph among them. */
async function writeOnNodes(
  page: Page,
  traffic: ApiRequests,
  seeded: MarginSeed,
  answer: IssueTreeRead
): Promise<void> {
  await openView(page, traffic, seeded.key, "graph");
  const written: Spot[] = [];
  for (const target of nodeWrites(seeded, answer)) {
    const { what } = target;
    const node = treeIdIn(answer, seeded.key, target.read, `the ${what}`);
    await selectWords(page, node, target.quote, 1);
    const sent = await sendFromControl(
      page,
      `A comment on words of the ${what}.`,
      inView("graph", `in the ${what}`)
    );
    expect(sent.status, inView("graph", `the comment on the ${what} is written`)).toBe(201);
    expect(
      sent.anchor,
      inView("graph", `the comment on the ${what} is written on its node, on the second match`)
    ).toEqual({ node: expect.stringContaining(target.read), occurrence: 1, quote: target.quote });
    const stored = await nodeAnchorOf(sent.id);
    expect(
      { node: stored.node.id, occurrence: stored.occurrence, quote: stored.quote },
      inView("graph", `the comment on the ${what} stores occurrence 1 of its words on its node`)
    ).toEqual({ node: target.read, occurrence: 1, quote: target.quote });
    const spot = { id: sent.id, node, occurrence: 1, quote: target.quote, replies: 0, what };
    await expectLanded(page, { ...spot, what: `the ${what}` }, "graph");
    written.push({ ...spot, what: `the thread written on the ${what}` });
  }
  const known = [...(await threadSpots(seeded)), ...(await nodeSpots(page, seeded))];
  for (const view of VIEWS) {
    await openView(page, traffic, seeded.key, view);
    const must = new Set(view === "graph" ? written.map(({ id }) => id) : []);
    await expectPlaced(page, [...known, ...written], must, view, true);
  }
}

/**
 * (4) in Graph, whose read is `answer`: no control stands on a selection across two rows, after
 * which `$mod+Alt+m` writes nothing and opens no composer; none on a risk's early warning line;
 * none on the author-only node; and none on the child issue's row. Before each, a selection in
 * a row beside it takes the control, so the control can stand there, and is let go, so the
 * control drawn for it stands over no words the next selection presses; each refused selection
 * is read once the page has had the frames it reads a selection in (`framesPass`).
 */
async function readNoControl(
  page: Page,
  traffic: ApiRequests,
  seeded: MarginSeed,
  answer: IssueTreeRead
): Promise<void> {
  await openView(page, traffic, seeded.key, "graph");
  const idOf = (read: string, what: string) => treeIdIn(answer, seeded.key, read, what);
  const lens = idOf(seeded.rowLens, "the lens");
  const finding = idOf(rowFindingOf(seeded, answer), "the finding");
  const risk = idOf(seeded.rowRisk, "the risk");
  const constraint = idOf(seeded.constraint, "the constraint");
  const authorOnly = idOf(seeded.authorOnly, "the author-only constraint");
  const child = idOf(seeded.child, "the child issue");
  const control = commentControl(page);
  const takes = async (node: string, quote: string, what: string) => {
    await selectWords(page, node, quote, 0);
    await expect(control, `Comment stands beside a selection in ${what}`).toBeVisible();
    await page.evaluate(() => document.getSelection()?.removeAllRanges());
    await expect(control, `the control goes with the selection in ${what}`).toHaveCount(0);
  };

  await takes(lens, "view", "the lens's verdict");
  await showRow(page, lens);
  await dragAcross(
    page,
    await wordsAt(page, lens, "margin", 0),
    await wordsAt(page, finding, "record", 0)
  );
  await framesPass(page);
  await expect(control, "no control stands on a selection across two rows").toHaveCount(0);
  await expectLoadSettled(traffic);
  traffic.reset();
  await page.keyboard.press("ControlOrMeta+Alt+KeyM");
  await expectLoadSettled(traffic);
  expect(
    traffic.requests
      .filter(({ method }) => method !== "GET")
      .map(({ method, route }) => `${method} ${route}`),
    "$mod+Alt+m on a selection across two rows writes nothing"
  ).toEqual([]);
  await expect(
    composerBox(page),
    "$mod+Alt+m on a selection across two rows opens no composer"
  ).toHaveCount(0);

  await takes(risk, "slow", "the risk's heading");
  // The risk's early warning line: "The warning comes before the reader asks."
  const warning = await lineWords(
    stageRow(page, risk).first().locator('[data-lens-line="early_warning"]'),
    "comes"
  );
  await dragAcross(page, warning, warning);
  await framesPass(page);
  await expect(
    control,
    "no control stands on a selection in a risk's early warning line"
  ).toHaveCount(0);

  await takes(constraint, "standard", "the constraint's standard");
  await selectWords(page, authorOnly, "author", 0);
  await framesPass(page);
  await expect(control, "no control stands on a selection in the author-only node").toHaveCount(0);

  await takes(lens, "view", "the lens's verdict");
  await selectWords(page, child, "titles", 0);
  await framesPass(page);
  await expect(control, "no control stands on a selection in the child issue's row").toHaveCount(0);
}

/** The resolved statement's words, which (5) edits away between a selection and its send. */
const SETTLED_WORDS = "A settled question still has a place.";

/** The draft (5) writes, which stays once the send is refused. */
const KEPT_DRAFT = "A comment on words an edit takes away before the send.";

/** (5) under Spec: the statement's words edited through the API between a selection and its
 *  send: the send draws the words-gone line with the role `status`, and the draft stays. */
async function readWordsGone(page: Page, traffic: ApiRequests, seeded: MarginSeed): Promise<void> {
  await openView(page, traffic, seeded.key, "spec");
  await selectWords(page, blockNode(seeded.specSlug, seeded.resolvedStatement), "place", 0);
  const control = commentControl(page);
  await expect(
    control,
    "Comment stands beside the selection in the resolved statement, in Spec"
  ).toBeVisible();
  await control.click();
  const field = composerField(page);
  await field.fill(KEPT_DRAFT);
  await editArtifact(seeded.specArtifactId, {
    ops: [{ find: SETTLED_WORDS, op: "replace", with: "A settled question still waits." }],
  });
  const refused = page.waitForResponse(commentWrite);
  await field.press("ControlOrMeta+Enter");
  await refused;
  await expect(
    composerBox(page).getByRole("status").filter({ hasText: MARGIN_ANSWERS.words_gone }),
    "the send of words an edit took away says they are no longer in this text"
  ).toBeVisible();
  await expect(composerField(page), "the draft stays once the words are gone").toHaveValue(
    KEPT_DRAFT
  );
}

// c-write, row 2. Below 1024 px the composer stands in its node's sheet, which each view's row
// draws once the part that last reshapes it is on the base (G6, departure five), so on `iphone`
// the row reads (4) alone and records the writes owed on those parts too.
// SLOW(>=2s): the whole seeded issue, a comment sent on a block in each of four views and on five
// nodes in Graph, each read back from the server, six views loaded again, four refused
// selections, and a send of words an edit took away.
test("a person comments on selected text of each kind of node @phone", async ({ browser }) => {
  test.setTimeout(900_000);
  const phone = test.info().project.name === "iphone";
  const sheet = (...views: View[]): Gate[] =>
    phone ? [...new Set(views.map((view) => SHEET_PART[view]))] : [];
  const blocks = onBase(
    "(1) to (3) a selection, its Comment and its send on a statement, a check, a plan task and a decision, read back from the server with its anchor's artifact, block_id, quote and occurrence 1, and the thread beside its node with the focus on its root",
    "COMPOSER",
    ...sheet("spec", "verification", "plan", "decisions")
  );
  onBase(
    "(1) to (3) on a criterion and on a block written in the conversation, with the issue's `tree` artifact",
    "COMPOSER",
    "TREE_ARTIFACT_ANCHOR",
    "PART_Y"
  );
  onBase(
    "(1) to (3) on a post, a part of an ask and a message: reply_to and reply_to_block, ask_id, in_reply_to and in_reply_to_block, each with quote and occurrence 1 on its reply_block",
    "COMPOSER",
    "REPLY_QUOTE"
  );
  const nodes = onBase(
    "(1) to (3) on a lens's verdict, a finding, a risk's heading, a constraint's standard and a result's observed words, with anchor.node, and after a reload the thread beside its node in every view that draws it",
    "COMPOSER",
    "NODE_ANCHOR",
    ...sheet("graph")
  );
  onBase(
    "(3) the thread beside a block node after a reload, in every view that draws it",
    "COMPOSER",
    "ANCHOR_OCCURRENCE"
  );
  onBase(
    "(3) right after its send, a block's thread level with the second match of its quote, the selected words (until then the page places it at the first)",
    "COMPOSER",
    "ANCHOR_OCCURRENCE"
  );
  onBase(
    "(3) on `iphone` at 390 px each thread under its node (departure five)",
    "COMPOSER",
    "PART_X",
    "PART_U",
    "PART_J",
    "PART_Y"
  );
  const refused = onBase(
    "(4) no control on a selection across two rows (and `$mod+Alt+m` writes nothing), on a risk's early warning line, on the author-only node and on the child issue's row",
    "COMPOSER",
    "NODE_ANCHOR"
  );
  onBase(
    "(4) no control on a selection across two blocks of one post (until then no selection in a post takes the control, so none stands to refuse it)",
    "COMPOSER",
    "REPLY_QUOTE"
  );
  const gone = onBase(
    "(5) the statement's words edited between a selection and its send: the send draws the words-gone line with the role status and the draft stays",
    "COMPOSER",
    ...sheet("spec")
  );
  onBase(
    "(5) the post's words edited between a selection and its send: the send draws the words-gone line with the role status and the draft stays",
    "COMPOSER",
    "REPLY_QUOTE",
    ...sheet("conversation")
  );
  onBase(
    "(6) the statement's editor's Comment opens the composer in the region with an anchor of `artifact` and `mark_id` alone, and its Suggest stands as a thread with Accept and Reject",
    "COMPOSER",
    "PART_Y"
  );
  onBase(
    "(7) a selection in Threads' opened thread of the post and in a Dashboard item",
    "COMPOSER",
    "REPLY_QUOTE",
    "THREADS",
    "DASHBOARD"
  );
  onBase(
    "(8) on the child's page, a selection in this issue's requirement in its Spec view, sent to this issue's comments, beside that requirement after the re-read",
    "COMPOSER",
    "OTHER_ISSUE_COMMENTS"
  );
  const notKept = onBase(
    "(9) a reply whose 201 holds no quote: a row under the post, the line that says the words were not kept, and no thread in the region",
    "COMPOSER",
    "REPLY_QUOTE",
    ...sheet("conversation")
  );
  if (blocks || nodes || refused || gone) {
    await phase(browser, seedMarginIssue, async (page, seeded) => {
      await page.setViewportSize(phone ? PHONE : DESKTOP);
      const traffic = watchApiRequests(page);
      const answer = await requestedTreeRead(page, seeded.key);
      if (blocks) await writeOnBlocks(page, traffic, seeded);
      if (nodes) await writeOnNodes(page, traffic, seeded, answer);
      if (refused) await readNoControl(page, traffic, seeded, answer);
      if (gone) await readWordsGone(page, traffic, seeded);
    });
  }
  if (!notKept) return;
  await phase(browser, seedMarginIssue, async (page, seeded) => {
    await page.setViewportSize(phone ? PHONE : DESKTOP);
    const stripped = await keepNoQuote(page);
    const traffic = watchApiRequests(page);
    await gotoIssue(page, presetPath(seeded.key, "conversation"));
    await expectLoadSettled(traffic);
    await selectWords(page, seeded.post, "sample", 1);
    const control = commentControl(page);
    await expect(control, "Comment stands beside the selection").toBeVisible();
    await control.click();
    const field = composerField(page);
    await field.fill("The second sample, said from a build that keeps no quote.");
    const written = page.waitForResponse(commentWrite);
    await field.press("ControlOrMeta+Enter");
    const answer: unknown = await (await written).json();
    const reply =
      typeof answer === "object" && answer !== null && "id" in answer ? String(answer.id) : "";
    expect(stripped.has(reply), "the route answered the reply with no quote").toBe(true);
    await expect(
      page.getByRole("status").filter({ hasText: MARGIN_ANSWERS.quote_not_kept }),
      "the page says the words were not kept"
    ).toBeVisible();
    await expect
      .poll(() => parentOfRow(page, reply), { message: "the reply stands as a row under the post" })
      .toBe(seeded.post);
    await expect(threadBox(page, reply), "the region holds no thread for the reply").toHaveCount(0);
  });
});

/** How many stops Tab passes at most on its way from a row to the region. */
const TAB_STOPS_AT_MOST = 200;

/** Where the focus stands: in the region, with the thread whose box holds it; in the main column;
 *  or elsewhere; and the stop itself, by its tag and its first words. */
interface FocusPlace {
  readonly place: "column" | "other" | "region";
  readonly stop: string;
  readonly thread: string | null;
}

function focusPlace(page: Page): Promise<FocusPlace> {
  return page.evaluate((name): FocusPlace => {
    const focused = document.activeElement;
    const column =
      [...document.querySelectorAll('[role="region"]')].find(
        (element) => element.getAttribute("aria-label") === name
      ) ?? null;
    const stage = document.querySelector('[data-testid="issue-column"]');
    const stop =
      focused === null
        ? "nothing"
        : `${focused.tagName.toLowerCase()} "${(focused.textContent ?? "").trim().slice(0, 40)}"`;
    if (focused !== null && column?.contains(focused) === true) {
      return {
        place: "region",
        stop,
        thread: focused.closest("[data-text-thread]")?.getAttribute("data-text-thread") ?? null,
      };
    }
    return {
      place: focused !== null && stage?.contains(focused) === true ? "column" : "other",
      stop,
      thread: null,
    };
  }, MARGIN_WORDS.region);
}

// c-write, row 3. R6's caret on a post waits on G4 as well, and the statement's editor caret on
// part Y; the foot's box, which Tab passes, is part L's.
// SLOW(>=2s): the whole seeded issue, loaded in Conversation with every branch opened, and Tab
// pressed from a row across the main column's later stops into the region.
test("the keyboard comments on selected text and reaches the margin", async ({ browser }) => {
  test.setTimeout(300_000);
  onBase(
    "on a post's row Enter puts the caret in the post's text, a key typed there changes nothing, Shift with the right arrow selects four characters, `$mod+Alt+m` opens the composer beside the post headed by them, Enter sends and Escape leaves the caret for the row",
    "COMPOSER",
    "REPLY_QUOTE"
  );
  onBase(
    "on a statement's row Enter puts the editor's caret in the statement, and a selection with `$mod+Alt+m` opens the composer in the region while Details draws no composer and no thread",
    "COMPOSER",
    "PART_Y"
  );
  const tab = onBase(
    "from the row Tab passes the main column's later stops and reaches the region's first thread, and Escape in that thread puts the focus on its node's row",
    "COMPOSER"
  );
  owe(
    "the box at the main column's foot among the stops Tab passes from the row",
    "part L (INFRA-3949) fills the foot (`slots/foot.tsx`), which no line of the gate table holds: at this base the foot draws no box"
  );
  if (!tab) return;
  await phase(browser, seedMarginIssue, async (page, seeded) => {
    const traffic = watchApiRequests(page);
    await openView(page, traffic, seeded.key, "conversation");
    const first = region(page).locator("[data-text-thread]:not([hidden])").first();
    await expect(first, "the region draws a thread").toBeVisible();
    // The wait above holds until the column draws its first thread, so one reading names it.
    const named = await first.evaluate((box) => ({
      node: box.getAttribute("data-text-thread-node"),
      thread: box.getAttribute("data-text-thread"),
    }));
    await focusRow(page, blockNode(seeded.specSlug, seeded.statementS1));
    const passed: string[] = [];
    let reached: FocusPlace | undefined;
    for (let press = 0; press < TAB_STOPS_AT_MOST && reached === undefined; press += 1) {
      await page.keyboard.press("Tab");
      // Tab moves the focus before its press resolves, so one reading names the stop it reached.
      const at = await focusPlace(page);
      if (at.place === "region") reached = at;
      else passed.push(`${at.place}: ${at.stop}`);
    }
    expect(
      passed.filter((stop) => !stop.startsWith("column:")),
      "from the row Tab passes the main column's later stops alone before it reaches the region"
    ).toEqual([]);
    expect(reached?.thread ?? null, "Tab from the row reaches the region's first thread").toBe(
      named.thread
    );
    await page.keyboard.press("Escape");
    await expect
      .poll(
        () =>
          page.evaluate(() => {
            const focused = document.activeElement;
            return focused?.matches("[data-node-row]") === true
              ? focused.getAttribute("data-node-id")
              : null;
          }),
        { message: "Escape in the thread puts the focus on the row of its node" }
      )
      .toBe(named.node);
  });
});

// ---------------------------------------------------------------------------------------------
// Row 4.
// ---------------------------------------------------------------------------------------------

/** The words of a reply alice writes from a thread's reply control. */
const MARGIN_REPLY = "A reply written from the thread beside the statement.";

/** (5) in `view` (an outline): the comment's Lens act opens the menu of lens kinds, and a pick
 *  draws the new lens under the comment in its thread, focused. */
async function invokeFromThread(card: Locator, view: View): Promise<void> {
  const before = await card
    .locator('[data-thread-lens-type="lens"]')
    .evaluateAll((lenses) => lenses.map((lens) => lens.getAttribute("data-thread-lens") ?? ""));
  // Until G5 stacks the column's cards, a lower node's card can sit over this one; focusing the
  // control raises its card above siblings through `focus-within:z-20`.
  const act = card.locator("[data-thread-acts] [data-row-lens]").first();
  await act.focus();
  await act.click();
  const menu = card.getByRole("menu", { name: LENS_WORDS.menu_title });
  await expect(
    menu,
    inView(view, `the comment's Lens act opens "${LENS_WORDS.menu_title}"`)
  ).toBeVisible();
  await menu.getByRole("menuitem").first().click();
  await expect
    .poll(
      () =>
        card.evaluate((element, known) => {
          const added = [...element.querySelectorAll('[data-thread-lens-type="lens"]')].filter(
            (lens) => !known.includes(lens.getAttribute("data-thread-lens") ?? "")
          );
          return {
            added: added.length,
            underComment: added.every((lens) => lens.closest("li[data-reply]") === null),
          };
        }, before),
      { message: inView(view, "a pick draws the new lens under the comment in its thread") }
    )
    .toEqual({ added: 1, underComment: true });
  await expect
    .poll(
      () =>
        card.evaluate((element, known) => {
          const focused = document.activeElement;
          return (
            focused !== null &&
            element.contains(focused) &&
            focused.getAttribute("data-thread-lens-type") === "lens" &&
            !known.includes(focused.getAttribute("data-thread-lens") ?? "")
          );
        }, before),
      { message: inView(view, "the new lens takes the focus") }
    )
    .toBe(true);
}

// c-thread-acts. (4) is read first: at the merge-base the row fails there, and with the lenses
// left out of the threads. (1) comes before (5), so a column that builds each thread from the
// load's first answer alone fails on (1).
// SLOW(>=2s): the whole seeded issue, its tree read through the page's own request context, a
// reply sent from the column and read back, two lenses invoked from the column in two outlines,
// a load of Spec and a resolve.
test("a thread in the margin replies, resolves and carries its acts and lenses", async ({
  browser,
}) => {
  test.setTimeout(300_000);
  onBase(
    "(2) in the post's quoted reply's thread a reply stored with `answers` the quoted reply, in that thread and in no row, with no Resolve; and in the message's thread a reply control that sends a message whose in_reply_to is the thread's root",
    "REPLY_QUOTE",
    "COMPOSER"
  );
  onBase('(6) "Write a criterion" on the comment, in Conversation, aims the box at it', "PART_Y");
  onBase(
    "(7) a reply alice writes in Threads on the statement's thread stands in the margin thread after the re-read",
    "THREADS"
  );
  await phase(browser, seedMarginIssue, async (page, seeded) => {
    const { key } = seeded;
    const traffic = watchApiRequests(page);
    await gotoIssue(page, presetPath(key, "conversation"));
    await expectLoadSettled(traffic);
    const card = threadCard(page, seeded.statementComment);

    // (4) The seeded pre-mortem on the comment on the statement.
    const lens = card
      .locator(`[data-thread-lens-read="${seeded.lens}"], [data-thread-lens="${seeded.lens}"]`)
      .first();
    await expect(lens, "the lens stands under the comment in its thread").toBeVisible();
    expect(
      await lens.evaluate((element) => element.closest("li[data-reply]") === null),
      "the lens stands under the comment in its thread, and under no reply"
    ).toBe(true);
    const [answer, catalogue] = await Promise.all([requestedTreeRead(page, key), lensCatalogue()]);
    const verdict = (answer.nodes ?? []).find((node) => node.id === seeded.lens)?.text ?? "";
    expect(verdict, "the tree read holds the lens's verdict").not.toBe("");
    const kindWords = (kind: string) =>
      sentenceCase(catalogue.find((one) => one.id === kind)?.words ?? kind);
    await expect(
      lens.locator("[data-node-tag]").first(),
      "the lens draws its kind's words"
    ).toHaveText(kindWords("pre-mortem"));
    await expect(lens.locator("[data-node-author]").first(), "the lens names alice").toHaveText(
      READER
    );
    await expect(lens.locator("[data-node-time]").first(), "the lens draws its time").toHaveText(
      /\S/u
    );
    await expect(lens, "the lens draws its verdict").toContainText(verdict);
    await expect(
      lens.locator('[data-lens-line="triggered_by"]').first(),
      "the lens draws its trigger line, which names the trade-off analysis"
    ).toContainText(kindWords("trade-off"));
    await expect(
      lens.locator('[data-thread-lens-type="finding"]').first(),
      "the lens's finding stands under it"
    ).toBeVisible();

    // (1) A reply from the thread's reply control.
    await card.getByRole("textbox", { exact: true, name: "Reply" }).fill(MARGIN_REPLY);
    await card.getByRole("button", { exact: true, name: "Send" }).click();
    await expect(
      card.locator("li[data-reply]").last(),
      "the reply is the thread's last entry"
    ).toContainText(MARGIN_REPLY);
    await expect
      .poll(
        async () =>
          (await listComments(key)).find((comment) => comment.body.trim() === MARGIN_REPLY)
            ?.reply_to ?? null,
        { message: "the server reads the reply with reply_to the thread's root" }
      )
      .toBe(seeded.statementComment);

    // (5) The comment's Lens act in both outlines, and none under Spec.
    await invokeFromThread(card, "conversation");
    await gotoIssue(page, presetPath(key, "graph"));
    await expectLoadSettled(traffic);
    await expect(card, "in Graph, the statement's thread is drawn").toBeVisible();
    await invokeFromThread(card, "graph");
    await gotoIssue(page, presetPath(key, "spec"));
    await expectLoadSettled(traffic);
    await expect(card, "under Spec, the statement's thread is drawn").toBeVisible();
    await expect(
      card.locator("[data-row-lens]"),
      "under Spec the comment carries no Lens act"
    ).toHaveCount(0);

    // (3) Resolve.
    await gotoIssue(page, presetPath(key, "conversation"));
    await expectLoadSettled(traffic);
    const resolveButton = card.getByRole("button", { exact: true, name: "Resolve" });
    await resolveButton.focus();
    await resolveButton.click();
    await expect(
      card.getByRole("button", { exact: true, name: "Reopen" }),
      "the resolved thread stays drawn, with Reopen"
    ).toBeVisible();
    await expect
      .poll(async () => (await getComment(seeded.statementComment)).comment.resolved, {
        message: "the server reads the statement's thread resolved",
      })
      .toBe(true);
    await expect(card, "the resolved thread stays drawn").toBeVisible();
  });
});

// ---------------------------------------------------------------------------------------------
// Row 5.
// ---------------------------------------------------------------------------------------------

/** Where the Contents entry of the comment `entry` stands, and where it should: the entry it
 *  stands under in the list (null at the top), and the nearest entry among the ancestors of
 *  `node`'s row in the outline (null where none is one). */
function entryPlace(
  page: Page,
  entry: string,
  node: string
): Promise<{ readonly actual: string | null; readonly expected: string | null }> {
  return page.evaluate(
    ({ entry: id, node: nodeId }) => {
      const level = (element: Element) => Number(element.getAttribute("aria-level") ?? "0");
      const entries = [
        ...document.querySelectorAll('[data-testid="issue-contents"] [data-contents-entry]'),
      ];
      const ids = new Set(entries.map((one) => one.getAttribute("data-contents-entry")));
      const at = entries.findIndex((one) => one.getAttribute("data-contents-entry") === id);
      const own = entries[at];
      const listed =
        own === undefined
          ? undefined
          : entries
              .slice(0, at)
              .reverse()
              .find((one) => level(one) < level(own));
      const rows = [...document.querySelectorAll('[data-testid="issue-column"] [data-node-row]')];
      const row = rows.findIndex((one) => one.getAttribute("data-node-id") === nodeId);
      let depth = row === -1 ? 0 : level(rows[row] as Element);
      let expected: string | null = null;
      for (let above = row - 1; above >= 0 && expected === null; above -= 1) {
        const candidate = rows[above] as Element;
        if (level(candidate) >= depth) continue;
        depth = level(candidate);
        const candidateId = candidate.getAttribute("data-node-id");
        if (ids.has(candidateId)) expected = candidateId;
      }
      return { actual: listed?.getAttribute("data-contents-entry") ?? null, expected };
    },
    { entry, node }
  );
}

// c-address. The named reply's focus is read first: at the merge-base the row fails there, and
// with an address that opens the issue's own address instead.
// SLOW(>=2s): the whole seeded issue, loaded at the addresses of a reply, the Change, the thread
// past the first page of a statement's children, the risk's comment in two views and a comment
// no view draws, and at the issue's own address for its Contents pane.
test("the address of a comment on text opens its thread", async ({ browser }) => {
  test.setTimeout(300_000);
  const risk = onBase(
    "`?comment=<id>` of the risk's comment opens the view that draws the risk's row, with the focus in that thread: Graph at Graph's address, and Conversation at the issue's own, since Conversation draws the risk (R8)",
    "NODE_ANCHOR"
  );
  const message = onBase(
    "`/issues/<key>/messages/<id>` of the reply that quotes the message opens Graph there",
    "REPLY_QUOTE"
  );
  onBase(
    "the address of the comment on the restating decision block's words opens Conversation with that thread beside the ask the block restates and the focus on it",
    "PART_U"
  );
  const unseen = onBase(
    "the address of a comment whose node no view draws opens the issue's own address with no line",
    "COMPOSER"
  );
  onBase("the address of a comment whose node no view draws opens it in Threads", "THREADS");
  await phase(browser, seedMarginIssue, async (page, seeded) => {
    const { key } = seeded;
    const spots = await threadSpots(seeded);
    const traffic = watchApiRequests(page);

    const replyPath = buildIssuePath({ id: seeded.statementReply, key, kind: "comment" });
    await gotoIssue(page, replyPath);
    await expect(
      namedReply(page, seeded.statementComment),
      "the named reply has the focus in its thread"
    ).toBeFocused();
    await expect(
      page,
      "the address of a reply in the statement's thread opens Conversation at that address"
    ).toHaveURL((url) => url.pathname === replyPath);
    const statement = spots.filter((spot) => spot.id === seeded.statementComment);
    await expect
      .poll(async () => offWords(await readSpots(page, statement), statement, false), {
        message: "the address of a reply opens its thread level with its words",
      })
      .toEqual([]);

    await gotoIssue(page, `${presetPath(key, "conversation")}?comment=${seeded.change}`);
    await expect(
      threadBox(page, seeded.change),
      "the Change stands beside its decision"
    ).toHaveAttribute("data-text-thread-node", blockNode(seeded.specSlug, seeded.agentDecision));
    await expect(
      threadCard(page, seeded.change),
      "the address focuses the Change's thread"
    ).toBeFocused();
    await expect(stageRow(page, seeded.change), "the Change is no outline row").toHaveCount(0);

    await gotoIssue(page, buildIssuePath({ id: seeded.pagedChildThread, key, kind: "comment" }));
    await expect(
      threadCard(page, seeded.pagedChildThread),
      "the address of the thread on the 55th child focuses that thread"
    ).toBeFocused();
    await expect(
      stageRow(page, blockNode(seeded.specSlug, seeded.pagedChild55)).first(),
      "the address of the thread on the 55th child draws that child's page of children"
    ).toBeVisible();

    if (risk) {
      const comment = seeded.riskComment;
      if (comment === null) throw new Error("the seed wrote no comment on the risk");
      // R8 opens Conversation where Conversation draws the thread's node, else Graph. The risk
      // stands under the lens on the resolved statement, and a risk is a kind of Conversation's
      // read, so Conversation draws its row and the issue's own address keeps the thread there.
      for (const view of ["graph", "conversation"] as const) {
        await gotoIssue(page, `${presetPath(key, view)}?comment=${comment}`);
        await expect(
          page,
          inView(view, "?comment= of the risk's comment opens the view that draws the risk's row")
        ).toHaveURL((url) => url.pathname === presetPath(key, view));
        await expect(
          threadCard(page, comment),
          inView(view, "?comment= of the risk's comment puts the focus in its thread")
        ).toBeFocused();
      }
    }
    if (message) {
      const reply = seeded.messageQuotedReply;
      if (reply === null) throw new Error("the seed wrote no reply that quotes the message");
      await gotoIssue(page, buildIssuePath({ id: reply, key, kind: "message" }));
      await expect(page, "the address of the reply that quotes the message opens Graph").toHaveURL(
        (url) => url.pathname === presetPath(key, "graph")
      );
      await expect(
        threadCard(page, reply),
        "the address of the reply that quotes the message focuses its thread"
      ).toBeFocused();
    }
    if (unseen) {
      const [comment] = seeded.uncitedThreads;
      if (comment === undefined)
        throw new Error("the seed wrote no thread on the uncited document");
      await gotoIssue(page, buildIssuePath({ id: comment, key, kind: "comment" }));
      await expect(
        page,
        "the address of a comment whose node no view draws opens the issue's own address"
      ).toHaveURL((url) => url.pathname === presetPath(key, "conversation"));
      await expect(
        issuePage(page).getByRole("heading", { exact: true, name: "Comment not found" }),
        "the address of a comment whose node no view draws draws no line"
      ).toHaveCount(0);
    }

    await loadView(page, traffic, key, "conversation");
    const entry = contentsPane(page).locator(`[data-contents-entry="${seeded.statementComment}"]`);
    await expect(entry, "the Contents pane lists alice's comment on the statement").toBeVisible();
    await expect(
      contentsPane(page).locator(`[data-contents-entry="${seeded.statementReply}"]`),
      "the Contents pane lists no reply in the statement's thread"
    ).toHaveCount(0);
    const place = await entryPlace(
      page,
      seeded.statementComment,
      blockNode(seeded.specSlug, seeded.statementS1)
    );
    expect(
      place.actual,
      "the comment's entry stands under the nearest entry above the statement"
    ).toBe(place.expected);
    await entry.click();
    await expect(
      threadCard(page, seeded.statementComment),
      "a press on the entry focuses the comment in its thread"
    ).toBeFocused();
  });
});

// ---------------------------------------------------------------------------------------------
// Row 6.
// ---------------------------------------------------------------------------------------------

/** The reads the shell sends on a timer of its own whatever the load: the credential requests
 *  every 15 s, and the agents every 15 s while the page draws an agent's name, as both seeds'
 *  rows by the planner do (`e2e/issue-page-w.e2e.ts`'s `SHELL_TIMED_READS`). */
const SHELL_TIMED_READS: Readonly<Record<string, true>> = {
  "/api/v1/agents": true,
  "/api/v1/credential-requests": true,
};

/** The reads of one comment or one message, which a read for each thread would be. */
const PER_ITEM_READS: Readonly<Record<string, true>> = {
  "/api/v1/comments/:id": true,
  "/api/v1/comments/:id/blocks": true,
  "/api/v1/messages/:id/blocks": true,
};

/** The issue's comments read. */
const COMMENTS_READ = "/api/v1/issues/:id/comments";

/** The two loads row 6 reads: Conversation at `/conversation`, and Graph. */
const LOADS = ["conversation", "graph"] as const satisfies readonly View[];

/** What one load sent: every request, and the count of each route but the shell's timed reads. */
interface Load {
  readonly requests: readonly ApiRequest[];
  readonly routes: readonly RouteCount[];
}

/** Loads `view` of `key` at its own route and waits out the load. */
async function loadOnce(
  page: Page,
  traffic: ApiRequests,
  key: string,
  view: (typeof LOADS)[number]
): Promise<Load> {
  traffic.reset();
  await gotoIssue(
    page,
    view === "graph" ? presetPath(key, "graph") : buildIssuePath({ key, kind: "conversation" })
  );
  await expectLoadSettled(traffic);
  return {
    requests: [...traffic.requests],
    routes: traffic.byRoute().filter((one) => SHELL_TIMED_READS[one.route] !== true),
  };
}

/** The words row 6 sends from the composer, whose requests it counts. */
const COUNTED_SEND = "A comment whose send row 6 counts the requests of.";

/** From the page Graph's load `load` left: selects the second "the" of statement S2 ("A reader
 *  finds the first note and the last marker in order."), opens the composer by its Comment, and
 *  answers every request from the send of `COUNTED_SEND` to the page's next quiet window. */
async function sendCounted(
  page: Page,
  traffic: ApiRequests,
  seeded: MarginSeed
): Promise<readonly ApiRequest[]> {
  await expectOpeningLanded(page);
  await unfoldEveryBranch(page);
  await selectWords(page, blockNode(seeded.specSlug, seeded.statementS2), "the", 1);
  const control = commentControl(page);
  await expect(
    control,
    "Comment stands beside the selection in statement S2, in Graph"
  ).toBeVisible();
  await control.click();
  const field = composerField(page);
  await field.fill(COUNTED_SEND);
  await expectLoadSettled(traffic);
  traffic.reset();
  const written = page.waitForResponse(commentWrite);
  await field.press("ControlOrMeta+Enter");
  await written;
  await expectLoadSettled(traffic);
  return [...traffic.requests];
}

// c-reads. The seed's threads are read first: at the merge-base the row fails there.
// SLOW(>=2s): two seeded issues, each loaded at Conversation's route and at Graph's, each load
// waited out to a 1.5 s quiet window, and a send from the composer waited out to that window.
test("the margin adds no request", async ({ browser }) => {
  test.setTimeout(300_000);
  const agentsEqual = onBase(
    "the load with threads reads /api/v1/agents as often as the load without them, since a fixed-open card's reply composer reads the agents only once its reply is used",
    "COMPOSER"
  );
  const sends = onBase(
    "a send from the composer sends its write and the re-reads of part T's R6 alone",
    "COMPOSER"
  );
  const nodeAnchor = onBase("the comments read carries `node_anchor=true`", "NODE_ANCHOR");
  const withThreads = new Map<View, Load>();
  const without = new Map<View, Load>();
  await phase(browser, seedMarginIssue, async (page, seeded) => {
    const spots = await threadSpots(seeded);
    const must = new Set([...seeded.threadsS1, seeded.agentComment, seeded.resolvedThread]);
    const traffic = watchApiRequests(page);
    for (const view of LOADS) {
      const load = await loadOnce(page, traffic, seeded.key, view);
      await expect
        .poll(async () => undrawn(await readSpots(page, spots), spots, must), {
          message: inView(view, "the region draws each seeded thread"),
        })
        .toEqual([]);
      withThreads.set(view, load);
    }
    if (!sends) return;
    // The page stands at Graph's load, the last of `LOADS`: each read a write sends again is a
    // read of that load.
    const graph = withThreads.get("graph");
    if (graph === undefined) throw new Error("row 6 holds no load of Graph");
    const sent = await sendCounted(page, traffic, seeded);
    const reread = new Set(graph.routes.map(({ route }) => route));
    expect(
      sent
        .filter(({ method }) => method !== "GET")
        .map(({ method, route }) => `${method} ${route}`),
      "a send from the composer sends its write alone"
    ).toEqual([`POST ${COMMENTS_READ}`]);
    expect(
      sent
        .filter(
          ({ method, route }) =>
            method === "GET" && SHELL_TIMED_READS[route] !== true && !reread.has(route)
        )
        .map(({ path }) => path),
      "a send from the composer reads again only what Graph's load read, as part T's R6 re-reads after a write"
    ).toEqual([]);
  });
  await phase(browser, seedNoThreads, async (page, seeded) => {
    const traffic = watchApiRequests(page);
    for (const view of LOADS) without.set(view, await loadOnce(page, traffic, seeded.key, view));
  });
  for (const view of LOADS) {
    const threaded = withThreads.get(view);
    const plain = without.get(view);
    if (threaded === undefined || plain === undefined) {
      throw new Error(`row 6 holds no load of ${PRESET_NAMES[view]}`);
    }
    expect(
      threaded.routes,
      inView(view, "the load with threads sends the requests of the load without them")
    ).toEqual(plain.routes);
    const agentReads = (load: Load) =>
      load.requests.filter((request) => request.route === "/api/v1/agents").length;
    if (agentsEqual) {
      expect(
        agentReads(threaded),
        inView(view, "the load with threads reads the agents as often as the load without them")
      ).toBe(agentReads(plain));
    }
    expect(
      threaded.requests
        .filter((request) => PER_ITEM_READS[request.route] === true)
        .map(({ path }) => path),
      inView(view, "the load sends no read of one comment or one message")
    ).toEqual([]);
    const comments = threaded.requests.filter(
      (request) => request.method === "GET" && request.route === COMMENTS_READ
    );
    expect(comments.length, inView(view, "the load sends one comments read")).toBe(1);
    if (nodeAnchor) {
      expect(
        comments.map(
          ({ path }) => new URL(path, "http://page.invalid").searchParams.get("node_anchor") ?? ""
        ),
        inView(view, "the comments read carries node_anchor=true")
      ).toEqual(["true"]);
    }
  }
});

// ---------------------------------------------------------------------------------------------
// Rows 7 and 8.
// ---------------------------------------------------------------------------------------------

// c-document. Owed until part N's opened document is on the base: the row records each reading
// owed and reads nothing.
test("an opened document draws its threads in the margin @phone", async ({ browser: _browser }) => {
  onBase(
    "Documents opens the seeded document that nothing cites, and at 1440 px each of its two threads stands in the region beside its block, at its mark",
    "PART_N"
  );
  onBase("the document's two threads stand with no overlap", "PART_N", "COMPOSER");
  onBase("Details draws no thread of the opened document", "PART_N");
  onBase(
    "a selection and the editor's Comment write a comment with today's anchor (`artifact`, `mark_id`, `quote`), which then stands beside its block",
    "PART_N",
    "COMPOSER"
  );
  onBase("on `iphone` at 390 px each thread stands under its block", "PART_N");
});

// c-change. Owed until part U is on the base (G5, the Change's place in the margin, is on it, and
// row 1 reads the Change's thread in Conversation and in Graph): the row records each reading
// owed and reads nothing.
test("a Change stands in the margin and Decisions keeps Changed by", async ({
  browser: _browser,
}) => {
  onBase("in Decisions the agent decision's row draws \"Changed by\" and alice's name", "PART_U");
  onBase(
    "the Change's words are drawn once in Decisions, in its thread in the region beside the decision",
    "PART_U",
    "COMPOSER"
  );
  onBase(
    "in Conversation and in Graph the Change's thread stands in the region beside the decision, and is no row",
    "PART_U",
    "COMPOSER"
  );
});
