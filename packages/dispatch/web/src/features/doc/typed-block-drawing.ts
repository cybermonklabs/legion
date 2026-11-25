import { askOpenerLines, askOpeners, parseBlockPointerValue } from "@legion/contracts";
import type { DOMOutputSpec, Node as ProseMirrorNode } from "prosemirror-model";

import type { Ask, AskOption, AskUrgency, BlockTypeSchema, TextAsk } from "../../api/types";
import {
  askBlockTint,
  askUrgencyAccent,
  borderDefault,
  textMutedOnSurfaceMuted,
  textPrimaryOnSurface,
} from "../../theme/classes";
import { URGENCY_LABELS } from "../inbox/ask-urgency";
import { buildReferencePath, parseNodeReference, shortForm } from "../refs/routes";

/**
 * `blockId` is the block's own identity rather than one of its schema attributes, so it is the
 * one name this function knows. Everything else it hides comes from the block type's own
 * schema: a hand-written list is a copy of that flag that goes stale the moment a type gains a
 * server-owned attribute (LEGION-67).
 */
const BLOCK_IDENTITY_ATTRIBUTE = "blockId";

function attributeValue(value: unknown): string {
  return Array.isArray(value) ? JSON.stringify(value) : String(value);
}

/**
 * One block a pointer names, as a link to it: the reference's short form as its words, the app
 * path as its `href`, and the reference itself as `data-dispatch-href`, which is how a link
 * drawn inside a document reaches the router and the hover card (`ProofDocument`'s click
 * handler and `RefPreview`'s trigger, the same pair `pictureView` writes for). A pointer that
 * names nothing is the text it is.
 */
function pointerLink(reference: string): DOMOutputSpec {
  const route = parseNodeReference(reference);
  if (route === undefined) {
    return ["span", {}, reference];
  }
  return [
    "a",
    { "data-dispatch-href": reference, href: buildReferencePath(route) },
    shortForm(route),
  ];
}

/** One attribute the header draws: its name, and the children its value draws as - a spec per
 *  element, or the plain text a text node carries, which `DOMOutputSpec` itself excludes. */
interface DrawnAttribute {
  readonly children: readonly (DOMOutputSpec | string)[];
  readonly name: string;
}

/**
 * The attributes of `node` the header draws, in the node's own order: the author's own, each
 * with the children its value draws as. A `block[]` attribute draws the blocks it points at,
 * one link each, separated as its value separates them; one that points at no block draws
 * nothing at all, since an empty pointer list is not a fact a reader came for the way a check's
 * blank negative control is. Every other kind draws its value's text.
 */
function drawnAttributes(
  node: ProseMirrorNode,
  type?: Pick<BlockTypeSchema, "attributes"> | undefined
): DrawnAttribute[] {
  return Object.entries(node.attrs).flatMap(([name, value]) => {
    if (
      name === BLOCK_IDENTITY_ATTRIBUTE ||
      type?.attributes[name]?.server === true ||
      value === null ||
      value === undefined ||
      value === ""
    ) {
      return [];
    }
    const declared = type?.attributes[name];
    if (declared?.kind !== "block[]") {
      return [{ children: [attributeValue(value)], name }];
    }
    // The node's value is the array the editor's own parser made of the attribute as markdown
    // wrote it (`parseAttribute`, `@legion/proof-editor`), and markdown and HTML carry those
    // same references joined by exactly one space, so reading the joined value serves either
    // shape through the parser the editor, the document service and the Plan tab share, under
    // the item kinds the attribute declares, and accepts exactly the values a writer is allowed
    // to store. One it refuses, which no stored block holds, points at nothing.
    const pointers =
      parseBlockPointerValue(
        Array.isArray(value) ? value.join(" ") : String(value),
        declared.items
      ) ?? [];
    return pointers.length === 0
      ? []
      : [
          {
            children: pointers.flatMap((reference, index) =>
              index === 0 ? [pointerLink(reference)] : [" ", pointerLink(reference)]
            ),
            name,
          },
        ];
  });
}

