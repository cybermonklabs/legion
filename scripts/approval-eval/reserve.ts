import { createHash } from "node:crypto";
import { createReadStream, type Dirent } from "node:fs";
import { readdir, stat } from "node:fs/promises";
import { basename, isAbsolute, join, resolve } from "node:path";
import type { CaseSeed } from "./build-corpus";
import { RESERVE_ENDED, RESERVE_READS, type ReserveRead } from "./claim";
import type { CommentReply, DispatchEvent } from "./dispatch-read";

/**
 * The reserve of INFRA-2897's next final test: the cases no command of this directory reads
 * before that round's claim run, under the rule registered in
 * dispatch://INFRA/artifact/infra-2897-final-test-reserve-md before its cut, under
 * dispatch://INFRA-3189's approved method ("Live and final test apart", "Data sets").
 *
 * The issue tree is the unit, as the approved method's draw groups by tree: an ask that opened at
 * or after `RESERVE_TREE_CUT` is reserved when the first hexadecimal digit of the sha256 of
 * `RESERVE_TREE_TEXT` followed by the key of its tree's root is 0 to 7. The root is read at the
 * ask's first opening (`askTreeRoot`, the root a live scorer meets) and at the build's pinned event
 * (`issueTreeRoot`), and a case is held when the rule picks either: an issue moves between trees,
 * and a case a scorer may have skipped under either root stays out of every development read.
 * Holding more never lets a read case into the final test: the claim checks every final-test tree
 * against the scorers' own records of what they read (the registration's item 6).
 *
 * The registration's version 1, which held issues by their own key from 2026-11-14T06:00:00Z, was
 * released by the owner of the next round in writing (that document's "Version 1 released"):
 * version 2's item 6 takes no case that version 1 alone held, so those cases are development data.
 *
 * Version 3 holds only the trees the final test can take. From `RESERVE_LIST_CUT` an ask is held
 * when its tree's root, at its first opening or at the build, is not in a list registered with it:
 * every issue of each tree that held an ask through event `RESERVE_LIST_THROUGH`, except the trees
 * version 2 held whole (`TreeLog.reserveListOf`). The build takes that list from the log and
 * refuses one whose sha256 is not `RESERVE_LIST_SHA256`, and records for each case whether each
 * root is in it (`askRootListed`, `issueRootListed`). Version 2's hash picks nothing from that cut,
 * and its cases whose root at their ask's opening is in the list are released: such a tree had an
 * ask before version 2's cut or an ask whose root version 2 did not pick, so item 6 takes none of
 * them.
 *
 * A reserved tree, by its root's key, that a read has already spent is in `MOVED_TO_DEVELOPMENT`,
 * with the record of that read, and is held no more.
 *
 * `build-corpus.ts` writes no reserved case to the `corpus.jsonl` it builds, only their count to
 * its `snapshot.json`, so the file itself holds none for any reader to meet, and `readCorpus`
 * (`corpus-io.ts`) refuses every file that holds one, an older build's or the claim run's
 * included, so no command here reads them; each does so unless `APPROVAL_EVAL_RESERVE` is
 * `read:<reference>`, `claim.ts` registers that reference for this run's digest, and the run is
 * the one claim run: its build is the run that the first start reply to that reference names
 * (`claimRunOf`), and every later command names the same reply (`reserveAccessOf`, and `readCorpus`
 * against the build's `snapshot.json`). Once `claim.ts` records that the reserve ended, no case is
 * held.
 *
 * The rule itself is written once, in `reserveRuleOf`: a build reads it over the two roots it
 * records for each case (`reserveOf`), and a scorer that meets an ask live reads it over the root
 * Dispatch gives now (`reserve-ask.ts`), which refuses while the registration names a rule
 * version above `RESERVE_RULE_VERSION`.
 */

/** The cut: an ask that opened at or after it can be reserved by its tree. */
export const RESERVE_TREE_CUT = "2026-11-16T00:00:00Z";

/** The text the rule appends the key of a tree's root to before it hashes it, as registered. */
export const RESERVE_TREE_TEXT = "superfan-experiments INFRA-2897 reserve v2 2026-11-16:";

/** Version 3's cut: from it, an ask is held when its tree's root is not in version 3's list. */
export const RESERVE_LIST_CUT = "2026-11-18T00:00:00Z";

