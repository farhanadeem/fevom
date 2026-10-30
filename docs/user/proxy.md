# Proxies

fev follows this Mac's proxy settings (System Settings › Network › your network › Details ›
Proxies), automatic configuration (PAC) included, with nothing to set in your containers or
machines.

- **Image pulls** go through the proxy the Mac uses for the registry's address.
- **Containers' and machines' TCP connections** go through it too, on every port, with no
  `HTTP_PROXY` in them (`network.proxy.transparent`, on by default). fev asks the proxy for
  the **name** the program looked up (or the name its TLS or HTTP request carries), so the
  Mac's exceptions and your PAC's rules by name apply, as they do for Safari. Plain `http://`
  requests are sent to the proxy as proxy requests; everything else as `CONNECT`.
- **Never through the proxy**: fev's own addresses and names (`*.fev.local`), this Mac
  (`host.docker.internal`), the networks the Mac is on, link-local addresses, and what you list
  in `network.proxy_exclude`.
- **UDP is never proxied**: QUIC and HTTP/3 leave directly (DNS is answered by the Mac's own
  resolver).
- **A proxy that refuses a connection, or that cannot be reached, fails the connection**: fev
  never goes around the proxy. A PAC that says `DIRECT` is followed.

Settings:

```sh
fev config set network.proxy auto                       # this Mac's settings (the default)
fev config set network.proxy http://proxy.example:3128  # a proxy of your own (http, https, socks5, socks5h; user:password@ allowed)
fev config set network.proxy none                       # never a proxy
fev config add network.proxy_exclude .corp.example 10.0.0.0/8   # always direct
fev config set network.proxy.transparent false          # containers and machines connect directly; pulls still use the proxy
```

A change applies to the next connection; nothing restarts. A proxy that asks for a password
is reached only through `network.proxy`'s URL: fev reads no password from the keychain.
