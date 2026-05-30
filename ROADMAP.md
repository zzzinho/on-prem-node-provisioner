# ONP — Roadmap

**ONP (On-Prem Node Provisioner)** — pending 파드의 spec 을 보고 적합한 on-prem 물리 노드를 Wake-on-LAN 으로 깨우고, 비면 안전하게 drain 후 전원을 끄는 Kubernetes 컨트롤러. 전원 제어는 pluggable (Phase 1 = WoL, 이후 IPMI/Redfish 확장 예정).

> 자세한 설계는 [`docs/DESIGN.md`](docs/DESIGN.md), 코드 작성 가이드는 [`CLAUDE.md`](CLAUDE.md) 참조.

---

## 작업 스타일

**에자일 / 수직 슬라이스 (walking skeleton)**.

- Phase 1 은 5 개의 milestone (M1~M5) 로 쪼갠다. 각 milestone 은 **end-to-end 로 동작하는 가장 작은 단위**.
- 매 milestone 끝에 **데모 가능한 상태**가 남는다 — "이만큼은 실제로 작동한다".
- horizontal layer 를 모두 쌓아두고 마지막에 연결하는 방식 (CRD → controller → wake → ...) 은 피한다. 대신 첫 milestone 부터 진짜 노드를 깨운다.
- milestone 안의 task 순서/세부는 가변. 진행하며 발견되는 것에 맞춰 조정.

---

## Design Defaults (Phase 1 합의)

- 노드는 이미 join 된 상태로 단순 재기동 (OS/kubelet 은 운영자 책임)
- Node 객체는 NotReady 로 유지 (삭제하지 않음)
- 컨트롤러/agent 는 컨트롤 플레인 노드(항상 켜짐)에 호스팅 — toleration 명시
- `/metrics` 노출만 제공, Prometheus 설치는 사용자 몫
- `Machine.name` = `Node.name` (Phase 1 단순화)
- Drain 기본값: `force: false` — timeout 시 멈춤 + Failed 전이
- Leader election: `coordination.k8s.io/Lease`

---

## Phase 0 — 설계 ✅

- [x] 프로젝트 이름 확정 (`wolscaler` → `ONP`)
- [x] 디렉터리 rename (`/Users/jeongjinho/code/wolscaler` → `/Users/jeongjinho/code/onp`)
- [x] `docs/DESIGN.md` — Google 스타일 디자인 doc (5 섹션, mermaid 5개)
- [x] `CLAUDE.md` — 코드 작성 시 자동 로드되는 한 페이지 가이드
- [x] TODO 를 수직 슬라이스 구조로 재구성

---

## Phase 1 — MVP (M1 → M5)

각 milestone 의 **Definition of Done** 은 "이걸 사람한테 보여줄 수 있다" 수준의 데모 가능 상태.

---

### M1 — Walking Skeleton: WoL 자체가 동작함을 증명

**Definition of Done**: 작은 CLI 바이너리로 `<MAC>` 을 받아 매직 패킷을 보내, **실제 물리 노드가 부팅된다**. Kubernetes 도, CRD 도, controller 도 없음.

**왜 이걸 먼저?** — 전체 시스템의 가장 기반 가정 "WoL 이 이 네트워크에서 진짜로 동작한다"를 가장 싸게 검증. 나중에 controller / agent 로 감싸도 이 한 줄이 작동 안 하면 전부 무의미.

- [x] `git init` (사용자가 직접)
- [x] `.gitignore`
- [x] `go mod init` → 모듈 경로 `github.com/zzzinho/on-prem-node-provisioner` (레포명과 일치, 코드 브랜드는 `onp` 유지)
- [x] `LICENSE` (Apache 2.0), 최소 `README` 뼈대
- [x] `internal/power/wol/magic.go` — `BuildPacket` + `Send` (SO_BROADCAST 설정 포함, 표준 라이브러리만)
- [x] `internal/power/wol/magic_test.go` — 테이블 기반 6 케이스, `go test -race` 통과
- [x] `cmd/wol-probe/main.go` — CLI: `wol-probe <mac> [broadcast]`
- [x] `go build / vet / test / gofmt` 모두 통과
- [x] **검증 완료** (2026-05-30): 같은 L2 세그먼트의 항상-켜진 노드에 `hostNetwork` 파드로 `wol-probe` 를 띄워, 꺼져 있던 타깃 노드를 매직 패킷으로 실제로 깨움.
  - 타임라인(상대): 송신 → ping 응답 +41s → 노드 `Ready=True` +75s.
  - 배포 메모: 클러스터에 레지스트리가 없어 이미지 pull 대신 `kubectl cp` 로 바이너리 주입 (Docker 이미지 빌드/스모크는 별도로 검증 완료). `onp-wol-agent`(M2)가 같은 코드를 hostNetwork DaemonSet 로 감싸면 동일 경로가 재현됨 — 라우팅 경계 너머로는 L2 broadcast 가 안 넘는 게 정확히 이 분리 이유.

