// Codex 리셋 상태를 감시하고 텔레그램 알림을 전송하는 실행 프로그램.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"
)

const (
	statusURL          = "https://codex-resets.com/api/v1/status"
	resetsURL          = "https://codex-resets.com/api/v1/resets?limit=50&order=desc"
	stateFile          = "state.json"
	lockFile           = ".codex-reset-monitor.lock"
	pollInterval       = time.Minute
	probabilityRiseMin = 20
	resetLinkWait      = 2 * time.Minute
	resetStateLimit    = 50
)

var kstLocation = time.FixedZone("KST", 9*60*60)

type config struct {
	botToken        string
	chatID          string
	telegramBaseURL string
	statePath       string
}

type statusResponse struct {
	Data statusData `json:"data"`
}

type resetListResponse struct {
	Data []reset `json:"data"`
}

type statusData struct {
	LatestReset *reset `json:"latest_reset"`
	ActiveWatch *watch `json:"active_watch"`
}

type reset struct {
	ID          string `json:"id"`
	ResetType   string `json:"reset_type"`
	AnnouncedAt string `json:"announced_at"`
	Source      source `json:"source"`
}

type watch struct {
	Level              string `json:"level"`
	ResetChancePercent *int   `json:"reset_chance_percent"`
	ForecastWindow     string `json:"forecast_window"`
	ObservedAt         string `json:"observed_at"`
	ExpiresAt          string `json:"expires_at"`
	Source             source `json:"source"`
}

type source struct {
	Type string `json:"type"`
	URL  string `json:"url"`
}

type state struct {
	Resets               map[string]resetState `json:"resets"`
	LastResetWatermarkAt string                `json:"last_reset_watermark_at"`
	LastWatchObservedAt  string                `json:"last_watch_observed_at"`
	LastWatchLevel       string                `json:"last_watch_level"`
	LastWatchPercent     *int                  `json:"last_watch_percent"`
	LegacyLastResetID    string                `json:"last_reset_id,omitempty"`
	LegacyLastResetType  string                `json:"last_reset_type,omitempty"`
	LegacyAnnouncedAt    string                `json:"last_reset_announced_at,omitempty"`
	LegacyResetSource    string                `json:"last_reset_source_type,omitempty"`
	LegacyLastResetURL   string                `json:"last_reset_url,omitempty"`
}

type resetState struct {
	ResetType         string    `json:"reset_type"`
	AnnouncedAt       string    `json:"announced_at"`
	SourceType        string    `json:"source_type"`
	URL               string    `json:"url"`
	FirstSeenAt       time.Time `json:"first_seen_at"`
	Notified          bool      `json:"notified"`
	TelegramMessageID int       `json:"telegram_message_id"`
}

