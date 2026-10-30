// SPDX-License-Identifier: GPL-2.0-only
#include <linux/capability.h>
#include <linux/dcache.h>
#include <linux/file.h>
#include <linux/fs.h>
#include <linux/fsnotify.h>
#include <linux/init.h>
#include <linux/iversion.h>
#include <linux/magic.h>
#include <linux/miscdevice.h>
#include <linux/moduleparam.h>
#include <linux/namei.h>
#include <linux/pagemap.h>
#include <linux/sysfs.h>
#include <linux/uaccess.h>
#include <uapi/linux/fev_fsnotify.h>

#include "fuse_i.h"

#define BFN_HAD_PARENT	FEV_FSN_HAD_PARENT
#define BFN_HAD_CHILD	FEV_FSN_HAD_CHILD
#define BFN_HAD_ENTRY	FEV_FSN_HAD_ENTRY
#define BFN_HAD_PARENT2	FEV_FSN_HAD_PARENT2
#define BFN_HAD_VICTIM	FEV_FSN_HAD_VICTIM
#define BFN_MOVED	FEV_FSN_MOVED

#define BFN_MOVE_TRIES	3

static atomic_t bfn_opened;

static bool fev_lock_wait;
module_param(fev_lock_wait, bool, 0644);
MODULE_PARM_DESC(fev_lock_wait,
		 "fev: the event device waits for locks, as before (tests only)");

static atomic_long_t bfn_busy, bfn_epochs, bfn_unmoved;

static int bfn_count_get(char *buf, const struct kernel_param *kp)
{
	return sysfs_emit(buf, "%ld\n", atomic_long_read(kp->arg));
}

static const struct kernel_param_ops bfn_count_ops = {
	.get = bfn_count_get,
};
module_param_cb(fev_lock_busy, &bfn_count_ops, &bfn_busy, 0444);
module_param_cb(fev_lock_epochs, &bfn_count_ops, &bfn_epochs, 0444);
module_param_cb(fev_lock_unmoved, &bfn_count_ops, &bfn_unmoved, 0444);

static bool bfn_trylock_dir(struct inode *dir)
{
	if (READ_ONCE(fev_lock_wait)) {
		inode_lock_nested(dir, I_MUTEX_PARENT);
		return true;
	}
	return inode_trylock(dir);
}

static bool bfn_lock_dir(struct fuse_conn *fc, struct inode *dir)
{
	if (bfn_trylock_dir(dir))
		return true;
	atomic_long_inc(&bfn_busy);
	if (inode_trylock_shared(dir)) {
		inode_unlock_shared(dir);
		return false;
	}
	if (IS_ENABLED(CONFIG_FUSE_FEV_CACHE) && fc->fev_cache) {
		atomic_inc(&fc->epoch);
		atomic_long_inc(&bfn_epochs);
	}
	return false;
}

static struct inode *bfn_inode(struct fuse_conn *fc, u64 nodeid)
{
	return nodeid ? fuse_ilookup(fc, nodeid, NULL) : NULL;
}

static void bfn_dir_changed(struct inode *dir)
{
	fuse_invalidate_attr_mask(dir, FUSE_STATX_MODDIR);
	inode_maybe_inc_iversion(dir, false);
	fuse_fev_dir_incomplete(dir);
}

static struct dentry *bfn_entry(struct inode *dir, struct qstr *name)
{
	struct dentry *parent = d_find_alias(dir);
	struct dentry *entry;

	if (!parent)
		return NULL;
	name->hash = full_name_hash(parent, name->name, name->len);
	entry = d_lookup(parent, name);
	dput(parent);
	return entry;
}

static struct dentry *bfn_expire(struct inode *dir, struct qstr *name)
{
	struct fuse_conn *fc = get_fuse_conn(dir);
	struct dentry *entry;
	bool held;

	held = bfn_lock_dir(fc, dir);
	atomic_inc(&fc->fev_events);
	fuse_fev_dir_record(dir);
	bfn_dir_changed(dir);
	entry = bfn_entry(dir, name);
	if (entry)
		fuse_invalidate_entry_cache(entry);
	if (held)
		inode_unlock(dir);
	return entry;
}

