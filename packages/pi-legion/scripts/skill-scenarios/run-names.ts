// What the rig calls a run, kept where no agent of the run can read it, and the words a run's
// world and its agents' own words are held to. A run's directory is named by an opaque id
// (rig.sh's cmd_run), so its path, and every path, variable and process built on it, names
// nothing of the measurement; the run's scenario, case, label and number are in its record,
// <work>/runs/<id>.json beside the directory (`recordFile`), which the run's namespace covers with
// the rest of the work directory. Every reader of a run's scenario, case or label reads it here
// (`runIdentity`): rig.sh, the readers (health.ts, boot-refusal.ts, completion-answers.ts), the
// scorer and judge.ts. It imports nothing of the scorer or of health: the daemon stand-in loads it
// at every boot, through completion-answers.ts.
//
//   run-names.ts case <scenario>            prints the case the scenario's own variable names
//                                           (SCENARIOS), nothing for a scenario that takes none;
//                                           exits 1, naming the variable and every case, for a
//                                           variable that names none, and 2 for no scenario of
//                                           SCENARIOS
//   run-names.ts guard <run dir> <sessions dir> <work dir> <install>
//                                           exits 0 when nothing the run's agent is about to read
//                                           names a word of the vocabulary (`worldNames`), and 1,
//                                           printing the first word and where it is, when
//                                           something does, and 1, printing why, when the run's
//                                           system prompt cannot be read (`systemPromptOf`), its
//                                           <run>/reviewer-wake.json cannot be parsed
//                                           (`promptOf`) or a link of the run points outside it,
//                                           the label's install <install> (load_label's I) and
//                                           the host's trees (`hostProvided`, `worldFiles`):
//                                           rig.sh's launch asks before every agent starts, and
//                                           starts none of a world that names one or that it
//                                           cannot read whole. At a later launch of the run (one
//                                           <run>/logs/launches records, `firstLaunch`) it reads
//                                           nothing an agent of the run can write (AGENT_WRITTEN)
import {
  existsSync,
  lstatSync,
  readdirSync,
  readFileSync,
  readlinkSync,
  realpathSync,
  type Stats,
  statSync,
} from "node:fs";
import path from "node:path";
import { z } from "zod";

/** Every scenario rig.sh runs, by the name `rig.sh batch` and `rig.sh run` take: the variable
 * that names its case (rig.sh's scenario_case reads it through `caseOf`), the cases it takes, in
 * the order a refusal lists them, and `world: "worker"` where rig.sh builds it a worker world
 * (worker_run_world): a Legion issue whose seeds record results at the world's pull request,
 * which the run's Dispatch checks against its GitHub at the seed (github-routes.ts, measured), and
 * whose head watcher it starts (start_heads). A scenario with no cases takes none. It is the rig's
 * one scenario table: the cases a run may be built for, the cases score.ts and health read back,
 * the worlds health holds to the GitHub and head records (rules 18 and 19), and the words the
 * vocabulary holds (`vocabulary`) are all this table. */
export const SCENARIOS: Readonly<
  Record<
    string,
    {
      readonly variable?: string;
      readonly cases: readonly string[];
      readonly world?: "worker";
    }
  >
> = {
  "ask-on-message": { cases: [] },
  "measure-before-ask": { cases: [] },
  "tester-proof": { cases: [], world: "worker" },
  // k1 runs text-only and its defect control (Verification v47); k3 runs last-push, text-only's
  // world on a base whose rules require another approver of the last push.
  "reviewer-text-findings": {
    variable: "SKILL_SCENARIOS_REVIEWER_CASE",
    cases: ["text-only", "defect", "last-push"],
    world: "worker",
  },
  // No case is a default: Verification v47's k2 names the case of every batch it runs, as k1 does
  // reviewer-text-findings', so an unset variable is refused rather than read as one of them. k2
  // runs accept, correct and the code-defect control; k3 runs crossing, accept's world with a
  // check its earlier pass does not hold at the reviewer's commit.
  "threads-closed": {
    variable: "SKILL_SCENARIOS_THREADS_CASE",
    cases: ["accept", "correct", "crossing", "code-defect"],
    world: "worker",
  },
  brainstorm: { cases: [] },
  askgate: {
    variable: "SKILL_SCENARIOS_ASKGATE_CASE",
    cases: [
      "facts",
      "link",
      "upload",
      "nochange",
      "nochange-rebuttal",
      "nochange-block",
      "untried",
      "tried",
    ],
  },
  "spec-writing": { cases: [] },
  "spec-rewrite": { cases: [] },
};

/** The cases of `scenario`, none for a scenario that takes none or that SCENARIOS does not name. */
export function casesOf(scenario: string): readonly string[] {
  return SCENARIOS[scenario]?.cases ?? [];
}

/** Whether rig.sh builds `scenario` a worker world (SCENARIOS' `world`). */
export function workerWorld(scenario: string): boolean {
  return SCENARIOS[scenario]?.world === "worker";
}

/** The case `env` names for `scenario` through the scenario's own variable, "" for a scenario
 * that takes none. Throws, naming the variable, the value and every case (`a, b or c`), for a
 * value that is no case of the scenario's, and for no scenario of SCENARIOS. */
export function caseOf(
  scenario: string,
  env: Readonly<Record<string, string | undefined>>
): string {
  const entry = SCENARIOS[scenario];
  if (entry === undefined) throw new Error(`unknown scenario ${scenario}`);
  if (entry.variable === undefined) return "";
  const value = env[entry.variable] ?? "";
  if (!entry.cases.includes(value))
    throw new Error(
      `${entry.variable} is '${value}', not ${entry.cases.slice(0, -1).join(", ")} or ${entry.cases.at(-1)}`
    );
  return value;
}

/** The shape of a run's id, its directory's own name (rig.sh's cmd_run): twelve hex digits, which
 * name nothing. */
export const RUN_ID = /^[0-9a-f]{12}$/;

/** A run's record: what the rig calls the run, which nothing the run's agents can read names.
 * rig.sh's cmd_run writes it, before anything of the run starts, to <work>/runs/<id>.json beside
 * the run's directory, and archive_run moves it with the directory. The fields:
 *   run       the run's id, its directory's own name;
 *   scenario  a scenario of SCENARIOS;
 *   case      its case, "" for a scenario that takes none;
 *   label     the label whose checkout's skills the run's agents load;
 *   n         the run's number among the runs of its scenario, case and label in the work
 *             directory (rig.sh's next_run);
 *   name      `<scenario>[-<case>]-<label>-<n>`, the name a run's directory had before the names
 *             left the world, for a person reading the record;
 *   batch     the batch the run was started in: the work directory's tag and the batch's start;
 *   ran       the run's directory as it ran, the path its transcripts and pane name, which differs
 *             from the directory read once the run is archived or copied;
 *   mode      askgate's SKILL_SCENARIOS_ASKGATE_MODE where one was set. */
export const RunRecord = z.strictObject({
  run: z.string().regex(RUN_ID),
  scenario: z.string(),
  case: z.string(),
  label: z.string().regex(/^[a-z0-9][a-z0-9-]*$/),
  n: z.number().int().positive(),
  name: z.string(),
  batch: z.string(),
  ran: z.string().startsWith("/"),
  mode: z.enum(["warn", "block"]).optional(),
});
export type RunRecord = z.infer<typeof RunRecord>;