func main() {
	lock, err := acquireProcessLock(lockFile)
	if err != nil {
		log.Fatal(err)
	}
	defer releaseProcessLock(lock)

	cfg, err := loadConfig()
	if err != nil {
		log.Fatal(err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	client := &http.Client{Timeout: 15 * time.Second}
	if err := monitor(ctx, client, cfg); err != nil && !errors.Is(err, context.Canceled) {
		log.Fatal(err)
	}
}

func loadConfig() (config, error) {
	if err := loadDotEnv(".env"); err != nil {
		return config{}, err
	}

	cfg := config{
		botToken:        os.Getenv("TELEGRAM_BOT_TOKEN"),
		chatID:          os.Getenv("TELEGRAM_CHAT_ID"),
		telegramBaseURL: "https://api.telegram.org",
		statePath:       stateFile,
	}

	if cfg.botToken == "" || cfg.chatID == "" {
		return config{}, errors.New("TELEGRAM_BOT_TOKEN과 TELEGRAM_CHAT_ID 환경변수가 필요합니다")
	}
	return cfg, nil
}

func loadDotEnv(path string) error {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf(".env 파일 열기: %w", err)
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	lineNumber := 0
	for scanner.Scan() {
		lineNumber++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")

		key, value, found := strings.Cut(line, "=")
		key = strings.TrimSpace(key)
		if !found || key == "" {
			return fmt.Errorf(".env %d번째 줄 형식이 올바르지 않습니다", lineNumber)
		}

		value = strings.TrimSpace(value)
		if len(value) >= 2 && ((value[0] == '\'' && value[len(value)-1] == '\'') ||
			(value[0] == '"' && value[len(value)-1] == '"')) {
			value = value[1 : len(value)-1]
		}

		if _, exists := os.LookupEnv(key); exists {
			continue
		}
		if err := os.Setenv(key, value); err != nil {
			return fmt.Errorf(".env %s 설정: %w", key, err)
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf(".env 파일 읽기: %w", err)
	}
	return nil
}

func monitor(ctx context.Context, client *http.Client, cfg config) error {
	if err := checkOnce(ctx, client, cfg); err != nil {
		log.Printf("초기 조회 실패: %v", err)
	}

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := checkOnce(ctx, client, cfg); err != nil {
				log.Printf("상태 조회 실패: %v", err)
			}
		}
	}
}

func checkOnce(ctx context.Context, client *http.Client, cfg config) error {
	data, err := fetchStatus(ctx, client)
	if err != nil {
		return err
	}
	resets, err := fetchResets(ctx, client)
	if err != nil {
		return err
	}

	current, exists, err := loadState(cfg.statePath)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	if !exists {
		current = baselineState(data, resets, now)
		if err := saveState(cfg.statePath, current); err != nil {
			return err
		}
		log.Print("첫 실행 기준 상태를 저장했습니다")
		return nil
	}
	migrateLegacyState(&current, resets, now)

	if err := processResets(ctx, client, cfg, &current, resets, now); err != nil {
		return err
	}
	if err := processWatch(ctx, client, cfg, &current, data); err != nil {
		return err
	}
	pruneResetStates(&current, resetStateLimit)
	if err := saveState(cfg.statePath, current); err != nil {
		return err
	}
	return nil
}

func fetchStatus(ctx context.Context, client *http.Client) (statusData, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, statusURL, nil)
	if err != nil {
		return statusData{}, err
	}
	req.Header.Set("User-Agent", "codex-reset-monitor/1.0")

	resp, err := client.Do(req)
	if err != nil {
		return statusData{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return statusData{}, fmt.Errorf("상태 API 응답 %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}

	var result statusResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return statusData{}, fmt.Errorf("상태 API JSON 해석: %w", err)
	}
	return result.Data, nil
}

func fetchResets(ctx context.Context, client *http.Client) ([]reset, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, resetsURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "codex-reset-monitor/1.0")

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("리셋 목록 API 응답 %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}

	var result resetListResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("리셋 목록 API JSON 해석: %w", err)
	}
	return result.Data, nil
}

func baselineState(data statusData, resets []reset, now time.Time) state {
	result := state{Resets: make(map[string]resetState)}
	for _, value := range resets {
		item := resetStateFrom(value, now)
		item.Notified = true
		result.Resets[value.ID] = item
		advanceResetWatermark(&result, value.AnnouncedAt)
	}
	if data.ActiveWatch != nil {
		result.LastWatchObservedAt = data.ActiveWatch.ObservedAt
		result.LastWatchLevel = data.ActiveWatch.Level
		result.LastWatchPercent = data.ActiveWatch.ResetChancePercent
	}
	return result
}

func migrateLegacyState(value *state, resets []reset, now time.Time) {
	if value.Resets == nil {
		value.Resets = make(map[string]resetState)
	}
	if value.LegacyLastResetID != "" {
		legacyID := value.LegacyLastResetID
		value.Resets[value.LegacyLastResetID] = resetState{
			ResetType:   value.LegacyLastResetType,
			AnnouncedAt: value.LegacyAnnouncedAt,
			SourceType:  value.LegacyResetSource,
			URL:         value.LegacyLastResetURL,
			FirstSeenAt: now,
			Notified:    true,
		}
		for _, current := range resets {
			if current.ID == legacyID {
				continue
			}
			item := resetStateFrom(current, now)
			item.Notified = true
			value.Resets[current.ID] = item
		}
	}
	value.LegacyLastResetID = ""
	value.LegacyLastResetType = ""
	value.LegacyAnnouncedAt = ""
	value.LegacyResetSource = ""
	value.LegacyLastResetURL = ""
	initializeResetWatermark(value)
}

func resetStateFrom(value reset, firstSeenAt time.Time) resetState {
	return resetState{
		ResetType:   value.ResetType,
		AnnouncedAt: value.AnnouncedAt,
		SourceType:  value.Source.Type,
		URL:         value.Source.URL,
		FirstSeenAt: firstSeenAt,
	}
}

func resetFromState(id string, value resetState) reset {
	return reset{
		ID:          id,
		ResetType:   value.ResetType,
		AnnouncedAt: value.AnnouncedAt,
		Source: source{
			Type: value.SourceType,
			URL:  value.URL,
		},
	}
}

func processResets(ctx context.Context, client *http.Client, cfg config, current *state, resets []reset, now time.Time) error {
	if current.Resets == nil {
		current.Resets = make(map[string]resetState)
	}
	initializeResetWatermark(current)
	reconcileCompletedResetStates(current, resets, resetStateLimit)
	watermark, hasWatermark := parseTimestamp(current.LastResetWatermarkAt)

	seen := make(map[string]bool, len(resets))
	for index := len(resets) - 1; index >= 0; index-- {
		value := resets[index]
		seen[value.ID] = true
		saved, exists := current.Resets[value.ID]
		if !exists {
			saved = resetStateFrom(value, now)
			announcedAt, validTimestamp := parseTimestamp(value.AnnouncedAt)
			if hasWatermark && (!validTimestamp || !announcedAt.After(watermark)) {
				saved.Notified = true
			}
			current.Resets[value.ID] = saved
		}
		notificationSent := false

		if !saved.Notified {
			if value.Source.URL != "" || now.Sub(saved.FirstSeenAt) >= resetLinkWait {
				messageID, err := sendTelegram(ctx, client, cfg, resetNotification(value), 0)
				if err != nil {
					return fmt.Errorf("리셋 알림 전송: %w", err)
				}
				saved.Notified = true
				saved.TelegramMessageID = messageID
				notificationSent = true
			}
		} else {
			message := resetUpdateNotification(saved, value)
			if message != "" {
				if _, err := sendTelegram(ctx, client, cfg, message, saved.TelegramMessageID); err != nil {
					return fmt.Errorf("리셋 업데이트 알림 전송: %w", err)
				}
				notificationSent = true
			}
		}

		saved.ResetType = value.ResetType
		saved.AnnouncedAt = value.AnnouncedAt
		saved.SourceType = value.Source.Type
		saved.URL = value.Source.URL
		current.Resets[value.ID] = saved
		advanceResetWatermark(current, value.AnnouncedAt)
		if notificationSent {
			if err := saveState(cfg.statePath, *current); err != nil {
				return err
			}
		}
	}

	for id, saved := range current.Resets {
		if seen[id] || saved.Notified || now.Sub(saved.FirstSeenAt) < resetLinkWait {
			continue
		}
		messageID, err := sendTelegram(ctx, client, cfg, resetNotification(resetFromState(id, saved)), 0)
		if err != nil {
			return fmt.Errorf("대기 중 리셋 알림 전송: %w", err)
		}
		saved.Notified = true
		saved.TelegramMessageID = messageID
		current.Resets[id] = saved
		if err := saveState(cfg.statePath, *current); err != nil {
			return err
		}
	}
	return nil
}

func reconcileCompletedResetStates(current *state, resets []reset, expectedCount int) {
	if len(resets) != expectedCount {
		return
	}

	seen := make(map[string]struct{}, len(resets))
	for _, value := range resets {
		seen[value.ID] = struct{}{}
	}
	if len(seen) != expectedCount {
		return
	}

	for id, saved := range current.Resets {
		if _, exists := seen[id]; !exists && saved.Notified {
			delete(current.Resets, id)
		}
	}
}

func initializeResetWatermark(current *state) {
	if current.LastResetWatermarkAt != "" {
		return
	}
	for _, saved := range current.Resets {
		advanceResetWatermark(current, saved.AnnouncedAt)
	}
}

func advanceResetWatermark(current *state, announcedAt string) {
	candidate, validCandidate := parseTimestamp(announcedAt)
	if !validCandidate {
		return
	}
	watermark, validWatermark := parseTimestamp(current.LastResetWatermarkAt)
	if !validWatermark || candidate.After(watermark) {
		current.LastResetWatermarkAt = announcedAt
	}
}

func parseTimestamp(value string) (time.Time, bool) {
	timestamp, err := time.Parse(time.RFC3339Nano, value)
	return timestamp, err == nil
}

func resetUpdateNotification(previous resetState, current reset) string {
	linkAdded := previous.URL == "" && current.Source.URL != ""
	typeChanged := previous.ResetType != "" && previous.ResetType != current.ResetType
	if !linkAdded && !typeChanged {
		return ""
	}

	if linkAdded && !typeChanged {
		message := "🔗 Codex 리셋 관련 링크가 추가되었습니다"
		if previous.TelegramMessageID == 0 {
			message += fmt.Sprintf("\n대상. %s에 감지된 %s 리셋", formatKST(current.AnnouncedAt), current.ResetType)
		}
		return message + "\n" + current.Source.URL
	}

	message := fmt.Sprintf(
		"ℹ️ Codex 리셋 정보가 업데이트되었습니다\n이전 종류. %s\n현재 종류. %s",
		previous.ResetType,
		current.ResetType,
	)
	if linkAdded {
		message += "\n관련 링크. " + current.Source.URL
	} else if current.Source.URL != "" {
		message += "\n" + current.Source.URL
	}
	return message
}

func processWatch(ctx context.Context, client *http.Client, cfg config, current *state, data statusData) error {
	previousObservedAt := current.LastWatchObservedAt
	previousLevel := current.LastWatchLevel
	previousPercent := current.LastWatchPercent

	if data.ActiveWatch == nil {
		current.LastWatchObservedAt = ""
		current.LastWatchLevel = ""
		current.LastWatchPercent = nil
		return nil
	}

	newWatch := data.ActiveWatch.ObservedAt != previousObservedAt
	levelRaised := previousLevel == "elevated" && data.ActiveWatch.Level == "strong"
	probabilityRaised := percentRise(previousPercent, data.ActiveWatch.ResetChancePercent) >= probabilityRiseMin
	if newWatch || levelRaised || probabilityRaised {
		percent := "미공개"
		if data.ActiveWatch.ResetChancePercent != nil {
			percent = fmt.Sprintf("%d%%", *data.ActiveWatch.ResetChancePercent)
		}
		message := fmt.Sprintf(
			"👀 Codex 리셋 가능성 감지\n단계. %s\n예상 확률. %s\n예측 구간. %s\n만료 시각 (UTC). %s\n만료 시각 (KST). %s\n%s",
			data.ActiveWatch.Level,
			percent,
			data.ActiveWatch.ForecastWindow,
			data.ActiveWatch.ExpiresAt,
			formatKST(data.ActiveWatch.ExpiresAt),
			data.ActiveWatch.Source.URL,
		)
		if _, err := sendTelegram(ctx, client, cfg, message, 0); err != nil {
			return fmt.Errorf("watch 알림 전송: %w", err)
		}
	}

	current.LastWatchObservedAt = data.ActiveWatch.ObservedAt
	current.LastWatchLevel = data.ActiveWatch.Level
	current.LastWatchPercent = data.ActiveWatch.ResetChancePercent
	return nil
}

func resetNotification(value reset) string {
	title := "🔄 Codex 사용량 리셋 발표"
	timeLabel := "발표 시각"
	if value.Source.Type == "observed" {
		title = "🔄 Codex 사용량 리셋 감지"
		timeLabel = "감지 시각"
	}

	message := fmt.Sprintf(
		"%s\n종류. %s\n%s (UTC). %s\n%s (KST). %s",
		title,
		value.ResetType,
		timeLabel,
		value.AnnouncedAt,
		timeLabel,
		formatKST(value.AnnouncedAt),
	)

	if value.Source.Type == "observed" {
		message += "\n출처. 공식 발표 없이 리셋이 관측됨"
	}
	if value.Source.URL == "" {
		return message + "\n원문 링크. 없음"
	}
	if value.Source.Type == "observed" {
		return message + "\n관련 링크. " + value.Source.URL
	}
	return message + "\n" + value.Source.URL
}

func formatKST(value string) string {
	timestamp, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return "변환 불가"
	}
	return timestamp.In(kstLocation).Format("2006-01-02 15:04:05 MST")
}

