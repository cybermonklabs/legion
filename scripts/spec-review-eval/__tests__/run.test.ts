import { afterAll, beforeAll, describe, expect, test } from "bun:test";
import { chmod, mkdir, mkdtemp, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import {
  type Agent,
  type Answer,
  blockIds,
  compareFindings,
  dispatchShim,
  environmentOf,
  type Fixture,
  type FormRead,
  formProblems,
  handlesOf,
  KINDS,
  layoutOf,
  leaks,
  type Places,
  parseFindings,
  placesOf,
  type Root,
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

  // The skill says to print the NOT REVIEWED line "and stop", so an answer that holds it and any
  // other line holds two verdicts, or a verdict and prose, and is no answer of the skill's form.
  test("refuses a NOT REVIEWED line beside a finding, beside NO FINDING, or beside prose", () => {
    const failed = "NOT REVIEWED: dispatch spec-form --issue ACME-3 --artifact spec.md failed: 404";
    for (const other of [
      "FINDING principle inbox-simple -: It states a quality.",
      "NO FINDING",
      "I could not reach Dispatch, but the request looks fine.",
    ]) {
      expect(parseFindings(`${other}\n${failed}`).verdict, other).toBe("unreadable");
      expect(parseFindings(`${failed}\n${other}`).verdict, other).toBe("unreadable");
    }
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

  // Step 6 of the skill says "no other text" for both answers, so a line out of the form makes
  // the answer no clean verdict: a reader that skipped such a line would pass what the skill
  // refuses.
  test("refuses prose before NO FINDING", () => {
    expect(parseFindings("I read each statement and each check.\nNO FINDING").verdict).toBe(
      "unreadable"
    );
  });

  test("refuses prose beside a FINDING line", () => {
    expect(
      parseFindings("FINDING principle inbox-simple -: It states a quality.\nThat is all.").verdict
    ).toBe("unreadable");
  });

  // The answer of the skill's form holds one verdict, and each of its lines is whole: a FINDING
  // line holds no other verdict, and a NOT REVIEWED line holds no verdict after its reason. A
  // reader that tests each line by its opening alone takes the first verdict of a line that holds
  // two, so the second hides in the free text of the first.
  test("refuses a line that holds a second verdict after its opening", () => {
    const finding = "FINDING undefined-noun manifest-daily manifest: Nothing defines manifest.";
    const failed =
      "NOT REVIEWED: dispatch spec-form --issue ACME-3 --artifact spec.md failed: ECONNREFUSED";
    for (const line of [
      `${finding}   NO FINDING`,
      `${finding}   ${failed}`,
      `${finding} FINDING principle inbox-simple -: It states a quality.`,
      `${failed}   NO FINDING`,
      `${failed}   ${finding}`,
      `NO FINDING   ${finding}`,
      `NO FINDING   ${failed}`,
    ]) {
      expect(parseFindings(line).verdict, line).toBe("unreadable");
    }
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

  test("never passes an answer that holds only a NOT REVIEWED line where a fixture expects a review, even with nothing planted", () => {
    const answer = parseFindings(
      "NOT REVIEWED: dispatch spec-form --issue ACME-4 --artifact spec.md --terms failed: unknown flag"
    );
    const reviewed = plan.fixtures.filter((one) => one.expect === "review");
    expect(reviewed.length).toBe(plan.fixtures.length - 1);
    for (const one of reviewed) {
      const result = compareFindings(one, answer);
      expect(result.pass).toBe(false);
      expect(result.problems).toEqual([
        "gave no verdict: dispatch spec-form --issue ACME-4 --artifact spec.md --terms failed: unknown flag",
      ]);
    }
  });

  // Fixture 11 declares that the reviewer cannot see: its agent's dispatch reaches no server. The
  // skill's answer then is NOT REVIEWED with the failed command, and NO FINDING or a finding would
  // tell the author a false thing.
  describe("where the fixture declares that the reviewer cannot see", () => {
    const blind = fixture("11-command-fails");

    test("passes a NOT REVIEWED line that names the failed dispatch spec-form", () => {
      const answer = parseFindings(
        "NOT REVIEWED: dispatch spec-form --issue ACME-14 --artifact spec.md failed: Dispatch at http://127.0.0.1:1 is unreachable: ECONNREFUSED"
      );
      expect(compareFindings(blind, answer)).toEqual({ pass: true, problems: [] });
    });

    test("fails NO FINDING, a finding, and a NOT REVIEWED line that names no failed dispatch spec-form", () => {
      expect(compareFindings(blind, { verdict: "none" }).problems).toEqual([
        "answered NO FINDING, but its dispatch reaches no server, so it could not see the request",
      ]);
      expect(
        compareFindings(
          blind,
          parseFindings("FINDING undefined-noun daily-digest digest: Nothing defines digest.")
        ).problems
      ).toEqual([
        "named findings, but its dispatch reaches no server, so it could not see the request",
      ]);
      expect(
        compareFindings(blind, parseFindings("NOT REVIEWED: the request is too long")).problems
      ).toEqual(["gave NOT REVIEWED with no failed dispatch spec-form: the request is too long"]);
      expect(compareFindings(blind, parseFindings("All good.")).pass).toBe(false);
    });
  });

  test("never passes an unreadable answer", () => {
    const answer: Answer = parseFindings("All good.");
    const result = compareFindings(fixture("3-defined-noun"), answer);
    expect(result.pass).toBe(false);
    expect(result.problems[0]).toStartWith("gave an unreadable answer");
  });

  test("fails one line of two verdicts where either verdict alone passes, on each kind of fixture", () => {
    const digest =
      "FINDING undefined-noun daily-digest digest: Neither the list nor the terms of the request define digest.";
    const failed =
      "NOT REVIEWED: dispatch spec-form --issue ACME-14 --artifact spec.md failed: Dispatch at http://127.0.0.1:1 is unreachable: ECONNREFUSED";
    const cases: [string, string, string][] = [
      ["2-undefined-noun", digest, "NO FINDING"],
      ["2-undefined-noun", digest, failed],
      ["3-defined-noun", "NO FINDING", failed],
      ["11-command-fails", failed, "NO FINDING"],
      ["11-command-fails", failed, digest],
    ];
    for (const [id, alone, second] of cases) {
      expect(compareFindings(fixture(id), parseFindings(alone)).pass, `${id}: ${alone}`).toBe(true);
      const both = `${alone}   ${second}`;
      expect(compareFindings(fixture(id), parseFindings(both)).pass, `${id}: ${both}`).toBe(false);
    }
  });

  // Fixture 9 breaks the form on purpose. Dispatch decided those texts already, so a finding the
  // reviewer writes at one of them fails the run, whatever its kind.
  test("passes fixture 9 when the answer names its planted noun and no text the form check found", () => {
    const answer = parseFindings(
      "FINDING undefined-noun voucher-print voucher: Neither the list nor the terms of the request define voucher."
    );
    expect(compareFindings(fixture("9-decided-findings"), answer)).toEqual({
      pass: true,
      problems: [],
    });
  });

  test("fails a run that names a text the form check found", () => {
    const answer = parseFindings(
      [
        "FINDING principle night-quiet -: It has three sentences.",
        "FINDING undefined-noun voucher-print voucher: Nothing defines voucher.",
        "FINDING repeated-meaning terms parcel: The list defines parcel already.",
      ].join("\n")
    );
    const result = compareFindings(fixture("9-decided-findings"), answer);
    expect(result.pass).toBe(false);
    expect(result.problems).toEqual([
      "named principle night-quiet, where the form check finds statement night-quiet",
      "named repeated-meaning terms parcel, where the form check finds definition parcel",
    ]);
  });
});

// The form check reads a statement in two forms: a requirement block, and the entry line of the
// how-to of The Spec. A run judges the reviewer at each node, so the fixtures' nodes are read in
// both forms too.
describe("blockIds", () => {
  test("reads the id of a requirement block, a check block and an entry line", () => {
    const markdown = [
      ":::requirement{#door-closes}",
      "The door closes after 60 seconds.",
      ":::",
      '::::check{#v-door-closes serves="dispatch://ACME-1/spec#b-door-closes"}',
      "The door closes.",
      "::::",
      "- **The kiosk prints one receipt.** `kiosk-receipt` · under: `kiosk` · check: none",
      "* **The kiosk beeps once.** `kiosk-beep` · under: `kiosk` · check: none",
      "- **A term**: not a statement.",
    ].join("\n");
    expect(blockIds(markdown)).toEqual([
      "door-closes",
      "v-door-closes",
      "kiosk-receipt",
      "kiosk-beep",
    ]);
  });
});

// The form check reads a term of the request only on a term line under a heading Terms, outside
// a code fence and outside a requirement or a check block: a line in that form inside such a
// block is a second definition at most, and adds no term. A line inside another typed block, such
// as an ask, is read as any other line.
describe("termNames", () => {
  test("reads no term line inside a requirement or a check block, nor in a code fence", () => {
    const markdown = [
      "## Terms",
      "",
      "- **chime**: A short sound that the kiosk makes for each key.",
      ":::requirement{#kiosk-chime}",
      "- **beep**: A sound of the kiosk.",
      ":::",
      '::::check{#v-chime serves="dispatch://ACME-1/spec#b-kiosk-chime"}',
      "- **tone**: A sound of the kiosk.",
      "::::",
      "```text",
      "- **buzz**: A sound of the kiosk.",
      "```",
      ':::ask{#ask-1 state="open"}',
      "- **ring**: A sound of the kiosk.",
      ":::",
    ].join("\n");
    expect(termNames(markdown)).toEqual(["chime", "ring"]);
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

// Before any agent runs, the command reads the form check of each seeded document. A fixture that
// declares no finding must get none, and fixture 9 must get exactly the two it declares, so the
// agent meets the findings that the skill tells it to leave alone.
describe("formProblems", () => {
  const nine = fixture("9-decided-findings");
  const read = (findings: [string, string][], inScope = true): FormRead => ({
    in_scope: inScope,
    scope: inScope ? "covered" : "no_root",
    findings: findings.map(([kind, node]) => ({ kind, node })),
  });

  test("passes a spec whose form check gives exactly the findings that its fixture declares", () => {
    const found = read([
      ["definition", "parcel"],
      ["statement", "night-quiet"],
    ]);
    expect(formProblems(nine, "spec.md", found)).toEqual([]);
  });

  test("fails a spec where the form check gives a declared finding no more, or one more", () => {
    const found = read([
      ["statement", "night-quiet"],
      ["claim", "v-other"],
    ]);
    expect(formProblems(nine, "spec.md", found)).toEqual([
      "the form check finds no definition parcel in 9-decided-findings spec.md, which the fixture declares",
      "the form check finds claim v-other in 9-decided-findings spec.md, which the fixture does not declare",
    ]);
  });

  test("fails a document of a fixture that declares nothing, when the form check finds a text", () => {
    expect(
      formProblems(fixture("1-principle"), "spec.md", read([["statement", "inbox-simple"]]))
    ).toEqual([
      "the form check finds statement inbox-simple in 1-principle spec.md, which the fixture does not declare",
    ]);
    expect(formProblems(nine, "verification.md", read([]))).toEqual([]);
  });

  test("fails a document out of scope, because DISPATCH_SPEC_ROOTS did not reach the server", () => {
    expect(formProblems(fixture("1-principle"), "spec.md", read([], false))).toEqual([
      "1-principle spec.md is out of scope (no_root), so DISPATCH_SPEC_ROOTS did not reach the server",
    ]);
  });
});

describe("the fixtures", () => {
  test("hold fifteen change requests under the three roots", () => {
    expect(plan.fixtures.map((one) => one.id)).toEqual([
      "1-principle",
      "2-undefined-noun",
      "3-defined-noun",
      "4-other-meaning",
      "5-repeated-meaning",
      "6-split-checks",
      "7-check-faults",
      "8-check-shape-passing",
      "9-decided-findings",
      "10-entry-lines",
      "11-command-fails",
      "12-common-nouns",
      "13-markup-heading",
      "14-quoted-blocks",
      "15-code-fence",
    ]);
    expect(plan.roots.map((root) => root.key)).toEqual(["ACME-1", "ACME-2", "ACME-3"]);
  });

  test("fixture 11 alone declares that its reviewer cannot see, on fixture 2's request with nothing planted", () => {
    const blind = fixture("11-command-fails");
    expect(blind.expect).toBe("not-reviewed");
    expect(blind.specText).toBe(fixture("2-undefined-noun").specText);
    expect(blind.planted).toEqual([]);
    expect(plan.fixtures.filter((one) => one.expect !== "review").map((one) => one.id)).toEqual([
      "11-command-fails",
    ]);
  });

  // The control of the noun test of the skill's step 4: beside the terms of the list, each
  // statement names only a unit of time in its usual meaning, a noun that the system does not
  // make, keep, send or show. A reviewer that takes such a noun for a noun of the system fails
  // the run. A word for a thing that the system shows, keeps or sends, such as page, file or
  // message, is not in it: step 4 can read it either way.
  test("fixture 12 plants nothing, names only units of time beside its terms, and each of its statements must not be named", () => {
    const twelve = fixture("12-common-nouns");
    expect(twelve.planted).toEqual([]);
    expect(twelve.mustNotName.map((item) => item.node)).toEqual(blockIds(twelve.specText));
    for (const noun of ["hours", "minutes", "days", "seconds"]) {
      expect(twelve.specText, noun).toContain(` ${noun}`);
    }
    for (const noun of ["page", "file", "database", "list", "message"]) {
      expect(twelve.specText, noun).not.toContain(noun);
    }
  });

  // Fixture 3 is fixture 2's request under a list that defines its noun. The list's definition
  // holds the same asks as the statement, so the statement uses the term with the meaning of its
  // definition, and no reader can take the use for a narrower one.
  test("fixture 3's list defines digest with the asks that its statement names", () => {
    const asks = "each ask that waited more than 24 hours";
    expect(fixture("3-defined-noun").specText).toContain(asks);
    expect(rootTerms(fixture("3-defined-noun").root)).toContain(
      `- **digest**: One message that Dispatch sends the person each day, with ${asks}.`
    );
  });

  // A claim is narrower than its statement (R6). The statement of v-kiosk-memory writes the last
  // 100 pickup codes, so its claim holds those and no other code.
  test("the claim of v-kiosk-memory holds the last 100 pickup codes, as its statement does", () => {
    for (const id of ["7-check-faults", "8-check-shape-passing"]) {
      const text = fixture(id).specText;
      expect(blockText(text, "kiosk-memory"), id).toContain("the last 100 pickup codes");
      expect(blockText(text, "v-kiosk-memory"), id).toContain(
        "The kiosk writes the last 100 pickup codes"
      );
    }
  });

  // The list defines the kiosk as the screen and the keypad, which print nothing, so fixture 9
  // gives the printing to the locker.
  test("fixture 9's voucher statement gives the printing to the locker, not to the kiosk", () => {
    const statement = blockText(fixture("9-decided-findings").specText, "voucher-print");
    expect(statement).toContain("The locker prints one voucher");
    expect(statement).not.toContain("kiosk");
  });

  // The Spec's own documents write each statement as an entry line, and its how-to puts the terms
  // under a heading Terms of any level. Fixture 10 is fixture 1 in that form, with a term of the
  // request under a heading of level three, so a reviewer that reads requirement blocks alone
  // misses its principle, and one that reads `## Terms` alone names its own term.
  test("fixture 10 is fixture 1 as entry lines, with a term of its own under ### Terms", () => {
    const ten = fixture("10-entry-lines");
    expect(ten.specText).not.toContain(":::requirement");
    expect(blockIds(ten.specText)).toEqual(["inbox-simple", "inbox-overdue-first"]);
    expect(ten.specText).toMatch(/^### Terms$/m);
    expect(termNames(ten.specText)).toEqual(["overdue ask"]);
    expect(ten.specText).toContain(
      "- **The inbox is simple, and the person always knows what to do next.** `inbox-simple`"
    );
    expect(ten.planted).toEqual(fixture("1-principle").planted);
  });

  // The form check takes a heading for the heading Terms only when its text is Terms alone, and it
  // reads no line of a quote. Fixture 13 defines its noun only under `## **Terms**`, a heading that
  // reads as Terms in another form: the form check finds it (terms-heading), and it opens no
  // terms, so to the program the noun has no definition. Fixture 14 quotes a principle as a
  // requirement block and as an entry line, so to the program neither is a statement.
  test("fixture 13 defines its noun only under a heading **Terms**, which the form check finds and which opens no terms", () => {
    const thirteen = fixture("13-markup-heading");
    expect(thirteen.specText).toMatch(/^## \*\*Terms\*\*$/m);
    expect(thirteen.specText).toContain("- **door alarm**: ");
    expect(termNames(thirteen.specText)).toEqual([]);
    expect(thirteen.form).toEqual([{ kind: "heading", node: '"## **Terms**"' }]);
    expect(thirteen.planted).toEqual([
      { kind: "undefined-noun", node: "door-alarm-start", subject: "door alarm" },
    ]);
    expect(termNames(rootTerms(thirteen.root))).not.toContain("door alarm");
  });

  test("fixture 14 quotes a requirement block and an entry line, which are no statements", () => {
    const fourteen = fixture("14-quoted-blocks");
    expect(fourteen.specText).toMatch(/^> :::requirement\{#quoted-calm\}$/m);
    expect(fourteen.specText).toMatch(/^> - \*\*.+\*\* `quoted-entry` · under:/m);
    expect(blockIds(fourteen.specText)).toEqual(["door-close-60"]);
    expect(fourteen.planted).toEqual([]);
    expect(fourteen.mustNotName.map((item) => item.node)).toEqual([
      "door-close-60",
      "quoted-calm",
      "quoted-entry",
    ]);
  });

  // The form check reads nothing in a code fence, so fixture 15's definition of its noun, in a
  // fence under `## Terms`, defines nothing to the program, and the noun has no definition.
  test("fixture 15 defines its noun only in a code fence under its heading Terms", () => {
    const fifteen = fixture("15-code-fence");
    expect(fifteen.specText).toMatch(/^## Terms\n\n```text\n- \*\*door alarm\*\*: .+\n```$/m);
    expect(termNames(fifteen.specText)).toEqual([]);
    expect(fifteen.planted).toEqual([
      { kind: "undefined-noun", node: "door-alarm-start", subject: "door alarm" },
    ]);
    expect(termNames(rootTerms(fifteen.root))).not.toContain("door alarm");
  });

  test("plant each of the seven kinds of the skill in some fixture", () => {
    const planted = new Set(plan.fixtures.flatMap((one) => one.planted.map((item) => item.kind)));
    expect([...planted].sort()).toEqual([...KINDS].sort());
  });

  test("hold one change request that plants the three faults of a check's shape together", () => {
    const three = ["wide-claim", "agent-choice", "agent-where-program"];
    const holders = plan.fixtures.filter((one) =>
      three.every((kind) => one.planted.some((item) => item.kind === kind))
    );
    expect(holders.map((one) => one.id)).toEqual(["7-check-faults"]);
  });

  test("fixture 8 is fixture 7 without its three planted faults", () => {
    const seven = fixture("7-check-faults");
    const eight = fixture("8-check-shape-passing");
    const planted = seven.planted.map((item) => item.node);
    const kept = blockIds(seven.specText).filter((id) => !planted.includes(id));
    expect(blockIds(eight.specText)).toEqual(kept);
    for (const id of kept) {
      expect(blockText(eight.specText, id), id).toBe(blockText(seven.specText, id));
    }
    expect(eight.planted).toEqual([]);
  });

  test("fixtures 9 and 13 declare the findings of the form check, and no other fixture declares one", () => {
    expect(fixture("9-decided-findings").form).toEqual([
      { kind: "statement", node: "night-quiet" },
      { kind: "definition", node: "parcel" },
    ]);
    const declaring = ["9-decided-findings", "13-markup-heading"];
    expect(plan.fixtures.filter((one) => one.form.length > 0).map((one) => one.id)).toEqual(
      declaring
    );
  });

  // The tester's scenario 7: a clean request, then an agent's edit that breaks the form and
  // inserts a second term line above the request's own one. Dispatch stores that Terms part as
  // two lists, the second with *, which the form check reads as a term line. So a reviewer whose
  // skill reads - lines alone can name the request's own term, and the run fails.
  test("fixture 9 is a clean request that an agent's edit breaks, with a term line inserted above its own", () => {
    const nine = fixture("9-decided-findings");
    expect(termNames(nine.specText)).toEqual(["quiet hours"]);
    expect(nine.editOps).toEqual([
      {
        op: "replace",
        find: "The locker sends no notice to a tenant in the quiet hours, and it sends each such notice at 07:00.",
        with: "The locker sends no notice to a tenant in the quiet hours. It keeps each such notice. It sends the notice at 07:00.",
      },
      {
        op: "insert",
        after: "heading:Terms",
        markdown: "- **parcel**: A package that a courier puts in a compartment for one tenant.",
      },
    ]);
    expect(blockText(nine.specText, "night-quiet")).toContain(nine.editOps[0]?.find ?? "-");
    for (const other of plan.fixtures.filter((one) => one.id !== "9-decided-findings")) {
      expect(other.editOps, other.id).toEqual([]);
    }
  });

  test("each planted, listed and form node is in its fixture's text", () => {
    for (const one of plan.fixtures) {
      const ids = [...blockIds(one.specText), ...blockIds(one.verificationText ?? ""), "terms"];
      for (const item of one.planted) {
        expect(ids, `${one.id}: ${item.node}`).toContain(item.node);
      }
      // A block in a quote is no statement to the form check, and fixture 14 lists two such
      // nodes that the reviewer must not name, so a listed node may be a quoted one.
      const quoted = blockIds(one.specText.replace(/^\s*>\s?/gm, ""));
      for (const item of one.mustNotName) {
        expect([...ids, ...quoted], `${one.id}: ${item.node}`).toContain(item.node);
      }
      const edited = [one.specText, ...one.editOps.map((op) => op.markdown ?? op.with ?? "")];
      for (const finding of one.form) {
        if (finding.kind === "definition") {
          expect(edited.join("\n"), `${one.id}: definition`).toContain(`**${finding.node}**: `);
        } else if (finding.kind === "heading") {
          // The form check names a heading by its line in quotes.
          const line = JSON.parse(finding.node) as string;
          expect(one.specText.split("\n"), `${one.id}: heading`).toContain(line);
        } else {
          expect(ids, `${one.id}: ${finding.kind} ${finding.node}`).toContain(finding.node);
        }
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
    const six = fixture("6-split-checks");
    expect(six.specText).not.toContain(":::check");
    expect(blockIds(six.verificationText ?? "")).toContain("v-second-answer-agent");
  });

  test("a Verification document gets the key of its own issue in each serves", () => {
    const text = withIssueKey(fixture("6-split-checks").verificationText ?? "", "ACME-9");
    expect(text).not.toContain("@ISSUE@");
    expect(text).toContain('serves="dispatch://ACME-9/spec#b-second-answer-refused"');
  });
});

// A fixture's agent reads the change request's own text, and nothing else that tells its answer.
// In each run each fixture gets an opaque handle, its place in a shuffle that the run's seed
// decides, and the handle, never the fixture's id, is in what its agent reads. The command refuses
// a run where a place that an agent reads holds a word of its fixture's id, of a kind that the
// fixture plants or of its kind, or its root's own name in expected.json.
describe("what the agent of a fixture reads", () => {
  const ids = plan.fixtures.map((one) => one.id);
  const AGENTS = "/tmp/x0AbCdE";
  const SKILL_PATH = "/repo/skills/spec-statement-review/SKILL.md";
  const STACK = {
    url: "http://127.0.0.1:40000",
    token: "dispatch-stack-6fa459ea-ee8a-3ca4-894e-db77e160355e",
  };
  const SHIM = dispatchShim("/usr/local/bin/bun", "/repo/packages/envoy-client/bin/dispatch.ts");

  /** Each agent of a run of the seed with what it reads, as the command makes both. */
  function readsIn(seed: number): [Agent, Places][] {
    const agents = layoutOf(plan, plan.fixtures, seed, AGENTS, SKILL_PATH);
    const beside: Record<string, string> = {
      ...Object.fromEntries(agents.map((one) => [`../${one.handle}`, ""])),
      bin: "",
      "bin/dispatch": SHIM,
      "dispatch-state": "",
      home: "",
      "home/.omp": "",
      work: "",
    };
    return agents.map((agent) => [
      agent,
      placesOf(agent, plan, environmentOf(agent, STACK, {}), beside, "main"),
    ]);
  }

  function readsOf(id: string): [Fixture, Root, Places] {
    const found = readsIn(1).find(([agent]) => agent.fixture.id === id);
    if (found === undefined) throw new Error(`no agent of ${id}`);
    const root = plan.roots.find((one) => one.name === found[0].fixture.root);
    if (root === undefined) throw new Error(`no root of ${id}`);
    return [found[0].fixture, root, found[1]];
  }

  test("gives handles 1 to N in a shuffle that the seed decides, and other seeds give other shuffles", () => {
    const first = handlesOf(ids, 1);
    expect([...first.keys()].sort()).toEqual([...ids].sort());
    expect([...first.values()].sort((a, b) => a - b)).toEqual(ids.map((_, at) => at + 1));
    expect(handlesOf(ids, 1)).toEqual(first);
    const orders = new Set(
      [1, 2, 3, 4, 5].map((seed) => ids.map((id) => handlesOf(ids, seed).get(id)).join(","))
    );
    expect(orders.size).toBeGreaterThan(1);
  });

  test("puts each agent's handle, never its fixture's id, in its issue's key, its title, its directory and its prompt", () => {
    const agents = layoutOf(plan, plan.fixtures, 4, AGENTS, SKILL_PATH);
    expect(agents.map((one) => one.handle)).toEqual(ids.map((_, at) => at + 1));
    for (const agent of agents) {
      expect(agent.issue).toBe(`ACME-${plan.roots.length + agent.handle}`);
      expect(agent.title).toContain(` ${agent.handle} `);
      expect(agent.work).toBe(`${AGENTS}/${agent.handle}/work`);
      expect(agent.prompt).toContain(`issue ${agent.issue},`);
      for (const text of [agent.title, agent.directory, agent.prompt]) {
        expect(text).not.toContain(agent.fixture.id);
      }
    }
  });

  test("names no fixture's answer in anything that the command writes for its agent", () => {
    for (const seed of [1, 2, 3]) {
      for (const [agent, places] of readsIn(seed)) {
        const root = plan.roots.find((one) => one.name === agent.fixture.root);
        if (root === undefined) throw new Error(`no root of ${agent.fixture.id}`);
        expect(leaks(agent.fixture, root, places), agent.fixture.id).toEqual([]);
      }
    }
  });

  test("refuses a word of the fixture's id in each place, one place at a time, and names that place", () => {
    const [two, root, clean] = readsOf("2-undefined-noun");
    expect(leaks(two, root, clean)).toEqual([]);
    const planted: [string, Places][] = [
      [`its issue's title holds "noun"`, { ...clean, title: `${clean.title} and a noun` }],
      [`its root's title holds "noun"`, { ...clean, rootTitle: "Nouns of The Spec" }],
      [
        `the name of the document nouns.md holds "noun"`,
        { ...clean, documents: [...clean.documents, "nouns.md"] },
      ],
      [
        `its working directory holds "undefined"`,
        { ...clean, workingDirectory: "/tmp/undefined/2/work" },
      ],
      [
        `the variable TMPDIR holds "noun"`,
        { ...clean, environment: { ...clean.environment, TMPDIR: "/tmp/nouns" } },
      ],
      [
        `the file bin/noun beside its working directory holds "noun"`,
        { ...clean, filesBeside: { ...clean.filesBeside, "bin/noun": "" } },
      ],
      [`the branch of the checkout holds "undefined"`, { ...clean, branch: "fix/undefined-word" }],
      [`its prompt holds "noun"`, { ...clean, prompt: `${clean.prompt} Name each noun.` }],
    ];
    for (const [problem, places] of planted) {
      expect(leaks(two, root, places), problem).toEqual([`2-undefined-noun: ${problem}`]);
    }
  });

  test("refuses a word of a planted kind, of the fixture's kind, a word that starts with one, and the root's own name", () => {
    const [one, rootOne, placesOne] = readsOf("1-principle");
    expect(leaks(one, rootOne, { ...placesOne, title: "The principles of the inbox" })).toEqual([
      `1-principle: its issue's title holds "principle"`,
    ]);
    const [seven, rootSeven, placesSeven] = readsOf("7-check-faults");
    expect(
      leaks(seven, rootSeven, { ...placesSeven, prompt: `${placesSeven.prompt} Agents wrote it.` })
    ).toEqual([`7-check-faults: its prompt holds "agent"`]);
    const [eleven, rootEleven, placesEleven] = readsOf("11-command-fails");
    expect(leaks(eleven, rootEleven, { ...placesEleven, title: "Reviewed requests" })).toEqual([
      `11-command-fails: its issue's title holds "reviewed"`,
    ]);
    const [two, rootTwo, placesTwo] = readsOf("2-undefined-noun");
    expect(leaks(two, rootTwo, { ...placesTwo, rootTitle: rootTwo.title })).toEqual([
      "2-undefined-noun: its root's title holds the name of its root in expected.json",
    ]);
    // Every prompt names the task, review, so `review`, the kind of every fixture but one, tells
    // no answer; and a word that holds a telling word only inside it, as pronoun holds noun, is
    // another word.
    expect(leaks(two, rootTwo, { ...placesTwo, title: "Review the pronouns" })).toEqual([]);
  });

  test("names a variable that holds a telling word, and never prints its value", () => {
    const [two, root, clean] = readsOf("2-undefined-noun");
    const problems = leaks(two, root, {
      ...clean,
      environment: { ...clean.environment, TMPDIR: "/srv/scratch/noun-place-0001" },
    });
    expect(problems).toEqual([`2-undefined-noun: the variable TMPDIR holds "noun"`]);
    expect(problems.join("\n")).not.toContain("noun-place-0001");
  });

  test("gives the agent a PATH of its own dispatch and the system's directories, and none of the caller's", () => {
    const [agent] = layoutOf(plan, plan.fixtures, 1, AGENTS, SKILL_PATH);
    if (agent === undefined) throw new Error("no agent");
    const environment = environmentOf(agent, STACK, { PATH: "/home/someone/agent-tools/bin" });
    expect(environment.PATH).toBe(`${agent.bin}:/usr/local/bin:/usr/bin:/bin`);
  });
});

// The header of run.ts says 2 when the command cannot run, and 1 only when a review failed. Each
// case below stops before any agent runs, so a 1 would tell the reader that a reviewer failed.
describe("the exit status of a command that cannot run", () => {
  const RUN = join(import.meta.dir, "..", "run.ts");
  const CLOSED_PORT = "postgres://127.0.0.1:1/postgres?sslmode=disable";
  let scratch = "";
  let stub = "";

  beforeAll(async () => {
    scratch = await mkdtemp(join(tmpdir(), "spec-review-eval-exit-"));
    stub = join(scratch, "omp");
    await writeFile(stub, "#!/bin/sh\necho /nonexistent-omp-config\n", "utf8");
    await chmod(stub, 0o755);
  });
  afterAll(async () => {
    await rm(scratch, { recursive: true, force: true });
  });

  async function command(
    database: string | undefined,
    out: string,
    extra: Record<string, string> = {}
  ): Promise<{ code: number; stderr: string }> {
    const env: Record<string, string> = {};
    for (const [name, value] of Object.entries(process.env)) {
      if (value !== undefined && name !== "DISPATCH_TEST_DATABASE_URL") env[name] = value;
    }
    if (database !== undefined) env.DISPATCH_TEST_DATABASE_URL = database;
    const child = Bun.spawn({
      cmd: [process.execPath, RUN, "--omp", stub, "--out", out],
      env: { ...env, ...extra },
      stdin: "ignore",
      stdout: "ignore",
      stderr: "pipe",
    });
    const [stderr, code] = await Promise.all([new Response(child.stderr).text(), child.exited]);
    return { code, stderr };
  }

  async function emptyOut(name: string): Promise<string> {
    const out = join(scratch, name);
    await mkdir(out);
    return out;
  }

  test("is 2 with no DISPATCH_TEST_DATABASE_URL", async () => {
    const { code, stderr } = await command(undefined, await emptyOut("no-database"));
    expect(stderr).toContain("DISPATCH_TEST_DATABASE_URL");
    expect(code).toBe(2);
  }, 30_000);

  test("is 2 with a database that refuses the connection", async () => {
    const { code, stderr } = await command(CLOSED_PORT, await emptyOut("closed-port"));
    expect(stderr).not.toBe("");
    expect(code).toBe(2);
  }, 30_000);

  test("is 2 with an --out that an earlier run used, and names what it holds", async () => {
    const out = await emptyOut("used");
    await mkdir(join(out, "run-1", "stack", "home"), { recursive: true });
    const { code, stderr } = await command(CLOSED_PORT, out);
    expect(stderr).toContain(`--out ${out} is not empty: it holds run-1;`);
    expect(code).toBe(2);
  }, 30_000);

  test("is 2 with an --out that holds one other file, and names that file", async () => {
    const out = await emptyOut("stray");
    await writeFile(join(out, "notes.txt"), "a note\n", "utf8");
    const { code, stderr } = await command(CLOSED_PORT, out);
    expect(stderr).toContain(`--out ${out} is not empty: it holds notes.txt;`);
    expect(code).toBe(2);
  }, 30_000);

  test("is 2 with an --out that is a file, in the command's own words", async () => {
    const out = join(scratch, "a-file");
    await writeFile(out, "not a directory\n", "utf8");
    const { code, stderr } = await command(CLOSED_PORT, out);
    expect(stderr.trim()).toBe(`--out ${out} is not a directory`);
    expect(code).toBe(2);
  }, 30_000);

  test("is 2 before any stack starts when what an agent reads names its answer, and keeps the value out", async () => {
    const { code, stderr } = await command(CLOSED_PORT, await emptyOut("leak"), {
      ANTHROPIC_BASE_URL: "http://principle.invalid",
    });
    expect(stderr).toStartWith("the command refuses the run");
    expect(stderr).toContain(`1-principle: the variable ANTHROPIC_BASE_URL holds "principle"`);
    expect(stderr).not.toContain("principle.invalid");
    expect(code).toBe(2);
  }, 30_000);
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
