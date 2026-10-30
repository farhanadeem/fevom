package nfsview

import (
	"encoding/binary"
	"fmt"
	"net/netip"
	"strings"
	"testing"
	"time"
)

var fixedNow = time.Unix(1_800_000_000, 0)

func answerer(t *testing.T) (*Answerer, *Tree) {
	t.Helper()
	tr := NewTree("/run/fev/view", Owner{UID: 501, GID: 20})
	a := &Answerer{
		Table:   tr.Table(),
		Clients: []netip.Addr{netip.MustParseAddr("127.0.0.1")},
		Expiry:  time.Hour,
		IDs:     MacNames{UID: 501, GID: 20, User: "ada", Group: "staff"},
		Now:     func() time.Time { return fixedNow },
	}
	return a, tr
}

const exp = "1800003600"

func fsidWord(f uint32) string {
	var b [4]byte
	binary.LittleEndian.PutUint32(b[:], f)
	return Hex(b[:])
}

func TestTheUpcallsAreAnsweredInTheKernelsForms(t *testing.T) {
	a, tr := answerer(t)
	ch := tr.Set(Object{Kind: KindVolume, Key: "data", Name: "data"})
	m := tr.Set(Object{Kind: KindMachine, Key: "u", Name: "u", UID: 501, GID: 501})
	img := tr.Set(Object{Kind: KindImage, Key: "sha256:ab12 ghcr.io/org/app:1", Name: "ghcr.io/org/app:1", ReadOnly: true})
	sp := tr.Set(Object{Kind: KindVolume, Key: "with space", Name: "with space"})
	for _, c := range []struct{ channel, req, want string }{
		{ChannelClient, "nfsd 127.0.0.1", "nfsd 127.0.0.1 " + exp + " fev"},
		{ChannelClient, "nfsd ::ffff:127.0.0.1", "nfsd ::ffff:127.0.0.1 " + exp + " fev"},
		{ChannelClient, "nfsd 192.168.64.1", "nfsd 192.168.64.1 " + exp},
		{ChannelFH, `fev 1 \x00000000`, `fev 1 \x00000000 ` + exp + " /run/fev/view"},
		{ChannelFH, "fev 1 " + fsidWord(ch.FSID), "fev 1 " + fsidWord(ch.FSID) + " " + exp + " /run/fev/view/docker/volumes/data"},
		{ChannelFH, `fev 1 \x0f0f0f0f`, `fev 1 \x0f0f0f0f ` + exp},
		{ChannelFH, `fev 6 \x0102030405060708090a0b0c0d0e0f10`, `fev 6 \x0102030405060708090a0b0c0d0e0f10 ` + exp},
		{ChannelFH, `other 1 \x00000000`, `other 1 \x00000000 ` + exp},
		{ChannelExport, "fev /run/fev/view", fmt.Sprintf("fev /run/fev/view %s %d 501 20 0 secinfo 1 1 %d", exp, FlagsRoot, FlagsRoot)},
		{ChannelExport, "fev /run/fev/view/docker/volumes/data", fmt.Sprintf("fev /run/fev/view/docker/volumes/data %s %d 0 0 %d secinfo 1 1 %d", exp, FlagsObject, ch.FSID, FlagsObject)},
		{ChannelExport, "fev /run/fev/view/u", fmt.Sprintf("fev /run/fev/view/u %s %d 501 501 %d secinfo 1 1 %d", exp, FlagsObject, m.FSID, FlagsObject)},
		{ChannelExport, `fev /run/fev/view/docker/volumes/with\040space`, fmt.Sprintf(`fev /run/fev/view/docker/volumes/with\040space %s %d 0 0 %d secinfo 1 1 %d`, exp, FlagsObject, sp.FSID, FlagsObject)},
		{ChannelExport, "fev /run/fev/view/docker", "fev /run/fev/view/docker " + exp},
		{ChannelExport, "fev /etc", "fev /etc " + exp},
		{ChannelExport, "* /run/fev/view", "* /run/fev/view " + exp},
		{ChannelNameToID, "* user ada@localdomain", "* user ada@localdomain " + exp + " 501"},
		{ChannelNameToID, "* user 501", "* user 501 " + exp + " 501"},
		{ChannelNameToID, "* group staff@localdomain", "* group staff@localdomain " + exp + " 20"},
		{ChannelNameToID, "* user root@Mac.localdomain", "* user root@Mac.localdomain " + exp + " 0"},
		{ChannelNameToID, "* group wheel@Mac.localdomain", "* group wheel@Mac.localdomain " + exp + " 0"},
		{ChannelNameToID, "* user maheen@Mac.localdomain", "* user maheen@Mac.localdomain " + exp + " 4294967294"},
		{ChannelNameToID, "* group root@Mac.localdomain", "* group root@Mac.localdomain " + exp + " 4294967294"},
		{ChannelNameToID, "* user staff@Mac.localdomain", "* user staff@Mac.localdomain " + exp + " 4294967294"},
		{ChannelIDToName, "* user 501", "* user 501 " + exp + " 501"},
	} {
		got, err := a.Answer(c.channel, c.req)
		if err != nil || got != c.want {
			t.Errorf("%s %q:\n got  %q, %v\n want %q", c.channel, c.req, got, err, c.want)
		}
	}
	got, _ := a.Answer(ChannelExport, "fev "+Quote(img.Path))
	var flags int
	fmt.Sscanf(strings.Fields(got)[3], "%d", &flags)
	if flags&FlagReadOnly == 0 || flags&FlagAsync != 0 {
		t.Errorf("an image's export: flags %#x", flags)
	}
	for _, bad := range []struct{ channel, req string }{
		{ChannelClient, "nfsd"},
		{ChannelFH, `fev 1 \x123`},
		{ChannelIDToName, "* user root"},
		{"nfsd.unknown", "x y"},
	} {
		if r, err := a.Answer(bad.channel, bad.req); err == nil {
			t.Errorf("%s %q answered %q", bad.channel, bad.req, r)
		}
	}
}

