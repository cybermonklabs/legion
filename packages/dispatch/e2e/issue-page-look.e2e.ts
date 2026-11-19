/** The typeface Dispatch ships, `Dispatch Inter`, and the one page that draws in it.
 *
 *  `web/src/styles.css` declares the face under a family name of its own and names that family
 *  in one rule, `html:has([data-issue-page])`, so the whole document takes it while the new issue
 *  page (`web/src/features/issue-page/IssueOutlinePage.tsx`, whose outermost element carries
 *  `data-issue-page`) is in it, and no other page names it or requests its file. The first row
 *  reads three other pages for the absence, and the second reads the new issue page for the
 *  presence: the face, the file the browser fetched for it, the stacks of the elements a menu can
 *  attach to, the notice the build lists the typeface under, and the way back to a page in the
 *  root's own stack without a reload.
 *
 *  Every family name the rows compare in Node is compared whole and whatever its case; the two
 *  helpers copied from the source, `typefaceAddress` and `typefaceStatuses`, compare the declared
 *  name exactly, in the page. A part of a name never counts: `Inter` is inside `Inter fallback`
 *  and `Dispatch Inter`, and the root's own stack names `Inter`, which is a face the reader's
 *  machine may hold and not one the font set declares. */

import { createIssue, createProject, createProjectDocument } from "./api";
import { documentEditor } from "./editor";
import { COMPACT_VIEWPORT_WIDTH, sidebarInbox } from "./inbox-threads";
import { withIssuePage } from "./issue-page-option";
import { expect, type Locator, type Page, type Response, test } from "./test";

/** The family `web/src/styles.css` declares for the typeface the app ships and the new issue
 *  page draws in. */
const TYPEFACE = "Dispatch Inter";

/** The address the page's stylesheet loads the typeface from, read from its own `@font-face`
 *  rule; null when no rule declares the family. c-typeface. */
