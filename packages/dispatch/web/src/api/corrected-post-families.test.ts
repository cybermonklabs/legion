import { expect, spyOn, test } from "bun:test";
import { readdirSync, readFileSync } from "node:fs";
import { join, relative } from "node:path";
import { ID_AMBIGUOUS } from "@legion/contracts";
import {
  QueryClient,
  QueryClientProvider,
  type QueryKey,
  QueryObserver,
  useQuery,
} from "@tanstack/react-query";
import { act, renderHook, waitFor } from "@testing-library/react";
import { createElement, type ReactNode } from "react";
import { MemoryRouter } from "react-router-dom";

import {
  correction,
  correctionFeed,
  DELETION,
  EDITED_AT,
  readAroundAFailedRefetch,
} from "../__tests__/correction-feed";
import { graphNode } from "../__tests__/graph-fixture";
import { messageCorrectionFields } from "../__tests__/message-fixture";
import { useMarginItems } from "../features/margin/useMarginItems";
import { replyRow } from "../features/threads/replies-fixture";
import { ApiError, ambiguousItems, api } from "./client";
import {
  CORRECTED_POST_FAMILIES,
  markCorrectedPost,
  NO_POST_FAMILIES,
  REFUSAL_FAMILIES,
} from "./corrected-post";
import { useEventFeed } from "./event-feed";
import { referenceTargetsQuery, whoAmIQuery } from "./queries";
import { DispatchQueryClient, hearLiveEvent } from "./query-client";
import type {
  Artifact,
  CommentWithTrees,
  DispatchEvent,
  GraphReferences,
  IDCandidate,
  IssueReferences,
  MessageRead,
  MessageWithTrees,
  ReferenceLink,
} from "./types";

// R2 and R3 of dispatch://INFRA-1523, and the architect's decision of 2026-11-20 07:46Z and 07:48Z:
// a correction writes its own facts into every cached family whose answer holds a comment or an
// issue message, or a copy of one's body or marks (`markCorrectedPost`), so a refetch that fails
// shows no old words. The list of those families was made by hand and missed two (test round
// 43), so it is derived here: the families the web app's source keys a query under, read as
// census query C-Q1 reads them (class C of the surface census), are each written
// (`CORRECTED_POST_FAMILIES`) or listed as holding no post with why (`NO_POST_FAMILIES`), and
// each listed family is still keyed somewhere.

/** The web app's source: `web/src`. */
const SOURCE_ROOT = join(import.meta.dir, "..");

/** Every `.ts` and `.tsx` file under `directory` but the tests, as C-Q1's pathspecs take them:
 *  no `*.test.ts`, no `*.test.tsx` and nothing under a `__tests__` directory. */
function sourceFiles(directory: string): string[] {
  return readdirSync(directory, { withFileTypes: true }).flatMap((entry) => {
    const path = join(directory, entry.name);
    if (entry.isDirectory()) return entry.name === "__tests__" ? [] : sourceFiles(path);
    return /\.tsx?$/u.test(entry.name) && !/\.test\.tsx?$/u.test(entry.name) ? [path] : [];
  });
}

/** One source file, by its path from `web/src`, with its text. */
interface Source {
  readonly path: string;
  readonly text: string;
}

const SOURCES: readonly Source[] = sourceFiles(SOURCE_ROOT).map((path) => ({
  path: relative(SOURCE_ROOT, path),
  text: readFileSync(path, "utf8"),
}));

/** C-Q1's own expression, `queryKey: *\[ *"[a-zA-Z_-]+"( *, *"[a-zA-Z_-]+")?`, held at the
 *  `queryKey:` it is tried at. */
