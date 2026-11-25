import { afterEach, expect, jest, spyOn, test } from "bun:test";
import { onlineManager, QueryClient, QueryClientProvider } from "@tanstack/react-query";
// biome-ignore lint/style/noRestrictedImports: the in-the-step row submits and checks in one synchronous step, which user-event has no form for
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

import { realClock, settle, tick } from "../../__tests__/fake-clock";
import { heldNotDisabled, pressHeld } from "../../__tests__/held-press";
import { keyedAttempt } from "../../__tests__/keyed-attempt";
import { listOf, pickFromLabelled, textBoxes } from "../../__tests__/list-picks";
import { answerRoute } from "../../__tests__/send-answers";
import { stopTheLateWrite } from "../../__tests__/stop-late-write";
import { userOnFakeClock } from "../../__tests__/user";
import {
  ApiError,
  api,
  NetworkError,
  REQUEST_DEADLINE_MS,
  type WriteBounds,
} from "../../api/client";
import { legionDaemonQuery } from "../../api/queries";
import { DispatchQueryClient } from "../../api/query-client";
import type { AuthenticatedUser, LegionSetting } from "../../api/types";
import { emulateChromiumFocusFixup } from "../issue-page/chromium-focus-fixup";
import { SEND_DEADLINE_MS, SendStoppedError, stoppedWriteNote } from "../writes/keyed-write";
import { writesOf } from "../writes/write-store";
import { LegionDaemonsSection } from "./LegionDaemonsSection";

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

/** `PUT /api/v1/projects/{key}/legion/daemon`, as Save makes it from its request alone
 *  (`legionDaemonRequest`): called with the project, the body and the attempt's bounds. */
function answerDaemonSaves() {
  return answerRoute<
    (project: string, body: { state_url: string }, bounds: WriteBounds) => Promise<LegionSetting>
  >("PUT", "/api/v1/projects/*/legion/daemon");
}

/** `DELETE /api/v1/projects/{key}/legion/daemon`, as a row's Remove makes it from its request
 *  alone (`legionDaemonRemovalRequest`): called with the project, no body and the bounds. */
function answerDaemonRemovals() {
  return answerRoute<(project: string, body: undefined, bounds: WriteBounds) => Promise<void>>(
    "DELETE",
    "/api/v1/projects/*/legion/daemon"
  );
}

/** A write's try that fails at the network, as every try does while Dispatch cannot be reached. */
const failsAtTheNetwork = () =>
  Promise.reject(
    new NetworkError("/api/v1/projects/ACME/legion/daemon", new TypeError("Failed to fetch"))
  );

/** The section's two writes, by the control that makes each: Save, and a row's Remove, which
 *  takes its row away at the press, so its hold is read on the other row's Remove (`start`). */
const savesAndRemovals: { label: string; start: string | undefined }[] = [
  { label: "Save Legion daemon", start: undefined },
  { label: "Remove the Legion daemon of CORE", start: "Remove the Legion daemon of ACME" },
];

/** How many tries `writes` took between them: every call of each. */
function triesOf(
  writes: readonly { readonly mock: { readonly calls: readonly (readonly unknown[])[] } }[]
): number {
  return writes.reduce((count, write) => count + write.mock.calls.length, 0);
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
  const putLegionDaemon = answerDaemonSaves().mockImplementation((project, body) =>
    server.put(project, body.state_url)
  );
  const section = await renderSection(server);

  await pickFromLabelled(user, "Legion project", "ACME · ACME");
  await user.click(section.getByLabelText("Legion daemon state URL"));
  await user.paste("http://127.0.0.1:9731");
  await user.click(section.getByRole("button", { name: "Save Legion daemon" }));

  await waitFor(() =>
    expect(putLegionDaemon).toHaveBeenLastCalledWith(
      "ACME",
      { state_url: "http://127.0.0.1:9731" },
      keyedAttempt
    )
  );
  expect(await section.findByRole("cell", { name: "http://127.0.0.1:9731" })).toBeDefined();
  // The form is empty again, ready for the next project's daemon.
  expect(section.getByLabelText("Legion daemon state URL")).toHaveProperty("value", "");
});

