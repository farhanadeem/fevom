package config

import (
	"errors"
	"fmt"
	"net/url"
	"strings"

	api "fev/proto/daemon"
)

func init() {
	checks[api.KeyDebugCacheURL] = checkDebugURL(api.KeyDebugCacheURL)
	checks[api.KeyDebugIndexURL] = checkDebugURL(api.KeyDebugIndexURL)
}

func checkDebugURL(key string) func(v any) (any, error) {
	return func(v any) (any, error) {
		s := strings.TrimSpace(v.(string))
		if s == "" {
			return "", nil
		}
		u, err := url.Parse(s)
		if err != nil {
			return nil, errors.New("is not a URL: " + s)
		}
		if strings.ToLower(u.Scheme) != "https" {
			if u.Scheme == "" {
				return nil, fmt.Errorf("needs a scheme: https://%s", s)
			}
			return nil, fmt.Errorf("has the scheme %q; a cache or index URL is https", u.Scheme)
		}
		if u.Host == "" || u.Hostname() == "" {
			return nil, errors.New("names no host")
		}
		if u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
			return nil, errors.New("has a query or a fragment; a cache or index URL is https://host[/path]")
		}
		u.Scheme = "https"
		return strings.TrimSuffix(u.String(), "/"), nil
	}
}
