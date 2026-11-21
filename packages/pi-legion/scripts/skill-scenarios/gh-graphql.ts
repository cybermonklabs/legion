// The GraphQL half of the gh stand-in (gh-standin.ts): a `gh api graphql` call's query, read as
// GitHub reads it, and answered from the world a route holds, field by field, as GitHub answers it.
// A route that answers GraphQL holds the world as a tree (`graphql` in its answer): the fields the
// world holds, under the names GitHub gives them. A field with arguments that pick what it is (a
// repository by its owner and name, a pull request by its number, an object by its oid or its
// expression) is held under its name and those arguments, `object(oid:"<sha>")`, in the canonical
// form below, or under `name(*)` for every other value of them; a field whose arguments only page
// it (`first`, `last`, `after`, `before`) is held under its bare name. A node held once and read
// under several names is written once under `$refs` and named by `{"$ref": <name>}`; a node the
// stand-in keeps (the review threads) is named by `{"$keep": <name>}`; and a name GitHub cannot
// resolve is `{"$error": {type, message}}`, whose message may name the field's arguments as
// `{<argument>}`. The answer is the query's own selection of that tree and nothing more, in the
// order the query asks: a field the tree does not hold, a fragment on a node with no `__typename`,
// a selection on a scalar or none on an object, two selections answering one key, a page the tree
// cannot state (a slice that leaves nodes out where the query reads the page, a page before a
// cursor or after one that is not the last node's), a fragment spread, a directive, a variable
// the query uses and does not declare, a value or a type the rig cannot type, a revision
// expression (`<branch>~1`), a mutation: each is a request the rig does not model (Unmodeled),
// which the stand-in refuses. An `$error` answers GitHub's: null where the field stands, and the
// error with its path and its place in the query.
// A connection's `pageInfo.endCursor` is GitHub's cursor of the page's last node, base64 of its
// 1-based index, unpadded ("NjQ" for the 64th), as a tree holds it; `after` that cursor answers
// GitHub's empty page, no node and `endCursor` null (measured, github-answers/README.txt).
// GitHub refuses some requests before it reads anything, and its body is then the errors alone
// (measured, github-answers/graphql-*): a variable of a type `T!` given no value or null, a
// String given for an Int (in a variable or written in the query), and a variable the query
// declares and does not use; parse holds them in the document's `errors`. A variable the call
// gives and the query does not declare is ignored, as GitHub ignores it (measured). A
// repository's owner and name match without regard to case, as GitHub's do (measured): a tree
// holds them in lower case.

/** A request the rig does not model, which the stand-in refuses as such. */
export class Unmodeled extends Error {}

type Scalar = null | boolean | number | string;
/** An argument's value with its variables put in: an enum value stays a name of its own. */
export type Value = Scalar | { enum: string } | Value[] | { [key: string]: Value };
export type Selection =
  | {
      kind: "field";
      alias: string | undefined;
      name: string;
      args: Record<string, Value>;
      sub: Selection[] | undefined;
      line: number;
      column: number;
    }
  | { kind: "fragment"; on: string; sub: Selection[] };
/** An error of a request GitHub refuses before it reads anything, as its body holds it. */
export type RequestError = Record<string, unknown>;
export type Document = { selections: Selection[]; errors: RequestError[] };

/** The type GitHub's schema gives each argument of the fields the worlds hold, by field and
 * argument; the paging arguments are any connection's. An argument of no type here is not typed. */
const SLOTS: Record<string, Record<string, string>> = {
  repository: { owner: "String!", name: "String!" },
  pullRequest: { number: "Int!" },
  object: { oid: "GitObjectID", expression: "String" },
  ref: { qualifiedName: "String!" },
};
const PAGE_SLOTS: Record<string, string> = {
  first: "Int",
  last: "Int",
  after: "String",
  before: "String",
};
const slotOf = (field: string, argument: string): string | undefined =>
  SLOTS[field]?.[argument] ?? PAGE_SLOTS[argument];
/** Whether a value is of a scalar type: true, false where GitHub's refusal is measured (a String
 * for an Int), and undefined where the rig cannot type it (not modeled). */
