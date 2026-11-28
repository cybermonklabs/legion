/**
 * The places in a hook's own code where it can wait on something outside itself: every module of
 * this repository the hook's entry reaches through its imports (type-only imports dropped, every
 * file under a `node_modules` directory left out), read as one TypeScript program, so each call is
 * known by the declaration it resolves to, whatever name, alias, destructuring, namespace or
 * element access it is made through.
 *
 * What a site is: a call that resolves to `Bun.spawn`, `Bun.spawnSync` or `Bun.$` (a tagged
 * template too); to a process start of `node:child_process` (`spawn`, `spawnSync`, `exec`,
 * `execSync`, `execFile`, `execFileSync`, `fork`, or a `promisify` of one); to `fetch`; to
 * `request` or `get` of `node:http` or `node:https`; to a socket's open (`connect` or
 * `createConnection` of `node:net` or `node:tls`, `Bun.connect`, `Bun.listen`, `new WebSocket`,
 * `new Socket`); a `getReader()` on a stream that is no fetch's `body`; `new Response(x)` over a
 * process's `stdout`, `stderr` or `stdin`; a read of `Bun.stdin` or `process.stdin`, or a path
 * naming `/dev/stdin`, `/dev/fd/…` or `/dev/tty`; every `for await` loop; and a `require` or
 * dynamic `import()` of one of those modules. A fetch's response body read carries that fetch's
 * signal and is not a site of its own.
 *
 * A site is bounded only by its call's own option, in the object literal it is passed: a `timeout`
 * on a process start that is a positive number no larger than the hook's budget (`maxBoundMs`: a
 * literal, or a constant one names), or a `signal`
 * on a process start, a fetch or an HTTP request that traces to `AbortSignal.timeout`, through
 * `AbortSignal.any`, conditionals, `??`, constants, the fields of a class it is assigned to, the
 * values a function of this repository returns, and a parameter whose every caller passes one.
 * The census (`hook-timed-waits.ts`) sees both bounds: a start's `timeout`, and the timer of
 * `AbortSignal.timeout`. Every other site is unbounded here, and a row names each one that no
 * reviewed entry of its own names the bound of.
 */
import { readFileSync, realpathSync } from "node:fs"
import { dirname, relative } from "node:path"
import ts from "typescript"
import { NAMED_BEFORE_GUARD_MS, untilTheGuard } from "./row-deadline"
import { NO_TRANSPILER_CACHE } from "./transpiler-cache"

export interface WaitSite {
  /** Its file, relative to the repository's root. */
  readonly file: string
  readonly line: number
  readonly kind: string
  /** The site's first line, trimmed. */
  readonly text: string
  /** The bound its call's own option carries, or undefined where it carries none traced. */
  readonly boundedBy: "timeout" | "signal" | undefined
}

/** The modules `entry` reaches through its non-type imports, inside `root` and outside every
 *  `node_modules` directory, `entry` first. */
