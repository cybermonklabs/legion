import { describe, expect, test } from "bun:test";
import { readdirSync, readFileSync } from "node:fs";
import { join } from "node:path";
import {
  DISPATCH_READING_SURFACES,
  DISPATCH_WRITING_BOXES,
  type DispatchReadingSurface,
  type DispatchReadingSurfaceRow,
  type DispatchWritingBoxRow,
} from "@legion/contracts";
import * as ts from "typescript";

import { referencesLaneFiles } from "../../../../e2e/reference-lane";

/**
 * The two lists say what the framework covers: `DISPATCH_WRITING_BOXES` every box a person
 * writes prose in, `DISPATCH_READING_SURFACES` every surface a reader reads a record on. Both
 * are hand-written, and a hand-written list goes stale the first time someone adds a box or a
 * surface without opening it - which compiles, passes every other test, and leaves the browser
 * rows that walk the lists covering one surface fewer than anyone thinks.
 *
 * This test is what stops that. It reads the dashboard's own source for the constructs a box or
 * a surface is made of. A box's construct fails when it is in a file no row names. A surface's
 * construct - a mount of the renderer, of a reference card or of the plain-text projection -
 * names its row itself, in its `surface`, and fails when it names none or a row that does not
 * name its file: a second mount in a file a row already names is a surface of its own until it
 * says which row it is. A mount on a row whose text is a record hands its construct that record
 * as its `source`, or its row says it cannot carry a question id; and a component that hands a
 * prop of its own on as that `source` (`AskHitStatus`'s `source`, `AskOptionList`'s) is handed
 * that prop wherever it is mounted, however many components hand it on, since a mount that
 * forwards a prop its caller left out hands the construct nothing. The test reads each row back
 * against the source, so a row whose box or surface is gone fails too, and a row drawn through
 * the projection names the test that proves it.
 *
 * A surface can also be drawn with no construct at all: a stored text written into a JSX child,
 * an attribute, a template literal, a `+` join or a helper's call as the characters it is, where
 * a mention reads as `[@name](dispatch://agent/...)`. The test finds those in the syntax tree
 * too, and fails on each one that no excluded row names by its id beside it and that the
 * reviewed list of fields holding no stored text (`NOT_STORED_TEXTS`) does not hold.
 *
 * The page parses no stored text on its own thread: the server sends the reading it made of each
 * one, and the renderer and the projection draw that tree. So every mount of either that is
 * handed a stored text hands it the text's tree too - the `tree` of an element, the first
 * argument of the hook - and so does every mount of a component that hands one a text and its
 * tree from its own props (`ReplyQuote`'s `excerpt` and `excerptTree`, `ReplyTurn`'s `quote`),
 * however many components hand it on. The one excuse is a row whose text reaches the page with
 * no tree (`UNTREED_ROWS`), named by its id: a live turn and its reasoning, a write the server
 * has not stored (a held message, and the draft a broadcast's send row draws until Dispatch has
 * made the broadcast), a GitHub body, and a suggestion's two sides, which
 * `margin/ThreadCard.tsx` - main's own mounts - draws inside a diff, where the renderer writes
 * characters and parses nothing. A row that names its text (`UntreedRow.text`) excuses that text
 * alone, so a made broadcast's row, drawn from the body Dispatch stored, is held to that body's
 * tree.
 *
 * The stored texts are the contract's: a field with a tree beside it (`<field>_tree`) is one.
 * A stored text with no such tree in the generated contract is named by hand (`HAND_NAMED_TEXTS`),
 * with where its tree is or that the server sends none: one whose tree rides elsewhere - an
 * event's under its payload path, a hand-written shape's, the dashboard's own `excerptTree` - is
 * held to a call or an element of the dashboard that hands it beside that tree, and every one to
 * staying a text the contract's trees do not name.
 *
 * What it cannot see, since it reads each file's syntax and not its types:
 *
 * - A stored text handed to a component of another file as a prop, and drawn raw there under
 *   the prop's name: the walk follows props only within one file.
 * - A stored text carried by a value that is no field read - state, a context, a ref, a store, a
 *   query's `select`, an argument of a callback - and a function of another module that answers
 *   one but is not imported by its own name.
 * - A field the dashboard names like its own words (`label`, `description`) on a value the
 *   trace does not see reached through `options`, and any field whose name no stored text has.
 * - What a spread (`{...props}`) hands a construct: a spread counts as handing it a source, and
 *   a tree.
 * - A stored text written into the page by the DOM itself (`textContent`, `setAttribute`,
 *   `document.title`) other than through a template literal.
 * - Which tree a mount hands: one is read as handed whatever text it is the tree of, and a
 *   field whose tree the server leaves out for some records is held only by the mount's own row
 *   of tests - a document candidate's first line is its file's name, which has none
 *   (`ItemMatchList.tsx`).
 * - A tree carried in a value built away from the mount - a reply target held in state, an
 *   object that is no literal where the mount writes it - which counts as handing one, and a
 *   function that is no component (a hook) handing its own parameter on, whose callers are not
 *   followed. A component is followed by its name, so two components of one name are one here.
 * - A host that draws characters (a link, a diff, a plain host), where the renderer parses
 *   nothing: a mount there is held to a tree like any other, or excused by its row.
 * - A record a component hands on that is not the one its mount's text is: a mount handing any
 *   `source` counts as handing the right one, and a hook handing its own option on as a
 *   `source` is not followed to its callers, as a component is.
 *
 * It walks `packages/proof-editor/src` beside the dashboard because the document body is one of
 * the three hosts that carry the picker: a box added inside the editor package must not be
 * invisible here.
 */

/** The repository root, from this file: `features/refs` -> `src` -> `web` -> `dispatch` ->
 *  `packages` -> the root. Every path below is written from there, which is how both lists
 *  write the files of a row. */
const REPO_ROOT = join(import.meta.dir, "..", "..", "..", "..", "..", "..");

/** What the walk reads. The editor package is here for the reason the header gives. */
const SCANNED_ROOTS = ["packages/dispatch/web/src", "packages/proof-editor/src"];

/** The shared field's own file. Its one `<textarea>` is the field itself - the thing every
 *  covered box mounts - rather than a box of its own, so the walk reads no box out of it. */
const SHARED_FIELD = "packages/dispatch/web/src/features/conversation/MentionField.tsx";

/** One construct of a box the walk looks for, and the words a failure calls it by. */
interface Construct {
  readonly name: string;
  readonly pattern: RegExp;
}

/** The two editors that host the picker in their own way: a document's body through
 *  `createProofEditor`, an agent's live conversation through assistant-ui's composer input. A box
 *  made of one picks its row beside the mount rather than on it (the document body's picker, the
 *  live conversation's triggers). The pattern skips `createProofEditor`'s own declaration in the
 *  editor package, which is no mount. */
