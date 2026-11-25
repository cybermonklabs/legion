/** Records the API answers for the comments-on-text tests. Run only against a disposable
 *  `e2e/harness-up.ts` stack: recording resets its database. No browser is needed. */
import { mkdirSync, writeFileSync } from "node:fs";
import { join, relative } from "node:path";
import { fileURLToPath } from "node:url";
import { parseArgs } from "node:util";
import { type Body, type Result, routeUrl } from "../web/src/api/paths";
import { CONVERSATION_KINDS, TREE_FILTERS } from "../web/src/api/queries";
import type { IssueTreeNode, IssueTreeRead } from "../web/src/api/types";
import { nextTreeCursor } from "../web/src/features/issue-page/tree-read/tree-nodes";
import { wordsOf } from "../web/src/features/refs/tree-fragment";
import { compareTimestamps } from "../web/src/lib/timestamps";
import { baseUrl, editArtifact, getArtifactBlocks, userHeaders } from "./api";
import { specBlockRef } from "./issue-layout-seed-text";
import {
  type MarginNodeComment,
  type MarginReplyQuote,
  type MarginSeed,
  readMarginTree,
  seedMarginIssue,
} from "./issue-page-margin";
import { GATES, type SkippedMarginItem } from "./issue-page-margin-gates";
import { seedKeys, sortedFields } from "./issue-page-t";
import { resetDatabase } from "./seed";

const ROOT = fileURLToPath(new URL("../../../", import.meta.url));
const FIXTURES = fileURLToPath(
  new URL("../web/src/features/issue-page/text-comments/fixtures/", import.meta.url)
);
const COMMENTS = "/api/v1/issues/{key}/comments";
const MESSAGES = "/api/v1/issues/{key}/messages";
const UUID = /[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}/i;
const SEED_ID = new RegExp(`\\bMRG-\\d+\\b|${UUID.source}`, "gi");
const INSTANT = /\d{4}-\d{2}-\d{2}[T ]\d{2}:\d{2}/;
const ISO_INSTANT = /\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:\d{2})/g;

// T's recorder checks strings and field names, not just the values of known ID fields.
function collectMinted(value: unknown, path: string, found: string[]): void {
  if (typeof value === "string") {
    if (UUID.test(value) || INSTANT.test(value)) found.push(`${path}: ${value}`);
  } else if (Array.isArray(value)) {
    for (const [index, item] of value.entries()) collectMinted(item, `${path}[${index}]`, found);
  } else if (value !== null && typeof value === "object") {
    for (const [field, item] of Object.entries(value)) {
      collectMinted(field, `field name of ${path}`, found);
      collectMinted(item, `${path}.${field}`, found);
    }
  }
}

function expectNothingMinted(value: unknown, path: string): void {
  const found: string[] = [];
  collectMinted(value, path, found);
  if (found.length > 0) {
    throw new Error(
      `${path} still contains ${found.length} UUID/instant(s):\n${found.slice(0, 40).join("\n")}`
    );
  }
}

function nameId(keys: Map<string, string>, id: string, name: string): void {
  if (!keys.has(id)) keys.set(id, name);
}

function seedIds(seed: MarginSeed): Map<string, string> {
  const ids = new Map<string, string>();
  for (const [id, name] of seedKeys(seed, [])) {
    if (UUID.exec(id)?.[0] === id) nameId(ids, id, name);
  }
  ids.set(seed.key, "key");
  ids.set(seed.child, "child");
  ids.set(seed.crossIssueChild, "crossIssueChild");
  return ids;
}