---

### M2 — 최소 컨트롤러: 선언적으로 노드를 깨움 — `DESIGN.md` 3.1, 3.2, 3.4

**Definition of Done**: `kubectl apply -f machine.yaml` 한 뒤 `kubectl annotate machine/<node> onp.io/wake-now=true` 하면 노드가 깨어나고, `Machine.status.state` 가 `Off → Booting → Ready` 로 자동 전이된다. pending pod 감지는 아직 없음 — 트리거는 어노테이션.

**왜 이걸?** — controller / agent / CRD / power provider / 상태 머신을 한 번에 통합. 자동화는 다음 milestone 의 일. **M1 의 `internal/power/wol` 코드를 agent 가 그대로 재사용**한다.

**기반 결정** (M2 착수 시 확정):

- 스캐폴딩: **controller-gen + controller-runtime 직접 레이아웃** (kubebuilder full init 아님 — 멀티 바이너리·기존 파일 보존, 의존성 가볍게).
- 컨트롤러 → wol-agent 전송: **HTTP/JSON (표준 라이브러리)**. `PowerProvider` 구현 내부에 숨겨 추후 교체 가능 (`DESIGN.md` 3.3 갱신됨).
- 이미지 배포: **`ghcr.io/zzzinho/...` 퍼블릭 이미지** (클러스터가 그냥 pull).
- 배포 방식: **Helm 차트 (`charts/onp/`)** — M2 부터 도입하고 마일스톤마다 컴포넌트를 더한다 (M4 shutdown-agent, M5 PSA·packaging). 생 manifest 는 두지 않는다.
- 검증 대상: **실제 microk8s + `desktop1`** (kind 아님).
- `bootTimeout`: 컨트롤러 플래그 `--boot-timeout=10m` 상수. NodePool 이동은 M3.
- Leader election: M2 는 1 replica, Lease 는 M5.

각 서브슬라이스 끝에 **관찰 가능한 체크포인트**가 남는다.

#### M2.0 — 스캐폴드

- [x] `api/v1alpha1/` — `groupversion_info.go` (`onp.io/v1alpha1`) + `machine_types.go` (`nodeName`, `capacity`, `power.provider`, `power.wol.{macAddress, broadcastAddress}`, `status.state`, `status.conditions`)
- [x] controller-gen 도입 — deepcopy + CRD yaml 생성 (`make generate` / `make manifests` 류 타깃)
- [x] `cmd/onp-controller/main.go` — 빈 manager (scheme 등록, CRD 설치 확인만)
- [x] ✅ 체크포인트: `kubectl apply machine.yaml` → `kubectl get machine` 동작

#### M2.1 — provider + agent (M1 재사용)

- [x] `internal/power/provider.go` — `PowerProvider` 인터페이스 + `Capabilities` + registry + `ErrUnsupported`
- [x] `internal/power/wol/{wire.go, client.go}` — **순수(k8s-free)** wire 타입 + agent HTTP 클라이언트
- [x] `internal/power/wolprovider.go` — `*Machine` 결합 wol provider, capabilities `{CanPowerOn: true}` (agent 경량화 위해 순수 `wol` 패키지와 분리 — ROADMAP 원안의 `wol/provider.go` 에서 변경)
- [x] `internal/agent/server.go` + `cmd/onp-wol-agent/main.go` — hostNetwork HTTP `POST /wake` → `wol.Send`(M1), slog 로깅, **k8s 의존성 0**
- [x] ✅ 체크포인트: `desktop` 노드에서 agent 가 실제 LAN broadcast 송신 (204 + JSON 로그) 검증 완료

#### M2.2 — reconciler wake 경로

