# 이슈 정리 + 공용 지식 테넌트(#12) 설계 문서

- **작성일**: 2026-07-08
- **대상 리포**: workspace-brain (Go 멀티테넌트 RAG control plane)
- **관련 이슈**: #1 ~ #10 (로드맵 feature/decision, 전부 open), #12 (신규 feature: 공용 지식 테넌트)

## 1. 개요/목표

workspace-brain 리포에는 현재 열린 이슈가 11개 있다. `git log`(WB-01 ~ WB-13 커밋 시퀀스) 기준으로 확인하면 #1~#10에 대응하는 코드는 대부분 이미 구현되어 있음에도 이슈 자체는 전부 open 상태로 남아 있다. 이 문서는 두 가지 작업을 함께 정의한다.

1. **이슈 위생(hygiene) 정리**: #1~#10을 실제 코드/테스트와 대조 검증하고, 구현되지 않은 잔여 갭이 있으면 별도 이슈로 분리한 뒤 원본 이슈를 닫는다.
2. **#12 설계·구현**: 여러 프로젝트 테넌트가 공유하는 "공용 지식(shared knowledge) 테넌트" 개념을 `brainapi.Core` 계약에 도입한다. #12는 현재 코드베이스에 `SharedTenant`, `promote` 관련 개념이 전혀 없는 순수 신규 기능이다(`SharedTenant|promote|shared_tenant|SharedScope` 전체 검색 결과 실제 구현 코드에서 매치 없음 — `.claude/` 내부 파일만 매치).

목표는 (a) 이슈 트래커가 실제 코드 상태를 정확히 반영하도록 만들고, (b) strict tenant isolation이라는 이 리포의 핵심 가치를 훼손하지 않으면서 공용 지식을 어떻게 얹을지에 대한 실행 가능한 설계를 확정하는 것이다.

## 2. 현황 분석

### 2.1 이슈 상태

| 이슈 | 유형 | 코드 상태 | 근거 |
|---|---|---|---|
| #1 ~ #10 | 로드맵 feature/decision | 대부분 구현 완료로 추정 | WB-01~WB-13 커밋 히스토리(예: WB-06b RabbitMQ ingest worker, WB-08 admin read surface, WB-09b dependency compose, WB-11 backup/restore, WB-12 HEALTHCHECK)가 각 이슈의 스코프를 커버하는 것으로 보이나, **이슈별 완료 기준과 실제 코드/테스트를 1:1 대조 검증한 적은 없음** |
| #12 | 신규 feature | 미구현 | `SharedTenant`/`promote`/`shared_tenant`/`SharedScope` grep 결과 실제 소스에 없음. `pkg/brainapi/types.go`의 `Core` 인터페이스, `QueryRequest`, `DiscoverRequest`에도 공용 스코프 개념 부재 |

"대부분 구현됨"이라는 판단은 커밋 로그에 근거한 정황 증거이지 검증된 사실이 아니다. Phase A는 이 정황을 이슈별 완료 기준 체크리스트와 실제 코드 대조로 사실 검증하는 단계다.

### 2.2 현재 계약 (`pkg/brainapi/types.go`)

- `Core` 인터페이스: `ResolveBinding`, `CreateProject`, `Ingest`, `JobStatus`, `Query`, `Discover`, `SetProjectState`, `AdminList*` 5종.
- `QueryRequest{TenantID, Question, Options}` — TenantID는 단일 값이며 클라이언트가 여러 테넌트를 합집합 조회할 방법이 없다.
- `DiscoverRequest{TenantID, Query}` — 동일하게 단일 테넌트 스코프.
- `ResolveBinding(binding BindingKey) (TenantID, error)` — surface-neutral 바인딩 키(`slack:channel:C123` 등)를 단일 tenant_id로 매핑하는 유일한 preflight 예외. `internal/control/gateway/gateway.go:500`에서 호출.
- `Source{ID, Title, URI, TenantID}` — 출처는 tenant_id만 표기하고 계층(project/shared) 구분이 없음.
- Action 상수: `create_project`, `query`, `discover`, `ingest`, `status`, `admin` 6종. 공용 테넌트 승격을 위한 별도 action 없음.
- Strict tenant isolation은 `internal/core/coretest/suite.go`의 `query_tenant_isolation` 테스트로 memory/postgres 두 core 구현이 공통으로 검증받는다. 이 conformance suite가 #12 설계의 안전망이자 확장 지점이다.

## 3. 결정 요약

