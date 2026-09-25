package telegram_test

import (
	"context"
	"testing"

	"github.com/dockerupbot/dockerupbot/internal/telegram"
)

func TestAllowedUsers(t *testing.T) {
	api := telegram.New("token", []int64{1, 2}, 2)
	if !api.Allowed(1) || !api.Allowed(2) || api.Allowed(3) {
		t.Fatal("allowlist mismatch")
	}
}

func TestAnswerEmptyCallbackIDNoPanic(t *testing.T) {
	api := telegram.New("token", []int64{1}, 2)
	api.Answer(context.Background(), "", "hi")
}
