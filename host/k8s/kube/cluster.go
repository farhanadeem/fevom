package kube

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"fev/host/docker/vsock"
	"fev/host/k8s"
	"fev/proto/relay"
)

const APIAddress = "127.0.0.1:6443"

type Cluster struct {
	Log *slog.Logger

	mu     sync.Mutex
	cfg    k8s.Config
	rest   *rest.Config
	cs     kubernetes.Interface
	m      *mgmt
	cancel context.CancelFunc
	w      *watcher
}

var _ k8s.Backend = (*Cluster)(nil)

func restConfig(cfg k8s.Config) (*rest.Config, error) {
	rc, err := clientcmd.RESTConfigFromKubeConfig([]byte(cfg.Kubeconfig))
	if err != nil {
		return nil, fmt.Errorf("the cluster's kubeconfig: %w", err)
	}
	opener := vsock.Relay{Path: cfg.Relay}
	rc.Dial = func(ctx context.Context, network, addr string) (net.Conn, error) {
		return opener.Open(ctx, relay.Request{Kind: relay.TCP, Target: APIAddress})
	}
	rc.TLSClientConfig.NextProtos = []string{"http/1.1"}
	rc.UserAgent = "fev-k8s"
	return rc, nil
}

func (c *Cluster) Start(ctx context.Context, cfg k8s.Config, reach func(k8s.Reach)) error {
	if cfg.Kubeconfig == "" || cfg.Relay == "" {
		return errors.New("cluster.start needs the cluster's kubeconfig and the relay's socket")
	}
	rc, err := restConfig(cfg)
	if err != nil {
		return err
	}
	cs, err := kubernetes.NewForConfig(rc)
	if err != nil {
		return err
	}
	m, err := newMgmt(rc, cs, cfg)
	if err != nil {
		return err
	}
	wctx, cancel := context.WithCancel(ctx)
	w := newWatcher(c.log(), cs, cfg, reach)
	c.mu.Lock()
	if c.cancel != nil {
		c.cancel()
	}
	c.cfg, c.rest, c.cs, c.m, c.cancel, c.w = cfg, rc, cs, m, cancel, w
	c.mu.Unlock()
	if err := w.start(wctx); err != nil {
		cancel()
		return err
	}
	if cfg.KubeconfigPath != "" {
		if err := MergeContext(cfg.KubeconfigPath, cfg.Kubeconfig, cfg.APIAddress); err != nil {
			c.log().Warn("k8s: the fev context", "file", cfg.KubeconfigPath, "err", err)
		}
	}
	return nil
}

func (c *Cluster) SetLAN(lan bool) {
	c.mu.Lock()
	w := c.w
	c.cfg.LAN = lan
	c.mu.Unlock()
	if w != nil {
		w.setLAN(lan)
	}
}

func (c *Cluster) Close() {
	c.mu.Lock()
	cancel := c.cancel
	c.cancel, c.w = nil, nil
	c.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (c *Cluster) clientset() (kubernetes.Interface, *rest.Config, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cs == nil {
		return nil, nil, errors.New("the cluster's client has not started")
	}
	return c.cs, c.rest, nil
}

func (c *Cluster) log() *slog.Logger {
	if c.Log != nil {
		return c.Log
	}
	return slog.Default()
}
