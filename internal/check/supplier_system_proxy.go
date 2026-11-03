package check

import (
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

type supplierSystemProxy struct {
	URL    string
	HTTP   string
	Bypass []string
}

var supplierSystemCache struct {
	sync.Mutex
	value supplierSystemProxy
	until time.Time
}

// supplierDefaultProxy follows environment then OS settings, with loopback and
// local network targets kept direct just as Magpie's global proxy does.
func supplierDefaultProxy(r *http.Request) (*url.URL, error) {
	host := strings.ToLower(r.URL.Hostname())
	if supplierLocalHost(host) {
		return nil, nil
	}
	proxy, err := http.ProxyFromEnvironment(r)
	if proxy != nil || err != nil {
		return proxy, err
	}
	all := os.Getenv("ALL_PROXY")
	if all == "" {
		all = os.Getenv("all_proxy")
	}
	no := os.Getenv("NO_PROXY")
	if no == "" {
		no = os.Getenv("no_proxy")
	}
	if all != "" && !supplierProxyBypassed(host, strings.Split(no, ",")) {
		return supplierProxyURL(all)
	}
	supplierSystemCache.Lock()
	if time.Now().After(supplierSystemCache.until) {
		supplierSystemCache.value = readSupplierSystemProxy()
		supplierSystemCache.until = time.Now().Add(15 * time.Second)
	}
	sys := supplierSystemCache.value
	supplierSystemCache.Unlock()
	if supplierProxyBypassed(host, sys.Bypass) {
		return nil, nil
	}
	choice := sys.URL
	if r.URL.Scheme == "http" && sys.HTTP != "" {
		choice = sys.HTTP
	}
	if choice == "" {
		return nil, nil
	}
	return supplierProxyURL(choice)
}

func supplierProxyURL(raw string) (*url.URL, error) {
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	return url.Parse(raw)
}

func supplierLocalHost(host string) bool {
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && (ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast())
}

func supplierProxyBypassed(host string, list []string) bool {
	for _, raw := range list {
		pattern := strings.ToLower(strings.TrimSpace(raw))
		if pattern == "" {
			continue
		}
		if pattern == "*" || pattern == host || (pattern == "<local>" && !strings.Contains(host, ".")) {
			return true
		}
		if _, network, err := net.ParseCIDR(pattern); err == nil {
			if ip := net.ParseIP(host); ip != nil && network.Contains(ip) {
				return true
			}
			continue
		}
		pattern = strings.TrimPrefix(pattern, "*")
		pattern = strings.TrimPrefix(pattern, ".")
		if h, _, err := net.SplitHostPort(pattern); err == nil {
			pattern = h
		}
		if host == pattern || strings.HasSuffix(host, "."+pattern) {
			return true
		}
	}
	return false
}
