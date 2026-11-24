package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestReadiness(t *testing.T) {
	for _, tc := range []struct {
		name         string
		telegram     bool
		dockerErr    error
		shuttingDown bool
		want         int
	}{
		{name: "ready", telegram: true, want: http.StatusOK},
		{name: "Telegram unavailable", want: http.StatusServiceUnavailable},
		{name: "Docker unavailable", telegram: true, dockerErr: errors.New("unavailable"), want: http.StatusServiceUnavailable},
		{name: "shutting down", telegram: true, shuttingDown: true, want: http.StatusServiceUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.shuttingDown {
				cancel()
			}
			handler := readiness(ctx, func() bool { return tc.telegram }, func(ctx context.Context) error {
				if _, ok := ctx.Deadline(); !ok {
					t.Error("Docker readiness probe must have a deadline")
				}
				return tc.dockerErr
			})
			resp := httptest.NewRecorder()
			handler(resp, httptest.NewRequest(http.MethodGet, "/readyz", nil))
			if resp.Code != tc.want {
				t.Fatalf("status %d, want %d", resp.Code, tc.want)
			}
		})
	}
}
