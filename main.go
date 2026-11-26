package main

import (
	"context"
	"encoding/json"
	"flag"
	"log"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/knoniewicz/kor/client"
	"github.com/knoniewicz/kor/messages"
)

func env(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

type nameIn struct {
	Name string `json:"name"`
}
type runsIn struct {
	Name  string `json:"name,omitempty"`
	Limit int    `json:"limit,omitempty"`
}

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))
	addr := flag.String("kor", env("KOR_ADDR", "127.0.0.1:4730"), "kor daemon address")
	dir := flag.String("dir", env("KRON_DIR", "./lua"), "directory with connectors/ and automations/")
	allow := flag.String("env-allow", os.Getenv("KRON_ENV_ALLOW"), "comma separated env names Lua may read via ctx.env")
	timeout := flag.Duration("timeout", 2*time.Minute, "per-run time limit")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	kc, err := client.Dial(*addr, "kron")
	if err != nil {
		log.Fatal(err)
	}
	defer kc.Close()

	var allowed []string
	for _, n := range strings.Split(*allow, ",") {
		if n = strings.TrimSpace(n); n != "" {
			allowed = append(allowed, n)
		}
	}
	k := NewKron(kc, Config{Dir: *dir, EnvAllow: allowed, Timeout: *timeout})
	if err := k.registerSchemas(ctx); err != nil {
		slog.Error("schema registration failed, writes to unknown types will be rejected", "err", err)
	}
	s := NewScheduler(ctx, k, kc.Subscribe)
	registerHandlers(ctx, kc, k, s)
	if err := s.Reload(); err != nil {
		log.Fatal(err)
	}
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	for {
		select {
		case <-hup:
			if err := s.Reload(); err != nil {
				slog.Error("reload failed, keeping old jobs", "err", err)
			}
		case <-ctx.Done():
			return
		}
	}
}

type listOut struct {
	Items []JobInfo `json:"items"`
}
type runOut struct {
	Run RunRecord `json:"run"`
}
type runsOut struct {
	Runs []json.RawMessage `json:"runs"`
}

func registerHandlers(ctx context.Context, kc *client.Client, k *Kron, s *Scheduler) {
	must := func(err error) {
		if err != nil {
			log.Fatal(err)
		}
	}
	d1, err := messages.HandlerDescriptorFor[struct{}, listOut]("List loaded connectors and automations")
	must(err)
	must(kc.Handle(ctx, "list", d1, func(messages.Message) (any, error) { return listOut{k.list()}, nil }))

	d2, err := messages.HandlerDescriptorFor[nameIn, runOut]("Run a connector or automation now and return its run record")
	must(err)
	must(kc.Handle(ctx, "run", d2, func(m messages.Message) (any, error) {
		var in nameIn
		if err := m.DecodePayload(&in); err != nil {
			return nil, err
		}
		j := k.job(in.Name)
		if j == nil {
			return nil, errNotFound(in.Name)
		}
		return runOut{k.Run(ctx, j, nil)}, nil
	}))

	d3, err := messages.HandlerDescriptorFor[runsIn, runsOut]("Recent run records (automation.run), newest first")
	must(err)
	must(kc.Handle(ctx, "runs", d3, func(m messages.Message) (any, error) {
		return k.runs(ctx, m)
	}))

	reg[nameIn, sourceOut](ctx, kc, "source", "Read a loaded job's own Lua source by name (read-only, max 256 KiB)", k.source)

	// Optional decider capability. Nothing depends on it; when d1 is unset or down the
	// caller gets an error, not a negative verdict.
	d18, err := messages.HandlerDescriptorFor[decideIn, decideOut]("Ask the optional decision model typed questions (noul/choice/score); errors when it is unset or unreachable")
	must(err)
	must(kc.Handle(ctx, "decision.evaluate", d18, func(m messages.Message) (any, error) {
		var in decideIn
		if err := m.DecodePayload(&in); err != nil {
			return nil, err
		}
		return k.evaluateDecision(ctx, in)
	}))
}

// reg registers one handler: descriptor for the In/Out shapes, then the adapter that
// hands ctx to fn. Registration failures are fatal, same as everywhere else here.
func reg[In any, Out any](ctx context.Context, kc *client.Client, name, desc string, fn func(context.Context, messages.Message) (any, error)) {
	d, err := messages.HandlerDescriptorFor[In, Out](desc)
	if err != nil {
		log.Fatal(err)
	}
	if err := kc.Handle(ctx, name, d, func(m messages.Message) (any, error) { return fn(ctx, m) }); err != nil {
		log.Fatal(err)
	}
}
