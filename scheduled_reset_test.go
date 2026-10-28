// 예정 리셋 발표와 계정·실제 리셋의 양방향 매핑을 검증하는 테스트입니다.
package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

func TestFetchStatusParsesScheduledReset(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		body := `{"data":{"latest_reset":null,"scheduled_reset":{"id":"scheduled-1","status":"scheduled","reset_type":"regular","announced_at":"2026-10-28T02:43:38Z","scheduled_for":"2026-10-28T07:00:00Z","source":{"type":"x_post","url":"https://example.com/scheduled-1"}},"active_watch":null}}`
		return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	})}

	got, err := fetchStatus(context.Background(), client)
	if err != nil {
		t.Fatal(err)
	}
	if got.ScheduledReset == nil || got.ScheduledReset.ID != "scheduled-1" || got.ScheduledReset.ScheduledFor != "2026-10-28T07:00:00Z" {
		t.Fatalf("scheduled_reset 파싱 결과가 올바르지 않습니다: %#v", got.ScheduledReset)
	}
}

func TestCodexResetsScheduledIntegration(t *testing.T) {
	if os.Getenv("CODEX_RESETS_INTEGRATION_TEST") != "1" {
		t.Skip("CODEX_RESETS_INTEGRATION_TEST=1일 때 실제 scheduled_reset 응답을 읽습니다")
	}
	data, err := fetchStatus(context.Background(), &http.Client{Timeout: 15 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if data.ScheduledReset == nil {
		t.Skip("현재 활성 scheduled_reset이 없습니다")
	}
	if data.ScheduledReset.ID == "" || data.ScheduledReset.ResetType == "" || data.ScheduledReset.ScheduledFor == "" {
		t.Fatalf("실제 scheduled_reset 응답이 불완전합니다: %#v", data.ScheduledReset)
	}
	t.Logf("예정 발표 읽기 성공: id=%s type=%s scheduled_for=%s", data.ScheduledReset.ID, data.ScheduledReset.ResetType, data.ScheduledReset.ScheduledFor)
}

func TestScheduledFirstThenAccountResetRepliesAndDoesNotRepeat(t *testing.T) {
	requests, client := newTelegramTestClient(t)
	cfg := testConfig(t.TempDir() + "/state.json")
	announcedAt := time.Date(2026, 10, 28, 2, 43, 38, 0, time.UTC)
	scheduledFor := time.Date(2026, 10, 28, 7, 0, 0, 0, time.UTC)
	current := state{Resets: make(map[string]resetState), ScheduledResets: make(map[string]scheduledResetState), AccountResets: make(map[string]accountResetState)}
	scheduled := scheduledResetFixture("scheduled-1", announcedAt, scheduledFor)

	if err := processScheduledReset(context.Background(), client, cfg, &current, &scheduled, announcedAt); err != nil {
		t.Fatal(err)
	}
	event := scheduledAccountEvent(scheduledFor.Add(5*time.Minute), scheduledFor)
	if err := notifyAccountReset(context.Background(), client, cfg, &current, event, nil); err != nil {
		t.Fatal(err)
	}
	if len(*requests) != 2 || (*requests)[0].ReplyParameters != nil || (*requests)[1].ReplyParameters == nil || (*requests)[1].ReplyParameters.MessageID != 1 {
		t.Fatalf("계정 반영이 예정 발표 원본에 답장되지 않았습니다: %#v", *requests)
	}
	saved := current.ScheduledResets[scheduled.ID]
	if saved.Status != scheduledStatusApplied || saved.AccountResetID == "" {
		t.Fatalf("예정 발표에 계정 리셋이 연결되지 않았습니다: %#v", saved)
	}
	account := current.AccountResets[saved.AccountResetID]
	if account.ScheduledResetID != scheduled.ID {
		t.Fatalf("계정 리셋의 역참조가 저장되지 않았습니다: %#v", account)
	}

	if err := processScheduledReset(context.Background(), client, cfg, &current, &scheduled, announcedAt.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := notifyAccountReset(context.Background(), client, cfg, &current, event, nil); err != nil {
		t.Fatal(err)
	}
	if len(*requests) != 2 {
		t.Fatalf("예정 발표 또는 계정 반영이 반복 전송됐습니다: %#v", *requests)
	}
}

func TestScheduledLinkAddedRepliesOnlyOnce(t *testing.T) {
	requests, client := newTelegramTestClient(t)
	cfg := testConfig(t.TempDir() + "/state.json")
	now := time.Date(2026, 10, 28, 2, 43, 38, 0, time.UTC)
	current := state{ScheduledResets: make(map[string]scheduledResetState), AccountResets: make(map[string]accountResetState)}
	scheduled := scheduledResetFixture("link-later", now, now.Add(4*time.Hour))
	scheduled.Source.URL = ""
	if err := processScheduledReset(context.Background(), client, cfg, &current, &scheduled, now); err != nil {
		t.Fatal(err)
	}
	scheduled.Source.URL = "https://example.com/link-later"
	if err := processScheduledReset(context.Background(), client, cfg, &current, &scheduled, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := processScheduledReset(context.Background(), client, cfg, &current, &scheduled, now.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if len(*requests) != 2 || (*requests)[1].ReplyParameters == nil || (*requests)[1].ReplyParameters.MessageID != 1 || !strings.Contains((*requests)[1].Text, scheduled.Source.URL) {
		t.Fatalf("예정 발표 링크 추가가 한 번만 답장되지 않았습니다: %#v", *requests)
	}
}

func TestAccountFirstThenLateScheduledAnnouncementReplies(t *testing.T) {
	requests, client := newTelegramTestClient(t)
	cfg := testConfig(t.TempDir() + "/state.json")
	resetAt := time.Date(2026, 10, 28, 7, 0, 0, 0, time.UTC)
	current := state{Resets: make(map[string]resetState), ScheduledResets: make(map[string]scheduledResetState), AccountResets: make(map[string]accountResetState)}
	event := scheduledAccountEvent(resetAt, resetAt)
	if err := notifyAccountReset(context.Background(), client, cfg, &current, event, nil); err != nil {
		t.Fatal(err)
	}
	scheduled := scheduledResetFixture("scheduled-late", resetAt.Add(10*time.Minute), resetAt)
	if err := processScheduledReset(context.Background(), client, cfg, &current, &scheduled, resetAt.Add(10*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if len(*requests) != 2 || (*requests)[1].ReplyParameters == nil || (*requests)[1].ReplyParameters.MessageID != 1 || !strings.Contains((*requests)[1].Text, "예정 발표") {
		t.Fatalf("뒤늦은 예정 발표가 계정 메시지에 답장되지 않았습니다: %#v", *requests)
	}
	if current.ScheduledResets[scheduled.ID].AccountResetID == "" {
		t.Fatalf("뒤늦은 예정 발표에 계정 리셋이 연결되지 않았습니다: %#v", current.ScheduledResets[scheduled.ID])
	}
}

func TestAccountFirstDoesNotMatchDifferentFutureSchedule(t *testing.T) {
	requests, client := newTelegramTestClient(t)
	cfg := testConfig(t.TempDir() + "/state.json")
	resetAt := time.Date(2026, 10, 28, 5, 0, 0, 0, time.UTC)
	current := state{Resets: make(map[string]resetState), ScheduledResets: make(map[string]scheduledResetState), AccountResets: make(map[string]accountResetState)}
	if err := notifyAccountReset(context.Background(), client, cfg, &current, scheduledAccountEvent(resetAt, resetAt), nil); err != nil {
		t.Fatal(err)
	}
	scheduled := scheduledResetFixture("future", resetAt.Add(time.Hour), resetAt.Add(4*time.Hour))
	if err := processScheduledReset(context.Background(), client, cfg, &current, &scheduled, resetAt.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if len(*requests) != 2 || (*requests)[1].ReplyParameters != nil {
		t.Fatalf("미래의 다른 예정 발표가 기존 계정 리셋에 연결됐습니다: %#v", *requests)
	}
	if current.ScheduledResets[scheduled.ID].AccountResetID != "" {
		t.Fatalf("미래 예정 발표에 잘못된 계정 ID가 저장됐습니다: %#v", current.ScheduledResets[scheduled.ID])
	}
}

func TestMultipleScheduledCandidatesDoNotAutoMatchAccountReset(t *testing.T) {
	requests, client := newTelegramTestClient(t)
	cfg := testConfig(t.TempDir() + "/state.json")
	resetAt := time.Date(2026, 10, 28, 7, 0, 0, 0, time.UTC)
	current := state{Resets: make(map[string]resetState), ScheduledResets: make(map[string]scheduledResetState), AccountResets: make(map[string]accountResetState)}
	for _, id := range []string{"one", "two"} {
		value := scheduledResetFixture(id, resetAt.Add(-time.Hour), resetAt)
		current.ScheduledResets[id] = scheduledResetStateFrom(value, resetAt.Add(-time.Hour))
	}

	if err := notifyAccountReset(context.Background(), client, cfg, &current, scheduledAccountEvent(resetAt, resetAt), nil); err != nil {
		t.Fatal(err)
	}
	if len(*requests) != 1 || (*requests)[0].ReplyParameters != nil {
		t.Fatalf("다중 예정 후보가 계정 리셋에 자동 연결됐습니다: %#v", *requests)
	}
	for _, event := range current.AccountResets {
		if !event.ScheduledMappingAmbiguous || event.ScheduledResetID != "" {
			t.Fatalf("계정 리셋이 모호 상태로 저장되지 않았습니다: %#v", event)
		}
	}
}

func TestScheduledAnnouncementWithMultipleAccountCandidatesStaysIndependent(t *testing.T) {
	requests, client := newTelegramTestClient(t)
	cfg := testConfig(t.TempDir() + "/state.json")
	resetAt := time.Date(2026, 10, 28, 7, 0, 0, 0, time.UTC)
	one := scheduledAccountEvent(resetAt.Add(-30*time.Minute), resetAt.Add(-30*time.Minute))
	two := scheduledAccountEvent(resetAt.Add(30*time.Minute), resetAt.Add(30*time.Minute))
	current := state{Resets: make(map[string]resetState), ScheduledResets: make(map[string]scheduledResetState), AccountResets: map[string]accountResetState{
		"one": one,
		"two": two,
	}}
	current.AccountResets["one"] = withNotifiedMessage(one, 11)
	current.AccountResets["two"] = withNotifiedMessage(two, 12)
	scheduled := scheduledResetFixture("ambiguous", resetAt.Add(time.Hour), resetAt)

	if err := processScheduledReset(context.Background(), client, cfg, &current, &scheduled, resetAt.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if len(*requests) != 1 || (*requests)[0].ReplyParameters != nil {
		t.Fatalf("다중 계정 후보 중 하나에 예정 발표가 자동 연결됐습니다: %#v", *requests)
	}
	if !current.ScheduledResets[scheduled.ID].MappingAmbiguous {
		t.Fatalf("예정 발표가 모호 상태로 저장되지 않았습니다: %#v", current.ScheduledResets[scheduled.ID])
	}
}

func TestActualResetJoinsScheduledGroupAndDoesNotRepeat(t *testing.T) {
	requests, client := newTelegramTestClient(t)
	cfg := testConfig(t.TempDir() + "/state.json")
	resetAt := time.Date(2026, 10, 28, 7, 0, 0, 0, time.UTC)
	current := state{Resets: make(map[string]resetState), ScheduledResets: make(map[string]scheduledResetState), AccountResets: make(map[string]accountResetState)}
	scheduled := scheduledResetFixture("scheduled", resetAt.Add(-4*time.Hour), resetAt)
	if err := processScheduledReset(context.Background(), client, cfg, &current, &scheduled, resetAt.Add(-4*time.Hour)); err != nil {
		t.Fatal(err)
	}
	actual := reset{ID: "actual", ResetType: "regular", AnnouncedAt: resetAt.Format(time.RFC3339), Source: source{Type: "observed", URL: "https://example.com/actual"}}
	if err := processResets(context.Background(), client, cfg, &current, []reset{actual}, resetAt); err != nil {
		t.Fatal(err)
	}
	if len(*requests) != 2 || (*requests)[1].ReplyParameters == nil || (*requests)[1].ReplyParameters.MessageID != 1 {
		t.Fatalf("실제 리셋이 예정 발표 그룹에 답장되지 않았습니다: %#v", *requests)
	}
	if !strings.Contains((*requests)[1].Text, "리셋 완료 발표가 확인되었습니다") || !strings.Contains((*requests)[1].Text, "내 계정 반영 확인 대기 중") || strings.Contains((*requests)[1].Text, "실제 발생이 확인되었습니다") {
		t.Fatalf("완료 발표와 계정 반영 상태가 명확히 구분되지 않았습니다: %q", (*requests)[1].Text)
	}
	if current.ScheduledResets[scheduled.ID].ActualResetID != actual.ID || current.Resets[actual.ID].ScheduledResetID != scheduled.ID {
		t.Fatalf("실제 리셋과 예정 발표의 상호 참조가 저장되지 않았습니다: scheduled=%#v reset=%#v", current.ScheduledResets[scheduled.ID], current.Resets[actual.ID])
	}
	account := scheduledAccountEvent(resetAt.Add(5*time.Minute), resetAt)
	if err := notifyAccountReset(context.Background(), client, cfg, &current, account, []reset{actual}); err != nil {
		t.Fatal(err)
	}
	linkedSchedule := current.ScheduledResets[scheduled.ID]
	linkedAccount := current.AccountResets[linkedSchedule.AccountResetID]
	linkedActual := current.Resets[actual.ID]
	if linkedAccount.OfficialResetID != actual.ID || linkedActual.AccountResetID != linkedSchedule.AccountResetID {
		t.Fatalf("예정·실제·계정 리셋의 세 방향 참조가 닫히지 않았습니다: scheduled=%#v account=%#v actual=%#v", linkedSchedule, linkedAccount, linkedActual)
	}
	if err := processResets(context.Background(), client, cfg, &current, []reset{actual}, resetAt.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if len(*requests) != 3 {
		t.Fatalf("실제 리셋 링크가 반복 전송됐습니다: %#v", *requests)
	}
}

func TestUnappliedScheduledResetExpiresWithoutNotification(t *testing.T) {
	requests, client := newTelegramTestClient(t)
	cfg := testConfig(t.TempDir() + "/state.json")
	scheduledFor := time.Date(2026, 10, 28, 7, 0, 0, 0, time.UTC)
	value := scheduledResetFixture("expired", scheduledFor.Add(-4*time.Hour), scheduledFor)
	current := state{ScheduledResets: map[string]scheduledResetState{"expired": scheduledResetStateFrom(value, scheduledFor.Add(-4*time.Hour))}}

	if err := processScheduledReset(context.Background(), client, cfg, &current, nil, scheduledFor.Add(scheduledMatchAfter+time.Minute)); err != nil {
		t.Fatal(err)
	}
	if current.ScheduledResets["expired"].Status != scheduledStatusExpired || len(*requests) != 0 {
		t.Fatalf("미반영 예정 발표가 조용히 만료되지 않았습니다: state=%#v requests=%#v", current.ScheduledResets["expired"], *requests)
	}
}

func TestBaselineStoresScheduledResetWithoutNotification(t *testing.T) {
	now := time.Date(2026, 10, 28, 2, 43, 38, 0, time.UTC)
	value := scheduledResetFixture("baseline", now, now.Add(4*time.Hour))
	current := baselineState(statusData{ScheduledReset: &value}, nil, now)
	saved, exists := current.ScheduledResets[value.ID]
	if !exists || !saved.Notified || saved.TelegramMessageID != 0 {
		t.Fatalf("첫 실행 예정 발표 기준선이 올바르지 않습니다: %#v", current.ScheduledResets)
	}
}

func TestCheckOnceProcessesScheduledResetOnlyOnce(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	scheduled := scheduledResetFixture("check-once", now, now.Add(2*time.Hour))
	statusBody, err := json.Marshal(statusResponse{Data: statusData{ScheduledReset: &scheduled}})
	if err != nil {
		t.Fatal(err)
	}
	path := t.TempDir() + "/state.json"
	current := state{
		Resets:              make(map[string]resetState),
		ScheduledResets:     make(map[string]scheduledResetState),
		AccountResets:       make(map[string]accountResetState),
		PublicBaselineReady: true,
	}
	if err := saveState(path, current); err != nil {
		t.Fatal(err)
	}
	requests := make([]telegramRequest, 0, 1)
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		body := ""
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/api/v1/status":
			body = string(statusBody)
		case request.Method == http.MethodGet && request.URL.Path == "/api/v1/resets":
			body = `{"data":[]}`
		case request.Method == http.MethodPost:
			var telegram telegramRequest
			if err := json.NewDecoder(request.Body).Decode(&telegram); err != nil {
				return nil, err
			}
			requests = append(requests, telegram)
			body = `{"ok":true,"result":{"message_id":31}}`
		}
		return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	})}
	cfg := testConfig(path)
	if err := checkOnce(context.Background(), client, cfg); err != nil {
		t.Fatal(err)
	}
	if err := checkOnce(context.Background(), client, cfg); err != nil {
		t.Fatal(err)
	}
	if len(requests) != 1 || !strings.Contains(requests[0].Text, "리셋 예정") {
		t.Fatalf("예정 발표가 한 번만 처리되지 않았습니다: %#v", requests)
	}
	loaded, _, err := loadState(path)
	if err != nil || loaded.ScheduledResets[scheduled.ID].TelegramMessageID != 31 {
		t.Fatalf("예정 발표 상태가 저장되지 않았습니다: err=%v state=%#v", err, loaded.ScheduledResets)
	}
}

func TestScheduledMatchingTimeBoundaries(t *testing.T) {
	scheduledFor := time.Date(2026, 10, 28, 7, 0, 0, 0, time.UTC)
	announcedAt := scheduledFor.Add(-4 * time.Hour)
	scheduled := scheduledResetStateFrom(scheduledResetFixture("boundary", announcedAt, scheduledFor), announcedAt)
	tests := []struct {
		name         string
		eventAt      time.Time
		accountFirst bool
		want         bool
	}{
		{name: "발표 이후 36시간 경계", eventAt: scheduledFor.Add(36 * time.Hour), want: true},
		{name: "발표 이후 36시간 초과", eventAt: scheduledFor.Add(36*time.Hour + time.Second), want: false},
		{name: "계정 선행 예정 시각 2시간 경계", eventAt: scheduledFor.Add(-2 * time.Hour), accountFirst: true, want: true},
		{name: "계정 선행 예정 시각 2시간 초과", eventAt: scheduledFor.Add(-2*time.Hour - time.Second), accountFirst: true, want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := scheduled
			if test.accountFirst {
				candidate.AnnouncedAt = test.eventAt.Add(time.Minute).Format(time.RFC3339)
			}
			if got := scheduledMatchesTime(candidate, test.eventAt); got != test.want {
				t.Fatalf("scheduledMatchesTime() = %v, 기대값 = %v", got, test.want)
			}
		})
	}
}

func TestScheduledStateRoundTripAndPruning(t *testing.T) {
	now := time.Date(2026, 10, 28, 7, 0, 0, 0, time.UTC)
	current := state{ScheduledResets: make(map[string]scheduledResetState)}
	for index := 0; index < 55; index++ {
		id := string(rune('a' + index))
		value := scheduledResetFixture(id, now.Add(time.Duration(index)*time.Hour), now.Add(time.Duration(index+1)*time.Hour))
		saved := scheduledResetStateFrom(value, now)
		saved.Status = scheduledStatusExpired
		saved.Notified = true
		current.ScheduledResets[id] = saved
	}
	pruneScheduledResetStates(&current, 50)
	if len(current.ScheduledResets) != 50 {
		t.Fatalf("예정 발표 상태 정리 후 개수 = %d, 기대값 = 50", len(current.ScheduledResets))
	}
	path := t.TempDir() + "/state.json"
	if err := saveState(path, current); err != nil {
		t.Fatal(err)
	}
	loaded, exists, err := loadState(path)
	if err != nil || !exists || len(loaded.ScheduledResets) != 50 {
		t.Fatalf("예정 발표 상태 왕복 저장에 실패했습니다: exists=%v err=%v state=%#v", exists, err, loaded.ScheduledResets)
	}
}

func TestPublicAPIFailureStillMapsStoredScheduleToAccountReset(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	path := t.TempDir() + "/state.json"
	snapshot := usageSnapshot("account", 0, now.Add(7*24*time.Hour), 2)
	scheduled := scheduledResetFixture("stored", now.Add(-time.Hour), now)
	scheduledState := scheduledResetStateFrom(scheduled, now.Add(-time.Hour))
	scheduledState.Notified = true
	scheduledState.TelegramMessageID = 77
	current := state{
		Resets:              make(map[string]resetState),
		ScheduledResets:     map[string]scheduledResetState{scheduled.ID: scheduledState},
		PublicBaselineReady: true,
		AccountResets:       make(map[string]accountResetState),
		PendingAccountReset: &accountResetCandidate{
			AccountFingerprint:  "account",
			LimitID:             "codex",
			PlanType:            "plus",
			PreviousUsedPercent: 55,
			CurrentUsedPercent:  0,
			PreviousResetsAt:    now.Add(4 * 24 * time.Hour).Unix(),
			CurrentResetsAt:     snapshot.ResetsAt,
			FirstObservedAt:     now.Add(-20 * time.Second),
			UsageDropSignal:     true,
			NewWindowSignal:     true,
			DetectionReason:     "usage_drop_and_new_window",
		},
	}
	setAccountUsageBaseline(&current, snapshot)
	if err := saveState(path, current); err != nil {
		t.Fatal(err)
	}
	requests := make([]telegramRequest, 0, 1)
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.Method == http.MethodGet {
			return &http.Response{StatusCode: http.StatusServiceUnavailable, Status: "503 Service Unavailable", Body: io.NopCloser(strings.NewReader(`{"detail":"temporary failure"}`)), Header: make(http.Header)}, nil
		}
		var telegram telegramRequest
		if err := json.NewDecoder(request.Body).Decode(&telegram); err != nil {
			return nil, err
		}
		requests = append(requests, telegram)
		return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Body: io.NopCloser(strings.NewReader(`{"ok":true,"result":{"message_id":91}}`)), Header: make(http.Header)}, nil
	})}

	err := checkOnceWithUsage(context.Background(), client, testConfig(path), staticUsageReader{snapshot: snapshot})
	if err == nil || !strings.Contains(err.Error(), "503") {
		t.Fatalf("공개 API 오류가 반환되지 않았습니다: %v", err)
	}
	if len(requests) != 1 || requests[0].ReplyParameters == nil || requests[0].ReplyParameters.MessageID != 77 {
		t.Fatalf("저장된 예정 발표에 계정 반영이 답장되지 않았습니다: %#v", requests)
	}
	loaded, _, loadErr := loadState(path)
	if loadErr != nil || loaded.ScheduledResets[scheduled.ID].Status != scheduledStatusApplied || loaded.ScheduledResets[scheduled.ID].AccountResetID == "" {
		t.Fatalf("API 장애 중 예정·계정 연결 상태가 저장되지 않았습니다: err=%v state=%#v", loadErr, loaded.ScheduledResets)
	}
}

func scheduledResetFixture(id string, announcedAt, scheduledFor time.Time) scheduledReset {
	return scheduledReset{
		ID:           id,
		Status:       "scheduled",
		ResetType:    "regular",
		AnnouncedAt:  announcedAt.UTC().Format(time.RFC3339),
		ScheduledFor: scheduledFor.UTC().Format(time.RFC3339),
		Source:       source{Type: "x_post", URL: "https://example.com/" + id},
	}
}

func scheduledAccountEvent(detectedAt, windowStartedAt time.Time) accountResetState {
	event := regularAccountEvent(detectedAt)
	event.CurrentResetsAt = windowStartedAt.Add(7 * 24 * time.Hour).Unix()
	event.WindowDurationMins = 10080
	event.DetectionReason = "usage_drop_and_new_window"
	return event
}

func withNotifiedMessage(event accountResetState, messageID int) accountResetState {
	event.Notified = true
	event.TelegramMessageID = messageID
	return event
}
