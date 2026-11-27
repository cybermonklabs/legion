import { beforeAll, expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { keyedAttempt } from "../../__tests__/keyed-attempt";
import { api } from "../../api/client";
import type { CredentialPendingResponse } from "../../api/types";
import { ADD_SOURCE_PATH } from "../issue/IssueComponentsLine";
import { warmMarkdownRenderer } from "../refs/markdown-engine";
import { ARCHITECTURE_SOURCES_HEADING_ID } from "./ArchitectureSourcesSection";
import { SettingsPage } from "./SettingsPage";

// The rows draw through the shared renderer, whose Markdown engine loads lazily, and they run in
// random order, so the file loads the engine before its first row, whichever row that is.
beforeAll(async () => {
  await warmMarkdownRenderer();
});

function renderPage() {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <MemoryRouter>
      <QueryClientProvider client={queryClient}>
        <SettingsPage />
      </QueryClientProvider>
    </MemoryRouter>
  );
}

/** Every query `SettingsPage` and its child sections issue besides the credential pending and
 *  grants reads each test sets, resolved with empty fixtures so the page settles past its own
 *  loading states. */
function stubUnrelatedQueries() {
  return {
    getDeliverySettings: spyOn(api, "getDeliverySettings").mockResolvedValue(null),
    getLegionDaemon: spyOn(api, "getLegionDaemon").mockResolvedValue(null),
    getSignIns: spyOn(api, "getSignIns").mockResolvedValue({ signins: [] }),
    listAgentTokens: spyOn(api, "listAgentTokens").mockResolvedValue([]),
    listArchitectureSources: spyOn(api, "listArchitectureSources").mockResolvedValue([]),
    listProjectDefaultAssignees: spyOn(api, "listProjectDefaultAssignees").mockResolvedValue([]),
    listProjects: spyOn(api, "listProjects").mockResolvedValue([]),
    whoAmI: spyOn(api, "whoAmI").mockResolvedValue({
      event_head: "0",
      kind: "user",
      login: "alice",
    }),
  };
}

test("hides the Live grants section on a Dispatch with no secrets broker", async () => {
  const unrelated = stubUnrelatedQueries();
  const getCredentialPending = spyOn(api, "getCredentialPending").mockResolvedValue(null);

  try {
    renderPage();

    await waitFor(() => expect(getCredentialPending).toHaveBeenCalledTimes(1));
    await waitFor(() => expect(screen.queryByText("Live grants")).not.toBeInTheDocument());
  } finally {
    cleanup();
    getCredentialPending.mockRestore();
    for (const spy of Object.values(unrelated)) {
      spy.mockRestore();
    }
  }
});

// An issue whose project has no architecture source links to Settings at its Architecture
// sources section (`IssueComponentsLine`'s ADD_SOURCE_PATH); the page opens there, since the app
// routes in the page and the browser's own scroll to a hash never runs (dispatch://INFRA-5200).
test("an address naming a section opens Settings at that section", async () => {
  const unrelated = stubUnrelatedQueries();
  const getCredentialPending = spyOn(api, "getCredentialPending").mockResolvedValue(null);
  const scrolled: string[] = [];
  const scrollIntoView = spyOn(Element.prototype, "scrollIntoView").mockImplementation(function (
    this: Element
  ) {
    scrolled.push(this.id);
  });
  try {
    const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(
      <MemoryRouter initialEntries={[ADD_SOURCE_PATH]}>
        <QueryClientProvider client={queryClient}>
          <SettingsPage />
        </QueryClientProvider>
      </MemoryRouter>
    );
    await waitFor(() => expect(scrolled.at(-1)).toBe(ARCHITECTURE_SOURCES_HEADING_ID));
    expect(screen.getByRole("heading", { name: "Architecture sources" }).id).toBe(
      ARCHITECTURE_SOURCES_HEADING_ID
    );
  } finally {
    cleanup();
    scrollIntoView.mockRestore();
    getCredentialPending.mockRestore();
    for (const spy of Object.values(unrelated)) {
      spy.mockRestore();
    }
  }
});

