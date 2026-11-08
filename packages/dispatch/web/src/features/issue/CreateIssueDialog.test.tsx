import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
// biome-ignore lint/style/noRestrictedImports: this file keeps fireEvent for the dialog form's own submit, which user-event has no form for
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ReactNode } from "react";
import { MemoryRouter, useLocation } from "react-router-dom";

import { ApiError, api } from "../../api/client";
import type { CreateIssueInput, Issue } from "../../api/types";
import { KeymapProvider } from "../shell/KeymapProvider";
import { CreateIssueDialog } from "./CreateIssueDialog";

const projects = [
  { created_at: "2026-09-10T00:00:00Z", key: "CORE", name: "Core", open_asks: 0, gated_steps: [] },
  {
    created_at: "2026-09-10T00:00:00Z",
    key: "OPS",
    name: "Operations",
    open_asks: 0,
    gated_steps: [],
  },
];

function issue(input: CreateIssueInput, key: string): Issue {
  return {
    closed_at: null,
    created_at: "2026-09-14T00:00:00Z",
    external_links: [],
    key,
    labels: input.labels ?? [],
    pinned: false,
    priority: null,
    project: input.project,
    rank: "a",
    route: null,
    status: "triage",
    title: input.title,
    updated_at: "2026-09-14T00:00:00Z",
  } as unknown as Issue;
}

function CurrentRoute(): ReactNode {
  const location = useLocation();
  return <output data-testid="current-route">{location.pathname}</output>;
}

function renderDialog(
  create: (input: CreateIssueInput) => Promise<Issue>,
  path = "/",
  onClose = () => {}
) {
  const listProjects = spyOn(api, "listProjects").mockResolvedValue(projects);
  // Every row types a title, so the dialog's related-item check is answered here instead of
  // reaching the network.
  const relatedItems = spyOn(api, "relatedItems").mockResolvedValue({
    outcome: "none",
    related: [],
    search: "keywords_only",
  });
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  const user = userEvent.setup();
  const view = render(
    <MemoryRouter initialEntries={[path]}>
      <QueryClientProvider client={queryClient}>
        <KeymapProvider>
          <CurrentRoute />
          <CreateIssueDialog createIssue={create} onClose={onClose} />
        </KeymapProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );
  return {
    user,
    unmount: () => {
      view.unmount();
      queryClient.clear();
      listProjects.mockRestore();
      relatedItems.mockRestore();
    },
  };
}

/** The reason a refused control carries, as a reader meets it: the text of the line its
 *  `aria-describedby` names. */
function refusalOf(control: HTMLElement): string | null {
  const describedBy = control.getAttribute("aria-describedby");
  return describedBy === null ? null : (document.getElementById(describedBy)?.textContent ?? null);
}

test("preselects the route's project, submits title and spec, and opens the created issue", async () => {
  const calls: CreateIssueInput[] = [];
  let closed = 0;
  const view = renderDialog(
    async (input) => {
      calls.push(input);
      return issue(input, "OPS-7");
    },
    "/projects/OPS",
    () => (closed += 1)
  );
  try {
    const select = await screen.findByRole<HTMLSelectElement>("combobox", { name: "Project" });
    await waitFor(() => expect(select.value).toBe("OPS"));
    const create = screen.getByRole("button", { name: "Create" });
    expect(create).toHaveAttribute("aria-disabled", "true");

    await view.user.click(screen.getByRole("textbox", { name: "Title" }));
    await view.user.paste("  Ship the keymap  ");
    expect(create).not.toHaveAttribute("aria-disabled");
    await view.user.click(screen.getByRole("textbox", { name: /Spec/ }));
    await view.user.paste("One line of spec.");
    await view.user.click(create);

    await waitFor(() =>
      expect(screen.getByTestId("current-route").textContent).toBe("/issues/OPS-7")
    );
    expect(calls).toEqual([
      { project: "OPS", spec: "One line of spec.", title: "Ship the keymap" },
    ]);
    expect(closed).toBe(1);
  } finally {
    view.unmount();
  }
});

// A 5xx is no verdict: the write is tried again under its key on its own. Dispatch's refusal is
// one, so the dialog keeps the draft, says why, and Retry asks again.
test("a refused create keeps the dialog and draft, shows the server message, and retries", async () => {
  let attempts = 0;
  const view = renderDialog(async (input) => {
    attempts += 1;
    if (attempts === 1) {
      throw new ApiError(409, { code: "PROJECT_LOCKED", error: "the project is being moved" });
    }
    return issue(input, "CORE-3");
  });
  try {
    await screen.findByRole("combobox", { name: "Project" });
    await view.user.click(screen.getByRole("textbox", { name: "Title" }));
    await view.user.paste("Flaky create");
    // The form's own submit: user-event has no form for it.
    fireEvent.submit(screen.getByRole("dialog", { name: "Create issue" }));

    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent("the project is being moved");
    expect(screen.getByRole<HTMLInputElement>("textbox", { name: "Title" }).value).toBe(
      "Flaky create"
    );

    await view.user.click(screen.getByRole("button", { name: "Retry" }));
    await waitFor(() =>
      expect(screen.getByTestId("current-route").textContent).toBe("/issues/CORE-3")
    );
    expect(attempts).toBe(2);
  } finally {
    view.unmount();
  }
});

