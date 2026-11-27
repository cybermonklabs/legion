import { expect, spyOn, test } from "bun:test";
import { QueryClient } from "@tanstack/react-query";
import { waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ReactNode } from "react";

import { precedes } from "../../__tests__/document-order";
import { whoAmIQuery } from "../../api/queries";
import type { Ask } from "../../api/types";
import { type PageWords, PageWordsProvider } from "../../components/tooltips";
import type { AgentNames } from "../conversation/CachedAuthor";
import { answeredAsk, renderCard } from "../inbox/ask-card-fixture";
import { useAskAnswerForm } from "../inbox/useAskAnswerForm";
import { AskBody } from "./AskBody";
import { type AskTextRenderer, markdownText } from "./ask-text";

// The ask body (dispatch://INFRA-5702/spec#b-r-ask-body, #b-r-ask-reads) drawn on its own, as a
// caller with no card around it draws it: with the form `useAskAnswerForm` answers, which reads no
// thread here (`readThread: false`), and every write of the form refused, so a case that sends one
// fails by it.

/** A write or a read the body has no business making: it fails, so a case that makes one fails. */
const refuse = (): Promise<never> => Promise.reject(new Error("the ask body sent a request"));

/** The asker: a person, whose name every surface draws as it is, from no read. */
const ASKER = { id: "carol", kind: "user" } as const;

/** An open question asked by `ASKER`, which waits on the reader. */
function openAsk(overrides: Partial<Ask> = {}): Ask {
  return answeredAsk({ answer: null, author: ASKER, state: "open", ...overrides });
}

/** An open approval request for version 12 of `spec.md`, after an approval of version 11. */
function approvalAsk(): Ask {
  return openAsk({
    approval: {
      approved: { at: "2026-10-05T14:30:00Z", by: { id: "alice", kind: "user" }, version: 11 },
      artifact_id: "artifact-1",
      documents: null,
      name: "spec.md",
      requested_version: 12,
      version: 12,
    },
    approval_artifact: { name: "spec.md", primary: true, project: "CORE", slug: "spec-md" },
    kind: "approval",
    options: [{ label: "Approve" }, { label: "Request changes" }],
    question: "Approve spec.md (version 12)?",
  });
}

/** A renderer that draws each text as it is, as a caller that hands its own renderer does. */
const plainText: AskTextRenderer = (text) => <p>{text.markdown}</p>;

function Body({
  agentNames = "live",
  ask,
  frame = "card",
  renderText = markdownText,
  urgencyWord = false,
}: {
  agentNames?: AgentNames;
  ask: Ask;
  frame?: "block" | "card";
  renderText?: AskTextRenderer;
  urgencyWord?: boolean;
}): ReactNode {
  const form = useAskAnswerForm({
    answer: refuse,
    ask,
    createReply: refuse,
    getAskThread: refuse,
    readThread: false,
  });
  return (
    <AskBody
      agentNames={agentNames}
      ask={ask}
      decline={refuse}
      documentLink
      form={form}
      frame={frame}
      renderText={renderText}
      urgencyWord={urgencyWord}
    />
  );
}

/** Draws `node` with the reader signed in as `alice`, a fact the shell's cache already holds, so
 *  nothing is read for it. */
function draw(node: ReactNode) {
  const queryClient = new QueryClient({
    defaultOptions: {
      mutations: { retry: false },
      queries: { retry: false, staleTime: Number.POSITIVE_INFINITY },
    },
  });
  queryClient.setQueryData(whoAmIQuery().queryKey, {
    event_head: "0",
    kind: "user",
    login: "alice",
  });
  const { view } = renderCard(node, queryClient);
  return view;
}

/** The ask body's root, the one element its mark names. */
function bodyOf(container: HTMLElement): HTMLElement {
  const bodies = container.querySelectorAll<HTMLElement>("[data-ask-body]");
  expect(bodies).toHaveLength(1);
  const body = bodies[0];
  if (body === undefined) throw new Error("no ask body was drawn");
  return body;
}

/** Each table of the issue page's list, its words being their keys: a surface with no tooltips,
 *  for the body to be drawn on, which the body reads no word of. */
function wordsByKey<Table>(): Table {
  return new Proxy({}, { get: (_table, key) => String(key) }) as Table;
}

const NO_TOOLTIPS: PageWords = {
  ANSWER_LINES: wordsByKey(),
  AUTHOR_KIND_WORDS: wordsByKey(),
  CONTROL_LABELS: wordsByKey(),
  PROPERTY_LABELS: wordsByKey(),
  STATE_LINES: wordsByKey(),
  authorText: { "data-author-text": "" },
  pageChrome: { "data-page-chrome": "" },
};

