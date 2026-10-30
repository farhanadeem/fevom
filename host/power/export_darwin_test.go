//go:build darwin && cgo

package power

func deliver(kind int, id int64) { fevPowerEvent(_Ctype_int(kind), _Ctype_long(id)) }
