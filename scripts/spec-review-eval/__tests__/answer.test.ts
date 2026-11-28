import { describe, expect, test } from "bun:test";
import {
  compareFindings,
  type Expectation,
  evaluationNamed,
  parseFindings,
  specFormCalled,
} from "../answer";
import { readPlan } from "../plan";

// The reviewer's answer is the line form of step 7 of skills/spec-statement-review/SKILL.md. One
// row for each way parseFindings and compareFindings decide.

const plan = await readPlan();

function fixture(id: string): Expectation {
  const found = plan.fixtures.find((one) => one.id === id);
  if (found === undefined) throw new Error(`expected.json has no fixture ${id}`);
  return found;
}

describe("parseFindings", () => {
  test("reads a FINDING line of each form: the - subject, a subject of two words, the terms node", () => {
    expect(
      parseFindings(
        [
          "FINDING principle lease-safety -: It states an aim.",
          "FINDING repeated-meaning terms work item: Its definition gives the meaning of issue.",
          "FINDING form-limit prose pull-request: The paragraph names a pull request as 见#12.",
        ].join("\n")
      )
    ).toEqual({
      verdict: "findings",
      findings: [
        { kind: "principle", node: "lease-safety", subject: null, sentence: "It states an aim." },
        {
          kind: "repeated-meaning",
          node: "terms",
          subject: "work item",
          sentence: "Its definition gives the meaning of issue.",
        },
        {
          kind: "form-limit",
          node: "prose",
          subject: "pull-request",
          sentence: "The paragraph names a pull request as 见#12.",
        },
      ],
    });
  });

  test("reads NO FINDING as a review that found nothing", () => {
    expect(parseFindings("NO FINDING\n")).toEqual({ verdict: "none" });
  });

  test("reads a NOT REVIEWED line alone as no verdict", () => {
    expect(
      parseFindings(
        "NOT REVIEWED: dispatch spec-form --issue ACME-3 --artifact spec.md failed: 404"
      )
    ).toEqual({
      verdict: "not-reviewed",
      reason: "dispatch spec-form --issue ACME-3 --artifact spec.md failed: 404",
    });
  });

  // The skill says to print the NOT REVIEWED line "and stop".
  test("refuses a NOT REVIEWED line beside another line", () => {
    expect(parseFindings("NO FINDING\nNOT REVIEWED: dispatch spec-form failed: 404").verdict).toBe(
      "unreadable"
    );
  });

  test("refuses a verdict's word after the reason of a NOT REVIEWED line", () => {
    expect(parseFindings("NOT REVIEWED: dispatch spec-form failed: 404   NO FINDING").verdict).toBe(
      "unreadable"
    );
  });

  test("refuses a line out of the form, as a kind the skill does not name", () => {
    expect(parseFindings("FINDING sentences inbox-simple -: It has 3 sentences.").verdict).toBe(
      "unreadable"
    );
  });

  test("refuses a verdict's word after the opening of a FINDING line", () => {
    expect(
      parseFindings(
        "FINDING undefined-noun manifest-daily manifest: Nothing defines it. NO FINDING"
      ).verdict
    ).toBe("unreadable");
  });

  test("refuses NO FINDING beside a FINDING line", () => {
    expect(
      parseFindings("NO FINDING\nFINDING principle inbox-simple -: It states a quality.").verdict
    ).toBe("unreadable");
  });

  test("refuses an answer with no FINDING and no NO FINDING line", () => {
    expect(parseFindings("").verdict).toBe("unreadable");
  });
});

describe("compareFindings", () => {
  test("passes a run that names exactly the planted items, a subject without its case and its quotes", () => {
    const answer = parseFindings(
      "FINDING undefined-noun daily-digest `Digest`: Nothing defines it."
    );
    expect(compareFindings(fixture("2-undefined-noun"), answer)).toEqual({
      pass: true,
      problems: [],
    });
  });

  test("fails a run that misses a planted item", () => {
    expect(compareFindings(fixture("1-principle"), { verdict: "none" }).problems).toEqual([
      "missed the planted principle inbox-simple",
    ]);
  });

  // The two controls of INFRA-3977: a reviewer that names a specific with its limit and its unit,
  // or a noun that the list defines, fails the run.
  test("fails a run that names fixture 1's specific, or fixture 3's noun, which its list defines", () => {
    const specific = parseFindings(
      "FINDING principle inbox-simple -: It states a quality.\nFINDING principle inbox-page-limit -: It has a limit."
    );
    expect(compareFindings(fixture("1-principle"), specific).problems).toEqual([
      "named inbox-page-limit, which it must not name (principle)",
    ]);
    const defined = parseFindings(
      "FINDING undefined-noun daily-digest digest: Nothing defines digest."
    );
    expect(compareFindings(fixture("3-defined-noun"), defined).problems).toEqual([
      "named daily-digest digest, which it must not name (undefined-noun)",
    ]);
  });

  test("fails a run that names an item that is neither planted nor listed", () => {
    const answer = parseFindings(
      "FINDING principle inbox-simple -: It states a quality.\nFINDING form-limit prose person: It names a person."
    );
    expect(compareFindings(fixture("1-principle"), answer).problems).toEqual([
      "named form-limit prose person, which is not planted",
    ]);
  });

  test("never passes an answer that is no verdict, even where nothing is planted", () => {
    const three = fixture("3-defined-noun");
    expect(
      compareFindings(three, parseFindings("NOT REVIEWED: dispatch spec-form failed: 404")).pass
    ).toBe(false);
    expect(compareFindings(three, parseFindings("All good.")).pass).toBe(false);
  });
});

const event = (type: string, toolName: string, part: Record<string, unknown>): string =>
  JSON.stringify({ type, toolName, ...part });

describe("specFormCalled", () => {
  test("finds a bash call that runs dispatch spec-form, and none where the agent ran another command", () => {
    const call = (command: string): string =>
      event("tool_execution_start", "bash", { args: { command } });
    expect(specFormCalled(call("dispatch spec-form --issue ACME-3 --artifact spec.md"))).toBe(true);
    expect(specFormCalled(`${call("dispatch doc-read --issue ACME-3")}\nnot json`)).toBe(false);
  });
});

describe("evaluationNamed", () => {
  test("names a call and a result that name this command's files, and nothing of a run that reads only Dispatch", () => {
    const honest = [
      event("tool_execution_start", "read", { args: { path: "/review/SKILL.md" } }),
      event("tool_execution_start", "bash", {
        args: { command: "dispatch spec-form --issue ACME-4 --artifact spec.md" },
      }),
      event("tool_execution_end", "bash", {
        result: { content: [{ type: "text", text: "statement inbox-simple" }] },
      }),
    ];
    expect(evaluationNamed(honest.join("\n"))).toEqual([]);
    const searched = [
      event("tool_execution_start", "bash", { args: { command: "grep -rl inbox-simple /" } }),
      event("tool_execution_end", "bash", {
        result: {
          content: [{ type: "text", text: "/srv/legion/scripts/spec-review-eval/expected.json" }],
        },
      }),
      event("tool_execution_start", "read", { args: { path: "/srv/out/run-1/a/events.jsonl" } }),
    ];
    expect(evaluationNamed(searched.join("\n"))).toEqual([
      "bash result names spec-review-eval",
      "bash result names expected.json",
      "read call names events.jsonl",
    ]);
  });
});
