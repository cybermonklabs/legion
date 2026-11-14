import { useQuery } from "@tanstack/react-query";
import type { ReactNode } from "react";

import { Link, useNavigate } from "react-router-dom";

import { api } from "../../api/client";
import {
  legionSettingsQuery,
  projectArchitectureSourcesQuery,
  projectsQuery,
  whoAmIQuery,
} from "../../api/queries";
import { QueryError } from "../../components/QueryError";
import { type TabDefinition, Tabs } from "../../components/Tabs";
import { AddressRedirect, usePageAddress } from "../../lib/page-address";
import {
  dangerText,
  surfaceMutedStrongBg,
  textMutedOnCanvas,
  textPrimaryOnCanvas,
  textSecondaryHoverToPrimary,
  textSecondaryOnSurface,
} from "../../theme/classes";
import { ArchitecturePage } from "../architecture/ArchitecturePage";
import { BlockedOnYou } from "../inbox/BlockedOnYou";
import { buildProjectPath, parseProjectPath } from "../refs/routes";
import { useKeymapScope } from "../shell/keymap";
import { NotFoundPage } from "../shell/NotFoundPage";
import { useDocumentTitle } from "../shell/useDocumentTitle";
import { DocumentList } from "./DocumentList";
import { IssuesTable } from "./IssuesTable";
import { ProjectSettings } from "./ProjectSettings";

type ProjectTab = "architecture" | "issues" | "documents" | "settings";

const architectureTab: TabDefinition<ProjectTab> = { id: "architecture", label: "Architecture" };
const settingsTab: TabDefinition<ProjectTab> = { id: "settings", label: "Settings" };
const tabs: readonly TabDefinition<ProjectTab>[] = [
  { id: "issues", label: "Issues" },
  { id: "documents", label: "Documents" },
];

