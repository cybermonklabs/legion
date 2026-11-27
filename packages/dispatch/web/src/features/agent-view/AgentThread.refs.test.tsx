import { afterEach, beforeAll, expect, type Mock, spyOn } from "bun:test";
import {
  AssistantRuntimeProvider,
  type ThreadMessageLike,
  useExternalStoreRuntime,
} from "@assistant-ui/react";
import { type AgentStreamCommand, dispatchWritingBox } from "@legion/contracts";
// biome-ignore lint/style/noRestrictedImports: the keys of an IME composition, and Safari's key code 229, which user-event has no form for
import { cleanup, fireEvent, screen, waitFor, within } from "@testing-library/react";
import userEvent, { type UserEvent } from "@testing-library/user-event";
import type { ReactNode } from "react";
import { MemoryRouter } from "react-router-dom";

import { graphNode } from "../../__tests__/graph-fixture";
import { renderWithQueries as render } from "../../__tests__/query-provider";
import { plannerAgent, searchResponse } from "../../__tests__/search-fixture";
import { api } from "../../api/client";
import type { ArtifactWithTrees, GraphReferences, SearchResult } from "../../api/types";
import { NO_SESSION_SENDS } from "../conversation/useSessionSends";
import { warmMarkdownRenderer } from "../refs/markdown-engine";
import { REF_PICKER_INSTRUCTION, REF_PICKER_PARTS_WORDS } from "../refs/ref-picker";
import { settleRefPickerSearch, refPickerTest as test } from "../refs/ref-picker-fixture";
import { AgentThread } from "./AgentThread";

// The rows draw through the shared renderer, whose Markdown engine loads lazily, and they run in
// random order, so the file loads the engine before its first row, whichever row that is.
beforeAll(async () => {
  await warmMarkdownRenderer();
});

/**
 * The reference picker in an agent's live conversation: the three doors of every other box over
 * every kind it reaches, beside the `/` that lists the session's own commands, in assistant-ui's
 * composer.
 */

// The session this conversation is with: it advertises no delivery mode, which is what a live
// conversation's own box needs none of.
const planner = plannerAgent({ capabilities: [] });

const COMMANDS: AgentStreamCommand[] = [
  { description: "Compact the session's context", name: "compact", source: "builtin" },
];

/** What the search answers for `relay`: an issue, and a message of it, which has an address of
 *  its own like any node of an issue. */
const hitRows: SearchResult[] = [
  {
    href: "/issues/CORE-2",
    id: "CORE-2",
    kind: "issue",
    owner: { key: "CORE-2", kind: "issue", status: "open", title: "Relay drops a reply" },
    rank: 0.1,
    snippet: "The relay drops a reply",
  },
  {
    href: "/issues/CORE-2/messages/message-1",
    id: "message-1",
    kind: "message",
    owner: { key: "CORE-2", kind: "issue", status: "open", title: "Relay drops a reply" },
    rank: 0.09,
    snippet: "I told the <mark>relay</mark> to retry",
  },
];

/** The message's one block, as its `part_of` backlinks answer it. */
const messageBlocks: GraphReferences = {
  edges: [
    {
      created_at: "2026-11-01T00:00:00Z",
      direction: "in",
      excerpt: { text: "Retry the relay once" },
      kind: "part_of",
      node: graphNode({ block_id: "block-1", id: "message-1", kind: "block" }),
      source_seq: null,
    },
  ],
  node: graphNode({ id: "message-1", kind: "message" }),
};

/** The one file the planner's conversation holds. */
const shot: ArtifactWithTrees = {
  created_at: "2026-11-01T00:00:00Z",
  created_by: { id: "alice@example.com", kind: "user" },
  id: "artifact-shot",
  issue_key: null,
  kind: "image",
  name: "shot.png",
  primary: false,
  project: "",
  ref_key: "agent/planner-session/shot-png",
  session_id: "planner-session",
  slug: "shot-png",
  versions: [],
};

interface Pane {
  /** Every message the composer sent, in press order. */
  readonly sent: string[];
  readonly search: Mock<typeof api.search>;
  readonly getReferences: Mock<typeof api.getReferences>;
  readonly listAgentArtifacts: Mock<typeof api.listAgentArtifacts>;
  readonly restore: () => void;
}

let pane: Pane | undefined;

afterEach(() => {
  cleanup();
  pane?.restore();
  pane = undefined;
});

