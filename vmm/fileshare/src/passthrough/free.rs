use std::collections::{HashMap, VecDeque};
use std::os::fd::OwnedFd;
use std::sync::Arc;

use super::*;

pub(super) const MAX_FREE_FDS: usize = 4096;

pub(super) const MAX_DIR_FDS: usize = 64;

pub(super) struct Fds {
    map: HashMap<u64, Arc<OwnedFd>>,
    order: VecDeque<u64>,
    max: usize,
}

impl Fds {
    pub(super) fn new(max: usize) -> Self {
        Self {
            map: HashMap::new(),
            order: VecDeque::new(),
            max,
        }
    }

    pub(super) fn clear(&mut self) {
        self.map.clear();
        self.order.clear();
    }

    pub(super) fn get(&self, node: u64) -> Option<Arc<OwnedFd>> {
        self.map.get(&node).cloned()
    }

    pub(super) fn remove(&mut self, node: u64) {
        self.map.remove(&node);
    }

    pub(super) fn insert(&mut self, node: u64, fd: Arc<OwnedFd>) {
        if self.map.insert(node, fd).is_none() {
            self.order.push_back(node);
        }
        while self.map.len() > self.max {
            match self.order.pop_front() {
                Some(old) if old != node => {
                    self.map.remove(&old);
                }
                Some(_) => self.order.push_back(node),
                None => break,
            }
        }
        if self.order.len() > 2 * self.max {
            let map = &self.map;
            self.order.retain(|n| map.contains_key(n));
        }
    }

    #[cfg(test)]
    pub(super) fn len(&self) -> usize {
        self.map.len()
    }
}

impl Passthrough {
    pub(super) fn free_fd(&self, node: u64) -> Result<Arc<OwnedFd>> {
        if let Some(fd) = self.free_fds.lock().unwrap().get(node) {
            return Ok(fd);
        }
        if let Some(fd) = self.orphan(node) {
            return Ok(fd);
        }
        let _tree = self.tree_read(node);
        let fd = Arc::new(self.open_node(node, S_IFREG, libc::O_RDONLY)?);
        let keep = self
            .nodes
            .lock()
            .unwrap()
            .by_id
            .get(&node)
            .is_some_and(|n| n.free);
        if keep {
            self.free_fds.lock().unwrap().insert(node, fd.clone());
        }
        Ok(fd)
    }

    pub(super) fn dir_opened(&self, node: u64) {
        if let Some(n) = self.nodes.lock().unwrap().by_id.get_mut(&node) {
            n.dir_ok = true;
        }
    }

    pub(super) fn openable(&self, mut attr: Attr, free: bool) -> Attr {
        let readable = if attr.flags & crate::abi::ATTR_CALLER_OWNER != 0 {
            attr.mode & 0o400 != 0
        } else {
            attr.mode & 0o004 != 0
        };
        if free && attr.mode & S_IFMT == S_IFREG && readable {
            attr.flags |= crate::abi::ATTR_OPEN_FREE;
        }
        attr
    }

    #[cfg(test)]
    pub(super) fn free_fds_held(&self) -> usize {
        self.free_fds.lock().unwrap().len()
    }
}
