package images

import (
	"fmt"
	"strings"

	"fev/proto/agent"
)

const Arch = "arm64"

const AMD64 = "amd64"

var archNames = map[string]string{
	"arm64": Arch, "aarch64": Arch,
	"amd64": AMD64, "x86_64": AMD64,
}

func Architecture(name string) (string, error) {
	if name == "" {
		return Arch, nil
	}
	if a, ok := archNames[strings.ToLower(name)]; ok {
		return a, nil
	}
	if len(name) > 40 {
		name = name[:40]
	}
	return "", fmt.Errorf("no architecture %q: a machine is arm64 (the Mac's own) or amd64 (under x86 emulation)", name)
}

func KernelArch(arch string) string {
	switch arch {
	case Arch:
		return "aarch64"
	case AMD64:
		return "x86_64"
	}
	return arch
}

var Defaults = map[string]string{
	"ubuntu":     "resolute",
	"debian":     "trixie",
	"fedora":     "44",
	"alpine":     "3.24",
	"archlinux":  "current",
	"almalinux":  "10",
	"rockylinux": "10",
	"centos":     "10-Stream",
	"oracle":     "10",
	"opensuse":   "16.0",
	"kali":       "current",
	"devuan":     "excalibur",
	"gentoo":     "current",
	"voidlinux":  "current",
}

var Variants = map[string][]string{
	"voidlinux": {agent.VariantDefault, "musl"},
	"gentoo":    {"openrc", "systemd"},
}

var variantAliases = map[string]map[string]string{
	"voidlinux": {"glibc": agent.VariantDefault},
}

var distroAliases = map[string]string{
	"arch":  "archlinux",
	"void":  "voidlinux",
	"alma":  "almalinux",
	"rocky": "rockylinux",
}

var releaseAliases = map[string]map[string]string{
	"ubuntu": {"22.04": "jammy", "24.04": "noble", "26.04": "resolute"},
	"debian": {"12": "bookworm", "13": "trixie", "14": "forky"},
	"kali":   {"kali-rolling": "current", "rolling": "current"},
	"centos": {"9": "9-Stream", "10": "10-Stream"},
	"devuan": {"5": "daedalus", "6": "excalibur"},
}

var metaReleases = map[string]map[string]string{
	"kali": {"current": "kali-rolling"},
}

func Name(distro, release string) (string, string, error) {
	distro = strings.ToLower(distro)
	if d, ok := distroAliases[distro]; ok {
		distro = d
	}
	if !pathPart(distro) || (release != "" && !pathPart(release)) {
		return "", "", fmt.Errorf("%q:%q is not a distribution and release", distro, release)
	}
	if release == "" {
		def, ok := Defaults[distro]
		if !ok {
			return "", "", fmt.Errorf("fev has no default release of %s: name one (%s:RELEASE)", distro, distro)
		}
		return distro, def, nil
	}
	if code, ok := releaseAliases[distro][release]; ok {
		release = code
	}
	return distro, release, nil
}

type Image struct {
	Distro, Release, Arch, Variant string
}

func Resolve(distro, release, arch string, userData bool) (Image, error) {
	a, err := Architecture(arch)
	if err != nil {
		return Image{}, err
	}
	rel, variant, hasVariant := strings.Cut(release, ":")
	if strings.Contains(variant, ":") {
		return Image{}, fmt.Errorf("%q is not RELEASE[:VARIANT]", release)
	}
	d := strings.ToLower(distro)
	if a, ok := distroAliases[d]; ok {
		d = a
	}
	if !hasVariant && rel != "" && isVariant(d, rel) {
		rel, variant, hasVariant = "", rel, true
	}
	d, r, err := Name(d, rel)
	if err != nil {
		return Image{}, err
	}
	if hasVariant {
		if variant == "" {
			return Image{}, fmt.Errorf("%q names no variant after its ':'", release)
		}
		if v, ok := variantAliases[d][variant]; ok {
			variant = v
		}
		if !pathPart(variant) || strings.Contains(variant, ":") {
			return Image{}, fmt.Errorf("%q is not a variant", variant)
		}
		if variant == agent.VariantCloud {
			return Image{}, fmt.Errorf("the cloud build of %s is used when user data is given (-c), not by name", d)
		}
	} else {
		variant = BaseVariant(d)
	}
	if userData {
		if variant != agent.VariantDefault {
			return Image{}, fmt.Errorf("the image server has no %s %s %s image with cloud-init for %s, so a machine of it cannot take user data", d, r, variant, a)
		}
		variant = agent.VariantCloud
	}
	return Image{Distro: d, Release: r, Arch: a, Variant: variant}, nil
}

func BaseVariant(distro string) string {
	if v := Variants[distro]; len(v) > 0 {
		return v[0]
	}
	return agent.VariantDefault
}

func isVariant(distro, name string) bool {
	if _, ok := variantAliases[distro][name]; ok {
		return true
	}
	for _, v := range Variants[distro] {
		if v == name {
			return true
		}
	}
	return false
}
