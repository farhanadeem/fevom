typedef unsigned long u64;
typedef unsigned int u32;
typedef unsigned short u16;
typedef unsigned char u8;
typedef long i64;

#if defined(__aarch64__)
#define NR_read 63
#define NR_write 64
#define NR_openat 56
#define NR_close 57
#define NR_getdents64 61
#define NR_mkdirat 34
#define NR_symlinkat 36
#define NR_readlinkat 78
#define NR_newfstatat 79
#define NR_chdir 49
#define NR_chroot 51
#define NR_fchdir 50
#define NR_mount 40
#define NR_umount2 39
#define NR_pivot_root 41
#define NR_setns 268
#define NR_unshare 97
#define NR_prctl 167
#define NR_execve 221
#define NR_exit_group 94
#define NR_ptrace 117
#define NR_wait4 260
#define NR_seccomp 277
#define NR_nanosleep 101
#define AUDIT_ARCH_SELF 0xc00000b7u
#define NR_SELF_ptrace 117
#define NR_SELF_vm_readv 270
#define NR_SELF_vm_writev 271
#elif defined(__x86_64__)
#define NR_read 0
#define NR_write 1
#define NR_openat 257
#define NR_close 3
#define NR_getdents64 217
#define NR_mkdirat 258
#define NR_symlinkat 266
#define NR_readlinkat 267
#define NR_newfstatat 262
#define NR_chdir 80
#define NR_chroot 161
#define NR_fchdir 81
#define NR_mount 165
#define NR_umount2 166
#define NR_pivot_root 155
#define NR_setns 308
#define NR_unshare 272
#define NR_prctl 157
#define NR_execve 59
#define NR_exit_group 231
#define NR_ptrace 101
#define NR_wait4 61
#define NR_seccomp 317
#define NR_nanosleep 35
#define AUDIT_ARCH_SELF 0xc000003eu
#define NR_SELF_ptrace 101
#define NR_SELF_vm_readv 310
#define NR_SELF_vm_writev 311
#else
#error "fev-debug-stage: arm64 or x86-64"
#endif
#define NR_open_tree 428
#define NR_move_mount 429
#define NR_fsopen 430
#define NR_fsconfig 431
#define NR_fsmount 432
#define NR_mount_setattr 442

#define AT_FDCWD (-100)
#define AT_EMPTY_PATH 0x1000
#define AT_SYMLINK_NOFOLLOW 0x100
#define AT_RECURSIVE 0x8000
#define O_RDONLY 0
#define O_WRONLY 1
#define O_CREAT 0100
#if defined(__aarch64__)
#define O_DIRECTORY 040000
#else
#define O_DIRECTORY 0200000
#endif
#define O_CLOEXEC 02000000
#define OPEN_TREE_CLONE 1
#define OPEN_TREE_CLOEXEC O_CLOEXEC
#define MOVE_MOUNT_F_EMPTY_PATH 4
#define FSCONFIG_SET_STRING 1
#define FSCONFIG_CMD_CREATE 6
#define MOUNT_ATTR_RDONLY 1
#define MOUNT_ATTR_NODEV 4
#define MS_NOSUID 2
#define MS_NODEV 4
#define MS_REC 16384
#define MS_SLAVE (1 << 19)
#define MNT_DETACH 2
#define CLONE_NEWNS 0x00020000
#define PR_SET_NO_NEW_PRIVS 38
#define PR_CAPBSET_DROP 24
#define PTRACE_DETACH 17
#define PTRACE_SEIZE 0x4206
#define PTRACE_INTERRUPT 0x4207
#define PTRACE_SECCOMP_GET_FILTER 0x420c
#define WALL 0x40000000
#define SECCOMP_SET_MODE_FILTER 1
#define S_IFMT 0170000
#define S_IFDIR 0040000
#define S_IFREG 0100000
#define S_IFLNK 0120000
#define EPERM 1
#define ENOENT 2
#define EINTR 4
#define EINVAL 22
#define EEXIST 17
#define ENOSYS 38

