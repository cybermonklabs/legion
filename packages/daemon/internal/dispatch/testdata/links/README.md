detail-acme-5768.json is Dispatch's answer to the detail read of an issue that links two pull
requests (GET /api/v1/issues/<KEY>, which HTTPClient.GetIssue reads): ACME-5768, whose
external_links name https://github.com/acme/widgets/pull/5481, in the issue's own repository, and
https://github.com/acme/deploy/pull/6990, in another.
It is the body of the boot's read of an issue's links (step 8 of the plan of dispatch://INFRA-5910), and no test reads it yet.

It is real: the answer of one GET, read on 2026-11-27 with the deployment's Dispatch token, <KEY> being the issue's key in that deployment:

    curl -fsS -H "Authorization: Bearer $(cat "$DISPATCH_TOKEN_FILE")" "$DISPATCH_URL/api/v1/issues/<KEY>"

This repository is public, so the answer went through this filter and rewrite before it was kept:

    jq 'with_entries(select(.key | IN("key","project","number","title","status","parent","rank","priority","resolved_priority","priority_from","labels","primary_artifact_id","verification_artifact_id","decisions_artifact_id","last_seq","resolved_repo","blocked_by","claim","external_links")))
      | walk(if type == "object" then del(.origin) else . end)
      | walk(if type == "string" then
          gsub("<KEY>"; "ACME-5768") | gsub("<REPO>"; "acme/widgets") | gsub("<OTHER>"; "acme/deploy")
          | gsub("<SESSION>"; "01900000-0000-7000-8000-000000000003")
          | if . == "<P>" then "ACME" else . end
        else . end)'

<P> is the project's key, <REPO> the issue's repository, <OTHER> the repository of its other link, and <SESSION> the id of the session that holds the issue's claim.
The filter keeps the issue's own fields that the client reads, its links and its claim among them, and drops every other field: the assignee's email address, the origin of the session that created the issue and the documents with their authors.
The rewrite then drops every origin object left, the claim's among them, which named the session's machine, its working directory and its title.
In every string the project's key became the neutral ACME, as AGENTS.md asks, so the issue keeps its number; the two repositories became acme/widgets and acme/deploy; and the session id became a neutral id, since it named a session of the deployment that may still be live.
Every other value is as the filter left it, the claim's holder kind and time among them, which the client reads; internal/dispatch's resolved_test.go treats its captures the same way.
The file is laid out by hand as `bunx biome format --write` lays out JSON, since biome was not installed where it was rewritten.
