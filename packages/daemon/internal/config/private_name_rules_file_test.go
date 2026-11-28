package config

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/leakguard"
)

// The deployment's private-name rule file is read as postgres_dsn is: LEGION_PRIVATE_NAME_RULES_FILE
// in the daemon's environment first, and `private_name_rules_file` over it, a relative path of the
// key's resolved against the configuration file's directory and the variable's kept as written.
// Neither, or a variable set to the empty string, names none. The setting that named the file is
// kept beside it, so a refusal of the file names what the operator has to change.
func TestPrivateNameRulesFileIsReadFromTheFileOrTheEnvironment(t *testing.T) {
	const (
		fromKey = "/etc/legion/private-names.toml"
		fromVar = "/var/run/legion/private-names.toml"
	)
	for _, tc := range []struct {
		name, line string
		env        map[string]string
		// want is the path, where the configuration file's directory stands for {dir}.
		want, wantNamedBy string
	}{
		{name: "neither"},
		{name: "the variable set empty", env: map[string]string{leakguard.RulesFileVar: ""}},
		{name: "the variable alone", env: map[string]string{leakguard.RulesFileVar: fromVar}, want: fromVar, wantNamedBy: "LEGION_PRIVATE_NAME_RULES_FILE"},
		{name: "the key alone", line: "private_name_rules_file: " + fromKey, want: fromKey, wantNamedBy: "private_name_rules_file"},
		{
			name: "both, the key over the variable", line: "private_name_rules_file: " + fromKey,
			env: map[string]string{leakguard.RulesFileVar: fromVar}, want: fromKey, wantNamedBy: "private_name_rules_file",
		},
		{
			name: "a relative key, against the file's directory", line: "private_name_rules_file: rules/private-names.toml",
			env: map[string]string{leakguard.RulesFileVar: fromVar}, want: "{dir}/rules/private-names.toml", wantNamedBy: "private_name_rules_file",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := minimalFile
			if tc.line != "" {
				body += tc.line + "\n"
			}
			path := writeConfigFile(t, body)

			cfg, err := Load(path, envMap(tc.env))
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			want := strings.ReplaceAll(tc.want, "{dir}", filepath.Dir(path))
			if cfg.PrivateNameRulesFile != want || cfg.PrivateNameRulesNamedBy != tc.wantNamedBy {
				t.Errorf("PrivateNameRulesFile, PrivateNameRulesNamedBy = %q, %q; want %q, %q",
					cfg.PrivateNameRulesFile, cfg.PrivateNameRulesNamedBy, want, tc.wantNamedBy)
			}
		})
	}
}

// A key written blank is refused at load, naming the key, as every other path key is
// (readNonEmptyString), rather than resolved against the configuration file's directory into a
// path that names no rule file, which only the boot would then refuse.
func TestABlankPrivateNameRulesFileIsRefused(t *testing.T) {
	_, err := Load(writeConfigFile(t, minimalFile+"private_name_rules_file: \" \"\n"), envMap(map[string]string{leakguard.RulesFileVar: "/var/run/legion/private-names.toml"}))
	if want := "private_name_rules_file must not be empty"; err == nil || err.Error() != want {
		t.Fatalf("Load = %v, want %q", err, want)
	}
}
