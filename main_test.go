package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"
)

func TestWriteErrFormat(t *testing.T) {
	rec := httptest.NewRecorder()
	writeErr(rec, 400, "BAD", "msg")
	if rec.Code != 400 {
		t.Fatalf("status = %d", rec.Code)
	}
	var body struct {
		Error struct{ Code, Message string } `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Error.Code != "BAD" || body.Error.Message != "msg" {
		t.Errorf("unexpected body: %+v", body)
	}
}

func TestOrderNoRegex(t *testing.T) {
	for _, s := range []string{"ORD-2587965432159-458"} {
		if !orderNoRe.MatchString(s) {
			t.Errorf("%q should be valid", s)
		}
	}
	for _, s := range []string{"", "abc", "ORD-1", "ORD--1", "ord-1-2", "ORD-1-2-3", "ORD-a-1"} {
		if orderNoRe.MatchString(s) {
			t.Errorf("%q should be invalid", s)
		}
	}
}

func sign(secret string, body []byte) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write(body)
	return hex.EncodeToString(m.Sum(nil))
}

func TestValidSignature(t *testing.T) {
	body := []byte(`{"eventId":"e1","orderNo":"ORD-1-1","type":"payment.succeeded"}`)
	good := sign("s3cret", body)

	cases := []struct {
		name   string
		secret string
		body   []byte
		sig    string
		want   bool
	}{
		{"ลายเซ็นถูก", "s3cret", body, good, true},
		{"secret ผิด", "other", body, good, false},
		{"body ถูกแก้", "s3cret", []byte(`{"eventId":"e2"}`), good, false},
		{"ลายเซ็นไม่ใช่ hex", "s3cret", body, "zzzz", false},
		{"ลายเซ็นว่าง", "s3cret", body, "", false},
		{"ลายเซ็นสั้นเกิน", "s3cret", body, "abcd", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := validSignature(c.secret, c.body, c.sig); got != c.want {
				t.Errorf("got %v want %v", got, c.want)
			}
		})
	}
}

func TestBackoffDelay(t *testing.T) {
	cases := []struct {
		base, attempts int
		want           time.Duration
	}{
		{5, 0, 5 * time.Second},
		{5, 1, 10 * time.Second},
		{5, 2, 20 * time.Second},
		{10, 2, 40 * time.Second},
		{5, -1, 5 * time.Second},
	}
	for _, c := range cases {
		if got := backoffDelay(c.base, c.attempts); got != c.want {
			t.Errorf("base=%d attempts=%d got %v want %v", c.base, c.attempts, got, c.want)
		}
	}
}
