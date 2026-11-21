import { expect, test } from "bun:test";

import { ceremonyRefusal } from "./refusals";

// What a browser's own refusal of the ceremony tells the person: a cancel or a timeout, a device
// that holds a passkey for this address already, and anything else by the browser's name for it.
// Each can go another way on a second press.
test("a browser's refusal of the ceremony is said in the person's words", () => {
  const refused = (name: string) => new DOMException("refused", name);
  for (const [error, ceremony, message] of [
    [
      refused("NotAllowedError"),
      "create",
      "No passkey was made: the request was cancelled or timed out. Press the button to try again.",
    ],
    [
      refused("NotAllowedError"),
      "get",
      "No passkey was used: the request was cancelled or timed out, or this device holds no passkey for Dispatch. Try again.",
    ],
    [
      refused("InvalidStateError"),
      "create",
      "This device holds a passkey for this address already. Sign in with it instead.",
    ],
    [
      refused("SecurityError"),
      "get",
      "Your browser refused the passkey (SecurityError). Try again, or use another browser.",
    ],
    [
      new TypeError("boom"),
      "create",
      "Your browser refused the passkey (TypeError). Try again, or use another browser.",
    ],
  ] as const) {
    expect(ceremonyRefusal(error, ceremony)).toEqual({ message, retry: true });
  }
});
