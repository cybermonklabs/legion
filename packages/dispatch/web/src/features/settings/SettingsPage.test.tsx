import { expect, jest, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { realClock } from "../../__tests__/fake-clock";
import { keyedAttempt } from "../../__tests__/keyed-attempt";
import { stopTheLateWrite } from "../../__tests__/stop-late-write";
import { userOnFakeClock } from "../../__tests__/user";
import { api, NetworkError } from "../../api/client";
import { SendStoppedError, stoppedWriteNote } from "../writes/keyed-write";
import { SettingsPage } from "./SettingsPage";

/** Renders the page, and hands back its query client, which a case that stops a late write
 *  reaches as the shell's indicator does. */
function renderPage(): QueryClient {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <MemoryRouter>
      <QueryClientProvider client={queryClient}>
        <SettingsPage />
      </QueryClientProvider>
    </MemoryRouter>
  );
  return queryClient;
}

/** Every query `SettingsPage` and its child sections issue besides the credential pending and
 *  grants reads each test sets, resolved with empty fixtures so the page settles past its own
 *  loading states. */
function stubUnrelatedQueries() {
  return {
    getDeliverySettings: spyOn(api, "getDeliverySettings").mockResolvedValue(null),
    getSignIns: spyOn(api, "getSignIns").mockResolvedValue({ signins: [] }),
    listAgentTokens: spyOn(api, "listAgentTokens").mockResolvedValue([]),
    listArchitectureSources: spyOn(api, "listArchitectureSources").mockResolvedValue([]),
    listProjectDefaultAssignees: spyOn(api, "listProjectDefaultAssignees").mockResolvedValue([]),
    listProjects: spyOn(api, "listProjects").mockResolvedValue([]),
    listRepoProjects: spyOn(api, "listRepoProjects").mockResolvedValue([]),
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

// A mapping's removal the reader stopped may have been made. The page keeps what it says of a
// failed mapping write from the write's own error handler (`onError`), so a stopped one is told
// apart there: the page says what Retry does with it, not that the removal failed.
test("a mapping removal the reader stopped says Retry makes it again, not that it failed", async () => {
  jest.useFakeTimers();
  const user = userOnFakeClock();
  const unrelated = stubUnrelatedQueries();
  unrelated.listProjects.mockResolvedValue([
    {
      created_at: "2026-09-01T00:00:00Z",
      gated_steps: [],
      key: "CORE",
      name: "Core",
      open_asks: 0,
    },
  ]);
  unrelated.listRepoProjects.mockResolvedValue([
    {
      created_at: "2026-09-01T00:00:00Z",
      created_by: { id: "alice", kind: "user" },
      default: true,
      project: "CORE",
      renamed_to: null,
      repo: "acme/api",
    },
  ]);
  const getCredentialPending = spyOn(api, "getCredentialPending").mockResolvedValue(null);
  const deleteRepoProject = spyOn(api, "deleteRepoProject").mockRejectedValue(
    new NetworkError("/api/v1/repo-projects/acme/api", new TypeError("Failed to fetch"))
  );

  try {
    const queryClient = renderPage();
    queryClient.setQueryData(["whoami"], { event_head: "0", kind: "user", login: "alice" });
    await user.click(await screen.findByRole("button", { name: "Delete mapping for acme/api" }));
    await stopTheLateWrite(queryClient);

    // The alert's paragraph, without its Retry button's name.
    expect(screen.getByRole("alert").querySelector("p")?.textContent).toBe(
      stoppedWriteNote("the removal of acme/api's mapping", "made", {
        again: "Retry",
        method: "DELETE",
        stopped: new SendStoppedError(),
      })
    );
  } finally {
    realClock();
    cleanup();
    getCredentialPending.mockRestore();
    deleteRepoProject.mockRestore();
    for (const spy of Object.values(unrelated)) {
      spy.mockRestore();
    }
  }
});
