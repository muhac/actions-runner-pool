package main

import (
	"strings"
	"testing"

	"github.com/muhac/actions-runner-pool/internal/store"
)

func TestCheckBaseURLDrift(t *testing.T) {
	cases := []struct {
		name       string
		existing   *store.AppConfig
		configured string
		wantWarn   bool
		wantSubs   []string
	}{
		{
			name:       "nil existing — no warn",
			existing:   nil,
			configured: "https://example.test",
			wantWarn:   false,
		},
		{
			name:       "equal — no warn",
			existing:   &store.AppConfig{BaseURL: "https://example.test"},
			configured: "https://example.test",
			wantWarn:   false,
		},
		{
			name:       "different — warn with both URLs",
			existing:   &store.AppConfig{BaseURL: "https://old.example.test"},
			configured: "https://new.example.test",
			wantWarn:   true,
			wantSubs:   []string{"https://old.example.test", "https://new.example.test"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			warn, msg := checkBaseURLDrift(tc.existing, tc.configured)
			if warn != tc.wantWarn {
				t.Fatalf("warn = %v, want %v (msg=%q)", warn, tc.wantWarn, msg)
			}
			if !warn && msg != "" {
				t.Errorf("expected empty msg when warn=false, got %q", msg)
			}
			for _, sub := range tc.wantSubs {
				if !strings.Contains(msg, sub) {
					t.Errorf("msg missing %q; got %q", sub, msg)
				}
			}
		})
	}
}
