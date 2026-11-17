package partner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/GuilhermeOliveira591/mba_arquitetura_desafio_cotacao_seguros/internal/breaker"
	"github.com/GuilhermeOliveira591/mba_arquitetura_desafio_cotacao_seguros/internal/platform"
)

type Quote struct {
	Partner         string `json:"partner"`
	QuoteID         string `json:"quote_id"`
	PremiumCents    int64  `json:"premium_cents"`
	Currency        string `json:"currency"`
	CoverageCents   int64  `json:"coverage_cents"`
	ValidForSeconds int64  `json:"valid_for_seconds"`
}

// Span attribute set on the request that the breaker kept from reaching the partner.
const AttrShortCircuited = "quotation.breaker.short_circuited"

type Client struct {
	http    *http.Client
	metrics *metrics

	breakerSettings *breaker.Settings
	mu              sync.Mutex
	breakers        map[string]*breaker.Breaker
}

type Option func(*Client)

// WithTimeout caps how long a partner may take to answer. A timeout counts as a failure for the breaker.
func WithTimeout(d time.Duration) Option {
	return func(c *Client) { c.http.Timeout = d }
}

// WithBreaker protects every partner with its own circuit breaker (scope: one per partner name).
func WithBreaker(settings breaker.Settings) Option {
	return func(c *Client) { c.breakerSettings = &settings }
}

func NewClient(options ...Option) *Client {
	c := &Client{
		http:     &http.Client{Transport: platform.InstrumentTransport(http.DefaultTransport)},
		breakers: map[string]*breaker.Breaker{},
	}
	for _, option := range options {
		option(c)
	}
	c.metrics = newMetrics(c.states)
	return c
}

const responseLimit = 1 << 20

func (c *Client) Quote(ctx context.Context, p platform.Partner, request any) (Quote, error) {
	done, allowed := c.allow(p.Name)
	if !allowed {
		span := trace.SpanFromContext(ctx)
		span.SetAttributes(attribute.Bool(AttrShortCircuited, true))
		span.AddEvent("partner call short-circuited: circuit open")
		return Quote{}, &Error{Partner: p.Name, Reason: "circuit open", ShortCircuited: true}
	}

	started := time.Now()
	quote, err := c.call(ctx, p, request)

	result, outcome := classify(ctx, err)
	done(result)
	c.metrics.observe(ctx, p.Name, outcome, time.Since(started))
	return quote, err
}

func (c *Client) call(ctx context.Context, p platform.Partner, request any) (Quote, error) {
	body, err := json.Marshal(request)
	if err != nil {
		return Quote{}, &Error{Partner: p.Name, Reason: fmt.Sprintf("invalid request: %v", err)}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.BaseURL+"/quotes", bytes.NewReader(body))
	if err != nil {
		return Quote{}, &Error{Partner: p.Name, Reason: err.Error()}
	}
	req.Header.Set("Content-Type", "application/json")

	response, err := c.http.Do(req)
	if err != nil {
		return Quote{}, &Error{Partner: p.Name, Reason: err.Error(), Timeout: isTimeout(err), cause: err}
	}
	defer func() { _ = response.Body.Close() }()

	if response.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, responseLimit))
		return Quote{}, &Error{
			Partner: p.Name,
			Status:  response.StatusCode,
			Reason:  fmt.Sprintf("partner replied %d", response.StatusCode),
		}
	}

	var quote Quote
	if err := json.NewDecoder(io.LimitReader(response.Body, responseLimit)).Decode(&quote); err != nil {
		return Quote{}, &Error{
			Partner: p.Name,
			Reason:  fmt.Sprintf("unreadable response: %v", err),
			Timeout: isTimeout(err),
			cause:   err,
		}
	}

	quote.Partner = p.Name
	return quote, nil
}

// allow asks the partner's breaker whether the call may go out. Without a configured breaker every
// call goes out and the outcome is discarded.
func (c *Client) allow(name string) (done func(breaker.Result), allowed bool) {
	if c.breakerSettings == nil {
		return func(breaker.Result) {}, true
	}

	c.mu.Lock()
	b, found := c.breakers[name]
	if !found {
		settings := *c.breakerSettings
		settings.OnTransition = func(from, to breaker.State) {
			log.Printf("quotation-api: breaker of %s went from %s to %s", name, from, to)
			c.metrics.transition(name, from, to)
			if c.breakerSettings.OnTransition != nil {
				c.breakerSettings.OnTransition(from, to)
			}
		}
		b = breaker.New(settings)
		c.breakers[name] = b
	}
	c.mu.Unlock()

	return b.Allow()
}

func (c *Client) states() map[string]breaker.State {
	c.mu.Lock()
	defer c.mu.Unlock()

	states := make(map[string]breaker.State, len(c.breakers))
	for name, b := range c.breakers {
		states[name] = b.State()
	}
	return states
}

// classify turns the outcome of a call into what the breaker counts and what the latency metric labels.
// Timeouts, transport errors, 5xx and unreadable bodies are failures of the partner. A caller that
// went away and a 4xx say nothing about the partner's health.
func classify(ctx context.Context, err error) (breaker.Result, string) {
	if err == nil {
		return breaker.Success, "success"
	}

	var failure *Error
	if !errors.As(err, &failure) {
		return breaker.Failure, "error"
	}
	switch {
	case failure.Timeout:
		return breaker.Failure, "timeout"
	case errors.Is(ctx.Err(), context.Canceled):
		return breaker.Ignored, "canceled"
	case failure.Status >= 400 && failure.Status < 500:
		return breaker.Ignored, "error"
	}
	return breaker.Failure, "error"
}

func isTimeout(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

type Error struct {
	Partner string
	Status  int
	Reason  string
	// ShortCircuited: the breaker was open and the partner was not called.
	ShortCircuited bool
	// Timeout: the partner did not answer within the client timeout.
	Timeout bool

	cause error
}

func (e *Error) Error() string {
	return fmt.Sprintf("partner %s: %s", e.Partner, e.Reason)
}

func (e *Error) Unwrap() error { return e.cause }
