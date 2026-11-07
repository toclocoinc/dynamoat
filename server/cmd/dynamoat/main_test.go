package main

import (
	"bytes"
	"fmt"
	"runtime"
	"runtime/debug"
	"testing"
)

func TestHandleArgs(t *testing.T) {
	oldVersion, oldCommit := version, commit
	version, commit = "v9.8.7", "deadbeef"
	t.Cleanup(func() { version, commit = oldVersion, oldCommit })

	tests := []struct {
		name       string
		args       []string
		wantCode   int
		wantStdout string
		wantStderr string
	}{
		{
			name:     "no arguments starts the server",
			args:     nil,
			wantCode: -1,
		},
		{
			name:       "version prints version, commit and go version",
			args:       []string{"--version"},
			wantCode:   0,
			wantStdout: fmt.Sprintf("dynamoat v9.8.7 (deadbeef, %s)\n", runtime.Version()),
		},
		{
			name:       "unknown argument prints usage and exits 2",
			args:       []string{"--bogus"},
			wantCode:   2,
			wantStderr: usage,
		},
		{
			name:       "version with an extra argument is an error",
			args:       []string{"--version", "extra"},
			wantCode:   2,
			wantStderr: usage,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := handleArgs(tt.args, &stdout, &stderr)
			if code != tt.wantCode {
				t.Fatalf("handleArgs(%q) code = %d, want %d", tt.args, code, tt.wantCode)
			}
			if got := stdout.String(); got != tt.wantStdout {
				t.Errorf("stdout = %q, want %q", got, tt.wantStdout)
			}
			if got := stderr.String(); got != tt.wantStderr {
				t.Errorf("stderr = %q, want %q", got, tt.wantStderr)
			}
		})
	}
}

func TestBuildCommitSet(t *testing.T) {
	old := commit
	commit = "abcdef0"
	t.Cleanup(func() { commit = old })

	if got := buildCommit(); got != "abcdef0" {
		t.Fatalf("buildCommit() = %q, want %q", got, "abcdef0")
	}
}

func TestBuildCommitFallsBackToVCS(t *testing.T) {
	old := commit
	commit = "unknown"
	t.Cleanup(func() { commit = old })

	got := buildCommit()
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, s := range info.Settings {
			if s.Key == "vcs.revision" {
				if got != s.Value {
					t.Fatalf("buildCommit() = %q, want vcs.revision %q", got, s.Value)
				}
				return
			}
		}
	}
	if got != "unknown" {
		t.Fatalf("buildCommit() = %q, want %q (no vcs.revision recorded)", got, "unknown")
	}
}
