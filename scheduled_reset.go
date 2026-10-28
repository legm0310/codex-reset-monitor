// 예정 리셋 발표의 저장, 만료, 계정·실제 리셋 양방향 매핑을 처리합니다.
package main

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"
)

const (
	scheduledMatchBefore          = time.Hour
	scheduledMatchAfter           = 36 * time.Hour
	accountFirstScheduleTolerance = 2 * time.Hour
	scheduledStatusPending        = "pending"
	scheduledStatusApplied        = "applied"
	scheduledStatusExpired        = "expired"
)

type scheduledReset struct {
	ID           string `json:"id"`
	Status       string `json:"status"`
	ResetType    string `json:"reset_type"`
	AnnouncedAt  string `json:"announced_at"`
	ScheduledFor string `json:"scheduled_for"`
	Source       source `json:"source"`
}

type scheduledResetState struct {
	ResetType         string    `json:"reset_type"`
	AnnouncedAt       string    `json:"announced_at"`
	ScheduledFor      string    `json:"scheduled_for"`
	SourceType        string    `json:"source_type"`
	URL               string    `json:"url"`
	FirstSeenAt       time.Time `json:"first_seen_at"`
	Status            string    `json:"status"`
	Notified          bool      `json:"notified"`
	TelegramMessageID int       `json:"telegram_message_id"`
	AccountResetID    string    `json:"account_reset_id,omitempty"`
	ActualResetID     string    `json:"actual_reset_id,omitempty"`
	MappingAmbiguous  bool      `json:"mapping_ambiguous,omitempty"`
}

func processScheduledReset(ctx context.Context, client *http.Client, cfg config, current *state, value *scheduledReset, now time.Time) error {
	if current.ScheduledResets == nil {
		current.ScheduledResets = make(map[string]scheduledResetState)
	}
	expireScheduledResets(current, now)
	if value == nil || value.ID == "" {
		return nil
	}

	saved, exists := current.ScheduledResets[value.ID]
	if !exists {
		saved = scheduledResetStateFrom(*value, now)
		accountID, accountEvent, matchCount := findAccountMatchForScheduled(*current, *value)
		if matchCount == 1 {
			saved.AccountResetID = accountID
			saved.Status = scheduledStatusApplied
			saved.Notified = true
			accountEvent.ScheduledResetID = value.ID
			if accountEvent.OfficialResetID != "" {
				saved.ActualResetID = accountEvent.OfficialResetID
				if actual, exists := current.Resets[accountEvent.OfficialResetID]; exists {
					actual.ScheduledResetID = value.ID
					current.Resets[accountEvent.OfficialResetID] = actual
				}
			}
			current.AccountResets[accountID] = accountEvent
			if accountEvent.Notified && accountEvent.TelegramMessageID != 0 {
				if _, err := sendTelegram(ctx, client, cfg, scheduledMatchedToAccountNotification(*value), accountEvent.TelegramMessageID); err != nil {
					return fmt.Errorf("뒤늦은 예정 발표 답장 전송: %w", err)
				}
				saved.TelegramMessageID = accountEvent.TelegramMessageID
			} else {
				messageID, err := sendTelegram(ctx, client, cfg, scheduledResetNotification(*value), 0)
				if err != nil {
					return fmt.Errorf("예정 발표 알림 전송: %w", err)
				}
				saved.TelegramMessageID = messageID
			}
		} else {
			saved.MappingAmbiguous = matchCount > 1
			messageID, err := sendTelegram(ctx, client, cfg, scheduledResetNotification(*value), 0)
			if err != nil {
				return fmt.Errorf("예정 발표 알림 전송: %w", err)
			}
			saved.Notified = true
			saved.TelegramMessageID = messageID
		}
	} else {
		linkAdded := saved.URL == "" && value.Source.URL != ""
		if linkAdded && saved.Notified && saved.TelegramMessageID != 0 {
			if _, err := sendTelegram(ctx, client, cfg, scheduledLinkAddedNotification(*value), saved.TelegramMessageID); err != nil {
				return fmt.Errorf("예정 발표 링크 추가 답장 전송: %w", err)
			}
		}
		saved.ResetType = value.ResetType
		saved.AnnouncedAt = value.AnnouncedAt
		saved.ScheduledFor = value.ScheduledFor
		saved.SourceType = value.Source.Type
		saved.URL = value.Source.URL
	}
	current.ScheduledResets[value.ID] = saved
	return nil
}

