import type { DispatchEvent, EventType } from "../types";
import { type Answered, answered } from "./answered";
import type { Cache } from "./cache";

const knownEventTypes: Record<EventType, true> = {
  "project.created": true,
  "project.updated": true,
  "settings.repo_project.updated": true,
  "settings.project_default_assignee.updated": true,
  "settings.architecture_source.updated": true,
  "architecture.synced": true,
  "architecture.sync_failed": true,
  "user_state.updated": true,
  "user_agent_state.updated": true,
  "user_thread_state.updated": true,
  "decision.kept": true,
  "issue.created": true,
  "issue.updated": true,
  "issue.closed": true,
  "issue.claimed": true,
  "issue.released": true,
  "issue.deployed": true,
  "handoff.recorded": true,
  "artifact.created": true,
  "artifact.renamed": true,
  "artifact.removed": true,
  "artifact.version": true,
  "artifact.human_edited": true,
  "artifact.approved": true,
  "artifact.changes_requested": true,
  "ask.opened": true,
  "ask.anchor_refreshed": true,
  "ask.edited": true,
  "ask.handed_back": true,
  "ask.asked_of": true,
  "ask.answered": true,
  "ask.resolved": true,
  "ask.follower_added": true,
  "ask.follower_removed": true,
  "block.repaired": true,
  "block.invalid": true,
  "result.recorded": true,
  "result.retracted": true,
  "issue.pull_request_attribution_changed": true,
  "comment.created": true,
  "comment.anchor_refreshed": true,
  "comment.delivery": true,
  "comment.answered": true,
  "comment.resolved": true,
  "comment.reopened": true,
  "comment.edited": true,
  "suggestion.accepted": true,
  "suggestion.rejected": true,
  "message.created": true,
  "message.delivery": true,
  "message.accepted": true,
  "message.answered": true,
  "child.status": true,
  "child.added": true,
  "child.removed": true,
  "subscription.remove_requested": true,
  "subscription.removed": true,
};

/**
 * The event feed's frame as the page reads it (`live-socket.ts` hands it over as the JSON Dispatch
 * sent): an event of a type this page knows, minted as Dispatch's answer (`answered.ts`), or
 * `undefined` for one it cannot parse, which the feed answers by refreshing everything.
 * `DispatchEvent`, not the route table's `Event`: the log this page keeps and every reader of it
 * (`activity-model`, `event-refresh`) narrows on the type to read the payload, which the route
 * table carries as `unknown`.
 */
export function decodeEvent(raw: unknown): Answered<DispatchEvent> | undefined {
  const type = typeof raw === "object" && raw !== null && "type" in raw ? raw.type : undefined;
  // `Object.hasOwn`, not `in`: `knownEventTypes` inherits `Object.prototype`, so `in` reads an
  // event named `constructor` or `toString` as a known type and reads it as an event.
  if (typeof type !== "string" || !Object.hasOwn(knownEventTypes, type)) {
    return undefined;
  }
  return answered(raw as DispatchEvent);
}

export interface EventPages {
  pageParams: unknown[];
  pages: DispatchEvent[][];
}

/**
 * Reconciles a fetched log with the cached one. The stream prepends each event the moment it is
 * published, while the refetch its burst starts reads the log a little later - or, under load, a
 * little *earlier*: a response whose head is older than the cached head predates turns the
 * stream already delivered, and replacing the page with it would make those turns vanish until
 * the next refetch. The response is authoritative for the range it covers; the cached events
 * above its head stay in front of it. A response at or beyond the cached head replaces the page.
 */
export function mergeEventPages(current: EventPages | undefined, incoming: EventPages): EventPages {
  const cached = current?.pages[0];
  const fetched = incoming.pages[0];
  if (cached === undefined || fetched === undefined) {
    return incoming;
  }
  const fetchedHead = fetched[0]?.seq ?? 0;
  const newer = cached.filter((event) => event.seq > fetchedHead);
  if (newer.length === 0) {
    return incoming;
  }
  return { ...incoming, pages: [[...newer, ...fetched], ...incoming.pages.slice(1)] };
}

/** Puts `event`, as Dispatch sent it, at the front of its issue's log, where the log is held and
 *  its head is older: Dispatch's event over Dispatch's pages. */
export function prependEventToLog(cache: Cache, event: Answered<DispatchEvent>): void {
  if (event.issue_key === null) {
    return;
  }
  cache.setDispatched(
    ["events", event.issue_key],
    event,
    (current: EventPages | undefined, streamed) => {
      if (current === undefined) {
        return current;
      }
      const first = current.pages[0] ?? [];
      if (streamed.seq <= (first[0]?.seq ?? 0)) {
        return current;
      }
      return {
        ...current,
        pages: [[streamed, ...first], ...current.pages.slice(1)],
      };
    }
  );
}
