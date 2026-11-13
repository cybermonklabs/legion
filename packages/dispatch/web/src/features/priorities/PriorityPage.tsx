import { useQuery } from "@tanstack/react-query";
import { type ReactNode, useMemo, useState } from "react";
import { Link, useParams } from "react-router-dom";
import { advancesIssuesQuery, prioritiesQuery } from "../../api/queries";
import { DisclosureToggle } from "../../components/DisclosureToggle";
import { LoadingSkeleton } from "../../components/LoadingSkeleton";
import { QueryError } from "../../components/QueryError";
import {
  linkHoverText,
  linkText,
  textMutedOnCanvas,
  textPrimaryOnCanvas,
  textSecondaryOnCanvas,
} from "../../theme/classes";
import { IssuesTable } from "../project/IssuesTable";
import { selectIssues } from "../project/issues-table-model";
import { InPlaceField } from "./InPlaceField";
import { PriorityDescription } from "./PriorityDescription";
import { PriorityMargin } from "./PriorityMargin";
import { buildPriorityItems, ownerOf } from "./priorities-model";
import { PRIORITIES_PATH } from "./priority-routes";
import { usePriorityWrites } from "./priority-writes";

/** The address prefix the work under the priority keeps its own view under. */
const WORK_PREFIX = "w";

/** The owner field's id, which the visible "Owner" beside it points at. */
const OWNER_FIELD = "priority-owner";

/**
 * `/priorities/<id>`: one priority, edited where it is read. The title is written in the
 * header, the owner in the control beside it, and the description in the disclosure under
 * them; each is saved as it is written, because nothing else in Dispatch has an edit mode or a
 * Save button. The order is not a control here: it is the row's own place on the priorities
 * page, which is where it is dragged.
 *
 * The description is an attribute of the priority node and not a Dispatch document, so this
 * page shows no versions, no comments on it and no anchors; there is no delete, since a
 * priority is retired by its state. What happened to it is beside the page, in the margin.
 */
export function PriorityPage(): ReactNode {
  const { id = "" } = useParams();
  const priorities = useQuery(prioritiesQuery());
  const work = useQuery({ ...advancesIssuesQuery({ advances: id }, true), enabled: id !== "" });
  const [open, setOpen] = useState(true);
  const { canWrite, patch } = usePriorityWrites();
  // The way out of this scope, in the issue table's own shape: the issues advancing this
  // priority, project by project, each as that project's table narrowed to them. The chip the
  // address raises there names this priority and gives the whole table back when it is removed.
  // Grouped once a read, not once a keystroke in the fields above.
  const byProject = useMemo(() => {
    const grouped = new Map<string, string[]>();
    for (const issue of work.data ?? []) {
      const project = issue.key.split("-")[0] ?? issue.key;
      const keys = grouped.get(project);
      if (keys === undefined) grouped.set(project, [issue.key]);
      else keys.push(issue.key);
    }
    return grouped;
  }, [work.data]);

  if (priorities.isPending) return <LoadingSkeleton label="Loading the priority" />;
  if (priorities.isError)
    return (
      <QueryError
        error={priorities.error}
        message="Could not load the priority."
        onRetry={() => void priorities.refetch()}
        retrying={priorities.isFetching}
      />
    );
  const items = buildPriorityItems(priorities.data.priorities ?? []);
  const item = items.find((candidate) => candidate.priority.id === id);
  if (item === undefined)
    return (
      <div className="space-y-2">
        <p className={textPrimaryOnCanvas}>No priority of that id is written.</p>
        <Link className={`${linkText} ${linkHoverText}`} to={PRIORITIES_PATH}>
          ‹ Priorities
        </Link>
      </div>
    );

  const { description, title } = item.priority;
  const owners = [...new Set(items.map(ownerOf))].sort((a, b) => a.localeCompare(b));

  return (
    <div className="flex flex-col gap-6 lg:flex-row">
      <div className="min-w-0 flex-1 space-y-4">
        <Link className={`text-sm ${linkText} ${linkHoverText}`} to={PRIORITIES_PATH}>
          ‹ Priorities
        </Link>
        <header className="space-y-2">
          <div className="flex flex-wrap items-center gap-3">
            <span className={`text-2xl font-semibold ${textMutedOnCanvas}`}>{item.order}</span>
            <h1 className={`min-w-64 flex-1 text-2xl font-semibold ${textPrimaryOnCanvas}`}>
              {canWrite ? (
                <InPlaceField
                  label="Title"
                  onSave={(next) => {
                    const named = next.trim();
                    if (named === "" || named === title) return;
                    patch.mutate({
                      id,
                      input: { title: named },
                      label: `Rename the priority “${title}” to “${named}”`,
                    });
                  }}
                  value={title}
                />
              ) : (
                title
              )}
            </h1>
          </div>
          <div
            className={`flex flex-wrap items-center gap-x-6 gap-y-2 text-sm ${textSecondaryOnCanvas}`}
          >
            {canWrite ? (
              <span className="flex items-center gap-2">
                <label htmlFor={OWNER_FIELD}>Owner</label>
                <InPlaceField
                  id={OWNER_FIELD}
                  label="Owner"
                  onSave={(owner) =>
                    patch.mutate({
                      id,
                      input: { owner },
                      label: `Owner of the priority “${title}”`,
                    })
                  }
                  options={owners}
                  placeholder="Who holds it"
                  value={item.priority.owner}
                />
              </span>
            ) : (
              <span className="flex items-center gap-2">
                <span>Owner</span>
                <span>{ownerOf(item)}</span>
              </span>
            )}
            <p>Order {item.order}, dragged on the priorities page</p>
            <p>
              In flight {item.inFlight} · waiting {item.waiting} · in backlog or icebox{" "}
              {item.parked}
            </p>
          </div>
        </header>

        <section aria-label="Description" className="space-y-2">
          <DisclosureToggle
            expanded={open}
            label={open ? "Description" : "Description, closed"}
            onToggle={() => setOpen((was) => !was)}
          />
          {open ? (
            <PriorityDescription
              canWrite={canWrite}
              description={description}
              label={`Description of ${item.order}`}
              onSave={(next) =>
                patch.mutate({
                  id,
                  input: { description: next },
                  label: `Description of the priority “${title}”`,
                })
              }
            />
          ) : null}
        </section>

        <section aria-label="Work advancing this priority" className="space-y-2">
          {byProject.size === 0 ? null : (
            <p className={`text-sm ${textSecondaryOnCanvas}`}>
              Leave this scope:{" "}
              {[...byProject.entries()]
                .sort((a, b) => a[0].localeCompare(b[0]))
                .map(([project, keys]) => (
                  <Link
                    className={`mr-3 ${linkText} ${linkHoverText}`}
                    key={project}
                    to={`/projects/${project}/issues?${selectIssues(new URLSearchParams(), keys, title)}`}
                  >
                    {project}’s issues table
                  </Link>
                ))}
            </p>
          )}
          <IssuesTable listing={{ advances: id }} stateKey={WORK_PREFIX} />
        </section>
      </div>
      <PriorityMargin id={id} />
    </div>
  );
}
