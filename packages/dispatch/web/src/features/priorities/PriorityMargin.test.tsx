import { afterEach, expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { api } from "../../api/client";
import type { PriorityAct, PriorityRow } from "../../api/types";
import { PriorityMargin } from "./PriorityMargin";

const alice = { id: "alice", kind: "user" };
const bob = { id: "bob", kind: "user" };

const PRIORITY: PriorityRow = {
  id: "rocq-lean",
  title: "Rocq and Lean speed work restarted",
  description: "",
  owner: "INFRA controller",
  rank: "n",
  state: "live",
  made_at: "2026-11-10T09:00:00Z",
  made_by: alice,
  work: {
    by_status: null,
    claimed: 0,
    closed_last_7_days: 0,
    newest_event_at: null,
    open_asks: 0,
  },
};

/** What the server answers for this priority, newest first, as it writes a history: the two acts
 *  of one `advances` relation, the three writes under them, and the acts of the making. */
const HISTORY: PriorityAct[] = [
  {
    actor: bob,
    asserted: false,
    at: "2026-11-12T12:00:00Z",
    field: "advances",
    issue: "CORE-4",
    value: "",
  },
  {
    actor: alice,
    asserted: true,
    at: "2026-11-12T11:00:00Z",
    field: "advances",
    issue: "CORE-4",
    value: "",
  },
  {
    actor: alice,
    asserted: true,
    at: "2026-11-12T10:00:00Z",
    field: "rank",
    issue: "",
    value: "0|hzzzzz:",
  },
  {
    actor: bob,
    asserted: true,
    at: "2026-11-11T09:00:00Z",
    field: "owner",
    issue: "",
    value: "INFRA controller",
  },
  {
    actor: bob,
    asserted: true,
    at: "2026-11-11T08:00:00Z",
    field: "title",
    issue: "",
    value: "Rocq and Lean speed work restarted",
  },
  {
    actor: alice,
    asserted: true,
    at: "2026-11-10T09:00:00Z",
    field: "description",
    issue: "",
    value: "# Why\n\nSpeedruns run again.",
  },
  {
    actor: alice,
    asserted: true,
    at: "2026-11-10T09:00:00Z",
    field: "state",
    issue: "",
    value: "live",
  },
];

const restores: (() => void)[] = [];
afterEach(() => {
  for (const restore of restores.splice(0)) restore();
  localStorage.clear();
});

function margin(acts: PriorityAct[] = HISTORY) {
  const history = spyOn(api, "getPriorityHistory").mockResolvedValue(acts);
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const view = render(
    <QueryClientProvider client={client}>
      <PriorityMargin priority={PRIORITY} />
    </QueryClientProvider>
  );
  restores.push(() => {
    view.unmount();
    client.clear();
    history.mockRestore();
  });
  return { client, history, view };
}

/** Each entry as a reader sees it: who stated the act, the moment the entry carries, and what it
 *  says, in the order the list draws them. */
function entries(): { at: string; who: string; words: string }[] {
  const list = screen.getByRole("complementary", { name: "Margin" });
  return within(list)
    .getAllByRole("listitem")
    .map((item) => {
      const lines = [...item.querySelectorAll("p")];
      return {
        at: item.querySelector("time")?.getAttribute("datetime") ?? "",
        who: (lines.at(-2)?.textContent ?? "").split(" ·")[0] ?? "",
        words: lines.at(-1)?.textContent ?? "",
      };
    });
}

test("everything lists the priority's own history, newest first, with each actor and time", async () => {
  const { history } = margin();
  await screen.findByText("retracted the relation to the issue CORE-4");
  expect(history).toHaveBeenCalledWith("rocq-lean");
  expect(entries()).toEqual([
    { at: "2026-11-12T12:00:00Z", who: "bob", words: "retracted the relation to the issue CORE-4" },
    { at: "2026-11-12T11:00:00Z", who: "alice", words: "related the issue CORE-4" },
    { at: "2026-11-12T10:00:00Z", who: "alice", words: "moved the order" },
    { at: "2026-11-11T09:00:00Z", who: "bob", words: "wrote the owner “INFRA controller”" },
    {
      at: "2026-11-11T08:00:00Z",
      who: "bob",
      words: "wrote the title “Rocq and Lean speed work restarted”",
    },
    { at: "2026-11-10T09:00:00Z", who: "alice", words: "wrote the description" },
    { at: "2026-11-10T09:00:00Z", who: "alice", words: "wrote this priority" },
  ]);
});

test("the history is the whole list: nothing says the rest is not listed", async () => {
  margin();
  await screen.findByText("moved the order");
  const text = document.body.textContent ?? "";
  expect(/not listed here|no route|cannot be read/i.test(text)).toBe(false);
});

test("the day the acts fall on heads them, once each", async () => {
  margin();
  await screen.findByText("moved the order");
  // The heading is the one line of an entry that names no actor; the seven acts fall on three
  // days, and each day is said once, above its newest act. The words of a heading are the
  // reader's own locale and the day they read on ("Today", "Yesterday", a date), so what is
  // read here is that there are three of them and where they sit.
  const headed = [...document.querySelectorAll("li")].map((item) =>
    [...item.querySelectorAll("p")].length === 3 ? "heads a day" : "under one"
  );
  expect(headed).toEqual([
    "heads a day",
    "under one",
    "under one",
    "heads a day",
    "under one",
    "heads a day",
    "under one",
  ]);
});

test("a history that cannot be read says so, and lists nothing", async () => {
  const history = spyOn(api, "getPriorityHistory").mockRejectedValue(new Error("no"));
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const view = render(
    <QueryClientProvider client={client}>
      <PriorityMargin priority={PRIORITY} />
    </QueryClientProvider>
  );
  restores.push(() => {
    view.unmount();
    client.clear();
    history.mockRestore();
  });
  expect(await screen.findByRole("alert")).toHaveTextContent(
    "Could not load what happened to this priority."
  );
  expect(screen.queryAllByRole("listitem")).toEqual([]);
});

test("the other three states each say in one line why nothing of that kind is there", async () => {
  const { history } = margin();
  await screen.findByText("moved the order");
  const state = screen.getByRole("combobox", { name: /Margin/ });
  for (const [option, line] of [
    ["words", "A priority carries no comments yet, so nobody has written anything here."],
    ["questions", "A priority carries no asks yet, so no question has been asked about this one."],
    ["pinned", "A priority carries nothing a reader can pin yet."],
  ]) {
    await userEvent.selectOptions(state, option as string);
    expect(screen.getByText(line as string)).toBeDefined();
    expect(screen.queryAllByRole("listitem")).toEqual([]);
    expect(screen.queryAllByRole("link")).toEqual([]);
  }
  expect(history).toHaveBeenCalledTimes(1);
});

test("the state the reader chose survives a return to the page", async () => {
  margin();
  await screen.findByText("moved the order");
  await userEvent.selectOptions(screen.getByRole("combobox", { name: /Margin/ }), "pinned");
  for (const restore of restores.splice(0)) restore();

  margin();
  expect(screen.getByRole("combobox", { name: /Margin/ })).toHaveValue("pinned");
  expect(screen.getByText("A priority carries nothing a reader can pin yet.")).toBeDefined();
});
