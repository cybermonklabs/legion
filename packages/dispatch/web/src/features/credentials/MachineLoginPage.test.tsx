import { expect, jest, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { realClock } from "../../__tests__/fake-clock";
import { keyedAttempt } from "../../__tests__/keyed-attempt";
import { stopTheLateWrite } from "../../__tests__/stop-late-write";
import { userOnFakeClock } from "../../__tests__/user";
import { ApiError, api, NetworkError } from "../../api/client";
import type { RecordResponse } from "../../api/types";
import { stoppedWriteNote } from "../writes/keyed-write";
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

/** A fetch that failed, which the write is tried again after: it goes late, for the reader to
 *  stop. */
const dropped = () => new NetworkError("/api/v1/credential-requests", new TypeError("Failed"));

/** The page over `queryClient`, on the held clock, with the code looked up when `lookedUp`. */
async function stoppedPage(queryClient: QueryClient, lookedUp: boolean) {
  const user = userOnFakeClock();
  render(
    <MemoryRouter initialEntries={["/credentials/machine"]}>
      <QueryClientProvider client={queryClient}>
        <MachineLoginPage />
      </QueryClientProvider>
    </MemoryRouter>
  );
  await user.click(screen.getByLabelText("Code shown on the machine"));
  await user.paste("abcd1234");
  await user.click(screen.getByRole("button", { name: "Look up" }));
  if (lookedUp) await screen.findByText("worker-7.example.com");
  return user;
}

// A lookup, an approval or a denial the reader stopped may have been made: the page says so,
// and what its button does now - a press of its own - rather than a failure it cannot know of.
test("a lookup the reader stopped says it may have been made, not that it failed", async () => {
  jest.useFakeTimers();
  const lookup = spyOn(api, "lookupMachineCredential").mockRejectedValue(dropped());
  const getMachineLogins = spyOn(api, "getMachineLogins").mockResolvedValue({ credentials: [] });
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  try {
    await stoppedPage(queryClient, false);
    await stopTheLateWrite(queryClient);
    expect(
      screen.getByText(
        stoppedWriteNote("the lookup of that code", "made", {
          anew: "Look up sends the code in the field as a new lookup.",
        })
      )
    ).toBeDefined();
  } finally {
    realClock();
    cleanup();
    queryClient.clear();
    lookup.mockRestore();
    getMachineLogins.mockRestore();
  }
});

test("an approval the reader stopped says it may have been made, not that it failed", async () => {
  jest.useFakeTimers();
  const lookup = spyOn(api, "lookupMachineCredential").mockResolvedValue(machineRecord());
  const getMachineLogins = spyOn(api, "getMachineLogins").mockResolvedValue({ credentials: [] });
  const approve = spyOn(api, "approveCredentialRecord").mockRejectedValue(dropped());
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  try {
    const user = await stoppedPage(queryClient, true);
    await user.click(screen.getByRole("button", { name: "Approve" }));
    await stopTheLateWrite(queryClient);
    expect(
      screen.getByText(
        stoppedWriteNote("the approval", "made", {
          anew: "Pressing Approve again sends a new approval.",
        })
      )
    ).toBeDefined();
  } finally {
    realClock();
    cleanup();
    queryClient.clear();
    lookup.mockRestore();
    getMachineLogins.mockRestore();
    approve.mockRestore();
  }
});

test("a denial the reader stopped says it may have been made, not that it failed", async () => {
  jest.useFakeTimers();
  const lookup = spyOn(api, "lookupMachineCredential").mockResolvedValue(machineRecord());
  const getMachineLogins = spyOn(api, "getMachineLogins").mockResolvedValue({ credentials: [] });
  const deny = spyOn(api, "denyCredentialRecord").mockRejectedValue(dropped());
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  try {
    const user = await stoppedPage(queryClient, true);
    await user.click(screen.getByRole("button", { name: "Deny" }));
    await stopTheLateWrite(queryClient);
    expect(
      screen.getByText(
        stoppedWriteNote("the denial", "made", { anew: "Pressing Deny again sends a new denial." })
      )
    ).toBeDefined();
  } finally {
    realClock();
    cleanup();
    queryClient.clear();
    lookup.mockRestore();
    getMachineLogins.mockRestore();
    deny.mockRestore();
  }
});
