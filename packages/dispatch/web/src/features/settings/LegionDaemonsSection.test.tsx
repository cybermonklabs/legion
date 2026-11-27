import { afterEach, beforeAll, expect, jest, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
// biome-ignore lint/style/noRestrictedImports: the in-the-step row submits and checks in one synchronous step, which user-event has no form for
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

import { realClock, settle } from "../../__tests__/fake-clock";
import { pressHeld } from "../../__tests__/held-press";
import { keyedAttempt } from "../../__tests__/keyed-attempt";
import { listOf, pickFromLabelled, textBoxes } from "../../__tests__/list-picks";
import { stopTheLateWrite } from "../../__tests__/stop-late-write";
import { userOnFakeClock } from "../../__tests__/user";
import { ApiError, api, NetworkError } from "../../api/client";
import { legionDaemonQuery } from "../../api/queries";
import { DispatchQueryClient } from "../../api/query-client";
import type { AuthenticatedUser, LegionSetting } from "../../api/types";
import { warmMarkdownRenderer } from "../refs/markdown-engine";
import { SendStoppedError, stoppedWriteNote } from "../writes/keyed-write";
import { LegionDaemonsSection } from "./LegionDaemonsSection";

// The rows draw through the shared renderer, whose Markdown engine loads lazily, and they run in
// random order, so the file loads the engine before its first row, whichever row that is.
beforeAll(async () => {
  await warmMarkdownRenderer();
});

/** Every spy a test installed, put back when it ends, so one test's identity or answers are
 *  never the next one's. */
const spies: { mockRestore(): void }[] = [];

afterEach(() => {
  cleanup();
  for (const spy of spies.splice(0)) spy.mockRestore();
});

const setting: LegionSetting = {
  created_by: { id: "alice", kind: "user" },
  project: "ACME",
  state_url: "http://127.0.0.1:9731",
  updated_at: "2026-11-03T05:04:00Z",
};

/** The three calls the section makes, over rows a test holds. */
interface LegionServer {
  get(project: string): Promise<LegionSetting | null>;
  put(project: string, stateUrl: string): Promise<LegionSetting>;
  remove(project: string): Promise<void>;
}

/** The Legion daemon routes' server state as the Settings page reaches it: at most one row per
 *  project, read one project at a time, `null` for a project that names none; a save of a project
 *  that already names a daemon moves its URL, and a removal drops the row, so the read a write
 *  settles into answers what the write did rather than the fixture it started from. */
function legionServer(initial: readonly LegionSetting[]): LegionServer {
  let rows = initial.map((row) => ({ ...row }));
  return {
    get: async (project: string): Promise<LegionSetting | null> => {
      const row = rows.find((one) => one.project === project);
      return row === undefined ? null : { ...row };
    },
    put: async (project: string, stateUrl: string): Promise<LegionSetting> => {
      const saved: LegionSetting = {
        ...setting,
        project,
        state_url: stateUrl,
        updated_at: "2026-11-04T06:00:00Z",
      };
      rows = [...rows.filter((row) => row.project !== project), saved];
      return { ...saved };
    },
    remove: async (project: string): Promise<void> => {
      rows = rows.filter((row) => row.project !== project);
    },
  };
}

/** Renders the section against `server`, over a deployment of two projects, ACME and CORE, in
 *  `queryClient`, which a case that stops a late write reaches as the shell's indicator does.
 *  `human` is what `/auth/whoami` says: a person is offered the field and the Remove, a bearer is
 *  not. A bearer's answer names a kind the browser's own contract does not — the SPA types the
 *  route by the one answer it is meant to get — so this writes the answer a bearer really
 *  receives. */
async function renderSection(
  server: LegionServer,
  human = true,
  queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } })
) {
  const whoAmI = spyOn(api, "whoAmI").mockResolvedValue(
    human
      ? { event_head: "0", kind: "user", login: "alice" }
      : ({ event_head: "0", kind: "agent", login: "alice" } as unknown as AuthenticatedUser)
  );
  const getLegionDaemon = spyOn(api, "getLegionDaemon").mockImplementation(server.get);
  const listProjects = spyOn(api, "listProjects").mockResolvedValue(
    ["ACME", "CORE"].map((key) => ({
      created_at: "2026-09-01T00:00:00Z",
      gated_steps: [],
      key,
      name: key,
      open_asks: 0,
      proven_sessions: false,
    }))
  );
  spies.push(whoAmI, getLegionDaemon, listProjects);
  spies.push({ mockRestore: () => queryClient.clear() });
  render(
    <QueryClientProvider client={queryClient}>
      <LegionDaemonsSection />
    </QueryClientProvider>
  );
  return within(await screen.findByRole("region", { name: "Legion" }));
}

