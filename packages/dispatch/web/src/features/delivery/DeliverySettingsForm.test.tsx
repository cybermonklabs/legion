import { beforeAll, expect, jest, spyOn, test } from "bun:test";
import { onlineManager, QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, render, screen, waitFor } from "@testing-library/react";
import type { UserEvent } from "@testing-library/user-event";
import userEvent from "@testing-library/user-event";

import { holdNotifications } from "../../__tests__/commits";
import { realClock, settle, tick } from "../../__tests__/fake-clock";
import { afterFrameAndTasks } from "../../__tests__/frames";
import { heldAnswers } from "../../__tests__/held-answers";
import { pressHeld } from "../../__tests__/held-press";
import { heldUntilLate, pressesOf } from "../../__tests__/held-until-late";
import { seedDeliveryReads } from "../../__tests__/live-socket-harness";
import { stopTheLateWrite } from "../../__tests__/stop-late-write";
import { userOnFakeClock } from "../../__tests__/user";
import { ApiError, api, NetworkError, REQUEST_DEADLINE_MS } from "../../api/client";
import { deliverySettingsQuery } from "../../api/queries";
import { DispatchQueryClient } from "../../api/query-client";
import type { DeliverySettings } from "../../api/types";
import { emulateChromiumFocusFixup } from "../issue-page/chromium-focus-fixup";
import { warmMarkdownRenderer } from "../refs/markdown-engine";
import { stoppedWriteNote } from "../writes/keyed-write";
import { DeliverySettingsForm } from "./DeliverySettingsForm";

// The rows draw through the shared renderer, whose Markdown engine loads lazily, and they run in
// random order, so the file loads the engine before its first row, whichever row that is.
beforeAll(async () => {
  await warmMarkdownRenderer();
});

const record: DeliverySettings = {
  deploy_repo: "acme/widgets",
  deploy_workflow_path: ".github/workflows/deploy.yml",
  excluded_repos: ["acme/dojo", "acme/widgets-smoke"],
  last_error: null,
  last_event_at: null,
  last_reconcile_at: null,
  population_authors: ["octocat", "octocat-agent[bot]"],
  pr_checks_rollup_job_name: null,
  pr_checks_workflow_path: ".github/workflows/pr-checks.yml",
  production_job_names: ["widgets-release / widgets-release", "release / release"],
  production_apply_step_name: null,
  staging_apply_job_name: null,
  staging_e2e_job_name: null,
  updated_at: "2026-10-07T12:00:00Z",
  updated_by: { id: "alice@example.com", kind: "user" },
};

/** The form as the Settings page draws it, reading its record through `queryClient`, a plain
 *  client unless the case needs the app's own; `inMain` draws it in the shell's main content, as
 *  the app shell draws it. */
function renderForm({
  queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } }),
  inMain = false,
}: {
  readonly queryClient?: QueryClient;
  readonly inMain?: boolean;
} = {}) {
  render(
    <QueryClientProvider client={queryClient}>
      {inMain ? (
        <main>
          <DeliverySettingsForm />
        </main>
      ) : (
        <DeliverySettingsForm />
      )}
    </QueryClientProvider>
  );
  return queryClient;
}

function field(label: string): HTMLInputElement | HTMLTextAreaElement {
  return screen.getByLabelText(label) as HTMLInputElement | HTMLTextAreaElement;
}

/** Fills the empty field `label` with `value`, pasted in one edit as a person pastes a draft. */
async function fill(user: UserEvent, label: string, value: string): Promise<void> {
  await user.click(field(label));
  await user.paste(value);
}

/** Replaces what the field `label` holds with `value`. */
async function replace(user: UserEvent, label: string, value: string): Promise<void> {
  const input = field(label);
  await user.clear(input);
  await user.click(input);
  await user.paste(value);
}

test("the form starts from the stored record, one list entry per line", async () => {
  const getDeliverySettings = spyOn(api, "getDeliverySettings").mockResolvedValue(record);

  try {
    renderForm();

    expect(await screen.findByDisplayValue("acme/widgets")).toBeDefined();
    expect(field("Deploy workflow").value).toBe(".github/workflows/deploy.yml");
    expect(field("Production jobs").value).toBe(
      "widgets-release / widgets-release\nrelease / release"
    );
    expect(field("PR checks workflow").value).toBe(".github/workflows/pr-checks.yml");
    expect(field("Population authors").value).toBe("octocat\noctocat-agent[bot]");
    expect(field("Excluded repositories").value).toBe("acme/dojo\nacme/widgets-smoke");
  } finally {
    cleanup();
    getDeliverySettings.mockRestore();
  }
});

