import { beforeAll, expect, setSystemTime, spyOn, test } from "bun:test";
import { focusManager, QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, type RenderResult, render, waitFor } from "@testing-library/react";

import { nextFrame } from "../../__tests__/frames";
import { api } from "../../api/client";
import type { Comment, IssueDetails, MessageRead } from "../../api/types";
import { warmMarkdownRenderer } from "../refs/markdown-engine";
import { FOOT_BOX_ATTRIBUTE, FOOT_BOX_TEST_ID } from "./identity-block";
import { type OutlineWriter, OutlineWriterProvider, useOutlineWriter } from "./outline-writer";

// The rows draw through the shared renderer, whose Markdown engine loads lazily, and they run in
// random order, so the file loads the engine before its first row, whichever row that is.
beforeAll(async () => {
  await warmMarkdownRenderer();
});

// R7: the writer adds no request. It reads the issue's asks, comments and messages under the keys
// the tree reads them by (`useIssueTree.ts`), and keeps them fresh as the tree does, by events
// and never by their age (decision 180). Staleness is per observer, so under the app's 30 s
// `staleTime` and its window-focus refetch (`main.tsx`) a reader who comes back to the window
// more than 30 s after the load would read all three again for the writer alone.
test("a window focus more than 30 s after the load reads the writer's asks, comments and messages no more", async () => {
  const reads = [
    spyOn(api, "listIssueAsks").mockResolvedValue([]),
    spyOn(api, "listComments").mockResolvedValue([]),
    spyOn(api, "listIssueMessages").mockResolvedValue([]),
  ];
  // The app's client's defaults (`main.tsx`), which a read that sets none of its own takes.
  const client = new QueryClient({
    defaultOptions: { queries: { refetchOnWindowFocus: true, retry: false, staleTime: 30_000 } },
  });
  try {
    const view = render(
      <QueryClientProvider client={client}>
        <OutlineWriterProvider issueKey="CORE-1" openAsks={[]}>
          {null}
        </OutlineWriterProvider>
      </QueryClientProvider>
    );
    try {
      await waitFor(() => expect(reads.map((read) => read.mock.calls.length)).toEqual([1, 1, 1]));
      await waitFor(() => expect(client.isFetching()).toBe(0));
      // The reader leaves the window and comes back to it more than 30 s after the load.
      setSystemTime(Date.now() + 31_000);
      act(() => {
        focusManager.setFocused(false);
        focusManager.setFocused(true);
      });
      // Long enough for a read the focus started to have been sent.
      await act(async () => {
        const waited = Promise.withResolvers<void>();
        setTimeout(waited.resolve, 50);
        await waited.promise;
      });
      expect(
        reads.map((read) => read.mock.calls.length),
        "the window's focus read the asks, the comments or the messages again"
      ).toEqual([1, 1, 1]);
    } finally {
      view.unmount();
    }
  } finally {
    focusManager.setFocused(undefined);
    setSystemTime();
    for (const read of reads) read.mockRestore();
    client.clear();
  }
});

// When the writer unmounts while its `focus()` is waiting on an animation frame for a field to
// draw, that frame is cancelled: a box mounted afterwards does not lose the reader's focus to
// the unmounted writer.
test("unmounting the writer cancels a pending focus frame so a later box does not take the focus", async () => {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  client.setQueryData(["asks", "CORE-1"], []);
  client.setQueryData(["asks", "CORE-1", "thread"], []);
  client.setQueryData(["comments", "CORE-1"], []);
  client.setQueryData(["issue-messages", "CORE-1"], []);
  const held: { writer?: OutlineWriter } = {};
  function Capture(): null {
    held.writer = useOutlineWriter();
    return null;
  }
  const first = render(
    <QueryClientProvider client={client}>
      <OutlineWriterProvider issueKey="CORE-1" openAsks={[]}>
        <Capture />
      </OutlineWriterProvider>
    </QueryClientProvider>
  );
  let second: RenderResult | undefined;
  try {
    if (held.writer === undefined) throw new Error("the provider drew no writer");
    // Queue a focus frame while no composer field is in the document, then unmount the writer.
    act(() => held.writer?.focus());
    first.unmount();

    // Mount a fresh box with a field of its own, and let more frames run than the writer waits.
    second = render(
      <section {...{ [FOOT_BOX_ATTRIBUTE]: "" }} data-testid={FOOT_BOX_TEST_ID}>
        <textarea aria-label="Comment" />
      </section>
    );
    const field = second.getByLabelText("Comment");
    for (let frame = 0; frame < 12; frame += 1) {
      await act(() => nextFrame());
    }
    expect(field).not.toHaveFocus();
  } finally {
    second?.unmount();
    first.unmount();
    client.clear();
  }
});

/** The writer of issue CORE-1 over `comments` and `messages`, held under the keys the tree reads
 *  them by, with the asks read answered with none and no ask the issue holds open. */
function drawWriter(
  comments: readonly Comment[],
  messages: readonly MessageRead[]
): { readonly unmount: () => void; readonly writer: () => OutlineWriter | undefined } {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  client.setQueryData(["asks", "CORE-1"], []);
  client.setQueryData(["comments", "CORE-1"], [...comments]);
  client.setQueryData(["issue-messages", "CORE-1"], [...messages]);
  const held: { writer?: OutlineWriter } = {};
  function Capture(): null {
    held.writer = useOutlineWriter();
    return null;
  }
  const view = render(
    <QueryClientProvider client={client}>
      <OutlineWriterProvider issueKey="CORE-1" openAsks={[]}>
        <Capture />
      </OutlineWriterProvider>
    </QueryClientProvider>
  );
  return {
    unmount: () => {
      view.unmount();
      client.clear();
    },
    writer: () => held.writer,
  };
}

