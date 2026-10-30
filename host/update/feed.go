package update

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
)

func NewKey(path string) (ed25519.PrivateKey, error) {
	seed := make([]byte, ed25519.SeedSize)
	if _, err := rand.Read(seed); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	if _, err := fmt.Fprintln(f, base64.StdEncoding.EncodeToString(seed)); err != nil {
		f.Close()
		return nil, err
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	return ed25519.NewKeyFromSeed(seed), nil
}

func ReadKey(path string) (ed25519.PrivateKey, error) {
	st, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if st.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("%s: others may read it (mode %v); chmod 600 it", path, st.Mode().Perm())
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(b)))
	if err != nil {
		return nil, fmt.Errorf("%s: not base64: %v", path, err)
	}
	switch len(raw) {
	case ed25519.SeedSize:
		return ed25519.NewKeyFromSeed(raw), nil
	case ed25519.PrivateKeySize + ed25519.PublicKeySize:
		k := ed25519.PrivateKey(raw[:ed25519.PrivateKeySize])
		if !bytes.Equal(k.Public().(ed25519.PublicKey), raw[ed25519.PrivateKeySize:]) {
			return nil, fmt.Errorf("%s: its public half does not match its private one", path)
		}
		return k, nil
	}
	return nil, fmt.Errorf("%s: %d bytes, not a key", path, len(raw))
}

func Public(k ed25519.PrivateKey) string {
	return base64.StdEncoding.EncodeToString(k.Public().(ed25519.PublicKey))
}

func ParsePublic(s string) (ed25519.PublicKey, error) {
	b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil || len(b) != ed25519.PublicKeySize {
		return nil, errors.New("not a public key")
	}
	return b, nil
}

func Sign(k ed25519.PrivateKey, b []byte) string {
	return base64.StdEncoding.EncodeToString(ed25519.Sign(k, b))
}

type Item struct {
	Version   string
	Date      string
	URL       string
	Length    int
	Signature string
	MinOS     string
	Notes     string
}

var versionForm = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)

func ValidVersion(v string) bool { return versionForm.MatchString(v) }

func escape(s string) string {
	var b strings.Builder
	xml.EscapeText(&b, []byte(s))
	return b.String()
}

func Render(it Item, k ed25519.PrivateKey) ([]byte, error) {
	if !ValidVersion(it.Version) {
		return nil, fmt.Errorf("version %q: not X.Y.Z", it.Version)
	}
	if strings.Contains(it.Notes, "]]>") {
		return nil, errors.New("the release notes hold \"]]>\"")
	}
	var b bytes.Buffer
	b.WriteString(`<?xml version="1.0" encoding="utf-8"?>
<rss version="2.0" xmlns:sparkle="http://www.andymatuschak.org/xml-namespaces/sparkle">
  <channel>
    <title>Fevom</title>
    <item>
`)
	fmt.Fprintf(&b, "      <title>Fevom %s</title>\n", escape(it.Version))
	fmt.Fprintf(&b, "      <pubDate>%s</pubDate>\n", escape(it.Date))
	fmt.Fprintf(&b, "      <sparkle:version>%s</sparkle:version>\n", escape(it.Version))
	fmt.Fprintf(&b, "      <sparkle:shortVersionString>%s</sparkle:shortVersionString>\n", escape(it.Version))
	fmt.Fprintf(&b, "      <sparkle:minimumSystemVersion>%s</sparkle:minimumSystemVersion>\n", escape(it.MinOS))
	if it.Notes != "" {
		fmt.Fprintf(&b, "      <description sparkle:format=\"markdown\"><![CDATA[%s]]></description>\n", it.Notes)
	}
	fmt.Fprintf(&b, "      <enclosure url=\"%s\" length=\"%d\" type=\"application/octet-stream\" sparkle:edSignature=\"%s\"/>\n",
		escape(it.URL), it.Length, it.Signature)
	b.WriteString("    </item>\n  </channel>\n</rss>\n")
	content := b.Bytes()
	return append(content, fmt.Sprintf("<!-- sparkle-signatures:\nedSignature: %s\nlength: %d\n-->\n", Sign(k, content), len(content))...), nil
}

