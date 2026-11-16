package quotation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/url"
	"time"

	"github.com/GuilhermeOliveira591/mba_arquitetura_desafio_cotacao_seguros/internal/partner"
)

// Cache is what the service needs from the quote cache. A failing cache is never fatal: the service
// treats an error as a miss and goes to the partner.
type Cache interface {
	Get(ctx context.Context, key string) (value []byte, found bool, err error)
	Set(ctx context.Context, key string, value []byte, ttl time.Duration) error
}

// MaxCacheTTL is the commercial ceiling: insurers honor a quoted premium for at most 24 hours.
const MaxCacheTTL = 24 * time.Hour

// CacheKey builds the cache key of one partner's quote for one broker and one risk:
//
//	quote:v1:{tenant_id}:{partner}:{sha256 of the normalized request as JSON, in hex}
//
// The broker comes first and in clear text: it is the isolation contract (the same risk quoted by two
// brokers never shares an entry) and it lets one broker's entries be listed and purged by prefix. The
// risk (document, birth year, plate, model, year, value, coverage) goes in hashed, so no CPF or plate
// is readable in a key name. The request must already be normalized.
func CacheKey(tenant, partnerName string, request Request) string {
	risk, _ := json.Marshal(request)
	sum := sha256.Sum256(risk)
	return "quote:v1:" + url.QueryEscape(tenant) + ":" + partnerName + ":" + hex.EncodeToString(sum[:])
}

type cacheEntry struct {
	Quote     partner.Quote `json:"quote"`
	FetchedAt time.Time     `json:"fetched_at"`
}

// entryTTL is the configured TTL, shortened to the validity the partner declared for this quote.
func entryTTL(configured time.Duration, quote partner.Quote) time.Duration {
	if declared := time.Duration(quote.ValidForSeconds) * time.Second; declared > 0 && declared < configured {
		return declared
	}
	return configured
}
