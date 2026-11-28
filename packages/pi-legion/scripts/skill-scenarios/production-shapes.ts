// The shapes of production's answers to the reads the world answers, which a row holds a world's
// answer to: production-shapes.json, derived by this module's `derive` from the production reads
// its operator hands it and never written by hand. Each pinned read keeps the command it was read
// with, when it was read, its exit, its stderr and its answer's shape: every JSON path the answer
// holds and the kind of the value there, never the value, and, for an answer that is text (gh's
// text views, a diff, a list's or the checks' rows, `gh auth status`, a refusal's empty stdout),
// the shape of each line too (`lineShapes`). The answers are a public pull request's and
// repository's, so their bodies, logins and ids stay out of the tree; the commands are kept with
// their values written as placeholders (`<owner>`, `<repo>`, `<number>`, `<head>`, `<base>`,
// `<review>`, `<comment>`, `<path>`, `<id>`, `<branch>`, `<app>`), and a derivation that leaves
// one of production's values in the file fails. stderr is gh's own line, and an error body's
// `message`, `documentation_url` and `status` are GitHub's own texts, which the tree keeps.
//
// A row compares a world's answer with its pin (`shapeDiff`): the paths production has that the
// world lacks, the paths the world has that production does not, and the paths whose kinds
// differ. An array the world holds empty is not compared below itself, and nor is a variant of a
// list (a ruleset's rule of one `type`) the world holds none of, since how many items a list holds
// is the world's state and no shape; an array production held empty has no item shape pinned, so
// an item the world holds there is a path production does not show. A text answer's lines are
// compared as a set of line shapes (`lineShapes`): a line shape production's text has that the
// world's lacks is missing, one the world's has that production's does not is extra, and how many
// lines of one shape each holds is no shape.
//
// One read is no gh call: `legion read_record` (`RECORD_READ`), the issue record the `legion`
// tool's `read_record` answered in a Legion issue pod, which the daemon stand-in's record is held
// to (daemon-standin.test.ts). Its two maps keyed by a name of the issue's are pinned as lists
// (`rolesListed`), so the pin names no role and no pair of phases.
//
//   bun production-shapes.ts <ruleset reads dir> <production reads json> <read_record json> \
//     [<later reads json>...] <out json>
//
// writes the pin from the sets of production reads INFRA-764 made, then formats it as the
// repository's biome does: the reads of a branch a ruleset protects (2026-11-19T05:35:00Z, after
// ruleset 12331919 came to require a code owner's review), the distinct reads of round 25, the
// issue record, and each later set of reads (`LaterReads`): the tester's reads of every
// read the world served with no pin (prod-reads-19.json, 2026-11-18), the implementer's of the
// ones that set does not hold (implementer-reads.json, 2026-11-18), and the tester's readings
// again, whole, of every text read (prod-text-reads.json, 2026-11-18), which replace round 25's
// readings cut short. The pin beside this module is made, with the uploads
// `implementer-production-ruleset-reads-2026-11-19-tgz-b64` (dispatch://INFRA-764, its tgz's
// sha256 78d9b6d248c3…, which unpacks to `infra-764-production-ruleset-reads`), version 3 of
// `infra-764-production-reads-tgz-b64` and `implementer-r26-evidence-tgz-b64` each unpacked in
// `<uploads>`, by this, run in this module's directory so that biome reads the repository's
// configuration; a second run writes the same bytes:
//
//   bun production-shapes.ts <uploads>/infra-764-production-ruleset-reads \
//     <uploads>/infra-764-production-reads/infra-764-production-reads.json \
//     <uploads>/production-read-record.json \
//     <uploads>/infra-764-production-reads/prod-reads-19.json <uploads>/implementer-reads.json \
//     <uploads>/infra-764-production-reads/prod-text-reads.json production-shapes.json
import { readdirSync, readFileSync, writeFileSync } from "node:fs";
import path from "node:path";
import { z } from "zod";

/** The pin this directory keeps. */
export const PRODUCTION_SHAPES = path.join(import.meta.dir, "production-shapes.json");

/** An answer's shape: each JSON path it holds (`.` the answer itself, `.key` a field, `[]` an
 * array's items and `[type=x]` its items of one variant), in the order the answer first holds
 * it, and the kinds of JSON value there, `|`-joined in byte order where they differ. */
export type Shape = Record<string, string>;

const PinnedRead = z.object({
  name: z.string(),
  command: z.array(z.string()),
  read_at: z.string(),
  exit: z.number(),
  stderr: z.string(),
  shape: z.record(z.string(), z.string()),
  lines: z.array(z.string()).optional(),
});
/** One read production answered: its name, the command it was read with (its values written as
 * placeholders), when it was read, its exit and stderr, its answer's shape, and, for a text
 * answer, its line shapes (`lineShapes`). */
