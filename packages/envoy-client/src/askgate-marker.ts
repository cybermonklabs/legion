import { createHash, randomBytes } from "node:crypto";
import {
  closeSync,
  constants,
  mkdirSync,
  openSync,
  readdirSync,
  readFileSync,
  readlinkSync,
  renameSync,
  rmSync,
  statSync,
  unlinkSync,
  utimesSync,
  writeFileSync,
} from "node:fs";
import { basename, isAbsolute, join, resolve } from "node:path";
import { hasErrnoCode, messageFor } from "./errors";

/**
 * AskGate's enforcement at the posting `dispatch` CLI, shared by both sides.
 *
 * The record. While an Oh My Pi process whose gate is bound runs a session, pi-envoy keeps an
 * owner file in the `askgate/` directory of that session's CLI state directory
 * (`<root>/sessions/<id>/askgate/<pid>-<start>-<ns>.json`): the process's id, its Linux start time
 * (field 22 of `/proc/<pid>/stat`, null elsewhere), its pid namespace (the inode of
 * `/proc/self/ns/pid`, null elsewhere) and its marker store. Each process writes and removes only
 * its own file, so two processes that bind one session at the same time never drop each other. An
 * owner counts while its process lives and, on Linux, has that start time, so a crashed session's
 * file, or one whose pid another process took, is inert. A pid and a start time mean something only
 * in the namespace they were read in, and one state directory is shared by the host and every agent
 * box on it, each box its own pid namespace. So a process judges by pid only an owner of its own
 * namespace; an owner of another one counts while its store changed within MARKER_MAX_AGE_MS, which
 * a live process keeps true by touching its store every STORE_BEAT_MS (`keepStore`). A session no
 * live process records (a terminal, CI, Claude Code, a pod without pi-envoy, a session with
 * Dispatch or AskGate off) needs no marker.
 *
 * The marker. For each literal `dispatch` call it lets run, the gate mints one file in the store,
 * `<store>/markers/<sha256 of the argv>.<32 random hex>`, created owner-only and exclusively,
 * bound to the exact argument vector the CLI receives and, for a reviewed write, to the digest of
 * the arguments its dry run printed and to the sha256 of each file the argv names
 * (`fileOperands`), as the gate read it for the review. Nothing of it is in the command the agent
 * sees.
 *
 * The CLI. Before a gated write posts, with a live record, it reads each file the argv names once,
 * and consumes one matching marker by unlinking it: exactly one process wins an unlink, so a
 * marker is spent by one write alone. What it does when no marker is consumed rests on one
 * question, since AskGate mints nothing for a call its review rejects: did a verdict exist? A
 * verdict AskGate wrote down (`readVerdict`) holds the write in either mode; a marker of this
 * argv that the verdict does not cover — other arguments, a file whose bytes changed, one past
 * its age — holds it where the session chose block; evidence that exists and cannot be read does
 * the same, since a refusal may be what it holds. Only a store read whole and holding neither
 * lets the write post, with a warning on both streams, whatever mode the session chose: that is
 * a call no review ever saw. A refusal consumes nothing, so the same call meets the same answer
 * when it is sent again. The process makes its store again before each write of its own owner
 * file (`keepStore`), so a store another process removed never outlives the owner's next call.
 * Each owner file of the session's record carries that owner's check mode, so the mode is the
 * session's and no other session of the same process can change it; a file with no mode is warn.
 */

/** The record's directory in a session's state directory: one file for each owner. */
export const ASKGATE_RECORD = "askgate";
/**
 * How old a marker may be when the CLI consumes it. The gate mints a marker when it prepares the
 * calls of a message, and the call can then wait behind an exclusive call of the same message for
 * up to that call's limit (an eval cell's is 3,600 s), so this is above the longest tool limit. The
 * gate releases each marker when its call ends, so this only bounds one a call left behind: a
 * backgrounded call's, or a killed session's.
 */
export const MARKER_MAX_AGE_MS = 2 * 60 * 60_000;
/**
 * How often a live process touches its store (`keepStore`). Well inside MARKER_MAX_AGE_MS, the age
 * past which a process in another pid namespace counts the store's owner gone, so a process that
 * runs is never taken for one that ended.
 */
export const STORE_BEAT_MS = 10 * 60_000;

const PRIVATE_DIR = 0o700;
const PRIVATE_FILE = 0o600;
const MARKERS = "markers";

export type AskgateMode = "warn" | "block";

/** One process that keeps a session's record live. */
export interface AskgateOwner {
  readonly pid: number;
  /** Field 22 of `/proc/<pid>/stat`; null where there is no `/proc`. */
  readonly start: string | null;
  /**
   * The inode of the process's pid namespace (`/proc/self/ns/pid`); null where there is none, and
   * in a record written before the namespace was kept, whose pid is read in the reader's own.
   */
  readonly ns: string | null;
  readonly store: string;
}

/** The Dispatch tools that always post text AskGate reviews. */
const ALWAYS_GATED: Record<string, true> = {
  dispatch_ask: true,
  dispatch_message: true,
  dispatch_edit_ask: true,
  dispatch_comment: true,
  dispatch_doc_edit: true,
  dispatch_request_approval: true,
};
const MARKDOWN_NAME = /\.md$/i;
const MARKDOWN_PATH = /\.(md|markdown)$/i;

/**
 * Whether an upload's arguments make a markdown document: inline text, or a name (as Dispatch
 * trims it) ending in `.md`, or a path ending in `.md` or `.markdown`, whose type Bun takes from it.
 */
export function uploadIsDocument(args: Readonly<Record<string, unknown>>): boolean {
  const { content, name, path } = args;
  return (
    typeof content === "string" ||
    (typeof name === "string" && MARKDOWN_NAME.test(name.trim())) ||
    (typeof path === "string" && MARKDOWN_PATH.test(path))
  );
}

/**
 * The one predicate of a gated write, the gate's and the CLI's: `ask`, `message`, `edit-ask`,
 * `comment`, `doc-edit` and `request-approval`, an `issue` with a spec, and an `artifact` that
 * uploads a markdown document.
 */
export function gatedWrite(tool: string, args: Readonly<Record<string, unknown>>): boolean {
  // `=== true`, so a name on Object's prototype (`constructor`) is no gated tool.
  if (ALWAYS_GATED[tool] === true) return true;
  const { spec } = args;
  if (tool === "dispatch_issue") return spec !== undefined;
  if (tool === "dispatch_artifact") return uploadIsDocument(args);
  return false;
}

/**
 * Whether this call may carry a verdict of AskGate's, which is a wider question than whether a
 * review would be asked for it (`gatedWrite`).
 *
 * The two differ for an upload, and the difference was a hole. A write's identity covers the
 * bytes and not the path, deliberately, so the same bytes under another name meet the same no —
 * but `uploadIsDocument` reads the path's suffix, so `plan.txt` was a copy of a refused
 * `plan.md` that the CLI never looked up at all. Same issue, same name, same bytes, the same
 * identity, and one extra shell word posted the refused text verbatim. A predicate may govern
 * what a missing marker means; it may never govern whether the verdict is read.
 */
