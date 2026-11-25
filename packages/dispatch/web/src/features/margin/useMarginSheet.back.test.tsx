import { beforeAll, expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, within } from "@testing-library/react";
import userEvent, { type UserEvent } from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";

import { heldDraftWords, stopReplySend } from "../../__tests__/stop-late-write";
import { ApiError, api } from "../../api/client";
import { warmMarkdownRenderer } from "../refs/markdown-engine";
import { buildIssuePath } from "../refs/page-routes";
import { SendStoppedError } from "../writes/keyed-write";
import { Margin } from "./Margin";
import { MarginProvider, marginReplySendKey } from "./margin-context";
import { comment, issue, PHONE_WIDTH, stubViewport } from "./margin-fixture";

// The rows draw through the shared renderer, whose Markdown engine loads lazily, and they run in
// random order, so the file loads the engine before its first row, whichever row that is.
beforeAll(async () => {
  await warmMarkdownRenderer();
});

/** What a refusal Dispatch gave says in the reply composer. */
const refusedText = "Couldn't send — the server is down";

/** What a send the reader stopped says in its composer at its own address: Dispatch has not
 *  confirmed it, and its Retry asks again under its own key. */
const stoppedText = new SendStoppedError().message;

/** The issue's margin on a phone, over the one comment thread `comment` roots, in `queryClient`. */
function phoneMargin(queryClient: QueryClient): () => void {
  queryClient.setQueryData(["issue", issue.key], issue);
  queryClient.setQueryData(["inbox"], []);
  queryClient.setQueryData(["asks", issue.key], []);
  queryClient.setQueryData(["user-state"], {});
  queryClient.setQueryData(["comments", issue.key], [comment]);
  const restoreViewport = stubViewport(PHONE_WIDTH);
  const view = render(
    <MemoryRouter initialEntries={[buildIssuePath({ key: issue.key, kind: "issue" })]}>
      <QueryClientProvider client={queryClient}>
        <MarginProvider>
          <Margin />
        </MarginProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );
  return () => {
    view.unmount();
    restoreViewport();
  };
}

/** Opens the comment's thread from its card, as a tap on a phone does: full-screen, over the
 *  margin's list. Answers the thread view. */
async function openThread(user: UserEvent): Promise<HTMLElement> {
  const card = await screen.findByTestId(`margin-comment-${comment.id}`);
  await user.click(within(card).getByRole("button", { expanded: false }));
  return screen.findByRole("dialog", { name: "Thread" });
}

/** Comes back to the comment's thread after Back: the thread view a tap on its card opens, or
 *  the card itself where the list shows it open already, as it does for a thread the margin
 *  opened on the reader's arrival. Answers where the thread's reply shows. */
async function returnToThread(user: UserEvent): Promise<HTMLElement> {
  const card = await screen.findByTestId(`margin-comment-${comment.id}`);
  return card.getAttribute("aria-expanded") === "true" ? card : openThread(user);
}

/** The thread's reply, as a stopped send answers it (`stopReplySend`). */
const threadReply = {
  actor: comment.author,
  excerpt: "",
  id: comment.id,
  parentKind: "comment",
} as const;

// A phone margin thread's Back is a move of the host's, not one the reader makes with the
// composer's own controls, so it carries the reply's draft nowhere: the draft is held for its
// thread, and opening the thread again brings it back - its text, a refusal's note, and a stopped
// send's note that it may have been delivered. Nothing is sent meanwhile.
for (const held of ["typed", "refused", "stopped"] as const) {
  test(`on a phone, a margin thread's reply ${heldDraftWords[held]} comes back in its thread after Back`, async () => {
    const user = userEvent.setup();
    const queryClient = new QueryClient({
      defaultOptions: {
        mutations: { retry: false },
        queries: { retry: false, staleTime: Number.POSITIVE_INFINITY },
      },
    });
    const createComment = spyOn(api, "createComment").mockRejectedValue(
      new ApiError(409, { code: "UNAVAILABLE", error: "the server is down" })
    );
    const body = `Reply ${heldDraftWords[held]}, then Back`;
    if (held === "stopped") {
      await stopReplySend(
        queryClient,
        createComment,
        marginReplySendKey(comment.id),
        body,
        threadReply
      );
    }
    const restore = phoneMargin(queryClient);

    try {
      let dialog: HTMLElement;
      if (held === "stopped") {
        // Coming to a document whose thread holds a reply opens that thread.
        dialog = await screen.findByRole("dialog", { name: "Thread" });
        expect(within(dialog).getByText(stoppedText)).toBeTruthy();
      } else {
        dialog = await openThread(user);
        const field = within(dialog).getByRole<HTMLTextAreaElement>("textbox", { name: "Reply" });
        await user.click(field);
        await user.paste(body);
        if (held === "refused") {
          await user.click(within(dialog).getByRole("button", { name: "Send" }));
          await within(dialog).findByText(refusedText);
        }
      }
      const sent = createComment.mock.calls.length;
      await user.click(within(dialog).getByRole("button", { name: "Back" }));
      expect(screen.queryByRole("dialog", { name: "Thread" })).not.toBeInTheDocument();

      const place = await returnToThread(user);
      // The text and the note in one comparison, so a row that fails says which of them is gone.
      expect({
        refusal: within(place).queryByText(refusedText) !== null,
        stopped: within(place).queryByText(stoppedText) !== null,
        text: within(place).getByRole<HTMLTextAreaElement>("textbox", { name: "Reply" }).value,
      }).toEqual({ refusal: held === "refused", stopped: held === "stopped", text: body });
      expect(createComment).toHaveBeenCalledTimes(sent);
    } finally {
      restore();
      createComment.mockRestore();
    }
  });
}
