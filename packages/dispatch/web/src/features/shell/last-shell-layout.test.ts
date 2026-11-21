import { afterEach, expect, test } from "bun:test";

import { LAST_SHELL_LAYOUT_KEY, rememberShellLayout } from "./last-shell-layout";

/** What the shell writes for the next load's first paint to read (`web/index.html`'s head).
 *  The value is read by a script that cannot import this module, so its shapes are held to that
 *  script in `features/margin/first-screen-reservation.test.ts`; what is held here is that the
 *  shell writes them at all, and that a browser refusing storage costs the page nothing. */

afterEach(() => {
  window.localStorage.removeItem(LAST_SHELL_LAYOUT_KEY);
});

test("both columns the shell drew are in one record", () => {
  rememberShellLayout(false, 520, false);
  expect(window.localStorage.getItem(LAST_SHELL_LAYOUT_KEY)).toBe("520/shown");
});

test("a column collapsed to its rail is remembered as a rail, not as a width", () => {
  rememberShellLayout(true, 520, true);
  expect(window.localStorage.getItem(LAST_SHELL_LAYOUT_KEY)).toBe("rail/rail");
});

test("each column is remembered on its own", () => {
  rememberShellLayout(false, 384, true);
  expect(window.localStorage.getItem(LAST_SHELL_LAYOUT_KEY)).toBe("384/rail");
  rememberShellLayout(true, 384, false);
  expect(window.localStorage.getItem(LAST_SHELL_LAYOUT_KEY)).toBe("rail/shown");
});

// A dragged width is `startWidth + startX - event.clientX` (`features/margin/Margin.tsx`), and
// `clientX` is fractional at a non-integer zoom or device pixel ratio. `412.5/shown` fails the
// pattern the head script reads this value by, so the first paint would lay the server's blocks
// out at the default 384 px - the re-wrap this record exists to stop (dispatch://INFRA-341).
test("a fractional width is remembered as a whole number of pixels", () => {
  rememberShellLayout(false, 412.5, false);
  expect(window.localStorage.getItem(LAST_SHELL_LAYOUT_KEY)).toBe("413/shown");
  expect(window.localStorage.getItem(LAST_SHELL_LAYOUT_KEY)).toMatch(/^[0-9]{1,4}\/shown$/);
});

test("storage that refuses the write is no failure of the shell's", () => {
  const storage = window.localStorage;
  Object.defineProperty(window, "localStorage", {
    configurable: true,
    value: {
      ...storage,
      setItem: () => {
        throw new Error("storage is full");
      },
    },
  });
  try {
    expect(() => rememberShellLayout(false, 520, false)).not.toThrow();
  } finally {
    Object.defineProperty(window, "localStorage", { configurable: true, value: storage });
  }
});
