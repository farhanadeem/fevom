package privhelper

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"syscall"
	"time"
)

const MaxLine = 64 << 10

const DefaultTimeout = 30 * time.Second

func Call(ctx context.Context, path, method string, params, result any) (*os.File, error) {
	raw, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}
	line, err := json.Marshal(Request{Version: "2.0", ID: 1, Method: method, Params: raw})
	if err != nil {
		return nil, err
	}
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", path)
	if err != nil {
		return nil, err
	}
	uc := conn.(*net.UnixConn)
	defer uc.Close()
	if deadline, ok := ctx.Deadline(); ok {
		uc.SetDeadline(deadline)
	} else {
		uc.SetDeadline(time.Now().Add(DefaultTimeout))
	}
	stop := context.AfterFunc(ctx, func() { uc.Close() })
	defer stop()
	if _, err := uc.Write(append(line, '\n')); err != nil {
		return nil, err
	}
	reply, fd, err := readReply(uc)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, err
	}
	var f *os.File
	if fd >= 0 {
		syscall.CloseOnExec(fd)
		f = os.NewFile(uintptr(fd), method)
	}
	wants := method == MethodTunnelOpen || method == MethodPortBind
	switch {
	case reply.Error != nil:
		if f != nil {
			f.Close()
		}
		return nil, reply.Error
	case wants && f == nil:
		return nil, fmt.Errorf("the helper answered %s without its descriptor", method)
	case !wants && f != nil:
		f.Close()
		return nil, fmt.Errorf("the helper passed a descriptor with %s, which passes none", method)
	}
	if result != nil {
		if err := json.Unmarshal(reply.Result, result); err != nil {
			if f != nil {
				f.Close()
			}
			return nil, fmt.Errorf("%s: %w", method, err)
		}
	}
	return f, nil
}

func readReply(uc *net.UnixConn) (Reply, int, error) {
	var line []byte
	buf := make([]byte, 4096)
	oob := make([]byte, syscall.CmsgSpace(4))
	passed := -1
	fail := func(err error) (Reply, int, error) {
		if passed >= 0 {
			syscall.Close(passed)
		}
		return Reply{}, -1, err
	}
	rc, err := uc.SyscallConn()
	if err != nil {
		return fail(err)
	}
	for {
		var n, oobn, flags int
		var fds []int
		var perr error
		err := rc.Read(func(s uintptr) bool {
			syscall.ForkLock.RLock()
			defer syscall.ForkLock.RUnlock()
			var rerr error
			n, oobn, flags, _, rerr = syscall.Recvmsg(int(s), buf, oob, 0)
			if rerr == syscall.EAGAIN {
				return false
			}
			if rerr != nil {
				perr = rerr
				return true
			}
			if oobn > 0 {
				msgs, err := syscall.ParseSocketControlMessage(oob[:oobn])
				if err != nil {
					perr = err
					return true
				}
				for _, m := range msgs {
					got, _ := syscall.ParseUnixRights(&m)
					for _, fd := range got {
						syscall.CloseOnExec(fd)
						fds = append(fds, fd)
					}
				}
			}
			return true
		})
		if err == nil {
			err = perr
		}
		if err == nil && n == 0 && oobn == 0 {
			err = io.EOF
		}
		for _, fd := range fds {
			if passed < 0 {
				passed = fd
			} else {
				syscall.Close(fd)
				for _, rest := range fds {
					if rest != fd && rest != passed {
						syscall.Close(rest)
					}
				}
				return fail(errors.New("the helper's reply carried more than one descriptor"))
			}
		}
		if flags&syscall.MSG_CTRUNC != 0 {
			return fail(errors.New("the helper's reply carried more than one descriptor"))
		}
		line = append(line, buf[:n]...)
		if len(line) > MaxLine {
			return fail(errors.New("the helper's reply is too long"))
		}
		if i := bytes.IndexByte(line, '\n'); i >= 0 {
			if i+1 != len(line) {
				return fail(errors.New("the helper sent bytes after its reply"))
			}
			var r Reply
			if err := json.Unmarshal(line[:i], &r); err != nil {
				return fail(fmt.Errorf("the helper's reply: %w", err))
			}
			if r.Version != "2.0" || r.ID != 1 || (r.Error == nil) == (r.Result == nil) {
				return fail(fmt.Errorf("the helper's reply is not one: %s", line[:i]))
			}
			return r, passed, nil
		}
		if err != nil {
			if err == io.EOF {
				err = errors.New("the helper closed the connection without a reply (refused by its peer check, or not installed for this user)")
			}
			return fail(err)
		}
	}
}

func ReadRequest(r io.Reader) (Request, error) {
	br := bufio.NewReaderSize(io.LimitReader(r, MaxLine+1), 4096)
	line, err := br.ReadBytes('\n')
	if err != nil {
		if len(line) > MaxLine {
			return Request{}, errors.New("the request is too long")
		}
		return Request{}, fmt.Errorf("reading the request: %w", err)
	}
	var req Request
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		return Request{}, fmt.Errorf("the request: %w", err)
	}
	if req.Version != "2.0" || req.Method == "" {
		return Request{}, errors.New("the request is not JSON-RPC 2.0")
	}
	return req, nil
}

const ReceiptBound = 10 * time.Second

func WriteReply(uc *net.UnixConn, req Request, result any, err error, fd int) error {
	r := Reply{Version: "2.0", ID: req.ID}
	if err != nil {
		var e *Error
		if !errors.As(err, &e) {
			var ref Refusal
			if errors.As(err, &ref) {
				e = Refused(ref)
			} else {
				e = &Error{Code: CodeFailed, Message: err.Error()}
			}
		}
		r.Error = e
		fd = -1
	} else {
		raw, merr := json.Marshal(result)
		if merr != nil {
			return merr
		}
		r.Result = raw
	}
	line, merr := json.Marshal(r)
	if merr != nil {
		return merr
	}
	line = append(line, '\n')
	var oob []byte
	if fd >= 0 {
		oob = syscall.UnixRights(fd)
	}
	n, _, werr := uc.WriteMsgUnix(line, oob, nil)
	if werr == nil && n < len(line) {
		_, werr = uc.Write(line[n:])
	}
	if werr == nil && fd >= 0 && !awaitReceipt(uc, ReceiptBound) {
		return fmt.Errorf("the caller did not take the descriptor within %v", ReceiptBound)
	}
	return werr
}

func awaitReceipt(uc *net.UnixConn, bound time.Duration) bool {
	uc.SetReadDeadline(time.Now().Add(bound))
	var b [64]byte
	for {
		if _, err := uc.Read(b[:]); err != nil {
			var ne net.Error
			return !(errors.As(err, &ne) && ne.Timeout())
		}
	}
}