test("every project that names a daemon is a row of its own", async () => {
  const section = await renderSection(
    legionServer([setting, { ...setting, project: "CORE", state_url: "https://daemon.example" }])
  );

  expect(await section.findByRole("cell", { name: "ACME" })).toBeDefined();
  expect(section.getByRole("cell", { name: "http://127.0.0.1:9731" })).toBeDefined();
  expect(section.getByRole("cell", { name: "https://daemon.example" })).toBeDefined();
  expect(section.queryByText("No project names a Legion daemon yet.")).toBeNull();
});

test("a deployment where no project names a daemon says so", async () => {
  const section = await renderSection(legionServer([]));

  expect(await section.findByText("No project names a Legion daemon yet.")).toBeDefined();
});

// The section says when Dispatch reads a daemon's whole state: to start the project's record or to
// check it, and not on a timer (the Legion state intake, packages/envoy/cmd/dispatch/README.md).
test("the section says Dispatch reads a daemon whole only to start or check its record", async () => {
  const section = await renderSection(legionServer([]));

  expect(
    await section.findByText(
      /Dispatch reads the daemon's whole state only to start that record or to check what it recorded; it does not poll the daemon on a timer\./
    )
  ).toBeDefined();
});

// No route lists every daemon, so one project's read can fail while the others answer: the
// section says it could not load them rather than drawing a table that leaves that project out.
test("a project whose daemon cannot be read fails the list rather than dropping out of it", async () => {
  const server = legionServer([setting]);
  const section = await renderSection({
    ...server,
    get: (project) =>
      project === "CORE"
        ? Promise.reject(new ApiError(503, { code: "UNAVAILABLE", error: "try again" }))
        : server.get(project),
  });

  expect(await section.findByText("Couldn't load the Legion daemons.")).toBeDefined();
  expect(section.queryByRole("cell", { name: "ACME" })).toBeNull();
});

test("naming a project's daemon sends the PUT and lists what the server stored", async () => {
  const user = userEvent.setup();
  const server = legionServer([]);
  const putLegionDaemon = spyOn(api, "putLegionDaemon").mockImplementation(server.put);
  spies.push(putLegionDaemon);
  const section = await renderSection(server);

  await pickFromLabelled(user, "Legion project", "ACME · ACME");
  await user.click(section.getByLabelText("Legion daemon state URL"));
  await user.paste("http://127.0.0.1:9731");
  await user.click(section.getByRole("button", { name: "Save Legion daemon" }));

  await waitFor(() =>
    expect(putLegionDaemon).toHaveBeenLastCalledWith("ACME", "http://127.0.0.1:9731", keyedAttempt)
  );
  expect(await section.findByRole("cell", { name: "http://127.0.0.1:9731" })).toBeDefined();
  // The form is empty again, ready for the next project's daemon.
  expect(section.getByLabelText("Legion daemon state URL")).toHaveProperty("value", "");
});

test("removing a project's daemon sends the DELETE and drops its row", async () => {
  const user = userEvent.setup();
  const server = legionServer([setting]);
  const deleteLegionDaemon = spyOn(api, "deleteLegionDaemon").mockImplementation(server.remove);
  spies.push(deleteLegionDaemon);
  const section = await renderSection(server);

  await user.click(
    await section.findByRole("button", { name: "Remove the Legion daemon of ACME" })
  );

  await waitFor(() => expect(deleteLegionDaemon).toHaveBeenLastCalledWith("ACME", keyedAttempt));
  expect(await section.findByText("No project names a Legion daemon yet.")).toBeDefined();
});

