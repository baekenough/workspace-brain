# workspace-brain

**workspace-brain**은 Slack 같은 업무 도구에서 흩어지는 맥락을 프로젝트 단위로 모으고, 안전하게 검색하고, 근거와 함께 다시 꺼내 쓰기 위한 **멀티테넌트 RAG 플랫폼의 실행 가능한 Go 골격**입니다.

이 저장소는 완성된 챗봇이 아닙니다. 먼저 단단한 뼈대를 세웁니다. 표면은 바뀌어도 흔들리지 않는 계약, 테넌트 격리, 안전한 에러 처리, 엄격한 테스트를 고정하고 그 위에 실제 저장소·큐·임베딩·검색 어댑터를 붙이는 방식으로 성장합니다.

## 무엇을 지향하나

- **업무 표면과 데이터 코어를 분리합니다.** Slack, Web, CLI가 늘어나도 Core 계약은 그대로 유지합니다.
- **프로젝트별 경계를 먼저 지킵니다.** 모든 요청은 `tenant_id`로 정규화되고, Core는 검증된 테넌트 범위 안에서만 동작합니다.
- **비동기 작업의 소유권을 명확히 둡니다.** Gateway가 `job_id`를 만들고, Control Plane이 사용자에게 보여 줄 상태를 책임집니다.
- **테스트가 설계를 잠급니다.** 공개 저장소에서 자신 있게 확장할 수 있도록 statement coverage 100.0%를 CI 기준으로 둡니다.

## 지금 들어 있는 것

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
  - Gateway 생성 `job_id`
  - Control Plane 자체 job 상태 저장소
- Slack slash-command HTTP adapter
  - Slack 서명 검증
  - `/brain create`
  - `/brain ingest`
  - `/brain ask`
  - `/brain discover`
  - `/brain status`
- In-memory Data Core
  - 테스트와 데모를 위한 tenant, binding, source, job 상태 저장
- 엄격한 테스트
  - contract validation
  - tenant isolation
  - duplicate protection
  - job lifecycle
  - callback/reconcile
  - Slack signature 검증
  - safe error message 검증
  - race test 대상 구조

## 아직 붙이지 않은 것

아래 영역은 의도적으로 interface 뒤로 미뤘습니다. 지금 단계의 목표는 외부 의존성보다 **계약과 경계**를 먼저 안정화하는 것입니다.

- PostgreSQL + RLS 기반 영속 저장소
- Qdrant tenant collection 연동
- RabbitMQ retry / DLQ 기반 수집 큐
- OpenAI `text-embedding-3-large` embedding adapter
- 하이브리드 검색, rerank, grounding, 출처 표시
- Slack manifest-as-code와 interactivity
- Admin surface
- 운영 관측, 감사, 백업 정책

후속 작업은 GitHub Issues에서 관리합니다.

## 다음에 붙일 것들

- [#1 PostgreSQL + RLS 영속 저장소 adapter 붙이기](https://github.com/baekenough/workspace-brain/issues/1)
- [#2 Qdrant tenant collection 기반 vector store adapter 붙이기](https://github.com/baekenough/workspace-brain/issues/2)
- [#3 RabbitMQ ingest worker, retry, DLQ 설계와 구현](https://github.com/baekenough/workspace-brain/issues/3)
- [#4 OpenAI embedding adapter와 deterministic test seam 추가](https://github.com/baekenough/workspace-brain/issues/4)
- [#5 Hybrid retrieval, rerank, grounded answer synthesis 구현](https://github.com/baekenough/workspace-brain/issues/5)
- [#6 Slack manifest-as-code와 interactivity endpoint 추가](https://github.com/baekenough/workspace-brain/issues/6)
- [#7 최소 Admin surface와 운영 제어 설계](https://github.com/baekenough/workspace-brain/issues/7)
- [#8 생성 권한, 멤버십 TTL, 감사 정책 확정](https://github.com/baekenough/workspace-brain/issues/8)
- [#9 운영 관측, 백업, 로컬 의존 서비스 구성](https://github.com/baekenough/workspace-brain/issues/9)
- [#10 LICENSE와 기여 안내 정리](https://github.com/baekenough/workspace-brain/issues/10)

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
| `/brain status <job-id>` | 작업 상태 조회 + Core reconcile |

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

이 프로젝트는 “계약을 먼저 테스트로 고정한다”를 원칙으로 합니다.

권장 검증:

```bash
go test ./...
go test -race ./...
./scripts/check-coverage.sh coverage.out
```

현재 기준은 **statement coverage 100.0% 필수**입니다. 100.0% 미만이면 CI가 실패합니다.

CI는 다음을 실행합니다.

- `go test ./...`
- `go test -race ./...`
- coverage 100.0% 강제
- `govulncheck ./...`

## 아키텍처 문서

- [ARCHITECTURE.md](./ARCHITECTURE.md) — 구현 기준 아키텍처 요약
- [docs/workspace-brain-control-plane-architecture.md](./docs/workspace-brain-control-plane-architecture.md) — Control Plane 설계
- [docs/workspace-brain-data-architecture.md](./docs/workspace-brain-data-architecture.md) — Data Core 설계

## 공개 포지션

이 저장소를 가장 정확히 소개하면 다음과 같습니다.

> A Go walking skeleton for a multi-tenant RAG control plane and data-core contract, with strict contract and isolation tests.

즉, **완성형 Slack RAG 봇**이 아니라 **계약, 경계, 격리, 테스트를 먼저 고정한 초기 아키텍처 구현체**입니다. 실제 저장소와 모델 연동은 이 골격 위에 단계적으로 붙입니다.

## 라이선스

아직 라이선스를 확정하지 않았습니다. 공개 배포 전 `LICENSE`를 명시하세요.
