import { beforeEach, expect, type Mock, spyOn, test } from "bun:test";
import { type QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, render, waitFor, within } from "@testing-library/react";
import { http } from "msw";
import { type Node as ProseMirrorNode, Schema } from "prosemirror-model";
import type { ReactNode } from "react";
import { MemoryRouter } from "react-router-dom";

import { commentDeliveryFields } from "../../__tests__/comment-fixture";
import { type FakeDocumentRuntime, fakeDocumentRuntime } from "../../__tests__/document-runtime";
import { graphNode } from "../../__tests__/graph-fixture";
import { answerRequests } from "../../__tests__/network";
import {
  answerTextReadsWithTheSeededText,
  artifact,
  documentSurfaceHandlers,
} from "../../__tests__/proof-document";
import { api } from "../../api/client";
import { myRepliesOwnerQuery, whoAmIQuery } from "../../api/queries";
import type { Ask, AskRead, Comment, GraphReferences, IssueReferences } from "../../api/types";
import { ArtifactDocument } from "../artifacts/ArtifactDocument";
import { DocumentRuntime } from "../doc/runtime";
import { issueDecisionsQuery } from "../doc/spec-marks";
import { MarginProvider } from "../margin/margin-context";
import { buildIssuePath } from "../refs/routes";
import { NoTooltips } from "./NoTooltips";
import { OpenedDocument } from "./OpenedDocument";
import { appClient } from "./window-focus-fixture";

/**
 * The editor of a document the new page opens in place (part N's R14): its reads of who is
 * reading, of the issue's decisions and of an open question's thread read set are kept fresh by
 * events (`EVENT_FRESH`), as the outline's are, so an editor opened more than 30 s after the load
 * asks none of them again. Without that option, as today's pages draw the same editor
 * (`ArtifactDocument`), each keeps the app's 30 s `staleTime` (`main.tsx`) and is asked again.
 */

answerTextReadsWithTheSeededText();

/** The question the spec's decision block holds, open and asked of the reader. */
const BLOCK_ASK: Ask = {
  anchor: null,
  answer: null,
  asked_of: null,
  asked_of_at: null,
  asked_of_by: null,
  artifact_id: null,
  author: { id: "01a086ad", kind: "session", origin: { session_title: "Architect for CORE-1" } },
  block_artifact: { id: artifact.id, primary: true, slug: artifact.slug },
  block_id: "b-1",
  created_at: new Date(Date.now() - 3 * 60_000).toISOString(),
  edited_at: null,
  id: "ask-1",
  issue_key: "CORE-1",
  kind: "question",
  multiple: false,
  opened_event_id: 1,
  options: [{ label: "Ship" }, { label: "Hold" }],
  question: "Should we ship?",
  state: "open",
  urgency: "high",
};

/** The asker's reply on the question's thread, the newest write the card draws, which its
 *  thread's read mark watches. */
const REPLY: Comment = {
  anchor: null,
  ask_id: BLOCK_ASK.id,
  author: { id: "01a086ad", kind: "session" },
  body: "Ship on Friday?",
  created_at: new Date(Date.now() - 2 * 60_000).toISOString(),
  ...commentDeliveryFields(),
  edited_at: null,
  id: "reply-1",
  issue_key: "CORE-1",
  reply_to: null,
  resolved: false,
  resolved_at: null,
  resolved_by: null,
  suggestion: null,
  turn: "human",
};

/** The question's thread, with the asker's reply. */
const THREAD: AskRead = {
  answers: [],
  ask: BLOCK_ASK,
  claimant: null,
  edits: [],
  followers: [],
  replies: [REPLY],
  route: null,
  route_holder: null,
  route_status: null,
  route_status_reason: null,
};

/** The spec's one version, as the server answers it. */
const VERSION = {
  authors: [{ id: "alice", kind: "user" }],
  created_at: "2026-09-09T00:00:00Z",
  markdown: "The first version",
  named: false,
  number: 1,
  summary: null,
} as const;

