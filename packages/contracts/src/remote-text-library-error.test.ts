// packages/contracts/src/remote-text-library-error.test.ts
//
// `LibraryError.answeredOAuthError` says the library threw oauth4webapi's `ResponseBodyError`, an
// OAuth error the far side answered in a body, by the code the library gives that class. Every
// client read it as the error's name before, which is the class's `constructor.name`: a bundle
// that renames the class, or a subclass, names the same error otherwise, and any error can carry
// the name.

import { describe, expect, test } from "bun:test";
import { LibraryError } from "./remote-text";

/** An error shaped as oauth4webapi 3.8.8 builds its `ResponseBodyError`, named `name`, with the
 *  library's code for the class where `code` is given, and the answer's status and words. */
function answered(name: string, code?: string): Error {
  return Object.assign(new Error("server responded with an error in the response body"), {
    name,
    status: 400,
    error: "invalid_grant",
    error_description: "the issuer refused this request",
    ...(code === undefined ? {} : { code }),
  });
}

describe("answeredOAuthError reads the library's code for the class, not the error's name", () => {
  for (const [row, thrown, want] of [
    ["response-body-error", answered("ResponseBodyError", "OAUTH_RESPONSE_BODY_ERROR"), true],
    ["renamed-class", answered("e", "OAUTH_RESPONSE_BODY_ERROR"), true],
    ["subclass", answered("RenamedResponseBodyError", "OAUTH_RESPONSE_BODY_ERROR"), true],
    ["name-only", answered("ResponseBodyError"), false],
    [
      "challenge",
      Object.assign(new Error("server responded with a challenge in the WWW-Authenticate header"), {
        name: "WWWAuthenticateChallengeError",
        code: "OAUTH_WWW_AUTHENTICATE_CHALLENGE",
        status: 401,
      }),
      false,
    ],
    ["no-answer", new TypeError("fetch failed"), false],
  ] as const) {
    test(row, () => {
      expect(new LibraryError(thrown).answeredOAuthError).toBe(want);
    });
  }
});
