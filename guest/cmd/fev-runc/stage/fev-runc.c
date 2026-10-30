#define NR_openat 56
#define NR_close 57
#define NR_read 63
#define NR_write 64
#define NR_exit_group 94
#define NR_execve 221
#define NR_prctl 167

#define NR_socket 198
#define NR_connect 203
#define NR_setsockopt 208

#define PR_SET_THP_DISABLE 41

#define AF_UNIX 1
#define SOCK_STREAM_CLOEXEC (1 | 02000000)
#define SOL_SOCKET 1
#define SO_RCVTIMEO 20
#define SO_SNDTIMEO 21

#define ENOENT 2
#define EINTR 4
#define AT_FDCWD (-100)
#define O_RDONLY_CLOEXEC 02000000

static const char CREATE[] = "/usr/libexec/fev/fev-runc-create";

static const char FEATURES[] = "/usr/libexec/fev/runc.features";
static const char VERSION[] = "/usr/libexec/fev/runc.version";
static const char KERNEL[] = "/usr/libexec/fev/runc.kernel";
static const char CMDLINE[] = "/usr/libexec/fev/runc.cmdline";
static const char PROC_CMDLINE[] = "/proc/cmdline";
static const char NOTES[] = "/sys/kernel/notes";
#define NT_GNU_BUILD_ID 3
#define MAX_ID 64

static const char *const RUNC[] = {"/usr/bin/runc", "/usr/local/bin/runc", "/usr/sbin/runc"};

static const char VIEW[] = "fev/view/ctl";
#define VIEW_WAIT 3
#define RELEASE_WAIT 30

static const char *const VALUED[] = {"--root", "--log", "--log-format", "--criu", "--rootless"};

static long sys(long n, long a, long b, long c)
{
	register long x8 __asm__("x8") = n;
	register long x0 __asm__("x0") = a;
	register long x1 __asm__("x1") = b;
	register long x2 __asm__("x2") = c;
	__asm__ volatile("svc #0"
			 : "+r"(x0)
			 : "r"(x8), "r"(x1), "r"(x2)
			 : "memory", "cc");
	return x0;
}

static long sys6(long n, long a, long b, long c, long d, long e)
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

static long prctl(long option, long arg2)
{
	register long x8 __asm__("x8") = NR_prctl;
	register long x0 __asm__("x0") = option;
	register long x1 __asm__("x1") = arg2;
	register long x2 __asm__("x2") = 0;
	register long x3 __asm__("x3") = 0;
	register long x4 __asm__("x4") = 0;
	__asm__ volatile("svc #0"
			 : "+r"(x0)
			 : "r"(x8), "r"(x1), "r"(x2), "r"(x3), "r"(x4)
			 : "memory", "cc");
	return x0;
}

static void add(char *buf, long size, long *n, const char *s)
{
	while (*s && *n < size)
		buf[(*n)++] = *s++;
}

static void __attribute__((noreturn)) fail(const char *what, long ret)
{
	char num[24], line[160];
	int i = sizeof num - 1;
	long n = 0;
	unsigned long e = (unsigned long)-ret;
	num[i] = 0;
	do {
		num[--i] = (char)('0' + e % 10);
		e /= 10;
	} while (e && i > 0);
	add(line, sizeof line - 1, &n, "fev-runc: ");
	add(line, sizeof line - 1, &n, what);
	add(line, sizeof line - 1, &n, ": errno ");
	add(line, sizeof line - 1, &n, num + i);
	line[n++] = '\n';
	sys(NR_write, 2, (long)line, n);
	for (;;)
		sys(NR_exit_group, 1, 0, 0);
}

static int same(const char *a, const char *b)
{
	while (*a && *a == *b) {
		a++;
		b++;
	}
	return *a == *b;
}

static const char *base(const char *p)
{
	const char *b = p;
	for (; *p; p++) {
		if (*p == '/')
			b = p + 1;
	}
	return b;
}

static int prefixed(const char *s, const char *prefix)
{
	while (*prefix) {
		if (*s != *prefix)
			return 0;
		s++;
		prefix++;
	}
	return 1;
}