test("the question is drawn whole, with its picture", async () => {
  const view = draw(
    <Body
      ask={openAsk({
        question:
          "Which release path?\n\nThe map shows both.\n\n![Rollout map](dispatch://CORE-1/artifact/rollout-png@v1)",
      })}
    />
  );
  try {
    const body = within(bodyOf(view.container));
    expect(await body.findByText("Which release path?")).toBeTruthy();
    expect(body.getByText("The map shows both.")).toBeTruthy();
    expect(await body.findByRole("img", { name: "Rollout map" })).toBeTruthy();
  } finally {
    view.unmount();
  }
});

test("each option keeps its description, its role and the option mark", async () => {
  const single = draw(
    <Body
      ask={openAsk({ options: [{ description: "Release it", label: "Ship" }, { label: "Hold" }] })}
    />
  );
  try {
    const body = within(bodyOf(single.container));
    const ship = await body.findByRole("radio", { name: "Ship Release it" });
    expect(ship).toHaveAttribute("data-ask-option", "");
    expect(await body.findByRole("radio", { name: "Hold" })).toHaveAttribute("data-ask-option", "");
  } finally {
    single.unmount();
  }
  const multiple = draw(<Body ask={openAsk({ multiple: true })} />);
  try {
    const body = within(bodyOf(multiple.container));
    expect(await body.findByRole("checkbox", { name: "Ship" })).toHaveAttribute(
      "data-ask-option",
      ""
    );
  } finally {
    multiple.unmount();
  }
});

test("Other keeps its role, and carries no option mark and no description", async () => {
  const view = draw(<Body ask={openAsk()} />);
  try {
    const root = bodyOf(view.container);
    const other = await within(root).findByRole("radio", { name: "Other" });
    expect(other).not.toHaveAttribute("data-ask-option");
    expect(root.querySelectorAll("[data-ask-option]")).toHaveLength(2);
  } finally {
    view.unmount();
  }
});

test("the answer box stands in place under the options with Answer, and Save answer beside Cancel while an answer changes", async () => {
  const user = userEvent.setup();
  const open = draw(<Body ask={openAsk()} />);
  try {
    const body = within(bodyOf(open.container));
    const hold = await body.findByRole("radio", { name: "Hold" });
    const box = body.getByRole("textbox", { name: "Your answer" });
    expect(box).toHaveAttribute("data-ask-answer", "");
    const answer = body.getByRole("button", { name: "Answer" });
    expect(precedes(hold, box)).toBe(true);
    expect(precedes(box, answer)).toBe(true);
    expect(body.queryByRole("button", { name: "Add a note or answer in your own words" })).toBe(
      null
    );
  } finally {
    open.unmount();
  }

  const mine = draw(<Body ask={answeredAsk({ author: ASKER })} />);
  try {
    const body = within(bodyOf(mine.container));
    await user.click(await body.findByRole("button", { name: "Change answer" }));
    const box = within(bodyOf(mine.container)).getByRole("textbox", { name: "Your answer" });
    expect((box as HTMLTextAreaElement).value).toBe("First note");
    const save = within(bodyOf(mine.container)).getByRole("button", { name: "Save answer" });
    const cancel = within(bodyOf(mine.container)).getByRole("button", { name: "Cancel" });
    expect(save.parentElement).toBe(cancel.parentElement);
    expect(precedes(save, cancel)).toBe(true);
  } finally {
    mine.unmount();
  }
});

test("Ask back stands beside Answer, and a question-shaped answer offers Ask back instead and Answer with it anyway", async () => {
  const user = userEvent.setup();
  const view = draw(<Body ask={openAsk()} />);
  try {
    const body = within(bodyOf(view.container));
    expect(await body.findByRole("button", { name: "Ask back" })).toBeDisabled();
    await user.click(body.getByRole("textbox", { name: "Your answer" }));
    await user.paste("Can we wait for the freeze?");
    expect(body.getByRole("button", { name: "Ask back" })).toBeEnabled();
    await user.click(body.getByRole("button", { name: "Answer" }));
    const choice = await body.findByRole("group", { name: "Your answer reads as a question" });
    expect(within(choice).getByRole("button", { name: "Ask back instead" })).toBeTruthy();
    expect(within(choice).getByRole("button", { name: "Answer with it anyway" })).toBeTruthy();
  } finally {
    view.unmount();
  }
});