test("removing a project's daemon sends the DELETE and drops its row", async () => {
  const user = userEvent.setup();
  const server = legionServer([setting]);
  const deleteLegionDaemon = answerDaemonRemovals().mockImplementation((project) =>
    server.remove(project)
  );
  const section = await renderSection(server);

  await user.click(
    await section.findByRole("button", { name: "Remove the Legion daemon of ACME" })
  );

  await waitFor(() =>
    expect(deleteLegionDaemon).toHaveBeenLastCalledWith("ACME", undefined, keyedAttempt)
  );
  expect(await section.findByText("No project names a Legion daemon yet.")).toBeDefined();
});

test("Remove hands focus to the preceding daemon's Remove as its row goes", async () => {
  const user = userEvent.setup();
  const server = legionServer([setting, { ...setting, project: "CORE" }]);
  const remove = answerDaemonRemovals().mockImplementation((project) => server.remove(project));
  const section = await renderSection(server);
  const pressed = await section.findByRole("button", { name: "Remove the Legion daemon of CORE" });
  const preceding = section.getByRole("button", { name: "Remove the Legion daemon of ACME" });
  act(() => pressed.focus());
  await user.keyboard("{Enter}");
  await waitFor(() => expect(pressed).not.toBeInTheDocument());
  expect(remove).toHaveBeenCalledTimes(1);
  expect(preceding).toHaveFocus();
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
] as const)("$label, held while a write of the section is out, keeps the focus, and a second press sends nothing", async ({
  label,
  role,
  start,
}) => {
  const user = userEvent.setup();
  // The writes' answer, held until the case refuses it inside `act`.
  const answer = Promise.withResolvers<never>();
  const writes = [
    answerDaemonSaves().mockReturnValue(answer.promise),
    answerDaemonRemovals().mockReturnValue(answer.promise),
  ];
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
      sent: () => triesOf(writes),
      start: start === undefined ? undefined : section.getByRole("button", { name: start }),
    });
  } finally {
    await act(async () => answer.reject(new ApiError(403, { code: "FORBIDDEN", error: "no" })));
  }
});

// Offline, a write of the section is tried under its key until Dispatch answers it: each try
// fails at the network, and the next waits out its backoff, whatever the browser says of the
// network (`networkMode: "always"`). At its deadline (`SEND_DEADLINE_MS`) the write goes late,
// still tried and now stoppable at the shell's indicator, and the section's hold ends there, so a
// write that never answers locks none of its controls. The control the reader pressed keeps the
// focus while it is held and once it is let go, under `emulateChromiumFocusFixup` throughout. A
// Remove takes its row away at the press, so its hold is read on the other row's Remove, which
// the reader moves to.
test.each(
  savesAndRemovals
)("offline, $label is held until its write's deadline, let go there, and keeps the focus throughout", async ({
  label,
  start,
}) => {
  const writes = [
    answerDaemonSaves().mockImplementation(failsAtTheNetwork),
    answerDaemonRemovals().mockImplementation(failsAtTheNetwork),
  ];
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const section = await renderSection(
    legionServer([setting, { ...setting, project: "CORE", state_url: "https://daemon.example" }]),
    true,
    queryClient
  );
  await section.findByRole("cell", { name: "CORE" });
  // The form is filled on the real clock, so every read the section makes has answered before
  // the clock is held for the write.
  if (start === undefined) await fillDaemonForm("http://127.0.0.1:9731");
  const pressed = section.getByRole("button", { name: label }) as HTMLButtonElement;
  const online = onlineManager.isOnline();
  jest.useFakeTimers();
  const stopFocusFixup = emulateChromiumFocusFixup();
  try {
    onlineManager.setOnline(false);
    await pressHeld(userOnFakeClock(), pressed, {
      name: label,
      sent: () => triesOf(writes),
      start: start === undefined ? undefined : section.getByRole("button", { name: start }),
    });

    // A second short of the deadline the write is still within it, and the control still held.
    await tick(SEND_DEADLINE_MS - 1_000);
    expect(writesOf(queryClient).rows()[0]?.status).toBe("sending");
    expect(heldNotDisabled(pressed, label)).toBe(true);
    expect(pressed).toHaveFocus();

    // Past it the write is late and still tried, and the control is let go with the focus on it.
    await tick(1_000);
    expect(writesOf(queryClient).rows()[0]?.status).toBe("late");
    expect(heldNotDisabled(pressed, label)).toBe(false);
    expect(pressed).toHaveFocus();
  } finally {
    // The section and its reads go before the network returns, so no read goes on after the row.
    cleanup();
    queryClient.clear();
    onlineManager.setOnline(online);
    stopFocusFixup();
    realClock();
  }
});

