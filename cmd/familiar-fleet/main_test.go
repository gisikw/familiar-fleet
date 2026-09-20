package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestCommandModes(t *testing.T) {
	cases := []struct {
		args    []string
		command string
		pos     []string
	}{
		{nil, "interactive", nil},
		{[]string{"connect", "https://familiar.example.com"}, "connect", []string{"https://familiar.example.com"}},
		{[]string{"--state-dir", "/var/lib/familiar-fleet", "herdr"}, "herdr", nil},
		{[]string{"tunnel"}, "tunnel", nil},
		{[]string{"run"}, "run", nil},
		{[]string{"version"}, "version", nil},
		{[]string{"runtime", "apply", "github:gisikw/familiar/abc123#familiar-worker-runtime"}, "runtime", []string{"apply", "github:gisikw/familiar/abc123#familiar-worker-runtime"}},
		{[]string{"runtime", "rollback"}, "runtime", []string{"rollback"}},
		{[]string{"--state-dir", "/tmp/s", "runtime", "status"}, "runtime", []string{"status"}},
	}
	for _, tc := range cases {
		inv, err := parseInvocation(tc.args, &bytes.Buffer{})
		if err != nil {
			t.Fatalf("%v: %v", tc.args, err)
		}
		if inv.command != tc.command || strings.Join(inv.args, "|") != strings.Join(tc.pos, "|") {
			t.Fatalf("%v parsed as command=%q args=%v", tc.args, inv.command, inv.args)
		}
	}
}

func TestConnectRequiresURL(t *testing.T) {
	if _, err := parseInvocation([]string{"connect"}, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "connect <familiar-url>") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRuntimeSubcommandUsage(t *testing.T) {
	for _, args := range [][]string{
		{"runtime"},
		{"runtime", "apply"},
		{"runtime", "apply", "a", "b"},
		{"runtime", "rollback", "x"},
		{"runtime", "status", "x"},
		{"runtime", "bogus"},
	} {
		if _, err := parseInvocation(args, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "runtime apply <installable>") {
			t.Errorf("%v: unexpected error %v", args, err)
		}
	}
}

func TestUsageDocumentsRuntimeFlow(t *testing.T) {
	var out bytes.Buffer
	_, err := parseInvocation([]string{"-h"}, &out)
	if err == nil {
		t.Fatal("expected flag.ErrHelp")
	}
	for _, want := range []string{"runtime apply github:gisikw/familiar/<commit>#familiar-worker-runtime", "runtime rollback", "runtime status"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("usage missing %q:\n%s", want, out.String())
		}
	}
}
