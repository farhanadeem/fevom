package cli

import "testing"

func TestAPasswordIsSentAsASHA512CryptString(t *testing.T) {
	for _, c := range []struct{ password, salt, want string }{
		{"Hello world!", "saltstring", "$6$saltstring$svn8UoSVapNtMuq1ukKS4tPQd8iKwSMHWjl/O817G3uBnIFNjnQJuesI68u4OTLiBFdcbYEdFCoEOfaS35inz1"},
		{"Hello world!", "saltstringsaltstring", "$6$saltstringsaltst$e.3mR68CqZEpesEX1HlFZT6sEanSOjM/b5UoDyDo00a8syek2cJldMjrbtKP86.FJvzluVR7nc3DNzelAwTxj."},
		{"pässwörd with a long phrase that is longer than sixty four bytes, to cross a block", "abc", "$6$abc$BKJbaIoXdaA2nCRkcCaBRpDw4tqmpca.R.X05ajn8Jb6cp05kBuqRT7HcwE8aGkUrILWSJnlTKKMspdWNOnUP1"},
	} {
		if got := sha512Crypt(c.password, c.salt); got != c.want {
			t.Errorf("crypt(%q, %q) = %s\nwant %s", c.password, c.salt, got, c.want)
		}
	}
	salt, err := newSalt()
	if err != nil || len(salt) != 16 {
		t.Fatalf("salt %q (%v)", salt, err)
	}
	for _, r := range salt {
		if !(r == '.' || r == '/' || r >= '0' && r <= '9' || r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z') {
			t.Errorf("salt %q has %q", salt, r)
		}
	}
}
