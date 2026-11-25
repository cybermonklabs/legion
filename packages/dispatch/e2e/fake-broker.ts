import { awaitsDecision, type DismissResponse } from "@legion/contracts";
import type {
  ApproverGrantResp,
  ApproverGrantsResponse,
  CredentialPendingResponse,
  CredentialPendingRow,
  DecisionResponse,
  LauncherCredentialResp,
  LauncherCredentialsResponse,
  PersonSignInResp,
  PersonSignInsResponse,
  RecordResponse,
  StateResponse,
} from "../web/src/api/types";
import { fakeBrokerAnswer } from "./fake-broker-token";
import { harnessPorts } from "./harness-ports";

// The secrets broker's reads Dispatch relays on every Inbox and Settings load: `GET /v1/pending`
// (the credential requests waiting on an approver, and the expired ones nobody dismissed),
// `GET /v1/grants` (the live grants of a person's sessions and those the person approved) and
// `GET /v1/person-signins` (the machines one person signed in on); `GET /v1/launcher-credentials`
// (the machine logins one person may revoke), which the machine-login page lists; the two routes
// that page drives: `POST /v1/machine-logins/lookup` (the login a typed code selects) and
// `POST /v1/credential-requests/{id}/approve|deny` (that login's decision, carrying the code
// again); `POST /v1/credential-requests/{id}/dismiss`, which the Inbox's Dismiss of an expired
// request drives; `POST /v1/person-signins/{id}/revoke`, which the Settings page's Sign-ins
// section drives; and the public `GET /.well-known/oauth-authorization-server`, the RFC 8414
// document Dispatch reads to learn whether this broker still serves the device grant. The server
// reaches this through
// DISPATCH_AGENT_SECRETS_URL (run-server.sh) whenever the harness switch is left unset
// (e2e/harness-broker.ts), and it lists nothing until a test seeds a request through
// /__fixture/pending, a grant through /__fixture/grants, a machine login a typed code selects
// through /__fixture/machine-logins, a live machine login through
// /__fixture/launcher-credentials or a sign-in through /__fixture/signins, relaying each row as
// seeded: a
// secret request's row carries the `approve_url` of its page on the broker that the Inbox links
// to (an absolute URL, which e2e/broker-approval.e2e.ts points at a record on the real broker,
// e2e/run-broker.sh), a machine login's row none. It also answers `POST /v1/oauth/introspect`,
// the RFC 7662 introspection Dispatch asks about every token presented under the DPoP scheme:
// a token minted by e2e/fake-broker-token.ts is live with the answer it carries, and every other
// token is not. It verifies no caller: the token the server presents,
// e2e/fake-broker-account-token's, is never read. Every other broker route answers 404.

/** A credential request as the broker lists it, beside the approver it waits on: the row is the
 *  SPA's own type, so a change to the contract fails the typecheck here too. Its `state` is the
 *  one it was seeded with, `pending` or `expired`; a row seeded with none is pending, as an older
 *  broker sends it, and is listed with none. */
export type FakePendingRequest = CredentialPendingRow & { readonly approver: string };

/** A machine login the operator's typed code selects, as `POST /v1/machine-logins/lookup`
 *  resolves it: the code the machine printed, and the record that lookup and every later read
 *  answer. Deciding the record writes its decision back into `record`, so a second lookup of the
 *  same code reports what the broker recorded. A record nobody decided whose `expires_at` has
 *  passed reads as expired, as the broker reads it, and refuses both decisions. */
export interface FakeMachineLogin {
  readonly code: string;
  record: RecordResponse;
}

/** One person's sign-in on one machine, as `GET /v1/person-signins` lists it beside the person it
 *  belongs to: the row is the SPA's own type, so a change to the contract fails the typecheck
 *  here too. Revoking it drops it from the list, as the real broker's revoke does. */
export type FakeSignIn = PersonSignInResp & { readonly person: string };

