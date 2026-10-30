package main

import (
	api "fev/proto/agent"

	"fev/guest/agent"
)

func (s *system) StorageCheck(follow bool) (api.StorageCheck, <-chan struct{}, error) {
	vol, err := s.volume()
	if err != nil {
		return api.StorageCheck{}, nil, err
	}
	if follow {
		c, done, ok := vol.FollowCheck()
		if !ok {
			return api.StorageCheck{}, nil, agent.ErrNoCheck
		}
		return c, done, nil
	}
	c, _, done := vol.StartCheck(false)
	return c, done, nil
}

func (s *system) StorageCheckNow() *api.StorageCheck {
	vol, err := s.volume()
	if err != nil {
		return nil
	}
	return vol.Check()
}
