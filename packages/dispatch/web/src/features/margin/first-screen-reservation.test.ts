import { expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import { HEADER_SCROLLS_QUERY } from "../issue-page/identity-block";
import {
  FIRST_SCREEN_MARGIN_PROPERTY,
  FIRST_SCREEN_SIDEBAR_PROPERTY,
  LAST_SHELL_LAYOUT_KEY,
  shellLayoutValue,
} from "../shell/last-shell-layout";
import { COMPACT_VIEWPORT_QUERY, PHONE_VIEWPORT_QUERY, XL_BREAKPOINT } from "../shell/useDialog";
import { DEFAULT_MARGIN_WIDTH, MIN_MARGIN_WIDTH } from "./margin-width";

/**
 * The shell's static reservation for the server's own draw of a document, pinned to the numbers
 * and the names it reserves from.
 *
 * `#first-screen` is written by the Go server into the page it sends, outside `#root`, and is on
 * the screen before any script of the page has run (`web/index.html`,
 * `internal/dispatch/routes/first_screen.go`). No component can hold columns open for it -
 * React has not mounted - so `web/src/styles.css` reserves them in CSS instead, and the head of
 * `web/index.html` narrows them to what this browser last drew (dispatch://INFRA-341, R11).
 *
 * Four files have to agree for that to work and none of them can import the others: a
 * stylesheet, a page of HTML the build copies through, the module the shell writes the layout
 * with, and the shell itself. They are held together here, so a renamed key, a renamed property
 * or a moved number fails this rather than silently leaving the server's blocks laid out at a
 * width the document does not keep. The reservation's width is the one every other width query at
 * `xl` of the stylesheet switches at. The stylesheet's two other width queries switch at 767 px
 * (`narrow-or-short`, as `PHONE_VIEWPORT_QUERY` writes it) and at 760 px (the new issue page's
 * header, as `HEADER_SCROLLS_QUERY` writes it), and each of them is held to the page's queries
 * here too.
 */

const read = (path: string) => readFileSync(join(import.meta.dir, path), "utf8");
const stylesCss = read("../../styles.css");
const appTsx = read("../../app.tsx");
const indexHtml = read("../../../index.html");
const firstScreenGo = read("../../../../../envoy/internal/dispatch/routes/first_screen.go");

/** `styles.css` without its comments: a query a comment names is no query of the sheet. */
const stylesRules = stylesCss.replace(/\/\*[\s\S]*?\*\//g, "");

/** The length a Tailwind theme token stands for: the sheet's own `@theme` where it declares the
 *  token, else Tailwind's `theme.css`, which its `@import "tailwindcss"` brings in. */
function themeLength(token: string): string | undefined {
  const declared = (css: string) => new RegExp(`${token}:\\s*([^;]+);`).exec(css)?.[1]?.trim();
  const tailwind = fileURLToPath(import.meta.resolve("tailwindcss/theme.css"));
  return declared(stylesRules) ?? declared(readFileSync(tailwind, "utf8"));
}

/** Below `xl` and from it, as every such `@media` of the sheet writes them: by the token the `xl:`
 *  classes compile from. */
const BELOW_XL = "(width < theme(--breakpoint-xl))";
const FROM_XL = "(width >= theme(--breakpoint-xl))";

/** The one rule in `styles.css` that lays the server's draw out: its breakpoint, the property
 *  on each side, and the default each property falls back to. */
const RESERVATION =
  /@media \(width >= theme\((?<breakpoint>--breakpoint-[a-z0-9]+)\)\) \{\n\s*#first-screen > \[data-first-screen-address\] \{\n\s*margin-left: var\((?<sidebarProperty>--[a-z-]+), calc\((?<sidebar>[^)]*)\)\);\n\s*margin-right: var\((?<marginProperty>--[a-z-]+), calc\((?<margin>\d+)px \+ (?<gutter>[^)]*)\)\);\n\s*\}\n\}/;

test("the server's draw is reserved for at exactly the widths above the compact viewport", () => {
  const found = RESERVATION.exec(stylesCss)?.groups;
  expect(found, "styles.css holds no reservation for the server's first-screen draw").toBeDefined();
  // The inverse of the query the shell reads: below it the margin is a sheet over the document
  // rather than a column beside it, and the sidebar is not drawn at all. Both are Tailwind's `xl`,
  // the sheet's by its token and the shell's by the length the token stands for, so the two
  // switch at one width whatever the reader's default font size.
  expect(found?.breakpoint).toBe("--breakpoint-xl");
  expect(themeLength("--breakpoint-xl")).toBe(XL_BREAKPOINT);
  expect(COMPACT_VIEWPORT_QUERY).toBe(`(width < ${XL_BREAKPOINT})`);
});

// Every width query of the sheet switches where one of the page's own queries does: at Tailwind's
// `xl`, written by the token the `xl:` classes compile from, which stands for the length the
// shell's queries are built from; in the variant the broadcast composer compacts under
// (`narrow-or-short`), at a phone's width, as `PHONE_VIEWPORT_QUERY` writes it; or, for the new
// issue page's header, at the width its header scrolls away, as `HEADER_SCROLLS_QUERY` writes it
// (a pixel width by design, paired with the header's `min-[761px]:` classes). A block written in a
// length of its own - `80rem` beside the token, or 1280 px, which a reader whose default font is
// not 16 px meets at another width than the shell's - fails it.
test("every width @media of styles.css switches at a width one of the page's queries does", () => {
  expect(themeLength("--breakpoint-xl"), "--breakpoint-xl is no longer the shell's xl").toBe(
    XL_BREAKPOINT
  );
  expect(
    BELOW_XL.replace("theme(--breakpoint-xl)", XL_BREAKPOINT),
    "below xl in the sheet is not the shell's compact query"
  ).toBe(COMPACT_VIEWPORT_QUERY);
  const widths = [...stylesRules.matchAll(/@media\s+([^{]+?)\s*\{/g)]
    .map(([, prelude = ""]) => prelude.trim())
    .filter((prelude) => prelude.includes("width"));
  expect(
    widths.filter((prelude) => prelude === BELOW_XL || prelude === FROM_XL).length,
    "styles.css reads fewer xl queries than the five it writes"
  ).toBeGreaterThanOrEqual(5);
  const known = new Set([BELOW_XL, FROM_XL, PHONE_VIEWPORT_QUERY, HEADER_SCROLLS_QUERY]);
  const strays = widths.filter((prelude) =>
    prelude
      .split(",")
      .map((clause) => clause.trim())
      .some((clause) => clause.includes("width") && !known.has(clause))
  );
  expect(strays, "a width @media of styles.css switches at a width of its own").toEqual([]);
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
    firstScreenGo,
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
