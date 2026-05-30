# ONP — On-Prem Node Provisioner

이 파일은 Claude Code 가 이 저장소에서 작업할 때 자동으로 로드되는 가이드입니다. 디자인 doc 전체(`docs/DESIGN.md`)를 읽기 전에 이 파일이 프로젝트의 핵심 의도와 제약을 빠르게 알려줍니다.

## 한 줄 정의

**ONP**: pending 파드의 spec 을 보고 적합한 on-prem 물리 노드를 Wake-on-LAN 으로 깨우고, 비면 안전하게 drain 후 전원을 끄는 Kubernetes 컨트롤러. 전원 제어는 pluggable (Phase 1 = WoL, 이후 IPMI/Redfish 확장).

## 왜 만드는가 (차별점)

- **Workload-aware proactive** (사용률 reactive 아님): pending 파드 spec 으로 어느 노드를 깨울지 결정.
- **선언적 CRD 모델** (정적 config 아님): `NodePool` (정책) + `Machine` (개별 물리 노드).
- **Pluggable Power Provider** (WoL 종속 아님): `PowerProvider` 인터페이스 + Capabilities 패턴.
- **on-prem 물리 노드 전용** (클라우드 ephemeral 아님): 노드는 long-lived, 전원을 켜고 끈다.

## 스코프 — 이건 한다 / 이건 안 한다

### 이건 한다 (Phase 1)

- pending pod → fit-check → 적합한 Machine 의 PowerOn
- 빈 노드 → cordon → drain (PDB 존중) → poweroff
- `NodePool` / `Machine` CRD 로 선언적 관리
- WoL provider 구현 + `PowerProvider` 인터페이스 정의

### 이건 안 한다 (Non-Goals — 코드 작성 시 자주 헷갈리는 영역)

- **OS / kubelet 설치**, PXE, cloud-init — 운영자 책임. ONP 는 "이미 join 된 노드의 전원만" 다룬다.
- **신규 노드 cluster join** — 단순 재기동만.
- **노드 헬스/자가 치유** — Node Problem Detector 영역.
- **Pod 스케줄링 자체** — kube-scheduler 가 함. ONP 는 fit 시뮬레이션만.
- **Consolidation / bin-packing** — Phase 1 은 `WhenEmpty` 만. `WhenUnderutilized` 는 Phase 2.
- **IPMI/Redfish 구현** — Phase 1 은 인터페이스만, 실제 구현 없음.
- **멀티 클러스터/멀티 테넌시** — 단일 클러스터 가정.

## 아키텍처 핵심 결정 (어기지 말 것)

### 컴포넌트는 세 개로 쪼갠다

| 컴포넌트 | 배포 | 권한 |
|---|---|---|
| `onp-controller` | Deployment, leader-elected (Lease) | RBAC 최소: Pod/Node read, eviction, NodePool/Machine RW |
| `onp-wol-agent` | DaemonSet, `hostNetwork: true` | 항상 켜진 노드(컨트롤 플레인)에만 배치 |
| `onp-shutdown-agent` | DaemonSet, `privileged: true` | 관리 대상 노드(=ONP 가 켜고 끄는 노드)에만 배치 |

**왜 셋인가**: (a) WoL 매직 패킷은 L2 브로드캐스트 → 호스트 네트워크 agent 필요. (b) 노드 전원을 끄려면 노드 위에서 명령 실행 필요. 합쳐서 한 바이너리로 하면 보안 표면이 더 나빠진다.

### CRD 두 개

- `NodePool` (정책): minNodes/maxNodes, machineSelector(label), template(labels/taints), disruption(`WhenEmpty` only), cooldown, drain(timeout, force).
- `Machine` (개별 물리 노드): nodeName(=Node.name, Phase 1), capacity(spec에 둠 — 꺼진 상태 fit check 용), power.provider + provider별 config, status.state (`Off|Booting|Ready|Draining|ShuttingDown|Failed`).

### 상태 머신의 source of truth 는 `Machine.status` (CRD)

- 컨트롤러 ↔ shutdown-agent 사이는 직접 RPC 가 아니라 CRD watch 로 조율.
- 이유: 컨트롤러 재시작 후 idempotent reconcile, 별도 통신 채널 불필요.
- 결과: 모든 상태 전이는 `Machine.status.state` 갱신을 거친다.

### Power Provider 는 Capabilities 패턴

- 인터페이스: `PowerOn / PowerOff / PowerStatus / Capabilities()`.
- 호출 측은 `provider.Capabilities().CanPowerOff` 가드 후 분기.
- WoL 은 `{CanPowerOn: true, 나머지 false}`.
- **Phase 1 의 끄기 경로는 항상 shutdown-agent** — `provider.PowerOff` 는 호출하지 않는다 (hard-cut 시나리오용으로 자리만 둠).

## 코드 작성 시 지켜야 할 제약

### 안전성 (이건 절대)

- **"조용히 데이터를 잃는 기본값은 만들지 않는다"** — drain force 는 명시적 opt-in, 모호한 성공은 항상 `Failed` 로 옮긴다.
- **PDB / Eviction API** 사용 — 직접 Pod delete 금지.
- **`minNodes` 하한 / `maxConcurrent` / `do-not-disrupt` 어노테이션** 항상 존중.
- **RBAC 최소 권한** — 각 컴포넌트가 자기 일에 필요한 최소치만.

### 인터페이스 안정성

- `PowerProvider` 인터페이스와 `NodePool` / `Machine` CRD 스키마는 **Phase 2/3 확장 시에도 안 깨져야** 함.
- IPMI/Redfish provider 추가 = 새 구현체 등록만으로 끝나야 함.
- `WhenUnderutilized` 추가 = `disruption.consolidationPolicy` enum 확장만으로 끝나야 함.
- 깨질 것 같으면 Phase 1 단계에서 미리 조정 (이 doc 의 API sketch 가 검증의 출발점).

### 관측성

- `/metrics` 노출 (Prometheus 호환). 메트릭 이름은 `onp_` 접두사.
- 핵심 메트릭: `onp_nodes_total{pool,state}`, `onp_scale_up_latency_seconds`, `onp_power_on_total{provider,result}`, `onp_drain_failure_total{reason}`.
- Kubernetes Events 적극 발행 — 운영자가 `kubectl describe machine` 으로 흐름을 따라갈 수 있어야 함.
- Status Conditions 표준 패턴 (`type`, `status`, `lastTransitionTime`, `reason`).

## 기술 스택 / 컨벤션

- **언어**: Go
- **표준 라이브러리 우선**, 의존성은 가볍게. controller-runtime / client-go 정도가 기본.
- **Go 스타일**: Effective Go / Zen of Go 엄격 준수 — 표준 라이브러리 우선, 명확함 우선.
- **CRD 그룹**: `onp.io/v1alpha1`
- **다이어그램**: 마크다운에서는 mermaid 사용 (ASCII 아트 금지).

## 더 깊이 알고 싶으면

- `docs/DESIGN.md` — 전체 설계 문서 (Google 스타일, 트레이드오프 중심).
- `ROADMAP.md` — Phase 1~3 작업 마일스톤.
- 디자인 doc 권위 출처: https://www.industrialempathy.com/posts/design-docs-at-google/
- 선행 작업 참고: [Karpenter](https://karpenter.sh/)
