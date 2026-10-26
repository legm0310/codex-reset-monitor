// Codex app-server에서 계정 사용량을 조회하고 리셋 감지와 공식 발표 매핑을 처리합니다.
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	accountConfirmationDelay = 20 * time.Second
	accountUsageTimeout      = 10 * time.Second
	accountResetDropMin      = 15
	accountMatchBefore       = 36 * time.Hour
	accountMatchAfter        = 24 * time.Hour
	bankedUseSuppression     = 5 * time.Minute
	weeklyWindowMinMinutes   = 24 * 60
	accountHistoryLimit      = 20
	freshWindowUsageMax      = 5
	windowStartTolerance     = 10 * time.Minute
	windowAdvanceMin         = 30 * time.Minute
)

type accountUsageReader interface {
	Read(context.Context) (accountUsageSnapshot, error)
}

type codexAppServerReader struct {
	binary string
}

type accountUsageSnapshot struct {
	AccountFingerprint string
	LimitID            string
	PlanType           string
	UsedPercent        int
	ResetsAt           int64
	WindowDurationMins int64
	ResetCredits       *int64
}

type accountUsageState struct {
	AccountFingerprint string    `json:"account_fingerprint"`
	LimitID            string    `json:"limit_id"`
	PlanType           string    `json:"plan_type"`
	UsedPercent        int       `json:"used_percent"`
	ResetsAt           int64     `json:"resets_at"`
	WindowDurationMins int64     `json:"window_duration_mins"`
	ResetCredits       *int64    `json:"reset_credits,omitempty"`
	SuppressDropUntil  time.Time `json:"suppress_drop_until,omitempty"`
}

type accountResetCandidate struct {
	AccountFingerprint  string    `json:"account_fingerprint"`
	LimitID             string    `json:"limit_id"`
	PlanType            string    `json:"plan_type"`
	PreviousUsedPercent int       `json:"previous_used_percent"`
	CurrentUsedPercent  int       `json:"current_used_percent"`
	PreviousResetsAt    int64     `json:"previous_resets_at"`
	CurrentResetsAt     int64     `json:"current_resets_at"`
	FirstObservedAt     time.Time `json:"first_observed_at"`
	UsageDropSignal     bool      `json:"usage_drop_signal,omitempty"`
	NewWindowSignal     bool      `json:"new_window_signal,omitempty"`
	DetectionReason     string    `json:"detection_reason,omitempty"`
}

type accountResetState struct {
	ResetType           string    `json:"reset_type"`
	DetectedAt          time.Time `json:"detected_at"`
	AccountFingerprint  string    `json:"account_fingerprint"`
	LimitID             string    `json:"limit_id"`
	PlanType            string    `json:"plan_type"`
	PreviousUsedPercent int       `json:"previous_used_percent"`
	CurrentUsedPercent  int       `json:"current_used_percent"`
	PreviousResetsAt    int64     `json:"previous_resets_at"`
	CurrentResetsAt     int64     `json:"current_resets_at"`
	PreviousCredits     *int64    `json:"previous_reset_credits,omitempty"`
	CurrentCredits      *int64    `json:"current_reset_credits,omitempty"`
	OfficialResetID     string    `json:"official_reset_id,omitempty"`
	MappingAmbiguous    bool      `json:"mapping_ambiguous,omitempty"`
	Notified            bool      `json:"notified"`
	TelegramMessageID   int       `json:"telegram_message_id"`
	DetectionReason     string    `json:"detection_reason,omitempty"`
}

type accountUsageObservation struct {
	ObservedAt         time.Time `json:"observed_at"`
	UsedPercent        int       `json:"used_percent"`
	ResetsAt           int64     `json:"resets_at"`
	WindowDurationMins int64     `json:"window_duration_mins"`
	ResetCredits       *int64    `json:"reset_credits,omitempty"`
	Outcome            string    `json:"outcome"`
}

type appServerRateLimitWindow struct {
	UsedPercent        int    `json:"usedPercent"`
	ResetsAt           *int64 `json:"resetsAt"`
	WindowDurationMins *int64 `json:"windowDurationMins"`
}

