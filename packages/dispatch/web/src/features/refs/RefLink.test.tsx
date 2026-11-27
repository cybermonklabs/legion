import { afterEach, beforeEach, expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, waitFor } from "@testing-library/react";
import type { Window as HappyDOMWindow } from "happy-dom";
import { createPortal } from "react-dom";

import { api } from "../../api/client";
import { setDispatchOrigins } from "../../api/origins";
import type { IssueReferenceSummary } from "../../api/types";
import { collectReferenceAnchors, RefLink } from "./RefLink";

/** The test DOM is happy-dom's window (`__tests__/register-dom.ts` registers it), which
 *  TypeScript knows only as the DOM's; each test puts the page on the outside origin of a
 *  Dispatch that answers at two. */
const testWindow = window as unknown as HappyDOMWindow;

const CANON = "https://canon.test";
const OUTSIDE = "https://outside.test";

let pageBefore = "about:blank";

beforeEach(() => {
  pageBefore = window.location.href;
  testWindow.happyDOM.setURL(`${OUTSIDE}/issues/CORE-2`);
  setDispatchOrigins([CANON, OUTSIDE]);
});

afterEach(() => {
  testWindow.happyDOM.setURL(pageBefore);
});

/** CORE-1's summary read, which titles its link. */
const coreSummary: IssueReferenceSummary = {
  documents: [],
  key: "CORE-1",
  project: "CORE",
  status: "in_progress",
  title: "Ship the outside origin",
};

/** A rendered body holding `html`, run through the link pass with a `RefLink` portalled into
 *  each reference anchor it found, as `useRenderedMarkdown` draws a body. */
function linkBody(html: string) {
  const root = document.createElement("div");
  root.innerHTML = html;
  document.body.append(root);
  const anchors = collectReferenceAnchors(root);
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const view = render(
    <QueryClientProvider client={queryClient}>
      {anchors.map(({ anchor, key, reads, route, text }) =>
        createPortal(
          <RefLink anchor={anchor} reads={reads} route={route} text={text} />,
          anchor,
          key
        )
      )}
    </QueryClientProvider>
  );
  return {
    root,
    unmount: () => {
      view.unmount();
      root.remove();
    },
  };
}

/** The origin and the rest of where an anchor opens, read as the browser follows its href. */
function opens(anchor: Element | null): { origin: string; rest: string } {
  const url = new URL(anchor?.getAttribute("href") ?? "", window.location.href);
  return { origin: url.origin, rest: `${url.pathname}${url.search}${url.hash}` };
}

test("a reference link copied on the canonical origin opens on the origin the page is on", async () => {
  const getIssueSummary = spyOn(api, "getIssueSummary").mockResolvedValue(coreSummary);
  const body = linkBody(`<p><a href="${CANON}/issues/CORE-1">${CANON}/issues/CORE-1</a></p>`);

  try {
    const anchor = body.root.querySelector("a");
    await waitFor(() => expect(anchor?.textContent).toBe("CORE-1 Ship the outside origin"));
    expect(opens(anchor)).toEqual({ origin: window.location.origin, rest: "/issues/CORE-1" });
  } finally {
    body.unmount();
    getIssueSummary.mockRestore();
  }
});

test("a link to a canonical-origin page that names no reference opens on the origin the page is on", () => {
  const body = linkBody(
    `<p><a href="${CANON}/projects/CORE?view=board#top">board</a> <a href="https://evil.test/projects/CORE">elsewhere</a></p>`
  );

  try {
    const [board, elsewhere] = body.root.querySelectorAll("a");
    expect(opens(board ?? null)).toEqual({
      origin: window.location.origin,
      rest: "/projects/CORE?view=board#top",
    });
    // A link to an origin this Dispatch does not answer at is the link its author wrote.
    expect(elsewhere?.getAttribute("href")).toBe("https://evil.test/projects/CORE");
  } finally {
    body.unmount();
  }
});
