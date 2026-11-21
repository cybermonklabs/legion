import { expect, spyOn, test } from "bun:test";
// biome-ignore lint/style/noRestrictedImports: a paste of a file, which user-event's paste does not carry
import { act, fireEvent, screen, waitFor, within } from "@testing-library/react";
import userEvent, { type UserEvent } from "@testing-library/user-event";

import { keyedAttempt } from "../../__tests__/keyed-attempt";
import { api } from "../../api/client";
import type { AnswerAskInput, Ask, TextRewrite } from "../../api/types";
import { KeymapProvider } from "../shell/KeymapProvider";
import { appKeymap } from "../shell/keymap";
import { AskCard } from "./AskCard";
import { installAskCardApiMocks, renderCard, thread } from "./ask-card-fixture";

// The ask card as INFRA-3231's R7 has it: one split button whose menu holds the card's other
// actions in three groups with their keys, the rewrite's one badge, whose turn it is only where
// it is not the reader's, the line that names who handed the question on, and no urgency or
// Asked-of label.

installAskCardApiMocks();

function openAsk(overrides: Partial<Ask> = {}): Ask {
  return {
    anchor: null,
    answer: null,
    artifact_id: null,
    asked_of: null,
    asked_of_at: null,
    asked_of_by: null,
    author: { id: "session-1", kind: "session" },
    block_id: null,
    created_at: "2026-10-08T00:00:00Z",
    edited_at: null,
    id: "ask-1",
    issue_key: "CORE-1",
    kind: "question",
    multiple: false,
    opened_event_id: 1,
    options: [
      { description: "Go out today.", label: "Ship" },
      { description: "Wait a week.", label: "Hold" },
    ],
    question: "Ship the release today, or hold it a week for the migration?",
    state: "open",
    urgency: "blocking",
    waiting_on: "human",
    waiting_on_person: "alice",
    ...overrides,
  };
}

function approvalAsk(): Ask {
  return openAsk({
    kind: "approval",
    options: [{ label: "Approve" }, { label: "Request changes" }],
    question: "Approve spec.md (version 4)?",
  });
}

const rewrittenAt = "2026-10-08T00:05:00Z";

/** The ask the agent wrote, as Dispatch keeps it beside the rewrite. */
const agentWording: TextRewrite["original"] = {
  body: "R3 flip audit: go with B per the train?",
  options: [{ description: "Per the train.", label: "Ship" }, { label: "Hold" }],
};

/** The card for `ask` under the keymap its keys register with, as the app draws it. */
function keyedCard(ask: Ask) {
  return (
    <KeymapProvider>
      <AskCard ask={ask} initialThread={thread(ask)} />
    </KeymapProvider>
  );
}

/** The card drawn for the ask `id`, `openAsk`'s by default. */
function card(id = "ask-1") {
  return within(screen.getByTestId(`ask-${id}`));
}

/** Opens the card's menu and answers its groups: each the labels and keys of its items. */
async function menuGroups(user: UserEvent): Promise<{ key: string | null; label: string }[][]> {
  await user.click(screen.getByRole("button", { name: "More actions" }));
  const menu = await screen.findByRole("menu", { name: "More actions" });
  return Array.from(menu.querySelectorAll("[data-ask-menu] > div"), (group) =>
    Array.from(group.querySelectorAll<HTMLElement>("[role=menuitem]"), (item) => ({
      key: item.getAttribute("aria-keyshortcuts"),
      label: item.querySelector("span")?.textContent ?? "",
    }))
  );
}

test("an open question's menu holds its other actions in three groups, each with its key", async () => {
  const user = userEvent.setup();
  const { view } = renderCard(keyedCard(openAsk()));
  try {
    await screen.findByRole("radio", { name: /^Ship/ });
    expect(await menuGroups(user)).toEqual([
      [
        { key: "r", label: "Rewrite" },
        { key: "b", label: "Ask back" },
        { key: "a", label: "Hand this question to…" },
      ],
      [{ key: "n", label: "Decline to answer" }],
      [{ key: "d", label: "Define selected words" }],
    ]);
    // Define acts on words selected in the question, and none are.
    expect(screen.getByRole("menuitem", { name: "Define selected words" })).toHaveAttribute(
      "aria-disabled",
      "true"
    );
  } finally {
    view.unmount();
  }
});

