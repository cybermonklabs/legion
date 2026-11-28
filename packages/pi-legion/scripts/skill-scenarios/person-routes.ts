// Which routes the Dispatch of a label refuses the world's shared agent token for, and the label's
// agents call: every route its table of the OAuth scopes each route needs (routes_table.go's
// `routeScopes`, or, since main's #5409 moved each route into a file of its own, each route's
// declared `OAuthScopes`) gives the `person` scope, and that its agents' client of Dispatch
// (envoy-client's dispatch-http.ts) sends a request to. The shared token holds no scope, so
// Dispatch answers each such request 403 OAUTH_SCOPE_REQUIRED, and production gives a Legion
// pod's agents, which run under its shared token, the same 403. Rule 16 of health.ts declares that
// 403 for every agent of every run (the architect's ruling of 2026-11-21: the class, not a list).
// Both sources are read off the label's own source by marker, as daemon-pane.ts's `labelShapes`
// reads the daemon's, so a person-scoped route main adds is declared at the label that has it. A
// marker this cannot find refuses, naming the file and the marker, and is never read as a label
// whose Dispatch refuses nothing. Their paths live in daemon-pane.ts (SCOPES_GO, ROUTES_DIR,
// CLIENT_TS), whose LABEL_SOURCES holds them, so that a boot, which loads daemon-pane.ts, does not
// load this module.
import { existsSync, readdirSync, readFileSync } from "node:fs";
import path from "node:path";
import { CLIENT_TS, ROUTES_DIR, SCOPES_GO } from "./daemon-pane";

/** The sources `labelPersonRoutes` reads, under a checkout's root: daemon-pane.ts's
 * `recordedCheckout` rebuilds them out of a run's commit beside the daemon's. ROUTES_DIR holds
 * SCOPES_GO and, since main's #5409, every route's own file. */
export const PERSON_ROUTE_SOURCES = [ROUTES_DIR, CLIENT_TS];

/** The line that opens `routeScopes`, the line alone that closes it, and one of its rows:
 * `"<METHOD> <pattern>": {<scope>, …},`, as gofmt writes it. */
const SCOPES_OPEN = "var routeScopes = map[string]routeScopeSet{";
const SCOPES_CLOSE = "}";
const SCOPES_ROW = /^\t"([A-Z]+) (\/\S*)":\s+\{([^{}]*)\},$/;
/** The scope a credential acts as its person by (model.go's `ScopePerson`, "person"), which the
 * shared agent token does not hold. */
const PERSON_SCOPE = "model.ScopePerson";
/** One route a file of Dispatch's API declares since main's #5409, `declareRoute(routeDecl{`
 * through the `})` that closes it at the start of a line, as dispatch-route-adopt writes each,
 * and the lines of it this reads: its method, its pattern and the scopes it needs. */
const DECLARATION = /declareRoute\(routeDecl\{\n([\s\S]*?)\n\}\)/g;
const DECLARED_METHOD = /^\tMethod:\s+http\.Method([A-Za-z]+),$/m;
const DECLARED_PATTERN = /^\tPattern:\s+"(\/\S*)",$/m;
const DECLARED_PATTERN_NAME = /^\tPattern:\s+(\w+),$/m;
const DECLARED_SCOPES = /^\tOAuthScopes:\s+routeScopeSet\{([^{}]*)\},$/m;

/** How the client spells a route's path: an array of its segments, `["api", "v1", …]`, a string
 * literal for a fixed segment and any other expression for one of the caller's values. */
const CLIENT_PATH = /\[\s*"api",\s*"v1"\s*[,\]]/g;
/** The calls a path is sent by, read off the text before the path: `this.#json`,
 * `this.#jsonAnswer`, `this.#form`, and main's `this.#write` and `this.#writeAnswer` (a write
 * sent once under one key, since main's #4639), with the method before it; `this.#upload`,
 * `this.#writingNothing` and `this.#roundTrip` (which hands its path to `this.#writingNothing`),
 * which post it; main's `this.#read` (`#json`'s GET at a cut, since #4639), which gets it; and a
 * `const` the path is bound to, directly, as either arm of a `? :`, or as the URL `this.#url`
 * makes of it, which a later call of the same method of the client sends: one of those first five
 * or `this.#send` given the method and the const, `this.#writingNothing` given the const, which
 * posts it, or `this.#read` given the const, which gets it. */
