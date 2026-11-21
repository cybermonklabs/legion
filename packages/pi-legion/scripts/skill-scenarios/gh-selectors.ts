// Which owners and which repository a gh call names, read as the Legion gh wrapper reads them
// (`legion gh`, packages/daemon/cmd/legion's ghTargetOf) by that wrapper's own selector table, and
// the refusal it prints for a call naming an owner outside the project, or, for a wrapper of main's
// since #4035, whether it runs such a call on the pane's own token instead (gh-pane-token.ts). The
// world's gh answers such a call so (gh-standin.ts), and health holds each such call to that same
// reading (health.ts's rule 7), so the world and its judge read a call one way.
//
// The table is the wrapper's, read out of the run's own label checkout (daemon-pane.ts's
// `labelGhSelectors`, which runs gh-selectors.go over its source and keeps the printout in the
// facts cache): the kind of each argument a gh usage names (ghSlotKinds), of each option a gh help
// lists (ghOptionKinds), each leaf's quirks (ghLeafQuirks), the order a slot's names rank in
// (ghKindRank) and the patterns a value is read by. What a leaf's usage and options are is gh's
// help, which the wrapper reads from the gh it runs: the world reads it from the help the label's
// checkout saves for the wrapper's own tests (testdata/gh-help, gh 2.98.0's), each saved command's
// whole help and, for every other, its section of gh's reference with the options its nearest
// saved parent group lists. Both are joined here into one description of every command
// (`selectorsOf`), which rig.sh's worker_boot writes into the run (RUN_SELECTORS), since the
// agent's namespace shows neither the checkout nor the work directory.
//
// A kind or a quirk the table holds that this reading does not model (MODELLED) is refused when
// the table is read, naming it, so a form the wrapper learns later fails the batch at services_up
// and the pin in gh-standin.test.ts, label by label, rather than reaching a world that would answer
// it otherwise than production. A call this reading cannot place (an option no help lists, an
// argument a usage has no room for, a value the wrapper refuses for another reason than its
// owner) is not read at all: the world answers it as it answers any call, which is the stated
// limit in packages/pi-envoy/scripts/README.md's rig section.
import { existsSync, readFileSync } from "node:fs";
import path from "node:path";
import { z } from "zod";

/** The run's copy of its label's selectors, which rig.sh's worker_boot writes before any agent of
 * the run starts (daemon-pane.ts's `gh-selectors` command). */
export const RUN_SELECTORS = "gh-selectors.json";

/** What this reading models of the wrapper's table: every kind an argument or an option selects
 * by, and every quirk a leaf may have, each read below as ghTargetOf reads it
 * (gh_selectors.go's fromCommand, fromOptions and fromOwnRepository). */
export const MODELLED = {
  slot: [
    "",
    "repository",
    "second-repository",
    "owner",
    "url",
    "query",
    "number",
    "new-repository",
  ],
  option: ["repository", "org-repositories", "owner", "owner/name", "host", "url", "nothing"],
  quirk: [
    "readsGHRepo",
    "readsAllRemotes",
    "readsRepositoryNumber",
    "pullRequestFinder",
    "targetOption",
  ],
} as const;

/** The wrapper's patterns, each by its variable's name: gh_help.go's grammar of a help, and
 * gh_selectors.go's (and internal/ghconfig's ownerLogin) of a value. */
const PATTERNS = [
  "ghFlagLine",
  "ghCommandWord",
  "ghSlotName",
  "ghMentionsSelector",
  "ghRepositoryNumber",
  "ghPullRequestPath",
  "ghQualifier",
  "ghPlaceholder",
  "ownerLogin",
] as const;
type PatternName = (typeof PATTERNS)[number];
/** The pattern of OWNER/REPO#N, which the wrapper reads in a number's place for a leaf whose quirk
 * says so (readsRepositoryNumber), and which one shape of it declares and another does not: main's
 * before #4424 (fdce4e94d2) declares it, with `agent-task view`'s quirk reading it, and main's
 * since declares neither, refusing `gh agent-task` whole (gh-selectors.go's optionalPatterns). */
const REPOSITORY_NUMBER = "ghRepositoryNumber";
type RequiredPattern = Exclude<PatternName, typeof REPOSITORY_NUMBER>;
/** The patterns a table holds: each but OWNER/REPO#N's always, and that one where its shape has
 * it. */
type Patterns = Readonly<Record<RequiredPattern, string>> & {
  readonly [REPOSITORY_NUMBER]?: string;
};

/** One argument name's kind and whether it stands for more than one argument (ghArgKind). */
interface ArgKind {
  readonly kind: string;
  readonly many: boolean;
}

/** The wrapper's grammar of a call that writes nothing (gh_reads.go's ghWritesNothing and
 * ghGraphQLQueryRead), as gh-selectors.go prints it: the options each leaf of ghReadLeaves may be
 * given, a `gh api` read's (ghAPIReadOptions) and a `gh api graphql` query's
 * (ghGraphQLReadOptions, main's since #4035), each spelling true where it takes a value, and the
 * headers the wrapper reads as a method (ghMethodOverrideHeaders). */
export const ReadGrammar = z.object({
  leaves: z.record(z.string(), z.record(z.string(), z.boolean())),
  api: z.record(z.string(), z.boolean()),
  graphql: z.record(z.string(), z.boolean()),
  overrides: z.array(z.string()),
});
export type ReadGrammar = z.infer<typeof ReadGrammar>;

/** The wrapper's table as gh-selectors.go prints it. */
export interface GhTable {
  readonly slots: Readonly<Record<string, ArgKind>>;
  readonly options: Readonly<Record<string, string>>;
  readonly quirks: Readonly<
    Record<
      string,
      { readonly fields: Record<string, string>; readonly slots: Record<string, ArgKind> }
    >
  >;
  readonly ranks: Readonly<Record<string, number>>;
  readonly patterns: Patterns;
  /** The file each pattern is declared in, under the daemon module the table was read from. */
  readonly declared: Patterns;
  /** Its read grammar, a `gh api graphql` query's options none (`{}`) where the wrapper declares
   * none, as main's before #4035. */
  readonly reads: ReadGrammar;
}

/** The table gh-selectors.go printed as `printed` over the daemon module `module`, every kind and
 * quirk held to MODELLED, and each pattern's file named under `module`. Throws naming the first
 * line it cannot read, the first entry of a kind or a quirk this reading does not model, and a
 * table of neither shape of OWNER/REPO#N's reading (`REPOSITORY_NUMBER`): its pattern with no
 * quirk reading it, or a quirk reading it with no pattern. */