/** A live grant as `GET /v1/grants?approver=<login>` lists it, beside the login whose list holds
 *  it. That login is not the grant's own `approver`: the broker lists a grant to the person who
 *  approved it and to the operator of the session holding it, and a judged grant of a shared
 *  secret to every signed-in person. The row is the SPA's own type, so a change to the contract
 *  fails the typecheck here too. */
export type FakeGrant = ApproverGrantResp & { readonly listedFor: string };

/** A live machine login as `GET /v1/launcher-credentials?approver=<login>` lists it, beside the
 *  login whose list holds it: a person's own machine's and pod's logins are on that person's list
 *  alone, and a service's login is on every signed-in person's. The row is the SPA's own type, so
 *  a change to the contract fails the typecheck here too. */
export type FakeLauncherCredential = LauncherCredentialResp & { readonly listedFor: string };

/** Every piece of fixture state this listener holds; `PUT /__fixture/reset` replaces it whole. */
interface FakeBrokerState {
  /** Whether this broker serves RFC 8628's device grant, which its RFC 8414 metadata names and
   *  Dispatch relays to decide whether to serve the machine-code page. A real broker serves it
   *  while BROKER_MACHINE_LOGIN_URL is set, which the suite's own broker does, so it is the
   *  default here and a row that drives the other answer sets it back by resetting. */
  deviceGrant: boolean;
  /** The dismissals of expired pending rows, at most one per record: a dismissed row leaves every
   *  list, and a second dismissal answers the first one's values. Seeding the pending rows again
   *  clears them, since they named the rows that seeding replaced. */
  dismissals: DismissResponse[];
  grants: FakeGrant[];
  launcherCredentials: FakeLauncherCredential[];
  machineLogins: FakeMachineLogin[];
  pending: FakePendingRequest[];
  signIns: FakeSignIn[];
}

function emptyState(): FakeBrokerState {
  return {
    deviceGrant: true,
    dismissals: [],
    grants: [],
    launcherCredentials: [],
    machineLogins: [],
    pending: [],
    signIns: [],
  };
}

let state = emptyState();

/** A confirmation code as the broker prints it, from the code as a person typed it: upper-cased,
 *  spaces dropped and the hyphen in place (internal/cipher/oauth/codes.go's NormalizeUserCode). */
function normalizeCode(code: string): string {
  const bare = code.replaceAll(/[ -]/g, "").toUpperCase();
  return bare.length === 8 ? `${bare.slice(0, 4)}-${bare.slice(4)}` : bare;
}

function brokerError(status: number, code: string, error: string): Response {
  return Response.json({ code, error }, { status });
}

/** A listed row's state as the broker names it: a row seeded with none is pending. */
function rowState(row: FakePendingRequest): NonNullable<FakePendingRequest["state"]> {
  return awaitsDecision(row) ? "pending" : "expired";
}

/** A machine login's record as the broker reads it now: one nobody decided whose deadline has
 *  passed is expired, with no decision, before the broker's sweep ends it and after
 *  (internal/cipher/requests's ReadRecord). */
function currentRecord(login: FakeMachineLogin): RecordResponse {
  const { record } = login;
  if (record.decided === null && Date.parse(record.expires_at) <= Date.now()) {
    return { ...record, state: "expired" };
  }
  return record;
}

/** Decides a machine login as internal/cipher/api/handlers_ui_records.go does, keeping the three
 *  refusals a test reaches: NOT_FOUND, for a record id the fixture does not hold, which is a
 *  mistake in the test rather than a crash; NOT_APPROVER, the control that proves Dispatch names
 *  the approver from its own session; and RECORD_TERMINAL for a login whose deadline passed
 *  before anybody decided it, which records nothing. The broker's other refusals of a machine
 *  login (a missing approver or code, a code that does not match, a record already decided) are
 *  gone, since nothing drives this listener into them. An approval mints the credential the login
 *  then holds, named after the record so a test can read it. */
