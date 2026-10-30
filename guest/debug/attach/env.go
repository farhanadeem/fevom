package attach

import (
	"strings"

	"fev/proto/debugstore"
)

const (
	BinDir   = "/nix/var/fev/bin"
	HomeDir  = "/nix/var/fev/home"
	CABundle = debugstore.EtcDir + "/ssl/ca-bundle.crt"
)

const defaultTargetPath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

func BuildEnv(targetEnv []string, term, name string) []string {
	targetPath := defaultTargetPath
	for _, e := range targetEnv {
		if v, ok := strings.CutPrefix(e, "PATH="); ok && v != "" {
			targetPath = v
		}
	}
	if term == "" {
		term = envValue(targetEnv, "TERM")
	}
	if term == "" {
		term = "xterm-256color"
	}
	over := []string{
		"PATH=" + debugstore.ProfileDir + "/bin:" + BinDir + ":" + targetPath,
		"HOME=" + HomeDir,
		"TERM=" + term,
		"ZDOTDIR=" + debugstore.EtcDir,
		"TERMINFO_DIRS=" + debugstore.EtcDir + "/terminfo:" + debugstore.ProfileDir + "/share/terminfo",
		"LANG=C.UTF-8",
		"SSL_CERT_FILE=" + CABundle,
		"NIX_SSL_CERT_FILE=" + CABundle,
		"EDITOR=vim",
		"PAGER=less",
		"FEV_DEBUG_TARGET=" + name,
	}
	return overrideEnv(targetEnv, over)
}

func envValue(env []string, key string) string {
	for _, e := range env {
		if v, ok := strings.CutPrefix(e, key+"="); ok {
			return v
		}
	}
	return ""
}

func overrideEnv(base, over []string) []string {
	out := append([]string{}, base...)
	index := map[string]int{}
	for i, e := range out {
		if k, _, ok := strings.Cut(e, "="); ok {
			index[k] = i
		}
	}
	for _, e := range over {
		k, _, _ := strings.Cut(e, "=")
		if i, ok := index[k]; ok {
			out[i] = e
			continue
		}
		index[k] = len(out)
		out = append(out, e)
	}
	return out
}
