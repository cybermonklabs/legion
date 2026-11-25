import { afterEach, beforeAll, beforeEach, expect, jest, type Mock, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";

import { realClock } from "../../__tests__/fake-clock";
import { keyedAttempt } from "../../__tests__/keyed-attempt";
import { stopTheLateWrite } from "../../__tests__/stop-late-write";
import { userOnFakeClock } from "../../__tests__/user";
import { ApiError, api, NetworkError } from "../../api/client";
import type { CredentialPendingRow } from "../../api/types";
import { warmMarkdownRenderer } from "../refs/markdown-engine";
import { stoppedWriteNote } from "../writes/keyed-write";
import { runningAgent } from "./agent-fixture";
import { CredentialRequestsSection } from "./CredentialRequestsSection";
import type { CredentialRequests } from "./pending";

// The rows draw through the shared renderer, whose Markdown engine loads lazily, and they run in
// random order, so the file loads the engine before its first row, whichever row that is.
beforeAll(async () => {
  await warmMarkdownRenderer();
});

afterEach(cleanup);

/** Whether this deployment serves the machine-code page, which every pending row reads
 *  (`useMachineCodePage`, GET /api/v1/credentials/capabilities): it serves none unless the test
 *  under way says it does. */
let capabilities: Mock<typeof api.getCredentialCapabilities>;

beforeEach(() => {
  capabilities = spyOn(api, "getCredentialCapabilities").mockResolvedValue({
    machine_code_page: false,
  });
});
afterEach(() => capabilities.mockRestore());

function pendingSecretRow(overrides: Partial<CredentialPendingRow> = {}): CredentialPendingRow {
  return {
    identifiers: ["ANTHROPIC_API_KEY"],
    kind: "agent_secret",
    record_id: "req-1",
    requested_at: "2026-09-27T00:00:00Z",
    session: { enrollment: null, request: null },
    ...overrides,
  };
}

/** An expired secret request: its deadline passed two hours ago, before anybody decided it. It
 *  carries the broker's page, which an expired row must not link to. */
function expiredSecretRow(overrides: Partial<CredentialPendingRow> = {}): CredentialPendingRow {
  return pendingSecretRow({
    approve_url: "https://secrets.example.com/approve/req-9",
    expires_at: new Date(Date.now() - 2 * 60 * 60 * 1000).toISOString(),
    identifiers: ["DEEL_API_KEY"],
    record_id: "req-9",
    state: "expired",
    ...overrides,
  });
}

function renderSection(
  credentials: CredentialRequests,
  client = new QueryClient({ defaultOptions: { queries: { retry: false } } })
) {
  return render(
    <MemoryRouter>
      <QueryClientProvider client={client}>
        <CredentialRequestsSection credentials={credentials} />
      </QueryClientProvider>
    </MemoryRouter>
  );
}

// The machine row links to `/credentials/machine` only where this deployment serves that page,
// which the credential capabilities say (`useMachineCodePage`, GET
// /api/v1/credentials/capabilities), so the row's link is there once that answer is in.
test("renders every pending credential request, linking a secret row to its broker page and a machine row to /credentials/machine", async () => {
  const machineRow: CredentialPendingRow = {
    identifiers: ["worker-7.example.com"],
    kind: "launcher_credential",
    record_id: "req-2",
    requested_at: "2026-09-27T01:00:00Z",
    session: { enrollment: null, request: null },
  };
  capabilities.mockResolvedValue({ machine_code_page: true });
  renderSection({
    requests: [
      pendingSecretRow({ approve_url: "https://secrets.example.com/approve/req-1" }),
      machineRow,
    ],
    status: "listed",
  });

  expect(screen.getByText("Secret request")).toBeDefined();
  expect(screen.getByText("Machine login")).toBeDefined();
  expect(screen.getByText("ANTHROPIC_API_KEY")).toBeDefined();
  expect(screen.getByText("worker-7.example.com")).toBeDefined();
  const secret = screen.getByRole("link", { name: /Secret request.*ANTHROPIC_API_KEY/s });
  expect(secret).toHaveAttribute("href", "https://secrets.example.com/approve/req-1");
  expect(secret).not.toHaveAttribute("target");
  const machine = await screen.findByRole("link", {
    name: /Machine login.*worker-7\.example\.com/s,
  });
  expect(machine).toHaveAttribute("href", "/credentials/machine");
  expect(screen.getAllByText("No session named.")).toHaveLength(2);
});

// A broker older than its own approval page names none, and an approve_url that is not an
// absolute http(s) URL is no page to send anyone to: either row reads as the same summary with
// no link, since Dispatch decides no secret request of its own any more.
test("a secret row with no usable approve_url renders its summary with no link", () => {
  for (const approve_url of [undefined, "javascript:alert(1)", "/approve/req-1"]) {
    const view = renderSection({ requests: [pendingSecretRow({ approve_url })], status: "listed" });
    expect(screen.getByText("ANTHROPIC_API_KEY")).toBeDefined();
    expect(
      screen.queryByRole("link", { name: /Secret request.*ANTHROPIC_API_KEY/s })
    ).not.toBeInTheDocument();
    view.unmount();
  }
});

// A row an older broker sends carries no `state`, and a newer broker's pending row says
// `pending`: either is a pending request, a link to where it is decided, with no mark and nothing
// to dismiss.
test("a row with no state, or a pending one, links to its broker page with no mark and no Dismiss", () => {
  for (const state of [undefined, "pending"] as const) {
    const view = renderSection({
      requests: [
        pendingSecretRow({ approve_url: "https://secrets.example.com/approve/req-1", state }),
      ],
      status: "listed",
    });
    expect(
      screen.getByRole("link", { name: /Secret request.*ANTHROPIC_API_KEY/s })
    ).toHaveAttribute("href", "https://secrets.example.com/approve/req-1");
    expect(screen.queryByText("Expired")).not.toBeInTheDocument();
    expect(screen.queryByText(/^Its deadline passed/)).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /^Dismiss/ })).not.toBeInTheDocument();
    view.unmount();
  }
});

