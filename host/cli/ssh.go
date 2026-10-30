package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/sys/unix"

	"fev/host/daemon/home"
	"fev/host/daemon/launch"
	api "fev/proto/daemon"
)

const proxyHold = 10 * time.Second

func (a *app) sshCmd() *cobra.Command {
	var proxy bool
	cmd := &cobra.Command{
		Use:   "ssh",
		Short: "Show how to log in to the Linux machines over SSH",
		Long: `Show how to reach the Linux machines with ssh, scp, sftp, VS Code Remote SSH or
JetBrains Gateway: fev's SSH server, its port, user names and key.

  ssh fev                  the default user in the default machine
  ssh MACHINE@fev          the default user in MACHINE
  ssh USER@MACHINE@fev     USER in MACHINE ("root@MACHINE@fev" is root)

"fev setup" includes fev's entry in ~/.ssh/config. Keys in
~/.fev/ssh/authorized_keys may log in too; the server takes public keys
only. Nothing is started until a client has logged in.

--proxy is the ProxyCommand of that entry, run by ssh: it starts fev if it
is not running, without starting the VM, and hands ssh a connection.`,
		Args: args(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			if proxy {
				return a.sshProxy()
			}
			return a.withDaemon(cmd.Context(), false, func(c *api.Client) error {
				info, err := c.SSHInfo(cmd.Context())
				if err != nil {
					return err
				}
				return a.emit(info, func(w io.Writer) { sshText(w, info) })
			})
		},
	}
	cmd.Flags().BoolVar(&proxy, "proxy", false, "be the ProxyCommand of ssh's fev entry (run by ssh, not by hand)")
	return cmd
}

func sshText(w io.Writer, i *api.SSHInfo) {
	fmt.Fprintf(w, "ssh %s                  the default user in the default machine\n", i.Alias)
	fmt.Fprintf(w, "ssh MACHINE@%s          the default user in MACHINE\n", i.Alias)
	fmt.Fprintf(w, "ssh USER@MACHINE@%s     USER in MACHINE\n\n", i.Alias)
	state := "listening"
	if !i.Listening {
		state = "not listening; ssh " + i.Alias + " still works through its ProxyCommand"
	}
	kv(w, "host", i.Host)
	kv(w, "port", strconv.Itoa(i.Port)+" ("+state+")")
	kv(w, "user", i.User+", or MACHINE, or USER@MACHINE")
	kv(w, "key", i.IdentityFile)
	kv(w, "known hosts", i.KnownHostsFile)
	included := "included in ~/.ssh/config"
	if !i.Included {
		included = `not included in ~/.ssh/config: "fev setup" includes it, or ssh -F ` + i.ConfigFile
	}
	kv(w, "config", i.ConfigFile+" ("+included+")")
	kv(w, "host key", i.HostKey)
	kv(w, "authorized keys", i.AuthorizedKeys)
	if i.AuthorizedKeysError != "" {
		kv(w, "warning", i.AuthorizedKeysError)
	}
	fmt.Fprintf(w, "\nVS Code Remote SSH: connect to host %q (or MACHINE@%s).\n", i.Alias, i.Alias)
	fmt.Fprintf(w, "JetBrains Gateway: host %s, port %d, user %s (or MACHINE), key %s.\n", "127.0.0.1", i.Port, i.User, i.IdentityFile)
}

func (a *app) sshProxy() error {
	out, ok := a.Stdout.(*os.File)
	in, ok2 := a.Stdin.(*os.File)
	if !ok || !ok2 {
		return failf(ExitFailed, "ssh --proxy is run by ssh as its ProxyCommand, with ProxyUseFdpass")
	}
	l, err := a.layout()
	if err != nil {
		return err
	}
	if err := l.Validate(); err != nil {
		return failf(ExitUnreachable, "%v", err)
	}
	if err := launch.Ensure(a.launchOptions(l, false)); err != nil {
		return unreachable(err)
	}
	greet(l, a.Name)
	fd, err := takeSSHConnection(api.SSHSocketPath(l.Home))
	if err != nil {
		return failf(ExitUnreachable, "the SSH server on %s: %v", api.SSHSocketPath(l.Home), err)
	}
	defer unix.Close(fd)
	if err := unix.Sendmsg(int(out.Fd()), []byte{0}, unix.UnixRights(fd), nil, 0); err != nil {
		if errors.Is(err, syscall.ENOTSOCK) {
			return failf(ExitFailed, "ssh --proxy hands ssh a socket and needs ProxyUseFdpass yes in the entry: %v", err)
		}
		return failf(ExitFailed, "ssh --proxy: passing the connection to ssh: %v", err)
	}
	if !waitEOF(int(in.Fd()), proxyHold) {
		return failf(ExitFailed, "ssh did not take the connection within %v", proxyHold)
	}
	return nil
}

func greet(l home.Layout, name string) {
	ctx, cancel := context.WithTimeout(context.Background(), conflictBound)
	defer cancel()
	conn, err := net.Dial("unix", l.Socket())
	if err != nil {
		return
	}
	if c, _, err := api.NewClient(ctx, conn, name, helloOptions()...); err == nil {
		c.Close()
	}
}

func takeSSHConnection(path string) (int, error) {
	c, err := net.DialTimeout("unix", path, proxyHold)
	if err != nil {
		return -1, err
	}
	uc := c.(*net.UnixConn)
	defer uc.Close()
	uc.SetDeadline(time.Now().Add(proxyHold))
	var line []byte
	buf := make([]byte, 512)
	oob := make([]byte, syscall.CmsgSpace(4))
	passed := -1
	fail := func(err error) (int, error) {
		if passed >= 0 {
			unix.Close(passed)
		}
		return -1, err
	}
	for {
		n, oobn, flags, _, err := uc.ReadMsgUnix(buf, oob)
		if oobn > 0 {
			msgs, perr := syscall.ParseSocketControlMessage(oob[:oobn])
			if perr != nil {
				return fail(perr)
			}
			for _, m := range msgs {
				fds, _ := syscall.ParseUnixRights(&m)
				for _, fd := range fds {
					if passed < 0 {
						passed = fd
					} else {
						unix.Close(fd)
					}
				}
			}
		}
		if flags&syscall.MSG_CTRUNC != 0 {
			return fail(errors.New("the descriptor was cut off in transit"))
		}
		line = append(line, buf[:n]...)
		if i := bytes.IndexByte(line, '\n'); i >= 0 {
			reply := string(line[:i])
			if reply != "ok" {
				return fail(fmt.Errorf("the daemon refused: %s", strings.TrimPrefix(reply, "err ")))
			}
			if passed < 0 {
				return fail(errors.New("the daemon answered with no connection"))
			}
			return passed, nil
		}
		if err != nil {
			return fail(err)
		}
		if n == 0 {
			return fail(io.ErrUnexpectedEOF)
		}
	}
}

func waitEOF(fd int, bound time.Duration) bool {
	deadline := time.Now().Add(bound)
	buf := make([]byte, 512)
	for {
		left := time.Until(deadline)
		if left <= 0 {
			return false
		}
		fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN | unix.POLLHUP}}
		n, err := unix.Poll(fds, int(left.Milliseconds())+1)
		if err != nil {
			if errors.Is(err, unix.EINTR) {
				continue
			}
			return true
		}
		if n == 0 {
			continue
		}
		r, err := unix.Read(fd, buf)
		if r == 0 || (err != nil && !errors.Is(err, unix.EAGAIN) && !errors.Is(err, unix.EINTR)) {
			return true
		}
	}
}