export function mayCarryVerdict(tool: string, args: Readonly<Record<string, unknown>>): boolean {
  return gatedWrite(tool, args) || tool === "dispatch_artifact";
}

const digest = (value: unknown): string =>
  createHash("sha256").update(JSON.stringify(value)).digest("hex");

/** The key of a marker: the digest of the words after the program, as the CLI receives them. */
export function argvDigest(argv: readonly string[]): string {
  return digest(argv);
}

/**
 * The one canonical encoding of a value, which every digest over arguments is taken on: object
 * keys sorted at every depth, arrays in their order, each string's trailing whitespace dropped,
 * and every value tagged by type so no two shapes can encode alike.
 *
 * Each rule is a spelling a reviewer read as the same write. The CLI's parser inserts arguments
 * in the order their flags appeared, so `--body X --issue Y` and `--issue Y --body X` differ
 * without the sort. A here-document body carries the newline the shell adds and the same text in
 * `--body` does not. Arrays keep their order, because `--ops-json` and the other repeatable
 * fields are ordered data and not sets. And the tags are what keep `{}` from meeting `[]`, and
 * `{extra: {x: 1}}` from meeting `{extra: [["x", 1]]}`: without them an allow of one shape
 * cleared the verdict on the other.
 */
export function canonicalText(value: unknown): string {
  if (typeof value === "string") return `s${JSON.stringify(value.replace(/\s+$/, ""))}`;
  if (typeof value === "number") return `n${JSON.stringify(value)}`;
  if (typeof value === "boolean") return `b${value}`;
  if (value === null) return "z";
  if (value === undefined) return "u";
  if (Array.isArray(value)) return `a[${value.map(canonicalText).join(",")}]`;
  if (typeof value !== "object") return `x${JSON.stringify(String(value))}`;
  const entries = Object.entries(value as Record<string, unknown>).sort(([left], [right]) =>
    left < right ? -1 : left > right ? 1 : 0
  );
  return `o{${entries.map(([key, member]) => `${JSON.stringify(key)}:${canonicalText(member)}`).join(",")}}`;
}

/**
 * The digest a reviewed marker binds: of the arguments the call's dry run printed, on the one
 * canonical encoding. The marker is keyed by the argv as well, which disambiguated it before
 * this was canonical, so the sort fixes an order-sensitivity nobody had yet met here: two
 * spellings of one command would have bound different digests had the argv ever matched across
 * them.
 */
export function argsDigest(args: Readonly<Record<string, unknown>>): string {
  return digest(canonicalText(args));
}

/** One file a `dispatch` call makes the CLI read, as its argv names it. */
export interface FileOperand {
  /** The flag that names it: a `--<field>-file`, or an upload's `--path`. */
  readonly flag: string;
  /** The operand as written. */
  readonly operand: string;
}

/** One file of a reviewed call: its operand, and the sha256 of its bytes; null when it was not read. */
export interface FileDigest extends FileOperand {
  readonly sha256: string | null;
}

/**
 * The files a `dispatch` argv (the words after the program) makes the CLI read, in argv order: the
 * operand of each `--<field>-file` but `-` (standard input), and an `artifact` upload's `--path`.
 * A flag's operand is the word after it, or the text after its `=`. The gate and the CLI both use
 * this list, so the digests a marker binds and the ones the CLI computes name the same files.
 */
export function fileOperands(argv: readonly string[]): FileOperand[] {
  const [command, ...flags] = argv;
  const files: FileOperand[] = [];
  for (let index = 0; index < flags.length; index++) {
    const word = flags[index] as string;
    const eq = word.indexOf("=");
    const flag = eq === -1 ? word : word.slice(0, eq);
    const named = flag.startsWith("--") && flag.endsWith("-file");
    if (!named && !(command === "artifact" && flag === "--path")) continue;
    const operand = eq === -1 ? flags[index + 1] : word.slice(eq + 1);
    if (operand !== undefined && operand !== "-") files.push({ flag, operand });
  }
  return files;
}

/**
 * The path the CLI opens for a file operand, from its working directory `cwd`. A `--<field>-file`
 * goes to `readFileSync` as written, so the kernel resolves it and a `..` after a symbolic link
 * leaves the link's target; an upload's `--path` goes through `resolve` (dispatch-execute.ts),
 * which removes a `..` first.
 */
export function operandPath(cwd: string, file: FileOperand): string {
  if (file.flag === "--path") return resolve(cwd, file.operand);
  return isAbsolute(file.operand) ? file.operand : `${cwd}/${file.operand}`;
}

/** The sha256 of a file's bytes, as a marker binds it. */
export function bytesDigest(bytes: Uint8Array): string {
  return createHash("sha256").update(bytes).digest("hex");
}

/** Field 22 of `/proc/<pid>/stat`, the process's start in clock ticks since boot; null without one. */
export function processStart(pid: number): string | null {
  try {
    const stat = readFileSync(`/proc/${pid}/stat`, "utf-8");
    // The name in parentheses may hold spaces and parentheses; the fields after it do not.
    return stat.slice(stat.lastIndexOf(")") + 2).split(" ")[19] ?? null;
  } catch {
    return null;
  }
}

let ownNamespace: string | null | undefined;

/**
 * The inode of this process's pid namespace, from `/proc/self/ns/pid` (`pid:[4026531836]`); null
 * where there is none. A pid and a start time read from `/proc` name a process only in this
 * namespace: an agent box is a namespace of its own over the host's state directory.
 */
export function pidNamespace(): string | null {
  if (ownNamespace === undefined) {
    try {
      ownNamespace = /^pid:\[([0-9]+)\]$/.exec(readlinkSync("/proc/self/ns/pid"))?.[1] ?? null;
    } catch {
      ownNamespace = null;
    }
  }
  return ownNamespace;
}

/** The parts of an owner its name is made of. */
type OwnerIdentity = Pick<AskgateOwner, "pid" | "start" | "ns">;

/**
 * `<pid>-<start>-<ns>`, `<pid>-<start>` with no namespace, or `<pid>` with no start time: an
 * owner's name, for its store and its file. The namespace keeps apart two boxes' processes that
 * have the same pid and started in the same clock tick.
 */
function ownerName({ pid, start, ns }: OwnerIdentity): string {
  if (start === null) return String(pid);
  return ns === null ? `${pid}-${start}` : `${pid}-${start}-${ns}`;
}

/** `<root>/askgate/<name>`: a store of its own for each process, never reused by a pid's next owner. */
export function askgateStore(
  root: string,
  pid: number,
  start: string | null,
  ns: string | null = null
): string {
  return join(root, "askgate", ownerName({ pid, start, ns }));
}

/** This process as an owner, its store under the CLI state root `root`. */
export function selfOwner(root: string): AskgateOwner {
  const start = processStart(process.pid);
  const ns = pidNamespace();
  return { pid: process.pid, start, ns, store: askgateStore(root, process.pid, start, ns) };
}

/**
 * Whether `owner`'s process runs, as this process can tell at `now`. An owner of this pid
 * namespace, or of none recorded, counts while its pid is live with its start time. One of another
 * namespace (an agent box's process seen from the host or from another box, which share the state
 * directory) cannot be looked up here: its pid may be free or another process's in this namespace.
 * It counts while its store changed within MARKER_MAX_AGE_MS, which its process keeps true by
 * touching the store every STORE_BEAT_MS while it runs.
 */
