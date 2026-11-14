import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { type RenderResult, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";

import { api } from "../api/client";
import { AuthGate } from "../app";
import { installInboxApiMocks, issueAsk, mockAskReads } from "../features/inbox/inbox-fixture";

// The peek's latch is the shell's, not the drawer's: from a reader's first peek `app.tsx` keeps
// `InboxDrawer` mounted through every close (`inboxDrawerEverOpened`), so the filter, the search
// text and the bands they folded are theirs when they open it again (dispatch://INFRA-341).
// `features/inbox/InboxDrawer.test.tsx` holds the drawer's own half of that - it keeps what it
// was given while it is kept mounted - but it mounts the drawer under a harness of its own that
// reimplements the latch, so that file stays green on a shell that lost it. This row is the
// other half, and the only one that reads the shell's: it opens the peek the way a reader does,
// narrows it, closes it with Escape and opens it again.
installInboxApiMocks();

/** The shell with no route of its own to read: `app.tsx`'s `*` route draws `NotFoundPage`, which
 *  reads nothing, so what is rendered here is the shell and the peek alone. It is also a route
 *  the `i` binding is offered on - the Inbox page is the one place it is not, since a peek over
 *  itself shows nothing new (`app.tsx`'s `inbox-drawer` binding). */
function renderShell(): RenderResult {
  return render(
    <QueryClientProvider
      client={
        new QueryClient({
          defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
        })
      }
    >
      <MemoryRouter initialEntries={["/nowhere"]}>
        <AuthGate />
      </MemoryRouter>
    </QueryClientProvider>
  );
}

test("the shell keeps the peek the reader left: what they narrowed it to survives a close", async () => {
  const user = userEvent.setup();
  const mine = issueAsk();
  const bobs = issueAsk({
    id: "ask-bob",
    issue: { assignee: "alice", key: "CORE-2", title: "Bob's issue" },
    issue_key: "CORE-2",
    question: "Bob's question?",
  });
  const rows = [mine, bobs];
  const getInbox = spyOn(api, "getInbox").mockResolvedValue(rows);
  const getAsk = mockAskReads(rows);
  const view = renderShell();
  try {
    // The reader opens the peek the way the shell offers it, from the keyboard.
    await user.keyboard("i");
    const dialog = await screen.findByRole("dialog", { name: "Inbox" });
    await within(dialog).findByText("Which approach?");
    await within(dialog).findByText("Bob's question?");

    // And narrows it to one of the two questions.
    await user.type(within(dialog).getByRole("searchbox", { name: "Search asks" }), "Bob's");
    await waitFor(() =>
      expect(within(dialog).queryByText("Which approach?")).not.toBeInTheDocument()
    );

    // Escape is one level out at a time, and the reader is three levels in: out of the search
    // box to the list, off the row it landed on, and then out of the peek itself, which is the
    // press the shell's own dialog takes (`Inbox.tsx`'s Escape bindings, then `useDialog`).
    await user.keyboard("{Escape}{Escape}{Escape}");
    await waitFor(() =>
      expect(screen.queryByRole("dialog", { name: "Inbox" })).not.toBeInTheDocument()
    );

    // The peek they open again is the peek they left. A shell that unmounted the drawer on the
    // close would open it on the whole list with an empty box, which is what the two reads
    // below refuse: the box still holds their word, and the question it hides is still hidden.
    await user.keyboard("i");
    const reopened = await screen.findByRole("dialog", { name: "Inbox" });
    await within(reopened).findByText("Bob's question?");
    const box = within(reopened).getByRole<HTMLInputElement>("searchbox", {
      name: "Search asks",
    });
    expect(
      box.value,
      "the peek opened again with an empty search box: the shell let the drawer go on the close"
    ).toBe("Bob's");
    expect(within(reopened).queryByText("Which approach?")).not.toBeInTheDocument();
  } finally {
    view.unmount();
    getAsk.mockRestore();
    getInbox.mockRestore();
  }
});
