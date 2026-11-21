/**
 * Dispatch's `/api/v2`: the paths and the request and answer shapes of every route it serves,
 * generated from the Go route table through `schemas/dispatch-openapi-v2.json`
 * (`dispatch-api-v2.generated.ts`, which `bun run gen:api` writes), and re-exported here whole:
 * `paths` for a route's own types, `components` and one alias per schema for its shapes.
 *
 * Reached as `@legion/contracts/dispatch-api-v2`, never from the barrel: the `/api/v1` document's
 * module, which `dispatch-api.ts` re-exports, declares many of the same schema names (`Issue`,
 * `Project`, ...), and two `export *` of one name do not build (TS2308).
 */
export * from "./dispatch-api-v2.generated";
