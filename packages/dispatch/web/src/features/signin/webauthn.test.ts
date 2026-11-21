import { afterEach, beforeEach, expect, test } from "bun:test";
import type { PasskeyCreationOptions } from "@legion/contracts";

import { installFakePasskeys } from "../../__tests__/fake-passkeys";
import { makePasskey, signWithPasskey } from "./webauthn";

const bytes = (...values: number[]) => new Uint8Array(values).buffer;

let browser: ReturnType<typeof installFakePasskeys>;

beforeEach(() => {
  browser = installFakePasskeys();
});

afterEach(() => {
  browser.restore();
});

/** `/auth/passkey/register/begin`'s answer, with every option the server can send and
 *  `challenge` as its challenge. */
function creation(challenge = "AAEC"): PasskeyCreationOptions {
  return {
    publicKey: {
      attestation: "none",
      authenticatorSelection: { residentKey: "required", userVerification: "required" },
      challenge,
      excludeCredentials: [{ id: "-w", transports: ["internal"], type: "public-key" }],
      extensions: { credProps: true },
      pubKeyCredParams: [{ alg: -7, type: "public-key" }],
      rp: { id: "dispatch.example", name: "Dispatch" },
      timeout: 300_000,
      user: { displayName: "ana@other.example", id: "_-8", name: "ana@other.example" },
    },
  };
}

/** A credential as a browser hands one back, with the buffers its response holds. */
function credential(
  response: Record<string, unknown>,
  rawId: ArrayBuffer | ArrayBufferView = bytes(0xfb)
): Credential {
  return {
    authenticatorAttachment: "platform",
    getClientExtensionResults: () => ({ credProps: { rk: true } }),
    id: "-w",
    rawId,
    response,
    type: "public-key",
  } as unknown as Credential;
}

/** A new passkey's response, with `value` for each of its buffers. */
function attestation(value: ArrayBuffer = bytes(0, 1, 2)) {
  return {
    attestationObject: value,
    clientDataJSON: bytes(0xff, 0xef),
    getTransports: () => ["hybrid", "internal"],
  };
}

// The server writes every buffer as base64url without padding, go-webauthn's
// URLEncodedBase64; the page reads it into bytes and writes the browser's bytes back the same
// way. The cases cover each padding length and the two characters base64url changes, and a
// buffer the browser hands back as a view rather than an ArrayBuffer.
test("base64url round-trips every padding length and the characters base64 spells otherwise", async () => {
  for (const [text, value] of [
    ["", bytes()],
    ["-w", bytes(0xfb)],
    ["_-8", bytes(0xff, 0xef)],
    ["AAEC", bytes(0, 1, 2)],
    ["-_8A_w", bytes(0xfb, 0xff, 0x00, 0xff)],
  ] as const) {
    browser.create.mockResolvedValueOnce(credential(attestation(value), new Uint8Array(value)));
    const made = await makePasskey(creation(text));
    const asked = browser.create.mock.calls.at(-1)?.[0].publicKey;
    expect(new Uint8Array(asked?.challenge as ArrayBuffer)).toEqual(new Uint8Array(value));
    expect(made.rawId).toBe(text);
    expect(made.response.attestationObject).toBe(text);
  }
  const asked = browser.create.mock.calls.length;
  await expect(makePasskey(creation("not base64url!"))).rejects.toThrow();
  expect(browser.create).toHaveBeenCalledTimes(asked);
});

test("the enrollment's options become the browser's, with every id and the challenge as bytes", async () => {
  await makePasskey(creation());
  const options = browser.create.mock.calls.at(-1)?.[0].publicKey;
  if (options === undefined) throw new Error("navigator.credentials.create was asked nothing");
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

test("the sign-in's options become the browser's, naming no credential when Dispatch names none", async () => {
  await signWithPasskey({
    publicKey: { challenge: "AAEC", rpId: "dispatch.example", userVerification: "required" },
  });
  const options = browser.get.mock.calls.at(-1)?.[0].publicKey;
  if (options === undefined) throw new Error("navigator.credentials.get was asked nothing");
  expect(new Uint8Array(options.challenge as ArrayBuffer)).toEqual(new Uint8Array([0, 1, 2]));
  expect(options.rpId).toBe("dispatch.example");
  expect(options.userVerification).toBe("required");
  expect(options.allowCredentials).toBeUndefined();
});

test("a new passkey goes to Dispatch as JSON, every buffer base64url, with its transports", async () => {
  browser.create.mockResolvedValueOnce(credential(attestation()));
  expect(await makePasskey(creation())).toEqual({
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

test("a passkey's signature goes to Dispatch as JSON, with its user handle when the browser gives one", async () => {
  const signed = {
    authenticatorData: bytes(0, 1, 2),
    clientDataJSON: bytes(0xff, 0xef),
    signature: bytes(0xfb),
  };
  const options = { publicKey: { challenge: "AAEC" } };
  browser.get.mockResolvedValueOnce(credential({ ...signed, userHandle: bytes(0, 1, 2) }));
  expect(await signWithPasskey(options)).toEqual({
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
  browser.get.mockResolvedValueOnce(credential({ ...signed, userHandle: null }));
  expect((await signWithPasskey(options)).response).toEqual({
    authenticatorData: "AAEC",
    clientDataJSON: "_-8",
    signature: "-w",
  });
});

// A browser that answers the ceremony with no credential made or used none, as a cancel does, so
// the page says what it says for a cancel.
test("a browser that answers with no credential is refused as a cancel is", async () => {
  browser.create.mockResolvedValueOnce(null);
  await expect(makePasskey(creation())).rejects.toMatchObject({ name: "NotAllowedError" });
  browser.get.mockResolvedValueOnce(null);
  await expect(signWithPasskey({ publicKey: { challenge: "AAEC" } })).rejects.toMatchObject({
    name: "NotAllowedError",
  });
});
