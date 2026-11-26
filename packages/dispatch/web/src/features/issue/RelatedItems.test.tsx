import { beforeAll, expect, jest, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { displayTree } from "../../__tests__/display-trees";
import { realClock, tick } from "../../__tests__/fake-clock";
import { api } from "../../api/client";
import type { AskRead, DisplayTree, Suggestions, WriteSuggestion } from "../../api/types";
import { warmMarkdownRenderer } from "../refs/markdown-engine";
import { RelatedItems } from "./RelatedItems";
import { RELATED_DEBOUNCE_MS } from "./related-timing";

// The rows draw through the shared renderer, whose Markdown engine loads lazily, and they run in
// random order, so the file loads the engine before its first row, whichever row that is.
beforeAll(async () => {
  await warmMarkdownRenderer();
});

const answer = { at: "2026-10-01T00:00:00Z", selected: [], text: null, user: "alice" };

const issueItem: WriteSuggestion = {
  href: "/issues/CORE-12",
  id: "CORE-12",
  kind: "issue",
  owner: { key: "CORE-12", kind: "issue", status: "in_progress", title: "Broadcast send order" },
  snippet: "the order broadcasts send in",
};

const documentItem: WriteSuggestion = {
  href: "/projects/CORE/documents/send-order",
  id: "doc-1",
  kind: "document",
  owner: {
    artifact_id: "art-1",
    kind: "document",
    name: "Send order",
    project: "CORE",
    slug: "send-order",
  },
  snippet: "how the send order was settled",
};

const askItem: WriteSuggestion = {
  ask: { answer, state: "answered" },
  href: "/issues/CORE-30/asks/ask-1",
  id: "ask-1",
  kind: "ask",
  owner: { key: "CORE-30", kind: "issue", status: "done", title: "Which order do we send in?" },
  snippet: "which order do we send in",
};

function renderRelated(
  check: () => Promise<Suggestions>,
  project = "CORE",
  title = "Broadcast send order"
) {
  const relatedItems = spyOn(api, "relatedItems").mockImplementation(() => check());
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const view = render(
    <MemoryRouter>
      <QueryClientProvider client={queryClient}>
        <RelatedItems project={project} title={title} />
      </QueryClientProvider>
    </MemoryRouter>
  );
  return {
    relatedItems,
    unmount: () => {
      view.unmount();
      queryClient.clear();
      relatedItems.mockRestore();
    },
  };
}

test("lists each related item as a link named by its owner, with where an ask stands", async () => {
  const view = renderRelated(async () => ({
    outcome: "found",
    related: [issueItem, documentItem, askItem],
    search: "meaning_and_keywords",
  }));
  try {
    await screen.findByText("Related items (searched by meaning and keywords):");
    const links = screen.getAllByRole("link");
    expect(links).toHaveLength(3);

    const issueLink = screen.getByRole("link", { name: "CORE-12 Broadcast send order" });
    expect(issueLink.getAttribute("href")).toBe("/issues/CORE-12");
    expect(issueLink.getAttribute("target")).toBe("_blank");

    const documentLink = screen.getByRole("link", { name: "Send order" });
    expect(documentLink.getAttribute("href")).toBe("/projects/CORE/documents/send-order");
    expect(documentLink.getAttribute("target")).toBe("_blank");

    const askLink = screen.getByRole("link", { name: "CORE-30 Which order do we send in?" });
    expect(askLink.getAttribute("href")).toBe("/issues/CORE-30/asks/ask-1");
    expect(askLink.getAttribute("target")).toBe("_blank");
    expect(screen.getByText("Answered")).toBeTruthy();
  } finally {
    view.unmount();
  }
});

// A related ask's status line draws its answer's chosen labels and text as records of that ask,
// so a `#id` in them is read against the question map of the document the ask belongs to, which
// the server answers for the ask itself (`refs.ScopeOf`). Mounted with no source, the line asks
// for no map and the `#q1` stays the characters written.
test("a #id in a related ask's chosen label resolves against the ask's own question map", async () => {
  await warmMarkdownRenderer();
  const getReferenceQuestions = spyOn(api, "getReferenceQuestions").mockResolvedValue({
    document: "spec-30",
    questions: [{ ask: "ask-9", block_id: "q1", reference: "dispatch://CORE-30/ask/ask-9" }],
  });
  // The ask the `#q1` names, which its link reads its title from.
  const getAsk = spyOn(api, "getAsk").mockResolvedValue({
    answers: [],
    ask: { id: "ask-9", issue_key: "CORE-30", options: [], question: "Which order first?" },
    edits: [],
    followers: [],
    replies: [],
  } as unknown as AskRead);
  const label = "Settled in #q1";
  const view = renderRelated(async () => ({
    outcome: "found",
    related: [
      {
        ...askItem,
        ask: {
          answer: {
            ...answer,
            selected: [label],
            selected_trees: [displayTree(label) as DisplayTree],
          },
          state: "answered",
        },
      },
    ],
    search: "keywords_only",
  }));
  try {
    await screen.findByText("Related items (searched by keywords only):");
    await waitFor(() => expect(getReferenceQuestions).toHaveBeenCalledWith("ask", "ask-1"));
    await waitFor(() =>
      expect(
        document.querySelector('[data-dispatch-ref="dispatch://CORE-30/ask/ask-9"]')
      ).not.toBeNull()
    );
  } finally {
    view.unmount();
    getReferenceQuestions.mockRestore();
    getAsk.mockRestore();
  }
});

// A check that never ran must not read as one that found nothing, so the two sentences differ
// and the one for a check that never ran names the words to search by hand.
test("a search that found nothing and a check that was never made read differently", async () => {
  const searched = renderRelated(async () => ({
    outcome: "none",
    related: [],
    search: "keywords_only",
  }));
  let foundNothing: string | null;
  try {
    foundNothing = (
      await screen.findByText("Related-item check searched (keywords only) and found nothing.")
    ).textContent;
  } finally {
    searched.unmount();
  }

  const notMade = renderRelated(async () => ({
    missing: "the search timed out",
    outcome: "not_made",
    related: [],
    search_query: "broadcast send order",
  }));
  try {
    const sentence = await screen.findByText(/No related-item check was made/);
    expect(sentence.textContent).toBe(
      "No related-item check was made: the search timed out. Search Dispatch for “broadcast send order” before you rely on this being new."
    );
    expect(sentence.textContent).not.toBe(foundNothing);
  } finally {
    notMade.unmount();
  }
});

test("a failed check says so and offers Retry", async () => {
  const view = renderRelated(async () => {
    throw new Error("network down");
  });
  try {
    await screen.findByText("Could not check for related items.");
    expect(screen.getByRole("button", { name: "Retry" })).toBeTruthy();
  } finally {
    view.unmount();
  }
});

test("a title below the floor and a dialog with no project ask nothing", async () => {
  jest.useFakeTimers();
  try {
    const short = renderRelated(async () => ({ outcome: "none", related: [] }), "CORE", "a");
    try {
      // Past the debounce: the component waits that long before it asks at all.
      await tick(RELATED_DEBOUNCE_MS + 200);
      expect(short.relatedItems).toHaveBeenCalledTimes(0);
    } finally {
      short.unmount();
    }

    const noProject = renderRelated(async () => ({ outcome: "none", related: [] }), "");
    try {
      // Past the debounce: the component waits that long before it asks at all.
      await tick(RELATED_DEBOUNCE_MS + 200);
      expect(noProject.relatedItems).toHaveBeenCalledTimes(0);
    } finally {
      noProject.unmount();
    }
  } finally {
    realClock();
  }
});
