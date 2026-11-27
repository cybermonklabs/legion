# Merge holds

Read this when a person or your deployment instructions ask for a merge hold or its lift, and on each `tick`. A merge hold stops the merges of one or more repositories of your project, for a deploy that must apply first or another reason they give. The project's holds are one Dispatch document of the project, `merge-holds.md` (slug `merge-holds-md`), one line a hold under its `Holds` heading. Only you write it. No line means no hold.

The architect reads the document before every `sign_off` and every enqueue, and the merger before it merges, so the document is the whole hold: send a hold to no tree, no architect and no role topic. Each change of the document wakes the architect that owns each issue in `accepting` and the merger of each issue in `merging`, held or not. It wakes no role of an issue out of the workflow, of an issue whose tree lingers, or of an issue, such as a root, that waits in `merging` after its pull request merged. It wakes no architect that holds the enqueue of a pull request no issue records: that architect reads the document again at its next wake.

## Write a hold

Write a hold once, as one line, when a person or your deployment instructions ask for it.

1. Read the document: `dispatch doc-read --project <PROJECT> --artifact merge-holds-md`.
   - `document "merge-holds-md" not found by slug`: write the header below to a file and create the document: `dispatch artifact --project <PROJECT> --name merge-holds.md --content-file <file>`.
   - Any other failed read decides nothing: create no document, write no line, and name the failed read with its first line in your summary.
2. Add the line:

   ```bash
   dispatch doc-edit --project <PROJECT> --artifact merge-holds-md \
     --ops-json '[{"op":"insert","after":"end","markdown":"- <repositories> | <reason> | <end>"}]' \
     --summary 'Hold <repositories>: <reason>'
   ```

## Lift a hold

Delete its line:

```bash
dispatch doc-edit --project <PROJECT> --artifact merge-holds-md \
  --ops-json '[{"op":"delete","find":"- <the line as the document reads it>"}]' \
  --summary 'Lift the hold on <repositories>'
```

On each `tick`, read the document and delete each line whose end is past: the passing of a time wakes no held role, and the deletion does. A `not found` answer is a project with no hold. Any other failed read deletes nothing: name it with its first line in your summary, and the next `tick` reads again.

## The document's header

```markdown
# Merge holds

The controller writes this document, and nobody else edits it. Each line under Holds is one
merge hold. An architect reads this document before every sign-off and every enqueue, and a
merger before it merges. A line that names your repository, or `all`, and whose end is not past
holds your sign-off, your enqueue or your merge. No line means no hold.

A line reads `- <repositories> | <reason> | <end>`. `<repositories>` is `all`, or one or more
`owner/name` separated by commas. `<reason>` holds no `|`. `<end>` is an RFC 3339 time in UTC,
or `until the controller lifts it`. The controller lifts a hold by deleting its line.

## Holds
```
