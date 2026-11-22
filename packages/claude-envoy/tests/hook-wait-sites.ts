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
 * on a process start that is a positive number (a literal, or a constant one names), or a `signal`
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
const WAIT_MODULES = /^(node:)?(child_process|http|https|net|tls|dgram)$/

/** The kind of wait a declaration's qualified name is, or undefined where it is none. */
function waitKind(qualified: string, inLibrary: boolean): string | undefined {
  const name = qualified.replaceAll('"', "")
  if (/^bun\.(spawn|spawnSync)$/.test(name)) return name.replace(/^bun/, "Bun")
  if (/^bun\.(\$|Shell|ShellPromise)/.test(name)) return "Bun.$"
  if (
    /^(node:)?child_process\.(spawn|spawnSync|exec|execSync|execFile|execFileSync|fork)(\.__promisify__)?$/.test(
      name,
    )
  ) {
    return "child_process"
  }
  if (inLibrary && /(^|\.)fetch$/.test(name)) return "fetch"
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
 *  repository's root the files are named against. */
export function waitSites(modules: readonly string[], root: string, tsconfig: string): WaitSite[] {
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

  // Every call of a function of this repository, by its declaration: a parameter is traced through
  // the arguments its callers pass.
  const callsOf = new Map<ts.Node, ts.CallExpression[]>()
  for (const source of sources) {
    const visit = (node: ts.Node): void => {
      if (ts.isCallExpression(node)) {
        const declaration = declarationOf(node)
        if (declaration !== undefined && !inLibrary(declaration)) {
          callsOf.set(declaration, [...(callsOf.get(declaration) ?? []), node])
        }
      }
      ts.forEachChild(node, visit)
    }
    visit(source)
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
    const symbol = checker.getSymbolAtLocation(node)
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
        if (callee.name.text === "timeout") return true
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
          list = unwrap(list.expression.expression)
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
      const calls = callsOf.get(owner) ?? []
      return (
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
      visit(declaration.parent)
      return allTraced(assigned, none, depth)
    }
    return false
  }

  /** The call's own option `name`: a property of the object literal at `index` (or the last
   *  object literal argument where `index` is undefined), spreads left out. */
  const ownOption = (
    call: ts.CallExpression | ts.NewExpression,
    name: string,
    index?: number,
  ): ts.Expression | undefined => {
    const args = call.arguments ?? ts.factory.createNodeArray()
    const literal =
      index === undefined ? [...args].reverse().find(ts.isObjectLiteralExpression) : args[index]
    if (literal === undefined || !ts.isObjectLiteralExpression(literal)) return undefined
    for (const property of literal.properties) {
      if (ts.isPropertyAssignment(property) && property.name.getText() === name)
        return property.initializer
      if (ts.isShorthandPropertyAssignment(property) && property.name.text === name)
        return property.name
    }
    return undefined
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
      if (value !== undefined && value > 0) return "timeout"
    }
    if (kind === "Bun.$" || kind === "socket") return undefined
    const signal = ownOption(call, "signal", index)
    return signal !== undefined && traceSignal(signal, none) ? "signal" : undefined
  }

  const sites: WaitSite[] = []
  for (const source of sources) {
    const file = relative(realRoot, realpathSync(source.fileName))
    const site = (node: ts.Node, kind: string, boundedBy: WaitSite["boundedBy"]): void => {
      const line = source.getLineAndCharacterOfPosition(node.getStart(source)).line + 1
      const text = node.getText(source).split("\n")[0]?.trim().slice(0, 120) ?? ""
      sites.push({ file, line, kind, text, boundedBy })
    }
    const visit = (node: ts.Node): void => {
      if (
        ts.isCallExpression(node) ||
        ts.isNewExpression(node) ||
        ts.isTaggedTemplateExpression(node)
      ) {
        const declaration = declarationOf(node)
        const kind =
          declaration === undefined
            ? undefined
            : waitKind(qualifiedName(declaration), inLibrary(declaration))
        if (kind !== undefined)
          site(node, kind, ts.isTaggedTemplateExpression(node) ? undefined : boundOf(node, kind))
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
        node.arguments?.some((argument) =>
          /\.(stdout|stderr|stdin)$/.test(argument.getText(source)),
        )
      ) {
        site(node, "stream read", undefined)
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
