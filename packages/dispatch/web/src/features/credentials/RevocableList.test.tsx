import { beforeAll, expect, jest, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
// biome-ignore lint/style/noRestrictedImports: the two presses below land in one synchronous act, which user-event's awaited interactions cannot do
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ReactNode } from "react";

import { holdNotifications } from "../../__tests__/commits";
import { realClock, settle, tick } from "../../__tests__/fake-clock";
import { heldAnswers } from "../../__tests__/held-answers";
import { pressHeld } from "../../__tests__/held-press";
import { heldUntilLate, pressesOf } from "../../__tests__/held-until-late";
import { keyedAttempt } from "../../__tests__/keyed-attempt";
import { stopTheLateWrite } from "../../__tests__/stop-late-write";
import { userOnFakeClock } from "../../__tests__/user";
import { ApiError, api, NetworkError, REQUEST_DEADLINE_MS } from "../../api/client";
import { DispatchQueryClient } from "../../api/query-client";
import { warmMarkdownRenderer } from "../refs/markdown-engine";
import { stoppedWriteNote } from "../writes/keyed-write";
import { GrantsSection } from "./GrantsSection";
import { MachineLoginsSection } from "./MachineLoginsSection";
import { SignInsSection } from "./SignInsSection";

// The rows draw through the shared renderer, whose Markdown engine loads lazily, and they run in
// random order, so the file loads the engine before its first row, whichever row that is.
beforeAll(async () => {
  await warmMarkdownRenderer();
});

/** One section rendered through `RevocableList`, with its one listed row stubbed and its revoke
 *  call replaced by `revoke`; `clearList` makes the list answer no rows from then on, and `empty`
 *  is what the section shows then. */
interface Section {
  empty: string;
  heading: string;
  /** The listed row as the section's sentences of its revoke name it. */
  name: string;
  refusal: string;
  rowId: string;
  section: () => ReactNode;
  stub: (revoke: (id: string) => Promise<void>) => {
    clearList: () => void;
    revokeCalls: () => unknown[][];
    restore: () => void;
  };
}

const sections: Section[] = [
  {
    empty: "No live grants.",
    heading: "Live grants",
    name: "the grant of WORKER_TOKEN to session · box-1 · ada@example.com",
    refusal: "only the grant's approver or the person its session acts for may revoke it",
    rowId: "grant-1",
    section: () => <GrantsSection />,
    stub: (revoke) => {
      const list = spyOn(api, "getCredentialGrants").mockResolvedValue({
        grants: [
          {
            approver: null,
            created_at: "2026-10-03T12:05:00Z",
            decided_by: null,
            enrollment: {
              kind: "session",
              operator: "ada@example.com",
              runtime_id: "box-1",
              slot: null,
            },
            expires_at: "2026-10-03T13:05:00Z",
            grant_id: "grant-1",
            granted: "automatic",
            names: ["WORKER_TOKEN"],
            record_id: null,
          },
        ],
      });
      const spy = spyOn(api, "revokeCredentialGrant").mockImplementation(revoke);
      return {
        clearList: () => list.mockResolvedValue({ grants: [] }),
        restore: () => {
          list.mockRestore();
          spy.mockRestore();
        },
        revokeCalls: () => spy.mock.calls,
      };
    },
  },
  {
    empty: "No live machine logins.",
    heading: "Machine logins",
    name: "the machine login for devbox.example.com",
    refusal: "only the person who approved a person's machine login may revoke it",
    rowId: "cred-devbox",
    section: () => <MachineLoginsSection />,
    stub: (revoke) => {
      const list = spyOn(api, "getMachineLogins").mockResolvedValue({
        credentials: [
          {
            approved_by: "ada@example.com",
            credential_id: "cred-devbox",
            expired: false,
            expires_at: "2026-10-10T12:00:00Z",
            host: "devbox.example.com",
            issued_at: "2026-10-03T12:00:00Z",
            person: null,
            service: null,
            subject: null,
          },
        ],
      });
      const spy = spyOn(api, "revokeMachineLogin").mockImplementation(revoke);
      return {
        clearList: () => list.mockResolvedValue({ credentials: [] }),
        restore: () => {
          list.mockRestore();
          spy.mockRestore();
        },
        revokeCalls: () => spy.mock.calls,
      };
    },
  },
  {
    empty: "No sign-ins.",
    heading: "Sign-ins",
    name: "your sign-in on devbox.example.com",
    refusal: "only the person a sign-in is of may revoke it",
    rowId: "signin-1",
    section: () => <SignInsSection />,
    stub: (revoke) => {
      const list = spyOn(api, "getSignIns").mockResolvedValue({
        signins: [
          {
            created_at: "2026-10-03T12:00:00Z",
            last_used_at: "2026-10-03T12:30:00Z",
            machine: "devbox.example.com",
            signin_id: "signin-1",
            thumbprint: "NzbLsXh8uDCcd-6MNwXF4W_7noWXFZAfHkxZsRGC9Xs",
          },
        ],
      });
      const spy = spyOn(api, "revokeSignIn").mockImplementation(revoke);
      return {
        clearList: () => list.mockResolvedValue({ signins: [] }),
        restore: () => {
          list.mockRestore();
          spy.mockRestore();
        },
        revokeCalls: () => spy.mock.calls,
      };
    },
  },
];

