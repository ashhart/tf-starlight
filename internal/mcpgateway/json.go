package mcpgateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"unicode/utf8"
)

const maxRequestBytes = 64 << 10

// checkJSON rejects ambiguous keys and bounds nesting before either endpoint interprets the request.
func checkJSON(raw []byte) error {
	return checkJSONLimit(raw, maxRequestBytes)
}

func checkJSONLimit(raw []byte, limit int) error {
	if len(raw) == 0 || len(raw) > limit || !utf8.Valid(raw) {
		return errors.New("request size is outside the supported bounds")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	nodes := 0
	if err := jsonValue(d, 0, &nodes); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return errors.New("request must contain one JSON value")
	}
	return nil
}

func jsonValue(d *json.Decoder, depth int, nodes *int) error {
	*nodes = *nodes + 1
	if depth > 32 || *nodes > 8192 {
		return errors.New("request JSON is too complex")
	}
	token, err := d.Token()
	if err != nil {
		return errors.New("invalid JSON")
	}
	delimiter, container := token.(json.Delim)
	if !container {
		return nil
	}
	switch delimiter {
	case '{':
		keys := map[string]bool{}
		for d.More() {
			key, err := d.Token()
			name, ok := key.(string)
			if err != nil || !ok || keys[name] {
				return errors.New("invalid or duplicate JSON key")
			}
			keys[name] = true
			if err := jsonValue(d, depth+1, nodes); err != nil {
				return err
			}
		}
	case '[':
		for d.More() {
			if err := jsonValue(d, depth+1, nodes); err != nil {
				return err
			}
		}
	default:
		return errors.New("invalid JSON container")
	}
	end, err := d.Token()
	if err != nil || delimiter == '{' && end != json.Delim('}') || delimiter == '[' && end != json.Delim(']') {
		return errors.New("invalid JSON container")
	}
	return nil
}
