// The names of a request's values that a wire contract declares public: every other value a
// request carries, under any name, is a credential the words of its refusal must not print
// (`requestCredentials` here, `requestTokens` in Go). A name is here because the contract that
// defines it says its value is public, never because a test needs it, and each entry names that
// contract. A name nobody listed counts as a credential, so a new parameter is redacted until a
// contract declares it public. Go reads this list from
// packages/envoy/internal/redact/public_names_generated.go, which `bun run gen:go` writes from it
// and `bun run check:go` holds to it; `public-request-names.test.ts` and Go's
// public_names_test.go fail on an entry with no source, or on a name a credential travels under.

/** Where a request carries the value: a header, or a field of its query, form or JSON body. */
export type PublicNamePlace = "header" | "field";

/** One name whose value is public, where it is carried, and the contract that says so. */
export type PublicRequestName = {
  readonly place: PublicNamePlace;
  readonly name: string;
  readonly source: string;
};

export const PUBLIC_REQUEST_NAMES: readonly PublicRequestName[] = [
  { place: "header", name: "Accept", source: "RFC 9110 §12.5.1" },
  { place: "header", name: "Content-Length", source: "RFC 9110 §8.6" },
  { place: "header", name: "Content-Type", source: "RFC 9110 §8.3" },
  { place: "header", name: "User-Agent", source: "RFC 9110 §10.1.5" },
  {
    place: "header",
    name: "Metadata-Flavor",
    source:
      "Google Compute Engine metadata server: the header every request must carry, value Google",
  },
  { place: "field", name: "grant_type", source: "RFC 6749 §4.1.3, §6; RFC 8693 §2.1" },
  {
    place: "field",
    name: "client_id",
    source: "RFC 6749 §2.2: a client identifier is not a secret",
  },
  { place: "field", name: "scope", source: "RFC 6749 §3.3" },
  { place: "field", name: "subject_token_type", source: "RFC 8693 §2.1" },
  { place: "field", name: "actor_token_type", source: "RFC 8693 §2.1" },
  { place: "field", name: "requested_token_type", source: "RFC 8693 §2.1" },
  { place: "field", name: "audience", source: "RFC 8693 §2.1" },
  { place: "field", name: "resource", source: "RFC 8693 §2.1; RFC 8707 §2" },
  {
    place: "field",
    name: "session",
    source: "the broker's token exchange: brokerclient/testdata/wire.json, form.session",
  },
  {
    place: "field",
    name: "projects",
    source: "the broker's token exchange: brokerclient/testdata/wire.json, form.projects",
  },
  {
    place: "field",
    name: "launcher_credential",
    source:
      "the broker's token exchange: brokerclient/testdata/wire.json, form.launcher_credential",
  },
  {
    place: "field",
    name: "kind",
    source: "POST /v1/enrollments: the broker contract's enrollment kind",
  },
  {
    place: "field",
    name: "runtime_id",
    source: "POST /v1/enrollments: the broker contract's runtime the enrollment names",
  },
  {
    place: "field",
    name: "session_id",
    source: "POST /v1/enrollments: the broker contract's session the enrollment names",
  },
  {
    place: "field",
    name: "operator",
    source: "POST /v1/enrollments: the broker contract's operator, the person's sign-in",
  },
  {
    place: "field",
    name: "thumbprint",
    source: "POST /v1/enrollments: the RFC 7638 thumbprint of the session's public key",
  },
  {
    place: "field",
    name: "delegates",
    source:
      "IAM Service Account Credentials API, generateAccessToken: the service accounts of the chain",
  },
  {
    place: "field",
    name: "lifetime",
    source: "IAM Service Account Credentials API, generateAccessToken: the token's lifetime",
  },
];

/** Whether the value under `name`, carried at `place`, is public by its contract: a header's name
 *  compared without case, as HTTP compares it, a field's as written. */
export function isPublicRequestName(place: PublicNamePlace, name: string): boolean {
  return PUBLIC_REQUEST_NAMES.some(
    (entry) =>
      entry.place === place &&
      (place === "header" ? entry.name.toLowerCase() === name.toLowerCase() : entry.name === name)
  );
}