| 항목 | 결정 |
|---|---|
| 전체 스코프 | #1~#10 검증 후 정리 + #12 설계·구현 |
| 검증 시 갭 처리 | Phase A에서 갭을 각각 **새 GitHub 이슈로 등록**(원본 이슈 링크 포함) |
| 원본 이슈 닫는 시점 | 갭을 새 이슈로 분리한 뒤 원본 이슈는 "코어 구현됨, 잔여 갭 #NN 분리" 코멘트 후 **바로 닫기** |
| 갭 이슈 처리 | Phase B에서 Go 전문가가 코드/테스트로 보완한 뒤 닫기 |
| #12 공용 스코프 표현 | **서버 측 바인딩 리소스** (`resolve_binding` 확장 방향). `Query`/`Discover` 계약은 클라이언트가 shared 스코프를 직접 지정할 수 없게 유지하고, Core 내부에서 [프로젝트 테넌트] ∪ [바인딩된 공용 테넌트]로 스코프를 확장한다. 클라이언트 조작을 통한 격리 우회를 원천 차단한다 |
| #12 승격(promote) 절차 | 공용 테넌트 직접 쓰기(Ingest) 차단 + **관리자 승인 기반 승격 경로(전용 action/API)까지** 구현. 비식별화(de-identification) 자동화는 이번 스코프 제외 (Out of Scope) |
| 채택 접근 | 접근 1: 검증(A) → 정리(B) → #12(C) 순차. #12가 #2/#5/#8에 의존하므로 재작업 위험 최소 |

## 4. 접근 비교

### 접근 1 — 검증(A) → 정리(B) → #12(C) 순차 (채택)

Phase A에서 #1~#10 전체를 먼저 검증하고, Phase B에서 발견된 갭을 보완한 뒤, Phase C에서 #12를 설계·구현한다.

- **장점**: #12는 바인딩 리소스 확장(#2 관련), grounding 출처 표기 확장(#5 관련), admin 승인 액션(#8 admin read surface 관련)에 의존한다. 이 선행 이슈들의 실제 계약이 검증·확정된 뒤 #12를 얹으면 재작업(rework) 위험이 최소화된다.
- **단점**: 전체 리드타임이 길어짐 (A→B→C 순차).
- **채택 이유**: #12가 의존하는 기반 계약이 아직 "구현 추정" 상태이므로, 검증 없이 바로 #12를 시작하면 기반이 흔들릴 때 #12 설계까지 되돌려야 하는 위험이 크다. 안정성이 속도보다 우선.

### 접근 2 — #12 먼저 설계·구현, #1~#10 검증은 나중

