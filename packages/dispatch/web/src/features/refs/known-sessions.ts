import { KNOWN_SESSIONS_MAX, referenceTargetFits } from "@legion/contracts";
import { queryOptions } from "@tanstack/react-query";

import { api } from "../../api/client";

/** What Dispatch answered about one session id: whether it holds any record of it, and the
 *  title its registry gives it when it lists the session. */
export interface KnownSessionAnswer {
  readonly known: boolean;
  readonly title: string | undefined;
}

interface Waiting {
  readonly resolve: (answer: KnownSessionAnswer) => void;
  readonly reject: (error: unknown) => void;
}

/** The ids asked for since the last flush, each with every read waiting on it. */
let batch: Map<string, Waiting[]> | undefined;

/**
 * Asks Dispatch which of the session ids read since the last task are ones it holds a record of,
 * in one request per `KNOWN_SESSIONS_MAX` of them, each id in an `id` parameter of its own. Every
 * reference a page draws mounts in one commit, so the reads of every unlisted session it names
 * start in one task and are answered by one request (`GET /api/v1/agents/known`), whatever their
 * number: a page naming many ended sessions costs the read budget one read, not one each
 * (`e2e/refetch-budget.e2e.ts`).
 */
function flush(): void {
  const taken = batch;
  batch = undefined;
  if (taken === undefined) {
    return;
  }
  const ids = [...taken.keys()];
  for (let at = 0; at < ids.length; at += KNOWN_SESSIONS_MAX) {
    const chunk = ids.slice(at, at + KNOWN_SESSIONS_MAX);
    api.getKnownSessions(chunk).then(
      (answer) => {
        const found = new Map(
          (answer.sessions ?? []).map((session) => [session.session_id, session])
        );
        for (const id of chunk) {
          const session = found.get(id);
          const reply = { known: session !== undefined, title: session?.title ?? undefined };
          for (const waiting of taken.get(id) ?? []) waiting.resolve(reply);
        }
      },
      (error: unknown) => {
        for (const id of chunk) {
          for (const waiting of taken.get(id) ?? []) waiting.reject(error);
        }
      }
    );
  }
}

function readKnownSession(session: string): Promise<KnownSessionAnswer> {
  // The route refuses an id longer than a `dispatch://agent/<session id>` reference's target may
  // be, since no reference in text names one, and that refusal would fail every read of its
  // batch. So such an id is answered unknown here and never sent.
  if (!referenceTargetFits(session)) {
    return Promise.resolve({ known: false, title: undefined });
  }
  if (batch === undefined) {
    batch = new Map();
    setTimeout(flush, 0);
  }
  const { promise, reject, resolve } = Promise.withResolvers<KnownSessionAnswer>();
  const list = batch.get(session);
  if (list === undefined) {
    batch.set(session, [{ reject, resolve }]);
  } else {
    list.push({ reject, resolve });
  }
  return promise;
}

/** Whether Dispatch holds any record of `session`, read in the page's one batched request. A
 *  session that held a role and ended keeps its records, so it is known; an id no agent ever held
 *  is not. Read once per id per page: a session's records do not go away. */
export const knownSessionQuery = (session: string | undefined) =>
  queryOptions({
    queryKey: ["known-session", session],
    queryFn: () => readKnownSession(session ?? ""),
    staleTime: Number.POSITIVE_INFINITY,
  });