static void bfn_drop(struct inode *dir, struct dentry *entry, bool all)
{
	if (!entry)
		return;
	if ((all || d_really_is_negative(entry)) && bfn_trylock_dir(dir)) {
		d_invalidate(entry);
		inode_unlock(dir);
	}
	dput(entry);
}

static void bfn_inval_inode(struct fuse_conn *fc, struct inode *inode, bool data)
{
	fuse_reverse_inval_inode(fc, get_node_id(inode), data ? 0 : -1, 0);
}

static void bfn_child_event(const struct path *path, struct inode *dir,
			    const struct qstr *name, struct inode *inode,
			    u32 mask)
{
	if (path) {
		fsnotify_parent(path->dentry, mask, path, FSNOTIFY_EVENT_PATH);
		return;
	}
	if (S_ISDIR(inode->i_mode))
		mask |= FS_ISDIR;
	if (dir && fsnotify_inode_watches_children(dir))
		fsnotify(mask | FS_EVENT_ON_CHILD, inode, FSNOTIFY_EVENT_INODE,
			 dir, name, inode, 0);
	else
		fsnotify(mask, inode, FSNOTIFY_EVENT_INODE, NULL, NULL, inode, 0);
}

static void bfn_name_event(struct inode *dir, const struct qstr *name,
			   struct inode *inode, u32 mask, bool isdir, u32 cookie)
{
	if (isdir)
		mask |= FS_ISDIR;
	fsnotify_name(mask, inode ?: dir, FSNOTIFY_EVENT_INODE, dir, name, cookie);
}

static void bfn_unlinked(struct fuse_conn *fc, struct inode *inode, bool last,
			 bool attrib)
{
	struct fuse_inode *fi = get_fuse_inode(inode);

	bfn_inval_inode(fc, inode, false);
	if (last) {
		if (READ_ONCE(fev_lock_wait))
			inode_lock(inode);
		else if (!inode_trylock(inode))
			goto out;
		spin_lock(&fi->lock);
		fi->attr_version = atomic64_inc_return(&fc->attr_version);
		clear_nlink(inode);
		spin_unlock(&fi->lock);
		inode_unlock(inode);
	}
out:
	if (attrib)
		fsnotify_link_count(inode);
}

static void bfn_unlinked_done(struct inode *inode, bool last, bool quiet)
{
	bool unnamed;

	if (!last || quiet)
		return;
	spin_lock(&inode->i_lock);
	unnamed = hlist_empty(&inode->i_dentry);
	spin_unlock(&inode->i_lock);
	if (unnamed)
		fsnotify_inoderemove(inode);
}

static struct dentry *bfn_move_target(struct dentry *parent, struct qstr *name,
				      struct inode *inode)
{
	struct inode *dir = d_inode(parent);
	struct dentry *d = NULL;

	if (!bfn_trylock_dir(dir)) {
		atomic_long_inc(&bfn_unmoved);
		return NULL;
	}
	if (IS_DEADDIR(dir))
		goto out;
	name->hash = full_name_hash(parent, name->name, name->len);
	d = d_lookup(parent, name);
	if (d && d_really_is_positive(d) && d_inode(d) != inode) {
		d_invalidate(d);
		dput(d);
		d = NULL;
	}
	if (!d) {
		d = d_alloc(parent, name);
		if (d)
			d_add(d, NULL);
	}
out:
	inode_unlock(dir);
	return d;
}

static struct dentry *bfn_trap(struct dentry *p1, struct dentry *p2)
{
	struct dentry *p = p1, *q = p2, *r;

	while ((r = p->d_parent) != p2 && r != p)
		p = r;
	if (r == p2)
		return p;
	while ((r = q->d_parent) != p1 && r != p && r != q)
		q = r;
	if (r == p1)
		return q;
	if (likely(r == p))
		return NULL;
	return ERR_PTR(-EXDEV);
}

static int bfn_start_moving(struct renamedata *rd, struct dentry *src,
			    struct dentry *target)
{
	struct mutex *rename_mutex = &src->d_sb->s_vfs_rename_mutex;
	struct dentry *p1, *p2 = rd->new_parent, *trap = NULL;
	int err;