function fits(type: string, value: Value): boolean | undefined {
  if (type === "Int") {
    if (typeof value === "number" && Number.isInteger(value)) return true;
    return typeof value === "string" ? false : undefined;
  }
  if (type === "String" || type === "GitObjectID") return typeof value === "string" || undefined;
  if (type === "Boolean") return typeof value === "boolean" || undefined;
  return undefined;
}

type Token = {
  kind: "name" | "punct" | "string" | "number";
  text: string;
  line: number;
  column: number;
};

function tokens(text: string): Token[] {
  const out: Token[] = [];
  let line = 1;
  let lineStart = 0;
  let i = 0;
  while (i < text.length) {
    const c = text[i] as string;
    if (c === "\n") {
      line += 1;
      i += 1;
      lineStart = i;
      continue;
    }
    if (/[\s,\ufeff]/.test(c)) {
      i += 1;
      continue;
    }
    if (c === "#") {
      while (i < text.length && text[i] !== "\n") i += 1;
      continue;
    }
    const column = i - lineStart + 1;
    if (text.startsWith("...", i)) {
      out.push({ kind: "punct", text: "...", line, column });
      i += 3;
    } else if ("{}():!=$[]@|&".includes(c)) {
      out.push({ kind: "punct", text: c, line, column });
      i += 1;
    } else if (c === '"') {
      if (text.startsWith('"""', i)) throw new Unmodeled("a block string");
      let j = i + 1;
      while (j < text.length && text[j] !== '"') j += text[j] === "\\" ? 2 : 1;
      if (j >= text.length) throw new Unmodeled("an unterminated string");
      out.push({ kind: "string", text: JSON.parse(text.slice(i, j + 1)), line, column });
      i = j + 1;
    } else if (/[-0-9]/.test(c)) {
      const m = /^-?\d+(?:\.\d+)?(?:[eE][+-]?\d+)?/.exec(text.slice(i));
      if (!m) throw new Unmodeled(`an unexpected ${c}`);
      out.push({ kind: "number", text: m[0], line, column });
      i += m[0].length;
    } else if (/[_A-Za-z]/.test(c)) {
      const m = /^[_A-Za-z][_0-9A-Za-z]*/.exec(text.slice(i)) as RegExpExecArray;
      out.push({ kind: "name", text: m[0], line, column });
      i += m[0].length;
    } else throw new Unmodeled(`an unexpected ${c}`);
  }
  return out;
}

/** The query `text` of a call, its variables put in from `variables`, and the errors of a request
 * GitHub refuses before it reads anything (the header). */
