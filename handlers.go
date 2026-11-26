package main

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/knoniewicz/kor/messages"
)

func errNotFound(name string) error { return fmt.Errorf("not_found: no job %q", name) }

func (k *Kron) runs(ctx context.Context, m messages.Message) (any, error) {
	var in runsIn
	if len(m.Payload) > 0 {
		if err := m.DecodePayload(&in); err != nil {
			return nil, err
		}
	}
	if in.Limit <= 0 {
		in.Limit = 20
	}
	q := map[string]any{"type": "automation.run", "order_by": "updated_at", "desc": true, "limit": in.Limit}
	if in.Name != "" {
		q["where"] = map[string]any{"name": in.Name}
	}
	res, err := k.call(ctx, "korpus.query", q)
	if err != nil {
		return nil, err
	}
	var out struct {
		Records []json.RawMessage `json:"records"`
	}
	if err := json.Unmarshal(res, &out); err != nil {
		return nil, err
	}
	return runsOut{Runs: out.Records}, nil
}
