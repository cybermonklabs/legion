import { expect, test } from "bun:test";
import { remoteText } from "./remote-text-matcher";

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
