import type { ReactNode } from "react";
import { useNavigate } from "react-router-dom";

import { commentDeliveryFields } from "../../__tests__/comment-fixture";
import type { Artifact, Ask, Comment, IssueDetails } from "../../api/types";
import { buildIssuePath } from "../refs/routes";
import { KEEPS_DETAILS_ATTRIBUTE } from "./Margin";
import { type DocumentBridge, useMargin } from "./margin-context";

export const specArtifact: Artifact = {
  created_at: "2026-09-09T00:00:00Z",
  created_by: { id: "alice", kind: "user" },
  id: "artifact-1",
  issue_key: "CORE-1",
  project: "CORE",
  kind: "doc",
  name: "spec.md",
  primary: true,
  ref_key: "CORE-1/spec",
  session_id: null,
  slug: "spec",
  versions: [],
};

export const issue: IssueDetails = {
  route_status: null,
  route_holder: null,
  route_status_reason: null,
  holder_state: null,
  last_seen: null,
  takeable_at: null,
  advances: [],
  advances_revision: 0,
  artifacts: [specArtifact],
  children: [],
  closed_at: null,
  created_at: "2026-09-09T00:00:00Z",
  created_by: { id: "alice", kind: "user" },
  design_gates: [],
  external_links: [],
  key: "CORE-1",
  labels: [],
  last_seq: 1,
  number: 1,
  parent: null,
  blocked_by: [],
  assignee: null,
  claim: null,
  components: { mode: "inherit", ids: [], unknown: [], reason: null, inherited_from: null },
  primary_artifact_id: "artifact-1",
  project: "CORE",
  referenced_by_count: 0,
  plan_artifact_id: "",
  verification_artifact_id: "",
  route: null,
  repo: null,
  resolved_repo: null,
  status: "open",
  priority: null,
  rank: "U",
  title: "Review the spec",
  open_asks: [],
  progress: { tasks: null, children: null },
  updated_at: "2026-09-09T00:00:00Z",
};

export const comment: Comment = {
  anchor: {
    artifact_id: "artifact-1",
    block_id: null,
    mark_id: "m-1",
    orphaned: false,
    quote: "brown",
    version: 1,
  },
  author: { id: "alice", kind: "user" },
  body: "why?",
  created_at: "2026-09-09T00:00:00Z",
  id: "comment-1",
  ask_id: null,
  turn: null,
  issue_key: "CORE-1",
  reply_to: null,
  resolved: false,
  resolved_by: null,
  resolved_at: null,
  edited_at: null,
  suggestion: null,
  ...commentDeliveryFields(),
};

export const anchoredAsk: Ask = {
  anchor: {
    artifact_id: "artifact-1",
    block_id: null,
    mark_id: "m-2",
    orphaned: false,
    quote: "Review",
    version: 1,
  },
  answer: null,
  author: { id: "session-1", kind: "session" },
  artifact_id: null,
  asked_of: null,
  asked_of_at: null,
  asked_of_by: null,
  block_id: null,
  created_at: "2026-09-09T00:00:00Z",
  edited_at: null,
  id: "ask-1",
  issue_key: "CORE-1",
  kind: "question",
  multiple: false,
  opened_event_id: 1,
  options: [{ label: "Ship" }],
  question: "Should this ship?",
  state: "open",
  urgency: "med",
};

/** The address a margin landing starts from beside its document on the new issue page: the
 *  address of the document the comment is anchored in, with the comment named by `?mark=`
 *  (`issue-page/issue-address.ts`'s `documentItem`). Every other address of the issue that names
 *  the comment - its own route (`/issues/KEY/comments/<id>`), `?comment=` - opens the new page's
 *  outline on the comment's node, and the margin in Details only marks its card there
 *  (`useMarginSheet.ts`'s `routeItemBesideDocument`, R20). */
export function markedCommentPath(id: string): string {
  return `${buildIssuePath({ key: "CORE-1", kind: "artifact", slug: "spec" })}?mark=${id}`;
}

export function CommentLink(): ReactNode {
  const navigate = useNavigate();

  return (
    <button
      onClick={() => navigate(buildIssuePath({ id: "comment-1", key: "CORE-1", kind: "comment" }))}
      type="button"
    >
      Open comment
    </button>
  );
}

/** A link to the very route the landing tests start on, so following it is a same-URL push. */
export function SameCommentLink(): ReactNode {
  const navigate = useNavigate();

  return (
    <button
      onClick={() =>
        navigate(`${buildIssuePath({ key: "CORE-1", kind: "spec" })}?comment=comment-1`)
      }
      type="button"
    >
      Open the same comment
    </button>
  );
}

