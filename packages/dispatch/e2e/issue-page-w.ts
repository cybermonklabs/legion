import type {
  ConstraintRead,
  LensFinding,
  LensInvoked,
  LensKindRead,
  PointerWritten,
  RiskWritten,
} from "@legion/contracts";
import { expect, type Locator, type Page, type Response } from "@playwright/test";

import type { IssueTreeAnswer, IssueTreeNode } from "../web/src/api/client";
import {
  type Body,
  type Op,
  type PathParams,
  type PathsOf,
  type Result,
  routeUrl,
} from "../web/src/api/paths";
import type { LineName } from "../web/src/features/issue-page/lenses/LensLines";
import type { NodeType } from "../web/src/features/issue-page/page-words";
import { mergeTreePages } from "../web/src/features/issue-page/tree-read/tree-nodes";
import { baseUrlOf, getIssueCoverage, getIssueResults, userHeaders } from "./api";
import { issuePage } from "./issue-page";
import type { Gate, TreeAnswerWatch } from "./issue-page-t";

/**
 * What the rows of `e2e/issue-page-w.e2e.ts` write and read, and what any other row that needs
 * a node reserved for its author writes it with: the lenses, findings, risks and constraints of
 * an issue, and the rows and lines the new issue page draws for them
 * (`web/src/features/issue-page/lenses/`).
 *
 * - **The writers** send one request each as alice, through the server's own routes
 *   (`packages/envoy/internal/dispatch/api/reasoning_routes.go`), each under an
 *   `Idempotency-Key` of its own, and answer the body the route answers. A refusal throws,
 *   naming the method, the path, the status and the body.
 * - **The seed** `seedLenses` writes, on an issue `seedWholeIssue` made, the lenses, risks and
 *   constraints its own comment lists, and takes every address it does not write from a read of
 *   the server.
 * - **The readers**: the rows `[data-node-row]` of the four types, each line `[data-lens-line]`
 *   under a row's words (its label `[data-page-chrome]`, its chips `[data-node-chip]` with their
 *   words `[data-chip-name]`, and its stored words `[data-author-text]`), the focused row's act
 *   `[data-row-lens]`, the menu it opens (`role="menu"`, each `role="menuitem"` holding a kind's
 *   words `[data-lens-kind-words]` and its standard `[data-lens-kind-standard]`) with its line
 *   `[data-lens-menu-line]`, a focused lens's chain (`[data-lens-show-chain]`, each entry
 *   `[data-lens-chain-entry]`), a row by the id the tree read gives its node
 *   (`data-node-read-id`), and the page's own words.
 * - **The tree read**: what the page received of `GET /api/v1/issues/{key}/tree` - its newest
 *   read whose every page answered, `e2e/issue-page-t.ts`'s `treeAnswerOf`, held until it holds
 *   what a row waits for (`heldTreeAnswer`), and the pages of each read (`treePagesOf`) - and
 *   the read ids of the nodes `seedLenses` wrote, read from that answer (`lensReadIds`), so a
 *   row names each node by the id the page itself received and never builds one.
 * - **The gate** `findingGate`, which a clause of a row waits on as the rows wait on the gates
 *   of `e2e/issue-page-t.ts`: a `Gate`, read by that file's `owedUnless`.
 */

/** What the writers answer, each the 201 body of its route as `@legion/contracts` names it:
 *  `POST /api/v1/issues/{key}/lenses` the lens with its findings, `…/nodes/{id}/findings` the
 *  finding, `…/risks` the risk, `…/constraints` the constraint, and `…/nodes/{id}/pointers` both
 *  ends of the tuple. Other rows name them from here. */
export type { ConstraintRead, LensFinding, LensInvoked, PointerWritten, RiskWritten };

/** What `POST /api/v1/issues/{key}/constraints` takes. */
type ConstraintBody = Body<"/api/v1/issues/{key}/constraints", "post">;

/** Who a write is for: `project`, the default, or `author`, which keeps it to alice. */
type Visibility = Pick<ConstraintBody, "visibility">;

/** A constraint that is a standard, with the reference to the part of it that applies. */
export type StandardConstraint = Required<Pick<ConstraintBody, "reference" | "standard">>;

