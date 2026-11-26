import { ISSUE_TITLE_MAX } from "@legion/contracts";
import { useQuery } from "@tanstack/react-query";
import { type ReactNode, useMemo, useState } from "react";
import { Link, useParams } from "react-router-dom";
import { advancesIssuesQuery, prioritiesQuery } from "../../api/queries";
import { DisclosureToggle } from "../../components/DisclosureToggle";
import { EmptyState } from "../../components/EmptyState";
import { LoadingSkeleton } from "../../components/LoadingSkeleton";
import { QueryError } from "../../components/QueryError";
import { issueProject } from "../../lib/issue-key";
import { addressPath, copyAddress } from "../../lib/page-address";
import { card, linkHoverText, linkText, textSecondaryOnCanvas } from "../../theme/classes";
import { selectIssues } from "../project/issues-table-model";
import { ListingTable } from "../project/ListingTable";
import { buildProjectPath } from "../refs/routes";
import { useKeymapScope } from "../shell/keymap";
import { FIELD_CHIP, HEADING_BOX, InPlaceField } from "./InPlaceField";
import { PriorityDescription } from "./PriorityDescription";
import { PriorityMargin } from "./PriorityMargin";
import { buildPriorityItems, ownerOf, type PriorityItem } from "./priorities-model";
import { PRIORITIES_PATH } from "./priority-routes";
import { textWriteLabel, usePriorityWrites } from "./priority-writes";

/** The address prefix the work under the priority keeps its own view under. */
const WORK_PREFIX = "w";

/** The work table's own stored layout (`ListingTable`): the issue, its title, its status and its
 *  project. Every other column is one tick away under Columns, who holds the issue and when it
 *  last changed among them; a project's own table keeps its own layout. The key, the project and
 *  the status are each as wide as what they hold (`fit`), as on each fault list
 *  (`PrioritiesPage.tsx`'s `FAULT_WIDTHS`), so none cuts a key or a pill however wide
 *  production's are. Measured in Chromium in the box a 1280 px window leaves beside the margin,
 *  606 px, a desktop's box (`useTableLayout`), on the review corpus: Key stands at 96 px, its
 *  widest key with the unread dot after it, and at 121 with keys as wide as production's
 *  `INFRA-2947`, which a fixed 96 cut; Project at 65, its header; and Status at 150, its longest
 *  pill, "Needs review". It draws no row's actions after them (`rowActions`), so no empty column
 *  stands at its right: the selection's bar keeps Start next. Title takes all the width they
 *  leave, 251 px there and 226 with production's keys, and wraps (`wrapTitles`) onto up to eight
 *  lines, in which a title of 190 characters reads whole at 226: the work is read by its titles,
 *  which mostly run to 194 characters in production and a few to 1,000, which end in '…' there
 *  with the whole title in its tooltip. It pins the checkbox, the key and Title (`pins`), so its
 *  pinned divider falls after Title as each fault list's does: the registry's pins count the
 *  Priority column this table opens hidden, which in that box left Title no room to stay pinned.
 *  Its key is its own, not the one a scoped table without a layout keeps (`ListingTable`), whose
 *  stored layouts were written for a project's opening set and would show that set again here.
 *  Its status bar draws the counts alone, with the names in a legend under it, where the
 *  segments with their names are wider than the bar (`statusLegend`): beside the margin at
 *  1280 px a priority's work in eight statuses is, and the bar cut "Retro" at its edge. */
const WORK_LAYOUT = {
  key: "priority-page-work",
  opens: ["key", "title", "status", "project"],
  pins: ["select", "key", "title"],
  rowActions: false,
  widths: {
    key: { fit: true },
    project: { fit: true },
    status: { fit: true },
  },
  wrapTitles: true,
  statusLegend: true,
} as const;

/** The way back to the priorities page, above the page's header as above an empty page. */
function BackLink(): ReactNode {
  return (
    <Link className={`text-sm ${linkText} ${linkHoverText}`} to={PRIORITIES_PATH}>
      ‹ Priorities
    </Link>
  );
}