async function renderSection(section: Section): Promise<HTMLElement> {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(<QueryClientProvider client={queryClient}>{section.section()}</QueryClientProvider>);
  const region = within(await screen.findByRole("region", { name: section.heading }));
  return region.findByRole("button", { name: "Revoke" });
}

for (const section of sections) {
  test(`${section.heading}: Revoke lets go at the write deadline and sends again`, async () => {
    const stubs = section.stub(async () => {
      throw new NetworkError("/api/v1/revoke", new TypeError("Failed to fetch"));
    });
    const confirm = spyOn(window, "confirm").mockReturnValue(true);
    const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    try {
      render(<QueryClientProvider client={queryClient}>{section.section()}</QueryClientProvider>);
      const button = await screen.findByRole<HTMLButtonElement>("button", { name: "Revoke" });
      jest.useFakeTimers();
      const user = userOnFakeClock();
      const sent = () => pressesOf([{ mock: { calls: stubs.revokeCalls() } }]);
      await heldUntilLate(user, button, {
        label: `${section.heading}: revoke ${section.rowId}`,
        name: "Revoke",
        queryClient,
        sent,
      });
      await user.keyboard("{Enter}");
      await tick(1_000);
      expect(sent()).toBe(2);
    } finally {
      realClock();
      cleanup();
      queryClient.clear();
      confirm.mockRestore();
      stubs.restore();
    }
  });

  // While the revoke is out Revoke is held (`heldPress`), never `disabled`: a `disabled` button
  // drops the focus the reader's press left on it to the page's body. The pressed Revoke keeps the
  // focus, a second press of it sends no second revoke, and it is free once the revoke settles.
  test(`${section.heading}: Revoke, held while its revoke is out, keeps the focus, and a second press sends nothing`, async () => {
    const user = userEvent.setup();
    const out = Promise.withResolvers<void>();
    const stubs = section.stub(() => out.promise);
    const confirm = spyOn(window, "confirm").mockReturnValue(true);
    try {
      const button = await renderSection(section);
      await pressHeld(user, button, { name: "Revoke", sent: () => stubs.revokeCalls().length });
      expect(stubs.revokeCalls()).toEqual([[section.rowId, keyedAttempt]]);

      await act(async () => out.resolve());
      const region = within(screen.getByRole("region", { name: section.heading }));
      await waitFor(() =>
        expect(region.getByRole("button", { name: "Revoke" })).not.toHaveAttribute("aria-disabled")
      );
    } finally {
      cleanup();
      confirm.mockRestore();
      stubs.restore();
    }
  });

  test(`${section.heading}: a double press before the button is held sends one revoke`, async () => {
    jest.useFakeTimers();
    const out = Promise.withResolvers<void>();
    const stubs = section.stub(() => out.promise);
    const confirm = spyOn(window, "confirm").mockReturnValue(true);
    try {
      const button = await renderSection(section);
      // Both presses land inside one act, so React re-renders only after both: the second reaches
      // the handler before the pending revoke holds the button, which only the guard stops.
      act(() => {
        // Two presses in one task: user-event waits between its events, so it cannot make them.
        fireEvent.click(button);
        // The second press before React re-renders and holds the button.
        fireEvent.click(button);
      });
      // A mutation calls its function a few microtasks after the click; a timer runs after
      // them all.
      await tick(0);
      expect(stubs.revokeCalls()).toEqual([[section.rowId, keyedAttempt]]);
      out.resolve();
    } finally {
      realClock();
      cleanup();
      confirm.mockRestore();
      stubs.restore();
    }
  });

  test(`${section.heading}: a refused revoke shows the broker's refusal`, async () => {
    const user = userEvent.setup();
    const stubs = section.stub(async () => {
      throw new ApiError(403, { code: "NOT_APPROVER", error: section.refusal });
    });
    const confirm = spyOn(window, "confirm").mockReturnValue(true);
    try {
      await user.click(await renderSection(section));
      expect(
        await screen.findByText(`Couldn't revoke ${section.name} — ${section.refusal}`)
      ).toBeDefined();
    } finally {
      cleanup();
      confirm.mockRestore();
      stubs.restore();
    }
  });

  // One line under the table speaks for every row, so it is the refused row's for as long as that
  // row is listed: once the row leaves the list - revoked elsewhere, or expired - the line goes
  // with it, rather than stand under rows it was never about.
  test(`${section.heading}: a refused revoke's line goes once its row leaves the list`, async () => {
    const user = userEvent.setup();
    const stubs = section.stub(async () => {
      throw new ApiError(403, { code: "NOT_APPROVER", error: section.refusal });
    });
    const confirm = spyOn(window, "confirm").mockReturnValue(true);
    const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    try {
      render(<QueryClientProvider client={queryClient}>{section.section()}</QueryClientProvider>);
      const region = within(await screen.findByRole("region", { name: section.heading }));
      await user.click(await region.findByRole("button", { name: "Revoke" }));
      const refusal = `Couldn't revoke ${section.name} — ${section.refusal}`;
      expect(await region.findByText(refusal)).toBeDefined();

      stubs.clearList();
      await act(() => queryClient.invalidateQueries());
      expect(await region.findByText(section.empty)).toBeDefined();
      expect(region.queryByText(refusal)).not.toBeInTheDocument();
    } finally {
      cleanup();
      queryClient.clear();
      confirm.mockRestore();
      stubs.restore();
    }
  });

  test(`${section.heading}: a revoke refreshes the list, so the revoked row leaves it`, async () => {
    const user = userEvent.setup();
    // The revoke answers as the broker would: once it succeeds, the list no longer holds the row.
    const stubs = section.stub(async () => stubs.clearList());
    const confirm = spyOn(window, "confirm").mockReturnValue(true);
    try {
      await user.click(await renderSection(section));
      const region = within(screen.getByRole("region", { name: section.heading }));
      expect(await region.findByText(section.empty)).toBeDefined();
      expect(region.queryByRole("button", { name: "Revoke" })).not.toBeInTheDocument();
      expect(stubs.revokeCalls()).toEqual([[section.rowId, keyedAttempt]]);
    } finally {
      cleanup();
      confirm.mockRestore();
      stubs.restore();
    }
  });

  // A revoke the reader stopped may have been made: the list says so, and that Revoke is a press
  // of its own, rather than a failure it cannot know of.
  test(`${section.heading}: a revoke the reader stopped says it may have been made, not that it failed`, async () => {
    jest.useFakeTimers();
    const user = userOnFakeClock();
    const stubs = section.stub(async () => {
      throw new NetworkError("/api/v1/revoke", new TypeError("Failed to fetch"));
    });
    const confirm = spyOn(window, "confirm").mockReturnValue(true);
    const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    try {
      render(<QueryClientProvider client={queryClient}>{section.section()}</QueryClientProvider>);
      const region = within(await screen.findByRole("region", { name: section.heading }));
      await user.click(await region.findByRole("button", { name: "Revoke" }));
      await stopTheLateWrite(queryClient);
      expect(
        region.getByText(
          stoppedWriteNote(`the revoke of ${section.name}`, "made", {
            anew: "Pressing Revoke again sends a new revoke.",
          })
        )
      ).toBeDefined();
    } finally {
      realClock();
      cleanup();
      queryClient.clear();
      confirm.mockRestore();
      stubs.restore();
    }
  });
}

