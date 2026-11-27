import { type ExpectsReply, ROLE_TOPIC_PREFIX } from "@legion/contracts";

// The answers of the fake Envoy listener's publish route (`POST /v1/messages/publish` in
// e2e/fake-envoy.ts) to a role publish. The route reads the role's holder from its own fixture and
// hands it here as that file's `asAnswered` gives it, so this module imports nothing of that file,
// which starts the listener when it is loaded.

/** A publish through the listener's publish route (`POST /v1/messages/publish`), as Dispatch's
 *  listener client writes one for a role it keeps a message for (`PublishToRole`): the role's
 *  topic, the send's own fields, `keep`, which a role publish leaves unset or sets true to have
 *  the listener keep the message for the role's next holder, and `dispatch_post`, the comment or
 *  message the publish delivers. Dispatch's publish names no `source_session`, and the listener
 *  keeps such a publish for a role with no live holder only for the post it names. */
export interface PublishedMessage {
  readonly topic: string;
  readonly source: string;
  readonly source_session?: string;
  readonly message: string;
  readonly payload: string;
  readonly idempotency_key: string;
  readonly urgency?: string;
  readonly expects_reply?: ExpectsReply;
  readonly keep?: boolean;
  readonly dispatch_post?: DispatchPost;
}

/** The Dispatch post a role publish delivers (`listenerwire.DispatchPost`), as the caller wrote
 *  it: either field may be missing or wrong, which `dispatchPostRefusal` refuses. */
interface DispatchPost {
  readonly kind?: string;
  readonly id?: string;
}

/** A role's holder that is not live, as the publish route's answers name it: its session, the
 *  state its own record puts it in, and its last heartbeat, which `asAnswered` in
 *  e2e/fake-envoy.ts gives a seeded session. */
interface AnsweredHolder {
  readonly session_id: string;
  readonly state: string;
  readonly last_seen?: number;
}

/** The publish route's request field naming the Dispatch post a publish delivers, as the real
 *  route's refusals name it (`dispatchPostField` in packages/envoy/cmd/listener/api.go). */
const DISPATCH_POST_FIELD = "dispatch_post";

/** A UUID in its canonical lower-case form, the one Dispatch's store writes and the only id of a
 *  post the real listener takes (`bus.ValidDispatchPost`). */
const CANONICAL_UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/;

/** The end of the error of the real listener's 404 to a role publish that names no source
 *  session and delivers no Dispatch post, to a role with no live holder
 *  (`noSourceSessionNotKept` in packages/envoy/cmd/listener/confirmed.go). */
const NO_SOURCE_SESSION_NOT_KEPT =
  "; nothing was kept, because the publish names no source_session: " +
  "a kept copy that no session took within its hour would expire with no notice to its sender";

/** How long the real listener keeps a role message no live session holds, for the role's next
 *  holder (`roleKeepWindow` in packages/envoy/cmd/listener/role_keep.go): the end of that hour is
 *  the `kept_until` a kept publish is answered with. */
const ROLE_KEEP_WINDOW_MS = 3_600_000;

/** The real listener's refusal of a publish carrying a Dispatch post the route does not take
 *  (`refuseDispatchPost` in packages/envoy/cmd/listener/api.go), answered before anything else
 *  of the publish is read; undefined for a publish that carries no post or one the route takes.
 *  It is the 400 naming `dispatch_post` for a post that is not one Dispatch makes for a role's
 *  first holder: on a topic that is not a role's, from a source other than dispatch, with a
 *  source_session named, or naming no comment or message by its canonical lower-case UUID. The
 *  real route takes the post from Dispatch's own principal alone and answers 403 naming the field
 *  to any other caller; this listener reads no caller's credential on any route, a send's
 *  included, so every caller is Dispatch here and that 403 is not modelled. */
export function dispatchPostRefusal(published: PublishedMessage): Response | undefined {
  const post = published.dispatch_post;
  if (post === undefined) return undefined;
  let message: string;
  if (!published.topic.startsWith(ROLE_TOPIC_PREFIX)) {
    message = `${DISPATCH_POST_FIELD} is taken only on a publish to a role's topic (${ROLE_TOPIC_PREFIX}<role>)`;
  } else if (published.source !== "dispatch") {
    message = `${DISPATCH_POST_FIELD} is taken only on a publish whose source is dispatch`;
  } else if (published.source_session) {
    message = `${DISPATCH_POST_FIELD} is taken only on a publish that names no source_session`;
  } else if (
    (post.kind !== "comment" && post.kind !== "message") ||
    !CANONICAL_UUID.test(post.id ?? "")
  ) {
    message = `${DISPATCH_POST_FIELD} names no Dispatch post: its kind must be comment or message and its id a UUID in lower case, not kind ${JSON.stringify(post.kind ?? "")} and id ${JSON.stringify(post.id ?? "")}`;
  } else {
    return undefined;
  }
  return Response.json({ error: message, expected: [DISPATCH_POST_FIELD] }, { status: 400 });
}

/** The real listener's 404 to a role publish whose keep would tell nobody, to a role with no live
 *  holder (`requestRecord.refuseRoleNoSourceSession`, `roleSend.keepTellsNobody` in
 *  packages/envoy/cmd/listener): the publish names no source session and delivers no Dispatch
 *  post, so a copy kept for the role would expire with no word to anyone, and nothing is kept. It
 *  is the role's no-holder refusal (`roleHolderRefusal`) - unclaimed, or holder_lapsed naming the
 *  holder whose claim stands - with `NO_SOURCE_SESSION_NOT_KEPT` ending its error. This listener
 *  models no claim revisions, so the lapsed answer carries none. */
export function noSourceSessionRefusal(role: string, holder: AnsweredHolder | undefined): Response {
  if (holder === undefined) {
    return Response.json(
      {
        claim_revision: 0,
        error: `no holder for role ${role}${NO_SOURCE_SESSION_NOT_KEPT}`,
        reason: "unclaimed",
      },
      { status: 404 }
    );
  }
  return Response.json(
    {
      claim_released: false,
      error: `role ${role} holder ${holder.session_id} lapsed; claim retained (last seen ${holder.last_seen})${NO_SOURCE_SESSION_NOT_KEPT}`,
      holder: holder.session_id,
      holder_state: holder.state,
      last_seen: holder.last_seen,
      reason: "holder_lapsed",
    },
    { status: 404 }
  );
}

/** The real listener's 202 to a role publish it keeps on the role's kept lane for the role's next
 *  holder (`roleSend.kept` in packages/envoy/cmd/listener/confirmed.go): `kept_reason` unclaimed
 *  for a role nobody holds, or holder_lapsed beside the holder whose claim stands, and
 *  `kept_until` the end of the copy's hour. `eventID` is the id this listener gives the publish. */
export function keptAnswer(
  published: PublishedMessage,
  eventID: string,
  holder: AnsweredHolder | undefined
): Response {
  return Response.json(
    {
      event_id: eventID,
      kept: true,
      kept_reason: holder === undefined ? "unclaimed" : "holder_lapsed",
      kept_until: Date.now() + ROLE_KEEP_WINDOW_MS,
      topic: published.topic,
      ...(holder === undefined ? {} : { holder: holder.session_id, holder_state: holder.state }),
    },
    { status: 202 }
  );
}
