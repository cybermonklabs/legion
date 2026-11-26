import { afterEach, beforeAll, expect, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { countEngineParses, displayTree } from "../../__tests__/display-trees";
import { answerQuestionOf, NAMED_QUESTION } from "../../__tests__/question-answers";
import type { IDCandidate } from "../../api/types";
import { warmMarkdownRenderer } from "../refs/markdown-engine";
import { ItemMatchList } from "./ItemMatchList";

beforeAll(async () => {
  await warmMarkdownRenderer();
});

afterEach(cleanup);

// A link's words hold no markup, so each match's first line is read as its plain-text
// projection: the bold goes and a `dispatch://` address reads as its target's name - the reading
// list's `item-match-first-line`. CORE-2's summary is in the cache before the list draws, so the
// name is there at once. Drawn as the characters it is, the link would read
// `**Warm** dispatch://CORE-2 first.`.
test("a match's first line reads its plain-text projection", async () => {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  client.setQueryData(["issue-summary", "CORE-2"], {
    documents: [],
    key: "CORE-2",
    project: "CORE",
    status: "in_progress",
    title: "Cut the release",
  });
  const candidates: IDCandidate[] = [
    {
      first_line: "**Warm** dispatch://CORE-2 first.",
      id: "c0ffee00-0000-4000-8000-000000000001",
      issue_key: "CORE-1",
      kind: "comment",
      ref: "dispatch://CORE-1/comment/c0ffee00-0000-4000-8000-000000000001",
    },
  ];
  render(
    <MemoryRouter>
      <QueryClientProvider client={client}>
        <ItemMatchList
          candidates={candidates}
          route={{ id: "c0ffee00", key: "CORE-1", kind: "comment" }}
        />
      </QueryClientProvider>
    </MemoryRouter>
  );
  expect(
    await screen.findByRole("link", { name: "Warm CORE-2 Cut the release first." })
  ).toHaveAttribute("href", "/issues/CORE-1/comments/c0ffee00-0000-4000-8000-000000000001");
});

// A match's first line is the head of the match's own text, so it reads a `#id` against that
// record, and it is drawn from the reading the server sends with it: the link shows the tree's
// words, never a parse of the line. Handed no record, `#q2` would stay as written; drawn from the
// line's markdown, the link would read `now` where the tree reads `later`.
test("a match's first line is drawn from its tree and reads a #id against its own record", async () => {
  const answers = answerQuestionOf("comment", "c0ffee00-0000-4000-8000-000000000002");
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const candidates: IDCandidate[] = [
    {
      first_line: "Decide #q2 now.",
      first_line_tree: displayTree("Decide #q2 later."),
      id: "c0ffee00-0000-4000-8000-000000000002",
      issue_key: "CORE-1",
      kind: "comment",
      ref: "dispatch://CORE-1/comment/c0ffee00-0000-4000-8000-000000000002",
    },
  ];
  render(
    <MemoryRouter>
      <QueryClientProvider client={client}>
        <ItemMatchList
          candidates={candidates}
          route={{ id: "c0ffee00", key: "CORE-1", kind: "comment" }}
        />
      </QueryClientProvider>
    </MemoryRouter>
  );
  try {
    expect(
      await screen.findByRole("link", { name: `Decide ${NAMED_QUESTION} later.` })
    ).toBeDefined();
  } finally {
    answers.restore();
  }
});

// A document's first line is its file's name, which the server sends no tree of: the link draws
// the name as the characters it is and parses nothing, so `*draft*.md` keeps its asterisks. Read
// as markdown, the link would read `draft.md`, and the page would have parsed a stored name.
test("a document match's first line is its file's name, drawn as its characters", async () => {
  const parses = countEngineParses();
  const candidates: IDCandidate[] = [
    {
      first_line: "*draft*.md",
      id: "c0ffee00-0000-4000-8000-000000000003",
      issue_key: "CORE-1",
      kind: "artifact",
      ref: "dispatch://CORE-1/artifact/draft-md",
    },
  ];
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <MemoryRouter>
      <QueryClientProvider client={client}>
        <ItemMatchList
          candidates={candidates}
          route={{ id: "c0ffee00", key: "CORE-1", kind: "comment" }}
        />
      </QueryClientProvider>
    </MemoryRouter>
  );
  try {
    expect(await screen.findByRole("link", { name: "*draft*.md" })).toBeDefined();
    expect(parses.count()).toBe(0);
  } finally {
    parses.restore();
  }
});
