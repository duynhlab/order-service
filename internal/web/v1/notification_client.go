package v1

import (
	"context"
	"fmt"
	"strconv"

	notificationv1 "github.com/duynhlab/pkg/proto/notification/v1"
	"go.uber.org/zap"
	"google.golang.org/grpc"
)

// NotificationGRPCClient publishes best-effort order notifications to the
// notification service over gRPC. It is never on the critical path of order
// creation — callers log and ignore any error it returns.
type NotificationGRPCClient struct {
	client notificationv1.NotificationServiceClient
}

// NewNotificationGRPCClient wraps a gRPC connection (typically from grpcx.Dial).
func NewNotificationGRPCClient(conn *grpc.ClientConn) *NotificationGRPCClient {
	return &NotificationGRPCClient{client: notificationv1.NewNotificationServiceClient(conn)}
}

// PublishOrderCreated sends an "order placed" notification for a committed
// order. userID is the order owner's id as a string; it is parsed to the int32
// the notification proto expects. A non-numeric userID is reported as an error
// so the caller can log and move on.
func (c *NotificationGRPCClient) PublishOrderCreated(ctx context.Context, userID, orderID string, total float64) error {
	uid, err := strconv.Atoi(userID)
	if err != nil {
		return fmt.Errorf("parse user id %q: %w", userID, err)
	}
	if uid < 0 {
		return fmt.Errorf("invalid user id %q", userID)
	}

	req := &notificationv1.SendEmailRequest{
		UserId:  int32(uid), //nolint:gosec // G115: uid is a DB-issued user id, guarded non-negative above
		To:      "noreply@orders.local",
		Subject: fmt.Sprintf("Order #%s placed", orderID),
		Body:    fmt.Sprintf("Your order #%s for $%.2f has been placed.", orderID, total),
	}
	if _, err := c.client.SendEmail(ctx, req); err != nil {
		return fmt.Errorf("notification gRPC call failed: %w", err)
	}
	return nil
}

// Global notification client (set during init), mirroring the cart client.
var notificationClient *NotificationGRPCClient

func SetNotificationClient(client *NotificationGRPCClient) {
	notificationClient = client
}

func getNotificationClient(logger *zap.Logger) *NotificationGRPCClient {
	if notificationClient == nil && logger != nil {
		logger.Warn("Notification client not initialized")
	}
	return notificationClient
}