test("with no record the fields are empty and their placeholders say what each holds", async () => {
  const getDeliverySettings = spyOn(api, "getDeliverySettings").mockResolvedValue(null);

  try {
    renderForm();

    await screen.findByRole("button", { name: "Save delivery settings" });
    const placeholders = [
      ["Deploy repository", "owner/repo"],
      ["Deploy workflow", ".github/workflows/<deploy>.yml"],
      ["Production jobs", "One job name per line, as a deploy run lists it"],
      ["PR checks workflow", ".github/workflows/<checks>.yml"],
      ["Population authors", "One GitHub login per line"],
      ["Excluded repositories", "One owner/repo per line"],
    ];
    for (const [label, placeholder] of placeholders) {
      const input = field(label ?? "");
      expect(input.value).toBe("");
      expect(input.placeholder).toBe(placeholder ?? "");
    }
  } finally {
    cleanup();
    getDeliverySettings.mockRestore();
  }
});

test("Save sends the fields trimmed and each list split into its entries", async () => {
  const user = userEvent.setup();
  const getDeliverySettings = spyOn(api, "getDeliverySettings").mockResolvedValue(null);
  const putDeliverySettings = spyOn(api, "putDeliverySettings").mockResolvedValue(record);

  try {
    const queryClient = renderForm();
    const reads = seedDeliveryReads(queryClient);
    const invalidate = spyOn(queryClient, "invalidateQueries");
    await screen.findByRole("button", { name: "Save delivery settings" });

    await fill(user, "Deploy repository", "acme/widgets");
    await fill(user, "Deploy workflow", ".github/workflows/deploy.yml ");
    await fill(
      user,
      "Production jobs",
      " widgets-release / widgets-release \n\nrelease / release\n"
    );
    await fill(user, "PR checks workflow", ".github/workflows/pr-checks.yml");
    await fill(user, "Population authors", " octocat\n\noctocat-agent[bot],  hubot \n");
    await fill(user, "Excluded repositories", "acme/dojo\r\n acme/widgets-smoke\n");
    await user.click(screen.getByRole("button", { name: "Save delivery settings" }));

    await waitFor(() => expect(putDeliverySettings).toHaveBeenCalledTimes(1));
    expect(putDeliverySettings.mock.calls[0]?.[0]).toEqual({
      deploy_repo: "acme/widgets",
      deploy_workflow_path: ".github/workflows/deploy.yml",
      excluded_repos: ["acme/dojo", "acme/widgets-smoke"],
      population_authors: ["octocat", "octocat-agent[bot]", "hubot"],
      pr_checks_rollup_job_name: null,
      pr_checks_workflow_path: ".github/workflows/pr-checks.yml",
      production_job_names: ["widgets-release / widgets-release", "release / release"],
      production_apply_step_name: null,
      staging_apply_job_name: null,
      staging_e2e_job_name: null,
    });
    // A save refreshes the record and the delivery reads that are not live (an open run's
    // drill-down); a live view learns of the save from Dispatch, as a delta.
    await waitFor(() =>
      expect(invalidate.mock.calls.map(([filters]) => filters?.queryKey)).toEqual([
        ["delivery-settings"],
        ["delivery"],
      ])
    );
    expect(queryClient.getQueryState(reads.run)?.isInvalidated).toBe(true);
    expect(queryClient.getQueryState(reads.live)?.isInvalidated).toBe(false);
  } finally {
    cleanup();
    getDeliverySettings.mockRestore();
    putDeliverySettings.mockRestore();
  }
});