/** A constraint that is a measurement, with its value and where it was taken. */
export type MeasurementConstraint = Required<
  Pick<ConstraintBody, "measurement" | "taken_where" | "value">
>;

/** One write as alice: `body` posted as JSON to `pattern` with `params` in its path, each value
 *  percent-encoded as one segment (`routeUrl`), under an `Idempotency-Key` of its own. */
async function write<P extends PathsOf<"post">>(
  pattern: P,
  params: PathParams<Op<P, "post">>,
  body: Body<P, "post">
): Promise<Result<P, "post">> {
  const path = routeUrl(pattern, params);
  const response = await fetch(new URL(path, baseUrlOf()), {
    body: JSON.stringify(body),
    headers: {
      ...(await userHeaders("alice")),
      "Content-Type": "application/json",
      "Idempotency-Key": crypto.randomUUID(),
    },
    method: "POST",
  });
  if (!response.ok) {
    throw new Error(`POST ${path} failed: ${response.status} ${await response.text()}`);
  }
  return (await response.json()) as Result<P, "post">;
}

/** One read as alice of `pattern` with `params` in its path. */
async function read<P extends PathsOf<"get">>(
  pattern: P,
  params: PathParams<Op<P, "get">>
): Promise<Result<P, "get">> {
  const path = routeUrl(pattern, params);
  const response = await fetch(new URL(path, baseUrlOf()), {
    headers: await userHeaders("alice"),
  });
  if (!response.ok) {
    throw new Error(`GET ${path} failed: ${response.status} ${await response.text()}`);
  }
  return (await response.json()) as Result<P, "get">;
}

// ---------------------------------------------------------------------------------------------
// The writers.
// ---------------------------------------------------------------------------------------------

/** Invokes a lens of the catalogue's `kind` (a `lens_kind` id, such as `trade-off`) on the node
 *  of issue `key` that `on` addresses, with its verdict and the lens that triggered it where they
 *  are given. The answer's `address` is the lens's, `dispatch://lens/<uuid>`. */
export function invokeLens(
  key: string,
  input: Omit<Body<"/api/v1/issues/{key}/lenses", "post">, "actor" | "findings">
): Promise<LensInvoked> {
  return write("/api/v1/issues/{key}/lenses", { key }, input);
}

/** Adds one finding holding `text` to the lens `lens` addresses: the `address` `invokeLens`
 *  answers, which the route takes whole as its `{id}` (`FindNodeUnderIssue`). */
export function addFinding(
  key: string,
  lens: string,
  text: string,
  { visibility }: Visibility = {}
): Promise<LensFinding> {
  return write("/api/v1/issues/{key}/nodes/{id}/findings", { id: lens, key }, { text, visibility });
}

/** Writes one risk under the pre-mortem lens `input.lens` addresses, prevented by the check
 *  block `input.prevented_by` addresses where it is given. */
export function writeRisk(
  key: string,
  input: Omit<Body<"/api/v1/issues/{key}/risks", "post">, "actor">
): Promise<RiskWritten> {
  return write("/api/v1/issues/{key}/risks", { key }, input);
}

/** Writes one constraint under issue `key`: a standard with its reference, or a measurement
 *  with its value and where it was taken. */
export function writeConstraint(
  key: string,
  shape: StandardConstraint | MeasurementConstraint,
  { visibility }: Visibility = {}
): Promise<ConstraintRead> {
  return write("/api/v1/issues/{key}/constraints", { key }, { ...shape, visibility });
}

/** Appends one pointer tuple of `kind` out of the node `node` addresses to the node `to`
 *  addresses. Both take a node's `dispatch://` address: a statement's is the coverage read's
 *  `ref`, and a constraint's the `address` `writeConstraint` answers, or a catalogue
 *  constraint's `ref` from `GET /api/v1/schema/blocks`. */
export function pointAt(
  key: string,
  node: string,
  kind: Body<"/api/v1/issues/{key}/nodes/{id}/pointers", "post">["kind"],
  to: string
): Promise<PointerWritten> {
  return write(
    "/api/v1/issues/{key}/nodes/{id}/pointers",
    { id: node, key },
    { action: "append", kind, to }
  );
}

