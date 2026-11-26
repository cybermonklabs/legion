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

// A reader who types straight after a pick, before the browser draws its next frame, keeps every
// key where it was typed: the field's caret after the pick lands with the pick's own text, and
// nothing set later moves a caret the reader has already moved on. Here the frame the browser
// would draw after the pick is held until two keys are typed, as a loaded machine holds it (INFRA-
// 1109's references lane, run 45850342764, writing-surfaces.e2e.ts:665 read "...)hich window does
// it replay? w").
test("keys typed after a pick and before the next frame stay in the order they were typed", async () => {
  const user = userEvent.setup();
  const { restore } = renderField();
  const held: FrameRequestCallback[] = [];
  const frames = spyOn(window, "requestAnimationFrame").mockImplementation((callback) => {
    held.push(callback);
    return held.length;
  });

  try {
    const field = screen.getByRole<HTMLTextAreaElement>("textbox", { name: "Comment" });
    await user.click(field);
    await user.paste("ask @");
    await screen.findByRole("option", PLANNER_ROW);
    await user.keyboard("{ArrowDown}{Enter}");
    await waitFor(() =>
      expect(field.value).toBe("ask [@Planner](dispatch://agent/planner-session)")
    );
    await user.keyboard("{End} w");
    frames.mockRestore();
    for (const callback of held.splice(0)) callback(performance.now());
    await user.keyboard("hich");
    await waitFor(() =>
      expect(field.value).toBe("ask [@Planner](dispatch://agent/planner-session) which")
    );
  } finally {
    frames.mockRestore();
    restore();
  }
});

// A reader who picks a mention in the middle of a sentence and does not act before the next frame
// gets the caret right after the pick, not at the end of the field where updating the textarea's
// value leaves it.
test("without a reader act before the next frame, a pick places the caret right after the mention", async () => {
  const user = userEvent.setup();
  const { restore } = renderField();
  const held: FrameRequestCallback[] = [];
  const frames = spyOn(window, "requestAnimationFrame").mockImplementation((callback) => {
    held.push(callback);
    return held.length;
  });
  const afterPick = "ask [@Planner](dispatch://agent/planner-session)".length;

  try {
    const field = screen.getByRole<HTMLTextAreaElement>("textbox", { name: "Comment" });
    await user.click(field);
    await user.paste(" please");
    field.setSelectionRange(0, 0);
    await user.paste("ask @");
    await screen.findByRole("option", PLANNER_ROW);
    await user.keyboard("{ArrowDown}{Enter}");
    await waitFor(() =>
      expect(field.value).toBe("ask [@Planner](dispatch://agent/planner-session) please")
    );
    frames.mockRestore();
    for (const callback of held.splice(0)) callback(performance.now());
    expect(document.activeElement).toBe(field);
    expect(field.selectionStart).toBe(afterPick);
    expect(field.selectionEnd).toBe(afterPick);
  } finally {
    frames.mockRestore();
    restore();
  }
});
