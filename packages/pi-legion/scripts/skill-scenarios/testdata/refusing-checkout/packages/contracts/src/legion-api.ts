// A checkout whose daemon API contract does not describe the state body this rig's stand-in
// serves, for the row of daemon-standin.test.ts that reads a second checkout's contract.
//
// It is the shape of the 2026-11-09 defect rather than a copy of any release: a contract that
// takes one key and refuses every other, so a reader pointed at it refuses the served body the
// way each base-label run of that night was refused `a body the daemon API contract does not
// describe`. Nothing of this repository imports it; the row is handed its path at run time, as
// it is handed a real checkout's.
import { z } from "zod";

export const LegionStateResponse = z.object({ onlyTheOldKey: z.string() }).strict();
