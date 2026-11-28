import { afterEach, beforeAll, beforeEach, expect, type Mock, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { waitFor } from "@testing-library/react";

import renderedAnswers from "../../../../../contracts/fixtures/rendered-answers.json";
import { renderWithQueries as render } from "../../__tests__/query-provider";
import { ApiError, api } from "../../api/client";
import { MarkdownBody } from "./MarkdownBody";
import { warmMarkdownRenderer } from "./markdown-engine";
import { flushTasks } from "./parse-worker-fixture";

// packages/contracts/fixtures/rendered-answers.json holds answers of the command bar's agent with
// the issues and project documents they link and the text the bar draws of each. The eval's judge
// reads an answer through chips.RenderAnswer (packages/envoy/going/chips/render.go), and its Go
// test holds RenderAnswer to the exact same fixture: the first link to each issue (a time link
// whose words are the issue's key included) is its chip, a later link to the same issue keeps the
// words it was written with (`takeIssueLinks`), a link to another item is its own words where they
// are not its address and the item's name where they are (`RefLink`), and a time link shows its
// words with its instant on hover (`dispatch-time.tsx`). So a change of either side fails a test
// until the fixture and the other side follow it.

beforeAll(async () => {
  await warmMarkdownRenderer();
});

const issues = new Map(renderedAnswers.issues.map((issue) => [issue.key, issue]));
const documents = new Map(
  renderedAnswers.documents.map((doc) => [`${doc.owner}/${doc.slug}`, doc])
);

const MONTHS = [
  "Jan",
  "Feb",
  "Mar",
  "Apr",
  "May",
  "Jun",
  "Jul",
  "Aug",
  "Sep",
  "Oct",
  "Nov",
  "Dec",
] as const;

let getIssueSummary: Mock<typeof api.getIssueSummary>;
let getProjectDocumentSummary: Mock<typeof api.getProjectDocumentSummary>;

beforeEach(() => {
  getIssueSummary = spyOn(api, "getIssueSummary").mockImplementation(async (key) => {
    const issue = issues.get(key);
    if (issue === undefined) {
      throw new ApiError(404, { code: "NOT_FOUND", error: `the fixture holds no issue ${key}` });
    }
    return { documents: [], project: key.slice(0, key.lastIndexOf("-")), ...issue };
  });
  getProjectDocumentSummary = spyOn(api, "getProjectDocumentSummary").mockImplementation(
    async (project, slug) => {
      const doc = documents.get(`${project}/${slug}`);
      if (doc === undefined) {
        throw new ApiError(404, {
          code: "ARTIFACT_NOT_FOUND",
          error: `the fixture holds no document ${project}/${slug}`,
        });
      }
      return {
        id: `artifact-${project}-${slug}`,
        kind: "doc",
        latest_version: 1,
        name: doc.name,
        project: doc.owner,
        slug: doc.slug,
      };
    }
  );
});

afterEach(() => {
  getIssueSummary.mockRestore();
  getProjectDocumentSummary.mockRestore();
});

/** Formats `at` in UTC the way `chips.RenderAnswer` writes a time link's hover date:
 *  `"18 Nov 2026, 11:35 UTC"`, or with seconds when `at` has non-zero seconds. */
function formatUtcHover(at: Date): string {
  const day = at.getUTCDate();
  const month = MONTHS[at.getUTCMonth()];
  const year = at.getUTCFullYear();
  const hh = String(at.getUTCHours()).padStart(2, "0");
  const mm = String(at.getUTCMinutes()).padStart(2, "0");
  const ss = at.getUTCSeconds();
  const clock = ss === 0 ? `${hh}:${mm}` : `${hh}:${mm}:${String(ss).padStart(2, "0")}`;
  return `${day} ${month} ${year}, ${clock} UTC`;
}

/** Reads the chip's drawn pieces (key, screen-reader status, title) joined with `" · "`. */
function readChip(chip: Element): string {
  const parts: string[] = [];
  for (const child of chip.children) {
    if (child.getAttribute("aria-hidden") === "true") {
      continue;
    }
    const text = child.textContent?.trim() ?? "";
    if (text !== "") {
      parts.push(text);
    }
  }
  return parts.join(" · ");
}

/** Reads the hover date a person sees on `chip` inside a `<time>` element: `ItemChip` sets
 *  `title` to `"<title> · <hover>"` once the summary loads, or `"<hover>"` alone before it. */
function readChipHover(chip: Element): string | null {
  const full = chip.getAttribute("title");
  if (full === null) {
    return null;
  }
  const titleText = chip.querySelector("[data-testid='item-chip-title']")?.textContent ?? "";
  if (titleText === "") {
    return full;
  }
  if (full === titleText) {
    return null;
  }
  const prefix = `${titleText} · `;
  return full.startsWith(prefix) ? full.slice(prefix.length) : full;
}

/** Reads a `<time>` element's drawn words and actual hover value: verifies that the rendered
 *  `title` equals `new Date(datetime).toLocaleString()` (what the page shows on hover in the
 *  reader's locale) and emits the UTC string `chips.RenderAnswer` writes, or the raw `title`
 *  (or missing hover) so any mismatch in hover presence, instant, or format fails the test. */
function readTime(time: Element): string {
  const words = readChildren(time);
  const chip = time.querySelector("[data-testid='item-chip']");
  const hover = chip !== null ? readChipHover(chip) : time.getAttribute("title");
  if (hover === null || hover === "") {
    return words;
  }
  const datetime = time.getAttribute("datetime");
  if (datetime !== null) {
    const parsed = new Date(datetime);
    if (!Number.isNaN(parsed.getTime()) && hover === parsed.toLocaleString()) {
      return `${words} (${formatUtcHover(parsed)})`;
    }
  }
  return `${words} (${hover})`;
}

/** Reads every child node of `parent` in document order. */
function readChildren(parent: Node): string {
  let out = "";
  for (const child of parent.childNodes) {
    out += readNode(child);
  }
  return out;
}

/** Reads what a person and a screen reader meet in `node`: plain text as drawn, each `ItemChip`
 *  as its key, status and title joined by `" · "`, and each `<time>` with its actual hover. */
function readNode(node: Node): string {
  if (node.nodeType === Node.TEXT_NODE) {
    return node.textContent ?? "";
  }
  if (!(node instanceof Element)) {
    return "";
  }
  if (node.getAttribute("data-testid") === "item-chip") {
    return readChip(node);
  }
  if (node.tagName === "TIME") {
    return readTime(node);
  }
  return readChildren(node);
}

for (const answer of renderedAnswers.answers) {
  test(`the bar draws ${answer.name} as packages/contracts' rendered-answers fixture says`, async () => {
    // `chips.RenderAnswer` marks chips and address-worded item names with square brackets for the
    // plain-text judge; the DOM renders those with chip boxes and links rather than bracket glyphs.
    const expected = answer.rendered.replaceAll("[", "").replaceAll("]", "");
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
      await waitFor(() =>
        expect(readNode(view.container).replace(/\s+/gu, " ").trim()).toBe(expected)
      );
      // Each read the answer's links made has answered, and the body drew what they gave.
      await waitFor(() => expect(client.isFetching()).toBe(0));
      await flushTasks();
      expect(readNode(view.container).replace(/\s+/gu, " ").trim()).toBe(expected);
    } finally {
      view.unmount();
    }
  });
}
