package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/textproto"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/sys/unix"

	api "fev/proto/daemon"
)

func (a *app) topCmd() *cobra.Command {
	var interval float64
	var count int
	cmd := &cobra.Command{
		Use:   "top",
		Short: "Show what the VM, machines, Compose projects and containers use",
		Long: `Show, live, what fev's VM uses on the Mac and what runs in it uses: CPU,
memory, disk and network for the Linux system as a whole, fev's services,
each machine, each Compose project (its containers under it) and each other
container. CPU is a percentage of one processor; rates are per second over
the last interval.

It refreshes every --interval seconds until interrupted, or -n times. To
a pipe it prints each refresh as a block of text; with --format json, one
stats sample per line, as the Daemon API has it. It starts nothing.`,
		Args: args(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			if interval < 0.25 {
				return usagef("--interval is at least 0.25 seconds")
			}
			if count < 0 {
				return usagef("-n is a number of refreshes, 0 for no end")
			}
			return a.top(cmd.Context(), time.Duration(interval*float64(time.Second)), count)
		},
	}
	cmd.Flags().Float64Var(&interval, "interval", 2, "seconds between refreshes")
	cmd.Flags().IntVarP(&count, "count", "n", 0, "refresh this many times, then exit (0: until interrupted)")
	return cmd
}

type ctrInfo struct {
	Name, Project, Service string
}

func containerList(ctx context.Context, socket string) (map[string]ctrInfo, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", socket)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		conn.SetDeadline(deadline)
	}
	if _, err := io.WriteString(conn, "GET /containers/json HTTP/1.0\r\nHost: docker\r\n\r\n"); err != nil {
		return nil, err
	}
	r := bufio.NewReader(conn)
	tp := textproto.NewReader(r)
	line, err := tp.ReadLine()
	if err != nil {
		return nil, err
	}
	if _, code, _ := strings.Cut(line, " "); !strings.HasPrefix(code, "200") {
		return nil, fmt.Errorf("the engine answered %q", line)
	}
	header, err := tp.ReadMIMEHeader()
	if err != nil {
		return nil, err
	}
	if strings.EqualFold(header.Get("Transfer-Encoding"), "chunked") {
		return nil, fmt.Errorf("the engine answered in chunks to an HTTP/1.0 request")
	}
	var list []struct {
		ID     string            `json:"Id"`
		Names  []string          `json:"Names"`
		Labels map[string]string `json:"Labels"`
	}
	if err := json.NewDecoder(r).Decode(&list); err != nil {
		return nil, err
	}
	out := map[string]ctrInfo{}
	for _, c := range list {
		info := ctrInfo{Project: c.Labels["com.docker.compose.project"], Service: c.Labels["com.docker.compose.service"]}
		if len(c.Names) > 0 {
			info.Name = strings.TrimPrefix(c.Names[0], "/")
		}
		out[c.ID] = info
	}
	return out, nil
}

type topState struct {
	mu    sync.Mutex
	names map[string]ctrInfo
	stale bool
	prev  *api.StatsSample
}

