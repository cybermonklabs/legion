/**
 * The key a test puts in the environment of each Bun process it starts under a HOME of the test's
 * own. Bun keeps its runtime transpiler cache under that HOME (`$HOME/.bun/install/cache/@t@`, or
 * `$XDG_CACHE_HOME/bun/@t@` where that is set), and a HOME the test made starts empty: the child
 * transpiles each module larger than 4 KB cold and writes it into the test's directory (149 files
 * for the dispatch entry), which the test's removal then deletes. That file work is no part of what
 * a test checks, and stalling it on purpose reproduces the two-child AskGate row's time-out. "0"
 * turns the cache off (https://bun.com/docs/runtime/environment-variables), so the child writes
 * nothing there and transpiles as much as it did before.
 *
 * Spread it beside the HOME: `env: { ...bunCacheOff, HOME: home, ... }`. A process that inherits
 * that environment, such as a shell that starts `bun`, inherits the key with it.
 */
export const bunCacheOff = { BUN_RUNTIME_TRANSPILER_CACHE_PATH: "0" } as const;
