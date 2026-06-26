package v1

import (
	"context"
	"errors"
	"testing"

	shippingv1 "github.com/duynhlab/pkg/proto/shipping/v1"
	"google.golang.org/grpc"
)

// stubShippingSvcClient is a fake shippingv1.ShippingServiceClient that returns
// canned responses so the gRPC mapping can be tested without a server.
type stubShippingSvcClient struct {
	resp       *shippingv1.GetShipmentByOrderResponse
	err        error
	gotOrderID string
}

func (s *stubShippingSvcClient) GetShipmentByOrder(_ context.Context, in *shippingv1.GetShipmentByOrderRequest, _ ...grpc.CallOption) (*shippingv1.GetShipmentByOrderResponse, error) {
	s.gotOrderID = in.GetOrderId()
	return s.resp, s.err
}

// Unused by ShippingGRPCClient (read-only fetcher); present to satisfy the interface.
func (s *stubShippingSvcClient) CreateShipment(context.Context, *shippingv1.CreateShipmentRequest, ...grpc.CallOption) (*shippingv1.CreateShipmentResponse, error) {
	return nil, nil
}

func (s *stubShippingSvcClient) CancelShipment(context.Context, *shippingv1.CancelShipmentRequest, ...grpc.CallOption) (*shippingv1.CancelShipmentResponse, error) {
	return nil, nil
}

func TestNewShippingGRPCClient(t *testing.T) {
	if NewShippingGRPCClient(nil) == nil {
		t.Fatal("NewShippingGRPCClient returned nil")
	}
}

func TestShippingGRPCClient_GetShipmentByOrderID(t *testing.T) {
	t.Run("maps the proto shipment and forwards the order id", func(t *testing.T) {
		stub := &stubShippingSvcClient{resp: &shippingv1.GetShipmentByOrderResponse{
			Shipment: &shippingv1.Shipment{
				Id: "5", OrderId: "18", TrackingNumber: "TN1", Carrier: "UPS",
				Status: "shipped", EstimatedDelivery: "2026-07-01", CreatedAt: "c", UpdatedAt: "u",
			},
		}}
		c := &ShippingGRPCClient{client: stub}

		got, err := c.GetShipmentByOrderID(context.Background(), "18")
		if err != nil {
			t.Fatalf("GetShipmentByOrderID err = %v", err)
		}
		if stub.gotOrderID != "18" {
			t.Errorf("forwarded order id = %q, want 18", stub.gotOrderID)
		}
		if got == nil || got.ID != 5 || got.OrderID != 18 || got.TrackingNumber != "TN1" ||
			got.Carrier != "UPS" || got.Status != "shipped" || got.CreatedAt != "c" || got.UpdatedAt != "u" {
			t.Fatalf("mapped shipment = %+v", got)
		}
		if got.EstimatedDelivery == nil || *got.EstimatedDelivery != "2026-07-01" {
			t.Errorf("estimated delivery = %v, want 2026-07-01", got.EstimatedDelivery)
		}
	})

	t.Run("empty estimated delivery maps to a nil pointer", func(t *testing.T) {
		stub := &stubShippingSvcClient{resp: &shippingv1.GetShipmentByOrderResponse{
			Shipment: &shippingv1.Shipment{Id: "1", OrderId: "2"},
		}}
		got, err := (&ShippingGRPCClient{client: stub}).GetShipmentByOrderID(context.Background(), "2")
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if got.EstimatedDelivery != nil {
			t.Errorf("estimated delivery = %q, want nil", *got.EstimatedDelivery)
		}
	})

	t.Run("no shipment yet returns (nil, nil)", func(t *testing.T) {
		stub := &stubShippingSvcClient{resp: &shippingv1.GetShipmentByOrderResponse{}}
		got, err := (&ShippingGRPCClient{client: stub}).GetShipmentByOrderID(context.Background(), "2")
		if got != nil || err != nil {
			t.Fatalf("got (%v, %v), want (nil, nil)", got, err)
		}
	})

	t.Run("gRPC error is wrapped", func(t *testing.T) {
		stub := &stubShippingSvcClient{err: errors.New("boom")}
		_, err := (&ShippingGRPCClient{client: stub}).GetShipmentByOrderID(context.Background(), "2")
		if err == nil {
			t.Fatal("want an error")
		}
	})
}
