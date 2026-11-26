package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestDecisionEvaluateOutageIsNotNegative(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	t.Setenv("KRON_DECIDER_URL", srv.URL)
	k := &Kron{}
	out, err := k.evaluateDecision(context.Background(), decideIn{
		State:     "x",
		Questions: map[string]decideQuestion{"met": {Type: "noul"}},
	})
	if err == nil || out.Answers != nil {
		t.Fatalf("an outage must be an error, not answers: %v %v", out, err)
	}
}

// TestDecisionEvaluate: the verdict path and the bounds. The outage case is kept separate
// above, since "error and no answers" is the one property that must not regress.
func TestDecisionEvaluate(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"answers":{"met":{"type":"noul","noul":0.9}},"model":"d1-3b"}`))
	}))
	defer srv.Close()
	nol := func(q map[string]decideQuestion) decideIn { return decideIn{State: "x", Questions: q} }
	cases := []struct {
		name string
		url  string
		in   decideIn
		want string // model of the accepted reply; "" means the input must be refused
	}{
		{"unconfigured decider is refused", "", nol(map[string]decideQuestion{"met": {Type: "noul"}}), ""},
		{"happy reply hands back answers and model", srv.URL, decideIn{State: "evidence", Questions: map[string]decideQuestion{"met": {Type: "noul", Instructions: "met?"}}}, "d1-3b"},
		{"unknown type is refused before any network call", "http://127.0.0.1:1", nol(map[string]decideQuestion{"bad": {Type: "freeform prose please"}}), ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("KRON_DECIDER_URL", tc.url)
			out, err := (&Kron{}).evaluateDecision(context.Background(), tc.in)
			if tc.want == "" {
				if err == nil || out.Answers != nil {
					t.Fatalf("want an error and no answers, got %v %v", out, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, ok := out.Answers["met"]; !ok || out.Model != tc.want {
				t.Fatalf("bad reply: %+v", out)
			}
		})
	}
}

// d1 reads `criteria` for choice and score questions; the bridge must forward it untouched.
func TestDecisionEvaluateForwardsCriteria(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&got)
		w.Write([]byte(`{"answers":{"p":{"type":"choice","choice":"a","confidence":0.9}},"model":"d1-3b"}`))
	}))
	defer srv.Close()
	t.Setenv("KRON_DECIDER_URL", srv.URL)
	k := &Kron{}
	_, err := k.evaluateDecision(context.Background(), decideIn{State: "x", Questions: map[string]decideQuestion{
		"p": {Type: "choice", Instructions: "which?", Criteria: json.RawMessage(`{"a":"first","b":"second"}`)}}})
	if err != nil {
		t.Fatal(err)
	}
	q := got["questions"].(map[string]any)["p"].(map[string]any)
	if c, _ := q["criteria"].(map[string]any); c["a"] != "first" || c["b"] != "second" {
		t.Fatalf("criteria not forwarded: %v", q)
	}
	for name, q := range map[string]decideQuestion{"choice without options": {Type: "choice"}, "score without rubric": {Type: "score"}} {
		if _, err := k.evaluateDecision(context.Background(), decideIn{State: "x", Questions: map[string]decideQuestion{"p": q}}); err == nil {
			t.Fatalf("%s must be refused", name)
		}
	}
}
