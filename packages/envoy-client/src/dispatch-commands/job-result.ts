// packages/envoy-client/src/dispatch-commands/job-result.ts
//
// `dispatch result` as a CI job writes it, `--project` or `--issue`: the one call of the
// `dispatch` command that runs as no session. `runDispatchCli` (dispatch-cli.ts) parses it and
// runs the local checks of its text, as for every command, and then hands it here
// (`isJobResult`, `jobResultCommand`).

import { sleep } from "@legion/contracts";
import { AIRLOCK_RESULTS_VAR } from "../airlock-credential";
import type { ParsedCommand } from "../dispatch-command";
import { ownIdentityDispatch } from "../dispatch-config";
import { checkDispatchCall, dispatchServerUrls } from "../dispatch-execute";
import {
  AirlockRefusedError,
  AirlockTunnelRefusedError,
  DISPATCH_TOOL_DEADLINE_MS,
  DISPATCH_WRITE_RETRY,
  DispatchClient,
  DispatchWriteLandedError,
  DispatchWriteUnsettledError,
  tokenNotChecked,
  writeOutcomeUnknown,
} from "../dispatch-http";
import { messageFor } from "../errors";
import { printable, printableJson, printableLine } from "../runner-log";
import { ToolInputError } from "../tool-input-errors";
import { checkCIJobResultCall, ciJobResultCommand } from "./ci-job-result";
import { OK, REFUSED, USAGE } from "./exit-codes";
import { specCheckResultCommand } from "./issues";

/** What `jobResultCommand` reads of the command's `DispatchCliIo` (dispatch-cli.ts). */
export interface JobResultIo {
  readonly cwd: string;
  readonly fetchImpl?: typeof fetch | undefined;
  readonly resultBound?: ResultBound | undefined;
}

/**
 * Whether `argv` is a CI job's `dispatch result` (`jobResultCommand`): the command `result` with
 * a `--project` flag, as `--project <KEY>` or `--project=<KEY>`, or any `result` call whose
 * environment holds the proxy placeholder in DISPATCH_RESULTS_TOKEN (`AIRLOCK_RESULTS_VAR`), set
 * and not empty, which only `agent-secrets result-grant` gives, and so only a CI job's step: its
 * `--issue` form records the job's result of the check its ci_check names. `env` is the
 * environment `runDispatchCli` reads, which holds no empty variable, so an empty placeholder is
 * no placeholder here. A session never holds that variable, so its `dispatch result --issue`
 * stays a role's record, AGENT_SECRETS_MACHINE_IDENTITY=github-actions or not. The parser reads a
 * word that starts with `--` as a flag and never as a value, so this is the call the parser
 * reads, and it is known before the parse, for an argv the parser refuses too.
 */
export function isJobResult(
  argv: readonly string[],
  env: Readonly<Record<string, string>>
): boolean {
  const [command, ...words] = argv;
  return (
    command === "result" &&
    (env[AIRLOCK_RESULTS_VAR] !== undefined ||
      words.some((word) => word === "--project" || word.startsWith("--project=")))
  );
}

/** The code `{"write_outcome_unknown": <code>}` names: that of the write's last failure, the last
 *  answer of a DispatchWriteUnsettledError: a transport error's code such as ECONNRESET, or a
 *  refusal's such as HTTP_502, else that error's name, such as TimeoutError. */
function outcomeCode(error: unknown): string {
  const last: unknown = error instanceof DispatchWriteUnsettledError ? error.last : error;
  const code = typeof last === "object" && last !== null ? Reflect.get(last, "code") : undefined;
  if (typeof code === "string" && code !== "") return code;
  return last instanceof Error ? last.name : String(last);
}

