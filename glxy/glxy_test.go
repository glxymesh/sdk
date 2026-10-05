package glxy

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	toolv1 "glxymesh.com/sdk/internal/toolv1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type weatherIn struct {
	City  string `json:"city"`
	Units string `json:"units"`
	Days  *int64 `json:"days,omitempty"`
}

// A stand-in egress gateway: it checks the call's token and answers /fetch.
func fakeEgress(t *testing.T) (*httptest.Server, *[]*toolv1.FetchRequest) {
	t.Helper()
	var mu sync.Mutex
	var seen []*toolv1.FetchRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/fetch" || r.Header.Get("Authorization") != "Bearer tok-1" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		var fr toolv1.FetchRequest
		proto.Unmarshal(raw, &fr)
		mu.Lock()
		seen = append(seen, &fr)
		mu.Unlock()
		var out *toolv1.FetchResponse
		switch {
		case strings.Contains(fr.Url, "blocked.example"):
			out = &toolv1.FetchResponse{Decision: toolv1.EgressDecision_EGRESS_DECISION_HOST_NOT_ALLOWED,
				Error: "blocked.example isn't among the hosts this tool may reach"}
		default:
			out = &toolv1.FetchResponse{Status: 200, Body: []byte(`{"temp":21}`),
				Headers: []*toolv1.Header{{Name: "Content-Type", Value: "application/json"}}}
		}
		b, _ := proto.Marshal(out)
		w.Write(b)
	}))
	t.Cleanup(srv.Close)
	return srv, &seen
}

func event(tool, args string) []byte {
	raw, _ := protojson.Marshal(&toolv1.InvokeEvent{
		CallId: "call-1", Tool: tool, ArgumentsJson: args, EgressToken: "tok-1",
		Caller:   &toolv1.Caller{Org: "acme", Project: "helpdesk", UserId: "u1", ClientId: "claude"},
		Deadline: timestamppb.New(time.Now().Add(5 * time.Second)),
	})
	return raw
}

func result(t *testing.T, out []byte) *toolv1.InvokeResult {
	t.Helper()
	var res toolv1.InvokeResult
	if err := protojson.Unmarshal(out, &res); err != nil {
		t.Fatalf("%s: %v", out, err)
	}
	return &res
}

func testRouter(t *testing.T) (*router, *[]*toolv1.FetchRequest) {
	t.Helper()
	egress, seen := fakeEgress(t)
	weather := func(ctx *Ctx, in weatherIn) (any, error) {
		if in.City == "" {
			return nil, Errorf("which city?")
		}
		ctx.Log().Info("looking up %s in %s for %s", in.City, in.Units, ctx.Caller.Org)
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.weather.example/v1?q="+in.City,
			strings.NewReader(`{"key":"`+Secret("WEATHER_KEY")+`"}`))
		req.Header.Set("X-Trace", "t1")
		resp, err := ctx.HTTP().Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		var body map[string]any
		json.NewDecoder(resp.Body).Decode(&body)
		return map[string]any{"city": in.City, "units": in.Units, "days": in.Days, "temp": body["temp"], "left": ctx.Remaining() > 0}, nil
	}
	return &router{
		tools: map[string]Tool{
			"weather": Func(weather).WithDefaults(`{"units":"metric"}`),
			"blocked": Func(func(ctx *Ctx, _ struct{}) (any, error) {
				_, err := ctx.HTTP().Get("https://blocked.example/")
				return nil, err
			}),
			"panics": Func(func(ctx *Ctx, _ struct{}) (any, error) { panic("boom") }),
			"noisy": Func(func(ctx *Ctx, _ struct{}) (any, error) {
				for range 200 {
					ctx.Log().Info("%s", strings.Repeat("x", 1000))
				}
				return "done", nil
			}),
		},
		egressURL: egress.URL,
		client:    http.DefaultClient,
	}, seen
}

func TestHandleRunsTheToolThroughEgress(t *testing.T) {
	r, seen := testRouter(t)
	res := result(t, r.handle(context.Background(), event("weather", `{"city":"Pune"}`)))
	if res.ErrorKind != toolv1.ErrorKind_ERROR_KIND_UNSPECIFIED ||
		res.ResultJson != `{"city":"Pune","days":null,"left":true,"temp":21,"units":"metric"}` {
		t.Fatalf("result = %v", res)
	}
	fr := (*seen)[0]
	if fr.Method != "POST" || fr.Url != "https://api.weather.example/v1?q=Pune" || string(fr.Body) != `{"key":"{{secret:WEATHER_KEY}}"}` {
		t.Errorf("sent %v; the placeholder travels, never a value", fr)
	}
	found := false
	for _, h := range fr.Headers {
		found = found || h.Name == "X-Trace" && h.Value == "t1"
	}
	if !found {
		t.Errorf("headers = %v", fr.Headers)
	}
	if len(res.Logs) != 1 || res.Logs[0].Message != "looking up Pune in metric for acme" || res.Logs[0].Level != "info" {
		t.Errorf("logs = %v", res.Logs)
	}
}

