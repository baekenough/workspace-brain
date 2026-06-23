# workspace-brain

**workspace-brain**은 Slack 같은 업무 표면에서 들어온 요청을 표면 무관 계약으로 정규화하고, 프로젝트별 데이터를 격리해 수집·검색·응답하는 **멀티테넌트 RAG 플랫폼의 Go walking skeleton**입니다.

현재 저장소는 완성형 RAG 제품이 아닙니다. 목표는 먼저 GitHub에 공개 가능한 수준의 **아키텍처 와꾸, 계약 경계, 보안/격리 테스트, 문서화된 확장 지점**을 만드는 것입니다.

## 지금 구현된 것

- Go 기반 walking skeleton
- Control Plane / Data Core 경계
- 표면 무관 Core 계약
  - `resolve_binding`
  - `create_project`
  - `ingest`
  - `get_job_status`
  - `query`
  - `discover`
  - `set_project_state`
- Gateway 계층
  - binding 해석
  - 권한 판정
  - 존재/권한 에러 정규화
  - gateway 생성 `job_id`
  - Control Plane 자체 job 상태 저장소
- Slack slash command HTTP adapter
  - Slack 서명 검증
  - `/brain create`
  - `/brain ingest`
  - `/brain ask`
  - `/brain discover`
  - `/brain status`
- In-memory Data Core
  - 테스트/데모용 tenant, binding, source, job 상태 저장
- 엄격한 테스트
  - contract validation
  - tenant isolation
  - duplicate protection
  - job lifecycle
  - callback/reconcile
  - Slack signature 검증
  - safe error message 검증
  - race test 대상 구조

## 아직 구현하지 않은 것

아래는 의도적으로 adapter 뒤로 미룬 영역입니다.

- 실제 PostgreSQL / RLS 저장소
- 실제 Qdrant collection 연동
- RabbitMQ retry / DLQ
- OpenAI embedding 호출
- 실제 RAG 합성 품질 개선
- Slack manifest 배포 자동화
- Admin UI
- 멀티호스트 Swarm 운영

현재 단계에서는 외부 의존성을 붙이기보다 **계약과 테스트를 먼저 고정**합니다.

## 빠른 시작

```bash
go test ./...
go test -race ./...
go run ./cmd/workspace-brain demo
```

데모 출력 예시:

```text
tenant=tenant-000001 job=job-000001 answer=아직 실제 RAG 합성은 연결되지 않았습니다. walking skeleton 응답입니다: workspace-brain은 무엇인가?
```

## Slack adapter 실행

실제 Slack slash command endpoint를 띄우려면 서명 secret이 필요합니다.

```bash
export SLACK_SIGNING_SECRET='...' 
export ADMIN_USERS='U123,U456'
export ADDR=':8080'
go run ./cmd/workspace-brain
```

Endpoint:

```text
POST /slack/commands
```

Slack command 기본값은 `/brain`입니다.

## 명령 모델

| 명령 | 역할 |
|---|---|
| `/brain create <name>` | 프로젝트 생성 + 현재 Slack 채널 binding 생성 |
| `/brain ingest <source-uri>` | 수집 작업 접수 |
| `/brain ask <question>` | tenant-scoped 질의 |
| `/brain discover <query>` | metadata-only 탐색 |
| `/brain status <job-id>` | 작업 상태 조회 + core reconcile |

## 저장소 구조

```text
cmd/workspace-brain/          # 실행 진입점
pkg/brainapi/                 # 공개 가능한 표면 무관 계약 타입
internal/control/gateway/     # Control Plane gateway
internal/control/jobs/        # Control Plane job 상태 저장소
internal/control/slack/       # Slack slash-command adapter
internal/core/memory/         # 테스트/데모용 in-memory Data Core
docs/                         # 세부 아키텍처 문서
```

## 테스트 정책

이 프로젝트는 “먼저 계약을 테스트로 고정한다”를 원칙으로 합니다.

권장 검증:

```bash
go test ./...
go test -race ./...
./scripts/check-coverage.sh coverage.out
```

현재 기준은 **statement coverage 100.0% 필수**입니다. 100.0% 미만이면 CI가 실패합니다.

CI에서는 다음을 실행하도록 구성했습니다.

- `go test ./...`
- `go test -race ./...`
- coverage 100.0% 강제
- `govulncheck ./...`

## 아키텍처 문서

- [ARCHITECTURE.md](./ARCHITECTURE.md) — 구현 기준 아키텍처 요약
- [docs/workspace-brain-control-plane-architecture.md](./docs/workspace-brain-control-plane-architecture.md) — Control Plane 설계
- [docs/workspace-brain-data-architecture.md](./docs/workspace-brain-data-architecture.md) — Data Core 설계

## 공개 상태

현재 공개 포지션은 다음이 적절합니다.

> A Go walking skeleton for a multi-tenant RAG control plane and data-core contract, with strict contract and isolation tests.

즉, “완성형 Slack RAG 봇”이 아니라 **계약/경계/테스트가 빡빡한 초기 아키텍처 구현체**입니다.

## 라이선스

아직 라이선스를 확정하지 않았습니다. 공개 배포 전 `LICENSE`를 명시하세요.
