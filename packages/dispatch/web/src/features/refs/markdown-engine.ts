import {
  HARD_BREAK_PIECE,
  referenceReading,
  type WordsFormatting,
  type WordsPiece,
  wordsAreAddress,
  wordsFormatting,
  wordsPieceUnder,
} from "@legion/contracts";
import { documentFromJSON } from "@legion/proof-editor/document-json";
import type { SoftBreaks } from "@legion/proof-editor/headless";
import type {
  Attrs,
  DOMOutputSpec,
  DOMSerializer,
  Mark,
  Node as ProseMirrorNode,
  Schema,
} from "prosemirror-model";
import { useLayoutEffect, useState } from "react";

import type { DisplayTree } from "../../api/types";
import { compiledBlockSchema } from "../doc/schema";
import { renderAskBlockShell, renderTypedBlock } from "../doc/typed-block-drawing";
import type { ParsedBody, ParseRequest } from "./markdown-parse.worker";
import { markdownWorkerThreshold } from "./markdown-worker-threshold";
import { isPartyRoute } from "./party-references";
import { keyMayNameIssue } from "./project-keys";
import {
  buildDispatchReference,
  type DispatchReferenceRoute,
  nodeRouteFromHref,
  referenceHref,
  referenceRouteFromHref,
  shortForm,
} from "./routes";
import { readTextReferences, type TextReference } from "./text-references";
import { rememberUpTo, type WorkerJob, WorkerQueue } from "./worker-queue";

/** What a single newline inside a paragraph becomes, chosen per parse: `"space"` (the document
 *  default, hard-wrapped Markdown is one paragraph) or `"line"` (text whose author meant its
 *  lines as lines, a model's streamed turn in the live view). The engine's own type. */
export type { SoftBreaks };

/**
 * `DOMSerializer.renderSpec` with the attribute object prosemirror-model's own serializer passes
 * it. Upstream's signature is `renderSpec(doc, structure, xmlNS = null, blockArraysIn)`, and
 * `blockArraysIn` is what makes it refuse a spec array that came out of an attribute object
 * (`suspiciousAttributes`), the guard a node's own `toDOM` is held to everywhere else.
 * prosemirror-model 1.25.11 declares only the first three in `dist/index.d.ts`, so the fourth is
 * passed through this one named type rather than at each call (sjawhar/legion#2519).
 */
export type RenderSpecWithAttrs = (
  doc: Document,
  structure: DOMOutputSpec,
  xmlNS: string | null,
  blockArraysIn: Attrs
) => { dom: HTMLElement; contentDOM?: HTMLElement };

/**
 * The one headless Proof engine every Markdown-bearing surface parses with - `MarkdownBody` for a
 * whole body, `MarkdownPreview` for the clamped line of one - so a question, a comment, a hover
 * card and a search hit all read their source the way the document editor does. Its
 * `DOMSerializer` (`pictureSerializer`) is built once with it: the schema is fixed for the
 * engine's life, and every render on the page would otherwise rebuild the same serializer.
 */
export interface MarkdownEngine {
  readonly serializer: DOMSerializer;
  /** `DOMSerializer.renderSpec`, which opens one node where the serializer would write it whole,
   *  for the painter that fills an element a batch at a time (`MarkdownBody`,
   *  `paintBlockInSlices`). It is a static of that class, and this module names the class as a
   *  type alone: everything written against prosemirror-model's own classes is built in the
   *  chunk the engine loads with, so the first route parses none of them
   *  (`markdown-serializer.ts`). */
  readonly renderSpec: RenderSpecWithAttrs;
  /** The schema this engine parses with, which the parse worker is built from too: what turns
   *  the worker's answer, a document as `ProseMirrorNode.toJSON()` gives it, back into a document
   *  of this page's own schema, the one carrying its `renderBlock`. */
  readonly schema: Schema;
  /** `schema` with the search highlight beside its own marks: what a tree the server sent is
   *  drawn into (`draw`). */
  readonly displaySchema: Schema;
  /**
   * The document a tree Go read a stored text into describes (`pmdoc.ReadForDisplay`), drawn
   * into `displaySchema` with no parser of this page's own; `undefined` for a tree that schema
   * refuses, which the caller shows as the stored text's own literal characters. A tree is read
   * once here and drawn by every surface that shows that text.
   */
  draw(tree: DisplayTree): ProseMirrorNode | undefined;
  /**
   * The parsed document, or `undefined` for a source the parser refuses, which the caller shows
   * as one plain-text node. The engine parses read-only: a container directive the block schema
   * refuses (`:::note` … `:::`), or a typed block whose content breaks its content rule (an ask
   * holding a heading), is drawn as its literal fence lines around its formatted content rather
   * than refused or silently dropped, a Pandoc `::: warning` fence or a `::leaf` / `:text{` line
   * reads as paragraph text, and so does a `:::` opener inside 32 open container directives, the
   * deepest the parser nests them (`directive-container.ts`), so none of them throws. A
   * CommonMark raw-HTML span or block (a bare tag-shaped substring outside a code span or fence,
   * e.g. `<img src=x>`) parses as an `html` node holding its literal characters, a block of HTML
   * inside a paragraph of its own; a mark span of a kind this schema does not carry has no node,
   * so the parser still throws for one. HTML-like text inside a code span or fence parses
   * safely as literal text, so nothing is escaped beforehand: escaping the whole source
   * regardless of context would print literal backslashes around any
   * code span or fence containing something tag-shaped. Reference-style links and images
   * (`[text][id]` with their `[id]: url` definition) resolve to the link or image they name, so
   * they do not throw.
   *
   * A throw leaves the parser's own stack mid-document, so the headless editor drops that
   * parser and the next parse builds a fresh one (about 0.03 ms): one refused body answers
   * `undefined` here and falls back to literal text, and the next turn or card someone else is
   * reading still formats.
   *
   * The read-only parse (`remarkReadOnlyDirectives` in @legion/proof-editor) drops every
   * attribute the block schema marks `server` on a typed block, so it reads at its schema
   * default: outside a document no server wrote it, so a body's `:::ask{state="answered" …}`
   * reads as the open decision its author wrote, never as an answer nobody recorded.
   */
  parse(markdown: string, softBreaks?: SoftBreaks): ProseMirrorNode | undefined;
}

