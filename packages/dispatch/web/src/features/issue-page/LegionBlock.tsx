import { useQuery, useQueryClient } from "@tanstack/react-query";
import { Fragment, type ReactNode, useEffect, useRef } from "react";
import { Link } from "react-router-dom";

import { api } from "../../api/client";
import { legionIssueQuery } from "../../api/legion-issue";
import { issueStepsQuery } from "../../api/queries";
import { refreshQueries } from "../../api/query-refresh";
import { pageFailedText, pageInkText, pageMutedText, pageQuietText } from "../../theme/classes";
import { standsFailed } from "../refs/reference-target";
import { Time } from "../steps/StepsTab";
import { DetailsErrorLine, retryRead, useRetryHandOff } from "./details-error";
import { type LegionPlacePart, legionBlockOf, waitReadsDocuments } from "./legion-block";
import { authorText, pageChrome } from "./page-structure";
import { ANSWER_LINES, CONTROL_LABELS } from "./page-words";
import { ROW_LINK } from "./row-references";

/** The block's words (`page-words.ts`). */
const LEGION = CONTROL_LABELS.legion;

/** What stands between two parts of one line of the block. */
const BETWEEN = " · ";

/** An instant of the daemon's state, as the runs of Details write theirs (`Time` of the Steps
 *  tab): the date and the time of day, text the page did not write. */
function LegionTime({ at }: { at: string }): ReactNode {
  return (
    <span {...authorText}>
      <Time at={at} />
    </span>
  );
}

/** One part of the block's place and wait, in its words; each number, key and member of the wait
 *  is text the page did not write. "Waits on a person" links only where the block built a link
 *  from ids it checked (`legion-block.ts`), and the wait's members follow it as text. */
function PlacePart({ part }: { part: LegionPlacePart }): ReactNode {
  switch (part.kind) {
    case "at_work":
      return <span {...pageChrome}>{LEGION.at_work}</span>;
    case "line":
      return (
        <span>
          <span {...authorText}>{part.place}</span>
          <span {...pageChrome}>{`${part.ending} ${LEGION.place_of} `}</span>
          <span {...authorText}>{part.length}</span>
          <span {...pageChrome}> {LEGION.in_line}</span>
        </span>
      );
    case "waits_on_person":
      return (
        <span>
          {part.href === null ? (
            <span {...pageChrome}>{LEGION.waits_on_person}</span>
          ) : (
            <Link
              className={`dispatch-inline-link ${ROW_LINK} ${pageInkText}`}
              to={part.href}
              {...pageChrome}
            >
              {LEGION.waits_on_person}
            </Link>
          )}
          {part.members.length === 0 ? null : (
            <span className={pageQuietText} data-legion-wait="">
              {part.members.map((member, index) => (
                // biome-ignore lint/suspicious/noArrayIndexKey: two members can hold one text, so a member's place is its identity
                <Fragment key={index}>
                  {index === 0 ? " " : BETWEEN}
                  <span {...authorText}>{member}</span>
                </Fragment>
              ))}
            </span>
          )}
        </span>
      );
    case "past_review":
      return <span {...pageChrome}>{LEGION.past_review}</span>;
    case "not_started":
      return <span {...pageChrome}>{LEGION.not_started}</span>;
    case "waits_for":
      return (
        <span>
          <span {...pageChrome}>{LEGION.waits_for} </span>
          {part.keys.map((key, index) => (
            // biome-ignore lint/suspicious/noArrayIndexKey: the daemon can name a key twice, so a key's place is its identity
            <Fragment key={index}>
              {index === 0 ? null : ", "}
              <span {...authorText}>{key}</span>
            </Fragment>
          ))}
        </span>
      );
  }
}

/**
 * The Legion block of Details (dispatch://INFRA-4870/spec#b-r-block): where Legion stands on the
 * issue `issueKey`, from its Legion read (`legionIssueQuery`), the live view `legion_issue` of
 * `/ws/live`, which this block observes only while Details draws it, that is while Details is
 * open, so Details shut holds no subscription of it. It draws, in the words of the parent's
 * "Words for the state" (`legion-block.ts`), the phase and since when, every role with a claim and
 * its claim's word, the place and the wait, and the time of the daemon's newest read, or that it
 * failed, with no error text. It holds no list of runs or handoffs and has no fold control.
 *
 * It draws nothing while its first answer is out, and nothing for a project that names no daemon,
 * an issue the daemon does not record, and a `done` issue. A read of its own that failed draws its
 * heading, the panel's error line and Retry, and the block it held, if any, stays above them.
 * Retry subscribes the view again, and once that read lands with the line, hands the focus it held
 * to the heading.
 *
 * The link of a gate's wait needs the document's slug, which the issue's list of documents holds
 * by id: it reads that list under the key the Documents tab reads it under (`["artifacts", key]`),
 * and only while the wait is a gate wait that is not answered and whose ids pass their checks
 * (the plan's D8), so no other state costs a request for it.
 */
