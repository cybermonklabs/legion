import { afterAll, beforeAll, describe, expect, test } from "bun:test";
import { createHash } from "node:crypto";
import { cp, mkdtemp, readFile, realpath, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { CLAIM_RUN_VARIABLE, RESERVE_VARIABLE } from "../reserve";
import {
  closureDigestOf,
  DECISION_PRELOAD_FILE,
  DecisionError,
  importClosureOf,
  RESERVE_ASK_CLOSURE,
  RESERVE_ASK_REGISTRATION,
  type ReserveVerdict,
  reserveAskDecision,
  spawnDecision,
} from "../s10-decision";

/**
 * The reserve's decision the S10 inputs stage takes on every ask among its hits, as
 * `s10-decision.ts` makes it: its pin, over this checkout's files and over copies of them changed
 * here, and the children it runs, over a stub that stands in for `reserve-ask.ts`. No copy of
 * `reserve-ask.ts` is ever run: the pin is read from the bytes, and only the stub is spawned,
 * with this checkout's preload or a copy of it beside a `claim.ts` changed here.
 */

const CLAIM = "dispatch://ACME-1/comment/registration";
const START = "start-1";

/** A record that ended the reserve, as a changed `claim.ts` names one. */
const ENDED = "dispatch://ACME-1/comment/claim-result";

/** This directory, which holds `reserve-ask.ts`, the rest of its closure and its preload. */
const HERE = join(import.meta.dir, "..");

/** The digest of the pinned closure at this head, as `sha256sum` over its six files prints it. */
const PINNED_DIGEST = "55931c3f6c327a25abdfb70453e7692e9c61ed10cd9ff4ca1443ebceb15eadab";

let scratch: string | undefined;

beforeAll(async () => {
  scratch = await realpath(await mkdtemp(join(tmpdir(), "s10-decision-")));
});

afterAll(async () => {
  if (scratch !== undefined) await rm(scratch, { recursive: true, force: true });
});

/** A directory under this file's scratch directory, by its real path, as Bun resolves imports. */
async function directory(name: string): Promise<string> {
  return mkdtemp(join(scratch as string, `${name}-`));
}

/** A directory holding a copy of each file of the closure, `claim.ts` with them, and no other. */
async function closureCopy(): Promise<string> {
  const copy = await directory("closure");
  for (const name of [...Object.keys(RESERVE_ASK_CLOSURE), RESERVE_ASK_REGISTRATION]) {
    await cp(join(HERE, name), join(copy, name));
  }
  return copy;
}

/** `name` in `copy` with its last byte, the newline that ends the file, made a space. */
async function oneByteChanged(copy: string, name: string): Promise<string> {
  const path = join(copy, name);
  const bytes = await readFile(path);
  expect(bytes[bytes.length - 1]).toBe(0x0a);
  bytes[bytes.length - 1] = 0x20;
  await writeFile(path, bytes);
  return createHash("sha256").update(bytes).digest("hex");
}

/**
 * The `claim.ts` in `directory` with `from` written as `to`, refused where it does not hold
 * `from`; gives back the text written.
 */
async function claimChanged(directory: string, from: string, to: string): Promise<string> {
  const path = join(directory, RESERVE_ASK_REGISTRATION);
  const before = await readFile(path, "utf8");
  const after = before.replace(from, to);
  expect(after).not.toBe(before);
  await writeFile(path, after, "utf8");
  return after;
}

/** What a refusal of the decision at `entry` says before its reason. */
function refusalOf(entry: string): string {
  return `${entry} is not the decision the claim names, reserve-ask.ts at 651db8d5: `;
}

/** The sentence every refusal of the pin ends with. */
const PIN_MOVES =
  "A change to a file of its closure is refused until a reviewed pull request moves the pin";

describe("the decision's pin", () => {
  test("this checkout's reserve-ask.ts with its preload loads the pinned files and claim.ts", async () => {
    const closure = await importClosureOf([
      join(HERE, "reserve-ask.ts"),
      join(HERE, DECISION_PRELOAD_FILE),
    ]);
    expect(closure).toEqual({
      ...RESERVE_ASK_CLOSURE,
      [RESERVE_ASK_REGISTRATION]: expect.stringMatching(/^[0-9a-f]{64}$/),
    });
    expect(Object.keys(RESERVE_ASK_CLOSURE)).toEqual([
      "corpus-io.ts",
      DECISION_PRELOAD_FILE,
      "dispatch-read.ts",
      "reserve-ask.ts",
      "reserve.ts",
      "stats.ts",
    ]);
    expect(closureDigestOf(RESERVE_ASK_CLOSURE)).toBe(PINNED_DIGEST);
  });

  test("a copy whose claim.ts registers a read is admitted, under the digest of the pinned files", async () => {
    const copy = await closureCopy();
    const read = `{ reference: "${CLAIM}", run: "${"a".repeat(64)}" }`;
    const registered = await claimChanged(
      copy,
      "RESERVE_READS: readonly ReserveRead[] = [];",
      `RESERVE_READS: readonly ReserveRead[] = [${read}];`
    );
    const entry = join(copy, "reserve-ask.ts");
    // The closure loads the changed claim.ts, and holds it to no pinned sha256.
    const closure = await importClosureOf([entry, join(copy, DECISION_PRELOAD_FILE)]);
    const sha256 = createHash("sha256").update(registered, "utf8").digest("hex");
    expect(closure[RESERVE_ASK_REGISTRATION]).toBe(sha256);
    const decision = await reserveAskDecision(entry, null);
    expect(decision.sha256).toBe(closureDigestOf(RESERVE_ASK_CLOSURE));
    expect(decision.sha256).toBe(PINNED_DIGEST);
    await expect(decision.confirm()).resolves.toBeUndefined();
  });

  test("refuses the decision when the RESERVE_ENDED handed in is set, under which it keeps every ask", async () => {
    const entry = join(await closureCopy(), "reserve-ask.ts");
    await expect(reserveAskDecision(entry, ENDED)).rejects.toThrow(DecisionError);
    await expect(reserveAskDecision(entry, ENDED)).rejects.toThrow(
      `claim.ts records that the reserve ended (${ENDED}), and the decision at 651db8d5 decides ` +
        "under a reserve that holds, where a set RESERVE_ENDED keeps every ask"
    );
  });

  test("refuses a closure one byte of whose stats.ts changed, naming the file and both sha256s", async () => {
    const copy = await closureCopy();
    const changed = await oneByteChanged(copy, "stats.ts");
    const pinned = RESERVE_ASK_CLOSURE["stats.ts"];
    const why = `its stats.ts has sha256 ${changed}, and the pin names ${pinned}`;
    const entry = join(copy, "reserve-ask.ts");
    await expect(reserveAskDecision(entry, null)).rejects.toThrow(DecisionError);
    await expect(reserveAskDecision(entry, null)).rejects.toThrow(
      `${refusalOf(entry)}${why}. ${PIN_MOVES}`
    );
  });

  test("refuses a preload one byte of which changed, since a preload could change every verdict", async () => {
    const copy = await closureCopy();
    const changed = await oneByteChanged(copy, DECISION_PRELOAD_FILE);
    const pinned = RESERVE_ASK_CLOSURE[DECISION_PRELOAD_FILE];
    const why = `its ${DECISION_PRELOAD_FILE} has sha256 ${changed}, and the pin names ${pinned}`;
    const entry = join(copy, "reserve-ask.ts");
    await expect(reserveAskDecision(entry, null)).rejects.toThrow(DecisionError);
    await expect(reserveAskDecision(entry, null)).rejects.toThrow(`${refusalOf(entry)}${why}.`);
  });

  test("refuses a closure that loads a file the pin does not name, naming that file", async () => {
    const copy = await closureCopy();
    // A name before every pinned one, so the refusal names it ahead of the stats.ts that loads it.
    await writeFile(join(copy, "a-new.ts"), "export {};\n", "utf8");
    const stats = join(copy, "stats.ts");
    await writeFile(stats, `import "./a-new";\n${await readFile(stats, "utf8")}`, "utf8");
    const entry = join(copy, "reserve-ask.ts");
    const why = "it loads a-new.ts, which the pin does not name";
    await expect(reserveAskDecision(entry, null)).rejects.toThrow(DecisionError);
    await expect(reserveAskDecision(entry, null)).rejects.toThrow(`${refusalOf(entry)}${why}.`);
  });

  test("refuses a reserve-ask.ts with no preload or no claim.ts beside it, or no other pinned file", async () => {
    const lone = await directory("reserve-ask");
    const entry = join(lone, "reserve-ask.ts");
    await writeFile(entry, "console.log('another rule');\n", "utf8");
    const preload = join(lone, DECISION_PRELOAD_FILE);
    await expect(reserveAskDecision(entry, null)).rejects.toThrow(DecisionError);
    await expect(reserveAskDecision(entry, null)).rejects.toThrow(
      `${preload}, which the decision loads, could not be read`
    );
    // The preload reads claim.ts's RESERVE_ENDED, so with no claim.ts its import does not resolve.
    await cp(join(HERE, DECISION_PRELOAD_FILE), preload);
    await expect(reserveAskDecision(entry, null)).rejects.toThrow(DecisionError);
    await expect(reserveAskDecision(entry, null)).rejects.toThrow(
      `${preload} imports ./claim, which does not resolve`
    );
    // With both beside it, the pinned files are compared, and the first the closure lacks named.
    await cp(join(HERE, RESERVE_ASK_REGISTRATION), join(lone, RESERVE_ASK_REGISTRATION));
    const why = "it loads no corpus-io.ts, which the pin names";
    await expect(reserveAskDecision(entry, null)).rejects.toThrow(DecisionError);
    await expect(reserveAskDecision(entry, null)).rejects.toThrow(
      `${refusalOf(entry)}${why}. ${PIN_MOVES}`
    );
  });

  test("confirm passes while nothing changed, and refuses a pinned file changed since the admission", async () => {
    const copy = await closureCopy();
    const entry = join(copy, "reserve-ask.ts");
    const decision = await reserveAskDecision(entry, null);
    await expect(decision.confirm()).resolves.toBeUndefined();
    const changed = await oneByteChanged(copy, "dispatch-read.ts");
    const pinned = RESERVE_ASK_CLOSURE["dispatch-read.ts"];
    const why = `its dispatch-read.ts has sha256 ${changed}, and the pin names ${pinned}`;
    // The second admission refuses as the first would have, in the same words.
    await expect(decision.confirm()).rejects.toThrow(DecisionError);
    await expect(decision.confirm()).rejects.toThrow(`${refusalOf(entry)}${why}. ${PIN_MOVES}`);
  });
});

/**
 * A stand-in for `reserve-ask.ts` that a decision runs: it exits 3 inside the claim's read, 4
 * where the rest of the environment it was handed did not reach it, and else as its ask id
 * (`Bun.argv[3]`) asks, the last by an error it does not catch.
 */
const STUB = [
  `if (process.env.${RESERVE_VARIABLE} !== undefined) process.exit(3);`,
  `if (process.env.${CLAIM_RUN_VARIABLE} !== undefined) process.exit(3);`,
  'if (process.env.S10_STUB !== "handed on") process.exit(4);',
  "const askId = Bun.argv[3];",
  'if (askId === "kept") process.exit(0);',
  'if (askId === "held") process.exit(1);',
  'if (askId === "error") process.exit(2);',
  'throw new Error("an error the stub does not catch");',
  "",
].join("\n");

/** The environment each decision is handed: the claim's read, and what the stub looks for. */
const ENV = {
  PATH: process.env.PATH,
  [RESERVE_VARIABLE]: `read:${CLAIM}`,
  [CLAIM_RUN_VARIABLE]: START,
  S10_STUB: "handed on",
};

describe("the decision's children", () => {
  test("each runs outside the read, and an error the child does not catch is no hold", async () => {
    const stub = join(await directory("stub"), "decide.ts");
    await writeFile(stub, STUB, "utf8");
    const decide = spawnDecision(stub, join(HERE, DECISION_PRELOAD_FILE), ENV);
    const verdicts: ReserveVerdict[] = [];
    for (const askId of ["kept", "held", "error", "uncaught"]) verdicts.push(await decide(askId));
    // Bun alone exits 1 on an uncaught error, which would read as held; the preload makes it 70.
    expect(verdicts).toEqual(["kept", "held", "error", "error"]);
  });

  test("a child whose own claim.ts records that the reserve ended decides nothing, whenever it runs", async () => {
    const beside = await directory("preload");
    for (const name of [DECISION_PRELOAD_FILE, RESERVE_ASK_REGISTRATION]) {
      await cp(join(HERE, name), join(beside, name));
    }
    const stub = join(beside, "decide.ts");
    await writeFile(stub, STUB, "utf8");
    const decide = spawnDecision(stub, join(beside, DECISION_PRELOAD_FILE), ENV);
    // The copy of this checkout's claim.ts holds, so the stub's exit 0 is the verdict.
    expect(await decide("kept")).toBe("kept");
    // The same decision, after its claim.ts records an end: the preload exits 71 before the stub.
    await claimChanged(
      beside,
      "RESERVE_ENDED: string | null = null;",
      `RESERVE_ENDED: string | null = "${ENDED}";`
    );
    expect(await decide("kept")).toBe("error");
  });

  test("a child whose claim.ts throws as it loads decides nothing, and is no hold", async () => {
    const beside = await directory("throwing-claim");
    for (const name of [DECISION_PRELOAD_FILE, RESERVE_ASK_REGISTRATION]) {
      await cp(join(HERE, name), join(beside, name));
    }
    const stub = join(beside, "decide.ts");
    await writeFile(stub, STUB, "utf8");
    const claim = join(beside, RESERVE_ASK_REGISTRATION);
    await writeFile(
      claim,
      `${await readFile(claim, "utf8")}throw new Error("claim.ts throws as it loads");\n`,
      "utf8"
    );
    // The preload's handlers are registered before it loads claim.ts, so the child exits 70, an
    // error, before the stub would keep the ask; Bun's own exit 1 would read as held.
    const decide = spawnDecision(stub, join(beside, DECISION_PRELOAD_FILE), ENV);
    expect(await decide("kept")).toBe("error");
  });
});