test("an approval request's menu is shorter: it is not rewritten and cannot be declined", async () => {
  const user = userEvent.setup();
  const { view } = renderCard(keyedCard(approvalAsk()));
  try {
    await screen.findByRole("radio", { name: "Approve" });
    expect(await menuGroups(user)).toEqual([
      [
        { key: "b", label: "Ask back" },
        { key: "a", label: "Hand this question to…" },
      ],
      [{ key: "d", label: "Define selected words" }],
    ]);
  } finally {
    view.unmount();
  }
});

test("the card draws no urgency label, no Asked-of label and no Decline button of its own", async () => {
  const { view } = renderCard(
    keyedCard(openAsk({ asked_of: "alice", waiting_on_person: "alice" }))
  );
  try {
    await card().findByRole("radio", { name: /^Ship/ });
    // The card's edge and its name keep the urgency (a decision block's header names it in
    // words, `AskBlockCard.test.tsx`).
    expect(screen.getByRole("article", { name: "Urgency: Blocking" })).toBeTruthy();
    expect(card().queryByText(/^blocking$/i)).not.toBeInTheDocument();
    expect(card().queryByText(/Asked of/)).not.toBeInTheDocument();
    expect(card().queryByRole("button", { name: "Decline to answer" })).not.toBeInTheDocument();
    // The reader's own question is waiting on them wherever it is drawn, so the card says
    // nothing of whose turn it is.
    expect(card().queryByText(/Waiting on/)).not.toBeInTheDocument();
  } finally {
    view.unmount();
  }
});

test("a question handed on names whose turn it is and who handed it on, and when", async () => {
  const { view } = renderCard(
    keyedCard(
      openAsk({
        asked_of: "bob",
        asked_of_at: "2026-10-08T00:10:00Z",
        asked_of_by: "dana",
        waiting_on_person: "bob",
      })
    )
  );
  try {
    const turn = await screen.findByTestId("turn-ask-1");
    expect(turn.textContent).toBe("Waiting on bob,");
    const handedOn = view.container.querySelector("[data-ask-handed-on]");
    expect(handedOn?.textContent).toMatch(/^handed on by dana /);
    expect(handedOn?.querySelector("time")).not.toBeNull();
  } finally {
    view.unmount();
  }
});

test("Rewrite reads Rewriting… at once, then Rewritten, and the badge or w shows the original in place", async () => {
  const user = userEvent.setup();
  const ask = openAsk();
  // The press reads the ask's thread again once it settles.
  const getAsk = spyOn(api, "getAsk").mockResolvedValue(thread(ask));
  const rewriteAsk = spyOn(api, "rewriteAsk").mockResolvedValue({
    ask_id: ask.id,
    rewrite: {
      at: "2026-10-08T00:04:00Z",
      by: "person",
      original: agentWording,
      state: "rewriting",
    },
    started: true,
  });
  const { rerender, view } = renderCard(keyedCard(ask));
  try {
    await card().findByRole("radio", { name: /^Ship/ });
    await user.click(screen.getByRole("button", { name: "More actions" }));
    await user.click(await screen.findByRole("menuitem", { name: "Rewrite" }));
    expect(rewriteAsk).toHaveBeenCalledWith(ask.id, keyedAttempt);
    expect((await card().findByRole("status")).textContent).toBe("Rewriting…");

    // The rewrite's `ask.edited` refreshes the ask's read, which carries the rewrite in place.
    rerender(
      keyedCard({
        ...ask,
        rewrite: { at: rewrittenAt, by: "person", original: agentWording, state: "rewritten" },
      })
    );
    const badge = await card().findByRole("button", { name: "Rewritten" });
    expect(card().getByText(ask.question)).toBeTruthy();
    // The card says nothing else of the rewrite: not who asked for it, nor why.
    expect(card().queryByText(/at your request|before it reached you/)).not.toBeInTheDocument();

    await user.click(badge);
    expect(card().getByRole("button", { name: "Original" })).toHaveAttribute(
      "aria-pressed",
      "true"
    );
    expect(card().getByText(agentWording.body)).toBeTruthy();
    expect(card().queryByText(ask.question)).not.toBeInTheDocument();
    // An option keeps its label and shows the description the agent wrote, or none.
    expect(card().getByRole("radio", { name: "Ship Per the train." })).toBeTruthy();
    expect(card().getByRole("radio", { name: "Hold" })).toBeTruthy();

    // `w`, with the card in hand, shows the rewrite again.
    act(() => card().getByRole("radio", { name: "Ship Per the train." }).focus());
    await user.keyboard("w");
    expect(await card().findByRole("button", { name: "Rewritten" })).toBeTruthy();
    expect(card().getByText(ask.question)).toBeTruthy();
  } finally {
    view.unmount();
    rewriteAsk.mockRestore();
    getAsk.mockRestore();
  }
});