type HeadlessModule = typeof import("@legion/proof-editor/headless");

let headlessModule: Promise<HeadlessModule> | undefined;
/** The one engine this page builds, since the manifest it builds from is this build's own
 *  (`compiledBlockSchema`) and cannot change under a page: main kept these by schema version,
 *  when the page fetched a schema from whichever server answered and two versions could meet. */
let loadingEngine: Promise<MarkdownEngine> | undefined;
/** The engine the newest resolved load produced: what a body renders with synchronously. The
 *  engine serves every soft-break policy (the policy is a parse argument), and a parse that
 *  throws no longer evicts it - the headless editor drops only its own refused parser. */
let readyEngine: MarkdownEngine | undefined;

function loadEngine(): Promise<MarkdownEngine> {
  if (loadingEngine !== undefined) {
    return loadingEngine;
  }
  headlessModule ??= import("@legion/proof-editor/headless");
  // The serializer travels with the headless editor rather than with this module: it is written
  // against prosemirror-model's own classes, and nothing can build one before that editor's
  // schema exists (`markdown-serializer.ts`).
  const created = Promise.all([headlessModule, import("./markdown-serializer")])
    .then(async ([{ createHeadlessProof }, { buildEngine }]) =>
      buildEngine(
        await createHeadlessProof({
          blockSchema: compiledBlockSchema,
          readOnly: true,
          renderBlock: (node, type) =>
            type.name === "ask" ? renderAskBlockShell(node) : renderTypedBlock(node, type),
        })
      )
    )
    .then((engine) => {
      readyEngine = engine;
      return engine;
    });
  loadingEngine = created;
  return created;
}

/** Loads the headless Proof chunk before Markdown-bearing UI needs to render; it reads no schema
 *  from a server, since the manifest is this build's own. The parse worker is not loaded here: a
 *  page that draws no long body downloads none of it. */
export async function warmMarkdownRenderer(): Promise<void> {
  await loadEngine();
}

/** How long the worker may hold one body before the page parses it itself. It runs from the
 *  moment the worker can start on that body, never while its chunk is still downloading, as the
 *  diagram renderer's own limit does (`mermaid-diagrams.ts`). */
const PARSE_TIME_LIMIT_MS = 10_000;

/** How many parsed documents the answer cache keeps; the one read longest ago goes first. A long
 *  body's document is large, and the renders that read one again are a page's own repaints, so a
 *  few are enough: the bodies on screen, and the streaming turn growing among them. */
const ANSWER_LIMIT = 4;

/**
 * One rendering of a body: the engine it renders with, the document a surface draws, and whether
 * it is still owed. A stored text is rendered once, from the tree the server read it into, in
 * the caller's own commit: it is never parsed here and never sent to the worker, whatever its
 * length. A text Dispatch does not store is parsed: one of at most `markdownWorkerThreshold`
 * characters in the caller's own commit, a longer one at once as pending, with no document - its
 * literal text is what the reader reads meanwhile - and again with the document once the worker
 * answers.
 */
export interface MarkdownRendering {
  /** `undefined` where neither the schema nor the engine could load, and while the parse is
   *  pending on a page where they are still loading. */
  readonly engine: MarkdownEngine | undefined;
  /** The document to draw: the server's tree for a stored text, the engine's parse for one
   *  Dispatch does not store. `undefined` while `pending`, and, once not pending, for a tree the
   *  schema refused, a body the parser refused or an engine that could not load: the body's one
   *  final failure, which a surface shows as its literal text. */
  readonly parsed: ProseMirrorNode | undefined;
  /** The parse is still owed - by the worker, or by a schema and engine chunk still loading. */
  readonly pending: boolean;
  /** `mermaid-diagrams.ts`'s open-fence check, as the worker made it with the parse it answered;
   *  `undefined` where the page must make it itself. */
  readonly openMermaidFence: boolean | undefined;
}

/** Whether a body is parsed in the worker rather than on the page's own thread.
 *  `useRenderedMarkdown` reads it too: such a body's first paint is the one put into the page a
 *  batch at a time, since it is the one that lands after its own commit. */
export function parsesInTheWorker(markdown: string): boolean {
  return markdown.length > markdownWorkerThreshold;
}

/** Creates the parse worker; `setParseWorkerFactory` replaces it for a test. */
export type ParseWorkerFactory = () => Worker;

/** A body's parse: the worker's answer to it, or the page's own where no worker could give one. */
interface ParsedAnswer {
  /** The parsed document; `undefined` for a body the parser refused. */
  readonly parsed: ProseMirrorNode | undefined;
  /** The open-fence check made with the parse; `undefined` where the page must make it. */
  readonly openMermaidFence: boolean | undefined;
}

/** One body the parse worker is parsing, or is about to (`worker-queue.ts`). */
interface SentBody extends WorkerJob {
  /** The engine its answer becomes a document of, and that parses it here if the worker cannot. */
  readonly engine: MarkdownEngine;
  /** Its entry in the answer cache, and its place in the queue: the schema version, the
   *  soft-break policy and the source. */
  readonly key: string;
  readonly markdown: string;
  readonly softBreaks: SoftBreaks;
}

let createParseWorker: ParseWorkerFactory | undefined;
/** `PARSE_TIME_LIMIT_MS`, unless a test shortened it through `setParseDeadline`. */
let deadlineMs = PARSE_TIME_LIMIT_MS;
const answers = new Map<string, ParsedAnswer>();

/** Parses `body` here, for a worker that could not: the reader waits for a parse on their own
 *  thread, and then reads the body formatted. */
function parseOnThePage(body: SentBody): ParsedAnswer {
  const answer: ParsedAnswer = {
    openMermaidFence: undefined,
    parsed: body.engine.parse(body.markdown, body.softBreaks),
  };
  rememberUpTo(answers, ANSWER_LIMIT, body.key, answer);
  return answer;
}

