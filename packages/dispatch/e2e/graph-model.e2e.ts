import { readFileSync } from "node:fs";

import { HocuspocusProvider, HocuspocusProviderWebsocket } from "@hocuspocus/provider";
import type {
  ArtifactReviewRead,
  BlockSchema,
  IssueAskRow,
  IssueTreeNode,
  IssueTreeRead,
  IssueTreeTuple,
} from "@legion/contracts";
import * as Y from "yjs";

import type { Actor } from "../web/src/api/types";
import { schemaCreatable } from "../web/src/features/doc/schema-creatable";
import { APPROVAL_STATE_WORDS, GLYPH_TOKENS } from "../web/src/features/issue-page/page-words";
import {
  baseUrl,
  createAsk,
  createComment,
  createIssue,
  createIssueArtifact,
  declineAsk,
  documentBase,
  editArtifact,
  getArtifact,
  getArtifactText,
  getArtifactVersion,
  keepDecision,
  listIssueArtifacts,
  listIssueAsks,
  requestApproval,
  reviewArtifact,
  userHeaders,
} from "./api";
import { type Corpus, premortemConvert, seedCorpus } from "./corpus";
import { gotoIssue, issuePage, phase } from "./issue-page";
import {
  blockNode,
  type DrawnRow,
  drawnRows,
  focusRow,
  focusThePage,
  nodeRow,
  nodeRows,
  READER,
  unfoldEveryBranch,
} from "./issue-page-g";
import { withIssuePage } from "./issue-page-option";
import { sql } from "./psql";
import { expect, type Page, test } from "./test";
import { asUser } from "./users";

// The graph model's four browser checks (dispatch://INFRA-1098's Verification document,
// c-decisions-view, c-spec-tie, c-interpretation and c-one-tree). Each row seeds the corpus every
// lane of the model reads (`seedCorpus`, the loader of packages/envoy/cmd/dispatch-corpus) into
// the database of its own stack, writes what its check writes through the API the agent and the
// person use, reads what the server stores through the tree read (`GET
// /api/v1/issues/{key}/tree`) and its other routes, and then reads the new issue page
// (dispatch://INFRA-1196) a person who turned it on reads. Each row turns the page on before it
// opens it (`phase`, or `withIssuePage` itself), fails where the page draws no
// `[data-issue-page]`, unfolds every branch before it reads a row, and reads rows only through the
// page's own readers (e2e/issue-page-g.ts) and the attributes that page holds stable on a row.
//
// What each row reads of the server comes first and what it reads of the page after, so a row
// run on a page that does not draw a part yet still proves every server behaviour it names. An
// assertion of a drawing is kept as its check states it: the rows pass once the page draws the
// parts its checks read.

/** How long a row of this file may take. Each seeds the whole corpus (about 20 s, and once per
 *  worker the build of the loader), writes through the API, waits for each settlement it reads,
 *  and reads several views of up to four issues. Measured on a 2-core pod at INFRA-1253's
 *  2afc1550, where each row stops at the first drawing the page does not hold yet, the rows took
 *  27 to 67 s; the page reads after that point are not measured, so the budget is four times the
 *  slowest measured row. */
const CORPUS_ROW_MS = 270_000;

/** How long a read of the page waits for what arrives after the tree: comments, questions,
 *  decisions and results each come from a read of their own (`useIssueTree.ts`). */
const DRAWN_MS = 30_000;

/** How long a read of the store waits for a settlement or an index pass the server runs behind
 *  the write's answer: the harness settles a document 250 ms after its last change
 *  (e2e/run-server.sh), and the block index runs on a short tick of its own. */
const SETTLED_MS = 30_000;

/** The agent's session every write of this file's own is made as, over the shared bearer. */
const AGENT: Actor = { id: "ses_graph_model_agent", kind: "session" };
const asAgent = { actor: AGENT, as: "agent" as const };

/** The project the corpus writes into (seedcorpus.DefaultProject). */
const PROJECT = "CORE";

// ---------------------------------------------------------------------------------------------
// The corpus and the tree read.
// ---------------------------------------------------------------------------------------------

/** The value the seed printed under `name`, failing where it printed none. */
function named(corpus: Corpus, name: string): string {
  const value = corpus.get(name);
  expect(value, `the seed printed no ${name}`).toBeTruthy();
  return value ?? "";
}

/** The views the tree read answers (`issueTreeViews`). */
type View = "decisions" | "plan" | "spec" | "verification";

/** `GET /api/v1/issues/{key}/tree` as `login` reads it: the whole tree, or one view of it at the
 *  head or at version `v` of the view's query node. */
async function treeRead(
  key: string,
  query: { readonly view?: View; readonly v?: number } = {},
  login: string = READER
): Promise<IssueTreeRead> {
  const url = new URL(`/api/v1/issues/${key}/tree`, baseUrl);
  if (query.view !== undefined) url.searchParams.set("view", query.view);
  if (query.v !== undefined) url.searchParams.set("v", String(query.v));
  const response = await fetch(url, { headers: await userHeaders(login) });
  const body = await response.text();
  expect(response.status, `GET ${url.pathname}${url.search} answered ${body}`).toBe(200);
  const tree = JSON.parse(body) as IssueTreeRead;
  expect(tree.cursor, `GET ${url.pathname}${url.search} answered more than one page`).toBeNull();
  return tree;
}

function nodesOf(tree: IssueTreeRead): readonly IssueTreeNode[] {
  return tree.nodes ?? [];
}

function tuplesOf(tree: IssueTreeRead): readonly IssueTreeTuple[] {
  return tree.tuples ?? [];
}

/** A node as a tuple's end names it. */
function endOf(node: Pick<IssueTreeNode, "id" | "kind">): string {
  return `${node.kind}:${node.id}`;
}

/** The block of the issue read whose block id is `block`, failing where the answer holds none. */
function blockOf(tree: IssueTreeRead, block: string): IssueTreeNode {
  const found = nodesOf(tree).find(
    (node) => node.kind === "block" && node.block_id === block && node.issue === undefined
  );
  expect(found, `the tree read of ${tree.issue} answers no block ${block}`).toBeDefined();
  return found as IssueTreeNode;
}

/** The node the answer holds under `kind` and `id`. */
function nodeAt(tree: IssueTreeRead, kind: string, id: string): IssueTreeNode | undefined {
  return nodesOf(tree).find((node) => node.kind === kind && node.id === id);
}

/** Every tuple of kind `kind` out of `node`. */
function tuplesFrom(tree: IssueTreeRead, node: IssueTreeNode, kind: string): IssueTreeTuple[] {
  return tuplesOf(tree).filter(
    (tuple) => tuple.kind === kind && tuple.from.kind === node.kind && tuple.from.id === node.id
  );
}

/** Every tuple of kind `kind` into `node`. */
function tuplesInto(tree: IssueTreeRead, node: IssueTreeNode, kind: string): IssueTreeTuple[] {
  return tuplesOf(tree).filter(
    (tuple) => tuple.kind === kind && tuple.to.kind === node.kind && tuple.to.id === node.id
  );
}

/** The one parent the `part_of` tuple behind `node` names, failing on none or more than one. */
function parentTuple(tree: IssueTreeRead, node: IssueTreeNode): IssueTreeTuple {
  const parents = tuplesFrom(tree, node, "part_of");
  expect(
    parents.map((tuple) => endOf(tuple.to)),
    `${endOf(node)} holds ${parents.length} part_of tuples in the tree read of ${tree.issue}, want one`
  ).toHaveLength(1);
  return parents[0] as IssueTreeTuple;
}

/** Each ask of `key` by its id, as the asks read answers it. */
async function asksOf(key: string): Promise<Map<string, IssueAskRow>> {
  return new Map((await listIssueAsks(key)).map((ask) => [ask.id, ask]));
}

/**
 * The id the page draws `node` of issue `key` by, the outline's own rule for a node of the tree
 * read: a node of another issue by its block id or its own id; the issue itself by its key; a
 * typed block of one of the issue's documents as `<slug>#b-<block id>` (`blockNode`); an ask by
 * the block it was asked from where the asks read names one, so its question and its block are one
 * row; and anything else by its id.
 */
