import { expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import { join } from "node:path";

import {
  FIRST_SCREEN_MARGIN_PROPERTY,
  FIRST_SCREEN_SIDEBAR_PROPERTY,
  LAST_SHELL_LAYOUT_KEY,
  shellLayoutValue,
} from "../shell/last-shell-layout";
import { COMPACT_VIEWPORT_QUERY } from "../shell/useDialog";
import { DEFAULT_MARGIN_WIDTH, MIN_MARGIN_WIDTH } from "./margin-width";

/**
 * The shell's static reservation for the server's own draw of a document, pinned to the numbers
 * and the names it reserves from.
 *
 * `#first-screen` is written by the Go server into the page it sends, outside `#root`, and is on
 * the screen before any script of the page has run (`web/index.html`,
 * `internal/dispatch/routes/router.go`). No component can hold columns open for it - React has
 * not mounted - so `web/src/styles.css` reserves them in CSS instead, and the head of
 * `web/index.html` narrows them to what this browser last drew (dispatch://INFRA-341, R11).
 *
 * Four files have to agree for that to work and none of them can import the others: a
 * stylesheet, a page of HTML the build copies through, the module the shell writes the layout
 * with, and the shell itself. They are held together here, so a renamed key, a renamed property
 * or a moved number fails this rather than silently leaving the server's blocks laid out at a
 * width the document does not keep.
 */

const read = (path: string) => readFileSync(join(import.meta.dir, path), "utf8");
const stylesCss = read("../../styles.css");
const appTsx = read("../../app.tsx");
const indexHtml = read("../../../index.html");
const routerGo = read("../../../../../envoy/internal/dispatch/routes/router.go");

/** The one rule in `styles.css` that lays the server's draw out: its breakpoint, the property
 *  on each side, and the default each property falls back to. */
const RESERVATION =
  /@media \(min-width: (?<breakpoint>\d+)px\) \{\n\s*#first-screen > \[data-first-screen-address\] \{\n\s*margin-left: var\((?<sidebarProperty>--[a-z-]+), calc\((?<sidebar>[^)]*)\)\);\n\s*margin-right: var\((?<marginProperty>--[a-z-]+), calc\((?<margin>\d+)px \+ (?<gutter>[^)]*)\)\);\n\s*\}\n\}/;

test("the server's draw is reserved for at exactly the widths above the compact viewport", () => {
  const found = RESERVATION.exec(stylesCss)?.groups;
  expect(found, "styles.css holds no reservation for the server's first-screen draw").toBeDefined();
  // The inverse of the query the shell reads: below it the margin is a sheet over the document
  // rather than a column beside it, and the sidebar is not drawn at all.
  const compact = /\(max-width: (\d+)px\)/.exec(COMPACT_VIEWPORT_QUERY);
  expect(compact, "COMPACT_VIEWPORT_QUERY is no longer a max-width query").not.toBeNull();
  expect(Number(found?.breakpoint)).toBe(Number(compact?.[1]) + 1);
});

test("the columns it falls back to are the ones the shell draws for a reader with no stored layout", () => {
  const found = RESERVATION.exec(stylesCss)?.groups;
  // `marginColumnWidth(undefined)`: what a browser gets when it has drawn no margin yet, and
  // what it gets when its storage cannot be read.
  expect(Number(found?.margin)).toBe(DEFAULT_MARGIN_WIDTH);
  // `w-80` is 20rem and `p-6` is 1.5rem, so both sides are written in the shell's own units and
  // move with the root font size exactly as the shell does.
  expect(found?.sidebar).toBe("20rem + 1.5rem");
  expect(found?.gutter).toBe("1.5rem");
  expect(
    appTsx,
    "the shell's sidebar is no longer the `w-80` column the reservation copies"
  ).toContain("order-1 w-80");
  expect(appTsx, "the shell's main is no longer padded `p-6`").toContain("min-w-0 flex-1 p-6");
});

test("the rule selects the element the page keeps and the attribute the server writes", () => {
  // Neither half of the selector is this package's to choose alone: `#first-screen` is the host
  // `web/index.html` keeps after `#root`, and `data-first-screen-address` is written by the Go
  // server onto the element it draws the blocks in. A rename of either leaves the rule matching
  // nothing, and the server's blocks laid out across the whole viewport again.
  expect(stylesCss).toContain("#first-screen > [data-first-screen-address]");
  expect(indexHtml, "web/index.html no longer keeps the `#first-screen` host").toContain(
    'id="first-screen"'
  );
  expect(
    routerGo,
    "the server no longer writes `data-first-screen-address` onto the element it draws"
  ).toContain("data-first-screen-address=");
});

test("the head's script sets the properties the rule reads, from the key the shell writes", () => {
  const found = RESERVATION.exec(stylesCss)?.groups;
  expect(found?.sidebarProperty).toBe(FIRST_SCREEN_SIDEBAR_PROPERTY);
  expect(found?.marginProperty).toBe(FIRST_SCREEN_MARGIN_PROPERTY);
  expect(
    indexHtml,
    "the head's script no longer sets the properties `styles.css` lays the server's draw out by"
  ).toContain(`setProperty("${FIRST_SCREEN_SIDEBAR_PROPERTY}"`);
  expect(indexHtml).toContain(`"${FIRST_SCREEN_MARGIN_PROPERTY}"`);
  expect(
    indexHtml,
    "the head's script no longer reads the key the shell writes its layout under"
  ).toContain(`getItem("${LAST_SHELL_LAYOUT_KEY}")`);
});

test("the shapes the shell writes are the shapes the head's script answers", () => {
  // One record with both columns in it, read back half by half.
  expect(shellLayoutValue(false, 520, false)).toBe("520/shown");
  expect(shellLayoutValue(true, 520, true)).toBe("rail/rail");
  expect(indexHtml, "the script no longer splits the record into its two halves").toContain(
    'stored.split("/")'
  );
  expect(indexHtml, "the script no longer answers a margin collapsed to its rail").toContain(
    'drawn[0] === "rail"'
  );
  expect(indexHtml, "the script no longer answers a sidebar hidden to its rail").toContain(
    'drawn[1] === "rail"'
  );
  // A rail is 5rem of gutter on either side, which is what the shell pads for one.
  expect(appTsx, "the shell no longer pads 5rem for the collapsed margin's rail").toContain(
    "xl:pr-20"
  );
  expect(appTsx, "the shell no longer pads 5rem for the hidden sidebar's rail").toContain(
    "xl:pl-20"
  );
  expect(indexHtml.match(/"5rem"/g) ?? []).toHaveLength(2);
  // And it clamps a stored margin width the way the margin's own column is clamped, so a width
  // stored on a wider screen cannot reserve most of a narrow one (`clampMarginWidth`).
  expect(indexHtml, "the script no longer clamps a stored width to the margin's minimum").toContain(
    `Math.max(Number(drawn[0]), ${MIN_MARGIN_WIDTH})`
  );
  expect(indexHtml, "the script no longer clamps a stored width to 60% of the viewport").toContain(
    "Math.floor(window.innerWidth * 0.6)"
  );
});
