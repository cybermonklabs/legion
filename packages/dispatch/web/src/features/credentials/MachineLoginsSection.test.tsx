import { expect, jest, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { realClock, tick } from "../../__tests__/fake-clock";
import { keyedAttempt } from "../../__tests__/keyed-attempt";
import { userOnFakeClock } from "../../__tests__/user";
import { api } from "../../api/client";
import type { ApproverGrantResp, LauncherCredentialResp } from "../../api/types";
import { GrantsSection } from "./GrantsSection";
import { MachineLoginPage } from "./MachineLoginPage";
import { MachineLoginsSection } from "./MachineLoginsSection";

const devbox: LauncherCredentialResp = {
  approved_by: "ada@example.com",
  credential_id: "cred-devbox",
  expired: false,
  expires_at: "2026-10-10T12:00:00Z",
  host: "devbox.example.com",
  issued_at: "2026-10-03T12:00:00Z",
  person: null,
  service: null,
  subject: null,
};
const laptop: LauncherCredentialResp = {
  approved_by: "ada@example.com",
  credential_id: "cred-laptop",
  expired: false,
  expires_at: "2026-10-09T08:00:00Z",
  host: "laptop.example.com",
  issued_at: "2026-10-02T08:00:00Z",
  person: null,
  service: null,
  subject: null,
};
/** A service's login a person approved before services logged in with their own Kubernetes
 *  token, which someone other than the viewer approved: anyone signed in lists and revokes it. */
const daemon: LauncherCredentialResp = {
  approved_by: "bob@example.com",
  credential_id: "cred-daemon",
  expired: false,
  expires_at: "2026-10-10T09:00:00Z",
  host: "cluster.example.com",
  issued_at: "2026-10-03T09:00:00Z",
  person: null,
  service: "legion-daemon",
  subject: null,
};
/** The devbox's previous login: expired, but a box it started still renews with its own key. */
const stale: LauncherCredentialResp = {
  approved_by: "ada@example.com",
  credential_id: "cred-stale",
  expired: true,
  expires_at: "2026-10-03T11:00:00Z",
  host: "devbox.example.com",
  issued_at: "2026-09-26T11:00:00Z",
  person: null,
  service: null,
  subject: null,
};
/** A person's pod login: the token login of a pod whose Kubernetes account the broker's
 *  BROKER_POD_OPERATORS maps to ada, the viewer. The broker sets its service, the login service
 *  the account logs in as, and its subject, and names ada as its person. Only ada's list holds it,
 *  and only she may revoke it. */
const adaPod: LauncherCredentialResp = {
  approved_by: "",
  credential_id: "cred-ada-pod",
  expired: false,
  expires_at: "2026-10-03T13:00:00Z",
  host: "people/ada",
  issued_at: "2026-10-03T12:00:00Z",
  person: "ada@example.com",
  service: "person-ada",
  subject: "system:serviceaccount:people:ada",
};

function renderPage() {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <MemoryRouter initialEntries={["/credentials/machine"]}>
      <QueryClientProvider client={queryClient}>
        <MachineLoginPage />
      </QueryClientProvider>
    </MemoryRouter>
  );
}

/** The page's confirm dialog that asks "Revoke <the login>?", checked to say `body`, then answered
 *  with `button`. The dialog is the page's own (`ConfirmDialog`), never `window.confirm`. */
async function answerRevoke(
  user: { click: (element: Element) => Promise<void> },
  login: string,
  button: "Cancel" | "Revoke",
  body?: string
): Promise<void> {
  const dialog = within(
    await screen.findByRole("dialog", { name: `Revoke the machine login for ${login}?` })
  );
  if (body !== undefined) expect(dialog.getByText(body)).toBeDefined();
  await user.click(dialog.getByRole("button", { name: button }));
}