export type PinnedRead = z.infer<typeof PinnedRead>;
const Pin = z.object({ made_by: z.string(), reads: z.array(PinnedRead) });

let pinned: Map<string, PinnedRead> | undefined;
/** Every read the pin holds, by its name. */
function pin(): Map<string, PinnedRead> {
  pinned ??= new Map(
    Pin.parse(JSON.parse(readFileSync(PRODUCTION_SHAPES, "utf8"))).reads.map((read) => [
      read.name,
      read,
    ])
  );
  return pinned;
}
/** The read the pin names `name`. A name it does not hold throws, naming it. */
export function pinnedRead(name: string): PinnedRead {
  const read = pin().get(name);
  if (read === undefined) throw new Error(`${PRODUCTION_SHAPES} pins no read named ${name}`);
  return read;
}
/** Every read the pin holds, in its order: what a row that holds each read of a kind lists. */
export function pinnedReads(): PinnedRead[] {
  return [...pin().values()];
}

/** `text` with each placeholder (`<name>`) filled from `values`. A placeholder `values` does not
 * fill throws, naming `read`. */
function filled(read: PinnedRead, text: string, values: Readonly<Record<string, string>>): string {
  return text.replace(/<(\w+)>/g, (whole, name: string) => {
    const value = values[name];
    if (value === undefined) throw new Error(`${read.name}: no value for ${whole}`);
    return value;
  });
}

/** `read`'s command, after its `gh`, with each placeholder filled from `values`, so a row reads
 * the world with the very call production was read with. A placeholder `values` does not fill
 * throws. */
export function argvOf(read: PinnedRead, values: Readonly<Record<string, string>>): string[] {
  return read.command.slice(1).map((word) => filled(read, word, values));
}

/** `read`'s stderr with each placeholder filled from `values`: the line gh printed, naming the
 * world's repository, branch and the rest where production's named its own. */
export function stderrOf(read: PinnedRead, values: Readonly<Record<string, string>>): string {
  return filled(read, read.stderr, values);
}

const isObject = (value: unknown): value is Record<string, unknown> =>
  typeof value === "object" && value !== null && !Array.isArray(value);

function kindOf(value: unknown): string {
  if (value === null) return "null";
  if (Array.isArray(value)) return "array";
  return typeof value;
}

/** The fields a list's items are told apart by, as GitHub names a rule's kind and GraphQL an
 * object's. */
const DISCRIMINATORS = ["type", "__typename"];
/** The field `list`, at `at`, groups its items by, or undefined to read them as one. */
type Grouping = (at: string, list: readonly unknown[]) => string | undefined;

const ROOT = ".";

function shapeInto(value: unknown, at: string, into: Shape, grouping: Grouping): void {
  const kinds = new Set(into[at]?.split("|"));
  kinds.add(kindOf(value));
  into[at] = [...kinds].sort().join("|");
  if (Array.isArray(value)) {
    const field = grouping(at, value);
    for (const item of value) {
      const tag =
        field !== undefined && isObject(item) && typeof item[field] === "string"
          ? `[${field}=${item[field]}]`
          : "[]";
      shapeInto(item, `${at}${tag}`, into, grouping);
    }
  } else if (isObject(value))
    for (const [key, inner] of Object.entries(value))
      shapeInto(inner, `${at === ROOT ? "" : at}.${key}`, into, grouping);
}

/** A list's items are read per variant where each names one (`DISCRIMINATORS`) and they do not
 * all have one shape: a ruleset's `deletion` rule holds a `type` alone, its `pull_request` rule
 * the parameters of a review. */
const byVariant: Grouping = (_, list) =>
  DISCRIMINATORS.find(
    (field) =>
      list.every((item) => isObject(item) && typeof item[field] === "string") &&
      new Set(list.map((item) => JSON.stringify(Object.entries(shapeOf(item)).sort()))).size > 1
  );

/** The shape of `value`, as the pin holds production's. */
export function shapeOf(value: unknown): Shape {
  const into: Shape = {};
  shapeInto(value, ROOT, into, byVariant);
  return into;
}

