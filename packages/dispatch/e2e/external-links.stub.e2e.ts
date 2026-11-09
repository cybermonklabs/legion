/** An unsafe stored external link, drawn inert. The row reads only what the page makes of one
 *  stored body, so it runs against a recording rather than against a harness
 *  (packages/dispatch/AGENTS.md, "The stubbed layer"). */
import { expect } from "@playwright/test";

import { createIssue, createProject } from "./api";
import { insertExternalLink } from "./seed";
import { stubbed } from "./stub/stub-test";

const unsafeUrl = "javascript:alert(document.domain)";

const test = stubbed(async () => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Unsafe link" });
  await insertExternalLink(issue.key, unsafeUrl);
  return { issue: issue.key };
});

test("an unsafe stored external link is rendered as inert text @pr", async ({ page, scene }) => {
  await page.goto(`/issues/${scene.issue}`);

  const unsafeLink = page.getByText(unsafeUrl);
  await expect(unsafeLink).toHaveAttribute("title", "Unsafe external link");
  await expect(unsafeLink).not.toHaveAttribute("href");
});
