import { expect, type Locator, type Page, type Request, type Route } from "@playwright/test";

import type { IssueTreeNode, IssueTreeRead } from "../web/src/api/types";
import { PAGE_CHROME_ATTRIBUTE } from "../web/src/features/issue-page/page-structure";
import {
  DECISIONS_WORDS,
  NODE_STATE_WORDS,
  PRESET_NAMES,
} from "../web/src/features/issue-page/page-words";
import type { TreeIdReads } from "../web/src/features/issue-page/tree-read/tree-nodes";
import { editArtifact, getIssue, listIssueAsks } from "./api";
import { asPlanner, blockNodeIn, readTree, treePath } from "./issue-layout-seed-support";
import { requirementBlockId, specBlockRef } from "./issue-layout-seed-text";
import { presetCount, presetToggle } from "./issue-page";
import { blockNode, longWords } from "./issue-page-g";
import { expectActivePreset, pickPreset } from "./issue-page-i";
import { decisionsPart } from "./issue-page-q";
import { drawnRowReadings, type RowReading, steadily } from "./issue-page-t";
import {
  type DecisionsRead,
  type DecisionsReadWatch,
  type Gate,
  linksTo,
  OWED_BY,
  type TreeView,
  treeReadRoute,
} from "./issue-page-t-tree";
import type { ApiRequests } from "./traffic";

/**
 * What the decision rows (`e2e/issue-page-u.e2e.ts`) read of the new issue page beside what the
 * other rows' helpers already read (`e2e/issue-page.ts`, `e2e/issue-page-{g,i,p,q,t}.ts` and
 * `e2e/issue-page-t-tree.ts`): the rows' own helper file, which reuses those readers where their
 * files export them. It keeps its own `releaseFilter`, which first checks that the filter's
 * control is drawn, where part T's (`e2e/issue-page-t-rows.ts`) presses it at once.
 *
 * - **The server's answer.** `e2e/issue-layout-seed-support.ts` holds the reads of the tree read
 *   (`GET /api/v1/issues/{key}/tree`) the rows share with the seeds: `readTree` is the read of
 *   the whole tree or of one view (`?view=`), every page its cursor names read once as alice;
 *   `decisionNodesOf` is the decision nodes it answers for some blocks, `decisionSumOf` the
 *   `text_sha256` one of them carries, `decisionSum` that of one block as the tree read answers
 *   it now, and `blockNodeIn` the node of one block out of a read's nodes. They stand there, and
 *   not here, because a seed keeps a decision by the sum `decisionSum` reads, and this file
 *   imports the seeds through `e2e/issue-page-t.ts`: a seed that imported this file would close
 *   an import cycle. The `treePath` there is a copy of the one `e2e/issue-page-t-tree.ts` holds,
 *   which that file does not export. Here, `viewNodesOf` is the nodes one view of the tree
 *   answers, `heldDecisionsRead` the decisions read that the gate on the seeds written as
 *   decision nodes (`decisionNodesGate`) reads, `decisionsViewServedGate` the gate on the
 *   server serving the tree read's Decisions view, and `sessionTitleGate` the gate on the tree
 *   read answering the title a session stamped on its session node.
 * - **How a row names a node.** `decisionRowId` is the id a decision node's row carries (the
 *   `<slug>#b-<block id>`), `blockRowId` the id the row of a seed's block carries, a decision's
 *   or a statement's, by that block's node in a tree read, `askRowId` an ask's (its block's node
 *   where the asks read names its block, as `tree-read/tree-nodes.ts` names it), and `askOfNode`
 *   the asks read's row of an ask node; `idReadsOf` is the issue read and the asks read
 *   `treeIdOf` names nodes by.
 * - **What the page draws.** `reviewWordOf` is the state word a decision's row draws from its
 *   node; `specColumn` is, for each statement the Spec view draws, what stands beside it - the
 *   decision cell whose bracket spans it (`[data-spec-cell]`) and the links of its line
 *   (`[data-spec-because]`) - and `statementsBeside` the statements a decision stands beside, a
 *   link going to a node by part T's rule (`issue-page-t-tree.ts`' `linksTo`).
 *   `readFilter` and `releaseFilter` press one filter on and off as part T's rows do
 *   (`e2e/issue-page-t-rows.ts`). `actsBesideWords` is what keeps the focused row's acts
 *   (`RowActs`) from standing beside its words; `decisionsPages` is each part of the Decisions
 *   view as row 4 reads its pages - its count, each entry with how many lines its words draw and
 *   whether they are cut or folded, and the words of the control under it that draws its next
 *   entries (`nextEntries`), which `nextCount` reads as the numbers it says;
 *   `decisionsPartHeading` is the words a headed part's heading names it by, its count left out.
 * - **What the page sends and receives.** `watchWrites` keeps every request under `/api/` that is
 *   no read, with its body; `holdTreeReads` holds every tree read the page sends until released.
 * - **Row 2's decisions.** `writeThirdDecision` writes, through the spec's edit route, a
 *   `:::decision` block whose `shaped` names `r1`, as the planner; `rewordThirdDecision` changes
 *   its words, and `rewordSecondDecision` the heading of the seed's second decision
 *   (`SECOND_DECISION`).
 * - **Row 4's decisions.** `writeFootDecisions` writes, through the spec's edit route, as the
 *   planner, `FOOT_DECISIONS` `:::decision` blocks that no `shaped` pointer ties to a statement,
 *   each with words that run past six lines at 1440 px.
 */

