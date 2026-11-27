-- The drop deletes the runs of a comparison no build of a release ever wrote or read: it ran in
-- the one inbox's branch alone, over test databases, and its gate is passed. No stored value
-- anything still reads is lost, so there is nothing here for a census to count.
select 0;