test("an expired login whose sessions still run is listed and marked so, and Revoke ends it", async () => {
  const user = userEvent.setup();
  let live = [devbox, stale];
  const getMachineLogins = spyOn(api, "getMachineLogins").mockImplementation(async () => ({
    credentials: live,
  }));
  const revokeMachineLogin = spyOn(api, "revokeMachineLogin").mockImplementation(async (id) => {
    live = live.filter((login) => login.credential_id !== id);
  });

  try {
    renderPage();
    const section = within(await screen.findByRole("region", { name: "Machine logins" }));
    const marked = await section.findByText("expired, sessions still running");
    const staleRow = marked.closest("tr") as HTMLElement;
    expect(within(staleRow).getByText("devbox.example.com")).toBeDefined();
    expect(section.getAllByText("expired, sessions still running")).toHaveLength(1);

    await user.click(within(staleRow).getByRole("button", { name: "Revoke" }));
    await answerRevoke(user, "devbox.example.com", "Revoke");
    await waitFor(() =>
      expect(revokeMachineLogin).toHaveBeenCalledWith("cred-stale", keyedAttempt)
    );
    await waitFor(() =>
      expect(section.queryByText("expired, sessions still running")).not.toBeInTheDocument()
    );
    expect(section.getByText("devbox.example.com")).toBeDefined();
  } finally {
    cleanup();
    getMachineLogins.mockRestore();
    revokeMachineLogin.mockRestore();
  }
});

// A machine login's revoke ends every session it enrolled and with them their grants, so a page
// showing Live grants beside the machine logins drops those grants without a reload.
test("revoking a machine login refreshes Live grants", async () => {
  const user = userEvent.setup();
  let logins = [devbox];
  const grant: ApproverGrantResp = {
    approver: null,
    created_at: "2026-10-03T12:05:00Z",
    decided_by: null,
    enrollment: { kind: "session", operator: "ada@example.com", runtime_id: "box-1", slot: null },
    expires_at: "2026-10-03T13:05:00Z",
    grant_id: "grant-1",
    granted: "automatic",
    names: ["WORKER_TOKEN"],
    record_id: null,
  };
  let grants = [grant];
  const getMachineLogins = spyOn(api, "getMachineLogins").mockImplementation(async () => ({
    credentials: logins,
  }));
  const getCredentialGrants = spyOn(api, "getCredentialGrants").mockImplementation(async () => ({
    grants,
  }));
  const revokeMachineLogin = spyOn(api, "revokeMachineLogin").mockImplementation(async () => {
    logins = [];
    grants = [];
  });
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });

  try {
    render(
      <QueryClientProvider client={queryClient}>
        <MachineLoginsSection />
        <GrantsSection />
      </QueryClientProvider>
    );
    const liveGrants = within(await screen.findByRole("region", { name: "Live grants" }));
    expect(await liveGrants.findByText("WORKER_TOKEN")).toBeDefined();
    const section = within(screen.getByRole("region", { name: "Machine logins" }));

    await user.click(await section.findByRole("button", { name: "Revoke" }));
    await answerRevoke(user, "devbox.example.com", "Revoke");
    await waitFor(() =>
      expect(revokeMachineLogin).toHaveBeenCalledWith("cred-devbox", keyedAttempt)
    );
    expect(await liveGrants.findByText("No live grants.")).toBeDefined();
    expect(liveGrants.queryByText("WORKER_TOKEN")).not.toBeInTheDocument();
  } finally {
    cleanup();
    getMachineLogins.mockRestore();
    getCredentialGrants.mockRestore();
    revokeMachineLogin.mockRestore();
  }
});