test("a reviewer's rewrite wears the same Rewritten badge", async () => {
  const ask = openAsk({
    rewrite: { at: rewrittenAt, by: "reviewer", original: agentWording, state: "rewritten" },
  });
  const { view } = renderCard(keyedCard(ask));
  try {
    expect(await card().findByRole("button", { name: "Rewritten" })).toBeTruthy();
  } finally {
    view.unmount();
  }
});

test("a failed rewrite reads Rewrite failed, its reason only in the tooltip, and r tries again", async () => {
  const user = userEvent.setup();
  const reason = "The rewrite changed an option's label.";
  const ask = openAsk({
    rewrite: { at: rewrittenAt, by: "person", original: agentWording, reason, state: "failed" },
  });
  const getAsk = spyOn(api, "getAsk").mockResolvedValue(thread(ask));
  const rewriteAsk = spyOn(api, "rewriteAsk").mockResolvedValue({
    ask_id: ask.id,
    rewrite: {
      at: "2026-10-08T00:06:00Z",
      by: "person",
      original: agentWording,
      state: "rewriting",
    },
    started: true,
  });
  const { view } = renderCard(keyedCard(ask));
  try {
    const badge = await card().findByRole("button", { name: "Rewrite failed" });
    expect(badge).toHaveAttribute("title", `${reason} Try again (r)`);
    expect(view.container.textContent).not.toContain(reason);

    act(() => card().getByRole("radio", { name: /^Ship/ }).focus());
    await user.keyboard("r");
    await waitFor(() => expect(rewriteAsk).toHaveBeenCalledWith(ask.id, keyedAttempt));
    expect((await card().findByRole("status")).textContent).toBe("Rewriting…");
  } finally {
    view.unmount();
    rewriteAsk.mockRestore();
    getAsk.mockRestore();
  }
});

// A host draws one card for one ask after another (a decision block's, `AskBlockCard`, draws it
// with no key). A Rewrite pressed on the first ask is that ask's: the card drawn for the next
// shows no Rewriting… of it, and takes the next ask's own Rewrite.
test("the card drawn for the next ask shows nothing of a Rewrite of the one before, and rewrites its own", async () => {
  const user = userEvent.setup();
  const first = openAsk();
  const next = openAsk({ id: "ask-2", question: "Which region goes first?" });
  const getAsk = spyOn(api, "getAsk").mockImplementation(async (id) =>
    thread(id === next.id ? next : first)
  );
  const rewriteAsk = spyOn(api, "rewriteAsk").mockImplementation(async (id) => ({
    ask_id: id,
    rewrite: {
      at: "2026-10-08T00:04:00Z",
      by: "person",
      original: agentWording,
      state: "rewriting",
    },
    started: true,
  }));
  const { rerender, view } = renderCard(keyedCard(first));
  try {
    await card().findByRole("radio", { name: /^Ship/ });
    await user.click(screen.getByRole("button", { name: "More actions" }));
    await user.click(await screen.findByRole("menuitem", { name: "Rewrite" }));
    expect((await card().findByRole("status")).textContent).toBe("Rewriting…");

    rerender(keyedCard(next));
    expect(card(next.id).queryByRole("status")).not.toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "More actions" }));
    const rewrite = await screen.findByRole("menuitem", { name: "Rewrite" });
    expect(rewrite).not.toHaveAttribute("aria-disabled", "true");
    await user.click(rewrite);
    await waitFor(() => expect(rewriteAsk).toHaveBeenLastCalledWith(next.id, keyedAttempt));
    expect((await card(next.id).findByRole("status")).textContent).toBe("Rewriting…");
  } finally {
    view.unmount();
    rewriteAsk.mockRestore();
    getAsk.mockRestore();
  }
});

