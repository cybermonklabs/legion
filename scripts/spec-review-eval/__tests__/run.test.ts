import { describe, expect, test } from "bun:test";
import {
  type Answer,
  blockIds,
  compareFindings,
  type Fixture,
  parseFindings,
  readPlan,
  specFormCalled,
  termNames,
  withIssueKey,
} from "../run";

// The reviewer's answer is the line form of skills/spec-statement-review/SKILL.md, step 6:
// `FINDING <kind> <node> <subject>: <sentence>`, `NO FINDING`, or `NOT REVIEWED: ...` when a
// command failed. The driver reads it with parseFindings and judges it with compareFindings, so
// these rows hold both to that form, and to the rule that an answer that says nothing readable,
// or that says no review was made, never passes a run.

const plan = await readPlan();

function fixture(id: string): Fixture {
  const found = plan.fixtures.find((one) => one.id === id);
  if (found === undefined) throw new Error(`expected.json has no fixture ${id}`);
  return found;
}

describe("parseFindings", () => {
  test("reads each kind's line, the terms node, the - subject and a subject of two words", () => {
    const answer = parseFindings(
      [
        "FINDING principle lease-safety -: It states an aim, and no act of the daemon that a check can show.",
        "FINDING undefined-noun slot-release slot: Neither the list nor the terms of the request define slot.",
        "FINDING other-meaning phase-timeout phase: The list defines phase as a step, and this statement uses it for a time window.",
        "FINDING repeated-meaning terms work item: Its definition gives the meaning of issue, which the list defines.",
        "FINDING wide-claim v-lease-expiry -: The claim holds the lease of each role.",
        "FINDING agent-choice planner-reads-goals -: Only the choice of the planner can make it true.",
        "FINDING agent-where-program v-claim-refused -: An agent reads an exit status, which a program can compare with 1.",
      ].join("\n")
    );
    expect(answer).toEqual({
      verdict: "findings",
      findings: [
        {
          kind: "principle",
          node: "lease-safety",
          subject: null,
          sentence: "It states an aim, and no act of the daemon that a check can show.",
        },
        {
          kind: "undefined-noun",
          node: "slot-release",
          subject: "slot",
          sentence: "Neither the list nor the terms of the request define slot.",
        },
        {
          kind: "other-meaning",
          node: "phase-timeout",
          subject: "phase",
          sentence:
            "The list defines phase as a step, and this statement uses it for a time window.",
        },
        {
          kind: "repeated-meaning",
          node: "terms",
          subject: "work item",
          sentence: "Its definition gives the meaning of issue, which the list defines.",
        },
        {
          kind: "wide-claim",
          node: "v-lease-expiry",
          subject: null,
          sentence: "The claim holds the lease of each role.",
        },
        {
          kind: "agent-choice",
          node: "planner-reads-goals",
          subject: null,
          sentence: "Only the choice of the planner can make it true.",
        },
        {
          kind: "agent-where-program",
          node: "v-claim-refused",
          subject: null,
          sentence: "An agent reads an exit status, which a program can compare with 1.",
        },
      ],
    });
  });

  test("reads NO FINDING as a review that found nothing", () => {
    expect(parseFindings("NO FINDING\n")).toEqual({ verdict: "none" });
  });

  test("reads an answer that holds only a NOT REVIEWED line as no verdict", () => {
    const line =
      "NOT REVIEWED: dispatch spec-form --issue ACME-3 --artifact spec.md --terms failed: Dispatch did not answer";
    expect(parseFindings(line)).toEqual({
      verdict: "not-reviewed",
      reason:
        "dispatch spec-form --issue ACME-3 --artifact spec.md --terms failed: Dispatch did not answer",
    });
  });

  test("reads a NOT REVIEWED line as no verdict even beside findings", () => {
    const answer = parseFindings(
      "FINDING principle inbox-simple -: It states a quality.\nNOT REVIEWED: dispatch read --issue ACME-3 failed: 404"
    );
    expect(answer.verdict).toBe("not-reviewed");
  });

  test("refuses an answer with no FINDING and no NO FINDING line", () => {
    const answer = parseFindings("The change request looks fine to me.");
    expect(answer.verdict).toBe("unreadable");
  });

  test("refuses an empty answer", () => {
    expect(parseFindings("").verdict).toBe("unreadable");
  });

  test("refuses a FINDING line of a kind the skill does not name", () => {
    expect(parseFindings("FINDING sentences inbox-simple -: It has 3 sentences.").verdict).toBe(
      "unreadable"
    );
  });

  test("refuses a FINDING line with no subject and no sentence", () => {
    expect(parseFindings("FINDING principle inbox-simple").verdict).toBe("unreadable");
  });

  test("refuses NO FINDING beside a FINDING line", () => {
    expect(
      parseFindings("NO FINDING\nFINDING principle inbox-simple -: It states a quality.").verdict
    ).toBe("unreadable");
  });
});

