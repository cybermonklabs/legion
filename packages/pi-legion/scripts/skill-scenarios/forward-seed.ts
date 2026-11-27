// A forward world on a run's scratch Dispatch, and what its run left there: the three modes of
// seed.ts that forward-only and forward-owner runs take, which seed.ts's main dispatches to. The
// world is a directory of worlds/<scenario>/ (forward-world.ts names each file).
//
//   seed.ts forward <file> <world dir>            (forwardIssues)
//     The world's project, with world.json's repository as its default repository, then its
//     issue (world.json's `issue`, which must be the first key the project gives), filed by the
//     person with spec.md as its spec, which the person approves, verification.md as its
//     Verification document, `in_progress` with the label `legion`; then each child of
//     world.json, a child of the issue in its status; then each comment of world.json, the
//     person's. Writes the fixture (ForwardFixture) to <file>. The label's TestRigDaemonLive,
//     which holds only the agent bearer, records the passes at the head it builds: the seed
//     records none, and makes every write that only a person may make.
//   seed.ts results-hold <file> <head>             (resultsHold)
//     Each before_merge check of the issue that holds no pass at <head> in the run's Dispatch,
//     one a line, and exit 1 when there is one: the run then stops (rig.sh run_forward).
//   seed.ts capture-forward <file> <run dir>       (captureForward)
//     Every version of the issue's spec with its markdown, and every approval request of it, as
//     forward-captured.json in <run dir> (ForwardCaptured).
import { readFileSync, writeFileSync } from "node:fs";
import path from "node:path";
import {
  createComment,
  createIssue,
  createIssueArtifact,
  createProject,
  getArtifact,
  getArtifactVersion,
  getIssueResults,
  listIssueAsks,
  patchIssue,
  putProjectDefaultRepository,
} from "../../../dispatch/e2e/api";
import { ForwardCaptured, ForwardFixture, readForwardWorld } from "./forward-world";
import { approveSpec } from "./owner-walk-seed";
import { PERSON } from "./seed-dispatch";

/** `forward <file> <world dir>`: the world's issues and comments (the header says each). */
export async function forwardIssues(file: string, worldDir: string): Promise<void> {
  const world = readForwardWorld(worldDir);
  const project = world.issue.replace(/-\d+$/, "");
  const person = { login: PERSON } as const;
  await createProject({ key: project, name: `the forward rig's ${world.world}` });
  await putProjectDefaultRepository(project, world.repository, person);
  const created = await createIssue(
    {
      project,
      force: true,
      title: world.title,
      spec: readFileSync(path.join(worldDir, "spec.md"), "utf8"),
    },
    person
  );
  if (created.key !== world.issue)
    throw new Error(
      `Dispatch gave the world's issue the key ${created.key}, and world.json names ${world.issue}`
    );
  const seeded = await approveSpec(created.primary_artifact_id);
  await createIssueArtifact(
    created.key,
    {
      name: "verification.md",
      content: readFileSync(path.join(worldDir, "verification.md"), "utf8"),
    },
    person
  );
  await patchIssue(created.key, { status: "in_progress", labels: ["legion"] }, person);
  const children: string[] = [];
  for (const child of world.children ?? []) {
    const filed = await createIssue(
      { project, force: true, title: child.title, parent: created.key },
      person
    );
    if (filed.key !== child.key)
      throw new Error(
        `Dispatch gave the child ${child.title} the key ${filed.key}, and world.json names ${child.key}`
      );
    await patchIssue(filed.key, { status: child.status }, person);
    children.push(filed.key);
  }
  for (const comment of world.comments ?? [])
    await createComment(comment.issue, { body: comment.body }, { as: "user", login: PERSON });
  const fixture = ForwardFixture.parse({
    issue: created.key,
    spec: { artifact: created.primary_artifact_id, seeded },
    children,
  });
  writeFileSync(file, `${JSON.stringify(fixture)}\n`);
  console.log(`seeded ${world.scenario}/${world.world}: ${[created.key, ...children].join(", ")}`);
}

/** `results-hold <file> <head>`: each before_merge check of the issue with no pass at `head`. */
export async function resultsHold(file: string, head: string): Promise<boolean> {
  const { issue } = ForwardFixture.parse(JSON.parse(readFileSync(file, "utf8")));
  const read = await getIssueResults(issue, { as: "agent" });
  // A read that names no checks enumerated nothing: it is no evidence that every check passed.
  if (read.checks === null || read.checks === undefined || read.checks.length === 0) {
    console.log(
      `the results of ${issue} name no check, so no before_merge check can be read as passed at ${head}`
    );
    return false;
  }
  const missing = read.checks
    .filter((check) => check.when === "before_merge")
    .filter(
      (check) =>
        !(check.results ?? []).some(
          (result) =>
            result.verdict === "pass" &&
            result.commit_sha === head &&
            !("retracted" in result && result.retracted)
        )
    );
  for (const check of missing)
    console.log(`the before_merge check ${check.block_id} of ${issue} has no pass at ${head}`);
  return missing.length === 0;
}

/** `capture-forward <file> <run dir>`: forward-captured.json, the issue's spec after the run. */
export async function captureForward(file: string, runDir: string): Promise<void> {
  const fixture = ForwardFixture.parse(JSON.parse(readFileSync(file, "utf8")));
  const reader = { as: "agent" } as const;
  const versions: ForwardCaptured["spec"]["versions"] = [];
  for (const version of (await getArtifact(fixture.spec.artifact, reader)).versions ?? []) {
    const text = await getArtifactVersion(fixture.spec.artifact, version.number, reader);
    versions.push({ version: version.number, markdown: text.markdown });
  }
  const approvals = (await listIssueAsks(fixture.issue, reader)).flatMap((ask) =>
    ask.kind === "approval" && ask.approval?.artifact_id === fixture.spec.artifact
      ? [{ id: ask.id, version: ask.approval.version, state: ask.state }]
      : []
  );
  const captured = ForwardCaptured.parse({
    issue: fixture.issue,
    spec: { ...fixture.spec, versions },
    approvals,
  });
  writeFileSync(path.join(runDir, "forward-captured.json"), `${JSON.stringify(captured)}\n`);
  console.log(
    `captured ${fixture.issue}: ${versions.length} spec versions, ${approvals.length} approval requests`
  );
}