test("an approval request draws its mark, its document, Approve and Request changes, and the approval it follows with Show diff", async () => {
  const view = draw(<Body ask={approvalAsk()} />);
  try {
    const body = within(bodyOf(view.container));
    expect(body.getByText("Approval requested")).toBeTruthy();
    expect(body.getByRole("link", { name: /spec\.md/ })).toBeTruthy();
    expect(await body.findByRole("radio", { name: "Approve" })).toBeTruthy();
    expect(await body.findByRole("radio", { name: "Request changes" })).toBeTruthy();
    expect(body.queryByRole("radio", { name: "Other" })).toBe(null);
    const line = body.getByText(/^Approved at version 11 on/);
    expect(line.textContent).toBe("Approved at version 11 on 2026-10-05 by alice");
    expect(body.getByRole("button", { name: "Show diff" })).toBeTruthy();
    // An approval request is answered with Approve or Request changes, and has no Decline.
    expect(body.queryByRole("button", { name: "Decline to answer" })).toBe(null);
  } finally {
    view.unmount();
  }
});

test("an anchored ask draws its quote and its document, and a block ask its Open in document link", async () => {
  const anchored = draw(
    <Body
      ask={{
        ...openAsk({
          anchor: {
            artifact_id: "artifact-design",
            block_id: "section-1",
            mark_id: "mark-1",
            orphaned: false,
            quote: "The chosen passage.",
            version: 1,
          },
        }),
        anchor_artifact: {
          name: "Design notes",
          primary: false,
          project: "CORE",
          slug: "design-notes",
        },
      }}
    />
  );
  try {
    const body = within(bodyOf(anchored.container));
    expect(body.getByRole("link", { name: "Design notes" })).toHaveAttribute(
      "href",
      "/issues/CORE-1/artifacts/design-notes#b-section-1"
    );
    expect(body.getByText("The chosen passage.")).toBeTruthy();
  } finally {
    anchored.unmount();
  }
  const block = draw(
    <Body
      ask={openAsk({
        block_artifact: { id: "artifact-design", primary: false, slug: "design-notes" },
        block_id: "decision-1",
      })}
    />
  );
  try {
    expect(
      within(bodyOf(block.container)).getByRole("link", { name: "Open in document" })
    ).toHaveAttribute("href", "/issues/CORE-1/artifacts/design-notes#b-decision-1");
  } finally {
    block.unmount();
  }
});

test("an open question says whom it waits on, and draws Hand on and Decline", async () => {
  const mine = draw(<Body ask={openAsk()} />);
  try {
    const body = within(bodyOf(mine.container));
    expect((await body.findByTestId("turn-ask-1")).textContent).toBe("Waiting on you");
    expect(body.getByRole("combobox", { name: "Hand this question to" })).toBeTruthy();
    expect(body.getByRole("button", { name: "Decline to answer" })).toBeTruthy();
  } finally {
    mine.unmount();
  }
  const theirs = draw(<Body ask={openAsk({ waiting_on: "agent" })} />);
  try {
    const body = within(bodyOf(theirs.container));
    expect((await body.findByTestId("turn-ask-1")).textContent).toBe("Waiting on carol");
  } finally {
    theirs.unmount();
  }
});

test("a settled ask draws its record, with its badge, inside the element its test id names", async () => {
  const declined = draw(
    <Body
      ask={openAsk({
        resolution: {
          actor: { id: "alice", kind: "user" },
          at: "2026-10-08T03:00:00Z",
          kind: "declined",
          reason: "Not mine to answer.",
        },
        state: "resolved",
      })}
    />
  );
  try {
    const record = within(declined.container).getByTestId("ask-ask-1");
    expect(record.querySelector("[data-ask-body]")).not.toBe(null);
    expect(within(record).getByTestId("ask-resolution-badge").textContent).toBe("Declined");
    expect(within(record).queryByRole("textbox")).toBe(null);
  } finally {
    declined.unmount();
  }
  const answered = draw(<Body ask={answeredAsk({ author: ASKER })} />);
  try {
    const record = within(answered.container).getByTestId("ask-ask-1");
    expect(record.querySelector("[data-ask-body]")).not.toBe(null);
    expect(record.textContent).toContain("Answered by");
    expect(within(record).getByRole("button", { name: "Change answer" })).toBeTruthy();
  } finally {
    answered.unmount();
  }
});

