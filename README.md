# snout

Snout is a local hypervisor agent that synchronizes the Open vSwitch FDB with BGP EVPN Type-2 routes through GoBGP.

Snout reads `snout.toml` from its working directory by default. Use `-c` to select another file. The file is required: it defines the local VTEP and the bridges and VNIs Snout serves.

```toml
gobgp_grpc_socket    = "127.0.0.1:50051"   # default
log_level            = "info"              # default
syslog_facility      = "local0"            # default
bridge_scan_interval = "5s"                # default
ovs_poll_interval    = "5s"                # default
vtep                 = "172.31.0.11"       # required: local VTEP, EVPN next hop
asn                  = 65000               # used for default route targets

[[bridge]]
name       = "br0"
vni_offset = 10000                         # VNI = VLAN + vni_offset
vlans      = [100, 200, "300-310"]         # active VLANs
```

Command-line options override values from TOML:

```text
--gobgp-grpc-socket 127.0.0.1:50051
--log-level info
--syslog-facility local0
--bridge-scan-interval 5s
```

Use `-f` to stay in the foreground. Without `-f`, Snout starts as a daemon. `-h` prints command help and `-v` prints the current version. Application logs are written to syslog; the default level is `info` and the default facility is `local0`.

### Bridges and VNIs

Every `[[bridge]]` is in one of two modes.

**VLAN-aware bridge** (`vni_offset`, `vlans` and/or `[[bridge.vni]]`): each active VLAN is the L2 domain of one VNI. Workload ports are access ports (`tag=<vlan>`); physical uplinks to other L2 networks are trunks with an explicit `trunks=` list. OVS `NORMAL` switching keeps VLANs, and therefore VNIs, isolated. Untagged workload ports are not supported in this mode, because an OVS port without `tag` and `trunks` is a trunk for all VLANs.

- A VLAN listed in `vlans` maps to `VNI = VLAN + vni_offset`.
- `[[bridge.vni]]` entries are needed only for exceptions: a VNI outside the offset rule, a custom RD or route target, or activating a VLAN that is not in `vlans`. An explicit entry wins over the offset.

```toml
[[bridge]]
name       = "br0"
vni_offset = 10000
vlans      = [100, 200]

  [[bridge.vni]]          # VLAN 200 -> VNI 5000 instead of 10200
  vlan = 200
  vni  = 5000
  route_target = "65000:777"
```

**Flat bridge** (`vni`): one VNI and no VLANs. Workload ports are untagged and the bridge passes every frame, tagged ones included.

```toml
[[bridge]]
name = "br-flat"
vni  = 100
```

Defaults: the route distinguisher is `<vtep>:<vlan>` on a VLAN-aware bridge and `<vtep>:<vni>` on a flat bridge; the route target is `<asn>:<vni>`. `rd` and `route_target` can be overridden on a flat bridge or in a `[[bridge.vni]]` entry. With a four-byte `asn` (above 65535), every VNI needs an explicit `route_target`. Snout refuses to start when VNIs or RDs collide, VLANs are out of range, or unknown settings are present.

Configured bridges are scanned periodically. A bridge is synchronized whenever it exists in Open vSwitch; a pre-created VXLAN interface is not required.

Remote EVPN routes are applied to the bridge and VLAN that own their VNI: Snout creates a fixed VXLAN port for the VNI/VTEP (an access port of the VLAN on a VLAN-aware bridge) and installs a static FDB entry in that VLAN. Snout marks its ports with `external_ids:snout-owned=true`, `external_ids:snout-vni`, and `external_ids:snout-vtep`; user-created VXLAN ports are not changed or removed. Local non-VXLAN FDB entries in active VLANs are announced through GoBGP with the RD and route target of their VNI; when an entry disappears, its path is withdrawn.

Snout advertises EVPN Type-3 IMET membership for every active VNI of every existing bridge. Remote Type-3 routes create ingress-replication targets before any Type-2 MAC is learned. VXLAN ports are marked as OVS protected ports so BUM traffic can pass between local and tunnel ports but cannot be forwarded from one tunnel port to another.

The fixed-port BUM dataplane relies on OVS `NORMAL` switching. A bridge controlled by custom OpenFlow rules must provide equivalent local flooding and forwarding behavior; Snout does not install OpenFlow rules in this mode.

On `SIGINT` or `SIGTERM`, Snout stops accepting events, withdraws its local Type-2 routes followed by its local Type-3 route, removes its remote FDB entries and owned VXLAN ports, and then closes GoBGP. Graceful shutdown has a shared 15-second timeout; a repeated signal forces a non-zero exit.

Static FDB programming uses the `fdb/add` and `fdb/del` commands available in Open vSwitch 2.17 and newer.

For a quick local Type-2 announcement/withdrawal test, use `scripts/fake-local-mac.sh`. It creates an internal OVS port (an access port of `VLAN`, or untagged with `VLAN=0` on a flat bridge) and adds a static FDB entry, which does not age out:

```sh
sudo scripts/fake-local-mac.sh add
sudo VLAN=200 PORT=fake-vm1 MAC=02:00:00:00:00:43 scripts/fake-local-mac.sh add
sudo scripts/fake-local-mac.sh del
sudo scripts/fake-local-mac.sh cleanup
```

The defaults are `BRIDGE=br0`, `PORT=fake-vm0`, `MAC=02:00:00:00:00:42` and `VLAN=100`. `VLAN` must be an active VLAN of the bridge in `snout.toml`; MACs in other VLANs are not announced. `cleanup` removes every port the script created on the bridge.

A four-VM test lab (three hypervisors and a GoBGP route reflector) with bootstrap and update scripts is described in [`docs/lab.md`](docs/lab.md).

The minimum requirements for BUM forwarding are documented in [`docs/minimal-bum.md`](docs/minimal-bum.md). The planned flow-based VXLAN/OpenFlow implementation is tracked in [`TODO.md`](TODO.md).