/**
 * `/priorities/<id>`: one priority, edited where it is read, in the shape an issue's page takes.
 * The header is a card, as an issue's is (`IssueHeader.tsx`): the title is its heading, in an
 * issue title's type, which a click opens into the box it is written in, and the owner is a chip
 * in the row under it, which a click opens into its field. The description is a card under the
 * header, with its disclosure, and the work under it after that. Each is saved as it is written,
 * because nothing else in Dispatch has an edit mode or a Save button. The order is not a control
 * here: it is the row's own place on the priorities page, which is where it is dragged.
 *
 * The description is an attribute of the priority node and not a Dispatch document, so this
 * page shows no versions, no comments on it and no anchors; there is no delete, since a
 * priority is retired by its state. What happened to it is beside the page, in the margin.
 */
export function PriorityPage(): ReactNode {
  const { id = "" } = useParams();
  // Keyed by the priority: going from one priority's page to another's draws the page afresh,
  // so each field starts from its own priority's text, and the writes the page held for the one
  // before go back to the shell's line (`usePriorityWrites`).
  return <PriorityView id={id} key={id} />;
}

/** The page of the priority `id`: its read, and the priority drawn once the read holds it. */
function PriorityView({ id }: { id: string }): ReactNode {
  const priorities = useQuery(prioritiesQuery());
  const work = useQuery({ ...advancesIssuesQuery({ advances: id }, true), enabled: id !== "" });
  // The keys every table is walked with (`j`, `k`, `x`, `o`, Enter), which a table registers
  // in the `project` scope: the work table is the one table this page draws, so they are its.
  useKeymapScope("project");
  // The way out of this scope, in the issue table's own shape: the issues advancing this
  // priority, project by project, each as that project's table narrowed to them. The chip the
  // address raises there names this priority and gives the whole table back when it is removed.
  // Grouped once a read, not once a keystroke in the fields above.
  const byProject = useMemo(() => {
    const grouped = new Map<string, string[]>();
    for (const issue of work.data ?? []) {
      const project = issueProject(issue.key);
      const keys = grouped.get(project);
      if (keys === undefined) grouped.set(project, [issue.key]);
      else keys.push(issue.key);
    }
    return grouped;
  }, [work.data]);
  // Each priority's title by its id, which the work table's Advances column names every priority
  // a row advances by. Built once a read, so the table's columns are not rebuilt every render.
  const titles = useMemo(
    () => new Map((priorities.data?.priorities ?? []).map((row) => [row.id, row.title])),
    [priorities.data]
  );

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
      <EmptyState
        action={<BackLink />}
        label="Priority empty state"
        message="No priority of that id is written."
      />
    );
  const owners = [...new Set(items.map(ownerOf))].sort((a, b) => a.localeCompare(b));
  return <PriorityShown byProject={byProject} item={item} owners={owners} titles={titles} />;
}

/**
 * The priority `item` as the page draws it, once the read holds it: its header, its description
 * and the work under it, beside its margin. It is the one part of the page that settles the
 * priority's conflicts with the reader (`usePriorityWrites`), since it draws the fields their
 * texts are offered beside: a page that draws no priority - its read still out or refused, or
 * no priority of its id written - claims none of them, and the shell's line keeps each, the
 * reader's text offered to copy there, rather than a page that offers it nowhere. The read going
 * from under it takes this part off the page, which hands what it claimed back to the line.
 */
