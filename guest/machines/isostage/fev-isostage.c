#define NR_fcntl 25
#define NR_close 57
#define NR_write 64
#define NR_exit_group 94
#define NR_setresuid 147
#define NR_setresgid 149
#define NR_setgroups 159
#define NR_prctl 167
#define NR_setns 268
#define NR_execveat 281

#define PR_SET_DUMPABLE 4
#define CLONE_NEWNS 0x00020000
#define CLONE_NEWCGROUP 0x02000000
#define CLONE_NEWUSER 0x10000000
#define F_SETFD 2
#define FD_CLOEXEC 1
#define AT_EMPTY_PATH 0x1000
#define EPERM 1

enum {
	MOUNT_FD = 4,
	CGROUP_FD = 5,
	USER_FD = 6,
	SELF_FD = 8,
};

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

static void put(const char *s)
{
	long n = 0;
	while (s[n])
		n++;
	sys(NR_write, 2, (long)s, n, 0, 0);
}

static void __attribute__((noreturn)) fail(const char *what, long ret)
{
	char num[24];
	int i = sizeof num - 1;
	unsigned long e = (unsigned long)-ret;
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
		sys(NR_exit_group, 126, 0, 0, 0, 0);
}

static void must(long ret, const char *what)
{
	if (ret < 0)
		fail(what, ret);
}

void __attribute__((noreturn, used)) stage_main(long *sp)
{
	long argc = sp[0];
	char **argv = (char **)(sp + 1);
	char **envp = argv + argc + 1;
	long r;

	must(sys(NR_prctl, PR_SET_DUMPABLE, 0, 0, 0, 0), "not dumpable");
	must(sys(NR_setns, USER_FD, CLONE_NEWUSER, 0, 0, 0), "join the machine's user namespace");
	must(sys(NR_setns, MOUNT_FD, CLONE_NEWNS, 0, 0, 0), "enter the machine's mount namespace");
	must(sys(NR_setns, CGROUP_FD, CLONE_NEWCGROUP, 0, 0, 0), "enter the machine's cgroup namespace");
	r = sys(NR_setgroups, 0, 0, 0, 0, 0);
	if (r < 0 && r != -EPERM)
		fail("setgroups", r);
	must(sys(NR_setresgid, 0, 0, 0, 0, 0), "become the machine's root (gid)");
	must(sys(NR_setresuid, 0, 0, 0, 0, 0), "become the machine's root (uid)");
	must(sys(NR_prctl, PR_SET_DUMPABLE, 0, 0, 0, 0), "not dumpable");
	sys(NR_close, MOUNT_FD, 0, 0, 0, 0);
	sys(NR_close, CGROUP_FD, 0, 0, 0, 0);
	sys(NR_close, USER_FD, 0, 0, 0, 0);
	must(sys(NR_fcntl, SELF_FD, F_SETFD, FD_CLOEXEC, 0, 0), "the user stage's descriptor");
	r = sys(NR_execveat, SELF_FD, (long)"", (long)argv, (long)envp, AT_EMPTY_PATH);
	fail("run the session's user stage", r);
}

__asm__(".text\n"
	".global _start\n"
	"_start:\n"
	"	mov x0, sp\n"
	"	bl stage_main\n"
	"	brk #0\n");
