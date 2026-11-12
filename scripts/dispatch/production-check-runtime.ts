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
 * the exit code that verdict takes, the fixed category an error is reported as, the `/healthz`
 * body each reads, the reads each makes of its own, and the Dispatch every command resolves. A
 * check's own flags, usage texts, schemas, steps and printed lines stay in the check itself. */

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
