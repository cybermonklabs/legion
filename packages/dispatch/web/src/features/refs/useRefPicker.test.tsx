import { afterEach, beforeAll, expect, type Mock, spyOn } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, render, screen, waitFor, within } from "@testing-library/react";
import userEvent, { type UserEvent } from "@testing-library/user-event";
import { type ReactNode, useState } from "react";
import { MemoryRouter } from "react-router-dom";

import { displayTree } from "../../__tests__/display-trees";
import { graphNode } from "../../__tests__/graph-fixture";
import { plannerAgent, searchResponse } from "../../__tests__/search-fixture";
import { AGENT_LIST_REFILLING, ApiError, api } from "../../api/client";
import type {
  Agent,
  ArchitectureTree,
  ArtifactWithTrees,
  DisplayTree,
  GraphReferences,
  IssueCollaborators,
  SearchResponse,
  SearchResult,
} from "../../api/types";
import { warmMarkdownRenderer } from "./markdown-engine";
import { RefPickerList } from "./RefPickerList";
import {
  applyInsertion,
  REF_PICKER_HEADINGS,
  REF_PICKER_INSTRUCTION,
  REF_PICKER_KIND_NAMES,
  REF_PICKER_KINDS,
  REF_PICKER_PARTS_WORDS,
  REF_PICKER_PARTY_WORDS,
  REF_PICKER_WORDS,
  type RefPickerInsertion,
  type RefPickerKind,
} from "./ref-picker";
import { refPickerTest as test } from "./ref-picker-fixture";
import { useRefPicker } from "./useRefPicker";

const now = Date.now();

beforeAll(async () => {
  await warmMarkdownRenderer();
});

/** `[` opens a key descriptor in user-event's keyboard syntax, so each one is doubled to type
 *  the character: this is the `[[` door typed out. */
const BRACKETS = "[[[[";

const planner = plannerAgent({ capabilities: ["btw"] });

const worker: Agent = {
  ...planner,
  dir: "/workspaces/worker",
  last_seen: now - 1_000,
  roles: [],
  session_id: "worker-session",
  title: "Worker",
};

/** The issue's collaborators: a person, and a reviewer whose session has stopped, so that the
 *  reviewer role has no holder the registry lists. */
const collaborators: IssueCollaborators = {
  agents: [
    {
      kind: "session",
      last_at: "2026-11-01T00:00:00Z",
      legion: {
        replaced_at: null,
        replaced_by: null,
        role: "reviewer",
        role_token: "reviewer",
        started_at: "2026-11-01T00:00:00Z",
      },
      reasons: ["posted"],
      session_id: "reviewer-session",
      title: "Reviewer",
    },
  ],
  issue_key: "CORE-1",
  legion_registry: "ok",
  people: [{ kind: "user", last_at: null, login: "alice@example.com", reasons: ["assignee"] }],
};

const architecture = {
  components: [
    {
      depends_on: [],
      done: 0,
      external: false,
      id: "relay",
      issues: [],
      own_done: 0,
      own_total: 0,
      parent: null,
      paths: [],
      prose: "",
      source: "cybermonklabs/legion",
      title: "The relay",
      total: 0,
    },
  ],
  not_architectural: [],
  retired_links: [],
  sources: [],
  totals: {
    components_without_work: 1,
    issues_done: 0,
    issues_total: 0,
    not_architectural: 0,
    retired_links: 0,
    unassigned: 0,
  },
  unassigned: [],
} satisfies ArchitectureTree;

/** What the search answers for a query the fixture's words hold: an issue and a comment of it. */
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
    href: "/issues/CORE-2/comments/comment-1",
    id: "comment-1",
    kind: "comment",
    owner: { key: "CORE-2", kind: "issue", status: "open", title: "Relay drops a reply" },
    rank: 0.09,
    snippet: "I saw the <mark>relay</mark> drop one",
  },
];

const blocks: GraphReferences = {
  edges: [
    {
      created_at: "2026-11-01T00:00:00Z",
      direction: "in",
      excerpt: { text: "The first words of the block" },
      kind: "part_of",
      node: graphNode({ block_id: "block-1", id: "comment-1", kind: "block" }),
      source_seq: null,
    },
  ],
  node: graphNode({ id: "comment-1", kind: "comment" }),
};

/** The one file the planner's conversation holds, as `GET /api/v1/agents/{session_id}/artifacts`
 *  lists it. */
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
  client: QueryClient;
  getArchitecture: Mock<typeof api.getArchitecture>;
  getIssueCollaborators: Mock<typeof api.getIssueCollaborators>;
  getReferences: Mock<typeof api.getReferences>;
  listAgentArtifacts: Mock<typeof api.listAgentArtifacts>;
  listAgents: Mock<typeof api.listAgents>;
  listUsers: Mock<typeof api.listUsers>;
  restore: () => void;
  search: Mock<typeof api.search>;
  unmount: () => void;
}

let pane: Pane | undefined;

afterEach(() => {
  pane?.unmount();
  pane?.restore();
  pane = undefined;
});

/**
 * A plain `<textarea>` host for the picker: it owns the text and the caret, hands the picker
 * each key, and applies the insertion the picker answers with. The three real hosts do the same
 * with their own documents, which is what this interface is for.
 */