export function ProjectPage(): ReactNode {
  const location = usePageAddress();
  const navigate = useNavigate();
  const route = parseProjectPath(location.pathname, location.search);
  const projectKey = route?.project;
  const inbox = useQuery({
    queryKey: ["inbox", projectKey],
    queryFn: () => api.getInbox(projectKey),
    enabled: projectKey !== undefined,
  });
  // Whose questions the pill counts, read as every other surface that counts work waiting on
  // the reader reads it.
  const viewer = useQuery(whoAmIQuery()).data?.login;
  const projects = useQuery(projectsQuery());
  const project = projects.data?.find((candidate) => candidate.key === route?.project);
  // Whether the project has an architecture source decides where the bare project path opens
  // and whether the Architecture tab exists at all: one read of its source list.
  const sources = useQuery({
    ...projectArchitectureSourcesQuery(projectKey ?? ""),
    enabled: projectKey !== undefined,
  });
  const hasSource = (sources.data?.length ?? 0) > 0;
  // Which projects name a Legion daemon: one deployment-wide read. A project that names one has
  // a Settings tab, which holds its daemon's settings panel.
  const legionSettings = useQuery(legionSettingsQuery());
  const hasLegion =
    projectKey !== undefined &&
    (legionSettings.data ?? []).some((setting) => setting.project === projectKey);
  useKeymapScope("project");
  useDocumentTitle(
    project === undefined || route === undefined
      ? "Not found · Dispatch"
      : `${route.project} · ${project.name} · Dispatch`
  );
  if (route === undefined || route.kind === "document") {
    return <NotFoundPage />;
  }
  const sourceError = (
    <QueryError
      message="Could not load this project's architecture sources."
      onRetry={() => void sources.refetch()}
      retrying={sources.isFetching}
    />
  );
  if (projects.isPending) {
    return <p className={textMutedOnCanvas}>Loading project…</p>;
  }
  if (projects.isError) {
    return <p className={dangerText}>Could not load this project.</p>;
  }
  if (project === undefined) {
    return <NotFoundPage />;
  }
  if (route.kind === "project") {
    // The open-on rule: a project with an architecture source opens on its Architecture; one
    // without (the list is empty) opens on Issues, the filter parameters forwarded. A failed
    // read is shown, never quietly turned into Issues. `AddressRedirect` replaces the entry, so
    // Back leaves for the page before; sidebar links stay on the bare path.
    if (sources.isPending) {
      return null;
    }
    if (sources.isError) {
      return sourceError;
    }
    if (hasSource) {
      return (
        <AddressRedirect to={buildProjectPath({ kind: "architecture", project: route.project })} />
      );
    }
    return (
      <AddressRedirect
        keepQuery
        to={buildProjectPath({ kind: "issues", project: route.project })}
      />
    );
  }
  if (route.kind === "architecture") {
    // Like the bare path: nothing until the source lookup settles, so the tab strip never
    // renders without its active tab.
    if (sources.isPending) {
      return null;
    }
    if (sources.isError) {
      return sourceError;
    }
    if (!hasSource) {
      // A direct link to /architecture on a project without a source: nothing to show there.
      return <NotFoundPage />;
    }
  }
  if (route.kind === "settings") {
    if (legionSettings.isPending) return null;
    if (!hasLegion) return <NotFoundPage />;
  }

  const activeTab: ProjectTab = route.kind;
  const selectTab = (tab: ProjectTab) => {
    navigate(buildProjectPath({ kind: tab, project: route.project }));
  };

  return (
    <section>
      {/* One strip at `md` and above. Every control is `shrink-0`, so something has to give
          when they outgrow the row. The title gives, truncating, which is what a long project
          name should do; otherwise the last control - the blocked-on-you pill - would be pushed
          off the right edge and unreachable. */}
      <header className="flex flex-wrap items-center gap-2 md:min-h-12 md:flex-nowrap">
        {/* `flex-initial` at `md`: the title takes the width its name needs and no more, so
            the project key sits beside a short name instead of 380px away, and only a long
            name truncates - carrying the whole name as its `title`. */}
        <h1
          className={`order-1 min-w-0 flex-1 truncate text-[22px] font-semibold tracking-tight md:flex-initial ${textPrimaryOnCanvas}`}
          title={project.name}
        >
          {project.name}
        </h1>
        <Link
          className={`order-2 inline-flex min-h-11 shrink-0 items-center rounded-full px-3 text-xs font-semibold md:min-h-9 md:px-2 ${surfaceMutedStrongBg} ${textSecondaryOnSurface} ${textSecondaryHoverToPrimary}`}
          to={buildProjectPath({ kind: "issues", project: route.project })}
        >
          {route.project}
        </Link>
        {/* The reader's own questions on this project, and no credential request: the pending
            list is global (a request belongs to no project), so counting one here would make
            this project's number partly about another page. */}
        <BlockedOnYou
          asks={inbox.data ?? []}
          className="order-3 shrink-0 md:order-5"
          variant="pill"
          viewer={viewer}
        />
        <div aria-hidden className="order-4 basis-full md:hidden" />
        <div className="order-5 shrink-0 md:order-3">
          <Tabs
            activeTab={activeTab}
            ariaLabel="Project"
            compact
            idPrefix="project"
            onSelect={selectTab}
            tabs={[
              ...(hasSource ? [architectureTab] : []),
              ...tabs,
              ...(hasLegion ? [settingsTab] : []),
            ]}
          />
        </div>
      </header>
      {hasSource ? (
        <div
          aria-hidden={activeTab !== "architecture"}
          aria-labelledby="project-architecture-tab"
          className="mt-3"
          hidden={activeTab !== "architecture"}
          id="project-architecture-panel"
          role="tabpanel"
        >
          {activeTab === "architecture" ? <ArchitecturePage project={route.project} /> : null}
        </div>
      ) : null}
      <div
        aria-hidden={activeTab !== "issues"}
        aria-labelledby="project-issues-tab"
        className="mt-3"
        hidden={activeTab !== "issues"}
        id="project-issues-panel"
        role="tabpanel"
      >
        {activeTab === "issues" ? (
          <IssuesTable key={route.project} project={route.project} />
        ) : null}
      </div>
      <div
        aria-hidden={activeTab !== "documents"}
        aria-labelledby="project-documents-tab"
        hidden={activeTab !== "documents"}
        id="project-documents-panel"
        role="tabpanel"
      >
        {activeTab === "documents" ? <DocumentList project={route.project} /> : null}
      </div>
      {hasLegion ? (
        <div
          aria-hidden={activeTab !== "settings"}
          aria-labelledby="project-settings-tab"
          className="mt-3"
          hidden={activeTab !== "settings"}
          id="project-settings-panel"
          role="tabpanel"
        >
          {activeTab === "settings" ? <ProjectSettings project={route.project} /> : null}
        </div>
      ) : null}
    </section>
  );
}
