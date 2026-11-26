# The steps of a finding ask's work

## File a clear defect no tree owns

Each reply under the evidence mentions the finder's role, the token after `Clear defect from `. Leave out `--mention` when that token is `controller`:

```bash
dispatch comment --issue '<evidence issue>' --reply-to '<comment id>' --mention 'role:<finder token>' --body-file -
```

1. Check that it is clear: the fix needs no choice that a person must make, and both role GitHub Apps are installed on its repository. Otherwise reply `Not filed: <the choice, or the repository the Apps miss>`, and open nothing. The finder opens its ask.
2. Search first (`skill://dispatch-first`). When an open issue tracks it and a live tree owns that issue (its record's `tree`), file nothing, and reply `Not filed: tracked by dispatch://<KEY>; write to role:<that tree's architect's token>`. When no running tree owns that issue, comment the evidence there, and reply `Filed dispatch://<KEY>`.
3. When a live tree's repository and component match (`skill://legion-controller/references/admission.md` says which trees are live), file nothing, and reply `Not filed: dispatch://<ROOT> owns it; write to role:<its architect's token>`. Build each token as `skill://legion-controller/references/operations.md#a-request-to-another-session` says.
4. When no running tree owns it, file it where the fix belongs: in the project that its repository maps to, or in your own project when Dispatch maps it to none.
   `dispatch issue --project <KEY> --repo <owner/name> --title '<the defect>' --label legion --spec-file -`
   The spec states the defect, where the fix belongs and the fix. It ends with the `Filed for Legion from` line.
5. Reply `Filed dispatch://<NEW KEY>`.
