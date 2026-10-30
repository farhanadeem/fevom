package grpcwire

import (
	"context"
	"encoding/binary"
	"io"
	"net"
	"net/http"
	"strconv"
)

type Handler func(method string, header http.Header, msg Fields) (answer []byte, code int, message string)

func Fake(path string, h Handler) (*Conn, func(), error) {
	l, err := net.Listen("unix", path)
	if err != nil {
		return nil, nil, err
	}
	var p http.Protocols
	p.SetUnencryptedHTTP2(true)
	srv := &http.Server{Protocols: &p, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("content-type", "application/grpc")
		w.Header().Set("Trailer", "grpc-status, grpc-message")
		var msg []byte
		if len(body) >= 5 {
			n := int(binary.BigEndian.Uint32(body[1:5]))
			if 5+n <= len(body) {
				msg = body[5 : 5+n]
			}
		}
		f, err := Decode(msg)
		code, message, ans := 3, "", []byte(nil)
		if err == nil && r.ProtoMajor == 2 {
			ans, code, message = h(r.URL.Path, r.Header, f)
		}
		w.WriteHeader(200)
		if code == 0 {
			w.Write(Frame(ans))
		}
		w.Header().Set("grpc-status", strconv.Itoa(code))
		w.Header().Set("grpc-message", message)
	})}
	go srv.Serve(l)
	c := Unix(path, nil)
	return c, func() { c.Close(); srv.Shutdown(context.Background()) }, nil
}
