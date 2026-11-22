package projectaccesstest

import (
	"context"
	"sync"

	"github.com/sjawhar/envoy/internal/cipher/projectaccess"
)

// Swappable is a projectaccess.Access a test points at a fresh client of a stand-in while the
// broker runs: a new client holds none of the old one's cached answers (projectaccess.CacheTTL),
// so the switch is how a test plays an owner's change, a lost project or an outage that the
// client's cache would otherwise hide. It is held under a lock because the broker's requests read
// it while a test replaces it. The zero value is not usable; call Serve before the broker reads it.
type Swappable struct {
	mu     sync.Mutex
	client projectaccess.Access
}

// Serve points s at a fresh client of dispatch.
func (s *Swappable) Serve(dispatch *Server) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.client = projectaccess.New(dispatch.URL, dispatch.Account, nil)
}

func (s *Swappable) current() projectaccess.Access {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.client
}

// Access answers from the client s is pointed at now.
func (s *Swappable) Access(ctx context.Context, project, person string) (projectaccess.Answer, error) {
	return s.current().Access(ctx, project, person)
}

// MemberKind answers from the client s is pointed at now.
func (s *Swappable) MemberKind(ctx context.Context, person string) (string, error) {
	return s.current().MemberKind(ctx, person)
}

// States answers from the client s is pointed at now.
func (s *Swappable) States(ctx context.Context, projects []string) (map[string]projectaccess.Answer, error) {
	return s.current().States(ctx, projects)
}
