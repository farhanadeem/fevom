//go:build !(darwin && cgo)

package power

func Listen() (Source, error) { return nil, ErrUnsupported }

func Assert(reason string, display bool) (release func(), err error) { return nil, ErrUnsupported }