// While a write of the section is out, Save Legion daemon, the address box and every row's Remove
// are held, by `aria-disabled`, never `disabled` (`pressHeld`). A Remove takes its row away at the
// press, so its hold is read on the other row's Remove, which the reader moves to. The control
// keeps the focus, and a second press sends nothing.
test.each([
  { label: "Save Legion daemon", role: "button", start: undefined },
  { label: "Legion daemon state URL", role: "textbox", start: undefined },
  {
    label: "Remove the Legion daemon of CORE",
    role: "button",
    start: "Remove the Legion daemon of ACME",
  },
] as const)(
  "$label, held while a write of the section is out, keeps the focus, and a second press sends nothing",
  async ({ label, role, start }) => {
    const user = userEvent.setup();
    // The writes' answer, held until the case refuses it inside `act`.
    const answer = Promise.withResolvers<never>();
    const writes = [
      spyOn(api, "putLegionDaemon").mockReturnValue(answer.promise),
      spyOn(api, "deleteLegionDaemon").mockReturnValue(answer.promise),
    ];
    spies.push(...writes);
    const section = await renderSection(
      legionServer([setting, { ...setting, project: "CORE", state_url: "https://daemon.example" }])
    );
    try {
      await section.findByRole("cell", { name: "CORE" });
      await pickFromLabelled(user, "Legion project", "ACME · ACME");
      await user.click(section.getByLabelText("Legion daemon state URL"));
      await user.paste("http://127.0.0.1:9731");
      await pressHeld(user, section.getByRole(role, { name: label }), {
        name: label,
        sent: () => writes.reduce((count, write) => count + write.mock.calls.length, 0),
        start: start === undefined ? undefined : section.getByRole("button", { name: start }),
      });
    } finally {
      await act(async () => answer.reject(new ApiError(403, { code: "FORBIDDEN", error: "no" })));
    }
  }
);

test("a refusal the server made is shown as the server wrote it, and the row goes back", async () => {
  const user = userEvent.setup();
  const server = legionServer([]);
  const putLegionDaemon = spyOn(api, "putLegionDaemon").mockRejectedValue(
    new ApiError(403, {
      code: "SCOPE_FORBIDDEN",
      error: "this credential names one project and cannot write a deployment setting",
    })
  );
  spies.push(putLegionDaemon);
  const section = await renderSection(server);

  await pickFromLabelled(user, "Legion project", "ACME · ACME");
  await user.click(section.getByLabelText("Legion daemon state URL"));
  await user.paste("http://127.0.0.1:9731");
  await user.click(section.getByRole("button", { name: "Save Legion daemon" }));

  await waitFor(() =>
    expect(section.getByRole("alert").textContent).toContain(
      "this credential names one project and cannot write a deployment setting"
    )
  );
  expect(section.getByText("No project names a Legion daemon yet.")).toBeDefined();
});

// The field refuses what the route refuses, in the route's own words (`legionStateURL`,
// packages/envoy/internal/dispatch/api/settings_legion.go), rather than sending a URL the server
// is bound to turn down.
test("a URL with a path is refused by the field, in the words the route uses", async () => {
  const user = userEvent.setup();
  const server = legionServer([]);
  const putLegionDaemon = spyOn(api, "putLegionDaemon").mockImplementation(server.put);
  spies.push(putLegionDaemon);
  const section = await renderSection(server);

  await pickFromLabelled(user, "Legion project", "ACME · ACME");
  await user.click(section.getByLabelText("Legion daemon state URL"));
  await user.paste("http://127.0.0.1:9731/legion/v1/state");
  await user.click(section.getByRole("button", { name: "Save Legion daemon" }));

  expect((await section.findByRole("alert")).textContent).toContain(
    "state_url must be an absolute http(s) URL with a host and no path"
  );
  // No Retry: the URL has to change first.
  expect(section.queryByRole("button", { name: "Retry" })).toBeNull();
  expect(putLegionDaemon).not.toHaveBeenCalled();
});

