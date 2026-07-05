package domain

import (
	"math"
	"time"
)

// Money is stored and computed in integer minor units (cents) — exact integer
// arithmetic, no float rounding drift. Conversion to/from a dollars float
// happens only at boundaries (cart ingress, HTTP responses, notifications) via
// MinorUnits / Dollars.

// Order represents an order aggregate. All money fields are minor units (cents).
type Order struct {
	ID        string      `json:"id"`
	UserID    string      `json:"user_id"`
	Status    string      `json:"status"`
	Items     []OrderItem `json:"items"`
	Subtotal  int64       `json:"subtotal"`
	Shipping  int64       `json:"shipping"`
	Total     int64       `json:"total"`
	CreatedAt time.Time   `json:"created_at"`

	// IdempotencyKey dedupes order creation on retry. Server-internal; never serialized.
	IdempotencyKey string `json:"-"`
}

// OrderItem represents an item in an order. Price and Subtotal are minor units.
type OrderItem struct {
	ProductID   string `json:"product_id"`
	ProductName string `json:"product_name"`
	Quantity    int    `json:"quantity"`
	Price       int64  `json:"price"`
	Subtotal    int64  `json:"subtotal"`
}

// MinorUnits converts a dollars amount to integer minor units (cents), rounding
// to the nearest cent. Used at ingress boundaries (e.g. cart prices arriving as
// a float). Inputs are assumed to be 2-decimal dollar values.
func MinorUnits(dollars float64) int64 {
	return int64(math.Round(dollars * 100))
}

// Dollars converts integer minor units (cents) back to a dollars amount for
// display/serialization boundaries (HTTP responses, notification text).
func Dollars(minor int64) float64 {
	return float64(minor) / 100
}

// CreateOrderRequest represents a request to create an order.
//
// Items and prices are NOT trusted from the client: the web layer overwrites
// Items with the authenticated user's cart (server-side source of truth) before
// the order is built, so Items is not bound from the request body (`json:"-"`)
// — any items a client sends are ignored, not parsed. UserID and IdempotencyKey
// are injected from the auth context and the Idempotency-Key header.
type CreateOrderRequest struct {
	UserID         string      `json:"-"`
	IdempotencyKey string      `json:"-"`
	Items          []OrderItem `json:"-"`
	// PaymentMethod is the checkout's opaque payment token (tok_*). Optional —
	// empty means the saga falls back to its demo token. Carried into the
	// workflow input only, never persisted on the order row; the payment
	// service is the authoritative validator and store.
	PaymentMethod string `json:"payment_method"`
}
