package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

type API struct {
	token    string
	users    map[int64]bool
	chatID   int64
	lastPoll atomic.Int64
	client   *http.Client
}

func New(token string, users []int64, chatID int64) *API {
	m := make(map[int64]bool, len(users))
	for _, user := range users {
		m[user] = true
	}
	return &API{token: token, users: m, chatID: chatID, client: &http.Client{Timeout: 60 * time.Second}}
}

func (a *API) Allowed(id int64) bool { return a.users[id] }

func (a *API) allowedInChat(user, chat int64) bool {
	return a.Allowed(user) && chat != 0 && (chat == user || chat == a.chatID)
}

func (a *API) PollHealthy() bool {
	last := a.lastPoll.Load()
	return last > 0 && time.Since(time.Unix(last, 0)) < 90*time.Second
}

// redactErr drops the request URL so a network error cannot include the bot token.
func (a *API) redactErr(err error) error {
	if err == nil {
		return nil
	}
	var uerr *url.Error
	if errors.As(err, &uerr) {
		err = fmt.Errorf("telegram %s: %v", uerr.Op, uerr.Err)
	}
	if a.token != "" && strings.Contains(err.Error(), a.token) {
		return fmt.Errorf("%s", strings.ReplaceAll(err.Error(), a.token, "[redacted]"))
	}
	return err
}

// BotInfo is the result of Telegram getMe.
type BotInfo struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
	Name     string `json:"name"`
}

// GetMe verifies the bot token with Telegram and returns bot identity.
func (a *API) GetMe(ctx context.Context) (BotInfo, error) {
	out, err := a.call(ctx, "getMe", url.Values{})
	if err != nil {
		return BotInfo{}, err
	}
	r, ok := out["result"].(map[string]any)
	if !ok {
		return BotInfo{}, fmt.Errorf("telegram getMe: missing result")
	}
	info := BotInfo{}
	if id, ok := r["id"].(float64); ok {
		info.ID = int64(id)
	}
	if u, ok := r["username"].(string); ok {
		info.Username = u
	}
	if n, ok := r["first_name"].(string); ok {
		info.Name = n
	}
	if info.ID == 0 {
		return BotInfo{}, fmt.Errorf("telegram getMe: empty bot id")
	}
	return info, nil
}

type apiError struct {
	Description string
	RetryAfter  int
}

func (e *apiError) Error() string { return e.Description }

func (a *API) call(ctx context.Context, method string, values url.Values) (map[string]any, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	endpoint := "https://api.telegram.org/bot" + a.token + "/" + method
	var lastErr error
	for attempt := 0; attempt < 4; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(values.Encode()))
		if err != nil {
			return nil, a.redactErr(err)
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		resp, err := a.client.Do(req)
		if err != nil {
			lastErr = a.redactErr(err)
			if attempt < 3 {
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-time.After(time.Duration(attempt+1) * time.Second):
				}
			}
			continue
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		resp.Body.Close()
		var out map[string]any
		if err := json.Unmarshal(body, &out); err != nil {
			return nil, err
		}
		if ok, _ := out["ok"].(bool); ok {
			return out, nil
		}
		desc, _ := out["description"].(string)
		retry := 0
		if params, ok := out["parameters"].(map[string]any); ok {
			switch v := params["retry_after"].(type) {
			case float64:
				retry = int(v)
			case json.Number:
				n, _ := v.Int64()
				retry = int(n)
			}
		}
		if resp.StatusCode == 429 || retry > 0 {
			if retry < 1 {
				retry = 1
			}
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Duration(retry) * time.Second):
			}
			lastErr = &apiError{Description: desc, RetryAfter: retry}
			continue
		}
		return out, &apiError{Description: fmt.Sprintf("telegram %s failed: %s", method, desc)}
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, fmt.Errorf("telegram %s failed", method)
}

func (a *API) Send(ctx context.Context, chat int64, text string, buttons [][]Button) (int64, error) {
	v := url.Values{"chat_id": {strconv.FormatInt(chat, 10)}, "text": {text}, "parse_mode": {"HTML"}}
	if len(buttons) > 0 {
		b, _ := json.Marshal(map[string]any{"inline_keyboard": buttons})
		v.Set("reply_markup", string(b))
	}
	out, e := a.call(ctx, "sendMessage", v)
	if e != nil {
		return 0, e
	}
	r, ok := out["result"].(map[string]any)
	if !ok {
		return 0, fmt.Errorf("telegram sendMessage: missing result")
	}
	id, ok := r["message_id"].(float64)
	if !ok {
		return 0, fmt.Errorf("telegram sendMessage: missing message_id")
	}
	return int64(id), nil
}

func (a *API) Edit(ctx context.Context, chat int64, msg int64, text string, buttons [][]Button) error {
	v := url.Values{
		"chat_id":    {strconv.FormatInt(chat, 10)},
		"message_id": {strconv.FormatInt(msg, 10)},
		"text":       {text},
		"parse_mode": {"HTML"},
	}
	if buttons != nil {
		b, _ := json.Marshal(map[string]any{"inline_keyboard": buttons})
		v.Set("reply_markup", string(b))
	} else {
		v.Set("reply_markup", `{"inline_keyboard":[]}`)
	}
	_, e := a.call(ctx, "editMessageText", v)
	return e
}