/**
 * Draws a document's host-owned typed blocks other than `ask`, which `AskBlockView` owns: the
 * block's kind and the author's own attributes as a header the reader sees, above its content.
 * The header reads as a label rather than a dump: the block's name is an eyebrow, a `kind` is a
 * badge beside it, a `title` is the header's own text, and any other attribute the author wrote
 * follows as a quiet name/value pair, a `block[]` one as a link to each block it points at
 * (`drawnAttributes`). What the author did not write never appears - the block's
 * identity, and every attribute the type's schema marks `server`, which is where the parser's
 * complaint and a decision's recorded state live. `type` is that schema entry; without one
 * (a node whose type the schema no longer describes) only the identity is hidden, and a
 * `block[]` attribute of such a type reads as the value it is. In the editor
 * it is the node view's drawing (`typedBlockView`), never the node's `toDOM`: HTML of the
 * document - a copy, a drag, or the editor's plain-text paste, which renders the markdown it
 * parsed through `toDOM` - carries only the block's section and content, which is all its parse
 * rule reads. In the read-only text engine (a comment, ask or message body) it is the schema's
 * `toDOM`, through the headless engine's `renderBlock` option; the parse rule still reads only
 * the content hole `[data-proof-block-content]`, so a paste of a body stays exact.
 */
export function renderTypedBlock(
  node: ProseMirrorNode,
  type?: Pick<BlockTypeSchema, "attributes"> | undefined
): DOMOutputSpec {
  const shown = drawnAttributes(node, type);
  const title = shown.find((attribute) => attribute.name === "title");
  const kind = shown.find((attribute) => attribute.name === "kind");
  const rest = shown.filter((attribute) => attribute.name !== "title" && attribute.name !== "kind");
  const badge: DOMOutputSpec[] =
    kind === undefined
      ? []
      : [
          [
            "span",
            { "data-proof-block-attribute": "kind", "data-proof-block-badge": "" },
            ...kind.children,
          ],
        ];
  const heading: DOMOutputSpec[] =
    title === undefined
      ? []
      : [
          [
            "span",
            { "data-proof-block-attribute": "title", "data-proof-block-title": "" },
            ...title.children,
          ],
        ];
  const others: DOMOutputSpec[] =
    rest.length === 0
      ? []
      : [
          [
            "dl",
            { "data-proof-block-attributes": "" },
            ...rest.flatMap(({ children, name }) => [
              ["dt", {}, name],
              ["dd", { "data-proof-block-attribute": name }, ...children],
            ]),
          ],
        ];
  return [
    "section",
    {
      class: `proof-typed-block proof-typed-block-${node.type.name}`,
      "data-proof-block-type": node.type.name,
    },
    [
      "header",
      { contenteditable: "false", "data-proof-block-summary": "" },
      ["span", { "data-proof-block-name": "" }, node.type.name],
      ...badge,
      ...heading,
      ...others,
    ],
    ["div", { "data-proof-block-content": "" }, 0],
  ];
}

/** What an `ask` node says about itself, read once per render from its attributes and content. */
export interface AskBlockFacts {
  readonly answerText: string;
  readonly answeredAt: string | undefined;
  readonly answeredBy: string;
  readonly blockId: string;
  /** The ask on another document this block is a copy of and that document's `dispatch://`
   *  address, which settlement names in `copied_from` and `copied_from_document`; the copy has no
   *  ask of its own and is answered on that document. */
  readonly copiedFrom: { readonly ask: string; readonly document: string } | undefined;
  /** Why the block cannot be answered as written, or undefined when it parses. */
  readonly malformedReason: string | undefined;
  readonly multiple: boolean;
  readonly options: AskOption[];
  readonly question: string;
  readonly selected: string[];
  readonly state: string;
  readonly urgency: AskUrgency;
}

/**
 * The ask the server reads for a block, keyed by the block's id. Mostly its row (`ask`): the
 * block's own ask on its document, or, for a copy of an ask on another document of the same owner
 * (the same block id, asking the same), that source ask and the `dispatch://` address of the
 * document it is on (`copiedFrom`), from `GET /api/v1/issues/{key}/asks` or a project document's
 * asks (`servedAskOf`). Otherwise what the document's live text answers for the block in its
 * `asks` member, or, from a Dispatch of the release before, which sends none, renders on the
 * block's opening line (`rendered`, `liveAskOf`): a copy on a project document reads its source
 * there, since no route lists the asks of a project's other documents. A person's answer is held
 * on the ask alone, so a block reads its state and answer from here.
 */
export type ServedAsk =
  | { readonly ask: Ask; readonly copiedFrom?: string }
  | { readonly rendered: AnswerFacts };