test("the machine-login page lists the viewer's machine logins, and Revoke ends the one confirmed", async () => {
  jest.useFakeTimers();
  const user = userOnFakeClock();
  let live = [devbox, laptop];
  const getMachineLogins = spyOn(api, "getMachineLogins").mockImplementation(async () => ({
    credentials: live,
  }));
  // Held, and given inside `act` after the press, as docs/testing.md ("The act rule of a Dispatch
  // unit test") says.
  const revokeAnswer = Promise.withResolvers<void>();
  const revokeMachineLogin = spyOn(api, "revokeMachineLogin").mockImplementation(async (id) => {
    await revokeAnswer.promise;
    live = live.filter((login) => login.credential_id !== id);
  });

  try {
    renderPage();
    const section = within(await screen.findByRole("region", { name: "Machine logins" }));
    const devboxRow = (await section.findByText("devbox.example.com")).closest("tr");
    expect(devboxRow).not.toBeNull();
    expect(section.getByText("laptop.example.com")).toBeDefined();

    await user.click(within(devboxRow as HTMLElement).getByRole("button", { name: "Revoke" }));
    await answerRevoke(
      user,
      "devbox.example.com",
      "Cancel",
      "Every agent session it started loses its secrets at once, and the machine needs a new login."
    );
    // A mutation calls its function a few microtasks after the click; a timer runs after them all.
    await tick(0);
    expect(revokeMachineLogin).not.toHaveBeenCalled();

    await user.click(within(devboxRow as HTMLElement).getByRole("button", { name: "Revoke" }));
    await answerRevoke(user, "devbox.example.com", "Revoke");
    await waitFor(() =>
      expect(revokeMachineLogin).toHaveBeenCalledWith("cred-devbox", keyedAttempt)
    );
    await act(async () => {
      revokeAnswer.resolve();
    });
    await waitFor(() => expect(section.queryByText("devbox.example.com")).not.toBeInTheDocument());
    expect(section.getByText("laptop.example.com")).toBeDefined();
    expect(revokeMachineLogin).toHaveBeenCalledTimes(1);
  } finally {
    realClock();
    cleanup();
    getMachineLogins.mockRestore();
    revokeMachineLogin.mockRestore();
  }
});

test("a service's login someone else approved is listed by its service and its approver, and its Revoke says its pods end", async () => {
  const user = userEvent.setup();
  let live = [daemon, devbox];
  const getMachineLogins = spyOn(api, "getMachineLogins").mockImplementation(async () => ({
    credentials: live,
  }));
  const revokeMachineLogin = spyOn(api, "revokeMachineLogin").mockImplementation(async (id) => {
    live = live.filter((login) => login.credential_id !== id);
  });

  try {
    renderPage();
    const section = within(await screen.findByRole("region", { name: "Machine logins" }));
    const daemonRow = (await section.findByText("legion-daemon on cluster.example.com")).closest(
      "tr"
    );
    expect(daemonRow).not.toBeNull();
    expect(within(daemonRow as HTMLElement).getByText("bob@example.com")).toBeDefined();
    const devboxRow = section.getByText("devbox.example.com").closest("tr");
    expect(within(devboxRow as HTMLElement).getByText("ada@example.com")).toBeDefined();

    await user.click(within(daemonRow as HTMLElement).getByRole("button", { name: "Revoke" }));
    await answerRevoke(
      user,
      "legion-daemon on cluster.example.com",
      "Revoke",
      "Every session it started, its worker pods included, ends at once."
    );
    await waitFor(() =>
      expect(revokeMachineLogin).toHaveBeenCalledWith("cred-daemon", keyedAttempt)
    );
    await waitFor(() =>
      expect(section.queryByText("legion-daemon on cluster.example.com")).not.toBeInTheDocument()
    );
    expect(section.getByText("devbox.example.com")).toBeDefined();
  } finally {
    cleanup();
    getMachineLogins.mockRestore();
    revokeMachineLogin.mockRestore();
  }
});

// A broker older than token logins sends no `subject` and no `person` at all, so while Dispatch
// deploys first, a service's login a person approved is shown with its approver and its plain
// Revoke, never as a token login of `undefined`.
test("a service's login from a broker that sends no subject is shown as one a person approved", async () => {
  const user = userEvent.setup();
  // The route table declares `subject` and `person` on every machine login, because the broker
  // Dispatch relays today sends both; a broker older than the fields sends no such keys at all,
  // which is what this row is, so the keys are dropped from the value rather than set to anything.
  const fromOlderBroker: LauncherCredentialResp = { ...daemon };
  Reflect.deleteProperty(fromOlderBroker, "subject");
  Reflect.deleteProperty(fromOlderBroker, "person");
  const getMachineLogins = spyOn(api, "getMachineLogins").mockResolvedValue({
    credentials: [fromOlderBroker],
  });

  try {
    renderPage();
    const section = within(await screen.findByRole("region", { name: "Machine logins" }));
    const row = (await section.findByText("legion-daemon on cluster.example.com")).closest("tr");
    expect(within(row as HTMLElement).getByText("bob@example.com")).toBeDefined();
    expect(section.queryByText(/undefined/)).not.toBeInTheDocument();
    await user.click(within(row as HTMLElement).getByRole("button", { name: "Revoke" }));
    await answerRevoke(
      user,
      "legion-daemon on cluster.example.com",
      "Cancel",
      "Every session it started, its worker pods included, ends at once."
    );
  } finally {
    cleanup();
    getMachineLogins.mockRestore();
  }
});