/** The last event of the log whose trees and asks version 3's list is taken from. */
export const RESERVE_LIST_THROUGH = 640951;

/** The sha256 of version 3's list as registered: its keys in order, each line ending in `\n`. */
export const RESERVE_LIST_SHA256 =
  "5f2b4e13fa8e8fc1f20ede4eab62f770504d292564eb8bffaf855472d8ff0a79";

/**
 * The highest rule version of the registration this code implements. A rule version is a
 * `## Version N` heading of the registration document (`RESERVE_DOCUMENT`). A live decision
 * (`reserve-ask.ts`) refuses while that document names a higher one, so a scorer running older
 * code holds the ask rather than applies a rule that no longer stands.
 */
export const RESERVE_RULE_VERSION = 3;

/** The registration document, as a project document of Dispatch: its project and its slug. */
export const RESERVE_DOCUMENT = { project: "INFRA", slug: "infra-2897-final-test-reserve-md" };

/** Version 3's list as registered: a project file at one version, whose sha256 is fixed above. */
export const RESERVE_LIST_FILE = {
  project: "INFRA",
  slug: "infra-2897-reserve-v3-list-txt",
  version: 1,
};

/** The environment variable that lets a command read the reserved cases. */
export const RESERVE_VARIABLE = "APPROVAL_EVAL_RESERVE";

/**
 * The reserved trees, by their root's key, that a read has already spent, each with the record
 * that says so. The rule's fifth item moves each into the development data, whole.
 */
export const MOVED_TO_DEVELOPMENT: Readonly<Record<string, string>> = {
  "INFRA-228":
    "the answer to one question of this tree, printed by the next round's owner while counting " +
    "the held cases between 2026-11-16T02:56:54Z and 02:58:13Z; the registration records it " +
    "under version 2, item 5",
};

const TREE_CUT_TIME = Date.parse(RESERVE_TREE_CUT);
const LIST_CUT_TIME = Date.parse(RESERVE_LIST_CUT);

/** The hexadecimal digits that pick a key: half of the sixteen. */
const PICKING_DIGITS = "01234567";

/** Whether the rule's hash picks the key of a tree's root, whenever its asks opened. */
export function pickedRoot(rootKey: string): boolean {
  const digest = createHash("sha256")
    .update(`${RESERVE_TREE_TEXT}${rootKey}`, "utf8")
    .digest("hex");
  return PICKING_DIGITS.includes(digest.charAt(0));
}

/**
 * An ask's time, which every cut is read against: its first opening, or, where that is missing or
 * does not parse, its answer, which comes after the opening.
 */
export function askTimeOf(row: Pick<CaseSeed, "askCreatedAt" | "answeredAt">): number {
  const opened = Date.parse(row.askCreatedAt ?? "");
  return Number.isNaN(opened) ? Date.parse(row.answeredAt) : opened;
}

