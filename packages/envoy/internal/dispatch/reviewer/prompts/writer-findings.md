{{define "writer-findings"}}
{{- if .Block -}}
Dispatch keeps your {{.Kind}} {{.ID}} out of its reader's Inbox until you act on it. It shows in its document. A reviewer read it and found {{len .Findings}} {{if eq (len .Findings) 1}}problem{{else}}problems{{end}} for its reader.
{{- else if .Held -}}
Dispatch holds your {{.Kind}} {{.ID}}. No person can see it until you act on it. A reviewer read it and found {{len .Findings}} {{if eq (len .Findings) 1}}problem{{else}}problems{{end}} for its reader.
{{- else -}}
Dispatch posted your {{.Kind}} {{.ID}}. A reviewer read it after the post and found {{len .Findings}} {{if eq (len .Findings) 1}}problem{{else}}problems{{end}} for its reader.
{{- end}}

## The findings
{{range $index, $finding := .Findings}}
{{add $index 1}}. Rule set "{{$finding.RuleSet}}". The words: "{{$finding.Quote}}"
   Why: {{$finding.Why}}
   Fix: {{$finding.Fix}}
{{end}}
{{- if .Rewrite}}
## The rewrite from the reviewer

{{.Rewrite}}
{{- with .NotApplied}}

Dispatch cannot post this rewrite as it is: {{.}}.
{{- end}}
{{end}}
## What to do now
{{if .Held}}
You have {{.TimeLeft}}. Do one of these two things:

- To change the text, run `dispatch held-edit {{.ID}}`. The reviewer reads the changed text again.
- To post the text as you wrote it, run `dispatch held-post {{.ID}} --reason '<reason>'`. In the reason, say why the text is correct for its reader. Dispatch records the reason.

If you do neither in {{.TimeLeft}}, Dispatch posts {{if .PostsRewrite}}the rewrite from the reviewer, marked as rewritten{{else}}your text as you wrote it{{end}}.
{{- else}}
If a finding is correct, change the text{{with .EditCommand}}: run `{{.}}`{{end}}.
{{- end}}
{{end}}

{{define "writer-post-failed"}}
Dispatch did not post your held {{.Kind}} {{.ID}}. Nothing of it was written, and no person can see it.
{{- if .WaitedOn}}
It waited on the held text {{.WaitedOn}}, and Dispatch did not post that text either.
{{- else}}
Its post was refused: {{.Code}}: {{.Message}}
{{- end}}

## What to do now

If the {{.Kind}} is still needed, write it again.
{{end}}

{{define "writer-near-copy"}}
Dispatch posted your held issue {{.ID}}. When it posted, Dispatch checked it again and found that it is a near copy of {{if eq (len .Copies) 1}}an open issue{{else}}open issues{{end}}:
{{range .Copies}}
- {{.Key}}: {{.Title}}
{{- end}}

Nobody waits on a post, so Dispatch did not refuse it. Its record says it went past {{if eq (len .Copies) 1}}that issue{{else}}those issues{{end}}.

## What to do now

If {{.ID}} and {{if eq (len .Copies) 1}}that issue{{else}}one of those issues{{end}} are the same work, fold one into the other: move what one holds into the other, then close the one you emptied.
{{end}}
