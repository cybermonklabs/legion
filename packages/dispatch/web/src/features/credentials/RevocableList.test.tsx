import { beforeAll, expect, jest, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
// biome-ignore lint/style/noRestrictedImports: the two presses below land in one synchronous act, which user-event's awaited interactions cannot do
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ReactNode } from "react";

import { realClock, tick } from "../../__tests__/fake-clock";
import { pressHeld } from "../../__tests__/held-press";
import { keyedAttempt } from "../../__tests__/keyed-attempt";
import { stopTheLateWrite } from "../../__tests__/stop-late-write";
import { userOnFakeClock } from "../../__tests__/user";
import { ApiError, api, NetworkError } from "../../api/client";
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

  // A row's Revoke is the table's one control (`TableShell`'s `holdsControls`), so the table's box
  // takes no Tab stop of its own while a row draws it; an empty list draws no control, and its box
  // keeps the browser's default.
  test(`${section.heading}: the table's box takes tabindex -1 while a row draws Revoke, and none while the list is empty`, async () => {
    const stubs = section.stub(() => Promise.resolve());
    try {
      const revoke = await renderSection(section);
      expect(revoke.closest('[data-control="table-shell"]')?.getAttribute("tabindex")).toBe("-1");
      cleanup();
      stubs.clearList();
      const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
      render(<QueryClientProvider client={queryClient}>{section.section()}</QueryClientProvider>);
      const empty = await screen.findByText(section.empty);
      expect(empty.closest('[data-control="table-shell"]')?.hasAttribute("tabindex")).toBe(false);
    } finally {
      cleanup();
      stubs.restore();
    }
  });
}

// A grant nobody may revoke draws the line that says what ends it where its Revoke would be, so a
// list of only such grants draws no control, and the table's box keeps the browser's default.
test("Live grants: a list of grants nobody may revoke leaves the table's box with no tabindex", async () => {
  const list = spyOn(api, "getCredentialGrants").mockResolvedValue({
    grants: [
      {
        approver: null,
        created_at: "2026-10-03T12:05:00Z",
        decided_by: null,
        enrollment: { kind: "session", operator: "", runtime_id: "box-2", slot: null },
        expires_at: "2026-10-03T13:05:00Z",
        grant_id: "grant-2",
        granted: "automatic",
        names: ["WORKER_TOKEN"],
        record_id: null,
      },
    ],
  });
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  try {
    render(
      <QueryClientProvider client={queryClient}>
        <GrantsSection />
      </QueryClientProvider>
    );
    const line = await screen.findByText("Ends with its machine login");
    expect(screen.queryByRole("button", { name: "Revoke" })).toBeNull();
    expect(line.closest('[data-control="table-shell"]')?.hasAttribute("tabindex")).toBe(false);
  } finally {
    cleanup();
    queryClient.clear();
    list.mockRestore();
  }
});
