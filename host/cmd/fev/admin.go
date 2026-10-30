package main

import (
	"fev/host/cli"
	"fev/host/privhelper"
	api "fev/proto/privhelper"
)

type admin struct{}

func (admin) Install(o cli.AdminInstall) (cli.AdminReport, error) {
	r, err := privhelper.Install(privhelper.InstallOptions{
		Root:     o.Root,
		Source:   o.Source,
		OwnerUID: o.OwnerUID,
		Record: api.InstallRecord{
			Build: o.Build, UID: o.UID, Home: o.Home, Base: o.Base, Base6: o.Base6,
			ResolverPort: o.ResolverPort, DockerSockLink: o.DockerSock, DockerSockConsent: o.DockerSockConsent,
		},
		Resolver:  true,
		BootJob:   false,
		Launchctl: o.Launchctl,
	})
	if err != nil {
		return cli.AdminReport{}, err
	}
	written := make([]string, len(r.Record.Paths), len(r.Record.Paths)+len(r.Store))
	for i, p := range r.Record.Paths {
		written[i] = o.Root + p
	}
	written = append(written, r.Store...)
	return cli.AdminReport{Written: written, Warnings: r.Warnings, Base6: r.Record.Base6}, nil
}

func (admin) Undo(root string, launchctl func(args ...string) error) (cli.AdminReport, error) {
	r, err := privhelper.Undo(root, launchctl)
	return cli.AdminReport{Removed: r.Removed, Left: r.Left}, err
}

func (admin) Plan(home string, uid int) (string, string, error) {
	return privhelper.ConfiguredPlan(home, uid)
}
