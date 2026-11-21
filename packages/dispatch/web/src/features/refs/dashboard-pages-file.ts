/**
 * The dashboard's pages file: what the dashboard's route table (`routes.ts`) reads in the address
 * of a page, written as data for the readers that cannot import this file. The server's reader of
 * a dashboard address (`packages/envoy/internal/dispatch/text`, which embeds the file), the agent
 * client's (`@legion/envoy-client`'s `dispatchRefFromUrl`) and the shared table of hrefs
 * (`DISPATCH_HREF_REFERENCES` in `@legion/contracts`) read it, and the form of The Spec reads an
 * address of a document against each page the file names for it. It holds:
 *
 * - `tabs`: each tab of an issue whose page names the issue and no one document, by the path
 *   segment that opens it and the segment of its `dispatch://` reference;
 * - `document_segments`: the path segments under an issue that open a document by its slug;
 * - `document_pages`: for each kind of document, every page the issue page, a project's page or
 *   an agent's page draws it on, `{key}`, `{project}`, `{session}` and `{slug}` standing for its
 *   owner and its slug;
 * - `references`: one href of each tab and each document segment, with the reference the
 *   dashboard reads it as, the rows every reader is held to.
 *
 * `bun web/src/features/refs/dashboard-pages-file.ts --write` (from `packages/dispatch`) writes it;
 * `dashboard-pages-file.test.ts` renders it again and fails when the committed file differs, so a
 * tab added to `ISSUE_TABS` fails that test until the file is written again.
 */
import type { Artifact, DocumentRole, IssueDetails } from "../../api/types";
import { issueDocuments } from "../issue/issue-documents";
import {
  buildDispatchReference,
  buildProjectPath,
  buildReferencePath,
  ISSUE_DOCUMENT_SEGMENTS,
  ISSUE_TABS,
  type IssueRoute,
  type IssueTab,
  parseIssuePath,
  referencedArtifact,
  referenceRouteFromHref,
} from "./routes";

/** Where the file is committed: beside the server's reader, which embeds it. */
export const DASHBOARD_PAGES_FILE = new URL(
  "../../../../../envoy/internal/dispatch/text/dashboard-pages.json",
  import.meta.url
);

const origin = "http://dispatch.test";
const key = "CORE-1";
const roles: readonly DocumentRole[] = ["verification", "plan", "decisions"];

/** One document of each kind of an issue: the spec, one document of each role, and one more. */
const documents = [
  { id: "a-spec", slug: "spec", kind: "primary" },
  ...roles.map((role) => ({ id: `a-${role}`, slug: `${role}-md`, kind: role })),
  { id: "a-notes", slug: "notes-md", kind: "issue" },
] as const;

/** The issue that holds them, as the issue page reads its details. */
const issue = {
  artifacts: documents.map(
    ({ id, slug, kind }) => ({ id, slug, kind: "doc", primary: kind === "primary" }) as Artifact
  ),
  primary_artifact_id: "a-spec",
  verification_artifact_id: "a-verification",
  plan_artifact_id: "a-plan",
  decisions_artifact_id: "a-decisions",
} as Pick<
  IssueDetails,
  | "artifacts"
  | "primary_artifact_id"
  | "verification_artifact_id"
  | "plan_artifact_id"
  | "decisions_artifact_id"
>;

/** The document the issue page draws at route: the active tab's document, by the page's own
 *  choice (`issueDocuments`, which `IssuePage.tsx` draws each tab from); a tab with no document
 *  draws none. */
function drawnDocument(route: IssueRoute): Artifact | undefined {
  const { activeTab, drawn } = issueDocuments(route, issue);
  const byTab: Partial<Record<IssueTab, Artifact | undefined>> = drawn;
  return byTab[activeTab];
}

/** Every page of the issue: its own, each tab's, and each document's by each segment. */
const issuePages = [
  `/issues/${key}`,
  ...ISSUE_TABS.map((tab) => `/issues/${key}/${tab}`),
  ...ISSUE_DOCUMENT_SEGMENTS.flatMap((segment) =>
    documents.map(({ slug }) => `/issues/${key}/${segment}/${slug}`)
  ),
];

/** The page as a pattern of its kind of document: the issue's key, and the slug a document
 *  segment opens, as placeholders. */
function template(page: string, slug: string): string {
  const pattern = page.replace(`/issues/${key}`, "/issues/{key}");
  const bySlug = ISSUE_DOCUMENT_SEGMENTS.some((segment) => pattern.endsWith(`/${segment}/${slug}`));
  return bySlug ? `${pattern.slice(0, -slug.length)}{slug}` : pattern;
}

/** The reference the dashboard reads the page of href as. */
function referenceOf(href: string): string {
  const route = referenceRouteFromHref(href, origin);
  if (route === undefined) {
    throw new Error(`the dashboard reads ${href} as no reference`);
  }
  return buildDispatchReference(route);
}

/** The file's content, as the route table reads it today. */
export function renderDashboardPages(): string {
  const tabs = ISSUE_TABS.flatMap((tab) => {
    const href = `/issues/${key}/${tab}`;
    const route = referenceRouteFromHref(href, origin);
    if (route === undefined) {
      throw new Error(`the dashboard reads the page of the tab ${tab}, ${href}, as no reference`);
    }
    // A tab whose page is a document's (the Spec tab, the Decisions tab) is read as that document.
    if (route.kind === "spec" || referencedArtifact(route) !== undefined) {
      return [];
    }
    return [{ page: tab, reference: referenceOf(href).slice(`dispatch://${key}/`.length) }];
  });
  const documentPages: Record<string, string[]> = {};
  for (const document of documents) {
    documentPages[document.kind] = issuePages
      .filter((page) => {
        const route = parseIssuePath(page);
        return route !== undefined && drawnDocument(route)?.id === document.id;
      })
      .map((page) => template(page, document.slug));
  }
  documentPages.project = [
    buildProjectPath({ kind: "document", project: "CORE", slug: "handbook-md" })
      .replace("/CORE/", "/{project}/")
      .replace(/handbook-md$/, "{slug}"),
  ];
  documentPages.agent = [
    buildReferencePath({ kind: "agent-artifact", session: "s-1", slug: "notes-md" })
      .replace("/s-1/", "/{session}/")
      .replace(/notes-md$/, "{slug}"),
  ];
  const references = [
    ...tabs.map(({ page }) => `/issues/${key}/${page}`),
    ...ISSUE_DOCUMENT_SEGMENTS.flatMap((segment) => [
      `/issues/${key}/${segment}/notes-md`,
      `/issues/${key}/${segment}/notes-md?v=2`,
    ]),
  ].map((href) => ({ href, ref: referenceOf(href) }));
  const file = {
    about:
      "Generated from the dashboard's route table by packages/dispatch/web/src/features/refs/dashboard-pages-file.ts; do not edit.",
    tabs,
    document_segments: [...ISSUE_DOCUMENT_SEGMENTS],
    document_pages: documentPages,
    references,
  };
  return `${JSON.stringify(file, null, 2)}\n`;
}

if (import.meta.main) {
  if (!process.argv.includes("--write")) {
    throw new Error("run with --write to write the dashboard's pages file");
  }
  await Bun.write(DASHBOARD_PAGES_FILE, renderDashboardPages());
}