function rowIdOf(node: IssueTreeNode, key: string, asks: ReadonlyMap<string, IssueAskRow>): string {
  const own = node.block_id ?? node.id;
  if (node.issue !== undefined && node.issue !== key) return own;
  if (node.kind === "issue" && node.id === key) return key;
  if (node.document !== undefined) return blockNode(node.document.slug, own);
  if (node.kind === "ask") {
    const ask = asks.get(node.id);
    const slug = ask?.block_artifact?.slug;
    if (slug !== undefined && ask?.block_id) return blockNode(slug, ask.block_id);
  }
  return own;
}

/** The first word of `drawn` that is not the word of `written` in its place, for a failure to
 *  print, or "" where `drawn` holds `written` whole. */
function firstWordThatDiffers(drawn: string, written: string): string {
  if (drawn.includes(written)) return "";
  const wanted = written.split(/\s+/u).filter(Boolean);
  const found = drawn.split(/\s+/u).filter(Boolean);
  const start = found.indexOf(wanted[0] ?? "");
  if (start === -1) return `the answer's first word ${JSON.stringify(wanted[0])} is not drawn`;
  for (let index = 0; index < wanted.length; index += 1) {
    if (found[start + index] !== wanted[index]) {
      return `word ${index + 1}: drawn ${JSON.stringify(found[start + index])}, written ${JSON.stringify(wanted[index])}`;
    }
  }
  return "";
}

/** Rejects unless `write` is refused, and answers the refusal's words: e2e/api.ts throws
 *  `<method> <path> failed: <status> <body>` for every answer that is not a 2xx. */
async function refusalOf(what: string, write: Promise<unknown>): Promise<string> {
  const outcome = await write.then(
    () => undefined,
    (error: unknown) => (error instanceof Error ? error.message : String(error))
  );
  expect(outcome, `${what} was taken, want a refusal`).toBeDefined();
  expect(outcome ?? "", `${what} answered no refusal status`).toMatch(/failed: 4\d\d /u);
  return outcome ?? "";
}

/** Reads `read` until `accept` holds of it, and answers what it read last, so a write the
 *  server finishes behind its answer - a settlement, an index pass - is read once it lands. */
async function settled<T>(what: string, read: () => Promise<T>, accept: (value: T) => boolean) {
  let last: T | undefined;
  await expect
    .poll(
      async () => {
        last = await read();
        return accept(last);
      },
      { message: `${what}, within ${SETTLED_MS} ms`, timeout: SETTLED_MS }
    )
    .toBe(true);
  return last as T;
}

/** The person or session the decided_by tuple of `node` names in `tree`, as `kind:id`. */
function decidedBy(tree: IssueTreeRead, node: IssueTreeNode): string[] {
  return tuplesFrom(tree, node, "decided_by").map((tuple) => endOf(tuple.to));
}

// ---------------------------------------------------------------------------------------------
// The page.
// ---------------------------------------------------------------------------------------------

/** Every row the page draws where a reader sees it, in document order. */
async function visibleRows(page: Page): Promise<DrawnRow[]> {
  return (await drawnRows(page)).filter((row) => row.visible);
}

/** Waits until the page, with every branch unfolded, draws a row for each of `ids`, and answers
 *  the rows it drew. A node that arrives late may arrive folded, so each read unfolds first. */
async function rowsDrawn(page: Page, ids: readonly string[], where: string): Promise<DrawnRow[]> {
  let rows: DrawnRow[] = [];
  await expect
    .poll(
      async () => {
        await unfoldEveryBranch(page);
        rows = await visibleRows(page);
        const drawn = new Set(rows.map((row) => row.id));
        return ids.filter((id) => !drawn.has(id));
      },
      { message: `${where} draws no row for these nodes`, timeout: DRAWN_MS }
    )
    .toEqual([]);
  return rows;
}

/** The drawn row of `id`, failing where the page draws none. */
function drawnRow(rows: readonly DrawnRow[], id: string, where: string): DrawnRow {
  const row = rows.find((candidate) => candidate.id === id);
  expect(row, `${where} draws no row ${id}`).toBeDefined();
  return row as DrawnRow;
}

/** The row each drawing of `id` stands under, by the tree's own `aria-level`: the page draws a
 *  check and a plan task once under each requirement it serves, so one id may stand in several
 *  places. `null` is the top level. */
function parentsOfRow(rows: readonly DrawnRow[], id: string): (string | null)[] {
  const parents: (string | null)[] = [];
  rows.forEach((row, index) => {
    if (row.id !== id) return;
    const level = Number(row.level ?? "");
    let parent: string | null = null;
    for (let before = index - 1; before >= 0; before -= 1) {
      const candidate = rows[before];
      if (candidate !== undefined && Number(candidate.level ?? "") === level - 1) {
        parent = candidate.id;
        break;
      }
    }
    parents.push(parent);
  });
  return parents;
}

/** The text a focused row draws, its body included: the row whose focus opens its body. */
async function focusedText(page: Page, id: string): Promise<string> {
  const row = await focusRow(page, id);
  return (await row.innerText()).replace(/\s+/gu, " ").trim();
}

/** The `href` of every link the row of `id` draws. */
async function linksOf(page: Page, id: string): Promise<string[]> {
  return nodeRow(page, id)
    .first()
    .getByRole("link")
    .evaluateAll((links) => links.map((link) => link.getAttribute("href") ?? ""));
}

