// The owner-walk world on a run's scratch Dispatch, and what a scored run left there: the two modes
// of seed.ts that owner-walk runs, which seed.ts's main dispatches to.
//
//   seed.ts owner-walk <file> <kind> <answer>   (ownerWalkIssues)
//     owner-walk's world under LWEVAL, for an owner of <kind> (root or child), from
//     worlds/owner-walk/ (owner-walk.ts names each file). <answer> is the run's plans-answer.json,
//     the recorded plans answer the proxy serves, and plans-answer.source beside it is its source
//     ({commit, sha256, keyMap}); the answer is held to its source as readRecordedPlans
//     (scored-runs-contracts.ts) holds it, so an answer whose sha256 is not the source's is refused,
//     as is a key map that does not map the answer's project to LWEVAL and each of its issues to a
//     key of LWEVAL, and nothing else. It files, in this order: each recorded issue, closed, with
//     its title from the answer, as the key map names it (the map's LWEVAL keys must be the first
//     ones the run's Dispatch gives, in their number order; another key is refused, naming both);
//     R (root.md), its spec approved by the person, `todo` for a root owner and `in_progress` for a
//     child owner; for a child owner, C (child.md), a child of R, approved, `in_progress`; E1
//     (earlier-issue.md) with its implement record (earlier-implement.json, under the session
//     EARLIER_IMPLEMENTER), then closed; G (issue.md, E1's key filled), a `backlog` child of the
//     owner's issue, with no plan. Each approval request is opened as OWNER_WALK_SEEDER, never the
//     run's session. Writes the fixture (OwnerWalkFixture) to <file>.
//   seed.ts capture-scored <file> <run dir>   (captureScored)
//     the world's Dispatch records after a scored run: each issue of the fixture <file>
//     (OwnerWalkFixture), with its status, every document with every version (its authors and its
//     markdown) and every handoff record with its author and body, as captured.json in <run dir>
//     (Captured), the fixture beside them.
import { readFileSync, writeFileSync } from "node:fs";
import path from "node:path";
import { z } from "zod";
import {
  answerAsk,
  createHandoff,
  createIssue,
  getArtifact,
  getArtifactVersion,
  getIssue,
  patchIssue,
  requestApproval,
  withOpenedAsk,
} from "../../../dispatch/e2e/api";
import { EARLIER_PR, EARLIER_SESSION, OWNER_WALK_DIR, worldText } from "./owner-walk";
import {
  Captured,
  OWNER_WALK_KINDS,
  OWNER_WALK_PROJECT,
  OwnerWalkFixture,
  type RecordedPlan,
  readRecordedPlans,
} from "./scored-runs-contracts";
import { APPROVE, handoffsOf, PERSON } from "./seed-dispatch";

/** The session the seed opens owner-walk's approval requests as: an earlier owner's, never the
 * run's. */
export const OWNER_WALK_SEEDER = { kind: "session", id: "rig-owner-walk-seed" } as const;
/** The session that wrote E1's implement record: the earlier agent of G's first task. */
export const EARLIER_IMPLEMENTER = { kind: "session", id: EARLIER_SESSION } as const;
/** How long the seed waits for Dispatch to settle a spec filed with its issue into its first
 * version, which an approval request needs (a request before it is refused `NO_VERSION`), and how
 * often it looks. */
const SPEC_SETTLE_WAIT_MS = 30_000;
const SPEC_SETTLE_POLL_MS = 250;

/** The person approves the newest version of a spec: an approval request opened as
 * OWNER_WALK_SEEDER once the spec has a settled version, answered Approve as the person. Answers
 * the approved version. */
async function approveSpec(artifact: string): Promise<number> {
  const deadline = Date.now() + SPEC_SETTLE_WAIT_MS;
  while (((await getArtifact(artifact, { as: "agent" })).versions ?? []).length === 0) {
    if (Date.now() > deadline)
      throw new Error(
        `the spec ${artifact} has no settled version after ${SPEC_SETTLE_WAIT_MS} ms, so nobody can approve it`
      );
    await Bun.sleep(SPEC_SETTLE_POLL_MS);
  }
  const requested = withOpenedAsk(
    await requestApproval(artifact, {}, { as: "agent", actor: OWNER_WALK_SEEDER })
  );
  await answerAsk(
    requested.ask.id,
    { selected: [APPROVE], expected_edited_at: requested.ask.edited_at },
    { login: PERSON }
  );
  return requested.version;
}

/** `owner-walk <file> <kind> <answer>`: owner-walk's world (the header says each issue and its
 * order). Every check of the inputs runs before the first write. */
