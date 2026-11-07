package dispatch

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestParseTargetsEmpty(t *testing.T) {
	for _, spec := range []string{"", "   ", "null", "[]"} {
		got, err := ParseTargets(spec)
		if err != nil {
			t.Fatalf("ParseTargets(%q) error: %v", spec, err)
		}
		if len(got) != 0 {
			t.Fatalf("ParseTargets(%q) = %v, want none", spec, got)
		}
	}
}

func TestParseTargetsFull(t *testing.T) {
	spec := `[{"name":"n","tables":"tenant-*","url":"http://example.com/x","batchSize":50,"batchWindow":"250ms","headers":{"k":"v"}}]`
	got, err := ParseTargets(spec)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d targets", len(got))
	}
	tt := got[0]
	if tt.Name != "n" || tt.Tables != "tenant-*" || tt.URL != "http://example.com/x" {
		t.Fatalf("target = %+v", tt)
	}
	if tt.BatchSize != 50 {
		t.Errorf("BatchSize = %d", tt.BatchSize)
	}
	if tt.BatchWindow != 250*time.Millisecond {
		t.Errorf("BatchWindow = %v", tt.BatchWindow)
	}
	if tt.Headers["k"] != "v" {
		t.Errorf("Headers = %v", tt.Headers)
	}
}

func TestParseTargetsDefaults(t *testing.T) {
	got, err := ParseTargets(`[{"name":"n","tables":"*","url":"http://x"}]`)
	if err != nil {
		t.Fatal(err)
	}
	tt := got[0]
	if tt.BatchSize != 100 {
		t.Errorf("BatchSize = %d, want 100", tt.BatchSize)
	}
	if tt.BatchWindow != 0 {
		t.Errorf("BatchWindow = %v, want 0", tt.BatchWindow)
	}
}

func TestParseTargetsFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "targets.json")
	if err := os.WriteFile(p, []byte(`[{"name":"n","tables":"*","url":"http://x"}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := ParseTargets("@" + p)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Name != "n" {
		t.Fatalf("got %+v", got)
	}
}

func TestParseTargetsErrors(t *testing.T) {
	cases := map[string]string{
		"not json":        `nope`,
		"missing name":    `[{"tables":"*","url":"http://x"}]`,
		"missing tables":  `[{"name":"n","url":"http://x"}]`,
		"missing url":     `[{"name":"n","tables":"*"}]`,
		"bad glob":        `[{"name":"n","tables":"[","url":"http://x"}]`,
		"bad url":         `[{"name":"n","tables":"*","url":"not a url"}]`,
		"batch too big":   `[{"name":"n","tables":"*","url":"http://x","batchSize":1001}]`,
		"batch too small": `[{"name":"n","tables":"*","url":"http://x","batchSize":0}]`,
		"bad window":      `[{"name":"n","tables":"*","url":"http://x","batchWindow":"xyz"}]`,
		"missing file":    `@/nonexistent/path/targets.json`,
	}
	for name, spec := range cases {
		if _, err := ParseTargets(spec); err == nil {
			t.Errorf("%s: ParseTargets(%q) succeeded, want error", name, spec)
		}
	}
}

func TestParseTargetsTimeout(t *testing.T) {
	got, err := ParseTargets(`[{"name":"a","tables":"*","url":"http://x","timeout":"5m"}]`)
	if err != nil || len(got) != 1 || got[0].Timeout != 5*time.Minute || got[0].timeout() != 5*time.Minute {
		t.Fatalf("timeout 5m: %+v, %v", got, err)
	}
	got, err = ParseTargets(`[{"name":"a","tables":"*","url":"http://x"}]`)
	if err != nil || got[0].timeout() != postTimeout {
		t.Fatalf("default timeout = %v, %v; want %v", got[0].timeout(), err, postTimeout)
	}
	for _, bad := range []string{`"0s"`, `"-1s"`, `"16m"`, `"soon"`} {
		if _, err := ParseTargets(`[{"name":"a","tables":"*","url":"http://x","timeout":` + bad + `}]`); err == nil {
			t.Errorf("timeout %s accepted, want an error", bad)
		}
	}
}