/**
 * The page's own worker: this module's sibling, which Vite bundles as a chunk of its own
 * (`markdown-parse.worker-<hash>.js`), fetched the first time a page draws a long body. The
 * `new Worker(new URL(…), { type: "module" })` below is written as one expression because that is
 * the shape a bundler recognises; split in two, the worker's module is copied out as an asset and
 * never bundled.
 */
function newParseWorker(): Worker {
  return new Worker(new URL("./markdown-parse.worker.ts", import.meta.url), { type: "module" });
}

/**
 * The bodies waiting on the parse worker, through the queue both workers of this folder share
 * (`worker-queue.ts`). A body the worker cannot answer - its deadline ran out, the worker
 * crashed, or none could be created at all - is parsed on the page instead, so a reader always
 * ends with a formatted body; only the parser refusing a body ends in its literal text.
 *
 * The worker is kept rather than terminated where the page moves on from the body it holds
 * (`stopAnAbandonedJob: false`): it carries a module of its own and builds a headless engine
 * before it can answer anything, which costs more than the one stale parse it would save. A turn
 * streaming into the live view therefore leaves the worker exactly one stale frame - the one it
 * was already parsing - and the frame the reader is waiting for is posted the moment that answer
 * lands, rather than queueing behind every frame the stream has written since. That stale frame
 * is the newest parse the reader can be given until the stream pauses, so the body that asked
 * for it is still shown it (`keepAPostedParse`), rather than watching an unchanging paint.
 */
const parseQueue = new WorkerQueue<SentBody, ParsedBody, ParsedAnswer>({
  stopAnAbandonedJob: false,
  answerOf(body, reply) {
    if (reply.doc === undefined) {
      // The parser refused the body: its one final failure, which reads as its literal text.
      const refused: ParsedAnswer = { openMermaidFence: undefined, parsed: undefined };
      rememberUpTo(answers, ANSWER_LIMIT, body.key, refused);
      return refused;
    }
    let answer: ParsedAnswer;
    try {
      answer = {
        openMermaidFence: reply.openMermaidFence,
        parsed: documentFromJSON(body.engine.schema, reply.doc),
      };
    } catch (error) {
      console.error(
        "Parsing a long Markdown body on the page, its answer does not fit the schema",
        error
      );
      return parseOnThePage(body);
    }
    rememberUpTo(answers, ANSWER_LIMIT, body.key, answer);
    return answer;
  },
  createWorker() {
    // The worker builds its engine from the manifest this build compiled in
    // (`compiledBlockSchema`, which it imports itself), so the page sends it no schema: both
    // sides read the same manifest whatever a server of another build would answer.
    return (createParseWorker ?? newParseWorker)();
  },
  limitMs() {
    return deadlineMs;
  },
  noWorkerFrom(error) {
    console.error(
      "Parsing long Markdown bodies on the page, the parse worker is unavailable",
      error
    );
  },
  post(worker, id, body) {
    worker.postMessage({
      id,
      markdown: body.markdown,
      softBreaks: body.softBreaks,
    } satisfies ParseRequest);
  },
  withoutAWorker: parseOnThePage,
  withoutTheWorker: parseOnThePage,
});

/**
 * Replaces the page's own `new Worker(…)` with `factory`, for the rows that drive a faked worker
 * (`parse-worker-fixture.ts`); `undefined` puts the page's own back. Installing one leaves the
 * engine holding no worker and no answer: the one it held is terminated, every body it held is
 * parsed on the page, and the answer cache is emptied after that, as `resetMermaidDiagrams`
 * empties the diagram renderer's, so nothing a page asked the worker before reaches the next one.
 */
export function setParseWorkerFactory(factory: ParseWorkerFactory | undefined): void {
  parseQueue.recover();
  parseQueue.clear(parseOnThePage);
  answers.clear();
  createParseWorker = factory;
}

/** How long the worker may hold one body, in place of `PARSE_TIME_LIMIT_MS` until the next call,
 *  so a row that proves what the deadline ends in need not wait it out, as
 *  `resetMermaidDiagrams` does for the diagram renderer's limit. With no argument it puts the
 *  page's own deadline back. */
export function setParseDeadline(limitMs = PARSE_TIME_LIMIT_MS): void {
  deadlineMs = limitMs;
}

/** What a caller of `renderWithEngine` wants beyond the rendering itself. */
export interface RenderWithEngineOptions {
  /** Keeps the caller's place on a parse the worker has already been posted when the render is
   *  cancelled, so that answer still reaches `render` (`worker-queue.ts`, `keepIfPosted`). Every
   *  element `useRenderedMarkdown` draws asks for it, whatever surface it is on: the streaming
   *  turn is where it matters - its reader would otherwise watch a body that never changes -
   *  and no other surface is harmed by it, since an element decides for itself what an answer to
   *  a source it has moved on from is worth (sjawhar/legion#2519). A caller that asks must
   *  expect such an answer, and gets nothing in place of one: the deadline and a worker that
   *  never runs answer a kept place with no parse at all, since the render that asked has gone.
   *  `useMarkdownHeadline` is the caller that does not ask - a headline of a source it has moved
   *  on from would read as the wrong one - and off is the default: a cancelled render runs no
   *  more. */
  readonly keepAPostedParse?: boolean;
}

/** Renders a body with a loaded engine: the server's tree drawn for a stored text; otherwise
 *  parsed here for a body up to the threshold, and for a longer one the answer already cached
 *  for it, or a pending rendering now and the parsed document once the worker answers. */
