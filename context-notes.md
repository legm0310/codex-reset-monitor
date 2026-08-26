# Codex Reset Monitor 컨텍스트 노트

## 2026-08-24 초기 결정

- 프로젝트 경로는 `/Users/dev4/track-reset/codex-reset-monitor`로 정한다.
- 공개 상태 API `https://codex-resets.com/api/v1/status`를 60초마다 조회한다.
- Telegram 설정은 `TELEGRAM_BOT_TOKEN`, `TELEGRAM_CHAT_ID` 환경변수로 받는다.
- 실행 디렉터리의 `state.json`에 `last_reset_id`, `last_watch_observed_at`, `last_watch_level`, `last_watch_percent`를 유지한다.
- 첫 실행은 기존 이벤트를 새 이벤트로 오인하지 않도록 현재 상태만 저장한다.
- 신규 리셋, 신규 watch, `elevated`에서 `strong`으로의 상승, 20%p 이상의 확률 상승을 알림 조건으로 정한다.
- 같은 조회에서 여러 변화가 발생하면 한 메시지로 합쳐 전송하고, 전송 성공 후에만 상태를 저장한다.
- 상태 파일은 임시 파일 작성 후 rename하여 갱신 중 손상 가능성을 줄인다.
- 작업공간은 Git 저장소가 아니며 사용자가 커밋을 잠시 보류해 달라고 요청했다.

## 2026-08-24 구현 결과

- Go 표준 라이브러리만 사용해 외부 모듈 의존성을 만들지 않았다.
- 같은 조회에서 리셋과 watch가 동시에 변하면 Telegram 메시지 한 건으로 합친다.
- Telegram 전송 실패 시 `state.json`을 갱신하지 않아 다음 조회에서 재시도한다.
- `state.json`과 임시 상태 파일, 빌드 바이너리는 `.gitignore`에 포함했다.
- `gofmt -w main.go main_test.go`를 실행했다.
- 기본 Go 캐시가 샌드박스에서 차단되어 `GOCACHE=/tmp/codex-reset-monitor-go-cache`를 사용했다.
- `env GOCACHE=/tmp/codex-reset-monitor-go-cache go test ./...`가 통과했다.
- `env GOCACHE=/tmp/codex-reset-monitor-go-cache go build -o /tmp/codex-reset-monitor .`가 통과했다.

## 2026-08-24 dotenv 및 커밋 결정

- 사용자가 `.env` 파일 지원과 `.env.example` 추가를 요청했다.
- 외부 모듈 없이 단순 `KEY=VALUE`, 따옴표 값, 빈 줄, 주석, `export` 접두사를 지원한다.
- 이미 설정된 시스템 환경변수는 `.env` 값으로 덮어쓰지 않는다.
- 실제 `.env`는 Git에서 제외하고 `.env.example`만 추적한다.
- 사용자가 커밋을 허용했으며 커밋 메시지는 `feat(config): .env 설정 지원`으로 정한다.
- `gofmt -w main.go main_test.go`를 다시 실행했다.
- `env GOCACHE=/tmp/codex-reset-monitor-go-cache go test ./...`가 통과했다.
- `env GOCACHE=/tmp/codex-reset-monitor-go-cache go build -o /tmp/codex-reset-monitor .`가 통과했다.
- `git diff --check`가 통과했다.

## 2026-08-26 리셋 정보 보강 정책

