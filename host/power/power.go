package power

import (
	"errors"
	"time"

	"golang.org/x/sys/unix"
)

type Kind int

const (
	WillSleep Kind = iota + 1
	HasPoweredOn
	WillPowerOn
	WillNotSleep
)

func (k Kind) String() string {
	switch k {
	case WillSleep:
		return "will sleep"
	case HasPoweredOn:
		return "has powered on"
	case WillPowerOn:
		return "will power on"
	case WillNotSleep:
		return "will not sleep"
	}
	return "unknown"
}

type Event struct {
	Kind    Kind
	Allow   func()
	Handled func(outcome any)
}

func (e Event) Ack() {
	if e.Allow != nil {
		e.Allow()
	}
}

type Source interface {
	Events() <-chan Event
	Close()
}

var ErrUnsupported = errors.New("power notifications need a build with cgo on macOS")

func Asleep() time.Duration {
	var continuous, awake unix.Timespec
	if unix.ClockGettime(unix.CLOCK_UPTIME_RAW, &awake) != nil || unix.ClockGettime(unix.CLOCK_MONOTONIC_RAW, &continuous) != nil {
		return 0
	}
	return time.Duration(continuous.Nano() - awake.Nano())
}

type Injected struct{ ch chan Event }

func NewInjected() *Injected { return &Injected{ch: make(chan Event, 16)} }

func (i *Injected) Events() <-chan Event { return i.ch }
func (i *Injected) Close()               {}

func (i *Injected) Send(kind Kind) any {
	done := make(chan any, 1)
	i.ch <- Event{Kind: kind, Handled: func(outcome any) { done <- outcome }}
	return <-done
}