static long sys(long n, long a, long b, long c, long d, long e)
{
#if defined(__aarch64__)
	register long x8 __asm__("x8") = n;
	register long x0 __asm__("x0") = a;
	register long x1 __asm__("x1") = b;
	register long x2 __asm__("x2") = c;
	register long x3 __asm__("x3") = d;
	register long x4 __asm__("x4") = e;
	__asm__ volatile("svc #0" : "+r"(x0) : "r"(x8), "r"(x1), "r"(x2), "r"(x3), "r"(x4) : "memory", "cc");
	return x0;
#else
	long ret;
	register long r10 __asm__("r10") = d;
	register long r8 __asm__("r8") = e;
	__asm__ volatile("syscall" : "=a"(ret) : "a"(n), "D"(a), "S"(b), "d"(c), "r"(r10), "r"(r8) : "rcx", "r11", "memory", "cc");
	return ret;
#endif
}

static unsigned slen(const char *s)
{
	unsigned n = 0;
	while (s[n])
		n++;
	return n;
}
static int seq(const char *a, const char *b)
{
	while (*a && *a == *b)
		a++, b++;
	return *a == *b;
}
static int sprefix(const char *s, const char *p)
{
	while (*p)
		if (*s++ != *p++)
			return 0;
	return 1;
}
static void put(const char *s) { sys(NR_write, 2, (long)s, slen(s), 0, 0); }

static char *num(char *end, unsigned long v)
{
	*--end = 0;
	do {
		*--end = (char)('0' + v % 10);
		v /= 10;
	} while (v);
	return end;
}

static void __attribute__((noreturn)) die(const char *what, long ret, int code)
{
	char b[24];
	put("fev debug: ");
	put(what);
	if (ret < 0) {
		put(": errno ");
		put(num(b + sizeof b, (unsigned long)-ret));
	}
	put("\n");
	for (;;)
		sys(NR_exit_group, code, 0, 0, 0, 0);
}
static long must(long r, const char *what)
{
	if (r < 0)
		die(what, r, 126);
	return r;
}

static char *join(char *dst, unsigned cap, const char *a, const char *b, const char *c)
{
	unsigned n = 0;
	const char *parts[3] = {a, b, c};
	for (int i = 0; i < 3; i++) {
		if (!parts[i])
			continue;
		for (const char *p = parts[i]; *p; p++) {
			if (n + 1 >= cap)
				die("a path too long", 0, 126);
			dst[n++] = *p;
		}
	}
	dst[n] = 0;
	return dst;
}

static char arena[1 << 18];
static unsigned arena_used;
static char *keep(const char *s, unsigned n)
{
	if (arena_used + n + 1 > sizeof arena)
		die("the environment is too large", 0, 126);
	char *out = arena + arena_used;
	for (unsigned i = 0; i < n; i++)
		out[i] = s[i];
	out[n] = 0;
	arena_used += n + 1;
	return out;
}

static long readall(const char *path, char *buf, unsigned cap)
{
	long fd = sys(NR_openat, AT_FDCWD, (long)path, O_RDONLY | O_CLOEXEC, 0, 0);
	if (fd < 0)
		return fd;
	unsigned n = 0;
	for (;;) {
		long r = sys(NR_read, fd, (long)(buf + n), cap - 1 - n, 0, 0);
		if (r == -EINTR)
			continue;
		if (r <= 0)
			break;
		n += (unsigned)r;
		if (n >= cap - 1)
			break;
	}
	sys(NR_close, fd, 0, 0, 0, 0);
	buf[n] = 0;
	return n;
}

