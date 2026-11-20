// packages/envoy/cmd/agent-secrets/gcp_source.go
//
// The sources a store command on Google Cloud takes its credential from, in the order it tries
// them (newGCPBackend): a token or a credentials file the environment names, then a gcloud
// configuration or login it chooses, then a file the person set up, then the active gcloud login,
// then the machine's own identity. A source that is absent is skipped; one that is present but
// fails stops the command.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"github.com/sjawhar/envoy/internal/redact"
)

// gcpSource is one Google credential source, in the order newGCPBackend tries them.
type gcpSource int

const (
	// sourceLentToken is the access token CLOUDSDK_AUTH_ACCESS_TOKEN holds.
	sourceLentToken gcpSource = iota
	// sourceCredentialsFile is the credentials file GOOGLE_APPLICATION_CREDENTIALS names.
	sourceCredentialsFile
	// sourceChosenGcloud is the gcloud configuration CLOUDSDK_ACTIVE_CONFIG_NAME names, or the
	// gcloud login CLOUDSDK_CORE_ACCOUNT names.
	sourceChosenGcloud
	// sourceWellKnownFile is gcloud's application_default_credentials.json
	// (wellKnownCredentialsFile).
	sourceWellKnownFile
	// sourceActiveGcloud is the login of gcloud's active configuration.
	sourceActiveGcloud
	// sourceMetadata is the machine's own service account, as the metadata server names it.
	sourceMetadata
)

// name is how a refusal that found no source names s.
func (s gcpSource) name() string {
	switch s {
	case sourceLentToken:
		return "CLOUDSDK_AUTH_ACCESS_TOKEN"
	case sourceCredentialsFile:
		return "GOOGLE_APPLICATION_CREDENTIALS"
	case sourceChosenGcloud:
		return "CLOUDSDK_ACTIVE_CONFIG_NAME and CLOUDSDK_CORE_ACCOUNT"
	case sourceWellKnownFile:
		return wellKnownCredentialsName()
	case sourceActiveGcloud:
		return "the active gcloud account"
	case sourceMetadata:
		return "the metadata server"
	}
	return "an unknown source"
}

// gcloudConfigDir is gcloud's own configuration directory, where gcloud itself looks for it
// (googlecloudsdk/core/config.py): a non-empty CLOUDSDK_CONFIG, else %APPDATA%\gcloud on Windows
// and $HOME/.config/gcloud elsewhere; "" where none of those is set. Go's own Application
// Default Credentials lookup ignores CLOUDSDK_CONFIG, so it is read here rather than there.
func gcloudConfigDir() string {
	if dir := os.Getenv("CLOUDSDK_CONFIG"); dir != "" {
		return dir
	}
	if runtime.GOOS == "windows" {
		if appData := os.Getenv("APPDATA"); appData != "" {
			return filepath.Join(appData, "gcloud")
		}
		return ""
	}
	if home := os.Getenv("HOME"); home != "" {
		return filepath.Join(home, ".config", "gcloud")
	}
	return ""
}

// wellKnownCredentialsFile is the Application Default Credentials file
// `gcloud auth application-default login` writes, in gcloudConfigDir; "" where there is no such
// directory.
func wellKnownCredentialsFile() string {
	dir := gcloudConfigDir()
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, "application_default_credentials.json")
}

// wellKnownCredentialsName is the well-known credentials file as a message names it: by the
// setting or default its directory comes from, never by its path. The file holds a credential,
// and CLOUDSDK_CONFIG, the setting that names its directory, is where a person who pasted the
// credential itself puts it: a value that is no directory (too long a name, or a file) is not
// absent, so the file is read, and its refusal would otherwise print the value.
func wellKnownCredentialsName() string {
	const file = "the well-known credentials file (application_default_credentials.json in "
	switch {
	case os.Getenv("CLOUDSDK_CONFIG") != "":
		return file + "the directory CLOUDSDK_CONFIG names)"
	case runtime.GOOS == "windows":
		return file + `%APPDATA%\gcloud)`
	default:
		return file + "$HOME/.config/gcloud)"
	}
}

// gcloudConfigurations is the name of every gcloud configuration gcloudConfigDir holds
// (configurations/config_<name>), sorted; none where it holds none or cannot be read.
func gcloudConfigurations() []string {
	dir := gcloudConfigDir()
	if dir == "" {
		return nil
	}
	entries, err := os.ReadDir(filepath.Join(dir, "configurations"))
	if err != nil {
		return nil
	}
	var names []string
	for _, entry := range entries {
		if name, ok := strings.CutPrefix(entry.Name(), "config_"); ok && name != "" && !entry.IsDir() {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	return names
}

// gcloudConfigurationList is gcloudConfigurations for a sentence: the names joined with commas,
// or "none".
func gcloudConfigurationList() string {
	if names := gcloudConfigurations(); len(names) > 0 {
		return strings.Join(names, ", ")
	}
	return "none"
}

// gcloudConfigurationAccount is the account the gcloud configuration name itself names: the
// [core] account property of its file in gcloudConfigDir, as gcloud's configparser reads it
// (`key = value` or `key: value`, `#` and `;` comments); "" where it names none. It is read from
// the file, not from `gcloud config list`, which on a Compute Engine VM answers a configuration
// that names no account with the metadata server's service account (gcloud's core/account
// callback), so a person's empty configuration would read as the machine's: measured on a
// developer's Compute Engine VM on 2026-11-15, where `config list --configuration=<name>` for a
// person's configuration named the VM's account and `config configurations describe <name>` named
// none. A file it cannot read is an error, with the system's reason alone (redact.File): the name
// is one this machine holds (newGCPBackend refuses any other first), and the path is gcloud's
// directory's.
func gcloudConfigurationAccount(name string) (string, error) {
	data, err := os.ReadFile(filepath.Join(gcloudConfigDir(), "configurations", "config_"+name))
	if err != nil {
		return "", fmt.Errorf("read the gcloud configuration %s: %w", name, redact.File(err))
	}
	section, account := "", ""
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		switch {
		case line == "" || line[0] == '#' || line[0] == ';':
		case strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]"):
			section = strings.ToLower(strings.TrimSpace(line[1 : len(line)-1]))
		case section == "core":
			if i := strings.IndexAny(line, "=:"); i > 0 && strings.EqualFold(strings.TrimSpace(line[:i]), "account") {
				account = strings.TrimSpace(line[i+1:])
			}
		}
	}
	return account, nil
}

// activeGcloudConfiguration is the configuration gcloud runs under when nothing names one: the
// name active_config holds in gcloudConfigDir, else default, as gcloud itself reads it
// (googlecloudsdk/core/configurations/named_configs.py).
func activeGcloudConfiguration() string {
	if dir := gcloudConfigDir(); dir != "" {
		if data, err := os.ReadFile(filepath.Join(dir, "active_config")); err == nil {
			if name := strings.TrimSpace(string(data)); name != "" {
				return name
			}
		}
	}
	return "default"
}