function renderLoaded(
  engine: MarkdownEngine,
  markdown: string,
  tree: DisplayTree | undefined,
  softBreaks: SoftBreaks,
  render: (rendering: MarkdownRendering) => void,
  { keepAPostedParse = false }: RenderWithEngineOptions
): () => void {
  if (tree !== undefined) {
    // A stored text is drawn from the tree the server read it into, however long it is: there
    // is nothing to parse, so neither the page's own thread nor the worker reads it. Its
    // mermaid fences are closed - the server stored the whole text - so every mermaid block of
    // it draws, and `drawMermaidDiagrams` asks for no second parse to find out.
    render({ engine, openMermaidFence: false, parsed: engine.draw(tree), pending: false });
    return () => undefined;
  }
  if (!parsesInTheWorker(markdown)) {
    render({
      engine,
      openMermaidFence: undefined,
      parsed: engine.parse(markdown, softBreaks),
      pending: false,
    });
    return () => undefined;
  }
  // The soft-break policy and the source are what a parse reads - the manifest is this build's
  // own and cannot change under a page (`compiledBlockSchema`) - so one answer serves every
  // later render of the same two: a new decision card, paint or decorate pass repaints the body
  // it already has rather than asking for it again.
  const key = `${softBreaks}\u0000${markdown}`;
  const cached = answers.get(key);
  if (cached !== undefined) {
    // Reading an answer keeps it: what the cache drops is the body read longest ago, never one
    // the page is still repainting.
    answers.delete(key);
    answers.set(key, cached);
    render({
      engine,
      openMermaidFence: cached.openMermaidFence,
      parsed: cached.parsed,
      pending: false,
    });
    return () => undefined;
  }
  // What the reader has until the answer lands is the body's own text.
  render({ engine, openMermaidFence: undefined, parsed: undefined, pending: true });
  return parseQueue.wait(
    key,
    () => ({ engine, key, markdown, softBreaks }),
    ({ openMermaidFence, parsed }) => {
      render({ engine, openMermaidFence, parsed, pending: false });
    },
    { keepIfPosted: keepAPostedParse }
  );
}

/**
 * The shared engine when a load has already resolved, and `undefined` until then: what a caller
 * that must answer in this task draws or parses with. It starts no load of its own - a caller
 * that wants one awaits `warmMarkdownRenderer` first, as the change marks' caller does - and so
 * is the synchronous half of `renderWithEngine` without its callback or its background load.
 */
export function loadedEngine(): MarkdownEngine | undefined {
  return readyEngine;
}

/**
 * Hands `use` the shared engine: in this task where a load has already resolved, and once the
 * schema and Proof's headless chunk arrive otherwise. `undefined` where neither could load,
 * which the cause of is reported and the schema cache does not retain, so the next caller tries
 * the fetch again. Returns the cancel, after which `use` never runs.
 */
function withEngine(use: (engine: MarkdownEngine | undefined) => void): () => void {
  if (readyEngine !== undefined) {
    use(readyEngine);
    return () => undefined;
  }
  let cancelled = false;
  void (async () => {
    let engine: MarkdownEngine | undefined;
    try {
      engine = await loadEngine();
    } catch (error) {
      console.error("Rendering literal Markdown, the Markdown engine is unavailable", error);
    }
    if (!cancelled) {
      use(engine);
    }
  })();
  return () => {
    cancelled = true;
  };
}

/**
 * Draws the stored text `tree` was read from, through the shared engine, and hands the document
 * to `drawn`: in this task where the engine has loaded, and once it does otherwise.
 * `undefined` for a tree the schema refuses and for an engine that cannot load. Returns the
 * cancel. It is what a caller that wants the words of a stored text rather than its nodes uses
 * (`useMarkdownHeadline`); a surface that paints one uses `renderWithEngine`.
 */
export function drawWithEngine(
  tree: DisplayTree,
  drawn: (document: ProseMirrorNode | undefined) => void
): () => void {
  return withEngine((engine) => {
    drawn(engine?.draw(tree));
  });
}

/**
 * Renders a body through the shared engine: `tree`, the reading Go made of a stored text, drawn
 * as it stands; or, for a text Dispatch does not store and so sent no tree for, `markdown`
 * parsed here under the soft-break policy `softBreaks`.
 *
 * A stored text is drawn in the layout phase of the commit that mounts it where a load has
 * already resolved, whatever its length: drawing reads no markdown, so neither the page's own
 * thread nor the parse worker parses it, and the engine's answer cache holds nothing for it.
 *
 * A text with no tree of its own is parsed. One of at most `markdownWorkerThreshold` characters
 * is parsed on the page's own thread and rendered synchronously, where a load has already
 * resolved, so it renders in the layout phase of the commit that mounts it (a list that inserts
 * a turn sees the turn's full height in that same commit, and `ViewportAnchor` compensates in
 * one measurement); otherwise once the schema and the engine chunk arrive. Only the very first
 * render on a page, or a schema that failed to load, takes that asynchronous path. Without the
 * server schema (or Proof's headless engine) `render` gets no engine and no document, and the
 * surface shows the literal text instead: readable, never lost.
 *
 * A longer one is parsed in the parse worker (`markdown-parse.worker.ts`), created on the first
 * one a page draws. `render` is called at once with a pending rendering, so the reader has the
 * body's own text, and again with the parsed document when the worker answers. A worker that
 * cannot be created, whose chunk does not load, that crashes, or that holds a body past its
 * deadline leaves that body to the page's own thread, so a reader always ends with a formatted
 * body; only the parser refusing a body ends in its literal text. That is for a render still
 * waiting: a place kept on a posted parse belongs to a render already cancelled, so it takes
 * the worker's own answer or none, and the page parses nothing for it (sjawhar/legion#2519).
 *
 * Returns the cancel: a cancelled `render` never runs again - unless the caller asked to keep a
 * posted parse (`RenderWithEngineOptions`) and the worker is holding that body, whose answer
 * still reaches it - and the body it was waiting for is parsed for whoever else waits.
 */
