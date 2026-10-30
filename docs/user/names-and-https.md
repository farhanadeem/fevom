# Names and HTTPS

Every running container, Compose service and Linux machine has a name under `fev.local`,
and every name serves HTTPS with a certificate your Mac trusts. You do not need to publish a
port or remember one:

```sh
docker run -d --name web nginx
open https://web.fev.local/        # Safari, Chrome, curl: no warning, no port
```

## The names

| Name | What answers |
|---|---|
| `NAME.fev.local` | the container `NAME` (its name, not its id) or the machine `NAME` |
| `anything.NAME.fev.local` | the same target: every name under it is a wildcard |
| `SERVICE.PROJECT.fev.local` | a Compose service's first replica (the lowest number running) |
| `fev.local` | the index page: every running container and machine with its links |
| `host.fev.internal` | the Mac, from containers and machines |
| `docker.fev.internal` | the VM's Docker host, from machines: containers' published ports (`docker.fev.internal:8080`) |

Each container name has an address of its own, kept when you recreate the container under
the same name, so a browser tab or a connection string keeps working. Every port of the name
reaches the container (`web.fev.local:5432`), TCP and UDP. Ports 80 and 443 go to fev's
web front, which serves the container's web port: the label's (below), else 80, else the one
it finds serving HTML. A container on `--network host` or `none` has a name and no address.

A container and a machine of the same name: the name is the container's while it runs, as on
other tools of the kind; the machine has it back when the container stops, and is reached by
its address meanwhile (`fev info NAME`). fev's log says which machine is shadowed, once.
Inside the VM a machine's name also has its IPv6 address (`AAAA`), which machines and
containers on IPv6 networks reach. On the Mac, with the admin helper, names have an IPv6
address too: a container's ends in its IPv4 one (`fd…:3::c613:402` for `198.19.4.2`), with its
web ports served by fev's web front and its other ports leading to the container's own IPv6
address (a container on a network made with `--ipv6`; with none they are refused, so a client
uses IPv4 at once), and a machine's is its own.

Names resolve on the Mac, in containers and in machines. On the Mac they go through [the
admin helper](admin-helper.md) once it is installed (about 10 ms a name); without it fev
answers them by mDNS on its own network interface (about 0.2 s a name, a name nothing holds
failing after about 10 s; `fev status` says "by mDNS").

On the Mac:

- **Your Mac's search list.** While fev runs, `fev.local` and each custom name of two
  parts (`shop.local`) are in your Mac's DNS search list (`scutil --dns`): macOS sends such a
  name to fev only when it is. So a short name you type is also tried under them: `ping web`
  reaches the container `web.fev.local`, and a container asking for a bare name gets the same
  answer. fev answers a name it does not hold at once, so nothing waits. Longer custom names
  (`app.shop.local`) stay out of the list.
- **One Local Network prompt an application.** macOS resolves names under `.local` by
  multicast as well, so the first time an application looks one up (Chrome, Firefox, VS
  Code, your terminal) it may ask for Local Network access. fev's names work either way:
  fev answers them itself, not by multicast. Safari and command-line tools started by
  launchd do not ask. Reaching a container by its address (`198.19.x.x`) asks nothing.
- **Chrome's own prompt.** Chrome may also ask, in its own words, whether a page may reach
  your local network: it did for a Vite development server's page under `fev.local`,
  whose live-reload connection goes back to that name. Allow it for that page.
- **Your network sees the names you look up.** macOS also sends each `.local` lookup to the
  LAN as a multicast question, whatever answers it, so a device listening there can see the
  names of the containers you open (never their addresses: fev answers them on your Mac).
  This is how every `.local` name works on a Mac.
- **A new name works at once; a name that moves takes a moment.** A container's name keeps
  its address when you recreate the container. A name that moves to another container (a
  label) reaches the new one in about 15 s, and in about 30 s in Chrome, which keeps its own
  cache.
- **While fev is stopped**, a lookup of a fev name fails after about 5 s (macOS waits for
  an answer by multicast), and a browser tab keeps waiting until fev is back. A connection
  to a container's address fails at once: the address never leaves your Mac.

### Your own names

```sh
docker run -d -l dev.fevom.domains='shop.local,*.api.local,shop.test' myapp
```

`dev.fevom.domains` gives a container more names, separated by commas. `*.x` answers `x` and
every name under it. Two containers asking for one name: the one that started first keeps it,
and `fev status` names both.

- A name under `.local` also resolves on the Mac, while the container runs, unless a device
  on your network already answers it (fev never takes a name from your printer or NAS).
  `fev status` shows each such name and whether the Mac has it. Without [the admin
  helper](admin-helper.md) fev answers it by mDNS, and a device answering the same name may
  be found first.
- **Any other name (`shop.test`, and even `github.com`) resolves inside fev's VM only, for
  every container and machine, and never on the Mac.** A label is part of an image: an image
  you pull can carry one, and a container started from it then answers that name for the
  other containers and machines while it runs. Check an image's labels
  (`docker image inspect -f '{{json .Config.Labels}}' IMAGE`) if that matters to you.
- Refused, whatever the image says: a name of one label (`local`, `com`, `*.local`),
  `fev.local` itself, and anything at or under `fev.internal` and `docker.internal`,
  whose names fev answers itself.

### Choosing the web port

| Label (container) | Machine setting | |
|---|---|---|
| `dev.fevom.http-port=8080` | `fev config set machine.NAME.http_port 8080` | the plain HTTP port |
| `dev.fevom.https-port=8443` | `fev config set machine.NAME.https_port 8443` | a port that serves TLS itself |

A machine that serves 80 or 443 itself is reached directly on those ports.

## HTTPS