- [ ] `internal/controller/machine_controller.go`:
  - 어노테이션 `onp.io/wake-now=true` + `state=Off` → `provider.PowerOn` → `state=Booting`
  - Node watch (`EnqueueRequestsFromMapFunc`) → 대상 Node Ready → `state=Ready` + 어노테이션 제거
  - `bootTimeout` 경과(Ready 미관찰) → `state=Failed` + Event
  - 컨트롤러 재시작에도 idempotent (source of truth = `status.state`)
- [ ] `internal/controller/machine_controller_test.go` — envtest
- [ ] ✅ 체크포인트: in-cluster 에서 어노테이션 한 줄에 상태 전이

#### M2.3 — Helm 차트 + 배포 + E2E

- [ ] `charts/onp/` Helm 차트:
  - `crds/onp.io_machines.yaml` — controller-gen 생성 CRD (Helm `crds/` 규약)
  - `templates/` — controller Deployment, wol-agent DaemonSet(`hostNetwork: true`, 컨트롤 플레인 nodeSelector) + Service, RBAC(controller: Machine RW/status·Node read·Events; agent: API 권한 없음) + ServiceAccount
  - `values.yaml` — 이미지 repo/tag, nodeSelector/toleration, `bootTimeout`
- [ ] `make` 타깃으로 controller-gen 산출물(CRD)을 차트 `crds/` 로 동기화
- [ ] ghcr 이미지 빌드/푸시 (M1 Dockerfile 확장, multi-stage)
- [ ] `examples/machine.yaml` — 샘플 Machine (차트 밖)
- [ ] ✅ **검증 (E2E)**: `helm install onp ./charts/onp` → microk8s 배포 → `desktop1` Machine 등록 → 어노테이션으로 깨움 → `Off → Booting → Ready` 관찰

---

### M3 — Pending Pod 자동 wake: 첫 자동 스케일 업 — `DESIGN.md` 3.3 (Scale-up)

**Definition of Done**: 풀의 모든 노드를 꺼둔 상태에서 Pod 을 apply 하면, 적합한 Machine 이 **자동으로** 깨어나고 Pod 가 스케줄된다. 어노테이션 불필요.