static const char *field(const char *status, const char *key)
{
	unsigned k = slen(key);
	const char *p = status;
	while (*p) {
		if (sprefix(p, key) && p[k] == ':') {
			p += k + 1;
			while (*p == ' ' || *p == '\t')
				p++;
			return p;
		}
		while (*p && *p != '\n')
			p++;
		if (*p)
			p++;
	}
	return 0;
}
static u64 hexval(const char *p)
{
	u64 v = 0;
	for (; p && *p; p++) {
		int d = (*p >= '0' && *p <= '9') ? *p - '0' : (*p >= 'a' && *p <= 'f') ? *p - 'a' + 10 : -1;
		if (d < 0)
			break;
		v = v * 16 + (u64)d;
	}
	return v;
}
static long decval(const char *p)
{
	long v = 0;
	if (!p || *p < '0' || *p > '9')
		return -1;
	for (; *p >= '0' && *p <= '9'; p++)
		v = v * 10 + (*p - '0');
	return v;
}

static char *envp[2048];
static int nenv;
static void env_set(const char *kv)
{
	unsigned k = 0;
	while (kv[k] && kv[k] != '=')
		k++;
	if (!kv[k])
		return;
	for (int i = 0; i < nenv; i++) {
		unsigned j = 0;
		while (j <= k && envp[i][j] == kv[j])
			j++;
		if (j > k) {
			envp[i] = keep(kv, slen(kv));
			return;
		}
	}
	if (nenv < 2047)
		envp[nenv++] = keep(kv, slen(kv));
}
static const char *env_get(const char *k)
{
	unsigned n = slen(k);
	for (int i = 0; i < nenv; i++)
		if (sprefix(envp[i], k) && envp[i][n] == '=')
			return envp[i] + n + 1;
	return 0;
}

static int new_tmpfs(void)
{
	int fs = (int)must(sys(NR_fsopen, (long)"tmpfs", 0, 0, 0, 0), "fsopen tmpfs");
	must(sys(NR_fsconfig, fs, FSCONFIG_SET_STRING, (long)"mode", (long)"0755", 0), "fsconfig mode");
	must(sys(NR_fsconfig, fs, FSCONFIG_CMD_CREATE, 0, 0, 0), "fsconfig create");
	int m = (int)must(sys(NR_fsmount, fs, 0, 0, 0, 0), "fsmount");
	sys(NR_close, fs, 0, 0, 0, 0);
	return m;
}
static void attach(int tree, int rootfd, const char *name)
{
	must(sys(NR_move_mount, tree, (long)"", rootfd, (long)name, MOVE_MOUNT_F_EMPTY_PATH), "move_mount");
	sys(NR_close, tree, 0, 0, 0, 0);
}
static void bind_into(int rootfd, const char *src, const char *name, int isdir)
{
	if (isdir) {
		long r = sys(NR_mkdirat, rootfd, (long)name, 0755, 0, 0);
		if (r < 0 && r != -EEXIST)
			die("mkdir in the session's root", r, 126);
	} else {
		long f = must(sys(NR_openat, rootfd, (long)name, O_CREAT | O_WRONLY | O_CLOEXEC, 0644, 0), "create in the session's root");
		sys(NR_close, f, 0, 0, 0, 0);
	}
	int t = (int)must(sys(NR_open_tree, AT_FDCWD, (long)src, OPEN_TREE_CLONE | OPEN_TREE_CLOEXEC | AT_RECURSIVE, 0, 0), "open_tree");
	attach(t, rootfd, name);
}

struct kstat {
	u8 raw[256];
};
static u32 mode_of(struct kstat *st)
{
#if defined(__aarch64__)
	return *(u32 *)(st->raw + 16);
#else
	return *(u32 *)(st->raw + 24);
#endif
}

