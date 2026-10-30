#include "power_darwin.h"

#include <CoreFoundation/CoreFoundation.h>
#include <IOKit/IOKitLib.h>
#include <IOKit/IOMessage.h>
#include <IOKit/pwr_mgt/IOPMLib.h>
#include <dispatch/dispatch.h>

extern void fevPowerEvent(int kind, long id);

static io_connect_t root_port;
static IONotificationPortRef notify_port;
static io_object_t notifier;
static dispatch_queue_t queue;

static void power_changed(void *ref, io_service_t service, natural_t type, void *arg) {
	(void)ref;
	(void)service;
	switch (type) {
	case kIOMessageCanSystemSleep:
		IOAllowPowerChange(root_port, (long)arg);
		break;
	case kIOMessageSystemWillSleep:
		fevPowerEvent(1, (long)arg);
		break;
	case kIOMessageSystemHasPoweredOn:
		fevPowerEvent(2, 0);
		break;
	case kIOMessageSystemWillPowerOn:
		fevPowerEvent(3, 0);
		break;
	case kIOMessageSystemWillNotSleep:
		fevPowerEvent(4, 0);
		break;
	}
}

int fev_power_register(void) {
	if (root_port != MACH_PORT_NULL) {
		return -2;
	}
	root_port = IORegisterForSystemPower(NULL, &notify_port, power_changed, &notifier);
	if (root_port == MACH_PORT_NULL) {
		return -1;
	}
	queue = dispatch_queue_create("fev.power", DISPATCH_QUEUE_SERIAL);
	IONotificationPortSetDispatchQueue(notify_port, queue);
	return 0;
}

void fev_power_unregister(void) {
	if (root_port == MACH_PORT_NULL) {
		return;
	}
	IODeregisterForSystemPower(&notifier);
	IOServiceClose(root_port);
	IONotificationPortDestroy(notify_port);
	dispatch_release(queue);
	root_port = MACH_PORT_NULL;
}

void fev_power_allow(long id) {
	if (root_port != MACH_PORT_NULL) {
		IOAllowPowerChange(root_port, id);
	}
}

int fev_power_assert(const char *reason, int display, uint32_t *id) {
	CFStringRef name = CFStringCreateWithCString(kCFAllocatorDefault, reason, kCFStringEncodingUTF8);
	if (name == NULL) {
		return -1;
	}
	IOPMAssertionID assertion = 0;
	IOReturn r = IOPMAssertionCreateWithName(
		display ? kIOPMAssertPreventUserIdleDisplaySleep : kIOPMAssertPreventUserIdleSystemSleep,
		kIOPMAssertionLevelOn, name, &assertion);
	CFRelease(name);
	*id = assertion;
	return (int)r;
}

void fev_power_release(uint32_t id) { IOPMAssertionRelease(id); }
