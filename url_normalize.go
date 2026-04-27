package main

import (
	"net/url"
	"os"
	"strings"
)

// junkQueryKeys are dropped unless STRIP_ALL_URL_QUERY is set (then the whole query goes).
var junkQueryKeys = map[string]struct{}{
	"utm_source": {}, "utm_medium": {}, "utm_campaign": {}, "utm_term": {}, "utm_content": {},
	"fbclid": {}, "gclid": {}, "mc_cid": {}, "mc_eid": {},
	"returnto": {}, "returntoquery": {},
	"ref": {}, "ref_src": {},
}

// normalizeURLString cleans the URL for storage and deduping. It always strips the fragment.
// By default only known junk query keys are removed. If env STRIP_ALL_URL_QUERY=true, the entire query string is removed.
func normalizeURLString(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	u.Fragment = ""

	if os.Getenv("STRIP_ALL_URL_QUERY") == "true" {
		u.RawQuery = ""
		return u.String(), nil
	}

	q := u.Query()
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	for _, k := range keys {
		if _, drop := junkQueryKeys[strings.ToLower(k)]; drop {
			q.Del(k)
		}
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}