/** The nodes the read of issue `key`'s view `view` answers, every page its cursor names. */
export async function viewNodesOf(key: string, view: TreeView): Promise<IssueTreeNode[]> {
  return (await readTree(key, view)).nodes ?? [];
}

/**
 * The decisions read that the gates on whether the seeds' decisions are nodes read
 * (`decisionNodesGate`, `partUGate`): the one the page received (`decisionsReadOf`), and where it
 * received none - the page sends its view read only once Decisions is pressed - the read of the
 * Decisions view the row sends itself, so the gate reads what the server holds and not whether
 * the page asked. A row whose server may serve no such view gates on `decisionsViewServedGate`
 * first.
 */
export async function heldDecisionsRead(
  key: string,
  watch: DecisionsReadWatch
): Promise<DecisionsRead> {
  return watch.read() ?? { answer: await readTree(key, "decisions"), kind: "view" };
}

/** The model's Decisions view of the tree read, as the server serves it: issue `key`'s tree read
 *  with `view=decisions` answers, read through the signed-in page's own request context. It is
 *  met by the server's answer, never by whether the page asked for the view, so a press of
 *  Decisions that sends no view read fails its row. */
export async function decisionsViewServedGate(page: Page, key: string): Promise<Gate> {
  const response = await page.request.get(`${treePath(key)}?view=decisions`);
  return {
    met: response.ok(),
    owed:
      "owed until the server serves the tree read's Decisions view on this branch's base: the " +
      `tree read answers ${response.status()} to view=decisions here; ${OWED_BY}`,
  };
}

/** The title each session stamped, on the session nodes the tree read answers, which the page
 *  names an agent's decision by where the registry does not list its decider (`AgentName.tsx`'s
 *  `drawnSessionName`), and which INFRA-1253's spec v168 puts on every session node the read
 *  answers. It is met where a context node tagged `session` of issue `key`'s tree read, every
 *  page its cursor names read as alice (`readTree`), carries a title that is not empty: by the
 *  server's answer, never by a flag. */
export async function sessionTitleGate(key: string): Promise<Gate> {
  const sessions = ((await readTree(key, null)).nodes ?? []).filter(
    (node) => node.role === "context" && node.supertag === "session"
  );
  return {
    met: sessions.some(
      (node) => "title" in node && typeof node.title === "string" && node.title.trim() !== ""
    ),
    owed:
      "owed until the tree read answers on each session node the title its session stamped " +
      `(INFRA-1253's spec v168) on this branch's base: of the ${sessions.length} session ` +
      `nodes ${key}'s tree read answers as context here, none carries a title; ${OWED_BY}`,
  };
}

/** The issue read and the asks read, as alice reads them: what `treeIdOf` names a node by. */
export async function idReadsOf(key: string): Promise<TreeIdReads> {
  const [issue, asks] = await Promise.all([getIssue(key), listIssueAsks(key)]);
  return { asks, issue };
}

/** The id the row of a decision node carries: `<slug>#b-<block id>` for a block of a
 *  document, which every decision node is, by its block id (`block_id`). */