/** A path of a variant: the list's path, the field and its value. */
const VARIANT = /^(.*)\[(\w+)=[^\]]*\]$/;
/** Whether `at` is a path below `above`. */
const under = (at: string, above: string) =>
  at !== above &&
  (above === ROOT || (at.startsWith(above) && /^[.[]/.test(at.slice(above.length))));

/** How gh lays out the text a read answers, by the command it was read with: a text view of
 * `<key>:\t<value>` lines, `--` and a body (`gh pr view`, its `--comments`, and a `gh run`
 * refusal, whose stdout is empty); a diff; a file's path a line (`--name-only`); tab-separated
 * rows (`gh pr list`, `gh pr checks`); `gh auth status`; and a commit's SHA (the `sha` media). */
type Layout = "view" | "diff" | "paths" | "rows" | "auth" | "sha";
function layoutOf(command: readonly string[]): Layout {
  const words = command.join(" ");
  if (command[1] === "auth") return "auth";
  if (command.includes("--name-only")) return "paths";
  if (command[2] === "diff" || words.endsWith(".diff")) return "diff";
  if (words.endsWith(".sha")) return "sha";
  if (command[2] === "list" || command[2] === "checks") return "rows";
  return "view";
}

/** The shape of a line no rule of its layout reads. A production line in this shape fails the
 * derivation: its shape would say nothing of the line. */
const UNREAD = "<unread line>";
/** Each layout's line rules, the first that matches a line giving its shape: gh's and git's own
 * words kept and every value a placeholder. Rows are read by their field count instead. */
const LINE_RULES: Readonly<Record<Exclude<Layout, "rows">, readonly [RegExp, string][]>> = {
  view: [[/^([a-z][a-z-]*):\t.*$/, "$1:\t<value>"]],
  paths: [[/^.+$/, "<path>"]],
  sha: [[/^[0-9a-f]{40}$/, "<sha>"]],
  auth: [
    [/^[a-z0-9.-]+$/, "<host>"],
    [
      /^ {2}✓ Logged in to \S+ account \S+ \(.+\)$/,
      "  ✓ Logged in to <host> account <login> (<source>)",
    ],
    [/^ {2}- ([A-Z][a-z]*(?: [a-z]+)*): .*$/, "  - $1: <value>"],
  ],
  diff: [
    [/^diff --git .*$/, "diff --git a/<path> b/<path>"],
    [/^index [0-9a-f]+\.\.[0-9a-f]+(?: \d+)?$/, "index <blob>..<blob>"],
    [/^(new|deleted) file mode \d+$/, "$1 file mode <mode>"],
    [/^(old|new) mode \d+$/, "$1 mode <mode>"],
    [/^similarity index \d+%$/, "similarity index <percent>"],
    [/^rename (from|to) .*$/, "rename $1 <path>"],
    [/^(---|\+\+\+) .*$/, "$1 <path>"],
    [/^@@ -\d+(?:,\d+)? \+\d+(?:,\d+)? @@.*$/, "@@ -<line> +<line> @@"],
    [/^\\ No newline at end of file$/, "\\ No newline at end of file"],
    [/^Binary files .* differ$/, "Binary files <path> and <path> differ"],
    [/^([ +-]).*$/, "$1<text>"],
  ],
};
/** A text view's `<key>:\t` line, which ends the body a `--` began when it follows the `--` after
 * that body (`gh pr view --comments`, an entry after each comment's closing `--`). */
const VIEW_KEY = /^[a-z][a-z-]*:\t/;

/** The line shapes of `text`, which `command` answered: each distinct shape once, in the order
 * the text first holds it, so many lines of one shape are one. A line of a text view after its
 * `--` is the body, one `<body>` entry, up to a `--` that the end or a `<key>:\t` line follows; a
 * row is `<n tab-separated fields>`; any other line is shaped by its layout's rules
 * (`LINE_RULES`), and one none of them reads is `<unread line>`. */
export function lineShapes(text: string, command: readonly string[]): string[] {
  const layout = layoutOf(command);
  const lines = text.split("\n");
  if (lines.at(-1) === "") lines.pop();
  const shapes = new Set<string>();
  let body = false;
  for (const [at, line] of lines.entries()) {
    if (layout === "view" && line === "--") {
      const next = lines[at + 1];
      if (body && next !== undefined && !VIEW_KEY.test(next)) continue;
      shapes.add("--");
      if (!body) shapes.add("<body>");
      body = !body;
      continue;
    }
    if (body) continue;
    if (layout === "rows") {
      shapes.add(
        line.includes("\t") ? `<${line.split("\t").length} tab-separated fields>` : UNREAD
      );
      continue;
    }
    const rule = LINE_RULES[layout].find(([pattern]) => pattern.test(line));
    shapes.add(rule === undefined ? UNREAD : line.replace(rule[0], rule[1]));
  }
  return [...shapes];
}

/** A text answer of this many characters exactly with no line end is a reading cut short, as the
 * README of version 1 of the production reads says its text answers were cut. */
const CUT_AT = 600;

/** One reading of a read before it is pinned: the read's name, its command with production's
 * values written as placeholders, when it was read, its exit, its stderr and its answer; the read
 * it is a reading of (`readOf`); and the length its set says a text answer was cut at, where its
 * set cut any. */
type Reading = Omit<PinnedRead, "shape" | "lines"> & {
  answer: unknown;
  read: string;
  cutAt: number;
};

/** The read `argv` makes, which every reading of that read shares: its words as they were run,
 * less a `--repo` naming `repo`, the repository its set read, since gh reads that repository
 * there as it reads it from a checkout (round 25's `gh pr view 2827` and the tester's `gh pr view
 * 2827 --repo cybermonklabs/legion` are one read). */
function readOf(argv: readonly string[], repo: string): string {
  const named = (at: number) => argv[at] === "--repo" && argv[at + 1] === repo;
  return JSON.stringify(argv.filter((_, at) => !named(at) && !named(at - 1)));
}

/** `reading` as the pin keeps it: its answer's shape and, for a text answer, the line shapes of
 * it (`lineShapes`). Throws, naming the read, for a text cut short — one at least as long as its
 * set says text answers were cut at, or of `CUT_AT` characters exactly with no line end — whose
 * lines past the cut were never read, and for a line no rule of its layout reads, whose shape
 * would say nothing of production's. */
function pinnedOf(reading: Reading): PinnedRead {
  const { name, command, read_at, exit, stderr, answer } = reading;
  const pinned = { name, command, read_at, exit, stderr, shape: shapeOf(answer) };
  if (typeof answer !== "string") return pinned;
  if (answer.length >= reading.cutAt || (answer.length === CUT_AT && !answer.endsWith("\n")))
    throw new Error(
      `${name}'s answer is cut short at ${answer.length} characters, so lines of it were never read: read it again whole`
    );
  const lines = lineShapes(answer, command);
  if (lines.includes(UNREAD))
    throw new Error(`a line of ${name}'s answer is in no shape its layout reads (${UNREAD})`);
  return { ...pinned, lines };
}

/** How a world's answer differs in shape from `read`'s: the paths production has that the world
 * lacks (`missing`), the paths the world has that production does not (`extra`), each the
 * highest of its kind, and each path whose kinds differ (`kinds`), which is compared no further
 * down. The world's lists are read per variant where production's are. An array the world holds
 * empty, and a variant of a list the world holds none of, is not compared below itself. A text
 * answer of a read pinned with its lines is compared line shape by line shape too
 * (`lineShapes`): each production has that the world's lacks is missing, and each the world's has
 * that production's does not is extra, as `line <shape>`. */
export function shapeDiff(
  answer: unknown,
  read: Pick<PinnedRead, "shape"> & Partial<Pick<PinnedRead, "lines" | "command">>
): { missing: string[]; extra: string[]; kinds: string[] } {
  const given = read.shape;
  const fields = new Map<string, string>();
  for (const at of Object.keys(given)) {
    const [, list, field] = VARIANT.exec(at) ?? [];
    if (list !== undefined && field !== undefined) fields.set(list, field);
  }
  const world: Shape = {};
  shapeInto(answer, ROOT, world, (at) => fields.get(at));
  const paths = Object.keys(world);
  const kinds: string[] = [];
  const differing: string[] = [];
  for (const at of paths) {
    const production = given[at];
    const held = world[at] ?? "";
    if (production === undefined) continue;
    if (held.split("|").every((kind) => production.split("|").includes(kind))) continue;
    kinds.push(`${at}: ${production} in production, ${held} in the world`);
    differing.push(at);
  }
  const empty = paths.filter(
    (at) => world[at]?.split("|").includes("array") && !paths.some((other) => under(other, at))
  );
  const variantAbsent = (at: string) =>
    [...at.matchAll(/\[\w+=[^\]]*\]/g)].some(
      (found) => world[at.slice(0, (found.index ?? 0) + found[0].length)] === undefined
    );
  const highest = (found: string[]) =>
    found.filter((at) => !found.some((above) => under(at, above)));
  const compared = (at: string) => !differing.some((above) => under(at, above));
  const pinned = read.lines ?? [];
  const worldLines =
    read.lines !== undefined && typeof answer === "string"
      ? lineShapes(answer, read.command ?? [])
      : pinned;
  return {
    missing: [
      ...highest(
        Object.keys(given).filter(
          (at) =>
            world[at] === undefined &&
            compared(at) &&
            !empty.some((above) => under(at, above)) &&
            !variantAbsent(at)
        )
      ),
      ...pinned.filter((line) => !worldLines.includes(line)).map((line) => `line ${line}`),
    ],
    extra: [
      ...highest(paths.filter((at) => given[at] === undefined && compared(at))),
      ...worldLines.filter((line) => !pinned.includes(line)).map((line) => `line ${line}`),
    ],
    kinds,
  };
}