function Harness({
  commands,
  onSend,
}: {
  commands?: AgentStreamCommand[];
  onSend: (body: string) => void;
}): ReactNode {
  const runtime = useExternalStoreRuntime({
    convertMessage: (message: ThreadMessageLike) => message,
    isRunning: false,
    messages: [],
    onNew: async (message) => {
      const [part] = message.content;
      onSend(part?.type === "text" ? part.text : "");
    },
  });
  return (
    <AssistantRuntimeProvider runtime={runtime}>
      <AgentThread
        commands={commands}
        contentIds={[]}
        empty="Nothing yet."
        front
        held={false}
        initialPlace={null}
        notice={undefined}
        onPlace={() => {}}
        ownSendCount={0}
        placeholder="Message the session…"
        replayCount={0}
        resetKey="session-1"
        sends={NO_SESSION_SENDS}
      />
    </AssistantRuntimeProvider>
  );
}

function renderThread(commands?: AgentStreamCommand[]): HTMLTextAreaElement {
  const listAgents = spyOn(api, "listAgents").mockResolvedValue([planner]);
  const listUsers = spyOn(api, "listUsers").mockResolvedValue([{ login: "alice@example.com" }]);
  const search = spyOn(api, "search").mockImplementation(async (query: string) =>
    searchResponse(
      hitRows.filter((hit) => hit.snippet.toLowerCase().includes(query.toLowerCase())),
      { took_ms: 2 }
    )
  );
  const getReferences = spyOn(api, "getReferences").mockResolvedValue(messageBlocks);
  const listAgentArtifacts = spyOn(api, "listAgentArtifacts").mockResolvedValue([shot]);
  const sent: string[] = [];
  render(
    <MemoryRouter>
      <Harness commands={commands} onSend={(body) => sent.push(body)} />
    </MemoryRouter>
  );
  pane = {
    getReferences,
    listAgentArtifacts,
    restore: () => {
      listAgentArtifacts.mockRestore();
      getReferences.mockRestore();
      search.mockRestore();
      listUsers.mockRestore();
      listAgents.mockRestore();
    },
    search,
    sent,
  };
  return within(screen.getByTestId("agent-composer")).getByRole("textbox") as HTMLTextAreaElement;
}

/** Writes `value` into the composer as one edit, as the slash-command rows do: a clear before
 *  the paste would be a delete the trigger popover follows and a list it closed. */
async function type(user: UserEvent, input: HTMLTextAreaElement, value: string): Promise<void> {
  await user.click(input);
  await user.keyboard("{Control>}a{/Control}");
  await user.paste(value);
}

/** Lets the search debounce's length pass inside act after a key that should do nothing: long
 *  enough for any read the key started to answer and its rows to draw, so a check made after it
 *  would see them, where a check made at once would find nothing whether the key acted or not. */
async function afterTheKey(): Promise<void> {
  await settleRefPickerSearch();
}

/** The picker's rows in the order they are drawn, each as `<heading>: <words>`: a row's
 *  heading is the nearest element before it that is not a row. */
function drawnRows(): string[] {
  const list = screen.getByRole("listbox", { name: "References" });
  return within(list)
    .getAllByRole("option")
    .map((option) => {
      let heading = "";
      for (let node = option.previousElementSibling; node !== null; ) {
        if (node.getAttribute("role") === "option") {
          node = node.previousElementSibling;
          continue;
        }
        heading = node.textContent ?? "";
        break;
      }
      return `${heading}: ${option.querySelector("span span")?.textContent ?? ""}`;
    });
}

test("each of the three doors opens the picker over the live composer", async () => {
  const user = userEvent.setup();
  const input = renderThread();

  await type(user, input, "@");
  await waitFor(() =>
    expect(drawnRows()).toEqual(["People: alice@example.com", "Agents: Planner", "Roles: planner"])
  );

  // `#` and `[[` reach the same rows; the searched kinds need two characters typed.
  await type(user, input, "#relay");
  await waitFor(() =>
    expect(drawnRows()).toEqual([
      "Issues: Relay drops a reply",
      "Messages: I told the relay to retry",
    ])
  );

  await type(user, input, "[[relay");
  await waitFor(() =>
    expect(drawnRows()).toEqual([
      "Issues: Relay drops a reply",
      "Messages: I told the relay to retry",
    ])
  );
});