export function renderWithEngine(
  markdown: string,
  tree: DisplayTree | undefined,
  softBreaks: SoftBreaks,
  render: (rendering: MarkdownRendering) => void,
  options: RenderWithEngineOptions = {}
): () => void {
  if (readyEngine !== undefined) {
    return renderLoaded(readyEngine, markdown, tree, softBreaks, render, options);
  }
  let cancelAnswer: (() => void) | undefined;
  if (tree === undefined && parsesInTheWorker(markdown)) {
    // The schema and the engine chunk are still loading, and the body is one the worker will
    // parse: the reader has its text now, as they would while the worker parsed it.
    render({ engine: undefined, openMermaidFence: undefined, parsed: undefined, pending: true });
  }
  const cancelLoad = withEngine((engine) => {
    if (engine === undefined) {
      render({ engine: undefined, openMermaidFence: undefined, parsed: undefined, pending: false });
      return;
    }
    cancelAnswer = renderLoaded(engine, markdown, tree, softBreaks, render, options);
  });
  return () => {
    cancelLoad();
    cancelAnswer?.();
  };
}

/** `.dispatch-markdown` in styles.css overrides Tailwind Typography's fixed palette, heading
 * scale and font size, so a rendered body takes the surrounding text's size and colour. */
export const markdownClassName =
  "dispatch-markdown prose prose-sm prose-slate break-words dark:prose-invert";

/** What one reference a projected text writes resolved to, under its `dispatch://` reference
 *  (`MarkdownHeadline.addresses`): `name`, what the reference reads as standing on its own - its
 *  target's current name, or undefined where Dispatch answered that its target does not exist or
 *  exists with no name of its own, where it reads as the words its author wrote (R8) - and
 *  `mention`, what a party's mention of it reads as, undefined where the mention keeps its
 *  author's words. An issue's name is its title alone, which the projection writes after its key
 *  (`standingName`), so a surface that sets the key apart reads the title alone. A reference the
 *  lookup does not answer has not been read yet. */
export interface ReferenceName {
  readonly name: string | undefined;
  readonly mention: string | undefined;
}

/** What each reference a projected text writes resolved to, by its `dispatch://` reference: a
 *  map, or a view over the sources a caller names references from, read through without a copy
 *  of them (`plain-text.tsx`). `undefined` for a reference not read yet. */
export interface ReferenceNameLookup {
  get(reference: string): ReferenceName | undefined;
}

/** The words `answer` names a reference to `route` by standing on its own: an issue's key and
 *  then its title, as a link to an issue reads (`RefLink`), and any other target's name.
 *  Undefined where it names none. */
function standingName(
  route: DispatchReferenceRoute,
  answer: ReferenceName | undefined
): string | undefined {
  const name = answer?.name;
  return name === undefined || route.kind !== "issue" ? name : `${route.key} ${name}`;
}

/** What a caller knows of the references a projected text writes: what each one resolved to
 *  (`names`); the question map of the document the text's source belongs to, which a `#id`
 *  resolves against (`questions`) - absent for a caller that names no source, whose `#id` reads
 *  as written, as a rendered body with no source links none; the references the text's own
 *  markdown cites (`cited`, `citedReferences`), which the projection is held to as a rendered
 *  body's link pass is (`ReferenceLinking.cited`): Markdown drops the backslash of an escaped
 *  `\#q2`, so the words hold a `#q2` the source never wrote, and it stays those words; and the
 *  keys of the projects the page holds (`projects`, `project-keys.ts`), so a word shaped like an
 *  issue key whose prefix is no project - `UTF-16` - is no reference to read. A caller with no
 *  markdown or no project list to read leaves either out, and every reference the words hold is
 *  named. */
export interface ReferenceNaming {
  readonly names: ReferenceNameLookup;
  readonly questions?: Readonly<Record<string, string>>;
  readonly cited?: ReadonlySet<string>;
  readonly projects?: ReadonlySet<string>;
  /** Whether the caller reads none of the references the projection answers: a title drawn over
   *  one record's text, where a name inside a name resolves nothing (`useReferenceTarget`). No
   *  name ever takes a reference's place there, so a `dispatch://` address reads as its short
   *  form (`dispatch://CORE-1/ask/a1` reads `CORE-1 ask`) and the title never shows the scheme.
   *  For any other caller, a reference `names` does not hold is one not read yet, and it reads
   *  as the words its author wrote until it resolves, as a rendered body's link does
   *  (`RefLink`). */
  readonly readsNone?: boolean;
}

/** Nothing known yet, shared so a projection that needs nothing keeps one identity across renders
 *  and the layout effect it is a dependency of never re-runs for a fresh empty value. */
export const NO_REFERENCE_NAMING: ReferenceNaming = { names: new Map() };

/** What one text of a document projects to: its words, the `dispatch://` reference of every
 *  reference it read outside code and a link's words, those of them it wrote a name for in place
 *  of the words their author wrote, and whether it holds a `#id`, which only a question map
 *  resolves. */
interface ProjectedText {
  readonly words: string;
  readonly addresses: readonly string[];
  readonly named: readonly string[];
  readonly questions: boolean;
}

/** Where one line's references are written as the projection reads them: every one it read
 *  (`ProjectedText.addresses`), and those of them it wrote a name for. */
interface LineReferences {
  readonly addresses: string[];
  readonly named: string[];
}

/** Whether a rendered body draws a reference to `route` by name at all (`RefLink.tsx`'s
 *  `linksReference` and `collectReferenceAnchors`): one that opens a page of this dashboard, and a
 *  person, whom no page shows but who reads as their login (R5). Any other is its words. */
function drawnByName(route: DispatchReferenceRoute): boolean {
  return referenceHref(route) !== undefined || route.kind === "person";
}

/** What a candidate the one reader found names, as the projection reads it: its `dispatch://`
 *  reference and the route behind it. An issue key names its issue whether or not the reader
 *  was told the issue exists, since the read of that reference is what says so - where its
 *  prefix is a project the page holds (`keyMayNameIssue`, the rule a rendered body's reader
 *  keeps). Undefined for a candidate that names nothing - a `#id` no question map declares, a
 *  key of no project - and for one a rendered body draws as its words (`drawnByName`). */
function candidateTarget(
  found: TextReference,
  projects: ReadonlySet<string> | undefined
): { reference: string; route: DispatchReferenceRoute } | undefined {
  const route =
    found.route ??
    (found.form === "key" && keyMayNameIssue(found.text, projects)
      ? { key: found.text, kind: "issue" as const }
      : undefined);
  if (route === undefined || !drawnByName(route)) {
    return undefined;
  }
  return { reference: found.reference ?? buildDispatchReference(route), route };
}

