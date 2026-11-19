import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { type ReactNode, useCallback, useEffect, useState } from "react";
import { MemoryRouter } from "react-router-dom";

import { api } from "../../api/client";
import { buildIssuePath } from "../refs/routes";
import { Margin } from "./Margin";
import { MarginProvider, useMargin } from "./margin-context";
import { issue, specArtifact, stubDocumentBridge, stubMatchMedia } from "./margin-fixture";
import { MarginSlotProvider, useFillMarginSlot } from "./margin-slot";

/**
 * The margin keeps the open composer mounted under its rail, so an unsent comment survives a
 * collapse (`MarginSheet.tsx`). It survives it only while the wrapper above the composer is
 * one shape in both states: the slot used to hand the margin's content to the page while the
 * margin was shown and draw it bare once collapsed, which changed the element above the
 * composer, remounted it and dropped the draft with it (the body is `MentionComposer`'s own
 * state).
 */

const MARK_ANCHOR = {
  artifact: specArtifact.id,
  mark_id: "m-1",
  placement: { blockId: "b-1", prefix: "The quick brown fox ", readings: 1, suffix: "." },
  quote: "jumps over the lazy dog",
};

function RegisterDocument(): ReactNode {
  const { registerDocument } = useMargin();
  useEffect(() => {
    const bridge = stubDocumentBridge(specArtifact.id);
    registerDocument(bridge);
    return () => registerDocument(undefined);
  }, [registerDocument]);
  return null;
}

function ComposeOnMark(): ReactNode {
  const { composeForMark } = useMargin();
  return (
    <button
      onClick={() => {
        void composeForMark({ anchor: MARK_ANCHOR, kind: "comment" }).catch(() => {});
      }}
      type="button"
    >
      Compose on the selection
    </button>
  );
}

/** A page filling the margin's slot, as the issue page does: it wraps the margin's own content
 *  in its panel, and keeps that one wrapper whether or not the margin is drawing the panel
 *  (`features/issue/IssueMargin.tsx`'s `cardsOnly`). */
function PageWithPanel({ panel }: { panel: boolean }): ReactNode {
  const fill = useCallback(
    (content: ReactNode) => (
      <div data-testid="page-panel">
        <div className="contents">{content}</div>
        {panel ? <p>Every property of this issue</p> : null}
      </div>
    ),
    [panel]
  );
  useFillMarginSlot(fill);
  return null;
}

/** The margin beside a page that fills its slot, with the reader's own collapse control. */
function MarginUnderTest(): ReactNode {
  const [collapsed, setCollapsed] = useState(false);
  return (
    <>
      <button onClick={() => setCollapsed(true)} type="button">
        Collapse the margin
      </button>
      <PageWithPanel panel={!collapsed} />
      <Margin collapsed={collapsed} onCollapsedChange={setCollapsed} />
    </>
  );
}

function renderMargin() {
  // A rail to collapse to exists from 1280 px up; below it the margin is the phone sheet and
  // the reader's collapse preference does nothing (`Margin.tsx`).
  const restoreMatchMedia = stubMatchMedia(false);
  const queryClient = new QueryClient({
    defaultOptions: {
      mutations: { retry: false },
      queries: { retry: false, staleTime: Number.POSITIVE_INFINITY },
    },
  });
  queryClient.setQueryData(["issue", issue.key], issue);
  queryClient.setQueryData(["inbox"], []);
  queryClient.setQueryData(["asks", issue.key], []);
  queryClient.setQueryData(["user-state"], {});
  queryClient.setQueryData(["comments", issue.key], []);
  const reads = [
    spyOn(api, "getIssue").mockResolvedValue(issue),
    spyOn(api, "getInbox").mockResolvedValue([]),
    spyOn(api, "listComments").mockResolvedValue([]),
  ];
  const view = render(
    <MemoryRouter initialEntries={[buildIssuePath({ key: issue.key, kind: "issue" })]}>
      <QueryClientProvider client={queryClient}>
        <MarginProvider>
          <MarginSlotProvider drawsSlot>
            <RegisterDocument />
            <ComposeOnMark />
            <MarginUnderTest />
          </MarginSlotProvider>
        </MarginProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );
  return {
    unmount: () => {
      view.unmount();
      restoreMatchMedia();
      for (const read of reads) read.mockRestore();
    },
  };
}

test("collapsing the margin keeps the unsent comment in its composer", async () => {
  const user = userEvent.setup();
  const view = renderMargin();

  try {
    await user.click(screen.getByRole("button", { name: "Compose on the selection" }));
    const composer = await screen.findByRole("form", { name: "Comment composer" });
    const box = within(composer).getByLabelText("Comment");
    await user.click(box);
    await user.paste("Half a thought.");
    expect(composer.textContent).toContain("Half a thought.");
    // The page stops drawing its panel in the margin as the reader collapses it, which is the
    // moment the wrapper above the composer used to change.
    await user.click(screen.getByRole("button", { name: "Collapse the margin" }));

    expect(screen.getByRole("button", { name: "Show margin" })).toBeInTheDocument();
    expect(screen.queryByText("Every property of this issue")).not.toBeInTheDocument();
    // The same composer, not a fresh one: a remount would have taken the draft with it. The
    // collapsed margin is hidden, so the form is read as the node it is rather than by role.
    expect(composer.isConnected).toBe(true);
    expect(composer.textContent).toContain("Half a thought.");
  } finally {
    view.unmount();
  }
});
