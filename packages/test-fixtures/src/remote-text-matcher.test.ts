import { expect, test } from "bun:test";
import { remoteText, remoteTexts } from "./remote-text-matcher";

/** A stand-in for the contracts box: its class name, a `#` field and its comparison. */
class RemoteText {
  readonly #text: string;
  constructor(text: string) {
    this.#text = text;
  }
  equals(text: string): boolean {
    return this.#text === text;
  }
}

test("remoteText matches a box that holds the text as it came, and nothing else", () => {
  expect({ code: new RemoteText("NOT_FOUND"), status: 404 }).toMatchObject({
    code: remoteText("NOT_FOUND"),
    status: 404,
  });
  expect({ code: new RemoteText("NOT_FOUND") }).not.toMatchObject({ code: remoteText("FOUND") });
  // A string is no box, though it is the text.
  expect({ code: "NOT_FOUND" }).not.toMatchObject({ code: remoteText("NOT_FOUND") });
  // Another class with the same comparison is no box either.
  expect({ code: { equals: () => true } }).not.toMatchObject({ code: remoteText("NOT_FOUND") });
});

test("remoteTexts matches data whose every string is a box that holds it, and nothing else", () => {
  const held = [{ key: new RemoteText("DSP-9"), terms: 3, href: new RemoteText("/issues/DSP-9") }];
  expect({ candidates: held }).toMatchObject({
    candidates: remoteTexts([{ key: "DSP-9", terms: 3, href: "/issues/DSP-9" }]),
  });
  // Another text, a missing or an extra member, an extra item, or a plain string is no match.
  expect(held).not.toEqual(remoteTexts([{ key: "DSP-8", terms: 3, href: "/issues/DSP-9" }]));
  expect(held).not.toEqual(remoteTexts([{ key: "DSP-9", terms: 3 }]));
  expect(held).not.toEqual(remoteTexts([{ key: "DSP-9", terms: 3, href: "/issues/DSP-9", x: 1 }]));
  expect(held).not.toEqual(remoteTexts([]));
  expect([{ key: "DSP-9" }]).not.toEqual(remoteTexts([{ key: "DSP-9" }]));
});
