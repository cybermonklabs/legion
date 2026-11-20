import { type UseQueryResult, useQuery } from "@tanstack/react-query";
import type { ReactNode } from "react";
import { Link } from "react-router-dom";

import { inboxComparisonQuery } from "../../api/queries";
import type { InboxComparisonResponse } from "../../api/types";
import { LoadingSkeleton } from "../../components/LoadingSkeleton";
import {
  borderDefault,
  card,
  dangerText,
  linkHoverText,
  linkText,
  successText,
  textMutedOnCanvas,
  textPrimaryOnCanvas,
} from "../../theme/classes";
import { Timestamp } from "../refs/Timestamp";
import { useDocumentTitle } from "../shell/useDocumentTitle";

/** Each shape of the corpus the gate needs, as a person reads it in the sentence that names a
 *  shape the corpus does not have. The keys are the shapes `envoy-dispatch inbox-compare` counts
 *  (`internal/dispatch/inboxcompare`), which the route answers by name. */
const SHAPE_LABELS: Record<string, string> = {
  agent_row: "a reply from a session to a message that a person sent",
  ask_thread: "a reply in an ask that a person wrote in",
  comment_thread: "a reply in a comment thread that a person wrote in",
  conversation_reply: "a reply from a session in a conversation that a person started",
  inbox_ask: "an open ask that waits on a person",
  message_thread: "a reply in an issue message thread that a person wrote in",
  person_with_none: "a person with no rows",
  person_with_rows: "a person with rows",
  reply_chain_at_bound: "a comment reply chain at the depth where the delivery walk stops",
};

/** The count of `noun` as one phrase: "1 difference", "2 differences". */
function counted(count: number, noun: string): string {
  return `${count} ${noun}${count === 1 ? "" : "s"}`;
}

/** `/inbox/comparison`: the gate of the one read (dispatch://INFRA-1152, R8). It states the newest
 *  run of the comparison - how many differences it found, how many people it checked, how many of
 *  them it did not reach - and, in one sentence, whether the gate is met. Each figure comes from
 *  `GET /api/v1/inbox/comparison`, which reads the run's stored row and answers nobody's rows.
 *  The branch that adds this page deletes it with the comparison, before a release. */
export function ComparisonPage(): ReactNode {
  useDocumentTitle("Inbox comparison · Dispatch");
  const comparison = useQuery(inboxComparisonQuery());

  return (
    <section aria-label="Inbox comparison">
      <header className={`mb-5 border-b pb-4 ${borderDefault}`}>
        <Link className={`text-sm ${linkText} ${linkHoverText}`} to="/">
          ← Inbox
        </Link>
        <h1 className={`mt-1 text-[22px] font-semibold tracking-tight ${textPrimaryOnCanvas}`}>
          Inbox comparison
        </h1>
        <p className={`mt-1 text-sm ${textMutedOnCanvas}`}>
          The comparison reads the three old Inbox reads and the one new read for each person. It
          counts each row that an old read shows and the new read does not show.
        </p>
      </header>
      <ComparisonBody comparison={comparison} />
    </section>
  );
}

function ComparisonBody({
  comparison,
}: {
  comparison: UseQueryResult<InboxComparisonResponse>;
}): ReactNode {
  if (comparison.isPending) return <LoadingSkeleton label="Loading the comparison" />;
  if (comparison.isError) {
    return <p className={dangerText}>Could not load the comparison: {comparison.error.message}</p>;
  }
  const run = comparison.data.run;
  if (run === null || run === undefined) {
    return (
      <p className={`text-base font-medium ${dangerText}`} data-testid="comparison-gate">
        The gate is not met, because no comparison ran.
      </p>
    );
  }
  const reasons = [
    ...(run.difference_count > 0
      ? [`The comparison found ${counted(run.difference_count, "difference")}.`]
      : []),
    ...(run.people_uncovered > 0
      ? [`The comparison did not check ${counted(run.people_uncovered, "person")}.`]
      : []),
    ...(run.missing_shapes ?? []).map(
      (shape) => `The corpus has no ${SHAPE_LABELS[shape] ?? shape}.`
    ),
  ];
  return (
    <div className="space-y-4">
      <p
        className={`text-base font-medium ${run.met ? successText : dangerText}`}
        data-testid="comparison-gate"
      >
        {run.met ? "The gate is met." : "The gate is not met."}
      </p>
      {reasons.length === 0 ? null : (
        <ul aria-label="Why the gate is not met" className="list-disc space-y-1 pl-5 text-sm">
          {reasons.map((reason) => (
            <li key={reason}>{reason}</li>
          ))}
        </ul>
      )}
      <dl
        aria-label="Newest comparison run"
        className={`grid max-w-md grid-cols-[auto_1fr] gap-x-6 gap-y-2 rounded-xl border p-4 text-sm ${card} ${borderDefault}`}
      >
        <dt className={textMutedOnCanvas}>Differences</dt>
        <dd data-testid="comparison-differences">{run.difference_count}</dd>
        <dt className={textMutedOnCanvas}>People checked</dt>
        <dd data-testid="comparison-people-checked">{run.people_checked}</dd>
        <dt className={textMutedOnCanvas}>People uncovered</dt>
        <dd data-testid="comparison-people-uncovered">{run.people_uncovered}</dd>
        <dt className={textMutedOnCanvas}>Ran</dt>
        <dd>
          <Timestamp at={run.ran_at} />
        </dd>
      </dl>
    </div>
  );
}
