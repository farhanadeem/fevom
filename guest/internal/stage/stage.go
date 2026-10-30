package stage

import (
	"errors"
	"fmt"
	"sync"
)

type ErrSkipped struct{ Reason string }

func (e ErrSkipped) Error() string { return "skipped: " + e.Reason }

func Skip(format string, args ...any) error {
	return ErrSkipped{Reason: fmt.Sprintf(format, args...)}
}

type Stage struct {
	Name     string
	After    []string
	Required bool
	Run      func() error
}

type Result struct {
	Name    string
	StartMS float64
	EndMS   float64
	Err     error
	Skipped string
	NotRun  bool
}

func Run(stages []Stage, now func() float64) ([]Result, error) {
	index := make(map[string]int, len(stages))
	for i, s := range stages {
		if _, dup := index[s.Name]; dup {
			return nil, fmt.Errorf("stage %q defined twice", s.Name)
		}
		index[s.Name] = i
	}
	for _, s := range stages {
		for _, dep := range s.After {
			if _, ok := index[dep]; !ok {
				return nil, fmt.Errorf("stage %q depends on unknown stage %q", s.Name, dep)
			}
		}
	}
	if cycle := findCycle(stages, index); cycle != "" {
		return nil, fmt.Errorf("stage dependency cycle through %q", cycle)
	}

	results := make([]Result, len(stages))
	done := make([]chan struct{}, len(stages))
	for i := range done {
		done[i] = make(chan struct{})
	}
	var wg sync.WaitGroup
	for i, s := range stages {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer close(done[i])
			r := &results[i]
			r.Name = s.Name
			blocked := false
			for _, dep := range s.After {
				d := index[dep]
				<-done[d]
				if stages[d].Required && (results[d].Err != nil || results[d].NotRun) {
					blocked = true
				}
			}
			r.StartMS = now()
			if blocked {
				r.NotRun = true
				r.EndMS = r.StartMS
				return
			}
			err := runProtected(s)
			r.EndMS = now()
			var skipped ErrSkipped
			if errors.As(err, &skipped) {
				r.Skipped = skipped.Reason
			} else {
				r.Err = err
			}
		}()
	}
	wg.Wait()
	return results, nil
}

func runProtected(s Stage) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
		}
	}()
	return s.Run()
}

func findCycle(stages []Stage, index map[string]int) string {
	const (
		unseen = iota
		visiting
		finished
	)
	state := make([]int, len(stages))
	var visit func(i int) string
	visit = func(i int) string {
		switch state[i] {
		case visiting:
			return stages[i].Name
		case finished:
			return ""
		}
		state[i] = visiting
		for _, dep := range stages[i].After {
			if name := visit(index[dep]); name != "" {
				return name
			}
		}
		state[i] = finished
		return ""
	}
	for i := range stages {
		if name := visit(i); name != "" {
			return name
		}
	}
	return ""
}
