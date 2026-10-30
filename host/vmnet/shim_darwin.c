#include "shim_darwin.h"

#include <arpa/inet.h>
#include <dispatch/dispatch.h>
#include <errno.h>
#include <fcntl.h>
#include <os/lock.h>
#include <stdatomic.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/socket.h>
#include <sys/uio.h>
#include <time.h>
#include <unistd.h>
#include <uuid/uuid.h>
#include <vmnet/vmnet.h>
#include <xpc/xpc.h>

extern void fevVmnetStopped(uintptr_t handle);

enum {
	BATCH = 32,
	MAXPFX = 64,
	ZONES_MAX = 8192,
	ETH = 14,
	TUNHDR = 4,
	AF_INET_UTUN = 2,
};

struct fev_shim {
	dispatch_queue_t q;
	interface_ref iface;
	int test_fd, tun_fd, mdns_fd;
	dispatch_source_t test_src, tun_src, mdns_src;
	dispatch_semaphore_t test_gone, tun_gone, mdns_gone;
	uintptr_t handle;
	size_t max_pkt;
	uint32_t mtu;
	uint8_t mac_ip[4], peer_ip[4];
	uint8_t our_mac[6];
	uint8_t mac_mac[6];
	int mac_known;
	time_t asked;

	os_unfair_lock lock;
	uint32_t pfx_addr[MAXPFX], pfx_mask[MAXPFX];
	int npfx;
	char zones[ZONES_MAX];
	size_t zones_len;

	int stopped;
	uint8_t *rbuf;
	uint8_t *wbuf;
	uint8_t mbuf[9000];

	_Atomic uint64_t frames_in, frames_out, wakeups, to_tunnel, from_tunnel, arp_answered, arp_ignored, mdns_up,
	    mdns_ignored, mdns_down, dropped_ipv6, dropped_other, dropped_no_mac, write_failed, malformed;
};

#define COUNT(s, f) atomic_fetch_add_explicit(&(s)->f, 1, memory_order_relaxed)

static const uint8_t mdns_mac[6] = {0x01, 0x00, 0x5e, 0x00, 0x00, 0xfb};
static const uint8_t mdns_group[4] = {224, 0, 0, 251};

static const char *status_text(vmnet_return_t r) {
	switch (r) {
	case VMNET_SUCCESS: return "VMNET_SUCCESS";
	case VMNET_FAILURE: return "VMNET_FAILURE";
	case VMNET_MEM_FAILURE: return "VMNET_MEM_FAILURE";
	case VMNET_INVALID_ARGUMENT: return "VMNET_INVALID_ARGUMENT";
	case VMNET_SETUP_INCOMPLETE: return "VMNET_SETUP_INCOMPLETE";
	case VMNET_INVALID_ACCESS: return "VMNET_INVALID_ACCESS";
	case VMNET_PACKET_TOO_BIG: return "VMNET_PACKET_TOO_BIG";
	case VMNET_BUFFER_EXHAUSTED: return "VMNET_BUFFER_EXHAUSTED";
	case VMNET_TOO_MANY_PACKETS: return "VMNET_TOO_MANY_PACKETS";
	case VMNET_SHARING_SERVICE_BUSY: return "VMNET_SHARING_SERVICE_BUSY";
	case VMNET_NOT_AUTHORIZED: return "VMNET_NOT_AUTHORIZED";
	default: return "an unknown vmnet status";
	}
}

static uint32_t be32(const uint8_t *p) { return (uint32_t)p[0] << 24 | (uint32_t)p[1] << 16 | (uint32_t)p[2] << 8 | p[3]; }
static uint16_t be16(const uint8_t *p) { return (uint16_t)(p[0] << 8 | p[1]); }
static void put16(uint8_t *p, uint16_t v) {
	p[0] = v >> 8;
	p[1] = v & 0xff;
}

static uint32_t sum16(const uint8_t *p, size_t n, uint32_t s) {
	for (; n > 1; n -= 2, p += 2)
		s += (uint32_t)(p[0] << 8 | p[1]);
	if (n)
		s += (uint32_t)p[0] << 8;
	return s;
}
static uint16_t fold(uint32_t s) {
	while (s >> 16)
		s = (s & 0xffff) + (s >> 16);
	return (uint16_t)~s;
}