/**
 * `dispatch result` as a CI job writes it, in its two forms: `--project`, its result of a check
 * of the spec files, and `--issue`, its result of the check of an issue's Verification document
 * that the check's ci_check gives to the job (`ciJobResultCommand`). The job runs it under
 * `agent-secrets result-grant`, and writes with the proxy placeholder of its run alone
 * (`ownIdentityDispatch`): no host, no session, no envoy.json, no AskGate and no listener, since a
 * job has none of them, no bearer or person's sign-in, since the result must carry the identity
 * the airlock authenticated, and no trade, since the broker holds the run's token. The call's own
 * argument check runs first, as for every command (`checkCIJobResultCall` for `--issue`, which
 * takes the job's flags alone), so a refused call sends nothing. It prints the stored result as
 * JSON and exits 0, or prints Dispatch's refusal as
 * `<CODE>: <message>` and exits 1. A write that landed with its answer lost prints
 * `{"landed_without_answer": <code>}`, the details the session path records for the same case,
 * and exits 0: the result is stored, and a results token reads nothing back. A write that may
 * have reached Dispatch while no answer says whether it stored the result (`writeOutcomeUnknown`)
 * prints `{"write_outcome_unknown": <code>}` and exits 1, as the write is not confirmed: the
 * result may be stored, and a second run is safe, since Dispatch keeps each result. A tunnel the
 * airlock proxy refused prints `AIRLOCK_TUNNEL_REFUSED: …` with its status, and an answer the proxy
 * gave itself before the write left it (`Cipher-Proxy-Forwarded: false`) prints its
 * `Cipher-Proxy-Code` and its words as the client's excerpt quotes them (`AirlockRefusedError`),
 * each saying that nothing reached Dispatch, and exits 1; a 5xx of either is asked again within
 * the bound first, as a connection that never opened is. An answer the proxy gave itself after the
 * write may have left it (`Cipher-Proxy-Forwarded: unknown`) is asked again under the same key, as
 * a reset is, and ends `{"write_outcome_unknown": <code>}` with exit 1 if no answer settles it. A
 * call Dispatch's check of the run's token turns away before anything is stored (`notNow`) is
 * asked again inside the call's one bound (`ResultBound`), and ends with the last such answer, its
 * code named, and exit 1. No credential reaches what it prints: the proxy's and Dispatch's words
 * name none, and the placeholder is named by its variable.
 *
 * What it prints goes into a GitHub Actions runner's log, which reads workflow commands from it,
 * so each text it did not write (an argument, a variable, an answer of the airlock proxy or of
 * Dispatch) is printed by runnerlog.Printable's rule (runner-log.ts): a problem of its arguments
 * as `printable` text, Dispatch's and the proxy's refusals as a `printableLine`, and the dry run,
 * the stored result and the outcome line as `printableJson`. No value starts a line of its own,
 * and no line is a workflow command. The landed line holds no such text: its code is one of the
 * client's three constants.
 */
export async function jobResultCommand(
  parsed: Extract<ParsedCommand, { readonly kind: "call" }>,
  env: Readonly<Record<string, string>>,
  io: JobResultIo,
  print: (text: string) => void,
  refuse: (text: string, code: number, tool?: string) => number
): Promise<number> {
  const resolution = ownIdentityDispatch(env);
  const projectForm = parsed.args.project !== undefined;
  const check = {
    tool: parsed.tool,
    args: parsed.args,
    cwd: io.cwd,
    env,
    serverUrls: resolution.enabled ? dispatchServerUrls(resolution) : [],
  };
  const checked = projectForm ? await checkDispatchCall(check) : await checkCIJobResultCall(check);
  if (checked.problems.length > 0) {
    return refuse(
      new ToolInputError(parsed.tool, checked.problems.map(printable)).message,
      USAGE,
      parsed.tool
    );
  }
  if (parsed.dryRun) {
    print(printableJson(JSON.stringify(parsed.args, null, 2)));
    return OK;
  }
  if (!resolution.enabled) {
    return refuse(
      `dispatch: ${printable(resolution.error ?? resolution.notice)}`,
      USAGE,
      parsed.tool
    );
  }
  // One bound, one signal (`ResultBound`): the timeout made here is the write client's deadline,
  // and every wait below ends at it too. The client's asks again under the key stop `reserveMs`
  // before it, and the rounds below are read on the same monotonic clock the client keeps its asks
  // on.
  const bound = io.resultBound ?? RESULT_BOUND;
  const ended = AbortSignal.timeout(bound.totalMs);
  const lastRound = performance.now() + bound.totalMs - bound.reserveMs;
  const client = new DispatchClient(resolution.url, resolution.credential, io.fetchImpl ?? fetch, {
    signal: ended,
    deadlineMs: bound.totalMs,
    writeRetry: {
      ...DISPATCH_WRITE_RETRY,
      withinMs: Math.min(DISPATCH_WRITE_RETRY.withinMs, bound.totalMs - bound.reserveMs),
      inFlightWithinMs: bound.totalMs - bound.reserveMs,
    },
  });
  for (;;) {
    try {
      const written = projectForm
        ? await specCheckResultCommand(client, checked.args)
        : await ciJobResultCommand(client, checked.args);
      print(printableJson(written.text));
      return OK;
    } catch (error) {
      // A write that landed with its answer lost is done. A program reads the job's output, so it
      // is the JSON of the session path's ledger entry, with no advice to read the result back.
      if (error instanceof DispatchWriteLandedError) {
        print(JSON.stringify({ landed_without_answer: error.code }));
        return OK;
      }
      // Only an attempt that may have reached a handler leaves the outcome unknown; an answer of
      // Dispatch's token check came before any handler ran (`tokenNotChecked`), and is below.
      if (writeOutcomeUnknown(error)) {
        return refuse(
          printableJson(JSON.stringify({ write_outcome_unknown: outcomeCode(error) })),
          REFUSED,
          parsed.tool
        );
      }
      // The airlock proxy refused the tunnel, or answered the request itself: nothing reached
      // Dispatch, and its line says so. Its words come from outside this command.
      if (error instanceof AirlockTunnelRefusedError) {
        return refuse(printableLine(`${error.code}: ${error.message}`), REFUSED, parsed.tool);
      }
      if (error instanceof AirlockRefusedError) {
        return refuse(
          printableLine(`${error.code}: ${error.body}; nothing was stored`),
          REFUSED,
          parsed.tool
        );
      }
      // One job writes a result for each of its checks, so it meets Dispatch's limit of one
      // address with no fault of its own: a call turned away before anything was stored is sent
      // again after the answer's wait, while the next round starts before the bound's last
      // `reserveMs`. Past that, it ends with the last answer, which names its code.
      const turnedAway = notNow(error);
      if (turnedAway !== undefined) {
        if (
          performance.now() + turnedAway.waitMs <= lastRound &&
          (await sleep(turnedAway.waitMs, ended).then(
            () => true,
            () => false
          ))
        ) {
          continue;
        }
        return refuse(
          printableLine(
            `${turnedAway.why}, and the ${bound.totalMs / 1_000} s this command may take leave no time to ask again: ${messageFor(error)}`
          ),
          REFUSED,
          parsed.tool
        );
      }
      return refuse(printableLine(messageFor(error)), REFUSED, parsed.tool);
    }
  }
}

