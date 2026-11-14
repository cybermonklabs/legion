import { z } from "zod";
import type { ActiveDispatchConfig } from "../../packages/envoy-client/src/dispatch-config";
import {
  DISPATCH_TOOL_DEADLINE_MS,
  DispatchServiceError,
  transientStatus,
} from "../../packages/envoy-client/src/dispatch-http";
import {
  sessionConfigFile,
  sessionDispatchSettings,
} from "../../packages/envoy-client/src/session-settings";

/** What the production checks of this directory spell the same way: the verdict each answers and
 * the exit code that verdict takes, the fixed category an error is reported as, the issue key
 * each reads, the `/healthz` body each reads with the build line each prints from it, the reads
 * each makes of its own, the Dispatch every command resolves, the verdict a whole run takes from
 * its parts, and the opening and the ending each gives its process. A check's own flags, usage
 * texts, schemas, steps and the rest of its printed lines stay in the check itself. */

/** An issue key, as every check reads one the server answered. */
export const ISSUE_KEY = /^[A-Z][A-Z0-9]*-[1-9][0-9]*$/;

/** The verdict of a check, of one of its parts, or of a whole run. */
export type Verdict = "pass" | "fail" | "unavailable";

/** The fixed words a check prints in place of an error's own text: the five `errorCategory`
 * answers, and the `config` a check prints when Dispatch is off. A check that is unavailable for
 * a reason of its own, as the meaning search check is for a query no word of which is indexable,
 * names that reason in a union of its own over this one. */
export type Category = "auth" | "refused" | "transport" | "malformed" | "not-found" | "config";

export const EXIT_CODE = { pass: 0, fail: 1, unavailable: 2 } as const satisfies Record<
  Verdict,
  0 | 1 | 2
>;

/** A command line the script refuses. Its message is fixed text that never repeats an argument. */
export class UsageError extends Error {
  override readonly name = "UsageError";
}

/** A status the script's own reads do not accept. */
export class HttpStatusError extends Error {
  override readonly name = "HttpStatusError";

  constructor(readonly status: number) {
    super(`HTTP ${status}`);
  }
}

/** The fixed category an error is reported as; its own text never is. */
export function errorCategory(error: unknown): Category {
  if (error instanceof z.ZodError) return "malformed";
  if (!(error instanceof DispatchServiceError || error instanceof HttpStatusError)) {
    return "transport";
  }
  if (transientStatus(error.status)) return "transport";
  if (error.status === 401 || error.status === 403) return "auth";
  if (error.status === 404) return "not-found";
  if (error.status >= 400 && error.status < 500) return "refused";
  return "transport";
}

/** `GET /healthz`, as Dispatch answers it on 200 and on 503. */
export const Health = z.looseObject({
  ok: z.boolean(),
  commit: z
    .string()
    .regex(/^[0-9a-f]{7,40}$/)
    .nullable(),
  schema_version: z.int().min(0).nullable(),
});
export type Health = z.infer<typeof Health>;

/** Parsed JSON from one of the script's own reads, with a status `accept` admits. A body that is
 * not JSON is returned as its text so the schema reports it malformed. */
export async function readJson(
  url: string,
  headers: Record<string, string>,
  accept: (status: number) => boolean
): Promise<unknown> {
  const response = await fetch(url, {
    headers: { Accept: "application/json", ...headers },
    signal: AbortSignal.timeout(DISPATCH_TOOL_DEADLINE_MS),
  });
  if (!accept(response.status)) throw new HttpStatusError(response.status);
  const text = await response.text();
  try {
    return JSON.parse(text);
  } catch {
    return text;
  }
}

export type DispatchConfig = Pick<ActiveDispatchConfig, "url" | "credential">;

/** Dispatch as every command resolves it, or null when it is off. Its warnings name file paths,
 * so they are not printed. */
export function resolveDispatch(): DispatchConfig | null {
  try {
    const { dispatch } = sessionDispatchSettings(
      sessionConfigFile("command", process.env),
      process.env,
      { cwd: process.cwd() }
    );
    return dispatch.enabled ? dispatch : null;
  } catch {
    return null;
  }
}

/** The `/healthz` read every check makes, whatever else its own reads are. */
interface HealthRead {
  /** `GET /healthz`, whose body is read on 200 and on 503. */
  healthz(): Promise<unknown>;
}

/** The running build, which is no assertion of a check's own: whether its commit carries a given
 * merge is decided outside these scripts. */
export async function checkBuild(
  dispatch: HealthRead,
  out: (line: string) => void
): Promise<Verdict> {
  let health: Health;
  try {
    health = Health.parse(await dispatch.healthz());
  } catch (error) {
    out(`build unavailable: ${errorCategory(error)}`);
    return "unavailable";
  }
  const schema = health.schema_version ?? "null";
  out(`build commit=${health.commit ?? "null"} schema=${schema} ok=${health.ok}`);
  return "pass";
}

/** Any unavailable check makes the run unavailable; else any failed check fails it. */
export function overallVerdict(verdicts: readonly Verdict[]): Verdict {
  if (verdicts.includes("unavailable")) return "unavailable";
  if (verdicts.includes("fail")) return "fail";
  return "pass";
}

/** What a check's `main` has once the command line holds: the options the check's own flags
 * parsed to, and the Dispatch its calls go to. It is not ready when the opening printed its line
 * instead, and then the status it answers is all that is left of the run. */
export type Prelude<Options> =
  | { readonly ready: true; readonly options: Options; readonly config: DispatchConfig }
  | { readonly ready: false; readonly code: number };

/**
 * The opening every check's `main` makes: the arguments, read by the check's own `parse`, and
 * Dispatch. A command line the check refuses prints that check's own usage text, and a Dispatch
 * that is off prints the word that check spells for it. Both go to stderr and answer 2, since a
 * check that could not be run at all is unavailable.
 */
export function readCommandLine<Options>(
  argv: readonly string[],
  parse: (argv: readonly string[]) => Options,
  unavailable: Category
): Prelude<Options> {
  let options: Options;
  try {
    options = parse(argv);
  } catch (error) {
    if (!(error instanceof UsageError)) throw error;
    console.error(`usage: ${error.message}`);
    return { ready: false, code: 2 };
  }
  const config = resolveDispatch();
  if (config === null) {
    console.error(`unavailable: ${unavailable}`);
    return { ready: false, code: 2 };
  }
  return { ready: true, options, config };
}

/** The ending every check gives its process: its `main` over the arguments the process was run
 * with, and the status that answered. An error that reached here is reported as its category
 * alone, as every other failure of a check is. */
export async function runScript(
  main: (argv: readonly string[]) => Promise<number>
): Promise<never> {
  let code: number;
  try {
    code = await main(Bun.argv.slice(2));
  } catch (error) {
    console.error(`unavailable: ${errorCategory(error)}`);
    code = 2;
  }
  process.exit(code);
}
