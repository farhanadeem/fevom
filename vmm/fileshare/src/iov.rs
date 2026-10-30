#[derive(Clone, Copy)]
pub struct IoVecs<'a> {
    v: &'a [libc::iovec],
}

impl<'a> IoVecs<'a> {
    pub unsafe fn new(v: &'a [libc::iovec]) -> Self {
        Self { v }
    }

    pub fn empty() -> Self {
        Self { v: &[] }
    }

    pub fn as_slice(&self) -> &'a [libc::iovec] {
        self.v
    }

    pub fn len(&self) -> usize {
        self.v
            .iter()
            .fold(0usize, |n, b| n.saturating_add(b.iov_len))
    }

    pub fn is_empty(&self) -> bool {
        self.len() == 0
    }

    pub fn sub(&self, mut skip: usize, mut len: usize) -> Vec<libc::iovec> {
        let mut out = Vec::new();
        for b in self.v {
            if len == 0 {
                break;
            }
            if skip >= b.iov_len {
                skip -= b.iov_len;
                continue;
            }
            let take = (b.iov_len - skip).min(len);
            out.push(libc::iovec {
                iov_base: unsafe { b.iov_base.cast::<u8>().add(skip) }.cast(),
                iov_len: take,
            });
            len -= take;
            skip = 0;
        }
        out
    }

    pub fn write_at(&self, offset: usize, data: &[u8]) -> bool {
        let mut done = 0;
        for piece in self.sub(offset, data.len()) {
            unsafe {
                std::ptr::copy_nonoverlapping(
                    data.as_ptr().add(done),
                    piece.iov_base.cast::<u8>(),
                    piece.iov_len,
                )
            };
            done += piece.iov_len;
        }
        done == data.len()
    }

    pub fn read_at(&self, offset: usize, out: &mut [u8]) -> bool {
        let mut done = 0;
        for piece in self.sub(offset, out.len()) {
            unsafe {
                std::ptr::copy_nonoverlapping(
                    piece.iov_base.cast::<u8>(),
                    out.as_mut_ptr().add(done),
                    piece.iov_len,
                )
            };
            done += piece.iov_len;
        }
        done == out.len()
    }
}

pub struct OwnedIov {
    _buf: Box<[u8]>,
    iov: Vec<libc::iovec>,
    ptr: *mut u8,
    len: usize,
}

impl OwnedIov {
    pub fn new(len: usize, piece: usize) -> Self {
        let mut buf = vec![0u8; len].into_boxed_slice();
        let ptr = buf.as_mut_ptr();
        let piece = piece.max(1);
        let iov = (0..len)
            .step_by(piece)
            .map(|at| libc::iovec {
                iov_base: unsafe { ptr.add(at) }.cast(),
                iov_len: piece.min(len - at),
            })
            .collect();
        Self {
            _buf: buf,
            iov,
            ptr,
            len,
        }
    }

    pub fn iov(&self) -> IoVecs<'_> {
        unsafe { IoVecs::new(&self.iov) }
    }

    pub fn bytes(&self, n: usize) -> Vec<u8> {
        let n = n.min(self.len);
        let mut out = vec![0u8; n];
        unsafe { std::ptr::copy_nonoverlapping(self.ptr, out.as_mut_ptr(), n) };
        out
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn copies_across_piece_boundaries_and_refuses_overruns() {
        let o = OwnedIov::new(10, 3);
        let v = o.iov();
        assert_eq!(v.len(), 10);
        assert!(v.write_at(2, b"abcdef"));
        assert_eq!(o.bytes(10), b"\0\0abcdef\0\0");
        let mut back = [0u8; 4];
        assert!(v.read_at(4, &mut back));
        assert_eq!(&back, b"cdef");
        assert!(!v.write_at(8, b"xyz"));
        assert!(!v.read_at(11, &mut back[..1]));
        assert!(v.write_at(10, b""));
        let sub = v.sub(4, 100);
        assert_eq!(sub.iter().map(|b| b.iov_len).sum::<usize>(), 6);
        assert!(v.sub(usize::MAX, 1).is_empty());
    }
}
