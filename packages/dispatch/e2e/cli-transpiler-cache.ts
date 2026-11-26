/** The one directory where Bun keeps its transpiler cache for every `dispatch` command a Playwright
 *  run starts (`runDispatch`, `e2e/dispatch-cli.ts`).
 *
 *  Each row starts the command as a fresh `bun` process under a HOME of its own. Bun keeps its
 *  runtime transpiler cache under HOME unless `BUN_RUNTIME_TRANSPILER_CACHE_PATH` names another
 *  directory, so each start compiled the command's modules again and wrote them into its row's
 *  HOME. The cache holds the compiled output of each source file over 4 KB and its source map,
 *  addressed by the file's content, and no runtime state
 *  (https://bun.com/docs/runtime/environment-variables#runtime-transpiler-caching): a file whose
 *  source changes gets a new entry, and a start reads nothing another start wrote but that
 *  compiled code. So one directory serves the whole run, and each start still reads its
 *  configuration, its token and its session state from its own HOME and environment alone.
 *
 *  `globalSetup` (`e2e/global-setup.ts`) makes the directory for the run under the temporary
 *  directory and publishes it in `cliTranspilerCacheVariable`, which every worker inherits, and
 *  hands Playwright the function below that removes the directory and the variable when the run
 *  ends. Where the variable is unset - a run of the stubbed layer alone, or a caller outside the
 *  runner - the command gets no cache path, and Bun keeps the cache under the row's HOME as it did
 *  before. `e2e/__tests__/cli-transpiler-cache.test.ts` starts a command twice through one such
 *  directory. */
import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

/** The variable the run's cache directory is published in. `cliTranspilerCacheEnv` hands its value
 *  to the command as Bun's own variable, and nothing else of the run reads it. */
export const cliTranspilerCacheVariable = "DISPATCH_E2E_CLI_TRANSPILER_CACHE";

/** Makes a fresh directory for this run's cache and publishes it in this process's environment,
 *  and answers the function that takes the variable out again and removes the directory. */
export function openCliTranspilerCache(env: NodeJS.ProcessEnv = process.env): () => void {
  const directory = mkdtempSync(join(tmpdir(), "dispatch-e2e-cli-transpiler-"));
  const previous = env[cliTranspilerCacheVariable];
  env[cliTranspilerCacheVariable] = directory;
  return () => {
    if (env[cliTranspilerCacheVariable] === directory) {
      if (previous === undefined) delete env[cliTranspilerCacheVariable];
      else env[cliTranspilerCacheVariable] = previous;
    }
    rmSync(directory, { force: true, recursive: true });
  };
}

/** What a command's environment takes from the run: Bun's cache path where the run published one,
 *  and nothing where it did not. An empty value counts as none, since Bun reads an empty path as
 *  the cache turned off. */
export function cliTranspilerCacheEnv(env: NodeJS.ProcessEnv = process.env): {
  readonly BUN_RUNTIME_TRANSPILER_CACHE_PATH?: string;
} {
  const directory = env[cliTranspilerCacheVariable];
  return directory ? { BUN_RUNTIME_TRANSPILER_CACHE_PATH: directory } : {};
}