/**
 * Why a call of `jobResultCommand` was turned away before anything was stored, so it may be sent
 * again, and how long to wait first; undefined for every other failure. One answer says so:
 * Dispatch's check of the run's token, which answered before any handler ran (`tokenNotChecked`:
 * its 429 of a spent bucket, its 503 for a broker that answered nothing). The airlock proxy puts
 * that token into the write, and every CI write reaches Dispatch from a broker pod's address, so
 * the jobs that write at one time share one bucket. An answer of the write itself is never one: a
 * refusal is final, and a write that may have stored is `writeOutcomeUnknown`'s.
 */
function notNow(error: unknown): { readonly why: string; readonly waitMs: number } | undefined {
  if (tokenNotChecked(error)) {
    return {
      why: "Dispatch's check of the job's token turned the write away",
      waitMs: error.retryAfterMs ?? NOT_NOW_STEP_MS,
    };
  }
  return undefined;
}

/** The wait `jobResultCommand` takes where an answer that says "not now" names none. */
const NOT_NOW_STEP_MS = 1_000;

/** The time a last attempt's answer gets beyond the slowest answer Dispatch gives before a
 *  handler runs: the round trip, and Dispatch's own work before the check. */
const RESERVE_MARGIN_MS = 2_000;

/** How long Dispatch's check of a broker token waits for the broker before it answers
 *  `503 BROKER_UNAVAILABLE`, with no handler run: `introspectTimeout`,
 *  packages/envoy/internal/ciphertoken/introspect.go, which a test holds this copy to. */
export const DISPATCH_TOKEN_CHECK_WAIT_MS = 5_000;
/**
 * 60 s in all, the write client's own deadline (DISPATCH_TOOL_DEADLINE_MS). The reserve must be
 * greater than the slowest answer Dispatch gives before any handler runs, its check of the token
 * waiting on the broker (`DISPATCH_TOKEN_CHECK_WAIT_MS`): a last ask sent at the edge of a
 * reserve no longer than that wait can be answered "not checked" only after the bound, which then
 * ends it as an attempt that may have reached a handler, though none ran. So it is that wait and
 * a margin, 7 s.
 */
const RESULT_BOUND: ResultBound = {
  totalMs: DISPATCH_TOOL_DEADLINE_MS,
  reserveMs: DISPATCH_TOKEN_CHECK_WAIT_MS + RESERVE_MARGIN_MS,
};

/**
 * The one bound of a CI job's `dispatch result`, `--project` or `--issue` (`jobResultCommand`).
 * The whole call ends `totalMs` after it starts: one timeout, made where the call starts, is the
 * signal every await of it runs under — each write with the asks the write client makes again
 * under its key, and each wait after an answer that says "not now" (`notNow`) — since the write
 * client takes that signal as its deadline. No round starts, and the write client asks nothing
 * again, in its last `reserveMs`, the time a last attempt has to be answered before the bound
 * ends it. A job's step of many checks still ends, and the wait of one call stays far below a
 * job's timeout.
 */
export interface ResultBound {
  readonly totalMs: number;
  readonly reserveMs: number;
}
