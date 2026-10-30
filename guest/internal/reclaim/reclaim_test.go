package reclaim

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func cgroup(t *testing.T, dir string, activeMiB, inactiveMiB int, populated bool) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	stat := "anon 4096\nfile 999999999999\nshmem 123\ninactive_anon 1\nactive_anon 2\n" +
		"inactive_file " + strconv.Itoa(inactiveMiB<<20) + "\nactive_file " + strconv.Itoa(activeMiB<<20) + "\nunevictable 0\n"
	p := "0"
	if populated {
		p = "1"
	}
	write(t, filepath.Join(dir, "memory.stat"), stat)
	write(t, filepath.Join(dir, "cgroup.events"), "populated "+p+"\nfrozen 0\n")
	write(t, filepath.Join(dir, "memory.low"), "0\n")
	write(t, filepath.Join(dir, "memory.reclaim"), "")
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func engineTree(t *testing.T) Tree {
	t.Helper()
	return engineTreeAt(t, t.TempDir())
}

func engineTreeAt(t *testing.T, root string) Tree {
	t.Helper()
	cgroup(t, root, 1000, 2000, true)
	cgroup(t, filepath.Join(root, "engine"), 150, 50, true)
	cgroup(t, filepath.Join(root, "exec"), 0, 0, false)
	cgroup(t, filepath.Join(root, "docker"), 950, 1800, true)
	cgroup(t, filepath.Join(root, "docker", "aaa"), 100, 200, true)
	cgroup(t, filepath.Join(root, "docker", "bbb"), 300, 200, true)
	cgroup(t, filepath.Join(root, "docker", "bbb", "init.scope"), 100, 0, true)
	cgroup(t, filepath.Join(root, "docker", "bbb", "system.slice"), 200, 200, true)
	cgroup(t, filepath.Join(root, "docker", "ccc"), 25, 25, false)
	return Tree{Root: root, Own: []string{"engine", "exec"}, Containers: "docker"}
}

func record(t *testing.T, root string) *[]string {
	t.Helper()
	var log []string
	old := writeFile
	writeFile = func(path string, data []byte, perm os.FileMode) error {
		rel, _ := filepath.Rel(root, path)
		log = append(log, rel+"="+string(data))
		return old(path, data, 0o644)
	}
	t.Cleanup(func() { writeFile = old })
	return &log
}

func TestTheSurveyTellsWhatNothingRunningOwns(t *testing.T) {
	tree := engineTree(t)
	s, err := tree.Look()
	if err != nil {
		t.Fatal(err)
	}
	if s.Total != 3000<<20 || s.Live != 800<<20 || s.Idle() != 2200<<20 {
		t.Errorf("total %d MiB, live %d MiB, idle %d MiB; want 3000, 800 and 2200", s.Total>>20, s.Live>>20, s.Idle()>>20)
	}
	for _, dir := range []string{"docker/aaa", "docker/bbb"} {
		write(t, filepath.Join(tree.Root, dir, "cgroup.events"), "populated 0\n")
	}
	if s, _ := tree.Look(); s.Live != 0 || s.Idle() != 3000<<20 {
		t.Errorf("with no container running: live %d MiB, idle %d MiB", s.Live>>20, s.Idle()>>20)
	}
	cgroup(t, filepath.Join(tree.Root, "custom"), 10, 10, true)
	cgroup(t, filepath.Join(tree.Root, "custom", "x"), 10, 10, true)
	if s, _ := tree.Look(); s.Live != 20<<20 {
		t.Errorf("a custom parent with a running container: live %d MiB, want 20", s.Live>>20)
	}
}

