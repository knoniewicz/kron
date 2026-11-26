package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/knoniewicz/kor/messages"
	"github.com/robfig/cron/v3"
)

func bytesReader(b []byte) io.Reader    { return bytes.NewReader(b) }
func jsonMarshal(v any) ([]byte, error) { return json.Marshal(v) }

type Scheduler struct {
	k   *Kron
	sub func(ctx context.Context, subject string, h func(messages.Message)) error
	ctx context.Context

	mu         sync.Mutex
	cron       *cron.Cron
	subscribed map[string]bool
	first      bool
}

func NewScheduler(ctx context.Context, k *Kron, sub func(context.Context, string, func(messages.Message)) error) *Scheduler {
	return &Scheduler{
		k: k, sub: sub, ctx: ctx, subscribed: map[string]bool{}, first: true,
	}
}

func (s *Scheduler) Reload() error {
	jobs, err := loadDir(s.k.cfg.Dir)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cron != nil {
		s.cron.Stop()
	}
	c := cron.New(cron.WithChain(cron.Recover(cron.DefaultLogger)))
	for _, j := range jobs {
		spec, _ := j.Spec()
		if spec != "" {
			c.AddFunc(spec, func() { s.runScheduled(j) })
		}
		if j.On != "" && !s.subscribed[j.On] {
			pattern := j.On
			if err := s.sub(s.ctx, pattern, func(m messages.Message) { s.dispatch(pattern, m) }); err != nil {
				slog.Error("subscribe failed", "on", pattern, "err", err)
				continue
			}
			s.subscribed[pattern] = true
		}
	}
	s.k.mu.Lock()
	s.k.jobs = jobs
	s.k.mu.Unlock()
	c.Start()
	s.cron = c
	slog.Info("jobs loaded", "count", len(jobs))
	if s.first {
		s.first = false
		for _, j := range jobs {
			if j.At != "" && j.Catchup {
				go s.catchup(j)
			}
		}
	}
	return nil
}

func (s *Scheduler) runScheduled(j *Job) {
	if !j.mu.TryLock() {
		slog.Warn("skipping run, previous still running", "job", j.Name)
		return
	}
	j.mu.Unlock()
	s.k.Run(s.ctx, j, nil)
}

func (s *Scheduler) dispatch(pattern string, m messages.Message) {
	var payload any
	json.Unmarshal(m.Payload, &payload)
	ev := map[string]any{"subject": m.Subject, "payload": payload}
	s.k.mu.RLock()
	var targets []*Job
	for _, j := range s.k.jobs {
		if j.On == pattern {
			targets = append(targets, j)
		}
	}
	s.k.mu.RUnlock()
	for _, j := range targets {
		go s.k.Run(s.ctx, j, ev)
	}
}

func (s *Scheduler) catchup(j *Job) {
	due := lastDue(j.At, time.Now())
	var res json.RawMessage
	var err error
	for i := 0; i < 12; i++ {
		res, err = s.k.call(s.ctx, "korpus.query", map[string]any{
			"type": "automation.run", "where": map[string]any{"name": j.Name, "ok": true},
			"order_by": "updated_at", "desc": true, "limit": 1})
		if err == nil || s.ctx.Err() != nil {
			break
		}
		time.Sleep(5 * time.Second)
	}
	if err != nil {
		slog.Warn("catch-up check failed, skipping", "job", j.Name, "err", err)
		return
	}
	var out struct {
		Records []struct {
			Data RunRecord `json:"data"`
		} `json:"records"`
	}
	json.Unmarshal(res, &out)
	if len(out.Records) > 0 {
		if t, err := time.Parse(time.RFC3339Nano, out.Records[0].Data.Started); err == nil && !t.Before(due) {
			return
		}
	}
	slog.Info("catch-up run", "job", j.Name)
	s.k.Run(s.ctx, j, nil)
}