test("the three optional pipeline job names round-trip, and an emptied one saves null", async () => {
  const user = userEvent.setup();
  const stored: DeliverySettings = {
    ...record,
    pr_checks_rollup_job_name: "checks-complete",
    staging_apply_job_name: "staging-release / apply",
    staging_e2e_job_name: "staging-e2e / run",
  };
  const getDeliverySettings = spyOn(api, "getDeliverySettings").mockResolvedValue(stored);
  const putDeliverySettings = spyOn(api, "putDeliverySettings").mockResolvedValue(stored);

  try {
    renderForm();
    await screen.findByDisplayValue("acme/widgets");
    expect(field("Staging end-to-end job").value).toBe("staging-e2e / run");
    expect(field("Staging deploy job").value).toBe("staging-release / apply");
    expect(field("PR checks roll-up job").value).toBe("checks-complete");
    expect(field("Staging end-to-end job").placeholder).toBe(
      "The staging end-to-end job, as a deploy run lists it (optional)"
    );
    expect(field("Staging deploy job").placeholder).toBe("The staging deploy job (optional)");
    expect(field("PR checks roll-up job").placeholder).toBe(
      "The PR-checks roll-up job the failing-jobs table leaves out (optional)"
    );
    expect(field("Staging end-to-end job").required).toBe(false);

    await replace(user, "Staging deploy job", " staging-release / go ");
    await replace(user, "PR checks roll-up job", "   ");
    await user.click(screen.getByRole("button", { name: "Save delivery settings" }));

    await waitFor(() => expect(putDeliverySettings).toHaveBeenCalledTimes(1));
    expect(putDeliverySettings.mock.calls[0]?.[0]).toMatchObject({
      pr_checks_rollup_job_name: null,
      staging_apply_job_name: "staging-release / go",
      staging_e2e_job_name: "staging-e2e / run",
    });
  } finally {
    cleanup();
    getDeliverySettings.mockRestore();
    putDeliverySettings.mockRestore();
  }
});

test("the production apply step round-trips trimmed and an empty value saves null", async () => {
  const user = userEvent.setup();
  const stored: DeliverySettings = {
    ...record,
    production_apply_step_name: "Run ./.github/actions/apply",
  };
  const getDeliverySettings = spyOn(api, "getDeliverySettings").mockResolvedValue(stored);
  const putDeliverySettings = spyOn(api, "putDeliverySettings").mockResolvedValue(stored);

  try {
    renderForm();
    await screen.findByDisplayValue("acme/widgets");
    expect(field("Production apply step").value).toBe("Run ./.github/actions/apply");
    await replace(user, "Production apply step", " Run ./.github/actions/apply ");
    await user.click(screen.getByRole("button", { name: "Save delivery settings" }));
    await waitFor(() => expect(putDeliverySettings).toHaveBeenCalledTimes(1));
    expect(putDeliverySettings.mock.calls[0]?.[0]).toMatchObject({
      production_apply_step_name: "Run ./.github/actions/apply",
    });

    await replace(user, "Production apply step", "   ");
    await user.click(screen.getByRole("button", { name: "Save delivery settings" }));
    await waitFor(() => expect(putDeliverySettings).toHaveBeenCalledTimes(2));
    expect(putDeliverySettings.mock.calls[1]?.[0]).toMatchObject({
      production_apply_step_name: null,
    });
  } finally {
    cleanup();
    getDeliverySettings.mockRestore();
    putDeliverySettings.mockRestore();
  }
});

test("a required field holding only whitespace is empty, so Save is refused before any request", async () => {
  const user = userEvent.setup();
  const getDeliverySettings = spyOn(api, "getDeliverySettings").mockResolvedValue(null);
  const putDeliverySettings = spyOn(api, "putDeliverySettings").mockResolvedValue(record);
  const filled: [string, string][] = [
    ["Deploy repository", "acme/widgets"],
    ["Deploy workflow", ".github/workflows/deploy.yml"],
    ["Production jobs", "widgets-release"],
    ["PR checks workflow", ".github/workflows/pr-checks.yml"],
    ["Population authors", "octocat"],
  ];
  const blank: [string, string][] = [
    ["Deploy workflow", "   "],
    ["Production jobs", " \n\t \n"],
    ["PR checks workflow", " "],
    ["Population authors", " \n,\n  , "],
  ];

  try {
    renderForm();
    await screen.findByRole("button", { name: "Save delivery settings" });
    for (const [label, value] of filled) {
      await fill(user, label, value);
    }

    for (const [label, whitespace] of blank) {
      await replace(user, label, whitespace);
      expect(field(label).value).toBe("");
      // A save TanStack started would call the API only after its own await, and user-event's
      // click flushes that: it waits through its own act.
      await user.click(screen.getByRole("button", { name: "Save delivery settings" }));
      expect(putDeliverySettings).not.toHaveBeenCalled();
      await replace(user, label, filled.find(([name]) => name === label)?.[1] ?? "");
    }

    // With every field holding text again the same button saves, so the refusals above were the
    // whitespace's and nothing else's.
    await user.click(screen.getByRole("button", { name: "Save delivery settings" }));
    await waitFor(() => expect(putDeliverySettings).toHaveBeenCalledTimes(1));
  } finally {
    cleanup();
    getDeliverySettings.mockRestore();
    putDeliverySettings.mockRestore();
  }
});