	if (READ_ONCE(fev_lock_wait))
		return start_renaming_two_dentries(rd, src, target);

	if (!mutex_trylock(rename_mutex))
		return -EBUSY;
	p1 = src->d_parent;
	if (p1 == p2) {
		if (!inode_trylock(d_inode(p2))) {
			mutex_unlock(rename_mutex);
			return -EBUSY;
		}
		mutex_unlock(rename_mutex);
	} else {
		trap = bfn_trap(p1, p2);
		if (IS_ERR(trap)) {
			mutex_unlock(rename_mutex);
			return PTR_ERR(trap);
		}
		if (!inode_trylock(d_inode(p1))) {
			mutex_unlock(rename_mutex);
			return -EBUSY;
		}
		if (!inode_trylock(d_inode(p2))) {
			inode_unlock(d_inode(p1));
			mutex_unlock(rename_mutex);
			return -EBUSY;
		}
	}

	err = -EINVAL;
	if (d_unhashed(src) || (rd->old_parent && rd->old_parent != p1))
		goto out_unlock;
	if (d_unhashed(target) || target->d_parent != p2)
		goto out_unlock;
	if (src == trap)
		goto out_unlock;
	err = -ENOTEMPTY;
	if (target == trap)
		goto out_unlock;
	err = -EEXIST;
	if (d_is_positive(target) && (rd->flags & RENAME_NOREPLACE))
		goto out_unlock;

	rd->old_dentry = dget(src);
	rd->new_dentry = dget(target);
	rd->old_parent = dget(p1);
	return 0;

out_unlock:
	inode_unlock(d_inode(p1));
	if (p1 != p2) {
		inode_unlock(d_inode(p2));
		mutex_unlock(rename_mutex);
	}
	return err;
}

static bool bfn_move_dir(struct inode *inode, struct inode *dir2,
			 struct qstr *name2)
{
	struct dentry *src = d_find_alias(inode);
	struct dentry *parent2 = d_find_alias(dir2);
	bool moved = false;
	int tries;

	if (!src || !parent2)
		goto out;
	for (tries = 0; tries < BFN_MOVE_TRIES && !moved; tries++) {
		struct renamedata rd = { .new_parent = parent2 };
		struct dentry *target;
		int err;

		if (IS_ROOT(src) || d_unhashed(src) || d_unlinked(parent2))
			break;
		target = bfn_move_target(parent2, name2, inode);
		if (!target)
			break;
		if (target == src) {
			dput(target);
			moved = true;
			break;
		}
		err = bfn_start_moving(&rd, src, target);
		if (!err) {
			if (d_really_is_negative(target) && d_inode(src) == inode) {
				bfn_dir_changed(d_inode(rd.old_parent));
				d_move(src, target);
				moved = true;
			}
			end_renaming(&rd);
		}
		dput(target);
		if (err == -EBUSY)
			atomic_long_inc(&bfn_unmoved);
		if (err && err != -EINVAL)
			break;
	}
out:
	dput(parent2);
	dput(src);
	return moved;
}

