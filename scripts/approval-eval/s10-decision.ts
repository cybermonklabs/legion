import { createHash } from "node:crypto";
import { readFile } from "node:fs/promises";
import { dirname, isAbsolute, join, relative, resolve } from "node:path";
import { RESERVE_ENDED } from "./claim";
import { CannotRunError } from "./corpus-io";
import { CLAIM_RUN_VARIABLE, RESERVE_VARIABLE } from "./reserve";

/**
 * The reserve's decision the claim's S10 inputs stage (`s10-inputs.ts`) takes on every ask among
 * its search hits, point 4 (d): `reserve-ask.ts` as commit 651db8d5 decides it, run outside the
 * claim's read. This module is the decision's pin, so moving the pin edits this file alone.
 *
 * The pin. Before anything is read, the import closure of `reserve-ask.ts` in this checkout and
 * of the preload its children run with (`decision-preload.ts`), every file Bun loads to run them
 * (`importClosureOf`; a type-only import, which Bun erases, loads none), must be the one pinned
 * here: the sha256 of each of its files (`RESERVE_ASK_CLOSURE`) but `claim.ts`, and of `claim.ts`
 * the one value the decision reads, `RESERVE_ENDED`, which must be null, as at 651db8d5. The
 * parent checks that value as this checkout loaded it, and the preload checks it again in each
 * child, as that child loaded it, before the child decides anything.
 *
 * `claim.ts` is the registration, which the claim changes to register its run digests, and which
 * is outside the run digest so that a registration does not move the digest it registers: this
 * file is inside it, so pinning `claim.ts`'s bytes here would move every run digest with each
 * registration. Nothing else of `claim.ts` reaches the decision. `reserve-ask.ts` calls, of
 * `reserve.ts`, only `reserveRuleOf`, whose `ended` defaults to `RESERVE_ENDED`, and
 * `noteIssueParent`, `treeRootOf`, `ruleVersionsOf` and constants, none of which reads
 * `claim.ts`. `reserve.ts` reads `RESERVE_READS` only into `REGISTRATION`, which `readCorpus` and
 * `reserveAccessOf` take and the decision never calls; under no read, `reserveAccessOf` gives
 * `ended` or `held` before it looks at a read anyway. `claim.ts` imports nothing, so a file it
 * came to import would be a file of the closure the pin does not name, and refused; and
 * `reserve.ts`, which is pinned, imports it, so a closure that loads no `claim.ts` is refused as
 * one that cannot be resolved.
 *
 * Against 651db8d5, `reserve-ask.ts` and `stats.ts` are byte for byte the same, and the pinned
 * `reserve.ts`, `corpus-io.ts` and `dispatch-read.ts` differ only in what the decision never
 * calls: the `--draw`, `--inputs` and `--claim-s10` path options of `runDigestOf` and the file
 * options of `s10_judge.py`, the two sha256s `readCorpus` records and the four field readers the
 * claim's stage readers share (`parsed`, `countAt`, `textAt`, `sha256At`), and `pageEventLog`'s
 * `after` and the new `readBytes`; and `dispatch-read.ts` differs in one path the decision does
 * call, its retry ladder, only in form: the waits are held as 1, 2, 4 and 8 units of a second,
 * beside two functions only a test calls to shorten that unit (`setRetryWaitUnitForTests`,
 * `resetRetryWaitUnitForTests`), so the decision still waits 1, 2, 4 and 8 seconds. So the
 * pinned closure is the 651db8d5 decision, and any later
 * change to a pinned file fails this check until a reviewed pull request moves the pin. The
 * pinned files are checked again once, right before the first child is spawned
 * (`Decision.confirm`).
 *
 * The children. Each ask is decided by a child process (`spawnDecision`) run with neither
 * `APPROVAL_EVAL_RESERVE` nor `APPROVAL_EVAL_CLAIM_RUN` in its environment, so outside the claim's
 * read, and with `decision-preload.ts` preloaded: exit 0 keeps the candidate, 1 holds it, and any
 * other exit (2 for an error `reserve-ask.ts` catches, 70 for one it does not, 71 for a child
 * whose `claim.ts` records that the reserve ended) holds it too and is counted as an error, as
 * S10 counts it. What a child prints is not kept: a held ask's facts are never written.
 */

/**
 * The import closure of the decision at this head: the sha256 of each file Bun loads to run
 * `reserve-ask.ts` with its preload, by its path from this directory, but the registration
 * (`RESERVE_ASK_REGISTRATION`), whose one value the decision reads is checked instead. A checkout
 * whose closure differs in any of these files, or loads a file more or less, is refused.
 */
