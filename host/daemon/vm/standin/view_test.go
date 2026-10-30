package standin

import (
	"encoding/json"
	"reflect"
	"testing"

	api "fev/proto/agent"
)

func TestTheStandInsViewListsAndTellsRemovals(t *testing.T) {
	a := booted(t)
	c, notes := hostConn(t, a)
	objs := []api.ViewObject{{Kind: api.ViewContainer, Name: "web", Path: "docker/containers/web", FSID: 7, Mounted: true}}
	a.SetViewObjects(objs)
	var l api.ViewObjectList
	if err := c.Call(bg, api.MethodViewObjects, api.Empty{}, &l); err != nil || len(l.Objects) != 0 {
		t.Fatalf("stopped: %+v %v", l, err)
	}
	if err := c.Call(bg, api.MethodViewStart, api.ViewStartParams{UID: 501, GID: 20}, nil); err != nil {
		t.Fatal(err)
	}
	waitNote(t, notes, api.NotifyViewChanged, func(p json.RawMessage) bool {
		var st api.ViewStatus
		json.Unmarshal(p, &st)
		return st.State == api.ViewReady
	})
	if err := c.Call(bg, api.MethodViewObjects, api.Empty{}, &l); err != nil || !reflect.DeepEqual(l.Objects, objs) {
		t.Fatalf("ready: %+v %v", l, err)
	}
	a.ViewRemoved([]string{"docker/containers/web"})
	p := waitNote(t, notes, api.NotifyViewRemoved, nil)
	var r api.ViewRemoved
	if json.Unmarshal(p, &r); !reflect.DeepEqual(r.Paths, []string{"docker/containers/web"}) {
		t.Errorf("view.removed: %s", p)
	}
}