function ownerLive(owner: AskgateOwner, now: number): boolean {
  if (owner.ns !== null && owner.ns !== pidNamespace()) {
    try {
      return now - storeTouched(owner.store) <= MARKER_MAX_AGE_MS;
    } catch (error) {
      // Only ENOENT is the store being gone — its process removed it at exit, or it ended long
      // ago. Anything else (EACCES, ENOTDIR, EIO, ELOOP) is a store that cannot be read, which
      // says nothing about whether its owner lives, and swallowing it dropped the only live
      // owner: `askgateState` then returned `off` and the CLI skipped the whole check,
      // `readVerdict` included, so a recorded refusal posted with no warning and no ledger row.
      // Keeping the owner live sends the same fault to `askgateState`'s own store check, which
      // answers `broken` — which is what that fault already gets for an owner of this
      // namespace. The verdict differed only by which namespace the owner was in, and
      // cross-namespace is the agent-box case.
      return !hasErrnoCode(error, "ENOENT");
    }
  }
  if (owner.start !== null) return processStart(owner.pid) === owner.start;
  try {
    process.kill(owner.pid, 0);
    return true;
  } catch (error) {
    // EPERM: a process of another user holds the pid, which is still a live process.
    return !hasErrnoCode(error, "ESRCH");
  }
}

/** An owner file's content, as `isOwnerText` admits it. */
interface OwnerText {
  readonly v?: unknown;
  readonly pid: number;
  readonly start: string | null;
  /** Absent in a file written before the namespace was kept. */
  readonly ns?: string | null;
  readonly store: string;
  readonly mode?: unknown;
  /** Absent in a file written by a gate from before capabilities (`OwnerCaps`). */
  readonly caps?: unknown;
}

/**
 * What a gate says it does, written into its own owner record so the CLI reads a fact instead of
 * inferring one. Without this, nothing on disk told "this gate wrote no verdict" from "this gate
 * cannot write verdicts", and an empty store was read as proof that no review had refused —
 * which is the substitution this whole change exists to remove, rebuilt one layer up.
 *
 * `verdicts` names the store a gate records its refusals in, and is held only when it is this
 * version's own: anything else, absent or unreadable, is **not held**, and absence of a verdict
 * then proves nothing. `markers` names the encoding its markers bind their arguments under.
 *
 * An unrecognised capability is treated as not held, always. That is what lets a gate of any
 * version write this record safely: a reader that cannot interpret a claim refuses rather than
 * posts, so no gate — older or newer — can talk a CLI into letting a write through.
 */
interface OwnerCaps {
  readonly verdicts?: string;
  readonly markers?: string;
}

/** The marker encoding a gate from before capabilities used: `digest(args)`, insertion order. */
const MARKERS_RAW = "raw";
/** The marker encoding of this version: `digest(canonicalText(args))`. */
const MARKERS_CANONICAL = "canonical";

/**
 * Whether `value` is an owner file's content: its pid, start time and store, and its pid namespace,
 * which a file written before the namespace was kept does not carry.
 */
function isOwnerText(value: unknown): value is OwnerText {
  if (typeof value !== "object" || value === null) return false;
  const { pid, start, ns, store } = value as Record<string, unknown>;
  return (
    typeof pid === "number" &&
    Number.isSafeInteger(pid) &&
    pid > 0 &&
    (start === null || typeof start === "string") &&
    (ns === undefined || ns === null || (typeof ns === "string" && /^[0-9]+$/.test(ns))) &&
    typeof store === "string" &&
    store.startsWith("/")
  );
}

/**
 * The owner and the check mode an owner file's text names, or undefined when it is no owner file.
 * The mode is per session per owner: a session this process bound in block keeps block whatever a
 * later session of the same process records for itself. A missing or unknown mode is warn.
 */
function parseOwner(
  text: string
): { owner: AskgateOwner; mode: AskgateMode; verdicts: boolean; markers: string[] } | undefined {
  let data: unknown;
  try {
    data = JSON.parse(text);
  } catch {
    return undefined;
  }
  if (!isOwnerText(data) || data.v !== 2) return undefined;
  const { pid, start, ns, store, mode, caps } = data;
  // Absent is a fact about a version that predates the field, so it infers that version's
  // behaviour. Present and unrecognised is a claim this reader cannot evaluate, so it takes the
  // stricter reading: a future gate writing a third mode would otherwise drop every "cannot
  // tell" of this session to posting.
  const checks: AskgateMode = mode === undefined || mode === "warn" ? "warn" : "block";
  // A record with no capabilities is not ambiguous: it is a gate from before them, which is a
  // fact and not a guess. It records no verdicts, and its markers bind the one encoding that
  // version used. One historical encoder, named by a declared fact rather than tried among
  // several — and deletable once no such gate is left, with the record itself saying when.
  return {
    owner: { pid, start, ns: ns ?? null, store },
    mode: checks,
    ...held(caps, "caps" in data),
  };
}

/**
 * What a record's capabilities entitle its gate to, written so that holding nothing is the
 * answer and every other one needs a reason.
 *
 * Three states, and only the first two are facts. A field that is **absent entirely** is a gate
 * from before capabilities: that is a fact about a version, so it infers the one encoding that
 * version used. A field that **is exactly this version's** is a fact it declared. Anything else
 * — an unrecognised value, an unknown or missing key, a duplicate that `JSON.parse` collapsed,
 * a shape that is not an object — is a claim this reader cannot evaluate, and it holds nothing:
 * no verdict capability, and no encoding, so no digest matches and every marker reads as stale.
 *
 * Written the other way round, `markers` once read an unrecognised value as the historical
 * encoding, so `{markers:"future"}` was honoured as raw and an old marker let a refused write
 * post. The rule is that a claim one cannot evaluate yields nothing, never a default.
 */
function held(caps: unknown, present: boolean): { verdicts: boolean; markers: string[] } {
  if (!present) return { verdicts: false, markers: [MARKERS_RAW] };
  // Every key known, every value known, nothing over. A key written twice is not seen here:
  // `JSON.parse` keeps the last, so what survives the collapse is what is read, and a record
  // carrying `{"verdicts":"nope","verdicts":"askgate-verdicts-v2","markers":"canonical"}`
  // holds the capability while the reverse order does not. Nothing in this tree writes a
  // duplicate key, and a same-user process that edits the record is already outside what the
  // marker store defends against, so this is left rather than guarded — but it is a shape
  // order decides, which is why it is written down rather than claimed closed.
  if (typeof caps !== "object" || caps === null || Array.isArray(caps))
    return { verdicts: false, markers: [] };
  const declared = caps as Record<string, unknown>;
  if (
    Object.keys(declared).length !== 2 ||
    declared.verdicts !== VERDICTS ||
    declared.markers !== MARKERS_CANONICAL
  )
    return { verdicts: false, markers: [] };
  return { verdicts: true, markers: [MARKERS_CANONICAL] };
}

