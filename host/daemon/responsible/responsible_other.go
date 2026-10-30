//go:build !darwin

package responsible

func Of(pid int) Who { return Who{} }

func Self() Who { return Who{} }