// The Legion project list is a button, which no `required` holds as the select it replaced held
// the form: with no project picked the form files nothing and says to choose one, as the browser
// said for the select, until a project is picked.
test("with no project picked, Save Legion daemon files nothing and says to choose a project", async () => {
  const user = userEvent.setup();
  const server = legionServer([]);
  const putLegionDaemon = spyOn(api, "putLegionDaemon").mockImplementation(server.put);
  spies.push(putLegionDaemon);
  const section = await renderSection(server);

  expect(await section.findByLabelText("Legion project")).toHaveTextContent("Choose a project");
  await user.click(section.getByLabelText("Legion daemon state URL"));
  await user.paste("http://127.0.0.1:9731");
  await user.click(section.getByRole("button", { name: "Save Legion daemon" }));
  await act(async () => {});
  expect(putLegionDaemon, "a daemon was saved for no project").not.toHaveBeenCalled();
  expect(section.getByRole("alert")).toHaveTextContent("Choose a project.");

  await pickFromLabelled(user, "Legion project", "ACME · ACME");
  expect(section.queryByRole("alert")).toBeNull();
  await user.click(section.getByRole("button", { name: "Save Legion daemon" }));
  await waitFor(() =>
    expect(putLegionDaemon).toHaveBeenLastCalledWith("ACME", "http://127.0.0.1:9731", keyedAttempt)
  );
  expect(putLegionDaemon).toHaveBeenCalledTimes(1);
});

// The Legion project list's options are every project, which grow with the deployment. Opened
// from its trigger, the list holds a text box of its own, and a word typed there leaves only the
// projects whose key or name holds it.
test("the Legion daemon's project list holds a text box that narrows its projects", async () => {
  const user = userEvent.setup();
  const section = await renderSection(legionServer([]));

  const trigger = await section.findByLabelText("Legion project");
  const before = new Set(textBoxes());
  await user.click(trigger);
  const own = textBoxes().filter((box) => !before.has(box));
  expect(own, "the Legion project list holds no text box of its own").toHaveLength(1);
  const box = own[0] as HTMLElement;
  expect(box).toHaveAccessibleName("Search projects");
  const list = listOf(box);
  expect(within(list).getAllByRole("option")).toHaveLength(2);
  await user.type(box, "co");
  await waitFor(() =>
    expect(
      within(list).queryAllByRole("option", { name: /^ACME\b/ }),
      `"co" leaves ACME in the Legion project list`
    ).toHaveLength(0)
  );
  expect(
    within(list)
      .getAllByRole("option")
      .map((option) => option.textContent)
  ).toEqual(["CORE · CORE"]);
});

// A daemon's save the reader stopped may have been made: the section says so, and that its Retry
// sends the same `PUT` again, which is the same however often it is made. The section is drawn,
// and its project picked, on the real clock, so every read it makes has answered before the clock
// is held for the save.
test("a daemon save the reader stopped says Retry sends it again, not that it failed", async () => {
  const putLegionDaemon = spyOn(api, "putLegionDaemon").mockRejectedValue(
    new NetworkError("/api/v1/projects/ACME/legion/daemon", new TypeError("Failed to fetch"))
  );
  spies.push(putLegionDaemon);
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const section = await renderSection(legionServer([]), true, queryClient);
  await pickFromLabelled(userEvent.setup(), "Legion project", "ACME · ACME");

  jest.useFakeTimers();
  try {
    const user = userOnFakeClock();
    await user.click(section.getByLabelText("Legion daemon state URL"));
    await user.paste("http://127.0.0.1:9731");
    await user.click(section.getByRole("button", { name: "Save Legion daemon" }));
    await stopTheLateWrite(queryClient);

    // The alert's paragraph, without its Retry button's name.
    expect(section.getByRole("alert").querySelector("p")?.textContent).toBe(
      stoppedWriteNote("the Legion daemon of ACME", "saved", {
        method: "PUT",
        stopped: new SendStoppedError(),
      })
    );
  } finally {
    realClock();
  }
});

