package engine

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"
	"time"
)

func TestCapturedActiveEngineMetricsQualifyAnActualBoundedWindow(t *testing.T) {
	raw, err := os.ReadFile("testdata/qwen38-active-capture.json")
	if err != nil {
		t.Fatal(err)
	}
	var receipt struct {
		Schema    int    `json:"schema"`
		Engine    string `json:"engine_head"`
		Gate      string `json:"gate_head"`
		Instance  string `json:"process_instance"`
		Before    string `json:"before_file"`
		After     string `json:"after_file"`
		BeforeSHA string `json:"before_sha256"`
		AfterSHA  string `json:"after_sha256"`
		Interval  int64  `json:"sample_complete_interval_ns"`
		Running   bool   `json:"gate_running_at_second_snapshot"`
	}
	if json.Unmarshal(raw, &receipt) != nil || receipt.Schema != 1 || receipt.Engine != "22c46c10aa3b222e46e77f94189b81e4c20d0dfb" || receipt.Gate != "9d9756f01ae1c27e14da1ce0a1d217bc674b30d9" || !receipt.Running || receipt.Instance == "" {
		t.Fatal("capture identity changed")
	}
	interval := time.Duration(receipt.Interval)
	if interval != 6_015_054_875*time.Nanosecond {
		t.Fatal("measured completion interval changed")
	}
	read := func(name, digest string) *sample {
		t.Helper()
		raw, err := os.ReadFile("testdata/" + name)
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(raw)
		if hex.EncodeToString(sum[:]) != digest {
			t.Fatal("selected capture bytes changed")
		}
		s := parseMetrics(bytes.NewReader(raw))
		if len(s.fam) != 6 {
			t.Fatal("selected capture lost a family")
		}
		s.base, s.epoch = receipt.Instance, 1
		return s
	}
	before, after := read(receipt.Before, receipt.BeforeSHA), read(receipt.After, receipt.AfterSHA)
	before.at = time.Unix(0, 0)
	after.at = before.at.Add(interval)
	if v, ok := produced(after); !ok || v != 125 {
		t.Fatal("finished92 plus live33 generated tokens lost")
	}
	if v, ok := prompted(after); !ok || v != 377 {
		t.Fatal("finished prompt count lost")
	}
	if v := measuredRate(before, after, produced); v == nil || *v != 125/interval.Seconds() {
		t.Fatal("captured generation interval did not qualify")
	}
	if v := measuredRate(before, after, prompted); v == nil || *v != 377/interval.Seconds() {
		t.Fatal("captured prompt interval did not qualify")
	}
	if v := measuredCount(after, famRunning); v == nil || *v != 1 {
		t.Fatal("active request became unknown")
	}
	if v := measuredCount(after, famWaiting); v == nil || *v != 0 {
		t.Fatal("known zero wait count lost")
	}
	if v, ok := after.max(famKV); !ok || v != 0.042847 {
		t.Fatal("live KV ratio lost")
	}
}
