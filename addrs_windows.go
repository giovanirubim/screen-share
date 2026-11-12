package main

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

// localAddrs uses GetAdaptersAddresses because, unlike net.Interfaces, it
// reports which IPv6 addresses are temporary privacy addresses.
func localAddrs() []addrInfo {
	size := uint32(16 * 1024)
	var buf []byte
	for {
		buf = make([]byte, size)
		err := windows.GetAdaptersAddresses(windows.AF_UNSPEC, 0, 0, (*windows.IpAdapterAddresses)(unsafe.Pointer(&buf[0])), &size)
		if err == nil {
			break
		}
		if err != windows.ERROR_BUFFER_OVERFLOW {
			return nil
		}
	}
	var out []addrInfo
	for aa := (*windows.IpAdapterAddresses)(unsafe.Pointer(&buf[0])); aa != nil; aa = aa.Next {
		if aa.OperStatus != windows.IfOperStatusUp {
			continue
		}
		name := windows.UTF16PtrToString(aa.FriendlyName)
		for ua := aa.FirstUnicastAddress; ua != nil; ua = ua.Next {
			if ua.PreferredLifetime == 0 { // deprecated address
				continue
			}
			out = append(out, addrInfo{
				ip:        ua.Address.IP(),
				iface:     name,
				temporary: ua.SuffixOrigin == windows.IpSuffixOriginRandom,
			})
		}
	}
	return out
}
