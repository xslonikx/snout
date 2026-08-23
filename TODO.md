# TODO

## BUM and multicast

- [ ] Implement the minimal Type-3 IMET and fixed-port BUM behavior defined in [`docs/minimal-bum.md`](docs/minimal-bum.md).

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
