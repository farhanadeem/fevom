#define NR_ioctl 29
#define NR_mount 40
#define NR_chdir 49
#define NR_close 57
#define NR_lseek 62
#define NR_read 63
#define NR_write 64
#define NR_exit_group 94
#define NR_setresuid 147
#define NR_setresgid 149
#define NR_setgroups 159
#define NR_prctl 167
#define NR_socket 198
#define NR_clone 220
#define NR_execve 221
#define NR_wait4 260
#define NR_setns 268
#define NR_seccomp 277
#define NR_rt_sigaction 134

#define PR_SET_DUMPABLE 4
#define PR_SET_NO_NEW_PRIVS 38
#define PR_CAPBSET_DROP 24

#define CLONE_NEWNS 0x00020000
#define CLONE_NEWUSER 0x10000000
#define SIGCHLD 17

#define SECCOMP_SET_MODE_FILTER 1
#define SECCOMP_FILTER_FLAG_TSYNC 1

#define MS_RDONLY 1
#define MS_NOSUID 2
#define MS_NODEV 4
#define MS_NOEXEC 8
#define MS_REMOUNT 32
#define MS_BIND 4096
#define MS_REC 16384

#define AF_INET 2
#define SOCK_DGRAM 2
#define SIOCGIFFLAGS 0x8913
#define SIOCSIFFLAGS 0x8914
#define IFF_UP 1

#define EPERM 1
#define EINVAL 22
#define ENOENT 2
#define ENOTDIR 20

#define SPEC_FLAG_JOIN_USER 1
#define SPEC_FLAG_FRESH_NS 2

enum {
	SPEC_FD = 3,
	USER_FD = 4,
};

typedef unsigned long u64;
typedef unsigned int u32;
typedef unsigned short u16;
typedef unsigned char u8;

static long sys(long n, long a, long b, long c, long d, long e)
{
	register long x8 __asm__("x8") = n;
	register long x0 __asm__("x0") = a;
	register long x1 __asm__("x1") = b;
	register long x2 __asm__("x2") = c;
	register long x3 __asm__("x3") = d;
	register long x4 __asm__("x4") = e;
	__asm__ volatile("svc #0"
			 : "+r"(x0)
			 : "r"(x8), "r"(x1), "r"(x2), "r"(x3), "r"(x4)
			 : "memory", "cc");
	return x0;
}

static unsigned slen(const char *s)
{
	unsigned n = 0;
	while (s[n])
		n++;
	return n;
}

static void put(const char *s) { sys(NR_write, 2, (long)s, slen(s), 0, 0); }

static void __attribute__((noreturn)) die(const char *what, long ret, int code)
{
	char num[24];
	int i = sizeof num - 1;
	unsigned long e = ret < 0 ? (unsigned long)-ret : (unsigned long)ret;
	num[i] = 0;
	do {
		num[--i] = (char)('0' + e % 10);
		e /= 10;
	} while (e && i > 0);
	put("fev: ");
	put(what);
	put(": errno ");
	put(num + i);
	put("\n");
	for (;;)
		sys(NR_exit_group, code, 0, 0, 0, 0);
}

static void must(long ret, const char *what)
{
	if (ret < 0)
		die(what, ret, 126);
}

static u8 buf[1 << 20];
static unsigned buflen;

static void read_spec(void)
{
	for (;;) {
		long n = sys(NR_read, SPEC_FD, (long)(buf + buflen), (long)(sizeof buf - buflen), 0, 0);
		if (n < 0)
			die("read the stage spec", n, 126);
		if (n == 0)
			break;
		buflen += (unsigned)n;
		if (buflen >= sizeof buf)
			die("the stage spec is too large", 0, 126);
	}
}

static unsigned off;
static void need(unsigned n)
{
	if (off + n > buflen)
		die("the stage spec is truncated", 0, 126);
}
static u32 rd32(void)
{
	need(4);
	u32 v = (u32)buf[off] | ((u32)buf[off + 1] << 8) | ((u32)buf[off + 2] << 16) | ((u32)buf[off + 3] << 24);
	off += 4;
	return v;
}
static u64 rd64(void)
{
	u64 lo = rd32();
	u64 hi = rd32();
	return lo | (hi << 32);
}
static char strbuf[1 << 16];
static unsigned strused;
static char *rdstr(void)
{
	u32 n = rd32();
	need(n);
	if (strused + n + 1 > sizeof strbuf)
		die("the stage spec has too much string data", 0, 126);
	char *out = strbuf + strused;
	for (u32 i = 0; i < n; i++)
		out[i] = (char)buf[off + i];
	out[n] = 0;
	off += n;
	strused += n + 1;
	return out;
}