// Delete removes a chat message (used so a newer update card replaces the old one).
func (a *API) Delete(ctx context.Context, chat, msg int64) error {
	if chat == 0 || msg == 0 {
		return nil
	}
	v := url.Values{
		"chat_id":    {strconv.FormatInt(chat, 10)},
		"message_id": {strconv.FormatInt(msg, 10)},
	}
	_, e := a.call(ctx, "deleteMessage", v)
	return e
}

// StripButtons removes inline keyboard from a message while keeping its text.
func (a *API) StripButtons(ctx context.Context, chat, msg int64) error {
	v := url.Values{
		"chat_id":      {strconv.FormatInt(chat, 10)},
		"message_id":   {strconv.FormatInt(msg, 10)},
		"reply_markup": {`{"inline_keyboard":[]}`},
	}
	_, e := a.call(ctx, "editMessageReplyMarkup", v)
	return e
}

func (a *API) Answer(ctx context.Context, id, text string) {
	if id == "" {
		return
	}
	v := url.Values{"callback_query_id": {id}}
	if text != "" {
		v.Set("text", text)
		v.Set("show_alert", "false")
	}
	_, _ = a.call(ctx, "answerCallbackQuery", v)
}

// SetMyCommands registers the bot command menu with Telegram.
func (a *API) SetMyCommands(ctx context.Context) error {
	cmds := []map[string]string{
		{"command": "updates", "description": "Show pending updates"},
		{"command": "status", "description": "Bot status"},
		{"command": "history", "description": "Recent update jobs"},
		{"command": "help", "description": "Help"},
	}
	b, _ := json.Marshal(cmds)
	_, err := a.call(ctx, "setMyCommands", url.Values{"commands": {string(b)}})
	return err
}

type Button struct {
	Text         string `json:"text"`
	CallbackData string `json:"callback_data,omitempty"`
}

type updateResponse struct {
	OK         bool `json:"ok"`
	Parameters struct {
		RetryAfter int `json:"retry_after"`
	} `json:"parameters"`
	Result []struct {
		UpdateID int64 `json:"update_id"`
		Message  *struct {
			Chat struct {
				ID int64 `json:"id"`
			} `json:"chat"`
			From struct {
				ID int64 `json:"id"`
			} `json:"from"`
			Text      string `json:"text"`
			MessageID int64  `json:"message_id"`
		} `json:"message"`
		Callback *struct {
			ID   string `json:"id"`
			From struct {
				ID int64 `json:"id"`
			} `json:"from"`
			Data    string `json:"data"`
			Message *struct {
				Chat struct {
					ID int64 `json:"id"`
				} `json:"chat"`
				MessageID int64 `json:"message_id"`
			} `json:"message"`
		} `json:"callback_query"`
	} `json:"result"`
}

type Handler interface {
	HandleTelegram(ctx context.Context, api *API, chatID int64, messageID int64, fromID int64, text, callback, callbackID string)
}

func (a *API) Run(ctx context.Context, h Handler) {
	offset := int64(0)
	slog.Info("telegram long-polling loop active")
	for ctx.Err() == nil {
		out, status, err := a.poll(ctx, offset)
		if err != nil || status != http.StatusOK || !out.OK {
			if ctx.Err() != nil {
				break
			}
			slog.Warn("telegram getUpdates failed", "status", status, "err", a.redactErr(err))
			delay := max(2, out.Parameters.RetryAfter)
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Duration(delay) * time.Second):
			}
			continue
		}
		a.lastPoll.Store(time.Now().Unix())
		for _, u := range out.Result {
			offset = u.UpdateID + 1
			if u.Message != nil {
				if !a.allowedInChat(u.Message.From.ID, u.Message.Chat.ID) {
					slog.Warn("telegram message ignored: user or chat not permitted", "user", u.Message.From.ID)
					continue
				}
				h.HandleTelegram(ctx, a, u.Message.Chat.ID, u.Message.MessageID, u.Message.From.ID, u.Message.Text, "", "")
			}
			if u.Callback != nil {
				chat, msg := int64(0), int64(0)
				if u.Callback.Message != nil {
					chat = u.Callback.Message.Chat.ID
					msg = u.Callback.Message.MessageID
				}
				if !a.allowedInChat(u.Callback.From.ID, chat) {
					slog.Warn("telegram callback ignored: user or chat not permitted", "user", u.Callback.From.ID)
					a.Answer(ctx, u.Callback.ID, "Unauthorized")
					continue
				}
				h.HandleTelegram(ctx, a, chat, msg, u.Callback.From.ID, "", u.Callback.Data, u.Callback.ID)
			}
		}
	}
	slog.Info("telegram long-polling stopped")
}

func (a *API) poll(ctx context.Context, offset int64) (updateResponse, int, error) {
	v := url.Values{"timeout": {"30"}, "offset": {strconv.FormatInt(offset, 10)}, "allowed_updates": {"[\"message\",\"callback_query\"]"}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.telegram.org/bot"+a.token+"/getUpdates", bytes.NewBufferString(v.Encode()))
	if err != nil {
		return updateResponse{}, 0, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := a.client.Do(req)
	if err != nil {
		return updateResponse{}, 0, err
	}
	defer resp.Body.Close()
	var out updateResponse
	err = json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&out)
	return out, resp.StatusCode, err
}
