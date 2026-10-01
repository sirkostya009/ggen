package model

import (
	"go/token"
	"go/types"

	"golang.org/x/tools/go/packages"
)

// FieldInterfaces records which methods and interfaces a field's type
// implements. It is computed at parse time, so the generator can emit direct
// calls instead of checking at runtime.
type FieldInterfaces struct {
	// Resolved is true when type checking ran. When it is false, on the
	// AST-only path, the generator must fall back to its runtime checks. It
	// must not read the zero flags below as "does not implement".
	Resolved bool

	TextUnmarshaler bool // *T has UnmarshalText([]byte) error
	TextMarshaler   bool // T or *T has MarshalText() ([]byte, error)
	TextAppender    bool // T or *T has AppendText(dst []byte) ([]byte, error), encoding.TextAppender
	JSONUnmarshaler bool // *T has UnmarshalJSON([]byte) error
	JSONMarshaler   bool // T or *T has MarshalJSON() ([]byte, error)
	ByteDecoder     bool // T has DecodeFrom(data []byte) (T, int, error)
	StreamDecoder   bool // T has DecodeFromStream(s *ggen.Stream) (T, error)
	AppendJSON      bool // T has AppendJSON(dst []byte) ([]byte, error)
	JSONSize        bool // T has JSONSize() int, a real size bound instead of a flat guess for foreign types
}

// stdInterfaces holds the standard interface types from encoding and
// encoding/json that types.Implements is checked against. It is built once
// per parse pass. A nil entry means the package could not be loaded.
// inspectCache stores inspectType results by type identity, since named
// types are unique within a pass. This saves repeated work when many fields
// share a type such as uuid.UUID or time.Time.
type stdInterfaces struct {
	textMarshaler   *types.Interface
	textUnmarshaler *types.Interface
	textAppender    *types.Interface
	jsonMarshaler   *types.Interface
	jsonUnmarshaler *types.Interface
	inspectCache    map[types.Type]FieldInterfaces
}

// findStdInterfaces finds the encoding and encoding/json interface types. It
// first searches the imports of the loaded packages, recursively. Any it does
// not find there are loaded directly with packages.Load. That case is common
// when the code uses only plain Go types. Without it, text types from other
// packages would miss the direct-call path.
func findStdInterfaces(pkgs []*packages.Package) stdInterfaces {
	out := stdInterfaces{inspectCache: map[types.Type]FieldInterfaces{}}
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
	// Load the packages the import graph did not reach. Errors are not fatal.
	// A nil interface only skips that check.
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

// inspectType computes FieldInterfaces for t. It checks the standard
// interfaces with types.Implements. It also walks the method set for the
// ggen methods such as DecodeFrom and AppendJSON, whose signatures name the
// receiver type itself.
func inspectType(t types.Type, std stdInterfaces) FieldInterfaces {
	if t == nil {
		return FieldInterfaces{}
	}
	if hit, ok := std.inspectCache[t]; ok {
		return hit
	}
	// Check the pointee, not the pointer. The method set of `*T` contains the
	// methods of T, so everything callable on a `*T` is what inspectType(T)
	// reports. The ggen methods also name the receiver type, for example
	// `DecodeFrom` returns T and not *T, so matching them against `*T` would
	// always fail. Removing the pointer also handles the text and json
	// unmarshalers correctly: they live on `*T`, which is built below.
	key := t
	for {
		p, ok := t.Underlying().(*types.Pointer)
		if !ok {
			break
		}
		t = p.Elem()
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

	// The ggen methods return T, the receiver type, so no single interface
	// can describe them. Match the signatures by shape instead.
	ms := types.NewMethodSet(t)
	for sel := range ms.Methods() {
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
		case "DecodeFromStream":
			if matchStreamDecoder(sig, t) {
				iface.StreamDecoder = true
			}
		case "AppendJSON":
			if matchAppendJSON(sig) {
				iface.AppendJSON = true
			}
		case "JSONSize":
			if matchJSONSize(sig) {
				iface.JSONSize = true
			}
		}
	}
	if std.inspectCache != nil {
		std.inspectCache[key] = iface
	}
	return iface
}

// matchJSONSize reports whether sig is `func() int`.
func matchJSONSize(sig *types.Signature) bool {
	if sig.Params().Len() != 0 || sig.Results().Len() != 1 {
		return false
	}
	b, ok := sig.Results().At(0).Type().(*types.Basic)
	return ok && b.Kind() == types.Int
}

// matchByteDecoder reports whether sig is `func(data []byte) (T, int, error)`
// where T is the receiver type.
func matchByteDecoder(sig *types.Signature, recv types.Type) bool {
	params := sig.Params()
	results := sig.Results()
	if params.Len() != 1 || results.Len() != 3 {
		return false
	}
	if !isByteSlice(params.At(0).Type()) {
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

// matchStreamDecoder reports whether sig is `func(s *ggen.Stream) (T, error)`.
// The parser has no handle on the ggen.Stream type, so it matches only the
// shape: one pointer parameter and the results (T, error).
func matchStreamDecoder(sig *types.Signature, recv types.Type) bool {
	params := sig.Params()
	results := sig.Results()
	if params.Len() != 1 || results.Len() != 2 {
		return false
	}
	if _, ok := params.At(0).Type().(*types.Pointer); !ok {
		return false
	}
	if !types.Identical(results.At(0).Type(), recv) {
		return false
	}
	return isError(results.At(1).Type())
}

// matchAppendJSON reports whether sig is `func(dst []byte) ([]byte, error)`,
// the shape renderAppendJSON emits. A one-result `func([]byte) []byte` is not
// accepted, because the code emitted at a call from another package assigns
// two results.
func matchAppendJSON(sig *types.Signature) bool {
	if sig.Params().Len() != 1 || sig.Results().Len() != 2 {
		return false
	}
	return isByteSlice(sig.Params().At(0).Type()) &&
		isByteSlice(sig.Results().At(0).Type()) &&
		isError(sig.Results().At(1).Type())
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

var _ = token.NoPos // keeps the go/token import used