export async function ownerWalkIssues(
  file: string,
  kind: string,
  answerFile: string
): Promise<void> {
  if (!(OWNER_WALK_KINDS as readonly string[]).includes(kind))
    throw new Error(`${kind} is no kind of owner; the kinds are ${OWNER_WALK_KINDS.join(" and ")}`);
  let plans: RecordedPlan[];
  try {
    plans = readRecordedPlans(answerFile, OWNER_WALK_PROJECT).held.plans;
  } catch (error) {
    throw new Error(
      `the seed takes the answer the capture recorded, no other: ${error instanceof Error ? error.message : String(error)}`
    );
  }
  const recordFile = path.join(OWNER_WALK_DIR, "earlier-implement.json");
  const record = z
    .looseObject({ head: z.string().regex(/^[0-9a-f]{40}$/) })
    .parse(JSON.parse(readFileSync(recordFile, "utf8")));
  const planned = worldText("issue.md");
  const texts = {
    root: worldText("root.md"),
    child: worldText("child.md"),
    earlier: worldText("earlier-issue.md"),
  };
  const person = { login: PERSON } as const;

  // First, so that Dispatch gives them the keys the map names, which the owner reads in the answer.
  for (const plan of plans) {
    const created = await createIssue(
      { project: OWNER_WALK_PROJECT, force: true, title: plan.title },
      person
    );
    if (created.key !== plan.key)
      throw new Error(
        `Dispatch gave the recorded issue ${plan.recorded} the key ${created.key}, and the key map names ${plan.key}: the map's keys must be the first keys of ${OWNER_WALK_PROJECT}, in their number order`
      );
    await patchIssue(created.key, { status: "done" }, person);
  }
  const issues: OwnerWalkFixture["issues"] = [];
  /** Files one world issue as the person, with its spec, and the person's approval of the spec
   * where `approve` asks it; its status is the move's below. */
  const fileIssue = async (
    part: OwnerWalkFixture["issues"][number]["part"],
    text: { title: string; spec: string },
    parent: string | null,
    approve: boolean
  ) => {
    const created = await createIssue(
      { project: OWNER_WALK_PROJECT, force: true, title: text.title, spec: text.spec, parent },
      person
    );
    const approvedVersion = approve ? await approveSpec(created.primary_artifact_id) : null;
    const issue = {
      key: created.key,
      part,
      title: text.title,
      parent,
      status: "",
      spec: { artifact: created.primary_artifact_id, approvedVersion },
    };
    issues.push(issue);
    return issue;
  };
  const move = async (issue: { key: string; status: string }, status: string) => {
    await patchIssue(issue.key, { status }, person);
    issue.status = status;
  };
  const root = await fileIssue("root", texts.root, null, true);
  await move(root, kind === "root" ? "todo" : "in_progress");
  let owner = root;
  if (kind === "child") {
    owner = await fileIssue("child", texts.child, root.key, true);
    await move(owner, "in_progress");
  }
  const earlier = await fileIssue("earlier", texts.earlier, null, false);
  // The record goes on the issue while it is open; the issue closes after it.
  const recorded = await createHandoff(
    earlier.key,
    "implement",
    { schemaVersion: 1, phase: "implement", issue: earlier.key, ...record },
    EARLIER_IMPLEMENTER
  );
  await move(earlier, "done");
  const spec = planned.spec.replaceAll("{{EARLIER_ISSUE}}", earlier.key);
  const left = /\{\{[A-Z_]+\}\}/.exec(spec);
  if (left !== null)
    throw new Error(`worlds/owner-walk/issue.md holds ${left[0]}, which the seed does not fill`);
  const g = await fileIssue("planned", { title: planned.title, spec }, owner.key, false);
  await move(g, "backlog");
  const fixture = OwnerWalkFixture.parse({
    scenario: "owner-walk",
    kind,
    project: OWNER_WALK_PROJECT,
    owner: owner.key,
    planned: g.key,
    issues,
    earlier: {
      key: earlier.key,
      record: { id: recorded.id, version: recorded.version },
      session: EARLIER_SESSION,
      pr: EARLIER_PR,
      head: record.head,
    },
    plans,
  });
  writeFileSync(file, `${JSON.stringify(fixture)}\n`);
  console.log(
    `seeded owner-walk for a ${kind} owner: ${issues.map((issue) => `${issue.part} ${issue.key}`).join(", ")}, and ${plans.length} recorded issues`
  );
}

/** `capture-scored <file> <run dir>`: captured.json, the world's Dispatch records after the run. */
export async function captureScored(file: string, runDir: string): Promise<void> {
  const fixture = OwnerWalkFixture.parse(JSON.parse(readFileSync(file, "utf8")));
  const reader = { as: "agent" } as const;
  const issues: Captured["issues"] = [];
  for (const { key } of fixture.issues) {
    const issue = await getIssue(key, reader);
    const artifacts: Captured["issues"][number]["artifacts"] = [];
    for (const artifact of issue.artifacts ?? []) {
      if (artifact.kind !== "doc") continue;
      const read = await getArtifact(artifact.id, reader);
      const versions: Captured["issues"][number]["artifacts"][number]["versions"] = [];
      for (const version of read.versions ?? []) {
        const text = await getArtifactVersion(artifact.id, version.number, reader);
        versions.push({
          version: version.number,
          created_at: version.created_at,
          authors: version.authors ?? [],
          markdown: text.markdown,
        });
      }
      artifacts.push({
        id: artifact.id,
        name: artifact.name,
        slug: artifact.slug,
        primary: artifact.primary,
        versions,
      });
    }
    const handoffs = (await handoffsOf(key)).map((handoff) => ({
      id: handoff.id,
      phase: handoff.phase,
      version: handoff.version,
      author: handoff.author,
      created_at: handoff.created_at,
      body: handoff.body,
    }));
    issues.push({
      key: issue.key,
      title: issue.title,
      status: issue.status,
      parent: issue.parent,
      artifacts,
      handoffs,
    });
  }
  const captured = Captured.parse({ fixture, issues });
  writeFileSync(path.join(runDir, "captured.json"), `${JSON.stringify(captured)}\n`);
  console.log(`captured ${issues.map((issue) => issue.key).join(", ")} into captured.json`);
}
