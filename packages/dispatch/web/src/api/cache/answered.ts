/** The brand's key: declared, never made, so no value outside this module's mint carries it. */
declare const answeredBrand: unique symbol;

/**
 * `T` as Dispatch answered it: the one kind of value a cache write takes (`Cache.setDispatched`),
 * so the cache holds Dispatch's answers alone (R10 of dispatch://INFRA-299) - nothing a screen
 * built can be written there, because nothing a screen builds carries this brand. It is minted at
 * the two places Dispatch's data enters the page: the API client's answer to a write
 * (`api/client.ts`, whose request builders name their answer `Answered`) and a live frame as it is
 * decoded (`live-socket.ts`' snapshot and delta, `event-log.ts`' event). A test's rig plays
 * Dispatch's part through `test-client.ts`' `answeredByDispatch`. Lint refuses naming this type,
 * and importing this module, everywhere else (`biome.json`), so no other module can cast to it.
 */
export type Answered<T> = T & { readonly [answeredBrand]: true };

/** `body`, which Dispatch sent, as its answer: the one mint, for the modules named above. */
export function answered<T>(body: T): Answered<T> {
  return body as Answered<T>;
}