static int answers(fev_shim *s, const uint8_t *a) {
	if (memcmp(a, s->mac_ip, 4) == 0)
		return 0;
	if (memcmp(a, s->peer_ip, 4) == 0)
		return 1;
	uint32_t v = be32(a);
	int yes = 0;
	os_unfair_lock_lock(&s->lock);
	for (int i = 0; i < s->npfx && !yes; i++)
		yes = (v & s->pfx_mask[i]) == s->pfx_addr[i];
	os_unfair_lock_unlock(&s->lock);
	return yes;
}

static void write_frames(fev_shim *s, uint8_t **f, size_t *len, int n) {
	if (n == 0)
		return;
	if (s->iface == NULL) {
		for (int i = 0; i < n; i++) {
			if (send(s->test_fd, f[i], len[i], 0) < 0)
				COUNT(s, write_failed);
			else
				COUNT(s, frames_out);
		}
		return;
	}
	struct iovec iov[BATCH];
	struct vmpktdesc d[BATCH];
	for (int i = 0; i < n; i++) {
		iov[i].iov_base = f[i];
		iov[i].iov_len = len[i];
		d[i].vm_pkt_size = len[i];
		d[i].vm_pkt_iov = &iov[i];
		d[i].vm_pkt_iovcnt = 1;
		d[i].vm_flags = 0;
	}
	int c = n;
	vmnet_return_t r = vmnet_write(s->iface, d, &c);
	if (r != VMNET_SUCCESS) {
		atomic_fetch_add_explicit(&s->write_failed, (uint64_t)n, memory_order_relaxed);
		return;
	}
	atomic_fetch_add_explicit(&s->frames_out, (uint64_t)c, memory_order_relaxed);
	if (c < n)
		atomic_fetch_add_explicit(&s->write_failed, (uint64_t)(n - c), memory_order_relaxed);
}

static void write_one(fev_shim *s, uint8_t *f, size_t n) { write_frames(s, &f, &n, 1); }

static size_t arp_frame(uint8_t *r, int op, const uint8_t *dst_mac, const uint8_t *sha, const uint8_t *sip,
    const uint8_t *tha, const uint8_t *tip, const uint8_t *src_mac) {
	memcpy(r, dst_mac, 6);
	memcpy(r + 6, src_mac, 6);
	r[12] = 0x08;
	r[13] = 0x06;
	r[14] = 0;
	r[15] = 1;
	r[16] = 0x08;
	r[17] = 0;
	r[18] = 6;
	r[19] = 4;
	r[20] = 0;
	r[21] = (uint8_t)op;
	memcpy(r + 22, sha, 6);
	memcpy(r + 28, sip, 4);
	memcpy(r + 32, tha, 6);
	memcpy(r + 38, tip, 4);
	return 42;
}

static void ask_mac(fev_shim *s) {
	time_t now = time(NULL);
	if (now == s->asked)
		return;
	s->asked = now;
	static const uint8_t bcast[6] = {0xff, 0xff, 0xff, 0xff, 0xff, 0xff}, zero[6] = {0};
	uint8_t r[42];
	arp_frame(r, 1, bcast, s->our_mac, s->peer_ip, zero, s->mac_ip, s->our_mac);
	write_one(s, r, sizeof r);
}

static void learn(fev_shim *s, const uint8_t *mac) {
	if (mac[0] & 1)
		return;
	if (!s->mac_known || memcmp(s->mac_mac, mac, 6) != 0) {
		memcpy(s->mac_mac, mac, 6);
		s->mac_known = 1;
	}
}

static void handle_arp(fev_shim *s, uint8_t *f, size_t n) {
	if (n < 42 || be16(f + 14) != 1 || be16(f + 16) != 0x0800 || f[18] != 6 || f[19] != 4) {
		COUNT(s, malformed);
		return;
	}
	uint16_t op = be16(f + 20);
	const uint8_t *sha = f + 22, *sip = f + 28, *tip = f + 38;
	if (memcmp(sip, s->mac_ip, 4) == 0)
		learn(s, sha);
	if (op != 1 || !answers(s, tip)) {
		COUNT(s, arp_ignored);
		return;
	}
	uint8_t r[42];
	arp_frame(r, 2, f + 6, s->our_mac, tip, sha, sip, s->our_mac);
	write_one(s, r, sizeof r);
	COUNT(s, arp_answered);
}

