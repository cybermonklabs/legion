// packages/cipher-client/test/metadata-server.ts
//
// The Compute Engine metadata server a machine asks for its own Google identity token
// (`src/metadata-identity.ts`), for every suite of this package whose client trades that machine:
// as the subject of a machine's own trade, or as the actor of a person's.

import { afterAll, beforeAll } from "bun:test";
import { METADATA_HOST_VAR } from "../src/metadata-identity";

/**
 * Serves `token`, as the machine's identity token, to the file or `describe` body that calls
 * this. The metadata identity reads `GCE_METADATA_HOST` from the process's environment, so the
 * variable names this server there and is put back after it.
 *
 * Call it at a test file's module scope, or in the body of a `describe`, never inside a test
 * body: it registers the `beforeAll` that starts the server and the `afterAll` that stops it
 * where it is called, and `bun:test` takes the scope of a hook from the point of the call.
 */
export function serveMetadataIdentity(token: string): void {
  let stop = () => {};
  let previousHost: string | undefined;
  beforeAll(() => {
    const server = Bun.serve({
      port: 0,
      hostname: "127.0.0.1",
      fetch: () => new Response(`${token}\n`),
    });
    stop = () => server.stop(true);
    previousHost = process.env[METADATA_HOST_VAR];
    process.env[METADATA_HOST_VAR] = `127.0.0.1:${server.port}`;
  });
  afterAll(() => {
    stop();
    if (previousHost === undefined) delete process.env[METADATA_HOST_VAR];
    else process.env[METADATA_HOST_VAR] = previousHost;
  });
}
