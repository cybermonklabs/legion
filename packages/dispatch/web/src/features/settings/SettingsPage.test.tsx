import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { keyedAttempt } from "../../__tests__/keyed-attempt";
import { ApiError, api } from "../../api/client";
import { whoAmIQuery } from "../../api/queries";
import type { AuthenticatedUser } from "../../api/types";
import { credentialGrantsQuery } from "../credentials/grants";
import { credentialPendingQuery } from "../credentials/pending";
import { SettingsPage } from "./SettingsPage";

/** The page, signed in as `user` through the identity the shell already read, or with no
 *  identity read yet when none is given; it answers the page's query client. A given identity is
 *  fresh for the whole test: every section that reads `/auth/whoami` (the restricted gate,
 *  `useIsHuman`) reads that answer, as the server would answer it again, and no refetch swaps in
 *  the full member `stubUnrelatedQueries` answers for a test that names nobody. */
function renderPage(user?: AuthenticatedUser): QueryClient {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  if (user !== undefined) {
    queryClient.setQueryDefaults(whoAmIQuery().queryKey, { staleTime: Number.POSITIVE_INFINITY });
    queryClient.setQueryData(whoAmIQuery().queryKey, user);
  }
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
    getLegionDaemon: spyOn(api, "getLegionDaemon").mockResolvedValue(null),
    getSignIns: spyOn(api, "getSignIns").mockResolvedValue({ signins: [] }),
    listAccessGroups: spyOn(api, "listAccessGroups").mockResolvedValue([]),
    listAgentTokens: spyOn(api, "listAgentTokens").mockResolvedValue([]),
    listArchitectureSources: spyOn(api, "listArchitectureSources").mockResolvedValue([]),
    listProjectDefaultAssignees: spyOn(api, "listProjectDefaultAssignees").mockResolvedValue([]),
    listProjects: spyOn(api, "listProjects").mockResolvedValue([]),
    listRepoProjects: spyOn(api, "listRepoProjects").mockResolvedValue([]),
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

// The server reads its broker setting once, at boot, so a credential list answered `null` (no
// secrets broker) is asked again only after a page load or a gap in the event feed
// (`reaskHeldCredentialList`). Settings asks the list to learn whether to draw the broker
// sections; a refresh of the list, the one every query takes when the feed refreshes them all,
// asks nothing more.
test("a full member's Settings asks a Dispatch with no broker for the credential list once", async () => {
  const unrelated = stubUnrelatedQueries();
  const getCredentialPending = spyOn(api, "getCredentialPending").mockResolvedValue(null);

  try {
    const queryClient = renderPage({ event_head: "0", kind: "user", login: "alice" });

    await waitFor(() => expect(getCredentialPending).toHaveBeenCalledTimes(1));
    expect(await screen.findByText("Architecture sources")).toBeDefined();
    await queryClient.invalidateQueries({ queryKey: credentialPendingQuery().queryKey });
    expect(getCredentialPending).toHaveBeenCalledTimes(1);
    expect(screen.queryByText("Live grants")).not.toBeInTheDocument();
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

// Groups are made and kept in Settings, beside the projects whose Access tabs name them.
test("lists every group under Groups, right after Projects", async () => {
  const unrelated = stubUnrelatedQueries();
  unrelated.listAccessGroups.mockResolvedValue([
    { members: ["carol@example.com"], name: "crew", not_signed_in: [], owner: "alice" },
  ]);
  const getCredentialPending = spyOn(api, "getCredentialPending").mockResolvedValue(null);

  try {
    renderPage();

    expect(await screen.findByRole("cell", { name: "crew" })).toBeDefined();
    const headings = screen.getAllByRole("heading", { level: 2 }).map((h) => h.textContent);
    expect(headings.slice(0, 2)).toEqual(["Projects", "Groups"]);
  } finally {
    cleanup();
    getCredentialPending.mockRestore();
    for (const spy of Object.values(unrelated)) {
      spy.mockRestore();
    }
  }
});

/** The page's section headings, in the order it draws them. */
function sectionHeadings(): (string | null)[] {
  return screen.getAllByRole("heading", { level: 2 }).map((heading) => heading.textContent);
}

// A full member's Settings is the one it was before member kinds existed: every section, each
// with its own read, the credential list among them deciding whether the two broker sections
// are drawn.
test("a full member's Settings draws every section from the reads it always made", async () => {
  const unrelated = stubUnrelatedQueries();
  const getCredentialPending = spyOn(api, "getCredentialPending").mockResolvedValue({
    pending: [],
  });
  const getCredentialGrants = spyOn(api, "getCredentialGrants").mockResolvedValue({ grants: [] });

  try {
    renderPage({ event_head: "0", kind: "user", login: "alice" });

    expect(await screen.findByText("Sign-ins")).toBeDefined();
    await waitFor(() => expect(unrelated.getDeliverySettings).toHaveBeenCalled());
    expect(sectionHeadings()).toEqual([
      "Projects",
      "Groups",
      "Agent tokens",
      "Live grants",
      "Sign-ins",
      "Repositories → Projects",
      "Architecture sources",
      "Delivery timeline",
      "Preview",
      "Legion",
    ]);
    expect(getCredentialPending).toHaveBeenCalled();
    expect(unrelated.listAccessGroups).toHaveBeenCalled();
  } finally {
    cleanup();
    getCredentialPending.mockRestore();
    getCredentialGrants.mockRestore();
    for (const spy of Object.values(unrelated)) {
      spy.mockRestore();
    }
  }
});

// A restricted member reads only the projects that name them, and Dispatch refuses them every
// read of the deployment's own rows (403 DEPLOYMENT_READ_REFUSED): the groups, the delivery
// settings, the credential list and the Legion daemon setting. Their Settings asks for none of them
// and draws no Groups, no Delivery timeline and no Legion. Their own grants and sign-ins are theirs
// to read, so those two sections stand on the grants read itself, which answers wherever this
// Dispatch has a secrets broker. Preview is kept in the browser and reads nothing.
test("a restricted member's Settings asks no deployment read, and keeps their own grants and sign-ins", async () => {
  const unrelated = stubUnrelatedQueries();
  const getCredentialPending = spyOn(api, "getCredentialPending").mockResolvedValue({
    pending: [],
  });
  const getCredentialGrants = spyOn(api, "getCredentialGrants").mockResolvedValue({ grants: [] });

  try {
    renderPage({
      event_head: "0",
      kind: "user",
      login: "bob@outside.example",
      member_kind: "restricted",
    });

    expect(await screen.findByText("Sign-ins")).toBeDefined();
    expect(await screen.findByText("Architecture sources")).toBeDefined();
    expect(sectionHeadings()).toEqual([
      "Projects",
      "Agent tokens",
      "Live grants",
      "Sign-ins",
      "Repositories → Projects",
      "Architecture sources",
      "Preview",
    ]);
    expect(getCredentialPending).not.toHaveBeenCalled();
    expect(unrelated.listAccessGroups).not.toHaveBeenCalled();
    expect(unrelated.getDeliverySettings).not.toHaveBeenCalled();
    expect(unrelated.getLegionDaemon).not.toHaveBeenCalled();
  } finally {
    cleanup();
    getCredentialPending.mockRestore();
    getCredentialGrants.mockRestore();
    for (const spy of Object.values(unrelated)) {
      spy.mockRestore();
    }
  }
});

// A person ends their own sign-in and revokes their own grant whatever their member kind: Dispatch
// takes a restricted member's revoke of either to the secrets broker, which decides whose each one
// is, as it does a full member's. So their Settings offers Revoke on each, and a press sends it.
test("a restricted member's Settings revokes their own grant and sign-in", async () => {
  const user = userEvent.setup();
  const unrelated = stubUnrelatedQueries();
  unrelated.getSignIns.mockResolvedValue({
    signins: [
      {
        created_at: "2026-09-30T00:00:00Z",
        last_used_at: "2026-09-30T01:00:00Z",
        machine: "outside-box",
        signin_id: "signin-1",
        thumbprint: "thumbprint-of-the-box-key",
      },
    ],
  });
  const getCredentialGrants = spyOn(api, "getCredentialGrants").mockResolvedValue({
    grants: [
      {
        approver: null,
        created_at: "2026-09-30T00:00:00Z",
        decided_by: null,
        enrollment: {
          kind: "session",
          operator: "bob@outside.example",
          runtime_id: "outside-box:4242:1",
          slot: null,
        },
        expires_at: "2026-09-30T01:00:00Z",
        grant_id: "grant-1",
        granted: "automatic",
        names: ["REEF_TOKEN"],
        record_id: null,
      },
    ],
  });
  const revokeCredentialGrant = spyOn(api, "revokeCredentialGrant").mockResolvedValue(undefined);
  const revokeSignIn = spyOn(api, "revokeSignIn").mockResolvedValue(undefined);
  const confirm = spyOn(window, "confirm").mockReturnValue(true);

  try {
    renderPage({
      event_head: "0",
      kind: "user",
      login: "bob@outside.example",
      member_kind: "restricted",
    });

    const grants = within(await screen.findByRole("region", { name: "Live grants" }));
    expect(await grants.findByText("REEF_TOKEN")).toBeDefined();
    await user.click(grants.getByRole("button", { name: "Revoke" }));
    await waitFor(() =>
      expect(revokeCredentialGrant).toHaveBeenCalledWith("grant-1", keyedAttempt)
    );

    const signIns = within(screen.getByRole("region", { name: "Sign-ins" }));
    expect(await signIns.findByText("outside-box")).toBeDefined();
    await user.click(signIns.getByRole("button", { name: "Revoke" }));
    await waitFor(() => expect(revokeSignIn).toHaveBeenCalledWith("signin-1", keyedAttempt));
  } finally {
    cleanup();
    getCredentialGrants.mockRestore();
    revokeCredentialGrant.mockRestore();
    revokeSignIn.mockRestore();
    confirm.mockRestore();
    for (const spy of Object.values(unrelated)) {
      spy.mockRestore();
    }
  }
});

/** Which controls of the two Settings writes that belong to the deployment rather than to a
 *  project are drawn: a new project's, and a repository mapping's (its form, and a listed mapping's
 *  Default and Delete). */
function deploymentWriteControls() {
  return {
    addMapping: screen.queryByRole("button", { name: "Add mapping" }) !== null,
    deleteMapping:
      screen.queryByRole("button", { name: "Delete mapping for acme/reefrepo" }) !== null,
    mappingDefault:
      screen.queryByRole("radio", { name: "Default repository of PRIV: acme/reefrepo" }) !== null,
    newProject: screen.queryByRole("button", { name: "New project" }) !== null,
  };
}

// A new project and a repository mapping belong to the deployment, and Dispatch refuses a
// restricted member every such write (403 RESTRICTED_MEMBER), so their Settings offers none: no New
// project form, no Add mapping form, and a listed mapping with no control to make it the default or
// delete it, while the mapping and its default still show. A full member's page offers all four.
test("a restricted member's Settings offers no project or mapping write, which Dispatch refuses them", async () => {
  const unrelated = stubUnrelatedQueries();
  unrelated.listProjects.mockResolvedValue([
    {
      created_at: "2026-11-19T00:00:00Z",
      gated_steps: [],
      key: "PRIV",
      name: "Reef",
      open_asks: 0,
    },
  ]);
  unrelated.listRepoProjects.mockResolvedValue([
    {
      created_at: "2026-11-19T00:00:00Z",
      created_by: { id: "carol@example.com", kind: "user" },
      default: true,
      project: "PRIV",
      renamed_to: null,
      repo: "acme/reefrepo",
    },
  ]);
  const getCredentialGrants = spyOn(api, "getCredentialGrants").mockResolvedValue({ grants: [] });
  const getCredentialPending = spyOn(api, "getCredentialPending").mockResolvedValue({
    pending: [],
  });

  try {
    renderPage({
      event_head: "0",
      kind: "user",
      login: "bob@outside.example",
      member_kind: "restricted",
    });
    expect(await screen.findByText("acme/reefrepo")).toBeDefined();
    expect(await screen.findByText("Reef")).toBeDefined();
    expect(deploymentWriteControls()).toEqual({
      addMapping: false,
      deleteMapping: false,
      mappingDefault: false,
      newProject: false,
    });
    // The column's heading and the mapping's own cell, which says it is the default in words.
    expect(screen.getAllByText("Default").map((node) => node.closest("th, td")?.tagName)).toEqual([
      "TH",
      "TD",
    ]);
    cleanup();

    renderPage({ event_head: "0", kind: "user", login: "alice" });
    expect(await screen.findByText("acme/reefrepo")).toBeDefined();
    expect(await screen.findByRole("button", { name: "New project" })).toBeDefined();
    expect(deploymentWriteControls()).toEqual({
      addMapping: true,
      deleteMapping: true,
      mappingDefault: true,
      newProject: true,
    });
  } finally {
    cleanup();
    getCredentialGrants.mockRestore();
    getCredentialPending.mockRestore();
    for (const spy of Object.values(unrelated)) {
      spy.mockRestore();
    }
  }
});

// On a Dispatch with no secrets broker the grants read answers 404 FEATURE_OFF, which is how a
// restricted member's Settings learns there is nothing to list: neither broker section is drawn,
// and no failure is shown for a feature the deployment does not have.
test("a restricted member's Settings on a Dispatch with no broker draws neither broker section", async () => {
  const unrelated = stubUnrelatedQueries();
  const getCredentialGrants = spyOn(api, "getCredentialGrants").mockRejectedValue(
    new ApiError(404, { code: "FEATURE_OFF", error: "this Dispatch has no secrets broker" })
  );

  try {
    renderPage({
      event_head: "0",
      kind: "user",
      login: "bob@outside.example",
      member_kind: "restricted",
    });

    await waitFor(() => expect(getCredentialGrants).toHaveBeenCalled());
    expect(await screen.findByText("Architecture sources")).toBeDefined();
    await waitFor(() => expect(screen.queryByText("Live grants")).not.toBeInTheDocument());
    expect(screen.queryByText("Sign-ins")).not.toBeInTheDocument();
    expect(screen.queryByText("Couldn't load live grants.")).not.toBeInTheDocument();
  } finally {
    cleanup();
    getCredentialGrants.mockRestore();
    for (const spy of Object.values(unrelated)) {
      spy.mockRestore();
    }
  }
});

// The grants list answers 404 FEATURE_OFF where there is no broker, which the server reads once,
// at boot, as it does for the credential list: a restricted member's Settings asks it once per
// page load and once per gap in the event feed (`reaskHeldCredentialList`), as a full member's
// asks the credential list. A refresh of it asks nothing more.
test("a restricted member's Settings asks a Dispatch with no broker for the grants list once", async () => {
  const unrelated = stubUnrelatedQueries();
  const getCredentialGrants = spyOn(api, "getCredentialGrants").mockRejectedValue(
    new ApiError(404, { code: "FEATURE_OFF", error: "this Dispatch has no secrets broker" })
  );

  try {
    const queryClient = renderPage({
      event_head: "0",
      kind: "user",
      login: "bob@outside.example",
      member_kind: "restricted",
    });

    await waitFor(() => expect(getCredentialGrants).toHaveBeenCalledTimes(1));
    expect(await screen.findByText("Architecture sources")).toBeDefined();
    await queryClient.invalidateQueries({ queryKey: credentialGrantsQuery().queryKey });
    expect(getCredentialGrants).toHaveBeenCalledTimes(1);
    expect(screen.queryByText("Live grants")).not.toBeInTheDocument();
  } finally {
    cleanup();
    getCredentialGrants.mockRestore();
    for (const spy of Object.values(unrelated)) {
      spy.mockRestore();
    }
  }
});