function Host({ issueKey }: { issueKey?: string }): ReactNode {
  const [draft, setDraft] = useState({ caret: 0, text: "" });
  const picker = useRefPicker({ caret: draft.caret, issueKey, text: draft.text });
  const write = (insertion: RefPickerInsertion) => {
    const next = applyInsertion(draft.text, insertion);
    setDraft(next);
  };
  return (
    <>
      <textarea
        aria-label="Comment"
        onChange={(event) =>
          setDraft({ caret: event.target.selectionStart ?? 0, text: event.target.value })
        }
        onKeyDown={(event) => {
          const result = picker.pressKey(event.key);
          if (result.action === "ignored") return;
          event.preventDefault();
          if (result.action === "took") write(result.insertion);
        }}
        value={draft.text}
      />
      <RefPickerList onTake={write} picker={picker} />
    </>
  );
}

/** The search answers the fixture's hits for a query its words hold, as the route would. */
const answerSearch = async (query: string): Promise<SearchResponse> =>
  searchResponse(
    hitRows.filter((hit) => hit.snippet.toLowerCase().includes(query.toLowerCase())),
    { took_ms: 2 }
  );

function renderHost({
  agents = [planner, worker],
  noIssue = false,
  searchAnswer = answerSearch,
}: {
  agents?: Agent[];
  /** The box belongs to no issue: it offers everyone who has signed in. */
  noIssue?: boolean;
  searchAnswer?: typeof api.search;
} = {}): Pane {
  const listAgents = spyOn(api, "listAgents").mockResolvedValue(agents);
  const getIssueCollaborators = spyOn(api, "getIssueCollaborators").mockResolvedValue(
    collaborators
  );
  const listUsers = spyOn(api, "listUsers").mockResolvedValue([{ login: "bob@example.com" }]);
  const getArchitecture = spyOn(api, "getArchitecture").mockResolvedValue(architecture);
  const getReferences = spyOn(api, "getReferences").mockResolvedValue(blocks);
  const listAgentArtifacts = spyOn(api, "listAgentArtifacts").mockResolvedValue([shot]);
  const search = spyOn(api, "search").mockImplementation(searchAnswer);
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const view = render(
    <MemoryRouter>
      <QueryClientProvider client={client}>
        <Host issueKey={noIssue ? undefined : "CORE-1"} />
      </QueryClientProvider>
    </MemoryRouter>
  );
  pane = {
    client,
    getArchitecture,
    getIssueCollaborators,
    getReferences,
    listAgentArtifacts,
    listAgents,
    listUsers,
    restore: () => {
      search.mockRestore();
      listAgentArtifacts.mockRestore();
      getReferences.mockRestore();
      getArchitecture.mockRestore();
      listUsers.mockRestore();
      getIssueCollaborators.mockRestore();
      listAgents.mockRestore();
    },
    search,
    unmount: view.unmount,
  };
  return pane;
}

/** The picker's rows in the order they are drawn, each as `<heading>: <words>`. */
function drawnRows(): string[] {
  const list = screen.getByRole("listbox", { name: "References" });
  return within(list)
    .getAllByRole("group")
    .flatMap((group) => {
      const heading = group.firstElementChild?.textContent ?? "";
      return within(group)
        .getAllByRole("option")
        .map((option) => `${heading}: ${option.querySelector("span span")?.textContent ?? ""}`);
    });
}

async function field(): Promise<HTMLTextAreaElement> {
  return (await screen.findByLabelText("Comment")) as HTMLTextAreaElement;
}

test("every door lists the parties and the components before a character is typed, with one instruction row", async () => {
  const user = userEvent.setup();
  const page = renderHost();

  const comment = await field();
  await user.click(comment);
  await user.keyboard("@");

  await screen.findByRole("listbox", { name: "References" });
  // The `@` door: people, agents, roles, then the kinds that need no query either. The issue
  // records a reviewer whose session has stopped, which is offered as a role nobody runs.
  await waitFor(() =>
    expect(drawnRows()).toEqual([
      "People: alice@example.com",
      "Agents: Planner",
      "Agents: Worker",
      "Roles: planner",
      "Roles: reviewer",
      "Components: The relay",
    ])
  );
  // The searched kinds cannot be listed yet, and the one instruction row says why.
  expect(screen.getByTestId("ref-picker-instruction").textContent).toBe(REF_PICKER_INSTRUCTION);
  expect(page.search).not.toHaveBeenCalled();
});

// The roles a picker offers are the known ones (`role-holders.ts`): a role a session the
// registry lists holds, live or unresponsive, and a role the issue's collaborators record whose
// session has stopped, which is offered as not running.
test("the picker offers a role a listed session holds, unresponsive or not, and the issue's stopped roles", async () => {
  const user = userEvent.setup();
  renderHost({ agents: [planner, { ...worker, roles: ["tester"], state: "unresponsive" }] });

  const comment = await field();
  await user.click(comment);
  await user.keyboard("@");

  await waitFor(() =>
    expect(drawnRows()).toEqual([
      "People: alice@example.com",
      "Agents: Planner",
      "Agents: Worker",
      "Roles: planner",
      "Roles: tester",
      "Roles: reviewer",
      "Components: The relay",
    ])
  );
});

