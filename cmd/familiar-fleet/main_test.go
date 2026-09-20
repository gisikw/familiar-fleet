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
