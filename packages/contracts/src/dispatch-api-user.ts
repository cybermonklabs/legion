import { z } from "zod";
import type { WhoamiAgent, WhoamiUser } from "./dispatch-api.generated";

/**
 * The signed-in viewer: who Dispatch resolves a request to, and the per-viewer state the
 * dashboard keeps for an issue, an agent's conversation and a thread, with the events that say
 * one of them moved.
 */

/** `GET /auth/whoami`'s answer for a person signed in to the dashboard. */
export interface AuthenticatedUser {
  readonly kind: "user";
  readonly login: string;
  /** Present only for a restricted member: a person signed in from outside the sign-in domain
   *  because a project's access list names them. Absent for everyone else, so a full member's
   *  answer is the one it always was. */
  readonly member_kind?: "restricted";
  /** The origins this Dispatch answers at, the canonical one first; absent where it answers at
   *  one origin. */
  readonly origins?: readonly string[];
  /** The decimal id at or below which every event is final (committed or rolled back for good)
   *  when Dispatch answered, read as the newest event this person can see, so no event of a
   *  project they cannot see moves it. Every read the page makes after this answer holds every
   *  event at or below it, so the event feed resumes from it on its first
   *  subscription and replays exactly what committed since. Always answered: a server that cannot
   *  answer the head answers a 503 rather than a whoami without one (`EVENT_HEAD_UNAVAILABLE` while
   *  no event id is proven final yet, which ends on its own; `EVENT_HEAD_READ_FAILED` when its
   *  database does not answer), and a
   *  server older than the field serves no `/ws/live` either, so no page of this release ever
   *  reads one of its answers. */
  readonly event_head: string;
}

/** `sign_in` on `GET /auth/whoami`'s `401 NO_IDENTITY` (`AUTH_NO_IDENTITY`) where the origin's one
 *  sign-in is a passkey: Dispatch's outside origin, whatever its passkey setting
 *  (`DISPATCH_SIGNIN_PASSKEY`), unless `DISPATCH_SIGNIN_OUTSIDE=1` signs people from outside in
 *  with Google there. The sign-in page there offers the passkey ceremony and no Google link.
 *  Every other 401 carries no `sign_in`. */
export const AUTH_SIGN_IN_PASSKEY = "passkey";

/**
 * The passkey ceremonies at Dispatch's outside origin, four unauthenticated `POST`s under
 * `/auth/passkey/` that sit outside the `/api/v1` route table and its keyed write layer, as
 * `/auth/logout` does. Each begin answers the options a browser's WebAuthn call takes, in the JSON
 * form go-webauthn writes them (every buffer a base64url string without padding), and sets the
 * ceremony's state in a cookie of its own; the finish that follows takes the credential the
 * browser made, in the same JSON form, and answers the person it signed in, with the session
 * cookie beside it.
 *
 * - `/auth/passkey/register/begin` takes `{token}`, the link's fragment (`PasskeyEnrollmentStart`),
 *   checks the link without using it, and answers `PasskeyEnrollmentOptions`: the creation
 *   options, whose `user.name` is the address the link was made for, and the link's `invitation`,
 *   the project it is for and the owner who made it.
 * - `/auth/passkey/register/finish` takes `PasskeyRegistration`, stores the passkey, uses the
 *   link, and signs the person in (`PasskeySignedIn`).
 * - `/auth/passkey/login/begin` takes `{}` and answers `PasskeyRequestOptions` for any passkey
 *   Dispatch holds (a discoverable credential, so it names none).
 * - `/auth/passkey/login/finish` takes `PasskeyAssertion` and signs the person in.
 *
 * A refusal is Dispatch's ordinary `{code, error}`, its code one of `PASSKEY_REFUSALS`.
 */
export interface PasskeyEnrollmentStart {
  /** The link's fragment: 32 random bytes, base64url without padding. */
  readonly token: string;
}

/** One credential an options list names, by its id. */
export interface PasskeyCredentialDescriptor {
  readonly type: "public-key";
  readonly id: string;
  readonly transports?: readonly string[];
}

/** The creation options of `/auth/passkey/register/begin`'s answer: go-webauthn's
 *  `protocol.CredentialCreation`. */