export function parse(text: string, variables: Record<string, unknown>): Document {
  const ts = tokens(text);
  let at = 0;
  const peek = () => ts[at];
  const next = () => {
    const t = ts[at];
    if (t === undefined) throw new Unmodeled("a query that ends early");
    at += 1;
    return t;
  };
  const expect = (text: string) => {
    const t = next();
    if (t.text !== text || t.kind === "string") throw new Unmodeled(`${t.text} where ${text} goes`);
    return t;
  };
  const errors: RequestError[] = [];
  /** The variables the operation declares, each with its type, the place of its `$` and whether
   * the query uses it; and the value each takes. */
  const declared = new Map<string, { type: string; line: number; column: number; used: boolean }>();
  const values: Record<string, Value> = {};
  const variable = (): { type: string; value: Value } => {
    const name = next();
    const held = declared.get(name.text);
    if (name.kind !== "name" || held === undefined)
      throw new Unmodeled("a variable the query does not declare");
    held.used = true;
    return { type: held.type, value: values[name.text] ?? null };
  };
  const value = (): Value => {
    const t = next();
    if (t.kind === "string") return t.text;
    if (t.kind === "number") return Number(t.text);
    if (t.kind === "punct" && t.text === "$") return variable().value;
    if (t.kind === "punct" && t.text === "[") {
      const list: Value[] = [];
      while (peek()?.text !== "]") list.push(value());
      expect("]");
      return list;
    }
    if (t.kind === "punct" && t.text === "{") {
      const object: Record<string, Value> = {};
      while (peek()?.text !== "}") {
        const key = next().text;
        expect(":");
        object[key] = value();
      }
      expect("}");
      return object;
    }
    if (t.kind === "name") {
      if (t.text === "true") return true;
      if (t.text === "false") return false;
      if (t.text === "null") return null;
      return { enum: t.text };
    }
    throw new Unmodeled(`a value ${t.text}`);
  };
  let anonymous = true;
  /** The argument `key` of field `field`, checked against its type: a variable of another type is
   * not modeled, and a literal of another type is GitHub's error where it is measured (a String
   * for an Int, under a path of field names alone, in an anonymous query). `path` is the field
   * names down to the field, or null under an alias or a fragment. */
  const argument = (field: Token, key: string, path: string[] | null): Value => {
    const slot = slotOf(field.text, key);
    if (peek()?.kind === "punct" && peek()?.text === "$") {
      next();
      const given = variable();
      if (slot !== undefined && given.type !== slot && given.type !== `${slot}!`)
        throw new Unmodeled("a variable of another type than its argument");
      return given.value;
    }
    const literal = value();
    if (slot === undefined) return literal;
    if (literal === null) {
      if (slot.endsWith("!")) throw new Unmodeled("null written for a value of a type `T!`");
      return literal;
    }
    const fit = fits(slot.replace(/!$/, ""), literal);
    if (fit === undefined) throw new Unmodeled(`a value the rig cannot type as ${slot}`);
    if (!fit) {
      if (path === null || !anonymous) throw new Unmodeled("a mistyped argument under an alias");
      errors.push({
        path: ["query", ...path, key],
        extensions: { code: "argumentLiteralsIncompatible", typeName: "Field", argumentName: key },
        locations: [{ line: field.line, column: field.column }],
        message: `Argument '${key}' on Field '${field.text}' has an invalid value (${render(literal)}). Expected type '${slot}'.`,
      });
    }
    return literal;
  };
  const selections = (path: string[] | null): Selection[] => {
    expect("{");
    const out: Selection[] = [];
    while (peek()?.text !== "}") {
      const t = next();
      if (t.kind === "punct" && t.text === "...") {
        const on = next();
        if (on.text !== "on") throw new Unmodeled("a fragment spread");
        const type = next().text;
        if (peek()?.text === "@") throw new Unmodeled("a directive");
        out.push({ kind: "fragment", on: type, sub: selections(null) });
        continue;
      }
      if (t.kind !== "name") throw new Unmodeled(`${t.text} where a field goes`);
      let alias: string | undefined;
      let field = t;
      if (peek()?.text === ":") {
        next();
        alias = t.text;
        field = next();
      }
      const name = field.text;
      const below = path === null || alias !== undefined ? null : [...path, name];
      const args: Record<string, Value> = {};
      if (peek()?.text === "(") {
        next();
        while (peek()?.text !== ")") {
          const key = next().text;
          expect(":");
          args[key] = argument(field, key, below);
        }
        expect(")");
      }
      if (peek()?.text === "@") throw new Unmodeled("a directive");
      const sub = peek()?.text === "{" ? selections(below) : undefined;
      out.push({ kind: "field", alias, name, args, sub, line: t.line, column: t.column });
    }
    expect("}");
    return out;
  };
  const first = peek();
  if (first?.kind === "name") {
    if (first.text === "fragment") throw new Unmodeled("a fragment definition");
    // A mutation or a subscription changes or watches what no world models.
    if (next().text !== "query") throw new Unmodeled(`a ${first.text}`);
    if (peek()?.kind === "name") {
      next();
      anonymous = false;
    }
    if (peek()?.text === "(") {
      next();
      while (peek()?.text !== ")") {
        const dollar = expect("$");
        const name = next();
        expect(":");
        const type = next();
        if (name.kind !== "name" || type.kind !== "name")
          throw new Unmodeled("a variable whose type is not a name (a list)");
        const nonNull = peek()?.text === "!";
        if (nonNull) next();
        if (peek()?.text === "=" || peek()?.text === "@")
          throw new Unmodeled("a default value or a directive of a variable");
        if (declared.has(name.text)) throw new Unmodeled("a variable declared twice");
        declared.set(name.text, {
          type: `${type.text}${nonNull ? "!" : ""}`,
          line: dollar.line,
          column: dollar.column,
          used: false,
        });
      }
      expect(")");
    }
    if (peek()?.text === "@") throw new Unmodeled("a directive");
  }
  // The value of each variable the operation declares: GitHub's error for none (or null) where
  // its type is `T!`, or for a value its type does not take (measured for a String given for an
  // Int; another it cannot type is not modeled). A variable the call gives that the operation
  // does not declare is ignored, as GitHub ignores it (measured).
  for (const [name, held] of declared) {
    const given = (variables[name] ?? null) as Value;
    const invalid = (value: Value, explanation: string) =>
      errors.push({
        extensions: { value, problems: [{ path: [], explanation }] },
        locations: [{ line: held.line, column: held.column }],
        message: `Variable $${name} of type ${held.type} was provided invalid value`,
      });
    values[name] = given;
    if (given === null) {
      if (held.type.endsWith("!")) invalid(null, "Expected value to not be null");
      continue;
    }
    const base = held.type.replace(/!$/, "");
    const fit = fits(base, given);
    if (fit === undefined) throw new Unmodeled(`a value the rig cannot type as ${held.type}`);
    if (!fit) invalid(given, `Could not coerce value ${render(given)} to ${base}`);
  }
  const document = { selections: selections(anonymous ? [] : null), errors };
  if (at !== ts.length) throw new Unmodeled("a second definition");
  // A variable the operation declares and the query does not use: GitHub's error (measured).
  for (const [name, held] of declared)
    if (!held.used) {
      if (!anonymous || first === undefined)
        throw new Unmodeled("an unused variable of a named query");
      errors.push({
        path: ["query"],
        extensions: { code: "variableNotUsed", variableName: name },
        locations: [{ line: first.line, column: first.column }],
        message: `Variable $${name} is declared by anonymous query but not used`,
      });
    }
  // Two errors GitHub may order or join in a way not measured.
  if (errors.length > 1) throw new Unmodeled("a request GitHub refuses for two reasons");
  return document;
}