// INFRA-3921: a request whose deadline passed before anybody decided it stays listed until
// somebody dismisses it, and every decision of it is refused. So neither kind links to where a
// pending one is decided - not the broker's page, not the machine-code page, which this
// deployment serves, as the pending machine row's link shows - and each is marked, says when its
// deadline passed and offers Dismiss, under a name that says which row it dismisses.
test("an expired row of either kind is marked, says when its deadline passed, links nowhere and offers Dismiss", async () => {
  const deadline = new Date(Date.now() - 2 * 60 * 60 * 1000).toISOString();
  const machineRow = (
    identifier: string,
    overrides: Partial<CredentialPendingRow> = {}
  ): CredentialPendingRow => ({
    identifiers: [identifier],
    kind: "launcher_credential",
    record_id: `machine-${identifier}`,
    requested_at: "2026-09-27T01:00:00Z",
    session: { enrollment: null, request: null },
    ...overrides,
  });
  capabilities.mockResolvedValue({ machine_code_page: true });
  renderSection({
    requests: [
      machineRow("worker-8.example.com"),
      expiredSecretRow({ expires_at: deadline }),
      machineRow("worker-7.example.com", { expires_at: deadline, state: "expired" }),
    ],
    status: "listed",
  });

  const pendingMachine = await screen.findByRole("link", {
    name: /Machine login.*worker-8\.example\.com/s,
  });
  expect(pendingMachine).toHaveAttribute("href", "/credentials/machine");
  expect(within(pendingMachine.closest("li") as HTMLElement).queryByRole("button")).toBeNull();
  expect(document.querySelector('a[href="https://secrets.example.com/approve/req-9"]')).toBeNull();

  for (const [identifier, name] of [
    ["DEEL_API_KEY", "Dismiss expired secret request DEEL_API_KEY"],
    ["worker-7.example.com", "Dismiss expired machine login worker-7.example.com"],
  ] as const) {
    const row = screen.getByText(identifier).closest("li") as HTMLElement;
    expect(within(row).getByText("Expired")).toBeDefined();
    expect(within(row).queryByRole("link")).toBeNull();
    expect(within(row).getByRole("button", { name })).toHaveTextContent("Dismiss");
    const time = row.querySelector(`time[datetime="${deadline}"]`);
    expect(time).not.toBeNull();
    expect(within(row).getByText(/^Its deadline passed/).textContent).toBe(
      `Its deadline passed ${time?.textContent}`
    );
    expect(within(row).getByText("No session named.")).toBeDefined();
  }
});