static int zone_wanted(fev_shim *s, const char *name, size_t nl) {
	int yes = 0;
	os_unfair_lock_lock(&s->lock);
	for (size_t o = 0; o < s->zones_len && !yes;) {
		const char *z = s->zones + o;
		size_t zl = strlen(z);
		if (zl == 0)
			break;
		if (nl == zl && memcmp(name, z, zl) == 0)
			yes = 1;
		else if (nl > zl && name[nl - zl - 1] == '.' && memcmp(name + nl - zl, z, zl) == 0)
			yes = 1;
		o += zl + 1;
	}
	os_unfair_lock_unlock(&s->lock);
	return yes;
}

static int mdns_wanted(fev_shim *s, const uint8_t *m, size_t n) {
	if (n < 12 || (m[2] & 0x80))
		return 0;
	int qd = be16(m + 4);
	size_t o = 12;
	for (int i = 0; i < qd && i < 64; i++) {
		char name[256];
		size_t w = 0, p = o;
		int jumped = 0, hops = 0;
		for (;;) {
			if (p >= n)
				return 0;
			uint8_t l = m[p];
			if ((l & 0xc0) == 0xc0) {
				if (p + 1 >= n || ++hops > 16)
					return 0;
				size_t to = (size_t)(l & 0x3f) << 8 | m[p + 1];
				if (!jumped)
					o = p + 2;
				jumped = 1;
				p = to;
				continue;
			}
			if (l & 0xc0)
				return 0;
			p++;
			if (l == 0)
				break;
			if (p + l > n || w + l + 1 >= sizeof name)
				return 0;
			if (w)
				name[w++] = '.';
			for (int k = 0; k < l; k++) {
				char c = (char)m[p + k];
				name[w++] = (c >= 'A' && c <= 'Z') ? (char)(c + 32) : c;
			}
			p += l;
		}
		if (!jumped)
			o = p;
		name[w] = 0;
		if (o + 4 > n)
			return 0;
		o += 4;
		if (zone_wanted(s, name, w))
			return 1;
	}
	return 0;
}

static void handle_ipv4(fev_shim *s, uint8_t *f, size_t n) {
	uint8_t *ip = f + ETH;
	size_t ihl = (size_t)(ip[0] & 15) * 4;
	if (n < ETH + 20 || (ip[0] >> 4) != 4 || ihl < 20 || n < ETH + ihl) {
		COUNT(s, malformed);
		return;
	}
	size_t total = be16(ip + 2);
	if (total < ihl || total > n - ETH) {
		COUNT(s, malformed);
		return;
	}
	if (memcmp(f, s->our_mac, 6) == 0) {
		if (memcmp(ip + 12, s->mac_ip, 4) == 0)
			learn(s, f + 6);
		uint8_t *d = ip - TUNHDR;
		d[0] = d[1] = d[2] = 0;
		d[3] = AF_INET_UTUN;
		if (send(s->tun_fd, d, TUNHDR + total, 0) < 0)
			COUNT(s, write_failed);
		else
			COUNT(s, to_tunnel);
		return;
	}
	if (memcmp(f, mdns_mac, 6) == 0 && memcmp(ip + 16, mdns_group, 4) == 0 && ip[9] == 17 && total >= ihl + 8 &&
	    be16(ip + ihl + 2) == 5353) {
		const uint8_t *udp = ip + ihl, *m = udp + 8;
		size_t ml = total - ihl - 8;
		if (memcmp(ip + 12, s->mac_ip, 4) == 0)
			learn(s, f + 6);
		if (!mdns_wanted(s, m, ml)) {
			COUNT(s, mdns_ignored);
			return;
		}
		uint8_t hdr[6];
		memcpy(hdr, ip + 12, 4);
		memcpy(hdr + 4, udp, 2);
		struct iovec iov[2] = {{hdr, 6}, {(void *)m, ml}};
		struct msghdr mh = {.msg_iov = iov, .msg_iovlen = 2};
		if (sendmsg(s->mdns_fd, &mh, 0) < 0)
			COUNT(s, write_failed);
		else
			COUNT(s, mdns_up);
		return;
	}
	COUNT(s, dropped_other);
}