// A daemon's removal the reader stopped may have been made. The section keeps what it says of a
// failed write from the write's own error handler (`onError`), so a stopped one is told apart
// there: the section says what Retry does with it, not that the removal failed.
test("a daemon removal the reader stopped says Retry makes it again, not that it failed", async () => {
  const deleteLegionDaemon = spyOn(api, "deleteLegionDaemon").mockRejectedValue(
    new NetworkError("/api/v1/projects/ACME/legion/daemon", new TypeError("Failed to fetch"))
  );
  spies.push(deleteLegionDaemon);
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const section = await renderSection(legionServer([setting]), true, queryClient);
  const remove = await section.findByRole("button", { name: "Remove the Legion daemon of ACME" });

  jest.useFakeTimers();
  try {
    const user = userOnFakeClock();
    await user.click(remove);
    await stopTheLateWrite(queryClient);

    // The alert's paragraph, without its Retry button's name.
    expect(section.getByRole("alert").querySelector("p")?.textContent).toBe(
      stoppedWriteNote("the removal of ACME's Legion daemon", "made", {
        method: "DELETE",
        stopped: new SendStoppedError(),
      })
    );
  } finally {
    realClock();
  }
});

test("a reader who is not a person is shown the daemons and offered no control", async () => {
  const section = await renderSection(legionServer([setting]), false);

  expect(await section.findByRole("cell", { name: "http://127.0.0.1:9731" })).toBeDefined();
  expect(section.queryByLabelText("Legion daemon state URL")).toBeNull();
  expect(section.queryByLabelText("Legion project")).toBeNull();
  expect(section.queryByRole("button", { name: "Save Legion daemon" })).toBeNull();
  expect(section.queryByRole("button", { name: "Remove the Legion daemon of ACME" })).toBeNull();
});

// A person's row draws its Remove, so the daemons' table box takes no Tab stop of its own
// (`TableShell`'s `holdsControls`). A reader who is not a person is offered no Remove, and with no
// daemon there is no row: either way the box holds no control and keeps the browser's default.
test("the daemons' box takes tabindex -1 while a person's row draws Remove", async () => {
  const section = await renderSection(legionServer([setting]));
  const remove = await section.findByRole("button", { name: "Remove the Legion daemon of ACME" });
  expect(remove.closest('[data-control="table-shell"]')?.getAttribute("tabindex")).toBe("-1");
});

test("the daemons' box takes no tabindex for a reader who is not a person", async () => {
  const reader = await renderSection(legionServer([setting]), false);
  const url = await reader.findByRole("cell", { name: "http://127.0.0.1:9731" });
  expect(url.closest('[data-control="table-shell"]')?.hasAttribute("tabindex")).toBe(false);
});

test("the daemons' box takes no tabindex while no project names a daemon", async () => {
  const section = await renderSection(legionServer([]));
  const empty = await section.findByText("No project names a Legion daemon yet.");
  expect(empty.closest('[data-control="table-shell"]')?.hasAttribute("tabindex")).toBe(false);
});

/** `server` with every read it is asked recorded by project, in order, and every write held
 *  until the row answers it (`answer`): the write reaches the server's rows only then, or, with
 *  `savedFirst`, at once, its answer alone held. Once `holdReads` is called, each read answers
 *  what the server held when it was asked, but only as the row lets it go - the oldest first
 *  (`releaseRead`), or all (`releaseReads`, which also ends the holding) - as a read Dispatch took
 *  before a write and answers after it does. */
function heldWrites(
  server: LegionServer,
  { savedFirst = false }: { readonly savedFirst?: boolean } = {}
): {
  answer: () => void;
  holdReads: () => void;
  reads: string[];
  releaseRead: () => void;
  releaseReads: () => void;
  server: LegionServer;
} {
  const reads: string[] = [];
  const answer = Promise.withResolvers<void>();
  const heldReads: (() => void)[] = [];
  let holding = false;
  return {
    answer: () => answer.resolve(),
    holdReads: () => {
      holding = true;
    },
    reads,
    releaseRead: () => heldReads.shift()?.(),
    releaseReads: () => {
      holding = false;
      for (const release of heldReads.splice(0)) release();
    },
    server: {
      get: async (project) => {
        reads.push(project);
        // The server's answer as the read reaches it, before the row lets it go.
        const row = await server.get(project);
        if (holding) {
          const released = Promise.withResolvers<void>();
          heldReads.push(released.resolve);
          await released.promise;
        }
        return row;
      },
      put: async (project, stateUrl) => {
        if (savedFirst) {
          const saved = await server.put(project, stateUrl);
          await answer.promise;
          return saved;
        }
        await answer.promise;
        return server.put(project, stateUrl);
      },
      remove: async (project) => {
        if (savedFirst) {
          await server.remove(project);
          await answer.promise;
          return;
        }
        await answer.promise;
        return server.remove(project);
      },
    },
  };
}

