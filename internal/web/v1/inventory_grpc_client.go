package v1

import (
	"context"
	"strings"

	inventoryv1 "github.com/duynhlab/pkg/proto/inventory/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// InventoryGRPCClient is the details-enrichment view of inventory.v1:
// one read, soft-fail, mirroring the payment enrichment client.
type InventoryGRPCClient struct {
	client inventoryv1.InventoryServiceClient
}

// NewInventoryGRPCClient wraps an inventory gRPC connection.
func NewInventoryGRPCClient(conn *grpc.ClientConn) *InventoryGRPCClient {
	return &InventoryGRPCClient{client: inventoryv1.NewInventoryServiceClient(conn)}
}

// GetReservationStatus returns the order's reservation status as a lowercase
// token ("reserved", "committed", ...), or "" when no reservation exists —
// which is the normal state for product-path orders, not an error.
func (c *InventoryGRPCClient) GetReservationStatus(ctx context.Context, orderID string) (string, error) {
	resp, err := c.client.GetReservation(ctx, &inventoryv1.GetReservationRequest{
		ReservationId: orderID,
	})
	if err != nil {
		if status.Code(err) == codes.NotFound {
			return "", nil
		}
		return "", err
	}
	s := resp.GetReservation().GetStatus().String()
	return strings.ToLower(strings.TrimPrefix(s, "RESERVATION_STATUS_")), nil
}
