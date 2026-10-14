package llm

import (
	"reflect"
	"testing"
)

// TestRequestSnapshotCloneSharesNoMemory fills every exported field of a
// snapshot, so a field added later without a matching clone fails here.
func TestRequestSnapshotCloneSharesNoMemory(t *testing.T) {
	var s RequestSnapshot
	fillValue(reflect.ValueOf(&s).Elem(), 0)
	c := s.clone()
	if !reflect.DeepEqual(c, s) {
		t.Fatal("clone differs from the original")
	}
	assertDisjoint(t, "RequestSnapshot", reflect.ValueOf(s), reflect.ValueOf(c))
}

func TestRequestSnapshotCloneKeepsNil(t *testing.T) {
	s := RequestSnapshot{Input: Input{Messages: []Message{{Role: RoleUser}}}}
	if c := s.clone(); !reflect.DeepEqual(c, s) {
		t.Fatalf("clone = %#v; want %#v", c, s)
	}
	if c := (RequestSnapshot{}).clone(); !reflect.DeepEqual(c, RequestSnapshot{}) {
		t.Fatalf("clone of zero = %#v", c)
	}
}

// fillValue sets every settable field reachable from v to a non-zero value,
// with one element in each slice and map.
func fillValue(v reflect.Value, depth int) {
	if depth > 10 || !v.CanSet() {
		return
	}
	switch v.Kind() {
	case reflect.String:
		v.SetString("x")
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v.SetInt(1)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		v.SetUint(1)
	case reflect.Float32, reflect.Float64:
		v.SetFloat(1.5)
	case reflect.Pointer:
		p := reflect.New(v.Type().Elem())
		fillValue(p.Elem(), depth+1)
		v.Set(p)
	case reflect.Slice:
		s := reflect.MakeSlice(v.Type(), 1, 1)
		fillValue(s.Index(0), depth+1)
		v.Set(s)
	case reflect.Map:
		m := reflect.MakeMap(v.Type())
		key := reflect.New(v.Type().Key()).Elem()
		val := reflect.New(v.Type().Elem()).Elem()
		fillValue(key, depth+1)
		fillValue(val, depth+1)
		m.SetMapIndex(key, val)
		v.Set(m)
	case reflect.Struct:
		for i := range v.NumField() {
			fillValue(v.Field(i), depth+1)
		}
	case reflect.Array:
		for i := range v.Len() {
			fillValue(v.Index(i), depth+1)
		}
	}
}

// assertDisjoint fails when a and b share a pointer, slice array, or map.
func assertDisjoint(t *testing.T, path string, a, b reflect.Value) {
	t.Helper()
	switch a.Kind() {
	case reflect.Pointer:
		if a.IsNil() || b.IsNil() {
			return
		}
		if a.Pointer() == b.Pointer() {
			t.Errorf("%s: shared pointer", path)
			return
		}
		assertDisjoint(t, path, a.Elem(), b.Elem())
	case reflect.Slice:
		if a.Len() == 0 || b.Len() == 0 {
			return
		}
		if a.Pointer() == b.Pointer() {
			t.Errorf("%s: shared slice", path)
			return
		}
		for i := range min(a.Len(), b.Len()) {
			assertDisjoint(t, path+"[]", a.Index(i), b.Index(i))
		}
	case reflect.Map:
		if a.Len() == 0 || b.Len() == 0 {
			return
		}
		if a.Pointer() == b.Pointer() {
			t.Errorf("%s: shared map", path)
			return
		}
		for _, k := range a.MapKeys() {
			if bv := b.MapIndex(k); bv.IsValid() {
				assertDisjoint(t, path+"[k]", a.MapIndex(k), bv)
			}
		}
	case reflect.Interface:
		if !a.IsNil() && !b.IsNil() {
			assertDisjoint(t, path, a.Elem(), b.Elem())
		}
	case reflect.Struct:
		for i := range a.NumField() {
			assertDisjoint(t, path+"."+a.Type().Field(i).Name, a.Field(i), b.Field(i))
		}
	case reflect.Array:
		for i := range a.Len() {
			assertDisjoint(t, path+"[]", a.Index(i), b.Index(i))
		}
	}
}
