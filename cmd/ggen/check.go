package main

import "github.com/sirkostya009/ggen/gen/model"

// Dry-run / vet-friendly validation entry points. checkPackage and checkFile
// mirror generateDir / generateSingleFile but stop after parsing: no codegen,
// no file write. The parse layer already surfaces every diagnostic the codegen
// path would hit; these just return the parser's errors.Join.

// checkPackage validates every annotated struct in dir, returning the parser's
// errors.Join unchanged for the caller's logger to unwrap + render.
func checkPackage(dir string) error {
	cliLog.Debugf("checking package %s", dir)
	pkg, err := model.ParsePackage(dir)
	if err != nil {
		return err
	}
	structs, pkgName := pkg.Structs, pkg.Name
	if len(structs) == 0 {
		cliLog.Tracef("no annotated structs in %s; skipping", dir)
		return nil
	}
	cliLog.Debugf("package %s: %d annotated structs", pkgName, len(structs))
	// Parity with generateDir: validate against the same struct shape.
	cliFlags.Apply(structs)
	cliLog.Infof("ok %s (%d structs)", model.RelPath(dir), len(structs))
	return nil
}

// checkFile is the single-file analogue of checkPackage; `wanted` is the
// optional positional-name filter.
func checkFile(filename string, wanted []string) error {
	res, err := model.ParseFile(filename, wanted)
	structs := res.Structs
	if err != nil {
		return err
	}
	cliFlags.Apply(structs)
	cliLog.Infof("ok %s (%d structs)", model.RelPath(filename), len(structs))
	return nil
}
