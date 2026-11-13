import { afterAll, expect, jest, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, Route, Routes } from "react-router-dom";

import { fakeDocumentRuntime } from "../../__tests__/document-runtime";
import { realClock } from "../../__tests__/fake-clock";
import { graphNode } from "../../__tests__/graph-fixture";
import { keyedAttempt } from "../../__tests__/keyed-attempt";
import { stopTheLateWrite } from "../../__tests__/stop-late-write";
import { userOnFakeClock } from "../../__tests__/user";
import { ApiError, api, NetworkError } from "../../api/client";
import type { Artifact, Subscriber } from "../../api/types";
import { DocumentRuntime } from "../doc/runtime";
import { MarginProvider } from "../margin/margin-context";
import { SendStoppedError, stoppedWriteNote } from "../writes/keyed-write";
import { DocumentPage } from "./DocumentPage";

const getReferences = spyOn(api, "getReferences").mockResolvedValue({
  edges: [],
  node: graphNode({ id: "artifact-1", kind: "artifact" }),
});
// The document's own blocks and the asks anchored in it: every rendered page reads both, and
// none of these rows carries either.
const getArtifactBlocks = spyOn(api, "getArtifactBlocks").mockResolvedValue([]);
const listArtifactAsks = spyOn(api, "listArtifactAsks").mockResolvedValue([]);

afterAll(() => {
  getReferences.mockRestore();
  getArtifactBlocks.mockRestore();
  listArtifactAsks.mockRestore();
});

const artifact: Artifact = {
  created_at: "2026-09-10T00:00:00Z",
  created_by: { id: "alice", kind: "user" },
  id: "artifact-1",
  issue_key: null,
  kind: "doc",
  name: "Design notes",
  primary: false,
  project: "CORE",
  ref_key: "CORE/design-notes",
  session_id: null,
  slug: "design-notes",
  versions: [
    {
      authors: [{ id: "alice", kind: "user" }],
      created_at: "2026-09-10T00:00:00Z",
      named: false,
      number: 1,
      summary: null,
    },
  ],
};

function renderDocumentPage() {
  const runtime = fakeDocumentRuntime({ text: "# Design notes" });
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  queryClient.setQueryData(["whoami"], { kind: "user", login: "alice" });
  const view = render(
    <MemoryRouter initialEntries={["/projects/CORE/documents/design-notes"]}>
      <QueryClientProvider client={queryClient}>
        <DocumentRuntime.Provider value={runtime.runtime}>
          <MarginProvider>
            <Routes>
              <Route element={<DocumentPage />} path="/projects/:key/documents/:slug" />
            </Routes>
          </MarginProvider>
        </DocumentRuntime.Provider>
      </QueryClientProvider>
    </MemoryRouter>
  );
  return { queryClient, runtime, view };
}

test("renders the header, the document, versions, and Referenced by for an unlinked document", async () => {
  const getProjectArtifact = spyOn(api, "getProjectArtifact").mockResolvedValue(artifact);
  const getArtifact = spyOn(api, "getArtifact").mockResolvedValue(artifact);
  const getArtifactSubscribers = spyOn(api, "getArtifactSubscribers").mockResolvedValue([]);
  const getArtifactText = spyOn(api, "getArtifactText").mockResolvedValue({
    markdown: "# Design notes",
    version: 1,
    token: "sha256:design-notes",
  });
  getReferences.mockResolvedValueOnce({
    edges: [
      {
        created_at: "2026-09-10T00:00:00Z",
        direction: "in",
        excerpt: { text: "See dispatch://CORE/artifact/design-notes" },
        kind: "mentions",
        node: graphNode({
          id: "comment-1",
          issue_key: "CORE-1",
          kind: "comment",
          project: "CORE",
          ref: "dispatch://CORE-1/comment/comment-1",
        }),
        source_seq: 1,
      },
    ],
    node: graphNode({
      id: "artifact-1",
      kind: "artifact",
      project: "CORE",
      ref: "dispatch://CORE/artifact/design-notes",
    }),
  });
  const { runtime, view } = renderDocumentPage();

  try {
    await screen.findByTestId("artifact-header");
    await waitFor(() => expect(runtime.connections).toHaveLength(1));
    runtime.sync();
    await screen.findByRole("textbox", { name: "Document editor" });

    expect(screen.getByRole("link", { name: "Project CORE" })).toHaveAttribute(
      "href",
      "/projects/CORE/documents"
    );
    expect(screen.queryByRole("combobox", { name: "Artifact version" })).not.toBeInTheDocument();
    expect(screen.getByRole("combobox", { name: "Version" })).not.toBeNull();
    const referencedBy = screen.getByRole("region", { name: "Referenced by" });
    // Nothing discloses this panel, so it is its own heading and carries the count.
    expect(within(referencedBy).getByRole("heading", { name: "Referenced by (1)" })).toBeTruthy();
    expect(referencedBy).toHaveTextContent("Comment · CORE-1");
    expect(within(referencedBy).getByRole("link", { name: "Comment · CORE-1" })).toHaveAttribute(
      "href",
      "/issues/CORE-1/comments/comment-1"
    );
  } finally {
    view.unmount();
    getArtifactText.mockRestore();
    getArtifactSubscribers.mockRestore();
    getArtifact.mockRestore();
    getProjectArtifact.mockRestore();
  }
});

