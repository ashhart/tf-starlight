package tracecollector

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
)

var names = map[string]bool{
	"gateway.request": true, "gateway.prepare": true, "gateway.admission": true, "gateway.dispatch": true,
	"gateway.relay": true, "gateway.settlement": true, "gateway.first_data": true, "gateway.first_output": true,
	"gateway.first_flush": true, "node.request": true, "node.permit": true, "node.engine_request": true,
	"node.engine_headers": true, "node.engine_relay": true,
}

func text(raw json.RawMessage) string {
	var s string
	json.Unmarshal(raw, &s)
	return s
}

func number(raw json.RawMessage) int {
	var n int
	if json.Unmarshal(raw, &n) != nil {
		return -1
	}
	return n
}

func id(s string, bytes int) bool {
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != bytes {
		return false
	}
	for _, v := range b {
		if v != 0 {
			return true
		}
	}
	return false
}

func decode(raw []byte) ([]Frame, error) {
	invalid := errors.New("OTLP request contains an unexpected field or invalid phase/identifier/timestamp")
	var root map[string]json.RawMessage
	if json.Unmarshal(raw, &root) != nil || len(root) != 1 {
		return nil, invalid
	}
	resources, err := objects(root["resourceSpans"])
	if err != nil || len(resources) != 1 || len(resources[0]) != 2 {
		return nil, invalid
	}
	var resource map[string]json.RawMessage
	if json.Unmarshal(resources[0]["resource"], &resource) != nil || len(resource) != 1 {
		return nil, invalid
	}
	attributes, err := objects(resource["attributes"])
	if err != nil || len(attributes) != 1 || len(attributes[0]) != 2 || text(attributes[0]["key"]) != "service.name" {
		return nil, invalid
	}
	var value map[string]json.RawMessage
	if json.Unmarshal(attributes[0]["value"], &value) != nil || len(value) != 1 ||
		(text(value["stringValue"]) != "starlight.gateway" && text(value["stringValue"]) != "starlight.node") {
		return nil, invalid
	}
	scopes, err := objects(resources[0]["scopeSpans"])
	if err != nil || len(scopes) != 1 || len(scopes[0]) != 2 {
		return nil, invalid
	}
	var scope map[string]json.RawMessage
	if json.Unmarshal(scopes[0]["scope"], &scope) != nil || len(scope) != 2 ||
		text(scope["name"]) != "tensorfold.starlight.phases" || text(scope["version"]) != "1" {
		return nil, invalid
	}
	spans, err := objects(scopes[0]["spans"])
	if err != nil || len(spans) < 1 || len(spans) > 128 {
		return nil, invalid
	}
	allowed := map[string]bool{"traceId": true, "spanId": true, "parentSpanId": true, "name": true, "kind": true,
		"flags": true, "startTimeUnixNano": true, "endTimeUnixNano": true, "status": true}
	var frames []Frame
	for _, s := range spans {
		for key := range s {
			if !allowed[key] {
				return nil, invalid
			}
		}
		f := Frame{Trace: text(s["traceId"]), ID: text(s["spanId"]), Parent: text(s["parentSpanId"]), Name: text(s["name"])}
		start, startErr := strconv.ParseInt(text(s["startTimeUnixNano"]), 10, 64)
		end, endErr := strconv.ParseInt(text(s["endTimeUnixNano"]), 10, 64)
		var status map[string]json.RawMessage
		if !id(f.Trace, 16) || !id(f.ID, 8) || f.Parent != "" && !id(f.Parent, 8) || !names[f.Name] ||
			startErr != nil || endErr != nil || start <= 0 || end < start || number(s["kind"]) < 1 || number(s["kind"]) > 3 ||
			number(s["flags"]) != 1 || json.Unmarshal(s["status"], &status) != nil || len(status) != 1 ||
			number(status["code"]) < 0 || number(status["code"]) > 2 {
			return nil, invalid
		}
		f.Start, f.End = start, end
		frames = append(frames, f)
	}
	return frames, nil
}
