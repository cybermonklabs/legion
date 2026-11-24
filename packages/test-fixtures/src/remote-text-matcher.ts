// The asymmetric matchers a test compares a held field by. A field another party wrote, such as an
// error's `code`, is a contracts RemoteText (`@legion/contracts/remote-text`), whose text is a `#`
// field: two boxes compare equal under `toEqual` whatever they hold, and a box never equals a
// string. `remoteText(text)` matches a box that holds `text` as it came, and `remoteTexts(value)`
// matches data whose every string, at any depth, is such a box (a refusal's candidates, held
// whole), each read through the box's own comparison (`equals`), so a test names the text it wants
// and prints none of the field. This package cannot import the contracts package, whose tests
// import it, so the box is known by its class name and its comparison method.

import { type AsymmetricMatcher, expect } from "bun:test";

declare module "bun:test" {
  interface AsymmetricMatchers {
    remoteText(text: string): AsymmetricMatcher;
    remoteTexts(value: unknown): AsymmetricMatcher;
  }
}

/** Whether `value` is a contracts RemoteText: its class, and the comparison it is read by. */
function isRemoteText(value: unknown): value is { equals(text: string): boolean } {
  return (
    typeof value === "object" &&
    value !== null &&
    value.constructor.name === "RemoteText" &&
    "equals" in value &&
    typeof value.equals === "function"
  );
}

/** Whether `received` is `expected` with each string in it a box that holds that string: an
 *  array of as many items, each held; an object of the same names, each held; anything else
 *  itself. */
function holds(received: unknown, expected: unknown): boolean {
  if (typeof expected === "string") return isRemoteText(received) && received.equals(expected);
  if (Array.isArray(expected)) {
    return (
      Array.isArray(received) &&
      received.length === expected.length &&
      expected.every((item, at) => holds(received[at], item))
    );
  }
  if (typeof expected !== "object" || expected === null) return Object.is(received, expected);
  if (typeof received !== "object" || received === null || Array.isArray(received)) return false;
  const names = Object.keys(expected);
  return (
    names.length === Object.keys(received).length &&
    names.every((name) => holds(Reflect.get(received, name), Reflect.get(expected, name)))
  );
}

expect.extend({
  remoteText(received: unknown, text: string) {
    return {
      pass: isRemoteText(received) && received.equals(text),
      message: () => `expected a RemoteText that holds ${JSON.stringify(text)}`,
    };
  },
  remoteTexts(received: unknown, value: unknown) {
    return {
      pass: holds(received, value),
      message: () => `expected ${JSON.stringify(value)} with each string held as a RemoteText`,
    };
  },
});

/** Matches a contracts RemoteText that holds `text` as it came. */
export function remoteText(text: string): AsymmetricMatcher {
  return expect.remoteText(text);
}

/** Matches `value` with each string in it, at any depth, a RemoteText that holds that string. */
export function remoteTexts(value: unknown): AsymmetricMatcher {
  return expect.remoteTexts(value);
}
