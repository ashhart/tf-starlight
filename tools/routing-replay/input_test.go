package main

import (
	"os"
	"strings"
	"testing"

	"github.com/ashhart/tf-starlight/internal/routing"
)

func TestReplayFilesAreStrictAndComplete(t *testing.T) {
	for _, name := range []string{"mixed", "unequal-cold", "hot-prefix", "stale"} {
		t.Run(name, func(t *testing.T) {
			f, err := os.Open("testdata/" + name + ".json")
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			trace, err := readTrace(f)
			if err != nil {
				t.Fatal(err)
			}
			results, err := routing.Replay(trace)
			if err != nil || len(results) != 2 {
				t.Fatalf("fixture replay failed: %+v, %v", results, err)
			}
			for _, r := range results {
				if r.Completed != len(trace.Requests) {
					t.Fatalf("fixture lost requests: %+v", r)
				}
			}
		})
	}
}

func TestReplayInputRejectsContentAndTrailingDocuments(t *testing.T) {
	for _, raw := range []string{
		`{"version":1,"prompt":"private content"}`,
		`{"requests":[{"prompt_tokens":1,"body":"private content"}]}`,
		`{} {}`,
		`{} trailing`,
		strings.Repeat(" ", (4<<20)+1),
	} {
		if _, err := readTrace(strings.NewReader(raw)); err == nil {
			t.Fatal("unbounded or non-schema trace input accepted")
		}
	}
}
