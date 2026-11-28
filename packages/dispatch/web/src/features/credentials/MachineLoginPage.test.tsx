import { beforeAll, expect, jest, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { realClock } from "../../__tests__/fake-clock";
import { keyedAttempt } from "../../__tests__/keyed-attempt";
import { stopTheLateWrite } from "../../__tests__/stop-late-write";
import { userOnFakeClock } from "../../__tests__/user";
import { ApiError, api, NetworkError } from "../../api/client";
import type { DecisionResponse, RecordResponse } from "../../api/types";
import { warmMarkdownRenderer } from "../refs/markdown-engine";
import { stoppedWriteNote } from "../writes/keyed-write";
import { MachineLoginPage } from "./MachineLoginPage";

// The rows draw through the shared renderer, whose Markdown engine loads lazily, and they run in
// random order, so the file loads the engine before its first row, whichever row that is.
beforeAll(async () => {
  await warmMarkdownRenderer();
});

function machineRecord(overrides: Partial<RecordResponse> = {}): RecordResponse {
  const { dispatch_scopes = null, ...rest } = overrides;
  return {
    approver: "sami",
    decided: null,
    dispatch_scopes,
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
    ...rest,
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

test("a Dispatch sign-in shows its asked scopes and sends the ticked subset", async () => {
  const user = userEvent.setup();
  const record = machineRecord({ dispatch_scopes: ["person"] });
  const lookupMachineCredential = spyOn(api, "lookupMachineCredential").mockResolvedValue(record);
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

    const person = (await screen.findByRole("checkbox", {
      name: "Act as you",
    })) as HTMLInputElement;
    expect(person.checked).toBe(true);
    expect(
      screen.getByText(
        "Whatever is ticked, approving lets this session or tool write in Dispatch and work on Envoy as its own session within your projects, and it reads no secret; Act as you adds acting as you."
      )
    ).toBeDefined();

    await user.click(person);
    await user.click(screen.getByRole("button", { name: "Approve" }));
    await waitFor(() =>
      expect(approveCredentialRecord).toHaveBeenCalledWith(
        "req-2",
        { code: "ABCD-1234", scopes: [] },
        keyedAttempt
      )
    );
  } finally {
    cleanup();
    lookupMachineCredential.mockRestore();
    getMachineLogins.mockRestore();
    approveCredentialRecord.mockRestore();
  }
});

test("an approved Dispatch sign-in names the scopes Approve sent, not the ones it asked", async () => {
  const user = userEvent.setup();
  const record = machineRecord({ dispatch_scopes: ["person"] });
  const lookupMachineCredential = spyOn(api, "lookupMachineCredential").mockResolvedValue(record);
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
    await user.click(await screen.findByRole("checkbox", { name: "Act as you" }));
    await user.click(screen.getByRole("button", { name: "Approve" }));

    expect(
      await screen.findByText(
        "Approved. worker-7.example.com is signed in to Dispatch with no scope."
      )
    ).toBeDefined();
  } finally {
    cleanup();
    lookupMachineCredential.mockRestore();
    getMachineLogins.mockRestore();
    approveCredentialRecord.mockRestore();
  }
});

// A sign-in's reason tells it from another sign-in of the same host, so its decided view names it.
test("an approved Dispatch sign-in with a reason names it beside its host", async () => {
  const user = userEvent.setup();
  const record = machineRecord({ dispatch_scopes: ["person"], reason: "the release tool" });
  const lookupMachineCredential = spyOn(api, "lookupMachineCredential").mockResolvedValue(record);
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
    await user.click(await screen.findByRole("button", { name: "Approve" }));

    expect(
      await screen.findByText(
        "Approved. worker-7.example.com (the release tool) is signed in to Dispatch with Act as you."
      )
    ).toBeDefined();
  } finally {
    cleanup();
    lookupMachineCredential.mockRestore();
    getMachineLogins.mockRestore();
    approveCredentialRecord.mockRestore();
  }
});