test("the first line draws question, the author and the time at its right end", async () => {
  const card = draw(<Body ask={approvalAsk()} />);
  try {
    const body = bodyOf(card.container);
    const words = await within(body).findByText("Approve spec.md (version 12)?");
    const kind = within(body).getByText("question", { exact: true });
    const end = kind.parentElement;
    if (end === null) throw new Error("the kind stands in no line");
    // The words and the line's end share one row, the words first.
    expect(end.parentElement?.contains(words)).toBe(true);
    expect(precedes(words, end)).toBe(true);
    // At the end: the approval's mark beside the kind, then who asked, then when.
    const mark = within(end).getByText("Approval requested");
    const author = within(end).getByText("carol");
    const time = end.querySelector("time");
    if (time === null) throw new Error("the first line draws no time");
    expect(precedes(mark, kind)).toBe(true);
    expect(precedes(kind, author)).toBe(true);
    expect(precedes(author, time)).toBe(true);
    // The first time the body draws is the first line's.
    expect(body.querySelector("time")).toBe(time);
    expect(body.querySelector("[data-ask-bullet]")).not.toBe(null);
  } finally {
    card.unmount();
  }

  // In a document's ask block the block draws the question and the kind; the line keeps who
  // asked and when.
  const block = draw(<Body ask={openAsk()} frame="block" />);
  try {
    const body = within(bodyOf(block.container));
    await body.findByRole("radio", { name: "Ship" });
    expect(body.queryByText("Which release path?")).toBe(null);
    expect(body.queryByText("question", { exact: true })).toBe(null);
    expect(body.getByText("carol")).toBeTruthy();
    expect(bodyOf(block.container).querySelector("time")).not.toBe(null);
    expect(bodyOf(block.container).querySelector("[data-ask-bullet]")).toBe(null);
  } finally {
    block.unmount();
  }
});

test("inside NoTooltips no time carries a title", async () => {
  const outside = draw(<Body ask={approvalAsk()} />);
  try {
    const body = bodyOf(outside.container);
    await within(body).findByRole("radio", { name: "Approve" });
    const times = [...body.querySelectorAll("time")];
    // The first line's time, and the earlier approval's.
    expect(times).toHaveLength(2);
    for (const time of times) expect(time.hasAttribute("title")).toBe(true);
  } finally {
    outside.unmount();
  }
  const inside = draw(
    <PageWordsProvider words={NO_TOOLTIPS}>
      <Body ask={approvalAsk()} />
    </PageWordsProvider>
  );
  try {
    const body = bodyOf(inside.container);
    await within(body).findByRole("radio", { name: "Approve" });
    const times = [...body.querySelectorAll("time")];
    expect(times).toHaveLength(2);
    for (const time of times) expect(time.hasAttribute("title")).toBe(false);
  } finally {
    inside.unmount();
  }
});

test("the ask body sends no request", async () => {
  // A client that fails any call: every request the page makes goes through `fetch`.
  const request = spyOn(globalThis, "fetch").mockRejectedValue(
    new Error("the ask body sent a request")
  );
  const view = draw(
    <Body agentNames="cache" ask={approvalAsk()} renderText={plainText} urgencyWord />
  );
  try {
    const body = within(bodyOf(view.container));
    expect(await body.findByRole("radio", { name: "Approve" })).toBeTruthy();
    expect(body.getByRole("textbox", { name: "Reason" })).toBeTruthy();
    expect(body.getByRole("combobox", { name: "Hand this question to" })).toBeTruthy();
    // Every read the body could start has had its chance to.
    await waitFor(() => expect(body.getByText(/^Approved at version 11/)).toBeTruthy());
    const tick = Promise.withResolvers<void>();
    setTimeout(tick.resolve, 0);
    await tick.promise;
    expect(request).not.toHaveBeenCalled();
  } finally {
    view.unmount();
    request.mockRestore();
  }
});

test("the urgency word stands beside the kind where the caller sets it, and nowhere else", async () => {
  const set = draw(<Body ask={openAsk({ urgency: "high" })} urgencyWord />);
  try {
    const body = bodyOf(set.container);
    const kind = await within(body).findByText("question", { exact: true });
    const high = within(body).getByText("High", { exact: true });
    // The urgency word, then the mark between them, then the kind: side by side on one line.
    expect(high.parentElement).toBe(kind.parentElement);
    expect(high.nextElementSibling?.nextElementSibling).toBe(kind);
  } finally {
    set.unmount();
  }
  const unset = draw(<Body ask={openAsk({ urgency: "high" })} />);
  try {
    const body = bodyOf(unset.container);
    await within(body).findByText("question", { exact: true });
    expect(within(body).queryByText("High", { exact: true })).toBe(null);
  } finally {
    unset.unmount();
  }
  // An ask of urgency med or low carries no word, wherever it is set.
  const med = draw(<Body ask={openAsk({ urgency: "med" })} urgencyWord />);
  try {
    const body = bodyOf(med.container);
    await within(body).findByText("question", { exact: true });
    expect(within(body).queryByText("Medium", { exact: true })).toBe(null);
  } finally {
    med.unmount();
  }
});