/**
 * What `node`, an ask, says about itself: the question its first child holds and the options the
 * list its own content ends with holds, passing over the blocks it closes with (a reading, whose
 * schema `parents` name `ask`), which are no part of what it asks. Its state, its answer, who gave
 * it and when, and a copy's source are `served`'s, the server's read of the ask; without one - a
 * block in a text body no server reads, or a version that holds them - they are what the block's
 * attributes record.
 */
export function askBlockFacts(node: ProseMirrorNode, served?: ServedAsk): AskBlockFacts {
  const options: AskOption[] = [];
  let blankOption: number | undefined;
  const own = node.children.filter(
    (child) => (child.type.spec.parents as readonly string[] | undefined)?.includes("ask") !== true
  );
  const list = own.at(-1);
  if (list?.type.name === "bullet_list") {
    list.forEach((item, _offset, index) => {
      const text = item.textContent;
      const separator = text.indexOf(": ");
      const label = (separator < 0 ? text : text.slice(0, separator)).trim();
      const description = separator < 0 ? "" : text.slice(separator + 2).trim();
      if (label === "") {
        blankOption ??= index + 1;
        return;
      }
      options.push(description === "" ? { label } : { description, label });
    });
  }
  const invalid =
    typeof node.attrs.invalid === "string" && node.attrs.invalid.trim() !== ""
      ? node.attrs.invalid.trim()
      : undefined;
  const question =
    node.firstChild?.type.name === "bullet_list" ? "" : (node.firstChild?.textContent.trim() ?? "");
  const recorded =
    served === undefined
      ? recordedAnswer(node.attrs)
      : "rendered" in served
        ? served.rendered
        : servedAnswer(served);
  return {
    ...recorded,
    blockId: String(node.attrs.blockId),
    malformedReason:
      invalid ??
      (question === ""
        ? "The question is empty"
        : blankOption === undefined
          ? undefined
          : `Option ${blankOption} has no label`),
    multiple: node.attrs.multiple === true,
    options,
    question,
    urgency: node.attrs.urgency in URGENCY_LABELS ? (node.attrs.urgency as AskUrgency) : "med",
  };
}

/** The part of a block's facts that records its answer. */
export type AnswerFacts = Pick<
  AskBlockFacts,
  "answerText" | "answeredAt" | "answeredBy" | "copiedFrom" | "selected" | "state"
>;

/** The answer `served`, the ask's row, reads off the ask. */
function servedAnswer({ ask, copiedFrom }: Extract<ServedAsk, { ask: Ask }>): AnswerFacts {
  const answer = ask.state === "answered" ? ask.answer : null;
  return {
    answerText: answer?.text?.trim() ?? "",
    answeredAt: answer?.at,
    answeredBy: answer?.user ?? "",
    copiedFrom: copiedFrom === undefined ? undefined : { ask: ask.id, document: copiedFrom },
    selected: answer?.selected ?? [],
    state: ask.state,
  };
}

/** The answer a block's attributes record, as a version written before the answer left the
 *  document holds it, or as the live text of a Dispatch of the release before renders it
 *  (`renderedAskOf`). A block that records no state is open: the server writes an ask's state into
 *  a block or nobody does, and the editor's schema gives the attribute no default, so a block it
 *  was never written into, or that settlement took it off, holds none. */
function recordedAnswer(attrs: Readonly<Record<string, unknown>>): AnswerFacts {
  return {
    answerText: typeof attrs.answer === "string" ? attrs.answer.trim() : "",
    answeredAt: typeof attrs.answered_at === "string" ? attrs.answered_at : undefined,
    answeredBy: String(attrs.answered_by ?? ""),
    copiedFrom:
      typeof attrs.copied_from === "string" &&
      attrs.copied_from !== "" &&
      typeof attrs.copied_from_document === "string" &&
      attrs.copied_from_document !== ""
        ? { ask: attrs.copied_from, document: attrs.copied_from_document }
        : undefined,
    selected: Array.isArray(attrs.selected)
      ? attrs.selected.filter((value): value is string => typeof value === "string")
      : [],
    state: typeof attrs.state === "string" ? attrs.state : "open",
  };
}

/**
 * The ask the server reads for the block `blockId` of the document `artifactId`, among `asks`, the
 * asks of its owner: the block's own ask, or the ask on another document of the owner it is a copy
 * of, which carries the same block id, with that document's `dispatch://` address. `project` is
 * the owner's project where it is a project, whose documents' asks name no issue.
 */
