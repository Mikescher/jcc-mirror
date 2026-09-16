package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// newTestClient serves handler and returns a client pointed at it, so no test
// ever reaches simplecloudnotifier.de.
func newTestClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return &Client{Endpoint: srv.URL, HTTP: srv.Client()}
}

// capture records the decoded body of the one request a test sends and answers
// with status. The map is filled in by the time Send returns.
func capture(t *testing.T, status int, body string) (*Client, map[string]any) {
	t.Helper()
	got := map[string]any{}

	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method is %s, want POST", r.Method)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type is %q, want application/json", ct)
		}
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read body: %v", err)
		}
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber() // keeps a JSON number distinguishable from a quoted one
		if err := dec.Decode(&got); err != nil {
			t.Errorf("decode body %q: %v", raw, err)
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	})
	return client, got
}

func TestSendEncodesMinimalBody(t *testing.T) {
	client, body := capture(t, http.StatusOK, `{"success":true}`)

	before := time.Now().Unix()
	err := client.Send(context.Background(), Config{UserID: "USR42", UserKey: "key"}, Message{Title: "sync failed"})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	after := time.Now().Unix()

	if body["user_id"] != "USR42" {
		t.Errorf("user_id is %T (%v), want the string USR42", body["user_id"], body["user_id"])
	}
	if body["user_key"] != "key" {
		t.Errorf("user_key is %v, want key", body["user_key"])
	}
	if body["title"] != "sync failed" {
		t.Errorf("title is %v, want sync failed", body["title"])
	}

	// Optional fields are absent rather than sent empty.
	for _, k := range []string{"content", "channel", "sender_name", "msg_id"} {
		if v, present := body[k]; present {
			t.Errorf("%s should be omitted when blank, got %v", k, v)
		}
	}

	// Priority is always sent, even at its zero value.
	prio, ok := body["priority"].(json.Number)
	if !ok || prio.String() != "0" {
		t.Errorf("priority is %v (%T), want the number 0", body["priority"], body["priority"])
	}

	ts, ok := body["timestamp"].(json.Number)
	if !ok {
		t.Fatalf("timestamp is %T (%v), want a JSON number", body["timestamp"], body["timestamp"])
	}
	secs, err := ts.Int64()
	if err != nil {
		t.Fatalf("timestamp %s: %v", ts, err)
	}
	if secs < before || secs > after {
		t.Errorf("timestamp is %d, want it filled in between %d and %d", secs, before, after)
	}
}

func TestSendEncodesFullBody(t *testing.T) {
	client, body := capture(t, http.StatusOK, "")

	when := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	cfg := Config{UserID: "7", UserKey: "k", Channel: "jcc-mirror", Sender: "nas"}
	msg := Message{
		Title:     "deletion guard tripped",
		Content:   "1204 of 4001 files would go",
		Priority:  2,
		MsgID:     MsgID("delete.blocked", "3", "2026-09-03"),
		Timestamp: when,
	}
	if err := client.Send(context.Background(), cfg, msg); err != nil {
		t.Fatalf("Send: %v", err)
	}

	want := map[string]string{
		"content":     msg.Content,
		"channel":     cfg.Channel,
		"sender_name": cfg.Sender,
		"msg_id":      msg.MsgID,
	}
	for k, v := range want {
		if body[k] != v {
			t.Errorf("%s is %v, want %q", k, body[k], v)
		}
	}
	if prio, _ := body["priority"].(json.Number); prio.String() != "2" {
		t.Errorf("priority is %v, want 2", body["priority"])
	}
	if ts, _ := body["timestamp"].(json.Number); ts.String() != strconv.FormatInt(when.Unix(), 10) {
		t.Errorf("timestamp is %v, want %d", body["timestamp"], when.Unix())
	}
}

func TestSendQuotaExhausted(t *testing.T) {
	client, _ := capture(t, http.StatusForbidden, `{"success":false,"message":"quota exceeded"}`)

	err := client.Send(context.Background(), Config{UserID: "1", UserKey: "k"}, Message{Title: "t"})
	if !errors.Is(err, ErrQuota) {
		t.Fatalf("Send on 403 returned %v, want an error wrapping ErrQuota", err)
	}
}

