# Snout test lab

This guide sets up and uses the four-VM lab defined in
[`workflows/phase_1.txt`](workflows/phase_1.txt): three pseudo-hypervisors
that run Open vSwitch, GoBGP and Snout, and one GoBGP route reflector. No real
VMs run on the hypervisors; "workloads" are OVS internal ports and synthetic
FDB entries.

```text
                     management network (default route, internet)
        ─────────┬────────────────┬────────────────┬────────────────┬──────
                 │                │                │                │
            ┌────┴────┐      ┌────┴────┐      ┌────┴────┐      ┌────┴────┐
            │ snout-rr│      │snout-hv1│      │snout-hv2│      │snout-hv3│
            │  GoBGP  │      │ OVS br0 │      │ OVS br0 │      │ OVS br0 │
            │   RR    │      │ GoBGP   │      │ GoBGP   │      │ GoBGP   │
            │         │      │ Snout   │      │ Snout   │      │ Snout   │
            └────┬────┘      └────┬────┘      └────┬────┘      └────┬────┘
              .10│             .11│             .12│             .13│
        ─────────┴────────────────┴────────────────┴────────────────┴──────
                     lab network 172.31.0.0/24 (no gateway)
                     iBGP AS 65000 l2vpn-evpn, VXLAN UDP 4789
```

| Segment | VLAN on `br0` | VNI   | Test port | Test addresses   |
|---------|---------------|-------|-----------|------------------|
| A       | 100           | 10100 | `test100` | 10.100.0.11-13/24 |
| B       | 200           | 10200 | `test200` | 10.200.0.11-13/24 |

## 1. Prepare the VMs

Create four VMs with **AlmaLinux 10** or **Oracle Linux 10** (minimal install
is enough, 2 vCPU / 2 GB RAM each):

| VM        | Lab address    | NICs |
|-----------|----------------|------|
| snout-rr  | 172.31.0.10/24 | management + lab |
| snout-hv1 | 172.31.0.11/24 | management + lab (+ optional trunk) |
| snout-hv2 | 172.31.0.12/24 | management + lab (+ optional trunk) |
| snout-hv3 | 172.31.0.13/24 | management + lab (+ optional trunk) |

- **Management NIC:** DHCP or static, with the default route and internet
  access. The scripts download packages, GoBGP, Go modules and the Snout source
  through it, and never change it.
- **Lab NIC:** all four VMs on one isolated L2 network. Leave it unconfigured;
  the bootstrap gives it the static lab address without gateway or DNS.
- **Trunk NIC (optional, hypervisors only):** a third NIC that becomes a VLAN
  trunk of `br0`, to connect hosts outside the lab to the overlay. The
  virtualization host must let it send and receive foreign MAC addresses
  (promiscuous mode / MAC spoofing / forged transmits allowed).

Find the NIC names on each VM with `ip -br link` and `ip route show default`
(the NIC in the default route is the management NIC).

## 2. Give the hypervisors read access to the repository

`snout-update.sh` clones `git@github.com:xslonikx/snout.git` as **root**. On
each hypervisor:

```sh
sudo ssh-keygen -t ed25519 -N '' -f /root/.ssh/id_ed25519
sudo cat /root/.ssh/id_ed25519.pub
```

Add the key as a read-only deploy key of the repository (or to an account that
can read it), then check:

```sh
sudo ssh -o StrictHostKeyChecking=accept-new -T git@github.com
```

## 3. Copy the lab scripts and fill in the NIC names

From a checkout of the repository on your workstation:

```sh
for vm in snout-rr snout-hv1 snout-hv2 snout-hv3; do
    scp -r misc root@$vm:/root/snout-lab
done
```

On every VM, set `UPLINK_IF` in its env file to the lab NIC name:

| VM        | Env file                       |
|-----------|--------------------------------|
| snout-rr  | `/root/snout-lab/lab/rr.env`   |
| snout-hv1 | `/root/snout-lab/lab/hv1.env`  |
| snout-hv2 | `/root/snout-lab/lab/hv2.env`  |
| snout-hv3 | `/root/snout-lab/lab/hv3.env`  |

```sh
sed -i 's/^UPLINK_IF=.*/UPLINK_IF=ens19/' /root/snout-lab/lab/hv1.env
```

Instead of editing the file you can also pass it on the command line:
`UPLINK_IF=ens19 bash ...` or `--uplink-if ens19`. Precedence is
**CLI option > environment variable > env file**, so the same files work on
all VMs with different NIC names.

## 4. Bootstrap

Run the route reflector first, then the three hypervisors (in any order).

