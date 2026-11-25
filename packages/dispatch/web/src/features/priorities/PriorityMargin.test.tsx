import { afterEach, expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { api } from "../../api/client";
import type { PriorityAct } from "../../api/types";
import { PriorityMargin } from "./PriorityMargin";

const alice = { id: "alice", kind: "user" };
const bob = { id: "bob", kind: "user" };

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
});

function margin(acts: PriorityAct[] = HISTORY) {
  const history = spyOn(api, "getPriorityHistory").mockResolvedValue(acts);
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  // A session's name links to its conversation, so the margin stands inside a router.
  const view = render(
    <MemoryRouter>
      <QueryClientProvider client={client}>
        <PriorityMargin id="rocq-lean" />
      </QueryClientProvider>
    </MemoryRouter>
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

test("the margin lists the priority's own history, newest first, with each actor and time", async () => {
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

// The server writes a priority's visibility with the priority, and no control of these pages
// writes it, so it is part of "wrote this priority": the margin lists no entry of its own for
// it, while the history the server answers keeps the act.
test("the visibility a priority is written with is no entry of the margin", async () => {
  const visibility: PriorityAct = {
    actor: alice,
    asserted: true,
    at: "2026-11-10T09:00:00Z",
    field: "visibility",
    issue: "",
    value: "everyone",
  };
  margin([...HISTORY, visibility]);
  await screen.findByText("wrote this priority");
  expect(entries().map(({ words }) => words)).toEqual([
    "retracted the relation to the issue CORE-4",
    "related the issue CORE-4",
    "moved the order",
    "wrote the owner “INFRA controller”",
    "wrote the title “Rocq and Lean speed work restarted”",
    "wrote the description",
    "wrote this priority",
  ]);
});

test("the history is the whole list: nothing says the rest is not listed", async () => {
  margin();
  await screen.findByText("moved the order");
  const text = document.body.textContent ?? "";
  expect(/not listed here|no route|cannot be read/i.test(text)).toBe(false);
});

test("the day the acts fall on heads them, once each", async () => {
  // A heading is the reader's own day, and HISTORY's moments are UTC instants, which fall on
  // other local days in a zone far from UTC: in UTC-11 its 10:00 of 12 Nov is on 11 Nov, and
  // the acts fall on four days. So the acts are read at the same hours of the reader's days.
  margin(
    HISTORY.map((act) => {
      const at = new Date(act.at);
      return {
        ...act,
        at: new Date(
          at.getUTCFullYear(),
          at.getUTCMonth(),
          at.getUTCDate(),
          at.getUTCHours(),
          at.getUTCMinutes()
        ).toISOString(),
      };
    })
  );
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
      <PriorityMargin id="rocq-lean" />
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
  expect(screen.queryAllByRole("listitem").length).toBe(0);
});

test("the margin is the history alone: no control of states and no view that can only be empty", async () => {
  margin();
  await screen.findByText("moved the order");
  // A priority carries no comments, questions or pins until dispatch://INFRA-1098 makes its
  // description a document, so the margin offers no choice of what it shows.
  const aside = screen.getByRole("complementary", { name: "Margin" });
  expect(within(aside).queryAllByRole("combobox").length).toBe(0);
  expect(within(aside).queryAllByRole("listbox").length).toBe(0);
  expect(within(aside).queryAllByRole("tab").length).toBe(0);
  expect(aside.querySelectorAll("select")).toHaveLength(0);
  const text = aside.textContent ?? "";
  for (const word of ["Words anyone wrote", "Questions", "Pinned"]) {
    expect(text).not.toContain(word);
  }
  // Nor does it draw a pin control, or an entry for a document: the description is no document.
  expect(within(aside).queryAllByRole("button", { name: /pin/i }).length).toBe(0);
  expect(within(aside).queryAllByRole("link", { name: /pin|document/i }).length).toBe(0);
  expect(/document/i.test(text)).toBe(false);
  expect(within(aside).getAllByRole("listitem")).toHaveLength(HISTORY.length);
});

// The page saves a field half a second after the typing rests, so one person writing a
// description writes many acts. The history keeps each; the margin reads a run of them as one.
test("consecutive writes of one field by one person read as one entry, at the newest", async () => {
  const write = (at: string, field: string, actor = alice, value = "") => ({
    actor,
    asserted: true,
    at,
    field,
    issue: "",
    value,
  });
  margin([
    write("2026-11-12T10:00:03Z", "description"),
    write("2026-11-12T10:00:02Z", "description"),
    write("2026-11-12T10:00:01Z", "description"),
    // Another field between two runs breaks them.
    write("2026-11-12T10:00:00Z", "title", alice, "Speedruns"),
    write("2026-11-12T09:59:59Z", "description"),
    // And so does another person.
    write("2026-11-12T09:59:58Z", "description", bob),
    write("2026-11-12T09:59:57Z", "description"),
  ]);
  await screen.findByText("wrote the title “Speedruns”");
  expect(entries()).toEqual([
    { at: "2026-11-12T10:00:03Z", who: "alice", words: "wrote the description" },
    { at: "2026-11-12T10:00:00Z", who: "alice", words: "wrote the title “Speedruns”" },
    { at: "2026-11-12T09:59:59Z", who: "alice", words: "wrote the description" },
    { at: "2026-11-12T09:59:58Z", who: "bob", words: "wrote the description" },
    { at: "2026-11-12T09:59:57Z", who: "alice", words: "wrote the description" },
  ]);
});

// A run is one actor's as the history names it - a login, or a session - and not the person
// behind it: two sessions of one person are two actors, and their writes stay two entries.
test("writes of one field by two sessions of one person do not fold together", async () => {
  const session = (id: string) => ({ id, kind: "session", owner: "alice" });
  const write = (at: string, actor: { id: string; kind: string; owner: string }) => ({
    actor,
    asserted: true,
    at,
    field: "description",
    issue: "",
    value: "",
  });
  margin([
    write("2026-11-12T10:00:03Z", session("session-one")),
    write("2026-11-12T10:00:02Z", session("session-one")),
    write("2026-11-12T10:00:01Z", session("session-two")),
    write("2026-11-12T10:00:00Z", session("session-two")),
  ]);
  await screen.findAllByText("wrote the description");
  expect(entries().map(({ at, words }) => ({ at, words }))).toEqual([
    { at: "2026-11-12T10:00:03Z", words: "wrote the description" },
    { at: "2026-11-12T10:00:01Z", words: "wrote the description" },
  ]);
});
