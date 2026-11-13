import { afterEach, beforeEach, expect, jest, type Mock, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, render, screen, waitFor, within } from "@testing-library/react";
import type { UserEvent } from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";

import { realClock, tick } from "../../__tests__/fake-clock";
import { answerRoute } from "../../__tests__/send-answers";
import { userOnFakeClock } from "../../__tests__/user";
import { api, type WriteBounds } from "../../api/client";
import type {
  GoingCapability,
  GoingDecisionEvent,
  GoingEvent,
  GoingProposalEvent,
  GoingWriteInput,
  GoingWriteLine,
  IssueReferenceSummary,
} from "../../api/types";
import { KeymapProvider } from "../shell/KeymapProvider";
import { forgetTurns } from "./going-turns";
import { OPERATION_OPTION_ID, offerOptionId } from "./OperationRows";
import { SearchPalette } from "./SearchPalette";
import { searchAnswer } from "./search-fixture";
import { optionId } from "./search-model";
import { forgetAnsweredRun } from "./useGoingAnswer";
import { KEYSTROKE_DEBOUNCE_MS, MEANING_PAUSE_MS } from "./useTieredSearch";

// The bar's operations as the person meets them: the operation the decision acts on is a row of
// its own above the hits, which the highlight never moves to by itself, and the bar's submit runs
// it; a submit before the proposal prepares and runs as it lands; below the threshold the two
// best are offered and only a pick runs one; a missing argument is asked for; and with the
// person's setting on every write asks first. The clock is held, as in AnswerPanel.test.tsx.

const configured: GoingCapability = {
  answer: { blocks: true, configured: true, model: "e2e-chat" },
  decision: { configured: true, model: "e2e-decide", reachable: true },
  label: { configured: true },
  session: { configured: true },
};

const hit = {
  href: "/issues/LEGION-3",
  id: "LEGION-3",
  kind: "issue",
  owner: { key: "LEGION-3", kind: "issue", status: "todo", title: "Instruments" },
  rank: 1,
  snippet: "The <mark>astrolabe</mark>.",
} as const;

let streams: { readonly send: (frame: GoingEvent) => void }[];
let settings: Mock<typeof api.myGoingSettings>;
let spies: { mockRestore: () => void }[];
type WriteAnswer = (run: string, input: GoingWriteInput, bounds: WriteBounds) => Promise<unknown>;
let written: Mock<WriteAnswer>;

beforeEach(() => {
  jest.useFakeTimers();
  streams = [];
  settings = spyOn(api, "myGoingSettings").mockResolvedValue({ confirm_writes: false });
  spies = [
    settings,
    spyOn(api, "createGoingRequest").mockImplementation(async (input) => ({
      author: { id: "alice", kind: "user" },
      created_at: new Date().toISOString(),
      ends_at: new Date(Date.now() + 30_000).toISOString(),
      id: "run-1",
      project: input.project ?? "",
      words: input.words,
    })),
    spyOn(api, "cancelGoingRequest").mockImplementation(async (id) => ({
      cancelled_at: new Date().toISOString(),
      id,
    })),
    spyOn(api, "goingCapability").mockResolvedValue(configured),
    spyOn(api, "search").mockResolvedValue(searchAnswer([hit])),
    spyOn(api, "getIssueSummary").mockImplementation((key) =>
      Promise.resolve({
        documents: [],
        key,
        project: "LEGION",
        status: "todo",
        title: "Instruments",
      } satisfies IssueReferenceSummary)
    ),
    spyOn(api, "streamGoingAnswer").mockImplementation((_id, onFrame, signal) => {
      streams.push({ send: (frame) => onFrame(frame, 1) });
      const { promise, reject } = Promise.withResolvers<void>();
      signal.addEventListener("abort", () => reject(signal.reason), { once: true });
      return promise;
    }),
  ];
  written = answerRoute<WriteAnswer>("POST", "/api/v1/going/runs/*/writes");
  written.mockImplementation(async (_run, input) => {
    const line: GoingWriteLine = {
      action: input.offer,
      before: { status: "todo" },
      field: "status",
      item: `LEGION-${3 + (input.invocation ?? 0)}`,
      now: { status: "in_progress" },
      outcome: { code: "", message: "", ok: true, status: 200 },
      write_id: `write-${input.invocation ?? 0}`,
    };
    return line;
  });
});

