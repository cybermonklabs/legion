import { afterEach, beforeEach, expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, waitFor } from "@testing-library/react";
import { type ComponentProps, createElement, type ReactNode } from "react";
import { MemoryRouter, useLocation } from "react-router-dom";

import { api, REFERENCE_TARGETS_MAX, REFERENCE_TARGETS_MAX_QUERY_CHARS } from "../../../api/client";
import { referenceTargetsQuery } from "../../../api/queries";
import type { Artifact, IssueDetails, ReferenceTarget } from "../../../api/types";
import { warmMarkdownRenderer } from "../../refs/markdown-engine";
import {
  PublishedReferenceNames,
  type PublishedReferences,
  ReferenceNamesProvider,
  usePublishReferenceNames,
} from "../../refs/reference-names";
import { parseIssuePath } from "../../refs/routes";
import { blockReferences } from "../address-target";
import { artifact, issueDetails, KEY } from "../decisions-filter/decisions-fixtures";
import { buildIssueTree, type TreeNode, targetNaming } from "../issue-tree";
import { NodeRow } from "../NodeRow";
import { NoTooltips } from "../NoTooltips";
import { waitInAct } from "../outline-fixtures";
import { authorKind, CONTROL_LABELS } from "../page-words";
import { RowText } from "../RowText";
import { ProvidersSlot } from "../slots/providers";
import { StageSlot } from "../slots/stage";
import { AppProviders, stubReads } from "../slots/stage-fixtures";
import { treeContext } from "../tree-drafts";
import { nodesInOrder } from "../tree-keys";
import { wholeTreeOnly } from "../tree-read/tree-fixtures";
import type { TreeAnswer } from "../tree-read/tree-read";
import { treeReferences } from "../useIssueTree";
import { fixture } from "./graph-edit-fixtures";
import { lastHeldPath } from "./last-held";
import { targetLastHeld } from "./last-held-chips";
import {
  useLastHeldOpening,
  usePublishLastHeldOpening,
  usePublishLastHeldReferences,
} from "./last-held-store";

/**
 * A citation of a deleted node opens the version that last held it (R9 of INFRA-5705's spec):
 * `graph-edit/last-held.ts` and the surfaces that draw a reference to a node the server answers
 * gone (`GET /api/v1/reference-targets`, whose member `last_held` the server gained).
 *
 * The interface this file expects (the task that writes it reads it here):
 *
 * - `lastHeldPath(lastHeld: string): string | undefined` in `graph-edit/last-held.ts`: the address
 *   on the new page that a `last_held` address opens, `dispatch://<KEY>/<spec | artifact/verification-md
 *   | artifact/plan-md>#b-<id>@v<N>` for a node a query held, `dispatch://<KEY>/artifact/<slug>#b-<id>@v<N>`
 *   for any other block. A node of the spec, the Verification document or the plan opens that
 *   query's filter at the version, `/issues/<KEY>/<spec | verification | plan>?v=<N>#b-<id>`; any
 *   other block opens its document at the version, `/issues/<KEY>/artifacts/<slug>?v=<N>#b-<id>`.
 *   An address of any other shape answers undefined.
 * - `NodeChip.lastHeld?: string` (`issue-tree.ts`): `chipOf` (`tree-drafts.ts`) sets it from
 *   `ReferenceTarget.last_held` on a chip that is `gone`, and on no other.
 * - `usePublishLastHeldReferences(lastHeld)` (`graph-edit/last-held-store.ts`) publishes, beside
 *   the names the page publishes from the same read (`refs/reference-names.tsx`'s
 *   `usePublishReferenceNames`), the map of each gone reference to its `last_held`
 *   (`last-held-chips.ts`' `targetLastHeld`), and `useKnownLastHeldReferences()` hands that map
 *   to the surfaces under the page's query client.
 * - A gone chip with `lastHeld` draws `CONTROL_LABELS.target_gone` as a link to `lastHeldPath` of
 *   it, titled `CONTROL_LABELS.last_held_title` (`NodeRow.tsx`'s `Chip`); a gone reference in a
 *   row's words (`RowText.tsx`, `row-references.ts`) draws the same link in place; with no
 *   `lastHeld` both draw as today.
 * - An address on the issue page whose `#b-<block>` names a block that no read holds (the Spec
 *   view's own address: `address-target.ts`' `lastHeldAsk`, through the page's names read in
 *   `useIssueTree.ts`, which publishes where it opens by the issue's key,
 *   `usePublishLastHeldOpening(issue, address)`, read by `useLastHeldOpening(issue)` and opened by
 *   `slots/view-spec.tsx`) sends one `reference-targets` read for it and opens `lastHeldPath` of
 *   its `last_held`, replacing the address; where the read answers no `last_held`, the address
 *   opens without the block (no `#b-` fragment) and draws no line.
 * - An address of the issue's own route, Graph's, or the Plan or Verification filter's whose
 *   `#b-<block>` names, in no document, a block no read holds (`address-target.ts`'
 *   `useUnqualifiedLastHeld`, opened by `slots/body.tsx`) sends one `reference-targets` read of
 *   the block in each document of the issue - the spec, the Verification document, the plan and
 *   the tree artifact first, as many as one request carries (`blockReferences`) - once every page
 *   of the reads its view draws from is read, and opens the one version its answer names; where
 *   it names none, or versions of more than one address, the address opens without the block and
 *   draws no line.
 *
 * The answers are the server's own, from `__fixtures__/graph-edit.json`'s `targets` member: the
 * `GET /api/v1/reference-targets` answer (`{ request, status, body: { targets } }`) for a live
 * node, a gone statement (a block of the spec), a gone check (a block of the Verification
 * document), a gone plan task (a block of the plan), a gone block of the issue's own fragment and
 * a gone lens, each gone block with the `last_held` the server gave it.
 */

