import {
  GOING_ERROR_ANSWER_DID_NOT_ARRIVE,
  GOING_ERROR_NO_MODEL_CONFIGURED,
  GOING_EVENT_CHANGED,
  GOING_EVENT_CUT,
  GOING_EVENT_ERROR,
  GOING_EVENT_ITEM,
  GOING_EVENT_NONE,
  GOING_EVENT_TEXT,
  type GraphFrame,
} from "@legion/contracts";
import { queryOptions, type UseQueryResult, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState } from "react";

import { ApiError, api } from "../../../api/client";
import { queryRetry } from "../../../api/query-client";
import type { GraphEdge, GraphReferences } from "../../../api/types";
import { safeExternalHref } from "../../../lib/external-href";
import { drawsEdge } from "../../refs/ReferencedBy";
import { referencesQuery } from "../../refs/reference-target";
import { parseNodeReference, referenceHref } from "../../refs/routes";
import { storeAddressReference } from "../../refs/store-address";
import { lineWordsRaw } from "../issue-tree";
import {
  EMPTY_STATE_LINES,
  NODE_REFERENCES_WORDS,
  NODE_TYPE_NAMES,
  STATE_LINES,
  sentenceCase,
} from "../page-words";

/**
 * A command bar run's own page, read without a browser: its address, its one read, and what the
 * page draws of the answer. A run is an `interaction` node, which only the person who asked it
 * can read; the page draws the asker's whole question, each frame of the answer in its own order
 * and words, and the nodes the run changed.
 */

/** Whether `id` can name a run at all: the lowercase canonical uuid the last segment of its
 *  address holds, as the store writes one. Anything else names no run, so nothing is asked for
 *  it. */
function isRunId(id: string | undefined): id is string {
  return id !== undefined && storeAddressReference(runAddress(id)) !== undefined;
}

/** The `dispatch://` address of the run `id`. */
function runAddress(id: string): string {
  return `dispatch://interaction/${id}`;
}

/** The edge kind from a node a run changed to that run. */
const CHANGED_BY_RUN: GraphEdge["kind"] = "changed_by_run";

/** An edge as the run's read answers it, with its excerpt's reading (`GraphEdgeWithTrees`). */
type ReadEdge = NonNullable<GraphReferences["edges"]>[number];

/** Whether a read failed because there is no run to read: the address names none (400), or none
 *  the reader may open (404), which the page cannot tell apart and neither can a retry. */
function isMissingRun(error: unknown): boolean {
  return error instanceof ApiError && (error.status === 400 || error.status === 404);
}

/**
 * The run `id` with the nodes it changed (`GET /api/v1/references?to=<address>&kind=changed_by_run`).
 * Its key is the backlink panel's `["references", "to", <address>]` (`referencesQuery`) and the
 * kind after it, which keeps it apart from the unfiltered read of the same address. No event's
 * refresh names a run (`api/event-refresh.ts`), so what sends this read again is a refresh by the
 * prefix `["references", "to"]` - `referencePanels`, where its `when` accepts this read's answer,
 * `EVERY_REFERENCE_READ`, and the whole-cache refresh after a gap in the event feed - window focus
 * (`main.tsx`), and the page mounted again. An event's refresh of one panel's exact key
 * (`referencePanel`) does not reach it, since this key is one longer. A run that is missing is
 * asked for once; any other failure is asked again as every read is (`queryRetry`).
 */
const runQuery = (id: string) =>
  queryOptions({
    queryKey: [...referencesQuery(runAddress(id)).queryKey, CHANGED_BY_RUN] as const,
    queryFn: () => api.getReferences(runAddress(id), { kind: CHANGED_BY_RUN }),
    retry: (failures: number, error: Error) => !isMissingRun(error) && queryRetry(failures, error),
  });

/** `useRun`'s answer: the read, and whether there is no run the reader may open at this id. */
export interface RunRead {
  /** True where `id` is no run id, which sends no request, and where the read last ended 400 or
   *  404, while it runs again too, until it ends with an answer or a failure of another kind. */
  readonly notFound: boolean;
  readonly query: UseQueryResult<GraphReferences>;
}

/** How the read of the run `id` last ended, as `useRun` keeps it: with no run to read or not. */
interface RunEnded {
  readonly id: string | undefined;
  readonly missing: boolean;
}

