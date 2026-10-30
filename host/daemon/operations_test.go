package daemon

import (
	"context"
	"errors"
	"testing"
	"time"

	api "fev/proto/daemon"
)

func TestEachOperationsEndSaysWorkEnded(t *testing.T) {
	ended := make(chan struct{}, 4)
	ops := NewOperations(func(api.Operation) {}, func() { ended <- struct{}{} })
	for _, end := range []func(*Operation){
		func(op *Operation) { op.Done(nil) },
		func(op *Operation) { op.Fail(errors.New("no room")) },
		func(op *Operation) { op.Cancelled() },
	} {
		ops.Start(context.Background(), api.MethodMachinesCreate, "m", "", end)
		select {
		case <-ended:
		case <-time.After(5 * time.Second):
			t.Fatal("an operation ended and work was not said to have ended")
		}
	}
}
