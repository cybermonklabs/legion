# Hash-gate wire fixtures

These examples are the Dispatch-to-daemon contract for INFRA-1253 and INFRA-6065. Placeholder UUIDs and SHA-256 values stand for real values. Each JSON file shows the event `type` and `payload`, or the artifact response body, without the surrounding notification envelope.

- `artifact.json`: approval of version 1 stands at latest version 2 because the current record digest matches the approved digest; no digest is owed.
- `artifact-owed.json`: approval of version 1 is pending at latest version 2. Its approved content digest is available, but `current_sha256` is null and `digest_owed` is true until the fill.
- `artifact-version.json`: `artifact.version` records the markdown digest and owes the query-record digest.
- `artifact-version-filled.json`: the optional `artifact.version` shape after the digest is filled. Dispatch's write event uses the owed form; this is the additive shape a consumer must also accept.
- `artifact-digest.json`: the post-commit `artifact.digest` records the query-record digest and the approval verdict. `approved_version` and `approved_sha256` identify the standing approval of version 1 at version 2.

The daemon must accept the optional hash-gate fields when absent as well as present. A false `approval_current` does not make the approved-version fields required.
