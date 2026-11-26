package main

// Shared Korpus record plumbing for kron-owned types (automation.run).
// Korpus writes are last-write-wins upserts; Kron is the single writer for its run records.

import (
	"context"
	"encoding/json"
)

// writeAs marshals v into a data map and upserts it as one kron-owned Korpus record.
func writeAs(ctx context.Context, k *Kron, typ, id string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return err
	}
	_, err = k.write(ctx, map[string]any{"type": typ, "id": id, "data": m, "source": "kron"})
	return err
}
