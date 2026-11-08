import { expect, type Mock, spyOn, test } from "bun:test";
import type { IssueClaimHolderState } from "@legion/contracts";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor } from "@testing-library/react";
import type { UserEvent } from "@testing-library/user-event";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";

import { api } from "../../api/client";
import type {
  IssueClaim,
  IssueDetails,
  Message,
  SearchResponse,
  SearchResultsPage,
} from "../../api/types";
import { BLOCK_ID_ATTRIBUTE } from "../doc/block-id";
import { KeymapProvider } from "../shell/KeymapProvider";
import { SearchPalette } from "./SearchPalette";
import { handoffBody } from "./send-to-agent";

const words = "find out why the rota is empty and write it up";
const session = "session-one";
const issueKey = "CORE-7";

/** One hit, so the words answer to something and the new-issue offer is not in the list beside
 *  this one: this row is about the agent, not about what the search found. */
function oneHit(): SearchResponse & SearchResultsPage {
  return {
    limit: 20,
    offset: 0,
    reachable: 1,
    results: [
      {
        href: `/issues/${issueKey}`,
        id: issueKey,
        kind: "issue",
        owner: { key: issueKey, kind: "issue", status: "in_progress", title: "The rota" },
        rank: 1,
        snippet: "The <mark>rota</mark> is empty.",
      },
    ],
    took_ms: 1,
    total: 1,
  };
}

const sessionClaim: IssueClaim = {
  actor: { id: session, kind: "session", origin: { session_title: "Implementer" } },
  at: "2026-11-07T06:00:00Z",
};

function issueDetails(
  claim: IssueClaim | null,
  holderState: IssueClaimHolderState | null
): IssueDetails {
  return {
    artifacts: [],
    assignee: null,
    blocked_by: [],
    children: [],
    claim,
    closed_at: null,
    components: { ids: [], inherited_from: null, mode: "inherit", reason: null, unknown: [] },
    created_at: "2026-11-07T05:00:00Z",
    created_by: { id: "alice", kind: "user" },
    external_links: [],
    holder_state: holderState,
    key: issueKey,
    labels: [],
    last_seen: null,
    last_seq: 1,
    number: 7,
    open_asks: [],
    parent: null,
    primary_artifact_id: "artifact-1",
    priority: null,
    progress: { children: null, tasks: null },
    project: "CORE",
    rank: "U",
    referenced_by_count: 0,
    repo: null,
    resolved_repo: null,
    route: null,
    route_holder: null,
    route_status: null,
    route_status_reason: null,
    status: "in_progress",
    takeable_at: null,
    title: "The rota",
    updated_at: "2026-11-07T06:00:00Z",
  };
}

interface Stubs {
  readonly createAgentMessage: Mock<typeof api.createAgentMessage>;
  readonly restore: () => void;
}

/** The bar over the issue page, with the issue read answering the claim given and the send
 *  stubbed. The registry answers nothing: the claim's own `holder_state` is what the issue read
 *  reports, and it is what the bar judges the holder by. */
function openBar(claim: IssueClaim | null, holderState: IssueClaimHolderState | null): Stubs {
  const search = spyOn(api, "search").mockResolvedValue(oneHit());
  const getIssue = spyOn(api, "getIssue").mockResolvedValue(issueDetails(claim, holderState));
  const listAgents = spyOn(api, "listAgents").mockRejectedValue(new Error("no registry"));
  const createAgentMessage = spyOn(api, "createAgentMessage").mockResolvedValue({
    deliveries: [{ delivery: "aside", state: "sent" }],
  } as Message);
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const view = render(
    <MemoryRouter initialEntries={[`/issues/${issueKey}`]}>
      <QueryClientProvider client={queryClient}>
        <KeymapProvider>
          <SearchPalette onClose={() => {}} open />
        </KeymapProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );
  return {
    createAgentMessage,
    restore: () => {
      for (const stub of [search, getIssue, listAgents, createAgentMessage]) {
        stub.mockRestore();
      }
      view.unmount();
      queryClient.clear();
    },
  };
}

/** Puts the words in the box and waits for the hits, so a row still absent after this is absent
 *  because the bar does not offer it and not because the bar has not answered yet. */
async function typeWords(user: UserEvent): Promise<void> {
  await user.click(screen.getByRole("combobox", { name: "Search" }));
  await user.paste(words);
  await waitFor(() => expect(screen.getAllByRole("option").length).toBeGreaterThan(0));
}

/** The writing cursor inside a block of the page behind the bar, as a reader writing in the
 *  document leaves it. The bar reads it on the render that opens it (`scopeAtCursor`), so this
 *  is put in place before `openBar`; it answers what that render will find, and the caller
 *  takes it off the page again. */
