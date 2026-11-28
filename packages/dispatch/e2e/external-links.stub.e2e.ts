/** An unsafe stored external link, drawn inert. The row reads only what the page makes of one
 *  stored body, so it runs against a recording rather than against a harness
 *  (packages/dispatch/README.md, "The stubbed layer"). */
import { expect } from "@playwright/test";

import { createIssue, createProject } from "./api";
import { openIssueDetails } from "./issue-details";
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
  // The issue's links are a row of the header's Details.
  await openIssueDetails(page);

  const unsafeLink = page.getByText(unsafeUrl);
  // Text no click opens; its title holds the whole address and why it is no link
  // (dispatch://INFRA-5200, c-review F4).
  await expect(unsafeLink).toHaveAttribute(
    "title",
    `Unsafe external link, not opened: ${unsafeUrl}`
  );
  await expect(unsafeLink).not.toHaveAttribute("href");
});
