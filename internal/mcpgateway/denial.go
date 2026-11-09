package mcpgateway

import (
	"context"
	"encoding/json"
	"net/http"
	"time"
)

func (h *Handler) reject(w http.ResponseWriter, r *http.Request, id json.RawMessage, problem *failure, event Event) {
	event.Phase, event.Outcome, event.Status = "deny", "refused", problem.status
	if event.Actor == nil {
		h.refusals.add(problem.status)
	} else {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), time.Second)
		defer cancel()
		if err := h.options.Record(ctx, event); err != nil && h.options.Log != nil {
			h.options.Log.Error("MCP denial accounting unavailable")
		}
	}
	writeFailure(w, id, problem, r.Header.Get("MCP-Protocol-Version"))
}
