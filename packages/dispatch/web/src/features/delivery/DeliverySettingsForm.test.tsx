import { beforeAll, expect, jest, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, render, screen, waitFor } from "@testing-library/react";
import type { UserEvent } from "@testing-library/user-event";
import userEvent from "@testing-library/user-event";
import { type HeldNotifications, holdNotifications, recordCommits } from "../../__tests__/commits";

import { realClock, tick } from "../../__tests__/fake-clock";
import { afterFrameAndTasks } from "../../__tests__/frames";
import { pressHeld } from "../../__tests__/held-press";
import { seedDeliveryReads } from "../../__tests__/live-socket-harness";
import { stopTheLateWrite } from "../../__tests__/stop-late-write";
import { userOnFakeClock } from "../../__tests__/user";
import { ApiError, api, NetworkError } from "../../api/client";
import { deliverySettingsQuery } from "../../api/queries";
import type { DeliverySettings } from "../../api/types";
import { emulateChromiumFocusFixup } from "../issue-page/chromium-focus-fixup";
import { warmMarkdownRenderer } from "../refs/markdown-engine";
import { SEND_DEADLINE_MS, stoppedWriteNote } from "../writes/keyed-write";
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

// The Layout rule (`web/src/AGENTS.md`), point 6: another writer's save changes the record under
// the form. The field the reader changed keeps what they typed, and the fields they did not touch
// take the new record.
test("another writer's save keeps the fields the reader changed and refreshes the rest", async () => {
  const user = userEvent.setup();
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const form = (initial: DeliverySettings) => (
    <QueryClientProvider client={queryClient}>
      <DeliverySettingsForm initial={initial} />
    </QueryClientProvider>
  );
  const view = render(form(record));
  try {
    await replace(user, "Deploy workflow", ".github/workflows/ship.yml");
    view.rerender(
      form({ ...record, deploy_repo: "acme/gadgets", updated_at: "2026-10-08T12:00:00Z" })
    );
    expect(field("Deploy workflow").value).toBe(".github/workflows/ship.yml");
    expect(field("Deploy repository").value).toBe("acme/gadgets");
  } finally {
    cleanup();
  }
});

// A list typed with a line break after its last entry is saved without it, so the record this
// tab's save lands holds the list in another form than the reader typed. The field then follows
// the record, and a later writer's change to that list shows, rather than the reader's old text
// staying over it and going back to the server with the next save.
test("a list saved with a trailing line break follows the record, and shows a later writer's change", async () => {
  const user = userEvent.setup();
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const form = (initial: DeliverySettings) => (
    <QueryClientProvider client={queryClient}>
      <DeliverySettingsForm initial={initial} />
    </QueryClientProvider>
  );
  const saved = {
    ...record,
    population_authors: ["alice", "bob"],
    updated_at: "2026-10-08T12:00:00Z",
  };
  const putDeliverySettings = spyOn(api, "putDeliverySettings").mockResolvedValue(saved);
  const view = render(form(record));
  try {
    await replace(user, "Population authors", "alice\nbob\n");
    // The reader saves, and their save lands: the record holds the list as Save sent it.
    await user.click(screen.getByRole("button", { name: "Save delivery settings" }));
    await waitFor(() => expect(putDeliverySettings).toHaveBeenCalledTimes(1));
    await waitFor(() => expect(queryClient.isMutating()).toBe(0));
    view.rerender(form(saved));
    expect(field("Population authors").value).toBe("alice\nbob");
    // Another writer adds carol.
    view.rerender(
      form({
        ...saved,
        population_authors: ["alice", "bob", "carol"],
        updated_at: "2026-10-09T12:00:00Z",
      })
    );
    expect(field("Population authors").value).toBe("alice\nbob\ncarol");
  } finally {
    cleanup();
    putDeliverySettings.mockRestore();
  }
});

