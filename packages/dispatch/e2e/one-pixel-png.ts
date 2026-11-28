/** The bytes every row that needs a picture sends, in a module of their own.
 *
 *  They stand here rather than beside the sends that paste them (`e2e/sends.ts`) because
 *  `e2e/api.ts` uploads them too, and `packages/pi-legion` imports `e2e/api.ts`
 *  (`scripts/skill-scenarios/seed.ts`) under a tsconfig whose `lib` is `ES2023` alone: a module
 *  that names `window`, `PopStateEvent` or `DataTransfer`, as `e2e/sends.ts` does, fails that
 *  package's typecheck. This module names nothing but a string. */

/** A 1x1 PNG, the smallest picture a browser draws. */
export const ONE_PIXEL_PNG =
  "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg==";
