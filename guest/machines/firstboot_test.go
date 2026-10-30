package machines

import (
	"strings"
	"testing"

	api "fev/proto/agent"
)

func argvs(p setupPlan) string {
	var out []string
	for _, c := range p.cmds {
		out = append(out, strings.Join(c.argv, " "))
	}
	return strings.Join(out, "\n")
}

func TestTheDefaultUserIsMadeWithTheDistributionsTools(t *testing.T) {
	u := api.MachineUser{Name: "ada", UID: 501}
	ubuntuPasswd := "root:x:0:0:root:/root:/bin/bash\nnobody:x:65534:65534:nobody:/nonexistent:/usr/sbin/nologin\nubuntu:x:1000:1000::/home/ubuntu:/bin/bash\n"
	ubuntuGroup := "root:x:0:\nadm:x:4:syslog\nsudo:x:27:ubuntu\nvideo:x:44:\nstaff:x:50:\nubuntu:x:1000:\n"
	sudoers := map[string]string{"90-incus": "# User rules for ubuntu\nubuntu ALL=(ALL) NOPASSWD:ALL\n", "README": "# a comment\n# only\n"}
	has := func(p string) bool {
		return p == "/usr/sbin/useradd" || p == "/bin/bash" || p == "/usr/lib/systemd/systemd"
	}
	p, err := planSetup(u, ubuntuPasswd, ubuntuGroup, sudoers, has)
	if err != nil {
		t.Fatal(err)
	}
	want := "userdel -r ubuntu\ngroupadd -g 501 ada\nuseradd -m -d /home/ada -u 501 -g ada -s /bin/bash -p ! -G adm,staff,sudo,video ada"
	if got := argvs(p); got != want {
		t.Errorf("ubuntu:\n%s\nwant:\n%s", got, want)
	}
	if strings.Join(p.remove, " ") != "/etc/sudoers.d/90-incus" {
		t.Errorf("removed %v", p.remove)
	}
	if len(p.write) != 2 || p.write[0].path != SudoersFile || p.write[0].content != "ada ALL=(ALL) NOPASSWD:ALL\n" || p.write[0].mode != 0o440 ||
		p.write[1].path != "/var/lib/systemd/linger/ada" || p.write[1].content != "" {
		t.Errorf("written %+v", p.write)
	}

	alpineGroup := "root:x:0:root\nadm:x:4:root,daemon\nwheel:x:10:root\nvideo:x:27:root\n"
	p, err = planSetup(u, "root:x:0:0:root:/root:/bin/sh\n", alpineGroup, nil, func(p string) bool { return p == "/usr/sbin/adduser" })
	if err != nil {
		t.Fatal(err)
	}
	want = "addgroup -g 501 ada\nadduser -D -u 501 -G ada -s /bin/sh -h /home/ada ada\naddgroup ada adm\naddgroup ada video\naddgroup ada wheel"
	if got := argvs(p); got != want {
		t.Errorf("alpine:\n%s\nwant:\n%s", got, want)
	}

	u2 := api.MachineUser{Name: "ubuntu", UID: 501, PasswordHash: "$6$salt$hash"}
	p, err = planSetup(u2, ubuntuPasswd+"other:x:501:501::/home/other:/bin/sh\n", ubuntuGroup+"other:x:501:\n", sudoers, has)
	if err != nil {
		t.Fatal(err)
	}
	want = "userdel -r ubuntu\nuserdel -r other\ngroupadd -g 501 ubuntu\nuseradd -m -d /home/ubuntu -u 501 -g ubuntu -s /bin/bash -p $6$salt$hash -G adm,staff,sudo,video ubuntu"
	if got := argvs(p); got != want {
		t.Errorf("replaced:\n%s\nwant:\n%s", got, want)
	}
	_ = p
	for _, bad := range []api.MachineUser{{Name: "Ada", UID: 501}, {Name: "root", UID: 501}, {Name: "a b", UID: 501}, {Name: "x", UID: 0}, {Name: "x", UID: 70000}} {
		if _, err := planSetup(bad, ubuntuPasswd, ubuntuGroup, nil, has); err == nil {
			t.Errorf("%+v was taken", bad)
		}
	}
	if _, err := planSetup(u, ubuntuPasswd, ubuntuGroup, nil, func(string) bool { return false }); err == nil {
		t.Error("a machine with no tool to make a user")
	}
	if rulesOnlyFor("ubuntu ALL=(ALL) ALL\n%admin ALL=(ALL) ALL\n", []string{"ubuntu"}) {
		t.Error("a file with another rule would be removed")
	}
}

