// Package distinct keeps each string once, in the order it was first added: delivery's issue
// keys, cited issues and session trailers of a pull request. Adding a value costs one set lookup
// however many values the list holds. A scan of the list for each value, which delivery used
// before, cost the square of the distinct values: 193 ms for the 10,943 distinct references a
// 64 KiB pull request body can carry. TestAddingCostsAConstantNumberOfStepsAValue counts the steps.
package distinct

// Strings is a list that keeps each string once, in the order it was first added. seen holds the
// list's members.
type Strings struct {
	list []string
	seen map[string]struct{}
}

// New returns an empty Strings. Its list is empty, not nil: delivery stores a nil attribution
// input as one it never read.
func New() *Strings {
	return &Strings{list: []string{}, seen: map[string]struct{}{}}
}

// Add appends each value the list does not hold yet.
func (s *Strings) Add(values ...string) {
	for _, value := range values {
		if _, held := s.seen[value]; held {
			continue
		}
		s.seen[value] = struct{}{}
		s.list = append(s.list, value)
	}
}

// List is the values, each once, in the order they were first added.
func (s *Strings) List() []string {
	return s.list
}