/** The references `markdown` cites, each by its `dispatch://` reference (`candidateTarget`), read
 *  by the one reader with the question map a caller holds: what `ReferenceNaming.cited` is. */
export function citedReferences(
  markdown: string,
  questions: Readonly<Record<string, string>> | undefined
): ReadonlySet<string> {
  const cited = new Set<string>();
  const reading = readTextReferences(markdown, { origin: window.location.origin, questions });
  for (const found of reading.references) {
    const target = candidateTarget(found, undefined);
    if (target !== undefined) {
      cited.add(target.reference);
    }
  }
  return cited;
}

/**
 * Text outside code and a link's words with every reference in it written as the renderer draws
 * it, by the one reader of what a text cites (`readTextReferences`) and the facts `naming` holds:
 * a `dispatch://` reference, a dashboard address, a short id, an issue key and - where `naming`
 * holds its document's question map - a `#id`, each as the name its target resolved to, and as
 * the words its author wrote until it resolves and where Dispatch answered that it has none, as
 * a rendered body draws the words of a link whose title has not resolved (`RefLink`). For a
 * caller that reads none of them (`ReferenceNaming.readsNone`), a `dispatch://` address reads as
 * its short form instead, and every other form as written. A reference the text's source does
 * not cite (`ReferenceNaming.cited`) is the words it is. Each reference read is added to
 * `references` by its `dispatch://` reference, an issue key's being its issue's, and among those
 * named where a name was written in its place.
 */
function nameReferences(
  text: string,
  naming: ReferenceNaming,
  references: LineReferences
): { words: string; questions: boolean } {
  const reading = readTextReferences(text, {
    origin: window.location.origin,
    questions: naming.questions,
  });
  let words = "";
  let cursor = 0;
  for (const found of reading.references) {
    const target = candidateTarget(found, naming.projects);
    if (target === undefined || naming.cited?.has(target.reference) === false) {
      continue;
    }
    references.addresses.push(target.reference);
    const name =
      naming.readsNone === true
        ? undefined
        : standingName(target.route, naming.names.get(target.reference));
    if (name !== undefined) references.named.push(target.reference);
    const drawn =
      naming.readsNone === true && found.form === "address" && found.text.startsWith("dispatch://")
        ? shortForm(target.route)
        : (name ?? found.text);
    words += `${text.slice(cursor, found.start)}${drawn}`;
    cursor = found.end;
  }
  return { questions: reading.needs.questions, words: words + text.slice(cursor) };
}

/**
 * What a link reads as, where that is not the words its author wrote, as a rendered body draws it
 * (`RefLink.tsx`'s `collectReferenceAnchors`, by `@legion/contracts`'s `referenceReading`): a link
 * whose words are its own address as its target's name, and a party's mention as the mention
 * sign and the party's name (`ReferenceName.mention`), each once its target has resolved. For a
 * caller that reads none of them (`ReferenceNaming.readsNone`), a link whose words are its own
 * `dispatch://` address reads as its short form, as the bare address does. Undefined where the
 * link reads as its words - an author's own words, words formatted in part or holding code or a
 * hard break, an address that names nothing, a target not yet read or that Dispatch answered has
 * no name - and the link's reference is added to `references` where it has one to read, and
 * among those named where a name was written in place of its words.
 */
function linkDrawn(
  href: string,
  words: string,
  formatting: WordsFormatting,
  naming: ReferenceNaming,
  references: LineReferences
): string | undefined {
  const origin = window.location.origin;
  const route = wordsAreAddress(words, href)
    ? referenceRouteFromHref(href, origin)
    : nodeRouteFromHref(href, origin);
  if (route === undefined || !drawnByName(route)) {
    return undefined;
  }
  const reads = referenceReading({
    address: href,
    party: isPartyRoute(route),
    words,
    wordsFormatting: formatting,
  });
  if (reads === "words") {
    return undefined;
  }
  const reference = buildDispatchReference(route);
  references.addresses.push(reference);
  if (naming.readsNone === true && reads !== "mention") {
    return href.startsWith("dispatch://") ? shortForm(route) : undefined;
  }
  const answer = naming.names.get(reference);
  const name = reads === "mention" ? answer?.mention : standingName(route, answer);
  if (name !== undefined) references.named.push(reference);
  return name;
}

/** The link mark over a text node, if any. */
export function linkMarkOf(node: ProseMirrorNode): Mark | undefined {
  return node.isText ? node.marks.find((mark) => mark.type.name === "link") : undefined;
}

/** Whether `parent`'s child at `index` is a hard break inside the link `link` marks: the text
 *  after it carries the same link, which is how a parsed text holds a link whose words a hard
 *  break cuts - the break itself holds no mark (`@legion/proof-editor`'s `withoutLeafMarks`). */
function breaksLink(parent: ProseMirrorNode, index: number, link: Mark): boolean {
  const next = parent.maybeChild(index + 1);
  return (
    parent.child(index).type.name === "hardbreak" &&
    next !== null &&
    next.isText &&
    link.isInSet(next.marks)
  );
}

/** The words of the link `link` marks, from `parent`'s child `index` on - the run of text nodes
 *  formatting inside the link cuts its words into, and the hard breaks between them, each a
 *  space - and how they are formatted, by the marks over each of those nodes
 *  (`@legion/contracts`'s `wordsFormatting`, a hard break being `HARD_BREAK_PIECE`). */
export function linkWordsAt(
  parent: ProseMirrorNode,
  index: number,
  link: Mark
): { words: string; formatting: WordsFormatting } {
  let words = "";
  const pieces: WordsPiece[] = [];
  for (let at = index; at < parent.childCount; at += 1) {
    const child = parent.child(at);
    if (child.isText && link.isInSet(child.marks)) {
      words += child.text ?? "";
      pieces.push(wordsPieceUnder(child.marks));
    } else if (breaksLink(parent, at, link)) {
      words += " ";
      pieces.push(HARD_BREAK_PIECE);
    } else {
      break;
    }
  }
  return { formatting: wordsFormatting(pieces), words };
}