static void handle_frame(fev_shim *s, uint8_t *f, size_t n) {
	COUNT(s, frames_in);
	if (n < ETH) {
		COUNT(s, malformed);
		return;
	}
	switch (be16(f + 12)) {
	case 0x0806: handle_arp(s, f, n); return;
	case 0x0800: handle_ipv4(s, f, n); return;
	case 0x86dd: COUNT(s, dropped_ipv6); return;
	default: COUNT(s, dropped_other); return;
	}
}

static void stopped_by_itself(fev_shim *s, const char *why) {
	if (s->stopped)
		return;
	s->stopped = 1;
	fprintf(stderr, "fev vmnet: the interface stopped: %s\n", why);
	if (s->tun_src)
		dispatch_source_cancel(s->tun_src);
	if (s->mdns_src)
		dispatch_source_cancel(s->mdns_src);
	if (s->test_src)
		dispatch_source_cancel(s->test_src);
	fevVmnetStopped(s->handle);
}

static void read_vmnet(fev_shim *s) {
	COUNT(s, wakeups);
	for (;;) {
		struct iovec iov[BATCH];
		struct vmpktdesc d[BATCH];
		for (int i = 0; i < BATCH; i++) {
			iov[i].iov_base = s->rbuf + (size_t)i * s->max_pkt;
			iov[i].iov_len = s->max_pkt;
			d[i].vm_pkt_size = s->max_pkt;
			d[i].vm_pkt_iov = &iov[i];
			d[i].vm_pkt_iovcnt = 1;
			d[i].vm_flags = 0;
		}
		int c = BATCH;
		vmnet_return_t r = vmnet_read(s->iface, d, &c);
		if (r != VMNET_SUCCESS) {
			stopped_by_itself(s, status_text(r));
			return;
		}
		for (int i = 0; i < c; i++)
			handle_frame(s, s->rbuf + (size_t)i * s->max_pkt, d[i].vm_pkt_size);
		if (c < BATCH)
			return;
	}
}

static void read_test(fev_shim *s) {
	COUNT(s, wakeups);
	for (int i = 0; i < BATCH; i++) {
		ssize_t n = recv(s->test_fd, s->rbuf, s->max_pkt, 0);
		if (n < 0)
			return;
		handle_frame(s, s->rbuf, (size_t)n);
	}
}

static void read_tunnel(fev_shim *s) {
	uint8_t *f[BATCH];
	size_t len[BATCH];
	int n = 0;
	for (int i = 0; i < BATCH; i++) {
		uint8_t *b = s->wbuf + (size_t)n * s->max_pkt;
		ssize_t got = recv(s->tun_fd, b + ETH - TUNHDR, s->max_pkt - (ETH - TUNHDR), 0);
		if (got < 0)
			break;
		COUNT(s, from_tunnel);
		if (got < TUNHDR + 20 || be32(b + ETH - TUNHDR) != AF_INET_UTUN) {
			COUNT(s, malformed);
			continue;
		}
		if (!s->mac_known) {
			COUNT(s, dropped_no_mac);
			ask_mac(s);
			continue;
		}
		memcpy(b, s->mac_mac, 6);
		memcpy(b + 6, s->our_mac, 6);
		b[12] = 0x08;
		b[13] = 0x00;
		f[n] = b;
		len[n] = (size_t)got + ETH - TUNHDR;
		n++;
	}
	write_frames(s, f, len, n);
}

