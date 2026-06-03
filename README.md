# ONP — On-Prem Node Provisioner

Pending 파드의 spec 을 보고 적합한 on-prem 물리 노드를 Wake-on-LAN 으로 깨우고, 비면 안전하게 drain 후 전원을 끄는 Kubernetes 컨트롤러. 전원 제어는 pluggable.

## 상태

**Phase 1 진행 중** — Helm 차트로 설치는 가능하나, 설치 가이드·샘플 매니페스트 정비(M5)는 아직입니다.

- ✅ **M1 — Walking Skeleton**: WoL 매직 패킷 빌더 + `wol-probe` CLI (표준 라이브러리만) + 멀티스테이지 Docker 이미지. 같은 L2 의 실제 꺼진 노드를 깨우는 것까지 end-to-end 검증 완료.
- ✅ **M2 — 최소 컨트롤러**: `Machine` CRD + 컨트롤러 + `onp-wol-agent` 로 선언적 wake (`onp.io/wake-now` 어노테이션).
- ✅ **M3 — 자동 scale-up**: `NodePool` CRD + fit 시뮬레이션으로 pending 파드에 맞는 노드를 자동 wake (`maxNodes`/`cooldown` 가드 포함). 실하드웨어 E2E 검증 — wake 부터 파드 스케줄까지 ~40초.
- ⏳ **M4 — Safe Shutdown**: 빈 노드를 자동 drain(PDB 존중) 후 전원 차단 (다음 차례).

전체 마일스톤(M1 → M5)은 [`ROADMAP.md`](ROADMAP.md) 참조.

## 문서

- [`docs/DESIGN.md`](docs/DESIGN.md) — 설계 문서 (Google 스타일)
- [`CLAUDE.md`](CLAUDE.md) — 코드 작성 가이드
- [`ROADMAP.md`](ROADMAP.md) — 마일스톤 (M1 → M5)

## License

[Apache License 2.0](LICENSE)