test("a service's token login names its ServiceAccount, and its Revoke says what logs in again", async () => {
  const user = userEvent.setup();
  const tokenLogin: LauncherCredentialResp = {
    ...daemon,
    approved_by: "",
    credential_id: "cred-token",
    host: "legion-system/legion-daemon-legion",
    subject: "system:serviceaccount:legion-system:legion-daemon-legion",
  };
  const getMachineLogins = spyOn(api, "getMachineLogins").mockResolvedValue({
    credentials: [tokenLogin],
  });
  const revokeMachineLogin = spyOn(api, "revokeMachineLogin").mockResolvedValue(undefined);

  try {
    renderPage();
    const section = within(await screen.findByRole("region", { name: "Machine logins" }));
    const row = (
      await section.findByText("legion-daemon on legion-system/legion-daemon-legion")
    ).closest("tr");
    expect(
      within(row as HTMLElement).getByText(
        "No one: system:serviceaccount:legion-system:legion-daemon-legion"
      )
    ).toBeDefined();
    await user.click(within(row as HTMLElement).getByRole("button", { name: "Revoke" }));
    await answerRevoke(
      user,
      "legion-daemon on legion-system/legion-daemon-legion",
      "Cancel",
      "Every session it started, its worker pods included, ends at once, and a running pod stays without secrets until it is replaced; legion-daemon logs in again with its Kubernetes token, for the pods it launches next, unless system:serviceaccount:legion-system:legion-daemon-legion is removed from the broker's BROKER_SERVICE_LOGINS."
    );
    expect(revokeMachineLogin).not.toHaveBeenCalled();
  } finally {
    cleanup();
    getMachineLogins.mockRestore();
    revokeMachineLogin.mockRestore();
  }
});

// A person's pod login carries a service and a subject, as a service's token login does, and
// its person besides: the viewer, whose list alone holds it and who alone may revoke it. So it
// reads as the viewer's own pod, and never as a service's login that anyone signed in may
// revoke, while a service's token login beside it still reads as the service's.
test("a person's pod login reads as the viewer's own pod, and its Revoke says the pod logs in again", async () => {
  const user = userEvent.setup();
  const serviceToken: LauncherCredentialResp = {
    ...daemon,
    approved_by: "",
    credential_id: "cred-token",
    host: "legion-system/legion-daemon-legion",
    subject: "system:serviceaccount:legion-system:legion-daemon-legion",
  };
  const getMachineLogins = spyOn(api, "getMachineLogins").mockResolvedValue({
    credentials: [adaPod, serviceToken],
  });
  const revokeMachineLogin = spyOn(api, "revokeMachineLogin").mockResolvedValue(undefined);

  try {
    renderPage();
    const section = within(await screen.findByRole("region", { name: "Machine logins" }));
    const podRow = (await section.findByText("people/ada (your pod)")).closest("tr") as HTMLElement;
    expect(
      within(podRow).getByText(
        "No one: BROKER_POD_OPERATORS maps system:serviceaccount:people:ada to you"
      )
    ).toBeDefined();
    expect(section.queryByText(/person-ada/)).not.toBeInTheDocument();
    const serviceRow = section
      .getByText("legion-daemon on legion-system/legion-daemon-legion")
      .closest("tr") as HTMLElement;
    expect(
      within(serviceRow).getByText(
        "No one: system:serviceaccount:legion-system:legion-daemon-legion"
      )
    ).toBeDefined();

    await user.click(within(podRow).getByRole("button", { name: "Revoke" }));
    await answerRevoke(
      user,
      "people/ada (your pod)",
      "Cancel",
      "Every session it started ends at once. The pod logs in again with its token, under a new login, while the broker's BROKER_SERVICE_LOGINS names its Kubernetes account, and that login is yours while BROKER_POD_OPERATORS maps the account to you."
    );
    expect(revokeMachineLogin).not.toHaveBeenCalled();
  } finally {
    cleanup();
    getMachineLogins.mockRestore();
    revokeMachineLogin.mockRestore();
  }
});

