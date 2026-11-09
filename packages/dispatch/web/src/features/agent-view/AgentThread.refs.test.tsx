import { afterEach, expect, type Mock, spyOn, test } from "bun:test";
import {
  AssistantRuntimeProvider,
  type ThreadMessageLike,
  useExternalStoreRuntime,
} from "@assistant-ui/react";
import type { AgentStreamCommand } from "@legion/contracts";
import { cleanup, screen, waitFor, within } from "@testing-library/react";
import userEvent, { type UserEvent } from "@testing-library/user-event";
import type { ReactNode } from "react";
import { MemoryRouter } from "react-router-dom";

import { renderWithQueries as render } from "../../__tests__/query-provider";
import { api } from "../../api/client";
import type { Agent, SearchResponse } from "../../api/types";
import { NO_SESSION_SENDS } from "../conversation/useSessionSends";
import { AgentThread } from "./AgentThread";

/**
 * The reference picker in an agent's live conversation: the three doors of every other box,
 * beside the `/` that lists the session's own commands, in assistant-ui's composer.
 */

const now = Date.now();

const planner: Agent = {
  capabilities: [],
  dir: "/workspaces/planner",
  host: "build-host",
  last_activity: new Date(now).toISOString(),
  last_seen: now,
  machine_id: "build-host",
  open_asks: 0,
  roles: ["planner"],
  session_id: "planner-session",
  title: "Planner",
};

const COMMANDS: AgentStreamCommand[] = [
  { description: "Compact the session's context", name: "compact", source: "builtin" },
];

/** What the search answers for `relay`: an issue, and a message, which this box must not offer
 *  - though the route is never asked for one. */
const hits: SearchResponse = {
  results: [
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
  ],
  took_ms: 2,
};

interface Pane {
  /** Every message the composer sent, in press order. */
  readonly sent: string[];
  readonly search: Mock<typeof api.search>;
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
        held={false}
        ownSendCount={0}
        placeholder="Message the session…"
        replayCount={0}
        resetKey="session-1"
        sends={NO_SESSION_SENDS}
        sessionId="session-1"
      />
    </AssistantRuntimeProvider>
  );
}

function renderThread(commands?: AgentStreamCommand[]): HTMLTextAreaElement {
  const listAgents = spyOn(api, "listAgents").mockResolvedValue([planner]);
  const listUsers = spyOn(api, "listUsers").mockResolvedValue([{ login: "alice@theorem.dev" }]);
  const search = spyOn(api, "search").mockImplementation(async (query: string) => ({
    results: hits.results.filter((hit) => hit.snippet.toLowerCase().includes(query.toLowerCase())),
    took_ms: 2,
  }));
  const sent: string[] = [];
  render(
    <MemoryRouter>
      <Harness commands={commands} onSend={(body) => sent.push(body)} />
    </MemoryRouter>
  );
  pane = {
    restore: () => {
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
    expect(drawnRows()).toEqual(["People: alice@theorem.dev", "Agents: Planner", "Roles: planner"])
  );

  // `#` and `[[` reach the same rows; the searched kinds need two characters typed.
  await type(user, input, "#relay");
  await waitFor(() => expect(drawnRows()).toEqual(["Issues: Relay drops a reply"]));

  await type(user, input, "[[relay");
  await waitFor(() => expect(drawnRows()).toEqual(["Issues: Relay drops a reply"]));
});

test("this box offers no message row and no message block, and asks for neither", async () => {
  const user = userEvent.setup();
  const input = renderThread();

  await type(user, input, "#relay");
  await waitFor(() => expect(pane?.search).toHaveBeenCalledTimes(1));
  // A message of a session's own conversation belongs to no issue, so it has no address: the
  // route is never asked for one, nor for the blocks of one.
  expect(pane?.search.mock.calls[0]).toEqual([
    "relay",
    { kind: ["issue", "document", "ask", "comment"], limit: 8 },
  ]);
  await waitFor(() => expect(drawnRows()).toEqual(["Issues: Relay drops a reply"]));
  expect(screen.queryByText("I told the relay to retry")).not.toBeInTheDocument();
  // The instruction names what the next two letters will add here, messages left out.
  await type(user, input, "@");
  await waitFor(() =>
    expect(screen.getByTestId("ref-picker-instruction").textContent).toBe(
      "Type two characters to search issues, documents, asks and comments."
    )
  );
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
  await waitFor(() => expect(drawnRows()).toEqual(["Issues: Relay drops a reply"]));

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

test("a picked agent is written as the link that names it, and the hint says who is told", async () => {
  const user = userEvent.setup();
  const input = renderThread();

  expect(screen.getByTestId("agent-message-hint").textContent).toBe(
    "Type @ for an agent, a role or a person, # for an issue, [[ for anything else. Nobody you pick is told."
  );

  await type(user, input, "@plan");
  await waitFor(() => expect(drawnRows()).toEqual(["Agents: Planner", "Roles: planner"]));
  await user.keyboard("{Enter}");
  await waitFor(() => expect(input.value).toBe("[Planner](dispatch://agent/planner-session) "));
});
