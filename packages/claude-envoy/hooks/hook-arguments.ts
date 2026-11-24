/**
 * `session-hook.js`'s arguments: `<mode> [arguments…] [--host <claude|codex>]`. Claude Code's
 * hooks.json names no host, so it is `claude`; the Codex plugin's names `--host codex`.
 */
export function parseHookArguments(argv: readonly string[]): {
  readonly host: "claude" | "codex"
  readonly mode: string | undefined
  readonly modeArguments: readonly string[]
} {
  const at = argv.indexOf("--host")
  let host: "claude" | "codex" = "claude"
  let rest = argv
  if (at >= 0) {
    const named = argv[at + 1]
    if (named !== "claude" && named !== "codex") {
      throw new Error(`session-hook: --host takes claude or codex, not ${JSON.stringify(named)}`)
    }
    host = named
    rest = [...argv.slice(0, at), ...argv.slice(at + 2)]
  }
  const [mode, ...modeArguments] = rest
  return { host, mode, modeArguments }
}