```sh
# snout-rr
sudo bash /root/snout-lab/bootstrap-route-reflector.sh --env /root/snout-lab/lab/rr.env

# snout-hv1, snout-hv2, snout-hv3 (use the matching env file)
sudo bash /root/snout-lab/bootstrap-hypervisor.sh --env /root/snout-lab/lab/hv1.env
```

Useful extra settings (environment variables or `--kebab-case` options):

| Setting | Purpose |
|---------|---------|
| `QA_PASSWORD=...` / `QA_SSH_KEY=/root/qa.pub` | log-in for the `QA` user (password-less sudo); the key is a file on the VM or the key text itself |
| `TRUNK_IF=ens20` | add the trunk NIC to `br0` |
| `SEGMENTS=100,200,300:5000` | VLANs; `vlan:vni` overrides the offset rule |
| `VNI_OFFSET=10000` | VNI = VLAN + offset |
| `OVERLAY_100=none` / `OVERLAY_100=192.168.100.11/24` | test port address per VLAN |
| `GOBGP_VERSION=4.10.0` | pin GoBGP instead of the latest release |
| `SNOUT_REF=my-branch` | build another branch, tag or commit |

What the hypervisor bootstrap does: creates the `QA` user, disables
firewalld, installs Open vSwitch (CentOS NFV SIG), NetworkManager-ovs, git,
make, golang and GoBGP, configures the lab NIC, creates `br0` and the
`test<vlan>` ports through NetworkManager, writes `/etc/snout/snout.toml`,
starts `gobgpd`, then builds and starts Snout with `snout-update.sh`. It ends
with pre/post checks; warnings such as "no tunnel to 172.31.0.12 yet" are
expected until all three hypervisors are up.

All scripts are idempotent: re-run them after changing a setting.

## 5. Check the lab

On **snout-rr**, three sessions must be `Establ`:

```sh
gobgp neighbor
```

On each **hypervisor**:

```sh
systemctl status snout gobgpd openvswitch
gobgp neighbor                         # session with 172.31.0.10 is Establ
gobgp global rib -a evpn multicast     # one Type-3 route per VNI per hypervisor (6)
ovs-vsctl show                         # 4 Snout tunnels: sn-* ports
ovs-vsctl --columns=name,options,external_ids find Interface external_ids:snout-owned=true
```

Every hypervisor has one `sn-*` VXLAN port per (VNI, neighbour): 2 VNIs x 2
neighbours = 4 ports, with `options:key` = VNI and `tag` = VLAN (check with
`ovs-vsctl list Port <name>`).

## 6. Test scenarios

### Unicast inside a segment

```sh
# on snout-hv1
ping -c 3 -I test100 10.100.0.12
ping -c 3 -I test200 10.200.0.13
```

After the first ping the MACs of the test ports are learned, announced as
Type-2 routes and installed as static FDB entries on the other hypervisors:

```sh
gobgp global rib -a evpn macadv        # on any hypervisor
ovs-appctl fdb/show br0                # remote MACs point to sn-* ports
```

### Synthetic VM MAC (no traffic needed)

`snout-update.sh` checks the repository out to `/usr/local/src/snout`, which
contains the helper:

```sh
# on snout-hv1: MAC 02:00:00:00:00:42 in VLAN 100
sudo VLAN=100 sh /usr/local/src/snout/scripts/fake-local-mac.sh add

# on snout-hv2 within ~5 s:
gobgp global rib -a evpn macadv | grep 02:00:00:00:00:42   # VNI 10100, next hop 172.31.0.11
ovs-appctl fdb/show br0 | grep 02:00:00:00:00:42           # VLAN 100, on a sn-* port

# on snout-hv1: withdraw and remove
sudo VLAN=100 sh /usr/local/src/snout/scripts/fake-local-mac.sh del
sudo sh /usr/local/src/snout/scripts/fake-local-mac.sh cleanup
```

### Segment isolation

```sh
# on snout-hv1: a MAC in VLAN 100 must never appear in VLAN 200
sudo VLAN=100 sh /usr/local/src/snout/scripts/fake-local-mac.sh add
# on snout-hv2
ovs-appctl fdb/show br0 | grep 02:00:00:00:00:42           # only VLAN 100

# broadcast in VLAN 100 must not reach VLAN 200
sudo tcpdump -ni test200 arp &                               # on snout-hv2
arping -c 3 -I test100 10.100.0.99                           # on snout-hv1: nothing on test200
```

### BUM replication and split horizon

