// Which routes the Dispatch of a label refuses the world's shared agent token for, and the label's
// agents call: every route its table of the OAuth scopes each route needs (routes_table.go's
// `routeScopes`) gives the `person` scope, and that its agents' client of Dispatch
// (envoy-client's dispatch-http.ts) sends a request to. The shared token holds no scope, so
// Dispatch answers each such request 403 OAUTH_SCOPE_REQUIRED, and production gives a Legion
// pod's agents, which run under its shared token, the same 403. Rule 16 of health.ts declares that
// 403 for every agent of every run (the architect's ruling of 2026-11-21: the class, not a list).
// Both sources are read off the label's own source by marker, as daemon-pane.ts's `labelShapes`
// reads the daemon's, so a person-scoped route main adds is declared at the label that has it. A
// marker this cannot find refuses, naming the file and the marker, and is never read as a label
// whose Dispatch refuses nothing. Their paths live in daemon-pane.ts (SCOPES_GO, CLIENT_TS), whose
// LABEL_SOURCES holds them, so that a boot, which loads daemon-pane.ts, does not load this module.
import { existsSync, readFileSync } from "node:fs";
import path from "node:path";
import { CLIENT_TS, SCOPES_GO } from "./daemon-pane";

/** The sources `labelPersonRoutes` reads, under a checkout's root: daemon-pane.ts's
 * `recordedCheckout` rebuilds them out of a run's commit beside the daemon's. */
export const PERSON_ROUTE_SOURCES = [SCOPES_GO, CLIENT_TS];

/** The line that opens `routeScopes`, the line alone that closes it, and one of its rows:
 * `"<METHOD> <pattern>": {<scope>, …},`, as gofmt writes it. */
const SCOPES_OPEN = "var routeScopes = map[string]routeScopeSet{";
const SCOPES_CLOSE = "}";
const SCOPES_ROW = /^\t"([A-Z]+) (\/\S*)":\s+\{([^{}]*)\},$/;
/** The scope a credential acts as its person by (model.go's `ScopePerson`, "person"), which the
 * shared agent token does not hold. */
const PERSON_SCOPE = "model.ScopePerson";

/** How the client spells a route's path: an array of its segments, `["api", "v1", …]`, a string
 * literal for a fixed segment and any other expression for one of the caller's values. */
const CLIENT_PATH = /\[\s*"api",\s*"v1"\s*[,\]]/g;
/** The calls a path is sent by, read off the text before the path: `this.#json`,
 * `this.#jsonAnswer` or `this.#form` with the method before it; `this.#upload`, which posts it;
 * and a `const` the path is bound to, directly, as either arm of a `? :`, or as the URL
 * `this.#url` makes of it, which a later call of the same method of the client sends. */
const SENT = /this\.#(?:json|jsonAnswer|form)(?:<[^()]*>)?\(\s*"([A-Z]+)",\s*$/;
const UPLOADED = /this\.#upload(?:<[^()]*>)?\(\s*$/;
const BOUND = /\bconst (\w+) = (?:this\.#url\(|(?:[^;]*[?:] )?)$/;
/** The end of a method of the client: its closing brace, alone on its line at the class's
 * indentation. */
const METHOD_END = /\n {2}\}\n/;

/** A request the client sends: its method and its path's segments after the leading `/`, each
 * the segment or null for one of the caller's values. */
interface Sent {
  readonly method: string;
  readonly segments: readonly (string | null)[];
}

/** Each person-scoped route of the label's Dispatch that its agents' client sends, kept for the
 * process by the checkout it was read in. */
const read = new Map<string, readonly string[] | Error>();

/** The text of `file`, under `checkout`, or a refusal naming it. */
function sourceOf(checkout: string, file: string, why: string): string {
  const at = path.join(checkout, file);
  if (!existsSync(at)) throw new Error(`${file} is not there, which ${why}`);
  return readFileSync(at, "utf8");
}

/** Every route `routeScopes` gives the person scope, `<METHOD> <pattern>` each, in the table's
 * order. Throws, naming the file and the marker, for a source that holds no such table, no line
 * closing it, a line in it that is neither a row, a comment nor blank, or no row of that scope. */
function personScoped(checkout: string): string[] {
  const text = sourceOf(checkout, SCOPES_GO, "holds the scopes each route needs (routeScopes)");
  const lines = text.split("\n");
  const open = lines.indexOf(SCOPES_OPEN);
  if (open < 0)
    throw new Error(
      `${SCOPES_GO} holds no ${JSON.stringify(SCOPES_OPEN)}, the line that opens the table of the scopes each route needs`
    );
  const close = lines.indexOf(SCOPES_CLOSE, open + 1);
  if (close < 0)
    throw new Error(
      `${SCOPES_GO}:${open + 1} opens routeScopes, and no ${JSON.stringify(SCOPES_CLOSE)} line closes it`
    );
  const routes: string[] = [];
  for (const [index, line] of lines.slice(open + 1, close).entries()) {
    const trimmed = line.trim();
    if (trimmed === "" || trimmed.startsWith("//")) continue;
    const [, method, pattern, scopes = ""] = SCOPES_ROW.exec(line) ?? [];
    if (method === undefined || pattern === undefined)
      throw new Error(
        `${SCOPES_GO}:${open + index + 2} is no row of routeScopes (\`"<METHOD> <pattern>": {<scope>, …},\`): ${trimmed}`
      );
    if (scopes.split(",").some((scope) => scope.trim() === PERSON_SCOPE))
      routes.push(`${method} ${pattern}`);
  }
  if (routes.length === 0)
    throw new Error(`${SCOPES_GO}'s routeScopes gives no route ${PERSON_SCOPE}, the person scope`);
  return routes;
}

