import { beforeAll, expect, spyOn, test } from "bun:test";
import {
  DISPATCH_WRITING_BOXES,
  type DispatchWritingBoxRow,
  dispatchWritingBox,
} from "@legion/contracts";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
// biome-ignore lint/style/noRestrictedImports: the keys of an IME composition, which user-event has no form for
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { type ReactNode, useState } from "react";
import { MemoryRouter } from "react-router-dom";

import { installResizeObserver } from "../../__tests__/scroll-box";
import {
  architectureTree,
  issueCollaborators,
  plannerAgent,
  searchResponse,
} from "../../__tests__/search-fixture";
import { api } from "../../api/client";
import { warmMarkdownRenderer } from "../refs/markdown-engine";
import { MentionField } from "./MentionField";

// The rows draw through the shared renderer, whose Markdown engine loads lazily, and they run in
// random order, so the file loads the engine before its first row, whichever row that is.
beforeAll(async () => {
  await warmMarkdownRenderer();
});

/** Every key the field's own listener may take, and what the host saw of it. */
interface HostKeys {
  readonly fromField: string[];
  readonly fromWindow: string[];
}

function Host({ keys, writing }: { keys: HostKeys; writing: DispatchWritingBoxRow }): ReactNode {
  const [body, setBody] = useState("");
  return (
    <MentionField
      body={body}
      issueKey="CORE-1"
      label="Comment"
      onChange={(change) => setBody(change.body)}
      onKeyDown={(event) => keys.fromField.push(event.key)}
      writing={writing}
    />
  );
}

function renderField(writing = dispatchWritingBox("conversation-composer", "comment")) {
  const listAgents = spyOn(api, "listAgents").mockResolvedValue([plannerAgent()]);
  const getIssueCollaborators = spyOn(api, "getIssueCollaborators").mockResolvedValue(
    // The issue has a person on it, whom the picker's People rows are read from.
    issueCollaborators({
      people: [{ kind: "user", last_at: null, login: "alice@example.com", reasons: ["assignee"] }],
    })
  );
  const getArchitecture = spyOn(api, "getArchitecture").mockResolvedValue(architectureTree());
  const search = spyOn(api, "search").mockResolvedValue(searchResponse([]));
  const keys: HostKeys = { fromField: [], fromWindow: [] };
  const windowKeys = (event: KeyboardEvent) => keys.fromWindow.push(event.key);
  window.addEventListener("keydown", windowKeys);
  const view = render(
    <MemoryRouter future={{ v7_relativeSplatPath: true, v7_startTransition: true }}>
      <QueryClientProvider
        client={
          new QueryClient({
            defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
          })
        }
      >
        <Host keys={keys} writing={writing} />
      </QueryClientProvider>
    </MemoryRouter>
  );
  return {
    keys,
    restore: () => {
      window.removeEventListener("keydown", windowKeys);
      search.mockRestore();
      getArchitecture.mockRestore();
      getIssueCollaborators.mockRestore();
      listAgents.mockRestore();
      view.unmount();
    },
  };
}

/** The planner session's row: the session's name is drawn in its role's row too, as the role's
 *  holder, so the row is found by its accessible name, which leads with the session's. */
const PLANNER_ROW = { name: /^Planner\b/ } as const;

// The keys an open picker owns never reach the host, whatever the host binds them to: the
// Inbox's hotkeys are on the window, an ask option's Enter is on the box itself.
test("an open picker takes the arrows, Enter, Tab and Escape ahead of the host", async () => {
  const user = userEvent.setup();
  const { keys, restore } = renderField();

  try {
    const field = screen.getByRole("textbox", { name: "Comment" });
    await user.click(field);
    await user.paste("@");
    await screen.findByRole("option", PLANNER_ROW);
    keys.fromField.length = 0;
    keys.fromWindow.length = 0;
    await user.keyboard("{ArrowDown}");
    await user.keyboard("{ArrowUp}");
    await user.keyboard("{Escape}");
    expect(keys.fromField).toEqual([]);
    expect(keys.fromWindow).toEqual([]);
  } finally {
    restore();
  }
});