/** The rule versions a registration document names: the N of each `## Version N` heading. */
export function ruleVersionsOf(markdown: string): number[] {
  return [...markdown.matchAll(/^## Version (\d+)\b/gm)].map((match) => Number(match[1]));
}

/** What the rule reads of one ask. A build reads it from the log, a live decision from Dispatch. */
export interface ReserveFacts {
  /** The ask's first opening, as `askTimeOf` reads it. */
  askTime: number;
  /** Whether the ask opened on an issue. */
  onIssue: boolean;
  /** Each root of the ask's tree the case is read under; null where a walk could not name one. */
  roots: readonly (string | null)[];
  /** Whether the root at the ask's first opening is in version 3's list. */
  openingRootListed: boolean;
  /** Whether every root in `roots` is in version 3's list. */
  everyRootListed: boolean;
}

/**
 * The registered rule over the facts of one ask, the one place it is written for every reader of
 * this directory. An ask on no issue, or one that opened before version 2's cut, is kept, and a
 * case is kept when a read spent its tree. Before version 3's cut a case is held when version 2's
 * rule picks one of its roots (a root no walk could name picks nothing), unless its root at the
 * opening is in version 3's list, which released it. From version 3's cut a case is held unless
 * every root is in that list. Once `ended` names the record that ended the reserve (`claim.ts`),
 * every case is kept.
 */
export function reserveRuleOf(
  facts: ReserveFacts,
  ended: string | null = RESERVE_ENDED
): "held" | "kept" {
  if (ended !== null || !facts.onIssue || facts.askTime < TREE_CUT_TIME) return "kept";
  const roots = facts.roots.filter((root): root is string => root !== null);
  if (roots.some((root) => Object.hasOwn(MOVED_TO_DEVELOPMENT, root))) return "kept";
  if (facts.askTime >= LIST_CUT_TIME) return facts.everyRootListed ? "kept" : "held";
  if (facts.openingRootListed) return "kept";
  return roots.some(pickedRoot) ? "held" : "kept";
}

/**
 * Whether a built case is held by the reserve (`reserveRuleOf`). The case is read under two
 * roots, its tree's root at its ask's first opening and at the build's pinned event: an issue
 * moves between trees, and a case a scorer may have skipped under either root stays out of every
 * development read. The root at the build releases nothing: it moves with each parent change, so
 * a tree moved under a listed issue for one build would put its cases in a development corpus and
 * could then move back. From version 2's cut, `unknown` is a case from a corpus built before
 * `build-corpus.ts` recorded its tree's roots, which a caller refuses. A root the build did not
 * look up in version 3's list (an older build, or one pinned before the list's event) counts as
 * not in it, so such a build releases nothing and holds every case from version 3's cut.
 */
export function reserveOf(
  row: CaseSeed,
  ended: string | null = RESERVE_ENDED
): "held" | "kept" | "unknown" {
  if (ended !== null || row.issueKey === null) return "kept";
  const askTime = askTimeOf(row);
  if (askTime < TREE_CUT_TIME) return "kept";
  if (row.askTreeRoot === undefined || row.issueTreeRoot === undefined) return "unknown";
  return reserveRuleOf(
    {
      askTime,
      onIssue: true,
      roots: [row.askTreeRoot, row.issueTreeRoot],
      openingRootListed: row.askRootListed === true,
      everyRootListed: row.askRootListed === true && row.issueRootListed === true,
    },
    ended
  );
}

/**
 * What `APPROVAL_EVAL_RESERVE` asks: `null` when it is unset or empty, so the reserve is held;
 * the reference when it is `read:<reference>`; and `undefined` for any other value, which a
 * caller refuses rather than guess at.
 */
export function readReferenceOf(value: string | undefined): string | null | undefined {
  const trimmed = value?.trim() ?? "";
  if (trimmed === "") return null;
  if (!trimmed.startsWith("read:")) return undefined;
  const reference = trimmed.slice("read:".length).trim();
  return reference === "" ? undefined : reference;
}

/** The repository root, which the bundle's directories are under. */
export const REPOSITORY_ROOT = join(import.meta.dir, "..", "..");

/** The directories, under the repository root, whose files are the bundle a run digest covers. */
export const BUNDLE_DIRECTORIES = ["scripts/approval-eval", "skills/approval-review"] as const;

/** What under those directories is outside the bundle: the registration itself, and the tests. */
const OUTSIDE_BUNDLE = ["scripts/approval-eval/claim.ts", "scripts/approval-eval/__tests__"];

/**
 * Options whose value names a directory a run reads or writes: the digest takes it as a path.
 * `--draw` is the draw directory `s10-inputs.ts` and the claim's analysis read, `--inputs` the
 * inputs directory `s10_judge.py` reads, and `--claim-s10` the judged directory the claim's
 * analysis (`analyze.ts --claim-s10`) reads, whose files the claim run's stage records pin by
 * their sha256 instead.
 */
const PATH_OPTIONS: readonly string[] = [
  "--corpus",
  "--out",
  "--round",
  "--run",
  "--slices",
  "--cache",
  "--draw",
  "--inputs",
  "--claim-s10",
];

/**
 * Each command's options whose value names a file it reads: the digest takes the value as written
 * and the bytes of the file at the trimmed value, as the commands trim it. An option is listed by
 * command because one name can mean a file in one command and a count in another (`--cases`).
 * `--omp` names an executable, which a bare name finds on `PATH`. A command that gains a file
 * option adds it here, or a change to that file does not move its digest.
 */
const FILE_OPTIONS: Readonly<Record<string, readonly string[]>> = {
  "analyze.ts": ["--cases"],
  "draw.ts": ["--backlog-roots", "--scorer-records", "--round-sessions"],
  "fresh-agent.ts": ["--omp"],
  "refusals.ts": ["--fixtures", "--distillation"],
  "run-round.ts": ["--arms", "--distillation"],
  "s10_judge.py": ["--code", "--skill", "--never-decide"],
};

/**
 * The environment that chooses the servers a run reads: the model server, and the Dispatch server
 * its documents and neighbours come from. Credentials are left out: a rolled key is no other run.
 */
export const RESULT_ENVIRONMENT = ["ANTHROPIC_BASE_URL", "DISPATCH_URL"] as const;

/** A run as its digest reads it. */
export interface Run {
  /** The arguments as `process.argv` gives them: the runtime, the command, then its own. */
  argv: readonly string[];
  env: Readonly<Record<string, string | undefined>>;
  /**
   * Lines a command adds for what it runs from outside the repository, read only when a digest is
   * made (`fresh-agent.ts`: the agent it runs and that agent's models file).
   */
  inputs?: () => Promise<readonly string[]>;
}

/** The run reads something a digest cannot cover, so no registration can name the run. */
export class DigestError extends Error {
  override readonly name = "DigestError";
}

/**
 * The sha256 of a file's bytes, following a link to it, or `missing` where nothing is there. A
 * path that names something other than a regular file (a pipe, a device, a directory) is refused
 * (`DigestError`): a command can read different bytes from it on each run.
 */
export async function bytesDigestOf(path: string): Promise<string> {
  const found = await stat(path).catch(() => null);
  if (found === null) return "missing";
  if (!found.isFile()) {
    throw new DigestError(
      `${path} is not a regular file, so no digest covers what is read from it`
    );
  }
  const hash = createHash("sha256");
  for await (const chunk of createReadStream(path)) hash.update(chunk);
  return hash.digest("hex");
}

/**
 * The digest of one run, which `claim.ts` registers: the sha256 of a text that names the command,
 * every file of the bundle by its path and the sha256 of its bytes, each argument of the run, the
 * environment `RESULT_ENVIRONMENT` names, and the lines the command adds (`Run.inputs`). An
 * argument is taken by what its option means. A `PATH_OPTIONS` value is taken as a path, so where
 * a corpus or an output lives does not change the digest. A `FILE_OPTIONS` value is taken as
 * written and by the bytes of its file, so an arms file (`--arms`) or a distillation
 * (`--distillation`) is pinned by what it holds. Every other value is taken as written: an arm, a
 * model, a number, never looked up on the disk. The value of a directory or file option is the
 * next argument whatever it starts with, as `parseArgs` reads it. A link in the bundle, or a file
 * option that names something other than a regular file, is refused (`DigestError`).
 *
 * What it leaves open: the files inside a directory an argument names (a corpus, a draw, an `--out`
 * that holds an earlier run's evidence or answers) are not in the digest; and what the model and
 * Dispatch servers answer is live. A registered digest admits only the one claim run, the run its
 * registration comment's first start reply names (`claimRunOf`), not every run made with it.
 */
export async function runDigestOf(run: Run, root: string): Promise<string> {
  const command = basename(run.argv[1] ?? "");
  const fileOptions = FILE_OPTIONS[command] ?? [];
  const lines = ["approval-eval run digest 4", `command ${command}`];
  const files: string[] = [];
  for (const directory of BUNDLE_DIRECTORIES) {
    const found = await filesUnder(root, directory);
    if (found === null) lines.push(`missing ${directory}`);
    else files.push(...found);
  }
  files.sort();
  for (const file of files) lines.push(`file ${file} ${await bytesDigestOf(join(root, file))}`);
  let option: string | null = null;
  for (const argument of run.argv.slice(2)) {
    const equals = argument.startsWith("--") ? argument.indexOf("=") : -1;
    if (option !== null && (PATH_OPTIONS.includes(option) || fileOptions.includes(option))) {
      lines.push(await valueLine(option, argument, run.env, fileOptions));
      option = null;
    } else if (equals > 0) {
      const name = argument.slice(0, equals);
      const value = argument.slice(equals + 1);
      lines.push(`flag ${name}`, await valueLine(name, value, run.env, fileOptions));
      option = null;
    } else if (argument.startsWith("-")) {
      lines.push(`flag ${argument}`);
      option = argument;
    } else {
      lines.push(await valueLine(option, argument, run.env, fileOptions));
      option = null;
    }
  }
  for (const name of RESULT_ENVIRONMENT) {
    lines.push(`env ${name} ${JSON.stringify(run.env[name] ?? null)}`);
  }
  for (const line of (await run.inputs?.()) ?? []) lines.push(`input ${line}`);
  return createHash("sha256")
    .update(`${lines.join("\n")}\n`, "utf8")
    .digest("hex");
}

/** The files of the bundle under one directory, by their paths from the root; null if it is not there. */
async function filesUnder(root: string, directory: string): Promise<string[] | null> {
  let entries: Dirent[];
  try {
    entries = await readdir(join(root, directory), { withFileTypes: true });
  } catch {
    return null;
  }
  const found: string[] = [];
  for (const entry of entries) {
    const path = `${directory}/${entry.name}`;
    if (OUTSIDE_BUNDLE.includes(path)) continue;
    if (entry.isDirectory()) found.push(...((await filesUnder(root, path)) ?? []));
    else if (entry.isFile()) found.push(path);
    else
      throw new DigestError(`${path} is not a regular file or directory, so no digest covers it`);
  }
  return found;
}

/** One argument value as the digest takes it, by the option it is the value of. */
async function valueLine(
  option: string | null,
  value: string,
  env: Run["env"],
  fileOptions: readonly string[]
): Promise<string> {
  if (option !== null && PATH_OPTIONS.includes(option)) return "value path";
  const written = `value ${JSON.stringify(value)}`;
  if (option === null || !fileOptions.includes(option)) return written;
  const trimmed = value.trim();
  if (option !== "--omp") return `${written} ${await bytesDigestOf(trimmed)}`;
  const executable = executableOf(trimmed, env.PATH);
  if (!isAbsolute(executable)) {
    throw new DigestError(`--omp ${trimmed} is not found on PATH, so no digest covers the agent`);
  }
  return `${written} ${await bytesDigestOf(executable)}`;
}

/**
 * The absolute path of the executable an `--omp` names: a path with a slash from the working
 * directory, a bare name from `PATH`, as the shell finds it. `fresh-agent.ts` runs this exact path
 * for every case, so the executable the digest reads is the one each case runs, whatever working
 * directory a case runs in. A bare name that `PATH` does not find is returned as it is, and a
 * digest refuses it, since the runtime could still find something else under that name.
 */
export function executableOf(omp: string, path: string | undefined): string {
  if (omp.includes("/")) return resolve(omp);
  const found = Bun.which(omp, { PATH: path ?? "" });
  return found === null ? omp : resolve(found);
}

/** What `claim.ts` registers: the reads of the reserve, and the record that ended it. */
export interface Registration {
  reads: readonly ReserveRead[];
  ended: string | null;
}

/** The registration `claim.ts` commits. */
export const REGISTRATION: Registration = { reads: RESERVE_READS, ended: RESERVE_ENDED };

/** What one run may do with the reserve, or why it may not run. */
export type ReserveAccess =
  | { kind: "held" }
  | { kind: "read"; reference: string }
  | { kind: "ended"; record: string }
  | { kind: "refused"; reason: string };

/**
 * The environment variable that names the claim run: the id of the reply to the registration
 * comment that started it (`claimRunOf`). It is outside the run digest, which is registered before
 * the run starts.
 */
export const CLAIM_RUN_VARIABLE = "APPROVAL_EVAL_CLAIM_RUN";

/** The words a reply to the claim's registration comment starts with to start the claim run. */
export const CLAIM_RUN_START = "approval-eval claim run starts";

/** Reads the replies to one Dispatch comment, by the comment's id (`dispatchRepliesOf`). */
export type RepliesOf = (commentId: string) => Promise<readonly CommentReply[]>;

/** The id of the comment a reference names, `dispatch://<item>/comment/<id>`, or null. */
export function registrationCommentOf(reference: string): string | null {
  return /^dispatch:\/\/\S+\/comment\/([^/#?\s]+)$/.exec(reference)?.[1] ?? null;
}

/**
 * A time Dispatch recorded, in microseconds since the epoch, or null when it does not parse.
 * Dispatch writes up to six digits of a second's fraction and drops trailing zeros, and
 * `Date.parse` keeps three, so the digits after the third are added here.
 */
export function microsecondsOf(time: string): number | null {
  const milliseconds = Date.parse(time);
  if (Number.isNaN(milliseconds)) return null;
  const fraction = /\.(\d+)Z$/.exec(time)?.[1] ?? "";
  return milliseconds * 1000 + Number(`${fraction}000000`.slice(3, 6));
}

/**
 * Whether this run is the one claim run the registered rule lets read the reserve (its item 7,
 * "the one claim run"), when `claim.ts` registers `reference` for this run's digest. The reference
 * names the claim's registration, a Dispatch comment. The claim run starts with a reply to that
 * comment whose body starts with `CLAIM_RUN_START`, and the first such reply, by the time Dispatch
 * recorded it, is the claim run: a run reads only when `APPROVAL_EVAL_CLAIM_RUN` names that reply's
 * id. A second claim run starts with a later reply, so it is refused, and the refusal names the
 * first, whose read spent the final test. Dispatch's API has no route that deletes a comment; a
 * reply edited after it was written is refused whole, since an edit can change which start came
 * first. A run that resumes the claim run names the same reply, so the code cannot tell a resume
 * from a second run made under that id: the start reply is the record of which run read the
 * final test. A thread Dispatch does not serve is refused too, and so are two starts at the
 * earliest time Dispatch recorded: no order of theirs is on record.
 *
 * The claim run's build asks it (`build-corpus.ts`) and records the reply in `snapshot.json`;
 * every later command of the run reads only that build's corpus, and only while
 * `APPROVAL_EVAL_CLAIM_RUN` names the same reply (`readCorpus` in `corpus-io.ts`).
 */
export async function claimRunOf(
  reference: string,
  named: string | undefined,
  repliesOf: RepliesOf
): Promise<{ kind: "claim"; id: string } | { kind: "refused"; reason: string }> {
  const comment = registrationCommentOf(reference);
  if (comment === null) {
    return {
      kind: "refused",
      reason:
        `claim.ts registers ${reference}, which names no Dispatch comment. The claim run starts ` +
        "with a reply to its registration comment, so a registered read names that comment " +
        "(dispatch://<item>/comment/<id>)",
    };
  }
  let replies: readonly CommentReply[];
  try {
    replies = await repliesOf(comment);
  } catch (error) {
    const why = error instanceof Error ? error.message : String(error);
    return {
      kind: "refused",
      reason: `the replies to ${reference} could not be read, so no run can show it is the claim run: ${why}`,
    };
  }
  const edited = replies.find((reply) => reply.edited_at !== null);
  if (edited !== undefined) {
    return {
      kind: "refused",
      reason:
        `reply ${edited.id} to ${reference} was edited at ${edited.edited_at}, and an edit can ` +
        "change which reply started the claim run, so no run reads the reserve under this " +
        "registration: claim.ts registers the read anew",
    };
  }
  const starts = replies
    .filter((reply) => reply.body.trimStart().startsWith(CLAIM_RUN_START))
    .map((reply) => ({ reply, at: microsecondsOf(reply.created_at) }));
  const untimed = starts.find((start) => start.at === null);
  if (untimed !== undefined) {
    return {
      kind: "refused",
      reason: `start reply ${untimed.reply.id} to ${reference} has no time Dispatch recorded, so no start can be shown first`,
    };
  }
  starts.sort((a, b) => (a.at ?? 0) - (b.at ?? 0));
  const [first, next] = starts;
  if (first === undefined) {
    return {
      kind: "refused",
      reason:
        `no claim run has started under ${reference}: reply to it with a body that starts ` +
        `"${CLAIM_RUN_START}", then set ${CLAIM_RUN_VARIABLE} to that reply's id`,
    };
  }
  if (next !== undefined && next.at === first.at) {
    return {
      kind: "refused",
      reason:
        `start replies ${first.reply.id} and ${next.reply.id} to ${reference} have the same ` +
        `time, ${first.reply.created_at}, so no start can be shown first: claim.ts registers the ` +
        "read anew",
    };
  }
  const id = named?.trim() ?? "";
  if (id === first.reply.id) return { kind: "claim", id };
  const second = starts.find((start) => start.reply.id === id);
  return {
    kind: "refused",
    reason:
      `the claim run under ${reference} is reply ${first.reply.id}, started at ` +
      `${first.reply.created_at}, and its read spent the final test: a second claim run makes it ` +
      'development data (dispatch://INFRA-3189, "Data sets"). ' +
      (id === ""
        ? `${CLAIM_RUN_VARIABLE} is not set`
        : second === undefined
          ? `${CLAIM_RUN_VARIABLE} names ${id}, which is no start reply to it`
          : `${CLAIM_RUN_VARIABLE} names ${id}, a later start, at ${second.reply.created_at}`),
  };
}

/**
 * What a run may do with the reserve, from `APPROVAL_EVAL_RESERVE`'s value, the registration, the
 * run, and the repository root. A reserve that ended holds nothing, whatever the variable says.
 * With the variable unset, the reserve is held. `read:<reference>` reads it only when the
 * registration lists that reference with this run's digest; the build then asks whether the run
 * is the one claim run (`claimRunOf`). Any other value is refused, and a refusal of a reference
 * names the run's digest, which is what a registration copies.
 */
export async function reserveAccessOf(
  value: string | undefined,
  registration: Registration,
  run: Run,
  root: string
): Promise<ReserveAccess> {
  if (registration.ended !== null) return { kind: "ended", record: registration.ended };
  const reference = readReferenceOf(value);
  if (reference === null) return { kind: "held" };
  if (reference === undefined) {
    return {
      kind: "refused",
      reason: `${RESERVE_VARIABLE} takes read:<reference>, naming a read claim.ts registers, or no value at all`,
    };
  }
  let digest: string;
  try {
    digest = await runDigestOf(run, root);
  } catch (error) {
    if (error instanceof DigestError) return { kind: "refused", reason: error.message };
    throw error;
  }
  const named = registration.reads.filter((read) => read.reference === reference);
  if (named.some((read) => read.run === digest)) return { kind: "read", reference };
  const why =
    named.length === 0
      ? `claim.ts registers no read under ${reference}`
      : `claim.ts registers ${reference} for other runs, and a change to a file of the bundle, ` +
        "an argument, a file an argument names, the servers or the command changes a run's digest";
  return {
    kind: "refused",
    reason:
      `${why}. The reserve of INFRA-2897's next final test opens only for a read that claim.ts ` +
      `lists, committed before the read. This run's digest is ${digest}`,
  };
}

/** Each issue's parent, by key, as far as a walk of the log has read: null for a root. */
export type IssueParents = Map<string, string | null>;

/** How many parents a chain is followed through before it is taken for a cycle. */
const TREE_DEPTH = 64;

/**
 * Records an issue's parent from an event of the log that carries the whole issue: an `issue.`
 * event whose payload holds the issue's `key` and a `parent` field (`issue.created`,
 * `issue.updated`, `issue.closed`). The latest such event read is the parent at that point of the
 * walk, so a parent that changes later does not reach back to an ask that opened earlier.
 */
export function noteIssueParent(parents: IssueParents, event: DispatchEvent): void {
  if (!event.type.startsWith("issue.")) return;
  const key = event.payload.key;
  if (typeof key !== "string" || !Object.hasOwn(event.payload, "parent")) return;
  const value = event.payload.parent;
  parents.set(key, typeof value === "string" && value !== "" ? value : null);
}

/**
 * The root of an issue's tree as `parents` records it: the issue reached by following each
 * issue's parent until one has none. Null when the chain reaches an issue `parents` does not
 * know, comes back to an issue it passed, or runs past `TREE_DEPTH` parents: such a root cannot
 * be named, and picks nothing.
 */
export function treeRootOf(
  issueKey: string,
  parents: ReadonlyMap<string, string | null>
): string | null {
  const passed = new Set<string>();
  let at = issueKey;
  while (passed.size <= TREE_DEPTH) {
    if (!parents.has(at) || passed.has(at)) return null;
    passed.add(at);
    const parent = parents.get(at) ?? null;
    if (parent === null) return at;
    at = parent;
  }
  return null;
}

/**
 * Records the root of an ask's tree at the ask's first opening, from its `ask.opened` event and
 * the parents read before it, keeping the first opening an ask id has: the root a live scorer
 * meets. An ask that opened on no issue records null.
 */
export function noteAskRoot(
  roots: Map<string, string | null>,
  parents: ReadonlyMap<string, string | null>,
  event: DispatchEvent
): void {
  if (event.type !== "ask.opened") return;
  const askId = event.payload.id;
  if (typeof askId !== "string" || roots.has(askId)) return;
  const own = event.payload.issue_key;
  const issueKey = typeof own === "string" ? own : event.issue_key;
  roots.set(askId, issueKey === null ? null : treeRootOf(issueKey, parents));
}
