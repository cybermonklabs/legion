# Merge holds

The project's merge holds are one Dispatch document of the project, `merge-holds.md` (slug `merge-holds-md`), one line a hold under its `Holds` heading. No line means no hold.

## Write a hold

Write each hold once, as one line.

1. Read the document: `dispatch doc-read --project <PROJECT> --artifact merge-holds-md`.
   - On `document "merge-holds-md" not found by slug`, write [the header](#the-documents-header) to a file, and create the document: `dispatch artifact --project <PROJECT> --name merge-holds.md --content-file <file>`.
   - Any other failed read decides nothing. Create no document, write no line, and name the failed read with its first line in your summary.
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

On each `tick`, read the document and delete each line whose end is past. The deletion wakes the held roles, and the passing of a time does not. A `not found` answer is a project with no hold. Any other failed read deletes nothing: name it with its first line in your summary.

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