function nameTreeBlocks(keys: Map<string, string>, answer: IssueTreeRead): void {
  const itemBlocks = new Map<string, number>();
  for (const node of answer.nodes ?? []) {
    if (node.tree) {
      nameId(keys, node.tree, `${keys.get(node.id) ?? node.id}.treeArtifactId`);
    }
    // The top-level room prose is named by the seed. Name other server-issued block IDs by
    // their document and position, regardless of which view happened to read them first.
    if (node.block_id && UUID.test(node.block_id) && node.document) {
      const document = keys.get(node.document.artifact_id) ?? node.document.slug;
      nameId(keys, node.block_id, `${document}.block[${node.document.position}]`);
    } else if (node.parent && ((node.block_id && UUID.test(node.block_id)) || UUID.test(node.id))) {
      // A block of a comment, a message or an ask stands under its item with no document:
      // name it by that item and its place among the item's blocks, in the answer's order.
      const item = keys.get(node.parent.id) ?? node.parent.id;
      const place = itemBlocks.get(item) ?? 0;
      itemBlocks.set(item, place + 1);
      if (node.block_id && UUID.test(node.block_id)) {
        nameId(keys, node.block_id, `${item}.itemBlock[${place}]`);
      }
      if (UUID.test(node.id)) {
        nameId(keys, node.id, `${item}.node[${place}]`);
      }
    }
  }
}

function nameAnchorBlock(
  keys: Map<string, string>,
  anchor: DocumentAnchor | null | undefined
): void {
  // A mark can live in a paragraph inside a typed block. That paragraph need not be a row.
  const id = anchor?.block_id;
  const comment = anchor ? keys.get(anchor.mark_id) : undefined;
  if (id && comment && UUID.test(id)) nameId(keys, id, `${comment}.anchorBlock`);
}

function keyText(keys: ReadonlyMap<string, string>, text: string): string {
  return text.replace(SEED_ID, (id) => keys.get(id) ?? id);
}

function keyedFixture(keys: ReadonlyMap<string, string>, data: unknown) {
  // Use one name for each instant in this answer, including `comments_before` if present.
  // The names preserve chronological order, not the time between writes.
  const instants = [...new Set(JSON.stringify(data).match(ISO_INSTANT) ?? [])].sort(
    compareTimestamps
  );
  const times = new Map(
    instants.map((at, index) => [at, `instant-${String(index + 1).padStart(3, "0")}`])
  );
  // A placement sequence is the server's counter, which differs from one run to the next: keep
  // its order in this answer and not its value, so two recordings of one seed are alike.
  const sequences = [
    ...new Set(
      [...JSON.stringify(data).matchAll(/"placement_seq":(\d+)/g)].map((match) => Number(match[1]))
    ),
  ].sort((a, b) => a - b);
  const ranks = new Map(sequences.map((sequence, index) => [sequence, index + 1]));
  const text = (value: string) =>
    keyText(keys, value).replace(ISO_INSTANT, (at) => times.get(at) ?? at);
  function walk(value: unknown, field?: string): unknown {
    if (typeof value === "string") {
      if (field !== "cursor" || value === "") return text(value);
      // Tree cursors contain a URL-base64 encoded kind and node ID.
      const cursor = text(Buffer.from(value, "base64url").toString("utf8"));
      expectNothingMinted(cursor, "tree cursor");
      return Buffer.from(cursor).toString("base64url");
    }
    if (field === "placement_seq" && typeof value === "number") return ranks.get(value) ?? value;
    if (Array.isArray(value)) return value.map((item) => walk(item));
    if (value === null || typeof value !== "object") return value;
    return Object.fromEntries(
      Object.entries(value).map(([field, item]) => [text(field), walk(item, field)])
    );
  }
  return sortedFields(walk(data));
}

async function pageRead<T>(path: string): Promise<T> {
  const response = await fetch(new URL(path, baseUrl), {
    headers: { ...(await userHeaders()), "Dispatch-Trees": "1" },
  });
  if (!response.ok) throw new Error(`GET ${path}: ${response.status} ${await response.text()}`);
  return (await response.json()) as T;
}

type CommentWrite = Body<typeof COMMENTS, "post">;
type MessageWrite = Body<typeof MESSAGES, "post">;
type CommentAnswer = Result<typeof COMMENTS, "post">;
type DocumentAnchor = NonNullable<CommentAnswer["anchor"]>;
interface QuoteAnswer {
  anchor?: DocumentAnchor | null;
  code?: string;
  id?: string;
  // G4 and G2: these stored selections do not exist in B0's generated response types.
  reply_block?: { block_id: string; quote?: string; occurrence?: number };
  node_anchor?: { node: string; quote: string; occurrence?: number };
}

