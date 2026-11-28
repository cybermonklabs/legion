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

/** A heads world's named CI job: its check run, the workflow run and suite it is in, its status. */
const CIJob = z.strictObject({
  name: z.string().min(1),
  checkRun: z.number().int(),
  run: z.number().int(),
  suite: z.number().int(),
  status: z.enum(["completed", "in_progress"]),
});

const World = z.strictObject({
  repository: z.string().regex(/^[A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+$/),
  defaultBranch: z.string().min(1),
  ciJobs: z.strictObject({ "ci-only": CIJob, "ci-pass-and-fail": CIJob }),
});

/** rig-world.json, beside this file. */
export const RIG_WORLD_FILE = path.join(import.meta.dir, "rig-world.json");

/** The world's repository (`owner/name`), its default branch and its named CI jobs. */
export const RIG_WORLD = World.parse(JSON.parse(readFileSync(RIG_WORLD_FILE, "utf8")));

/** The owner of the world's repository. */
export const RIG_OWNER = RIG_WORLD.repository.split("/")[0] ?? "";