static int bfn_change(struct fuse_conn *fc, struct vfsmount *mnt,
		      const struct fev_fsn_event *ev, struct qstr *name)
{
	struct inode *dir = bfn_inode(fc, ev->parent);
	struct inode *inode;
	bool quiet = ev->flags & FEV_FSN_F_QUIET;
	struct path path = { .mnt = mnt }, *via = NULL;
	int had = 0;

	if (dir && !S_ISDIR(dir->i_mode)) {
		iput(dir);
		dir = NULL;
	}
	if (dir) {
		fuse_fev_dir_record(dir);
		if (READ_ONCE(fev_lock_wait)) {
			inode_lock_nested(dir, I_MUTEX_PARENT);
			inode_unlock(dir);
		}
	}
	inode = bfn_inode(fc, ev->child);
	if (dir)
		had |= BFN_HAD_PARENT;
	if (inode)
		had |= BFN_HAD_CHILD;

	if (!inode && dir && name->len && !quiet &&
	    fsnotify_inode_watches_children(dir)) {
		struct dentry *parent = d_find_alias(dir);
		struct dentry *d = NULL;

		if (parent && READ_ONCE(fev_lock_wait)) {
			d = lookup_noperm_positive_unlocked(name, parent);
		} else if (parent && inode_trylock(dir)) {
			d = lookup_noperm(name, parent);
			inode_unlock(dir);
		}
		dput(parent);
		if (d && !IS_ERR(d)) {
			if (d_really_is_positive(d))
				inode = igrab(d_inode(d));
			dput(d);
		} else if (!d) {
			struct fuse_entry_out outarg;
			struct inode *found;

			if (!fuse_lookup_name(dir->i_sb, get_node_id(dir), name,
					      &outarg, &found))
				inode = found;
		}
	}
	if (!inode)
		goto out;

	bfn_inval_inode(fc, inode, ev->mask & FEV_FSN_M_MODIFY);
	if (quiet)
		goto out;
	if (!name->len) {
		iput(dir);
		dir = NULL;
	}
	if (dir) {
		path.dentry = bfn_entry(dir, name);
		if (path.dentry && d_inode(path.dentry) == inode &&
		    is_subdir(path.dentry, mnt->mnt_root))
			via = &path;
	}
	if (ev->mask & FEV_FSN_M_OPEN)
		bfn_child_event(via, dir, name, inode, FS_OPEN);
	if (ev->mask & FEV_FSN_M_MODIFY)
		bfn_child_event(via, dir, name, inode, FS_MODIFY);
	if (ev->mask & FEV_FSN_M_ATTRIB)
		bfn_child_event(via, dir, name, inode, FS_ATTRIB);
	if (ev->mask & FEV_FSN_M_CLOSE_WRITE)
		bfn_child_event(via, dir, name, inode, FS_CLOSE_WRITE);
	dput(path.dentry);
out:
	iput(inode);
	iput(dir);
	return had;
}

static int bfn_create(struct fuse_conn *fc, const struct fev_fsn_event *ev,
		      struct qstr *name)
{
	struct inode *dir = bfn_inode(fc, ev->parent);
	struct dentry *entry;
	int had;

	if (!dir)
		return 0;
	if (!S_ISDIR(dir->i_mode) || !name->len) {
		iput(dir);
		return -ENOTDIR;
	}
	entry = bfn_expire(dir, name);
	had = BFN_HAD_PARENT | (entry ? BFN_HAD_ENTRY : 0);
	bfn_drop(dir, entry, false);
	if (!(ev->flags & FEV_FSN_F_QUIET))
		bfn_name_event(dir, name, NULL, FS_CREATE,
			       ev->flags & FEV_FSN_F_ISDIR, 0);
	iput(dir);
	return had;
}

static int bfn_delete(struct fuse_conn *fc, const struct fev_fsn_event *ev,
		      struct qstr *name)
{
	struct inode *dir = bfn_inode(fc, ev->parent);
	struct inode *inode = bfn_inode(fc, ev->child);
	bool last = ev->flags & FEV_FSN_F_LAST_LINK;
	bool quiet = ev->flags & FEV_FSN_F_QUIET;
	bool isdir = ev->flags & FEV_FSN_F_ISDIR;
	struct dentry *entry = NULL;
	int had = 0;

	if (dir && (!S_ISDIR(dir->i_mode) || !name->len)) {
		iput(dir);
		dir = NULL;
	}
	if (dir) {
		entry = bfn_expire(dir, name);
		had |= BFN_HAD_PARENT | (entry ? BFN_HAD_ENTRY : 0);
	}
	if (inode) {
		had |= BFN_HAD_CHILD;
		isdir = S_ISDIR(inode->i_mode);
		last = last || isdir;
		bfn_unlinked(fc, inode, last, !quiet && !isdir);
	}
	if (dir)
		bfn_drop(dir, entry, true);
	if (inode)
		bfn_unlinked_done(inode, last, quiet);
	if (dir && !quiet)
		bfn_name_event(dir, name, inode, FS_DELETE, isdir, 0);
	iput(inode);
	iput(dir);
	return had;
}