test("a refused save shows the server's message as it wrote it, and the draft stays", async () => {
  const user = userEvent.setup();
  const message =
    "the GitHub App is not installed on acme/widgets: its token cannot read acme/widgets";
  const getDeliverySettings = spyOn(api, "getDeliverySettings").mockResolvedValue(record);
  const putDeliverySettings = spyOn(api, "putDeliverySettings").mockRejectedValue(
    new ApiError(409, { code: "DELIVERY_SETTINGS_ACCESS", error: message })
  );

  try {
    renderForm();
    await screen.findByDisplayValue("acme/widgets");

    await replace(user, "Deploy repository", "acme/elsewhere");
    await user.click(screen.getByRole("button", { name: "Save delivery settings" }));

    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toBe(message);
    expect(field("Deploy repository").value).toBe("acme/elsewhere");
  } finally {
    cleanup();
    getDeliverySettings.mockRestore();
    putDeliverySettings.mockRestore();
  }
});

// A caller that holds the record (the Delivery page's live view) replaces the form once the save's
// snapshot comes, so Save stays held after the save lands, and the submit guard, which covers only
// the save's own round trip, has let go by then. A second press of Save then sends nothing.
test("a Save pressed after the save lands, before the caller replaces the form, sends nothing", async () => {
  const user = userEvent.setup();
  const putDeliverySettings = spyOn(api, "putDeliverySettings").mockResolvedValue(record);

  try {
    const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(
      <QueryClientProvider client={queryClient}>
        <DeliverySettingsForm initial={record} />
      </QueryClientProvider>
    );
    const save = screen.getByRole("button", { name: "Save delivery settings" });
    await user.click(save);
    await waitFor(() => expect(putDeliverySettings).toHaveBeenCalledTimes(1));
    // The save has settled, and with it the guard's hold, but the caller has not drawn its
    // answer yet: the form is still the one the save was made from.
    await waitFor(() => expect(queryClient.isMutating()).toBe(0));
    expect(save.getAttribute("aria-disabled")).toBe("true");

    await user.click(save);
    expect(putDeliverySettings).toHaveBeenCalledTimes(1);
    expect(document.activeElement).toBe(save);
  } finally {
    cleanup();
    putDeliverySettings.mockRestore();
  }
});

// A save the reader stopped may have been made: the form says so, and that its button sends what
// the fields hold then, rather than a failure it cannot know of.
test("a save the reader stopped says it may have been made, not that it failed", async () => {
  jest.useFakeTimers();
  const user = userOnFakeClock();
  const getDeliverySettings = spyOn(api, "getDeliverySettings").mockResolvedValue(record);
  const putDeliverySettings = spyOn(api, "putDeliverySettings").mockRejectedValue(
    new NetworkError("/api/v1/settings/delivery", new TypeError("Failed to fetch"))
  );
  const queryClient = renderForm();
  try {
    await screen.findByDisplayValue("acme/widgets");
    await user.click(screen.getByRole("button", { name: "Save delivery settings" }));
    await stopTheLateWrite(queryClient);
    expect(screen.getByRole("alert").textContent).toBe(
      stoppedWriteNote("the delivery settings", "saved", {
        anew: "Save delivery settings sends what the form holds now.",
      })
    );
  } finally {
    realClock();
    cleanup();
    queryClient.clear();
    getDeliverySettings.mockRestore();
    putDeliverySettings.mockRestore();
  }
});

