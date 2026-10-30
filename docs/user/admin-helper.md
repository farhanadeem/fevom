# fev's admin helper

Reaching containers and machines by their own addresses from the Mac, and their
`*.fev.local` names in every application, needs a few things macOS reserves for the
administrator. fev does them through a small helper you install once:

```sh
sudo fev setup --admin                 # install (asks for your password once)
sudo fev setup --admin --docker-sock   # also make /var/run/docker.sock fev's
sudo fev setup --admin --undo          # remove everything it installed
fev setup --admin --dry-run            # show what it would write and run; no sudo
```

The password is asked at install and uninstall, when the helper's protocol changes, and for
an address range outside the one it recorded. It is never asked at run time, at boot, after
sleep, on a network change or after a rebuild.

Direct access covers IPv6 too: the Mac reaches a machine's IPv6 address and a container's on a
network made with `--ipv6`. On an IPv6-only network with NAT64 (a phone's hotspot, some
carriers), where macOS gives programs a NAT64 address for an IPv4 one such as
`198.19.64.2`, fev routes those addresses to the VM as well, so `curl http://198.19.64.2/`
works there as anywhere; the container then sees the Mac as `198.19.3.3`.

Without the helper, fev reaches containers and machines from the Mac another way, with no
password at all: a network interface of its own (`bridgeN`, as `ifconfig` shows it, with
fev's addresses on it), and names on the Mac answered by mDNS on that interface, as
`fev status` says ("with no admin helper"). macOS gives such an interface only private
addresses, so fev set up without the helper numbers containers and machines in
`10.198.0.0/16` (or another private range your Mac does not use) instead of `198.19.0.0/16`;
installing the helper later keeps that range. The differences: ports below 1024 and the
`/var/run/docker.sock` link still need the helper; a name takes about 0.2 s to look up rather
than about 10 ms, a name nothing holds about 10 s to fail rather than at once, and a device on
your network answering the same `.local` name may be found first; there is no IPv6 on that
interface; and the interface brings fev a wake-up or so a second from the Mac's own network
announcements while it is idle (the helper's tunnel brings none). `fev config set
admin.enabled false` makes fev stop using an installed helper without removing it: fev
then goes the same way, unless the helper's reserved route below still holds fev's
addresses, which `fev status` says, with the two ways out (turn admin features on again, or
`sudo fev setup --admin --undo`).

## What it installs

| Path | What |
|---|---|
| `/Library/PrivilegedHelperTools/dev.fevom.privhelper` | the helper (root, read-only) |
| `/Library/LaunchDaemons/dev.fevom.privhelper.plist` | its launchd job. The helper runs only while fev calls it, one short process a call, and once at each start of the Mac: then it puts back fev's resolver entry for `fev.local` and, if you asked for it, the `docker.sock` link |
| `/var/run/dev.fevom.privhelper.sock` | the socket launchd listens on (yours, mode 0600) |
| `/Library/Application Support/dev.fevom/privhelper/install.json` | the record of the install: your user, the address range, the resolver's port and every path written |
| `/Library/Application Support/dev.fevom/privhelper/localnames.lock` | made at first use, readable by root alone. It keeps how often custom names may be added |
| resolver entries in the network configuration (`State:/Network/Service/dev.fevom.resolver.NAME/DNS`, not files) | one for `fev.local`, which sends `fev.local` and every name under it to fev on `127.0.0.1`, and one for each custom `.local` name a running container's `dev.fevom.domains` label holds, only while that container runs. `scutil --dns` lists them; none adds a search domain. They are not files because macOS asks a name like `shop.local` or `fev.local` itself only of such an entry: an `/etc/resolver` file covers only the names under it |
| reject routes on `lo0` for fev's two address ranges (`198.19.0.0/16` and the IPv6 `/48` the install records, `netstat -rn` flag `R`) | while fev is down, a connection to a container's or a machine's address fails at once on the Mac instead of going to your network's router. fev's own routes are more specific and win while it runs. Put back at each start of the Mac. A route of another program's for the same range is left alone, and none of fev's is added beside it |
| `/var/run/docker.sock` | with `--docker-sock` only: a link to `~/.fev/run/docker.sock`. fev makes it again at each start of its VM if it is gone, with direct access on or off (`admin.enabled false` stops that too) |

macOS shows the job under System Settings, General, Login Items, as "dev.fevom.privhelper,
item from an unidentified developer" with a blank icon: fev is not signed by a registered
developer. Switching it off there unloads the job, so fev's calls are refused: direct access
and names on the Mac stop (the tunnel fev already holds lasts until its daemon exits), and
`fev status` says why. Switching it back on restores them at fev's next call. After
`--undo`, Login Items' own record of the item may stay listed until macOS prunes it; it runs
nothing.

Some things live only while fev runs, and nothing removes them by hand: the tunnel interface
(`utunN` with `198.19.3.1`), its routes and fev's ports below 1024. All of them end when
fev's daemon exits, and also when it crashes.

## Removing it

`sudo fev setup --admin --undo` removes exactly what `install.json` lists. It also removes
fev's resolver entries, and `/var/run/docker.sock` if that is fev's link. Nothing that is
not fev's is touched: a resolver entry is fev's only if its service is named
`dev.fevom.resolver.*`, a file only if its first line is `# fev: written by ...` (the
file form, `--resolver-form file`), and a reject route only while it is still the one
the install recorded (another program's route for the same range, a VPN's for instance, is
left, and the removal says so).

If fev is already gone, remove the helper by hand:

```sh
sudo launchctl bootout system/dev.fevom.privhelper
sudo rm -f /Library/PrivilegedHelperTools/dev.fevom.privhelper \
  /Library/LaunchDaemons/dev.fevom.privhelper.plist \
  /var/run/dev.fevom.privhelper.sock
b6=$(sed -n 's/.*"base6": "\(.*\)".*/\1/p' "/Library/Application Support/dev.fevom/privhelper/install.json" 2>/dev/null)
sudo rm -rf "/Library/Application Support/dev.fevom/privhelper"
sudo rmdir "/Library/Application Support/dev.fevom" 2>/dev/null
# fev's resolver entries only (a restart of the Mac also clears them)
printf 'list State:/Network/Service/dev.fevom.resolver.*\n' | scutil |
  sed -n 's/.*= //p' | while read -r k; do printf 'remove %s\n' "$k" | sudo scutil; done
# and the file form's: the files whose first line is fev's
for f in /etc/resolver/*; do
  head -1 "$f" | grep -q '^# fev:' && sudo rm "$f"
done
# the docker.sock link, only if it is fev's
[ "$(readlink /var/run/docker.sock)" = "$HOME/.fev/run/docker.sock" ] && sudo rm /var/run/docker.sock
# fev's reject routes (a restart of the Mac also clears them): first look that the
# ranges' routes on lo0 with flag R are fev's ones (netstat -rn | grep lo0), then
sudo route -n delete -net 198.19.0.0/16 -interface lo0
[ -z "$b6" ] || sudo route -n delete -inet6 -net "$b6" -interface lo0
```

Check that nothing is left: `ls /Library/PrivilegedHelperTools /Library/LaunchDaemons | grep
dev.fevom`, `scutil --dns | grep -B2 -A3 fev` and `ls -l /var/run/docker.sock`.

## What any program of yours can ask of it

The helper takes requests only from you and from root. It cannot tell fev from another
program running as you, so its own rules carry the safety. Such a program can:

- route unused addresses inside fev's recorded range into a tunnel;
- open fev's tunnel while fev holds none;
- take a loopback port below 1024 that nothing holds;
- make or remove fev's `docker.sock` link, and only if you asked for the link at install;
- send a `.local` name to fev's resolver for every account on the Mac. This works only for
  a name that nothing on your network or in your DNS answered when the helper probed for it.

It cannot run commands, write a file of its choosing, take over a route or a name something
else already has, bind anything but loopback, or trust a certificate. Direct access opens
containers' unpublished ports to every program and user on this Mac, as a Linux Docker host
does.
