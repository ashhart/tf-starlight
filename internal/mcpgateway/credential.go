package mcpgateway

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
)

func reflectsCredential(raw []byte, credential string) bool {
	if credential == "" {
		return false
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	for {
		token, err := d.Token()
		if err == io.EOF {
			return false
		}
		if err != nil {
			return true
		}
		if value, ok := token.(string); ok && strings.Contains(value, credential) {
			return true
		}
	}
}
