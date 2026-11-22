/**
 * The places in a hook's own code where it can wait on something outside itself: every module of
 * this repository the hook's entry reaches through its imports (type-only imports dropped, every
 * file under a `node_modules` directory left out), read with the TypeScript parser, and in each the
 * calls that start a process, fetch, open a socket or read a stream.
 *
 * A site is bounded where its call carries a bound the timed-wait census (`hook-timed-waits.ts`)
 * can see: a `timeout` option on a process start, or a `signal` (an `AbortSignal.timeout`, or one
 * derived from it) on a process start or a fetch. Every other site is unbounded here, and a row
 * names each one that no reviewed entry of its own names the bound of.
 *
 * What a site is: a call of `Bun.spawn` or `Bun.spawnSync`; a call of a function imported from
 * `node:child_process`, or of a `promisify` of one; a call of `fetch`, or of any method or private
 * field named `fetch`; `Bun.connect`, `Bun.listen`, `connect` or `createConnection` of `node:net`,
 * `new WebSocket`, `new Socket`; a read of `Bun.stdin` or `process.stdin`; `new Response(x)` over a
 * process's `stdout`, `stderr` or `stdin`; `getReader()`; and every `for await` loop. A response
 * body read after a fetch carries that fetch's signal and is not a site of its own.
 */
import { readFileSync, realpathSync } from "node:fs"
import { dirname, relative } from "node:path"
import ts from "typescript"

export interface WaitSite {
  /** Its file, relative to the repository's root. */
  readonly file: string
  readonly line: number
  readonly kind: string
  /** The call's first line, trimmed. */
  readonly text: string
  /** The bound its call carries, or undefined where it carries none the census sees. */
  readonly boundedBy: "timeout" | "signal" | undefined
}

/** The bound a call's arguments carry: an identifier or property named `timeout`, else one named
 *  `signal`, anywhere in them (a property, a shorthand, a variable passed on). */
function carriedBound(nodes: readonly ts.Node[]): WaitSite["boundedBy"] {
  const found = { timeout: false, signal: false }
  const visit = (node: ts.Node): void => {
    if (ts.isIdentifier(node) || ts.isPrivateIdentifier(node)) {
      if (node.text === "timeout") found.timeout = true
      if (node.text === "signal") found.signal = true
    }
    ts.forEachChild(node, visit)
  }
  for (const node of nodes) visit(node)
  if (found.timeout) return "timeout"
  if (found.signal) return "signal"
  return undefined
}

/** The modules `entry` reaches through its non-type imports, inside `root` and outside every
 *  `node_modules` directory, `entry` first. */
export function reachedModules(entry: string, root: string): string[] {
  const transpiler = new Bun.Transpiler({ loader: "ts" })
  const realRoot = realpathSync(root)
  const seen: string[] = []
  const queue = [realpathSync(entry)]
  while (queue.length > 0) {
    const file = queue.shift() as string
    if (seen.includes(file)) continue
    seen.push(file)
    for (const { path } of transpiler.scanImports(readFileSync(file, "utf8"))) {
      if (path.startsWith("node:") || path === "bun" || path.startsWith("bun:")) continue
      let resolved: string
      try {
        resolved = realpathSync(Bun.resolveSync(path, dirname(file)))
      } catch {
        continue
      }
      if (!resolved.startsWith(`${realRoot}/`) || resolved.includes("/node_modules/")) continue
      if (/\.(ts|tsx|js|mjs)$/.test(resolved)) queue.push(resolved)
    }
  }
  return seen
}

