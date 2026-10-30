package privhelper

import (
	"errors"
	"io/fs"
	"os"
	"sort"

	api "fev/proto/privhelper"
	"fev/proto/version"
)

func (s *Server) hello() (api.HelloResult, error) {
	r := api.HelloResult{
		ProtocolVersion: api.ProtocolVersion,
		SecurityLevel:   api.SecurityLevel,
		Build:           version.Build,
		UID:             s.rec.UID,
		Home:            s.rec.Home,
		Base:            s.rec.Base,
		Base6:           s.rec.Base6,
		ResolverPort:    s.rec.ResolverPort,
		LocalNames:      []string{},
		RoutesNAT64:     true,
	}
	r.Resolver = s.ownState()
	link, err := DockerSockState(s.path(api.DockerSockPath), dockerSockTarget(s.rec.Home))
	if err != nil {
		return api.HelloResult{}, err
	}
	r.DockerSock, r.DockerSockTarget = link.State, link.Target
	r.DockerSockLink = s.rec.DockerSockLink
	present, err := s.customEntries()
	if err != nil {
		return api.HelloResult{}, err
	}
	for n := range present {
		r.LocalNames = append(r.LocalNames, n)
	}
	sort.Strings(r.LocalNames)
	return r, nil
}

func resolverState(path string) string {
	ours, err := isFev(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return api.StateAbsent
	case err == nil && ours:
		return api.StateFev
	}
	if _, err := os.Lstat(path); errors.Is(err, fs.ErrNotExist) {
		return api.StateAbsent
	}
	return api.StateForeign
}