export function decisionRowId(node: IssueTreeNode): string {
  return node.document === undefined
    ? node.id
    : blockNode(node.document.slug, node.block_id ?? node.id);
}

/** The id the row of block `block` carries, where `answer`, issue `key`'s tree read, holds it as a
 *  node of the issue tagged `supertag` (a decision, a statement): `decisionRowId`'s
 *  `<slug>#b-<block id>`, the id the page names every block of a document by, never the block id
 *  alone. A block the read holds no such node of fails, naming the block. */
export function blockRowId(
  key: string,
  answer: IssueTreeRead,
  supertag: string,
  block: string
): string {
  return decisionRowId(blockNodeIn(answer.nodes ?? [], supertag, block, `${key}'s tree read`));
}

/** One row of the asks read. */
type ReadAsk = NonNullable<TreeIdReads["asks"]>[number];

/** The id the row of ask `askId` carries: the node of the block the asks read says it was indexed
 *  from, in that block's document, and its own id where it names none. */
export function askRowId(reads: TreeIdReads, askId: string): string {
  const ask = (reads.asks ?? []).find((one) => one.id === askId);
  if (ask === undefined || ask.block_id === null) return askId;
  const slug =
    ask.block_artifact?.slug ??
    reads.issue.artifacts?.find((artifact) => artifact.id === ask.artifact_id)?.slug;
  return slug === undefined ? askId : blockNode(slug, ask.block_id);
}

/** The row of the asks read that the ask node `node` is: by the block it is in its document
 *  (`block_id`), or by its own id where it is a block of none. */
export function askOfNode(node: IssueTreeNode, reads: TreeIdReads): ReadAsk | undefined {
  const { document } = node;
  return (reads.asks ?? []).find((ask) =>
    document === undefined
      ? ask.id === node.id
      : ask.block_id === (node.block_id ?? node.id) &&
        (ask.block_artifact?.id ?? ask.artifact_id) === document.artifact_id
  );
}

/** The state word a decision's row draws from the model's decision its node carries
 *  (`decision`): "Replaced", "Waiting for review", or "Kept by" and the person whose Keep stands;
 *  none for a decision in none of them. */
export function reviewWordOf(node: IssueTreeNode): string | null {
  const words = NODE_STATE_WORDS.decision;
  const decision = node.decision;
  if (decision?.replaced === true) return words.replaced;
  if (decision?.waiting_review === true) return words.waiting_review;
  return decision?.kept == null ? null : `${words.kept} ${decision.kept.kept_by}`;
}

/** What stands beside one statement of the Spec view. */
export interface SpecColumnRow {
  /** The `data-node-id` and `data-decision-id` of the cell whose bracket spans the statement. */
  readonly cell: readonly string[];
  /** The statement's row id. */
  readonly id: string;
  /** The `href` of each link of the statement's line: the decision of its run, and each other
   *  decision that informed it. */
  readonly links: readonly string[];
}

/** Each statement the Spec view draws, in drawing order, with what stands beside it. */
export function specColumn(page: Page): Promise<SpecColumnRow[]> {
  return page.evaluate(() => {
    const cells = [...document.querySelectorAll("[data-issue-page] [data-spec-cell]")];
    return [...document.querySelectorAll("[data-issue-page] [data-spec-statement]")].map(
      (statement) => {
        const id = statement.getAttribute("data-node-id") ?? "";
        const cell = cells.find((one) =>
          (one.getAttribute("data-spec-statements") ?? "").split(" ").includes(id)
        );
        return {
          cell: [cell?.getAttribute("data-node-id"), cell?.getAttribute("data-decision-id")].filter(
            (name): name is string => typeof name === "string" && name !== ""
          ),
          id,
          links: [...statement.querySelectorAll("[data-spec-because] a[href]")].map(
            (link) => link.getAttribute("href") ?? ""
          ),
        };
      }
    );
  });
}

/** The statements of `column` a decision stands beside, by any of its `names` (its row id, its
 *  block id, its ask's id): the cell beside the statement is the decision's, or the statement's
 *  line links to it. */
export function statementsBeside(
  column: readonly SpecColumnRow[],
  names: readonly string[]
): string[] {
  return column.flatMap((row) =>
    row.cell.some((name) => names.includes(name)) || row.links.some((href) => linksTo(href, names))
      ? [row.id]
      : []
  );
}