func TestUpdatedbIsKeptOffTheMac(t *testing.T) {
	debian := "PRUNE_BIND_MOUNTS=\"yes\"\n# PRUNENAMES=\".git .bzr .hg .svn\"\nPRUNEPATHS=\"/tmp /var/spool /media /var/lib/os-prober\"\nPRUNEFS=\"NFS afs\"\n"
	got, changed := pruneUpdatedb(debian)
	want := "PRUNEPATHS=\"/tmp /var/spool /media /var/lib/os-prober /mnt/mac /Users /Volumes /private /Applications /Library /mnt/machines\""
	if !changed || !strings.Contains(got, want+"\n") || !strings.Contains(got, "PRUNEFS=\"NFS afs\"") {
		t.Fatalf("debian:\n%s", got)
	}
	if again, changed := pruneUpdatedb(got); changed || again != got {
		t.Fatalf("a second time changed it:\n%s", again)
	}
	if got, _ := pruneUpdatedb("PRUNEPATHS = '/afs /media'\n"); !strings.Contains(got, "PRUNEPATHS='/afs /media /mnt/mac") {
		t.Fatalf("single quotes:\n%s", got)
	}
	if got, changed := pruneUpdatedb("PRUNEFS=\"NFS\"\n"); !changed || !strings.HasPrefix(got, "PRUNEPATHS=\"/mnt/mac ") {
		t.Fatalf("no line:\n%s", got)
	}
	if !strings.Contains(profileScript, `PATH="$PATH:/opt/fev/bin"`) || !strings.Contains(profileScript, `*:/opt/fev/bin:*) ;;`) {
		t.Fatalf("profile:\n%s", profileScript)
	}
}

func TestDhcpcdTakesTheLeaseWithoutAProbe(t *testing.T) {
	conf := "duid\npersistent\nnoipv4ll"
	got, changed := dhcpcdNoARP(conf)
	if !changed || !strings.HasPrefix(got, conf+"\n") || !strings.HasSuffix(got, "\nnoarp\n") {
		t.Fatalf("%q", got)
	}
	if again, changed := dhcpcdNoARP(got); changed || again != got {
		t.Errorf("added twice: %q", again)
	}
	if _, changed := dhcpcdNoARP("# a comment\n  noarp  \n"); changed {
		t.Error("a noarp of the user's own was not seen")
	}
}

func TestAGettyOnATerminalTheMachineHasNotIsNotRespawned(t *testing.T) {
	tab := "id:3:initdefault:\n" +
		"x1:12345:respawn:/sbin/agetty 38400 console linux\n" +
		"f0:12345:respawn:/sbin/agetty 9600 ttyAMA0 vt100\n" +
		"c1:2345:respawn:/sbin/getty 38400 tty1\n" +
		"#s0:12345:respawn:/sbin/agetty ttyS0\n" +
		"pf:12345:powerwait:/sbin/halt\n"
	has := func(dev string) bool { return dev == "tty1" }
	got, changed := inittabWithoutAbsentTerminals(tab, has)
	want := "id:3:initdefault:\n" +
		"x1:12345:respawn:/sbin/agetty 38400 console linux\n" +
		"#fev: this machine has no /dev/ttyAMA0: f0:12345:respawn:/sbin/agetty 9600 ttyAMA0 vt100\n" +
		"c1:2345:respawn:/sbin/getty 38400 tty1\n" +
		"#s0:12345:respawn:/sbin/agetty ttyS0\n" +
		"pf:12345:powerwait:/sbin/halt\n"
	if !changed || got != want {
		t.Errorf("got %v\n%s", changed, got)
	}
	if _, changed := inittabWithoutAbsentTerminals(want, has); changed {
		t.Error("a second pass changed it")
	}
}

func TestAnFstabThatMountsATmpfsAtTmpIsRead(t *testing.T) {
	for fstab, want := range map[string]bool{
		"#\n# <file system>\t<dir>\t<type>\ntmpfs\t\t/tmp\ttmpfs\tdefaults,nosuid,nodev   0       0\n": true,
		"#tmpfs /tmp tmpfs defaults 0 0\n":                                  false,
		"tmpfs /var/tmp tmpfs defaults 0 0\n/dev/vda / ext4 defaults 0 1\n": false,
		"": false,
	} {
		if got := fstabTmpfsTmp(fstab); got != want {
			t.Errorf("%q: %v", fstab, got)
		}
	}
}
