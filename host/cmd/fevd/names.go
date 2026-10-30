package main

import (
	"context"
	"os"
	"os/user"
	"path/filepath"
	"time"

	"fev/host/daemon"
	"fev/host/daemon/home"
	"fev/host/trust"
	api "fev/proto/daemon"
)

type names struct {
	guest *daemon.GuestNet
	names *daemon.Names
	https *daemon.HTTPS
}

func newNames(layout home.Layout, caDir string, dnsPort int, fakeVM, ownHome bool) names {
	n := names{guest: &daemon.GuestNet{}, https: &daemon.HTTPS{}}
	n.names = &daemon.Names{AddressFile: layout.NamesFile(), Port: dnsPort}
	n.guest.Zone = n.names
	changed := func() {
		n.names.RootChanged()
		n.https.Refresh()
	}
	var keeper *trust.Keeper
	root := trust.RootOptions{User: currentUser(), Host: hostName(), Constrain: constrainRoot()}
	switch {
	case caDir != "":
		dir, err := filepath.Abs(caDir)
		if err != nil {
			fail("--ca-dir: %v", err)
		}
		keeper = trust.NewKeeper(trust.KeeperOptions{Dir: dir, Root: root, OnRootChange: changed})
	case !fakeVM && !ownHome:
		keeper = trust.NewKeeper(trust.KeeperOptions{RootPath: filepath.Join(layout.HTTPSDir(), "ca.pem"), Root: root, OnRootChange: changed})
		n.names.Offer = func() { offerTrust(keeper, filepath.Join(layout.HTTPSDir(), "offered")) }
	}
	if keeper == nil {
		n.https.Keeper = daemon.NoCA{}
		return n
	}
	n.https.Keeper = keeper
	n.names.CA = keeper
	return n
}

func constrainRoot() bool { return os.Getenv("FEV_CA_CONSTRAIN") != "0" }

func currentUser() string {
	if u, err := user.Current(); err == nil {
		return u.Username
	}
	return ""
}

func hostName() string {
	h, _ := os.Hostname()
	return h
}

func offerTrust(k *trust.Keeper, mark string) {
	if _, err := os.Stat(mark); err == nil {
		return
	}
	switch k.State().Trust {
	case api.TrustTrusted, api.TrustDeclined:
		return
	}
	if err := os.MkdirAll(filepath.Dir(mark), 0o700); err != nil {
		return
	}
	if err := os.WriteFile(mark, []byte(time.Now().UTC().Format(time.RFC3339)+"\n"), 0o600); err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	k.Trust(ctx, api.TrustInstall)
}
