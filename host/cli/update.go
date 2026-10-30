package cli

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"fev/host/bundle"
	"fev/host/update"
	"fev/proto/version"
)

func (a *app) updateCmd() *cobra.Command {
	var check bool
	cmd := &cobra.Command{
		Use:   "update",
		Short: "Check for a newer fev and install it",
		Long: `Ask Fevom.app to check for a newer release: its window says what it found,
shows the release notes, and installs it when you say so. Installing quits
and reopens Fevom, which restarts fev. Fevom also checks by itself once a
day (Settings, App).

--check asks the update feed from here, checks its signature and prints
what it offers, installing nothing: exit 0 when this is the newest, 1 with
"available" when a newer one is.

Only a release's app updates itself: a fev built from a checkout has no
update feed and says so.`,
		Args: args(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			b, err := ownBundle()
			if err != nil {
				return err
			}
			if !check {
				if err := openApp(filepath.Dir(b.contents), "fev://update"); err != nil {
					return failf(ExitFailed, "update: could not open Fevom: %v", err)
				}
				if !a.quiet {
					fmt.Fprintln(a.Stdout, "Fevom is checking for updates: its window says what it finds.")
				}
				return nil
			}
			it, err := fetchFeed(cmd.Context(), b)
			if err != nil {
				return failf(ExitFailed, "update: %v", err)
			}
			newer := update.Newer(it.Version, b.version)
			out := struct {
				Current   string `json:"current"`
				Latest    string `json:"latest"`
				Available bool   `json:"available"`
				URL       string `json:"url"`
			}{b.version, it.Version, newer, it.URL}
			if err := a.emit(out, func(w io.Writer) {
				if a.quiet {
					fmt.Fprintln(w, it.Version)
					return
				}
				if newer {
					fmt.Fprintf(w, "Fevom %s is available (this is %s): fev update installs it.\n", it.Version, b.version)
				} else {
					fmt.Fprintf(w, "Fevom %s is the newest release.\n", b.version)
				}
			}); err != nil {
				return err
			}
			if newer {
				return &exit{code: ExitFailed, silent: true, err: fmt.Errorf("%s is available", it.Version)}
			}
			return nil
		},
	}
	cmd.Flags().BoolVarP(&check, "check", "c", false, "say what the feed offers, install nothing")
	return cmd
}

type ownApp struct {
	contents string
	version  string
	feed     string
	key      string
}

var selfPath = func() (string, error) {
	self, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(self)
}

var openApp = func(path, url string) error {
	return exec.Command("/usr/bin/open", "-a", path, url).Run()
}

func plistValue(plist, key string) string {
	out, err := exec.Command("/usr/bin/plutil", "-extract", key, "raw", "-o", "-", plist).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func ownBundle() (ownApp, error) {
	self, err := selfPath()
	if err != nil {
		return ownApp{}, failf(ExitFailed, "update: %v", err)
	}
	c := bundle.Contents(self)
	if c == "" {
		return ownApp{}, failf(ExitFailed, "update: this fev (build %s) is not an app's: only a release's Fevom.app updates itself", version.Build)
	}
	plist := filepath.Join(c, "Info.plist")
	b := ownApp{contents: c, version: plistValue(plist, "CFBundleShortVersionString"),
		feed: plistValue(plist, "SUFeedURL"), key: plistValue(plist, "SUPublicEDKey")}
	if b.feed == "" || b.key == "" || !update.ValidVersion(b.version) {
		return ownApp{}, failf(ExitFailed, "update: this Fevom.app (build %s) is not a release's and does not update itself", version.Build)
	}
	return b, nil
}

var feedClient = &http.Client{Timeout: 30 * time.Second}

func fetchFeed(ctx context.Context, b ownApp) (update.Item, error) {
	pub, err := update.ParsePublic(b.key)
	if err != nil {
		return update.Item{}, fmt.Errorf("the bundle's SUPublicEDKey: %v", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, b.feed, nil)
	if err != nil {
		return update.Item{}, err
	}
	resp, err := feedClient.Do(req)
	if err != nil {
		return update.Item{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return update.Item{}, fmt.Errorf("%s: %s", b.feed, resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return update.Item{}, err
	}
	it, err := update.Verify(body, pub)
	if err != nil {
		return update.Item{}, fmt.Errorf("%s: %v", b.feed, err)
	}
	return it, nil
}
