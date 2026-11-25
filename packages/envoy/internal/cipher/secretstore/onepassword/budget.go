package onepassword

import "context"

// Budget is the deployment's daily budget of 1Password requests, which the store spends from
// before each call it makes.
type Budget interface {
	// Spend counts n requests of call and answers whether they were spent; one it did not spend
	// is a call the store does not make.
	Spend(ctx context.Context, call string, n int) (spent bool, err error)
}

// Unlimited is the Budget that never holds: it spends every call and counts nothing.
var Unlimited Budget = unlimited{}

type unlimited struct{}

func (unlimited) Spend(context.Context, string, int) (bool, error) { return true, nil }