// Decision 573: a person's reply to a turn of an agents' comment thread tied to no text, a thread
// the history holds, takes that thread out of the history (`agents-only.ts`), and the outline then
// draws the thread whole, the reply under the comment it answers: the reply lands in the outline,
// whichever turn of the thread it answers. The writer decided from the reads before the reply,
// which still held the thread in the history, and sent the landing there: the history waited for
// a turn it never drew, the outline left the landing alone, and the focus fell to the page's body.
test("a reply to a turn of an agents' comment thread tied to no text lands in the outline, which draws the thread once a person wrote in it", () => {
  const planner = { id: "planner", kind: "session" };
  const head = { anchor: null, ask_id: null, author: planner, id: "c-agent", reply_to: null };
  const reviewer = { id: "reviewer", kind: "session" };
  const reply = { ...head, author: reviewer, id: "c-agent-reply", reply_to: head.id };
  const page = drawWriter([head, reply] as Comment[], []);
  try {
    for (const turn of [head, reply]) {
      act(() => page.writer()?.land({ to: "newest", under: { id: turn.id, kind: "comment" } }));
      expect(
        page.writer()?.landing,
        `the reply to ${turn.id}, a turn of an agents' comment thread, did not land in the outline`
      ).toMatchObject({ in: "outline", under: { id: turn.id } });
    }
  } finally {
    page.unmount();
  }
});

// Decision 127, which decision 573 leaves standing: a thread the Legion daemon began stays in the
// history whoever writes in it, so a reply to any turn of it lands there.
test("a reply to a turn of the Legion daemon's message thread lands in the history, which holds that thread whoever writes in it", () => {
  const root = { author: { id: "legion-daemon:CORE", kind: "session" }, id: "m-daemon" };
  const answer = { author: { id: "alice", kind: "user" }, id: "m-alice" };
  const thread = {
    message: root as MessageRead["message"],
    replies: [answer as MessageRead["message"]],
  };
  const page = drawWriter([], [thread]);
  try {
    for (const turn of [root, answer]) {
      act(() => page.writer()?.land({ to: "newest", under: { id: turn.id, kind: "message" } }));
      expect(
        page.writer()?.landing,
        `the reply to ${turn.id}, a turn of the daemon's thread, did not land in the history`
      ).toMatchObject({ in: "history", under: { id: turn.id } });
    }
  } finally {
    page.unmount();
  }
});

// Decision 127: the writer reads the rule (`agents-only.ts`) over the asks the outline and the
// history read it over: the asks the issue holds open, then the asks read (`issueAsks`). An
// agent's comment under an open approval request is a turn of a thread the history holds, which
// the outline draws no row of. The writer read the rule over the asks read alone, and while that
// read was out took every ask for a question: a landing back on that comment, once the line's
// target is ended, went to the outline, which draws no row for it and drops it, and which the
// history leaves alone, so the focus went nowhere. A reply to that comment is a person's turn in
// the thread, which takes the thread out of the history (decision 573): the outline then draws the
// comment under the issue, since it draws no approval request, with the reply under it.
test("with the asks read out, a reply to an agent's comment under an open approval request lands in the outline, and the line's target ended on it lands in the history", async () => {
  const approval = { id: "ask-approval", kind: "approval" };
  const comment = {
    anchor: null,
    ask_id: approval.id,
    author: { id: "planner", kind: "session" },
    id: "comment-agent",
    reply_to: null,
  } as Comment;
  // The asks read is out, and stays out for the case.
  const asksRead = spyOn(api, "listIssueAsks").mockImplementation(() => new Promise(() => {}));
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  client.setQueryData(["comments", "CORE-1"], [comment]);
  client.setQueryData(["issue-messages", "CORE-1"], []);
  const held: { writer?: OutlineWriter } = {};
  function Capture(): null {
    held.writer = useOutlineWriter();
    return null;
  }
  const view = render(
    <QueryClientProvider client={client}>
      <OutlineWriterProvider
        issueKey="CORE-1"
        openAsks={[approval] as unknown as IssueDetails["open_asks"]}
      >
        <Capture />
      </OutlineWriterProvider>
    </QueryClientProvider>
  );
  try {
    await waitFor(() => expect(asksRead, "the writer did not read the asks").toHaveBeenCalled());
    expect(client.getQueryData(["asks", "CORE-1"]), "the asks read answered").toBeUndefined();
    act(() => held.writer?.land({ to: "newest", under: { id: comment.id, kind: "comment" } }));
    expect(
      held.writer?.landing,
      "a person's reply to an agent's comment under an approval request did not land in the outline"
    ).toMatchObject({ in: "outline", under: { id: comment.id } });
    act(() => held.writer?.land({ to: "self", under: { id: comment.id, kind: "comment" } }));
    expect(
      held.writer?.landing,
      "the line's target ended on a turn of the history did not land in the history"
    ).toMatchObject({ in: "history", under: { id: comment.id } });
  } finally {
    view.unmount();
    asksRead.mockRestore();
    client.clear();
  }
});