/** The path the array literal opening at `open` in `text` spells, and where it ends; undefined
 * for one that does not close. Strings are skipped whole, and a comma inside a nested call is no
 * segment's end. */
function pathAt(
  text: string,
  open: number
): { readonly end: number; readonly segments: (string | null)[] } | undefined {
  const parts: string[] = [];
  let depth = 0;
  let start = open + 1;
  for (let at = open; at < text.length; at += 1) {
    const char = text[at];
    if (char === '"' || char === "'" || char === "`") {
      at += 1;
      while (at < text.length && text[at] !== char) at += text[at] === "\\" ? 2 : 1;
    } else if (char === "[" || char === "(" || char === "{") {
      depth += 1;
    } else if (char === "]" || char === ")" || char === "}") {
      depth -= 1;
      if (depth === 0) {
        parts.push(text.slice(start, at));
        const segments = parts
          .map((part) => part.trim())
          .filter((part) => part !== "")
          .map((part) => /^"([^"\\]*)"$/.exec(part)?.[1] ?? null);
        return { end: at + 1, segments };
      }
    } else if (char === "," && depth === 1) {
      parts.push(text.slice(start, at));
      start = at + 1;
    }
  }
  return undefined;
}

/** Every request the client sends, one for each path it spells and the method of the call that
 * sends it. Throws, naming the file and the marker, for a client that spells no path so, and for a
 * path no call this reads sends. */
function clientRequests(checkout: string): Sent[] {
  const text = sourceOf(checkout, CLIENT_TS, "sends the agents' requests to Dispatch");
  const sent: Sent[] = [];
  for (const found of text.matchAll(CLIENT_PATH)) {
    const open = found.index ?? 0;
    const line = text.slice(0, open).split("\n").length;
    const spelled = pathAt(text, open);
    if (spelled === undefined)
      throw new Error(`${CLIENT_TS}:${line} opens a route path that no "]" closes`);
    const before = text.slice(Math.max(0, open - 400), open);
    const bound = BOUND.exec(before)?.[1];
    let method = SENT.exec(before)?.[1] ?? (UPLOADED.test(before) ? "POST" : undefined);
    if (method === undefined && bound !== undefined) {
      const rest = text.slice(spelled.end);
      const body = rest.slice(0, METHOD_END.exec(rest)?.index ?? rest.length);
      const sends = new RegExp(
        `this\\.#(?:json|jsonAnswer|form|send)(?:<[^()]*>)?\\(\\s*"([A-Z]+)",\\s*${bound}\\b`
      );
      method = sends.exec(body)?.[1];
    }
    if (method === undefined)
      throw new Error(
        `${CLIENT_TS}:${line} spells the route path ${text.slice(open, spelled.end).replace(/\s+/g, " ")}, which no call this reads sends: this.#json, this.#jsonAnswer or this.#form given "<METHOD>" and it, this.#upload given it, or a const it is bound to that one of them or this.#send sends later in the same method`
      );
    sent.push({ method, segments: spelled.segments });
  }
  if (sent.length === 0)
    throw new Error(
      `${CLIENT_TS} spells no route path ["api", "v1", …], which is how it names each route it sends`
    );
  return sent;
}

/** Whether a request of `sent` reaches `route`, `<METHOD> <pattern>`: the same method, and each
 * segment of the pattern the client's own, a `{name}` the pattern leaves to any value, or a value
 * of the caller's, which may be any. */
function reaches(route: string, sent: readonly Sent[]): boolean {
  const [method, pattern = ""] = route.split(" ");
  const segments = pattern.split("/").slice(1);
  return sent.some((one) => {
    if (one.method !== method || one.segments.length !== segments.length) return false;
    return segments.every((segment, at) => {
      const value = one.segments[at];
      return value === null || value === segment || /^\{\w+\}$/.test(segment);
    });
  });
}

/** Every route the Dispatch of `checkout` gives the person scope (`routeScopes`) and its agents'
 * client sends a request to, `<METHOD> <pattern>` each, with `{name}` for a segment of any value,
 * in the table's order. Kept for the process by checkout. Throws, naming the file and the marker,
 * for a source of no shape this reads. */
export function labelPersonRoutes(checkout: string): readonly string[] {
  let known = read.get(checkout);
  if (known === undefined) {
    try {
      const sent = clientRequests(checkout);
      known = personScoped(checkout).filter((route) => reaches(route, sent));
    } catch (error) {
      known = new Error(
        `the Dispatch sources of ${checkout} are of no shape its person-scoped routes are read by: ${error instanceof Error ? error.message : String(error)}`
      );
    }
    read.set(checkout, known);
  }
  if (known instanceof Error) throw known;
  return known;
}
