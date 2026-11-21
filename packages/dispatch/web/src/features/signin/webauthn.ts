import type {
  PasskeyAssertion,
  PasskeyCreationOptions,
  PasskeyCredentialDescriptor,
  PasskeyRegistration,
  PasskeyRequestOptions,
} from "@legion/contracts";

/**
 * The browser's half of the passkey ceremonies (`@legion/contracts`' `PasskeyCreationOptions` and
 * its neighbours): `makePasskey` and `signWithPasskey` take a begin's answer, run the browser's
 * ceremony, and answer the finish's body. Dispatch writes every buffer of the WebAuthn options as
 * base64url without padding, and takes the credential the browser made in the same form. The
 * conversion is written here rather than read from
 * `PublicKeyCredential.parseCreationOptionsFromJSON` and `toJSON`, because a person from outside
 * brings whatever browser they have, and those two are newer than WebAuthn itself in every engine.
 */

/** Makes a passkey with the browser from `/auth/passkey/register/begin`'s answer, and answers
 *  `/auth/passkey/register/finish`'s body. A browser that makes none (`null`) is refused as a
 *  cancel is, with a `NotAllowedError`. */
export async function makePasskey(options: PasskeyCreationOptions): Promise<PasskeyRegistration> {
  const passkey = await navigator.credentials.create({ publicKey: creationOptions(options) });
  if (passkey === null) throw new DOMException("no passkey was made", "NotAllowedError");
  return registrationJSON(passkey as PublicKeyCredential);
}

/** Signs with a passkey the device holds, from `/auth/passkey/login/begin`'s answer, and answers
 *  `/auth/passkey/login/finish`'s body. A browser that signs with none (`null`) is refused as a
 *  cancel is, with a `NotAllowedError`. */
export async function signWithPasskey(options: PasskeyRequestOptions): Promise<PasskeyAssertion> {
  const signature = await navigator.credentials.get({ publicKey: requestOptions(options) });
  if (signature === null) throw new DOMException("no passkey was used", "NotAllowedError");
  return assertionJSON(signature as PublicKeyCredential);
}

/** The bytes a base64url text names, padded or not; text that is not base64url throws. */
function fromBase64url(text: string): ArrayBuffer {
  const base64 = text.replaceAll("-", "+").replaceAll("_", "/");
  const binary = atob(base64.padEnd(base64.length + ((4 - (base64.length % 4)) % 4), "="));
  const bytes = new Uint8Array(binary.length);
  for (let index = 0; index < binary.length; index++) {
    bytes[index] = binary.charCodeAt(index);
  }
  return bytes.buffer;
}

/** `buffer` as base64url without padding, as Dispatch reads it. */
function toBase64url(buffer: ArrayBuffer | ArrayBufferView): string {
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
function creationOptions({
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
function requestOptions({ publicKey }: PasskeyRequestOptions): PublicKeyCredentialRequestOptions {
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
function registrationJSON(credential: PublicKeyCredential): PasskeyRegistration {
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
function assertionJSON(credential: PublicKeyCredential): PasskeyAssertion {
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