// A revoke that lands takes its row away once the list is read again, and the browser would leave
// the focus the reader's press left on Revoke on the page's body. The list's own hand-off
// (`useFocusPlace`), armed by the press at the list's root (`ARM_ON_PRESS`), gives it to the
// Revoke before it, a control the reader can see. The press counts while Revoke is held, and
// Revoke is held through the read that takes the row away, so a read that answers well after the
// revoke still finds the press live.
test("Sign-ins: Revoke by the keyboard hands the focus to the Revoke before it once its row leaves the list", async () => {
  const user = userEvent.setup();
  const signIn = (signin_id: string, machine: string) => ({
    created_at: "2026-10-03T12:00:00Z",
    last_used_at: "2026-10-03T12:30:00Z",
    machine,
    signin_id,
    thumbprint: "NzbLsXh8uDCcd-6MNwXF4W_7noWXFZAfHkxZsRGC9Xs",
  });
  const kept = signIn("signin-1", "devbox.example.com");
  let revoked = false;
  const list = spyOn(api, "getSignIns").mockImplementation(async () => {
    if (!revoked) return { signins: [kept, signIn("signin-2", "laptop.example.com")] };
    // The read after the revoke answers long after the frame and the task a press outlives
    // the hold that kept it live by.
    const later = Promise.withResolvers<void>();
    setTimeout(later.resolve, 200);
    await later.promise;
    return { signins: [kept] };
  });
  const revoke = spyOn(api, "revokeSignIn").mockImplementation(async () => {
    revoked = true;
  });
  const confirm = spyOn(window, "confirm").mockReturnValue(true);
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  try {
    render(
      <QueryClientProvider client={queryClient}>
        <SignInsSection />
      </QueryClientProvider>
    );
    const region = within(await screen.findByRole("region", { name: "Sign-ins" }));
    const [before, pressed] = await region.findAllByRole("button", { name: "Revoke" });
    if (before === undefined || pressed === undefined) throw new Error("two rows, two Revokes");
    act(() => pressed.focus());
    await user.keyboard("{Enter}");

    await waitFor(() => expect(region.getAllByRole("button", { name: "Revoke" })).toHaveLength(1), {
      timeout: 2_000,
    });
    expect(revoke.mock.calls).toEqual([["signin-2", keyedAttempt]]);
    await waitFor(() => expect(before).toHaveFocus());
  } finally {
    cleanup();
    queryClient.clear();
    confirm.mockRestore();
    list.mockRestore();
    revoke.mockRestore();
  }
});