afterEach(() => {
  for (const spy of spies) spy.mockRestore();
  forgetAnsweredRun();
  forgetTurns();
  realClock();
});

function renderBar(): { unmount: () => void } {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const view = render(
    <MemoryRouter initialEntries={["/projects/LEGION/issues"]}>
      <QueryClientProvider client={queryClient}>
        <KeymapProvider>
          <SearchPalette onClose={() => {}} open />
        </KeymapProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );
  return {
    unmount: () => {
      view.unmount();
      queryClient.clear();
    },
  };
}

/** Types `words`, waits for their hits, and lets the pause ask the question. */
async function ask(user: UserEvent, words: string): Promise<void> {
  const input = screen.getByRole<HTMLInputElement>("combobox", { name: "Search" });
  await user.click(input);
  await user.keyboard(words);
  await tick(KEYSTROKE_DEBOUNCE_MS);
  await waitFor(() => expect(screen.getAllByRole("option").length).toBeGreaterThan(0));
  await tick(MEANING_PAUSE_MS - KEYSTROKE_DEBOUNCE_MS);
  await waitFor(() => expect(streams).toHaveLength(1));
}

function decision(act: boolean, actions: readonly string[]): GoingDecisionEvent {
  return {
    data: {
      act,
      actions: actions.map((action, index) => ({
        action,
        confidence: index === 0 ? 0.9 : null,
        probability: null,
      })),
      context: "named",
      display: { chart: false, choice: "prose", confidence: 0.4 },
    },
    event: "decision",
  };
}

/** A status change of LEGION-3 and LEGION-4, and a comment on LEGION-3. */
const proposal: GoingProposalEvent = {
  data: {
    offers: [
      {
        action: "dispatch_issue_update",
        invocations: [
          { arguments: { issue: "LEGION-3", status: "in_progress" }, missing: null },
          { arguments: { issue: "LEGION-4", status: "in_progress" }, missing: null },
        ],
        offered: true,
      },
      {
        action: "dispatch_comment",
        invocations: [{ arguments: { body: "On it.", issue: "LEGION-3" }, missing: null }],
        offered: true,
      },
    ],
  },
  event: "proposal",
};

const ended: GoingEvent = { data: { items: 0, words: 3 }, event: "end" };

/** What each write sent: the offer, the invocation and how it came to run. */
function sent(): unknown[] {
  return written.mock.calls.map(([, input]) => input);
}

/** Hands the run's stream `frames`, in order, as the server writes them. */
function stream(...frames: GoingEvent[]): void {
  act(() => {
    for (const frame of frames) streams[0]?.send(frame);
  });
}

/** The bar's submit, from the box. */
async function submit(user: UserEvent): Promise<void> {
  await user.keyboard("{Control>}{Enter}{/Control}");
}

test("at or above the threshold the operation is a row above the hits the highlight never takes, and the submit runs each invocation at once", async () => {
  const user = userOnFakeClock();
  const bar = renderBar();
  try {
    await ask(user, "astrolabe");
    stream(decision(true, ["dispatch_issue_update", "dispatch_comment"]), proposal, ended);
    const row = await screen.findByRole("option", {
      name: /^Run issue update on LEGION-3 and LEGION-4/,
    });
    expect(row).toHaveAttribute("id", OPERATION_OPTION_ID);
    expect(within(row).getByRole("button", { name: "Run" })).toBeInTheDocument();
    // The hit stays the head: an Enter now opens it and runs nothing.
    expect(screen.getByRole("combobox", { name: "Search" })).toHaveAttribute(
      "aria-activedescendant",
      optionId(hit)
    );
    expect(written).not.toHaveBeenCalled();

    await submit(user);

    await waitFor(() =>
      expect(sent()).toEqual([
        { approved: "auto", invocation: 0, offer: "dispatch_issue_update" },
        { approved: "auto", invocation: 1, offer: "dispatch_issue_update" },
      ])
    );
    const lines = await screen.findAllByTestId("going-write-line");
    await waitFor(() =>
      expect(lines.map((line) => line.getAttribute("data-state"))).toEqual(["done", "done"])
    );
    expect(lines[0]).toHaveTextContent("LEGION-3 · status: todo → in_progress");
    expect(lines[1]).toHaveTextContent("LEGION-4 · status: todo → in_progress");
    // The operation ran, so its row is gone and no second submit runs it again.
    expect(screen.queryByRole("option", { name: /^Run issue update/ })).not.toBeInTheDocument();
  } finally {
    bar.unmount();
  }
});