// A write that lands as the browser goes offline has the read of its project paused by the
// network, with no deadline of its own running: the app's client holds a read of an idle query
// until its paused fetch answers (`DispatchQueryClient`). The section holds Save and every
// Remove through that read only up to the request deadline (`REQUEST_DEADLINE_MS`), lets them go
// there with the focus on the control pressed, and says why the daemons are not read yet. Once
// the network returns the project is read, and the note goes.
test.each(
  savesAndRemovals
)("offline, $label whose write landed is let go at the request deadline, and the section says why", async ({
  label,
  start,
}) => {
  const saved = Promise.withResolvers<LegionSetting>();
  const removed = Promise.withResolvers<void>();
  const writes = [
    answerDaemonSaves().mockImplementation(() => saved.promise),
    answerDaemonRemovals().mockImplementation(() => removed.promise),
  ];
  const queryClient = new DispatchQueryClient({ defaultOptions: { queries: { retry: false } } });
  const section = await renderSection(
    legionServer([setting, { ...setting, project: "CORE", state_url: "https://daemon.example" }]),
    true,
    queryClient
  );
  await section.findByRole("cell", { name: "CORE" });
  // The form is filled on the real clock, so every read the section makes has answered before
  // the clock is held for the write.
  if (start === undefined) await fillDaemonForm("http://127.0.0.1:9731");
  const pressed = section.getByRole("button", { name: label }) as HTMLButtonElement;
  const online = onlineManager.isOnline();
  jest.useFakeTimers();
  const stopFocusFixup = emulateChromiumFocusFixup();
  try {
    await pressHeld(userOnFakeClock(), pressed, {
      name: label,
      sent: () => triesOf(writes),
      start: start === undefined ? undefined : section.getByRole("button", { name: start }),
    });

    // The write lands as the network goes: the project's read is asked, and paused.
    onlineManager.setOnline(false);
    await act(async () => {
      saved.resolve({ ...setting, state_url: "http://127.0.0.1:9731" });
      removed.resolve();
    });
    await tick(0);
    expect(queryClient.getQueryState(legionDaemonQuery("ACME").queryKey)?.fetchStatus).toBe(
      "paused"
    );

    // The write landed in the step before, which is when the hold's timer started: two seconds
    // short of the request deadline the control is still held.
    await tick(REQUEST_DEADLINE_MS - 2_000);
    expect(heldNotDisabled(pressed, label)).toBe(true);
    expect(pressed).toHaveFocus();

    // At it the section lets the control go, with the focus on it, and says why.
    await tick(2_000);
    expect(heldNotDisabled(pressed, label)).toBe(false);
    expect(pressed).toHaveFocus();
    expect(section.getByRole("alert").textContent).toBe(
      "Couldn't load the Legion daemons. Dispatch could not be reached."
    );

    // The network returns, the project is read, and the note goes.
    act(() => onlineManager.setOnline(true));
    await waitFor(() => expect(section.queryByRole("alert")).toBeNull());
  } finally {
    // The section and its reads go before the network returns, so no read paused offline goes on.
    cleanup();
    queryClient.clear();
    onlineManager.setOnline(online);
    stopFocusFixup();
    realClock();
  }
});

// A write the reader stops while the browser is offline settles at once, and the read of its
// project that settling asks for is paused by the network. A stopped write is no longer late, so
// a section that held its controls through that read would hold them again, with nothing to end
// it until the network returns. The section lets them go with the stop instead, says the write
// was stopped, and the project is still read once the network returns.
test.each(
  savesAndRemovals
)("offline, $label is let go when its late write is stopped, though the project's read is paused", async ({
  label,
  start,
}) => {
  const writes = [
    answerDaemonSaves().mockImplementation(failsAtTheNetwork),
    answerDaemonRemovals().mockImplementation(failsAtTheNetwork),
  ];
  const queryClient = new DispatchQueryClient({ defaultOptions: { queries: { retry: false } } });
  const section = await renderSection(
    legionServer([setting, { ...setting, project: "CORE", state_url: "https://daemon.example" }]),
    true,
    queryClient
  );
  await section.findByRole("cell", { name: "CORE" });
  if (start === undefined) await fillDaemonForm("http://127.0.0.1:9731");
  const pressed = section.getByRole("button", { name: label }) as HTMLButtonElement;
  const online = onlineManager.isOnline();
  jest.useFakeTimers();
  const stopFocusFixup = emulateChromiumFocusFixup();
  try {
    onlineManager.setOnline(false);
    await pressHeld(userOnFakeClock(), pressed, {
      name: label,
      sent: () => triesOf(writes),
      start: start === undefined ? undefined : section.getByRole("button", { name: start }),
    });
    await stopTheLateWrite(queryClient);

    expect(queryClient.getQueryState(legionDaemonQuery("ACME").queryKey)?.fetchStatus).toBe(
      "paused"
    );
    expect(heldNotDisabled(pressed, label)).toBe(false);
    expect(pressed).toHaveFocus();
    // The alert's paragraph, without its Retry button's name: it is the only alert, so no
    // "not read yet" line stands beside the stop's.
    expect(section.getByRole("alert").querySelector("p")?.textContent).toBe(
      start === undefined
        ? stoppedWriteNote("the Legion daemon of ACME", "saved", {
            method: "PUT",
            stopped: new SendStoppedError(),
          })
        : stoppedWriteNote("the removal of ACME's Legion daemon", "made", {
            method: "DELETE",
            stopped: new SendStoppedError(),
          })
    );
  } finally {
    // The section and its reads go before the network returns, so no read paused offline goes on.
    cleanup();
    queryClient.clear();
    onlineManager.setOnline(online);
    stopFocusFixup();
    realClock();
  }
});