/** The record of the run whose directory is `runDir`: beside it, `<run dir>.json`. */
export function recordFile(runDir: string): string {
  return `${path.resolve(runDir)}.json`;
}

/** The kind of a directory entry, by its own type, a symlink never followed: what a refusal of an
 * entry that is no run directory names it as (score.ts's `runsOf`, health's run directories). */
export function entryKind(
  entry: Pick<
    Stats,
    "isSymbolicLink" | "isFile" | "isDirectory" | "isFIFO" | "isSocket" | "isBlockDevice"
  >
): string {
  if (entry.isSymbolicLink()) return "symlink";
  if (entry.isFile()) return "file";
  if (entry.isDirectory()) return "directory";
  if (entry.isFIFO()) return "fifo";
  if (entry.isSocket()) return "socket";
  return entry.isBlockDevice() ? "block device" : "character device";
}

/** What a run is: its scenario, case, label and number, the name a person reads it by, and where
 * and in which batch its record says it ran (each undefined for a run read from its name, whose
 * pane names where it ran and which recorded no batch). */
export interface RunIdentity {
  readonly scenario: string;
  readonly case: string;
  readonly label: string;
  readonly n: number;
  readonly name: string;
  readonly ran: string | undefined;
  readonly batch: string | undefined;
  /** Whether the identity was read from the run's record (`recordFile`), or, for a run recorded
   * before the names left the world, from its directory's name. */
  readonly recorded: boolean;
  readonly mode: string | undefined;
}

/** The scenario, case, label and number a name `<scenario>[-<case>]-<label>-<n>` gives: the
 * longest scenario of SCENARIOS it starts with, then, for a cased scenario, the longest of its
 * cases (`nochange-block` before `nochange`), and the rest before the number is the label. Why
 * it gives none, for a name of no such shape. */
function identityOfName(
  name: string
): Omit<RunIdentity, "ran" | "batch" | "recorded" | "mode"> | string {
  const scenario = Object.keys(SCENARIOS)
    .filter((one) => name.startsWith(`${one}-`))
    .sort((a, b) => b.length - a.length)[0];
  if (scenario === undefined) return `${name} names no scenario rig.sh runs`;
  const number = /-(\d+)$/.exec(name);
  if (number === null) return `${name} ends with no run number`;
  const middle = name.slice(scenario.length + 1, number.index);
  const cases = casesOf(scenario);
  const runCase =
    cases.length === 0
      ? ""
      : [...cases].sort((a, b) => b.length - a.length).find((one) => middle.startsWith(`${one}-`));
  if (runCase === undefined)
    return `${name} names no case of ${scenario}: expected ${scenario}-<case>-<label>-<n>`;
  const label = runCase === "" ? middle : middle.slice(runCase.length + 1);
  if (label === "") return `${name} names no label`;
  return { scenario, case: runCase, label, n: Number(number[1]), name };
}

/** The identity of the run whose directory is `runDir`, or why it has none. A run with a record
 * (`recordFile`) is read from the record alone, whose run must be the directory's own name and
 * whose scenario and case SCENARIOS must hold. A run recorded before the names left the world
 * has no record, and its directory's name is the only place its scenario, case and label were
 * ever kept: it is read from that name (`identityOfName`). A directory named by a run id with no
 * record is no run anyone can read. */
export function runIdentity(runDir: string): RunIdentity | string {
  const base = path.basename(path.resolve(runDir));
  const file = recordFile(runDir);
  if (existsSync(file)) {
    const parsed = RunRecord.safeParse(JSON.parse(readFileSync(file, "utf8")));
    if (!parsed.success) return `${file} is no run record: ${z.prettifyError(parsed.error)}`;
    const record = parsed.data;
    if (record.run !== base) return `${file} is the record of run ${record.run}, not of ${base}`;
    const cases = casesOf(record.scenario);
    if (SCENARIOS[record.scenario] === undefined)
      return `${file} names the scenario ${record.scenario}, which rig.sh does not run`;
    if (cases.length === 0 ? record.case !== "" : !cases.includes(record.case))
      return `${file} names the case '${record.case}', which is no case of ${record.scenario}`;
    return {
      scenario: record.scenario,
      case: record.case,
      label: record.label,
      n: record.n,
      name: record.name,
      ran: record.ran,
      batch: record.batch,
      recorded: true,
      mode: record.mode,
    };
  }
  if (RUN_ID.test(base)) return `${runDir} is named by a run id and has no record ${file}`;
  // A run recorded before the names left the world: its name is its record.
  const named = identityOfName(base);
  return typeof named === "string"
    ? named
    : { ...named, ran: undefined, batch: undefined, recorded: false, mode: undefined };
}

/** `runIdentity`, or a throw naming why the run has none. */
export function identityOf(runDir: string): RunIdentity {
  const identity = runIdentity(runDir);
  if (typeof identity === "string") throw new Error(identity);
  return identity;
}

/** The names the architect's ruling of 2026-11-16T09:0xZ took out of everything an agent can
 * read, each traced to the sentences of the batch at c273084d that called the world a test: the
 * seeded Dispatch sessions (`rig-<role>`), the work directory's own name (`skill-scenarios`, its
 * stem, so SKILL_SCENARIO_RUN is one too), the Dispatch project key (`lweval`), the seeding
 * session (`e2e-seed`) and the repository (`example/widgets`). Each is matched where a word
 * starts, whatever follows it (`lweval-1`, `lweval1`, `rig-tester`). */
export const REMOVED_NAMES = ["rig-", "skill-scenario", "lweval", "e2e-seed", "example/widgets"];

/** The Dispatch project every run's own issue is filed in (seed.ts), the same for every label: a
 * key of the shape a team's project has, for the widgets repository the world is on. It was
 * `LWEVAL`. */
export const WORLD_PROJECT = "WID";

/** The remote helper the world's issue workspace fetches and pushes through, which production's
 * clone has not got: its origin is `https://github.com/<owner>/<repo>`, as production's is, and
 * `remote.origin.vcs` names this helper, which git runs as `git-remote-<vcs>` from the pane's PATH
 * to reach the run's own bare remote (rig.sh's world_remote, whose `world_remote_vcs` is this
 * name). `git config -l` shows the vcs key and `ls <run>/bin` the helper, so an agent that writes
 * either read the world's own transport, which no production clone has: rule 17 of health reads
 * each as a word of the measurement where an input held it first (`proseNames`, `holds`), as it
 * reads a removed name, where a word starts and whatever follows it. The helper's name comes
 * first, so a sentence that names it is read by its whole name. */
export const WORLD_REMOTE_VCS = "ghapp";
export const WORLD_REMOTE_NAMES = [`git-remote-${WORLD_REMOTE_VCS}`, WORLD_REMOTE_VCS];

/** The words a run's world is held to, derived from SCENARIOS and the labels given: every
 * scenario, every case and every label. A word with a character that is no letter or digit
 * (`text-only`, `threads-closed`) names the measurement wherever it stands alone, a case not
 * inside a longer identifier (`besideWord`); a plain word (`head`,
 * `base`, `defect`, `askgate`) is also an ordinary word, and names it only as a part of a name or
 * a path, or beside another word of the measurement (`proseNames`). The removed names
 * (REMOVED_NAMES) are distinctive. */
