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
- [x] 디렉터리 rename (`wolscaler` → `onp`)
- [x] `docs/DESIGN.md` — Google 스타일 디자인 doc (5 섹션, mermaid 5개)
- [x] `CLAUDE.md` — 코드 작성 시 자동 로드되는 한 페이지 가이드
- [x] TODO 를 수직 슬라이스 구조로 재구성

---

## Phase 1 — MVP (M1 → M5)

**상태: 완료 ✅** — M1~M5 + 정합성 패스(C1~C7) 전부 구현·머지(PR #5, 차트 0.6.0), 155 단위 테스트 + 실하드웨어 E2E #1~#4 통과. 수직 슬라이스(WoL wake → 자동 scale-up → 안전 scale-down → 운영 폴리시)가 실하드웨어에서 검증됨. 노드 WoL 하드웨어 한계는 아래 "메모 / 결정 기록" 참조(ONP Non-Goal).

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
- 이미지 배포: **자체 zot OCI 레지스트리 (`<registry>/onp/...`)** (microk8s containerd 익명 pull, pullSecret 불필요).
- 배포 방식: **Helm 차트 (`charts/onp/`)** — M2 부터 도입하고 마일스톤마다 컴포넌트를 더한다 (M4 shutdown-agent, M5 PSA·packaging). 생 manifest 는 두지 않는다.
- 검증 대상: **실제 microk8s 클러스터의 물리 노드** (kind 아님).
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
- [x] ✅ 체크포인트: 같은 L2 의 항상-켜진 노드에서 agent 가 실제 LAN broadcast 송신 (204 + JSON 로그) 검증 완료

#### M2.2 — reconciler wake 경로

- [x] `internal/controller/machine_controller.go`:
  - 어노테이션 `onp.io/wake-now=true` + `state=Off` → `provider.PowerOn` → `state=Booting`
  - Node watch (`EnqueueRequestsFromMapFunc` + `spec.nodeName` field indexer) → 대상 Node Ready → `state=Ready` + 어노테이션 제거
  - `bootTimeout` 경과(Ready 미관찰) → `state=Failed` + Event; PowerOn 실패는 `Off` 유지 + backoff 재시도
  - 컨트롤러 재시작에도 idempotent (source of truth = `status.state`)
- [x] `internal/controller/machine_controller_test.go` — **fake client + fake clock/provider**, 5개 전이 케이스 (envtest 대신 hermetic)
- [x] RBAC(`config/rbac/role.yaml`) 생성, `status.bootStartTime` CRD 필드 추가
- [x] ✅ 체크포인트: 실제 클러스터에서 타깃 노드 Machine 어노테이션 → `Off → Booting → Ready` 라이브 검증 (Events 로 확인)

#### M2.3 — Helm 차트 + 배포 + E2E

- [x] `charts/onp/` Helm 차트:
  - `crds/onp.io_machines.yaml` — 생성 CRD 복사 (Helm `crds/` 규약, byte-identical)
  - `templates/` — controller Deployment, wol-agent DaemonSet(`hostNetwork: true`, `onp.io/always-on` nodeSelector) + Service(ClusterIP 9119), ClusterRole/Binding + SA, hardened securityContext(nonroot·drop ALL·readonly fs)
  - `values.yaml` — 이미지 registry/repo/tag, placement(nodeSelector/toleration), `bootTimeout`
- [x] 파라미터화 `Dockerfile` (`--build-arg BIN`, `distroless/static:nonroot`, `$BUILDPLATFORM` 크로스컴파일) — controller/agent/wol-probe 공용
- [x] 이미지 빌드/푸시 → `<registry>/onp/{onp-controller,onp-wol-agent}:0.1.0` (자체 레지스트리, 익명 pull)
- [x] `securityContext.runAsUser: 65532` — distroless `USER nonroot`(비숫자) + `runAsNonRoot` 검증 이슈 해결
- [x] ✅ **검증 (E2E)**: `helm install onp ./charts/onp -n onp-system` → microk8s 배포(controller Deployment + wol-agent DaemonSet) → Machine 어노테이션 → **배포된 controller→Service→agent 가 실제 패킷 송신**, `Off→Booting→Ready`(Events). 물리 wake(타깃 노드 OFF→부팅)는 별도로 실증.

---

### M3 — Pending Pod 자동 wake: 첫 자동 스케일 업 — `DESIGN.md` 3.3 (Scale-up)

**Definition of Done**: 풀의 모든 노드를 꺼둔 상태에서 Pod 을 apply 하면, 적합한 Machine 이 **자동으로** 깨어나고 Pod 가 스케줄된다. 어노테이션 불필요.

- [x] `api/v1alpha1/nodepool_types.go` — NodePool CRD 전체 스키마 (`minNodes`/`maxNodes`/`machineSelector`/`template`/`disruption`/`cooldown`/`drain` — M3 미사용 필드는 전방호환 자리, cluster-scoped `np`)
- [x] NodePool reconciler — `machineSelector` 멤버십 → `status.totalMachines`/`readyMachines` 집계 (fake client 테스트 + 라이브 검증 `total=2 ready=1`)
- [x] Pod watcher — `PodScheduled=False, Reason=Unschedulable` 만 큐잉 (predicate 필터)
- [x] Fit checker (`internal/scheduler/fit.go`) — 리소스 + nodeSelector + tolerations + required nodeAffinity. kube-scheduler framework 의존 X (predicate 는 `k8s.io/component-helpers` 위임, 17개 단위 테스트)
- [x] 후보 Machine 선정 로직: off 상태 + pool 멤버 + fit pass → best-fit (가장 작은 capacity 우선) + in-flight 가드(중복 wake 방지), 어노테이션 트리거로 M2.2 wake 경로 재사용
- [x] `maxNodes`, `cooldown.scaleUp` 적용 (per-pool 가드, cooldown 은 `NodePool.status.lastScaleUpTime` 앵커 + 정확한 requeue, fake-clock 테스트)
- [x] **검증 (E2E #1)**: 풀의 모든 노드 끄기 → `kubectl run pod ...` → 자동 wake + 스케줄 확인
  - 실하드웨어에서 전 구간 통과(0.2.0 배포): unschedulable 파드 → scaleup 이 best-fit Machine 선정 → `wake-now` → PowerOn(WoL 매직 패킷) → 노드 부팅 → Node Ready → Machine Ready → kube-scheduler 가 파드를 해당 노드에 바인딩. **wake 송신부터 Ready+스케줄까지 ~40초.**
  - 검증 노트: 타깃 노드가 ICMP 를 차단하므로 liveness 는 ICMP ping 이 아니라 **Node Ready / arping(L2)** 로 판정해야 함. 노드의 WoL 무장(`ethtool wol g`)은 비영속이라 재부팅 시 풀릴 수 있음 — 검증 전 무장 상태 확인 필요(ONP 외부 / 노드 운영자 책임).

---

### M4 — Safe Shutdown: 첫 자동 스케일 다운 — `DESIGN.md` 3.3 (Scale-down), 5.DisruptionSafety

**Definition of Done**: 모든 Pod 을 지우면 `consolidateAfter` 후 빈 노드가 **자동으로** drain (PDB 존중) 되고 전원이 꺼진다. 상태 전이 `Ready → Draining → ShuttingDown → Off` 확인.

- [x] `cmd/onp-shutdown-agent/` DaemonSet (`privileged: true`, hostPID) — M4.0
  - [x] 자기 호스트의 Machine 만 watch (predicate 필터, RBAC read-only `machines` get/list/watch)
  - [x] `state == ShuttingDown` 감지 시 `nsenter -t 1 systemctl poweroff` (graceful; 멱등 sync.Once)
- [x] `disruption.consolidationPolicy: WhenEmpty` + `consolidateAfter` 처리 — M4.2 (`ScaleDownReconciler`: 빈 노드가 `consolidateAfter` 동안 유지되면 `onp.io/drain-now` 트리거 → M4.1 drain 경로 재사용. policy/consolidateAfter 둘 다 명시 opt-in 이어야 동작 — 둘 중 하나라도 없으면 자동 scale-down off)
- [x] Empty 노드 감지 (DaemonSet / static pod 제외) — M4.2 (`status.emptySince` 앵커, drain 완료 판정과 동일한 evictable 정의 공유 — DaemonSet/mirror/terminating/finished 제외, fake-clock 단위 테스트)
- [x] cordon → Eviction API (PDB 존중) → `state=ShuttingDown` — M4.1 (`onp.io/drain-now` 트리거, DaemonSet/mirror/terminating 제외, Eviction 주입형으로 단위 테스트)
- [x] `drain.timeoutSeconds` (기본 300s, NodePool 해석) 초과 시 `state=Failed` + uncordon + Event (`force=false` 기본) — M4.1
- [x] PSA `privileged` 네임스페이스 격리 매니페스트 — M5.4 에서 옵션 `namespace.yaml`(`createNamespace` 게이트) + NOTES/README 의 `kubectl label ns ... enforce=privileged` 안내로 완료
- [x] Node NotReady 감지 시 `state=Off` 전이 — M4.0 (MachineReconciler `ShuttingDown→Off`, 실하드웨어 검증: agent `PoweringOff` → controller `PoweredOff`)
- [x] **검증 (E2E #2)** — 실하드웨어 통과 (2026-06-03, 차트 0.4.0 배포). 두 경로 모두 검증:
  - **성공경로**: 빈 노드(desktop1) → `consolidateAfter` 60s 경과 → `ScaleDown` 이벤트("empty for 1m0s") → 자동 `drain-now` → `Draining`(빈 노드라 즉시) → `DrainSucceeded` → shutdown-agent `PoweringOff` → Node NotReady → `state=Off`. 재기상(실 WoL 부팅 ~54s)으로 복구.
  - **안전경로**: PDB(`pg-primary`, ALLOWED DISRUPTIONS=0)로 보호된 CNPG `pg-1` 이 있는 노드에 drain → eviction 차단 → `drain.timeoutSeconds`(30s) 초과 → `DrainTimeout` Warning + **uncordon + `state=Failed`**. **pg-1 은 한 번도 안 내려감** — "조용히 데이터를 잃지 않는다" 기본값 실증.
  - 검증 노트: desktop1 의 GPU 핀 워크로드(Knative LLM)는 KPA 가 재배치하므로 빈 노드를 만들려면 수동 cordon 필요. **발견한 갭**: scale-down 이 cordon 한 노드를 재기상해도 ONP 가 자동 uncordon 하지 않음 → scale-up 으로 깨운 노드가 unschedulable 로 남을 수 있음. wake→Ready 경로에서 uncordon 필요 (아래 M5 항목 참조).

---

### M5 — 운영 폴리시 + 배포 가능성 — `DESIGN.md` 5 (DisruptionSafety, Observability)

**Definition of Done**: `helm install onp ./charts/onp` 한 번으로 클린 클러스터에서 전체 동작. 운영 안전장치(minNodes, cooldown, do-not-disrupt, maxConcurrent) 모두 작동. `/metrics` 로 핵심 메트릭 노출. README 만으로 다른 사람이 설치 가능.

**기반 결정 / 리뷰 반영** (M5 착수 시 확정):

- M4 직후 전체 코드 리뷰 → destructive path 는 안전(Eviction/PDB/force-off/노드 신원 이중방어)하나 운영 안전장치 미구현 확인. minNodes 하한이 1순위 갭.
- 스키마 4필드 추가 (모두 additive): `disruption.maxConcurrent`(기본 1), `status.lastScaleDownTime`, Machine `status.shutdownStartTime`/`notReadySince`.
- 리뷰에서 나온 상태머신 갭 2개 포함: **A1** shutdown 타임아웃→`Failed`, **A2** 외부 노드 손실 시 grace 후 `Ready→Off`.
- emptiness ↔ evictability 분리 (`isWorkload` vs `isDrainable`) — do-not-disrupt 를 단일 술어에 넣는 함정 회피.
- Leader election 은 wiring + lease RBAC 만, 차트 기본은 1 replica / off (전원 컨트롤러는 단일 결정자가 안전).

- [x] `minNodes` 하한 보장 — 풀이 minNodes 아래로 내려가는 스케일 다운 거절 (`ScaleDownReconciler.triggerScaleDown`, keptOn 카운트는 in-flight drain 제외)
- [x] `maxConcurrent` — 풀당 동시 Draining 노드 수 제한 (기본 1, `disruption.maxConcurrent` nil→1)
- [x] `onp.io/do-not-disrupt` 어노테이션 (Node/Pod 단위) 존중 — Node=scale-down 제외, Pod=노드 non-empty 취급 + 수동 drain hard-guard (force 아니면 evict 안 함→timeout→Failed)
- [x] `cooldown.scaleDown` 적용 (`status.lastScaleDownTime` 앵커, `stampScaleDown`/`coolingDownUntil` = scaleUp 대칭)
- [x] **wake→Ready 시 자동 uncordon** (E2E #2 에서 발견) — `Booting→Ready` 에서 `onp.io/cordoned-by-onp` 마커가 붙은 ONP 자신의 cordon 만 해제 (운영자 수동 cordon 과 구분). M4 단계에서 선반영됨.
- [x] **A1 — shutdown 타임아웃** (리뷰): `ShuttingDown` 에서 Node 가 `--shutdown-timeout`(기본 5m) 내 NotReady 안 되면 `Failed` (무한 폴링 제거).
- [x] **A2 — 외부 노드 손실** (리뷰): `Ready` Machine 의 Node 가 ONP drain 없이 NotReady → `--node-loss-grace-period`(기본 1m) 후 `Off` (scale-up 이 자연 복구; blip 흡수).
- [x] Leader election (`coordination.k8s.io/Lease`) — wiring + `LeaderElectionReleaseOnCancel` + 조건부 lease RBAC. 차트 기본 1 replica/off.
- [x] `/metrics` 노출 (`onp_` 5종, controller-runtime 레지스트리):
  - [x] `onp_nodes_total{pool, state}` (gauge) — scrape-time Collector
  - [x] `onp_scale_up_latency_seconds` (histogram) — BootStartTime→Ready
  - [x] `onp_power_on_total{provider, result}` (counter)
  - [x] `onp_drain_failure_total{reason}` (counter) — drain_timeout / shutdown_timeout
  - [x] `onp_pending_unschedulable` (gauge) — scrape-time Collector
- [x] Kubernetes Events 발행 (Machine / NodePool / 관련 Pod) — NodePool `Membership` 이벤트 추가, Machine/Pod 는 기존
- [x] Status Conditions 표준화 (`PowerOnSucceeded`, `DrainSucceeded`, `Ready`) — `setCondition` 표준 패턴
- [x] `charts/onp/` Helm 차트 마무리 — PSA(옵션 namespace 템플릿)·lease RBAC·values 정리·신규 플래그·버전 0.5.0
- [x] README: 설치 가이드, 샘플 `NodePool` / `Machine` YAML, 운영 폴리시 표, 메트릭, 트러블슈팅
- [x] **검증 (E2E #3)** — 실하드웨어 전수 통과 (2026-06-06, 차트 0.5.0 배포, microk8s desktop/desktop1). 12개 Phase + auto-uncordon: 설치/CRD, leader election, 메트릭 5종, 실 WoL wake(×4)·scale-up latency, minNodes 하한, 실 poweroff(×3), do-not-disrupt Pod/Node, drain.force, A1 shutdown 타임아웃, A2 노드 손실, cooldown.scaleDown 모두 관측. 목적 기능(wake / scale-down / do-not-disrupt / force) 재현 절차는 `docs/e2e/`.
  - `maxConcurrent` 직렬화는 단일 wakeable 노드라 미관측(단위 테스트 `TestScaleDownDefersAtMaxConcurrent` 커버).
  - **발견**: (1) Helm 은 `helm upgrade` 시 `crds/` 를 갱신하지 않음 → 업그레이드 후 `kubectl apply -f charts/onp/crds/` 필요. (2) 관리 노드는 NodePool `template.labels` 를 실제로 carry 해야 wake 후 스케줄됨 → C3 에서 ONP 가 Ready 시 자동 적용으로 해소. 둘 다 README 트러블슈팅 반영.

---

### M5.6 — Phase 1 정합성 패스 (DESIGN ↔ 코드 ↔ CLAUDE 불변식)

**왜**: M5 완료 후 디자인 doc·CLAUDE.md 의 안전/확장 불변식 대비 전수 점검에서 7개 갭(C1~C7)을 확정. 모두 additive 또는 동작 강화이고 인터페이스/CRD 스키마는 안 깨짐. 단위 테스트 동반(155 통과). 실하드웨어 재검증 완료 — 아래 E2E #4.

- [x] **C1** — operator cordon 흡수 방지: ONP 가 자기 cordon 에 `onp.io/cordoned-by-onp` 마커를 달고, wake/timeout uncordon 시 **마커가 있는 노드만** 해제 (운영자 수동 cordon 보존).
- [x] **C2** — stale `onp.io/drain-now` 정리: `Off`/`Booting`/`Draining` 진입 시 잔존 drain-now 어노테이션 제거 (재기상 후 즉시 재drain 방지).
- [x] **C3** — Ready 시 `NodePool.template.labels`(Machine `spec.labels` 우선) + `template.taints` 를 Node 에 적용 + `spec.capacity` vs `Node.status.capacity` drift 경고 Event(`CapacityDrift`).
- [x] **C4** — 한 Machine 이 복수 NodePool 에 매칭되는 충돌 감지: scale-down 보류 + `PoolConflict` Warning Event (DESIGN 3.2 의 "충돌을 감지해 Event 로 알리고 reconcile 보류" 실현).
- [x] **C5** — wol-agent 인증: 컨트롤러↔agent 공유 bearer token(constant-time 비교, `MaxBytesReader`), Helm 토큰 Secret 자동 생성·persist, controller split-brain 가드. (hostNetwork 라 NetworkPolicy 불가 → 토큰이 실질 방어.)
- [x] **C6** — shutdown-agent 가 `spec.shutdown.provider` 존중: non-agent provider 선택 시 agent 가 전원 안 끔 (Phase 2 hard-cut 경로와 충돌 방지).
- [x] **C7** — `PowerProvider.PowerStatus` 반환을 `(power.State, error)` 로 안정화 (DESIGN 3.4 인터페이스와 일치, IPMI/Redfish 확장 대비).
- [x] **검증 (E2E #4)** — 실하드웨어 통과 (2026-06-15, 차트 0.6.0 배포, microk8s desktop/desktop1). 이미지 빌드·푸시 → `helm upgrade` 0.5.0→0.6.0(토큰 Secret 자동 생성·split-brain 가드·CRD 무변·파드 健全), 그리고: **C5** 인증 wake 경로(controller→agent 401 없이 패킷 송신, Off→Booting), **C3** Ready 시 `CapacityDrift` Event(선언 4코어/8Gi vs 실제 20코어/62Gi) + 라벨 적용, **C1** spurious cordon 없음, **A2** 수동 shutdown→`NodeLost`+grace 1분→Ready→Off 모두 관측. C2/C4/C6/A1 은 단위 테스트(155) 커버, scale-down 파괴 경로는 M5 E2E #3(0.5.0) 기검증.
  - **WoL 하드웨어 한계 발견**: desktop1(RTL8125B)은 **long-off WoL 불안정**(short-off OK / 2일 off 실패, ARP incomplete=링크 down). 전수 점검 결과 ASPM(lspci로 Disabled 확인)/EEE/wakeup 전부 정상이라 원인은 장시간 S5 링크 down — **스위치 green-ethernet 1순위 의심**(이전 ErP 가설은 short-off 성공으로 폐기). ONP는 boot-timeout→Failed 로 안전 처리 — Phase 2 boot-retry/backoff + IPMI provider 가치를 실증.

### M5.7 — always-on 노드 보호 (차트 0.6.1)

**왜**: `onp.io/always-on` 은 차트가 컨트롤러·wol-agent 를 고정하는 데만 쓰였고, 컨트롤러 자신은 이 라벨을 몰랐다. always-on 노드 위에 Machine 을 잘못 선언하면 ONP 가 자기(와 컨트롤 플레인)를 drain·종료할 수 있었다. 또 `leaderElect: false` 인데 RollingUpdate 라 업그레이드 때마다 컨트롤러 두 개가 몇 초씩 동시에 active 였다(split-brain 창).

- [x] **always-on drain 거부** — 수동 drain-now 와 자동 scale-down 이 만나는 유일한 관문 `startDraining` 에서, 백킹 Node 가 `onp.io/always-on=true` 면 Ready 유지 + drain-now 제거 + `DrainRefused` Warning Event. cordon·eviction 없음.
- [x] **scale-down 제외** — always-on 노드는 Node `do-not-disrupt` 처럼 empty 타이머를 시작하지 않는다. 거부된 drain 을 scale-down 이 계속 재요청하는 루프 방지.
- [x] **Recreate 롤아웃** — `controller.leaderElect: false` 면 Deployment strategy 를 `Recreate` 로 렌더(구 파드 종료 후 신 파드 시작).
  - **Helm 4 업그레이드 주의**: Helm 4 는 server-side apply 라, 0.6.0 이하에서 올리면 apiserver 가 기본값으로 채운 `strategy.rollingUpdate`(소유자 없음)가 남아 `rollingUpdate: Forbidden ... 'Recreate'` 로 실패한다(템플릿에 `rollingUpdate: null` 을 넣어도 동일). 업그레이드 전에 한 번만 `kubectl -n onp-system patch deploy onp-controller --type=json -p '[{"op":"remove","path":"/spec/strategy/rollingUpdate"},{"op":"replace","path":"/spec/strategy/type","value":"Recreate"}]'`. strategy 는 파드 템플릿 밖이라 롤아웃을 일으키지 않는다. 신규 설치·Helm 3 업그레이드는 해당 없음.
- [x] **검증** — 실클러스터 배포 (2026-10-04, `helm upgrade` 0.6.0→0.6.1, 위 1회 패치 후). 컨트롤러·wol-agent 0.6.1 이 always-on 노드(desktop1)에서 Running, strategy `Recreate`, 에러 로그 없음. 거부 경로 자체는 단위 테스트로 커버(가드 제거 시 두 테스트 실패 확인) — always-on 노드를 실제로 drain 시도하는 라이브 검증은 하지 않는다.

### M5.8 — drain 완료 판정 + Booting 중 PowerOn 재전송 (차트 0.6.2)

**왜**: on-demand 노드(desktop) 실하드웨어 전원 사이클(2026-10-05)에서 두 결함이 드러났다. ① eviction 이 받아들여지는 순간 종료 중(Terminating) 파드를 워크로드에서 빼서, `Draining`·`DrainSucceeded`·전원 차단이 같은 초에 일어났다 — 파드의 graceful 종료(preStop, `terminationGracePeriodSeconds`)를 기다리지 않고 OS 를 내림. ② `ShuttingDown → Off` 는 Node NotReady(=kubelet 정지) 시점이라 OS 종료 완료보다 앞선다. Pending 파드 때문에 scale-up 이 Off 직후(+60초) 깨웠고, 아직 S5 에 못 들어간 보드가 매직 패킷을 버려 boot-timeout(10분) → `Failed` 로 끝났다.

- [x] **drain 이 종료 중 파드를 기다림** — `isWorkload` 가 Terminating 파드도 노드를 차지하는 것으로 센다(`kubectl drain` 의 wait-for-delete 와 같은 의미). `isDrainable` 은 이미 종료 중인 파드를 다시 evict 하지 않는다. 영영 안 사라지면 기존 drain timeout → `Failed` + uncordon 이 backstop. scale-down 의 empty 판정도 같은 정의를 써서 파드가 실제로 사라진 뒤에 타이머를 시작한다.
- [x] **Booting 중 PowerOn 재전송** — Node 가 아직 Ready 가 아니면 첫 폴링 간격(15초) 이후 매 폴링마다 `PowerOn` 을 다시 보낸다. 켜진 보드에 power-on 은 no-op 이라 안전, CRD 변경 없음. `Failed` 이후 재시도는 여전히 Phase 2.
- [x] **검증** — 실하드웨어 (2026-10-05, 차트 0.6.2, desktop 이 S5 로 ~40분 꺼진 뒤 `Failed → Off` 복구 + wake-now). 08:23:48 첫 `PowerOn` → 08:24:03·08:24:18 재전송(15초 간격) → 08:24:20 Node Ready + `Uncordoned`. 재전송은 부팅 중 무해함을 확인(이번엔 첫 패킷으로 깨어남). drain 대기 수정은 단위 테스트로 커버, 실하드웨어 drain 은 다음 전원 사이클에서 확인.

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
- **알려진 한계 — 노드 WoL (ONP Non-Goal)**: 검증 환경의 일부 NIC(Realtek RTL8125B)은 장시간 종료 후 WoL 매직 패킷에 응답하지 않는다. 정밀 진단(2026-06-15) 결과 노드 측 설정은 정상(WoL 무장 `g`, ASPM/EEE off, PCI·ACPI wakeup enabled, PME D3cold) — 원인은 장시간 S5 에서 이더넷 링크가 끊기는 것으로, **스위치 포트의 green-ethernet/EEE 가 1순위 의심**(이전 ErP 가설은 short-off 성공으로 폐기). ONP 송신/인증/boot-timeout 은 정상이며 불응답 시 `Failed` 로 안전 처리. 근본 해결은 스위치 EEE off 또는 독립 전원 경로(Intel NIC / Phase 2 IPMI provider).
- **디자인 doc 권위 출처**: <https://www.industrialempathy.com/posts/design-docs-at-google/>