// A reader adding a third author types a line break after the second, and the field then holds
// the list the record holds in another form, with no save of this tab out. Another writer's save
// of another field landing keeps the line break as the reader typed it, and the caret after it,
// so the name they type next is an entry of its own and Save sends three entries, not a second
// entry run into the third.
test("another writer's save keeps the line break a reader typed after a list's last entry", async () => {
  const user = userEvent.setup();
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const form = (initial: DeliverySettings) => (
    <QueryClientProvider client={queryClient}>
      <DeliverySettingsForm initial={initial} />
    </QueryClientProvider>
  );
  const stored = { ...record, population_authors: ["alice", "bob"] };
  const putDeliverySettings = spyOn(api, "putDeliverySettings").mockResolvedValue(stored);
  const view = render(form(stored));
  try {
    await user.type(field("Population authors"), "{Enter}");
    expect(field("Population authors").value).toBe("alice\nbob\n");
    view.rerender(
      form({ ...stored, deploy_repo: "acme/gadgets", updated_at: "2026-10-08T12:00:00Z" })
    );
    expect(field("Population authors").value).toBe("alice\nbob\n");
    await user.keyboard("carol");
    expect(field("Population authors").value).toBe("alice\nbob\ncarol");
    await user.click(screen.getByRole("button", { name: "Save delivery settings" }));
    await waitFor(() => expect(putDeliverySettings).toHaveBeenCalledTimes(1));
    expect(putDeliverySettings.mock.calls[0]?.[0]).toMatchObject({
      deploy_repo: "acme/gadgets",
      population_authors: ["alice", "bob", "carol"],
    });
  } finally {
    cleanup();
    putDeliverySettings.mockRestore();
  }
});

// The fields this tab's save sent follow its answer from its landing on, and the reader types once
// it has landed - the fields are disabled while it is out. A line break typed then after the
// list's last entry stays as typed when the read the landing started answers the same record,
// with the caret where the reader left it, so the next name is an entry of its own.
test("text a reader types after their save landed stays as typed when the read that landing started answers", async () => {
  const user = userEvent.setup();
  const saved = {
    ...record,
    population_authors: ["alice", "bob"],
    updated_at: "2026-10-08T12:00:00Z",
  };
  const reread = Promise.withResolvers<DeliverySettings>();
  const getDeliverySettings = spyOn(api, "getDeliverySettings")
    .mockResolvedValueOnce(record)
    .mockReturnValue(reread.promise);
  const putDeliverySettings = spyOn(api, "putDeliverySettings").mockResolvedValue(saved);
  try {
    const queryClient = renderForm();
    await screen.findByDisplayValue("acme/widgets");
    await replace(user, "Population authors", "alice, bob\n");
    await user.click(screen.getByRole("button", { name: "Save delivery settings" }));
    await waitFor(() => expect(queryClient.isMutating()).toBe(0));
    expect(field("Population authors").value).toBe("alice\nbob");
    await user.type(field("Population authors"), "\n");
    reread.resolve(saved);
    await waitFor(() =>
      expect(queryClient.getQueryState(["delivery-settings"])?.fetchStatus).toBe("idle")
    );
    expect(field("Population authors").value).toBe("alice\nbob\n");
    await user.keyboard("carol");
    expect(field("Population authors").value).toBe("alice\nbob\ncarol");
  } finally {
    reread.resolve(saved);
    cleanup();
    getDeliverySettings.mockRestore();
    putDeliverySettings.mockRestore();
  }
});

// This tab's save lands, and the read its landing starts answers another writer's newer record,
// which holds a deploy workflow of their own and never the one this tab sent. Dispatch's answer to
// the save is the record the form follows from the landing on, so the field shows the other
// writer's value once their record arrives, and the next Save sends it, rather than this tab's old
// text, which would undo their save.
test("a landed save whose own record never arrives follows another writer's newer record, and the next Save sends it", async () => {
  const user = userEvent.setup();
  const saved = {
    ...record,
    deploy_workflow_path: ".github/workflows/ship.yml",
    updated_at: "2026-10-08T12:00:00Z",
  };
  const theirs = {
    ...record,
    deploy_workflow_path: ".github/workflows/theirs.yml",
    updated_at: "2026-10-08T12:00:05Z",
  };
  const getDeliverySettings = spyOn(api, "getDeliverySettings")
    .mockResolvedValueOnce(record)
    .mockResolvedValue(theirs);
  const putDeliverySettings = spyOn(api, "putDeliverySettings").mockResolvedValue(saved);

  try {
    renderForm();
    await screen.findByDisplayValue("acme/widgets");
    await replace(user, "Deploy workflow", ".github/workflows/ship.yml");
    await user.click(screen.getByRole("button", { name: "Save delivery settings" }));
    await waitFor(() =>
      expect(field("Deploy workflow").value).toBe(".github/workflows/theirs.yml")
    );
    await user.click(screen.getByRole("button", { name: "Save delivery settings" }));
    await waitFor(() => expect(putDeliverySettings).toHaveBeenCalledTimes(2));
    expect(putDeliverySettings.mock.calls[1]?.[0]).toMatchObject({
      deploy_workflow_path: ".github/workflows/theirs.yml",
    });
  } finally {
    cleanup();
    getDeliverySettings.mockRestore();
    putDeliverySettings.mockRestore();
  }
});