static void read_mdns(fev_shim *s) {
	for (int i = 0; i < BATCH; i++) {
		uint8_t *b = s->mbuf;
		uint8_t *m = b + ETH + 20 + 8 - 6;
		ssize_t got = recv(s->mdns_fd, m, sizeof s->mbuf - (ETH + 20 + 8 - 6), 0);
		if (got < 0)
			return;
		if (got < 6 + 12 || (size_t)got - 6 + ETH + 28 > s->max_pkt) {
			COUNT(s, malformed);
			continue;
		}
		uint8_t dst[4], port[2];
		memcpy(dst, m, 4);
		memcpy(port, m + 4, 2);
		size_t ml = (size_t)got - 6;
		int group = memcmp(dst, mdns_group, 4) == 0;
		if (!group && !s->mac_known) {
			COUNT(s, dropped_no_mac);
			ask_mac(s);
			continue;
		}
		memcpy(b, group ? mdns_mac : s->mac_mac, 6);
		memcpy(b + 6, s->our_mac, 6);
		b[12] = 0x08;
		b[13] = 0x00;
		uint8_t *ip = b + ETH, *udp = ip + 20;
		size_t ul = 8 + ml;
		memset(ip, 0, 20);
		ip[0] = 0x45;
		put16(ip + 2, (uint16_t)(20 + ul));
		ip[8] = 255;
		ip[9] = 17;
		memcpy(ip + 12, s->peer_ip, 4);
		memcpy(ip + 16, dst, 4);
		put16(ip + 10, fold(sum16(ip, 20, 0)));
		put16(udp, 5353);
		memcpy(udp + 2, port, 2);
		put16(udp + 4, (uint16_t)ul);
		put16(udp + 6, 0);
		uint32_t ps = sum16(ip + 12, 8, 0) + 17 + (uint32_t)ul;
		uint16_t c = fold(sum16(udp, ul, ps));
		put16(udp + 6, c ? c : 0xffff);
		write_one(s, b, ETH + 20 + ul);
		COUNT(s, mdns_down);
	}
}

static int pair_end(int fd) {
	int buf = 1 << 20;
	if (fcntl(fd, F_SETFL, fcntl(fd, F_GETFL) | O_NONBLOCK) < 0)
		return -1;
	fcntl(fd, F_SETFD, FD_CLOEXEC);
	setsockopt(fd, SOL_SOCKET, SO_SNDBUF, &buf, sizeof buf);
	setsockopt(fd, SOL_SOCKET, SO_RCVBUF, &buf, sizeof buf);
	return 0;
}

static dispatch_source_t reader(fev_shim *s, int fd, void (*fn)(fev_shim *), dispatch_semaphore_t *gone) {
	dispatch_source_t src = dispatch_source_create(DISPATCH_SOURCE_TYPE_READ, (uintptr_t)fd, 0, s->q);
	dispatch_semaphore_t g = dispatch_semaphore_create(0);
	*gone = g;
	dispatch_source_set_event_handler(src, ^{
	  if (!s->stopped)
		  fn(s);
	});
	dispatch_source_set_cancel_handler(src, ^{
	  dispatch_semaphore_signal(g);
	});
	dispatch_resume(src);
	return src;
}

static void parse_mac(const char *t, uint8_t *mac) {
	unsigned m[6];
	if (t && sscanf(t, "%x:%x:%x:%x:%x:%x", &m[0], &m[1], &m[2], &m[3], &m[4], &m[5]) == 6)
		for (int i = 0; i < 6; i++)
			mac[i] = (uint8_t)m[i];
}

static void free_shim(fev_shim *s) {
	if (s->q)
		dispatch_release(s->q);
	free(s->rbuf);
	free(s->wbuf);
	free(s);
}

