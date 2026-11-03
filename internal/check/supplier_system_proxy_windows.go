package check

import (
	"golang.org/x/sys/windows/registry"
	"strings"
)

func readSupplierSystemProxy() supplierSystemProxy {
	key, err := registry.OpenKey(registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\Internet Settings`, registry.QUERY_VALUE)
	if err != nil {
		return supplierSystemProxy{}
	}
	defer key.Close()
	enabled, _, err := key.GetIntegerValue("ProxyEnable")
	if err != nil || enabled == 0 {
		return supplierSystemProxy{}
	}
	server, _, err := key.GetStringValue("ProxyServer")
	if err != nil {
		return supplierSystemProxy{}
	}
	bypass, _, _ := key.GetStringValue("ProxyOverride")
	out := supplierSystemProxy{Bypass: strings.Split(bypass, ";")}
	if !strings.Contains(server, "=") {
		out.URL = server
		return out
	}
	values := map[string]string{}
	for _, item := range strings.Split(server, ";") {
		k, v, ok := strings.Cut(item, "=")
		if ok {
			values[strings.ToLower(strings.TrimSpace(k))] = strings.TrimSpace(v)
		}
	}
	out.HTTP = values["http"]
	for _, choice := range []string{values["https"], out.HTTP} {
		if choice != "" {
			out.URL = choice
			break
		}
	}
	if out.URL == "" && values["socks"] != "" {
		out.URL = "socks5://" + values["socks"]
	}
	return out
}