describe("compareFindings", () => {
  const principle = fixture("1-principle");

  test("passes a run that names exactly the planted items", () => {
    const answer = parseFindings(
      "FINDING principle inbox-simple -: It states a quality, and no act."
    );
    expect(compareFindings(principle, answer)).toEqual({ pass: true, problems: [] });
  });

  test("compares a subject without its letter case and its code quotes", () => {
    const answer = parseFindings(
      "FINDING undefined-noun daily-digest `Digest`: Nothing defines it."
    );
    expect(compareFindings(fixture("2-undefined-noun"), answer).pass).toBe(true);
  });

  test("fails a run that misses a planted item", () => {
    const result = compareFindings(principle, { verdict: "none" });
    expect(result.pass).toBe(false);
    expect(result.problems).toEqual(["missed the planted principle inbox-simple"]);
  });

  test("fails a run that names fixture 1's specific beside its principle", () => {
    const answer = parseFindings(
      "FINDING principle inbox-simple -: It states a quality.\nFINDING principle inbox-page-limit -: It has a limit."
    );
    const result = compareFindings(principle, answer);
    expect(result.pass).toBe(false);
    expect(result.problems).toEqual(["named inbox-page-limit, which it must not name (principle)"]);
  });

  test("fails a run that names fixture 3's noun, which its list defines", () => {
    const answer = parseFindings(
      "FINDING undefined-noun daily-digest digest: Nothing defines digest."
    );
    const result = compareFindings(fixture("3-defined-noun"), answer);
    expect(result.pass).toBe(false);
    expect(result.problems).toEqual([
      "named daily-digest digest, which it must not name (undefined-noun)",
    ]);
  });

  test("fails a run that names an item that is neither planted nor listed", () => {
    const answer = parseFindings(
      "FINDING principle inbox-simple -: It states a quality.\nFINDING wide-claim v-somewhere -: It is wide."
    );
    const result = compareFindings(principle, answer);
    expect(result.pass).toBe(false);
    expect(result.problems).toEqual(["named wide-claim v-somewhere, which is not planted"]);
  });

  test("fails a run whose kind is wrong for the planted node", () => {
    const answer = parseFindings("FINDING agent-choice inbox-simple -: An agent decides it.");
    const result = compareFindings(principle, answer);
    expect(result.pass).toBe(false);
    expect(result.problems).toContain("missed the planted principle inbox-simple");
  });

  test("passes NO FINDING where nothing is planted", () => {
    expect(compareFindings(fixture("3-defined-noun"), { verdict: "none" })).toEqual({
      pass: true,
      problems: [],
    });
  });

  test("never passes an answer that holds only a NOT REVIEWED line, even where nothing is planted", () => {
    const answer = parseFindings(
      "NOT REVIEWED: dispatch spec-form --issue ACME-4 --artifact spec.md --terms failed: unknown flag"
    );
    for (const one of plan.fixtures) {
      const result = compareFindings(one, answer);
      expect(result.pass).toBe(false);
      expect(result.problems).toEqual([
        "gave no verdict: dispatch spec-form --issue ACME-4 --artifact spec.md --terms failed: unknown flag",
      ]);
    }
  });

  test("never passes an unreadable answer", () => {
    const answer: Answer = parseFindings("All good.");
    const result = compareFindings(fixture("3-defined-noun"), answer);
    expect(result.pass).toBe(false);
    expect(result.problems[0]).toStartWith("gave an unreadable answer");
  });
});