// The line under the table for a list read the bound ended is that revoke's own: the next press of
// Revoke takes it away as its revoke lands, though the earlier read is still out, and the read
// after it, once answered, takes the row away. Dispatch holds the first revoke's read until the
// row answers it, so the next revoke's read, which waits for that one (`DispatchQueryClient`),
// answers after it.
test("Sign-ins: the line a revoke's read the bound ended draws goes as the next revoke lands, whose read takes the row away", async () => {
  const list = spyOn(api, "getSignIns").mockResolvedValue({
    signins: [
      {
        created_at: "2026-10-03T12:00:00Z",
        last_used_at: "2026-10-03T12:30:00Z",
        machine: "devbox.example.com",
        signin_id: "signin-1",
        thumbprint: "NzbLsXh8uDCcd-6MNwXF4W_7noWXFZAfHkxZsRGC9Xs",
      },
    ],
  });
  // Each revoke's answer, held until the row moves the clock, so what it sets off runs in `act`.
  const { answers: revokes, next } = heldAnswers<void>();
  const revoke = spyOn(api, "revokeSignIn").mockImplementation(next);
  const confirm = spyOn(window, "confirm").mockReturnValue(true);
  const queryClient = new DispatchQueryClient({ defaultOptions: { queries: { retry: false } } });
  const note = /^Couldn't load sign-ins\./;
  try {
    render(
      <QueryClientProvider client={queryClient}>
        <SignInsSection />
      </QueryClientProvider>
    );
    const region = within(await screen.findByRole("region", { name: "Sign-ins" }));
    const button = await region.findByRole("button", { name: "Revoke" });
    const reread = Promise.withResolvers<{ signins: [] }>();
    list.mockResolvedValue({ signins: [] });
    list.mockReturnValueOnce(reread.promise);
    jest.useFakeTimers();
    const user = userOnFakeClock();
    await user.click(button);
    await tick(0);
    revokes[0]?.();
    await tick(0);
    expect(button).toHaveAttribute("aria-disabled", "true");
    await tick(REQUEST_DEADLINE_MS);
    expect(button).not.toHaveAttribute("aria-disabled");
    expect(region.getByText(note).textContent).toBe(
      "Couldn't load sign-ins. Dispatch did not answer within 30 seconds."
    );

    await user.click(button);
    await tick(0);
    expect(revoke).toHaveBeenCalledTimes(2);
    revokes[1]?.();
    await tick(0);
    expect(button).toHaveAttribute("aria-disabled", "true");
    expect(
      region.queryByText(note),
      "the earlier revoke's read kept its line past the next revoke"
    ).toBeNull();

    await act(async () => reread.resolve({ signins: [] }));
    await tick(0);
    expect(region.getByText("No sign-ins.")).toBeInTheDocument();
    expect(region.queryByText(note)).toBeNull();
  } finally {
    cleanup();
    queryClient.clear();
    realClock();
    confirm.mockRestore();
    list.mockRestore();
    revoke.mockRestore();
  }
});

