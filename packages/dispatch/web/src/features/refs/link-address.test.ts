import { expect, test } from "bun:test";
import type { Window as HappyDOMWindow } from "happy-dom";

import { linkAddress } from "./link-address";

/** An element built from `html`, as a page draws it. */
function element(html: string): Element {
  const holder = document.createElement("div");
  holder.innerHTML = html;
  const built = holder.firstElementChild;
  if (built === null) throw new Error(`no element in ${html}`);
  return built;
}

test("a rendered body's reference link copies the reference it carries", () => {
  expect(
    linkAddress(
      element('<a href="/issues/CORE-1/spec" data-dispatch-ref="dispatch://CORE-1/spec">x</a>')
    )
  ).toBe("dispatch://CORE-1/spec");
});

test("an editor's link, title or picture link copies its target as the node's own reference", () => {
  // A link mark: the sanitizer emptied its `href` and moved the target.
  expect(linkAddress(element('<a href="" data-dispatch-href="dispatch://CORE-2">x</a>'))).toBe(
    "dispatch://CORE-2"
  );
  // A title the editor draws over a bare key: its `href` is the page, its target the reference.
  expect(
    linkAddress(
      element('<a href="/issues/CORE-2" data-dispatch-href="dispatch://CORE-2">CORE-2 Fix</a>')
    )
  ).toBe("dispatch://CORE-2");
  expect(
    linkAddress(
      element(
        '<a href="/issues/CORE-1/artifacts/notes-md?v=1" data-dispatch-href="dispatch://CORE-1/artifact/notes-md@v1">x</a>'
      )
    )
  ).toBe("dispatch://CORE-1/artifact/notes-md@v1");
  // A target the reference grammar refuses is copied as written.
  expect(linkAddress(element('<a href="" data-dispatch-href="dispatch://">x</a>'))).toBe(
    "dispatch://"
  );
});

test("a link to a page of Dispatch copies the reference of the item the page shows", () => {
  expect(linkAddress(element('<a href="http://dispatch.test/issues/CORE-2">x</a>'))).toBe(
    "dispatch://CORE-2"
  );
  expect(linkAddress(element('<a href="/issues/CORE-2/spec">x</a>'))).toBe(
    "dispatch://CORE-2/spec"
  );
});

test("a page that shows no item, another site and a mail address copy the link's URL", () => {
  expect(linkAddress(element('<a href="/">Inbox</a>'))).toBe("http://dispatch.test/");
  expect(linkAddress(element('<a href="https://example.com/a">x</a>'))).toBe(
    "https://example.com/a"
  );
  expect(linkAddress(element('<a href="mailto:a@b.test">a@b.test</a>'))).toBe("mailto:a@b.test");
});

test("an element with no address is no link to copy", () => {
  expect(linkAddress(element("<a>no href</a>"))).toBeUndefined();
  expect(linkAddress(element('<span role="link">x</span>'))).toBeUndefined();
});

// The sanitizer draws a link whose scheme it refuses (`javascript:`, `data:`) as `<a href="">`
// with no data attribute. An empty `href` reads as the page the reader is on, here an issue's
// spec, whose reference the link never named.
test("a link whose address the sanitizer removed is no link to copy, on an item's own page too", () => {
  const testWindow = window as unknown as HappyDOMWindow;
  const pageBefore = window.location.href;
  testWindow.happyDOM.setURL("http://dispatch.test/issues/CORE-9/spec");
  try {
    for (const html of ['<a href="">run me</a>', '<a href="  ">run me</a>']) {
      expect({ address: linkAddress(element(html)), html }).toEqual({ address: undefined, html });
    }
  } finally {
    testWindow.happyDOM.setURL(pageBefore);
  }
});