// On a Dispatch with a secrets broker, Live grants and Sign-ins are drawn above Architecture
// sources once the broker's read answers, which moves the section down after the first scroll;
// the page scrolls to it again once that read has settled (dispatch://INFRA-5200).
test("Settings scrolls to the section again once the broker's read has drawn the sections above it", async () => {
  const unrelated = stubUnrelatedQueries();
  // A Dispatch with a secrets broker, whose list answers only once the test says so.
  const pending = Promise.withResolvers<CredentialPendingResponse>();
  const getCredentialPending = spyOn(api, "getCredentialPending").mockImplementation(
    () => pending.promise
  );
  const getCredentialGrants = spyOn(api, "getCredentialGrants").mockResolvedValue({ grants: [] });
  const scrolled: string[] = [];
  const scrollIntoView = spyOn(Element.prototype, "scrollIntoView").mockImplementation(function (
    this: Element
  ) {
    scrolled.push(this.id);
  });
  try {
    const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(
      <MemoryRouter initialEntries={[ADD_SOURCE_PATH]}>
        <QueryClientProvider client={queryClient}>
          <SettingsPage />
        </QueryClientProvider>
      </MemoryRouter>
    );
    await waitFor(() => expect(scrolled).toEqual([ARCHITECTURE_SOURCES_HEADING_ID]));
    expect(screen.queryByRole("heading", { name: "Live grants" })).not.toBeInTheDocument();

    pending.resolve({ pending: [] });
    expect(await screen.findByRole("heading", { name: "Live grants" })).toBeDefined();
    await waitFor(() =>
      expect(scrolled).toEqual([ARCHITECTURE_SOURCES_HEADING_ID, ARCHITECTURE_SOURCES_HEADING_ID])
    );
  } finally {
    cleanup();
    scrollIntoView.mockRestore();
    getCredentialGrants.mockRestore();
    getCredentialPending.mockRestore();
    for (const spy of Object.values(unrelated)) {
      spy.mockRestore();
    }
  }
});

// A reader who scrolls before the broker's read settles has gone where they meant to: the page
// does not scroll them back to the section when that read draws Live grants and Sign-ins above it
// (dispatch://INFRA-5200). Each of the reader's ways cancels it: a wheel turn, and the scroll
// position moving from where the page's first scroll left it.
test.each([
  ["a wheel turn", () => window.dispatchEvent(new WheelEvent("wheel", { deltaY: 120 }))],
  [
    "the scroll position moving",
    () => {
      Object.defineProperty(window, "scrollY", { configurable: true, value: 400, writable: true });
      window.dispatchEvent(new Event("scroll"));
    },
  ],
])("a reader's own scroll (%s) before the broker's read settles keeps Settings from scrolling them back", async (_way, readerScrolls) => {
  const unrelated = stubUnrelatedQueries();
  const pending = Promise.withResolvers<CredentialPendingResponse>();
  const getCredentialPending = spyOn(api, "getCredentialPending").mockImplementation(
    () => pending.promise
  );
  const getCredentialGrants = spyOn(api, "getCredentialGrants").mockResolvedValue({ grants: [] });
  const scrolled: string[] = [];
  const scrollIntoView = spyOn(Element.prototype, "scrollIntoView").mockImplementation(function (
    this: Element
  ) {
    scrolled.push(this.id);
  });
  const scrollY = Object.getOwnPropertyDescriptor(window, "scrollY");
  try {
    const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(
      <MemoryRouter initialEntries={[ADD_SOURCE_PATH]}>
        <QueryClientProvider client={queryClient}>
          <SettingsPage />
        </QueryClientProvider>
      </MemoryRouter>
    );
    await waitFor(() => expect(scrolled).toEqual([ARCHITECTURE_SOURCES_HEADING_ID]));
    readerScrolls();

    pending.resolve({ pending: [] });
    expect(await screen.findByRole("heading", { name: "Live grants" })).toBeDefined();
    // The render that drew Live grants started the section's own read in the same effects pass as
    // the page's, so that read's answer drawn means the page's effects have run.
    expect(await screen.findByText("No live grants.")).toBeDefined();
    expect(scrolled).toEqual([ARCHITECTURE_SOURCES_HEADING_ID]);
  } finally {
    cleanup();
    if (scrollY === undefined) {
      Reflect.deleteProperty(window, "scrollY");
    } else {
      Object.defineProperty(window, "scrollY", scrollY);
    }
    scrollIntoView.mockRestore();
    getCredentialGrants.mockRestore();
    getCredentialPending.mockRestore();
    for (const spy of Object.values(unrelated)) {
      spy.mockRestore();
    }
  }
});

