import { type Authenticator, nkeyAuthenticator, nkeys } from "nats";
import { messageFor } from "./errors";
import type { NatsCredential } from "./nats-credential";

/** The part of nats.js's `nkeys` export this module uses; nats.js types the export `any`. */
const seedKeys: { fromSeed(seed: Uint8Array): { getPublicKey(): string } } = nkeys;

/** The `connect` options naming the credential a client connects with (`envoySettings`'s
 * `natsCredential`): the nkey user a seed names, the session's broker token
 * (`BrokerNatsCredential.authenticator`), or none, which is `{}`: the connection carries no
 * credential, as every connection did before servers required one. A seed that is not a user nkey
 * seed throws naming its source; no error carries the seed. */
export function natsAuthOptions(credential: NatsCredential): {
  readonly authenticator?: Authenticator;
} {
  switch (credential.kind) {
    case "none":
      return {};
    case "broker":
      return { authenticator: credential.broker.authenticator };
    case "seed": {
      const { seed } = credential;
      const bytes = new TextEncoder().encode(seed.value);
      let publicKey: string;
      try {
        publicKey = seedKeys.fromSeed(bytes).getPublicKey();
      } catch (error) {
        throw new Error(`${seed.source} does not hold a valid nkey seed: ${messageFor(error)}`);
      }
      if (!publicKey.startsWith("U")) {
        throw new Error(
          `${seed.source} holds an nkey seed that is not a user's (public key ${publicKey})`
        );
      }
      return { authenticator: nkeyAuthenticator(bytes) };
    }
  }
}