- `x_post`는 URL이 필수지만 `observed`는 공식 발표 없이 감지될 수 있어 URL이 영구적으로 없을 수 있다.
- 새 리셋은 URL 유무와 관계없이 즉시 알린다.
- URL이 없으면 observed 출처와 원문 링크 없음 상태를 메시지에 명시한다.
- 이미 알린 같은 리셋 ID에 URL이 추가되면 전체 알림 대신 링크 후속 알림을 한 번 보낸다.
- 리셋 종류가 변경되면 변경 알림을 보내고, 발표 시각만 수정되면 상태만 갱신한다.
- 기존 상태 파일에는 새 필드가 없으므로 Go JSON의 zero value로 읽어 마이그레이션 없이 호환한다.
- 기존 상태에 URL 필드가 없고 현재 API에 같은 리셋의 URL이 있으면 배포 후 링크 후속 알림이 한 번 발생한다.
- `gofmt -w main.go main_test.go`를 실행했다.
- `env GOCACHE=/tmp/codex-reset-monitor-go-cache go test ./...`가 통과했다.
- `env GOCACHE=/tmp/codex-reset-monitor-go-cache go build -o /tmp/codex-reset-monitor .`가 통과했다.
- `git diff --check`가 통과했다.

## 2026-08-26 리셋별 상태와 2분 대기 정책

- 링크 없는 신규 리셋은 최초 발견부터 2분간 기다린다.
- 2분 이내 링크가 추가되면 링크를 포함한 전체 알림을 보내고, 계속 없으면 링크 없음으로 알린다.
- 최초 알림의 Telegram `message_id`를 리셋 ID별로 저장한다.
- 이미 알린 리셋에 링크가 추가되면 해당 `message_id`에 답장하고, 답장 본문에는 리셋 ID를 노출하지 않는다.
- 최신 리셋이 바뀐 뒤 이전 리셋이 보강되는 경우를 찾기 위해 reset 목록 API를 함께 조회한다.
- 상태는 최신 50건으로 제한하되 아직 알리지 않은 pending 항목은 삭제하지 않는다.
- 기존 단일 리셋 상태는 알림 완료 상태로 마이그레이션하며 기존 Telegram 메시지 ID는 없는 것으로 처리한다.
- 여러 프로세스가 같은 상태를 읽어 중복 전송하지 않도록 비차단 파일 잠금으로 단일 실행을 보장한다.
- 미추적 `nohup.out`은 계속 사용자 파일로 보존한다.
- 기존 상태를 마이그레이션할 때 목록 API의 다른 과거 리셋은 `notified=true` 기준 상태로 채워 과거 알림이 대량 전송되지 않게 한다.
- 자동 테스트는 포트 바인딩이 필요 없는 메모리 HTTP 전송 모형으로 Telegram 요청과 답장 대상을 검증한다.
- `gofmt -w main.go main_test.go`를 실행했다.
- `env GOCACHE=/tmp/codex-reset-monitor-go-cache go test ./...`가 통과했다.
- `env GOCACHE=/tmp/codex-reset-monitor-go-cache go build -o /tmp/codex-reset-monitor .`가 통과했다.
- `env GOCACHE=/tmp/codex-reset-monitor-go-cache go vet ./...`가 통과했다.
- `git diff --check`가 통과했다.
- `TELEGRAM_INTEGRATION_TEST=1 go test -run '^TestTelegramNotificationFlow$' -v`로 실제 Telegram 네 단계 알림과 이전 메시지 답장을 검증했다.
- 실제 Telegram 테스트는 운영 `state.json`을 사용하거나 변경하지 않는다.

## 2026-08-26 KST 표시 결정

- 사용자가 Telegram 메시지에 UTC와 함께 KST 시각 표시를 요청했다.
- API 원문 UTC 시각은 유지하고 바로 다음 줄에 KST 변환 시각을 추가한다.
- 리셋의 `announced_at`과 watch의 `expires_at` 모두 같은 형식을 적용한다.
- KST는 서머타임이 없으므로 UTC+9 고정 시간대로 변환한다.
- 미추적 `nohup.out`은 사용자 파일로 보고 수정하거나 커밋하지 않는다.
- `gofmt -w main.go main_test.go`를 실행했다.
- `env GOCACHE=/tmp/codex-reset-monitor-go-cache go test ./...`가 통과했다.
- `env GOCACHE=/tmp/codex-reset-monitor-go-cache go build -o /tmp/codex-reset-monitor .`가 통과했다.
- `git diff --check`가 통과했다.
