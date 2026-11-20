import { expect, test } from "bun:test";

import {
  assertionJSON,
  ceremonyFailure,
  creationOptions,
  fromBase64url,
  registrationJSON,
  requestOptions,
  toBase64url,
} from "./webauthn";

const bytes = (...values: number[]) => new Uint8Array(values).buffer;

// The server writes every buffer as base64url without padding, go-webauthn's
// URLEncodedBase64; the page reads it into bytes and writes the browser's bytes back the same
// way. The cases cover each padding length and the two characters base64url changes.
test("base64url round-trips every padding length and the characters base64 spells otherwise", () => {
  for (const [text, value] of [
    ["", bytes()],
    ["-w", bytes(0xfb)],
    ["_-8", bytes(0xff, 0xef)],
    ["AAEC", bytes(0, 1, 2)],
    ["-_8A_w", bytes(0xfb, 0xff, 0x00, 0xff)],
  ] as const) {
    expect(new Uint8Array(fromBase64url(text))).toEqual(new Uint8Array(value));
    expect(toBase64url(value)).toBe(text);
    expect(toBase64url(new Uint8Array(value))).toBe(text);
  }
  expect(() => fromBase64url("not base64url!")).toThrow();
});

test("the enrollment's options become the browser's, with every id and the challenge as bytes", () => {
  const options = creationOptions({
    publicKey: {
      attestation: "none",
      authenticatorSelection: { residentKey: "required", userVerification: "required" },
      challenge: "AAEC",
      excludeCredentials: [{ id: "-w", transports: ["internal"], type: "public-key" }],
      extensions: { credProps: true },
      pubKeyCredParams: [{ alg: -7, type: "public-key" }],
      rp: { id: "dispatch.example", name: "Dispatch" },
      timeout: 300_000,
      user: { displayName: "ana@other.example", id: "_-8", name: "ana@other.example" },
    },
  });
  expect(new Uint8Array(options.challenge as ArrayBuffer)).toEqual(new Uint8Array([0, 1, 2]));
  expect(new Uint8Array(options.user.id as ArrayBuffer)).toEqual(new Uint8Array([0xff, 0xef]));
  expect(options.user.name).toBe("ana@other.example");
  expect(options.rp).toEqual({ id: "dispatch.example", name: "Dispatch" });
  expect(options.pubKeyCredParams).toEqual([{ alg: -7, type: "public-key" }]);
  expect(options.authenticatorSelection).toEqual({
    residentKey: "required",
    userVerification: "required",
  });
  expect(options.attestation).toBe("none");
  expect(options.timeout).toBe(300_000);
  expect(options.extensions).toEqual({ credProps: true });
  const [excluded] = options.excludeCredentials ?? [];
  expect(new Uint8Array(excluded?.id as ArrayBuffer)).toEqual(new Uint8Array([0xfb]));
  expect(excluded?.transports).toEqual(["internal"]);
});

test("the sign-in's options become the browser's, naming no credential when Dispatch names none", () => {
  const options = requestOptions({
    publicKey: { challenge: "AAEC", rpId: "dispatch.example", userVerification: "required" },
  });
  expect(new Uint8Array(options.challenge as ArrayBuffer)).toEqual(new Uint8Array([0, 1, 2]));
  expect(options.rpId).toBe("dispatch.example");
  expect(options.userVerification).toBe("required");
  expect(options.allowCredentials).toBeUndefined();
});

/** A credential as a browser hands one back, with the buffers its response holds. */
function credential(response: Record<string, unknown>): PublicKeyCredential {
  return {
    authenticatorAttachment: "platform",
    getClientExtensionResults: () => ({ credProps: { rk: true } }),
    id: "-w",
    rawId: bytes(0xfb),
    response,
    type: "public-key",
  } as unknown as PublicKeyCredential;
}

test("a new passkey goes to Dispatch as JSON, every buffer base64url, with its transports", () => {
  expect(
    registrationJSON(
      credential({
        attestationObject: bytes(0, 1, 2),
        clientDataJSON: bytes(0xff, 0xef),
        getTransports: () => ["hybrid", "internal"],
      })
    )
  ).toEqual({
    authenticatorAttachment: "platform",
    clientExtensionResults: { credProps: { rk: true } },
    id: "-w",
    rawId: "-w",
    response: {
      attestationObject: "AAEC",
      clientDataJSON: "_-8",
      transports: ["hybrid", "internal"],
    },
    type: "public-key",
  });
});

test("a passkey's signature goes to Dispatch as JSON, with its user handle when the browser gives one", () => {
  const signed = {
    authenticatorData: bytes(0, 1, 2),
    clientDataJSON: bytes(0xff, 0xef),
    signature: bytes(0xfb),
  };
  expect(assertionJSON(credential({ ...signed, userHandle: bytes(0, 1, 2) }))).toEqual({
    authenticatorAttachment: "platform",
    clientExtensionResults: { credProps: { rk: true } },
    id: "-w",
    rawId: "-w",
    response: {
      authenticatorData: "AAEC",
      clientDataJSON: "_-8",
      signature: "-w",
      userHandle: "AAEC",
    },
    type: "public-key",
  });
  expect(assertionJSON(credential({ ...signed, userHandle: null })).response).toEqual({
    authenticatorData: "AAEC",
    clientDataJSON: "_-8",
    signature: "-w",
  });
});

// What a browser's own refusal of the ceremony tells the person: a cancel or a timeout, a device
// that holds a passkey for this address already, and anything else by the browser's name for it.
test("a browser's refusal of the ceremony is said in the person's words", () => {
  const refused = (name: string) => new DOMException("refused", name);
  expect(ceremonyFailure(refused("NotAllowedError"), "create")).toBe(
    "No passkey was made: the request was cancelled or timed out. Press the button to try again."
  );
  expect(ceremonyFailure(refused("NotAllowedError"), "get")).toBe(
    "No passkey was used: the request was cancelled or timed out, or this device holds no passkey for Dispatch. Try again."
  );
  expect(ceremonyFailure(refused("InvalidStateError"), "create")).toBe(
    "This device holds a passkey for this address already. Sign in with it instead."
  );
  expect(ceremonyFailure(refused("SecurityError"), "get")).toBe(
    "Your browser refused the passkey (SecurityError). Try again, or use another browser."
  );
  expect(ceremonyFailure(new TypeError("boom"), "create")).toBe(
    "Your browser refused the passkey (TypeError). Try again, or use another browser."
  );
});