// A save that never answers - each try fails at the network, and the next waits out its backoff -
// holds Save until its deadline (`SEND_DEADLINE_MS`), and no longer: past it the save is late and
// still tried, and Save is let go, with the focus on it. A press then is a save of its own, which
// sends what the fields hold: the guard that drops a second press of the same task lets go too.
test("Save, held until its save's deadline, is let go there, and a press then saves again", async () => {
  const getDeliverySettings = spyOn(api, "getDeliverySettings").mockResolvedValue(record);
  const putDeliverySettings = spyOn(api, "putDeliverySettings").mockRejectedValue(
    new NetworkError("/api/v1/settings/delivery", new TypeError("Failed to fetch"))
  );
  const queryClient = renderForm();
  try {
    await screen.findByDisplayValue("acme/widgets");
    jest.useFakeTimers();
    const user = userOnFakeClock();
    await heldUntilLate(
      user,
      screen.getByRole<HTMLButtonElement>("button", { name: "Save delivery settings" }),
      {
        label: "Delivery settings saved",
        name: "Save delivery settings",
        queryClient,
        sent: () => pressesOf([putDeliverySettings]),
      }
    );
    await user.keyboard("{Enter}");
    await tick(1_000);
    expect(pressesOf([putDeliverySettings]), "a press past the deadline saved nothing").toBe(2);
  } finally {
    realClock();
    cleanup();
    queryClient.clear();
    getDeliverySettings.mockRestore();
    putDeliverySettings.mockRestore();
  }
});

// Save is held while its save is out and through the record's read: by `aria-disabled`, not
// `disabled`, which Chromium answers by taking the focus off the button
// (`emulateChromiumFocusFixup`). So Save keeps the focus through the read. The form stays the
// same element through the read and fills its fields again from the stored record, and the focus
// stays on the same Save once it is let go: never on the form's first field, out of view at the
// form's top, where the reader's next key would be typed.
test("Save keeps the focus once the read draws the stored record, and no field takes it", async () => {
  const user = userEvent.setup();
  const stored: DeliverySettings = {
    ...record,
    deploy_repo: "acme/gadgets",
    excluded_repos: ["acme/dojo", "acme/gadgets-smoke"],
    updated_at: "2026-10-08T09:00:00Z",
  };
  const getDeliverySettings = spyOn(api, "getDeliverySettings").mockResolvedValue(record);
  const putDeliverySettings = spyOn(api, "putDeliverySettings").mockResolvedValue(stored);
  const restoreFocus = emulateChromiumFocusFixup();

  try {
    const queryClient = renderForm({ inMain: true });
    await screen.findByDisplayValue("acme/widgets");
    await replace(user, "Deploy repository", "acme/gadgets");
    // The list as typed, one line; the server stores it as its entries.
    await replace(user, "Excluded repositories", "acme/dojo, acme/gadgets-smoke");
    // The read after the save is held, and answers the record as the server stored it.
    const read = Promise.withResolvers<DeliverySettings>();
    getDeliverySettings.mockReturnValue(read.promise);
    await user.tab();
    const save = screen.getByRole("button", { name: "Save delivery settings" });
    expect(save).toHaveFocus();
    await user.keyboard("{Enter}");

    await waitFor(() => expect(getDeliverySettings).toHaveBeenCalledTimes(2));
    expect(putDeliverySettings).toHaveBeenCalledTimes(1);
    // Save is held through the read, and keeps the focus.
    await afterFrameAndTasks();
    expect(save).toHaveAttribute("aria-disabled", "true");
    expect(save).toHaveFocus();

    await act(async () => read.resolve(stored));
    await waitFor(() => expect(queryClient.isMutating()).toBe(0));
    await waitFor(() => expect(save).not.toHaveAttribute("aria-disabled"));
    await afterFrameAndTasks();
    expect(document.activeElement).toBe(save);
    expect(screen.getByRole("button", { name: "Save delivery settings" })).toBe(save);
    expect(field("Deploy repository")).not.toHaveFocus();
    expect(field("Deploy repository").value).toBe("acme/gadgets");
    expect(field("Excluded repositories").value).toBe("acme/dojo\nacme/gadgets-smoke");
  } finally {
    restoreFocus();
    cleanup();
    getDeliverySettings.mockRestore();
    putDeliverySettings.mockRestore();
  }
});

