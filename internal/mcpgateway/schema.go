package mcpgateway

import (
	"encoding/json"
	"strings"
)

// validHeaderSchema excludes invalid HTTP-mirrored annotations without hiding valid sibling tools.
func validHeaderSchema(raw []byte) bool {
	var schema map[string]json.RawMessage
	return json.Unmarshal(raw, &schema) == nil && schema != nil && headerSchema(schema, true, false, map[string]bool{})
}

func headerSchema(schema map[string]json.RawMessage, reachable, property bool, seen map[string]bool) bool {
	if raw, ok := schema["x-mcp-header"]; ok {
		var name, kind string
		if !reachable || !property || json.Unmarshal(raw, &name) != nil || !headerToken(name) || seen[strings.ToLower(name)] ||
			json.Unmarshal(schema["type"], &kind) != nil || (kind != "integer" && kind != "boolean" && kind != "string") {
			return false
		}
		seen[strings.ToLower(name)] = true
	}
	for key, raw := range schema {
		switch key {
		case "properties", "$defs", "definitions", "patternProperties", "dependentSchemas":
			var children map[string]json.RawMessage
			if json.Unmarshal(raw, &children) != nil {
				return false
			}
			for _, child := range children {
				if !headerChild(child, reachable && key == "properties", key == "properties", seen) {
					return false
				}
			}
		case "allOf", "anyOf", "oneOf", "prefixItems":
			var children []json.RawMessage
			if json.Unmarshal(raw, &children) != nil {
				return false
			}
			for _, child := range children {
				if !headerChild(child, false, false, seen) {
					return false
				}
			}
		case "items", "not", "if", "then", "else", "contains", "additionalProperties", "propertyNames", "unevaluatedProperties", "unevaluatedItems":
			if !headerChild(raw, false, false, seen) {
				return false
			}
		}
	}
	return true
}

func headerChild(raw json.RawMessage, reachable, property bool, seen map[string]bool) bool {
	if string(raw) == "true" || string(raw) == "false" {
		return true
	}
	var child map[string]json.RawMessage
	return json.Unmarshal(raw, &child) == nil && child != nil && headerSchema(child, reachable, property, seen)
}

func headerToken(name string) bool {
	if name == "" {
		return false
	}
	for _, ch := range []byte(name) {
		if ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || strings.ContainsRune("!#$%&'*+-.^_`|~", rune(ch)) {
			continue
		}
		return false
	}
	return true
}