- **장점**: 신규 기능(#12)이 가장 가치가 높다고 보고 먼저 착수하면 사용자 체감 임팩트가 빠르다.
- **단점**: #12가 의존하는 `ResolveBinding`/grounding/admin 계약이 실제로 완료됐는지 모르는 상태에서 그 위에 기능을 쌓게 됨. 만약 Phase A에서 사후에 갭이 발견되면 #12 코드까지 소급 수정해야 함.
- **미채택 이유**: 의존 관계의 방향(#12 → #2/#5/#8)과 검증 부재로 인한 재작업 위험이 접근 1보다 큼.

### 접근 3 — #1~#10 검증/정리와 #12 설계를 완전 병렬로 진행

- **장점**: 리드타임 최소화. Phase A(검증)와 Phase C(설계) 착수 시점이 동시.
- **단점**: #12 설계가 "아직 검증되지 않은 가정" 위에서 시작되므로, Phase A 결과가 Phase C 진행 중간에 나오면 설계를 다시 바꿔야 할 수 있음. 두 갈래 산출물의 정합성 관리 비용도 발생.
- **미채택 이유**: #12가 #2(바인딩)/#5(grounding 출처)/#8(admin 액션)에 명시적으로 의존하는 구조에서, 의존 대상이 아직 미검증인 채로 병렬 착수하면 접근 2와 동일한 재작업 위험을 안게 됨. 다만 Phase A 내부의 이슈별 도메인 분석 자체는 병렬화한다(5절 참조) — 이는 "완전 병렬"과는 다른, Phase A **내부**의 병렬 실행이다.

## 5. Phase A — 검증/갭 분석

### 5.1 목적

#1~#10 각각의 완료 기준(원 이슈 본문에 명시된 조건)을 실제 코드·테스트와 대조하여, 각 이슈가 (a) 완전히 구현되었는지, (b) 부분 구현이고 잔여 갭이 있는지, (c) 실제로는 미구현인지 판정한다.

### 5.2 실행 방식

- 이슈 10개를 도메인별로 묶어 병렬 분석한다 (R009). 예: 게이트웨이/바인딩 계열, ingest/job 계열, admin/observability 계열, ops/배포 계열 등으로 그룹핑하여 각 그룹을 담당 서브에이전트(주로 `lang-golang-expert`/`be-go-backend-expert`)에 위임.
- 각 서브에이전트는 해당 이슈의 완료 기준 체크박스를 하나씩 실제 코드 위치(`internal/`, `pkg/`, `cmd/`)와 테스트 파일에 대조하고, 충족 여부를 근거(파일:라인, 테스트 이름)와 함께 기록한다.
- 오케스트레이터(메인 대화)는 Read/Grep으로 이슈 본문과 코드 개요만 파악하고, 실제 판정 작업과 GitHub 이슈 조작은 서브에이전트에 위임한다 (R010).

### 5.3 산출물

- **이슈별 판정표**: 이슈 번호 / 판정(완전 충족·부분 충족·미충족) / 충족 근거(코드·테스트 참조) / 갭 목록.
- 갭이 하나라도 있는 이슈는 갭 항목마다 **새 GitHub 이슈**를 등록한다. 새 이슈 본문에는 원본 이슈 번호를 링크(`Related to #N`)로 포함하여 추적 가능성을 유지한다.
- 갭 분리가 끝난 원본 이슈에는 "코어 구현됨, 잔여 갭 #NN(신규 이슈 번호들) 분리" 코멘트를 남기고 **바로 닫는다**. 갭이 전혀 없는 이슈(완전 충족)는 "완료 검증됨" 코멘트 후 즉시 닫는다.
- GitHub 이슈 등록/코멘트/닫기는 `mgr-gitnerd`를 통해서만 수행한다 (R010 Delegation Rules — Git operations).

## 6. Phase B — 갭 보완

- Phase A에서 등록된 갭 이슈를 도메인별로 `lang-golang-expert`/`be-go-backend-expert`에 위임하여 코드·테스트로 보완한다. 갭 이슈 개수가 3개 이상이거나 리뷰 사이클이 필요한 경우 Agent Teams 기준(R018)을 우선 검토한다.
- 닫기 조건: 해당 갭 이슈가 요구하는 신규/수정 코드에 대한 테스트가 통과하고, 리포의 기존 커버리지 기준(100% 목표)이 유지되어야 닫을 수 있다. 테스트 없이 "코드만 추가"된 상태로는 닫지 않는다.
- 갭 보완 커밋/PR도 `mgr-gitnerd`를 통해 처리하며, 구조 변경(에이전트/스킬/가이드)이 동반되지 않는 한 R017 전체 검증 사이클은 필요하지 않다. 단, Go 코드 변경은 통상적인 lint/test 검증(R023 Tier 1~2)을 거친다.

## 7. Phase C — #12 설계·구현

### 7.1 계약 변경

- `Core` 인터페이스에 **공용 바인딩 조회 seam**을 신설한다. 이는 서버 측 개념으로, 클라이언트가 요청 페이로드에 shared 스코프를 직접 지정하는 것이 아니라, 프로젝트 테넌트에 대해 "이 프로젝트가 어떤 공용 테넌트에 바인딩되어 있는가"를 Core 내부에서 조회하는 메커니즘이다. `ResolveBinding`이 surface binding key → 단일 project tenant_id를 매핑하는 기존 preflight 예외였다면, 신규 seam은 project tenant_id → 바인딩된 공용 tenant_id 목록(0개 이상)을 반환하는 2차 조회다.
- `Query`/`Discover` 내부 처리에서 실제 조회 스코프를 [요청된 프로젝트 테넌트] ∪ [해당 프로젝트가 바인딩된 공용 테넌트]로 확장한다. `QueryRequest`/`DiscoverRequest` 자체의 `TenantID` 필드는 여전히 단일 값으로 유지하여, 클라이언트가 필드 조작만으로 임의의 다른 프로젝트 테넌트나 임의의 공용 테넌트를 지정할 수 없게 한다.
- 프로젝트 테넌트 간의 합집합(예: 프로젝트 A가 프로젝트 B 데이터를 조회)은 계속 절대 금지. 확장되는 것은 "프로젝트 ∪ 그 프로젝트에 바인딩된 공용 테넌트"뿐이며, 공용 테넌트 자체도 명시적 바인딩 관계가 있는 경우에만 스코프에 들어온다.

### 7.2 쓰기 정책

- 공용 테넌트에 대한 직접 `Ingest` 호출은 차단한다 (공용 테넌트를 대상 tenant_id로 하는 ingest 요청은 권한 오류로 거부).
- 공용 테넌트에 콘텐츠를 반영하는 유일한 경로는 **관리자 승인 기반 승격(promote) action**이다. 새 action 상수(예: `promote_to_shared`)를 `brainapi.Action`에 추가하고, `ActionAdmin`과 동일한 권한 체크 경로(관리자 principal 필요)를 거치도록 게이트웨이 authorizer를 확장한다.
- 승격 요청은 원본(프로젝트 테넌트의 특정 source/청크)을 지정하고, 관리자가 승인하면 Core가 해당 콘텐츠를 공용 테넌트로 복제한다. 승격된 콘텐츠에 대한 비식별화(de-identification) 자동 처리는 이번 스코프에 포함하지 않는다(8절 참조) — 승격 전에 콘텐츠를 정제할지 여부는 관리자의 수동 판단에 맡긴다.

### 7.3 Grounding 연결

- `Source` 구조체에 계층(tier) 필드를 추가하여 `project` 또는 `shared` 값을 표기한다. 이는 #5(grounding/출처 표기)의 확장이며, `QueryResponse.Sources`에 담긴 각 출처가 어느 계층에서 왔는지 사용자에게 투명하게 노출한다.
- 기존 `GroundedSpans`/`SupplementedSpans`/`GroundingAvailable` 필드는 변경하지 않는다. 계층 표기는 순수하게 `Source` 단위의 부가 정보다.

### 7.4 테스트

- `internal/core/coretest/suite.go`의 기존 conformance suite를 확장한다:
  - 기존 `query_tenant_isolation`(프로젝트 간 교차 차단)은 그대로 유지 — 회귀 방지.
  - 신규 케이스: 프로젝트 테넌트가 바인딩된 공용 테넌트의 콘텐츠를 함께 조회할 수 있음을 검증.
  - 신규 케이스: 바인딩되지 않은 공용 테넌트는 여전히 조회 스코프에 들어오지 않음을 검증(공용 테넌트가 여러 개 존재할 때의 교차 차단).
  - 신규 계약 테스트: 공용 테넌트에 대한 직접 `Ingest` 호출이 거부됨(승격 action 없이는 쓰기 불가)을 검증.
  - 신규 계약 테스트: 승격 action이 관리자 principal 없이는 거부됨을 검증.
- memory core와 postgres core 양쪽 구현이 동일한 conformance suite를 통과해야 하며, 리포 기존 커버리지 기준(100%)을 유지한다.

## 8. 실행 모델

- 오케스트레이터(메인 대화)는 Read/Grep으로만 이슈 본문과 코드 구조를 파악하고, 직접 파일 수정이나 git 조작을 하지 않는다 (R010).
- 코드 작성·판정·테스트 보완은 `lang-golang-expert` 또는 `be-go-backend-expert`에 위임한다.
- 이슈 등록/코멘트/닫기, 커밋, PR, 푸시는 전부 `mgr-gitnerd`에 위임한다.
- Phase A의 이슈별 도메인 분석처럼 독립적인 작업 단위가 2개 이상이면 병렬 에이전트 실행을 우선한다 (R009). Agent Teams가 활성화되어 있고 3개 이상의 에이전트 또는 리뷰 사이클이 필요한 배치라면 R018 기준에 따라 Agent Teams를 사용한다.
- 커밋·푸시 전에는 구조 변경(에이전트/스킬/가이드/규칙) 여부와 무관하게, 이 계획 자체가 다수의 이슈 조작과 코드 변경을 포함하는 다단계 작업이므로 `mgr-sauron` 구조 검증(R017)을 거친 뒤에만 push한다. 특히 Phase C에서 `pkg/brainapi/types.go` 계약이 변경되므로, 계약 변경이 다른 문서(`ARCHITECTURE.md`, `docs/workspace-brain-data-architecture.md`, `docs/workspace-brain-control-plane-architecture.md`)와 어긋나지 않는지 확인이 필요하다.

## 9. 완료 기준 (전체)

- **이슈 정리**: #1~#10 전부가 닫혀 있고, 각각 "완전 충족" 또는 "갭 이슈 #NN으로 분리" 코멘트를 갖는다.
- **갭 이슈**: Phase A에서 등록된 모든 갭 이슈가 코드·테스트로 보완되어 닫혀 있다.
- **#12 완료 기준** (4항목):
  1. 프로젝트 테넌트 간 격리가 계속 유지된다 (교차 조회 불가 — 회귀 테스트 통과).
  2. 프로젝트 테넌트 조회 시 바인딩된 공용 테넌트의 콘텐츠가 함께 조회된다.
  3. 공용 테넌트에 대한 직접 쓰기(Ingest)는 차단되고, 관리자 승인 기반 승격 action을 통해서만 콘텐츠가 반영된다.
  4. Query 응답의 출처(`Source`)에 계층(project/shared) 표기가 포함된다.
- **커버리지**: 위 변경 전체에 걸쳐 리포의 기존 테스트 커버리지 기준(100%)이 유지된다.

## 10. 범위 외 (Out of Scope)

- **비식별화(de-identification) 자동화 파이프라인**: 프로젝트 콘텐츠를 공용 테넌트로 승격하기 전에 개인정보·민감정보를 자동으로 탐지·마스킹하는 기능은 이번 스코프에 포함하지 않는다. 현재는 관리자의 수동 판단에 의존하며, 별도 이슈 후보로 남긴다.
