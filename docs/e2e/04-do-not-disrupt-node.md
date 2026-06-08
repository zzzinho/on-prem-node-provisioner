# 04 — do-not-disrupt 노드

> **노드에 `onp.io/do-not-disrupt: "true"` 를 달면 ONP 가 그 노드를 자동 축소에서 완전히 제외한다.** 빈 노드라도 `emptySince` 조차 스탬프하지 않고 skip 한다. 어노테이션을 떼면 정상 축소가 재개된다.

## 기능

`ScaleDownReconciler` 는 reconcile 초입에서 노드의 `onp.io/do-not-disrupt` 를 검사해, 있으면 `emptySince` 를 지우고 즉시 반환한다(축소 후보에서 배제). 03(파드 단위)이 "노드를 non-empty 로 만드는" 것과 달리, 이건 "노드 자체를 손대지 않는" 더 강한 제외다.

## 목적

운영자가 특정 노드(예: 디버깅 중, 특수 하드웨어)를 한 줄로 ONP 손에서 빼게 한다.

## 예상 결과

- 어노테이션 있는 동안: 빈 노드라도 `status.emptySince` 가 nil 로 유지(스탬프 자체가 안 됨), worker-1 `Ready` 유지.
- 어노테이션 제거 후: 정상 축소 재개(다시 `emptySince` 스탬프 → `consolidateAfter` 후 drain).

## 재현 방법

1. worker-1 Ready·비운 상태에서 노드 어노테이션:

   ```bash
   kubectl annotate node worker-1 onp.io/do-not-disrupt=true --overwrite
   kubectl patch nodepool demo --type merge -p \
     '{"spec":{"minNodes":0,"disruption":{"consolidationPolicy":"WhenEmpty","consolidateAfter":"60s"}}}'
   ```

2. 90초+ 관찰 — `emptySince` 가 계속 비어 있고 안 꺼져야 함:

   ```bash
   kubectl get machine worker-1 -o jsonpath='{.status.state}{" emptySince="}{.status.emptySince}{"\n"}'
   ```

3. 어노테이션 제거로 재개 확인:

   ```bash
   kubectl annotate node worker-1 onp.io/do-not-disrupt-
   # 축소가 재개돼 emptySince 가 다시 붙는다(워크로드/머신 이벤트가 reconcile 를 트리거할 때).
   kubectl get machine worker-1 -o jsonpath='{.status.emptySince}{"\n"}'
   ```

> scale-down 컨트롤러는 **Pod/Machine 이벤트**로 reconcile 한다(노드 어노테이션 변경엔 직접 트리거되지 않음). 제거 직후 재개가 안 보이면 파드 하나를 띄웠다 지워 reconcile 를 깨우거나 잠시 기다린다.
