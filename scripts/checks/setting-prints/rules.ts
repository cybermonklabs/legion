/**
 * The rules of the language and of the libraries by which the search's analysis reads a value:
 * which calls end a value's path, judge it, take it as a URL, keep its text, print it or start a
 * child with it, and the syntax it recognizes them in.
 */
import ts from "typescript";

/** The functions that end a value's path: the one naming function of a configured URL and of a
 *  request built from one (`urlForText`, `requestForText`), and the words a refusal names a value
 *  it hides by (`unshownSettingUrl`). `shownSettingUrl` is no such function: it decides whether a
 *  value is admitted, and what it answers can still hold a piece of a password, so a line that
 *  prints its answer prints the setting. */
export const SANITIZERS = new Set(["urlForText", "requestForText", "unshownSettingUrl"]);
/** The calls whose answer, read by a guard, admits a value to a `new URL(…)` of it. */
export const JUDGES = new Set(["shownSettingUrl", "canParse"]);
/** Calls and classes that take their argument as a URL, or hand it to another program. */
export const URL_TAKERS = new Set([
  "URL",
  "canParse",
  "fetch",
  "Request",
  "WebSocket",
  "EventSource",
  "connect",
  "postgres",
  "Client",
  "Pool",
  "SQL",
  "shownSettingUrl",
  "unshownSettingUrl",
  "urlForText",
  "requestForText",
]);

/** Methods whose answer holds their receiver's text: of a string, an array, a map, a URL. */
export const KEEPS_RECEIVER = new Set([
  "at",
  "charAt",
  "concat",
  "entries",
  "fill",
  "filter",
  "find",
  "findLast",
  "flat",
  "get",
  "getAll",
  "join",
  "json",
  "keys",
  "match",
  "matchAll",
  "normalize",
  "padEnd",
  "padStart",
  "pop",
  "repeat",
  "replace",
  "replaceAll",
  "reverse",
  "shift",
  "slice",
  "sort",
  "splice",
  "split",
  "substr",
  "substring",
  "text",
  "toLocaleLowerCase",
  "toLocaleString",
  "toLocaleUpperCase",
  "toLowerCase",
  "toReversed",
  "toSorted",
  "toString",
  "toUpperCase",
  "trim",
  "trimEnd",
  "trimStart",
  "values",
  "valueOf",
  "with",
  "catch",
  "finally",
]);
/** Methods that also take text from their arguments. */
export const KEEPS_ARGUMENTS = new Set([
  "concat",
  "replace",
  "replaceAll",
  "padEnd",
  "padStart",
  "with",
  "splice",
  "fill",
]);
/** Methods whose answer is what their callback returns, given each element of the receiver. */
export const MAPPERS = new Set(["map", "flatMap", "then"]);
/** Methods that hand each element of their receiver to their callback. */
export const VISITORS = new Set([
  "map",
  "flatMap",
  "forEach",
  "filter",
  "find",
  "findLast",
  "findIndex",
  "findLastIndex",
  "some",
  "every",
  "sort",
  "toSorted",
  "then",
]);
/** Methods that put their arguments into their receiver. */
export const MUTATORS = new Set(["push", "unshift", "add", "set", "append", "splice", "fill"]);
/** Library functions whose answer holds their arguments' text. */
export const KEEPS = new Set([
  "String",
  "encodeURIComponent",
  "encodeURI",
  "decodeURIComponent",
  "decodeURI",
  "structuredClone",
  "btoa",
  "atob",
  "escape",
  "unescape",
  "entries",
  "values",
  "fromEntries",
  "assign",
  "freeze",
  "from",
  "of",
  "stringify",
  "parse",
  "safeParse",
  "parseAsync",
  "safeParseAsync",
  "parseArgs",
  "resolve",
  "all",
  "allSettled",
  "race",
  "any",
  "join",
  "relative",
  "dirname",
  "basename",
  "extname",
  "normalize",
  "format",
  "inspect",
  "fileURLToPath",
  "pathToFileURL",
  "exec",
  "dedent",
]);
/** Calls whose answer is text made from their arguments or receiver: a line that writes the value. */
export const RENDERS = new Set(["String", "join", "toString", "stringify", "format", "inspect"]);
/** Classes whose instance holds the text it was made from. */
export const KEEPING_CLASSES = new Set([
  "URL",
  "URLSearchParams",
  "Map",
  "Set",
  "Headers",
  "Array",
  "String",
]);
/** A call of one of these, with no body the search can read, prints or logs its arguments. */
export const LOG_NAMES = new Set([
  "log",
  "info",
  "warn",
  "warning",
  "error",
  "debug",
  "trace",
  "fatal",
  "notice",
  "print",
  "println",
  "say",
  "report",
  "die",
  "abort",
  "panic",
  "complain",
  "usage",
  "refuse",
  "setFailed",
]);
/** A call of one of these writes its arguments into a child's command line. */
export const COMMANDS = new Set([
  "spawn",
  "spawnSync",
  "exec",
  "execSync",
  "execFile",
  "execFileSync",
]);

