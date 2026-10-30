package daemon

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"fev/proto/relay"
)

func AttachSession(ctx context.Context, home, token string) (*net.UnixConn, error) {
	req := relay.Request{Kind: relay.Exec, Target: token}
	if err := req.Validate(); err != nil {
		return nil, err
	}
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", SessionSocketPath(home))
	if err != nil {
		return nil, err
	}
	sess := conn.(*net.UnixConn)
	defer sess.Close()
	if deadline, ok := ctx.Deadline(); ok {
		sess.SetDeadline(deadline)
	} else {
		sess.SetDeadline(time.Now().Add(30 * time.Second))
	}
	stop := context.AfterFunc(ctx, func() { sess.Close() })
	defer stop()
	if err := relay.WriteRequest(sess, req); err != nil {
		return nil, err
	}
	var line []byte
	buf := make([]byte, relay.MaxLine)
	oob := make([]byte, syscall.CmsgSpace(4))
	passed := -1
	closePassed := func() {
		if passed >= 0 {
			syscall.Close(passed)
			passed = -1
		}
	}
	for {
		n, oobn, flags, _, err := sess.ReadMsgUnix(buf, oob)
		if oobn > 0 {
			msgs, perr := syscall.ParseSocketControlMessage(oob[:oobn])
			if perr != nil {
				closePassed()
				return nil, perr
			}
			for _, m := range msgs {
				fds, _ := syscall.ParseUnixRights(&m)
				for _, fd := range fds {
					if passed < 0 {
						passed = fd
					} else {
						syscall.Close(fd)
					}
				}
			}
		}
		if flags&syscall.MSG_CTRUNC != 0 {
			closePassed()
			return nil, errors.New("the session's reply carried more than one descriptor")
		}
		line = append(line, buf[:n]...)
		if i := bytes.IndexByte(line, '\n'); i >= 0 {
			if err := relay.ReadReply(bytes.NewReader(line[:i+1])); err != nil {
				closePassed()
				return nil, err
			}
			if passed < 0 {
				return nil, errors.New("the daemon answered ok without the session's stream")
			}
			if i+1 != len(line) {
				closePassed()
				return nil, errors.New("the daemon sent bytes after its reply")
			}
			break
		}
		if err != nil {
			closePassed()
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, fmt.Errorf("the session socket: %w", err)
		}
	}
	syscall.CloseOnExec(passed)
	f := os.NewFile(uintptr(passed), filepath.Base(SessionSocketPath(home)))
	stream, err := net.FileConn(f)
	f.Close()
	if err != nil {
		return nil, err
	}
	return stream.(*net.UnixConn), nil
}
