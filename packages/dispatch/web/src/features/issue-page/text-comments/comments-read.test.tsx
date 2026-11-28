import { afterEach, expect, spyOn, test } from "bun:test";
import type { CommentNodeAnchor } from "@legion/contracts";
import { useQuery } from "@tanstack/react-query";
import { act, render, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http } from "msw";
import { MemoryRouter } from "react-router-dom";

import { displayTree } from "../../../__tests__/display-trees";
import { answerRequests } from "../../../__tests__/network";
import { api } from "../../../api/client";
import { eventRefreshTargets } from "../../../api/event-refresh";
import { refreshQueries } from "../../../api/query-refresh";
import type { Comment, DispatchEvent } from "../../../api/types";
import { commentListKey } from "../../conversation/composer-model";
import { useMarginItems } from "../../margin/useMarginItems";
import { warmMarkdownRenderer } from "../../refs/markdown-engine";
import { EVENT_FRESH } from "../../refs/reference-target";
import { KEY } from "../decisions-filter/decisions-fixtures";
import { CONTROL_LABELS } from "../page-words";
import { AppProviders, treeOf } from "../slots/stage-fixtures";
import { commentNode, treeNode } from "../tree-read/tree-fixtures";
import { issueCommentsQuery } from "../useIssueTree";
import {
  answerSeededReads,
  DECISION,
  DECISION_WORDS,
  drawPage,
  newClient,
  ON_DECISION,
  ON_STATEMENT,
  REPLY_ON_STATEMENT,
  rowOf,
  SERVES,
  STATEMENT,
  seededNodes,
} from "./margin-fixtures";

/** R9: the new outline and the shell margin read one issue list. Read the HTTP request rather
 * than counting calls to a client method, so the test observes the opt-in on the wire. */
const OWNER = { issueKey: KEY, kind: "issue" } as const;
const MARGIN_OWNER = { key: KEY, kind: "issue" } as const;
const PLACEMENTS = new Map();
let stubs: { mockRestore(): void }[] = [];
afterEach(() => {
  for (const stub of stubs) stub.mockRestore();
  stubs = [];
});

function MarginReader() {
  const { commentRecords } = useMarginItems(
    MARGIN_OWNER,
    "comments",
    undefined,
    PLACEMENTS,
    PLACEMENTS,
    undefined
  );
  return (
    <output aria-label="Shell comments">{commentRecords.map(({ body }) => body).join(" ")}</output>
  );
}

function OutlineReader() {
  const { data } = useQuery({ ...issueCommentsQuery(KEY), ...EVENT_FRESH });
  return <output aria-label="Outline comments">{data?.map(({ body }) => body).join(" ")}</output>;
}

type CommentRow = Comment & { node_anchor?: CommentNodeAnchor };

function serveComments(comments: () => readonly CommentRow[]) {
  const queries: string[] = [];
  answerRequests(
    http.get("/api/v1/inbox", () => Response.json([])),
    http.get(`/api/v1/issues/${KEY}/comments`, ({ request }) => {
      queries.push(new URL(request.url).search);
      return Response.json(comments());
    })
  );
  return queries;
}

const ON_LENS: CommentRow = {
  ...ON_STATEMENT.comment,
  anchor: null,
  body: "Keep the second verdict.",
  id: "on-lens",
  node_anchor: {
    node: { id: "lens-1", kind: "lens", state: "live" },
    occurrence: 1,
    orphaned: false,
    quote: "verdict",
  },
};

function answerLensTree() {
  stubs.push(
    spyOn(api, "getIssueTree").mockResolvedValue(
      treeOf(
        [
          ...seededNodes([]),
          treeNode("lens-1", "lens", STATEMENT, {
            attributes: { verdict: "The verdict keeps its verdict." },
            text: "The verdict keeps its verdict.",
          }),
          {
            ...commentNode(ON_LENS, "lens-1"),
            anchor: {
              node: { id: "lens-1", kind: "lens", state: "live" },
              occurrence: 1,
              orphaned: false,
              quote: "verdict",
            },
          },
        ],
        SERVES
      )
    )
  );
}

for (const first of ["outline", "shell"] as const) {
  test(`the new page's two readers send one comments read with node_anchor (${first} first)`, async () => {
    stubs = answerSeededReads({ threads: [], wireComments: true });
    const queries = serveComments(() => [ON_LENS]);
    const client = newClient();
    const readers =
      first === "outline" ? (
        <>
          <OutlineReader />
          <MarginReader />
        </>
      ) : (
        <>
          <MarginReader />
          <OutlineReader />
        </>
      );
    const view = drawPage(client, "conversation", { beside: readers, drawer: true });
    try {
      for (const name of ["Shell comments", "Outline comments"]) {
        await waitFor(() => expect(view.getByLabelText(name)).toHaveTextContent(ON_LENS.body));
      }
      expect(queries, "both readers share one opted-in HTTP answer").toEqual(["?node_anchor=true"]);
    } finally {
      view.finish();
      client.clear();
    }
  });
}

test("today's page sends its comments read with no parameter", async () => {
  stubs = answerSeededReads({ threads: [], wireComments: true });
  const queries = serveComments(() => [ON_STATEMENT.comment]);
  const client = newClient();
  const view = render(
    <MemoryRouter initialEntries={[`/issues/${KEY}`]}>
      <AppProviders client={client}>
        <MarginReader />
      </AppProviders>
    </MemoryRouter>
  );
  try {
    await waitFor(() =>
      expect(view.getByLabelText("Shell comments")).toHaveTextContent(ON_STATEMENT.comment.body)
    );
    expect(queries, "today's shell issue margin read changed its URL").toEqual([""]);
  } finally {
    view.unmount();
    client.clear();
  }
});