/** Whether `callee` starts a child: one of `COMMANDS` called by its name, or as a member of the
 *  child-process module, but not a regular expression's `exec`. */
export function childCommand(callee: ts.Expression): boolean {
  const e = strip(callee);
  if (!COMMANDS.has(nameOf(e))) return false;
  return (
    !ts.isPropertyAccessExpression(e) ||
    /^(child_process|childProcess|cp)$/.test(nameOf(e.expression))
  );
}
/** A call of one of these reads a file. */
export const FILE_READS = new Set(["readFileSync", "readFile"]);

export type FunctionWithBody = ts.SignatureDeclaration & { readonly body?: ts.Node };

export function withBody(node: ts.Node): node is FunctionWithBody {
  return (
    (ts.isFunctionDeclaration(node) ||
      ts.isFunctionExpression(node) ||
      ts.isArrowFunction(node) ||
      ts.isMethodDeclaration(node) ||
      ts.isConstructorDeclaration(node) ||
      ts.isGetAccessorDeclaration(node) ||
      ts.isSetAccessorDeclaration(node)) &&
    node.body !== undefined
  );
}

export function strip(expression: ts.Expression): ts.Expression {
  let e = expression;
  while (
    ts.isParenthesizedExpression(e) ||
    ts.isAsExpression(e) ||
    ts.isNonNullExpression(e) ||
    ts.isTypeAssertionExpression(e) ||
    ts.isSatisfiesExpression(e) ||
    ts.isAwaitExpression(e)
  ) {
    e = e.expression;
  }
  return e;
}

export function nameOf(expression: ts.Expression): string {
  const e = strip(expression);
  if (ts.isIdentifier(e)) return e.text;
  if (ts.isPropertyAccessExpression(e)) return e.name.text;
  if (ts.isElementAccessExpression(e) && ts.isStringLiteral(e.argumentExpression)) {
    return e.argumentExpression.text;
  }
  return "";
}

/** `process.env`, `Bun.env` and `import.meta.env`, the environment as an object. */
export function environment(expression: ts.Expression): boolean {
  const e = strip(expression);
  if (!ts.isPropertyAccessExpression(e) || e.name.text !== "env") return false;
  const owner = strip(e.expression);
  if (ts.isIdentifier(owner)) return owner.text === "process" || owner.text === "Bun";
  if (ts.isMetaProperty(owner)) return true;
  return (
    ts.isPropertyAccessExpression(owner) &&
    owner.name.text === "process" &&
    ts.isIdentifier(owner.expression) &&
    owner.expression.text === "globalThis"
  );
}

/** `process.argv` and `Bun.argv`, the command line. */
export function commandLine(expression: ts.Expression): boolean {
  const e = strip(expression);
  return (
    ts.isPropertyAccessExpression(e) &&
    e.name.text === "argv" &&
    ts.isIdentifier(e.expression) &&
    (e.expression.text === "process" || e.expression.text === "Bun")
  );
}

/** The key a member read names: a property's name, a literal index's text, or undefined. */
export function keyOf(
  expression: ts.PropertyAccessExpression | ts.ElementAccessExpression
): string | undefined {
  if (ts.isPropertyAccessExpression(expression)) return expression.name.text;
  const key = strip(expression.argumentExpression);
  return ts.isStringLiteralLike(key) || ts.isNumericLiteral(key) ? key.text : undefined;
}
