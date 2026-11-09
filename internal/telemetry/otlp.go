package telemetry

import "encoding/json"

type attribute struct {
	Key   string      `json:"key"`
	Value stringValue `json:"value"`
}

type stringValue struct {
	String string `json:"stringValue"`
}

type scope struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type scopeSpans struct {
	Scope scope    `json:"scope"`
	Spans []record `json:"spans"`
}

type resource struct {
	Attributes []attribute `json:"attributes"`
}

type resourceSpans struct {
	Resource resource     `json:"resource"`
	Scopes   []scopeSpans `json:"scopeSpans"`
}

func marshalBatch(component Component, batch []record) ([]byte, error) {
	service := "starlight.gateway"
	if component == NodeComponent {
		service = "starlight.node"
	}
	payload := struct {
		Resources []resourceSpans `json:"resourceSpans"`
	}{[]resourceSpans{{Resource: resource{[]attribute{{Key: "service.name", Value: stringValue{service}}}},
		Scopes: []scopeSpans{{Scope: scope{"tensorfold.starlight.phases", "1"}, Spans: batch}}}}}
	return json.Marshal(payload)
}
