// Package v1 implements order's first inbound gRPC surface (RFC-0015 P2,
// homelab ADR-018): order.v1/CreateOrder, the checkout confirm handoff. It is
// a thin adapter over the same logic seam the REST endpoint uses
// (logicv1.OrderService.CreateOrder — validate + enrich + atomic insert with
// idempotency-conflict replay), deliberately skipping the live cart re-read:
// the only caller (checkout) has already re-validated items and prices
// against product-service.
//
// Trust model: east-west posture — no per-request user auth; user_id and
// prices are trusted from the caller and the fence is the NetworkPolicy
// admitting only checkout to :9090 (cluster P5). Same accepted posture as
// ReserveStock/CreateShipment/GetCart.
package v1

import (
	"context"
	"errors"
	"math"
	"strconv"
	"time"
	"unicode/utf8"

	enumspb "go.temporal.io/api/enums/v1"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	orderv1 "github.com/duynhlab/pkg/proto/order/v1"

	"github.com/duynhlab/order-service/internal/core/domain"
	"github.com/duynhlab/order-service/internal/fulfillment"
	logicv1 "github.com/duynhlab/order-service/internal/logic/v1"
)

// Caller-input bounds, aligned with the actual schema (000001: product_id
// INTEGER, product_name VARCHAR(255)) so invalid input fails here with
// InvalidArgument instead of mid-transaction with an opaque Internal.
const (
	maxIdempotencyKeyLen = 200
	maxItems             = 200
	maxProductNameRunes  = 255
	maxQuantity          = 10_000
	// maxUnitPriceMinor caps a unit price at 10^12 minor units. With the
	// item/quantity caps the worst-case subtotal is 2×10^18 < MaxInt64, so
	// the enrichment arithmetic cannot overflow.
	maxUnitPriceMinor = 1_000_000_000_000
)

// msgFulfillmentUnavailable answers the caller when the saga kickoff cannot be
// attempted (nil client) or fails: retryable by contract, and generic so no
// Temporal internals leak into the caller's error path.
const msgFulfillmentUnavailable = "fulfillment temporarily unavailable, retry"

// OrderCreator is the slice of the logic layer this server depends on
// (*logicv1.OrderService satisfies it).
type OrderCreator interface {
	CreateOrder(ctx context.Context, req domain.CreateOrderRequest) (*domain.Order, error)
	GetByIdempotencyKey(ctx context.Context, userID, key string) (*domain.Order, error)
}

// Server implements order.v1.OrderService.
type Server struct {
	orderv1.UnimplementedOrderServiceServer

	svc       OrderCreator
	temporal  fulfillment.Starter // nil when Temporal is unavailable at startup
	taskQueue string
}

// NewServer wires the gRPC adapter.
func NewServer(svc OrderCreator, temporal fulfillment.Starter, taskQueue string) *Server {
	return &Server{svc: svc, temporal: temporal, taskQueue: taskQueue}
}