int fev_shim_start(const fev_shim_config *cfg, fev_shim **out, char *mac_out, size_t mac_len, uint32_t *mtu_out,
    uint64_t *max_out, char *err, size_t err_len) {
	fev_shim *s = calloc(1, sizeof *s);
	if (!s) {
		snprintf(err, err_len, "out of memory");
		return 1;
	}
	s->lock = OS_UNFAIR_LOCK_INIT;
	s->test_fd = cfg->test_fd;
	s->tun_fd = cfg->tun_fd;
	s->mdns_fd = cfg->mdns_fd;
	s->handle = cfg->handle;
	memcpy(s->mac_ip, cfg->mac_ip, 4);
	memcpy(s->peer_ip, cfg->peer_ip, 4);
	s->mtu = cfg->mtu ? cfg->mtu : 1500;
	s->max_pkt = ETH + s->mtu;
	if (pair_end(s->tun_fd) < 0 || pair_end(s->mdns_fd) < 0 || (s->test_fd >= 0 && pair_end(s->test_fd) < 0)) {
		snprintf(err, err_len, "the socket pairs: %s", strerror(errno));
		free_shim(s);
		return 1;
	}
	s->q = dispatch_queue_create("dev.fevom.vmnet", DISPATCH_QUEUE_SERIAL);
	if (s->test_fd >= 0) {
		static const uint8_t test_mac[6] = {0x02, 0x00, 0xbe, 0x27, 0x0b, 0x01};
		memcpy(s->our_mac, test_mac, 6);
	} else {
		xpc_object_t d = xpc_dictionary_create(NULL, NULL, 0);
		xpc_dictionary_set_uint64(d, vmnet_operation_mode_key, VMNET_HOST_MODE);
		uuid_t u;
		uuid_generate_random(u);
		xpc_dictionary_set_uuid(d, vmnet_interface_id_key, u);
		xpc_dictionary_set_bool(d, vmnet_allocate_mac_address_key, true);
		xpc_dictionary_set_string(d, vmnet_host_ip_address_key, cfg->mac_ip_text);
		xpc_dictionary_set_string(d, vmnet_host_subnet_mask_key, cfg->mask_text);
		uuid_generate_random(u);
		xpc_dictionary_set_uuid(d, vmnet_network_identifier_key, u);
		xpc_dictionary_set_uint64(d, vmnet_mtu_key, s->mtu);
		if (cfg->tso)
			xpc_dictionary_set_bool(d, vmnet_enable_tso_key, true);
		dispatch_semaphore_t sem = dispatch_semaphore_create(0);
		__block vmnet_return_t res = VMNET_FAILURE;
		__block struct {
			char s[32];
		} mac = {""};
		__block uint64_t mtu = 0, max = 0;
		interface_ref iface = vmnet_start_interface(d, s->q, ^(vmnet_return_t status, xpc_object_t p) {
		  res = status;
		  if (status == VMNET_SUCCESS && p) {
			  const char *m = xpc_dictionary_get_string(p, vmnet_mac_address_key);
			  if (m)
				  snprintf(mac.s, sizeof mac.s, "%s", m);
			  mtu = xpc_dictionary_get_uint64(p, vmnet_mtu_key);
			  max = xpc_dictionary_get_uint64(p, vmnet_max_packet_size_key);
		  }
		  dispatch_semaphore_signal(sem);
		});
		xpc_release(d);
		if (!iface) {
			snprintf(err, err_len, "vmnet refused the interface at once (an MTU of %u, or no entitlement)", s->mtu);
			free_shim(s);
			return 1;
		}
		if (dispatch_semaphore_wait(sem, dispatch_time(DISPATCH_TIME_NOW, 20LL * NSEC_PER_SEC))) {
			snprintf(err, err_len, "vmnet did not start the interface in 20 s");
			free_shim(s);
			return 1;
		}
		if (res != VMNET_SUCCESS) {
			snprintf(err, err_len, "vmnet did not start the interface: %s%s", status_text(res),
			    res == VMNET_FAILURE ? " (fevd lacks the com.apple.security.virtualization entitlement, or macOS refused the network: its log, process InternetSharing, says why)" : "");
			free_shim(s);
			return 1;
		}
		s->iface = iface;
		parse_mac(mac.s, s->our_mac);
		if (mtu)
			s->mtu = (uint32_t)mtu;
		if (max)
			s->max_pkt = (size_t)max;
	}
	s->rbuf = malloc((size_t)BATCH * s->max_pkt);
	s->wbuf = malloc((size_t)BATCH * s->max_pkt);
	if (!s->rbuf || !s->wbuf) {
		snprintf(err, err_len, "out of memory");
		fev_shim_stop(s);
		return 1;
	}
	s->tun_src = reader(s, s->tun_fd, read_tunnel, &s->tun_gone);
	s->mdns_src = reader(s, s->mdns_fd, read_mdns, &s->mdns_gone);
	if (s->iface) {
		vmnet_return_t r = vmnet_interface_set_event_callback(s->iface, VMNET_INTERFACE_PACKETS_AVAILABLE, s->q,
		    ^(interface_event_t e, xpc_object_t x) {
		      (void)e;
		      (void)x;
		      if (!s->stopped)
			      read_vmnet(s);
		    });
		if (r != VMNET_SUCCESS) {
			snprintf(err, err_len, "vmnet's packet callback: %s", status_text(r));
			fev_shim_stop(s);
			return 1;
		}
	} else
		s->test_src = reader(s, s->test_fd, read_test, &s->test_gone);
	snprintf(mac_out, mac_len, "%02x:%02x:%02x:%02x:%02x:%02x", s->our_mac[0], s->our_mac[1], s->our_mac[2],
	    s->our_mac[3], s->our_mac[4], s->our_mac[5]);
	*mtu_out = s->mtu;
	*max_out = s->max_pkt;
	*out = s;
	return 0;
}