const EDITOR_MOUNT: Construct = {
  name: "an editor mount",
  pattern: /(?<!function )\bcreateProofEditor\(|<ComposerPrimitive\.Input\b/,
};

/** The elements that mount the shared field, bare or through the composer that dresses it. A
 *  mount picks the box's row on its own attributes (`picksOf`). */
const SHARED_FIELD_ELEMENTS: readonly string[] = ["MentionField", "MentionComposer"];

/** What a box is made of: the plain textarea of a box that takes no picker, an editor that
 *  hosts the picker, and a mount of the shared field. */
const WRITING_CONSTRUCTS: readonly Construct[] = [
  { name: "a textarea element", pattern: /<textarea\b/ },
  EDITOR_MOUNT,
  {
    name: "a mount of the shared field",
    pattern: new RegExp(SHARED_FIELD_ELEMENTS.map((element) => `<${element}\\b`).join("|")),
  },
];

/** What a reading surface is made of, as the walk finds it in a file's syntax tree: a mount of
 *  the one renderer, as a body or as a line or two of one; a mount of a reference card - the
 *  cards under a body that is nothing but references, and the hover card the shell portals once
 *  for the whole app; and the renderer's plain-text projection, for a place that can hold no
 *  markup. Each names its row of the reading list in its `surface`: an element as an attribute,
 *  a hook in the options object that is its last argument. */
interface ReadingConstruct {
  /** The words a failure calls it by. */
  readonly name: string;
  /** The elements that mount it. */
  readonly elements: readonly string[];
  /** The hooks that mount it. */
  readonly hooks: readonly string[];
}

const READING_CONSTRUCTS: readonly ReadingConstruct[] = [
  // `PostBody` and `PostPreview` are the renderer for a comment's or an issue message's text,
  // which draw the deleted line in its place once its author deleted it (`refs/PostBody.tsx`,
  // with the marks in `refs/PostMarks.tsx`), and hand their caller's `surface` on to the
  // renderer they are made of.
  {
    elements: ["MarkdownBody", "MarkdownPreview", "PostBody", "PostPreview"],
    hooks: [],
    name: "a mount of the body or the preview renderer",
  },
  // `UnfurlRead` is the mount the renderer itself makes, with the readings it already has.
  {
    elements: ["Unfurl", "UnfurlRead", "RefPreviewHost"],
    hooks: [],
    name: "a mount of the reference card",
  },
  // `useNodeLine` is the new issue page's one line of a node, which hands its `surface` on to
  // the projection it is made of (`issue-page/node-line.tsx`).
  {
    elements: ["PlainText"],
    hooks: ["usePlainHeadline", "useNodeLine"],
    name: "a mount of the plain-text projection",
  },
];

const READING_ELEMENTS = new Map(
  READING_CONSTRUCTS.flatMap((construct) =>
    construct.elements.map((element) => [element, construct] as const)
  )
);

const READING_HOOKS = new Map(
  READING_CONSTRUCTS.flatMap((construct) =>
    construct.hooks.map((hook) => [hook, construct] as const)
  )
);

/** The attributes that hand a construct the text it draws: a body's or a preview's `markdown`,
 *  the text a body links (`referenceText`), and a card's `body`. A stored text there is drawn by
 *  the construct, so it is no raw draw. */
const CONSTRUCT_TEXT_ATTRIBUTES: readonly string[] = ["body", "markdown", "referenceText"];

/** The constructs that read a `#id` against the record their text is, and so take that record
 *  as their `source`. The hover card and the cards the renderer mounts with the readings it has
 *  already made read their records themselves, so they take none. */
const SOURCE_TAKERS: ReadonlySet<string> = new Set([
  "MarkdownBody",
  "MarkdownPreview",
  "PlainText",
  "PostBody",
  "PostPreview",
  "Unfurl",
  "usePlainHeadline",
]);

/** The attributes whose value the browser shows a person as the characters it is. */
const PLAIN_ATTRIBUTES: readonly string[] = ["alt", "aria-label", "label", "placeholder", "title"];

/** The attributes of an element of the page that show nothing of their value as words: a class
 *  list, an element's handles, an address to open, and a box's own draft, which the writing list
 *  holds (`DISPATCH_WRITING_BOXES`). A stored text in any other attribute of an element is read
 *  as drawn. A component's props are not attributes of the page: what one draws of them is read
 *  where it draws it. */
const UNDRAWN_ATTRIBUTES: readonly string[] = [
  "className",
  "defaultValue",
  "href",
  "htmlFor",
  "id",
  "key",
  "ref",
  "style",
  "to",
  "value",
];

/** Where the walk looks for a stored text drawn around the renderer: the dashboard's own source,
 *  since the editor package draws no record of Dispatch's. */
const RAW_DRAW_ROOT = "packages/dispatch/web/src";

/** The reader itself: the modules that turn a stored text into what the renderer and the
 *  projection draw. Whatever they write of a text is the reading of it - the words a reference
 *  is drawn as, the line a body is cut to - so it is no raw draw of the text. */
const READER_MODULES: readonly string[] = [
  "packages/dispatch/web/src/features/refs/MarkdownBody.tsx",
  "packages/dispatch/web/src/features/refs/MarkdownPreview.tsx",
  "packages/dispatch/web/src/features/refs/markdown-engine.ts",
  "packages/dispatch/web/src/features/refs/plain-text.tsx",
  "packages/dispatch/web/src/features/refs/tree-fragment.ts",
  // The cut of a head (a row's label, an excerpt's line): the twin of the server's HeadRefs.
  "packages/dispatch/web/src/features/refs/head-refs.ts",
];

/** The API's generated contract, whose trees name the stored texts (`TREED_FIELDS`). */
const API_CONTRACT = "packages/contracts/src/dispatch-api.generated.ts";

/** A stored text the contract's scan cannot name (`TREED_FIELDS`), since no `<field>_tree` stands
 *  beside it in the generated contract: a writer's own words, where a mention reads as
 *  `[@name](address)` until something reads it. The raw-draw walk reads each as the stored text
 *  it is (`STORED_PROSE_FIELDS`). */
interface HandNamedText {
  /** The field the text stands in. */
  readonly field: string;
  /** What the text is, and where its tree is, or that the server sends none. */
  readonly what: string;
  /** Where the dashboard draws the text with the tree the server sends of it: the file, and the
   *  tree as that file writes it beside the text, in one call or element (`handsWithTree`).
   *  Absent for a text no read the dashboard draws carries a tree of. */
  readonly tree?: { readonly file: string; readonly written: string };
}

/** The stored texts the contract's scan cannot name, each held below to what it says of its
 *  tree: one with a tree to the place the dashboard draws it with that tree, and every one to
 *  staying out of the scan's reach, so the list names no text the contract already trees. */
const HAND_NAMED_TEXTS: readonly HandNamedText[] = [
  {
    field: "ask_question",
    what: "The question a reply answers, as the reply's own event carries it. Its tree rides in the event's `trees` under its payload path, on every event list the page reads and on every live frame (`event_trees.go`), and the event line reads it from there (`eventProse`).",
    tree: {
      file: "packages/dispatch/web/src/features/activity/activity-model.ts",
      written: "payload.ask_question",
    },
  },
  {
    field: "excerpt",
    what: "A quoted parent's body, under the dashboard's own name for it (`ReplyQuote`), which carries its tree beside it as `excerptTree`. A backlink's excerpt is an object whose `text` the contract trees (`GraphExcerptWithTrees`); the `excerpt` string of an artifact's references read (`ReferencedBy`) has no tree, and no page of the dashboard reads it.",
    tree: {
      file: "packages/dispatch/web/src/features/conversation/ReplyTurn.tsx",
      written: "excerptTree",
    },
  },
  {
    field: "heading",
    what: "A decision's heading: an ask's question, else the words of the first block of the decision's own content that holds any. The server sends no tree beside it; its tree is that block of the decision's `text_tree`, or the `text_tree` of an ask's entry, which holds the question (`decisionHeadingTree`).",
    tree: {
      file: "packages/dispatch/web/src/features/issue/DecisionsPanel.tsx",
      written: "headingTree",
    },
  },
  {
    field: "negative_control",
    what: "A check's negative control, and a check result's: what its writer says would fail. The server sends no tree of it.",
  },
  {
    field: "quote",
    what: "An anchor's quote: the document's own text where a comment or an ask was left. The server sends no tree of it.",
  },
  {
    field: "summary",
    what: "A document version's summary: the line its writer gave the version. The server sends no tree of it.",
  },
];

/** A reader this change folded into the shared one. Nothing may import it back: a second reader
 *  of a stored text is the drift both lists exist to stop. */
interface DeletedReader {
  /** What it read, for the failure to say. */
  readonly what: string;
  /** The module it lived in, as an importer would write the end of the specifier. */
  readonly module?: string;
  /** The names it exported, each a word no source file writes now. */
  readonly names: readonly string[];
}

const DELETED_READERS: readonly DeletedReader[] = [
  {
    module: "inbox/question-line",
    names: ["questionLine"],
    what: "the second reader of an ask's question line",
  },
  {
    names: ["composerReferences", "ComposerReference"],
    what: "the composer's own reader of a draft's references",
  },
  { module: "refs/ReferencePicker", names: [], what: "the Ctrl+K reference dialog" },
];

// ------------------------------------------------------------------------------------------
// The walk
// ------------------------------------------------------------------------------------------

function collectSourceFiles(root: string): string[] {
  const files: string[] = [];
  const walk = (relative: string): void => {
    for (const entry of readdirSync(join(REPO_ROOT, relative), { withFileTypes: true })) {
      const child = `${relative}/${entry.name}`;
      if (entry.isDirectory()) {
        // The suite's own harnesses stand in for the shell - one of them mounts the hover card
        // as `app.tsx` does - so they are the suite rather than a surface a reader reaches.
        if (entry.name !== "__tests__") {
          walk(child);
        }
        continue;
      }
      const isSource = entry.name.endsWith(".ts") || entry.name.endsWith(".tsx");
      // A test file is the suite, and so is a draft matrix's coverage list (`*.coverage.ts`):
      // test-support data no product module imports, whose strings name the mounts the composer
      // census finds (`<MentionComposer mutationKey={...}>`) rather than mounting anything. So is
      // a fixture module (`*-fixture.ts`, `*-fixtures.ts`), which only the suite imports: one
      // reads a node's line as a row draws it (`issue-page/projection-fixture.tsx`), mounting
      // the line's projection for no reader.
      const isTest =
        entry.name.endsWith(".test.ts") ||
        entry.name.endsWith(".test.tsx") ||
        entry.name.endsWith(".coverage.ts") ||
        /-fixtures?\.tsx?$/u.test(entry.name);
      if (isSource && !isTest) {
        files.push(child);
      }
    }
  };
  walk(root);
  return files;
}

const SCANNED_FILES = SCANNED_ROOTS.flatMap(collectSourceFiles).sort();

/** What the scanner is reading at one character. */
type Mode = "block" | "code" | "line" | "quoted";

/**
 * One file's lines with every comment of `text` blanked, so prose that names a construct - a
 * `<textarea>` an interface documents, a `createProofEditor()` an explanation mentions - is not
 * read as a mount of it. Blanks rather than drops, so a line number below is the file's own.
 *
 * It reads character by character because a comment's opening characters are also ordinary text
 * inside a string: `path="/issues/:key/*"` in `app.tsx` opens no comment, and a regex that
 * swallowed everything after it to the next comment close would quietly stop this test from
 * seeing a hundred lines of the shell. A string's own characters are left as they are: no
 * construct is written in one, and the import check below reads module specifiers.
 */
function sourceLines(text: string): string[] {
  const chars = [...text];
  let mode: Mode = "code";
  let quote = "";
  let at = 0;
  while (at < chars.length) {
    const char = chars[at] as string;
    const pair = `${char}${chars[at + 1] ?? ""}`;
    if (mode === "code") {
      if (pair === "//" || pair === "/*") {
        mode = pair === "//" ? "line" : "block";
        chars[at] = " ";
        chars[at + 1] = " ";
        at += 2;
        continue;
      }
      if (char === '"' || char === "'" || char === "`") {
        mode = "quoted";
        quote = char;
      }
    } else if (mode === "line") {
      if (char === "\n") {
        mode = "code";
      } else {
        chars[at] = " ";
      }
    } else if (mode === "block") {
      if (pair === "*/") {
        mode = "code";
        chars[at] = " ";
        chars[at + 1] = " ";
        at += 2;
        continue;
      }
      if (char !== "\n") {
        chars[at] = " ";
      }
    } else if (char === "\\") {
      at += 2;
      continue;
    } else if (char === quote || (char === "\n" && quote !== "`")) {
      // Only a template literal holds a newline, so one ends a `'` that was an apostrophe in
      // text rather than the opening of a string.
      mode = "code";
    }
    at += 1;
  }
  return chars.join("").split("\n");
}

/** Each scanned file's own text, read once: the row checks below read a word anywhere in it,
 *  the walk of the boxes reads it with its comments blanked, the pick reader parses it
 *  (`picksOf`), and so do the walks of the reading list (`TREE_BY_FILE`). */
const TEXT_BY_FILE = new Map(
  SCANNED_FILES.map((file) => [file, readFileSync(join(REPO_ROOT, file), "utf8")] as const)
);

const LINES_BY_FILE = new Map(
  SCANNED_FILES.map((file) => [file, sourceLines(TEXT_BY_FILE.get(file) ?? "")] as const)
);

/** Whether `file` holds any of `constructs`. */
function holdsAny(file: string, constructs: readonly Construct[]): boolean {
  return (
    LINES_BY_FILE.get(file)?.some((line) =>
      constructs.some((construct) => construct.pattern.test(line))
    ) === true
  );
}

/** Whether `file` writes `word`, comments included: a row's id is written at its site as a
 *  pointer to the row, and a pointer is a comment. */
function writesWord(file: string, word: string): boolean {
  return TEXT_BY_FILE.get(file)?.includes(word) === true;
}

/** What one file's code picks of the writing list, read from its syntax tree, so a comment, a
 *  string anywhere else - a mutation key, a test id - and an attribute of another element are
 *  no pick. */
interface Picks {
  /** The rows its shared-field mounts pick on their own attributes: `box="<id>"`,
   *  `writing={dispatchWritingBox("<id>", ...)}`, or `writing={NAME}` where NAME is a `const` of
   *  the file initialised with that call. */
  readonly mounts: ReadonlySet<string>;
  /** Every row it calls `dispatchWritingBox("<id>", ...)` with, anywhere: an editor hosts the
   *  picker beside its mount, so a box made of one picks its row there. */
  readonly calls: ReadonlySet<string>;
}

/** The row a `dispatchWritingBox("<id>", ...)` call picks; `undefined` for any other node,
 *  a call whose row is no string literal included. */
function writingBoxCall(node: ts.Node | undefined): string | undefined {
  if (
    node === undefined ||
    !ts.isCallExpression(node) ||
    !ts.isIdentifier(node.expression) ||
    node.expression.text !== "dispatchWritingBox"
  ) {
    return undefined;
  }
  const [box] = node.arguments;
  return box !== undefined && ts.isStringLiteral(box) ? box.text : undefined;
}

/** `source`'s picks, parsed as the TSX it is: the parser ends a string, a comment and a tag
 *  where TypeScript does, escapes included, so a mount's attributes are its own and no more. */
function picksOf(file: string, source: string): Picks {
  const tree = ts.createSourceFile(file, source, ts.ScriptTarget.Latest, true, ts.ScriptKind.TSX);
  const calls = new Set<string>();
  const held = new Map<string, string>();
  const mounts: ts.JsxOpeningLikeElement[] = [];
  const visit = (node: ts.Node): void => {
    const call = writingBoxCall(node);
    if (call !== undefined) {
      calls.add(call);
    }
    if (
      ts.isVariableDeclaration(node) &&
      ts.isIdentifier(node.name) &&
      (ts.getCombinedNodeFlags(node) & ts.NodeFlags.Const) !== 0
    ) {
      const box = writingBoxCall(node.initializer);
      if (box !== undefined) {
        held.set(node.name.text, box);
      }
    }
    if (
      (ts.isJsxOpeningElement(node) || ts.isJsxSelfClosingElement(node)) &&
      SHARED_FIELD_ELEMENTS.includes(node.tagName.getText(tree))
    ) {
      mounts.push(node);
    }
    ts.forEachChild(node, visit);
  };
  visit(tree);
  // The mounts are read once the whole file has been, since a constant one names may be
  // declared below it.
  const picked = new Set<string>();
  for (const mount of mounts) {
    for (const attribute of mount.attributes.properties) {
      if (!ts.isJsxAttribute(attribute) || attribute.initializer === undefined) {
        continue;
      }
      const name = attribute.name.getText(tree);
      const value = attribute.initializer;
      let box: string | undefined;
      if (name === "box" && ts.isStringLiteral(value)) {
        box = value.text;
      } else if (name === "writing" && ts.isJsxExpression(value)) {
        const { expression } = value;
        box =
          expression !== undefined && ts.isIdentifier(expression)
            ? held.get(expression.text)
            : writingBoxCall(expression);
      }
      if (box !== undefined) {
        picked.add(box);
      }
    }
  }
  return { calls, mounts: picked };
}

/** Each scanned file's picks, read the first time a covered row of it asks. */
const PICKS_BY_FILE = new Map<string, Picks>();

/** Whether `file` picks `box`: on a shared-field mount's own attributes, or, where `editor` is
 *  set, in a `dispatchWritingBox("<box>", ...)` call anywhere, since an editor hosts the picker
 *  beside its mount. A mount picking another row stands in for nothing. */
function picksBox(file: string, box: string, editor: boolean): boolean {
  let picks = PICKS_BY_FILE.get(file);
  if (picks === undefined) {
    picks = picksOf(file, TEXT_BY_FILE.get(file) ?? "");
    PICKS_BY_FILE.set(file, picks);
  }
  return picks.mounts.has(box) || (editor && picks.calls.has(box));
}

/** One place the walk found a construct. */
interface Site {
  readonly at: string;
  readonly construct: string;
  readonly file: string;
}

function sitesOf(constructs: readonly Construct[], skipped: ReadonlySet<string>): Site[] {
  const sites: Site[] = [];
  for (const file of SCANNED_FILES) {
    if (skipped.has(file)) {
      continue;
    }
    const lines = LINES_BY_FILE.get(file) ?? [];
    lines.forEach((line, index) => {
      for (const construct of constructs) {
        if (construct.pattern.test(line)) {
          sites.push({ at: `${file}:${index + 1}`, construct: construct.name, file });
        }
      }
    });
  }
  return sites;
}

const WRITING_SITES = sitesOf(WRITING_CONSTRUCTS, new Set([SHARED_FIELD]));

const WRITING_FILES = new Set(DISPATCH_WRITING_BOXES.flatMap((row) => row.files));

/** `source` parsed as what its name says it is, so the parser ends a string, a comment and a tag
 *  where TypeScript does, escapes included. A `.ts` file is read as no TSX, since there
 *  `<T>value` is a type assertion rather than an element. */
function parseSource(file: string, source: string): ts.SourceFile {
  const kind = file.endsWith(".tsx") ? ts.ScriptKind.TSX : ts.ScriptKind.TS;
  return ts.createSourceFile(file, source, ts.ScriptTarget.Latest, true, kind);
}

/** Each scanned file's syntax tree, parsed once for both walks of the reading list. */
const TREE_BY_FILE = new Map(
  SCANNED_FILES.map((file) => [file, parseSource(file, TEXT_BY_FILE.get(file) ?? "")] as const)
);

/** The line `node` starts on, counted from one as an editor counts it. */
function lineOf(tree: ts.SourceFile, node: ts.Node): number {
  return tree.getLineAndCharacterOfPosition(node.getStart(tree)).line + 1;
}

/** One mount of a reading construct, and the row its `surface` names. */
interface Mount {
  readonly at: string;
  readonly construct: string;
  readonly file: string;
  /** Whether it hands the construct the record its text is, for a construct that takes one
   *  (`SOURCE_TAKERS`); `undefined` for one that takes none. */
  readonly sourced: boolean | undefined;
  /** The row its `surface` names as a string; `undefined` where it names none. */
  readonly surface: string | undefined;
}

/** Whether a `source` value hands a record: anything but `undefined` written out. */
function handsRecord(value: ts.Expression | undefined): boolean {
  return value !== undefined && !(ts.isIdentifier(value) && value.text === "undefined");
}

/** Whether an element's own attributes hand it a `source`. A spread may hold one, and the walk
 *  cannot read what it holds, so it counts as one. */
function elementSource(element: ts.JsxOpeningLikeElement, tree: ts.SourceFile): boolean {
  return element.attributes.properties.some((property) => {
    if (ts.isJsxSpreadAttribute(property)) {
      return true;
    }
    if (!ts.isJsxAttribute(property) || property.name.getText(tree) !== "source") {
      return false;
    }
    const value = property.initializer;
    return handsRecord(value !== undefined && ts.isJsxExpression(value) ? value.expression : value);
  });
}

/** Whether a hook's options hand it a `source`: the object literal that is its last argument
 *  names one, or spreads what may. Options the walk cannot read as a literal count as one. */
function hookSource(call: ts.CallExpression): boolean {
  const options = call.arguments[call.arguments.length - 1];
  if (call.arguments.length < 3 || options === undefined) {
    return false;
  }
  if (!ts.isObjectLiteralExpression(options)) {
    return true;
  }
  return options.properties.some((property) => {
    if (ts.isSpreadAssignment(property)) {
      return true;
    }
    if (property.name === undefined || !ts.isIdentifier(property.name)) {
      return false;
    }
    if (property.name.text !== "source") {
      return false;
    }
    return ts.isShorthandPropertyAssignment(property)
      ? true
      : ts.isPropertyAssignment(property) && handsRecord(property.initializer);
  });
}

/** A `surface` that hands on the construct's own: a preview's title is the projection's line,
 *  drawn on the preview's surface. */
const HANDED_ON = Symbol("handed on");

/** What a mount's `surface` holds: a row's id written as a string, the construct's own handed
 *  on (`HANDED_ON`), or nothing the walk can read. */
function surfaceOf(value: ts.Expression | undefined): string | typeof HANDED_ON | undefined {
  if (value === undefined) {
    return undefined;
  }
  if (ts.isStringLiteral(value) || ts.isNoSubstitutionTemplateLiteral(value)) {
    return value.text;
  }
  return ts.isIdentifier(value) && value.text === "surface" ? HANDED_ON : undefined;
}

/** The `surface` an element's own attributes give it. */
function elementSurface(
  element: ts.JsxOpeningLikeElement,
  tree: ts.SourceFile
): string | typeof HANDED_ON | undefined {
  const attribute = element.attributes.properties.find(
    (property): property is ts.JsxAttribute =>
      ts.isJsxAttribute(property) && property.name.getText(tree) === "surface"
  );
  const value = attribute?.initializer;
  return surfaceOf(value !== undefined && ts.isJsxExpression(value) ? value.expression : value);
}

/** The `surface` a hook's options give it: the object literal that is its last argument. */
function hookSurface(call: ts.CallExpression): string | typeof HANDED_ON | undefined {
  const options = call.arguments[call.arguments.length - 1];
  if (options === undefined || !ts.isObjectLiteralExpression(options)) {
    return undefined;
  }
  for (const property of options.properties) {
    if (property.name === undefined || !ts.isIdentifier(property.name)) {
      continue;
    }
    if (property.name.text !== "surface") {
      continue;
    }
    if (ts.isShorthandPropertyAssignment(property)) {
      return HANDED_ON;
    }
    return ts.isPropertyAssignment(property) ? surfaceOf(property.initializer) : undefined;
  }
  return undefined;
}

/** The name of the function `node` is written in: a declaration's own, or that of the constant
 *  an arrow or a function expression initialises. A callback handed to a call has none, so the
 *  walk reads through it to the function around it. */
function functionNameOf(node: ts.Node): string | undefined {
  for (let at: ts.Node | undefined = node.parent; at !== undefined; at = at.parent) {
    if (ts.isFunctionDeclaration(at)) {
      return at.name?.text;
    }
    if (
      (ts.isArrowFunction(at) || ts.isFunctionExpression(at)) &&
      ts.isVariableDeclaration(at.parent) &&
      ts.isIdentifier(at.parent.name)
    ) {
      return at.parent.name.text;
    }
  }
  return undefined;
}

/** `tree`'s mounts of the reading constructs. A construct handing its own `surface` on to the
 *  construct it is made of - a preview to the projection of its title - is no mount of its own:
 *  its callers' mounts name the row. Anywhere else a `surface` handed on names no row the walk
 *  can read, so that mount names none. */
function mountsOf(tree: ts.SourceFile): Mount[] {
  const file = tree.fileName;
  const mounts: Mount[] = [];
  const visit = (node: ts.Node): void => {
    let construct: ReadingConstruct | undefined;
    let surface: string | typeof HANDED_ON | undefined;
    let sourced: boolean | undefined;
    if (ts.isJsxOpeningElement(node) || ts.isJsxSelfClosingElement(node)) {
      const tag = node.tagName.getText(tree);
      construct = READING_ELEMENTS.get(tag);
      surface = construct === undefined ? undefined : elementSurface(node, tree);
      sourced = SOURCE_TAKERS.has(tag) ? elementSource(node, tree) : undefined;
    } else if (ts.isCallExpression(node) && ts.isIdentifier(node.expression)) {
      construct = READING_HOOKS.get(node.expression.text);
      surface = construct === undefined ? undefined : hookSurface(node);
      sourced = SOURCE_TAKERS.has(node.expression.text) ? hookSource(node) : undefined;
    }
    if (construct !== undefined) {
      const name = functionNameOf(node) ?? "";
      const handedOn =
        surface === HANDED_ON && (READING_ELEMENTS.has(name) || READING_HOOKS.has(name));
      if (!handedOn) {
        mounts.push({
          at: `${file}:${lineOf(tree, node)}`,
          construct: construct.name,
          file,
          sourced,
          surface: typeof surface === "string" ? surface : undefined,
        });
      }
    }
    ts.forEachChild(node, visit);
  };
  visit(tree);
  return mounts;
}

const MOUNTS = [...TREE_BY_FILE.values()].flatMap(mountsOf);

/** The mount's place, or what is wrong with the row it names: none, one the list does not hold,
 *  or one that does not name the mount's file. One thing drawn in two layouts is one row
 *  (`DispatchReadingSurfaceRow`), so any number of mounts may name the same one. */
function mountComplaint(mount: Mount): string {
  if (mount.surface === undefined) {
    return `${mount.at}: ${mount.construct} names no row of DISPATCH_READING_SURFACES in its surface`;
  }
  const row = DISPATCH_READING_SURFACES.find((candidate) => candidate.surface === mount.surface);
  if (row === undefined) {
    return `${mount.at}: ${mount.construct} names ${mount.surface}, which is no row of DISPATCH_READING_SURFACES`;
  }
  return row.files.includes(mount.file)
    ? mount.at
    : `${mount.at}: ${mount.construct} names ${mount.surface}, whose row does not name ${mount.file}`;
}

/** The mount's place, or what is wrong with the record it is given: a mount on a row whose text
 *  is a record (`record: true`) hands the construct that record as its `source`, since a `#id`
 *  in the text is read against that record's document, unless the row says its surface cannot
 *  carry a question id (`cannotCarry`). A mount of a construct that takes no source, and one on
 *  a row whose text is no record, is held to nothing here. */
function sourceComplaint(mount: Mount): string {
  if (mount.sourced !== false || !rowTakesSource(mount.surface)) {
    return mount.at;
  }
  return `${mount.at}: ${mount.construct} draws ${mount.surface}, whose text is a record, and hands it no source`;
}

/** Whether a mount on the row `surface` names is handed the record its text is: a row whose text
 *  is a record, and that does not say its surface cannot carry a question id. */
function rowTakesSource(surface: string | undefined): boolean {
  const row = DISPATCH_READING_SURFACES.find((candidate) => candidate.surface === surface);
  return row?.record === true && row.cannotCarry?.forms.includes("question-id") !== true;
}

/** A component that hands a prop of its own on as the record a construct's text is: the
 *  construct's `source`, or the same prop of a component that hands it on in turn. The record
 *  reaches the construct only where whoever mounts the component hands it that prop. */
interface SourceForward {
  readonly component: string;
  readonly ref: PropRef;
  readonly surface: string;
}

/**
 * Each mount `trees` hold of a component that hands a prop of its own on as a construct's
 * `source`, on a row that takes one (`rowTakesSource`): its place, or what is wrong where it
 * hands that prop nothing. A component is found to hand its prop on where a mount's `source` is
 * read from its own props (`propsHanded`), and is then held to it wherever it is mounted, by its
 * name, in any of the files, as the tree check holds a component; the walk runs until no
 * component is added, so one that hands the prop to another that hands it on is held too. A
 * spread may hold the prop, and the walk cannot read what it holds, so it counts as handing one.
 */
function forwardedSourceComplaints(trees: readonly ts.SourceFile[]): string[] {
  const forwards: SourceForward[] = [];
  const known = new Set<string>();
  const learn = (node: ts.Node, value: ts.Expression | undefined, surface: string): boolean => {
    const component = componentOf(node);
    if (value === undefined || component === undefined) {
      return false;
    }
    let added = false;
    for (const ref of propsHanded(value, component)) {
      const key = `${component.name}.${ref.prop}.${ref.member ?? ""}`;
      if (!known.has(key)) {
        known.add(key);
        forwards.push({ component: component.name, ref, surface });
        added = true;
      }
    }
    return added;
  };
  const elementsOf = (tree: ts.SourceFile): ts.JsxOpeningLikeElement[] => {
    const elements: ts.JsxOpeningLikeElement[] = [];
    const visit = (node: ts.Node): void => {
      if (ts.isJsxOpeningElement(node) || ts.isJsxSelfClosingElement(node)) {
        elements.push(node);
      }
      ts.forEachChild(node, visit);
    };
    visit(tree);
    return elements;
  };
  const elements = trees.flatMap((tree) => elementsOf(tree).map((node) => ({ node, tree })));
  for (const { node, tree } of elements) {
    const surface = elementSurface(node, tree);
    if (
      SOURCE_TAKERS.has(node.tagName.getText(tree)) &&
      typeof surface === "string" &&
      rowTakesSource(surface)
    ) {
      learn(node, attributesOf(node, tree).values.get("source"), surface);
    }
  }
  for (let added = true; added; ) {
    added = false;
    for (const { node, tree } of elements) {
      for (const forward of forwards.filter(
        (each) => each.component === node.tagName.getText(tree)
      )) {
        added =
          learn(node, attributesOf(node, tree).values.get(forward.ref.prop), forward.surface) ||
          added;
      }
    }
  }
  const complaints: string[] = [];
  for (const { node, tree } of elements) {
    const tag = node.tagName.getText(tree);
    for (const forward of forwards.filter((each) => each.component === tag)) {
      const at = `${tree.fileName}:${lineOf(tree, node)}`;
      const handed = handedAt(attributesOf(node, tree), forward.ref);
      complaints.push(
        handed.some((each) => each.kind !== "none")
          ? at
          : `${at}: ${tag} hands its ${forward.ref.prop} on as the source of ${forward.surface}, whose text is a record, and is mounted here with none`
      );
    }
  }
  return complaints;
}

/** What the API's contract says of the stored texts it sends. */
interface ContractTexts {
  /** Every field the server also sends read as a tree - `<field>_tree` beside it, or for a list
   *  of texts `<field>_trees`, one tree for each item: a tree is what the server makes of a
   *  writer's stored text, so the field holds one. */
  readonly fields: readonly string[];
  /** For each of those fields, the properties of the contract's schemas, at any depth, whose type
   *  names a schema holding it, or a list of one (`schemasNamed`): `options` for an ask option's
   *  `label`. These are the fields a dashboard value is reached through, so neither a schema's
   *  own name nor a key of the API's operations (`responses`, `content`, `application/json`)
   *  is one. */
  readonly holders: ReadonlyMap<string, ReadonlySet<string>>;
}

/** The schemas `type` names itself - `components["schemas"]["AskOption"]`, a list of one, or
 *  either in a union - and none an object type written inside it names: those are its own
 *  properties' schemas. */
function schemasNamed(type: ts.TypeNode, contract: ts.SourceFile): string[] {
  if (ts.isTypeLiteralNode(type)) return [];
  if (ts.isIndexedAccessTypeNode(type)) {
    const schema = /^components\["schemas"\]\["(\w+)"\]$/.exec(type.getText(contract))?.[1];
    return schema === undefined ? [] : [schema];
  }
  const named: string[] = [];
  ts.forEachChild(type, (child) => {
    if (ts.isTypeNode(child)) named.push(...schemasNamed(child, contract));
  });
  return named;
}

/** The contract's stored texts, read from the contract itself, so a stored text the API gains
 *  is looked for the day it lands. */
function readContract(): ContractTexts {
  const source = readFileSync(join(REPO_ROOT, API_CONTRACT), "utf8");
  const contract = parseSource(API_CONTRACT, source);
  const fields = new Set<string>();
  /** Each schema's stored fields, by the schema's name. */
  const bySchema = new Map<string, string[]>();
  /** Each property of a schema: its name and the schemas its type names (`schemasNamed`). */
  const properties: [string, string[]][] = [];
  // `inSchemas` is whether `node` is inside `components`' `schemas`, the API's own shapes.
  const visit = (node: ts.Node, inSchemas: boolean): void => {
    if (ts.isTypeLiteralNode(node) || ts.isInterfaceDeclaration(node)) {
      const names = new Set(
        node.members.flatMap((member) =>
          member.name !== undefined &&
          (ts.isIdentifier(member.name) || ts.isStringLiteral(member.name))
            ? [member.name.text]
            : []
        )
      );
      const stored = [...names].filter(
        (name) => names.has(`${name}_tree`) || names.has(`${name}_trees`)
      );
      for (const field of stored) {
        fields.add(field);
      }
      const owner = node.parent;
      if (
        stored.length > 0 &&
        ts.isPropertySignature(owner) &&
        (ts.isIdentifier(owner.name) || ts.isStringLiteral(owner.name))
      ) {
        bySchema.set(owner.name.text, stored);
      }
    }
    let inside = inSchemas;
    if (
      ts.isPropertySignature(node) &&
      node.type !== undefined &&
      (ts.isIdentifier(node.name) || ts.isStringLiteral(node.name))
    ) {
      if (inSchemas) {
        properties.push([node.name.text, schemasNamed(node.type, contract)]);
      }
      inside ||=
        node.name.text === "schemas" &&
        ts.isInterfaceDeclaration(node.parent) &&
        node.parent.name.text === "components";
    }
    ts.forEachChild(node, (child) => visit(child, inside));
  };
  visit(contract, false);
  const holders = new Map<string, Set<string>>();
  for (const [name, schemas] of properties) {
    for (const schema of schemas) {
      for (const field of bySchema.get(schema) ?? []) {
        holders.set(field, (holders.get(field) ?? new Set<string>()).add(name));
      }
    }
  }
  return { fields: [...fields].sort(), holders };
}

const CONTRACT_TEXTS = readContract();

const TREED_FIELDS = CONTRACT_TEXTS.fields;

/** The fields whose value is a writer's stored text. */
const STORED_PROSE_FIELDS = new Set([
  ...TREED_FIELDS,
  ...HAND_NAMED_TEXTS.map((text) => text.field),
]);

/** The stored fields the dashboard's own controls and records also call their words by - a
 *  tab's, a menu item's or a filter's `label`, a picker row's `description`, and the `name` of a
 *  document, a project, a job or a group - so a member of one of these names is read as a stored
 *  text only on a value reached through a field of the contract that holds it
 *  (`ContractTexts.holders`; the test "reads exactly the fields of the contract that hold a
 *  label, a description or a name" lists them). Every other field is read as stored wherever it
 *  is written. */
const UI_WORD_FIELDS: readonly string[] = ["description", "label", "name"];

/** The fields of the contract a value holding one of `UI_WORD_FIELDS` is reached through. */
const UI_WORD_HOLDERS = new Set(
  UI_WORD_FIELDS.flatMap((field) => [...(CONTRACT_TEXTS.holders.get(field) ?? [])])
);

/** The list methods whose callback's first parameter is each item of the list. */
const MAPPING_METHODS: readonly string[] = ["flatMap", "map"];

/** The methods whose answer is still the text, or the texts, they are called on: a list of texts
 *  filtered or joined, or a text cut or tidied. */
const TEXT_METHODS: readonly string[] = [
  "filter",
  "join",
  "replace",
  "replaceAll",
  "slice",
  "substring",
  "trim",
  "trimEnd",
  "trimStart",
];

/** One place a stored text is drawn as the characters it is, with no construct between. */
interface RawDraw {
  readonly at: string;
  /** The line the JSX attribute the draw is written in starts on, where it is in one: a value
   *  that runs over several lines can carry its comment only above itself. */
  readonly attributeLine: number | undefined;
  readonly field: string;
  readonly file: string;
  readonly line: number;
  readonly pattern: string;
  /** The expression drawn, as written, its runs of space made one. */
  readonly written: string;
}

/** What a failure and the test's own name call a raw draw: one line of the work list. */
function drawLabel(draw: RawDraw): string {
  return `${draw.at} ${draw.field} ${draw.pattern}`;
}

/** The expressions `expression` draws: itself, or each one it hands on - through `??`, `||`,
 *  the right of `&&`, both sides of a `+` join, a conditional's two branches, a `Cast`, the
 *  items of a list written out, and a method whose answer is still its text (`TEXT_METHODS`). */
function drawnLeaves(expression: ts.Expression): ts.Expression[] {
  if (ts.isArrayLiteralExpression(expression)) {
    return expression.elements.flatMap(drawnLeaves);
  }
  if (isCast(expression)) {
    return drawnLeaves(expression.expression);
  }
  if (ts.isConditionalExpression(expression)) {
    return [...drawnLeaves(expression.whenTrue), ...drawnLeaves(expression.whenFalse)];
  }
  if (ts.isBinaryExpression(expression)) {
    const operator = expression.operatorToken.kind;
    if (
      operator === ts.SyntaxKind.QuestionQuestionToken ||
      operator === ts.SyntaxKind.BarBarToken ||
      operator === ts.SyntaxKind.PlusToken
    ) {
      return [...drawnLeaves(expression.left), ...drawnLeaves(expression.right)];
    }
    if (operator === ts.SyntaxKind.AmpersandAmpersandToken) {
      return drawnLeaves(expression.right);
    }
  }
  if (
    ts.isCallExpression(expression) &&
    ts.isPropertyAccessExpression(expression.expression) &&
    TEXT_METHODS.includes(expression.expression.name.text)
  ) {
    return drawnLeaves(expression.expression.expression);
  }
  return [expression];
}

/** What a name bound to a projection hook's answer holds: the line the projection made of a
 *  stored text, which is the reader's words whatever field it came from. */
const PROJECTED = Symbol("projected");

/** A name holding a value reached through a field of the contract that holds one of
 *  `UI_WORD_FIELDS`: an ask's option, mapped out of its `options`. */
interface Through {
  readonly through: string;
}

/** A component's props, held whole under the parameter's own name, by the component they are
 *  the props of: `props.words` is what a mount of that component hands its `words`. */
interface Props {
  readonly props: string;
}

/** A value declared as one of the dashboard's own records that carry a writer's stored text
 *  (`PROSE_RECORDS`), by the record's type name. */
interface ProseRecord {
  readonly record: string;
}

/** What a name in a function holds: the stored-prose field it was read from, `PROJECTED`, a
 *  value reached through a holder of a field named like the dashboard's own words, a
 *  component's props, or one of the dashboard's records that carry a stored text. */
type Held = string | typeof PROJECTED | Through | Props | ProseRecord;

/**
 * The dashboard's own records that carry a writer's stored text under a field the contract's
 * scan cannot name, each by the type name it is declared under, with the paths of its fields
 * that hold that text: the new issue page's node, the facts of the focused one the panel names
 * it by, the reading a chip names its target by, and the line a filter names a parent by. A
 * value declared as one of them - a prop, a parameter - is that text: handed whole to a call
 * whose answer is drawn, as G's folded line once handed a node to
 * `firstLineOf(lineWords(node))`, it draws it, and so does one of those fields read off it, as
 * `chipWords(wordsOf(chip.reading.tree))` would, unless the call is the projection, whose answer
 * is its line. A chip's `reading.words` is the page's word for a document and no writer's.
 */
const PROSE_RECORDS: ReadonlyMap<string, readonly string[]> = new Map([
  ["FocusedNode", ["text", "tree"]],
  ["NodeChip", ["reading.tree"]],
  ["ParentLine", ["text", "tree"]],
  ["TreeNode", ["text", "tree"]],
]);

/** The record type `type` names (`PROSE_RECORDS`): a reference to it by name, never a list. */
function recordOf(type: ts.TypeNode | undefined): string | undefined {
  if (type === undefined) return undefined;
  if (ts.isTypeReferenceNode(type) && ts.isIdentifier(type.typeName)) {
    return PROSE_RECORDS.has(type.typeName.text) ? type.typeName.text : undefined;
  }
  return undefined;
}

/** The record type a destructured prop is declared as, read off its parameter's type literal:
 *  `{ node: TreeNode }` declares `node` a node. */
function declaredRecord(parameter: ts.ParameterDeclaration, prop: string): string | undefined {
  const literal = parameter.type;
  if (literal === undefined || !ts.isTypeLiteralNode(literal)) return undefined;
  const member = literal.members.find(
    (one): one is ts.PropertySignature =>
      ts.isPropertySignature(one) &&
      (ts.isIdentifier(one.name) || ts.isStringLiteral(one.name)) &&
      one.name.text === prop
  );
  return recordOf(member?.type);
}

/** Whether `expression` is a call of a projection hook. */
function isProjection(expression: ts.Expression): boolean {
  return (
    ts.isCallExpression(expression) &&
    ts.isIdentifier(expression.expression) &&
    READING_HOOKS.has(expression.expression.text)
  );
}

/** The names a binding binds, through every pattern it destructures. */
function boundNames(name: ts.BindingName): string[] {
  if (ts.isIdentifier(name)) {
    return [name.text];
  }
  return name.elements.flatMap((element) =>
    ts.isBindingElement(element) ? boundNames(element.name) : []
  );
}

/** The declaration a destructured binding belongs to: the variable or the parameter whose name
 *  its pattern is. */
function bindingRoot(element: ts.BindingElement): ts.Node {
  let at: ts.Node = element.parent;
  while (ts.isObjectBindingPattern(at) || ts.isArrayBindingPattern(at) || ts.isBindingElement(at)) {
    at = at.parent;
  }
  return at;
}

/** The function, or the file, whose names a declaration's names are. */
function scopeOf(node: ts.Node): ts.Node {
  let at: ts.Node = node.parent;
  while (!ts.isFunctionLike(at) && !ts.isSourceFile(at)) {
    at = at.parent;
  }
  return at;
}

/** Whether `node` is in the text a construct is handed to draw: a projection hook's argument,
 *  or a construct element's `markdown`, `referenceText` or `body`. */
function handedToConstruct(node: ts.Node, tree: ts.SourceFile): boolean {
  let child = node;
  let at: ts.Node | undefined = node.parent;
  while (at !== undefined) {
    if (ts.isCallExpression(at) && at.expression !== child && isProjection(at)) {
      return true;
    }
    if (
      ts.isJsxAttribute(at) &&
      CONSTRUCT_TEXT_ATTRIBUTES.includes(at.name.getText(tree)) &&
      READING_ELEMENTS.has(at.parent.parent.tagName.getText(tree))
    ) {
      return true;
    }
    child = at;
    at = at.parent;
  }
  return false;
}

/** What a member chain reads (`memberChain`): the name it starts from, where it starts from one,
 *  and the key of each member it reads after that name, in order - a property's name, an
 *  element's literal key, and `undefined` for any other element key. */
interface MemberChain {
  readonly path: readonly (string | undefined)[];
  readonly root: ts.Identifier | undefined;
}

/** The member chain `expression` reads (`MemberChain`), through `?.` and what `uncast` takes
 *  off: `edge.excerpt?.text` is `edge`, then `excerpt` and `text`, and so is
 *  `(edge as Edge).excerpt!.text`. */
function memberChain(expression: ts.Expression): MemberChain {
  const path: (string | undefined)[] = [];
  let at = uncast(expression);
  for (;;) {
    if (ts.isPropertyAccessExpression(at)) {
      path.unshift(at.name.text);
    } else if (ts.isElementAccessExpression(at)) {
      const key = uncast(at.argumentExpression);
      path.unshift(ts.isStringLiteral(key) || ts.isNumericLiteral(key) ? key.text : undefined);
    } else {
      break;
    }
    at = uncast(at.expression);
  }
  return { path, root: ts.isIdentifier(at) ? at : undefined };
}

/** The names `tree` imports, each with the module it comes from. A name from the theme module
 *  holds class names and never a stored text - a tone's `text` there is the class its words are
 *  painted in - and a function that answers a stored text is that function only where the file
 *  imports it, since another file may declare its own of the same name. */
function importsOf(tree: ts.SourceFile): Map<string, string> {
  const names = new Map<string, string>();
  for (const statement of tree.statements) {
    if (!ts.isImportDeclaration(statement) || !ts.isStringLiteral(statement.moduleSpecifier)) {
      continue;
    }
    const bindings = statement.importClause?.namedBindings;
    if (bindings !== undefined && ts.isNamedImports(bindings)) {
      for (const element of bindings.elements) {
        names.set(element.name.text, statement.moduleSpecifier.text);
      }
    }
  }
  return names;
}

/** An expression that holds the value of the one inside it: brackets, a cast (`as`,
 *  `satisfies` or `<T>`) or `!`. */
type Cast =
  | ts.AsExpression
  | ts.NonNullExpression
  | ts.ParenthesizedExpression
  | ts.SatisfiesExpression
  | ts.TypeAssertion;

/** Whether `node` is a `Cast`: the one list of them, which a walk down to a value (`uncast`,
 *  `drawnLeaves`, `throughOf`) takes off and a walk up from one (`memberReadOf`, `passesOn`)
 *  goes through. */
function isCast(node: ts.Node): node is Cast {
  return (
    ts.isAsExpression(node) ||
    ts.isNonNullExpression(node) ||
    ts.isParenthesizedExpression(node) ||
    ts.isSatisfiesExpression(node) ||
    ts.isTypeAssertionExpression(node)
  );
}

/** `expression` with every `Cast` around it taken off: `{ … } as const` is the object literal
 *  it casts, and `(LINES as Lines)!` is `LINES`. */
function uncast(expression: ts.Expression): ts.Expression {
  let value = expression;
  while (isCast(value)) {
    value = value.expression;
  }
  return value;
}

/** The constants `tree` declares at its top level as an object literal, by name, each with that
 *  literal: a table of the page's own words is one (`issue-page/slots/body.tsx`'s
 *  `ABSENT_LINES`, whose lines each hold a `heading`). A name the file binds again anywhere below
 *  its top level is no table (`namesBoundBelowTop`), since a read of it may be of that other
 *  binding. Neither is a name the file writes to (`writtenNames`), nor one it uses whole
 *  (`namesUsedWhole`) - the name itself, or a member of it, passed on as a value - since what it
 *  holds is then no longer what its literal says, or the walk no longer sees all that is done
 *  with it. Which member read off a table is words the file wrote is `readsWordTable`'s to say. */
function wordTablesOf(tree: ts.SourceFile): ReadonlyMap<string, ts.ObjectLiteralExpression> {
  const below = namesBoundBelowTop(tree);
  const written = writtenNames(tree);
  const literals = new Map<string, ts.ObjectLiteralExpression>();
  for (const statement of tree.statements) {
    if (
      !ts.isVariableStatement(statement) ||
      (statement.declarationList.flags & ts.NodeFlags.Const) === 0
    ) {
      continue;
    }
    for (const declaration of statement.declarationList.declarations) {
      const value =
        declaration.initializer === undefined ? undefined : uncast(declaration.initializer);
      const name = ts.isIdentifier(declaration.name) ? declaration.name.text : undefined;
      if (
        value !== undefined &&
        ts.isObjectLiteralExpression(value) &&
        name !== undefined &&
        !below.has(name) &&
        !written.has(name)
      ) {
        literals.set(name, value);
      }
    }
  }
  const whole = namesUsedWhole(tree, literals);
  return new Map([...literals].filter(([name]) => !whole.has(name)));
}

/** Every name a declaration below `tree`'s top level binds (`boundNames`): a variable's - a
 *  `var` in an inner block among them - a parameter's, a catch clause's, and the own name of a
 *  function or a class, declared or written as an expression. A top-level declaration's own
 *  names are the top level's; whatever it holds is below it. */
function namesBoundBelowTop(tree: ts.SourceFile): ReadonlySet<string> {
  const names = new Set<string>();
  const below = (node: ts.Node): void => {
    if (ts.isVariableDeclaration(node) || ts.isParameter(node)) {
      for (const name of boundNames(node.name)) names.add(name);
    } else if (
      (ts.isFunctionDeclaration(node) ||
        ts.isFunctionExpression(node) ||
        ts.isClassDeclaration(node) ||
        ts.isClassExpression(node)) &&
      node.name !== undefined
    ) {
      names.add(node.name.text);
    }
    ts.forEachChild(node, below);
  };
  for (const statement of tree.statements) {
    if (ts.isVariableStatement(statement)) {
      for (const declaration of statement.declarationList.declarations) {
        if (declaration.initializer !== undefined) below(declaration.initializer);
      }
    } else if (ts.isFunctionDeclaration(statement) || ts.isClassDeclaration(statement)) {
      ts.forEachChild(statement, (child) => {
        if (child !== statement.name) below(child);
      });
    } else {
      below(statement);
    }
  }
  return names;
}

/** Each target a destructuring assignment's pattern writes, at any depth - a member's value, a
 *  shorthand member's name, an element, a rest, each with its default taken off - and any other
 *  target itself: `LINES.absent` of `({ heading: LINES.absent } = decision)` and of
 *  `[LINES.absent] = words`. */
function patternTargets(target: ts.Expression): ts.Expression[] {
  if (ts.isObjectLiteralExpression(target)) {
    return target.properties.flatMap((property): ts.Expression[] => {
      if (ts.isPropertyAssignment(property)) return patternTargets(property.initializer);
      if (ts.isShorthandPropertyAssignment(property)) return [property.name];
      if (ts.isSpreadAssignment(property)) return patternTargets(property.expression);
      return [];
    });
  }
  if (ts.isArrayLiteralExpression(target)) {
    return target.elements.flatMap((element) =>
      patternTargets(ts.isSpreadElement(element) ? element.expression : element)
    );
  }
  if (ts.isBinaryExpression(target) && target.operatorToken.kind === ts.SyntaxKind.EqualsToken) {
    return patternTargets(target.left);
  }
  return [target];
}

/** Every name `tree` writes to, as the root of what it writes (`memberChain`, casts taken off):
 *  each target an assignment of any kind writes, compound ones among them, and each target in
 *  the pattern of a destructuring one (`patternTargets`); each target a `for … in` or a
 *  `for … of` over an expression writes, through its pattern too; what `++` or `--` steps; and
 *  what `delete` takes out. A name, or a member of it, handed to a call - `Object.assign` and
 *  `Reflect.set` among them - is `namesUsedWhole`'s. */
function writtenNames(tree: ts.SourceFile): ReadonlySet<string> {
  const names = new Set<string>();
  const write = (target: ts.Expression): void => {
    const root = memberChain(target).root;
    if (root !== undefined) names.add(root.text);
  };
  const visit = (node: ts.Node): void => {
    if (
      ts.isBinaryExpression(node) &&
      node.operatorToken.kind >= ts.SyntaxKind.FirstAssignment &&
      node.operatorToken.kind <= ts.SyntaxKind.LastAssignment
    ) {
      for (const target of patternTargets(node.left)) write(target);
    } else if (ts.isForInStatement(node) || ts.isForOfStatement(node)) {
      const target = node.initializer;
      if (!ts.isVariableDeclarationList(target)) {
        for (const each of patternTargets(target)) write(each);
      }
    } else if (
      (ts.isPrefixUnaryExpression(node) || ts.isPostfixUnaryExpression(node)) &&
      (node.operator === ts.SyntaxKind.PlusPlusToken ||
        node.operator === ts.SyntaxKind.MinusMinusToken)
    ) {
      write(node.operand);
    } else if (ts.isDeleteExpression(node)) {
      write(node.expression);
    }
    ts.forEachChild(node, visit);
  };
  visit(tree);
  return names;
}

/** Every name `tree` uses whole, wherever it stands for a value (`namesNoValue`): bare, or as
 *  the root of a member read that is passed on (`memberReadOf`, `passesOn`). A bare name is
 *  one handed to a call (`fill(LINES)`, `Object.defineProperty(LINES, …)`), aliased
 *  (`const alias = LINES`), the source of a destructuring (`const { absent } = LINES`) or of a
 *  `for … of`, spread, held by a shorthand member, handed to an attribute, returned, or any
 *  other value made of it. A member read passed on is one the file may write through out of the
 *  walk's sight: `fill(LINES.absent)`, `Object.assign(LINES.absent, …)`,
 *  `const absent = LINES.absent`. The one exception is a member read that `readsWordTable`
 *  resolves, inside the name's own literal in `literals`, to nothing but strings: what
 *  `cut(LINES.absent.heading)` hands on is words, which no call writes. A name inside a type
 *  (`typeof LINES`) names no value, and is not read. */
function namesUsedWhole(
  tree: ts.SourceFile,
  literals: ReadonlyMap<string, ts.ObjectLiteralExpression>
): ReadonlySet<string> {
  const names = new Set<string>();
  const visit = (node: ts.Node): void => {
    if (ts.isTypeNode(node)) return;
    if (ts.isIdentifier(node) && !namesNoValue(node)) {
      const read = memberReadOf(node);
      if (read === undefined || (passesOn(read) && !readsWordTable(memberChain(read), literals))) {
        names.add(node.text);
      }
    }
    ts.forEachChild(node, visit);
  };
  visit(tree);
  return names;
}

/** Whether `name` stands where it names no value: the name a declaration binds, or that of a
 *  member, a key, an attribute or an import, the member a property access reads, the key of a
 *  destructured member, and a label. A shorthand member's name is the value it holds, and an
 *  export's local name the value it exports. */
function namesNoValue(name: ts.Identifier): boolean {
  const parent = name.parent;
  if (ts.isShorthandPropertyAssignment(parent)) return false;
  if (ts.isExportSpecifier(parent)) return (parent.propertyName ?? parent.name) !== name;
  if (ts.isBindingElement(parent) && parent.propertyName === name) return true;
  if (ts.isLabeledStatement(parent) || ts.isBreakOrContinueStatement(parent)) return true;
  return "name" in parent && parent.name === name;
}

/** The member read `name` is the root of, at its whole length, through every `Cast` inside it
 *  (`memberChain`): `LINES.absent.heading` for `LINES` there, and `(LINES as Lines)!["absent"]`
 *  for `LINES` there. `undefined` where `name` roots no member read. */
function memberReadOf(name: ts.Identifier): ts.Expression | undefined {
  let at: ts.Node = name;
  let read: ts.Expression | undefined;
  for (;;) {
    const parent = at.parent;
    if (
      (ts.isPropertyAccessExpression(parent) || ts.isElementAccessExpression(parent)) &&
      parent.expression === at
    ) {
      read = parent;
    } else if (!isCast(parent)) {
      return read;
    }
    at = parent;
  }
}

/**
 * Whether `value` is passed on as a value, so that what it holds may be written out of the
 * walk's sight. It is where it stands - itself, or through brackets, casts, `!`, a conditional's
 * branch, either side of `??` or `||`, the right of `&&` or of `,`, or an `await` that answers
 * it - as an argument of a call or of `new`, or a substitution of a tagged template, which its
 * tag is called with; the initializer of a variable, a parameter, a destructured binding, a
 * class's field or a member of an object literal; the right side of `=`, `&&=`, `||=` or `??=`;
 * what a `return`, an arrow's body or `export default` answers; what a `yield` hands the
 * generator's caller; a spread, in a list, a call's arguments, an object or an element's
 * attributes; an element of a list written out; an attribute's value, or a component's child,
 * which the component is handed as its `children`; or the list a `for … of` walks. Anywhere
 * else - drawn as the child of an element of the page, joined with `+` or into an untagged
 * template, compared, written to, a method called on it - it is not.
 */
function passesOn(value: ts.Expression): boolean {
  let at: ts.Node = value;
  for (;;) {
    const parent = at.parent;
    const binary = ts.isBinaryExpression(parent) ? parent : undefined;
    const operator = binary?.operatorToken.kind;
    const answers =
      isCast(parent) ||
      ts.isAwaitExpression(parent) ||
      (ts.isConditionalExpression(parent) && parent.condition !== at) ||
      operator === ts.SyntaxKind.QuestionQuestionToken ||
      operator === ts.SyntaxKind.BarBarToken ||
      (operator === ts.SyntaxKind.AmpersandAmpersandToken && binary?.right === at) ||
      (operator === ts.SyntaxKind.CommaToken && binary?.right === at);
    if (!answers) break;
    at = parent;
  }
  const holder = at.parent;
  if (ts.isCallExpression(holder) || ts.isNewExpression(holder)) {
    return holder.arguments?.some((argument) => argument === at) === true;
  }
  if (
    ts.isVariableDeclaration(holder) ||
    ts.isParameter(holder) ||
    ts.isBindingElement(holder) ||
    ts.isPropertyDeclaration(holder) ||
    ts.isPropertyAssignment(holder)
  ) {
    return holder.initializer === at;
  }
  if (ts.isBinaryExpression(holder)) {
    const operator = holder.operatorToken.kind;
    return (
      holder.right === at &&
      (operator === ts.SyntaxKind.EqualsToken ||
        operator === ts.SyntaxKind.AmpersandAmpersandEqualsToken ||
        operator === ts.SyntaxKind.BarBarEqualsToken ||
        operator === ts.SyntaxKind.QuestionQuestionEqualsToken)
    );
  }
  if (ts.isArrowFunction(holder)) return holder.body === at;
  if (ts.isJsxExpression(holder)) {
    // An element of the page is told from a component by its tag, as `rawDrawsOf` tells it.
    const owner = holder.parent;
    return (
      ts.isJsxAttribute(owner) ||
      (ts.isJsxElement(owner) && !/^[a-z]/.test(owner.openingElement.tagName.getText()))
    );
  }
  if (ts.isTemplateSpan(holder)) {
    const template = holder.parent;
    return ts.isTaggedTemplateExpression(template.parent) && template.parent.template === template;
  }
  if (ts.isForOfStatement(holder)) return holder.expression === at;
  return (
    ts.isReturnStatement(holder) ||
    ts.isExportAssignment(holder) ||
    ts.isYieldExpression(holder) ||
    ts.isSpreadElement(holder) ||
    ts.isSpreadAssignment(holder) ||
    ts.isJsxSpreadAttribute(holder) ||
    ts.isArrayLiteralExpression(holder)
  );
}

/** The key a member of an object literal is written under, `undefined` for a computed one. */
function memberKey(name: ts.PropertyName): string | undefined {
  if (ts.isIdentifier(name) || ts.isStringLiteral(name) || ts.isNumericLiteral(name)) {
    return name.text;
  }
  return undefined;
}

/**
 * Whether `chain`, a member read off a name (`memberChain`), reads words the file itself wrote:
 * the name is one of `tables` (`wordTablesOf`), and the read's path, resolved inside that
 * literal - a named member by its key, a computed one by every member at that place - reaches
 * nothing but string literals and templates with no substitution. `ABSENT_LINES[kind].heading`
 * is such a read; a holder of a record (`last.node.text`, where `node` is written `undefined`)
 * and a record filled at run time (`byKey[id].title`, an empty literal) are not.
 */
function readsWordTable(
  chain: MemberChain,
  tables: ReadonlyMap<string, ts.ObjectLiteralExpression>
): boolean {
  const { path, root } = chain;
  const table = root === undefined ? undefined : tables.get(root.text);
  if (table === undefined) return false;
  let values: ts.Expression[] = [table];
  for (const key of path) {
    const next: ts.Expression[] = [];
    for (const value of values) {
      const object = uncast(value);
      if (!ts.isObjectLiteralExpression(object)) return false;
      for (const member of object.properties) {
        const named = member.name === undefined ? undefined : memberKey(member.name);
        if (key !== undefined && named !== undefined && named !== key) continue;
        // A spread, a shorthand, a method or a computed key at the place the path reads leaves
        // what it holds unknown.
        if (!ts.isPropertyAssignment(member) || named === undefined) return false;
        next.push(member.initializer);
      }
    }
    if (next.length === 0) return false;
    values = next;
  }
  return values.every((value) => {
    const words = uncast(value);
    return ts.isStringLiteral(words) || ts.isNoSubstitutionTemplateLiteral(words);
  });
}

/** The name of the function whose own parameter list `parameter` is in: a declaration's own,
 *  or that of the constant an arrow or a function expression initialises. */
function ownerName(parameter: ts.ParameterDeclaration): string | undefined {
  const owner = parameter.parent;
  if (ts.isFunctionDeclaration(owner)) {
    return owner.name?.text;
  }
  const holder = owner.parent;
  return holder !== undefined && ts.isVariableDeclaration(holder) && ts.isIdentifier(holder.name)
    ? holder.name.text
    : undefined;
}

/** A function of the dashboard that answers a stored text, and the file that declares it. */
interface ProseAnswer {
  readonly field: string;
  readonly file: string;
}

/** The expressions a `useMemo` call answers: its factory's own expression, or each `return` of
 *  its body. Empty for any other expression. */
function memoAnswers(expression: ts.Expression): ts.Expression[] {
  if (
    !ts.isCallExpression(expression) ||
    !ts.isIdentifier(expression.expression) ||
    expression.expression.text !== "useMemo"
  ) {
    return [];
  }
  const [factory] = expression.arguments;
  if (factory === undefined || !(ts.isArrowFunction(factory) || ts.isFunctionExpression(factory))) {
    return [];
  }
  if (!ts.isBlock(factory.body)) {
    return [factory.body];
  }
  const returned: ts.Expression[] = [];
  const visit = (node: ts.Node): void => {
    if (ts.isReturnStatement(node) && node.expression !== undefined) {
      returned.push(node.expression);
    } else if (!ts.isFunctionLike(node)) {
      ts.forEachChild(node, visit);
    }
  };
  visit(factory.body);
  return returned;
}

/** The functions that answer a stored text, by name: each one a `return` of which hands back a
 *  stored field, as `askResolutionReason` hands back a resolution's `reason` - read off a value,
 *  through a constant of the function's own initialised with one, or through a `useMemo` of
 *  one. A call of one is read as the field it answers in the file that declares it and in every
 *  file that imports it (`rawDrawsOf`). A field named like the dashboard's own words makes no
 *  such function: what answers a `label` is a control's far more often than a record's. */
function proseAnswersOf(trees: readonly ts.SourceFile[]): Map<string, ProseAnswer> {
  const answers = new Map<string, ProseAnswer>();
  const answered = (
    expression: ts.Expression,
    locals: ReadonlyMap<string, string>
  ): string | undefined =>
    [...drawnLeaves(expression), ...memoAnswers(expression).flatMap(drawnLeaves)]
      .flatMap((leaf) => {
        if (ts.isPropertyAccessExpression(leaf)) {
          return [leaf.name.text];
        }
        const local = ts.isIdentifier(leaf) ? locals.get(leaf.text) : undefined;
        return local === undefined ? [] : [local];
      })
      .find((field) => STORED_PROSE_FIELDS.has(field) && !UI_WORD_FIELDS.includes(field));
  for (const tree of trees) {
    const answer = (owner: string | undefined, field: string | undefined): void => {
      if (owner !== undefined && field !== undefined) {
        answers.set(owner, { field, file: tree.fileName });
      }
    };
    // `owner` is the named function a `return` answers for: a declaration, or the constant an
    // arrow or a function expression initialises. A callback handed to a call answers for
    // nobody. `locals` are the function's own constants that hold a stored field.
    const visit = (node: ts.Node, owner: string | undefined, locals: Map<string, string>): void => {
      let inside = owner;
      let scope = locals;
      if (ts.isFunctionDeclaration(node)) {
        inside = node.name?.text;
        scope = new Map();
      } else if (ts.isArrowFunction(node) || ts.isFunctionExpression(node)) {
        const holder = node.parent;
        inside =
          ts.isVariableDeclaration(holder) && ts.isIdentifier(holder.name)
            ? holder.name.text
            : undefined;
        scope = new Map();
        if (ts.isArrowFunction(node) && !ts.isBlock(node.body)) {
          answer(inside, answered(node.body, scope));
        }
      } else if (ts.isFunctionLike(node)) {
        inside = undefined;
        scope = new Map();
      } else if (
        ts.isVariableDeclaration(node) &&
        ts.isIdentifier(node.name) &&
        node.initializer !== undefined
      ) {
        const field = answered(node.initializer, locals);
        if (field !== undefined) {
          locals.set(node.name.text, field);
        }
      } else if (ts.isReturnStatement(node) && node.expression !== undefined) {
        answer(owner, answered(node.expression, locals));
      }
      ts.forEachChild(node, (child) => visit(child, inside, scope));
    };
    visit(tree, undefined, new Map());
  }
  return answers;
}

/**
 * `tree`'s raw draws of a stored text: a field that holds one (`STORED_PROSE_FIELDS`) drawn as a
 * JSX child, as the value of an attribute the browser shows as characters - any of
 * `PLAIN_ATTRIBUTES`, and on an element of the page any other but the few that show nothing of
 * their value (`UNDRAWN_ATTRIBUTES`) - or into a template literal, outside the text a construct
 * is handed (`handedToConstruct`). A `className` is a list of classes rather than text, so
 * nothing in one is a draw, and neither is a member of a name the theme module exports
 * (`importsOf`). A field named like the dashboard's own words is a stored text only on a value
 * reached through a field that holds it (`UI_WORD_FIELDS`).
 *
 * The field is read where it is written, as what a function that answers one hands back
 * (`answers`), as the argument of any other call whose answer is drawn - a call of the
 * projection is the projection's line - and through a light trace inside one file: a constant
 * initialised with it or with a `useMemo` of it, a binding that destructures it, the parameter
 * of a `map` over a list of it, a method whose answer is still its text (`TEXT_METHODS`), a join
 * with `+`, and a component's prop - destructured under any name, or read off its props - where
 * a mount of that component in the same file hands it one. A prop no mount in its file hands a
 * stored text is left alone: a shared control's words are the app's own far more often than a
 * writer's. Whatever a projection hook answers is its line rather than the text, so nothing
 * read from it is a raw draw. What the walk does not see is the test's header's list.
 */
function rawDrawsOf(tree: ts.SourceFile, answers: ReadonlyMap<string, ProseAnswer>): RawDraw[] {
  const file = tree.fileName;
  const imports = importsOf(tree);
  const tables = wordTablesOf(tree);
  /** Each function's names that hold a stored text's field, or the projection's line of one. */
  const held = new Map<ts.Node, Map<string, Held>>();
  const hold = (scope: ts.Node, name: string, what: Held): void => {
    const names = held.get(scope) ?? new Map<string, Held>();
    names.set(name, what);
    held.set(scope, names);
  };
  const heldAs = (name: ts.Identifier): Held | undefined => {
    for (let at: ts.Node | undefined = name.parent; at !== undefined; at = at.parent) {
      const what = held.get(at)?.get(name.text);
      if (what !== undefined) {
        return what;
      }
    }
    return undefined;
  };
  /** The holder of a field named like the dashboard's own words that `expression` is reached
   *  through: a member of that name on its chain, through every `Cast`, or a name held as
   *  reached through one. */
  const throughOf = (expression: ts.Expression): string | undefined => {
    if (ts.isPropertyAccessExpression(expression) && UI_WORD_HOLDERS.has(expression.name.text)) {
      return expression.name.text;
    }
    if (
      isCast(expression) ||
      ts.isPropertyAccessExpression(expression) ||
      ts.isElementAccessExpression(expression) ||
      ts.isCallExpression(expression)
    ) {
      return throughOf(expression.expression);
    }
    if (
      ts.isBinaryExpression(expression) &&
      expression.operatorToken.kind === ts.SyntaxKind.QuestionQuestionToken
    ) {
      return throughOf(expression.left);
    }
    const what = ts.isIdentifier(expression) ? heldAs(expression) : undefined;
    return typeof what === "object" && "through" in what ? what.through : undefined;
  };
  /** Whether `field` read off `receiver` is a stored text: any stored field is, but one named
   *  like the dashboard's own words only on a value reached through a field that holds it. */
  const storedOn = (field: string, receiver: ts.Expression): boolean =>
    !UI_WORD_FIELDS.includes(field) ||
    CONTRACT_TEXTS.holders.get(field)?.has(throughOf(receiver) ?? "") === true;
  /** What each mount in the file hands each prop, by `<component>.<prop>`. */
  const handed = new Map<string, ts.Expression[]>();
  /** The props being read, so a component that hands its own prop on to itself ends the read. */
  const reading = new Set<string>();
  /** The field a mount in the file hands `component`'s `prop`, where one hands it a stored text. */
  const handedField = (component: string | undefined, prop: string): string | undefined => {
    const key = `${component}.${prop}`;
    if (reading.has(key)) {
      return undefined;
    }
    reading.add(key);
    const field = (handed.get(key) ?? [])
      .map((value) => firstFieldOf(value))
      .find((found) => found !== undefined);
    reading.delete(key);
    return field;
  };
  /** The record a name holds (`PROSE_RECORDS`), or `undefined` for any other value. */
  const recordHeld = (name: ts.Identifier): string | undefined => {
    const what = heldAs(name);
    return typeof what === "object" && "record" in what ? what.record : undefined;
  };
  /** The stored text a member chain off a record reads (`PROSE_RECORDS`): the path of the field
   *  it reads, as `reading.tree` of `chip.reading?.tree`, where that field holds one. */
  const recordField = (chain: MemberChain, record: string): string | undefined => {
    const { path, root } = chain;
    if (root === undefined || path.some((key) => key === undefined)) return undefined;
    const read = path.join(".");
    return PROSE_RECORDS.get(record)?.some((field) => `${read}.`.startsWith(`${field}.`))
      ? read
      : undefined;
  };
  const fieldOf = (leaf: ts.Expression): string | undefined => {
    if (ts.isIdentifier(leaf)) {
      const what = heldAs(leaf);
      // A record handed whole holds its stored text, which a call whose answer is drawn draws.
      return typeof what === "string" ? what : recordHeld(leaf);
    }
    if (ts.isCallExpression(leaf) && !isProjection(leaf)) {
      const name = ts.isIdentifier(leaf.expression) ? leaf.expression.text : "";
      const answer = answers.get(name);
      if (answer !== undefined && (answer.file === file || imports.has(name))) {
        return answer.field;
      }
      // Any other call whose answer is drawn draws what it is handed: a text cut, tidied or
      // worded around by a helper is still that text's characters. A portal draws its children
      // alone: its second argument is the node it draws them into, such as `document.body`.
      const callee = ts.isPropertyAccessExpression(leaf.expression)
        ? leaf.expression.name.text
        : name;
      const drawn = callee === "createPortal" ? leaf.arguments.slice(0, 1) : leaf.arguments;
      return [...drawn, ...memoAnswers(leaf)]
        .map((argument) => firstFieldOf(argument))
        .find((field) => field !== undefined);
    }
    if (!ts.isPropertyAccessExpression(leaf)) {
      return undefined;
    }
    // The chain the leaf reads, read once and handed to each rule below that reads it.
    const chain = memberChain(leaf);
    const root = chain.root;
    const what = root === undefined ? undefined : heldAs(root);
    const record = root === undefined ? undefined : recordHeld(root);
    const recorded = record === undefined ? undefined : recordField(chain, record);
    if (recorded !== undefined) {
      return recorded;
    }
    // A component's own props, read off the object they come in: what a mount hands the prop.
    if (typeof what === "object" && "props" in what && leaf.expression === root) {
      const given = handedField(what.props, leaf.name.text);
      if (given !== undefined) {
        return given;
      }
    }
    if (!STORED_PROSE_FIELDS.has(leaf.name.text)) {
      return undefined;
    }
    // A member of a table of words the file writes out is the file's own words (`readsWordTable`).
    if (what === undefined && readsWordTable(chain, tables)) {
      return undefined;
    }
    // What the projection answered is its line, whatever member of the answer holds it, and a
    // member of a theme name is a class.
    if (what === PROJECTED) {
      return undefined;
    }
    if (imports.get(root?.text ?? "")?.includes("/theme/") === true) {
      return undefined;
    }
    return storedOn(leaf.name.text, leaf.expression) ? leaf.name.text : undefined;
  };
  const firstFieldOf = (expression: ts.Expression): string | undefined =>
    drawnLeaves(expression)
      .map(fieldOf)
      .find((field) => field !== undefined);

  const readMounts = (node: ts.Node): void => {
    if (ts.isJsxOpeningElement(node) || ts.isJsxSelfClosingElement(node)) {
      for (const property of node.attributes.properties) {
        if (!ts.isJsxAttribute(property)) {
          continue;
        }
        const value = property.initializer;
        if (value !== undefined && ts.isJsxExpression(value) && value.expression !== undefined) {
          const key = `${node.tagName.getText(tree)}.${property.name.getText(tree)}`;
          handed.set(key, [...(handed.get(key) ?? []), value.expression]);
        }
      }
    }
    ts.forEachChild(node, readMounts);
  };
  readMounts(tree);
  // The trace reads the file in the order it is written, so a constant read from another is
  // held as the field the first was.
  const trace = (node: ts.Node): void => {
    if (ts.isVariableDeclaration(node) && node.initializer !== undefined) {
      if (isProjection(node.initializer)) {
        for (const name of boundNames(node.name)) {
          hold(scopeOf(node), name, PROJECTED);
        }
      } else if (ts.isIdentifier(node.name)) {
        const memo = memoAnswers(node.initializer);
        const field = firstFieldOf(node.initializer) ?? memo.map(firstFieldOf).find(Boolean);
        const through = throughOf(node.initializer);
        if (field !== undefined) {
          hold(scopeOf(node), node.name.text, field);
        } else if (through !== undefined) {
          hold(scopeOf(node), node.name.text, { through });
        }
      }
    }
    // A component's props taken whole, under the parameter's own name. A component's name is
    // written in capitals, which tells it from a function whose first parameter is a value.
    if (
      ts.isParameter(node) &&
      ts.isIdentifier(node.name) &&
      node.parent.parameters[0] === node &&
      /^[A-Z]/.test(ownerName(node) ?? "")
    ) {
      hold(node.parent, node.name.text, { props: ownerName(node) ?? "" });
    }
    // A value declared as one of the records that carry a stored text, as `node: TreeNode`.
    if (ts.isParameter(node) && ts.isIdentifier(node.name)) {
      const record = recordOf(node.type);
      if (record !== undefined) {
        hold(node.parent, node.name.text, { record });
      }
    }
    if (
      ts.isBindingElement(node) &&
      ts.isIdentifier(node.name) &&
      ts.isObjectBindingPattern(node.parent)
    ) {
      const key = node.propertyName ?? node.name;
      const field = ts.isIdentifier(key) || ts.isStringLiteral(key) ? key.text : undefined;
      const root = bindingRoot(node);
      // A variable bound from the projection holds its line. A prop holds a stored text only
      // where a mount of its component in this file hands it one, under whatever name the prop
      // has: it holds the field that mount hands it.
      if (ts.isParameter(root)) {
        const given = field === undefined ? undefined : handedField(ownerName(root), field);
        const record =
          field === undefined || node.parent !== root.name
            ? undefined
            : declaredRecord(root, field);
        if (given !== undefined) {
          hold(scopeOf(node), node.name.text, given);
        } else if (record !== undefined) {
          hold(scopeOf(node), node.name.text, { record });
        }
      } else if (
        field !== undefined &&
        ts.isVariableDeclaration(root) &&
        root.initializer !== undefined &&
        ts.isIdentifier(root.initializer) &&
        node.parent === root.name &&
        PROSE_RECORDS.get(recordHeld(root.initializer) ?? "")?.includes(field) === true
      ) {
        // A field that holds the stored text, destructured off a record: `{ text } = node`.
        hold(scopeOf(node), node.name.text, field);
      } else if (
        field !== undefined &&
        STORED_PROSE_FIELDS.has(field) &&
        ts.isVariableDeclaration(root) &&
        (root.initializer === undefined
          ? !UI_WORD_FIELDS.includes(field)
          : !isProjection(root.initializer) && storedOn(field, root.initializer))
      ) {
        hold(scopeOf(node), node.name.text, field);
      }
    }
    if (
      ts.isCallExpression(node) &&
      ts.isPropertyAccessExpression(node.expression) &&
      MAPPING_METHODS.includes(node.expression.name.text)
    ) {
      const [callback] = node.arguments;
      const list = node.expression.expression;
      const field = firstFieldOf(list);
      const through = throughOf(list);
      const [item] =
        callback !== undefined &&
        (ts.isArrowFunction(callback) || ts.isFunctionExpression(callback))
          ? callback.parameters
          : [];
      if (callback !== undefined && item !== undefined && ts.isIdentifier(item.name)) {
        if (field !== undefined) {
          hold(callback, item.name.text, field);
        } else if (through !== undefined) {
          hold(callback, item.name.text, { through });
        }
      }
    }
    ts.forEachChild(node, trace);
  };

  const draws: RawDraw[] = [];
  const report = (expression: ts.Expression, pattern: string): void => {
    if (handedToConstruct(expression, tree)) {
      return;
    }
    // The attribute whose own value the draw is in: an element nested in that value is drawn
    // text of its own, which a comment above the attribute does not speak for.
    let attribute: ts.Node | undefined = expression;
    while (attribute !== undefined && !ts.isJsxAttribute(attribute)) {
      const nested: boolean =
        ts.isJsxElement(attribute) ||
        ts.isJsxSelfClosingElement(attribute) ||
        ts.isJsxFragment(attribute);
      attribute = nested ? undefined : attribute.parent;
    }
    const attributeLine = attribute === undefined ? undefined : lineOf(tree, attribute);
    for (const leaf of drawnLeaves(expression)) {
      const field = fieldOf(leaf);
      if (field !== undefined) {
        const line = lineOf(tree, leaf);
        draws.push({
          at: `${file}:${line}`,
          attributeLine,
          field,
          file,
          line,
          pattern,
          written: leaf.getText(tree).replace(/\s+/g, " "),
        });
      }
    }
  };
  const find = (node: ts.Node): void => {
    if (ts.isJsxAttribute(node) && node.name.getText(tree) === "className") {
      return;
    }
    if (
      ts.isJsxExpression(node) &&
      node.expression !== undefined &&
      (ts.isJsxElement(node.parent) || ts.isJsxFragment(node.parent))
    ) {
      report(node.expression, "a JSX child");
    } else if (
      ts.isJsxAttribute(node) &&
      node.initializer !== undefined &&
      ts.isJsxExpression(node.initializer) &&
      node.initializer.expression !== undefined
    ) {
      const name = node.name.getText(tree);
      const tag = node.parent.parent.tagName.getText(tree);
      if (PLAIN_ATTRIBUTES.includes(name)) {
        report(node.initializer.expression, `the ${name} attribute`);
      } else if (
        /^[a-z]/.test(tag) &&
        !UNDRAWN_ATTRIBUTES.includes(name) &&
        !name.startsWith("data-") &&
        !/^on[A-Z]/.test(name)
      ) {
        report(node.initializer.expression, `the ${name} attribute`);
      }
    } else if (ts.isTemplateSpan(node)) {
      report(node.expression, "a template literal");
    }
    ts.forEachChild(node, find);
  };

  // Twice, so a prop is held where the mount that hands it a stored text is written below the
  // component: the first pass holds what the mount hands, the second reads the prop by it.
  trace(tree);
  trace(tree);
  find(tree);
  return draws;
}

/** The dashboard's files the raw-draw walk reads: all of them but the reader's own. */
const DRAWING_TREES = [...TREE_BY_FILE.values()].filter(
  (tree) => tree.fileName.startsWith(`${RAW_DRAW_ROOT}/`) && !READER_MODULES.includes(tree.fileName)
);

const PROSE_ANSWERS = proseAnswersOf(DRAWING_TREES);

const RAW_DRAWS = DRAWING_TREES.flatMap((tree) => rawDrawsOf(tree, PROSE_ANSWERS));

/** The rows a raw draw may name as its excuse: the surfaces the list says are drawn some other
 *  way than the renderer, each with the reason why. */
const UNRENDERED_ROWS = DISPATCH_READING_SURFACES.filter((row) => !row.rendered);

const FEATURES = "packages/dispatch/web/src/features";

/**
 * The draws the walk finds that hold no writer's stored text: a value of the dashboard's own, or
 * a word the server or the broker writes, under a name a stored text also has - which the walk,
 * reading the syntax tree and not the types, cannot tell apart. Each is the file, the expression
 * exactly as written there, and what it holds. It is reviewed here, in one place, rather than
 * in a comment at the site: no comment silences a draw, so a stored text cannot be excused by a
 * reason no one reads beside the list. An entry the walk no longer finds fails
 * (`every entry of the list of fields that hold no stored text is still a draw the walk finds`),
 * so the list cannot outlive the code it speaks for.
 */
const NOT_STORED_TEXTS: readonly { file: string; written: string; holds: string }[] = [
  {
    file: "packages/dispatch/web/src/components/DisplayMenu.tsx",
    holds: "the menu's own word for one of its choices, the label of its radio button",
    written: "label",
  },
  {
    file: "packages/dispatch/web/src/components/DisplayMenu.tsx",
    holds: "the menu's own word for one of its choices, handed to the choice that draws it",
    written: "option.label",
  },
  {
    file: `${FEATURES}/agents/AgentsPage.tsx`,
    holds: "a count of the group's agents, from the page's own selection of sessions",
    written: "groupCountLabel(agents, selected)",
  },
  {
    file: `${FEATURES}/agents/AgentsPage.tsx`,
    holds: "a count of the sessions the reader selected",
    written: "selectionSummary(selection)",
  },
  {
    file: `${FEATURES}/agents/BroadcastComposer.tsx`,
    holds: "the composer's own word for why a selected session gets no broadcast",
    written: "note.reason",
  },
  {
    file: `${FEATURES}/conversation/MentionComposer.tsx`,
    holds: "the host's own words for why a kind of reference cannot be picked here",
    written: "kindRefusal.text",
  },
  {
    file: `${FEATURES}/conversation/held-sends.ts`,
    holds: "the words a mention was typed as, matched against the draft and drawn nowhere",
    written: "mention.text",
  },
  {
    file: `${FEATURES}/conversation/held-sends.ts`,
    holds: "the draft being rewritten with a mention's link, drawn nowhere",
    written: "link",
  },
  {
    file: `${FEATURES}/credentials/MachineLoginsSection.tsx`,
    holds: "a login's subject, the Kubernetes identity its token names",
    written: "login.subject",
  },
  {
    file: `${FEATURES}/delivery/SourceFreshness.tsx`,
    holds: "the page's own line on how fresh a source is",
    written: "row.text",
  },
  {
    file: `${FEATURES}/delivery/lib/freshness.ts`,
    holds: "the page's own line on how fresh a source is",
    written: "live.text",
  },
  {
    file: `${FEATURES}/inbox/AskHandOn.tsx`,
    holds: "the waiting-on badge's text colour, a class of the theme's badge tone",
    written: "tone.text",
  },
  {
    file: `${FEATURES}/inbox/ComposeDialog.tsx`,
    holds: "the server's or the send's own words for why a broadcast leaves a recipient out",
    written: "outcome.reason",
  },
  {
    file: `${FEATURES}/inbox/useInboxSelection.ts`,
    holds: "the server's own words for why it refused a snooze",
    written: "first.reason",
  },
  {
    file: `${FEATURES}/issue-page/IssueActionsMenu.tsx`,
    holds:
      "the page's own words for one of the issue's actions, or for its write while it saves (`CONTROL_LABELS`, `ANSWER_LINES`), as the old header's Pin, Close and Reopen draw theirs",
    written: "action.text",
  },
  {
    file: `${FEATURES}/issue-page/NodeRow.tsx`,
    holds:
      "the page's own word for an answer, the label of a node's answer line (`CONTROL_LABELS`)",
    written: "CONTROL_LABELS.answer",
  },
  {
    file: `${FEATURES}/issue-page/NodeRow.tsx`,
    holds: "whether the reader picked this option: a boolean, drawn nowhere",
    written: "picks.includes(option.label)",
  },
  {
    file: `${FEATURES}/issue-page/issue-tree.ts`,
    holds:
      "the issue's title, joined to its key for the words the words filter matches and the batched name read's citations are read from, drawn nowhere (`nodeWords`)",
    written: "title",
  },
  {
    file: `${FEATURES}/issue-page/useTreeKeymap.ts`,
    holds: "the page's own word for an answer, in the label of the answer key (`CONTROL_LABELS`)",
    written: "CONTROL_LABELS.answer",
  },
  {
    file: `${FEATURES}/issue/AssigneeControl.tsx`,
    holds: "the assignee badge's text colour, a class of the theme's badge tone",
    written: "tone.text",
  },
  {
    file: `${FEATURES}/issue/PriorityControl.tsx`,
    holds: "the priority badge's text colour, a class of the theme's badge tone",
    written: "tone.text",
  },
  {
    file: `${FEATURES}/issue/RelatedItems.tsx`,
    holds: "the server's own words for why no related-item check was made",
    written: "notMadeText(check.reason, title, check.searchQuery)",
  },
  {
    file: `${FEATURES}/issue/decision-text.ts`,
    holds: "a tree's words joined to match an anchor quote against, drawn nowhere",
    written: "current.text",
  },
  {
    file: `${FEATURES}/issue/event-description.ts`,
    holds: "the server's own words for why it refused a check block or what is malformed in one",
    written: "payload.reason",
  },
  {
    file: `${FEATURES}/machines/MachinePage.tsx`,
    holds: "a machine's counts of its sessions",
    written: "sessionCountSentence(model.summary)",
  },
  {
    file: `${FEATURES}/plan/PlanTab.tsx`,
    holds:
      "a block pointer's name by the naming rule (`useReferenceLabel`): its target's title, which a block of a document holds, never a post's",
    written: "text",
  },
  {
    file: `${FEATURES}/priorities/PriorityDescription.tsx`,
    holds: "the documents' editor's own error for a description it could not draw",
    written: "failed",
  },
  {
    file: `${FEATURES}/project/IssuesTable.tsx`,
    holds: "the table's own notice of what a table action did",
    written: "notice?.text",
  },
  {
    file: `${FEATURES}/project/issues-table-actions.ts`,
    holds:
      "the table's own notice of what a table action did, said before its line for a list read that has not settled",
    written: "notice.text",
  },
  {
    file: `${FEATURES}/refs/RefLink.tsx`,
    holds:
      "a reference's name by the naming rule (`useReferenceLabel`): its target's title, cut and marked edited, or the page's own deleted line",
    written: "named",
  },
  {
    file: `${FEATURES}/refs/RefPickerList.tsx`,
    holds: "the picker's own name for a group of rows",
    written: "group.heading",
  },
  {
    file: `${FEATURES}/verification/VerificationTab.tsx`,
    holds:
      "the page's words for the server's reason it cannot say where a result's commit stands, a word of its model the page words, or shows as given when it is new",
    written: "reasonWords(stands.reason, stands.repo)",
  },
];

/** Whether `draw` is one the list of fields that hold no stored text names. */
function holdsNoStoredText(draw: RawDraw): boolean {
  return NOT_STORED_TEXTS.some(
    (entry) => entry.file === draw.file && entry.written === draw.written
  );
}

/** Whether `line` writes `word` whole: `write-label` and not `write-labels`. */
function writesWhole(line: string, word: string): boolean {
  return new RegExp(`(?<![\\w-])${word}(?![\\w-])`).test(line);
}

/** The draw's label where a comment beside it excuses it - on its own line, on the line above,
 *  or on the line above the JSX attribute it is written in - or where it holds no stored text
 *  (`NOT_STORED_TEXTS`), and what is wrong where neither does. An excuse names a `rendered:
 *  false` row whose files hold this one by its id: the row's reason is then the excuse, as an
 *  excluded row's pointer comment works. Words of any other kind excuse nothing, so a reason no
 *  row of the list holds cannot silence a draw. */
function drawComplaint(draw: RawDraw, text: string): string {
  const label = drawLabel(draw);
  const lines = text.split("\n");
  const near = [draw.line - 1, draw.line, (draw.attributeLine ?? draw.line) - 1].map(
    (line) => lines[line - 1] ?? ""
  );
  const excused =
    holdsNoStoredText(draw) ||
    near.some((line) =>
      UNRENDERED_ROWS.some((row) => row.files.includes(draw.file) && writesWhole(line, row.surface))
    );
  return excused
    ? label
    : `${label}: a stored text drawn as its characters, by no construct and under no row of DISPATCH_READING_SURFACES`;
}

// ------------------------------------------------------------------------------------------
// The trees
// ------------------------------------------------------------------------------------------

/** The constructs that draw a stored text from the reading the server sent of it rather than
 *  parsing it on the page's thread: an element takes that reading as its `tree` and the text as
 *  its `markdown`; the hook takes the tree as its first argument and the text as its second. */
const TREE_TAKERS: ReadonlySet<string> = new Set([
  "MarkdownBody",
  "MarkdownPreview",
  "PlainText",
  "PostBody",
  "PostPreview",
  "usePlainHeadline",
]);

/** A row of the reading list whose text reaches the page with no tree the server read of it,
 *  and why. `text`, where it is given, is the one text the row excuses, as its mounts write it: a
 *  mount on the row handing any other stored text with no tree of it fails, as a mount on no row
 *  of this list does. */
interface UntreedRow {
  readonly surface: DispatchReadingSurface;
  readonly text?: string;
  readonly why: string;
}

/**
 * The rows of the reading list whose text reaches the page with no tree the server read of it,
 * so a mount on one hands its construct none and the page reads the text itself: the one excuse
 * the tree check takes, by the row's id. A row here whose every mount hands a tree fails, so the
 * list cannot outlive the mounts it speaks for.
 */
const UNTREED_ROWS: readonly UntreedRow[] = [
  {
    surface: "agent-turn-text",
    why: "A session's live transcript, which Dispatch stores nothing of, so the server has no text to read a tree of.",
  },
  {
    surface: "agent-turn-reasoning",
    why: "The reasoning beside a live turn, which is that same transcript.",
  },
  {
    surface: "agent-held-send",
    why: "A message the server has not stored yet: the page parses it, as it parses every write the server has not stored.",
  },
  {
    surface: "github-reference-card",
    why: "A GitHub issue's or pull request's body, read from GitHub's API, which Dispatch does not store.",
  },
  {
    surface: "broadcast-send-row",
    text: "row.send.input.body",
    why: "A send not made yet - queued, on the wire, late or failed - has no stored text the page holds: Dispatch has stored nothing of it, or its answer is a refusal or never came. Its row draws the draft it holds (`row.send.input.body`), which the page parses, as it parses every write the server has not stored. A made broadcast's row draws the body Dispatch stored, from the answer the write landed with, beside that answer's tree (`row.created.body`, `row.created.body_tree`), and this row does not excuse it.",
  },
  {
    surface: "margin-suggestion-diff",
    why: "A suggestion's two sides are drawn inside a diff (`ReferenceHostIs host=\"diff\"`), where the renderer writes the characters it is handed and parses nothing (`hostDrawsMarkup`), so no tree is needed. The mounts are main's own (`margin/ThreadCard.tsx`).",
  },
];

/** Where a text or a tree a mount hands on comes from, where it is the props of the component the
 *  mount is in: the prop, and the member of it for a prop that is an object holding the text
 *  (`quote.excerpt`, `quote.excerptTree`). */
interface PropRef {
  readonly prop: string;
  readonly member?: string;
}

/** A component that hands a construct, or another such component, a text and the tree of it
 *  that it is handed itself: the props that carry each. Every mount of it then hands the tree
 *  wherever it hands the text, as a mount of the construct does. */
interface TreePair {
  readonly text: PropRef;
  readonly tree: PropRef;
}

/** One mount the tree check reads: where it is, the row its `surface` names for a construct's
 *  own mount, whether it hands a stored text and no tree of it, the row of `UNTREED_ROWS` that
 *  excuses that (`excusedBy`), and the complaint that makes where no row excuses it. */
interface TreeMount {
  readonly at: string;
  readonly complaint: string | undefined;
  readonly excusedBy: DispatchReadingSurface | undefined;
  readonly surface: string | undefined;
  readonly untreed: boolean;
}

/** An element's attributes as a mount hands them: each with the expression it holds (a string
 *  literal for one written `name="..."`), and whether it spreads anything. */
interface MountAttributes {
  readonly spread: boolean;
  readonly values: ReadonlyMap<string, ts.Expression | undefined>;
}

/** One text a mount hands, with every tree it hands beside it. */
interface HandedPair {
  readonly text: Handed;
  readonly trees: readonly Handed[];
}

/** What a mount hands a construct or a component for one text or tree: nothing, a string written
 *  out (no stored text), a value the walk reads, or one it cannot see into - a spread, or a
 *  value that is no object literal where the walk needs a member of one. */
type Handed =
  | { readonly kind: "none" }
  | { readonly kind: "literal" }
  | { readonly kind: "unseen" }
  | { readonly kind: "value"; readonly value: ts.Expression };

/** What `expression` hands as a text or a tree. */
function handedValue(expression: ts.Expression | undefined): Handed {
  if (
    expression === undefined ||
    (ts.isIdentifier(expression) && expression.text === "undefined")
  ) {
    return { kind: "none" };
  }
  if (ts.isStringLiteral(expression) || ts.isNoSubstitutionTemplateLiteral(expression)) {
    return { kind: "literal" };
  }
  return { kind: "value", value: expression };
}

/** The component `node` is written in: the nearest function whose name is written in capitals,
 *  read through the callbacks inside it, with its props as its first parameter binds them. */
interface Component {
  readonly name: string;
  readonly body: ts.Node;
  /** Each name a destructured prop is bound to, with the prop it holds. */
  readonly locals: ReadonlyMap<string, string>;
  /** The parameter's own name where the props are taken whole. */
  readonly whole: string | undefined;
}

function componentOf(node: ts.Node): Component | undefined {
  for (let at: ts.Node | undefined = node.parent; at !== undefined; at = at.parent) {
    let name: string | undefined;
    if (ts.isFunctionDeclaration(at)) {
      name = at.name?.text;
    } else if (
      (ts.isArrowFunction(at) || ts.isFunctionExpression(at)) &&
      ts.isVariableDeclaration(at.parent) &&
      ts.isIdentifier(at.parent.name)
    ) {
      name = at.parent.name.text;
    }
    if (name === undefined || !/^[A-Z]/.test(name) || !ts.isFunctionLike(at)) {
      continue;
    }
    const [props] = at.parameters;
    const locals = new Map<string, string>();
    let whole: string | undefined;
    if (props !== undefined && ts.isObjectBindingPattern(props.name)) {
      for (const element of props.name.elements) {
        const key = element.propertyName ?? element.name;
        if (ts.isIdentifier(element.name) && (ts.isIdentifier(key) || ts.isStringLiteral(key))) {
          locals.set(element.name.text, key.text);
        }
      }
    } else if (props !== undefined && ts.isIdentifier(props.name)) {
      whole = props.name.text;
    }
    return { body: at, locals, name, whole };
  }
  return undefined;
}

/** The constant of `component` named `name`, where one is written in it. */
function constantIn(component: Component, name: string): ts.Expression | undefined {
  let found: ts.Expression | undefined;
  const visit = (node: ts.Node): void => {
    if (
      found === undefined &&
      ts.isVariableDeclaration(node) &&
      ts.isIdentifier(node.name) &&
      node.name.text === name &&
      node.initializer !== undefined
    ) {
      found = node.initializer;
    }
    ts.forEachChild(node, visit);
  };
  visit(component.body);
  return found;
}

/** The props of `component` that `expression` hands on, through what it draws (`drawnLeaves`) and
 *  the component's own constants, in the order it reads them. */
function propsHanded(expression: ts.Expression, component: Component, depth = 0): PropRef[] {
  return drawnLeaves(expression).flatMap((leaf): PropRef[] => {
    if (ts.isIdentifier(leaf)) {
      const prop = component.locals.get(leaf.text);
      if (prop !== undefined) {
        return [{ prop }];
      }
      const constant = depth < 4 ? constantIn(component, leaf.text) : undefined;
      return constant === undefined ? [] : propsHanded(constant, component, depth + 1);
    }
    if (ts.isPropertyAccessExpression(leaf) && ts.isIdentifier(leaf.expression)) {
      const owner = leaf.expression.text;
      if (owner === component.whole) {
        return [{ prop: leaf.name.text }];
      }
      const prop = component.locals.get(owner);
      return prop === undefined ? [] : [{ member: leaf.name.text, prop }];
    }
    return [];
  });
}

/** The attributes a mount's element names, each with the expression it holds (a string literal
 *  for one written `name="..."`), and whether it spreads anything. */
function attributesOf(element: ts.JsxOpeningLikeElement, tree: ts.SourceFile): MountAttributes {
  const values = new Map<string, ts.Expression | undefined>();
  let spread = false;
  for (const property of element.attributes.properties) {
    if (ts.isJsxSpreadAttribute(property)) {
      spread = true;
    } else if (ts.isJsxAttribute(property)) {
      const value = property.initializer;
      values.set(
        property.name.getText(tree),
        value === undefined ? undefined : ts.isJsxExpression(value) ? value.expression : value
      );
    }
  }
  return { spread, values };
}

/** What an object literal hands as `member`: the property it names, else what a spread of other
 *  object literals inside it hands (`...(on ? {} : { to })`), and a spread of anything else is
 *  unseen. */
function memberOf(literal: ts.ObjectLiteralExpression, member: string): Handed {
  const named = literal.properties.find(
    (property) => property.name !== undefined && property.name.getText() === member
  );
  if (named !== undefined) {
    return ts.isShorthandPropertyAssignment(named)
      ? { kind: "value", value: named.name }
      : handedValue(ts.isPropertyAssignment(named) ? named.initializer : undefined);
  }
  for (const spread of literal.properties.filter(ts.isSpreadAssignment)) {
    for (const leaf of drawnLeaves(spread.expression)) {
      if (!ts.isObjectLiteralExpression(leaf)) {
        return { kind: "unseen" };
      }
      const inner = memberOf(leaf, member);
      if (inner.kind !== "none") {
        return inner;
      }
    }
  }
  return { kind: "none" };
}

/** What an element hands for `ref`, once for each object it hands where `ref` names a member. */
function handedAt(attributes: MountAttributes, ref: PropRef): readonly Handed[] {
  if (attributes.spread) {
    return [{ kind: "unseen" }];
  }
  const value = attributes.values.get(ref.prop);
  if (ref.member === undefined || value === undefined) {
    return [handedValue(value)];
  }
  const member = ref.member;
  return drawnLeaves(value).map((leaf): Handed => {
    if (!ts.isObjectLiteralExpression(leaf)) {
      return handedValue(leaf).kind === "none" ? { kind: "none" } : { kind: "unseen" };
    }
    return memberOf(leaf, member);
  });
}

/** The text and the tree one mount hands, paired as the mount pairs them: one pair for each
 *  object it hands where both are members of one prop, and one pair of all it hands otherwise. */
function handedPairs(
  texts: readonly Handed[],
  trees: readonly Handed[],
  sameObject: boolean
): HandedPair[] {
  if (sameObject && texts.length === trees.length) {
    return texts.map((text, index) => ({ text, trees: [trees[index] as Handed] }));
  }
  return texts.map((text) => ({ text, trees }));
}

/** Each stored text a mount hands with no tree of it, as the mount writes it. */
function textsWithNoTree(pairs: readonly HandedPair[]): string[] {
  return pairs.flatMap((pair) =>
    pair.text.kind === "value" && pair.trees.every((tree) => tree.kind === "none")
      ? [pair.text.value.getText()]
      : []
  );
}

/** One mount as the tree check reads it, by `what`'s own words in its complaint: excused where
 *  its `surface` names a row of `UNTREED_ROWS` and that row names no text, or names the one text
 *  the mount hands with no tree. */
function treeMount(
  at: string,
  what: string,
  pairs: readonly HandedPair[],
  surface: string | undefined
): TreeMount {
  const texts = textsWithNoTree(pairs);
  const untreed = texts.length > 0;
  const excuse = untreed
    ? UNTREED_ROWS.find(
        (row) =>
          row.surface === surface &&
          (row.text === undefined || texts.every((text) => text === row.text))
      )
    : undefined;
  return {
    at,
    complaint:
      untreed && excuse === undefined
        ? `${at}: ${what} is handed a stored text and no tree of it, so the page parses that text on its own thread`
        : undefined,
    excusedBy: excuse?.surface,
    surface,
    untreed,
  };
}

/**
 * Every mount `trees` hold of a construct that draws from a tree (`TREE_TAKERS`), and of a
 * component that hands one a text and its tree from its own props, each with its complaint where
 * it hands a stored text and no tree of it. A text written out as a string is no stored text, and
 * a mount on a row of `UNTREED_ROWS` is excused by that row, for the text the row names where it
 * names one (`treeMount`). A component is found to hand a tree on where its mount's tree is read
 * from its own props (`propsHanded`), and is then held to it wherever it is mounted, by its name,
 * in any of the files; the walk runs until no component is added, so a component that hands its
 * tree to another one that hands it on is held too.
 */
function treeMountsOf(trees: readonly ts.SourceFile[]): TreeMount[] {
  const pairs = new Map<string, TreePair[]>();
  const learn = (
    node: ts.Node,
    texts: readonly Handed[],
    treesHanded: readonly Handed[]
  ): boolean => {
    const component = componentOf(node);
    if (component === undefined) {
      return false;
    }
    let added = false;
    texts.forEach((text, index) => {
      const tree = treesHanded[index] ?? treesHanded[0];
      if (text.kind !== "value" || tree === undefined || tree.kind !== "value") {
        return;
      }
      const textProps = propsHanded(text.value, component);
      const treeProps = propsHanded(tree.value, component);
      treeProps.forEach((treeProp, at) => {
        const textProp = textProps[at];
        const known = pairs.get(component.name) ?? [];
        if (
          textProp === undefined ||
          known.some(
            (pair) =>
              pair.tree.prop === treeProp.prop &&
              pair.tree.member === treeProp.member &&
              pair.text.prop === textProp.prop &&
              pair.text.member === textProp.member
          )
        ) {
          return;
        }
        pairs.set(component.name, [...known, { text: textProp, tree: treeProp }]);
        added = true;
      });
    });
    return added;
  };
  let mounts: TreeMount[] = [];
  let learning = true;
  for (let round = 0; learning && round < 8; round += 1) {
    learning = false;
    mounts = [];
    for (const tree of trees) {
      const visit = (node: ts.Node): void => {
        const at = `${tree.fileName}:${ts.isSourceFile(node) ? 0 : lineOf(tree, node)}`;
        if (ts.isJsxOpeningElement(node) || ts.isJsxSelfClosingElement(node)) {
          const tag = node.tagName.getText(tree);
          const attributes = attributesOf(node, tree);
          if (TREE_TAKERS.has(tag)) {
            const surface = surfaceOf(attributes.values.get("surface"));
            const texts = handedAt(attributes, { prop: "markdown" });
            const treesHanded = handedAt(attributes, { prop: "tree" });
            learning = learn(node, texts, treesHanded) || learning;
            mounts.push(
              treeMount(
                at,
                tag,
                handedPairs(texts, treesHanded, false),
                typeof surface === "string" ? surface : undefined
              )
            );
          }
          for (const pair of pairs.get(tag) ?? []) {
            const texts = handedAt(attributes, pair.text);
            const treesHanded = handedAt(attributes, pair.tree);
            const sameObject = pair.text.prop === pair.tree.prop && pair.text.member !== undefined;
            learning = learn(node, texts, treesHanded) || learning;
            const handed = `${pair.text.prop}${pair.text.member === undefined ? "" : `.${pair.text.member}`}`;
            mounts.push(
              treeMount(
                at,
                `${tag}'s ${handed}`,
                handedPairs(texts, treesHanded, sameObject),
                undefined
              )
            );
          }
        } else if (
          ts.isCallExpression(node) &&
          ts.isIdentifier(node.expression) &&
          TREE_TAKERS.has(node.expression.text)
        ) {
          const [treeArgument, textArgument] = node.arguments;
          const surface = hookSurface(node);
          const texts = [handedValue(textArgument)];
          const treesHanded = [handedValue(treeArgument)];
          learning = learn(node, texts, treesHanded) || learning;
          mounts.push(
            treeMount(
              at,
              node.expression.text,
              handedPairs(texts, treesHanded, false),
              typeof surface === "string" ? surface : undefined
            )
          );
        }
        ts.forEachChild(node, visit);
      };
      visit(tree);
    }
  }
  return mounts;
}

const TREE_MOUNTS = treeMountsOf(DRAWING_TREES);

/** Each row of `UNTREED_ROWS`, or what is wrong with it: a row the reading list does not hold, or
 *  one with no mount left that it excuses, one that hands its construct no tree. */
function untreedRowComplaint(surface: DispatchReadingSurface): string {
  if (!DISPATCH_READING_SURFACES.some((row) => row.surface === surface)) {
    return `${surface} is no row of DISPATCH_READING_SURFACES`;
  }
  return TREE_MOUNTS.some((mount) => mount.excusedBy === surface)
    ? surface
    : `${surface} has no mount left that hands its construct no tree`;
}

/**
 * Whether `tree` hands the stored text in `field` to one call or element beside its tree as
 * `written` writes it: one argument or attribute that reads the field, and another that reads
 * the tree - `eventProse(event, "payload.ask_question", event.payload.ask_question)`,
 * `<PlainText markdown={candidate.first_line} tree={candidate.first_line_tree}>`. A value reads
 * a name where a member of that name, a name bound as it, or a string written as it is among
 * what the value hands on (`drawnLeaves`). A text handed with no tree, or with its tree in
 * another call, is not.
 */
function handsWithTree(tree: ts.SourceFile, field: string, written: string): boolean {
  const reads = (value: ts.Expression, name: string): boolean =>
    drawnLeaves(value).some(
      (leaf) =>
        (ts.isPropertyAccessExpression(leaf) && leaf.name.text === name) ||
        (ts.isIdentifier(leaf) && leaf.text === name) ||
        (ts.isStringLiteral(leaf) && leaf.text === name)
    );
  let found = false;
  const visit = (node: ts.Node): void => {
    const handed = ts.isCallExpression(node)
      ? [...node.arguments]
      : ts.isJsxOpeningElement(node) || ts.isJsxSelfClosingElement(node)
        ? [...attributesOf(node, tree).values.values()].flatMap((value) =>
            value === undefined ? [] : [value]
          )
        : [];
    const texts = handed.filter((value) => reads(value, field));
    const trees = handed.filter((value) => reads(value, written));
    found ||= texts.some((text) => trees.some((reading) => reading !== text));
    if (!found) {
      ts.forEachChild(node, visit);
    }
  };
  visit(tree);
  return found;
}

/** The gate that holds the `dispatch_references` filter, which starts the reference rows on a
 *  pull request: an EXCLUDED filter, kept by hand (`excluded(filter, why, patterns)`), whose
 *  patterns CI's filter engine writes as they are written there into the filters file the
 *  `changes` job's paths-filter step reads, a file made at run time that no commit holds. */
const REFERENCE_GATE = ".github/ci-gates/dispatch-references.py";

/** A quoted string of Python source at `start`, whose quote is `source[start]`, read to its
 *  closing quote with each escape taken as written: the string's characters and where the source
 *  goes on after it, or `undefined` where `start` holds no quote or the string never closes. */
function pythonString(
  source: string,
  start: number
): { readonly value: string; readonly end: number } | undefined {
  const quote = source[start];
  if (quote !== "'" && quote !== '"') return undefined;
  let value = "";
  for (let at = start + 1; at < source.length; at++) {
    const character = source[at];
    if (character === "\\") {
      value += source[at + 1] ?? "";
      at++;
    } else if (character === quote) {
      return { end: at + 1, value };
    } else {
      value += character;
    }
  }
  return undefined;
}

/**
 * The patterns of `dispatch_references`, read from its gate (`REFERENCE_GATE`): the quoted
 * strings of the list that is the third argument of its `excluded('dispatch_references', why,
 * [...])` call, its comments skipped, as CI's filter engine writes them. A gate that is absent,
 * that holds no such call, or whose call the reading cannot follow reads as no filter at all, so
 * the guard row below that reads the filter fails rather than this passing over nothing. The
 * paths-filter step reads its patterns with the `some-with-excludes` quantifier, so a `!` pattern
 * is an exclusion: a file is in the filter when a pattern without `!` matches it and no `!`
 * pattern does.
 */
function referenceFilter(): { include: Bun.Glob[]; exclude: Bun.Glob[] } {
  let source = "";
  try {
    source = readFileSync(join(REPO_ROOT, REFERENCE_GATE), "utf8");
  } catch {
    source = "";
  }
  const call = /\bexcluded\(\s*(['"])dispatch_references\1\s*,\s*/u.exec(source);
  const why = call === null ? undefined : pythonString(source, call.index + call[0].length);
  const opening = why === undefined ? null : /^\s*,\s*\[/u.exec(source.slice(why.end));
  const patterns: string[] = [];
  if (why !== undefined && opening !== null) {
    let at = why.end + opening[0].length;
    let closed = false;
    while (at < source.length && !closed) {
      const character = source[at] ?? "";
      if (character === "]") {
        closed = true;
      } else if (character === "#") {
        const lineEnd = source.indexOf("\n", at);
        at = lineEnd === -1 ? source.length : lineEnd;
        continue;
      } else if (character === "'" || character === '"') {
        const pattern = pythonString(source, at);
        if (pattern === undefined) break;
        patterns.push(pattern.value);
        at = pattern.end;
        continue;
      }
      at++;
    }
    if (!closed) patterns.length = 0;
  }
  return {
    include: patterns.filter((pattern) => !pattern.startsWith("!")).map((p) => new Bun.Glob(p)),
    exclude: patterns
      .filter((pattern) => pattern.startsWith("!"))
      .map((pattern) => new Bun.Glob(pattern.slice(1))),
  };
}

const REFERENCE_FILTER = referenceFilter();

/** Whether a pull request that changes `file` starts the reference rows. */
function inReferenceFilter(file: string): boolean {
  return (
    REFERENCE_FILTER.include.some((glob) => glob.match(file)) &&
    !REFERENCE_FILTER.exclude.some((glob) => glob.match(file))
  );
}

/** The file, or the file and the list it is missing from: a failure a person can act on
 *  without opening anything first. */
function listing(file: string, named: ReadonlySet<string>, list: string): string {
  return named.has(file) ? file : `${file} is in no row of ${list}`;
}

/** The row's name, or what is wrong with its reason and which files to look in. A row that is
 *  not covered, or that does not render a record, carries one; a row that does carries none,
 *  so a reason cannot sit beside a box or a surface it does not excuse. */
function reasonComplaint(
  label: string,
  covered: boolean,
  reason: string | undefined,
  files: readonly string[]
): string {
  if (covered) {
    return reason === undefined ? label : `${label} needs no reason and carries one`;
  }
  return (reason ?? "") === ""
    ? `${label} is excluded and says no why: ${files.join(", ")}`
    : label;
}

// ------------------------------------------------------------------------------------------
// The walk is not vacuous
// ------------------------------------------------------------------------------------------

describe("the walk reads the dashboard's source", () => {
  test("scans a non-trivial number of source files", () => {
    expect(SCANNED_FILES.length).toBeGreaterThan(100);
  });

  test("reads both packages", () => {
    for (const root of SCANNED_ROOTS) {
      expect(SCANNED_FILES.filter((file) => file.startsWith(`${root}/`)).length).toBeGreaterThan(5);
    }
  });

  test("finds a non-trivial number of boxes and surfaces", () => {
    // A guard on the patterns themselves: were one to stop matching, every check below would
    // pass over nothing at all.
    expect(WRITING_SITES.length).toBeGreaterThan(15);
    expect(MOUNTS.length).toBeGreaterThan(40);
  });

  test("reads every stored text the contract gives a tree", () => {
    // A guard on the reading of the contract: were it to find no tree, the raw-draw walk would
    // look for the hand-written fields alone. `selected` is an answer's chosen labels, whose
    // trees are a list beside them (`selected_trees`) rather than one tree.
    expect(TREED_FIELDS).toEqual(
      expect.arrayContaining([
        "body",
        "description",
        "label",
        "markdown",
        "name",
        "prose",
        "question",
        "reason",
        "selected",
        "snippet",
        "subject",
        "text",
      ])
    );
  });

  test("reads exactly the fields of the contract that hold a label, a description or a name", () => {
    // A guard on the reading of the contract's holders, each field's whole set: a holder it
    // missed would leave that field read as a stored text on no value reached through it, and
    // one too many - a schema's own name, a key of an operation - would read it as one on any
    // value reached through a member so named.
    const holders: Readonly<Record<string, readonly string[]>> = {
      description: ["options", "priorities"],
      label: ["options"],
      name: ["linked_from", "links"],
    };
    const read: Readonly<Record<string, readonly string[]>> = Object.fromEntries(
      UI_WORD_FIELDS.map((field) => [field, [...(CONTRACT_TEXTS.holders.get(field) ?? [])].sort()])
    );
    expect(read).toEqual(holders);
  });

  test("looks for raw draws across the dashboard", () => {
    const read = SCANNED_FILES.filter((file) => file.startsWith(`${RAW_DRAW_ROOT}/`));
    expect(read.length).toBeGreaterThan(100);
  });

  test("skips only reader modules that are source files of the dashboard", () => {
    // A guard on the skip list: a reader module renamed or moved would leave a name here that
    // skips nothing, and its new file read as no reader at all.
    expect(READER_MODULES.filter((file) => !SCANNED_FILES.includes(file))).toEqual([]);
  });

  test("reads the reference rows' filter in its gate", () => {
    // A guard on the reading of the gate: were it to find no filter, no covered row's file would
    // be in it, and every covered row below would fail for a reason none of them has. The filter
    // the reading found names every spec file the reference rows' lane runs.
    expect(REFERENCE_FILTER.include.length).toBeGreaterThan(20);
    const lane = referencesLaneFiles().map((file) => `packages/dispatch/${file}`);
    expect(lane.length).toBeGreaterThan(10);
    expect(lane.filter((file) => !inReferenceFilter(file))).toEqual([]);
  });
});

// ------------------------------------------------------------------------------------------
// A mount's pick is read from its own attributes
// ------------------------------------------------------------------------------------------

describe("a mount's pick is read from its own attributes", () => {
  // A string's escaped quote ends neither the string nor the tag, so the element after a mount
  // is never read as part of it.
  const fixture = (field: string): string =>
    [
      "export const Box = () => (",
      "  <>",
      `    <MentionField label={"Quote \\" here"} ${field}body={draft} onChange={type} />`,
      '    <AskCard box="priority-description" />',
      "  </>",
      ");",
    ].join("\n");

  test("a box on the element after a mount is no pick of it", () => {
    expect([...picksOf("fixture.tsx", fixture("")).mounts]).toEqual([]);
  });

  test("a box on the mount itself is its pick", () => {
    expect([...picksOf("fixture.tsx", fixture('box="ask-answer" ')).mounts]).toEqual([
      "ask-answer",
    ]);
  });
});

// ------------------------------------------------------------------------------------------
// A mount's surface is read from its own attributes
// ------------------------------------------------------------------------------------------

describe("a mount's surface is read from its own attributes", () => {
  // As with a box: a string's escaped quote ends neither the string nor the tag, so the element
  // after a mount is never read as part of it.
  const fixture = (attribute: string): string =>
    [
      "export const Row = () => (",
      "  <>",
      `    <MarkdownBody markdown={"Quote \\" here"} ${attribute}source={source} />`,
      '    <AskCard surface="answers-subject" />',
      '    <MarkdownPreview lines={1} markdown="x" surface="answers-answer" />',
      "  </>",
      ");",
    ].join("\n");
  const mounts = (source: string): Mount[] => mountsOf(parseSource("fixture.tsx", source));

  test("a surface on the element after a mount is no surface of it", () => {
    expect(mounts(fixture("")).map((mount) => mount.surface)).toEqual([
      undefined,
      "answers-answer",
    ]);
  });

  test("a surface on the mount itself is its row", () => {
    expect(mounts(fixture('surface="answers-reply" ')).map((mount) => mount.surface)).toEqual([
      "answers-reply",
      "answers-answer",
    ]);
  });

  test("a mount with no surface fails, naming its file and line", () => {
    expect(mounts(fixture("")).map(mountComplaint)[0]).toBe(
      "fixture.tsx:3: a mount of the body or the preview renderer names no row of DISPATCH_READING_SURFACES in its surface"
    );
  });

  test("a mount naming a row that does not name its file fails", () => {
    expect(mounts(fixture('surface="answers-reply" ')).map(mountComplaint)[0]).toBe(
      "fixture.tsx:3: a mount of the body or the preview renderer names answers-reply, whose row does not name fixture.tsx"
    );
  });

  test("a hook's surface is read from its options, and a construct may hand its own on", () => {
    const source = [
      "export function useLine(ask: Ask) {",
      '  return usePlainHeadline(ask.question_tree, ask.question, { surface: "write-label" });',
      "}",
      "export function Line({ surface, text }: Props) {",
      "  return usePlainHeadline(undefined, text, { surface });",
      "}",
      "export function PlainText({ surface, text }: Props) {",
      "  return usePlainHeadline(undefined, text, { surface });",
      "}",
    ].join("\n");
    // `PlainText` is a construct handing its own surface on, so its call is no mount. `Line` is
    // none, so what it hands on names no row the walk can read.
    expect(mounts(source).map((mount) => `${mount.at} ${mount.surface}`)).toEqual([
      "fixture.tsx:2 write-label",
      "fixture.tsx:5 undefined",
    ]);
  });

  // A `#id` is read against the record a text is, so a mount on a row whose text is a record
  // hands its construct that record: by its `source` attribute or option, or by a spread the walk
  // cannot read. `undefined` written out hands none, and the hover card takes none at all.
  const sourced = [
    "export const Row = ({ x, rest }: Props) => (",
    "  <>",
    '    <MarkdownBody markdown={x.body} source={source} surface="answers-reply" />',
    '    <MarkdownBody markdown={x.body} source={undefined} surface="answers-reply" />',
    '    <MarkdownBody markdown={x.body} {...rest} surface="answers-reply" />',
    '    <MarkdownBody markdown={x.body} surface="broadcast-recipient-reply" />',
    '    <MarkdownBody markdown={x.body} surface="broadcast-sent-body" />',
    "    <RefPreviewHost />",
    "  </>",
    ");",
    "export function useLine(x: Ask) {",
    '  return usePlainHeadline(x.question_tree, x.question, { source, surface: "answers-reply" });',
    "}",
    "export function useBare(x: Ask) {",
    '  return usePlainHeadline(x.question_tree, x.question, { surface: "answers-reply" });',
    "}",
  ].join("\n");

  test("a mount's source is read from its own attributes or options", () => {
    expect(mounts(sourced).map((mount) => `${mount.at} ${mount.sourced}`)).toEqual([
      "fixture.tsx:3 true",
      "fixture.tsx:4 false",
      "fixture.tsx:5 true",
      "fixture.tsx:6 false",
      "fixture.tsx:7 false",
      "fixture.tsx:8 undefined",
      "fixture.tsx:12 true",
      "fixture.tsx:15 false",
    ]);
  });

  test("a mount on a record's row with no source fails, unless its row cannot carry a question id", () => {
    // `broadcast-recipient-reply` is a record that cannot carry a question id, and
    // `broadcast-sent-body` is no record, so neither needs a source.
    expect(
      mounts(sourced)
        .map(sourceComplaint)
        .filter((complaint) => complaint.includes(": "))
    ).toEqual([
      "fixture.tsx:4: a mount of the body or the preview renderer draws answers-reply, whose text is a record, and hands it no source",
      "fixture.tsx:15: a mount of the plain-text projection draws answers-reply, whose text is a record, and hands it no source",
    ]);
  });

  // A component that hands its own prop on as a construct's `source` hands the record on only
  // where it is handed one: each mount of it is held to that prop, and a component that hands
  // the prop on to such a component in turn is held to it too.
  test("a component that forwards its prop as a source is held to it at every mount", () => {
    const fixture = [
      "export function Status({ ask, source }: Props) {",
      '  return <MarkdownBody markdown={ask.text} source={source} surface="search-ask-status" />;',
      "}",
      "export function Hit({ hit, record }: Props) {",
      "  return <Status ask={hit.ask} source={record} />;",
      "}",
      "export const List = ({ x, rest }: Props) => (",
      "  <>",
      "    <Status ask={x.ask} source={x.source} />",
      "    <Status ask={x.ask} />",
      "    <Status ask={x.ask} source={undefined} />",
      "    <Status ask={x.ask} {...rest} />",
      "    <Hit hit={x} record={x.source} />",
      "    <Hit hit={x} />",
      "  </>",
      ");",
    ].join("\n");
    expect(
      forwardedSourceComplaints([parseSource("fixture.tsx", fixture)]).filter((complaint) =>
        complaint.includes(": ")
      )
    ).toEqual([
      "fixture.tsx:10: Status hands its source on as the source of search-ask-status, whose text is a record, and is mounted here with none",
      "fixture.tsx:11: Status hands its source on as the source of search-ask-status, whose text is a record, and is mounted here with none",
      "fixture.tsx:14: Hit hands its record on as the source of search-ask-status, whose text is a record, and is mounted here with none",
    ]);
  });
});

// ------------------------------------------------------------------------------------------
// A stored text drawn as its characters is read from the syntax tree
// ------------------------------------------------------------------------------------------

describe("a stored text drawn as its characters is read from the syntax tree", () => {
  // A fixture's own functions are the ones that answer for it.
  const drawsOf = (source: string, file = "fixture.tsx"): RawDraw[] => {
    const tree = parseSource(file, source);
    return rawDrawsOf(tree, proseAnswersOf([tree]));
  };
  const draws = (source: string): string[] => drawsOf(source).map(drawLabel);

  test("a JSX child, a plain attribute and a template literal draw it; a construct's text does not", () => {
    const source = [
      "export const Row = ({ x }: { x: Event }) => (",
      "  <div title={x.question}>",
      "    {x.body}",
      // biome-ignore lint/suspicious/noTemplateCurlyInString: the fixture's source holds a template literal
      "    <span>{`Retracted: ${x.reason}`}</span>",
      '    <MarkdownBody markdown={x.body} surface="answers-reply" />',
      // biome-ignore lint/suspicious/noTemplateCurlyInString: the fixture's source holds a template literal
      "    <MarkdownBody markdown={`${x.question}`} />",
      '    <p className="x">{x.id}</p>',
      "  </div>",
      ");",
    ].join("\n");
    expect(draws(source)).toEqual([
      "fixture.tsx:2 question the title attribute",
      "fixture.tsx:3 body a JSX child",
      "fixture.tsx:4 reason a template literal",
    ]);
  });

  // A portal draws its children, the first argument, into the node its second names, which is
  // the page's own DOM (`document.body`) and no text a writer stored.
  test("a portal draws its children, and the node it draws them into is no draw", () => {
    const source = [
      "export const Sheet = ({ open, x }: Props) => (",
      "  <>",
      "    {open",
      "      ? createPortal(<p>{x.body}</p>, document.body)",
      "      : null}",
      "    {createPortal(x.question, document.body)}",
      "  </>",
      ");",
    ].join("\n");
    expect(draws(source)).toEqual([
      "fixture.tsx:4 body a JSX child",
      "fixture.tsx:6 question a JSX child",
    ]);
  });

  test("it is drawn through ??, ||, the right of && and a conditional's branches", () => {
    const source = [
      "export const Row = ({ x, on }: Props) => (",
      "  <p>",
      '    {x.observed === "" ? "No reading." : x.observed}',
      "    {on && x.negative_control}",
      "    {x.excerpt ?? x.quote}",
      "    {x.heading.length > 0 || x.id}",
      "  </p>",
      ");",
    ].join("\n");
    expect(draws(source)).toEqual([
      "fixture.tsx:3 observed a JSX child",
      "fixture.tsx:4 negative_control a JSX child",
      "fixture.tsx:5 excerpt a JSX child",
      "fixture.tsx:5 quote a JSX child",
    ]);
  });

  test("it is traced through a constant, a prop a mount hands it, a mapped list and a text method", () => {
    const source = [
      "function Excerpt({ text }: { text: string }) {",
      "  return <p>{text}</p>;",
      "}",
      "function Hint({ text }: { text: string }) {",
      "  return <p>{text}</p>;",
      "}",
      "function Match({ candidate, edge }: Props) {",
      '  const words = candidate.first_line ?? "";',
      "  return (",
      "    <a>",
      "      <Excerpt text={edge.excerpt.text} />",
      '      <Hint text="Pick one" />',
      '      {words === "" ? candidate.id : words}',
      "      {(candidate.selected ?? []).map((label) => <i key={label}>{label}</i>)}",
      // biome-ignore lint/suspicious/noTemplateCurlyInString: the fixture's source holds a template literal
      '      {`Chose ${(candidate.selected ?? []).join(", ")}`}',
      "      {candidate.question.trim()}",
      '      {[candidate.text, edge.quote].filter(Boolean).join(" - ")}',
      "    </a>",
      "  );",
      "}",
    ].join("\n");
    // `Hint`'s text is no stored text: no mount in the file hands it one.
    expect(draws(source)).toEqual([
      "fixture.tsx:2 text a JSX child",
      "fixture.tsx:13 first_line a JSX child",
      "fixture.tsx:14 selected a JSX child",
      "fixture.tsx:15 selected a template literal",
      "fixture.tsx:16 question a JSX child",
      "fixture.tsx:17 text a JSX child",
      "fixture.tsx:17 quote a JSX child",
    ]);
  });

  test("a label or a description is stored only on an ask's option, and a class list draws nothing", () => {
    const source = [
      'import { badgeLow } from "../../theme/classes";',
      "export const Options = ({ ask, tabs }: Props) => (",
      "  <ul>",
      "    {tabs.map((tab) => <li key={tab.id}>{tab.label}</li>)}",
      "    {ask.options.map((option) => <li key={option.id}>{option.label}</li>)}",
      "    <li title={ask.options[0]?.description}>{badgeLow.text}</li>",
      // biome-ignore lint/suspicious/noTemplateCurlyInString: the fixture's source holds a template literal
      "    <li className={`${ask.question} ${badgeLow.text}`} />",
      "  </ul>",
      ");",
    ].join("\n");
    expect(draws(source)).toEqual([
      "fixture.tsx:5 label a JSX child",
      "fixture.tsx:6 description the title attribute",
    ]);
  });

  test("what the projection answers is its line, not the stored text", () => {
    const source = [
      "export function Title({ tree, markdown }: Props) {",
      "  const { reads, text: title } = usePlainHeadline(tree, markdown);",
      "  return <span title={title?.text}>{reads}</span>;",
      "}",
      // biome-ignore lint/suspicious/noTemplateCurlyInString: the fixture's source holds a template literal
      "export const line = (x: Ask) => usePlainHeadline(x.question_tree, `${x.question}`);",
    ].join("\n");
    expect(draws(source)).toEqual([]);
  });

  test("a call of a function that answers a stored field draws it", () => {
    const source = [
      "export function reasonOf(resolution: Resolution): string | undefined {",
      '  return resolution.kind === "withdrawn" ? undefined : resolution.reason;',
      "}",
      "const nameOf = (row: Row) => row.name;",
      "export const Line = ({ x }: Props) => {",
      "  const reason = reasonOf(x.resolution);",
      "  return (",
      "    <p>",
      "      {nameOf(x)}",
      // biome-ignore lint/suspicious/noTemplateCurlyInString: the fixture's source holds a template literal
      "      {reason === undefined ? null : ` - ${reason}`}",
      "    </p>",
      "  );",
      "};",
    ].join("\n");
    expect(draws(source)).toEqual(["fixture.tsx:10 reason a template literal"]);
  });

  test("a prop under another name, a component's props, and a helper's own constant or memo draw what they hold", () => {
    const source = [
      "function Row({ words }: { words: string }) {",
      "  return <p>{words}</p>;",
      "}",
      "function Line(props: { words: string }) {",
      "  return <p>{props.words}</p>;",
      "}",
      "function questionOf(ask: Ask) {",
      "  const question = ask.question;",
      "  return question;",
      "}",
      "export function Card({ ask }: Props) {",
      "  const label = useMemo(() => ask.heading, [ask]);",
      "  return (",
      "    <div>",
      "      <Row words={ask.question} />",
      "      <Line words={ask.body} />",
      "      {questionOf(ask)}",
      "      {label}",
      "    </div>",
      "  );",
      "}",
    ].join("\n");
    expect(draws(source)).toEqual([
      "fixture.tsx:2 question a JSX child",
      "fixture.tsx:5 body a JSX child",
      "fixture.tsx:17 question a JSX child",
      "fixture.tsx:18 heading a JSX child",
    ]);
  });

  test("a join with +, a call's argument and an element's other attribute draw it; a handle, an address and a component's prop do not", () => {
    const source = [
      "export const Row = ({ x }: Props) => (",
      "  <div>",
      '    {"Asked: " + x.question}',
      "    {cut(x.body, 40)}",
      '    {x.selected.length > 0 ? "chosen" : "none"}',
      "    <meter aria-valuetext={x.reason} />",
      "    <a data-quote={x.quote} href={x.text} />",
      "    <Card summary={x.summary} />",
      "  </div>",
      ");",
    ].join("\n");
    // A component's prop is read where that component draws it, in a file of its own, which
    // this walk does not follow (the header's blind spots).
    expect(draws(source)).toEqual([
      "fixture.tsx:3 question a JSX child",
      "fixture.tsx:4 body a JSX child",
      "fixture.tsx:6 reason the aria-valuetext attribute",
    ]);
  });

  // Part I's outline says in its own place that an address names nothing the issue holds, under
  // a heading from a table of its own words (`slots/body.tsx`'s `ABSENT_LINES`). A member of
  // such a table is no stored text, whatever it is called; the same member read off a value that
  // came from a read still is.
  test("a member of a table of words the file writes out draws no stored text; a record's does", () => {
    const source = [
      "const LINES = {",
      '  absent: { heading: "Not found", line: "Nothing here." },',
      "} as const;",
      "export function Gone({ decision }: { decision: Decision }) {",
      "  return (",
      "    <div>",
      "      <h2>{LINES.absent.heading}</h2>",
      "      <p>{decision.heading}</p>",
      "    </div>",
      "  );",
      "}",
    ].join("\n");
    expect(draws(source)).toEqual(["fixture.tsx:8 heading a JSX child"]);
  });

  // The rule exempts a read only where the literal itself holds the words it reaches: a holder
  // of a record and a record filled at run time each read a stored text, and each is still a
  // draw.
  test("a holder and a record filled at run time each draw their stored text", () => {
    const source = [
      "const LINES = {",
      '  absent: { heading: "Not found" },',
      "} as const;",
      "const last = { node: undefined as TreeNode | undefined };",
      "const byKey: Record<string, Comment> = {};",
      "export function Held({ id }: { id: string }) {",
      "  return (",
      "    <div>",
      "      <p>{last.node.text}</p>",
      "      <p>{byKey[id].body}</p>",
      "    </div>",
      "  );",
      "}",
    ].join("\n");
    expect(draws(source)).toEqual([
      "fixture.tsx:9 text a JSX child",
      "fixture.tsx:10 body a JSX child",
    ]);
  });

  // A name the file binds again anywhere below its top level is no table (`namesBoundBelowTop`):
  // a read of it may be of that other binding, whose words came from a read, so it is still a
  // draw wherever it stands - under a local of the table's name, read off a key the table holds.
  test("a local that shadows a table's name draws its stored text", () => {
    const source = [
      "const LINES = {",
      '  absent: { heading: "Not found" },',
      "} as const;",
      "export function Shadowed() {",
      "  const LINES = useDecision();",
      "  return <h2>{LINES.absent.heading}</h2>;",
      "}",
    ].join("\n");
    expect(draws(source)).toEqual(["fixture.tsx:6 heading a JSX child"]);
  });

  // The same for a `var` of the table's name in an inner block, which binds the name for the
  // whole function around it, outside the block too.
  test("a var of a table's name in an inner block draws its stored text", () => {
    const source = [
      "const LINES = {",
      '  absent: { heading: "Not found" },',
      "} as const;",
      "export function Late({ late }: { late: boolean }) {",
      "  if (late) {",
      "    var LINES = useDecision();",
      "  }",
      "  return <h2>{LINES.absent.heading}</h2>;",
      "}",
    ].join("\n");
    expect(draws(source)).toEqual(["fixture.tsx:8 heading a JSX child"]);
  });

  // The same for a class expression of the table's name, which binds the name inside the class.
  test("a class expression named as a table draws its stored text", () => {
    const source = [
      "const LINES = {",
      '  absent: { heading: "Not found" },',
      "} as const;",
      "export const Card = class LINES {",
      "  static absent = useDecision();",
      "  render() {",
      "    return <h2>{LINES.absent.heading}</h2>;",
      "  }",
      "};",
    ].join("\n");
    expect(draws(source)).toEqual(["fixture.tsx:7 heading a JSX child"]);
  });

  // A table the file writes to holds what its literal says no longer (`writtenNames`): a member
  // of it assigned, assigned by a compound assignment, stepped by `++` or `--`, or taken out by
  // `delete`, a read of it is still a draw, and so is a read of a table a member of which is
  // handed to `Object.assign` (`namesUsedWhole`). A table nothing writes keeps its exemption.
  // The rows after this one read the other writes and uses that end it.
  test("a table the file writes to draws what it holds, however it is written", () => {
    const source = [
      'const ONE = { absent: { heading: "Not found" } };',
      'const TWO = { absent: { heading: "Not found" } };',
      'const THREE = { absent: { heading: "Not found" } };',
      'const FOUR = { absent: { heading: "Not found" } };',
      'const FIVE = { absent: { heading: "Not found" } };',
      'const SIX = { absent: { heading: "Not found" } };',
      'const KEPT = { absent: { heading: "Not found" } };',
      "export function Filled({ decision }: { decision: Decision }) {",
      "  ONE.absent.heading = decision.heading;",
      "  TWO.absent.heading += decision.heading;",
      "  THREE.absent.count++;",
      "  --SIX.absent.count;",
      "  delete FOUR.absent;",
      "  Object.assign(FIVE.absent, decision);",
      "  return (",
      "    <div>",
      "      <h2>{ONE.absent.heading}</h2>",
      "      <h2>{TWO.absent.heading}</h2>",
      "      <h2>{THREE.absent.heading}</h2>",
      "      <h2>{FOUR.absent.heading}</h2>",
      "      <h2>{FIVE.absent.heading}</h2>",
      "      <h2>{SIX.absent.heading}</h2>",
      "      <h2>{KEPT.absent.heading}</h2>",
      "    </div>",
      "  );",
      "}",
    ].join("\n");
    expect(draws(source)).toEqual([
      "fixture.tsx:17 heading a JSX child",
      "fixture.tsx:18 heading a JSX child",
      "fixture.tsx:19 heading a JSX child",
      "fixture.tsx:20 heading a JSX child",
      "fixture.tsx:21 heading a JSX child",
      "fixture.tsx:22 heading a JSX child",
    ]);
  });

  // A table handed whole to a call is out of the walk's sight from then: the call may write it,
  // as `Object.defineProperty` and any function of the file's own may (`namesUsedWhole`). So is
  // a table a member of which is handed to `Reflect.set`, as to any other call. A read of either
  // is still a draw.
  test("a table handed to a call draws what it holds", () => {
    const source = [
      'const FILLED = { absent: { heading: "Not found" } };',
      'const DEFINED = { absent: { heading: "Not found" } };',
      'const SET = { absent: { heading: "Not found" } };',
      'const KEPT = { absent: { heading: "Not found" } };',
      "export function Handed({ decision }: { decision: Decision }) {",
      "  fill(FILLED, decision);",
      '  Object.defineProperty(DEFINED, "absent", { value: decision });',
      '  Reflect.set(SET.absent, "heading", decision.heading);',
      "  return (",
      "    <div>",
      "      <h2>{FILLED.absent.heading}</h2>",
      "      <h2>{DEFINED.absent.heading}</h2>",
      "      <h2>{SET.absent.heading}</h2>",
      "      <h2>{KEPT.absent.heading}</h2>",
      "    </div>",
      "  );",
      "}",
    ].join("\n");
    expect(draws(source)).toEqual([
      "fixture.tsx:11 heading a JSX child",
      "fixture.tsx:12 heading a JSX child",
      "fixture.tsx:13 heading a JSX child",
    ]);
  });

  // A member of a table handed to a call is out of the walk's sight as well: the call may write
  // it, as `fill` and a constructor may (`namesUsedWhole`, `passesOn`). A member that holds a
  // string is words, which no call writes, so a table whose only member handed on is one keeps
  // its exemption (`readsWordTable`). A read of each other is still a draw.
  test("a table whose member is handed to a call draws what it holds", () => {
    const source = [
      'const FILLED = { absent: { heading: "Not found" } };',
      'const BUILT = { absent: { heading: "Not found" } };',
      'const KEPT = { absent: { heading: "Not found" } };',
      "export function Handed({ decision }: { decision: Decision }) {",
      "  fill(FILLED.absent, decision);",
      "  new Filler(BUILT.absent, decision);",
      "  announce(KEPT.absent.heading);",
      "  return (",
      "    <div>",
      "      <h2>{FILLED.absent.heading}</h2>",
      "      <h2>{BUILT.absent.heading}</h2>",
      "      <h2>{KEPT.absent.heading}</h2>",
      "    </div>",
      "  );",
      "}",
    ].join("\n");
    expect(draws(source)).toEqual([
      "fixture.tsx:10 heading a JSX child",
      "fixture.tsx:11 heading a JSX child",
    ]);
  });

  // A table held under another name is written through that name, out of the walk's sight: an
  // alias of it, and a destructuring of it whose member is written (`namesUsedWhole`). A read of
  // it is still a draw.
  test("a table aliased draws what it holds", () => {
    const source = [
      'const ALIASED = { absent: { heading: "Not found" } };',
      'const SOURCE = { absent: { heading: "Not found" } };',
      'const KEPT = { absent: { heading: "Not found" } };',
      "export function Aliased({ decision }: { decision: Decision }) {",
      "  const alias = ALIASED;",
      "  alias.absent.heading = decision.heading;",
      "  const { absent } = SOURCE;",
      "  absent.heading = decision.heading;",
      "  return (",
      "    <div>",
      "      <h2>{ALIASED.absent.heading}</h2>",
      "      <h2>{SOURCE.absent.heading}</h2>",
      "      <h2>{KEPT.absent.heading}</h2>",
      "    </div>",
      "  );",
      "}",
    ].join("\n");
    expect(draws(source)).toEqual([
      "fixture.tsx:11 heading a JSX child",
      "fixture.tsx:12 heading a JSX child",
    ]);
  });

  // A member of a table held under another name is written through that name, out of the
  // walk's sight: an alias of it, and one a conditional chooses (`namesUsedWhole`, `passesOn`).
  // A member that holds a string is words, which no write reaches, so a table whose only member
  // held so is one keeps its exemption (`readsWordTable`). A read of each other is still a draw.
  test("a table whose member is aliased draws what it holds", () => {
    const source = [
      'const ALIASED = { absent: { heading: "Not found" } };',
      'const CHOSEN = { absent: { heading: "Not found" }, late: { heading: "Too late" } };',
      'const KEPT = { absent: { heading: "Not found" } };',
      "export function Aliased({ decision, late }: { decision: Decision; late: boolean }) {",
      "  const absent = ALIASED.absent;",
      "  absent.heading = decision.heading;",
      "  const line = late ? CHOSEN.late : CHOSEN.absent;",
      "  line.heading = decision.heading;",
      "  const heading = KEPT.absent.heading;",
      "  return (",
      "    <div>",
      "      <h2>{ALIASED.absent.heading}</h2>",
      "      <h2>{CHOSEN.absent.heading}</h2>",
      "      <h2>{KEPT.absent.heading}</h2>",
      "      <p>{heading}</p>",
      "    </div>",
      "  );",
      "}",
    ].join("\n");
    expect(draws(source)).toEqual([
      "fixture.tsx:12 heading a JSX child",
      "fixture.tsx:13 heading a JSX child",
    ]);
  });

  // The same holds wherever else a member of a table is passed on as a value (`passesOn`):
  // returned, an arrow's answer, assigned, the list a `for … of` walks, held by a member of an
  // object, spread, an element of a list, answered by `??`, or handed to an attribute. A string
  // member handed to one keeps its table's exemption. A read of each other is still a draw.
  test("a table whose member is passed on any other way draws what it holds", () => {
    const source = [
      'const RETURNED = { absent: { heading: "Not found" } };',
      'const ANSWERED = { absent: { heading: "Not found" } };',
      'const ASSIGNED = { absent: { heading: "Not found" } };',
      'const WALKED = { absent: { heading: "Not found" }, lines: [{ heading: "Not found" }] };',
      'const HELD = { absent: { heading: "Not found" } };',
      'const SPREAD = { absent: { heading: "Not found" } };',
      'const LISTED = { absent: { heading: "Not found" } };',
      'const FALLEN = { absent: { heading: "Not found" } };',
      'const HANDED = { absent: { heading: "Not found" } };',
      'const KEPT = { absent: { heading: "Not found" } };',
      "function absentLine() {",
      "  return RETURNED.absent;",
      "}",
      "const answered = () => ANSWERED.absent;",
      "export function Passed({ decision }: { decision: Decision }) {",
      "  let line;",
      "  line = ASSIGNED.absent;",
      "  for (const each of WALKED.lines) each.heading = decision.heading;",
      "  const held = { line: HELD.absent, copy: { ...SPREAD.absent }, list: [LISTED.absent] };",
      "  const fallen = decision.line ?? FALLEN.absent;",
      "  return (",
      "    <div>",
      "      <Line line={HANDED.absent} title={KEPT.absent.heading} />",
      "      <h2>{RETURNED.absent.heading}</h2>",
      "      <h2>{ANSWERED.absent.heading}</h2>",
      "      <h2>{ASSIGNED.absent.heading}</h2>",
      "      <h2>{WALKED.absent.heading}</h2>",
      "      <h2>{HELD.absent.heading}</h2>",
      "      <h2>{SPREAD.absent.heading}</h2>",
      "      <h2>{LISTED.absent.heading}</h2>",
      "      <h2>{FALLEN.absent.heading}</h2>",
      "      <h2>{HANDED.absent.heading}</h2>",
      "      <h2>{KEPT.absent.heading}</h2>",
      "    </div>",
      "  );",
      "}",
    ].join("\n");
    expect(draws(source)).toEqual([
      "fixture.tsx:24 heading a JSX child",
      "fixture.tsx:25 heading a JSX child",
      "fixture.tsx:26 heading a JSX child",
      "fixture.tsx:27 heading a JSX child",
      "fixture.tsx:28 heading a JSX child",
      "fixture.tsx:29 heading a JSX child",
      "fixture.tsx:30 heading a JSX child",
      "fixture.tsx:31 heading a JSX child",
      "fixture.tsx:32 heading a JSX child",
    ]);
  });

  // A component's child is handed to it as its `children`, as an attribute's value is
  // (`passesOn`), so a table a member of which is one is out of the walk's sight. A child of an
  // element of the page is drawn and passed on to nothing: its table keeps its exemption. A
  // read of the other is still a draw.
  test("a table whose member is a component's child draws what it holds", () => {
    const source = [
      'const FILLED = { absent: { heading: "Not found" } };',
      'const KEPT = { absent: { heading: "Not found" } };',
      "export function Filled() {",
      "  return (",
      "    <div>",
      "      <Fill>{FILLED.absent}</Fill>",
      "      <span>{KEPT.absent}</span>",
      "      <h2>{FILLED.absent.heading}</h2>",
      "      <h2>{KEPT.absent.heading}</h2>",
      "    </div>",
      "  );",
      "}",
    ].join("\n");
    expect(draws(source)).toEqual(["fixture.tsx:8 heading a JSX child"]);
  });

  // A member of a table the file exports as its default is handed to every file that imports
  // it (`passesOn`). A read of it is still a draw.
  test("a table whose member is exported as the default draws what it holds", () => {
    const source = [
      'const EXPORTED = { absent: { heading: "Not found" } };',
      'const KEPT = { absent: { heading: "Not found" } };',
      "export function Exported() {",
      "  return (",
      "    <div>",
      "      <h2>{EXPORTED.absent.heading}</h2>",
      "      <h2>{KEPT.absent.heading}</h2>",
      "    </div>",
      "  );",
      "}",
      "export default EXPORTED.absent;",
    ].join("\n");
    expect(draws(source)).toEqual(["fixture.tsx:6 heading a JSX child"]);
  });

  // The right of `,` is what the expression answers, so a member of a table there is passed on
  // wherever the expression is (`passesOn`); the left is answered by nothing, and its table
  // keeps its exemption. A read of the other is still a draw.
  test("a table whose member is the right of a comma draws what it holds", () => {
    const source = [
      'const RIGHT = { absent: { heading: "Not found" } };',
      'const LEFT = { absent: { heading: "Not found" } };',
      "export function Sequenced({ decision }: { decision: Decision }) {",
      "  const right = (decision.line, RIGHT.absent);",
      "  const left = (LEFT.absent, decision.line);",
      "  return (",
      "    <div>",
      "      <h2>{RIGHT.absent.heading}</h2>",
      "      <h2>{LEFT.absent.heading}</h2>",
      "    </div>",
      "  );",
      "}",
    ].join("\n");
    expect(draws(source)).toEqual(["fixture.tsx:8 heading a JSX child"]);
  });

  // `await` answers the value it waits on, so a member of a table there is passed on wherever
  // the `await` is; `yield` hands it to the generator's caller (`passesOn`). A read of each is
  // still a draw.
  test("a table whose member is awaited or yielded draws what it holds", () => {
    const source = [
      'const AWAITED = { absent: { heading: "Not found" } };',
      'const YIELDED = { absent: { heading: "Not found" } };',
      'const KEPT = { absent: { heading: "Not found" } };',
      "async function awaited() {",
      "  const line = await AWAITED.absent;",
      "  return line;",
      "}",
      "function* lines() {",
      "  yield YIELDED.absent;",
      "}",
      "export function Waited() {",
      "  return (",
      "    <div>",
      "      <h2>{AWAITED.absent.heading}</h2>",
      "      <h2>{YIELDED.absent.heading}</h2>",
      "      <h2>{KEPT.absent.heading}</h2>",
      "    </div>",
      "  );",
      "}",
    ].join("\n");
    expect(draws(source)).toEqual([
      "fixture.tsx:14 heading a JSX child",
      "fixture.tsx:15 heading a JSX child",
    ]);
  });

  // A tagged template hands each of its substitutions to its tag as an argument (`passesOn`),
  // so a table a member of which is one is out of the walk's sight. An untagged template joins
  // its substitutions and passes them on to nothing: its table keeps its exemption. A read of
  // the other is still a draw.
  test("a table whose member is a tagged template's substitution draws what it holds", () => {
    const source = [
      'const TAGGED = { absent: { heading: "Not found" } };',
      'const JOINED = { absent: { heading: "Not found" } };',
      "export function Tagged() {",
      // biome-ignore lint/suspicious/noTemplateCurlyInString: the fixture's source holds a template literal
      "  const styled = css`${TAGGED.absent}`;",
      // biome-ignore lint/suspicious/noTemplateCurlyInString: the fixture's source holds a template literal
      "  const joined = `${JOINED.absent}`;",
      "  return (",
      "    <div>",
      "      <h2>{TAGGED.absent.heading}</h2>",
      "      <h2>{JOINED.absent.heading}</h2>",
      "    </div>",
      "  );",
      "}",
    ].join("\n");
    expect(draws(source)).toEqual(["fixture.tsx:8 heading a JSX child"]);
  });

  // A write through a cast writes the table under it (`memberChain` takes the cast off): a cast
  // on the whole table, and one on a member of it. A read of either is still a draw.
  test("a table written through a cast draws what it holds", () => {
    const source = [
      'const CAST = { absent: { heading: "Not found" } };',
      'const MEMBER = { absent: { heading: "Not found" } };',
      'const KEPT = { absent: { heading: "Not found" } };',
      "export function Cast({ decision }: { decision: Decision }) {",
      "  (CAST as Lines).absent.heading = decision.heading;",
      "  (MEMBER.absent satisfies Line).heading = decision.heading;",
      "  return (",
      "    <div>",
      "      <h2>{CAST.absent.heading}</h2>",
      "      <h2>{MEMBER.absent.heading}</h2>",
      "      <h2>{KEPT.absent.heading}</h2>",
      "    </div>",
      "  );",
      "}",
    ].join("\n");
    expect(draws(source)).toEqual([
      "fixture.tsx:9 heading a JSX child",
      "fixture.tsx:10 heading a JSX child",
    ]);
  });

  // A destructuring assignment writes each target of its pattern, and a `for … of` or a
  // `for … in` over an expression writes it on every turn (`writtenNames`): a member of a table
  // in an object's pattern, in an array's, and as the target of each loop. A read of each is
  // still a draw.
  test("a table a destructuring or a for-of writes through draws what it holds", () => {
    const source = [
      'const PATTERN = { absent: { heading: "Not found" } };',
      'const LISTED = { absent: { heading: "Not found" } };',
      'const LOOPED = { absent: { heading: "Not found" } };',
      'const KEYED = { absent: { heading: "Not found" } };',
      'const KEPT = { absent: { heading: "Not found" } };',
      "export function Unpacked({ decision, words }: { decision: Decision; words: string[] }) {",
      "  ({ heading: PATTERN.absent.heading } = decision);",
      "  [, LISTED.absent.heading] = words;",
      "  for (LOOPED.absent.heading of words) {}",
      "  for (KEYED.absent.heading in decision) {}",
      "  return (",
      "    <div>",
      "      <h2>{PATTERN.absent.heading}</h2>",
      "      <h2>{LISTED.absent.heading}</h2>",
      "      <h2>{LOOPED.absent.heading}</h2>",
      "      <h2>{KEYED.absent.heading}</h2>",
      "      <h2>{KEPT.absent.heading}</h2>",
      "    </div>",
      "  );",
      "}",
    ].join("\n");
    expect(draws(source)).toEqual([
      "fixture.tsx:13 heading a JSX child",
      "fixture.tsx:14 heading a JSX child",
      "fixture.tsx:15 heading a JSX child",
      "fixture.tsx:16 heading a JSX child",
    ]);
  });

  // The new issue page's node, chip and parent line carry a writer's stored text under fields the
  // contract's scan cannot name (`PROSE_RECORDS`). G's folded line drew a node handed whole to a
  // call, `firstLineOf(lineWords(node))`, and its chip a call on the chip's name, and the walk
  // saw neither: a call on a record, or on such a field of it, now draws what it is handed,
  // while the projection of the same fields draws its line.
  test("a call handed a record that carries a stored text, or such a field of it, draws it; the projection does not", () => {
    const source = [
      "function FoldedLine({ node }: { node: TreeNode }) {",
      "  const first = firstLineOf(lineWords(node));",
      "  return <span>{drawnLine(first)}</span>;",
      "}",
      "function Chip({ chip }: { chip: NodeChip }) {",
      "  return <a>{chipWords(wordsOf(chip.reading?.tree))}</a>;",
      "}",
      "const parentWords = (line: ParentLine) => <i>{cut(wordsOf(line.tree), 40)}</i>;",
      "function Line({ node }: { node: TreeNode }) {",
      "  const { text: stored, tree } = node;",
      "  const { reads, text } = usePlainHeadline(tree, stored);",
      "  return (",
      "    <span>",
      "      {reads}",
      '      {drawnLine(firstLineOf(text?.text ?? ""))}',
      "    </span>",
      "  );",
      "}",
    ].join("\n");
    expect(draws(source)).toEqual([
      "fixture.tsx:3 TreeNode a JSX child",
      "fixture.tsx:6 reading.tree a JSX child",
      "fixture.tsx:8 tree a JSX child",
    ]);
  });

  test("a draw is excused by its row's id beside it or above its attribute, and by no other words", () => {
    // The file of `agent-message-issue-option`, a row the list says is drawn without the
    // renderer, so a draw in it may name that row.
    const listed = "packages/dispatch/web/src/features/agents/AgentMessageComposer.tsx";
    const source = [
      "export const Options = ({ x }: Props) => (",
      "  <select>",
      "    {/* Why it is drawn so is the reading list's",
      "        `agent-message-issue-option`. */}",
      "    <option>{x.question}</option>",
      "    <option>{x.body}</option>",
      "    {/* not stored prose: the tab's own name */}",
      "    <option>{x.text}</option>",
      "    {/* agent-message-issue-option */}",
      "    <option>{x.reason}</option>",
      "    <Confirm",
      "      // agent-message-issue-option: a login's own name",
      "      words={() =>",
      "        `Revoke ${",
      "          x.subject",
      // biome-ignore lint/suspicious/noTemplateCurlyInString: the fixture's source holds a template literal
      "        }? ${<b>{x.snippet}</b>}`",
      "      }",
      "    />",
      "  </select>",
      ");",
    ].join("\n");
    const excused = (file: string): number[] =>
      drawsOf(source, file)
        .filter((draw) => drawComplaint(draw, source) === drawLabel(draw))
        .map((draw) => draw.line);
    // Free words excuse nothing: line 8's comment says why, and names no row. The comment above
    // the attribute speaks for its own value, not for an element inside it.
    expect(excused(listed)).toEqual([5, 10, 15]);
    // A row whose files do not hold the draw's file excuses nothing there.
    expect(excused("fixture.tsx")).toEqual([]);
    expect(drawsOf(source).map((draw) => draw.line)).toEqual([5, 6, 8, 10, 15, 16]);
  });
});

// ------------------------------------------------------------------------------------------
// Every box and every surface the dashboard draws is in its list
// ------------------------------------------------------------------------------------------

describe("every box the dashboard draws is in the writing list", () => {
  test.each(
    WRITING_SITES.map((site) => [site.at, site.construct, site.file] as const)
  )("%s is %s", (_at, _construct, file) => {
    expect(listing(file, WRITING_FILES, "DISPATCH_WRITING_BOXES")).toBe(file);
  });
});

describe("every surface the dashboard draws names its row of the reading list", () => {
  test.each(
    MOUNTS.map((mount) => [mount.at, mount.construct, mount] as const)
  )("%s is %s", (_at, _construct, mount) => {
    expect(mountComplaint(mount)).toBe(mount.at);
  });
});

describe("every mount of a record's text hands its construct that record", () => {
  test.each(
    MOUNTS.filter((mount) => mount.sourced !== undefined).map(
      (mount) => [mount.at, mount.construct, mount] as const
    )
  )("%s is %s", (_at, _construct, mount) => {
    expect(sourceComplaint(mount)).toBe(mount.at);
  });
});

describe("every mount of a component that hands its prop on as a record hands it that prop", () => {
  const complaints = forwardedSourceComplaints(DRAWING_TREES);
  test("finds the components that hand a record on", () => {
    // A guard on the walk itself: were it to follow no component, every row below would pass
    // over nothing. The palette's ask hit hands its `source` on to the renderer.
    expect(complaints.some((complaint) => complaint.includes("SearchPalette.tsx"))).toBe(true);
  });
  test.each(
    complaints.map((complaint) => [complaint.split(": ")[0] ?? complaint, complaint] as const)
  )("%s", (at, complaint) => {
    expect(complaint).toBe(at);
  });
});

describe("a mount's tree is read from its own attributes and followed through a component's props", () => {
  const fixture = [
    "function Quote({ text, textTree }: Props) {",
    "  const shown = textTree;",
    '  return <MarkdownPreview lines={1} markdown={text} surface="reply-quote" tree={shown} />;',
    "}",
    "function Turn({ quote }: Props) {",
    "  return <Quote text={quote.excerpt} textTree={quote.excerptTree} />;",
    "}",
    "export const Page = ({ x }: Props) => (",
    "  <>",
    '    <MarkdownBody markdown={x.body} surface="reply-quote" />',
    '    <MarkdownBody markdown={x.body} surface="reply-quote" tree={x.body_tree} />',
    '    <MarkdownBody markdown="Our own words." surface="reply-quote" />',
    "    <Quote text={x.body} />",
    '    <Quote text="" />',
    "    <Turn quote={{ excerpt: x.body }} />",
    "    <Turn quote={{ excerpt: x.body, excerptTree: x.body_tree }} />",
    '    <Turn quote={{ excerpt: "" }} />',
    "    <Turn quote={held} />",
    "    <Turn quote={{ excerpt: x.body, ...(x.on ? {} : { to: x.to }) }} />",
    "  </>",
    ");",
  ].join("\n");

  test("a construct or a component handed a stored text and no tree of it fails, and no other", () => {
    const mounts = treeMountsOf([parseSource("fixture.tsx", fixture)]);
    expect(mounts.filter((mount) => mount.untreed).map((mount) => mount.at)).toEqual([
      "fixture.tsx:10",
      "fixture.tsx:13",
      "fixture.tsx:15",
      "fixture.tsx:19",
    ]);
    expect(mounts.find((mount) => mount.at === "fixture.tsx:13")?.complaint).toBe(
      "fixture.tsx:13: Quote's text is handed a stored text and no tree of it, so the page parses that text on its own thread"
    );
  });

  test("a mount on a row whose text has no tree is excused by that row and no other", () => {
    const mounts = treeMountsOf([
      parseSource(
        "fixture.tsx",
        [
          "export const Row = ({ x }: Props) => (",
          "  <>",
          '    <MarkdownBody markdown={x.text} surface="agent-turn-text" />',
          '    <MarkdownBody markdown={x.text} surface="reply-quote" />',
          "  </>",
          ");",
        ].join("\n")
      ),
    ]);
    expect(mounts.map((mount) => [mount.untreed, mount.complaint === undefined])).toEqual([
      [true, true],
      [true, false],
    ]);
  });

  // A broadcast's send row draws two texts: the draft of a send Dispatch has not made, which has
  // no tree, and the body Dispatch stored for a made one, whose answer carries its tree. The row
  // excuses the draft alone (`UntreedRow.text`), so the made row is held to its tree.
  test("a row that names its text excuses that text alone", () => {
    const mounts = treeMountsOf([
      parseSource(
        "fixture.tsx",
        [
          "export const Row = ({ row }: Props) => (",
          "  <>",
          '    <MarkdownPreview lines={1} markdown={row.send.input.body} surface="broadcast-send-row" />',
          '    <MarkdownPreview lines={1} markdown={row.created.body} surface="broadcast-send-row" />',
          '    <MarkdownPreview lines={1} markdown={row.created.body} surface="broadcast-send-row" tree={row.created.body_tree} />',
          "  </>",
          ");",
        ].join("\n")
      ),
    ]);
    expect(
      mounts.map((mount) => [mount.untreed, mount.complaint === undefined, mount.excusedBy])
    ).toEqual([
      [true, true, "broadcast-send-row"],
      [true, false, undefined],
      [false, true, undefined],
    ]);
  });
});

describe("every mount of the renderer or the projection on a stored text hands it the text's tree", () => {
  test("finds the mounts and the components that hand a tree on", () => {
    // A guard on the walk itself: were it to read no mount, or follow no component, every row
    // below would pass over nothing.
    expect(TREE_MOUNTS.length).toBeGreaterThan(100);
    expect(TREE_MOUNTS.some((mount) => mount.at.includes("ReplyTurn.tsx"))).toBe(true);
  });
  test.each(TREE_MOUNTS.map((mount) => [mount.at, mount] as const))("%s", (at, mount) => {
    expect(mount.complaint ?? at).toBe(at);
  });
  test.each(
    UNTREED_ROWS.map((row) => [row.surface] as const)
  )("%s still has a mount its text reaches with no tree", (surface) => {
    expect(untreedRowComplaint(surface)).toBe(surface);
  });
});

describe("every stored text the contract's scan cannot name keeps what its entry says of its tree", () => {
  test("a text handed to one call or element beside its tree is drawn with it, and no other", () => {
    const hands = (source: string, field: string, written: string): boolean =>
      handsWithTree(parseSource("fixture.tsx", source), field, written);
    expect([
      hands(
        'const line = eventProse(event, "payload.ask_question", event.payload.ask_question);',
        "ask_question",
        "payload.ask_question"
      ),
      hands(
        'const line = eventProse(event, "payload.question", event.payload.ask_question);',
        "ask_question",
        "payload.ask_question"
      ),
      hands(
        "const row = <PlainText markdown={x.first_line} tree={x.first_line_tree} />;",
        "first_line",
        "first_line_tree"
      ),
      hands(
        "const row = <><PlainText markdown={x.first_line} /><i title={x.first_line_tree} /></>;",
        "first_line",
        "first_line_tree"
      ),
    ]).toEqual([true, false, true, false]);
  });
  test.each(
    HAND_NAMED_TEXTS.flatMap((text) =>
      text.tree === undefined ? [] : [[text.field, text.tree.file, text.tree] as const]
    )
  )("%s is drawn with its tree in %s", (field, file, tree) => {
    const source = TREE_BY_FILE.get(file);
    expect(
      source !== undefined && handsWithTree(source, field, tree.written)
        ? field
        : `${file} hands ${field} to no call or element beside ${tree.written}`
    ).toBe(field);
  });
  test.each(
    HAND_NAMED_TEXTS.map((text) => [text.field] as const)
  )("%s is a text no tree of the contract's names", (field) => {
    expect(TREED_FIELDS).not.toContain(field);
  });
});

describe("every row drawn through the projection names the test that proves it", () => {
  // A rendered row is proved by its browser driver (`e2e/reading-rows.ts`); a row the renderer
  // does not draw and the projection does has no driver, so it names the test that reads its
  // line (`proof`), which `e2e/__tests__/reading-walk.test.ts` holds to a test that exists.
  const projected = DISPATCH_READING_SURFACES.filter(
    (row) =>
      !row.rendered &&
      MOUNTS.some(
        (mount) =>
          mount.surface === row.surface &&
          mount.construct === "a mount of the plain-text projection"
      )
  );
  test("finds the rows drawn through the projection", () => {
    expect(projected.length).toBeGreaterThan(3);
  });
  test.each(projected.map((row) => [row.surface, row] as const))("%s", (label, row) => {
    expect(row.proof === undefined ? `${label} names no test that proves it` : label).toBe(label);
  });
});

describe("no stored text is drawn as its characters outside a construct and the list", () => {
  test.each(RAW_DRAWS.map((draw) => [drawLabel(draw), draw] as const))("%s", (label, draw) => {
    expect(drawComplaint(draw, TEXT_BY_FILE.get(draw.file) ?? "")).toBe(label);
  });
});

describe("every entry of the list of fields that hold no stored text is still a draw the walk finds", () => {
  test.each(
    NOT_STORED_TEXTS.map((entry) => [`${entry.file} ${entry.written}`, entry] as const)
  )("%s", (label, entry) => {
    const found = RAW_DRAWS.some(
      (draw) => draw.file === entry.file && draw.written === entry.written
    );
    expect(found ? label : `${label} names no draw the walk finds: remove it from the list`).toBe(
      label
    );
  });
});

// ------------------------------------------------------------------------------------------
// Every row is a box or a surface the dashboard still draws
// ------------------------------------------------------------------------------------------

describe("every row of the writing list is a box the dashboard still draws", () => {
  test.each(
    DISPATCH_WRITING_BOXES.map((row) => [`${row.box} writing ${row.write}`, row] as const)
  )("%s", (label: string, row: DispatchWritingBoxRow) => {
    for (const file of row.files) {
      expect(
        LINES_BY_FILE.has(file) ? file : `${file} (named by ${label}) is not a source file`
      ).toBe(file);
    }
    const drawn = row.files.some((file) => holdsAny(file, WRITING_CONSTRUCTS));
    expect(drawn ? label : `${label} names no file that draws a box: ${row.files.join(", ")}`).toBe(
      label
    );
    const named = row.files.some((file) => writesWord(file, row.box));
    expect(named ? label : `no file of ${label} writes its box id: ${row.files.join(", ")}`).toBe(
      label
    );
    if (row.covered) {
      // A covered box takes the picker: a mount of the shared field in a file of it picks the
      // row on its own attributes, or a file of it mounts an editor that hosts the picker and a
      // file of it picks the row beside it (the document body's editor is mounted in one file
      // and its picker picks the row in another). A comment naming the row, a string that is no
      // pick, and a mount picking another row are none of these.
      const editor = row.files.some((file) => holdsAny(file, [EDITOR_MOUNT]));
      const picked = row.files.some((file) => picksBox(file, row.box, editor));
      expect(
        picked ? label : `no mount in a file of ${label} picks its row: ${row.files.join(", ")}`
      ).toBe(label);
      // A pull request runs the reference rows only where it changes a path the
      // `dispatch_references` filter names (`REFERENCE_GATE`), so a covered box's file the filter
      // misses is one a pull request can take the picker out of with none of those rows run.
      for (const file of row.files) {
        expect(
          inReferenceFilter(file)
            ? file
            : `${file} (named by ${label}) is not in the dispatch_references filter of ${REFERENCE_GATE}`
        ).toBe(file);
      }
    }
  });

  test.each(
    DISPATCH_WRITING_BOXES.map((row) => [`${row.box} writing ${row.write}`, row] as const)
  )("%s says why it is not covered", (label: string, row: DispatchWritingBoxRow) => {
    // An excluded row with no reason is the one thing a list like this cannot leave behind:
    // the box has no picker and nothing anywhere says why.
    expect(reasonComplaint(label, row.covered, row.reason, row.files)).toBe(label);
  });
});

describe("every row of the reading list is a surface the dashboard still draws", () => {
  test.each(
    DISPATCH_READING_SURFACES.map((row) => [row.surface, row] as const)
  )("%s", (label: string, row: DispatchReadingSurfaceRow) => {
    for (const file of row.files) {
      expect(
        LINES_BY_FILE.has(file) ? file : `${file} (named by ${label}) is not a source file`
      ).toBe(file);
    }
    if (row.rendered) {
      // A rendered surface is a mount that names it, in a file of it: a mount in that file for
      // another row stands in for nothing.
      const drawn = MOUNTS.some(
        (mount) => mount.surface === row.surface && row.files.includes(mount.file)
      );
      expect(drawn ? label : `${label} renders, but no mount in a file of it names it`).toBe(label);
    }
    if (row.reason !== undefined) {
      // An excluded row's tie to the place it excuses: the site names the row rather than
      // carrying a second copy of the reason that can drift from this one.
      const pointed = row.files.some((file) => writesWord(file, row.surface));
      expect(pointed ? label : `no file of ${label} points at it by name`).toBe(label);
    }
  });

  test.each(
    DISPATCH_READING_SURFACES.map((row) => [row.surface, row] as const)
  )("%s says why it is excluded", (label: string, row: DispatchReadingSurfaceRow) => {
    expect(reasonComplaint(label, row.rendered && row.record, row.reason, row.files)).toBe(label);
  });
});

// ------------------------------------------------------------------------------------------
// No second reader came back
// ------------------------------------------------------------------------------------------

describe("no file imports a reader this change deleted", () => {
  test.each(
    DELETED_READERS.map((reader) => [reader.what, reader] as const)
  )("%s is imported nowhere", (_what: string, reader: DeletedReader) => {
    // The patterns are the reader's own, so one is built per deleted reader rather than per
    // line of every file the walk reads.
    const importPattern =
      reader.module === undefined ? undefined : new RegExp(`from "[^"]*${reader.module}"`);
    const namePatterns = reader.names.map((name) => new RegExp(`\\b${name}\\b`));
    const offenders: string[] = [];
    for (const file of SCANNED_FILES) {
      const lines = LINES_BY_FILE.get(file) ?? [];
      lines.forEach((line, index) => {
        const importsModule = importPattern?.test(line) === true;
        const writesName = namePatterns.some((pattern) => pattern.test(line));
        if (importsModule || writesName) {
          offenders.push(`${file}:${index + 1}`);
        }
      });
    }
    expect(offenders).toEqual([]);
  });
});
