/**
 * The variable each Bun process a test of this package starts under a HOME of its own is given:
 * Bun's runtime transpiler cache off ("0"). The child transpiles this checkout's TypeScript as it
 * would with an empty cache, and writes no cache file. No row here tests that cache.
 *
 * Bun keeps the cache under HOME (`~/.bun/install/cache/@t@` where neither this variable nor
 * `XDG_CACHE_HOME` names another place, as Bun 1.4.4 places it), and a test's HOME starts empty,
 * so each such start wrote the whole cache before its own code ran: a file under a temporary name
 * and a rename for each module it caches (Bun caches only files above a size floor), 128 files for
 * bin/envoy-send.ts, 162 for hooks/session-hook.ts and 282 for bin/envoy-channel.ts (strace,
 * 2026-11-22). Each is a directory-entry operation on the disk the HOME is on, and one directory
 * takes them one at a time. On the dev box's shared xfs disk one took up to 0.78 s at a load of 500,
 * and rows that wait for such a child overran their bounds by tens of seconds
 * (dispatch://INFRA-3904, the table's row "claude-envoy rows whose verdict comes from a fixed
 * per-row bound").
 */
export const NO_TRANSPILER_CACHE = { BUN_RUNTIME_TRANSPILER_CACHE_PATH: "0" } as const
