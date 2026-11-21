// The routes of GitHub a worker world answers, and the sweep that reads which routes the role
// texts name. GitHub answers the same routes whichever label asks, so the routes a world must
// answer are every `gh` read the role texts of either label name — `skills/` and
// `packages/daemon/internal/prompts/` of each checkout — and every route the census saw an agent
// call, each of those declared with the run that called it (`WORLD_ROUTES`). A route a text
// names that the world does not answer is a refusal production never gives, and it voids the run
// that meets it; a route the world answers that nothing names is a world that tells an agent
// more than it was asked about.
//
// A route is written as a method and a path template: `GET repos/{owner}/{repo}/commits/{ref}/
// status` for a REST call, `GRAPHQL query repository.pullRequest.reviewThreads` for a GraphQL
// query (its fields from the root down to the first that selects more than one, or a page of a
// connection), `GRAPHQL mutation resolveReviewThread` for a mutation, and `GH pr view` for a gh
// command of its own. The gh stand-in reads its GraphQL calls with this module's parser and
// answers them with its executor, so the query the sweep names and the query the world answers
// are read one way.
import { readdirSync, readFileSync, statSync } from "node:fs";
import path from "node:path";
import { endpointOf, FIELD_FLAGS, methodOf, valueAt } from "./gh-call";
import { SCHEMA_TYPES } from "./graphql-schema";

/** A route a text names or a call reaches: its key and whether it reads or writes. */
export interface RouteKey {
  readonly key: string;
  readonly kind: "read" | "write";
}

/** The `gh pr`, `gh run` and `gh auth` commands that read: every other one writes. */
const READ_COMMANDS: Readonly<Record<string, readonly string[]>> = {
  pr: ["view", "list", "diff", "checks", "status"],
  run: ["view", "list", "watch", "download"],
  auth: ["status"],
};

/** A path segment that a text writes as a placeholder (`<n>`, `{owner}`, `$base_path`, `<approved
 * sha>...<new head>`) rather than as a value. */
const PLACEHOLDER = /[<>{}$]/;

/** The route `gh <argv...>` reaches, or undefined for a call that names none (a bare `gh api`, a
 * `gh pr` with no command, a GraphQL call whose query is a file or no query this parser reads).
 * A path segment written as a placeholder is `{}`; a value stays as it is written, and a route
 * template's parameter matches either (`templateMatches`). The query string is no part of the
 * route: GitHub routes `actions/runs?head_sha=…` to the runs list and filters it. */