void fev_shim_set_prefixes(fev_shim *s, const uint8_t *addrs, const uint8_t *bits, int n) {
	os_unfair_lock_lock(&s->lock);
	s->npfx = 0;
	for (int i = 0; i < n && i < MAXPFX; i++) {
		if (bits[i] > 32)
			continue;
		uint32_t m = bits[i] == 0 ? 0 : 0xffffffffu << (32 - bits[i]);
		s->pfx_mask[s->npfx] = m;
		s->pfx_addr[s->npfx] = be32(addrs + 4 * i) & m;
		s->npfx++;
	}
	os_unfair_lock_unlock(&s->lock);
}

void fev_shim_set_zones(fev_shim *s, const char *zones, size_t len) {
	os_unfair_lock_lock(&s->lock);
	if (len > sizeof s->zones - 1)
		len = 0;
	memcpy(s->zones, zones, len);
	s->zones[len] = 0;
	s->zones_len = len;
	os_unfair_lock_unlock(&s->lock);
}

void fev_shim_set_mac_mac(fev_shim *s, const uint8_t *mac) {
	struct {
		uint8_t b[6];
	} m;
	memcpy(m.b, mac, 6);
	dispatch_async(s->q, ^{
	  if (!s->mac_known)
		  learn(s, m.b);
	});
}

void fev_shim_stats_read(fev_shim *s, fev_shim_stats *o) {
#define R(f) o->f = atomic_load_explicit(&s->f, memory_order_relaxed)
	R(frames_in);
	R(frames_out);
	R(wakeups);
	R(to_tunnel);
	R(from_tunnel);
	R(arp_answered);
	R(arp_ignored);
	R(mdns_up);
	R(mdns_ignored);
	R(mdns_down);
	R(dropped_ipv6);
	R(dropped_other);
	R(dropped_no_mac);
	R(write_failed);
	R(malformed);
#undef R
}

void fev_shim_stop(fev_shim *s) {
	dispatch_sync(s->q, ^{
	  s->stopped = 1;
	  if (s->iface)
		  vmnet_interface_set_event_callback(s->iface, VMNET_INTERFACE_PACKETS_AVAILABLE, NULL, NULL);
	});
	dispatch_source_t srcs[3] = {s->tun_src, s->mdns_src, s->test_src};
	dispatch_semaphore_t gone[3] = {s->tun_gone, s->mdns_gone, s->test_gone};
	for (int i = 0; i < 3; i++)
		if (srcs[i]) {
			dispatch_source_cancel(srcs[i]);
			dispatch_semaphore_wait(gone[i], DISPATCH_TIME_FOREVER);
			dispatch_release(srcs[i]);
			dispatch_release(gone[i]);
		}
	if (s->iface) {
		dispatch_semaphore_t sem = dispatch_semaphore_create(0);
		if (vmnet_stop_interface(s->iface, s->q, ^(vmnet_return_t r) {
			    (void)r;
			    dispatch_semaphore_signal(sem);
		    }) == VMNET_SUCCESS)
			dispatch_semaphore_wait(sem, dispatch_time(DISPATCH_TIME_NOW, 10LL * NSEC_PER_SEC));
		s->iface = NULL;
	}
	close(s->tun_fd);
	close(s->mdns_fd);
	if (s->test_fd >= 0)
		close(s->test_fd);
	dispatch_sync(s->q, ^{
	});
	free_shim(s);
}
