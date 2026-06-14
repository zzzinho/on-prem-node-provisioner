# 05 — `drain.force` 강제 evict

> **`drain.force: true` 는 do-not-disrupt 면제를 해제해 보호 파드까지 Eviction API 로 내린다 — 단 PDB 는 여전히 존중한다.** 운영자의 명시적 opt-in 으로만 동작하며, 노드가 비워지면 정상 poweroff 로 이어진다.

## 기능

`NodePool.spec.drain.force` 가 true 면 `MachineReconciler` 의 drain 이 `onp.io/do-not-disrupt` 파드도 evict 대상에 포함한다. evict 는 항상 Eviction API 를 거치므로 PodDisruptionBudget 은 그대로 적용된다(직접 Pod delete 아님).

## 목적

"강제로 내려야 하는" 상황을 안전하게 표현한다 — 데이터 손실 위험이 있는 동작은 기본 off(`force=false`)이고, 운영자가 의도를 명시했을 때만 보호를 푼다. 그래도 PDB 라는 마지막 안전선은 유지한다.

## 예상 결과

- `force=true` + do-not-disrupt 파드 `guarded` → drain 시 guarded **evict 진행**(Eviction API).
- 노드 비워짐 → `Draining → ShuttingDown → Off`(poweroff).
- (PDB 가 0 disruption 을 강제하면 그 파드는 여전히 안 내려가고 타임아웃 — PDB 우선.)

## 재현 방법

1. worker-1 Ready 에 보호 파드 배치(03 과 동일):

   ```bash
   kubectl run guarded --image=nginx --restart=Never \
     --overrides='{"metadata":{"annotations":{"onp.io/do-not-disrupt":"true"}},
       "spec":{"nodeName":"worker-1","tolerations":[{"operator":"Exists"}],
       "containers":[{"name":"guarded","image":"nginx",
       "resources":{"requests":{"cpu":"100m","memory":"64Mi"}}}]}}'
   ```

2. 풀에 `force=true` 설정:

   ```bash
   kubectl patch nodepool demo --type merge -p \
     '{"spec":{"drain":{"force":true,"timeoutSeconds":120}}}'
   ```

3. 수동 drain 트리거 후 evict → poweroff 관찰:

   ```bash
   kubectl annotate machine worker-1 onp.io/drain-now=true --overwrite
   kubectl get machine worker-1 -w          # Draining -> ShuttingDown -> Off
   kubectl get pod guarded -w               # Running -> 사라짐(evicted)
   kubectl get events --field-selector involvedObject.name=worker-1 | grep -E 'Draining|PoweringOff'
   ```

4. 검증 후 원복:

   ```bash
   kubectl patch nodepool demo --type merge -p '{"spec":{"drain":{"force":false}}}'
   ```

> 이 테스트는 **의도적으로** 보호 파드를 내린다. force 의 반대(보호 유지)는 03 에서 확인한다.
