import { afterEach, beforeEach, expect, test } from "bun:test";
import type { Window as HappyDOMWindow } from "happy-dom";

import { referenceRouteFromHref } from "../features/refs/routes";
import { createApiClient } from "./client";
import { dispatchOrigins, hereHref, setDispatchOrigins } from "./origins";

/** The test DOM is happy-dom's window (`__tests__/register-dom.ts` registers it), which
 *  TypeScript knows only as the DOM's; each test puts the page on the served address first. */
const testWindow = window as unknown as HappyDOMWindow;

const CANONICAL = "https://a.example.test";
const SERVED = "https://b.example.test";
const FORMER = "https://old.example.test";
const OTHER = "https://other.example.test";

/** The route a link to CORE-1's page names. */
const CORE_1 = { key: "CORE-1", kind: "issue" } as const;

let pageBefore = "about:blank";

/** Puts the page on `origin`. */
function pageAt(origin: string): void {
  testWindow.happyDOM.setURL(`${origin}/issues/CORE-2`);
}

beforeEach(() => {
  pageBefore = window.location.href;
  pageAt(SERVED);
});

afterEach(() => {
  testWindow.happyDOM.setURL(pageBefore);
});

/** A client whose `/auth/whoami` answers `answer`, as a Dispatch of that deployment does. */
function whoamiAnswering(answer: Record<string, unknown>) {
  return createApiClient(async () => Response.json(answer));
}

/** A person's whoami answer naming `origins`, or naming none where they are left out, as a
 *  Dispatch that lists no second address answers, and one older than the lists. */
function signedIn(origins?: readonly string[]): Record<string, unknown> {
  return {
    event_head: "0",
    kind: "user",
    login: "alice@a.example.test",
    ...(origins === undefined ? {} : { origins }),
  };
}

test("an href of the canonical address opens on the served address the page is on", () => {
  setDispatchOrigins([CANONICAL, SERVED]);

  expect(hereHref(`${CANONICAL}/issues/A-1?x=1#b-2`)).toBe(`${SERVED}/issues/A-1?x=1#b-2`);
});

test("an href of an origin Dispatch does not name is left as it is", () => {
  setDispatchOrigins([CANONICAL, SERVED]);

  expect(hereHref(`${OTHER}/issues/A-1?x=1#b-2`)).toBe(`${OTHER}/issues/A-1?x=1#b-2`);
});

test("an href that names no origin already opens on the page's own, and is left as it is", () => {
  setDispatchOrigins([CANONICAL, SERVED]);

  expect(hereHref("/issues/A-1?x=1#b-2")).toBe("/issues/A-1?x=1#b-2");
});

test("the page's own origin comes first, then each other origin whoami named", async () => {
  await whoamiAnswering(signedIn([CANONICAL, SERVED, FORMER])).whoAmI();

  expect(dispatchOrigins()).toEqual([SERVED, CANONICAL, FORMER]);
});

// A Dispatch that lists no second address names none, and the page reads every dashboard URL
// against its own origin alone, as it always has.
test("a whoami answer with no origins key leaves one origin, the page's own", async () => {
  await whoamiAnswering(signedIn([CANONICAL, SERVED])).whoAmI();
  expect(dispatchOrigins()).toEqual([SERVED, CANONICAL]);

  await whoamiAnswering(signedIn()).whoAmI();

  expect(dispatchOrigins()).toEqual([window.location.origin]);
});

// A reading memoized on the origins (`useTextReferences`) is redone only when they change, and
// every refetch of whoami names them again.
test("the same origins named again keep one list", () => {
  setDispatchOrigins([CANONICAL, SERVED]);
  const first = dispatchOrigins();

  setDispatchOrigins([CANONICAL, SERVED]);

  expect(dispatchOrigins()).toBe(first);
});

// INFRA-5491. A deployment serves addresses beside its canonical one and still reads links at an
// address it served before, and whoami names them all: the canonical address, each served
// address, each former one. The rows below are the states of the page's list, each read as a
// link is read: whether it is a reference (`referenceRouteFromHref`) and where it opens
// (`hereHref`). The state before whoami answers is `app.test.tsx`'s row "nothing is read and no
// socket opens until whoami answers, and the feed resumes from its event_head": `AuthGate`
// mounts no page, and so no reader of the list, until whoami has answered.