// ---------------------------------------------------------------------------------------------
// The seed.
// ---------------------------------------------------------------------------------------------

/** Every node `seedLenses` writes, by the server's answer for it, and every node it read the
 *  address of, by that address. */
export interface LensSeed {
  /** The constraint of the catalogue statement 6 is constrained by: the first source of the
   *  catalogue's first lens kind. */
  readonly catalogueConstraint: string;
  /** The first check of the Verification document, which prevents risk A. */
  readonly check1: string;
  /** The trade-off analysis's one finding. */
  readonly finding: LensFinding;
  /** The measurement constraint, which no statement points at. */
  readonly measurement: ConstraintRead;
  /** The pre-mortem on statement 1, triggered by the trade-off analysis, with a verdict. */
  readonly preMortem: LensInvoked;
  /** The risk under the pre-mortem that check 1 prevents. */
  readonly riskA: RiskWritten;
  /** The risk under the pre-mortem that nothing prevents. */
  readonly riskB: RiskWritten;
  /** The standard constraint, which statement 5 is constrained by. */
  readonly standard: ConstraintRead;
  /** The first, fifth and sixth statements of the spec, in document order. */
  readonly statements: { readonly five: string; readonly one: string; readonly six: string };
  /** The trade-off analysis on statement 1, with no verdict. */
  readonly tradeOff: LensInvoked;
}

/**
 * Writes, on the issue `key` that `seedWholeIssue` made: a trade-off analysis on statement 1
 * with one finding and no verdict; a pre-mortem on statement 1 triggered by it, with a verdict,
 * and two risks under it, A prevented by check 1 and B by nothing; a standard and a measurement
 * constraint; and statement 5 constrained by the standard and statement 6 by the catalogue's
 * `lens_kinds[0].sources[0].ref`. Each statement's and the check's address is read from the
 * server (the coverage read's `ref`, the results read's check `ref`), never built. The lens
 * kinds are the catalogue's ids (migration `20261116123354_lens_catalogue.up.sql`). The writes
 * go one after another, so the lenses of statement 1 stand in the order written.
 */
export async function seedLenses(key: string): Promise<LensSeed> {
  const [coverage, results, catalogue] = await Promise.all([
    getIssueCoverage(key),
    getIssueResults(key),
    lensCatalogue(),
  ]);
  const statement = (place: number): string => {
    const ref = coverage.requirements?.[place - 1]?.ref;
    if (ref === undefined)
      throw new Error(`the coverage read of ${key} holds no statement ${place}`);
    return ref;
  };
  const statements = { five: statement(5), one: statement(1), six: statement(6) };
  const check1 = results.checks?.[0]?.ref;
  if (check1 === undefined) throw new Error(`the results read of ${key} holds no check`);
  const catalogueConstraint = catalogue[0]?.sources?.[0]?.ref;
  if (catalogueConstraint === undefined) {
    throw new Error("the catalogue's first lens kind holds no source");
  }

  const tradeOff = await invokeLens(key, { kind: "trade-off", on: statements.one });
  const finding = await addFinding(
    key,
    tradeOff.address,
    "Folding the tree to the frontier saves a screen and costs a press for each branch."
  );
  const preMortem = await invokeLens(key, {
    kind: "pre-mortem",
    on: statements.one,
    triggered_by: tradeOff.address,
    verdict: "Ship it once every folded branch says how many rows it holds.",
  });
  const riskA = await writeRisk(key, {
    early_warning: "Questions are answered late on issues whose branches stay folded.",
    heading: "A reader misses a question folded under a closed branch.",
    lens: preMortem.address,
    prevented_by: check1,
    prevention: "The fold budget never folds a branch that holds an open question.",
  });
  const riskB = await writeRisk(key, {
    early_warning: "The tree read takes longer on issues that hold many lenses.",
    heading: "The page grows slower with every lens written under a statement.",
    lens: preMortem.address,
    prevention: "The tree read answers every lens of the issue in one request.",
  });
  const standard = await writeConstraint(key, {
    reference: "Success Criterion 2.4.11, Focus Not Obscured (Minimum)",
    standard: "Web Content Accessibility Guidelines 2.2",
  });
  const measurement = await writeConstraint(key, {
    measurement: "Rows drawn on the first screen",
    taken_where: "a 1440 by 900 desktop window on the seeded issue",
    value: "42",
  });
  await pointAt(key, statements.five, "constrained_by", standard.address);
  await pointAt(key, statements.six, "constrained_by", catalogueConstraint);

  return {
    catalogueConstraint,
    check1,
    finding,
    measurement,
    preMortem,
    riskA,
    riskB,
    standard,
    statements,
    tradeOff,
  };
}