test("a submit before the proposal says it is preparing and runs once the proposal lands", async () => {
  const user = userOnFakeClock();
  const bar = renderBar();
  try {
    await ask(user, "astrolabe");
    stream(decision(true, ["dispatch_issue_update"]));
    await screen.findByRole("option", { name: /^Run issue update/ });

    await submit(user);

    expect(await screen.findByTestId("going-operation-preparing")).toBeInTheDocument();
    expect(written).not.toHaveBeenCalled();
    stream(proposal);
    await waitFor(() => expect(written).toHaveBeenCalledTimes(2));
    expect(screen.queryByTestId("going-operation-preparing")).not.toBeInTheDocument();
  } finally {
    bar.unmount();
  }
});

test("below the threshold the two best are offered, a submit runs neither, and a pick runs that one as the person's", async () => {
  const user = userOnFakeClock();
  const bar = renderBar();
  try {
    await ask(user, "astrolabe");
    stream(decision(false, ["dispatch_issue_update", "dispatch_comment"]), proposal, ended);
    const comment = await screen.findByRole("option", { name: /^Run comment on LEGION-3/ });
    expect(comment).toHaveAttribute("id", offerOptionId("dispatch_comment"));
    expect(
      screen.getByRole("option", { name: /^Run issue update on LEGION-3 and LEGION-4/ })
    ).toHaveAttribute("id", offerOptionId("dispatch_issue_update"));

    await submit(user);
    await tick(1000);
    expect(written).not.toHaveBeenCalled();

    await user.click(comment);
    await waitFor(() =>
      expect(sent()).toEqual([{ approved: "person", invocation: 0, offer: "dispatch_comment" }])
    );
  } finally {
    bar.unmount();
  }
});

test("with the person's setting on, the operation asks first and is sent confirmed once they confirm", async () => {
  settings.mockResolvedValue({ confirm_writes: true });
  const user = userOnFakeClock();
  const bar = renderBar();
  try {
    await ask(user, "astrolabe");
    stream(decision(true, ["dispatch_issue_update"]), proposal, ended);
    await screen.findByRole("option", { name: /^Run issue update/ });

    await submit(user);

    const step = await screen.findByTestId("going-confirm-step");
    expect(
      within(step)
        .getAllByTestId("going-write-line")
        .map((line) => line.textContent)
    ).toEqual(["issue update LEGION-3: asks first", "issue update LEGION-4: asks first"]);
    await tick(1000);
    expect(written).not.toHaveBeenCalled();

    await user.click(within(step).getByRole("button", { name: "Confirm" }));
    await waitFor(() =>
      expect(sent()).toEqual([
        { approved: "confirmed", invocation: 0, offer: "dispatch_issue_update" },
        { approved: "confirmed", invocation: 1, offer: "dispatch_issue_update" },
      ])
    );
  } finally {
    bar.unmount();
  }
});

test("an invocation missing one argument asks for it, and the write carries the value as its fill", async () => {
  const user = userOnFakeClock();
  const bar = renderBar();
  try {
    await ask(user, "astrolabe");
    stream(
      decision(true, ["dispatch_follow"]),
      {
        data: {
          offers: [
            {
              action: "dispatch_follow",
              invocations: [
                {
                  arguments: { action: "follow", ask: "ask-1" },
                  missing: { field: "@session", kind: "text" },
                },
              ],
              offered: true,
            },
          ],
        },
        event: "proposal",
      } satisfies GoingProposalEvent,
      ended
    );
    await screen.findByRole("option", { name: /^Run follow on ask-1/ });

    await submit(user);

    const field = await screen.findByRole("textbox", { name: "Fill the session for ask-1" });
    await waitFor(() => expect(field).toHaveFocus());
    expect(written).not.toHaveBeenCalled();
    await user.keyboard("Planner{Enter}");
    await waitFor(() =>
      expect(sent()).toEqual([
        { approved: "auto", fill: "Planner", invocation: 0, offer: "dispatch_follow" },
      ])
    );
  } finally {
    bar.unmount();
  }
});
