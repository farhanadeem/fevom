/* SPDX-License-Identifier: GPL-2.0 WITH Linux-syscall-note */
#ifndef _UAPI_LINUX_FEV_FSNOTIFY_H
#define _UAPI_LINUX_FEV_FSNOTIFY_H

#include <linux/ioctl.h>
#include <linux/types.h>

#define FEV_FSN_NAME_MAX	255

#define FEV_FSN_CHANGE	1
#define FEV_FSN_CREATE	2
#define FEV_FSN_DELETE	3
#define FEV_FSN_RENAME	4
#define FEV_FSN_INVAL_ALL	5

#define FEV_FSN_F_ISDIR	(1 << 0)
#define FEV_FSN_F_LAST_LINK	(1 << 1)
#define FEV_FSN_F_QUIET	(1 << 2)
#define FEV_FSN_F_VICTIM_GONE	(1 << 3)

#define FEV_FSN_M_MODIFY	(1 << 0)
#define FEV_FSN_M_ATTRIB	(1 << 1)
#define FEV_FSN_M_CLOSE_WRITE	(1 << 2)
#define FEV_FSN_M_OPEN	(1 << 3)

struct fev_fsn_event {
	__u32	op;
	__u32	flags;
	__s32	fd;
	__u32	mask;
	__u64	parent;
	__u64	child;
	__u64	parent2;
	__u64	victim;
	__u16	namelen;
	__u16	namelen2;
	__u32	reserved;
	char	name[FEV_FSN_NAME_MAX + 1];
	char	name2[FEV_FSN_NAME_MAX + 1];
};

#define FEV_FSN_HAD_PARENT	(1 << 0)
#define FEV_FSN_HAD_CHILD	(1 << 1)
#define FEV_FSN_HAD_ENTRY	(1 << 2)
#define FEV_FSN_HAD_PARENT2	(1 << 3)
#define FEV_FSN_HAD_VICTIM	(1 << 4)
#define FEV_FSN_MOVED		(1 << 5)

#define FEV_FSN_EMIT	_IOW('b', 1, struct fev_fsn_event)

#endif
