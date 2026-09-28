package main

import (
	"net/http/httptest"
	"testing"
)

func TestTracedSkipsCapabilityLinks(t *testing.T) {
	cases := map[string]bool{
		"/universal/review/tok": false,
		"/universal/edit/tok":   false,
		"/universal/list/tok":   false,
		"/health":               false,
		"/ready":                false,
		"/universal":            true,
		"/universal/links":      true,
		"/grafana":              true,
	}
	for path, want := range cases {
		if got := traced(httptest.NewRequest("GET", path, nil)); got != want {
			t.Errorf("traced(%s) = %v, want %v", path, got, want)
		}
	}
}
