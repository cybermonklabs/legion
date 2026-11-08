import { expect, type Locator, test } from "@playwright/test";

import * as C from "../web/src/theme/classes";
import { oklchToSrgb8 } from "../web/src/theme/contrast";
import type { Swatch } from "../web/src/theme/palette";
import { createAsk, createComment, createIssue, createProject } from "./api";
import { conversationThreadScope } from "./editor";
import { openIssueAddress } from "./issue-presets";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

const session = {
  actor: { kind: "session" as const, id: "e2e-theme" },
  as: "agent" as const,
};

// Chromium serializes `getComputedStyle` colors in the color space they were declared in —
// Tailwind v4's default palette is `oklch()`, so `backgroundColor`/`color`/`borderBottomColor`
// come back as e.g. `oklch(0.208 0.042 265.755)`, not `rgb(...)`. Rather than pin that literal
// string (brittle against browser/Tailwind version formatting changes) or fall back to a
// lightness-threshold check (accepts any two colors on the right side of the light/dark line,
// not the *specific* token the component is supposed to render), this converts the actual
// computed color through the same OKLCH→sRGB math `palette.test.ts` uses to verify contrast,
// then compares it against the exact expected `classes.ts` `ColorPair` swatch for the
// element's role and color scheme — an 8-bit RGB triple, exact.
const OKLCH_PATTERN = /^oklch\(([\d.]+)\s+([\d.]+)\s+([\d.]+)/;
const RGB_PATTERN = /^rgba?\((\d+),\s*(\d+),\s*(\d+)/;

function toRgbString([r, g, b]: readonly [number, number, number]): string {
  return `rgb(${r}, ${g}, ${b})`;
}

/** Normalizes a computed color string (`oklch(...)` or `rgb(...)`/`rgba(...)`) to a canonical
 * `rgb(r, g, b)` 8-bit triple, dropping alpha — none of this suite's assertions target a
 * translucent element. */
function normalizeComputedColor(value: string): string {
  const oklchMatch = OKLCH_PATTERN.exec(value);
  if (oklchMatch !== null) {
    const [, l, c, h] = oklchMatch;
    return toRgbString(oklchToSrgb8({ c: Number(c), h: Number(h), l: Number(l) }));
  }
  const rgbMatch = RGB_PATTERN.exec(value);
  if (rgbMatch !== null) {
    const [, r, g, b] = rgbMatch;
    return toRgbString([Number(r), Number(g), Number(b)]);
  }
  throw new Error(`could not parse a css color from "${value}"`);
}

/** The exact 8-bit RGB `classes.ts` expects a `Swatch` to render as. */
function expectedRgb(swatch: Swatch): string {
  return toRgbString(oklchToSrgb8(swatch.oklch));
}

async function expectBackgroundColor(locator: Locator, swatch: Swatch): Promise<void> {
  const actual = await locator.first().evaluate((el) => getComputedStyle(el).backgroundColor);
  expect(normalizeComputedColor(actual)).toBe(expectedRgb(swatch));
}

/** Polls, since a link or button eases its `color` over 150ms (styles.css) after a scheme switch. */
async function expectTextColor(locator: Locator, swatch: Swatch): Promise<void> {
  await expect
    .poll(async () =>
      normalizeComputedColor(await locator.first().evaluate((el) => getComputedStyle(el).color))
    )
    .toBe(expectedRgb(swatch));
}

async function expectBorderColor(locator: Locator, swatch: Swatch): Promise<void> {
  const actual = await locator.first().evaluate((el) => getComputedStyle(el).borderBottomColor);
  expect(normalizeComputedColor(actual)).toBe(expectedRgb(swatch));
}

// A row of this suite attaches no screenshot of its own. A shot nothing compares is a file an
// upload carries and nobody reads, and the shots that are compared are the visual review's
// (e2e/visual.e2e.ts), against committed baselines.

test.beforeEach(async () => {
  await resetDatabase();
});

// THE ONE COMPUTED-COLOUR ROW OF THIS SUITE, and why one is kept while a whole visual layer
// exists. That layer compares 18 pages against committed baselines in both schemes
// (e2e/visual.e2e.ts), which is a wider claim about appearance than
// this row makes and a different one: a baseline is generated from the code under test, so a
// changed palette becomes the new baseline and passes. It detects a change against a file
// somebody committed; it cannot bind a rendered element to the swatch `web/src/theme/classes.ts`
// names for that element's role, because it holds pixels and not that mapping. This row is that
// binding - the registered tokens, in both schemes, read off the live elements - and it is
// untagged, so `chromium` runs it on every pull request, where the visual review, which is the
// full lane's alone (.github/workflows/dispatch-browser.yaml), does not run at all.
// SLOW(>=2s): a database reset, five API writes, a sign-in, three pages read in both schemes,
// and two states no baseline holds - an expanded thread and the phone margin sheet - opened and
// read in both schemes too.
test("the Inbox, an issue's spec and its Conversation render each palette token in light and in dark", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({
    project: "CORE",
    spec: "Decide the rollout plan.",
    title: "Roll out the new pipeline",
  });
  const ask = await createAsk(
    issue.key,
    { anchor: { artifact: "spec", quote: "Decide" }, question: "Ship this week?" },
    session
  );
  const comment = await createComment(
    issue.key,
    { body: "See [the plan](https://example.com/plan)." },
    session
  );
  // Anchored on the spec, so the margin has a card and the review panel something to show: the
  // sheet renders empty-handed otherwise, and an empty sheet is not the surface under test.
  await createComment(
    issue.key,
    { anchor: { artifact: "spec", quote: "Decide" }, body: "left a note" },
    session
  );
  const alice = await asUser(browser, "alice");

  try {
    const page = await alice.newPage();
    const shell = page.getByTestId("app-shell");
    const card = page.getByTestId(`ask-${ask.id}`);

    await page.emulateMedia({ colorScheme: "light" });
    await page.goto("/");
    await expect(page.getByRole("heading", { name: "Inbox" })).toBeVisible();
    await expect(card).toBeVisible();
    await expectBackgroundColor(shell, C.CANVAS.light);
    await expectBackgroundColor(card, C.SURFACE.light);

    await page.emulateMedia({ colorScheme: "dark" });
    await expectBackgroundColor(shell, C.CANVAS.dark);
    await expectBackgroundColor(card, C.SURFACE.dark);

    // The issue header by its test id, not `page.locator("header").first()`: the shell's compact
    // top bar is a `header` too and its border is `railBorder`, slate-800 in both schemes, so a
    // lookup that could reach it would read a border that says nothing about the scheme.
    const header = page.getByTestId("issue-header");
    const title = header.getByRole("heading", { level: 1, name: issue.title });

    await page.emulateMedia({ colorScheme: "light" });
    await page.goto(`/issues/${issue.key}/spec`);
    await expect(title).toBeVisible();
    await expectTextColor(title, C.TEXT_PRIMARY.light);
    await expectBorderColor(header, C.BORDER_DEFAULT.light);

    await page.emulateMedia({ colorScheme: "dark" });
    await expectTextColor(title, C.TEXT_PRIMARY.dark);
    await expectBorderColor(header, C.BORDER_DEFAULT.dark);

    // Collapsed, the turn shows the comment as a two-line `MarkdownPreview` with live links,
    // whose `<a>` Typography colours through `--tw-prose-links`.
    const turn = page.locator(`[data-turn="comment:${comment.id}"]`);
    const link = turn.locator(".dispatch-markdown").getByRole("link", { name: "the plan" });

    // The composer docked at the foot of the Conversation route sits on `SURFACE`, the token
    // that separates a writing surface from the canvas behind it. Read here rather than left to
    // a baseline because this page has no committed shot to carry it: a turn arriving over the
    // live socket makes it sometimes 20 pixels shorter, so its screenshots were taken out of the
    // visual set (e2e/review-routes.ts).
    const dock = page.getByRole("form", { name: "Comment composer" });

    await page.emulateMedia({ colorScheme: "light" });
    await openIssueAddress(page, "conversation");
    await expect(link).toBeVisible();
    await expectTextColor(link, C.LINK.light);
    await expect(dock).toBeVisible();
    await expectBackgroundColor(dock, C.SURFACE.light);

    await page.emulateMedia({ colorScheme: "dark" });
    await expectTextColor(link, C.LINK.dark);
    await expectBackgroundColor(dock, C.SURFACE.dark);

    // Expanded, the turn swaps that preview for the full `MarkdownBody` - in place above the
    // phone breakpoint, in a full-screen Thread dialog below it, which is what the scope helper
    // resolves. A second `.dispatch-markdown` body, and a second `--tw-prose-links` binding: no
    // baseline reaches it, since every committed shot holds the thread collapsed.
    await turn.getByRole("button", { name: "Expand thread" }).click();
    const thread = conversationThreadScope(page, test.info().project.name, turn);
    const bodyLink = thread.locator(".dispatch-markdown").getByRole("link", { name: "the plan" });
    await expect(bodyLink).toBeVisible();
    await expectTextColor(bodyLink, C.LINK.dark);

    await page.emulateMedia({ colorScheme: "light" });
    await expectTextColor(bodyLink, C.LINK.light);

    // On a phone viewport the review margin is a bottom sheet the reader opens, and its surface
    // is `SURFACE` - the same token the Inbox card above reads, on a different component. No
    // baseline reaches it either: the phone shots are taken with the sheet closed.
    await page.setViewportSize({ height: 844, width: 390 });
    await page.goto(`/issues/${issue.key}/spec`);
    await page.getByRole("button", { name: "Open review panel" }).click();
    const sheet = page.getByTestId("margin-sheet");
    await expect(sheet).toBeVisible();
    await expectBackgroundColor(sheet, C.SURFACE.light);

    await page.emulateMedia({ colorScheme: "dark" });
    await expectBackgroundColor(sheet, C.SURFACE.dark);
  } finally {
    await alice.close();
  }
});