// A role the issue's collaborators record is known while no session holds it, whatever the
// registry says of the session that last did: here that session is live and claims no role, so
// the role is offered as not running - the one rule the renderer links it by
// (`reference-naming.test.tsx`, "a role whose recorded session runs on without it").
test("the picker offers a recorded role whose session is live and claims no role", async () => {
  const user = userEvent.setup();
  const reviewer = {
    ...worker,
    last_seen: now - 2_000,
    roles: [],
    session_id: "reviewer-session",
    title: "Reviewer",
  };
  renderHost({ agents: [planner, worker, reviewer] });

  const comment = await field();
  await user.click(comment);
  await user.keyboard("@");

  await waitFor(() =>
    expect(drawnRows()).toEqual([
      "People: alice@example.com",
      "Agents: Planner",
      "Agents: Worker",
      "Agents: Reviewer",
      "Roles: planner",
      "Roles: reviewer",
      "Components: The relay",
    ])
  );
});

test("the doors differ only in their order, and `#` leads with the issues the search answers", async () => {
  const user = userEvent.setup();
  const page = renderHost();

  const comment = await field();
  await user.click(comment);
  await user.keyboard("#relay");

  await waitFor(() => expect(page.search).toHaveBeenCalledTimes(1));
  expect(page.search.mock.calls[0]).toEqual([
    "relay",
    { kind: ["issue", "document", "ask", "comment", "message"], limit: 8 },
  ]);
  await waitFor(() =>
    expect(drawnRows()).toEqual([
      "Issues: Relay drops a reply",
      "Comments: I saw the relay drop one",
      "Components: The relay",
    ])
  );
  expect(screen.queryByTestId("ref-picker-instruction")).not.toBeInTheDocument();
});

test("the `[[` door ranks the same rows with the documents first and the parties last", async () => {
  const user = userEvent.setup();
  renderHost();

  const comment = await field();
  await user.click(comment);
  await user.keyboard(`${BRACKETS}relay`);

  await waitFor(() =>
    expect(drawnRows()).toEqual([
      "Issues: Relay drops a reply",
      "Comments: I saw the relay drop one",
      "Components: The relay",
    ])
  );
  // `[[` over a query that matches a party puts that party after the searched kinds, where
  // `@` puts it first.
  await user.clear(comment);
  await user.keyboard(`${BRACKETS}plan`);
  await waitFor(() => expect(drawnRows()).toEqual(["Agents: Planner", "Roles: planner"]));
});

/** A hit of each kind the search route ranks, each holding the word the query is: the issue and
 *  the comment of `hitRows`, and a document, an ask and a message of the same issue. */
const everyKindHits: SearchResult[] = [
  ...hitRows,
  {
    artifact: { name: "notes.md", slug: "notes-md" },
    href: "/issues/CORE-2/artifacts/notes-md",
    id: "document-1",
    kind: "document",
    owner: { key: "CORE-2", kind: "issue", status: "open", title: "Relay drops a reply" },
    rank: 0.08,
    snippet: "Notes on the <mark>relay</mark>",
  },
  {
    href: "/issues/CORE-2/asks/ask-1",
    id: "ask-1",
    kind: "ask",
    owner: { key: "CORE-2", kind: "issue", status: "open", title: "Relay drops a reply" },
    rank: 0.07,
    snippet: "Should the <mark>relay</mark> retry?",
  },
  {
    href: "/issues/CORE-2/messages/message-1",
    id: "message-1",
    kind: "message",
    owner: { key: "CORE-2", kind: "issue", status: "open", title: "Relay drops a reply" },
    rank: 0.06,
    snippet: "The <mark>relay</mark> is back",
  },
];

/** Each kind that holds blocks, and the kind its blocks are listed under. */
const BLOCK_HOLDERS = [
  ["document", "document-block"],
  ["ask", "ask-part"],
  ["comment", "comment-block"],
  ["message", "message-block"],
] as const satisfies readonly (readonly [RefPickerKind, RefPickerKind])[];

/** The four block kinds, which share one glyph by design, each under its own name
 *  (`kind-icons.tsx`): a block is a part of the node above it. */
const BLOCK_KINDS: readonly RefPickerKind[] = BLOCK_HOLDERS.map(([, block]) => block);

/** One drawn row: the heading of the group it sits in, and the icon it wears - the label the
 *  icon carries for a reader who cannot see it (its `<title>`), and its glyph, written as the
 *  tag and the attributes of each shape it draws. */
interface DrawnIcon {
  readonly glyph: string;
  readonly heading: string;
  readonly label: string;
}

/** The icon each row of the open list wears, in the order the rows are drawn. A row with no icon
 *  reads as an empty label and an empty glyph. */
function drawnIcons(): DrawnIcon[] {
  const list = screen.getByRole("listbox", { name: "References" });
  return within(list)
    .getAllByRole("group")
    .flatMap((group) => {
      const heading = group.firstElementChild?.textContent ?? "";
      return within(group)
        .getAllByRole("option")
        .map((option) => {
          const icon = Array.from(option.children).find(
            (child) => child.tagName.toLowerCase() === "svg"
          );
          const parts = Array.from(icon?.children ?? []);
          const label =
            parts.find((part) => part.tagName.toLowerCase() === "title")?.textContent ?? "";
          const glyph = parts
            .filter((part) => part.tagName.toLowerCase() !== "title")
            .map((shape) => {
              const attributes = shape
                .getAttributeNames()
                .sort()
                .map((name) => `${name}=${shape.getAttribute(name)}`);
              return `${shape.tagName.toLowerCase()}(${attributes.join(" ")})`;
            })
            .join(" ");
          return { glyph, heading, label };
        });
    });
}

