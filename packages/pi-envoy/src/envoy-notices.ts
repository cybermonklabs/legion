import { noticeKind, noticeText } from "@legion/envoy-client/notice-text";

/**
 * The notices the Envoy extension writes into its own session, beside those Envoy's client builds
 * for every host (`@legion/envoy-client/consumer-delivery`, `delivery-ledger`). Each carries
 * `LOCAL_ENVOY_NOTICE`, since it is the session's own doing and no event from outside.
 */

/**
 * The session-id notice (`tellIdChanged` in `extensions/envoy.ts`): a fork or a branch carried
 * this conversation over from the session `previous_session_id` to `session_id`. The step its
 * reader takes is in `envoy_whoami`'s description (`@legion/envoy-client/tool-contract`), which
 * its kind names (`@legion/envoy-client/notice-text`).
 */
export const SESSION_ID_CHANGED_NOTICE = noticeKind("envoy", "session id changed (envoy_whoami)", [
  "previous_session_id",
  "session_id",
]);

/** The session-id notice for a conversation carried over from `previous` to `current`. */
export function sessionIdChangedText(previous: string, current: string): string {
  return noticeText(SESSION_ID_CHANGED_NOTICE, {
    previous_session_id: previous,
    session_id: current,
  });
}

/**
 * The wake (`CardWake`): `unread` Envoy cards above it arrived with no turn to read them, and it
 * starts that turn. The step its reader takes is in `envoy_inbox`'s description
 * (`@legion/envoy-client/tool-contract`), which its kind names.
 */
export const UNREAD_MESSAGES_NOTICE = noticeKind("envoy", "unread messages (envoy_inbox)", [
  "unread",
]);

/** The wake for `unread` cards. */
export function unreadMessagesText(unread: number): string {
  return noticeText(UNREAD_MESSAGES_NOTICE, { unread });
}
