#include <stdint.h>

int fev_power_register(void);
void fev_power_unregister(void);
void fev_power_allow(long id);
int fev_power_assert(const char *reason, int display, uint32_t *id);
void fev_power_release(uint32_t id);