export function tableOf(printed: string, module: string): GhTable {
  const slots: Record<string, ArgKind> = {};
  const options: Record<string, string> = {};
  const quirks: Record<string, { fields: Record<string, string>; slots: Record<string, ArgKind> }> =
    {};
  const ranks: Record<string, number> = {};
  const patterns: Partial<Record<PatternName, string>> = {};
  const declared: Partial<Record<PatternName, string>> = {};
  const reads: ReadGrammar = { leaves: {}, api: {}, graphql: {}, overrides: [] };
  const quirkOf = (leaf: string) => {
    quirks[leaf] ??= { fields: {}, slots: {} };
    return quirks[leaf];
  };
  const unmodelled = (what: string, kind: string, entry: string) =>
    new Error(
      `the wrapper's table holds the ${what} ${JSON.stringify(kind)} (${entry}), which the world's gh does not model (gh-selectors.ts's MODELLED)`
    );
  const slotKind = (kind: string, many: string, entry: string): ArgKind => {
    if (!(MODELLED.slot as readonly string[]).includes(kind))
      throw unmodelled("argument kind", kind, entry);
    return { kind, many: many === "1" };
  };
  for (const line of printed.split("\n")) {
    if (line === "") continue;
    const [type = "", ...fields] = line.split("\t");
    const [a = "", b = "", c = "", d = ""] = fields;
    if (type === "slot" && fields.length === 3) slots[a] = slotKind(b, c, `ghSlotKinds ${a}`);
    else if (type === "option" && fields.length === 2) {
      if (!(MODELLED.option as readonly string[]).includes(b))
        throw unmodelled("option kind", b, `ghOptionKinds ${a}`);
      options[a] = b;
    } else if (type === "quirk" && fields.length === 3) {
      if (!(MODELLED.quirk as readonly string[]).includes(b))
        throw unmodelled("quirk", b, `ghLeafQuirks ${a}`);
      quirkOf(a).fields[b] = c;
    } else if (type === "quirk-slot" && fields.length === 4)
      quirkOf(a).slots[b] = slotKind(c, d, `ghLeafQuirks ${a}'s ${b}`);
    else if (type === "rank" && fields.length === 2) {
      if (!(MODELLED.slot as readonly string[]).includes(a))
        throw unmodelled("argument kind", a, "ghKindRank");
      ranks[a] = Number(b);
    } else if (
      type === "pattern" &&
      fields.length === 3 &&
      (PATTERNS as readonly string[]).includes(a)
    ) {
      patterns[a as PatternName] = b;
      declared[a as PatternName] = path.join(module, c);
    } else if (type === "read-leaf" && fields.length === 1) reads.leaves[a] ??= {};
    else if (type === "read" && fields.length === 3 && ["0", "1"].includes(c)) {
      reads.leaves[a] ??= {};
      reads.leaves[a][b] = c === "1";
    } else if (type === "api-read" && fields.length === 2 && ["0", "1"].includes(b))
      reads.api[a] = b === "1";
    else if (type === "graphql-read" && fields.length === 2 && ["0", "1"].includes(b))
      reads.graphql[a] = b === "1";
    else if (type === "override-header" && fields.length === 1) reads.overrides.push(a);
    else
      throw new Error(
        `gh-selectors printed ${JSON.stringify(line)}, which is no line of its table`
      );
  }
  const missing = PATTERNS.filter(
    (name) => name !== REPOSITORY_NUMBER && patterns[name] === undefined
  );
  if (missing.length > 0 || Object.keys(slots).length === 0 || Object.keys(options).length === 0)
    throw new Error(
      `gh-selectors printed no ${missing.length > 0 ? `pattern ${missing.join(", ")}` : "argument or option kinds"}, so the wrapper's table is not read whole`
    );
  if (
    Object.keys(reads.leaves).length === 0 ||
    Object.keys(reads.api).length === 0 ||
    reads.overrides.length === 0
  )
    throw new Error(
      "gh-selectors printed no read grammar (ghReadLeaves, ghAPIReadOptions and ghMethodOverrideHeaders), so which call writes nothing is not read"
    );
  const reading = Object.entries(quirks)
    .filter(([, { fields }]) => fields.readsRepositoryNumber === "true")
    .map(([leaf]) => leaf);
  if ((patterns[REPOSITORY_NUMBER] === undefined) !== (reading.length === 0))
    throw new Error(
      reading.length === 0
        ? `the wrapper declares ${REPOSITORY_NUMBER} and no leaf's quirk reads it (readsRepositoryNumber), which is neither shape of its OWNER/REPO#N this reads: both before #4424, neither since`
        : `the wrapper's ghLeafQuirks read OWNER/REPO#N for ${reading.join(", ")} (readsRepositoryNumber) and it declares no ${REPOSITORY_NUMBER}, which is neither shape of its OWNER/REPO#N this reads: both before #4424, neither since`
    );
  return {
    slots,
    options,
    quirks,
    ranks,
    patterns: patterns as Patterns,
    declared: declared as Patterns,
    reads,
  };
}

/** A pattern as the wrapper spells it, compiled: Go's leading `(?i)` is JavaScript's `i` flag. */
function compiled(pattern: string): RegExp {
  return pattern.startsWith("(?i)") ? new RegExp(pattern.slice(4), "i") : new RegExp(pattern);
}

/** One argument a leaf's usage names (gh_help.go's ghSlot): its names, the kind of its most
 * specific name (`kind`), the first of its names the table does not hold (`unknown`, "" for
 * none), the kinds of all its names (`takes`), and whether it takes more than one argument
 * (`many`). */
const Slot = z.object({
  names: z.array(z.string()),
  kind: z.string(),
  unknown: z.string(),
  takes: z.array(z.string()),
  many: z.boolean(),
});
type Slot = z.infer<typeof Slot>;

/** One option of a leaf (gh_help.go's ghFlag): whether it takes a value, only after `=`, or its
 * help cannot say, the placeholder its help names that value by, and what the value selects
 * (ghOptionKind). */
const Flag = z.object({
  value: z.boolean(),
  optional: z.boolean(),
  uncertain: z.boolean(),
  placeholder: z.string(),
  selects: z.string(),
});
type Flag = z.infer<typeof Flag>;

/** One leaf of gh: its usage's arguments, whether it passes those after `--` on, its options by
 * long name and shorthand, whether it reads GH_REPO for its own repository, and its quirk. */
const Leaf = z.object({
  slots: z.array(Slot),
  passthrough: z.boolean(),
  flags: z.record(z.string(), Flag),
  shorts: z.record(z.string(), z.string()),
  readsGHRepo: z.boolean(),
  readsAllRemotes: z.boolean(),
  readsRepositoryNumber: z.boolean(),
  pullRequestFinder: z.boolean(),
  targetOption: z.string(),
});
type Leaf = z.infer<typeof Leaf>;

