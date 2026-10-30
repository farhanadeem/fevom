package daemon

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"fev/host/daemon/config"
	"fev/host/daemon/home"
	"fev/proto/agent"
	api "fev/proto/daemon"
	"fev/proto/vmmctl"
)

type Module interface {
	Name() string
	Marks() []string
	Start(h Host) error
	StateChanged(st api.Status)
	ConfigChanged(ch api.ConfigChange)
	Close()
}

type Preparer interface {
	BeforeBoot() error
}

type Stopper interface {
	BeforeStop(boot string)
}

type Ender interface {
	VMEnded(boot string)
}

type WorkEnder interface {
	WorkEnded()
}

type PauseSink interface {
	Paused(paused bool)
}

type Quieter interface {
	Quiet()
	QuietWait(deadline time.Time) (left int)
	Unquiet()
}

type EventSink interface {
	Event(event any)
}

type Greeter interface {
	Hello(p api.HelloParams)
}

type DenialSink interface {
	Denied(folder string)
}

type GrantSink interface {
	Granted(folder string)
}

type StatusAsker interface {
	StatusAsked(ctx context.Context)
}

type ContainerCounter interface {
	RunningContainers() (n int, known bool)
}

type MethodFunc func(ctx context.Context, params json.RawMessage) (any, error)

type Host interface {
	Layout() home.Layout
	Log() *slog.Logger
	Config() *config.Store
	Fake() bool
	Status() api.Status
	VM() (VM, bool)
	Mark(name string)
	Handle(method string, fn MethodFunc)
	PublishForward(change api.ForwardsChanged)
	PublishDocker(event api.DockerEvent)
	PublishFiles(status api.FilesStatus)
	WorkEnded()
	StopVM(ctx context.Context) error
	StartVM() error
	StartOnDemand() error
	CheckAlive()
	Warn(key, text string)
	Rouse(asked bool) bool
	StartAsked() error
	WaitStatus(ctx context.Context, until func(api.Status) bool) (api.Status, error)
	PublishMachine(change api.MachinesChanged)
	PublishOperation(op api.Operation)
}

type Caller interface {
	Name() string
	Notify(method string, params any) error
	OnGone(fn func())
}

type callerKey struct{}

func CallerOf(ctx context.Context) (Caller, bool) {
	c, ok := ctx.Value(callerKey{}).(Caller)
	return c, ok
}

type VM interface {
	VsockPath(port uint32) string
	Agent() *agent.Client
	BootID() string
	Control() *vmmctl.Client
}

type Setting struct {
	Key   string
	Apply func(on bool)
}

func (s *Setting) Name() string    { return "setting " + s.Key }
func (s *Setting) Marks() []string { return nil }
func (s *Setting) Start(h Host) error {
	s.Apply(h.Config().Bool(s.Key))
	return nil
}
func (s *Setting) StateChanged(api.Status) {}
func (s *Setting) ConfigChanged(ch api.ConfigChange) {
	if on, ok := ch.Value.(bool); ok && ch.Key == s.Key {
		s.Apply(on)
	}
}
func (s *Setting) Close() {}