export const RESERVE_ASK_CLOSURE: Readonly<Record<string, string>> = {
  "corpus-io.ts": "fa89727871dcf12c849756f84a888e52c6141c81467a353277fbaf98aba03198",
  "decision-preload.ts": "50ff3dd835d4345aac95c64df7a3e820a30c625b1e1e63e1c0277ea278dccad3",
  "dispatch-read.ts": "60353edd7f7d8b078aba0c2473f52dc138d587b14dd0fd18afa9f82e5e120a9b",
  "reserve-ask.ts": "604e2f943ed71f6d43761f594d333e083d9d2811f28df779673b32a2fefdde42",
  "reserve.ts": "9debce6d78cc8f6579f5a393540fb97124db53ca25401e9d617a41615320724d",
  "stats.ts": "aaf9b5a598436d3ae5be4173b659f5889a24dd4d8b8afef0be9b5852eec6ffe9",
};

/**
 * The file of the closure that holds the claim's registration, whose bytes are not pinned: of it
 * the decision reads `RESERVE_ENDED` alone, which must be null.
 */
export const RESERVE_ASK_REGISTRATION = "claim.ts";

/** The preload each child runs with, beside the `reserve-ask.ts` it runs. */
export const DECISION_PRELOAD_FILE = "decision-preload.ts";

/** That command in this checkout, which is run for every ask among the search hits. */
const RESERVE_ASK_PATH = join(import.meta.dir, "reserve-ask.ts");

/** The decision is not the one the claim names, so the command stops before it reads. */
export class DecisionError extends CannotRunError {
  override readonly name = "DecisionError";
}

/** How `reserve-ask.ts` decided one ask: kept, held, or an error, which S10 holds and counts. */
export type ReserveVerdict = "kept" | "held" | "error";

/** Decides one ask under the reserve, outside the claim's read. */
export type ReserveDecision = (askId: string) => Promise<ReserveVerdict>;

/**
 * The decision a run makes every candidate's with: the sha256 its stage record names it by, the
 * digest of the pinned closure (`reserveAskDecision`) or a test's own, which must be a sha256 as
 * well; the decision itself; and `confirm`, which refuses where the decision's files are no
 * longer the pinned ones, and which the stage calls once, right before it spawns its first child.
 */
export interface Decision {
  sha256: string;
  decide: ReserveDecision;
  confirm: () => Promise<void>;
}

/** The sha256 of some bytes. */
function sha256Of(bytes: Uint8Array | string): string {
  return createHash("sha256").update(bytes).digest("hex");
}

/**
 * The sha256 of each file Bun loads to run `entries`, by its path from the first entry's
 * directory: each entry itself and every file it imports at run time, transitively, as Bun's
 * transpiler reads the imports (`scanImports`, which leaves out the type-only imports Bun
 * erases). A built-in module is no file and is left out.
 */
export async function importClosureOf(entries: readonly string[]): Promise<Record<string, string>> {
  const root = dirname(resolve(entries[0] ?? "."));
  const transpiler = new Bun.Transpiler({ loader: "ts" });
  const files: Record<string, string> = {};
  const waiting = entries.map((entry) => resolve(entry));
  for (let path = waiting.pop(); path !== undefined; path = waiting.pop()) {
    const name = relative(root, path);
    if (Object.hasOwn(files, name)) continue;
    let bytes: Buffer;
    try {
      bytes = await readFile(path);
    } catch {
      throw new DecisionError(`${path}, which the decision loads, could not be read`);
    }
    files[name] = sha256Of(bytes);
    for (const imported of transpiler.scanImports(bytes.toString("utf8"))) {
      let target: string;
      try {
        target = Bun.resolveSync(imported.path, dirname(path));
      } catch {
        throw new DecisionError(`${path} imports ${imported.path}, which does not resolve`);
      }
      if (isAbsolute(target)) waiting.push(target);
    }
  }
  return Object.fromEntries(Object.entries(files).sort(([one], [other]) => (one < other ? -1 : 1)));
}

/** The one sha256 of a closure: of its `<sha256>  <path>` lines, by path, as `sha256sum` prints. */
export function closureDigestOf(closure: Readonly<Record<string, string>>): string {
  const names = Object.keys(closure).sort();
  return sha256Of(names.map((name) => `${closure[name]}  ${name}\n`).join(""));
}

