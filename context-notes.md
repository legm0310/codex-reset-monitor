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

## 2026-09-05 과거 리셋 반복 알림 원인과 수정 결정

- API에서 삭제된 최신 리셋 `2095979536043401428`이 완료 상태에 남아 최신 50건 중 한 자리를 차지했다.
- API의 50번째 과거 리셋 `1986166501435711936`은 매 조회에서 신규 상태로 추가된 뒤 시각 기준 정리에서 다시 삭제되어 1분마다 반복 알림이 발생했다.
- 상태에 `last_reset_watermark_at`을 추가해 지금까지 처리한 가장 최신 발표 시각을 영구적으로 유지한다.
- 상태에 없는 API 항목이 watermark 이전이면 API 복원이나 과거 이력 변경으로 간주하고 알림 없이 `notified=true`로 등록한다.
- API가 정확히 50건을 반환했을 때만 API에 없는 완료 상태를 제거하며, 아직 알리지 않은 pending 상태는 보호한다.
- 기존 상태에는 watermark가 없으므로 저장된 리셋 중 가장 최신 `announced_at`으로 자동 마이그레이션한다.
- 사용자가 기존 바이너리를 중지했으며 수정된 바이너리 검증 전에는 다시 실행하지 않는다.
- 50건 상태에 API에서 삭제된 최신 항목 하나가 남고 API의 오래된 항목 하나가 복원되는 회귀 테스트를 추가했다.
- 복원된 과거 항목은 두 번 조회해도 알림이 없고 watermark 이후의 실제 신규 항목은 한 번 알리는 것을 검증했다.
- `env GOCACHE=/tmp/codex-reset-monitor-go-cache go test ./...`가 통과했다.
- `env GOCACHE=/tmp/codex-reset-monitor-go-cache go build -o /tmp/codex-reset-monitor .`가 통과했다.
- `env GOCACHE=/tmp/codex-reset-monitor-go-cache go vet ./...`가 통과했다.
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

## 2026-10-23 사용자 계정 리셋 조기 감지 결정

- 공식 발표보다 사용자 계정에 리셋이 먼저 반영되는 경우를 찾기 위해 로컬 `codex app-server`의 `account/rateLimits/read`를 읽는다.
- 인증 토큰은 직접 읽거나 저장하지 않고 Codex CLI가 보유한 로그인 세션만 사용한다.
- 계정 ID 원문은 저장하지 않고 SHA-256 지문만 `state.json`에 저장한다.
- app-server 조회 실패는 공개 API 감시를 중단시키지 않고 해당 주기의 계정 감지만 건너뛴다.
- 첫 조회와 계정, 플랜, 제한 ID 변경은 기준선만 저장하고 알리지 않는다.
- 제한 버킷의 primary와 secondary 중 24시간 이상이면서 가장 긴 window를 주간 제한으로 선택하고, `used_percent`가 자연 갱신 시각 5분 전보다 이른 시점에 15%p 이상 감소하면 후보로 만든다.
- 오탐을 줄이기 위해 같은 감소 상태가 다음 조회에서도 유지될 때 계정 리셋으로 확정한다.
- reset credit이 감소하면서 사용률이 낮아진 경우는 사용자가 banked reset을 소비한 것으로 보고 일반 리셋 알림을 만들지 않는다.
- reset credit 증가 자체는 banked 리셋 증거로 기록하며 banked 공식 발표와만 매핑한다.
- regular 공식 발표는 계정 감지 시각 기준 12시간 전부터 24시간 후까지의 미매핑 후보가 정확히 하나일 때만 자동 매핑한다.
- 후보가 없으면 기다리고 둘 이상이면 모호한 상태로 남겨 잘못된 자동 답장을 피한다.
- 계정 감지가 먼저면 계정 전용 문구로 알린 뒤 공식 발표 링크를 기존 메시지에 답장한다.
- 공식 발표가 먼저면 기존 발표 메시지에 사용자 계정 반영 확인을 답장한다.
- 같은 조회에서 두 증거가 처음 확인되면 중복 없이 하나의 결합 메시지를 보낸다.
- 계정 이벤트 상태도 완료 항목 최신 50건만 유지하고 미확정 후보는 정리 대상에서 제외한다.
- 새 기능은 기본 활성화하되 `CODEX_USAGE_MONITOR_ENABLED=false`로 끌 수 있고 `CODEX_BINARY`로 CLI 경로를 지정할 수 있게 한다.
- 실제 설치된 Codex CLI 0.163.0 응답에서는 `codex` 버킷의 10,080분 주간 제한이 secondary가 아니라 primary에 있었다. 특정 필드 위치를 가정하지 않고 가장 긴 주간 window를 고른다.
- banked reset 크레딧 차감과 사용률 감소가 서로 다른 조회에서 반영될 수 있으므로 차감 후 5분간 감소를 일반 리셋 후보에서 제외한다.
- 계정 감지와 공식 발표 중 후보가 둘 이상인 방향은 모두 자동 매핑하지 않는다.
- 계정 사용량 알림 직후 상태를 저장해 이후 공개 API 처리 실패가 발생해도 같은 계정 알림이 반복되지 않게 한다.
- app-server RPC 요청·주간 창 선택, 기준선·식별자 변경, 2회 확인, 자연 갱신 제외, banked 지급·사용, 양방향·동시·모호 매핑, 상태 정리·보안, 실패 격리를 각각 독립 테스트했다.
- `env GOCACHE=/tmp/codex-reset-monitor-go-cache go test ./...`가 통과했다.
- `env GOCACHE=/tmp/codex-reset-monitor-go-cache go test -race ./...`가 통과했다.
- `env GOCACHE=/tmp/codex-reset-monitor-go-cache go vet ./...`가 통과했다.
- `env GOCACHE=/tmp/codex-reset-monitor-go-cache go build -o /tmp/codex-reset-monitor .`가 통과했다.
- `CODEX_USAGE_INTEGRATION_TEST=1` 통합 테스트에서 실제 로컬 계정의 `codex` 10,080분 제한을 읽었고 Telegram 메시지는 전송하지 않았다.
- 실행용 `codex-reset-monitor` 바이너리를 최종 소스로 다시 빌드했으며 `.gitignore` 정책에 따라 커밋 대상에서는 제외한다.