/** Plain words of a stored markdown text, for a comparison with what a row draws. */
function plainWords(markdown: string): string {
  return markdown
    .replace(/^:{3,}[a-z_]*(\{[^\n]*\})?\s*$/gmu, " ")
    .replace(/[*_`#>]/gu, "")
    .replace(/\s+/gu, " ")
    .trim();
}

/** The block schema this build's page is built with, read from the file it embeds: Node, which
 *  loads this file, refuses the plain JSON import web/src/features/doc/schema.ts makes. */
const BUILT_SCHEMA = JSON.parse(
  readFileSync(
    new URL("../../envoy/internal/dispatch/pmdoc/schema/blocks.json", import.meta.url),
    "utf8"
  )
) as BlockSchema;
// ---------------------------------------------------------------------------------------------
// The browser's own settlement.
// ---------------------------------------------------------------------------------------------

/**
 * A browser's editor that puts `claims` on block `block` of document `artifactId` in its live
 * tree, as `login`, and answers the value the tree holds of each claimed attribute once the
 * document has settled. This build's editor never writes a server-owned attribute
 * (`domAttrs` and `writtenDefault` in proof-editor's block-schema.ts), so only a client other
 * than this build's editor can put one there; this plays that client over the browser's own
 * transport, the document socket and the person's own cookie, with the schema version this
 * build's page presents.
 */
async function claimInTheLiveTree(
  artifactId: string,
  block: string,
  claims: Readonly<Record<string, string>>,
  login: string
): Promise<Record<string, unknown>> {
  const headers = await userHeaders(login);
  const artifact = await getArtifact(artifactId);
  const fragmentName = artifact.fragment ?? "prosemirror";
  const document = new Y.Doc();
  const Socket = class extends globalThis.WebSocket {
    constructor(url: string, protocols?: string | string[]) {
      super(url, { headers, protocols } as unknown as string[]);
    }
  };
  const websocket = new HocuspocusProviderWebsocket({
    WebSocketPolyfill: Socket,
    parameters: {
      schema_creatable: schemaCreatable(BUILT_SCHEMA),
      schema_version: BUILT_SCHEMA.version,
    },
    url: `${baseUrl.replace(/^http/u, "ws")}/ws/doc/${encodeURIComponent(artifactId)}`,
  });
  const provider = new HocuspocusProvider({
    document,
    name: artifactId,
    token: String(BUILT_SCHEMA.version),
    websocketProvider: websocket,
  });
  try {
    await expect
      .poll(() => provider.isSynced && provider.isAuthenticated, {
        message: `the live document ${artifactId} admitted no socket for ${login}`,
        timeout: SETTLED_MS,
      })
      .toBe(true);
    const element = await settled(
      `the live tree of ${artifactId} holds block ${block}`,
      async () => blockElement(document.getXmlFragment(fragmentName), block),
      (found) => found !== undefined
    );
    document.transact(() => {
      for (const [name, value] of Object.entries(claims)) element?.setAttribute(name, value);
    });
    await expect
      .poll(() => provider.unsyncedChanges, {
        message: `the live document ${artifactId} took the claims on ${block}`,
        timeout: SETTLED_MS,
      })
      .toBe(0);
    // Settlement repairs a server-owned attribute in the live tree with what the store holds:
    // each claimed value is replaced or taken out once the document has settled.
    await expect
      .poll(
        () =>
          Object.entries(claims).filter(([name, value]) => element?.getAttribute(name) === value),
        {
          message: `settlement left a claimed attribute of ${block} in the live tree`,
          timeout: SETTLED_MS,
        }
      )
      .toEqual([]);
    return Object.fromEntries(
      Object.keys(claims).map((name) => [name, element?.getAttribute(name) ?? null])
    );
  } finally {
    provider.destroy();
    websocket.destroy();
  }
}

/** The element of the live tree whose block id is `block`, at any depth. */
function blockElement(
  parent: Y.XmlElement | Y.XmlFragment,
  block: string
): Y.XmlElement | undefined {
  for (const child of parent.toArray()) {
    if (!(child instanceof Y.XmlElement)) continue;
    if (child.getAttribute("blockId") === block) return child;
    const below = blockElement(child, block);
    if (below !== undefined) return below;
  }
  return undefined;
}

// ---------------------------------------------------------------------------------------------
// c-decisions-view
// ---------------------------------------------------------------------------------------------

/** A decision block an agent writes claiming `person` decided it. */
function claimedDecision(id: string, words: string, person: string): string {
  return `:::decision{#${id} decided_by="${person}"}\n${words}\n:::\n`;
}

// SLOW(>=2s): it seeds the whole corpus, writes a decision through each of the four write paths
// and waits for each settlement, and reads the Decisions view of three issues with every branch
// unfolded and each answered ask and agent decision focused in turn.
test("the decisions view is a query over the issue own nodes", async ({ browser }) => {
  test.setTimeout(CORPUS_ROW_MS);
  const corpus = await seedCorpus();
  const seeded = named(corpus, "model.issue");
  const spec = named(corpus, "model.spec");
  const copied = [named(corpus, "acme-1115.issue"), named(corpus, "acme-1042.issue")];
  const openAsk = (await asksOf(seeded)).get(named(corpus, "model.ask_open"));
  const person = openAsk?.waiting_on_person ?? openAsk?.asked_of ?? "";
  expect(person, "the seeded open ask names the person it is asked of").not.toBe("");

  // A third ask of the seeded issue, which that person declines.
  const third = await createAsk(
    seeded,
    {
      options: [{ label: "Fold it" }, { label: "Keep it open" }],
      question: "Does the folded part stay folded once the person has read it?",
    },
    asAgent
  );
  await declineAsk(third.id, { note: "Not mine to say." }, { login: person });

  // The agent claims the person as the decider of a decision written through each write path.
  // Each write is taken, never refused: decided_by is no value a caller sends, so settlement
  // overwrites it.
  const specMarkdown = (await getArtifactText(spec)).markdown;
  const specName = (await listIssueArtifacts(seeded)).find(
    (artifact) => artifact.id === spec
  )?.name;
  expect(specName, `the seeded issue lists no document ${spec}`).toBeTruthy();
  await createIssueArtifact(
    seeded,
    {
      content: `${specMarkdown.trimEnd()}\n\n${claimedDecision("d-upload", "The upload decides the order.", person)}`,
      name: specName ?? "",
      precondition: await documentBase(spec),
    },
    asAgent
  );
  await editArtifact(
    spec,
    {
      ops: [
        {
          after: "end",
          markdown: claimedDecision("d-edit", "The editor operation decides the fold.", person),
          op: "insert",
        },
        {
          after: "end",
          markdown: ":::decision{#d-browser}\nThe browser's settlement decides the shape.\n:::\n",
          op: "insert",
        },
      ],
    },
    asAgent
  );
  const first = await createIssue(
    {
      force: true,
      project: PROJECT,
      spec: `## Summary\n\nAn issue whose first document claims a person decided.\n\n## Decisions\n\n${claimedDecision("d-first", "The first document decides the title.", person)}`,
      title: "The first document claims a decider",
    },
    asAgent
  );
  const browserClaim = await claimInTheLiveTree(spec, "d-browser", { decided_by: person }, person);
  expect(browserClaim.decided_by, "settlement left the claimed decider in the live tree").not.toBe(
    person
  );

  const agentEnd = endOf(AGENT);
  const personEnd = `person:${person}`;
  const decisionsOf = async (key: string, blocks: readonly string[]) =>
    settled(
      `every decision of ${blocks.join(", ")} on ${key} names who decided it`,
      () => treeRead(key, { view: "decisions" }),
      (tree) =>
        blocks.every((block) => {
          const node = nodesOf(tree).find((candidate) => candidate.block_id === block);
          return node !== undefined && decidedBy(tree, node).length > 0;
        })
    );
  const ownTree = await decisionsOf(seeded, ["d-upload", "d-edit", "d-browser"]);
  const firstTree = await decisionsOf(first.key, ["d-first"]);
  for (const [tree, block] of [
    [ownTree, "d-upload"],
    [ownTree, "d-edit"],
    [ownTree, "d-browser"],
    [firstTree, "d-first"],
  ] as const) {
    const stored = decidedBy(tree, blockOf(tree, block));
    expect(
      stored,
      `the stored decider of ${block}, which the agent wrote claiming ${person}`
    ).toEqual([agentEnd]);
    expect(stored, `the stored decider of ${block} is the person the agent claimed`).not.toContain(
      personEnd
    );
  }

  // An answered ask is the person's decision, and no agent can give it an uncertainty, nor a
  // reason for one alone. The attributes ride on a retype of the ask's own block, which keeps its
  // type and its words: the one operation that writes a typed block's attributes alone.
  const claimOnAnswered = (attributes: Record<string, string>) =>
    editArtifact(
      spec,
      { ops: [{ attributes, block: "q-answered", op: "retype", type: "ask" }] },
      asAgent
    );
  const uncertain = await refusalOf(
    "an uncertainty written on the person's answered decision",
    claimOnAnswered({ uncertainty: "low" })
  );
  expect(uncertain, "the refusal of an uncertainty names the attribute").toContain("uncertainty");
  const reasoned = await refusalOf(
    "an uncertainty reason alone written on the person's answered decision",
    claimOnAnswered({ uncertainty_reason: "An agent doubts the answer." })
  );
  expect(reasoned, "the refusal of an uncertainty reason names the attribute").toContain(
    "uncertainty_reason"
  );

  // Keep acts on a decision block and answers its version; a Keep of a block that is no decision
  // is refused.
  const kept = blockOf(ownTree, "d-agent");
  expect(
    kept.decision,
    "the decisions view answers d-agent with no decision members"
  ).toBeDefined();
  const keep = await keepDecision(spec, "d-agent", kept.decision?.text_sha256 ?? "", {
    login: person,
  });
  expect(keep.changed_version, "the version a Keep of d-agent answers").toBe(
    kept.decision?.changed_version
  );
  const notDecision = await refusalOf(
    "a Keep of requirement r-one, which is no decision",
    keepDecision(spec, "r-one", kept.decision?.text_sha256 ?? "", { login: person })
  );
  expect(notDecision, "the refusal of a Keep of a requirement").toMatch(/failed: 404 /u);

  // The page, as the person the open ask waits on reads it.
  const context = await asUser(browser, person);
  try {
    await withIssuePage(context, person);
    const page = await context.newPage();
    await page.setViewportSize({ height: 900, width: 1440 });
    for (const key of [...copied, seeded, first.key]) {
      const where = `the Decisions view of ${key}`;
      const tree = await treeRead(key, { view: "decisions" });
      const asks = await asksOf(key);
      const counted = nodesOf(tree).filter(
        (node) => node.supertag === "ask" || node.supertag === "decision"
      );
      console.log(`${where}: the tree read answers ${counted.length} asks and decisions`);
      expect(
        counted.length,
        `${where}: the tree read answers no ask and no decision`
      ).toBeGreaterThan(0);
      const expected = new Map(counted.map((node) => [rowIdOf(node, key, asks), node]));
      const readings = new Map(
        nodesOf(tree)
          .filter((node) => node.supertag === "reading")
          .map((node) => [rowIdOf(node, key, asks), node])
      );
      await gotoIssue(page, `/issues/${key}/decisions`);
      await expect(issuePage(page), `${where} draws no [data-issue-page]`).toHaveCount(1);
      const rows = await rowsDrawn(page, [...expected.keys()], where);

      // No other row, but an agent's reading under its decision.
      const others = rows.filter((row) => {
        if (row.id !== null && expected.has(row.id)) return false;
        const reading = row.id === null ? undefined : readings.get(row.id);
        if (reading === undefined) return true;
        const parent = parentTuple(tree, reading).to;
        return nodeAt(tree, parent.kind, parent.id)?.supertag !== "decision";
      });
      expect(
        others.map((row) => `${row.type} ${row.id}`),
        `${where} draws rows that are no ask, decision or reading of a decision of the tree read`
      ).toEqual([]);

      for (const [id, node] of expected) {
        const row = drawnRow(rows, id, where);
        // Who made it, in words and by a shape: the person or the agent who decided it, else
        // who asked it.
        const maker = (
          tuplesFrom(tree, node, "decided_by")[0] ?? tuplesFrom(tree, node, "authored_by")[0]
        )?.to;
        expect(maker, `${where}: the tree read names nobody who made ${id}`).toBeDefined();
        expect(row.author, `${where}: ${id} names who made it`).toBeTruthy();
        expect(row.authorShape, `${where}: the shape of who made ${id} (${maker?.kind})`).toBe(
          maker?.kind === "person" ? GLYPH_TOKENS.authors.person : GLYPH_TOKENS.authors.agent
        );
        // Each row links to its own node.
        const own = node.kind === "ask" ? node.id : (node.block_id ?? node.id);
        expect(
          (await linksOf(page, id)).some((href) => href.includes(own)),
          `${where}: ${id} draws no link to its own node ${own}`
        ).toBe(true);

        if (node.supertag === "ask") {
          const ask = asks.get(node.id);
          for (const option of node.ask?.options ?? []) {
            expect(row.text, `${where}: the ask ${id} draws its option ${option.label}`).toContain(
              option.label
            );
          }
          expect(row.text, `${where}: the ask ${id} draws its question`).toContain(
            (ask?.question ?? node.text).split("\n")[0]
          );
          if (ask?.state === "answered" && ask.answer !== null) {
            // The answer, character for character as the ask row holds it, and never the old
            // decisions.md wording of the fixture.
            const written = ask.answer.text ?? (ask.answer.selected ?? []).join(", ");
            const drawn = await focusedText(page, id);
            expect(
              firstWordThatDiffers(drawn, written),
              `${where}: the answer the page draws for ${id} against the ask row it came from`
            ).toBe("");
            await focusThePage(page);
          }
        } else {
          const decision = node.decision;
          expect(row.text, `${where}: the decision ${id} draws its text`).toContain(
            decision?.heading ?? plainWords(node.text)
          );
          const level = node.attributes?.uncertainty;
          if (level) {
            expect(row.text.toLowerCase(), `${where}: ${id} draws its uncertainty`).toContain(
              level.toLowerCase()
            );
          }
          const reason = node.attributes?.uncertainty_reason;
          if (reason) {
            expect(await focusedText(page, id), `${where}: ${id} draws its reason`).toContain(
              reason
            );
            await focusThePage(page);
          }
        }
      }

      // The open ask before the answered one.
      const order = rows.map((row) => row.id);
      const open = [...expected].filter(([, node]) => asks.get(node.id)?.state === "open");
      const answered = [...expected].filter(([, node]) => asks.get(node.id)?.state === "answered");
      for (const [openId] of open) {
        for (const [answeredId] of answered) {
          expect(
            order.indexOf(openId),
            `${where}: the open ask ${openId} is drawn after the answered ${answeredId}`
          ).toBeLessThan(order.indexOf(answeredId));
        }
      }

      if (key === seeded) {
        const declined = drawnRow(
          rows,
          rowIdOf(nodeAt(tree, "ask", third.id) as IssueTreeNode, key, asks),
          where
        );
        expect(declined.state, `${where}: the declined ask is marked declined`).toBe("declined");
        expect(declined.answerText, `${where}: the declined ask is drawn as an answer`).toBeNull();
      }
      // Each decision the agent wrote claiming the person is drawn as the agent's.
      for (const block of key === seeded
        ? ["d-upload", "d-edit", "d-browser"]
        : key === first.key
          ? ["d-first"]
          : []) {
        const row = drawnRow(rows, blockNode("spec", block), where);
        expect(row.authorShape, `${where}: ${block} is drawn as a person's`).toBe(
          GLYPH_TOKENS.authors.agent
        );
        expect(row.author, `${where}: ${block} names ${person} as who made it`).not.toBe(person);
      }
    }
  } finally {
    await context.close();
  }
});

// ---------------------------------------------------------------------------------------------
// c-spec-tie
// ---------------------------------------------------------------------------------------------

/** The words the seeded answered ask asks, and the person's written answer (seedcorpus model.go). */
const ANSWERED_QUESTION = "Which page lists the answered asks?";
const ANSWERED_TEXT = "One page, so nobody hunts for an answer.";

/** The words of requirement r-one before and after the version that rewrites them. */
const R_ONE = "The page lists every answered ask.";
const R_ONE_REWRITTEN = "The page lists every answered ask, the newest first.";

/** The words a decision line says of a requirement rewritten since the decision shaped it. */
const CHANGED_SINCE = "Changed since";

/** The text of the row of `id` with the focus on no row and the pointer off the page. */
async function restingText(page: Page, rows: readonly DrawnRow[], id: string, where: string) {
  await focusThePage(page);
  await page.mouse.move(0, 0);
  return drawnRow(rows, id, where).text;
}

// SLOW(>=2s): it seeds the whole corpus, writes four versions of the seeded spec and an approval
// between them, waits for each settlement, and reads the outline and the Spec view at each.
test("a spec block says which decision shaped it, at every version", async ({ browser }) => {
  test.setTimeout(CORPUS_ROW_MS);
  await phase(browser, seedCorpus, async (page, corpus) => {
    const key = named(corpus, "model.issue");
    const spec = named(corpus, "model.spec");
    const answeredAsk = named(corpus, "model.ask_answered");
    const rOne = blockNode("spec", "r-one");

    // The tie is one shaped tuple from the answered ask's node to r-one, which neither r-one nor
    // its criterion hangs under; the ask's markdown renders the same pointer.
    const whole = await treeRead(key);
    const ask = nodeAt(whole, "ask", answeredAsk);
    expect(ask, `the tree read of ${key} answers no node of the answered ask`).toBeDefined();
    const requirement = blockOf(whole, "r-one");
    const shapers = tuplesInto(whole, requirement, "shaped").map((tuple) => endOf(tuple.from));
    expect(shapers, "the shaped tuples into r-one").toEqual([endOf(ask as IssueTreeNode)]);
    const criterion = parentTuple(whole, requirement).to;
    expect(criterion, "r-one hangs under the answered ask").not.toEqual({
      id: answeredAsk,
      kind: "ask",
    });
    expect(endOf(criterion), "the parent of r-one").toBe(endOf(blockOf(whole, "c-want")));
    expect(
      endOf(parentTuple(whole, blockOf(whole, "c-want")).to),
      "the criterion of r-one hangs under something other than the issue"
    ).toBe(`issue:${key}`);
    expect(
      (await getArtifactText(spec)).markdown,
      "the markdown of q-answered renders no shaped pointer at r-one"
    ).toMatch(
      new RegExp(`:::ask\\{#q-answered[^\\n]*shaped="dispatch://${key}/spec#b-r-one"`, "u")
    );
    const specView = await treeRead(key, { view: "spec" });
    expect(
      tuplesInto(specView, blockOf(specView, "r-one"), "shaped").map((tuple) => endOf(tuple.from)),
      "the shaped tuples into r-one the Spec view answers"
    ).toEqual([endOf(ask as IssueTreeNode)]);

    // The line at the block, at rest: no pointer, the focus on another node, no view.
    const readLine = async (path: string, where: string) => {
      await gotoIssue(page, path);
      const rows = await rowsDrawn(page, [rOne, blockNode("spec", "r-two")], where);
      return { rows, text: await restingText(page, rows, rOne, where) };
    };
    const outline = await readLine(`/issues/${key}`, "the outline of the seeded issue");
    for (const words of [ANSWERED_QUESTION, ANSWERED_TEXT]) {
      expect(outline.text, `the decision line at r-one draws ${words}`).toContain(words);
    }
    expect(
      (await linksOf(page, rOne)).some((href) => href.includes(answeredAsk)),
      "the decision line at r-one draws no link to the answered ask"
    ).toBe(true);
    await focusRow(page, blockNode("spec", "r-two"));
    expect(
      drawnRow(await visibleRows(page), rOne, "the outline").text,
      "the decision line at r-one with the focus on r-two"
    ).toContain(ANSWERED_TEXT);
    expect(
      await focusedText(page, rOne),
      "the whole answer at r-one with the focus on it"
    ).toContain(ANSWERED_TEXT);
    expect(
      outline.text,
      "the line of the person's answered ask carries an uncertainty"
    ).not.toMatch(/\b(low|medium|high)\b/u);
    const viewed = await readLine(`/issues/${key}/spec`, "the Spec view of the seeded issue");
    expect(viewed.text, "the Spec view draws the decision line at r-one").toContain(ANSWERED_TEXT);

    // A version that leaves r-one's words alone keeps the line; one that rewrites them says so.
    await editArtifact(
      spec,
      {
        ops: [
          {
            after: "end",
            markdown: "A version that leaves every requirement alone.\n",
            op: "insert",
          },
        ],
      },
      asAgent
    );
    await settled(
      "the seeded spec settles the version that leaves r-one alone",
      () => getArtifactText(spec),
      (text) => text.markdown.includes("A version that leaves every requirement alone.")
    );
    const untouched = await readLine(
      `/issues/${key}`,
      "the outline after a version that leaves r-one alone"
    );
    expect(untouched.text, "the line after a version that leaves r-one alone").toContain(
      ANSWERED_TEXT
    );
    expect(untouched.text, "a version that left r-one alone marks its line changed").not.toContain(
      CHANGED_SINCE
    );
    await editArtifact(
      spec,
      { ops: [{ find: R_ONE, op: "replace", with: R_ONE_REWRITTEN }] },
      asAgent
    );
    await settled(
      "the seeded spec settles the version that rewrites r-one",
      () => getArtifactText(spec),
      (text) => text.markdown.includes(R_ONE_REWRITTEN)
    );
    const rewritten = await readLine(`/issues/${key}`, "the outline after r-one is rewritten");
    expect(rewritten.text, "the line after r-one is rewritten").toContain(CHANGED_SINCE);

    // The person approves this version. Then, with every word left alone, d-agent comes to shape
    // r-one and the answered ask stops shaping it.
    const approved = (await reviewArtifact(spec, { state: "approved" }, { login: READER })).version;
    // A retype of each block writes its attributes and keeps its type and its words: d-agent,
    // with its uncertainty and its reason as the node holds them, now also shapes r-one, and the
    // answered ask, with the urgency and the choice it was asked with, shapes nothing.
    const decisionValues = blockOf(whole, "d-agent").attributes ?? {};
    await editArtifact(
      spec,
      {
        ops: [
          {
            attributes: {
              shaped: `dispatch://${key}/spec#b-r-one`,
              uncertainty: decisionValues.uncertainty,
              uncertainty_reason: decisionValues.uncertainty_reason,
            },
            block: "d-agent",
            op: "retype",
            type: "decision",
          },
          {
            attributes: { multiple: false, urgency: "high" },
            block: "q-answered",
            op: "retype",
            type: "ask",
          },
        ],
      },
      asAgent
    );
    const moved = await settled(
      "the tree read answers d-agent alone shaping r-one",
      () => treeRead(key, { view: "spec" }),
      (tree) =>
        tuplesInto(tree, blockOf(tree, "r-one"), "shaped")
          .map((tuple) => endOf(tuple.from))
          .join() === endOf(blockOf(tree, "d-agent"))
    );
    const agentDecision = blockOf(moved, "d-agent");
    const atApproval = await treeRead(key, { v: approved, view: "spec" });
    expect(
      tuplesInto(atApproval, blockOf(atApproval, "r-one"), "shaped").map((tuple) =>
        endOf(tuple.from)
      ),
      `the shaped tuples into r-one at the approved version ${approved}`
    ).toEqual([endOf(ask as IssueTreeNode)]);

    const current = await readLine(`/issues/${key}/spec`, "the current Spec view");
    expect(current.text, "the current Spec view draws d-agent's line at r-one").toContain(
      agentDecision.decision?.heading ?? ""
    );
    expect(current.text, "the current line carries d-agent's uncertainty").toContain(
      agentDecision.attributes?.uncertainty ?? "an uncertainty"
    );
    expect(current.text, "the current line carries d-agent's reason").toContain(
      agentDecision.attributes?.uncertainty_reason ?? "a reason"
    );
    const then = await readLine(`/issues/${key}/spec?v=${approved}`, "the approved Spec view");
    expect(then.text, "the approved Spec view draws the answered ask's line at r-one").toContain(
      ANSWERED_TEXT
    );
    expect(then.text, "the approved Spec view draws today's tie at r-one").not.toContain(
      agentDecision.decision?.heading ?? "d-agent"
    );
  });
});

// ---------------------------------------------------------------------------------------------
// c-interpretation
// ---------------------------------------------------------------------------------------------

// SLOW(>=2s): it seeds the whole corpus, writes two readings, an approval, a Keep and a Change
// through the API and waits for each settlement, and reads the outline and the Decisions view.
test("a reading is a child node of its decision and leaves the approval alone", async ({
  browser,
}) => {
  test.setTimeout(CORPUS_ROW_MS);
  await phase(browser, seedCorpus, async (page, corpus) => {
    const key = named(corpus, "model.issue");
    const spec = named(corpus, "model.spec");
    const answeredAsk = named(corpus, "model.ask_answered");
    const documentsBefore = (await listIssueArtifacts(key)).map((artifact) => artifact.slug).sort();

    // A second reading, written by an agent inside the person's answered ask.
    await editArtifact(
      spec,
      {
        ops: [
          {
            into: "block:q-answered",
            markdown:
              ":::reading{#g-ask}\nThe agent reads the answer as one page for every answer.\n:::\n",
            op: "insert",
          },
        ],
      },
      asAgent
    );
    const tree = await settled(
      "the tree read answers the reading written in the answered ask",
      () => treeRead(key),
      (read) => nodesOf(read).some((node) => node.block_id === "g-ask")
    );
    const decision = blockOf(tree, "d-agent");
    const firstReading = blockOf(tree, "g-agent");
    const secondReading = blockOf(tree, "g-ask");
    expect(endOf(parentTuple(tree, firstReading).to), "the parent of the decision's reading").toBe(
      endOf(decision)
    );
    expect(
      endOf(parentTuple(tree, secondReading).to),
      "the parent of the answered ask's reading"
    ).toBe(`ask:${answeredAsk}`);
    for (const [reading, holder] of [
      [firstReading, "d-agent"],
      [secondReading, answeredAsk],
    ] as const) {
      expect(
        Object.entries(reading.attributes ?? {}).filter(([, value]) => value.includes(holder)),
        `an attribute of ${reading.block_id} holds the address of what it reads`
      ).toEqual([]);
    }
    expect(
      (await listIssueArtifacts(key)).map((artifact) => artifact.slug).sort(),
      "the issue's documents after the reading"
    ).toEqual(documentsBefore);

    // The person approves the spec; a later reading leaves it approved, and the gate an approval
    // opens answers rather than asking the person again.
    const approved = (await reviewArtifact(spec, { state: "approved" }, { login: READER })).version;
    await editArtifact(
      spec,
      {
        ops: [
          {
            into: "block:d-agent",
            markdown:
              ":::reading{#g-later}\nThe agent reads its decision again after the approval.\n:::\n",
            op: "insert",
          },
        ],
      },
      asAgent
    );
    await settled(
      "the tree read answers the reading written after the approval",
      () => treeRead(key),
      (read) => nodesOf(read).some((node) => node.block_id === "g-later")
    );
    const specRow = (await listIssueArtifacts(key)).find((artifact) => artifact.id === spec);
    expect(specRow?.approval?.state, "the current spec after a reading").toBe("approved");
    // The approval the person gave stands apart, at the version it covered, in the document's
    // reviews, as their own act, with no reason. The version the reading wrote is approved by a
    // carry from that approval (R4), never by an act of the person's: the review the server
    // writes for it names the version it carries in its reason, the one field of a review that
    // tells a carry from the person's own approval, which shares its kind and its actor.
    const reviewsUrl = new URL(`/api/v1/artifacts/${spec}/reviews`, baseUrl);
    const reviewsRead = await fetch(reviewsUrl, { headers: await userHeaders(READER) });
    expect(reviewsRead.status, `GET ${reviewsUrl.pathname} answered ${reviewsRead.status}`).toBe(
      200
    );
    const reviews = (await reviewsRead.json()) as ArtifactReviewRead[];
    expect(
      reviews
        .filter((review) => review.version === approved)
        .map((review) => `${review.state} by ${review.actor.id}, reason ${review.reason}`),
      `the person's own approval of version ${approved}`
    ).toEqual([`approved by ${READER}, reason null`]);
    const carry = `Carried forward from the approval of version ${approved}:`;
    expect(
      reviews
        .filter((review) => review.version > approved)
        .map((review) =>
          review.state === "approved" && review.reason?.startsWith(carry) === true
            ? "a carry"
            : `v${review.version} ${review.state}, reason ${review.reason}`
        ),
      `the reviews after the reading: one carry from version ${approved}, and none the person's own`
    ).toEqual(["a carry"]);
    expect(
      (await requestApproval(spec, {}, asAgent)).ask,
      "the approval gate after a reading asks the person again"
    ).toBeNull();

    // Keep and Change act on the decision's node.
    const before = blockOf(await treeRead(key, { view: "decisions" }), "d-agent");
    const keep = await keepDecision(spec, "d-agent", before.decision?.text_sha256 ?? "", {
      login: READER,
    });
    expect(keep.changed_version, "the version a Keep of d-agent answers").toBe(
      before.decision?.changed_version
    );
    await editArtifact(
      spec,
      {
        ops: [
          {
            find: "The rows are drawn from the tree read, newest first.",
            op: "replace",
            with: "The rows are drawn from the tree read, oldest first.",
          },
        ],
      },
      asAgent
    );
    const changed = await settled(
      "the decisions view answers d-agent's new words",
      () => treeRead(key, { view: "decisions" }),
      (read) => blockOf(read, "d-agent").decision?.text_sha256 !== before.decision?.text_sha256
    );
    const stale = await refusalOf(
      "a Keep of d-agent at the words it held before they changed",
      keepDecision(spec, "d-agent", before.decision?.text_sha256 ?? "", { login: READER })
    );
    expect(stale, "the refusal of a Keep of a changed decision").toContain("DECISION_CHANGED");
    const words = blockOf(changed, "d-agent").decision?.text ?? "";
    await createComment(
      key,
      {
        anchor: { artifact: spec, quote: "The rows are drawn from the tree read, oldest first." },
        body: "Draw them newest first, as the person asked.",
      },
      { login: READER }
    );
    const afterChange = await treeRead(key);
    expect(
      blockOf(afterChange, "d-agent").decision?.text,
      "d-agent's own text after a Change"
    ).toBe(words);
    expect(
      endOf(parentTuple(afterChange, blockOf(afterChange, "g-agent")).to),
      "the parent of the decision's reading after a Change"
    ).toBe(endOf(blockOf(afterChange, "d-agent")));

    // The page: each reading under what it reads, the agent's and apart from the answer.
    const answeredRow = rowIdOf(
      nodeAt(afterChange, "ask", answeredAsk) as IssueTreeNode,
      key,
      await asksOf(key)
    );
    const ids = {
      decision: blockNode("spec", "d-agent"),
      first: blockNode("spec", "g-agent"),
      second: blockNode("spec", "g-ask"),
    };
    await gotoIssue(page, `/issues/${key}`);
    const rows = await rowsDrawn(page, Object.values(ids), "the outline of the seeded issue");
    expect(parentsOfRow(rows, ids.first), "the row the decision's reading is drawn under").toEqual([
      ids.decision,
    ]);
    expect(
      parentsOfRow(rows, ids.second),
      "the row the answered ask's reading is drawn under"
    ).toEqual([answeredRow]);
    const secondRow = drawnRow(rows, ids.second, "the outline");
    expect(secondRow.authorShape, "the answered ask's reading is drawn as a person's").toBe(
      GLYPH_TOKENS.authors.agent
    );
    expect(secondRow.answerText, "the answered ask's reading is drawn as the answer").toBeNull();
    expect(
      issuePage(page).getByText(APPROVAL_STATE_WORDS.approved, { exact: true }),
      "the approval control draws the current spec as approved"
    ).not.toHaveCount(0);
    await expect(
      issuePage(page).getByText(APPROVAL_STATE_WORDS.stale, { exact: true }),
      "the approval control draws the current spec as changed since its approval"
    ).toHaveCount(0);

    await gotoIssue(page, `/issues/${key}/decisions`);
    const viewRows = await rowsDrawn(page, [ids.decision, ids.first], "the Decisions view");
    expect(
      parentsOfRow(viewRows, ids.first),
      "the Decisions view's row of the decision's reading"
    ).toEqual([ids.decision]);
    expect(
      viewRows
        .filter(
          (row, index) =>
            row.type === "reading" &&
            parentsOfRow(viewRows.slice(0, index + 1), row.id ?? "").includes(answeredRow)
        )
        .map((row) => row.id),
      "the Decisions view draws a reading under the answered ask"
    ).toEqual([]);
  });
});

// ---------------------------------------------------------------------------------------------
// c-one-tree
// ---------------------------------------------------------------------------------------------

/** The fourth fixture: this issue's own pre-mortem, as captured. */
const PREMORTEM = JSON.parse(
  readFileSync(new URL("./fixtures/acme-1098-premortem.json", import.meta.url), "utf8")
) as { readonly document: { readonly markdown: string }; readonly risks: readonly unknown[] };

/** The slug of `graph-premortem.md`, as the upload names a document. */
const PREMORTEM_SLUG = "graph-premortem-md";

// SLOW(>=2s): it seeds the whole corpus, files a child issue, moves a kept decision, converts the
// captured pre-mortem with envoy-dispatch twice, and reads the outline and three views.
test("the issue draws whole from one tree of nodes", async ({ browser }, testInfo) => {
  test.setTimeout(CORPUS_ROW_MS);
  // The loader writes the map of the pre-mortem's second conversion beside the corpus, since the
  // map names a check of the issue it seeds.
  const preventedBy = testInfo.outputPath("prevented-by.tsv");
  await phase(
    browser,
    () => seedCorpus(preventedBy),
    async (page, corpus) => {
      const key = named(corpus, "model.issue");
      const spec = named(corpus, "model.spec");
      const approved = (await reviewArtifact(spec, { state: "approved" }, { login: READER }))
        .version;

      // A child issue whose one check serves r-two of the seeded issue.
      const child = await createIssue(
        {
          force: true,
          parent: key,
          project: PROJECT,
          spec: "## Summary\n\nA child that checks r-two.\n",
          title: "A child issue with a check",
        },
        asAgent
      );
      await createIssueArtifact(
        child.key,
        {
          content: `:::check{#k-child serves="dispatch://${key}/spec#b-r-two" surface="cli" run="go test ./..."}\nThe child's check of every decision an agent made.\n:::\n`,
          name: "verification.md",
        },
        asAgent
      );
      const whole = await settled(
        "the tree read of the seeded issue answers the child's check",
        () => treeRead(key),
        (tree) =>
          nodesOf(tree).some((node) => node.block_id === "k-child" && node.issue === child.key)
      );
      const asks = await asksOf(key);
      const childCheck = nodesOf(whole).find(
        (node) => node.block_id === "k-child"
      ) as IssueTreeNode;
      expect(
        childCheck.parent,
        "the parent the tree read answers for the child's check"
      ).toMatchObject({
        id: child.key,
        kind: "issue",
      });

      // Each parent is the part_of tuple behind it, and the criteria hold the spec blocks that
      // answer them; each check and plan task holds one parent, and each check its results.
      expect(endOf(parentTuple(whole, blockOf(whole, "r-one")).to)).toBe(
        endOf(blockOf(whole, "c-want"))
      );
      expect(endOf(parentTuple(whole, blockOf(whole, "r-two")).to)).toBe(
        endOf(blockOf(whole, "c-want"))
      );
      expect(endOf(parentTuple(whole, blockOf(whole, "r-three")).to)).toBe(
        endOf(blockOf(whole, "c-other"))
      );
      // The issue's own nodes of its tree. A block of an ask, a comment or a message is that item's
      // own words, which the item's row draws, so it is no row of its own.
      const ownNodes = nodesOf(whole).filter(
        (node) => node.issue === undefined && node.role === "tree" && !node.kind.endsWith("_block")
      );
      const servers = ownNodes.filter(
        (node) => node.supertag === "check" || node.supertag === "plan_task"
      );
      console.log(`the seeded issue holds ${servers.length} checks and plan tasks`);
      expect(servers.length, "the seeded issue answers no check or plan task").toBeGreaterThan(0);
      for (const node of servers) parentTuple(whole, node);
      for (const check of ownNodes.filter((node) => node.supertag === "check")) {
        for (const result of tuplesInto(whole, check, "result_of")) {
          const node = nodeAt(whole, result.from.kind, result.from.id) as IssueTreeNode;
          expect(
            endOf(parentTuple(whole, node).to),
            `the parent of a result of ${check.block_id}`
          ).toBe(endOf(check));
        }
      }
      for (const [criterion, holder] of [
        ["c-said", "comment"],
        ["c-asked", "ask"],
        ["c-messaged", "message"],
      ] as const) {
        const parent = parentTuple(whole, blockOf(whole, criterion)).to;
        expect(parent.kind, `the kind of what ${criterion} sits under`).toBe(holder);
        expect(
          nodeAt(whole, parent.kind, parent.id),
          `the ${holder} ${criterion} sits under resolves`
        ).toBeDefined();
      }

      // document_blocks.parent_block_id is the projection of each block's part_of.
      const blocks = ownNodes.filter(
        (node) => node.kind === "block" && node.document !== undefined
      );
      const projected = new Map<string, string>(
        (
          await sql(
            `select d.block_id || '|' || coalesce(d.parent_block_id, '') from document_blocks d join artifacts a on a.id = d.artifact_id where a.issue_key = '${key}'`
          )
        )
          .split("\n")
          .filter(Boolean)
          .map((line) => line.split("|") as [string, string])
      );
      console.log(`document_blocks holds ${projected.size} rows of the seeded issue's blocks`);
      expect(projected.size, "document_blocks holds no row of the seeded issue").toBeGreaterThan(0);
      for (const node of blocks) {
        const parent = parentTuple(whole, node).to;
        const parentNode = nodeAt(whole, parent.kind, parent.id);
        const want = parent.kind === "block" ? (parentNode?.block_id ?? "") : "";
        expect(
          projected.get(node.block_id ?? ""),
          `document_blocks.parent_block_id of ${node.block_id}, whose part_of is ${endOf(parent)}`
        ).toBe(want);
      }

      // A kept decision moves without one character changed: a new parent, the same id, the same
      // changed_version, its Keep standing, and the old placement read back as a retracted tuple.
      // It starts inside a note, a block that holds blocks, and moves out to the note's side, where
      // it hangs from the issue: a move names places beside blocks (`block:<id>`), never inside one.
      await editArtifact(
        spec,
        {
          ops: [
            {
              after: "end",
              markdown:
                '::::callout{#n-box kind="note"}\nThe note the moved decision starts in.\n\n:::decision{#d-moved}\nThe decision the test moves.\n:::\n::::\n',
              op: "insert",
            },
          ],
        },
        asAgent
      );
      const box = await settled(
        "the tree read answers d-moved inside the note n-box",
        () => treeRead(key),
        (tree) => {
          const moved = nodesOf(tree).find((node) => node.block_id === "d-moved");
          const note = nodesOf(tree).find((node) => node.block_id === "n-box");
          return (
            moved !== undefined &&
            note !== undefined &&
            tuplesFrom(tree, moved, "part_of").some((tuple) => endOf(tuple.to) === endOf(note))
          );
        }
      );
      const noteEnd = endOf(blockOf(box, "n-box"));
      const placed = blockOf(await treeRead(key, { view: "decisions" }), "d-moved");
      await keepDecision(spec, "d-moved", placed.decision?.text_sha256 ?? "", { login: READER });
      await editArtifact(
        spec,
        { ops: [{ after: "block:n-box", block: "d-moved", op: "move" }] },
        asAgent
      );
      // The parent the tree read answers for d-moved, read until it is the issue, so a read that
      // never moves it prints the parent it kept.
      let afterMove = await treeRead(key);
      await expect
        .poll(
          async () => {
            afterMove = await treeRead(key);
            const moved = nodesOf(afterMove).find((node) => node.block_id === "d-moved");
            return moved === undefined
              ? "no node d-moved"
              : tuplesFrom(afterMove, moved, "part_of")
                  .map((tuple) => endOf(tuple.to))
                  .join(", ");
          },
          {
            message: `the parent the tree read answers for d-moved, within ${SETTLED_MS} ms of the move`,
            timeout: SETTLED_MS,
          }
        )
        .toBe(`issue:${key}`);
      const movedNode = blockOf(afterMove, "d-moved");
      expect(movedNode.id, "the id of the moved decision").toBe(placed.id);
      const movedDecision = blockOf(await treeRead(key, { view: "decisions" }), "d-moved").decision;
      expect(movedDecision?.changed_version, "the changed_version of the moved decision").toBe(
        placed.decision?.changed_version
      );
      expect(movedDecision?.kept?.kept_by, "the Keep on the moved decision").toBe(READER);
      const placements = await sql(
        `select to_kind || ':' || to_id || ':' || asserted::text from graph_tuples where from_kind = 'block' and from_id = '${placed.id}' and kind = 'part_of' order by seq`
      );
      console.log(`the part_of history of d-moved: ${placements.replace(/\n/gu, ", ")}`);
      expect(
        placements.split("\n"),
        "the old placement of d-moved, in the note, read back as a retraction"
      ).toContain(`${noteEnd}:false`);

      // The three role documents are queries of the tree, and no row of the seeded issue holds a
      // role. The artifacts read keeps main's contract, which deployed clients parse: the spec is
      // the primary document and names no role, since a client reads a role as verification, plan
      // or decisions; the Verification document and the plan name their query as their role. The
      // tree read answers every query by its name, the spec's included.
      const roleColumn = (
        await sql(`select count(*) || '|' || count(role) from artifacts where issue_key = '${key}'`)
      ).split("|");
      console.log(
        `artifacts holds ${roleColumn[0]} rows of ${key}, ${roleColumn[1]} of them with a role`
      );
      expect(Number(roleColumn[0]), `artifacts holds no row of ${key}`).toBeGreaterThan(0);
      expect(Number(roleColumn[1]), `rows of ${key} holding a role in artifacts.role`).toBe(0);
      const listed = await listIssueArtifacts(key);
      const answered = listed
        .map(
          (artifact) =>
            `${artifact.slug} primary=${artifact.primary} role=${artifact.role ?? "none"}`
        )
        .join("; ");
      const specRow = listed.find((artifact) => artifact.id === spec);
      expect(specRow?.primary, `the spec the artifacts read answers is primary: ${answered}`).toBe(
        true
      );
      expect(specRow?.role, `the spec the artifacts read answers names a role: ${answered}`).toBe(
        undefined
      );
      for (const [slug, query] of [
        ["verification-md", "verification"],
        ["plan-md", "plan"],
      ] as const) {
        expect(
          listed.find((artifact) => artifact.slug === slug)?.role,
          `the role the artifacts read answers for ${slug}: ${answered}`
        ).toBe(query);
      }
      for (const query of ["spec", "verification", "plan"] as const) {
        expect(
          (await treeRead(key, { view: query })).view,
          `the tree read of the ${query} query`
        ).toBe(query);
      }
      const verification = await treeRead(key, { view: "verification" });
      const checks = nodesOf(verification).filter(
        (node) => node.supertag === "check" && node.issue === undefined
      );
      console.log(`the Verification view answers ${checks.length} checks of ${key}`);
      expect(
        checks.length,
        "the Verification view answers no check of the seeded issue"
      ).toBeGreaterThan(0);

      // The seeded pre-mortem and the one risk it holds that a check prevents.
      const lens = nodeAt(
        whole,
        "lens",
        named(corpus, "lens.premortem").replace("dispatch://lens/", "")
      ) as IssueTreeNode;
      expect(lens, "the tree read answers no seeded pre-mortem lens").toBeDefined();
      const risks = tuplesInto(whole, lens, "part_of").map(
        (tuple) => nodeAt(whole, tuple.from.kind, tuple.from.id) as IssueTreeNode
      );
      expect(
        risks.map((risk) => risk.kind),
        "the nodes under the seeded pre-mortem"
      ).toEqual(["risk", "risk"]);
      const prevented = risks.flatMap((risk) =>
        tuplesFrom(whole, risk, "prevented_by").map((tuple) => ({ check: tuple.to, risk }))
      );
      expect(
        prevented.map(({ check }) => endOf(check)),
        "the checks the seeded risks are prevented by"
      ).toEqual([endOf(blockOf(whole, "k-pass"))]);

      // The fourth fixture, converted by envoy-dispatch over this stack's database.
      const second = await createIssue(
        {
          force: true,
          project: PROJECT,
          spec: "## Summary\n\nThe issue the captured pre-mortem is converted on.\n",
          title: "The pre-mortem converted by the command",
        },
        asAgent
      );
      await createIssueArtifact(
        second.key,
        { content: PREMORTEM.document.markdown, name: "graph-premortem.md" },
        asAgent
      );
      const dry = await premortemConvert({
        artifact: PREMORTEM_SLUG,
        dryRun: true,
        issue: second.key,
        preventedBy,
      });
      expect(dry.code, `premortem-convert --dry-run exited ${dry.code}: ${dry.stderr}`).toBe(0);
      expect(
        (await listIssueArtifacts(second.key)).map((artifact) => artifact.slug),
        "the documents after a dry run"
      ).toContain(PREMORTEM_SLUG);
      expect(
        nodesOf(await treeRead(second.key)).filter((node) => node.kind === "lens"),
        "the lenses after a dry run"
      ).toEqual([]);
      const converted = await premortemConvert({
        artifact: PREMORTEM_SLUG,
        dryRun: false,
        issue: second.key,
        preventedBy,
      });
      expect(
        converted.code,
        `premortem-convert exited ${converted.code}: ${converted.stderr}`
      ).toBe(0);
      const convertedTree = await treeRead(second.key);
      const lenses = nodesOf(convertedTree).filter((node) => node.kind === "lens");
      expect(lenses.length, "the lenses of the converted pre-mortem").toBe(1);
      const convertedLens = lenses[0] as IssueTreeNode;
      expect(
        tuplesFrom(convertedTree, convertedLens, "has_kind").map((tuple) => endOf(tuple.to)),
        "the kind of the converted lens"
      ).toEqual(["lens_kind:pre-mortem"]);
      const convertedRisks = tuplesInto(convertedTree, convertedLens, "part_of").filter(
        (tuple) => tuple.from.kind === "risk"
      );
      expect(convertedRisks.length, "the risks under the converted lens").toBe(
        PREMORTEM.risks.length
      );
      expect(
        tuplesOf(convertedTree).filter(
          (tuple) =>
            tuple.kind === "part_of" && tuple.from.kind === "risk" && tuple.to.kind === "risk"
        ),
        "a risk under another risk"
      ).toEqual([]);
      for (const issue of [key, second.key]) {
        expect(
          (await listIssueArtifacts(issue))
            .filter((artifact) => /pre-?mortem/iu.test(artifact.name))
            .map((artifact) => artifact.name),
          `the documents of ${issue} holding a pre-mortem`
        ).toEqual([]);
      }
      // The approval stands against the digest it covered, and the render at that version is the
      // cache artifact_versions holds.
      const reviewsUrl = new URL(`/api/v1/artifacts/${spec}/reviews`, baseUrl);
      const reviewsRead = await fetch(reviewsUrl, { headers: await userHeaders(READER) });
      expect(reviewsRead.status, `GET ${reviewsUrl.pathname} answered ${reviewsRead.status}`).toBe(
        200
      );
      const standing = ((await reviewsRead.json()) as ArtifactReviewRead[]).filter(
        (review) => review.state === "approved" && review.version === approved
      );
      expect(standing.length, `the approvals standing at version ${approved}`).toBe(1);
      const render = await getArtifactVersion(spec, approved);
      expect(standing[0]?.content_sha256, "the digest the spec's approval covers").toBe(
        render.sha256
      );
      const cached = await sql(
        `select markdown from artifact_versions where artifact_id = '${spec}' and number = ${approved}`
      );
      expect(
        render.markdown.trim(),
        `the render of version ${approved} against artifact_versions.markdown`
      ).toBe(cached.trim());

      // The page draws the seeded issue whole from the tree read.
      const rowIds = new Map(ownNodes.map((node) => [endOf(node), rowIdOf(node, key, asks)]));
      const idOf = (end: string) => rowIds.get(end) ?? end;
      await gotoIssue(page, `/issues/${key}`);
      const childRow = rowIdOf(childCheck, key, asks);
      const rows = await rowsDrawn(
        page,
        // The issue itself is the outline's root, which every top-level row stands under, and no
        // row of its own.
        [...new Set([...rowIds.values(), childRow])].filter((id) => id !== key),
        "the outline of the seeded issue"
      );
      for (const node of ownNodes) {
        const drawnParents = parentsOfRow(rows, idOf(endOf(node)));
        const served = tuplesFrom(whole, node, "serves").map((tuple) => idOf(endOf(tuple.to)));
        if ((node.supertag === "check" || node.supertag === "plan_task") && served.length > 0) {
          expect(drawnParents.sort(), `the rows ${node.block_id} is drawn under`).toEqual(
            [...served].sort()
          );
        } else if (node.kind !== "issue") {
          const parent = parentTuple(whole, node).to;
          const want =
            parent.kind === "issue" && parent.id === key ? [null] : [idOf(endOf(parent))];
          expect(
            drawnParents,
            `the row ${idOf(endOf(node))} is drawn under, against its part_of ${endOf(parent)}`
          ).toEqual(want);
        }
      }
      expect(parentsOfRow(rows, childRow), "the row the child's check is drawn under").toEqual([
        blockNode("spec", "r-two"),
      ]);
      expect(
        drawnRow(rows, childRow, "the outline").text,
        "the child's check is marked with the child's key"
      ).toContain(child.key);
      expect(
        parentsOfRow(rows, blockNode("spec", "d-moved")),
        "the row the moved decision is drawn under, out of the note it moved from"
      ).toEqual([null]);
      const preventedRisk = prevented[0]?.risk as IssueTreeNode;
      expect(
        (await linksOf(page, idOf(endOf(preventedRisk)))).some((href) => href.includes("k-pass")),
        "the risk the seeded check prevents draws no link to that check"
      ).toBe(true);

      // The left arrow walks up from r-said, the requirement under the criterion alice wrote in her
      // comment, to that criterion and on to the words that asked for it.
      const said = blockOf(whole, "r-said");
      const criterion = parentTuple(whole, said).to;
      const words = parentTuple(
        whole,
        nodeAt(whole, criterion.kind, criterion.id) as IssueTreeNode
      ).to;
      expect(words.kind, "the kind of what r-said's criterion sits under").toBe("comment");
      await focusRow(page, idOf(endOf(said)));
      for (const step of [criterion, words]) {
        await page.keyboard.press("ArrowLeft");
        await expect(
          nodeRow(page, idOf(endOf(step))).first(),
          `the left arrow from r-said up to ${endOf(step)}`
        ).toBeFocused();
      }

      // Each view draws the answer of the tree read at ?view=.
      for (const view of ["spec", "plan", "verification"] as const) {
        const answer = await treeRead(key, { view });
        const queried = nodesOf(answer).filter((node) => node.role === "query");
        console.log(`the ${view} view of ${key} answers ${queried.length} nodes of its query`);
        expect(queried.length, `the ${view} view answers no node of its query`).toBeGreaterThan(0);
        const allowed = new Set(nodesOf(answer).map((node) => rowIdOf(node, key, asks)));
        await gotoIssue(page, `/issues/${key}/${view}`);
        const viewRows = await rowsDrawn(
          page,
          queried.map((node) => rowIdOf(node, key, asks)),
          `the ${view} view`
        );
        expect(
          viewRows.filter((row) => row.id === null || !allowed.has(row.id)).map((row) => row.id),
          `the ${view} view draws rows the tree read at ?view=${view} does not answer`
        ).toEqual([]);
      }
      await expect(nodeRows(page).first(), "the last view drew no row").toBeVisible();
    }
  );
});
