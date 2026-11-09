package engine

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"testing"
	"time"
)

// Captured from unchanged engine 22c46c10aa3b222e46e77f94189b81e4c20d0dfb, with only the six requested families.
func TestCapturedEngineMetricsKeepExpiredRatesUnknownAndIdleGaugesKnown(t *testing.T) {
	read := func(name, digest, at string) *sample {
		t.Helper()
		raw, err := os.ReadFile("testdata/" + name)
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(raw)
		if hex.EncodeToString(sum[:]) != digest {
			t.Fatal("captured metric bytes changed")
		}
		s := parseMetrics(bytes.NewReader(raw))
		if len(s.fam) != 6 {
			t.Fatalf("parsed %d families, want 6", len(s.fam))
		}
		s.at, err = time.Parse(time.RFC3339Nano, at)
		if err != nil {
			t.Fatal(err)
		}
		s.base, s.epoch = "captured-process", 1
		return s
	}
	// Capture start times establish the expired interval; this fixture cannot qualify a valid rate.
	before := read("qwen38-before.prom", "fb2c3d16ebd4e7e13c406c64da9701bb4211199e76e2b5188ed415de0bb64933", "2026-11-09T02:58:44.459394Z")
	after := read("qwen38-after.prom", "a148c56b2795b18c2a943d52f04d56df25cca28868ba136ef20997c00ca9307b", "2026-11-09T02:59:05.169606Z")
	if generated, ok := produced(after); !ok || generated != 186 {
		t.Fatalf("captured generated count %v, known=%v", generated, ok)
	}
	if prompt, ok := prompted(after); !ok || prompt != 977 {
		t.Fatalf("captured prompt count %v, known=%v", prompt, ok)
	}
	for _, family := range []string{famRunning, famWaiting} {
		if value := measuredCount(after, family); value == nil || *value != 0 {
			t.Fatalf("captured idle gauge %s became unknown or nonzero", family)
		}
	}
	if kv, ok := after.max(famKV); !ok || kv != 0 {
		t.Fatalf("captured idle KV %v, known=%v", kv, ok)
	}
	if measuredRate(before, after, produced) != nil || measuredRate(before, after, prompted) != nil {
		t.Fatal("the 20.7-second captured window became a qualified rate")
	}
}