static void answer(const char *path)
{
	char buf[4096];
	long fd = sys(NR_openat, AT_FDCWD, (long)path, O_RDONLY_CLOEXEC);
	if (fd < 0)
		return;
	for (;;) {
		long n = sys(NR_read, fd, (long)buf, sizeof buf);
		if (n == -EINTR)
			continue;
		if (n < 0)
			fail("read its answer", n);
		if (n == 0)
			break;
		for (long off = 0; off < n;) {
			long w = sys(NR_write, 1, (long)(buf + off), n - off);
			if (w == -EINTR)
				continue;
			if (w <= 0)
				fail("write its answer", w);
			off += w;
		}
	}
	sys(NR_close, fd, 0, 0);
	for (;;)
		sys(NR_exit_group, 0, 0, 0);
}

static long slurp(const char *path, char *buf, long size)
{
	long fd = sys(NR_openat, AT_FDCWD, (long)path, O_RDONLY_CLOEXEC), m = 0, r;

	if (fd < 0)
		return -1;
	for (;;) {
		r = sys(NR_read, fd, (long)(buf + m), size - m);
		if (r == -EINTR)
			continue;
		if (r <= 0)
			break;
		m += r;
		if (m == size)
			break;
	}
	sys(NR_close, fd, 0, 0);
	return r < 0 || m == size ? -1 : m;
}

static unsigned long le32(const unsigned char *p)
{
	return p[0] | (unsigned long)p[1] << 8 | (unsigned long)p[2] << 16 | (unsigned long)p[3] << 24;
}

static int same_kernel(void)
{
	unsigned char notes[4096];
	char want[2 * MAX_ID + 1], got[sizeof want + 1];
	static const char digits[] = "0123456789abcdef";
	unsigned long n, off = 0, namesz, descsz, name, desc, end;
	long r, m, w = -1, i;

	r = slurp(NOTES, (char *)notes, sizeof notes);
	if (r < 0)
		return 0;
	n = (unsigned long)r;
	while (off < n) {
		if (n - off < 12)
			return 0;
		namesz = le32(notes + off);
		descsz = le32(notes + off + 4);
		name = off + 12;
		if (namesz > n || descsz > n)
			return 0;
		desc = name + ((namesz + 3) & ~3UL);
		end = desc + ((descsz + 3) & ~3UL);
		if (end > n)
			return 0;
		if (le32(notes + off + 8) == NT_GNU_BUILD_ID && namesz == 4 && notes[name] == 'G' &&
		    notes[name + 1] == 'N' && notes[name + 2] == 'U' && notes[name + 3] == 0) {
			if (w >= 0 || descsz == 0 || descsz > MAX_ID)
				return 0;
			for (w = 0, i = 0; i < (long)descsz; i++) {
				want[w++] = digits[notes[desc + i] >> 4];
				want[w++] = digits[notes[desc + i] & 15];
			}
			want[w++] = '\n';
		}
		off = end;
	}
	if (w < 0)
		return 0;
	m = slurp(KERNEL, got, sizeof got);
	if (m != w)
		return 0;
	for (i = 0; i < w; i++) {
		if (got[i] != want[i])
			return 0;
	}
	return 1;
}

static int blank(char c)
{
	return c == ' ' || c == '\t' || c == '\n';
}

static int same_cmdline(void)
{
	char line[4096], want[4096];
	long r = slurp(PROC_CMDLINE, line, sizeof line), m, w = 0, i = 0, j;

	if (r < 0)
		return 0;
	m = slurp(CMDLINE, want, sizeof want);
	if (m < 0)
		return 0;
	while (i < r) {
		while (i < r && blank(line[i]))
			i++;
		if (i == r)
			break;
		for (j = i; j < r && !blank(line[j]); j++)
			;
		if (j - i < 6 || !prefixed(line + i, "fev.")) {
			if (m - w < j - i + 1)
				return 0;
			for (; i < j; i++) {
				if (want[w++] != line[i])
					return 0;
			}
			if (want[w++] != '\n')
				return 0;
		}
		i = j;
	}
	return w == m;
}

static int is_id(const char *s)
{
	int n = 0;
	for (; *s; s++, n++) {
		if (!((*s >= '0' && *s <= '9') || (*s >= 'a' && *s <= 'f')))
			return 0;
	}
	return n == 64;
}

static int has(const char *s, char c)
{
	for (; *s; s++) {
		if (*s == c)
			return 1;
	}
	return 0;
}