func (a *app) top(ctx context.Context, interval time.Duration, count int) error {
	l, err := a.layout()
	if err != nil {
		return err
	}
	c, err := a.connect(ctx)
	if err != nil {
		return err
	}
	defer c.Close()
	st := &topState{stale: true}
	_, unsubscribe, err := c.Subscribe(ctx, api.Events{Docker: func(e api.DockerEvent) {
		if e.Type != "container" {
			return
		}
		st.mu.Lock()
		st.stale = true
		st.mu.Unlock()
	}})
	if err != nil {
		return err
	}
	defer unsubscribe()
	samples := make(chan api.StatsSample, 4)
	stop, err := c.SubscribeStats(ctx, int(interval/time.Millisecond), func(s api.StatsSample) {
		select {
		case samples <- s:
		default:
		}
	})
	if err != nil {
		return err
	}
	defer stop()
	out, isFile := a.Stdout.(*os.File)
	terminal := !a.json() && isFile && isTerminal(out)
	if terminal {
		fmt.Fprint(a.Stdout, "\x1b[?1049h\x1b[?25l")
		defer fmt.Fprint(a.Stdout, "\x1b[?25h\x1b[?1049l")
	}
	socket := filepath.Join(l.RunDir(), "docker.sock")
	for n := 0; count == 0 || n < count; n++ {
		var s api.StatsSample
		select {
		case s = <-samples:
		case <-c.Done():
			return failf(ExitUnreachable, "top: the daemon's connection ended")
		case <-ctx.Done():
			return nil
		}
		if a.json() {
			line, err := jsonLine(s)
			if err != nil {
				return err
			}
			fmt.Fprintln(a.Stdout, line)
			continue
		}
		st.mu.Lock()
		if st.stale && s.State == api.StateRunning {
			lctx, cancel := context.WithTimeout(ctx, 2*time.Second)
			if names, err := containerList(lctx, socket); err == nil {
				st.names, st.stale = names, false
			}
			cancel()
		}
		names, prev := st.names, st.prev
		st.prev = &s
		st.mu.Unlock()
		width, height := 0, 0
		if terminal {
			if ws, err := unix.IoctlGetWinsize(int(out.Fd()), unix.TIOCGWINSZ); err == nil {
				width, height = int(ws.Col), int(ws.Row)
			}
			fmt.Fprint(a.Stdout, "\x1b[H\x1b[2J")
		} else if n > 0 {
			fmt.Fprintln(a.Stdout)
		}
		fmt.Fprint(a.Stdout, topFrame(prev, &s, names, width, height))
	}
	return nil
}

type topRow struct {
	kind, name string
	indent     bool
	cpu        float64
	mem        uint64
	read       float64
	write      float64
	rx, tx     float64
	pids       int
	hasNet     bool
}

func rate(a, b uint64, secs float64, known bool) float64 {
	if !known || secs <= 0 || b < a {
		return -1
	}
	return float64(b-a) / secs
}

