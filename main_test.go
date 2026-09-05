// Codex 리셋 모니터의 상태 전환과 알림 동작을 검증하는 테스트.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type telegramRequest struct {
	Text            string `json:"text"`
	ReplyParameters *struct {
		MessageID int `json:"message_id"`
	} `json:"reply_parameters"`
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

func TestLoadDotEnv(t *testing.T) {
	t.Setenv("EXISTING_VALUE", "system")
	path := filepath.Join(t.TempDir(), ".env")
	content := "# Telegram 설정\nTEST_DOTENV_TOKEN='token'\nTEST_DOTENV_CHAT_ID=12345\nEXISTING_VALUE=file\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	os.Unsetenv("TEST_DOTENV_TOKEN")
	os.Unsetenv("TEST_DOTENV_CHAT_ID")
	t.Cleanup(func() {
		os.Unsetenv("TEST_DOTENV_TOKEN")
		os.Unsetenv("TEST_DOTENV_CHAT_ID")
	})

	if err := loadDotEnv(path); err != nil {
		t.Fatal(err)
	}
	if got := os.Getenv("TEST_DOTENV_TOKEN"); got != "token" {
		t.Fatalf("TEST_DOTENV_TOKEN = %q, 기대값 = token", got)
	}
	if got := os.Getenv("TEST_DOTENV_CHAT_ID"); got != "12345" {
		t.Fatalf("TEST_DOTENV_CHAT_ID = %q, 기대값 = 12345", got)
	}
	if got := os.Getenv("EXISTING_VALUE"); got != "system" {
		t.Fatalf("EXISTING_VALUE = %q, 기대값 = system", got)
	}
}

func TestLoadDotEnvMissingFile(t *testing.T) {
	if err := loadDotEnv(filepath.Join(t.TempDir(), ".env")); err != nil {
		t.Fatal(err)
	}
}