type appServerRateLimit struct {
	LimitID   *string                   `json:"limitId"`
	PlanType  *string                   `json:"planType"`
	Primary   *appServerRateLimitWindow `json:"primary"`
	Secondary *appServerRateLimitWindow `json:"secondary"`
}

type appServerRateLimitsResponse struct {
	AccountID            *string                       `json:"accountId"`
	RateLimits           appServerRateLimit            `json:"rateLimits"`
	RateLimitsByLimitID  map[string]appServerRateLimit `json:"rateLimitsByLimitId"`
	RateLimitResetCredit *struct {
		AvailableCount int64 `json:"availableCount"`
	} `json:"rateLimitResetCredits"`
}

type rpcResponse struct {
	ID     json.RawMessage `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func (reader codexAppServerReader) Read(ctx context.Context) (accountUsageSnapshot, error) {
	readCtx, cancel := context.WithTimeout(ctx, accountUsageTimeout)
	defer cancel()

	command := exec.CommandContext(readCtx, reader.binary, "app-server", "--listen", "stdio://")
	var stderr bytes.Buffer
	command.Stderr = &stderr
	stdin, err := command.StdinPipe()
	if err != nil {
		return accountUsageSnapshot{}, fmt.Errorf("app-server stdin 연결: %w", err)
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		return accountUsageSnapshot{}, fmt.Errorf("app-server stdout 연결: %w", err)
	}
	if err := command.Start(); err != nil {
		return accountUsageSnapshot{}, fmt.Errorf("app-server 시작: %w", err)
	}
	defer func() {
		_ = stdin.Close()
		if command.Process != nil {
			_ = command.Process.Kill()
		}
		_ = command.Wait()
	}()

	response, err := readAccountUsageRPC(stdin, stdout)
	if err != nil {
		if errors.Is(readCtx.Err(), context.DeadlineExceeded) {
			return accountUsageSnapshot{}, errors.New("app-server 응답 시간 초과")
		}
		if detail := strings.TrimSpace(stderr.String()); detail != "" {
			return accountUsageSnapshot{}, fmt.Errorf("%w: %s", err, detail)
		}
		return accountUsageSnapshot{}, err
	}
	return flattenAccountUsage(response)
}

func readAccountUsageRPC(writer io.Writer, reader io.Reader) (appServerRateLimitsResponse, error) {
	encoder := json.NewEncoder(writer)
	initialize := map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "initialize",
		"params": map[string]any{
			"clientInfo":   map[string]string{"name": "codex-reset-monitor", "title": "Codex Reset Monitor", "version": "1.0.0"},
			"capabilities": map[string]bool{"experimentalApi": true},
		},
	}
	if err := encoder.Encode(initialize); err != nil {
		return appServerRateLimitsResponse{}, fmt.Errorf("initialize 요청 쓰기: %w", err)
	}

	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), 2*1024*1024)
	if _, err := scanRPCResponse(scanner, "1"); err != nil {
		return appServerRateLimitsResponse{}, fmt.Errorf("initialize 응답: %w", err)
	}
	if err := encoder.Encode(map[string]any{"jsonrpc": "2.0", "method": "initialized", "params": map[string]any{}}); err != nil {
		return appServerRateLimitsResponse{}, fmt.Errorf("initialized 알림 쓰기: %w", err)
	}
	request := map[string]any{
		"jsonrpc": "2.0",
		"id":      2,
		"method":  "account/rateLimits/read",
		"params": map[string]bool{
			"excludeResetCreditDetails": true,
			"supportsLunaReserve":       false,
		},
	}
	if err := encoder.Encode(request); err != nil {
		return appServerRateLimitsResponse{}, fmt.Errorf("사용량 요청 쓰기: %w", err)
	}
	response, err := scanRPCResponse(scanner, "2")
	if err != nil {
		return appServerRateLimitsResponse{}, fmt.Errorf("사용량 응답: %w", err)
	}
	var result appServerRateLimitsResponse
	if err := json.Unmarshal(response.Result, &result); err != nil {
		return appServerRateLimitsResponse{}, fmt.Errorf("사용량 JSON 해석: %w", err)
	}
	return result, nil
}

func scanRPCResponse(scanner *bufio.Scanner, wantedID string) (rpcResponse, error) {
	for scanner.Scan() {
		var response rpcResponse
		if err := json.Unmarshal(scanner.Bytes(), &response); err != nil {
			continue
		}
		if strings.TrimSpace(string(response.ID)) != wantedID {
			continue
		}
		if response.Error != nil {
			return rpcResponse{}, fmt.Errorf("RPC %d: %s", response.Error.Code, response.Error.Message)
		}
		return response, nil
	}
	if err := scanner.Err(); err != nil {
		return rpcResponse{}, err
	}
	return rpcResponse{}, errors.New("app-server 연결이 응답 전에 종료됐습니다")
}

func flattenAccountUsage(response appServerRateLimitsResponse) (accountUsageSnapshot, error) {
	key, limit, ok := selectWeeklyLimit(response)
	window := selectWeeklyWindow(limit)
	if !ok || window == nil {
		return accountUsageSnapshot{}, fmt.Errorf("주간 사용량 제한을 찾을 수 없습니다 (%s)", summarizeRateLimitShape(response))
	}
	result := accountUsageSnapshot{LimitID: key, UsedPercent: window.UsedPercent}
	if response.AccountID != nil {
		digest := sha256.Sum256([]byte(*response.AccountID))
		result.AccountFingerprint = hex.EncodeToString(digest[:16])
	}
	if limit.LimitID != nil && *limit.LimitID != "" {
		result.LimitID = *limit.LimitID
	}
	if limit.PlanType != nil {
		result.PlanType = *limit.PlanType
	}
	if window.ResetsAt != nil {
		result.ResetsAt = *window.ResetsAt
	}
	if window.WindowDurationMins != nil {
		result.WindowDurationMins = *window.WindowDurationMins
	}
	if response.RateLimitResetCredit != nil {
		count := response.RateLimitResetCredit.AvailableCount
		result.ResetCredits = &count
	}
	return result, nil
}

func summarizeRateLimitShape(response appServerRateLimitsResponse) string {
	parts := make([]string, 0, len(response.RateLimitsByLimitID)+1)
	keys := make([]string, 0, len(response.RateLimitsByLimitID))
	for key := range response.RateLimitsByLimitID {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		value := response.RateLimitsByLimitID[key]
		parts = append(parts, fmt.Sprintf("%s primary=%s secondary=%s", key, rateLimitDuration(value.Primary), rateLimitDuration(value.Secondary)))
	}
	parts = append(parts, fmt.Sprintf("legacy primary=%s secondary=%s", rateLimitDuration(response.RateLimits.Primary), rateLimitDuration(response.RateLimits.Secondary)))
	return strings.Join(parts, ", ")
}

func rateLimitDuration(window *appServerRateLimitWindow) string {
	if window == nil {
		return "none"
	}
	if window.WindowDurationMins == nil {
		return "unknown"
	}
	return strconv.FormatInt(*window.WindowDurationMins, 10) + "m"
}

func selectWeeklyLimit(response appServerRateLimitsResponse) (string, appServerRateLimit, bool) {
	if value, exists := response.RateLimitsByLimitID["codex"]; exists && selectWeeklyWindow(value) != nil {
		return "codex", value, true
	}
	keys := make([]string, 0, len(response.RateLimitsByLimitID))
	for key := range response.RateLimitsByLimitID {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if selectWeeklyWindow(response.RateLimitsByLimitID[key]) != nil {
			return key, response.RateLimitsByLimitID[key], true
		}
	}
	if selectWeeklyWindow(response.RateLimits) != nil {
		return "default", response.RateLimits, true
	}
	return "", appServerRateLimit{}, false
}

func selectWeeklyWindow(limit appServerRateLimit) *appServerRateLimitWindow {
	windows := []*appServerRateLimitWindow{limit.Primary, limit.Secondary}
	var selected *appServerRateLimitWindow
	var selectedDuration int64
	for _, window := range windows {
		if window == nil || window.WindowDurationMins == nil || *window.WindowDurationMins < weeklyWindowMinMinutes {
			continue
		}
		if selected == nil || *window.WindowDurationMins > selectedDuration {
			selected = window
			selectedDuration = *window.WindowDurationMins
		}
	}
	return selected
}

func processAccountUsage(ctx context.Context, client *http.Client, cfg config, current *state, snapshot accountUsageSnapshot, resets []reset, now time.Time) (bool, error) {
	if current.AccountResets == nil {
		current.AccountResets = make(map[string]accountResetState)
	}
	if current.AccountUsage == nil || accountIdentityChanged(*current.AccountUsage, snapshot) {
		outcome := "baseline"
		if current.AccountUsage != nil {
			outcome = "identity_changed"
			log.Printf("Codex 계정 사용량 기준선 변경: account, plan 또는 limit ID가 변경됐습니다")
		}
		setAccountUsageBaseline(current, snapshot)
		current.PendingAccountReset = nil
		recordAccountObservation(current, snapshot, now, outcome)
		return false, nil
	}

	previous := *current.AccountUsage
	nextBaseline := accountUsageStateFrom(snapshot)
	nextBaseline.SuppressDropUntil = previous.SuppressDropUntil
	defer func() { current.AccountUsage = &nextBaseline }()
	if creditsDecreased(previous.ResetCredits, snapshot.ResetCredits) {
		current.PendingAccountReset = nil
		nextBaseline.SuppressDropUntil = now.Add(bankedUseSuppression)
		recordAccountObservation(current, snapshot, now, "banked_credit_used")
		log.Printf("Codex 계정 regular 리셋 판정 제외: banked reset 크레딧 감소")
		return false, nil
	}

	bankedGranted := false
	if creditsIncreased(previous.ResetCredits, snapshot.ResetCredits) {
		event := newAccountResetState("banked", previous, snapshot, now)
		event.DetectionReason = "banked_credit_increase"
		if err := notifyAccountReset(ctx, client, cfg, current, event, resets); err != nil {
			return false, err
		}
		bankedGranted = true
		log.Printf("Codex 계정 banked reset 지급 확정: credits %s -> %s", formatOptionalInt(previous.ResetCredits), formatOptionalInt(snapshot.ResetCredits))
	}

	if candidate := current.PendingAccountReset; candidate != nil {
		if !candidate.UsageDropSignal && !candidate.NewWindowSignal {
			candidate.UsageDropSignal = candidate.PreviousUsedPercent-candidate.CurrentUsedPercent >= accountResetDropMin
			candidate.DetectionReason = resetDetectionReason(candidate.UsageDropSignal, candidate.NewWindowSignal)
		}
		usageDropConfirmed, newWindowConfirmed := candidateConfirmedSignals(*candidate, snapshot)
		if usageDropConfirmed || newWindowConfirmed {
			current.PendingAccountReset = nil
			event := accountResetState{
				ResetType:           "regular",
				DetectedAt:          candidate.FirstObservedAt,
				AccountFingerprint:  candidate.AccountFingerprint,
				LimitID:             candidate.LimitID,
				PlanType:            candidate.PlanType,
				PreviousUsedPercent: candidate.PreviousUsedPercent,
				CurrentUsedPercent:  snapshot.UsedPercent,
				PreviousResetsAt:    candidate.PreviousResetsAt,
				CurrentResetsAt:     snapshot.ResetsAt,
				PreviousCredits:     cloneInt64(previous.ResetCredits),
				CurrentCredits:      cloneInt64(snapshot.ResetCredits),
				DetectionReason:     resetDetectionReason(usageDropConfirmed, newWindowConfirmed),
			}
			if err := notifyAccountReset(ctx, client, cfg, current, event, resets); err != nil {
				return false, err
			}
			recordAccountObservation(current, snapshot, now, "candidate_confirmed:"+event.DetectionReason)
			log.Printf("Codex 계정 regular 리셋 확정: reason=%s used=%d%%->%d%% resets_at=%d->%d", event.DetectionReason, event.PreviousUsedPercent, event.CurrentUsedPercent, event.PreviousResetsAt, event.CurrentResetsAt)
			return false, nil
		}
		current.PendingAccountReset = nil
		candidateBaseline := accountUsageState{
			AccountFingerprint: candidate.AccountFingerprint,
			LimitID:            candidate.LimitID,
			PlanType:           candidate.PlanType,
			UsedPercent:        candidate.PreviousUsedPercent,
			ResetsAt:           candidate.PreviousResetsAt,
			WindowDurationMins: snapshot.WindowDurationMins,
			ResetCredits:       cloneInt64(previous.ResetCredits),
		}
		usageDropSignal, newWindowSignal := accountResetSignals(candidateBaseline, snapshot, now)
		if usageDropSignal || newWindowSignal {
			reason := resetDetectionReason(usageDropSignal, newWindowSignal)
			current.PendingAccountReset = newAccountResetCandidate(candidateBaseline, snapshot, now, usageDropSignal, newWindowSignal)
			recordAccountObservation(current, snapshot, now, "candidate_replaced:"+reason)
			log.Printf("Codex 계정 regular 리셋 후보 교체: reason=%s used=%d%%->%d%% resets_at=%d->%d", reason, candidate.PreviousUsedPercent, snapshot.UsedPercent, candidate.PreviousResetsAt, snapshot.ResetsAt)
			return true, nil
		}
		recordAccountObservation(current, snapshot, now, "candidate_rejected")
		log.Printf("Codex 계정 regular 리셋 후보 취소: 재확인 조건 불충족, used=%d%% resets_at=%d", snapshot.UsedPercent, snapshot.ResetsAt)
		return false, nil
	}

	if now.Before(previous.SuppressDropUntil) {
		recordAccountObservation(current, snapshot, now, "banked_use_suppression")
		return false, nil
	}
	usageDropSignal, newWindowSignal := accountResetSignals(previous, snapshot, now)
	if !usageDropSignal && !newWindowSignal {
		outcome := "normal"
		if bankedGranted {
			outcome = "banked_grant"
		}
		recordAccountObservation(current, snapshot, now, outcome)
		return false, nil
	}
	reason := resetDetectionReason(usageDropSignal, newWindowSignal)
	current.PendingAccountReset = newAccountResetCandidate(previous, snapshot, now, usageDropSignal, newWindowSignal)
	outcome := "candidate_created:" + reason
	if bankedGranted {
		outcome += "+banked_grant"
	}
	recordAccountObservation(current, snapshot, now, outcome)
	log.Printf("Codex 계정 regular 리셋 후보 생성: reason=%s used=%d%%->%d%% resets_at=%d->%d", reason, previous.UsedPercent, snapshot.UsedPercent, previous.ResetsAt, snapshot.ResetsAt)
	return true, nil
}

func newAccountResetCandidate(previous accountUsageState, snapshot accountUsageSnapshot, now time.Time, usageDropSignal, newWindowSignal bool) *accountResetCandidate {
	return &accountResetCandidate{
		AccountFingerprint:  snapshot.AccountFingerprint,
		LimitID:             snapshot.LimitID,
		PlanType:            snapshot.PlanType,
		PreviousUsedPercent: previous.UsedPercent,
		CurrentUsedPercent:  snapshot.UsedPercent,
		PreviousResetsAt:    previous.ResetsAt,
		CurrentResetsAt:     snapshot.ResetsAt,
		FirstObservedAt:     now,
		UsageDropSignal:     usageDropSignal,
		NewWindowSignal:     newWindowSignal,
		DetectionReason:     resetDetectionReason(usageDropSignal, newWindowSignal),
	}
}

func notifyAccountReset(ctx context.Context, client *http.Client, cfg config, current *state, event accountResetState, resets []reset) error {
	eventID := accountResetID(event)
	if _, exists := current.AccountResets[eventID]; exists {
		return nil
	}
	matched, matchCount := findOfficialMatch(*current, event, resets)
	if matchCount == 1 {
		event.OfficialResetID = matched.ID
		saved, exists := current.Resets[matched.ID]
		if exists && saved.Notified {
			messageID, err := sendTelegram(ctx, client, cfg, accountAppliedNotification(event), saved.TelegramMessageID)
			if err != nil {
				return fmt.Errorf("계정 반영 답장 전송: %w", err)
			}
			event.TelegramMessageID = messageID
			event.Notified = true
			saved.AccountResetID = eventID
			current.Resets[matched.ID] = saved
		} else {
			messageID, err := sendTelegram(ctx, client, cfg, combinedAccountNotification(event, matched), 0)
			if err != nil {
				return fmt.Errorf("계정·공식 결합 알림 전송: %w", err)
			}
			event.TelegramMessageID = messageID
			event.Notified = true
			saved = resetStateFrom(matched, event.DetectedAt)
			saved.Notified = true
			saved.TelegramMessageID = messageID
			saved.AccountResetID = eventID
			current.Resets[matched.ID] = saved
			advanceResetWatermark(current, matched.AnnouncedAt)
		}
	} else {
		event.MappingAmbiguous = matchCount > 1
		messageID, err := sendTelegram(ctx, client, cfg, accountResetNotification(event), 0)
		if err != nil {
			return fmt.Errorf("계정 리셋 알림 전송: %w", err)
		}
		event.TelegramMessageID = messageID
		event.Notified = true
	}
	current.AccountResets[eventID] = event
	return nil
}

func accountIdentityChanged(previous accountUsageState, current accountUsageSnapshot) bool {
	return previous.AccountFingerprint != current.AccountFingerprint || previous.LimitID != current.LimitID || previous.PlanType != current.PlanType
}

func setAccountUsageBaseline(current *state, snapshot accountUsageSnapshot) {
	baseline := accountUsageStateFrom(snapshot)
	current.AccountUsage = &baseline
}

func accountUsageStateFrom(snapshot accountUsageSnapshot) accountUsageState {
	return accountUsageState{
		AccountFingerprint: snapshot.AccountFingerprint,
		LimitID:            snapshot.LimitID,
		PlanType:           snapshot.PlanType,
		UsedPercent:        snapshot.UsedPercent,
		ResetsAt:           snapshot.ResetsAt,
		WindowDurationMins: snapshot.WindowDurationMins,
		ResetCredits:       cloneInt64(snapshot.ResetCredits),
	}
}

func accountResetSignals(previous accountUsageState, snapshot accountUsageSnapshot, now time.Time) (bool, bool) {
	usageDrop := previous.UsedPercent-snapshot.UsedPercent >= accountResetDropMin
	newWindow := resetWindowStarted(previous.ResetsAt, snapshot, now)
	return usageDrop, newWindow
}

func candidateConfirmedSignals(candidate accountResetCandidate, snapshot accountUsageSnapshot) (bool, bool) {
	if candidate.AccountFingerprint != snapshot.AccountFingerprint || candidate.LimitID != snapshot.LimitID || candidate.PlanType != snapshot.PlanType {
		return false, false
	}
	usageDrop := candidate.UsageDropSignal && candidate.PreviousUsedPercent-snapshot.UsedPercent >= accountResetDropMin
	windowStartedAt, validWindow := usageWindowStartedAt(snapshot)
	newWindow := candidate.NewWindowSignal && validWindow && snapshot.UsedPercent <= freshWindowUsageMax &&
		snapshot.ResetsAt >= candidate.PreviousResetsAt+int64(windowAdvanceMin/time.Second) &&
		absDuration(windowStartedAt.Sub(candidate.FirstObservedAt)) <= windowStartTolerance+accountConfirmationDelay
	return usageDrop, newWindow
}

func resetWindowStarted(previousResetsAt int64, snapshot accountUsageSnapshot, now time.Time) bool {
	windowStartedAt, valid := usageWindowStartedAt(snapshot)
	return valid && previousResetsAt > 0 && snapshot.UsedPercent <= freshWindowUsageMax &&
		snapshot.ResetsAt >= previousResetsAt+int64(windowAdvanceMin/time.Second) &&
		absDuration(windowStartedAt.Sub(now)) <= windowStartTolerance
}

func usageWindowStartedAt(snapshot accountUsageSnapshot) (time.Time, bool) {
	if snapshot.ResetsAt <= 0 || snapshot.WindowDurationMins <= 0 {
		return time.Time{}, false
	}
	return time.Unix(snapshot.ResetsAt, 0).Add(-time.Duration(snapshot.WindowDurationMins) * time.Minute), true
}

func absDuration(value time.Duration) time.Duration {
	if value < 0 {
		return -value
	}
	return value
}

func resetDetectionReason(usageDrop, newWindow bool) string {
	switch {
	case usageDrop && newWindow:
		return "usage_drop_and_new_window"
	case usageDrop:
		return "usage_drop"
	case newWindow:
		return "new_window"
	default:
		return "unknown"
	}
}

func creditsIncreased(previous, current *int64) bool {
	return previous != nil && current != nil && *current > *previous
}

func creditsDecreased(previous, current *int64) bool {
	return previous != nil && current != nil && *current < *previous
}

func newAccountResetState(resetType string, previous accountUsageState, current accountUsageSnapshot, now time.Time) accountResetState {
	return accountResetState{
		ResetType:           resetType,
		DetectedAt:          now,
		AccountFingerprint:  current.AccountFingerprint,
		LimitID:             current.LimitID,
		PlanType:            current.PlanType,
		PreviousUsedPercent: previous.UsedPercent,
		CurrentUsedPercent:  current.UsedPercent,
		PreviousResetsAt:    previous.ResetsAt,
		CurrentResetsAt:     current.ResetsAt,
		PreviousCredits:     cloneInt64(previous.ResetCredits),
		CurrentCredits:      cloneInt64(current.ResetCredits),
	}
}

func accountResetID(event accountResetState) string {
	value := strings.Join([]string{
		event.AccountFingerprint,
		event.LimitID,
		event.ResetType,
		strconv.FormatInt(event.PreviousResetsAt, 10),
		strconv.FormatInt(event.CurrentResetsAt, 10),
		event.DetectedAt.UTC().Format(time.RFC3339Nano),
	}, "|")
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:16])
}

func cloneInt64(value *int64) *int64 {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func recordAccountObservation(current *state, snapshot accountUsageSnapshot, observedAt time.Time, outcome string) {
	current.AccountUsageHistory = append(current.AccountUsageHistory, accountUsageObservation{
		ObservedAt:         observedAt,
		UsedPercent:        snapshot.UsedPercent,
		ResetsAt:           snapshot.ResetsAt,
		WindowDurationMins: snapshot.WindowDurationMins,
		ResetCredits:       cloneInt64(snapshot.ResetCredits),
		Outcome:            outcome,
	})
	if len(current.AccountUsageHistory) > accountHistoryLimit {
		current.AccountUsageHistory = append([]accountUsageObservation(nil), current.AccountUsageHistory[len(current.AccountUsageHistory)-accountHistoryLimit:]...)
	}
}

func findOfficialMatch(current state, event accountResetState, resets []reset) (reset, int) {
	candidates := make(map[string]reset)
	for id, saved := range current.Resets {
		candidates[id] = resetFromState(id, saved)
	}
	for _, value := range resets {
		candidates[value.ID] = value
	}
	matches := make([]reset, 0, 2)
	for _, value := range candidates {
		saved := current.Resets[value.ID]
		if saved.AccountResetID != "" || value.ResetType != event.ResetType {
			continue
		}
		announcedAt, valid := parseTimestamp(value.AnnouncedAt)
		if !valid || announcedAt.Before(event.DetectedAt.Add(-accountMatchBefore)) || announcedAt.After(event.DetectedAt.Add(accountMatchAfter)) {
			continue
		}
		matches = append(matches, value)
	}
	if len(matches) != 1 {
		return reset{}, len(matches)
	}
	return matches[0], 1
}

func findAccountMatch(current state, value reset) (string, accountResetState, int) {
	announcedAt, valid := parseTimestamp(value.AnnouncedAt)
	if !valid {
		return "", accountResetState{}, 0
	}
	type match struct {
		id    string
		event accountResetState
	}
	matches := make([]match, 0, 2)
	for id, event := range current.AccountResets {
		if event.OfficialResetID != "" || event.MappingAmbiguous || event.ResetType != value.ResetType {
			continue
		}
		if announcedAt.Before(event.DetectedAt.Add(-accountMatchBefore)) || announcedAt.After(event.DetectedAt.Add(accountMatchAfter)) {
			continue
		}
		matches = append(matches, match{id: id, event: event})
	}
	if len(matches) != 1 {
		return "", accountResetState{}, len(matches)
	}
	return matches[0].id, matches[0].event, 1
}

func accountResetNotification(event accountResetState) string {
	title := "🔄 내 Codex 계정 리셋 감지"
	detail := fmt.Sprintf("주간 사용량. %d%% → %d%%", event.PreviousUsedPercent, event.CurrentUsedPercent)
	if event.ResetType == "banked" {
		title = "🏦 내 Codex 계정 banked reset 지급 감지"
		detail = fmt.Sprintf("사용 가능 크레딧. %s → %s", formatOptionalInt(event.PreviousCredits), formatOptionalInt(event.CurrentCredits))
	}
	reason := ""
	if event.DetectionReason != "" {
		reason = "\n감지 근거. " + detectionReasonLabel(event.DetectionReason)
	}
	return fmt.Sprintf("%s\n종류. %s\n%s%s\n감지 시각 (UTC). %s\n감지 시각 (KST). %s\n공식 발표. 확인 대기 중", title, event.ResetType, detail, reason, event.DetectedAt.UTC().Format(time.RFC3339), formatKST(event.DetectedAt.UTC().Format(time.RFC3339)))
}

func combinedAccountNotification(event accountResetState, official reset) string {
	message := accountResetNotification(event)
	message = strings.Replace(message, "공식 발표. 확인 대기 중", "공식 발표. 함께 확인됨", 1)
	if official.Source.URL != "" {
		message += "\n" + official.Source.URL
	}
	return message
}

func accountAppliedNotification(event accountResetState) string {
	if event.ResetType == "banked" {
		return fmt.Sprintf("✅ 내 Codex 계정에 banked reset 지급이 확인되었습니다\n사용 가능 크레딧. %s → %s\n확인 시각 (KST). %s", formatOptionalInt(event.PreviousCredits), formatOptionalInt(event.CurrentCredits), formatKST(event.DetectedAt.UTC().Format(time.RFC3339)))
	}
	return fmt.Sprintf("✅ 내 Codex 계정에 리셋 반영이 확인되었습니다\n주간 사용량. %d%% → %d%%\n감지 근거. %s\n확인 시각 (KST). %s", event.PreviousUsedPercent, event.CurrentUsedPercent, detectionReasonLabel(event.DetectionReason), formatKST(event.DetectedAt.UTC().Format(time.RFC3339)))
}

func detectionReasonLabel(reason string) string {
	switch reason {
	case "usage_drop_and_new_window":
		return "사용률 급감 및 새 사용량 창"
	case "usage_drop":
		return "사용률 급감"
	case "new_window":
		return "새 사용량 창"
	case "banked_credit_increase":
		return "banked reset 크레딧 증가"
	default:
		return "계정 사용량 변화"
	}
}

func officialMatchedNotification(value reset) string {
	message := "🔗 계정에서 먼저 감지한 리셋의 공식 발표가 확인되었습니다"
	if value.Source.URL != "" {
		message += "\n" + value.Source.URL
	}
	return message
}

func formatOptionalInt(value *int64) string {
	if value == nil {
		return "미확인"
	}
	return strconv.FormatInt(*value, 10)
}

func pruneAccountResetStates(value *state, limit int) {
	if len(value.AccountResets) <= limit {
		return
	}
	type entry struct {
		id string
		at time.Time
	}
	completed := make([]entry, 0, len(value.AccountResets))
	for id, event := range value.AccountResets {
		if event.Notified {
			completed = append(completed, entry{id: id, at: event.DetectedAt})
		}
	}
	if len(completed) <= limit {
		return
	}
	sort.Slice(completed, func(i, j int) bool { return completed[i].at.After(completed[j].at) })
	for _, item := range completed[limit:] {
		delete(value.AccountResets, item.id)
	}
}

func envEnabled(key string, defaultValue bool) bool {
	value, exists := os.LookupEnv(key)
	if !exists || strings.TrimSpace(value) == "" {
		return defaultValue
	}
	enabled, err := strconv.ParseBool(strings.TrimSpace(value))
	return err == nil && enabled
}

func envOrDefault(key, defaultValue string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return defaultValue
}
