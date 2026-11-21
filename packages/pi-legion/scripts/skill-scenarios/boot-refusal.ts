// The one reader of a claim boot refusal: whether an agent's claim boot was refused, and whether
// before its first turn. Two programs read it: rig.sh's boot_agent, through this module's own
// command line, inside every run's boot, to decide whether to launch the agent once more; and
// health.ts's "claim boot" rule, which holds the run to the same record. score.ts reads a half's
// own claim boot sentence through it too (`claimBootFailure`). `health.ts --boot-refusal`, the
// entry boot_agent asked through before the names left the world, answers the same.
//
// It is on the world's path, so it imports neither health.ts nor score.ts, whose readers of a
// whole run change with every rule: only the leaf modules a run's record needs, and
// boot-refusal.test.ts walks the graph. That graph is completion-answers.ts (the daemon
// stand-in's log, run-records.ts's opening line under it, and run-names.ts's record of the run's
// label), daemon-pane.ts (`labelCheckout`, the checkout a run's built-from names) and
// transcript.ts (an attempt's transcripts).
import { existsSync, readdirSync, readFileSync } from "node:fs";
import path from "node:path";
import { answerOf, type DaemonLine, daemonLines } from "./completion-answers";
import { labelCheckout } from "./daemon-pane";
import { type SessionEntry, transcriptEntries } from "./transcript";

const USAGE = "usage: boot-refusal.ts <run dir> <role> <since> <sessions dir>";

/** The words the sentence every label's plugin prints for a claim boot that registered and could
 * not finish starts with: an older plugin's, main ff2a10ef's, `claim boot failed after
 * claims/register registered <claim token>; …` (packages/pi-shared/src/legion-claim-core.ts:97
 * there) and this checkout's `claim boot failed at <step> after claims/register registered <role>
 * with the daemon at …; …` (`stopAfterRegistration`). Only an output that holds them is read for
 * the label's own opening (`claimBootOpening`), so a run that met no such failure needs no
 * checkout to read. */
const CLAIM_BOOT_WORDS = "claim boot failed ";
/** The module, under a checkout, that writes that sentence. */
const CLAIM_CORE = path.join("packages", "pi-shared", "src", "legion-claim-core.ts");
/** Each checkout's opening, read once. */
const claimBootOpenings = new Map<string, string>();
/** The opening of the sentence the plugin of `checkout` prints for a claim boot that registered
 * and could not finish, read out of that checkout's own CLAIM_CORE, as the daemon facts a run's
 * stand-in answers by are read out of its checkout: the value of the module's
 * CLAIM_BOOT_FAILED_OPENING where it names one (this checkout's, `claim boot failed at `), and
 * otherwise the text of the template that writes the sentence up to its first substitution (an
 * older plugin's, main ff2a10ef's, `claim boot failed after claims/register registered `). The two
 * word it apart, so one checkout's opening read for another's runs never finds the other's
 * sentence. Throws for a checkout whose module writes no such sentence: what its plugin prints is
 * unknown. */
export function claimBootOpening(checkout: string): string {
  const known = claimBootOpenings.get(checkout);
  if (known !== undefined) return known;
  const file = path.join(checkout, CLAIM_CORE);
  const source = existsSync(file) ? readFileSync(file, "utf8") : "";
  const opening =
    /\bCLAIM_BOOT_FAILED_OPENING\s*=\s*"([^"]+)"/.exec(source)?.[1] ??
    /`(claim boot failed [^`$]*)/.exec(source)?.[1];
  if (opening === undefined)
    throw new Error(
      `${file} writes no sentence for a claim boot that registered and could not finish (\`${CLAIM_BOOT_WORDS}…\`), so what that checkout's plugin prints for one is unknown`
    );
  claimBootOpenings.set(checkout, opening);
  return opening;
}
/** The sentence the claim boot leaves in a half's own output when a session that registered
 * could not finish booting, or undefined when `text` holds none: the line opening with the
 * opening of the run's own label's plugin (`claimBootOpening` of the checkout the run's
 * built-from names, daemon-pane.ts's `labelCheckout`), which names why. The agent then exits so
 * the daemon would launch it again. The rig launches it once more only when that came before its
 * first turn, and keeps that first attempt under <run>/boot-1/<role>/, which this reads none of
 * (rig.sh's boot_agent); a half whose own output holds the line is one refused after its first
 * turn or on its relaunch too, so the half is gone from the middle of the run (score.ts's
 * `unscored`). */
