package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"

	"github.com/ashhart/tf-starlight/internal/routing"
)

func readTrace(r io.Reader) (routing.ReplayTrace, error) {
	var trace routing.ReplayTrace
	raw, err := io.ReadAll(io.LimitReader(r, (4<<20)+1))
	if err != nil {
		return trace, err
	}
	if len(raw) > 4<<20 {
		return trace, errors.New("routing trace exceeds 4 MiB")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&trace); err != nil {
		return trace, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return trace, errors.New("routing trace must contain one JSON document")
	}
	return trace, nil
}
