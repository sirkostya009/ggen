package main

// Dry-run / vet-friendly validation entry points. checkPackage and checkFile
// mirror generateDir / generateSingleFile but stop after parsing: no codegen,
// no file write. The parse layer already surfaces every diagnostic the codegen
// path would hit; these just return the parser's errors.Join.

// checkPackage validates every annotated struct in dir, returning the parser's
// errors.Join unchanged for the caller's logger to unwrap + render.
func checkPackage(dir string) error {
	cliLog.Debug("checking package %s", dir)
	structs, pkgName, err := parsePackage(dir)
	if err != nil {
		return err
	}
	if len(structs) == 0 {
		cliLog.Trace("no annotated structs in %s; skipping", dir)
		return nil
	}
	cliLog.Debug("package %s: %d annotated structs", pkgName, len(structs))
	// Parity with generateDir: validate against the same struct shape.
	applyCLIFlags(structs)
	cliLog.Info("ok %s (%d structs)", relPath(dir), len(structs))
	return nil
}

// checkFile is the single-file analogue of checkPackage; `wanted` is the
// optional positional-name filter.
func checkFile(filename string, wanted []string) error {
	structs, _, _, err := parseFile(filename, wanted)
	if err != nil {
		return err
	}
	applyCLIFlags(structs)
	cliLog.Info("ok %s (%d structs)", relPath(filename), len(structs))
	return nil
}