func percentRise(previous, current *int) int {
	if previous == nil || current == nil {
		return 0
	}
	return *current - *previous
}

func pruneResetStates(value *state, limit int) {
	type entry struct {
		id        string
		timestamp time.Time
	}

	completed := make([]entry, 0, len(value.Resets))
	for id, saved := range value.Resets {
		if !saved.Notified {
			continue
		}
		timestamp, err := time.Parse(time.RFC3339Nano, saved.AnnouncedAt)
		if err != nil {
			timestamp = saved.FirstSeenAt
		}
		completed = append(completed, entry{id: id, timestamp: timestamp})
	}
	if len(completed) <= limit {
		return
	}

	sort.Slice(completed, func(i, j int) bool {
		return completed[i].timestamp.After(completed[j].timestamp)
	})
	for _, item := range completed[limit:] {
		delete(value.Resets, item.id)
	}
}

func loadState(path string) (state, bool, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return state{}, false, nil
	}
	if err != nil {
		return state{}, false, fmt.Errorf("상태 파일 읽기: %w", err)
	}

	var result state
	if err := json.Unmarshal(data, &result); err != nil {
		return state{}, false, fmt.Errorf("상태 파일 JSON 해석: %w", err)
	}
	return result, true, nil
}

func saveState(path string, value state) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Errorf("상태 JSON 생성: %w", err)
	}
	data = append(data, '\n')

	temporaryPath := path + ".tmp"
	if err := os.WriteFile(temporaryPath, data, 0o600); err != nil {
		return fmt.Errorf("임시 상태 파일 쓰기: %w", err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("상태 파일 교체: %w", err)
	}
	return nil
}