/** The command each ruleset read was made with (`gh api <endpoint>`), by the name of its files. */
const RULESET_READS: Readonly<Record<string, string>> = {
  "rules-branches-main": "repos/<owner>/<repo>/rules/branches/main",
  rulesets: "repos/<owner>/<repo>/rulesets",
  "ruleset-<id>": "repos/<owner>/<repo>/rulesets/<id>",
  "branches-main": "repos/<owner>/<repo>/branches/main",
  "branches-main-protection": "repos/<owner>/<repo>/branches/main/protection",
};

/** `text` as the JSON it holds, or `text` itself where it holds none (a text answer). */
function parsed(text: string): unknown {
  try {
    return JSON.parse(text);
  } catch {
    return text;
  }
}

/** The name the pin holds production's `read_record` under. */
export const RECORD_READ = "legion read_record";

/** An issue record with its two maps keyed by a name of the issue's read as lists of their
 * values: `workers`, keyed by role, and a row's `backwardMoves`, keyed by the pair of phases a
 * move went between. Which roles hold a row and which moves an issue has made are its state and
 * no shape, so a shape that named them would hold a world to the roles production's one issue
 * happened to have run; as lists, every row is read under one path and the kinds of each member
 * are joined over the rows. A list the world holds empty is compared no further down
 * (`shapeDiff`), so a record with no row at all passes this comparison: the row that holds a
 * world's record to the pin holds its roles by name as well. */