function typefaceAddress(page: Page): Promise<string | null> {
  return page.evaluate((family) => {
    const find = (rules: CSSRuleList, base: string): string | null => {
      for (const rule of rules) {
        if (rule instanceof CSSFontFaceRule) {
          const named = rule.style.getPropertyValue("font-family").replace(/["']/g, "").trim();
          const source = /url\(["']?([^"')]+)["']?\)/.exec(rule.style.getPropertyValue("src"));
          if (named === family && source?.[1] !== undefined) return new URL(source[1], base).href;
        } else if (rule instanceof CSSGroupingRule) {
          const found = find(rule.cssRules, base);
          if (found !== null) return found;
        }
      }
      return null;
    };
    for (const sheet of document.styleSheets) {
      const found = find(sheet.cssRules, sheet.href ?? document.baseURI);
      if (found !== null) return found;
    }
    return null;
  }, TYPEFACE);
}

/** How each face of the typeface's family stands in the document's font set: `loaded` once the
 *  file arrived, `error` once its request failed. c-typeface. */
function typefaceStatuses(page: Page): Promise<string[]> {
  return page.evaluate(
    (family) =>
      [...document.fonts]
        .filter((face) => face.family.replace(/["']/g, "") === family)
        .map((face) => face.status),
    TYPEFACE
  );
}

/** A family name without the quotes Chromium writes around a name that holds a space. */
function unquoted(name: string): string {
  return name.trim().replace(/^(["'])(.*)\1$/s, "$2");
}

/** Whether two family names are one family: compared whole, whatever the case. */
function sameFamily(a: string, b: string): boolean {
  return a.toLowerCase() === b.toLowerCase();
}

/** The families a `font-family` stack names, in its order, each unquoted. A quoted name is read
 *  whole, so a comma inside one does not split it. */
function stackFamilies(stack: string): string[] {
  return (stack.match(/"[^"]*"|'[^']*'|[^,\s][^,]*/g) ?? []).map(unquoted);
}

/** The family of every face of the document's font set, one entry per face, unquoted. A face an
 *  `@font-face` rule declares is in the set whether or not any text has asked for it. */
async function fontSetFamilies(page: Page): Promise<string[]> {
  const families = await page.evaluate(() => [...document.fonts].map((face) => face.family));
  return families.map(unquoted);
}

/** The first family of `stack`, in the stack's order, that `held` names. */
function firstHeld(stack: string, held: string[]): string | undefined {
  return stackFamilies(stack).find((name) => held.some((family) => sameFamily(family, name)));
}

/** The computed `font-family` stack one element draws in. */
function stackOf(element: Locator): Promise<string> {
  return element.evaluate((node) => getComputedStyle(node).fontFamily);
}

/** The `--font-sans` custom property as `html` computes it. */
function rootFontSans(page: Page): Promise<string> {
  return page
    .locator("html")
    .evaluate((node) => getComputedStyle(node).getPropertyValue("--font-sans"));
}

type ElementStack = { element: string; stack: string };

/** The computed `font-family` stack of every element of the document, `html` and `body` among
 *  them, in document order, each beside the words a failure names it by: its tag, then its id,
 *  its test id or its first classes, then its place in the document. */
function elementStacks(page: Page): Promise<ElementStack[]> {
  return page.evaluate(() =>
    Array.from(document.querySelectorAll("*"), (element, index) => {
      const testId = element.getAttribute("data-testid");
      let name = Array.from(element.classList.values(), (token) => `.${token}`)
        .slice(0, 3)
        .join("");
      if (testId !== null) name = `[data-testid="${testId}"]`;
      if (element.id !== "") name = `#${element.id}`;
      return {
        element: `<${element.localName}${name}>, element ${index} of the document`,
        stack: getComputedStyle(element).fontFamily,
      };
    })
  );
}

/** The first element of `stacks` whose stack names a family of `held`, in the words a failure
 *  prints, or null when no element's does. */
function firstElementNaming(where: string, stacks: ElementStack[], held: string[]): string | null {
  for (const { element, stack } of stacks) {
    const family = firstHeld(stack, held);
    if (family !== undefined) {
      return `${where}: ${element} draws in \`${stack}\`, whose ${family} is a family of the font set`;
    }
  }
  return null;
}

// The row carries `@phone` because the elements it reads differ by viewport: on a phone the shell
// mounts components of its own, `MarginSheet`'s phone layout among them, by `PHONE_VIEWPORT_QUERY`
// in `web/src/features/shell/useDialog.ts`, so the phone's pages hold elements whose stacks the
// desktop width never draws.
// SLOW(>=2s): three pages loaded one after another, the last a project document whose editor
// opens its live room, each read for every element's stack after its fonts are ready.
test("no page but the new issue page takes the shipped typeface @phone", async ({ page }) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "A page in the root's stack" });
  const notes = await createProjectDocument("CORE", {
    content: "# Design notes\n\nA paragraph in the root's stack.\n",
    name: "Design notes",
  });
  // The Inbox is first, so a selector that reached every page fails on the first page loaded.
  const pages = [
    { drawn: page.locator("main").getByText("Nothing needs you"), name: "the Inbox", path: "/" },
    {
      drawn: page.getByTestId("issue-header").getByRole("heading", { level: 1, name: issue.title }),
      name: `today's page of ${issue.key}`,
      path: `/issues/${issue.key}`,
    },
    {
      drawn: documentEditor(page).getByText("A paragraph in the root's stack."),
      name: "the project document",
      path: `/projects/CORE/documents/${notes.artifact.slug}`,
    },
  ];
  const fontFiles = new Map<string, string[]>(pages.map(({ name }) => [name, []]));
  let loading = pages[0].name;
  page.on("request", (request) => {
    if (new URL(request.url()).pathname.endsWith(".woff2")) {
      fontFiles.get(loading)?.push(request.url());
    }
  });

  for (const { drawn, name, path } of pages) {
    loading = name;
    await page.goto(path);
    await expect(drawn).toBeVisible();
    await page.evaluate(() => document.fonts.ready.then(() => undefined));
    const held = await fontSetFamilies(page);
    expect(firstElementNaming(name, await elementStacks(page), held)).toBeNull();
    const rootSans = await rootFontSans(page);
    expect(
      firstHeld(rootSans, held),
      `${name}: --font-sans on html is \`${rootSans}\``
    ).toBeUndefined();
    // Read after the stacks: a request is what a stack naming the face leads to, so the stacks
    // are the first place a page that took the typeface shows it.
    expect(fontFiles.get(name), `${name}: the font files the page requested`).toEqual([]);
    await expect(page.locator("[data-issue-page]")).toHaveCount(0);
  }
});

// The row carries `@phone` for the first row's reason, and for one of the phone's own: at a phone
// width the link back to the Inbox is the top bar's, which the shell draws below `xl` in place of
// the navigation column the desktop width clicks.
// SLOW(>=2s): the new issue page's own chunk and the typeface's file loaded and read, the build's
// notices fetched, and a navigation back to the Inbox drawn and read again.
test("the new issue page draws the whole document in the shipped typeface @phone", async ({
  context,
  page,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "A page in the shipped typeface" });
  await withIssuePage(context, "alice");
  const fontRequests: string[] = [];
  const fontResponses: Promise<Response | null>[] = [];
  page.on("request", (request) => {
    if (!new URL(request.url()).pathname.endsWith(".woff2")) return;
    fontRequests.push(request.url());
    fontResponses.push(request.response());
  });
  await page.goto(`/issues/${issue.key}`);

  const issuePage = page.locator("[data-issue-page]");
  await expect(issuePage).toBeVisible();
  await expect(issuePage).toContainText(issue.key);
  await expect(issuePage).toContainText(issue.title);

  const families = await fontSetFamilies(page);
  expect(families.filter((family) => sameFamily(family, TYPEFACE))).toHaveLength(1);
  await expect.poll(() => typefaceStatuses(page)).toEqual(["loaded"]);
  expect(families.filter((family) => sameFamily(family, "Inter"))).toEqual([]);

  const address = await typefaceAddress(page);
  expect(address).not.toBeNull();
  expect(fontRequests.length).toBeGreaterThan(0);
  expect([...new Set(fontRequests)]).toEqual([address]);
  for (const answer of fontResponses) {
    const response = await answer;
    expect(response, "the browser's response to the font file").not.toBeNull();
    expect(response?.status()).toBe(200);
    expect(response?.headers()["content-type"]).toBe("font/woff2");
    expect((await response?.body())?.subarray(0, 4).toString("latin1")).toBe("wOF2");
  }

  // `body` is where `createPortal` attaches a menu, outside the page's own element.
  const drawnIn = {
    "--font-sans on html": await rootFontSans(page),
    "[data-issue-page]": await stackOf(issuePage),
    body: await stackOf(page.locator("body")),
    html: await stackOf(page.locator("html")),
  };
  for (const [where, stack] of Object.entries(drawnIn)) {
    expect(stackFamilies(stack)[0]?.toLowerCase(), `${where} draws in \`${stack}\``).toBe(
      TYPEFACE.toLowerCase()
    );
  }

  const notices = await page.request.get("/THIRD_PARTY_NOTICES.txt");
  expect(notices.status()).toBe(200);
  const noticesText = await notices.text();
  expect(noticesText).toMatch(/^Inter@4\.1\nLicense: OFL-1\.1$/m);
  expect(noticesText).toContain("SIL OPEN FONT LICENSE Version 1.1");

  // A marker on the window, which a reload would take away with the document.
  await page.evaluate(() => {
    Object.assign(window, { __noReload: 1 });
  });
  // Below `xl` (`COMPACT_VIEWPORT_QUERY`, `web/src/features/shell/useDialog.ts`) the shell draws
  // its top bar, whose `Dispatch` link goes to the Inbox, and no navigation column.
  const compact = (page.viewportSize()?.width ?? 0) < COMPACT_VIEWPORT_WIDTH;
  const toInbox = compact
    ? page.getByRole("banner").getByRole("link", { exact: true, name: "Dispatch" })
    : sidebarInbox(page);
  await toInbox.click();
  await expect(
    page.locator("main").getByRole("heading", { exact: true, level: 1, name: "Inbox" })
  ).toBeVisible();
  await expect(page.locator("main").getByText("Nothing needs you")).toBeVisible();
  expect(await page.evaluate(() => Reflect.get(window, "__noReload"))).toBe(1);
  const held = await fontSetFamilies(page);
  const htmlStack = await stackOf(page.locator("html"));
  expect(firstHeld(htmlStack, held), `the Inbox's html draws in \`${htmlStack}\``).toBeUndefined();
  await expect(page.locator("[data-issue-page]")).toHaveCount(0);
});
