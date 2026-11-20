// rule-search.ts: every sentence of an ablate label's rule in the texts its role reads, each one
// classified. A label measures its rule only if no text its role reads still states that rule
// (rig.sh's header: "A new scenario is an instrument only once an ablate arm has scored lower on
// it"), so a label's list of cuts comes from this search, never from a hand-made list.
//
//   bun rule-search.ts <checkout> <base|head> <label>...
//   bun rule-search.ts <checkout> cut <label> <head checkout>
//   bun rule-search.ts <checkout> texts <reviewer|architect|sub-architect>
//
// <checkout> is a tree of cybermonklabs/legion. <base> says it is #4230 before T5.12 of its plan
// addendum (621d1378273e), <head> that it holds the changes of T5.12 and T5.13: an
// entry marked for one of the two is not expected in the other. `cut` says that <checkout> is
// <head checkout>, a tree at head, with the label's cuts applied (below). `texts` lists the texts
// of a reader, each with its depth and the chain of links that reaches it first.
//
// The texts of a role are those that role-texts.ts walks: the closure of the links of its prompt
// parts and its skill, each with its depth. A text that a link of the first two kinds names and the
// checkout does not hold is a FAULT. Each text is cut into sentences as sentences.ts cuts it. A
// sentence that one of the label's patterns (label-rules.ts) matches is a hit. Each hit must
// match exactly one entry of the label: `cut` (the label deletes it or undoes T5.12's change of it)
// or `keep` (the reason it does not state the rule). Each entry that applies to the tree must match
// exactly one hit. A `fenced` entry is the one exception: it keeps each hit in a fenced block of its
// file (a format, a template or a command, which states no rule), and it may match none.
//
// Exit 0: every hit is classified and every entry matched once. Exit 1: one FAULT line for each
// hit with no entry, hit with two, entry with no hit or two, missing text, and for a run that read
// no text or found no hit (it could not see). Exit 2: a usage error.
//
// `cut` shows that no text the label's role reads in the label's checkout still states its rule.
// The head checkout's own search must be clean. Then each sentence that it cuts must be gone from
// the label's checkout, word for word, and each hit there must be a sentence that the head's search
// keeps, unchanged, or the label's form of a sentence that an entry cuts (one that a `cut` entry of
// either tree matches by its words: a sentence with other words cut, or a base sentence restored).
// A hit that is neither, a sentence the head keeps that the label changed included, is a FAULT.

import { readFileSync } from "node:fs";
import path from "node:path";
import { type Entry, RULES, type Tree } from "./label-rules";
import { chainOf, depthText, roleTexts } from "./role-texts";
import { type Sentence, sentences } from "./sentences";

export interface Outcome {
  readonly lines: string[];
  readonly faults: string[];
  readonly hits: number;
  readonly cut: number;
  readonly keep: number;
  readonly texts: number;
  /** How many texts at each depth, as `depth 0: 7, depth 1: 16`. */
  readonly depths: string;
  /** The texts that hold a cut, each once. */
  readonly cutTexts: number;
}

/** The entries of `entries` that classify `sentence`, a hit in `file`. */
function ownersOf(entries: readonly Entry[], file: string, sentence: Sentence): Entry[] {
  return entries.filter(
    (entry) =>
      entry.file === file &&
      (entry.fenced === true
        ? sentence.fenced
        : !sentence.fenced && sentence.text.includes(entry.has ?? "\u0000"))
  );
}