/**
 * The run at the page's `id`, read only where `id` can name one (`isRunId`).
 *
 * Whether the run is missing is kept from how the read last ended, not read off its failure
 * alone: TanStack clears the failure of a read that holds no answer as the read runs again - on
 * window focus, at a broad refresh of the backlink panels (`runQuery`), or as the page is mounted
 * again over the read the cache holds - and the failure alone would then draw the run's own page,
 * its line saying the read failed, until the read answered 400 or 404 again. So a read that ended
 * 400 or 404 stays missing until it ends with an answer or a failure of another kind. The hook
 * keeps that per id, since the page stays drawn as its address moves to another run. For an id it
 * has kept nothing of, it reads the cache's state. Where no fetch of the read is running as the
 * hook first renders for that id, that state still holds the read's last failure: TanStack sends
 * the read again only after that render, though the result it hands the render already reads as
 * pending. Where one is running, the state holds no failure (`error: null`): `runQuery` reads no
 * abort signal, so a fetch running as the page unmounts goes on, and a page mounted again over it
 * draws the loading line until its 400 or 404 lands, then the not-found page.
 */
export function useRun(id: string | undefined): RunRead {
  const named = isRunId(id);
  const options = runQuery(id ?? "");
  const client = useQueryClient();
  const query = useQuery({ ...options, enabled: named });
  const [ended, setEnded] = useState<RunEnded | null>(null);
  const missing =
    query.error !== null
      ? isMissingRun(query.error)
      : query.data === undefined &&
        (ended !== null && ended.id === id
          ? ended.missing
          : isMissingRun(client.getQueryState(options.queryKey)?.error));
  // Kept as the hook renders, as React has state follow what a render reads: React renders again
  // at once with it, before it commits this render.
  if (ended === null || ended.id !== id || ended.missing !== missing) setEnded({ id, missing });
  return { notFound: !named || missing, query };
}

/** One piece of the answer, in the order its frames came: the model's text, one thing it cites,
 *  or a line the page says of the answer, in the page's own words. An item's `href` is the page
 *  it opens, none where the frame's href is no page the page links (`linkedHref`). */
export type RunPart =
  | { readonly kind: "answer"; readonly markdown: string }
  | { readonly kind: "item"; readonly href?: string; readonly title: string }
  | { readonly kind: "line"; readonly words: string };

/** One node the run changed. */
export interface RunChange {
  /** The node's address, which makes it one entry however many edges name it. */
  readonly key: string;
  /** The page's word for the node's kind, sentence-cased; `""` where the page has none, since a
   *  kind's schema token is no word a reader reads. */
  readonly kind: string;
  /** The words the node's one line is cut from (`lineWordsRaw`): the first block of the server's
   *  reading of the references read's excerpt (`text_tree`), and of the excerpt as stored where
   *  the read sent none, each bare address as written; `""` where the excerpt holds none. The
   *  page cuts its line from them and names each reference in it (`RunPage.tsx`'s
   *  `namedFirstLine`). */
  readonly words: string;
  /** The page that opens the node; none where its address names no page. */
  readonly href?: string;
}

/** What the run's page draws. */
export interface RunView {
  /** The asker's question, whole and as typed. */
  readonly question: string;
  /** When the run was asked; null where the read says nothing. */
  readonly at: string | null;
  readonly parts: readonly RunPart[];
  readonly changed: readonly RunChange[];
}

/** A frame's count, where a count that is no number reads as 0. */
function countOf(value: unknown): number {
  return typeof value === "number" && Number.isFinite(value) ? value : 0;
}

/** A frame's string field, where any other value reads as `""`. */
function stringOf(value: unknown): string {
  return typeof value === "string" ? value : "";
}

/** The origin a path an item names is read against (`linkedHref`): a reserved name (RFC 6761),
 *  since only whether the path keeps the origin it is read against matters, not which. */
const PATH_ORIGIN = "https://dispatch.invalid";

/**
 * The page an item's `href` opens, or none where the page links nothing: a path on Dispatch's own
 * origin as written, or an absolute `http:` or `https:` URL in the form it parses into. A path
 * starts with `/` and the browser reads it as a path on the page's origin, so it is no `//host`,
 * and no `/\host` or `/` and `/` split by a tab or a line break, which the browser reads as
 * `//host`. Every other href is read by `safeExternalHref`, the app's one answer to which URL is
 * safe to follow, so this page trusts no URL the others refuse: `javascript:` or another scheme,
 * which the page's link (`Link`) would hand to the browser as written, and a path relative to the
 * run's own address, which names no page the answer meant, are linked nowhere.
 */
