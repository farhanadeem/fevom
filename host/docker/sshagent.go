package docker

import (
	"context"
	"net"
	"time"

	"fev/host/daemon"
	"fev/host/docker/vsock"
	"fev/host/netstack"
	"fev/proto/agent"
	"fev/proto/relay"
)

const sshDialTimeout = 5 * time.Second

const maxRefusals = 16

func (m *Module) streamRequests(vm daemon.VM) func(agent.StreamRequest) {
	return func(req agent.StreamRequest) {
		if (relay.Request{Kind: relay.Stream, Target: req.Token}).Validate() != nil {
			return
		}
		select {
		case m.streams <- struct{}{}:
		default:
			select {
			case m.refusals <- struct{}{}:
				go func() {
					defer func() { <-m.refusals }()
					m.refuseStream(vm, req.Token, "the daemon has too many streams open")
				}()
			default:
			}
			return
		}
		go func() {
			defer func() { <-m.streams }()
			m.serveStream(vm, req)
		}()
	}
}

func (m *Module) refuseStream(vm daemon.VM, token, reason string) {
	ctx, cancel := context.WithTimeout(context.Background(), sshDialTimeout)
	defer cancel()
	m.refuseCall(ctx, vm, token, reason)
	m.streamFailed("docker: refused a stream the guest asked for", "reason", reason)
}

func (m *Module) serveStream(vm daemon.VM, req agent.StreamRequest) {
	if req.Service != agent.StreamSSHAgent {
		m.refuseStream(vm, req.Token, "the daemon has no service "+clip(req.Service, 40))
		return
	}
	sock := m.opts.SSHAuthSock
	if m.opts.Agent != nil {
		sock = m.opts.Agent()
	}
	if sock == "" {
		m.refuseStream(vm, req.Token, "SSH_AUTH_SOCK is not set")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), sshDialTimeout)
	defer cancel()
	var d net.Dialer
	service, err := d.DialContext(ctx, "unix", sock)
	if err != nil {
		m.refuseStream(vm, req.Token, "the SSH agent at SSH_AUTH_SOCK does not answer: "+err.Error())
		return
	}
	stream, err := vsock.Relay{Path: vm.VsockPath(portRelay)}.Open(ctx, relay.Request{Kind: relay.Stream, Target: req.Token})
	if err != nil {
		service.Close()
		m.streamFailed("docker: the guest did not take the stream it asked for", "err", err)
		return
	}
	if now := time.Now().UnixNano(); now-m.streamFailuresSince.Load() > int64(streamLogWindow) {
		m.streamFailuresSince.Store(now)
		m.streamFailures.Store(0)
	}
	netstack.Splice(stream, service)
	stream.Close()
	service.Close()
}

const streamLogWindow = 10 * time.Minute

func (m *Module) streamFailed(msg string, args ...any) {
	n := m.streamFailures.Add(1)
	if n&(n-1) != 0 {
		return
	}
	m.log.Debug(msg, append(args, "count", n)...)
}