function PriorityShown({
  byProject,
  item,
  owners,
  titles,
}: {
  /** The keys of the work advancing the priority, by project. */
  byProject: ReadonlyMap<string, readonly string[]>;
  item: PriorityItem;
  /** Every owner the priorities name, which the owner's field offers. */
  owners: readonly string[];
  titles: ReadonlyMap<string, string>;
}): ReactNode {
  const {
    description,
    description_tree: descriptionTree,
    description_tree_exact: descriptionTreeExact,
    id,
    owner,
    title,
  } = item.priority;
  const [open, setOpen] = useState(true);
  // Where the owner's notice of the reader's kept texts is drawn: under the header's row of
  // chips, not inside it.
  const [ownerNoticeAt, setOwnerNoticeAt] = useState<HTMLElement | null>(null);
  // The one priority this page shows, whose conflicts it settles with the reader
  // (`usePriorityWrites`).
  const shown = useMemo(() => [id], [id]);
  const { canWrite, describe, keep, kept, keptMadeOn, patch, rename } = usePriorityWrites(shown);
  const keptDescription = kept(id, "description", description);

  return (
    <div className="flex flex-col gap-6 lg:flex-row">
      <div className="min-w-0 flex-1 space-y-3">
        <BackLink />
        <header className={`min-w-0 rounded-xl border p-3 ${card}`} data-testid="priority-header">
          {/* The title's notice of the reader's kept texts stands under the heading, outside it:
              drawn in it, it would be read as part of the heading's name. Each field's write
              names the server's text the field last took in (`from`), never the page's read,
              which may hold another person's text the field has not taken in yet, and hands on
              the text the edit was made on (`on`); the page says what each kept text's own
              write was made on (`keptMadeOn`). */}
          {canWrite ? (
            <InPlaceField
              closed="heading"
              kept={kept(id, "title", title)}
              keptMadeOn={(text) => keptMadeOn(id, "title", text)}
              label="Title"
              maxLength={ISSUE_TITLE_MAX}
              onKeptChange={(texts) => keep(id, "title", texts)}
              onSave={(next, from, on) =>
                rename(id, next, from, on, textWriteLabel("title", title, next))
              }
              required
              value={title}
            />
          ) : (
            <h1 className={HEADING_BOX}>{title}</h1>
          )}
          <div className="mt-2 flex flex-wrap items-center gap-2">
            {canWrite ? (
              <InPlaceField
                closed="chip"
                label="Owner"
                noticeAt={ownerNoticeAt}
                onSave={(next) =>
                  patch.mutate({
                    id,
                    input: { owner: next },
                    label: `Owner of the priority “${title}”`,
                  })
                }
                options={owners}
                placeholder="Who holds it"
                value={owner}
              />
            ) : (
              <span className={FIELD_CHIP}>
                {owner.trim() === "" ? null : <span className="font-normal">Owner</span>}
                <span>{ownerOf(item)}</span>
              </span>
            )}
          </div>
          {/* The owner's notice, on a line of its own under the chips. Empty, it takes no room. */}
          <div className="empty:hidden" ref={setOwnerNoticeAt} />
        </header>

        {/* The disclosure's control is 44 px tall around its 20 px line, so the card's padding
            above and below it is that much less than at its sides: its words stand 16 px in from
            each edge, as the card's padding at its sides sets them. */}
        <section aria-label="Description" className={`rounded-xl border px-4 py-1 ${card}`}>
          <DisclosureToggle
            expanded={open}
            label={open ? "Description" : "Description, closed"}
            onToggle={() => setOpen((was) => !was)}
          />
          {/* Drawn while closed too where it keeps the reader's text beside it, so the notice of
              a write another person's change refused is on the page whether it is open or not.
              The card is the editor's box, so the editor draws none of its own. */}
          {open || keptDescription.length > 0 ? (
            <div className="space-y-2 pb-3">
              <PriorityDescription
                boxed={false}
                canWrite={canWrite}
                description={description}
                descriptionTree={descriptionTree}
                descriptionTreeExact={descriptionTreeExact}
                kept={keptDescription}
                keptMadeOn={(text) => keptMadeOn(id, "description", text)}
                label="Description"
                onKeptChange={(texts) => keep(id, "description", texts)}
                onSave={(next, from, on) =>
                  describe(id, next, from, on, textWriteLabel("description", title, next))
                }
              />
            </div>
          ) : null}
        </section>

        <section aria-label="Work advancing this priority" className="space-y-2">
          {byProject.size === 0 ? null : (
            <p className={`flex flex-wrap gap-x-3 text-sm ${textSecondaryOnCanvas}`}>
              {[...byProject.entries()]
                .sort((a, b) => a[0].localeCompare(b[0]))
                .map(([project, keys]) => (
                  <Link
                    className={`${linkText} ${linkHoverText}`}
                    key={project}
                    to={addressPath(
                      buildProjectPath({ kind: "issues", project }),
                      selectIssues(copyAddress(), keys, title)
                    )}
                  >
                    Open in {project}
                  </Link>
                ))}
            </p>
          )}
          <ListingTable
            layout={WORK_LAYOUT}
            listing={{ advances: id }}
            priorityTitles={titles}
            stateKey={WORK_PREFIX}
          />
        </section>
      </div>
      <PriorityMargin id={id} />
    </div>
  );
}