// A key of an input method's composition is the input method's, open picker or not: the Enter
// that confirms a word takes no row, and the Escape that cancels it closes no list. Safari ends
// the composition before the Enter that confirms it, which carries the IME's key code 229 alone.
test("an open picker leaves a composition's Enter and Escape to the host", async () => {
  const user = userEvent.setup();
  const { keys, restore } = renderField();

  try {
    const field = screen.getByRole<HTMLTextAreaElement>("textbox", { name: "Comment" });
    await user.click(field);
    await user.paste("@");
    await screen.findByRole("option", PLANNER_ROW);
    keys.fromField.length = 0;
    keys.fromWindow.length = 0;
    // user-event has no form for this: it cannot mark a key as an IME composition.
    fireEvent.keyDown(field, { isComposing: true, key: "Enter" });
    fireEvent.keyDown(field, { isComposing: true, key: "Escape" });
    fireEvent.keyDown(field, { isComposing: false, key: "Enter", keyCode: 229 });
    expect(field.value).toBe("@");
    expect(screen.getByRole("option", PLANNER_ROW)).toBeInTheDocument();
    expect(keys.fromField).toEqual(["Enter", "Escape", "Enter"]);
    expect(keys.fromWindow).toEqual(["Enter", "Escape", "Enter"]);
  } finally {
    restore();
  }
});

test("a closed picker leaves every key to the host", async () => {
  const user = userEvent.setup();
  const { keys, restore } = renderField();

  try {
    const field = screen.getByRole("textbox", { name: "Comment" });
    await user.click(field);
    await user.paste("hello");
    keys.fromField.length = 0;
    keys.fromWindow.length = 0;
    await user.keyboard("{ArrowDown}{ArrowUp}{Enter}{Escape}");
    expect(keys.fromField).toEqual(["ArrowDown", "ArrowUp", "Enter", "Escape"]);
    expect(keys.fromWindow).toEqual(["ArrowDown", "ArrowUp", "Enter", "Escape"]);
  } finally {
    restore();
  }
});

// Enter takes the row the arrows are on, and the pick replaces the trigger alone: the text
// around the caret is the reader's and stays.
test("Enter takes the highlighted row into the draft at the caret", async () => {
  const user = userEvent.setup();
  const { restore } = renderField();

  try {
    const field = screen.getByRole<HTMLTextAreaElement>("textbox", { name: "Comment" });
    await user.click(field);
    await user.paste("ask @");
    await screen.findByRole("option", PLANNER_ROW);
    await user.keyboard("{ArrowDown}{Enter}");
    await waitFor(() =>
      expect(field.value).toBe("ask [@Planner](dispatch://agent/planner-session)")
    );
  } finally {
    restore();
  }
});

// A box in the middle of a sentence: the pick lands where the trigger was and leaves the rest.
test("a pick replaces the trigger and leaves the text after the caret", async () => {
  const user = userEvent.setup();
  const { restore } = renderField(dispatchWritingBox("decisions-change", "comment"));

  try {
    const field = screen.getByRole<HTMLTextAreaElement>("textbox", { name: "Comment" });
    await user.click(field);
    await user.paste("tail");
    field.setSelectionRange(0, 0);
    await user.paste("@");
    await screen.findByRole("option", PLANNER_ROW);
    await user.keyboard("{ArrowDown}{Enter}");
    await waitFor(() =>
      expect(field.value).toBe("[@Planner](dispatch://agent/planner-session)tail")
    );
  } finally {
    restore();
  }
});

// A door never opens in a written link's words: a pick there would write a link inside a link.
// The caret moved back into a mention's words opens no list, and Enter keeps its ordinary
// meaning: the host is handed it, and the box types its line break.
test("the caret inside a written mention opens no list, and Enter is the host's", async () => {
  const user = userEvent.setup();
  const { keys, restore } = renderField();
  const mention = "[@Planner](dispatch://agent/planner-session)";

  try {
    const field = screen.getByRole<HTMLTextAreaElement>("textbox", { name: "Comment" });
    await user.click(field);
    await user.paste(mention);
    // Back through the address and into the words: `[@Plan|ner](…)`.
    await user.keyboard("{ArrowLeft}".repeat(mention.length - "[@Plan".length));
    expect(field.selectionStart).toBe("[@Plan".length);
    expect(screen.queryByTestId("ref-picker")).not.toBeInTheDocument();

    keys.fromField.length = 0;
    await user.keyboard("{Enter}");
    expect(keys.fromField).toEqual(["Enter"]);
    await waitFor(() => expect(field.value).toBe("[@Plan\nner](dispatch://agent/planner-session)"));
  } finally {
    restore();
  }
});

// A door opens in a link's address on a trigger typed there, and its pick writes the address
// alone: the link around it already has its words, so a party is no mention there.
test("a door opens in a link's address, and a party picked there is its bare address", async () => {
  const user = userEvent.setup();
  const { restore } = renderField();

  try {
    const field = screen.getByRole<HTMLTextAreaElement>("textbox", { name: "Comment" });
    await user.click(field);
    await user.paste("[the plan](@");
    await screen.findByRole("option", PLANNER_ROW);
    await user.keyboard("{ArrowDown}{Enter}");
    await waitFor(() => expect(field.value).toBe("[the plan](dispatch://agent/planner-session"));
  } finally {
    restore();
  }
});

