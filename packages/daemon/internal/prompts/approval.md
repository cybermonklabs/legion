{{- /*
The approval message: the words that POST /legion/v1/claims/approval answers to the architect of
a tree root while a person's approval of a spec stands (record.StandingSpecApproval). The architect's
plugin adds the message to each model request as it is, and adds no words to it.

Each form is one paragraph. Its first sentence names the version and the document. The rest is the
same in every form, word for word. A tree that carries a copy of another root's gate gets the
carried form: its first sentence names the other root's spec and says that this tree carries its
work, and "deliver it and no other" becomes "deliver what it asks of this tree, and no other".
Where the hand-over's row names no root, the carried form names the document by its id.

The fields are prompts.Approval's: .Tree, .Spec, .Document and .Version.
*/ -}}

{{define "own" -}}
A person has approved version {{.Version}} of the spec of {{.Spec}} (dispatch://{{.Spec}}/spec@v{{.Version}}). Your top responsibility is now to deliver it and no other, {{template "standard"}}
{{- end}}

{{define "carried" -}}
A person has approved version {{.Version}} of the spec of {{.Spec}} (dispatch://{{.Spec}}/spec@v{{.Version}}), whose work this tree, {{.Tree}}, carries. Your top responsibility is now to deliver what it asks of this tree, and no other, {{template "standard"}}
{{- end}}

{{define "carried-no-key" -}}
A person has approved version {{.Version}} of the spec document {{.Document}}, whose work this tree, {{.Tree}}, carries. Your top responsibility is now to deliver what it asks of this tree, and no other, {{template "standard"}}
{{- end}}

{{define "standard" -}}
at the highest standard. The team will bring reasons to change it. Be prepared to refuse them, and to help find a way to keep the goal and the spec whole. A change to what the spec delivers goes to the person as one decision before any work on it, and a requirement the work missed goes back to the work.
{{- end}}