/** Clicks the control that lists the blocks of the row under `heading`. */
async function openBlocks(user: UserEvent, heading: string) {
  const list = screen.getByRole("listbox", { name: "References" });
  const group = within(list)
    .getAllByRole("group")
    .find((one) => one.firstElementChild?.textContent === heading);
  if (group === undefined) throw new Error(`the list draws no group headed "${heading}"`);
  await user.click(
    within(group).getByRole("button", { name: REF_PICKER_PARTS_WORDS.blocks.control })
  );
}

// R2: the rows are grouped under a heading per kind and carry the kind's own icon
// (`RefKindIcon`). Every kind the picker reaches is drawn here, as a reader reaches it: the
// parties and the components before a character is typed, the five kinds the search answers,
// and the blocks of each of the four kinds that hold blocks, one node's at a time. The icon is
// read by what it carries - its label and the attributes of its shapes - so a kind drawn with no
// icon, with another kind's label or with a glyph another kind wears fails the row.
test("each kind's rows sit under that kind's heading and wear that kind's own icon", async () => {
  const user = userEvent.setup();
  renderHost({ searchAnswer: async () => searchResponse(everyKindHits) });
  const seen: DrawnIcon[] = [];

  const comment = await field();
  await user.click(comment);
  await user.keyboard("@");
  await waitFor(() =>
    expect(drawnRows()).toEqual([
      "People: alice@example.com",
      "Agents: Planner",
      "Agents: Worker",
      "Roles: planner",
      "Roles: reviewer",
      "Components: The relay",
    ])
  );
  seen.push(...drawnIcons());

  await user.clear(comment);
  await user.keyboard("#relay");
  await waitFor(() =>
    expect(drawnRows()).toEqual([
      "Issues: Relay drops a reply",
      "Documents: notes.md",
      "Asks: Should the relay retry?",
      "Comments: I saw the relay drop one",
      "Messages: The relay is back",
      "Components: The relay",
    ])
  );
  seen.push(...drawnIcons());
  for (const [holder, block] of BLOCK_HOLDERS) {
    await openBlocks(user, REF_PICKER_HEADINGS[holder]);
    await waitFor(() =>
      expect(drawnRows()).toContain(`${REF_PICKER_HEADINGS[block]}: The first words of the block`)
    );
    seen.push(...drawnIcons());
  }

  const kindOf = new Map(REF_PICKER_KINDS.map((kind) => [REF_PICKER_HEADINGS[kind], kind]));
  const worn = new Map<RefPickerKind, { glyphs: Set<string>; labels: Set<string> }>();
  for (const row of seen) {
    const kind = kindOf.get(row.heading);
    if (kind === undefined) throw new Error(`no kind's rows are headed "${row.heading}"`);
    const icons = worn.get(kind) ?? { glyphs: new Set<string>(), labels: new Set<string>() };
    icons.glyphs.add(row.glyph);
    icons.labels.add(row.label);
    worn.set(kind, icons);
  }
  // Every kind the picker reaches was drawn, each under its own heading.
  expect([...worn.keys()].sort()).toEqual([...REF_PICKER_KINDS].sort());
  const glyphOf = new Map<RefPickerKind, string>();
  for (const [kind, icons] of worn) {
    // Every row of a kind wears one icon, which carries that kind's own name.
    expect([...icons.labels], `the label ${kind}'s icon carries`).toEqual([
      REF_PICKER_KIND_NAMES[kind],
    ]);
    const glyphs = [...icons.glyphs];
    expect(glyphs, `the glyphs ${kind}'s rows draw`).toHaveLength(1);
    expect(glyphs[0], `${kind}'s rows draw no glyph`).not.toBe("");
    glyphOf.set(kind, glyphs[0] ?? "");
  }
  // And the glyph is the kind's own: no two kinds draw the same one, but the four block kinds.
  const shared = [...glyphOf].flatMap(([kind, glyph]) =>
    [...glyphOf]
      .filter(
        ([other, theirs]) =>
          other > kind &&
          theirs === glyph &&
          !(BLOCK_KINDS.includes(kind) && BLOCK_KINDS.includes(other))
      )
      .map(([other]) => `${kind} and ${other}`)
  );
  expect(shared, "kinds that draw one glyph").toEqual([]);
});

test("the picker filters as the reader types, moves with the arrows and takes the row on Enter", async () => {
  const user = userEvent.setup();
  renderHost();

  const comment = await field();
  await user.click(comment);
  await user.keyboard("@");
  await screen.findByRole("listbox", { name: "References" });

  await waitFor(() => expect(drawnRows()).toHaveLength(6));

  await user.keyboard("worker");
  await waitFor(() => expect(drawnRows()).toEqual(["Agents: Worker"]));

  await user.keyboard("{Backspace}".repeat("worker".length));
  await waitFor(() => expect(drawnRows()).toHaveLength(6));
  // The first row is highlighted until the reader moves; one step down takes the second.
  const options = screen.getAllByRole("option");
  expect(options[0]?.getAttribute("aria-selected")).toBe("true");
  await user.keyboard("{ArrowDown}");
  await waitFor(() =>
    expect(screen.getAllByRole("option")[1]?.getAttribute("aria-selected")).toBe("true")
  );
  await user.keyboard("{ArrowUp}{ArrowUp}");
  await waitFor(() =>
    expect(screen.getAllByRole("option")[5]?.getAttribute("aria-selected")).toBe("true")
  );

  await user.keyboard("{Enter}");
  await waitFor(() => expect(comment.value).toBe("dispatch://CORE/component/relay"));
});

