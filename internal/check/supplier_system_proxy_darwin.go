package check

import (
	"context"
	"net"
	"os/exec"
	"strings"
	"time"
)

func readSupplierSystemProxy() supplierSystemProxy {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	raw, err := exec.CommandContext(ctx, "scutil", "--proxy").Output()
	if err != nil {
		return supplierSystemProxy{}
	}
	fields := map[string]string{}
	var bypass []string
	inList := false
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if inList {
			if line == "}" {
				inList = false
			} else if _, v, ok := strings.Cut(line, " : "); ok {
				bypass = append(bypass, v)
			}
			continue
		}
		k, v, ok := strings.Cut(line, " : ")
		if !ok {
			continue
		}
		if k == "ExceptionsList" {
			inList = true
			continue
		}
		fields[k] = v
	}
	entry := func(name, scheme string) string {
		if fields[name+"Enable"] != "1" || fields[name+"Proxy"] == "" {
			return ""
		}
		host := fields[name+"Proxy"]
		if port := fields[name+"Port"]; port != "" {
			host = net.JoinHostPort(host, port)
		}
		return scheme + "://" + host
	}
	out := supplierSystemProxy{HTTP: entry("HTTP", "http"), Bypass: bypass}
	for _, choice := range []string{entry("HTTPS", "http"), out.HTTP, entry("SOCKS", "socks5")} {
		if choice != "" {
			out.URL = choice
			break
		}
	}
	return out
}