static int bfn_rename(struct fuse_conn *fc, const struct fev_fsn_event *ev,
		      struct qstr *name, struct qstr *name2)
{
	struct inode *dir = bfn_inode(fc, ev->parent);
	struct inode *dir2 = bfn_inode(fc, ev->parent2);
	struct inode *inode = bfn_inode(fc, ev->child);
	struct inode *victim = bfn_inode(fc, ev->victim);
	bool gone = ev->flags & FEV_FSN_F_VICTIM_GONE;
	bool quiet = ev->flags & FEV_FSN_F_QUIET;
	bool isdir = ev->flags & FEV_FSN_F_ISDIR;
	struct dentry *entry = NULL, *entry2 = NULL;
	u32 cookie = fsnotify_get_cookie();
	int had = 0;

	if (dir && (!S_ISDIR(dir->i_mode) || !name->len)) {
		iput(dir);
		dir = NULL;
	}
	if (dir2 && (!S_ISDIR(dir2->i_mode) || !name2->len)) {
		iput(dir2);
		dir2 = NULL;
	}
	if (victim == inode) {
		iput(victim);
		victim = NULL;
	}
	if (inode) {
		had |= BFN_HAD_CHILD;
		isdir = S_ISDIR(inode->i_mode);
		bfn_inval_inode(fc, inode, false);
	}
	if (victim)
		gone = gone || S_ISDIR(victim->i_mode);

	if (inode && isdir && dir2 && bfn_move_dir(inode, dir2, name2))
		had |= BFN_MOVED;

	if (dir) {
		entry = bfn_expire(dir, name);
		had |= BFN_HAD_PARENT | (entry ? BFN_HAD_ENTRY : 0);
	}
	if (entry && (had & BFN_MOVED) && d_inode(entry) == inode) {
		dput(entry);
		entry = NULL;
	}
	if (dir2 && (had & BFN_MOVED)) {
		bool held = bfn_trylock_dir(dir2);

		fuse_fev_dir_record(dir2);
		bfn_dir_changed(dir2);
		if (held)
			inode_unlock(dir2);
		had |= BFN_HAD_PARENT2;
	} else if (dir2) {
		entry2 = bfn_expire(dir2, name2);
		had |= BFN_HAD_PARENT2;
	}
	if (!quiet) {
		if (dir)
			bfn_name_event(dir, name, inode, FS_MOVED_FROM, isdir, cookie);
		if (dir2)
			bfn_name_event(dir2, name2, inode, FS_MOVED_TO, isdir, cookie);
	}
	if (victim) {
		had |= BFN_HAD_VICTIM;
		bfn_unlinked(fc, victim, gone, !quiet);
	}
	if (inode && !quiet)
		fsnotify_inode(inode, FS_MOVE_SELF);
	if (dir)
		bfn_drop(dir, entry, true);
	if (dir2)
		bfn_drop(dir2, entry2, true);
	if (victim)
		bfn_unlinked_done(victim, gone, quiet);

	iput(victim);
	iput(inode);
	iput(dir2);
	iput(dir);
	return had;
}

static int bfn_inval_all(struct fuse_conn *fc, struct super_block *sb)
{
	struct inode *inode, *toput = NULL;
	struct dentry *alias;

	if (IS_ENABLED(CONFIG_FUSE_FEV_CACHE) && fc->fev_cache)
		atomic_inc(&fc->epoch);
	atomic_inc(&fc->fev_events);
	shrink_dcache_sb(sb);

	spin_lock(&sb->s_inode_list_lock);
	list_for_each_entry(inode, &sb->s_inodes, i_sb_list) {
		spin_lock(&inode->i_lock);
		if (inode_state_read(inode) & (I_FREEING | I_WILL_FREE | I_NEW)) {
			spin_unlock(&inode->i_lock);
			continue;
		}
		hlist_for_each_entry(alias, &inode->i_dentry, d_alias)
			fuse_invalidate_entry_cache(alias);
		__iget(inode);
		spin_unlock(&inode->i_lock);
		spin_unlock(&sb->s_inode_list_lock);

		if (S_ISDIR(inode->i_mode))
			bfn_dir_changed(inode);
		bfn_inval_inode(fc, inode, S_ISREG(inode->i_mode));
		iput(toput);
		toput = inode;

		cond_resched();
		spin_lock(&sb->s_inode_list_lock);
	}
	spin_unlock(&sb->s_inode_list_lock);
	iput(toput);
	return 0;
}

