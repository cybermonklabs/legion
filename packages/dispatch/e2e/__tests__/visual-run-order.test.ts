import { expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";

import { projectsOnCommandLine } from "../configurations";
import { rowLimit } from "./bounded-subprocess";
import { listing, retained } from "./playwright-listing";

// One seed serves the whole visual run, and no row of it reseeds (e2e/visual.e2e.ts, "ONE SEED
// FOR THE WHOLE FILE"). The command bar's rows (e2e/going-bar.visual.e2e.ts, the `visual-bar`
// project) write into that seed the states they compare - a run, a plan applied and undone, a
// filed issue - and the guard that stops a page row's writes is not on their pages
// (e2e/visual-guard.ts would stop the writes that are the state under review). What keeps those
// writes out of every page row's shot is order alone: the run has one worker, which takes the
// projects in the order e2e/playwright.config.ts declares them, and `visual-bar` is declared last,
// so every page row draws before the first bar row writes. A second worker would run a bar row
// beside a page row, and a visual project declared after `visual-bar` would draw after its writes.
// This file fails on either, reading the config as Playwright resolves it for the visual run's own
// command line.

/** The arguments e2e/visual-run.sh hands Playwright's `test` command inside the container,
 *  `--config` and its value left out, since the listing names the config itself. The person's own
 *  arguments (`"$@"`) are not among them: CI's visual step passes none
 *  (.github/scripts/dispatch-browser-visual-suite.sh). */
function visualRunArguments(): string[] {
  const script = readFileSync(fileURLToPath(new URL("../visual-run.sh", import.meta.url)), "utf8");
  const command = /cli\.js test ([\s\S]*?)"\$@"/u.exec(script)?.[1];
  if (command === undefined) {
    throw new Error(
      'e2e/visual-run.sh holds no `cli.js test ... "$@"` command, which is the visual run this ' +
        "file reads"
    );
  }
  const words = command.replaceAll("\\\n", " ").split(/\s+/u).filter(Boolean);
  const config = words.indexOf("--config");
  return config === -1 ? words : [...words.slice(0, config), ...words.slice(config + 2)];
}

const runArguments = visualRunArguments();

/** The two fields of the resolved config this file reads: the run's worker count, and every
 *  project in the order the config declares them, which is the order one worker takes them in. */
type ResolvedConfig = {
  readonly projects?: readonly { readonly name?: string }[];
  readonly workers?: number;
};

// Under `CI`, as the visual job's container runs (e2e/visual-run.sh forwards it). Read-only, so it
// starts here and both rows read it.
const visualRunListing = retained(
  listing(import.meta.url, "e2e/playwright.config.ts", { CI: "true" }, runArguments)
);

test(
  "the visual run has one worker, so no command bar row runs beside a page row",
  async () => {
    const config = (await visualRunListing).config as ResolvedConfig | undefined;
    expect(
      config?.workers,
      `The visual run (e2e/visual-run.sh: ${runArguments.join(" ")}) resolves to ` +
        `${JSON.stringify(config?.workers)} workers. The command bar's rows write into the seed ` +
        "every visual row shares, unguarded, and a second worker would run one of them beside a " +
        "page row, whose shot could then draw that write. Keep the config's `workers: 1`, and " +
        "give the visual run no `--workers`."
    ).toBe(1);
  },
  rowLimit("listing")
);

test(
  "the command bar's project is the last project the visual run takes",
  async () => {
    const config = (await visualRunListing).config as ResolvedConfig | undefined;
    const named = projectsOnCommandLine(runArguments);
    const order = (config?.projects ?? [])
      .map((project) => project.name ?? "")
      .filter((name) => named.includes(name.toLowerCase()));
    expect(
      order.at(-1),
      `The visual run takes its projects in this order: ${JSON.stringify(order)}. The command ` +
        "bar's rows (`visual-bar`) write the states they compare into the seed every visual row " +
        "shares, so every page row must draw before the first of them: declare `visual-bar` " +
        "last of `visualProjects` in e2e/playwright.config.ts."
    ).toBe("visual-bar");
  },
  rowLimit("listing")
);