/** Writes `text` to `file` through an exclusive owner-only temporary file and `rename`. */
function writeAtomically(file: string, text: string): void {
  const temporary = `${file}.${randomBytes(8).toString("hex")}.tmp`;
  writeFileSync(temporary, text, { flag: "wx", mode: PRIVATE_FILE });
  renameSync(temporary, file);
}

const OWNER_FILE = ".json";

/** `owner`'s own file in session directory `dir`'s record. */
function ownerFile(dir: string, owner: AskgateOwner): string {
  return join(dir, ASKGATE_RECORD, `${ownerName(owner)}${OWNER_FILE}`);
}

/**
 * Makes `owner` an owner of session directory `dir`'s record, carrying `mode` for the CLI's marker
 * check of that session, and removes the files of owners known to be gone. It makes the owner's
 * store first (`keepStore`), so the file never names a store that is not there. Only an owner of
 * this pid namespace is known gone: no later process of the namespace has its pid and start time.
 * The file of an owner of another namespace, or of one written before the namespace was kept, stays
 * whatever its pid reads here, since its process may still run and would lose its record until its
 * next call; it is inert wherever its owner counts gone, and goes with the session directory's
 * prune. One with no start time is left too, as its pid can return.
 */
export function writeAskgateRecord(
  dir: string,
  owner: AskgateOwner,
  mode: AskgateMode,
  now = Date.now()
): void {
  keepStore(owner.store, now);
  const record = join(dir, ASKGATE_RECORD);
  mkdirSync(record, { recursive: true, mode: PRIVATE_DIR });
  const own = ownerFile(dir, owner);
  writeAtomically(own, ownerText(owner, mode));
  for (const name of readdirSync(record)) {
    const file = join(record, name);
    if (file === own || !name.endsWith(OWNER_FILE)) continue;
    let other: { owner: AskgateOwner } | undefined;
    try {
      other = parseOwner(readFileSync(file, "utf-8"));
    } catch {
      continue;
    }
    const gone =
      other !== undefined &&
      other.owner.start !== null &&
      other.owner.ns !== null &&
      other.owner.ns === pidNamespace() &&
      !ownerLive(other.owner, now);
    if (gone) {
      rmSync(file, { force: true });
    }
  }
}

/**
 * Rewrites `owner`'s own file in session directory `dir`'s record with `mode`, for the gate to set
 * the current session's check mode on each main-agent call. It touches no other owner's file, so a
 * session this process also bound keeps the mode its own calls recorded. Like the bind, it makes
 * the owner's store and the record's directory when another process removed them (a prune that took
 * this process for a gone one), so the record is whole again before the call it runs for.
 */
export function setRecordMode(
  dir: string,
  owner: AskgateOwner,
  mode: AskgateMode,
  now = Date.now()
): void {
  keepStore(owner.store, now);
  mkdirSync(join(dir, ASKGATE_RECORD), { recursive: true, mode: PRIVATE_DIR });
  writeAtomically(ownerFile(dir, owner), ownerText(owner, mode));
}

/**
 * The check mode `owner`'s own file in session directory `dir`'s record carries, or undefined when
 * that file is absent or is no owner file. A rebind of the same session reads it so that a block
 * the session recorded on a main-agent call is never turned back to warn before the gate's next
 * call sets the mode again.
 */
export function recordedOwnerMode(dir: string, owner: AskgateOwner): AskgateMode | undefined {
  try {
    return parseOwner(readFileSync(ownerFile(dir, owner), "utf-8"))?.mode;
  } catch {
    return undefined;
  }
}

function ownerText(owner: AskgateOwner, mode: AskgateMode): string {
  const { pid, start, ns, store } = owner;
  const caps: OwnerCaps = { verdicts: VERDICTS, markers: MARKERS_CANONICAL };
  return JSON.stringify({ v: 2, pid, start, ns, store, mode, caps });
}

/**
 * Takes `owner` off session directory `dir`'s record by removing its own file. The directory
 * stays: an empty one names no owner, and removing it could fail another process's write there.
 */
export function dropAskgateRecord(dir: string, owner: AskgateOwner): void {
  rmSync(ownerFile(dir, owner), { force: true });
}

/** Makes a store and its markers directory, owner-only. */
export function openStore(store: string): void {
  mkdirSync(join(store, MARKERS), { recursive: true, mode: PRIVATE_DIR });
}

/**
 * Makes `store` when it is gone and marks it changed at `now`. A live process calls it every
 * STORE_BEAT_MS, so a process of another pid namespace counts it live and its prune keeps the
 * store, and before each write of its own owner file. A store made again holds no marker, so
 * nothing it lacks is taken for reviewed.
 */
export function keepStore(store: string, now: number): void {
  openStore(store);
  const at = new Date(now);
  utimesSync(store, at, at);
}

/** Removes a store and every marker in it. */
export function closeStore(store: string): void {
  rmSync(store, { recursive: true, force: true });
}

/** A store's name under `<root>/askgate/`, as `askgateStore` makes it (`ownerName`). */
const STORE_NAME = /^([1-9][0-9]*)(?:-([0-9]+)(?:-([0-9]+))?)?$/;

/** The last change to a store: its own or its markers directory's, whichever is later. */
function storeTouched(store: string): number {
  const own = statSync(store).mtimeMs;
  try {
    return Math.max(own, statSync(join(store, MARKERS)).mtimeMs);
  } catch (error) {
    if (hasErrnoCode(error, "ENOENT")) return own;
    throw error;
  }
}

/**
 * How long the sweep keeps a store named before the pid namespace was kept (`<pid>-<start>`) whose
 * process it cannot find by pid. That process may be gone, or may be a live one of another
 * namespace that an agent box started on an earlier pi-envoy, which names no namespace and touches
 * its store only when it mints. The sweep cannot tell the two apart, so it keeps such a store as
 * long as the daily prune keeps an idle session directory, and the earlier process keeps its store
 * until it restarts on a pi-envoy that names its namespace.
 */
const UNNAMESPACED_STORE_MAX_AGE_MS = 14 * 86_400_000;

/**
 * Removes each store under `<root>/askgate/` whose process is gone (a crashed process's, whose
 * markers no CLI reads) and that nothing changed for MARKER_MAX_AGE_MS. A store of another pid
 * namespace's process counts gone only by that age (`ownerLive`), which its process keeps from
 * passing while it runs. A store named before the namespace was kept has only its pid and start
 * time, read in this namespace, and is kept for UNNAMESPACED_STORE_MAX_AGE_MS. A store it cannot
 * remove stays: this is tidiness, so it never stops a call.
 */
export function sweepStores(root: string, now: number): void {
  const parent = join(root, "askgate");
  let names: string[];
  try {
    names = readdirSync(parent);
  } catch {
    return;
  }
  for (const name of names) {
    const match = STORE_NAME.exec(name);
    if (match === null) continue;
    const store = join(parent, name);
    try {
      const owner = { pid: Number(match[1]), start: match[2] ?? null, ns: match[3] ?? null, store };
      if (ownerLive(owner, now)) continue;
      const unnamespaced = owner.start !== null && owner.ns === null;
      const keep = unnamespaced ? UNNAMESPACED_STORE_MAX_AGE_MS : MARKER_MAX_AGE_MS;
      if (now - storeTouched(store) <= keep) continue;
      rmSync(store, { recursive: true, force: true });
    } catch {
      // Gone since the listing, or not this user's to remove.
    }
  }
}

