package breaker

import (
	"fmt"
	"testing"
	"time"
)

type fakeClock struct{ now time.Time }

func (c *fakeClock) Now() time.Time          { return c.now }
func (c *fakeClock) advance(d time.Duration) { c.now = c.now.Add(d) }

func newTestBreaker(clock *fakeClock, transitions *[]string) *Breaker {
	return New(Settings{
		FailureThreshold: 5,
		OpenFor:          10 * time.Second,
		Now:              clock.Now,
		OnTransition: func(from, to State) {
			*transitions = append(*transitions, fmt.Sprintf("%s->%s", from, to))
		},
	})
}

func call(t *testing.T, b *Breaker, result Result) {
	t.Helper()
	done, allowed := b.Allow()
	if !allowed {
		t.Fatalf("call rejected in state %s, expected it to go through", b.State())
	}
	done(result)
}

func TestBreakerOpensOnTheFifthConsecutiveFailure(t *testing.T) {
	var transitions []string
	b := newTestBreaker(&fakeClock{now: time.Unix(0, 0)}, &transitions)

	// Four failures, a success, four failures: never five in a row.
	for _, result := range []Result{Failure, Failure, Failure, Failure, Success, Failure, Failure, Failure, Failure} {
		call(t, b, result)
	}
	if b.State() != Closed {
		t.Fatalf("state %s after failures that were never five in a row, expected closed", b.State())
	}

	call(t, b, Failure)
	if b.State() != Open {
		t.Fatalf("state %s after the fifth consecutive failure, expected open", b.State())
	}
	if _, allowed := b.Allow(); allowed {
		t.Fatal("an open circuit let a call through")
	}
	if len(transitions) != 1 || transitions[0] != "closed->open" {
		t.Fatalf("transitions %v, expected [closed->open]", transitions)
	}
}

func TestBreakerWalksThroughTheThreeStates(t *testing.T) {
	var transitions []string
	clock := &fakeClock{now: time.Unix(0, 0)}
	b := newTestBreaker(clock, &transitions)

	for i := 0; i < 5; i++ {
		call(t, b, Failure)
	}

	clock.advance(10*time.Second - time.Nanosecond)
	if _, allowed := b.Allow(); allowed {
		t.Fatal("the circuit let a call through before the open period ended")
	}

	clock.advance(time.Nanosecond)
	probe, allowed := b.Allow()
	if !allowed || b.State() != HalfOpen {
		t.Fatalf("allowed=%v state=%s after the open period, expected a probe in half_open", allowed, b.State())
	}
	if _, second := b.Allow(); second {
		t.Fatalf("half_open let a second call through while the probe was in flight, expected %d", HalfOpenProbes)
	}

	probe(Failure)
	if b.State() != Open {
		t.Fatalf("state %s after a failed probe, expected open", b.State())
	}

	clock.advance(10 * time.Second)
	call(t, b, Success)
	if b.State() != Closed {
		t.Fatalf("state %s after a successful probe, expected closed", b.State())
	}

	expected := []string{"closed->open", "open->half_open", "half_open->open", "open->half_open", "half_open->closed"}
	if fmt.Sprint(transitions) != fmt.Sprint(expected) {
		t.Fatalf("transitions %v, expected %v", transitions, expected)
	}
}

func TestBreakerIgnoresResultsFromBeforeTheLastTransition(t *testing.T) {
	var transitions []string
	clock := &fakeClock{now: time.Unix(0, 0)}
	b := newTestBreaker(clock, &transitions)

	late, _ := b.Allow()
	for i := 0; i < 5; i++ {
		call(t, b, Failure)
	}
	clock.advance(10 * time.Second)
	probe, _ := b.Allow()

	// A call admitted while the circuit was still closed finishes during the probe.
	late(Success)
	if b.State() != HalfOpen {
		t.Fatalf("state %s: a stale result decided the probe", b.State())
	}

	probe(Ignored)
	if _, allowed := b.Allow(); !allowed {
		t.Fatal("an ignored probe did not free the half_open slot")
	}
}
