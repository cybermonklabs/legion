import { afterEach, expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import { join } from "node:path";

import {
  FIRST_SCREEN_MARGIN_PROPERTY,
  FIRST_SCREEN_SIDEBAR_PROPERTY,
  LAST_SHELL_LAYOUT_KEY,
  shellLayoutValue,
} from "../shell/last-shell-layout";
import { clampMarginWidth, MIN_MARGIN_WIDTH } from "./margin-width";

/**
 * The head's own script, run rather than read.
 *
 * `first-screen-reservation.test.ts` holds `web/index.html` to the names and numbers it shares
 * with `styles.css` and the shell, as text; `last-shell-layout.test.ts` covers the writer. What
 * neither does is execute the reader, and that script is what lays the server's blocks out on
 * every first paint (dispatch://INFRA-341, R11): a value it reads too loosely would put a
 * reader's own stored text into a `style` property, and one it reads too strictly would leave
 * every reader the default columns and the re-wrap this is here to stop.
 *
 * So the script's text is taken out of the page and called, with a `window` of this row's own -
 * the stored value and the viewport's width - over this document's `documentElement`. Each row
 * asserts both halves: the property the script sets, and the property it leaves alone.
 */

const indexHtml = readFileSync(join(import.meta.dir, "../../../index.html"), "utf8");

/** The one classic script of the page that reads the stored layout, as the page holds it. */
function headScriptSource(): string {
  const holders = indexHtml
    .split("<script>")
    .filter((part) => part.includes(`getItem("${LAST_SHELL_LAYOUT_KEY}")`));
  expect(
    holders,
    "web/index.html no longer holds exactly one script reading the layout"
  ).toHaveLength(1);
  return (holders[0] ?? "").split("</script>")[0] ?? "";
}

/** The script with its own `window` shadowed by this row's, so a row names the stored value and
 *  the viewport width without touching this process's storage. `document` stays the global one:
 *  the properties the script sets are what each row reads back. */
const runHeadScript = new Function("window", headScriptSource()) as (window: unknown) => void;

interface StoredLayout {
  /** What `localStorage.getItem` answers, or `"throws"` for storage this browser refuses to
   *  read at all (a browser with site data blocked). */
  readonly stored: string | null | "throws";
  readonly innerWidth?: number;
}

function run({ stored, innerWidth = 1280 }: StoredLayout): void {
  // On both windows: the one the script reads, and this process's own, which is where
  // `clampMarginWidth` reads the viewport from (`maxMarginWidth`). A row that expects the rule
  // and the script then names one width, and neither can be read against the other's.
  Object.defineProperty(window, "innerWidth", { configurable: true, value: innerWidth });
  runHeadScript({
    innerWidth,
    localStorage: {
      getItem(key: string): string | null {
        expect(key).toBe(LAST_SHELL_LAYOUT_KEY);
        if (stored === "throws") {
          throw new DOMException("The operation is insecure.", "SecurityError");
        }
        return stored;
      },
    },
  });
}

/** What the page's own root carries now: the margin's reservation and the sidebar's, each `""`
 *  where the script set nothing and the rule in `styles.css` keeps its default. */
function insets(): { margin: string; sidebar: string } {
  const style = document.documentElement.style;
  return {
    margin: style.getPropertyValue(FIRST_SCREEN_MARGIN_PROPERTY),
    sidebar: style.getPropertyValue(FIRST_SCREEN_SIDEBAR_PROPERTY),
  };
}

afterEach(() => {
  document.documentElement.style.removeProperty(FIRST_SCREEN_MARGIN_PROPERTY);
  document.documentElement.style.removeProperty(FIRST_SCREEN_SIDEBAR_PROPERTY);
  Reflect.deleteProperty(window, "innerWidth");
});

test("a stored width reserves that width and leaves the sidebar's default alone", () => {
  run({ stored: shellLayoutValue(false, 520, false) });
  expect(insets()).toEqual({ margin: "calc(520px + 1.5rem)", sidebar: "" });
});

test("both halves at their rail reserve 5rem on each side", () => {
  run({ stored: shellLayoutValue(true, 520, true) });
  expect(insets()).toEqual({ margin: "5rem", sidebar: "5rem" });
});

test("a width past 60% of the viewport is clamped to it, as the margin's own column is", () => {
  // The clamp is the margin's own rule rather than a copy of its numbers, so a change to the
  // 60 % or to the minimum fails this row instead of leaving the page's copy behind
  // (`clampMarginWidth`, which the script re-implements because it is a classic script).
  run({ stored: "9999/rail", innerWidth: 1280 });
  expect(insets()).toEqual({
    margin: `calc(${clampMarginWidth(9999)}px + 1.5rem)`,
    sidebar: "5rem",
  });
});

test("a width under the margin's minimum is raised to it", () => {
  run({ stored: `12/shown`, innerWidth: 1280 });
  expect(insets()).toEqual({ margin: `calc(${MIN_MARGIN_WIDTH}px + 1.5rem)`, sidebar: "" });
});

test("a narrow viewport clamps to the minimum rather than to 60% of it", () => {
  // 60% of 360 is 216, under the margin's minimum, so the floor is the minimum - which is the
  // rule's own reading of it, not a number written here.
  run({ stored: "600/shown", innerWidth: 360 });
  expect(insets()).toEqual({ margin: `calc(${clampMarginWidth(600)}px + 1.5rem)`, sidebar: "" });
});

test("a negative width reserves nothing", () => {
  run({ stored: "-40/shown" });
  expect(insets()).toEqual({ margin: "", sidebar: "" });
});

test("storage that cannot be read reserves nothing", () => {
  run({ stored: "throws" });
  expect(insets()).toEqual({ margin: "", sidebar: "" });
});

test("nothing stored reserves nothing", () => {
  run({ stored: null });
  expect(insets()).toEqual({ margin: "", sidebar: "" });
});

test("an empty value reserves nothing", () => {
  run({ stored: "" });
  expect(insets()).toEqual({ margin: "", sidebar: "" });
});

test("CSS written after the width reaches no property", () => {
  // The half is read whole or not at all: a value carrying a declaration of its own - from a
  // tab that wrote this key itself, or a reader who typed it into storage - would otherwise be
  // pasted into `style` by the `calc(...)` below it.
  run({ stored: "520px;--first-screen-sidebar-inset:9rem/shown" });
  expect(insets()).toEqual({ margin: "", sidebar: "" });
  run({ stored: "520/shown;--first-screen-sidebar-inset:9rem" });
  expect(insets()).toEqual({ margin: "calc(520px + 1.5rem)", sidebar: "" });
});

test("a value written before this key had two halves reserves only the half it holds", () => {
  run({ stored: "520" });
  expect(insets()).toEqual({ margin: "calc(520px + 1.5rem)", sidebar: "" });
});
