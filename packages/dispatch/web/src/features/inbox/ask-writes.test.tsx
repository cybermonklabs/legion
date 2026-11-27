/**
 * What an Inbox keeps of an ask write once it has landed (`useAskWrites`). A kind whose surfaces
 * read nothing of a landed press keeps none. The answers and the declines keep one only while a
 * surface is drawn for its ask (`useOutstandingAnswerWrites`), which reads its landing.
 */

import { expect, test } from "bun:test";
import { QueryClientProvider } from "@tanstack/react-query";
import { act, render, renderHook, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";

import { whoAmIQuery } from "../../api/queries";
import { DispatchQueryClient } from "../../api/query-client";
import type { Ask } from "../../api/types";
import { type AskPress, useAskWrites } from "./ask-writes";
import { issueAsk } from "./inbox-fixture";
import {
  type OutstandingAnswerWrites,
  useInboxAnswerWrite,
  useOutstandingAnswerWrites,
} from "./outstanding-answers";

/** Dispatch's own client, signed in as alice, around `children`. */
function signedIn(): ({ children }: { children: ReactNode }) => ReactNode {
  const client = new DispatchQueryClient({ defaultOptions: { queries: { retry: false } } });
  client.setQueryData(whoAmIQuery().queryKey, { event_head: "0", kind: "user", login: "alice" });
  return ({ children }) => <QueryClientProvider client={client}>{children}</QueryClientProvider>;
}

/** The ask a press of the first case is of. */
function askOf({ askId }: { readonly askId: string }): string {
  return askId;
}

// A snooze or a hand-on reads nothing of a press once it has landed, so its entry goes as it
// lands: the entries would otherwise grow by one for each ask written while the Inbox stays.
test("a landed press of a kind that keeps no landed press leaves no entry behind", async () => {
  const { result, unmount } = renderHook(
    () =>
      useAskWrites(askOf, {
        inline: true,
        label: () => "Ask written",
        method: "PUT",
        write: async () => ({}),
      }),
    { wrapper: signedIn() }
  );
  try {
    act(() => result.current.press({ askId: "ask-1" }));
    expect([...result.current.out]).toEqual(["ask-1"]);
    await waitFor(() => expect(result.current.newest.size).toBe(0));
    expect(result.current.out.size).toBe(0);
  } finally {
    unmount();
  }
});

/** The ask the answer cases write. */
const ASK: Ask = issueAsk({ id: "ask-1" });

/** What an answer case reads of what it draws: the Inbox's answers, and the press of the surface
 *  drawn for `ASK` while it is drawn. */
interface Seen {
  press?: (press: AskPress) => void;
  writes?: OutstandingAnswerWrites;
}

/** The Inbox's answers, with a surface drawn for `ASK` while `drawn`, whose write answers when
 *  `answered` settles. */
function Answers({
  answered,
  drawn,
  seen,
}: {
  answered: Promise<Ask>;
  drawn: boolean;
  seen: Seen;
}): ReactNode {
  const writes = useOutstandingAnswerWrites();
  seen.writes = writes;
  return drawn ? <AnswerSurface answered={answered} seen={seen} writes={writes} /> : null;
}

/** A surface drawn for `ASK` in the Inbox's answers (`useInboxAnswerWrite`). */
function AnswerSurface({
  answered,
  seen,
  writes,
}: {
  answered: Promise<Ask>;
  seen: Seen;
  writes: OutstandingAnswerWrites;
}): ReactNode {
  const write = useInboxAnswerWrite(writes, ASK.id, {
    label: () => "Answer",
    method: "POST",
    write: () => answered,
  });
  seen.press = write?.mutate;
  return null;
}

// A card drawn for the ask reads the answer's landing (`isSuccess`, `submittedAt`), so the
// landed answer stays while one is drawn. The last card drawn for the ask going lets it go.
test("a landed answer stays while a surface is drawn for its ask, and goes with the last one", async () => {
  const seen: Seen = {};
  const answered = Promise.resolve(ASK);
  const wrapper = signedIn();
  const view = render(<Answers answered={answered} drawn seen={seen} />, { wrapper });
  try {
    act(() => seen.press?.({ ask: ASK }));
    await waitFor(() => expect(seen.writes?.of(ASK.id).isSuccess).toBe(true));
    view.rerender(<Answers answered={answered} drawn={false} seen={seen} />);
    await waitFor(() => expect(seen.writes?.of(ASK.id).submittedAt).toBe(0));
    expect(seen.writes?.of(ASK.id).isSuccess).toBe(false);
  } finally {
    view.unmount();
  }
});

// The card that pressed went while the answer was out, and no card is drawn for the ask when it
// lands: nothing reads the landing, so it leaves no entry behind.
test("an answer that lands with no surface drawn for its ask leaves no entry behind", async () => {
  const seen: Seen = {};
  const answer = Promise.withResolvers<Ask>();
  const wrapper = signedIn();
  const view = render(<Answers answered={answer.promise} drawn seen={seen} />, { wrapper });
  try {
    act(() => seen.press?.({ ask: ASK }));
    expect(seen.writes?.of(ASK.id).isPending).toBe(true);
    view.rerender(<Answers answered={answer.promise} drawn={false} seen={seen} />);
    expect(seen.writes?.of(ASK.id).isPending).toBe(true);
    await act(async () => {
      answer.resolve(ASK);
      await answer.promise;
    });
    await waitFor(() => expect(seen.writes?.of(ASK.id).isPending).toBe(false));
    expect(seen.writes?.of(ASK.id).submittedAt).toBe(0);
  } finally {
    view.unmount();
  }
});
