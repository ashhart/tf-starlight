// Command routing-replay reports matched synthetic routing arms; its input assumptions do not qualify real devices.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/ashhart/tf-starlight/internal/routing"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	input := flag.String("input", "", "content-free synthetic trace JSON, at most 4 MiB")
	flag.Parse()
	if *input == "" || flag.NArg() != 0 {
		return errors.New("use -input TRACE.json")
	}
	f, err := os.Open(*input)
	if err != nil {
		return err
	}
	defer f.Close()
	digest := sha256.New()
	trace, err := readTrace(io.TeeReader(f, digest))
	if err != nil {
		return err
	}
	results, err := routing.Replay(trace)
	if err != nil {
		return err
	}
	out := struct {
		Basis       string                 `json:"basis"`
		InputSHA256 string                 `json:"input_sha256"`
		Results     []routing.ReplayResult `json:"results"`
	}{"synthetic single-worker FIFO with assumed rates and a one-entry cache; not device evidence",
		hex.EncodeToString(digest.Sum(nil)), results}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(out)
}