/** A label's selectors: every command of its gh, a group or a leaf, by its path's words joined
 * by a space; every cobra alias its helps list, by its spelling, the path it stands for; the
 * wrapper's patterns of a value, OWNER/REPO#N's only where its wrapper declares it
 * (`REPOSITORY_NUMBER`); and, where the wrapper runs a call that writes nothing and names only
 * owners it holds no directory for with the pane's own GH_CONFIG_DIR rather than refusing it
 * (main's since #4035, read off the label's source by daemon-pane.ts's `labelGhSelectors`), its
 * grammar of such a call (`paneToken`, gh-pane-token.ts); absent for a wrapper that refuses every
 * such call, main's before #4035. This is the file rig.sh writes into a run (RUN_SELECTORS). */
export const GhSelectors = z.object({
  groups: z.array(z.string()),
  leaves: z.record(z.string(), Leaf),
  aliases: z.record(z.string(), z.string()),
  patterns: z.object({
    ghRepositoryNumber: z.string().optional(),
    ghPullRequestPath: z.string(),
    ghQualifier: z.string(),
    ghPlaceholder: z.string(),
    ownerLogin: z.string(),
  }),
  paneToken: ReadGrammar.optional(),
});
export type GhSelectors = z.infer<typeof GhSelectors>;

/** The selectors of the run under `runDir` (RUN_SELECTORS), which its world's gh refused an owner
 * by and health reads its refusals by, or undefined for a run that holds none. Throws naming the
 * file when it is there and holds no selectors. */
export function runSelectors(runDir: string): GhSelectors | undefined {
  const file = path.join(runDir, RUN_SELECTORS);
  if (!existsSync(file)) return undefined;
  let text: unknown;
  try {
    text = JSON.parse(readFileSync(file, "utf8"));
  } catch (error) {
    throw new Error(
      `${file} cannot be read: ${error instanceof Error ? error.message : String(error)}`
    );
  }
  const read = GhSelectors.safeParse(text);
  if (!read.success) throw new Error(`${file} holds no gh selectors: ${read.error.message}`);
  return read.data;
}

/** What one help says of its command, as gh_help.go's parseGhHelp reads it: the path its USAGE
 * names, the arguments after it, its option lines and its inherited ones, the aliases its ALIASES
 * section lists, and whether it is a group. */
interface Help {
  readonly path: readonly string[];
  readonly args: string;
  readonly flags: readonly string[];
  readonly inherited: readonly string[];
  readonly aliases: readonly string[];
  readonly group: boolean;
  readonly listed: { readonly aliases: readonly string[]; readonly extensions: readonly string[] };
}

/** Whether a USAGE line's arguments after the command's path are a group's (gh_help.go's
 * ghUsageNamesSubcommands). */
const namesSubcommands = (args: string) =>
  args.startsWith("<command>") ||
  args.startsWith("[subcommand]") ||
  args.startsWith("<subcommand>");

/** `text`, a saved help, read as parseGhHelp reads gh's (its sections, USAGE's first line). */
function helpOf(text: string, word: RegExp): Help | undefined {
  const lines = text.split("\n");
  let usage = "";
  let section = "";
  let group = false;
  const flags: string[] = [];
  const inherited: string[] = [];
  const aliases: string[] = [];
  const listed = { aliases: [] as string[], extensions: [] as string[] };
  for (const [at, line] of lines.entries()) {
    if (line === "" || line[0] === " " || line[0] === "\t") {
      const entry = line.trim();
      if (entry === "") continue;
      if (section === "FLAGS" && entry.startsWith("-")) flags.push(line);
      else if (section === "INHERITED FLAGS" && entry.startsWith("-")) inherited.push(line);
      else if (section === "ALIASES") aliases.push(...entry.split(",").map((one) => one.trim()));
      else if (section === "ALIAS COMMANDS") listed.aliases.push(entry.split(":")[0] ?? "");
      else if (section === "EXTENSION COMMANDS") listed.extensions.push(entry.split(":")[0] ?? "");
      continue;
    }
    section = line.trim();
    if (section.endsWith("COMMANDS")) group = true;
    if (section === "USAGE" && usage === "")
      usage =
        lines
          .slice(at + 1)
          .find((next) => next.trim() !== "")
          ?.trim() ?? "";
  }
  const words = usage.split(/\s+/).filter((one) => one !== "");
  if (words[0] !== "gh") return undefined;
  let n = 1;
  while (n < words.length && word.test(words[n] ?? "")) n++;
  const args = words.slice(n).join(" ");
  return {
    path: words.slice(1, n),
    args,
    flags,
    inherited,
    aliases,
    group: group || namesSubcommands(args),
    listed,
  };
}

/** Every section of gh's reference (reference.txt), as the wrapper's own tests read it
 * (gh_test.go's ghReferenceUsages): each `#…# gh <path> <args>` heading, the option lines under
 * it, and the aliases its `Aliases` paragraph lists. */
