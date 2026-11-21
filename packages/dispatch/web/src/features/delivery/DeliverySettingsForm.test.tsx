import { expect, jest, spyOn, test } from "bun:test";
import { onlineManager, QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, render, screen, waitFor } from "@testing-library/react";
import type { UserEvent } from "@testing-library/user-event";
import userEvent from "@testing-library/user-event";

import { realClock, tick } from "../../__tests__/fake-clock";
import { afterRescue, RescuedMain } from "../../__tests__/focus-rescue";
import { seedDeliveryReads } from "../../__tests__/live-socket-harness";
import { stopTheLateWrite } from "../../__tests__/stop-late-write";
import { userOnFakeClock } from "../../__tests__/user";
import { ApiError, api, NetworkError, REQUEST_DEADLINE_MS } from "../../api/client";
import { deliverySettingsQuery } from "../../api/queries";
import { DispatchQueryClient } from "../../api/query-client";
import type { DeliverySettings } from "../../api/types";
import { emulateChromiumFocusFixup } from "../issue-page/chromium-focus-fixup";
import { stoppedWriteNote } from "../writes/keyed-write";
import { DeliverySettingsForm } from "./DeliverySettingsForm";

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
 *  client unless the case needs the app's own; `rescued` draws it in the shell's main content with
 *  the page's focus rescue mounted on it (`RescuedMain`), as the app shell mounts it. */
function renderForm({
  queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } }),
  rescued = false,
}: {
  readonly queryClient?: QueryClient;
  readonly rescued?: boolean;
} = {}) {
  render(
    <QueryClientProvider client={queryClient}>
      {rescued ? (
        <RescuedMain>
          <DeliverySettingsForm />
        </RescuedMain>
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

// Save is disabled while its save is out and through the record's read, and Chromium takes the
// focus off a button that turns disabled (`emulateChromiumFocusFixup`). The form stays the same
// element through the read and fills its fields again from the stored record, so the page's
// rescue gives the focus back to the same Save once it is enabled again: never to the form's first
// field, out of view at the form's top, where the reader's next key would be typed.
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
    const queryClient = renderForm({ rescued: true });
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
    // Save is held through the read, and the focus has left it for the page.
    await afterRescue();
    expect((save as HTMLButtonElement).disabled).toBe(true);
    expect(document.activeElement).toBe(document.body);

    await act(async () => read.resolve(stored));
    await waitFor(() => expect(queryClient.isMutating()).toBe(0));
    await waitFor(() => expect(save).toHaveFocus());
    await afterRescue();
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

// `DispatchQueryClient` answers the invalidation of the idle record after a save with the
// record's own fetch, which TanStack pauses while the browser is offline, and no request deadline
// runs while a fetch is paused. So Save's hold through that read has a timer of its own, the
// request deadline (`REQUEST_DEADLINE_MS`): Save whose save landed offline is enabled again then,
// and the form says the record is not read yet and why. Once the network returns, the record is
// read and the note goes.
test("offline, Save whose save landed is enabled again at the request deadline, and the form says why", async () => {
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
    expect(save).toBeDisabled();

    // The save landed within the second before, which is when the hold's timer started.
    await tick(REQUEST_DEADLINE_MS - 2_000);
    expect(save).toBeDisabled();

    await tick(2_000);
    expect(save).not.toBeDisabled();
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
