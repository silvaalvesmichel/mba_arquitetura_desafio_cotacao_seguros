package quotation

import (
	"fmt"
	"strings"
	"time"

	"github.com/GuilhermeOliveira591/mba_arquitetura_desafio_cotacao_seguros/internal/partner"
)

type Request struct {
	Driver   Driver  `json:"driver"`
	Vehicle  Vehicle `json:"vehicle"`
	Coverage string  `json:"coverage"`
}

type Driver struct {
	Document  string `json:"document"`
	BirthYear int    `json:"birth_year"`
}

type Vehicle struct {
	Plate      string `json:"plate"`
	Model      string `json:"model"`
	Year       int    `json:"year"`
	ValueCents int64  `json:"value_cents"`
}

var coverages = map[string]bool{"comprehensive": true, "third_party": true}

const defaultCoverage = "comprehensive"

func (r *Request) Normalize() error {
	r.Coverage = strings.TrimSpace(r.Coverage)
	if r.Coverage == "" {
		r.Coverage = defaultCoverage
	}

	r.Driver.Document = strings.TrimSpace(r.Driver.Document)
	r.Vehicle.Plate = strings.ToUpper(strings.TrimSpace(r.Vehicle.Plate))
	r.Vehicle.Model = strings.TrimSpace(r.Vehicle.Model)

	switch {
	case r.Driver.Document == "":
		return fmt.Errorf("driver.document is required")
	case r.Driver.BirthYear < 1900:
		return fmt.Errorf("driver.birth_year is required and must be a valid year")
	case r.Vehicle.Plate == "":
		return fmt.Errorf("vehicle.plate is required")
	case r.Vehicle.Year < 1900:
		return fmt.Errorf("vehicle.year is required and must be a valid year")
	case r.Vehicle.ValueCents <= 0:
		return fmt.Errorf("vehicle.value_cents must be greater than zero")
	case !coverages[r.Coverage]:
		return fmt.Errorf("coverage must be comprehensive or third_party")
	}
	return nil
}

type partnerRequest struct {
	Broker string `json:"broker"`
	Request
}

const (
	SourcePartner = "partner"
	SourceCache   = "cache"
)

// Quote is a partner quote as presented to the broker. The premium is always the partner's own:
// either fetched now (source "partner") or fetched earlier for this same broker and risk (source
// "cache"). QuoteID and FetchedAt trace a cached quote back to the consultation that produced it.
type Quote struct {
	partner.Quote
	Source     string    `json:"source"`
	FetchedAt  time.Time `json:"fetched_at"`
	AgeSeconds int64     `json:"age_seconds"`
}

const (
	ReasonCircuitOpen = "circuit_open"
	ReasonTimeout     = "timeout"
	ReasonError       = "error"
)

// Unavailable names a partner that is missing from a degraded response, and why.
type Unavailable struct {
	Partner string `json:"partner"`
	Reason  string `json:"reason"`
}

// Response is complete when every partner is in Quotes. When at least one is missing, Degraded is
// true and Unavailable lists which ones; the quotes that did arrive are still sorted by premium.
type Response struct {
	TenantID    string        `json:"tenant_id"`
	Quotes      []Quote       `json:"quotes"`
	Degraded    bool          `json:"degraded"`
	Unavailable []Unavailable `json:"unavailable,omitempty"`
	ElapsedMs   int64         `json:"elapsed_ms"`
}