function referenceOf(text: string, word: RegExp, flagLine: RegExp): Help[] {
  const sections: { path: string[]; args: string; flags: string[]; aliases: string[] }[] = [];
  let aliasesNext = false;
  for (const line of text.split("\n")) {
    if (line.startsWith("#")) {
      aliasesNext = false;
      const words = line
        .replace(/^#+/, "")
        .trim()
        .replace(/^`+|`+$/g, "")
        .split(/\s+/)
        .filter((one) => one !== "");
      if (words.length < 2 || words[0] !== "gh") continue;
      let n = 1;
      while (n < words.length && word.test(words[n] ?? "")) n++;
      sections.push({
        path: words.slice(1, n),
        args: words.slice(n).join(" "),
        flags: [],
        aliases: [],
      });
      continue;
    }
    const last = sections.at(-1);
    if (last === undefined) continue;
    if (line === "Aliases") aliasesNext = true;
    else if (aliasesNext && line.trim() !== "") {
      last.aliases.push(...line.split(",").map((one) => one.trim()));
      aliasesNext = false;
    } else if (flagLine.test(line)) last.flags.push(line);
  }
  const joined = sections.map((section) => section.path.join(" "));
  return sections.map((section, at) => {
    const key = joined[at] ?? "";
    return {
      ...section,
      inherited: [],
      group: namesSubcommands(section.args) || joined.some((other) => other.startsWith(`${key} `)),
      listed: { aliases: [], extensions: [] },
    };
  });
}

/** The usage arguments `args` of the leaf `leaf` as slots (gh_help.go's ghSlots): each `<name>`,
 * `[…]` or `{…}` group naming arguments, `[flags]`, an option a usage names and a `[-- …]`
 * passthrough left out, the last marking passthrough; each name's kind the leaf's quirk's or the
 * table's, and the slot's the most specific of them (ghKindRank). */
function slotsOf(
  leaf: string,
  args: string,
  table: GhTable,
  slotName: RegExp
): { slots: Slot[]; passthrough: boolean } {
  const groups: string[] = [];
  let depth = 0;
  let start = -1;
  for (const [at, char] of [...args].entries()) {
    if (char === "[" || char === "{") {
      if (depth === 0) start = at;
      depth++;
    } else if (char === "]" || char === "}") {
      depth--;
      if (depth === 0 && start >= 0) {
        groups.push(args.slice(start, at + 1));
        start = -1;
      }
    } else if (char === " " && depth === 0) {
      if (start >= 0) {
        groups.push(args.slice(start, at));
        start = -1;
      }
    } else if (depth === 0 && start < 0) start = at;
  }
  if (start >= 0) groups.push(args.slice(start));
  const slots: Slot[] = [];
  let passthrough = false;
  const all = new RegExp(slotName.source, "g");
  for (const group of groups) {
    const inner = group.replace(/^[[\]{}]+|[[\]{}]+$/g, "");
    if (group === "[flags]") continue;
    if (inner.startsWith("-- ") || inner === "--") {
      passthrough = true;
      continue;
    }
    if (inner.startsWith("-")) continue;
    let names = [...group.matchAll(all)].map((match) => match[1] ?? "");
    if (names.length === 0)
      names = inner
        .replaceAll("|", " ")
        .split(/\s+/)
        .filter((one) => one !== "");
    const kinds: (ArgKind | undefined)[] = names.map(
      (name) => table.quirks[leaf]?.slots[name] ?? table.slots[name]
    );
    const unknownAt = kinds.indexOf(undefined);
    let kind = "";
    for (const one of kinds)
      if (one !== undefined && (table.ranks[one.kind] ?? 0) > (table.ranks[kind] ?? 0))
        kind = one.kind;
    slots.push({
      names,
      kind: unknownAt >= 0 ? "" : kind,
      unknown:
        names.length === 0
          ? "an argument with no name"
          : unknownAt >= 0
            ? (names[unknownAt] ?? "")
            : "",
      takes: kinds.flatMap((one) => (one === undefined ? [] : [one.kind])),
      many: group.includes("...") || kinds.some((one) => one?.many === true),
    });
  }
  return { slots, passthrough };
}

/** One option line of a help as the wrapper reads it (gh_help.go's addFlag): its shorthand, its
 * name, the placeholder of its value ("" for none) and its description. */
export interface OptionLine {
  readonly short: string;
  readonly name: string;
  readonly placeholder: string;
  readonly description: string;
}
/** The reader of an option line under the wrapper's pattern `flagLine` (gh_help.go's
 * ghFlagLine and addFlag), read from the file `file`, which answers undefined for a line the
 * pattern does not read. The pattern has had two shapes, told apart by its capture groups: 4
 * before #4418 — the shorthand, the name, the placeholder and the description — and 5 since, with
 * the boolean marker `[=true|false]` gh 2.104.0 prints right after a boolean option's name as the
 * third. A pattern of any other count throws here, naming it, its file, its count and the two
 * counts this reads, and no line is read by it as either shape; a line that marks its option
 * boolean and names a value, which the wrapper refuses, throws when it is read. */
export function optionLines(
  flagLine: RegExp,
  file: string
): (line: string) => OptionLine | undefined {
  const groups = (new RegExp(`${flagLine.source}|`).exec("")?.length ?? 1) - 1;
  if (groups !== 4 && groups !== 5)
    throw new Error(
      `the wrapper's ghFlagLine ${JSON.stringify(flagLine.source)}, read from ${file}, has ${groups} capture groups, which is neither shape of its option line this reads: 4 before #4418 (shorthand, name, placeholder, description), or 5 since (the boolean marker after the name third)`
    );
  return (line) => {
    const match = flagLine.exec(line);
    if (match === null) return undefined;
    const [, short = "", name = "", ...rest] = match;
    const [marker = "", placeholder = "", description = ""] = groups === 5 ? rest : ["", ...rest];
    if (marker !== "" && placeholder !== "")
      throw new Error(
        `the wrapper refuses the option line ${JSON.stringify(line.trim())}, which marks its option boolean and names a value (gh_help.go's addFlag)`
      );
    return { short, name, placeholder, description };
  };
}

/** The option lines `lines` of the leaf `leaf` (gh_help.go's addFlag and ghOptionKind): each
 * option's arity, and what its value selects, the table's kind keyed `<leaf> --<name>
 * <placeholder>` before `--<name> <placeholder>`, "unknown" for one whose help mentions a
 * selector the table does not hold, "" for any other, "nothing" for one taking no value. A line
 * `option` does not read is left out, as no option of this leaf (`optionLines`). */
function flagsOf(
  leaf: string,
  lines: readonly string[],
  table: GhTable,
  option: (line: string) => OptionLine | undefined,
  mentions: RegExp
): { flags: Record<string, Flag>; shorts: Record<string, string> } {
  const flags: Record<string, Flag> = {};
  const shorts: Record<string, string> = {};
  for (const line of lines) {
    const read = option(line);
    if (read === undefined) continue;
    const { short, name, description } = read;
    let { placeholder } = read;
    let optional = false;
    let uncertain = false;
    const value = placeholder !== "";
    if (value) {
      const cut = placeholder.indexOf("[=");
      if (cut > 0) {
        optional = true;
        placeholder = placeholder.slice(0, cut);
      }
      if (placeholder.startsWith("-") || placeholder === "count") uncertain = true;
    }
    const key = `--${name} ${placeholder}`;
    const selects = !value
      ? "nothing"
      : (table.options[`${leaf} ${key}`] ??
        table.options[key] ??
        (mentions.test(description) || mentions.test(placeholder) ? "unknown" : ""));
    flags[name] = { value, optional, uncertain, placeholder, selects };
    if (short !== "") shorts[short] = name;
  }
  return { flags, shorts };
}

/** Every command of a label's gh, joined with the wrapper's table `table`, from the help the
 * label saves for the wrapper's tests: `saved`, each file of testdata/gh-help by its name (its
 * path's words joined by `_`, `gh.txt` gh's own), and `reference`, gh's reference. A leaf with a
 * saved help is read from it whole, as the wrapper reads gh's; any other from its reference
 * section, its options and `--help`, with the options of its nearest parent group that has a
 * saved help or lists options in the reference (gh's persistent `-R`). A gh alias or an extension
 * gh's own help lists is no command here: the wrapper refuses it before reading anything.
 * `paneToken` says whether the wrapper runs a call that writes nothing and names only owners it
 * holds no directory for with the pane's own GH_CONFIG_DIR (main's since #4035), and its read
 * grammar is then carried whole; a wrapper of that shape that declares no `gh api graphql` query's
 * options is refused, since it reads such a query by them (gh_reads.go's ghGraphQLQueryRead). */
export function selectorsOf(
  table: GhTable,
  saved: Readonly<Record<string, string>>,
  reference: string,
  paneToken: boolean
): GhSelectors {
  if (paneToken && Object.keys(table.reads.graphql).length === 0)
    throw new Error(
      "the wrapper runs a read naming only owners it holds no directory for with the pane's own GH_CONFIG_DIR, and declares no ghGraphQLReadOptions, which it reads a `gh api graphql` query by"
    );
  const word = compiled(table.patterns.ghCommandWord);
  const flagLine = compiled(table.patterns.ghFlagLine);
  const option = optionLines(flagLine, table.declared.ghFlagLine);
  const slotName = compiled(table.patterns.ghSlotName);
  const mentions = compiled(table.patterns.ghMentionsSelector);
  const savedHelp = (leaf: readonly string[]) => {
    const text = saved[`${leaf.length === 0 ? "gh" : leaf.join("_")}.txt`];
    const help = text === undefined ? undefined : helpOf(text, word);
    return help !== undefined && help.path.join(" ") === leaf.join(" ") ? help : undefined;
  };
  const own = savedHelp([]);
  const refused = new Set([...(own?.listed.aliases ?? []), ...(own?.listed.extensions ?? [])]);
  const sections = referenceOf(reference, word, flagLine).filter(
    (section) => !refused.has(section.path[0] ?? "")
  );
  const groups: string[] = [];
  const leaves: Record<string, Leaf> = {};
  const aliases: Record<string, string> = {};
  for (const section of sections) {
    const leaf = section.path.join(" ");
    const help = savedHelp(section.path) ?? section;
    for (const alias of new Set([...section.aliases, ...help.aliases])) {
      const spelled = alias.split(/\s+/);
      if (spelled[0] === "gh" && spelled.length > 1) aliases[spelled.slice(1).join(" ")] = leaf;
    }
    if (section.group || help.group) {
      groups.push(leaf);
      continue;
    }
    let inherited = help.inherited;
    if (help === section) {
      inherited = ["  --help   Show help for command"];
      for (let depth = section.path.length - 1; depth > 0; depth--) {
        const parent = section.path.slice(0, depth);
        const parentHelp =
          savedHelp(parent) ?? sections.find((one) => one.path.join(" ") === parent.join(" "));
        const lines = [...(parentHelp?.flags ?? []), ...(parentHelp?.inherited ?? [])];
        if (lines.length > 0) {
          inherited = [...inherited, ...lines];
          break;
        }
      }
    }
    const { flags, shorts } = flagsOf(leaf, [...inherited, ...help.flags], table, option, mentions);
    const { slots, passthrough } = slotsOf(leaf, help.args, table, slotName);
    const quirk = table.quirks[leaf]?.fields ?? {};
    leaves[leaf] = {
      slots,
      passthrough,
      flags,
      shorts,
      readsGHRepo: flags.repo?.placeholder === "[HOST/]OWNER/REPO" || quirk.readsGHRepo === "true",
      readsAllRemotes: quirk.readsAllRemotes === "true",
      readsRepositoryNumber: quirk.readsRepositoryNumber === "true",
      // gh_help.go's quirkOf: every pr leaf reads a URL through gh's pr finder.
      pullRequestFinder: quirk.pullRequestFinder === "true" || section.path[0] === "pr",
      targetOption: quirk.targetOption ?? "",
    };
  }
  return {
    groups,
    leaves,
    aliases,
    patterns: {
      ...(table.patterns.ghRepositoryNumber === undefined
        ? {}
        : { ghRepositoryNumber: table.patterns.ghRepositoryNumber }),
      ghPullRequestPath: table.patterns.ghPullRequestPath,
      ghQualifier: table.patterns.ghQualifier,
      ghPlaceholder: table.patterns.ghPlaceholder,
      ownerLogin: table.patterns.ownerLogin,
    },
    ...(paneToken ? { paneToken: table.reads } : {}),
  };
}

/** What the wrapper reads of a call (gh.go's ghTarget): every owner it names, each with the
 * argument that named it first, in the order the wrapper meets them, and the one repository every
 * selector names, "" where a selector names its owner alone, two repositories are named, or none. */
export interface GhTarget {
  readonly owners: readonly (readonly [owner: string, what: string])[];
  readonly repo: string;
}

/** The wrapper's refusal of an argument it cannot place, which this reading does not model: the
 * call is not read (`ghTargetOf` answers undefined). */
class Unplaced extends Error {}

/** A leaf's arguments as pflag reads them (gh.go's ghCommand.parse): its positional arguments,
 * the values of its options by long name, and whether it only prints its help. */
interface Parsed {
  positionals: string[];
  values: Record<string, string[]>;
  help: boolean;
}

/** Go's strconv.ParseBool. */
function parseBool(value: string): boolean {
  if (["1", "t", "T", "TRUE", "true", "True"].includes(value)) return true;
  if (["0", "f", "F", "FALSE", "false", "False"].includes(value)) return false;
  throw new Unplaced(`${value} is no boolean`);
}

function parseArgs(leaf: Leaf, args: readonly string[]): Parsed {
  const parsed: Parsed = { positionals: [], values: {}, help: false };
  const set = (name: string, flag: Flag, value: string) => {
    if (name === "help") parsed.help = parseBool(value);
    else if (flag.value) {
      parsed.values[name] ??= [];
      parsed.values[name].push(value);
    }
  };
  for (let i = 0; i < args.length; i++) {
    const arg = args[i] ?? "";
    if (arg === "--") {
      let rest = args.slice(i + 1);
      if (leaf.passthrough && parsed.positionals.length > 0) rest = [];
      else if (leaf.passthrough && rest.length > 1) rest = rest.slice(0, 1);
      parsed.positionals.push(...rest);
      return parsed;
    }
    if (arg.startsWith("--")) {
      const equals = arg.indexOf("=");
      const name = equals < 0 ? arg.slice(2) : arg.slice(2, equals);
      let value: string | undefined = equals < 0 ? undefined : arg.slice(equals + 1);
      const flag = leaf.flags[name];
      if (flag === undefined || flag.uncertain) throw new Unplaced(arg);
      if (value === undefined) {
        if (!flag.value) value = "true";
        else if (flag.optional) value = undefined;
        else if (i + 1 === args.length) throw new Unplaced(arg);
        else value = args[++i] ?? "";
      }
      if (value !== undefined) set(name, flag, value);
    } else if (arg.startsWith("-") && arg !== "-") {
      let shorts = arg.slice(1);
      while (shorts !== "") {
        const short = shorts[0] ?? "";
        let rest = shorts.slice(1);
        const name = leaf.shorts[short];
        if (name === undefined) {
          if (short === "h") {
            parsed.help = true;
            return parsed;
          }
          throw new Unplaced(arg);
        }
        const flag = leaf.flags[name];
        if (flag === undefined || flag.uncertain) throw new Unplaced(arg);
        let value: string | undefined;
        if (rest.length > 1 && rest[0] === "=") {
          value = rest.slice(1);
          rest = "";
        } else if (!flag.value) value = "true";
        else if (flag.optional) value = undefined;
        else if (rest !== "") {
          value = rest;
          rest = "";
        } else if (i + 1 < args.length) value = args[++i] ?? "";
        else throw new Unplaced(arg);
        if (value !== undefined) set(name, flag, value);
        shorts = rest;
      }
    } else parsed.positionals.push(arg);
  }
  return parsed;
}

/** The index in `leaf.slots` of the slot each of `n` positional arguments fills (gh_help.go's
 * slotIndexes), or undefined where the usage has no room for them. */
function slotIndexes(leaf: Leaf, n: number): number[] | undefined {
  const many = leaf.slots.findIndex((slot) => slot.many);
  if (many < 0 && n > leaf.slots.length) return undefined;
  const after = leaf.slots.length - many - 1;
  return Array.from({ length: n }, (_, i) => {
    if (many < 0 || i < many) return i;
    if (i >= Math.max(n - after, many + 1)) return leaf.slots.length - (n - i);
    return many;
  });
}

/** Every owner and repository the selectors of one call name (gh_selectors.go's selectors). */
class Selected {
  readonly owners = new Map<string, string>();
  readonly repos = new Set<string>();
  ownerOnly = false;
  targeted = false;
  readsBase = false;
  constructor(
    readonly patterns: {
      readonly placeholder: RegExp;
      readonly qualifier: RegExp;
      readonly login: RegExp;
    }
  ) {}
  add(owner: string, what: string) {
    if (!this.owners.has(owner)) this.owners.set(owner, what);
  }
  /** A repository named as gh names one (gh_selectors.go's repositoryTarget): OWNER/REPO,
   * HOST/OWNER/REPO or a URL, of github.com or a subdomain, its owner a GitHub login. */
  target(value: string): [owner: string, repo: string] {
    let host = "";
    let rest = value;
    let url: URL | undefined;
    try {
      url = new URL(value);
    } catch {}
    if (url !== undefined && url.protocol !== "" && url.host !== "") {
      host = url.hostname;
      rest = url.pathname.replace(/^\/+|\/+$/g, "");
    } else {
      const parts = value.replace(/^\/+|\/+$/g, "").split("/");
      if (parts.length >= 3) {
        host = parts[0] ?? "";
        rest = parts.slice(1).join("/");
      }
    }
    const lower = host.toLowerCase();
    if (host !== "" && lower !== "github.com" && !lower.endsWith(".github.com"))
      throw new Unplaced(value);
    const [owner = "", repo = ""] = rest.split("/");
    if (owner === "" || repo === "") throw new Unplaced(value);
    return [this.login(owner), repo];
  }
  login(owner: string): string {
    const lower = owner.toLowerCase();
    if (!this.patterns.login.test(lower)) throw new Unplaced(owner);
    return lower;
  }
  repository(value: string, what: string) {
    const [owner, repo] = this.target(value);
    this.add(owner, what);
    // ghconfig.RepositoryName: lowercased, without a `.git`.
    this.repos.add(repo.toLowerCase().replace(/\.git$/, ""));
  }
  ownerAlone(owner: string, what: string) {
    this.add(owner, what);
    this.ownerOnly = true;
  }
  owner(value: string, what: string) {
    this.ownerAlone(this.login(value), what);
  }
  pairOwner(value: string, what: string) {
    this.ownerAlone(this.target(value)[0], what);
  }
  /** A search query's repo:/owner:/user:/org: qualifiers (gh_selectors.go's query). */
  query(words: readonly string[]) {
    let named = "";
    let joined = "";
    for (const word of words) {
      if (!word.includes(":")) {
        if (joined === "" && (/^(or|not)$/i.test(word) || /[()]/.test(word))) joined = word;
        continue;
      }
      const qualifier = this.patterns.qualifier.exec(word);
      if (qualifier === null) throw new Unplaced(word);
      const name = (qualifier[2] ?? "").toLowerCase();
      if (!["repo", "owner", "user", "org"].includes(name)) continue;
      if (qualifier[1] === "-") throw new Unplaced(word);
      if (name === "repo") this.repository(qualifier[3] ?? "", `the qualifier ${word}`);
      else this.owner(qualifier[3] ?? "", `the qualifier ${word}`);
      if (named === "") named = word;
    }
    if (named !== "" && joined !== "") throw new Unplaced(joined);
  }
  /** The owner and repository a gh api endpoint names (gh_selectors.go's endpoint), and whether it
   * is a search endpoint. */
  endpoint(endpoint: string): boolean {
    let full = endpoint;
    if (endpoint.includes("://")) {
      let url: URL;
      try {
        url = new URL(endpoint);
      } catch {
        throw new Unplaced(endpoint);
      }
      if (url.host.toLowerCase() !== "api.github.com") throw new Unplaced(endpoint);
      full = url.pathname + url.search;
    }
    const question = full.replace(/^\//, "").indexOf("?");
    const trimmed = full.replace(/^\//, "");
    const route = question < 0 ? trimmed : trimmed.slice(0, question);
    const rawQuery = question < 0 ? "" : trimmed.slice(question + 1);
    const segments = route.split("/");
    const first = (segments[0] ?? "").toLowerCase();
    if (first === "repositories") throw new Unplaced(endpoint);
    if ((first === "repos" || first === "orgs" || first === "users") && segments.length > 1) {
      const owner = segments[1] ?? "";
      if (this.patterns.placeholder.test(owner)) return false;
      const repo = segments[2] ?? "";
      if (first === "repos" && repo !== "" && !this.patterns.placeholder.test(repo))
        this.repository(`${owner}/${repo}`, `the endpoint ${endpoint}`);
      else this.owner(owner, `the endpoint ${endpoint}`);
      return false;
    }
    if (first !== "search") return false;
    for (const q of new URLSearchParams(rawQuery).getAll("q")) this.query(fields(q));
    return true;
  }
  /** The one repository every selector named (gh_selectors.go's onlyRepository). */
  onlyRepository(): string {
    return this.ownerOnly || this.repos.size !== 1 ? "" : ([...this.repos][0] ?? "");
  }
}

/** Go's strings.Fields. */
const fields = (text: string) => text.split(/\s+/).filter((one) => one !== "");

/** Whether `value` holds a placeholder gh fills from the command's own repository
 * (gh_selectors.go's readsBaseRepository). */
function readsBase(value: string, placeholder: RegExp): boolean {
  const all = new RegExp(placeholder.source, "g");
  return [...value.matchAll(all)].some((match) =>
    [":owner", ":repo", "{owner}", "{repo}"].includes(match[0])
  );
}

/** Whether `value` is an http or https URL, as gh reads a URL argument. */
function isWebURL(value: string): boolean {
  try {
    const url = new URL(value);
    return url.protocol === "http:" || url.protocol === "https:";
  } catch {
    return false;
  }
}

/** What the Legion gh wrapper reads of `argv` (gh's arguments, without `gh`) under `env` by the
 * label's `selectors` (gh.go's ghTargetOf): the owners every selector names and the one
 * repository they name, the command's own repository among them where gh resolves it, GH_REPO's
 * for a leaf that reads it. The working directory's git remotes are not read: a world's workspace's
 * origin names the world's repository (rig.sh's fixture_workspace), which GH_REPO, the one gh
 * reads before any remote, names too in every worker pane. Undefined for a call
 * naming no leaf, one that only prints help or a version, and one this cannot place, which the
 * wrapper refuses for some other reason than its owner (`Unplaced`). */
export function ghTargetOf(
  argv: readonly string[],
  selectors: GhSelectors,
  env: Readonly<Record<string, string | undefined>>
): GhTarget | undefined {
  return ghCallOf(argv, selectors, env)?.target;
}

/** `ghTargetOf`'s reading of a call, with the path of the leaf its words resolved to and whether
 * the wrapper reads the working directory's git remotes for it (gh_selectors.go's
 * fromWorkingDirectory), which in a world name the world's repository (`ghTargetOf`), which
 * gh-pane-token.ts's `runsOnPaneToken` reads a call by. */
export function ghCallOf(
  argv: readonly string[],
  selectors: GhSelectors,
  env: Readonly<Record<string, string | undefined>>
): { target: GhTarget; path: readonly string[]; remotes: boolean } | undefined {
  const rest: string[] = [];
  let leafPath: string[] = [];
  let leaf: Leaf | undefined;
  for (let i = 0; i < argv.length && leaf === undefined; i++) {
    const arg = argv[i] ?? "";
    if (leafPath.length === 0 && (arg === "--version" || arg === "help")) return undefined;
    if (arg === "-R" || arg === "--repo") {
      if (i + 1 === argv.length) return undefined;
      rest.push(arg, argv[++i] ?? "");
    } else if (
      arg.startsWith("--repo=") ||
      (arg.startsWith("-R") && arg.length > 2) ||
      arg === "--help" ||
      arg.startsWith("--help=") ||
      arg === "-h"
    )
      rest.push(arg);
    else if (arg.startsWith("-")) return undefined;
    else {
      const spelled = [...leafPath, arg].join(" ");
      const named = selectors.aliases[spelled] ?? spelled;
      leafPath = named.split(" ");
      leaf = selectors.leaves[named];
      if (leaf !== undefined) rest.push(...argv.slice(i + 1));
      else if (!selectors.groups.includes(named)) return undefined;
    }
  }
  if (leaf === undefined) return undefined;
  const patterns = {
    placeholder: new RegExp(selectors.patterns.ghPlaceholder),
    qualifier: new RegExp(selectors.patterns.ghQualifier),
    login: new RegExp(selectors.patterns.ownerLogin),
  };
  const api = leafPath.join(" ") === "api";
  const s = new Selected(patterns);
  let remotes = false;
  try {
    const parsed = parseArgs(leaf, rest);
    if (parsed.help) return undefined;
    fromCommand(s, leaf, parsed, api, selectors);
    remotes = fromOwnRepository(s, leaf, parsed, api, env);
  } catch (error) {
    if (error instanceof Unplaced) return undefined;
    throw error;
  }
  return {
    target: { owners: [...s.owners], repo: s.owners.size === 1 ? s.onlyRepository() : "" },
    path: leafPath,
    remotes,
  };
}

/** gh_selectors.go's fromCommand: the owners the leaf's arguments and options name. */
function fromCommand(
  s: Selected,
  leaf: Leaf,
  parsed: Parsed,
  api: boolean,
  selectors: GhSelectors
) {
  s.targeted = (parsed.values.repo?.length ?? 0) > 0 && leaf.readsGHRepo;
  if (leaf.targetOption !== "" && (parsed.values[leaf.targetOption]?.length ?? 0) > 0)
    s.targeted = true;
  if (api) fromAPI(s, parsed);
  else {
    const indexes = slotIndexes(leaf, parsed.positionals.length);
    if (indexes === undefined) throw new Unplaced("an argument the usage has no place for");
    // OWNER/REPO#N is read only by a wrapper that declares its pattern, main's before #4424, and
    // only for a leaf whose quirk reads it (`REPOSITORY_NUMBER`).
    const number =
      selectors.patterns.ghRepositoryNumber === undefined
        ? undefined
        : new RegExp(selectors.patterns.ghRepositoryNumber);
    const pullRequest = new RegExp(selectors.patterns.ghPullRequestPath);
    const query: string[] = [];
    for (const [i, arg] of parsed.positionals.entries()) {
      const slot = leaf.slots[indexes[i] ?? -1];
      if (slot === undefined || slot.unknown !== "") throw new Unplaced(arg);
      const repositoryNumber = number?.exec(arg) ?? null;
      if (
        slot.takes.includes("number") &&
        leaf.readsRepositoryNumber &&
        repositoryNumber !== null
      ) {
        s.repository(repositoryNumber[1] ?? "", `the issue or pull request ${arg}`);
        s.targeted = true;
        continue;
      }
      switch (slot.kind) {
        case "repository":
          s.repository(arg, `the repository ${arg}`);
          s.targeted = true;
          break;
        case "new-repository":
          if (arg.includes("/")) {
            s.repository(arg, `the repository ${arg}`);
            s.targeted = true;
          }
          break;
        case "second-repository":
          s.repository(arg, `the repository ${arg}`);
          break;
        case "owner":
          s.owner(arg, `the owner ${arg}`);
          s.targeted = true;
          break;
        case "url":
          if (isWebURL(arg)) {
            s.repository(arg, `the URL ${arg}`);
            if (!leaf.pullRequestFinder || pullRequest.test(new URL(arg).pathname))
              s.targeted = true;
          }
          break;
        case "query":
          query.push(arg);
          break;
      }
    }
    s.query(query);
  }
  fromOptions(s, leaf, parsed, api);
}

/** gh_selectors.go's fromAPI: gh api's endpoint, a GraphQL query's owner variable and a search
 * endpoint's q field. */
function fromAPI(s: Selected, parsed: Parsed) {
  if (parsed.positionals.length > 1) throw new Unplaced(parsed.positionals[1] ?? "");
  let search = false;
  let graphql = false;
  const placeholder = s.patterns.placeholder;
  const endpoint = parsed.positionals[0];
  if (endpoint !== undefined) {
    s.readsBase = readsBase(endpoint, placeholder);
    graphql = endpoint === "graphql";
    search = s.endpoint(endpoint);
  }
  for (const option of ["field", "raw-field"]) {
    for (const field of parsed.values[option] ?? []) {
      const equals = field.indexOf("=");
      const key = equals < 0 ? field : field.slice(0, equals);
      const value = equals < 0 ? "" : field.slice(equals + 1);
      if (option === "field" && readsBase(value, placeholder)) s.readsBase = true;
      if (
        graphql &&
        ["owner", "org", "organization", "login"].includes(key) &&
        !placeholder.test(value)
      )
        s.owner(value, `the field ${field}`);
      else if (search && key === "q") {
        if (option === "field" && value.startsWith("@")) throw new Unplaced(field);
        s.query(fields(value));
      }
    }
  }
}

/** gh_selectors.go's fromOptions: the owners the leaf's options name, by what each selects. */
function fromOptions(s: Selected, leaf: Leaf, parsed: Parsed, api: boolean) {
  const orgs = parsed.values.org ?? [];
  for (const name of Object.keys(parsed.values).sort()) {
    if (api && (name === "field" || name === "raw-field")) continue;
    const selects = leaf.flags[name]?.selects ?? "";
    for (const value of parsed.values[name] ?? []) {
      if (selects === "unknown") throw new Unplaced(`--${name} ${value}`);
      if (selects === "" || selects === "nothing") continue;
      for (const item of value.split(",")) {
        const what = `--${name} ${item}`;
        if (selects === "host") {
          if (item.toLowerCase() !== "github.com") throw new Unplaced(what);
        } else if (selects === "owner") s.owner(item, what);
        else if (selects === "org-repositories") {
          if (item.includes("/")) s.repository(item, what);
          else if (orgs.length === 0) throw new Unplaced(what);
        } else if (selects === "repository") s.repository(item, what);
        else if (selects === "owner/name") s.pairOwner(item, what);
        else if (selects === "url" && isWebURL(item)) s.repository(item, what);
      }
    }
  }
}

/** gh_selectors.go's fromOwnRepository: the command's own repository, where gh resolves it, which
 * here is GH_REPO's for a leaf that reads it (or gh api's base placeholders), the working
 * directory's remotes naming none. True where the wrapper reads the working directory's remotes
 * for it (fromWorkingDirectory): a leaf whose quirk reads every remote, and a call reading its own
 * repository, or naming no owner, that GH_REPO does not answer. */
function fromOwnRepository(
  s: Selected,
  leaf: Leaf,
  parsed: Parsed,
  api: boolean,
  env: Readonly<Record<string, string | undefined>>
): boolean {
  if (leaf.readsAllRemotes) return true;
  const n = parsed.positionals.length;
  const indexes = slotIndexes(leaf, n) ?? [];
  const readsOwn =
    leaf.readsGHRepo ||
    leaf.slots.some(
      (slot, index) =>
        (slot.takes.includes("repository") || slot.takes.includes("second-repository")) &&
        !indexes.includes(index)
    );
  const reads = api ? s.readsBase : !s.targeted && readsOwn;
  if (!reads && s.owners.size > 0) return false;
  const repo = env.GH_REPO ?? "";
  if (repo !== "" && (leaf.readsGHRepo || (api && s.readsBase))) {
    s.repository(repo, `GH_REPO ${repo}`);
    return false;
  }
  return true;
}

/** Whether `target` names an owner other than the owner of the world's repository `repo`: the
 * world's project holds that one owner alone, so the wrapper refuses every other. */
export function namesOwnerOutside(target: GhTarget | undefined, repo: string): boolean {
  const world = (repo.split("/")[0] ?? "").toLowerCase();
  return target?.owners.some(([owner]) => owner !== world) ?? false;
}

/** The line the Legion gh wrapper prints, refusing with exit 2 a call whose `target` names an
 * owner outside the project of the world's repository `repo`, with LEGION_GH_OWNERS_DIR
 * `ownersDir`; undefined for a call naming none. Its two readings of one owner, each measured on a
 * Legion pod on 2026-11-16:
 *   - a call naming one of that owner's repositories and no other (`gh repo view actions/checkout`,
 *     `gh release view v7.0.1 -R actions/checkout`, `gh api repos/actions/checkout/…`): the leak
 *     guard reads that repository's visibility through the wrapper, which refuses the read, and
 *     the guard's setup error refuses the call (gh.go's runGh, leakguard's SetupFailure);
 *   - a call naming the owner alone (`gh search repos checkout --owner actions`, `gh repo list
 *     actions`): the guard has no repository to read, and the wrapper's own lookup of the owner's
 *     gh directory refuses it (gitcredential.go's repositoryGHConfigDir).
 * A call naming two owners, the world's own among them where gh reads GH_REPO, is refused before
 * either, naming each with what named it (ghTargetOf's "more than one owner"). */
export function ownerRefusal(
  target: GhTarget | undefined,
  repo: string,
  ownersDir: string
): string | undefined {
  if (target === undefined || !namesOwnerOutside(target, repo)) return undefined;
  const [first] = target.owners;
  if (target.owners.length > 1 || first === undefined) {
    const listed = target.owners.map(([owner, what]) => `${owner} (${what})`).sort();
    return `legion gh: the command names more than one owner, ${listed.join(" and ")}, so it sends no token; name one repository\n`;
  }
  const [owner] = first;
  const notOwner = `legion gh: "${owner}" is not an owner of this project: ${ownersDir} holds no gh directory for it`;
  if (target.repo === "") return `${notOwner}\n`;
  const slug = `${owner}/${target.repo}`;
  return `legion gh: LEAK CHECK SETUP ERROR: the gh call: the visibility of ${slug} could not be read: gh api repos/${slug} exited 2: ${notOwner}\n`;
}
