// role-texts.ts: the texts a reader reads in a checkout of cybermonklabs/legion, the walk that
// rule-search.ts and gate-search.ts both search.
//
// The texts of a role are the closure of its links. Its entry texts are its prompt parts, as
// prompts.Compose gives them (packages/daemon/internal/prompts/prompts.go, Compose), and its
// skill. Then each file that a link in a text names is a text too, and so on, until no new text
// appears. The depth of a text is the fewest links from an entry text. A link is one of three:
//   - `skill://<name>` is skills/<name>/SKILL.md, and `skill://<name>/<path>` is skills/<name>/<path>;
//   - a relative `](<file>.md)` is read from the linking file's folder;
//   - a path in backticks that ends `.md` and names a file of the checkout, read from the linking
//     file's folder, from its skill's folder (skills/<name>/), or, for a path that starts
//     `skills/` or `packages/daemon/internal/prompts/`, from the checkout's root.
// A file of the first two kinds that the checkout does not hold is missing. A path in backticks
// that names no file is no link (`plan.md`, `AGENTS.md`). The session's list of skills is a link
// too: Oh My Pi names each installed skill to the session, and the two plugins install every folder
// of skills/ (scripts/pi-plugin-prepack.sh), so each skills/<name>/SKILL.md is at depth 1 at most.
// A text that no chain of links reaches, such as another role's prompt part, is not read. The rig's
// architect is a tree's root architect (architect-root.md).

import { existsSync, readdirSync, readFileSync } from "node:fs";
import path from "node:path";

export type Role = "reviewer" | "architect";
/** A reader of texts: a label's role, or the architect of a tree's child (architect.md), whom no
 * label runs and gate-search.ts reads for. */
export type Reader = Role | "sub-architect";

const PROMPTS = "packages/daemon/internal/prompts";
const ROLE_TEXTS: Record<Reader, { readonly parts: readonly string[]; readonly skill: string }> = {
  reviewer: {
    parts: [
      "roles/core/common.md",
      "roles/core/reviewer.md",
      "roles/mechanics/headless.md",
      "roles/reviewer.md",
      "go/reviewer.md",
      "go/worker-common.md",
    ].map((part) => `${PROMPTS}/${part}`),
    skill: "skills/legion-worker/SKILL.md",
  },
  architect: {
    parts: ["roles/architect-root.md", "go/architect-root.md", "go/architect-common.md"].map(
      (part) => `${PROMPTS}/${part}`
    ),
    skill: "skills/legion-architect/SKILL.md",
  },
  "sub-architect": {
    parts: ["roles/architect.md", "go/architect.md", "go/architect-common.md"].map(
      (part) => `${PROMPTS}/${part}`
    ),
    skill: "skills/legion-architect/SKILL.md",
  },
};

/** The texts of a role in `checkout`: the closure of its links. */
export interface RoleTexts {
  /** Each text, relative to the checkout, in byte order. */
  readonly files: string[];
  /** Each link of the first two kinds that names no file of the checkout, with the text that names it. */
  readonly missing: string[];
  /** The fewest links from an entry text to each text: 0 for an entry text. */
  readonly depth: ReadonlyMap<string, number>;
  /** The text whose link first reached each text; none for an entry text. */
  readonly via: ReadonlyMap<string, string>;
}

/** The files that the links of `text`, the text `file`, name. `strict` links must name a file. */
export function linksOf(
  checkout: string,
  file: string,
  text: string
): { strict: string[]; mentions: string[] } {
  const strict: string[] = [];
  for (const match of text.matchAll(/skill:\/\/([A-Za-z0-9_.-]+)((?:\/[A-Za-z0-9_./-]+)?)/g)) {
    const rest = (match[2] ?? "").replace(/[./]+$/, "");
    strict.push(rest === "" ? `skills/${match[1]}/SKILL.md` : `skills/${match[1]}${rest}`);
  }
  for (const match of text.matchAll(/\]\((?!https?:|skill:|dispatch:|#)([^)#\s]+\.md)/g))
    strict.push(path.normalize(path.join(path.dirname(file), match[1] ?? "")));
  const mentions: string[] = [];
  const skillDir = /^skills\/[^/]+/.exec(file)?.[0];
  for (const match of text.matchAll(/`([A-Za-z0-9_.][A-Za-z0-9_./-]*\.md)`/g)) {
    const named = match[1] ?? "";
    const places = [path.join(path.dirname(file), named)];
    if (skillDir !== undefined) places.push(path.join(skillDir, named));
    if (named.startsWith("skills/") || named.startsWith(`${PROMPTS}/`)) places.push(named);
    const found = places
      .map((place) => path.normalize(place))
      .find((place) => existsSync(path.join(checkout, place)));
    if (found !== undefined) mentions.push(found);
  }
  return { strict, mentions };
}

/** What names a skill that no link of an entry text names: the list of skills a session gets. */
export const SKILL_LIST = "(the session's list of skills)";

/** Each installed skill's SKILL.md: each folder of skills/ that holds one, as the plugins stage
 * them (scripts/pi-plugin-prepack.sh, whose two lists partition skills/). */
export function installedSkills(checkout: string): string[] {
  const dir = path.join(checkout, "skills");
  if (!existsSync(dir)) return [];
  return readdirSync(dir, { withFileTypes: true })
    .filter((entry) => entry.isDirectory() && existsSync(path.join(dir, entry.name, "SKILL.md")))
    .map((entry) => `skills/${entry.name}/SKILL.md`)
    .sort();
}

/** The texts a reader reads in `checkout`: its entry texts, then each text a link names, to closure. */
export function roleTexts(checkout: string, reader: Reader): RoleTexts {
  const { parts, skill } = ROLE_TEXTS[reader];
  const depth = new Map<string, number>();
  const via = new Map<string, string>();
  const missing: string[] = [];
  let frontier: string[] = [];
  const reach = (file: string, from: string | undefined, at: number, strict: boolean) => {
    if (depth.has(file)) return;
    if (!existsSync(path.join(checkout, file))) {
      if (strict && !missing.some((m) => m.startsWith(`${file} `)))
        missing.push(`${file} (named in ${from ?? "the role's prompt parts and skill"})`);
      return;
    }
    depth.set(file, at);
    if (from !== undefined) via.set(file, from);
    frontier.push(file);
  };
  for (const root of [...parts, skill]) reach(root, undefined, 0, true);
  for (let at = 1; frontier.length > 0; at++) {
    const level = frontier;
    frontier = [];
    for (const file of level) {
      const { strict, mentions } = linksOf(
        checkout,
        file,
        readFileSync(path.join(checkout, file), "utf8")
      );
      for (const named of strict) reach(named, file, at, true);
      for (const named of mentions) reach(named, file, at, false);
    }
    if (at === 1)
      for (const installed of installedSkills(checkout)) reach(installed, SKILL_LIST, at, false);
  }
  return { files: [...depth.keys()].sort(), missing, depth, via };
}

/** The chain of links from an entry text to `file`, the entry text first. */
export function chainOf(texts: RoleTexts, file: string): string[] {
  const chain = [file];
  for (let from = texts.via.get(file); from !== undefined; from = texts.via.get(from))
    chain.unshift(from);
  return chain;
}

/** How many texts of `texts` sit at each depth. */
export function depthText(texts: RoleTexts): string {
  const count = new Map<number, number>();
  for (const at of texts.depth.values()) count.set(at, (count.get(at) ?? 0) + 1);
  return [...count]
    .sort((a, b) => a[0] - b[0])
    .map(([at, n]) => `depth ${at}: ${n}`)
    .join(", ");
}
