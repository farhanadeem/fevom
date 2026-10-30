//go:build !darwin

package images

import "net/http"

var macProxy = http.ProxyFromEnvironment