func sendTelegram(ctx context.Context, client *http.Client, cfg config, text string, replyToMessageID int) (int, error) {
	requestBody := map[string]any{
		"chat_id":                  cfg.chatID,
		"text":                     text,
		"disable_web_page_preview": true,
	}
	if replyToMessageID != 0 {
		requestBody["reply_parameters"] = map[string]any{
			"message_id":                  replyToMessageID,
			"allow_sending_without_reply": true,
		}
	}

	payload, err := json.Marshal(requestBody)
	if err != nil {
		return 0, err
	}

	url := fmt.Sprintf("%s/bot%s/sendMessage", strings.TrimRight(cfg.telegramBaseURL, "/"), cfg.botToken)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return 0, err
	}
	var result struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
		Result      struct {
			MessageID int `json:"message_id"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return 0, fmt.Errorf("Telegram API JSON 해석: %w", err)
	}
	if resp.StatusCode != http.StatusOK || !result.OK {
		return 0, fmt.Errorf("Telegram API 응답 %s: %s", resp.Status, result.Description)
	}
	return result.Result.MessageID, nil
}

func acquireProcessLock(path string) (*os.File, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("실행 잠금 파일 열기: %w", err)
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		return nil, errors.New("codex-reset-monitor가 이미 실행 중입니다")
	}
	return file, nil
}

func releaseProcessLock(file *os.File) {
	_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
	_ = file.Close()
}
