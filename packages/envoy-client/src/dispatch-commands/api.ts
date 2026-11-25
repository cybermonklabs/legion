import { type DispatchToolResult, type SessionContext, stringArg } from "./context";

/**
 * `dispatch api`: one GET of a route under `/api/v1` that no named command reads, sent through
 * the client every command uses, so it presents this session's own credential (the broker token
 * under `DISPATCH_TOKEN_BROKER=1`, else the bearer), and Dispatch's JSON answer printed for `jq`
 * to read. The contract's rule (`dispatch_api` in `@legion/contracts`) refused a path that could
 * leave `/api/v1` before this runs. A refusal is Dispatch's own error, thrown as the client throws
 * it, which the command prints and exits 1 on. It names no owner, so it runs on the session's
 * context alone, and it writes nothing: a write goes through the command named for it.
 */
export async function apiCommand(context: SessionContext): Promise<DispatchToolResult> {
  const path = stringArg(context.args, "path");
  const answer = await context.newClient().readRoute(path);
  return {
    text: typeof answer === "string" ? answer : JSON.stringify(answer, null, 2),
    details: { path },
  };
}