static void ro_path(const char *p)
{
	long r = sys(NR_mount, (long)p, (long)p, 0, MS_BIND | MS_REC, 0);
	if (r == -ENOENT)
		return;
	must(r, p);
	must(sys(NR_mount, (long)p, (long)p, 0, MS_BIND | MS_REMOUNT | MS_RDONLY | MS_NOSUID | MS_NODEV | MS_NOEXEC, 0), p);
}

static void mask_path(const char *p)
{
	long r = sys(NR_mount, (long)"/dev/null", (long)p, 0, MS_BIND, 0);
	if (r == -ENOENT)
		return;
	if (r == -ENOTDIR)
		r = sys(NR_mount, (long)"tmpfs", (long)p, (long)"tmpfs", MS_RDONLY | MS_NOSUID | MS_NODEV | MS_NOEXEC, (long)"size=0");
	must(r, p);
}

struct sock_fprog {
	u16 len;
	u16 _pad[3];
	const void *filter;
};

struct ifreq {
	char name[16];
	short flags;
	char pad[22];
};

void __attribute__((noreturn, used)) stage_main(long *sp)
{
	long argc = sp[0];
	(void)argc;
	long r;

	must(sys(NR_prctl, PR_SET_DUMPABLE, 0, 0, 0, 0), "not dumpable");
	read_spec();

	u32 magic = rd32();
	if (magic != 0x31474244)
		die("the stage spec has a bad magic", 0, 126);
	u32 flags = rd32();
	u32 uid = rd32();
	u32 gid = rd32();
	u64 capkeep = rd64();
	char *dir = rdstr();
	u32 argn = rd32();
	if (argn == 0 || argn > 4096)
		die("the stage spec has no program", 0, 126);
	static char *argv[4097];
	for (u32 i = 0; i < argn; i++)
		argv[i] = rdstr();
	argv[argn] = 0;
	u32 envn = rd32();
	if (envn > 65535)
		die("the stage spec has too many environment entries", 0, 126);
	static char *envp[65536];
	for (u32 i = 0; i < envn; i++)
		envp[i] = rdstr();
	envp[envn] = 0;

	u32 nlayers = rd32();
	if (nlayers > 64)
		die("the stage spec has too many seccomp layers", 0, 126);
	static const void *layer_ptr[64];
	static u32 layer_len[64];
	for (u32 i = 0; i < nlayers; i++) {
		u32 m = rd32();
		need(m * 8);
		layer_ptr[i] = buf + off;
		layer_len[i] = m;
		off += m * 8;
	}

	if (flags & SPEC_FLAG_JOIN_USER) {
		must(sys(NR_setns, USER_FD, CLONE_NEWUSER, 0, 0, 0), "join the target's user namespace");
		r = sys(NR_setgroups, 0, 0, 0, 0, 0);
		if (r < 0 && r != -EPERM)
			die("setgroups in the target's user namespace", r, 126);
		must(sys(NR_setresgid, 0, 0, 0, 0, 0), "become the target's root (gid)");
		must(sys(NR_setresuid, 0, 0, 0, 0, 0), "become the target's root (uid)");
	}

	{
		struct {
			u64 handler;
			u64 flags;
			u64 restorer;
			u64 mask;
		} act = {1 , 0, 0, 0};
		sys(NR_rt_sigaction, 2 , (long)&act, 0, 8, 0);
		sys(NR_rt_sigaction, 3 , (long)&act, 0, 8, 0);
	}

	long child = sys(NR_clone, SIGCHLD, 0, 0, 0, 0);
	if (child < 0)
		die("fork the session", child, 126);
	if (child > 0) {
		int status = 0;
		long w;
		do {
			w = sys(NR_wait4, child, (long)&status, 0, 0, 0);
		} while (w < 0 && w == -4 );
		int code;
		if ((status & 0x7f) == 0)
			code = (status >> 8) & 0xff;
		else
			code = 128 + (status & 0x7f);
		for (;;)
			sys(NR_exit_group, code, 0, 0, 0, 0);
	}

	if (flags & SPEC_FLAG_FRESH_NS) {
		must(sys(NR_mount, (long)"proc", (long)"/proc", (long)"proc", MS_NOSUID | MS_NODEV | MS_NOEXEC, 0), "mount /proc");
		must(sys(NR_mount, (long)"sysfs", (long)"/sys", (long)"sysfs", MS_RDONLY | MS_NOSUID | MS_NODEV | MS_NOEXEC, 0), "mount /sys");
		must(sys(NR_mount, (long)"cgroup2", (long)"/sys/fs/cgroup", (long)"cgroup2", MS_RDONLY | MS_NOSUID | MS_NODEV | MS_NOEXEC, 0), "mount /sys/fs/cgroup");
		must(sys(NR_mount, (long)"mqueue", (long)"/dev/mqueue", (long)"mqueue", MS_NOSUID | MS_NODEV | MS_NOEXEC, 0), "mount /dev/mqueue");
		{
			static const char *const ro[] = {"/proc/bus", "/proc/fs", "/proc/irq", "/proc/sys", "/proc/sysrq-trigger", 0};
			static const char *const masked[] = {"/proc/acpi", "/proc/asound", "/proc/interrupts", "/proc/kcore",
							     "/proc/keys", "/proc/latency_stats", "/proc/timer_list", "/proc/timer_stats",
							     "/proc/sched_debug", "/proc/scsi", "/sys/firmware", "/sys/devices/virtual/powercap", 0};
			for (int i = 0; ro[i]; i++)
				ro_path(ro[i]);
			for (int i = 0; masked[i]; i++)
				mask_path(masked[i]);
		}
		long s = sys(NR_socket, AF_INET, SOCK_DGRAM, 0, 0, 0);
		if (s >= 0) {
			struct ifreq ifr;
			for (int i = 0; i < (int)sizeof ifr; i++)
				((char *)&ifr)[i] = 0;
			ifr.name[0] = 'l';
			ifr.name[1] = 'o';
			if (sys(NR_ioctl, s, SIOCGIFFLAGS, (long)&ifr, 0, 0) >= 0) {
				ifr.flags |= IFF_UP;
				sys(NR_ioctl, s, SIOCSIFFLAGS, (long)&ifr, 0, 0);
			}
			sys(NR_close, s, 0, 0, 0, 0);
		}
	}

	for (int cap = 0; cap < 64; cap++) {
		if (!(capkeep & (1ULL << cap))) {
			long d = sys(NR_prctl, PR_CAPBSET_DROP, cap, 0, 0, 0);
			if (d < 0 && d != -EINVAL)
				die("drop a capability", d, 126);
		}
	}

	r = sys(NR_setgroups, 0, 0, 0, 0, 0);
	if (r < 0 && r != -EPERM)
		die("setgroups", r, 126);
	if (gid != 0)
		must(sys(NR_setresgid, gid, gid, gid, 0, 0), "become the shell's group");
	if (uid != 0)
		must(sys(NR_setresuid, uid, uid, uid, 0, 0), "become the shell's user");

	must(sys(NR_prctl, PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0), "no_new_privs");
	for (u32 i = 0; i < nlayers; i++) {
		struct sock_fprog fp;
		for (int k = 0; k < (int)sizeof fp; k++)
			((char *)&fp)[k] = 0;
		fp.len = (u16)layer_len[i];
		fp.filter = layer_ptr[i];
		must(sys(NR_seccomp, SECCOMP_SET_MODE_FILTER, SECCOMP_FILTER_FLAG_TSYNC, (long)&fp, 0, 0), "install a seccomp layer");
	}

	sys(NR_close, SPEC_FD, 0, 0, 0, 0);
	sys(NR_close, USER_FD, 0, 0, 0, 0);

	if (sys(NR_chdir, (long)dir, 0, 0, 0, 0) < 0) {
		put("fev: the working directory is not here; starting in /\n");
		sys(NR_chdir, (long)"/", 0, 0, 0, 0);
	}

	r = sys(NR_execve, (long)argv[0], (long)argv, (long)envp, 0, 0);
	die(argv[0], r, r == -ENOENT ? 127 : 126);
}

__asm__(".text\n"
	".global _start\n"
	"_start:\n"
	"	mov x0, sp\n"
	"	bl stage_main\n"
	"	brk #0\n");
