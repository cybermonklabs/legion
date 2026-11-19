import { afterEach, expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

import { settle } from "../../__tests__/fake-clock";
import { keyedAttempt } from "../../__tests__/keyed-attempt";
import { ApiError, api } from "../../api/client";
import { legionDaemonQuery } from "../../api/queries";
import { DispatchQueryClient } from "../../api/query-client";
import type { AuthenticatedUser, LegionSetting } from "../../api/types";
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

/** Renders the section against `server`, over a deployment of two projects, ACME and CORE, on
 *  `queryClient`. `human` is what `/auth/whoami` says: a person is offered the field and the
 *  Remove, a bearer is not. A bearer's answer names a kind the browser's own contract does not —
 *  the SPA types the route by the one answer it is meant to get — so this writes the answer a
 *  bearer really receives. */
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

test("a reader who is not a person is shown the daemons and offered no control", async () => {
  const section = await renderSection(legionServer([setting]), false);

  expect(await section.findByRole("cell", { name: "http://127.0.0.1:9731" })).toBeDefined();
  expect(section.queryByLabelText("Legion daemon state URL")).toBeNull();
  expect(section.queryByLabelText("Legion project")).toBeNull();
  expect(section.queryByRole("button", { name: "Save Legion daemon" })).toBeNull();
  expect(section.queryByRole("button", { name: "Remove the Legion daemon of ACME" })).toBeNull();
});

/** `server` with every read it is asked recorded by project, in order, and every write held
 *  until the row answers it (`answer`): the write reaches the server's rows only then. Once
 *  `holdReads` is called, each read answers what the server held when it was asked, but only as
 *  the row lets it go - the oldest first (`releaseRead`), or all (`releaseReads`, which also ends
 *  the holding) - as a read Dispatch took before a write and answers after it does. */
function heldWrites(server: LegionServer): {
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
        await answer.promise;
        return server.put(project, stateUrl);
      },
      remove: async (project) => {
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

// A read of the project asked while its save is out - an event's refresh, another surface - can
// reach Dispatch before the save does, and would answer the URL from before the press over the
// one the reader just saved. It waits for the save's answer, then reads what the save left.
test("a read of a project asked while its daemon's save is out waits for the answer", async () => {
  const user = userEvent.setup();
  const queryClient = new DispatchQueryClient({ defaultOptions: { queries: { retry: false } } });
  const held = heldWrites(legionServer([setting]));
  const putLegionDaemon = spyOn(api, "putLegionDaemon").mockImplementation(held.server.put);
  spies.push(putLegionDaemon);
  const section = await renderSection(held.server, true, queryClient);

  await section.findByRole("cell", { name: "http://127.0.0.1:9731" });
  await user.selectOptions(section.getByLabelText("Legion project"), "ACME");
  await user.click(section.getByLabelText("Legion daemon state URL"));
  await user.paste("https://daemon.example");
  await user.click(section.getByRole("button", { name: "Save Legion daemon" }));
  await waitFor(() =>
    expect(putLegionDaemon).toHaveBeenCalledWith("ACME", "https://daemon.example", keyedAttempt)
  );
  expect(shownUrls()).toEqual(["https://daemon.example"]);

  const asked = held.reads.length;
  await act(async () => {
    void queryClient.invalidateQueries({ queryKey: legionDaemonQuery("ACME").queryKey });
    await settle();
  });
  expect(held.reads.slice(asked)).toEqual([]);
  expect(shownUrls()).toEqual(["https://daemon.example"]);

  await act(async () => {
    held.answer();
  });
  await waitFor(() => expect(held.reads.slice(asked)).toContain("ACME"));
  await waitFor(() => expect(queryClient.isFetching()).toBe(0));
  expect(shownUrls()).toEqual(["https://daemon.example"]);
  expect(document.querySelector('[data-legion-daemon="ACME"] time')?.getAttribute("datetime")).toBe(
    "2026-11-04T06:00:00Z"
  );
});

// A read of the project asked while its removal is out can reach Dispatch before the removal
// does, and would answer the daemon the reader just removed, back in the list. It waits for the
// removal's answer, then reads that the project names none.
test("a read of a project asked while its daemon's removal is out waits for the answer", async () => {
  const user = userEvent.setup();
  const queryClient = new DispatchQueryClient({ defaultOptions: { queries: { retry: false } } });
  const held = heldWrites(legionServer([setting]));
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

  const asked = held.reads.length;
  await act(async () => {
    void queryClient.invalidateQueries({ queryKey: legionDaemonQuery("ACME").queryKey });
    await settle();
  });
  expect(held.reads.slice(asked)).toEqual([]);
  expect(shownUrls()).toEqual([]);

  await act(async () => {
    held.answer();
  });
  await waitFor(() => expect(held.reads.slice(asked)).toContain("ACME"));
  await waitFor(() => expect(queryClient.isFetching()).toBe(0));
  expect(shownUrls()).toEqual([]);
  expect(section.getByText("No project names a Legion daemon yet.")).toBeDefined();
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
  await user.selectOptions(section.getByLabelText("Legion project"), "ACME");
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
