import { createHash } from "node:crypto";
import { createReadStream, type Dirent } from "node:fs";
import { readdir, stat } from "node:fs/promises";
import { basename, isAbsolute, join, resolve } from "node:path";
import type { CaseRow } from "./build-corpus";
import { RESERVE_ENDED, RESERVE_READS, type ReserveRead } from "./claim";
import type { DispatchEvent } from "./dispatch-read";

/**
 * The reserve of INFRA-2897's next final test: the cases no command of this directory reads
 * before that round's claim run.
 *
 * The rule was registered on 2026-11-14, before its cut, under dispatch://INFRA-3189's approved
 * method ("Live and final test apart", "Grouping"):
 * dispatch://INFRA/artifact/infra-2897-final-test-reserve-md. An issue is reserved when it was
 * created at or after `RESERVE_CUT` and the first hexadecimal digit of the sha256 of
 * `RESERVE_TEXT` followed by its key is 0 to 7. The issue is the unit, as the grouping rule makes
 * it and as the rule's fifth item reads it: every case on a reserved issue is held, its approval
 * requests and its questions alike, so that no development read learns what the person answered
 * on an issue the final test will draw. An issue that a read has already spent is in
 * `MOVED_TO_DEVELOPMENT`, with the record of that read, and is held no more.
 *
 * `readCorpus` (`corpus-io.ts`) holds the reserved cases out of every corpus it reads, so every
 * command here skips them, unless `APPROVAL_EVAL_RESERVE` is `read:<reference>` and `claim.ts`
 * registers that reference for this run's digest (`reserveAccessOf`). Once `claim.ts` records
 * that the reserve ended, no case is held.
 */

/** The cut: an issue created at or after it can be reserved. */
export const RESERVE_CUT = "2026-11-14T06:00:00Z";

/** The text the issue key is appended to before it is hashed, as the registration writes it. */
export const RESERVE_TEXT = "superfan-experiments INFRA-2897 reserve 2026-11-14:";

/** The environment variable that lets a command read the reserved cases. */
export const RESERVE_VARIABLE = "APPROVAL_EVAL_RESERVE";

/**
 * The reserved issues a read has already spent, each with the record that says so. The rule's
 * fifth item moves such an issue into the development data, whole.
 */
export const MOVED_TO_DEVELOPMENT: Readonly<Record<string, string>> = {
  "INFRA-3689":
    "a question's answer read on 2026-11-15 at 04:24Z by the super fan of the live experiments " +
    "(dispatch://INFRA/artifact/superfan-experiments-log-md, round 3, c4)",
};

const CUT_TIME = Date.parse(RESERVE_CUT);

/** The hexadecimal digits that pick a key: half of the sixteen. */
const PICKING_DIGITS = "01234567";

/** Whether the rule's hash picks an issue key, whatever the issue's creation time. */
export function pickedKey(issueKey: string): boolean {
  const digest = createHash("sha256").update(`${RESERVE_TEXT}${issueKey}`, "utf8").digest("hex");
  return PICKING_DIGITS.includes(digest.charAt(0));
}

/**
 * Whether a case is held by the reserve.
 *
 * `held` and `kept` are the rule's answer. `unknown` is a case the rule could hold that carries
 * no `issueCreatedAt` at all: its corpus was built before `build-corpus.ts` recorded when each
 * issue was created, and only a new build can say. A case answered before the cut is never held,
 * since an issue created at or after the cut has no answer before it. An issue whose creation the
 * build did not find (`issueCreatedAt` null), or whose time does not parse, is held: nothing shows
 * that it was created before the cut. Once `ended` names the record that ended the reserve
 * (`claim.ts`), every case is kept.
 */
export function reserveOf(
  row: CaseRow,
  ended: string | null = RESERVE_ENDED
): "held" | "kept" | "unknown" {
  if (ended !== null) return "kept";
  const key = row.issueKey;
  if (key === null || !pickedKey(key) || Object.hasOwn(MOVED_TO_DEVELOPMENT, key)) return "kept";
  const answered = Date.parse(row.answeredAt);
  if (!Number.isNaN(answered) && answered < CUT_TIME) return "kept";
  if (row.issueCreatedAt === undefined) return "unknown";
  if (row.issueCreatedAt === null) return "held";
  const created = Date.parse(row.issueCreatedAt);
  return Number.isNaN(created) || created >= CUT_TIME ? "held" : "kept";
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

/** Options whose value names a directory a run reads or writes: the digest takes it as a path. */
const PATH_OPTIONS: readonly string[] = [
  "--corpus",
  "--out",
  "--round",
  "--run",
  "--slices",
  "--cache",
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
  "draw.ts": ["--backlog-roots"],
  "fresh-agent.ts": ["--omp"],
  "refusals.ts": ["--fixtures", "--distillation"],
  "run-round.ts": ["--arms", "--distillation"],
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
 * What it leaves open: a registered run can be made again until the reserve ends; the files inside
 * a directory an argument names (a corpus, a draw, an `--out` that holds an earlier run's evidence
 * or answers) are not in the digest; and what the model and Dispatch servers answer is live.
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
 * What a run may do with the reserve, from `APPROVAL_EVAL_RESERVE`'s value, the registration, the
 * run, and the repository root. A reserve that ended holds nothing, whatever the variable says.
 * With the variable unset, the reserve is held. `read:<reference>` reads it only when the
 * registration lists that reference with this run's digest. Any other value is refused, and a
 * refusal of a reference names the run's digest, which is what a registration copies.
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

/**
 * Records an issue's creation time from an `issue.created` event of the log, keeping the first
 * one a key has. The payload's own `created_at` is the issue's; the event's is the fallback.
 */
export function noteIssueCreated(times: Map<string, string>, event: DispatchEvent): void {
  if (event.type !== "issue.created" || event.issue_key === null) return;
  if (times.has(event.issue_key)) return;
  const own = event.payload.created_at;
  times.set(event.issue_key, typeof own === "string" ? own : event.created_at);
}
