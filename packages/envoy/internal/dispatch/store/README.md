# Dispatch store

`migrations/` holds every `<version>_<name>.up.sql` boot applies in version order, with the `<version>_<name>.census.sql` each one declares for the pre-deploy census.
`<version>` is one of the numbers the set grew up with, each of whose files keeps the number it landed with, and for every migration added since the UTC moment its author created it, `YYYYMMDDHHMMSS` (`date -u +%Y%m%d%H%M%S`):
two pull requests open at once cannot both claim one timestamp, while both would claim the next number.
This file sits beside that directory rather than in it because the directory is embedded with `all:` and `pgmigrate.Load` refuses a set holding a file named anything but `<version>_<name>.up.sql`, `.down.sql` or `.census.sql`, which would refuse every boot.

Boot applies every version `schema_migrations` lacks, in version order, so a fresh database applies the whole set in that order, and a migration that merges below one a database has already applied still runs there, once.
A deployed database applies a migration when the release carrying it deploys:
if T2's release deploys before T1 merges, that database applies T2 then T1 while a fresh one applies T1 then T2.
So a migration must not depend on its order relative to one written in a concurrent pull request, another branch open at the same time;
one that needs another's effect is written after that one has merged.
`max(version)`, the `schema_version` `/healthz` reports, is the highest version applied, not proof that every version below it is applied, which is why the auto-deployer's rollback guard compares the whole set of recorded versions.

**A migration must leave the release before it able to read and write the schema.**
A Dispatch release rolls, so the old build and the new build run against this schema at once, and nothing at boot refuses a schema newer than the binary knows.
A change is therefore expand-then-contract across two releases:
the expanding migration adds, with a default or a backfill, and the release that ships it writes both shapes, while the migration that drops, renames or narrows ships no earlier than the next release, once no running build reads the old shape.
A migration that cannot be written that way is a stop-the-world migration, deployed with no server of the earlier release running, and that is an exception named in the pull request, not a default.

`../../../cmd/dispatch/README.md` ("Database migrations") has the rule with its examples, the naming and the identifiers a set is refused for, and what the census is held to.

## Derived graph

The graph is the union of `graph_edges`, `graph_person_edges` and `graph_issue_edges`, through `refs.AllEdgesSQL`.
`graph_edges` retains the citation and structural relations existing builds read; sibling views add identity, recorded delivery and issue relations without changing old builds' results or query plans.
None of these views stores or writes an edge.
`person_aliases(person, alias_kind, alias)` derives canonical logins with `user_threads_login`, raw user actor ids, and session ids only where a stored session actor's server-verified `owner` identifies the person.
Routes, claims and role holders do not prove person ownership, so there is no role alias.
Delivery reads the four word-carrying event types used by `sessionanswers`, their `published_destinations`, and sent message/comment delivery attempts; a recorded role topic identifies the role, not a session or a person.
`envoy-dispatch census --deliveries` independently compares the graph's recipient sets with those records for every comment, message and ask created in the last 30 days.

`graph_issue_edges` derives `duplicate_of` for a closed issue from its latest explicit `Duplicate of dispatch://KEY` message or comment at or before closure, ordered by the comment's edit time when present.
Only declarations after the issue last reopened count; code examples and quoted content do not, and reopening removes the edge.
`filed_from` derives from the nullable `issues.filed_from` canonical reference, written only during issue creation because older records contain no reliable filing provenance.
The create path accepts an existing public message, ask or comment on an issue or project document in the filing's project; it refuses direct messages and guessed ownership.
`GET /api/v1/issues/{key}/links` reads the complete derived relation in the `GraphReferences` shape.
`GET /api/v1/issues/{key}/links/shadow` independently compares citations, parent, blocker and component edges with their source rows, returning `legacy`, `graph` and `mismatches`; zero is equality, not a count of the new kinds.
