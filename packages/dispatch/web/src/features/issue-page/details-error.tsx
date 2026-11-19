import {
  type ReactNode,
  type Ref,
  type RefCallback,
  useCallback,
  useLayoutEffect,
  useRef,
  useState,
} from "react";

import { AGENT_LIST_REFILLING, isAgentListRefilling } from "../../api/client";
import { pageFailedText, pageHeaderControl, pageQuietText } from "../../theme/classes";
import { pageChrome } from "./page-structure";
import { CONTROL_LABELS } from "./page-words";

/** What a row of Details is handed of a read or a write that failed. */
export interface DetailsErrorLineProps {
  /** The failure itself. An agent list that is refilling (`isAgentListRefilling`) is not a
   *  failure to report: the refilling line replaces `message`, as status, not alert. */
  readonly error?: unknown;
  /** Which failure the line says: a write's `SaidFailure.count`, a read's `errorUpdateCount`.
   *  The line stays while its Retry's request is out, so each new failure is drawn afresh by
   *  this, and a screen reader says it again even in the same words. It keys the alert alone, so
   *  a failure that repeats is said again while Retry stays the same button, with its focus. */
  readonly failure?: number;
  readonly message: string;
  /** Omitted for a failure a retry cannot get past; no Retry is offered then. */
  readonly onRetry?: () => void;
  /** Whether the write Retry sent again is still out: Retry takes no press (`aria-disabled`,
   *  with the `refused:` look of `styles.css`), and the caller drops a press made meanwhile, as
   *  for any control whose own write is out. */
  readonly refused?: boolean;
  /** Takes the Retry, so the row can hand its focus on when the line goes (`useFocusHandOff`). */
  readonly retryRef?: Ref<HTMLButtonElement>;
  /** Whether the read Retry sent again is still out: Retry reads busy (`aria-busy`, and a
   *  quieter look) and still takes a press, which the caller joins to the read that is out. */
  readonly retrying?: boolean;
}

/**
 * A read or a write of a row of Details that failed, said in that row with its Retry: the line
 * the shared `QueryError` draws, in the page's colours and with the page's word for its control.
 * An agent list that is refilling is said as the line every view says it in
 * (`AgentListRefilling`'s test id), as status rather than alert.
 *
 * Retry is never `disabled`, since Chromium moves the focus of a control that turns disabled to
 * the page's body, which would lose a reader who pressed it by keyboard. While the request it
 * sent again is still out, Retry reads busy if that request is a read (`retrying`) and takes no
 * press if it is a write (`refused`), and keeps the focus either way.
 */
export function DetailsErrorLine({
  error,
  failure,
  message,
  onRetry,
  refused = false,
  retryRef,
  retrying = false,
}: DetailsErrorLineProps): ReactNode {
  return (
    <div className="flex flex-wrap items-center gap-3 text-sm">
      {isAgentListRefilling(error) ? (
        <p className={pageQuietText} data-testid="agent-list-refilling" role="status">
          {AGENT_LIST_REFILLING}
        </p>
      ) : (
        <p className={pageFailedText} key={failure} role="alert">
          {message}
        </p>
      )}
      {onRetry === undefined ? null : (
        <button
          aria-busy={retrying || undefined}
          aria-disabled={refused || undefined}
          className={`${pageHeaderControl} aria-busy:opacity-50 refused:cursor-not-allowed refused:opacity-50`}
          onClick={onRetry}
          ref={retryRef}
          type="button"
          {...pageChrome}
        >
          {CONTROL_LABELS.retry}
        </button>
      )}
    </div>
  );
}

/** The Retry of a read of Details that failed: it sends the read again, and a press while that
 *  read is still out joins it (`cancelRefetch: false`) rather than ending it and sending another,
 *  as the app's other refetches do (`DispatchQueryClient` in `api/query-client.ts`, `drive` in
 *  `api/query-refresh.ts`). The query itself decides that as the press lands. A gate on the
 *  render's `isFetching` would not hold: a second press can arrive before React draws the
 *  first one's fetch (`hooks/useSubmitGuard.ts`). Retry reads busy while the read is out (the
 *  query's `isFetching`) but stays enabled, because Chromium moves the focus of a control that
 *  turns disabled to the page's body, which would lose a reader who pressed Retry by keyboard. A
 *  read that fails again is drawn afresh (`errorUpdateCount`), so a screen reader says it again
 *  even in the same words. */
