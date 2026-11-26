package main

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// Kron owns the schemas of the record types it writes. Korpus keeps what it is told,
// so registering on every start is a no-op once a schema is known.
//
//go:embed schemas/*.json
var schemaFS embed.FS

func (k *Kron) registerSchemas(ctx context.Context) error {
	files, err := schemaFS.ReadDir("schemas")
	if err != nil {
		return err
	}
	for _, f := range files {
		raw, err := schemaFS.ReadFile("schemas/" + f.Name())
		if err != nil {
			return err
		}
		typ := strings.TrimSuffix(f.Name(), ".json")
		if err := k.register(ctx, typ, raw); err != nil {
			return fmt.Errorf("register %s: %w", typ, err)
		}
	}
	slog.Info("schemas registered", "count", len(files))
	return nil
}

// register retries while Korpus is not reachable yet.
func (k *Kron) register(ctx context.Context, typ string, schema json.RawMessage) error {
	delay := k.cfg.Backoff
	var err error
	for attempt := 0; attempt < 6; attempt++ {
		if _, err = k.call(ctx, "korpus.register", map[string]any{"type": typ, "schema": schema}); err == nil {
			return nil
		}
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return ctx.Err()
		}
		delay *= 2
	}
	return err
}