export function LegionBlock({ issueKey }: { issueKey: string }): ReactNode {
  const query = useQuery(legionIssueQuery(issueKey));
  const documents = useQuery({
    enabled: waitReadsDocuments(query.data),
    queryFn: () => api.listArtifacts(issueKey),
    queryKey: ["artifacts", issueKey],
  });
  const heading = useRef<HTMLHeadingElement>(null);
  const retry = useRetryHandOff(() => heading.current);
  const block =
    query.data === undefined ? null : legionBlockOf(query.data, issueKey, documents.data);
  // A first read that failed stands failed while Retry reads again; a read that answered and then
  // failed live keeps its answer, which stays drawn above the line.
  const failed = standsFailed(query) || query.isError;
  if (block === null && !failed) return null;
  return (
    <section
      aria-label={LEGION.heading}
      className="flex min-w-0 flex-col gap-1"
      data-testid="legion-block"
    >
      <h2
        className={`text-sm font-semibold outline-none ${pageInkText}`}
        ref={heading}
        tabIndex={-1}
        {...pageChrome}
      >
        {LEGION.heading}
      </h2>
      {block === null ? null : (
        <>
          <p className={`text-sm ${pageInkText}`} data-legion-line="phase">
            <span className="font-semibold" {...pageChrome}>
              {block.phase}
            </span>{" "}
            <span className={pageMutedText} {...pageChrome}>
              {block.since.before ? LEGION.since_before : LEGION.since}
            </span>{" "}
            <LegionTime at={block.since.at} />
          </p>
          {block.roles.length === 0 ? null : (
            <ul className={`flex min-w-0 flex-col text-sm ${pageInkText}`}>
              {block.roles.map((role) => (
                // The server lists each role once, and a role the page has a word for is never
                // its own text, so a role's name is its identity.
                <li className="min-w-0 [overflow-wrap:anywhere]" key={role.name}>
                  <span {...(role.own ? authorText : pageChrome)}>{role.name}</span>{" "}
                  <span className={pageMutedText} {...pageChrome}>
                    {role.state}
                  </span>
                </li>
              ))}
            </ul>
          )}
          {block.place.length === 0 ? null : (
            <p
              className={`min-w-0 text-sm [overflow-wrap:anywhere] ${pageInkText}`}
              data-legion-line="place"
            >
              {block.place.map((part, index) => (
                <Fragment key={part.kind}>
                  {index === 0 ? null : BETWEEN}
                  <PlacePart part={part} />
                </Fragment>
              ))}
            </p>
          )}
          {block.read === null ? null : (
            <p
              className={`text-xs ${block.read.failed ? pageFailedText : pageQuietText}`}
              data-legion-line="read"
            >
              <span {...pageChrome}>
                {block.read.failed ? LEGION.read_failed.before : LEGION.read_at}
              </span>{" "}
              <LegionTime at={block.read.at} />
              {block.read.failed ? <span {...pageChrome}> {LEGION.read_failed.after}</span> : null}
            </p>
          )}
        </>
      )}
      {failed ? (
        <DetailsErrorLine
          error={query.error}
          failure={query.errorUpdateCount}
          message={ANSWER_LINES.legion_unreadable}
          onRetry={retryRead(query)}
          {...retry}
          retrying={query.isFetching}
        />
      ) : null}
    </section>
  );
}

/**
 * Reads the issue's runs again when the runs' revision of its Legion read moves (the plan's D16):
 * a write of a run appends no issue event (`api/steps.go`), so its live view's `runs_revision` is
 * how an open page learns of one. It observes the one query the block observes, so the two hold
 * one subscription, and is drawn only while Details is open, for a project that names no daemon
 * as well, whose answer still carries the revision. The first revision it sees starts no read:
 * Details' opening reads the runs on its own.
 */
export function LegionRunsFollower({ issueKey }: { issueKey: string }): null {
  const queryClient = useQueryClient();
  const revision = useQuery({
    ...legionIssueQuery(issueKey),
    select: (answer) => answer.runs_revision,
  }).data;
  const seen = useRef<{ readonly issue: string; readonly revision: number } | undefined>(undefined);
  useEffect(() => {
    if (revision === undefined) return;
    const last = seen.current;
    seen.current = { issue: issueKey, revision };
    if (last === undefined || last.issue !== issueKey || last.revision === revision) return;
    refreshQueries(queryClient, [{ exact: true, queryKey: issueStepsQuery(issueKey).queryKey }]);
  }, [issueKey, queryClient, revision]);
  return null;
}
