package serial

import (
	"errors"
	"reflect"

	goserial "go.bug.st/serial"
)

// go.bug.st/serial v1.8.0 has no flow-control setting (it even clears CRTSCTS when opening), so flow control is
// applied to the OS handle underneath it: the unexported "handle" field of the library's port (an int file
// descriptor on unix, a windows.Handle on Windows). The library version is pinned by go.mod and a unit test checks
// that the field is still found, so an upgrade that moves it fails loudly instead of silently ignoring flow control.

var errNoHandle = errors.New("serial: cannot reach the OS handle of the port to configure flow control")

// portHandle returns the OS handle of a port opened by go.bug.st/serial.
func portHandle(p goserial.Port) (uintptr, error) {
	v := reflect.ValueOf(p)
	if !v.IsValid() || v.Kind() != reflect.Pointer || v.IsNil() {
		return 0, errNoHandle
	}
	e := v.Elem()
	if e.Kind() != reflect.Struct {
		return 0, errNoHandle
	}
	f := e.FieldByName("handle")
	if !f.IsValid() {
		return 0, errNoHandle
	}
	switch f.Kind() {
	case reflect.Int, reflect.Int32, reflect.Int64:
		if f.Int() < 0 {
			return 0, errNoHandle
		}
		return uintptr(f.Int()), nil
	case reflect.Uintptr, reflect.Uint, reflect.Uint32, reflect.Uint64:
		if f.Uint() == 0 {
			return 0, errNoHandle
		}
		return uintptr(f.Uint()), nil
	}
	return 0, errNoHandle
}
