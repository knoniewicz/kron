package main

// decision.evaluate is the optional bridge to a d1-style decider (KRON_DECIDER_URL). It is a
// capability, never a dependency: nothing in the scheduler calls it, and an unset or unreachable
// decider returns a typed error so callers can defer or pick an approved alternative. An outage
// must never become a negative result, and model confidence never decides permissions.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

const (
	decideTimeout     = 30 * time.Second
	decideMaxQ        = 8
	decideMaxChoices  = 16
	decideMaxStateLen = 4000
	decideMaxInstrLen = 1000
	decideMaxBody     = 1 << 20
)

type decideQuestion struct {
	Type         string   `json:"type"` // noul (yes/no probability) | choice | score
	Instructions string   `json:"instructions,omitempty"`
	Choices      []string `json:"choices,omitempty"`
	// Criteria is what d1 reads for choice (option -> description) and score (ordered rubric) questions; it is forwarded untouched.
	Criteria json.RawMessage `json:"criteria,omitempty"`
}

type decideIn struct {
	State     string                    `json:"state"`
	Questions map[string]decideQuestion `json:"questions"`
}

type decideOut struct {
	Answers   map[string]json.RawMessage `json:"answers"`
	Model     string                     `json:"model,omitempty"`
	LatencyMS int64                      `json:"latency_ms"`
}

// evaluateDecision validates the bounded questions and forwards them to the decider.
func (k *Kron) evaluateDecision(ctx context.Context, in decideIn) (decideOut, error) {
	url := os.Getenv("KRON_DECIDER_URL")
	if url == "" {
		return decideOut{}, errors.New("decider not configured (KRON_DECIDER_URL is unset)")
	}
	if len(in.Questions) == 0 || len(in.Questions) > decideMaxQ {
		return decideOut{}, fmt.Errorf("need 1..%d questions", decideMaxQ)
	}
	if len(in.State) > decideMaxStateLen {
		return decideOut{}, fmt.Errorf("state too long (%d > %d)", len(in.State), decideMaxStateLen)
	}
	for name, q := range in.Questions {
		switch q.Type {
		case "noul", "choice", "score":
		default:
			return decideOut{}, fmt.Errorf("question %q: unknown type %q", name, q.Type)
		}
		if len(q.Instructions) > decideMaxInstrLen {
			return decideOut{}, fmt.Errorf("question %q: instructions too long", name)
		}
		if len(q.Choices) > decideMaxChoices {
			return decideOut{}, fmt.Errorf("question %q: too many choices", name)
		}
		if q.Type == "choice" && len(q.Choices) < 2 && len(q.Criteria) == 0 {
			return decideOut{}, fmt.Errorf("question %q: choice needs at least 2 options or criteria", name)
		}
		if q.Type == "score" && len(q.Criteria) == 0 {
			return decideOut{}, fmt.Errorf("question %q: score needs criteria", name)
		}
	}
	body, err := json.Marshal(map[string]any{"state": in.State, "questions": in.Questions})
	if err != nil {
		return decideOut{}, err
	}
	cctx, cancel := context.WithTimeout(ctx, decideTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodPost, url+"/decide", bytes.NewReader(body))
	if err != nil {
		return decideOut{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	start := time.Now()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return decideOut{}, fmt.Errorf("decider unreachable: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, decideMaxBody))
	if err != nil {
		return decideOut{}, fmt.Errorf("decider reply unreadable: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return decideOut{}, fmt.Errorf("decider HTTP %d: %.200s", resp.StatusCode, raw)
	}
	var out decideOut
	if err := json.Unmarshal(raw, &out); err != nil {
		return decideOut{}, fmt.Errorf("decider reply is not JSON: %w", err)
	}
	if out.Answers == nil {
		return decideOut{}, errors.New("decider returned no answers")
	}
	out.LatencyMS = time.Since(start).Milliseconds()
	return out, nil
}
