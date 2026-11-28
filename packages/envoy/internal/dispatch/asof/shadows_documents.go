package asof

// artifactsShadow is decision 2's artifacts: each row made at or before the cut (created_at, its
// insert's now()). A document removed since has no row and is in no shadow: it answers 404 by id
// and is left out of every list at the cut, as it is now.
//   - name: the previous_name of the first artifact.renamed after the cut, which records the
//     name the correction replaced (renameArtifact, api/artifact_rename.go); else the row's. The
//     rename names the document in its payload (artifact.id), by which events_artifact_renamed
//     keys it.
//   - last_seq: the newest seq of the document's own events at the cut, as the append sets it; 0
//     for a document whose events are its issue's.
//   - Every other column: the row's. No write changes slug, kind, is_primary, created_by,
//     project_key, ref_key or session_id; role is set from the slug, at the insert or at the
//     first version a release with the column writes (api/artifacts.go).
//
// The versions of a document are artifact_versions' rows made at or before the cut
// (createdByTheCut). A version's summary and renamed_by_delete are the present ones: a comment's
// delete renames the version its accepted suggestion named (renameAcceptedVersion,
// api/comment_delete.go), and a post deleted after the cut reads at the cut as deleted, with no
// part of its body (decision 14).
func artifactsShadow(cut string) string {
	return `select a.id as id,
		a.issue_key as issue_key,
		a.slug as slug,
		coalesce((
			select r.payload->>'previous_name' from public.events r
			where r.type = 'artifact.renamed' and r.payload->'artifact'->>'id' = a.id::text
				and r.created_at > ` + cut + `
			order by r.id limit 1), a.name) as name,
		a.kind as kind,
		a.is_primary as is_primary,
		a.created_by as created_by,
		a.created_at as created_at,
		a.project_key as project_key,
		a.ref_key as ref_key,
		coalesce((select e.seq from public.events e where e.artifact_id = a.id and e.created_at <= ` + cut + `
			order by e.seq desc limit 1), 0) as last_seq,
		a.session_id as session_id,
		a.role as role
	from public.artifacts a
	where a.created_at <= ` + cut
}
