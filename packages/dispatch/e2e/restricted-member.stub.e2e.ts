/** A restricted member's rail and the pages it leaves out, read over a recording of what
 *  Dispatch answers that member (packages/dispatch/AGENTS.md, "The stubbed layer").
 *
 *  The recording is bob's: `/auth/whoami` carries `member_kind: "restricted"`, and the project
 *  list is the one PRIV, whose access list names him. What the row asserts is what the page does
 *  with those answers - the rows the rail draws, the pages the router answers as not found, and
 *  the priorities it never reads - which is a rule the page applies itself. That the server lists
 *  bob no project his access does not reach is the real-stack row's claim, in
 *  `restricted-member.e2e.ts`, which runs on the full lane.
 */
import { expect, type Page } from "@playwright/test";

import { baseUrl, createProject, userHeaders } from "./api";
import { sql } from "./psql";
import { stubbed } from "./stub/stub-test";

/** The restricted member: a person from outside the sign-in domain whom PRIV's access list
 *  names. */
const BOB = "bob@outside.example";

const test = stubbed(async () => {
  // The recording signs bob in before this runs, which records him as a full member; outside
  // sign-in records such a person as restricted, and that sign-in is not what this row is about.
  await sql(`update people set member_kind = 'restricted' where email = '${BOB}'`);
  await createProject({ key: "PRIV", name: "Private work" });
  const named = await fetch(
    new URL(`/api/v1/projects/PRIV/access/people/${encodeURIComponent(BOB)}`, baseUrl),
    { headers: await userHeaders("alice"), method: "PUT" }
  );
  if (named.status !== 200) {
    throw new Error(`naming ${BOB} on PRIV answered ${named.status}: ${await named.text()}`);
  }
  return { project: "PRIV" };
}, BOB);

/** Where each link of the rail goes, in the order the rail draws them. */
function railTargets(page: Page): Promise<(string | null)[]> {
  return page
    .getByRole("navigation", { name: "Navigation" })
    .getByRole("link")
    .evaluateAll((links) => links.map((link) => link.getAttribute("href")));
}

test("a restricted member's rail draws Inbox, Agents, their projects and Settings, and the views over every project answer Page not found @pr", async ({
  page,
  scene,
}) => {
  let priorityReads = 0;
  page.on("request", (request) => {
    if (new URL(request.url()).pathname.startsWith("/api/v1/priorities")) priorityReads += 1;
  });

  // A page other than the Inbox, where a full member is offered the Peek.
  await page.goto(`/projects/${scene.project}`);
  await expect
    .poll(() => railTargets(page))
    .toEqual(["/", "/agents", `/projects/${scene.project}`, "/settings"]);
  await expect(
    page.getByRole("button", { name: "Peek at the inbox without leaving this page" })
  ).toHaveCount(0);

  for (const path of ["/priorities", "/priorities/launch", "/delivery", "/pipeline"]) {
    await page.goto(path);
    await expect(page.getByRole("heading", { name: "Page not found" })).toBeVisible();
    await expect(page).toHaveTitle("Not found · Dispatch");
  }
  expect(priorityReads).toBe(0);
});