// ---------------------------------------------------------------------------------------------
// The readers.
// ---------------------------------------------------------------------------------------------

/** The types of the rows `lenses/lens-drafts.ts` drafts. */
const LENS_ROW_TYPES = [
  "lens",
  "finding",
  "risk",
  "constraint",
] as const satisfies readonly NodeType[];

/** The selectors the readers pass into the browser. */
const SELECTORS = {
  authorText: "[data-author-text]",
  chip: "[data-node-chip]",
  chipName: "[data-chip-name]",
  kindStandard: "[data-lens-kind-standard]",
  kindWords: "[data-lens-kind-words]",
  lensLine: "[data-lens-line]",
  nodeRow: "[data-node-row]",
  pageChrome: "[data-page-chrome]",
} as const;

/** One chip of a lens line: the words it names its target by, and the link to the target's row
 *  (`/issues/<key>?node=<id>`), or null where it draws the words alone. */
export interface LensLineChip {
  readonly href: string | null;
  readonly words: string;
}

/** One line a row draws under its words from its lens facts. */
export interface LensLine {
  /** Its chips, in order. */
  readonly chips: readonly LensLineChip[];
  /** Its label, the page's own words; empty on a line that draws none (`kind`, `reference`). */
  readonly label: string;
  /** Its `data-lens-line`: the `LENS_WORDS` key of its label, or `kind` or `reference`. */
  readonly line: string;
  /** The stored words it draws outside its chips; empty on a line that draws none. */
  readonly words: string;
}

/** One kind of the lens menu, as its item draws it. */
export interface LensMenuItem {
  /** The kind's primary source (its standard). */
  readonly standard: string;
  /** The kind's words. */
  readonly words: string;
}

/** Every row of the page that stands for a lens, a finding, a risk or a constraint. */
export function lensRows(page: Page): Locator {
  return issuePage(page).locator(
    LENS_ROW_TYPES.map((type) => `${SELECTORS.nodeRow}[data-node-type="${type}"]`).join(", ")
  );
}

/** The lines one row draws under its words, in order, read from the row `row` locates. A line
 *  of a row drawn inside it is that row's, and is left out. */
export function lensLines(row: Locator): Promise<LensLine[]> {
  return row.evaluate((rowElement, selectors) => {
    const text = (elements: readonly Element[]): string =>
      elements
        .map((element) => element.textContent ?? "")
        .join(" ")
        .replace(/\s+/g, " ")
        .trim();
    return [...rowElement.querySelectorAll(selectors.lensLine)]
      .filter((line) => line.closest(selectors.nodeRow) === rowElement)
      .map((line) => {
        // The label and the stored words stand outside the chips, whose names are stored words.
        const loose = [
          ...line.querySelectorAll(`${selectors.pageChrome}, ${selectors.authorText}`),
        ].filter((element) => element.closest(selectors.chip) === null);
        return {
          chips: [...line.querySelectorAll(selectors.chip)].map((chip) => {
            const name = chip.querySelector(selectors.chipName);
            return {
              href: name?.closest("a[href]")?.getAttribute("href") ?? null,
              words: text(name === null ? [] : [name]),
            };
          }),
          label: text(loose.filter((element) => element.matches(selectors.pageChrome))),
          line: line.getAttribute("data-lens-line") ?? "",
          words: text(loose.filter((element) => element.matches(selectors.authorText))),
        };
      });
  }, SELECTORS);
}

