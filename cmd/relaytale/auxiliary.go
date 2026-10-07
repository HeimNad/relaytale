package main

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"relaytale/internal/database"
)

// The returned cleanup runs after synchronous command work has joined. Even
// cancellation reported as success cannot hide loss of the ownership session.
func auxiliaryOperation(ctx context.Context, db *sql.DB, slot string) (context.Context, func(*error), error) {
	if err := database.CheckRuntimePrivileges(ctx, db); err != nil {
		return nil, nil, err
	}
	g, err := database.AcquireAuxiliary(ctx, db, slot)
	if err != nil {
		return nil, nil, errors.New("auxiliary ownership unavailable; inspect runtime and auxiliary owners")
	}
	owned, cancel := g.OfflineContext(ctx)
	return owned, func(result *error) {
		cancel()
		cleanup, done := context.WithTimeout(context.Background(), 5*time.Second)
		defer done()
		if err := g.Release(cleanup); err != nil {
			*result = errors.Join(*result, errors.New("auxiliary ownership retained; confirm process stopped before manual release"))
		}
	}, nil
}
