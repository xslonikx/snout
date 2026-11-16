# TODO

## BUM and multicast

- [x] Implement Type-3 IMET publication/withdrawal, remote VTEP membership, fixed-port ingress replication, independent Type-2/Type-3 references, and protected-port split horizon as defined in [`docs/minimal-bum.md`](docs/minimal-bum.md).
- [ ] Validate the minimum BUM integration scenarios in a multi-hypervisor lab, including ARP, unknown unicast, Ethernet multicast, split horizon, withdrawal, VNI isolation, and restart recovery.

## Multi-VNI

- [x] Multi-VNI with flat and VLAN-aware bridges, static VLAN<->VNI map with `vni_offset` and explicit `[[bridge.vni]]` overrides (see [`docs/workflows/phase_1.txt`](docs/workflows/phase_1.txt)).
- [ ] Optionally derive the active VLANs from OVS access ports (`tag` of non-Snout ports, explicit `trunks`) instead of `vlans`, reusing `vni_offset`; announce/withdraw Type-3 when the first/last port of a VLAN appears/disappears.

## Flow-based VXLAN dataplane

- [ ] Replace per-`(VNI, remote VTEP)` fixed VXLAN ports with an optional OpenFlow-based dataplane that uses one existing flow-based VXLAN interface per bridge:

  ```text
  options:remote_ip=flow
  options:key=flow
  ```

- [ ] Discover and validate the configured flow-based VXLAN interface instead of creating a tunnel interface for every VTEP.
- [ ] Translate every remote Type-2 route into an owned OpenFlow unicast rule that matches the destination MAC and VNI/broadcast domain, sets `tun_id` to the VNI, sets `tun_dst` to the remote VTEP, and outputs to the shared VXLAN interface.
- [ ] Translate Type-3 IMET membership into OpenFlow ingress-replication rules or groups that make one copy of local BUM traffic for each remote VTEP, setting the appropriate `tun_id` and `tun_dst` for every copy.
- [ ] Enforce split horizon in OpenFlow: traffic received from the VXLAN interface may reach local workload ports but must never be re-encapsulated to a remote VTEP.
- [ ] Define dedicated OpenFlow tables, priorities, and a Snout-specific cookie namespace. Snout must modify and delete only flows and groups it owns.
- [ ] Preserve unrelated controller flows and detect/report table or cookie conflicts.
- [ ] Make add, replace, withdraw, restart recovery, and MAC mobility operations idempotent.
- [ ] Remove owned flows and groups safely when a route is withdrawn, a VNI/bridge is disabled, or Snout exits.
- [ ] Add unit tests for exact `ovs-ofctl` commands and integration tests for unicast, ARP/ND, unknown unicast, multicast replication, split horizon, MAC mobility, withdrawal, and restart recovery.

The existing fixed-port implementation should remain available until the OpenFlow dataplane passes the same integration scenarios.