static void mirror_root(int rootfd)
{
	static u8 dbuf[1 << 16];
	char src[4096], lnk[4096];
	long dfd = must(sys(NR_openat, AT_FDCWD, (long)"/", O_RDONLY | O_DIRECTORY | O_CLOEXEC, 0, 0), "open /");
	for (;;) {
		long n = sys(NR_getdents64, dfd, (long)dbuf, sizeof dbuf, 0, 0);
		if (n < 0)
			die("read /", n, 126);
		if (n == 0)
			break;
		for (long off = 0; off < n;) {
			u16 reclen = *(u16 *)(dbuf + off + 16);
			const char *name = (const char *)(dbuf + off + 19);
			off += reclen;
			if (seq(name, ".") || seq(name, "..") || seq(name, "nix"))
				continue;
			join(src, sizeof src, "/", name, 0);
			struct kstat st;
			if (sys(NR_newfstatat, AT_FDCWD, (long)src, (long)&st, AT_SYMLINK_NOFOLLOW, 0) < 0)
				continue;
			u32 m = mode_of(&st) & S_IFMT;
			if (m == S_IFLNK) {
				long k = sys(NR_readlinkat, AT_FDCWD, (long)src, (long)lnk, sizeof lnk - 1, 0);
				if (k < 0)
					die("readlink", k, 126);
				lnk[k] = 0;
				long r = sys(NR_symlinkat, (long)lnk, rootfd, (long)name, 0, 0);
				if (r < 0 && r != -EEXIST)
					die("symlink in the session's root", r, 126);
			} else if (m == S_IFDIR) {
				bind_into(rootfd, src, name, 1);
			} else if (m == S_IFREG) {
				bind_into(rootfd, src, name, 0);
			}
		}
	}
	sys(NR_close, dfd, 0, 0, 0, 0);
}

static void become_root(int rootfd)
{
	must(sys(NR_move_mount, rootfd, (long)"", AT_FDCWD, (long)"/", MOVE_MOUNT_F_EMPTY_PATH), "move_mount the new root over /");
	must(sys(NR_fchdir, rootfd, 0, 0, 0, 0), "fchdir the new root");
	long r = sys(NR_pivot_root, (long)".", (long)".", 0, 0, 0);
	if (r == 0) {
		must(sys(NR_umount2, (long)".", MNT_DETACH, 0, 0, 0), "detach the old root");
		must(sys(NR_chdir, (long)"/", 0, 0, 0, 0), "chdir /");
		return;
	}
	if (r != -EPERM)
		die("pivot_root", r, 126);
	must(sys(NR_chroot, (long)".", 0, 0, 0, 0), "chroot");
	must(sys(NR_chdir, (long)"/", 0, 0, 0, 0), "chdir /");
}

struct insn {
	u16 code;
	u8 jt, jf;
	u32 k;
};
#define MAX_LAYERS 64
#define MAX_INSNS 32768
static struct insn stack_insns[MAX_INSNS];
static unsigned layer_off[MAX_LAYERS], layer_len[MAX_LAYERS], nlayers;

static void detach(long pid) { sys(NR_ptrace, PTRACE_DETACH, pid, 0, 0, 0); }

