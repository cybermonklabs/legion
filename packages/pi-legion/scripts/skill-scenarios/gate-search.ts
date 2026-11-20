// gate-search.ts: each sentence that states the merge condition in a text that a reviewer or an
// architect reads, which facts of the spec's R14 to R16 it states, and a flag on each one that
// states fewer. A text that states the condition with fewer facts is one that an agent can take for
// the whole rule: a reviewer approved a head whose line read `yes, from implementer` because a
// reference stated the gate as "no pass standing at the head being reviewed", with no word of the
// two roles that its role prompt asks for.
//
//   bun gate-search.ts <checkout> [--generated <dir>]
//
// <checkout> is a tree of cybermonklabs/legion. The texts are those that role-texts.ts gives the
// reviewer, the root architect and the architect of a child (the closure of the links of each one's
// prompt parts and skill), and each `.md` file of `packages/claude-envoy/legion/`, the plugin that a
// Claude Code pane loads. That plugin is generated, never committed: the program builds it with the
// checkout's own `packages/claude-envoy/scripts/build.ts --out` into a directory of its own, or
// reads the build that `--generated` names (the `--out` directory of such a build).
//
// The merge condition is when the reviewer approves, when the architect accepts or signs off, and
// when a pull request may merge. A hit is a sentence outside a fenced block that names one of those
// acts (ACT) and a pass of a Dispatch check (CHECK). The statement of a hit is the hit and each later
// sentence of its paragraph or list item that starts on the line where the one before it ends: a
// sentence that ends a line ends the statement. So in a text with one group of sentences per line
// the statement is the rest of the hit's line, and in a wrapped paragraph the rest of the paragraph.
//
// Each hit that states the condition is a gate of one of three kinds: the reviewer's approval, the
// architect's acceptance, or another statement of when a pull request may merge (`merge`, such as
// the retro's read before `merging`). An entry below gives the kind where the hit's words do not:
// they make it the architect's when they name an acceptance or a sign-off, and the reviewer's
// otherwise. A statement states each fact that NEEDS lists for its kind, as FACTS words it; one that
// lacks a fact is FLAGGED. A hit that states no merge condition is kept only by an entry that gives
// the reason, so a new sentence that states the condition is judged by its own words.
//
// Exit 0: no flag and no fault. Exit 1: a FAULT line for each flag, for each entry that matches no
// hit or more than one in a text, for a hit that two entries match, and for a run that read no text,
// found no hit or no gate, or could not read the generated plugin (it could not see). Exit 2: a usage
// error.