export interface Vocabulary {
  readonly scenarios: readonly string[];
  readonly cases: readonly string[];
  readonly labels: readonly string[];
  readonly distinctive: readonly string[];
  readonly ordinary: readonly string[];
}

/** Every case of every scenario of SCENARIOS, once. */
const CASES: ReadonlySet<string> = new Set(
  Object.keys(SCENARIOS).flatMap((scenario) => casesOf(scenario))
);

export function vocabulary(labels: readonly string[]): Vocabulary {
  const scenarios = Object.keys(SCENARIOS);
  const cases = [...CASES];
  const all = [...new Set([...scenarios, ...cases, ...labels].map((word) => word.toLowerCase()))];
  return {
    scenarios,
    cases,
    labels: [...new Set(labels)],
    distinctive: all.filter((word) => /[^a-z0-9]/.test(word)),
    ordinary: all.filter((word) => !/[^a-z0-9]/.test(word)),
  };
}

const literal = (word: string) => word.replace(/[.*+?^${}()|[\]\\/]/g, "\\$&");
/** A text as the words are matched in it: lower case, an underscore read as a hyphen. */
const folded = (text: string) => text.toLowerCase().replaceAll("_", "-");
/** What may stand on either side of `word`, a word of the vocabulary, where it stands alone in a
 * text `folded`: no letter or digit, and, for a case of SCENARIOS, no hyphen either (an
 * underscore folded to one). A case name inside a longer identifier joined by `_` or `-` is that
 * identifier's and not the rig's word (the architect's ruling D6 of INFRA-764's plan v9): the
 * reviewer's system prompt names the base rule `require_last_push_approval`, which every world's
 * rules answer (rig.sh's base_ruleset_routes), and the case `last-push` stands inside it, so a
 * guard that read it there refused every run of every reviewer world. */
const besideWord = (word: string) => (CASES.has(word) ? "[a-z0-9-]" : "[a-z0-9]");

/** The first removed name (REMOVED_NAMES), or the first of `names`, in `lower`, a text `folded`:
 * where a word starts, whatever follows it. */
function removedIn(lower: string, names: readonly string[] = REMOVED_NAMES): string | undefined {
  return names.find((word) => new RegExp(`(?<![a-z0-9])${literal(word)}`).test(lower));
}

/** The first distinctive word of `vocab` in `text`, or of REMOVED_NAMES when `removed` holds:
 * a vocabulary word standing alone (`besideWord`: no letter or digit on either side, and no
 * hyphen beside a case), a removed name where a word starts (`removedIn`). */
function distinctiveIn(text: string, vocab: Vocabulary, removed: boolean): string | undefined {
  const lower = folded(text);
  for (const word of vocab.distinctive) {
    const beside = besideWord(word);
    if (new RegExp(`(?<!${beside})${literal(word)}(?!${beside})`).test(lower)) return word;
  }
  return removed ? removedIn(lower) : undefined;
}

/** The first word of `vocab` in `value`, a name or a path an agent can read (a path, an
 * environment variable's name, a profile, a project, an issue key, a topic): any distinctive word
 * or removed name, and any plain word that is a whole part of it, the parts being the runs of
 * letters and digits (`skills-head` holds `head`; `refs/heads` does not). */
export function nameWords(value: string, vocab: Vocabulary): string | undefined {
  const found = distinctiveIn(value, vocab, true);
  if (found !== undefined) return found;
  const parts = new Set(value.toLowerCase().split(/[^a-z0-9]+/));
  return vocab.ordinary.find((word) => parts.has(word));
}

/** The first distinctive word or removed name in `text`, free text an agent can read (a prompt,
 * a variable's value that is no path, a file of the world): a plain word there is the work's own
 * word as often as not, so it is read only in a name (`nameWords`). */
export function textWords(text: string, vocab: Vocabulary): string | undefined {
  return distinctiveIn(text, vocab, true);
}

/** The first word of the measurement a sentence says, or undefined: a distinctive scenario, case
 * or label word (`text-only`, `threads-closed`); a removed name (REMOVED_NAMES, `removedIn`:
 * `skill-scenarios`, `LWEVAL`, `rig-reviewer`); a name of the world's remote (WORLD_REMOTE_NAMES,
 * read as a removed name is: `git-remote-ghapp`, `ghapp`); a run's name in rig.sh's old shape
 * (`askgate-facts-on-1`); or a case word and a label word side by side, as a run's name puts them
 * (`defect head`, `base text-only`). A plain word alone is the work's own (`head SHA`, `base
 * branch`, `not a defect`). It is the one definition of a word that names the measurement for
 * every reader of what an agent says or sends: health's rule 17, the names an agent's own text
 * and thinking say, and its rule 13, the words a web search that is part of the world holds none
 * of. A removed name is one of them, as the tester's reader counts it too (the architect's ruling
 * of 2026-11-16): the world was rid of each because it told an agent its world was a test, so an
 * input that hands one to an agent anyway leaks the measurement as a scenario's name does, and a
 * query that carries one sends the measurement off the world. A name of the world's remote is one
 * because no production clone has it (the architect's ruling of 2026-11-18): an agent that says it
 * said what the world showed it of itself. */
export function proseNames(sentence: string, vocab: Vocabulary): string | undefined {
  const lower = folded(sentence);
  for (const word of vocab.distinctive)
    if (new RegExp(`(?<![a-z0-9-])${literal(word)}(?![a-z0-9-])`).test(lower)) return word;
  const removed = removedIn(lower) ?? removedIn(lower, WORLD_REMOTE_NAMES);
  if (removed !== undefined) return removed;
  const alt = (words: readonly string[]) =>
    [...words]
      .map((word) => literal(word.toLowerCase()))
      .sort((a, b) => b.length - a.length)
      .join("|");
  if (vocab.labels.length === 0) return undefined;
  const scenarios = alt(vocab.scenarios);
  const cases = alt(vocab.cases);
  const labels = alt(vocab.labels);
  const run = new RegExp(
    `(?<![a-z0-9-])(?:${scenarios})(?:-(?:${cases}))?-(?:${labels})-\\d+(?![a-z0-9-])`
  ).exec(lower);
  if (run !== null) return run[0];
  const pair = new RegExp(
    `(?<![a-z0-9-])(?:(?:${cases})[ -](?:${labels})|(?:${labels})[ -](?:${cases}))(?![a-z0-9-])`
  ).exec(lower);
  return pair?.[0];
}

/** Whether `text`, an input an agent was given or a thing it wrote, holds `word`, a word
 * `proseNames` read in a sentence: where a word starts and ends (no letter or digit beside it, an
 * underscore read as a hyphen, and no hyphen beside a case either, `besideWord`), so a path that
 * names a run (`runs/threads-closed-correct-head-1/`) holds its scenario word, an input that
 * names the rule `require_last_push_approval` holds no case `last-push`, and a space in a pair
 * (`defect head`) is a space or a hyphen; a removed name or a name of the world's remote where a
 * word starts, whatever follows it, as `removedIn` reads it (`rig-` in `rig-reviewer`). */
export function holds(text: string, word: string): boolean {
  const pattern = literal(word).replaceAll(" ", "[ -]");
  const beside = besideWord(word);
  const end = [...REMOVED_NAMES, ...WORLD_REMOTE_NAMES].includes(word) ? "" : `(?!${beside})`;
  return new RegExp(`(?<!${beside})${pattern}${end}`).test(folded(text));
}

