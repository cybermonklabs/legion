import type { LegionRole } from "@legion/contracts";
import { useQuery } from "@tanstack/react-query";
import { type ReactNode, type RefObject, useRef } from "react";

import { issueCollaboratorsQuery } from "../../api/queries";
import type { IssueCollaboratorPerson } from "../../api/types";
import {
  pageInkText,
  pageLineBorder,
  pageMutedText,
  pagePaperBg,
  pageQuietText,
} from "../../theme/classes";
import { collaboratorSections } from "../issue/CollaboratorsTab";
import { AgentName } from "../refs/AgentName";
import { standsFailed } from "../refs/reference-target";
import { Timestamp } from "../refs/Timestamp";
import { DetailsErrorLine, retryRead, useRetryHandOff } from "./details-error";
import { groupsDrawn, useListPages } from "./details-leaves";
import { PROPERTY_LABELS, pageChrome } from "./page-structure";
import { ANSWER_LINES, CONTROL_LABELS, EMPTY_STATE_LINES, STATE_LINES } from "./page-words";

/** The page's word for each Legion role's list, and for each way a collaborator worked the
 *  issue: every token the contract declares has one. */
const ROLE_WORDS: Readonly<Record<LegionRole, string>> = CONTROL_LABELS.collaborator_roles;
const REASON_WORDS: Readonly<
  Record<NonNullable<IssueCollaboratorPerson["reasons"]>[number], string>
> = CONTROL_LABELS.collaborator_reasons;

function SectionHeading({ children }: { children: ReactNode }): ReactNode {
  return (
    <h2 className={`text-sm font-semibold ${pageMutedText}`} {...pageChrome}>
      {children}
    </h2>
  );
}

/** One collaborator's row: who, then the line that says how they worked the issue, on the
 *  page's paper inside its line. `data-collaborator` marks the row's own name, which a test
 *  selects instead of the row's shape, since the detail line can name another session (the one
 *  that replaced this row's). */
function Row({ detail, name }: { detail: ReactNode; name: ReactNode }): ReactNode {
  return (
    <li className={`min-w-0 rounded-lg border p-3 break-words ${pageLineBorder} ${pagePaperBg}`}>
      <div className={`min-w-0 font-medium ${pageInkText}`} data-collaborator="">
        {name}
      </div>
      <div
        className={`mt-1 flex min-w-0 flex-wrap items-center gap-x-2 gap-y-1 text-sm ${pageMutedText}`}
      >
        {detail}
      </div>
    </li>
  );
}

/** How a collaborator worked the issue - `assignee`, `claimed`, `posted`, `answered` - in the
 *  page's words, and when they last did. */
function Worked({
  lastAt,
  reasons,
}: {
  lastAt: string | null;
  reasons: IssueCollaboratorPerson["reasons"];
}): ReactNode {
  const worked = reasons ?? [];
  return (
    <>
      {worked.length === 0 ? null : (
        <span {...pageChrome}>{worked.map((reason) => REASON_WORDS[reason]).join(", ")}</span>
      )}
      {lastAt === null ? null : <Timestamp at={lastAt} />}
    </>
  );
}

/**
 * Everyone who worked the issue, running or not (`GET /issues/{key}/collaborators`): Legion's
 * agents by role, other agent sessions, and people. Each agent is an `AgentName` link to its live
 * conversation, its running state from the agents registry, so a replaced or stopped agent is one
 * click from the conversation it had; a person is their login as plain text, as everywhere.
 *
 * It is drawn in the People section of the issue page's one right-hand panel, behind that
 * section's own disclosure (`details-more.tsx`), where the nine-tab
 * page gave it a tab of its own. Each of its three lists is drawn 20 at a time with Show more
 * under it (`useListPages`), Legion's agents as one list across their roles. What a read answered
 * stays drawn when a later read fails, and no failure is said over it; the failure and its Retry
 * are drawn only while no read has answered.
 */