/** Each daemon state URL the section lists, top to bottom. */
function shownUrls(): (string | null)[] {
  return Array.from(
    document.querySelectorAll("[data-legion-daemon-url]"),
    (cell) => cell.textContent
  );
}

/** Names `url` as ACME's daemon through the section's form, without pressing Save. */
async function fillDaemonForm(url: string) {
  const user = userEvent.setup();
  await pickFromLabelled(user, "Legion project", "ACME · ACME");
  await user.click(screen.getByLabelText("Legion daemon state URL"));
  await user.paste(url);
}

// Dispatch takes this page's save at once, and another browser then names a daemon of its own for
// the project, which this page hears (an event refreshes the project) while its save's answer is
// still on the way. No live event names the daemon this page wrote, so a hold would keep that
// read back until the answer, however late: the save holds nothing, and the read asked then goes
// at once and shows the other browser's daemon. Once the late answer lands, the page reads the
// project again rather than put its own URL back.
test("another browser's daemon of the project shows while this page's save is out, and the save's late answer does not undo it", async () => {
  const user = userEvent.setup();
  const queryClient = new DispatchQueryClient({ defaultOptions: { queries: { retry: false } } });
  const server = legionServer([setting]);
  const held = heldWrites(server, { savedFirst: true });
  const putLegionDaemon = spyOn(api, "putLegionDaemon").mockImplementation(held.server.put);
  spies.push(putLegionDaemon);
  const section = await renderSection(held.server, true, queryClient);

  await section.findByRole("cell", { name: "http://127.0.0.1:9731" });
  await fillDaemonForm("https://daemon.example");
  await user.click(section.getByRole("button", { name: "Save Legion daemon" }));
  await waitFor(() =>
    expect(putLegionDaemon).toHaveBeenCalledWith("ACME", "https://daemon.example", keyedAttempt)
  );
  expect(shownUrls()).toEqual(["https://daemon.example"]);

  // The other browser's save, and the event's refresh of this page's project.
  await server.put("ACME", "https://elsewhere.example");
  const asked = held.reads.length;
  await act(async () => {
    void queryClient.invalidateQueries({ queryKey: legionDaemonQuery("ACME").queryKey });
    await settle();
  });
  expect(held.reads.slice(asked)).toContain("ACME");
  await waitFor(() => expect(shownUrls()).toEqual(["https://elsewhere.example"]));
  expect(queryClient.isMutating()).toBe(1);

  await act(async () => {
    held.answer();
  });
  await waitFor(() => expect(queryClient.isMutating()).toBe(0));
  await waitFor(() => expect(queryClient.isFetching()).toBe(0));
  expect(shownUrls()).toEqual(["https://elsewhere.example"]);
});

// Dispatch takes this page's removal at once, and another browser then names a daemon for the
// project again, which this page hears while the removal's answer is still on the way. The read
// asked then goes at once and shows that daemon, and the removal's late answer leaves it there.
test("another browser's daemon of the project shows while this page's removal is out, and the removal's late answer does not undo it", async () => {
  const user = userEvent.setup();
  const queryClient = new DispatchQueryClient({ defaultOptions: { queries: { retry: false } } });
  const server = legionServer([setting]);
  const held = heldWrites(server, { savedFirst: true });
  const deleteLegionDaemon = spyOn(api, "deleteLegionDaemon").mockImplementation(
    held.server.remove
  );
  spies.push(deleteLegionDaemon);
  const section = await renderSection(held.server, true, queryClient);

  await user.click(
    await section.findByRole("button", { name: "Remove the Legion daemon of ACME" })
  );
  await waitFor(() => expect(deleteLegionDaemon).toHaveBeenCalledWith("ACME", keyedAttempt));
  expect(shownUrls()).toEqual([]);

  // The other browser's save, and the event's refresh of this page's project.
  await server.put("ACME", "https://elsewhere.example");
  const asked = held.reads.length;
  await act(async () => {
    void queryClient.invalidateQueries({ queryKey: legionDaemonQuery("ACME").queryKey });
    await settle();
  });
  expect(held.reads.slice(asked)).toContain("ACME");
  await waitFor(() => expect(shownUrls()).toEqual(["https://elsewhere.example"]));
  expect(queryClient.isMutating()).toBe(1);

  await act(async () => {
    held.answer();
  });
  await waitFor(() => expect(queryClient.isMutating()).toBe(0));
  await waitFor(() => expect(queryClient.isFetching()).toBe(0));
  expect(shownUrls()).toEqual(["https://elsewhere.example"]);
});