/** What keeps the acts of the focused row `row` (`RowActs`, `[data-row-acts]`) from standing
 *  beside its words (`[data-node-words]`) and outside them, each as words: none where the row
 *  draws one line of acts, in the column its words stand in, outside their box. */
export function actsBesideWords(row: Locator): Promise<string[]> {
  return row.evaluate((element) => {
    const words = element.querySelector("[data-node-words]");
    const acts = [...element.querySelectorAll("[data-row-acts]")];
    const [line] = acts;
    if (words === null) return ["draws no words"];
    if (line === undefined || acts.length > 1) return [`draws ${acts.length} lines of acts`];
    const problems: string[] = [];
    if (words.contains(line)) problems.push("draws its acts inside its words");
    if (line.parentElement !== words.parentElement) {
      problems.push("draws its acts outside the column its words stand in");
    }
    const drawn = line.getBoundingClientRect();
    const box = words.getBoundingClientRect();
    if (
      drawn.top < box.bottom &&
      drawn.bottom > box.top &&
      drawn.left < box.right &&
      drawn.right > box.left
    ) {
      problems.push("draws its acts over its words");
    }
    return problems;
  });
}

/** One entry of a part of the Decisions view as row 4 reads it: its node, how many lines its
 *  words draw, whether they are cut - a control that opens the rest (`[data-node-more]`), or
 *  words their box hides - and whether its row is folded (`aria-expanded="false"`). */
export interface PagedEntry {
  readonly cut: boolean;
  readonly folded: boolean;
  readonly id: string;
  readonly lines: number;
}

/** One part of the Decisions view as row 4 reads it: its count, the entries it draws in the
 *  order it draws them, the words of each control under it that draws its next entries
 *  (`[data-decisions-next]`), and for the foot what its control says of it (`aria-expanded`),
 *  null for a headed part. */
export interface PagedPart {
  readonly count: string;
  readonly entries: readonly PagedEntry[];
  readonly next: readonly string[];
  readonly part: string;
  readonly unfolded: string | null;
}

/** Each part of the Decisions view, in the order it is drawn, with the pages it draws. A line
 *  of an entry's words is a run of their text whose top stands 4 px or more below the last. */
export function decisionsPages(page: Page): Promise<PagedPart[]> {
  return page.evaluate(() => {
    const text = (element: Element | null) =>
      (element?.textContent ?? "").replace(/\s+/gu, " ").trim();
    const linesOf = (words: Element | null): number => {
      if (words === null) return 0;
      const tops: number[] = [];
      const walker = document.createTreeWalker(words, NodeFilter.SHOW_TEXT);
      for (let node = walker.nextNode(); node !== null; node = walker.nextNode()) {
        const range = document.createRange();
        range.selectNodeContents(node);
        for (const rect of [...range.getClientRects()]) {
          if (rect.width > 0) tops.push(rect.top);
        }
      }
      tops.sort((a, b) => a - b);
      return tops.filter((top, at) => at === 0 || top - (tops[at - 1] ?? top) >= 4).length;
    };
    return [...document.querySelectorAll("[data-issue-page] [data-decisions-part]")].map(
      (part) => ({
        count: text(part.querySelector("[data-decisions-count]")),
        entries: [...part.querySelectorAll("[data-node-row]")].map((row) => {
          const words = row.querySelector("[data-node-words]");
          return {
            cut:
              row.querySelector("[data-node-more]") !== null ||
              (words !== null && words.scrollHeight > words.clientHeight),
            folded: row.getAttribute("aria-expanded") === "false",
            id: row.getAttribute("data-node-id") ?? "",
            lines: linesOf(words),
          };
        }),
        next: [...part.querySelectorAll("[data-decisions-next] button")].map(text),
        part: part.getAttribute("data-decisions-part") ?? "",
        unfolded:
          part.querySelector("[data-decisions-foot] button")?.getAttribute("aria-expanded") ?? null,
      })
    );
  });
}

/** The button of the control under the Decisions view's part `part` that draws its next
 *  entries (`DecisionsFilterBody.tsx`' `NextEntries`), a treeitem out of the tab order. */
export function nextEntries(page: Page, part: string): Locator {
  return decisionsPart(page, part).locator("[data-decisions-next] button");
}

