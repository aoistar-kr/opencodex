# enter-force-submit — post-submit clear + 구조화 usage 트리거 구현 계획

상태: 확정(구현 전). 산출물: 이 문서 1건. 코드/빌드/설치/커밋 없음.

## 1. 목표

두 가지를 추가한다.

1. **post-submit clear** — `--ocx-force-submit`가 성공을 보고한 뒤 composer에 남아 있는 초안을
   결정적으로 비운다. 사용량 한도로 앱이 전송을 막은 상태에서 브리지가 대신 턴을 시작하면,
   앱이 입력창을 스스로 비우지 않는 경우가 있어 같은 초안이 남아 재제출 위험이 된다.
2. **structured usage trigger** — 지금처럼 화면 문자열(limit 문구)을 추측하지 않고, app-server가
   주는 **구조화된 usage 신호**(중첩 `usage.limitState`가 정확히 `confirmed`)를 트리거로 삼는다.
   확정 신호가 없으면(부재/`advisory`/`unknown`/비JSON/커스텀 provider) 기존 UI 문구 매칭으로
   폴백한다.

## 2. 비목표 (명시적 제외)

- `app.asar`, Codex JS 번들, 앱 설정 파일 수정 없음.
- 클립보드 사용 없음(복사/붙여넣기로 비우지 않는다).
- UIA `ValuePattern.SetValue` 등 **값 직접 쓰기 없음**.
- 기존 routing/브리지 규약 변경 없음. 새 명령은 가산적(additive)으로만 추가한다.

## 3. 공식 레퍼런스

- app-server: https://learn.chatgpt.com/docs/app-server
- account 스키마(v2): https://github.com/openai/codex/blob/main/codex-rs/app-server-protocol/src/protocol/v2/account.rs

## 4. 계약: 구조화 usage 필드 중첩

파서는 중첩 경로 하나만 읽는다.

- 응답 → 최상위 `usage` 객체가 1차 컨테이너.
- 하드 블록 확정 필드는 그 안의 `limitState` 하나이며, 값이 정확히 `confirmed`일 때만 확정한다.
- 최상위 `limitState` 등 다른 위치의 값은 읽지 않는다(중첩 필수).
- `rateLimits`/`usedPercent` 같은 비율 필드는 판정에 쓰지 않는다(제거됨).
- 객체가 없거나 값이 `advisory`/`unknown`/그 밖의 문자열이면 확정으로 승격하지 않고
  **불명(unknown)** 으로 두고 기존 UIA 스캔으로 폴백한다.

## 5. Signal / State / Lifecycle / TTL / Scope / custom provider fail-open

- **Signal**: bridge `--ocx-status` 응답의 중첩 `usage.limitState`를 1차 신호로 삼는다. 트리거 조건은
  “`limitState`가 정확히 `confirmed` + 전송 컨트롤이 실제로 비활성”의 교집합이며, 확정 신호가 없으면
  기존 UIA 문구 스캔으로 폴백한다.
- **State**: helper는 작은 상태기계를 유지한다. `idle → armed → submitting → clearing → verifying → cooldown`.
  각 전이는 관측된 결과로만 진행하고, 애매하면 `idle`로 되돌린다.
- **Lifecycle**: 폴링 → 신호 판정 → (필요 시) 브리지 제출 → post-submit clear → 잔여 검증 → 쿨다운.
  한 사이클 동안 다른 초안에 대해 재진입하지 않는다.
- **TTL**: 사용량 스냅샷은 유효기간을 둔다. TTL이 지난 값으로는 트리거하지 않는다. TTL과 기존
  “캐시 스냅샷 600ms / 디바운스” 규칙은 같은 방향(오래된 근거로 행동 금지)을 따른다.
- **Scope**: 대상 프로세스·포그라운드 창·composer 신원이 모두 일치할 때만 동작한다. 전역 키 입력
  후킹은 관찰만 하고, clear/제출은 스코프 일치 시에만 실행한다.
- **custom provider fail-open**: 사용량 소스가 커스텀 provider이거나 스키마가 불명이면 **아무 것도
  하지 않는다**(fail-open = 행동하지 않음). 구조화 신호를 못 믿으면 UI 추측으로 승격하지 않는다.

## 6. bridge / helper 변경 위치

