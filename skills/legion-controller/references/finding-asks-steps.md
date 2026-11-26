# The steps of a finding ask's work

Read this when `skill://legion-controller/references/finding-asks.md` sends you here; that file states the rule each section here carries out.

## File a clear defect

Every reply under the evidence mentions the finder's role, the token after `Clear defect from ` (leave out `--mention` when it is `controller`):

```bash
dispatch comment --issue '<the evidence's issue>' --reply-to '<comment id>' --mention 'role:<the finder's token>' --body-file -
```

1. Check it is clear: the fix needs no choice a person must make, in a repository both role GitHub Apps are installed on. Otherwise reply `Not filed: <the choice, or the repository the Apps miss>` and open nothing: the finder opens its ask.
2. Search first (`skill://dispatch-first`). When an open issue tracks it and a live tree owns that issue (its record's `tree`), file nothing and reply `Not filed: tracked by dispatch://<KEY>; write to role:<that tree's architect's token>`. When that issue is in no running tree, comment the evidence there and reply `Filed dispatch://<KEY>`.
3. When a live tree's open pull request changes the files the fix touches (the live trees: `skill://legion-controller/references/admission.md`), file nothing and reply `Not filed: dispatch://<ROOT>; write to role:<its architect's token>`. Build each token as `skill://legion-controller/references/operations.md#a-request-to-another-session` says.
4. Otherwise file it where the fix belongs, in the project that repository maps to, or in your own project when Dispatch maps it to none:
   `dispatch issue --project <KEY> --repo <owner/name> --title '<the defect>' --label legion --spec-file -`
   The spec states the defect, where the fix belongs and the fix, and ends with the `Filed for Legion from` line.
5. Reply `Filed dispatch://<NEW KEY>`.