// This tab's save lands, and the read its landing starts has not answered: the record read before
// the save still stands in the cache. Dispatch's answer is the record from the landing on. The
// row holds the display - the list as the save stored it, one entry per line, where a form that
// kept the fields as typed until that read answered still shows `alice, bob\n` - and the payload,
// against a form that retires the fields at the landing without writing the answer, whose second
// Save sends the record from before the save (`octocat`) and so undoes the reader's own save.
test("a second Save before the read a landed save started answers sends the first save's value", async () => {
  const user = userEvent.setup();
  const saved = {
    ...record,
    population_authors: ["alice", "bob"],
    updated_at: "2026-10-08T12:00:00Z",
  };
  const reread = Promise.withResolvers<DeliverySettings>();
  const getDeliverySettings = spyOn(api, "getDeliverySettings")
    .mockResolvedValueOnce(record)
    .mockReturnValue(reread.promise);
  const putDeliverySettings = spyOn(api, "putDeliverySettings").mockResolvedValue(saved);

  try {
    const queryClient = renderForm();
    await screen.findByDisplayValue("acme/widgets");
    await replace(user, "Population authors", "alice, bob\n");
    await user.click(screen.getByRole("button", { name: "Save delivery settings" }));
    await waitFor(() => expect(queryClient.isMutating()).toBe(0));
    await user.click(screen.getByRole("button", { name: "Save delivery settings" }));
    await waitFor(() => expect(putDeliverySettings).toHaveBeenCalledTimes(2));
    expect(putDeliverySettings.mock.calls[1]?.[0]).toMatchObject({
      population_authors: ["alice", "bob"],
    });
    expect(field("Population authors").value).toBe("alice\nbob");
  } finally {
    reread.resolve(saved);
    cleanup();
    getDeliverySettings.mockRestore();
    putDeliverySettings.mockRestore();
  }
});

// The save's answer reaches the cache in its own callback, and the form's host only when TanStack
// Query tells the cache's observers, in a browser a timer later, which the row holds back. The
// fields the save sent are retired in that callback and follow the answer in the very commit that
// retires them, so no commit draws the record from before the save in their place (INFRA-3904).
test("every commit from a save's landing draws the saved value, while the form's host still passes the record before it", async () => {
  const user = userEvent.setup();
  // The list as each commit drew it; none while the form still reads the record.
  const commits = recordCommits<null>(
    () => (screen.queryByLabelText("Population authors") as HTMLTextAreaElement | null)?.value ?? ""
  );
  const saved = {
    ...record,
    population_authors: ["alice", "bob"],
    updated_at: "2026-10-08T12:00:00Z",
  };
  const answer = Promise.withResolvers<DeliverySettings>();
  const getDeliverySettings = spyOn(api, "getDeliverySettings")
    .mockResolvedValueOnce(record)
    .mockReturnValue(new Promise<DeliverySettings>(() => {}));
  const putDeliverySettings = spyOn(api, "putDeliverySettings").mockReturnValue(answer.promise);
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={queryClient}>
      <commits.Recorded host={null}>
        <DeliverySettingsForm />
      </commits.Recorded>
    </QueryClientProvider>
  );
  let notifications: HeldNotifications | undefined;
  try {
    await screen.findByDisplayValue("acme/widgets");
    await replace(user, "Population authors", "alice, bob\n");
    await user.click(screen.getByRole("button", { name: "Save delivery settings" }));
    await waitFor(() => expect(putDeliverySettings).toHaveBeenCalledTimes(1));
    const fromLanding = commits.markAt(document, "landing");
    notifications = holdNotifications();
    document.dispatchEvent(new Event("landing"));
    await act(async () => {
      answer.resolve(saved);
      await answer.promise;
    });
    await waitFor(() => expect(field("Population authors").value).toBe("alice\nbob"));
    // The cache holds the answer, and its host has not been told.
    expect(notifications.held()).toBeGreaterThan(0);
    const drawn = fromLanding()
      .map((commit) => commit.drawn)
      .filter((value) => value !== "alice, bob\n");
    expect(drawn.length).toBeGreaterThan(0);
    expect(drawn).toEqual(drawn.map(() => "alice\nbob"));
  } finally {
    notifications?.tell();
    cleanup();
    getDeliverySettings.mockRestore();
    putDeliverySettings.mockRestore();
  }
});

