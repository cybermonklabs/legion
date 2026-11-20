import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { keyedAttempt } from "../../__tests__/keyed-attempt";
import { ApiError, api } from "../../api/client";
import { whoAmIQuery } from "../../api/queries";
import type { RecordResponse } from "../../api/types";
import { MachineLoginPage } from "./MachineLoginPage";

function machineRecord(overrides: Partial<RecordResponse> = {}): RecordResponse {
  return {
    approver: "sami",
    decided: null,
    enrollment: null,
    expires_at: "2026-09-27T01:00:00Z",
    identifiers: ["worker-7.example.com"],
    kind: "launcher_credential",
    lifetime_seconds: 900,
    reason: "",
    record_id: "req-2",
    requested_at: "2026-09-27T00:00:00Z",
    rules_version: "v3",
    service: null,
    session: { enrollment: null, request: null },
    state: "pending",
    ...overrides,
  };
}

/** The page, signed in as a restricted member when `restricted` is set: the shell holds
 *  `/auth/whoami`'s answer before any page draws, so it is seeded here as the shell holds it. */
function renderPage(restricted = false) {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  if (restricted) {
    queryClient.setQueryData(whoAmIQuery().queryKey, {
      event_head: "0",
      kind: "user",
      login: "erin@outside.example",
      member_kind: "restricted",
    });
  }
  return render(
    <MemoryRouter initialEntries={["/credentials/machine"]}>
      <QueryClientProvider client={queryClient}>
        <MachineLoginPage />
      </QueryClientProvider>
    </MemoryRouter>
  );
}

