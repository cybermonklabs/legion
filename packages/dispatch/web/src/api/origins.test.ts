import { afterEach, beforeEach, expect, test } from "bun:test";
import type { Window as HappyDOMWindow } from "happy-dom";

import { createApiClient } from "./client";
import { dispatchOrigins, hereHref, setDispatchOrigins } from "./origins";

/** The test DOM is happy-dom's window (`__tests__/register-dom.ts` registers it), which
 *  TypeScript knows only as the DOM's; each test puts the page on the outside origin. */
const testWindow = window as unknown as HappyDOMWindow;

const CANON = "https://canon.test";
const OUTSIDE = "https://outside.test";

let pageBefore = "about:blank";

beforeEach(() => {
  pageBefore = window.location.href;
  testWindow.happyDOM.setURL(`${OUTSIDE}/issues/A-2`);
});

afterEach(() => {
  testWindow.happyDOM.setURL(pageBefore);
});

/** A client whose `/auth/whoami` answers `answer`, as a Dispatch of that deployment does. */
function whoamiAnswering(answer: Record<string, unknown>) {
  return createApiClient(async () => Response.json(answer));
}

/** A restricted member's answer from a Dispatch that answers at both origins. */
const outsideAnswer = {
  event_head: "0",
  kind: "user",
  login: "alice@outside.test",
  member_kind: "restricted",
  origins: [CANON, OUTSIDE],
};

test("an href of the canonical origin opens on the outside origin the page is on", () => {
  setDispatchOrigins([CANON, OUTSIDE]);

  expect(hereHref(`${CANON}/issues/A-1?x=1#b-2`)).toBe(`${OUTSIDE}/issues/A-1?x=1#b-2`);
});

test("an href of an origin Dispatch does not answer at is left as it is", () => {
  setDispatchOrigins([CANON, OUTSIDE]);

  expect(hereHref("https://evil.test/issues/A-1?x=1#b-2")).toBe(
    "https://evil.test/issues/A-1?x=1#b-2"
  );
});

test("an href that names no origin already opens on the page's own, and is left as it is", () => {
  setDispatchOrigins([CANON, OUTSIDE]);

  expect(hereHref("/issues/A-1?x=1#b-2")).toBe("/issues/A-1?x=1#b-2");
});

test("the page's own origin comes first, then each other origin whoami named", async () => {
  await whoamiAnswering(outsideAnswer).whoAmI();

  expect(dispatchOrigins()).toEqual([OUTSIDE, CANON]);
});

// TestWhoamiWithNoOriginsKeyLeavesOneOrigin: a Dispatch that answers at one origin names none,
// and the page reads every dashboard URL against its own origin alone, as it always has.
test("a whoami answer with no origins key leaves one origin, the page's own", async () => {
  await whoamiAnswering(outsideAnswer).whoAmI();
  expect(dispatchOrigins()).toEqual([OUTSIDE, CANON]);

  await whoamiAnswering({ event_head: "0", kind: "user", login: "alice@canon.test" }).whoAmI();

  expect(dispatchOrigins()).toEqual([window.location.origin]);
});

// A reading memoized on the origins (`useTextReferences`) is redone only when they change, and
// every refetch of whoami names them again.
test("the same origins named again keep one list", () => {
  setDispatchOrigins([CANON, OUTSIDE]);
  const first = dispatchOrigins();

  setDispatchOrigins([CANON, OUTSIDE]);

  expect(dispatchOrigins()).toBe(first);
});