static const char *runc_call(long argc, char **argv, const char **id)
{
	long i = 1, j;
	const char *verb = 0;

	*id = 0;
	for (; i < argc; i++) {
		if (prefixed(argv[i], "--fev-root="))
			continue;
		if (argv[i][0] != '-')
			break;
		for (j = 0; j < (long)(sizeof VALUED / sizeof VALUED[0]); j++) {
			if (same(argv[i], VALUED[j]) && !has(argv[i], '='))
				i++;
		}
	}
	if (i >= argc)
		return 0;
	if (same(argv[i], "create"))
		verb = "attach ";
	else if (same(argv[i], "start"))
		verb = "start ";
	else if (same(argv[i], "delete"))
		verb = "release ";
	else
		return 0;
	for (i++; i < argc; i++) {
		if (argv[i][0] == '-') {
			if (same(argv[i], "--bundle") || same(argv[i], "-b") || same(argv[i], "--console-socket") ||
			    same(argv[i], "--pid-file") || same(argv[i], "--preserve-fds"))
				i++;
			continue;
		}
		*id = argv[i];
	}
	return *id && is_id(*id) ? verb : 0;
}

static void view_call(const char *verb, const char *id, long wait)
{
	struct {
		unsigned short family;
		char path[108];
	} addr;
	struct {
		long sec, usec;
	} tv = {wait, 0};
	char line[96], answer[256];
	long n = 0, fd, r, len, i;

	fd = sys(NR_socket, AF_UNIX, SOCK_STREAM_CLOEXEC, 0);
	if (fd < 0)
		return;
	sys6(NR_setsockopt, fd, SOL_SOCKET, SO_SNDTIMEO, (long)&tv, sizeof tv);
	sys6(NR_setsockopt, fd, SOL_SOCKET, SO_RCVTIMEO, (long)&tv, sizeof tv);
	addr.family = AF_UNIX;
	addr.path[0] = 0;
	for (len = 0; VIEW[len]; len++)
		addr.path[1 + len] = VIEW[len];
	if (sys(NR_connect, fd, (long)&addr, 2 + 1 + len) < 0)
		goto out;
	add(line, sizeof line - 1, &n, verb);
	add(line, sizeof line - 1, &n, id);
	line[n++] = '\n';
	for (i = 0; i < n;) {
		r = sys(NR_write, fd, (long)(line + i), n - i);
		if (r == -EINTR)
			continue;
		if (r <= 0)
			goto out;
		i += r;
	}
	for (i = 0; i < (long)sizeof answer;) {
		r = sys(NR_read, fd, (long)(answer + i), sizeof answer - i);
		if (r == -EINTR)
			continue;
		if (r <= 0)
			break;
		i += r;
		if (answer[i - 1] == '\n')
			break;
	}
out:
	sys(NR_close, fd, 0, 0);
}

void __attribute__((noreturn, used)) stage_main(long *sp)
{
	long argc = sp[0];
	char **argv = (char **)(sp + 1);
	char **envp = argv + argc + 1;
	long i, n, r = -ENOENT;

	prctl(PR_SET_THP_DISABLE, 0);
	{
		const char *id, *verb = runc_call(argc, argv, &id);
		if (verb)
			view_call(verb, id, verb[0] == 'r' ? RELEASE_WAIT : VIEW_WAIT);
	}
	char mode[16];
	int by_name = same(base(argv[0]), "runc");
	for (i = 1; i < argc; i++) {
		int create = same(argv[i], "create");
		if (!create && !same(argv[i], "run"))
			continue;
		if ((create && !by_name) || slurp("/run/fev/emulation.runtime", mode, sizeof mode) > 0) {
			r = sys(NR_execve, (long)CREATE, (long)argv, (long)envp);
			fail("run fev-runc-create", r);
		}
	}
	argv[0] = (char *)"runc";
	for (i = n = 1; i < argc; i++) {
		if (!prefixed(argv[i], "--fev-root="))
			argv[n++] = argv[i];
	}
	argv[n] = 0;
	if (n == 2 && same(argv[1], "features") && same_kernel() && same_cmdline())
		answer(FEATURES);
	if (n == 2 && same(argv[1], "--version"))
		answer(VERSION);
	for (i = 0; i < (long)(sizeof RUNC / sizeof RUNC[0]); i++) {
		r = sys(NR_execve, (long)RUNC[i], (long)argv, (long)envp);
		if (r != -ENOENT)
			break;
	}
	fail("exec runc", r);
}

__asm__(".text\n"
	".global _start\n"
	"_start:\n"
	"	mov x0, sp\n"
	"	bl stage_main\n"
	"	brk #0\n");