export function servedAskOf(
  asks: readonly Ask[],
  artifactId: string,
  blockId: string,
  project?: string
): ServedAsk | undefined {
  const own = asks.find(
    (candidate) => candidate.block_id === blockId && candidate.block_artifact?.id === artifactId
  );
  if (own !== undefined) {
    return { ask: own };
  }
  const source = asks.find(
    (candidate) =>
      candidate.block_id === blockId &&
      candidate.block_artifact !== undefined &&
      candidate.block_artifact.id !== artifactId
  );
  const document = source?.block_artifact;
  if (source === undefined || document === undefined) {
    return undefined;
  }
  const place = document.primary ? "spec" : `artifact/${encodeURIComponent(document.slug)}`;
  const owner = source.issue_key ?? project;
  return owner === undefined || owner === null
    ? { ask: source }
    : { ask: source, copiedFrom: `dispatch://${owner}/${place}` };
}

/** The character references the renderer writes in a directive attribute's value
 *  (pmdoc's escapeDirectiveAttribute), each with the character it stands for. */
// kept for one release: release-before-text
const ATTRIBUTE_REFERENCES: Readonly<Record<string, string>> = {
  "&#x22;": '"',
  "&#xA;": "\n",
  "&#xD;": "\r",
  "&amp;": "&",
  "&gt;": ">",
  "&lt;": "<",
};

/**
 * The ask the block `blockId` shows, as `markdown`, a document's live text (`GET
 * /api/v1/artifacts/{id}/text`) from a Dispatch of the release before, renders it on the block's
 * opening line: that server writes there, from the store, the state, answer, answerer and time of
 * the block's own ask, or of the ask on another document of the owner the block is a copy of,
 * with that ask's id and document (`copied_from`, `copied_from_document`). A Dispatch of this
 * release writes none of them into the text and answers them in its `asks` (`liveAskOf`). Only a
 * real opener of the block counts, as the CLI reads one (`askOpenerLines`, `askOpeners`): a line
 * that quotes the opener in fenced code or in front matter is the document's own text. Undefined
 * where the text renders the block no state, or holds no real opener of it, or more than one.
 */
// kept for one release: release-before-text
export function renderedAskOf(
  markdown: string | undefined,
  blockId: string
): ServedAsk | undefined {
  const openers = markdown === undefined ? [] : askOpeners(askOpenerLines(markdown), blockId);
  if (openers.length !== 1) {
    return undefined;
  }
  const attributes: Record<string, unknown> = {};
  for (const [, name, value] of openers[0].matchAll(/([\w-]+)="([^"]*)"/g)) {
    attributes[name] = value.replace(
      /&(?:#x22|#xA|#xD|amp|gt|lt);/g,
      (reference) => ATTRIBUTE_REFERENCES[reference] ?? reference
    );
  }
  // A list of strings is written as its JSON (pmdoc's encodeTypedAttribute).
  if (typeof attributes.selected === "string") {
    try {
      attributes.selected = JSON.parse(attributes.selected);
    } catch {
      attributes.selected = undefined;
    }
  }
  return attributes.state === undefined ? undefined : { rendered: recordedAnswer(attributes) };
}

/**
 * The ask the block `blockId` shows in `text`, a document's live text (`GET
 * /api/v1/artifacts/{id}/text`): the entry of its `asks` member for the block, which the server
 * reads from the block's own ask, or from the ask on another document of the owner the block is a
 * copy of, with that ask's id and document. It is where a copy on a project document reads its
 * source, which no read the page makes lists. A Dispatch of the release before sends no `asks`
 * and renders the same on the block's opening line, which is read instead (`renderedAskOf`).
 * Undefined where the text has no entry for the block with a state - a block no ask stands
 * behind yet - or more than one entry for it.
 */
export function liveAskOf(
  text: { readonly markdown: string; readonly asks?: readonly TextAsk[] | null } | undefined,
  blockId: string
): ServedAsk | undefined {
  // kept for one release: release-before-text
  if (text?.asks === undefined || text.asks === null) {
    return renderedAskOf(text?.markdown, blockId);
  }
  const entries = text.asks.filter((entry) => entry.block_id === blockId);
  const entry = entries.length === 1 ? entries[0] : undefined;
  if (entry?.state === undefined) {
    return undefined;
  }
  return {
    rendered: {
      answerText: entry.answer?.trim() ?? "",
      answeredAt: entry.answered_at,
      answeredBy: entry.answered_by ?? "",
      copiedFrom:
        entry.copied_from === undefined || entry.copied_from_document === undefined
          ? undefined
          : { ask: entry.copied_from, document: entry.copied_from_document },
      selected: entry.selected ?? [],
      state: entry.state,
    },
  };
}

