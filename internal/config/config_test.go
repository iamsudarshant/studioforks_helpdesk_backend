package config

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestKEK(t *testing.T) {
	b64 := func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

	t.Run("accepts exactly 32 bytes", func(t *testing.T) {
		s := Storage{MasterKEK: b64("0123456789abcdef0123456789abcdef")}
		key, err := s.KEK()
		if err != nil {
			t.Fatalf("expected a valid key, got %v", err)
		}
		if len(key) != 32 {
			t.Fatalf("expected 32 bytes, got %d", len(key))
		}
	})

	t.Run("tolerates surrounding whitespace", func(t *testing.T) {
		// A key pasted into .env often carries a trailing space or newline.
		s := Storage{MasterKEK: "  " + b64("0123456789abcdef0123456789abcdef") + "  "}
		if _, err := s.KEK(); err != nil {
			t.Fatalf("expected whitespace to be trimmed, got %v", err)
		}
	})

	t.Run("rejects the wrong length and says how to fix it", func(t *testing.T) {
		// 34 bytes: the exact mistake shipped in .env.example, where a 34-char
		// passphrase was base64-encoded and looked plausible.
		s := Storage{MasterKEK: b64("change-me-32-byte-key-for-develop!")}
		_, err := s.KEK()
		if err == nil {
			t.Fatal("expected a 34-byte key to be rejected")
		}
		if !strings.Contains(err.Error(), "openssl rand -base64 32") {
			t.Fatalf("error must tell the operator how to generate a key, got: %v", err)
		}
	})

	t.Run("rejects values that are not base64", func(t *testing.T) {
		if _, err := (Storage{MasterKEK: "not base64!!"}).KEK(); err == nil {
			t.Fatal("expected non-base64 to be rejected")
		}
	})
}

func TestIsPlaceholderKEK(t *testing.T) {
	b64 := func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

	// The regression this guards: the check used to run on the encoded string,
	// so a published placeholder sailed through into production.
	placeholders := []string{
		"change-me-in-production-32bytes!",  // current .env.example
		"change-me-32-byte-key-for-develop", // previous .env.example
		"dev-only-32-byte-master-key-1234",  // local development value
	}
	for _, p := range placeholders {
		if !isPlaceholderKEK(b64(p)) {
			t.Errorf("placeholder %q must be recognised", p)
		}
	}

	t.Run("a real random key is not a placeholder", func(t *testing.T) {
		if isPlaceholderKEK("S9AFPTqkhCU92XtFh6rGWmI7m0BGlDQtT7xnu6OqvV8=") {
			t.Error("a randomly generated key must not be flagged")
		}
	})

	t.Run("undecodable values are left to KEK to report", func(t *testing.T) {
		if isPlaceholderKEK("not base64!!") {
			t.Error("undecodable input must not be reported as a placeholder")
		}
	})
}

func TestValidateRejectsPlaceholdersInProduction(t *testing.T) {
	realKey := base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))
	exampleKey := base64.StdEncoding.EncodeToString([]byte("change-me-in-production-32bytes!"))

	base := func() *Config {
		c := &Config{}
		c.App.Env = "production"
		c.App.BaseURL = "https://helpdesk.example.com"
		c.Auth.JWTSecret = strings.Repeat("s", 48)
		c.Storage.Driver = "local"
		c.Storage.MasterKEK = realKey
		c.Tenancy.AllowedOrigins = []string{"https://helpdesk.example.com"}
		return c
	}

	t.Run("a sound production config passes", func(t *testing.T) {
		if err := base().validate(); err != nil {
			t.Fatalf("expected the config to validate, got %v", err)
		}
	})

	t.Run("the example key is refused", func(t *testing.T) {
		c := base()
		c.Storage.MasterKEK = exampleKey
		err := c.validate()
		if err == nil {
			t.Fatal("production must not boot with the shipped example key")
		}
		if !strings.Contains(err.Error(), "MASTER_KEK still holds the development placeholder") {
			t.Fatalf("expected the placeholder to be named, got: %v", err)
		}
	})

	t.Run("the example JWT secret is refused", func(t *testing.T) {
		c := base()
		c.Auth.JWTSecret = "change-me-to-a-long-random-value-at-least-32-bytes"
		if err := c.validate(); err == nil {
			t.Fatal("production must not boot with the example JWT secret")
		}
	})

	t.Run("a default tenant slug is refused", func(t *testing.T) {
		// In production every request must resolve its own tenant; a fallback
		// would silently serve one workspace's data for an unmatched host.
		c := base()
		c.Tenancy.DefaultSlug = "demo"
		if err := c.validate(); err == nil {
			t.Fatal("production must not boot with TENANT_DEFAULT_SLUG set")
		}
	})

	t.Run("placeholders are allowed in development", func(t *testing.T) {
		c := base()
		c.App.Env = "development"
		c.App.BaseURL = "http://localhost:8090"
		c.Storage.MasterKEK = exampleKey
		c.Tenancy.DefaultSlug = "demo"
		if err := c.validate(); err != nil {
			t.Fatalf("development must tolerate the example values, got %v", err)
		}
	})
}

func TestWithClientFoundRows(t *testing.T) {
	// Matched-row semantics are load-bearing: repositories use affected-rows to
	// decide whether a row exists, and MySQL's default (changed rows) makes an
	// unchanged UPDATE indistinguishable from a missing record.
	t.Run("appends to existing params", func(t *testing.T) {
		got := withClientFoundRows("parseTime=true&loc=UTC")
		if !strings.Contains(got, "clientFoundRows=true") {
			t.Fatalf("expected the flag to be appended, got %q", got)
		}
		if !strings.Contains(got, "parseTime=true") {
			t.Fatalf("existing params must survive, got %q", got)
		}
	})

	t.Run("handles empty params", func(t *testing.T) {
		if got := withClientFoundRows(""); got != "clientFoundRows=true" {
			t.Fatalf("got %q", got)
		}
	})

	t.Run("does not duplicate an explicit setting", func(t *testing.T) {
		got := withClientFoundRows("clientFoundRows=false")
		if strings.Count(got, "clientFoundRows") != 1 {
			t.Fatalf("an explicit setting must be left alone, got %q", got)
		}
	})

	t.Run("the built DSN carries the flag", func(t *testing.T) {
		d := DB{Host: "127.0.0.1", Port: 3306, Name: "complydesk", User: "root",
			Params: "parseTime=true"}
		if !strings.Contains(d.DSN(), "clientFoundRows=true") {
			t.Fatalf("DSN must request matched-row semantics, got %q", d.DSN())
		}
	})
}
