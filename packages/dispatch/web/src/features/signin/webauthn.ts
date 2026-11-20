import type {
  PasskeyAssertion,
  PasskeyCreationOptions,
  PasskeyCredentialDescriptor,
  PasskeyRegistration,
  PasskeyRequestOptions,
} from "@legion/contracts";

/**
 * The browser's half of the passkey ceremonies (`@legion/contracts`' `PasskeyCreationOptions` and
 * its neighbours): Dispatch writes every buffer of the WebAuthn options as base64url without
 * padding, and takes the credential the browser made in the same form. The conversion is written
 * here rather than read from `PublicKeyCredential.parseCreationOptionsFromJSON` and `toJSON`,
 * because a person from outside brings whatever browser they have, and those two are newer than
 * WebAuthn itself in every engine.
 */

/** The bytes a base64url text names, padded or not; text that is not base64url throws. */
export function fromBase64url(text: string): ArrayBuffer {
  const base64 = text.replaceAll("-", "+").replaceAll("_", "/");
  const binary = atob(base64.padEnd(base64.length + ((4 - (base64.length % 4)) % 4), "="));
  const bytes = new Uint8Array(binary.length);
  for (let index = 0; index < binary.length; index++) {
    bytes[index] = binary.charCodeAt(index);
  }
  return bytes.buffer;
}

/** `buffer` as base64url without padding, as Dispatch reads it. */
export function toBase64url(buffer: ArrayBuffer | ArrayBufferView): string {
  const bytes =
    buffer instanceof ArrayBuffer
      ? new Uint8Array(buffer)
      : new Uint8Array(buffer.buffer, buffer.byteOffset, buffer.byteLength);
  let binary = "";
  for (const byte of bytes) {
    binary += String.fromCharCode(byte);
  }
  return btoa(binary).replaceAll("+", "-").replaceAll("/", "_").replace(/=+$/, "");
}

function descriptors(
  listed: readonly PasskeyCredentialDescriptor[] | undefined
): PublicKeyCredentialDescriptor[] | undefined {
  return listed?.map((descriptor) => ({
    id: fromBase64url(descriptor.id),
    type: descriptor.type,
    ...(descriptor.transports === undefined
      ? {}
      : { transports: [...descriptor.transports] as AuthenticatorTransport[] }),
  }));
}

/** `navigator.credentials.create`'s `publicKey` from `/auth/passkey/register/begin`'s answer.
 *  The enumerations are WebAuthn's own words, which the server writes as they are. */
export function creationOptions({
  publicKey,
}: PasskeyCreationOptions): PublicKeyCredentialCreationOptions {
  return {
    challenge: fromBase64url(publicKey.challenge),
    pubKeyCredParams: publicKey.pubKeyCredParams.map(({ alg, type }) => ({ alg, type })),
    rp: { ...publicKey.rp },
    user: { ...publicKey.user, id: fromBase64url(publicKey.user.id) },
    ...(publicKey.timeout === undefined ? {} : { timeout: publicKey.timeout }),
    ...(publicKey.excludeCredentials === undefined
      ? {}
      : { excludeCredentials: descriptors(publicKey.excludeCredentials) }),
    ...(publicKey.authenticatorSelection === undefined
      ? {}
      : {
          authenticatorSelection: {
            ...publicKey.authenticatorSelection,
          } as AuthenticatorSelectionCriteria,
        }),
    ...(publicKey.attestation === undefined
      ? {}
      : { attestation: publicKey.attestation as AttestationConveyancePreference }),
    ...(publicKey.extensions === undefined
      ? {}
      : { extensions: { ...publicKey.extensions } as AuthenticationExtensionsClientInputs }),
  };
}

/** `navigator.credentials.get`'s `publicKey` from `/auth/passkey/login/begin`'s answer. */
export function requestOptions({
  publicKey,
}: PasskeyRequestOptions): PublicKeyCredentialRequestOptions {
  return {
    challenge: fromBase64url(publicKey.challenge),
    ...(publicKey.timeout === undefined ? {} : { timeout: publicKey.timeout }),
    ...(publicKey.rpId === undefined ? {} : { rpId: publicKey.rpId }),
    ...(publicKey.allowCredentials === undefined
      ? {}
      : { allowCredentials: descriptors(publicKey.allowCredentials) }),
    ...(publicKey.userVerification === undefined
      ? {}
      : { userVerification: publicKey.userVerification as UserVerificationRequirement }),
    ...(publicKey.extensions === undefined
      ? {}
      : { extensions: { ...publicKey.extensions } as AuthenticationExtensionsClientInputs }),
  };
}

/** What every credential carries beside its response. */
function credentialJSON(credential: PublicKeyCredential) {
  return {
    ...(credential.authenticatorAttachment === null ||
    credential.authenticatorAttachment === undefined
      ? {}
      : { authenticatorAttachment: credential.authenticatorAttachment }),
    clientExtensionResults: { ...credential.getClientExtensionResults() },
    id: credential.id,
    rawId: toBase64url(credential.rawId),
    type: "public-key" as const,
  };
}

/** `/auth/passkey/register/finish`'s body from the passkey `navigator.credentials.create` made. */
export function registrationJSON(credential: PublicKeyCredential): PasskeyRegistration {
  const response = credential.response as AuthenticatorAttestationResponse;
  return {
    ...credentialJSON(credential),
    response: {
      attestationObject: toBase64url(response.attestationObject),
      clientDataJSON: toBase64url(response.clientDataJSON),
      // A browser older than the method gives no transports, which the server stores as none.
      ...(typeof response.getTransports === "function"
        ? { transports: response.getTransports() }
        : {}),
    },
  };
}

/** `/auth/passkey/login/finish`'s body from the signature `navigator.credentials.get` made. */
export function assertionJSON(credential: PublicKeyCredential): PasskeyAssertion {
  const response = credential.response as AuthenticatorAssertionResponse;
  return {
    ...credentialJSON(credential),
    response: {
      authenticatorData: toBase64url(response.authenticatorData),
      clientDataJSON: toBase64url(response.clientDataJSON),
      signature: toBase64url(response.signature),
      ...(response.userHandle === null ? {} : { userHandle: toBase64url(response.userHandle) }),
    },
  };
}

/** Whether this browser can make and use a passkey at all. */
export function passkeysSupported(): boolean {
  return (
    typeof window !== "undefined" &&
    typeof window.PublicKeyCredential === "function" &&
    typeof navigator.credentials?.create === "function"
  );
}

/** What the person reads when their browser refused the ceremony itself (`create` makes a
 *  passkey, `get` signs with one), before anything reached Dispatch. A browser names its refusal
 *  with a `DOMException`: `NotAllowedError` for a cancel, a timeout or no passkey to offer, and
 *  `InvalidStateError` for a device that holds a passkey of this address already. */
export function ceremonyFailure(error: unknown, ceremony: "create" | "get"): string {
  const name = error instanceof Error ? error.name : "an unknown error";
  if (name === "NotAllowedError") {
    return ceremony === "create"
      ? "No passkey was made: the request was cancelled or timed out. Press the button to try again."
      : "No passkey was used: the request was cancelled or timed out, or this device holds no passkey for Dispatch. Try again.";
  }
  if (name === "InvalidStateError" && ceremony === "create") {
    return "This device holds a passkey for this address already. Sign in with it instead.";
  }
  return `Your browser refused the passkey (${name}). Try again, or use another browser.`;
}
