package cmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// failingWriter reports an error on every write, exercising the I/O error path.
type failingWriter struct{}

var errWriteFailed = errors.New("write failed")

func (failingWriter) Write([]byte) (int, error) { return 0, errWriteFailed }

func TestWriteVersion(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		format      string
		wantSubstr  string
		wantJSON    bool
		wantErrPart string
	}{
		{name: "text format", format: "text", wantSubstr: "version:"},
		{name: "json format", format: "json", wantJSON: true},
		{name: "unknown format", format: "yaml", wantErrPart: `unknown output format "yaml"`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var buf bytes.Buffer
			err := writeVersion(&buf, tt.format)

			if tt.wantErrPart != "" {
				if err == nil {
					t.Fatalf("writeVersion(%q) = nil error, want error", tt.format)
				}
				if !strings.Contains(err.Error(), tt.wantErrPart) {
					t.Fatalf("error = %q, want it to contain %q", err, tt.wantErrPart)
				}
				if buf.Len() != 0 {
					t.Errorf("wrote %q on an invalid format, want no output", buf.String())
				}
				return
			}

			if err != nil {
				t.Fatalf("writeVersion(%q) error = %v", tt.format, err)
			}
			if tt.wantSubstr != "" && !strings.Contains(buf.String(), tt.wantSubstr) {
				t.Errorf("output = %q, missing %q", buf.String(), tt.wantSubstr)
			}
			if tt.wantJSON && !json.Valid(buf.Bytes()) {
				t.Errorf("output = %q, want valid JSON", buf.String())
			}
		})
	}
}

func TestWriteVersionWrapsWriteError(t *testing.T) {
	t.Parallel()

	err := writeVersion(failingWriter{}, formatText)
	if err == nil {
		t.Fatal("writeVersion() = nil error, want the underlying write error")
	}
	if !errors.Is(err, errWriteFailed) {
		t.Errorf("error = %v, want it to wrap errWriteFailed", err)
	}
}

func TestVersionCommandOutput(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		args     []string
		wantJSON bool
		wantErr  bool
	}{
		{name: "default text", args: []string{"version"}},
		{name: "explicit json", args: []string{"version", "--output", "json"}, wantJSON: true},
		{name: "short flag json", args: []string{"version", "-o", "json"}, wantJSON: true},
		{name: "rejects arguments", args: []string{"version", "extra"}, wantErr: true},
		{name: "rejects bad format", args: []string{"version", "-o", "xml"}, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var out bytes.Buffer
			root := NewRootCmd()
			root.SetOut(&out)
			root.SetErr(&out)
			root.SetArgs(tt.args)

			err := root.Execute()

			if tt.wantErr {
				if err == nil {
					t.Fatalf("Execute(%v) = nil error, want error", tt.args)
				}
				return
			}
			if err != nil {
				t.Fatalf("Execute(%v) error = %v", tt.args, err)
			}
			if tt.wantJSON && !json.Valid(out.Bytes()) {
				t.Errorf("output = %q, want valid JSON", out.String())
			}
			if out.Len() == 0 {
				t.Error("version command produced no output")
			}
		})
	}
}