// A broker that names no deadline for an expired row still marks it expired: the line then says
// only that the deadline passed.
test("an expired row with no deadline says only that its deadline passed", () => {
  renderSection({ requests: [expiredSecretRow({ expires_at: undefined })], status: "listed" });
  const line = screen.getByText(/^Its deadline passed/);
  expect(line.textContent).toBe("Its deadline passed");
  expect(line.querySelector("time")).toBeNull();
});

test("Dismiss on an expired row sends that record's dismissal and reads the list again", async () => {
  const user = userEvent.setup();
  const dismiss = spyOn(api, "dismissCredentialRequest").mockResolvedValue(undefined);
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const invalidate = spyOn(client, "invalidateQueries");
  try {
    renderSection({ requests: [expiredSecretRow()], status: "listed" }, client);
    await user.click(
      screen.getByRole("button", { name: "Dismiss expired secret request DEEL_API_KEY" })
    );
    await waitFor(() => expect(dismiss.mock.calls).toEqual([["req-9", keyedAttempt]]));
    await waitFor(() =>
      expect(invalidate).toHaveBeenCalledWith({ queryKey: ["credential-pending"] })
    );
  } finally {
    dismiss.mockRestore();
  }
});

// The broker's refusal is shown under the row it names, and the list is read again, since the
// refusal says the list the page holds is not what the broker holds.
test("a refused dismissal shows the broker's message and reads the list again", async () => {
  const user = userEvent.setup();
  const refusal = "only an approver of this request may dismiss it";
  const dismiss = spyOn(api, "dismissCredentialRequest").mockRejectedValue(
    new ApiError(403, { code: "NOT_APPROVER", error: refusal })
  );
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const invalidate = spyOn(client, "invalidateQueries");
  try {
    renderSection({ requests: [expiredSecretRow()], status: "listed" }, client);
    await user.click(
      screen.getByRole("button", { name: "Dismiss expired secret request DEEL_API_KEY" })
    );
    const row = screen.getByText("DEEL_API_KEY").closest("li") as HTMLElement;
    expect(await within(row).findByText(refusal)).toBeDefined();
    expect(invalidate).toHaveBeenCalledWith({ queryKey: ["credential-pending"] });
  } finally {
    dismiss.mockRestore();
  }
});

// A dismissal the reader stopped may have been made: the row says so, and that Dismiss is a press
// of its own, rather than that the dismissal failed.
test("a dismissal the reader stopped says it may have been made, not that it failed", async () => {
  const dismiss = spyOn(api, "dismissCredentialRequest").mockRejectedValue(
    new NetworkError("/api/v1/credentials/requests/req-9/dismiss", new TypeError("Failed to fetch"))
  );
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  jest.useFakeTimers();
  try {
    renderSection({ requests: [expiredSecretRow()], status: "listed" }, client);
    const user = userOnFakeClock();
    await user.click(
      screen.getByRole("button", { name: "Dismiss expired secret request DEEL_API_KEY" })
    );
    await stopTheLateWrite(client);
    const row = screen.getByText("DEEL_API_KEY").closest("li") as HTMLElement;
    expect(
      within(row).getByText(
        stoppedWriteNote("the dismissal of this request", "made", {
          anew: "Pressing Dismiss again sends a new dismissal.",
        })
      )
    ).toBeDefined();
    expect(within(row).queryByText("Could not dismiss this request.")).toBeNull();
  } finally {
    realClock();
    dismiss.mockRestore();
  }
});

