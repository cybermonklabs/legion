// packages/envoy/cmd/cipher/tag_printed_test.go
//
// What the secret forms print about a tag flag's value, or a NAME, they refuse: the flag and the
// rule the value broke, never the value. A flag's value is where a person who pasted a credential
// in the wrong place puts it, and the broker's tag parsers (internal/cipher/policy) quote each
// entry they refuse, so the forms say a refusal in words of their own.
package main

import "testing"

// TestARefusedTagValueOrNameIsNotPrinted: each tag flag's rule, and the NAME a form takes, refuses
// a made-up secret put where a person types the value, naming the flag (or NAME) and its rule,
// and no part of the secret. Control: each rule still takes a value it accepts.
func TestARefusedTagValueOrNameIsNotPrinted(t *testing.T) {
	// The secret holds a '.', which no GitHub login holds, an upper-case letter, which no scope
	// name or email entry holds, and lower case, which no secret NAME holds.
	const secret = "hunter2pw-Tags.7c3e91"
	refusals := []struct {
		name  string
		check func() error
		says  string
	}{
		{"--owner", func() error { _, err := checkOwnerTier(secret, ""); return err }, "--owner is me, shared, a person's email or group:EMAIL, a group's lowercase email (the value is not printed)"},
		{"--owner, a group whose address is not lowercase", func() error { _, err := checkOwnerTier("group:"+secret+"@example.com", ""); return err }, "--owner group:EMAIL names a group by its lowercase email"},
		{"--tier", func() error { _, err := checkOwnerTier("", secret); return err }, "--tier is agent or human (the value is not printed)"},
		{"--type", func() error { return checkType(secret) }, "--type is "},
		{"--github-owners, an entry that is no login", func() error { _, err := checkGitHubOwners(secret, false); return err }, "--github-owners: github-owners names GitHub logins"},
		{"--github-owners, none beside a login", func() error { _, err := checkGitHubOwners("none "+secret, true); return err }, "--github-owners: none removes the tag and is given alone"},
		{"--github-owners, none on create", func() error { _, err := checkGitHubOwners("none "+secret, false); return err }, "--github-owners: none names no GitHub account"},
		{"--requesters, an entry that is neither", func() error { _, err := checkRequesters(secret+"@example.com", false); return err }, "--requesters: requesters names people by their lowercase sign-in email"},
		{"--requesters, none beside an entry", func() error { _, err := checkRequesters("none "+secret, true); return err }, "--requesters: none removes the tag and is given alone"},
		{"--requesters, none on create", func() error { _, err := checkRequesters("none "+secret, false); return err }, "--requesters: none names no person or service"},
		{"--scopes, an entry that is no scope name", func() error { _, err := checkScopes(secret, false); return err }, "--scopes: scopes names the part of a Google scope"},
		// The secret in lower case passes a scope name's pattern, so the list fails on its repeat.
		{"--scopes, a scope given twice", func() error {
			_, err := checkScopes("hunter2pw-tags.7c3e91 hunter2pw-tags.7c3e91", false)
			return err
		}, "--scopes: scopes names each scope once"},
		{"--scopes, none beside a scope", func() error { _, err := checkScopes("none "+secret, true); return err }, "--scopes: none removes the tag and is given alone"},
		{"--scopes, none on create", func() error { _, err := checkScopes("none "+secret, false); return err }, "--scopes: none names no OAuth scope"},
		{"--admin on create", func() error { return checkAdmin(secret, false) }, "--admin is true (the value is not printed)"},
		{"--admin on retag", func() error { return checkAdmin(secret, true) }, "--admin is true or none (the value is not printed)"},
		{"--default-grant", func() error { return checkDefaultGrant(secret, false) }, "--default-grant is session (the value is not printed)"},
		{"NAME", func() error { _, _, err := secretName([]string{secret}); return err }, "NAME (not printed): "},
	}
	for _, row := range refusals {
		t.Run(row.name, func(t *testing.T) {
			err := row.check()
			if err == nil {
				t.Fatal("the rule took the secret")
			}
			wantNamedNotPrinted(t, "the refusal", err.Error(), []string{row.says}, []string{secret, "hunter2pw", "7c3e91"}...)
		})
	}
	t.Run("control: each rule takes a value it accepts", func(t *testing.T) {
		if owner, err := checkOwnerTier("Ada@Example.com", "agent"); err != nil || owner != "ada@example.com" {
			t.Errorf("checkOwnerTier = %q, %v; want ada@example.com", owner, err)
		}
		if owner, err := checkOwnerTier("group:approvers@example.com", "agent"); err != nil || owner != "group:approvers@example.com" {
			t.Errorf("checkOwnerTier = %q, %v; want group:approvers@example.com", owner, err)
		}
		for name, err := range map[string]error{
			"--type":          checkType("github-app"),
			"--admin":         checkAdmin("true", false),
			"--default-grant": checkDefaultGrant("session", false),
		} {
			if err != nil {
				t.Errorf("%s refused a value it takes: %v", name, err)
			}
		}
		for name, check := range map[string]func() (string, error){
			"--github-owners": func() (string, error) { return checkGitHubOwners("Ada octocat", false) },
			"--requesters":    func() (string, error) { return checkRequesters("ada@example.com", false) },
			"--scopes":        func() (string, error) { return checkScopes("bigquery", false) },
		} {
			if parsed, err := check(); err != nil || parsed == "" {
				t.Errorf("%s = %q, %v; want the list as the broker reads it", name, parsed, err)
			}
		}
		if name, slug, err := secretName([]string{"SCRATCH_TEST"}); err != nil || name != "SCRATCH_TEST" || slug == "" {
			t.Errorf("secretName = %q, %q, %v; want SCRATCH_TEST and its slug", name, slug, err)
		}
	})
}

// TestARefusedGitHubNarrowingFlagIsNotPrinted: the exec form's --github-owner and --github-repo
// refuse a value that is no GitHub login or repository name, naming the flag and its rule and no
// part of the value, which the broker's parser (policy.ParseGitHubNarrowing) quotes. Control: a
// login and a repository name still narrow.
func TestARefusedGitHubNarrowingFlagIsNotPrinted(t *testing.T) {
	const secret = "hunter2pw.narrow-77aa"
	for _, row := range []struct {
		name, owner string
		repos       []string
		says        string
	}{
		{"--github-owner that is no login", secret, nil, "--github-owner names one GitHub login (the value is not printed)"},
		{"--github-repo that is no repository name", "ada", []string{secret + "/x"}, "--github-repo names at least one and at most 100 of the owner's repositories"},
	} {
		t.Run(row.name, func(t *testing.T) {
			_, err := githubNarrowing(row.owner, true, row.repos, false, false)
			if err == nil {
				t.Fatal("the flags narrowed")
			}
			wantNamedNotPrinted(t, "the refusal", err.Error(), []string{row.says}, secret, "hunter2pw", "77aa")
		})
	}
	t.Run("control: a login and a repository name narrow", func(t *testing.T) {
		got, err := githubNarrowing("Ada", true, []string{"proofs"}, true, false)
		if err != nil || got == nil || got.Owner != "ada" || len(got.Repositories) != 1 || got.Repositories[0] != "proofs" {
			t.Fatalf("githubNarrowing = %+v, %v; want ada's proofs", got, err)
		}
	})
}