test("shows Document not found with a way back for an unknown slug", async () => {
  const getProjectArtifact = spyOn(api, "getProjectArtifact").mockRejectedValue(
    new ApiError(404, { code: "ARTIFACT_NOT_FOUND", error: "not found" })
  );
  const { view } = renderDocumentPage();

  try {
    await screen.findByRole("heading", { name: "Document not found" });
    expect(screen.getByRole("link", { name: "Back to CORE documents" })).toHaveAttribute(
      "href",
      "/projects/CORE/documents"
    );
  } finally {
    view.unmount();
    getProjectArtifact.mockRestore();
  }
});

test("shows Subscribed agents and unsubscribes after confirming the dialog", async () => {
  const user = userEvent.setup();
  const getProjectArtifact = spyOn(api, "getProjectArtifact").mockResolvedValue(artifact);
  const getArtifact = spyOn(api, "getArtifact").mockResolvedValue(artifact);
  const getArtifactText = spyOn(api, "getArtifactText").mockResolvedValue({
    markdown: "# Design notes",
    version: 1,
    token: "sha256:design-notes",
  });
  let currentSubscribers: Subscriber[] = [
    {
      last_seen: 1,
      live: true,
      removable: true,
      session_id: "0123456789abcdef",
      title: "Planner (live)",
      topics: [],
    },
  ];
  const getArtifactSubscribers = spyOn(api, "getArtifactSubscribers").mockImplementation(
    async () => currentSubscribers
  );
  const unsubscribeArtifactSession = spyOn(api, "unsubscribeArtifactSession").mockImplementation(
    async () => {
      currentSubscribers = [];
    }
  );
  // The subscriber's running state is the agent registry's, which lists it.
  const listAgents = spyOn(api, "listAgents").mockResolvedValue([
    {
      capabilities: [],
      dir: "/workspaces/legion",
      host: "example-host-devbox",
      last_activity: null,
      last_seen: 1,
      machine_id: "example-host-devbox",
      open_asks: 0,
      roles: [],
      session_id: "0123456789abcdef",
      state: "live",
      title: "Planner (live)",
    },
  ]);
  const { runtime, view } = renderDocumentPage();

  try {
    await screen.findByTestId("artifact-header");
    await waitFor(() => expect(runtime.connections).toHaveLength(1));
    runtime.sync();

    const subscribed = await screen.findByRole("region", { name: "Subscribed agents" });
    const planner = await within(subscribed).findByRole("link", { name: "Planner (live)" });
    expect(planner).toHaveAttribute("href", "/agents/0123456789abcdef/live");
    await waitFor(() =>
      expect(
        subscribed
          .querySelector('[data-agent-name="0123456789abcdef"]')
          ?.getAttribute("data-agent-state")
      ).toBe("running")
    );

    await user.click(within(subscribed).getByRole("button", { name: "Unsubscribe" }));
    const dialog = await screen.findByRole("dialog");
    expect(dialog).toHaveTextContent(
      "Unsubscribe Planner (live) from CORE/design-notes? They will be told."
    );
    await user.click(within(dialog).getByRole("button", { name: "Confirm" }));

    await waitFor(() =>
      expect(unsubscribeArtifactSession).toHaveBeenCalledWith(
        "artifact-1",
        "0123456789abcdef",
        keyedAttempt
      )
    );
    await waitFor(() =>
      expect(screen.queryByRole("region", { name: "Subscribed agents" })).not.toBeInTheDocument()
    );
  } finally {
    view.unmount();
    unsubscribeArtifactSession.mockRestore();
    listAgents.mockRestore();
    getArtifactSubscribers.mockRestore();
    getArtifactText.mockRestore();
    getArtifact.mockRestore();
    getProjectArtifact.mockRestore();
  }
});