/**
 * The server's read of the ask the block `blockId` of the document `artifactId` shows, as the
 * document page and the Inbox's reading pane both read it: its row among `asks`, the asks of the
 * document's owner (`servedAskOf`), else what `text`, the document's live text, answers for the
 * block (`liveAskOf`), which is the one read of a copy's source on a project document: the
 * document's own asks hold no row of another document's. `project` is the owner's project where
 * it is a project.
 */
export function servedAskOfBlock(
  asks: readonly Ask[],
  text: { readonly markdown: string; readonly asks?: readonly TextAsk[] | null } | undefined,
  artifactId: string,
  blockId: string,
  project?: string
): ServedAsk | undefined {
  return servedAskOf(asks, artifactId, blockId, project) ?? liveAskOf(text, blockId);
}

/**
 * Whether `attrs`, the attributes of an ask block of a stored version as the version's reading
 * holds them, record all the card draws of the ask's answer: a `state`, and for an answered ask
 * the options chosen or the words (`selected`, `answer`). They are the parsed block's own, so a
 * line elsewhere in the version that quotes the block's opener, in fenced code or in front
 * matter, changes nothing. A version the release before wrote holds them, and is read as it was
 * written. A version this build writes holds the state of an ask that is not open and, for an
 * answered ask, who answered and when, but never the options or the words (the server's
 * `keptAskState`); it holds no state for an open ask. The release before wrote an answered block
 * whose document had no room for the answer without them too. So a version that records no state,
 * or an answered state without the answer, is read from the server's read of the ask.
 */
export function recordsAnswerState(attrs: Readonly<Record<string, unknown>>): boolean {
  return (
    typeof attrs.state === "string" &&
    (attrs.state !== "answered" ||
      Array.isArray(attrs.selected) ||
      typeof attrs.answer === "string")
  );
}

/** A mounted decision block: the React card portals into its two slots. A new object is issued
 * on every node change so a host list in React state re-renders. */
export interface AskBlockHost {
  /** Stable per mounted view in the editor, for React keys; on the text path, the block's index
   * in the body. */
  readonly key: number;
  readonly node: ProseMirrorNode;
  readonly header: HTMLElement;
  readonly footer: HTMLElement;
}

/** The attributes every decision shell stamps on its frame, wherever it is drawn: its urgency,
 * its state, and `data-dispatch-ask-malformed`, `"true"` while the block is malformed and
 * `undefined` otherwise. The key is always present so one loop writes every stamp: a static spec
 * skips an undefined attribute (prosemirror-model's `renderSpec`), and `applyAskBlockShell`
 * removes it. The state stamped here is the block's own attribute; the card mounted into the
 * shell stamps the state it shows over it, the server's read of the ask where it holds one
 * (`AskBlockCard`), since the block no longer records an answer. */
type AskBlockStamps = {
  readonly "data-dispatch-ask-urgency": AskUrgency;
  readonly "data-dispatch-ask-state": string;
  readonly "data-dispatch-ask-malformed": "true" | undefined;
};

/** How a decision block's shell presents itself: the frame's classes (the card frame with its
 * urgency accent and tint), the content hole's classes, the attributes the frame stamps, and
 * whether a mounted card folds the source option list. */
interface AskBlockShell {
  readonly frameClass: string;
  readonly contentClass: string;
  readonly stamps: AskBlockStamps;
  /** Whether the frame takes `data-dispatch-ask-options="rows"` where a card is mounted into it:
   * the card's rows render the options, so the source list folds, except while the block is
   * malformed, when the list is what the reader fixes. */
  readonly foldsOptions: boolean;
}

function askBlockShell(facts: AskBlockFacts): AskBlockShell {
  const malformed = facts.malformedReason !== undefined;
  return {
    contentClass: malformed ? textMutedOnSurfaceMuted : textPrimaryOnSurface,
    foldsOptions: !malformed,
    frameClass: `proof-typed-block proof-typed-block-ask my-5 rounded-xl border border-l-4 px-4 pt-3 pb-4 shadow-sm ${borderDefault} ${askUrgencyAccent[facts.urgency]} ${askBlockTint[facts.urgency]}`,
    stamps: {
      "data-dispatch-ask-urgency": facts.urgency,
      "data-dispatch-ask-state": facts.state,
      "data-dispatch-ask-malformed": malformed ? "true" : undefined,
    },
  };
}

