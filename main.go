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
	"strings"
	"syscall"
	"time"
)

const (
	statusURL          = "https://codex-resets.com/api/v1/status"
	stateFile          = "state.json"
	pollInterval       = time.Minute
	probabilityRiseMin = 20
)

type config struct {
	botToken string
	chatID   string
}

type statusResponse struct {
	Data statusData `json:"data"`
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
	URL string `json:"url"`
}

type state struct {
	LastResetID         string `json:"last_reset_id"`
	LastWatchObservedAt string `json:"last_watch_observed_at"`
	LastWatchLevel      string `json:"last_watch_level"`
	LastWatchPercent    *int   `json:"last_watch_percent"`
}

func main() {
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
		botToken: os.Getenv("TELEGRAM_BOT_TOKEN"),
		chatID:   os.Getenv("TELEGRAM_CHAT_ID"),
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

	current := stateFrom(data)
	previous, exists, err := loadState(stateFile)
	if err != nil {
		return err
	}
	if !exists {
		if err := saveState(stateFile, current); err != nil {
			return err
		}
		log.Print("첫 실행 기준 상태를 저장했습니다")
		return nil
	}

	messages := notifications(previous, current, data)
	if len(messages) > 0 {
		if err := sendTelegram(ctx, client, cfg, strings.Join(messages, "\n\n")); err != nil {
			return fmt.Errorf("텔레그램 전송: %w", err)
		}
	}

	if err := saveState(stateFile, current); err != nil {
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

func stateFrom(data statusData) state {
	var result state
	if data.LatestReset != nil {
		result.LastResetID = data.LatestReset.ID
	}
	if data.ActiveWatch != nil {
		result.LastWatchObservedAt = data.ActiveWatch.ObservedAt
		result.LastWatchLevel = data.ActiveWatch.Level
		result.LastWatchPercent = data.ActiveWatch.ResetChancePercent
	}
	return result
}

func notifications(previous, current state, data statusData) []string {
	var messages []string

	if data.LatestReset != nil && current.LastResetID != previous.LastResetID {
		messages = append(messages, fmt.Sprintf(
			"🔄 Codex 사용량 리셋 발표\n종류. %s\n발표 시각. %s\n%s",
			data.LatestReset.ResetType,
			data.LatestReset.AnnouncedAt,
			data.LatestReset.Source.URL,
		))
	}

	if data.ActiveWatch == nil {
		return messages
	}

	newWatch := current.LastWatchObservedAt != previous.LastWatchObservedAt
	levelRaised := previous.LastWatchLevel == "elevated" && current.LastWatchLevel == "strong"
	probabilityRaised := percentRise(previous.LastWatchPercent, current.LastWatchPercent) >= probabilityRiseMin
	if newWatch || levelRaised || probabilityRaised {
		percent := "미공개"
		if data.ActiveWatch.ResetChancePercent != nil {
			percent = fmt.Sprintf("%d%%", *data.ActiveWatch.ResetChancePercent)
		}
		messages = append(messages, fmt.Sprintf(
			"👀 Codex 리셋 가능성 감지\n단계. %s\n예상 확률. %s\n예측 구간. %s\n만료 시각. %s\n%s",
			data.ActiveWatch.Level,
			percent,
			data.ActiveWatch.ForecastWindow,
			data.ActiveWatch.ExpiresAt,
			data.ActiveWatch.Source.URL,
		))
	}

	return messages
}

func percentRise(previous, current *int) int {
	if previous == nil || current == nil {
		return 0
	}
	return *current - *previous
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

func sendTelegram(ctx context.Context, client *http.Client, cfg config, text string) error {
	payload, err := json.Marshal(map[string]any{
		"chat_id":                  cfg.chatID,
		"text":                     text,
		"disable_web_page_preview": true,
	})
	if err != nil {
		return err
	}

	url := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", cfg.botToken)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("Telegram API 응답 %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	return nil
}