func topFrame(prev, cur *api.StatsSample, names map[string]ctrInfo, width, height int) string {
	var b strings.Builder
	when := cur.Time
	if t, err := time.Parse(time.RFC3339Nano, cur.Time); err == nil {
		when = t.Local().Format("15:04:05")
	}
	fmt.Fprintf(&b, "fev %s  VM %s", when, cur.State)
	if v := cur.VMM; v != nil {
		fmt.Fprintf(&b, "  on the Mac: %s (of %s guest RAM; %s given back)", bytesText(int64(v.FootprintBytes)), bytesText(int64(v.GuestBytes)), bytesText(int64(v.ReleasedBytes)))
	}
	b.WriteString("\n")
	g := cur.Guest
	if g == nil {
		if cur.State == api.StatePaused {
			b.WriteString("The VM is paused: nothing in it is running, and nothing is asked of it.\n")
		} else {
			b.WriteString("Nothing runs: fev's VM is not running.\n")
		}
		return clip(b.String(), width)
	}
	used := (g.MemTotalKB - g.MemAvailableKB) << 10
	fmt.Fprintf(&b, "Linux: %s used of %s, %s file cache\n\n", bytesText(used), bytesText(g.MemTotalKB<<10), bytesText(g.CachedKB<<10))

	secs := 0.0
	before := map[string]api.StatsGroup{}
	if prev != nil && prev.Guest != nil {
		t0, err0 := time.Parse(time.RFC3339Nano, prev.Time)
		t1, err1 := time.Parse(time.RFC3339Nano, cur.Time)
		if err0 == nil && err1 == nil {
			secs = t1.Sub(t0).Seconds()
		}
		for _, gr := range prev.Guest.Groups {
			before[gr.Kind+"/"+gr.ID+"/"+gr.Name] = gr
		}
	}
	row := func(gr api.StatsGroup) topRow {
		p, known := before[gr.Kind+"/"+gr.ID+"/"+gr.Name]
		r := topRow{kind: gr.Kind, name: gr.Name, mem: gr.MemoryBytes, pids: gr.PIDs, hasNet: gr.Kind != api.StatsService,
			cpu:  rate(p.CPUUsec, gr.CPUUsec, secs, known) / 1e4,
			read: rate(p.DiskReadBytes, gr.DiskReadBytes, secs, known), write: rate(p.DiskWriteBytes, gr.DiskWriteBytes, secs, known),
			rx: rate(p.NetRxBytes, gr.NetRxBytes, secs, known), tx: rate(p.NetTxBytes, gr.NetTxBytes, secs, known)}
		if r.cpu < 0 {
			r.cpu = -1
		}
		return r
	}
	var system, services, machines, loose []topRow
	projects := map[string][]topRow{}
	for _, gr := range g.Groups {
		r := row(gr)
		switch gr.Kind {
		case api.StatsSystem:
			r.name = "Linux (all)"
			system = append(system, r)
		case api.StatsService:
			services = append(services, r)
		case api.StatsMachine:
			machines = append(machines, r)
		case api.StatsContainer:
			info, listed := names[gr.ID]
			if listed && info.Name != "" {
				r.name = info.Name
			}
			if listed && info.Project != "" {
				projects[info.Project] = append(projects[info.Project], r)
			} else {
				loose = append(loose, r)
			}
		}
	}
	byName := func(rows []topRow) {
		sort.Slice(rows, func(i, j int) bool { return rows[i].name < rows[j].name })
	}
	byName(services)
	byName(machines)
	byName(loose)
	rows := append([]topRow{}, system...)
	for _, set := range []struct {
		title string
		rows  []topRow
	}{{"service", services}, {"machine", machines}} {
		for _, r := range set.rows {
			r.kind = set.title
			rows = append(rows, r)
		}
	}
	var titles []string
	for p := range projects {
		titles = append(titles, p)
	}
	sort.Strings(titles)
	for _, p := range titles {
		members := projects[p]
		byName(members)
		sum := topRow{kind: "compose", name: p, hasNet: true}
		for _, m := range members {
			sum.mem += m.mem
			sum.pids += m.pids
			sum.cpu = addKnown(sum.cpu, m.cpu)
			sum.read, sum.write = addKnown(sum.read, m.read), addKnown(sum.write, m.write)
			sum.rx, sum.tx = addKnown(sum.rx, m.rx), addKnown(sum.tx, m.tx)
		}
		rows = append(rows, sum)
		for _, m := range members {
			m.indent = true
			m.kind = "container"
			rows = append(rows, m)
		}
	}
	for _, r := range loose {
		r.kind = "container"
		rows = append(rows, r)
	}
	header := []string{"KIND", "NAME", "CPU%", "MEMORY", "DISK READ/s", "DISK WRITE/s", "NET IN/s", "NET OUT/s", "PIDS"}
	var cells [][]string
	for _, r := range rows {
		name := r.name
		if r.indent {
			name = "  " + name
		}
		netIn, netOut := rateText(r.rx), rateText(r.tx)
		if !r.hasNet {
			netIn, netOut = "", ""
		}
		cells = append(cells, []string{r.kind, name, pctText(r.cpu), bytesText(int64(r.mem)), rateText(r.read), rateText(r.write), netIn, netOut, strconv.Itoa(r.pids)})
	}
	hidden := 0
	if height > 0 && len(cells) > height-5 {
		hidden = len(cells) - max(height-6, 0)
		cells = cells[:len(cells)-hidden]
	}
	var t strings.Builder
	table(&t, header, cells)
	b.WriteString(t.String())
	if hidden > 0 {
		fmt.Fprintf(&b, "(%d more rows; make the window taller)\n", hidden)
	}
	if len(projects) == 0 && len(loose) == 0 {
		b.WriteString("No container is running.\n")
	}
	return clip(b.String(), width)
}

func addKnown(a, b float64) float64 {
	if a < 0 || b < 0 {
		return -1
	}
	return a + b
}

func pctText(p float64) string {
	if p < 0 {
		return "-"
	}
	return fmt.Sprintf("%.1f", p)
}

func rateText(r float64) string {
	if r < 0 {
		return "-"
	}
	return bytesText(int64(r))
}

func clip(text string, width int) string {
	if width <= 0 {
		return text
	}
	lines := strings.SplitAfter(text, "\n")
	for i, line := range lines {
		body := strings.TrimSuffix(line, "\n")
		if len(body) > width {
			lines[i] = body[:width] + strings.TrimPrefix(line, body)
		}
	}
	return strings.Join(lines, "")
}