import { existsSync, mkdtempSync, readdirSync, readFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import path from "node:path";
import { type Reader, roleTexts } from "./role-texts";
import { type Sentence, sentences } from "./sentences";

type Kind = "reviewer" | "architect" | "merge";

export interface Entry {
  /** The text, relative to the checkout. It classifies the same sentence in its generated copy. */
  readonly file: string;
  /** Words of the sentence, with runs of white space as one space. */
  readonly has: string;
  /** The kind of the gate the sentence states, where its words do not give it. */
  readonly gate?: Kind;
  /** Why the sentence states no merge condition. */
  readonly keep?: string;
}

const ACT =
  /\bapprov(e|es|ed|al|ing)\b|\baccept(s|ed|ance|ing)?\b|\bsign(s|ed|ing)?[ _-]?off\b|\bmerges?\b|\bmerged\b|\bgate[sd]?\b/i;
const CHECK =
  /\bbefore_merge\b|\bpass(es)? stands?\b|\bpass standing\b|\breads? yes\b|\bno pass\b|\bpassing result\b|\bdispatch results\b/i;
const ARCHITECT = /\baccept(s|ed|ance|ing)?\b|\bsign(s|ed|ing)?[ _-]?off\b/i;
const NAMING_THE_CHECK = String.raw`\bnam(e|es|ing)\b[^.]{0,30}\bcheck\b`;

/** A fact of R14 to R16, and the words that state it: a statement states it when each matches. */
export const FACTS: Record<string, { readonly name: string; readonly says: readonly RegExp[] }> = {
  line: {
    name: "R14: the reviewed head's own line decides",
    says: [/\bline of (the|that|this) ([\w-]+ ){0,3}head\b|\bhead's (own )?line\b/i],
  },
  "other head": {
    name: "R14: another head's state does not count",
    says: [/\banother (commit|head)\b|\bother heads?\b/i],
  },
  roles: {
    name: "R14: a pass from the implementer and one from the tester",
    says: [
      /\bfrom the implementer and (one |a pass )?from the tester\b|\bfrom the tester and (one |a pass )?from the implementer\b/i,
    ],
  },
  ci: {
    name: "R14: a pass from CI for a check that CI proves",
    says: [/`ci_check`/, /\bfrom CI\b|\bCI's\b/],
  },
  "not known": {
    name: "R15 and R16: a pass and a fail at one head leave it not known",
    says: [/\bpass and a fail\b|\bfail and a pass\b/i, /\bnot known\b/i],
  },
  refusal: {
    name: "R15: a refusal that names the check",
    says: [
      new RegExp(
        String.raw`\b(REQUEST_CHANGES|refus\w*|request(s|ing)? changes)\b[^.]*?${NAMING_THE_CHECK}`,
        "i"
      ),
    ],
  },
  "send back": {
    name: "R16: the issue sent back, naming the check",
    says: [
      new RegExp(
        String.raw`(\bsend(s|ing)? (the issue|it) back\b|\brequest_backward_move\b)[^.]*?${NAMING_THE_CHECK}`,
        "i"
      ),
    ],
  },
  "no acceptance": {
    name: "R16: no acceptance",
    says: [/\bdo(es)? not accept\b|\bno acceptance\b|\bnever accept\b|\baccept only when\b/i],
  },
  "no wait": {
    name: "R15 and R16: no wait for a later result",
    says: [/\bno wait\b|\bdo(es)? not wait\b|\bwithout wait(ing)?\b|\bnever waits?\b/i],
  },
};

/** The facts that a statement of each kind of gate states: R14 and R15 for the reviewer's, R16 for
 * the architect's, and for another statement of when a pull request may merge, R14 and the state
 * that R15 and R16 both act on. */
export const NEEDS: Record<Kind, readonly string[]> = {
  reviewer: ["line", "other head", "roles", "ci", "not known", "refusal", "no wait"],
  architect: ["not known", "send back", "no acceptance", "no wait"],
  merge: ["line", "other head", "roles", "ci", "not known"],
};

const ACCEPTANCE_STEP =
  "What the acceptance reads and its two moves, with no condition of its own: the condition is section 6 of `skill://legion-architect`, which states each fact of R16.";
const NO_CHECK =
  "An issue with no `before_merge` check, so no check whose answer is read: the acceptance decides its delivered items.";
const MERGE_OF_THE_BASE =
  "Which checks run again after a merge of the base, and that a pass from before that merge carries none to its head. It states no condition of an approval, an acceptance or a merge of the pull request.";
const ENTRIES: readonly Entry[] = [
  {
    file: "packages/daemon/internal/prompts/go/architect-common.md",
    has: "The reviewer's `phase-finished` notice over an `APPROVED` review is the daemon telling you",
    keep: ACCEPTANCE_STEP,
  },
  {
    file: "packages/daemon/internal/prompts/go/worker-common.md",
    has: "A check those files or the resolution can reach runs again",
    keep: MERGE_OF_THE_BASE,
  },
  {
    file: "packages/daemon/internal/prompts/roles/architect-root.md",
    has: "You then read the tester's handoff",
    keep: "What the acceptance reads, once the reviewer's notice reaches the architect. Its condition is the gate this text states below it.",
  },
  {
    file: "packages/daemon/internal/prompts/roles/architect-root.md",
    has: "The gate is the `before_merge` checks of the issue's own Verification document",
    gate: "architect",
  },
  {
    file: "packages/daemon/internal/prompts/roles/architect-root.md",
    has: "An issue with no Verification document, or with one that holds no `before_merge` check",
    keep: NO_CHECK,
  },
  {
    file: "packages/daemon/internal/prompts/roles/architect.md",
    has: "The reviewer's approval moves this child to `accepting`",
    keep: ACCEPTANCE_STEP,
  },
  {
    file: "packages/daemon/internal/prompts/roles/architect.md",
    has: "The gate is the `before_merge` checks of the issue's own Verification document",
    gate: "architect",
  },
  {
    file: "packages/daemon/internal/prompts/roles/architect.md",
    has: "An issue with no Verification document, or with one that holds no `before_merge` check",
    keep: NO_CHECK,
  },
  {
    file: "packages/daemon/internal/prompts/roles/core/reviewer.md",
    has: "A check those files or the resolution can reach runs again",
    keep: MERGE_OF_THE_BASE,
  },
  {
    file: "skills/legion-worker/references/conflicts-and-rewrites.md",
    has: "A check those files or the resolution can reach runs again",
    keep: MERGE_OF_THE_BASE,
  },
];

/** Where the generated plugin's texts sit in a checkout's terms. */
export const GENERATED = "packages/claude-envoy/legion";

/** The text a generated text is made from: a staged skill's file, or a task agent's. */
export function sourceOf(file: string): string {
  if (file.startsWith(`${GENERATED}/skills/`)) return file.slice(GENERATED.length + 1);
  if (file.startsWith(`${GENERATED}/agents/`))
    return `packages/pi-legion/agents/${file.slice(`${GENERATED}/agents/`.length)}`;
  return file;
}

/** The checkout's plugin, built by its own build.ts into a new directory: that `--out` directory,
 * or why there is none. The caller removes the directory that holds it. */
export function buildGenerated(checkout: string): { out?: string; fault?: string } {
  const pkg = path.join(checkout, "packages/claude-envoy");
  if (!existsSync(path.join(pkg, "scripts/build.ts")))
    return { fault: `${checkout} holds no packages/claude-envoy/scripts/build.ts` };
  const out = path.join(mkdtempSync(path.join(tmpdir(), "gate-search-")), "out");
  const built = Bun.spawnSync(["bun", "scripts/build.ts", "--out", out], { cwd: pkg });
  if (built.exitCode !== 0)
    return { out, fault: `build.ts exited ${built.exitCode}: ${built.stderr.toString().trim()}` };
  return { out };
}

interface Text {
  readonly file: string;
  readonly body: string;
  /** Who reads it: each reader with its depth (`reviewer 0, architect 2`), or `generated`. */
  readonly readers: string;
}

export interface GateOutcome {
  readonly lines: string[];
  readonly faults: string[];
  readonly texts: number;
  readonly generated: number;
  readonly hits: number;
  readonly gates: number;
  readonly flagged: number;
  readonly kept: number;
}

const READERS: readonly Reader[] = ["reviewer", "architect", "sub-architect"];

/** The texts a reviewer or an architect reads in `checkout`, then the generated plugin's texts
 * under `out` (none where `out` is null). */
function textsOf(checkout: string, out: string | null): { texts: Text[]; faults: string[] } {
  const faults: string[] = [];
  const read = READERS.map((reader) => ({ reader, texts: roleTexts(checkout, reader) }));
  for (const { reader, texts } of read)
    for (const missing of texts.missing)
      faults.push(`a text the ${reader} reads is not in the checkout: ${missing}`);
  const files = [...new Set(read.flatMap(({ texts }) => texts.files))].sort();
  const texts: Text[] = files.map((file) => ({
    file,
    body: readFileSync(path.join(checkout, file), "utf8"),
    readers: read
      .filter(({ texts }) => texts.depth.has(file))
      .map(({ reader, texts }) => `${reader} ${texts.depth.get(file)}`)
      .join(", "),
  }));
  if (out === null) return { texts, faults };
  const root = path.join(out, "legion");
  const generated = existsSync(root)
    ? readdirSync(root, { recursive: true, encoding: "utf8" })
        .filter((name) => name.endsWith(".md"))
        .sort()
    : [];
  if (generated.length === 0)
    faults.push(`UNKNOWN: no generated text under ${root}, so the plugin's copies are not read`);
  for (const name of generated)
    texts.push({
      file: `${GENERATED}/${name}`,
      body: readFileSync(path.join(root, name), "utf8"),
      readers: "generated",
    });
  return { texts, faults };
}

/** The words of the statement of the hit at `index`: the hit, and each later sentence of its unit
 * that starts on the line where the one before it ends. */
function statementOf(all: readonly Sentence[], index: number): string {
  const hit = all[index] as Sentence;
  const words = [hit.text];
  for (let next = index + 1; next < all.length; next++) {
    const sentence = all[next] as Sentence;
    if (sentence.unit !== hit.unit || sentence.line !== (all[next - 1] as Sentence).last) break;
    words.push(sentence.text);
  }
  return words.join(" ");
}

/** Each statement of the merge condition in `checkout`, with its facts. `generated` is a build's
 * `--out` directory, undefined to build one, or null to read no generated text. */
export function gateSearch(
  checkout: string,
  generated: string | null | undefined,
  exemptions: readonly Entry[] = []
): GateOutcome {
  const entries = [...ENTRIES, ...exemptions];
  const faults: string[] = [];
  let out = generated ?? null;
  let built: string | undefined;
  if (generated === undefined) {
    const build = buildGenerated(checkout);
    if (build.out !== undefined) built = path.dirname(build.out);
    if (build.fault !== undefined)
      faults.push(`UNKNOWN: the generated plugin is not read: ${build.fault}`);
    else out = build.out ?? null;
  }
  try {
    const { texts, faults: unread } = textsOf(checkout, out);
    faults.push(...unread);
    const lines: string[] = [];
    const matched = new Map<Entry, Map<string, number>>();
    let hits = 0;
    let gates = 0;
    let flagged = 0;
    let kept = 0;
    for (const { file, body, readers } of texts) {
      const all = sentences(file, body);
      all.forEach((sentence, index) => {
        if (sentence.fenced || !ACT.test(sentence.text) || !CHECK.test(sentence.text)) return;
        hits++;
        const owners = entries.filter(
          (entry) => entry.file === sourceOf(file) && sentence.text.includes(entry.has)
        );
        for (const owner of owners) {
          const counts = matched.get(owner) ?? new Map<string, number>();
          counts.set(file, (counts.get(file) ?? 0) + 1);
          matched.set(owner, counts);
        }
        const where = `${file}:${sentence.line} (${readers})`;
        const words =
          sentence.text.length > 160 ? `${sentence.text.slice(0, 157)}...` : sentence.text;
        if (owners.length > 1) {
          faults.push(
            `${where} matches ${owners.length} entries: ${owners.map((o) => o.has).join(" | ")}`
          );
          return;
        }
        const owner = owners[0];
        if (owner?.keep !== undefined) {
          kept++;
          lines.push(`${where} KEEP ${words}\n    ${owner.keep}`);
          return;
        }
        const kind = owner?.gate ?? (ARCHITECT.test(sentence.text) ? "architect" : "reviewer");
        const statement = statementOf(all, index);
        const states = NEEDS[kind].filter((id) =>
          FACTS[id]?.says.every((pattern) => pattern.test(statement))
        );
        const lacks = NEEDS[kind].filter((id) => !states.includes(id));
        gates++;
        lines.push(
          `${where} GATE ${kind}${owner === undefined ? " (by its words)" : ""}: states ${states.join(", ") || "none"}${lacks.length > 0 ? `; FLAG: lacks ${lacks.join(", ")}` : ""}\n    ${words}`
        );
        if (lacks.length > 0) {
          flagged++;
          faults.push(
            `${where} states the ${kind}'s merge condition and lacks ${lacks.map((id) => FACTS[id]?.name).join("; ")}: ${words}`
          );
        }
      });
    }
    for (const entry of entries) {
      const own = texts.map((text) => text.file).filter((file) => sourceOf(file) === entry.file);
      if (own.length === 0)
        faults.push(`the entry "${entry.has}" names ${entry.file}, which no reader reads`);
      for (const file of own) {
        const count = matched.get(entry)?.get(file) ?? 0;
        if (count !== 1)
          faults.push(
            `the entry "${entry.has}" of ${entry.file} matches ${count} hits in ${file}, want 1`
          );
      }
    }
    if (texts.length === 0) faults.push("UNKNOWN: read no text of the reviewer or the architect");
    if (hits === 0)
      faults.push(`UNKNOWN: no hit in ${texts.length} texts, so the net sees nothing`);
    else if (gates === 0) faults.push(`UNKNOWN: ${hits} hits and none of them a gate`);
    return {
      lines,
      faults,
      texts: texts.length,
      generated: texts.filter((text) => text.readers === "generated").length,
      hits,
      gates,
      flagged,
      kept,
    };
  } finally {
    if (built !== undefined) rmSync(built, { recursive: true, force: true });
  }
}

function main(argv: string[]): number {
  const [checkout, flag, dir, ...rest] = argv;
  if (
    checkout === undefined ||
    rest.length > 0 ||
    (flag !== undefined && (flag !== "--generated" || dir === undefined))
  ) {
    console.error("usage: bun gate-search.ts <checkout> [--generated <build.ts --out dir>]");
    return 2;
  }
  const outcome = gateSearch(checkout, dir);
  for (const line of outcome.lines) console.log(line);
  console.log(
    `gate-search: ${outcome.texts} texts (${outcome.generated} generated), ${outcome.hits} hits, ${outcome.gates} gates, ${outcome.flagged} flagged, ${outcome.kept} kept with a reason, ${outcome.faults.length} faults`
  );
  for (const fault of outcome.faults) console.error(`FAULT: ${fault}`);
  return outcome.faults.length > 0 ? 1 : 0;
}

if (import.meta.main) process.exit(main(Bun.argv.slice(2)));