export function retryRead(read: {
  readonly refetch: (options: { cancelRefetch: boolean }) => Promise<unknown>;
}): () => void {
  return () => void read.refetch({ cancelRefetch: false });
}

/** Whether a read of Details is said as failed, with its Retry: while it stands failed, and while
 *  it is read again after it failed holding no answer. A fetch of a read that holds no answer
 *  clears its error and puts it back to pending (`fetchState` in `@tanstack/query-core`), which
 *  would take the line, and its Retry with a reader's focus, away while the read Retry sent is
 *  out. Such a read has answered nothing since its last failure, so a failure it counts
 *  (`errorUpdateCount`) is one it still stands in; `failureCount` would not tell, since the
 *  fetch sets it back to 0 as it starts. */
export function readFailed(read: {
  readonly errorUpdateCount: number;
  readonly isError: boolean;
  readonly isPending: boolean;
}): boolean {
  return read.isError || (read.isPending && read.errorUpdateCount > 0);
}

/** What a row reads of a write whose failure it says: `useMutation`'s state of its last press. */
export interface RowWrite<Variables> {
  readonly error: unknown;
  readonly isError: boolean;
  readonly isPending: boolean;
  readonly submittedAt: number;
  readonly variables: Variables | undefined;
}

/** A failure of a write that a row of Details says, with the variables of the press that failed. */
export interface SaidFailure<Variables> {
  /** One more for each failure said while the line stayed (`DetailsErrorLineProps.failure`). */
  readonly count: number;
  readonly error: unknown;
  readonly submittedAt: number;
  readonly variables: Variables | undefined;
}

/**
 * The failure a row of Details says of `write`: its last press's while that press stands
 * failed, and the same one while the write is out again with the same variables, as a press of
 * the line's Retry sends it; `null` once the write lands, or while a write of other variables is
 * out. A write that is out reports no error, so a line drawn from the error alone went at the
 * press of its Retry, and the focus of a reader who pressed Retry by keyboard went with it to the
 * page's body, there to stay when the write failed again and the line came back.
 */
export function useSaidFailure<Variables>(
  write: RowWrite<Variables>
): SaidFailure<Variables> | null {
  const [said, setSaid] = useState<SaidFailure<Variables> | null>(null);
  let next = said;
  if (write.isError) {
    if (said === null || said.error !== write.error || said.submittedAt !== write.submittedAt) {
      next = {
        count: (said?.count ?? 0) + 1,
        error: write.error,
        submittedAt: write.submittedAt,
        variables: write.variables,
      };
    }
  } else if (
    !write.isPending ||
    said === null ||
    // Variables built afresh at each press, as a form's are, are the same press by their JSON.
    JSON.stringify(said.variables) !== JSON.stringify(write.variables)
  ) {
    next = null;
  }
  if (next !== said) setSaid(next);
  return next;
}

/**
 * The ref of an element that may leave the page with the focus inside it. As React takes the
 * element off the page, the ref notes whether the focus is inside it; if it is, the hook's next
 * layout effect, which runs in each commit that renders the component calling the hook, focuses
 * what `to` returns, without scrolling. The commit that takes the element away need not render
 * that component - where another component draws the element, as the repository's control draws
 * the lines the Repository row hands it - so the ref then asks for a render of it, whose layout
 * effect hands the focus on. `to` is called only then, after that commit has changed the page,
 * so it can return an element the commit drew. Where it returns none, or an element that takes
 * no focus, the focus stays where the browser put it; a focus outside the element as it leaves
 * is not moved.
 */
export function useFocusHandOff(
  to: () => HTMLElement | null | undefined
): RefCallback<HTMLElement> {
  const left = useRef(false);
  const [, render] = useState(0);
  useLayoutEffect(() => {
    if (!left.current) return;
    left.current = false;
    to()?.focus({ preventScroll: true });
  });
  return useCallback((element: HTMLElement | null) => {
    if (element === null) return undefined;
    // React runs this before it takes the element out of the page, so the focus is still where
    // the reader left it.
    return () => {
      if (!element.contains(document.activeElement)) return;
      left.current = true;
      render((renders) => renders + 1);
    };
  }, []);
}
