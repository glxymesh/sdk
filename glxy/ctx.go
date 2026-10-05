// Package glxy is what a Go function tool is written against. A tool
// is a plain function, func Run(ctx *glxy.Ctx, in Input) (any, error); only
// the gateway speaks MCP. Its code reaches the internet through ctx.HTTP(),
// which goes by the egress gateway: the tool's allow-list holds there, and
// its keys are added there, so the code never holds them.
package glxy

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"
)

// Ctx is the call: a context.Context that ends at the call's deadline, plus
// who is calling, the tool's way out and its log.
type Ctx struct {
	context.Context
	Caller Caller
	log    *Logger
	http   *http.Client
	// The egress gateway, for connections and passed-in keys (connect.go).
	egressURL, token string
	client           *http.Client
	ends             []func()
}

// onEnd runs f when the call ends.
func (c *Ctx) onEnd(f func()) { c.ends = append(c.ends, f) }

func (c *Ctx) end() {
	for _, f := range c.ends {
		f()
	}
}

// Caller is who the call is for, as code may see them: never an email or a
// token.
type Caller struct {
	Org     string
	Project string
	UserID  string
	// The MCP client the person allowed, such as Claude or Cursor.
	ClientID string
}

// HTTP is the tool's client for every outside request. Pass it to any SDK
// that takes an *http.Client.
func (c *Ctx) HTTP() *http.Client { return c.http }

func (c *Ctx) Log() *Logger { return c.log }

// Remaining is the time left before the call's deadline.
func (c *Ctx) Remaining() time.Duration {
	if d, ok := c.Deadline(); ok {
		return time.Until(d)
	}
	return 0
}

// Secret stands in for a key where no header or query binding fits, such as
// a JSON body. The egress gateway swaps in the value, and only on the hosts
// the key is bound to; anywhere else the request is refused.
func Secret(name string) string { return "{{secret:" + name + "}}" }

// ToolError ends a call with a message the model reads and can act on. Any
// other error is treated as a bug: the model gets a fixed sentence and the
// owner the log.
type ToolError struct{ msg string }

func (e *ToolError) Error() string { return e.msg }

func Errorf(format string, args ...any) error {
	return &ToolError{msg: fmt.Sprintf(format, args...)}
}

func isToolError(err error) (*ToolError, bool) {
	var te *ToolError
	ok := errors.As(err, &te)
	return te, ok
}
