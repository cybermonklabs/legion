import { expect, test } from "bun:test";

import {
  applyInsertion,
  groupRows,
  insertionFor,
  insertionText,
  kindOrder,
  matchesQuery,
  orderedRows,
  REF_PICKER_HEADINGS,
  type RefPickerRow,
  readTrigger,
  stepRow,
} from "./ref-picker";

function row(kind: RefPickerRow["kind"], label: string, reference: string): RefPickerRow {
  return { detail: "", id: `${kind}-${label}`, kind, label, reference };
}

/** One set of rows, one of every kind the three doors rank differently. */
const rows: RefPickerRow[] = [
  row("issue", "Fix the thing", "dispatch://CORE-1"),
  row("document", "goals.md", "dispatch://CORE/artifact/goals-md"),
  row("comment", "a remark", "dispatch://CORE-1/comment/c1"),
  row("component", "The relay", "dispatch://CORE/component/relay"),
  row("agent", "Planner", "dispatch://agent/planner-session"),
  row("role", "reviewer", "dispatch://role/reviewer"),
  row("person", "alice@theorem.dev", "dispatch://person/alice@theorem.dev"),
];

test("the three doors rank one set of rows three ways, and each reaches every kind", () => {
  const kindsOf = (door: "all" | "parties" | "issues") =>
    groupRows(rows, door).map((group) => group.kind);

  expect(kindsOf("all")).toEqual([
    "document",
    "issue",
    "comment",
    "component",
    "agent",
    "role",
    "person",
  ]);
  expect(kindsOf("parties")).toEqual([
    "person",
    "agent",
    "role",
    "document",
    "issue",
    "comment",
    "component",
  ]);
  expect(kindsOf("issues")).toEqual([
    "issue",
    "document",
    "comment",
    "component",
    "agent",
    "role",
    "person",
  ]);
  // Every door reaches every kind: the three orders are the same set, and so is the kind list.
  for (const door of ["all", "parties", "issues"] as const) {
    expect([...kindOrder(door)].sort()).toEqual([...kindOrder("all")].sort());
    expect(orderedRows(groupRows(rows, door))).toHaveLength(rows.length);
  }
});

test("each group is headed by its kind's own name", () => {
  expect(groupRows(rows, "parties").map((group) => group.heading)).toEqual([
    "People",
    "Agents",
    "Roles",
    "Documents",
    "Issues",
    "Comments",
    "Components",
  ]);
  expect(REF_PICKER_HEADINGS["ask-part"]).toBe("Ask parts");
});

test("a trigger opens only at a word boundary", () => {
  expect(readTrigger("@")).toEqual({ door: "parties", query: "", start: 0 });
  expect(readTrigger("see @pl")).toEqual({ door: "parties", query: "pl", start: 4 });
  expect(readTrigger("(#CO")).toEqual({ door: "issues", query: "CO", start: 1 });
  expect(readTrigger("a line\n#")).toEqual({ door: "issues", query: "", start: 7 });
  expect(readTrigger("see [[go")).toEqual({ door: "all", query: "go", start: 4 });

  // Mid-word: an email's `@`, an anchor's `#`, and a `[[` glued to a word open nothing.
  expect(readTrigger("alice@theorem.dev")).toBeUndefined();
  expect(readTrigger("issue#5")).toBeUndefined();
  expect(readTrigger("note[[")).toBeUndefined();
});

test("a space typed after a trigger closes it, which leaves a heading a heading", () => {
  expect(readTrigger("# ")).toBeUndefined();
  expect(readTrigger("# Heading")).toBeUndefined();
  expect(readTrigger("@planner now")).toBeUndefined();
  // The latest trigger still standing is the one that is open.
  expect(readTrigger("# Heading with @pl")).toEqual({ door: "parties", query: "pl", start: 15 });
});

test("the browser-side filter matches a row's words, its detail and its address", () => {
  const agent = {
    ...row("agent", "Planner", "dispatch://agent/planner-session"),
    detail: "/w/one",
  };
  expect(matchesQuery(agent, "")).toBe(true);
  expect(matchesQuery(agent, "plan")).toBe(true);
  expect(matchesQuery(agent, "/w/")).toBe(true);
  expect(matchesQuery(agent, "planner-session")).toBe(true);
  expect(matchesQuery(agent, "reviewer")).toBe(false);
});

test("a party pick inserts a worded link and every other pick the bare address", () => {
  expect(insertionText(row("agent", "Planner", "dispatch://agent/planner-session"))).toBe(
    "[Planner](dispatch://agent/planner-session)"
  );
  expect(insertionText(row("role", "reviewer", "dispatch://role/reviewer"))).toBe(
    "[reviewer](dispatch://role/reviewer)"
  );
  expect(
    insertionText(row("person", "alice@theorem.dev", "dispatch://person/alice@theorem.dev"))
  ).toBe("[alice@theorem.dev](dispatch://person/alice@theorem.dev)");
  // A name holding a bracket cannot break out of the link's words.
  expect(insertionText(row("agent", "Planner [2]", "dispatch://agent/two"))).toBe(
    "[Planner \\[2\\]](dispatch://agent/two)"
  );

  expect(insertionText(row("issue", "Fix the thing", "dispatch://CORE-1"))).toBe(
    "dispatch://CORE-1"
  );
  expect(insertionText(row("component", "The relay", "dispatch://CORE/component/relay"))).toBe(
    "dispatch://CORE/component/relay"
  );
  expect(insertionText(row("document-block", "First words", "dispatch://CORE-1/spec#b-one"))).toBe(
    "dispatch://CORE-1/spec#b-one"
  );
});

test("an insertion replaces the trigger through the caret and keeps the text after it", () => {
  const text = "see #CO and the rest";
  const caret = "see #CO".length;
  const trigger = readTrigger(text.slice(0, caret));
  if (trigger === undefined) throw new Error("the trigger reads nothing");
  const insertion = insertionFor(row("issue", "Fix", "dispatch://CORE-1"), trigger, caret);

  expect(insertion).toEqual({ end: 7, start: 4, text: "dispatch://CORE-1" });
  expect(applyInsertion(text, insertion)).toEqual({
    caret: "see dispatch://CORE-1".length,
    text: "see dispatch://CORE-1 and the rest",
  });
});

test("the highlight wraps at either end of the list", () => {
  expect(stepRow(0, -1, 3)).toBe(2);
  expect(stepRow(2, 1, 3)).toBe(0);
  expect(stepRow(0, 1, 0)).toBe(0);
});