export function IssueCollaboratorList({
  issueKey,
  owner,
}: {
  issueKey: string;
  /** The control that opens the list, which the focus goes to when the list's Retry goes. */
  owner: RefObject<HTMLElement | null>;
}): ReactNode {
  const query = useQuery(issueCollaboratorsQuery(issueKey));
  // Each list's pages, and the box its entries are drawn in, where a press of its Show more
  // finds the first entry it drew.
  const legionPages = useListPages();
  const legionList = useRef<HTMLElement>(null);
  const otherPages = useListPages<HTMLUListElement>();
  const peoplePages = useListPages<HTMLUListElement>();
  // The Retry the reader pressed goes with its line once the read lands, and the focus with it:
  // the focus goes to the control that opens the list rather than fall to the page, as Unpin in
  // Pinned hands it on (`IssuePins.tsx`). It is never disabled while it reads again, so a read
  // that fails again leaves the focus on it: Chromium drops the focus of a focused button that
  // turns disabled to the page. It reads busy then, and a press while the list is read joins
  // that read rather than ending it and sending another (`retryRead`).
  const retry = useRetryHandOff(() => owner.current);
  // The failure line stands in place of the loading line while the read stands failed
  // (`standsFailed`).
  const failure = standsFailed(query) ? (
    <DetailsErrorLine
      failure={query.errorUpdateCount}
      message={ANSWER_LINES.collaborators_unreadable}
      onRetry={retryRead(query)}
      {...retry}
      retrying={query.isFetching}
    />
  ) : null;
  if (query.data === undefined) {
    return (
      failure ?? (
        <p className={`text-sm ${pageQuietText}`} {...pageChrome}>
          {ANSWER_LINES.loading_collaborators}
        </p>
      )
    );
  }
  const collaborators = query.data;
  const sections = collaboratorSections(collaborators);
  const titles = new Map(
    (collaborators.agents ?? []).map((agent) => [agent.session_id, agent.title ?? undefined])
  );
  const unavailable = collaborators.legion_registry === "unavailable";
  const empty =
    (collaborators.agents ?? []).length === 0 && (collaborators.people ?? []).length === 0;
  return (
    <section aria-label={PROPERTY_LABELS.collaborators} className="min-w-0 space-y-6">
      {unavailable ? (
        <p className={`text-sm ${pageMutedText}`} role="status" {...pageChrome}>
          {STATE_LINES.legion_record_unreadable}
        </p>
      ) : null}
      {empty && !unavailable ? (
        <p className={`text-sm ${pageQuietText}`} {...pageChrome}>
          {EMPTY_STATE_LINES.no_collaborators}
        </p>
      ) : null}
      {sections.legion.length === 0 ? null : (
        <section
          aria-label={CONTROL_LABELS.collaborators_legion}
          className="space-y-3"
          ref={legionList}
        >
          <SectionHeading>{CONTROL_LABELS.collaborators_legion}</SectionHeading>
          {groupsDrawn(sections.legion, ({ agents }) => agents, legionPages.drawn).map(
            ({ entries, group: { role } }) => (
              <section aria-label={ROLE_WORDS[role]} className="space-y-2" key={role}>
                <h3 className={`text-xs font-semibold uppercase ${pageQuietText}`} {...pageChrome}>
                  {ROLE_WORDS[role]}
                </h3>
                <ul className="space-y-2">
                  {entries.map((agent) => (
                    <Row
                      detail={
                        <>
                          <span>
                            <span {...pageChrome}>{CONTROL_LABELS.collaborator_started}</span>{" "}
                            <Timestamp at={agent.legion.started_at} />
                          </span>
                          {agent.legion.replaced_by === null ? null : (
                            <span data-replaced-by={agent.legion.replaced_by}>
                              <span {...pageChrome}>{CONTROL_LABELS.collaborator_replaced_by}</span>{" "}
                              <AgentName
                                inline
                                sessionId={agent.legion.replaced_by}
                                storedTitle={titles.get(agent.legion.replaced_by)}
                              />
                            </span>
                          )}
                        </>
                      }
                      key={agent.session_id}
                      name={
                        <AgentName
                          sessionId={agent.session_id}
                          storedTitle={agent.title ?? undefined}
                        />
                      }
                    />
                  ))}
                </ul>
              </section>
            )
          )}
          {legionPages.more(
            sections.legion.reduce((count, { agents }) => count + agents.length, 0),
            (index) => legionList.current?.querySelectorAll("li")[index]
          )}
        </section>
      )}
      {sections.others.length === 0 ? null : (
        <section aria-label={CONTROL_LABELS.collaborators_others} className="space-y-2">
          <SectionHeading>{CONTROL_LABELS.collaborators_others}</SectionHeading>
          <ul className="space-y-2" ref={otherPages.list}>
            {sections.others.slice(0, otherPages.drawn).map((agent) => (
              <Row
                detail={<Worked lastAt={agent.last_at} reasons={agent.reasons} />}
                key={agent.session_id}
                name={
                  <AgentName sessionId={agent.session_id} storedTitle={agent.title ?? undefined} />
                }
              />
            ))}
          </ul>
          {otherPages.more(sections.others.length)}
        </section>
      )}
      {sections.people.length === 0 ? null : (
        <section aria-label={CONTROL_LABELS.collaborators_people} className="space-y-2">
          <SectionHeading>{CONTROL_LABELS.collaborators_people}</SectionHeading>
          <ul className="space-y-2" ref={peoplePages.list}>
            {sections.people.slice(0, peoplePages.drawn).map((person) => (
              <Row
                detail={<Worked lastAt={person.last_at} reasons={person.reasons} />}
                key={person.login}
                name={person.login}
              />
            ))}
          </ul>
          {peoplePages.more(sections.people.length)}
        </section>
      )}
    </section>
  );
}