const LITERAL_KEY = /queryKey: *\[ *"([a-zA-Z_-]+)"(?: *, *"([a-zA-Z_-]+)")?/uy;

/** A family as `CORRECTED_POST_FAMILIES` names it: the key's first string, and its second where
 *  the key writes one, joined by a space. */
function familyName(first: string, second: string | undefined): string {
  return second === undefined ? first : `${first} ${second}`;
}

/** Where `text[from]`, a quote or a backtick, ends: the index past its closing mark. */
function quotedEnd(text: string, from: number): number {
  const quote = text[from];
  let at = from + 1;
  let braces = 0;
  while (at < text.length) {
    const char = text[at];
    if (char === "\\") {
      at += 2;
      continue;
    }
    if (quote === "`" && char === "$" && text[at + 1] === "{") braces += 1;
    else if (quote === "`" && char === "}" && braces > 0) braces -= 1;
    else if (char === quote && braces === 0) return at + 1;
    at += 1;
  }
  return at;
}

/** The expression that starts at `from`: up to the first `,` or `;` at its own depth, or the
 *  bracket that closes the one it stands in, with its comments left out. */
function expressionAt(text: string, from: number): string {
  let depth = 0;
  let at = from;
  let expression = "";
  while (at < text.length) {
    const char = text[at] ?? "";
    if (char === '"' || char === "'" || char === "`") {
      const end = quotedEnd(text, at);
      expression += text.slice(at, end);
      at = end;
      continue;
    }
    if (char === "/" && text[at + 1] === "/") {
      const end = text.indexOf("\n", at);
      at = end < 0 ? text.length : end;
      continue;
    }
    if (char === "/" && text[at + 1] === "*") {
      const end = text.indexOf("*/", at);
      at = end < 0 ? text.length : end + 2;
      continue;
    }
    if ("([{".includes(char)) depth += 1;
    else if (")]}".includes(char)) {
      if (depth === 0) break;
      depth -= 1;
    } else if ((char === "," || char === ";") && depth === 0) break;
    expression += char;
    at += 1;
  }
  return expression.trim();
}

/** Where the bracket at `open` in `expression` is closed: the index of its closing mark, or -1. */
function closingAt(expression: string, open: number): number {
  let depth = 0;
  for (let at = open; at < expression.length; at += 1) {
    const char = expression[at] ?? "";
    if (char === '"' || char === "'" || char === "`") {
      at = quotedEnd(expression, at) - 1;
      continue;
    }
    if ("([{".includes(char)) depth += 1;
    else if (")]}".includes(char)) {
      depth -= 1;
      if (depth === 0) return at;
    }
  }
  return -1;
}

/** The two branches of `expression` where it is a conditional at its own depth
 *  (`test ? when : otherwise`); undefined for any other expression. */
function conditionalBranches(expression: string): readonly [string, string] | undefined {
  let depth = 0;
  let question = -1;
  let pending = 0;
  for (let at = 0; at < expression.length; at += 1) {
    const char = expression[at] ?? "";
    if (char === '"' || char === "'" || char === "`") {
      at = quotedEnd(expression, at) - 1;
      continue;
    }
    if ("([{".includes(char)) depth += 1;
    else if (")]}".includes(char)) depth -= 1;
    if (depth !== 0) continue;
    // `?.` and `??` are no conditional.
    if (char === "?" && expression[at + 1] !== "." && expression[at + 1] !== "?") {
      if (expression[at - 1] === "?") continue;
      if (question < 0) question = at;
      else pending += 1;
    } else if (char === ":" && question >= 0) {
      if (pending > 0) pending -= 1;
      else return [expression.slice(question + 1, at).trim(), expression.slice(at + 1).trim()];
    }
  }
  return undefined;
}

/** `make`'s answer, worked out the first time it is asked for and kept. */
function once<T>(make: () => T): () => T {
  let made: { readonly value: T } | undefined;
  return () => {
    if (made === undefined) made = { value: make() };
    return made.value;
  };
}

/** A name a file binds with `const` or `let`, or declares as a function, where it binds it. */
interface Binding {
  readonly path: string;
  readonly at: number;
  readonly exported: boolean;
  /** The bound expression, or each `return` of a function's body, read from the source the first
   *  time a key is resolved through the binding. */
  readonly expressions: () => readonly string[];
}

/** Every `const`, `let` and `function` of the source, by the name it binds. */
const BINDINGS: ReadonlyMap<string, readonly Binding[]> = (() => {
  const bindings = new Map<string, Binding[]>();
  const bind = (name: string, binding: Binding) => {
    const named = bindings.get(name);
    if (named === undefined) bindings.set(name, [binding]);
    else named.push(binding);
  };
  for (const { path, text } of SOURCES) {
    for (const found of text.matchAll(
      /(export\s+)?(?:const|let)\s+([a-zA-Z_$][\w$]*)\s*(?::[^=;]+?)?=(?![=>])\s*/gu
    )) {
      const from = (found.index ?? 0) + found[0].length;
      bind(found[2] ?? "", {
        at: found.index ?? 0,
        exported: found[1] !== undefined,
        expressions: once(() => [expressionAt(text, from)]),
        path,
      });
    }
    for (const found of text.matchAll(/(export\s+)?function\s+([a-zA-Z_$][\w$]*)\s*[<(]/gu)) {
      const start = text.indexOf("{", text.indexOf(")", found.index ?? 0));
      bind(found[2] ?? "", {
        at: found.index ?? 0,
        exported: found[1] !== undefined,
        expressions: once(() => {
          const body = start < 0 ? "" : `{${expressionAt(text, start + 1)}}`;
          return [...body.matchAll(/\breturn\s+/gu)].map((one) =>
            expressionAt(body, (one.index ?? 0) + one[0].length)
          );
        }),
        path,
      });
    }
  }
  return bindings;
})();

/** The bindings `name` reads at `at` in `path`: the file's own last one before it, else the file's
 *  first, else every exported one, as an import names it. */
function bindingsOf(name: string, path: string, at: number): readonly Binding[] {
  const all = BINDINGS.get(name) ?? [];
  const own = all.filter((binding) => binding.path === path);
  const before = own.filter((binding) => binding.at < at).at(-1);
  if (before !== undefined) return [before];
  if (own[0] !== undefined) return [own[0]];
  return all.filter((binding) => binding.exported);
}

/** What a computed key resolves to: the families it can name, or a key another site keys a query
 *  under (`someQuery(…).queryKey`), which the test counts there, or a type, where the colon
 *  annotates a parameter or a field and keys nothing. */
type Resolved = readonly string[] | "named elsewhere" | "a type";

/** The families `expression`, a key as written at `at` in `path`, names; undefined where no rule
 *  here reads it. */
function keyFamilies(
  expression: string,
  path: string,
  at: number,
  seen: ReadonlySet<string> = new Set()
): Resolved | undefined {
  const bare = expression.replace(/\s+(?:as\s+const|satisfies\s[\s\S]+)$/u, "").trim();
  const branches = conditionalBranches(bare);
  if (branches !== undefined) {
    const each = branches.map((branch) => keyFamilies(branch, path, at, seen));
    if (each.some((one) => one === undefined)) return undefined;
    return each.flatMap((one) => (Array.isArray(one) ? one : []));
  }
  if (bare.startsWith("(") && closingAt(bare, 0) === bare.length - 1) {
    return keyFamilies(bare.slice(1, -1), path, at, seen);
  }
  const arrow = /^(?:\([^)]*\)|[a-zA-Z_$][\w$]*)\s*(?::[^=]+?)?=>\s*/u.exec(bare);
  if (arrow !== null) {
    const body = bare.slice(arrow[0].length);
    if (!body.startsWith("{")) return keyFamilies(body, path, at, seen);
    const returns = [...body.matchAll(/\breturn\s+/gu)].map((one) =>
      keyFamilies(expressionAt(body, (one.index ?? 0) + one[0].length), path, at, seen)
    );
    if (returns.length === 0 || returns.some((one) => one === undefined)) return undefined;
    return returns.flatMap((one) => (Array.isArray(one) ? one : []));
  }
  if (bare.startsWith("[")) {
    const head = /^\[\s*"([a-zA-Z_-]+)"(?:\s*,\s*"([a-zA-Z_-]+)")?/u.exec(bare);
    if (head !== null) return [familyName(head[1] ?? "", head[2])];
    const spread = /^\[\s*\.\.\./u.exec(bare);
    return spread === null
      ? undefined
      : keyFamilies(expressionAt(bare, spread[0].length), path, at, seen);
  }
  if (/\.queryKey$/u.test(bare)) return "named elsewhere";
  const name = /^[a-zA-Z_$][\w$]*$/u.exec(bare)?.[0];
  const called = /^([a-zA-Z_$][\w$]*)\s*\(/u.exec(bare);
  const bound =
    name ??
    (called !== null && closingAt(bare, called[0].length - 1) === bare.length - 1
      ? called[1]
      : undefined);
  if (bound === undefined) return undefined;
  if (name !== undefined && /^[A-Z]/u.test(name)) return "a type";
  const bindings = bindingsOf(bound, path, at);
  if (bindings.length === 0) return undefined;
  const families: string[] = [];
  for (const binding of bindings) {
    const key = `${binding.path}:${binding.at}`;
    if (seen.has(key)) return undefined;
    for (const bindingExpression of binding.expressions()) {
      const resolved = keyFamilies(
        bindingExpression,
        binding.path,
        binding.at,
        new Set([...seen, key])
      );
      if (resolved === undefined) return undefined;
      if (Array.isArray(resolved)) families.push(...resolved);
    }
  }
  return families;
}

/** One place the source keys a query, by its file and line. */
interface KeySite {
  readonly where: string;
  readonly families: readonly string[];
}

/** Each `queryKey:` of the source: C-Q1's literal families, and each computed key's (C-Q1's
 *  second command, `queryKey: *[^\[ ]`, and the keys neither of its commands reads: a literal
 *  spread from another key, `queryKey: [...myRepliesKey, "count"]`, or one whose array opens on
 *  the next line). A computed key in a comment is no key. */
function keySites(): { readonly sites: KeySite[]; readonly unread: string[] } {
  const sites: KeySite[] = [];
  const unread: string[] = [];
  for (const { path, text } of SOURCES) {
    for (const found of text.matchAll(/queryKey:/gu)) {
      const at = found.index ?? 0;
      const line = text.slice(0, at).split("\n").length;
      const where = `${path}:${line}`;
      LITERAL_KEY.lastIndex = at;
      const literal = LITERAL_KEY.exec(text);
      if (literal !== null) {
        sites.push({ families: [familyName(literal[1] ?? "", literal[2])], where });
        continue;
      }
      const lineStart = text.lastIndexOf("\n", at) + 1;
      if (/\/\/|^\s*\*/u.test(text.slice(lineStart, at))) continue;
      const expression = expressionAt(text, at + found[0].length);
      const resolved = keyFamilies(expression, path, at);
      if (resolved === undefined) unread.push(`${where}: queryKey: ${expression}`);
      else if (Array.isArray(resolved)) sites.push({ families: resolved, where });
    }
  }
  return { sites, unread };
}

const { sites: KEY_SITES, unread: UNREAD_KEYS } = keySites();

/** Every family the source keys a query under, with the first place it does. */
const SOURCE_FAMILIES: ReadonlyMap<string, string> = new Map(
  KEY_SITES.flatMap(({ families, where }) =>
    families.map((family) => [family, where] as const)
  ).reverse()
);

test("every computed query key resolves to the families it names", () => {
  expect(
    UNREAD_KEYS.map(
      (site) => `${site}: write the key as a literal array, or as a name this test resolves`
    )
  ).toEqual([]);
  // The positive control: the reader finds C-Q1's families and the computed ones C-Q1 misses.
  expect(SOURCE_FAMILIES.has("issue-messages")).toBe(true);
  expect(SOURCE_FAMILIES.has("me replies")).toBe(true);
});

test("every query family the source keys is written by a correction or listed as holding no post", () => {
  const unlisted = [...SOURCE_FAMILIES]
    .filter(
      ([family]) =>
        !Object.hasOwn(CORRECTED_POST_FAMILIES, family) && !Object.hasOwn(NO_POST_FAMILIES, family)
    )
    .map(
      ([family, where]) =>
        `query family "${family}" (${where}) is in neither list: write it in CORRECTED_POST_FAMILIES, or list it in NO_POST_FAMILIES with why it holds no post`
    );
  expect(unlisted).toEqual([]);
});

test("every family either list names is still keyed in the source, and in one list alone", () => {
  const listed = [...Object.keys(CORRECTED_POST_FAMILIES), ...Object.keys(NO_POST_FAMILIES)];
  expect(
    listed
      .filter((family) => !SOURCE_FAMILIES.has(family))
      .map((family) => `listed family "${family}" no longer appears in the source`)
  ).toEqual([]);
  expect(
    Object.keys(CORRECTED_POST_FAMILIES).filter((family) => Object.hasOwn(NO_POST_FAMILIES, family))
  ).toEqual([]);
  for (const [family, reason] of Object.entries(NO_POST_FAMILIES)) {
    expect(reason.trim().length, family).toBeGreaterThan(0);
  }
});

// Each written family has a row here below: an answer that holds the post's words in every field
// the family gives them in, which a delete must leave none of. These tests drive a family's
// correction through a refetch that fails as well, each in the file it names.
const WRITTEN_ELSEWHERE: Readonly<
  Record<string, { readonly file: string; readonly test: string }>
> = {
  "ask-thread": {
    file: "features/inbox/AskThread.test.tsx",
    test: "a thread whose refetch fails after a reply's edit and delete reads the reply as edited, then deleted",
  },
  // The document read by its slug holds no post; it lists the document's versions, whose name a
  // comment's delete renames (`renamed_version`).
  "artifact-ref": {
    file: "api/corrected-post-versions.test.ts",
    test: "a version a comment's delete renamed takes its new name in a project document's read by its slug, whose refetch fails",
  },
  comment: {
    file: "features/refs/reference-target.test.ts",
    test: "a post's reference whose refetch fails after its edit and delete reads as edited, then deleted",
  },
  comments: {
    file: "features/margin/PinnedTab.test.tsx",
    test: "a pinned comment's and message's cards whose refetch fails read as edited, then deleted",
  },
  // The issue's details hold no post; they list each document's versions, whose name a comment's
  // delete renames (`renamed_version`). Its references read, which names posts, is driven through
  // a refetch that fails below, in this file.
  issue: {
    file: "api/corrected-post-versions.test.ts",
    test: "a version a comment's delete renamed takes its new name in the issue's details, whose refetch fails",
  },
  "issue-message": {
    file: "features/refs/reference-target.test.ts",
    test: "a post's reference whose refetch fails after its edit and delete reads as edited, then deleted",
  },
  "issue-messages": {
    file: "api/corrected-post-messages.test.ts",
    test: "a message's delete writes its mark into the issue's message list, as into the pinned copy",
  },
  "me replies": {
    file: "features/inbox/useInboxThreadRows.test.tsx",
    test: "a thread row whose refetch fails after its post's edit and delete reads as edited, then deleted",
  },
  "reference-targets": {
    file: "api/correction-post-addresses.test.ts",
    test: "a delete writes its mark into each kept member naming the post, and no other",
  },
  search: {
    file: "features/search/SearchPalette.test.tsx",
    test: "a post's hit whose refetch fails after its edit and delete carries the edited mark, then the deleted line",
  },
};

const POST = "5e6f7a8b-9c0d-4e1f-8a2b-3c4d5e6f7a8b";
/** Another comment or message: a reply quoting the post, or the root of a thread the post replies
 *  in. */
const OTHER = "6f7a8b9c-0d1e-4f2a-8b3c-4d5e6f7a8b9c";
const OLD_WORDS = "Hold the deploy until Friday.";
/** The post's words as the tree a read gives beside them. */
const WORDS_TREE = { content: [{ text: OLD_WORDS, type: "text" }], type: "doc" };

/** The post as a comment, or a message, as a read answers it before its correction. */
function commentOf(id = POST, more: Partial<CommentWithTrees> = {}): CommentWithTrees {
  return {
    anchor: null,
    author: { id: "alice", kind: "user" },
    body: OLD_WORDS,
    created_at: "2026-11-19T00:00:00Z",
    deleted_at: null,
    deleted_by: null,
    edited_at: null,
    id,
    issue_key: "CORE-1",
    reply_block: null,
    reply_to: null,
    suggestion: null,
    ...more,
  } as CommentWithTrees;
}

function messageOf(id = POST): MessageWithTrees {
  return {
    author: { id: "alice", kind: "user" },
    body: OLD_WORDS,
    broadcast_id: null,
    created_at: "2026-11-19T00:00:00Z",
    deliveries: [],
    id,
    in_reply_to: null,
    issue_key: "CORE-1",
    reply_block: null,
    target: "session:019a1c01-0d7b-7a2a-8e93-000000000a01",
    ...messageCorrectionFields(),
  };
}

/** The post's anchor as a pending insert suggestion holds it: its quote is the text the insert put
 *  in the document, the post's own words, which the post's delete empties (`clearDeletedPostBody`
 *  in `api/post_history.go`). */
const INSERT_ANCHOR = {
  artifact_id: "doc-1",
  block_id: null,
  mark_id: "mark-1",
  orphaned: false,
  quote: OLD_WORDS,
  version: 1,
};

/** The post as a comment with its words in every field a read gives them in: a pending insert
 *  suggestion's body and its tree, its replacement text and its anchor quote. */
const insertComment = () => ({
  ...commentOf(),
  anchor: INSERT_ANCHOR,
  body_tree: WORDS_TREE,
  suggestion: { accepted: null, replace_with: OLD_WORDS },
});

/** The post as a message, with its words in its body and its tree. */
const wordyMessage = () => ({ ...messageOf(), body_tree: WORDS_TREE });

/** A reply of `kind` quoting block b1 of the post, with the block's text and tree as a read of the
 *  reply gives them (`reply_block`). */
function quotingReply(kind: "comment" | "message") {
  const reply_block = {
    block_id: "b1",
    node: `${kind}/${POST}#b1`,
    removed: false,
    text: OLD_WORDS,
    text_tree: WORDS_TREE,
  };
  return kind === "comment"
    ? { ...commentOf(OTHER), body: "Agreed.", reply_block, reply_to: POST }
    : { ...messageOf(OTHER), body: "Agreed.", in_reply_to: POST, reply_block };
}

/** An ask's thread, as its read and an ask row carry it: the post as a reply in it, and a reply
 *  quoting a block of the post. */
const askThread = () => ({
  ask: { id: "ask-1" },
  replies: [insertComment(), quotingReply("comment")],
});

/** doc-1 as a read lists its versions: version 2, which an accept of the post's suggestion named
 *  after the post's body. */
const DOCUMENT = {
  id: "doc-1",
  name: "spec.md",
  versions: [
    { number: 1, summary: null },
    { number: 2, summary: OLD_WORDS },
  ],
};

/** A read of version 2 of doc-1 alone, with its name. */
const VERSION_TWO = { markdown: "Hold the deploy until Monday.\n", number: 2, summary: OLD_WORDS };

/** Two Inbox thread rows of `kind` (`GET /api/v1/me/replies`): one whose root is the post, with its
 *  words as the row's subject and, for a comment, the root's anchor, whose quote the card prints
 *  (`ThreadQuote`); and one whose latest reply is the post, with its words as that reply's first
 *  line. */
function threadRows(kind: "comment" | "message") {
  return [
    replyRow(POST, {
      kind,
      subject: OLD_WORDS,
      ...(kind === "comment" ? { anchor: INSERT_ANCHOR } : {}),
    }),
    replyRow(OTHER, { kind, reply: { ...replyRow(OTHER).reply, first_line: OLD_WORDS, id: POST } }),
  ];
}

/** What a row reads of the post's copy an answer holds. */
interface CopyMarks {
  readonly deleted_at?: string | null;
  readonly edited_at?: string | null;
}

/** One written family's cached answer holding a copy of the post, and where in it the copy is. */
interface WriteRow {
  readonly family: string;
  readonly kind: "comment" | "message";
  readonly key: QueryKey;
  readonly answer: unknown;
  /** Each copy of the post the answer holds that an edit and a delete both mark. */
  readonly copies: (answer: unknown) => readonly CopyMarks[];
  /** Each copy a delete marks and an edit does not: a thread row's latest reply. */
  readonly deletedCopies?: (answer: unknown) => readonly CopyMarks[];
  /** The delete the answer is written: the post's own (`POST_DELETES`), unless the family holds
   *  a copy only another delete writes. */
  readonly deleted?: DispatchEvent;
}

const at = (value: unknown, ...path: (string | number)[]): unknown =>
  path.reduce<unknown>(
    (held, step) =>
      typeof held === "object" && held !== null ? Reflect.get(held, step) : undefined,
    value
  );
const marksAt = (value: unknown, ...path: (string | number)[]): CopyMarks =>
  (at(value, ...path) ?? {}) as CopyMarks;
/** Where the copies are in `askThread` held at `path`: the post, and the quote of a block of it. */
const threadCopies = (answer: unknown, ...path: (string | number)[]): CopyMarks[] => [
  marksAt(answer, ...path, "replies", 0),
  marksAt(answer, ...path, "replies", 1, "reply_block"),
];

const commentEvent: DispatchEvent = {
  actor: { id: "alice", kind: "user" },
  artifact_id: null,
  created_at: "2026-11-19T00:00:00Z",
  id: 7,
  issue_key: "CORE-1",
  notify: true,
  payload: { ...commentOf(), body_truncated: false },
  project: "CORE",
  seq: 7,
  trees: {},
  type: "comment.created",
} as unknown as DispatchEvent;

/** The post's `created` event of `kind` as the log holds it, with the post's words in every field:
 *  its body and its tree, and a comment's suggestion and anchor quote. */
const postEvent = (kind: "comment" | "message") => ({
  ...commentEvent,
  payload:
    kind === "comment"
      ? {
          ...commentOf(),
          anchor: INSERT_ANCHOR,
          body_truncated: false,
          suggestion: { accepted: null, replace_with: OLD_WORDS },
        }
      : { ...messageOf(), body_truncated: false },
  trees: { "payload.body": WORDS_TREE },
  type: `${kind}.created`,
});

/** The post's delete of each kind: a comment's is the delete of the pending insert suggestion
 *  `insertComment` is, whose event carries the anchor with the quote the delete emptied. */
const POST_DELETES = {
  comment: correction("comment", POST, false, { anchor: { ...INSERT_ANCHOR, quote: "" } }),
  message: correction("message", POST, false),
};

/** The delete of the post once an accept made its suggestion document text, so the anchor keeps
 *  its quote, and named version 2 of doc-1 after the post's body: the delete gives that version a
 *  name of none of it (`renamed_version`). */
const ACCEPTED_DELETE = correction("comment", POST, false, {
  anchor: { ...INSERT_ANCHOR, quote: "Friday" },
  renamed_version: {
    artifact_id: "doc-1",
    number: 2,
    summary: 'Accepted suggestion: "Friday" → "Monday"',
  },
});

/** A row of `issue`'s references read (`["issue", key, "references"]`) whose node is `node`, named
 *  as the read names a post or a block of it: by the head of its words. */
const referenceRow = (issue: string, node: ReferenceLink["node"], name = OLD_WORDS) => ({
  kinds: ["mentions"],
  name,
  node,
  via: { id: issue, kind: "issue" },
});

/** CORE-1's references read: in `linked_from`, the post as a comment on OPS-1 that links to
 *  CORE-1, and another comment, named by words of its own. */
const commentReferences = (): IssueReferences => ({
  linked_from: [
    referenceRow(
      "CORE-1",
      graphNode({
        id: POST,
        issue_key: "OPS-1",
        kind: "comment",
        project: "OPS",
        ref: `dispatch://OPS-1/comment/${POST}`,
      })
    ),
    referenceRow(
      "CORE-1",
      graphNode({
        id: OTHER,
        issue_key: "OPS-1",
        kind: "comment",
        project: "OPS",
        ref: `dispatch://OPS-1/comment/${OTHER}`,
      }),
      "Agreed."
    ),
  ],
  links: [],
  members: [],
  truncated: false,
});

/** CORE-2's references read: in `links`, block b1 of the post as a message on OPS-1, which
 *  CORE-2's own content links to, and another message, named by words of its own. */
const messageBlockReferences = (): IssueReferences => ({
  linked_from: [],
  links: [
    referenceRow(
      "CORE-2",
      graphNode({
        block_id: "b1",
        id: `message/${POST}#b1`,
        issue_key: "OPS-1",
        kind: "block",
        parent: { id: POST, kind: "message", ref: `dispatch://OPS-1/message/${POST}` },
        project: "OPS",
        ref: `dispatch://OPS-1/message/${POST}#b-b1`,
      })
    ),
    referenceRow(
      "CORE-2",
      graphNode({
        id: OTHER,
        issue_key: "OPS-1",
        kind: "message",
        project: "OPS",
        ref: `dispatch://OPS-1/message/${OTHER}`,
      }),
      "Agreed."
    ),
  ],
  members: [],
  truncated: false,
});

const ROWS: readonly WriteRow[] = [
  {
    answer: askThread(),
    copies: (answer) => threadCopies(answer),
    family: "ask",
    key: ["ask", "ask-1"],
    kind: "comment",
  },
  {
    answer: askThread(),
    copies: (answer) => threadCopies(answer),
    family: "ask-thread",
    key: ["ask-thread", "ask-1"],
    kind: "comment",
  },
  {
    answer: [{ id: "ask-1", thread: askThread() }],
    copies: (answer) => threadCopies(answer, 0, "thread"),
    family: "asks",
    key: ["asks", "CORE-1", "thread"],
    kind: "comment",
  },
  {
    answer: [{ id: "ask-1", thread: askThread() }],
    copies: (answer) => threadCopies(answer, 0, "thread"),
    family: "inbox",
    key: ["inbox"],
    kind: "comment",
  },
  {
    answer: [insertComment(), quotingReply("comment")],
    copies: (answer) => [marksAt(answer, 0), marksAt(answer, 1, "reply_block")],
    family: "artifact",
    key: ["artifact", "doc-1", "comments"],
    kind: "comment",
  },
  {
    answer: [{ id: "ask-2", thread: askThread() }],
    copies: (answer) => threadCopies(answer, 0, "thread"),
    family: "artifact",
    key: ["artifact", "doc-1", "asks"],
    kind: "comment",
  },
  // The reads that hold a version's name, which the accept took from the post's body.
  {
    answer: DOCUMENT,
    copies: () => [],
    deleted: ACCEPTED_DELETE,
    family: "artifact",
    key: ["artifact", "doc-1"],
    kind: "comment",
  },
  {
    answer: VERSION_TWO,
    copies: () => [],
    deleted: ACCEPTED_DELETE,
    family: "artifact",
    key: ["artifact", "doc-1", "version", 2],
    kind: "comment",
  },
  {
    answer: VERSION_TWO,
    copies: () => [],
    deleted: ACCEPTED_DELETE,
    family: "artifact",
    key: ["artifact", "doc-1", 2],
    kind: "comment",
  },
  {
    answer: DOCUMENT,
    copies: () => [],
    deleted: ACCEPTED_DELETE,
    family: "artifact-ref",
    key: ["artifact-ref", "CORE/spec"],
    kind: "comment",
  },
  {
    answer: { artifacts: [DOCUMENT], key: "CORE-1" },
    copies: () => [],
    deleted: ACCEPTED_DELETE,
    family: "issue",
    key: ["issue", "CORE-1"],
    kind: "comment",
  },
  {
    answer: { comment: insertComment(), replies: [quotingReply("comment")] },
    copies: (answer) => [marksAt(answer, "comment"), marksAt(answer, "replies", 0, "reply_block")],
    family: "comment",
    key: ["comment", POST],
    kind: "comment",
  },
  {
    answer: [insertComment(), quotingReply("comment")],
    copies: (answer) => [marksAt(answer, 0), marksAt(answer, 1, "reply_block")],
    family: "comments",
    key: ["comments", "CORE-1"],
    kind: "comment",
  },
  {
    answer: { pageParams: [0], pages: [[postEvent("comment")]] },
    copies: (answer) => [marksAt(answer, "pages", 0, 0, "payload")],
    family: "events",
    key: ["events", "CORE-1"],
    kind: "comment",
  },
  {
    answer: [postEvent("message")],
    copies: (answer) => [marksAt(answer, 0, "payload")],
    family: "events",
    key: ["events", "CORE-1", "margin-pinned", [POST]],
    kind: "message",
  },
  {
    answer: [{ message: wordyMessage(), replies: [quotingReply("message")] }],
    copies: (answer) => [
      marksAt(answer, 0, "message"),
      marksAt(answer, 0, "replies", 0, "reply_block"),
    ],
    family: "agents",
    key: ["agents", "019a1c01-0d7b-7a2a-8e93-000000000a01", "messages"],
    kind: "message",
  },
  {
    answer: { message: wordyMessage(), replies: [quotingReply("message")] },
    copies: (answer) => [marksAt(answer, "message"), marksAt(answer, "replies", 0, "reply_block")],
    family: "issue-message",
    key: ["issue-message", "CORE-1", POST],
    kind: "message",
  },
  {
    answer: [{ message: wordyMessage(), replies: [quotingReply("message")] }],
    copies: (answer) => [
      marksAt(answer, 0, "message"),
      marksAt(answer, 0, "replies", 0, "reply_block"),
    ],
    family: "issue-messages",
    key: ["issue-messages", "CORE-1"],
    kind: "message",
  },
  {
    answer: {
      pageParams: [0],
      pages: [
        {
          limit: 20,
          offset: 0,
          rows: [
            {
              ask_id: "ask-1",
              kind: "reply",
              reply: { body: OLD_WORDS, body_tree: WORDS_TREE, id: POST },
            },
          ],
          total: 1,
        },
      ],
    },
    copies: (answer) => [marksAt(answer, "pages", 0, "rows", 0, "reply")],
    family: "me answers",
    key: ["me", "answers"],
    kind: "comment",
  },
  // What the reader dismissed (pages), and one owner's unread set (one page).
  {
    answer: {
      pageParams: [0],
      pages: [{ limit: 20, offset: 0, rows: threadRows("comment"), total: 2 }],
    },
    copies: (answer) => [marksAt(answer, "pages", 0, "rows", 0)],
    deletedCopies: (answer) => [marksAt(answer, "pages", 0, "rows", 1, "reply")],
    family: "me replies",
    key: ["me", "replies", "dismissed"],
    kind: "comment",
  },
  {
    answer: { limit: 20, offset: 0, rows: threadRows("message"), total: 2 },
    copies: (answer) => [marksAt(answer, "rows", 0)],
    deletedCopies: (answer) => [marksAt(answer, "rows", 1, "reply")],
    family: "me replies",
    key: ["me", "replies", "unread", "CORE-1"],
    kind: "message",
  },
  // A person's page (`PersonPage`, pages): the post's row, named by the first line of its words,
  // beside another comment's.
  {
    answer: {
      pageParams: [0],
      pages: [
        {
          limit: 50,
          offset: 0,
          person: "alice",
          rows: [
            {
              created_at: "2026-11-19T00:00:00Z",
              href: `/issues/CORE-1/comments/${POST}`,
              id: POST,
              kind: "comment",
              relations: ["authored_by"],
              title: OLD_WORDS,
            },
            {
              created_at: "2026-11-18T00:00:00Z",
              href: `/issues/CORE-1/comments/${OTHER}`,
              id: OTHER,
              kind: "comment",
              relations: ["authored_by"],
              title: "Agreed.",
            },
          ],
          total: 2,
        },
      ],
    },
    copies: (answer) => [marksAt(answer, "pages", 0, "rows", 0)],
    family: "people",
    key: ["people", "alice", "nodes"],
    kind: "comment",
  },
  // A names read naming the post, and a block of it, with each member's words.
  {
    answer: {
      targets: [
        { gone: false, kind: "comment", name: OLD_WORDS, ref: `dispatch://CORE-1/comment/${POST}` },
        {
          gone: false,
          kind: "block",
          name: OLD_WORDS,
          ref: `dispatch://CORE-1/comment/${POST}#b-b1`,
          text_tree: WORDS_TREE,
        },
      ],
    },
    copies: (answer) => [marksAt(answer, "targets", 0), marksAt(answer, "targets", 1)],
    family: "reference-targets",
    key: referenceTargetsQuery([
      `dispatch://CORE-1/comment/${POST}`,
      `dispatch://CORE-1/comment/${POST}#b-b1`,
    ]).queryKey,
    kind: "comment",
  },
  // The Referenced by panel's list (`ReferencedBy.tsx`), which it draws beside "Referenced by
  // unavailable" when its read fails: an edge from the comment, and one from a block of it.
  {
    answer: {
      edges: [
        {
          created_at: "2026-11-19T00:00:00Z",
          direction: "in",
          excerpt: { text: OLD_WORDS },
          kind: "mentions",
          node: { id: POST, kind: "comment" },
          source_seq: 7,
        },
        {
          created_at: "2026-11-19T00:00:00Z",
          direction: "in",
          excerpt: { block_id: "b2", text: OLD_WORDS },
          kind: "mentions",
          node: {
            block_id: "b2",
            id: `comment/${POST}#b2`,
            kind: "block",
            parent: { id: POST, kind: "comment" },
          },
          source_seq: 8,
        },
      ],
      node: { id: "CORE-1", kind: "issue" },
    },
    copies: (answer) => [marksAt(answer, "edges", 0, "node"), marksAt(answer, "edges", 1, "node")],
    family: "references to",
    key: ["references", "to", "dispatch://CORE-1"],
    kind: "comment",
  },
  // A read of a block of the post pinned to its first revision: its text, its tree and the text at
  // the pin.
  {
    answer: {
      edges: [],
      node: {
        block_id: "b2",
        id: `comment/${POST}#b2`,
        kind: "block",
        parent: { id: POST, kind: "comment" },
        pin: {
          ref: `dispatch://CORE-1/comment/${POST}#b-b2@v1`,
          state: "unchanged",
          text: OLD_WORDS,
          text_tree: WORDS_TREE,
          version: 1,
        },
        text: OLD_WORDS,
        text_tree: WORDS_TREE,
      },
    },
    copies: (answer) => [marksAt(answer, "node")],
    family: "references to",
    key: ["references", "to", `dispatch://CORE-1/comment/${POST}#b-b2@v1`],
    kind: "comment",
  },
  // The Artifacts tab's references read (`ReferenceLinkRow`), which names each row's post, or the
  // post of its block, by the head of its words: a comment that links to the issue, and a block
  // of a message the issue links to.
  {
    answer: commentReferences(),
    copies: (answer) => [marksAt(answer, "linked_from", 0, "node")],
    family: "issue",
    key: ["issue", "CORE-1", "references"],
    kind: "comment",
  },
  {
    answer: messageBlockReferences(),
    copies: (answer) => [marksAt(answer, "links", 0, "node")],
    family: "issue",
    key: ["issue", "CORE-2", "references"],
    kind: "message",
  },
  {
    answer: {
      results: [
        {
          href: `/issues/CORE-1/comments/${POST}`,
          id: POST,
          kind: "comment",
          rank: 1,
          snippet: OLD_WORDS,
          snippet_tree: WORDS_TREE,
        },
      ],
    },
    copies: (answer) => [marksAt(answer, "results", 0)],
    family: "search",
    key: ["search", "deploy", {}],
    kind: "comment",
  },
];

test("every family a correction writes has a row that proves the write", () => {
  const here = new Set(ROWS.map((row) => row.family));
  expect(Object.keys(CORRECTED_POST_FAMILIES).filter((family) => !here.has(family))).toEqual([]);
  for (const [family, { file, test: name }] of Object.entries(WRITTEN_ELSEWHERE)) {
    expect(readFileSync(join(SOURCE_ROOT, file), "utf8"), family).toContain(name);
  }
});

// A family whose refusal a correction writes (`REFUSAL_FAMILIES`) is one whose answers it writes
// too, and its refusal's write is proved below by tests named after it (`<family>: …`); every
// family those tests name is in the table.
test("every family whose refusal a correction writes has its answers written too, and tests here of the refusal's write", () => {
  const named = new Set(
    [...readFileSync(import.meta.path, "utf8").matchAll(/^test\("([a-z -]+): /gmu)].map(
      (match) => match[1]
    )
  );
  const refused = Object.keys(REFUSAL_FAMILIES);
  expect({
    answersUnwritten: refused.filter((family) => !Object.hasOwn(CORRECTED_POST_FAMILIES, family)),
    tested: [...named].sort(),
  }).toEqual({ answersUnwritten: [], tested: [...refused].sort() });
});

for (const row of ROWS) {
  test(`a delete writes its mark into ${row.family} (${JSON.stringify(row.key)}), and keeps none of the post's words in any field`, () => {
    const client = new QueryClient();
    client.setQueryData(row.key, row.answer);
    expect(JSON.stringify(client.getQueryData(row.key))).toContain(OLD_WORDS);
    markCorrectedPost(client, row.deleted ?? POST_DELETES[row.kind]);
    const answer = client.getQueryData(row.key);
    const marked = [...row.copies(answer), ...(row.deletedCopies?.(answer) ?? [])];
    expect(marked.map((copy) => copy.deleted_at)).toEqual(marked.map(() => DELETION.deleted_at));
    expect(JSON.stringify(answer)).not.toContain(OLD_WORDS);
  });

  test(`an edit writes its time into ${row.family} (${JSON.stringify(row.key)})`, () => {
    const client = new QueryClient();
    client.setQueryData(row.key, row.answer);
    markCorrectedPost(client, correction(row.kind, POST, true));
    const answer = client.getQueryData(row.key);
    expect(row.copies(answer).map((copy) => copy.edited_at)).toEqual(
      row.copies(row.answer).map(() => EDITED_AT)
    );
    expect(JSON.stringify(answer)).toContain(OLD_WORDS);
  });
}

// The issue's references read names the row of the post, or of a block of it, by the head of the
// post's words: a delete empties that name beside the node's mark, and leaves the row of another
// post as the read gave it.
test("a delete empties the name of an issue's references row whose node is the post or a block of it, and leaves another post's row as it was", () => {
  const reads = [
    {
      key: ["issue", "CORE-1", "references"],
      kind: "comment",
      list: "linked_from",
      read: commentReferences,
    },
    {
      key: ["issue", "CORE-2", "references"],
      kind: "message",
      list: "links",
      read: messageBlockReferences,
    },
  ] as const;
  for (const { key, kind, list, read } of reads) {
    const client = new QueryClient();
    client.setQueryData(key, read());
    markCorrectedPost(client, POST_DELETES[kind]);
    const rows = client.getQueryData<IssueReferences>(key)?.[list];
    const post = rows?.[0];
    expect(
      { deleted_at: post?.node.deleted_at, deleted_by: post?.node.deleted_by, name: post?.name },
      kind
    ).toEqual({ ...DELETION, name: "" });
    expect(rows?.[1], kind).toEqual(read()[list]?.[1]);
  }
});

// The architect's decision of 2026-11-21 03:47Z on census rows 7.13 and 8.54: Dispatch refuses the
// graph read of an id written short that names several items (`ID_AMBIGUOUS`), listing each with
// its first line, and the page holds that refusal as the query's error, not as its answer. The
// family's writer writes a correction into it as into an answer (`REFUSAL_FAMILIES`): the post's
// item carries the edit's time, or loses its first line beside the delete's mark, and the refusal
// stays a refusal, its other items as they were, so the list the reader chooses from stays drawn.
const PREFIX_READ = ["references", "to", "dispatch://CORE-1/comment/5e6f7a8b"];

/** Dispatch's refusal of the prefix read: the post and another comment, each with its first line,
 *  as the prefix layer lists them (`refs.Candidate`). */
function prefixRefusal(): ApiError {
  const item = (id: string, first_line: string): IDCandidate => ({
    first_line,
    id,
    issue_key: "CORE-1",
    kind: "comment",
    ref: `dispatch://CORE-1/comment/${id}`,
  });
  return new ApiError(409, {
    candidates: [item(POST, OLD_WORDS), item(OTHER, "Agreed.")],
    code: ID_AMBIGUOUS,
    error: 'the id prefix "5e6f7a8b" names 2 items; write more of the id',
  });
}

/** The items of the refusal the prefix read holds as its error, where it holds one. */
function refusedItems(client: QueryClient): readonly IDCandidate[] | undefined {
  return ambiguousItems(client.getQueryState(PREFIX_READ)?.error);
}

test("references to: an edit writes its time, and a delete its mark, into a refusal of an id written short that lists the post, which stays a refusal and keeps none of the post's first line", async () => {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  await client
    .fetchQuery({ queryFn: () => Promise.reject(prefixRefusal()), queryKey: PREFIX_READ })
    .catch(() => undefined);
  const other = refusedItems(client)?.[1];

  markCorrectedPost(client, correction("comment", POST, true));
  expect(refusedItems(client)?.[0]).toMatchObject({ edited_at: EDITED_AT, first_line: OLD_WORDS });

  markCorrectedPost(client, correction("comment", POST, false));
  const state = client.getQueryState(PREFIX_READ);
  const items = refusedItems(client);
  expect({
    code: state?.error instanceof ApiError ? state.error.code : undefined,
    data: state?.data,
    item: items?.[0],
    other: items?.[1] === other,
    status: state?.status,
  }).toEqual({
    code: ID_AMBIGUOUS,
    data: undefined,
    item: {
      ...DELETION,
      edited_at: EDITED_AT,
      id: POST,
      issue_key: "CORE-1",
      kind: "comment",
      ref: `dispatch://CORE-1/comment/${POST}`,
    },
    other: true,
    status: "error",
  });
  expect(JSON.stringify(items)).not.toContain(OLD_WORDS);
  client.clear();
});

// A read of the prefix out as the delete arrives, with no refusal before it, was asked before the
// delete: the refusal it lands can list the post as it stood, so the delete is kept for that read
// and written into the refusal as it lands, as into an answer such a read lands.
test("references to: a refusal a read out at the delete lands takes the delete's mark", async () => {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const held = Promise.withResolvers<never>();
  const out = client
    .fetchQuery({ queryFn: () => held.promise, queryKey: PREFIX_READ })
    .catch(() => undefined);
  markCorrectedPost(client, correction("comment", POST, false));
  held.reject(prefixRefusal());
  await out;
  expect(refusedItems(client)?.[0]?.deleted_at).toBe(DELETION.deleted_at);
  expect(JSON.stringify(refusedItems(client))).not.toContain(OLD_WORDS);
  client.clear();
});

// TanStack empties a query's error while a read of it that holds no answer is out (`fetchState`),
// so while a refused prefix is read again the cache holds no refusal, and the outline draws the
// list it last drew (census row 8.54). A delete that arrives then gives that read up, which puts
// the refusal back, writes its mark there, and reads the prefix again, holding the written refusal
// beside the read asked again: the cache holds the list as the delete left it while that read is
// out.
test("references to: a delete that arrives while a refused prefix is read again marks the refusal, which the cache holds while the read it asks again is out", async () => {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  // How each read of the prefix asked so far is refused, in the order they were asked.
  const refusals: ((reason: unknown) => void)[] = [];
  const observer = new QueryObserver(client, {
    queryFn: () => {
      const { promise, reject } = Promise.withResolvers<never>();
      refusals.push(reject);
      return promise;
    },
    queryKey: PREFIX_READ,
  });
  const unsubscribe = observer.subscribe(() => {});
  try {
    await waitFor(() => expect(refusals.length).toBe(1));
    refusals[0]?.(prefixRefusal());
    await waitFor(() => expect(client.getQueryState(PREFIX_READ)?.status).toBe("error"));
    void observer.refetch();
    await waitFor(() => expect(refusals.length).toBe(2));
    expect(client.getQueryState(PREFIX_READ)?.error, "TanStack kept the refusal").toBeNull();

    markCorrectedPost(client, correction("comment", POST, false));
    await waitFor(() => expect(refusals.length).toBe(3));
    expect(client.getQueryState(PREFIX_READ)?.fetchStatus).toBe("fetching");
    expect(refusedItems(client)?.[0]?.deleted_at).toBe(DELETION.deleted_at);
    expect(JSON.stringify(refusedItems(client))).not.toContain(OLD_WORDS);
  } finally {
    unsubscribe();
    client.clear();
  }
});

// A prefix read can hold an answer beside a refusal: the id written short named the post once,
// and a later read was refused (`ID_AMBIGUOUS`), listing the post among others; TanStack keeps the
// answer and holds the refusal as the read's error, so the page lists the matches. The post's
// delete writes its mark into both, as a read out at it is given up and the prefix asked again:
// the refusal stays the read's error and its status, so the page still lists the matches, with the
// post's line marked deleted, and does not take the prefix as resolved to the post, as writing the
// answer by `setQueryData` would (it clears the error). Giving up the read asked again puts back
// the written state, not the one from before the delete.
test("references to: a delete writes its mark into an answer a refused prefix read holds beside its refusal, which stays the read's error", async () => {
  const answer = { edges: [], node: graphNode({ id: POST, kind: "comment", text: OLD_WORDS }) };
  const later = Promise.withResolvers<never>();
  let asked = 0;
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  client.setQueryData(PREFIX_READ, answer);
  const page = new QueryObserver(client, {
    queryFn: () => {
      asked += 1;
      return asked === 1 ? Promise.reject(prefixRefusal()) : later.promise;
    },
    queryKey: PREFIX_READ,
  });
  const unsubscribe = page.subscribe(() => {});
  /** What the read holds and how the page reads it. */
  const held = () => {
    const state = client.getQueryState(PREFIX_READ);
    const node = (state?.data as GraphReferences | undefined)?.node;
    return {
      isError: page.getCurrentResult().isError,
      items: refusedItems(client)?.map((item) => [item.id, item.first_line, item.deleted_at]),
      node: [node?.deleted_at, node?.text],
      status: state?.status,
      words: JSON.stringify([state?.data, state?.error]).includes(OLD_WORDS),
    };
  };
  const written = {
    isError: true,
    items: [
      [POST, undefined, DELETION.deleted_at],
      [OTHER, "Agreed.", undefined],
    ],
    node: [DELETION.deleted_at, ""],
    status: "error" as const,
    words: false,
  };
  try {
    await waitFor(() => expect(client.getQueryState(PREFIX_READ)?.status).toBe("error"));
    // A read of the prefix is out beside the answer and the refusal as the delete arrives.
    void page.refetch();
    await waitFor(() => expect(asked).toBe(2));
    markCorrectedPost(client, correction("comment", POST, false));
    const atDelete = { ...held(), askedAgain: asked === 3 };
    await client.cancelQueries({ exact: true, queryKey: PREFIX_READ });
    expect({ atDelete, givenUp: held() }).toEqual({
      atDelete: { ...written, askedAgain: true },
      givenUp: written,
    });
  } finally {
    unsubscribe();
    client.clear();
  }
});

// The architect's decision of 2026-11-20 07:52Z: an answer composed before a correction cannot take
// the correction's mark out. A read already out when the delete arrives was asked before it, so
// the feed puts the delete's mark over it (`replaceQueryData`): the read is given up in the same
// step and the list read again, so the answer the held read composed lands nowhere, and the
// refetch, which fails here, leaves the mark where it is.
test("an answer a read out at a delete lands with the post's words keeps the delete's mark, and no body, when the refetch after it fails", async () => {
  const list = ["issue-messages", "CORE-1"];
  const read: MessageRead = { message: messageOf(), replies: [] };
  const held = Promise.withResolvers<MessageRead[]>();
  let reads = 0;
  const fetchList = (): Promise<MessageRead[]> => {
    reads += 1;
    if (reads === 1) return Promise.resolve([read]);
    if (reads === 2) return held.promise;
    return Promise.reject(new Error("the read failed"));
  };
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  client.setQueryData(whoAmIQuery().queryKey, { event_head: "0", kind: "user", login: "alice" });
  const feed = correctionFeed(client);
  const hook = renderHook(
    () => {
      useEventFeed();
      return useQuery({ queryFn: fetchList, queryKey: list });
    },
    {
      wrapper: ({ children }: { children: ReactNode }) =>
        createElement(QueryClientProvider, { client }, children),
    }
  );
  const message = () => client.getQueryData<MessageRead[]>(list)?.[0]?.message;

  try {
    await waitFor(() => expect(hook.result.current.data?.[0]?.message.body).toBe(OLD_WORDS));
    feed.start();
    void hook.result.current.refetch();
    await waitFor(() => expect(reads).toBe(2));
    // The answer the held read composed before the delete lands once the feed has written the
    // delete's mark.
    const landAfterTheMark = client.getQueryCache().subscribe(() => {
      if (message()?.deleted_at !== DELETION.deleted_at) return;
      landAfterTheMark();
      held.resolve([read]);
    });
    await feed.deliver(correction("message", POST, false), [list]);

    expect(reads).toBe(3);
    expect({ body: message()?.body, deleted_at: message()?.deleted_at }).toEqual({
      body: "",
      deleted_at: DELETION.deleted_at,
    });
  } finally {
    hook.unmount();
    feed.restore();
    client.clear();
  }
});

// Census row C.10 of dispatch://INFRA-1523: the issue's references read (`["issue", key,
// "references"]`, the Artifacts tab's) names a comment on another issue that links to it by the
// head of the comment's words. The comment's delete is that other issue's event, which names the
// issue among those whose references it changed (`references_changed`), so the feed sends the
// issue's references read again, and here that read fails. The tab then draws "References
// unavailable" (`ArtifactsTab.tsx`, `QueryError`), not the rows; the answer the cache keeps, which
// the tab draws while the read is retried and once a later read answers, holds the delete's mark
// and an empty name, and none of the comment's words.
test("an issue's references read whose refetch fails after a linking comment's delete keeps the delete's mark, an empty name and none of its words", async () => {
  const deleted = {
    ...correction("comment", POST, false, {
      issue_key: "OPS-1",
      references_changed: [{ id: "CORE-1", kind: "issue" }],
    }),
    issue_key: "OPS-1",
    project: "OPS",
  } as DispatchEvent;
  const read = await readAroundAFailedRefetch(
    ["issue", "CORE-1", "references"],
    commentReferences(),
    deleted,
    (cached) => {
      const row = cached?.linked_from?.[0];
      return {
        deleted_at: row?.node.deleted_at,
        deleted_by: row?.node.deleted_by,
        name: row?.name,
      };
    }
  );
  expect(read.before.name).toBe(OLD_WORDS);
  expect(read.after).toEqual({ ...DELETION, name: "" });
  expect(read.kept).not.toContain(OLD_WORDS);
});

// The bound: a correction is kept for the reads out at its arrival of a query whose answer it
// wrote nothing, and no longer. Here the list has no answer yet, so the delete writes nothing and
// no write gives the read up: the answer it lands was composed before the delete, and is written
// the delete's facts. Once the one read out has landed, an answer the cache is handed is left as
// it is handed.
test("a correction is kept only while a read out at its arrival is out", async () => {
  const list = ["issue-messages", "CORE-1"];
  const read: MessageRead = { message: messageOf(), replies: [] };
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const held = Promise.withResolvers<MessageRead[]>();
  const out = client.fetchQuery({ queryFn: () => held.promise, queryKey: list });
  expect(client.getQueryState(list)?.fetchStatus).toBe("fetching");
  markCorrectedPost(client, correction("message", POST, false));
  held.resolve([read]);
  await out;
  expect(client.getQueryData<MessageRead[]>(list)?.[0]?.message.body).toBe("");

  client.setQueryData(list, [read]);
  expect(client.getQueryData<MessageRead[]>(list)?.[0]?.message.body).toBe(OLD_WORDS);
  client.clear();
});

// The architect's decision of 2026-11-20 13:35Z (W3): a kept correction is written into the answer
// of a read it was kept for, and into no other. Here the delete is kept for the read of CORE-1's
// list, out with no answer yet. CORE-2's list, of the same family, is read and answered while
// that read is still out: the delete was not kept for it, and its answer is left as it lands.
// The held read then lands, and takes the delete's facts.
test("a kept correction is written into the answer of a read it was kept for, and not into another query's", async () => {
  const list = ["issue-messages", "CORE-1"];
  const other = ["issue-messages", "CORE-2"];
  const read: MessageRead = { message: messageOf(), replies: [] };
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const held = Promise.withResolvers<MessageRead[]>();
  const out = client.fetchQuery({ queryFn: () => held.promise, queryKey: list });
  markCorrectedPost(client, correction("message", POST, false));

  await client.fetchQuery({ queryFn: () => Promise.resolve([read]), queryKey: other });
  expect(client.getQueryState(list)?.fetchStatus).toBe("fetching");
  expect(client.getQueryData<MessageRead[]>(other)?.[0]?.message.body).toBe(OLD_WORDS);

  held.resolve([read]);
  await out;
  expect(client.getQueryData<MessageRead[]>(list)?.[0]?.message.body).toBe("");
  client.clear();
});

/** The document a margin shows on CORE-1: its spec. */
const SPEC: Artifact = {
  created_at: "2026-11-19T00:00:00Z",
  created_by: { id: "alice", kind: "user" },
  id: "doc-1",
  issue_key: "CORE-1",
  kind: "doc",
  name: "spec.md",
  primary: true,
  project: "CORE",
  ref_key: "CORE-1/spec",
  session_id: null,
  slug: "spec",
  versions: [],
};

// The architect's decision of 2026-11-20 13:35Z, item 2: a write made after a correction's event
// from an answer read before it. A margin action on a comment (`useMarginItems`) writes its value
// over the issue's comments as it starts, and puts back the comments it read then where Dispatch
// refuses it. Here the comment's author deletes it while an accept of its suggestion is out: the
// delete's event is heard as the feed hears it (`hearLiveEvent`, then `markCorrectedPost`),
// Dispatch refuses the accept, which has no replacement text left to move (`409 POST_DELETED`,
// `commentAction` in `api/comment_actions.go`), and the rollback puts back the comments as they
// stood before the delete. No read is out at the delete, so it is kept for the accept alone, which
// is still pending then. The read of the comments after the rollback fails, and the margin's
// comments read as deleted.
test("a margin action refused after its comment's delete puts back no word of the comment", async () => {
  const client = new DispatchQueryClient({
    defaultOptions: {
      mutations: { retry: false },
      queries: { retry: false, staleTime: Number.POSITIVE_INFINITY },
    },
  });
  const comments = ["comments", "CORE-1"];
  const suggestion = { accepted: null, replace_with: "Hold the deploy until Monday." };
  client.setQueryData(["issue", "CORE-1"], { artifacts: [SPEC], primary_artifact_id: SPEC.id });
  client.setQueryData(["asks", "CORE-1"], []);
  client.setQueryData(comments, [commentOf(POST, { suggestion })]);
  client.setQueryData(["inbox"], []);
  client.setQueryData(["user-state"], {});
  const save = Promise.withResolvers<never>();
  const acceptComment = spyOn(api, "acceptComment").mockReturnValue(save.promise);
  const listComments = spyOn(api, "listComments").mockRejectedValue(new Error("the read failed"));
  const places = new Map();
  const hook = renderHook(
    () =>
      useMarginItems({ key: "CORE-1", kind: "issue" }, "comments", SPEC, places, places, undefined),
    {
      wrapper: ({ children }: { children: ReactNode }) =>
        createElement(MemoryRouter, null, createElement(QueryClientProvider, { client }, children)),
    }
  );
  const shown = () => hook.result.current.commentRecords[0];
  try {
    act(() => hook.result.current.mutateItem({ id: POST, kind: "accept" }));
    await waitFor(() => expect(acceptComment).toHaveBeenCalled());
    await waitFor(() => expect(shown()?.suggestion?.accepted).toBe(true));

    const deleted = correction("comment", POST, false);
    act(() => {
      hearLiveEvent(client, deleted, "alice");
      markCorrectedPost(client, deleted);
    });
    expect({ body: shown()?.body, suggestion: shown()?.suggestion }).toEqual({
      body: "",
      suggestion: null,
    });
    await act(async () => {
      save.reject(new ApiError(409, { code: "POST_DELETED", error: "this comment is deleted" }));
    });
    await waitFor(() => expect(client.isMutating()).toBe(0));
    await waitFor(() => expect(listComments).toHaveBeenCalled());
    await waitFor(() => expect(client.isFetching()).toBe(0));

    expect({
      body: shown()?.body,
      deleted_at: shown()?.deleted_at,
      suggestion: shown()?.suggestion,
    }).toEqual({ body: "", deleted_at: DELETION.deleted_at, suggestion: null });
  } finally {
    hook.unmount();
    acceptComment.mockRestore();
    listComments.mockRestore();
    client.clear();
  }
});

// A kept edit is written into the answer the read it was kept for lands, and an answer composed
// after a later edit, whose event has not arrived yet, carries that edit's time: the kept edit's
// older time never moves it back. The list holds no copy of the post at the edit, so the edit
// writes nothing there and is kept for the read out. Its delete's facts would apply whatever the
// answer said.
test("an answer that lands carrying a later edit than a kept one keeps its own edit's time", async () => {
  const list = ["issue-messages", "CORE-1"];
  const later = "2026-11-20T00:45:00.5Z";
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  client.setQueryData<MessageRead[]>(list, []);
  const held = Promise.withResolvers<MessageRead[]>();
  const out = client.fetchQuery({ queryFn: () => held.promise, queryKey: list });
  markCorrectedPost(client, correction("message", POST, true));
  expect(client.getQueryData<MessageRead[]>(list)).toEqual([]);
  held.resolve([{ message: { ...messageOf(), edited_at: later }, replies: [] }]);
  await out;
  expect(client.getQueryData<MessageRead[]>(list)?.[0]?.message.edited_at).toBe(later);
  client.clear();
});

// A delete empties the anchor quote of the post's own cached events where it emptied the
// comment's (`markedEvent`), as the server's read of the log does (`attachPostMarks`): an insert
// suggestion's quote is the text it put in the document, the comment's own words, which the delete
// clears (`clearDeletedPostBody`), and the delete's own event carries the anchor with that quote
// empty. A replace suggestion's quote is the document's own text, which its delete leaves.
test("a delete empties a cached event's anchor quote where its own event's is empty, and keeps a replace suggestion's", () => {
  const anchor = {
    artifact_id: "doc-1",
    block_id: null,
    mark_id: "mark-1",
    orphaned: false,
    version: 1,
  };
  const pinned = ["events", "CORE-1", "margin-pinned", [POST]];
  const quoteAfter = (cached: string, deleted: string) => {
    const client = new QueryClient();
    client.setQueryData(pinned, [
      {
        ...commentEvent,
        payload: { ...commentEvent.payload, anchor: { ...anchor, quote: cached } },
      },
    ]);
    markCorrectedPost(
      client,
      correction("comment", POST, false, { anchor: { ...anchor, quote: deleted } })
    );
    const payload = at(client.getQueryData(pinned), 0, "payload");
    return { deleted_at: at(payload, "deleted_at"), quote: at(payload, "anchor", "quote") };
  };
  expect({
    insert: quoteAfter("Friday at noon", ""),
    replace: quoteAfter("the old wording", "the old wording"),
  }).toEqual({
    insert: { deleted_at: DELETION.deleted_at, quote: "" },
    replace: { deleted_at: DELETION.deleted_at, quote: "the old wording" },
  });
});

// A delete empties the anchor quote of a cached Inbox thread row whose root is the post, where it
// emptied the comment's. The row carries its root comment's anchor (`readMyReplyRows` in
// `api/replies.go`), and the card prints that anchor's quote above the thread (`ThreadQuote`).
// Where the root is a pending insert suggestion, the quote is the text the insert put in the
// document, the comment's own words: its delete empties the row's quote, as it does the comment's
// own copies and its events', so no word of the suggestion stays in the row. The control: a
// delete that leaves the quote, a replace suggestion's or a plain comment's, which is the
// document's own text, leaves the row's.
test("a delete empties a cached Inbox thread row's anchor quote where its own event's is empty, and keeps a quote of the document's text", () => {
  const rows = ["me", "replies", "inbox"];
  const rowAfter = (cached: string, deleted: string) => {
    const client = new QueryClient();
    client.setQueryData(rows, {
      limit: 20,
      offset: 0,
      rows: [replyRow(POST, { anchor: { ...INSERT_ANCHOR, quote: cached }, subject: OLD_WORDS })],
      total: 1,
    });
    markCorrectedPost(
      client,
      correction("comment", POST, false, { anchor: { ...INSERT_ANCHOR, quote: deleted } })
    );
    return at(client.getQueryData(rows), "rows", 0);
  };
  const insert = rowAfter(OLD_WORDS, "");
  const kept = rowAfter("the old wording", "the old wording");
  expect({
    insert: { deleted_at: at(insert, "deleted_at"), quote: at(insert, "anchor", "quote") },
    kept: { deleted_at: at(kept, "deleted_at"), quote: at(kept, "anchor", "quote") },
  }).toEqual({
    insert: { deleted_at: DELETION.deleted_at, quote: "" },
    kept: { deleted_at: DELETION.deleted_at, quote: "the old wording" },
  });
  expect(JSON.stringify(insert)).not.toContain(OLD_WORDS);
});