test("Tab takes the highlighted row, and a party is written as the mention naming the party", async () => {
  const user = userEvent.setup();
  renderHost();

  const comment = await field();
  await user.click(comment);
  await user.keyboard("@plann");
  await waitFor(() => expect(drawnRows()).toEqual(["Agents: Planner", "Roles: planner"]));

  await user.keyboard("{Tab}");
  await waitFor(() => expect(comment.value).toBe("[@Planner](dispatch://agent/planner-session)"));
  expect(screen.queryByRole("listbox", { name: "References" })).not.toBeInTheDocument();
});

test("a pick keeps the text after the caret", async () => {
  const user = userEvent.setup();
  renderHost();

  const comment = await field();
  await user.click(comment);
  await user.keyboard("see  and the rest");
  comment.setSelectionRange("see ".length, "see ".length);
  await user.keyboard("@plann");
  await waitFor(() => expect(drawnRows()).toEqual(["Agents: Planner", "Roles: planner"]));

  await user.keyboard("{Enter}");
  await waitFor(() =>
    expect(comment.value).toBe("see [@Planner](dispatch://agent/planner-session) and the rest")
  );
});

test("Escape closes the picker and leaves the character the reader typed", async () => {
  const user = userEvent.setup();
  renderHost();

  const comment = await field();
  await user.click(comment);
  await user.keyboard("@pl");
  await screen.findByRole("listbox", { name: "References" });

  await user.keyboard("{Escape}");

  expect(screen.queryByRole("listbox", { name: "References" })).not.toBeInTheDocument();
  expect(comment.value).toBe("@pl");
  // It stays shut while the reader writes on after the character it left.
  await user.keyboard("anner");
  expect(comment.value).toBe("@planner");
  expect(screen.queryByRole("listbox", { name: "References" })).not.toBeInTheDocument();
});

test("a space typed before a row is taken closes the picker, leaving a heading a heading", async () => {
  const user = userEvent.setup();
  renderHost();

  const comment = await field();
  await user.click(comment);
  await user.keyboard("#");
  await screen.findByRole("listbox", { name: "References" });

  await user.keyboard(" Heading");

  expect(screen.queryByRole("listbox", { name: "References" })).not.toBeInTheDocument();
  expect(comment.value).toBe("# Heading");
});

test("a trigger that is not at a word boundary opens nothing", async () => {
  const user = userEvent.setup();
  renderHost();

  const comment = await field();
  await user.click(comment);
  await user.keyboard("mail alice@example.com");

  expect(screen.queryByRole("listbox", { name: "References" })).not.toBeInTheDocument();
});

test("a node's row lists its blocks from its part_of backlinks, and a block pick is the bare address", async () => {
  const user = userEvent.setup();
  const page = renderHost();

  const comment = await field();
  await user.click(comment);
  await user.keyboard("#relay");
  await waitFor(() => expect(drawnRows()).toContain("Comments: I saw the relay drop one"));

  await user.click(screen.getByRole("button", { name: "Blocks" }));

  await waitFor(() =>
    expect(page.getReferences).toHaveBeenCalledWith("dispatch://CORE-2/comment/comment-1", {
      kind: "part_of",
    })
  );
  await waitFor(() =>
    expect(drawnRows()).toContain("Comment blocks: The first words of the block")
  );

  await user.click(screen.getByRole("option", { name: /The first words/ }));
  await waitFor(() => expect(comment.value).toBe("dispatch://CORE-2/comment/comment-1#b-block-1"));
});

// A document hit's href names the block the hit matched (`#b-`), so its reader lands on the
// passage (the server's `searchHref`). The row is the document all the same: its detail and the
// address a pick writes are the document's, and it lists the document's blocks, which the graph
// answers for the document's address alone - a block holds no blocks of its own.
test("a document hit that matched one of its blocks is offered as the document, and lists the document's blocks", async () => {
  const user = userEvent.setup();
  const notes = "dispatch://CORE-2/artifact/notes-md";
  const page = renderHost({
    searchAnswer: async () =>
      searchResponse([
        {
          artifact: { name: "notes.md", slug: "notes-md" },
          href: "/issues/CORE-2/artifacts/notes-md?q=notes#b-block-2",
          id: "document-1",
          kind: "document",
          owner: { key: "CORE-2", kind: "issue", status: "open", title: "Relay drops a reply" },
          rank: 0.08,
          snippet: "The relay <mark>notes</mark>",
        },
      ]),
  });
  page.getReferences.mockImplementation(async (reference) => ({
    edges:
      reference === notes
        ? [
            {
              created_at: "2026-11-01T00:00:00Z",
              direction: "in",
              excerpt: { text: "The relay notes." },
              kind: "part_of",
              node: graphNode({ block_id: "block-2", id: "document-1", kind: "block" }),
              source_seq: null,
            },
          ]
        : [],
    node: graphNode({ id: "document-1", kind: reference === notes ? "artifact" : "block" }),
  }));

  const comment = await field();
  await user.click(comment);
  await user.keyboard(`${BRACKETS}notes`);
  await waitFor(() => expect(drawnRows()).toEqual(["Documents: notes.md"]));
  const row = screen.getByRole("option", { name: /^notes\.md/ });
  expect(within(row).getByTitle("CORE-2 notes-md")).toBeInTheDocument();

  await user.click(within(row).getByRole("button", { name: "Blocks" }));

  await waitFor(() => expect(page.getReferences).toHaveBeenCalledWith(notes, { kind: "part_of" }));
  await waitFor(() =>
    expect(drawnRows()).toEqual(["Documents: notes.md", "Document blocks: The relay notes."])
  );

  await user.click(screen.getByRole("option", { name: /^notes\.md/ }));
  await waitFor(() => expect(comment.value).toBe(notes));
});

