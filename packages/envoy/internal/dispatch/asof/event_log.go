package asof

// NewestEventIDStatement reads the newest event id at the cut: the largest id of an event at or
// before the cut, or null when no event is at or before it. The store binds it as it binds every
// statement at a cut, so it reads the events shadow. Ids are not commit order: an event at the
// cut can hold a larger id than an event after the cut. So this id is the largest id of the
// events at the cut, not the id of the newest of them. A page of an event log at a cut ends its id
// range at this id (decision 13 of the plan), so the page that reaches the newest event at the
// cut does not read each event that was written after the cut.
//
// It reads max(id), not `order by id desc limit 1`. That second form walks down the primary key
// from the newest event until it finds an event at the cut, so it reads each event written after
// the cut. The planner does not expect this, because it does not know that ids and times grow
// together. PostgreSQL does not change max() into that walk in a statement that has a WITH list,
// and each statement at a cut has one. So max(id) reads the ids of the events at the cut from
// events_created_at, an index on (created_at, id) that holds the id after the time.
const NewestEventIDStatement = `select max(id) from events`
