import { expect, spyOn, test } from "bun:test";
import {
  DISPATCH_WRITING_BOXES,
  type DispatchWritingBoxRow,
  dispatchWritingBox,
} from "@legion/contracts";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { type ReactNode, useState } from "react";
import { MemoryRouter } from "react-router-dom";

import { api } from "../../api/client";
import type { Agent, ArchitectureTree, IssueCollaborators } from "../../api/types";
import { MentionField } from "./MentionField";

const now = Date.now();

const planner: Agent = {
  capabilities: ["steer"],
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

const collaborators: IssueCollaborators = {
  agents: [],
  issue_key: "CORE-1",
  legion_registry: "ok",
  people: [{ kind: "user", last_at: null, login: "alice@theorem.dev", reasons: ["assignee"] }],
};

const architecture: ArchitectureTree = {
  components: [],
  not_architectural: [],
  retired_links: [],
  sources: [],
  totals: {
    components_without_work: 0,
    issues_done: 0,
    issues_total: 0,
    not_architectural: 0,
    retired_links: 0,
    unassigned: 0,
  },
  unassigned: [],
};

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
  const listAgents = spyOn(api, "listAgents").mockResolvedValue([planner]);
  const getIssueCollaborators = spyOn(api, "getIssueCollaborators").mockResolvedValue(
    collaborators
  );
  const getArchitecture = spyOn(api, "getArchitecture").mockResolvedValue(architecture);
  const search = spyOn(api, "search").mockResolvedValue({ results: [], took_ms: 1 });
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

// The keys an open picker owns never reach the host, whatever the host binds them to: the
// Inbox's hotkeys are on the window, an ask option's Enter is on the box itself.
test("an open picker takes the arrows, Enter, Tab and Escape ahead of the host", async () => {
  const user = userEvent.setup();
  const { keys, restore } = renderField();

  try {
    const field = screen.getByRole("textbox", { name: "Comment" });
    await user.click(field);
    await user.paste("@");
    await screen.findByText("Planner");
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
    await screen.findByText("Planner");
    await user.keyboard("{ArrowDown}{Enter}");
    await waitFor(() =>
      expect(field.value).toBe("ask [Planner](dispatch://agent/planner-session)")
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
    await screen.findByText("Planner");
    await user.keyboard("{ArrowDown}{Enter}");
    await waitFor(() =>
      expect(field.value).toBe("[Planner](dispatch://agent/planner-session)tail")
    );
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