test("this box offers an issue's message as every box does, and asks the search for every kind", async () => {
  const user = userEvent.setup();
  const input = renderThread();

  await type(user, input, "#relay");
  await waitFor(() => expect(pane?.search).toHaveBeenCalledTimes(1));
  // The picker's message is a message of an issue, which has an address like any of the issue's
  // nodes: the route is asked for it as for every other searched kind.
  expect(pane?.search.mock.calls[0]).toEqual([
    "relay",
    { kind: ["issue", "document", "ask", "comment", "message"], limit: 8 },
  ]);
  await waitFor(() => expect(drawnRows()).toContain("Messages: I told the relay to retry"));
  await user.click(screen.getByRole("option", { name: /^I told the relay to retry/ }));
  await waitFor(() => expect(input.value).toBe("dispatch://CORE-2/message/message-1 "));
  // The instruction names what the next two letters will add, messages among them.
  await type(user, input, "@");
  await waitFor(() =>
    expect(screen.getByTestId("ref-picker-instruction").textContent).toBe(REF_PICKER_INSTRUCTION)
  );
  expect(REF_PICKER_INSTRUCTION).toContain("messages");
});

test("a row's Blocks control lists its blocks, and a block pick is its address", async () => {
  const user = userEvent.setup();
  const input = renderThread();

  await type(user, input, "#relay");
  await waitFor(() => expect(drawnRows()).toContain("Messages: I told the relay to retry"));
  // An issue holds no blocks of its own, so the message's row alone offers them.
  expect(screen.getAllByRole("button", { name: "Blocks" })).toHaveLength(1);

  await user.click(screen.getByRole("button", { name: "Blocks" }));

  await waitFor(() =>
    expect(pane?.getReferences).toHaveBeenCalledWith("dispatch://CORE-2/message/message-1", {
      kind: "part_of",
    })
  );
  await waitFor(() => expect(drawnRows()).toContain("Message blocks: Retry the relay once"));
  // Opening the parts is not a pick: the trigger the reader typed is still there.
  expect(input.value).toBe("#relay");
  await user.click(screen.getByRole("option", { name: /^Retry the relay once/ }));
  await waitFor(() => expect(input.value).toBe("dispatch://CORE-2/message/message-1#b-block-1 "));
  expect(pane?.sent).toEqual([]);
});

test("ArrowRight opens the highlighted row's parts, and nothing on a row that holds none", async () => {
  const user = userEvent.setup();
  const input = renderThread();

  await type(user, input, "#relay");
  await waitFor(() =>
    expect(drawnRows()).toEqual([
      "Issues: Relay drops a reply",
      "Messages: I told the relay to retry",
    ])
  );
  // The issue is highlighted and holds no blocks: ArrowRight opens nothing.
  await user.keyboard("{ArrowRight}");
  expect(pane?.getReferences).not.toHaveBeenCalled();

  await user.keyboard("{ArrowDown}");
  await waitFor(() =>
    expect(screen.getByRole("option", { selected: true })).toHaveTextContent(
      "I told the relay to retry"
    )
  );
  await user.keyboard("{ArrowRight}");
  await waitFor(() =>
    expect(pane?.getReferences).toHaveBeenCalledWith("dispatch://CORE-2/message/message-1", {
      kind: "part_of",
    })
  );
  await waitFor(() => expect(drawnRows()).toContain("Message blocks: Retry the relay once"));
  await user.keyboard("{ArrowDown}{ArrowDown}");
  await waitFor(() =>
    expect(screen.getByRole("option", { selected: true })).toHaveTextContent("Retry the relay once")
  );
  await user.keyboard("{Enter}");
  await waitFor(() => expect(input.value).toBe("dispatch://CORE-2/message/message-1#b-block-1 "));
  expect(pane?.sent).toEqual([]);
});

// An input method's arrows move between the segments of the word it is composing, so an
// ArrowRight of a composition opens no parts, on a row that holds some.
test("ArrowRight in the middle of a composition opens no parts", async () => {
  const user = userEvent.setup();
  const input = renderThread();

  await type(user, input, "#relay");
  await waitFor(() =>
    expect(drawnRows()).toEqual([
      "Issues: Relay drops a reply",
      "Messages: I told the relay to retry",
    ])
  );
  await user.keyboard("{ArrowDown}");
  await waitFor(() =>
    expect(screen.getByRole("option", { selected: true })).toHaveTextContent(
      "I told the relay to retry"
    )
  );
  // user-event has no form for this: it cannot mark a key as an IME composition.
  fireEvent.keyDown(input, { isComposing: true, key: "ArrowRight" });
  await afterTheKey();
  expect(drawnRows().filter((row) => row.startsWith("Message blocks:"))).toEqual([]);
  expect(pane?.getReferences).not.toHaveBeenCalled();
});

