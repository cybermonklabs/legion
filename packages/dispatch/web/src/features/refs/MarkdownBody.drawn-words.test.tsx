import { afterEach, beforeAll, beforeEach, expect, type Mock, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { waitFor } from "@testing-library/react";

import drawnWords from "../../../../../contracts/fixtures/drawn-words.json";
import { renderWithQueries as render } from "../../__tests__/query-provider";
import { api } from "../../api/client";
import { MarkdownBody } from "./MarkdownBody";
import { warmMarkdownRenderer } from "./markdown-engine";
import { flushTasks } from "./parse-worker-fixture";

// packages/contracts/fixtures/drawn-words.json holds answers of the command bar's agent with the
// issues they link and the words the bar draws of each. The server counts an answer's words so to
// hold it to 120 (going/chips' DrawnWords), and its Go test holds it to the same answers: the
// first link to each issue is its chip, a later link to the same issue keeps the words it was
// written with (`takeIssueLinks`), and a link to another item is its own words where they are not
// its address (`RefLink`). So a change of either side fails a test until the fixture and the other
// side follow it.

beforeAll(async () => {
  await warmMarkdownRenderer();
});

const issues = new Map(drawnWords.issues.map((issue) => [issue.key, issue]));

let getIssueSummary: Mock<typeof api.getIssueSummary>;

beforeEach(() => {
  getIssueSummary = spyOn(api, "getIssueSummary").mockImplementation(async (key) => {
    const issue = issues.get(key);
    if (issue === undefined) {
      throw new Error(`the fixture holds no issue ${key}`);
    }
    return { documents: [], project: key.slice(0, key.lastIndexOf("-")), ...issue };
  });
});

afterEach(() => {
  getIssueSummary.mockRestore();
});

/** How many words the bar draws in `element`, as a person and a screen reader meet them: every
 *  text node, a chip's key, its status and its title each as their own words, split on white
 *  space, each piece that holds a letter or a digit one word. */
function wordCount(element: HTMLElement): number {
  const texts: string[] = [];
  const walker = document.createTreeWalker(element, NodeFilter.SHOW_TEXT);
  for (let node = walker.nextNode(); node !== null; node = walker.nextNode()) {
    texts.push(node.textContent ?? "");
  }
  return texts
    .join(" ")
    .split(/\s+/)
    .filter((word) => /[\p{L}\p{N}]/u.test(word)).length;
}

for (const answer of drawnWords.answers) {
  test(`the bar draws ${answer.name} in the words packages/contracts' drawn-words fixture says`, async () => {
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    // As the bar's conversation draws an answer (`ConversationView`).
    const view = render(
      <MarkdownBody
        chips
        markdown={answer.markdown}
        referenceText=""
        softBreaks="line"
        surface="agent-turn-text"
      />,
      {
        wrapper: ({ children }) => (
          <QueryClientProvider client={client}>{children}</QueryClientProvider>
        ),
      }
    );
    try {
      await waitFor(() => expect(wordCount(view.container)).toBe(answer.words));
      // Each read the answer's links made has answered, and the body drew what they gave.
      await waitFor(() => expect(client.isFetching()).toBe(0));
      await flushTasks();
      expect(wordCount(view.container)).toBe(answer.words);
    } finally {
      view.unmount();
    }
  });
}