/** The focused row's Lens act, which opens the menu of lens kinds. */
export function lensAct(row: Locator): Locator {
  return row.locator("[data-row-lens]");
}

/** The menu of lens kinds a row's Lens act opened. */
export function lensMenu(page: Page): Locator {
  return page.locator('[data-node-row] [role="menu"]');
}

/** Each item of the menu `menu` locates, in order, by the kind's words and its standard. */
export function menuItems(menu: Locator): Promise<LensMenuItem[]> {
  return menu.locator('[role="menuitem"]').evaluateAll(
    (items, selectors) =>
      items.map((item) => ({
        standard: (item.querySelector(selectors.kindStandard)?.textContent ?? "").trim(),
        words: (item.querySelector(selectors.kindWords)?.textContent ?? "").trim(),
      })),
    SELECTORS
  );
}

/** The id the page draws the node `readId` names under (`data-node-id`, the id the readers of
 *  `e2e/issue-page-g.ts` take), read off the first row that carries its read id
 *  (`data-node-read-id`) - a check under two statements is drawn twice under one id - or null
 *  where the page draws none. */
export async function drawnIdOf(page: Page, readId: string): Promise<string | null> {
  expect(readId, `${readId} cannot be written into a selector`).not.toContain('"');
  const rows = issuePage(page).locator(`${SELECTORS.nodeRow}[data-node-read-id="${readId}"]`);
  // One instant is this read's meaning: each caller polls it, or reads it after a poll.
  return (await rows.count()) === 0 ? null : rows.first().getAttribute("data-node-id");
}

/** The read id of the row that holds the focus itself, or null where the focus is on no row, or
 *  on a control inside one. */
export function focusedRowReadId(page: Page): Promise<string | null> {
  return page.evaluate((nodeRow) => {
    const focused = document.activeElement;
    return focused?.matches(nodeRow) === true ? focused.getAttribute("data-node-read-id") : null;
  }, SELECTORS.nodeRow);
}

/** The links of the chips one line of the row `row` locates draws (`data-lens-line`, the
 *  `LENS_WORDS` key of its label). */
export function lensChipLinks(row: Locator, line: LineName): Locator {
  return row.locator(`${SELECTORS.lensLine}[data-lens-line="${line}"] ${SELECTORS.chip} a[href]`);
}

/** The control of a focused lens that reads its chain. */
export function lensShowChain(row: Locator): Locator {
  return row.locator("[data-lens-show-chain]");
}

/** One entry of a drawn chain: its lens's kind's words, as the `lens_kind` node holds them, and
 *  the first line of the row that lens stands under; empty where it draws none. */
export interface LensChainEntry {
  readonly kind: string;
  readonly line: string;
}

/** The entries of the chain the row `row` locates draws, in order. */
export function lensChainEntries(row: Locator): Promise<LensChainEntry[]> {
  return row.locator("[data-lens-chain-entry]").evaluateAll((entries) =>
    entries.map((entry) => ({
      kind: (entry.querySelector("[data-lens-chain-kind]")?.textContent ?? "").trim(),
      line: (entry.querySelector("[data-lens-chain-line]")?.textContent ?? "").trim(),
    }))
  );
}

/** The line beside the open menu of lens kinds (`role="status"`), which says that the catalogue
 *  is being read or could not be, or that a pick is out; empty while it says nothing. */
export function lensMenuLine(page: Page): Locator {
  return page.locator(`${SELECTORS.nodeRow} [data-lens-menu-line]`);
}

/** One kind of the lens catalogue, as `GET /api/v1/schema/blocks` answers it. */
export type LensKind = LensKindRead;

/** The lens catalogue, in its own order, read as alice: what the menu of lens kinds lists. */
export async function lensCatalogue(): Promise<LensKind[]> {
  return (await read("/api/v1/schema/blocks", {})).lens_kinds ?? [];
}

// ---------------------------------------------------------------------------------------------
// What the page received of the tree read.
// ---------------------------------------------------------------------------------------------

/** The tree read's path, whatever issue it reads (`GET /api/v1/issues/{key}/tree`). */
const TREE_ROUTE = /^\/api\/v1\/issues\/[^/]+\/tree$/;