// The request deadline ends a revoke's hold and draws the line under the table in one change of
// the hold's store, and the revoke, which `onSuccess` keeps pending through that hold, stops
// pending as it ends. Revoke lets go in that same task, so a press of it once the line says why
// sends the next revoke. A browser tells TanStack's observers of the revoke's end a timer later
// (`holdNotifications`), so the row holds those notifications back. The list's read starts as the
// revoke lands, before the clock moves, so one jump of the deadline lands on the bound's own
// timer, and only microtasks run after it.
test("Sign-ins: Revoke is let go in the task the line under the table says why at the request deadline, and a press there revokes", async () => {
  const list = spyOn(api, "getSignIns").mockResolvedValue({
    signins: [
      {
        created_at: "2026-10-03T12:00:00Z",
        last_used_at: "2026-10-03T12:30:00Z",
        machine: "devbox.example.com",
        signin_id: "signin-1",
        thumbprint: "NzbLsXh8uDCcd-6MNwXF4W_7noWXFZAfHkxZsRGC9Xs",
      },
    ],
  });
  // Each revoke's answer, held until the row gives it, so what it sets off runs in `act`.
  const { answers: revokes, next } = heldAnswers<void>();
  const revoke = spyOn(api, "revokeSignIn").mockImplementation(next);
  const confirm = spyOn(window, "confirm").mockReturnValue(true);
  const queryClient = new DispatchQueryClient({ defaultOptions: { queries: { retry: false } } });
  const reread = Promise.withResolvers<{ signins: [] }>();
  try {
    render(
      <QueryClientProvider client={queryClient}>
        <SignInsSection />
      </QueryClientProvider>
    );
    const region = within(await screen.findByRole("region", { name: "Sign-ins" }));
    const button = await region.findByRole("button", { name: "Revoke" });
    list.mockReturnValueOnce(reread.promise);
    jest.useFakeTimers();
    const user = userOnFakeClock();
    await user.click(button);
    await tick(0);
    await act(async () => revokes[0]?.());
    expect(list).toHaveBeenCalledTimes(2);
    expect(button).toHaveAttribute("aria-disabled", "true");

    const notifications = holdNotifications();
    try {
      await act(async () => {
        jest.advanceTimersByTime(REQUEST_DEADLINE_MS);
        await settle();
      });
      expect(region.getByText(/^Couldn't load sign-ins\./).textContent).toBe(
        "Couldn't load sign-ins. Dispatch did not answer within 30 seconds."
      );
      expect(button, "the line said why while Revoke was held").not.toHaveAttribute(
        "aria-disabled"
      );
      await user.click(button);
    } finally {
      notifications.tell();
    }
    await tick(0);
    expect(revoke).toHaveBeenCalledTimes(2);
  } finally {
    reread.resolve({ signins: [] });
    cleanup();
    queryClient.clear();
    realClock();
    confirm.mockRestore();
    list.mockRestore();
    revoke.mockRestore();
  }
});
