package store

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"testing"
)

func TestConfigDefaultsAndSet(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	values, err := s.Config(ctx)
	if err != nil {
		t.Fatalf("Config: %v", err)
	}
	if got := values.Get(KeyTimezone); got != "Europe/Berlin" {
		t.Errorf("default timezone = %q, want Europe/Berlin", got)
	}
	if got := values.Get(KeyRemoteURL); got != "" {
		t.Errorf("unset key = %q, want empty", got)
	}

	changed, err := s.ConfigSet(ctx, map[string]string{KeyRemoteURL: "http://10.13.13.2:5005/media"}, "test")
	if err != nil {
		t.Fatalf("ConfigSet: %v", err)
	}
	if len(changed) != 1 || changed[0] != KeyRemoteURL {
		t.Errorf("changed = %v, want [%s]", changed, KeyRemoteURL)
	}

	// Writing the same value again is not a change, so it must not fill the audit
	// trail with rows that say nothing happened.
	changed, err = s.ConfigSet(ctx, map[string]string{KeyRemoteURL: "http://10.13.13.2:5005/media"}, "test")
	if err != nil {
		t.Fatalf("ConfigSet: %v", err)
	}
	if len(changed) != 0 {
		t.Errorf("re-setting the same value reported %v as changed", changed)
	}
}

func TestConfigRejectsUnknownKey(t *testing.T) {
	s := newStore(t)

	_, err := s.ConfigSet(context.Background(), map[string]string{"wg.privatekey": "x"}, "test")
	if !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("error = %v, want ErrUnknownKey", err)
	}
}

func TestConfigValidation(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	cases := map[string]struct{ key, value string }{
		"default route":  {KeyWGAllowedIPs, "0.0.0.0/0"},
		"endpoint":       {KeyWGEndpoint, "rootserver"},
		"not base64":     {KeyWGPeerKey, "definitely not a key"},
		"short key":      {KeyWGPeerKey, "aGVsbG8="},
		"scheme missing": {KeyRemoteURL, "10.13.13.2:5005/media"},
		"timezone":       {KeyTimezone, "Middle/Earth"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := s.ConfigSet(ctx, map[string]string{c.key: c.value}, "test")
			var invalid *ValidationError
			if !errors.As(err, &invalid) {
				t.Fatalf("error = %v, want a ValidationError", err)
			}
		})
	}
}

// A batch is all-or-nothing: half-applied tunnel settings would open a tunnel
// nobody asked for.
func TestConfigSetIsAtomic(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	_, err := s.ConfigSet(ctx, map[string]string{
		KeyWGAddress:    "10.13.13.3/32",
		KeyWGAllowedIPs: "0.0.0.0/0",
	}, "test")
	if err == nil {
		t.Fatal("ConfigSet succeeded, want a validation error")
	}

	got, err := s.ConfigGet(ctx, KeyWGAddress)
	if err != nil {
		t.Fatalf("ConfigGet: %v", err)
	}
	if got != "" {
		t.Errorf("the valid half of a rejected batch was written: %q", got)
	}
}

func TestConfigAuditMasksSecrets(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	if _, err := s.ConfigSet(ctx, map[string]string{
		KeyRemotePassword: "hunter2",
		KeyRemoteUser:     "ro",
	}, "test"); err != nil {
		t.Fatalf("ConfigSet: %v", err)
	}

	entries, err := s.ConfigAudit(ctx, 10)
	if err != nil {
		t.Fatalf("ConfigAudit: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("got %d audit entries, want 2", len(entries))
	}
	for _, e := range entries {
		switch e.Key {
		case KeyRemotePassword:
			if e.New != SecretMask {
				t.Errorf("audit recorded the password as %q", e.New)
			}
		case KeyRemoteUser:
			if e.New != "ro" {
				t.Errorf("audit of a non-secret = %q, want ro", e.New)
			}
		}
	}

	// The value itself is still readable, of course - only the trail is masked.
	got, err := s.ConfigGet(ctx, KeyRemotePassword)
	if err != nil {
		t.Fatalf("ConfigGet: %v", err)
	}
	if got != "hunter2" {
		t.Errorf("stored password = %q", got)
	}
}

func TestEnsureGeneratedRunsOnce(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	calls := 0
	gen := func(key string) (string, error) {
		calls++
		if key == KeyWGPrivateKey {
			return base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32)), nil
		}
		return "generated-" + key, nil
	}

	created, err := s.EnsureGenerated(ctx, gen)
	if err != nil {
		t.Fatalf("EnsureGenerated: %v", err)
	}
	if len(created) == 0 {
		t.Fatal("nothing was generated")
	}

	again, err := s.EnsureGenerated(ctx, gen)
	if err != nil {
		t.Fatalf("EnsureGenerated: %v", err)
	}
	if len(again) != 0 {
		t.Errorf("second run generated %v, want nothing", again)
	}
	if calls != len(created) {
		t.Errorf("generator called %d times for %d keys", calls, len(created))
	}
}

func TestEveryKeyIsInAGroup(t *testing.T) {
	for _, d := range Keys() {
		if d.Group == "" || d.Label == "" {
			t.Errorf("%s has no group or label, so the setup view cannot render it", d.Name)
		}
	}
}
