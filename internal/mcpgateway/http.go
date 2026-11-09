package mcpgateway

import (
	"encoding/json"
	"net/http"
)

func writeFailure(w http.ResponseWriter, id json.RawMessage, problem *failure, requested ...string) {
	if problem.status == http.StatusUnauthorized {
		w.Header().Set("WWW-Authenticate", `Bearer realm="starlight-mcp"`)
	}
	envelope := map[string]any{"jsonrpc": "2.0", "error": map[string]any{"code": problem.code, "message": problem.message}}
	if validID(id) {
		envelope["id"] = id
	}
	if problem.code == -32022 {
		version := ""
		if len(requested) != 0 {
			version = requested[0]
		}
		envelope["error"].(map[string]any)["data"] = map[string]any{"supported": []string{protocolVersion}, "requested": version}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(problem.status)
	_ = json.NewEncoder(w).Encode(envelope)
}

func writeDiscovery(w http.ResponseWriter, id json.RawMessage) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{
		"resultType": "complete", "supportedVersions": []string{protocolVersion}, "capabilities": map[string]any{"tools": map[string]any{}},
		"_meta": map[string]any{"io.modelcontextprotocol/serverInfo": map[string]string{"name": "Starlight governed tools", "version": "1"}},
		"ttlMs": 0, "cacheScope": "private"}})
}

type streamWriter struct {
	http.ResponseWriter
	started bool
}

func (w *streamWriter) WriteHeader(status int) {
	w.started = true
	w.ResponseWriter.WriteHeader(status)
}
func (w *streamWriter) Write(data []byte) (int, error) {
	w.started = true
	return w.ResponseWriter.Write(data)
}
func (w *streamWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