/** The words a parsed document renders, line by line, each line with the references it read:
 *  text as it reads (references named, `nameReferences`), a link as it reads (`linkDrawn`), raw
 *  inline HTML as the literal characters a body shows for it, and every leaf but a hard break a
 *  space. A line ends at every block boundary, at a hard break, and at a newline in a text or an
 *  HTML value - which is where a code block's lines end, and a block of HTML's, which the engine
 *  draws as one `html` node holding all of its lines. A table's row is the exception: it reads as
 *  one line, every boundary or break inside it a space, so its cells read in a row. An item of an
 *  ordered list opens on its number, as a rendered body draws it: the list's `order` and the
 *  item's place in it, written before the first words the item holds, wherever they stand, so
 *  `1. Stand the harness up.` keeps its `1.` and `1. - a` reads `1. a`, never a line of `1.`
 *  alone. A link's words name no reference of their own, as a rendered body links none there,
 *  and a link a hard break cuts is read once, as the one link it is (`linkWordsAt`). */
function plainLines(doc: ProseMirrorNode, naming: ReferenceNaming): ProjectedText[] {
  const lines: ProjectedText[] = [];
  let words = "";
  let references: LineReferences = { addresses: [], named: [] };
  let questions = false;
  // Where the table row being read ends: a node that starts before it is one of its cells.
  let rowEnd = -1;
  // The link being read, and whether a name was written in place of its words: the text nodes
  // after its first, which formatting inside the link cuts its words into, then add nothing.
  let reading: { link: Mark; drawn: boolean } | undefined;
  // The numbers of the ordered list's items opened and not written yet, an item's after the
  // items it stands in, and where the innermost of those items ends: written before the first
  // words it holds, and dropped where it ends holding none.
  let numbers: { readonly end: number; readonly text: string } | undefined;
  const breakLine = (position: number): void => {
    if (position < rowEnd) {
      words += " ";
      return;
    }
    lines.push({ ...references, questions, words });
    words = "";
    references = { addresses: [], named: [] };
    questions = false;
  };
  // A reference never spans a newline (`@legion/contracts`' `reference-spans.ts`), so each line
  // of a text is named on its own.
  const write = (position: number, text: string, name: boolean): void => {
    for (const [index, line] of text.split("\n").entries()) {
      if (index > 0) {
        breakLine(position);
      }
      if (numbers !== undefined && (position >= numbers.end || line.trim() !== "")) {
        words += position < numbers.end ? numbers.text : "";
        numbers = undefined;
      }
      if (!name) {
        words += line;
        continue;
      }
      const named = nameReferences(line, naming, references);
      words += named.words;
      questions ||= named.questions;
    }
  };
  doc.descendants((node, position, parent, index) => {
    const link = linkMarkOf(node);
    if (reading !== undefined && link?.eq(reading.link) === true) {
      if (!reading.drawn) {
        write(position, node.text ?? "", false);
      }
      return true;
    }
    if (reading !== undefined && parent !== null && breaksLink(parent, index, reading.link)) {
      // A link whose words a hard break cuts reads as its words, so the break is the line break
      // it is everywhere else.
      breakLine(position);
      return true;
    }
    reading = undefined;
    if (node.isText) {
      const text = node.text ?? "";
      const href = link?.attrs.href;
      if (link !== undefined && parent !== null) {
        const written = linkWordsAt(parent, index, link);
        const drawn =
          typeof href === "string"
            ? linkDrawn(href, written.words, written.formatting, naming, references)
            : undefined;
        reading = { drawn: drawn !== undefined, link };
        write(position, drawn ?? text, false);
        return true;
      }
      const code =
        parent?.type.spec.code === true || node.marks.some((mark) => mark.type.spec.code === true);
      write(position, text, !code);
    } else if (node.type.name === "html") {
      write(position, String(node.attrs.value ?? ""), false);
    } else if (node.isBlock || node.type.name === "hardbreak") {
      breakLine(position);
      if (node.type.name === "table_row" || node.type.name === "table_header_row") {
        rowEnd = position + node.nodeSize;
      }
      if (node.type.name === "list_item" && parent?.type.name === "ordered_list") {
        const order = parent.attrs.order;
        const number = `${(typeof order === "number" ? order : 1) + index}. `;
        const within = numbers !== undefined && position < numbers.end ? numbers.text : "";
        numbers = { end: position + node.nodeSize, text: `${within}${number}` };
      }
    } else if (node.isLeaf) {
      words += " ";
    }
    return true;
  });
  breakLine(doc.content.size);
  return lines;
}

/** The words a parsed document renders, on one line: its lines (`plainLines`) a space apart. */
function plainWords(doc: ProseMirrorNode, naming: ReferenceNaming): ProjectedText {
  const lines = plainLines(doc, naming);
  return {
    addresses: lines.flatMap((line) => line.addresses),
    named: lines.flatMap((line) => line.named),
    questions: lines.some((line) => line.questions),
    words: lines.map((line) => line.words).join(" "),
  };
}

/** The first of a parsed document's lines (`plainLines`) that holds any words, with the
 *  references read on it alone; nothing where no line holds words. A line holding nothing but
 *  an image or a break holds no words, since the projection draws neither, so the next line is
 *  read; a line of HTML holds its literal characters, as the projection draws them, so a block
 *  of HTML reads as its first line. */
function firstLineOf(doc: ProseMirrorNode, naming: ReferenceNaming): ProjectedText {
  return (
    plainLines(doc, naming).find((line) => line.words.trim() !== "") ?? {
      addresses: [],
      named: [],
      questions: false,
      words: "",
    }
  );
}