/** The labels the work directory `work` holds a profile for (`rig.sh profile`), by the names
 * under <work>/profiles. */
export function workLabels(work: string): string[] {
  const profiles = path.join(work, "profiles");
  return existsSync(profiles)
    ? readdirSync(profiles).filter((label) => existsSync(path.join(profiles, label, "checkout")))
    : [];
}

/** Where `rig.sh profile` installed the profile of the label the run under `runDir` ran, its HOME
 * and its plugins, which every run of the label reads its skills from, as the run recorded it:
 * the `ro <at>/installs/<id>` line of its namespace.txt, the install its world showed (rig.sh's
 * world_paths), so no path a run's agents read names the label; <at>/profiles/<label> itself for
 * a run from before the installs, which installed there and records none. `at` is the work
 * directory the run had when it ran. Never <work>/profiles/<label>/install as the work directory
 * holds it now (the architect's ruling of 2026-11-18: a reader reads a run from its archive and
 * the provenance it recorded), which a copy read elsewhere does not have. */
export function labelInstall(runDir: string, label: string, at: string): string {
  const file = path.join(runDir, "namespace.txt");
  const installs = `ro ${path.join(at, "installs")}/`;
  const line = existsSync(file)
    ? readFileSync(file, "utf8")
        .split("\n")
        .find((one) => one.startsWith(installs) && !one.slice(installs.length).includes("/"))
    : undefined;
  return line?.slice("ro ".length) ?? path.join(at, "profiles", label);
}

/** The variables of a pane file, KEY=value a line, as `env -i` takes them, the last of two. */
function paneVars(file: string): Map<string, string> {
  const vars = new Map<string, string>();
  if (!existsSync(file)) return vars;
  for (const line of readFileSync(file, "utf8").split("\n")) {
    const equals = line.indexOf("=");
    if (equals > 0) vars.set(line.slice(0, equals), line.slice(equals + 1));
  }
  return vars;
}

/** The variables whose value is a name a reader of the world takes for a name rather than text:
 * the profile, the Legion project, issue and tree, the role, and the repository. A value with a
 * `/` in it is a path or a list of paths, and is read as a name too. */
const NAMED_VARS: Record<string, true> = {
  OMP_PROFILE: true,
  PI_PROFILE: true,
  LEGION_PROJECT: true,
  LEGION_ISSUE: true,
  LEGION_TREE: true,
  LEGION_ROLE: true,
  GH_REPO: true,
};

/** Where a run's world names a word, and the word: one place of everything the agent about to
 * start can read, or undefined for a world that names none. */
export interface Named {
  readonly place: string;
  readonly word: string;
  readonly value: string;
}

/** The paths the run's namespace mounts, as private_world mounts them from <run>/namespace.txt
 * (rig.sh's world_paths): each cover and credentials path that is a directory and lies under no
 * cover mounted before it (one that does is gone once that cover is mounted, and is skipped),
 * each rw and ro path that is a directory, at its own path and from its own path, and the run's
 * tmp, which is bound onto /tmp. Each is a mount point the agent's /proc/self/mountinfo lists,
 * and each bound path is also the source that line names. */
export function mountedPaths(runDir: string): string[] {
  const file = path.join(runDir, "namespace.txt");
  if (!existsSync(file)) return [];
  const isDir = (one: string) => existsSync(one) && statSync(one).isDirectory();
  const covered: string[] = [];
  const mounted: string[] = [];
  const lines = readFileSync(file, "utf8")
    .split("\n")
    .filter((line) => line !== "")
    .map((line) => {
      const space = line.indexOf(" ");
      return { kind: line.slice(0, space), at: line.slice(space + 1) };
    });
  for (const { kind, at } of lines)
    if ((kind === "cover" || kind === "credentials") && isDir(at)) {
      if (covered.some((one) => at === one || at.startsWith(`${one}/`))) continue;
      covered.push(at);
      mounted.push(at);
    }
  for (const { kind, at } of lines)
    if ((kind === "rw" || kind === "ro") && isDir(at)) mounted.push(at);
  mounted.push(path.join(runDir, "tmp"));
  return mounted;
}

/** A run's system prompt, as `systemPromptIn` reads it: the `--append-system-prompt` argument
 * <run>/system-args holds, which rig.sh's launch puts on the agent's command line, and the text
 * of the prompt the agent starts with, with the place that text was read. */
export interface SystemPrompt {
  /** The argument, as shell text: what the agent's command line carries. */
  readonly argument: string;
  /** The prompt's text. */
  readonly text: string;
  /** Where `text` was read: <run>/system-args for a prompt the argument carries itself, else the
   * file the argument names, where the run is now (`systemPromptIn`), as <run>/… where the file
   * is under the run. */
  readonly place: string;
}

const PROMPT_FLAG = "--append-system-prompt";

/** The system prompt of the agent of `runDir`, read out of <run>/system-args (`systemPromptIn`),
 * or undefined for a run with no such file: rig.sh's launch gives an agent a system prompt only
 * where that file is (a worker pane's, which worker-pane.ts writes), and a spec run's agent starts
 * with none. Every reader of a run's system prompt reads it here: launch's guard (`worldNames`)
 * and rule 17 of health.ts (`namesOf`). */
export function systemPromptOf(runDir: string): SystemPrompt | undefined {
  const file = path.join(runDir, "system-args");
  return existsSync(file) ? systemPromptIn(readFileSync(file, "utf8"), runDir) : undefined;
}

/** The system prompt `argument`, one `--append-system-prompt` argument as shell text, gives the
 * agent of `runDir`, in either shape the daemon's pane composer has answered with (daemon-pane.go's
 * systemPromptArgument). Since the daemon hands each role its system prompt as a file
 * (cybermonklabs/legion#4190, runtime.WriteSystemPrompt), the composer writes the prompt to
 * <run>/state/secrets/<claim>-system-prompt.md and the argument names that file, one shell word
 * (shellprefix.Word: a bare path, or a single-quoted one), so the prompt is the file's text.
 * Before, the argument carried the prompt itself, one quoted word (the role prompts' `$(cat …)` and
 * the addressing, double-quoted), and that text is the prompt as the agent's command line held it;
 * a run composed or recorded then still holds that shape. The argument names the file as the run
 * had it while it ran: a path under the directory its record says it ran in (`runIdentity`'s
 * `ran`) is read at the same place under `runDir`, since rig.sh's archive_run moves a run that
 * has ended, and its prompt file with it, and a run read from a copy holds its own file; any
 * other path, and every path of a run with no record, is read as named. Anything else is
 * refused, and so are a named file that cannot be read where it is read and an empty prompt, each
 * naming the run and what it holds: a reader that took one of them for a prompt would hold the
 * agent's prompt to nothing, and a guard or a rule that reads nothing finds nothing wrong. */
