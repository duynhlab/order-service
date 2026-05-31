package v1

import (
	"context"
	"fmt"
	"strconv"

	shippingv1 "github.com/duynhlab/pkg/proto/shipping/v1"
	"google.golang.org/grpc"
)

// ShippingGRPCClient fetches shipments from the shipping service over gRPC.
// It implements shipmentFetcher and returns the same *Shipment as the REST
// ShippingClient, so the aggregated order-details response is identical on
// either transport.
type ShippingGRPCClient struct {
	client shippingv1.ShippingServiceClient
}

// NewShippingGRPCClient wraps a gRPC connection (typically from grpcx.Dial).
func NewShippingGRPCClient(conn *grpc.ClientConn) *ShippingGRPCClient {
	return &ShippingGRPCClient{client: shippingv1.NewShippingServiceClient(conn)}
}

// GetShipmentByOrderID calls ShippingService.GetShipmentByOrder. An unset
// shipment in the response means the order has no shipment yet — returned as
// (nil, nil), matching the REST client's HTTP-404 handling.
func (c *ShippingGRPCClient) GetShipmentByOrderID(ctx context.Context, orderID string) (*Shipment, error) {
	resp, err := c.client.GetShipmentByOrder(ctx, &shippingv1.GetShipmentByOrderRequest{OrderId: orderID})
	if err != nil {
		return nil, fmt.Errorf("shipping gRPC call failed: %w", err)
	}

	ps := resp.GetShipment()
	if ps == nil {
		return nil, nil
	}

	return shipmentFromProto(ps), nil
}

// shipmentFromProto maps the protobuf shipment to order's Shipment, identically
// to how the REST client decodes the JSON shipment.
func shipmentFromProto(s *shippingv1.Shipment) *Shipment {
	var estimatedDelivery *string
	if v := s.GetEstimatedDelivery(); v != "" {
		estimatedDelivery = &v
	}

	// IDs are integers in the JSON contract; the proto carries them as strings.
	id, _ := strconv.Atoi(s.GetId())
	orderID, _ := strconv.Atoi(s.GetOrderId())

	return &Shipment{
		ID:                id,
		OrderID:           orderID,
		TrackingNumber:    s.GetTrackingNumber(),
		Carrier:           s.GetCarrier(),
		Status:            s.GetStatus(),
		EstimatedDelivery: estimatedDelivery,
		CreatedAt:         s.GetCreatedAt(),
		UpdatedAt:         s.GetUpdatedAt(),
	}
}
