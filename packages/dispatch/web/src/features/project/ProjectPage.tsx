import { useQuery } from "@tanstack/react-query";
import type { ReactNode } from "react";

import { Link, Navigate, useLocation, useNavigate } from "react-router-dom";

import { api } from "../../api/client";
import { projectArchitectureSourcesQuery, projectsQuery, whoAmIQuery } from "../../api/queries";
import { QueryError } from "../../components/QueryError";
import { type TabDefinition, Tabs } from "../../components/Tabs";
import {
  borderDefault,
  dangerText,
  surfaceMutedBg,
  surfaceMutedStrongBg,
  textMutedOnCanvas,
  textPrimaryOnCanvas,
  textSecondaryHoverToPrimary,
  textSecondaryOnCanvas,
  textSecondaryOnSurface,
} from "../../theme/classes";
import { ArchitecturePage } from "../architecture/ArchitecturePage";
import { BlockedOnYou } from "../inbox/BlockedOnYou";
import { buildProjectPath, parseProjectPath } from "../refs/routes";
import { useKeymap, useKeymapScope } from "../shell/keymap";
import { NotFoundPage } from "../shell/NotFoundPage";
import { useDocumentTitle } from "../shell/useDocumentTitle";
import { useUserPreference } from "../shell/userPreference";
import { DocumentList } from "./DocumentList";
import { IssueBoard } from "./IssueBoard";
import { IssueFilters } from "./IssueFilters";
import { IssuesTable } from "./IssuesTable";

type ProjectTab = "architecture" | "issues" | "documents";

const architectureTab: TabDefinition<ProjectTab> = { id: "architecture", label: "Architecture" };
const tabs: readonly TabDefinition<ProjectTab>[] = [
  { id: "issues", label: "Issues" },
  { id: "documents", label: "Documents" },
];

type IssueView = "list" | "board";

/**
 * A project's page. Its header's groups, in this order: Identity (the project's name and its
 * key), Attention (the questions on this project that wait on the reader), Navigation (the tabs,
 * and beside them, while the read of the project's architecture sources is pending or failed, that
 * read's state, since the Architecture tab exists only once it says there is a source) and View
 * (list or board, and the board's two toggles). A row too narrow for them all moves the last
 * group that does not fit to the next line whole. Below `md` Identity and Attention share the
 * first line and the rest start the next; from `md` Attention goes last, after the view controls.
 */