/** The classified search of one label in `checkout`. */
export function search(checkout: string, tree: Tree, label: string): Outcome {
  const rule = RULES[label];
  if (rule === undefined) throw new Error(`no rule for the label ${label}`);
  const texts = roleTexts(checkout, rule.role);
  const { files, missing } = texts;
  const cutFiles = new Set<string>();
  const faults = missing.map(
    (file) => `${label}: a text the ${rule.role} reads is not in the checkout: ${file}`
  );
  const entries = rule.entries.filter((entry) => entry.at === undefined || entry.at === tree);
  const matched = new Map<Entry, number>();
  const lines: string[] = [];
  let cut = 0;
  let keep = 0;
  let hits = 0;
  for (const file of files) {
    for (const sentence of sentences(file, readFileSync(path.join(checkout, file), "utf8"))) {
      if (!rule.patterns.some((pattern) => pattern.test(sentence.text))) continue;
      hits++;
      const owners = ownersOf(entries, file, sentence);
      for (const owner of owners) matched.set(owner, (matched.get(owner) ?? 0) + 1);
      const where = `${file}:${sentence.line} (depth ${texts.depth.get(file)})`;
      const words =
        sentence.text.length > 140 ? `${sentence.text.slice(0, 137)}...` : sentence.text;
      if (owners.length === 1) {
        const owner = owners[0] as Entry;
        if (owner.cut !== undefined) {
          cut++;
          cutFiles.add(file);
        } else keep++;
        lines.push(
          `${where} ${owner.cut !== undefined ? "CUT " : "KEEP"} ${words}\n    ${owner.cut ?? owner.keep}`
        );
      } else {
        faults.push(
          owners.length === 0
            ? `${label}: ${where} states the rule, or may, and no entry classifies it: ${words}`
            : `${label}: ${where} matches ${owners.length} entries: ${owners.map((o) => o.has ?? "fenced").join(" | ")}`
        );
      }
    }
  }
  for (const entry of entries) {
    const count = matched.get(entry) ?? 0;
    if (entry.fenced === true) continue;
    if (count !== 1)
      faults.push(
        `${label}: the entry "${entry.has}" of ${entry.file} matches ${count} hits at ${tree}, want 1`
      );
  }
  if (files.length === 0) faults.push(`${label}: UNKNOWN: read no text of the ${rule.role}`);
  if (hits === 0)
    faults.push(`${label}: UNKNOWN: no hit in ${files.length} texts, so the patterns see nothing`);
  return {
    lines,
    faults,
    hits,
    cut,
    keep,
    texts: files.length,
    depths: depthText(texts),
    cutTexts: cutFiles.size,
  };
}

/** The hits of a label's search in `head`, a tree at head, that one entry classifies, by text:
 * each one it cuts, and the words of each one it keeps. */
export function headSentences(
  head: string,
  label: string
): { cuts: Map<string, Sentence[]>; kept: Map<string, Set<string>> } {
  const rule = RULES[label];
  if (rule === undefined) throw new Error(`no rule for the label ${label}`);
  const headEntries = rule.entries.filter((entry) => entry.at !== "base");
  const cuts = new Map<string, Sentence[]>();
  const kept = new Map<string, Set<string>>();
  for (const file of roleTexts(head, rule.role).files) {
    for (const sentence of sentences(file, readFileSync(path.join(head, file), "utf8"))) {
      if (!rule.patterns.some((pattern) => pattern.test(sentence.text))) continue;
      const owners = ownersOf(headEntries, file, sentence);
      if (owners.length !== 1) continue;
      if ((owners[0] as Entry).cut !== undefined)
        cuts.set(file, [...(cuts.get(file) ?? []), sentence]);
      else kept.set(file, (kept.get(file) ?? new Set()).add(sentence.text));
    }
  }
  return { cuts, kept };
}

/** The search of a label's own checkout: `checkout` is `head`, a tree at head, with the label's
 * cuts applied. Each sentence that the head's search cuts must be gone from `checkout`, and each hit
 * there must be a sentence that the head's search keeps, unchanged, or the label's form of a
 * sentence that a `cut` entry of either tree matches. A `cut` entry with `restore` words, whose
 * sentence the head holds, puts the old rule back: the label's checkout must hold a sentence of its
 * file with those words, or the label deleted the rule where it was to restore it. */
