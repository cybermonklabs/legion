import { readFileSync } from "node:fs";

/** The script whose `references)` arm lists the specs the pull-request lane runs. */
const LANE_SCRIPT = new URL(
  "../../../.github/scripts/dispatch-browser-shard-suite.sh",
  import.meta.url
);

/** The files in that arm's `lane_files` assignment, relative to packages/dispatch. */
export function referencesLaneFiles(): string[] {
  const script = readFileSync(LANE_SCRIPT, "utf8")
    .split("\n")
    .map((line) => line.trim())
    .filter((line) => line !== "" && !line.startsWith("#"))
    .join("\n");
  const arm = /^references\)$(.*?)^;;$/msu.exec(script)?.[1] ?? "";
  return (/^lane_files="([^"]*)"/mu.exec(arm)?.[1] ?? "")
    .split(/\s+/u)
    .filter((entry) => entry !== "");
}