/** Whether `response` is an answer of the tree read with no view that the page could read. */
function treeReadAnswer(response: Response): boolean {
  const url = new URL(response.url());
  return (
    response.request().method() === "GET" &&
    TREE_ROUTE.test(url.pathname) &&
    url.searchParams.get("view") === null &&
    response.ok()
  );
}

/** How long `heldTreeAnswer` waits for the read a write sends again. */
const TREE_HELD_MS = 30_000;

/** Waits until the newest read `watch` holds (`e2e/issue-page-t.ts`'s `treeAnswerOf`) is one
 *  `holds` is true of, and answers it: that file's `heldAnswer` answers any read whose every
 *  page answered, and this the read a write sent again, once it holds what that write changed.
 *  A reload is no such wait: the read from before it already holds what a row looks for, so a
 *  row polls what the page draws. */
export async function heldTreeAnswer(
  watch: TreeAnswerWatch,
  what: string,
  holds: (answer: IssueTreeAnswer) => boolean
): Promise<IssueTreeAnswer> {
  const message = `${what} never answered the read the row waits for`;
  await expect
    .poll(
      () => {
        const answer = watch.answer();
        return answer !== undefined && holds(answer);
      },
      { message, timeout: TREE_HELD_MS }
    )
    .toBe(true);
  const answer = watch.answer();
  if (answer === undefined) throw new Error(message);
  return answer;
}

/** One page of the tree read as the page received it: the cursor its request sent (null for a
 *  read's first page) and the cursor it answered for the next (null on a read's last page). */
export interface TreePage {
  readonly cursor: string | null;
  readonly next: string | null;
}

/** Keeps each page of the tree read with no view the page receives from now on, so a row counts
 *  the pages a read has against the requests it sent; the answer reads them, in the order they
 *  arrived, once every page received so far has been read. A page whose body the browser let go
 *  of before it was read is none, as `e2e/issue-page-t.ts`'s `treeAnswerOf` reads it. */
export function treePagesOf(page: Page): () => Promise<TreePage[]> {
  const kept: Promise<TreePage | undefined>[] = [];
  page.on("response", (response) => {
    if (!treeReadAnswer(response)) return;
    const cursor = new URL(response.url()).searchParams.get("cursor");
    kept.push(
      response.json().then(
        (body: IssueTreeAnswer) => ({ cursor, next: body.cursor }),
        // A body the browser let go of before it was read, as the page moved on, is no page.
        () => undefined
      )
    );
  });
  return async () => (await Promise.all(kept)).filter((one) => one !== undefined);
}

/** The read ids of what `seedLenses` wrote, as the issue's tree read holds them. */
export interface LensReadIds {
  /** The check the `prevented_by` tuple of risk A names. */
  readonly check1: string;
  /** The node under the trade-off analysis: its one finding. */
  readonly finding: string;
  readonly measurement: string;
  readonly preMortem: string;
  readonly riskA: string;
  readonly riskB: string;
  readonly standard: string;
  /** The issue's own statements in the read's order, which is the spec's: statement N is
   *  `statements[N - 1]`, as `seedLenses` counts them from the coverage read. */
  readonly statements: readonly string[];
  /** The lens the pre-mortem's `triggered_by` tuple names. */
  readonly tradeOff: string;
}

/**
 * The read id of each node `seedLenses` wrote on issue `key`, found in `answer` by what the seed
 * wrote into it and by the server's own tuples, never built: the pre-mortem by its verdict, the
 * trade-off analysis by the pre-mortem's `triggered_by`, the finding as the node under it, each
 * risk by its heading and check 1 by the `prevented_by` of risk A, each constraint by its
 * standard or its measurement. A node of another issue, and the context a view or a catalogue
 * tuple brings in, is none of them. Throws, naming the node, where the answer holds none.
 */