// The save has its answer, and Save stays held while the Delivery page, which holds the record,
// is still to draw it (`useHoldUntilDrawn`). Held, Save keeps the reader's focus: it is
// `aria-disabled`, not `disabled`, which Chromium answers by taking the focus off the button
// (`emulateChromiumFocusFixup`). A press of it then - Enter, a click, or the form's submit - sends
// nothing.
test("a held Save keeps the keyboard focus, and a press or a submit while it is held sends nothing", async () => {
  const user = userEvent.setup();
  const putDeliverySettings = spyOn(api, "putDeliverySettings").mockResolvedValue({
    ...record,
    updated_at: "2026-10-08T09:00:00Z",
  });
  const restoreFocus = emulateChromiumFocusFixup();
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  try {
    render(
      <QueryClientProvider client={queryClient}>
        <main>
          <DeliverySettingsForm initial={record} />
        </main>
      </QueryClientProvider>
    );
    const save = screen.getByRole("button", { name: "Save delivery settings" });
    act(() => save.focus());
    await user.keyboard("{Enter}");
    await waitFor(() => expect(putDeliverySettings).toHaveBeenCalledTimes(1));
    await waitFor(() => expect(queryClient.isMutating()).toBe(0));
    await afterFrameAndTasks();
    expect(save).toHaveAttribute("aria-disabled", "true");
    expect(save).not.toBeDisabled();
    expect(save).toHaveFocus();

    await user.keyboard("{Enter}");
    await user.click(save);
    act(() => save.closest("form")?.requestSubmit());
    await afterFrameAndTasks();
    expect(putDeliverySettings).toHaveBeenCalledTimes(1);
    expect(save).toHaveAttribute("aria-disabled", "true");
    expect(save).toHaveFocus();
  } finally {
    restoreFocus();
    cleanup();
    queryClient.clear();
    putDeliverySettings.mockRestore();
  }
});

// Enter in a field submits the form, and the field keeps the focus through the hold: each field is
// held by `aria-disabled` and read-only, never `disabled`, which Chromium answers by taking the
// focus off it (`emulateChromiumFocusFixup`, on in `pressHeld`). A second Enter there sends
// nothing.
test("Enter in the Deploy workflow box submits, and the box keeps the focus through the hold", async () => {
  const user = userEvent.setup();
  const putDeliverySettings = spyOn(api, "putDeliverySettings").mockResolvedValue({
    ...record,
    updated_at: "2026-10-08T09:00:00Z",
  });
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  try {
    render(
      <QueryClientProvider client={queryClient}>
        <DeliverySettingsForm initial={record} />
      </QueryClientProvider>
    );
    const box = field("Deploy workflow");
    await pressHeld(user, box, {
      name: "Deploy workflow",
      sent: () => putDeliverySettings.mock.calls.length,
    });
    expect(box.readOnly).toBe(true);
  } finally {
    cleanup();
    queryClient.clear();
    putDeliverySettings.mockRestore();
  }
});

// The save has its answer, and Save stays held until the Delivery page, which holds the record,
// draws what the save changed in the form's place (`useHoldUntilDrawn`): several frames and tasks
// after the answer, Save is still held, and keeps the focus.
test("Save stays held, with the focus, after its save is answered and before its caller draws what it changed", async () => {
  const user = userEvent.setup();
  const putDeliverySettings = spyOn(api, "putDeliverySettings").mockResolvedValue({
    ...record,
    updated_at: "2026-10-08T09:00:00Z",
  });
  const restoreFocus = emulateChromiumFocusFixup();
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  try {
    render(
      <QueryClientProvider client={queryClient}>
        <main>
          <DeliverySettingsForm initial={record} />
        </main>
      </QueryClientProvider>
    );
    const save = screen.getByRole("button", { name: "Save delivery settings" });
    act(() => save.focus());
    await user.keyboard("{Enter}");
    await waitFor(() => expect(putDeliverySettings).toHaveBeenCalledTimes(1));
    await waitFor(() => expect(queryClient.isMutating()).toBe(0));
    for (const _ of [0, 1, 2]) await afterFrameAndTasks();
    expect(save).toHaveAttribute("aria-disabled", "true");
    expect(save).toHaveFocus();
  } finally {
    restoreFocus();
    cleanup();
    queryClient.clear();
    putDeliverySettings.mockRestore();
  }
});

