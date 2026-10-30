#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct Errno(pub i32);

impl Errno {
    pub const EPERM: Self = Self(1);
    pub const ENOENT: Self = Self(2);
    pub const EIO: Self = Self(5);
    pub const EBADF: Self = Self(9);
    pub const ENOMEM: Self = Self(12);
    pub const EACCES: Self = Self(13);
    pub const EEXIST: Self = Self(17);
    pub const ENOTDIR: Self = Self(20);
    pub const EISDIR: Self = Self(21);
    pub const EINVAL: Self = Self(22);
    pub const ENFILE: Self = Self(23);
    pub const ENOTTY: Self = Self(25);
    pub const EROFS: Self = Self(30);
    pub const ERANGE: Self = Self(34);
    pub const ENAMETOOLONG: Self = Self(36);
    pub const ENOSYS: Self = Self(38);
    pub const ELOOP: Self = Self(40);
    pub const ENODATA: Self = Self(61);
    pub const EPROTO: Self = Self(71);
    pub const EILSEQ: Self = Self(84);
    pub const EOPNOTSUPP: Self = Self(95);
    pub const ESTALE: Self = Self(116);

    pub fn from_darwin(e: i32) -> Self {
        Self(match e {
            11 => 35,
            24 => 23,
            1..=34 => e,
            35 => 11,
            36 => 115,
            37 => 114,
            38 => 88,
            39 => 89,
            40 => 90,
            41 => 91,
            42 => 92,
            43 => 93,
            44 => 94,
            45 => 95,
            46 => 96,
            47 => 97,
            48 => 98,
            49 => 99,
            50 => 100,
            51 => 101,
            52 => 102,
            53 => 103,
            54 => 104,
            55 => 105,
            56 => 106,
            57 => 107,
            58 => 108,
            59 => 109,
            60 => 110,
            61 => 111,
            62 => 40,
            63 => 36,
            64 => 112,
            65 => 113,
            66 => 39,
            67 => 11,
            68 => 87,
            69 => 122,
            70 => 116,
            71 => 66,
            77 => 37,
            78 => 95,
            79 => 22,
            80 | 81 => 13,
            84 => 75,
            85..=88 => 8,
            89 => 125,
            90 => 43,
            91 => 42,
            92 => 84,
            93 => 61,
            94 => 74,
            95 => 72,
            96 => 61,
            97 => 67,
            98 => 63,
            99 => 60,
            100 => 71,
            101 => 62,
            102 => 95,
            104 => 131,
            105 => 130,
            106 => 105,
            _ => 5,
        })
    }

    pub fn last() -> Self {
        Self::from_darwin(std::io::Error::last_os_error().raw_os_error().unwrap_or(5))
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn darwin_numbers_become_linux_numbers() {
        for (darwin, linux) in [
            (libc::ENOENT, 2),
            (libc::EPERM, 1),
            (libc::EAGAIN, 11),
            (libc::EDEADLK, 35),
            (libc::ENOTEMPTY, 39),
            (libc::ELOOP, 40),
            (libc::ENAMETOOLONG, 36),
            (libc::ENOATTR, 61),
            (libc::EOPNOTSUPP, 95),
            (libc::ENOTSUP, 95),
            (libc::ESTALE, 116),
            (libc::EDQUOT, 122),
            (libc::EILSEQ, 84),
            (libc::EOVERFLOW, 75),
        ] {
            assert_eq!(Errno::from_darwin(darwin), Errno(linux), "darwin {darwin}");
        }
    }

    #[test]
    fn no_mac_error_latches_or_blames_the_guest() {
        assert_eq!(Errno::from_darwin(libc::ENOSYS), Errno::EOPNOTSUPP);
        assert_eq!(Errno::from_darwin(libc::EMFILE), Errno::ENFILE);
        for e in -5..300 {
            let l = Errno::from_darwin(e).0;
            assert!(l > 0 && l != 38 && l != 24, "darwin {e} became {l}");
        }
    }
}