/** A value as the canonical text writes it. */
function render(value: Value): string {
  if (value === null || typeof value !== "object") return JSON.stringify(value);
  if (Array.isArray(value)) return `[${value.map(render).join(",")}]`;
  if ("enum" in value && typeof value.enum === "string" && Object.keys(value).length === 1)
    return value.enum;
  return `{${Object.keys(value)
    .sort()
    .map((key) => `${key}:${render((value as Record<string, Value>)[key] as Value)}`)
    .join(",")}}`;
}
/** The arguments of a field that pick what it is, as the tree keys it: `name(a:1,b:"x")`; a
 * repository's owner and name in lower case, since GitHub matches them without regard to case. */
function keyOf(name: string, args: Record<string, Value>): string {
  const picking = Object.keys(args)
    .filter((key) => PAGE_SLOTS[key] === undefined)
    .sort();
  if (picking.length === 0) return name;
  const folded = (key: string, value: Value) =>
    name === "repository" && (key === "owner" || key === "name") && typeof value === "string"
      ? value.toLowerCase()
      : value;
  return `${name}(${picking.map((key) => `${key}:${render(folded(key, args[key] as Value))}`).join(",")})`;
}

/** The query as routes match it: no aliases, no whitespace but one space between selections,
 * every variable put in, arguments sorted. */
export function canonical(document: Document): string {
  const set = (selections: Selection[]): string =>
    `{${selections
      .map((s) =>
        s.kind === "fragment"
          ? `...on ${s.on}${set(s.sub)}`
          : `${s.name}${
              Object.keys(s.args).length === 0
                ? ""
                : `(${Object.keys(s.args)
                    .sort()
                    .map((key) => `${key}:${render(s.args[key] as Value)}`)
                    .join(",")})`
            }${s.sub === undefined ? "" : set(s.sub)}`
      )
      .join(" ")}}`;
  return set(document.selections);
}

type Tree = unknown;
export type GraphqlError = {
  type: string;
  path: (string | number)[];
  locations: { line: number; column: number }[];
  message: string;
};

/** The query's own selection of `tree` (the route's `graphql`), with `$refs` and the stand-in's
 * `keep` resolving the names a tree gives its shared nodes; and the errors GitHub would give. */