export function reachedModules(entry: string, root: string): string[] {
  const transpiler = new Bun.Transpiler({ loader: "ts" })
  const realRoot = realpathSync(root)
  const seen: string[] = []
  const queue = [realpathSync(entry)]
  for (let file = queue.shift(); file !== undefined; file = queue.shift()) {
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

/** The modules whose functions are waits: a `require` or `import()` of one is a site. */
const WAIT_MODULES =
  /^(node:)?(child_process|http|https|net|tls|dgram|dns|dns\/promises|worker_threads)$/

/** The kind of wait a declaration's qualified name is, or undefined where it is none. */
function waitKind(qualified: string, inLibrary: boolean): string | undefined {
  const name = qualified.replaceAll('"', "")
  if (/^bun\.(spawn|spawnSync)$/.test(name)) return name.replace(/^bun/, "Bun")
  if (inLibrary && /(^|\.)Atomics\.(wait|waitAsync)$/.test(name)) return "Atomics.wait"
  if (/^bun\.(\$|Shell|ShellPromise)/.test(name)) return "Bun.$"
  if (
    /^(node:)?child_process\.(spawn|spawnSync|exec|execSync|execFile|execFileSync|fork)(\.__promisify__)?$/.test(
      name,
    )
  ) {
    return "child_process"
  }
  if (inLibrary && /(^|\.)fetch$/.test(name)) return "fetch"
  if (
    /^(node:)?dns(\/promises)?\.(Resolver\.)?(lookup|lookupService|resolve\w*|reverse)$/.test(name)
  )
    return "dns"
  if (inLibrary && /^(global\.)?(prompt|confirm|alert)$/.test(name)) return "stream read"
  if (
    inLibrary &&
    /(^|\.)(ReadableStream|BunConsumerConvenienceMethods)\.(text|json|bytes|arrayBuffer|blob)$/.test(
      name,
    )
  )
    return "stream read"
  if (inLibrary && /(^|\.)Worker(\.__new|\.__constructor)?$/.test(name)) return "worker"
  if (/^(node:)?https?\.(request|get)$/.test(name)) return "http request"
  if (/^(node:)?(net|tls)\.(connect|createConnection|Socket\.__constructor)$/.test(name))
    return "socket"
  if (
    /^bun\.(connect|listen|udpSocket)$/.test(name) ||
    (inLibrary && /(^|\.)WebSocket(\.__new|\.__constructor)?$/.test(name))
  ) {
    return "socket"
  }
  return undefined
}

/** Every wait site of `modules`, read as one program with `tsconfig`'s options, with `root` the
 *  repository's root the files are named against. Seconds of synchronous work: `waitSites` runs it
 *  in a process of its own. */
function scanWaitSites(
  modules: readonly string[],
  root: string,
  tsconfig: string,
  maxBoundMs: number,
): WaitSite[] {
  const config = ts.readConfigFile(tsconfig, ts.sys.readFile)
  const options = ts.parseJsonConfigFileContent(config.config, ts.sys, dirname(tsconfig)).options
  const program = ts.createProgram({ rootNames: [...modules], options })
  const checker = program.getTypeChecker()
  const realRoot = realpathSync(root)
  const sources = modules.map((module) => {
    const source = program.getSourceFile(module)
    if (source === undefined)
      throw new Error(`the program over the hook's modules has no ${module}`)
    return source
  })
  const inLibrary = (node: ts.Node): boolean => {
    const file = node.getSourceFile()
    return file.isDeclarationFile || file.fileName.includes("/node_modules/")
  }
  const declarationOf = (call: ts.CallLikeExpression): ts.Declaration | undefined =>
    checker.getResolvedSignature(call)?.getDeclaration() ?? undefined
  /** The qualified name of `declaration`, or of the nearest named node above it. */
  const qualifiedName = (declaration: ts.Node): string => {
    for (let node: ts.Node | undefined = declaration; node !== undefined; node = node.parent) {
      // getNameOfDeclaration reads any node: one that declares no name answers undefined.
      const named = node as ts.Declaration
      const name = ts.getNameOfDeclaration(named)
      const symbol = name === undefined ? undefined : checker.getSymbolAtLocation(name)
      if (symbol !== undefined) return checker.getFullyQualifiedName(symbol)
    }
    return ""
  }

  // The identifiers of the hook's modules by their text, read once: a function's callers, and the
  // places it is handed on as a value, are found among the identifiers that bear its name.
  const byName = new Map<string, (ts.Identifier | ts.PrivateIdentifier)[]>()
  for (const source of sources) {
    const visit = (node: ts.Node): void => {
      if (ts.isIdentifier(node) || ts.isPrivateIdentifier(node))
        byName.set(node.text, [...(byName.get(node.text) ?? []), node])
      ts.forEachChild(node, visit)
    }
    visit(source)
  }
  /** The calls of `owner`, a function of this repository, or undefined where it is also used as a
   *  value (`[…].map(fn)`, a callback), whose callers the scan cannot list. */
  const callsOfCache = new Map<ts.Node, ts.CallExpression[] | undefined>()
  const callsOf = (owner: ts.SignatureDeclaration): ts.CallExpression[] | undefined => {
    if (callsOfCache.has(owner)) return callsOfCache.get(owner)
    const named =
      owner.name !== undefined
        ? owner.name
        : ts.isVariableDeclaration(owner.parent) || ts.isPropertyAssignment(owner.parent)
          ? owner.parent.name
          : undefined
    const symbol = named === undefined ? undefined : checker.getSymbolAtLocation(named)
    let calls: ts.CallExpression[] | undefined = []
    if (
      symbol === undefined ||
      named === undefined ||
      !(ts.isIdentifier(named) || ts.isPrivateIdentifier(named))
    )
      calls = undefined
    for (const identifier of named !== undefined &&
    (ts.isIdentifier(named) || ts.isPrivateIdentifier(named))
      ? (byName.get(named.text) ?? [])
      : []) {
      if (calls === undefined || identifier === named || resolvedSymbol(identifier) !== symbol)
        continue
      const reference =
        ts.isPropertyAccessExpression(identifier.parent) && identifier.parent.name === identifier
          ? identifier.parent
          : identifier
      if (ts.isCallExpression(reference.parent) && reference.parent.expression === reference)
        calls.push(reference.parent)
      else calls = undefined
    }
    callsOfCache.set(owner, calls)
    return calls
  }

  const unwrap = (expression: ts.Expression): ts.Expression => {
    let node = expression
    while (
      ts.isParenthesizedExpression(node) ||
      ts.isAsExpression(node) ||
      ts.isNonNullExpression(node) ||
      ts.isSatisfiesExpression(node)
    ) {
      node = node.expression
    }
    return node
  }
  const resolvedSymbol = (node: ts.Node): ts.Symbol | undefined => {
    // A shorthand `{ signal }` names its value, not the property it makes.
    const symbol =
      ts.isShorthandPropertyAssignment(node.parent) && node.parent.name === node
        ? checker.getShorthandAssignmentValueSymbol(node.parent)
        : checker.getSymbolAtLocation(node)
    return symbol !== undefined && symbol.flags & ts.SymbolFlags.Alias
      ? checker.getAliasedSymbol(symbol)
      : symbol
  }
  const constInitializer = (symbol: ts.Symbol | undefined): ts.Expression | undefined => {
    const declaration = symbol?.valueDeclaration
    if (declaration === undefined || !ts.isVariableDeclaration(declaration)) return undefined
    const list = declaration.parent
    if (!ts.isVariableDeclarationList(list) || !(list.flags & ts.NodeFlags.Const)) return undefined
    return declaration.initializer
  }

  /** The positive number `expression` is, through literals, constants and arithmetic. */
  const traceNumber = (expression: ts.Expression, depth = 0): number | undefined => {
    const node = unwrap(expression)
    if (depth > 12) return undefined
    if (ts.isNumericLiteral(node)) return Number(node.text)
    if (ts.isIdentifier(node) || ts.isPropertyAccessExpression(node)) {
      const initializer = constInitializer(resolvedSymbol(ts.isIdentifier(node) ? node : node.name))
      return initializer === undefined ? undefined : traceNumber(initializer, depth + 1)
    }
    if (ts.isBinaryExpression(node)) {
      const left = traceNumber(node.left, depth + 1)
      const right = traceNumber(node.right, depth + 1)
      if (left === undefined || right === undefined) return undefined
      if (node.operatorToken.kind === ts.SyntaxKind.AsteriskToken) return left * right
      if (node.operatorToken.kind === ts.SyntaxKind.PlusToken) return left + right
    }
    return undefined
  }

  /** Arguments a parameter is bound to while a function's return values are traced for one call. */
  type Bindings = ReadonlyMap<
    ts.Symbol,
    { readonly argument: ts.Expression; readonly bindings: Bindings }
  >
  const none: Bindings = new Map()
  const isGlobalAbortSignal = (node: ts.Expression): boolean => {
    const symbol =
      ts.isIdentifier(node) && node.text === "AbortSignal"
        ? checker.getSymbolAtLocation(node)
        : undefined
    return (symbol?.declarations ?? []).length > 0 && (symbol?.declarations ?? []).every(inLibrary)
  }
  const onPath = new Set<ts.Node>()
  /** Whether `expression` is a signal that aborts at an `AbortSignal.timeout`. */
  const traceSignal = (expression: ts.Expression, bindings: Bindings, depth = 0): boolean => {
    const node = unwrap(expression)
    if (depth > 16 || onPath.has(node)) return false
    onPath.add(node)
    try {
      return traceSignalNode(node, bindings, depth + 1)
    } finally {
      onPath.delete(node)
    }
  }
  const allTraced = (
    expressions: readonly ts.Expression[],
    bindings: Bindings,
    depth: number,
  ): boolean =>
    expressions.length > 0 &&
    expressions.every((expression) => traceSignal(expression, bindings, depth))
  const traceSignalNode = (node: ts.Expression, bindings: Bindings, depth: number): boolean => {
    if (ts.isCallExpression(node)) {
      const callee = unwrap(node.expression)
      if (ts.isPropertyAccessExpression(callee) && isGlobalAbortSignal(callee.expression)) {
        if (callee.name.text === "timeout") {
          // A deadline above the hook's budget bounds nothing the budget needs.
          const ms = node.arguments[0] === undefined ? undefined : traceNumber(node.arguments[0])
          return node.arguments[0] !== undefined && (ms === undefined || ms <= maxBoundMs)
        }
        if (callee.name.text !== "any") return false
        // The list `any` is given: an array literal, or a constant holding one or a `.filter` of one.
        let list = node.arguments[0] === undefined ? undefined : unwrap(node.arguments[0])
        if (list !== undefined && ts.isIdentifier(list)) {
          const initializer = constInitializer(resolvedSymbol(list))
          list = initializer === undefined ? undefined : unwrap(initializer)
        }
        if (
          list !== undefined &&
          ts.isCallExpression(list) &&
          ts.isPropertyAccessExpression(list.expression) &&
          list.expression.name.text === "filter"
        ) {
          // Only a filter that drops nothing but `undefined` or `null` keeps the timeout in.
          const keep = list.arguments[0]?.getText().replace(/\s+/g, " ") ?? ""
          list = /^(Boolean|\(?(\w+)\)? => \2 !==? (undefined|null))$/.test(keep)
            ? unwrap(list.expression.expression)
            : undefined
        }
        return (
          list !== undefined &&
          ts.isArrayLiteralExpression(list) &&
          list.elements.some(
            (element) => !ts.isSpreadElement(element) && traceSignal(element, bindings, depth),
          )
        )
      }
      const declaration = declarationOf(node)
      if (
        declaration === undefined ||
        inLibrary(declaration) ||
        !(
          ts.isFunctionDeclaration(declaration) ||
          ts.isMethodDeclaration(declaration) ||
          ts.isArrowFunction(declaration) ||
          ts.isFunctionExpression(declaration)
        )
      ) {
        return false
      }
      const body = declaration.body
      if (body === undefined) return false
      const bound = new Map(bindings)
      declaration.parameters.forEach((parameter, index) => {
        const argument = node.arguments[index]
        const symbol = checker.getSymbolAtLocation(parameter.name)
        if (argument !== undefined && symbol !== undefined)
          bound.set(symbol, { argument, bindings })
      })
      const returned: ts.Expression[] = []
      if (!ts.isBlock(body)) returned.push(body)
      else {
        const visit = (inner: ts.Node): void => {
          if (ts.isFunctionLike(inner)) return
          if (ts.isReturnStatement(inner) && inner.expression !== undefined)
            returned.push(inner.expression)
          ts.forEachChild(inner, visit)
        }
        ts.forEachChild(body, visit)
      }
      return allTraced(returned, bound, depth)
    }
    if (ts.isConditionalExpression(node))
      return allTraced([node.whenTrue, node.whenFalse], bindings, depth)
    if (
      ts.isBinaryExpression(node) &&
      (node.operatorToken.kind === ts.SyntaxKind.QuestionQuestionToken ||
        node.operatorToken.kind === ts.SyntaxKind.BarBarToken)
    ) {
      return allTraced([node.left, node.right], bindings, depth)
    }
    if (ts.isIdentifier(node)) {
      const symbol = resolvedSymbol(node)
      if (symbol === undefined) return false
      const bound = bindings.get(symbol)
      if (bound !== undefined) return traceSignal(bound.argument, bound.bindings, depth)
      const initializer = constInitializer(symbol)
      if (initializer !== undefined) return traceSignal(initializer, bindings, depth)
      const declaration = symbol.valueDeclaration
      if (
        declaration === undefined ||
        !ts.isParameter(declaration) ||
        !ts.isIdentifier(declaration.name)
      )
        return false
      const owner = declaration.parent
      const index = owner.parameters.indexOf(declaration)
      const calls = callsOf(owner)
      return (
        calls !== undefined &&
        calls.length > 0 &&
        calls.every((call) => {
          const argument = call.arguments[index] ?? declaration.initializer
          return argument !== undefined && traceSignal(argument, none, depth)
        })
      )
    }
    if (ts.isPropertyAccessExpression(node) && node.expression.kind === ts.SyntaxKind.ThisKeyword) {
      const symbol = checker.getSymbolAtLocation(node.name)
      const declaration = symbol?.valueDeclaration
      if (declaration === undefined || !ts.isPropertyDeclaration(declaration)) return false
      const assigned: ts.Expression[] =
        declaration.initializer === undefined ? [] : [declaration.initializer]
      const visit = (inner: ts.Node): void => {
        if (
          ts.isBinaryExpression(inner) &&
          inner.operatorToken.kind === ts.SyntaxKind.EqualsToken &&
          ts.isPropertyAccessExpression(inner.left) &&
          checker.getSymbolAtLocation(inner.left.name) === symbol
        ) {
          assigned.push(inner.right)
        }
        ts.forEachChild(inner, visit)
      }
      // A private field is written only in its class; a public one can be written anywhere.
      if (ts.isPrivateIdentifier(node.name)) visit(declaration.parent)
      else for (const source of sources) visit(source)
      return allTraced(assigned, none, depth)
    }
    return false
  }

  /** The call's own option `name`: a property of the object literal at `index` (or the last
   *  object literal argument where `index` is undefined), spreads left out. An options argument
   *  given by the name of a constant whose initializer is an object literal written `as const`
   *  (`PS_OPTIONS`) is read as that literal: its properties are read-only, so no write elsewhere
   *  (`O.timeout = 99_999`) can change the bound the scan reads. A constant without `as const` is
   *  not read, and its call stays an unbounded site. */
  const ownOption = (
    call: ts.CallExpression | ts.NewExpression,
    name: string,
    index?: number,
  ): ts.Expression | undefined => {
    const literalOf = (
      argument: ts.Expression | undefined,
    ): ts.ObjectLiteralExpression | undefined => {
      if (argument === undefined) return undefined
      const node = unwrap(argument)
      if (ts.isObjectLiteralExpression(node)) return node
      if (!ts.isIdentifier(node)) return undefined
      let initializer = constInitializer(resolvedSymbol(node))
      while (initializer !== undefined && ts.isParenthesizedExpression(initializer)) {
        initializer = initializer.expression
      }
      if (
        initializer === undefined ||
        !ts.isAsExpression(initializer) ||
        !ts.isTypeReferenceNode(initializer.type) ||
        initializer.type.typeName.getText() !== "const"
      ) {
        return undefined
      }
      const held = unwrap(initializer.expression)
      return ts.isObjectLiteralExpression(held) ? held : undefined
    }
    const args = [...(call.arguments ?? [])]
    const literal =
      index === undefined
        ? args
            .reverse()
            .map(literalOf)
            .find((found) => found !== undefined)
        : literalOf(args[index])
    if (literal === undefined) return undefined
    let found: ts.Expression | undefined
    for (const property of literal.properties) {
      if (ts.isPropertyAssignment(property) && property.name.getText() === name)
        found = property.initializer
      else if (ts.isShorthandPropertyAssignment(property) && property.name.text === name)
        found = property.name
      // A spread after the option whose type can hold it can take it away (`{ timeout, ...opts }`).
      else if (
        ts.isSpreadAssignment(property) &&
        found !== undefined &&
        checker.getTypeAtLocation(property.expression).getProperty(name) !== undefined
      )
        found = undefined
    }
    return found
  }
  const boundOf = (
    call: ts.CallExpression | ts.NewExpression,
    kind: string,
  ): WaitSite["boundedBy"] => {
    const first = call.arguments?.[0]
    const index =
      kind === "Bun.spawn" || kind === "Bun.spawnSync"
        ? first !== undefined && ts.isObjectLiteralExpression(first)
          ? 0
          : 1
        : kind === "fetch"
          ? 1
          : undefined
    if (kind !== "fetch" && kind !== "http request") {
      const timeout = ownOption(call, "timeout", index)
      const value = timeout === undefined ? undefined : traceNumber(timeout)
      if (value !== undefined && value > 0 && value <= maxBoundMs) return "timeout"
    }
    if (kind === "Atomics.wait") {
      const timeout = call.arguments?.[3]
      const value = timeout === undefined ? undefined : traceNumber(timeout)
      return value !== undefined && value > 0 && value <= maxBoundMs ? "timeout" : undefined
    }
    if (kind === "Bun.$" || kind === "socket") return undefined
    const signal = ownOption(call, "signal", index)
    return signal !== undefined && traceSignal(signal, none) ? "signal" : undefined
  }

  /** The declaration a call reaches when its type was cast away (`(Bun as any).spawnSync`), or
   *  held in a variable first (`const run = (Bun as any)["spawnSync"]`): the property, read on the
   *  type of the object before the cast. */
  const throughCasts = (callee: ts.Expression, depth: number): ts.Declaration | undefined => {
    const node = unwrap(callee)
    if (depth > 8) return undefined
    if (ts.isIdentifier(node)) {
      const declaration = resolvedSymbol(node)?.valueDeclaration
      const initializer =
        declaration !== undefined && ts.isVariableDeclaration(declaration)
          ? declaration.initializer
          : undefined
      return initializer === undefined ? undefined : throughCasts(initializer, depth + 1)
    }
    const name = ts.isPropertyAccessExpression(node)
      ? node.name.text
      : ts.isElementAccessExpression(node) && ts.isStringLiteralLike(node.argumentExpression)
        ? node.argumentExpression.text
        : undefined
    if (
      name === undefined ||
      !(ts.isPropertyAccessExpression(node) || ts.isElementAccessExpression(node))
    ) {
      return undefined
    }
    let object = unwrap(node.expression)
    for (let step = 0; step < 8 && ts.isIdentifier(object); step++) {
      if (!(checker.getTypeAtLocation(object).flags & ts.TypeFlags.Any)) break
      const declaration = resolvedSymbol(object)?.valueDeclaration
      const initializer =
        declaration !== undefined && ts.isVariableDeclaration(declaration)
          ? declaration.initializer
          : undefined
      if (initializer === undefined) break
      object = unwrap(initializer)
    }
    const property = checker.getTypeAtLocation(object).getProperty(name)
    return property?.valueDeclaration ?? property?.declarations?.[0]
  }
  /** A call no declaration explains, through an `any` the scan cannot see behind, whose name is a
   *  wait's: a site, unbounded. */
  const WAIT_NAMES =
    /^(spawn|spawnSync|exec|execSync|execFile|execFileSync|fork|fetch|request|connect|createConnection|\$)$/
  const unresolvedWait = (callee: ts.Expression): string | undefined => {
    const node = unwrap(callee)
    const name = ts.isIdentifier(node)
      ? node.text
      : ts.isPropertyAccessExpression(node)
        ? node.name.text
        : ts.isElementAccessExpression(node) && ts.isStringLiteralLike(node.argumentExpression)
          ? node.argumentExpression.text
          : undefined
    return name !== undefined && WAIT_NAMES.test(name) ? `unresolved ${name}` : undefined
  }

  /** Whether `node` is in a type, or names what an import, export or declaration binds. */
  const inTypeOrDeclaration = (node: ts.Node): boolean => {
    for (let at: ts.Node | undefined = node.parent; at !== undefined; at = at.parent) {
      if (ts.isTypeNode(at) || ts.isImportDeclaration(at) || ts.isExportDeclaration(at)) return true
      if (ts.isStatement(at)) break
    }
    const parent = node.parent
    return (
      (ts.isVariableDeclaration(parent) && parent.name === node) ||
      (ts.isPropertyAssignment(parent) && parent.name === node) ||
      (ts.isBindingElement(parent) && (parent.name === node || parent.propertyName === node)) ||
      (ts.isParameter(parent) && parent.name === node) ||
      ((ts.isPropertyDeclaration(parent) ||
        ts.isMethodDeclaration(parent) ||
        ts.isFunctionDeclaration(parent)) &&
        parent.name === node)
    )
  }

  const sites: WaitSite[] = []
  for (const source of sources) {
    const file = relative(realRoot, realpathSync(source.fileName))
    const site = (node: ts.Node, kind: string, boundedBy: WaitSite["boundedBy"]): void => {
      const line = source.getLineAndCharacterOfPosition(node.getStart(source)).line + 1
      // The whole call with its line breaks folded, so a call laid out over lines reads as it
      // would on one: no break after an opening bracket or before a closing one, one space else.
      const text = node
        .getText(source)
        .replace(/([([])\s*\n\s*/g, "$1")
        .replace(/\s*\n\s*([)\]])/g, "$1")
        .replace(/\s*\n\s*/g, " ")
        .slice(0, 160)
      sites.push({ file, line, kind, text, boundedBy })
    }
    const visit = (node: ts.Node): void => {
      if (
        ts.isCallExpression(node) ||
        ts.isNewExpression(node) ||
        ts.isTaggedTemplateExpression(node)
      ) {
        const callee = ts.isTaggedTemplateExpression(node) ? node.tag : node.expression
        const declaration = declarationOf(node) ?? throughCasts(callee, 0)
        const kind =
          declaration === undefined
            ? unresolvedWait(callee)
            : waitKind(qualifiedName(declaration), inLibrary(declaration))
        if (kind !== undefined) {
          const bounded =
            ts.isTaggedTemplateExpression(node) || kind.startsWith("unresolved")
              ? undefined
              : boundOf(node, kind)
          site(node, kind, bounded)
        }
      }
      if (ts.isCallExpression(node)) {
        const callee = unwrap(node.expression)
        const specifier = node.arguments[0]
        const loads =
          (node.expression.kind === ts.SyntaxKind.ImportKeyword ||
            (ts.isIdentifier(callee) && callee.text === "require")) &&
          specifier !== undefined &&
          ts.isStringLiteralLike(specifier) &&
          WAIT_MODULES.test(specifier.text)
        if (loads) site(node, "load of a wait module", undefined)
        const computed =
          (node.expression.kind === ts.SyntaxKind.ImportKeyword ||
            (ts.isIdentifier(callee) && callee.text === "require")) &&
          specifier !== undefined &&
          !ts.isStringLiteralLike(specifier)
        if (computed) site(node, "load of a computed module", undefined)
        // A read of file descriptor 0, the process's stdin.
        const reader = declarationOf(node)
        if (
          reader !== undefined &&
          /^(node:)?fs(\/promises)?\.(readFileSync|readFile|readSync|read|createReadStream|openSync|open)$|^bun\.file$/.test(
            qualifiedName(reader).replaceAll('"', ""),
          ) &&
          specifier !== undefined &&
          /^(0|process\.stdin\.fd)$/.test(specifier.getText(source))
        ) {
          site(node, "stream read", undefined)
        }
        if (ts.isPropertyAccessExpression(callee) && callee.name.text === "getReader") {
          const stream = unwrap(callee.expression)
          if (!(ts.isPropertyAccessExpression(stream) && stream.name.text === "body"))
            site(node, "stream read", undefined)
        }
      }
      if (
        ts.isNewExpression(node) &&
        ts.isIdentifier(node.expression) &&
        node.expression.text === "Response" &&
        node.arguments?.some(
          (argument) =>
            /\.(stdout|stderr|stdin)$/.test(argument.getText(source)) ||
            checker.getTypeAtLocation(argument).getSymbol()?.getName() === "ReadableStream",
        )
      ) {
        site(node, "stream read", undefined)
      }
      // A wait handed on as a value (`callIt(Bun.spawnSync)`, `Bun.spawn.call(…)`, `{ fetch }`):
      // whoever calls it, its call carries no bound the scan can read.
      const handedOn =
        (ts.isPropertyAccessExpression(node) ||
          (ts.isIdentifier(node) &&
            !(ts.isPropertyAccessExpression(node.parent) && node.parent.name === node))) &&
        !(ts.isCallExpression(node.parent) && node.parent.expression === node) &&
        !(ts.isNewExpression(node.parent) && node.parent.expression === node) &&
        !(ts.isTaggedTemplateExpression(node.parent) && node.parent.tag === node) &&
        !inTypeOrDeclaration(node)
      if (handedOn) {
        const symbol = resolvedSymbol(ts.isPropertyAccessExpression(node) ? node.name : node)
        const declaration = symbol?.valueDeclaration ?? symbol?.declarations?.[0]
        const kind =
          declaration === undefined || !inLibrary(declaration)
            ? undefined
            : waitKind(qualifiedName(declaration), true)
        if (kind !== undefined && kind !== "stream read")
          site(node, `${kind} handed on as a value`, undefined)
      }
      if (
        ts.isPropertyAccessExpression(node) &&
        node.name.text === "stdin" &&
        ts.isIdentifier(node.expression) &&
        /^(Bun|process)$/.test(node.expression.text)
      ) {
        site(node.parent, "stream read", undefined)
      }
      if (ts.isStringLiteralLike(node) && /^\/dev\/(stdin|tty|fd\/)/.test(node.text))
        site(node, "stream read", undefined)
      if (ts.isForOfStatement(node) && node.awaitModifier !== undefined)
        site(node, "for await", undefined)
      ts.forEachChild(node, visit)
    }
    visit(source)
  }
  return sites
}

/**
 * Every wait site of `modules` (`scanWaitSites`), found by this file run as a program, in a
 * process of its own: the scan is seconds of synchronous TypeScript work, which no limit of the
 * caller's could end on the caller's own thread. `rowLimitMs` is the calling row's limit. The
 * process is killed `NAMED_BEFORE_GUARD_MS` before it, counted from the row's start as Bun counts
 * the limit (`untilTheGuard`; the calling file runs `rowStarted` in a `beforeEach`), so the
 * promise fails, naming the scan, before Bun's own limit ends the row, however long the row took
 * before the spawn. The caller's process holds that deadline: a caller killed first leaves the
 * scan to end by itself. What the scan writes to stderr reaches the caller's stderr, as it did when
 * the scan ran in the caller's process.
 */
export async function waitSites(
  modules: readonly string[],
  root: string,
  tsconfig: string,
  maxBoundMs: number,
  rowLimitMs: number,
): Promise<WaitSite[]> {
  // At least 1 ms: Bun 1.4.4 reads a spawn's `timeout: 0` as no timeout at all, and the row's
  // time can be spent before the spawn.
  const deadlineMs = Math.max(1, Math.floor(untilTheGuard(rowLimitMs)))
  const started = performance.now()
  const scan = Bun.spawn(["bun", import.meta.path], {
    env: { PATH: process.env["PATH"] ?? "", ...NO_TRANSPILER_CACHE },
    stdin: new TextEncoder().encode(JSON.stringify({ modules, root, tsconfig, maxBoundMs })),
    stdout: "pipe",
    stderr: "pipe",
    timeout: deadlineMs,
    killSignal: "SIGKILL",
  })
  const [stdout, stderr, exitCode] = await Promise.all([
    new Response(scan.stdout).text(),
    new Response(scan.stderr).text(),
    scan.exited,
  ])
  if (exitCode !== 0 || scan.signalCode !== null) {
    throw new Error(
      `the wait-site scan ended after ${Math.round(performance.now() - started)} ms, exit ` +
        `${exitCode}, signal ${scan.signalCode}; its deadline was ${deadlineMs} ms after its ` +
        `start, where a SIGKILL ends it ${NAMED_BEFORE_GUARD_MS} ms before the row's limit: ${stderr}`,
    )
  }
  if (stderr !== "") process.stderr.write(stderr)
  return JSON.parse(stdout)
}

// Run as a program by `waitSites`: the scan of what stdin names, written to stdout as JSON.
if (import.meta.main) {
  const { modules, root, tsconfig, maxBoundMs } = JSON.parse(await Bun.stdin.text())
  await Bun.write(Bun.stdout, JSON.stringify(scanWaitSites(modules, root, tsconfig, maxBoundMs)))
}
