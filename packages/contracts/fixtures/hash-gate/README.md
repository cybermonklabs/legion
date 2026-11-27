# Hash-gate wire fixtures

These files are the contract between the Legion daemon and Dispatch for the hash-gate fields (R1 of dispatch://INFRA-6065/spec#b-r-contract-additive).
Each file holds one shape: Dispatch's verdict on whether the approval of a document still stands for its latest version, as Dispatch sends it.

No Dispatch release sends these fields yet, so every file but `approval-version-mode.json` is written by hand from R1.
dispatch://INFRA-1253's emission test (its check `c-approval-bytes`) compares the output of Dispatch with these files.
The daemon's own readers decode them in `TestHashGateApprovalReadsTheVerdict` (`packages/daemon/internal/dispatch/hash_gate_test.go`) and `TestHashGateDecodesDigestEvents` (`packages/daemon/internal/intake/hash_gate_test.go`).
A change to a file here runs those tests again: each test file names this directory in a `//ci:reads` line.

Change a file only by hand, and change it together with Dispatch's emission and the daemon's readers.
`biome.json` leaves the directory out of the formatter, so every file stays as written.

## The files

| File | Shape | Spec clause | Actor |
| --- | --- | --- | --- |
| `approval-standing-below-latest.json` | GET body: `state: approved`, `version: 7`, `latest_version: 8`, `content_sha256`, `current_sha256`, `digest_owed: false` | R1; R5, the approval stands for the latest version whatever the approved version | none (a read) |
| `approval-digest-owed.json` | GET body: `state: stale`, `version: 7`, `latest_version: 8`, `current_sha256: null`, `digest_owed: true` | R1; R3 "A read", a read whose digest is owed gives no verdict; R5, the approval does not stand | none (a read) |
| `approval-version-mode.json` | GET body with no `current_sha256` and no `digest_owed`, captured | R1's last sentence, a field Dispatch does not send reads as no verdict; R5, today's rule | none (a read) |
| `artifact-version-digest-owed.json` | `artifact.version` event of version 8 with `digest_owed: true` and `content_sha256` | R1; R3 "Entry into hash mode" and "A version", a version that owes its digest | `session` (the author of the version) |
| `artifact-version-digest-not-owed.json` | `artifact.version` event of version 9 with `digest_owed: false` and `content_sha256` | R1; R3 "Entry into hash mode" | `session` (the author of the version) |
| `artifact-digest-current.json` | `artifact.digest` event of version 8: `approval_current: true`, with `approved_version` and `approved_sha256` | R1; R3 "A digest" | `system` (Dispatch's fill) |
| `artifact-digest-not-current.json` | `artifact.digest` event of version 10: `approval_current: false`, with no `approved_version` and no `approved_sha256` | R1; R3 "A digest" | `system` (Dispatch's fill) |

The specs are dispatch://INFRA-6065/spec#b-r-contract-additive (R1), dispatch://INFRA-6065/spec#b-r-daemon-event-transitions (R3) and dispatch://INFRA-6065/spec#b-r-approval-readers (R5).

## The GET bodies

A GET body is the part of the answer of `GET /api/v1/artifacts/{id}` that the daemon reads (`dispatch.HTTPClient.Approval`): `issue_key`, `slug`, `primary`, `role` and `approval`, the approval without its `reason_tree`.
`approval.content_sha256` is the SHA-256 of the text of the version the latest review reviewed, which Dispatch pinned when it recorded the review (`content_sha256_source`).
`approval.current_sha256` is the digest of the latest version, null while that digest is owed.
`approval.digest_owed` says whether Dispatch still owes the digest of the latest version.
The daemon reads an absent `digest_owed` as version mode, and `false` as a verdict: the two are not the same.

The two hash-gate bodies are on one document of `ACME-7`: a person approved version 7, and version 8 changed no approved requirement.
`approval-digest-owed.json` is the read while the digest of version 8 is owed, and `approval-standing-below-latest.json` the read after Dispatch filled it.

`approval-version-mode.json` was captured on 2026-11-27 from the spec of dispatch://INFRA-1098, approved at its latest version 180, with this command, which gives curl the token on standard input:

```sh
printf 'Authorization: Bearer %s' "$(cat "$DISPATCH_TOKEN_FILE")" | curl -fsS -H @- "$DISPATCH_URL/api/v1/artifacts/89c8e344-c6ad-4fa0-89c5-d252856dc817" | jq '{issue_key, slug, primary, role, approval: (.approval | del(.reason_tree))}'
```

One value was redacted: the approver's `approval.by.id`, a person's email, is `person@example.com`.
Every other field is as the command printed it.
The two hash-gate bodies have the same fields, and `current_sha256` and `digest_owed` besides.

## The events

An event file is one whole Dispatch event: `id`, `issue_key`, `artifact_id`, `project`, `seq`, `type`, `actor`, `notify`, `created_at` and `payload`.
It is the shape of the issue's event log (`dispatch.Client.EventsAfter`, which the boot catch-up decodes with `intake.DecodeDispatchEvent`).
Dispatch publishes it on the NATS subject `notifications.dispatch.issue.<project>.<key>.<type>`, for example `notifications.dispatch.issue.ACME.ACME-7.artifact.digest`.
Envoy carries it as the `payload` string of its envelope, as `packages/daemon/internal/intake/testdata/dispatch/artifact-version.json` shows for a captured `artifact.version`.
The event is whole because the daemon orders two verdicts of one version by the event's mark, its `seq` and `created_at`.

An `artifact.version` keeps the payload of a captured one, and adds `digest_owed` and `content_sha256` beside `version`.
An `artifact.digest` payload holds `artifact_id`, `name`, `version` (an integer, not the object of `artifact.version`), `content_sha256` and `approval_current`, and `approved_version` and `approved_sha256` where Dispatch names the approved version.
The daemon reads `approved_version` for nothing: the approved version stays the version of the approval it applied.

The actor of an `artifact.digest` is of kind `system`: Dispatch's fill, which writes the digest after the version.
Its id here, `digest-fill`, is a placeholder; the daemon reads no id of it.

The four events are on the same document of `ACME-7`, in the order of their `seq`:
version 8 owes its digest (40), the fill reports the approval of version 7 current for it (41), version 9 comes with `digest_owed: false` (42), and the fill reports the approval not current for version 10 (44), whose own `artifact.version` (43) is not a file here.
Each `artifact.version` that carries `digest_owed`, true or false, is followed by an `artifact.digest` of that version, so the daemon reads the version's verdict as owed until that digest comes; the digest of version 9 is not a file here.

The events with no digest field are the existing captures in `packages/daemon/internal/intake/testdata/dispatch/` (`TestDecodeCapturedProducerEnvelopes`).
