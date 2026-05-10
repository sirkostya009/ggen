package main

import (
	"go/token"
	"go/types"

	"golang.org/x/tools/go/packages"
)

// FieldInterfaces tracks which method-set / interface flavors a field's
// type implements. Computed once at parse time via go/types so the
// generator can emit hardcoded method calls instead of runtime probes.
type FieldInterfaces struct {
	// Resolved is true when static analysis ran on the field's type.
	// False = type info wasn't available (AST-only path); the generator
	// must fall back to its runtime probe cascade rather than trusting
	// the zero-valued flags below as "doesn't implement".
	Resolved bool

	TextUnmarshaler bool // *T has UnmarshalText([]byte) error
	TextMarshaler   bool // T or *T has MarshalText() ([]byte, error)
	TextAppender    bool // T or *T has AppendText(dst []byte) ([]byte, error) — Go 1.24+ encoding.TextAppender
	JSONUnmarshaler bool // *T has UnmarshalJSON([]byte) error
	JSONMarshaler   bool // T or *T has MarshalJSON() ([]byte, error)
	ByteDecoder     bool // T has DecodeFrom(data []byte, i int) (T, int, error)
	StreamDecoder   bool // T has DecodeStreamFrom(s *scan.Stream, i int) (T, int, error)
	AppendJSON      bool // T has AppendJSON(dst []byte) ([]byte, error)
}

// stdInterfaces caches well-known interface types loaded from the user's
// transitive dependency graph (encoding, encoding/json). Built once per
// parse pass; nil entries mean the package wasn't reachable.
type stdInterfaces struct {
	textMarshaler   *types.Interface
	textUnmarshaler *types.Interface
	textAppender    *types.Interface
	jsonMarshaler   *types.Interface
	jsonUnmarshaler *types.Interface
}

// findStdInterfaces resolves the encoding / encoding/json interface types
// the generator needs to drive types.Implements at field-type inspection
// time. First walks the loaded packages' transitive imports to pick them
// up cheaply; for any interface not found that way (user code that doesn't
// transitively import encoding — quite common when only plain Go types are
// in play), falls back to a targeted packages.Load. Without this, fields
// of cross-package text-encoded types would silently miss the fast path
// and route through the encoding/json fallback.
func findStdInterfaces(pkgs []*packages.Package) stdInterfaces {
	var out stdInterfaces
	visited := map[string]struct{}{}
	var visit func(p *packages.Package)
	visit = func(p *packages.Package) {
		if p == nil || p.Types == nil {
			return
		}
		if _, seen := visited[p.PkgPath]; seen {
			return
		}
		visited[p.PkgPath] = struct{}{}
		switch p.PkgPath {
		case "encoding":
			out.textMarshaler = lookupIface(p.Types, "TextMarshaler")
			out.textUnmarshaler = lookupIface(p.Types, "TextUnmarshaler")
			out.textAppender = lookupIface(p.Types, "TextAppender")
		case "encoding/json":
			out.jsonMarshaler = lookupIface(p.Types, "Marshaler")
			out.jsonUnmarshaler = lookupIface(p.Types, "Unmarshaler")
		}
		for _, imp := range p.Imports {
			visit(imp)
		}
	}
	for _, p := range pkgs {
		visit(p)
	}
	// Force-load the stdlib interface packages when the user's import graph
	// didn't reach them. We piggyback on the first loaded package's config
	// when possible; otherwise spin up a minimal one. Errors are non-fatal
	// — leaving an interface nil just means inspectType skips that probe.
	var missing []string
	if out.textMarshaler == nil || out.textUnmarshaler == nil || out.textAppender == nil {
		missing = append(missing, "encoding")
	}
	if out.jsonMarshaler == nil || out.jsonUnmarshaler == nil {
		missing = append(missing, "encoding/json")
	}
	if len(missing) > 0 {
		cfg := &packages.Config{
			Mode: packages.NeedName | packages.NeedTypes,
		}
		extra, err := packages.Load(cfg, missing...)
		if err == nil {
			for _, p := range extra {
				switch p.PkgPath {
				case "encoding":
					if out.textMarshaler == nil {
						out.textMarshaler = lookupIface(p.Types, "TextMarshaler")
					}
					if out.textUnmarshaler == nil {
						out.textUnmarshaler = lookupIface(p.Types, "TextUnmarshaler")
					}
					if out.textAppender == nil {
						out.textAppender = lookupIface(p.Types, "TextAppender")
					}
				case "encoding/json":
					if out.jsonMarshaler == nil {
						out.jsonMarshaler = lookupIface(p.Types, "Marshaler")
					}
					if out.jsonUnmarshaler == nil {
						out.jsonUnmarshaler = lookupIface(p.Types, "Unmarshaler")
					}
				}
			}
		}
	}
	return out
}