/** A headline, whether it had to be cut to fit, and the references it read. */
export interface MarkdownHeadline {
  readonly text: string;
  readonly cut: boolean;
  /** The `dispatch://` reference of every reference the projection read, in the order it read
   *  them: a caller that resolves their names (`usePlainHeadline`) hands them back in `naming`,
   *  and one that does not ignores them. */
  readonly addresses: readonly string[];
  /** Those of `addresses` the projection wrote a name for in place of the words their author
   *  wrote (`naming`), in the order it wrote them: for a caller that draws a reference only once
   *  it has a name, as a chip does. */
  readonly named: readonly string[];
  /** Whether the projected text holds a `#id`, which only its document's question map names. */
  readonly questions: boolean;
}

/**
 * A stored text projected to one line of plain words and cut to `max` characters, for a place
 * that can hold only a string: a reference link's text, a card's title, a hover tooltip, an
 * `aria-label`. The words are `tree`'s, drawn from the reading Go made of that text, so nothing
 * on this page parses markdown for a stored text. Formatting goes (a link's own styling is the
 * formatting there) and no syntax stays: `**Blocking:** the deploy` reads `Blocking: the
 * deploy`, a code span keeps its words, every reference reads as a rendered body draws it, by
 * the facts `naming` holds (`nameReferences`, `linkDrawn`), and the cut happens on the projected
 * text, so it never splits a mark (nor a character outside the Basic Multilingual Plane).
 * Blocks and hard breaks become single spaces. `cut` says whether the text was cut, which an
 * ellipsis the author typed cannot fake.
 *
 * `markdown` is for a text that carries no tree to take the words from. That is the three texts
 * Dispatch does not store — a refused broadcast send's row names its own text on hover
 * (`BroadcastSends.tsx`) — and it is also a stored text whose answer carries no tree: a caller
 * that did not ask for one, or a server that has not rolled, which a deploy serving two builds
 * puts in front of one dashboard (`reference-target.ts`). It is read only where `tree` is
 * absent, and it is parsed, as everything about such a text is.
 *
 * `naming` is what a caller has resolved for the references the last projection answered; a
 * caller that leaves it out reads every reference as written. A title drawn over one record's
 * text reads none of them, since a name inside a name resolves nothing, and says so
 * (`ReferenceNaming.readsNone`, `useReferenceTarget`): a `dispatch://` reference reads there as
 * its short form, and every other one as written.
 *
 * `firstLine` projects the text's first line alone (`firstLineOf`): the first line of the
 * projection that holds any words - of a paragraph, a heading, a list item, a code block, a
 * table's whole row, or a block of HTML, which reads as its own first line's literal characters
 * (`<details>`) - the lines before it that hold none - an image alone, a break - passed over, for
 * a place that shows the opening line of a text and no more of it. A text read from `markdown`
 * is parsed with its soft breaks kept as line breaks for this reading, so a paragraph's first
 * line is the line its author wrote; a tree's soft break is whatever Go's reading made of it, a
 * space, so only a hard break ends that line. Only a closed row of the priorities table asks for
 * it today (`priorities-columns.tsx`); every other caller reads the whole text, which is the
 * default. `cut` still says only whether the projected words were cut to `max`, and `addresses`
 * holds only the line's own.
 *
 * `undefined` where neither is given, until the engine has answered (one pre-paint commit once
 * it is loaded on the page, and for a parsed body until the worker's answer lands), and for a
 * tree the schema refuses or an engine that cannot load: the surfaces that show a headline
 * show their own words in its place.
 */
export function useMarkdownHeadline(
  tree: DisplayTree | undefined,
  max: number,
  markdown?: string,
  naming: ReferenceNaming = NO_REFERENCE_NAMING,
  firstLine = false
): MarkdownHeadline | undefined {
  const [headline, setHeadline] = useState<MarkdownHeadline>();
  useLayoutEffect(() => {
    const fit = (document: ProseMirrorNode | undefined): void => {
      const next =
        document === undefined ? undefined : headlineOf(document, max, naming, firstLine);
      // A projection again over facts that change nothing of the line - a name read for a
      // reference it does not hold, a lookup made anew - keeps the headline in hand, so its
      // caller draws nothing again.
      setHeadline((current) => (sameHeadline(current, next) ? current : next));
    };
    if (tree !== undefined) {
      return drawWithEngine(tree, fit);
    }
    if (markdown === undefined) {
      setHeadline(undefined);
      return;
    }
    return renderWithEngine(
      markdown,
      undefined,
      firstLine ? "line" : "space",
      ({ parsed, pending }) => {
        // A long body's headline waits for the worker rather than reading as its own source,
        // and the headline left in place would be the one a frame before it produced.
        fit(pending ? undefined : parsed);
      }
    );
  }, [firstLine, markdown, max, naming, tree]);
  return headline;
}

/** Whether two lists of references are the same, in the same order. */
function sameReferences(one: readonly string[], other: readonly string[]): boolean {
  return one.length === other.length && one.every((reference, at) => other[at] === reference);
}

/** Whether two headlines read the same: the same words, cut alike, from the same references,
 *  named alike. */
function sameHeadline(
  one: MarkdownHeadline | undefined,
  other: MarkdownHeadline | undefined
): boolean {
  if (one === undefined || other === undefined) return one === other;
  return (
    one.text === other.text &&
    one.cut === other.cut &&
    one.questions === other.questions &&
    sameReferences(one.addresses, other.addresses) &&
    sameReferences(one.named, other.named)
  );
}

/** The headline of a parsed document (`useMarkdownHeadline`). The line is read from the parsed
 *  document, never from the markdown, by the projection that reads a whole text, so the two
 *  cannot disagree on what a line holds. */
function headlineOf(
  document: ProseMirrorNode,
  max: number,
  naming: ReferenceNaming,
  firstLine: boolean
): MarkdownHeadline {
  const projected = firstLine ? firstLineOf(document, naming) : plainWords(document, naming);
  const characters = Array.from(projected.words.replace(/\s+/g, " ").trim());
  const cut = characters.length > max;
  return {
    addresses: projected.addresses,
    named: projected.named,
    questions: projected.questions,
    cut,
    text: cut ? `${characters.slice(0, max).join("")}…` : characters.join(""),
  };
}
