package glxy

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"

	toolv1 "glxymesh.com/sdk/internal/toolv1"
	"google.golang.org/protobuf/proto"
)

// The egress gateway's own caps; a larger body would be refused there anyway.
const maxBodyBytes = 4 << 20

// EgressError is a request the egress gateway refused or couldn't complete:
// a host the tool may not reach, a private address, a key sent off its hosts.
// Reported as it is, it tells the tool's author what to add to tool.yml.
type EgressError struct {
	Decision toolv1.EgressDecision
	Message  string
}

func (e *EgressError) Error() string { return "egress: " + e.Message }

// egressTransport sends every request to the egress gateway's /fetch with
// the call's token, which is the only way out of a tool Lambda: its network
// has no other route, and its DNS answers nothing else.
type egressTransport struct {
	url    string
	token  string
	client *http.Client
}

func (t *egressTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.url == "" {
		return nil, errors.New("egress: no egress gateway is configured for this call")
	}
	var body []byte
	if req.Body != nil {
		var err error
		body, err = io.ReadAll(io.LimitReader(req.Body, maxBodyBytes+1))
		req.Body.Close()
		if err != nil {
			return nil, err
		}
		if len(body) > maxBodyBytes {
			return nil, errors.New("egress: request body over 4 MB")
		}
	}
	fr := &toolv1.FetchRequest{Method: req.Method, Url: req.URL.String(), Body: body}
	for name, values := range req.Header {
		for _, v := range values {
			fr.Headers = append(fr.Headers, &toolv1.Header{Name: name, Value: v})
		}
	}
	raw, err := proto.Marshal(fr)
	if err != nil {
		return nil, err
	}
	out, err := http.NewRequestWithContext(req.Context(), http.MethodPost, t.url+"/fetch", bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	out.Header.Set("Authorization", "Bearer "+t.token)
	out.Header.Set("Content-Type", "application/x-protobuf")
	resp, err := t.client.Do(out)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	answer, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes+1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("egress: the egress gateway answered %d", resp.StatusCode)
	}
	var got toolv1.FetchResponse
	if err := proto.Unmarshal(answer, &got); err != nil {
		return nil, err
	}
	if got.Status == 0 {
		return nil, &EgressError{Decision: got.Decision, Message: got.Error}
	}
	header := http.Header{}
	for _, h := range got.Headers {
		header.Add(h.Name, h.Value)
	}
	return &http.Response{
		Status:        strconv.Itoa(int(got.Status)) + " " + http.StatusText(int(got.Status)),
		StatusCode:    int(got.Status),
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        header,
		Body:          io.NopCloser(bytes.NewReader(got.Body)),
		ContentLength: int64(len(got.Body)),
		Request:       req,
	}, nil
}