/** A decision block's node, as the editor hands its node view one. */
const askSchema = new Schema({
  nodes: {
    ask: {
      attrs: {
        blockId: { default: null },
        multiple: { default: false },
        state: { default: "open" },
        urgency: { default: "med" },
      },
      content: "paragraph+",
      group: "block",
    },
    doc: { content: "block+" },
    paragraph: { content: "inline*", group: "block" },
    text: { group: "inline" },
  },
});

/** The spec's decision block of `BLOCK_ASK`. */
function askNode(): ProseMirrorNode {
  return askSchema.node("ask", { blockId: BLOCK_ASK.block_id, urgency: "high" }, [
    askSchema.node("paragraph", undefined, [askSchema.text(BLOCK_ASK.question)]),
  ]);
}

/** The document's reads besides its own: what references it and the issue, which nothing does. */
beforeEach(() => {
  answerRequests(
    http.get("/api/v1/issues/:key/references", () =>
      Response.json({
        linked_from: [],
        links: [],
        members: [],
        truncated: false,
      } satisfies IssueReferences)
    ),
    http.get("/api/v1/references", ({ request }) =>
      Response.json({
        edges: [],
        node: graphNode({
          id: new URL(request.url).searchParams.get("to") ?? "",
          kind: "artifact",
        }),
      } satisfies GraphReferences)
    ),
    ...documentSurfaceHandlers({ asks: () => [{ ...BLOCK_ASK, last_reply: null }] })
  );
});

/** The three reads the editor must not ask again, each counted: an answer that never comes, so
 *  no answer draws anything again after the count. */
interface EditorReads {
  readonly decisions: Mock<typeof api.getIssueDecisions>;
  readonly readSet: Mock<typeof api.listMyReplies>;
  readonly who: Mock<typeof api.whoAmI>;
}

/** Every read the editor makes past the app's staleness, counted or answered. */
function countEditorReads(): EditorReads & { restore(): void } {
  const never = () => Promise.withResolvers<never>().promise;
  const reads = {
    decisions: spyOn(api, "getIssueDecisions").mockImplementation(never),
    readSet: spyOn(api, "listMyReplies").mockImplementation(never),
    who: spyOn(api, "whoAmI").mockImplementation(never),
  };
  // The sessions the open question reaches, which its card lists.
  const subscribers = spyOn(api, "getIssueSubscribers").mockResolvedValue([]);
  return {
    ...reads,
    restore: () => {
      for (const read of Object.values(reads)) read.mockRestore();
      subscribers.mockRestore();
    },
  };
}

/**
 * The app's client (`appClient`: a read is stale 30 s after it answered), holding who is reading,
 * the issue's decisions and the reader's unread set on the issue as they were answered 31 s ago,
 * and the open question with its thread, which the card reads under keys of their own.
 */
function answeredPastStaleness(): QueryClient {
  const client = appClient();
  const answered = { updatedAt: Date.now() - 31_000 };
  client.setQueryData(
    whoAmIQuery().queryKey,
    { event_head: "0", kind: "user", login: "alice" },
    answered
  );
  client.setQueryData(
    issueDecisionsQuery("CORE-1", true).queryKey,
    { decisions: [], document: null, open_questions: [] },
    answered
  );
  client.setQueryData(
    myRepliesOwnerQuery({ issue: "CORE-1" }).queryKey,
    { as_of: "2026-11-05T02:00:00Z", limit: 200, offset: 0, rows: [], total: 0 },
    answered
  );
  client.setQueryData(["asks", "CORE-1"], [BLOCK_ASK]);
  client.setQueryData(["ask-thread", BLOCK_ASK.id], THREAD);
  client.setQueryData(["artifact", artifact.id, "version", VERSION.number], VERSION);
  return client;
}

/** How many times each counted read is asked as `editor` is drawn: the live document, with its
 *  decision block's card drawn once the room has synced, or a version of it. */
