-- Under DISPATCH_IDENTITY=iap, Dispatch reads each full member's Workspace account through the
-- Directory API. directory_confirmed_at is the last active answer, directory_checked_at the last
-- check of any outcome, and not_held_until the last active answer plus 24 hours: after it, a
-- not-held answer is no longer served. directory_user_id is the Directory user's immutable id from
-- the first active answer, which later answers are compared with. All four stay null outside IAP.
alter table people
  add column directory_confirmed_at timestamptz,
  add column directory_checked_at timestamptz,
  add column not_held_until timestamptz,
  add column directory_user_id text check (directory_user_id <> ''),
  add constraint people_directory_deadline check ((directory_confirmed_at is null) = (not_held_until is null));