async function decide(recordID: string, approve: boolean, request: Request): Promise<Response> {
  const login = state.machineLogins.find((entry) => entry.record.record_id === recordID);
  if (login === undefined) {
    return brokerError(404, "NOT_FOUND", "no such credential request");
  }
  const body = (await request.json()) as { approver?: string };
  const { record } = login;
  if (record.approver !== "anyone" && record.approver !== body.approver) {
    return brokerError(403, "NOT_APPROVER", `${body.approver} may not decide this request`);
  }
  if (currentRecord(login).state === "expired") {
    return brokerError(
      409,
      "RECORD_TERMINAL",
      "this machine login expired before its approver acted on it"
    );
  }
  const credentialID = approve ? `cred-${record.record_id}` : null;
  login.record = {
    ...record,
    decided: {
      at: new Date().toISOString(),
      credential_id: credentialID,
      event: approve ? "approved" : "denied",
    },
    state: approve ? "approved" : "denied",
  };
  // The two decisions answer two different shapes: an approval names the credential it
  // minted (DecisionResponse), a denial names the state alone (StateResponse).
  const answer: DecisionResponse | StateResponse = approve
    ? { credential_id: credentialID, state: "approved" }
    : { state: "denied" };
  return Response.json(answer);
}

/** Dismisses an expired request as internal/cipher/api/handlers_ui_records.go's dismissRecord
 *  does, refusing in its order: APPROVER_REQUIRED for a body that names nobody; NOT_FOUND for a
 *  record id no seeded row holds; NOT_APPROVER for a login that is not the row's approver, where
 *  that approver is not `anyone`, whom every login is; and RECORD_PENDING for a row that can
 *  still be decided. A row is dismissed once: the dismissal takes it out of every list, and a
 *  second answers the first one's values and writes nothing. The broker's RECORD_TERMINAL is
 *  gone, since no seeded row was decided. */
async function dismiss(recordID: string, request: Request): Promise<Response> {
  const { approver = "" } = (await request.json()) as { approver?: string };
  if (approver === "") {
    return brokerError(400, "APPROVER_REQUIRED", "approver is required");
  }
  const row = state.pending.find((entry) => entry.record_id === recordID);
  if (row === undefined) {
    return brokerError(404, "NOT_FOUND", "no such credential request");
  }
  // The fake seeds no group rows, so the broker's group-member arm has nothing to admit here.
  if (row.approver !== "anyone" && row.approver !== approver) {
    return brokerError(
      403,
      "NOT_APPROVER",
      "only the record's approver, a member of the group that is its approver, or anyone signed in for a request whose approver is anyone, may dismiss it"
    );
  }
  if (rowState(row) === "pending") {
    return brokerError(409, "RECORD_PENDING", "request has not expired; it can still be decided");
  }
  const earlier = state.dismissals.find((dismissal) => dismissal.record_id === recordID);
  if (earlier !== undefined) {
    return Response.json(earlier);
  }
  const dismissal: DismissResponse = {
    dismissed_at: new Date().toISOString(),
    dismissed_by: approver,
    record_id: recordID,
  };
  state = { ...state, dismissals: [...state.dismissals, dismissal] };
  return Response.json(dismissal);
}

