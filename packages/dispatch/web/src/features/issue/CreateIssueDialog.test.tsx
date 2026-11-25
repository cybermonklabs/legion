import { expect, jest, mock, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
// biome-ignore lint/style/noRestrictedImports: this file keeps fireEvent for the dialog form's own submit, which user-event has no form for
import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import type { UserEvent } from "@testing-library/user-event";
import userEvent from "@testing-library/user-event";
import type { ReactNode } from "react";
import { MemoryRouter, useLocation } from "react-router-dom";

import { realClock, tick } from "../../__tests__/fake-clock";
import { pressHeld } from "../../__tests__/held-press";
import { heldUntilLate, pressesOf } from "../../__tests__/held-until-late";
import { listOf, panelOf, pickNamed, textBoxes } from "../../__tests__/list-picks";
import { stopTheLateWrite } from "../../__tests__/stop-late-write";
import { userOnFakeClock } from "../../__tests__/user";
import { ApiError, api, NetworkError, type WriteBounds } from "../../api/client";
import type { CreateIssueInput, Issue, Project } from "../../api/types";
import { KeymapProvider } from "../shell/KeymapProvider";
import { SendStoppedError, stoppedWriteNote } from "../writes/keyed-write";
import { CreateIssueDialog } from "./CreateIssueDialog";

const projects: Project[] = [
  {
    created_at: "2026-09-10T00:00:00Z",
    key: "CORE",
    name: "Core",
    open_asks: 0,
    gated_steps: [],
    proven_sessions: false,
  },
  {
    created_at: "2026-09-10T00:00:00Z",
    key: "OPS",
    name: "Operations",
    open_asks: 0,
    gated_steps: [],
    proven_sessions: false,
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

/** The dialog, its project list and its related-item check answered here; `readProjects` answers
 *  the project list, at once by default. `create` is each attempt of a create, with its bounds. */
function renderDialog(
  user: UserEvent,
  create: (input: CreateIssueInput, bounds: WriteBounds) => Promise<Issue>,
  path = "/",
  onClose = () => {},
  readProjects: () => Promise<Project[]> = () => Promise.resolve(projects)
) {
  const listProjects = spyOn(api, "listProjects").mockImplementation(readProjects);
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
    queryClient,
    user,
    unmount: () => {
      view.unmount();
      queryClient.clear();
      listProjects.mockRestore();
      relatedItems.mockRestore();
    },
  };
}

/** Waits until the project read has answered and the dialog has picked its first project: the
 *  Project list's trigger is drawn at once with no project, and Create does nothing while no
 *  project is picked. */
async function projectPicked(): Promise<void> {
  const trigger = await screen.findByRole("button", { name: pickNamed("Project") });
  await waitFor(() =>
    expect(trigger).toHaveAccessibleName(`Project ${projects[0]?.key} · ${projects[0]?.name}`)
  );
}

test("preselects the route's project, submits title and spec, and opens the created issue", async () => {
  const calls: CreateIssueInput[] = [];
  let closed = 0;
  const view = renderDialog(
    userEvent.setup(),
    async (input) => {
      calls.push(input);
      return issue(input, "OPS-7");
    },
    "/projects/OPS",
    () => (closed += 1)
  );
  try {
    const trigger = await screen.findByRole("button", { name: pickNamed("Project") });
    await waitFor(() => expect(trigger).toHaveAccessibleName("Project OPS · Operations"));
    const create = screen.getByRole("button", { name: "Create" });
    // With no title there is nothing to create: Create is held, not disabled.
    expect(create.getAttribute("aria-disabled")).toBe("true");
    expect(create).not.toHaveAttribute("disabled");

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
  const view = renderDialog(userEvent.setup(), async (input) => {
    attempts += 1;
    if (attempts === 1) {
      throw new ApiError(409, { code: "PROJECT_LOCKED", error: "the project is being moved" });
    }
    return issue(input, "CORE-3");
  });
  try {
    await projectPicked();
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
  jest.useFakeTimers();
  let attempts = 0;
  let closed = 0;
  const view = renderDialog(
    userOnFakeClock(),
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
    await projectPicked();
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
    await tick(20);
    expect(attempts).toBe(1);
    expect(closed).toBe(0);
  } finally {
    realClock();
    view.unmount();
  }
});

test("a possible duplicate names the candidate and Create anyway forces the create", async () => {
  const calls: CreateIssueInput[] = [];
  const view = renderDialog(userEvent.setup(), async (input) => {
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
    await projectPicked();
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

// While the create is out, the field or the button the reader submitted from is held -
// `aria-disabled`, and a field read-only - never disabled (`pressHeld`). A disabled control drops
// the focus to the page's body in a browser, out of the open modal dialog. So the pressed control
// keeps the focus through the create, and a second press sends nothing.
test.each([
  { label: "Title", name: "Title", press: "{Enter}", role: "textbox" },
  { label: /^Spec/, name: "Spec", press: "{Control>}{Enter}{/Control}", role: "textbox" },
  { label: "Create", name: "Create", press: "{Enter}", role: "button" },
] as const)("$name, held while the create is out, keeps the focus, and a second press sends nothing", async ({
  label,
  name,
  press,
  role,
}) => {
  const calls: CreateIssueInput[] = [];
  // The create's answer, held until the case gives it inside `act`.
  const answer = Promise.withResolvers<Issue>();
  const view = renderDialog(userEvent.setup(), (input) => {
    calls.push(input);
    return answer.promise;
  });
  try {
    await projectPicked();
    await view.user.click(screen.getByRole("textbox", { name: "Title" }));
    await view.user.paste("Held create");
    await pressHeld(view.user, screen.getByRole(role, { name: label }), {
      name,
      press,
      sent: () => calls.length,
    });
  } finally {
    await act(async () =>
      answer.resolve(issue(calls[0] ?? { project: "CORE", title: "Held create" }, "CORE-9"))
    );
    view.unmount();
  }
});

/** The dialog over a create that Dispatch never answers while `refusing` is false - each try
 *  fails at the network, and the next waits out its backoff - and refuses once it is true, and
 *  how many presses made it: the keys its attempts went out under. The case holds the fake
 *  clock, from before the dialog is drawn. */
function slowCreateDialog(refusing: () => boolean) {
  const create = mock((_input: CreateIssueInput, _bounds: WriteBounds) =>
    Promise.reject(
      refusing()
        ? new ApiError(403, { code: "FORBIDDEN", error: "not in this project" })
        : new NetworkError("/api/v1/issues", new TypeError("Failed to fetch"))
    )
  );
  const view = renderDialog(userOnFakeClock(), create);
  return { presses: () => pressesOf([create]), view };
}

/** Files "Slow create" by Enter on Create, and checks Create is held until that create's
 *  deadline and let go past it (`heldUntilLate`), with the create still out. */
async function createUntilLate(
  user: UserEvent,
  queryClient: QueryClient,
  presses: () => number
): Promise<HTMLButtonElement> {
  await projectPicked();
  await user.click(screen.getByRole("textbox", { name: "Title" }));
  await user.paste("Slow create");
  const create = screen.getByRole<HTMLButtonElement>("button", { name: "Create" });
  await heldUntilLate(user, create, {
    label: "New issue “Slow create”",
    name: "Create",
    queryClient,
    sent: presses,
  });
  return create;
}

/** What the dialog says beside Create of a press it refused while "Slow create" is out. */
const STILL_SENDING =
  "Create sent nothing: the issue “Slow create” is still being sent. It opens once Dispatch makes it; if it fails, this dialog says why and you can press Create again.";

/** The dialog's line of a press it refused, if it shows one: the page's route is a status too. */
const refusedLine = () =>
  within(screen.getByRole("dialog", { name: "Create issue" })).queryByRole("status");

// A create that never answers holds the dialog until its deadline (`SEND_DEADLINE_MS`), and no
// longer: past it the create is late and still tried, and the dialog is let go, with the focus on
// the Create the reader pressed. A press then would make a second issue while the first is still
// tried under its key, so it sends nothing, and the line beside Create says why and what comes:
// the first is still being sent, and it opens once Dispatch makes it.
test("Create is let go at its create's deadline, and a press while that create is out sends no second one and says so", async () => {
  jest.useFakeTimers();
  const { presses, view } = slowCreateDialog(() => false);
  try {
    const create = await createUntilLate(view.user, view.queryClient, presses);
    expect(refusedLine()).toBeNull();
    await view.user.keyboard("{Enter}");
    await tick(1_000);
    expect(presses(), "a press while the create is out sent one").toBe(1);
    expect(refusedLine()?.textContent).toBe(STILL_SENDING);
    expect(create).toHaveFocus();
  } finally {
    realClock();
    view.unmount();
  }
});

// The line goes once the first create is decided. Refused, the dialog shows the reason, and Create
// takes a press again, which sends a create of its own.
test("once a create out past its deadline is refused, Create sends a create again", async () => {
  jest.useFakeTimers();
  let refusing = false;
  const { presses, view } = slowCreateDialog(() => refusing);
  try {
    await createUntilLate(view.user, view.queryClient, presses);
    await view.user.keyboard("{Enter}");
    await tick(1_000);
    expect(refusedLine()?.textContent).toBe(STILL_SENDING);

    refusing = true;
    // Past the next try, half a minute at the longest backoff.
    await tick(30_000);
    expect(screen.getByRole("alert")).toHaveTextContent("not in this project");
    expect(refusedLine()).toBeNull();
    await view.user.keyboard("{Enter}");
    await tick(1_000);
    expect(presses(), "a press after the refusal sent no create").toBe(2);
  } finally {
    realClock();
    view.unmount();
  }
});

// INFRA-3135, R3: "Every list of options a reader picks from holds a text box that narrows the
// options." The Create issue dialog's Project is such a list: its options are every project,
// which grow with the deployment. Opened inside the dialog, it holds a text box of its own, and a
// word typed into it leaves only the projects that match. Escape there closes the list alone,
// with the focus back on its trigger inside the dialog; a pick sets the project the issue is
// filed in, and puts the focus back on the trigger too.
test("the Create issue dialog's Project list holds a text box that narrows the projects, and a pick files the issue there", async () => {
  const calls: CreateIssueInput[] = [];
  let closed = 0;
  const view = renderDialog(
    userEvent.setup(),
    async (input) => {
      calls.push(input);
      return issue(input, "OPS-8");
    },
    "/projects/CORE",
    () => (closed += 1)
  );
  try {
    const trigger = await screen.findByRole("button", { name: pickNamed("Project") });
    await waitFor(() => expect(trigger).toHaveAccessibleName("Project CORE · Core"));
    const before = new Set(textBoxes());
    await view.user.click(trigger);
    await waitFor(() =>
      expect(screen.queryAllByRole("option", { name: /^OPS\b/ }).length).toBeGreaterThan(0)
    );
    const own = textBoxes().filter((box) => !before.has(box));
    expect(own, "the Project list holds no text box of its own").toHaveLength(1);
    const list = listOf(own[0] as HTMLElement);
    expect(within(list).getByRole("option", { name: /^CORE\b/ })).toHaveAttribute(
      "aria-selected",
      "true"
    );
    await view.user.type(own[0] as HTMLElement, "oper");
    await waitFor(() =>
      expect(
        within(list).queryAllByRole("option", { name: /^CORE\b/ }),
        `"oper" leaves CORE in the Project list`
      ).toHaveLength(0)
    );
    await view.user.keyboard("{Escape}");
    expect(screen.queryByRole("listbox", { name: "Project options" })).toBeNull();
    expect(screen.getByRole("dialog", { name: "Create issue" })).toBeTruthy();
    expect(trigger).toHaveFocus();
    expect(trigger).toHaveAccessibleName("Project CORE · Core");
    expect(closed).toBe(0);

    await view.user.click(trigger);
    await view.user.click(within(panelOf(trigger)).getByRole("option", { name: /^OPS\b/ }));
    expect(trigger).toHaveFocus();
    expect(trigger).toHaveAccessibleName("Project OPS · Operations");

    await view.user.click(screen.getByRole("textbox", { name: "Title" }));
    await view.user.paste("Crane check");
    await view.user.click(screen.getByRole("button", { name: "Create" }));
    await waitFor(() => expect(calls).toEqual([{ project: "OPS", title: "Crane check" }]));
  } finally {
    view.unmount();
  }
});

// While the create is out the Project list is held: a pick in it would be dropped with no word
// (`onPick`), so neither a press on its trigger nor the trigger's ArrowDown opens it, and the
// focus stays on the trigger, as on `AssigneeControl`'s.
test("while the create is out, a press and the trigger's ArrowDown open no Project list", async () => {
  // The create never answers, so it is out for the whole row.
  const view = renderDialog(userEvent.setup(), () => new Promise<Issue>(() => {}));
  try {
    await projectPicked();
    const trigger = screen.getByRole("button", { name: pickNamed("Project") });
    await view.user.click(screen.getByRole("textbox", { name: "Title" }));
    await view.user.paste("Held create");
    await view.user.click(screen.getByRole("button", { name: "Create" }));
    await waitFor(() => expect(trigger).toHaveAttribute("aria-disabled", "true"));

    await view.user.click(trigger);
    expect(trigger).toHaveFocus();
    expect(trigger).toHaveAttribute("aria-expanded", "false");
    expect(screen.queryByRole("listbox", { name: "Project options" })).toBeNull();

    await view.user.keyboard("{ArrowDown}");
    expect(trigger).toHaveFocus();
    expect(trigger).toHaveAttribute("aria-expanded", "false");
    expect(screen.queryByRole("listbox", { name: "Project options" })).toBeNull();
  } finally {
    view.unmount();
  }
});

// INFRA-3135, round 12: a list whose read is out or failed says so. While the projects are being
// read the Project list says they are loading, and after the read failed that they could not be
// loaded, and it never calls an unread list empty.
test("the Create issue dialog's Project list says its projects are loading, then that they could not be loaded, and never that there are none", async () => {
  const read = Promise.withResolvers<Project[]>();
  const view = renderDialog(
    userEvent.setup(),
    async (input) => issue(input, "CORE-9"),
    "/",
    () => {},
    () => read.promise
  );
  try {
    const trigger = await screen.findByRole("button", { name: pickNamed("Project") });
    await view.user.click(trigger);
    const panel = panelOf(trigger);
    expect(within(panel).getByRole("listbox")).toHaveTextContent("Loading projects…");
    expect(within(panel).queryByText("No projects yet")).toBeNull();

    await act(async () => {
      read.reject(new Error("projects unreachable"));
      await read.promise.catch(() => undefined);
    });
    expect(await within(panel).findByText("Could not load projects.")).toBeTruthy();
    expect(within(panel).queryByText("Loading projects…")).toBeNull();
    expect(within(panel).queryByText("No projects yet")).toBeNull();
  } finally {
    view.unmount();
  }
});

// The Project list is no longer disabled while it has no project to offer, so it can say why;
// that lets nobody file an issue in no project. With no project picked - the read still out,
// failed, or answering none - Create stays disabled whatever the title, and the form's own submit
// sends nothing.
test("with no project picked, Create stays disabled and the dialog files nothing", async () => {
  for (const [state, readProjects] of [
    ["still being read", () => new Promise<Project[]>(() => {})],
    ["failed", () => Promise.reject(new Error("projects unreachable"))],
    ["empty", () => Promise.resolve([])],
  ] as const) {
    const calls: CreateIssueInput[] = [];
    const view = renderDialog(
      userEvent.setup(),
      async (input) => {
        calls.push(input);
        return issue(input, "CORE-9");
      },
      "/",
      () => {},
      readProjects
    );
    try {
      const trigger = await screen.findByRole("button", { name: pickNamed("Project") });
      if (state !== "still being read") {
        await waitFor(() => expect(trigger).toHaveTextContent("Choose a project"));
      }
      expect(trigger, `with the projects ${state}`).not.toHaveAccessibleName(/·/);
      await view.user.click(screen.getByRole("textbox", { name: "Title" }));
      await view.user.paste("Nowhere to file");
      expect(
        screen.getByRole("button", { name: "Create" }),
        `with the projects ${state}`
      ).toHaveAttribute("disabled");
      // The form's own submit: user-event has no form for it.
      fireEvent.submit(screen.getByRole("dialog", { name: "Create issue" }));
      await act(async () => {});
      expect(calls, `with the projects ${state}`).toEqual([]);
    } finally {
      view.unmount();
    }
  }
});

// A create the reader stopped may have made the issue: the dialog says so, and that its Retry
// asks again under the create's key (`retryLast`), rather than a failure it cannot know of.
test("a create the reader stopped says Retry asks again under its key, not that it failed", async () => {
  jest.useFakeTimers();
  const view = renderDialog(userOnFakeClock(), () =>
    Promise.reject(new NetworkError("/api/v1/issues", new TypeError("Failed to fetch")))
  );
  try {
    await projectPicked();
    await view.user.click(screen.getByRole("textbox", { name: "Title" }));
    await view.user.paste("Flaky create");
    // The form's own submit: user-event has no form for it.
    fireEvent.submit(screen.getByRole("dialog", { name: "Create issue" }));
    await stopTheLateWrite(view.queryClient);
    // The alert's paragraph, without its Retry button's name.
    expect(screen.getByRole("alert").querySelector("p")?.textContent).toBe(
      stoppedWriteNote("the new issue", "made", {
        method: "POST",
        stopped: new SendStoppedError(),
      })
    );
  } finally {
    realClock();
    view.unmount();
  }
});