func TestProcessResetsWaitsAndRepliesToOriginalMessage(t *testing.T) {
	requests := make([]telegramRequest, 0, 4)
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		var request telegramRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			return nil, err
		}
		requests = append(requests, request)
		body := `{"ok":true,"result":{"message_id":` + string(rune('0'+len(requests))) + `}}`
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Body:       io.NopCloser(strings.NewReader(body)),
			Header:     make(http.Header),
		}, nil
	})}

	statePath := filepath.Join(t.TempDir(), "state.json")
	cfg := config{
		botToken:        "test-token",
		chatID:          "test-chat",
		telegramBaseURL: "https://telegram.test",
		statePath:       statePath,
	}
	current := state{Resets: make(map[string]resetState)}
	now := time.Date(2026, 8, 26, 0, 0, 0, 0, time.UTC)
	first := reset{
		ID:          "reset-1",
		ResetType:   "regular",
		AnnouncedAt: now.Format(time.RFC3339),
		Source:      source{Type: "x_post", URL: "https://example.com/reset-1"},
	}
	observed := reset{
		ID:          "observed-2",
		ResetType:   "regular",
		AnnouncedAt: now.Add(time.Minute).Format(time.RFC3339),
		Source:      source{Type: "observed"},
	}
	third := reset{
		ID:          "reset-3",
		ResetType:   "regular",
		AnnouncedAt: now.Add(4 * time.Minute).Format(time.RFC3339),
		Source:      source{Type: "x_post", URL: "https://example.com/reset-3"},
	}

	if err := processResets(context.Background(), client, cfg, &current, []reset{first}, now); err != nil {
		t.Fatal(err)
	}
	if len(requests) != 1 || !strings.Contains(requests[0].Text, "reset-1") {
		t.Fatalf("첫 기본 리셋 알림이 올바르지 않습니다: %#v", requests)
	}

	if err := processResets(context.Background(), client, cfg, &current, []reset{observed, first}, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if len(requests) != 1 {
		t.Fatalf("2분 전 observed 리셋이 전송됐습니다: %#v", requests)
	}
	if err := processResets(context.Background(), client, cfg, &current, []reset{observed, first}, now.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if len(requests) != 2 || !strings.Contains(requests[1].Text, "원문 링크. 없음") {
		t.Fatalf("2분 후 링크 없는 알림이 올바르지 않습니다: %#v", requests)
	}
	observedMessageID := current.Resets[observed.ID].TelegramMessageID

	if err := processResets(context.Background(), client, cfg, &current, []reset{third, observed, first}, now.Add(4*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if len(requests) != 3 || !strings.Contains(requests[2].Text, "reset-3") {
		t.Fatalf("후속 기본 리셋 알림이 올바르지 않습니다: %#v", requests)
	}

	observed.Source.URL = "https://example.com/observed-2"
	if err := processResets(context.Background(), client, cfg, &current, []reset{third, observed, first}, now.Add(5*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if len(requests) != 4 || !strings.Contains(requests[3].Text, "리셋 관련 링크가 추가되었습니다") {
		t.Fatalf("링크 추가 알림이 올바르지 않습니다: %#v", requests)
	}
	if requests[3].ReplyParameters == nil || requests[3].ReplyParameters.MessageID != observedMessageID {
		t.Fatalf("observed 리셋 원문에 답장하지 않았습니다: %#v", requests[3])
	}
	if repeated := processResets(context.Background(), client, cfg, &current, []reset{third, observed, first}, now.Add(6*time.Minute)); repeated != nil {
		t.Fatal(repeated)
	}
	if len(requests) != 4 {
		t.Fatalf("링크 추가 알림이 반복됐습니다: %#v", requests)
	}
}

func TestProcessResetsSendsLinkedResetDuringWait(t *testing.T) {
	requests, client := newTelegramTestClient(t)
	cfg := testConfig(filepath.Join(t.TempDir(), "state.json"))
	current := state{Resets: make(map[string]resetState)}
	now := time.Date(2026, 8, 26, 0, 0, 0, 0, time.UTC)
	value := reset{ID: "observed", ResetType: "regular", AnnouncedAt: now.Format(time.RFC3339), Source: source{Type: "observed"}}

	if err := processResets(context.Background(), client, cfg, &current, []reset{value}, now); err != nil {
		t.Fatal(err)
	}
	value.Source.URL = "https://example.com/observed"
	if err := processResets(context.Background(), client, cfg, &current, []reset{value}, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if len(*requests) != 1 || !strings.Contains((*requests)[0].Text, "관련 링크. https://example.com/observed") {
		t.Fatalf("대기 중 추가된 링크를 포함하지 않았습니다: %#v", *requests)
	}
}

func TestResetTimeCorrectionDoesNotNotify(t *testing.T) {
	previous := resetState{
		ResetType: "regular",
		URL:       "https://example.com/reset",
		Notified:  true,
	}
	current := reset{
		ID:          "same",
		ResetType:   "regular",
		AnnouncedAt: "2026-08-25T14:30:00Z",
		Source:      source{Type: "observed", URL: "https://example.com/reset"},
	}
	if got := resetUpdateNotification(previous, current); got != "" {
		t.Fatalf("시각만 수정됐는데 알림이 생성됐습니다: %s", got)
	}
}

func TestPruneResetStatesKeepsLatestFiftyAndPending(t *testing.T) {
	now := time.Date(2026, 8, 26, 0, 0, 0, 0, time.UTC)
	value := state{Resets: make(map[string]resetState)}
	for index := 0; index < 55; index++ {
		id := time.Duration(index).String()
		value.Resets[id] = resetState{
			AnnouncedAt: now.Add(time.Duration(index) * time.Minute).Format(time.RFC3339),
			FirstSeenAt: now,
			Notified:    true,
		}
	}
	value.Resets["pending"] = resetState{FirstSeenAt: now, Notified: false}

	pruneResetStates(&value, 50)
	if len(value.Resets) != 51 {
		t.Fatalf("정리 후 상태 개수 = %d, 기대값 = 51", len(value.Resets))
	}
	if _, exists := value.Resets["pending"]; !exists {
		t.Fatal("pending 상태가 삭제됐습니다")
	}
	if _, exists := value.Resets[(time.Duration(54)).String()]; !exists {
		t.Fatal("가장 최근 상태가 삭제됐습니다")
	}
}

func TestProcessResetsSilentlyReconcilesOldResetAtRetentionBoundary(t *testing.T) {
	requests, client := newTelegramTestClient(t)
	cfg := testConfig(filepath.Join(t.TempDir(), "state.json"))
	base := time.Date(2025, 11, 5, 20, 19, 29, 0, time.UTC)
	apiResets := make([]reset, 0, resetStateLimit)
	current := state{Resets: make(map[string]resetState)}

	for index := 0; index < resetStateLimit; index++ {
		announcedAt := base.Add(time.Duration(index) * 24 * time.Hour)
		value := reset{
			ID:          fmt.Sprintf("api-%02d", index),
			ResetType:   "regular",
			AnnouncedAt: announcedAt.Format(time.RFC3339),
			Source:      source{Type: "x_post", URL: fmt.Sprintf("https://example.com/api-%02d", index)},
		}
		apiResets = append(apiResets, value)
		if index > 0 {
			item := resetStateFrom(value, base)
			item.Notified = true
			current.Resets[value.ID] = item
		}
	}
	current.Resets["removed-upstream"] = resetState{
		ResetType:   "banked",
		AnnouncedAt: base.Add(48*time.Hour + time.Hour).Format(time.RFC3339),
		SourceType:  "x_post",
		URL:         "https://example.com/removed-upstream",
		FirstSeenAt: base,
		Notified:    true,
	}
	current.LastResetWatermarkAt = apiResets[len(apiResets)-1].AnnouncedAt

	if err := processResets(context.Background(), client, cfg, &current, apiResets, base.Add(60*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	pruneResetStates(&current, resetStateLimit)
	if len(*requests) != 0 {
		t.Fatalf("복원된 과거 리셋이 알림으로 전송됐습니다: %#v", *requests)
	}
	if _, exists := current.Resets["removed-upstream"]; exists {
		t.Fatal("API에서 삭제된 완료 상태가 남아 있습니다")
	}
	if restored, exists := current.Resets["api-00"]; !exists || !restored.Notified {
		t.Fatalf("과거 리셋이 기준 상태로 등록되지 않았습니다: %#v", restored)
	}

	if err := processResets(context.Background(), client, cfg, &current, apiResets, base.Add(60*24*time.Hour+time.Minute)); err != nil {
		t.Fatal(err)
	}
	if len(*requests) != 0 {
		t.Fatalf("다음 조회에서 과거 리셋 알림이 반복됐습니다: %#v", *requests)
	}

	newReset := reset{
		ID:          "new-reset",
		ResetType:   "regular",
		AnnouncedAt: base.Add(60 * 24 * time.Hour).Format(time.RFC3339),
		Source:      source{Type: "x_post", URL: "https://example.com/new-reset"},
	}
	apiResets = append(apiResets[1:], newReset)
	if err := processResets(context.Background(), client, cfg, &current, apiResets, base.Add(61*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if len(*requests) != 1 || !strings.Contains((*requests)[0].Text, "new-reset") {
		t.Fatalf("실제 신규 리셋 알림이 올바르지 않습니다: %#v", *requests)
	}
}

func TestMigrateLegacyState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	legacy := `{"last_reset_id":"123","last_reset_type":"regular","last_reset_url":"","last_watch_level":"strong","last_watch_percent":80}`
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	got, exists, err := loadState(path)
	if err != nil || !exists {
		t.Fatalf("기존 상태 읽기 실패: %v", err)
	}
	apiResets := []reset{{ID: "456", ResetType: "regular", AnnouncedAt: "2026-08-26T00:00:00Z", Source: source{Type: "x_post", URL: "https://example.com/456"}}}
	migrateLegacyState(&got, apiResets, time.Now().UTC())
	migrated, exists := got.Resets["123"]
	if !exists || !migrated.Notified || migrated.ResetType != "regular" {
		t.Fatalf("기존 상태 마이그레이션 실패: %#v", got)
	}
	if got.LegacyLastResetID != "" {
		t.Fatalf("기존 필드가 제거되지 않았습니다: %#v", got)
	}
	if baseline, exists := got.Resets["456"]; !exists || !baseline.Notified {
		t.Fatalf("기존 이력이 기준 상태로 채워지지 않았습니다: %#v", got)
	}
	if got.LastResetWatermarkAt != "2026-08-26T00:00:00Z" {
		t.Fatalf("watermark 자동 마이그레이션 실패: %q", got.LastResetWatermarkAt)
	}
}

func TestSaveAndLoadState(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	want := state{LastResetWatermarkAt: now.Format(time.RFC3339), Resets: map[string]resetState{
		"123": {
			ResetType:         "regular",
			AnnouncedAt:       now.Format(time.RFC3339),
			SourceType:        "observed",
			URL:               "https://example.com/reset",
			FirstSeenAt:       now,
			Notified:          true,
			TelegramMessageID: 100,
		},
	}}
	path := filepath.Join(t.TempDir(), "state.json")
	if err := saveState(path, want); err != nil {
		t.Fatal(err)
	}
	got, exists, err := loadState(path)
	if err != nil || !exists {
		t.Fatalf("상태 읽기 실패: %v", err)
	}
	if got.Resets["123"].TelegramMessageID != 100 || got.Resets["123"].URL != "https://example.com/reset" || got.LastResetWatermarkAt != want.LastResetWatermarkAt {
		t.Fatalf("불일치 상태: %#v", got)
	}
}

func TestProcessLockRejectsSecondInstance(t *testing.T) {
	path := filepath.Join(t.TempDir(), "monitor.lock")
	first, err := acquireProcessLock(path)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseProcessLock(first)
	if second, err := acquireProcessLock(path); err == nil {
		releaseProcessLock(second)
		t.Fatal("두 번째 실행 잠금이 허용됐습니다")
	}
}

func TestFormatKSTCrossesDateBoundary(t *testing.T) {
	got := formatKST("2026-08-24T16:30:00.123Z")
	want := "2026-08-25 01:30:00 KST"
	if got != want {
		t.Fatalf("formatKST() = %q, 기대값 = %q", got, want)
	}
}

func TestTelegramNotificationFlow(t *testing.T) {
	if os.Getenv("TELEGRAM_INTEGRATION_TEST") != "1" {
		t.Skip("TELEGRAM_INTEGRATION_TEST=1일 때 실제 Telegram 알림을 전송합니다")
	}
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Timeout: 15 * time.Second}
	now := time.Now().UTC()

	first := reset{
		ID:          "test-basic-1",
		ResetType:   "regular",
		AnnouncedAt: now.Format(time.RFC3339),
		Source:      source{Type: "x_post", URL: "https://example.com/codex-reset-test-1"},
	}
	if _, err := sendTelegram(context.Background(), client, cfg, "🧪 테스트 알림 1/4\n"+resetNotification(first), 0); err != nil {
		t.Fatal(err)
	}

	observed := reset{
		ID:          "test-observed-2",
		ResetType:   "regular",
		AnnouncedAt: now.Add(time.Minute).Format(time.RFC3339),
		Source:      source{Type: "observed"},
	}
	observedMessageID, err := sendTelegram(context.Background(), client, cfg, "🧪 테스트 알림 2/4\n"+resetNotification(observed), 0)
	if err != nil {
		t.Fatal(err)
	}

	third := reset{
		ID:          "test-basic-3",
		ResetType:   "regular",
		AnnouncedAt: now.Add(2 * time.Minute).Format(time.RFC3339),
		Source:      source{Type: "x_post", URL: "https://example.com/codex-reset-test-3"},
	}
	if _, err := sendTelegram(context.Background(), client, cfg, "🧪 테스트 알림 3/4\n"+resetNotification(third), 0); err != nil {
		t.Fatal(err)
	}

	linkUpdate := "🧪 테스트 알림 4/4\n🔗 Codex 리셋 관련 링크가 추가되었습니다\nhttps://example.com/codex-reset-test-2"
	if _, err := sendTelegram(context.Background(), client, cfg, linkUpdate, observedMessageID); err != nil {
		t.Fatal(err)
	}
}

func newTelegramTestClient(t *testing.T) (*[]telegramRequest, *http.Client) {
	t.Helper()
	requests := make([]telegramRequest, 0)
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		var request telegramRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			return nil, err
		}
		requests = append(requests, request)
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Body:       io.NopCloser(strings.NewReader(`{"ok":true,"result":{"message_id":1}}`)),
			Header:     make(http.Header),
		}, nil
	})}
	return &requests, client
}

func testConfig(statePath string) config {
	return config{
		botToken:        "test-token",
		chatID:          "test-chat",
		telegramBaseURL: "https://telegram.test",
		statePath:       statePath,
	}
}
