package engine

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ashhart/tf-starlight/internal/control/wire"
)

// enginePin is engine.json: the TensorFold release Starlight is tested with.
type enginePin struct {
	Repository string            `json:"repository"`
	Version    string            `json:"version"`
	Archives   map[string]string `json:"archives"`
}

func TestPinnedEngineIsOneStarlightRuns(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "..", "engine.json"))
	if err != nil {
		t.Fatal(err)
	}
	var pin enginePin
	if err := json.Unmarshal(data, &pin); err != nil {
		t.Fatal(err)
	}
	if v, ok := parseVersion(pin.Version); !ok || older(v, MinEngine) || pin.Repository != "ashhart/TensorFold" {
		t.Fatalf("engine.json pins %s %q; Starlight runs ashhart/TensorFold %v and newer", pin.Repository, pin.Version, MinEngine)
	}
	for _, platform := range []string{"linux-aarch64", "linux-x86_64", "macos-arm64"} {
		if sum, err := hex.DecodeString(pin.Archives[platform]); err != nil || len(sum) != 32 {
			t.Errorf("engine.json has no SHA-256 for the %s archive", platform)
		}
	}
}

// TestEngineContract runs the engine in STARLIGHT_ENGINE as a node does, short of needing a model or a GPU.
func TestEngineContract(t *testing.T) {
	bin := os.Getenv("STARLIGHT_ENGINE")
	if bin == "" {
		t.Skip("STARLIGHT_ENGINE names no TensorFold engine binary")
	}
	if err := checkEngine(context.Background(), bin); err != nil {
		t.Fatal(err)
	}
	if usage, err := exec.Command(bin, "--help").CombinedOutput(); err != nil || !strings.Contains(string(usage), "pull REPO[@REVISION]") {
		t.Errorf("the engine's usage lacks pull REPO[@REVISION], which nodes run for artifacts (%v):\n%s", err, usage)
	}
	dir := t.TempDir()
	keys, err := WriteKeys(dir)
	if err != nil {
		t.Fatal(err)
	}
	model, drafter := filepath.Join(dir, "no-model"), filepath.Join(dir, "no-drafter")
	var served []string
	for _, backend := range []string{"", "metal", "cuda", "hip"} {
		out, took := parsesServe(t, bin, Args(wire.Spec{Backend: backend}, model, "", "contract", keys.File), model)
		if !took && backend != "" && strings.Contains(out, "is not served by") {
			continue
		}
		if !took {
			t.Fatalf("backend %q: the engine refused a node's plainest argv:\n%s", backend, out)
		}
		served = append(served, backend)
		for name, spec := range variants(backend) {
			d := ""
			if name == "drafter" || name == "everything" {
				d = drafter
			}
			if out, took := parsesServe(t, bin, Args(spec, model, d, "contract", keys.File), model); !took {
				t.Errorf("backend %q with %s: the engine refused the argv a node passes:\n%s", backend, name, out)
			}
		}
	}
	if runtime.GOOS != "darwin" {
		for _, spec := range []wire.Spec{{SSDExpertsGiB: 16}, {PLEOnSSD: true}} {
			if out, took := parsesServe(t, bin, Args(spec, model, "", "contract", keys.File), model); took {
				t.Errorf("a %s engine now takes %v; let wire.Spec allow it there, since it says only metal does:\n%s", runtime.GOOS, spec, out)
			}
		}
	}
	want := map[string][]string{"darwin": {"metal"}, "linux": {"cuda", "hip"}}[runtime.GOOS]
	if len(want) > 0 && !slices.ContainsFunc(want, func(b string) bool { return slices.Contains(served, b) }) {
		t.Errorf("the engine serves backends %q, none of %q that a %s node runs", served, want, runtime.GOOS)
	}
}

// variants turns on each optional field a node passes as a flag, alone and then together, where the backend takes it.
func variants(backend string) map[string]wire.Spec {
	out := map[string]wire.Spec{
		"context": {Backend: backend, Context: 8192}, "parallel": {Backend: backend, Parallel: 4},
		"thinking budget": {Backend: backend, ThinkingBudget: 2048}, "max tokens": {Backend: backend, MaxTokens: 4096},
		"prompt cache size": {Backend: backend, PromptCacheGiB: 8}, "prompt cache off": {Backend: backend, PromptCache: "off"},
		"bf16 KV cache": {Backend: backend, KVCache: "bf16"}, "fp8 KV cache": {Backend: backend, KVCache: "fp8"},
		"reasoning effort": {Backend: backend, ReasoningEffort: "xhigh"}, "vision": {Backend: backend, Vision: true},
		"no drafts": {Backend: backend, NoDrafts: true}, "drafter": {Backend: backend},
		"everything": {Backend: backend, Context: 8192, Parallel: 4, ThinkingBudget: 2048, MaxTokens: 4096, PromptCacheGiB: 8,
			KVCache: "fp8", ReasoningEffort: "xhigh", Vision: true},
	}
	if backend != "hip" {
		out["conversations on disk"] = wire.Spec{Backend: backend, LearnConversations: true}
		out["conversations on disk, limited"] = wire.Spec{Backend: backend, LearnConversations: true, LearnGiB: 64, LearnDailyGiB: 128}
	}
	if backend == "metal" {
		out["experts on SSD"] = wire.Spec{Backend: backend, SSDExpertsGiB: 16}
		out["n-gram tables on SSD"] = wire.Spec{Backend: backend, PLEOnSSD: true}
		out["everything on SSD"] = wire.Spec{Backend: backend, SSDExpertsGiB: 16, PLEOnSSD: true, LearnConversations: true}
	}
	return out
}

// parsesServe reports whether the engine took every flag in argv and went on to look for the missing model.
func parsesServe(t *testing.T, bin string, argv []string, model string) (string, bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, argv...)
	cmd.Env = childEnv(nil, true)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("the engine served %s, which does not exist:\n%s", model, out)
	}
	return string(out), !strings.Contains(string(out), "tensorfold serve: error:") && strings.Contains(string(out), model)
}
