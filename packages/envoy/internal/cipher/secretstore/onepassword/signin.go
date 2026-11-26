package onepassword

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/sjawhar/envoy/internal/cipher/secretstore"
)

// tokenVariable is the variable the CLI signs in with through the SDK, as 1Password's own tools
// read it: a service account's token. It is never printed, and never part of a printed command.
const tokenVariable = "OP_SERVICE_ACCOUNT_TOKEN"

// accountVariable is the variable op reads the account to use from, where it holds more than one.
const accountVariable = "OP_ACCOUNT"

// OpAccount is whom op is signed in as: a person's email, or a service account.
type OpAccount struct {
	Email          string
	ServiceAccount bool
}

// opWhoami runs `op whoami --format json` through run, under secretstore.CallDeadline, and reads
// whom op is signed in as: user_type SERVICE_ACCOUNT, or no email, is a service account. op
// refuses when it holds no sign-in.
func opWhoami(ctx context.Context, run secretstore.RunChild) (OpAccount, error) {
	ctx, cancel := secretstore.WithCallDeadline(ctx)
	defer cancel()
	out, err := runOp(ctx, run, nil, "whoami", "--format", "json")
	if err != nil {
		return OpAccount{}, fmt.Errorf("op is signed in to no account it can use: sign in with `op signin`, or set %s: %w", tokenVariable, failure("op whoami", err))
	}
	var who struct {
		Email    string `json:"email"`
		UserType string `json:"user_type"`
	}
	if err := json.Unmarshal(out, &who); err != nil {
		return OpAccount{}, fmt.Errorf("op whoami printed output this build cannot read: %w", err)
	}
	return OpAccount{Email: who.Email, ServiceAccount: who.UserType == "SERVICE_ACCOUNT" || who.Email == ""}, nil
}

// sdkSignIn is the CLI signed in through the SDK with OP_SERVICE_ACCOUNT_TOKEN.
type sdkSignIn struct{}

func (sdkSignIn) Identity() string { return tokenVariable + ": a 1Password service account" }

func (sdkSignIn) ChooseAnother() string {
	return "choose another by setting " + tokenVariable + " to another service account's token, or unset it to sign in as op does"
}

// RerunPrefix is none: a printed command runs under the same token only where the person sets it
// again, and the token is never printed.
func (sdkSignIn) RerunPrefix() string { return "" }

func (sdkSignIn) PersonEmail(context.Context) (string, error) {
	return "", errors.New("a service account names no person")
}

// opSignIn is the CLI signed in as op is: account is whom op answered, and opAccount is
// OP_ACCOUNT, the account op was told to use, "" where it chose its own.
type opSignIn struct {
	account   OpAccount
	opAccount string
}

func (s opSignIn) Identity() string {
	if s.account.ServiceAccount {
		return "op: a service account"
	}
	return "op: " + s.account.Email
}

func (opSignIn) ChooseAnother() string {
	return "choose another with " + accountVariable + " (one of op's accounts) or " + tokenVariable + " (a service account's token)"
}

// RerunPrefix is OP_ACCOUNT=<account> where OP_ACCOUNT chose the account, so a printed command
// signs in the same way.
func (s opSignIn) RerunPrefix() string {
	if s.opAccount == "" {
		return ""
	}
	return accountVariable + "=" + secretstore.ShellWord(s.opAccount) + " "
}

func (s opSignIn) PersonEmail(context.Context) (string, error) {
	if s.account.ServiceAccount {
		return "", errors.New("op is signed in as a service account, which names no person")
	}
	return strings.ToLower(s.account.Email), nil
}