interface QuoteCase {
  item: string;
  node: IssueTreeNode;
  quote: string;
  path: typeof COMMENTS | typeof MESSAGES;
  body: (selection: MarginReplyQuote) => CommentWrite | MessageWrite | MarginNodeComment;
}

async function quoteWrite(
  key: string,
  entry: QuoteCase,
  occurrence: number,
  keys: Map<string, string>
) {
  const url = routeUrl(entry.path, { key });
  const response = await fetch(new URL(url, baseUrl), {
    body: JSON.stringify(entry.body({ occurrence, quote: entry.quote })),
    headers: {
      ...(await userHeaders()),
      "Content-Type": "application/json",
      "Dispatch-Trees": "1",
    },
    method: "POST",
  });
  const status = response.status;
  const answer = (await response.json()) as QuoteAnswer;
  if (status === 201 && answer.id) {
    nameId(keys, answer.id, `quotes.${entry.item}[${occurrence}]`);
    nameAnchorBlock(keys, answer.anchor);
    if (answer.anchor)
      return { anchor: { block_id: answer.anchor.block_id, quote: answer.anchor.quote }, status };
    if (answer.reply_block?.quote !== undefined) return { reply_block: answer.reply_block, status };
    if (answer.node_anchor) return { node_anchor: answer.node_anchor, status };
    throw new Error(
      `Writing ${entry.item} occurrence ${occurrence} returned 201 but did not store a selection`
    );
  }
  if (status >= 400 && status < 500 && answer.code) return { code: answer.code, status };
  throw new Error(
    `Writing ${entry.item} occurrence ${occurrence}: ${status} ${JSON.stringify(answer)}`
  );
}

function quoteOffsets(text: string, quote: string, occurrence: number) {
  let start = -1;
  for (let i = 0; i <= occurrence; i += 1) {
    // Start one character after the last match, not after the end of the quote.
    start = text.indexOf(quote, start + 1);
    if (start < 0) return null;
  }
  return { end: start + quote.length, start };
}

async function recordQuoteCase(key: string, entry: QuoteCase, keys: Map<string, string>) {
  const { item, node, quote } = entry;
  if (!node.text_tree) throw new Error(`The tree read of ${item} did not include text_tree`);
  const plainText = wordsOf(node.text_tree);
  const namedText = keyText(keys, plainText);
  const namedQuote = keyText(keys, quote);
  const writes = [];
  for (const occurrence of [0, 1]) {
    const offsets = quoteOffsets(namedText, namedQuote, occurrence);
    if (occurrence === 0 && offsets === null)
      throw new Error(`${item}: ${JSON.stringify(quote)} is not in ${JSON.stringify(plainText)}`);
    writes.push({
      answer: await quoteWrite(key, entry, occurrence, keys),
      selection: { occurrence, offsets, quote },
    });
  }
  return {
    block_id: node.block_id,
    item,
    node: node.id,
    plainText,
    text: node.text,
    text_tree: node.text_tree,
    writes,
  };
}

