# Codex Reset Monitor

`codex-resets.com`의 공개 API를 1분마다 조회하고 새로운 리셋 또는 watch 변화를 Telegram으로 알립니다.

## 알림 조건

- 리셋 목록에서 새로운 리셋 ID가 발견된 경우.
- 새로운 `active_watch.observed_at`이 발견된 경우.
- watch 단계가 `elevated`에서 `strong`으로 상승한 경우.
- 동일 watch의 확률이 직전 상태보다 20%p 이상 상승한 경우.
- 같은 리셋에 원문 또는 관련 링크가 나중에 추가된 경우.
- 같은 리셋의 종류가 `regular`과 `banked` 사이에서 변경된 경우.

첫 실행에서는 기존 이벤트를 알리지 않고 현재 값을 `state.json`에 저장합니다. 이후 Telegram 전송이 성공한 경우에만 새 상태를 저장합니다.

공식 X 발표 없이 관측된 리셋에 링크가 없으면 2분 동안 보강을 기다립니다. 2분 이내에 링크가 생기면 링크를 포함해 알리고, 계속 없으면 링크 없음으로 알립니다. 알림 후 링크가 추가되면 기존 Telegram 메시지에 답장합니다. 발표 또는 감지 시각만 수정된 경우에는 알리지 않고 상태만 갱신합니다.

리셋 발표 시각과 watch 만료 시각은 API 원문의 UTC와 변환된 KST를 함께 표시합니다.

`state.json`은 리셋 ID별 Telegram 메시지 ID와 보강 상태를 저장하며, 알림 완료 상태는 최신 50건만 유지합니다. 아직 알리지 않은 대기 상태는 정리 대상에서 제외됩니다. 프로그램은 파일 잠금으로 같은 실행 디렉터리에서 한 인스턴스만 동작합니다.

## 실행

```bash
cp .env.example .env
# .env에 실제 봇 토큰과 채팅 ID를 입력합니다.
go run .
```

프로그램은 실행 디렉터리의 `.env`를 자동으로 읽습니다. 시스템 환경변수와 `.env`에 같은 키가 있으면 시스템 환경변수를 우선합니다. `.env`는 Git에서 제외됩니다.

바이너리는 다음 명령으로 만들 수 있습니다.

```bash
go build -o codex-reset-monitor .
./codex-reset-monitor
```

`state.json`은 실행한 디렉터리에 생성되며 Git에서 제외됩니다.

## 테스트

```bash
go test ./...
```

실제 Telegram으로 네 단계 테스트 알림을 보내려면 다음 명령을 실행합니다. 링크 있는 리셋, 링크 없는 리셋, 다음 리셋, 이전 링크 없는 리셋에 대한 답장 순서로 전송됩니다.

```bash
TELEGRAM_INTEGRATION_TEST=1 go test -run TestTelegramNotificationFlow -v
```