export function claimBootFailure(text: string, runDir: string): string | undefined {
  if (!text.includes(CLAIM_BOOT_WORDS)) return undefined;
  const opened = text.indexOf(claimBootOpening(labelCheckout(runDir)));
  if (opened < 0) return undefined;
  const ended = text.indexOf("\n", opened);
  return ended < 0 ? text.slice(opened) : text.slice(opened, ended);
}

/** The route a phase worker's claim boot registers on. */
const REGISTER_PATH = "/legion/v1/claims/register";

/** One launch of an agent as the run records it: its pane output, the daemon stand-in's lines of
 * it, and every entry of its session's transcripts, its subagents' included; `run` is the run's
 * directory, whose built-from names the checkout its plugin came from. */
export interface BootAttempt {
  readonly run: string;
  readonly out: string;
  readonly daemon: readonly DaemonLine[];
  readonly entries: readonly SessionEntry[];
}

/** The refusal an attempt's claim boot met, or undefined: the daemon stand-in's refusal of its
 * `claims/register`, or the line the run's own label's plugin prints for a boot that registered
 * and could not finish (`claimBootFailure`, which reads that label's wording out of its
 * checkout), which round 23's refusal by the Envoy listener of a session that held no consumer
 * ends in. The plugin exits on either, so the daemon launches the agent again. */
export function bootRefusal(attempt: BootAttempt): string | undefined {
  const refused = attempt.daemon.find((line) => line.path === REGISTER_PATH && line.status >= 400);
  if (refused !== undefined)
    return `the daemon stand-in answered the ${refused.role ?? "agent"}'s ${refused.method ?? "POST"} ${REGISTER_PATH} ${answerOf(refused)}`;
  return claimBootFailure(attempt.out, attempt.run);
}

/** Whether an attempt made a turn: a model call or a tool call its transcripts recorded, an
 * assistant message or a tool result. */
export function madeTurn(attempt: BootAttempt): boolean {
  return attempt.entries.some(
    (entry) => entry.message?.role === "assistant" || entry.message?.role === "toolResult"
  );
}

/** Every entry of every transcript under `dir`, at any depth: an attempt's session directory,
 * which holds its subagents' too. */
export function entriesUnder(dir: string): SessionEntry[] {
  if (!existsSync(dir)) return [];
  return readdirSync(dir, { recursive: true, encoding: "utf8" })
    .filter((file) => file.endsWith(".jsonl"))
    .sort()
    .flatMap((file) => transcriptEntries(path.join(dir, file)));
}

/** rig.sh's question once an agent's launch has ended (boot_agent), `args` being `<run dir>
 * <role> <since> <sessions dir>`: the refusal rig.sh relaunches the agent for, a claim boot
 * refused (`bootRefusal`) before its first turn (`madeTurn`), read from <run>/out.txt, the
 * stand-in's lines of that role at or after <since> and the transcripts under <sessions dir>. One
 * refused after a turn is never relaunched: the daemon's relaunch would not take that turn back.
 * Prints the refusal and answers 0 for a relaunch; answers 1 for none, a refusal after a turn
 * included; and 2, printing `usage` or `<program>: <why>`, for a usage error or a record it cannot
 * read, which a status of 1 would have read as no refusal. */
export function askBootRefusal(args: readonly string[], usage: string, program: string): number {
  const [dir, role, since, sessions, ...extra] = args;
  if (
    dir === undefined ||
    role === undefined ||
    since === undefined ||
    sessions === undefined ||
    extra.length > 0
  ) {
    console.error(usage);
    return 2;
  }
  let refusal: string | undefined;
  try {
    const out = path.join(dir, "out.txt");
    const attempt = {
      run: dir,
      out: existsSync(out) ? readFileSync(out, "utf8") : "",
      daemon: daemonLines(dir).filter((line) => line.role === role && line.at >= since),
      entries: entriesUnder(sessions),
    };
    refusal = madeTurn(attempt) ? undefined : bootRefusal(attempt);
  } catch (error) {
    console.error(`${program}: ${error instanceof Error ? error.message : String(error)}`);
    return 2;
  }
  if (refusal === undefined) return 1;
  console.log(refusal);
  return 0;
}

if (import.meta.main) process.exit(askBootRefusal(Bun.argv.slice(2), USAGE, "boot-refusal"));
