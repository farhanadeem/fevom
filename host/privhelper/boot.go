package privhelper

import (
	"errors"
	"fmt"

	api "fev/proto/privhelper"
)

func (s *Server) BootPass() error {
	var errs []error
	removed := 0
	defer func() {
		s.logf("boot pass: %d custom-name entries removed, the link %s", removed, map[bool]string{true: "asked for", false: "not asked for"}[s.rec.DockerSockLink])
	}()
	present, err := s.customEntries()
	if err != nil {
		errs = append(errs, err)
	}
	for n := range present {
		if err := s.removeCustom(n); err != nil {
			errs = append(errs, err)
		} else {
			removed++
			s.logf("boot: removed the entry for %s", n)
		}
	}
	if err := holdBases(s.sys, recordedHeld(s.rec)); err != nil {
		errs = append(errs, err)
	}
	if err := s.ensureOwn(); err != nil {
		errs = append(errs, fmt.Errorf("fev.local's entry: %w", err))
	}
	if s.rec.DockerSockLink {
		st, err := LinkDockerSock(s.path(api.DockerSockPath), dockerSockTarget(s.rec.Home), s.rec.DockerSockConsent)
		if err != nil {
			errs = append(errs, fmt.Errorf("the docker.sock link: %w", err))
		} else {
			s.logf("boot: %s is %s (%s)", api.DockerSockPath, st.State, st.Target)
		}
	}
	return errors.Join(errs...)
}
