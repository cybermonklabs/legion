import type { WritingRulesSettings } from "@legion/contracts";
import { commandName } from "./dispatch-command";
import {
  type DispatchClient,
  DispatchNoRouteError,
  DispatchWriteHeld,
  isNoRoute,
} from "./dispatch-http";
import {
  readWritingRules,
  updateWritingRules,
  type WritingRulesSettingsRead,
} from "./dispatch-session-state";
import { messageFor } from "./errors";

/**
 * The refusal that shows the written rules (INFRA-3231, R6). Before a write of an ask, an issue,
 * a comment or a message, the `dispatch` command refuses, once, a session's first such write and
 * its first such write after each compaction of the session's context. The refusal prints the
 * rules for the person who reads that write, from `POST /api/v1/writing-rules`, and tells the
 * agent to check its text against them and run the command again; the next call sends the write.
 * The server says whether the refusal is on (`GET /api/v1/settings/writing-rules`), and a server
 * without that route has no refusal. The host plugin counts each compaction in the session's
 * state file (`noteCompaction`), which this reads. A person's own typed command is never refused
 * (`personTypedCall`).
 */

/** The commands whose write the refusal comes before: the kinds of text the server's reviewer may
 *  hold, each written to a person. Read with `=== true`, so a name on Object's prototype
 *  (`constructor`) is no such command. */
export const WRITING_RULES_COMMANDS: Readonly<Record<string, true>> = {
  dispatch_ask: true,
  dispatch_issue: true,
  dispatch_comment: true,
  dispatch_message: true,
};

/** How long the command uses Dispatch's answer about the refusal before it asks again. */
export const WRITING_RULES_SETTINGS_MS = 10 * 60_000;

/**
 * Whether this call runs a person's own typed line: `DISPATCH_PERSON_TYPED=1`, which pi-envoy's
 * typed shell puts before a `!dispatch …` line a person typed (INFRA-3231, Decision 26). The
 * command reads it once, when it starts, and refuses no write of such a call; it gives the value
 * to the `DispatchClient` it makes, which sends `Dispatch-Person-Typed: 1` on each write.
 *
 * It takes the variable as it reads it: it deletes it from `env`, the command's own copy of its
 * environment, and from `process.env`, before the command starts any child process (jj, git, ps,
 * a configured `/bin/sh -c` command, agent-secrets), so no program the command starts, and no
 * agent such a program starts, inherits it.
 */
export function personTypedCall(env: Record<string, string | undefined>): boolean {
  // biome-ignore lint/complexity/useLiteralKeys: claude-envoy's tsconfig refuses dot access
  const typed = env["DISPATCH_PERSON_TYPED"] === "1";
  // biome-ignore lint/complexity/useLiteralKeys: claude-envoy's tsconfig refuses dot access
  delete env["DISPATCH_PERSON_TYPED"];
  // biome-ignore lint/complexity/useLiteralKeys: claude-envoy's tsconfig refuses dot access
  delete process.env["DISPATCH_PERSON_TYPED"];
  return typed;
}

/** What `writingRulesRefusal` reads for one call. */
export interface WritingRulesCall {
  /** The command's tool name, `dispatch_comment`. */
  readonly tool: string;
  /** The session's state directory (`sessionDirectory`). */
  readonly dir: string;
  /** A client of the session's Dispatch for the rules (`POST /api/v1/writing-rules`), which asks
   *  again a route a rolling release has not brought yet, as every request does; it must send its
   *  writes. */
  readonly client: DispatchClient;
  /** A client of the same Dispatch for the settings read (`GET /api/v1/settings/writing-rules`),
   *  built with `retryNoRoute: false`: a Dispatch older than that route has no refusal, so its
   *  first answer of no route decides, with none of the waits a rolling release asks for. */
  readonly settingsClient: DispatchClient;
  /** Whether the call passes its own argument check (`checkDispatchCall`), which sends no
   *  request: a call that fails it goes on to its own refusal, and Dispatch is asked nothing. */
  readonly passesCheck: () => Promise<boolean>;
  /** Runs the call with every write held (`holdWrites`), so that it throws `DispatchWriteHeld`
   *  at its write and sends nothing. */
  readonly heldRun: () => Promise<unknown>;
  /** The time now, in ms since the epoch. */
  readonly now: number;
}