export function systemPromptIn(argument: string, runDir: string): SystemPrompt {
  const refuse = (what: string): never => {
    throw new Error(`the system prompt of ${path.basename(runDir)} ${what}`);
  };
  // A file's trailing newlines are no part of the argument: rig.sh's launch reads it with
  // `$(<"$R/system-args")`, which drops them.
  const given = argument.replace(/\n+$/, "");
  const held = `<run>/system-args holds ${JSON.stringify(given.slice(0, 200))}`;
  if (!given.startsWith(`${PROMPT_FLAG} `)) refuse(`is no ${PROMPT_FLAG} argument: ${held}`);
  const word = given.slice(PROMPT_FLAG.length + 1);
  const quoted = /^'(?:[^']|'\\'')*'$/.test(word)
    ? word.slice(1, -1).replaceAll("'\\''", "'")
    : undefined;
  const named = /^\/[A-Za-z0-9_./:-]+$/.test(word)
    ? word
    : quoted !== undefined && /^\/[^\n]*$/.test(quoted)
      ? quoted
      : undefined;
  if (named === undefined) {
    if (quoted === undefined && !/^"[\s\S]*"$/.test(word))
      refuse(`is neither a prompt nor the path of a prompt's file: ${held}`);
    if ((quoted ?? word.slice(1, -1)).trim() === "") refuse(`is empty: ${held}`);
    return { argument: given, text: given, place: "<run>/system-args" };
  }
  // The path as the run had it (its record's `ran`), read where the run is now: an archived
  // run's prompt file is under its archived directory, and nothing is at the path named.
  const identity = runIdentity(runDir);
  const ran = typeof identity === "string" ? undefined : identity.ran;
  const moved = ran === undefined ? undefined : within(ran, named);
  const file = moved === undefined ? named : path.join(runDir, moved);
  const inRun = within(runDir, file);
  const place = inRun === undefined ? file : `<run>/${inRun}`;
  let text = "";
  try {
    text = readFileSync(file, "utf8");
  } catch (error) {
    refuse(
      `names ${place}, which cannot be read: ${error instanceof Error ? error.message : String(error)}`
    );
  }
  if (text.trim() === "") refuse(`names ${place}, which is empty`);
  return { argument: given, text, place };
}

/** `file`'s path relative to `dir`, or undefined for a file outside it. */
function within(dir: string, file: string): string | undefined {
  const relative = path.relative(dir, file);
  return relative === ".." || relative.startsWith(`..${path.sep}`) || path.isAbsolute(relative)
    ? undefined
    : relative;
}

/** The file rig.sh's run_threads_closed keeps the reviewer's wake in, whole: `{wake: [...]}`,
 * each message the implementer sent the reviewer as Envoy's record holds it (record-publishes.ts:
 * `at`, `topic`, `message`, `sender`, `subject`), in the order the rig wrote them into the
 * reviewer's <run>/prompt.txt. */
const WAKE_FILE = "reviewer-wake.json";

/** One message an agent of a run sent the next, which the rig relayed into the next one's prompt
 * (`promptOf`): when Envoy accepted it (`at`, a time), its body (`message`) and the session Envoy
 * recorded as its sender (`sender`). */
const Relayed = z.looseObject({
  at: z.string().refine((at) => Number.isFinite(Date.parse(at)), "is no time"),
  message: z.string(),
  sender: z.string(),
});
export type Relayed = z.infer<typeof Relayed>;
const Wake = z.strictObject({ wake: z.array(Relayed) });

/** One message the rig relayed into a run's prompt, and where its text is in <run>/prompt.txt:
 * from `start` up to `end`. */
export interface RelayedAt {
  readonly relayed: Relayed;
  readonly start: number;
  readonly end: number;
}

/** A run's prompt, as `promptOf` reads it: the text of <run>/prompt.txt, and each message of
 * <run>/reviewer-wake.json at the place the rig wrote it there. */
export interface RunPrompt {
  readonly text: string;
  readonly relayed: readonly RelayedAt[];
}

/** The prompt of the agent of `runDir`, or undefined for a run with no <run>/prompt.txt. Every
 * reader of a run's prompt reads it here: launch's guard (`worldNames`) and rule 17 of health.ts
 * (`namesOf`). The prompt is the rig's text but for the messages it relays from one agent of the
 * run to the next (rig.sh's run_threads_closed, which keeps them whole in
 * <run>/reviewer-wake.json), whose words are the sending agent's own (the architect's ruling of
 * 2026-11-20T17:30Z): each is read where the rig wrote it (`relayedIn`), and `rigText` leaves it
 * out. No such file is no message relayed; one that cannot be parsed is refused, naming it. */
export function promptOf(runDir: string): RunPrompt | undefined {
  const file = path.join(runDir, "prompt.txt");
  if (!existsSync(file) || !statSync(file).isFile()) return undefined;
  const text = readFileSync(file, "utf8");
  return { text, relayed: relayedIn(text, wakeOf(runDir)) };
}

/** The messages <run>/reviewer-wake.json holds, in its order, and none for a run with no such
 * file; a throw naming the file for one that is no JSON or not of the shape the rig writes. */
function wakeOf(runDir: string): readonly Relayed[] {
  const file = path.join(runDir, WAKE_FILE);
  if (!existsSync(file)) return [];
  let why: string;
  try {
    const parsed = Wake.safeParse(JSON.parse(readFileSync(file, "utf8")));
    if (parsed.success) return parsed.data.wake;
    why = z.prettifyError(parsed.error);
  } catch (error) {
    why = error instanceof Error ? error.message : String(error);
  }
  throw new Error(`<run>/${WAKE_FILE} cannot be parsed: ${why}`);
}

/** Where in `prompt` the rig wrote each of `wake`, in its order: run_threads_closed writes each
 * message whole after a prefix of its own (`Implementer → reviewer, <key>. `), the line ending
 * with a newline after the message. So each message is read at the end of the next line after the
 * one before it, its first line ending where that line ends and the rest of it and a newline
 * following. A message not there is none the rig wrote there: it and every one after it are read
 * nowhere, and the prompt's text stays the rig's. */
function relayedIn(prompt: string, wake: readonly Relayed[]): RelayedAt[] {
  const found: RelayedAt[] = [];
  let from = 0;
  for (const relayed of wake) {
    const lineEnd = prompt.indexOf("\n", from);
    const firstLine = relayed.message.indexOf("\n");
    const start = lineEnd - (firstLine < 0 ? relayed.message.length : firstLine);
    if (lineEnd < 0 || start < from || !prompt.startsWith(`${relayed.message}\n`, start)) break;
    const end = start + relayed.message.length;
    found.push({ relayed, start, end });
    from = end + 1;
  }
  return found;
}

/** `prompt`'s text less the text of each of `relayed` (`promptOf`), which are in its order: what
 * the rig wrote of it, the prefix of each relayed message's line and the newline after it
 * included. */
export function rigText(prompt: string, relayed: readonly RelayedAt[]): string {
  let text = "";
  let from = 0;
  for (const { start, end } of relayed) {
    text += prompt.slice(from, start);
    from = end;
  }
  return text + prompt.slice(from);
}

/** The paths of a run whose words are an agent's own whenever they are there, which the walker of
 * a run's files (`worldFiles`) passes over at every launch, each with why: everything else under
 * the run's directory is read at the run's first launch (AGENT_WRITTEN is read there alone), but
 * the HOME copied from the label's install, of which the files the rig writes are read alone
 * (RIG_HOME, LABELS_OWN), and a link, of which its target's path is read and never what it points
 * to. It is read by launch's guard before every agent (`worldNames`) and by rig.test.ts's "no file
 * rig.sh writes, copies or builds into a run before its agent starts names a word of the
 * measurement", which reads each scenario's composed run through the same walker. The
 * architect's ruling of 2026-11-21T13:57:03Z: the guard reads every file the rig wrote before the
 * agent starts, the built ones included, after the gh bundle (<run>/lib/gh.js) carried a
 * scenario's name into every run at 9083afb5, which no row had built. Each pattern is matched
 * against a path under the run's directory. */
