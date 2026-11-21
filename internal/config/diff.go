package config

import (
	"reflect"
	"strings"
)

// Diff возвращает конкретные пути, значения которых различаются в a и b.
// Скаляры — "retry.max_attempts"; элементы списков backends и auth.api_keys
// сопоставляются по имени (при пустом/повторяющемся — по индексу) и
// сообщаются целиком: "backends[gpu-1]" (добавлен, удалён или изменён).
// Порядок элементов списка сам по себе изменением не считается.
// Секретные значения участвуют в сравнении, но в результат попадают только пути.
func Diff(a, b *Config) []string {
	var out []string
	diffStruct(reflect.ValueOf(*a), reflect.ValueOf(*b), "", &out)
	return out
}

func diffStruct(a, b reflect.Value, prefix string, out *[]string) {
	t := a.Type()
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		name := strings.Split(f.Tag.Get("yaml"), ",")[0]
		path := name
		if prefix != "" {
			path = prefix + "." + name
		}
		av, bv := a.Field(i), b.Field(i)
		switch {
		case path == "backends":
			diffItems("backends", backendNames(a.Interface().(Config).Backends), backendNames(b.Interface().(Config).Backends), av, bv, out)
		case path == "auth.api_keys":
			diffItems("auth.api_keys", apiKeyNames(a.Interface().(Auth).APIKeys), apiKeyNames(b.Interface().(Auth).APIKeys), av, bv, out)
		case f.Type.Kind() == reflect.Struct:
			diffStruct(av, bv, path, out)
		default:
			if !reflect.DeepEqual(av.Interface(), bv.Interface()) {
				*out = append(*out, path)
			}
		}
	}
}

// diffItems сравнивает два списка элементов, сопоставляя их по ключу адресации.
func diffItems(list string, namesA, namesB []string, av, bv reflect.Value, out *[]string) {
	keysA, keysB := itemKeys(namesA), itemKeys(namesB)
	idxA := make(map[string]int, len(keysA))
	for i, k := range keysA {
		idxA[k] = i
	}
	seen := make(map[string]bool, len(keysB))
	for j, k := range keysB {
		seen[k] = true
		i, ok := idxA[k]
		if !ok || !itemEqual(av.Index(i).Interface(), bv.Index(j).Interface()) {
			*out = append(*out, itemPath(list, k))
		}
	}
	for _, k := range keysA {
		if !seen[k] {
			*out = append(*out, itemPath(list, k))
		}
	}
}

// itemEqual сравнивает элементы списков; пустой и nil-список models равны.
func itemEqual(x, y any) bool {
	if bx, ok := x.(Backend); ok {
		by := y.(Backend)
		if len(bx.Models) == 0 && len(by.Models) == 0 {
			bx.Models, by.Models = nil, nil
		}
		return reflect.DeepEqual(bx, by)
	}
	return reflect.DeepEqual(x, y)
}

// DiffByClass делит пути Diff на hot и restart.
func DiffByClass(paths []string) (hot, restart []string) {
	for _, p := range paths {
		if ClassOf(p) == ApplyHot {
			hot = append(hot, p)
		} else {
			restart = append(restart, p)
		}
	}
	return hot, restart
}
