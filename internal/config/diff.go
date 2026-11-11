package config

import (
	"fmt"
	"reflect"
	"strings"
)

// ChangedKeys names the keys of the file whose values differ between two
// configurations, as the file spells them: "http.port", "users[2].paths".
// It names keys only, never values, so that a record of a change cannot
// carry a password. A list whose length changed is named as a whole, since
// its entries no longer pair up.
func ChangedKeys(before, after Config) []string {
	var changed []string
	diffValue(reflect.ValueOf(before), reflect.ValueOf(after), "", &changed)
	return changed
}

var configPackage = reflect.TypeOf(Config{}).PkgPath()

func diffValue(a, b reflect.Value, path string, changed *[]string) {
	switch a.Kind() {
	case reflect.Struct:
		// a struct of another package, such as a time.Time, is a value of
		// its own, compared whole rather than by fields it keeps to itself
		if a.Type().PkgPath() != configPackage {
			if !reflect.DeepEqual(a.Interface(), b.Interface()) {
				*changed = append(*changed, path)
			}
			return
		}
		for i := range a.NumField() {
			field := a.Type().Field(i)
			if !field.IsExported() {
				continue
			}
			name, _, _ := strings.Cut(field.Tag.Get("toml"), ",")
			switch name {
			case "-":
				continue
			case "":
				name = strings.ToLower(field.Name[:1]) + field.Name[1:]
			}
			if path != "" {
				name = path + "." + name
			}
			diffValue(a.Field(i), b.Field(i), name, changed)
		}
	case reflect.Slice, reflect.Array:
		if a.Len() != b.Len() {
			*changed = append(*changed, path)
			return
		}
		for i := range a.Len() {
			diffValue(a.Index(i), b.Index(i), fmt.Sprintf("%s[%d]", path, i), changed)
		}
	case reflect.Pointer:
		if a.IsNil() || b.IsNil() {
			if a.IsNil() != b.IsNil() {
				*changed = append(*changed, path)
			}
			return
		}
		diffValue(a.Elem(), b.Elem(), path, changed)
	default:
		if !reflect.DeepEqual(a.Interface(), b.Interface()) {
			*changed = append(*changed, path)
		}
	}
}