const SENT = /this\.#(?:json|jsonAnswer|form|write|writeAnswer)(?:<[^()]*>)?\(\s*"([A-Z]+)",\s*$/;
const POSTED = /this\.#(?:upload|writingNothing|roundTrip)(?:<[^()]*>)?\(\s*$/;
const READ = /this\.#read(?:<[^()]*>)?\(\s*$/;
const BOUND = /\bconst (\w+) = (?:this\.#url\(\s*|(?:[^;]*[?:] )?)$/;
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
 * order, or, for a source with no such table, every route a file of ROUTES_DIR declares with that
 * scope (`declaredPersonScoped`). Throws, naming the file and the marker, for a source that holds
 * neither, no line closing the table, a line in it that is neither a row, a comment nor blank, or
 * no row of that scope. */
function personScoped(checkout: string): string[] {
  const text = sourceOf(checkout, SCOPES_GO, "holds the scopes each route needs (routeScopes)");
  const lines = text.split("\n");
  const open = lines.indexOf(SCOPES_OPEN);
  const declared = open < 0 ? declaredPersonScoped(checkout) : undefined;
  if (declared !== undefined) return declared;
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

/** Every route a file of ROUTES_DIR declares with the person scope, `<METHOD> <pattern>` each, by
 * file name and then in each file's order, as main's Dispatch declares its routes since #5409;
 * undefined where no file there declares a route, so the caller refuses the source as one of no
 * shape. Only a person-scoped route's method and pattern are read: a pattern given as a constant
 * (`Pattern: goingCataloguePath,`) is that constant's string, `<name> = "<pattern>"` in a file of
 * ROUTES_DIR. Throws, naming the file, for a person-scoped declaration whose method or pattern
 * this cannot read, and for routes none of which needs that scope. */
function declaredPersonScoped(checkout: string): string[] | undefined {
  const dir = path.join(checkout, ROUTES_DIR);
  const files = existsSync(dir)
    ? readdirSync(dir)
        .filter((file) => file.endsWith(".go") && !file.endsWith("_test.go"))
        .sort()
    : [];
  const texts = files.map((file) => [file, readFileSync(path.join(dir, file), "utf8")] as const);
  const constant = (name: string) => {
    const held = new RegExp(`\\b${name}\\s*=\\s*"(/[^"]*)"`);
    for (const [, text] of texts) {
      const found = held.exec(text)?.[1];
      if (found !== undefined) return found;
    }
    return undefined;
  };
  const routes: string[] = [];
  let declared = 0;
  for (const [file, text] of texts) {
    for (const [, body = ""] of text.matchAll(DECLARATION)) {
      declared += 1;
      const scopes = DECLARED_SCOPES.exec(body)?.[1] ?? "";
      if (!scopes.split(",").some((scope) => scope.trim() === PERSON_SCOPE)) continue;
      const method = DECLARED_METHOD.exec(body)?.[1];
      const named = DECLARED_PATTERN_NAME.exec(body)?.[1];
      const pattern =
        DECLARED_PATTERN.exec(body)?.[1] ?? (named === undefined ? undefined : constant(named));
      if (method === undefined || pattern === undefined)
        throw new Error(
          `${ROUTES_DIR}/${file} declares a person-scoped route with no \`Method: http.Method<Name>,\` line, or no \`Pattern:\` line holding "<pattern>" or a constant of one`
        );
      routes.push(`${method.toUpperCase()} ${pattern}`);
    }
  }
  if (declared === 0) return undefined;
  if (routes.length === 0)
    throw new Error(
      `no route a file of ${ROUTES_DIR} declares gives ${PERSON_SCOPE}, the person scope`
    );
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
    let method =
      SENT.exec(before)?.[1] ??
      (POSTED.test(before) ? "POST" : READ.test(before) ? "GET" : undefined);
    if (method === undefined && bound !== undefined) {
      const rest = text.slice(spelled.end);
      const body = rest.slice(0, METHOD_END.exec(rest)?.index ?? rest.length);
      const sends = new RegExp(
        `this\\.#(?:json|jsonAnswer|form|write|writeAnswer|send)(?:<[^()]*>)?\\(\\s*"([A-Z]+)",\\s*${bound}\\b`
      );
      const posts = new RegExp(`this\\.#writingNothing(?:<[^()]*>)?\\(\\s*${bound}\\b`);
      const reads = new RegExp(`this\\.#read(?:<[^()]*>)?\\(\\s*${bound}\\b`);
      method =
        sends.exec(body)?.[1] ?? (posts.test(body) ? "POST" : reads.test(body) ? "GET" : undefined);
    }
    if (method === undefined)
      throw new Error(
        `${CLIENT_TS}:${line} spells the route path ${text.slice(open, spelled.end).replace(/\s+/g, " ")}, which no call this reads sends: this.#json, this.#jsonAnswer or this.#form given "<METHOD>" and it, this.#upload, this.#writingNothing or this.#roundTrip given it, or a const it is bound to that one of the first three or this.#send given "<METHOD>", or this.#writingNothing, sends later in the same method`
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
