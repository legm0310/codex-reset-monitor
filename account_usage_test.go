// Codex 계정 사용량 판정과 공식 리셋 매핑 정책을 검증하는 테스트입니다.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

func TestReadAccountUsageRPCAndFlatten(t *testing.T) {
	responses := strings.NewReader(strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"result":{"serverInfo":{"name":"codex","version":"1"}}}`,
		`{"jsonrpc":"2.0","method":"account/rateLimits/updated","params":{}}`,
		`{"jsonrpc":"2.0","id":2,"result":{"accountId":"secret-account-id","rateLimitResetCredits":{"availableCount":2},"rateLimits":{},"rateLimitsByLimitId":{"codex":{"limitId":"codex","planType":"plus","secondary":{"usedPercent":43,"resetsAt":1800000000,"windowDurationMins":10080}}}}}`,
	}, "\n") + "\n")
	var requests bytes.Buffer

	response, err := readAccountUsageRPC(&requests, responses)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := flattenAccountUsage(response)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.AccountFingerprint == "" || snapshot.AccountFingerprint == "secret-account-id" {
		t.Fatalf("계정 ID가 안전한 지문으로 변환되지 않았습니다: %q", snapshot.AccountFingerprint)
	}
	if snapshot.LimitID != "codex" || snapshot.PlanType != "plus" || snapshot.UsedPercent != 43 || snapshot.ResetsAt != 1800000000 || snapshot.ResetCredits == nil || *snapshot.ResetCredits != 2 {
		t.Fatalf("사용량 응답 변환 결과가 올바르지 않습니다: %#v", snapshot)
	}

	lines := strings.Split(strings.TrimSpace(requests.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("RPC 요청 개수 = %d, 기대값 = 3", len(lines))
	}
	var usageRequest map[string]any
	if err := json.Unmarshal([]byte(lines[2]), &usageRequest); err != nil {
		t.Fatal(err)
	}
	if usageRequest["method"] != "account/rateLimits/read" {
		t.Fatalf("사용량 RPC 메서드가 올바르지 않습니다: %#v", usageRequest)
	}
}

func TestFlattenAccountUsageAcceptsWeeklyPrimaryWindow(t *testing.T) {
	duration := int64(10080)
	resetsAt := int64(1800000000)
	response := appServerRateLimitsResponse{RateLimitsByLimitID: map[string]appServerRateLimit{
		"codex": {
			Primary: &appServerRateLimitWindow{UsedPercent: 27, ResetsAt: &resetsAt, WindowDurationMins: &duration},
		},
	}}
	snapshot, err := flattenAccountUsage(response)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.UsedPercent != 27 || snapshot.WindowDurationMins != 10080 || snapshot.ResetsAt != resetsAt {
		t.Fatalf("primary 주간 창을 선택하지 못했습니다: %#v", snapshot)
	}
}

func TestAccountUsageFirstReadAndIdentityChangeOnlySetBaseline(t *testing.T) {
	requests, client := newTelegramTestClient(t)
	cfg := testConfig(t.TempDir() + "/state.json")
	current := state{}
	now := time.Date(2026, 10, 23, 0, 0, 0, 0, time.UTC)
	first := usageSnapshot("account-a", 80, now.Add(5*24*time.Hour), 1)

	created, err := processAccountUsage(context.Background(), client, cfg, &current, first, nil, now)
	if err != nil || created || len(*requests) != 0 || current.AccountUsage == nil {
		t.Fatalf("첫 조회가 기준선만 저장하지 않았습니다: created=%v err=%v requests=%#v state=%#v", created, err, *requests, current)
	}
	changes := []struct {
		name     string
		snapshot accountUsageSnapshot
	}{
		{name: "계정", snapshot: usageSnapshot("account-b", 10, now.Add(6*24*time.Hour), 1)},
		{name: "플랜", snapshot: usageSnapshot("account-a", 10, now.Add(6*24*time.Hour), 1)},
		{name: "제한 ID", snapshot: usageSnapshot("account-a", 10, now.Add(6*24*time.Hour), 1)},
	}
	changes[1].snapshot.PlanType = "pro"
	changes[2].snapshot.LimitID = "codex-other"
	for _, test := range changes {
		t.Run(test.name, func(t *testing.T) {
			candidateState := state{}
			setAccountUsageBaseline(&candidateState, first)
			created, err := processAccountUsage(context.Background(), client, cfg, &candidateState, test.snapshot, nil, now.Add(time.Minute))
			if err != nil || created || candidateState.PendingAccountReset != nil {
				t.Fatalf("식별자 변경이 알림 없는 기준선으로 처리되지 않았습니다: created=%v err=%v state=%#v", created, err, candidateState)
			}
		})
	}
	if len(*requests) != 0 {
		t.Fatalf("식별자 변경 중 알림이 전송됐습니다: %#v", *requests)
	}
}

func TestAccountResetRequiresTwoObservationsAndDoesNotRepeat(t *testing.T) {
	requests, client := newTelegramTestClient(t)
	cfg := testConfig(t.TempDir() + "/state.json")
	now := time.Date(2026, 10, 23, 0, 0, 0, 0, time.UTC)
	current := state{}
	setAccountUsageBaseline(&current, usageSnapshot("account", 80, now.Add(5*24*time.Hour), 1))
	after := usageSnapshot("account", 10, now.Add(12*24*time.Hour), 1)

	created, err := processAccountUsage(context.Background(), client, cfg, &current, after, nil, now)
	if err != nil || !created || current.PendingAccountReset == nil || len(*requests) != 0 {
		t.Fatalf("첫 관측이 후보만 만들지 않았습니다: created=%v err=%v requests=%#v state=%#v", created, err, *requests, current)
	}
	after.UsedPercent = 11
	created, err = processAccountUsage(context.Background(), client, cfg, &current, after, nil, now.Add(20*time.Second))
	if err != nil || created || current.PendingAccountReset != nil || len(*requests) != 1 || !strings.Contains((*requests)[0].Text, "80% → 11%") {
		t.Fatalf("두 번째 관측이 계정 리셋 한 건을 확정하지 않았습니다: created=%v err=%v requests=%#v state=%#v", created, err, *requests, current)
	}
	if _, err := processAccountUsage(context.Background(), client, cfg, &current, after, nil, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if len(*requests) != 1 {
		t.Fatalf("확정한 계정 리셋이 반복 전송됐습니다: %#v", *requests)
	}
}

func TestNaturalResetAndSmallDropDoNotCreateCandidate(t *testing.T) {
	requests, client := newTelegramTestClient(t)
	cfg := testConfig(t.TempDir() + "/state.json")
	now := time.Date(2026, 10, 23, 0, 0, 0, 0, time.UTC)
	current := state{}
	setAccountUsageBaseline(&current, usageSnapshot("account", 80, now.Add(4*time.Minute), 1))

	natural := usageSnapshot("account", 5, now.Add(7*24*time.Hour), 1)
	created, err := processAccountUsage(context.Background(), client, cfg, &current, natural, nil, now)
	if err != nil || created || current.PendingAccountReset != nil {
		t.Fatalf("자연 갱신이 후보로 생성됐습니다: created=%v err=%v state=%#v", created, err, current)
	}
	setAccountUsageBaseline(&current, usageSnapshot("account", 50, now.Add(5*24*time.Hour), 1))
	small := usageSnapshot("account", 36, now.Add(5*24*time.Hour), 1)
	created, err = processAccountUsage(context.Background(), client, cfg, &current, small, nil, now)
	if err != nil || created || current.PendingAccountReset != nil || len(*requests) != 0 {
		t.Fatalf("15%%p 미만 감소가 후보로 생성됐습니다: created=%v err=%v state=%#v", created, err, current)
	}
	withoutResetTime := usageSnapshot("account", 80, time.Unix(0, 0), 1)
	withoutResetTime.ResetsAt = 0
	setAccountUsageBaseline(&current, withoutResetTime)
	unknownSchedule := withoutResetTime
	unknownSchedule.UsedPercent = 5
	created, err = processAccountUsage(context.Background(), client, cfg, &current, unknownSchedule, nil, now)
	if err != nil || created || current.PendingAccountReset != nil {
		t.Fatalf("갱신 시각을 모르는 감소가 후보로 생성됐습니다: created=%v err=%v state=%#v", created, err, current)
	}
}

func TestAccountResetCandidateMustRemainOnSecondObservation(t *testing.T) {
	requests, client := newTelegramTestClient(t)
	cfg := testConfig(t.TempDir() + "/state.json")
	now := time.Date(2026, 10, 23, 0, 0, 0, 0, time.UTC)
	current := state{}
	setAccountUsageBaseline(&current, usageSnapshot("account", 80, now.Add(5*24*time.Hour), 1))
	after := usageSnapshot("account", 10, now.Add(12*24*time.Hour), 1)
	if created, err := processAccountUsage(context.Background(), client, cfg, &current, after, nil, now); err != nil || !created {
		t.Fatalf("첫 후보 생성 실패: created=%v err=%v", created, err)
	}
	after.ResetsAt = now.Add(13 * 24 * time.Hour).Unix()
	after.UsedPercent = 79
	if created, err := processAccountUsage(context.Background(), client, cfg, &current, after, nil, now.Add(20*time.Second)); err != nil || created {
		t.Fatalf("불일치 재관측이 확정됐습니다: created=%v err=%v", created, err)
	}
	if current.PendingAccountReset != nil || len(*requests) != 0 {
		t.Fatalf("불일치 후보가 제거되지 않았습니다: state=%#v requests=%#v", current, *requests)
	}
}

func TestBankedCreditUseIsSuppressedAndGrantIsNotified(t *testing.T) {
	requests, client := newTelegramTestClient(t)
	cfg := testConfig(t.TempDir() + "/state.json")
	now := time.Date(2026, 10, 23, 0, 0, 0, 0, time.UTC)
	current := state{}
	setAccountUsageBaseline(&current, usageSnapshot("account", 90, now.Add(5*24*time.Hour), 1))

	used := usageSnapshot("account", 5, now.Add(12*24*time.Hour), 0)
	created, err := processAccountUsage(context.Background(), client, cfg, &current, used, nil, now)
	if err != nil || created || len(*requests) != 0 || current.PendingAccountReset != nil {
		t.Fatalf("banked reset 수동 사용이 일반 리셋으로 알려졌습니다: created=%v err=%v requests=%#v", created, err, *requests)
	}
	granted := usageSnapshot("account", 6, now.Add(12*24*time.Hour), 1)
	created, err = processAccountUsage(context.Background(), client, cfg, &current, granted, nil, now.Add(time.Minute))
	if err != nil || created || len(*requests) != 1 || !strings.Contains((*requests)[0].Text, "banked reset 지급") {
		t.Fatalf("banked reset 지급 알림이 올바르지 않습니다: created=%v err=%v requests=%#v", created, err, *requests)
	}
}

func TestDelayedUsageDropAfterBankedCreditUseIsSuppressed(t *testing.T) {
	requests, client := newTelegramTestClient(t)
	cfg := testConfig(t.TempDir() + "/state.json")
	now := time.Date(2026, 10, 23, 0, 0, 0, 0, time.UTC)
	current := state{}
	setAccountUsageBaseline(&current, usageSnapshot("account", 90, now.Add(5*24*time.Hour), 1))

	creditUsed := usageSnapshot("account", 90, now.Add(5*24*time.Hour), 0)
	if _, err := processAccountUsage(context.Background(), client, cfg, &current, creditUsed, nil, now); err != nil {
		t.Fatal(err)
	}
	usageUpdated := usageSnapshot("account", 5, now.Add(12*24*time.Hour), 0)
	created, err := processAccountUsage(context.Background(), client, cfg, &current, usageUpdated, nil, now.Add(time.Minute))
	if err != nil || created || current.PendingAccountReset != nil || len(*requests) != 0 {
		t.Fatalf("지연 반영된 banked reset 사용이 일반 리셋으로 감지됐습니다: created=%v err=%v state=%#v", created, err, current)
	}
}

func TestAccountFirstThenOfficialAnnouncementReplies(t *testing.T) {
	requests, client := newTelegramTestClient(t)
	cfg := testConfig(t.TempDir() + "/state.json")
	now := time.Date(2026, 10, 23, 0, 0, 0, 0, time.UTC)
	current := state{Resets: make(map[string]resetState), AccountResets: make(map[string]accountResetState)}
	event := regularAccountEvent(now)
	if err := notifyAccountReset(context.Background(), client, cfg, &current, event, nil); err != nil {
		t.Fatal(err)
	}
	official := officialReset("official", now.Add(time.Hour))
	if err := processResets(context.Background(), client, cfg, &current, []reset{official}, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if len(*requests) != 2 || (*requests)[1].ReplyParameters == nil || (*requests)[1].ReplyParameters.MessageID != 1 || !strings.Contains((*requests)[1].Text, official.Source.URL) {
		t.Fatalf("공식 발표가 계정 감지 메시지에 답장되지 않았습니다: %#v", *requests)
	}
}

func TestOfficialFirstThenAccountDetectionReplies(t *testing.T) {
	requests, client := newTelegramTestClient(t)
	cfg := testConfig(t.TempDir() + "/state.json")
	now := time.Date(2026, 10, 23, 0, 0, 0, 0, time.UTC)
	current := state{Resets: make(map[string]resetState), AccountResets: make(map[string]accountResetState)}
	official := officialReset("official", now)
	if err := processResets(context.Background(), client, cfg, &current, []reset{official}, now); err != nil {
		t.Fatal(err)
	}
	if err := notifyAccountReset(context.Background(), client, cfg, &current, regularAccountEvent(now.Add(time.Hour)), []reset{official}); err != nil {
		t.Fatal(err)
	}
	if len(*requests) != 2 || (*requests)[1].ReplyParameters == nil || (*requests)[1].ReplyParameters.MessageID != 1 || !strings.Contains((*requests)[1].Text, "내 Codex 계정에 리셋 반영") {
		t.Fatalf("계정 반영이 공식 발표 메시지에 답장되지 않았습니다: %#v", *requests)
	}
}

func TestSameCycleSendsOneCombinedMessage(t *testing.T) {
	requests, client := newTelegramTestClient(t)
	cfg := testConfig(t.TempDir() + "/state.json")
	now := time.Date(2026, 10, 23, 0, 0, 0, 0, time.UTC)
	current := state{Resets: make(map[string]resetState), AccountResets: make(map[string]accountResetState)}
	official := officialReset("official", now)
	if err := notifyAccountReset(context.Background(), client, cfg, &current, regularAccountEvent(now), []reset{official}); err != nil {
		t.Fatal(err)
	}
	if err := processResets(context.Background(), client, cfg, &current, []reset{official}, now); err != nil {
		t.Fatal(err)
	}
	if len(*requests) != 1 || !strings.Contains((*requests)[0].Text, "공식 발표. 함께 확인됨") || !strings.Contains((*requests)[0].Text, official.Source.URL) {
		t.Fatalf("같은 조회의 증거가 한 메시지로 결합되지 않았습니다: %#v", *requests)
	}
}

func TestAmbiguousOfficialCandidatesAreNotAutoMapped(t *testing.T) {
	requests, client := newTelegramTestClient(t)
	cfg := testConfig(t.TempDir() + "/state.json")
	now := time.Date(2026, 10, 23, 0, 0, 0, 0, time.UTC)
	current := state{Resets: make(map[string]resetState), AccountResets: make(map[string]accountResetState)}
	officials := []reset{officialReset("one", now), officialReset("two", now.Add(time.Hour))}
	if err := notifyAccountReset(context.Background(), client, cfg, &current, regularAccountEvent(now), officials); err != nil {
		t.Fatal(err)
	}
	for _, event := range current.AccountResets {
		if !event.MappingAmbiguous || event.OfficialResetID != "" {
			t.Fatalf("다중 후보가 모호한 미매핑 상태로 남지 않았습니다: %#v", event)
		}
	}
	if err := processResets(context.Background(), client, cfg, &current, officials, now.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if len(*requests) != 3 {
		t.Fatalf("모호한 후보가 계정 메시지에 자동 답장됐습니다: %#v", *requests)
	}
	for _, request := range *requests {
		if request.ReplyParameters != nil {
			t.Fatalf("모호한 후보에 답장이 생성됐습니다: %#v", *requests)
		}
	}
}

func TestOfficialAnnouncementWithMultipleAccountCandidatesIsNotMapped(t *testing.T) {
	requests, client := newTelegramTestClient(t)
	cfg := testConfig(t.TempDir() + "/state.json")
	now := time.Date(2026, 10, 23, 0, 0, 0, 0, time.UTC)
	one := regularAccountEvent(now.Add(-time.Hour))
	two := regularAccountEvent(now.Add(time.Hour))
	current := state{Resets: make(map[string]resetState), AccountResets: map[string]accountResetState{
		"one": {ResetType: one.ResetType, DetectedAt: one.DetectedAt, Notified: true, TelegramMessageID: 11},
		"two": {ResetType: two.ResetType, DetectedAt: two.DetectedAt, Notified: true, TelegramMessageID: 12},
	}}
	official := officialReset("official", now)
	if err := processResets(context.Background(), client, cfg, &current, []reset{official}, now); err != nil {
		t.Fatal(err)
	}
	if len(*requests) != 1 || (*requests)[0].ReplyParameters != nil || current.Resets[official.ID].AccountResetID != "" {
		t.Fatalf("공식 발표가 여러 계정 후보 중 하나에 연결됐습니다: requests=%#v state=%#v", *requests, current)
	}
}

func TestPruneAccountResetStatesKeepsLatestFifty(t *testing.T) {
	now := time.Date(2026, 10, 23, 0, 0, 0, 0, time.UTC)
	current := state{AccountResets: make(map[string]accountResetState)}
	for index := 0; index < 55; index++ {
		current.AccountResets[string(rune('a'+index))] = accountResetState{DetectedAt: now.Add(time.Duration(index) * time.Minute), Notified: true}
	}
	pruneAccountResetStates(&current, 50)
	if len(current.AccountResets) != 50 {
		t.Fatalf("계정 이벤트 정리 후 개수 = %d, 기대값 = 50", len(current.AccountResets))
	}
}

func TestAccountStateRoundTripDoesNotContainRawAccountID(t *testing.T) {
	path := t.TempDir() + "/state.json"
	current := state{Resets: make(map[string]resetState), AccountResets: make(map[string]accountResetState)}
	response := appServerRateLimitsResponse{AccountID: stringPointer("raw-secret-account")}
	duration := int64(10080)
	response.RateLimits.Secondary = &appServerRateLimitWindow{UsedPercent: 25, WindowDurationMins: &duration}
	snapshot, err := flattenAccountUsage(response)
	if err != nil {
		t.Fatal(err)
	}
	setAccountUsageBaseline(&current, snapshot)
	if err := saveState(path, current); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "raw-secret-account") {
		t.Fatal("state.json에 원문 계정 ID가 저장됐습니다")
	}
	loaded, exists, err := loadState(path)
	if err != nil || !exists || loaded.AccountUsage == nil || loaded.AccountUsage.AccountFingerprint == "" {
		t.Fatalf("계정 상태 왕복 저장에 실패했습니다: exists=%v err=%v state=%#v", exists, err, loaded)
	}
}

func TestCodexAccountUsageIntegration(t *testing.T) {
	if os.Getenv("CODEX_USAGE_INTEGRATION_TEST") != "1" {
		t.Skip("CODEX_USAGE_INTEGRATION_TEST=1일 때 로컬 Codex 계정 사용량을 읽습니다")
	}
	snapshot, err := (codexAppServerReader{binary: envOrDefault("CODEX_BINARY", "codex")}).Read(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.LimitID == "" || snapshot.UsedPercent < 0 || snapshot.UsedPercent > 100 {
		t.Fatalf("실제 사용량 응답이 올바르지 않습니다: limit=%q used=%d", snapshot.LimitID, snapshot.UsedPercent)
	}
	t.Logf("로컬 계정 사용량 읽기 성공: limit=%s plan=%s used=%d%%", snapshot.LimitID, snapshot.PlanType, snapshot.UsedPercent)
}

func TestAccountUsageFailureDoesNotStopPublicResetMonitoring(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	path := t.TempDir() + "/state.json"
	baseline := state{Resets: make(map[string]resetState), LastResetWatermarkAt: now.Add(-time.Hour).Format(time.RFC3339)}
	if err := saveState(path, baseline); err != nil {
		t.Fatal(err)
	}
	telegramRequests := make([]telegramRequest, 0, 1)
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		body := ""
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/api/v1/status":
			body = `{"data":{"latest_reset":null,"active_watch":null}}`
		case request.Method == http.MethodGet && request.URL.Path == "/api/v1/resets":
			body = `{"data":[{"id":"new","reset_type":"regular","announced_at":"` + now.Format(time.RFC3339) + `","source":{"type":"x_post","url":"https://example.com/new"}}]}`
		case request.Method == http.MethodPost:
			var telegramRequest telegramRequest
			if err := json.NewDecoder(request.Body).Decode(&telegramRequest); err != nil {
				return nil, err
			}
			telegramRequests = append(telegramRequests, telegramRequest)
			body = `{"ok":true,"result":{"message_id":7}}`
		default:
			return nil, errors.New("예상하지 못한 HTTP 요청")
		}
		return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	})}
	cfg := testConfig(path)
	cfg.telegramBaseURL = "https://telegram.test"

	if err := checkOnceWithUsage(context.Background(), client, cfg, failingUsageReader{}); err != nil {
		t.Fatal(err)
	}
	if len(telegramRequests) != 1 || !strings.Contains(telegramRequests[0].Text, "https://example.com/new") {
		t.Fatalf("계정 조회 실패 후 공개 리셋 알림이 전송되지 않았습니다: %#v", telegramRequests)
	}
}

func usageSnapshot(account string, used int, resetsAt time.Time, credits int64) accountUsageSnapshot {
	return accountUsageSnapshot{
		AccountFingerprint: account,
		LimitID:            "codex",
		PlanType:           "plus",
		UsedPercent:        used,
		ResetsAt:           resetsAt.Unix(),
		WindowDurationMins: 10080,
		ResetCredits:       int64Pointer(credits),
	}
}

func regularAccountEvent(detectedAt time.Time) accountResetState {
	return accountResetState{
		ResetType:           "regular",
		DetectedAt:          detectedAt,
		AccountFingerprint:  "account",
		LimitID:             "codex",
		PlanType:            "plus",
		PreviousUsedPercent: 80,
		CurrentUsedPercent:  5,
		PreviousResetsAt:    detectedAt.Add(5 * 24 * time.Hour).Unix(),
		CurrentResetsAt:     detectedAt.Add(12 * 24 * time.Hour).Unix(),
	}
}

func officialReset(id string, announcedAt time.Time) reset {
	return reset{
		ID:          id,
		ResetType:   "regular",
		AnnouncedAt: announcedAt.UTC().Format(time.RFC3339),
		Source:      source{Type: "x_post", URL: "https://example.com/" + id},
	}
}

func int64Pointer(value int64) *int64 {
	return &value
}

func stringPointer(value string) *string {
	return &value
}

type failingUsageReader struct{}

func (failingUsageReader) Read(context.Context) (accountUsageSnapshot, error) {
	return accountUsageSnapshot{}, errors.New("의도한 계정 조회 실패")
}