// The save lands with its answer while the read its landing started is still out: the fields it
// sent show that answer from the landing on - neither as typed nor as the record from before the
// save - and still show it once that read answers the same record.
test("a landed save's fields show its answer before the read its landing started answers, and after", async () => {
  const user = userEvent.setup();
  const saved = {
    ...record,
    population_authors: ["alice", "bob"],
    updated_at: "2026-10-08T12:00:00Z",
  };
  const reread = Promise.withResolvers<DeliverySettings>();
  const getDeliverySettings = spyOn(api, "getDeliverySettings")
    .mockResolvedValueOnce(record)
    .mockReturnValue(reread.promise);
  const putDeliverySettings = spyOn(api, "putDeliverySettings").mockResolvedValue(saved);

  try {
    const queryClient = renderForm();
    await screen.findByDisplayValue("acme/widgets");
    await replace(user, "Population authors", "alice, bob\n");
    await user.click(screen.getByRole("button", { name: "Save delivery settings" }));
    await waitFor(() => expect(putDeliverySettings).toHaveBeenCalledTimes(1));
    await waitFor(() => expect(queryClient.isMutating()).toBe(0));
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
    expect(queryClient.getQueryState(["delivery-settings"])?.fetchStatus).toBe("fetching");
    expect(field("Population authors").value).toBe("alice\nbob");
    reread.resolve(saved);
    await waitFor(() =>
      expect(queryClient.getQueryState(["delivery-settings"])?.fetchStatus).toBe("idle")
    );
    expect(field("Population authors").value).toBe("alice\nbob");
  } finally {
    reread.resolve(saved);
    cleanup();
    getDeliverySettings.mockRestore();
    putDeliverySettings.mockRestore();
  }
});

// A save Dispatch refused landed nothing of this tab's: a list the reader typed with a line break
// after its last entry stays as typed when another writer's save then lands a record that holds
// the same entries - and contradicts the refused save's deploy workflow - so the name they type
// next is an entry of its own and the next Save sends three entries.
test("a refused save keeps a list's trailing line break over another writer's record of that list", async () => {
  const user = userEvent.setup();
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const form = (initial: DeliverySettings) => (
    <QueryClientProvider client={queryClient}>
      <DeliverySettingsForm initial={initial} />
    </QueryClientProvider>
  );
  const stored = { ...record, population_authors: ["alice", "bob"] };
  const message =
    "the GitHub App is not installed on acme/widgets: its token cannot read acme/widgets";
  const putDeliverySettings = spyOn(api, "putDeliverySettings")
    .mockRejectedValueOnce(new ApiError(409, { code: "DELIVERY_SETTINGS_ACCESS", error: message }))
    .mockResolvedValue(stored);
  const view = render(form(stored));
  try {
    await user.type(field("Population authors"), "{Enter}");
    await replace(user, "Deploy workflow", ".github/workflows/ship.yml");
    await user.click(screen.getByRole("button", { name: "Save delivery settings" }));
    expect((await screen.findByRole("alert")).textContent).toBe(message);
    // Another writer changes only the excluded repositories.
    view.rerender(
      form({ ...stored, excluded_repos: ["acme/dojo"], updated_at: "2026-10-08T12:00:00Z" })
    );
    expect(field("Population authors").value).toBe("alice\nbob\n");
    expect(field("Deploy workflow").value).toBe(".github/workflows/ship.yml");
    await user.type(field("Population authors"), "carol");
    expect(field("Population authors").value).toBe("alice\nbob\ncarol");
    await user.click(screen.getByRole("button", { name: "Save delivery settings" }));
    await waitFor(() => expect(putDeliverySettings).toHaveBeenCalledTimes(2));
    expect(putDeliverySettings.mock.calls[1]?.[0]).toMatchObject({
      population_authors: ["alice", "bob", "carol"],
    });
  } finally {
    cleanup();
    putDeliverySettings.mockRestore();
  }
});

