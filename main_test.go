package main

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
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