// The success view names the scopes the press carried: a box unticked while the approval is still
// out changes neither the request nor what the view says once it is answered.
test("an approval still out keeps the scopes its press sent, whatever the boxes hold after", async () => {
  const user = userEvent.setup();
  const record = machineRecord({ dispatch_scopes: ["person"] });
  const lookupMachineCredential = spyOn(api, "lookupMachineCredential").mockResolvedValue(record);
  const getMachineLogins = spyOn(api, "getMachineLogins").mockResolvedValue({ credentials: [] });
  // Held, and given inside `act` after the press, as docs/testing.md ("The act rule of a Dispatch
  // unit test") says.
  const decided = Promise.withResolvers<DecisionResponse>();
  const approveCredentialRecord = spyOn(api, "approveCredentialRecord").mockReturnValue(
    decided.promise
  );

  try {
    renderPage();
    await user.click(screen.getByLabelText("Code shown on the machine"));
    await user.paste("abcd1234");
    await user.click(screen.getByRole("button", { name: "Look up" }));
    const person = await screen.findByRole("checkbox", { name: "Act as you" });
    await user.click(screen.getByRole("button", { name: "Approve" }));
    await waitFor(() =>
      expect(approveCredentialRecord).toHaveBeenCalledWith(
        "req-2",
        { code: "ABCD-1234", scopes: ["person"] },
        keyedAttempt
      )
    );

    await user.click(person);
    await act(async () => {
      decided.resolve({ credential_id: "cred-9", state: "approved" });
    });

    expect(
      await screen.findByText(
        "Approved. worker-7.example.com is signed in to Dispatch with Act as you."
      )
    ).toBeDefined();
    expect(approveCredentialRecord).toHaveBeenCalledTimes(1);
  } finally {
    cleanup();
    lookupMachineCredential.mockRestore();
    getMachineLogins.mockRestore();
    approveCredentialRecord.mockRestore();
  }
});

// A broker newer than this page may take a scope the page has no words for. The page draws the
// word as itself, unticked, so the person never approves a scope the page did not show them; the
// ticks start from the scopes it draws in its own words, and Approve sends only ticked boxes.
test("a Dispatch sign-in's unknown scope shows as its word, unticked, and is sent only once ticked", async () => {
  const user = userEvent.setup();
  const record = machineRecord({ dispatch_scopes: ["person", "triage"] });
  const lookupMachineCredential = spyOn(api, "lookupMachineCredential").mockResolvedValue(record);
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

    const person = (await screen.findByRole("checkbox", {
      name: "Act as you",
    })) as HTMLInputElement;
    const triage = screen.getByRole("checkbox", { name: "triage" }) as HTMLInputElement;
    expect(person.checked).toBe(true);
    expect(triage.checked).toBe(false);
    expect(
      screen.getByText("This page does not know what this scope allows: tick it only if you do.")
    ).toBeDefined();

    await user.click(screen.getByRole("button", { name: "Approve" }));
    await waitFor(() =>
      expect(approveCredentialRecord).toHaveBeenCalledWith(
        "req-2",
        { code: "ABCD-1234", scopes: ["person"] },
        keyedAttempt
      )
    );
    expect(
      await screen.findByText(
        "Approved. worker-7.example.com is signed in to Dispatch with Act as you."
      )
    ).toBeDefined();
  } finally {
    cleanup();
    lookupMachineCredential.mockRestore();
    getMachineLogins.mockRestore();
    approveCredentialRecord.mockRestore();
  }
});

// p0 lets the session make an issue's own priority 0, which the person gives one sign-in only by
// ticking it (dispatch://INFRA-5943/ask/3dfbf22b-74af-4e6d-af17-3bf368aac9fa): the page draws it
// in its Settings words and unticked, though the sign-in asked for it, beside person ticked, so
// an Approve press that ticks nothing more sends no p0.
test("a Dispatch sign-in asking for p0 draws it unticked, and Approve without ticking it sends no p0", async () => {
  const user = userEvent.setup();
  const record = machineRecord({ dispatch_scopes: ["p0", "person"] });
  const lookupMachineCredential = spyOn(api, "lookupMachineCredential").mockResolvedValue(record);
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

    const p0 = (await screen.findByRole("checkbox", { name: "Set P0" })) as HTMLInputElement;
    const person = screen.getByRole("checkbox", { name: "Act as you" }) as HTMLInputElement;
    expect(p0.checked).toBe(false);
    expect(person.checked).toBe(true);
    expect(screen.getByText("Set an issue's priority to P0.")).toBeDefined();

    await user.click(screen.getByRole("button", { name: "Approve" }));
    await waitFor(() =>
      expect(approveCredentialRecord).toHaveBeenCalledWith(
        "req-2",
        { code: "ABCD-1234", scopes: ["person"] },
        keyedAttempt
      )
    );
    expect(
      await screen.findByText(
        "Approved. worker-7.example.com is signed in to Dispatch with Act as you."
      )
    ).toBeDefined();
  } finally {
    cleanup();
    lookupMachineCredential.mockRestore();
    getMachineLogins.mockRestore();
    approveCredentialRecord.mockRestore();
  }
});