// R9: the words under a box are its row's, so the list and the box cannot drift apart.
test("the box shows the hint of the row it was mounted with", async () => {
  for (const row of DISPATCH_WRITING_BOXES.filter((one) => one.covered)) {
    const { restore } = renderField(row);
    try {
      expect(screen.getByText(row.hint)).toBeTruthy();
    } finally {
      restore();
    }
  }
});

// An inline box is as tall as the text it holds. A width it changes to - the window resized, or a
// composer's field that takes the whole line as it takes the focus below 761 px - wraps the text
// anew, and the box is made as tall as its text again, so no line of it is scrolled out of the
// box. The height the box sets itself changes its size too, and at the same width that sets
// nothing more. happy-dom lays nothing out, so the row says how wide the box is and how tall its
// text is, and tells the box's observer when that changed.
test("an inline box whose width changes is made as tall as its text again, and its own height sets nothing more", () => {
  const sizes = installResizeObserver();
  const view = render(
    <MemoryRouter future={{ v7_relativeSplatPath: true, v7_startTransition: true }}>
      <QueryClientProvider client={new QueryClient()}>
        <MentionField
          body="A reply long enough to wrap in a narrow box"
          inline
          issueKey="CORE-1"
          label="Reply"
          onChange={() => undefined}
          writing={dispatchWritingBox("conversation-composer", "comment")}
        />
      </QueryClientProvider>
    </MemoryRouter>
  );
  try {
    const box = screen.getByRole("textbox", { name: "Reply" });
    const laidOut = { text: 42, width: 600 };
    // The box's text and padding (`scrollHeight`) at its width, inside a 1 px border above and
    // below: drawn 2 px taller than the area inside it.
    for (const [name, read] of [
      ["clientWidth", () => laidOut.width],
      ["scrollHeight", () => laidOut.text],
      ["offsetHeight", () => 44],
      ["clientHeight", () => 42],
    ] as const) {
      Object.defineProperty(box, name, { configurable: true, get: read });
    }
    sizes.resize(box);
    expect(box.style.height, "the box laid out 600 px wide").toBe("44px");

    Object.assign(laidOut, { text: 90, width: 300 });
    sizes.resize(box);
    expect(box.style.height, "the box the window narrowed to 300 px").toBe("92px");

    // Measured again, the box would be 116 px tall: at the same width it is not measured.
    laidOut.text = 114;
    sizes.resize(box);
    expect(box.style.height, "the box's own height change, at the same width").toBe("92px");
  } finally {
    view.unmount();
    sizes.restore();
  }
});

/** A reply's host as a composer is one: it holds the box's text, and its Send clears it, a change
 *  of the host's own that is no edit of the box. */
function ReplyHost(): ReactNode {
  const [body, setBody] = useState("");
  return (
    <>
      <MentionField
        body={body}
        inline
        issueKey="CORE-1"
        label="Reply"
        onChange={(change) => setBody(change.body)}
        writing={dispatchWritingBox("conversation-composer", "comment")}
      />
      <button onClick={() => setBody("")} type="button">
        Send
      </button>
    </>
  );
}

// An inline box is as tall as the text it holds, whoever changed that text: the reader's typing,
// and the host's own change, which fires no edit of the box - the text cleared after a send, a
// pick put in. Cleared, the box is one line tall again. happy-dom lays nothing out, so the row
// says how tall the box's text is.
test("an inline box whose text the host clears after a send is one line tall again", async () => {
  const user = userEvent.setup();
  const view = render(
    <MemoryRouter future={{ v7_relativeSplatPath: true, v7_startTransition: true }}>
      <QueryClientProvider client={new QueryClient()}>
        <ReplyHost />
      </QueryClientProvider>
    </MemoryRouter>
  );
  try {
    const box = screen.getByRole("textbox", { name: "Reply" }) as HTMLTextAreaElement;
    // The text and its padding (`scrollHeight`): one line while the box is empty, three once the
    // reader has written, inside a 1 px border above and below.
    for (const [name, read] of [
      ["scrollHeight", () => (box.value === "" ? 40 : 88)],
      ["offsetHeight", () => 44],
      ["clientHeight", () => 42],
    ] as const) {
      Object.defineProperty(box, name, { configurable: true, get: read });
    }
    await user.click(box);
    await user.keyboard("A reply the box wraps on three lines");
    expect(box.style.height, "the box the reader wrote three lines in").toBe("90px");

    await user.click(screen.getByRole("button", { name: "Send" }));
    expect(box.value).toBe("");
    expect(box.style.height, "the box its host cleared after the send").toBe("42px");
  } finally {
    view.unmount();
  }
});