func TestSendServerError(t *testing.T) {
	client, _ := capture(t, http.StatusInternalServerError, "  upstream exploded  ")

	err := client.Send(context.Background(), Config{UserID: "1", UserKey: "k"}, Message{Title: "t"})
	if err == nil {
		t.Fatal("Send on 500 returned no error")
	}
	if errors.Is(err, ErrQuota) {
		t.Errorf("a 500 must not look like a quota error: %v", err)
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("error %q does not carry the status", err)
	}
	if !strings.Contains(err.Error(), "upstream exploded") {
		t.Errorf("error %q does not carry the body excerpt", err)
	}
	if strings.Contains(err.Error(), "  upstream") {
		t.Errorf("body excerpt should be trimmed: %q", err)
	}
}

func TestSendLargeErrorBodyIsCapped(t *testing.T) {
	client, _ := capture(t, http.StatusBadRequest, strings.Repeat("x", 10_000))

	err := client.Send(context.Background(), Config{UserID: "1", UserKey: "k"}, Message{Title: "t"})
	if err == nil {
		t.Fatal("Send on 400 returned no error")
	}
	if n := strings.Count(err.Error(), "x"); n != maxExcerpt {
		t.Errorf("error carries %d body bytes, want the excerpt capped at %d", n, maxExcerpt)
	}
}

func TestEnabled(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
		want bool
	}{
		{"empty", Config{}, false},
		{"id only", Config{UserID: "1"}, false},
		{"key only", Config{UserKey: "k"}, false},
		{"blank", Config{UserID: "  ", UserKey: "\t"}, false},
		{"pair", Config{UserID: "1", UserKey: "k"}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.cfg.Enabled(); got != tc.want {
				t.Errorf("Enabled() = %v, want %v", got, tc.want)
			}
		})
	}
}

// A rejected message must never leave the process, so these cases point at a
// server that fails the test if it is ever reached.
func TestSendRejectsBeforeSending(t *testing.T) {
	tests := []struct {
		name    string
		cfg     Config
		msg     Message
		wantErr error
		wantStr string
	}{
		{name: "not configured", cfg: Config{}, msg: Message{Title: "t"}, wantErr: ErrDisabled},
		{name: "half configured", cfg: Config{UserID: "1"}, msg: Message{Title: "t"}, wantErr: ErrDisabled},
		{name: "no title", cfg: Config{UserID: "1", UserKey: "k"}, msg: Message{}, wantStr: "title"},
		{name: "blank title", cfg: Config{UserID: "1", UserKey: "k"}, msg: Message{Title: "   "}, wantStr: "title"},
		{name: "priority too low", cfg: Config{UserID: "1", UserKey: "k"}, msg: Message{Title: "t", Priority: -1}, wantStr: "priority"},
		{name: "priority too high", cfg: Config{UserID: "1", UserKey: "k"}, msg: Message{Title: "t", Priority: 3}, wantStr: "priority"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				t.Errorf("%s reached the server", tc.name)
			})

			err := client.Send(context.Background(), tc.cfg, tc.msg)
			if err == nil {
				t.Fatalf("Send accepted %s", tc.name)
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Errorf("Send returned %v, want %v", err, tc.wantErr)
			}
			if tc.wantStr != "" && !strings.Contains(err.Error(), tc.wantStr) {
				t.Errorf("error %q does not name %q", err, tc.wantStr)
			}
		})
	}
}

func TestMsgID(t *testing.T) {
	id := MsgID("sync.failed", "3", "2026-09-03")
	if len(id) != 16 {
		t.Errorf("MsgID returned %q, want 16 hex characters", id)
	}
	if strings.Trim(id, "0123456789abcdef") != "" {
		t.Errorf("MsgID returned %q, want hex only", id)
	}
	if again := MsgID("sync.failed", "3", "2026-09-03"); again != id {
		t.Errorf("MsgID is not deterministic: %q then %q", id, again)
	}

	// Different inputs differ, including ones that would collide if the parts
	// were concatenated without a separator.
	others := [][]string{
		{"sync.failed", "3", "2026-09-04"},
		{"sync.failed", "4", "2026-09-03"},
		{"delete.blocked", "3", "2026-09-03"},
		{"sync.failed3", "", "2026-09-03"},
		{""},
	}
	seen := map[string]bool{id: true}
	for _, parts := range others {
		got := MsgID(parts...)
		if seen[got] {
			t.Errorf("MsgID(%q) = %q collides with an earlier id", parts, got)
		}
		seen[got] = true
	}
}
