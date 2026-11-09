/** What a run of `e2e/playwright.config.ts` wants of the stubbed layer, read once.
 *
 *  Three questions, and the config asks each of them exactly once:
 *
 *  - **Is this run recording?** (`DISPATCH_E2E_STUB_RECORD=1`, `bun run e2e:stub:record`.) Then
 *    the stubbed rows meet the real harness, so the config takes the harness's base URL for
 *    them, starts no static server, and keeps every refusal that a harness run meets.
 *  - **Does this run hold a stubbed row?** A run that names a stub project does; so does a run
 *    that names no project at all, which runs every project of its mode. That is what decides
 *    whether the static file server is declared, and it is not idle bookkeeping: Playwright
 *    starts a `webServer` for every run of a config but a listing, whatever projects the run
 *    selects, and the containerised `visual` job of the full lane runs this config where there
 *    is no bun to start it with (`e2e/__tests__/stub-web-server.test.ts` holds both directions).
 *  - **Is this run the stubbed layer alone?** Then it reaches no server of the harness - no Go
 *    server, no Postgres, no fake listener - so the config's harness refusals and its global
 *    setup are about a harness it does not want, and it is exempt from them. Every refusal
 *    keeps its sentence for every other run, so a person who forgets the harness still gets the
 *    line that fixes it.
 *
 *  A deployed run (`PLAYWRIGHT_BASE_URL`, `bun run e2e:deployed`) holds no stubbed row whatever
 *  it names. The stubbed layer replays a recording against a build this checkout made, so
 *  running it against a deployed server would prove something about neither: the config leaves
 *  the stub projects out of such a run and declares no server for it.
 */

/** Set to `1` by `bun run e2e:stub:record`: the rows run against the real harness and write
 *  what they fetched. Unset, which is every other run, they replay. */
export const stubRecordVariable = "DISPATCH_E2E_STUB_RECORD";

/** Carries the stub-only decision to the workers. Every worker re-imports the config, and a
 *  worker's own argv is Playwright's (`.../workerMain.js`) rather than the command line, so a
 *  worker that read the selection itself would answer `false` and meet a refusal the run was
 *  exempt from. The process that reads the command line writes this, and Playwright gives each
 *  worker its environment. Setting it by hand is how a person runs this config against no
 *  harness at all; it exempts the whole run, real-stack projects included, so a run that
 *  selects one of those and sets this gets no refusal and no preflight either. */
export const stubOnlyVariable = "DISPATCH_E2E_STUB_ONLY";

/** What one run wants of the stubbed layer. */
export interface StubSelection {
  /** The rows meet the real harness and write what they fetched. */
  readonly recording: boolean;
  /** The run holds at least one stubbed row, so it is given the static file server. */
  readonly holdsStubRow: boolean;
  /** The run is the stubbed layer alone, so it is exempt from the harness refusals and the
   *  global setup. */
  readonly onlyStub: boolean;
}

/** The projects `--project` names on `argv`, in either spelling Playwright takes, read before a
 *  `--` separator. An empty list is a run that names none, which is every project of its mode. */
export function projectsOnCommandLine(argv: readonly string[]): string[] {
  const separator = argv.indexOf("--");
  const scanned = separator === -1 ? argv : argv.slice(0, separator);
  const named: string[] = [];
  for (const [index, argument] of scanned.entries()) {
    if (argument === "--project") named.push(scanned[index + 1] ?? "");
    else if (argument.startsWith("--project=")) named.push(argument.slice("--project=".length));
  }
  return named;
}

/** Reads the three questions above once, and writes `stubOnlyVariable` where the answer to the
 *  third is yes, so every worker of the run reads the same answer.
 *
 *  `projects` is the names of the stubbed layer's projects, and `processes` the run's Dispatch
 *  server count: two-process mode runs its own project list, which holds no stub project. */
export function stubSelection(options: {
  argv: readonly string[];
  env: NodeJS.ProcessEnv;
  processes: number;
  projects: readonly string[];
}): StubSelection {
  const { argv, env, processes, projects } = options;
  const recording = (env[stubRecordVariable] ?? "") === "1";
  const deployed = (env.PLAYWRIGHT_BASE_URL ?? "") !== "";
  const named = projectsOnCommandLine(argv);
  const reachable = processes === 1 && !deployed;
  const holdsStubRow =
    reachable && (named.length === 0 || named.some((project) => projects.includes(project)));
  const onlyStub =
    env[stubOnlyVariable] === "1" ||
    (reachable &&
      !recording &&
      named.length > 0 &&
      named.every((project) => projects.includes(project)));
  if (onlyStub) env[stubOnlyVariable] = "1";
  return { holdsStubRow, onlyStub, recording };
}
