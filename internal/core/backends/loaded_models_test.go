package backends

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func newProbeOpenAI(t *testing.T, baseURL, kind string) *OpenAI {
	t.Helper()
	o, err := NewOpenAI("srv", baseURL, "", 5*time.Second)
	if err != nil {
		t.Fatalf("NewOpenAI: %v", err)
	}
	o.SetProbeKind(kind)
	return o
}

func TestLoadedModels_Ollama(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/ps" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"models":[{"name":"llama3:latest","model":"llama3:latest"},{"name":"qwen2.5:7b","model":"qwen2.5:7b"}]}`))
	}))
	defer srv.Close()

	o := newProbeOpenAI(t, srv.URL, ProbeOllama)
	got, supported, err := o.LoadedModels(context.Background())
	if err != nil || !supported {
		t.Fatalf("LoadedModels err=%v supported=%v", err, supported)
	}
	assertEqualStrings(t, got, []string{"llama3:latest", "qwen2.5:7b"})
}

func TestLoadedModels_LMStudio(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/models" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		// LM Studio v1 REST API: загружена ⇔ loaded_instances непуст; ключ — key.
		_, _ = w.Write([]byte(`{"models":[
			{"key":"loaded-a","loaded_instances":[{"id":"loaded-a"}]},
			{"key":"idle-b","loaded_instances":[]},
			{"key":"loaded-c","loaded_instances":[{"id":"loaded-c"}]}
		]}`))
	}))
	defer srv.Close()

	o := newProbeOpenAI(t, srv.URL, ProbeLMStudio)
	got, supported, err := o.LoadedModels(context.Background())
	if err != nil || !supported {
		t.Fatalf("LoadedModels err=%v supported=%v", err, supported)
	}
	assertEqualStrings(t, got, []string{"loaded-a", "loaded-c"})
}

func TestLoadedModels_LlamaCpp(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"qwen3-8b","status":"loaded"},{"id":"llama-3.2-3b","status":"unloaded"}]}`))
	}))
	defer srv.Close()

	o := newProbeOpenAI(t, srv.URL, ProbeLlamaCpp)
	got, supported, err := o.LoadedModels(context.Background())
	if err != nil || !supported {
		t.Fatalf("LoadedModels err=%v supported=%v", err, supported)
	}
	assertEqualStrings(t, got, []string{"qwen3-8b"})
}

func TestLoadedModels_Unsupported(t *testing.T) {
	o := newProbeOpenAI(t, "http://127.0.0.1:1", "") // probeKind="" → no probe
	got, supported, err := o.LoadedModels(context.Background())
	if supported {
		t.Fatalf("expected supported=false for empty probeKind")
	}
	if err != nil || got != nil {
		t.Fatalf("expected (nil,false,nil), got (%v,%v,%v)", got, supported, err)
	}
}

func TestLoadedModels_EmptyMemory(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"models":[]}`))
	}))
	defer srv.Close()

	o := newProbeOpenAI(t, srv.URL, ProbeOllama)
	got, supported, err := o.LoadedModels(context.Background())
	if err != nil || !supported {
		t.Fatalf("LoadedModels err=%v supported=%v", err, supported)
	}
	if len(got) != 0 {
		t.Fatalf("expected empty slice, got %v", got)
	}
}

func assertEqualStrings(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("len mismatch: got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("at %d: got %q want %q (full got=%v)", i, got[i], want[i], got)
		}
	}
}
