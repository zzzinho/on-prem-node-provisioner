# 02 — 자동 scale-down / 실 poweroff

> **빈 노드가 `consolidateAfter` 만큼 유지되면 ONP 가 cordon → drain(PDB 존중) → shutdown-agent 로 실제 전원을 끈다.** 관측: `Ready→Draining→ShuttingDown→Off`, Event `PoweringOff`, 노드 실제 다운, `drain_failure` 증가 없음.

## 기능

`ScaleDownReconciler` 가 빈 노드에 `emptySince` 를 스탬프하고, `consolidateAfter` 경과 + 풀 가드(minNodes/maxConcurrent/cooldown) 통과 시 `onp.io/drain-now` 를 단다. `MachineReconciler` 가 cordon → Eviction API drain → `ShuttingDown` 으로 전이하고, 해당 노드의 `onp-shutdown-agent` 가 `Machine.status` 를 watch 하다 `nsenter systemctl poweroff` 를 실행한다.

## 목적

빈 노드를 안전하게(데이터 손실 없이) 끄는 핵심 경로를 실하드웨어로 확인한다. 상태 머신의 source of truth 가 `Machine.status` 이고 컨트롤러↔agent 가 CRD watch 로 조율됨을 검증한다.

## 예상 결과

- `Ready → Draining(빈 노드라 즉시) → ShuttingDown → Off`.
- Event `PoweringOff: issued graceful power-off on node "worker-1"`.
- 노드 `NotReady`(전원 차단), `nodepool.status.lastScaleDownTime` 스탬프.
- `onp_drain_failure_total` **증가 없음**(클린 성공), `onp_nodes_total{state="Off"}` 반영.

## 재현 방법

1. shutdown-agent 가 worker-1 에 떠 있는지 확인(poweroff 주체):

   ```bash
   kubectl get pod -n onp-system -l app.kubernetes.io/component=shutdown-agent -o wide   # NODE=worker-1, Running
   ```

2. 풀을 scale-down 가능 상태로:

   ```bash
   kubectl patch nodepool demo --type merge -p \
     '{"spec":{"minNodes":0,"disruption":{"consolidationPolicy":"WhenEmpty","consolidateAfter":"60s"},"drain":{"timeoutSeconds":120,"force":false}}}'
   ```

3. worker-1 을 비운다(공통 전제) — 이미 비었으면 그대로.

4. 전 과정 관찰:

   ```bash
   kubectl get machine worker-1 -w     # Ready -> Draining -> ShuttingDown -> Off
   kubectl get nodes worker-1 -w       # Ready -> NotReady
   kubectl get events --field-selector involvedObject.name=worker-1 | grep -E 'ScaleDown|Draining|PoweringOff'
   ```

5. 합격 기준: 상태가 `Off` 로 가고 worker-1 전원이 실제로 차단된다. `lastScaleDownTime` 확인:

   ```bash
   kubectl get nodepool demo -o jsonpath='{.status.lastScaleDownTime}{"\n"}'
   ```

> agent 로그를 보려 해도 노드가 꺼진 뒤에는 `no route to host` 가 정상(노드 다운의 방증). `PoweringOff` 이벤트가 agent 가 명령을 실행했음을 증명한다.