// Two logins of one service from machines reporting one host name read alike but for their
// credential ids, which every enrollment a login made names: each row shows its own, and Revoke on
// the row a credential id picks ends that login and no other.
test("two logins of one service on one host are told apart by their credential ids, and Revoke ends the one picked by its id", async () => {
  const user = userEvent.setup();
  const copy: LauncherCredentialResp = {
    ...daemon,
    approved_by: "mallory@example.com",
    credential_id: "cred-copy",
    issued_at: "2026-10-03T10:00:00Z",
  };
  let live = [daemon, copy];
  const getMachineLogins = spyOn(api, "getMachineLogins").mockImplementation(async () => ({
    credentials: live,
  }));
  const revokeMachineLogin = spyOn(api, "revokeMachineLogin").mockImplementation(async (id) => {
    live = live.filter((login) => login.credential_id !== id);
  });

  try {
    renderPage();
    const section = within(await screen.findByRole("region", { name: "Machine logins" }));
    expect(await section.findAllByText("legion-daemon on cluster.example.com")).toHaveLength(2);
    const copyRow = section.getByText("cred-copy").closest("tr") as HTMLElement;
    expect(within(copyRow).getByText("mallory@example.com")).toBeDefined();
    const daemonRow = section.getByText("cred-daemon").closest("tr") as HTMLElement;
    expect(within(daemonRow).getByText("bob@example.com")).toBeDefined();

    await user.click(within(copyRow).getByRole("button", { name: "Revoke" }));
    await answerRevoke(user, "legion-daemon on cluster.example.com", "Revoke");
    await waitFor(() => expect(revokeMachineLogin).toHaveBeenCalledWith("cred-copy", keyedAttempt));
    await waitFor(() => expect(section.queryByText("cred-copy")).not.toBeInTheDocument());
    expect(section.getByText("cred-daemon")).toBeDefined();
    expect(revokeMachineLogin).toHaveBeenCalledTimes(1);
  } finally {
    cleanup();
    getMachineLogins.mockRestore();
    revokeMachineLogin.mockRestore();
  }
});

test("a machine login approved on the page joins the list", async () => {
  const user = userEvent.setup();
  let live: LauncherCredentialResp[] = [];
  const getMachineLogins = spyOn(api, "getMachineLogins").mockImplementation(async () => ({
    credentials: live,
  }));
  const lookupMachineCredential = spyOn(api, "lookupMachineCredential").mockResolvedValue({
    approver: "ada@example.com",
    decided: null,
    enrollment: null,
    expires_at: "2026-10-03T12:15:00Z",
    identifiers: ["devbox.example.com"],
    kind: "launcher_credential",
    lifetime_seconds: 604800,
    reason: "",
    record_id: "req-1",
    requested_at: "2026-10-03T12:00:00Z",
    rules_version: "v1",
    service: null,
    session: { enrollment: null, request: null },
    state: "pending",
  });
  const approveCredentialRecord = spyOn(api, "approveCredentialRecord").mockImplementation(
    async () => {
      live = [devbox];
      return { credential_id: "cred-devbox", state: "approved" };
    }
  );

  try {
    renderPage();
    const section = within(await screen.findByRole("region", { name: "Machine logins" }));
    expect(await section.findByText("No live machine logins.")).toBeDefined();

    await user.click(screen.getByLabelText("Code shown on the machine"));
    await user.paste("abcd1234");
    await user.click(screen.getByRole("button", { name: "Look up" }));
    await user.click(await screen.findByRole("button", { name: "Approve" }));

    await waitFor(() => expect(approveCredentialRecord).toHaveBeenCalledTimes(1));
    expect(await section.findByText("devbox.example.com")).toBeDefined();
  } finally {
    cleanup();
    getMachineLogins.mockRestore();
    lookupMachineCredential.mockRestore();
    approveCredentialRecord.mockRestore();
  }
});