static void dump_stack(long pid)
{
	static char status[1 << 14];
	char path[64], b[24];
	join(path, sizeof path, "/proc/", num(b + sizeof b, (unsigned long)pid), "/status");
	if (readall(path, status, sizeof status) < 0)
		die("the target's status", -ENOENT, 126);
	long mode = decval(field(status, "Seccomp"));
	long filters = decval(field(status, "Seccomp_filters"));
	if (mode < 0 || filters < 0)
		die("the target's status has no seccomp lines", 0, 126);
	if (mode == 0 && filters == 0)
		return;
	if (filters > MAX_LAYERS)
		die("the target has more seccomp filters than the stage installs", 0, 126);
	for (int try = 0;; try++) {
		long s = sys(NR_ptrace, PTRACE_SEIZE, pid, 0, 0, 0);
		if (s == 0)
			break;
		if (s != -EPERM || try >= 150)
			die("ptrace seize the target", s, 126);
		readall(path, status, sizeof status);
		const char *tp = field(status, "TracerPid");
		if (try >= 5 && (!tp || decval(tp) == 0))
			die("ptrace seize the target", s, 126);
		struct {
			long sec, nsec;
		} ts = {0, 20000000};
		sys(NR_nanosleep, (long)&ts, 0, 0, 0, 0);
	}
	long r = sys(NR_ptrace, PTRACE_INTERRUPT, pid, 0, 0, 0);
	if (r < 0) {
		detach(pid);
		die("ptrace interrupt the target", r, 126);
	}
	int ws = 0;
	do {
		r = sys(NR_wait4, pid, (long)&ws, WALL, 0, 0);
	} while (r == -EINTR);
	if (r < 0) {
		detach(pid);
		die("wait for the seized target", r, 126);
	}
	unsigned used = 0;
	for (long idx = 0;; idx++) {
		long n = sys(NR_ptrace, PTRACE_SECCOMP_GET_FILTER, pid, idx, 0, 0);
		if (n == -ENOENT && idx > 0)
			break;
		if (n < 0 || n == 0 || n > 4096 || idx >= MAX_LAYERS || used + (unsigned)n > MAX_INSNS) {
			detach(pid);
			die("reading the target's seccomp filters", n < 0 ? n : 0, 126);
		}
		long m = sys(NR_ptrace, PTRACE_SECCOMP_GET_FILTER, pid, idx, (long)(stack_insns + used), 0);
		if (m != n) {
			detach(pid);
			die("reading the target's seccomp filters", m < 0 ? m : 0, 126);
		}
		layer_off[nlayers] = used;
		layer_len[nlayers] = (unsigned)n;
		nlayers++;
		used += (unsigned)n;
	}
	detach(pid);
	if ((long)nlayers != filters || mode != 2)
		die("the target's seccomp filters are not what its status says", 0, 126);
}

static void install_stack(void)
{
	static struct insn prog[4096 + 16];
	struct insn prefix[] = {
		{0x20, 0, 0, 4},
		{0x15, 0, 6, AUDIT_ARCH_SELF},
		{0x20, 0, 0, 0},
		{0x15, 3, 0, NR_SELF_ptrace},
		{0x15, 2, 0, NR_SELF_vm_readv},
		{0x15, 1, 0, NR_SELF_vm_writev},
		{0x05, 0, 0, 1},
		{0x06, 0, 0, 0x7fff0000},
	};
	const unsigned np = sizeof prefix / sizeof prefix[0];
	for (unsigned i = 0; i < nlayers; i++) {
		if (layer_len[i] + np > 4096)
			die("a seccomp filter of the target's too long for the prefix", 0, 126);
		for (unsigned k = 0; k < np; k++)
			prog[k] = prefix[k];
		for (unsigned k = 0; k < layer_len[i]; k++)
			prog[np + k] = stack_insns[layer_off[i] + k];
		struct {
			u16 len;
			u16 pad[3];
			void *filter;
		} fp = {(u16)(np + layer_len[i]), {0, 0, 0}, prog};
		must(sys(NR_seccomp, SECCOMP_SET_MODE_FILTER, 0, (long)&fp, 0, 0), "install a seccomp layer");
	}
}

static char *shell[] = {"/nix/profiles/default/bin/zsh", 0};
#define PROFILE "/nix/profiles/default/bin"

