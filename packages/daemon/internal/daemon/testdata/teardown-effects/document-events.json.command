Three events of fixture F3, the real event log of one issue of a Legion deployment's Dispatch, read on 2026-11-27 with:

  curl -fsS -H "Authorization: Bearer $(cat "$DISPATCH_TOKEN_FILE")" "$DISPATCH_URL/api/v1/issues/<issue>/events?limit=200"

and kept with:

  jq '[.[] | select(.seq == 3 or .seq == 6 or .seq == 13)]
      | walk(if type == "object" and (.origin | type) == "object"
             then .origin = {host: "host", machine: "machine", cwd: "/work", session_title: "session"} else . end)
      | walk(if type == "string" then gsub("<PROJECT>"; "ACME") | gsub("<project>"; "acme") else . end)'

The events are plan.md's artifact.created (seq 3) and its artifact.version (seq 6), each of which names the
document in its payload alone (`artifact.id`, `artifact_id`) and holds no `artifact_id` of its own, and a
comment anchored to words of plan.md (seq 13), whose payload names the document in `artifact_name` and in
`anchor.artifact_id` and has no reply_to.

Replacements: every origin of an actor or an author (its host, machine, cwd and session_title) is the neutral
host, machine, /work and session; the project's key, in upper case and in lower case (issue keys, the
project, ref keys, block references, a role name in the comment's body), is ACME and acme. Nothing else was
changed: ids, times, the quote and the rest of the comment's body are as Dispatch answered them.
