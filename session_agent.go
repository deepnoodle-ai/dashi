package dashi

import (
	"reflect"
	"time"
)

// mergeAgent fills each zero field of a from b, recursing into Settings and
// Limits: a nil pointer, empty string, slice, or map, zero number, or false
// falls through, so a setting is turned off with its negative or No… form.
// Tools and NoTools are one selection and move together: a layer that sets
// either replaces both.
//
// It walks the fields by reflection so that a field added to Agent, Limits,
// or llm.Settings is merged without a change here.
func mergeAgent(a, b Agent) Agent {
	if len(a.Tools) > 0 || a.NoTools {
		b.Tools, b.NoTools = a.Tools, a.NoTools
	}
	fillZero(reflect.ValueOf(&a).Elem(), reflect.ValueOf(b))
	return a
}

var timeType = reflect.TypeFor[time.Time]()

func fillZero(dst, src reflect.Value) {
	for i := range dst.NumField() {
		f := dst.Field(i)
		if !f.CanSet() {
			continue
		}
		switch {
		case f.Kind() == reflect.Struct && f.Type() != timeType:
			fillZero(f, src.Field(i))
		case f.IsZero(),
			(f.Kind() == reflect.Slice || f.Kind() == reflect.Map) && f.Len() == 0:
			f.Set(src.Field(i))
		}
	}
}