func TestHandleErrors(t *testing.T) {
	r, _ := testRouter(t)
	for _, tt := range []struct {
		tool, args string
		kind       toolv1.ErrorKind
		msg        string
	}{
		{"weather", `{}`, toolv1.ErrorKind_ERROR_KIND_TOOL, "which city?"},
		{"blocked", `{}`, toolv1.ErrorKind_ERROR_KIND_CRASH, "blocked.example isn't among the hosts"},
		{"panics", `{}`, toolv1.ErrorKind_ERROR_KIND_CRASH, "panic: boom"},
		{"missing", `{}`, toolv1.ErrorKind_ERROR_KIND_CRASH, "no tool named missing"},
		{"weather", `{"city":7}`, toolv1.ErrorKind_ERROR_KIND_CRASH, "run gxmesh gen"},
	} {
		res := result(t, r.handle(context.Background(), event(tt.tool, tt.args)))
		if res.ErrorKind != tt.kind || !strings.Contains(res.ErrorMessage, tt.msg) {
			t.Errorf("%s %s = %v %q, want %v containing %q", tt.tool, tt.args, res.ErrorKind, res.ErrorMessage, tt.kind, tt.msg)
		}
	}
	res := result(t, r.handle(context.Background(), event("panics", `{}`)))
	if len(res.Logs) != 2 || !strings.Contains(res.Logs[0].Message, "goroutine") {
		t.Errorf("a panic's stack didn't reach the owner's log: %v", res.Logs)
	}
	if res := result(t, r.handle(context.Background(), []byte(`{nope`))); res.ErrorKind != toolv1.ErrorKind_ERROR_KIND_CRASH {
		t.Errorf("a malformed event: %v", res)
	}
	res = result(t, r.handle(context.Background(), event("noisy", `{}`)))
	if res.ResultJson != `"done"` || !res.LogsTruncated || len(res.Logs) != 65 {
		t.Errorf("200 KB of logs: %d lines kept, truncated %v", len(res.Logs), res.LogsTruncated)
	}
}

func TestEgressErrorIsReadable(t *testing.T) {
	r, _ := testRouter(t)
	c := &Ctx{Context: context.Background(), log: &Logger{},
		http: &http.Client{Transport: &egressTransport{url: r.egressURL, token: "tok-1", client: http.DefaultClient}}}
	_, err := c.HTTP().Get("https://blocked.example/")
	var ee *EgressError
	if !errors.As(err, &ee) || ee.Decision != toolv1.EgressDecision_EGRESS_DECISION_HOST_NOT_ALLOWED {
		t.Errorf("err = %v", err)
	}
	c.http.Transport.(*egressTransport).token = "wrong"
	if _, err := c.HTTP().Get("https://api.weather.example/"); err == nil || !strings.Contains(err.Error(), "401") {
		t.Errorf("a refused token: %v", err)
	}
}

// The Runtime API: next hands over a call and blocks until the answer comes.
func TestServeAnswersTheRuntimeAPI(t *testing.T) {
	r, _ := testRouter(t)
	answers := make(chan []byte, 1)
	calls := 0
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch {
		case req.URL.Path == "/2018-06-01/runtime/invocation/next" && calls == 0:
			calls++
			w.Header().Set("Lambda-Runtime-Aws-Request-Id", "req-9")
			w.Write(event("weather", `{"city":"Goa","days":3}`))
		case req.URL.Path == "/2018-06-01/runtime/invocation/next":
			<-req.Context().Done()
		case req.URL.Path == "/2018-06-01/runtime/invocation/req-9/response":
			raw, _ := io.ReadAll(req.Body)
			answers <- raw
		}
	}))
	defer api.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.serve(ctx, api.URL+"/2018-06-01/runtime/invocation/") }()
	select {
	case raw := <-answers:
		if res := result(t, raw); !strings.Contains(res.ResultJson, `"city":"Goa","days":3`) {
			t.Errorf("answer = %v", res)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no answer reached the Runtime API")
	}
	cancel()
	if err := <-done; err == nil {
		t.Error("serve returned nil on a cancelled context")
	}
}
