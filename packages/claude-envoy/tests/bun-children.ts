/**
 * The places in this package's tests that start a Bun process with an environment of their own:
 * a `Bun.spawn` or `Bun.spawnSync` (in either form, `(argv, options)` or `({ cmd, … })`), or a start
 * of `node:child_process` (`spawn`, `spawnSync`, `execFile`, `execFileSync`, `fork`, or `exec` of a
 * string starting `bun `), whose command's first word is `"bun"`, `process.execPath`, or a
 * launcher of this package's `bin/` that execs bun (`bin/dispatch`), named by a `join(…, "bin",
 * "<name>")` there or in the constant it is held in, or a `StdioClientTransport` whose `command`
 * is one of those, with an `env` option. Such a child gets
 * only that environment, so it keeps Bun's transpiler cache under the HOME the test gives it unless
 * the environment turns the cache off (`NO_TRANSPILER_CACHE`, `./transpiler-cache.ts`), or hands on
 * this process's own (`...process.env`), whose HOME is the person's own.
 *
 * Read with the TypeScript parser. Not read: a command a shell string starts (`bash -c "bun …"`),
 * a command array built elsewhere and passed by name, and an `env` passed by a name whose value is
 * built elsewhere, which counts as carrying neither.
 */
import { readdirSync, readFileSync, statSync } from "node:fs"
import { join } from "node:path"
import ts from "typescript"

/** The launchers in this package's `bin/` that exec bun: a child they start is a Bun child. */
const binDirectory = join(import.meta.dir, "..", "bin")
const launchers = readdirSync(binDirectory).filter((name) => {
  const path = join(binDirectory, name)
  return statSync(path).isFile() && /^exec bun\b/m.test(readFileSync(path, "utf8"))
})

export interface BunChild {
  readonly line: number
  /** The call's first line, trimmed. */
  readonly text: string
  /** Whether its environment turns the cache off or hands on this process's own. */
  readonly cacheOff: boolean
}

/** The object literal of `node`'s options: its last object literal argument. */
function optionsOf(
  node: ts.CallExpression | ts.NewExpression,
): ts.ObjectLiteralExpression | undefined {
  return [...(node.arguments ?? [])].reverse().find(ts.isObjectLiteralExpression)
}

/** The initializer of `options`'s property `name`, or undefined where it has none. */
function property(options: ts.ObjectLiteralExpression, name: string): ts.Expression | undefined {
  for (const element of options.properties) {
    if (ts.isPropertyAssignment(element) && element.name.getText() === name)
      return element.initializer
    if (ts.isShorthandPropertyAssignment(element) && element.name.text === name) return element.name
  }
  return undefined
}

/** Every Bun child `file` starts with an environment of its own. */
export function bunChildren(file: string): BunChild[] {
  const source = ts.createSourceFile(file, readFileSync(file, "utf8"), ts.ScriptTarget.Latest, true)
  /** The text a command names: its own, or the initializer of the constant it is held in. */
  const named = (node: ts.Node): string => {
    if (!ts.isIdentifier(node)) return node.getText(source)
    let found = node.getText(source)
    const visit = (inner: ts.Node): void => {
      if (
        ts.isVariableDeclaration(inner) &&
        ts.isIdentifier(inner.name) &&
        inner.name.text === node.text &&
        inner.initializer
      ) {
        found = inner.initializer.getText(source)
      }
      ts.forEachChild(inner, visit)
    }
    visit(source)
    return found
  }
  const isBun = (node: ts.Node | undefined): boolean => {
    if (node === undefined) return false
    const text = named(node)
    return (
      /^("bun"|'bun'|process\.execPath)$/.test(text) ||
      launchers.some((name) => text.includes(`"bin", ${JSON.stringify(name)}`))
    )
  }
  const children: BunChild[] = []
  const visit = (node: ts.Node): void => {
    let options: ts.ObjectLiteralExpression | undefined
    const callee = ts.isCallExpression(node) ? node.expression.getText(source) : ""
    if (ts.isCallExpression(node) && /^Bun\.spawn(Sync)?$/.test(callee)) {
      // `(argv, options)`, or `({ cmd, …options })`.
      const first = node.arguments[0]
      if (first !== undefined && ts.isArrayLiteralExpression(first) && isBun(first.elements[0])) {
        options = optionsOf(node)
      }
      if (first !== undefined && ts.isObjectLiteralExpression(first)) {
        const cmd = property(first, "cmd")
        if (cmd !== undefined && ts.isArrayLiteralExpression(cmd) && isBun(cmd.elements[0]))
          options = first
      }
    } else if (
      ts.isCallExpression(node) &&
      /(^|\.)(spawn|spawnSync|execFile|execFileSync|fork)$/.test(callee)
    ) {
      // node:child_process: `(file, args?, options?)`.
      if (isBun(node.arguments[0])) options = optionsOf(node)
    } else if (ts.isCallExpression(node) && /(^|\.)(exec|execSync)$/.test(callee)) {
      const command = node.arguments[0]
      if (command !== undefined && ts.isStringLiteralLike(command) && /^bun\s/.test(command.text)) {
        options = optionsOf(node)
      }
    }
    if (ts.isNewExpression(node) && node.expression.getText(source) === "StdioClientTransport") {
      const literal = optionsOf(node)
      if (literal !== undefined && isBun(property(literal, "command"))) options = literal
    }
    const env = options === undefined ? undefined : property(options, "env")
    if (env !== undefined) {
      const text = env.getText(source)
      children.push({
        line: source.getLineAndCharacterOfPosition(node.getStart(source)).line + 1,
        text: node.getText(source).split("\n")[0]?.trim().slice(0, 120) ?? "",
        // `...process.env` hands on the test process's own, which the package's test preload sets
        // the cache off in (`./no-transpiler-cache-preload.ts`).
        cacheOff: /\bNO_TRANSPILER_CACHE\b|\.\.\.process\.env\b/.test(text),
      })
    }
    ts.forEachChild(node, visit)
  }
  visit(source)
  return children
}
