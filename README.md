# Codex Reset Monitor

`codex-resets.com`의 공개 API를 1분마다 조회하고 새로운 리셋 또는 watch 변화를 Telegram으로 알립니다.

## 알림 조건

- 새로운 `latest_reset.id`가 발견된 경우.
- 새로운 `active_watch.observed_at`이 발견된 경우.
- watch 단계가 `elevated`에서 `strong`으로 상승한 경우.
- 동일 watch의 확률이 직전 상태보다 20%p 이상 상승한 경우.

첫 실행에서는 기존 이벤트를 알리지 않고 현재 값을 `state.json`에 저장합니다. 이후 Telegram 전송이 성공한 경우에만 새 상태를 저장합니다.

리셋 발표 시각과 watch 만료 시각은 API 원문의 UTC와 변환된 KST를 함께 표시합니다.

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
