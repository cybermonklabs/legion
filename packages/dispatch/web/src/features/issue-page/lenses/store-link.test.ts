import { expect, test } from "bun:test";

import { buildIssuePath, parseIssuePath } from "../../refs/routes";
import { outlineRowPath, storeLinkPath } from "./store-link";

/** The page of the issue CORE-1, as the router holds it. */
const PAGE = { pathname: "/issues/CORE-1", search: "" };

const RISK = "d4000000-0000-4000-8000-000000000004";
const LENS = "c3000000-0000-4000-8000-000000000003";

// A row's link to a store address opens its title only where that title is the path a name's
// link holds: the outline of the page's issue at the `?node=` of the node the address names,
// written as `outlineRowPath` writes it. Every other title opens nothing.
test("storeLinkPath takes only the path a name's link holds", () => {
  const risk = `dispatch://risk/${RISK}`;
  const finding = `dispatch://lens/${LENS}#b-first`;

  // The paths a name's link holds: the node's read id, bare or after its kind, and a finding's
  // by its block id alone or by its lens's read id and its block id.
  for (const [href, readId] of [
    [risk, RISK],
    [risk, `risk/${RISK}`],
    [finding, "first"],
    [finding, `lens/${LENS}#first`],
  ] as const) {
    const path = outlineRowPath("CORE-1", readId);
    expect(storeLinkPath(href, path, PAGE), `${href} at ${readId}`).toBe(path);
  }
  // The same paths on a page of the issue under another view: the issue is the page's.
  const spec = { pathname: "/issues/CORE-1/spec", search: "" };
  expect(storeLinkPath(risk, outlineRowPath("CORE-1", RISK), spec)).toBe(
    outlineRowPath("CORE-1", RISK)
  );

  // Titles that are not that path open nothing.
  const refused: readonly (readonly [string, string, typeof PAGE])[] = [
    // Another issue's outline, the node's own read id.
    [risk, outlineRowPath("OTHER-1", RISK), PAGE],
    // The page's outline at another node.
    [risk, outlineRowPath("CORE-1", "e5000000-0000-4000-8000-000000000005"), PAGE],
    // A finding's row under another lens.
    [finding, outlineRowPath("CORE-1", "lens/b2000000-0000-4000-8000-000000000002#first"), PAGE],
    // The node's row with a words filter the name's link does not hold.
    [risk, outlineRowPath("CORE-1", RISK, "failover"), PAGE],
    // The node's row at another view of the issue.
    [risk, `${buildIssuePath({ key: "CORE-1", kind: "spec" })}?node=${RISK}`, PAGE],
    // A title with no query, and none at all.
    [risk, buildIssuePath({ key: "CORE-1", kind: "issue" }), PAGE],
    [risk, "", PAGE],
    // An address that is no store address, written as a name's link would be.
    ["dispatch://CORE-2", outlineRowPath("CORE-1", "CORE-2"), PAGE],
    // A page that is no issue's.
    [risk, outlineRowPath("CORE-1", RISK), { pathname: "/inbox", search: "" }],
  ];
  for (const [href, title, page] of refused) {
    expect(storeLinkPath(href, title, page), `${href} titled ${title} on ${page.pathname}`).toBe(
      undefined
    );
  }
});

// The row's path is the issue's own path as the router builds it (`buildIssuePath`), which the
// page's slots read back as the issue with no view (`parseIssuePath`), and its query names the node
// by its read id, after the words filter where one is given.
test("outlineRowPath builds the row's path with buildIssuePath", () => {
  const readId = `lens/${LENS}#first`;
  for (const filter of [undefined, "two regions"]) {
    const path = outlineRowPath("CORE-1", readId, filter);
    const query = path.indexOf("?");
    const pathname = path.slice(0, query);
    const search = path.slice(query);
    expect(pathname, `the filter ${filter}`).toBe(buildIssuePath({ key: "CORE-1", kind: "issue" }));
    expect(parseIssuePath(pathname, search), `the filter ${filter}`).toMatchObject({
      key: "CORE-1",
      kind: "issue",
    });
    expect([...new URLSearchParams(search)], `the filter ${filter}`).toEqual(
      filter === undefined
        ? [["node", readId]]
        : [
            ["q", filter],
            ["node", readId],
          ]
    );
  }
});
