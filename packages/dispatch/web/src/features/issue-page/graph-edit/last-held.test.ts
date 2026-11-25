import { afterEach, beforeEach, expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, waitFor } from "@testing-library/react";
import { createElement, type ReactNode } from "react";
import { MemoryRouter, useLocation } from "react-router-dom";

import { api } from "../../../api/client";
import type { IssueDetails, ReferenceTarget } from "../../../api/types";
import { parseIssuePath } from "../../refs/routes";
import { issueDetails, KEY } from "../decisions-filter/decisions-fixtures";
import type { TreeNode } from "../issue-tree";
import { NodeRow } from "../NodeRow";
import { NoTooltips } from "../NoTooltips";
import { authorKind, CONTROL_LABELS } from "../page-words";
import { RowText } from "../RowText";
import { ReferenceNamesProvider, usePublishReferenceNames } from "../reference-names";
import { ProvidersSlot } from "../slots/providers";
import { StageSlot } from "../slots/stage";
import { AppProviders, stubReads } from "../slots/stage-fixtures";
import { treeContext } from "../tree-drafts";
import { wholeTreeOnly } from "../tree-read/tree-fixtures";
import type { TreeAnswer } from "../tree-read/tree-read";
import fixture from "./__fixtures__/graph-edit.json";
import { lastHeldPath } from "./last-held";

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
 * - `usePublishReferenceNames(names, ambiguous, edited, deleted, lastHeld)` (`reference-names.tsx`)
 *   takes the map of each gone reference to its `last_held` as its fifth argument, and
 *   `useKnownLastHeldReferences(): ReadonlyMap<string, string>` hands it to the surfaces.
 * - A gone chip with `lastHeld` draws `CONTROL_LABELS.target_gone` as a link to `lastHeldPath` of
 *   it, titled `CONTROL_LABELS.last_held_title` (`NodeRow.tsx`'s `Chip`); a gone reference in a
 *   row's words (`RowText.tsx`, `row-references.ts`) draws the same link in place; with no
 *   `lastHeld` both draw as today.
 * - An address on the issue page whose `#b-<block>` names a block that no read holds (the address
 *   fallback of `slots/body.tsx`, `address-target.ts`) sends one `reference-targets` read for it
 *   and opens `lastHeldPath` of its `last_held`, replacing the address; where the read answers no
 *   `last_held`, the address opens without the block (no `#b-` fragment) and draws no line.
 *
 * The answers are the server's own, from `__fixtures__/graph-edit.json`'s `targets` member: the
 * `GET /api/v1/reference-targets` answer (`{ request, status, body: { targets } }`) for a live
 * node, a gone statement (a block of the spec), a gone check (a block of the Verification
 * document), a gone block of the issue's own fragment and a gone lens. The member is not in the
 * fixture yet (a later task adds it with the server's `last_held`): each case that reads it fails
 * on its absence until then.
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
// check. A node of the Verification document does not open the Spec filter.
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

/** Publishes what the page publishes from its one batched read, for the surfaces under it. */
function Publish({
  lastHeld,
  names,
}: {
  lastHeld: ReadonlyMap<string, string>;
  names: ReadonlyMap<string, string>;
}): ReactNode {
  usePublishReferenceNames(names, new Set(), new Set(), new Map(), lastHeld);
  return null;
}

const NO_NAMES: ReadonlyMap<string, string> = new Map();

// R9: every surface that draws a reference to a gone node reads `last_held`, none by a new import of
// the model: `chipOf` carries it into `NodeChip`, the row's `Chip` draws it, and a row's words and
// the names the page publishes (`reference-names.tsx`, `row-references.ts`, `RowText.tsx`) draw it
// in place. The address fallback of `slots/body.tsx` and `address-target.ts` is the last two cases.
test("every surface that draws a gone reference reads last_held", () => {
  const targets = allTargets();
  const statement = STATEMENT();
  const expected = urlOf(lastHeldPath(statement.last_held ?? ""));
  const reads = { issue: issueDetails(), targets, tree: undefined as TreeAnswer | undefined };

  // `chipOf` and `NodeChip`: the gone chip carries it, a live one and a gone lens carry none.
  const context = treeContext(reads, () => "");
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

  // `reference-names.tsx`, `row-references.ts` and `RowText.tsx`: a row's words that cite the gone
  // node draw the same link, in place.
  const lastHeld = new Map([[statement.ref, statement.last_held ?? ""]]);
  const words = render(
    createElement(
      MemoryRouter,
      null,
      createElement(
        ReferenceNamesProvider,
        null,
        createElement(Publish, { lastHeld, names: NO_NAMES }),
        createElement(RowText, { markdown: `See ${statement.ref}.` })
      )
    )
  );
  const cited = words.container.querySelector<HTMLAnchorElement>("a[data-row-reference]");
  expect(cited?.textContent, "the row's words draw the gone reference as Deleted").toBe(
    CONTROL_LABELS.target_gone
  );
  expect(urlOf(cited?.getAttribute("href") ?? "").pathname).toBe(expected.pathname);
  expect(urlOf(cited?.getAttribute("href") ?? "").searchParams.get("v")).toBe(
    expected.searchParams.get("v")
  );
  words.unmount();
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
  return createElement(ProvidersSlot, { issue, route }, createElement(StageSlot, { issue, route }));
}

/** The page at `path`, its `reference-targets` read answered by `answer`, which is told each read's
 *  references. */
function drawAt(path: string, answer: (refs: string[]) => ReferenceTarget[]) {
  spyOn(api, "getIssueTree").mockImplementation(wholeTreeOnly(held));
  spyOn(api, "getReferenceTargets").mockImplementation(async (refs: string[]) => {
    asked.push(refs);
    return { targets: answer(refs) };
  });
  const seen = { at: "" };
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const view = render(
    createElement(
      MemoryRouter,
      { initialEntries: [path] },
      createElement(Where, { seen }),
      createElement(AppProviders, { client }, createElement(Stage, { issue: issueDetails() }))
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
