//go:build ignore

package main

import (
	"fmt"
	"strings"
)

// plant is one file of planted shapes, read as one more file of the module's package in dir: a
// line marked `planted: found` must be found, and one marked `planted: quiet` must not be; a line
// that is neither must not be found either.
type plant struct {
	name string // where its lines are named, relative to the root
	dir  string // its package's directory, relative to the module
	text string
}

// plants are the planted files: Dispatch's command's; the broker's, where a URL setting held in a
// field reached a line no file of Dispatch's command holds (brokertoken's metadata read); a helper
// of a package the commands share, which keeps what it is handed in a variable of its package, so
// one command's setting stays out of another command's print of its answer and the listener's own
// setting is found there; and a package no command imports, whose print the extra unit finds.
var plants = []plant{
	{name: "scripts/checks/setting-prints.planted.go", dir: "cmd/dispatch", text: `package main

import (
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strings"

	"github.com/sjawhar/envoy/internal/fold"
	"github.com/sjawhar/envoy/internal/redact"
	"github.com/sjawhar/envoy/pgsetting"
)

type plantedBoot struct{ Issuer string }

func plantedRefusal(env settingValues) error {
	value := strings.TrimSpace(env.get("PLANTED_URL"))
	if _, err := url.Parse(value); err == nil {
		return nil
	}
	return fmt.Errorf("PLANTED_URL=%q is refused", value) // planted: found
}

func plantedJudged(env settingValues) error {
	return fmt.Errorf("PLANTED_URL=%q is refused", redact.URL(env.get("PLANTED_URL"))) // planted: quiet
}

func plantedLog(boot plantedBoot) {
	_, _ = url.Parse(boot.Issuer)
	slog.Info("planted", "issuer", boot.Issuer) // planted: found
}

func plantedLength() error {
	return errors.New("PLANTED_URL is " + fmt.Sprint(len(os.Getenv("PLANTED_URL"))) + " long") // planted: quiet
}

func plantedMode(env settingValues) error {
	return fmt.Errorf("PLANTED_MODE=%q is no mode", env.get("PLANTED_MODE")) // planted: quiet
}

func plantedParse(raw string) error {
	_, err := url.Parse(raw)
	return err
}

func plantedRefuse(env settingValues) {
	fmt.Println(pgsetting.Refuse("PLANTED_DATABASE_URL", env.get("PLANTED_DATABASE_URL"))) // planted: quiet
}

func plantedShared(env settingValues) {
	value := env.get("PLANTED_SHARED_URL")
	_, _ = url.Parse(value)
	fmt.Println("shared " + fold.PlantedRemember(value)) // planted: found
}

func plantedCalls() {
	env := processSettings()
	_ = plantedRefusal(env)
	_ = plantedJudged(env)
	plantedLog(plantedBoot{Issuer: env.get("PLANTED_ISSUER")})
	_ = plantedLength()
	_ = plantedMode(env)
	fmt.Println(plantedParse(os.Getenv("PLANTED_PARSE"))) // planted: found
	plantedRefuse(env)
	plantedShared(env)
}
`},
	{name: "scripts/checks/setting-prints.planted-broker.go", dir: "cmd/broker", text: `package main

import (
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"

	"github.com/sjawhar/envoy/internal/fold"
	"github.com/sjawhar/envoy/pgsetting"
)

type plantedAccount struct{ Audience string }

func (p *plantedAccount) read() (string, error) {
	return "", fmt.Errorf("read the identity token for %s: %w", p.Audience, errors.New("refused")) // planted: found
}

func plantedBrokerCalls() {
	audience := os.Getenv("PLANTED_AUDIENCE")
	if _, err := url.Parse(audience); err != nil {
		return
	}
	account := &plantedAccount{Audience: audience}
	if _, err := account.read(); err != nil {
		slog.Warn("planted", "error", err)
	}
	fmt.Println(pgsetting.Refuse("PLANTED_DATABASE_URL", os.Getenv("PLANTED_DATABASE_URL"))) // planted: quiet
	fmt.Println("plain " + fold.PlantedRemember("plain text")) // planted: quiet
}

type plantedPlainNamer struct{ audience string }

func (n plantedPlainNamer) PlantedName() string { return "plain" }

func plantedNamer() {
	audience := os.Getenv("PLANTED_NAMER_URL")
	_, _ = url.Parse(audience)
	var namer fold.PlantedNamer = plantedPlainNamer{audience: audience}
	fmt.Println("namer " + namer.PlantedName()) // planted: quiet
}
`},
	{name: "scripts/checks/setting-prints.planted-fold.go", dir: "internal/fold", text: `package fold

var plantedLast string

// PlantedRemember keeps what it is handed in a variable of its package, and answers it.
func PlantedRemember(text string) string {
	plantedLast = text
	return plantedLast
}

// PlantedNamer is an interface the commands' planted files implement.
type PlantedNamer interface{ PlantedName() string }
`},
	{name: "scripts/checks/setting-prints.planted-listener.go", dir: "cmd/listener", text: `package main

import (
	"fmt"
	"net/url"
	"os"

	"github.com/sjawhar/envoy/internal/fold"
)

func plantedListenerShared() {
	value := os.Getenv("PLANTED_LISTENER_URL")
	_, _ = url.Parse(value)
	fmt.Println("shared " + fold.PlantedRemember(value)) // planted: found
}

type plantedSettingNamer struct{ value string }

func (n plantedSettingNamer) PlantedName() string { return n.value }

func plantedListenerNamer() {
	value := os.Getenv("PLANTED_LISTENER_NAMER_URL")
	_, _ = url.Parse(value)
	var namer fold.PlantedNamer = plantedSettingNamer{value: value}
	fmt.Println("namer " + namer.PlantedName()) // planted: found
}
`},
	{name: "scripts/checks/setting-prints.planted-orphan.go", dir: "urlsetting", text: `package urlsetting

import (
	"fmt"
	"net/url"
	"os"
)

func plantedOrphan() {
	value := os.Getenv("PLANTED_ORPHAN_URL")
	_, _ = url.Parse(value)
	fmt.Println("orphan " + value) // planted: found
}
`},
}

// plantedBlind is what the planted shapes gave that was not as marked, given the planted lines the
// run found in each planted file: a line marked found that was not, a line marked quiet that was,
// and an unmarked line that was.
func plantedBlind(found map[string]map[int]bool) []string {
	var blind []string
	for _, p := range plants {
		for i, line := range strings.Split(p.text, "\n") {
			n := i + 1
			switch {
			case strings.HasSuffix(line, "// planted: found") && !found[p.name][n]:
				blind = append(blind, fmt.Sprintf("the planted line %s:%d was not found", p.name, n))
			case strings.HasSuffix(line, "// planted: quiet") && found[p.name][n]:
				blind = append(blind, fmt.Sprintf("the planted line %s:%d was found", p.name, n))
			case !strings.HasSuffix(line, "// planted: found") && !strings.HasSuffix(line, "// planted: quiet") && found[p.name][n]:
				blind = append(blind, fmt.Sprintf("the planted line %s:%d, which is not marked, was found", p.name, n))
			}
		}
	}
	return blind
}

// planted reports whether file is a planted file's name.
func planted(file string) bool {
	for _, p := range plants {
		if p.name == file {
			return true
		}
	}
	return false
}