/** The words the heading of the Decisions view's headed part `part` names it by, its count left
 *  out: the page's own words in its heading (`[data-decisions-heading]`), the count being the
 *  author's text (`DecisionsFilterBody.tsx`). Null where the view draws no such part. */
export async function decisionsPartHeading(page: Page, part: string): Promise<string | null> {
  const [words] = await decisionsPart(page, part)
    .locator(`[data-decisions-heading] [${PAGE_CHROME_ATTRIBUTE}]`)
    .allTextContents();
  return words === undefined ? null : words.replace(/\s+/gu, " ").trim();
}

/** The words of the control that draws a part's next entries, by the page's own entries, with
 *  the two numbers it says: "Show the next 7 (7 left)". */
const NEXT_WORDS = new RegExp(
  `^${DECISIONS_WORDS.page_next} (\\d+) \\((\\d+) ${DECISIONS_WORDS.page_left}\\)$`,
  "u"
);

/** What the control under a part says, read off its words `words`: how many entries a press
 *  draws and how many are left; its words as they are where they say neither. */
export function nextCount(
  words: string
): { readonly left: number; readonly shows: number } | { readonly words: string } {
  const said = NEXT_WORDS.exec(words);
  return said === null ? { words } : { left: Number(said[2]), shows: Number(said[1]) };
}

/** One filter as it holds still once pressed: its control's count and the rows it draws. */
export interface FilterReading {
  readonly count: string | null;
  readonly rows: readonly RowReading[];
}

/** Turns `filter` on, as `pickPreset` presses it, and reads it once it holds still. */
export async function readFilter(
  page: Page,
  traffic: ApiRequests,
  filter: TreeView
): Promise<FilterReading> {
  const name = PRESET_NAMES[filter];
  await pickPreset(page, name);
  await expectActivePreset(page, name);
  return steadily(page, traffic, `the ${name} filter`, async () => ({
    count: await presetCount(page, filter).textContent(),
    rows: await drawnRowReadings(page, { bodies: false }),
  }));
}

/** Turns `filter` off, which puts the page back on Conversation. */
export async function releaseFilter(page: Page, filter: TreeView): Promise<void> {
  const name = PRESET_NAMES[filter];
  await expect(presetToggle(page, filter), `the ${name} control is drawn`).toBeVisible();
  await presetToggle(page, filter).click();
  await expectActivePreset(page, PRESET_NAMES.conversation);
}

/** One request the page sent under `/api/` that is no read: its method, its path, and its body,
 *  as JSON where it is JSON and as its text otherwise. */
export interface SentWrite {
  readonly body: unknown;
  readonly method: string;
  readonly path: string;
}

/** A request's body, as JSON where it is JSON. `postDataJSON` throws on a body that is not, and
 *  such a body is kept as its text. */
function bodyOf(request: Request): unknown {
  try {
    return request.postDataJSON();
  } catch {
    return request.postData();
  }
}

/** Keeps every request the page sends under `/api/` from now on that is no `GET`, in the order
 *  the page sent them. */
export function watchWrites(page: Page): readonly SentWrite[] {
  const writes: SentWrite[] = [];
  page.on("request", (request) => {
    const url = new URL(request.url());
    if (request.method() === "GET" || !url.pathname.startsWith("/api/")) return;
    writes.push({ body: bodyOf(request), method: request.method(), path: url.pathname });
  });
  return writes;
}

/** The tree reads `holdTreeReads` holds. */
export interface HeldReads {
  /** How many requests of the tree route are held. */
  count(): number;
  /** Stops holding, and lets every held request go on to the server. */
  release(): Promise<void>;
}

/** Holds every request of issue `key`'s tree route the page sends from now on, its views' with
 *  it, until `release`. */
export async function holdTreeReads(page: Page, key: string): Promise<HeldReads> {
  const held: Route[] = [];
  const matches = treeReadRoute(key);
  const hold = (route: Route): void => {
    held.push(route);
  };
  await page.route(matches, hold);
  return {
    count: () => held.length,
    release: async () => {
      await page.unroute(matches, hold);
      for (const route of held.splice(0)) {
        // A read the page gave up while it was held - a newer read of the same query takes its
        // place - has no request left to go on, and Playwright refuses to continue it.
        await route.continue().catch(() => undefined);
      }
    },
  };
}

