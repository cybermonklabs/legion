/** A restricted member's rail, the pages it leaves out, and their Inbox, read over a recording of
 *  what Dispatch answers that member (packages/dispatch/AGENTS.md, "The stubbed layer").
 *
 *  The recording is bob's: `/auth/whoami` carries `member_kind: "restricted"`, and the project
 *  list is the one PRIV, whose access list names him. What the rows assert is what the page does
 *  with those answers - the rows the rail draws, the pages the router answers as not found, the
 *  deployment's rows it never reads, and the Inbox that says so when nothing needs him - which is
 *  a rule the page applies itself. That the server lists bob no project his access does not reach,
 *  and that no page of his draws a refusal from it, is the real-stack rows' claim, in
 *  `restricted-member.e2e.ts`, which runs on the full lane.
 */
import { expect } from "@playwright/test";

import { baseUrl, createProject, userHeaders } from "./api";
import { sql } from "./psql";
import { railTargets } from "./rail";
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

test("a restricted member's rail draws Inbox, Agents, Delivery, their projects and Settings, and Priorities and Pipeline answer Page not found @pr", async ({
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
    .toEqual(["/", "/agents", "/delivery", `/projects/${scene.project}`, "/settings"]);
  await expect(
    page.getByRole("button", { name: "Peek at the inbox without leaving this page" })
  ).toHaveCount(0);

  for (const path of ["/priorities", "/priorities/launch", "/pipeline"]) {
    await page.goto(path);
    await expect(page.getByRole("heading", { name: "Page not found" })).toBeVisible();
    await expect(page).toHaveTitle("Not found · Dispatch");
  }
  expect(priorityReads).toBe(0);
});

// The credential list is the deployment's, which Dispatch refuses a restricted member (403
// DEPLOYMENT_READ_REFUSED): bob's Inbox never asks for it, and with nothing waiting on him it
// says so, rather than that a list failed to load.
test("a restricted member's empty Inbox says Nothing needs you and asks for no credential list @pr", async ({
  page,
  scene,
}) => {
  const credentialReads: string[] = [];
  page.on("request", (request) => {
    const path = new URL(request.url()).pathname;
    if (path.startsWith("/api/v1/credential-requests")) credentialReads.push(path);
  });

  await page.goto("/");
  // The rail is bob's: it lists his one project.
  await expect.poll(() => railTargets(page)).toContain(`/projects/${scene.project}`);
  await expect(page.getByText("Nothing needs you")).toBeVisible();
  await expect(page.getByText("Couldn't load credential requests.")).toHaveCount(0);
  expect(credentialReads).toEqual([]);
});
