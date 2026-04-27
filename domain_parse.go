package main

import (
	"fmt"
	"net/url"
	"strings"

	"golang.org/x/net/publicsuffix"
)

func hostFromURLString(urlStr string) (string, error) {
	u, err := url.Parse(urlStr)
	if err != nil {
		return "", err
	}
	h := u.Hostname()
	if h == "" {
		return "", fmt.Errorf("empty host")
	}
	return strings.ToLower(h), nil
}

// registrableDomainFromHost returns the eTLD+1 (registrable / "site") for a host,
// e.g. en.wikipedia.org -> wikipedia.org. Falls back to the host on lookup failure.
func registrableDomainFromHost(host string) string {
	h := strings.ToLower(strings.TrimSpace(host))
	if h == "" {
		return ""
	}
	etld1, err := publicsuffix.EffectiveTLDPlusOne(h)
	if err != nil || etld1 == "" {
		return h
	}
	return etld1
}

func resolveHostAndSite(urlStr string) (host string, site string, err error) {
	host, err = hostFromURLString(urlStr)
	if err != nil {
		return "", "", err
	}
	return host, registrableDomainFromHost(host), nil
}
