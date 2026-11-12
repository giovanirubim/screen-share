package main

import (
	"fmt"
	"net"
	"sort"
)

type viewerLink struct {
	URL  string `json:"url"`
	Note string `json:"note"`
	rank int
}

// addrInfo is one address on one network adapter. temporary is true for
// IPv6 privacy addresses, which Windows replaces about once a day.
type addrInfo struct {
	ip        net.IP
	iface     string
	temporary bool
}

func findViewerLinks(port int) []viewerLink {
	var links []viewerLink
	seen := map[string]bool{}
	for _, a := range localAddrs() {
		if !keepInterface(a.iface) || a.ip.IsLoopback() || a.ip.IsLinkLocalUnicast() {
			continue
		}
		var l viewerLink
		if ip4 := a.ip.To4(); ip4 != nil {
			if !ip4.IsPrivate() {
				l = viewerLink{URL: fmt.Sprintf("http://%s:%d/", ip4, port), Note: "public IPv4", rank: 1}
			} else {
				l = viewerLink{URL: fmt.Sprintf("http://%s:%d/", ip4, port), Note: "same Wi-Fi/LAN only", rank: 3}
			}
		} else {
			if a.ip.IsPrivate() { // fc00::/7 unique-local
				continue
			}
			l = viewerLink{URL: fmt.Sprintf("http://[%s]:%d/", a.ip, port), Note: "IPv6, permanent address - share this one", rank: 0}
			if a.temporary {
				l.Note, l.rank = "IPv6, temporary (changes daily)", 2
			}
		}
		if !seen[l.URL] {
			seen[l.URL] = true
			links = append(links, l)
		}
	}
	sort.SliceStable(links, func(i, j int) bool { return links[i].rank < links[j].rank })
	return links
}
