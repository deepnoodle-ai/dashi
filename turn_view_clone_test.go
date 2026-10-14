package dashi

import (
	"reflect"
	"slices"
	"testing"
	"time"
	"unsafe"
)

// fillValue sets every field reachable from v to a non-zero value: one
// element per slice and map, a target for every pointer. Interfaces stay
// nil. depth bounds recursive types.
func fillValue(v reflect.Value, depth int) {
	if depth > 10 {
		return
	}
	if v.Type() == reflect.TypeFor[time.Time]() {
		v.Set(reflect.ValueOf(time.Unix(1, 0).UTC()))
		return
	}
	switch v.Kind() {
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v.SetInt(1)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		v.SetUint(1)
	case reflect.Float32, reflect.Float64:
		v.SetFloat(1.5)
	case reflect.String:
		v.SetString("x")
	case reflect.Pointer:
		p := reflect.New(v.Type().Elem())
		fillValue(p.Elem(), depth+1)
		v.Set(p)
	case reflect.Slice:
		s := reflect.MakeSlice(v.Type(), 1, 2)
		fillValue(s.Index(0), depth+1)
		v.Set(s)
	case reflect.Array:
		for i := range v.Len() {
			fillValue(v.Index(i), depth+1)
		}
	case reflect.Map:
		m := reflect.MakeMap(v.Type())
		key := reflect.New(v.Type().Key()).Elem()
		fillValue(key, depth+1)
		value := reflect.New(v.Type().Elem()).Elem()
		fillValue(value, depth+1)
		m.SetMapIndex(key, value)
		v.Set(m)
	case reflect.Struct:
		for i := range v.NumField() {
			fillValue(settable(v.Field(i)), depth+1)
		}
	}
}

// settable returns f, made settable even when it is unexported. v must be
// addressable.
func settable(f reflect.Value) reflect.Value {
	return reflect.NewAt(f.Type(), unsafe.Pointer(f.UnsafeAddr())).Elem()
}

// sharedMemory returns the paths at which a and b share a pointer, slice
// array, or map, so changing one would change the other.
func sharedMemory(a, b reflect.Value, path string) []string {
	var out []string
	switch a.Kind() {
	case reflect.Pointer:
		if a.IsNil() || b.IsNil() {
			return nil
		}
		if a.UnsafePointer() == b.UnsafePointer() {
			return []string{path}
		}
		return sharedMemory(a.Elem(), b.Elem(), path)
	case reflect.Interface:
		if a.IsNil() || b.IsNil() {
			return nil
		}
		return sharedMemory(a.Elem(), b.Elem(), path)
	case reflect.Slice:
		if a.Len() == 0 || b.Len() == 0 {
			return nil
		}
		if a.UnsafePointer() == b.UnsafePointer() {
			return []string{path}
		}
		for i := range min(a.Len(), b.Len()) {
			out = append(out, sharedMemory(a.Index(i), b.Index(i), path+"[]")...)
		}
	case reflect.Map:
		if a.Len() == 0 || b.Len() == 0 {
			return nil
		}
		if a.UnsafePointer() == b.UnsafePointer() {
			return []string{path}
		}
		for _, k := range a.MapKeys() {
			if bv := b.MapIndex(k); bv.IsValid() {
				out = append(out, sharedMemory(a.MapIndex(k), bv, path+"[k]")...)
			}
		}
	case reflect.Struct:
		for i := range a.NumField() {
			out = append(out, sharedMemory(a.Field(i), b.Field(i), path+"."+a.Type().Field(i).Name)...)
		}
	}
	return out
}

// TestTurnViewCloneIsDeep fails when a field reachable from TurnView is
// added without TurnView.clone copying it.
func TestTurnViewCloneIsDeep(t *testing.T) {
	var v TurnView
	fillValue(reflect.ValueOf(&v).Elem(), 0)
	got := v.clone()
	if !reflect.DeepEqual(got, v) {
		t.Fatal("clone lost a field")
	}
	if shared := sharedMemory(reflect.ValueOf(v), reflect.ValueOf(got), "TurnView"); len(shared) > 0 {
		t.Fatalf("clone shares memory at %v", shared)
	}
	if got := snapshotView(v); !reflect.DeepEqual(got, v) {
		t.Fatal("snapshotView lost a field")
	}
}

// replayStateShared lists the replayState fields clone shares by design.
// Replay replaces each of them whole, never changes it in place.
var replayStateShared = []string{
	"contextCache",   // a cache over the turn's fixed prefixes
	"source",         // replaced with the turn being rebuilt
	"latestResult",   // points into a step
	"activeHooks",    // points into a step
	"compiledSchema", // compiled once, read only
	"Agent",          // set once at start
	"OutputSchema",   // set once at start
	"Parent",         // set once at start
	"Compaction",     // set once at start
	"Budget",         // replaced on each raise
	"Stop",           // replaced on each stop
	"Output",         // set once
}

// TestReplayStateCloneCoversFields fails when replayState or TurnView gains
// a map, slice, or pointer field that clone neither copies nor lists in
// replayStateShared.
func TestReplayStateCloneCoversFields(t *testing.T) {
	var s replayState
	fillValue(reflect.ValueOf(&s).Elem(), 0)
	got := s.clone()
	check := func(a, b reflect.Value) {
		for i := range a.NumField() {
			f := a.Type().Field(i)
			if f.Anonymous || slices.Contains(replayStateShared, f.Name) {
				continue
			}
			x, y := a.Field(i), b.Field(i)
			switch x.Kind() {
			case reflect.Map:
				if x.UnsafePointer() == y.UnsafePointer() {
					t.Errorf("clone shares map %s", f.Name)
				}
			case reflect.Slice:
				// A clipped slice is safe: an append copies it.
				if y.Cap() != y.Len() && x.UnsafePointer() == y.UnsafePointer() {
					t.Errorf("clone shares slice %s", f.Name)
				}
			case reflect.Pointer, reflect.Interface, reflect.Struct:
				if len(sharedMemory(x, y, f.Name)) > 0 {
					t.Errorf("clone shares %s; copy it or list it in replayStateShared", f.Name)
				}
			}
		}
	}
	check(reflect.ValueOf(s), reflect.ValueOf(*got))
	check(reflect.ValueOf(s.TurnView), reflect.ValueOf(got.TurnView))
	if !reflect.DeepEqual(*got, s) {
		t.Fatal("clone lost a field")
	}
}
