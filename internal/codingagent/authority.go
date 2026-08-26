package codingagent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"gorm.io/gorm"
)

// acquireAuthorityLock uses the same PostgreSQL advisory-lock namespace as
// hosted agents and integration disconnect/member removal handlers. Holding it
// from credential injection through durable completion makes "authority was
// revoked" a crisp boundary across replicas: removal either commits first and
// this job never starts, or waits for this already-authorized job to finish.
func acquireAuthorityLock(ctx context.Context, db *gorm.DB, organizationID, userID string) (func(), error) {
	if db == nil || organizationID == "" || userID == "" {
		return nil, errors.New("coding agent authority lock identity is empty")
	}
	if db.Dialector.Name() != "postgres" {
		return func() {}, nil
	}
	sqlDB, err := db.DB()
	if err != nil {
		return nil, err
	}
	connection, err := sqlDB.Conn(ctx)
	if err != nil {
		return nil, err
	}
	key := "authority:" + organizationID + ":" + userID
	if _, err := connection.ExecContext(ctx, "SELECT pg_advisory_lock(hashtextextended($1, 0))", key); err != nil {
		_ = connection.Close()
		return nil, fmt.Errorf("acquire coding agent authority lock: %w", err)
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			unlockCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
			defer cancel()
			if _, err := connection.ExecContext(unlockCtx, "SELECT pg_advisory_unlock(hashtextextended($1, 0))", key); err != nil {
				slog.ErrorContext(unlockCtx, "could not release coding agent authority lock", "key", key, "error", err)
			}
			_ = connection.Close()
		})
	}, nil
}
