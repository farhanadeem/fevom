#ifndef FEV_VMNET_SHIM_H
#define FEV_VMNET_SHIM_H

#include <stddef.h>
#include <stdint.h>

typedef struct fev_shim fev_shim;

typedef struct {
	const char *mac_ip_text, *mask_text;
	uint8_t mac_ip[4], peer_ip[4];
	uint32_t mtu;
	int tso;
	int test_fd;
	int tun_fd, mdns_fd;
	uintptr_t handle;
} fev_shim_config;

typedef struct {
	uint64_t frames_in, frames_out, wakeups;
	uint64_t to_tunnel, from_tunnel;
	uint64_t arp_answered, arp_ignored;
	uint64_t mdns_up, mdns_ignored, mdns_down;
	uint64_t dropped_ipv6, dropped_other, dropped_no_mac, write_failed, malformed;
} fev_shim_stats;

int fev_shim_start(const fev_shim_config *cfg, fev_shim **out, char *mac_out, size_t mac_len, uint32_t *mtu_out,
    uint64_t *max_out, char *err, size_t err_len);
void fev_shim_set_prefixes(fev_shim *s, const uint8_t *addrs, const uint8_t *bits, int n);
void fev_shim_set_zones(fev_shim *s, const char *zones, size_t len);
void fev_shim_set_mac_mac(fev_shim *s, const uint8_t *mac);
void fev_shim_stats_read(fev_shim *s, fev_shim_stats *out);
void fev_shim_stop(fev_shim *s);

#endif