/** Every wait site of `file`, with `root` the repository's root its path is given against. */
export function waitSites(file: string, root: string): WaitSite[] {
  const text = readFileSync(file, "utf8")
  const source = ts.createSourceFile(file, text, ts.ScriptTarget.Latest, true, ts.ScriptKind.TS)
  const name = relative(realpathSync(root), file)
  // The local names of `node:child_process`'s functions, of `promisify` of one, and of `node:net`'s.
  const processStarters = new Set<string>()
  const netOpeners = new Set<string>()
  // Local names a fetch is held under: `const send = options.fetch ?? fetch`.
  const fetchers = new Set<string>(["fetch"])
  const sites: WaitSite[] = []
  const site = (node: ts.Node, kind: string, boundedBy: WaitSite["boundedBy"]): void => {
    const start = source.getLineAndCharacterOfPosition(node.getStart(source))
    const first = node.getText(source).split("\n")[0]?.trim() ?? ""
    sites.push({ file: name, line: start.line + 1, kind, text: first.slice(0, 120), boundedBy })
  }
  for (const statement of source.statements) {
    if (!ts.isImportDeclaration(statement) || !ts.isStringLiteral(statement.moduleSpecifier))
      continue
    const from = statement.moduleSpecifier.text.replace(/^node:/, "")
    const target =
      from === "child_process" ? processStarters : from === "net" ? netOpeners : undefined
    const bindings = statement.importClause?.namedBindings
    if (target === undefined || bindings === undefined || !ts.isNamedImports(bindings)) continue
    for (const element of bindings.elements) target.add(element.name.text)
  }
  const visit = (node: ts.Node): void => {
    if (ts.isVariableDeclaration(node) && ts.isIdentifier(node.name) && node.initializer) {
      const initializer = node.initializer
      if (
        ts.isCallExpression(initializer) &&
        initializer.expression.getText(source) === "promisify" &&
        initializer.arguments.some((argument) => processStarters.has(argument.getText(source)))
      ) {
        processStarters.add(node.name.text)
      }
      // An initializer that names a fetch without calling it or wrapping it in a function of its
      // own, whose calls inside are sites already.
      if (
        !ts.isCallExpression(initializer) &&
        !ts.isArrowFunction(initializer) &&
        !ts.isFunctionExpression(initializer) &&
        /(^|[^\w#])#?fetch\b/.test(initializer.getText(source))
      ) {
        fetchers.add(node.name.text)
      }
    }
    if (ts.isCallExpression(node)) {
      const callee = node.expression.getText(source)
      const last = ts.isPropertyAccessExpression(node.expression)
        ? node.expression.name.text
        : ts.isIdentifier(node.expression)
          ? node.expression.text
          : ""
      const bound = carriedBound(node.arguments)
      if (callee === "Bun.spawn" || callee === "Bun.spawnSync") {
        site(node, callee, bound)
      } else if (ts.isIdentifier(node.expression) && processStarters.has(last)) {
        site(node, "child_process", bound)
      } else if (
        last.replace(/^#/, "") === "fetch" ||
        (ts.isIdentifier(node.expression) && fetchers.has(last))
      ) {
        site(node, "fetch", bound === "signal" ? "signal" : undefined)
      } else if (
        callee === "Bun.connect" ||
        callee === "Bun.listen" ||
        (ts.isIdentifier(node.expression) && netOpeners.has(last))
      ) {
        site(node, "socket", undefined)
      } else if (last === "getReader" && !/\.body\??\.getReader$/.test(callee)) {
        // A fetch's own response body reads under that fetch's signal: no site of its own.
        site(node, "stream read", undefined)
      }
    }
    if (ts.isNewExpression(node)) {
      const constructed = node.expression.getText(source)
      if (/(^|\.)(WebSocket|Socket)$/.test(constructed)) site(node, "socket", undefined)
      if (
        constructed === "Response" &&
        node.arguments?.some((argument) =>
          /\.(stdout|stderr|stdin)$/.test(argument.getText(source)),
        )
      ) {
        site(node, "stream read", undefined)
      }
    }
    // `Bun.stdin.text()`, `process.stdin.on(…)`: the read is the access's parent.
    if (
      ts.isPropertyAccessExpression(node) &&
      /^(Bun|process)\.stdin$/.test(node.expression.getText(source))
    ) {
      site(node.parent, "stream read", undefined)
    }
    if (ts.isForOfStatement(node) && node.awaitModifier !== undefined) {
      site(node, "for await", undefined)
    }
    ts.forEachChild(node, visit)
  }
  visit(source)
  return sites
}