func TestReclaimTakesTheGonesCacheFirstAndShieldsWhatRuns(t *testing.T) {
	tree := engineTree(t)
	write(t, filepath.Join(tree.Root, "docker", "aaa", "memory.low"), "104857600\n")
	s, err := tree.Look()
	if err != nil {
		t.Fatal(err)
	}
	log := record(t, tree.Root)
	if err := s.Reclaim(2100 << 20); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"docker/ccc/memory.reclaim=" + strconv.Itoa(50<<20) + " swappiness=0",
		"docker/aaa/memory.low=max", "docker/bbb/memory.low=max", "docker/ccc/memory.low=max",
		"docker/memory.reclaim=" + strconv.Itoa(1898<<20) + " swappiness=0",
		"docker/aaa/memory.low=104857600", "docker/bbb/memory.low=0", "docker/ccc/memory.low=0",
		"docker/memory.low=max", "engine/memory.low=max", "exec/memory.low=max",
		"memory.reclaim=" + strconv.Itoa(48<<20) + " swappiness=0",
		"docker/memory.low=0", "engine/memory.low=0", "exec/memory.low=0",
		"engine/memory.reclaim=" + strconv.Itoa(104<<20) + " swappiness=0",
	}
	if strings.Join(*log, "\n") != strings.Join(want, "\n") {
		t.Errorf("wrote:\n%s\nwant:\n%s", strings.Join(*log, "\n"), strings.Join(want, "\n"))
	}
	if low, _ := os.ReadFile(filepath.Join(tree.Root, "docker", "aaa", "memory.low")); string(low) != "104857600" {
		t.Errorf("the container's own protection is now %q", low)
	}

	*log = nil
	if err := s.Reclaim(60 << 20); err != nil {
		t.Fatal(err)
	}
	want = []string{
		"docker/ccc/memory.reclaim=" + strconv.Itoa(50<<20) + " swappiness=0",
		"docker/aaa/memory.low=max", "docker/bbb/memory.low=max", "docker/ccc/memory.low=max",
		"docker/memory.reclaim=" + strconv.Itoa(10<<20) + " swappiness=0",
		"docker/aaa/memory.low=104857600", "docker/bbb/memory.low=0", "docker/ccc/memory.low=0",
	}
	if strings.Join(*log, "\n") != strings.Join(want, "\n") {
		t.Errorf("a request for 60 MiB wrote:\n%s\nwant:\n%s", strings.Join(*log, "\n"), strings.Join(want, "\n"))
	}
}

func TestTheSystemRootsOwnCacheIsIdle(t *testing.T) {
	system := t.TempDir()
	cgroup(t, system, 1650, 2000, true)
	cgroup(t, filepath.Join(system, "machines"), 400, 0, true)
	tree := engineTreeAt(t, filepath.Join(system, "fev.system"))
	tree.System = system
	s, err := tree.Look()
	if err != nil {
		t.Fatal(err)
	}
	if s.Total != 3250<<20 || s.Live != 800<<20 || s.Idle() != 2450<<20 {
		t.Errorf("total %d MiB, live %d MiB, idle %d MiB; want 3250, 800 and 2450", s.Total>>20, s.Live>>20, s.Idle()>>20)
	}
	if s.Own() != 200<<20 {
		t.Errorf("own %d MiB; want 200, the engine's own services' alone", s.Own()>>20)
	}
	log := record(t, system)
	if err := s.Reclaim(2200 << 20); err != nil {
		t.Fatal(err)
	}
	rel := "fev.system"
	want := []string{
		rel + "/docker/ccc/memory.reclaim=" + strconv.Itoa(50<<20) + " swappiness=0",
		rel + "/docker/aaa/memory.low=max", rel + "/docker/bbb/memory.low=max", rel + "/docker/ccc/memory.low=max",
		rel + "/docker/memory.reclaim=" + strconv.Itoa(1898<<20) + " swappiness=0",
		rel + "/docker/aaa/memory.low=0", rel + "/docker/bbb/memory.low=0", rel + "/docker/ccc/memory.low=0",
		rel + "/docker/memory.low=max", rel + "/engine/memory.low=max", rel + "/exec/memory.low=max",
		rel + "/memory.reclaim=" + strconv.Itoa(48<<20) + " swappiness=0",
		rel + "/docker/memory.low=0", rel + "/engine/memory.low=0", rel + "/exec/memory.low=0",
		"fev.system/memory.low=max", "machines/memory.low=max",
		"memory.reclaim=" + strconv.Itoa(204<<20) + " swappiness=0",
		"fev.system/memory.low=0", "machines/memory.low=0",
	}
	if strings.Join(*log, "\n") != strings.Join(want, "\n") {
		t.Errorf("wrote:\n%s\nwant:\n%s", strings.Join(*log, "\n"), strings.Join(want, "\n"))
	}
}

