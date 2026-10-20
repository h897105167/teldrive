package cloudflare

import (
	"bytes"
	"reflect"
	"strings"
	"time"

	"github.com/knadh/koanf/v2"
	"github.com/pelletier/go-toml"

	"github.com/tgdrive/teldrive/v2/internal/config"
)

// marshalConfigTOML serialises the Config struct back to TOML using the
// koanf tag as the source of truth for kebab-case section/key names.
func marshalConfigTOML(cfg *config.Config) ([]byte, error) {
	k := koanf.New(".")

	// 1. Seed with defaults so missing fields still appear in output.
	defaults := config.Default()
	mergeMap(k, "", defaultsMap(&defaults))

	// 2. Override with the supplied values.
	mergeMap(k, "", structToMap(reflect.ValueOf(cfg).Elem()))

	// 3. Flatten → nest for pelletier → serialise.
	nested := unflatten(k.Raw())
	return toml.Marshal(nested)
}

// --- helpers ---

func defaultsMap(cfg *config.Config) map[string]any {
	return structToMap(reflect.ValueOf(cfg).Elem())
}

func mergeMap(k *koanf.Koanf, path string, m map[string]any) {
	for key, val := range m {
		fullPath := key
		if path != "" {
			fullPath = path + "." + key
		}
		if sub, ok := val.(map[string]any); ok {
			mergeMap(k, fullPath, sub)
			continue
		}
		k.Set(fullPath, val)
	}
}

func structToMap(v reflect.Value) map[string]any {
	result := make(map[string]any)
	t := v.Type()
	for i := range v.NumField() {
		field := v.Field(i)
		if !field.CanInterface() {
			continue
		}
		fieldName := t.Field(i).Name
		if tag := t.Field(i).Tag.Get("koanf"); tag != "" {
			fieldName = tag
		} else {
			fieldName = toKebab(fieldName)
		}
		if isNestedStruct(field.Type()) {
			result[fieldName] = structToMap(field)
			continue
		}
		switch field.Type() {
		case reflect.TypeFor[time.Duration]():
			result[fieldName] = field.Interface().(time.Duration).String()
		default:
			result[fieldName] = field.Interface()
		}
	}
	return result
}

func isNestedStruct(t reflect.Type) bool {
	if t.Kind() != reflect.Struct {
		return false
	}
	if t == reflect.TypeFor[time.Duration]() {
		return false
	}
	return true
}

func toKebab(name string) string {
	var b strings.Builder
	prevLower := false
	for i := 0; i < len(name); i++ {
		c := name[i]
		if c >= 'A' && c <= 'Z' {
			if i > 0 && prevLower {
				b.WriteByte('-')
			}
			b.WriteByte(c + 32)
			prevLower = false
		} else {
			b.WriteByte(c)
			prevLower = true
		}
	}
	return b.String()
}

func unflatten(flat map[string]any) map[string]any {
	result := make(map[string]any)
	for key, value := range flat {
		parts := strings.Split(key, ".")
		cursor := result
		for i := 0; i < len(parts)-1; i++ {
			next, ok := cursor[parts[i]].(map[string]any)
			if !ok || next == nil {
				next = make(map[string]any)
				cursor[parts[i]] = next
			}
			cursor = next
		}
		cursor[parts[len(parts)-1]] = value
	}
	return result
}

// unused keeps bytes import alive for future serialisation paths.
var _ = bytes.Reader{}
