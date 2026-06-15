# 03 — do-not-disrupt 파드 + drain hard-guard

> **`onp.io/do-not-disrupt: "true"` 파드가 있는 노드는 자동 축소 대상에서 빠지고(노드를 non-empty 로 취급), 수동 drain 을 걸어도 `force=false` 면 그 파드를 evict 하지 않아 노드가 안 비워지고 drain 타임아웃 → uncordon + `Failed` 로 안전하게 멈춘다.** 보호 파드는 한 번도 안 내려간다.

## 기능

ONP 는 "비었나(emptiness)"와 "evict 대상(evictability)"을 분리한다. do-not-disrupt 파드는 **workload 로 카운트**(노드가 안 비었다고 봄)되고, drain 시에는 `drain.force=false` 면 evict 목록에서 제외된다. 결과적으로 그 파드는 자동·수동 어느 경로로도 내려가지 않는다.

## 목적

CLAUDE.md 의 "조용히 데이터를 잃는 기본값은 만들지 않는다" 안전 규칙. 보호 파드가 있는 노드를 실수로 비우거나, 모호한 성공으로 끝내지 않고 명확히 `Failed` 로 보낸다.

## 예상 결과

- **3a (자동 제외)**: guarded 파드가 있으면 노드 non-empty → `emptySince` 안 붙음 → scale-down 안 일어남. worker-1 `Ready`, guarded `Running` 유지.
- **3b (수동 drain hard-guard, force=false)**: `Draining` 진입하지만 guarded 는 evict 안 됨 → 노드 안 비워짐 → drain 타임아웃 → Event `DrainTimeout … uncordoned and marked Failed`, Machine `Failed`, 노드 uncordon, guarded 끝까지 `Running`. `onp_drain_failure_total{reason="drain_timeout"}` +1.

## 재현 방법

1. worker-1 Ready 에서 보호 파드를 nodeName 으로 핀(cordon 우회):

   ```bash
   kubectl run guarded --image=nginx --restart=Never \
     --overrides='{"metadata":{"annotations":{"onp.io/do-not-disrupt":"true"}},
       "spec":{"nodeName":"worker-1","tolerations":[{"operator":"Exists"}],
       "containers":[{"name":"guarded","image":"nginx",
       "resources":{"requests":{"cpu":"100m","memory":"64Mi"}}}]}}'
   ```

2. **3a** — 자동 축소 제외 확인(다른 워크로드는 비움):

   ```bash
   kubectl patch nodepool demo --type merge -p \
     '{"spec":{"minNodes":0,"disruption":{"consolidationPolicy":"WhenEmpty","consolidateAfter":"60s"}}}'
   # 90초+ 관찰: emptySince 가 계속 비어 있어야 함
   kubectl get machine worker-1 -o jsonpath='{.status.state}{" emptySince="}{.status.emptySince}{"\n"}'
   ```

3. **3b** — 수동 drain hard-guard:

   ```bash
   kubectl patch nodepool demo --type merge -p '{"spec":{"drain":{"timeoutSeconds":60,"force":false}}}'
   kubectl annotate machine worker-1 onp.io/drain-now=true --overwrite
   kubectl get machine worker-1 -w          # Draining -> (60s) -> Failed
   kubectl get pod guarded -o wide          # 내내 Running
   kubectl get events --field-selector involvedObject.name=worker-1 | grep DrainTimeout
   curl -s localhost:8080/metrics | grep 'onp_drain_failure_total{reason="drain_timeout"}'
   ```

4. 복구(Failed → Ready, 노드는 살아있음):

   ```bash
   kubectl patch machine worker-1 --subresource=status --type merge -p '{"status":{"state":"Ready"}}'
   ```

> 강제로 내려야 한다면 `drain.force=true` 로 면제를 해제한다 → 05(drain-force) 참고.