func TestTheViewsCgroupsColdCacheIsNobodys(t *testing.T) {
	system := t.TempDir()
	cgroup(t, system, 1330, 2000, true)
	cgroup(t, filepath.Join(system, "fev.view"), 30, 80, true)
	tree := engineTreeAt(t, filepath.Join(system, "fev.system"))
	tree.System = system
	tree.Others = []string{filepath.Join(system, "fev.view"), filepath.Join(system, "not-there")}
	s, err := tree.Look()
	if err != nil {
		t.Fatal(err)
	}
	if s.Total != 3300<<20 || s.Live != 800<<20 || s.Idle() != 2500<<20 || s.Own() != 200<<20 {
		t.Errorf("total %d MiB, live %d, idle %d, own %d; want 3300, 800, 2500 and 200", s.Total>>20, s.Live>>20, s.Idle()>>20, s.Own()>>20)
	}
	log := record(t, system)
	if err := s.Reclaim(130 << 20); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"fev.system/docker/ccc/memory.reclaim=" + strconv.Itoa(50<<20) + " swappiness=0",
		"fev.view/memory.reclaim=" + strconv.Itoa(80<<20) + " swappiness=0",
	}
	if strings.Join(*log, "\n") != strings.Join(want, "\n") {
		t.Errorf("wrote:\n%s\nwant:\n%s", strings.Join(*log, "\n"), strings.Join(want, "\n"))
	}
}

func TestWithNoEngineTheViewsAndTheRootsCacheIsStillIdle(t *testing.T) {
	system := t.TempDir()
	cgroup(t, system, 100, 300, true)
	cgroup(t, filepath.Join(system, "fev.init"), 50, 50, true)
	cgroup(t, filepath.Join(system, "fev.view"), 20, 60, true)
	tree := Tree{System: system, Others: []string{filepath.Join(system, "fev.view")}}
	s, err := tree.Look()
	if err != nil {
		t.Fatal(err)
	}
	if s.Total != 280<<20 || s.Live != 0 || s.Idle() != 280<<20 || s.Own() != 0 {
		t.Errorf("total %d MiB, live %d, idle %d, own %d; want 280, 0, 280 and 0", s.Total>>20, s.Live>>20, s.Idle()>>20, s.Own()>>20)
	}
	log := record(t, system)
	if err := s.Reclaim(1 << 30); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"fev.view/memory.reclaim=" + strconv.Itoa(60<<20) + " swappiness=0",
		"fev.init/memory.low=max", "fev.view/memory.low=max",
		"memory.reclaim=" + strconv.Itoa(220<<20-margin) + " swappiness=0",
		"fev.init/memory.low=0", "fev.view/memory.low=0",
	}
	if strings.Join(*log, "\n") != strings.Join(want, "\n") {
		t.Errorf("wrote:\n%s\nwant:\n%s", strings.Join(*log, "\n"), strings.Join(want, "\n"))
	}
}