/** The page with one removable subscriber, its unsubscribe answered by `unsubscribe`, which is
 *  handed the subscribers the list reads so it can empty them. Presses Unsubscribe and answers
 *  the dialog's Confirm button, for the case to press. */
async function unsubscribing(unsubscribe: (subscribers: Subscriber[]) => Promise<void>) {
  const subscribers: Subscriber[] = [
    {
      last_seen: 1,
      live: true,
      removable: true,
      session_id: "0123456789abcdef",
      title: "Planner (live)",
      topics: [],
    },
  ];
  const mocks = [
    spyOn(api, "getProjectArtifact").mockResolvedValue(artifact),
    spyOn(api, "getArtifact").mockResolvedValue(artifact),
    spyOn(api, "getArtifactText").mockResolvedValue({
      markdown: "# Design notes",
      version: 1,
      token: "sha256:design-notes",
    }),
    spyOn(api, "getArtifactSubscribers").mockImplementation(async () => [...subscribers]),
    spyOn(api, "unsubscribeArtifactSession").mockImplementation(() => unsubscribe(subscribers)),
    spyOn(api, "listAgents").mockResolvedValue([]),
  ];
  const { queryClient, runtime, view } = renderDocumentPage();
  await screen.findByTestId("artifact-header");
  await waitFor(() => expect(runtime.connections).toHaveLength(1));
  runtime.sync();
  const subscribed = await screen.findByRole("region", { name: "Subscribed agents" });
  await userEvent
    .setup()
    .click(await within(subscribed).findByRole("button", { name: "Unsubscribe" }));
  const confirm = within(await screen.findByRole("dialog")).getByRole("button", {
    name: "Confirm",
  });
  return {
    confirm,
    queryClient,
    restore: () => {
      view.unmount();
      queryClient.clear();
      for (const mock of mocks) mock.mockRestore();
    },
  };
}

// An unsubscribe the reader stopped may have been made: the page says so, and that its Retry
// sends the same `DELETE` again, which is the same however often it is made.
test("an unsubscribe the reader stopped says Retry sends it again, not that it failed", async () => {
  const page = await unsubscribing(() =>
    Promise.reject(new NetworkError("/api/v1/artifacts/artifact-1", new TypeError("Failed")))
  );
  try {
    jest.useFakeTimers();
    await userOnFakeClock().click(page.confirm);
    await stopTheLateWrite(page.queryClient);
    // The alert's paragraph, without its Retry button's name.
    expect(screen.getByRole("alert").querySelector("p")?.textContent).toBe(
      stoppedWriteNote("this agent's unsubscribe", "made", {
        again: "Retry",
        method: "DELETE",
        stopped: new SendStoppedError(),
      })
    );
  } finally {
    realClock();
    page.restore();
  }
});

// A `DELETE` sends no key, so one made again after an unsubscribe that landed finds the session
// unsubscribed already and is answered `404 SUBSCRIBER_NOT_FOUND`: the unsubscribe done, which
// the page takes as it takes a `204`, not as a refusal.
test("an unsubscribe answered SUBSCRIBER_NOT_FOUND is the unsubscribe done, not a refusal", async () => {
  const page = await unsubscribing(async (subscribers) => {
    subscribers.splice(0);
    throw new ApiError(404, { code: "SUBSCRIBER_NOT_FOUND", error: "session is not subscribed" });
  });
  try {
    await userEvent.setup().click(page.confirm);
    await waitFor(() =>
      expect(screen.queryByRole("region", { name: "Subscribed agents" })).not.toBeInTheDocument()
    );
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
  } finally {
    page.restore();
  }
});