test("an answer fetched without node_anchor changes no thread or row of the new page", async () => {
  stubs = answerSeededReads({ threads: [], wireComments: true });
  answerLensTree();
  const { node_anchor: _anchor, ...plain } = ON_LENS;
  const queries = serveComments(() => [ON_LENS]);
  const client = newClient();
  client.setQueryData(["comments", KEY], [plain]);
  const view = drawPage(client, "conversation");
  try {
    const card = () => view.container.querySelector(`[data-text-thread="${plain.id}"]`);
    await waitFor(() => {
      expect(rowOf(view, "lens-1"), "the lens row").not.toBeNull();
      expect(card()).toHaveAttribute("data-text-thread-node", "lens-1");
      expect(card()).toHaveTextContent(plain.body);
      expect(rowOf(view, plain.id), "the comment is not an outline row").toBeNull();
    });
    expect(queries, "the old cache answer caused a second read").toEqual([]);
    await act(() => client.invalidateQueries({ exact: true, queryKey: ["comments", KEY] }));
    expect(queries).toEqual(["?node_anchor=true"]);
    expect(card()).toHaveAttribute("data-text-thread-node", "lens-1");
    expect(rowOf(view, plain.id), "the re-read made the comment an outline row").toBeNull();
  } finally {
    view.finish();
    client.clear();
  }
});

test("an event's invalidation re-reads the new page's comments read", async () => {
  stubs = answerSeededReads({ threads: [ON_STATEMENT], wireComments: true });
  let comments = [ON_STATEMENT.comment];
  const queries = serveComments(() => comments);
  const client = newClient();
  const view = drawPage(client, "conversation");
  try {
    await waitFor(() =>
      expect(queries, "the initial comments read").toEqual(["?node_anchor=true"])
    );
    comments = [...comments, REPLY_ON_STATEMENT];
    const event = {
      actor: REPLY_ON_STATEMENT.author,
      artifact_id: null,
      created_at: REPLY_ON_STATEMENT.created_at,
      id: 1,
      issue_key: KEY,
      notify: true,
      payload: { ...REPLY_ON_STATEMENT, artifact_name: "", kind: "comment" },
      project: "CORE",
      seq: 1,
      type: "comment.created",
    } as unknown as DispatchEvent;
    act(() => refreshQueries(client, eventRefreshTargets(event)));
    await view.findByText(REPLY_ON_STATEMENT.body);
    expect(queries, "the event refresh reads the same opted-in list").toEqual([
      "?node_anchor=true",
      "?node_anchor=true",
    ]);
  } finally {
    view.finish();
    client.clear();
  }
});

test("DecisionActions' exact invalidation re-reads it", async () => {
  await warmMarkdownRenderer();
  stubs = answerSeededReads({ threads: [], wireComments: true });
  let comments: CommentRow[] = [];
  const queries = serveComments(() => comments);
  const body = "Keep this decision beside its source.";
  const sent = { ...ON_DECISION.comment, body, artifact_id: null };
  stubs.push(
    spyOn(api, "createComment").mockImplementation(async () => {
      comments = [sent];
      return sent;
    })
  );
  const markdown = `:::decision{#${DECISION}}\n${DECISION_WORDS}\n:::\n`;
  stubs.push(
    spyOn(api, "getArtifactText").mockResolvedValue({
      markdown,
      markdown_tree: displayTree(markdown),
      token: "",
      version: 1,
    })
  );
  const client = newClient();
  const view = drawPage(client, "decisions", { beside: <OutlineReader /> });
  const user = userEvent.setup();
  try {
    const change = await view.findByRole("button", { name: CONTROL_LABELS.change_decision });
    await waitFor(() => expect(queries).toEqual(["?node_anchor=true"]));
    await user.click(change);
    await user.type(view.getByLabelText(CONTROL_LABELS.your_change), body);
    await user.click(view.getByRole("button", { name: CONTROL_LABELS.send_change }));
    await waitFor(() => expect(view.getByLabelText("Outline comments")).toHaveTextContent(body));
    expect(queries, "the Change re-reads the opted-in list exactly once").toEqual([
      "?node_anchor=true",
      "?node_anchor=true",
    ]);
  } finally {
    view.finish();
    client.clear();
  }
});

test("an optimistic post lands in the new page's list", async () => {
  stubs = answerSeededReads({ threads: [ON_STATEMENT], wireComments: true });
  const queries = serveComments(() => [ON_STATEMENT.comment]);
  const client = newClient();
  const view = drawPage(client, "conversation");
  try {
    await waitFor(() =>
      expect(queries, "the initial comments read").toEqual(["?node_anchor=true"])
    );
    act(() =>
      client.setQueryData<Comment[]>(commentListKey(OWNER), (current) => [
        ...(current ?? []),
        REPLY_ON_STATEMENT,
      ])
    );
    const reply = await view.findByText(REPLY_ON_STATEMENT.body);
    expect(
      reply.closest(`[data-text-thread="${ON_STATEMENT.comment.id}"]`),
      "the reply's thread"
    ).not.toBeNull();
    expect(queries, "the cache update caused a second read").toEqual(["?node_anchor=true"]);
  } finally {
    view.finish();
    client.clear();
  }
});