test("with no origins in whoami's answer, a link at another address of the deployment stays external", async () => {
  pageAt(SERVED);
  await whoamiAnswering(signedIn()).whoAmI();

  expect(dispatchOrigins()).toEqual([SERVED]);
  expect(referenceRouteFromHref(`${CANONICAL}/issues/CORE-1`)).toBeUndefined();
  expect(hereHref(`${CANONICAL}/issues/CORE-1?x=1#b-2`)).toBe(`${CANONICAL}/issues/CORE-1?x=1#b-2`);
});

test("a link at a served or a former address whoami names is a reference, and opens on the page's own origin", async () => {
  pageAt(CANONICAL);
  await whoamiAnswering(signedIn([CANONICAL, SERVED, FORMER])).whoAmI();

  for (const origin of [SERVED, FORMER]) {
    expect(referenceRouteFromHref(`${origin}/issues/CORE-1`), origin).toEqual(CORE_1);
    expect(hereHref(`${origin}/issues/CORE-1?x=1#b-2`), origin).toBe(
      `${CANONICAL}/issues/CORE-1?x=1#b-2`
    );
  }
});

test("beside the lists whoami names, a link at a host it does not name stays external", async () => {
  pageAt(CANONICAL);
  await whoamiAnswering(signedIn([CANONICAL, SERVED, FORMER])).whoAmI();

  expect(referenceRouteFromHref(`${OTHER}/issues/CORE-1`)).toBeUndefined();
  expect(hereHref(`${OTHER}/issues/CORE-1?x=1#b-2`)).toBe(`${OTHER}/issues/CORE-1?x=1#b-2`);
});

// A deployment that adds or drops an address between two whoami answers changes what the page
// reads at the next read. Every refetch names the lists again, and a reading memoized on them
// (`useTextReferences`) is redone only when they change.
test("the page reads the lists of whoami's newest answer, and keeps one list while they stay the same", async () => {
  pageAt(CANONICAL);
  await whoamiAnswering(signedIn([CANONICAL, SERVED])).whoAmI();
  const first = dispatchOrigins();
  expect(referenceRouteFromHref(`${FORMER}/issues/CORE-1`)).toBeUndefined();

  await whoamiAnswering(signedIn([CANONICAL, SERVED])).whoAmI();
  expect(dispatchOrigins()).toBe(first);

  await whoamiAnswering(signedIn([CANONICAL, SERVED, FORMER])).whoAmI();
  const second = dispatchOrigins();
  expect(second).not.toBe(first);
  expect(second).toEqual([CANONICAL, SERVED, FORMER]);
  expect(referenceRouteFromHref(`${FORMER}/issues/CORE-1`)).toEqual(CORE_1);

  await whoamiAnswering(signedIn([CANONICAL, SERVED, FORMER])).whoAmI();
  expect(dispatchOrigins()).toBe(second);

  await whoamiAnswering(signedIn([CANONICAL, FORMER])).whoAmI();
  expect(referenceRouteFromHref(`${SERVED}/issues/CORE-1`)).toBeUndefined();
});

test("on a served address, a link at the canonical address is a reference that opens on the served one", async () => {
  pageAt(SERVED);
  await whoamiAnswering(signedIn([CANONICAL, SERVED, FORMER])).whoAmI();

  expect(referenceRouteFromHref(`${CANONICAL}/issues/CORE-1`)).toEqual(CORE_1);
  expect(hereHref(`${CANONICAL}/issues/CORE-1?x=1#b-2`)).toBe(`${SERVED}/issues/CORE-1?x=1#b-2`);
});

// A browser writes a host in lower case, and so does whoami, but an author may write a link's
// host in capitals, and it names the same address: the page reads it so, as the server does,
// which compares hosts without case.
test("a link whose host is written in capitals is a reference at the address it names", async () => {
  pageAt(CANONICAL);
  await whoamiAnswering(signedIn([CANONICAL, SERVED, FORMER])).whoAmI();

  expect(referenceRouteFromHref("https://B.EXAMPLE.TEST/issues/CORE-1")).toEqual(CORE_1);
  expect(hereHref("https://B.EXAMPLE.TEST/issues/CORE-1?x=1#b-2")).toBe(
    `${CANONICAL}/issues/CORE-1?x=1#b-2`
  );
});