/** Row 2's third decision: its block, the words it is written with, and the words the row
 *  changes them to. */
export const THIRD_DECISION = {
  block: "d-tie-again",
  reworded: "Tie the first statement once again",
  words: "Tie the first statement a second time",
} as const;

/** The id of issue `key`'s spec, the document its decisions are blocks of. */
async function specOf(key: string): Promise<string> {
  const spec = (await getIssue(key)).primary_artifact_id;
  if (spec === undefined || spec === null) throw new Error(`${key} holds no spec`);
  return spec;
}

/**
 * Writes row 2's third decision at the end of issue `key`'s spec, through the spec's edit route
 * (`POST /api/v1/artifacts/{id}/edits`), as the planner, so it is an agent's: a `:::decision`
 * block whose `shaped` names the first statement `r1`, so it waits for review too, and newest of
 * the issue's decisions. Answers the spec's id.
 */
export async function writeThirdDecision(key: string): Promise<string> {
  const spec = await specOf(key);
  const statement = specBlockRef(key, requirementBlockId(1));
  const markdown =
    `:::decision{#${THIRD_DECISION.block} shaped="${statement}"}\n` +
    `${THIRD_DECISION.words}\n\nShapes ${statement}.\n:::`;
  await editArtifact(spec, { ops: [{ after: "end", markdown, op: "insert" }] }, asPlanner);
  return spec;
}

/** Changes the third decision's words in the spec `spec`, as the planner. */
export async function rewordThirdDecision(spec: string): Promise<void> {
  await editArtifact(
    spec,
    { ops: [{ find: THIRD_DECISION.words, op: "replace", with: THIRD_DECISION.reworded }] },
    asPlanner
  );
}

/** Row 2's second decision, `seedReviewList`'s `tiedOlder`: the words of its heading, and the
 *  words the row changes them to. Its first paragraph, which its Change quotes, is "Shapes" and
 *  the address of `r1`. */
export const SECOND_DECISION = {
  heading: "Tie the first statement",
  reworded: "Tie statement one",
} as const;

/**
 * Changes the words of row 2's second decision (`SECOND_DECISION`) in the spec `spec`, as the
 * planner: its heading, which a Change never quotes, since a Change quotes the decision's first
 * paragraph, a heading passed over (`decision-nodes.ts`' `decisionQuote`). So the decision's
 * words and its sum change while the words its Change quotes stand as they were. Row 2's third
 * decision says the heading's words too, after it, so the edit names their first match
 * (`occurrence` 0).
 */
export async function rewordSecondDecision(spec: string): Promise<void> {
  await editArtifact(
    spec,
    {
      ops: [
        {
          find: SECOND_DECISION.heading,
          occurrence: 0,
          op: "replace",
          with: SECOND_DECISION.reworded,
        },
      ],
    },
    asPlanner
  );
}

/** How many decisions row 4 writes at the foot of the Decisions view: more than the 20 a part
 *  draws at a time (`DecisionsFilterBody.tsx`' `PAGE`, root R40). */
const FOOT_DECISIONS = 25;

/** How many words each of row 4's decisions says: four paragraphs of forty (`longWords`), which
 *  run past six lines in the Decisions view's list at 1440 px, 960 px wide, as c-layout's long
 *  questions do. */
const FOOT_DECISION_WORDS = 160;

/**
 * Writes row 4's decisions at the end of issue `key`'s spec, through the spec's edit route, as
 * the planner, so each is an agent's: `FOOT_DECISIONS` `:::decision` blocks with no `shaped`
 * pointer, so no statement rests on any of them and each stands at the foot of the Decisions
 * view, each with words of its own that run past six lines at 1440 px. Answers their block ids,
 * in the order they are written.
 */
export async function writeFootDecisions(key: string): Promise<string[]> {
  const spec = await specOf(key);
  const blocks = Array.from({ length: FOOT_DECISIONS }, (_unused, at) => `d-foot-${at + 1}`);
  const markdown = blocks
    .map(
      (block, at) =>
        `:::decision{#${block}}\n${longWords(`foot${at + 1}w`, FOOT_DECISION_WORDS)}\n:::`
    )
    .join("\n\n");
  await editArtifact(spec, { ops: [{ after: "end", markdown, op: "insert" }] }, asPlanner);
  return blocks;
}