/**
 * Whether an owner file of session directory `dir`'s record names a marker store that is still
 * there. The daily prune keeps such a directory whatever `askgateState` reads of its owners: a
 * store goes when its process exits or once the store sweep counts it gone, and the sweep keeps the
 * store of an owner it cannot look up (one an earlier pi-envoy wrote with no pid namespace, which
 * may be another namespace's live process) longer than any pid lookup here could. A record that
 * cannot be read counts as naming one.
 */
export function recordNamesStore(dir: string): boolean {
  const record = join(dir, ASKGATE_RECORD);
  let names: string[];
  try {
    names = readdirSync(record);
  } catch (error) {
    return !hasErrnoCode(error, "ENOENT");
  }
  for (const name of names) {
    if (!name.endsWith(OWNER_FILE)) continue;
    try {
      const parsed = parseOwner(readFileSync(join(record, name), "utf-8"));
      if (parsed === undefined) return true;
      statSync(parsed.owner.store);
      return true;
    } catch (error) {
      // The owner file went after the listing, or the store it names is gone.
      if (!hasErrnoCode(error, "ENOENT")) return true;
    }
  }
  return false;
}

/** What the record of a session's state directory requires of a gated write. */
export type AskgateState =
  | { readonly kind: "off" }
  | {
      readonly kind: "on";
      readonly stores: readonly string[];
      readonly mode: AskgateMode;
      /**
       * Whether every live owner records its refusals where this version reads them. One owner
       * that does not is enough to make absence meaningless for the whole session: that gate
       * can have refused a write and left nothing behind.
       */
      readonly verdicts: boolean;
      /** The marker encodings the live owners declare; a marker may come from any of them. */
      readonly markers: readonly string[];
    }
  | { readonly kind: "broken"; readonly problem: string };

/**
 * The state of session directory `dir`'s record at `now`: off when there is none or no owner
 * lives, on with each live owner's store (block when any one's session chose block, else warn),
 * broken when the record or an owner file cannot be read or a live owner's store is gone.
 */
export function askgateState(dir: string, now = Date.now()): AskgateState {
  const record = join(dir, ASKGATE_RECORD);
  let names: string[];
  try {
    names = readdirSync(record);
  } catch (error) {
    if (hasErrnoCode(error, "ENOENT")) return { kind: "off" };
    return { kind: "broken", problem: `${record}, could not be read: ${messageFor(error)}` };
  }
  const live: {
    readonly file: string;
    readonly store: string;
    readonly mode: AskgateMode;
    readonly verdicts: boolean;
    readonly markers: readonly string[];
  }[] = [];
  for (const name of names) {
    // Not an owner file: a temporary file that its write's `rename` has yet to make one.
    if (!name.endsWith(OWNER_FILE)) continue;
    const file = join(record, name);
    let text: string;
    try {
      text = readFileSync(file, "utf-8");
    } catch (error) {
      // Its owner removed it after the listing.
      if (hasErrnoCode(error, "ENOENT")) continue;
      return { kind: "broken", problem: `${file}, could not be read: ${messageFor(error)}` };
    }
    const parsed = parseOwner(text);
    if (parsed === undefined)
      return { kind: "broken", problem: `${file}, is not an AskGate record` };
    if (ownerLive(parsed.owner, now)) {
      live.push({
        file,
        store: parsed.owner.store,
        mode: parsed.mode,
        verdicts: parsed.verdicts,
        markers: parsed.markers,
      });
    }
  }
  if (live.length === 0) return { kind: "off" };
  for (const { file, store } of live) {
    try {
      if (!statSync(join(store, MARKERS)).isDirectory()) throw new Error("not a directory");
    } catch (error) {
      return {
        kind: "broken",
        problem: `${file}, names the marker store ${store}, which cannot be read: ${messageFor(error)}`,
      };
    }
  }
  return {
    kind: "on",
    stores: live.map(({ store }) => store),
    mode: live.some(({ mode }) => mode === "block") ? "block" : "warn",
    verdicts: live.every(({ verdicts }) => verdicts),
    markers: [...new Set(live.flatMap(({ markers }) => markers))],
  };
}

/**
 * What one marker binds. `args` null: an argv-only marker, minted for a call no review stands
 * behind, which binds no file either.
 */
export interface MarkerInput {
  readonly argv: readonly string[];
  /** The arguments the call's dry run printed. */
  readonly args: Readonly<Record<string, unknown>> | null;
  /** Each file the argv names (`fileOperands`), with the digest of the bytes AskGate reviewed. */
  readonly files: readonly FileDigest[];
  readonly toolCallId: string;
  /** The gate's outcome for the call, or `subagent` for a task subagent's call. */
  readonly outcome: string;
}

interface Marker {
  readonly argv: string;
  readonly args: string | null;
  readonly files: readonly FileDigest[];
  readonly createdAt: number;
}

const isFileDigest = (value: unknown): value is FileDigest =>
  typeof value === "object" &&
  value !== null &&
  typeof (value as FileDigest).flag === "string" &&
  typeof (value as FileDigest).operand === "string" &&
  typeof (value as FileDigest).sha256 === "string";

/** Removes the markers and temporary files older than MARKER_MAX_AGE_MS from a markers directory. */
function sweepMarkers(dir: string, now: number): void {
  for (const name of readdirSync(dir)) {
    const file = join(dir, name);
    try {
      if (now - statSync(file).mtimeMs > MARKER_MAX_AGE_MS) unlinkSync(file);
    } catch {
      // Consumed or swept by another process since the listing.
    }
  }
}

/** Mints one marker in `store` and returns its path. Throws when the store cannot take it. */
export function mintMarker(store: string, input: MarkerInput, now: number): string {
  openStore(store);
  const dir = join(store, MARKERS);
  sweepMarkers(dir, now);
  const key = argvDigest(input.argv);
  const nonce = randomBytes(16).toString("hex");
  const temporary = join(dir, `.${nonce}.tmp`);
  writeFileSync(
    temporary,
    JSON.stringify({
      v: 2,
      argv: key,
      args: input.args === null ? null : argsDigest(input.args),
      files: input.args === null ? [] : input.files,
      toolCallId: input.toolCallId,
      outcome: input.outcome,
      createdAt: now,
    }),
    { flag: "wx", mode: PRIVATE_FILE }
  );
  // Whole or absent under its real name: a CLI never reads a marker half written.
  const file = join(dir, `${key}.${nonce}`);
  renameSync(temporary, file);
  return file;
}

/**
 * A marker's binding, read without following a link. `"gone"` where the file is no longer there,
 * which a concurrent sweep or another process's consume does in the window between the listing
 * and this open: that is a marker that is absent, not one that cannot be read. `"unreadable"` for
 * anything else — a link in its place (`O_NOFOLLOW`), a mode this process cannot open, a body
 * that is not a marker. A mint writes its file under a temporary name and renames it, so no
 * marker is ever half written under its real name: `"unreadable"` is a file that exists, says it
 * is a marker of this call, and will not say what it holds, which the CLI must not read as "no
 * review ran" (dispatch://INFRA-2079, row 3.5).
 */