test("an agent's row lists its conversation's documents, and a document pick is its address", async () => {
  const user = userEvent.setup();
  const page = renderHost();

  const comment = await field();
  await user.click(comment);
  await user.keyboard("@plann");
  await waitFor(() => expect(drawnRows()).toEqual(["Agents: Planner", "Roles: planner"]));
  // A role holds no documents of its own: only the session's row opens any.
  expect(screen.getAllByRole("button", { name: "Documents" })).toHaveLength(1);
  expect(page.listAgentArtifacts).not.toHaveBeenCalled();

  await user.click(screen.getByRole("button", { name: "Documents" }));

  await waitFor(() => expect(page.listAgentArtifacts).toHaveBeenCalledWith("planner-session"));
  await waitFor(() =>
    expect(drawnRows()).toEqual(["Agents: Planner", "Roles: planner", "Documents: shot.png"])
  );
  // A file holds no blocks, so its row opens nothing.
  expect(screen.getAllByRole("button", { name: "Documents" })).toHaveLength(1);
  expect(screen.queryByRole("button", { name: "Blocks" })).not.toBeInTheDocument();

  await user.click(screen.getByRole("option", { name: /^shot\.png/ }));
  await waitFor(() =>
    expect(comment.value).toBe("dispatch://agent/planner-session/artifact/shot-png")
  );
});

test("ArrowRight on an agent's row lists its documents, and Enter takes one", async () => {
  const user = userEvent.setup();
  const page = renderHost();

  const comment = await field();
  await user.click(comment);
  await user.keyboard("@plann");
  await waitFor(() => expect(drawnRows()).toEqual(["Agents: Planner", "Roles: planner"]));

  await user.keyboard("{ArrowRight}");

  await waitFor(() => expect(page.listAgentArtifacts).toHaveBeenCalledWith("planner-session"));
  await waitFor(() =>
    expect(drawnRows()).toEqual(["Agents: Planner", "Roles: planner", "Documents: shot.png"])
  );
  await user.keyboard("{ArrowDown}{ArrowDown}{Enter}");
  await waitFor(() =>
    expect(comment.value).toBe("dispatch://agent/planner-session/artifact/shot-png")
  );
});

// An agent's row offers its documents before any read says whether it holds one, so an opened
// row that lists none says so, rather than reading as a control that did nothing. ArrowLeft
// closes the opened row, and the note with it.
test("an agent whose conversation holds no file says so when its documents are opened", async () => {
  const user = userEvent.setup();
  const page = renderHost();
  page.listAgentArtifacts.mockResolvedValue([]);

  const comment = await field();
  await user.click(comment);
  await user.keyboard("@plann");
  await waitFor(() => expect(drawnRows()).toEqual(["Agents: Planner", "Roles: planner"]));

  await user.keyboard("{ArrowRight}");

  await waitFor(() => expect(page.listAgentArtifacts).toHaveBeenCalledWith("planner-session"));
  await waitFor(() =>
    expect(screen.getByTestId("ref-picker-no-parts").textContent).toBe(
      REF_PICKER_PARTS_WORDS.documents.none
    )
  );
  expect(drawnRows()).toEqual(["Agents: Planner", "Roles: planner"]);

  await user.keyboard("{ArrowLeft}");
  await waitFor(() => expect(screen.queryByTestId("ref-picker-no-parts")).not.toBeInTheDocument());
  expect(comment.value).toBe("@plann");
});

test("a box with no issue offers everyone who has signed in", async () => {
  const user = userEvent.setup();
  const page = renderHost({ noIssue: true });

  const comment = await field();
  await user.click(comment);
  await user.keyboard("@");

  await waitFor(() => expect(drawnRows()).toContain("People: bob@example.com"));
  expect(page.getIssueCollaborators).not.toHaveBeenCalled();
  expect(page.listUsers).toHaveBeenCalledTimes(1);
});

test("the list says it is loading, that nothing matches, and that Dispatch is unreachable", async () => {
  const user = userEvent.setup();
  const slow = Promise.withResolvers<SearchResponse>();
  const page = renderHost({ agents: [], searchAnswer: () => slow.promise });
  page.getIssueCollaborators.mockResolvedValue({ ...collaborators, agents: [], people: [] });
  page.getArchitecture.mockRejectedValue(new ApiError(404, { code: "SOURCE_NOT_FOUND" }));

  const comment = await field();
  await user.click(comment);
  await user.keyboard("@relay");

  // Nothing of its own to list, and the search is still out: the list says it is loading.
  await waitFor(() =>
    expect(screen.getByTestId("ref-picker-loading").textContent).toBe(REF_PICKER_WORDS.loading)
  );

  slow.resolve(searchResponse([]));
  await waitFor(() =>
    expect(screen.getByTestId("ref-picker-empty").textContent).toBe(REF_PICKER_WORDS.empty)
  );

  // A read that failed is not an empty list: it says the read failed.
  page.search.mockRejectedValue(new Error("network error"));
  await user.keyboard("x");
  await waitFor(() =>
    expect(screen.getByTestId("ref-picker-unreachable").textContent).toBe(
      REF_PICKER_WORDS.unreachable
    )
  );
});