export function ProjectPage(): ReactNode {
  const location = useLocation();
  const navigate = useNavigate();
  const [issueView, setIssueView] = useUserPreference<IssueView>(
    "project.issue-view",
    (stored) => (stored === "board" ? "board" : "list"),
    (view) => view
  );
  const [showEdges, setShowEdges] = useUserPreference(
    "project.board-edges",
    (stored) => stored === "shown",
    (shown) => (shown ? "shown" : "hidden")
  );
  const [lanes, setLanes] = useUserPreference(
    "project.board-lanes",
    (stored) => stored === "shown",
    (shown) => (shown ? "shown" : "hidden")
  );
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
  useKeymapScope("project");
  useKeymap("project", [
    {
      id: "toggle-view",
      keys: "v",
      label: "Toggle List / Board",
      run: () => setIssueView(issueView === "list" ? "board" : "list"),
      when: () => route?.kind === "issues",
    },
  ]);
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
    // read is shown, never quietly turned into Issues. `replace` keeps Back sane; sidebar links
    // stay on the bare path.
    if (sources.isPending) {
      return null;
    }
    if (sources.isError) {
      return sourceError;
    }
    if (hasSource) {
      return (
        <Navigate replace to={buildProjectPath({ kind: "architecture", project: route.project })} />
      );
    }
    return (
      <Navigate
        replace
        to={{
          pathname: buildProjectPath({ kind: "issues", project: route.project }),
          search: location.search,
        }}
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

  const activeTab: ProjectTab = route.kind;
  const selectTab = (tab: ProjectTab) => {
    navigate(buildProjectPath({ kind: tab, project: route.project }));
  };

  return (
    <section>
      <header className="flex flex-wrap items-center gap-2 md:min-h-12">
        <div className="flex min-w-0 max-w-full items-center gap-2 md:max-w-[50%]">
          <h1
            className={`min-w-0 truncate text-[22px] font-semibold tracking-tight ${textPrimaryOnCanvas}`}
            title={project.name}
          >
            {project.name}
          </h1>
          <Link
            className={`inline-flex min-h-11 shrink-0 items-center rounded-full px-3 text-xs font-semibold md:min-h-9 md:px-2 ${surfaceMutedStrongBg} ${textSecondaryOnSurface} ${textSecondaryHoverToPrimary}`}
            to={buildProjectPath({ kind: "issues", project: route.project })}
          >
            {route.project}
          </Link>
        </div>
        {/* The reader's own questions on this project, and no credential request: the pending
            list is global (a request belongs to no project), so counting one here would make
            this project's number partly about another page. */}
        <BlockedOnYou
          asks={inbox.data ?? []}
          className="shrink-0 md:order-last"
          variant="pill"
          viewer={viewer}
        />
        <div aria-hidden className="basis-full md:hidden" />
        <div className="flex max-w-full flex-wrap items-center gap-x-3 gap-y-1">
          <Tabs
            activeTab={activeTab}
            ariaLabel="Project"
            compact
            idPrefix="project"
            onSelect={selectTab}
            tabs={hasSource ? [architectureTab, ...tabs] : tabs}
          />
          {/* No source read yet is not no source: the reader sees that the tab strip may still
              gain Architecture, or why it cannot say. */}
          {sources.isPending ? (
            <span className={`text-xs ${textMutedOnCanvas}`} role="status">
              Checking for an architecture…
            </span>
          ) : sources.isError ? (
            <span
              className={`flex flex-wrap items-center gap-2 text-xs ${dangerText}`}
              role="alert"
            >
              Could not check for an architecture.
              <button
                className="font-medium underline"
                onClick={() => void sources.refetch()}
                type="button"
              >
                Retry
              </button>
            </span>
          ) : null}
        </div>
        {activeTab === "issues" ? (
          <div className="flex flex-wrap items-center gap-2 md:ml-auto">
            <fieldset className={`inline-flex shrink-0 rounded-xl border p-1 ${borderDefault}`}>
              <legend className="sr-only">Issue view</legend>
              {(["list", "board"] as const).map((view) => (
                <button
                  aria-pressed={issueView === view}
                  className={`min-h-11 rounded-lg px-3 text-sm font-medium md:min-h-9 md:px-2 ${
                    issueView === view ? surfaceMutedStrongBg : surfaceMutedBg
                  } ${textSecondaryOnCanvas}`}
                  key={view}
                  onClick={() => setIssueView(view)}
                  type="button"
                >
                  {view === "list" ? "List" : "Board"}
                </button>
              ))}
            </fieldset>
            {issueView === "board" ? (
              <button
                aria-pressed={showEdges}
                className={`min-h-11 shrink-0 rounded-xl border px-3 text-sm font-medium md:min-h-9 md:px-2 ${borderDefault} ${
                  showEdges ? surfaceMutedStrongBg : surfaceMutedBg
                } ${textSecondaryOnCanvas}`}
                onClick={() => setShowEdges(!showEdges)}
                type="button"
              >
                {showEdges ? "Hide Icebox & Done" : "Show Icebox & Done"}
              </button>
            ) : null}
            {issueView === "board" ? (
              <button
                aria-pressed={lanes}
                className={`min-h-11 shrink-0 rounded-xl border px-3 text-sm font-medium md:min-h-9 md:px-2 ${borderDefault} ${
                  lanes ? surfaceMutedStrongBg : surfaceMutedBg
                } ${textSecondaryOnCanvas}`}
                onClick={() => setLanes(!lanes)}
                type="button"
              >
                {lanes ? "Hide lanes" : "Show lanes"}
              </button>
            ) : null}
          </div>
        ) : null}
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
          issueView === "list" ? (
            <IssuesTable key={route.project} listing={{ project: route.project }} />
          ) : (
            <>
              <IssueFilters key={route.project} project={route.project} />
              {/* Keyed by the project, as the list and the filters are: the board holds what
                  became of its last move, and a refusal of one project's move names that
                  project's issue, so another project's board is drawn with none of it. The key
                  names the board too: the filters beside it are keyed by the same project, and
                  siblings are told apart by their keys. */}
              <IssueBoard
                key={`board:${route.project}`}
                lanes={lanes}
                project={route.project}
                showEdges={showEdges}
              />
            </>
          )
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
    </section>
  );
}