async function recordQuotes(seed: MarginSeed, keys: Map<string, string>) {
  const issue = `dispatch://${seed.child}`;
  const reference = specBlockRef(seed.key, seed.statementS1);
  const source = [
    { item: "plain", markdown: "Pick the blue line or the blue square.", quote: "blue" },
    {
      item: "link",
      markdown: "Follow the [reading guide](https://example.com/guide) first.",
      quote: "the reading guide",
    },
    { item: "emphasis", markdown: "Keep the *first* line visible.", quote: "first" },
    { item: "code-span", markdown: "Run `format` from the root.", quote: "format" },
    { item: "issue-reference", markdown: `See ${issue} before you send.`, quote: issue },
    { item: "block-reference", markdown: `See ${reference} before you send.`, quote: reference },
    {
      item: "heading",
      markdown: "### Read the heading\n\nThis is a different line.",
      quote: "Read the heading",
    },
    {
      item: "list-item",
      markdown: "- First list entry\n- Second list entry",
      quote: "First list entry",
    },
    {
      item: "table-cell",
      markdown: "| Type | Value |\n| --- | --- |\n| Color | pale green |",
      quote: "pale green",
    },
    { item: "overlap", markdown: "aaaa", quote: "aaa" },
  ];
  await editArtifact(seed.specArtifactId, {
    ops: [
      {
        after: "end",
        // Decisions can hold headings, lists and tables. Requirements can only hold paragraphs.
        markdown: source
          .map(({ item, markdown }) => `:::decision{#quote-${item}}\n${markdown}\n:::`)
          .join("\n\n"),
        op: "insert",
      },
    ],
  });
  const nodes: IssueTreeNode[] = [];
  const asked = new Set<string>();
  let cursor: string | undefined;
  do {
    const page = await readMarginTree(seed.key, { cursor });
    nameTreeBlocks(keys, page);
    nodes.push(...(page.nodes ?? []));
    cursor = nextTreeCursor(page);
    if (cursor !== undefined) {
      if (asked.has(cursor))
        throw new Error("The Graph read repeated a cursor while reading quote cases");
      asked.add(cursor);
    }
  } while (cursor !== undefined);

  const entries: QuoteCase[] = source.map(({ item, quote }) => {
    const block_id = `quote-${item}`;
    const node = nodes.find(
      (row) => row.block_id === block_id && row.document?.artifact_id === seed.specArtifactId
    );
    if (!node) throw new Error(`The Graph read has no ${block_id} block`);
    return {
      body: (selection) => ({
        anchor: { artifact: seed.specArtifactId, block_id, ...selection },
        body: "A note on the selected text.",
      }),
      item,
      node,
      path: COMMENTS,
      quote,
    };
  });

  const skipped: SkippedMarginItem[] = [];
  // G4: POST uses quote and occurrence alongside the existing block-reply coordinates.
  if (GATES.REPLY_QUOTE) {
    for (const [item, id, block, quote, path] of [
      ["post-block-reply", seed.post, seed.postBlock, "sample", COMMENTS],
      ["message-block-reply", seed.message, seed.messageBlock, "marker", MESSAGES],
    ] as const) {
      const node = nodes.find((row) => row.block_id === block && row.parent?.id.endsWith(id));
      if (!node) throw new Error(`The Graph read has no block for ${item}`);
      entries.push({
        body: (selection) => ({
          body: "A reply on the selected text.",
          ...selection,
          ...(path === COMMENTS
            ? { reply_to: id, reply_to_block: block }
            : { in_reply_to: id, in_reply_to_block: block }),
        }),
        item,
        node,
        path,
        quote,
      });
    }
  } else {
    skipped.push(
      { gate: "REPLY_QUOTE", item: "post-block-reply" },
      { gate: "REPLY_QUOTE", item: "message-block-reply" }
    );
  }
  // G2: node anchors take a node address instead of a document and block ID.
  if (GATES.NODE_ANCHOR) {
    for (const [item, id, address, quote] of [
      ["lens-anchor", seed.lens, seed.lensAddress, "verdict"],
      ["risk-anchor", seed.risk, seed.riskAddress, "delay"],
    ] as const) {
      const node = nodes.find((row) => row.id === id || row.id.endsWith(`/${id}`));
      if (!node) throw new Error(`The Graph read has no node for ${item}`);
      entries.push({
        body: (selection) => ({
          anchor: { node: address, ...selection },
          body: "A note on the node's words.",
        }),
        item,
        node,
        path: COMMENTS,
        quote,
      });
    }
  } else {
    skipped.push(
      { gate: "NODE_ANCHOR", item: "lens-anchor" },
      { gate: "NODE_ANCHOR", item: "risk-anchor" }
    );
  }
  const cases = [];
  for (const entry of entries) cases.push(await recordQuoteCase(seed.key, entry, keys));
  return { cases, skipped };
}

function biomeFormatted(file: string, json: string): string {
  const biome = Bun.which("biome", {
    PATH: [join(ROOT, "node_modules/.bin"), process.env.PATH ?? ""].join(":"),
  });
  if (biome === null)
    throw new Error("Biome is required to format fixtures. Run bun install first.");
  const formatted = Bun.spawnSync({
    cmd: [biome, "format", `--stdin-file-path=${relative(ROOT, file)}`],
    cwd: ROOT,
    stderr: "inherit",
    stdin: new TextEncoder().encode(json),
  });
  if (formatted.exitCode !== 0) throw new Error(`Biome could not format ${file}`);
  // Like T's recorder, keep sorted JSON if Biome skips a file over its size limit.
  return formatted.stdout.toString() || json;
}

