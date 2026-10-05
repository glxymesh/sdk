package glxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime/debug"

	toolv1 "glxymesh.com/sdk/internal/toolv1"
	"google.golang.org/protobuf/encoding/protojson"
)

// Main runs the project's Lambda: it takes each call from the Lambda Runtime
// API, runs the named tool and answers with its result. The generated main
// package calls it; a tool author never does.
func Main(tools map[string]Tool) {
	api := os.Getenv("AWS_LAMBDA_RUNTIME_API")
	if api == "" {
		fmt.Fprintln(os.Stderr, "glxy: not running in Lambda; run tools locally with gxmesh dev")
		os.Exit(1)
	}
	r := &router{tools: tools, egressURL: os.Getenv("GLXY_EGRESS_URL"), client: &http.Client{}}
	if err := r.serve(context.Background(), "http://"+api+"/2018-06-01/runtime/invocation/"); err != nil {
		fmt.Fprintln(os.Stderr, "glxy:", err)
		os.Exit(1)
	}
}

type router struct {
	tools     map[string]Tool
	egressURL string
	client    *http.Client
}

// serve is the Runtime API's loop: next waits for a call, and every call is
// answered with a result, crashes included, so Lambda never has to report a
// failure of its own for a tool's bug.
func (r *router) serve(ctx context.Context, base string) error {
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"next", nil)
		if err != nil {
			return err
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return err
		}
		payload, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return err
		}
		id := resp.Header.Get("Lambda-Runtime-Aws-Request-Id")
		out := r.handle(ctx, payload)
		post, err := http.NewRequestWithContext(ctx, http.MethodPost, base+id+"/response", bytes.NewReader(out))
		if err != nil {
			return err
		}
		post.Header.Set("Content-Type", "application/json")
		answered, err := http.DefaultClient.Do(post)
		if err != nil {
			return err
		}
		answered.Body.Close()
	}
}

func (r *router) handle(ctx context.Context, payload []byte) []byte {
	res := r.invoke(ctx, payload)
	out, err := protojson.Marshal(res)
	if err != nil {
		out, _ = protojson.Marshal(crash("the result could not be encoded: "+err.Error(), nil))
	}
	return out
}

func (r *router) invoke(ctx context.Context, payload []byte) *toolv1.InvokeResult {
	var ev toolv1.InvokeEvent
	if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(payload, &ev); err != nil {
		return crash("malformed invoke event: "+err.Error(), nil)
	}
	tool, ok := r.tools[ev.Tool]
	if !ok {
		return crash("this build has no tool named "+ev.Tool, nil)
	}
	if ev.Deadline != nil {
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, ev.Deadline.AsTime())
		defer cancel()
	}
	log := &Logger{}
	c := &Ctx{
		Context: ctx,
		Caller: Caller{Org: ev.Caller.GetOrg(), Project: ev.Caller.GetProject(),
			UserID: ev.Caller.GetUserId(), ClientID: ev.Caller.GetClientId()},
		log:       log,
		http:      &http.Client{Transport: &egressTransport{url: r.egressURL, token: ev.EgressToken, client: r.client}},
		egressURL: r.egressURL, token: ev.EgressToken, client: r.client,
	}
	result, err := run(tool, c, json.RawMessage(ev.ArgumentsJson))
	c.end()
	var out *toolv1.InvokeResult
	if te, isTool := isToolError(err); isTool {
		out = &toolv1.InvokeResult{ErrorKind: toolv1.ErrorKind_ERROR_KIND_TOOL, ErrorMessage: te.msg}
	} else if err != nil {
		log.Error("%v", err)
		out = crash(err.Error(), nil)
	} else if raw, merr := json.Marshal(result); merr != nil {
		log.Error("the result isn't JSON: %v", merr)
		out = crash("the result isn't JSON: "+merr.Error(), nil)
	} else {
		out = &toolv1.InvokeResult{ResultJson: string(raw)}
	}
	out.Logs, out.LogsTruncated = log.drain()
	return out
}

// A panic is a bug like any other: the stack goes to the owner's log and the
// Lambda stays up for the next call.
func run(tool Tool, c *Ctx, args json.RawMessage) (result any, err error) {
	defer func() {
		if p := recover(); p != nil {
			c.log.Error("panic: %v\n%s", p, debug.Stack())
			result, err = nil, fmt.Errorf("panic: %v", p)
		}
	}()
	return tool.call(c, args)
}

func crash(msg string, logs []*toolv1.LogLine) *toolv1.InvokeResult {
	return &toolv1.InvokeResult{ErrorKind: toolv1.ErrorKind_ERROR_KIND_CRASH, ErrorMessage: msg, Logs: logs}
}
