import { describe, expect, test } from "bun:test";
import { existsSync, readFileSync } from "node:fs";
import { join } from "node:path";

// lib/rig.sh's `start_container IDS NAME ARGS...` adds the id its `docker create` printed to the
// array IDS, and `remove_containers IDS` removes each id of IDS; both read IDS by its name
// (`local -n`). A script that calls remove_containers on an array it never declares ends its
// cleanup at that call under `set -u` (an unset name is an unbound variable, which ends an EXIT
// trap), so it removes no container and skips every line of the cleanup after it; one that
// declares the array and never fills it removes nothing. So each tracked shell script that calls
// `remove_containers ARRAY` declares `ARRAY=()` and fills it through `start_container ARRAY`. What
// this does not read: a script that sources another for either line, which no script does.

const root = join(import.meta.dir, "..", "..", "..");

/** A call of remove_containers, at the start of a command: the array it names is group 1. */
const removal = /(?:^\s*|[;&|({]\s*|\b(?:then|do|else)\s+)remove_containers\s+([A-Za-z_]\w*)/;

/** Each call of `remove_containers ARRAY` in `text`, the script `name`, whose ARRAY the script
 *  does not declare empty (`ARRAY=()`) or never fills through `start_container ARRAY`, one line
 *  each, naming its line and what the script lacks. A comment is no call. */
function unfilledArrays(name: string, text: string): string[] {
  const problems: string[] = [];
  text.split("\n").forEach((line, index) => {
    if (line.trimStart().startsWith("#")) return;
    const array = removal.exec(line)?.[1];
    if (array === undefined) return;
    const lacks: string[] = [];
    if (!new RegExp(`^\\s*(?:local\\s+-a\\s+)?${array}=\\(\\)`, "m").test(text)) {
      lacks.push(`no declaration ${array}=()`);
    }
    if (!new RegExp(`(?:^\\s*|[;&|({]\\s*)start_container\\s+${array}\\s`, "m").test(text)) {
      lacks.push(`no start_container ${array}`);
    }
    if (lacks.length > 0) {
      problems.push(
        `${name}:${index + 1} calls remove_containers ${array}, and the script has ${lacks.join(" and ")}`
      );
    }
  });
  return problems;
}

/** The tracked shell scripts of the checkout, read by `git ls-files`. */
const listed = Bun.spawnSync(["git", "ls-files", "-z", "--", "*.sh"], {
  cwd: root,
  stderr: "pipe",
});
if (listed.exitCode !== 0) {
  throw new Error(`git ls-files exited ${listed.exitCode}: ${listed.stderr.toString()}`);
}
const scripts = listed.stdout
  .toString()
  .split("\0")
  .filter((name) => name !== "" && existsSync(join(root, name)));

/** The scripts known to call remove_containers, a floor under the scan: a scan that finds none of
 *  them read nothing. */
const knownCallers = [
  "scripts/e2e/controller-start-tmux.sh",
  "scripts/e2e/parked-decision-devbox.sh",
  "scripts/e2e/stage3-devbox-workflow.sh",
];

describe("the arrays remove_containers removes", () => {
  test("each tracked script that calls remove_containers ARRAY declares ARRAY and fills it through start_container", () => {
    const texts = new Map(scripts.map((name) => [name, readFileSync(join(root, name), "utf8")]));
    const callers = [...texts].filter(([, text]) =>
      text.split("\n").some((line) => !line.trimStart().startsWith("#") && removal.test(line))
    );
    expect(callers.map(([name]) => name)).toEqual(expect.arrayContaining(knownCallers));
    expect(callers.flatMap(([name, text]) => unfilledArrays(name, text))).toEqual([]);
  });

  // The controls: planted scripts of each shape, which the search must name, beside those it must
  // pass. The first is claude-backend-sandbox.sh's cleanup as one merge left it: its containers
  // made by `docker run -d --name`, and a remove_containers of an array it never had.
  test("a call of remove_containers on an array the script never declares or never fills is named", () => {
    const planted: Record<string, string> = {
      "never-declared.sh": `docker run -d --name "$pg" image\ncleanup() {\n  remove_containers containers\n}\n`,
      "never-filled.sh": `containers=()\ncleanup() { remove_containers containers; }\n`,
      "undeclared.sh": `start_container containers "$pg" image\ncleanup() { stop || remove_containers containers; }\n`,
      "filled.sh": `containers=()\nstart_container containers "$pg" image\ncleanup() { remove_containers containers; }\n`,
      "in-a-comment.sh": `# remove_containers containers is lib/rig.sh's\n`,
    };
    expect(Object.entries(planted).flatMap(([name, text]) => unfilledArrays(name, text))).toEqual([
      "never-declared.sh:3 calls remove_containers containers, and the script has no declaration containers=() and no start_container containers",
      "never-filled.sh:2 calls remove_containers containers, and the script has no start_container containers",
      "undeclared.sh:2 calls remove_containers containers, and the script has no declaration containers=()",
    ]);
  });
});