/** Two links that keep the phone's Details open when followed in it (`Margin.tsx`'s
 *  `KEEPS_DETAILS_ATTRIBUTE`): one that carries the attribute, as a history turn's time link
 *  does, and one inside an element that carries it. Neither leaves the page. */
export function KeptDetailsLinks(): ReactNode {
  const keeps = { [KEEPS_DETAILS_ATTRIBUTE]: "" };
  return (
    <>
      <a href="/issues/CORE-3" onClick={(event) => event.preventDefault()} {...keeps}>
        a turn's time
      </a>
      <span {...keeps}>
        <a href="/issues/CORE-4" onClick={(event) => event.preventDefault()}>
          a link inside a turn
        </a>
      </span>
    </>
  );
}

/** A document bridge whose editor actions do nothing unless a test supplies them. */
export function stubDocumentBridge(
  artifactId: string,
  overrides: Partial<DocumentBridge> = {}
): DocumentBridge {
  return {
    artifactId,
    focusBlock: () => {},
    focusMark: () => {},
    removeMark: () => {},
    remarkMark: () => ({ refused: "gone" }),
    retypeMark: () => ({ refused: "missing" }),
    setActiveBlocks: () => {},
    setActiveMarks: () => {},
    whenSent: async () => {},
    ...overrides,
  };
}

/** Stands in for a document registering with the margin and reporting its layout: by default the
 *  spec the landing tests' margin shows. A document with no live mark and no typed ask block
 *  reports empty maps, which is still an answer. */
export function ReportEmptyLayoutButton({
  artifactId = specArtifact.id,
}: {
  artifactId?: string;
}): ReactNode {
  const { registerDocument, setBlockPlacements, setMarkPlacements } = useMargin();

  return (
    <button
      onClick={() => {
        registerDocument(stubDocumentBridge(artifactId));
        setMarkPlacements(new Map());
        setBlockPlacements(new Map());
      }}
      type="button"
    >
      Report layout
    </button>
  );
}

export function SelectedItemLabel(): ReactNode {
  const { selectedItemId } = useMargin();

  return <output aria-label="Selected margin item">{selectedItemId ?? "none"}</output>;
}

/** A phone's width in CSS pixels. It sits below `PHONE_VIEWPORT_QUERY`'s 767 px, so the phone and
 *  compact queries hold there and neither `MD_VIEWPORT_QUERY` nor `READING_PANE_QUERY` does. */
export const PHONE_WIDTH = 390;

/** A wide desktop's width in CSS pixels. It sits past `READING_PANE_QUERY`'s 80rem (1280 px), so
 *  the reading pane and `MD_VIEWPORT_QUERY` hold there and neither the phone nor the compact query
 *  does. */
export const DESKTOP_WIDTH = 1440;

/** The CSS pixels in a `rem` at the browser's default font size, which the stylesheet's `rem`
 *  breakpoints assume. */
const PX_PER_REM = 16;

/** One `(min-width: …)` or `(max-width: …)` clause, in `px` or `rem`. */
const WIDTH_CLAUSE = /^\(\s*(min|max)-width\s*:\s*(\d*\.?\d+)(px|rem)\s*\)$/;

/** Whether a viewport `widthPx` wide matches `query`, which holds when every clause it joins with
 *  `and` holds. A clause of any other kind throws, naming the query, because a guessed answer
 *  would let a row pass on a layout that no real screen draws. */
function viewportMatches(widthPx: number, query: string): boolean {
  return query.split(/\s+and\s+/i).every((clause) => {
    const match = WIDTH_CLAUSE.exec(clause.trim());
    if (match === null) throw new Error(`stubViewport cannot read the media query "${query}"`);
    const [, bound, value, unit] = match;
    const px = Number(value) * (unit === "rem" ? PX_PER_REM : 1);
    return bound === "min" ? widthPx >= px : widthPx <= px;
  });
}

/** Answers each media query as a real viewport `widthPx` wide does, until the restore it returns
 *  runs. The width never changes meanwhile, so no `change` is ever sent and the listeners do
 *  nothing. */
export function stubViewport(widthPx: number): () => void {
  const original = window.matchMedia;
  window.matchMedia = ((query: string) =>
    ({
      addEventListener: () => {},
      addListener: () => {},
      dispatchEvent: () => true,
      matches: viewportMatches(widthPx, query),
      media: query,
      onchange: null,
      removeEventListener: () => {},
      removeListener: () => {},
    }) as MediaQueryList) as typeof window.matchMedia;
  return () => {
    window.matchMedia = original;
  };
}