test("renders nothing while the list loads or when it lists none, a broker-less Dispatch's answer", () => {
  for (const status of ["loading", "listed"] as const) {
    const view = renderSection({ requests: [], status });
    expect(view.container.firstChild).toBeNull();
    view.unmount();
  }
});

test("surfaces a failure to load the list instead of hiding the section", () => {
  renderSection({ requests: [], status: "failed" });
  expect(screen.getByText("Couldn't load credential requests.")).toBeDefined();
});

// LEGION-587: a row whose session names a live agent links to it; one whose session names an id
// the agents list doesn't carry reads as not running, with neither mistaken for the other. Every
// line carries the full id: a request's id is an unsigned claim, and a short form is shared by
// any id with the same first eight characters.
test("a row's session reads as a running session's title, linked to its live conversation", async () => {
  const agents = spyOn(api, "listAgents").mockResolvedValue([runningAgent()]);
  try {
    renderSection({
      requests: [pendingSecretRow({ session: { enrollment: "sess-1", request: null } })],
      status: "listed",
    });
    const link = await screen.findByRole("link", { name: "Reviewing LEGION-587" });
    expect(link).toHaveAttribute("href", "/agents/sess-1/live");
    await waitFor(() =>
      expect(link.closest("[data-agent-name]")).toHaveAttribute("data-agent-state", "running")
    );
    expect(screen.getByRole("img", { name: "Running" })).toBeDefined();
    expect(link.closest("[data-agent-name]")?.parentElement?.textContent).toBe(
      "Reviewing LEGION-587 · sess-1 · /home/alice/legion · devbox-alice"
    );
  } finally {
    agents.mockRestore();
  }
});

// LEGION-587's review (round 3): `generatePath` does not escape its params, so a running
// session's own id - an unsigned claim nothing here can constrain the shape of - must be
// percent-encoded before it reaches the live link, or an id crafted with `/` and `..` segments
// would navigate somewhere other than `/agents/:sessionId/live`.
test("encodes a running session's id before building its live link", async () => {
  const agents = spyOn(api, "listAgents").mockResolvedValue([
    runningAgent({ session_id: "../../settings" }),
  ]);
  try {
    renderSection({
      requests: [pendingSecretRow({ session: { enrollment: "../../settings", request: null } })],
      status: "listed",
    });
    const link = await screen.findByRole("link", { name: "Reviewing LEGION-587" });
    expect(link).toHaveAttribute("href", "/agents/..%2F..%2Fsettings/live");
  } finally {
    agents.mockRestore();
  }
});

// An id the agents list does not carry is the agent's not-running state: its short id, marked
// not running, still linked, then its full id and "isn't running" in words, with no directory or
// machine.
test("a row's session naming an id the agents list doesn't carry reads as not running", async () => {
  const agents = spyOn(api, "listAgents").mockResolvedValue([]);
  try {
    renderSection({
      requests: [pendingSecretRow({ session: { enrollment: "sess-gone", request: null } })],
      status: "listed",
    });
    expect(await screen.findByRole("img", { name: "Not running" })).toBeDefined();
    const name = screen.getByRole("link", { name: "session:sess-gon…" });
    expect(name).toHaveAttribute("href", "/agents/sess-gone/live");
    const agent = name.closest("[data-agent-name]");
    expect(agent).toHaveAttribute("data-agent-state", "not-running");
    expect(agent?.parentElement?.textContent).toBe("session:sess-gon… · sess-gone isn't running.");
  } finally {
    agents.mockRestore();
  }
});