// CreateOrder inserts a pending order and starts the fulfillment saga,
// idempotently by (user_id, idempotency_key). Saga-start semantics (the
// doubt-cycle findings, in order):
//
//   - The kickoff is attempted on fresh AND replayed orders, but ONLY while
//     the order is still "pending" (status gate): a key replayed after the
//     7-day Temporal retention must never re-run the saga on a confirmed
//     order — that would re-charge and re-ship.
//   - The start uses WorkflowIDReusePolicy REJECT_DUPLICATE + the
//     order-fulfillment-<id> dedup id; "already started" (open, or closed
//     within retention) is success — the saga already happened.
//   - Any other start failure (Temporal down/nil) answers Unavailable so the
//     machine caller retries; the retry's replay path heals the zombie.
//     Answering success there would strand a pending order forever, because
//     callers do not retry successes.
func (s *Server) CreateOrder(ctx context.Context, req *orderv1.CreateOrderRequest) (*orderv1.CreateOrderResponse, error) {
	// Server-side bound: never depend on the caller sending a deadline (the
	// DB work would otherwise pin pool connections for as long as a rude
	// client keeps the stream open).
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	if err := validateCreate(req); err != nil {
		return nil, err
	}

	// Idempotency pre-check. A lookup error is Internal — treating it as a
	// miss would widen the conflict window for no benefit.
	existing, err := s.svc.GetByIdempotencyKey(ctx, req.GetUserId(), req.GetIdempotencyKey())
	if err != nil {
		return nil, status.Error(codes.Internal, "order lookup failed")
	}

	if existing != nil {
		// Idempotency fingerprint (Stripe semantics, security review): a
		// "retry" must be the same request. The stored order never persists
		// payment_method, so the fingerprint is the item payload; a reused
		// key with a different basket is a caller bug, not a replay.
		if !matchesExisting(existing, req) {
			return nil, status.Error(codes.FailedPrecondition, "idempotency key reused with a different request")
		}
	}

	order := existing
	if order == nil {
		items := make([]domain.OrderItem, 0, len(req.GetItems()))
		for _, it := range req.GetItems() {
			items = append(items, domain.OrderItem{
				ProductID:   it.GetProductId(),
				ProductName: it.GetProductName(),
				Quantity:    int(it.GetQuantity()),
				Price:       it.GetUnitPriceMinor(),
			})
		}
		order, err = s.svc.CreateOrder(ctx, domain.CreateOrderRequest{
			UserID:           req.GetUserId(),
			Items:            items,
			PaymentMethod:    req.GetPaymentMethod(),
			IdempotencyKey:   req.GetIdempotencyKey(),
			TotalsProvided:   true,
			ShippingFeeMinor: req.GetShippingFeeMinor(),
			TaxMinor:         req.GetTaxMinor(),
			DiscountMinor:    req.GetDiscountMinor(),
		})
		if err != nil {
			if errors.Is(err, logicv1.ErrInvalidOrder) {
				return nil, status.Error(codes.InvalidArgument, "order rejected by validation")
			}
			return nil, status.Error(codes.Internal, "order creation failed")
		}
	}

	// Status gate + idempotent kickoff (see the contract note above).
	if order.Status == "pending" {
		if s.temporal == nil {
			return nil, status.Error(codes.Unavailable, msgFulfillmentUnavailable)
		}
		err := fulfillment.Start(ctx, s.temporal, s.taskQueue, order, req.GetPaymentMethod(),
			fulfillment.Options{ReusePolicy: rejectDuplicate()})
		if err != nil && !errors.Is(err, fulfillment.ErrAlreadyStarted) {
			return nil, status.Error(codes.Unavailable, msgFulfillmentUnavailable)
		}
	}

	return &orderv1.CreateOrderResponse{OrderId: order.ID, Status: order.Status}, nil
}