// Safari ends the composition before the Enter that confirms it, so that Enter carries only the
// IME's key code 229, and assistant-ui, which skips a key marked composing, would take the row.
test("Safari's Enter that confirms a composition takes no row, where a plain Enter does", async () => {
  const user = userEvent.setup();
  const input = renderThread();

  await type(user, input, "#relay");
  await waitFor(() =>
    expect(drawnRows()).toEqual([
      "Issues: Relay drops a reply",
      "Messages: I told the relay to retry",
    ])
  );
  // user-event has no form for this: it cannot mark a key as an IME composition.
  fireEvent.keyDown(input, { isComposing: false, key: "Enter", keyCode: 229 });
  await afterTheKey();
  expect(input.value).toBe("#relay");
  expect(pane?.sent).toEqual([]);

  // The same Enter with no IME code is the list's: it takes the highlighted issue.
  fireEvent.keyDown(input, { key: "Enter" });
  await waitFor(() => expect(input.value).toBe("dispatch://CORE-2 "));
  expect(pane?.sent).toEqual([]);
});

test("an agent's row opens its conversation's documents, and a pick is the document's address", async () => {
  const user = userEvent.setup();
  const input = renderThread();

  await type(user, input, "@plan");
  await waitFor(() => expect(drawnRows()).toEqual(["Agents: Planner", "Roles: planner"]));
  await user.click(screen.getByRole("button", { name: "Documents" }));

  await waitFor(() => expect(pane?.listAgentArtifacts).toHaveBeenCalledWith("planner-session"));
  await waitFor(() =>
    expect(drawnRows()).toEqual(["Agents: Planner", "Roles: planner", "Documents: shot.png"])
  );
  await user.click(screen.getByRole("option", { name: /^shot\.png/ }));
  await waitFor(() =>
    expect(input.value).toBe("dispatch://agent/planner-session/artifact/shot-png ")
  );
});

// A session's row carries the session's full id as its tooltip and accessible description, as in
// every other box, since a session's name can be a short form two sessions share; a role's row
// is named by its role, and carries none.
test("a session's row has its full id as its title and description, and a role's row has none", async () => {
  const user = userEvent.setup();
  const input = renderThread();

  await type(user, input, "@plan");
  await waitFor(() => expect(drawnRows()).toEqual(["Agents: Planner", "Roles: planner"]));
  const list = screen.getByRole("listbox", { name: "References" });
  const session = within(list).getByRole("option", { name: /^Planner/ });
  expect(session).toHaveAccessibleDescription("planner-session");
  expect(session.title).toBe("planner-session");
  const role = within(list).getByRole("option", { name: /^planner/ });
  expect(role).not.toHaveAccessibleDescription();
  expect(role.hasAttribute("title")).toBe(false);
});

// ArrowLeft closes what ArrowRight opened, as it does in every other box: the key is the list's
// while a row's parts are open, so the caret stays where the reader left it.
test("ArrowLeft closes the parts ArrowRight opened, and leaves the caret where it was", async () => {
  const user = userEvent.setup();
  const input = renderThread();

  await type(user, input, "@plan");
  await waitFor(() => expect(drawnRows()).toEqual(["Agents: Planner", "Roles: planner"]));
  await user.keyboard("{ArrowRight}");
  await waitFor(() =>
    expect(drawnRows()).toEqual(["Agents: Planner", "Roles: planner", "Documents: shot.png"])
  );

  await user.keyboard("{ArrowLeft}");
  await waitFor(() => expect(drawnRows()).toEqual(["Agents: Planner", "Roles: planner"]));
  expect(input.selectionStart).toBe("@plan".length);
});

// An agent's row offers its documents before any read says whether it holds one, so an opened
// row that lists none says so here as in every other box.
test("an agent whose conversation holds no file says so when its documents are opened", async () => {
  const user = userEvent.setup();
  const input = renderThread();
  pane?.listAgentArtifacts.mockResolvedValue([]);

  await type(user, input, "@plan");
  await waitFor(() => expect(drawnRows()).toEqual(["Agents: Planner", "Roles: planner"]));
  await user.click(screen.getByRole("button", { name: "Documents" }));

  await waitFor(() => expect(pane?.listAgentArtifacts).toHaveBeenCalledWith("planner-session"));
  await waitFor(() =>
    expect(screen.getByTestId("ref-picker-no-parts").textContent).toBe(
      REF_PICKER_PARTS_WORDS.documents.none
    )
  );
  expect(drawnRows()).toEqual(["Agents: Planner", "Roles: planner"]);
});

