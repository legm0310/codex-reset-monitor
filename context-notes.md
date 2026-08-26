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
