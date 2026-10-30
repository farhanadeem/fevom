package main

import (
	"errors"
	"os"
	"path/filepath"
	"time"

	api "fev/proto/agent"

	"fev/guest/internal/logbuf"
	"fev/guest/storage"
)

func (s *system) K8sCRI(on bool) (api.K8sStatus, error) {
	k := s.cluster()
	if err := k.begin(api.MethodK8sCRI); err != nil {
		return k.status(false), err
	}
	defer k.end()
	if s.engine.sup.Status().State != api.EngineReady {
		return k.status(false), errors.New("the Docker engine is not running")
	}
	k.mu.Lock()
	sup, wanted := k.sup, k.wanted
	k.mu.Unlock()
	if sup.cri == on && sup.state != "" && (sup.state == api.K8sOff || sup.state == "purged" || on) {
		return k.status(false), nil
	}
	if !on {
		if sup.state == api.K8sRunning {
			return k.status(false), errors.New("the cluster runs: containerd keeps its CRI plugins until it is stopped")
		}
		st, err := k.down("off", api.K8sOff)
		if err != nil {
			return st, err
		}
		k.unmake()
		return k.status(false), nil
	}
	if wanted {
		return k.status(false), nil
	}
	k.mu.Lock()
	k.criMade = nil
	k.mu.Unlock()
	if err := s.k8sBindParts(); err != nil {
		return k.status(false), err
	}
	if err := os.WriteFile(k8sRunFile("want"), []byte("cri\n"), 0o644); err != nil {
		return k.status(false), err
	}
	began := time.Now()
	mark := k.mark()
	if err := k.signal(); err != nil {
		return k.status(false), err
	}
	if err := k.waitCRI(mark, true, k8sStopWait); err != nil {
		return k.status(false), err
	}
	s.log.Logf(logbuf.Info, "k8s: containerd restarted with its CRI plugins in %d ms", time.Since(began).Milliseconds())
	return k.status(false), nil
}

func (k *k8sGuest) waitCRI(mark int, on bool, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	for {
		k.mu.Lock()
		s, ch, said := k.sup, k.changed, k.said
		k.mu.Unlock()
		if said > mark && s.state == api.K8sOff && s.cri == on && s.serving {
			return nil
		}
		select {
		case <-ch:
		case <-timer.C:
			return errors.New("containerd is not back with its CRI plugins as asked (the cluster is " + s.state + ")")
		}
	}
}

func (k *k8sGuest) unmake() {
	k.mu.Lock()
	made := k.criMade
	k.criMade = nil
	k.mu.Unlock()
	if len(made) == 0 {
		return
	}
	vol, err := k.s.volume()
	if err != nil {
		return
	}
	for _, sub := range made {
		switch sub {
		case storage.K8sSubvolume:
			if _, err := os.Stat(filepath.Join(k8sData, "k3s", filepath.Base(k8sImported))); err == nil {
				continue
			}
			if err := vol.RemoveK8sSubvolume(); err != nil {
				k.s.log.Logf(logbuf.Warning, "k8s: removing the unused @k8s: %v", err)
			}
		case storage.K8sCacheSubvolume:
			used := false
			for _, d := range k8sCacheDirs {
				if ents, _ := os.ReadDir(filepath.Join(k8sCache, d)); len(ents) > 0 {
					used = true
				}
			}
			if used {
				continue
			}
			if err := vol.RemoveK8sCacheSubvolume(); err != nil {
				k.s.log.Logf(logbuf.Warning, "k8s: removing the unused @k8s-cache: %v", err)
			}
		}
	}
	k.s.log.Logf(logbuf.Info, "k8s: a start that never ran: %v removed", made)
}

func (s *system) k8sBindParts() error {
	vol, err := s.volume()
	if err != nil {
		return err
	}
	k := s.cluster()
	note := func(made bool, sub string) {
		if made {
			k.mu.Lock()
			k.criMade = append(k.criMade, sub)
			k.mu.Unlock()
		}
	}
	_, made, err := vol.EnsureK8sSubvolume()
	if err != nil {
		return err
	}
	note(made, storage.K8sSubvolume)
	cache, made, err := vol.EnsureK8sCacheSubvolume()
	if err != nil {
		return err
	}
	note(made, storage.K8sCacheSubvolume)
	for _, d := range k8sCacheDirs {
		if err := os.MkdirAll(filepath.Join(cache, d), 0o755); err != nil {
			return err
		}
	}
	if err := k8sNested(); err != nil {
		return err
	}
	return s.engine.k8sBindRun(true)
}