// The case the roles nobody runs are offered for: every agent of the issue has stopped, so the
// live list answers with no agent and the picker holds those roles alone, as not running. It must
// not say that nothing matches under the rows it is showing.
test("a picker holding only stopped roles does not say no agent matches", async () => {
  const user = userEvent.setup();
  const page = renderHost({ agents: [] });
  page.getIssueCollaborators.mockResolvedValue({ ...collaborators, people: [] });
  page.getArchitecture.mockRejectedValue(new ApiError(404, { code: "SOURCE_NOT_FOUND" }));

  const comment = await field();
  await user.click(comment);
  await user.keyboard("@");

  await waitFor(() => expect(drawnRows()).toEqual(["Roles: reviewer"]));
  expect(screen.getByRole("option")).toHaveTextContent(
    "Reviewer · reviewer · not running, not told"
  );
  expect(screen.queryByTestId("ref-picker-empty")).not.toBeInTheDocument();
  expect(screen.queryByTestId("agent-list-unreachable")).not.toBeInTheDocument();
});

// A role is offered as not running only where a live list that answered names nobody holding
// it: a list whose read failed says nothing of who is absent. So while it has failed, the picker
// offers none of the roles the issue records, and says the list could not load where the agent
// and role rows would be.
test("the picker adds no stopped option while the live agents list failed", async () => {
  const user = userEvent.setup();
  const page = renderHost();
  page.listAgents.mockRejectedValue(new Error("listener down"));

  const comment = await field();
  await user.click(comment);
  await user.keyboard("@");

  await waitFor(() =>
    expect(screen.getByTestId("agent-list-unreachable").textContent).toBe(
      REF_PICKER_PARTY_WORDS.unreachable
    )
  );
  expect(page.getIssueCollaborators).toHaveBeenCalledWith("CORE-1");
  await waitFor(() =>
    expect(drawnRows()).toEqual(["People: alice@example.com", "Components: The relay"])
  );
});

// The agent list is Envoy's, and its failure is said by its own line alone: with every Dispatch
// read answering, a query no row matches says neither that Dispatch is unreachable nor that
// nothing matches, since which agent and role rows match is not known, as the mention
// suggestions it replaced drew the listener's line alone. A Dispatch read that fails beside it
// still says so.
test("an agent list that failed is said by its own line alone, never as Dispatch unreachable", async () => {
  const user = userEvent.setup();
  const page = renderHost();
  page.listAgents.mockRejectedValue(new Error("listener down"));

  const comment = await field();
  await user.click(comment);
  await user.keyboard("@nobody");

  await waitFor(() => expect(page.search).toHaveBeenCalledWith("nobody", expect.anything()));
  await waitFor(() => expect(screen.queryAllByRole("option")).toHaveLength(0));
  await waitFor(() =>
    expect(screen.getByTestId("agent-list-unreachable").textContent).toBe(
      REF_PICKER_PARTY_WORDS.unreachable
    )
  );
  expect(screen.queryByTestId("ref-picker-unreachable")).not.toBeInTheDocument();
  expect(screen.queryByTestId("ref-picker-empty")).not.toBeInTheDocument();

  page.search.mockRejectedValue(new Error("network error"));
  await user.keyboard("x");
  await waitFor(() =>
    expect(screen.getByTestId("ref-picker-unreachable").textContent).toBe(
      REF_PICKER_WORDS.unreachable
    )
  );
  expect(screen.getByTestId("agent-list-unreachable").textContent).toBe(
    REF_PICKER_PARTY_WORDS.unreachable
  );
});

// A refilling agent list keeps the rows an earlier read gave and never says that nothing
// matches: it says the list is refilling instead, where the agent and role rows are read from,
// under a query that matches no row as under none.
test("mention suggestions say the agent list is refilling instead of that no agent matches", async () => {
  const user = userEvent.setup();
  const page = renderHost();
  const comment = await field();
  await user.click(comment);
  await user.keyboard("@");
  await waitFor(() => expect(drawnRows()).toContain("Agents: Planner"));
  page.listAgents.mockRejectedValue(
    new ApiError(503, { code: "ENVOY_REFILLING", error: "The agent list is refilling." })
  );
  await act(async () => {
    await page.client.refetchQueries({ queryKey: ["agents"] });
  });
  await waitFor(() =>
    expect(screen.getByTestId("agent-list-refilling").textContent).toBe(AGENT_LIST_REFILLING)
  );
  expect(drawnRows()).toContain("Agents: Planner");
  expect(screen.queryByTestId("ref-picker-empty")).not.toBeInTheDocument();
  // The row kept from the earlier read draws its agent's running state as unknown, never as not
  // running, while the list it would be judged by is refilling.
  const kept = screen.getByRole("option", { name: /^Planner/ });
  expect(
    kept.querySelector('[data-agent-name="planner-session"]')?.getAttribute("data-agent-state")
  ).toBe("unknown");

  await user.keyboard("nobody");
  await waitFor(() => expect(page.search).toHaveBeenCalledWith("nobody", expect.anything()));
  await waitFor(() => expect(screen.queryAllByRole("option")).toHaveLength(0));
  expect(screen.getByTestId("agent-list-refilling")).toBeInTheDocument();
  expect(screen.queryByTestId("ref-picker-empty")).not.toBeInTheDocument();
});

