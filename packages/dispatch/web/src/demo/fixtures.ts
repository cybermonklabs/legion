// The views demo's fixtures (packages/dispatch/e2e/views-demo/README.md): the production reads the
// stub answers from. The dev server serves them from DISPATCH_DEMO_FIXTURES under FIXTURE_PATH
// (vite.demo.config.ts); the single-file build inlines them as `window.__DISPATCH_DEMO_FIXTURES__`,
// so a page opened from the disk reads nothing.
import type { IssueSummary, LegionSetting, Project, UserIssueState } from "@legion/contracts";
import type { BoardPopulation } from "../../../e2e/views-demo/board-rules";

/** Where the dev server serves each fixture file. */
export const FIXTURE_PATH = "/__demo/fixtures/";

/** What the manifest says of one fixture: where it came from, when it was read, and which of its
 *  fields are real, derived or invented. */
export interface FixtureRecord {
  source: string;
  read_at: string | null;
  real: string[];
  derived: string[];
  invented: string[];
}

export interface DemoManifest {
  built_at: string;
  /** The project with a Legion daemon: the one the board, the setting and the facts are of. */
  project: string;
  /** Real projects whose issues the fixtures hold too, with no Legion setting, and when each
   *  one's listings were read: the views as a project with no daemon draws them. */
  no_daemon_projects: { key: string; read_at: string }[];
  reader: string;
  /** When the issues and the daemon's state were read, for the banner. */
  issues_read_at: string;
  daemon_read_at: string;
  /** The figures estimated from production reads by a rule of the demo's own, in the banner's
   *  words. */
  derived_summary: string[];
  /** The figures no production read holds, in the banner's words. */
  invented_summary: string[];
  fixtures: Record<string, FixtureRecord>;
}

/** The reader the page is signed in as, and the server state that belongs to them. */
export interface ReaderFixture {
  login: string;
  user_state: Record<string, UserIssueState>;
  legion_settings: LegionSetting[];
}

export interface DemoFixtures {
  manifest: DemoManifest;
  /** Every issue of the project, open and done, as the listing answers it. */
  issues: IssueSummary[];
  population: BoardPopulation;
  projects: Project[];
  reader: ReaderFixture;
  /** The server's block schema (`GET /api/v1/schema/blocks`). */
  schema: unknown;
  /** The agent sessions Dispatch listed (`GET /api/v1/agents`). */
  agents: unknown[];
}

/** Every fixture file the stub loads, `<name>.json` each. */
export const FIXTURE_NAMES = [
  "manifest",
  "issues",
  "population",
  "projects",
  "reader",
  "schema",
  "agents",
] as const;

declare global {
  interface Window {
    __DISPATCH_DEMO_FIXTURES__?: DemoFixtures;
  }
}

/** The fixtures, from the page itself where the build inlined them, else from the dev server
 *  through `fetchImpl`, the browser's own fetch from before the stub replaced it. */
export async function loadFixtures(fetchImpl: typeof fetch): Promise<DemoFixtures> {
  const inlined = window.__DISPATCH_DEMO_FIXTURES__;
  if (inlined !== undefined) return inlined;
  const read = await Promise.all(
    FIXTURE_NAMES.map(async (name) => {
      const response = await fetchImpl(`${FIXTURE_PATH}${name}.json`);
      if (!response.ok) throw new Error(`demo fixture ${name}.json: ${response.status}`);
      return [name, await response.json()] as const;
    })
  );
  return Object.fromEntries(read) as unknown as DemoFixtures;
}