// Dispatch made the issue and lost its answer (`409 KEY_COMMITTED`): the dialog has no issue to
// open, so it lets go of the draft and says what was made - and a second press, with the draft
// gone, sends nothing, where the old draft would have made a second issue.
test("a create answered KEY_COMMITTED drops the draft, says the issue was made, and a second press sends nothing", async () => {
  let attempts = 0;
  let closed = 0;
  const view = renderDialog(
    async () => {
      attempts += 1;
      throw new ApiError(409, { code: "KEY_COMMITTED", error: "already made" });
    },
    "/",
    () => {
      closed += 1;
    }
  );
  try {
    await screen.findByRole("combobox", { name: "Project" });
    const title = screen.getByRole<HTMLInputElement>("textbox", { name: "Title" });
    await view.user.click(title);
    await view.user.paste("Made once");
    // The form's own submit: user-event has no form for it.
    fireEvent.submit(screen.getByRole("dialog", { name: "Create issue" }));

    const made = await screen.findByTestId("landed-without-answer");
    expect(made.textContent).toBe(
      "The issue “Made once” was made, but Dispatch lost its answer, so it may take a moment to appear."
    );
    expect(title.value).toBe("");
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
    expect(screen.getByTestId("current-route").textContent).toBe("/");

    // The form's own submit again, with the draft gone: user-event has no form for it.
    fireEvent.submit(screen.getByRole("dialog", { name: "Create issue" }));
    await Bun.sleep(20);
    expect(attempts).toBe(1);
    expect(closed).toBe(0);
  } finally {
    view.unmount();
  }
});

test("a possible duplicate names the candidate and Create anyway forces the create", async () => {
  const calls: CreateIssueInput[] = [];
  const view = renderDialog(async (input) => {
    calls.push(input);
    if (input.force !== true) {
      throw new ApiError(409, {
        code: "POSSIBLE_DUPLICATE",
        error: "possible duplicate of CORE-1: Calibrate the astrolabe",
      });
    }
    return issue(input, "CORE-2");
  });
  try {
    await screen.findByRole("combobox", { name: "Project" });
    await view.user.click(screen.getByRole("textbox", { name: "Title" }));
    await view.user.paste("Astrolabe calibration");
    await view.user.click(screen.getByRole("button", { name: "Create" }));

    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent("CORE-1");
    expect(screen.queryByRole("button", { name: "Retry" })).not.toBeInTheDocument();
    await view.user.click(screen.getByRole("button", { name: "Create anyway" }));

    await waitFor(() =>
      expect(screen.getByTestId("current-route").textContent).toBe("/issues/CORE-2")
    );
    expect(calls.map((call) => call.force)).toEqual([undefined, true]);
  } finally {
    view.unmount();
  }
});

// A draft with no title cannot be filed, so Create refuses it: it stays in the tab order, where
// a greyed-out button would have dropped out of it, sends nothing, and says what is missing.
test("Create refused by an empty draft keeps focus, sends nothing and says why", async () => {
  const calls: CreateIssueInput[] = [];
  const view = renderDialog(async (input) => {
    calls.push(input);
    return issue(input, "CORE-3");
  });
  try {
    await screen.findByRole("combobox", { name: "Project" });
    const create = screen.getByRole("button", { name: "Create" });
    create.focus();
    // A press on a control the refusal already holds, with the focus the line above set:
    // user-event will not click what it reads as unpressable, which is the row's subject.
    fireEvent.click(create);

    expect(calls).toEqual([]);
    expect(document.activeElement).toBe(create);
    expect(refusalOf(create)).toBe("Pick a project and write a title.");
  } finally {
    view.unmount();
  }
});

// The duplicate's override is refused by the same empty draft, and the same way: the title
// cleared after the refusal leaves nothing to file.
test("Create anyway refused by a cleared title keeps focus, sends nothing and says why", async () => {
  const calls: CreateIssueInput[] = [];
  const view = renderDialog(async (input) => {
    calls.push(input);
    throw new ApiError(409, {
      code: "POSSIBLE_DUPLICATE",
      error: "possible duplicate of CORE-1: Calibrate the astrolabe",
    });
  });
  try {
    await screen.findByRole("combobox", { name: "Project" });
    const title = screen.getByRole("textbox", { name: "Title" });
    // The title field's own `change`, written and then cleared, and presses on a refused
    // Create: user-event has no form for a press it reads as unpressable.
    fireEvent.change(title, { target: { value: "Astrolabe calibration" } });
    fireEvent.click(screen.getByRole("button", { name: "Create" }));
    await screen.findByRole("alert");
    fireEvent.change(title, { target: { value: "" } });

    const anyway = screen.getByRole("button", { name: "Create anyway" });
    anyway.focus();
    // A press on a control the refusal already holds, with the focus the line above set.
    fireEvent.click(anyway);

    expect(calls).toHaveLength(1);
    expect(document.activeElement).toBe(anyway);
    expect(refusalOf(anyway)).toBe("Pick a project and write a title.");
  } finally {
    view.unmount();
  }
});