test("of two cards on a page, the one holding the focus has the keys, and the command bar lists them", async () => {
  const first = openAsk();
  const second = openAsk({ id: "ask-2", question: "Which region goes first?" });
  const { view } = renderCard(
    <KeymapProvider>
      <AskCard ask={first} initialThread={thread(first)} />
      <AskCard ask={second} initialThread={thread(second)} />
    </KeymapProvider>
  );
  try {
    await screen.findByTestId(`ask-${second.id}`);
    act(() => card(second.id).getByRole("radio", { name: /^Hold/ }).focus());
    // In each scope no two bindings of one key act at once: each card registers the same keys,
    // and only the card in hand's apply.
    expect(appKeymap.conflicts()).toEqual([]);
    // The command bar lists the keys that act with no text field in focus, for the card in hand.
    const rows = appKeymap
      .actions()
      .filter((action) => action.scope === "ask")
      .map((action) => action.label);
    expect(rows).toEqual(
      expect.arrayContaining([
        "Ask back",
        "Decline to answer",
        "Hand this question to…",
        "Pick option 1",
        "Pick option 2",
        "Rewrite",
      ])
    );
    // Only the options the card has, and no original to show on a text nobody rewrote.
    expect(rows).not.toContain("Pick option 3");
    expect(rows).not.toContain("Show or hide the original wording");
  } finally {
    view.unmount();
  }
});

// The card's send key (`$mod+Enter`) acts from anywhere in the card, and like Answer it waits for
// a picture pasted into the answer to finish uploading: an answer sent before the upload lands
// goes without the picture, whose link then lands in the draft of an ask already answered.
test("the card's send key sends nothing while a picture pasted into the answer uploads", async () => {
  const user = userEvent.setup();
  const sent: AnswerAskInput[] = [];
  const { promise: landed, resolve: land } = Promise.withResolvers<void>();
  const uploadArtifact = spyOn(api, "uploadArtifact").mockImplementation(async () => {
    await landed;
    return { artifact: { name: "shot.png", slug: "shot-png" }, version: { number: 1 } } as never;
  });
  const ask = openAsk();
  const { view } = renderCard(
    <KeymapProvider>
      <AskCard
        answerAsk={(_id, input) => {
          sent.push(input);
          // Never answered: the row reads what was sent, not what the card does after.
          return Promise.withResolvers<Ask>().promise;
        }}
        ask={ask}
        initialThread={thread(ask)}
      />
    </KeymapProvider>
  );
  try {
    const answer = (await card().findByLabelText("Your answer")) as HTMLTextAreaElement;
    // An answer this card sends: an option picked, and a note in the box.
    await user.click(await card().findByRole("radio", { name: /^Ship/ }));
    await user.click(answer);
    await user.paste("This one");
    fireEvent.paste(answer, {
      clipboardData: {
        files: [new File([new Uint8Array([137, 80, 78, 71])], "shot.png", { type: "image/png" })],
      },
    });
    await card().findByText("Uploading file…");
    // The box sends nothing itself while the picture uploads, and leaves the press to the card's
    // key, which must send nothing either.
    await user.keyboard("{Control>}{Enter}{/Control}");

    land();
    const withPicture = "This one ![shot.png](dispatch://CORE-1/artifact/shot-png@v1)";
    await waitFor(() => expect(answer.value).toBe(withPicture));
    // The same key sends it once the picture is in, pressed on the picked option, where the card's
    // key alone takes it. The row waits for that answer to arrive and then reads every answer
    // sent, rather than reading none a count of turns after the first press, which a send may
    // outlast: an answer the first press sent, without the picture's link, arrives first, or
    // keeps the form busy so that this press sends nothing.
    act(() => card().getByRole("radio", { name: /^Ship/ }).focus());
    await user.keyboard("{Control>}{Enter}{/Control}");
    await waitFor(() => expect(sent).not.toHaveLength(0));
    expect(sent.map((input) => input.text)).toEqual([withPicture]);
  } finally {
    view.unmount();
    uploadArtifact.mockRestore();
  }
});
