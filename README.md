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

Bridges from `bridge_list` are scanned periodically. A bridge is synchronized only while it contains a `type=vxlan` interface; ordinary bridges are logged at info level and ignored. OVS synchronization and local-MAC announcements currently also require:

- `SNOUT_VNI` — VXLAN VNI;
- `SNOUT_VTEP` — local VTEP IP used as the EVPN next hop;
- `SNOUT_RD` — route distinguisher, for example `65000:100`;
- `SNOUT_ROUTE_TARGET` — route target in ASN form, for example `65000:100`.

Optional settings:

- `SNOUT_OVS_POLL_INTERVAL` (default `5s`).

Remote EVPN routes create a VXLAN port for their VNI/VTEP and install a static FDB entry. Local non-VXLAN FDB entries are announced through GoBGP; when an entry disappears, its path is withdrawn.

Static FDB programming uses the `fdb/add` and `fdb/del` commands available in Open vSwitch 2.17 and newer.
