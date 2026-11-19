//go:build ignore

package main

import (
	"fmt"
	"strings"
)

// plantedName is where the planted shapes stand as one more file of the first named package.
const plantedName = "scripts/checks/setting-prints.planted.go"

// planted holds the planted shapes: a line marked `planted: found` must be found, and one marked
// `planted: quiet` must not be; a line that is neither must not be found either.
const planted = `package main

import (
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strings"

	"github.com/sjawhar/envoy/internal/brokertoken"
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

func plantedAccount(env settingValues) {
	account := brokertoken.NewGoogleAccount(env.get("PLANTED_AUDIENCE"))
	if _, err := account.Read(); err != nil {
		slog.Warn("planted", "error", err) // planted: found
	}
}

func plantedRefuse(env settingValues) {
	fmt.Println(pgsetting.Refuse("PLANTED_DATABASE_URL", env.get("PLANTED_DATABASE_URL"))) // planted: quiet
}

func plantedCalls() {
	env := processSettings()
	_ = plantedRefusal(env)
	_ = plantedJudged(env)
	plantedLog(plantedBoot{Issuer: env.get("PLANTED_ISSUER")})
	_ = plantedLength()
	_ = plantedMode(env)
	fmt.Println(plantedParse(os.Getenv("PLANTED_PARSE"))) // planted: found
	plantedAccount(env)
	plantedRefuse(env)
}
`

// plantedBlind is what the planted shapes gave that was not as marked, given the planted lines the
// run found: a line marked found that was not, a line marked quiet that was, and an unmarked line
// that was.
func plantedBlind(found map[int]bool) []string {
	var blind []string
	for i, line := range strings.Split(planted, "\n") {
		n := i + 1
		switch {
		case strings.HasSuffix(line, "// planted: found") && !found[n]:
			blind = append(blind, fmt.Sprintf("the planted line scripts/checks/setting-prints.planted.go:%d was not found", n))
		case strings.HasSuffix(line, "// planted: quiet") && found[n]:
			blind = append(blind, fmt.Sprintf("the planted line scripts/checks/setting-prints.planted.go:%d was found", n))
		case !strings.HasSuffix(line, "// planted: found") && !strings.HasSuffix(line, "// planted: quiet") && found[n]:
			blind = append(blind, fmt.Sprintf("the planted line scripts/checks/setting-prints.planted.go:%d, which is not marked, was found", n))
		}
	}
	return blind
}