void __attribute__((noreturn, used)) stage_main(long *sp)
{
	long argc = sp[0];
	char **argv = (char **)(sp + 1);
	char **envin = argv + argc + 1;
	int armA = 0, armB = 0, keepcaps = 0;
	long tpid = 0;
	const char *dir = 0;
	char **cmd = 0;
	for (long i = 1; i < argc; i++) {
		const char *a = argv[i];
		if (seq(a, "-A"))
			armA = 1;
		else if (seq(a, "-B"))
			armB = 1;
		else if (seq(a, "-C"))
			keepcaps = 1;
		else if (seq(a, "-p") && i + 1 < argc)
			tpid = decval(argv[++i]);
		else if (seq(a, "-w") && i + 1 < argc)
			dir = argv[++i];
		else if (seq(a, "--")) {
			if (i + 1 < argc)
				cmd = argv + i + 1;
			break;
		} else
			die("usage: fev-debug-stage -A|-B [-C] [-p PID] [-w DIR] [-- ARGV...]", 0, 2);
	}
	if (armA == armB)
		die("usage: fev-debug-stage -A|-B [-C] [-p PID] [-w DIR] [-- ARGV...]", 0, 2);
	if (tpid < 0)
		die("-p takes a pid", 0, 2);
	long target = tpid > 0 ? tpid : 1;
	char b[24], path[128];

	{
		static char eb[1 << 16];
		long n = readall(join(path, sizeof path, "/proc/", num(b + sizeof b, (unsigned long)target), "/environ"), eb, sizeof eb);
		for (long i = 0; i < n;) {
			long j = i;
			while (j < n && eb[j])
				j++;
			if (j > i)
				env_set(keep(eb + i, (unsigned)(j - i)));
			i = j + 1;
		}
	}
	static char status[1 << 14];
	if (readall(join(path, sizeof path, "/proc/", num(b + sizeof b, (unsigned long)target), "/status"), status, sizeof status) < 0)
		die("the target's status", -ENOENT, 126);
	u64 capbnd = hexval(field(status, "CapBnd"));

	if (tpid > 0)
		dump_stack(tpid);

	if (armB) {
		long store = sys(NR_open_tree, AT_FDCWD, (long)"/nix", OPEN_TREE_CLONE | OPEN_TREE_CLOEXEC | AT_RECURSIVE, 0, 0);
		long fsp = store >= 0 ? sys(NR_fsopen, (long)"tmpfs", 0, 0, 0, 0) : -ENOSYS;
		if (store == -ENOSYS || fsp == -ENOSYS) {
			put("fev debug: this engine lacks the mount API for the target-at-/ layout; using the tools-at-/ layout (the target is at /proc/<pid>/root)\n");
			if (store >= 0)
				sys(NR_close, store, 0, 0, 0, 0);
			if (fsp >= 0)
				sys(NR_close, fsp, 0, 0, 0, 0);
			armB = 0;
			armA = 1;
		} else {
			must(store, "open_tree /nix");
			if (fsp >= 0)
				sys(NR_close, fsp, 0, 0, 0, 0);
			struct {
				u64 set, clr, prop, userns;
			} ro = {MOUNT_ATTR_RDONLY | MOUNT_ATTR_NODEV, 0, 0, 0};
			must(sys(NR_mount_setattr, (int)store, (long)"", AT_EMPTY_PATH | AT_RECURSIVE, (long)&ro, sizeof ro), "the store read-only");
			long mnt = must(sys(NR_openat, AT_FDCWD, (long)join(path, sizeof path, "/proc/", num(b + sizeof b, (unsigned long)target), "/ns/mnt"), O_RDONLY | O_CLOEXEC, 0, 0), "the target's mount namespace");
			must(sys(NR_setns, mnt, CLONE_NEWNS, 0, 0, 0), "enter the target's mount namespace (is --cap-add SYS_ADMIN set?)");
			sys(NR_close, mnt, 0, 0, 0, 0);
			must(sys(NR_unshare, CLONE_NEWNS, 0, 0, 0, 0), "unshare the mount namespace");
			must(sys(NR_mount, 0, (long)"/", 0, MS_REC | MS_SLAVE, 0), "make / a slave");
			int root = new_tmpfs();
			mirror_root(root);
			sys(NR_mkdirat, root, (long)"nix", 0755, 0, 0);
			attach((int)store, root, "nix");
			struct {
				u64 set, clr, prop, userns;
			} top = {MOUNT_ATTR_RDONLY, 0, 0, 0};
			must(sys(NR_mount_setattr, root, (long)"", AT_EMPTY_PATH, (long)&top, sizeof top), "the root's top read-only");
			become_root(root);
			long r = sys(NR_mount, (long)"tmpfs", (long)"/tmp", (long)"tmpfs", MS_NOSUID | MS_NODEV, (long)"mode=1777");
			if (r < 0 && r != -ENOENT)
				die("mount the shell's tmp", r, 126);
		}
	}

	{
		static char np[8192];
		const char *p = env_get("PATH");
		join(np, sizeof np, "PATH=" PROFILE ":", p ? p : "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin", 0);
		env_set(np);
	}
	env_set("LANG=C.UTF-8");
	env_set(armB ? "HOME=/tmp" : "HOME=/root");
	env_set("TMPDIR=/tmp");
	env_set("ZDOTDIR=/nix/etc");
	env_set("FEV_PROFILE=/nix/profiles/default");
	env_set("TERMINFO_DIRS=/nix/etc/terminfo:/nix/profiles/default/share/terminfo:");
	env_set("SSL_CERT_FILE=/nix/etc/ssl/ca-bundle.crt");
	env_set("NIX_SSL_CERT_FILE=/nix/etc/ssl/ca-bundle.crt");
	env_set("EDITOR=vim");
	env_set("PAGER=less");
	env_set("TERM=xterm-256color");
	for (char **e = envin; *e; e++)
		if (sprefix(*e, "TERM=") || sprefix(*e, "COLORTERM=") || sprefix(*e, "FEV_DEBUG_TARGET="))
			env_set(*e);
	envp[nenv] = 0;

	if (keepcaps) {
		u64 keepset = capbnd | (1ULL << 19);
		for (int cap = 0; cap < 64; cap++)
			if (!(keepset & (1ULL << cap))) {
				long r = sys(NR_prctl, PR_CAPBSET_DROP, cap, 0, 0, 0);
				if (r < 0 && r != -EINVAL)
					die("drop a capability", r, 126);
			}
	}

	must(sys(NR_prctl, PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0), "no_new_privs");
	install_stack();

	{
		static char d[4300];
		if (armA)
			join(d, sizeof d, "/proc/", num(b + sizeof b, (unsigned long)target), "/root");
		else
			d[0] = 0;
		join(d, sizeof d, d, dir ? dir : (armA ? "" : "/"), 0);
		if (sys(NR_chdir, (long)d, 0, 0, 0, 0) < 0) {
			put("fev debug: the working directory is not here; starting in /\n");
			sys(NR_chdir, (long)"/", 0, 0, 0, 0);
		}
	}

	char **a = cmd ? cmd : shell;
	long r = -ENOENT;
	if (a[0][0] == '/' || (a[0][0] == '.' && a[0][1] == '/')) {
		r = sys(NR_execve, (long)a[0], (long)a, (long)envp, 0, 0);
	} else {
		static char full[4600];
		const char *p = env_get("PATH");
		while (p && *p) {
			unsigned k = 0;
			char dirbuf[4096];
			while (p[k] && p[k] != ':' && k < sizeof dirbuf - 1) {
				dirbuf[k] = p[k];
				k++;
			}
			dirbuf[k] = 0;
			p += k;
			if (*p == ':')
				p++;
			if (!k)
				continue;
			join(full, sizeof full, dirbuf, "/", a[0]);
			long e = sys(NR_execve, (long)full, (long)a, (long)envp, 0, 0);
			if (e != -ENOENT)
				r = e;
		}
	}
	put("fev debug: ");
	put(a[0]);
	put(r == -ENOENT ? ": command not found\n" : ": cannot run it\n");
	for (;;)
		sys(NR_exit_group, r == -ENOENT ? 127 : 126, 0, 0, 0, 0);
}

#if defined(__aarch64__)
__asm__(".text\n"
	".global _start\n"
	"_start:\n"
	"	mov x0, sp\n"
	"	bl stage_main\n"
	"	brk #0\n");
#else
__asm__(".text\n"
	".global _start\n"
	"_start:\n"
	"	mov %rsp, %rdi\n"
	"	and $-16, %rsp\n"
	"	call stage_main\n"
	"	hlt\n");
#endif
