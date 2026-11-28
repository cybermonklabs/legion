import { describe, expect, test } from "bun:test";
import { type Fixture, type FormRead, formProblems, readingProblems, readPlan } from "../plan";

const plan = await readPlan();

function fixture(id: string): Fixture {
  const found = plan.fixtures.find((one) => one.id === id);
  if (found === undefined) throw new Error(`expected.json has no fixture ${id}`);
  return found;
}

describe("the fixtures", () => {
  // INFRA-3977 asks for change requests that plant a principle, an undefined noun, a term with
  // another meaning and a new term with a defined meaning, and one that plants the three faults
  // of a check's shape together.
  test("plant each kind that INFRA-3977 asks for, the three of a check's shape in one request", () => {
    const planted = new Set(plan.fixtures.flatMap((one) => one.planted.map((item) => item.kind)));
    for (const kind of [
      "principle",
      "undefined-noun",
      "other-meaning",
      "repeated-meaning",
      "wide-claim",
      "agent-choice",
      "agent-where-program",
    ] as const) {
      expect(planted.has(kind), kind).toBe(true);
    }
    const shape = ["wide-claim", "agent-choice", "agent-where-program"];
    expect(
      plan.fixtures
        .filter((one) => shape.every((kind) => one.planted.some((item) => item.kind === kind)))
        .map((one) => one.id)
    ).toEqual(["6-check-faults"]);
  });

  // Whether Dispatch reads each one as a text or a term is the command's to check against its
  // answers before any agent runs (readingProblems).
  test("write each node and subject that a fixture names in its spec", () => {
    for (const one of plan.fixtures) {
      for (const item of [...one.planted, ...one.mustNotName]) {
        if (item.node !== "terms")
          expect(one.specText, `${one.id}: ${item.node}`).toContain(item.node);
        if (item.subject !== undefined) {
          expect(one.specText.toLowerCase(), `${one.id}: ${item.subject}`).toContain(item.subject);
        }
      }
    }
  });
});

const read = (
  nodes: readonly string[],
  definitions: readonly string[] = [],
  terms: readonly string[] = ["ask", "session"]
): FormRead => ({
  in_scope: true,
  scope: "covered",
  findings: [],
  nodes: nodes.map((node) => ({ kind: "statement", node })),
  definitions: definitions.map((term) => ({ term })),
  terms: terms.map((term) => ({ term })),
  unread: [],
});

// Before any agent runs, the command reads what the application's own reader read in each
// fixture's spec, and takes the statements, the checks and the terms from it.
describe("readingProblems", () => {
  test("passes a fixture whose nodes and terms Dispatch read as its expectation names them", () => {
    expect(
      readingProblems(fixture("4-other-meaning"), read(["session-in-ask", "person-session-ends"]))
    ).toEqual([]);
  });

  test("fails a node that Dispatch did not read", () => {
    expect(readingProblems(fixture("1-principle"), read(["inbox-page-limit"]))).toEqual([
      "1-principle: principle inbox-simple is no text that Dispatch read",
    ]);
  });

  test("fails a planted undefined noun that the list defines", () => {
    expect(
      readingProblems(
        fixture("2-undefined-noun"),
        read(["daily-digest", "answer-closes-ask"], [], ["ask", "Digest"])
      )
    ).toEqual([
      "2-undefined-noun: undefined-noun daily-digest digest is a term that the list or the request defines",
    ]);
  });

  test("fails a planted other meaning that no term defines", () => {
    expect(
      readingProblems(
        fixture("4-other-meaning"),
        read(["session-in-ask", "person-session-ends"], [], [])
      )
    ).toEqual([
      "4-other-meaning: other-meaning person-session-ends session is no term of the list or of the request",
    ]);
  });

  test("fails an item at the node terms that the request does not define", () => {
    expect(
      readingProblems(
        fixture("5-repeated-meaning"),
        read(["read-state-set", "thread-in-ask"], ["read state"])
      )
    ).toEqual([
      "5-repeated-meaning: repeated-meaning terms agent thread is no term that the request defines",
    ]);
  });

  test("reads a form-limit item's subject as a rule and its node prose as no text, and fails another node that Dispatch did not read", () => {
    const one = {
      ...fixture("1-principle"),
      planted: [
        { kind: "form-limit", node: "prose", subject: "pull-request" },
        { kind: "form-limit", node: "inbox-simple", subject: "person" },
        { kind: "form-limit", node: "inbox-gone", subject: "incident" },
      ] as const,
      mustNotName: [],
    };
    expect(readingProblems(one, read(["inbox-simple", "inbox-page-limit"]))).toEqual([
      "1-principle: form-limit inbox-gone incident is no text that Dispatch read",
    ]);
  });

  test("fails an approved version under the root that Dispatch cannot read", () => {
    const line =
      "Dispatch cannot read terms.md version 1 of ACME-1, an approved document under the root.";
    expect(
      readingProblems(fixture("1-principle"), {
        ...read(["inbox-simple", "inbox-page-limit"]),
        unread: [{ line }],
      })
    ).toEqual([`1-principle: ${line}`]);
  });
});

describe("formProblems", () => {
  test("passes a spec in scope with no finding, and fails one with a finding or out of scope", () => {
    const one = fixture("1-principle");
    expect(formProblems(one, read([]))).toEqual([]);
    expect(
      formProblems(one, { ...read([]), findings: [{ kind: "statement", node: "inbox-simple" }] })
    ).toEqual([
      "the form check finds statement inbox-simple in 1-principle, which no fixture holds",
    ]);
    expect(formProblems(one, { ...read([]), in_scope: false, scope: "no_root" })).toEqual([
      "1-principle is out of scope (no_root), so DISPATCH_SPEC_ROOTS did not reach the server",
    ]);
  });
});
