package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/sjawhar/envoy/internal/redact"
	"github.com/sjawhar/envoy/internal/urljoin"
	"github.com/sjawhar/envoy/listenerwire"
)

// runRegister registers every session of the sets through the listener's own route, as an agent's
// plugin does: one POST /v1/interests/subscribe each, the route joined onto --url's path with its
// query kept (urljoin.Join), which writes the interest and, for a self-subscribed session, the
// session record. Each registration asks for the session consumer the listener requires of every
// registration it takes (consumerFor in cmd/listener), with the delivery the listener's own tests
// register with (cmd/listener/api_test.go): a registration with none is refused 409 and writes
// neither record. A --url that is no absolute URL with a host is refused in urljoin's words, which
// quote none of it.
func runRegister(args []string) error {
	flags := flag.NewFlagSet("register", flag.ContinueOnError)
	url := flags.String("url", "", "listener URL")
	spec := flags.String("sets", "", "record sets, name=count[:variant],...")
	role := flags.String("role", "", "role the first session of each set holds")
	workers := flags.Int("concurrency", 8, "requests in flight")
	if err := flags.Parse(args); err != nil {
		return err
	}
	joined, err := urljoin.Join(*url, "/v1/interests/subscribe")
	if err != nil {
		return fmt.Errorf("--url: %w", err)
	}
	subscribe := joined.String()
	sets, err := parseSets(*spec)
	if err != nil {
		return err
	}
	type job struct {
		set   recordSet
		index int
	}
	jobs := make(chan job)
	errs := make(chan error, *workers)
	client := &http.Client{Timeout: 30 * time.Second}
	var wg sync.WaitGroup
	for range *workers {
		wg.Go(func() {
			for j := range jobs {
				body, _ := json.Marshal(listenerwire.SubscribeRequest{
					SessionID:      sessionID(j.set.name, j.index),
					Dir:            "/refill/" + j.set.name,
					Title:          sessionTitle(j.set.name, j.index, j.set.variant),
					Topics:         topics(j.set, j.index, *role),
					SelfSubscribed: true,
					Delivery:       &listenerwire.SubscribeDelivery{Consumer: true, Epoch: "e"},
				})
				resp, err := client.Post(subscribe, "application/json", bytes.NewReader(body))
				if err == nil {
					answer, _ := redact.ReadAnswer(resp, 64<<10)
					_ = resp.Body.Close()
					if resp.StatusCode != http.StatusOK {
						err = fmt.Errorf("subscribe %s: %d %s", sessionID(j.set.name, j.index), resp.StatusCode, answer.Words())
					}
				}
				if err != nil {
					select {
					case errs <- err:
					default:
					}
				}
			}
		})
	}
	started := time.Now()
	n := 0
	for _, set := range sets {
		for index := range set.count {
			jobs <- job{set, index}
			n++
		}
	}
	close(jobs)
	wg.Wait()
	select {
	case err := <-errs:
		return err
	default:
	}
	fmt.Printf("registered %d sessions through %s in %s\n", n, subscribe, time.Since(started).Round(time.Millisecond))
	return nil
}