export function linkedHref(href: string): string | undefined {
  if (!href.startsWith("/")) return safeExternalHref(href) ?? undefined;
  // `new URL` throws for a path that does not parse, such as `//`, which is then no link.
  try {
    return new URL(href, PATH_ORIGIN).origin === PATH_ORIGIN ? href : undefined;
  } catch {
    return undefined;
  }
}

/** The line an `error` frame draws for its code: `GOING_ERROR_ANSWER_FAILED`, a code the page
 *  does not know, and no code at all are each the answer having failed. */
function errorLine(code: unknown): string {
  switch (code) {
    case GOING_ERROR_ANSWER_DID_NOT_ARRIVE:
      return STATE_LINES.run_did_not_arrive;
    case GOING_ERROR_NO_MODEL_CONFIGURED:
      return STATE_LINES.run_no_model;
    default:
      return STATE_LINES.run_failed;
  }
}

/** The parts `frames` draw, in `sequence` order: a run of `text` frames is one answer, and an
 *  `end` frame, or a frame of a name the page does not know, draws nothing. A run's texts are
 *  joined with `+`: the reading list's source test reads a text in a template literal as drawn
 *  there (`surface-lists.test.ts`), and the join draws nothing, its answer being the renderer's
 *  to draw. */
function partsOf(frames: readonly GraphFrame[]): RunPart[] {
  const parts: RunPart[] = [];
  for (const frame of [...frames].sort((a, b) => a.sequence - b.sequence)) {
    const data = frame.data ?? {};
    switch (frame.name) {
      case GOING_EVENT_TEXT: {
        const last = parts.at(-1);
        const text = stringOf(data.text);
        if (last?.kind === "answer") {
          parts[parts.length - 1] = { kind: "answer", markdown: last.markdown + text };
        } else {
          parts.push({ kind: "answer", markdown: text });
        }
        break;
      }
      case GOING_EVENT_ITEM: {
        parts.push({
          href: linkedHref(stringOf(data.href)),
          kind: "item",
          title: stringOf(data.title),
        });
        break;
      }
      case GOING_EVENT_CUT:
        parts.push({
          kind: "line",
          words: `${STATE_LINES.run_cut.before}${countOf(data.words)}${STATE_LINES.run_cut.after}`,
        });
        break;
      case GOING_EVENT_CHANGED:
        parts.push({
          kind: "line",
          words: `${STATE_LINES.run_changed.before}${countOf(data.dropped)}${STATE_LINES.run_changed.after}`,
        });
        break;
      case GOING_EVENT_NONE:
        parts.push({ kind: "line", words: EMPTY_STATE_LINES.run_none });
        break;
      case GOING_EVENT_ERROR:
        parts.push({ kind: "line", words: errorLine(data.code) });
        break;
    }
  }
  return parts;
}

/** The nodes `edges` name, once each, in the read's order: each by its kind's word, the words its
 *  one line is cut from (`lineWordsRaw`), and the page its address opens. An edge of a kind, or
 *  to a node of a kind, that this page does not know is left out, as every list of a node's
 *  references leaves it (`drawsEdge`). `RunPage.tsx`'s `ChangedNodeList` decides which of these
 *  nodes have a line or a kind word to draw. */
function changedOf(edges: readonly ReadEdge[]): RunChange[] {
  const kinds: Readonly<Partial<Record<string, string>>> = NODE_REFERENCES_WORDS.kinds;
  const types: Readonly<Partial<Record<string, string>>> = NODE_TYPE_NAMES;
  const changed = new Map<string, RunChange>();
  for (const edge of edges) {
    if (!drawsEdge(edge)) continue;
    const { id, kind, ref } = edge.node;
    const key = ref ?? `${kind}:${id}`;
    if (changed.has(key)) continue;
    const word = kinds[kind] ?? types[kind];
    const excerpt = edge.excerpt;
    const words =
      excerpt === undefined ? "" : lineWordsRaw({ text: excerpt.text, tree: excerpt.text_tree });
    const route = ref === undefined ? undefined : parseNodeReference(ref);
    changed.set(key, {
      href: route === undefined ? undefined : referenceHref(route),
      key,
      kind: word === undefined ? "" : sentenceCase(word),
      words,
    });
  }
  return [...changed.values()];
}

/** What the run's page draws of `answer`, the run's read (`runQuery`). */
export function runView(answer: GraphReferences): RunView {
  return {
    at: answer.node.made_at,
    changed: changedOf(answer.edges ?? []),
    parts: partsOf(answer.node.frames ?? []),
    question: answer.node.text ?? "",
  };
}
