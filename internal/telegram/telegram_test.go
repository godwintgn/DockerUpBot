package telegram

import (
	"context"
	"testing"
	"time"
)

func TestAllowedUsers(t *testing.T) {
	api := New("token", []int64{1, 2}, 2)
	if !api.Allowed(1) || !api.Allowed(2) || api.Allowed(3) {
		t.Fatal("allowlist mismatch")
	}
}

func TestAnswerEmptyCallbackIDNoPanic(t *testing.T) {
	api := New("token", []int64{1}, 2)
	api.Answer(context.Background(), "", "hi")
}

func TestPrivateAndConfiguredChatsOnly(t *testing.T) {
	api := New("token", []int64{7}, -100)
	if !api.allowedInChat(7, 7) || !api.allowedInChat(7, -100) || api.allowedInChat(7, -200) || api.allowedInChat(8, -100) || api.allowedInChat(7, 0) {
		t.Fatal("user or chat was incorrectly authorized")
	}
	if api.PollHealthy() {
		t.Fatal("ready before any successful poll")
	}
	api.lastPoll.Store(time.Now().Unix())
	if !api.PollHealthy() {
		t.Fatal("recent poll should be healthy")
	}
	api.lastPoll.Store(time.Now().Add(-2 * time.Minute).Unix())
	if api.PollHealthy() {
		t.Fatal("stale poll should be unhealthy")
	}
}