```sh
sudo tcpdump -ni <lab NIC> udp port 4789      # on snout-hv2 and snout-hv3
arping -c 3 -I test100 10.100.0.99            # on snout-hv1
```

Each ARP request leaves snout-hv1 once per neighbour (head-end replication).
snout-hv2 and snout-hv3 receive it but do not re-send it to each other
(Snout's tunnel ports are OVS protected ports).

### Restart recovery

```sh
sudo systemctl restart snout                  # on any hypervisor
ovs-vsctl show; ovs-appctl fdb/show br0       # same tunnels and remote MACs within ~10 s
```

On a clean stop (`systemctl stop snout`) Snout withdraws its routes and
removes its tunnel ports and FDB entries; the other hypervisors withdraw that
VTEP from their flood lists.

### Trunk (optional)

With `TRUNK_IF` set, a host behind the trunk NIC that sends frames tagged
with VLAN 100 becomes part of segment A and can reach `10.100.0.11-13`.

## 7. Update Snout

```sh
sudo bash /root/snout-lab/snout-update.sh                     # latest main
sudo SNOUT_REF=my-branch bash /root/snout-lab/snout-update.sh # another ref
sudo SNOUT_SKIP_TESTS=1 bash /root/snout-lab/snout-update.sh  # skip go test
```

The script fetches the source to `/usr/local/src/snout`, checks that the
installed Go matches `go.mod`, runs `go test ./...`, builds, then stops
`snout`, keeps the old binary as `/usr/local/sbin/snout.prev`, installs the
new one and starts the service. If the build or tests fail, the running
Snout is not touched; if the new binary does not start, the previous one is
restored. It never changes `/etc/snout/snout.toml`.

## 8. Change the segments

Edit `SEGMENTS` / `VNI_OFFSET` in the env file (or pass them) and re-run the
hypervisor bootstrap on every hypervisor. It updates the test ports
(removing ports of dropped VLANs), rewrites `/etc/snout/snout.toml` (the
previous version is kept as `snout.toml.bak`) and restarts Snout through
`snout-update.sh`. Use the same `SEGMENTS` on all hypervisors: a VNI only
connects hypervisors that all have it.

## 9. Files on the VMs

| Path | Content |
|------|---------|
| `/etc/snout/snout.toml` | Snout configuration (generated by the bootstrap) |
| `/usr/local/sbin/snout`, `snout.prev` | current and previous binary |
| `/etc/systemd/system/snout.service` | Snout unit (from `misc/snout.service`) |
| `/usr/local/src/snout` | source checkout used by `snout-update.sh` (reset on every update, do not edit there) |
| `/var/lib/snout/commit` | commit of the installed binary |
| `/etc/gobgp/gobgpd.toml`, `/etc/systemd/system/gobgpd.service` | GoBGP |
| `/usr/local/bin/gobgp`, `/usr/local/bin/gobgpd` | GoBGP binaries |
| `/etc/sudoers.d/90-qa` | sudo rule of the `QA` user |
| NetworkManager `lab-<nic>`, `ovs-bridge-br0`, `ovs-port-*`, `ovs-if-*`, `ovs-eth-*` | lab NIC, bridge, test ports, trunk |

## 10. Troubleshooting

| Symptom | Check |
|---------|-------|
| bootstrap stops with "required environment variable(s) not set" | `UPLINK_IF` empty in the env file, or a variable missing |
| "carries the default route" | `UPLINK_IF` points to the management NIC |
| `git clone failed` | root's SSH key is not a deploy key of the repository (section 2) |
| "Go X is older than go Y required by go.mod" | update the `golang` package |
| BGP session not `Establ` | `ping` the peer over the lab NIC; `journalctl -u gobgpd`; is the RR bootstrapped? |
| no `sn-*` ports | `journalctl -u snout`; the neighbours must run Snout with the same VNIs |
| MAC not announced | its VLAN must be in `SEGMENTS`; learned MACs age out after 300 s, use `fake-local-mac.sh` for persistent ones |
| ping between test ports fails | `ovs-appctl fdb/show br0`, `tcpdump -ni <lab NIC> udp port 4789`, MTU of the lab network |

Logs: `journalctl -u snout -f` (Snout logs to syslog facility `local0`),
`journalctl -u gobgpd -f`, `journalctl -u NetworkManager`.

## 11. Developing the lab scripts

The scripts share `misc/lib/lab-common.sh`. Their logic is covered by a
dependency-free test suite that needs no root, network or OVS:

```sh
bash misc/tests/run-tests.sh
shellcheck -x misc/*.sh misc/lib/*.sh misc/tests/*.sh
```
