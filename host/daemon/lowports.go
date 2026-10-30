package daemon

import (
	"context"
	"errors"
	"os"
	"time"

	api "fev/proto/daemon"
	priv "fev/proto/privhelper"
)

const lowBindTimeout = 3 * time.Second

var errAdminOff = errors.New("admin features are off (" + api.KeyAdminEnabled + ")")

func (n *Network) BindLow(proto string, family int, port uint16) (*os.File, error) {
	if n.cfg == nil || n.Helper == nil {
		return nil, errors.New("the daemon has not started its admin helper's module")
	}
	if !n.cfg.Bool(api.KeyAdminEnabled) {
		return nil, errAdminOff
	}
	ctx, cancel := context.WithTimeout(context.Background(), lowBindTimeout)
	defer cancel()
	var bound priv.Bound
	f, err := n.Helper.Call(ctx, priv.MethodPortBind, priv.PortParams{Proto: proto, Family: family, Port: int(port)}, &bound)
	if err != nil {
		return nil, err
	}
	if f == nil {
		return nil, errors.New("the admin helper's answer to port.bind carried no socket")
	}
	return f, nil
}