// Envoy unavailable or not configured: the agents list never answers, so an id is the agent's
// unknown state, never not running, and the line says in words, with the full id, that it
// couldn't check.
test("a row's session reads as unknown while the agents list cannot be read", async () => {
  const agents = spyOn(api, "listAgents").mockRejectedValue(new Error("envoy unavailable"));
  try {
    renderSection({
      requests: [pendingSecretRow({ session: { enrollment: "sess-gone", request: null } })],
      status: "listed",
    });
    await waitFor(() => expect(agents).toHaveBeenCalled());
    expect(await screen.findByRole("img", { name: "Running state unknown" })).toBeDefined();
    expect(
      screen.getByRole("link", { name: "session:sess-gon…" }).closest("[data-agent-name]")
        ?.parentElement?.textContent
    ).toBe("session:sess-gon… · couldn't check whether sess-gone is running.");
    expect(screen.queryByRole("img", { name: "Not running" })).not.toBeInTheDocument();
    expect(screen.queryByText(/isn't running/)).not.toBeInTheDocument();
  } finally {
    agents.mockRestore();
  }
});

// A request-only session id - the common case for a host Oh My Pi session, which has no enrollment
// session_id of its own - is shown as written, with no preamble.
test("a row's request-only session id is shown with no preamble", async () => {
  const agents = spyOn(api, "listAgents").mockResolvedValue([runningAgent()]);
  try {
    renderSection({
      requests: [pendingSecretRow({ session: { enrollment: null, request: "sess-1" } })],
      status: "listed",
    });
    const link = await screen.findByRole("link", { name: "Reviewing LEGION-587" });
    expect(link).toHaveAttribute("href", "/agents/sess-1/live");
    expect(
      link
        .closest("[data-agent-name]")
        ?.parentElement?.textContent?.startsWith("Reviewing LEGION-587")
    ).toBe(true);
    expect(screen.queryByText(/says it came from/)).not.toBeInTheDocument();
  } finally {
    agents.mockRestore();
  }
});

// LEGION-587's review (round 3): the whole pending-requests list reads the Agents list once,
// never once per row. Each row mounts more than the query's 10s `staleTime` after the last, so a
// per-row reader that refetched on mount (with already-stale cached data) would fire its own
// request - three rows would then cost three fetches instead of the one the initial mount
// already made. Refreshing it is the shell's one poller's job (`AgentRegistryPoller`).
test("reads the agents list once for the whole list, not once per pending row", async () => {
  const listAgents = spyOn(api, "listAgents").mockResolvedValue([runningAgent()]);
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const tree = (requests: CredentialPendingRow[]) => (
    <MemoryRouter>
      <QueryClientProvider client={client}>
        <CredentialRequestsSection credentials={{ requests, status: "listed" }} />
      </QueryClientProvider>
    </MemoryRouter>
  );
  jest.useFakeTimers();
  try {
    const row1 = pendingSecretRow({ session: { enrollment: "sess-1", request: null } });
    const { rerender } = render(tree([row1]));
    await act(async () => {});
    expect(listAgents).toHaveBeenCalledTimes(1);

    // A second row mounts 11s later - past the 10s staleTime.
    await act(async () => {
      jest.advanceTimersByTime(11_000);
    });
    const row2 = pendingSecretRow({
      record_id: "req-2",
      session: { enrollment: "sess-2", request: null },
    });
    rerender(tree([row1, row2]));
    await act(async () => {});

    // A third row mounts another 11s later.
    await act(async () => {
      jest.advanceTimersByTime(11_000);
    });
    const row3 = pendingSecretRow({
      record_id: "req-3",
      session: { enrollment: "sess-3", request: null },
    });
    rerender(tree([row1, row2, row3]));
    await act(async () => {});

    // Still the one fetch from the first mount: adding rows past the staleTime window never
    // starts a second read of the shared Agents list.
    expect(listAgents).toHaveBeenCalledTimes(1);
  } finally {
    jest.useRealTimers();
    listAgents.mockRestore();
  }
});
