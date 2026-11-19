/**
 * The draft matrix, ask card rows (INFRA-1503).
 *
 * An ask's answer and its replies are the ask's, not the field's showing them (packages/dispatch
 * AGENTS.md): every field for one ask reads and writes the same draft, by the ask's id, in a
 * store that outlives every card (`answer-drafts.ts`, `reply-drafts.ts`), and the tab keeps each
 * for a reload of the page (`draft-store.ts`). Each cell writes a draft typed and never sent,
 * makes one move, and asserts where the text ends. A refused or stopped answer or reply is its
 * form's own write, which `AskCard.stopped-writes.test.tsx` holds to the ask it was pressed on;
 * what these stores keep is the text, so a cell holds it in no other state.
 *
 * Hosts: the ask card (`AskCard`) and the two fields it holds, which the composer census follows
 * up to the card and not past it (`DRAFT_FIELDS` in `composer-census/trees.ts`): the answer
 * field under an open question (`Ask answer`), and the reply composer under a settled one, in the
 * card's inline thread (`AskThread`, `Ask reply`) or behind its collapsed thread's Write a reply
 * (`AskThreadDisclosure`, `Ask reply, collapsed`).
 *
 * | Move                                               | answer | reply  | reply, collapsed |
 * | -------------------------------------------------- | ------ | ------ | ---------------- |
 * | The card drawn for another ask, then back          | cell   | cell   | cell             |
 * | The card ended, then drawn again (off)             | cell   | cell   | cell             |
 * | Write a reply closed, then opened (off)            | -      | -      | cell             |
 * | The note folded, then opened (off)                 | no (3) | -      | -                |
 * | The fields' ends of the card ordered the other way | no (1) | no (1) | no (1)           |
 * | The card drawn in another form                     | no (2) | no (2) | no (2)           |
 * | The question settled, or its answer changed        | no (4) | no (7) | no (7)           |
 * | The ask opened again                               | -      | no (5) | no (5)           |
 * | The ask's thread read fails, then lands            | -      | no (6) | no (6)           |
 * | The page reloaded, then back (off)                 | cell   | cell   | cell             |
 *
 * (off) marks a move that takes the field off the screen; the field drawn again holds the draft.
 * The reload rows end the whole page, as any reload does (`endAsAReload`): the stores forget what
 * they hold, as the next page's start without it, and the tab's `sessionStorage` is all the next
 * load finds, with the reader signed in as before (`signedInAsReader`). They are the fields'
 * lifetimes the census finds (`lifetime.ts`).
 *
 * The numbered reasons - the moves the composer census found that the rows above do not make -
 * are `REASONS` in `draft-matrix.ask.coverage.ts`, which holds each census item this file answers
 * for to a test above or to one of them.
 */
import { expect, test } from "bun:test";
import { QueryClientProvider } from "@tanstack/react-query";
import { render, screen } from "@testing-library/react";
import userEvent, { type UserEvent } from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";

import {
  BODY,
  endAsAReload,
  newQueryClient,
  signedInAsReader,
} from "../../__tests__/draft-matrix-fixture";
import type { Ask } from "../../api/types";
import { AskCard } from "./AskCard";
import { answeredAsk, installAskCardApiMocks, thread } from "./ask-card-fixture";

installAskCardApiMocks();

const ANSWER = "Ask answer";

/** The reply composer's two places in a card: its inline thread, and its collapsed one. */
const REPLIES = [
  { form: "inline", host: "Ask reply" },
  { form: "collapsed", host: "Ask reply, collapsed" },
] as const;

type ThreadForm = (typeof REPLIES)[number]["form"];

/** The open question an answer is typed under, and the one the card is drawn for after it. */
const openAsk = answeredAsk({ answer: null, state: "open" });
const otherOpenAsk = answeredAsk({
  answer: null,
  id: "ask-2",
  question: "Which rollout window?",
  state: "open",
});

/** The answered question a reply is typed under, and the one the card is drawn for after it. */
const settledAsk = answeredAsk();
const otherSettledAsk = answeredAsk({ id: "ask-2", question: "Which rollout window?" });

/** The page the rows draw: one card, as a host draws it for one ask, for the reader. */
interface Page {
  /** The same card drawn for `ask` where it was, as a host that draws one card for one ask after
   *  another does. */
  readonly move: (ask: Ask) => void;
  /** The card ended, and a card drawn for `ask` in its place over the same page. */
  readonly again: (ask: Ask) => void;
  /** Ends the page as a reload does and loads it again over the same tab on `ask`, signed in as
   *  the same reader, with a new query client (`endAsAReload`, `signedInAsReader`). */
  readonly reload: (ask: Ask) => void;
  readonly unmount: () => void;
  readonly user: UserEvent;
}