export function routeOf(argv: readonly string[]): RouteKey | undefined {
  const [program, command] = argv;
  if (program === "pr" || program === "run" || program === "auth") {
    const name = (command ?? "").replace(/[.,:;]+$/, "");
    if (!/^[a-z][a-z-]*$/.test(name)) return undefined;
    const read = READ_COMMANDS[program]?.includes(name) ?? false;
    return { key: `GH ${program} ${name}`, kind: read ? "read" : "write" };
  }
  if (program !== "api") return undefined;
  const endpoint = endpointOf(argv)?.replace(/[.,:;]+$/, "");
  if (endpoint === undefined || endpoint === "") return undefined;
  if (endpoint === "graphql") {
    const query = graphqlQueryOf(argv);
    if (query === undefined) return undefined;
    const operation = parseGraphql(query);
    return operation === undefined
      ? undefined
      : { key: graphqlKey(operation), kind: operation.type === "query" ? "read" : "write" };
  }
  const segments = (endpoint.replace(/^\//, "").split("?")[0] ?? "")
    .split("/")
    .filter((segment) => segment !== "")
    .map((segment) => (PLACEHOLDER.test(segment) ? "{}" : segment));
  const method = methodOf(argv);
  return { key: `${method} ${segments.join("/")}`, kind: method === "GET" ? "read" : "write" };
}

/** A query a text writes as a placeholder for a class of queries rather than as one it sends:
 * an ellipsis (`query=…`, as headless.md's account of the reads `legion gh` runs writes it) or
 * one placeholder in angle brackets (`query=<query>`). */
const PLACEHOLDER_QUERY = /^(?:…|\.\.\.|<[^<>]+>)$/;

/** The query text a `gh api graphql` call sends in its `query` field, or undefined when it sends
 * none, sends a file (`-F query=@q.graphql`), whose text a role text does not hold, or names a
 * class of queries with a placeholder (PLACEHOLDER_QUERY), which, like a bare `gh api graphql`,
 * names no route. */
export function graphqlQueryOf(argv: readonly string[]): string | undefined {
  let query: string | undefined;
  for (const index of argv.keys()) {
    const pair = valueAt(argv, index, FIELD_FLAGS);
    if (pair?.startsWith("query=")) query = pair.slice("query=".length);
  }
  return query === undefined || query.startsWith("@") || PLACEHOLDER_QUERY.test(query)
    ? undefined
    : query;
}

/** Whether the route `key` is one `template` answers: the same method, and a path whose segments
 * are the template's, where a `{name}` segment of the template is any one segment and a
 * `{name+}` one is one segment or more. A GraphQL or gh command key is its template exactly. */
export function templateMatches(template: string, key: string): boolean {
  const [method = "", rest = ""] = splitKey(template);
  const [keyMethod = "", keyRest = ""] = splitKey(key);
  if (method !== keyMethod) return false;
  if (method === "GRAPHQL" || method === "GH") return rest === keyRest;
  const want = rest.split("/");
  const got = keyRest.split("/");
  const walk = (w: number, g: number): boolean => {
    if (w === want.length) return g === got.length;
    const segment = want[w] ?? "";
    if (/^\{\w+\+\}$/.test(segment)) {
      for (let end = g + 1; end <= got.length; end++) if (walk(w + 1, end)) return true;
      return false;
    }
    if (g === got.length) return false;
    const value = got[g] ?? "";
    if (/^\{\w+\}$/.test(segment)) return walk(w + 1, g + 1);
    return value === segment && walk(w + 1, g + 1);
  };
  return walk(0, 0);
}

function splitKey(key: string): [string, string] {
  const space = key.indexOf(" ");
  return space < 0 ? [key, ""] : [key.slice(0, space), key.slice(space + 1)];
}

/** One `gh` command a role text names: the route it reaches, where the text names it and the
 * command as the text writes it. */
export interface Named extends RouteKey {
  readonly file: string;
  readonly line: number;
  readonly command: string;
}

/** The directories of a checkout that hold its role texts: the skills, and the daemon's prompts. */
export const TEXT_DIRS = ["skills", path.join("packages", "daemon", "internal", "prompts")];

/** Every `gh` route the role texts under `root` name (`TEXT_DIRS`), with the file and line that
 * name it, and every GraphQL call they write whose query this module cannot read (`unread`): a
 * sweep that skipped a query it could not read would pass a text whose route the world refuses.
 * A bare `gh api graphql` in prose sends no query and names no route. A Go test file is no role
 * text, and the sweep reads none. */
export function sweep(root: string): { named: Named[]; unread: Omit<Named, "key" | "kind">[] } {
  const named: Named[] = [];
  const unread: Omit<Named, "key" | "kind">[] = [];
  for (const dir of TEXT_DIRS) {
    for (const file of filesUnder(path.join(root, dir))) {
      if (file.endsWith("_test.go")) continue;
      const relative = path.relative(root, file);
      for (const found of commandsIn(readFileSync(file, "utf8"))) {
        const route = routeOf(found.argv);
        const command = `gh ${found.argv.join(" ")}`;
        if (route !== undefined)
          named.push({ ...route, file: relative, line: found.line, command });
        else if (found.argv[0] === "api" && graphqlQueryOf(found.argv) !== undefined)
          unread.push({ file: relative, line: found.line, command });
      }
    }
  }
  return { named, unread };
}

function filesUnder(dir: string): string[] {
  let entries: string[];
  try {
    entries = readdirSync(dir).sort();
  } catch {
    return [];
  }
  return entries.flatMap((entry) => {
    const full = path.join(dir, entry);
    return statSync(full).isDirectory() ? filesUnder(full) : [full];
  });
}

/** Where a `gh api`, `gh pr` or `gh run` command starts in a text. */
const GH_COMMAND = /(?<![\w./-])gh[ \t]+(?:api|pr|run)\b/g;

/** Every `gh api`, `gh pr` and `gh run` command `text` writes, as the argv after `gh` a shell
 * would hand it, with the line it starts on. A command ends where a shell command ends — an
 * unescaped newline, `|`, `&&`, `;`, `)` or `>` outside quotes — and, written inside a
 * Markdown code span, at the span's closing backtick, the span's own line breaks being spaces.
 * A backslash at a line's end continues the command, as in a shell. A placeholder in angle
 * brackets is one word, spaces and all (`<approved sha>...<new head>`). */
export function commandsIn(text: string): { argv: string[]; line: number }[] {
  const found: { argv: string[]; line: number }[] = [];
  for (const match of text.matchAll(GH_COMMAND)) {
    const start = match.index ?? 0;
    const argv = wordsFrom(text, start, text[start - 1] === "`").slice(1);
    found.push({ argv, line: text.slice(0, start).split("\n").length });
  }
  return found;
}

function wordsFrom(text: string, start: number, inline: boolean): string[] {
  const words: string[] = [];
  let word: string | undefined;
  const end = () => {
    if (word !== undefined) words.push(word);
    word = undefined;
  };
  let index = start;
  while (index < text.length) {
    const char = text[index] ?? "";
    if (inline && char === "`") break;
    if (char === "\\" && text[index + 1] === "\n") {
      end();
      index += 2;
      continue;
    }
    if (char === "\n" && !inline) break;
    if (char === " " || char === "\t" || char === "\n") {
      end();
      index++;
      continue;
    }
    if (char === "'" || char === '"') {
      const close = closing(text, index, char);
      const span = close < 0 ? "" : text.slice(index + 1, close);
      if (close < 0 || (inline && span.includes("`"))) break;
      word = (word ?? "") + (char === '"' ? span.replace(/\\(["\\$`])/g, "$1") : span);
      index = close + 1;
      continue;
    }
    if (char === "<") {
      const close = text.indexOf(">", index);
      const span = close < 0 ? "" : text.slice(index, close + 1);
      if (close < 0 || span.includes("\n") || span.length > 80 || span.slice(1).includes("<"))
        break;
      word = (word ?? "") + span;
      index = close + 1;
      continue;
    }
    if ("|;&()>".includes(char)) break;
    word = (word ?? "") + char;
    index++;
  }
  end();
  return words;
}

/** The index of the quote that closes the one at `open`, or -1: a double quote skips each
 * backslash-escaped character, as a shell does. */
function closing(text: string, open: number, quote: string): number {
  for (let index = open + 1; index < text.length; index++) {
    if (quote === '"' && text[index] === "\\") index++;
    else if (text[index] === quote) return index;
  }
  return -1;
}

// GraphQL, as far as reading which route a query reaches and answering it from a world goes:
// operations, variables, fields with arguments and aliases, inline fragments, read token by token
// as GitHub's own parser (graphql-ruby) reads them, so a query that is no GraphQL document is
// refused as GitHub refuses it: its parser's message for the token it stopped at, and where that
// token stands (`readGraphql`). A fragment spread, a directive, a fragment definition or a
// document of more than one operation is GraphQL that no text or call of these worlds writes,
// and a query holding one is one this module does not read.

/** Where a token of a query starts, as GitHub counts it: its line and its column, each from 1. */
export interface Location {
  readonly line: number;
  readonly column: number;
}

type Value =
  | { readonly kind: "var"; readonly name: string }
  | { readonly kind: "scalar"; readonly value: string | number | boolean | null }
  | { readonly kind: "enum"; readonly value: string }
  | { readonly kind: "list"; readonly values: readonly Value[] }
  | { readonly kind: "object"; readonly fields: Readonly<Record<string, Value>> };

export type Selection =
  | {
      readonly kind: "field";
      readonly alias: string | undefined;
      readonly name: string;
      readonly args: Readonly<Record<string, Value>>;
      readonly selections: readonly Selection[] | undefined;
      /** Where the field starts (its alias, where it has one), which GitHub's errors name. */
      readonly loc: Location;
    }
  | {
      readonly kind: "fragment";
      readonly on: string | undefined;
      readonly selections: readonly Selection[];
    };

export interface Operation {
  readonly type: "query" | "mutation" | "subscription";
  readonly name: string | undefined;
  /** Each variable the operation declares, its type and where its `$` stands, which GitHub's
   * refusal of a variable the call does not send names. */
  readonly variables: readonly { name: string; type: string; loc: Location }[];
  readonly selections: readonly Selection[];
}

/** One error of GitHub's GraphQL answer, with the keys GitHub gives it in the order it writes
 * them: a parser's refusal is its `message` and its `locations`; a validator's (`undefinedFields`)
 * its `path`, `extensions`, `locations` and `message`; an object a query names and the world has
 * not got, its `type`, `path`, `locations` and `message` (measured on 2026-11-17, gh-standin.ts's
 * `graphAnswer` says how). */
export interface GraphError {
  readonly type?: string;
  readonly path?: readonly string[];
  readonly extensions?: Readonly<Record<string, unknown>>;
  readonly locations?: readonly Location[];
  readonly message: string;
}

/** The name GitHub's lexer gives each punctuation token, which its parser's refusal names. */
const PUNCTUATION: Readonly<Record<string, string>> = {
  "{": "LCURLY",
  "}": "RCURLY",
  "(": "LPAREN",
  ")": "RPAREN",
  "[": "LBRACKET",
  "]": "RBRACKET",
  ":": "COLON",
  $: "VAR_SIGN",
  "@": "DIR_SIGN",
  "=": "EQUALS",
  "!": "BANG",
  "|": "PIPE",
  "&": "AMP",
};
/** The words GitHub's lexer reads as tokens of their own, each named by the word in capitals;
 * its parser takes each where it takes a name. */
const KEYWORDS = new Set([
  "on",
  "fragment",
  "true",
  "false",
  "null",
  "query",
  "mutation",
  "subscription",
  "schema",
  "scalar",
  "type",
  "extend",
  "implements",
  "interface",
  "union",
  "enum",
  "input",
  "directive",
  "repeatable",
]);
/** The tokens a document's definition starts with that define no operation: a fragment, an
 * extension and the definitions of a schema, each GraphQL this module does not read. */
const DEFINITIONS = new Set([
  "FRAGMENT",
  "EXTEND",
  "SCHEMA",
  "SCALAR",
  "TYPE",
  "ENUM",
  "INPUT",
  "UNION",
  "INTERFACE",
  "DIRECTIVE",
]);
const BLOCK_STRING = /"""(?:\\"""|[^"]|"(?!""))*"""/y;
const STRING = /"(?:[^"\\\n\r]|\\["\\/bfnrt]|\\u[0-9A-Fa-f]{4})*"/y;
const NUMBER = /-?(?:0|[1-9]\d*)(\.\d+)?([eE][+-]?\d+)?/y;
const NAME = /[_A-Za-z][_0-9A-Za-z]*/y;
/** GitHub's lexer's refusal of a string it cannot read to its end, which names where the string
 * starts and, unlike its parser's, says nothing of that place in its message. Measured on
 * 2026-11-17: `query{repository(owner:"<owner>,name:"<repo>"){…}}` was answered `Expected string
 * or block string, but it was malformed` at line 1, column 51, the quote that opens a string no
 * quote closes. */
const MALFORMED_STRING = "Expected string or block string, but it was malformed";

/** A token of a query: its kind as GitHub's lexer names it, its text, the value GitHub's parser
 * names it by in a refusal, whether that parser takes it as a name, and where it starts. */
interface Token {
  readonly kind: string;
  readonly text: string;
  readonly value: string;
  readonly name: boolean;
  readonly at: Location;
}
/** GitHub's refusal of a query, thrown where its parser stops. */
class Refused extends Error {
  constructor(readonly error: GraphError) {
    super(error.message);
  }
}
/** A document of GraphQL this module does not read. */
class Unread extends Error {}

/** The reader of `text`'s tokens, one a call, as GitHub's lexer reads them: past white space,
 * line ends, commas and comments, and only when its parser asks for the next, so a string no quote
 * closes is refused only once the parser reaches it. A character no token starts with is GitHub's
 * `UNKNOWN_CHAR`, whose value in a refusal is the character after it — measured on 2026-11-17:
 * `query{viewer{login %x}}` was refused `Expected NAME, actual: UNKNOWN_CHAR ("x") at [1, 20]`, and
 * `…login %}}` with `("}")` — as graphql-ruby's lexer reads the one character past it. */
function tokensOf(text: string): () => Token | undefined {
  let offset = 0;
  let line = 1;
  let lineStart = 0;
  const match = (pattern: RegExp) => {
    pattern.lastIndex = offset;
    return pattern.exec(text);
  };
  return () => {
    while (offset < text.length) {
      const char = text[offset];
      if (char === "\n" || char === "\r") {
        offset += char === "\r" && text[offset + 1] === "\n" ? 2 : 1;
        line++;
        lineStart = offset;
      } else if (char === "#")
        while (offset < text.length && text[offset] !== "\n" && text[offset] !== "\r") offset++;
      else if (char === " " || char === "\t" || char === "," || char === "\uFEFF") offset++;
      else break;
    }
    if (offset >= text.length) return undefined;
    const at = { line, column: offset - lineStart + 1 };
    const char = text[offset] ?? "";
    const token = (kind: string, length: number, value: string, name = false): Token => {
      const read = text.slice(offset, offset + length);
      offset += length;
      return { kind, text: read, value, name, at };
    };
    if (text.startsWith("...", offset)) return token("ELLIPSIS", 3, "...");
    const punctuation = PUNCTUATION[char];
    if (punctuation !== undefined) return token(punctuation, 1, char);
    if (char === '"') {
      const block = text.startsWith('"""', offset);
      const read = match(block ? BLOCK_STRING : STRING)?.[0];
      if (read === undefined) throw new Refused({ message: MALFORMED_STRING, locations: [at] });
      // A block string's lines are lines of the query, which every later token is counted from.
      const breaks = [...read.matchAll(/\r\n|\n|\r/g)];
      const value = block ? read.slice(3, -3) : (JSON.parse(read) as string);
      const string = token("STRING", read.length, value);
      const lastBreak = breaks.at(-1);
      if (lastBreak !== undefined) {
        line += breaks.length;
        lineStart = offset - read.length + (lastBreak.index ?? 0) + lastBreak[0].length;
      }
      return string;
    }
    const number = /[-\d]/.test(char) ? match(NUMBER) : null;
    if (number !== null)
      return token(
        number[1] === undefined && number[2] === undefined ? "INT" : "FLOAT",
        number[0].length,
        number[0]
      );
    const word = match(NAME)?.[0];
    if (word !== undefined)
      return token(KEYWORDS.has(word) ? word.toUpperCase() : "IDENTIFIER", word.length, word, true);
    return token("UNKNOWN_CHAR", 1, text[offset + 1] ?? "");
  };
}

/** The one operation the document `text` holds, read as GitHub's parser reads it: each refusal
 * is that parser's, `Expected <what>, actual: <token> (<value>) at [<line>, <column>]`, at the
 * token it stopped at, or, past the last token, at that last token — measured on 2026-11-17
 * against production, each answered so and exit 1: a query with one `}` too few (`Expected NAME,
 * actual: (none) ("") at [1, 87]`, the query's last `}`, and at [1, 176] for the eight `{` and
 * seven `}` of prod-gql-error), a `{` where a `)` belongs (`Expected NAME, actual: LCURLY ("{")`),
 * an argument with no `:` (`Expected COLON, actual: STRING ("<owner>")`, `actual: INT ("<n>")`),
 * no value (`Expected VALUE, actual: RPAREN (")")`), `()` (`Expected ARGUMENT_NAME`), a `}` too many
 * or a misspelt `query` (`Expected one of SCHEMA, SCALAR, TYPE, ENUM, INPUT, UNION, INTERFACE,
 * actual: RCURLY ("}")`, `actual: IDENTIFIER ("quer") at [1, 1]`), and a document of white space
 * and comments alone (`Unexpected end of document`, with no location). Throws `Refused` with
 * GitHub's error, or `Unread` for GraphQL this module does not read. */
function parseDocument(text: string): Operation {
  const next = tokensOf(text);
  let token = next();
  if (token === undefined) throw new Refused({ message: "Unexpected end of document" });
  let last = token.at;
  const advance = () => {
    token = next();
    if (token !== undefined) last = token.at;
  };
  const at = (kind: string) => token?.kind === kind;
  const refuse = (message: string): never => {
    const where = token?.at ?? last;
    throw new Refused({
      message: `${message} at [${where.line}, ${where.column}]`,
      locations: [where],
    });
  };
  const actual = () => `actual: ${token?.kind ?? "(none)"} (${JSON.stringify(token?.value ?? "")})`;
  const expect = (kind: string) => {
    if (!at(kind)) refuse(`Expected ${kind}, ${actual()}`);
    advance();
  };
  const name = (): string => {
    const read = token;
    if (read === undefined || !read.name) return refuse(`Expected NAME, ${actual()}`);
    advance();
    return read.text;
  };
  const value = (): Value => {
    const read = token;
    switch (read?.kind) {
      case "INT":
      case "FLOAT":
        advance();
        return { kind: "scalar", value: Number(read.text) };
      case "STRING":
        advance();
        return { kind: "scalar", value: read.value };
      case "TRUE":
      case "FALSE":
        advance();
        return { kind: "scalar", value: read.kind === "TRUE" };
      case "NULL":
        advance();
        return { kind: "scalar", value: null };
      case "IDENTIFIER":
        advance();
        return { kind: "enum", value: read.text };
      case "VAR_SIGN":
        advance();
        return { kind: "var", name: name() };
      case "LBRACKET": {
        advance();
        const values: Value[] = [];
        while (!at("RBRACKET")) values.push(value());
        advance();
        return { kind: "list", values };
      }
      case "LCURLY": {
        advance();
        const fields: Record<string, Value> = {};
        while (!at("RCURLY")) {
          const key = name();
          expect("COLON");
          fields[key] = value();
        }
        advance();
        return { kind: "object", fields };
      }
      default:
        return refuse(`Expected VALUE, ${actual()}`);
    }
  };
  const typeOf = (variable: string): string => {
    let read: string;
    if (at("LBRACKET")) {
      advance();
      read = `[${typeOf(variable)}]`;
      expect("RBRACKET");
    } else if (at("IDENTIFIER")) read = name();
    else return refuse(`Missing type definition for variable: $${variable}`);
    if (!at("BANG")) return read;
    advance();
    return `${read}!`;
  };
  const selectionSet = (): Selection[] => {
    expect("LCURLY");
    const selections: Selection[] = [];
    while (!at("RCURLY")) {
      if (at("ELLIPSIS")) {
        advance();
        if (at("LCURLY")) {
          selections.push({ kind: "fragment", on: undefined, selections: selectionSet() });
          continue;
        }
        if (!at("ON")) {
          // A spread of a named fragment, which GitHub reads by its name.
          name();
          throw new Unread();
        }
        advance();
        const on = name();
        if (at("DIR_SIGN")) throw new Unread();
        selections.push({ kind: "fragment", on, selections: selectionSet() });
        continue;
      }
      const loc = token?.at ?? last;
      let field = name();
      let alias: string | undefined;
      if (at("COLON")) {
        advance();
        alias = field;
        field = name();
      }
      const args: Record<string, Value> = {};
      if (at("LPAREN")) {
        advance();
        while (!at("RPAREN")) {
          const key = name();
          expect("COLON");
          args[key] = value();
        }
        if (Object.keys(args).length === 0) refuse(`Expected ARGUMENT_NAME, ${actual()}`);
        advance();
      }
      if (at("DIR_SIGN")) throw new Unread();
      const inner = at("LCURLY") ? selectionSet() : undefined;
      selections.push({ kind: "field", alias, name: field, args, selections: inner, loc });
    }
    advance();
    return selections;
  };
  const operation = (start: Token): Operation => {
    const type = start.kind === "LCURLY" ? "query" : (start.text as Operation["type"]);
    if (start.kind !== "LCURLY") advance();
    const named = at("LPAREN") || at("LCURLY") || at("DIR_SIGN") ? undefined : name();
    const variables: { name: string; type: string; loc: Location }[] = [];
    if (at("LPAREN")) {
      advance();
      while (!at("RPAREN")) {
        const loc = token?.at ?? last;
        expect("VAR_SIGN");
        const variable = name();
        expect("COLON");
        const declared = typeOf(variable);
        if (at("EQUALS")) {
          advance();
          value();
        }
        if (at("DIR_SIGN")) throw new Unread();
        variables.push({ name: variable, type: declared, loc });
      }
      advance();
    }
    if (at("DIR_SIGN")) throw new Unread();
    return { type, name: named, variables, selections: selectionSet() };
  };
  const operations: Operation[] = [];
  for (let start = token; start !== undefined; start = token) {
    if (["QUERY", "MUTATION", "SUBSCRIPTION", "LCURLY"].includes(start.kind)) {
      operations.push(operation(start));
      continue;
    }
    // A definition of the schema's own may follow a description, which is a string.
    if (start.kind === "STRING") advance();
    if (token !== undefined && DEFINITIONS.has(token.kind)) throw new Unread();
    refuse(`Expected one of SCHEMA, SCALAR, TYPE, ENUM, INPUT, UNION, INTERFACE, ${actual()}`);
  }
  const [one, ...more] = operations;
  if (one === undefined || more.length > 0) throw new Unread();
  return one;
}

/** The operation `text` holds, or undefined when it holds none this module reads: a query that
 * is no GraphQL document, or GraphQL this module does not read. */
export function parseGraphql(text: string): Operation | undefined {
  try {
    return parseDocument(text);
  } catch (error) {
    if (error instanceof Refused || error instanceof Unread) return undefined;
    throw error;
  }
}

/** What GitHub makes of the query `text` a call sends: the operation it runs; its refusal of a
 * query that is no GraphQL document (`parseDocument`), of one selecting a field its type has not
 * got (`undefinedFields`), and of a call that sends no query, which GitHub answers `A query
 * attribute must be specified and must be a string.` with no location (measured on 2026-11-17
 * for `-f query=` and for a call with no `query` field); or `unread`, GraphQL this module does not
 * read. */
export function readGraphql(
  text: string
): { operation: Operation } | { refused: GraphError[] } | { unread: true } {
  if (text === "")
    return { refused: [{ message: "A query attribute must be specified and must be a string." }] };
  let operation: Operation;
  try {
    operation = parseDocument(text);
  } catch (error) {
    if (error instanceof Refused) return { refused: [error.error] };
    if (error instanceof Unread) return { unread: true };
    throw error;
  }
  const refused = undefinedFields(operation);
  return refused.length === 0 ? { operation } : { refused };
}

/** GitHub's refusal of each field `operation` selects that the type it selects it on has not got,
 * read against GitHub's own schema of the world's types (graphql-schema.ts): GitHub validates the
 * whole query before it runs any of it and answers every such field, with no data. Each is named
 * by the path GitHub's validator gives it — the operation (`query`, or `query <name>`), each field
 * above it by the name it answers under (its alias, where it has one), and `... on <type>` for an
 * inline fragment — measured on 2026-11-17: an aliased field of a named query was refused at
 * `["query Q","repository","pr","b"]`, a field of a review thread's node at
 * `[…,"reviewThreads","nodes","nope"]`, and one of an inline fragment at
 * `[…,"author","... on Bot","nope"]`, each with `extensions` naming its code, its type and the
 * field. A field whose type the schema does not hold is read no further, and nothing below it is
 * refused. */
export function undefinedFields(operation: Operation): GraphError[] {
  const errors: GraphError[] = [];
  const walk = (selections: readonly Selection[], type: string, at: readonly string[]) => {
    const fields = SCHEMA_TYPES[type];
    if (fields === undefined) return;
    for (const selection of selections) {
      if (selection.kind === "fragment") {
        if (selection.on === undefined) walk(selection.selections, type, at);
        else walk(selection.selections, selection.on, [...at, `... on ${selection.on}`]);
        continue;
      }
      // GitHub's own fields of every type: `__typename`, and the schema's at the root.
      if (selection.name.startsWith("__")) continue;
      const path = [...at, selection.alias ?? selection.name];
      const returns = fields[selection.name];
      if (returns === undefined)
        errors.push({
          path,
          extensions: { code: "undefinedField", typeName: type, fieldName: selection.name },
          locations: [selection.loc],
          message: `Field '${selection.name}' doesn't exist on type '${type}'`,
        });
      else if (selection.selections !== undefined) walk(selection.selections, returns, path);
    }
  };
  const root = { query: "Query", mutation: "Mutation", subscription: "Subscription" }[
    operation.type
  ];
  walk(operation.selections, root, [
    operation.name === undefined ? operation.type : `${operation.type} ${operation.name}`,
  ]);
  return errors;
}

/** The route key of `operation`: a mutation by its first field, a query by the path of fields
 * from the root down to the first that selects more than one field, or a page of a connection
 * (`nodes`, `edges`, `pageInfo`). */
export function graphqlKey(operation: Operation): string {
  if (operation.type !== "query") {
    const first = operation.selections.find((selection) => selection.kind === "field");
    return `GRAPHQL ${operation.type} ${first?.kind === "field" ? first.name : ""}`;
  }
  const descend = (selections: readonly Selection[]): string[] => {
    const fields = selections.filter(
      (selection) => selection.kind !== "field" || selection.name !== "__typename"
    );
    const [only] = fields;
    if (fields.length !== 1 || only?.kind !== "field") return [];
    if (["nodes", "edges", "pageInfo"].includes(only.name)) return [];
    return [only.name, ...(only.selections === undefined ? [] : descend(only.selections))];
  };
  return `GRAPHQL query ${descend(operation.selections).join(".")}`;
}

/** What GitHub's GraphQL endpoint answers: the data, and the errors where there are any, which
 * gh prints to stderr and exits 1 on. */
export interface GraphAnswer {
  readonly data?: Record<string, unknown> | null;
  readonly errors?: readonly GraphError[];
}

type Json = Record<string, unknown>;
/** Whether `value` is a JSON object: the one guard the world's answers are read by, here and in
 * the gh stand-in. */
export const isObject = (value: unknown): value is Json =>
  typeof value === "object" && value !== null && !Array.isArray(value);

/** The cursor of the item at `index` of a connection, opaque as GitHub's are, and back. */
const cursorOf = (index: number) => Buffer.from(`cursor:v2:${index}`).toString("base64");
function indexOf(cursor: unknown): number | undefined {
  if (typeof cursor !== "string") return undefined;
  const match = /^cursor:v2:(\d+)$/.exec(Buffer.from(cursor, "base64").toString());
  return match === null ? undefined : Number(match[1]);
}

/** `value` as the call's `variables` give it: a variable its value (null where the call sends
 * none), a list and an input object each of their values. */
function evaluated(value: Value, variables: Readonly<Record<string, unknown>>): unknown {
  switch (value.kind) {
    case "var":
      return variables[value.name] ?? null;
    case "scalar":
    case "enum":
      return value.value;
    case "list":
      return value.values.map((one) => evaluated(one, variables));
    case "object":
      return Object.fromEntries(
        Object.entries(value.fields).map(([key, field]) => [key, evaluated(field, variables)])
      );
  }
}

/** The arguments `field` is called with, as the call's `variables` give them: what the gh
 * stand-in reads a mutation's `input` from (gh-standin.ts's `threadCall`), by the reading the
 * executor below answers a query's arguments with. */
export function argumentsOf(
  field: Selection & { kind: "field" },
  variables: Readonly<Record<string, unknown>>
): Record<string, unknown> {
  return Object.fromEntries(
    Object.entries(field.args).map(([key, value]) => [key, evaluated(value, variables)])
  );
}

/** `operation` answered from `graph`, the world's state as GitHub's schema names it, with
 * `variables` the call's own. A list in the graph is a connection, paged as GitHub pages one
 * (`first`, `last`, `after`, `before`, `nodes`, `edges`, `pageInfo`, `totalCount`) and filtered by
 * `itemTypes` where a timeline is asked for by them; an object whose `__args` names the arguments
 * that find it (a repository by owner and name, a pull request by number) answers null and
 * GitHub's NOT_FOUND where a call names another, at the place the query names it (`locations`:
 * measured on 2026-11-17, `…{pullRequest(number:999999){number} name}}` was answered with the
 * pull request null, the repository's name, and the error's `type`, `path`, `locations` and
 * `message`, in that order); a field the graph does not hold is null. A variable the operation
 * declares non-null and the call does not send is GitHub's refusal of the whole query, with no
 * data: its `extensions` (the value it was given, null, and why that is no value), the place of
 * its `$` and its message, in that order, as measured on 2026-11-18 (implementer-reads.json, `gh
 * api graphql -f query='query($n:Int!){…}'` with no `n`). */
export function executeGraphql(
  operation: Operation,
  graph: Json,
  variables: Readonly<Record<string, unknown>>
): GraphAnswer {
  const missing = operation.variables.find(
    (variable) =>
      variable.type.endsWith("!") &&
      (variables[variable.name] === undefined || variables[variable.name] === null)
  );
  if (missing !== undefined)
    return {
      errors: [
        {
          extensions: {
            value: null,
            problems: [{ path: [], explanation: "Expected value to not be null" }],
          },
          locations: [missing.loc],
          message: `Variable $${missing.name} of type ${missing.type} was provided invalid value`,
        },
      ],
    };
  const errors: GraphError[] = [];
  const items = new WeakSet<unknown[]>();
  const project = (selections: readonly Selection[], value: Json, at: string[]): Json => {
    const out: Json = {};
    for (const selection of selections) {
      if (selection.kind === "fragment") {
        if (selection.on === undefined || value.__typename === selection.on)
          Object.assign(out, project(selection.selections, value, at));
        continue;
      }
      const key = selection.alias ?? selection.name;
      out[key] =
        selection.name === "__typename"
          ? (value.__typename ?? null)
          : resolve(selection, value[selection.name], [...at, key]);
    }
    return out;
  };
  const resolve = (field: Selection & { kind: "field" }, raw: unknown, at: string[]): unknown => {
    if (raw === undefined || raw === null) return null;
    if (field.selections === undefined) return raw;
    if (Array.isArray(raw) && items.has(raw))
      return raw.map((item) => (isObject(item) ? project(field.selections ?? [], item, at) : item));
    const args = argumentsOf(field, variables);
    const list = Array.isArray(raw)
      ? raw
      : isObject(raw) && Array.isArray(raw.nodes)
        ? raw.nodes
        : undefined;
    if (list !== undefined) {
      const page = connection(list, args, at);
      return page === undefined ? null : project(field.selections, page, at);
    }
    if (!isObject(raw)) return null;
    const finds = raw.__args;
    if (isObject(finds))
      for (const [name, want] of Object.entries(args))
        if (name in finds && String(finds[name]).toLowerCase() !== String(want).toLowerCase()) {
          errors.push({
            type: "NOT_FOUND",
            path: at,
            locations: [field.loc],
            message: notFound(field.name, args),
          });
          return null;
        }
    return project(field.selections, raw, at);
  };
  const connection = (list: unknown[], args: Json, at: string[]): Json | undefined => {
    const types = Array.isArray(args.itemTypes)
      ? args.itemTypes.map((type) =>
          String(type)
            .toLowerCase()
            .replace(/(?:^|_)([a-z])/g, (_, letter: string) => letter.toUpperCase())
        )
      : undefined;
    const all = list
      .map((node, index) => ({ node, index }))
      .filter(
        ({ node }) =>
          types === undefined || (isObject(node) && types.includes(String(node.__typename)))
      );
    let from = 0;
    let to = all.length;
    for (const [name, bound] of [
      ["after", 1],
      ["before", 0],
    ] as const) {
      if (args[name] === undefined || args[name] === null) continue;
      const index = indexOf(args[name]);
      if (index === undefined) {
        errors.push({
          type: "INVALID_CURSOR_ARGUMENTS",
          path: at,
          message: `\`${String(args[name])}\` does not appear to be a valid cursor.`,
        });
        return undefined;
      }
      const position = all.findIndex((entry) => entry.index === index);
      if (bound === 1) from = Math.max(from, position + 1);
      else to = Math.min(to, position < 0 ? to : position);
    }
    let window = all.slice(from, to);
    const first = typeof args.first === "number" ? args.first : undefined;
    const last = typeof args.last === "number" ? args.last : undefined;
    const before = window.length;
    if (first !== undefined) window = window.slice(0, first);
    if (last !== undefined) window = window.slice(Math.max(0, window.length - last));
    const nodes = window.map(({ node }) => node);
    const edges = window.map(({ node, index }) => ({ cursor: cursorOf(index), node }));
    items.add(nodes);
    items.add(edges);
    const startEntry = window[0];
    const endEntry = window[window.length - 1];
    return {
      nodes,
      edges,
      totalCount: all.length,
      pageInfo: {
        hasNextPage:
          first !== undefined ? window.length < before || to < all.length : to < all.length,
        hasPreviousPage: last !== undefined ? window.length < before || from > 0 : from > 0,
        startCursor: startEntry === undefined ? null : cursorOf(startEntry.index),
        endCursor: endEntry === undefined ? null : cursorOf(endEntry.index),
      },
    };
  };
  const data = project(operation.selections, graph, []);
  return errors.length === 0 ? { data } : { data, errors };
}

/** GitHub's NOT_FOUND message for the object `field` names by `args`. */
function notFound(field: string, args: Json): string {
  if (field === "repository")
    return `Could not resolve to a Repository with the name '${String(args.owner)}/${String(args.name)}'.`;
  if (field === "pullRequest")
    return `Could not resolve to a PullRequest with the number of ${String(args.number)}.`;
  return `Could not resolve to a ${field[0]?.toUpperCase() ?? ""}${field.slice(1)}.`;
}

/** Why a world answers a route no role text of either label names: a recorded run whose agent
 * called it (`run`, `<batch>/<run>/calls.jsonl:<line>`), or, where no recorded run called it, why
 * GitHub serves it to a role (`world`). A world that stopped answering such a route would give a
 * refusal production does not, so it stays, and says why. */
export type Evidence =
  | { readonly run: string; readonly reason: string }
  | { readonly world: string };

/** One route a world answers: its key (`templateMatches`), what its answer is read from, and,
 * for a route no role text of either label names, its evidence. */
export interface WorldRoute {
  readonly route: string;
  readonly answer: string;
  readonly evidence?: Evidence;
}

/** Every route a worker world answers (rig.sh's fixture_write and the gh stand-in's own answers).
 * A route a role text names needs no evidence; a route neither label's texts name carries its
 * evidence (`Evidence`). A call outside this table is refused as `gh: Not Found (HTTP 404)`
 * (gh-standin.ts), which `rig.sh health` reports as a world missing an answer, and, for a write,
 * as a write the world does not model. */
export const WORLD_ROUTES: readonly WorldRoute[] = [
  // Reads.
  {
    route: "GET repos/{owner}/{repo}",
    answer:
      "world.json and the remote: the repository, its settings and what GitHub reads off the remote, as GitHub answers it",
  },
  {
    route: "GET repos/{owner}/{repo}/pulls/{number}",
    answer:
      "world.json and the remote: the pull request at the branch's tip, as GitHub's REST API answers it",
  },
  {
    route: "GET repos/{owner}/{repo}/pulls/{number}/commits",
    answer:
      "the remote: the commits the pull request's head holds and its base does not, oldest first, paged, or GitHub's 404 for a pull request the world has not got",
    evidence: {
      world:
        "GitHub's REST list of the pull request's commits, the read `gh pr view --json commits` makes through GraphQL",
    },
  },
  {
    route: "GET repos/{owner}/{repo}/pulls/{number}/files",
    answer:
      "the remote: the files of the pull request's diff from its merge base, as the comparison lists them, paged, or GitHub's 404 for a pull request the world has not got",
    evidence: {
      world:
        "GitHub's REST list of the pull request's files, the read `gh pr view --json files` and `gh pr diff --name-only` make",
    },
  },
  {
    route: "GET repos/{owner}/{repo}/pulls/{number}/reviews",
    answer: "the world's reviews, then the reviews calls.jsonl records the run creating",
  },
  {
    route: "GET repos/{owner}/{repo}/pulls/{number}/reviews/{id}",
    answer: "the review calls.jsonl records the run creating, or the world's review by id, or 404",
    evidence: {
      run: "round17-forty/threads-closed-text-fixes-base-2/calls.jsonl:33",
      reason: "a reviewer reads one review back by its id",
    },
  },
  {
    route: "GET repos/{owner}/{repo}/pulls/{number}/reviews/{id}/comments",
    answer: "the comments of that review's threads: the world's, and the ones its create carried",
    evidence: {
      run: "round17-forty/reviewer-text-findings-text-only-head-2/calls.jsonl:26",
      reason: "a reviewer reads back the inline comments of the review it created",
    },
  },
  {
    route: "GET repos/{owner}/{repo}/pulls/{number}/comments",
    answer:
      "every comment of the review threads: the world's, then each one calls.jsonl records the run making",
    evidence: {
      run: "round17-forty/reviewer-text-findings-text-only-head-2/calls.jsonl:25",
      reason: "a reviewer lists the pull request's inline comments beside its threads",
    },
  },
  {
    route: "GET repos/{owner}/{repo}/issues/{number}/comments",
    answer: "the pull request's conversation: the comments calls.jsonl records the run posting",
    evidence: {
      run: "round17-forty/reviewer-text-findings-defect-base-2/calls.jsonl:29",
      reason: "a reviewer reads the pull request's conversation before it reviews",
    },
  },
  {
    route: "GET repos/{owner}/{repo}/commits/{ref}/check-runs",
    answer: "the world's check runs, reported on the ref the remote resolves, with its head_sha",
  },
  {
    route: "GET repos/{owner}/{repo}/commits/{ref}/status",
    answer: "the world's commit statuses, none, so `state` pending; the ref resolved on the remote",
  },
  {
    route: "GET repos/{owner}/{repo}/commits/{ref+}",
    answer:
      "the remote: the commit the ref resolves to, its parents, files and stats, or GitHub's 422",
    evidence: {
      run: "round29-world-health/94caf2e09844/calls.jsonl:8",
      reason: "a reviewer reads the commit the implementer pushed, for its author and parents",
    },
  },
  {
    route: "GET repos/{owner}/{repo}/commits",
    answer:
      "the remote: the commits the `sha` parameter's ref reaches, newest first, paged, or GitHub's 404",
    evidence: {
      world:
        "GitHub's list of the commits a branch reaches, the REST form of the history beside the read of one commit",
    },
  },
  {
    route: "GET repos/{owner}/{repo}/actions/runs",
    answer: "the world's workflow runs: none, its checks being check runs of no Actions workflow",
  },
  {
    route: "GET repos/{owner}/{repo}/actions/runs/{run_id}/jobs",
    answer: "404, as GitHub answers a run it has not got: the world holds no workflow run",
  },
  {
    route: "GET repos/{owner}/{repo}/rules/branches/{branch+}",
    answer:
      "the rules the base branch's one ruleset puts on it: the checks it requires, and review-thread resolution where the world requires it",
  },
  {
    route: "GET repos/{owner}/{repo}/rulesets",
    answer: "the repository's rulesets: the one on its base branch",
    evidence: {
      world:
        "the list of the rulesets whose id each rule of the base branch names, which a reviewer reaches from those rules",
    },
  },
  {
    route: "GET repos/{owner}/{repo}/rulesets/{id}",
    answer: "the base branch's ruleset, its conditions and its rules; GitHub's 404 for another id",
    evidence: {
      world:
        "the ruleset a rule of the base branch names by its id, which a reviewer reads for the rules it holds",
    },
  },
  {
    route: "GET repos/{owner}/{repo}/branches/{branch}/protection",
    answer:
      "GitHub's 403 to a role App, whose token may not read a repository's administration: `Resource not accessible by integration`",
    evidence: {
      world:
        "GitHub's protection read of the base branch the merge gate reads, which a reviewer reaches for its required checks",
    },
  },
  {
    route: "GET repos/{owner}/{repo}/branches/{branch+}",
    answer:
      "the remote: the base branch at its head, protected by its ruleset alone, so no classic protection and no check of its own required",
  },
  {
    route: "GET repos/{owner}/{repo}/branches",
    answer:
      "the remote's branches, each with its tip, the base alone protected, filtered by `protected`, paged",
    evidence: {
      world: "GitHub's list of the branches, beside the read of one branch the world serves",
    },
  },
  {
    route: "GET repos/{owner}/{repo}/tags",
    answer: "the remote's tags, newest version first: none in any world, so an empty list",
    evidence: {
      world: "GitHub's list of the tags, which it answers empty for a repository with none",
    },
  },
  {
    route: "GET repos/{owner}/{repo}/compare/{basehead+}",
    answer:
      "the remote: the comparison of the two refs, or GitHub's 404 for a ref it cannot resolve and for `<base>..<head>`",
  },
  {
    route: "GET repos/{owner}/{repo}/git/ref/{ref+}",
    answer: "the remote's refs and the pull request's two: the ref and the object it names, or 404",
  },
  {
    route: "GET repos/{owner}/{repo}/git/refs",
    answer: "the remote's refs and the pull request's two, by name",
    evidence: {
      world: "GitHub's list of every ref, the plural form of the ref read the world serves",
    },
  },
  {
    route: "GET repos/{owner}/{repo}/git/refs/{ref+}",
    answer:
      "the ref the path names, or the list of the refs its name starts, or GitHub's 404, as GitHub answers `git/refs/<ref>`",
    evidence: {
      world:
        "GitHub's read of one ref or of the refs under a name, the plural form of the ref read the world serves",
    },
  },
  {
    route: "GET repos/{owner}/{repo}/git/commits/{sha}",
    answer: "the remote: the commit's git data for a full SHA, or GitHub's 404",
    evidence: {
      world: "GitHub's git-data read of one commit, which it serves beside the read of a commit",
    },
  },
  {
    route: "GET repos/{owner}/{repo}/git/trees/{sha+}",
    answer: "the remote: the tree of the object the path names, recursive where asked, or 404",
    evidence: {
      world: "GitHub's read of a commit's tree, the files of a head listed without a checkout",
    },
  },
  {
    route: "GET repos/{owner}/{repo}/contents/{path+}",
    answer: "the remote: the file or directory at the ref, raw or as GitHub's JSON, or 404",
    evidence: {
      run: "not-counted-r24/threads-closed-text-fixes-head-3/calls.jsonl:16",
      reason: "a head reviewer re-reads the fixed file at the head through the API",
    },
  },
  {
    route: "GET user",
    answer:
      "GitHub's 403 to a role App, whose installation token is no user's: `Resource not accessible by integration`",
    evidence: { world: "the identity read every gh session can make, which GitHub refuses an App" },
  },
  {
    route: "GH auth status",
    answer:
      "gh's text for the pane's gh configuration: the account its hosts.yml names and the rig's token as gh masks it, no token read",
    evidence: { world: "the identity read every gh session can make" },
  },
  {
    route: "GH pr view",
    answer: "world.json and the remote: the pull request at the branch's tip",
  },
  { route: "GH pr list", answer: "world.json: the one pull request open on the issue branch" },
  {
    route: "GH pr diff",
    answer: "the remote: the issue branch against the base",
    evidence: {
      world:
        "gh's read of the pull request's diff, which round 25's agents made in two spellings no role text named, `gh pr diff <n>` and `gh pr diff <n> --color never` (production-shapes.json's `gh pr diff -` and `gh pr diff ---color`, their readings again of 2026-11-18); the one text that named the read, conflicts-and-rewrites.md's `gh pr diff <n> --name-only`, left with #4464",
    },
  },
  { route: "GH pr checks", answer: "the world's check runs of the branch's tip" },
  {
    route: "GH run view",
    answer: "gh's refusal: the world holds no workflow run, or no run named",
  },
  {
    route: "GH run watch",
    answer: "gh's refusal: the world holds no workflow run, or no run named",
  },
  {
    route: "GRAPHQL query repository.pullRequest.reviewThreads",
    answer: "the world's review threads and every call calls.jsonl records on them, paged",
  },
  {
    route: "GRAPHQL query viewer.login",
    answer: "world.json: the bot of the App the calling role's token is",
  },
  {
    route: "GRAPHQL query repository.pullRequest",
    answer: "world.json and the remote: the pull request at the branch's tip, in no merge queue",
  },
  {
    route: "GRAPHQL query repository.pullRequest.timelineItems",
    answer: "the world's timeline events: none",
  },
  // Writes.
  {
    route: "POST repos/{owner}/{repo}/pulls/{number}/reviews",
    answer: "the review the call creates, its state, head and body the call's own",
  },
  {
    route: "POST repos/{owner}/{repo}/pulls/{number}/reviews/{id}/events",
    answer: "the pending review calls.jsonl records the run creating, or 422",
    evidence: { world: "submits the pending review the world's own create leaves" },
  },
  {
    route: "PUT repos/{owner}/{repo}/pulls/{number}/reviews/{id}",
    answer:
      "the review with the body the call sends, which every later read of it reads; 404 for a review the pull request has not got, 422 for no body",
    evidence: {
      run: "round25-runs/1e2a0aeced4b/calls.jsonl:23",
      reason: "a reviewer rewrites its approval's body after it submitted it",
    },
  },
  {
    route: "PATCH repos/{owner}/{repo}/pulls/{number}",
    answer:
      "the pull request with the body the call leaves, as GitHub's REST API answers it; a PATCH of anything else is refused",
  },
  {
    route: "POST repos/{owner}/{repo}/pulls/{number}/comments/{id}/replies",
    answer:
      "the reply, in the thread of the comment the path names, replying to its first comment; 404 where no thread holds it",
    evidence: { world: "the REST form of the review-thread reply" },
  },
  {
    route: "POST repos/{owner}/{repo}/pulls/{number}/comments",
    answer:
      "the comment the call makes: a thread of its own, or a reply where it names in_reply_to",
    evidence: {
      run: "round17-forty/reviewer-text-findings-text-only-head-5/calls.jsonl:21",
      reason: "a reviewer posts an inline comment on its own",
    },
  },
  {
    route: "POST repos/{owner}/{repo}/issues/{number}/comments",
    answer: "the comment on the conversation, numbered after the run's earlier ones, and kept",
    evidence: { world: "the REST form of the conversation comment gh pr comment posts" },
  },
  {
    route: "GH pr comment",
    answer: "the comment's URL, its id the conversation's; the comment kept for the conversation",
  },
  {
    route: "GH pr edit",
    answer:
      "the pull request's URL; the body the call leaves kept; an edit of anything else refused",
    evidence: {
      run: "round17-forty/reviewer-text-findings-defect-base-4/calls.jsonl:32",
      reason: "a role writes the body's sections with gh pr edit",
    },
  },
  {
    route: "GRAPHQL mutation resolveReviewThread",
    answer: "the thread the call names, resolved, or GitHub's NOT_FOUND",
  },
  {
    route: "GRAPHQL mutation addPullRequestReviewThreadReply",
    answer: "the reply the call posts in the thread it names, or GitHub's NOT_FOUND",
    evidence: {
      run: "round17-forty/reviewer-text-findings-defect-base-5/calls.jsonl:34",
      reason: "a reviewer replies on a review thread through GraphQL",
    },
  },
];

/** The row of `WORLD_ROUTES` that answers `key`, or undefined: the first that matches, so a
 * narrower route stands ahead of a wider one it shares a prefix with. */
export function worldRouteOf(key: string): WorldRoute | undefined {
  return WORLD_ROUTES.find((row) => templateMatches(row.route, key));
}