export function rolesListed(record: unknown): unknown {
  if (!isObject(record) || !isObject(record.workers)) return record;
  return {
    ...record,
    workers: Object.values(record.workers).map((row) =>
      isObject(row) && isObject(row.backwardMoves)
        ? { ...row, backwardMoves: Object.values(row.backwardMoves) }
        : row
    ),
  };
}

/** A production `read_record` as INFRA-764 kept it: a note naming when it was read, and the
 * record the tool answered. */
const RecordRead = z.object({ note: z.string(), record: z.unknown() });

/** A path of a record's shape whose every key is a word of the contract's: the root, or keys of
 * letters starting in lower case, each a list or not. */
const RECORD_PATH = /^\.$|^(\.[a-z][A-Za-z]*(\[\])*)+$/;

/** One read of a later set: its name, the command it was run with, when, its exit and its stderr. */
const LaterRead = z.object({
  name: z.string(),
  command: z.array(z.string()),
  read_at: z.string(),
  exit: z.number(),
  stderr: z.string(),
});
/** A later set of production reads, as INFRA-764 kept each: where it was read (`repo`), the pull
 * requests it read (`pull_requests`, by a name of the set's own), and its reads, each either a
 * read of its own, with the item of the unpinned list it reads (`item`) and its whole answer,
 * parsed where it is JSON (prod-reads-19.json, implementer-reads.json), or a reading again of a
 * read, with its stdout whole (`stdout`, prod-text-reads.json's). */
const LaterReads = z.object({
  source: z.string(),
  repo: z.string(),
  pull_requests: z.record(z.string(), z.string()).optional(),
  reads: z.array(
    z.union([
      LaterRead.extend({ item: z.string(), answer: z.unknown() }),
      LaterRead.extend({ stdout: z.string() }),
    ])
  ),
});

/** A commit's SHA as a command names one. */
const SHA = /\b[0-9a-f]{40}\b/g;

/** The readings of the later sets (`LaterReads`), each with production's values written as
 * placeholders in its name, its command and its stderr — which, unlike round 25's, carries them:
 * a `gh run view` names the run's URL on the repository, and `gh pr checks` the pull request's
 * branch — and whether it reads again a read an earlier set holds (`again`, a read with no
 * `item`); and the values the leak check looks for. A read's name starts with the program it ran,
 * as every pinned read's does, so a set whose names leave it out (prod-text-reads.json's `pr view
 * (text)`) has it put first. The values are read off the sets themselves, each for all of them,
 * since one set's text can name a value another's JSON gives: each set's repository; each pull
 * request's number; the two sides of a comparison their commands name, `<base>` and `<head>`, and
 * any other commit they name as `<head>`, of which one alone may be named; each branch their
 * answers name a pull request's head by (`headRefName`), `<branch>`, which production's names
 * with an issue key of its deployment; an App's slug a filter names (`app/<app>`); and a file's
 * path (`<path>`). */