- **bridge**: `native/codex-appserver-bridge/main.go`
  - 인자 디스패치(`--ocx-status`, `--ocx-force-submit`)에 구조화 usage 조회 명령을 가산적으로 추가.
  - app-server 응답에서 중첩 `usage.limitState`만 그대로 노출한다. 소켓/스레드 소유 규약은 그대로 유지.
- **helper**: `native/windows/enter-force-submit/EnterForceSubmit.cs`
  - 상태 폴링부(`--ocx-status` 호출, ≈396행)에서 새 usage 신호를 함께 읽는다.
  - 제출부(`--ocx-force-submit` 호출, ≈414행) 성공 직후 `clearing` 단계를 삽입한다.
  - 중복 억제 가드(성공 시에만 초안 기록, 비우면 해제)와 일관되게 clear 성공도 기록한다.
- **계약 문서**: 두 README(`native/codex-appserver-bridge/README.md`,
  `native/windows/enter-force-submit/README.md`)에 새 명령과 clear 단계를 반영한다.

## 7. SendInput clear guards / MTA / verify

clear는 실제 키 입력(**SendInput**)으로만 수행한다.

- **MTA**: UIA 접근 스레드는 `COINIT_MULTITHREADED`(MTA)로 유지한다. SendInput과 UIA 호출을 같은
  규율로 다룬다.
- **guards** (하나라도 어긋나면 clear를 건너뛴다):
  1. 대상 포그라운드 창·프로세스가 여전히 유효.
  2. 포커스된 UIA 요소가 여전히 Edit(composer).
  3. composer 초안이 현재 초안과 **정확히 일치**(다른 내용을 지우지 않음).
  4. 브리지 제출이 성공으로 기록됨.
  5. 모디파이어가 눌려 있지 않음.
  - 조건이 깨지면 clear를 하지 않고 `idle`로 복귀한다(잘못 지우기보다 안 지우는 쪽).
- **final recheck**: 키 입력 직전 창에서 composer 상태를 **한 번 더** resolve해 전체 신원(창·프로세스·
  composer runtime id·task fingerprint)과 **raw 초안 exact match**를 요구하고, 이어서 포커스·포그라운드·
  모디파이어를 재확인한다. 재포커스(SetFocus)는 하지 않는다. 어긋나면 clear를 건너뛴다.
- **동작**: composer로 포커스 확인 → 전체 선택(`Ctrl+A`) → 삭제(`Delete`)를 **태그된 단일 SendInput 배치**로
  전송. 클립보드·SetValue는 쓰지 않는다.
- **partial 배치**: 배치 이벤트가 전부 수락된 경우에만 성공으로 본다. 일부만 수락되면 Ctrl+A/Delete를
  **재시도하지 않고**, 태그된 best-effort 보상 배치로 `Ctrl`/`A`/`Delete` up만 풀어 키가 눌린 채 남지
  않게 한다.
- **verify**: 전송 후 즉시 fresh 스냅샷으로 초안이 비었는지 확인한다. 잔여 텍스트가 있으면 clear를
  반복하지 않고 로그만 남기고 멈춘다(과잉 입력 방지). 비었으면 중복 억제 가드를 해제한다.
- **guard 해제**: clear의 가드 해제는 **compare-and-clear**다. 현재 기록된 초안이 이 clear job이 제출한
  초안과 Ordinal로 같을 때만 원자적으로 비운다(늦게 기록된 새 가드를 지우지 않음).

## 8. 테스트와 실제 소진 한계

- **테스트**: `--dry-run`/`--probe`로 관찰만 하는 경로를 사용해 (a) 구조화 신호 파싱(중첩/불명 케이스),
  (b) clear 가드 통과/차단 표, (c) clear 후 verify 결과를 확인한다. 실 제출·실 clear는 dry-run 경로로
  검증하고, 라이브 소진은 유도하지 않는다.
- **실제 소진 한계**: 실제 사용량 소진 상태는 재현·예약이 불가능하고 비용이 든다. 따라서
  - 구조화 신호 경로는 저장된/모의 응답으로 검증하고,
  - 하드 블록 실측은 “소진이 실제로 발생했을 때” 한정 관찰로만 남긴다.
  - 소진을 인위적으로 만들기 위한 대량 호출은 하지 않는다(한계이자 제약).

## 9. 점수 이력

62 → 85 → 90 → 96 → 100
