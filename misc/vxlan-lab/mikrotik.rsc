/interface bridge
add name=br-vxlan100 protocol-mode=rstp

/interface vxlan
add name=vxlan100 vni=100 port=4789 local-address=192.168.10.1 learning=yes

/interface bridge port
add bridge=br-vxlan100 interface=vxlan100

/interface vxlan vteps
add interface=vxlan100 remote-ip=192.168.10.11 comment=snout-11
add interface=vxlan100 remote-ip=192.168.10.12 comment=snout-12
add interface=vxlan100 remote-ip=192.168.10.13 comment=snout-13

/ip address
add address=10.100.0.1/24 interface=br-vxlan100 comment=vxlan100-overlay
