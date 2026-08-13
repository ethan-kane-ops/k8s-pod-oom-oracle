package cmd

import (
	"bytes"
	"strings"
	"testing"
)

func TestNewRootCmdIsIndependentPerCall(t *testing.T) {
	t.Parallel()

	first, second := NewRootCmd(), NewRootCmd()
	if first == second {
		t.Fatal("NewRootCmd() returned the same pointer twice; command state would be shared across tests")
	}

	// Mutating one tree must not leak into the other.
	first.SetArgs([]string{"version", "-o", "json"})
	var firstOut bytes.Buffer
	first.SetOut(&firstOut)
	if err := first.Execute(); err != nil {
		t.Fatalf("first tree Execute() error = %v", err)
	}

	var secondOut bytes.Buffer
	second.SetOut(&secondOut)
	second.SetArgs([]string{"version"})
	if err := second.Execute(); err != nil {
		t.Fatalf("second tree Execute() error = %v", err)
	}

	if strings.HasPrefix(strings.TrimSpace(secondOut.String()), "{") {
		t.Error("second tree inherited the --output=json flag from the first")
	}
}

func TestRootCmdMetadata(t *testing.T) {
	t.Parallel()

	root := NewRootCmd()

	if root.Use != "oom-oracle" {
		t.Errorf("root.Use = %q, want %q", root.Use, "oom-oracle")
	}
	if !root.SilenceUsage {
		t.Error("root.SilenceUsage = false; runtime errors would dump usage text")
	}
	if !root.SilenceErrors {
		t.Error("root.SilenceErrors = false; errors would be printed twice")
	}
	if root.Version == "" {
		t.Error("root.Version is empty; --version would be unavailable")
	}
}

func TestRootCmdRegistersSubcommands(t *testing.T) {
	t.Parallel()

	want := map[string]bool{"version": false}

	for _, sub := range NewRootCmd().Commands() {
		if _, ok := want[sub.Name()]; ok {
			want[sub.Name()] = true
		}
	}

	for name, found := range want {
		if !found {
			t.Errorf("subcommand %q is not registered on the root command", name)
		}
	}
}

func TestRootCmdWithNoArgsShowsHelp(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	root := NewRootCmd()
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{})

	if err := root.Execute(); err != nil {
		t.Fatalf("Execute() with no args error = %v", err)
	}
	if !strings.Contains(out.String(), "Usage:") {
		t.Errorf("output = %q, want help text containing %q", out.String(), "Usage:")
	}
}

func TestRootCmdRejectsUnknownSubcommand(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	root := NewRootCmd()
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"nope"})

	if err := root.Execute(); err == nil {
		t.Fatal("Execute() with an unknown subcommand = nil error, want error")
	}
}
