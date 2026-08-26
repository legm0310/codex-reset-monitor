# Codex Reset Monitor 체크리스트

- [x] 빈 작업공간과 Git 상태를 확인한다.
- [x] Go 모듈과 실행 진입점을 만든다.
- [x] `state.json`에 마지막 리셋과 watch 상태를 저장한다.
- [x] 신규 리셋 및 watch 변화의 중복 알림을 방지한다.
- [x] Telegram Bot API로 메시지를 전송한다.
- [x] 첫 실행에서는 알림 없이 기준 상태만 저장한다.
- [x] 상태 비교와 파일 저장을 단위 테스트한다.
- [x] `gofmt`, `go test`, `go build`를 통과한다.
- [x] `.env` 자동 로딩과 환경변수 우선순위를 지원한다.
- [x] `.env.example`을 추가하고 실제 `.env`는 Git에서 제외한다.
- [x] 변경 후 테스트와 빌드를 다시 검증한다.
- [x] `feat(config): .env 설정 지원`으로 첫 커밋을 만든다.
- [x] 리셋 발표 시각을 UTC와 KST로 함께 표시한다.
- [x] watch 만료 시각을 UTC와 KST로 함께 표시한다.
- [x] 날짜 경계를 넘는 UTC→KST 변환을 테스트한다.
- [x] 테스트와 빌드를 검증하고 KST 표시 변경을 커밋한다.