const sigPrefix = "<!-- sparkle-signatures:\n"

func split(feed []byte) (content []byte, sig []byte, length int, err error) {
	i := bytes.LastIndex(feed, []byte(sigPrefix))
	if i < 0 {
		return nil, nil, 0, errors.New("the feed is not signed")
	}
	rest := feed[i+len(sigPrefix):]
	j := bytes.Index(rest, []byte("-->"))
	if j < 0 {
		return nil, nil, 0, errors.New("the feed's signature block is not closed")
	}
	length = -1
	for _, line := range strings.Split(string(rest[:j]), "\n") {
		if v, ok := strings.CutPrefix(line, "edSignature:"); ok {
			if sig, err = base64.StdEncoding.DecodeString(strings.TrimSpace(v)); err != nil {
				return nil, nil, 0, fmt.Errorf("the feed's signature: %v", err)
			}
		} else if v, ok := strings.CutPrefix(line, "length:"); ok {
			if length, err = strconv.Atoi(strings.TrimSpace(v)); err != nil {
				return nil, nil, 0, fmt.Errorf("the feed's length: %v", err)
			}
		}
	}
	return feed[:i], sig, length, nil
}

type parsed struct {
	Items []struct {
		Version     string `xml:"http://www.andymatuschak.org/xml-namespaces/sparkle version"`
		MinOS       string `xml:"http://www.andymatuschak.org/xml-namespaces/sparkle minimumSystemVersion"`
		Date        string `xml:"pubDate"`
		Description string `xml:"description"`
		Enclosure   struct {
			URL       string `xml:"url,attr"`
			Length    int    `xml:"length,attr"`
			Signature string `xml:"http://www.andymatuschak.org/xml-namespaces/sparkle edSignature,attr"`
		} `xml:"enclosure"`
	} `xml:"channel>item"`
}

func Verify(feed []byte, pub ed25519.PublicKey) (Item, error) {
	content, sig, length, err := split(feed)
	if err != nil {
		return Item{}, err
	}
	if length != len(content) {
		return Item{}, fmt.Errorf("the feed's length is %d, its content %d bytes", length, len(content))
	}
	if !ed25519.Verify(pub, content, sig) {
		return Item{}, errors.New("the feed's signature does not verify")
	}
	var p parsed
	if err := xml.Unmarshal(content, &p); err != nil {
		return Item{}, err
	}
	if len(p.Items) != 1 {
		return Item{}, fmt.Errorf("%d items, not one", len(p.Items))
	}
	x := p.Items[0]
	if !ValidVersion(x.Version) {
		return Item{}, fmt.Errorf("version %q: not X.Y.Z", x.Version)
	}
	return Item{Version: x.Version, Date: x.Date, URL: x.Enclosure.URL, Length: x.Enclosure.Length,
		Signature: x.Enclosure.Signature, MinOS: x.MinOS, Notes: x.Description}, nil
}

func VerifyArchive(it Item, archive []byte, pub ed25519.PublicKey) error {
	if len(archive) != it.Length {
		return fmt.Errorf("the archive is %d bytes, the feed says %d", len(archive), it.Length)
	}
	s, err := base64.StdEncoding.DecodeString(it.Signature)
	if err != nil || !ed25519.Verify(pub, archive, s) {
		return errors.New("the archive's signature does not verify")
	}
	return nil
}

func Newer(a, b string) bool {
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < 3 && i < len(pa) && i < len(pb); i++ {
		x, _ := strconv.Atoi(pa[i])
		y, _ := strconv.Atoi(pb[i])
		if x != y {
			return x > y
		}
	}
	return false
}