async function askedWhileOpening(
  reads: EditorReads,
  fake: FakeDocumentRuntime,
  editor: ReactNode,
  live: boolean
): Promise<{ decisions: number; readSet: number; who: number }> {
  const view = render(
    <MemoryRouter initialEntries={[buildIssuePath({ key: "CORE-1", kind: "spec" })]}>
      <QueryClientProvider client={answeredPastStaleness()}>
        <DocumentRuntime.Provider value={fake.runtime}>
          <MarginProvider>{editor}</MarginProvider>
        </DocumentRuntime.Provider>
      </QueryClientProvider>
    </MemoryRouter>
  );
  try {
    if (live) {
      await waitFor(() => expect(fake.connections).toHaveLength(1));
      await act(async () => fake.sync());
      await waitFor(() => expect(fake.editors).toHaveLength(1));
      // The editor builds the decision block's node view, which hosts the question's card.
      const nodeViews = fake.editors[0]?.viewProps.nodeViews as
        | Record<string, (node: ProseMirrorNode) => { dom: HTMLElement; destroy(): void }>
        | undefined;
      const construct = nodeViews?.ask;
      if (construct === undefined) throw new Error("the ask node view was not installed");
      act(() => {
        fake.editors[0]?.root.append(construct(askNode()).dom);
      });
      const section = fake.editors[0]?.root.querySelector<HTMLElement>(
        "section[data-dispatch-ask-block]"
      );
      if (section === null || section === undefined) throw new Error("no decision block drawn");
      await within(section).findByText(REPLY.body);
    } else {
      await waitFor(() =>
        expect(view.container.querySelector('[data-testid="version-view"]')).not.toBeNull()
      );
    }
    // A read the mount asks for is sent from its observer's effect: a flush of effects and
    // microtasks once the editor is drawn sends any not yet sent.
    await act(async () => {});
    return {
      decisions: reads.decisions.mock.calls.length,
      readSet: reads.readSet.mock.calls.length,
      who: reads.who.mock.calls.length,
    };
  } finally {
    view.unmount();
    reads.decisions.mockClear();
    reads.readSet.mockClear();
    reads.who.mockClear();
  }
}

/** The new page's opened spec, at its latest version or at `version`. */
function opened(version?: number): ReactNode {
  return (
    <NoTooltips>
      <OpenedDocument
        artifact={artifact}
        artifactRoute={{ key: "CORE-1", kind: "artifact", slug: artifact.slug, version }}
        highlightTerm={undefined}
        isClosed={false}
        issueKey="CORE-1"
        item={undefined}
        onShowDiff={() => {}}
        onToolbarChange={() => {}}
        onVersionChange={() => {}}
        outline={buildIssuePath({ key: "CORE-1", kind: "issue" })}
        showDiff={false}
      />
    </NoTooltips>
  );
}

/** The same spec as today's pages draw it, with no freshness given. */
function todays(version?: number): ReactNode {
  return (
    <ArtifactDocument
      artifact={artifact}
      isClosed={false}
      onVersionChange={() => {}}
      owner={{ key: "CORE-1", kind: "issue" }}
      showDiff={false}
      version={version}
    />
  );
}

test("an editor opened after 30 seconds asks neither whoami nor the decisions again", async () => {
  const reads = countEditorReads();
  try {
    expect(
      await askedWhileOpening(reads, fakeDocumentRuntime({ text: "Spec" }), opened(), true),
      "the live spec asked again"
    ).toEqual({ decisions: 0, readSet: 0, who: 0 });
    expect(
      await askedWhileOpening(reads, fakeDocumentRuntime(), opened(VERSION.number), false),
      "a version of the spec asked again"
    ).toEqual({ decisions: 0, readSet: 0, who: 0 });
  } finally {
    reads.restore();
  }
});

test("an editor drawn with no freshness asks who is reading, the decisions and the thread's read set again, as today's pages do", async () => {
  const reads = countEditorReads();
  try {
    expect(
      await askedWhileOpening(reads, fakeDocumentRuntime({ text: "Spec" }), todays(), true),
      "the live spec"
    ).toEqual({ decisions: 1, readSet: 1, who: 1 });
    expect(
      (await askedWhileOpening(reads, fakeDocumentRuntime(), todays(VERSION.number), false)).who,
      "a version of the spec"
    ).toBe(1);
  } finally {
    reads.restore();
  }
});