func lookupIface(p *types.Package, name string) *types.Interface {
	if p == nil {
		return nil
	}
	obj := p.Scope().Lookup(name)
	if obj == nil {
		return nil
	}
	iface, _ := obj.Type().Underlying().(*types.Interface)
	return iface
}

// inspectType derives FieldInterfaces for the given Go type, querying
// types.Implements against the well-known interfaces and walking the
// method set for the ggen-shaped methods (DecodeFrom, AppendJSON, etc.)
// whose signature involves the receiver type itself (no clean stdlib
// interface to test against).
func inspectType(t types.Type, std stdInterfaces) FieldInterfaces {
	if t == nil {
		return FieldInterfaces{}
	}
	iface := FieldInterfaces{Resolved: true}
	ptr := types.NewPointer(t)

	if std.textMarshaler != nil {
		iface.TextMarshaler = types.Implements(t, std.textMarshaler) ||
			types.Implements(ptr, std.textMarshaler)
	}
	if std.textUnmarshaler != nil {
		iface.TextUnmarshaler = types.Implements(ptr, std.textUnmarshaler)
	}
	if std.textAppender != nil {
		iface.TextAppender = types.Implements(t, std.textAppender) ||
			types.Implements(ptr, std.textAppender)
	}
	if std.jsonMarshaler != nil {
		iface.JSONMarshaler = types.Implements(t, std.jsonMarshaler) ||
			types.Implements(ptr, std.jsonMarshaler)
	}
	if std.jsonUnmarshaler != nil {
		iface.JSONUnmarshaler = types.Implements(ptr, std.jsonUnmarshaler)
	}

	// ggen-shaped methods: DecodeFrom / DecodeStreamFrom / AppendJSON.
	// The first two return T (the receiver), so we can't construct a
	// clean stdlib interface to test against. Walk the method set and
	// shape-match the signatures.
	ms := types.NewMethodSet(t)
	for i := 0; i < ms.Len(); i++ {
		sel := ms.At(i)
		fn, ok := sel.Obj().(*types.Func)
		if !ok {
			continue
		}
		sig, ok := fn.Type().(*types.Signature)
		if !ok {
			continue
		}
		switch fn.Name() {
		case "DecodeFrom":
			if matchByteDecoder(sig, t) {
				iface.ByteDecoder = true
			}
		case "DecodeStreamFrom":
			if matchStreamDecoder(sig, t) {
				iface.StreamDecoder = true
			}
		case "AppendJSON":
			if matchAppendJSON(sig) {
				iface.AppendJSON = true
			}
		}
	}
	return iface
}

// matchByteDecoder reports whether sig is `func(data []byte, i int) (T, int, error)`
// where T is the receiver type.
func matchByteDecoder(sig *types.Signature, recv types.Type) bool {
	params := sig.Params()
	results := sig.Results()
	if params.Len() != 2 || results.Len() != 3 {
		return false
	}
	if !isByteSlice(params.At(0).Type()) {
		return false
	}
	if !isInt(params.At(1).Type()) {
		return false
	}
	if !types.Identical(results.At(0).Type(), recv) {
		return false
	}
	if !isInt(results.At(1).Type()) {
		return false
	}
	return isError(results.At(2).Type())
}

// matchStreamDecoder reports whether sig is `func(s *scan.Stream, i int) (T, int, error)`.
// We don't have a handle on scan.Stream's type here, so we only match the
// shape (one pointer-to-struct param + one int + (T, int, error)).
func matchStreamDecoder(sig *types.Signature, recv types.Type) bool {
	params := sig.Params()
	results := sig.Results()
	if params.Len() != 2 || results.Len() != 3 {
		return false
	}
	if _, ok := params.At(0).Type().(*types.Pointer); !ok {
		return false
	}
	if !isInt(params.At(1).Type()) {
		return false
	}
	if !types.Identical(results.At(0).Type(), recv) {
		return false
	}
	if !isInt(results.At(1).Type()) {
		return false
	}
	return isError(results.At(2).Type())
}

// matchAppendJSON reports whether sig is `func(dst []byte) []byte`.
func matchAppendJSON(sig *types.Signature) bool {
	if sig.Params().Len() != 1 || sig.Results().Len() != 1 {
		return false
	}
	return isByteSlice(sig.Params().At(0).Type()) && isByteSlice(sig.Results().At(0).Type())
}

func isByteSlice(t types.Type) bool {
	s, ok := t.(*types.Slice)
	if !ok {
		return false
	}
	b, ok := s.Elem().(*types.Basic)
	return ok && (b.Kind() == types.Byte || b.Kind() == types.Uint8)
}

func isInt(t types.Type) bool {
	b, ok := t.(*types.Basic)
	return ok && b.Kind() == types.Int
}

func isError(t types.Type) bool {
	if named, ok := t.(*types.Named); ok {
		return named.Obj().Name() == "error" && named.Obj().Pkg() == nil
	}
	return false
}

var _ = token.NoPos // keep go/token import in case future refactor needs positions