export interface PasskeyCreationOptions {
  readonly publicKey: {
    readonly rp: { readonly id?: string; readonly name: string };
    readonly user: { readonly id: string; readonly name: string; readonly displayName: string };
    readonly challenge: string;
    readonly pubKeyCredParams: readonly { readonly type: "public-key"; readonly alg: number }[];
    readonly timeout?: number;
    readonly excludeCredentials?: readonly PasskeyCredentialDescriptor[];
    readonly authenticatorSelection?: {
      readonly authenticatorAttachment?: string;
      readonly requireResidentKey?: boolean;
      readonly residentKey?: string;
      readonly userVerification?: string;
    };
    readonly hints?: readonly string[];
    readonly attestation?: string;
    readonly extensions?: Readonly<Record<string, unknown>>;
  };
}

/** What the enrollment page says of the link before the person makes a passkey: the key of the
 *  project whose access list names them, and the login of the owner who made the link. */
export interface PasskeyInvitation {
  readonly project: string;
  readonly made_by: string;
}

/** `/auth/passkey/register/begin`'s answer: the creation options, and the link's invitation. */
export interface PasskeyEnrollmentOptions extends PasskeyCreationOptions {
  readonly invitation: PasskeyInvitation;
}

/** `/auth/passkey/login/begin`'s answer: go-webauthn's `protocol.CredentialAssertion`. */
export interface PasskeyRequestOptions {
  readonly publicKey: {
    readonly challenge: string;
    readonly timeout?: number;
    readonly rpId?: string;
    readonly allowCredentials?: readonly PasskeyCredentialDescriptor[];
    readonly userVerification?: string;
    readonly hints?: readonly string[];
    readonly extensions?: Readonly<Record<string, unknown>>;
  };
}

/** What every credential a browser made carries, as JSON. */
interface PasskeyCredentialJSON {
  readonly id: string;
  readonly rawId: string;
  readonly type: "public-key";
  readonly authenticatorAttachment?: string;
  readonly clientExtensionResults: Readonly<Record<string, unknown>>;
}

/** `/auth/passkey/register/finish`'s body: the new passkey as `navigator.credentials.create`
 *  made it. */
export interface PasskeyRegistration extends PasskeyCredentialJSON {
  readonly response: {
    readonly clientDataJSON: string;
    readonly attestationObject: string;
    readonly transports?: readonly string[];
  };
}

/** `/auth/passkey/login/finish`'s body: the passkey's signature as `navigator.credentials.get`
 *  made it. */
export interface PasskeyAssertion extends PasskeyCredentialJSON {
  readonly response: {
    readonly clientDataJSON: string;
    readonly authenticatorData: string;
    readonly signature: string;
    readonly userHandle?: string;
  };
}

/** Each finish's answer: the person the passkey signed in, whose session cookie came with it. */
export interface PasskeySignedIn {
  readonly login: string;
}

/**
 * The codes a passkey ceremony is refused with:
 * - `INVITATION_UNKNOWN` (404): Dispatch holds no such link. The owner made a newer one for the
 *   address, reset it, or took it off the access list, each of which ends the address's unused
 *   links.
 * - `INVITATION_USED` (410): the link enrolled a passkey already.
 * - `INVITATION_EXPIRED` (410): 24 hours have passed since the owner made it.
 * - `INVITATION_NOT_LISTED` (403): the project's access list no longer names the address directly.
 * - `LISTED_BY_ANOTHER_OWNER` (403): a project whose owner did not make the link names the address
 *   directly; a passkey reads every project that names its person, so only one owner binds them.
 * - `WORKSPACE_ADDRESS` (403): the address is on a domain of the Google Workspace Dispatch signs
 *   people in for, whose people sign in with Google and never with a passkey.
 * - `FULL_MEMBER` (409): the address's people row is a full member's, who signs in with Google.
 * - `NOT_LISTED` (403): no access list names the person any more; that finish also ended their
 *   sign-in and deleted their passkeys.
 * - `MEMBERSHIP_UNREADABLE` (503): the access lists could not be read; nothing changed.
 * - `PASSKEY_UNKNOWN` (401): Dispatch holds no such passkey (the owner's Reset or a delisting
 *   deleted it).
 * - `PASSKEY_SIGN_COUNT` (401): the passkey's counter is not past the one Dispatch stored, as a
 *   copy's would be.
 * - `CEREMONY_EXPIRED` (400): the begin this finish follows is gone, older than five minutes, or
 *   used already: each begin's challenge is single-use, so a replayed finish, and an enrollment of
 *   the same address that another one finished first, meet it too. The page begins anew.
 * - `PASSKEY_REFUSED` (400): the credential did not verify for any other reason.
 * - `PASSKEY_RATE_LIMITED` (429): too many begins reached Dispatch at once; the next press a moment
 *   later begins anew.
 *
 * A step sent from a page of another origin (an `Origin` that is not the outside origin, or none
 * with a `Sec-Fetch-Site` other than `same-origin`) is refused 403 `{"error":"invalid request
 * origin"}` with no code, as a cross-origin cookie write is.
 */