// FAKE_BROKER_PORT, or with it unset port 0, so the OS chooses; the line at the end reports the
// port bound, which e2e/harness-start.ts reads.
const server = Bun.serve({
  hostname: "127.0.0.1",
  port: harnessPorts().fakeBroker.port,
  async fetch(request) {
    const url = new URL(request.url);
    // `GET /v1/settings`: this broker takes the approver fields, as the broker of this release does;
    // `decide` reads the approver and ignores the rest.
    if (request.method === "GET" && url.pathname === "/v1/settings") {
      return Response.json({ approver_fields: true });
    }
    // The broker's own row shape names no approver: it lists one approver's requests. It lists
    // only the states the caller asks for (Dispatch asks for both), every seeded row when it names
    // none, and never a dismissed row; the pending rows come before the expired ones, as
    // internal/cipher/requests's PendingForApprover orders them.
    if (request.method === "GET" && url.pathname === "/v1/pending") {
      const approver = url.searchParams.get("approver");
      const states = url.searchParams.getAll("state");
      const listed = state.pending.filter(
        (row) =>
          row.approver === approver &&
          (states.length === 0 || states.includes(rowState(row))) &&
          !state.dismissals.some((dismissal) => dismissal.record_id === row.record_id)
      );
      const body: CredentialPendingResponse = {
        pending: [
          ...listed.filter((row) => rowState(row) === "pending"),
          ...listed.filter((row) => rowState(row) === "expired"),
        ].map(({ approver: _, ...row }) => row),
      };
      return Response.json(body);
    }
    // The grants one person's page lists, as the real broker answers them: the rows seeded for
    // that login, each without the fixture's own `listedFor`, which no broker sends.
    if (request.method === "GET" && url.pathname === "/v1/grants") {
      const approver = url.searchParams.get("approver");
      const body: ApproverGrantsResponse = {
        grants: state.grants
          .filter((row) => row.listedFor === approver)
          .map(({ listedFor: _, ...row }) => row),
      };
      return Response.json(body);
    }
    // The machine logins one person may revoke, as internal/cipher/api/handlers_ui_machinelogin.go
    // lists them: the rows seeded for that login, each without the fixture's own `listedFor`,
    // which no broker sends. A row's `person` is relayed as seeded, which is the field that marks
    // a person's pod login.
    if (request.method === "GET" && url.pathname === "/v1/launcher-credentials") {
      const approver = url.searchParams.get("approver");
      const body: LauncherCredentialsResponse = {
        credentials: state.launcherCredentials
          .filter((row) => row.listedFor === approver)
          .map(({ listedFor: _, ...row }) => row),
      };
      return Response.json(body);
    }
    // The RFC 8414 document, public on the real broker too: Dispatch reads
    // `grant_types_supported` from it and serves the machine-code page only while it names the
    // device code. The other members are what internal/cipher/api/handlers_metadata.go writes,
    // against this listener's own address, since a client that reads one reads them all.
    if (request.method === "GET" && url.pathname === "/.well-known/oauth-authorization-server") {
      const issuer = url.origin;
      return Response.json({
        issuer,
        token_endpoint: `${issuer}/v1/oauth/token`,
        ...(state.deviceGrant
          ? { device_authorization_endpoint: `${issuer}/v1/oauth/device_authorization` }
          : {}),
        introspection_endpoint: `${issuer}/v1/oauth/introspect`,
        revocation_endpoint: `${issuer}/v1/oauth/revoke`,
        grant_types_supported: [
          "refresh_token",
          "urn:ietf:params:oauth:grant-type:token-exchange",
          ...(state.deviceGrant ? ["urn:ietf:params:oauth:grant-type:device_code"] : []),
        ],
        response_types_supported: [],
        token_endpoint_auth_methods_supported: ["none"],
        dpop_signing_alg_values_supported: ["ES256"],
        subject_token_types_supported: ["urn:ietf:params:oauth:token-type:access_token"],
      });
    }
    // The answer Dispatch reads a DPoP-presented token back through (internal/ciphertoken's
    // introspect): a form carrying `token`, answered with what that token says of itself.
    if (request.method === "POST" && url.pathname === "/v1/oauth/introspect") {
      const form = new URLSearchParams(await request.text());
      return Response.json(fakeBrokerAnswer(form.get("token") ?? ""));
    }
    // One person's sign-ins, as internal/cipher/api/handlers_ui_personsignin.go lists them: the
    // person the caller names and nobody else, since that is what keeps one person off another's.
    if (request.method === "GET" && url.pathname === "/v1/person-signins") {
      const person = url.searchParams.get("person");
      if ((person ?? "") === "") {
        return brokerError(400, "PERSON_REQUIRED", "person is required");
      }
      const body: PersonSignInsResponse = {
        signins: state.signIns
          .filter((row) => row.person === person)
          .map(({ person: _, ...row }) => row),
      };
      return Response.json(body);
    }
    // A sign-in of anyone but the person the body names is an unknown id, the refusal that keeps
    // one person from reaching another's by trying ids.
    if (request.method === "POST" && url.pathname.startsWith("/v1/person-signins/")) {
      const rest = url.pathname.slice("/v1/person-signins/".length);
      if (rest.endsWith("/revoke")) {
        const signInID = decodeURIComponent(rest.slice(0, -"/revoke".length));
        const { person } = (await request.json()) as { person?: string };
        const found = state.signIns.find(
          (row) => row.signin_id === signInID && row.person === person
        );
        if (found === undefined) {
          return brokerError(404, "NOT_FOUND", "no sign-in of that person has that id");
        }
        state = { ...state, signIns: state.signIns.filter((row) => row !== found) };
        return Response.json({ state: "revoked", tokens_ended: 1 });
      }
    }
    // Only the typed code selects a machine login (decision 13), so this answers that login's
    // record whole, decided, expired or neither, as internal/cipher/api/handlers_ui_machinelogin.go
    // does.
    if (request.method === "POST" && url.pathname === "/v1/machine-logins/lookup") {
      const { code } = (await request.json()) as { code?: string };
      if ((code ?? "") === "") {
        return brokerError(400, "CODE_REQUIRED", "code is required");
      }
      const login = state.machineLogins.find(
        (entry) => normalizeCode(entry.code) === normalizeCode(code ?? "")
      );
      if (login === undefined) {
        return brokerError(404, "NO_SUCH_CODE", "no pending machine login has this code");
      }
      return Response.json(currentRecord(login));
    }
    if (request.method === "POST" && url.pathname.startsWith("/v1/credential-requests/")) {
      const rest = url.pathname.slice("/v1/credential-requests/".length);
      const slash = rest.lastIndexOf("/");
      const action = rest.slice(slash + 1);
      const recordID = decodeURIComponent(rest.slice(0, slash));
      if (action === "approve" || action === "deny") {
        return decide(recordID, action === "approve", request);
      }
      if (action === "dismiss") {
        return dismiss(recordID, request);
      }
    }
    if (request.method === "PUT" && url.pathname === "/__fixture/pending") {
      const pending = (await request.json()) as FakePendingRequest[];
      state = { ...state, dismissals: [], pending };
      return Response.json({ ok: true });
    }
    if (request.method === "PUT" && url.pathname === "/__fixture/grants") {
      const grants = (await request.json()) as FakeGrant[];
      state = { ...state, grants };
      return Response.json({ ok: true });
    }
    if (request.method === "PUT" && url.pathname === "/__fixture/launcher-credentials") {
      const launcherCredentials = (await request.json()) as FakeLauncherCredential[];
      state = { ...state, launcherCredentials };
      return Response.json({ ok: true });
    }
    if (request.method === "PUT" && url.pathname === "/__fixture/machine-logins") {
      const machineLogins = (await request.json()) as FakeMachineLogin[];
      state = { ...state, machineLogins };
      return Response.json({ ok: true });
    }
    if (request.method === "PUT" && url.pathname === "/__fixture/signins") {
      const signIns = (await request.json()) as FakeSignIn[];
      state = { ...state, signIns };
      return Response.json({ ok: true });
    }
    if (request.method === "PUT" && url.pathname === "/__fixture/device-grant") {
      const { served } = (await request.json()) as { served: boolean };
      state = { ...state, deviceGrant: served };
      return Response.json({ ok: true });
    }
    if (request.method === "PUT" && url.pathname === "/__fixture/reset") {
      state = emptyState();
      return Response.json({ ok: true });
    }
    if (url.pathname === "/healthz") return Response.json({ ok: true });
    return brokerError(404, "NOT_FOUND", "not found");
  },
});

console.log(`fake broker listener on 127.0.0.1:${server.port}`);
