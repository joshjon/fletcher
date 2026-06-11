package events_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/joshjon/fletcher/internal/events"
)

func TestPublishFansOutToSubscribers(t *testing.T) {
	bus := events.NewBus()
	ch1, cancel1 := bus.Subscribe()
	ch2, cancel2 := bus.Subscribe()
	defer cancel1()
	defer cancel2()

	bus.Publish(events.Event{Type: events.TypeSession, Action: "running", ID: "s1", Name: "dev"})

	for _, ch := range []<-chan events.Event{ch1, ch2} {
		select {
		case e := <-ch:
			require.Equal(t, "running", e.Action)
			require.Equal(t, "dev", e.Name)
			require.False(t, e.At.IsZero(), "At is stamped when unset")
		case <-time.After(time.Second):
			t.Fatal("subscriber did not receive the event")
		}
	}
}

func TestCancelledSubscriberStopsReceiving(t *testing.T) {
	bus := events.NewBus()
	ch, cancel := bus.Subscribe()
	cancel()
	bus.Publish(events.Event{Type: events.TypeJob, Action: "succeeded"})
	select {
	case _, ok := <-ch:
		require.False(t, ok, "channel should deliver nothing after cancel")
	default:
		// Nothing buffered: also fine.
	}
}

func TestSlowSubscriberDropsInsteadOfBlocking(t *testing.T) {
	bus := events.NewBus()
	_, cancel := bus.Subscribe() // never read
	defer cancel()

	done := make(chan struct{})
	go func() {
		// Far more events than the subscriber buffer holds.
		for range 1000 {
			bus.Publish(events.Event{Type: events.TypeJob, Action: "running"})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Publish blocked on a slow subscriber")
	}
}