function laterReads(files: readonly string[]): {
  readings: (Reading & { again: boolean })[];
  values: string[];
  answers: unknown[];
} {
  const sets = files.map((file) => LaterReads.parse(JSON.parse(readFileSync(file, "utf8"))));
  const reads = sets.flatMap((set) => set.reads);
  const answerOf = (read: (typeof reads)[number]) =>
    "stdout" in read ? parsed(read.stdout) : read.answer;
  const answers = reads.map(answerOf);
  const numbers = new Set(sets.flatMap((set) => Object.values(set.pull_requests ?? {})));
  const commands = reads.map(({ command }) => command.join(" ")).join("\n");
  const bases = new Set(
    [...commands.matchAll(/compare\/([0-9a-f]{40})\.\.\./g)].map((found) => found[1] ?? "")
  );
  const heads = new Set(
    [...commands.matchAll(SHA)].map(([sha]) => sha).filter((sha) => !bases.has(sha))
  );
  if (bases.size > 1 || heads.size > 1)
    throw new Error(
      `${files.join(", ")} name more than one base or head commit, which one placeholder cannot tell apart`
    );
  const branches = new Set(
    answers.flatMap((answer) =>
      isObject(answer) && typeof answer.headRefName === "string" ? [answer.headRefName] : []
    )
  );
  const apps = new Set([...commands.matchAll(/\bapp\/([\w-]+)/g)].map((found) => found[1] ?? ""));
  const owners = sets.map((set) => set.repo.split("/")[0] ?? "");
  const readings = sets.flatMap((set) => {
    const [owner = "", repo = ""] = set.repo.split("/");
    const placeholders = (text: string) => {
      let written = text
        .replaceAll(`${owner}/${repo}`, "<owner>/<repo>")
        .replaceAll(`"${owner}"`, '"<owner>"')
        .replaceAll(`"${repo}"`, '"<repo>"')
        .replace(new RegExp(`^(\\w+)=${owner}$`), "$1=<owner>")
        .replace(new RegExp(`^(\\w+)=${repo}$`), "$1=<repo>")
        .replace(/\/contents\/[^?\s]+/, "/contents/<path>")
        .replace(/\bapp\/[\w-]+/g, "app/<app>");
      for (const sha of bases) written = written.replaceAll(sha, "<base>");
      for (const sha of heads) written = written.replaceAll(sha, "<head>");
      for (const branch of branches) written = written.replaceAll(branch, "<branch>");
      for (const number of numbers)
        written = written.replace(new RegExp(`(?<![\\w])${number}(?![\\w])`, "g"), "<number>");
      return written;
    };
    return set.reads.map((read) => {
      const program = `${read.command[0]} `;
      return {
        name: placeholders(read.name.startsWith(program) ? read.name : program + read.name),
        command: read.command.map(placeholders),
        read_at: read.read_at,
        exit: read.exit,
        stderr: placeholders(read.stderr),
        answer: answerOf(read),
        read: readOf(read.command, set.repo),
        cutAt: Number.POSITIVE_INFINITY,
        again: !("item" in read),
      };
    });
  });
  return { readings, values: [...owners, ...bases, ...heads, ...branches, ...apps], answers };
}

/** The pin of the sets of production reads: `rulesetDir`, one `NAME.json`, `NAME.err` and
 * `NAME.exit` per read of a branch a ruleset alone protects, with its README naming when they were
 * read; `readsFile`, every distinct read of a pull request INFRA-764's round 25 made, with its
 * `substitutions` naming the production values its commands carry and its README when it was read;
 * each of `laterFiles`, a later set of reads (`laterReads`), each read with the time it was made,
 * where a reading again of a read replaces its earlier reading; and, where one is given,
 * `recordFile`, one `read_record` of a Legion issue with a note naming when it was read, pinned
 * last under `RECORD_READ` by its shape alone (`rolesListed`). Throws where a value of production
 * would be left in the pin, and where a text answer it pins is cut short (`pinnedOf`). */