// The save shows its row in the step of the press: whatever reads the project then - a second
// press, an event's refresh - reads the URL just saved, not the one from before it.
test("a daemon's save is in the project's query in the step of the press", async () => {
  const queryClient = new DispatchQueryClient({ defaultOptions: { queries: { retry: false } } });
  const held = heldWrites(legionServer([setting]));
  const putLegionDaemon = spyOn(api, "putLegionDaemon").mockImplementation(held.server.put);
  spies.push(putLegionDaemon);
  const section = await renderSection(held.server, true, queryClient);

  await section.findByRole("cell", { name: "http://127.0.0.1:9731" });
  await fillDaemonForm("https://daemon.example");
  // The form's own submit, and the check in the same step: user-event has no form for it.
  fireEvent.submit(
    section.getByLabelText("Legion daemon state URL").closest("form") as HTMLFormElement
  );
  expect(queryClient.getQueryData(legionDaemonQuery("ACME").queryKey)).toMatchObject({
    project: "ACME",
    state_url: "https://daemon.example",
  });

  await act(async () => {
    held.answer();
  });
  await waitFor(() => expect(queryClient.isMutating()).toBe(0));
  await waitFor(() => expect(queryClient.isFetching()).toBe(0));
  expect(shownUrls()).toEqual(["https://daemon.example"]);
});

// A read of the project out when a save's answer lands, asked while the save was out, would put
// the URL from before the press back over the one the reader just saved, if Dispatch took it
// before the save. Each read here answers what the server held when it reached it.
test("a read of a project out when its daemon's save is answered does not put back the old URL", async () => {
  const user = userEvent.setup();
  const queryClient = new DispatchQueryClient({ defaultOptions: { queries: { retry: false } } });
  const held = heldWrites(legionServer([setting]));
  const putLegionDaemon = spyOn(api, "putLegionDaemon").mockImplementation(held.server.put);
  spies.push(putLegionDaemon);
  const section = await renderSection(held.server, true, queryClient);

  await section.findByRole("cell", { name: "http://127.0.0.1:9731" });
  await pickFromLabelled(user, "Legion project", "ACME · ACME");
  await user.click(section.getByLabelText("Legion daemon state URL"));
  await user.paste("https://daemon.example");
  await user.click(section.getByRole("button", { name: "Save Legion daemon" }));
  await waitFor(() =>
    expect(putLegionDaemon).toHaveBeenCalledWith("ACME", "https://daemon.example", keyedAttempt)
  );

  // Asked while the save is out, as an event's refresh would ask it.
  held.holdReads();
  await act(async () => {
    void queryClient.invalidateQueries({ queryKey: legionDaemonQuery("ACME").queryKey });
    await settle();
  });
  await act(async () => {
    held.answer();
  });
  // The answer is in: the form is emptied for the next project.
  await waitFor(() =>
    expect(section.getByLabelText("Legion daemon state URL")).toHaveProperty("value", "")
  );
  expect(shownUrls()).toEqual(["https://daemon.example"]);

  // The oldest read still out answers.
  await act(async () => {
    held.releaseRead();
    await settle();
  });
  expect(shownUrls()).toEqual(["https://daemon.example"]);

  await act(async () => {
    held.releaseReads();
    await settle();
  });
  await waitFor(() => expect(queryClient.isFetching()).toBe(0));
  expect(shownUrls()).toEqual(["https://daemon.example"]);
});