/**
 * The text that refuses this call, or undefined when the call goes on. The call goes on when its
 * command is not one of `WRITING_RULES_COMMANDS`, when the session already saw the rules since its
 * last compaction, when its arguments fail its own check, when Dispatch has the refusal off or has
 * no route for it, and when no answer about the refusal comes, so a Dispatch that cannot be read
 * refuses nothing. The call also goes on when its held run ends in no held write: the call itself
 * then makes no write, or meets the same failure and reports it. The session counts as shown the
 * rules only once that is written in its state file, so a state file that cannot be written
 * refuses nothing, rather than every call. A state file that is not JSON throws, naming it.
 */
export async function writingRulesRefusal(call: WritingRulesCall): Promise<string | undefined> {
  if (WRITING_RULES_COMMANDS[call.tool] !== true) return undefined;
  const state = readWritingRules(call.dir);
  if (state.shownForCompaction === state.compactions) return undefined;
  // An answer the state file kept, while it is younger than WRITING_RULES_SETTINGS_MS, decides
  // with no request and no check; a new one is asked only for a call whose arguments pass.
  const kept = state.settings;
  const age = kept === undefined ? Number.NaN : call.now - Date.parse(kept.readAt);
  const fresh =
    kept !== undefined && age >= 0 && age < WRITING_RULES_SETTINGS_MS ? kept : undefined;
  if (fresh?.refusal === false) return undefined;
  if (!(await call.passesCheck())) return undefined;
  const settings = fresh ?? (await askedSettings(call));
  if (settings?.refusal !== true) return undefined;
  let held: DispatchWriteHeld;
  try {
    await call.heldRun();
    return undefined;
  } catch (error) {
    if (!(error instanceof DispatchWriteHeld)) return undefined;
    held = error;
  }
  let rules: string;
  try {
    rules = (
      await call.client.writingRules({
        route: `${held.method} ${new URL(held.url).pathname}`,
        body: jsonObject(held.body),
      })
    ).text;
  } catch (error) {
    if (isNoRoute(error) || error instanceof DispatchNoRouteError) return undefined;
    rules = `Dispatch did not send the rules for this write: ${messageFor(error)}. Read skill://plain-words, which holds the same rules.`;
  }
  try {
    updateWritingRules(call.dir, (now) => ({ ...now, shownForCompaction: state.compactions }));
  } catch {
    return undefined;
  }
  return refusalText(call.tool, rules);
}

/** Dispatch's answer about the refusal, asked now, which the state file then keeps; undefined
 *  when no answer comes. */
async function askedSettings(
  call: WritingRulesCall
): Promise<WritingRulesSettingsRead | undefined> {
  let answer: WritingRulesSettings | null;
  try {
    answer = await call.settingsClient.writingRulesSettings();
  } catch {
    return undefined;
  }
  const read: WritingRulesSettingsRead = {
    readAt: new Date(call.now).toISOString(),
    refusal: answer?.refusal === "on",
  };
  try {
    updateWritingRules(call.dir, (now) => ({ ...now, settings: read }));
  } catch {
    // Not kept: the next call asks Dispatch again.
  }
  return read;
}

/** The JSON object `text` holds, or an empty object for text that is not one (a held upload's
 *  form), whose route alone then decides the reader. */
function jsonObject(text: string | null): Record<string, unknown> {
  if (text === null) return {};
  try {
    const value: unknown = JSON.parse(text);
    return typeof value === "object" && value !== null && !Array.isArray(value)
      ? (value as Record<string, unknown>)
      : {};
  } catch {
    return {};
  }
}

/** The refusal of `tool`'s call: what to do, then the rules. Its first line is the line every
 *  refusal of a call before it reaches Dispatch starts with (`cliRefusals`). */
function refusalText(tool: string, rules: string): string {
  return [
    `dispatch ${commandName(tool)} was not called: this call is the first write of this session, or the first write after a compaction of the session.`,
    "Before that write, read the rules for text that a person reads. The rules are below. Then do these steps:",
    "1. Compare each sentence of your text with the rules.",
    "2. Change each sentence that does not obey a rule.",
    "3. Run the command again with the changed text. The next call sends the write.",
    "",
    rules.trimEnd(),
  ].join("\n");
}