export function searchCut(checkout: string, head: string, label: string): Outcome {
  const rule = RULES[label];
  if (rule === undefined) throw new Error(`no rule for the label ${label}`);
  const faults = search(head, "head", label).faults.map((fault) => `the head checkout: ${fault}`);
  const { cuts, kept } = headSentences(head, label);
  const texts = roleTexts(checkout, rule.role);
  for (const file of texts.missing)
    faults.push(`${label}: a text the ${rule.role} reads is not in the checkout: ${file}`);
  const lines: string[] = [];
  const cutFiles = new Set<string>();
  let hits = 0;
  let cut = 0;
  let keep = 0;
  const restored = new Set<Entry>();
  for (const file of texts.files) {
    const all = sentences(file, readFileSync(path.join(checkout, file), "utf8"));
    for (const entry of rule.entries)
      if (
        entry.file === file &&
        entry.restore !== undefined &&
        all.some((sentence) => sentence.text.includes(entry.restore ?? "\u0000"))
      )
        restored.add(entry);
    const words = new Set(all.map((sentence) => sentence.text));
    for (const sentence of cuts.get(file) ?? [])
      if (words.has(sentence.text))
        faults.push(
          `${label}: the cut of ${file}:${sentence.line} at head is not applied, its words are still there: ${sentence.text.slice(0, 137)}`
        );
    for (const sentence of all) {
      if (!rule.patterns.some((pattern) => pattern.test(sentence.text))) continue;
      hits++;
      const where = `${file}:${sentence.line} (depth ${texts.depth.get(file)})`;
      const short =
        sentence.text.length > 140 ? `${sentence.text.slice(0, 137)}...` : sentence.text;
      if (kept.get(file)?.has(sentence.text)) {
        keep++;
        lines.push(
          `${where} KEEP ${short}\n    The head's sentence, unchanged, which its search keeps.`
        );
        continue;
      }
      const owner = rule.entries.find(
        (entry) =>
          entry.cut !== undefined &&
          (ownersOf([entry], file, sentence).length === 1 ||
            (entry.file === file && sentence.text.includes(entry.restore ?? "\u0000")))
      );
      if (owner !== undefined) {
        cut++;
        cutFiles.add(file);
        lines.push(`${where} CUT  ${short}\n    The label's form of it: ${owner.cut}`);
        continue;
      }
      faults.push(
        `${label}: ${where} states the rule, or may, and is neither a sentence the head's search keeps, unchanged, nor the label's form of a cut: ${short}`
      );
    }
  }
  for (const entry of rule.entries) {
    const cutAtHead = (cuts.get(entry.file) ?? []).some((sentence) =>
      sentence.text.includes(entry.has ?? "\u0000")
    );
    if (entry.restore !== undefined && cutAtHead && !restored.has(entry))
      faults.push(
        `${label}: the cut of "${entry.has}" in ${entry.file} restores the old rule ("${entry.restore}"), and the label's checkout holds no sentence with those words there: it deleted the rule instead`
      );
  }
  if (texts.files.length === 0) faults.push(`${label}: UNKNOWN: read no text of the ${rule.role}`);
  if (cuts.size === 0)
    faults.push(`${label}: UNKNOWN: the head's search cuts no sentence, so no cut is checked`);
  return {
    lines,
    faults,
    hits,
    cut,
    keep,
    texts: texts.files.length,
    depths: depthText(texts),
    cutTexts: cutFiles.size,
  };
}

function main(argv: string[]): number {
  const [checkout, tree, ...labels] = argv;
  if (checkout !== undefined && tree === "texts") {
    const reader = labels[0];
    if (
      labels.length !== 1 ||
      (reader !== "reviewer" && reader !== "architect" && reader !== "sub-architect")
    ) {
      console.error(
        "usage: bun rule-search.ts <checkout> texts <reviewer|architect|sub-architect>"
      );
      return 2;
    }
    const texts = roleTexts(checkout, reader);
    for (const file of texts.files)
      console.log(
        `${texts.depth.get(file)} ${file} <= ${chainOf(texts, file).slice(0, -1).join(" > ")}`
      );
    console.log(`${reader}: ${texts.files.length} texts, ${depthText(texts)}`);
    for (const file of texts.missing)
      console.error(`FAULT: a text the ${reader} reads is not in the checkout: ${file}`);
    return texts.missing.length > 0 || texts.files.length === 0 ? 1 : 0;
  }
  const head = tree === "cut" ? labels[1] : undefined;
  if (
    checkout === undefined ||
    (tree !== "base" && tree !== "head" && tree !== "cut") ||
    labels.length === 0 ||
    (tree === "cut" && (labels.length !== 2 || head === undefined))
  ) {
    console.error("usage: bun rule-search.ts <checkout> <base|head> <label>...");
    console.error("       bun rule-search.ts <checkout> cut <label> <head checkout>");
    console.error("       bun rule-search.ts <checkout> texts <reviewer|architect|sub-architect>");
    console.error(`labels: ${Object.keys(RULES).join(", ")}`);
    return 2;
  }
  const named = tree === "cut" ? labels.slice(0, 1) : labels;
  const unknown = named.filter((label) => RULES[label] === undefined);
  if (unknown.length > 0) {
    console.error(`no rule for ${unknown.join(", ")}; labels: ${Object.keys(RULES).join(", ")}`);
    return 2;
  }
  let failed = false;
  for (const label of named) {
    const outcome =
      tree === "cut" ? searchCut(checkout, head as string, label) : search(checkout, tree, label);
    console.log(`## ${label}: ${RULES[label]?.rule}`);
    for (const line of outcome.lines) console.log(line);
    console.log(
      `${label} at ${tree === "cut" ? `the label's cuts of ${head}` : tree}: ${outcome.hits} hits in ${outcome.texts} texts of the ${RULES[label]?.role} (${outcome.depths}), ${outcome.cut} cut in ${outcome.cutTexts} texts, ${outcome.keep} kept with a reason, ${outcome.faults.length} faults`
    );
    for (const fault of outcome.faults) console.error(`FAULT: ${fault}`);
    failed ||= outcome.faults.length > 0;
  }
  return failed ? 1 : 0;
}

if (import.meta.main) process.exit(main(Bun.argv.slice(2)));
