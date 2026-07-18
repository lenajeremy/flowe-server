package database

import (
	"log/slog"
	"time"

	"workflow-ai/server/internal/telemetry"

	"gorm.io/gorm"
)

const startKey = "telemetry:query_start"

// slowQueryThreshold: queries slower than this get a WARN log with the SQL
// (placeholders only — the otel gorm plugin strips bound values, and so do we).
const slowQueryThreshold = 200 * time.Millisecond

// InstrumentQueries registers before/after callbacks on every GORM operation
// so each query lands in the flowe.db.query.duration histogram and slow
// queries surface in the logs with their statement.
func InstrumentQueries(db *gorm.DB) {
	register := func(op string, before func(string, func(*gorm.DB)) error, after func(string, func(*gorm.DB)) error) {
		_ = before("telemetry:"+op+":start", func(tx *gorm.DB) {
			tx.InstanceSet(startKey, time.Now())
		})
		_ = after("telemetry:"+op+":done", func(tx *gorm.DB) {
			v, ok := tx.InstanceGet(startKey)
			if !ok {
				return
			}
			start, ok := v.(time.Time)
			if !ok {
				return
			}
			dur := time.Since(start)
			telemetry.RecordDBQuery(tx.Statement.Context, op, dur, tx.Error)
			if dur > slowQueryThreshold {
				slog.WarnContext(tx.Statement.Context, "slow db query",
					"operation", op,
					"duration_ms", dur.Milliseconds(),
					"table", tx.Statement.Table,
					"sql", tx.Statement.SQL.String(),
					"rows", tx.RowsAffected,
				)
			}
		})
	}

	c := db.Callback()
	register("create", c.Create().Before("gorm:create").Register, c.Create().After("gorm:create").Register)
	register("query", c.Query().Before("gorm:query").Register, c.Query().After("gorm:query").Register)
	register("update", c.Update().Before("gorm:update").Register, c.Update().After("gorm:update").Register)
	register("delete", c.Delete().Before("gorm:delete").Register, c.Delete().After("gorm:delete").Register)
	register("row", c.Row().Before("gorm:row").Register, c.Row().After("gorm:row").Register)
	register("raw", c.Raw().Before("gorm:raw").Register, c.Raw().After("gorm:raw").Register)
}
