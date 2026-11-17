// Package breaker is a three-state circuit breaker with an injectable clock, so every transition can
// be tested without sleeping.
package breaker

import (
	"sync"
	"time"
)

type State int

// The numeric values are what the state gauge exports: the higher, the less traffic reaches the partner.
const (
	Closed State = iota
	HalfOpen
	Open
)

func (s State) String() string {
	switch s {
	case Closed:
		return "closed"
	case HalfOpen:
		return "half_open"
	case Open:
		return "open"
	}
	return "unknown"
}

type Result int

const (
	Success Result = iota
	Failure
	// Ignored is a call that says nothing about the partner's health (the caller gave up, the partner
	// rejected the request itself).
	Ignored
)

// HalfOpenProbes is how many calls the half-open state lets through at a time. One: a recovering
// partner is tested by a single request, never by a burst.
const HalfOpenProbes = 1

type Settings struct {
	// FailureThreshold is the number of consecutive failures that opens the circuit.
	FailureThreshold int
	// OpenFor is how long the circuit stays open before the next call is let through as a probe.
	OpenFor time.Duration
	// Now defaults to time.Now.
	Now func() time.Time
	// OnTransition is called, outside the lock, after every state change.
	OnTransition func(from, to State)
}

type Breaker struct {
	settings Settings

	mu         sync.Mutex
	state      State
	generation uint64
	failures   int
	openedAt   time.Time
	probing    bool
}

func New(settings Settings) *Breaker {
	if settings.Now == nil {
		settings.Now = time.Now
	}
	return &Breaker{settings: settings}
}

// State reports the current state. Once the open period has elapsed the circuit is half-open, waiting
// for its probe, whether or not a request has arrived yet: reading the state is what a gauge does, and
// it must not show "open" for a partner that would already be tested by the next call.
func (b *Breaker) State() State {
	b.mu.Lock()
	from := b.state
	if b.state == Open && b.settings.Now().Sub(b.openedAt) >= b.settings.OpenFor {
		b.move(HalfOpen)
	}
	to := b.state
	b.mu.Unlock()

	b.notify(from, to)
	return to
}

// Allow reports whether the call may reach the partner. When it may, the caller must report the
// outcome through done exactly once. When it may not, the partner must not be called at all.
func (b *Breaker) Allow() (done func(Result), allowed bool) {
	b.mu.Lock()
	from := b.state

	switch b.state {
	case Open:
		if b.settings.Now().Sub(b.openedAt) < b.settings.OpenFor {
			b.mu.Unlock()
			return nil, false
		}
		b.move(HalfOpen)
		b.probing = true
	case HalfOpen:
		if b.probing {
			b.mu.Unlock()
			return nil, false
		}
		b.probing = true
	}

	generation, to := b.generation, b.state
	b.mu.Unlock()
	b.notify(from, to)

	return func(result Result) { b.record(generation, result) }, true
}

func (b *Breaker) record(generation uint64, result Result) {
	b.mu.Lock()
	// A call admitted before the last transition says nothing about the current state.
	if generation != b.generation {
		b.mu.Unlock()
		return
	}
	from := b.state

	switch b.state {
	case Closed:
		switch result {
		case Success:
			b.failures = 0
		case Failure:
			b.failures++
			if b.failures >= b.settings.FailureThreshold {
				b.open()
			}
		}
	case HalfOpen:
		switch result {
		case Success:
			b.move(Closed)
		case Failure:
			b.open()
		case Ignored:
			b.probing = false
		}
	}

	to := b.state
	b.mu.Unlock()
	b.notify(from, to)
}

func (b *Breaker) open() {
	b.move(Open)
	b.openedAt = b.settings.Now()
}

func (b *Breaker) move(to State) {
	b.state = to
	b.generation++
	b.failures = 0
	b.probing = false
}

func (b *Breaker) notify(from, to State) {
	if from != to && b.settings.OnTransition != nil {
		b.settings.OnTransition(from, to)
	}
}