export const AGENTS_OWN: readonly { readonly path: RegExp; readonly why: string }[] = [
  {
    path: /^reviewer-wake\.json$/,
    why: "each message one agent of the run sent the next, kept whole, whose words are its sender's (the architect's ruling of 2026-11-20T17:30Z); prompt.txt is read less each of them (`promptOf`, `rigText`)",
  },
  { path: /^sessions\//, why: "each agent's transcripts, its Oh My Pi session directory (launch)" },
  {
    path: /^out(?:-[^/]+)?\.txt$/,
    why: "each agent's own output, and each earlier agent's or turn's",
  },
  {
    path: /^boot-1\//,
    why: "a relaunched agent's first attempt: its output, its transcripts and the daemon stand-in's lines of it (boot_agent)",
  },
  { path: /^home-[^/]+\//, why: "the HOME an earlier agent of the run worked in (worker_boot)" },
  { path: /^tmp\//, why: "the agent's TMPDIR, which is its /tmp too (base_env, private_world)" },
  {
    path: /^(?:logs\/|calls\.jsonl$|published\.jsonl$|daemon\.log$|pushes\.log$)/,
    why: "the logs of what the agents did: the services' logs, and the run's records of each gh and bun call, each Envoy publish, each request to the daemon and each push its agents made",
  },
  {
    path: /^(?:state-\d+\.json|answers(?:-\d+)?\.jsonl)$/,
    why: "a spec run's asks as the agent wrote them on its issue after each turn (seed.ts state), and the person stand-in's answers to them (judge.ts answer)",
  },
  {
    path: /(?:^|\/)\.(?:jj|git)\/|^remote\.git\/(?!hooks\/)/,
    why: "the version-control records of the run's repositories, which keep each agent's commands, commits and branch names as it makes them (jj's operation log holds a command's own arguments); the trees, histories and hooks the rig builds are held by rig.test.ts's \"what the rig writes into a run's world\"",
  },
];

/** The paths of a run an agent of it can write, each with the write that reaches it, which the
 * walker of a run's files (`worldFiles`) reads at the run's first launch alone, when no agent has
 * run and each holds only what the rig wrote, and passes over at every later launch, when each
 * holds what an agent wrote as well: what an agent of the run wrote is that agent's own (the
 * architect's ruling of 2026-11-21T14:24:55Z, as the ruling of 2026-11-20T17:30Z holds for the
 * messages one agent sends the next). The rig writes nothing into them after the first launch:
 * rig.test.ts's "after a run's first launch the rig writes nothing into what an agent of the run
 * can write" composes every later launch it reaches, with agents that write nothing, and holds
 * each of these paths to the bytes the first launch read; and gh-standin.test.ts's "a write the gh
 * stand-in keeps in fixtures.json adds nothing the call did not give it" holds the one stand-in
 * that writes here. The other paths an agent writes are AGENTS_OWN's, read at no launch: its
 * transcripts, output and TMPDIR, the records of its calls, its pushes and its publishes, and the
 * version-control records of the run's repositories (the remote it pushes to through
 * <run>/bin/git-remote-<vcs>, but for the hooks the rig writes there, which a push writes none
 * of). Each pattern is matched against a path under the run's directory. */
export const AGENT_WRITTEN: readonly { readonly path: RegExp; readonly why: string }[] = [
  {
    path: /^cwd\//,
    why: "the agent's working directory, where launch starts it (launch.sh's `cd` and private_world's): every file it makes or edits there; before the first launch, what the scenario put there (stranded.csv, export-host.crontab, todo.py)",
  },
  {
    path: /^ws\//,
    why: "the issue workspace (fixture_workspace), the workspace a worker pane names, where each role writes its change with jj and git",
  },
  {
    path: /^repos\//,
    why: "the shared clone the workspace is a worktree of (fixture_workspace), which every jj and git command of a role in the workspace writes through",
  },
  {
    path: /^home\//,
    why: "the agent's HOME (base_env), where its Oh My Pi (PI_CODING_AGENT_DIR) and every tool that keeps a file under HOME write: a spec run's later turns keep it (spec_turns), and a worker run's later agent gets the label's afresh (worker_boot)",
  },
  {
    path: /^state\/home\//,
    why: "a worker pane's four XDG base directories under its LEGION_STATE_DIR (daemon-pane.go's pane, the tmux runtime's xdgDirectories), where its jj reads and writes the repository's configuration (fixture_workspace) and its tools keep their caches",
  },
  {
    path: /^state\/(?:legion-stall-exits-[^/]+\.json|omp-attribution-[^/]+\.toml)$/,
    why: "the Legion plugin's own files in its pane's LEGION_STATE_DIR: the count of its stall exits (pi-legion's stall-exits.ts) and its session's jj attribution layer (pi-shared's jj-attribution.ts)",
  },
  {
    path: /^fixtures\.json$/,
    why: "the gh answers, which the gh stand-in writes again on an agent's edit of the pull request's body (gh-standin.ts: `gh pr edit` with a body, a PATCH of the pull request with one)",
  },
];

/** The files of the run's HOME the rig writes, each with the write that makes it: of that HOME the
 * walker of a run's files (`worldFiles`) reads these alone. base_env copies the label's HOME into
 * the run whole (`cp -a "$I/home" "$R/home"`), and beside the rig's writes that HOME holds the
 * label's install (LABELS_OWN) and links into the host's trees (`hostProvided`), which the guard
 * does not read: it reads what the rig wrote for the run, not what the label's checkout or the
 * host provides (the architect's ruling of 2026-11-21T17:22:22Z). Each write is cmd_profile's,
 * into the label's install before any run, by a step of its own or by the checkout's installer it
 * runs; nothing of rig.sh writes into a run's HOME after base_env's copy. The HOME is
 * AGENT_WRITTEN's, so these are read at a run's first launch alone. Each pattern is matched
 * against a path under the run's directory. */
export const RIG_HOME: readonly { readonly path: RegExp; readonly why: string }[] = [
  {
    path: /^home\/\.jjconfig-operator-paths\.toml$/,
    why: "the overlay that pins each ~/ value of the operator's jj configuration to the operator's HOME and turns jj's fsmonitor off: make_omp_home (scripts/e2e/lib/omp-home.sh, its `: >\"$overlay\"` and two `>>\"$overlay\"`), which cmd_profile runs on the install's HOME",
  },
  {
    path: /^home\/\.omp\/profiles\/[^/]+\/agent\/config\.yml$/,
    why: "the profile's settings, which load_label reads: cmd_profile's install_model_route, through the route's installer (install-model-gateway.sh's and install-key-file-model-route.sh's `cat >\"$agent/config.yml\"` and omp_setup_wizard_off, or install_provider_route's `>\"$agent/config.yml\"`)",
  },
  {
    path: /^home\/\.omp\/profiles\/[^/]+\/agent\/models\.yml$/,
    why: "the profile's model route: cmd_profile's install_model_route, through the route's installer (install-model-gateway.sh's and install-key-file-model-route.sh's `cat >\"$agent/models.yml\"`, or install_provider_route's bun, which writes \"$agent/models.yml\")",
  },
  {
    path: /^home\/\.omp\/profiles\/[^/]+\/agent\/envoy\.json$/,
    why: "the Envoy configuration `rig.sh profile <label> <checkout> --envoy-json <file>` merges into the profile's agent directory (cmd_profile's jq and mv)",
  },
  {
    path: /^home\/\.omp\/profiles\/[^/]+\/agent\/skills\//,
    why: "the superpowers skills install_superpowers copies into the profile's user skills (`cp -RL`), for a label built with SKILL_SCENARIOS_SUPERPOWERS",
  },
];

/** The trees of the run's HOME that are the label's install, each with why, which the walker of a
 * run's files (`worldFiles`) passes over at every launch, beside AGENTS_OWN and AGENT_WRITTEN:
 * what the label's checkout installs is the product the run measures, and a production pane's
 * HOME holds it as well, so its words are no text of the rig's (the architect's ruling of
 * 2026-11-21T17:22:22Z). Of the rest of the HOME the walker reads RIG_HOME's files alone, and a
 * link there, as anywhere in the run, by the path it names (`worldFiles`). INFRA-764's round 31 at
 * c42409bc: the walker followed the plugins' links into the label's install and refused run
 * 4f4d19499cf8 on `var SPEC_WRITING_POINTER` in the Envoy plugin's dist/dispatch.js. Each pattern
 * is matched against a path under the run's directory. */
export const LABELS_OWN: readonly { readonly path: RegExp; readonly why: string }[] = [
  {
    path: /^home\/\.omp\/profiles\/[^/]+\/plugins\//,
    why: "the label's packed plugins, as `omp plugin install` puts them in the profile (install-plugin-profile.sh, which cmd_profile runs for pi-envoy and for pi-legion): omp-plugins.lock.json, and node_modules/@sjawhar/pi-envoy and pi-legion, links into the label's install (<work>/installs/<id>/pi-envoy and pi-legion)",
  },
];

/** Whether `file`, a path under a run's directory, is AGENTS_OWN's, which no launch reads. */
export function agentsOwn(file: string): boolean {
  return AGENTS_OWN.some(({ path: own }) => own.test(file));
}

/** Whether `file`, a path under a run's directory, is AGENT_WRITTEN's, which the first launch
 * alone reads. */
export function agentWritten(file: string): boolean {
  return AGENT_WRITTEN.some(({ path: written }) => written.test(file));
}

/** Whether `file`, a path under a run's directory, is LABELS_OWN's, which no launch reads. */
export function labelsOwn(file: string): boolean {
  return LABELS_OWN.some(({ path: own }) => own.test(file));
}

/** Whether `file`, a path under a run's directory, is a file the rig writes into the run's HOME
 * (RIG_HOME), which the first launch alone reads. */
export function rigHome(file: string): boolean {
  return RIG_HOME.some(({ path: written }) => written.test(file));
}

/** The run's record of its launches, a line for each (`<time> <tmux session>`), which rig.sh's
 * launch appends once the guard has passed and before the agent starts. It is under <run>/logs,
 * which every agent's namespace covers with an empty read-only tmpfs (rig.sh's world_paths and
 * private_world), a mount a namespace of the agent's own cannot take off either, so only the rig
 * writes it: rig.test.ts's "an agent of the run cannot write the run's record of its launches". */
export const LAUNCHES = "logs/launches";

/** Whether the agent about to start in `runDir` is the run's first: its record of launches
 * (LAUNCHES) is not there or holds none. */
export function firstLaunch(runDir: string): boolean {
  const file = path.join(runDir, LAUNCHES);
  return !existsSync(file) || readFileSync(file, "utf8").trim() === "";
}

/** The trees outside a run that a link of the run may point into, whose files the guard does not
 * read (`worldFiles`): the label's install and the host's. */
export interface Provided {
  /** The label's install, <work>/installs/<id> (load_label's I), which holds its packed plugins. */
  readonly install: string;
  /** The host's native module cache, which make_omp_home links each install's .omp/natives to. */
  readonly natives: string;
  /** The host's git exec path, which world_remote links each program of into <run>/lib/git-core. */
  readonly gitExec: string;
}

/** The trees the label and the host provide a run (Provided), with `install` the label's install
 * and the host's two as the rig made its links into them, read from `env`: the native module cache
 * as make_omp_home picks it (scripts/e2e/lib/omp-home.sh's `omp_natives_dir`),
 * $XDG_CACHE_HOME/omp/natives where XDG_CACHE_HOME is set and $XDG_CACHE_HOME/omp exists and
 * $HOME/.omp/natives otherwise, and the exec path as world_remote reads it, `git --exec-path`. A
 * throw, naming why, where git names none. */
export function hostProvided(
  install: string,
  env: Readonly<Record<string, string | undefined>> = process.env
): Provided {
  const cache = env.XDG_CACHE_HOME ?? "";
  const natives =
    cache !== "" && existsSync(path.join(cache, "omp"))
      ? path.join(cache, "omp", "natives")
      : path.join(env.HOME ?? "", ".omp", "natives");
  const git = Bun.spawnSync(["git", "--exec-path"], {
    env: { ...env },
    stdout: "pipe",
    stderr: "pipe",
  });
  const gitExec = git.stdout.toString().trim();
  if (git.exitCode !== 0 || gitExec === "")
    throw new Error(
      `\`git --exec-path\` named no exec path (exit ${git.exitCode}: ${git.stderr.toString().trim()}), so no link into it can be told from one outside the run`
    );
  return {
    install: path.resolve(install),
    natives: path.resolve(natives),
    gitExec: path.resolve(gitExec),
  };
}

/** Every entry under `dir`, by its path under it, in byte order, with what lstat says of it: a
 * link is an entry of its own, and nothing it points to is listed, so no walk of these enters one.
 * Bun's recursive readdirSync enters a link to a directory, and at c42409bc the walker read the
 * label's plugins and the host's natives (124 MiB) through the links of a run's HOME. */
export function runEntries(dir: string): { file: string; entry: Stats }[] {
  const entries: { file: string; entry: Stats }[] = [];
  const list = (below: string) => {
    for (const name of readdirSync(path.join(dir, below))) {
      const file = below === "" ? name : `${below}/${name}`;
      const entry = lstatSync(path.join(dir, file));
      entries.push({ file, entry });
      if (entry.isDirectory()) list(file);
    }
  };
  list("");
  return entries.sort((a, b) => (a.file < b.file ? -1 : a.file > b.file ? 1 : 0));
}

/** One thing of a run the walker read (`worldFiles`): its path under the run, and its text, which
 * for a link is the path the link names. */
export interface WorldFile {
  readonly file: string;
  readonly text: string;
  readonly link: boolean;
}

/** Everything under `runDir`, the run's directory, the agent about to start can read and the rig
 * wrote, in byte order (`runEntries`), one at a time, so the guard holds one file's text at a
 * time: with no part of AGENTS_OWN, and, at a later launch of the run (`firstLaunch`), with no part
 * of AGENT_WRITTEN either; of the HOME copied from the label's install, RIG_HOME's files alone,
 * and nothing of LABELS_OWN; by its path under the run and its text. No link is entered: a link's
 * text is the path it names (readlink), read for the guard's words, and where that path is decides
 * the rest. Inside the run, nothing more, since the walker reads what is there as itself. Inside
 * the label's install, the host's native module cache or its git exec path (`provided`), nothing
 * is read, those being the product's and the host's. Anywhere else, a throw naming the link: what
 * it points to is neither the rig's to read nor the label's or the host's to pass over (the
 * architect's ruling of 2026-11-21T17:22:22Z, on round 31, where the walker went through the HOME's
 * links into the plugins and the host's natives, refused every run and was killed at 1,170 MiB).
 * The prompt is the rig's text of it (`promptOf`, `rigText`), and services-from is read less the
 * rig's own directory, which the run's service provenance names in every run (services_from: each
 * program it runs, by where it is): a residual of the architect's ruling of 2026-11-18T08:40Z
 * (class-search.json's "<run>/services-from is readable under gVisor"). That directory is read
 * here, where only the launch guard (`worldNames`) and rig.test.ts's whole-world row reach it, and
 * no reader of a run (health.ts, score.ts, judge.ts), which reads a run by what it recorded
 * alone. */
export function* worldFiles(runDir: string, provided: Provided): Generator<WorldFile> {
  const run = path.resolve(runDir);
  const rigDirectory = realpathSync(import.meta.dir);
  const first = firstLaunch(run);
  const prompt = promptOf(run);
  const trees = [provided.install, provided.natives, provided.gitExec];
  for (const { file, entry } of runEntries(run)) {
    if (agentsOwn(file) || (!first && agentWritten(file))) continue;
    const at = path.join(run, file);
    if (entry.isSymbolicLink()) {
      const target = readlinkSync(at, "utf8");
      yield { file, text: target, link: true };
      const where = path.resolve(path.dirname(at), target);
      if (
        within(run, where) === undefined &&
        trees.every((tree) => within(tree, where) === undefined)
      )
        throw new Error(
          `<run>/${file} is a link to ${target}, outside the run, the label's install (${provided.install}), the host's native module cache (${provided.natives}) and its git exec path (${provided.gitExec}), so what it points to is neither the rig's to read nor the label's or the host's to pass over`
        );
      continue;
    }
    if (!entry.isFile() || labelsOwn(file) || (file.startsWith("home/") && !rigHome(file)))
      continue;
    if (file === "prompt.txt" && prompt !== undefined)
      yield { file, text: rigText(prompt.text, prompt.relayed), link: false };
    else if (file === "services-from")
      yield {
        file,
        text: readFileSync(at, "utf8").replaceAll(rigDirectory, "<the rig's directory>"),
        link: false,
      };
    else yield { file, text: readFileSync(at, "utf8"), link: false };
  }
}

/** The first word of the vocabulary (`vocabulary` of `labels`) in what the rig composes for the
 * agent about to start in `runDir`, as rig.sh's launch has written it: every variable of its pane
 * file, its name and its value (a value with a `/`, and a NAMED_VARS value, read as a name:
 * `nameWords`; any other as text: `textWords`), which holds its profile, its Legion project,
 * issue and tree (so the Envoy topics they make), its HOME and TMPDIR; every path its namespace
 * mounts (`mountedPaths`: each a mount point and the host source /proc/self/mountinfo names);
 * its working directory and session directory; its system prompt, which its command line,
 * /proc/1's, carries, and the text of the prompt file that argument names, which its Oh My Pi
 * reads (`systemPromptOf`, which refuses a system prompt it cannot read, and so does the guard);
 * and everything of the run's directory the rig wrote, by its path and its text, one file at a
 * time (`worldFiles`: its prompt less each message the rig relayed into it from another agent of
 * the run, which `promptOf` refuses to read where <run>/reviewer-wake.json cannot be parsed, and so
 * does the guard; nothing of AGENTS_OWN; of its HOME, RIG_HOME's files alone; each link by the path
 * it names, which `worldFiles` refuses where that path is outside the run and the trees `provided`
 * names, and so does the guard; and, at a later launch, nothing of AGENT_WRITTEN), each named with
 * the text around the word. */
export function worldNames(
  runDir: string,
  sessionsDir: string,
  labels: readonly string[],
  provided: Provided
): Named | undefined {
  const vocab = vocabulary(labels);
  const named = (place: string, value: string, name: boolean): Named | undefined => {
    const word = name ? nameWords(value, vocab) : textWords(value, vocab);
    if (word === undefined) return undefined;
    const at = Math.max(0, folded(value).search(literal(word)));
    return { place, word, value: value.slice(Math.max(0, at - 60), at + 140) };
  };
  const checks: { place: string; value: string; name: boolean }[] = [];
  for (const [key, value] of paneVars(path.join(runDir, "pane.env"))) {
    checks.push({ place: `the variable name ${key}`, value: key, name: true });
    checks.push({
      place: `the variable ${key}`,
      value,
      name: NAMED_VARS[key] === true || value.includes("/"),
    });
  }
  for (const mounted of mountedPaths(runDir))
    checks.push({ place: "a mount of its namespace", value: mounted, name: true });
  checks.push({ place: "its working directory", value: path.join(runDir, "cwd"), name: true });
  checks.push({ place: "its session directory", value: sessionsDir, name: true });
  const system = systemPromptOf(runDir);
  if (system !== undefined) {
    checks.push({ place: "<run>/system-args", value: system.argument, name: false });
    if (system.place !== "<run>/system-args")
      checks.push({ place: system.place, value: system.text, name: false });
  }
  for (const { place, value, name } of checks) {
    const found = named(place, value, name);
    if (found !== undefined) return found;
  }
  for (const { file, text, link } of worldFiles(runDir, provided)) {
    const found =
      named(`the path <run>/${file}`, file, false) ??
      named(link ? `the link <run>/${file}'s target` : `<run>/${file}`, text, false);
    if (found !== undefined) return found;
  }
  return undefined;
}

if (import.meta.main) {
  const [command, ...rest] = Bun.argv.slice(2);
  if (command === "case" && rest.length === 1) {
    const scenario = rest[0] ?? "";
    if (SCENARIOS[scenario] === undefined) {
      console.error(`run-names.ts: unknown scenario ${scenario}`);
      process.exit(2);
    }
    try {
      const found = caseOf(scenario, process.env);
      if (found !== "") console.log(found);
    } catch (error) {
      console.error(error instanceof Error ? error.message : String(error));
      process.exit(1);
    }
  } else if (command === "guard" && rest.length === 4) {
    const [runDir = "", sessionsDir = "", work = "", install = ""] = rest;
    let named: Named | undefined;
    try {
      named = worldNames(runDir, sessionsDir, workLabels(work), hostProvided(install));
    } catch (error) {
      // A world the guard cannot read whole (`systemPromptIn`'s refusals, a link outside the run
      // and the trees the label and the host provide) is one it cannot clear.
      console.error(
        `the world of ${path.basename(runDir)} cannot be read whole: ${error instanceof Error ? error.message : String(error)}`
      );
      process.exit(1);
    }
    if (named !== undefined) {
      console.error(
        `the world of ${path.basename(runDir)} names "${named.word}" in ${named.place}: ${named.value.slice(0, 200)}`
      );
      process.exit(1);
    }
  } else {
    console.error(
      "usage: run-names.ts case <scenario> | guard <run dir> <sessions dir> <work dir> <install>"
    );
    process.exit(2);
  }
}