// A restricted member approves the device login of their own agents on this page, and lists and
// revokes them there: Dispatch answers them their own machine logins alone, and no service's, so
// the list says so rather than offering a service's login.
test("a restricted member's page approves their own login and lists and revokes their own machine logins", async () => {
  const user = userEvent.setup();
  const record = machineRecord({ approver: "erin@outside.example" });
  const lookupMachineCredential = spyOn(api, "lookupMachineCredential").mockResolvedValue(record);
  const getMachineLogins = spyOn(api, "getMachineLogins").mockResolvedValue({
    credentials: [
      {
        approved_by: "erin@outside.example",
        credential_id: "cred-erin",
        expired: false,
        expires_at: "2026-12-20T00:00:00Z",
        host: "erin-laptop",
        issued_at: "2026-11-20T00:00:00Z",
        service: null,
        subject: null,
      },
    ],
  });
  const approveCredentialRecord = spyOn(api, "approveCredentialRecord").mockResolvedValue({
    credential_id: "cred-9",
    state: "approved",
  });
  const revokeMachineLogin = spyOn(api, "revokeMachineLogin").mockResolvedValue(undefined);
  const confirm = spyOn(window, "confirm").mockReturnValue(true);

  try {
    renderPage(true);

    await user.click(screen.getByLabelText("Code shown on the machine"));
    await user.paste("abcd1234");
    await user.click(screen.getByRole("button", { name: "Look up" }));
    expect(await screen.findByText("worker-7.example.com")).toBeDefined();
    await user.click(screen.getByRole("button", { name: "Approve" }));
    await waitFor(() => expect(approveCredentialRecord).toHaveBeenCalledTimes(1));

    expect(await screen.findByText("erin-laptop")).toBeDefined();
    expect(screen.queryByText(/every service's login/)).toBeNull();
    await user.click(screen.getByRole("button", { name: /Revoke/ }));
    await waitFor(() => expect(revokeMachineLogin).toHaveBeenCalledTimes(1));
    expect(revokeMachineLogin.mock.calls[0]?.[0]).toBe("cred-erin");
  } finally {
    cleanup();
    lookupMachineCredential.mockRestore();
    getMachineLogins.mockRestore();
    approveCredentialRecord.mockRestore();
    revokeMachineLogin.mockRestore();
    confirm.mockRestore();
  }
});

test("code -> lookup -> Approve posts the looked-up code", async () => {
  const user = userEvent.setup();
  const record = machineRecord();
  const lookupMachineCredential = spyOn(api, "lookupMachineCredential").mockResolvedValue(record);
  // The page lists the viewer's machine logins beside the code form; no test here reads them.
  const getMachineLogins = spyOn(api, "getMachineLogins").mockResolvedValue({ credentials: [] });
  const approveCredentialRecord = spyOn(api, "approveCredentialRecord").mockResolvedValue({
    credential_id: "cred-9",
    state: "approved",
  });

  try {
    renderPage();

    await user.click(screen.getByLabelText("Code shown on the machine"));
    await user.paste("abcd1234");
    await user.click(screen.getByRole("button", { name: "Look up" }));

    await waitFor(() =>
      expect(lookupMachineCredential).toHaveBeenCalledWith("ABCD-1234", keyedAttempt)
    );
    expect(await screen.findByText("worker-7.example.com")).toBeDefined();

    await user.click(screen.getByRole("button", { name: "Approve" }));

    await waitFor(() => expect(approveCredentialRecord).toHaveBeenCalledTimes(1));
    expect(approveCredentialRecord).toHaveBeenCalledWith(
      "req-2",
      { code: "ABCD-1234" },
      keyedAttempt
    );
  } finally {
    cleanup();
    lookupMachineCredential.mockRestore();
    getMachineLogins.mockRestore();
    approveCredentialRecord.mockRestore();
  }
});

test("a NO_SUCH_CODE lookup failure renders inline under the input", async () => {
  const user = userEvent.setup();
  const lookupMachineCredential = spyOn(api, "lookupMachineCredential").mockRejectedValue(
    new ApiError(404, { code: "NO_SUCH_CODE", error: "No pending request matches that code." })
  );
  const getMachineLogins = spyOn(api, "getMachineLogins").mockResolvedValue({ credentials: [] });

  try {
    renderPage();

    await user.click(screen.getByLabelText("Code shown on the machine"));
    await user.paste("zzzz9999");
    await user.click(screen.getByRole("button", { name: "Look up" }));

    expect(await screen.findByText("No pending request matches that code.")).toBeDefined();
    expect(screen.queryByText("worker-7.example.com")).not.toBeInTheDocument();
  } finally {
    cleanup();
    lookupMachineCredential.mockRestore();
    getMachineLogins.mockRestore();
  }
});

test("code -> lookup -> Deny posts the looked-up code too, which the broker requires", async () => {
  const user = userEvent.setup();
  const record = machineRecord();
  const lookupMachineCredential = spyOn(api, "lookupMachineCredential").mockResolvedValue(record);
  const getMachineLogins = spyOn(api, "getMachineLogins").mockResolvedValue({ credentials: [] });
  const denyCredentialRecord = spyOn(api, "denyCredentialRecord").mockResolvedValue({
    state: "denied",
  });

  try {
    renderPage();

    await user.click(screen.getByLabelText("Code shown on the machine"));
    await user.paste("abcd1234");
    await user.click(screen.getByRole("button", { name: "Look up" }));

    await waitFor(() =>
      expect(lookupMachineCredential).toHaveBeenCalledWith("ABCD-1234", keyedAttempt)
    );
    expect(await screen.findByText("worker-7.example.com")).toBeDefined();

    await user.click(screen.getByRole("button", { name: "Deny" }));

    await waitFor(() => expect(denyCredentialRecord).toHaveBeenCalledTimes(1));
    expect(denyCredentialRecord).toHaveBeenCalledWith("req-2", { code: "ABCD-1234" }, keyedAttempt);
  } finally {
    cleanup();
    lookupMachineCredential.mockRestore();
    denyCredentialRecord.mockRestore();
    getMachineLogins.mockRestore();
  }
});

test("a pending service login from before token logins offers Deny alone, which the broker allows", async () => {
  const user = userEvent.setup();
  const record = machineRecord({ approver: "anyone", service: "legion-daemon" });
  const lookupMachineCredential = spyOn(api, "lookupMachineCredential").mockResolvedValue(record);
  const getMachineLogins = spyOn(api, "getMachineLogins").mockResolvedValue({ credentials: [] });
  const denyCredentialRecord = spyOn(api, "denyCredentialRecord").mockResolvedValue({
    state: "denied",
  });

  try {
    renderPage();

    await user.click(screen.getByLabelText("Code shown on the machine"));
    await user.paste("abcd1234");
    await user.click(screen.getByRole("button", { name: "Look up" }));

    await waitFor(() =>
      expect(lookupMachineCredential).toHaveBeenCalledWith("ABCD-1234", keyedAttempt)
    );
    expect(await screen.findByRole("button", { name: "Deny" })).toBeDefined();
    expect(screen.queryByRole("button", { name: "Approve" })).not.toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: "Deny" }));
    await waitFor(() =>
      expect(denyCredentialRecord).toHaveBeenCalledWith(
        "req-2",
        { code: "ABCD-1234" },
        keyedAttempt
      )
    );
  } finally {
    cleanup();
    lookupMachineCredential.mockRestore();
    denyCredentialRecord.mockRestore();
    getMachineLogins.mockRestore();
  }
});
