# Response fixtures of the Cloud Storage double

These files hold the shapes of real Google answers.
`TestCloudStorageResponseFixtures` in `../fake_test.go` compares the status, the keys, the nesting and the JSON types of each answer of the double with its fixture.
It does not compare the values.

An issue pod of a Legion deployment captured the answers on 2026-11-25, between 22:17Z and 22:25Z.
The pod used its own read-only identity for build input and output, and `curl` with the token of the pod's metadata server.
Each file is the answer to one `curl` request, the request its section below names, and the last list page listed a narrower prefix than the first (below).
No command wrote to the bucket.
A script then replaced each bucket name, object name, id, generation, size, checksum, page token and timestamp with a placeholder.
It also removed the links (`selfLink`, `mediaLink`) and other fields that the double does not use.
No raw answer is kept, and no file names the deployment's bucket or project.

## The files

`list-next.json` is the first page of a JSON API list (`GET /storage/v1/b/<bucket>/o`) with a prefix and `maxResults=1`.
The status was 200.
The page holds 1 item and a `nextPageToken`.

`list-final.json` is the last page of a JSON API list.
The capture listed a narrower prefix of the same bucket for this page, because a walk through all pages of the first prefix took minutes.
The status was 200.
The page holds 72 items and no `nextPageToken`.

`object.json` is the object resource of a JSON API object read (`GET /storage/v1/b/<bucket>/o/<object>`) of the item in `list-next.json`.
The status was 200.
The fixture holds no `metadata` key, which holds the custom metadata of an object.
The double adds `metadata` when an upload sets it, as Cloud Storage does, and the test permits that one extra key.

`object-not-found.json` is the answer to a JSON API object read of a name that does not exist.
The status was 404.

`object-precondition.json` is the answer to a JSON API object read with `ifGenerationMatch=1`, of an object whose generation is not 1.
The status was 412.

`metadata-email.txt` is the answer of the GCE metadata server to `instance/service-accounts/default/email` with the header `Metadata-Flavor: Google`.
It is plain text with no line feed.
The address is a placeholder.

`metadata-token.json` is the answer of the GCE metadata server to `instance/service-accounts/default/token`.
The capture replaced the token with `fixture-token` before it wrote the file.
The capture kept `expires_in` and `token_type`.
This file spells that placeholder `<fixture-token>`, because the leak guard that runs before a push refuses the spelling `fixture-token` as a token.

`iam-sign-403.json` is the refusal of the IAM Credentials method `signBlob` for a service account that the pod may not sign as.
The status was 403.
The error id is a placeholder.

`iam-sign-200.json` is not a capture.
No identity that a pod may use can get a real signature, so this file holds the `SignBlobResponse` shape that Google documents (<https://cloud.google.com/iam/docs/reference/credentials/rest/v1/projects.serviceAccounts/signBlob>).
The key id and the signature are placeholders.
