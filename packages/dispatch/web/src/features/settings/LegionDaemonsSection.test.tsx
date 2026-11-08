import { afterEach, beforeEach, expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";

import { keyedAttempt } from "../../__tests__/keyed-attempt";
import {
  type FakeLiveSockets,
  installFakeLiveSockets,
  snapshotFrame,
} from "../../__tests__/live-socket-harness";
import { ApiError, api } from "../../api/client";
import type { AuthenticatedUser, LegionBoardResponse, LegionSetting } from "../../api/types";
import { LegionDaemonsSection } from "./LegionDaemonsSection";

/** Every spy a test installed, put back when it ends, so one test's identity or answers are
 *  never the next one's. */
const spies: { mockRestore(): void }[] = [];

let sockets: FakeLiveSockets;
beforeEach(() => {
  sockets = installFakeLiveSockets();
});
afterEach(() => {
  cleanup();
  sockets.restore();
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
  list(): Promise<LegionSetting[]>;
  put(project: string, stateUrl: string): Promise<LegionSetting>;
  remove(project: string): Promise<void>;
}

/** The Legion settings routes' server state as the Settings page reaches it: one row per
 *  project, listed by project key as `ListSettings` does; a save of a project that already names
 *  a daemon moves its URL, and a removal drops the row, so the read a write settles into answers
 *  what the write did rather than the fixture it started from. */
function legionServer(initial: readonly LegionSetting[]): LegionServer {
  let rows = initial.map((row) => ({ ...row }));
  return {
    list: async (): Promise<LegionSetting[]> => rows.map((row) => ({ ...row })),
    put: async (project: string, stateUrl: string): Promise<LegionSetting> => {
      const saved: LegionSetting = {
        ...setting,
        project,
        state_url: stateUrl,
        updated_at: "2026-11-04T06:00:00Z",
      };
      rows = rows.some((row) => row.project === project)
        ? rows.map((row) => (row.project === project ? saved : row))
        : [...rows, saved];
      rows.sort((a, b) => (a.project < b.project ? -1 : 1));
      return { ...saved };
    },
    remove: async (project: string): Promise<void> => {
      rows = rows.filter((row) => row.project !== project);
    },
  };
}

/** Renders the section against `server`. `human` is what `/auth/whoami` says: a person is offered
 *  the field and the Remove, a bearer is not. A bearer's answer names a kind the browser's own
 *  contract does not — the SPA types the route by the one answer it is meant to get — so this
 *  writes the answer a bearer really receives. */
async function renderSection(server: LegionServer, human = true) {
  const whoAmI = spyOn(api, "whoAmI").mockResolvedValue(
    human
      ? { event_head: "0", kind: "user", login: "alice" }
      : ({ event_head: "0", kind: "agent", login: "alice" } as unknown as AuthenticatedUser)
  );
  const listLegionSettings = spyOn(api, "listLegionSettings").mockImplementation(server.list);
  const listProjects = spyOn(api, "listProjects").mockResolvedValue([
    { created_at: "2026-09-01T00:00:00Z", key: "ACME", name: "Acme" },
  ]);
  spies.push(whoAmI, listLegionSettings, listProjects);
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
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

test("naming a project's daemon sends the PUT and lists what the server stored", async () => {
  const server = legionServer([]);
  const putLegionSetting = spyOn(api, "putLegionSetting").mockImplementation(server.put);
  spies.push(putLegionSetting);
  const section = await renderSection(server);

  fireEvent.change(await section.findByLabelText("Legion project"), { target: { value: "ACME" } });
  fireEvent.change(section.getByLabelText("Legion daemon state URL"), {
    target: { value: "http://127.0.0.1:9731" },
  });
  fireEvent.click(section.getByRole("button", { name: "Save Legion daemon" }));

  await waitFor(() =>
    expect(putLegionSetting).toHaveBeenLastCalledWith("ACME", "http://127.0.0.1:9731", keyedAttempt)
  );
  expect(await section.findByRole("cell", { name: "http://127.0.0.1:9731" })).toBeDefined();
  // The form is empty again, ready for the next project's daemon.
  expect(section.getByLabelText("Legion daemon state URL")).toHaveProperty("value", "");
});

test("removing a project's daemon sends the DELETE and drops its row", async () => {
  const server = legionServer([setting]);
  const deleteLegionSetting = spyOn(api, "deleteLegionSetting").mockImplementation(server.remove);
  spies.push(deleteLegionSetting);
  const section = await renderSection(server);

  fireEvent.click(await section.findByRole("button", { name: "Remove the Legion daemon of ACME" }));

  await waitFor(() => expect(deleteLegionSetting).toHaveBeenLastCalledWith("ACME", keyedAttempt));
  expect(await section.findByText("No project names a Legion daemon yet.")).toBeDefined();
});

test("a refusal the server made is shown as the server wrote it, and the row goes back", async () => {
  const server = legionServer([]);
  const putLegionSetting = spyOn(api, "putLegionSetting").mockRejectedValue(
    new ApiError(403, {
      code: "SCOPE_FORBIDDEN",
      error: "this credential names one project and cannot write a deployment setting",
    })
  );
  spies.push(putLegionSetting);
  const section = await renderSection(server);

  fireEvent.change(await section.findByLabelText("Legion project"), { target: { value: "ACME" } });
  fireEvent.change(section.getByLabelText("Legion daemon state URL"), {
    target: { value: "http://127.0.0.1:9731" },
  });
  fireEvent.click(section.getByRole("button", { name: "Save Legion daemon" }));

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
  const server = legionServer([]);
  const putLegionSetting = spyOn(api, "putLegionSetting").mockImplementation(server.put);
  spies.push(putLegionSetting);
  const section = await renderSection(server);

  fireEvent.change(await section.findByLabelText("Legion project"), { target: { value: "ACME" } });
  fireEvent.change(section.getByLabelText("Legion daemon state URL"), {
    target: { value: "http://127.0.0.1:9731/legion/v1/state" },
  });
  fireEvent.click(section.getByRole("button", { name: "Save Legion daemon" }));

  expect((await section.findByRole("alert")).textContent).toContain(
    "state_url must be an absolute http(s) URL with a host and no path"
  );
  // No Retry: the URL has to change first.
  expect(section.queryByRole("button", { name: "Retry" })).toBeNull();
  expect(putLegionSetting).not.toHaveBeenCalled();
});

test("a reader who is not a person is shown the daemons and offered no control", async () => {
  const section = await renderSection(legionServer([setting]), false);

  expect(await section.findByRole("cell", { name: "http://127.0.0.1:9731" })).toBeDefined();
  expect(section.queryByLabelText("Legion daemon state URL")).toBeNull();
  expect(section.queryByLabelText("Legion project")).toBeNull();
  expect(section.queryByRole("button", { name: "Save Legion daemon" })).toBeNull();
  expect(section.queryByRole("button", { name: "Remove the Legion daemon of ACME" })).toBeNull();
});

// The settings control beside each daemon's URL (dispatch://INFRA-979; moved from the removed
// Legion view): the settings and their sources ride the project's board answer.

/** A board answer with no cards, carrying `overrides` (the settings and their sources). */
function board(overrides: Partial<LegionBoardResponse> = {}): LegionBoardResponse {
  return {
    columns: [],
    computed_at: "2026-11-03T05:04:00Z",
    header: {
      queue_length: 0,
      read_at: "2026-11-03T05:03:30Z",
      read_error: null,
      read_ok: true,
      slot_cap: 4,
      slots_in_use: 2,
      trees: 2,
    },
    project: "ACME",
    setting_sources: {},
    settings: {},
    ...overrides,
  };
}

/** Renders the section with ACME's daemon named, and answers the control's board subscription
 *  with `body`. */
async function renderControl(body: LegionBoardResponse, human = true) {
  await renderSection(legionServer([setting]), human);
  await waitFor(() => expect(sockets.sockets.length).toBeGreaterThan(0));
  const socket = sockets.latest();
  act(() => socket.open());
  const frame = await waitFor(() => socket.subscribed("legion"));
  act(() => socket.receive(snapshotFrame(frame.sub, body)));
  await screen.findByText("ACME daemon settings");
}

const SETTINGS = {
  settings: { admission_cap: 4, instructions: "work/instructions.md" },
  setting_sources: {
    admission_cap: { at: "2026-11-02T09:00:00Z", by: "bob", source: "route" as const },
    instructions: { at: "2026-10-20T00:00:00Z", by: null, source: "file" as const },
  },
};

test("the settings panel shows each setting's value and where it came from", async () => {
  await renderControl(board(SETTINGS));

  const cap = document.querySelector('[data-legion-setting="admission_cap"]');
  expect(cap?.querySelector("[data-legion-setting-value]")?.textContent).toBe("4");
  expect(cap?.querySelector('[data-legion-setting-source="route"]')?.textContent).toContain(
    "Changed here by bob"
  );
  const instructions = document.querySelector('[data-legion-setting="instructions"]');
  expect(instructions?.querySelector("[data-legion-setting-value]")?.textContent).toBe(
    '"work/instructions.md"'
  );
  expect(instructions?.querySelector('[data-legion-setting-source="file"]')?.textContent).toContain(
    "configuration file"
  );
});

test("a daemon that has not sent its settings says so rather than showing an empty table", async () => {
  await renderControl(board());

  expect(document.querySelector("[data-legion-settings-empty]")?.textContent).toContain(
    "The daemon has not sent its settings yet"
  );
});

// The relay answers the daemon's own body, which keys each verdict by path under `settings`
// (`LegionSettingsChangeResponse` in @legion/contracts/legion-api, the daemon's
// api.SettingsChangeResponse): a fake answering a flat map of paths is a fake no daemon matches,
// which is why the shape is a contract type both ends read. The client hands the panel that
// body with `recorded`, whether the daemon wrote the change into its own store
// (`putLegionProjectSettings`).
test("a change goes to the relay route and shows what the daemon did with the key", async () => {
  const put = spyOn(api, "putLegionProjectSettings").mockResolvedValue({
    recorded: true,
    settings: { admission_cap: { applied: "at-once" } },
  });
  try {
    await renderControl(board(SETTINGS));

    const input = screen.getByLabelText("New value for admission_cap") as HTMLInputElement;
    expect(input.value).toBe("4");
    fireEvent.change(input, { target: { value: "6" } });
    fireEvent.click(screen.getByRole("button", { name: "Change admission_cap" }));

    expect(await screen.findByText("Applied at once")).toBeDefined();
    expect(put).toHaveBeenCalledWith("ACME", { admission_cap: 6 }, expect.anything());
    // The row shows the new value, and that it came from here, by the person who changed it.
    const cap = document.querySelector('[data-legion-setting="admission_cap"]');
    await waitFor(() =>
      expect(cap?.querySelector("[data-legion-setting-value]")?.textContent).toBe("6")
    );
    expect(cap?.querySelector('[data-legion-setting-source="route"]')?.textContent).toContain(
      "Changed here by alice"
    );
  } finally {
    put.mockRestore();
  }
});

test("a refused key shows the daemon's reason and leaves the value as it was", async () => {
  const put = spyOn(api, "putLegionProjectSettings").mockResolvedValue({
    recorded: true,
    settings: { admission_cap: { refused: "a running issue holds a slot" } },
  });
  try {
    await renderControl(board(SETTINGS));

    fireEvent.change(screen.getByLabelText("New value for admission_cap"), {
      target: { value: "1" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Change admission_cap" }));

    expect(await screen.findByText("Refused: a running issue holds a slot")).toBeDefined();
    const cap = document.querySelector('[data-legion-setting="admission_cap"]');
    await waitFor(() =>
      expect(cap?.querySelector("[data-legion-setting-value]")?.textContent).toBe("4")
    );
  } finally {
    put.mockRestore();
  }
});

// The backend's own refusal is the other half of D12's gate: the control is offered to any
// signed-in person, and a caller whose credential names one project is refused the write, which
// `scopeDeployment` answers `403 PROJECT_SCOPE_DEPLOYMENT`. A `503` is not this test's refusal:
// the keyed write retries every 5xx under its key until Dispatch decides it (`retriesUnderKey`),
// so a daemon that cannot be reached is tried again rather than shown.
test("a refusal Dispatch itself made lands on the row it was made from", async () => {
  const put = spyOn(api, "putLegionProjectSettings").mockRejectedValue(
    new ApiError(403, {
      code: "PROJECT_SCOPE_DEPLOYMENT",
      error: "this request's credential may write project ACME and nothing else",
    })
  );
  try {
    await renderControl(board(SETTINGS));

    fireEvent.change(screen.getByLabelText("New value for admission_cap"), {
      target: { value: "6" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Change admission_cap" }));

    expect(
      await screen.findByText(/this request's credential may write project ACME/)
    ).toBeDefined();
    const cap = document.querySelector('[data-legion-setting="admission_cap"]');
    await waitFor(() =>
      expect(cap?.querySelector("[data-legion-setting-value]")?.textContent).toBe("4")
    );
  } finally {
    put.mockRestore();
  }
});

// The daemon applied the change and could not record it in its own store: it answers its own
// verdicts with a 500, which Dispatch relays unchanged (the daemon's
// internal/api/settings_route.go, its `config.NotRecorded` arm) and the client hands the panel
// as `recorded: false`. The change took effect, so the row must not go back to the old value
// and must not read as a failure; what the panel says is that the daemon is running on the new
// value and a restart puts back the old one.
test("a change the daemon applied and could not record stays applied on the row, and says what a restart undoes", async () => {
  const put = spyOn(api, "putLegionProjectSettings").mockResolvedValue({
    recorded: false,
    settings: { admission_cap: { applied: "at-once" } },
  });
  try {
    await renderControl(board(SETTINGS));

    fireEvent.change(screen.getByLabelText("New value for admission_cap"), {
      target: { value: "6" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Change admission_cap" }));

    const answer = await screen.findByText(/running on the new value but could not record it/);
    expect(answer.textContent).toContain("Applied at once");
    expect(screen.queryByText(/Couldn't change admission_cap/)).toBeNull();
    const cap = document.querySelector('[data-legion-setting="admission_cap"]');
    await waitFor(() =>
      expect(cap?.querySelector("[data-legion-setting-value]")?.textContent).toBe("6")
    );
    expect(cap?.querySelector('[data-legion-setting-source="route"]')?.textContent).toContain(
      "Changed here by alice"
    );
  } finally {
    put.mockRestore();
  }
});

test("a value that is not JSON is refused here, and nothing is sent", async () => {
  const put = spyOn(api, "putLegionProjectSettings");
  try {
    await renderControl(board(SETTINGS));

    fireEvent.change(screen.getByLabelText("New value for admission_cap"), {
      target: { value: "six" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Change admission_cap" }));

    expect(await screen.findByText(/That is not JSON/)).toBeDefined();
    expect(put).not.toHaveBeenCalled();
  } finally {
    put.mockRestore();
  }
});

test("a reader who is not a person is shown the settings and offered no control", async () => {
  await renderControl(board(SETTINGS), false);

  expect(document.querySelector('[data-legion-setting="admission_cap"]')).not.toBeNull();
  expect(screen.queryByLabelText("New value for admission_cap")).toBeNull();
  expect(screen.queryByRole("button", { name: "Change admission_cap" })).toBeNull();
});
