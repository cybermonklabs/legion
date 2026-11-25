// packages/cipher-client/scripts/print-identity-sources.ts
//
// Prints this client's identity registry as JSON, one entry per source, each with the name
// AGENT_SECRETS_MACHINE_IDENTITY takes and the broker identity provider whose tokens it makes
// (null for the device login, whose broker access token no provider verifies). The broker's own
// test reads it (`internal/cipher/identity/boot/sources_agree_test.go`) to hold the two clients'
// registries and the server's providers to each other, so this prints the registry the client
// itself evaluates rather than a copy that could go stale.

import { IDENTITY_SOURCES } from "../src/environment";

process.stdout.write(
  `${JSON.stringify(IDENTITY_SOURCES.map(({ name, provider }) => ({ name, provider })))}\n`
);
