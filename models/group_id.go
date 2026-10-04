package models

import (
	"net/url"
	"strings"
)

// GroupPathID is WhatsApp-safe: @ in a URL is treated as a mention and the link is truncated.
func GroupPathID(groupID string) string {
	return strings.ReplaceAll(strings.TrimSpace(groupID), "@", ".")
}

// CanonicalGroupID restores a JID from a dashboard path or a WhatsApp-truncated URL.
func CanonicalGroupID(id string) string {
	id = strings.TrimSpace(id)
	if unesc, err := url.PathUnescape(id); err == nil {
		id = unesc
	}
	if strings.Contains(id, "@") {
		return id
	}
	lower := strings.ToLower(id)
	for _, suffix := range []string{".g.us", ".c.us", ".lid", ".s.whatsapp.net"} {
		if strings.HasSuffix(lower, suffix) {
			return id[:len(id)-len(suffix)] + "@" + suffix[1:]
		}
	}
	if id != "" && strings.Trim(id, "0123456789") == "" {
		return id + "@g.us"
	}
	return id
}

// GroupIDKeys are lookup aliases for a group/trip id from chat, dashboard, or a truncated URL.
func GroupIDKeys(id string) []string {
	id = strings.TrimSpace(id)
	canon := CanonicalGroupID(id)
	seen := map[string]bool{}
	var out []string
	add := func(v string) {
		v = strings.TrimSpace(v)
		if v == "" || seen[v] {
			return
		}
		seen[v] = true
		out = append(out, v)
	}
	add(id)
	add(canon)
	add(GroupPathID(canon))
	add(GroupPathID(id))
	return out
}