// `DispatchQueryClient` answers the invalidation of the idle record after a save with the
// record's own fetch, which TanStack pauses while the browser is offline, and no request deadline
// runs while a fetch is paused. So Save's hold through that read has a timer of its own, the
// request deadline (`REQUEST_DEADLINE_MS`): Save whose save landed offline is let go then, and the
// form says the record is not read yet and why. Once the network returns, the record is read and
// the note goes.
test("offline, Save whose save landed is let go at the request deadline, and the form says why", async () => {
  const online = onlineManager.isOnline();
  const getDeliverySettings = spyOn(api, "getDeliverySettings").mockResolvedValue(record);
  // The save's answer, held until the case moves the clock, so what it sets off runs in `act`.
  const saved = Promise.withResolvers<DeliverySettings>();
  const putDeliverySettings = spyOn(api, "putDeliverySettings").mockImplementation(
    () => saved.promise
  );
  const queryClient = renderForm({
    queryClient: new DispatchQueryClient({ defaultOptions: { queries: { retry: false } } }),
  });
  await screen.findByDisplayValue("acme/widgets");
  const save = screen.getByRole("button", { name: "Save delivery settings" });
  jest.useFakeTimers();
  try {
    const user = userOnFakeClock();
    onlineManager.setOnline(false);
    await user.click(save);
    await tick(0);
    expect(putDeliverySettings).toHaveBeenCalledTimes(1);
    saved.resolve({ ...record, updated_at: "2026-10-08T09:00:00Z" });
    await tick(1_000);
    expect(queryClient.getQueryState(deliverySettingsQuery().queryKey)?.fetchStatus).toBe("paused");
    expect(save).toHaveAttribute("aria-disabled", "true");

    // The save landed within the second before, which is when the hold's timer started.
    await tick(REQUEST_DEADLINE_MS - 2_000);
    expect(save).toHaveAttribute("aria-disabled", "true");

    await tick(2_000);
    expect(save).not.toHaveAttribute("aria-disabled");
    expect(getDeliverySettings).toHaveBeenCalledTimes(1);
    expect(screen.getByRole("alert").textContent).toBe(
      "Couldn't load the delivery settings. Dispatch could not be reached."
    );

    act(() => onlineManager.setOnline(true));
    await tick(0);
    expect(getDeliverySettings).toHaveBeenCalledTimes(2);
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
  } finally {
    // The form and its reads go before the network returns, so no read paused offline goes on.
    cleanup();
    queryClient.clear();
    onlineManager.setOnline(online);
    realClock();
    getDeliverySettings.mockRestore();
    putDeliverySettings.mockRestore();
  }
});

// The line under the form for a record read the bound ended is that save's own: the next press of
// Save takes it away as its save lands, though the earlier read is still out, and that save's
// read answering says nothing more. Dispatch holds the first save's read until the row answers
// it, so the next save's read, which waits for that one (`DispatchQueryClient`), answers after it.
test("the line a save's read the bound ended draws goes as the next save lands, whose read answers", async () => {
  const getDeliverySettings = spyOn(api, "getDeliverySettings").mockResolvedValue(record);
  // Each save's answer, held until the row moves the clock, so what it sets off runs in `act`.
  const { answers: saves, next } = heldAnswers<DeliverySettings>();
  const putDeliverySettings = spyOn(api, "putDeliverySettings").mockImplementation(next);
  const queryClient = renderForm({
    queryClient: new DispatchQueryClient({ defaultOptions: { queries: { retry: false } } }),
  });
  await screen.findByDisplayValue("acme/widgets");
  const save = screen.getByRole("button", { name: "Save delivery settings" });
  const stored = { ...record, updated_at: "2026-10-08T09:00:00Z" };
  const reread = Promise.withResolvers<DeliverySettings>();
  getDeliverySettings.mockResolvedValue(stored);
  getDeliverySettings.mockReturnValueOnce(reread.promise);
  jest.useFakeTimers();
  try {
    const user = userOnFakeClock();
    await user.click(save);
    await tick(0);
    saves[0]?.(stored);
    await tick(0);
    expect(save).toHaveAttribute("aria-disabled", "true");
    await tick(REQUEST_DEADLINE_MS);
    expect(save).not.toHaveAttribute("aria-disabled");
    expect(screen.getByRole("alert").textContent).toBe(
      "Couldn't load the delivery settings. Dispatch did not answer within 30 seconds."
    );

    await user.click(save);
    await tick(0);
    expect(putDeliverySettings).toHaveBeenCalledTimes(2);
    saves[1]?.(stored);
    await tick(0);
    expect(save).toHaveAttribute("aria-disabled", "true");
    expect(
      screen.queryByRole("alert"),
      "the earlier save's read kept its line past the next save"
    ).toBeNull();

    await act(async () => reread.resolve(stored));
    await tick(0);
    expect(save).not.toHaveAttribute("aria-disabled");
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
  } finally {
    cleanup();
    queryClient.clear();
    realClock();
    getDeliverySettings.mockRestore();
    putDeliverySettings.mockRestore();
  }
});

