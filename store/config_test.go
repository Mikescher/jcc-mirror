package store

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"testing"
	"time"
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

func TestValueGetters(t *testing.T) {
	v := Values{
		KeyScanWorkers:    " 12 ",
		KeyMTimeTolerance: "5s",
		KeyChunkSize:      "8MiB",
		KeyHashAfterCopy:  "true",
	}
	if got := v.Int(KeyScanWorkers); got != 12 {
		t.Errorf("Int = %d, want 12", got)
	}
	if got := v.Duration(KeyMTimeTolerance); got != 5*time.Second {
		t.Errorf("Duration = %v, want 5s", got)
	}
	if got := v.Size(KeyChunkSize); got != 8<<20 {
		t.Errorf("Size = %d, want %d", got, 8<<20)
	}
	if got := v.Bool(KeyHashAfterCopy); !got {
		t.Error("Bool = false, want true")
	}
}

// A stored value is validated before it is written, so a parse failure here means
// the table was edited by hand. The registry's default is a better answer than a
// zero: zero workers never scan and a zero mtime tolerance re-transfers 30 TB.
func TestValueGettersFallBackToTheDefault(t *testing.T) {
	v := Values{
		KeyScanWorkers:    "lots",
		KeyMTimeTolerance: "soon",
		KeyChunkSize:      "huge",
		KeyTransferChunks: "",
	}
	if got := v.Int(KeyScanWorkers); got != 8 {
		t.Errorf("Int = %d, want the default 8", got)
	}
	if got := v.Duration(KeyMTimeTolerance); got != 2*time.Second {
		t.Errorf("Duration = %v, want the default 2s", got)
	}
	if got := v.Size(KeyChunkSize); got != 64<<20 {
		t.Errorf("Size = %d, want the default %d", got, 64<<20)
	}
	if got := v.Int(KeyTransferChunks); got != 4 {
		t.Errorf("Int of an empty value = %d, want the default 4", got)
	}
}

func TestConfigDefaultsAreUsable(t *testing.T) {
	values, err := newStore(t).Config(context.Background())
	if err != nil {
		t.Fatalf("Config: %v", err)
	}

	if got := values.Duration(KeyMTimeTolerance); got < time.Second {
		t.Errorf("mtime tolerance = %v; WebDAV dates carry whole seconds, so it must never be below one", got)
	}
	if got := values.Int(KeyScanWorkers); got < 1 {
		t.Errorf("scan workers = %d, want at least 1", got)
	}
	if got := values.Int(KeyTransferChunks); got < 1 {
		t.Errorf("transfer chunks = %d, want at least 1", got)
	}
	if got := values.Int(KeyMaxAttempts); got < 1 {
		t.Errorf("max attempts = %d, want at least 1", got)
	}
	if got := values.Size(KeyChunkSize); got < 1 {
		t.Errorf("chunk size = %d, want a positive default", got)
	}
	if got := values.Duration(KeyRetryBackoff); got <= 0 {
		t.Errorf("retry backoff = %v, want a positive default", got)
	}
	if values.Bool(KeyHashAfterCopy) {
		t.Error("hashing after copy defaults to on; it costs a second read of 30 TB")
	}
}

// A default the registry itself would reject is a setting that cannot be saved
// from the setup view without being changed first.
func TestKeyDefaultsPassTheirOwnValidator(t *testing.T) {
	for _, d := range Keys() {
		if d.Default == "" || d.Validate == nil {
			continue
		}
		t.Run(d.Name, func(t *testing.T) {
			if err := d.Validate(d.Default); err != nil {
				t.Errorf("default %q is rejected by its own validator: %v", d.Default, err)
			}
		})
	}
}

// Every tunable the engine reads through a typed getter needs a default: without
// one an unconfigured mirror runs with a zero.
func TestTunablesHaveDefaults(t *testing.T) {
	for _, name := range []string{
		KeyScanWorkers, KeyMTimeTolerance, KeyTransferChunks,
		KeyChunkSize, KeyMaxAttempts, KeyRetryBackoff, KeyHashAfterCopy,
	} {
		d, ok := Key(name)
		if !ok {
			t.Errorf("%s is not in the registry", name)
			continue
		}
		if d.Default == "" {
			t.Errorf("%s has no default", name)
		}
		if d.Validate == nil {
			t.Errorf("%s has no validator, so the setup view accepts anything", name)
		}
	}
}
