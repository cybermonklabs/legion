// packages/envoy-client/src/__tests__/dispatch-link-join.test.ts
//
// A dashboard link a command prints opens its page joined onto the path of the Dispatch URL the
// links are built on, with the page's own query and fragment. Resolved against that URL, the
// page's absolute path (`/issues/LEGION-1`) replaced the URL's path, so a dashboard served under
// a path (`http://host/prefix`) was linked at `http://host/issues/LEGION-1`. A URL with no path is
// linked as before, and a URL with a query builds no link. The stand-in records the path and the
// query each followed link asks.
//
// A dashboard URL an agent passes reads as its reference where it sits under that path, so a
// printed link reads back; read from the origin alone, the page under the path was no page.

import { afterEach, describe, expect, test } from "bun:test";
import { dispatchRefFromUrl, linkBaseOf, linkFor } from "../dispatch-commands/links";
import { parseDispatchRef } from "../dispatch-owner";

const stops: (() => unknown)[] = [];
afterEach(async () => {
  for (const stop of stops.splice(0)) await stop();
});

/** A dashboard stand-in that records `[path, query]` of each request and answers 200. */
function dashboard() {
  const asked: string[][] = [];
  const server = Bun.serve({
    hostname: "127.0.0.1",
    port: 0,
    fetch(request) {
      const url = new URL(request.url);
      asked.push([url.pathname, url.search]);
      return new Response("page");
    },
  });
  stops.push(() => server.stop(true));
  return { asked, origin: `http://127.0.0.1:${server.port}` };
}

const ISSUE_ASK = "/issues/LEGION-1/asks/a1";
const DOCUMENT_ASK = "/projects/CORE/documents/handbook-md?ask=a1";
const DOCUMENT_BLOCK = "/projects/CORE/documents/handbook-md?q=pelican#b-p1";

describe("a printed dashboard link opens its page under the Dispatch URL's path", () => {
  for (const [name, at, prefix] of [
    ["path", "/prefix", "/prefix"],
    ["path-trailing-slash", "/prefix/", "/prefix"],
    ["plain-origin", "", ""],
  ] as const) {
    test(name, async () => {
      const { asked, origin } = dashboard();
      const linkBase = linkBaseOf(`${origin}${at}`);
      const links = [ISSUE_ASK, DOCUMENT_ASK, DOCUMENT_BLOCK].map((href) =>
        linkFor(href, linkBase)
      );
      for (const link of links) await fetch(link);

      expect(asked).toEqual([
        [`${prefix}/issues/LEGION-1/asks/a1`, ""],
        [`${prefix}/projects/CORE/documents/handbook-md`, "?ask=a1"],
        [`${prefix}/projects/CORE/documents/handbook-md`, "?q=pelican"],
      ]);
      expect(links[2]).toBe(
        `${origin}${prefix}/projects/CORE/documents/handbook-md?q=pelican#b-p1`
      );
    });
  }

  test("a Dispatch URL with a query builds no link", () => {
    expect(linkBaseOf("http://127.0.0.1:8080/prefix?key=k3y-made-up")).toBeUndefined();
  });
});

describe("a dashboard URL under the Dispatch URL's path reads as its reference", () => {
  for (const [name, server, root] of [
    ["path", "http://dispatch.test/prefix", "http://dispatch.test/prefix"],
    ["path-trailing-slash", "http://dispatch.test/prefix/", "http://dispatch.test/prefix"],
    ["plain-origin", "http://dispatch.test", "http://dispatch.test"],
  ] as const) {
    test(name, () => {
      for (const href of [ISSUE_ASK, DOCUMENT_ASK]) {
        // The reference a command prints for the page where it builds no link.
        const reference = linkFor(href, undefined);
        expect(parseDispatchRef(reference)).toMatchObject({ kind: "ask", id: "a1" });
        expect(dispatchRefFromUrl(`${root}${href}`, server)).toBe(reference);
        expect(dispatchRefFromUrl(linkFor(href, linkBaseOf(server)), server)).toBe(reference);
      }
    });
  }

  test("a page beside the path is not one of the dashboard's", () => {
    const server = "http://dispatch.test/prefix";
    for (const page of [ISSUE_ASK, `/prefixed${ISSUE_ASK}`]) {
      expect(dispatchRefFromUrl(`http://dispatch.test${page}`, server)).toBeUndefined();
    }
  });
});
