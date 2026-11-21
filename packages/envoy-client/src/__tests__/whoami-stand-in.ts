import type { WhoamiAgent } from "@legion/contracts";
import { type DispatchToolName, dispatchCommands } from "../dispatch-commands/table";
import {
  type DispatchToolResult,
  type ExecuteDispatchToolInput,
  executeDispatchTool as executeTool,
} from "../dispatch-execute";

// A read command (`reads` in the command table: read, doc-read, search, results, answers,
// open-asks and inbox) reads `GET /api/v1/whoami` before any other request, since a token that
// carries a cut reads Dispatch at that cut (dispatch://INFRA-4620). A stand-in written for
// another part of a read answers whoami through this: a token that carries no cut, so the
// command reads the present, as it did before. The calls of a read under a cut are
// `dispatch-as-of-calls.test.ts`'s.

/** Whoami of a session's token that carries no cut. */
export const WHOAMI_NO_CUT = {
  kind: "agent",
  owner: "person@example.com",
  service: null,
  session: "ses-1",
  project: null,
  scopes: ["person"],
  cut: null,
} satisfies WhoamiAgent;

/** Whether a request is `GET /api/v1/whoami`. */
export function asksWhoami(pathname: string): boolean {
  return pathname.endsWith("/api/v1/whoami");
}

/** `fetchImpl` with `GET /api/v1/whoami` answered as `WHOAMI_NO_CUT`, before it is asked: every
 *  other request goes to `fetchImpl` as it came, so what it records is unchanged. */
function answeringWhoami(fetchImpl: typeof fetch): typeof fetch {
  return Object.assign(
    async (input: Parameters<typeof fetch>[0], init?: RequestInit): Promise<Response> => {
      const url = new URL(input instanceof Request ? input.url : String(input));
      if (asksWhoami(url.pathname)) return Response.json(WHOAMI_NO_CUT);
      return fetchImpl(input, init);
    },
    { preconnect: fetchImpl.preconnect }
  );
}

/** `input` with whoami answered where its tool is a read command, and as it is otherwise. */
function withWhoamiAnswered(input: ExecuteDispatchToolInput): ExecuteDispatchToolInput {
  const command = dispatchCommands[input.tool as DispatchToolName];
  return command?.reads === true
    ? { ...input, fetchImpl: answeringWhoami(input.fetchImpl ?? fetch) }
    : input;
}

/** The executor, with whoami answered for a read command (`withWhoamiAnswered`): a test's
 *  stand-in answers the reads the test is about. */
export function executeDispatchTool(input: ExecuteDispatchToolInput): Promise<DispatchToolResult> {
  return executeTool(withWhoamiAnswered(input));
}
