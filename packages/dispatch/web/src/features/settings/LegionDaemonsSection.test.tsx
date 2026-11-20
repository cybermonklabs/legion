import { afterEach, expect, jest, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

import { realClock } from "../../__tests__/fake-clock";
import { keyedAttempt } from "../../__tests__/keyed-attempt";
import { stopTheLateWrite } from "../../__tests__/stop-late-write";
import { userOnFakeClock } from "../../__tests__/user";
import { ApiError, api, NetworkError } from "../../api/client";
import type { AuthenticatedUser, LegionSetting } from "../../api/types";
import { SendStoppedError, stoppedWriteNote } from "../writes/keyed-write";
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

  await user.selectOptions(await section.findByLabelText("Legion project"), "ACME");
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

  await user.selectOptions(await section.findByLabelText("Legion project"), "ACME");
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

  await user.selectOptions(await section.findByLabelText("Legion project"), "ACME");
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

// A daemon's save the reader stopped may have been made: the section says so, and that its Retry
// sends the same `PUT` again, which is the same however often it is made. The section is drawn on
// the real clock, so every read it makes has answered before the clock is held for the save.
test("a daemon save the reader stopped says Retry sends it again, not that it failed", async () => {
  const putLegionDaemon = spyOn(api, "putLegionDaemon").mockRejectedValue(
    new NetworkError("/api/v1/projects/ACME/legion/daemon", new TypeError("Failed to fetch"))
  );
  spies.push(putLegionDaemon);
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const section = await renderSection(legionServer([]), true, queryClient);
  const projectField = await section.findByLabelText("Legion project");

  jest.useFakeTimers();
  try {
    const user = userOnFakeClock();
    await user.selectOptions(projectField, "ACME");
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
