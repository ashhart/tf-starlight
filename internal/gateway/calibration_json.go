package gateway

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"unicode"
)

// uniqueCalibrationJSON bounds diagnostic parsing and rejects duplicate keys at every object depth.
func uniqueCalibrationJSON(data []byte) bool {
	if len(data) > 1<<20 {
		return false
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	budget := 4096
	if !calibrationValue(d, 0, &budget) {
		return false
	}
	_, err := d.Token()
	return err == io.EOF
}

func calibrationValue(d *json.Decoder, depth int, budget *int) bool {
	if depth > 64 || *budget <= 0 {
		return false
	}
	*budget--
	token, err := d.Token()
	if err != nil || depth == 0 && token != json.Delim('{') {
		return false
	}
	switch token {
	case json.Delim('{'):
		seen := map[string]bool{}
		for d.More() {
			if *budget <= 0 {
				return false
			}
			*budget--
			key, err := d.Token()
			name, ok := key.(string)
			if err != nil || !ok {
				return false
			}
			name = calibrationKey(name)
			if seen[name] {
				return false
			}
			seen[name] = true
			if !calibrationValue(d, depth+1, budget) {
				return false
			}
		}
		end, err := d.Token()
		return err == nil && end == json.Delim('}')
	case json.Delim('['):
		for d.More() {
			if !calibrationValue(d, depth+1, budget) {
				return false
			}
		}
		end, err := d.Token()
		return err == nil && end == json.Delim(']')
	default:
		_, delimiter := token.(json.Delim)
		return !delimiter
	}
}

func calibrationKey(name string) string {
	return strings.Map(func(r rune) rune {
		minimum := r
		for next := unicode.SimpleFold(r); next != r; next = unicode.SimpleFold(next) {
			if next < minimum {
				minimum = next
			}
		}
		return minimum
	}, name)
}