export function derive(
  rulesetDir: string,
  readsFile: string,
  recordFile?: string,
  laterFiles: readonly string[] = []
): z.infer<typeof Pin> {
  const readme = (dir: string) => readFileSync(path.join(dir, "README.txt"), "utf8");
  const rulesetAt = /read at (\d{4}-\d\d-\d\dT[\d:]+Z)/.exec(readme(rulesetDir))?.[1];
  const readsAt = /\(read (\d{4}-\d\d-\d\d about [\d:]+Z)/.exec(
    readme(path.dirname(readsFile))
  )?.[1];
  if (rulesetAt === undefined || readsAt === undefined)
    throw new Error("a README names no time its reads were made");
  // Round 25's README marks its text answers cut at 600 characters: one that long is a reading
  // cut short, which a later set must read again whole (`pinnedOf`).
  const cutAt = Number(
    /text answers cut at (\d+) characters/.exec(readme(path.dirname(readsFile)))?.[1] ?? Infinity
  );
  const answers: unknown[] = [];
  const file = (name: string, ext: string) =>
    readFileSync(path.join(rulesetDir, `${name}.${ext}`), "utf8");
  const names = readdirSync(rulesetDir)
    .filter((entry) => entry.endsWith(".json"))
    .map((entry) => entry.slice(0, -".json".length))
    .sort();
  const ids = z
    .array(z.object({ id: z.number() }))
    .parse(parsed(file("rulesets", "json")))
    .map(({ id }) => String(id));
  const rulesetReadings = names.map((name): Reading => {
    const named = name.replace(/^ruleset-(\d+)$/, (whole, id: string) =>
      ids.includes(id) ? "ruleset-<id>" : whole
    );
    const endpoint = RULESET_READS[named];
    if (endpoint === undefined) throw new Error(`${rulesetDir}: no command is known for ${name}`);
    const answer = parsed(file(name, "json"));
    answers.push(answer);
    const command = ["gh", "api", endpoint];
    return {
      name: named,
      command,
      read_at: rulesetAt,
      exit: Number(file(name, "exit").trim()),
      stderr: file(name, "err"),
      answer,
      read: readOf(command, ""),
      cutAt: Number.POSITIVE_INFINITY,
    };
  });
  const Reads = z.object({
    substitutions: z.object({
      repo: z.string(),
      pr: z.string(),
      head: z.string(),
      base: z.string(),
      review: z.string(),
      comment: z.string(),
    }),
    reads: z.array(
      z.object({
        kind: z.string(),
        key: z.string(),
        command: z.array(z.string()),
        exit: z.number(),
        stderr: z.string(),
        answer: z.unknown(),
      })
    ),
  });
  const source = Reads.parse(JSON.parse(readFileSync(readsFile, "utf8")));
  const { substitutions: given } = source;
  // `<world> -> <production>` for the two the world renames, the production value for the rest.
  const production = (text: string) => text.split(" -> ").at(-1) ?? text;
  const [owner = "", repo = ""] = production(given.repo).split("/");
  const number = production(given.pr);
  const placeholders = (word: string) =>
    word
      .replaceAll(given.head, "<head>")
      .replaceAll(given.base, "<base>")
      .replaceAll(given.review, "<review>")
      .replaceAll(given.comment, "<comment>")
      .replaceAll(`${owner}/${repo}`, "<owner>/<repo>")
      .replaceAll(`"${owner}"`, '"<owner>"')
      .replaceAll(`"${repo}"`, '"<repo>"')
      .replace(new RegExp(`^(\\w+)=${owner}$`), "$1=<owner>")
      .replace(new RegExp(`^(\\w+)=${repo}$`), "$1=<repo>")
      .replace(new RegExp(`(?<![\\w])${number}(?![\\w])`, "g"), "<number>")
      .replace(/\/contents\/[^?]+/, "/contents/<path>");
  const prReadings = source.reads.map((read): Reading => {
    answers.push(read.answer);
    return {
      name: `${read.kind} ${read.key}`,
      command: read.command.map(placeholders),
      read_at: readsAt,
      exit: read.exit,
      stderr: read.stderr,
      answer: read.answer,
      read: readOf(read.command, `${owner}/${repo}`),
      cutAt,
    };
  });
  const later = laterReads(laterFiles);
  answers.push(...later.answers);
  // A reading again of a read an earlier set holds replaces each reading of it there, which
  // keeps its name, so the pin holds a read as production last answered it: round 25's text
  // readings, cut short, are read whole in prod-text-reads.json. Any other reading is a read of
  // its own.
  const readings = [...rulesetReadings, ...prReadings];
  for (const { again, ...reading } of later.readings) {
    let replaced = false;
    for (const [at, held] of readings.entries())
      if (again && held.read === reading.read) {
        readings[at] = { ...reading, name: held.name };
        replaced = true;
      }
    if (!replaced) readings.push(reading);
  }
  const reads = readings.map(pinnedOf);
  if (recordFile !== undefined) {
    const { note, record } = RecordRead.parse(JSON.parse(readFileSync(recordFile, "utf8")));
    const readAt = /read (\d{4}-\d\d-\d\dT[\d:]+Z)/.exec(note)?.[1];
    if (readAt === undefined) throw new Error(`${recordFile}'s note names no time it was read`);
    const shape = shapeOf(rolesListed(record));
    // The record's values can reach the pin through its shape alone, whose paths are its keys:
    // a map keyed by a value of the issue's (an issue key, a session, a pair of phases) would
    // write that value there. Every key the daemon's contract gives the record is one
    // camel-cased word of letters (all 112 paths of the read pinned on 2026-11-17 are), so a
    // path holding anything else holds a value, and is refused by name.
    const valued = Object.keys(shape).filter((at) => !RECORD_PATH.test(at));
    if (valued.length > 0)
      throw new Error(`the pin would hold production's values: ${valued.slice(0, 5).join(", ")}`);
    reads.push({
      name: RECORD_READ,
      // The tool and its op as one word, since it is no argv a row runs: the tool's own name,
      // alone, is the name of production's repository, a value the gh reads' answers hold and
      // the check below refuses.
      command: [RECORD_READ],
      read_at: readAt,
      exit: 0,
      stderr: "",
      shape,
    });
  }
  const unique = new Set(reads.map(({ name }) => name));
  if (unique.size !== reads.length) throw new Error("two reads share a name");
  const pin = {
    made_by:
      "production-shapes.ts (bun production-shapes.ts <ruleset reads dir> <production reads json> <read_record json> [<later reads json>...] <out json>), from INFRA-764's production reads of a branch a ruleset alone protects, of every distinct read round 25's runs made, of one issue's record as the legion tool's read_record answers it, of each read the world served with no pin, and of every text read again whole, each read from a Legion issue pod with the role App token its gh gives",
    reads,
  };
  // gh's own words — the group and the command a call names (`gh auth status`, `gh pr view`) and
  // a `--json` list of its fields — are never a value of production's, while an answer can hold
  // one of them as a value (round 25's check runs name GitHub Actions' App, whose `events` list
  // `milestone` and `status`), so they are left out of what the check reads. A `gh api` call's
  // endpoint is no word of gh's, and stays.
  const text = JSON.stringify({
    ...pin,
    reads: reads.map((read) => ({
      ...read,
      command: read.command.map((word, at) =>
        read.command[at - 1] === "--json" || (read.command[1] !== "api" && at <= 2) ? "" : word
      ),
    })),
  });
  const leaked = [
    owner,
    given.head,
    given.base,
    given.review,
    given.comment,
    ...ids,
    ...later.values,
    ...answers.flatMap(leaves),
  ].filter((value) => value !== "" && text.includes(value));
  if (leaked.length > 0)
    throw new Error(`the pin would hold production's values: ${leaked.slice(0, 5).join(", ")}`);
  return pin;
}

/** Every value of `answer` that would be production's own in the pin, as it would be written
 * there: a string of six characters or more, quoted, other than an error body's GitHub texts
 * (`message`, `documentation_url`, `status`) and a variant's name; and a number of five digits or
 * more, an id. */
function leaves(answer: unknown): string[] {
  if (typeof answer === "string") return answer.length >= 6 ? [JSON.stringify(answer)] : [];
  if (typeof answer === "number") return Math.abs(answer) >= 10_000 ? [String(answer)] : [];
  if (Array.isArray(answer)) return answer.flatMap(leaves);
  if (!isObject(answer)) return [];
  return Object.entries(answer).flatMap(([key, value]) =>
    DISCRIMINATORS.includes(key) ||
    ("message" in answer && ["message", "documentation_url", "status"].includes(key))
      ? []
      : leaves(value)
  );
}

if (import.meta.main) {
  const args = Bun.argv.slice(2);
  const [rulesetDir, readsFile, recordFile] = args;
  const out = args.at(-1);
  if (
    rulesetDir === undefined ||
    readsFile === undefined ||
    recordFile === undefined ||
    out === undefined ||
    args.length < 4
  ) {
    console.error(
      "usage: bun production-shapes.ts <ruleset reads dir> <production reads json> <read_record json> [<later reads json>...] <out json>"
    );
    process.exit(2);
  }
  const pin = derive(rulesetDir, readsFile, recordFile, args.slice(3, -1));
  writeFileSync(out, `${JSON.stringify(pin, null, 2)}\n`);
  const formatted = Bun.spawnSync(["bunx", "biome", "format", "--write", out], {
    stdout: "inherit",
    stderr: "inherit",
  });
  process.exit(formatted.exitCode);
}