/** The first file in which a closure is not the pinned one, worded for a refusal; null if none. */
function closureRefusal(
  found: Readonly<Record<string, string>>,
  pinned: Readonly<Record<string, string>>
): string | null {
  for (const name of [...new Set([...Object.keys(pinned), ...Object.keys(found)])].sort()) {
    const is = Object.hasOwn(found, name) ? found[name] : undefined;
    const was = Object.hasOwn(pinned, name) ? pinned[name] : undefined;
    if (is === was) continue;
    if (is === undefined) return `it loads no ${name}, which the pin names`;
    if (was === undefined) return `it loads ${name}, which the pin does not name`;
    return `its ${name} has sha256 ${is}, and the pin names ${was}`;
  }
  return null;
}

/**
 * The digest of the decision at `path`, refused unless its closure with its preload, the
 * registration aside, is `RESERVE_ASK_CLOSURE`, and `ended`, the registration's
 * `RESERVE_ENDED`, is null as at 651db8d5: a set one keeps every ask (`reserveRuleOf`). Since the
 * pinned part must equal the pin, the digest is always that of `RESERVE_ASK_CLOSURE`.
 */
async function admittedDigestOf(path: string, ended: string | null): Promise<string> {
  const closure = await importClosureOf([path, join(dirname(path), DECISION_PRELOAD_FILE)]);
  const pinned = Object.fromEntries(
    Object.entries(closure).filter(([name]) => name !== RESERVE_ASK_REGISTRATION)
  );
  const refusal = closureRefusal(pinned, RESERVE_ASK_CLOSURE);
  if (refusal !== null) {
    throw new DecisionError(
      `${path} is not the decision the claim names, reserve-ask.ts at 651db8d5: ${refusal}. ` +
        "A change to a file of its closure is refused until a reviewed pull request moves the pin"
    );
  }
  if (ended !== null) {
    throw new DecisionError(
      `${RESERVE_ASK_REGISTRATION} records that the reserve ended (${ended}), and the decision ` +
        "at 651db8d5 decides under a reserve that holds, where a set RESERVE_ENDED keeps every ask"
    );
  }
  return closureDigestOf(pinned);
}

/**
 * The decision point 4 (d) names, admitted (`admittedDigestOf`) and named by the digest of the
 * pinned files, whose `confirm` admits it again. The inputs stage calls `confirm` once, right
 * before its first child, so a pinned file changed between the run's start and that moment is
 * refused before any child loads it; one changed while the children run is not checked again.
 * `ended` is this checkout's `RESERVE_ENDED`, of the `claim.ts` the decision at
 * `RESERVE_ASK_PATH` loads; only a test hands in another, with a `path` of its own. A `claim.ts`
 * changed after that check is read by each child's preload.
 */
export async function reserveAskDecision(
  path: string = RESERVE_ASK_PATH,
  ended: string | null = RESERVE_ENDED
): Promise<Decision> {
  const sha256 = await admittedDigestOf(path, ended);
  const confirm = async (): Promise<void> => {
    await admittedDigestOf(path, ended);
  };
  const preload = join(dirname(path), DECISION_PRELOAD_FILE);
  return { sha256, decide: spawnDecision(path, preload, process.env), confirm };
}

/**
 * Decides each ask by running the script at `path` for it, `--ask <ask id>`, in a child with
 * `env` less `APPROVAL_EVAL_RESERVE` and `APPROVAL_EVAL_CLAIM_RUN`, so outside the claim's read,
 * and with `preload` (`decision-preload.ts`) preloaded. Exit 0 keeps the ask, 1 holds it, and any
 * other exit is an error: `reserve-ask.ts` exits 2 on an error it catches, and the preload makes
 * one it does not exit 70 rather than 1, and a child whose `claim.ts` records that the reserve
 * ended exit 71. What the child prints is not kept.
 */
export function spawnDecision(
  path: string,
  preload: string,
  env: Readonly<Record<string, string | undefined>>
): ReserveDecision {
  const outside = new Set<string>([RESERVE_VARIABLE, CLAIM_RUN_VARIABLE]);
  const childEnv = Object.fromEntries(Object.entries(env).filter(([name]) => !outside.has(name)));
  return async (askId) => {
    const child = Bun.spawn([process.execPath, "--preload", preload, path, "--ask", askId], {
      env: childEnv,
      stdout: "ignore",
      stderr: "ignore",
    });
    const exit = await child.exited;
    return exit === 0 ? "kept" : exit === 1 ? "held" : "error";
  };
}
