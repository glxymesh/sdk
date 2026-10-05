package glxy

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	toolv1 "glxymesh.com/sdk/internal/toolv1"
	"google.golang.org/protobuf/encoding/protojson"
)

// DialContext opens a TCP connection, for a database or another protocol
// that isn't HTTPS, by way of the egress gateway: address is host:port as
// the tool's network.connect lists it, or the server a connection string
// names. Its signature is the dial hook drivers take, such as pgx's
// config.DialFunc and go-redis's Dialer. The connection ends with the call.
func (c *Ctx) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	if network != "tcp" && network != "tcp4" && network != "tcp6" {
		return nil, fmt.Errorf("egress: only tcp connections, not %s", network)
	}
	u, err := url.Parse(c.egressURL)
	if c.egressURL == "" || err != nil {
		return nil, errors.New("egress: no egress gateway is configured for this call")
	}
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", u.Host)
	if err != nil {
		return nil, fmt.Errorf("egress: %w", err)
	}
	if dl, ok := ctx.Deadline(); ok {
		conn.SetDeadline(dl)
	}
	fmt.Fprintf(conn, "GET /connect HTTP/1.1\r\nHost: %s\r\nConnection: Upgrade\r\nUpgrade: glxymesh-tcp\r\nAuthorization: Bearer %s\r\nX-Glxymesh-Connect: %s\r\n\r\n", u.Host, c.token, address)
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("egress: %w", err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		defer conn.Close()
		return nil, &EgressError{Decision: toolv1.EgressDecision_EGRESS_DECISION_HOST_NOT_ALLOWED, Message: refusal(resp)}
	}
	if dl, ok := c.Deadline(); ok {
		conn.SetDeadline(dl)
	} else {
		conn.SetDeadline(time.Time{})
	}
	return &bufferedConn{Conn: conn, r: br}, nil
}

// Forward listens on 127.0.0.1 and carries each connection made there to
// address by way of the egress gateway: for a driver that opens its own
// sockets and takes no dial hook. It returns the local host:port to give the
// driver; the listener closes when the call ends.
func (c *Ctx) Forward(address string) (string, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	c.onEnd(func() { ln.Close() })
	go func() {
		for {
			local, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer local.Close()
				remote, err := c.DialContext(c, "tcp", address)
				if err != nil {
					c.log.Error("forward to %s: %v", address, err)
					return
				}
				defer remote.Close()
				var wg sync.WaitGroup
				wg.Add(2)
				go func() { defer wg.Done(); io.Copy(remote, local); closeWrite(remote) }()
				go func() { defer wg.Done(); io.Copy(local, remote); closeWrite(local) }()
				wg.Wait()
			}()
		}
	}()
	return ln.Addr().String(), nil
}

// SecretValue is a passed-in key's value, for this call: a key tool.yml
// binds with mode: pass-in, which a person approved before the revision
// went live. Every other key stays with the egress gateway.
func (c *Ctx) SecretValue(name string) (string, error) {
	if c.egressURL == "" {
		return "", errors.New("egress: no egress gateway is configured for this call")
	}
	req, err := http.NewRequestWithContext(c, http.MethodGet, c.egressURL+"/secret/"+url.PathEscape(name), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("egress: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", Errorf("%s", refusal(resp))
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return "", err
	}
	var v toolv1.SecretValue
	if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(raw, &v); err != nil {
		return "", err
	}
	return v.Value, nil
}

func refusal(resp *http.Response) string {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	var e struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(raw, &e) == nil && e.Error != "" {
		return e.Error
	}
	if s := strings.TrimSpace(string(raw)); s != "" {
		return s
	}
	return resp.Status
}

type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (b *bufferedConn) Read(p []byte) (int, error) { return b.r.Read(p) }

func closeWrite(c net.Conn) {
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		cw.CloseWrite()
		return
	}
	if b, ok := c.(*bufferedConn); ok {
		closeWrite(b.Conn)
		return
	}
	c.Close()
}
