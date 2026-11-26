package eval

// render.go gives the bar's renderer, going/chips, the items of a try's store and of the seed: a
// Store answers items for a try's store after its run; a Seed for the seed as it stands before any
// task, which the calibration answers of labels.json are read against.

// IssueChip is the issue's status and title in the store.
func (s *Store) IssueChip(key string) (string, string, bool) {
	issue, found := s.Issues[key]
	return issue.Status, issue.Title, found
}

// DocumentName is the name of the document or file of owner, an issue's key or a project's key,
// with slug, in the store.
func (s *Store) DocumentName(owner, slug string) (string, bool) {
	for _, d := range s.Documents {
		if d.Slug == slug && (d.Issue == owner || (d.Issue == "" && d.Project == owner)) {
			return d.Name, true
		}
	}
	return "", false
}

// ItemText is the question of the ask, or the text of the comment or of the message on issue,
// stored under id.
func (s *Store) ItemText(kind, issue, id string) (string, bool) {
	switch kind {
	case "ask":
		ask, found := s.Asks[id]
		return ask.Question, found
	case "comment":
		comment, found := s.Comments[id]
		return comment.Body, found
	case "message":
		message, found := s.Messages[id]
		return message.Body, found && message.Issue == issue
	}
	return "", false
}

// IssueChip is the seeded issue's status and title.
func (s *Seed) IssueChip(key string) (string, string, bool) {
	issue, found := s.Issue(key)
	return issue.Status, issue.Title, found
}

// DocumentName is the name of the seeded document or file of owner with slug.
func (s *Seed) DocumentName(owner, slug string) (string, bool) {
	for _, a := range s.Artifacts {
		if a.Owner == owner && a.Slug == slug {
			return a.Name, true
		}
	}
	return "", false
}

// ItemText is the question of the seeded ask, or the text of the seeded comment or of the message
// on issue, that Dispatch stores under id (ItemID), the comments WriteSeed writes for the seed's
// links of kind relates included.
func (s *Seed) ItemText(kind, issue, id string) (string, bool) {
	switch kind {
	case "ask":
		for _, a := range s.Asks {
			if ItemID("ask", a.ID) == id {
				return a.Question, true
			}
		}
	case "comment":
		for _, c := range s.Comments {
			if ItemID("comment", c.ID) == id {
				return c.Body, true
			}
		}
		for _, link := range s.Links {
			if c := relatesComment(link); link.Kind == LinkRelates && ItemID("comment", c.ID) == id {
				return c.Body, true
			}
		}
	case "message":
		for _, m := range s.Messages {
			if ItemID("message", m.ID) == id && m.Issue == issue {
				return m.Body, true
			}
		}
	}
	return "", false
}
