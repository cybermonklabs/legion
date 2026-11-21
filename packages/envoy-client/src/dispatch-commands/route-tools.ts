import type { Actor, DispatchToolRoute } from "@legion/contracts";
import type { DispatchClient } from "../dispatch-http";
import type { CommandContext, DispatchToolResult, ToolArguments } from "./context";

/** The value of an argument a call names, or undefined for one it leaves out or names as "". */
function named(args: ToolArguments, name: string): unknown {
  const value = args[name];
  return value === undefined || value === "" ? undefined : value;
}

/**
 * The route of `routes` a call takes: the first whose every placeholder an argument of the call
 * fills, the order the Dispatch server chooses a row in. Undefined where none is filled.
 */
export function boundRoute(
  routes: readonly DispatchToolRoute[],
  args: ToolArguments
): DispatchToolRoute | undefined {
  return routes.find((route) =>
    Object.values(route.params ?? {}).every((argument) => named(args, argument) !== undefined)
  );
}

/**
 * Calls the route of `routes` the arguments fill, as the route binds them: each placeholder of
 * its path filled by its argument, each query argument the call names sent as a query parameter
 * under its own name, and, for every method but GET, a JSON body of each body argument the call
 * names, under its own name, with the session's actor beside them. The answer is the route's JSON
 * body, printed compact.
 */
export async function callBoundRoute(
  client: DispatchClient,
  routes: readonly DispatchToolRoute[],
  args: ToolArguments,
  actor: Actor
): Promise<DispatchToolResult> {
  const route = boundRoute(routes, args);
  if (route === undefined) {
    const wanted = routes
      .map((each) => Object.values(each.params ?? {}).join(" and "))
      .join(", or ");
    throw new Error(`these arguments fill no route of this command: name ${wanted}`);
  }
  const params = route.params ?? {};
  const path = route.path
    .split("/")
    .filter((segment) => segment !== "")
    .map((segment) => {
      const placeholder = /^\{(.+)\}$/.exec(segment)?.[1];
      return placeholder === undefined ? segment : String(named(args, params[placeholder] ?? ""));
    });
  const query: Record<string, unknown> = {};
  for (const name of route.query ?? []) {
    const value = named(args, name);
    if (value !== undefined) query[name] = value;
  }
  let body: Record<string, unknown> | undefined;
  if (route.method !== "GET") {
    body = {};
    for (const name of route.body ?? []) {
      if (args[name] !== undefined) body[name] = args[name];
    }
    body["actor"] = actor;
  }
  const answer = await client.callRoute(route.method, path, { body, query });
  return {
    text: answer === undefined ? "" : JSON.stringify(answer),
    details: { route: `${route.method} ${route.path}`, answer },
  };
}

/** The command of a spec that binds its routes: its route called as `callBoundRoute` calls it. */
export function routeCommand(
  routes: readonly DispatchToolRoute[]
): (context: CommandContext) => Promise<DispatchToolResult> {
  return (context) => callBoundRoute(context.client, routes, context.args, context.actor);
}
