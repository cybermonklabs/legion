import { beforeAll, expect, jest, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { realClock, settle, tick } from "../../__tests__/fake-clock";
import { heldNotDisabled, pressHeld } from "../../__tests__/held-press";
import { heldUntilLate, pressesOf } from "../../__tests__/held-until-late";
import { keyedAttempt } from "../../__tests__/keyed-attempt";
import { stopTheLateWrite } from "../../__tests__/stop-late-write";
import { userOnFakeClock } from "../../__tests__/user";
import { ApiError, api, NetworkError } from "../../api/client";
import type { RecordResponse } from "../../api/types";
import { warmMarkdownRenderer } from "../refs/markdown-engine";
import { stoppedWriteNote } from "../writes/keyed-write";
import { MachineLoginPage } from "./MachineLoginPage";

// The rows draw through the shared renderer, whose Markdown engine loads lazily, and they run in
// random order, so the file loads the engine before its first row, whichever row that is.
beforeAll(async () => {
  await warmMarkdownRenderer();
});

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

function renderPage(
  queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } })
) {
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

// Approve and Deny are both held while either decision is out (`CredentialDecisionButtons`,
// `heldPress`), never `disabled`: a `disabled` button drops the focus the reader's press left on
// it to the page's body. The pressed one keeps the focus and a second press of it sends nothing,
// and the other is held beside it, so a press of it sends nothing either.
for (const pressed of ["Approve", "Deny"] as const) {
  const other = pressed === "Approve" ? "Deny" : "Approve";
  test(`${pressed}, held while its decision is out, keeps the focus, and neither it nor ${other} sends a second decision`, async () => {
    const user = userEvent.setup();
    const answered = Promise.withResolvers<void>();
    const lookupMachineCredential = spyOn(api, "lookupMachineCredential").mockResolvedValue(
      machineRecord()
    );
    const getMachineLogins = spyOn(api, "getMachineLogins").mockResolvedValue({ credentials: [] });
    const approveCredentialRecord = spyOn(api, "approveCredentialRecord").mockImplementation(
      async () => {
        await answered.promise;
        return { credential_id: "cred-9", state: "approved" };
      }
    );
    const denyCredentialRecord = spyOn(api, "denyCredentialRecord").mockImplementation(async () => {
      await answered.promise;
      return { state: "denied" };
    });
    const decisions = { Approve: approveCredentialRecord, Deny: denyCredentialRecord };

    try {
      renderPage();

      await user.click(screen.getByLabelText("Code shown on the machine"));
      await user.paste("abcd1234");
      await user.click(screen.getByRole("button", { name: "Look up" }));
      expect(await screen.findByText("worker-7.example.com")).toBeDefined();

      await pressHeld(user, screen.getByRole("button", { name: pressed }), {
        name: pressed,
        sent: () => decisions[pressed].mock.calls.length,
      });
      const otherButton = screen.getByRole<HTMLButtonElement>("button", { name: other });
      expect(heldNotDisabled(otherButton, other)).toBe(true);
      await user.click(otherButton);
      expect(decisions[other]).not.toHaveBeenCalled();
      expect(decisions[pressed]).toHaveBeenCalledTimes(1);

      await act(async () => answered.resolve());
    } finally {
      cleanup();
      lookupMachineCredential.mockRestore();
      getMachineLogins.mockRestore();
      approveCredentialRecord.mockRestore();
      denyCredentialRecord.mockRestore();
    }
  });
}

// A lookup that never answers - each try fails at the network, and the next waits out its
// backoff - holds Look up until its deadline (`SEND_DEADLINE_MS`), and no longer: past it the
// lookup is late and still tried, and Look up is let go, with the focus on it. A press then is a
// lookup of its own: the guard that drops a second press of the same task lets go too.
test("Look up, held until its lookup's deadline, is let go there, and a press then looks the code up again", async () => {
  const lookupMachineCredential = spyOn(api, "lookupMachineCredential").mockRejectedValue(
    dropped()
  );
  const getMachineLogins = spyOn(api, "getMachineLogins").mockResolvedValue({ credentials: [] });
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  try {
    renderPage(queryClient);
    jest.useFakeTimers();
    const user = userOnFakeClock();
    await user.click(screen.getByLabelText("Code shown on the machine"));
    await user.paste("abcd1234");
    await heldUntilLate(user, screen.getByRole<HTMLButtonElement>("button", { name: "Look up" }), {
      label: "Lookup of a machine login code",
      name: "Look up",
      queryClient,
      sent: () => pressesOf([lookupMachineCredential]),
    });
    await user.keyboard("{Enter}");
    await tick(1_000);
    expect(
      pressesOf([lookupMachineCredential]),
      "a press past the deadline looked nothing up"
    ).toBe(2);
  } finally {
    realClock();
    cleanup();
    queryClient.clear();
    lookupMachineCredential.mockRestore();
    getMachineLogins.mockRestore();
  }
});

// So for a decision: one that never answers holds Approve and Deny until its deadline, and no
// longer, and a press then sends a decision of its own.
test.each([
  { label: "Approval of a machine login", pressed: "Approve" },
  { label: "Denial of a machine login", pressed: "Deny" },
] as const)("$pressed, held until its decision's deadline, is let go there, and a press then sends a decision again", async ({
  label,
  pressed,
}) => {
  const user = userEvent.setup();
  const lookupMachineCredential = spyOn(api, "lookupMachineCredential").mockResolvedValue(
    machineRecord()
  );
  const getMachineLogins = spyOn(api, "getMachineLogins").mockResolvedValue({ credentials: [] });
  const decisions = [
    spyOn(api, "approveCredentialRecord").mockRejectedValue(dropped()),
    spyOn(api, "denyCredentialRecord").mockRejectedValue(dropped()),
  ];
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  try {
    renderPage(queryClient);
    await user.click(screen.getByLabelText("Code shown on the machine"));
    await user.paste("abcd1234");
    await user.click(screen.getByRole("button", { name: "Look up" }));
    await screen.findByText("worker-7.example.com");
    jest.useFakeTimers();
    const onFakeClock = userOnFakeClock();
    await heldUntilLate(
      onFakeClock,
      screen.getByRole<HTMLButtonElement>("button", { name: pressed }),
      {
        label,
        name: pressed,
        queryClient,
        sent: () => pressesOf(decisions),
      }
    );
    await onFakeClock.keyboard("{Enter}");
    await tick(1_000);
    expect(pressesOf(decisions), "a press past the deadline sent no decision").toBe(2);
  } finally {
    realClock();
    cleanup();
    queryClient.clear();
    lookupMachineCredential.mockRestore();
    getMachineLogins.mockRestore();
    for (const decision of decisions) decision.mockRestore();
  }
});

// An approval Dispatch made and lost its answer for (`409 KEY_COMMITTED`) looks the code up again
// for its decision, a lookup the approval starts outside Look up's guard and lets the guard go
// beside. While that lookup is out, Look up and Enter in the code field send no second one.
test("while the lookup an unanswered approval starts is out, Look up and Enter send no second lookup", async () => {
  const user = userEvent.setup();
  const lookedUpAgain = Promise.withResolvers<RecordResponse>();
  const lookupMachineCredential = spyOn(api, "lookupMachineCredential")
    .mockResolvedValueOnce(machineRecord())
    .mockReturnValue(lookedUpAgain.promise);
  const getMachineLogins = spyOn(api, "getMachineLogins").mockResolvedValue({ credentials: [] });
  const approveCredentialRecord = spyOn(api, "approveCredentialRecord").mockRejectedValue(
    new ApiError(409, { code: "KEY_COMMITTED", error: "made; the answer was lost" })
  );

  try {
    renderPage();

    const field = screen.getByLabelText("Code shown on the machine");
    await user.click(field);
    await user.paste("abcd1234");
    await user.click(screen.getByRole("button", { name: "Look up" }));
    expect(await screen.findByText("worker-7.example.com")).toBeDefined();

    await user.click(screen.getByRole("button", { name: "Approve" }));
    await waitFor(() => expect(lookupMachineCredential).toHaveBeenCalledTimes(2));
    const lookUp = screen.getByRole("button", { name: "Look up" });
    await waitFor(() => expect(lookUp).toHaveAttribute("aria-disabled", "true"));

    await user.click(lookUp);
    await user.click(field);
    await user.keyboard("{Enter}");
    await act(settle);
    expect(approveCredentialRecord).toHaveBeenCalledTimes(1);
    expect(lookupMachineCredential).toHaveBeenCalledTimes(2);
  } finally {
    cleanup();
    lookupMachineCredential.mockRestore();
    getMachineLogins.mockRestore();
    approveCredentialRecord.mockRestore();
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

/** The page over `queryClient`, on the held clock, with the code looked up when `answerLookup`
 *  is given: it answers the lookup the press of Look up sent, inside `act` once that press is
 *  out, as docs/testing.md ("The act rule of a Dispatch unit test") says. */
async function stoppedPage(
  queryClient: QueryClient,
  answerLookup?: (record: RecordResponse) => void
) {
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
  if (answerLookup !== undefined) {
    await act(async () => {
      answerLookup(machineRecord());
    });
    await screen.findByText("worker-7.example.com");
  }
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
    await stoppedPage(queryClient);
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
  const lookedUp = Promise.withResolvers<RecordResponse>();
  const lookup = spyOn(api, "lookupMachineCredential").mockReturnValue(lookedUp.promise);
  const getMachineLogins = spyOn(api, "getMachineLogins").mockResolvedValue({ credentials: [] });
  const approve = spyOn(api, "approveCredentialRecord").mockRejectedValue(dropped());
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  try {
    const user = await stoppedPage(queryClient, lookedUp.resolve);
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
  const lookedUp = Promise.withResolvers<RecordResponse>();
  const lookup = spyOn(api, "lookupMachineCredential").mockReturnValue(lookedUp.promise);
  const getMachineLogins = spyOn(api, "getMachineLogins").mockResolvedValue({ credentials: [] });
  const deny = spyOn(api, "denyCredentialRecord").mockRejectedValue(dropped());
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  try {
    const user = await stoppedPage(queryClient, lookedUp.resolve);
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
