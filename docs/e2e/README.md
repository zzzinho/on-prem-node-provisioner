# ONP 목적 기능 E2E 재현 가이드

ONP 의 핵심 기능 — pending 파드 spec 으로 적합한 노드를 Wake-on-LAN 으로 깨우고(workload-aware scale-up), 노드가 비면 안전하게 drain 후 전원을 끄는(safe scale-down) — 을 기능별로 실클러스터에서 재현·확인한다. 배포·HA·관측·정책 knob 같은 운영 기능은 여기서 다루지 않는다.

> 노드 이름(`cp-1`, `worker-1`), 풀 이름(`demo`), MAC 주소는 모두 예시다 — 자신의 클러스터 값으로 바꿔 쓴다.

## 전제 (공통)

**Kubernetes 클러스터는 이미 구성돼 있다고 가정한다** — 노드 join 완료, `kubectl` 접근 가능. 추가로 아래 두 종류의 노드가 필요하다.

### 토폴로지

- **항상 켜진 노드**(예: 컨트롤 플레인) — `onp-wol-agent` 가 여기 떠서 같은 L2 세그먼트로 WoL 매직 패킷을 보낸다. 이 문서에서는 `cp-1` 로 부른다.
- **관리 대상 노드**(ONP 가 켜고 끄는 물리 노드, WoL 지원) — 1대 이상. 이 문서에서는 `worker-1` 로 부른다.

### 노드 라벨

```bash
kubectl label node cp-1     onp.io/always-on=true --overwrite   # wol-agent 스케줄
kubectl label node worker-1 onp.io/managed=true   --overwrite   # shutdown-agent 스케줄
kubectl label node worker-1 onp.io/pool=demo      --overwrite   # 풀 파드 스케줄(운영자 책임)
```

> `onp.io/pool` 라벨이 없으면 ONP 가 노드를 깨워도 `nodeSelector: onp.io/pool=demo` 파드가 스케줄되지 않는다. ONP 의 fit 시뮬레이션은 `NodePool.template.labels` 로 판정할 뿐 노드 라벨링은 하지 않는다(노드 셋업은 Non-Goal).

### ONP 배포 + CRD

```bash
helm upgrade --install onp ./charts/onp -n onp-system --create-namespace
kubectl apply -f charts/onp/crds/        # 업그레이드 때마다 필수
kubectl -n onp-system rollout status deploy/onp-controller
```

> 이미지가 차트 기본 레지스트리(`ghcr.io`)가 아닌 곳에 있으면 `--set image.registry=<registry>` 로 지정한다.
>
> **함정**: Helm 은 `helm upgrade` 시 `crds/` 의 CRD 를 갱신하지 않는다(최초 install 만). 차트를 올렸으면 위 `kubectl apply -f charts/onp/crds/` 를 꼭 같이 친다. 안 그러면 컨트롤러가 새 status 필드를 patch 하다 `unknown field` 로 거절당한다.

### NodePool + Machine

풀 정책 1개와, 관리 노드마다 Machine 1개를 만든다(아래는 예시 — 노드 이름·MAC·용량을 자신의 값으로 바꾼다):

```yaml
apiVersion: onp.io/v1alpha1
kind: NodePool
metadata: { name: demo }
spec:
  minNodes: 0
  maxNodes: 3
  machineSelector: { matchLabels: { onp.io/pool: demo } }
  template:        { labels:      { onp.io/pool: demo } }
---
apiVersion: onp.io/v1alpha1
kind: Machine
metadata:
  name: worker-1
  labels: { onp.io/pool: demo }          # 풀 멤버십
spec:
  nodeName: worker-1
  capacity: { cpu: "4", memory: 8Gi }    # 꺼진 상태 fit-check 용 (노드 실제 용량에 맞춤)
  labels:   { kubernetes.io/hostname: worker-1 }
  power:    { provider: wol, wol: { macAddress: "aa:bb:cc:dd:ee:ff", broadcastAddress: "255.255.255.255" } }
  shutdown: { provider: agent }
```

`config/samples/` 의 예시 매니페스트를 복사해 써도 된다.

### 관찰 명령 (모든 문서 공통)

```bash
kubectl get machine worker-1 -w                                                   # 상태 머신 실시간
kubectl get events --field-selector involvedObject.name=worker-1 --sort-by=.lastTimestamp
kubectl get nodes
kubectl -n onp-system port-forward deploy/onp-controller 8080:8080 &              # 메트릭
curl -s localhost:8080/metrics | grep '^onp_'
```

### 관리 노드 비우기 (scale-down 계열 전제: 02 / 03 / 04)

ONP 의 자동 scale-down 은 노드가 **빈 상태**(workload 파드 없음 — DaemonSet·mirror 파드는 제외)일 때만 동작한다. 따라서 02/03/04 를 재현하려면 관리 노드를 먼저 비운다. 테스트 클러스터라면 관리 노드에 워크로드를 두지 않는 게 가장 간단하다. 이미 떠 있다면 무엇이 도는지 확인하고 다른 노드로 옮기거나 0 으로 스케일 다운한다:

```bash
kubectl get pods -A --field-selector spec.nodeName=worker-1   # workload 파드 확인 (DaemonSet 제외)
# 각 워크로드를 graceful 하게 내린다: Deployment/StatefulSet replicas=0, Operator 는 자체 방식.
```

> do-not-disrupt 파드도 workload 로 카운트되므로(03 참고), 비울 때 누락하면 노드가 "비었다"고 판정되지 않는다. 복원은 내렸던 워크로드를 되돌리면 된다 — 노드를 수동 cordon 했다면 재기상 후 uncordon 한다.

## 목적 기능 테스트

워크로드-aware wake(01) 와 안전한 scale-down(02) 이 두 축이고, 03–05 는 scale-down 의 안전 의미론(보호 파드 무중단, 노드 단위 제외, 강제 evict)이다.

| # | 문서 | 기능 |
| --- | --- | --- |
| 01 | [scale-up-wol-wake](01-scale-up-wol-wake.md) | pending 파드 → 적합 노드 WoL 실 wake (workload-aware proactive) |
| 02 | [scale-down-poweroff](02-scale-down-poweroff.md) | 빈 노드 cordon → drain(PDB 존중) → 실 poweroff |
| 03 | [do-not-disrupt-pod](03-do-not-disrupt-pod.md) | 보호 파드 무중단 + 수동 drain hard-guard → 타임아웃 Failed |
| 04 | [do-not-disrupt-node](04-do-not-disrupt-node.md) | 노드 단위 자동 축소 제외 |
| 05 | [drain-force](05-drain-force.md) | 보호 면제 해제 강제 evict (PDB 는 여전히 존중) |