// The control of the row above: the same sign-in, with the person ticking p0, sends it.
test("a Dispatch sign-in's p0, once its person ticks it, is sent with the approval", async () => {
  const user = userEvent.setup();
  const record = machineRecord({ dispatch_scopes: ["p0", "person"] });
  const lookupMachineCredential = spyOn(api, "lookupMachineCredential").mockResolvedValue(record);
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

    const p0 = (await screen.findByRole("checkbox", { name: "Set P0" })) as HTMLInputElement;
    await user.click(p0);
    expect(p0.checked).toBe(true);

    await user.click(screen.getByRole("button", { name: "Approve" }));
    await waitFor(() =>
      expect(approveCredentialRecord).toHaveBeenCalledWith(
        "req-2",
        { code: "ABCD-1234", scopes: ["person", "p0"] },
        keyedAttempt
      )
    );
    expect(
      await screen.findByText(
        "Approved. worker-7.example.com is signed in to Dispatch with Act as you, Set P0."
      )
    ).toBeDefined();
  } finally {
    cleanup();
    lookupMachineCredential.mockRestore();
    getMachineLogins.mockRestore();
    approveCredentialRecord.mockRestore();
  }
});

// A sign-in that asked for no scope says so, in place of an empty set of boxes, and says only what
// an approval lets it do: it writes and works on Envoy as its own session. With no box to tick, it
// says nothing of what is ticked and nothing of Act as you; the sign-in that asks for person
// ("a Dispatch sign-in shows its asked scopes and sends the ticked subset") keeps those words.
test("a Dispatch sign-in that asks for no scope says so, and only what approving it allows", async () => {
  const user = userEvent.setup();
  const record = machineRecord({ dispatch_scopes: [] });
  const lookupMachineCredential = spyOn(api, "lookupMachineCredential").mockResolvedValue(record);
  const getMachineLogins = spyOn(api, "getMachineLogins").mockResolvedValue({ credentials: [] });

  try {
    renderPage();
    await user.click(screen.getByLabelText("Code shown on the machine"));
    await user.paste("abcd1234");
    await user.click(screen.getByRole("button", { name: "Look up" }));

    expect(await screen.findByText("It asks for no scope.")).toBeDefined();
    expect(screen.queryByRole("checkbox")).toBeNull();
    expect(
      screen.getByText(
        "Approving lets this session or tool write in Dispatch and work on Envoy as its own session within your projects, and it reads no secret."
      )
    ).toBeDefined();
    expect(screen.queryByText(/Whatever is ticked/)).toBeNull();
    expect(screen.queryByText(/Act as you/)).toBeNull();
  } finally {
    cleanup();
    lookupMachineCredential.mockRestore();
    getMachineLogins.mockRestore();
  }
});

// A sign-in looked up once decided carries the scopes it asked for, not the ones approved, so its
// line names the asked scopes as asked and sends the person to the list that names the approved.
test("a Dispatch sign-in looked up once approved names what it asked for, not what was approved", async () => {
  const user = userEvent.setup();
  const record = machineRecord({
    decided: { at: "2026-09-27T00:05:00Z", credential_id: "cred-9", event: "approved" },
    dispatch_scopes: ["person"],
    state: "approved",
  });
  const lookupMachineCredential = spyOn(api, "lookupMachineCredential").mockResolvedValue(record);
  const getMachineLogins = spyOn(api, "getMachineLogins").mockResolvedValue({ credentials: [] });

  try {
    renderPage();
    await user.click(screen.getByLabelText("Code shown on the machine"));
    await user.paste("abcd1234");
    await user.click(screen.getByRole("button", { name: "Look up" }));

    expect(
      await screen.findByText(
        "Approved. worker-7.example.com is signed in to Dispatch. It asked for Act as you; Machine logins below names the scopes approved."
      )
    ).toBeDefined();
  } finally {
    cleanup();
    lookupMachineCredential.mockRestore();
    getMachineLogins.mockRestore();
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
