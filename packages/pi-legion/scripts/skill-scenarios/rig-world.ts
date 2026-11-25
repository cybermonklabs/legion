/**
 * The repository and default branch of every phase worker's world, and the named CI job of each
 * heads world that has one, from rig-world.json, the one place they are written: the pane's
 * addressing (daemon-pane.go, through `workerPane`), the stand-in daemon's state, Dispatch's GitHub
 * stand-in (heads-world.ts) and rig.sh's git fixture and gh routes all read them, so the text an
 * agent gets, the world Dispatch reads and the world the agent reads name one repository and one
 * check run of each named job.
 */
import { readFileSync } from "node:fs";
import path from "node:path";
import { z } from "zod";

/** A heads world's named CI job: its check run, the workflow run and suite it is in, its status,
 * and for one that ended, how it concluded (`success` where none is named) and the log GitHub
 * keeps of it. A job a world lets its agent run once more names `rerun`: the check run of that
 * second attempt and how it concludes. */
const CIJob = z.strictObject({
  name: z.string().min(1),
  checkRun: z.number().int(),
  run: z.number().int(),
  suite: z.number().int(),
  status: z.enum(["completed", "in_progress"]),
  conclusion: z.enum(["success", "failure"]).optional(),
  log: z.string().optional(),
  rerun: z
    .strictObject({ checkRun: z.number().int(), conclusion: z.enum(["success", "failure"]) })
    .optional(),
});
/** One named CI job of a heads world, as rig-world.json writes it. */
export type RigCIJob = z.infer<typeof CIJob>;

/** The worlds' named jobs: ci-only's and ci-pass-and-fail's, the job of k2, the check a CI job
 * runs, in the ci-check-fail worlds: failed at the head whose case says so, passed at the other,
 * and the job of k-ci in each world of fix-judge, failed at #702's head: a test of a package the
 * change does not touch, which passes on its second attempt; a test of the package the change
 * touches, which fails again; and the lint job beside which a tester works. */
const World = z.strictObject({
  repository: z.string().regex(/^[A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+$/),
  defaultBranch: z.string().min(1),
  ciJobs: z.strictObject({
    "ci-only": CIJob,
    "ci-pass-and-fail": CIJob,
    "ci-check-fail": CIJob,
    "ci-check-pass": CIJob,
    "fix-judge-unrelated-test": CIJob,
    "fix-judge-touched-test": CIJob,
    "fix-judge-tester-beside": CIJob,
  }),
});

/** rig-world.json, beside this file. */
export const RIG_WORLD_FILE = path.join(import.meta.dir, "rig-world.json");

/** The world's repository (`owner/name`), its default branch and its named CI jobs. */
export const RIG_WORLD = World.parse(JSON.parse(readFileSync(RIG_WORLD_FILE, "utf8")));

/** The owner of the world's repository. */
export const RIG_OWNER = RIG_WORLD.repository.split("/")[0] ?? "";
