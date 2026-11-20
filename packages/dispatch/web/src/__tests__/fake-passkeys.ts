import { type Mock, mock } from "bun:test";
import type { PasskeyCreationOptions, PasskeyRequestOptions } from "@legion/contracts";

/** A browser that can make and use a passkey, for a page test: `navigator.credentials` and
 *  `window.PublicKeyCredential`, which happy-dom has neither of, with `create` and `get` the
 *  test's to answer. `restore` puts back what the window had. */
export function installFakePasskeys(): {
  readonly create: Mock<(options: CredentialCreationOptions) => Promise<Credential | null>>;
  readonly get: Mock<(options: CredentialRequestOptions) => Promise<Credential | null>>;
  readonly restore: () => void;
} {
  const credentials = Object.getOwnPropertyDescriptor(navigator, "credentials");
  const publicKeyCredential = Object.getOwnPropertyDescriptor(window, "PublicKeyCredential");
  const create = mock(
    async (_options: CredentialCreationOptions): Promise<Credential | null> => createdPasskey()
  );
  const get = mock(
    async (_options: CredentialRequestOptions): Promise<Credential | null> => passkeySignature()
  );
  Object.defineProperty(navigator, "credentials", { configurable: true, value: { create, get } });
  Object.defineProperty(window, "PublicKeyCredential", {
    configurable: true,
    value: function PublicKeyCredential() {},
  });
  return {
    create,
    get,
    restore: () => {
      for (const [target, name, descriptor] of [
        [navigator, "credentials", credentials],
        [window, "PublicKeyCredential", publicKeyCredential],
      ] as const) {
        if (descriptor === undefined) Reflect.deleteProperty(target, name);
        else Object.defineProperty(target, name, descriptor);
      }
    },
  };
}

const bytes = (...values: number[]) => new Uint8Array(values).buffer;

/** A credential as the browser hands one back, around `response`. */
function credential(response: Record<string, unknown>): Credential {
  return {
    authenticatorAttachment: "platform",
    getClientExtensionResults: () => ({}),
    id: "AAEC",
    rawId: bytes(0, 1, 2),
    response,
    type: "public-key",
  } as unknown as Credential;
}

/** The passkey a device made, as `navigator.credentials.create` answers it. */
export function createdPasskey(): Credential {
  return credential({
    attestationObject: bytes(0xff, 0xef),
    clientDataJSON: bytes(0xfb),
    getTransports: () => ["internal"],
  });
}

/** The JSON `createdPasskey` goes to Dispatch as. */
export const createdPasskeyJSON = {
  authenticatorAttachment: "platform",
  clientExtensionResults: {},
  id: "AAEC",
  rawId: "AAEC",
  response: { attestationObject: "_-8", clientDataJSON: "-w", transports: ["internal"] },
  type: "public-key",
} as const;

/** A passkey's signature, as `navigator.credentials.get` answers it. */
export function passkeySignature(): Credential {
  return credential({
    authenticatorData: bytes(0xff, 0xef),
    clientDataJSON: bytes(0xfb),
    signature: bytes(0, 1, 2),
    userHandle: bytes(0xfb),
  });
}

/** The JSON `passkeySignature` goes to Dispatch as. */
export const passkeySignatureJSON = {
  authenticatorAttachment: "platform",
  clientExtensionResults: {},
  id: "AAEC",
  rawId: "AAEC",
  response: { authenticatorData: "_-8", clientDataJSON: "-w", signature: "AAEC", userHandle: "-w" },
  type: "public-key",
} as const;

/** `/auth/passkey/register/begin`'s answer for a link made for `email`. */
export function enrollmentOptions(email: string): PasskeyCreationOptions {
  return {
    publicKey: {
      attestation: "none",
      authenticatorSelection: { residentKey: "required", userVerification: "required" },
      challenge: "AAEC",
      pubKeyCredParams: [{ alg: -7, type: "public-key" }],
      rp: { id: "localhost", name: "Dispatch" },
      user: { displayName: email, id: "-w", name: email },
    },
  };
}

/** `/auth/passkey/login/begin`'s answer. */
export const signInOptions: PasskeyRequestOptions = {
  publicKey: { challenge: "AAEC", rpId: "localhost", userVerification: "required" },
};
