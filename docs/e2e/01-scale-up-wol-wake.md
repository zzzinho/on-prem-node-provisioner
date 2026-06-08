# 01 — 자동 scale-up / WoL wake

> **풀에 맞는 unschedulable 파드를 만들면 ONP 가 적합한 꺼진 Machine 을 골라 실제 Wake-on-LAN 매직 패킷을 쏘고, 노드가 부팅돼 Ready 가 되면 파드가 스케줄된다.** 관측: `Off→Booting→Ready`, `onp_power_on_total` +1, `onp_scale_up_latency_seconds` 기록(~45초).

## 기능

`scaleup` 컨트롤러가 pending·unschedulable 파드의 spec 을 보고, 꺼진 Machine 중 fit 하는 것을 골라 `onp.io/wake-now` 를 달아 PowerOn 한다(WoL provider → wol-agent 가 L2 브로드캐스트). 이게 ONP 의 "workload-aware proactive" 차별점이다.

## 목적

사용률 reactive 가 아니라 **pending 파드 spec 기반**으로 어느 노드를 깨울지 결정함을, 그리고 WoL 경로가 실하드웨어에서 실제로 노드를 켬을 확인한다.

## 예상 결과

- `up-probe` 파드 Pending(Unschedulable) → ONP 가 worker-1 선정 → Machine `Off→Booting→Ready`.
- worker-1 실제 부팅(노드 `Rebooted`/`NodeReady` 이벤트), kube-scheduler 가 파드 바인딩.
- `onp_power_on_total{provider="wol",result="success"}` +1.
- `onp_scale_up_latency_seconds_count` +1, `_sum` 에 power-on→Node Ready 초(관측치 ~44–48s) 누적.

## 재현 방법

1. 초기 상태 확인 — worker-1 이 `Off` 여야 함(아니면 02 로 끄거나 비우고 대기):

   ```bash
   kubectl get machine worker-1 -o jsonpath='{.status.state}{"\n"}'   # Off
   ```

2. 풀에 맞는, fit 가능한 unschedulable 파드 생성:

   ```bash
   kubectl run up-probe --image=nginx --restart=Never \
     --overrides='{"spec":{"nodeSelector":{"onp.io/pool":"demo"},
       "containers":[{"name":"up-probe","image":"nginx",
       "resources":{"requests":{"cpu":"100m","memory":"64Mi"}}}]}}'
   ```

3. wake 진행 관찰:

   ```bash
   kubectl get machine worker-1 -w          # Off -> Booting -> Ready
   kubectl get nodes worker-1 -w            # NotReady -> Ready
   ```

4. 파드 스케줄 + 메트릭 확인:

   ```bash
   kubectl get pod up-probe -o wide                       # NODE=worker-1, Running
   curl -s localhost:8080/metrics | grep -E \
     '^onp_power_on_total|^onp_scale_up_latency_seconds_(count|sum)'
   ```

5. 정리:

   ```bash
   kubectl delete pod up-probe
   ```

### 함정 — 깨웠는데 파드가 Pending

노드 `worker-1` 에 `onp.io/pool=demo` 라벨이 없으면 wake 는 일어나지만(fit 시뮬레이션은 `template.labels` 사용) 파드가 바인딩되지 않는다. 공통 전제의 노드 라벨을 확인한다.