// A record that arrives while this tab's save is still out is no answer to it: a list typed with
// a line break after its last entry stays as typed under another writer's record of the same
// entries, and follows the record once the save lands.
test("a record that arrives while the save is out keeps a list's trailing line break until the save lands", async () => {
  const user = userEvent.setup();
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const form = (initial: DeliverySettings) => (
    <QueryClientProvider client={queryClient}>
      <DeliverySettingsForm initial={initial} />
    </QueryClientProvider>
  );
  const stored = { ...record, population_authors: ["alice", "bob"] };
  const answer = Promise.withResolvers<DeliverySettings>();
  const putDeliverySettings = spyOn(api, "putDeliverySettings").mockReturnValue(answer.promise);
  const view = render(form(stored));
  try {
    await user.type(field("Population authors"), "{Enter}");
    await user.click(screen.getByRole("button", { name: "Save delivery settings" }));
    await waitFor(() => expect(putDeliverySettings).toHaveBeenCalledTimes(1));
    // Another writer changes only the excluded repositories while the save is out.
    const other = { ...stored, excluded_repos: ["acme/dojo"], updated_at: "2026-10-08T12:00:00Z" };
    view.rerender(form(other));
    expect(field("Population authors").value).toBe("alice\nbob\n");
    answer.resolve({ ...other, updated_at: "2026-10-08T12:00:01Z" });
    await waitFor(() => expect(field("Population authors").value).toBe("alice\nbob"));
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

// Save is held while its save is out: by `aria-disabled`, not `disabled`, which Chromium answers
// by taking the focus off the button (`emulateChromiumFocusFixup`). So Save keeps the focus
// through the save. At the save's landing its answer is written into the cache as the record, the
// form stays the same element and draws the stored values, and the focus stays on the same Save
// once it is let go: never on the form's first field, out of view at the form's top, where the
// reader's next key would be typed.
test("Save keeps the focus once the save lands and draws the stored record, and no field takes it", async () => {
  const user = userEvent.setup();
  const stored: DeliverySettings = {
    ...record,
    deploy_repo: "acme/gadgets",
    excluded_repos: ["acme/dojo", "acme/gadgets-smoke"],
    updated_at: "2026-10-08T09:00:00Z",
  };
  const answer = Promise.withResolvers<DeliverySettings>();
  const getDeliverySettings = spyOn(api, "getDeliverySettings").mockResolvedValue(record);
  const putDeliverySettings = spyOn(api, "putDeliverySettings").mockReturnValue(answer.promise);
  const restoreFocus = emulateChromiumFocusFixup();

  try {
    const queryClient = renderForm({ inMain: true });
    await screen.findByDisplayValue("acme/widgets");
    await replace(user, "Deploy repository", "acme/gadgets");
    // The list as typed, one line; the server stores it as its entries.
    await replace(user, "Excluded repositories", "acme/dojo, acme/gadgets-smoke");
    await user.tab();
    const save = screen.getByRole("button", { name: "Save delivery settings" });
    expect(save).toHaveFocus();
    await user.keyboard("{Enter}");

    await waitFor(() => expect(putDeliverySettings).toHaveBeenCalledTimes(1));
    // Save is held while the save is out, and keeps the focus.
    await afterFrameAndTasks();
    expect(save).toHaveAttribute("aria-disabled", "true");
    expect(save).toHaveFocus();

    getDeliverySettings.mockResolvedValue(stored);
    await act(async () => answer.resolve(stored));
    await waitFor(() => expect(queryClient.isMutating()).toBe(0));
    await waitFor(() => expect(save).not.toHaveAttribute("aria-disabled"));
    await afterFrameAndTasks();
    expect(document.activeElement).toBe(save);
    expect(screen.getByRole("button", { name: "Save delivery settings" })).toBe(save);
    expect(field("Deploy repository")).not.toHaveFocus();
    expect(field("Deploy repository").value).toBe("acme/gadgets");
    expect(field("Excluded repositories").value).toBe("acme/dojo\nacme/gadgets-smoke");
  } finally {
    answer.resolve(stored);
    restoreFocus();
    cleanup();
    getDeliverySettings.mockRestore();
    putDeliverySettings.mockRestore();
  }
});

// Held by `aria-disabled` while its save is out, Save draws as a control that cannot be pressed:
// its look is the `refused:` variant's (`styles.css`), which covers `aria-disabled` as it covers
// `disabled`. A `disabled:` look would leave the held Save drawn as one that can be pressed.
test("a Save held while its save is out draws as one that cannot be pressed", async () => {
  const user = userEvent.setup();
  const answer = Promise.withResolvers<DeliverySettings>();
  const getDeliverySettings = spyOn(api, "getDeliverySettings").mockResolvedValue(record);
  const putDeliverySettings = spyOn(api, "putDeliverySettings").mockReturnValue(answer.promise);
  try {
    renderForm();
    await screen.findByDisplayValue("acme/widgets");
    await replace(user, "Deploy repository", "acme/gadgets");
    const save = screen.getByRole("button", { name: "Save delivery settings" });
    await user.click(save);
    await waitFor(() => expect(putDeliverySettings).toHaveBeenCalledTimes(1));
    await afterFrameAndTasks();
    expect(save).toHaveAttribute("aria-disabled", "true");
    expect(save).not.toBeDisabled();
    expect(save.className.split(/\s+/)).toEqual(
      expect.arrayContaining(["refused:cursor-not-allowed", "refused:opacity-50"])
    );
  } finally {
    answer.resolve(record);
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

// The Delivery page holds no record for the form to read again: it draws the timeline in the
// form's place once the snapshot a save brings comes (`useHoldUntilDrawn`). Where that snapshot
// does not come, Save is let go at the hold's deadline, and the form says why the timeline is not
// drawn, in the words the page says for a timeline read that failed.
test("on the Delivery page, Save let go at its hold's deadline with no timeline drawn says why", async () => {
  const user = userEvent.setup();
  // The save's answer, held until the row gives it, so what it sets off runs in `act`.
  const saved = Promise.withResolvers<DeliverySettings>();
  const putDeliverySettings = spyOn(api, "putDeliverySettings").mockImplementation(
    () => saved.promise
  );
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  try {
    render(
      <QueryClientProvider client={queryClient}>
        <DeliverySettingsForm initial={null} />
      </QueryClientProvider>
    );
    await fill(user, "Deploy repository", "acme/widgets");
    await fill(user, "Deploy workflow", ".github/workflows/deploy.yml");
    await fill(user, "Production jobs", "widgets-release / widgets-release");
    await fill(user, "PR checks workflow", ".github/workflows/pr-checks.yml");
    await fill(user, "Population authors", "octocat");
    const save = screen.getByRole("button", { name: "Save delivery settings" });
    jest.useFakeTimers();
    await userOnFakeClock().click(save);
    await tick(0);
    expect(putDeliverySettings).toHaveBeenCalledTimes(1);
    await act(async () => saved.resolve(record));
    await tick(0);
    expect(save).toHaveAttribute("aria-disabled", "true");

    // The save landed in the step before, which is when the hold's timer started.
    await tick(SEND_DEADLINE_MS - 1_000);
    expect(save).toHaveAttribute("aria-disabled", "true");
    expect(screen.queryByRole("alert")).toBeNull();

    await tick(1_000);
    expect(save).not.toHaveAttribute("aria-disabled");
    expect(
      screen.queryByRole("alert")?.textContent,
      "Save let go at its deadline and said nothing of the timeline it waited for"
    ).toBe("Couldn't load the delivery timeline. Dispatch did not answer within 30 seconds.");
  } finally {
    cleanup();
    queryClient.clear();
    realClock();
    putDeliverySettings.mockRestore();
  }
});

// Another tab saved, and this tab's read brings the record with its new write time: the fields
// the reader did not change show the stored record, the one they changed stays as typed (#5224's
// draft model, as the rows above hold), and a refusal of this tab's draft goes with the record it
// was refused over.
test("a save of another tab's that the read brings fills the fields the reader did not change from the stored record, and the refusal shown goes", async () => {
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

    expect(field("Deploy repository").value).toBe("acme/elsewhere");
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
