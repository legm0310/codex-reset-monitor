// Codex 리셋 모니터의 상태 비교와 저장 동작을 검증하는 테스트.
package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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
	path := filepath.Join(t.TempDir(), ".env")
	if err := loadDotEnv(path); err != nil {
		t.Fatal(err)
	}
}

func TestNotificationsNewReset(t *testing.T) {
	previous := state{LastResetID: "1"}
	data := statusData{LatestReset: &reset{
		ID:          "2",
		ResetType:   "regular",
		AnnouncedAt: "2026-08-24T00:00:00Z",
		Source:      source{URL: "https://example.com/reset"},
	}}

	messages := notifications(previous, stateFrom(data), data)
	if len(messages) != 1 {
		t.Fatalf("알림 개수 = %d, 기대값 = 1", len(messages))
	}
	if !strings.Contains(messages[0], "발표 시각 (KST). 2026-08-24 09:00:00 KST") {
		t.Fatalf("KST 발표 시각이 없습니다: %s", messages[0])
	}
}

func TestNotificationsNewWatch(t *testing.T) {
	chance := 70
	data := statusData{ActiveWatch: &watch{
		Level:              "elevated",
		ResetChancePercent: &chance,
		ObservedAt:         "2026-08-24T00:00:00Z",
		ExpiresAt:          "2026-08-24T06:00:00Z",
		Source:             source{URL: "https://example.com/watch"},
	}}

	messages := notifications(state{}, stateFrom(data), data)
	if len(messages) != 1 {
		t.Fatalf("알림 개수 = %d, 기대값 = 1", len(messages))
	}
	if !strings.Contains(messages[0], "만료 시각 (KST). 2026-08-24 15:00:00 KST") {
		t.Fatalf("KST 만료 시각이 없습니다: %s", messages[0])
	}
}

func TestNotificationsWatchLevelRaised(t *testing.T) {
	previousChance := 70
	currentChance := 75
	previous := state{
		LastWatchObservedAt: "same",
		LastWatchLevel:      "elevated",
		LastWatchPercent:    &previousChance,
	}
	data := statusData{ActiveWatch: &watch{
		Level:              "strong",
		ResetChancePercent: &currentChance,
		ObservedAt:         "same",
	}}

	messages := notifications(previous, stateFrom(data), data)
	if len(messages) != 1 {
		t.Fatalf("알림 개수 = %d, 기대값 = 1", len(messages))
	}
}

func TestNotificationsProbabilityRaisedByTwenty(t *testing.T) {
	previousChance := 50
	currentChance := 70
	previous := state{
		LastWatchObservedAt: "same",
		LastWatchLevel:      "elevated",
		LastWatchPercent:    &previousChance,
	}
	data := statusData{ActiveWatch: &watch{
		Level:              "elevated",
		ResetChancePercent: &currentChance,
		ObservedAt:         "same",
	}}

	messages := notifications(previous, stateFrom(data), data)
	if len(messages) != 1 {
		t.Fatalf("알림 개수 = %d, 기대값 = 1", len(messages))
	}
}

func TestNotificationsIgnoreSmallProbabilityChange(t *testing.T) {
	previousChance := 50
	currentChance := 60
	previous := state{
		LastWatchObservedAt: "same",
		LastWatchLevel:      "elevated",
		LastWatchPercent:    &previousChance,
	}
	data := statusData{ActiveWatch: &watch{
		Level:              "elevated",
		ResetChancePercent: &currentChance,
		ObservedAt:         "same",
	}}

	messages := notifications(previous, stateFrom(data), data)
	if len(messages) != 0 {
		t.Fatalf("알림 개수 = %d, 기대값 = 0", len(messages))
	}
}

func TestSaveAndLoadState(t *testing.T) {
	chance := 80
	want := state{
		LastResetID:         "123",
		LastWatchObservedAt: "2026-08-24T00:00:00Z",
		LastWatchLevel:      "strong",
		LastWatchPercent:    &chance,
	}
	path := filepath.Join(t.TempDir(), "state.json")

	if err := saveState(path, want); err != nil {
		t.Fatal(err)
	}
	got, exists, err := loadState(path)
	if err != nil {
		t.Fatal(err)
	}
	if !exists {
		t.Fatal("저장한 상태 파일이 존재하지 않습니다")
	}
	if got.LastResetID != want.LastResetID ||
		got.LastWatchObservedAt != want.LastWatchObservedAt ||
		got.LastWatchLevel != want.LastWatchLevel ||
		got.LastWatchPercent == nil || *got.LastWatchPercent != chance {
		t.Fatalf("불일치 상태 = %#v", got)
	}
}

func TestFormatKSTCrossesDateBoundary(t *testing.T) {
	got := formatKST("2026-08-24T16:30:00.123Z")
	want := "2026-08-25 01:30:00 KST"
	if got != want {
		t.Fatalf("formatKST() = %q, 기대값 = %q", got, want)
	}
}

func TestFormatKSTInvalidTimestamp(t *testing.T) {
	if got := formatKST("invalid"); got != "변환 불가" {
		t.Fatalf("formatKST() = %q, 기대값 = 변환 불가", got)
	}
}