## 2026-10-26 계정 리셋 누락 보완 결정

- 10월 26일 계정의 10,080분 사용량 창이 새로 시작됐지만 `account_resets` 이벤트가 생성되지 않은 사실을 확인했다.
- 당시 사용량이 50% 이상에서 초기화됐다는 사용자 확인이 있으므로 계정 알림이 발생했어야 하며 실제 누락으로 판단한다.
- 최신값만 저장하는 기존 상태로는 첫 관측, 20초 재확인, 제외 분기 중 어디에서 누락됐는지 사후 확정할 수 없다.
- 계정 사용량 조회를 codex-resets.com의 상태·목록 API 성공 여부와 분리한다.
- 15%p 이상 사용률 감소와 새 사용량 창 시작을 독립 신호로 사용한다.
- 새 사용량 창은 `resets_at - window_duration`이 현재 관측 시각 근처이고 사용률이 초기 상태에 가까우며 `resets_at`이 앞으로 이동한 경우로 판정한다.
- 자연 갱신 여부와 무관하게 실제 계정 초기화 신호가 두 번 확인되면 알린다.
- 20초 재확인에서는 `resets_at` 완전 일치를 요구하지 않고 동일한 새 사용량 창 범위인지 확인한다.
- banked 크레딧 증가는 banked 이벤트로 처리하면서 regular 신호 판정도 계속해 동시 발생을 보존한다.
- 최근 계정 관측값과 판정 결과는 민감정보 없이 최대 20건만 `state.json`에 저장한다.
- 후보 생성·확정·취소와 제외 사유를 로그에 남겨 다음 누락을 재현 가능하게 한다.
- 10월 25일 공식 발표와 10월 26일 계정 반영의 약 24시간 차이를 연결할 수 있도록 공식 발표 선행 매핑 범위를 12시간에서 36시간으로 늘린다. 같은 종류 후보가 둘 이상이면 기존처럼 자동 매핑하지 않는다.
- `55% → 0%` 감소와 새 7일 창이 함께 시작되고 재확인 중 `resets_at`이 45초 조정되는 회귀 테스트가 통과했다.
- 사용률 변화가 `0% → 0%`여도 현재 시각에 새 사용량 창이 시작되면 두 번 확인 후 알리는 테스트가 통과했다.
- 사용량이 높은 상태의 시각 이동, 현재와 무관한 창 시작 시각, 30분 미만 조정은 후보가 되지 않는 테스트가 통과했다.
- 하루 전 공식 발표의 Telegram 메시지에 계정 반영 확인을 답장하는 테스트가 통과했다.
- codex-resets.com의 상태와 목록 API가 모두 503이어도 계정 후보 확정, Telegram 전송, 상태 저장이 수행되는 테스트가 통과했다.
- 기존 형식의 pending 후보도 사용률 감소 신호로 마이그레이션해 확정할 수 있게 했다.
- `env GOCACHE=/tmp/codex-reset-monitor-go-cache go test ./...`가 통과했다.
- `env GOCACHE=/tmp/codex-reset-monitor-go-cache go test -race ./...`가 통과했다.
- `env GOCACHE=/tmp/codex-reset-monitor-go-cache go vet ./...`가 통과했다.
- `env GOCACHE=/tmp/codex-reset-monitor-go-cache go build -o /tmp/codex-reset-monitor .`가 통과했다.
- 실제 로컬 app-server 읽기 전용 통합 테스트에서 `codex`, `prolite`, 10,080분 주간 창 응답을 정상 처리했다.
- 실행용 바이너리를 갱신하고 기존 프로세스를 종료한 뒤 launchctl 제출 작업 `com.legm0310.codex-reset-monitor`로 다시 실행했다.
- launchctl의 기본 `PATH`에는 nvm의 `codex`가 없어 첫 시도가 실패했으며 실제 오류 `exec: "codex": executable file not found in $PATH`를 확인했다.
- 실행 작업에 `CODEX_BINARY=/Users/dev4/.nvm/versions/node/v22.22.0/bin/codex`를 명시해 보완했다.
- 새 프로세스 PID 50847이 실행 중이며, 재시작 후 계정 관측 이력이 2건으로 증가하고 최신 결과가 `normal`로 저장됐다.
- 재시작 전후 `account_resets`는 1건, pending 후보는 없어서 과거 이벤트 재전송이나 거짓 후보가 생성되지 않았다.