// The request deadline ends a save's hold and draws the line under the form in one change of the
// hold's store, and the save, which `onSuccess` keeps pending through that hold, stops pending as
// it ends. Save lets go in that same task, so a press of it once the form says why saves again. A
// browser tells TanStack's observers of the save's end a timer later (`holdNotifications`), so
// the row holds those notifications back. The record's read starts as the save lands, before the
// clock moves, so one jump of the deadline lands on the bound's own timer, and only microtasks run
// after it.
test("Save is let go in the task the form says why at the request deadline, and a press there saves again", async () => {
  const getDeliverySettings = spyOn(api, "getDeliverySettings").mockResolvedValue(record);
  // Each save's answer, held until the row gives it, so what it sets off runs in `act`.
  const { answers: saves, next } = heldAnswers<DeliverySettings>();
  const putDeliverySettings = spyOn(api, "putDeliverySettings").mockImplementation(next);
  const queryClient = renderForm({
    queryClient: new DispatchQueryClient({ defaultOptions: { queries: { retry: false } } }),
  });
  const stored = { ...record, updated_at: "2026-10-08T09:00:00Z" };
  const reread = Promise.withResolvers<DeliverySettings>();
  try {
    await screen.findByDisplayValue("acme/widgets");
    const save = screen.getByRole("button", { name: "Save delivery settings" });
    getDeliverySettings.mockReturnValueOnce(reread.promise);
    jest.useFakeTimers();
    const user = userOnFakeClock();
    await user.click(save);
    await tick(0);
    await act(async () => saves[0]?.(stored));
    expect(getDeliverySettings).toHaveBeenCalledTimes(2);
    expect(save).toHaveAttribute("aria-disabled", "true");

    const notifications = holdNotifications();
    try {
      await act(async () => {
        jest.advanceTimersByTime(REQUEST_DEADLINE_MS);
        await settle();
      });
      expect(screen.getByRole("alert").textContent).toBe(
        "Couldn't load the delivery settings. Dispatch did not answer within 30 seconds."
      );
      expect(save, "the form said why while Save was held").not.toHaveAttribute("aria-disabled");
      await user.click(save);
    } finally {
      notifications.tell();
    }
    await tick(0);
    expect(putDeliverySettings).toHaveBeenCalledTimes(2);
  } finally {
    reread.resolve(stored);
    cleanup();
    queryClient.clear();
    realClock();
    getDeliverySettings.mockRestore();
    putDeliverySettings.mockRestore();
  }
});

// Another tab saved, and this tab's read brings the record with its new write time: the fields
// show the stored record, and a refusal of this tab's draft goes with that draft.
test("a save of another tab's that the read brings fills the fields from the stored record, and the refusal shown goes", async () => {
  const user = userEvent.setup();
  const message =
    "the GitHub App is not installed on acme/elsewhere: its token cannot read acme/elsewhere";
  const elsewhere: DeliverySettings = {
    ...record,
    deploy_repo: "acme/gadgets",
    population_authors: ["hubot"],
    updated_at: "2026-10-08T09:00:00Z",
    updated_by: { id: "bob@example.com", kind: "user" },
  };
  const getDeliverySettings = spyOn(api, "getDeliverySettings").mockResolvedValue(record);
  const putDeliverySettings = spyOn(api, "putDeliverySettings").mockRejectedValue(
    new ApiError(409, { code: "DELIVERY_SETTINGS_ACCESS", error: message })
  );

  try {
    const queryClient = renderForm();
    await screen.findByDisplayValue("acme/widgets");
    await replace(user, "Deploy repository", "acme/elsewhere");
    await user.click(screen.getByRole("button", { name: "Save delivery settings" }));
    expect((await screen.findByRole("alert")).textContent).toBe(message);

    getDeliverySettings.mockResolvedValue(elsewhere);
    await act(async () => {
      await queryClient.invalidateQueries({ queryKey: deliverySettingsQuery().queryKey });
    });

    expect(field("Deploy repository").value).toBe("acme/gadgets");
    expect(field("Population authors").value).toBe("hubot");
    expect(field("Production jobs").value).toBe(
      "widgets-release / widgets-release\nrelease / release"
    );
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
  } finally {
    cleanup();
    getDeliverySettings.mockRestore();
    putDeliverySettings.mockRestore();
  }
});
