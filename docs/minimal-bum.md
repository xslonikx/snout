# Minimal BUM forwarding requirements

This document defines the minimum behavior required for broadcast, unknown unicast, and multicast traffic in the current fixed VXLAN-port dataplane. Selective multicast is outside this milestone.

## Control plane

1. Snout must support EVPN Type-3 Inclusive Multicast Ethernet Tag (IMET) routes in addition to the existing Type-2 routes.
2. When a configured OVS bridge becomes active for a VNI, Snout must advertise one local Type-3 route containing the VNI and local VTEP.
3. A configured bridge is active when it exists in OVS and the VNI configuration is valid; a pre-existing VXLAN interface is not required. When the bridge disappears, the VNI is disabled, or Snout shuts down normally, Snout must withdraw its local Type-3 route.
4. Snout must decode remote Type-3 announcements and withdrawals into internal application events. GoBGP protobuf types must remain inside `internal/gobgp`.
5. A Type-3 route identifies VTEP membership in a VNI. It must not be treated as a MAC route or added to the OVS FDB.
6. Snout must ignore its own Type-3 routes and routes for VNIs not configured on the local bridge.
7. Type-2 and Type-3 state must be maintained independently:
   - Type-2: `(VNI, MAC) -> remote VTEP`;
   - Type-3: `VNI -> set of remote VTEPs`.

## Fixed-port dataplane

1. A remote Type-3 announcement must create or retain one fixed VXLAN port for `(bridge, VNI, remote VTEP)` with:

   ```text
   type=vxlan
   options:remote_ip=<remote VTEP>
   options:key=<VNI>
   ```

2. The port must exist even when no Type-2 MAC route has been received from that VTEP. This supplies the replication target needed for ARP, IPv6 ND, DHCP, unknown unicast, and multicast.
3. Every port created by Snout must carry `external_ids:snout-owned=true`, `external_ids:snout-vni=<VNI>`, and `external_ids:snout-vtep=<remote VTEP>`. User-created VXLAN ports must not be changed or deleted.
4. All Type-2 routes for the same `(bridge, VNI, remote VTEP)` must reuse that port.
5. A Type-3 withdrawal must remove the remote VTEP from the VNI flood list. The VXLAN port may be deleted only after both conditions are true:
   - the Type-3 membership is absent;
   - no installed Type-2 route still references the port.
6. A Type-2 withdrawal must remove only the MAC FDB entry. It must not remove a port that is retained by Type-3 membership or another Type-2 route.
7. Repeated announcements and withdrawals must be idempotent.

## Flooding and split horizon

1. BUM traffic received from a local workload port must be replicated to:
   - other eligible local workload ports;
   - each remote VTEP present in the Type-3 flood list for the VNI.
2. BUM traffic received from a VXLAN port must be delivered only to eligible local workload ports.
3. A frame received from one VXLAN port must never be forwarded to another VXLAN port. This split-horizon rule is mandatory to prevent loops and repeated replication.
4. Replication must remain within the same VNI/broadcast domain. Traffic must not leak between VNIs or unrelated bridges.
5. The minimal implementation uses ingress/head-end replication. It must not depend on multicast support in the underlay.

## Multicast scope

The minimal implementation floods all L2 multicast to every remote VTEP participating in the VNI. It does not yet optimize delivery based on receivers.

The following are explicitly deferred:

- IGMP and MLD membership tracking;
- selective multicast forwarding;
- EVPN Type-6, Type-7, and Type-8 routes;
- multicast groups in the physical underlay.

## Recovery and lifecycle

1. After startup or GoBGP reconnection, current Type-3 and Type-2 routes must reconstruct the flood list, tunnel ports, and FDB state without waiting for new route changes.
2. When a configured bridge appears in OVS, stored Type-3 membership and Type-2 routes must be applied immediately.
3. When a configured bridge disappears, Snout must stop programming it and withdraw locally originated routes for that bridge/VNI.
4. Errors affecting one bridge or VTEP must be logged and must not stop synchronization of unrelated bridges.

## Minimum integration tests

The integration environment must verify at least:

1. A remote Type-3 route creates a VXLAN replication port before any Type-2 route exists.
2. ARP broadcast reaches a remote workload through that port.
3. Unknown unicast and an Ethernet multicast frame reach all remote VTEPs in the same VNI.
4. A VXLAN-ingress frame is not forwarded to another VXLAN port.
5. Type-2 add/withdraw updates the FDB without prematurely deleting the Type-3 port.
6. Type-3 withdrawal removes the replication target when no Type-2 reference remains.
7. Traffic does not cross VNI boundaries.
8. Restarting Snout reconstructs the same dataplane state.
