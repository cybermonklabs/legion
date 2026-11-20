// packages/broker-client/src/wire.ts
//
// The names the secrets broker's RFC 8693 token exchange is spoken in, as both clients spell them:
// the Go client's `internal/broker/oauthwire` holds the same constants, and
// `packages/envoy/internal/brokerclient/testdata/wire.json` is the corpus that holds the two to
// one wire. A value here that drifts from that file fails this package's own suite.

/** The broker's token endpoint, where every trade is posted. */
export const TOKEN_PATH = "/v1/oauth/token";

/** RFC 8693's own grant name. */
export const GRANT_TYPE_TOKEN_EXCHANGE = "urn:ietf:params:oauth:grant-type:token-exchange";

/** The OAuth client every caller of the exchange asks as (`oauthwire.ExchangeClientID`). */
export const EXCHANGE_CLIENT_ID = "dispatch-token-exchange";

/** The broker's own form parameter: the session name the minted token carries. RFC 8693 has
 *  none, and an entry that fixes no session requires it. */
export const FORM_SESSION = "session";

/** The broker's second form parameter: whether this trade wants the launcher credential its
 *  identity holds minted or moved onto the key it proved. An absent parameter asks, which is what
 *  every client written before the parameter sends and what `agent-secrets` and the helper want;
 *  {@link FORM_LAUNCHER_CREDENTIAL_DECLINED} is the one value that declines, and the broker
 *  refuses any other spelling. */
export const FORM_LAUNCHER_CREDENTIAL = "launcher_credential";

/** The one value of {@link FORM_LAUNCHER_CREDENTIAL} that declines the credential. */
export const FORM_LAUNCHER_CREDENTIAL_DECLINED = "false";

/** The broker's third form parameter: the projects the minted token names, project keys
 *  separated by one space, sorted and each once (`oauthwire.FormProjects`). Only a trade that acts
 *  as its person sends it, and the broker names only projects that person may see; absent is no
 *  projects. */
export const FORM_PROJECTS = "projects";

/** The `subject_token_type` a pod presents its projected Kubernetes service-account token under. */
export const SUBJECT_TOKEN_TYPE_KUBERNETES =
  "urn:envoy:params:oauth:token-type:kubernetes-service-account";

/** The `subject_token_type` a machine presents its own Google identity token under. */
export const SUBJECT_TOKEN_TYPE_GOOGLE = "urn:envoy:params:oauth:token-type:google-identity";

/** The `subject_token_type` a GitHub Actions job presents its workflow run's OIDC token under. */
export const SUBJECT_TOKEN_TYPE_GITHUB = "urn:envoy:params:oauth:token-type:github-workflow-run";

/** The `subject_token_type` a person presents their own Google identity token under, which only a
 *  sign-in carries. */
export const SUBJECT_TOKEN_TYPE_PERSON = "urn:envoy:params:oauth:token-type:person-google-identity";

/** The `actor_token_type` a machine's Google identity token takes beside a person's, under a type
 *  of its own because what it proves there is not who is asking but which box they ask from. */
export const ACTOR_TOKEN_TYPE_MACHINE = "urn:envoy:params:oauth:token-type:machine-google-identity";

/** The only `token_type` this grant mints: every token it answers is bound to the key the caller
 *  proved, and is presented as `Authorization: DPoP <token>` beside a proof of that key. */
export const TOKEN_TYPE_DPOP = "DPoP";

/** Followed by a colon, begins the `error_description` of the broker's `invalid_grant` for a pod
 *  whose session no registration names yet (`oauthwire.PodSessionNotRegistered`): a refusal of
 *  the broker's state that a later trade cures, which a client trades again on rather than stop. */
export const POD_SESSION_NOT_REGISTERED = "pod_session_not_registered";

/** Followed by a colon, begins the `error_description` of the broker's final `invalid_grant` for
 *  a pod a daemon other than the one its session name is bound to registered, or that a daemon
 *  registered under a name bound to none (`oauthwire.SessionNameNotThisRegistrars`): a name
 *  belongs for good to the daemon that first registered it, and only a registration, never a
 *  later trade, binds a name bound to none, so no later trade cures the refusal, and a client
 *  stops on it rather than trade again. */
export const SESSION_NAME_NOT_THIS_REGISTRARS = "session_name_not_this_registrars";