// A `#` part that is no valid percent-encoding names no section: Settings opens at its top and
// stays usable, rather than the decode throwing from the page's effect (dispatch://INFRA-5200).
test("an address whose # part cannot be decoded opens Settings at its top", async () => {
  const unrelated = stubUnrelatedQueries();
  const getCredentialPending = spyOn(api, "getCredentialPending").mockResolvedValue(null);
  const scrollIntoView = spyOn(Element.prototype, "scrollIntoView").mockImplementation(() => {});
  try {
    const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(
      <MemoryRouter initialEntries={["/settings#%E0%A4%A"]}>
        <QueryClientProvider client={queryClient}>
          <SettingsPage />
        </QueryClientProvider>
      </MemoryRouter>
    );
    await waitFor(() => expect(getCredentialPending).toHaveBeenCalledTimes(1));
    expect(screen.getByRole("heading", { level: 1, name: "Settings" })).toBeDefined();
    expect(screen.getByRole("heading", { name: "Architecture sources" })).toBeDefined();
    expect(scrollIntoView).not.toHaveBeenCalled();
  } finally {
    cleanup();
    scrollIntoView.mockRestore();
    getCredentialPending.mockRestore();
    for (const spy of Object.values(unrelated)) {
      spy.mockRestore();
    }
  }
});

test("lists live grants once credential requests are configured, and Revoke is one plain POST", async () => {
  const user = userEvent.setup();
  const unrelated = stubUnrelatedQueries();
  const getCredentialPending = spyOn(api, "getCredentialPending").mockResolvedValue({
    pending: [],
  });
  const getCredentialGrants = spyOn(api, "getCredentialGrants").mockResolvedValue({
    grants: [
      {
        approver: "sami@example.com",
        created_at: "2026-09-30T00:00:00Z",
        decided_by: "sami@example.com",
        enrollment: {
          kind: "session",
          operator: "sami@example.com",
          runtime_id: "box-1",
          slot: null,
        },
        expires_at: "2026-09-30T01:00:00Z",
        grant_id: "grant-1",
        granted: "approval",
        names: ["DEEL_API_KEY"],
        record_id: "rec-1",
      },
    ],
  });
  const revokeCredentialGrant = spyOn(api, "revokeCredentialGrant").mockResolvedValue(undefined);

  try {
    renderPage();

    expect(await screen.findByText("Live grants")).toBeDefined();
    expect(await screen.findByText("DEEL_API_KEY")).toBeDefined();
    await user.click(screen.getByRole("button", { name: "Revoke" }));
    await waitFor(() => expect(revokeCredentialGrant).toHaveBeenCalledTimes(1));
    expect(revokeCredentialGrant).toHaveBeenCalledWith("grant-1", keyedAttempt);
  } finally {
    cleanup();
    getCredentialPending.mockRestore();
    getCredentialGrants.mockRestore();
    revokeCredentialGrant.mockRestore();
    for (const spy of Object.values(unrelated)) {
      spy.mockRestore();
    }
  }
});

// The list holds every grant of the viewer's sessions, so each row says how it was granted: by the
// policy without asking, or on the approval of a login it names, which can be another person's.
test("names how each grant was granted: automatically, or approved by whom", async () => {
  const unrelated = stubUnrelatedQueries();
  const getCredentialPending = spyOn(api, "getCredentialPending").mockResolvedValue({
    pending: [],
  });
  const enrollment = {
    kind: "session",
    operator: "sami@example.com",
    runtime_id: "example-host-devbox:4242:1",
    slot: null,
  };
  const getCredentialGrants = spyOn(api, "getCredentialGrants").mockResolvedValue({
    grants: [
      {
        approver: "mallory@example.com",
        created_at: "2026-09-30T00:00:00Z",
        decided_by: "mallory@example.com",
        enrollment,
        expires_at: "2026-09-30T01:00:00Z",
        grant_id: "grant-1",
        granted: "approval",
        names: ["DEEL_API_KEY"],
        record_id: "rec-1",
      },
      {
        approver: null,
        created_at: "2026-09-30T00:00:00Z",
        decided_by: null,
        enrollment,
        expires_at: "2026-09-30T01:00:00Z",
        grant_id: "grant-2",
        granted: "automatic",
        names: ["GWS_READ_TOKEN"],
        record_id: null,
      },
    ],
  });

  try {
    renderPage();

    expect(await screen.findByRole("columnheader", { name: "Granted" })).toBeDefined();
    expect(screen.getByRole("cell", { name: "Approved by mallory@example.com" })).toBeDefined();
    expect(screen.getByRole("cell", { name: "Automatically" })).toBeDefined();
    expect(screen.getAllByRole("button", { name: "Revoke" })).toHaveLength(2);
  } finally {
    cleanup();
    getCredentialPending.mockRestore();
    getCredentialGrants.mockRestore();
    for (const spy of Object.values(unrelated)) {
      spy.mockRestore();
    }
  }
});