// validateCreate bounds every caller-controlled field. Error messages are
// generic by design — they must never echo input values (a rejected
// payment_method may be PAN-shaped, and the caller may run this RPC inside a
// Temporal activity whose history would otherwise record it).
func validateCreate(req *orderv1.CreateOrderRequest) error {
	if l := len(req.GetIdempotencyKey()); l == 0 || l > maxIdempotencyKeyLen {
		return status.Error(codes.InvalidArgument, "idempotency_key is required (max 200 chars)")
	}
	if !isInt32(req.GetUserId()) {
		return status.Error(codes.InvalidArgument, "user_id must be a numeric id")
	}
	if n := len(req.GetItems()); n == 0 || n > maxItems {
		return status.Error(codes.InvalidArgument, "items must contain between 1 and 200 entries")
	}
	for _, it := range req.GetItems() {
		if !isInt32(it.GetProductId()) {
			return status.Error(codes.InvalidArgument, "each item needs a numeric product_id")
		}
		if utf8.RuneCountInString(it.GetProductName()) > maxProductNameRunes {
			return status.Error(codes.InvalidArgument, "product_name too long (max 255 chars)")
		}
		if digitCount(it.GetProductName()) >= 12 {
			// Defense-in-depth (same total-digit rule as ValidPaymentToken):
			// a field-swap bug must not smuggle a PAN into the orders DB.
			return status.Error(codes.InvalidArgument, "product_name looks like card data")
		}
		if q := it.GetQuantity(); q < 1 || q > maxQuantity {
			return status.Error(codes.InvalidArgument, "quantity must be between 1 and 10000")
		}
		if p := it.GetUnitPriceMinor(); p < 0 || p > maxUnitPriceMinor {
			return status.Error(codes.InvalidArgument, "unit_price_minor out of range")
		}
	}
	if pm := req.GetPaymentMethod(); pm != "" && !domain.ValidPaymentToken(pm) {
		return status.Error(codes.InvalidArgument, "payment_method must be an opaque tok_ reference")
	}
	// Totals components (P4): each bounded like unit prices; the discount may
	// never exceed what it discounts (items subtotal + fee + tax) — a
	// negative charged total is not a thing.
	var itemsSubtotal int64
	for _, it := range req.GetItems() {
		itemsSubtotal += it.GetUnitPriceMinor() * int64(it.GetQuantity())
	}
	for name, v := range map[string]int64{
		"shipping_fee_minor": req.GetShippingFeeMinor(),
		"tax_minor":          req.GetTaxMinor(),
		"discount_minor":     req.GetDiscountMinor(),
	} {
		if v < 0 || v > maxUnitPriceMinor {
			return status.Error(codes.InvalidArgument, name+" out of range")
		}
	}
	if req.GetDiscountMinor() > itemsSubtotal+req.GetShippingFeeMinor()+req.GetTaxMinor() {
		return status.Error(codes.InvalidArgument, "discount_minor exceeds the order total")
	}
	if k := req.GetIdempotencyKey(); !validKeyAlphabet(k) {
		return status.Error(codes.InvalidArgument, "idempotency_key has invalid characters")
	}
	return nil
}

// matchesExisting compares the replayed request's basket AND its composed
// total to the stored order. Coarse by design — it catches key-reuse bugs
// (different basket, or a different discount/fee/tax under an old key)
// without re-deriving the full enrichment.
func matchesExisting(order *domain.Order, req *orderv1.CreateOrderRequest) bool {
	if len(order.Items) != len(req.GetItems()) {
		return false
	}
	var reqSum, storedSum int64
	for _, it := range req.GetItems() {
		reqSum += it.GetUnitPriceMinor() * int64(it.GetQuantity())
	}
	for _, it := range order.Items {
		storedSum += it.Price * int64(it.Quantity)
	}
	if reqSum != storedSum {
		return false
	}
	reqTotal := reqSum + req.GetShippingFeeMinor() + req.GetTaxMinor() - req.GetDiscountMinor()
	return reqTotal == order.Total
}

// validKeyAlphabet restricts idempotency keys to a token alphabet so nothing
// free-text (PAN-shaped included) can be persisted through this field.
func validKeyAlphabet(s string) bool {
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9', r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z',
			r == ':', r == '_', r == '-':
		default:
			return false
		}
	}
	return true
}

// rejectDuplicate is the reuse policy for the idempotent kickoff: no new
// execution while a previous one with this id exists (open — any policy — or
// closed within the namespace retention).
// Semantics: https://docs.temporal.io/workflow-execution/workflowid-runid
func rejectDuplicate() enumspb.WorkflowIdReusePolicy {
	return enumspb.WORKFLOW_ID_REUSE_POLICY_REJECT_DUPLICATE
}

// digitCount counts decimal digits in s (total, not the longest run).
func digitCount(s string) int {
	n := 0
	for _, r := range s {
		if r >= '0' && r <= '9' {
			n++
		}
	}
	return n
}

// isInt32 reports whether s is a base-10 integer that fits the schema's
// INTEGER columns (orders.user_id, order_items.product_id).
func isInt32(s string) bool {
	n, err := strconv.ParseInt(s, 10, 64)
	return err == nil && n > 0 && n <= math.MaxInt32
}