export function lensReadIds(answer: IssueTreeAnswer, key: string, seeded: LensSeed): LensReadIds {
  const own = answer.nodes.filter(
    (node) => (node.issue === undefined || node.issue === key) && node.mark !== "context"
  );
  const idOf = (what: string, node: IssueTreeNode | undefined): string => {
    if (node === undefined) throw new Error(`the tree read of ${key} holds no ${what}`);
    return node.id;
  };
  const targetOf = (what: string, kind: string, from: string): string => {
    const tuple = answer.tuples.find((one) => one.kind === kind && one.from === from);
    if (tuple === undefined) throw new Error(`the tree read of ${key} holds no ${what}`);
    return tuple.to;
  };
  const holding = (supertag: string, attribute: string, value: string | null) =>
    own.find((node) => node.supertag === supertag && node.attributes[attribute] === value);
  const preMortem = idOf("pre-mortem", holding("lens", "verdict", seeded.preMortem.verdict));
  const tradeOff = targetOf("trigger of the pre-mortem", "triggered_by", preMortem);
  const riskA = idOf("risk A", holding("risk", "heading", seeded.riskA.heading));
  return {
    check1: targetOf("check that prevents risk A", "prevented_by", riskA),
    finding: idOf(
      "finding",
      own.find((node) => node.parent === tradeOff)
    ),
    measurement: idOf(
      "measurement constraint",
      holding("constraint", "measurement", seeded.measurement.measurement)
    ),
    preMortem,
    riskA,
    riskB: idOf("risk B", holding("risk", "heading", seeded.riskB.heading)),
    standard: idOf(
      "standard constraint",
      holding("constraint", "standard", seeded.standard.standard)
    ),
    statements: own.filter((node) => node.supertag === "requirement").map((node) => node.id),
    tradeOff,
  };
}

/** The read id of every lens `answer` holds, in its order. */
export function lensIdsIn(answer: IssueTreeAnswer): string[] {
  return answer.nodes.flatMap((node) => (node.supertag === "lens" ? [node.id] : []));
}

// ---------------------------------------------------------------------------------------------
// The gate: a clause whose server change is not on the base is a row of its own, which skips.
// ---------------------------------------------------------------------------------------------

/** Issue `key`'s tree read with no view, every page of it in order, read through the signed-in
 *  page's own request context, as `issue-page-t.ts`'s gates read it. */
async function requestedTreeRead(page: Page, key: string): Promise<IssueTreeAnswer> {
  const pageAt = async (cursor: string | null): Promise<IssueTreeAnswer> => {
    const path = routeUrl(
      "/api/v1/issues/{key}/tree",
      { key },
      cursor === null ? undefined : { cursor }
    );
    const response = await page.request.get(path);
    if (!response.ok()) {
      throw new Error(`GET ${path} failed: ${response.status()} ${await response.text()}`);
    }
    return (await response.json()) as IssueTreeAnswer;
  };
  const first = await pageAt(null);
  const rest: IssueTreeAnswer[] = [];
  let cursor = first.cursor;
  while (cursor !== null) {
    const next = await pageAt(cursor);
    rest.push(next);
    cursor = next.cursor;
  }
  return mergeTreePages([first, ...rest]);
}

/**
 * Whether the server's commit 5c85ffdc1b on the node model is on the base: it makes a lens
 * invoked on a finding point at it as `lens_block`, the kind the graph addresses a finding by,
 * so the tree read stands that lens under the finding. The probe invokes a lens on the finding
 * `seedLenses` wrote on issue `key` (`invokeLens`) and reads every page of the issue's tree read
 * through the signed-in page's own request context, as `issue-page-t.ts`'s `treeReadGate` reads
 * it: met only where that read holds a lens whose parent is the finding. The probe's lens stays,
 * a lens on the finding before the row's own.
 */
export async function findingGate(page: Page, key: string, seeded: LensSeed): Promise<Gate> {
  await invokeLens(key, { kind: "trade-off", on: seeded.finding.address });
  const answer = await requestedTreeRead(page, key);
  const { finding } = lensReadIds(answer, key, seeded);
  return {
    met: answer.nodes.some((node) => node.supertag === "lens" && node.parent === finding),
    owed:
      "owed until a lens invoked on a finding stands under that finding in the tree read " +
      "(the node model's commit 5c85ffdc1b)",
  };
}