// Two roles of one pod hold grants on one runtime id, so each row names its enrollment's slot; a
// grant on an enrollment with no slot names none.
test("two grants on one pod read apart by their slots", async () => {
  const unrelated = stubUnrelatedQueries();
  const getCredentialPending = spyOn(api, "getCredentialPending").mockResolvedValue({
    pending: [],
  });
  const grant = (grantId: string, slot: string | null) => ({
    approver: "sami@example.com",
    created_at: "2026-09-30T00:00:00Z",
    decided_by: "sami@example.com",
    enrollment: { kind: "pod", operator: "", runtime_id: "3f9c-pod-uid", slot },
    expires_at: "2026-09-30T01:00:00Z",
    grant_id: grantId,
    granted: "approval" as const,
    names: ["DEEL_API_KEY"],
    record_id: `rec-${grantId}`,
  });
  const getCredentialGrants = spyOn(api, "getCredentialGrants").mockResolvedValue({
    grants: [
      grant("grant-1", "implementer-g1"),
      grant("grant-2", "reviewer-g1"),
      grant("grant-3", null),
    ],
  });

  try {
    renderPage();

    expect(await screen.findByRole("cell", { name: /slot implementer-g1/ })).toBeDefined();
    expect(screen.getByRole("cell", { name: /slot reviewer-g1/ })).toBeDefined();
    const unslotted = screen.getAllByRole("cell", { name: "pod · 3f9c-pod-uid · —" });
    expect(unslotted).toHaveLength(1);
  } finally {
    cleanup();
    getCredentialPending.mockRestore();
    getCredentialGrants.mockRestore();
    for (const spy of Object.values(unrelated)) {
      spy.mockRestore();
    }
  }
});

// A grant that lasts until its session ends has no expiry, which the broker answers as null; its
// row says so rather than a time read from null (1970), and a grant with an expiry still shows it.
test("a grant with no expiry says it ends when its session ends", async () => {
  const unrelated = stubUnrelatedQueries();
  const getCredentialPending = spyOn(api, "getCredentialPending").mockResolvedValue({
    pending: [],
  });
  const grant = (grantId: string, expiresAt: string | null) => ({
    approver: null,
    created_at: "2026-09-30T00:00:00Z",
    decided_by: null,
    enrollment: { kind: "session", operator: "sami@example.com", runtime_id: "box-1", slot: null },
    expires_at: expiresAt,
    grant_id: grantId,
    granted: "automatic" as const,
    names: [grantId === "grant-1" ? "DEEL_API_KEY" : "GWS_READ_TOKEN"],
    record_id: null,
  });
  const getCredentialGrants = spyOn(api, "getCredentialGrants").mockResolvedValue({
    grants: [grant("grant-1", null), grant("grant-2", "2026-09-30T01:00:00Z")],
  });

  try {
    renderPage();

    expect(await screen.findByRole("cell", { name: "When the session ends" })).toBeDefined();
    expect(document.querySelectorAll("time[datetime='2026-09-30T01:00:00Z']")).toHaveLength(1);
    // Two Created times and the one grant's Expires time: the grant with no expiry shows none.
    expect(document.querySelectorAll("time")).toHaveLength(3);
  } finally {
    cleanup();
    getCredentialPending.mockRestore();
    getCredentialGrants.mockRestore();
    for (const spy of Object.values(unrelated)) {
      spy.mockRestore();
    }
  }
});
