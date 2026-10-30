package images

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"fev/proto/agent"
)

type Guest interface {
	MachineList(ctx context.Context) (*agent.MachineList, error)
	MachineCreate(ctx context.Context, p agent.MachineCreateParams, progress func(agent.OperationProgress)) (*agent.Operation, error)
	OperationCancel(ctx context.Context, id uint64) error
}

type Stream interface {
	io.Writer
	CloseWrite() error
	Close() error
}

type OpenStream func(ctx context.Context, token string) (Stream, error)

type Prepared struct {
	Image   agent.MachineImage
	Kept    bool
	Offline error
	Build   *Build
}

func (p *Prepared) Close() {
	if p.Build != nil {
		p.Build.Close()
	}
}

func keptSerial(kept []agent.ImageInfo, distro, release, arch, variant string) string {
	for _, k := range kept {
		if k.Distro == distro && k.Release == release && k.Arch == arch && k.Variant == variant {
			return k.Serial
		}
	}
	return ""
}

func (s *Source) Prepare(ctx context.Context, kept []agent.ImageInfo, distro, release, arch, variant string, progress Progress) (*Prepared, error) {
	distro, release, err := Name(distro, release)
	if err != nil {
		return nil, err
	}
	if arch, err = Architecture(arch); err != nil {
		return nil, err
	}
	have := keptSerial(kept, distro, release, arch, variant)
	keptOne := func(offline error) *Prepared {
		return &Prepared{Image: agent.MachineImage{ImageRef: agent.ImageRef{Distro: distro, Release: release, Arch: arch, Variant: variant, Serial: have}}, Kept: true, Offline: offline}
	}
	entries, err := s.Index(ctx)
	if err != nil {
		if have != "" && ctx.Err() == nil {
			return keptOne(err), nil
		}
		return nil, fmt.Errorf("the image server's list of builds: %w", err)
	}
	e, ok := Newest(entries, distro, release, arch, variant)
	if !ok {
		return nil, missing(entries, distro, release, arch, variant)
	}
	if have != "" {
		if at, err := ParseSerial(have); err == nil {
			if newest, _ := ParseSerial(e.Serial); !newest.After(at) {
				return keptOne(nil), nil
			}
		}
	}
	b, err := s.Fetch(ctx, e, have, progress)
	if err != nil {
		return nil, err
	}
	return &Prepared{Image: agent.MachineImage{ImageRef: b.ImageRef, Templates: b.Templates}, Build: b}, nil
}

func missing(entries []Entry, distro, release, arch, variant string) error {
	if variant == agent.VariantCloud && len(Releases(entries, distro, arch, agent.VariantDefault)) > 0 {
		if _, ok := Newest(entries, distro, release, arch, agent.VariantDefault); ok {
			return fmt.Errorf("the image server has no %s %s image with cloud-init for %s, so a machine of it cannot take user data", distro, release, arch)
		}
	}
	if have := Releases(entries, distro, arch, variant); len(have) > 0 {
		return fmt.Errorf("the image server has no %s %s for %s; it has %s", distro, release, arch, strings.Join(have, ", "))
	}
	if have := variantsOf(entries, distro, release, arch); len(have) > 0 {
		return fmt.Errorf("the image server has no %s build of %s %s for %s; it has %s", variant, distro, release, arch, strings.Join(have, ", "))
	}
	return fmt.Errorf("the image server has no %s image for %s", distro, arch)
}

func variantsOf(entries []Entry, distro, release, arch string) []string {
	var out []string
	seen := map[string]bool{}
	for _, e := range entries {
		if e.Distro == distro && e.Release == release && e.Arch == arch && e.Variant != agent.VariantCloud && !seen[e.Variant] {
			seen[e.Variant] = true
			out = append(out, e.Variant)
		}
	}
	return out
}

var ErrNotFetched = errors.New("the guest asked for an image fev did not fetch, since the guest said it kept it")

type Sent struct {
	TarBytes int64
	Parallel bool
	Took     time.Duration
}

func (p *Prepared) Send(ctx context.Context, op *agent.Operation, open OpenStream) (Sent, error) {
	defer p.Close()
	if op.Token == "" {
		return Sent{}, nil
	}
	if p.Build == nil {
		return Sent{}, ErrNotFetched
	}
	start := time.Now()
	conn, err := open(ctx, op.Token)
	if err != nil {
		return Sent{}, fmt.Errorf("the image's stream: %w", err)
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	n, parallel, err := p.Build.WriteTar(ctx, conn)
	if err == nil {
		err = conn.CloseWrite()
	}
	if err != nil {
		return Sent{TarBytes: n, Parallel: parallel}, fmt.Errorf("the image's stream, after %d bytes: %w", n, err)
	}
	return Sent{TarBytes: n, Parallel: parallel, Took: time.Since(start)}, nil
}

func (s *Source) Create(ctx context.Context, g Guest, open OpenStream, distro, release, arch string, p agent.MachineCreateParams, fetched Progress, progress func(agent.OperationProgress)) (*agent.Operation, *Prepared, Sent, error) {
	img, err := Resolve(distro, release, arch, p.UserData != "")
	if err != nil {
		return nil, nil, Sent{}, err
	}
	list, err := g.MachineList(ctx)
	if err != nil {
		return nil, nil, Sent{}, err
	}
	prep, err := s.Prepare(ctx, list.Images, img.Distro, img.Release, img.Arch, img.Variant, fetched)
	if err != nil {
		return nil, nil, Sent{}, err
	}
	p.Image = prep.Image
	op, err := g.MachineCreate(ctx, p, progress)
	if err != nil {
		prep.Close()
		return nil, prep, Sent{}, err
	}
	sent, err := prep.Send(ctx, op, open)
	if err != nil {
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		g.OperationCancel(cctx, op.ID)
		cancel()
		return op, prep, sent, err
	}
	return op, prep, sent, nil
}