/**
 * The decision block's shell as a static spec: the frame, a header slot, the content hole, and a
 * footer slot. The editor's `AskBlockView` and the read-only text engine (a comment, ask or
 * message body) both draw this one spec, so the two shells cannot drift apart. The slots hold the
 * app's own card, not document text: `not-prose` keeps the surrounding Typography (`prose`, from
 * the editor's library theme or a body's `markdownClassName`) off everything React renders into
 * them, in every state of the card. The frame carries the shell's `stamps`. Three attributes are
 * absent on purpose. `data-dispatch-ask-options` folds the source option list, so
 * `applyAskBlockShell` adds it, as the shell's `foldsOptions` says, only where a card whose rows
 * render the options is mounted; without it the list stays visible. `AskBlockView` adds the two
 * identity attributes: `data-dispatch-ask-block` marks a live decision under the editor root, and
 * a body rendered inside a document's decision footer (a reply, an answer note) would otherwise
 * add a second decision id there; `data-block-id` is the schema's own wrapper's to add.
 */
export function renderAskBlockShell(node: ProseMirrorNode): DOMOutputSpec {
  const shell = askBlockShell(askBlockFacts(node));
  return [
    "section",
    { class: shell.frameClass, "data-proof-block-type": "ask", ...shell.stamps },
    ["div", { class: "not-prose", contenteditable: "false", "data-dispatch-ask-header": "" }],
    ["div", { class: shell.contentClass, "data-proof-block-content": "" }, 0],
    ["div", { class: "not-prose", contenteditable: "false", "data-dispatch-ask-footer": "" }],
  ];
}

/** A decision shell's own three slots: the header and footer the card portals into, and the
 * content hole. */
export interface AskShellSlots {
  readonly header: HTMLElement;
  readonly content: HTMLElement;
  readonly footer: HTMLElement;
}

/**
 * The slots of the decision shell `frame` (`renderAskBlockShell`): its own direct children
 * marked `data-dispatch-ask-header`, `data-proof-block-content` and `data-dispatch-ask-footer`,
 * never the slots of a decision nested in its content. It answers `undefined` when any is missing
 * rather than throwing, because the text path must show a body without a card rather than crash
 * its layout effect; the editor's node view throws on that answer itself.
 */
export function askShellSlots(frame: HTMLElement): AskShellSlots | undefined {
  const header = frame.querySelector<HTMLElement>(":scope > [data-dispatch-ask-header]");
  const content = frame.querySelector<HTMLElement>(":scope > [data-proof-block-content]");
  const footer = frame.querySelector<HTMLElement>(":scope > [data-dispatch-ask-footer]");
  if (header === null || content === null || footer === null) {
    return undefined;
  }
  return { content, footer, header };
}

/**
 * Writes `node`'s shell (`askBlockShell`) over the drawn decision shell `frame` and its content
 * hole `content`: the frame's class, every stamp (an undefined one removed), the fold, and the
 * content's class. The shell's class is this writer's. It is called only where a card is mounted
 * into the shell (the editor's node view always; the text path only under the `TextAskCard`
 * provider), so the static spec stays card-less, with no fold, and a body with no card keeps its
 * options as the source list. Where it folds, the card renders the options as rows; the source
 * list still shows while the block is malformed (no fold), or, in the editor, while the caret is
 * inside the block (`askBlockEditingPlugin`).
 */
export function applyAskBlockShell(
  frame: HTMLElement,
  content: HTMLElement,
  node: ProseMirrorNode
): void {
  const shell = askBlockShell(askBlockFacts(node));
  frame.className = shell.frameClass;
  for (const [name, value] of Object.entries(shell.stamps)) {
    if (value === undefined) {
      frame.removeAttribute(name);
    } else {
      frame.setAttribute(name, value);
    }
  }
  if (shell.foldsOptions) {
    frame.setAttribute("data-dispatch-ask-options", "rows");
  } else {
    frame.removeAttribute("data-dispatch-ask-options");
  }
  content.className = shell.contentClass;
}