async function record(recordedOn: string) {
  await resetDatabase();
  const seed = await seedMarginIssue();
  const keys = seedIds(seed);
  const { key } = seed;
  const reads: Record<string, IssueTreeRead> = {
    conversation: await readMarginTree(key, { kind: [...CONVERSATION_KINDS] }),
    graph: await readMarginTree(key),
  };
  for (const view of TREE_FILTERS) reads[view] = await readMarginTree(key, { view });
  // The browser uses a different route for Decisions. Each tree request above is its first page
  // as on load; it has no cursor or comments_before in the request.
  for (const read of Object.values(reads)) nameTreeBlocks(keys, read);
  const commentsPath = routeUrl(COMMENTS, { key });
  const comments = await pageRead<Result<typeof COMMENTS, "get">>(commentsPath);
  for (const comment of comments) nameAnchorBlock(keys, comment.anchor);
  const commentsWithNodeAnchor = GATES.NODE_ANCHOR
    ? await pageRead<unknown>(`${commentsPath}?node_anchor=true`)
    : undefined;
  const messages = await pageRead<Result<typeof MESSAGES, "get">>(routeUrl(MESSAGES, { key }));
  // Name any document blocks whose minted IDs are mentioned by another record but not drawn.
  for (const artifact of [
    seed.specArtifactId,
    seed.verificationArtifactId,
    seed.planArtifactId,
    seed.childVerificationArtifactId,
  ]) {
    const blocks = await getArtifactBlocks(artifact);
    for (const [index, block] of blocks.entries()) {
      if (UUID.test(block.id)) nameId(keys, block.id, `${keys.get(artifact)}.blocks[${index}]`);
    }
  }
  const quotes = await recordQuotes(seed, keys);
  const metadata = { gate: "B0", recordedOn };
  const fixtures = {
    "comments.json": { ...metadata, comments, commentsWithNodeAnchor },
    "messages.json": { ...metadata, messages },
    "quotes.json": { ...metadata, ...quotes },
    // One file for each view's read: the five reads in one file pass 1 MiB once formatted,
    // which is jj's largest new file and Biome's largest checked one.
    ...Object.fromEntries(
      Object.entries(reads).map(([view, read]) => [
        `tree-b0-${view}.json`,
        {
          ...metadata,
          pagination: "the first page of the read, as on load",
          read,
          skipped: seed.skipped,
          view,
        },
      ])
    ),
  };
  // Check and format every fixture before touching any file on disk.
  const outputs = Object.entries(fixtures).map(([name, data]) => {
    const fixture = keyedFixture(keys, data);
    expectNothingMinted(fixture, name);
    const path = join(FIXTURES, name);
    return { path, source: biomeFormatted(path, `${JSON.stringify(fixture, null, 2)}\n`) };
  });
  mkdirSync(FIXTURES, { recursive: true });
  for (const { path, source } of outputs) {
    writeFileSync(path, source);
    console.log(`wrote ${relative(process.cwd(), path)}`);
  }
}

if (import.meta.main) {
  if (process.env.DISPATCH_E2E_REUSE_SERVERS !== "1" || process.env.PLAYWRIGHT_BASE_URL) {
    console.error(
      "Start a disposable stack with bun e2e/harness-up.ts and source its exports (DISPATCH_E2E_REUSE_SERVERS=1). The recorder resets that stack's database."
    );
    process.exit(2);
  }
  try {
    const { values } = parseArgs({ options: { "recorded-on": { type: "string" } }, strict: true });
    const recordedOn = values["recorded-on"];
    if (recordedOn === undefined || !/^[0-9a-f]{7,64}$/.test(recordedOn)) {
      throw new Error("usage: bun e2e/issue-page-margin-record.ts --recorded-on <commit>");
    }
    await record(recordedOn);
  } catch (error) {
    console.error(error);
    process.exit(1);
  }
}
