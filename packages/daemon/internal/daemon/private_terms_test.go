package daemon

import (
	"context"
	"log/slog"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/leakguard"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
)

// The deployment's term list reaches every launch as a launch secret, whole, exactly where the
// daemon's environment holds a term: not where it holds none, and not where it holds only blank
// lines and `key:` lines, which name no term.
func TestTheTermListIsALaunchSecretWhereTheEnvironmentHoldsATerm(t *testing.T) {
	const list = "a-placeholder-term\n\nkey:QUILL\n"
	for _, tc := range []struct {
		name   string
		values map[string]string
		want   string
	}{
		{"a list", map[string]string{leakguard.TermsVar: list}, list},
		{"no variable", nil, ""},
		{"blank and key: lines alone", map[string]string{leakguard.TermsVar: " \nkey:QUILL\n"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			secrets, err := readLaunchSecrets(config.Config{}, lookup(tc.values))
			if err != nil {
				t.Fatalf("readLaunchSecrets: %v", err)
			}
			got, ok := secrets[leakguard.TermsVar]
			if (tc.want != "") != ok || got != tc.want {
				t.Fatalf("the launch secret %s = %q (present %v), want %q", leakguard.TermsVar, got, ok, tc.want)
			}
		})
	}
}

// The boot says once that the deployment hands it no term list, and nothing of it under an
// environment that holds one: run logs it before anything that can stop the boot, so a boot that
// then fails on its port has logged it all the same.
func TestRunLogsTheTermListLineAtBoot(t *testing.T) {
	without := slices.DeleteFunc(os.Environ(), func(entry string) bool {
		return strings.HasPrefix(entry, leakguard.TermsVar+"=")
	})
	for _, tc := range []struct {
		name    string
		environ []string
		want    int
	}{
		{name: "no term list", environ: without, want: 1},
		{name: "the deployment's list", environ: append(slices.Clone(without), leakguard.TermsVar+"=a-placeholder-term"), want: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testConfig(t)
			o := fakeRuntime(fake.NewRuntime(), &built{})
			o.listen = nil
			o.environ = tc.environ
			var logged strings.Builder
			if err := run(context.Background(), cfg, slog.New(slog.NewJSONHandler(&logged, nil)), o); err == nil {
				t.Fatal("run returned no error although another listener held its port")
			}
			if got := strings.Count(logged.String(), privateNameTermsNotConfigured); got != tc.want {
				t.Fatalf("the boot logged the term-list line %d times, want %d; log:\n%s", got, tc.want, logged.String())
			}
		})
	}
}