func TestReclaimableObjectsAreCache(t *testing.T) {
	tree := engineTree(t)
	for _, dir := range []string{"docker/ccc", "docker", "."} {
		path := filepath.Join(tree.Root, dir, "memory.stat")
		data, _ := os.ReadFile(path)
		write(t, path, string(data)+"slab_reclaimable "+strconv.Itoa(30<<20)+"\n")
	}
	s, err := tree.Look()
	if err != nil {
		t.Fatal(err)
	}
	if s.Idle() != 2230<<20 {
		t.Errorf("idle %d MiB; want 2230, the stopped container's 30 MiB of objects with the rest", s.Idle()>>20)
	}
}

func TestRunningContainersAreNamedAndTrimmedAlone(t *testing.T) {
	tree := engineTree(t)
	write(t, filepath.Join(tree.Root, "docker", "aaa", "cpu.stat"), "usage_usec 1500000\nuser_usec 1000000\n")
	s, err := tree.Look()
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]Running{
		filepath.Join(tree.Root, "docker", "aaa"): {InactiveBytes: 200 << 20, CPUUsec: 1500000},
		filepath.Join(tree.Root, "docker", "bbb"): {InactiveBytes: 200 << 20},
	}
	if len(s.Running) != len(want) {
		t.Fatalf("running %+v", s.Running)
	}
	for _, r := range s.Running {
		w, ok := want[r.Dir]
		if !ok || r.InactiveBytes != w.InactiveBytes || r.CPUUsec != w.CPUUsec {
			t.Errorf("%+v; want %+v", r, w)
		}
	}
	log := record(t, tree.Root)
	if err := Trim(filepath.Join(tree.Root, "docker", "aaa"), 184<<20); err != nil {
		t.Fatal(err)
	}
	if strings.Join(*log, "\n") != "docker/aaa/memory.reclaim="+strconv.Itoa(184<<20)+" swappiness=0" {
		t.Errorf("a trim wrote %q", *log)
	}
	if err := Trim(filepath.Join(tree.Root, "docker", "gone"), 1<<20); err != nil {
		t.Errorf("a trim of a container gone meanwhile: %v", err)
	}
}

func TestReclaimTellsARefusalFromAShortfall(t *testing.T) {
	tree := engineTree(t)
	s, _ := tree.Look()
	os.RemoveAll(filepath.Join(tree.Root, "docker", "ccc"))
	if err := s.Reclaim(100 << 20); err != nil {
		t.Errorf("a container removed meanwhile: %v", err)
	}
	os.Remove(filepath.Join(tree.Root, "docker", "memory.reclaim"))
	os.Mkdir(filepath.Join(tree.Root, "docker", "memory.reclaim"), 0o755)
	if err := s.Reclaim(2000 << 20); err == nil {
		t.Error("a request the kernel refused was not reported")
	}
}

func TestLookAtSomethingThatIsNotACgroup(t *testing.T) {
	if _, err := (Tree{Root: t.TempDir()}).Look(); err == nil {
		t.Error("an empty directory has a file cache")
	}
	if Exists(t.TempDir()) {
		t.Error("an empty directory is a cgroup")
	}
	root := t.TempDir()
	write(t, filepath.Join(root, "memory.stat"), "anon 1\nactive_file 5\n")
	if _, err := (Tree{Root: root}).Look(); err == nil || !strings.Contains(err.Error(), "1 of the two") {
		t.Errorf("a memory.stat with one list: %v", err)
	}
	if !Exists(root) {
		t.Error("a directory with memory.stat is not a cgroup")
	}
}

func TestPressureReadsTheTenSecondFigure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memory")
	write(t, path, "some avg10=12.34 avg60=1.00 avg300=0.10 total=123456\nfull avg10=3.00 avg60=0.50 avg300=0.00 total=999\n")
	if got, err := Pressure(path); err != nil || got != 12.34 {
		t.Errorf("pressure %v, %v", got, err)
	}
	write(t, path, "full avg10=3.00\n")
	if _, err := Pressure(path); err == nil {
		t.Error("a pressure file with no line for some was read")
	}
	if _, err := Pressure(path + ".none"); err == nil {
		t.Error("a pressure file that does not exist was read")
	}
}