- [ ] `api/v1alpha1/nodepool_types.go` — 최소 필드 (`minNodes`, `maxNodes`, `machineSelector`, `template`, `cooldown.scaleUp`)
- [ ] NodePool reconciler — pool 멤버십 갱신, `status.totalMachines`
- [ ] Pod watcher — `PodScheduled=False, Reason=Unschedulable` 만 큐잉
- [ ] Fit checker (`internal/scheduler/fit.go`) — 리소스 + nodeSelector + tolerations + required nodeAffinity. kube-scheduler framework 의존 X.
- [ ] 후보 Machine 선정 로직: off 상태 + pool 멤버 + fit pass → best-fit (가장 작은 capacity 우선)
- [ ] `maxNodes`, `cooldown.scaleUp` 적용
- [ ] **검증 (E2E #1)**: 풀의 모든 노드 끄기 → `kubectl run pod ...` → 자동 wake + 스케줄 확인

---

### M4 — Safe Shutdown: 첫 자동 스케일 다운 — `DESIGN.md` 3.3 (Scale-down), 5.DisruptionSafety

**Definition of Done**: 모든 Pod 을 지우면 `consolidateAfter` 후 빈 노드가 **자동으로** drain (PDB 존중) 되고 전원이 꺼진다. 상태 전이 `Ready → Draining → ShuttingDown → Off` 확인.

- [ ] `cmd/onp-shutdown-agent/` DaemonSet (`privileged: true`, hostPID/hostIPC)
  - 자기 호스트의 Machine 만 watch (RBAC read-only)
  - `state == ShuttingDown` 감지 시 `systemctl poweroff`
- [ ] `disruption.consolidationPolicy: WhenEmpty` + `consolidateAfter` 처리
- [ ] Empty 노드 감지 (DaemonSet / static pod 제외)
- [ ] cordon → Eviction API (PDB 존중) → `state=ShuttingDown`
- [ ] `drain.timeoutSeconds` (기본 300s) 초과 시 `state=Failed` + uncordon + Event (`force=false` 기본)
- [ ] PSA `privileged` 네임스페이스 격리 매니페스트
- [ ] Node NotReady 감지 시 `state=Off` 전이
- [ ] **검증 (E2E #2)**: 모든 Pod 삭제 → 일정 시간 후 빈 노드 자동 drain → 전원 OFF → `state=Off` 확인

---

### M5 — 운영 폴리시 + 배포 가능성 — `DESIGN.md` 5 (DisruptionSafety, Observability)

**Definition of Done**: `helm install onp ./charts/onp` 한 번으로 클린 클러스터에서 전체 동작. 운영 안전장치(minNodes, cooldown, do-not-disrupt, maxConcurrent) 모두 작동. `/metrics` 로 핵심 메트릭 노출. README 만으로 다른 사람이 설치 가능.

- [ ] `minNodes` 하한 보장 — 풀이 minNodes 아래로 내려가는 스케일 다운 거절
- [ ] `maxConcurrent` — 풀당 동시 Draining 노드 수 제한 (기본 1)
- [ ] `onp.io/do-not-disrupt` 어노테이션 (Node/Pod 단위) 존중
- [ ] `cooldown.scaleDown` 적용
- [ ] Leader election (`coordination.k8s.io/Lease`)
- [ ] `/metrics` 노출:
  - [ ] `onp_nodes_total{pool, state}` (gauge)
  - [ ] `onp_scale_up_latency_seconds` (histogram)
  - [ ] `onp_power_on_total{provider, result}` (counter)
  - [ ] `onp_drain_failure_total{reason}` (counter)
  - [ ] `onp_pending_unschedulable` (gauge)
- [ ] Kubernetes Events 발행 (Machine / NodePool / 관련 Pod)
- [ ] Status Conditions 표준화 (`PowerOnSucceeded`, `DrainSucceeded`, `Ready`)
- [ ] `charts/onp/` Helm 차트 마무리 — M2~M4 에서 누적된 차트(controller + wol-agent + shutdown-agent + CRD + RBAC)에 PSA·values 정리·packaging·버전 태깅 추가
- [ ] README: 설치 가이드, 샘플 `NodePool` / `Machine` YAML, 트러블슈팅
- [ ] **검증**: 빈 클러스터에 `helm install` → E2E #1 / #2 시나리오 통과

---

## Phase 2 — 운영성

- [ ] 배치 스케줄링 + bin-packing (여러 pending 파드 → 최소 노드 집합)
- [ ] `WhenUnderutilized` consolidation (Karpenter 스타일)
- [ ] **IPMI provider** (full bidirectional: PowerOn / PowerOff / PowerStatus)
- [ ] Prometheus / Grafana 샘플 대시보드
- [ ] CRD validation webhook (capacity 음수, 충돌 selector, 잘못된 provider config 등)
- [ ] 부트 실패 retry / backoff 정책
- [ ] Dry-run 모드 (PowerOn/Off 호출 로깅만)
- [ ] `provider.PowerOff` 활성화 — 무응답 노드 hard-cut fallback
- [ ] `Machine` CRD rename 검토 (`PhysicalMachine` 등, Cluster API 충돌 회피)

---

## Phase 3 — 확장

- [ ] **Redfish provider**
- [ ] **Script provider** (escape hatch — 사용자 정의 셸 스크립트)
- [ ] 노드 OS 업데이트 윈도우 (꺼진 노드를 주기적으로 깨워 패치 적용 후 재차단)
- [ ] Multi-cluster 지원 (선택)
- [ ] e2e 테스트 자동화 (kind + 가상 노드)
- [ ] 다중 서브넷 wol-agent 라우팅 (`Machine.spec.power.wol.subnet`)

---

## 메모 / 결정 기록

- **매직 패킷**은 외부 라이브러리 없이 직접 구현 (102 bytes, 단순).
- **Drain** 은 client-go 의 Eviction API 직접 사용 (`kubectl/pkg/drain` 은 의존성이 큼).
- **Fit checker** 는 kube-scheduler framework 를 가져오지 않고 핵심 predicate 만 자체 구현 (Karpenter 와 같은 선택).
- **상태 머신 source of truth** 는 `Machine.status` (CRD-driven choreography). 컨트롤러 ↔ shutdown-agent 사이 직접 RPC 없음.
- **Phase 1 끄기 경로**는 항상 `shutdown-agent` — `provider.PowerOff` 는 Phase 2 의 hard-cut fallback 용으로 인터페이스에만 자리.
- **디자인 doc 권위 출처**: <https://www.industrialempathy.com/posts/design-docs-at-google/>