static int bfn_name(struct qstr *q, const char *buf, unsigned int len)
{
	if (len > FEV_FSN_NAME_MAX)
		return -ENAMETOOLONG;
	if (memchr(buf, '/', len) || memchr(buf, 0, len))
		return -EINVAL;
	if ((len == 1 && buf[0] == '.') ||
	    (len == 2 && buf[0] == '.' && buf[1] == '.'))
		return -EINVAL;
	*q = QSTR_LEN(buf, len);
	return 0;
}

static long bfn_ioctl(struct file *file, unsigned int cmd, unsigned long arg)
{
	struct fev_fsn_event *ev __free(kfree) = NULL;
	struct qstr name, name2;
	struct super_block *sb;
	struct fuse_conn *fc;
	int ret;

	if (cmd != FEV_FSN_EMIT)
		return -ENOTTY;

	ev = memdup_user((void __user *)arg, sizeof(*ev));
	if (IS_ERR(ev))
		return PTR_ERR(no_free_ptr(ev));
	if (ev->reserved ||
	    (ev->flags & ~(FEV_FSN_F_ISDIR | FEV_FSN_F_LAST_LINK |
			   FEV_FSN_F_QUIET | FEV_FSN_F_VICTIM_GONE)) ||
	    (ev->mask & ~(FEV_FSN_M_MODIFY | FEV_FSN_M_ATTRIB |
			  FEV_FSN_M_CLOSE_WRITE | FEV_FSN_M_OPEN)))
		return -EINVAL;
	ret = bfn_name(&name, ev->name, ev->namelen);
	if (!ret)
		ret = bfn_name(&name2, ev->name2, ev->namelen2);
	if (ret)
		return ret;

	CLASS(fd_raw, f)(ev->fd);
	if (fd_empty(f))
		return -EBADF;
	sb = file_inode(fd_file(f))->i_sb;
	if (sb->s_magic != FUSE_SUPER_MAGIC)
		return -EINVAL;
	fc = get_fuse_conn_super(sb);

	atomic_inc(&fc->fev_records);

	down_read(&fc->killsb);
	switch (ev->op) {
	case FEV_FSN_CHANGE:
		ret = bfn_change(fc, fd_file(f)->f_path.mnt, ev, &name);
		break;
	case FEV_FSN_CREATE:
		ret = bfn_create(fc, ev, &name);
		break;
	case FEV_FSN_DELETE:
		ret = bfn_delete(fc, ev, &name);
		break;
	case FEV_FSN_RENAME:
		ret = bfn_rename(fc, ev, &name, &name2);
		break;
	case FEV_FSN_INVAL_ALL:
		ret = bfn_inval_all(fc, sb);
		break;
	default:
		ret = -EINVAL;
	}
	up_read(&fc->killsb);
	return ret;
}

static int bfn_open(struct inode *inode, struct file *file)
{
	if (!capable(CAP_SYS_ADMIN))
		return -EPERM;
	if (atomic_cmpxchg(&bfn_opened, 0, 1))
		return -EBUSY;
	return 0;
}

static int bfn_release(struct inode *inode, struct file *file)
{
	atomic_set(&bfn_opened, 0);
	return 0;
}

static const struct file_operations bfn_fops = {
	.owner		= THIS_MODULE,
	.open		= bfn_open,
	.release	= bfn_release,
	.unlocked_ioctl	= bfn_ioctl,
	.compat_ioctl	= compat_ptr_ioctl,
	.llseek		= noop_llseek,
};

static struct miscdevice bfn_dev = {
	.minor	= MISC_DYNAMIC_MINOR,
	.name	= "fev-fsnotify",
	.fops	= &bfn_fops,
	.mode	= 0600,
};

static int __init bfn_init(void)
{
	return misc_register(&bfn_dev);
}
device_initcall(bfn_init);
