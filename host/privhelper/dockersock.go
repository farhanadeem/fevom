package privhelper

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"

	api "fev/proto/privhelper"
)

func dockerSockTarget(home string) string {
	return filepath.Join(home, ".fev", "run", "docker.sock")
}

func DockerSockState(path, target string) (api.DockerSock, error) {
	fi, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return api.DockerSock{State: api.StateAbsent}, nil
	}
	if err != nil {
		return api.DockerSock{}, err
	}
	if fi.Mode()&fs.ModeSymlink == 0 {
		return api.DockerSock{State: api.StateForeign}, nil
	}
	to, err := os.Readlink(path)
	if err != nil {
		return api.DockerSock{}, err
	}
	if to == target {
		return api.DockerSock{State: api.StateFev, Target: to}, nil
	}
	return api.DockerSock{State: api.StateForeign, Target: to}, nil
}

func LinkDockerSock(path, target string, consent bool) (api.DockerSock, error) {
	st, err := DockerSockState(path, target)
	if err != nil {
		return api.DockerSock{}, err
	}
	switch {
	case st.State == api.StateFev:
		return st, nil
	case st.State == api.StateForeign && st.Target == "":
		return st, api.Refusal{Arg: "docker_sock", Value: path, Reason: "is not a link (an engine running on the Mac itself has it); fev never replaces it"}
	case st.State == api.StateForeign && !consent:
		return st, api.Refusal{Arg: "docker_sock", Value: st.Target, Reason: "the link points at another product's socket; fev replaces it only when the admin setup recorded the consent to"}
	}
	var rnd [6]byte
	rand.Read(rnd[:])
	tmp := filepath.Join(filepath.Dir(path), ".fev-docker.sock."+hex.EncodeToString(rnd[:]))
	if err := os.Symlink(target, tmp); err != nil {
		return api.DockerSock{}, err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return api.DockerSock{}, err
	}
	return DockerSockState(path, target)
}

func UnlinkDockerSock(path, target string) (api.DockerSock, error) {
	st, err := DockerSockState(path, target)
	if err != nil || st.State != api.StateFev {
		return st, err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return api.DockerSock{}, err
	}
	return DockerSockState(path, target)
}