test("a refusal the server made is shown as the server wrote it, and the row goes back", async () => {
  const user = userEvent.setup();
  const server = legionServer([]);
  answerDaemonSaves().mockRejectedValue(
    new ApiError(403, {
      code: "SCOPE_FORBIDDEN",
      error: "this credential names one project and cannot write a deployment setting",
    })
  );
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
  const putLegionDaemon = answerDaemonSaves().mockImplementation((project, body) =>
    server.put(project, body.state_url)
  );
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
  const putLegionDaemon = answerDaemonSaves().mockImplementation((project, body) =>
    server.put(project, body.state_url)
  );
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
    expect(putLegionDaemon).toHaveBeenLastCalledWith(
      "ACME",
      { state_url: "http://127.0.0.1:9731" },
      keyedAttempt
    )
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
  answerDaemonSaves().mockRejectedValue(
    new NetworkError("/api/v1/projects/ACME/legion/daemon", new TypeError("Failed to fetch"))
  );
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
  answerDaemonRemovals().mockRejectedValue(
    new NetworkError("/api/v1/projects/ACME/legion/daemon", new TypeError("Failed to fetch"))
  );
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
  const putLegionDaemon = answerDaemonSaves().mockImplementation((project, body) =>
    held.server.put(project, body.state_url)
  );
  const section = await renderSection(held.server, true, queryClient);

  await section.findByRole("cell", { name: "http://127.0.0.1:9731" });
  await fillDaemonForm("https://daemon.example");
  await user.click(section.getByRole("button", { name: "Save Legion daemon" }));
  await waitFor(() =>
    expect(putLegionDaemon).toHaveBeenCalledWith(
      "ACME",
      { state_url: "https://daemon.example" },
      keyedAttempt
    )
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
  const deleteLegionDaemon = answerDaemonRemovals().mockImplementation((project) =>
    held.server.remove(project)
  );
  const section = await renderSection(held.server, true, queryClient);

  await user.click(
    await section.findByRole("button", { name: "Remove the Legion daemon of ACME" })
  );
  await waitFor(() =>
    expect(deleteLegionDaemon).toHaveBeenCalledWith("ACME", undefined, keyedAttempt)
  );
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
  answerDaemonSaves().mockImplementation((project, body) =>
    held.server.put(project, body.state_url)
  );
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
  const putLegionDaemon = answerDaemonSaves().mockImplementation((project, body) =>
    held.server.put(project, body.state_url)
  );
  const section = await renderSection(held.server, true, queryClient);

  await section.findByRole("cell", { name: "http://127.0.0.1:9731" });
  await pickFromLabelled(user, "Legion project", "ACME · ACME");
  await user.click(section.getByLabelText("Legion daemon state URL"));
  await user.paste("https://daemon.example");
  await user.click(section.getByRole("button", { name: "Save Legion daemon" }));
  await waitFor(() =>
    expect(putLegionDaemon).toHaveBeenCalledWith(
      "ACME",
      { state_url: "https://daemon.example" },
      keyedAttempt
    )
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

/**
 * The two presses of a row where Save of ACME's daemon is made behind an earlier write of it that
 * is late: `first` - Save of `https://first.example`, or ACME's Remove - and then, past that
 * write's deadline and past the try it makes a second after it, Save of `https://second.example`,
 * each pressed with Enter from the focus. Dispatch cannot be reached meanwhile: each try of a
 * write fails at the network, until `reach`, after which each try is taken by the server's rows
 * and named in `taken`, in the order Dispatch took it. Nothing tells the page it can be reached
 * again - no `online`, as when the browser's network was never lost and a gateway comes back - so
 * each write that waits tries again at the end of its own backoff. The section is drawn and filled
 * on the real clock and pressed on the fake one, which the row puts back (`realClock`).
 */
async function saveBehindLateWrite(
  first: "Save Legion daemon" | "Remove the Legion daemon of ACME"
) {
  const server = legionServer([setting]);
  const taken: string[] = [];
  let reachable = false;
  answerDaemonSaves().mockImplementation((project, body) => {
    if (!reachable) return failsAtTheNetwork();
    taken.push(`PUT ${body.state_url}`);
    return server.put(project, body.state_url);
  });
  answerDaemonRemovals().mockImplementation((project) => {
    if (!reachable) return failsAtTheNetwork();
    taken.push("DELETE");
    return server.remove(project);
  });
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const section = await renderSection(server, true, queryClient);
  await section.findByRole("cell", { name: "ACME" });
  await fillDaemonForm(
    first === "Save Legion daemon" ? "https://first.example" : "https://second.example"
  );
  const save = section.getByRole("button", { name: "Save Legion daemon" }) as HTMLButtonElement;
  jest.useFakeTimers();
  const user = userOnFakeClock();
  act(() => section.getByRole("button", { name: first }).focus());
  await user.keyboard("{Enter}");
  // The write tries at 0, 1, 3, 7, 15 and 31 s, each failing at the network; its next try waits
  // its longest backoff, half a minute.
  await tick(SEND_DEADLINE_MS + 2_000);
  expect(writesOf(queryClient).rows()[0]?.status).toBe("late");
  if (first === "Save Legion daemon") {
    await user.clear(section.getByLabelText("Legion daemon state URL"));
    await user.paste("https://second.example");
  }
  act(() => save.focus());
  await user.keyboard("{Enter}");
  await tick(0);
  return {
    queryClient,
    reach: () => {
      reachable = true;
    },
    save,
    taken,
  };
}

// A write of the section past its deadline holds the form no longer, so the reader can make
// another write of the same daemon while the first is still tried. Both go on the daemon's line,
// in press order: the later press waits its turn behind the late write and goes once that one is
// decided. Off the line, the later press would try again a second after Dispatch can be reached,
// long before the late write's next try, half a minute on, which would then land last and undo
// it. On the line, Dispatch takes the two in press order, the section ends on the later press's
// URL, and the focus is on the Save the reader pressed last.
test.each([
  {
    first: "Save Legion daemon",
    inPressOrder: ["PUT https://first.example", "PUT https://second.example"],
  },
  {
    first: "Remove the Legion daemon of ACME",
    inPressOrder: ["DELETE", "PUT https://second.example"],
  },
] as const)("a Save pressed while $first's write of the daemon is late goes after it, and the daemon ends as the Save left it", async ({
  first,
  inPressOrder,
}) => {
  try {
    const { queryClient, reach, save, taken } = await saveBehindLateWrite(first);
    reach();
    // Past the late write's next try, and the later press's write after it.
    await tick(SEND_DEADLINE_MS);
    expect(taken).toEqual([...inPressOrder]);
    expect(writesOf(queryClient).rows()).toEqual([]);
    expect(shownUrls()).toEqual(["https://second.example"]);
    expect(save).toHaveFocus();
  } finally {
    realClock();
  }
});

// The later press waits its turn behind the late write, and no deadline of its own runs while it
// waits: it holds the form no more than the late write does, so a write that never answers locks
// the form no more by a second press than by the first. Save is free, with the focus on it.
test("a Save pressed while an earlier write of its daemon is late waits its turn, and holds the form no more than that write does", async () => {
  try {
    const { queryClient, save } = await saveBehindLateWrite("Save Legion daemon");
    expect(
      writesOf(queryClient)
        .rows()
        .map((row) => row.status)
    ).toEqual(["late", "queued"]);
    expect(heldNotDisabled(save, "Save Legion daemon")).toBe(false);
    expect(save).toHaveFocus();
  } finally {
    realClock();
  }
});