func scheduledResetStateFrom(value scheduledReset, firstSeenAt time.Time) scheduledResetState {
	return scheduledResetState{
		ResetType:    value.ResetType,
		AnnouncedAt:  value.AnnouncedAt,
		ScheduledFor: value.ScheduledFor,
		SourceType:   value.Source.Type,
		URL:          value.Source.URL,
		FirstSeenAt:  firstSeenAt,
		Status:       scheduledStatusPending,
	}
}

func scheduledResetFromState(id string, value scheduledResetState) scheduledReset {
	return scheduledReset{
		ID:           id,
		Status:       "scheduled",
		ResetType:    value.ResetType,
		AnnouncedAt:  value.AnnouncedAt,
		ScheduledFor: value.ScheduledFor,
		Source:       source{Type: value.SourceType, URL: value.URL},
	}
}

func expireScheduledResets(current *state, now time.Time) {
	for id, saved := range current.ScheduledResets {
		if saved.Status != scheduledStatusPending || saved.AccountResetID != "" {
			continue
		}
		scheduledFor, valid := parseTimestamp(saved.ScheduledFor)
		if valid && now.After(scheduledFor.Add(scheduledMatchAfter)) {
			saved.Status = scheduledStatusExpired
			current.ScheduledResets[id] = saved
		}
	}
}

func findScheduledMatch(current state, event accountResetState) (string, scheduledResetState, int) {
	type match struct {
		id    string
		value scheduledResetState
	}
	matches := make([]match, 0, 2)
	for id, saved := range current.ScheduledResets {
		if saved.Status == scheduledStatusExpired || saved.AccountResetID != "" || saved.MappingAmbiguous || saved.ResetType != event.ResetType {
			continue
		}
		if scheduledMatchesAccountTime(saved, event) {
			matches = append(matches, match{id: id, value: saved})
		}
	}
	if len(matches) != 1 {
		return "", scheduledResetState{}, len(matches)
	}
	return matches[0].id, matches[0].value, 1
}

func findAccountMatchForScheduled(current state, value scheduledReset) (string, accountResetState, int) {
	saved := scheduledResetStateFrom(value, time.Time{})
	type match struct {
		id    string
		event accountResetState
	}
	matches := make([]match, 0, 2)
	for id, event := range current.AccountResets {
		if event.ScheduledResetID != "" || event.ScheduledMappingAmbiguous || event.ResetType != value.ResetType {
			continue
		}
		if scheduledMatchesAccountTime(saved, event) {
			matches = append(matches, match{id: id, event: event})
		}
	}
	if len(matches) != 1 {
		return "", accountResetState{}, len(matches)
	}
	return matches[0].id, matches[0].event, 1
}

func findScheduledMatchForReset(current state, value reset) (string, scheduledResetState, int) {
	resetAt, valid := parseTimestamp(value.AnnouncedAt)
	if !valid {
		return "", scheduledResetState{}, 0
	}
	type match struct {
		id    string
		value scheduledResetState
	}
	matches := make([]match, 0, 2)
	for id, saved := range current.ScheduledResets {
		if saved.Status == scheduledStatusExpired || saved.ActualResetID != "" || saved.MappingAmbiguous || saved.ResetType != value.ResetType {
			continue
		}
		if scheduledMatchesTime(saved, resetAt) {
			matches = append(matches, match{id: id, value: saved})
		}
	}
	if len(matches) != 1 {
		return "", scheduledResetState{}, len(matches)
	}
	return matches[0].id, matches[0].value, 1
}

