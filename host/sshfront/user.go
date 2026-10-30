package sshfront

import (
	"errors"
	"fmt"
	"strings"

	"fev/proto/relay"
)

var ErrNoDefault = errors.New(`there is no Linux machine to log in to; make one with "fev create DISTRO NAME"`)

func ParseUser(login string, isMachine func(string) bool, def string) (Target, error) {
	user, machine, two := strings.Cut(login, "@")
	if !two {
		machine, user = "", login
		if login == DefaultUser {
			user = ""
		} else if login != "" && isMachine(login) {
			machine, user = login, ""
		}
	}
	if user == DefaultUser {
		user = ""
	}
	if strings.Contains(machine, "@") {
		return Target{}, fmt.Errorf("%q is not a login: it is USER@MACHINE, MACHINE or USER", login)
	}
	if machine == "" || machine == DefaultUser {
		if def == "" {
			return Target{}, ErrNoDefault
		}
		machine = def
	} else if err := relay.CheckMachineName(machine); err != nil {
		return Target{}, err
	}
	if strings.ContainsAny(user, ":/\n") {
		return Target{}, fmt.Errorf("%q is not a user's name", user)
	}
	return Target{Machine: machine, User: user}, nil
}