function readMarker(file: string): Marker | "gone" | "unreadable" {
  let text: string;
  try {
    const fd = openSync(file, constants.O_RDONLY | constants.O_NOFOLLOW);
    try {
      text = readFileSync(fd, "utf-8");
    } finally {
      closeSync(fd);
    }
  } catch (error) {
    return hasErrnoCode(error, "ENOENT") ? "gone" : "unreadable";
  }
  try {
    const data: unknown = JSON.parse(text);
    if (typeof data !== "object" || data === null) return "unreadable";
    const { v, argv, args, files, createdAt } = data as Record<string, unknown>;
    if (v !== 2 || typeof argv !== "string" || typeof createdAt !== "number") return "unreadable";
    if (args !== null && typeof args !== "string") return "unreadable";
    if (!Array.isArray(files) || !files.every(isFileDigest)) return "unreadable";
    return { argv, args, files, createdAt };
  } catch {
    return "unreadable";
  }
}

/**
 * What the stores say about AskGate's review of this call. One of four, and the CLI's whole
 * decision rests on telling them apart (dispatch://INFRA-2079, row 3.5): a refusal requires a
 * verdict, and a write posts unreviewed only where no verdict can positively be shown to exist.
 *
 * - `stale`: a marker AskGate minted for this exact argv was found and rejected, so a review ran
 *   and this write is not what its verdict covers — other arguments, a file whose bytes changed
 *   since the review read them, a marker past its age. `changed` names the first such file.
 * - `unknown`: evidence exists for this call and cannot be read. Absence of a marker is not a
 *   statement here, since the thing that cannot be read may be a refusal.
 * - `none`: the stores were read whole and hold no marker for this argv. Only this is a positive
 *   showing that no marker was minted; a stored verdict is looked for separately (`readVerdict`),
 *   since AskGate mints nothing when it rejects.
 */
export type Consumed =
  /**
   * One marker consumed. `bound` is whether its review read these very arguments and files: an
   * argv-only marker says in as many words that no review stands behind it — a halt, a skip, a
   * subagent's pass — so it is permission without a verdict, and only a bound one may outrank
   * the CLI's uncertainty about a refusal it cannot read.
   */
  | { readonly ok: true; readonly bound: boolean }
  | {
      readonly ok: false;
      readonly verdict: "stale";
      readonly why: string;
      readonly changed?: FileOperand;
    }
  | { readonly ok: false; readonly verdict: "unknown"; readonly why: string; readonly path: string }
  | { readonly ok: false; readonly verdict: "none"; readonly why: string };

/**
 * Consumes one marker for `argv` from `stores`: a marker bound to `args` and to `files` (each file
 * the argv names, as the CLI read it) first, else an argv-only one. One bound to other files or
 * other arguments, and one past MARKER_MAX_AGE_MS, is left where it is: a refusal consumes
 * nothing, so the same call sent again meets the same answer rather than an emptier store. The
 * sweep at the gate's next mint takes an expired marker, and the store goes with its session. A
 * lost race for a marker (another process unlinked it first) goes on to the next.
 */
export function consumeMarker(
  stores: readonly string[],
  argv: readonly string[],
  args: Readonly<Record<string, unknown>>,
  files: readonly FileDigest[],
  now: number,
  /** The marker encodings the session's live owners declare (`askgateState`). */
  encodings: readonly string[] = [MARKERS_CANONICAL],
  /**
   * Whether an argv-only marker may be spent. False where the caller will refuse the write
   * whatever it finds, so that a refusal consumes nothing and the same call meets the same
   * answer rather than an emptier store.
   */
  acceptUnbound = true
): Consumed {
  const key = argvDigest(argv);
  // One digest per encoding a live owner says it writes. Not a search over every encoding that
  // ever existed: the record names which apply, so a gate from before `canonicalText` has its
  // markers honoured rather than refused with the untrue reason that the arguments differ.
  const reviewed = new Set(
    encodings.map((encoding) => (encoding === MARKERS_RAW ? digest(args) : argsDigest(args)))
  );
  let changed: FileOperand | undefined;
  let mismatched = false;
  let expired = false;
  /** The first file of this call's own key that exists and will not say what it holds. */
  let unreadable: string | undefined;
  const usable: Array<{ file: string; bound: boolean }> = [];
  for (const store of stores) {
    const dir = join(store, MARKERS);
    for (const name of readdirSync(dir)) {
      if (!name.startsWith(`${key}.`)) continue;
      const file = join(dir, name);
      const marker = readMarker(file);
      // Swept or consumed since the listing: absent, which is this call's answer and no fault.
      if (marker === "gone") continue;
      // A file of this call's key that cannot be read, or that is no marker of this argv at all:
      // what it would have said cannot be told, and one thing it can say is no.
      if (marker === "unreadable" || marker.argv !== key) {
        unreadable ??= file;
        continue;
      }
      if (now - marker.createdAt > MARKER_MAX_AGE_MS) {
        expired = true;
        continue;
      }
      if (marker.args !== null) {
        // The argv keys the marker, so both lists name the same files; a file whose bytes differ
        // says more than the arguments it changed with it.
        const differs = marker.files.find(
          (bound, index) =>
            bound.flag !== files[index]?.flag ||
            bound.operand !== files[index]?.operand ||
            bound.sha256 !== files[index]?.sha256
        );
        if (differs !== undefined) {
          changed ??= { flag: differs.flag, operand: differs.operand };
          continue;
        }
        // A marker whose digest matches none the record declares reads as `stale`, which fails
        // closed. An earlier encoding is not that case: the owner says it writes one, so it is
        // honoured. Round seven refused it instead, telling the agent its arguments differed
        // from the reviewed ones when only the encoding had moved, and since an old gate mints
        // a fresh old-encoding marker on every write, the sweep never caught up — in block mode
        // that refused every reviewed literal write for the session's life.
        if (marker.files.length !== files.length || !reviewed.has(marker.args)) {
          mismatched = true;
          continue;
        }
      }
      usable.push({ file, bound: marker.args !== null });
    }
  }
  usable.sort((a, b) => Number(b.bound) - Number(a.bound));
  for (const { file, bound } of usable) {
    // An argv-only marker says no review stands behind it, so it may not answer an uncertainty
    // — neither the caller's, from the verdict store (`acceptUnbound`), nor this scan's own,
    // from a marker of this very call that exists and will not be read, or that a verdict does
    // not cover. The second was found here and reported only after this loop had returned, so
    // a subagent's marker, or one minted for a timeout, posted past a damaged marker of the
    // same call and past a stale or expired one — silently, since nothing refused.
    if (
      !bound &&
      (!acceptUnbound || unreadable !== undefined || changed !== undefined || mismatched || expired)
    )
      continue;
    try {
      unlinkSync(file);
      return { ok: true, bound };
    } catch (error) {
      if (!hasErrnoCode(error, "ENOENT")) throw error;
    }
  }
  if (changed !== undefined)
    return {
      ok: false,
      verdict: "stale",
      why: `a file this write posts changed after AskGate read it (\`${changed.flag} ${changed.operand}\`)`,
      changed,
    };
  if (mismatched)
    return {
      ok: false,
      verdict: "stale",
      why: "its arguments differ from the ones AskGate reviewed",
    };
  if (expired)
    return { ok: false, verdict: "stale", why: "its AskGate marker expired before it ran" };
  // A readable marker is gone or says no to this call; an unreadable one is read before absence
  // is reported, since a file that will not speak is no proof that nothing was said.
  if (unreadable !== undefined)
    return {
      ok: false,
      verdict: "unknown",
      why: `AskGate's marker for this call cannot be read (${unreadable})`,
      path: unreadable,
    };
  return { ok: false, verdict: "none", why: "this call carries no AskGate marker" };
}

