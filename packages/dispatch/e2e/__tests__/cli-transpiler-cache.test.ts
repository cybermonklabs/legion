/** The run's one transpiler cache for the `dispatch` command (`e2e/cli-transpiler-cache.ts`), as
 *  `runDispatch` (`e2e/dispatch-cli.ts`) hands it to each start of the command.
 *
 *  Every start of one Playwright run reads that cache, each under a HOME of its own. Bun addresses
 *  an entry by the source file's content, so a start after the source changed must run the new
 *  source, whatever the cache already holds for the old one. The row starts one command file
 *  twice through one cache, under two clean HOMEs, and rewrites the file between the starts to the
 *  same length with another word. It reads the new word from the second start; a new entry beside
 *  the first start's, so the cache was in use and the old entry did not answer for the new source;
 *  and nothing in either HOME, so neither start wrote anywhere but the run's directory. Then it
 *  calls the function `globalSetup` hands Playwright, and reads the directory and the variable
 *  gone, which gives a later command its HOME as Bun's cache again.
 *
 *  The command is a file of the row's own rather than `packages/envoy-client/bin/dispatch.ts`:
 *  the row rewrites its source, and starts it with no Dispatch to call. */
import { expect, test } from "bun:test";
import { existsSync, readdirSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories";

import {
  cliTranspilerCacheEnv,
  cliTranspilerCacheVariable,
  openCliTranspilerCache,
} from "../cli-transpiler-cache";
import { rowLimit, runBounded } from "./bounded-subprocess";

const directories = temporaryDirectories("cli-transpiler-cache");

/** The length of the command's source before and after the edit: far over the 4 KB from which Bun
 *  caches a file, and the same both times, so only the content tells the two sources apart. */
const sourceBytes = 65_536;

/** The command's source: it prints `word` and the HOME and state directory it was started with,
 *  and a comment pads it to `sourceBytes`. */
function commandSource(word: "alpha" | "bravo"): string {
  const program =
    `console.log(JSON.stringify({ word: ${JSON.stringify(word)}, ` +
    `home: process.env.HOME, state: process.env.DISPATCH_STATE_DIR }));\n`;
  return `${program}//${"x".repeat(sourceBytes - program.length - 3)}\n`;
}

/** The entries Bun wrote in `cache`: one `.pile` file for each source file it cached. */
function piles(cache: string): string[] {
  const entries = readdirSync(cache, { encoding: "utf-8", recursive: true });
  return entries.filter((entry) => entry.endsWith(".pile"));
}

/** Starts the command once, as `runDispatch` starts the real one: a fresh Bun with `home` as its
 *  HOME, its state directory and its working directory, and the run's cache path beside them.
 *  `label` tells the two starts apart in the cost line, which prints no HOME. */
function start(command: string, home: string, label: string, runEnv: NodeJS.ProcessEnv) {
  return runBounded({
    argv: [process.execPath, command],
    bound: "short",
    cwd: home,
    env: {
      PATH: process.env.PATH ?? "",
      HOME: home,
      DISPATCH_STATE_DIR: home,
      DISPATCH_E2E_CASE: label,
      ...cliTranspilerCacheEnv(runEnv),
    },
    from: import.meta.url,
  });
}

test(
  "a second start of the command under another HOME runs its edited source through the run's one transpiler cache, and the run's cleanup removes the cache and its variable",
  async () => {
    const command = join(directories("command"), "dispatch.ts");
    const firstHome = directories("first-home");
    const secondHome = directories("second-home");
    const runEnv: NodeJS.ProcessEnv = {};
    const close = openCliTranspilerCache(runEnv);
    const cache = runEnv[cliTranspilerCacheVariable] ?? "";
    try {
      // The command is given the directory the run published, as Bun's own variable, and an
      // empty one: the run's cache starts with nothing in it.
      expect(cliTranspilerCacheEnv(runEnv)).toEqual({ BUN_RUNTIME_TRANSPILER_CACHE_PATH: cache });
      expect(piles(cache)).toEqual([]);

      const alpha = commandSource("alpha");
      const bravo = commandSource("bravo");
      expect([alpha.length, bravo.length]).toEqual([sourceBytes, sourceBytes]);

      // 1. The first start runs the source and writes its compiled copy into the run's directory.
      writeFileSync(command, alpha);
      const first = await start(command, firstHome, "first", runEnv);
      expect({ code: first.code, stderr: first.stderr }, first.named).toEqual({
        code: 0,
        stderr: "",
      });
      expect(JSON.parse(first.stdout)).toEqual({
        word: "alpha",
        home: firstHome,
        state: firstHome,
      });
      const written = piles(cache);
      expect(written.length).toBeGreaterThan(0);

      // 2. The same file, rewritten to the same length with another word, started again through
      // the same directory under a HOME of its own, runs the new word and adds an entry for it.
      writeFileSync(command, bravo);
      const second = await start(command, secondHome, "edited", runEnv);
      expect({ code: second.code, stderr: second.stderr }, second.named).toEqual({
        code: 0,
        stderr: "",
      });
      expect(JSON.parse(second.stdout)).toEqual({
        word: "bravo",
        home: secondHome,
        state: secondHome,
      });
      expect(piles(cache).filter((entry) => !written.includes(entry))).not.toEqual([]);

      // 3. Neither start wrote anything under its HOME.
      expect(readdirSync(firstHome)).toEqual([]);
      expect(readdirSync(secondHome)).toEqual([]);
    } finally {
      close();
    }

    // The run's cleanup removed the directory and the variable, so a command started now gets
    // no cache path and Bun keeps its cache under that command's HOME.
    expect(existsSync(cache)).toBe(false);
    expect(runEnv[cliTranspilerCacheVariable]).toBeUndefined();
    expect(cliTranspilerCacheEnv(runEnv)).toEqual({});
  },
  rowLimit("short", "short")
);
