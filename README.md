# snout

Snout is a local hypervisor agent that synchronizes the Open vSwitch FDB with BGP EVPN Type-2 routes through GoBGP.

Snout reads `snout.toml` from its working directory by default. Use `-c` to select another file. Command-line options override values from TOML:

```toml
gobgp_grpc_socket = "127.0.0.1:50051"
log_level = "info"
syslog_facility = "local0"
bridge_list = ["br0", "br1"]
bridge_scan_interval = "5s"
```

The equivalent CLI options are:

```text
--gobgp-grpc-socket 127.0.0.1:50051
--log-level info
--syslog-facility local0
--bridge-list br0,br1
--bridge-scan-interval 5s
```

Use `-f` to stay in the foreground. Without `-f`, Snout starts as a daemon. `-h` prints command help and `-v` prints the current version. Application logs are written to syslog; the default level is `info` and the default facility is `local0`.

Bridges from `bridge_list` are scanned periodically. A bridge is synchronized whenever it exists in Open vSwitch and the VNI, RD, route target, and local VTEP configuration is valid. A pre-created VXLAN interface is not required. OVS synchronization and local-MAC announcements currently also require:

- `SNOUT_VNI` — VXLAN VNI;
- `SNOUT_VTEP` — local VTEP IP used as the EVPN next hop;
- `SNOUT_RD` — route distinguisher, for example `65000:100`;
- `SNOUT_ROUTE_TARGET` — route target in ASN form, for example `65000:100`.

Optional settings:

- `SNOUT_OVS_POLL_INTERVAL` (default `5s`).

Remote EVPN routes create a fixed VXLAN port for their VNI/VTEP and install a static FDB entry. Snout marks its ports with `external_ids:snout-owned=true`, `external_ids:snout-vni`, and `external_ids:snout-vtep`; user-created VXLAN ports are not changed or removed. Local non-VXLAN FDB entries are announced through GoBGP; when an entry disappears, its path is withdrawn.

Snout advertises EVPN Type-3 IMET membership for active VXLAN bridges. Remote Type-3 routes create ingress-replication targets before any Type-2 MAC is learned. VXLAN ports are marked as OVS protected ports so BUM traffic can pass between local and tunnel ports but cannot be forwarded from one tunnel port to another.

The fixed-port BUM dataplane relies on OVS `NORMAL` switching. A bridge controlled by custom OpenFlow rules must provide equivalent local flooding and forwarding behavior; Snout does not install OpenFlow rules in this mode.

On `SIGINT` or `SIGTERM`, Snout stops accepting events, withdraws its local Type-2 routes followed by its local Type-3 route, removes its remote FDB entries and owned VXLAN ports, and then closes GoBGP. Graceful shutdown has a shared 15-second timeout; a repeated signal forces a non-zero exit.

Static FDB programming uses the `fdb/add` and `fdb/del` commands available in Open vSwitch 2.17 and newer.

For a quick local Type-2 announcement/withdrawal test, use `scripts/fake-local-mac.sh`. It creates an internal OVS port and adds a synthetic local FDB entry:

```sh
sudo scripts/fake-local-mac.sh add
sudo scripts/fake-local-mac.sh del
sudo scripts/fake-local-mac.sh cleanup
```

The defaults are `BRIDGE=br0`, `PORT=fake-vm0`, and `MAC=02:00:00:00:00:42`. Override them as environment variables. If the bridge has no VXLAN interface, set `CREATE_VXLAN=1` to let the script create a temporary flow-based test interface.

The minimum requirements for BUM forwarding are documented in [`docs/minimal-bum.md`](docs/minimal-bum.md). The planned flow-based VXLAN/OpenFlow implementation is tracked in [`TODO.md`](TODO.md).