const NAMED = "<issue>";

/** `value` with the fixture's name of the issue written as the key the routes take: a name for a
 *  name, no other change to what the server answered. */
function renamed<T>(value: T): T {
  return JSON.parse(JSON.stringify(value).replaceAll(NAMED, KEY)) as T;
}

const members: Readonly<Record<string, unknown>> = fixture;
// The member a later regeneration of the fixture holds.
const exchange = members.targets as { body: { targets: ReferenceTarget[] } } | undefined;

function allTargets(): ReferenceTarget[] {
  if (exchange === undefined) throw new Error("the fixture holds no `targets` member");
  return renamed(exchange.body.targets);
}

/** The target of the fixture that is gone and whose address matches `pattern`. */
function gone(pattern: RegExp): ReferenceTarget {
  const found = allTargets().find((target) => target.gone && pattern.test(target.ref));
  if (found === undefined) throw new Error(`the fixture holds no gone target of ${pattern}`);
  return found;
}

const STATEMENT = () => gone(/\/spec#b-/u);
const CHECK = () => gone(/\/artifact\/verification-md#b-/u);
const TASK = () => gone(/\/artifact\/plan-md#b-/u);
const PROSE = () => gone(/\/artifact\/tree#b-/u);
const LENS = () => gone(/^dispatch:\/\/lens\//u);

/** What a `last_held` address holds: the version `N`, the block, and the address without it. */
function partsOf(target: ReferenceTarget): { address: string; block: string; version: string } {
  const held = target.last_held ?? "";
  const matched = /^(dispatch:\/\/[^#]+)#b-(.+)@v(\d+)$/u.exec(held);
  if (matched === null) throw new Error(`${target.ref} answers last_held ${JSON.stringify(held)}`);
  return { address: matched[1] ?? "", block: matched[2] ?? "", version: matched[3] ?? "" };
}

/** `path` as a URL, to compare its parts apart from how the fragment is escaped. */
function urlOf(path: string | undefined): URL {
  if (path === undefined) throw new Error("the address opens nowhere");
  return new URL(path, "http://page.test");
}

// R9: a gone block that a query held opens that query's filter at the newest version whose answer
// held it, with the block in the fragment: the spec's for a statement, the Verification's for a
// check, the plan's for a plan task. A node of the Verification document does not open the Spec
// filter.
test("last_held opens the filter of the query that held the node", () => {
  const statement = partsOf(STATEMENT());
  expect(statement.address).toBe(`dispatch://${KEY}/spec`);
  const spec = urlOf(lastHeldPath(STATEMENT().last_held ?? ""));
  expect(spec.pathname).toBe(`/issues/${KEY}/spec`);
  expect(spec.searchParams.get("v")).toBe(statement.version);
  expect(decodeURIComponent(spec.hash)).toBe(`#b-${statement.block}`);

  const check = partsOf(CHECK());
  expect(check.address).toBe(`dispatch://${KEY}/artifact/verification-md`);
  const verification = urlOf(lastHeldPath(CHECK().last_held ?? ""));
  expect(verification.pathname).toBe(`/issues/${KEY}/verification`);
  expect(verification.searchParams.get("v")).toBe(check.version);
  expect(decodeURIComponent(verification.hash)).toBe(`#b-${check.block}`);

  const task = partsOf(TASK());
  expect(task.address).toBe(`dispatch://${KEY}/artifact/plan-md`);
  const plan = urlOf(lastHeldPath(TASK().last_held ?? ""));
  expect(plan.pathname).toBe(`/issues/${KEY}/plan`);
  expect(plan.searchParams.get("v")).toBe(task.version);
  expect(decodeURIComponent(plan.hash)).toBe(`#b-${task.block}`);

  // An address that is no last_held opens nowhere.
  expect(lastHeldPath(`dispatch://${KEY}/spec#b-${statement.block}`)).toBeUndefined();
  expect(lastHeldPath("not an address")).toBeUndefined();
});

// R9: any other block opens its document at the version, by its slug.
test("last_held of another block opens its document at that version", () => {
  const prose = partsOf(PROSE());
  expect(prose.address).toBe(`dispatch://${KEY}/artifact/tree`);
  const opened = urlOf(lastHeldPath(PROSE().last_held ?? ""));
  expect(opened.pathname).toBe(`/issues/${KEY}/artifacts/tree`);
  expect(opened.searchParams.get("v")).toBe(prose.version);
  expect(decodeURIComponent(opened.hash)).toBe(`#b-${prose.block}`);
  // A lens holds no block of a document's text: its target answers no last_held.
  expect(LENS().last_held).toBeUndefined();
});

/** The node row the page draws for a chip: the chip's own link where it draws one. */
function drawRow(chip: TreeNode["chips"][number]) {
  const node: TreeNode = {
    alsoUnder: [],
    children: [],
    chips: [chip],
    facts: [],
    gaps: [],
    held: 0,
    id: "spec#b-r1",
    question: false,
    replaced: false,
    text: "A statement that cites a node.",
    transcluded: false,
    type: "requirement",
    waiting: false,
  };
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    createElement(
      QueryClientProvider,
      { client },
      createElement(
        MemoryRouter,
        null,
        createElement(
          NoTooltips,
          null,
          createElement(NodeRow, {
            author: {
              kind: authorKind(undefined),
              name: CONTROL_LABELS.unknown_author,
              reader: false,
            },
            context: false,
            focused: false,
            isClosed: false,
            level: 1,
            node,
            onFocus: () => {},
            onToggle: () => {},
            ownAnswer: false,
            rowKey: "row",
            tabStop: false,
          })
        )
      )
    )
  );
}

/** Publishes what the page publishes from its one batched read: its names (`published`) and the
 *  versions that last held its gone references (`lastHeld`), and draws `children` with the
 *  page's names handed to them in the render that draws them, as the outline is drawn
 *  (`PublishedReferenceNames`, `IssueTree.tsx`), so no surface reads a reference the page asks
 *  for. */
function Publish({
  children,
  lastHeld,
  published,
}: {
  children?: ReactNode;
  lastHeld: ReadonlyMap<string, string | undefined>;
  published: PublishedReferences;
}): ReactNode {
  usePublishReferenceNames(published);
  usePublishLastHeldReferences(lastHeld);
  // The provider's type holds its children among its props, as `ProvidersSlot`'s does (`Stage`).
  const props: ComponentProps<typeof PublishedReferenceNames> = { children, published };
  return createElement(PublishedReferenceNames, props);
}

/** What the page publishes of `reads`' batched read, as `useIssueTree.ts` publishes it: each
 *  target as a chip names it (`targetNaming`), with `asked` the references the page asks for,
 *  every target by default. */
function publishedOf(
  reads: Parameters<typeof targetNaming>[0],
  asked: readonly string[] = (reads.targets ?? []).map((target) => target.ref)
): PublishedReferences {
  return { ...targetNaming(reads), asked: new Set(asked) };
}

// R9: every surface that draws a reference to a gone node reads `last_held`, none by a new import of
// the model: `chipOf` carries it into `NodeChip`, the row's `Chip` draws it, and a row's words and
// the versions the page publishes (`last-held-store.ts`, `row-references.ts`, `RowText.tsx`) draw
// it in place. The address fallback (`address-target.ts`, `useIssueTree.ts`,
// `slots/view-spec.tsx`, `slots/body.tsx`) is the cases that draw the page at an address
// (`drawAt`).
test("every surface that draws a gone reference reads last_held", async () => {
  await warmMarkdownRenderer();
  const targets = allTargets();
  const statement = STATEMENT();
  const expected = urlOf(lastHeldPath(statement.last_held ?? ""));
  const reads = { issue: issueDetails(), targets, tree: undefined as TreeAnswer | undefined };

  // `chipOf` and `NodeChip`: the gone chip carries it, a live one and a gone lens carry none.
  const context = treeContext(reads);
  const chip = context.chipOf(statement.ref);
  expect(chip).toMatchObject({ gone: true, lastHeld: statement.last_held });
  const live = targets.find((target) => !target.gone);
  expect(live, "the fixture holds a live target").toBeDefined();
  expect(context.chipOf(live?.ref ?? "").lastHeld).toBeUndefined();
  expect(context.chipOf(LENS().ref).lastHeld).toBeUndefined();

  // `Chip` of `NodeRow.tsx`: Deleted, as a link to the version.
  const row = drawRow(chip);
  const link = [...row.container.querySelectorAll("a")].find(
    (each) => each.textContent === CONTROL_LABELS.target_gone
  );
  expect(link, "the gone chip draws no link").toBeDefined();
  expect(urlOf(link?.getAttribute("href") ?? "").pathname).toBe(expected.pathname);
  expect(urlOf(link?.getAttribute("href") ?? "").searchParams.get("v")).toBe(
    expected.searchParams.get("v")
  );
  expect(link?.getAttribute("title")).toBe(CONTROL_LABELS.last_held_title);
  row.unmount();
  // With no `lastHeld` the chip draws as it does today: Deleted, no link.
  const bare = drawRow({ ...chip, lastHeld: undefined } as typeof chip);
  expect(bare.container.textContent).toContain(CONTROL_LABELS.target_gone);
  expect(
    [...bare.container.querySelectorAll("a")].filter(
      (each) => each.textContent === CONTROL_LABELS.target_gone
    )
  ).toEqual([]);
  bare.unmount();

  // `last-held-store.ts`, `row-references.ts` and `RowText.tsx`: a row's words that cite the gone
  // node draw the same link, in place, once the reading is painted.
  const lastHeld = new Map([[statement.ref, statement.last_held ?? ""]]);
  // Under a query client, as the page draws a row's words: who is signed in is the read
  // `stubReads` answers.
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const words = render(
    createElement(
      QueryClientProvider,
      { client },
      createElement(
        MemoryRouter,
        null,
        createElement(
          ReferenceNamesProvider,
          null,
          createElement(
            Publish,
            { lastHeld, published: publishedOf(reads) },
            createElement(RowText, { markdown: `See ${statement.ref}.` })
          )
        )
      )
    )
  );
  await waitFor(() =>
    expect(
      words.container.querySelector<HTMLAnchorElement>("a[data-row-reference]")?.textContent,
      "the row's words draw the gone reference as Deleted"
    ).toBe(CONTROL_LABELS.target_gone)
  );
  const cited = words.container.querySelector<HTMLAnchorElement>("a[data-row-reference]");
  expect(urlOf(cited?.getAttribute("href") ?? "").pathname).toBe(expected.pathname);
  expect(urlOf(cited?.getAttribute("href") ?? "").searchParams.get("v")).toBe(
    expected.searchParams.get("v")
  );
  words.unmount();
});

// R9: where the Spec view's address opens instead is published by the page's names read, by the
// issue's key (`last-held-store.ts`' `usePublishLastHeldOpening`), and the view's slot reads it
// there (`useLastHeldOpening`, `slots/view-spec.tsx`), building no tree of its own. It stands
// while its publisher names it and goes when the publisher names none or leaves, and a reader of
// another issue reads none.
test("the Spec view's opening is read by the issue's key while its publisher names it", () => {
  const opened = lastHeldPath(STATEMENT().last_held ?? "");
  if (opened === undefined) throw new Error("the statement's last_held opens nowhere");
  const other = `${KEY}-OTHER`;
  const seen: { other?: string; own?: string } = {};
  function Read(): ReactNode {
    seen.own = useLastHeldOpening(KEY);
    seen.other = useLastHeldOpening(other);
    return null;
  }
  function Opening({ address }: { address: string | undefined }): ReactNode {
    usePublishLastHeldOpening(KEY, address);
    return null;
  }
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const draw = (publisher: ReactNode) =>
    createElement(QueryClientProvider, { client }, publisher, createElement(Read));
  const view = render(draw(createElement(Opening, { address: opened })));
  expect(seen.own, "the published opening was not read").toBe(opened);
  expect(seen.other, "another issue read this issue's opening").toBeUndefined();
  view.rerender(draw(createElement(Opening, { address: undefined })));
  expect(seen.own, "the opening stayed after its publisher named none").toBeUndefined();
  view.rerender(draw(createElement(Opening, { address: opened })));
  expect(seen.own).toBe(opened);
  view.rerender(draw(null));
  expect(seen.own, "the opening stayed after its publisher left").toBeUndefined();
  view.unmount();
});

/** A stub of every read the page's outline is drawn from, answering the references the page asks
 *  for with the targets of the fixture, and the tree with the whole read. */
let stubs: { mockRestore(): void }[] = [];
const asked: string[][] = [];
beforeEach(() => {
  asked.length = 0;
  stubs = stubReads();
});
afterEach(() => {
  cleanup();
  for (const stub of stubs) stub.mockRestore();
});

/** The tree read of the issue as the server answered it: the held tree read has no node of
 *  the gone blocks (the nodes were deleted since). */
const held = renamed(fixture.tree.converted.person.body) as unknown as TreeAnswer;

/** Where the router stands, as the stage last drew it. */
function Where({ seen }: { seen: { at: string } }): ReactNode {
  const { hash, pathname, search } = useLocation();
  seen.at = `${pathname}${search}${hash}`;
  return null;
}

function Stage({ issue }: { issue: IssueDetails }): ReactNode {
  const { pathname, search } = useLocation();
  const route = parseIssuePath(pathname, search);
  if (route === undefined) throw new Error(`${pathname} names no route`);
  // The slot's type holds its children among its props (`ProvidersSlot`).
  const props: ComponentProps<typeof ProvidersSlot> = {
    children: createElement(StageSlot, { issue, route }),
    issue,
    route,
  };
  return createElement(ProvidersSlot, props);
}

/** What the stub of the tree read answers: the tree's read, each page by its cursor, and each view
 *  of it. */
type TreeStub = (
  key: string,
  query?: { readonly cursor?: string; readonly view?: string }
) => Promise<TreeAnswer>;

/** The page at `path`, its `reference-targets` read answered by `answer`, which is told each read's
 *  references; for `issue`, `issueDetails()` where none is given, with its tree read answered by
 *  `tree`, the held read whole and no view where none is given. */
function drawAt(
  path: string,
  answer: (refs: string[]) => ReferenceTarget[],
  {
    issue = issueDetails(),
    tree = wholeTreeOnly(held),
  }: { readonly issue?: IssueDetails; readonly tree?: TreeStub } = {}
) {
  spyOn(api, "getIssueTree").mockImplementation(tree);
  spyOn(api, "getReferenceTargets").mockImplementation(async (refs: string[]) => {
    asked.push(refs);
    return { targets: answer(refs) };
  });
  const seen = { at: "" };
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  // What the app provides above the page, its children among its props as its type holds them.
  const providers: ComponentProps<typeof AppProviders> = {
    children: createElement(Stage, { issue }),
    client,
  };
  const view = render(
    createElement(
      MemoryRouter,
      { initialEntries: [path] },
      createElement(Where, { seen }),
      createElement(AppProviders, providers)
    )
  );
  return { seen, view };
}

// R9, R12: an address whose block no read holds sends one `reference-targets` read for it, and
// opens `last_held` where the read answers one. The drawing at that address is the part's that draws
// `?v=` (decisions 463 and 466 (2)): this case reads the address the page opens.
test("an address whose block no read holds opens last_held", async () => {
  const target = STATEMENT();
  const { block } = partsOf(target);
  expect(
    held.nodes?.some((node) => node.block_id === block),
    "the tree read holds the block the case asks for"
  ).toBe(false);
  const { seen } = drawAt(`/issues/${KEY}/spec#b-${block}`, (refs) =>
    allTargets().filter((one) => refs.includes(one.ref))
  );
  const opened = urlOf(lastHeldPath(target.last_held ?? ""));
  await waitFor(
    () => expect(urlOf(seen.at).searchParams.get("v")).toBe(opened.searchParams.get("v")),
    {
      onTimeout: () => new Error(`the address stayed ${seen.at}`),
    }
  );
  expect(urlOf(seen.at).pathname).toBe(opened.pathname);
  expect(decodeURIComponent(urlOf(seen.at).hash)).toBe(`#b-${block}`);
  // One read for the block, the one of R12.
  expect(asked.filter((refs) => refs.includes(target.ref))).toHaveLength(1);
});

// R9: where the read answers the block gone with no last_held (a lens holds no block of a
// document's text), the address opens without the block and no line says so. The fixture's
// answer for a gone lens is answered for the address the page asks: only its `ref` is the
// address's own.
test("an address with no last_held opens without the block and no line", async () => {
  const lens = LENS();
  const block = "no-such-block";
  const ref = `dispatch://${KEY}/spec#b-${block}`;
  const { seen, view } = drawAt(`/issues/${KEY}/spec#b-${block}`, (refs) =>
    refs.includes(ref) ? [{ ...lens, ref }] : []
  );
  await waitFor(() => expect(asked.filter((refs) => refs.includes(ref))).toHaveLength(1));
  await waitFor(() => expect(urlOf(seen.at).hash, "the address kept the block").toBe(""));
  expect(urlOf(seen.at).pathname).toBe(`/issues/${KEY}/spec`);
  expect(urlOf(seen.at).searchParams.has("v"), "the address opened a version").toBe(false);
  expect(
    view.queryByRole("heading", { name: "No row for this block" }),
    "the address drew the line that says no row stands for the block"
  ).toBeNull();
});

/** The issue with its Verification document, its plan and its tree artifact beside its spec, each
 *  in its role and under the id the fixture's tree read names it by: the documents a block a
 *  fragment names in no document is looked for in. `more` are further documents of the issue. */
function documentedIssue(...more: Artifact[]): IssueDetails {
  const issue = issueDetails();
  const verification = artifact("<verification>", "verification-md");
  const plan = artifact("<plan>", "plan-md");
  const tree: Artifact = { ...artifact(held.tree ?? "", "tree"), kind: "tree" };
  return {
    ...issue,
    artifacts: [...(issue.artifacts ?? []), verification, plan, tree, ...more],
    plan_artifact_id: plan.id,
    verification_artifact_id: verification.id,
  };
}

/** The block of the deleted statement, which no read holds, as a fragment of no document names it,
 *  and its reference in each document of `documentedIssue`, in the order the page asks them. */
const UNQUALIFIED = "last-held-statement";
const IN_DOCUMENTS = [
  `dispatch://${KEY}/spec#b-${UNQUALIFIED}`,
  `dispatch://${KEY}/artifact/verification-md#b-${UNQUALIFIED}`,
  `dispatch://${KEY}/artifact/plan-md#b-${UNQUALIFIED}`,
  `dispatch://${KEY}/artifact/tree#b-${UNQUALIFIED}`,
] as const;

/** The one read of the block in each document: the reads that name it in the plan, which no other
 *  read of the page names it in. */
function blockReads(block = UNQUALIFIED): string[][] {
  return asked.filter((refs) => refs.includes(`dispatch://${KEY}/artifact/plan-md#b-${block}`));
}

/** The fixture's answer for each reference asked, but for those `given` answers as it gives. */
function answering(given: ReadonlyMap<string, ReferenceTarget>) {
  return (refs: string[]): ReferenceTarget[] =>
    refs.flatMap((ref) => {
      const member = given.get(ref) ?? allTargets().find((one) => one.ref === ref);
      return member === undefined ? [] : [member];
    });
}

// R9, R12 and the INFRA-5705 architect's decision on t12: a fragment of no document - on the
// issue's own address, Graph's, Plan's and Verification's - naming a block no read holds sends one
// `reference-targets` read of it in each document of the issue, the spec, the Verification
// document, the plan and the tree artifact first, and opens the one version the answer names. Two
// documents that answer the same version open it.
test("a fragment of no document whose block no read holds opens the one version the read names", async () => {
  expect(
    held.nodes?.some((node) => node.block_id === UNQUALIFIED),
    "the tree read holds the block the case asks for"
  ).toBe(false);
  const statement = STATEMENT();
  expect(statement.ref).toBe(IN_DOCUMENTS[0]);
  const opened = urlOf(lastHeldPath(statement.last_held ?? ""));
  // The Verification document answers the same version as the spec, which opens it.
  const verification = IN_DOCUMENTS[1];
  const agreeing = new Map<string, ReferenceTarget>([
    [verification, { ...statement, ref: verification }],
  ]);
  // The Plan and Verification filters' own view reads, which hold no node of the block.
  const tree: TreeStub = (key, query = {}) =>
    query.view === undefined
      ? wholeTreeOnly(held)(key, query)
      : Promise.resolve({ ...held, nodes: [], tuples: [] });
  for (const preset of ["", "/graph", "/plan", "/verification"]) {
    asked.length = 0;
    const from = `/issues/${KEY}${preset}#b-${UNQUALIFIED}`;
    const { seen, view } = drawAt(from, answering(agreeing), {
      issue: documentedIssue(),
      tree,
    });
    await waitFor(
      () => expect(urlOf(seen.at).searchParams.get("v")).toBe(opened.searchParams.get("v")),
      { onTimeout: () => new Error(`${from} stayed ${seen.at}`) }
    );
    expect(urlOf(seen.at).pathname, from).toBe(opened.pathname);
    expect(decodeURIComponent(urlOf(seen.at).hash), from).toBe(`#b-${UNQUALIFIED}`);
    // One read, the one of R12, naming the block once in each document.
    const reads = blockReads();
    expect(reads, `${from} read the block other than once`).toHaveLength(1);
    expect([...(reads[0] ?? [])].sort(), from).toEqual([...IN_DOCUMENTS].sort());
    view.unmount();
  }
});

// The decision on t12: where the documents answer versions of more than one address, the page
// does not choose between them, and the address opens without the block, with no line. The answers
// are the fixture's for a deleted statement and a deleted block of the tree artifact, each under
// the block's reference in its own document.
test("a fragment of no document whose block two documents last held at two versions opens without the block", async () => {
  const { seen, view } = drawAt(
    `/issues/${KEY}#b-${UNQUALIFIED}`,
    answering(
      new Map<string, ReferenceTarget>([[IN_DOCUMENTS[3], { ...PROSE(), ref: IN_DOCUMENTS[3] }]])
    ),
    { issue: documentedIssue() }
  );
  await waitFor(() => expect(blockReads()).toHaveLength(1));
  await waitFor(() => expect(urlOf(seen.at).hash, "the address kept the block").toBe(""));
  expect(urlOf(seen.at).pathname).toBe(`/issues/${KEY}`);
  expect(urlOf(seen.at).searchParams.has("v"), "the address opened a version").toBe(false);
  expect(
    view.queryByRole("heading", { name: "No row for this block" }),
    "the address drew the line that says no row stands for the block"
  ).toBeNull();
  expect(blockReads(), "the block was read again").toHaveLength(1);
});

// R9: where no document answers the block gone with a version - live in one, gone with no version
// in the others - the address opens without the block, with no line.
test("a fragment of no document whose block the read answers with no version opens without the block and no line", async () => {
  const live = allTargets().find((target) => !target.gone);
  if (live === undefined) throw new Error("the fixture holds no live target");
  const [spec, ...others] = IN_DOCUMENTS;
  const { seen, view } = drawAt(
    `/issues/${KEY}/graph#b-${UNQUALIFIED}`,
    answering(
      new Map<string, ReferenceTarget>([
        [spec, { ...live, ref: spec }],
        ...others.map((ref): [string, ReferenceTarget] => [ref, { ...LENS(), ref }]),
      ])
    ),
    { issue: documentedIssue() }
  );
  await waitFor(() => expect(blockReads()).toHaveLength(1));
  await waitFor(() => expect(urlOf(seen.at).hash, "the address kept the block").toBe(""));
  expect(urlOf(seen.at).pathname).toBe(`/issues/${KEY}/graph`);
  expect(urlOf(seen.at).searchParams.has("v"), "the address opened a version").toBe(false);
  expect(
    view.queryByRole("heading", { name: "No row for this block" }),
    "the address drew the line that says no row stands for the block"
  ).toBeNull();
});

// R12 and R15: the page reads the outline's next pages for the address's block first. A block a
// later page holds lands there and sends no `reference-targets` read of it; one no page holds is
// read once, after the last page.
test("a fragment of no document is read for only once every page of the outline read lacks its block", async () => {
  const pages = fixture.paged.pages.map((page) => renamed(page.body) as unknown as TreeAnswer);
  const [first, second] = pages;
  if (first === undefined || second === undefined) throw new Error("the fixture holds no 2 pages");
  const later = "s2";
  expect(first.nodes?.some((node) => node.block_id === later)).toBe(false);
  expect(second.nodes?.some((node) => node.block_id === later)).toBe(true);
  const events: string[] = [];
  const tree: TreeStub = (_key, query = {}) => {
    if (query.view !== undefined) return Promise.withResolvers<never>().promise;
    events.push(query.cursor === undefined ? "first page" : "next page");
    return Promise.resolve(query.cursor === undefined ? first : second);
  };
  const answer = (refs: string[]): ReferenceTarget[] => {
    if (refs.includes(IN_DOCUMENTS[2])) events.push("block read");
    return answering(new Map())(refs);
  };

  const onLater = drawAt(`/issues/${KEY}#b-${later}`, answer, { issue: documentedIssue(), tree });
  await waitFor(() => expect(events).toContain("next page"));
  await waitInAct(50);
  expect(blockReads(later), "the block a later page holds was read for").toEqual([]);
  expect(onLater.seen.at, "the address of a block a later page holds lost it").toBe(
    `/issues/${KEY}#b-${later}`
  );
  onLater.view.unmount();

  events.length = 0;
  const opened = urlOf(lastHeldPath(STATEMENT().last_held ?? ""));
  const { seen } = drawAt(`/issues/${KEY}#b-${UNQUALIFIED}`, answer, {
    issue: documentedIssue(),
    tree,
  });
  await waitFor(() => expect(urlOf(seen.at).pathname).toBe(opened.pathname));
  expect(events.filter((event) => event !== "first page")).toEqual(["next page", "block read"]);
});

// R12: the read of a block a fragment names in no document is one request. The spec, the
// Verification document, the plan and the tree artifact come first, each document once, and the
// documents past the server's cap (`REFERENCE_TARGETS_MAX`) or the client's budget for one request
// (`REFERENCE_TARGETS_MAX_QUERY_CHARS`) are left out.
test("the read of a block a fragment names in no document asks the role documents first, within one request", async () => {
  const notes = Array.from({ length: REFERENCE_TARGETS_MAX + 1 }, (_, at) =>
    artifact(`artifact-notes-${at}`, `notes-${at}`)
  );
  const refs = blockReferences({
    block: UNQUALIFIED,
    issue: documentedIssue(...notes),
    key: KEY,
    tree: held.tree,
  });
  expect(refs.slice(0, IN_DOCUMENTS.length)).toEqual([...IN_DOCUMENTS]);
  expect(new Set(refs).size, "a document was asked twice").toBe(refs.length);
  const written = (named: readonly string[]) =>
    new URLSearchParams(named.map((ref) => ["ref", ref])).toString().length;
  expect(refs.length).toBeLessThanOrEqual(REFERENCE_TARGETS_MAX);
  expect(written(refs)).toBeLessThanOrEqual(REFERENCE_TARGETS_MAX_QUERY_CHARS);
  // Each other document in the issue's order, as many as one request carries: the next one would
  // pass a bound.
  const rest = refs.slice(IN_DOCUMENTS.length);
  const inNotes = notes.slice(0, rest.length).map((one) => one.slug);
  expect(rest).toEqual(
    inNotes.map((slug) => `dispatch://${KEY}/artifact/${slug}#b-${UNQUALIFIED}`)
  );
  const next = `dispatch://${KEY}/artifact/notes-${rest.length}#b-${UNQUALIFIED}`;
  expect(
    refs.length === REFERENCE_TARGETS_MAX ||
      written([...refs, next]) > REFERENCE_TARGETS_MAX_QUERY_CHARS,
    "a document one request carries was left out"
  ).toBe(true);
  // The page's batched read sends them in one request.
  const requests: string[][] = [];
  spyOn(api, "getReferenceTargets").mockImplementation(async (named: string[]) => {
    requests.push(named);
    return { targets: [] };
  });
  await new QueryClient().fetchQuery(referenceTargetsQuery(refs));
  expect(requests).toHaveLength(1);
});

// R9 and c-citation: a comment of the person that cites a node deleted in Graph reads it as
// Deleted in its words and on a chip of its row, each a link to the one version that last held
// the node. The chip is the one `chipOf` makes from the batched read's answer, which the row
// gains where its words cite the node (`last-held-chips.ts`' `addLastHeldChips`): once however
// often they cite it, after the chip its words earn for a document, and on no row whose words do
// not cite it. A reference the read answers live, gone with no version, or gone with a version
// the page cannot open earns no chip, and before the read answers none does.
test("a comment citing a node deleted in Graph draws it on a chip as in its words", async () => {
  await warmMarkdownRenderer();
  const statement = STATEMENT();
  const opened = lastHeldPath(statement.last_held ?? "");
  if (opened === undefined) throw new Error("the deleted statement's last_held opens nowhere");
  const live = allTargets().find((target) => !target.gone);
  if (live === undefined) throw new Error("the fixture holds no live target");
  // The fixture's answers for a gone lens, which names no version, and for the deleted
  // statement with a version the page cannot read, each under an address of its own.
  const unpinned = { ...LENS(), ref: `dispatch://${KEY}/spec#b-no-such-block` };
  const unread = {
    ...statement,
    last_held: "not an address",
    ref: `dispatch://${KEY}/spec#b-unread-block`,
  };
  const targets = [...allTargets(), unpinned, unread];
  const words = [
    `The delete in Graph takes ${statement.ref} with it, and ${statement.ref} as well.`,
    `${live.ref} stands, ${unpinned.ref} and ${unread.ref} went,`,
    `and dispatch://${KEY}/spec holds the rest.`,
  ].join(" ");
  // The person's post as the tree read answers one, its words and their reading beside them.
  const post = (held.nodes ?? []).find((node) => node.id === "<message>");
  if (post === undefined) throw new Error("the fixture's tree read holds no post of the person");
  const citing = {
    ...post,
    id: "<citing-comment>",
    kind: "comment",
    supertag: "comment",
    text: words,
    text_tree: {
      content: [{ content: [{ text: words, type: "text" }], type: "paragraph" }],
      type: "doc",
    },
  };
  const tree: TreeAnswer = { ...held, nodes: [...(held.nodes ?? []), citing] };
  const reads = { issue: issueDetails(), targets, tree };
  const rowOf = (root: TreeNode): TreeNode => {
    const row = nodesInOrder(root).find((node) => node.readId === citing.id);
    if (row === undefined) throw new Error("the tree draws no row of the comment");
    return row;
  };

  // Before the read answers, the row draws the chip its words earn for the spec alone.
  const before = rowOf(buildIssueTree({ issue: reads.issue, tree }));
  expect(before.chips.map(({ gone, ref }) => ({ gone, ref }))).toEqual([
    { gone: false, ref: `dispatch://${KEY}/artifact/spec` },
  ]);
  // Once it answers, the row draws the deleted statement's chip after that one, once, with the
  // version, and no chip of any other reference its words cite.
  const root = buildIssueTree(reads);
  const drawn = rowOf(root);
  const chip = treeContext(reads).chipOf(statement.ref);
  expect(chip).toMatchObject({ gone: true, lastHeld: statement.last_held });
  expect(drawn.chips).toEqual([...before.chips, chip]);
  // No other row gains a chip, though the read answers nodes deleted with a version, and each
  // keeps its own, in its order, as it draws them before the read answers.
  const others = nodesInOrder(root).filter((node) => node.readId !== citing.id);
  const unnamed = nodesInOrder(buildIssueTree({ issue: reads.issue, tree: held }));
  expect(others.map(({ chips, id }) => ({ id, refs: chips.map(({ ref }) => ref) }))).toEqual(
    unnamed.map(({ chips, id }) => ({ id, refs: chips.map(({ ref }) => ref) }))
  );

  // The row's words and its chip each link the deleted statement to the one version, as the
  // page draws the row under the names it publishes from the same answer.
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const view = render(
    createElement(
      QueryClientProvider,
      { client },
      createElement(
        MemoryRouter,
        null,
        createElement(
          ReferenceNamesProvider,
          null,
          createElement(
            Publish,
            {
              lastHeld: targetLastHeld(targets),
              // Every reference the tree cites, as the page asks its names read for them.
              published: publishedOf(reads, treeReferences(root)),
            },
            createElement(
              NoTooltips,
              null,
              createElement(NodeRow, {
                author: {
                  kind: authorKind(undefined),
                  name: CONTROL_LABELS.unknown_author,
                  reader: false,
                },
                context: false,
                focused: false,
                isClosed: false,
                level: 1,
                node: drawn,
                onFocus: () => {},
                onToggle: () => {},
                ownAnswer: false,
                rowKey: "row",
                tabStop: false,
              })
            )
          )
        )
      )
    )
  );
  // In the order the row draws them: the words' link at each citation, then the chip's.
  await waitFor(() =>
    expect(
      [...view.container.querySelectorAll<HTMLAnchorElement>("a")]
        .filter((anchor) => anchor.textContent === CONTROL_LABELS.target_gone)
        .map((anchor) => ({
          chip: anchor.closest("[data-node-chip]") !== null,
          href: anchor.getAttribute("href"),
        }))
    ).toEqual([
      { chip: false, href: opened },
      { chip: false, href: opened },
      { chip: true, href: opened },
    ])
  );
  expect(view.container.querySelectorAll("[data-node-chip]")).toHaveLength(2);
  view.unmount();
});
