# Static VXLAN lab

This directory configures a static VXLAN full mesh between Linux VTEPs `192.168.10.11`, `192.168.10.12`, `192.168.10.13`, and MikroTik `192.168.10.1`.

Defaults:

- Linux OVS bridge: `ovsbr0`;
- VNI: `100`;
- VXLAN UDP port: `4789`;
- overlay subnet: `10.100.0.0/24`.

Copy and run the matching script on each Linux host:

```sh
sudo ./host-192.168.10.11.sh
sudo ./host-192.168.10.12.sh
sudo ./host-192.168.10.13.sh
```

The bridge, VNI, and UDP port can be overridden through environment variables:

```sh
sudo env BRIDGE=ovsbr1 VNI=200 VXLAN_PORT=4789 ./host-192.168.10.11.sh
```

Import or paste `mikrotik.rsc` into a RouterOS 7 terminal. The MikroTik bridge name is independent of the Linux OVS bridge name.

Verify Linux forwarding with:

```sh
ovs-vsctl show
ovs-appctl fdb/show ovsbr0
tcpdump -ni any udp port 4789
```

Static full-mesh ports may duplicate the dynamic `sn-*` ports created by Snout from EVPN Type-3 IMET routes. Do not run both approaches on the same VNI during a baseline static VXLAN test.