/**
 * Where a session keeps AskGate's verdicts, under its own state directory beside its record.
 *
 * Not the process's marker store. A marker is permission for one call of one process and lives
 * in `markers/`, swept at each mint and spent by the call it was minted for. A verdict is a fact
 * about content, and what bounds it is the session: the gate's halt, breaker and rebuttals all
 * reset on a session switch, and a no the model gave in one session is nothing to another. Kept
 * in the process store, a no leaked to every session that process later served, and died with a
 * gate restart that the session itself survived.
 *
 * Lifetime: born at the review, and gone when a later review of the same identity allows the
 * write (`clearVerdict`), or when the session's directory goes (the CLI's daily prune). Never
 * swept by age — an expired permission must refuse, and an expired no must never become
 * permission — and it outlives a restart of the gate, since the directory is the session's. A
 * session with no live owner has AskGate off altogether and nothing here is read.
 */
const VERDICTS = "askgate-verdicts-v2";
/**
 * The file that marks a session whose gate lost a verdict it could not write
 * (`markVerdictsLost`). It sits in the session's own directory, beside its AskGate record and
 * its ledger, and not inside the verdict store: a detector kept inside the fault domain it
 * reports cannot report that domain failing. Measured, a `chmod 500` of the verdict store alone
 * leaves the session's directory, its ledger and the CLI healthy, so the mark is written and the
 * session degrades to strict; only the session's own directory refusing a new file takes the
 * mark down with the verdict, and that same fault stops the ledger too.
 */
const VERDICTS_LOST = "askgate-verdicts-lost";

/**
 * The identity of a proposed write: what the reviewer judged, and nothing of how the command
 * spelled it. The Dispatch tool and every argument, on the one canonical encoding
 * (`canonicalText`), with an upload's bytes standing in for whatever named them.
 *
 * Scope, exactly. It covers the tool and every argument of the write — the issue, the body or
 * content, the artifact name — so a no about one write never reaches another issue, another
 * tool or other text, and a corrected resend has another identity and is untouched. It covers
 * no spelling: flag order, `--issue=X` against `--issue X`, and text inline against text from a
 * `--<field>-file`, which the parser has already resolved to the same string, all meet the same
 * verdict. For an upload it covers the bytes and not the path, so the rejected bytes sent again
 * through `--content`, `--content-file`, `--path-file` or a file under another name meet the
 * same no, while a file whose text was corrected has a new identity and is free. It names no
 * session, machine or time: the session that owns the verdict store bounds it.
 *
 * `uploadBytes` are the bytes an upload sends, where this write is one: the reviewer read them,
 * and the CLI reads them again (`uploadBytesOf`). Undefined for every other write, and for an
 * upload whose bytes could not be read — which leaves no identity at all, and absence of
 * identity is never absence of a verdict.
 */
export function writeIdentity(
  tool: string,
  args: Readonly<Record<string, unknown>>,
  uploadBytes?: Uint8Array
): string {
  const { path: named, content: inline, ...rest } = args;
  const bytes =
    uploadBytes ?? (typeof inline === "string" ? Buffer.from(inline, "utf8") : undefined);
  const upload = typeof named === "string" || typeof inline === "string";
  const judged = upload && bytes !== undefined ? { ...rest, content: bytesDigest(bytes) } : args;
  return digest(canonicalText({ tool, args: judged }));
}

/**
 * The bytes an upload sends, read from the arguments alone and never from the flags that set
 * them. One place knows this, because every round of review has found another spelling: the
 * parser resolves `--path` and `--path-file` to `args.path`, and `--content` and
 * `--content-file` to `args.content`, so reading the arguments covers each of the four and
 * whatever is added next. Undefined where this is no upload, or where its file cannot be read.
 */
export function uploadBytesOf(
  args: Readonly<Record<string, unknown>>,
  readBytes: (path: string) => Uint8Array | undefined
): Uint8Array | undefined {
  const { path: named, content: inline } = args;
  if (typeof named === "string") return readBytes(named);
  return typeof inline === "string" ? Buffer.from(inline, "utf8") : undefined;
}

/** `<session>/askgate-verdicts-v2`, where this scheme's verdicts of no are kept. */
function verdictDir(session: string): string {
  return join(session, VERDICTS);
}

/** A verdict store of some scheme: this one, or `askgate-verdicts` from the first. */
const VERDICT_STORE = /^askgate-verdicts(-v[0-9]+)?$/;

/**
 * Whether this session holds verdicts of a scheme this version cannot read.
 *
 * It never decodes one. A foreign store is detected and the write is held, which is the whole of
 * the compatibility story: reading an old verdict would oblige every future version to keep every
 * encoding it has ever used, exactly, forever, and six rounds of this branch are the evidence
 * that the obligation cannot be met — a fallback added in one commit missed the scheme the commit
 * before it was writing.
 *
 * Entries beginning with a dot do not count. The probe writes `.probe-<hex>` here and a write
 * leaves `.<hex>.tmp` while it renames, so litter from a failed removal would otherwise make a
 * session that recorded no verdict at all read as holding a foreign one — at the next scheme
 * change, long after anyone remembered why.
 */
function foreignVerdicts(
  session: string
): { kind: "none" } | { kind: "foreign"; path: string } | { kind: "unreadable"; path: string } {
  let names: string[];
  try {
    names = readdirSync(session);
  } catch (error) {
    // Only ENOENT establishes absence. A session whose directory cannot be listed — search
    // permission without read, an EIO, a path component that is no directory — says nothing
    // about what it holds, and reading that silence as "no verdict" posted writes a review had
    // refused. This rule holds at every read on this path, not only here.
    return hasErrnoCode(error, "ENOENT") ? { kind: "none" } : { kind: "unreadable", path: session };
  }
  const mine = basename(verdictDir(session));
  for (const name of names) {
    if (name === mine || !VERDICT_STORE.test(name)) continue;
    const dir = join(session, name);
    try {
      if (readdirSync(dir).some((entry) => !entry.startsWith(".")))
        return { kind: "foreign", path: dir };
    } catch (error) {
      if (!hasErrnoCode(error, "ENOENT")) return { kind: "unreadable", path: dir };
    }
  }
  return { kind: "none" };
}

/**
 * Records that AskGate reviewed this write and did not send it. Written whole under a temporary
 * name and renamed, owner-only, as a marker is, and rewritten freely: one verdict per identity.
 * Throws when the session's directory cannot take it, which the gate reports and marks
 * (`markVerdictsLost`) rather than swallows.
 */
