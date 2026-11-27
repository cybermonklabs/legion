-- Dispatch holds a person whom Google refused (held_since), records the Workspace
-- domain each sign-in's ID token carried (hosted_domain), keeps a signed-out person's refresh
-- token for membership checks while they have access that outlives the browser (signed_out_at),
-- and records the membership pass's last refresh attempt (confirm_tried_at) and its last check of
-- a signed-out row (outliving_checked_at), so each row is tried about once an hour however many
-- the pass cannot finish with. A row has a recorded hold while held_since is later than
-- signed_in_at, and is signed out while signed_out_at is later than signed_in_at, so a sign-in by
-- any build releases both. Five nullable columns and a check added not valid, which writes the
-- catalog alone: no existing row is read or changed.
alter table people
  add column held_since timestamptz,
  add column hosted_domain text,
  add column signed_out_at timestamptz,
  add column confirm_tried_at timestamptz,
  add column outliving_checked_at timestamptz;
alter table people
  add constraint people_hosted_domain_not_empty
  check (hosted_domain is null or hosted_domain <> '') not valid;
