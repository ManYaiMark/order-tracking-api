package main

import "testing"

func TestCanTransition(t *testing.T) {
	cases := []struct {
		from, to Status
		want     bool
	}{
		{PendingPayment, Queued, true},
		{Queued, InProgress, true},
		{InProgress, Completed, true},
		{InProgress, Queued, true},
		{Failed, Queued, true},
		{Expired, Refunded, true},

		{PendingPayment, Completed, false}, // ข้ามขั้น
		{Queued, Completed, false},         // ข้ามขั้น
		{Completed, Queued, false},         // จบแล้วย้อนไม่ได้
		{Expired, Queued, false},
		{Queued, Queued, false}, // สถานะเดิม
	}
	for _, c := range cases {
		if got := CanTransition(c.from, c.to); got != c.want {
			t.Errorf("%s -> %s: got %v, want %v", c.from, c.to, got, c.want)
		}
	}
}

func TestTerminalStates(t *testing.T) {
	for _, s := range []Status{Completed, Refunded} {
		if len(transitions[s]) != 0 {
			t.Errorf("%s should be terminal", s)
		}
	}
}
