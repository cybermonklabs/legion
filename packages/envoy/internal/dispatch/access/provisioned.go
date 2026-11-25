package access

import (
	"context"
	"fmt"
)

// PrivateOffList answers, sorted, the keys of the projects the database holds private that list,
// every project the deployment names as one that may be private (DISPATCH_PRIVATE_PROJECTS, the
// projects it has provisioned as private, and DISPATCH_PRIVATE_BEFORE_FULL_SWITCH, those it makes
// private before their full switch), does not name. Such a project stays private, and only its
// owner's switch to public changes its visibility; the boot, the maintenance sweep and /healthz
// report it (cmd/dispatch). With no private project it answers nothing, whatever the list.
func PrivateOffList(ctx context.Context, q Querier, list []string) ([]string, error) {
	if list == nil {
		list = []string{}
	}
	rows, err := q.Query(ctx, `select key from projects where visibility = 'private' and not (key = any($1)) order by key`, list)
	if err != nil {
		return nil, fmt.Errorf("read the private projects neither DISPATCH_PRIVATE_PROJECTS nor DISPATCH_PRIVATE_BEFORE_FULL_SWITCH names: %w", err)
	}
	defer rows.Close()
	var keys []string
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return nil, fmt.Errorf("read the private projects neither DISPATCH_PRIVATE_PROJECTS nor DISPATCH_PRIVATE_BEFORE_FULL_SWITCH names: %w", err)
		}
		keys = append(keys, key)
	}
	return keys, rows.Err()
}
