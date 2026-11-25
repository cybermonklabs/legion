package main

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// One handover's grant probe gets no answer. A second caller that loses its receive term must
// return without waiting for the first handover's context to end.
func TestATakeoverGrantWaiterStopsWhenItsContextEnds(t *testing.T) {
	uri := testnats.URL(t)
	client, err := bus.ConnectOwningStream([]string{uri}, bus.WithReplicas(1))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)
	consumer, _ := bus.SessionConsumerName("envoy-consumer-grant-probe")
	relay := testnats.StartUnansweredRelayOn(t, uri, "$JS.API.CONSUMER.INFO."+bus.Stream+"."+consumer)
	delivery, err := connectSessionDelivery([]string{relay.URL()}, 0, client, logging.NewWithWriter("test", io.Discard))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(delivery.close)
	relay.Arm(2)
	first, stopFirst := context.WithCancel(testwait.Context(t))
	defer stopFirst()
	probe := make(chan bool, 1)
	go func() { probe <- delivery.holdsConsumerGrant(first) }()
	testwait.Eventually(t, "the first grant probe to be sent and left unanswered", func() bool { return relay.Sent() == 1 })

	second, stopSecond := context.WithCancel(t.Context())
	stopSecond()
	answer := make(chan bool, 1)
	go func() { answer <- delivery.holdsConsumerGrant(second) }()
	select {
	case granted := <-answer:
		if granted {
			t.Fatal("the canceled grant waiter returned true while the probe drew no answer")
		}
	case <-time.After(time.Second):
		t.Fatal("the canceled grant waiter still waits for the first handover's outstanding grant probe")
	case <-probe:
		t.Fatal("the first probe returned before its context ended")
	}
}