fev makes a certificate authority of its own the first time HTTPS is used, and signs a
certificate for each name when it is first asked for. The Mac trusts it after one dialog
asking for your password, which `fev setup` raises (and `fev setup --https` again later):

```sh
fev setup --https            # trust fev's root on this Mac (one dialog)
fev setup --https --undo     # take the trust away
fev config set network.https false   # no HTTPS at all (port 80 still works)
```

The dialog appears on the Mac's own screen, so run these in a Terminal on the Mac: over SSH
they stop at once and say so, rather than leave a dialog nobody is there to answer.

The root is trusted for TLS alone, for your user only. Its key is in your login keychain, and
it cannot be exported; any program you run can ask the keychain to sign with it while you
are logged in, which is why the root is limited to names under `.local`. `fev status` says whether the root is trusted and
where its key is.

Safari, Chrome, `curl`, Go programs and `node --use-system-ca` trust the root as soon as the
dialog is answered. **Firefox** reads the Mac's trusted roots when it starts: quit it and
open it again after `fev setup --https`. If it still warns, set
`security.enterprise_roots.enabled` to `true` in `about:config`, or import
`~/.fev/https/ca.pem` under Settings, Privacy & Security, Certificates.

**Programs that keep trust stores of their own** do not read the Mac's: give them the root's
file, `~/.fev/https/ca.pem`. Python (with its own OpenSSL, as Homebrew's) and Node without
`--use-system-ca` are such programs (Java keeps its own store, below).

| Program | How |
|---|---|
| Node.js | `NODE_EXTRA_CA_CERTS=~/.fev/https/ca.pem`, or `node --use-system-ca` |
| Python `requests` | `REQUESTS_CA_BUNDLE=~/.fev/https/ca.pem` |
| Java | `keytool -importcert -alias fev -file ~/.fev/https/ca.pem -cacerts` |

### In containers

fev writes its root into each new container's own trust store when it is created: the
image's bundle (`/etc/ssl/certs/ca-certificates.crt`, `/etc/pki/tls/certs/ca-bundle.crt`,
`/etc/ssl/cert.pem`, and any `SSL_CERT_FILE`, `CURL_CA_BUNDLE`, `REQUESTS_CA_BUNDLE` or
`GIT_SSL_CAINFO` the image sets) and its family's anchor, so `update-ca-certificates` and
`update-ca-trust` run inside keep it. So `curl https://web.fev.local/` works from one
container to another with no flag. Inside a container, Node, Python's `requests` and Java
need the root as on the Mac: it is at `http://fev.local/ca.pem`.

- An image with no bundle (scratch, `debian:trixie-slim`) is left as it is.
- `-l dev.fevom.ca=false` or `--runtime runc` leaves a container's files as the image has them.
- `docker diff` shows the files fev wrote, and `docker commit` keeps them in the new image.
- `docker info` names fev's runtime wrapper, `fev-runc`, as the default runtime; it runs
  `runc` unchanged.

### A container that serves TLS itself

fev's front ends TLS with its own certificate and connects to the container's TLS port
behind it without checking the container's certificate, so `https://NAME.fev.local/`
always shows fev's. To serve a certificate the Mac and other containers trust on the
container's own port, ask fev for one of the container's own names:

```sh
openssl req -new -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes \
  -keyout key.pem -subj /CN=web.fev.local -addext "subjectAltName=DNS:web.fev.local,DNS:api.web.fev.local" -out req.csr
curl -fsS --data-binary @req.csr http://fev.local/csr > cert.pem    # valid 90 days; ask again to renew
curl -fsS http://fev.local/ca.pem > fev-root.pem
```

The request must come from the container itself (a machine may ask for its own name too),
and every name in it must be one of the container's own: its name or a name under it, its
Compose service's, or its label's under `.local`. A label's name outside `fev.local`
(`shop.local`) is signed only while the Mac has it, as `fev status` shows: never for a
name a device on your network answers, and not at all without the admin helper (or with
`network.direct_access` off): a name answered by mDNS is not one fev holds alone.
Otherwise the request is refused (403), and the server's own names under `fev.local` still
work. A container that asks the moment it starts may be told to ask again: fev learns of
it from Docker a moment after it runs. A certificate already given stays valid for its 90
days. Each name is exact: fev signs no wildcard (`*.web.fev.local` is refused), so list
every name the server answers to.

### Dev servers

Hot reload works through fev's front; two dev servers need to be told about fev's names:

- **Vite** refuses requests for a host it does not know (`403 Blocked request`): add
  `server: { allowedHosts: ['.fev.local'] }` to `vite.config.js`.
- **Next.js** serves the page but refuses its hot-reload connection to an origin it does not
  know, its own name included: add `allowedDevOrigins: ['*.fev.local']` to
  `next.config.js`.

## Web pages from the internet

A page you open in a browser can make requests to addresses it does not own. fev's front
for `NAME.fev.local` answers only requests that name one of the container's or machine's
names (or its address): a page that points its own domain at a fev address (DNS rebinding)
is refused with 421, so it cannot read your containers' pages as its own.

What fev cannot stop: a container's own address and a name's ports other than 80 and 443
are plain routes from the Mac, and Chrome treats fev's addresses (`198.19.0.0/16`) as public,
so it asks you nothing before a page sends requests there. Such a page cannot read the answers
unless the server allows it (CORS), but it can send simple requests (a form's GET or POST) to a
server in a container; and a page that points its own domain at a container's address can
read what that server answers, unless the server checks the `Host` header. Keep development servers that act on such requests behind a check of
the `Host` header (Vite's `allowedHosts`, above) or of a token, as on any machine on your
network. WebKit's local network check, not yet in Safari, treats fev's addresses as the
Mac's own and refuses such requests from public pages. fev keeps these addresses rather
than ones Chrome guards because they keep it off the ranges LANs and VPNs use.
