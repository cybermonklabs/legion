package model

// EventPayloadSQL is the payload of the events row named e as this build hands it to a caller or a
// subscriber. Every read that hands a stored event on selects it in place of the bare column: api's
// eventSelect, which the issue and document event feeds, the whole log and the live socket's replay
// and relay read through, and the outbox's readBatchSQL, which publishes each event to its topics.
//
// It is the stored payload, but for one shape. An issue.* payload carries the issue's waits under
// both spellings through this release (Issue.BlockedBy and Issue.Needs, R10 of the issue-tree
// spec). A payload the release before wrote holds blocked_by alone, since its Issue had no needs,
// and the two builds serve one database through a roll and after a rollback. So an issue.* payload
// that holds one of the two members and not the other answers both: the member it lacks carries the
// value of the one it holds, the same targets or the same null. The stored row is never rewritten,
// since the log is the record of what was written. A payload that holds one of them is an object:
// `->` with a key answers null on any other JSON value.
//
// A payload that holds both, or neither, and every payload of another type, is the column itself,
// so it reads byte for byte as it is stored. The changed list of the payload stays as it was
// written: the release before names a move of the waits blocked_by alone, where this build names
// blocked_by and needs (api's issueChangedFields), and no reader needs needs named there. The page
// refreshes on either name (ISSUE_RELATION_FIELDS in packages/dispatch's
// web/src/api/event-refresh.ts), and the daemon reads only status and the resolved priority from
// the list.
const EventPayloadSQL = `case when e.type like 'issue.%' and (e.payload -> 'blocked_by' is null) <> (e.payload -> 'needs' is null)
		then e.payload || jsonb_build_object(
			'blocked_by', coalesce(e.payload -> 'blocked_by', e.payload -> 'needs'),
			'needs', coalesce(e.payload -> 'needs', e.payload -> 'blocked_by'))
		else e.payload end`