describe("specFormCalled", () => {
  const event = (toolName: string, args: Record<string, unknown>): string =>
    JSON.stringify({ type: "tool_execution_start", toolName, args });

  test("finds a bash call that runs dispatch spec-form", () => {
    const stdout = [
      event("read", { path: "/repo/skills/spec-statement-review/SKILL.md" }),
      event("bash", { command: "dispatch spec-form --issue ACME-3 --artifact spec.md --terms" }),
    ].join("\n");
    expect(specFormCalled(stdout)).toBe(true);
  });

  test("finds none where the agent only read files and ran another command", () => {
    const stdout = [
      event("read", { path: "/repo/skills/spec-statement-review/SKILL.md" }),
      event("bash", { command: "dispatch doc-read --issue ACME-3 --artifact spec.md" }),
      "not json",
    ].join("\n");
    expect(specFormCalled(stdout)).toBe(false);
  });
});

describe("the fixtures", () => {
  test("hold six change requests under the two roots", () => {
    expect(plan.fixtures.map((one) => one.id)).toEqual([
      "1-principle",
      "2-undefined-noun",
      "3-defined-noun",
      "4-other-meaning",
      "5-repeated-meaning",
      "6-checks-in-verification",
    ]);
    expect(plan.roots.map((root) => root.key)).toEqual(["ACME-1", "ACME-2"]);
  });

  test("each planted and each listed node is in its fixture's text", () => {
    for (const one of plan.fixtures) {
      const ids = [...blockIds(one.specText), ...blockIds(one.verificationText ?? ""), "terms"];
      for (const item of [...one.planted, ...one.mustNotName]) {
        expect(ids, `${one.id}: ${item.node}`).toContain(item.node);
      }
    }
  });

  test("each planted subject is in the text of its node", () => {
    for (const one of plan.fixtures) {
      for (const item of one.planted) {
        if (item.subject === undefined) continue;
        if (item.node === "terms") {
          expect(termNames(one.specText), one.id).toContain(item.subject);
        } else {
          expect(blockText(one.specText, item.node).toLowerCase(), one.id).toContain(item.subject);
        }
      }
    }
  });

  test("fixture 3 is fixture 2 under the root whose list defines its noun", () => {
    const two = fixture("2-undefined-noun");
    const three = fixture("3-defined-noun");
    expect(three.specText).toBe(two.specText);
    expect(termNames(rootTerms(two.root))).not.toContain("digest");
    expect(termNames(rootTerms(three.root))).toContain("digest");
  });

  test("fixture 6 keeps every check in its Verification document, the planted one among them", () => {
    const six = fixture("6-checks-in-verification");
    expect(six.specText).not.toContain(":::check");
    expect(blockIds(six.verificationText ?? "")).toContain("v-second-answer-agent");
  });

  test("a Verification document gets the key of its own issue in each serves", () => {
    const text = withIssueKey(fixture("6-checks-in-verification").verificationText ?? "", "ACME-9");
    expect(text).not.toContain("@ISSUE@");
    expect(text).toContain('serves="dispatch://ACME-9/spec#b-second-answer-refused"');
  });
});

/** The body of the block whose id is `node`. */
function blockText(markdown: string, node: string): string {
  const match = new RegExp(`^:::\\w+\\{#${node}[^\\n]*\\n([\\s\\S]*?)\\n:::`, "m").exec(markdown);
  if (match === null) throw new Error(`no block ${node}`);
  return match[1] ?? "";
}

/** The terms of the named root. */
function rootTerms(name: string): string {
  const root = plan.roots.find((one) => one.name === name);
  if (root === undefined) throw new Error(`expected.json has no root ${name}`);
  return root.termsText;
}
