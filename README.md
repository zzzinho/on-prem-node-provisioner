# ONP — On-Prem Node Provisioner

Pending 파드의 spec 을 보고 적합한 on-prem 물리 노드를 Wake-on-LAN 으로 깨우고, 비면 안전하게 drain 후 전원을 끄는 Kubernetes 컨트롤러. 전원 제어는 pluggable.

## 상태

**Phase 1 진행 중** — 아직 클러스터에 설치해 쓰는 단계는 아닙니다.

- ✅ **M1 — Walking Skeleton**: WoL 매직 패킷 빌더 + `wol-probe` CLI (표준 라이브러리만) + 멀티스테이지 Docker 이미지. 같은 L2 의 실제 꺼진 노드를 깨우는 것까지 end-to-end 검증 완료.
- ⏳ **M2 — 최소 컨트롤러**: `Machine` CRD + 컨트롤러 + `onp-wol-agent` 로 선언적 wake (다음 차례).

전체 마일스톤(M1 → M5)은 [`ROADMAP.md`](ROADMAP.md) 참조.

## 문서

- [`docs/DESIGN.md`](docs/DESIGN.md) — 설계 문서 (Google 스타일)
- [`CLAUDE.md`](CLAUDE.md) — 코드 작성 가이드
- [`ROADMAP.md`](ROADMAP.md) — 마일스톤 (M1 → M5)

## License

[Apache License 2.0](LICENSE)