export function writeVerdict(
  session: string,
  identity: string,
  reason: string,
  now: number
): string {
  const dir = verdictDir(session);
  mkdirSync(dir, { recursive: true, mode: PRIVATE_DIR });
  const temporary = join(dir, `.${randomBytes(8).toString("hex")}.tmp`);
  writeFileSync(temporary, JSON.stringify({ v: 1, reason, at: now }), {
    flag: "wx",
    mode: PRIVATE_FILE,
  });
  const file = join(dir, identity);
  renameSync(temporary, file);
  return file;
}

/**
 * Removes this session's verdict of no for `identity`, where there is one: a later review that
 * allows the same write replaces the verdict that refused it, and a no that outlived the verdict
 * replacing it would refuse a write its reviewer has since passed.
 */
export function clearVerdict(session: string, identity: string): void {
  rmSync(join(verdictDir(session), identity), { force: true });
}

/**
 * Marks this session as one whose gate reached a verdict of no and could not write it down. From
 * then on, a store that holds nothing for a write is no proof that nothing was said, so the CLI
 * reads absence as `unknown` rather than as none for the rest of the session.
 *
 * It never clears: the verdict that was lost stays lost, and a later write that succeeds says
 * nothing about the one that did not. The cost is a session that is strict from its first failed
 * record onward — the behaviour every session had before dispatch://INFRA-2079 — and the cost of
 * clearing it would be a rejected write posting. Best effort: where the directory takes nothing
 * at all the mark cannot be written either, and the gate's own refusal of that call is then the
 * only thing holding it.
 */
export function markVerdictsLost(session: string, now: number): void {
  writeFileSync(join(session, VERDICTS_LOST), JSON.stringify({ v: 1, at: now }), {
    mode: PRIVATE_FILE,
  });
}

/** Writes one probe file where `write` puts it and removes it; false on any failure. */
function canWrite(write: (name: string) => string, now: number): boolean {
  const name = `.probe-${randomBytes(8).toString("hex")}`;
  let file: string | undefined;
  try {
    file = write(name);
    writeFileSync(file, JSON.stringify({ v: 1, at: now }), { flag: "wx", mode: PRIVATE_FILE });
    return true;
  } catch {
    return false;
  } finally {
    // The probe leaves nothing behind, and a removal that fails is not itself a reason to stop:
    // a leftover `.probe-` is read as no verdict, since every entry beginning with a dot is
    // passed over. Nothing sweeps this directory — a verdict is never swept — so such a file
    // stays until the session's own directory goes, read by nothing.
    try {
      if (file !== undefined) rmSync(file, { force: true });
    } catch {
      // Nothing more to do: the file is named so that nothing reads it as a verdict.
    }
  }
}

/**
 * Whether this session can hold no verdict at all: neither one where a verdict goes nor the mark
 * of a lost one. Both must fail, which is the two-tier rule the model already states — a fault in
 * the verdict store alone degrades the session to strict, and only the session's own directory
 * refusing a file stops the gate.
 *
 * Asked on every gated write and never remembered. The gate otherwise learns it cannot persist
 * only when it has something to persist, and an outcome no review stands behind never gets that
 * far, so a restarted gate on a still-faulty session would mint an unbound marker and the
 * rejected write would post. Asking each time also means a fault that clears — a disk freed, a
 * permission repaired, a transient failure — is noticed: a conclusion drawn now, not a latch
 * that outlives its cause. A probe file is briefly visible to anything watching the directory;
 * no production watcher reads it today.
 */
export function verdictsUnkeepable(session: string, now: number): boolean {
  if (
    canWrite((name) => {
      mkdirSync(verdictDir(session), { recursive: true, mode: PRIVATE_DIR });
      return join(verdictDir(session), name);
    }, now)
  )
    return false;
  return !canWrite((name) => join(session, name), now);
}

/** What a session's verdict store says about one write. */
export type VerdictRead =
  /** AskGate reviewed this write and did not send it. */
  | { readonly kind: "refused"; readonly reason: string; readonly at: number }
  /** A verdict for it, or the mark of one that was lost, is there and cannot be read. */
  | { readonly kind: "unknown"; readonly why: string; readonly path: string }
  /** The store was read whole and holds no verdict for this write, and none was lost. */
  | { readonly kind: "none" }
  /**
   * No verdict for this write, but this session lost one it could not write down. `path` is the
   * mark and `store` the directory that would not take the verdict — the one an operator fixes.
   */
  | { readonly kind: "lost"; readonly path: string; readonly store: string };

/** Reads one file of the verdict store, without following a link. */
function readVerdictFile(file: string): Record<string, unknown> | "gone" | "unreadable" {
  let text: string;
  try {
    const fd = openSync(file, constants.O_RDONLY | constants.O_NOFOLLOW);
    try {
      text = readFileSync(fd, "utf-8");
    } finally {
      closeSync(fd);
    }
  } catch (error) {
    return hasErrnoCode(error, "ENOENT") ? "gone" : "unreadable";
  }
  try {
    const data: unknown = JSON.parse(text);
    return typeof data === "object" && data !== null
      ? (data as Record<string, unknown>)
      : "unreadable";
  } catch {
    return "unreadable";
  }
}

/**
 * What this session has to say about the write `identity` names. A session whose directory holds
 * no verdict store at all has said nothing: an older gate wrote none, and a session that has
 * refused nothing writes none.
 */
export function readVerdict(session: string, identity: string): VerdictRead {
  const found = readOneVerdict(session, identity);
  if (found !== undefined) return found;
  // A store of another scheme, holding anything: this version cannot say what it holds, and one
  // thing it can hold is a refusal of this very write.
  const foreign = foreignVerdicts(session);
  if (foreign.kind === "foreign")
    return {
      kind: "unknown",
      why: `this session holds AskGate verdicts of a scheme this version does not read (${foreign.path})`,
      path: foreign.path,
    };
  if (foreign.kind === "unreadable")
    return {
      kind: "unknown",
      why: `whether this session holds an AskGate verdict for this write cannot be told: ${foreign.path} cannot be read`,
      path: foreign.path,
    };
  const lost = join(session, VERDICTS_LOST);
  const mark = readVerdictFile(lost);
  if (mark === "gone") return { kind: "none" };
  if (mark === "unreadable")
    return {
      kind: "unknown",
      why: `AskGate's record of a lost verdict cannot be read (${lost})`,
      path: lost,
    };
  return { kind: "lost", path: lost, store: verdictDir(session) };
}

/** This session's verdict filed under one identity, or undefined where it holds none. */
function readOneVerdict(session: string, identity: string): VerdictRead | undefined {
  const file = join(verdictDir(session), identity);
  const unreadable = (path: string, what: string): VerdictRead => ({
    kind: "unknown",
    why: `AskGate's ${what} cannot be read (${path})`,
    path,
  });
  const found = readVerdictFile(file);
  if (found === "unreadable") return unreadable(file, "verdict on this write");
  if (found !== "gone") {
    const { reason, at } = found;
    if (typeof reason !== "string" || typeof at !== "number")
      return unreadable(file, "verdict on this write");
    return { kind: "refused", reason, at };
  }
  return undefined;
}