export const PASSKEY_REFUSALS = [
  "INVITATION_UNKNOWN",
  "INVITATION_USED",
  "INVITATION_EXPIRED",
  "INVITATION_NOT_LISTED",
  "LISTED_BY_ANOTHER_OWNER",
  "WORKSPACE_ADDRESS",
  "FULL_MEMBER",
  "NOT_LISTED",
  "MEMBERSHIP_UNREADABLE",
  "PASSKEY_UNKNOWN",
  "PASSKEY_SIGN_COUNT",
  "CEREMONY_EXPIRED",
  "PASSKEY_REFUSED",
  "PASSKEY_RATE_LIMITED",
] as const;

export type PasskeyRefusal = (typeof PASSKEY_REFUSALS)[number];

/** GET /api/v1/whoami: a person by lowercase email (`login`), or an agent with its personal
 *  token's owner (a lowercase email) — null under the shared token or a verified service token —
 *  and that service token's subject, null unless one authenticated the request. `session` is the
 *  session the credential itself proved, which only an access token the secrets broker issued
 *  does, and `project`, deprecated, the one key of a one-key `projects`, null for any other list.
 *  Both are absent from a Dispatch older than broker-issued tokens, which a client reads as an
 *  older build rather than as null. The agent form's `projects` is the credential's list, sorted,
 *  and its `private_projects` those of them the caller must treat as private (a private project,
 *  one the credential no longer sees, one that names no project), so a key is public exactly when
 *  it is in `projects` and not in `private_projects`; both are absent when empty. A person at a
 *  browser lists none, so the person form carries neither.
 *
 *  `scopes` is the OAuth scopes the caller's own credential holds, which decide the routes it may
 *  call: both of them for a person at a browser, the set its row names for a personal token, and
 *  `[]` for the shared token and a verified service token, which act as nobody. The kind stays
 *  `agent` for a personal token holding `person`, because the kind names the credential and not
 *  the act; the person it acts for is `owner`. */
export type WhoamiResponse = WhoamiUser | WhoamiAgent;

export const UserIssueStateSchema = z
  .strictObject({
    pinned: z.boolean(),
    last_read_seq: z.number().int(),
    dismissed: z.array(z.string()),
    seq: z.number().int(),
  })
  .readonly();

export const UserStateUpdatedEventPayloadSchema = z
  .strictObject({
    login: z.string(),
    /** The viewer's row for one issue as the write left it. Absent from a snooze's event, which
     *  every signed-in feed carries and so names no issue of the writer's. */
    state: UserIssueStateSchema.optional(),
    /** Which of `pinned`, `last_read_seq`, `dismissed` and `snoozed` the write moved (`[]` when it
     *  moved none), so another tab refreshes its pinned list for a pin, its whole Inbox for a snooze
     *  or an un-snooze (`PUT`/`DELETE /api/v1/me/asks/{id}/snooze`, whose event carries only `login`
     *  and `["snoozed"]`), and neither for a read mark. Absent from an event written before the
     *  field existed, which a reader takes as "any of pinned, last_read_seq and dismissed may have
     *  moved". */
    changed: z.array(z.string()).optional(),
  })
  .readonly();

export type UserStateUpdatedEventPayload = z.output<typeof UserStateUpdatedEventPayloadSchema>;

/** `user_agent_state.updated`: one viewer's Clear or read mark on one session's conversation
 *  changed. It carries no state; the viewer's other tabs and devices refetch theirs. */
export const UserAgentStateUpdatedEventPayloadSchema = z
  .strictObject({ login: z.string(), session_id: z.string() })
  .readonly();

export type UserAgentStateUpdatedEventPayload = z.output<
  typeof UserAgentStateUpdatedEventPayloadSchema
>;

/** `user_thread_state.updated`: one viewer's read of the Inbox's threads moved. It carries no
 *  state; the viewer's other tabs and devices refetch theirs. `thread_id` names the thread when
 *  one thread was marked (`PUT /api/v1/me/threads/{id}/read`); a mark-all
 *  (`POST /api/v1/me/inbox/read`) names no thread, since one event stands for every thread it
 *  marked in that project. */
export const UserThreadStateUpdatedEventPayloadSchema = z
  .strictObject({ login: z.string(), thread_id: z.string().optional() })
  .readonly();

export type UserThreadStateUpdatedEventPayload = z.output<
  typeof UserThreadStateUpdatedEventPayloadSchema
>;