test("the `/` door still lists the session's own commands", async () => {
  const user = userEvent.setup();
  const input = renderThread(COMMANDS);

  await type(user, input, "/");
  await waitFor(() =>
    expect(
      within(screen.getByRole("listbox", { name: "Slash commands" }))
        .getAllByRole("option")
        .map((option) => option.textContent)
    ).toEqual(["/compactCompact the session's context"])
  );
  // The slash list is its own: a command is the characters a terminal reads, not a reference.
  expect(screen.queryByRole("listbox", { name: "References" })).not.toBeInTheDocument();
  await user.keyboard("{Enter}");
  await waitFor(() => expect(input.value).toBe("/compact "));
  expect(pane?.sent).toEqual([]);
});

test("Enter takes the highlighted row while the picker is open, and sends while it is closed", async () => {
  const user = userEvent.setup();
  const input = renderThread();

  await type(user, input, "See #relay");
  await waitFor(() =>
    expect(drawnRows()).toEqual([
      "Issues: Relay drops a reply",
      "Messages: I told the relay to retry",
    ])
  );

  // Enter belongs to the open list: it takes the row and sends nothing.
  await user.keyboard("{Enter}");
  await waitFor(() => expect(input.value).toBe("See dispatch://CORE-2 "));
  expect(pane?.sent).toEqual([]);
  expect(screen.queryByRole("listbox", { name: "References" })).not.toBeInTheDocument();

  // With the list closed Enter is the composer's Send again.
  await user.keyboard("{Enter}");
  // The space the pick left after the address goes out with the message, as a typed one would.
  await waitFor(() => expect(pane?.sent).toEqual(["See dispatch://CORE-2 "]));
});

test("a picked agent is written as the mention that names it, and the hint says who is told", async () => {
  const user = userEvent.setup();
  const input = renderThread();

  // The words the list owns, read as the box reads them: a change of them is made in one place.
  expect(screen.getByTestId("agent-message-hint").textContent).toBe(
    dispatchWritingBox("agent-live-composer", "agent-message").hint
  );

  await type(user, input, "@plan");
  await waitFor(() => expect(drawnRows()).toEqual(["Agents: Planner", "Roles: planner"]));
  await user.keyboard("{Enter}");
  await waitFor(() => expect(input.value).toBe("[@Planner](dispatch://agent/planner-session) "));
  // The mention's words start with the `@` the door opens on, but the door does not open again
  // over the mention it wrote: not even with the caret right after it.
  await user.keyboard("{Backspace}");
  await waitFor(() => expect(input.value).toBe("[@Planner](dispatch://agent/planner-session)"));
  expect(screen.queryByRole("listbox", { name: "References" })).not.toBeInTheDocument();
});

// A door never opens in a written link's words: a pick there would write a link inside a link.
// The caret moved back into a mention's words opens no list, and Enter keeps its ordinary
// meaning here, which is Send.
test("the caret inside a written mention opens no list, and Enter sends the message", async () => {
  const user = userEvent.setup();
  const input = renderThread();
  const mention = "[@Planner](dispatch://agent/planner-session)";

  await type(user, input, mention);
  // Back through the address and into the words: `[@Plan|ner](…)`.
  await user.keyboard("{ArrowLeft}".repeat(mention.length - "[@Plan".length));
  expect(input.selectionStart).toBe("[@Plan".length);
  expect(screen.queryByRole("listbox", { name: "References" })).not.toBeInTheDocument();

  await user.keyboard("{Enter}");
  await waitFor(() => expect(pane?.sent).toEqual([mention]));
});

// A door opens in a link's address on a trigger typed there, as in every other box, and its pick
// writes the address alone: assistant-ui hands the formatter the picked item alone, so this row
// is what proves the composer tells it where the door stands.
test("a door opens in a link's address, and an agent picked there is its bare address", async () => {
  const user = userEvent.setup();
  const input = renderThread();

  await type(user, input, "[the plan](@plan");
  await waitFor(() => expect(drawnRows()).toEqual(["Agents: Planner", "Roles: planner"]));
  await user.keyboard("{Enter}");
  // assistant-ui leaves a space after a pick, which a link may hold before its `)`.
  await waitFor(() => expect(input.value).toBe("[the plan](dispatch://agent/planner-session "));
  expect(pane?.sent).toEqual([]);
});
