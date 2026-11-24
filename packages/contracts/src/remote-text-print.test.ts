import { expect, test } from "bun:test";
import { printTest, REMOTE_WORDS } from "@legion/test-fixtures/print-test";
import { LibraryError, RemoteText } from "./remote-text";

// The TypeScript box print test of the box itself: RemoteText holding the probe, by every printer
// at every placement, beside its twin, the probe as a plain string.
test("box print test: RemoteText prints no secret by any printer at any placement", () => {
  const failures = printTest({
    name: "contracts.RemoteText",
    kind: "text",
    boxed: new RemoteText(REMOTE_WORDS),
    same: REMOTE_WORDS,
  });
  expect(failures.map((f) => f.why)).toEqual([]);
});

/** A LibraryError's twin: an Error whose code and description are public strings, as they came,
 *  with the classes LibraryError keeps, and whose message quotes the library's own message as it
 *  came. */
class LibraryErrorTwin extends Error {
  readonly status = 400;
  readonly code = REMOTE_WORDS;
  readonly description = REMOTE_WORDS;
  readonly thrownClass = "ResponseBodyError (message withheld)";
  readonly causeClass = "Error (message withheld)";
  constructor() {
    super(REMOTE_WORDS);
    this.name = "ResponseBodyError";
  }
}

// The box print test of the library boundary's holder: a LibraryError over an error the library
// threw whose message, `error`, `error_description` and cause's message each quote the probe.
test("box print test: LibraryError prints no secret by any printer at any placement", () => {
  const thrown = Object.assign(new Error(REMOTE_WORDS, { cause: new Error(REMOTE_WORDS) }), {
    name: "ResponseBodyError",
    status: 400,
    error: REMOTE_WORDS,
    error_description: REMOTE_WORDS,
  });
  const failures = printTest({
    name: "contracts.LibraryError",
    kind: "error",
    boxed: new LibraryError(thrown),
    same: new LibraryErrorTwin(),
  });
  expect(failures.map((f) => f.why)).toEqual([]);
});