func scheduledMatchesAccountTime(scheduled scheduledResetState, event accountResetState) bool {
	return scheduledMatchesTime(scheduled, accountResetEffectiveAt(event))
}

func scheduledMatchesTime(scheduled scheduledResetState, eventAt time.Time) bool {
	announcedAt, validAnnouncement := parseTimestamp(scheduled.AnnouncedAt)
	scheduledFor, validSchedule := parseTimestamp(scheduled.ScheduledFor)
	if !validAnnouncement || !validSchedule || eventAt.IsZero() {
		return false
	}
	if announcedAt.After(eventAt) {
		return absDuration(scheduledFor.Sub(eventAt)) <= accountFirstScheduleTolerance
	}
	return !eventAt.Before(announcedAt.Add(-scheduledMatchBefore)) && !eventAt.After(scheduledFor.Add(scheduledMatchAfter))
}

func accountResetEffectiveAt(event accountResetState) time.Time {
	if strings.Contains(event.DetectionReason, "new_window") && event.CurrentResetsAt > 0 && event.WindowDurationMins > 0 {
		return time.Unix(event.CurrentResetsAt, 0).Add(-time.Duration(event.WindowDurationMins) * time.Minute)
	}
	return event.DetectedAt
}

func scheduledResetNotification(value scheduledReset) string {
	message := fmt.Sprintf(
		"⏳ Codex 사용량 리셋 예정\n종류. %s\n상태. 내 계정 반영 대기 중\n발표 시각 (UTC). %s\n발표 시각 (KST). %s\n예정 시각 (UTC). %s\n예정 시각 (KST). %s",
		value.ResetType,
		value.AnnouncedAt,
		formatKST(value.AnnouncedAt),
		value.ScheduledFor,
		formatKST(value.ScheduledFor),
	)
	if value.Source.URL != "" {
		message += "\n" + value.Source.URL
	}
	return message
}

func scheduledMatchedToAccountNotification(value scheduledReset) string {
	message := fmt.Sprintf(
		"📢 계정에서 먼저 감지한 리셋의 예정 발표가 확인되었습니다\n예정 시각 (KST). %s",
		formatKST(value.ScheduledFor),
	)
	if value.Source.URL != "" {
		message += "\n" + value.Source.URL
	}
	return message
}

func combinedScheduledAccountNotification(event accountResetState, scheduled scheduledReset) string {
	message := accountResetNotification(event)
	message = strings.Replace(message, "공식 발표. 확인 대기 중", fmt.Sprintf("예정 발표. 함께 확인됨\n예정 시각 (KST). %s", formatKST(scheduled.ScheduledFor)), 1)
	if scheduled.Source.URL != "" {
		message += "\n" + scheduled.Source.URL
	}
	return message
}

func actualResetMatchedNotification(value reset) string {
	message := fmt.Sprintf("🔗 예정된 Codex 리셋의 실제 발생이 확인되었습니다\n확인 시각 (KST). %s", formatKST(value.AnnouncedAt))
	if value.Source.URL != "" {
		message += "\n" + value.Source.URL
	}
	return message
}

func scheduledLinkAddedNotification(value scheduledReset) string {
	return "🔗 Codex 리셋 예정 발표 링크가 추가되었습니다\n" + value.Source.URL
}

func pruneScheduledResetStates(current *state, limit int) {
	if len(current.ScheduledResets) <= limit {
		return
	}
	type entry struct {
		id string
		at time.Time
	}
	completed := make([]entry, 0, len(current.ScheduledResets))
	for id, saved := range current.ScheduledResets {
		if saved.Status == scheduledStatusPending {
			continue
		}
		at, valid := parseTimestamp(saved.ScheduledFor)
		if !valid {
			at = saved.FirstSeenAt
		}
		completed = append(completed, entry{id: id, at: at})
	}
	if len(completed) <= limit {
		return
	}
	sort.Slice(completed, func(i, j int) bool { return completed[i].at.After(completed[j].at) })
	for _, item := range completed[limit:] {
		delete(current.ScheduledResets, item.id)
	}
}
