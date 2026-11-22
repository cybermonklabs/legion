// The files a copy of the rig carries, for a test that stages one of its own: rig.sh, the library
// it sources from its own directory at its start (red-at-the-head.sh), and the jq modules that
// library loads (jq/). A module the rig comes to load at its start is added here once, and every
// staged copy carries it: rig.test.ts's row "a staged rig carries every file rig.sh and its
// library load" holds this list to what the two scripts source and load.
import { cpSync } from "node:fs";
import path from "node:path";

export const RIG_FILES = ["rig.sh", "red-at-the-head.sh", "jq"] as const;

/** Copies the rig's files into `scripts`, the copy's `packages/pi-legion/scripts/skill-scenarios`. */
export function stageRig(scripts: string): void {
  for (const file of RIG_FILES) {
    cpSync(path.join(import.meta.dir, file), path.join(scripts, file), { recursive: true });
  }
}
