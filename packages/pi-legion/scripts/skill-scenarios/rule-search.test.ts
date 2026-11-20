// The control of rule-search.ts. For each label, a sentence of its rule planted in a text that its
// role reaches through one link, through two links, and through the most links that any text of the
// role needs, makes the search fail, naming that text. The same sentence planted in a text that no
// chain of links reaches leaves the search clean. A checkout with no texts is UNKNOWN, never clean.
// Each planted text's depth is asserted first, so a plant that no longer sits at its depth fails
// instead of passing at another one.
//
// The `cut` mode, at head: the head itself, taken for the label's checkout, fails once for each
// sentence the head's search cuts; a copy with each of those sentences deleted, and nothing else
// changed, is clean; and the label's sentence planted in that copy fails again, naming its text.
//
//   bun test rule-search.test.ts
//
// RULE_SEARCH_CHECKOUT names the checkout (default: the repository this file is in) and
// RULE_SEARCH_TREE its tree, base or head (default: head). Each test works in a copy of the
// checkout's prompts and skills under its own mktemp directory.

import { afterAll, describe, expect, setDefaultTimeout, test } from "bun:test";
import { appendFileSync, cpSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import path from "node:path";
import { RULES } from "./label-rules";
import { chainOf, roleTexts } from "./role-texts";
import { headSentences, search, searchCut } from "./rule-search";
import { fileRowLimit } from "./test-limit";

// Each row and hook copies or removes the checkout's prompts and skills: each test and hook gets
// the limit test-limit.ts computes from the bounds of what they wait for, set before the hook
// below is registered.
setDefaultTimeout(fileRowLimit());

const checkout = process.env.RULE_SEARCH_CHECKOUT ?? path.resolve(import.meta.dir, "../../../..");
const tree = (process.env.RULE_SEARCH_TREE ?? "head") as "base" | "head";
const scratch = mkdtempSync(path.join(tmpdir(), "rule-search-"));
afterAll(() => rmSync(scratch, { recursive: true, force: true }));

let copies = 0;
function copy(): string {
  const dir = path.join(scratch, String(copies++));
  for (const part of ["skills", "packages/daemon/internal/prompts"])
    cpSync(path.join(checkout, part), path.join(dir, part), { recursive: true });
  return dir;
}

// A prompt part of another role: prompts.Compose gives it to neither role, and no link names it.
const UNREAD = "packages/daemon/internal/prompts/roles/implementer.md";

// For each label: a sentence of its rule, and the text it is planted in at each depth. Main's
// skills link no text deeper than two links for any of the three roles (main's
// legion-retro/references/learnings.md no longer names the tester's prompt), so the two-link plant
// is the deepest, and the row below holds it there.
const PLANTS: Record<string, { sentence: string; at: Record<1 | 2, string> }> = {
  "ablate-two-roles": {
    sentence:
      "A check has a pass at a head only when the implementer and the tester each recorded one there.",
    at: {
      1: "skills/legion-worker/references/tools.md",
      2: "skills/legion-controller/references/triage.md",
    },
  },
  "ablate-later-fail": {
    sentence:
      "Where one head holds a pass and a fail of a check, that head is not known, so request changes at once.",
    at: {
      1: "skills/legion-architect/SKILL.md",
      2: "skills/legion-architect/references/wake-routing.md",
    },
  },
  "ablate-architect-later-fail": {
    sentence:
      "A head that holds a pass and a fail of a check has no pass, so send the issue back without waiting.",
    at: {
      1: "skills/legion-retro/SKILL.md",
      2: "skills/legion-worker/references/pr-body.md",
    },
  },
};

describe("rule-search", () => {
  for (const [label, plant] of Object.entries(PLANTS)) {
    const role = RULES[label]?.role;
    if (role === undefined) throw new Error(`no rule for the label ${label}`);

    test(`${label}: the checkout as it is has no fault`, () => {
      const outcome = search(copy(), tree, label);
      expect(outcome.faults).toEqual([]);
      expect(outcome.hits).toBeGreaterThan(0);
    });

    test(`${label}: the deepest plant is as deep as the ${role}'s deepest text`, () => {
      const texts = roleTexts(copy(), role);
      expect(Math.max(...texts.depth.values())).toBe(
        Math.max(...Object.keys(plant.at).map(Number))
      );
    });

    for (const [depth, file] of Object.entries(plant.at)) {
      test(`${label}: a sentence of the rule in ${file}, ${depth} links deep, is a fault naming that text`, () => {
        const dir = copy();
        const texts = roleTexts(dir, role);
        expect({ file, depth: texts.depth.get(file) }).toEqual({ file, depth: Number(depth) });
        appendFileSync(path.join(dir, file), `\n${plant.sentence}\n`);
        const outcome = search(dir, tree, label);
        console.log(
          `planted ${label} at depth ${depth} (${chainOf(texts, file).join(" > ")}): ${outcome.faults.join(" | ")}`
        );
        expect(outcome.faults).toHaveLength(1);
        expect(outcome.faults[0]).toContain(`${file}:`);
        expect(outcome.faults[0]).toContain(`(depth ${depth})`);
        expect(outcome.faults[0]).toContain("no entry classifies it");
      });
    }

    test(`${label}: the same sentence in ${UNREAD}, which no chain of links reaches, is no fault`, () => {
      const dir = copy();
      expect(roleTexts(dir, role).files).not.toContain(UNREAD);
      appendFileSync(path.join(dir, UNREAD), `\n${plant.sentence}\n`);
      expect(search(dir, tree, label).faults).toEqual([]);
    });

    /** The entries of the label that put old words back, whose sentence the head holds. */
    const restoresAt = (head: string) => {
      const cuts = [...headSentences(head, label).cuts.entries()];
      return (RULES[label]?.entries ?? []).filter(
        (entry) =>
          entry.restore !== undefined &&
          cuts.some(
            ([file, list]) =>
              file === entry.file && list.some((s) => s.text.includes(entry.has ?? "\u0000"))
          )
      );
    };
    /** A copy of `head` with each sentence the head's search cuts deleted, or, where its entry
     * restores old words and `restore` is set, replaced by them. */
    const ablate = (head: string, restore: boolean) => {
      const ablated = copy();
      for (const [file, cut] of headSentences(head, label).cuts) {
        let text = readFileSync(path.join(ablated, file), "utf8");
        for (const sentence of cut) {
          const words = new RegExp(
            sentence.text
              .split(" ")
              .map((word) => word.replace(/[.*+?^${}()|[\]\\]/g, "\\$&"))
              .join("\\s+")
          );
          expect(words.test(text)).toBe(true);
          const entry = RULES[label]?.entries.find(
            (e) =>
              e.file === file && e.cut !== undefined && sentence.text.includes(e.has ?? "\u0000")
          );
          text = text.replace(words, restore && entry?.restore ? `${entry.restore}.` : "");
        }
        writeFileSync(path.join(ablated, file), text);
      }
      return ablated;
    };

    test.if(tree === "head")(
      `${label}: cut, the head taken for the label's checkout fails once for each sentence its search cuts, and once for each restore`,
      () => {
        const dir = copy();
        const count = [...headSentences(dir, label).cuts.values()].flat().length;
        expect(count).toBeGreaterThan(0);
        const faults = searchCut(dir, dir, label).faults;
        const restores = restoresAt(dir).length;
        expect(faults).toHaveLength(count + restores);
        expect(faults.filter((fault) => fault.includes("is not applied"))).toHaveLength(count);
        expect(faults.filter((fault) => fault.includes("deleted the rule instead"))).toHaveLength(
          restores
        );
      }
    );

    test.if(tree === "head")(
      `${label}: cut, a copy with each sentence the head's search cuts deleted or restored is clean, and the rule planted in it fails`,
      () => {
        const head = copy();
        const ablated = ablate(head, true);
        expect(searchCut(ablated, head, label).faults).toEqual([]);
        // A cut can take a link with it, so the plant goes in the first of its texts the copy reads.
        const read = roleTexts(ablated, role).files;
        const file = Object.values(plant.at).find((at) => read.includes(at));
        expect(file).toBeDefined();
        appendFileSync(path.join(ablated, file as string), `\n${plant.sentence}\n`);
        const faults = searchCut(ablated, head, label).faults;
        expect(faults).toHaveLength(1);
        expect(faults[0]).toContain(`${file}:`);
        expect(faults[0]).toContain("nor the label's form of a cut");
      }
    );

    // Catches: a label that deletes a sentence it is to restore in its old words, so nothing tells
    // its agent the old rule and its control cannot score below head (the fifth round's
    // ablate-later-fail scored 5/5, as head did).
    test.if(tree === "head")(
      `${label}: cut, a copy that deletes each sentence it is to restore fails once for each`,
      () => {
        const head = copy();
        const restores = restoresAt(head);
        const faults = searchCut(ablate(head, false), head, label).faults;
        expect(faults).toHaveLength(restores.length);
        for (const [index, entry] of restores.entries())
          expect(faults[index]).toContain(`restores the old rule ("${entry.restore}")`);
      }
    );
  }

  // Catches: a label the rig no longer runs left in the search (the approved checks dropped
  // ablate-head and ablate-ci-pass, which never scored below head on their own case), and a label
  // of the search with no plant, which no row above would test.
  test("the search names the three labels the rig runs, each with its plants, and no other", () => {
    expect(Object.keys(RULES).sort()).toEqual(Object.keys(PLANTS).sort());
    expect(Object.keys(RULES).sort()).toEqual([
      "ablate-architect-later-fail",
      "ablate-later-fail",
      "ablate-two-roles",
    ]);
  });

  test("a checkout with no texts is UNKNOWN for each label, never clean", () => {
    const empty = path.join(scratch, "empty");
    cpSync(path.join(checkout, "skills/legion-worker/SKILL.md"), path.join(empty, "README.md"));
    for (const label of Object.keys(PLANTS)) {
      const faults = search(empty, tree, label).faults;
      expect(faults.some((fault) => fault.includes("UNKNOWN"))).toBe(true);
      expect(faults.some((fault) => fault.includes("is not in the checkout"))).toBe(true);
    }
  });
});
