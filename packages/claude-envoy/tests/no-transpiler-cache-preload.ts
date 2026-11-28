/**
 * Preloaded into every `bun test` of this package (bunfig.toml): Bun's transpiler cache off
 * (`./transpiler-cache.ts`) in this process's own environment, so a child that inherits it, or
 * spreads it (`{ ...process.env, HOME: <a fresh directory> }`), writes no cache either. A child given
 * an environment of its own still needs `NO_TRANSPILER_CACHE` in it, which `transpiler-cache.test.ts`
 * holds.
 */
import { NO_TRANSPILER_CACHE } from "./transpiler-cache"

Object.assign(process.env, NO_TRANSPILER_CACHE)
