// What every part of the dispatch-session-settings driver (dispatch-session-settings.ts) shares:
// the run's setup as the shell script wrote it, the person the page signs in as, the transcript
// (`== <name>`, notes, `CHECK <name>: PASS`, and the failure that ends a check), and the polling
// helpers.

export interface Rig {
  readonly work: string;
  readonly evidence: string;
  readonly omp: string;
  readonly dispatchUrl: string;
  readonly envoyUrl: string;
  readonly natsUrl: string;
  /** The model stand-in every keyed provider of the profile names (lib/session-settings-model.ts). */
  readonly standinUrl: string;
  /** The file holding the placeholder key the stand-in takes, which the profile names. */
  readonly standinKeyFile: string;
  /** A loopback port held bound and never listened on, the broken provider's. */
  readonly brokenPort: number;
  readonly plugin: string;
  readonly home: string;
  /** The profile's agent directory, `<home>/.omp/agent`, where writeStandinProfile writes. */
  readonly agentDir: string;
  readonly sessionCwd: string;
  readonly tmp: string;
  readonly envoyTokenFile: string;
  readonly dispatchTokenFile: string;
  readonly pidFile: string;
}

/** The person the held-open page is signed in as, and whose sends the checks make. */
export const PERSON = "alice";

// ---- the transcript -------------------------------------------------------------------------------

export class CheckFailure extends Error {}

// Before the first check begins, a failure (the profile could not be written) is the driver's own.
let current = "driver";
let checkNumber = 0;
let checkStarted = Date.now();

/** The check or setup stage running now, as the transcript names it. */
export function currentName(): string {
  return current;
}
/** The running check's number, from 1; 0 during setup. */
export function currentNumber(): number {
  return checkNumber;
}

export function begin(name: string): void {
  checkNumber += 1;
  current = name;
  checkStarted = Date.now();
  process.stdout.write(`== ${name}\n`);
}
export function note(text: string): void {
  process.stdout.write(`   ${text}\n`);
}
export function pass(): void {
  process.stdout.write(`   took ${Math.round((Date.now() - checkStarted) / 1000)}s\n`);
  process.stdout.write(`CHECK ${current}: PASS\n`);
}
export function fail(message: string): never {
  throw new CheckFailure(message);
}
export function expect(condition: boolean, message: string): void {
  if (!condition) fail(message);
}

// ---- waiting --------------------------------------------------------------------------------------

export const sleep = (ms: number) => new Promise<void>((resolve) => setTimeout(resolve, ms));

/** Polls `probe` every quarter second until it answers something other than undefined, for at
 *  most `limitMs`; the failure names what it waited for and the last thing it saw. */
export async function until<T>(
  what: string,
  limitMs: number,
  probe: () => Promise<T | undefined> | T | undefined,
  last?: () => string
): Promise<T> {
  const deadline = Date.now() + limitMs;
  for (;;) {
    const value = await probe();
    if (value !== undefined) return value;
    if (Date.now() > deadline) {
      fail(
        `timed out after ${limitMs / 1000}s waiting for ${what}${last ? `; last: ${last()}` : ""}`
      );
    }
    await sleep(250);
  }
}
