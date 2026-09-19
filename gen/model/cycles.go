package model

import "regexp"

var goIdentRe = regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_]*`)

// CyclicTypes over-approximates the reference graph by scraping type
// identifiers out of each field's type strings and intersecting with the
// generated set — an extra hit only costs a struct an unnecessary (correct)
// depth shim, so precision is not load-bearing.
func CyclicTypes(structs []StructInfo) map[string]struct{} {
	names := make(map[string]struct{}, len(structs))
	for _, s := range structs {
		names[s.Name] = struct{}{}
	}
	adj := make(map[string]map[string]struct{}, len(structs))
	var collect func(f FieldInfo, out map[string]struct{})
	collect = func(f FieldInfo, out map[string]struct{}) {
		for _, t := range []string{f.GoType, f.ElemType, f.PointeeType} {
			for _, id := range goIdentRe.FindAllString(t, -1) {
				if _, ok := names[id]; ok {
					out[id] = struct{}{}
				}
			}
		}
		if f.SQLNullInner != nil {
			collect(*f.SQLNullInner, out)
		}
	}
	for _, s := range structs {
		out := make(map[string]struct{})
		for _, f := range s.Fields {
			collect(f, out)
		}
		if s.IsAlias {
			for _, id := range goIdentRe.FindAllString(s.AliasUnderlying, -1) {
				if _, ok := names[id]; ok {
					out[id] = struct{}{}
				}
			}
		}
		adj[s.Name] = out
	}
	cyc := make(map[string]struct{})
	for name := range adj {
		// name is cyclic iff it can reach itself.
		seen := map[string]struct{}{}
		stack := make([]string, 0, 8)
		for n := range adj[name] {
			stack = append(stack, n)
		}
		for len(stack) > 0 {
			n := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if n == name {
				cyc[name] = struct{}{}
				break
			}
			if _, ok := seen[n]; ok {
				continue
			}
			seen[n] = struct{}{}
			for m := range adj[n] {
				stack = append(stack, m)
			}
		}
	}
	return cyc
}
