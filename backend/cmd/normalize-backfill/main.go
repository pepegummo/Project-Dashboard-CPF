// normalize-backfill drains every registered source table into the canonical
// model in one pass, then exits. Use it after loading a historical dump; the
// server's normalizer keeps up with the live tail on its own.
//
//	go run ./cmd/normalize-backfill
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"iot-dashboard/internal/config"
	"iot-dashboard/internal/database"
	"iot-dashboard/internal/normalizer"
)

func main() {
	config.Load()
	ctx := context.Background()
	if err := database.Connect(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "❌ database: %v\n", err)
		os.Exit(1)
	}
	defer database.Close()

	// A dump-sized backfill scans tens of millions of rows; no request is waiting
	// on it, so give it hours rather than the API's seconds.
	ctx, cancel := context.WithTimeout(ctx, 6*time.Hour)
	defer cancel()

	start := time.Now()
	fmt.Println("🔄  Backfilling canonical model from registered sources…")
	if err := normalizer.RunUntilCaughtUp(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "❌ backfill: %v\n", err)
		os.Exit(1)
	}

	// The refresh POLICY only covers a recent window (7 days for 1h, 30 for 1d),
	// so a dump reaching years back would never be materialized by it. Refresh the
	// whole range once, hourly before daily — the daily rollup reads the hourly one.
	for _, view := range []string{"readings_1h", "readings_1d"} {
		fmt.Printf("↻  refreshing %s over all time…\n", view)
		if _, err := database.Pool.Exec(ctx,
			fmt.Sprintf("CALL refresh_continuous_aggregate('%s', NULL, NULL)", view)); err != nil {
			fmt.Fprintf(os.Stderr, "⚠️  %s not refreshed: %v\n", view, err)
		}
	}
	fmt.Printf("👍 Done in %s\n", time.Since(start).Round(time.Second))
}
