package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/knoniewicz/kor/client"
	"github.com/knoniewicz/kor/messages"
)

type Kor interface {
	Call(ctx context.Context, subject string, payload any) (*messages.Message, error)
	Publish(subject string, payload any, d messages.PublicationDescriptor) error
}

var _ Kor = (*client.Client)(nil)

type Config struct {
	Dir      string
	EnvAllow []string
	Timeout  time.Duration
	Backoff  time.Duration
}

type Kron struct {
	kor Kor
	cfg Config

	mu   sync.RWMutex
	jobs map[string]*Job
}

func NewKron(k Kor, cfg Config) *Kron {
	if cfg.Timeout == 0 {
		cfg.Timeout = 2 * time.Minute
	}
	if cfg.Backoff == 0 {
		cfg.Backoff = time.Second
	}
	return &Kron{kor: k, cfg: cfg, jobs: map[string]*Job{}}
}

func (k *Kron) getenv(name string) (string, bool) {
	for _, n := range k.cfg.EnvAllow {
		if n == name {
			return os.Getenv(name), true
		}
	}
	return "", false
}

func (k *Kron) call(ctx context.Context, subject string, payload any) (json.RawMessage, error) {
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	m, err := k.kor.Call(cctx, subject, payload)
	if err != nil {
		return nil, err
	}
	return m.Payload, nil
}

func (k *Kron) publish(subject string, payload any) error {
	return k.kor.Publish(subject, payload, messages.PublicationDescriptor{Description: "published by a kron job"})
}

func (k *Kron) write(ctx context.Context, rec map[string]any) (json.RawMessage, error) {
	if _, ok := rec["fetched_at"]; !ok {
		rec["fetched_at"] = time.Now().Format(time.RFC3339)
	}
	delay := k.cfg.Backoff
	var err error
	for attempt := 0; attempt < 4; attempt++ {
		var res json.RawMessage
		res, err = k.call(ctx, "korpus.write", rec)
		if err == nil {
			return res, nil
		}
		var ep messages.ErrorPayload
		if errors.As(err, &ep) && ep.Code != messages.ErrorCodeHandlerNotFound {
			return nil, err
		}
		if ctx.Err() != nil {
			return nil, err
		}
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return nil, err
		}
		delay *= 2
	}
	return nil, err
}

func (k *Kron) ask(ctx context.Context, prompt string, data any, timeout time.Duration) (string, error) {
	if data != nil {
		b, _ := json.Marshal(data)
		prompt += "\n\n" + string(b)
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	m, err := k.kor.Call(cctx, "agent.ask", map[string]string{"prompt": prompt})
	if err != nil {
		return "", err
	}
	var out struct {
		Text string `json:"text"`
	}
	if err := m.DecodePayload(&out); err != nil {
		return "", err
	}
	if strings.TrimSpace(out.Text) == "" {
		return "", errors.New("empty answer")
	}
	return out.Text, nil
}

type RunRecord struct {
	Name       string `json:"name"`
	Kind       string `json:"kind"`
	Started    string `json:"started"`
	DurationMS int64  `json:"duration_ms"`
	OK         bool   `json:"ok"`
	Error      string `json:"error,omitempty"`
}

func (k *Kron) Run(parent context.Context, j *Job, event map[string]any) RunRecord {
	start := time.Now()
	rec := RunRecord{Name: j.Name, Kind: j.Kind, Started: start.Format(time.RFC3339Nano), OK: true}
	if err := j.execute(parent, k, event); err != nil {
		rec.OK, rec.Error = false, err.Error()
		slog.Error("run failed", "job", j.Name, "err", err)
	}
	rec.DurationMS = time.Since(start).Milliseconds()

	wctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := writeAs(wctx, k, "automation.run", fmt.Sprintf("%s@%s", j.Name, rec.Started), rec); err != nil {
		slog.Error("could not write run record", "job", j.Name, "err", err)
	}
	return rec
}

func (k *Kron) job(name string) *Job {
	k.mu.RLock()
	defer k.mu.RUnlock()
	return k.jobs[name]
}

func (k *Kron) list() []JobInfo {
	k.mu.RLock()
	defer k.mu.RUnlock()
	var out []JobInfo
	for _, j := range k.jobs {
		out = append(out, j.Info())
	}
	sortInfos(out)
	return out
}
