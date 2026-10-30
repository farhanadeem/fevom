package cli

import (
	"crypto/rand"
	"crypto/sha512"
	"strings"
)

func sha512Crypt(password, salt string) string {
	if len(salt) > 16 {
		salt = salt[:16]
	}
	pw, s := []byte(password), []byte(salt)

	b := sha512.New()
	b.Write(pw)
	b.Write(s)
	b.Write(pw)
	sumB := b.Sum(nil)

	a := sha512.New()
	a.Write(pw)
	a.Write(s)
	for n := len(pw); n > 0; n -= 64 {
		a.Write(sumB[:min(n, 64)])
	}
	for n := len(pw); n > 0; n >>= 1 {
		if n&1 != 0 {
			a.Write(sumB)
		} else {
			a.Write(pw)
		}
	}
	sumA := a.Sum(nil)

	dp := sha512.New()
	for range len(pw) {
		dp.Write(pw)
	}
	sumDP := dp.Sum(nil)
	p := make([]byte, 0, len(pw))
	for n := len(pw); n > 0; n -= 64 {
		p = append(p, sumDP[:min(n, 64)]...)
	}

	ds := sha512.New()
	for range 16 + int(sumA[0]) {
		ds.Write(s)
	}
	sumDS := ds.Sum(nil)
	sb := make([]byte, 0, len(s))
	for n := len(s); n > 0; n -= 64 {
		sb = append(sb, sumDS[:min(n, 64)]...)
	}

	c := sumA
	for i := range 5000 {
		h := sha512.New()
		if i&1 != 0 {
			h.Write(p)
		} else {
			h.Write(c)
		}
		if i%3 != 0 {
			h.Write(sb)
		}
		if i%7 != 0 {
			h.Write(p)
		}
		if i&1 != 0 {
			h.Write(c)
		} else {
			h.Write(p)
		}
		c = h.Sum(nil)
	}

	const itoa64 = "./0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
	var out strings.Builder
	out.WriteString("$6$" + salt + "$")
	b64 := func(b2, b1, b0 byte, n int) {
		w := uint(b2)<<16 | uint(b1)<<8 | uint(b0)
		for ; n > 0; n-- {
			out.WriteByte(itoa64[w&0x3f])
			w >>= 6
		}
	}
	order := [][3]int{
		{0, 21, 42}, {22, 43, 1}, {44, 2, 23}, {3, 24, 45}, {25, 46, 4}, {47, 5, 26}, {6, 27, 48},
		{28, 49, 7}, {50, 8, 29}, {9, 30, 51}, {31, 52, 10}, {53, 11, 32}, {12, 33, 54}, {34, 55, 13},
		{56, 14, 35}, {15, 36, 57}, {37, 58, 16}, {59, 17, 38}, {18, 39, 60}, {40, 61, 19}, {62, 20, 41},
	}
	for _, o := range order {
		b64(c[o[0]], c[o[1]], c[o[2]], 4)
	}
	b64(0, 0, c[63], 2)
	return out.String()
}

func newSalt() (string, error) {
	const itoa64 = "./0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	for i := range b {
		b[i] = itoa64[b[i]&0x3f]
	}
	return string(b[:]), nil
}