// A row is an option inside a listbox, which owns only options, so the agent in it is drawn with
// its running state and no link: a session's row by its name, and a role's row by its holder.
test("the mention picker draws each agent with its state and no link", async () => {
  const user = userEvent.setup();
  renderHost();
  const comment = await field();
  await user.click(comment);
  await user.keyboard("@plann");
  await waitFor(() => expect(drawnRows()).toEqual(["Agents: Planner", "Roles: planner"]));
  const list = screen.getByRole("listbox", { name: "References" });
  for (const option of within(list).getAllByRole("option")) {
    const agent = option.querySelector('[data-agent-name="planner-session"]');
    expect(agent?.textContent, option.textContent ?? "").toBe("Planner");
    await waitFor(() => expect(agent?.getAttribute("data-agent-state")).toBe("running"));
  }
  expect(list.querySelector("a")).toBeNull();
});

// A session's row is named by the session's name, which for a session with no title is the short
// form `session:<8>…` that two sessions may share, so the row carries the session's full id as
// its tooltip and accessible description. A role's row is named by its role, and carries none.
test("a session's row has its full id as its title and description, and a role's row has none", async () => {
  const user = userEvent.setup();
  const twins = ["twin-session-one", "twin-session-two"];
  renderHost({
    agents: twins.map((session_id, index) => ({
      ...worker,
      roles: index === 0 ? ["planner"] : [],
      session_id,
      title: "",
    })),
  });
  const comment = await field();
  await user.click(comment);
  await user.keyboard("@");
  const list = await screen.findByRole("listbox", { name: "References" });
  const sessions = await within(list).findAllByRole("option", { name: /^session:twin-ses…/ });
  expect(sessions.map((option) => option.title).sort()).toEqual(twins);
  for (const option of sessions) {
    // The title is the id of the session the row draws.
    expect(option.querySelector(`[data-agent-name="${option.title}"]`)).not.toBeNull();
    expect(option).toHaveAccessibleDescription(option.title);
  }
  const role = within(list).getByRole("option", { name: /^planner/ });
  expect(role).not.toHaveAccessibleDescription();
  expect(role.hasAttribute("title")).toBe(false);
});

/** A comment's hit whose snippet is `snippet`, with the tree the server read it into where the
 *  test gives one, numbered so several can stand in one answer. */
function commentHit(index: number, snippet: string, snippetTree?: DisplayTree): SearchResult {
  return {
    href: `/issues/CORE-2/comments/comment-${index}`,
    id: `comment-${index}`,
    kind: "comment",
    owner: { key: "CORE-2", kind: "issue", status: "open", title: "Relay drops a reply" },
    rank: 0.1 - index / 100,
    snippet,
    ...(snippetTree === undefined ? {} : { snippet_tree: snippetTree }),
  };
}

// A row is an option the reader takes, which holds no link, so a hit worded with its record's
// own text draws that text's plain words from the tree the server read it into: its formatting
// gone, and a mention read as the party's current name rather than as its Markdown.
test("a hit's row reads its snippet as its plain words", async () => {
  const user = userEvent.setup();
  const snippet = "I saw **the relay** drop [@Old](dispatch://agent/planner-session)";
  renderHost({
    searchAnswer: async () => searchResponse([commentHit(1, snippet, displayTree(snippet))]),
  });

  const comment = await field();
  await user.click(comment);
  await user.keyboard("#relay");
  await waitFor(() => expect(drawnRows()).toContain("Comments: I saw the relay drop @Planner"));
});

// The head of a block, which its node's `part_of` backlink carries with its tree, is drawn the
// same way.
test("a block's row reads its excerpt as its plain words", async () => {
  const user = userEvent.setup();
  const page = renderHost();
  const text = "Ask *the planner*: [@Old](dispatch://agent/planner-session)";
  const [edge] = blocks.edges ?? [];
  if (edge === undefined) throw new Error("the fixture holds no block");
  page.getReferences.mockResolvedValue({
    ...blocks,
    edges: [{ ...edge, excerpt: { text, text_tree: displayTree(text) } }],
  });

  const comment = await field();
  await user.click(comment);
  await user.keyboard("#relay");
  await waitFor(() => expect(drawnRows()).toContain("Comments: I saw the relay drop one"));
  await user.click(screen.getByRole("button", { name: "Blocks" }));
  await waitFor(() => expect(drawnRows()).toContain("Comment blocks: Ask the planner: @Planner"));
});

// A hit the server sent no tree for is drawn as its words written, cut to one line where no
// reference crosses the cut: a cut that would land in a mention's link, in an address or in a
// bare key moves back to its start, so the row never ends in half of one. A snippet that holds
// none is cut by length, as it always was.
test("a hit's row drawn as written is cut where no reference crosses the cut", async () => {
  const user = userEvent.setup();
  // 108 characters, so a reference written after them crosses the 120th.
  const lead = "relay ".repeat(18);
  const plain = "relay ".repeat(30).trim();
  renderHost({
    searchAnswer: async () =>
      searchResponse([
        commentHit(1, `${lead}[@Planner](dispatch://agent/planner-session) after`),
        commentHit(2, `${lead}dispatch://CORE-1/spec after`),
        commentHit(3, `${lead}CORE-1234567 after`),
        commentHit(4, plain),
      ]),
  });

  const comment = await field();
  await user.click(comment);
  await user.keyboard("#relay");
  const head = `Comments: ${lead.trimEnd()}…`;
  await waitFor(() =>
    expect(drawnRows().filter((row) => row.startsWith("Comments:"))).toEqual([
      head,
      head,
      head,
      `Comments: ${plain.slice(0, 119)}…`,
    ])
  );
});
