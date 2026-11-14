import { modelsLane, scriptChat } from "./fake-models-helpers";
import { answerPanel, answerText, askAndWait, openBarOver, prose, seedIssue } from "./going-panel";
import { expect, test } from "./test";
import { asUser } from "./users";

// Check c3 of dispatch://INFRA-1588/artifact/verification-md, serving R1: nothing the model
// writes in the prose becomes a link, an image or a tag. The fake writing model writes a markdown
// link to an outside address, an image, a bare address and an img tag with an onerror attribute,
// beside a link to the one issue it was given. The server escapes the prose and drops a word a
// renderer would link; this row reads the panel: the link's and the image's words show as text,
// the prose holds no a and no img element, no handler ran, the escapes are invisible under the
// page's markdown renderer, and the panel's only links are the item frames. A panel that drew
// the text frame as raw HTML would put a link or an image in the prose, which this row catches.

const title = "Quasar sightings log";
const question = "quasar sightings log";

// SLOW(>=2s): a seeded issue, a decision and an answer.
test("the model's links, image, bare address and tag show as text, and the only links are the item frames", async ({
  browser,
}) => {
  modelsLane();
  test.setTimeout(90_000);
  const hit = await seedIssue(question, title);
  await scriptChat({
    default: prose(
      "Read [click here](https://evil.example/x) and ![a picture](https://evil.example/p.png) " +
        'or https://evil.example/bare <img src=x onerror="window.__goingPwned=1"> ' +
        `and then [${title}](${hit.href}).`
    ),
  });
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await openBarOver(page, "/projects/CORE/issues");
    await askAndWait(page, question);

    const text = answerText(page);
    await expect(text).toContainText("click here");
    await expect(text).toContainText("a picture");
    await expect(text.locator("a")).toHaveCount(0);
    await expect(text.locator("img")).toHaveCount(0);
    await expect(answerPanel(page).locator("img")).toHaveCount(0);
    await expect(answerPanel(page).locator("[onerror]")).toHaveCount(0);
    await expect(answerPanel(page).locator('a[href*="evil.example"]')).toHaveCount(0);
    expect(await page.evaluate(() => Reflect.get(window, "__goingPwned"))).toBeUndefined();
    // The server's escapes are read by the renderer, not shown.
    expect(await text.textContent()).not.toContain("\\");

    // Every link in the panel is an item frame, and the one item is the issue the model was given.
    const links = answerPanel(page).locator("a");
    const items = answerPanel(page).getByTestId("going-answer-item");
    await expect(items).toHaveCount(1);
    await expect(links).toHaveCount(1);
    await expect(items).toHaveAttribute("href", hit.href);
  } finally {
    await context.close();
  }
});
