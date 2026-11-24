// The asymmetric matcher a test compares a held field by. A field another party wrote, such as an
// error's `code`, is a contracts RemoteText (`@legion/contracts/remote-text`), whose text is a `#`
// field: two boxes compare equal under `toEqual` whatever they hold, and a box never equals a
// string. `remoteText(text)` matches a box that holds `text` as it came, read through the box's own
// comparison (`equals`), so a test names the text it wants and prints none of the field. This
// package cannot import the contracts package, whose tests import it, so the box is known by its
// class name and its comparison method.

import { type AsymmetricMatcher, expect } from "bun:test";

declare module "bun:test" {
  interface AsymmetricMatchers {
    remoteText(text: string): AsymmetricMatcher;
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

expect.extend({
  remoteText(received: unknown, text: string) {
    return {
      pass: isRemoteText(received) && received.equals(text),
      message: () => `expected a RemoteText that holds ${JSON.stringify(text)}`,
    };
  },
});

/** Matches a contracts RemoteText that holds `text` as it came. */
export function remoteText(text: string): AsymmetricMatcher {
  return expect.remoteText(text);
}
