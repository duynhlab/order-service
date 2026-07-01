package database

import (
	"context"
	"testing"
	"time"

	"github.com/duynhlab/order-service/config"
)

// TestConnect_ParseError verifies Connect returns an error when the DSN fails
// to parse. An invalid sslmode makes pgxpool.ParseConfig reject the DSN before
// any network call happens.
func TestConnect_ParseError(t *testing.T) {
	cfg := &config.Config{
		Database: config.DatabaseConfig{
			Host:    "localhost",
			Port:    "5432",
			Name:    "order",
			User:    "order",
			SSLMode: "bogus",
		},
	}

	pool, err := Connect(context.Background(), cfg)
	if err == nil {
		if pool != nil {
			pool.Close()
		}
		t.Fatal("expected parse error for invalid sslmode, got nil")
	}
}

// TestConnect_PingError verifies Connect returns an error when the pool cannot
// reach the database. Port 1 on loopback is not accepting connections, so the
// Ping fails and Connect cleans up the pool.
func TestConnect_PingError(t *testing.T) {
	cfg := &config.Config{
		Database: config.DatabaseConfig{
			Host:           "127.0.0.1",
			Port:           "1",
			Name:           "order",
			User:           "order",
			SSLMode:        "disable",
			MaxConnections: 25,
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	pool, err := Connect(ctx, cfg)
	if err == nil {
		if pool != nil {
			pool.Close()
		}
		t.Fatal("expected ping error for unreachable host, got nil")
	}
}
