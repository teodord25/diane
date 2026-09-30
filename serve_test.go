package main

import (
	"net/http/httptest"
	"testing"
)

func TestIsLoopback(t *testing.T) {
	for host, want := range map[string]bool{
		"127.0.0.1:7777":    true,
		"localhost:7777":    true,
		"[::1]:7777":        true,
		"127.0.0.1":         true,
		"evil.example:7777": false,
		"100.64.0.3:7777":   false,
		"":                  false,
	} {
		if got := isLoopback(host); got != want {
			t.Errorf("isLoopback(%q) = %v, want %v", host, got, want)
		}
	}
}

func TestOpenModeGuards(t *testing.T) {
	s := &server{} // token "" = DIANE_OPEN
	cases := []struct {
		name, method, host, origin string
		want                       bool
	}{
		{"new tab page", "GET", "127.0.0.1:7777", "", true},
		{"same-origin post", "POST", "127.0.0.1:7777", "http://127.0.0.1:7777", true},
		{"curl post", "POST", "127.0.0.1:7777", "", true},
		{"cross-site post", "POST", "127.0.0.1:7777", "https://evil.example", false},
		{"dns rebinding", "POST", "evil.example:7777", "http://evil.example:7777", false},
		{"open mode off loopback", "GET", "100.64.0.3:7777", "", false},
	}
	for _, c := range cases {
		r := httptest.NewRequest(c.method, "http://"+c.host+"/drop", nil)
		r.Host = c.host
		if c.origin != "" {
			r.Header.Set("Origin", c.origin)
		}
		if got := s.authed(httptest.NewRecorder(), r); got != c.want {
			t.Errorf("%s: authed = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestTokenModeRejectsCrossSite(t *testing.T) {
	s := &server{token: "secret"}
	r := httptest.NewRequest("POST", "http://box:7777/drop?token=secret", nil)
	r.Header.Set("Origin", "https://evil.example")
	if s.authed(httptest.NewRecorder(), r) {
		t.Error("cross-site POST accepted in token mode")
	}
}