function placeCaretInBlock(text: string): { readonly chip: string; readonly remove: () => void } {
  const editable = document.createElement("div");
  editable.setAttribute("contenteditable", "true");
  const block = document.createElement("p");
  block.setAttribute(BLOCK_ID_ATTRIBUTE, "b-1");
  block.textContent = text;
  editable.append(block);
  document.body.append(editable);
  editable.focus();
  const range = document.createRange();
  range.setStart(block.firstChild ?? block, 0);
  range.collapse(true);
  const selection = window.getSelection();
  selection?.removeAllRanges();
  selection?.addRange(range);
  return { chip: `the block “${text}”`, remove: () => editable.remove() };
}

/** Presses one key on the bar's box, which is where the two keys that move the chip act. */
async function pressOnBox(user: UserEvent, key: string): Promise<void> {
  await user.click(screen.getByRole("combobox", { name: "Search" }));
  await user.keyboard(`{${key}}`);
}

const offerLabel = `Send this to the agent on ${issueKey}`;

test("offers the words to the agent holding the claim, and sends nothing until the row is pressed", async () => {
  const user = userEvent.setup();
  const bar = openBar(sessionClaim, "live");
  try {
    await typeWords(user);
    await waitFor(() => expect(screen.getByText(offerLabel)).toBeDefined());
    expect(bar.createAgentMessage).not.toHaveBeenCalled();
  } finally {
    bar.restore();
  }
});

test("approving sends one message naming the issue and the words, and the bar says it was sent", async () => {
  const user = userEvent.setup();
  const bar = openBar(sessionClaim, "live");
  try {
    await typeWords(user);
    await user.click(await waitFor(() => screen.getByText(offerLabel)));

    await waitFor(() => expect(bar.createAgentMessage).toHaveBeenCalled());
    expect(bar.createAgentMessage.mock.calls[0]?.[0]).toBe(session);
    expect(bar.createAgentMessage.mock.calls[0]?.[1]).toEqual({
      body: `${issueKey}: ${words}`,
      delivery: "aside",
    });
    await waitFor(() =>
      expect(screen.getByTestId("search-send-to-agent-said").textContent).toContain(
        `the agent on ${issueKey}`
      )
    );
    // One approval is one message: the row is gone, so there is no second press to make.
    expect(screen.queryByText(offerLabel)).toBeNull();
    expect(bar.createAgentMessage).toHaveBeenCalledTimes(1);
  } finally {
    bar.restore();
  }
});

test("the row is absent with no claim, a person's claim, a holder that stopped, or no registry", async () => {
  const user = userEvent.setup();
  const absent: [string, IssueClaim | null, IssueClaimHolderState | null][] = [
    ["no claim at all", null, null],
    [
      "a person's claim",
      { actor: { id: "alice", kind: "user" }, at: "2026-11-07T06:00:00Z" },
      null,
    ],
    ["a holder that stopped", sessionClaim, "gone"],
    ["a holder that stopped answering", sessionClaim, "unresponsive"],
    ["a registry nothing could read", sessionClaim, null],
  ];
  for (const [what, claim, holderState] of absent) {
    const bar = openBar(claim, holderState);
    try {
      await typeWords(user);
      expect(screen.queryByText(offerLabel), what).toBeNull();
    } finally {
      bar.restore();
    }
  }
});

test("the body names the issue, then the person's words as they typed them", () => {
  expect(handoffBody("CORE-7", undefined, "write the rota up")).toBe("CORE-7: write the rota up");
});

test("Backspace widens the chip to the project, which no agent holds, so the row goes with it", async () => {
  const user = userEvent.setup();
  const bar = openBar(sessionClaim, "live");
  try {
    // Backspace acts on an empty box alone, so it is pressed before the words are typed.
    await pressOnBox(user, "Backspace");
    await typeWords(user);

    expect(screen.getByTestId("search-scope").textContent).toContain("the project CORE");
    expect(screen.queryByText(offerLabel)).toBeNull();
    expect(bar.createAgentMessage).not.toHaveBeenCalled();
  } finally {
    bar.restore();
  }
});

test("Tab narrows the chip to the block, and the message names that block before the words", async () => {
  const block = placeCaretInBlock("The sextant sights the horizon.");
  const user = userEvent.setup();
  const bar = openBar(sessionClaim, "live");
  try {
    await pressOnBox(user, "Tab");
    await typeWords(user);

    expect(screen.getByTestId("search-scope").textContent).toContain(block.chip);
    await user.click(await waitFor(() => screen.getByText(offerLabel)));

    await waitFor(() => expect(bar.createAgentMessage).toHaveBeenCalled());
    expect(bar.createAgentMessage.mock.calls[0]?.[1]).toEqual({
      body: `${issueKey}, ${block.chip}: ${words}`,
      delivery: "aside",
    });
  } finally {
    bar.restore();
    block.remove();
  }
});