function drawPage(ask: Ask, form: ThreadForm): Page {
  const user = userEvent.setup();
  let client = signedInAsReader(newQueryClient());
  const tree = (shown: Ask) => (
    <MemoryRouter future={{ v7_relativeSplatPath: true, v7_startTransition: true }}>
      <QueryClientProvider client={client}>
        <AskCard
          ask={shown}
          getAskThread={async () => thread(shown)}
          initialThread={thread(shown)}
          thread={form}
        />
      </QueryClientProvider>
    </MemoryRouter>
  );
  let view = render(tree(ask));
  return {
    again: (shown) => {
      view.unmount();
      view = render(tree(shown));
    },
    move: (shown) => view.rerender(tree(shown)),
    reload: (shown) => {
      endAsAReload(() => view.unmount());
      client = signedInAsReader(newQueryClient());
      view = render(tree(shown));
    },
    unmount: () => view.unmount(),
    user,
  };
}

/** The card's answer field. */
function answerField(): HTMLTextAreaElement {
  return screen.getByLabelText<HTMLTextAreaElement>("Your answer");
}

/** The card's reply field, where it is on screen. */
function replyField(): HTMLTextAreaElement {
  return screen.getByRole<HTMLTextAreaElement>("textbox", { name: "Reply" });
}

/** The card's reply field, opened by its Write a reply where the thread is collapsed. */
async function openReply(user: UserEvent, form: ThreadForm): Promise<HTMLTextAreaElement> {
  if (form === "collapsed") {
    await user.click(await screen.findByRole("button", { name: "Write a reply" }));
  }
  return screen.findByRole<HTMLTextAreaElement>("textbox", { name: "Reply" });
}

/** Types `BODY` into `field`. */
async function type(user: UserEvent, field: HTMLTextAreaElement): Promise<void> {
  await user.click(field);
  await user.paste(BODY);
}

// The card drawn for another ask is the host's move: the field shows that ask's draft, none, and
// the first ask's draft waits under its id for the card drawn for it again.
test(`${ANSWER} · the card drawn for another ask, then back · typed`, async () => {
  const page = drawPage(openAsk, "inline");
  try {
    await type(page.user, answerField());
    page.move(otherOpenAsk);
    const other = answerField().value;
    page.move(openAsk);
    expect({ back: answerField().value, other }).toEqual({ back: BODY, other: "" });
  } finally {
    page.unmount();
  }
});

// A card that ends takes its field with it; the card drawn for the ask again holds its draft.
test(`${ANSWER} · the card ended, then drawn again · typed`, async () => {
  const page = drawPage(openAsk, "inline");
  try {
    await type(page.user, answerField());
    page.again(openAsk);
    expect(answerField().value).toBe(BODY);
  } finally {
    page.unmount();
  }
});

// A reload ends the whole page and loads it again over the same tab: the card drawn for the ask
// again holds the answer the reader typed, which only the tab kept.
test(`${ANSWER} · the page reloaded, then back · typed`, async () => {
  const page = drawPage(openAsk, "inline");
  try {
    await type(page.user, answerField());
    page.reload(openAsk);
    expect((await screen.findByLabelText<HTMLTextAreaElement>("Your answer")).value).toBe(BODY);
  } finally {
    page.unmount();
  }
});

for (const { form, host } of REPLIES) {
  // The card drawn for another ask is the host's move: the composer, which stays open, shows
  // that ask's draft, none, and the first ask's draft waits under its id.
  test(`${host} · the card drawn for another ask, then back · typed`, async () => {
    const page = drawPage(settledAsk, form);
    try {
      await type(page.user, await openReply(page.user, form));
      page.move(otherSettledAsk);
      const other = replyField().value;
      page.move(settledAsk);
      expect({ back: replyField().value, other }).toEqual({ back: BODY, other: "" });
    } finally {
      page.unmount();
    }
  });

  // A card that ends takes its composer with it; the composer of the card drawn for the ask
  // again holds its draft.
  test(`${host} · the card ended, then drawn again · typed`, async () => {
    const page = drawPage(settledAsk, form);
    try {
      await type(page.user, await openReply(page.user, form));
      page.again(settledAsk);
      expect((await openReply(page.user, form)).value).toBe(BODY);
    } finally {
      page.unmount();
    }
  });

  // A reload ends the whole page and loads it again over the same tab: the composer of the card
  // drawn for the ask again holds the reply the reader typed, which only the tab kept.
  test(`${host} · the page reloaded, then back · typed`, async () => {
    const page = drawPage(settledAsk, form);
    try {
      await type(page.user, await openReply(page.user, form));
      page.reload(settledAsk);
      expect((await openReply(page.user, form)).value).toBe(BODY);
    } finally {
      page.unmount();
    }
  });
}

// Closing Write a reply takes the composer off the screen; opening it again draws a composer
// that holds the ask's draft.
test("Ask reply, collapsed · Write a reply closed, then opened · typed", async () => {
  const page = drawPage(settledAsk, "collapsed");
  try {
    await type(page.user, await openReply(page.user, "collapsed"));
    const toggle = screen.getByRole("button", { name: "Write a reply" });
    await page.user.click(toggle);
    const closed = screen.queryByRole("textbox", { name: "Reply" });
    await page.user.click(toggle);
    expect({ closed, text: replyField().value }).toEqual({ closed: null, text: BODY });
  } finally {
    page.unmount();
  }
});
