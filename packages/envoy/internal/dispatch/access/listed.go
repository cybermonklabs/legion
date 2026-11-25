package access

import (
	"context"
	"fmt"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// PersonDirectlyListedSQL is true when some project names the person personParam binds directly:
// as its owner, or as a person on its access list. It has no group arm, by design: a restricted
// member is admitted only by being named directly (v24 R2), so outside sign-in rests on this
// read alone, whatever any group holds.
func PersonDirectlyListedSQL(personParam string) string {
	return fragment("PersonDirectlyListedSQL", fmt.Sprintf(`(exists (select 1 from projects where owner = %[1]s offset 0)
		or exists (select 1 from project_access where principal_kind = 'person' and principal = %[1]s offset 0))`, personParam))
}

// Listed reports whether some project names email directly, compared in its stored form: trimmed
// and lowercase. Its error, as MemberKind's, never names the email.
func Listed(ctx context.Context, q Querier, email string) (bool, error) {
	var listed bool
	login := model.CanonicalLogin(email)
	if err := q.QueryRow(ctx, `select `+PersonDirectlyListedSQL("$1"), login).Scan(&listed); err != nil {
		return false, fmt.Errorf("read whether any project names a person directly: %w", err)
	}
	return listed, nil
}
