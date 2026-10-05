package glxy

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// Tool is one function tool as the router holds it; Func makes one from a
// Run function.
type Tool interface {
	call(ctx *Ctx, args json.RawMessage) (any, error)
}

type funcTool[In any] struct {
	run      func(*Ctx, In) (any, error)
	defaults map[string]json.RawMessage
}

func Func[In any](run func(*Ctx, In) (any, error)) *funcTool[In] {
	return &funcTool[In]{run: run}
}

// WithDefaults fills inputs the call left out from tool.yml's defaults. The
// generated router passes them; a tool author never calls it.
func (f *funcTool[In]) WithDefaults(defaultsJSON string) *funcTool[In] {
	if err := json.Unmarshal([]byte(defaultsJSON), &f.defaults); err != nil {
		panic(fmt.Sprintf("glxy: generated defaults don't parse: %v", err))
	}
	return f
}

// The gateway checked the arguments against the tool's schema before the
// call, so a decode failure here means the generated types are stale.
func (f *funcTool[In]) call(ctx *Ctx, args json.RawMessage) (any, error) {
	given := map[string]json.RawMessage{}
	if len(bytes.TrimSpace(args)) > 0 && !bytes.Equal(bytes.TrimSpace(args), []byte("null")) {
		if err := json.Unmarshal(args, &given); err != nil {
			return nil, fmt.Errorf("arguments: %w", err)
		}
	}
	for k, v := range f.defaults {
		if _, ok := given[k]; !ok {
			given[k] = v
		}
	}
	merged, err := json.Marshal(given)
	if err != nil {
		return nil, err
	}
	var in In
	if err := json.Unmarshal(merged, &in); err != nil {
		return nil, fmt.Errorf("arguments don't fit Input (run gxmesh gen): %w", err)
	}
	return f.run(ctx, in)
}
