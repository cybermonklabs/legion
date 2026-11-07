/** Where this run's Dispatch server embeds text for meaning search, from one switch,
 * `DISPATCH_E2E_EMBEDDER`, which `e2e/run-server.sh` reads the same way:
 *
 * - unset: no embedder at all (`DISPATCH_TEST_EMBEDDER_OFF=1`), which is what nearly every spec
 *   is written against - search answers the keyword leg and says so, and no seeded text is
 *   embedded;
 * - `1`: the server's own development embedder, which hashes a text's words into a vector in the
 *   process and calls nothing. It proves that meaning search finds what it should, and nothing
 *   outside the server can see the calls it makes;
 * - `fake`: `e2e/fake-embedder.ts` on `FAKE_EMBEDDER_PORT`, which the harness starts and the
 *   server calls as it calls Bedrock. It counts the calls it was asked for, so a row can read
 *   back how many embeddings a stretch of typing cost (`e2e/search-tiers.e2e.ts`).
 *
 * A deployed run (`PLAYWRIGHT_BASE_URL`) starts no server, so it starts no fake embedder either
 * and a row that reads one is skipped. This module only reads the environment, so any process
 * may import it.
 */
export const usesFakeEmbedder =
  process.env.DISPATCH_E2E_EMBEDDER === "fake" && !process.env.PLAYWRIGHT_BASE_URL;
