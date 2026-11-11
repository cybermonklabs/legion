-- 20261111181500_pod_resume_seen_release.up.sql — a resume records the release it was taken
-- after, so the heal of a stuck resume asks one question and the wake that wrote the row
-- answered it (dispatch://INFRA-714).
--
-- seen_release is the newest release of the issue, of whatever kind, at the moment the wake was
-- taken. It is not the release the wake ends: release_id beside it is that one, and it is read
-- with the roles filter that keeps the woken share honest (store's LatestPodRelease), which this
-- column leaves exactly as it is.
--
-- The two are different questions and a row that answers only the first cannot answer the second.
-- "Which release did this wake end" passes over a release that suspended no role, because such a
-- release woke nobody. "Has the pod this resume was waiting for gone away" counts every release,
-- because the release of a pod no claim of the issue can use takes the pod away just as the idle
-- rule does. Reading the second question off release_id made every resume written after a
-- roleless release answer yes for ever, since its pairing release is the older roleful one: the
-- heal then fired on the resume it had itself just taken, and each wake of that role queued one
-- more start row against R6's one task per wake.
--
-- So the writer records this column from the one reading (store's newestPodRelease) and the heal
-- compares it against that same reading. A resume is stale exactly when a release has been
-- recorded since it was written, which is what "the pod it waited for is gone" means, and the
-- release that took that pod is one the wake could not have seen.
--
-- There is no down file: migrations here are forward-only.
alter table pod_resumes add column seen_release bigint not null default 0;

-- Every row standing now is given the issue's newest release as it stands, so no resume already
-- open is read as stale by the first wake after the deploy: only a release recorded after this
-- migration makes one so. A resume of an issue with no release row keeps the default, and the
-- next release of that issue is what makes it stale.
update pod_resumes r set seen_release = coalesce(
  (select max(p.id) from pod_releases p where p.project = r.project and p.issue = r.issue), 0);