func TestARemovedObjectIsRefusedAndAFlushIsAskedFor(t *testing.T) {
	a, tr := answerer(t)
	o := Object{Kind: KindContainer, Key: strings.Repeat("ab", 32), Name: "web"}
	ch := tr.Set(o)
	if ch.Flush {
		t.Error("a first export asked for a flush")
	}
	if r, _ := a.Answer(ChannelExport, "fev "+ch.Path); !strings.Contains(r, " secinfo ") {
		t.Fatalf("not exported: %q", r)
	}
	rm := tr.Remove(KindContainer, o.Key)
	if !rm.Flush || rm.OldPath != ch.Path {
		t.Errorf("remove after a lookup: %+v", rm)
	}
	if r, _ := a.Answer(ChannelExport, "fev "+ch.Path); strings.Contains(r, " secinfo ") {
		t.Errorf("a removed container answered: %q", r)
	}
	if r, _ := a.Answer(ChannelFH, "fev 1 "+fsidWord(ch.FSID)); strings.Count(r, " ") > 3 {
		t.Errorf("a removed container's fsid answered: %q", r)
	}
	again := tr.Set(o)
	if !again.Flush || again.FSID != ch.FSID {
		t.Errorf("exported again after a refusal: %+v (fsid was %#x)", again, ch.FSID)
	}
	tr.Table().Flushed()
	if rm := tr.Remove(KindContainer, o.Key); rm.Flush {
		t.Error("a removal nfsd never asked about since the flush asked for one")
	}
}

func TestHandleAnswersEveryLineOfARead(t *testing.T) {
	a, _ := answerer(t)
	answers, bad := a.Handle(ChannelClient, []byte("nfsd 127.0.0.1\nnfsd\nnfsd 10.1.2.3\n"))
	if len(answers) != 2 || len(bad) != 1 {
		t.Fatalf("answers %q, bad %q", answers, bad)
	}
	for _, ans := range answers {
		if !strings.HasSuffix(string(ans), "\n") || strings.Count(string(ans), "\n") != 1 {
			t.Errorf("answer %q is not one line", ans)
		}
	}
}