export function project(
  document: Document,
  tree: Tree,
  refs: Record<string, Tree>,
  keep: (marker: Record<string, unknown>) => Tree
): { data: unknown; errors: GraphqlError[] } {
  const errors: GraphqlError[] = [];
  const resolve = (node: Tree): Tree => {
    let at = node;
    for (let hops = 0; hops < 8; hops += 1) {
      if (typeof at !== "object" || at === null || Array.isArray(at)) return at;
      if ("$ref" in at) {
        const named = refs[String((at as { $ref: unknown }).$ref)];
        if (named === undefined) throw new Unmodeled("a node the tree names and does not hold");
        at = named;
      } else if ("$keep" in at) at = keep(at as Record<string, unknown>);
      else return at;
    }
    throw new Unmodeled("a node that names itself");
  };
  const page = (node: Record<string, Tree>, args: Record<string, Value>, sub: Selection[]) => {
    if ((args.before ?? null) !== null) throw new Unmodeled("a page before a cursor");
    const { first, last } = args;
    if ((args.after ?? null) !== null) {
      // After the cursor of the last node, GitHub's page holds no node and no cursor (measured).
      const info = resolve(node.pageInfo);
      if (
        last !== undefined ||
        !Array.isArray(node.nodes) ||
        typeof info !== "object" ||
        info === null ||
        Array.isArray(info) ||
        Object.keys(info).some((name) => name !== "hasNextPage" && name !== "endCursor") ||
        (info as Record<string, Tree>).endCursor !== args.after
      )
        throw new Unmodeled("a page after a cursor that is not the last node's");
      return { ...node, nodes: [], pageInfo: { hasNextPage: false, endCursor: null } };
    }
    if (first === undefined && last === undefined) return node;
    if (!Array.isArray(node.nodes)) throw new Unmodeled("a page of a field that is no connection");
    const all = node.nodes as Tree[];
    const count = typeof first === "number" ? first : typeof last === "number" ? last : all.length;
    if (count >= all.length) return node;
    if (sub.some((s) => s.kind === "field" && s.name === "pageInfo"))
      throw new Unmodeled("a page that leaves nodes out, read with its page info");
    return { ...node, nodes: typeof first === "number" ? all.slice(0, count) : all.slice(-count) };
  };
  const select = (node: Tree, selections: Selection[], path: (string | number)[]): unknown => {
    const at = resolve(node);
    if (at === null) return null;
    if (Array.isArray(at))
      return at.map((item, index) => select(item, selections, [...path, index]));
    if (typeof at !== "object") throw new Unmodeled("a selection on a scalar");
    const object = at as Record<string, Tree>;
    const out: Record<string, unknown> = {};
    const fill = (from: Selection[]) => {
      for (const s of from) {
        if (s.kind === "fragment") {
          if (typeof object.__typename !== "string")
            throw new Unmodeled("a fragment on an untyped node");
          if (object.__typename === s.on) fill(s.sub);
          continue;
        }
        const key = s.alias ?? s.name;
        if (key in out) throw new Unmodeled("two selections of one key");
        const held = keyOf(s.name, s.args);
        // A revision expression names a commit by its history, which no tree holds.
        if (s.name === "object" && /[~^:@]|\.\./.test(String(s.args.expression ?? "")))
          throw new Unmodeled("a revision expression");
        let value = object[held];
        if (value === undefined && held !== s.name) value = object[`${s.name}(*)`];
        if (value === undefined) throw new Unmodeled(`a field the world does not hold: ${held}`);
        value = resolve(value);
        if (
          typeof value === "object" &&
          value !== null &&
          !Array.isArray(value) &&
          "$error" in value
        ) {
          const { type, message } = (value as { $error: { type: string; message: string } }).$error;
          errors.push({
            type,
            path: [...path, key],
            locations: [{ line: s.line, column: s.column }],
            message: message.replace(/\{(\w+)\}/g, (named, arg: string) =>
              s.args[arg] === undefined ? named : String(s.args[arg] as Scalar)
            ),
          });
          out[key] = null;
          continue;
        }
        if (s.sub === undefined) {
          const leaf = Array.isArray(value)
            ? value.every((v) => typeof v !== "object" || v === null)
            : typeof value !== "object" || value === null;
          if (!leaf) throw new Unmodeled("an object read with no selection");
          out[key] = value;
          continue;
        }
        if (value === null) {
          out[key] = null;
          continue;
        }
        if (typeof value !== "object") throw new Unmodeled("a selection on a scalar");
        const paged = Array.isArray(value)
          ? value
          : page(value as Record<string, Tree>, s.args, s.sub);
        out[key] = select(paged, s.sub, [...path, key]);
      }
    };
    fill(selections);
    return out;
  };
  const data = select(tree, document.selections, []);
  return { data, errors };
}
