//go:build !windows

package main

import "net"

func localAddrs() []addrInfo {
	ifaces, _ := net.Interfaces()
	var out []addrInfo
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 {
			continue
		}
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			if n, ok := a.(*net.IPNet); ok {
				out = append(out, addrInfo{ip: n.IP, iface: ifc.Name})
			}
		}
	}
	return out
}
